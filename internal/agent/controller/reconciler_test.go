package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/SongOf/pulsar-operator/internal/agent/model"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"
)

type backendCall struct {
	vg, uid          string
	devices          []model.Device
	approved, legacy bool
}
type fakeBackend struct {
	calls []backendCall
	err   error
	after func()
}

func (b *fakeBackend) EnsurePool(_ context.Context, vg string, devices []model.Device, approved bool, uid string, legacy bool) (model.PoolResult, error) {
	b.calls = append(b.calls, backendCall{vg, uid, devices, approved, legacy})
	if b.after != nil {
		b.after()
	}
	return model.PoolResult{VGName: vg, CapacityBytes: 90 << 30, AvailableBytes: 80 << 30}, b.err
}

func makePool() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": Group + "/" + Version, "kind": "DiskPool",
		"metadata": map[string]interface{}{"name": "ssd-01", "uid": "uid-pool", "resourceVersion": "1", "generation": int64(1), "labels": map[string]interface{}{NodeLabel: "worker-01"}},
		"spec": map[string]interface{}{"nodeName": "worker-01", "vgName": "df_ssd01", "approved": true,
			"devices": []interface{}{map[string]interface{}{"path": "/dev/disk/by-id/wwn-test", "serial": "SER001", "sizeBytes": int64(100 << 30)}},
		},
	}}
}

func fixture(t *testing.T, objects ...runtime.Object) (*Reconciler, *fake.FakeDynamicClient, *fakeBackend) {
	t.Helper()
	node := &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "v1", "kind": "Node", "metadata": map[string]interface{}{
		"name": "worker-01", "labels": map[string]interface{}{"kubernetes.io/hostname": "actual-host"},
	}}}
	objects = append(objects, node)
	client := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		Pools: "DiskPoolList", Disks: "PhysicalDiskList", Classes: "StorageClassList", Nodes: "NodeList",
	}, objects...)
	client.PrependReactor("create", "*", func(action k8stesting.Action) (bool, runtime.Object, error) {
		obj := action.(k8stesting.CreateAction).GetObject().(*unstructured.Unstructured)
		obj.SetResourceVersion("1")
		obj.SetUID(types.UID("uid-" + obj.GetName()))
		return false, nil, nil
	})
	backend := &fakeBackend{}
	r := &Reconciler{Client: client, Backend: backend, NodeName: "worker-01", WritesEnabled: true,
		Discover: func(context.Context) ([]model.Disk, error) { return nil, nil },
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	return r, client, backend
}

func get(t *testing.T, client dynamic.Interface, resource schema.GroupVersionResource, name string) *unstructured.Unstructured {
	t.Helper()
	obj, err := client.Resource(resource).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return obj
}

func phase(t *testing.T, client dynamic.Interface, resource schema.GroupVersionResource, name string) string {
	t.Helper()
	value, _, err := unstructured.NestedString(get(t, client, resource, name).Object, "status", "phase")
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func reconcile(t *testing.T, r *Reconciler) {
	t.Helper()
	if err := r.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestPoolLifecycleAndStableStatus(t *testing.T) {
	r, client, backend := fixture(t, makePool())
	reconcile(t, r)
	if len(backend.calls) != 1 || backend.calls[0].uid != "uid-pool" || !backend.calls[0].approved || backend.calls[0].legacy {
		t.Fatalf("calls: %+v", backend.calls)
	}
	obj := get(t, client, Pools, "ssd-01")
	if phase(t, client, Pools, "ssd-01") != "Ready" || !reflect.DeepEqual(obj.GetFinalizers(), []string{Finalizer}) {
		t.Fatal(obj)
	}
	sc := get(t, client, Classes, "diskfleet-ssd-01")
	if sc.Object["reclaimPolicy"] != "Retain" || sc.Object["volumeBindingMode"] != "WaitForFirstConsumer" || sc.GetAnnotations()[Group+"/pool-uid"] != "uid-pool" {
		t.Fatal(sc)
	}
	parameters := sc.Object["parameters"].(map[string]interface{})
	if parameters["vgpattern"] != "^df_ssd01$" || parameters["thinProvision"] != "no" {
		t.Fatal(parameters)
	}
	host, _, _ := unstructured.NestedString(obj.Object, "status", "topologyHostname")
	if host != "actual-host" || !strings.Contains(fmt.Sprint(sc.Object["allowedTopologies"]), "actual-host") {
		t.Fatal(obj, sc)
	}
	client.ClearActions()
	reconcile(t, r)
	for _, action := range client.Actions() {
		if action.GetVerb() == "patch" {
			t.Fatalf("unchanged status was patched: %v", action)
		}
	}
}

func TestApprovalWriteSwitchAndDeletion(t *testing.T) {
	for _, scenario := range []struct {
		name, phase                 string
		approved, enabled, deleting bool
	}{
		{"not-approved", "PendingApproval", false, true, false},
		{"writes-disabled", "WritesDisabled", true, false, false},
		{"retained", "Retained", true, true, true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			obj := makePool()
			_ = unstructured.SetNestedField(obj.Object, scenario.approved, "spec", "approved")
			if scenario.deleting {
				obj.SetDeletionTimestamp(&metav1.Time{Time: metav1.Now().Time})
				obj.SetFinalizers([]string{Finalizer})
			}
			r, client, backend := fixture(t, obj)
			r.WritesEnabled = scenario.enabled
			reconcile(t, r)
			if len(backend.calls) != 0 || phase(t, client, Pools, "ssd-01") != scenario.phase {
				t.Fatal(backend.calls, get(t, client, Pools, "ssd-01"))
			}
			if !reflect.DeepEqual(get(t, client, Pools, "ssd-01").GetFinalizers(), []string{Finalizer}) {
				t.Fatal("retention finalizer missing")
			}
		})
	}
}

func TestClaimConflictsIncludeUnlabelledPools(t *testing.T) {
	for _, match := range []string{"vg", "serial", "path"} {
		t.Run(match, func(t *testing.T) {
			other := makePool()
			other.SetName("other-pool")
			other.SetUID("other-uid")
			other.SetLabels(nil)
			spec := other.Object["spec"].(map[string]interface{})
			device := spec["devices"].([]interface{})[0].(map[string]interface{})
			if match != "vg" {
				spec["vgName"] = "df_other"
			}
			if match != "serial" {
				device["serial"] = "OTHER"
			}
			if match != "path" {
				device["path"] = "/dev/disk/by-id/other"
			}
			r, client, backend := fixture(t, makePool(), other)
			reconcile(t, r)
			if len(backend.calls) != 0 || phase(t, client, Pools, "ssd-01") != "Error" {
				t.Fatal("conflict reached disk backend")
			}
		})
	}
}

func TestForeignNodeIgnoredDespiteLabel(t *testing.T) {
	obj := makePool()
	_ = unstructured.SetNestedField(obj.Object, "worker-02", "spec", "nodeName")
	r, client, backend := fixture(t, obj)
	reconcile(t, r)
	if len(backend.calls) != 0 {
		t.Fatal("foreign device reached backend")
	}
	for _, action := range client.Actions() {
		if action.GetVerb() == "patch" {
			t.Fatal("foreign pool mutated")
		}
	}
}

func TestExistingClassAndLegacyOwnership(t *testing.T) {
	for _, scenario := range []string{"owned-ready", "owner-mismatch", "drift", "no-status-proof"} {
		t.Run(scenario, func(t *testing.T) {
			obj := makePool()
			obj.Object["status"] = map[string]interface{}{"phase": "Ready", "storageClass": "diskfleet-ssd-01", "topologyHostname": "actual-host"}
			var spec poolSpec
			_ = runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object["spec"].(map[string]interface{}), &spec)
			sc := storageClass(pool{obj, spec}, "actual-host")
			switch scenario {
			case "owner-mismatch":
				sc.SetAnnotations(map[string]string{Group + "/pool-uid": "other"})
			case "drift":
				sc.Object["reclaimPolicy"] = "Delete"
			case "no-status-proof":
				delete(obj.Object, "status")
			}
			r, client, backend := fixture(t, obj, sc)
			reconcile(t, r)
			if scenario == "owner-mismatch" || scenario == "drift" {
				if len(backend.calls) != 0 || phase(t, client, Pools, "ssd-01") != "Error" {
					t.Fatal("untrusted class reached backend")
				}
			} else if len(backend.calls) != 1 || backend.calls[0].legacy != (scenario == "owned-ready") {
				t.Fatalf("legacy proof: %+v", backend.calls)
			}
		})
	}
}

func TestInvalidTopologyBlocksEveryPoolInCycle(t *testing.T) {
	other := makePool()
	other.SetName("second")
	other.SetUID("second-uid")
	spec := other.Object["spec"].(map[string]interface{})
	spec["vgName"] = "df_second"
	device := spec["devices"].([]interface{})[0].(map[string]interface{})
	device["path"] = "/dev/disk/by-id/second"
	device["serial"] = "SECOND"
	r, client, backend := fixture(t, makePool(), other)
	node := get(t, client, Nodes, "worker-01")
	node.SetLabels(map[string]string{"kubernetes.io/hostname": strings.Repeat("x", 64)})
	_, _ = client.Resource(Nodes).Update(context.Background(), node, metav1.UpdateOptions{})
	reconcile(t, r)
	if len(backend.calls) != 0 {
		t.Fatal("invalid hostname reused across pools")
	}
}

func TestClassRevalidatedAfterDiskOperations(t *testing.T) {
	for _, replacement := range []string{"deleted", "foreign"} {
		t.Run(replacement, func(t *testing.T) {
			obj := makePool()
			var spec poolSpec
			_ = runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object["spec"].(map[string]interface{}), &spec)
			sc := storageClass(pool{obj, spec}, "actual-host")
			r, client, backend := fixture(t, obj, sc)
			backend.after = func() {
				if err := client.Resource(Classes).Delete(context.Background(), sc.GetName(), metav1.DeleteOptions{}); err != nil {
					t.Fatal(err)
				}
				if replacement == "foreign" {
					sc.SetAnnotations(map[string]string{Group + "/pool-uid": "other"})
					if _, err := client.Resource(Classes).Create(context.Background(), sc, metav1.CreateOptions{}); err != nil {
						t.Fatal(err)
					}
				}
			}
			reconcile(t, r)
			want := "Ready"
			if replacement == "foreign" {
				want = "Error"
			}
			if actual := phase(t, client, Pools, "ssd-01"); actual != want {
				t.Fatalf("phase=%s want=%s", actual, want)
			}
			if replacement == "deleted" && get(t, client, Classes, sc.GetName()).GetAnnotations()[Group+"/pool-uid"] != "uid-pool" {
				t.Fatal("missing replacement class")
			}
		})
	}
}

func TestDiscoveryIdentityAndOffline(t *testing.T) {
	r, client, _ := fixture(t)
	disk := model.Disk{Path: "/dev/disk/by-id/wwn-test", Serial: "SER001", SizeBytes: 100 << 30, Model: "TEST", Media: "SSD", State: "Available"}
	r.Discover = func(context.Context) ([]model.Disk, error) { return []model.Disk{disk}, nil }
	reconcile(t, r)
	name := DiskName(r.NodeName, disk)
	if phase(t, client, Disks, name) != "Available" {
		t.Fatal("disk not available")
	}
	disk.SizeBytes *= 2
	reconcile(t, r)
	if phase(t, client, Disks, name) != "Protected" {
		t.Fatal("changed identity accepted")
	}
	size, _, _ := unstructured.NestedInt64(get(t, client, Disks, name).Object, "spec", "sizeBytes")
	if size != 100<<30 {
		t.Fatal("immutable disk identity changed")
	}
	r.Discover = func(context.Context) ([]model.Disk, error) { return nil, fmt.Errorf("lsblk failed") }
	if r.ReconcileOnce(context.Background()) == nil {
		t.Fatal("failed inventory was accepted")
	}
	if phase(t, client, Disks, name) != "Protected" {
		t.Fatal("failed discovery marked disk offline")
	}
	r.Discover = func(context.Context) ([]model.Disk, error) { return nil, nil }
	reconcile(t, r)
	if phase(t, client, Disks, name) != "Offline" {
		t.Fatal("missing device not offline")
	}
}

func TestNamingCompatibleWithPython(t *testing.T) {
	// Fixed vectors generated with the original diskfleet.operator helpers.
	if NodeLabelValue("worker-01") != "worker-01" {
		t.Fatal("short label changed")
	}
	if len(NodeLabelValue(strings.Repeat("a", 100))) > 63 || len(StorageClassName(strings.Repeat("a", 100))) > 63 {
		t.Fatal("unbounded name")
	}
	if StorageClassName(strings.Repeat("a", 100)) != "diskfleet-"+strings.Repeat("a", 44)+"-28165978" {
		t.Fatal("storage class naming changed")
	}
	if NodeLabelValue(strings.Repeat("a", 100)) != "sha256-2816597888e4a0d3a36b82b83316ab32680eb8f0" {
		t.Fatal("node hashing changed")
	}
	if DiskName("worker-01", model.Disk{Serial: "SER001", Path: "/dev/disk/by-id/wwn-test"}) != "disk-9f3fddb8e4031687f5d9d6da50cc6f5b" {
		t.Fatal("physical disk name changed")
	}
}

func TestPaginationAndResourceVersionOverHTTP(t *testing.T) {
	ctx := context.Background()
	obj := makePool()
	other := makePool()
	other.SetName("other")
	other.SetUID("other")
	other.SetLabels(nil)
	pages, patches := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case req.Method == "GET" && strings.HasSuffix(req.URL.Path, "/diskpools"):
			pages++
			if req.URL.Query().Get("limit") != "500" {
				t.Error("missing page limit")
			}
			list := &unstructured.UnstructuredList{Object: map[string]interface{}{"apiVersion": Group + "/" + Version, "kind": "DiskPoolList"}}
			if req.URL.Query().Get("continue") == "" {
				list.Items = []unstructured.Unstructured{*obj.DeepCopy()}
				list.SetContinue("next")
			} else {
				list.Items = []unstructured.Unstructured{*other}
			}
			_ = json.NewEncoder(w).Encode(list)
		case req.Method == "GET" && strings.HasSuffix(req.URL.Path, "/physicaldisks"):
			_, _ = io.WriteString(w, `{"apiVersion":"storage.diskfleet.io/v1alpha1","kind":"PhysicalDiskList","items":[]}`)
		case req.Method == "PATCH":
			patches++
			var body struct {
				Metadata struct {
					ResourceVersion string   `json:"resourceVersion"`
					Finalizers      []string `json:"finalizers"`
				} `json:"metadata"`
				Status map[string]interface{} `json:"status"`
			}
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if body.Metadata.ResourceVersion != obj.GetResourceVersion() {
				t.Error("stale resourceVersion")
			}
			if body.Status != nil {
				if !strings.HasSuffix(req.URL.Path, "/status") || body.Status["phase"] != "Error" {
					t.Error("missing conflict status subresource")
				}
				obj.Object["status"] = body.Status
			} else {
				obj.SetFinalizers(body.Metadata.Finalizers)
			}
			obj.SetResourceVersion(fmt.Sprint(patches + 1))
			_ = json.NewEncoder(w).Encode(obj)
		default:
			t.Errorf("unexpected API request: %s %s", req.Method, req.URL)
			http.NotFound(w, req)
		}
	}))
	defer server.Close()
	client, err := dynamic.NewForConfig(&rest.Config{Host: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	backend := &fakeBackend{}
	r := &Reconciler{Client: client, Backend: backend, NodeName: "worker-01", WritesEnabled: true, Discover: func(context.Context) ([]model.Disk, error) { return nil, nil }, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := r.ReconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if pages != 2 || patches != 2 || len(backend.calls) != 0 {
		t.Fatalf("pages=%d patches=%d calls=%v", pages, patches, backend.calls)
	}
}
