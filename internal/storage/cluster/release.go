package cluster

import (
	"context"
	"database/sql"
	"sort"

	"github.com/SongOf/pulsar-operator/internal/storage/model"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func retainedState(record object, pvs map[string]object) object {
	result := copyObject(record)
	result["canManage"] = false
	delete(result, "message")
	if str(record, "pvName") == "" {
		result["status"] = "Deleted"
		return result
	}
	pv := pvs[str(record, "pvName")]
	if pv == nil || (str(record, "pvUid") != "" && str(pv, "metadata", "uid") != str(record, "pvUid")) || str(pv, "spec", "claimRef", "uid") != str(record, "pvcUid") || str(pv, "spec", "persistentVolumeReclaimPolicy") != "Retain" {
		result["status"], result["message"] = "ReleaseRequested", "PVC 已不存在；保留 PV 身份或策略需要人工核对"
		return result
	}
	result["status"] = "ReleaseRequested"
	if str(pv, "status", "phase") == "Released" {
		result["status"] = "Released"
	}
	return result
}
func (c *Cluster) finishRelease(ctx context.Context, record object) error {
	return c.store.Transaction(ctx, func(tx *sql.Tx) error {
		if err := c.store.Put(ctx, tx, "retained", record); err != nil {
			return err
		}
		return c.store.Delete(ctx, tx, "release-intents", str(record, "id"))
	})
}
func (c *Cluster) releaseHistory(ctx context.Context, key, kind string) ([]object, error) {
	page, err := c.store.List(ctx, kind, model.ListOptions{Limit: 1000000})
	if err != nil {
		return nil, err
	}
	records := []object{}
	for _, record := range page.Items {
		if str(record, "pvcKey") == key {
			records = append(records, record)
		}
	}
	sort.SliceStable(records, func(i, j int) bool { return str(records[i], "requestedAt") > str(records[j], "requestedAt") })
	return records, nil
}
func (c *Cluster) recoverRelease(ctx context.Context, record object) (object, error) {
	pvs := map[string]object{}
	if name := str(record, "pvName"); name != "" {
		pv, err := c.get(ctx, pvsResource, name)
		if err != nil && !apierrors.IsNotFound(err) {
			return nil, err
		}
		if pv != nil {
			pvs[name] = pv
		}
	}
	result := retainedState(record, pvs)
	if err := c.finishRelease(ctx, result); err != nil {
		return nil, err
	}
	c.Invalidate()
	return result, nil
}
func historyUID(intents, completed []object, uid string) []object {
	result := []object{}
	for _, records := range [][]object{intents, completed} {
		for _, record := range records {
			if str(record, "pvcUid") == uid {
				result = append(result, record)
			}
		}
	}
	return result
}

func (c *Cluster) Release(ctx context.Context, key, expectedUID string) (object, error) {
	if expectedUID != "" {
		if _, err := model.Identifier(expectedUID); err != nil {
			return nil, err
		}
	}
	client, name, err := c.pvcClient(key)
	if err != nil {
		return nil, err
	}
	intents, err := c.releaseHistory(ctx, key, "release-intents")
	if err != nil {
		return nil, err
	}
	completed, err := c.releaseHistory(ctx, key, "retained")
	if err != nil {
		return nil, err
	}
	value, err := client.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return nil, kubeError(err)
		}
		history := intents
		if len(history) == 0 {
			history = completed
		}
		if expectedUID != "" {
			history = historyUID(intents, completed, expectedUID)
		}
		if len(history) == 0 {
			return nil, err
		}
		return c.recoverRelease(ctx, history[0])
	}
	pvc := value.Object
	meta := asObject(pvc["metadata"])
	uid := str(meta, "uid")
	if expectedUID != "" && expectedUID != uid {
		history := historyUID(intents, completed, expectedUID)
		if len(history) > 0 {
			return c.recoverRelease(ctx, history[0])
		}
		return nil, model.NewProblem(409, "PVC UID 已变化，请刷新后重新确认释放")
	}
	if expectedUID == "" && len(intents) == 0 && len(completed) > 0 && str(completed[0], "pvcUid") != uid {
		return nil, model.NewProblem(409, "同名 PVC 已重建，释放时必须提供当前 PVC UID")
	}
	if len(intents) > 0 && str(intents[0], "pvcUid") != uid {
		if expectedUID == "" {
			return c.recoverRelease(ctx, intents[0])
		}
		if _, err := c.recoverRelease(ctx, intents[0]); err != nil {
			return nil, err
		}
		intents = nil
	}
	if !canManagePVC(meta) {
		return nil, model.NewProblem(403, "该 PVC 由外部工作负载管理，DiskFleet 仅提供只读观察")
	}
	if len(intents) > 0 && str(meta, "deletionTimestamp") != "" {
		result := copyObject(intents[0])
		result["status"], result["canManage"] = "ReleaseRequested", false
		if err := c.finishRelease(ctx, result); err != nil {
			return nil, err
		}
		c.Invalidate()
		return result, nil
	}
	pvName := str(pvc, "spec", "volumeName")
	pv := object{}
	if pvName != "" {
		pv, err = c.get(ctx, pvsResource, pvName)
		if err != nil {
			return nil, err
		}
		if str(pv, "spec", "persistentVolumeReclaimPolicy") != "Retain" {
			return nil, model.NewProblem(409, "实际 PV 回收策略不是 Retain，拒绝释放")
		}
		for _, field := range []string{"uid", "namespace", "name"} {
			if str(pv, "spec", "claimRef", field) != str(meta, field) {
				return nil, model.NewProblem(409, "实际 PV claimRef 与 PVC 身份不匹配，拒绝释放")
			}
		}
	}
	c.Invalidate()
	data, err := c.read(ctx)
	if err != nil {
		return nil, err
	}
	var observed object
	for _, volume := range data["volumes"] {
		if str(volume, "id") == key {
			observed = volume
			break
		}
	}
	if observed == nil {
		return nil, model.NewProblem(409, "卷快照不可用，请重试")
	}
	if str(observed, "pvcUid") != uid || str(observed, "bindingMessage") != "" {
		return nil, model.NewProblem(409, "PVC/PV 绑定身份无法核验，拒绝释放")
	}
	rv := str(meta, "resourceVersion")
	if uid == "" || rv == "" {
		return nil, model.NewProblem(409, "PVC UID 或 resourceVersion 缺失，拒绝释放")
	}
	record := copyObject(observed)
	record["id"], record["pvcUid"], record["pvcKey"], record["pvcResourceVersion"] = "retained-"+uid, uid, key, rv
	record["pvName"], record["pvUid"] = pvName, str(pv, "metadata", "uid")
	record["requestedAt"] = model.Now()
	if len(intents) > 0 {
		record["requestedAt"] = intents[0]["requestedAt"]
	}
	record["status"], record["name"], record["canManage"] = "ReleaseRequested", name+" (retained)", false
	record["sizeBytes"] = quantity(at(pvc, "spec", "resources", "requests", "storage"))
	// Commit durable intent before DELETE. A timeout, conflict or process crash
	// leaves enough identity to recover without ever deleting a reused name.
	if err := c.store.Transaction(ctx, func(tx *sql.Tx) error {
		if err := c.store.Put(ctx, tx, "release-intents", record); err != nil {
			return err
		}
		return c.store.Event(ctx, tx, "pvc.release-requested", key+" 已请求删除；若仍被 Pod 使用，Kubernetes 会等待卸载。已绑定 PV 保留。", "", "info")
	}); err != nil {
		return nil, err
	}
	c.Invalidate()
	defer c.Invalidate()
	deleteUID := types.UID(uid)
	if err := client.Delete(ctx, name, metav1.DeleteOptions{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "DeleteOptions"}, Preconditions: &metav1.Preconditions{UID: &deleteUID, ResourceVersion: &rv}}); err != nil {
		return nil, kubeError(err)
	}
	if err := c.finishRelease(ctx, record); err != nil {
		return nil, err
	}
	return record, nil
}
