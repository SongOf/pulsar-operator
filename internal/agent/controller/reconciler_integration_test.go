//go:build integration

package controller

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/SongOf/pulsar-operator/internal/agent/model"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// The API server enforces the shipped CRD schema, status subresources and
// resourceVersion conflicts. Device observations and LVM operations remain fake.
// Explicitly disable reuse of any user cluster, even if the envtest environment
// variable USE_EXISTING_CLUSTER has been set externally.
func TestIntegrationAgentReconciliationAgainstAPIServer(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Fatal("integration tests require KUBEBUILDER_ASSETS; run the module integration script")
	}
	useExisting := false
	environment := &envtest.Environment{
		UseExistingCluster:       &useExisting,
		BinaryAssetsDirectory:    assets,
		CRDDirectoryPaths:        []string{filepath.Join("..", "..", "..", "config", "storage", "crd")},
		ErrorIfCRDPathMissing:    true,
		ControlPlaneStartTimeout: time.Minute,
	}
	config, err := environment.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Error(err)
		}
	})
	client, err := dynamic.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_, err = client.Resource(Nodes).Create(ctx, &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1", "kind": "Node",
		"metadata": map[string]interface{}{"name": "worker-01", "labels": map[string]interface{}{"kubernetes.io/hostname": "actual-host"}},
	}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	requested := makePool()
	requested.SetResourceVersion("")
	requested.SetUID("")
	requested.SetGeneration(0)
	created, err := client.Resource(Pools).Create(ctx, requested, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	backend := &fakeBackend{}
	disk := model.Disk{Path: "/dev/disk/by-id/wwn-test", Serial: "SER001", SizeBytes: 100 << 30, Model: "TEST", Media: "SSD", State: "Available"}
	r := &Reconciler{
		Client: client, Backend: backend, NodeName: "worker-01", WritesEnabled: true,
		Discover: func(context.Context) ([]model.Disk, error) { return []model.Disk{disk}, nil },
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if err := r.ReconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	poolName, diskName, className := created.GetName(), DiskName(r.NodeName, disk), StorageClassName(created.GetName())
	ready := get(t, client, Pools, poolName)
	physical := get(t, client, Disks, diskName)
	class := get(t, client, Classes, className)
	if phase(t, client, Pools, poolName) != "Ready" || !reflect.DeepEqual(ready.GetFinalizers(), []string{Finalizer}) {
		t.Fatalf("pool did not become Ready with retention protection: %+v", ready.Object)
	}
	if ready.GetResourceVersion() == created.GetResourceVersion() || len(backend.calls) != 1 || backend.calls[0].uid != string(created.GetUID()) {
		t.Fatal("actual API server identity or updated resourceVersion was not used")
	}
	observed, _, err := unstructured.NestedInt64(ready.Object, "status", "observedGeneration")
	if err != nil || observed != ready.GetGeneration() {
		t.Fatalf("unexpected observedGeneration: %d, error: %v", observed, err)
	}
	var spec poolSpec
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(ready.Object["spec"].(map[string]interface{}), &spec); err != nil {
		t.Fatal(err)
	}
	if err := validateClass(class, storageClass(pool{object: ready, spec: spec}, "actual-host")); err != nil {
		t.Fatal(err)
	}
	if phase(t, client, Disks, diskName) != "Managed" || physical.GetResourceVersion() == "" {
		t.Fatalf("PhysicalDisk status was not persisted: %+v", physical.Object)
	}

	t.Run("unchanged reconcile preserves resource versions", func(t *testing.T) {
		if err := r.ReconcileOnce(ctx); err != nil {
			t.Fatal(err)
		}
		if get(t, client, Pools, poolName).GetResourceVersion() != ready.GetResourceVersion() ||
			get(t, client, Disks, diskName).GetResourceVersion() != physical.GetResourceVersion() ||
			get(t, client, Classes, className).GetResourceVersion() != class.GetResourceVersion() {
			t.Fatal("unchanged desired state caused an API write")
		}
	})

	t.Run("stale status update conflicts", func(t *testing.T) {
		stale := get(t, client, Pools, poolName)
		changed := stale.DeepCopy()
		changed.SetAnnotations(map[string]string{"test.storage.diskfleet.io/concurrent-update": "true"})
		if _, err := client.Resource(Pools).Update(ctx, changed, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
		if err := r.status(ctx, Pools, stale, map[string]interface{}{"phase": "Error", "message": "stale writer"}); !apierrors.IsConflict(err) {
			t.Fatalf("expected resourceVersion conflict, got %v", err)
		}
		if phase(t, client, Pools, poolName) != "Ready" {
			t.Fatal("stale status write overwrote the current object")
		}
	})

	t.Run("stale finalizer update conflicts", func(t *testing.T) {
		other := requested.DeepCopy()
		other.SetName("finalizer-race")
		if err := unstructured.SetNestedField(other.Object, "df_finalizer", "spec", "vgName"); err != nil {
			t.Fatal(err)
		}
		other.Object["spec"].(map[string]interface{})["devices"] = []interface{}{map[string]interface{}{
			"path": "/dev/disk/by-id/wwn-finalizer", "serial": "FINALIZER", "sizeBytes": int64(100 << 30),
		}}
		stale, err := client.Resource(Pools).Create(ctx, other, metav1.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := client.Resource(Pools).Delete(ctx, other.GetName(), metav1.DeleteOptions{}); err != nil {
				t.Error(err)
			}
		}()
		changed := stale.DeepCopy()
		changed.SetAnnotations(map[string]string{"test.storage.diskfleet.io/concurrent-update": "true"})
		if _, err := client.Resource(Pools).Update(ctx, changed, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
		if err := r.ensureFinalizer(ctx, stale); !apierrors.IsConflict(err) {
			t.Fatalf("expected resourceVersion conflict adding finalizer, got %v", err)
		}
		if len(get(t, client, Pools, other.GetName()).GetFinalizers()) != 0 {
			t.Fatal("stale finalizer patch changed the current object")
		}
	})

	t.Run("CRD rejects immutable device changes", func(t *testing.T) {
		currentPool := get(t, client, Pools, poolName)
		poolPatch, err := json.Marshal(map[string]interface{}{
			"metadata": map[string]interface{}{"resourceVersion": currentPool.GetResourceVersion()},
			"spec": map[string]interface{}{"devices": []interface{}{map[string]interface{}{
				"path": disk.Path, "serial": "REPLACEMENT", "sizeBytes": disk.SizeBytes,
			}}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.Resource(Pools).Patch(ctx, poolName, types.MergePatchType, poolPatch, metav1.PatchOptions{}); !apierrors.IsInvalid(err) {
			t.Fatalf("DiskPool device identity changed: %v", err)
		}
		currentDisk := get(t, client, Disks, diskName)
		diskPatch, err := json.Marshal(map[string]interface{}{
			"metadata": map[string]interface{}{"resourceVersion": currentDisk.GetResourceVersion()},
			"spec":     map[string]interface{}{"sizeBytes": disk.SizeBytes * 2},
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.Resource(Disks).Patch(ctx, diskName, types.MergePatchType, diskPatch, metav1.PatchOptions{}); !apierrors.IsInvalid(err) {
			t.Fatalf("PhysicalDisk byte capacity changed: %v", err)
		}
		if !reflect.DeepEqual(get(t, client, Disks, diskName).Object["spec"], physical.Object["spec"]) {
			t.Fatal("immutable inventory fields were modified")
		}
	})

	t.Run("deletion retains media and class", func(t *testing.T) {
		before := len(backend.calls)
		if err := client.Resource(Pools).Delete(ctx, poolName, metav1.DeleteOptions{}); err != nil {
			t.Fatal(err)
		}
		if err := r.ReconcileOnce(ctx); err != nil {
			t.Fatal(err)
		}
		retained := get(t, client, Pools, poolName)
		if phase(t, client, Pools, poolName) != "Retained" || retained.GetDeletionTimestamp() == nil ||
			!reflect.DeepEqual(retained.GetFinalizers(), []string{Finalizer}) || len(backend.calls) != before {
			t.Fatalf("deleting pool bypassed retention: %+v", retained.Object)
		}
		if get(t, client, Classes, className).GetUID() != class.GetUID() {
			t.Fatal("retained StorageClass was replaced")
		}
	})
}
