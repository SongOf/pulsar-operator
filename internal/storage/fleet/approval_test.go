package fleet

import (
	"testing"
	"time"

	storagev1 "github.com/SongOf/pulsar-operator/api/storage/v1alpha1"
	"github.com/SongOf/pulsar-operator/internal/storage/model"
	"github.com/SongOf/pulsar-operator/internal/storage/poolstate"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Add explicit synthetic review evidence to historical planner inputs. The
// captured output manifests remain unchanged; the new admission is mandatory.
func approveFixture(p model.Object) {
	uid, node := text(p, "uid"), text(p, "node")
	p["initializationApproved"] = true
	p["nodeUID"] = "node-uid-" + node
	p["storageClass"] = "diskfleet-" + text(p, "id")
	device := storagev1.Device{Path: "/dev/disk/by-id/fixture-" + text(p, "id"), Serial: "fixture-" + text(p, "id"), SizeBytes: model.Int64(p["capacityBytes"])}
	p["devices"] = []storagev1.Device{device}
	end := metav1.NewTime(time.Unix(1700000000, 0))
	start := metav1.NewTime(end.Add(-2 * time.Minute))
	b := &storagev1.DiskPoolBenchmark{PoolUID: uid, NodeUID: text(p, "nodeUID"), VGName: text(p, "vgName"), Device: device, RunID: poolstate.RunID(uid), Profile: poolstate.Profile, Phase: "Succeeded", StartedAt: &start, CompletedAt: &end, TestSizeBytes: poolstate.TestSizeBytes, CleanupVerified: true}
	for _, w := range []struct {
		name  string
		block int64
	}{{"seq-write", 1 << 20}, {"seq-read", 1 << 20}, {"rand-write", 4096}, {"rand-read", 4096}} {
		b.Workloads = append(b.Workloads, storagev1.DiskBenchmarkWorkload{Name: w.name, BlockSizeBytes: w.block, QueueDepth: poolstate.QueueDepth, DurationSeconds: 30, Bytes: 1 << 30, BandwidthBytesPerSecond: 1 << 25, IOPS: 1000, LatencyMeanNS: 100, LatencyP95NS: 200, LatencyP99NS: 300})
	}
	b.ResultDigest = poolstate.Digest(*b)
	p["benchmark"] = b
	p["allocationApproval"] = &storagev1.DiskPoolAllocationApproval{PoolUID: uid, RunID: b.RunID, ResultDigest: b.ResultDigest, Decision: "Approved", ReviewedAt: end}
}
func TestPlannerRejectsMissingOrForgedApproval(t *testing.T) {
	for _, mutation := range []string{"missing", "wrong-digest", "rejected", "backend-denied", "initialization-revoked", "deleting"} {
		t.Run(mutation, func(t *testing.T) {
			f := loadFixture(t, "three-node-e2e")
			p := f.Pools[0]
			p["canCreateVolume"] = true
			switch mutation {
			case "backend-denied":
				p["canCreateVolume"] = false
			case "initialization-revoked":
				p["initializationApproved"] = false
			case "deleting":
				p["deletionTimestamp"] = "2023-11-14T22:13:20Z"
			case "missing":
				delete(p, "benchmark")
			case "wrong-digest":
				p["allocationApproval"].(*storagev1.DiskPoolAllocationApproval).ResultDigest = "forged"
			case "rejected":
				p["allocationApproval"].(*storagev1.DiskPoolAllocationApproval).Decision = "Rejected"
			}
			if _, err := PulsarPlan(f.Pools, f.Assignments, "diskfleet-pulsar-test", "pulsar", 3, "three-node-e2e"); err == nil {
				t.Fatal("unapproved pool accepted")
			}
		})
	}
}
