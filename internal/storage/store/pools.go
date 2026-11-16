package store

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"time"

	storagev1 "github.com/SongOf/pulsar-operator/api/storage/v1alpha1"
	"github.com/SongOf/pulsar-operator/internal/storage/model"
	"github.com/SongOf/pulsar-operator/internal/storage/poolstate"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func demoPool(pool model.Object) error {
	typed, err := model.PoolIdentity(pool)
	if err != nil {
		return err
	}
	model.DescribeAllocation(pool, typed, model.String(pool["nodeUID"]), model.String(pool["storageClass"]) != "")
	pool["mode"] = "demo"
	return nil
}
func simulatedBenchmark(pool model.Object, device storagev1.Device) *storagev1.DiskPoolBenchmark {
	end := metav1.NewTime(time.Now().UTC().Truncate(time.Second))
	start := metav1.NewTime(end.Add(-2 * time.Minute))
	b := &storagev1.DiskPoolBenchmark{PoolUID: model.String(pool["uid"]), NodeUID: model.String(pool["nodeUID"]), VGName: model.String(pool["vgName"]), Device: device, RunID: poolstate.RunID(model.String(pool["uid"])), Profile: poolstate.Profile, Phase: "Succeeded", StartedAt: &start, CompletedAt: &end, TestSizeBytes: poolstate.TestSizeBytes, CleanupVerified: true, Message: "模拟压测数据；未运行 fio，也未操作主机设备"}
	for _, w := range []struct {
		name  string
		block int64
	}{{"seq-write", 1 << 20}, {"seq-read", 1 << 20}, {"rand-write", 4096}, {"rand-read", 4096}} {
		b.Workloads = append(b.Workloads, storagev1.DiskBenchmarkWorkload{Name: w.name, BlockSizeBytes: w.block, QueueDepth: poolstate.QueueDepth, DurationSeconds: poolstate.DurationSeconds, Bytes: 1 << 30, BandwidthBytesPerSecond: 35 * 1024 * 1024, IOPS: 1000, LatencyMeanNS: 100000, LatencyP95NS: 200000, LatencyP99NS: 300000})
	}
	b.ResultDigest = poolstate.Digest(*b)
	return b
}
func (s *Store) Pool(ctx context.Context, id string) (model.Object, error) {
	if _, err := model.Identifier(id); err != nil {
		return nil, err
	}
	var pool model.Object
	err := s.Transaction(ctx, func(tx *sql.Tx) error {
		var err error
		pool, err = s.getRequired(ctx, tx, "pools", id)
		if err != nil {
			return err
		}
		return demoPool(pool)
	})
	return pool, err
}
func (s *Store) ReviewPool(ctx context.Context, id string, body model.Object) (model.Object, error) {
	if _, err := model.Identifier(id); err != nil {
		return nil, err
	}
	review, err := model.ParsePoolReview(body)
	if err != nil {
		return nil, err
	}
	// Commit the request before the reviewed decision, mirroring the remote API path.
	current, err := s.Pool(ctx, id)
	if err != nil {
		return nil, err
	}
	typed, err := model.PoolIdentity(current)
	if err != nil {
		return nil, err
	}
	if err = review.Matches(typed); err != nil {
		return nil, err
	}
	if current["canReview"] != true {
		return nil, model.NewProblem(409, "模拟压测结果尚不可审核")
	}
	if err = s.Audit(ctx, "pool.review-requested", "请求审核模拟存储池 "+id+"，决定 "+review.Decision, model.String(current["node"]), "info"); err != nil {
		return nil, err
	}
	var pool model.Object
	err = s.Transaction(ctx, func(tx *sql.Tx) error {
		var err error
		pool, err = s.getRequired(ctx, tx, "pools", id)
		if err != nil {
			return err
		}
		if err = demoPool(pool); err != nil {
			return err
		}
		typed, err := model.PoolIdentity(pool)
		if err != nil {
			return err
		}
		if err = review.Matches(typed); err != nil {
			return err
		}
		if pool["canReview"] != true {
			return model.NewProblem(409, "模拟压测结果已变化")
		}
		version, err := strconv.ParseUint(typed.ResourceVersion, 10, 64)
		if err != nil {
			return err
		}
		pool["resourceVersion"] = strconv.FormatUint(version+1, 10)
		pool["allocationApproval"] = review.Approval(metav1.NewTime(time.Now().UTC()))
		if review.Decision == "Approved" {
			pool["state"], pool["storageClass"] = "Ready", "diskfleet-"+id
		} else {
			pool["state"] = "AwaitingReview"
		}
		if err = demoPool(pool); err != nil {
			return err
		}
		if err = s.Put(ctx, tx, "pools", pool); err != nil {
			return err
		}
		return s.Event(ctx, tx, "pool.reviewed", "模拟存储池 "+id+" 审核结果 "+review.Decision, model.String(pool["node"]), "info")
	})
	if err != nil {
		auditCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if auditErr := s.Audit(auditCtx, "pool.review-failed", "模拟存储池审核失败，请刷新后重试", model.String(current["node"]), "warning"); auditErr != nil {
			return nil, errors.Join(err, auditErr)
		}
	}
	return pool, err
}
