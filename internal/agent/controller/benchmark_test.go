package controller

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	storagev1 "github.com/SongOf/pulsar-operator/api/storage/v1alpha1"
	"github.com/SongOf/pulsar-operator/internal/agent/model"
	"github.com/SongOf/pulsar-operator/internal/storage/poolstate"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestUnsafeBenchmarkResultsNeverPublishStorageClass(t *testing.T) {
	cases := map[string]func(*storagev1.DiskPoolBenchmark){
		"failed":           func(b *storagev1.DiskPoolBenchmark) { b.Phase = "Failed" },
		"interrupted":      func(b *storagev1.DiskPoolBenchmark) { b.Phase = "Interrupted" },
		"blocked":          func(b *storagev1.DiskPoolBenchmark) { b.Phase = "Blocked" },
		"cleanup pending":  func(b *storagev1.DiskPoolBenchmark) { b.CleanupVerified = false },
		"device errors":    func(b *storagev1.DiskPoolBenchmark) { b.Workloads[0].Errors = 1 },
		"missing workload": func(b *storagev1.DiskPoolBenchmark) { b.Workloads = b.Workloads[:3] },
		"wrong identity":   func(b *storagev1.DiskPoolBenchmark) { b.Device.Serial = "REPLACED" },
		"execution error":  func(b *storagev1.DiskPoolBenchmark) { b.ErrorCode = "FioFailed" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			r, client, backend := fixture(t, makePool())
			backend.benchmarkResult = func(req model.BenchmarkRequest) (storagev1.DiskPoolBenchmark, error) {
				result := successfulBenchmark(req)
				mutate(&result)
				result.ResultDigest = poolstate.Digest(result)
				return result, nil
			}
			reconcile(t, r)
			assertNoClass(t, client, "ssd-01")
			if phase(t, client, Pools, "ssd-01") != "BenchmarkFailed" {
				t.Fatal("unsafe report was reviewable")
			}
			// Even a manually forged approval cannot substitute for safe evidence.
			reviewPool(t, client, "ssd-01", "Approved", "")
			reconcile(t, r)
			assertNoClass(t, client, "ssd-01")
			if len(backend.calls) != 1 {
				t.Fatal("unsafe report reached allocation verification")
			}
		})
	}
}

func TestBenchmarkRecoveryDoesNotReinitializeOwnedVG(t *testing.T) {
	for _, scenario := range []string{"persistence failure", "cleanup pending"} {
		t.Run(scenario, func(t *testing.T) {
			r, client, backend := fixture(t, makePool())
			backend.benchmarkResult = func(req model.BenchmarkRequest) (storagev1.DiskPoolBenchmark, error) {
				if scenario == "persistence failure" {
					return storagev1.DiskPoolBenchmark{}, errors.New("journal fsync failed")
				}
				result := successfulBenchmark(req)
				result.Phase, result.CleanupVerified, result.ErrorCode = "Failed", false, "CleanupPending"
				result.ResultDigest = poolstate.Digest(result)
				return result, nil
			}
			reconcile(t, r)
			if phase(t, client, Pools, "ssd-01") != "BenchmarkFailed" {
				t.Fatal("failure not recorded")
			}
			assertNoClass(t, client, "ssd-01")
			backend.benchmarkResult = func(req model.BenchmarkRequest) (storagev1.DiskPoolBenchmark, error) {
				result := successfulBenchmark(req)
				if scenario == "cleanup pending" {
					result.Phase, result.ErrorCode = "Failed", "FioFailed"
					result.ResultDigest = poolstate.Digest(result)
				}
				return result, nil
			}
			reconcile(t, r)
			if len(backend.calls) != 1 || len(backend.benchmarkCalls) != 2 {
				t.Fatal("cleanup recovery reinitialized the VG")
			}
			if !reflect.DeepEqual(backend.benchmarkCalls[0], backend.benchmarkCalls[1]) {
				t.Fatal("recovery changed task identity")
			}
			assertNoClass(t, client, "ssd-01")
			want := "AwaitingReview"
			if scenario == "cleanup pending" {
				want = "BenchmarkFailed"
			}
			if phase(t, client, Pools, "ssd-01") != want {
				t.Fatal("cleanup recovery erased failure or bypassed review")
			}
		})
	}
}

func TestApprovalMustBindCurrentBenchmark(t *testing.T) {
	for _, field := range []string{"poolUID", "runID", "resultDigest", "reviewedAt"} {
		t.Run(field, func(t *testing.T) {
			r, client, backend := fixture(t, makePool())
			reconcile(t, r)
			reviewPool(t, client, "ssd-01", "Approved", "")
			obj := get(t, client, Pools, "ssd-01")
			value := "stale-identity"
			if field == "reviewedAt" {
				value = "2026-11-15T00:00:00Z"
			}
			if err := unstructured.SetNestedField(obj.Object, value, "spec", "allocationApproval", field); err != nil {
				t.Fatal(err)
			}
			if _, err := client.Resource(Pools).Update(context.Background(), obj, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			reconcile(t, r)
			assertNoClass(t, client, "ssd-01")
			if len(backend.calls) != 1 || phase(t, client, Pools, "ssd-01") != "AwaitingReview" {
				t.Fatal("stale approval authorized allocation")
			}
		})
	}
}

func TestRejectedBenchmarkCanBeExplicitlyApproved(t *testing.T) {
	r, client, backend := fixture(t, makePool())
	reconcile(t, r)
	reviewPool(t, client, "ssd-01", "Rejected", "latency requires investigation")
	reconcile(t, r)
	assertNoClass(t, client, "ssd-01")
	if phase(t, client, Pools, "ssd-01") != "Rejected" {
		t.Fatal("rejection not reflected")
	}
	reviewPool(t, client, "ssd-01", "Approved", "result reviewed after investigation")
	reconcile(t, r)
	if phase(t, client, Pools, "ssd-01") != "Ready" || len(backend.benchmarkCalls) != 1 {
		t.Fatal("explicit approval did not publish the existing result")
	}
}

func TestLegacyReadyPoolRetainsClassWithoutBenchmarking(t *testing.T) {
	obj := makePool()
	obj.Object["status"] = map[string]interface{}{"phase": "Ready", "storageClass": "diskfleet-ssd-01", "topologyHostname": "actual-host"}
	p, err := decodePool(obj)
	if err != nil {
		t.Fatal(err)
	}
	sc := storageClass(pool{obj, p.Spec}, "actual-host")
	r, client, backend := fixture(t, obj, sc)
	for i := 0; i < 2; i++ {
		reconcile(t, r)
		if phase(t, client, Pools, "ssd-01") != "LegacyUnreviewed" || len(backend.calls) != 0 || len(backend.benchmarkCalls) != 0 {
			t.Fatal("legacy data was automatically benchmarked or approved")
		}
		if actual := get(t, client, Classes, sc.GetName()); !reflect.DeepEqual(actual.Object, sc.Object) {
			t.Fatal("legacy StorageClass changed")
		}
	}
}

func TestExistingBroadClassesBlockBeforeDiskInitialization(t *testing.T) {
	for _, selector := range []map[string]interface{}{
		{"vgpattern": ".*"}, {"vgpattern": "^df_"}, {"vgpattern": "[invalid"}, {"volgroup": "df_ssd01"}, {},
	} {
		t.Run(strings.ReplaceAll(strings.TrimSpace(strings.Join([]string{stringValue(selector["vgpattern"]), stringValue(selector["volgroup"])}, " ")), "/", "_"), func(t *testing.T) {
			selector["storage"] = "lvm"
			sc := &unstructured.Unstructured{Object: map[string]interface{}{
				"apiVersion": "storage.k8s.io/v1", "kind": "StorageClass", "metadata": map[string]interface{}{"name": "preexisting"},
				"provisioner": "local.csi.openebs.io", "parameters": selector,
			}}
			r, client, backend := fixture(t, makePool(), sc)
			reconcile(t, r)
			if len(backend.calls) != 0 || len(backend.benchmarkCalls) != 0 || phase(t, client, Pools, "ssd-01") != "Error" {
				t.Fatal("published VG selector reached initialization")
			}
			assertNoClass(t, client, "ssd-01")
		})
	}
}

func stringValue(value interface{}) string { valueString, _ := value.(string); return valueString }

func TestBenchmarkGuardsRejectIdentityOrAuthorizationChange(t *testing.T) {
	for _, scenario := range []string{"node UID", "pool UID", "initialization approval", "device identity", "new wildcard class"} {
		t.Run(scenario, func(t *testing.T) {
			r, client, backend := fixture(t, makePool())
			backend.duringBenchmark = func(ctx context.Context) {
				if scenario == "new wildcard class" {
					_, err := client.Resource(Classes).Create(ctx, &unstructured.Unstructured{Object: map[string]interface{}{
						"apiVersion": "storage.k8s.io/v1", "kind": "StorageClass", "metadata": map[string]interface{}{"name": "concurrent-selector"},
						"provisioner": "local.csi.openebs.io", "parameters": map[string]interface{}{"storage": "lvm", "vgpattern": ".*"},
					}}, metav1.CreateOptions{})
					if err != nil {
						t.Error(err)
					}
					return
				}
				resource, name := Pools, "ssd-01"
				if scenario == "node UID" {
					resource, name = Nodes, r.NodeName
				}
				obj := get(t, client, resource, name)
				switch scenario {
				case "node UID", "pool UID":
					obj.SetUID("replacement")
				case "initialization approval":
					_ = unstructured.SetNestedField(obj.Object, false, "spec", "approved")
				case "device identity":
					obj.Object["spec"].(map[string]interface{})["devices"].([]interface{})[0].(map[string]interface{})["serial"] = "REPLACED"
				}
				if _, err := client.Resource(resource).Update(ctx, obj, metav1.UpdateOptions{}); err != nil {
					t.Error(err)
				}
			}
			reconcile(t, r)
			assertNoClass(t, client, "ssd-01")
			if backend.guardError == nil || len(backend.benchmarkCalls) != 1 {
				t.Fatal("benchmark guard accepted changed identity/authorization")
			}
		})
	}
}

func TestChangedResultDuringAllocationVerificationCannotPublish(t *testing.T) {
	r, client, backend := fixture(t, makePool())
	reconcile(t, r)
	reviewPool(t, client, "ssd-01", "Approved", "")
	backend.after = func() {
		obj := get(t, client, Pools, "ssd-01")
		p, err := decodePool(obj)
		if err != nil {
			t.Fatal(err)
		}
		p.Status.Benchmark.Workloads[0].IOPS++
		p.Status.Benchmark.ResultDigest = poolstate.Digest(*p.Status.Benchmark)
		if err := setBenchmark(obj.Object["status"].(map[string]interface{}), p.Status.Benchmark); err != nil {
			t.Fatal(err)
		}
		if _, err := client.Resource(Pools).Update(context.Background(), obj, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	reconcile(t, r)
	assertNoClass(t, client, "ssd-01")
	if phase(t, client, Pools, "ssd-01") != "Error" {
		t.Fatal("post-IO approval was not revalidated")
	}
}

func TestDiscoveryRefreshContinuesDuringBlockedBenchmark(t *testing.T) {
	r, client, backend := fixture(t, makePool())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	backend.duringBenchmark = func(ctx context.Context) { close(started); <-ctx.Done() }
	poolDone := make(chan error, 1)
	go func() { poolDone <- r.reconcile(ctx, false) }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("benchmark did not start")
	}
	var scans atomic.Int64
	base := r.Now()
	r.Now = func() time.Time { return base.Add(time.Duration(scans.Load()) * time.Minute) }
	disk := observedDisk()
	r.Discover = func(context.Context) ([]model.Disk, error) { scans.Add(1); return []model.Disk{disk}, nil }
	discoveryDone := make(chan struct{})
	go func() { defer close(discoveryDone); r.runDiscovery(ctx, 15*time.Millisecond) }()
	defer func() {
		cancel()
		select {
		case <-poolDone:
		case <-time.After(3 * time.Second):
			t.Error("pool worker failed to stop")
		}
		select {
		case <-discoveryDone:
		case <-time.After(3 * time.Second):
			t.Error("discovery worker failed to stop")
		}
	}()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		obj, err := client.Resource(Disks).Get(ctx, DiskName(r.NodeName, disk), metav1.GetOptions{})
		if err == nil {
			observed, _, _ := unstructured.NestedString(obj.Object, "status", "lastObservedTime")
			observedAt, parseErr := time.Parse(time.RFC3339, observed)
			if parseErr == nil && !observedAt.Before(base.Add(2*time.Minute)) {
				select {
				case err := <-poolDone:
					t.Fatalf("benchmark returned before cancellation: %v", err)
				default:
				}
				if phase(t, client, Disks, obj.GetName()) != "Managed" {
					t.Fatal("in-progress pool disk was not protected")
				}
				assertNoClass(t, client, "ssd-01")
				return
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("blocked benchmark prevented inventory heartbeat refresh")
}
