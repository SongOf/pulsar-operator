package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/SongOf/pulsar-operator/internal/storage/model"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

const agentNamespace = "diskfleet-system"
const agentName = "diskfleet-agent"

var podsResource = schema.GroupVersionResource{Version: "v1", Resource: "pods"}
var daemonsetsResource = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "daemonsets"}

func nodeHealth(raw object) []string {
	reasons := []string{}
	osLabel, osInfo := str(raw, "metadata", "labels", "kubernetes.io/os"), str(raw, "status", "nodeInfo", "operatingSystem")
	if (osLabel == "" && osInfo == "") || (osLabel != "" && osLabel != "linux") || (osInfo != "" && osInfo != "linux") {
		reasons = append(reasons, "Agent 仅支持已确认操作系统为 Linux 的节点")
	}
	if str(raw, "metadata", "uid") == "" {
		reasons = append(reasons, "节点 UID 缺失")
	}
	if str(raw, "metadata", "deletionTimestamp") != "" {
		reasons = append(reasons, "节点正在删除")
	}
	if at(raw, "spec", "unschedulable") == true {
		reasons = append(reasons, "节点已封锁")
	}
	ready := false
	for _, condition := range array(at(raw, "status", "conditions")) {
		typ, status := str(condition, "type"), str(condition, "status")
		if typ == "Ready" {
			ready = status == "True"
		}
		if (typ == "MemoryPressure" || typ == "DiskPressure" || typ == "PIDPressure" || typ == "NetworkUnavailable") && status != "False" {
			reasons = append(reasons, "节点条件异常："+typ)
		}
	}
	if !ready {
		reasons = append(reasons, "节点尚未 Ready")
	}
	return reasons
}
func (c *Cluster) agentObjects(ctx context.Context, nodeNames ...string) (object, []object, error) {
	ds, err := c.kube.Resource(daemonsetsResource).Namespace(agentNamespace).Get(ctx, agentName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, []object{}, nil
	}
	if err != nil {
		return nil, nil, kubeError(err)
	}
	pods := []object{}
	opts := metav1.ListOptions{Limit: 500}
	if len(nodeNames) > 0 && nodeNames[0] != "" {
		opts.FieldSelector = "spec.nodeName=" + nodeNames[0]
	}
	seen := map[string]bool{}
	for {
		page, err := c.kube.Resource(podsResource).Namespace(agentNamespace).List(ctx, opts)
		if err != nil {
			return nil, nil, kubeError(err)
		}
		for _, pod := range page.Items {
			pods = append(pods, pod.Object)
		}
		opts.Continue = page.GetContinue()
		if opts.Continue == "" {
			break
		}
		if seen[opts.Continue] {
			return nil, nil, model.NewProblem(502, "Agent Pod 分页游标重复")
		}
		seen[opts.Continue] = true
	}
	return ds.Object, pods, nil
}
func describeNode(raw, ds object, pods []object) object {
	meta := asObject(raw["metadata"])
	name := str(meta, "name")
	reasons := nodeHealth(raw)
	ready := false
	for _, condition := range array(at(raw, "status", "conditions")) {
		if str(condition, "type") == "Ready" {
			ready = str(condition, "status") == "True"
		}
	}
	if ds == nil || str(ds, "metadata", "uid") == "" || str(ds, "metadata", "deletionTimestamp") != "" {
		reasons = append(reasons, "请先安装可用的 diskfleet-system/diskfleet-agent DaemonSet")
	} else {
		selector := asObject(at(ds, "spec", "template", "spec", "nodeSelector"))
		if str(selector, label+"enabled") != "true" {
			reasons = append(reasons, "Agent DaemonSet 必须使用启用标签作为节点选择器")
		}
		for key, value := range selector {
			if key != label+"enabled" && at(meta, "labels", key) != value {
				reasons = append(reasons, "节点不匹配 Agent nodeSelector："+key)
			}
		}
		if target := str(ds, "spec", "template", "spec", "nodeName"); target != "" && target != name {
			reasons = append(reasons, "节点不匹配 Agent 固定 nodeName")
		}
	}
	enrolled := str(meta, "labels", label+"enabled") == "true"
	status := "NotEnrolled"
	if ds == nil {
		status = "NotInstalled"
	} else if enrolled {
		status = "Pending"
	}
	running := false
	for _, pod := range pods {
		if str(pod, "spec", "nodeName") != name || str(pod, "metadata", "deletionTimestamp") != "" || str(pod, "status", "phase") != "Running" {
			continue
		}
		owned := false
		for _, owner := range array(at(pod, "metadata", "ownerReferences")) {
			if str(owner, "kind") == "DaemonSet" && str(owner, "name") == agentName && str(owner, "uid") != "" && str(owner, "uid") == str(ds, "metadata", "uid") && at(owner, "controller") == true {
				owned = true
			}
		}
		if !owned {
			continue
		}
		for _, condition := range array(at(pod, "status", "conditions")) {
			if str(condition, "type") == "Ready" && str(condition, "status") == "True" {
				running = true
			}
		}
	}
	if enrolled && running {
		status = "Running"
	}
	enableReasons := append([]string{}, reasons...)
	canEnable := !enrolled && len(reasons) == 0
	if !enrolled {
		reasons = append(reasons, "节点尚未启用 Agent")
	}
	if !running {
		reasons = append(reasons, "节点 Agent 尚未运行并就绪")
	}
	if enrolled && len(enableReasons) > 0 && ds != nil {
		status = "Unavailable"
	}
	return object{"name": name, "uid": str(meta, "uid"), "resourceVersion": str(meta, "resourceVersion"), "zone": fallback(at(meta, "labels", "topology.kubernetes.io/zone"), "—"), "state": map[bool]string{true: "Ready", false: "NotReady"}[ready], "ready": ready, "unschedulable": at(raw, "spec", "unschedulable") == true, "enrolled": enrolled, "agentStatus": status, "canEnableAgent": canEnable, "enableReasons": enableReasons, "canCreatePool": len(reasons) == 0, "reasons": reasons, "mode": "cluster", "disks": 0, "capacityBytes": int64(0), "allocatedBytes": int64(0)}
}
func describeDisk(raw, node object, claims []object, now time.Time) object {
	meta, spec, status := asObject(raw["metadata"]), asObject(raw["spec"]), asObject(raw["status"])
	reasons := []string{}
	for _, r := range array(status["reasons"]) {
		reasons = append(reasons, model.String(r))
	}
	disk := object{"id": str(meta, "name"), "uid": str(meta, "uid"), "resourceVersion": str(meta, "resourceVersion"), "name": diskBase(str(spec, "path")), "node": str(spec, "nodeName"), "path": str(spec, "path"), "serial": str(spec, "serial"), "sizeBytes": model.Int64(spec["sizeBytes"]), "media": fallback(spec["media"], "Unknown"), "model": str(spec, "model"), "state": fallback(status["phase"], "Protected"), "poolId": nil, "nodeUID": str(status, "nodeUID"), "lastObservedTime": str(status, "lastObservedTime"), "validUntil": str(status, "validUntil"), "mode": "cluster"}
	if node == nil {
		reasons = append(reasons, "所属节点不存在")
	} else {
		for _, r := range array(node["reasons"]) {
			reasons = append(reasons, model.String(r))
		}
	}
	observed, e1 := time.Parse(time.RFC3339Nano, str(status, "lastObservedTime"))
	valid, e2 := time.Parse(time.RFC3339Nano, str(status, "validUntil"))
	fresh := e1 == nil && e2 == nil && !observed.After(now) && valid.After(now) && valid.After(observed) && str(status, "nodeUID") != "" && str(status, "nodeUID") == str(node, "uid")
	disk["fresh"] = fresh
	if !fresh {
		reasons = append(reasons, "磁盘观察已过期、时间无效或节点身份不匹配")
	}
	if str(meta, "uid") == "" || str(meta, "deletionTimestamp") != "" {
		reasons = append(reasons, "磁盘资源身份缺失或正在删除")
	}
	if str(spec, "serial") == "" || model.Int64(spec["sizeBytes"]) <= 0 || !strings.HasPrefix(str(spec, "path"), "/dev/disk/by-id/") {
		reasons = append(reasons, "磁盘稳定设备身份不完整")
	}
	if str(status, "phase") != "Available" {
		reasons = append(reasons, "磁盘不是 Available")
	}
	if str(status, "poolName") != "" {
		disk["poolId"] = str(status, "poolName")
		reasons = append(reasons, "磁盘已纳管")
	}
	claimCount := 0
	for _, pool := range claims {
		if str(pool, "spec", "nodeName") != str(spec, "nodeName") {
			continue
		}
		for _, device := range array(at(pool, "spec", "devices")) {
			if str(device, "path") == str(spec, "path") || str(device, "serial") == str(spec, "serial") {
				disk["poolId"] = str(pool, "metadata", "name")
				claimCount++
				reasons = append(reasons, "磁盘已被 DiskPool 认领")
				break
			}
		}
	}
	if claimCount > 1 {
		disk["poolId"] = nil
	}
	disk["reasons"], disk["canCreatePool"] = reasons, len(reasons) == 0
	return disk
}

// Inventory gates are always computed from fresh API reads, never the display cache.
func (c *Cluster) inventory(ctx context.Context, nodeNames ...string) (map[string][]object, error) {
	var nodes []object
	var err error
	target := ""
	if len(nodeNames) > 0 {
		target = nodeNames[0]
	}
	if target != "" {
		var node object
		node, err = c.get(ctx, nodesResource, target)
		nodes = []object{node}
	} else {
		nodes, err = c.listKube(ctx, nodesResource, "")
	}
	if err != nil {
		return nil, err
	}
	ds, pods, err := c.agentObjects(ctx, target)
	if err != nil {
		return nil, err
	}
	disks, err := c.listKube(ctx, disksResource, "")
	if err != nil {
		return nil, err
	}
	pools, err := c.listKube(ctx, poolsResource, "")
	if err != nil {
		return nil, err
	}
	podsByNode := map[string][]object{}
	for _, pod := range pods {
		name := str(pod, "spec", "nodeName")
		podsByNode[name] = append(podsByNode[name], pod)
	}
	claimsByDevice := map[string][]object{}
	for _, pool := range pools {
		for _, device := range array(at(pool, "spec", "devices")) {
			for _, field := range []string{"path", "serial"} {
				value := str(device, field)
				if value != "" {
					key := str(pool, "spec", "nodeName") + "\x00" + field + "\x00" + value
					claimsByDevice[key] = append(claimsByDevice[key], pool)
				}
			}
		}
	}
	data := map[string][]object{"nodes": {}, "disks": {}}
	byName := map[string]object{}
	for _, raw := range nodes {
		node := describeNode(raw, ds, podsByNode[str(raw, "metadata", "name")])
		byName[str(node, "name")] = node
		data["nodes"] = append(data["nodes"], node)
	}
	for _, raw := range disks {
		if target != "" && str(raw, "spec", "nodeName") != target {
			continue
		}
		node := byName[str(raw, "spec", "nodeName")]
		claims := []object{}
		seen := map[string]bool{}
		for _, field := range []string{"path", "serial"} {
			for _, pool := range claimsByDevice[str(raw, "spec", "nodeName")+"\x00"+field+"\x00"+str(raw, "spec", field)] {
				key := str(pool, "metadata", "name")
				if !seen[key] {
					claims = append(claims, pool)
					seen[key] = true
				}
			}
		}
		disk := describeDisk(raw, node, claims, time.Now())
		data["disks"] = append(data["disks"], disk)
		if node != nil {
			node["disks"] = model.Int64(node["disks"]) + 1
			node["capacityBytes"] = model.Int64(node["capacityBytes"]) + model.Int64(disk["sizeBytes"])
		}
	}
	conflicts := conflictingPools(pools)
	for _, pool := range pools {
		node := byName[str(pool, "spec", "nodeName")]
		if node != nil && !conflicts[str(pool, "metadata", "name")] {
			node["allocatedBytes"] = model.Int64(node["allocatedBytes"]) + max(int64(0), model.Int64(at(pool, "status", "capacityBytes"))-model.Int64(at(pool, "status", "availableBytes")))
		}
	}
	return data, nil
}
func (c *Cluster) Node(ctx context.Context, name string, opts model.ListOptions) (object, error) {
	if _, err := model.NodeName(name); err != nil {
		return nil, err
	}
	data, err := c.inventory(ctx, name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, model.NewProblem(404, "节点不存在")
		}
		return nil, err
	}
	for _, node := range data["nodes"] {
		if str(node, "name") == name {
			opts.Node = name
			return object{"node": node, "disks": pageItems(data["disks"], opts)}, nil
		}
	}
	return nil, model.NewProblem(404, "节点不存在")
}
func (c *Cluster) EnableAgent(ctx context.Context, name string, body object) (object, error) {
	if _, err := model.NodeName(name); err != nil {
		return nil, err
	}
	uid, err := model.Identifier(body["uid"])
	if err != nil {
		return nil, err
	}
	rv, err := model.Identifier(body["resourceVersion"])
	if err != nil {
		return nil, err
	}
	raw, err := c.get(ctx, nodesResource, name)
	if err != nil {
		return nil, err
	}
	if uid != str(raw, "metadata", "uid") || rv != str(raw, "metadata", "resourceVersion") {
		return nil, model.NewProblem(409, "节点身份或版本已变化，请刷新后重试")
	}
	ds, pods, err := c.agentObjects(ctx, name)
	if err != nil {
		return nil, err
	}
	node := describeNode(raw, ds, pods)
	if reasons := array(node["enableReasons"]); len(reasons) > 0 {
		return nil, model.NewProblem(409, joinReasons(reasons))
	}
	if node["enrolled"] == true {
		return node, nil
	}
	if err := c.store.Audit(ctx, "node.agent-enable-requested", "请求启用节点 Agent："+name, name, "info"); err != nil {
		return nil, err
	}
	patch, _ := json.Marshal(object{"metadata": object{"uid": uid, "resourceVersion": rv, "labels": object{label + "enabled": "true"}}})
	updated, err := c.kube.Resource(nodesResource).Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{})
	c.Invalidate()
	if err != nil {
		auditCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		auditErr := c.store.Audit(auditCtx, "node.agent-enable-failed", "节点 Agent 启用请求失败，请刷新节点状态后重试", name, "warning")
		return nil, errors.Join(kubeError(err), auditErr)
	}
	if err := c.store.Audit(ctx, "node.agent-enabled", "已启用节点 Agent；等待调度与磁盘发现，写盘开关未改变", name, "info"); err != nil {
		return nil, err
	}
	return describeNode(updated.Object, ds, pods), nil
}
func joinReasons(reasons []any) string {
	texts := []string{}
	for _, r := range reasons {
		texts = append(texts, model.String(r))
	}
	return strings.Join(texts, "；")
}
