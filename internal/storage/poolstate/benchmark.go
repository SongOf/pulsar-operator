// Package poolstate validates the shared, immutable benchmark and allocation
// approval contract. It performs no Kubernetes, filesystem, or device operations.
package poolstate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"

	storagev1 "github.com/SongOf/pulsar-operator/api/storage/v1alpha1"
)

const (
	Profile               = "temporary-lv-v1"
	TestSizeBytes   int64 = 1 << 30
	DurationSeconds       = 30
	QueueDepth      int32 = 16
)

// RunID is stable across restarts. Retrying a reconcile must not rerun a test.
func RunID(poolUID string) string {
	sum := sha256.Sum256([]byte(Profile + "\x00" + poolUID))
	return hex.EncodeToString(sum[:16])
}

// Digest includes identity, measured results, times, and verified cleanup, but
// excludes mutable Kubernetes metadata and the approval itself.
func Digest(result storagev1.DiskPoolBenchmark) string {
	result.ResultDigest = ""
	data, err := json.Marshal(result)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Reviewable rejects missing, partial, mismatched, or unsafe benchmark results.
// nodeUID must come from the current live Node, not from the benchmark itself.
func Reviewable(pool *storagev1.DiskPool, nodeUID string) error {
	if pool == nil || pool.UID == "" || pool.DeletionTimestamp != nil || !pool.Spec.Approved || len(pool.Spec.Devices) != 1 || nodeUID == "" {
		return errors.New("pool and live node identity are required")
	}
	b := pool.Status.Benchmark
	if b == nil {
		return errors.New("single-disk benchmark has not been recorded")
	}
	if b.PoolUID != string(pool.UID) || b.NodeUID != nodeUID || b.VGName != pool.Spec.VGName || b.Device != pool.Spec.Devices[0] || b.RunID != RunID(string(pool.UID)) {
		return errors.New("benchmark identity does not match the current pool, node, or device")
	}
	if b.Profile != Profile || b.TestSizeBytes != TestSizeBytes || b.Phase != "Succeeded" || !b.CleanupVerified || b.ErrorCode != "" {
		return errors.New("benchmark must succeed with verified temporary LV cleanup")
	}
	if b.StartedAt == nil || b.CompletedAt == nil || b.StartedAt.IsZero() || b.CompletedAt.IsZero() || b.CompletedAt.Before(b.StartedAt) {
		return errors.New("benchmark timestamps are incomplete")
	}
	expected := map[string]int64{"seq-write": 1 << 20, "seq-read": 1 << 20, "rand-write": 4096, "rand-read": 4096}
	if len(b.Workloads) != len(expected) {
		return errors.New("benchmark requires all four workload results")
	}
	for _, w := range b.Workloads {
		block, exists := expected[w.Name]
		if !exists || w.BlockSizeBytes != block || w.QueueDepth != QueueDepth || w.Errors != 0 || w.Bytes <= 0 || w.BandwidthBytesPerSecond <= 0 || !positive(w.DurationSeconds) || w.DurationSeconds < DurationSeconds || !positive(w.IOPS) || !positive(w.LatencyMeanNS) || !positive(w.LatencyP95NS) || !positive(w.LatencyP99NS) || w.LatencyP95NS > w.LatencyP99NS {
			return errors.New("benchmark contains incomplete or invalid workload metrics")
		}
		delete(expected, w.Name)
	}
	if b.ResultDigest == "" || b.ResultDigest != Digest(*b) {
		return errors.New("benchmark result digest does not match")
	}
	return nil
}

// Approved binds the human decision to exactly the result being allocated.
func Approved(pool *storagev1.DiskPool, nodeUID string) error {
	if err := Reviewable(pool, nodeUID); err != nil {
		return err
	}
	a, b := pool.Spec.AllocationApproval, pool.Status.Benchmark
	if a == nil || a.Decision != "Approved" || a.PoolUID != b.PoolUID || a.RunID != b.RunID || a.ResultDigest != b.ResultDigest || a.ReviewedAt.IsZero() || a.ReviewedAt.Before(b.CompletedAt) {
		return errors.New("human allocation approval is missing or does not match the benchmark")
	}
	return nil
}

func Allocatable(pool *storagev1.DiskPool, nodeUID string) error {
	if err := Approved(pool, nodeUID); err != nil {
		return err
	}
	if pool.Status.Phase != "Ready" || pool.Status.StorageClass == "" || pool.Status.TopologyHostname == "" {
		return errors.New("approved pool is waiting for Agent to publish its StorageClass")
	}
	return nil
}

func positive(value float64) bool { return value > 0 && !math.IsInf(value, 0) && !math.IsNaN(value) }
