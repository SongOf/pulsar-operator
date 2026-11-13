// Package fleet renders reviewed Pulsar integration manifests. It performs no
// Kubernetes writes and never executes node operations.
package fleet

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"strings"

	pulsarv1 "github.com/SongOf/pulsar-operator/api/pulsar/v1alpha1"
	"github.com/SongOf/pulsar-operator/internal/storage/cluster"
	"github.com/SongOf/pulsar-operator/internal/storage/model"
)

const (
	Label            = "storage.diskfleet.io/"
	TwoNodeLabel     = "pulsar.songof.dev/two-node"
	SourceRevision   = "3c042c5"
	SourceRepository = "https://github.com/SongOf/pulsar-operator"
)

// The component defaults were captured from the original reviewed planner.
//
//go:embed templates/pulsar-spec.json
var templates embed.FS

var roles = []string{"zookeeper", "journal", "ledger"}
var vgPattern = regexp.MustCompile(`^df_[a-z0-9][a-z0-9_-]{0,51}$`)

func TwoNode(profile string) bool {
	return profile == "two-node-lab" || profile == "two-node-shared-disk-lab"
}
func object(value any) model.Object            { result, _ := value.(map[string]any); return result }
func text(obj model.Object, key string) string { return model.String(obj[key]) }
func values(value any) []any                   { result, _ := value.([]any); return result }
func set(list []string) map[string]bool {
	result := map[string]bool{}
	for _, value := range list {
		result[value] = true
	}
	return result
}

func poolIdentity(p model.Object) error {
	if text(p, "state") != "Ready" || text(p, "uid") == "" || text(p, "node") == "" {
		return model.NewProblem(409, "共享存储类只允许引用具有 UID 的 Ready 池")
	}
	if !vgPattern.MatchString(text(p, "vgName")) {
		return model.NewProblem(400, "不合法的 DiskFleet VG 名称")
	}
	if text(p, "topologyHostname") == "" {
		return model.NewProblem(400, "缺少节点的真实 hostname 拓扑")
	}
	return nil
}

func ValidateTwoNodeTargets(pools []model.Object, assignments map[string][]string, nodes []model.Object, profile string) error {
	selected := map[string]model.Object{}
	count := 0
	for _, node := range nodes {
		meta := object(node["metadata"])
		if text(object(meta["labels"]), TwoNodeLabel) != "true" {
			continue
		}
		count++
		selected[text(meta, "name")] = node
	}
	if count != 2 {
		return model.NewProblem(409, profile+" 要求恰好 2 个带 "+TwoNodeLabel+"=true 标签的节点")
	}
	if len(selected) != 2 || selected[""] != nil {
		return model.NewProblem(409, profile+" 节点名称缺失或重复")
	}
	for name, node := range selected {
		ready := false
		for _, value := range values(object(node["status"])["conditions"]) {
			condition := object(value)
			if text(condition, "type") == "Ready" && text(condition, "status") == "True" {
				ready = true
			}
		}
		meta := object(node["metadata"])
		if !ready || object(node["spec"])["unschedulable"] == true || text(meta, "deletionTimestamp") != "" {
			return model.NewProblem(409, name+" 必须 Ready、未封锁且未删除，才能生成 "+profile+" 计划")
		}
		if text(object(meta["labels"]), "kubernetes.io/hostname") == "" {
			return model.NewProblem(409, name+" 缺少真实 kubernetes.io/hostname 标签")
		}
	}
	byID := map[string]model.Object{}
	for _, p := range pools {
		byID[text(p, "id")] = p
	}
	for _, role := range roles {
		if len(assignments[role]) == 0 {
			return model.NewProblem(400, role+" 缺少已审核的存储池")
		}
		nodes := map[string]bool{}
		for _, id := range assignments[role] {
			p := byID[id]
			if p == nil {
				return model.NewProblem(400, role+" 缺少已审核的存储池")
			}
			node := text(p, "node")
			nodes[node] = true
			actual := selected[node]
			if actual == nil {
				return model.NewProblem(409, role+" 池所在节点必须与已标记的两个 Ready 节点完全相同")
			}
			if text(p, "topologyHostname") != text(object(object(actual["metadata"])["labels"]), "kubernetes.io/hostname") {
				return model.NewProblem(409, id+" 的 hostname 拓扑与实时节点标签不一致")
			}
		}
		if len(nodes) != len(selected) {
			return model.NewProblem(409, role+" 池所在节点必须与已标记的两个 Ready 节点完全相同")
		}
	}
	return nil
}

func PulsarPlan(pools []model.Object, assignments map[string][]string, namespace, name string, replicas int, profile string) (model.Object, error) {
	if _, err := model.Name(namespace); err != nil {
		return nil, err
	}
	if err := pulsarv1.ValidateClusterName(name); err != nil {
		return nil, model.NewProblem(400, err.Error())
	}
	twoNode, shared := TwoNode(profile), profile == "two-node-shared-disk-lab"
	if profile != "three-node-e2e" && !twoNode {
		return nil, model.NewProblem(400, "不支持的 Pulsar profile："+profile)
	}
	if len(name) > 40 {
		return nil, model.NewProblem(400, "验收用 PulsarCluster 名称不能超过 40 个字符")
	}
	if !strings.HasPrefix(namespace, "diskfleet-pulsar-") {
		return nil, model.NewProblem(400, "测试命名空间必须以 diskfleet-pulsar- 开头")
	}
	if replicas < 3 {
		return nil, model.NewProblem(400, "多节点验证至少需要 3 个副本")
	}
	if twoNode && replicas != 3 {
		return nil, model.NewProblem(400, profile+" 固定为 3 个 ZooKeeper 和 3 个 Bookie 副本")
	}
	if len(assignments) != 3 {
		return nil, model.NewProblem(400, "必须分别指定 zookeeper、journal、ledger 池集合")
	}
	byID := map[string]model.Object{}
	for _, p := range pools {
		byID[text(p, "id")] = p
	}
	if len(byID) != len(pools) {
		return nil, model.NewProblem(400, "存储池清单中存在重复 ID")
	}
	selected := map[string][]model.Object{}
	roleHosts := map[string]map[string]bool{}
	for _, role := range roles {
		ids := assignments[role]
		if len(ids) == 0 || len(set(ids)) != len(ids) {
			return nil, model.NewProblem(400, role+" 需要不重复的池 ID 列表")
		}
		nodes, hosts := map[string]bool{}, map[string]bool{}
		for _, id := range ids {
			p := byID[id]
			if p == nil {
				return nil, model.NewProblem(400, role+" 引用了不存在的池")
			}
			if err := poolIdentity(p); err != nil {
				return nil, err
			}
			selected[role] = append(selected[role], p)
			nodes[text(p, "node")] = true
			hosts[text(p, "topologyHostname")] = true
		}
		if len(nodes) != len(ids) {
			return nil, model.NewProblem(400, role+" 在每个节点只能选择一个池")
		}
		if len(hosts) != len(ids) {
			return nil, model.NewProblem(400, "多个节点使用了相同 hostname；无法验证故障域分离")
		}
		roleHosts[role] = hosts
		if twoNode && len(ids) != 2 {
			return nil, model.NewProblem(400, role+" 的 "+profile+" 配置必须恰好覆盖 2 个不同节点")
		}
		if !twoNode && len(ids) < replicas {
			return nil, model.NewProblem(400, fmt.Sprintf("%s 需要至少 %d 个不同节点，不能将单节点类用于 Required 反亲和", role, replicas))
		}
	}
	if !reflect.DeepEqual(roleHosts["zookeeper"], roleHosts["journal"]) || !reflect.DeepEqual(roleHosts["journal"], roleHosts["ledger"]) {
		return nil, model.NewProblem(400, "三个存储等级必须覆盖相同节点，防止多 PVC 拓扑交集为空")
	}
	if shared && (!reflect.DeepEqual(set(assignments["zookeeper"]), set(assignments["journal"])) || !reflect.DeepEqual(set(assignments["journal"]), set(assignments["ledger"]))) {
		return nil, model.NewProblem(400, "two-node-shared-disk-lab 的三个存储角色必须选择同一组 2 个 Ready 池；每节点一个共享数据盘")
	}
	if !shared {
		journals := set(assignments["journal"])
		for _, id := range assignments["ledger"] {
			if journals[id] {
				return nil, model.NewProblem(400, "journal 与 ledger 必须使用不同物理盘/存储池")
			}
		}
	}
	hostNodes := map[string]string{}
	required := map[string]int64{}
	for _, role := range roles {
		allowedNodes, allowedVGs := map[string]bool{}, map[string]bool{}
		allowedIDs := set(assignments[role])
		size := int64(5)
		if role == "ledger" {
			size = 22
		}
		if twoNode {
			size = 15
			if role == "ledger" {
				size = 64
			}
		}
		for _, p := range selected[role] {
			node, host := text(p, "node"), text(p, "topologyHostname")
			allowedNodes[node], allowedVGs[text(p, "vgName")] = true, true
			if previous := hostNodes[host]; previous != "" && previous != node {
				return nil, model.NewProblem(400, "hostname 映射到不同节点")
			}
			hostNodes[host] = node
			required[text(p, "id")] += size * model.GiB
		}
		for _, p := range pools {
			if allowedNodes[text(p, "node")] && allowedVGs[text(p, "vgName")] && !allowedIDs[text(p, "id")] {
				return nil, model.NewProblem(400, role+" 的 VG 名称跨节点冲突，会选中未审核的其他池；请使用唯一 VG 名称")
			}
		}
	}
	for id, size := range required {
		if model.Int64(byID[id]["availableBytes"]) < size {
			return nil, model.NewProblem(409, fmt.Sprintf("%s 容量不足：需要 %d GiB（含验收余量）", id, size/model.GiB))
		}
	}
	identity := namespace + ":" + name
	if twoNode {
		identity += ":" + profile
	}
	sum := sha256.Sum256([]byte(identity))
	suffix := hex.EncodeToString(sum[:])[:10]
	classes := map[string]string{}
	for _, role := range roles {
		classes[role] = "diskfleet-pulsar-" + suffix + "-" + role
	}
	integration := "pulsar"
	if twoNode {
		integration = "pulsar-two-node-lab"
		if shared {
			integration = "pulsar-two-node-shared-disk-lab"
		}
	}
	labels := model.Object{Label + "integration": integration}
	annotations := model.Object{Label + "source-revision": SourceRevision}
	if twoNode {
		labels[Label+"profile"] = profile
		annotations[Label+"availability"] = "lab-only-not-host-failure-ha"
		annotations[Label+"validation"] = "three-node-e2e-not-applicable"
		annotations[Label+"placement"] = "Preferred anti-affinity; a 2+1 placement is not guaranteed"
		annotations[Label+"capacity-preflight"] = "Each selected pool fits all 3 replicas; shared ZK/journal >=30Gi, ledger >=64Gi"
		if shared {
			annotations[Label+"storage-layout"] = "ZooKeeper, journal and ledger share one physical data disk per node; no disk or IO isolation"
			annotations[Label+"capacity-preflight"] = "Each selected shared pool fits all 3 replicas: ZK 15Gi + journal 15Gi + ledger 64Gi = 94Gi available"
		}
	}
	items := []any{model.Object{"apiVersion": "v1", "kind": "Namespace", "metadata": model.Object{"name": namespace, "labels": labels}}}
	for _, role := range roles {
		sc, err := cluster.FleetStorageClass(classes[role], selected[role])
		if err != nil {
			return nil, err
		}
		classLabels := sc.GetLabels()
		for key, value := range labels {
			classLabels[key] = model.String(value)
		}
		classLabels[Label+"role"] = role
		sc.SetLabels(classLabels)
		items = append(items, sc.Object)
	}
	data, err := templates.ReadFile("templates/pulsar-spec.json")
	if err != nil {
		return nil, err
	}
	var spec model.Object
	if err := json.Unmarshal(data, &spec); err != nil {
		return nil, err
	}
	zk, bk := object(spec["zookeeper"]), object(spec["bookkeeper"])
	zk["replicas"], bk["replicas"] = replicas, replicas
	object(zk["storage"])["storageClassName"] = classes["zookeeper"]
	for _, role := range []string{"journal", "ledger"} {
		object(object(bk["storage"])[role])["storageClassName"] = classes[role]
	}
	if twoNode {
		bk["autoRecovery"] = model.Object{"enabled": true, "replicas": 1, "jvm": model.Object{"heap": "256m", "directMemory": "256m"}, "resources": model.Object{"requests": model.Object{"cpu": "100m", "memory": "1Gi"}, "limits": model.Object{"memory": "2Gi"}}}
		proxy := object(spec["proxy"])
		proxy["resources"] = model.Object{"requests": model.Object{"cpu": "100m", "memory": "1536Mi"}, "limits": model.Object{"memory": "2Gi"}}
		for _, component := range []model.Object{zk, bk, object(spec["broker"]), proxy, object(bk["autoRecovery"])} {
			component["pod"] = model.Object{"antiAffinity": "Preferred", "nodeSelector": model.Object{TwoNodeLabel: "true"}, "tolerations": []any{
				model.Object{"key": "node-role.kubernetes.io/control-plane", "operator": "Exists", "effect": "NoSchedule"}, model.Object{"key": "node-role.kubernetes.io/master", "operator": "Exists", "effect": "NoSchedule"},
			}}
		}
	}
	items = append(items, model.Object{"apiVersion": "pulsar.songof.dev/v1alpha1", "kind": "PulsarCluster", "metadata": model.Object{"name": name, "namespace": namespace, "labels": labels, "annotations": annotations}, "spec": spec})
	return model.Object{"apiVersion": "v1", "kind": "List", "items": items}, nil
}
