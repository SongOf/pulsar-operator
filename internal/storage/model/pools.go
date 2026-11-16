package model

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	storagev1 "github.com/SongOf/pulsar-operator/api/storage/v1alpha1"
	"github.com/SongOf/pulsar-operator/internal/storage/poolstate"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// PoolIdentity reconstructs the immutable identity carried by an API snapshot.
// Offline snapshots remain unverified input; they must carry the full contract.
func PoolIdentity(value Object) (*storagev1.DiskPool, error) {
	raw := Object{"metadata": Object{"name": value["id"], "uid": value["uid"], "resourceVersion": value["resourceVersion"], "deletionTimestamp": value["deletionTimestamp"]}, "spec": Object{"nodeName": value["node"], "vgName": value["vgName"], "approved": value["initializationApproved"], "devices": value["devices"], "allocationApproval": value["allocationApproval"]}, "status": Object{"phase": value["state"], "storageClass": value["storageClass"], "topologyHostname": value["topologyHostname"], "benchmark": value["benchmark"]}}
	data, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	var pool storagev1.DiskPool
	err = json.Unmarshal(data, &pool)
	return &pool, err
}

func RequireAllocation(value Object) error {
	if allowed, exists := value["canCreateVolume"]; exists && allowed != true {
		return NewProblem(409, "当前存储池不可新分配容量")
	}
	pool, err := PoolIdentity(value)
	if err != nil {
		return NewProblem(409, "存储池审核数据不完整")
	}
	if err := poolstate.Allocatable(pool, String(value["nodeUID"])); err != nil || pool.Status.Benchmark.CompletedAt.After(time.Now()) {
		return NewProblem(409, "存储池尚未完成有效压测、人工审核或 StorageClass 发布")
	}
	return nil
}

// DescribeAllocation keeps observation of existing volumes independent from new allocation.
func DescribeAllocation(value Object, pool *storagev1.DiskPool, nodeUID string, classExists bool) {
	value["uid"], value["resourceVersion"], value["nodeUID"], value["devices"] = string(pool.UID), pool.ResourceVersion, nodeUID, jsonValue(pool.Spec.Devices)
	value["initializationApproved"], value["deletionTimestamp"] = pool.Spec.Approved, jsonValue(pool.DeletionTimestamp)
	value["benchmark"], value["allocationApproval"] = jsonValue(pool.Status.Benchmark), jsonValue(pool.Spec.AllocationApproval)
	state := "AwaitingReview"
	reasons := []string{}
	reviewErr := poolstate.Reviewable(pool, nodeUID)
	if reviewErr == nil && pool.Status.Benchmark.CompletedAt.After(time.Now()) {
		reviewErr = errors.New("benchmark completion is in the future")
	}
	approved := pool.Spec.AllocationApproval != nil && pool.Spec.AllocationApproval.Decision == "Approved"
	switch {
	case pool.Status.Benchmark == nil:
		state = "PendingBenchmark"
		if pool.Status.Phase == "Ready" || pool.Status.StorageClass != "" {
			state = "LegacyUnreviewed"
		}
		reasons = append(reasons, "尚无有效单盘压测结果，不能新分配容量")
	case pool.Status.Benchmark.Phase == "Running" || pool.Status.Benchmark.Phase == "Pending":
		state = "Benchmarking"
		reasons = append(reasons, "单盘压测尚未完成")
	case pool.Status.Benchmark.Phase != "Succeeded":
		state = "BenchmarkFailed"
		reasons = append(reasons, "单盘压测未成功，需检查执行和清理状态")
	case reviewErr != nil:
		state = "Blocked"
		reasons = append(reasons, "压测身份、指标、摘要或清理结果无效")
	case approved:
		if poolstate.Approved(pool, nodeUID) != nil {
			state = "Blocked"
			reasons = append(reasons, "人工审核与当前压测结果不匹配")
		} else if poolstate.Allocatable(pool, nodeUID) != nil {
			state = "AwaitingStorageClass"
			reasons = append(reasons, "审核已通过，等待 Agent 发布 StorageClass")
		} else {
			state = "Approved"
		}
	case pool.Spec.AllocationApproval != nil && pool.Spec.AllocationApproval.Decision == "Rejected":
		state = "Rejected"
		reasons = append(reasons, "人工审核已拒绝")
	default:
		reasons = append(reasons, "等待人工审核压测结果")
	}
	canReview := reviewErr == nil && !approved && !classExists && pool.Status.StorageClass == ""
	if classExists && !approved {
		canReview = false
		reasons = append(reasons, "已有 StorageClass，需人工处理旧池迁移，不能直接补签审核")
	}
	canAllocate := reviewErr == nil && poolstate.Allocatable(pool, nodeUID) == nil && classExists
	if !classExists && approved && canAllocate == false && len(reasons) == 0 {
		state = "AwaitingStorageClass"
		reasons = append(reasons, "审核已通过，StorageClass 尚不存在")
	}
	value["allocationState"], value["canReview"], value["canCreateVolume"], value["allocationReasons"] = state, canReview, canAllocate, reasons
}

type PoolReview struct{ UID, ResourceVersion, RunID, ResultDigest, Decision, Note string }

func ParsePoolReview(body Object) (PoolReview, error) {
	r := PoolReview{}
	for _, field := range []struct {
		key    string
		target *string
	}{{"uid", &r.UID}, {"resourceVersion", &r.ResourceVersion}, {"runID", &r.RunID}, {"resultDigest", &r.ResultDigest}} {
		value, err := Identifier(body[field.key])
		if err != nil {
			return r, err
		}
		*field.target = value
	}
	r.Decision = String(body["decision"])
	if r.Decision != "Approved" && r.Decision != "Rejected" {
		return r, NewProblem(400, "审核决定必须为 Approved 或 Rejected")
	}
	if value, exists := body["note"]; exists {
		var ok bool
		r.Note, ok = value.(string)
		if !ok {
			return r, NewProblem(400, "审核备注必须为字符串")
		}
	}
	if utf8.RuneCountInString(r.Note) > 1024 || r.Decision == "Rejected" && strings.TrimSpace(r.Note) == "" {
		return r, NewProblem(400, "拒绝时必须填写备注；备注不得超过 1024 字符")
	}
	return r, nil
}
func (r PoolReview) Matches(pool *storagev1.DiskPool) error {
	if pool.UID != types.UID(r.UID) || pool.ResourceVersion != r.ResourceVersion {
		return NewProblem(409, "存储池身份或版本已变化，请刷新后重新审核")
	}
	if pool.Spec.AllocationApproval != nil && pool.Spec.AllocationApproval.Decision == "Approved" {
		return NewProblem(409, "已批准的分配审核不可撤销或改写")
	}
	b := pool.Status.Benchmark
	if b == nil || b.RunID != r.RunID || b.ResultDigest != r.ResultDigest {
		return NewProblem(409, "压测结果已变化，请重新查看报告")
	}
	return nil
}
func (r PoolReview) Approval(now metav1.Time) *storagev1.DiskPoolAllocationApproval {
	return &storagev1.DiskPoolAllocationApproval{PoolUID: r.UID, RunID: r.RunID, ResultDigest: r.ResultDigest, Decision: r.Decision, ReviewedAt: now, Note: r.Note}
}

func jsonValue(value any) any {
	data, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	var result any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&result); err != nil {
		return nil
	}
	return result
}
