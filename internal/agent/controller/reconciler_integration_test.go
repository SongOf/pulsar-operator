//go:build integration

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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
	now := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	disk := model.Disk{Path: "/dev/disk/by-id/wwn-test", Serial: "SER001", SizeBytes: 100 << 30, Model: "TEST", Media: "SSD", State: "Available"}
	r := &Reconciler{
		Client: client, Backend: backend, NodeName: "worker-01", WritesEnabled: true,
		Discover: func(context.Context) ([]model.Disk, error) { return []model.Disk{disk}, nil },
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:      func() time.Time { return now },
	}
	if err := r.ReconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	poolName, diskName, className := created.GetName(), DiskName(r.NodeName, disk), StorageClassName(created.GetName())
	awaiting := get(t, client, Pools, poolName)
	if phase(t, client, Pools, poolName) != "AwaitingReview" || len(backend.calls) != 1 || len(backend.benchmarkCalls) != 1 {
		t.Fatalf("benchmark did not stop at the manual approval gate: %+v", awaiting.Object)
	}
	assertNoClass(t, client, poolName)
	if err := r.ReconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if get(t, client, Pools, poolName).GetResourceVersion() != awaiting.GetResourceVersion() || len(backend.benchmarkCalls) != 1 {
		t.Fatal("unreviewed result was rerun or rewritten")
	}
	assertNoClass(t, client, poolName)
	t.Run("schema rejects incomplete or blank rejection", func(t *testing.T) {
		for _, note := range []string{"", " \t\n", "\u00a0\u2003\u0085", strings.Repeat("界", 1025)} {
			obj := get(t, client, Pools, poolName)
			setHumanReview(t, obj, "Rejected", note)
			if _, err := client.Resource(Pools).Update(ctx, obj, metav1.UpdateOptions{}); !apierrors.IsInvalid(err) {
				t.Fatalf("invalid rejection note accepted: %v", err)
			}
		}
		for _, key := range []string{"poolUID", "runID", "resultDigest", "decision", "reviewedAt"} {
			obj := get(t, client, Pools, poolName)
			setHumanReview(t, obj, "Approved", "")
			unstructured.RemoveNestedField(obj.Object, "spec", "allocationApproval", key)
			if _, err := client.Resource(Pools).Update(ctx, obj, metav1.UpdateOptions{}); !apierrors.IsInvalid(err) {
				t.Fatalf("approval without %s accepted: %v", key, err)
			}
		}
	})
	reviewPool(t, client, poolName, "Rejected", "review requires follow-up")
	if err := r.ReconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if phase(t, client, Pools, poolName) != "Rejected" {
		t.Fatal("rejection not reflected")
	}
	assertNoClass(t, client, poolName)
	// This spec update represents the user's separate review, not controller IO.
	reviewPool(t, client, poolName, "Approved", "")
	if err := r.ReconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	ready := get(t, client, Pools, poolName)
	physical := get(t, client, Disks, diskName)
	class := get(t, client, Classes, className)
	if phase(t, client, Pools, poolName) != "Ready" || !reflect.DeepEqual(ready.GetFinalizers(), []string{Finalizer}) {
		t.Fatalf("pool did not become Ready with retention protection: %+v", ready.Object)
	}
	if ready.GetResourceVersion() == created.GetResourceVersion() || len(backend.calls) != 2 || backend.calls[0].uid != string(created.GetUID()) || len(backend.benchmarkCalls) != 1 {
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
	nodeUID := string(get(t, client, Nodes, r.NodeName).GetUID())
	assertObservation(t, diskStatus(t, client, diskName), now, 36*time.Second, nodeUID)

	t.Run("approved review cannot be changed or removed", func(t *testing.T) {
		for _, field := range []string{"remove", "note", "decision", "runID", "resultDigest", "reviewedAt", "poolUID"} {
			obj := get(t, client, Pools, poolName)
			if field == "remove" {
				unstructured.RemoveNestedField(obj.Object, "spec", "allocationApproval")
			} else {
				value := "changed"
				switch field {
				case "decision":
					value = "Rejected"
				case "runID":
					value = strings.Repeat("a", 32)
				case "resultDigest":
					value = strings.Repeat("a", 64)
				case "reviewedAt":
					value = "2026-11-16T01:00:00Z"
				}
				if err := unstructured.SetNestedField(obj.Object, value, "spec", "allocationApproval", field); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := client.Resource(Pools).Update(ctx, obj, metav1.UpdateOptions{}); !apierrors.IsInvalid(err) {
				t.Fatalf("approved %s was mutable: %v", field, err)
			}
		}
	})

	t.Run("benchmark schema rejects malformed report fields", func(t *testing.T) {
		for _, field := range []string{"runID", "resultDigest", "profile", "phase", "testSizeBytes", "negative metric", "missing metric", "duplicate workload", "too many workloads"} {
			obj := get(t, client, Pools, poolName)
			status, _, err := unstructured.NestedMap(obj.Object, "status")
			if err != nil {
				t.Fatal(err)
			}
			b := status["benchmark"].(map[string]interface{})
			switch field {
			case "runID", "resultDigest":
				b[field] = "invalid-hex"
			case "profile", "phase":
				b[field] = "unknown"
			case "testSizeBytes":
				b[field] = int64(2 << 30)
			case "negative metric":
				b["workloads"].([]interface{})[0].(map[string]interface{})["latencyMeanNs"] = float64(-1)
			case "missing metric":
				delete(b["workloads"].([]interface{})[0].(map[string]interface{}), "iops")
			case "duplicate workload":
				b["workloads"].([]interface{})[1].(map[string]interface{})["name"] = "seq-write"
			case "too many workloads":
				b["workloads"] = append(b["workloads"].([]interface{}), b["workloads"].([]interface{})[0])
			}
			if err := r.status(ctx, Pools, obj, status); !apierrors.IsInvalid(err) {
				t.Fatalf("invalid report %s accepted: %v", field, err)
			}
		}
	})

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

	t.Run("successful scan refreshes only the physical observation", func(t *testing.T) {
		now = now.Add(30 * time.Second)
		if err := r.ReconcileOnce(ctx); err != nil {
			t.Fatal(err)
		}
		assertObservation(t, diskStatus(t, client, diskName), now, 36*time.Second, nodeUID)
		if get(t, client, Disks, diskName).GetResourceVersion() == physical.GetResourceVersion() ||
			get(t, client, Pools, poolName).GetResourceVersion() != ready.GetResourceVersion() ||
			get(t, client, Classes, className).GetResourceVersion() != class.GetResourceVersion() {
			t.Fatal("observation refresh must update the disk without unnecessary pool/class writes")
		}
	})

	t.Run("schema preserves optional timestamps and rejects malformed evidence", func(t *testing.T) {
		for _, change := range []map[string]interface{}{
			{"lastObservedTime": "not-a-time"}, {"validUntil": "2026-99-99T00:00:00Z"}, {"nodeUID": ""},
		} {
			current := get(t, client, Disks, diskName)
			status, _, err := unstructured.NestedMap(current.Object, "status")
			if err != nil {
				t.Fatal(err)
			}
			for key, value := range change {
				status[key] = value
			}
			if err := r.status(ctx, Disks, current, status); !apierrors.IsInvalid(err) {
				t.Fatalf("invalid freshness evidence accepted (%v): %v", change, err)
			}
		}
		// Old resources remain schema-compatible without the optional fields.
		legacy := physical.DeepCopy()
		legacy.SetName("legacy-observation")
		legacy.SetUID("")
		legacy.SetResourceVersion("")
		delete(legacy.Object, "status")
		created, err := client.Resource(Disks).Create(ctx, legacy, metav1.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if err := r.status(ctx, Disks, created, map[string]interface{}{"phase": "Protected"}); err != nil {
			t.Fatal(err)
		}
		if err := client.Resource(Disks).Delete(ctx, legacy.GetName(), metav1.DeleteOptions{}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("failed and missing scans expire without renewal", func(t *testing.T) {
		before := diskStatus(t, client, diskName)
		now = now.Add(time.Minute)
		r.Discover = func(context.Context) ([]model.Disk, error) { return []model.Disk{disk}, errors.New("scan failed") }
		if r.ReconcileOnce(ctx) == nil {
			t.Fatal("failed scan accepted")
		}
		if actual := diskStatus(t, client, diskName); !reflect.DeepEqual(actual, before) || actual.ValidUntil.Time.After(now) {
			t.Fatalf("failed observation renewed: %+v", actual)
		}
		r.Discover = func(context.Context) ([]model.Disk, error) { return nil, nil }
		if err := r.ReconcileOnce(ctx); err != nil {
			t.Fatal(err)
		}
		status := diskStatus(t, client, diskName)
		if status.Phase != "Offline" {
			t.Fatal("missing disk not Offline")
		}
		assertObservation(t, status, before.LastObservedTime.Time, 36*time.Second, nodeUID)
		r.Discover = func(context.Context) ([]model.Disk, error) { return []model.Disk{disk}, nil }
	})

	t.Run("same-name Node recreation requires a new successful observation", func(t *testing.T) {
		before := diskStatus(t, client, diskName)
		node := get(t, client, Nodes, r.NodeName)
		if err := client.Resource(Nodes).Delete(ctx, r.NodeName, metav1.DeleteOptions{}); err != nil {
			t.Fatal(err)
		}
		node.SetUID("")
		node.SetResourceVersion("")
		node.SetCreationTimestamp(metav1.Time{})
		replacement, err := client.Resource(Nodes).Create(ctx, node, metav1.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if replacement.GetUID() == types.UID(nodeUID) {
			t.Fatal("Node was not recreated")
		}
		r.Discover = func(context.Context) ([]model.Disk, error) { return nil, errors.New("scan failed") }
		if r.ReconcileOnce(ctx) == nil {
			t.Fatal("failed scan accepted")
		}
		if actual := diskStatus(t, client, diskName); !reflect.DeepEqual(actual, before) {
			t.Fatal("replacement Node renewed an old observation without a successful scan")
		}
		r.Discover = func(context.Context) ([]model.Disk, error) { return []model.Disk{disk}, nil }
		if err := r.ReconcileOnce(ctx); err != nil {
			t.Fatal(err)
		}
		nodeUID = string(replacement.GetUID())
		assertObservation(t, diskStatus(t, client, diskName), now, 36*time.Second, nodeUID)
	})

	t.Run("stale status update conflicts", func(t *testing.T) {
		stale := get(t, client, Pools, poolName)
		previousPhase := phase(t, client, Pools, poolName)
		changed := stale.DeepCopy()
		changed.SetAnnotations(map[string]string{"test.storage.diskfleet.io/concurrent-update": "true"})
		if _, err := client.Resource(Pools).Update(ctx, changed, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
		if err := r.status(ctx, Pools, stale, map[string]interface{}{"phase": "Error", "message": "stale writer"}); !apierrors.IsConflict(err) {
			t.Fatalf("expected resourceVersion conflict, got %v", err)
		}
		if phase(t, client, Pools, poolName) != previousPhase {
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
