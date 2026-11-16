package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	storagev1 "github.com/SongOf/pulsar-operator/api/storage/v1alpha1"
	"github.com/SongOf/pulsar-operator/internal/storage/model"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func typedPool(raw object) (*storagev1.DiskPool, error) {
	data, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	pool := &storagev1.DiskPool{}
	err = json.Unmarshal(data, pool)
	return pool, err
}
func poolObject(raw object) object {
	meta, spec, status := asObject(raw["metadata"]), asObject(raw["spec"]), asObject(raw["status"])
	name := str(meta, "name")
	capacity, free := model.Int64(status["capacityBytes"]), model.Int64(status["availableBytes"])
	return object{"id": name, "uid": str(meta, "uid"), "resourceVersion": str(meta, "resourceVersion"), "name": fallback(at(meta, "annotations", label+"display-name"), name), "node": str(spec, "nodeName"), "vgName": str(spec, "vgName"), "capacityBytes": capacity, "allocatedBytes": max(int64(0), capacity-free), "availableBytes": free, "state": fallback(status["phase"], "Pending"), "diskIds": []any{}, "storageClass": str(status, "storageClass"), "topologyHostname": str(status, "topologyHostname"), "createdAt": str(meta, "creationTimestamp"), "message": str(status, "message")}
}
func (c *Cluster) describePool(ctx context.Context, raw object) (object, *storagev1.DiskPool, error) {
	pool, err := typedPool(raw)
	if err != nil {
		return nil, nil, err
	}
	node, err := c.get(ctx, nodesResource, pool.Spec.NodeName)
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, nil, err
	}
	actualClass, err := c.get(ctx, classesResource, className(pool.Name))
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, nil, err
	}
	classExists := err == nil
	result := poolObject(raw)
	nodeUID := str(node, "metadata", "uid")
	if str(node, "metadata", "deletionTimestamp") != "" {
		nodeUID = ""
	}
	model.DescribeAllocation(result, pool, nodeUID, classExists)
	if result["canCreateVolume"] == true && (pool.Status.StorageClass != className(pool.Name) || str(actualClass, "metadata", "annotations", label+"pool-uid") != string(pool.UID) || !classMatches(actualClass, storageClass(raw, pool.Status.TopologyHostname))) {
		result["canCreateVolume"] = false
		result["allocationState"] = "Blocked"
		result["allocationReasons"] = []string{"StorageClass 安全配置发生漂移"}
	}
	return result, pool, nil
}
func (c *Cluster) Pool(ctx context.Context, id string) (object, error) {
	if _, err := model.Identifier(id); err != nil {
		return nil, err
	}
	raw, err := c.get(ctx, poolsResource, id)
	if err != nil {
		return nil, err
	}
	result, _, err := c.describePool(ctx, raw)
	return result, err
}
func (c *Cluster) ReviewPool(ctx context.Context, id string, body object) (object, error) {
	if _, err := model.Identifier(id); err != nil {
		return nil, err
	}
	review, err := model.ParsePoolReview(body)
	if err != nil {
		return nil, err
	}
	raw, err := c.get(ctx, poolsResource, id)
	if err != nil {
		return nil, err
	}
	result, pool, err := c.describePool(ctx, raw)
	if err != nil {
		return nil, err
	}
	if err := review.Matches(pool); err != nil {
		return nil, err
	}
	if result["canReview"] != true {
		return nil, model.NewProblem(409, "压测结果尚不可审核，或该池已有 StorageClass")
	}
	if err := c.store.Audit(ctx, "pool.review-requested", "请求审核存储池 "+id+"，决定 "+review.Decision, pool.Spec.NodeName, "info"); err != nil {
		return nil, err
	}
	approval := review.Approval(metav1.NewTime(time.Now().UTC()))
	patch, err := json.Marshal(object{"metadata": object{"uid": review.UID, "resourceVersion": review.ResourceVersion}, "spec": object{"allocationApproval": approval}})
	if err != nil {
		return nil, err
	}
	updated, err := c.kube.Resource(poolsResource).Patch(ctx, id, types.MergePatchType, patch, metav1.PatchOptions{})
	c.Invalidate()
	auditCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err != nil {
		auditErr := c.store.Audit(auditCtx, "pool.review-failed", "存储池审核更新失败，请刷新后重试", pool.Spec.NodeName, "warning")
		return nil, errors.Join(kubeError(err), auditErr)
	}
	if err := c.store.Audit(auditCtx, "pool.reviewed", "存储池 "+id+" 审核结果 "+review.Decision, pool.Spec.NodeName, "info"); err != nil {
		return nil, err
	}
	value, _, err := c.describePool(ctx, updated.Object)
	return value, err
}
