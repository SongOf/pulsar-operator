package cluster

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	storagev1 "github.com/SongOf/pulsar-operator/api/storage/v1alpha1"
	"github.com/SongOf/pulsar-operator/internal/storage/model"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
)

func (c *Cluster) Plan(ctx context.Context, body object) (object, error) {
	name, err := model.Name(body["name"])
	if err != nil {
		return nil, err
	}
	node, err := model.NodeName(body["node"])
	if err != nil {
		return nil, err
	}
	ids := array(body["diskIds"])
	if len(ids) != 1 {
		return nil, model.NewProblem(400, "每个池只能包含一块盘")
	}
	c.Invalidate()
	data, err := c.read(ctx)
	if err != nil {
		return nil, err
	}
	var disk object
	for _, candidate := range data["disks"] {
		if str(candidate, "id") == model.String(ids[0]) {
			disk = candidate
			break
		}
	}
	if disk == nil || str(disk, "node") != node || str(disk, "state") != "Available" || str(disk, "poolId") != "" {
		return nil, model.NewProblem(409, "磁盘不可用或已纳管")
	}
	hash := digest(node + ":" + str(disk, "serial"))[:16]
	vg := "df_" + hash
	return c.store.SavePlan(ctx, object{"name": name, "node": node, "diskIds": clone(ids), "disks": []any{copyObject(disk)}, "vgName": vg, "resourceName": "pool-" + hash, "confirmation": "接管 " + str(disk, "serial"),
		"commands": []any{[]any{"pvcreate", disk["path"]}, []any{"vgcreate", vg, disk["path"]}}, "warnings": []any{"此计划会初始化所选空盘。节点必须显式开启 ENABLE_DISK_WRITES。", "一块盘一个 VG；原始盘不需要挂到 /mnt，业务 LV 由 CSI 挂载。"}})
}

func (c *Cluster) CreatePool(ctx context.Context, planID, confirmation string) (object, error) {
	var plan object
	resource := ""
	err := c.store.Transaction(ctx, func(tx *sql.Tx) error {
		var consumed bool
		var err error
		plan, consumed, err = c.store.ReadPlan(ctx, tx, planID, confirmation)
		if err != nil {
			return err
		}
		resource = str(plan, "resourceName")
		disks := array(plan["disks"])
		if resource == "" || len(disks) != 1 {
			return model.NewProblem(409, "接管计划缺少设备身份")
		}
		disk := asObject(disks[0])
		spec, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&storagev1.DiskPoolSpec{
			NodeName: str(plan, "node"), VGName: str(plan, "vgName"), Approved: true,
			Devices: []storagev1.Device{{Path: str(disk, "path"), Serial: str(disk, "serial"), SizeBytes: model.Int64(disk["sizeBytes"])}},
		})
		if err != nil {
			return err
		}
		body := object{"apiVersion": storagev1.GroupVersion.String(), "kind": "DiskPool", "metadata": object{"name": resource, "labels": object{label + "node": nodeLabel(str(plan, "node"))}, "annotations": object{label + "display-name": str(plan, "name")}}, "spec": spec}
		claims, err := c.listKube(ctx, poolsResource, "")
		if err != nil {
			return err
		}
		for _, other := range claims {
			if str(other, "metadata", "name") != resource && claimsOverlap(spec, asObject(other["spec"])) {
				return model.NewProblem(409, "磁盘或 VG 已由其它 DiskPool 认领："+str(other, "metadata", "name"))
			}
		}
		existing, err := c.get(ctx, poolsResource, resource)
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		matches := func(existing object) bool {
			return equal(existing["spec"], spec) && str(existing, "metadata", "deletionTimestamp") == ""
		}
		if existing != nil {
			if !matches(existing) {
				return model.NewProblem(409, "磁盘已有参数不同或正在删除的接管请求")
			}
		} else {
			if consumed {
				return model.NewProblem(409, "原接管资源已删除，拒绝自动重建；请调查磁盘状态")
			}
			current, err := c.get(ctx, disksResource, str(disk, "id"))
			if err != nil {
				return err
			}
			actual := asObject(current["spec"])
			if str(actual, "nodeName") != str(disk, "node") || str(actual, "path") != str(disk, "path") || str(actual, "serial") != str(disk, "serial") || model.Int64(actual["sizeBytes"]) != model.Int64(disk["sizeBytes"]) || str(current, "status", "phase") != "Available" || str(current, "status", "poolName") != "" {
				return model.NewProblem(409, "磁盘身份或使用状态已变化，请重新生成接管计划")
			}
			_, err = c.create(ctx, poolsResource, body)
			if apierrors.IsAlreadyExists(err) {
				existing, err = c.get(ctx, poolsResource, resource)
				if err == nil && !matches(existing) {
					return model.NewProblem(409, "磁盘已有参数不同或正在删除的接管请求")
				}
			}
			if err != nil {
				return err
			}
		}
		if err := c.store.ConsumePlan(ctx, tx, planID); err != nil {
			return err
		}
		return c.store.Event(ctx, tx, "pool.requested", "提交磁盘接管请求 "+resource, str(plan, "node"), "info")
	})
	c.Invalidate()
	if err != nil {
		return nil, err
	}
	data, err := c.read(ctx)
	if err != nil {
		return nil, err
	}
	for _, pool := range data["pools"] {
		if str(pool, "id") == resource {
			return copyObject(pool), nil
		}
	}
	return object{"id": resource, "name": str(plan, "name"), "state": "Pending"}, nil
}

func (c *Cluster) CreateVolume(ctx context.Context, body object) (object, error) {
	name, err := model.Name(body["name"])
	if err != nil {
		return nil, err
	}
	namespaceValue, supplied := body["namespace"]
	if !supplied {
		namespaceValue = "default"
	}
	namespace, err := model.Name(namespaceValue)
	if err != nil {
		return nil, err
	}
	size, err := model.SizeGiB(body["sizeGiB"])
	if err != nil {
		return nil, err
	}
	poolID, err := model.Identifier(body["poolId"])
	if err != nil {
		return nil, err
	}
	c.Invalidate()
	data, err := c.read(ctx)
	if err != nil {
		return nil, err
	}
	var pool object
	for _, candidate := range data["pools"] {
		if str(candidate, "id") == poolID {
			pool = candidate
			break
		}
	}
	if pool == nil || str(pool, "state") != "Ready" || str(pool, "storageClass") == "" {
		return nil, model.NewProblem(409, "存储池或 StorageClass 尚未就绪")
	}
	actual, err := c.get(ctx, classesResource, str(pool, "storageClass"))
	if err != nil {
		return nil, err
	}
	raw := object{"metadata": object{"name": poolID, "uid": str(pool, "uid")}, "spec": object{"nodeName": str(pool, "node"), "vgName": str(pool, "vgName")}}
	expected := storageClass(raw, str(pool, "topologyHostname"))
	if str(pool, "uid") == "" || str(pool, "topologyHostname") == "" || str(expected, "metadata", "name") != str(pool, "storageClass") || str(actual, "metadata", "annotations", label+"pool-uid") != str(pool, "uid") || !classMatches(actual, expected) {
		return nil, model.NewProblem(409, "StorageClass 安全配置发生漂移，拒绝创建 PVC")
	}
	request := object{"apiVersion": "v1", "kind": "PersistentVolumeClaim", "metadata": object{"name": name, "namespace": namespace, "labels": object{label + "managed": "true", label + "pool": poolID}}, "spec": object{"accessModes": []any{"ReadWriteOnce"}, "volumeMode": "Filesystem", "storageClassName": str(pool, "storageClass"), "resources": object{"requests": object{"storage": fmt.Sprintf("%dGi", size/model.GiB)}}}}
	client := c.kube.Resource(pvcsResource).Namespace(namespace)
	_, err = client.Create(ctx, &unstructured.Unstructured{Object: request}, metav1.CreateOptions{})
	defer c.Invalidate()
	if apierrors.IsAlreadyExists(err) {
		old, readErr := client.Get(ctx, name, metav1.GetOptions{})
		if readErr != nil {
			return nil, kubeError(readErr)
		}
		pvc := old.Object
		if str(pvc, "metadata", "deletionTimestamp") != "" {
			return nil, model.NewProblem(409, "同名 PVC 正在删除；等待删除完成后重新申请")
		}
		if !canManagePVC(asObject(pvc["metadata"])) || str(pvc, "spec", "storageClassName") != str(pool, "storageClass") || quantity(at(pvc, "spec", "resources", "requests", "storage")) != size {
			return nil, model.NewProblem(409, "同名 PVC 已存在且参数不同")
		}
	} else if err != nil {
		return nil, kubeError(err)
	}
	if err := c.store.Audit(ctx, "pvc.created", namespace+"/"+name+" 等待消费 Pod 后由 CSI 分配", str(pool, "node"), "info"); err != nil {
		return nil, err
	}
	return object{"id": namespace + "~" + name, "name": name, "namespace": namespace, "status": "Pending", "sizeBytes": size, "poolId": poolID, "node": str(pool, "node")}, nil
}

func (c *Cluster) pvcClient(key string) (dynamic.ResourceInterface, string, error) {
	parts := strings.Split(key, "~")
	if len(parts) != 2 {
		return nil, "", model.NewProblem(400, "无效 PVC 标识")
	}
	namespace, err := model.Name(parts[0])
	if err != nil {
		return nil, "", err
	}
	name, err := model.Name(parts[1])
	if err != nil {
		return nil, "", err
	}
	return c.kube.Resource(pvcsResource).Namespace(namespace), name, nil
}
func (c *Cluster) volume(ctx context.Context, key string) (dynamic.ResourceInterface, object, error) {
	client, name, err := c.pvcClient(key)
	if err != nil {
		return nil, nil, err
	}
	value, err := client.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, nil, kubeError(err)
	}
	pvc := value.Object
	if !canManagePVC(asObject(pvc["metadata"])) {
		return nil, nil, model.NewProblem(403, "该 PVC 由外部工作负载管理，DiskFleet 仅提供只读观察")
	}
	if str(pvc, "spec", "volumeName") != "" || str(pvc, "status", "phase") == "Bound" {
		c.Invalidate()
		data, err := c.read(ctx)
		if err != nil {
			return nil, nil, err
		}
		verified := false
		for _, volume := range data["volumes"] {
			if str(volume, "id") == key && str(volume, "pvcUid") == str(pvc, "metadata", "uid") && volume["bindingVerified"] == true {
				verified = true
				break
			}
		}
		if !verified {
			return nil, nil, model.NewProblem(409, "PVC/PV 绑定身份无法核验，拒绝修改")
		}
	}
	return client, pvc, nil
}
func (c *Cluster) Expand(ctx context.Context, key string, body object) (object, error) {
	size, err := model.SizeGiB(body["sizeGiB"])
	if err != nil {
		return nil, err
	}
	client, pvc, err := c.volume(ctx, key)
	if err != nil {
		return nil, err
	}
	if str(pvc, "metadata", "deletionTimestamp") != "" {
		return nil, model.NewProblem(409, "PVC 正在释放，不能扩容")
	}
	if size < quantity(at(pvc, "spec", "resources", "requests", "storage")) {
		return nil, model.NewProblem(409, "不支持缩容")
	}
	rv := str(pvc, "metadata", "resourceVersion")
	if rv == "" {
		return nil, model.NewProblem(409, "PVC resourceVersion 缺失，拒绝修改")
	}
	patch, err := json.Marshal(object{"metadata": object{"resourceVersion": rv}, "spec": object{"resources": object{"requests": object{"storage": fmt.Sprintf("%dGi", size/model.GiB)}}}})
	if err != nil {
		return nil, err
	}
	_, err = client.Patch(ctx, str(pvc, "metadata", "name"), types.MergePatchType, patch, metav1.PatchOptions{})
	defer c.Invalidate()
	if err != nil {
		return nil, kubeError(err)
	}
	if err := c.store.Audit(ctx, "pvc.expanded", key+" 扩容请求已提交", "", "info"); err != nil {
		return nil, err
	}
	return object{"id": key, "sizeBytes": size, "status": "Pending"}, nil
}
