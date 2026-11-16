package controller

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	storagev1 "github.com/SongOf/pulsar-operator/api/storage/v1alpha1"
	"github.com/SongOf/pulsar-operator/internal/agent/model"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	k8stesting "k8s.io/client-go/testing"
)

func observedDisk() model.Disk {
	return model.Disk{Path: "/dev/disk/by-id/wwn-test", Serial: "SER001", SizeBytes: 100 << 30, Model: "TEST", Media: "SSD", State: "Available"}
}

func diskStatus(t *testing.T, client dynamic.Interface, name string) storagev1.PhysicalDiskStatus {
	t.Helper()
	status, _, err := unstructured.NestedMap(get(t, client, Disks, name).Object, "status")
	if err != nil {
		t.Fatal(err)
	}
	var result storagev1.PhysicalDiskStatus
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(status, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func assertObservation(t *testing.T, status storagev1.PhysicalDiskStatus, observed time.Time, validFor time.Duration, uid string) {
	t.Helper()
	if status.LastObservedTime == nil || !status.LastObservedTime.Time.Equal(observed) ||
		status.ValidUntil == nil || !status.ValidUntil.Time.Equal(observed.Add(validFor)) || status.NodeUID != uid {
		t.Fatalf("observation=%+v, want observed=%s validFor=%s nodeUID=%s", status, observed, validFor, uid)
	}
}

func TestSuccessfulDiskObservationUsesConfiguredScanWindow(t *testing.T) {
	for _, tc := range []struct {
		name             string
		interval, window time.Duration
	}{
		{"standalone-default", 0, 36 * time.Second},
		{"configured", 50 * time.Second, 60 * time.Second},
		{"minimum", time.Second, 12 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, client, _ := fixture(t)
			now := r.Now()
			r.Now = func() time.Time { return now }
			r.ReconcileInterval = tc.interval
			disk := observedDisk()
			r.Discover = func(context.Context) ([]model.Disk, error) { return []model.Disk{disk}, nil }
			reconcile(t, r)
			name := DiskName(r.NodeName, disk)
			assertObservation(t, diskStatus(t, client, name), now, tc.window, "uid-node")
			// Protection does not imply failed observation: a current mounted disk
			// remains fresh, but its phase prevents allocation.
			disk.State, disk.Reasons = "Protected", []string{"mounted"}
			now = now.Add(tc.window)
			reconcile(t, r)
			status := diskStatus(t, client, name)
			assertObservation(t, status, now, tc.window, "uid-node")
			if status.Phase != "Protected" {
				t.Fatal("freshness update changed device protection")
			}
		})
	}
}

func TestFailedAndAbsentDiskScansDoNotRenewObservation(t *testing.T) {
	r, client, _ := fixture(t)
	now := r.Now()
	r.Now = func() time.Time { return now }
	disk := observedDisk()
	r.Discover = func(context.Context) ([]model.Disk, error) { return []model.Disk{disk}, nil }
	reconcile(t, r)
	name := DiskName(r.NodeName, disk)
	before := diskStatus(t, client, name)
	now = now.Add(time.Minute)
	// A failed scan can include partial results; none may extend the lease.
	r.Discover = func(context.Context) ([]model.Disk, error) { return []model.Disk{disk}, errors.New("lsblk failed") }
	if r.ReconcileOnce(context.Background()) == nil {
		t.Fatal("failed discovery accepted")
	}
	if actual := diskStatus(t, client, name); !reflect.DeepEqual(actual, before) || actual.ValidUntil.Time.After(now) {
		t.Fatalf("failed scan refreshed expired observation: %+v", actual)
	}
	r.Discover = func(context.Context) ([]model.Disk, error) { return nil, nil }
	reconcile(t, r)
	status := diskStatus(t, client, name)
	if status.Phase != "Offline" {
		t.Fatal("missing disk not offline")
	}
	assertObservation(t, status, before.LastObservedTime.Time, 36*time.Second, before.NodeUID)
	// A later successful scan revives this disk, not the missing observation.
	r.Discover = func(context.Context) ([]model.Disk, error) { return []model.Disk{disk}, nil }
	reconcile(t, r)
	assertObservation(t, diskStatus(t, client, name), now, 36*time.Second, "uid-node")
}

func TestDiskScanRejectsNodeIdentityChanges(t *testing.T) {
	for _, scenario := range []string{"replaced", "deleted", "deleting", "missing-uid", "read-fails"} {
		t.Run(scenario, func(t *testing.T) {
			r, client, _ := fixture(t)
			disk := observedDisk()
			r.Discover = func(context.Context) ([]model.Disk, error) { return []model.Disk{disk}, nil }
			reconcile(t, r)
			name := DiskName(r.NodeName, disk)
			before := diskStatus(t, client, name)
			r.Now = func() time.Time { return before.ValidUntil.Time.Add(time.Second) }
			r.Discover = func(ctx context.Context) ([]model.Disk, error) {
				node := get(t, client, Nodes, r.NodeName)
				switch scenario {
				case "replaced":
					node.SetUID("replacement-node")
				case "deleted":
					if err := client.Resource(Nodes).Delete(ctx, r.NodeName, metav1.DeleteOptions{}); err != nil {
						t.Fatal(err)
					}
					return []model.Disk{disk}, nil
				case "deleting":
					deleted := metav1.NewTime(r.Now())
					node.SetDeletionTimestamp(&deleted)
				case "missing-uid":
					node.SetUID("")
				case "read-fails":
					client.PrependReactor("get", "nodes", func(k8stesting.Action) (bool, runtime.Object, error) {
						return true, nil, errors.New("API unavailable")
					})
					return []model.Disk{disk}, nil
				}
				if _, err := client.Resource(Nodes).Update(ctx, node, metav1.UpdateOptions{}); err != nil {
					t.Fatal(err)
				}
				return []model.Disk{disk}, nil
			}
			if r.ReconcileOnce(context.Background()) == nil {
				t.Fatal("untrusted node observation accepted")
			}
			if actual := diskStatus(t, client, name); !reflect.DeepEqual(actual, before) {
				t.Fatalf("node identity failure refreshed observation: %+v", actual)
			}
			if scenario == "replaced" {
				// A new scan on the new Node incarnation can publish its own UID.
				r.Discover = func(context.Context) ([]model.Disk, error) { return []model.Disk{disk}, nil }
				reconcile(t, r)
				assertObservation(t, diskStatus(t, client, name), r.Now(), 36*time.Second, "replacement-node")
			}
		})
	}
}

func TestRunUsesItsIntervalForFreshness(t *testing.T) {
	for _, interval := range []time.Duration{time.Second, 47 * time.Second} {
		r, client, _ := fixture(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := r.Run(ctx, interval); err != nil {
			t.Fatal(err)
		}
		disk := observedDisk()
		r.Discover = func(context.Context) ([]model.Disk, error) { return []model.Disk{disk}, nil }
		reconcile(t, r)
		effective := max(interval, 10*time.Second)
		// metav1.Time persists whole seconds, matching the API timestamp format.
		window := (effective + effective/5).Truncate(time.Second)
		assertObservation(t, diskStatus(t, client, DiskName(r.NodeName, disk)), r.Now(), window, "uid-node")
	}
}
