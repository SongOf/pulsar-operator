package cluster

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/SongOf/pulsar-operator/internal/storage/model"
	"github.com/SongOf/pulsar-operator/internal/storage/store"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"
)

type fixture struct {
	t          *testing.T
	root       string
	store      *store.Store
	client     *fake.FakeDynamicClient
	cluster    *Cluster
	deleteMode string
	deletes    int
}

func rawPool(name, uid, node, vg, serial, path string) object {
	return object{"apiVersion": "storage.diskfleet.io/v1alpha1", "kind": "DiskPool", "metadata": object{"name": name, "uid": uid, "resourceVersion": "1"}, "spec": object{"nodeName": node, "vgName": vg, "approved": true, "devices": []any{object{"path": path, "serial": serial, "sizeBytes": int64(100 * model.GiB)}}}, "status": object{"phase": "Ready", "storageClass": className(name), "topologyHostname": node, "capacityBytes": int64(100 * model.GiB), "availableBytes": int64(90 * model.GiB)}}
}
func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	local, err := store.New(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = local.Close() })
	pool := rawPool("pool-one", "pool-uid", "worker-01", "df_one", "SER001", "/dev/disk/by-id/wwn-one")
	disk := object{"apiVersion": "storage.diskfleet.io/v1alpha1", "kind": "PhysicalDisk", "metadata": object{"name": "disk-one", "uid": "disk-uid", "resourceVersion": "1"}, "spec": object{"nodeName": "worker-01", "path": "/dev/disk/by-id/wwn-one", "serial": "SER001", "sizeBytes": int64(100 * model.GiB), "model": "test", "media": "SSD"}, "status": object{"phase": "Available", "poolName": ""}}
	node := object{"apiVersion": "v1", "kind": "Node", "metadata": object{"name": "worker-01", "labels": object{label + "enabled": "true", "topology.kubernetes.io/zone": "az-1"}}, "status": object{"conditions": []any{object{"type": "Ready", "status": "True"}}}}
	client := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{poolsResource: "DiskPoolList", disksResource: "PhysicalDiskList", classesResource: "StorageClassList", pvcsResource: "PersistentVolumeClaimList", pvsResource: "PersistentVolumeList", nodesResource: "NodeList"},
		&unstructured.Unstructured{Object: pool}, &unstructured.Unstructured{Object: disk}, &unstructured.Unstructured{Object: storageClass(pool, "worker-01")}, &unstructured.Unstructured{Object: node})
	f := &fixture{t: t, root: root, store: local, client: client}
	f.cluster = New(local, client)
	client.PrependReactor("create", "*", func(action k8stesting.Action) (bool, runtime.Object, error) {
		obj := action.(k8stesting.CreateAction).GetObject().(*unstructured.Unstructured)
		if obj.GetUID() == "" {
			obj.SetUID(types.UID("uid-" + obj.GetName()))
		}
		if obj.GetResourceVersion() == "" {
			obj.SetResourceVersion("1")
		}
		return false, nil, nil
	})
	client.PrependReactor("delete", "persistentvolumeclaims", func(action k8stesting.Action) (bool, runtime.Object, error) {
		f.deletes++
		a := action.(k8stesting.DeleteAction)
		options := a.GetDeleteOptions()
		page, err := f.store.List(context.Background(), "release-intents", model.ListOptions{Limit: 1000000})
		if err != nil {
			return true, nil, err
		}
		if len(page.Items) == 0 {
			t.Error("remote DELETE occurred without committed intent")
		}
		current, err := client.Tracker().Get(pvcsResource, a.GetNamespace(), a.GetName())
		if err != nil {
			return true, nil, err
		}
		pvc := current.(*unstructured.Unstructured)
		if options.Preconditions == nil || options.Preconditions.UID == nil || options.Preconditions.ResourceVersion == nil || *options.Preconditions.UID != pvc.GetUID() || *options.Preconditions.ResourceVersion != pvc.GetResourceVersion() {
			return true, nil, apierrors.NewConflict(pvcsResource.GroupResource(), a.GetName(), errors.New("preconditions failed"))
		}
		if f.deleteMode == "conflict" {
			return true, nil, apierrors.NewConflict(pvcsResource.GroupResource(), a.GetName(), errors.New("resourceVersion changed"))
		}
		if f.deleteMode == "deleting" {
			now := metav1.Now()
			pvc.SetDeletionTimestamp(&now)
			return true, nil, client.Tracker().Update(pvcsResource, pvc, a.GetNamespace())
		}
		if err := client.Tracker().Delete(pvcsResource, a.GetNamespace(), a.GetName()); err != nil {
			return true, nil, err
		}
		if name := str(pvc.Object, "spec", "volumeName"); name != "" {
			if pv, err := client.Tracker().Get(pvsResource, "", name); err == nil {
				u := pv.(*unstructured.Unstructured)
				u.Object["status"] = object{"phase": "Released"}
				if err := client.Tracker().Update(pvsResource, u, ""); err != nil {
					return true, nil, err
				}
			}
		}
		if f.deleteMode == "lost" {
			return true, nil, apierrors.NewServiceUnavailable("response lost after committed deletion")
		}
		return true, nil, nil
	})
	return f
}
func (f *fixture) get(resource schema.GroupVersionResource, namespace, name string) object {
	f.t.Helper()
	obj, err := f.client.Resource(resource).Namespace(namespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		f.t.Fatal(err)
	}
	return obj.Object
}
func (f *fixture) put(resource schema.GroupVersionResource, value object) {
	f.t.Helper()
	u := &unstructured.Unstructured{Object: value}
	_, err := f.client.Resource(resource).Namespace(u.GetNamespace()).Update(context.Background(), u, metav1.UpdateOptions{})
	if apierrors.IsNotFound(err) {
		_, err = f.client.Resource(resource).Namespace(u.GetNamespace()).Create(context.Background(), u, metav1.CreateOptions{})
	}
	if err != nil {
		f.t.Fatal(err)
	}
	f.cluster.Invalidate()
}
func (f *fixture) volumes() []object {
	f.t.Helper()
	page, err := f.cluster.List(context.Background(), "volumes", model.ListOptions{Limit: 1000000})
	if err != nil {
		f.t.Fatal(err)
	}
	return page.Items
}
func (f *fixture) count(kind string) int {
	f.t.Helper()
	page, err := f.store.List(context.Background(), kind, model.ListOptions{Limit: 1000000})
	if err != nil {
		f.t.Fatal(err)
	}
	return page.Total
}
func (f *fixture) boundVolume(managed bool) object {
	f.t.Helper()
	managedLabel := "false"
	if managed {
		managedLabel = "true"
	}
	pvc := object{"apiVersion": "v1", "kind": "PersistentVolumeClaim", "metadata": object{"name": "data", "namespace": "default", "uid": "pvc-uid", "resourceVersion": "17", "labels": object{label + "managed": managedLabel}}, "spec": object{"volumeName": "pv-data", "storageClassName": "diskfleet-pool-one", "resources": object{"requests": object{"storage": "1Gi"}}}, "status": object{"phase": "Bound"}}
	expression := object{"key": "openebs.io/nodename", "operator": "In", "values": []any{"worker-01"}}
	pv := object{"apiVersion": "v1", "kind": "PersistentVolume", "metadata": object{"name": "pv-data", "uid": "pv-uid", "resourceVersion": "1"}, "spec": object{"persistentVolumeReclaimPolicy": "Retain", "storageClassName": "diskfleet-pool-one", "claimRef": object{"uid": "pvc-uid", "namespace": "default", "name": "data"}, "csi": object{"driver": "local.csi.openebs.io", "volumeHandle": "volume-one", "volumeAttributes": object{"openebs.io/volgroup": "df_one"}}, "nodeAffinity": object{"required": object{"nodeSelectorTerms": []any{object{"matchExpressions": []any{expression}}}}}}, "status": object{"phase": "Bound"}}
	f.put(pvcsResource, pvc)
	f.put(pvsResource, pv)
	return pvc
}
func assertProblem(t *testing.T, err error, status int) {
	t.Helper()
	var p *model.Problem
	if !errors.As(err, &p) || p.Status != status {
		t.Fatalf("expected Problem %d, got %v", status, err)
	}
}
func assertNoWrites(t *testing.T, client *fake.FakeDynamicClient) {
	t.Helper()
	for _, action := range client.Actions() {
		switch action.GetVerb() {
		case "create", "patch", "delete", "update":
			t.Fatalf("unexpected Kubernetes mutation: %v", action)
		}
	}
}

func TestExternalOwnershipRemainsReadOnly(t *testing.T) {
	for _, scenario := range []string{"unmanaged", "owner-reference", "manager-label"} {
		t.Run(scenario, func(t *testing.T) {
			f := newFixture(t)
			pvc := f.boundVolume(scenario != "unmanaged")
			meta := asObject(pvc["metadata"])
			if scenario == "owner-reference" {
				meta["ownerReferences"] = []any{object{"kind": "StatefulSet", "name": "bookkeeper", "uid": "sts-uid"}}
			}
			if scenario == "manager-label" {
				asObject(meta["labels"])["app.kubernetes.io/managed-by"] = "pulsar-operator"
			}
			f.put(pvcsResource, pvc)
			f.client.ClearActions()
			volumes := f.volumes()
			if len(volumes) != 1 || volumes[0]["canManage"] != false || str(volumes[0], "management") != "external" || str(volumes[0], "poolId") != "pool-one" {
				t.Fatal(volumes)
			}
			_, err := f.cluster.Expand(context.Background(), "default~data", object{"sizeGiB": int64(2)})
			assertProblem(t, err, 403)
			_, err = f.cluster.Release(context.Background(), "default~data", "")
			assertProblem(t, err, 403)
			assertNoWrites(t, f.client)
		})
	}
}

func TestClassIdentityAndParametersMustRemainExact(t *testing.T) {
	for _, mutation := range []string{"uid", "driver", "reclaim", "pattern", "thin", "fstype", "storage", "expansion", "topology"} {
		t.Run(mutation, func(t *testing.T) {
			f := newFixture(t)
			f.boundVolume(false)
			sc := f.get(classesResource, "", "diskfleet-pool-one")
			switch mutation {
			case "uid":
				asObject(at(sc, "metadata", "annotations"))[label+"pool-uid"] = "forged"
			case "driver":
				sc["provisioner"] = "foreign.csi"
			case "reclaim":
				sc["reclaimPolicy"] = "Delete"
			case "pattern":
				asObject(sc["parameters"])["vgpattern"] = ".*"
			case "thin":
				asObject(sc["parameters"])["thinProvision"] = "yes"
			case "fstype":
				asObject(sc["parameters"])["fsType"] = "xfs"
			case "storage":
				asObject(sc["parameters"])["storage"] = "other"
			case "expansion":
				sc["allowVolumeExpansion"] = false
			case "topology":
				sc["allowedTopologies"] = topology([]string{"unknown"})
			}
			f.put(classesResource, sc)
			f.client.ClearActions()
			if got := f.volumes(); len(got) != 0 {
				t.Fatal(got)
			}
			_, err := f.cluster.CreateVolume(context.Background(), object{"name": "new", "poolId": "pool-one", "sizeGiB": int64(1)})
			assertProblem(t, err, 409)
			assertNoWrites(t, f.client)
		})
	}
	f := newFixture(t)
	f.boundVolume(false)
	pool := f.get(poolsResource, "", "pool-one")
	asObject(pool["status"])["phase"] = "Error"
	f.put(poolsResource, pool)
	if got := f.volumes(); len(got) != 1 || got[0]["canManage"] != false {
		t.Fatal("unhealthy pool lost external observation", got)
	}
}

func TestActualPVBindingControlsMutations(t *testing.T) {
	for _, mutation := range []string{"claim", "namespace", "driver", "vg", "class", "node", "unconstrained-or", "multiple-nodes", "missing-pv"} {
		t.Run(mutation, func(t *testing.T) {
			f := newFixture(t)
			f.boundVolume(true)
			pv := f.get(pvsResource, "", "pv-data")
			spec := asObject(pv["spec"])
			terms := array(at(spec, "nodeAffinity", "required", "nodeSelectorTerms"))
			expr := asObject(array(at(terms[0], "matchExpressions"))[0])
			switch mutation {
			case "claim":
				asObject(spec["claimRef"])["uid"] = "other"
			case "namespace":
				asObject(spec["claimRef"])["namespace"] = "other"
			case "driver":
				asObject(spec["csi"])["driver"] = "other"
			case "vg":
				asObject(at(spec, "csi", "volumeAttributes"))["openebs.io/volgroup"] = "df_external"
			case "class":
				spec["storageClassName"] = "foreign"
			case "node":
				expr["values"] = []any{"worker-02"}
			case "unconstrained-or":
				asObject(at(spec, "nodeAffinity", "required"))["nodeSelectorTerms"] = append(terms, object{})
			case "multiple-nodes":
				expr["values"] = []any{"worker-01", "worker-02"}
			}
			f.put(pvsResource, pv)
			if mutation == "missing-pv" {
				if err := f.client.Tracker().Delete(pvsResource, "", "pv-data"); err != nil {
					t.Fatal(err)
				}
			}
			f.cluster.Invalidate()
			f.client.ClearActions()
			got := f.volumes()
			if len(got) != 1 || got[0]["bindingVerified"] != false || got[0]["canManage"] != false || str(got[0], "poolId") != "" || str(got[0], "bindingMessage") == "" {
				t.Fatal(got)
			}
			_, err := f.cluster.Expand(context.Background(), "default~data", object{"sizeGiB": int64(2)})
			assertProblem(t, err, 409)
			assertNoWrites(t, f.client)
		})
	}
}

func (f *fixture) fleetVolume() object {
	f.t.Helper()
	second := rawPool("pool-two", "pool-two-uid", "worker-02", "df_two", "SER002", "/dev/disk/by-id/wwn-two")
	asObject(second["status"])["topologyHostname"] = "rack-host-two"
	f.put(poolsResource, second)
	page, err := f.cluster.List(context.Background(), "pools", model.ListOptions{Limit: 100})
	if err != nil {
		f.t.Fatal(err)
	}
	sc, err := FleetStorageClass("diskfleet-fleet", page.Items)
	if err != nil {
		f.t.Fatal(err)
	}
	f.put(classesResource, sc.Object)
	pvc := f.boundVolume(false)
	asObject(pvc["spec"])["storageClassName"] = "diskfleet-fleet"
	asObject(pvc["metadata"])["annotations"] = object{"volume.kubernetes.io/selected-node": "worker-01"}
	f.put(pvcsResource, pvc)
	pv := f.get(pvsResource, "", "pv-data")
	asObject(pv["spec"])["storageClassName"] = "diskfleet-fleet"
	asObject(at(pv, "spec", "csi", "volumeAttributes"))["openebs.io/volgroup"] = "df_two"
	expr := asObject(array(at(array(at(pv, "spec", "nodeAffinity", "required", "nodeSelectorTerms"))[0], "matchExpressions"))[0])
	expr["key"], expr["values"] = "kubernetes.io/hostname", []any{"rack-host-two"}
	f.put(pvsResource, pv)
	return pvc
}
func TestSharedClassResolvesPVAndDoesNotInventPendingPlacement(t *testing.T) {
	f := newFixture(t)
	pvc := f.fleetVolume()
	volume := f.volumes()[0]
	if str(volume, "poolId") != "pool-two" || str(volume, "node") != "worker-02" || volume["bindingVerified"] != true || volume["canManage"] != false {
		t.Fatal(volume)
	}
	pool := f.get(poolsResource, "", "pool-two")
	asObject(pool["status"])["phase"] = "Error"
	f.put(poolsResource, pool)
	if str(f.volumes()[0], "poolId") != "pool-two" {
		t.Fatal("unhealthy member lost ownership")
	}
	delete(asObject(pvc["metadata"]), "annotations")
	delete(asObject(pvc["spec"]), "volumeName")
	pvc["status"] = object{"phase": "Pending"}
	f.put(pvcsResource, pvc)
	volume = f.volumes()[0]
	if str(volume, "poolId") != "" || str(volume, "node") != "" || str(volume, "pool") != "多节点存储类" {
		t.Fatal(volume)
	}
	for _, mutation := range []string{"uid", "pattern", "topology"} {
		t.Run(mutation, func(t *testing.T) {
			g := newFixture(t)
			g.fleetVolume()
			sc := g.get(classesResource, "", "diskfleet-fleet")
			switch mutation {
			case "uid":
				asObject(at(sc, "metadata", "annotations"))[label+"pool-uids"] = `{"pool-one":"forged","pool-two":"pool-two-uid"}`
			case "pattern":
				asObject(sc["parameters"])["vgpattern"] = "^df_.*$"
			case "topology":
				sc["allowedTopologies"] = topology([]string{"worker-01", "rack-host-two", "unknown"})
			}
			g.put(classesResource, sc)
			if len(g.volumes()) != 0 {
				t.Fatal("unreviewed shared class trusted")
			}
		})
	}
}

func TestConflictingClaimsExcludeCapacityAndPreserveDiskHealth(t *testing.T) {
	f := newFixture(t)
	f.boundVolume(true)
	pool := f.get(poolsResource, "", "pool-one")
	asObject(pool["metadata"])["name"] = "duplicate"
	asObject(pool["metadata"])["uid"] = "duplicate-uid"
	asObject(pool["status"])["storageClass"] = "diskfleet-duplicate"
	f.put(poolsResource, pool)
	f.put(classesResource, storageClass(pool, "worker-01"))
	overview, err := f.cluster.Overview(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"capacityBytes", "allocatedBytes", "availableBytes"} {
		if model.Int64(at(overview, "totals", field)) != 0 {
			t.Fatal(overview)
		}
	}
	page, err := f.cluster.List(context.Background(), "pools", model.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range page.Items {
		if str(p, "state") != "Conflict" {
			t.Fatal(p)
		}
	}
	if len(f.volumes()) != 0 {
		t.Fatal("conflicting pool class remains trusted")
	}
	disks, _ := f.cluster.List(context.Background(), "disks", model.ListOptions{})
	if disks.Items[0]["poolId"] != nil {
		t.Fatal(disks.Items)
	}
	g := newFixture(t)
	disk := g.get(disksResource, "", "disk-one")
	asObject(disk["status"])["phase"] = "Offline"
	g.put(disksResource, disk)
	disks, _ = g.cluster.List(context.Background(), "disks", model.ListOptions{})
	if str(disks.Items[0], "state") != "Offline" {
		t.Fatal("Ready pool hid offline physical disk")
	}
}

func (f *fixture) plan() object {
	f.t.Helper()
	if err := f.client.Tracker().Delete(poolsResource, "", "pool-one"); err != nil {
		f.t.Fatal(err)
	}
	plan, err := f.cluster.Plan(context.Background(), object{"name": "first", "node": "worker-01", "diskIds": []any{"disk-one"}})
	if err != nil {
		f.t.Fatal(err)
	}
	return plan
}
func TestPlanCommitRevalidatesIdentityStateAndClaims(t *testing.T) {
	for _, mutation := range []string{"serial", "size", "path", "state", "claim"} {
		t.Run(mutation, func(t *testing.T) {
			f := newFixture(t)
			plan := f.plan()
			disk := f.get(disksResource, "", "disk-one")
			switch mutation {
			case "serial":
				asObject(disk["spec"])["serial"] = "replacement"
			case "size":
				asObject(disk["spec"])["sizeBytes"] = int64(200 * model.GiB)
			case "path":
				asObject(disk["spec"])["path"] = "/dev/disk/by-id/other"
			case "state":
				asObject(disk["status"])["phase"] = "Protected"
			case "claim":
				f.put(poolsResource, rawPool("manual", "manual-uid", "worker-01", "df_other", "SER001", "/dev/disk/by-id/alias"))
			}
			f.put(disksResource, disk)
			f.client.ClearActions()
			_, err := f.cluster.CreatePool(context.Background(), str(plan, "planId"), str(plan, "confirmation"))
			assertProblem(t, err, 409)
			assertNoWrites(t, f.client)
		})
	}
}
func TestPoolCreationIsIdempotentAndConsumedPlansCannotRecreate(t *testing.T) {
	f := newFixture(t)
	plan := f.plan()
	first, err := f.cluster.CreatePool(context.Background(), str(plan, "planId"), str(plan, "confirmation"))
	if err != nil {
		t.Fatal(err)
	}
	disk := f.get(disksResource, "", "disk-one")
	asObject(disk["status"])["phase"] = "Managed"
	f.put(disksResource, disk)
	if err := f.store.Transaction(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(context.Background(), "UPDATE plans SET consumed=0 WHERE id=?", str(plan, "planId"))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	f.client.ClearActions()
	second, err := f.cluster.CreatePool(context.Background(), str(plan, "planId"), str(plan, "confirmation"))
	if err != nil || str(first, "id") != str(second, "id") {
		t.Fatal(second, err)
	}
	assertNoWrites(t, f.client)
	if err := f.client.Tracker().Delete(poolsResource, "", str(first, "id")); err != nil {
		t.Fatal(err)
	}
	_, err = f.cluster.CreatePool(context.Background(), str(plan, "planId"), str(plan, "confirmation"))
	assertProblem(t, err, 409)
}
func TestVolumeNamespaceRejectsExplicitInvalidValuesBeforeKubernetesAccess(t *testing.T) {
	for _, value := range []any{nil, "", false, int64(1), []any{"default"}, object{"name": "default"}} {
		encoded, _ := json.Marshal(value)
		t.Run(string(encoded), func(t *testing.T) {
			f := newFixture(t)
			f.client.ClearActions()
			_, err := f.cluster.CreateVolume(context.Background(), object{"name": "data", "namespace": value, "poolId": "pool-one", "sizeGiB": int64(1)})
			assertProblem(t, err, 400)
			if len(f.client.Actions()) != 0 {
				t.Fatal("invalid explicit namespace reached Kubernetes", f.client.Actions())
			}
		})
	}
}

func TestVolumeCreationExpansionAndTerminatingName(t *testing.T) {
	f := newFixture(t)
	body := object{"name": "data", "poolId": "pool-one", "sizeGiB": int64(2)}
	created, err := f.cluster.CreateVolume(context.Background(), body)
	if err != nil || str(created, "status") != "Pending" {
		t.Fatal(created, err)
	}
	pvc := f.get(pvcsResource, "default", "data")
	if str(pvc, "spec", "storageClassName") != "diskfleet-pool-one" || str(pvc, "spec", "resources", "requests", "storage") != "2Gi" || at(pvc, "spec", "volumeName") != nil {
		t.Fatal(pvc)
	}
	if _, err := f.cluster.CreateVolume(context.Background(), body); err != nil {
		t.Fatal("matching retry failed", err)
	}
	f.client.ClearActions()
	_, err = f.cluster.Expand(context.Background(), "default~data", object{"sizeGiB": int64(1)})
	assertProblem(t, err, 409)
	assertNoWrites(t, f.client)
	_, err = f.cluster.Expand(context.Background(), "default~data", object{"sizeGiB": int64(3)})
	if err != nil {
		t.Fatal(err)
	}
	var patch object
	for _, action := range f.client.Actions() {
		if action.GetVerb() == "patch" {
			if err := json.Unmarshal(action.(k8stesting.PatchAction).GetPatch(), &patch); err != nil {
				t.Fatal(err)
			}
		}
	}
	if str(patch, "metadata", "resourceVersion") != "1" || str(patch, "spec", "resources", "requests", "storage") != "3Gi" {
		t.Fatal(patch)
	}
	pvc = f.get(pvcsResource, "default", "data")
	asObject(pvc["metadata"])["deletionTimestamp"] = "2026-11-13T00:00:00Z"
	f.put(pvcsResource, pvc)
	_, err = f.cluster.CreateVolume(context.Background(), object{"name": "data", "poolId": "pool-one", "sizeGiB": int64(3)})
	assertProblem(t, err, 409)
	_, err = f.cluster.Expand(context.Background(), "default~data", object{"sizeGiB": int64(4)})
	assertProblem(t, err, 409)
}

func TestReleaseRequiresRetainAndDurableIdentity(t *testing.T) {
	for _, mutation := range []string{"policy", "claim", "uid"} {
		t.Run(mutation, func(t *testing.T) {
			f := newFixture(t)
			f.boundVolume(true)
			pv := f.get(pvsResource, "", "pv-data")
			switch mutation {
			case "policy":
				asObject(pv["spec"])["persistentVolumeReclaimPolicy"] = "Delete"
			case "claim":
				asObject(at(pv, "spec", "claimRef"))["uid"] = "other"
			}
			f.put(pvsResource, pv)
			uid := ""
			if mutation == "uid" {
				uid = "stale-uid"
			}
			_, err := f.cluster.Release(context.Background(), "default~data", uid)
			assertProblem(t, err, 409)
			if f.deletes != 0 || f.count("release-intents") != 0 {
				t.Fatal("unverified delete attempted")
			}
		})
	}
	f := newFixture(t)
	f.boundVolume(true)
	record, err := f.cluster.Release(context.Background(), "default~data", "pvc-uid")
	if err != nil || str(record, "status") != "ReleaseRequested" || str(record, "pvUid") != "pv-uid" {
		t.Fatal(record, err)
	}
	if f.count("retained") != 1 || f.count("release-intents") != 0 || f.deletes != 1 {
		t.Fatal("intent not finalized")
	}
	volume := f.volumes()[0]
	if str(volume, "status") != "Released" || volume["canManage"] != false {
		t.Fatal(volume)
	}
	pool, _ := f.cluster.List(context.Background(), "pools", model.ListOptions{})
	if model.Int64(pool.Items[0]["availableBytes"]) != 90*model.GiB {
		t.Fatal("retention falsely freed capacity")
	}
}
func TestLostDeleteResponseRecoversAfterRestartAndProtectsReusedName(t *testing.T) {
	f := newFixture(t)
	old := f.boundVolume(true)
	f.deleteMode = "lost"
	_, err := f.cluster.Release(context.Background(), "default~data", "pvc-uid")
	if err == nil {
		t.Fatal("lost response was hidden")
	}
	if f.count("release-intents") != 1 || f.count("retained") != 0 {
		t.Fatal("intent did not survive")
	}
	// Restart reads durable intent, resolves the actual retained PV, and then
	// refuses a name-only retry against a replacement UID.
	reopened, err := store.New(f.root)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restarted := New(reopened, f.client)
	page, err := restarted.List(context.Background(), "volumes", model.ListOptions{})
	if err != nil || len(page.Items) != 1 || str(page.Items[0], "status") != "Released" {
		t.Fatal(page, err)
	}
	if f.count("release-intents") != 0 {
		t.Fatal("read recovery did not finalize intent")
	}
	replacement := copyObject(old)
	asObject(replacement["metadata"])["uid"] = "new-pvc"
	asObject(replacement["metadata"])["resourceVersion"] = "100"
	asObject(replacement["spec"])["volumeName"] = "pv-new"
	f.put(pvcsResource, replacement)
	_, err = restarted.Release(context.Background(), "default~data", "")
	assertProblem(t, err, 409)
	record, err := restarted.Release(context.Background(), "default~data", "pvc-uid")
	if err != nil || str(record, "pvcUid") != "pvc-uid" || f.deletes != 1 {
		t.Fatal(record, err, f.deletes)
	}
	pv := f.get(pvsResource, "", "pv-data")
	asObject(pv["metadata"])["name"] = "pv-new"
	asObject(pv["metadata"])["uid"] = "pv-new-uid"
	asObject(at(pv, "spec", "claimRef"))["uid"] = "new-pvc"
	pv["status"] = object{"phase": "Bound"}
	f.put(pvsResource, pv)
	f.deleteMode = ""
	record, err = restarted.Release(context.Background(), "default~data", "new-pvc")
	if err != nil || str(record, "pvcUid") != "new-pvc" || f.deletes != 2 || f.count("retained") != 2 {
		t.Fatal(record, err)
	}
}
func TestPendingOldIntentCannotDeleteReusedName(t *testing.T) {
	f := newFixture(t)
	old := f.boundVolume(true)
	f.deleteMode = "lost"
	if _, err := f.cluster.Release(context.Background(), "default~data", ""); err == nil {
		t.Fatal("expected lost response")
	}
	updated := copyObject(old)
	asObject(updated["metadata"])["uid"] = "replacement"
	asObject(updated["metadata"])["resourceVersion"] = "100"
	f.put(pvcsResource, updated)
	record, err := f.cluster.Release(context.Background(), "default~data", "")
	if err != nil || str(record, "pvcUid") != "pvc-uid" || f.deletes != 1 || str(f.get(pvcsResource, "default", "data"), "metadata", "uid") != "replacement" {
		t.Fatal(record, err)
	}
}
func TestDeleteConflictAndLiveUIDDoNotFalselyCompleteRetention(t *testing.T) {
	f := newFixture(t)
	f.boundVolume(true)
	f.deleteMode = "conflict"
	_, err := f.cluster.Release(context.Background(), "default~data", "")
	if !apierrors.IsConflict(err) {
		t.Fatal(err)
	}
	volume := f.volumes()[0]
	if str(volume, "status") != "Bound" || f.count("retained") != 0 || f.count("release-intents") != 1 {
		t.Fatal(volume)
	}
	class := f.get(classesResource, "", "diskfleet-pool-one")
	class["reclaimPolicy"] = "Delete"
	f.put(classesResource, class)
	if len(f.volumes()) != 0 || f.count("release-intents") != 1 {
		t.Fatal("untrusted class implied deletion of live UID")
	}
}
func TestIntentWriteFailurePreventsDelete(t *testing.T) {
	f := newFixture(t)
	f.boundVolume(true)
	err := f.store.Transaction(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(context.Background(), `CREATE TRIGGER reject_intent BEFORE INSERT ON objects WHEN NEW.kind='release-intents' BEGIN SELECT RAISE(ABORT, 'disk full'); END`)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.cluster.Release(context.Background(), "default~data", "")
	if err == nil || f.deletes != 0 {
		t.Fatal("remote mutation escaped failed intent commit", err)
	}
}
func TestTerminatingAndReplacedRetainedPVStates(t *testing.T) {
	f := newFixture(t)
	f.boundVolume(true)
	f.deleteMode = "deleting"
	if _, err := f.cluster.Release(context.Background(), "default~data", ""); err != nil {
		t.Fatal(err)
	}
	volumes := f.volumes()
	if len(volumes) != 1 || str(volumes[0], "status") != "Releasing" {
		t.Fatal(volumes)
	}
	if err := f.client.Tracker().Delete(pvcsResource, "default", "data"); err != nil {
		t.Fatal(err)
	}
	pv := f.get(pvsResource, "", "pv-data")
	asObject(pv["metadata"])["uid"] = "new-pv"
	f.put(pvsResource, pv)
	volumes = f.volumes()
	if len(volumes) != 1 || str(volumes[0], "status") != "ReleaseRequested" || str(volumes[0], "message") == "" {
		t.Fatal("replaced PV falsely reported Released", volumes)
	}
}
func TestCacheConcurrencyAndReturnedObjectsAreIsolated(t *testing.T) {
	f := newFixture(t)
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 4; j++ {
				page, err := f.cluster.List(context.Background(), "pools", model.ListOptions{})
				if err != nil {
					t.Error(err)
					return
				}
				page.Items[0]["state"] = "client mutation"
				f.cluster.Invalidate()
			}
		}()
	}
	wg.Wait()
	page, err := f.cluster.List(context.Background(), "pools", model.ListOptions{})
	if err != nil || str(page.Items[0], "state") != "Ready" {
		t.Fatal(page, err)
	}
}

func TestQuantityAndLongNodeNaming(t *testing.T) {
	if quantity("1.5Gi") != model.GiB+model.GiB/2 || quantity("9007199254740993") != 9007199254740993 || quantity("invalid") != 0 {
		t.Fatal("quantity lost precision")
	}
	f := newFixture(t)
	if err := f.client.Tracker().Delete(poolsResource, "", "pool-one"); err != nil {
		t.Fatal(err)
	}
	node := strings.Repeat("a", 60) + "." + strings.Repeat("b", 60)
	disk := f.get(disksResource, "", "disk-one")
	asObject(disk["spec"])["nodeName"] = node
	f.put(disksResource, disk)
	plan, err := f.cluster.Plan(context.Background(), object{"name": "long-node", "node": node, "diskIds": []any{"disk-one"}})
	if err != nil {
		t.Fatal(err)
	}
	created, err := f.cluster.CreatePool(context.Background(), str(plan, "planId"), str(plan, "confirmation"))
	if err != nil {
		t.Fatal(err)
	}
	raw := f.get(poolsResource, "", str(created, "id"))
	if str(raw, "spec", "nodeName") != node || len(str(raw, "metadata", "labels", label+"node")) > 63 {
		t.Fatal(raw)
	}
}
