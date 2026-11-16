package poolstate

import (
	"math"
	"strings"
	"testing"
	"time"

	storagev1 "github.com/SongOf/pulsar-operator/api/storage/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func benchmarkFixture() *storagev1.DiskPool {
	device := storagev1.Device{Path: "/dev/disk/by-id/wwn-fixture", Serial: "FIXTURE-SERIAL", SizeBytes: 100 << 30}
	start := metav1.NewTime(time.Date(2026, time.November, 16, 8, 0, 0, 0, time.UTC))
	end := metav1.NewTime(start.Add(2 * time.Minute))
	pool := &storagev1.DiskPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-fixture", UID: types.UID("pool-uid"), ResourceVersion: "17"},
		Spec:       storagev1.DiskPoolSpec{NodeName: "worker-one", VGName: "df_fixture", Approved: true, Devices: []storagev1.Device{device}},
		Status: storagev1.DiskPoolStatus{Phase: "AwaitingReview", Benchmark: &storagev1.DiskPoolBenchmark{
			PoolUID: "pool-uid", NodeUID: "node-uid", VGName: "df_fixture", Device: device,
			RunID: RunID("pool-uid"), Profile: Profile, Phase: "Succeeded", StartedAt: &start, CompletedAt: &end,
			TestSizeBytes: TestSizeBytes, CleanupVerified: true,
		}},
	}
	for _, workload := range []struct {
		name  string
		block int64
	}{{"seq-write", 1 << 20}, {"seq-read", 1 << 20}, {"rand-write", 4096}, {"rand-read", 4096}} {
		pool.Status.Benchmark.Workloads = append(pool.Status.Benchmark.Workloads, storagev1.DiskBenchmarkWorkload{
			Name: workload.name, BlockSizeBytes: workload.block, QueueDepth: QueueDepth,
			DurationSeconds: DurationSeconds, Bytes: 1 << 30, BandwidthBytesPerSecond: 35 << 20,
			IOPS: 1000, LatencyMeanNS: 100000, LatencyP95NS: 200000, LatencyP99NS: 300000,
		})
	}
	pool.Status.Benchmark.ResultDigest = Digest(*pool.Status.Benchmark)
	return pool
}

func approveFixture(pool *storagev1.DiskPool) {
	b := pool.Status.Benchmark
	pool.Spec.AllocationApproval = &storagev1.DiskPoolAllocationApproval{
		PoolUID: b.PoolUID, RunID: b.RunID, ResultDigest: b.ResultDigest,
		Decision: "Approved", ReviewedAt: metav1.NewTime(b.CompletedAt.Add(time.Second)),
	}
}

func TestBenchmarkApprovalAndPublicationAreSeparateRequirements(t *testing.T) {
	pool := benchmarkFixture()
	if err := Reviewable(pool, "node-uid"); err != nil {
		t.Fatalf("complete benchmark not reviewable: %v", err)
	}
	if Approved(pool, "node-uid") == nil || Allocatable(pool, "node-uid") == nil {
		t.Fatal("unreviewed benchmark authorized allocation")
	}
	// An old Ready marker or StorageClass does not substitute for approval.
	pool.Status.Phase = "Ready"
	pool.Status.StorageClass, pool.Status.TopologyHostname = "diskfleet-pool-fixture", "host-one"
	if Allocatable(pool, "node-uid") == nil {
		t.Fatal("Ready alone authorized allocation")
	}
	pool.Status.Phase, pool.Status.StorageClass, pool.Status.TopologyHostname = "AwaitingReview", "", ""
	approveFixture(pool)
	if err := Approved(pool, "node-uid"); err != nil {
		t.Fatalf("valid approval rejected: %v", err)
	}
	if Allocatable(pool, "node-uid") == nil {
		t.Fatal("approval bypassed Agent StorageClass publication")
	}
	pool.Status.Phase = "Ready"
	pool.Status.StorageClass, pool.Status.TopologyHostname = "diskfleet-pool-fixture", "host-one"
	if err := Allocatable(pool, "node-uid"); err != nil {
		t.Fatalf("approved and published pool not allocatable: %v", err)
	}
}

func TestReviewableRejectsUnsafeReportsEvenWithRecomputedDigest(t *testing.T) {
	cases := map[string]func(*storagev1.DiskPool){
		"missing benchmark":     func(p *storagev1.DiskPool) { p.Status.Benchmark = nil },
		"pool UID missing":      func(p *storagev1.DiskPool) { p.UID = "" },
		"deleting pool":         func(p *storagev1.DiskPool) { now := metav1.Now(); p.DeletionTimestamp = &now },
		"initialization denied": func(p *storagev1.DiskPool) { p.Spec.Approved = false },
		"missing device":        func(p *storagev1.DiskPool) { p.Spec.Devices = nil },
		"multiple devices":      func(p *storagev1.DiskPool) { p.Spec.Devices = append(p.Spec.Devices, p.Spec.Devices[0]) },
		"recreated pool":        func(p *storagev1.DiskPool) { p.UID = "new-pool-uid" },
		"forged node UID":       func(p *storagev1.DiskPool) { p.Status.Benchmark.NodeUID = "other-node-uid" },
		"forged VG":             func(p *storagev1.DiskPool) { p.Status.Benchmark.VGName = "df_other" },
		"different device path": func(p *storagev1.DiskPool) { p.Status.Benchmark.Device.Path += "-other" },
		"different serial":      func(p *storagev1.DiskPool) { p.Status.Benchmark.Device.Serial += "-other" },
		"different capacity":    func(p *storagev1.DiskPool) { p.Status.Benchmark.Device.SizeBytes++ },
		"different run":         func(p *storagev1.DiskPool) { p.Status.Benchmark.RunID = RunID("different-pool") },
		"unknown profile":       func(p *storagev1.DiskPool) { p.Status.Benchmark.Profile = "other" },
		"wrong test size":       func(p *storagev1.DiskPool) { p.Status.Benchmark.TestSizeBytes++ },
		"not cleaned":           func(p *storagev1.DiskPool) { p.Status.Benchmark.CleanupVerified = false },
		"execution error":       func(p *storagev1.DiskPool) { p.Status.Benchmark.ErrorCode = "fio-failed" },
		"missing start":         func(p *storagev1.DiskPool) { p.Status.Benchmark.StartedAt = nil },
		"missing completion":    func(p *storagev1.DiskPool) { p.Status.Benchmark.CompletedAt = nil },
		"zero completion":       func(p *storagev1.DiskPool) { p.Status.Benchmark.CompletedAt = &metav1.Time{} },
		"reversed time": func(p *storagev1.DiskPool) {
			end := metav1.NewTime(p.Status.Benchmark.StartedAt.Add(-time.Second))
			p.Status.Benchmark.CompletedAt = &end
		},
		"partial workloads":    func(p *storagev1.DiskPool) { p.Status.Benchmark.Workloads = p.Status.Benchmark.Workloads[:3] },
		"duplicate workload":   func(p *storagev1.DiskPool) { p.Status.Benchmark.Workloads[3] = p.Status.Benchmark.Workloads[0] },
		"unknown workload":     func(p *storagev1.DiskPool) { p.Status.Benchmark.Workloads[0].Name = "unknown" },
		"wrong block size":     func(p *storagev1.DiskPool) { p.Status.Benchmark.Workloads[0].BlockSizeBytes = 4096 },
		"wrong queue depth":    func(p *storagev1.DiskPool) { p.Status.Benchmark.Workloads[0].QueueDepth = 1 },
		"truncated workload":   func(p *storagev1.DiskPool) { p.Status.Benchmark.Workloads[0].DurationSeconds = DurationSeconds - 1 },
		"no IO completed":      func(p *storagev1.DiskPool) { p.Status.Benchmark.Workloads[0].Bytes = 0 },
		"missing bandwidth":    func(p *storagev1.DiskPool) { p.Status.Benchmark.Workloads[0].BandwidthBytesPerSecond = 0 },
		"missing IOPS":         func(p *storagev1.DiskPool) { p.Status.Benchmark.Workloads[0].IOPS = 0 },
		"missing mean latency": func(p *storagev1.DiskPool) { p.Status.Benchmark.Workloads[0].LatencyMeanNS = 0 },
		"missing P95":          func(p *storagev1.DiskPool) { p.Status.Benchmark.Workloads[0].LatencyP95NS = 0 },
		"missing P99":          func(p *storagev1.DiskPool) { p.Status.Benchmark.Workloads[0].LatencyP99NS = 0 },
		"invalid percentile order": func(p *storagev1.DiskPool) {
			p.Status.Benchmark.Workloads[0].LatencyP95NS = p.Status.Benchmark.Workloads[0].LatencyP99NS + 1
		},
		"IO error count":   func(p *storagev1.DiskPool) { p.Status.Benchmark.Workloads[0].Errors = 1 },
		"NaN IOPS":         func(p *storagev1.DiskPool) { p.Status.Benchmark.Workloads[0].IOPS = math.NaN() },
		"infinite latency": func(p *storagev1.DiskPool) { p.Status.Benchmark.Workloads[0].LatencyMeanNS = math.Inf(1) },
	}
	for _, phase := range []string{"Running", "Failed", "Interrupted", "Blocked"} {
		cases["phase "+phase] = func(p *storagev1.DiskPool) { p.Status.Benchmark.Phase = phase }
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			pool := benchmarkFixture()
			change(pool)
			if pool.Status.Benchmark != nil {
				pool.Status.Benchmark.ResultDigest = Digest(*pool.Status.Benchmark)
			}
			if err := Reviewable(pool, "node-uid"); err == nil {
				t.Fatal("unsafe report was accepted")
			}
		})
	}
	for _, nodeUID := range []string{"", "new-node-uid"} {
		if Reviewable(benchmarkFixture(), nodeUID) == nil {
			t.Fatal("report accepted without matching live Node UID")
		}
	}
	if Reviewable(nil, "node-uid") == nil || Approved(nil, "node-uid") == nil || Allocatable(nil, "node-uid") == nil {
		t.Fatal("missing pool accepted")
	}
}

func TestDigestDetectsTamperingAndDoesNotHashItsOwnField(t *testing.T) {
	pool := benchmarkFixture()
	b := pool.Status.Benchmark
	original := b.ResultDigest
	b.ResultDigest = strings.Repeat("0", 64)
	if Digest(*b) != original {
		t.Fatal("digest depends on its previous value")
	}
	if Reviewable(pool, "node-uid") == nil {
		t.Fatal("forged digest accepted")
	}
	b.ResultDigest = original
	b.Workloads[0].BandwidthBytesPerSecond++
	if Reviewable(pool, "node-uid") == nil {
		t.Fatal("modified measurement accepted without new digest")
	}
	if changed := Digest(*b); changed == "" || changed == original {
		t.Fatal("measurement change did not change result identity")
	}
}

func TestApprovalCannotBeReplayedAgainstAnotherResult(t *testing.T) {
	cases := map[string]func(*storagev1.DiskPool){
		"rejected":     func(p *storagev1.DiskPool) { p.Spec.AllocationApproval.Decision = "Rejected" },
		"old pool":     func(p *storagev1.DiskPool) { p.Spec.AllocationApproval.PoolUID = "old-pool-uid" },
		"old run":      func(p *storagev1.DiskPool) { p.Spec.AllocationApproval.RunID = RunID("old-pool") },
		"old digest":   func(p *storagev1.DiskPool) { p.Spec.AllocationApproval.ResultDigest = strings.Repeat("0", 64) },
		"missing time": func(p *storagev1.DiskPool) { p.Spec.AllocationApproval.ReviewedAt = metav1.Time{} },
		"review before completion": func(p *storagev1.DiskPool) {
			p.Spec.AllocationApproval.ReviewedAt = metav1.NewTime(p.Status.Benchmark.CompletedAt.Add(-time.Second))
		},
		"changed measured result": func(p *storagev1.DiskPool) {
			p.Status.Benchmark.Workloads[0].IOPS++
			p.Status.Benchmark.ResultDigest = Digest(*p.Status.Benchmark)
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			pool := benchmarkFixture()
			approveFixture(pool)
			change(pool)
			if err := Reviewable(pool, "node-uid"); err != nil {
				t.Fatalf("fixture no longer isolates approval: %v", err)
			}
			if Approved(pool, "node-uid") == nil || Allocatable(pool, "node-uid") == nil {
				t.Fatal("stale or rejected approval authorized allocation")
			}
		})
	}
}
