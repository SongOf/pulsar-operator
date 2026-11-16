package cluster

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/SongOf/pulsar-operator/internal/storage/model"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"
)

func testAgentDaemonSet() object {
	return object{"apiVersion": "apps/v1", "kind": "DaemonSet", "metadata": object{"name": agentName, "namespace": agentNamespace, "uid": "agent-ds-uid"}, "spec": object{"template": object{"spec": object{"nodeSelector": object{label + "enabled": "true"}}}}}
}
func testAgentPod(node string) object {
	return object{"apiVersion": "v1", "kind": "Pod", "metadata": object{"name": "agent-one", "namespace": agentNamespace, "uid": "agent-pod-uid", "ownerReferences": []any{object{"kind": "DaemonSet", "name": agentName, "uid": "agent-ds-uid", "controller": true}}}, "spec": object{"nodeName": node}, "status": object{"phase": "Running", "conditions": []any{object{"type": "Ready", "status": "True"}}}}
}
func TestAllNodesEnrollmentAndFreshDiskDetails(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	node := f.get(nodesResource, "", "worker-01")
	asObject(node["metadata"])["name"] = "unselected"
	asObject(node["metadata"])["uid"] = "node-other"
	asObject(at(node, "metadata", "labels"))[label+"enabled"] = "false"
	f.put(nodesResource, node)
	page, err := f.cluster.List(ctx, "nodes", model.ListOptions{})
	if err != nil || page.Total != 2 {
		t.Fatal(page, err)
	}
	detail, err := f.cluster.Node(ctx, "unselected", model.ListOptions{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	got := asObject(detail["node"])
	if got["canEnableAgent"] != true || got["enrolled"] != false || got["agentStatus"] != "NotEnrolled" || detail["disks"].(model.Page).Total != 0 {
		t.Fatal(detail)
	}
	_, err = f.cluster.EnableAgent(ctx, "unselected", object{"uid": "old-uid", "resourceVersion": "1"})
	assertProblem(t, err, 409)
	f.client.ClearActions()
	enabled, err := f.cluster.EnableAgent(ctx, "unselected", object{"uid": "node-other", "resourceVersion": "1"})
	if err != nil || enabled["enrolled"] != true || enabled["agentStatus"] != "Pending" || enabled["canCreatePool"] != false {
		t.Fatal(enabled, err)
	}
	patches := 0
	for _, action := range f.client.Actions() {
		if action.GetVerb() != "patch" {
			continue
		}
		patches++
		if action.GetResource() != nodesResource {
			t.Fatal(action)
		}
		var body object
		if err := json.Unmarshal(action.(k8stesting.PatchAction).GetPatch(), &body); err != nil {
			t.Fatal(err)
		}
		if len(body) != 1 || str(body, "metadata", "uid") != "node-other" || str(body, "metadata", "resourceVersion") != "1" || len(asObject(at(body, "metadata", "labels"))) != 1 || str(body, "metadata", "labels", label+"enabled") != "true" {
			t.Fatal(body)
		}
	}
	if patches != 1 {
		t.Fatal("expected exactly one node patch", patches)
	}
	// A deleted disk remains visible only if Kubernetes still has the object;
	// stale timestamps are recalculated without invalidating the display cache.
	_ = f.plan()
	disks, err := f.cluster.List(ctx, "disks", model.ListOptions{})
	if err != nil || disks.Items[0]["canCreatePool"] != true {
		t.Fatal(disks, err)
	}
	raw := f.get(disksResource, "", "disk-one")
	asObject(raw["status"])["validUntil"] = time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano)
	if err := f.client.Tracker().Update(disksResource, &unstructured.Unstructured{Object: raw}, ""); err != nil {
		t.Fatal(err)
	}
	disks, err = f.cluster.List(ctx, "disks", model.ListOptions{})
	if err != nil || disks.Items[0]["canCreatePool"] != false || disks.Items[0]["fresh"] != false {
		t.Fatal(disks, err)
	}
}
func TestEnrollmentRejectsHealthSelectorMissingDSAndConcurrentReplacement(t *testing.T) {
	for _, scenario := range []string{"missing-ds", "selector", "ds-enabled-selector", "cordoned", "not-ready", "pressure", "deleting", "windows", "unknown-os", "conflict"} {
		t.Run(scenario, func(t *testing.T) {
			f := newFixture(t)
			node := f.get(nodesResource, "", "worker-01")
			asObject(at(node, "metadata", "labels"))[label+"enabled"] = "false"
			switch scenario {
			case "missing-ds":
				if err := f.client.Tracker().Delete(daemonsetsResource, agentNamespace, agentName); err != nil {
					t.Fatal(err)
				}
			case "selector", "ds-enabled-selector":
				ds := f.get(daemonsetsResource, agentNamespace, agentName)
				key := "rack"
				if scenario == "ds-enabled-selector" {
					key = label + "enabled"
				}
				asObject(at(ds, "spec", "template", "spec", "nodeSelector"))[key] = "other"
				f.put(daemonsetsResource, ds)
			case "cordoned":
				node["spec"] = object{"unschedulable": true}
			case "not-ready":
				node["status"] = object{"conditions": []any{object{"type": "Ready", "status": "False"}}}
			case "pressure":
				asObject(node["status"])["conditions"] = append(array(at(node, "status", "conditions")), object{"type": "DiskPressure", "status": "True"})
			case "deleting":
				asObject(node["metadata"])["deletionTimestamp"] = model.Now()
			case "windows":
				asObject(node["status"])["nodeInfo"] = object{"operatingSystem": "windows"}
			case "unknown-os":
				delete(asObject(at(node, "metadata", "labels")), "kubernetes.io/os")
			case "conflict":
				f.client.PrependReactor("patch", "nodes", func(a k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, apierrors.NewConflict(nodesResource.GroupResource(), "worker-01", errors.New("changed"))
				})
			}
			f.put(nodesResource, node)
			f.client.ClearActions()
			_, err := f.cluster.EnableAgent(context.Background(), "worker-01", object{"uid": "node-uid", "resourceVersion": "1"})
			if scenario == "conflict" {
				if !apierrors.IsConflict(err) {
					t.Fatal(err)
				}
				events, listErr := f.store.Events(context.Background(), model.ListOptions{Node: "worker-01", Limit: 100})
				if listErr != nil {
					t.Fatal(listErr)
				}
				actions := map[string]bool{}
				for _, event := range events.Items {
					actions[str(event, "action")] = true
				}
				if !actions["node.agent-enable-requested"] || !actions["node.agent-enable-failed"] {
					t.Fatal(events)
				}
			} else {
				assertProblem(t, err, 409)
				assertNoWrites(t, f.client)
			}
		})
	}
}
func TestPlanAndCommitFailClosedOnLiveNodeAgentAndObservationChanges(t *testing.T) {
	mutations := []string{"node-replaced", "disk-replaced", "agent-owner", "agent-not-ready", "agent-terminating", "cordoned", "pressure", "reasons", "expired", "missing-time", "future-time", "reversed-time", "node-uid", "pool-name", "disk-deleting"}
	for _, stage := range []string{"plan", "commit"} {
		for _, mutation := range mutations {
			t.Run(stage+"/"+mutation, func(t *testing.T) {
				f := newFixture(t)
				plan := f.plan()
				disk := f.get(disksResource, "", "disk-one")
				node := f.get(nodesResource, "", "worker-01")
				pod := f.get(podsResource, agentNamespace, "agent-one")
				switch mutation {
				case "node-replaced":
					asObject(node["metadata"])["uid"] = "replaced"
					asObject(disk["status"])["nodeUID"] = "replaced"
				case "disk-replaced":
					asObject(disk["metadata"])["uid"] = "new-disk"
				case "agent-owner":
					asObject(array(at(pod, "metadata", "ownerReferences"))[0])["uid"] = "old-daemonset"
				case "agent-not-ready":
					asObject(pod["status"])["conditions"] = []any{}
				case "agent-terminating":
					asObject(pod["metadata"])["deletionTimestamp"] = model.Now()
				case "cordoned":
					node["spec"] = object{"unschedulable": true}
				case "pressure":
					asObject(node["status"])["conditions"] = append(array(at(node, "status", "conditions")), object{"type": "MemoryPressure", "status": "Unknown"})
				case "reasons":
					asObject(disk["status"])["reasons"] = []any{"device mounted"}
				case "expired":
					asObject(disk["status"])["validUntil"] = time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano)
				case "missing-time":
					delete(asObject(disk["status"]), "lastObservedTime")
				case "future-time":
					asObject(disk["status"])["lastObservedTime"] = time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
				case "reversed-time":
					asObject(disk["status"])["validUntil"] = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
				case "node-uid":
					asObject(disk["status"])["nodeUID"] = "old-node"
				case "pool-name":
					asObject(disk["status"])["poolName"] = "other-pool"
				case "disk-deleting":
					asObject(disk["metadata"])["deletionTimestamp"] = model.Now()
				}
				f.put(nodesResource, node)
				f.put(podsResource, pod)
				f.put(disksResource, disk)
				f.client.ClearActions()
				var err error
				if stage == "plan" {
					_, err = f.cluster.Plan(context.Background(), object{"name": "retry", "node": "worker-01", "diskIds": []any{"disk-one"}})
				} else {
					_, err = f.cluster.CreatePool(context.Background(), str(plan, "planId"), str(plan, "confirmation"))
				}
				if stage == "plan" && (mutation == "node-replaced" || mutation == "disk-replaced") {
					if err != nil {
						t.Fatal("new valid observation must allow a new plan", err)
					}
				} else {
					assertProblem(t, err, 409)
				}
				assertNoWrites(t, f.client)
			})
		}
	}
}
func TestNodeDetailPaginationAndNameValidation(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	detail, err := f.cluster.Node(ctx, "worker-01", model.ListOptions{Limit: 1, Offset: 1})
	if err != nil || detail["disks"].(model.Page).Total != 1 || len(detail["disks"].(model.Page).Items) != 0 {
		t.Fatal(detail, err)
	}
	_, err = f.cluster.Node(ctx, "../worker-01", model.ListOptions{})
	assertProblem(t, err, 400)
	_, err = f.cluster.Node(ctx, "missing", model.ListOptions{})
	assertProblem(t, err, 404)
	_, err = f.cluster.EnableAgent(ctx, "worker-01", object{"uid": "node-uid"})
	assertProblem(t, err, 400)
	if !strings.Contains(joinReasons([]any{"one", "two"}), "two") {
		t.Fatal("missing reason")
	}
}

func TestCompletedPoolRetryDoesNotRequireLiveInventory(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	plan := f.plan()
	first, err := f.cluster.CreatePool(ctx, str(plan, "planId"), str(plan, "confirmation"))
	if err != nil {
		t.Fatal(err)
	}
	// A successful request retry performs no new remote mutation, including after
	// the node and disk have disappeared or the Agent's observation has expired.
	for _, entry := range []struct {
		resource schema.GroupVersionResource
		name     string
	}{{nodesResource, "worker-01"}, {disksResource, "disk-one"}} {
		if err := f.client.Tracker().Delete(entry.resource, "", entry.name); err != nil {
			t.Fatal(err)
		}
	}
	f.client.ClearActions()
	again, err := f.cluster.CreatePool(ctx, str(plan, "planId"), str(plan, "confirmation"))
	if err != nil || again["id"] != first["id"] {
		t.Fatal(again, err)
	}
	assertNoWrites(t, f.client)
}

func TestPoolApprovalExpiryDuringKubernetesReadsPreventsCreation(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	plan := f.plan()
	deadline := time.Now().Add(time.Second)
	if err := f.store.Transaction(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "UPDATE plans SET expires=? WHERE id=?", float64(deadline.UnixNano())/1e9, str(plan, "planId"))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	reached := false
	f.client.PrependReactor("get", "physicaldisks", func(action k8stesting.Action) (bool, runtime.Object, error) {
		reached = true
		time.Sleep(time.Until(deadline) + 10*time.Millisecond)
		return false, nil, nil
	})
	f.client.ClearActions()
	_, err := f.cluster.CreatePool(ctx, str(plan, "planId"), str(plan, "confirmation"))
	assertProblem(t, err, 409)
	if !reached {
		t.Fatal("test did not cross expiry during the remote read")
	}
	assertNoWrites(t, f.client)
}
