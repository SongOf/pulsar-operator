package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/SongOf/pulsar-operator/internal/storage/model"
	"github.com/SongOf/pulsar-operator/internal/storage/store"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

type Cluster struct {
	store    *store.Store
	kube     dynamic.Interface
	mu       sync.Mutex
	snapshot map[string][]object
	expires  time.Time
}

func New(localStore *store.Store, kube dynamic.Interface) *Cluster {
	return &Cluster{store: localStore, kube: kube}
}

// Invalidate waits for an in-flight refresh before expiring its snapshot. Thus
// a pre-mutation fetch cannot repopulate the cache after this method returns.
func (c *Cluster) Invalidate() { c.mu.Lock(); defer c.mu.Unlock(); c.expires = time.Time{} }

func (c *Cluster) listKube(ctx context.Context, resource schema.GroupVersionResource, selector string) ([]object, error) {
	options := metav1.ListOptions{Limit: 500, LabelSelector: selector}
	result := []object{}
	seen := map[string]bool{}
	for {
		page, err := c.kube.Resource(resource).List(ctx, options)
		if err != nil {
			return nil, kubeError(err)
		}
		for _, item := range page.Items {
			result = append(result, item.Object)
		}
		options.Continue = page.GetContinue()
		if options.Continue == "" {
			return result, nil
		}
		if seen[options.Continue] {
			return nil, fmt.Errorf("repeated pagination cursor for %s", resource.Resource)
		}
		seen[options.Continue] = true
	}
}
func (c *Cluster) get(ctx context.Context, resource schema.GroupVersionResource, name string) (object, error) {
	obj, err := c.kube.Resource(resource).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, kubeError(err)
	}
	return obj.Object, nil
}
func (c *Cluster) create(ctx context.Context, resource schema.GroupVersionResource, body object) (object, error) {
	obj, err := c.kube.Resource(resource).Create(ctx, &unstructured.Unstructured{Object: body}, metav1.CreateOptions{})
	if err != nil {
		return nil, kubeError(err)
	}
	return obj.Object, nil
}

func (c *Cluster) observedClasses(ctx context.Context, rawPools, pools []object) (map[string][]object, error) {
	byName := map[string]object{}
	for _, pool := range pools {
		byName[str(pool, "id")] = pool
	}
	result := map[string][]object{}
	classes, err := c.listKube(ctx, classesResource, label+"pool")
	if err != nil {
		return nil, err
	}
	byClass := map[string]object{}
	for _, class := range classes {
		byClass[str(class, "metadata", "name")] = class
	}
	for _, raw := range rawPools {
		uid, classname, hostname := str(raw, "metadata", "uid"), str(raw, "status", "storageClass"), str(raw, "status", "topologyHostname")
		if uid == "" || classname == "" || hostname == "" {
			continue
		}
		expected := storageClass(raw, hostname)
		if classname != str(expected, "metadata", "name") {
			continue
		}
		actual := byClass[classname]
		if actual != nil && str(actual, "metadata", "annotations", label+"pool-uid") == uid && classMatches(actual, expected) {
			result[classname] = []object{byName[str(raw, "metadata", "name")]}
		}
	}
	fleets, err := c.listKube(ctx, classesResource, label+"fleet=true")
	if err != nil {
		return nil, err
	}
	for _, actual := range fleets {
		var refs map[string]string
		if err := json.Unmarshal([]byte(str(actual, "metadata", "annotations", label+"pool-uids")), &refs); err != nil || len(refs) == 0 {
			continue
		}
		ids := make([]string, 0, len(refs))
		valid := true
		for id, uid := range refs {
			if uid == "" || byName[id] == nil || str(byName[id], "uid") != uid {
				valid = false
				break
			}
			ids = append(ids, id)
		}
		if !valid {
			continue
		}
		sort.Strings(ids)
		members, ready := []object{}, []object{}
		for _, id := range ids {
			members = append(members, byName[id])
			p := copyObject(byName[id])
			p["state"] = "Ready"
			ready = append(ready, p)
		}
		expected, err := fleetStorageClass(str(actual, "metadata", "name"), ready)
		if err == nil && classMatches(actual, expected) {
			result[str(actual, "metadata", "name")] = members
		}
	}
	return result, nil
}

func (c *Cluster) read(ctx context.Context) (map[string][]object, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Now().Before(c.expires) {
		return c.snapshot, nil
	}
	data := map[string][]object{"disks": {}, "pools": {}, "volumes": {}, "nodes": {}}
	rawPools, err := c.listKube(ctx, poolsResource, "")
	if err != nil {
		return nil, err
	}
	conflicts := conflictingPools(rawPools)
	devices := map[string]object{}
	trustedRaw, trustedPools := []object{}, []object{}
	for _, raw := range rawPools {
		meta, spec, status := asObject(raw["metadata"]), asObject(raw["spec"]), asObject(raw["status"])
		name := str(meta, "name")
		capacity, free := model.Int64(status["capacityBytes"]), model.Int64(status["availableBytes"])
		pool := object{"id": name, "uid": str(meta, "uid"), "name": fallback(at(meta, "annotations", label+"display-name"), name), "node": str(spec, "nodeName"), "vgName": str(spec, "vgName"), "capacityBytes": capacity, "allocatedBytes": max(int64(0), capacity-free), "availableBytes": free, "state": fallback(status["phase"], "Pending"), "diskIds": []any{}, "storageClass": str(status, "storageClass"), "topologyHostname": str(status, "topologyHostname"), "createdAt": str(meta, "creationTimestamp"), "message": str(status, "message")}
		if conflicts[name] {
			pool["state"] = "Conflict"
			pool["capacityBytes"], pool["allocatedBytes"], pool["availableBytes"] = int64(0), int64(0), int64(0)
			pool["message"] = "多个 DiskPool 认领相同节点的 VG 或设备；无法确认唯一所有者，容量不计入可分配汇总"
		} else {
			trustedRaw = append(trustedRaw, raw)
			trustedPools = append(trustedPools, pool)
			for _, device := range array(spec["devices"]) {
				devices[deviceKey(str(spec, "nodeName"), asObject(device))] = pool
			}
		}
		data["pools"] = append(data["pools"], pool)
	}
	rawDisks, err := c.listKube(ctx, disksResource, "")
	if err != nil {
		return nil, err
	}
	for _, raw := range rawDisks {
		meta, spec, status := asObject(raw["metadata"]), asObject(raw["spec"]), asObject(raw["status"])
		pool := devices[deviceKey(str(spec, "nodeName"), spec)]
		disk := object{"id": str(meta, "name"), "name": diskBase(str(spec, "path")), "node": str(spec, "nodeName"), "path": str(spec, "path"), "serial": str(spec, "serial"), "sizeBytes": model.Int64(spec["sizeBytes"]), "media": fallback(spec["media"], "Unknown"), "model": str(spec, "model"), "state": fallback(status["phase"], "Protected"), "reasons": append([]any{}, array(status["reasons"])...), "poolId": nil}
		if pool != nil {
			disk["poolId"] = pool["id"]
			pool["diskIds"] = append(array(pool["diskIds"]), disk["id"])
		}
		data["disks"] = append(data["disks"], disk)
	}
	classes, err := c.observedClasses(ctx, trustedRaw, trustedPools)
	if err != nil {
		return nil, err
	}
	pvcs, err := c.listKube(ctx, pvcsResource, "")
	if err != nil {
		return nil, err
	}
	listPVs, err := c.listKube(ctx, pvsResource, "")
	if err != nil {
		return nil, err
	}
	pvs := map[string]object{}
	for _, pv := range listPVs {
		pvs[str(pv, "metadata", "name")] = pv
	}
	liveUIDs, deletingUIDs := map[string]bool{}, map[string]bool{}
	for _, pvc := range pvcs {
		meta, spec, status := asObject(pvc["metadata"]), asObject(pvc["spec"]), asObject(pvc["status"])
		uid := str(meta, "uid")
		liveUIDs[uid] = true
		if str(meta, "deletionTimestamp") != "" {
			deletingUIDs[uid] = true
		}
		members := classes[str(spec, "storageClassName")]
		if len(members) == 0 {
			continue
		}
		bound := str(status, "phase") == "Bound" || str(spec, "volumeName") != ""
		pool := object{}
		message, node := "", ""
		if bound {
			pool, message = boundPool(pvc, pvs[str(spec, "volumeName")], members)
			node = str(pool, "node")
		} else {
			selected := str(meta, "annotations", "volume.kubernetes.io/selected-node")
			eligible := members
			if selected != "" {
				eligible = []object{}
				for _, member := range members {
					if str(member, "node") == selected {
						eligible = append(eligible, member)
					}
				}
			}
			if len(eligible) == 1 {
				pool = eligible[0]
			}
			node = str(pool, "node")
			if selected != "" && len(eligible) > 0 {
				node = selected
			}
		}
		owners := []any{}
		for _, owner := range array(meta["ownerReferences"]) {
			owners = append(owners, object{"kind": str(owner, "kind"), "name": str(owner, "name"), "uid": str(owner, "uid")})
		}
		management := "external"
		if canManagePVC(meta) {
			management = "diskfleet"
		}
		state := fallback(status["phase"], "Pending")
		if deletingUIDs[uid] {
			state = "Releasing"
		}
		data["volumes"] = append(data["volumes"], object{"id": str(meta, "namespace") + "~" + str(meta, "name"), "pvcUid": uid, "name": str(meta, "name"), "namespace": str(meta, "namespace"), "poolId": str(pool, "id"), "pool": fallback(pool["name"], "多节点存储类"), "node": node, "sizeBytes": quantity(at(spec, "resources", "requests", "storage")), "status": state, "storageClass": str(spec, "storageClassName"), "pvName": str(spec, "volumeName"), "mountPath": "由 CSI / kubelet 管理", "createdAt": str(meta, "creationTimestamp"), "management": management, "canManage": canManagePVC(meta) && message == "", "owners": owners, "bindingVerified": bound && len(pool) > 0, "bindingMessage": message})
	}
	type nodeStats struct {
		disks               int
		capacity, allocated int64
	}
	stats := map[string]*nodeStats{}
	for _, disk := range data["disks"] {
		node := str(disk, "node")
		if stats[node] == nil {
			stats[node] = &nodeStats{}
		}
		stats[node].disks++
		stats[node].capacity += model.Int64(disk["sizeBytes"])
	}
	for _, pool := range data["pools"] {
		node := str(pool, "node")
		if stats[node] == nil {
			stats[node] = &nodeStats{}
		}
		stats[node].allocated += model.Int64(pool["allocatedBytes"])
	}
	nodes, err := c.listKube(ctx, nodesResource, label+"enabled=true")
	if err != nil {
		return nil, err
	}
	for _, raw := range nodes {
		meta := asObject(raw["metadata"])
		node := str(meta, "name")
		state := "NotReady"
		for _, condition := range array(at(raw, "status", "conditions")) {
			if str(condition, "type") == "Ready" {
				if str(condition, "status") == "True" {
					state = "Ready"
				}
				break
			}
		}
		s := stats[node]
		if s == nil {
			s = &nodeStats{}
		}
		data["nodes"] = append(data["nodes"], object{"name": node, "zone": fallback(at(meta, "labels", "topology.kubernetes.io/zone"), "—"), "state": state, "disks": s.disks, "capacityBytes": s.capacity, "allocatedBytes": s.allocated})
	}
	intents, err := c.store.List(ctx, "release-intents", model.ListOptions{Limit: 1000000})
	if err != nil {
		return nil, err
	}
	for _, intent := range intents.Items {
		uid := str(intent, "pvcUid")
		var result object
		if !liveUIDs[uid] {
			result = retainedState(intent, pvs)
		} else if deletingUIDs[uid] {
			result = copyObject(intent)
			result["status"], result["canManage"] = "ReleaseRequested", false
		}
		if result != nil {
			if err := c.finishRelease(ctx, result); err != nil {
				return nil, err
			}
		}
	}
	retained, err := c.store.List(ctx, "retained", model.ListOptions{Limit: 1000000})
	if err != nil {
		return nil, err
	}
	for _, record := range retained.Items {
		uid := fallback(record["pvcUid"], strings.TrimPrefix(str(record, "id"), "retained-"))
		if !liveUIDs[uid] {
			record = copyObject(record)
			record["pvcUid"] = uid
			data["volumes"] = append(data["volumes"], retainedState(record, pvs))
		}
	}
	c.snapshot, c.expires = data, time.Now().Add(30*time.Second)
	return data, nil
}

func (c *Cluster) List(ctx context.Context, kind string, options model.ListOptions) (model.Page, error) {
	data, err := c.read(ctx)
	if err != nil {
		return model.Page{}, err
	}
	items, ok := data[kind]
	if !ok {
		return model.Page{}, model.NewProblem(404, "接口不存在")
	}
	if options.Limit <= 0 {
		options.Limit = 50
	}
	if options.Offset < 0 {
		options.Offset = 0
	}
	filtered := []object{}
	for _, item := range items {
		if options.Node != "" && fallback(item["node"], str(item, "name")) != options.Node {
			continue
		}
		if options.State != "" && fallback(item["state"], str(item, "status")) != options.State {
			continue
		}
		if options.Query != "" {
			data, _ := json.Marshal(item)
			if !strings.Contains(strings.ToLower(string(data)), strings.ToLower(options.Query)) {
				continue
			}
		}
		filtered = append(filtered, item)
	}
	sort.SliceStable(filtered, func(i, j int) bool {
		if str(filtered[i], "node") == str(filtered[j], "node") {
			return str(filtered[i], "name") < str(filtered[j], "name")
		}
		return str(filtered[i], "node") < str(filtered[j], "node")
	})
	page := model.Page{Items: []object{}, Total: len(filtered), Limit: options.Limit, Offset: options.Offset}
	start := min(len(filtered), options.Offset)
	end := start + min(options.Limit, len(filtered)-start)
	for _, item := range filtered[start:end] {
		page.Items = append(page.Items, copyObject(item))
	}
	return page, nil
}
func (c *Cluster) Events(ctx context.Context, options model.ListOptions) (model.Page, error) {
	return c.store.Events(ctx, options)
}
func (c *Cluster) Overview(ctx context.Context) (object, error) {
	data, err := c.read(ctx)
	if err != nil {
		return nil, err
	}
	totals := object{}
	for key, items := range data {
		totals[key] = len(items)
	}
	var capacity, allocated, available int64
	alerts := []any{}
	for _, pool := range data["pools"] {
		capacity += model.Int64(pool["capacityBytes"])
		allocated += model.Int64(pool["allocatedBytes"])
		available += model.Int64(pool["availableBytes"])
		state := str(pool, "state")
		if state != "Ready" && state != "Pending" {
			alerts = append(alerts, object{"level": "warning", "message": str(pool, "name") + ": " + fallback(pool["message"], state)})
		}
	}
	totals["capacityBytes"], totals["allocatedBytes"], totals["availableBytes"] = capacity, allocated, available
	events, err := c.Events(ctx, model.ListOptions{Limit: 8})
	if err != nil {
		return nil, err
	}
	return object{"mode": "cluster", "totals": totals, "recentEvents": events.Items, "alerts": alerts}, nil
}

var _ model.Backend = (*Cluster)(nil)
