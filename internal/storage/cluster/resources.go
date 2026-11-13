// Package cluster implements the production storage management backend. The
// Kubernetes API remains authoritative; the local store holds plans and audit
// records, never substitutes for the node agent or CSI volume lifecycle.
package cluster

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	storagev1 "github.com/SongOf/pulsar-operator/api/storage/v1alpha1"
	"github.com/SongOf/pulsar-operator/internal/storage/model"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const label = storagev1.GroupName + "/"

var (
	poolsResource   = storagev1.DiskPoolResource
	disksResource   = storagev1.PhysicalDiskResource
	classesResource = schema.GroupVersionResource{Group: "storage.k8s.io", Version: "v1", Resource: "storageclasses"}
	pvcsResource    = schema.GroupVersionResource{Version: "v1", Resource: "persistentvolumeclaims"}
	pvsResource     = schema.GroupVersionResource{Version: "v1", Resource: "persistentvolumes"}
	nodesResource   = schema.GroupVersionResource{Version: "v1", Resource: "nodes"}
	vgPattern       = regexp.MustCompile(`^df_[a-z0-9][a-z0-9_-]{0,51}$`)
	classFields     = []string{"provisioner", "parameters", "reclaimPolicy", "volumeBindingMode", "allowVolumeExpansion", "allowedTopologies"}
)

type object = model.Object

func asObject(value any) object { result, _ := value.(map[string]any); return result }
func at(value any, keys ...string) any {
	for _, key := range keys {
		value = asObject(value)[key]
	}
	return value
}
func str(value any, keys ...string) string { return model.String(at(value, keys...)) }
func fallback(value any, defaultValue string) string {
	if value == nil {
		return defaultValue
	}
	return model.String(value)
}
func array(value any) []any {
	switch values := value.(type) {
	case []any:
		return values
	case []object:
		out := make([]any, len(values))
		for i := range values {
			out[i] = values[i]
		}
		return out
	case []string:
		out := make([]any, len(values))
		for i := range values {
			out[i] = values[i]
		}
		return out
	default:
		return nil
	}
}
func clone(value any) any {
	switch value := value.(type) {
	case map[string]any:
		out := object{}
		for key, item := range value {
			out[key] = clone(item)
		}
		return out
	case []any:
		out := make([]any, len(value))
		for i, item := range value {
			out[i] = clone(item)
		}
		return out
	case []object:
		out := make([]object, len(value))
		for i, item := range value {
			out[i] = copyObject(item)
		}
		return out
	case []string:
		return append([]string{}, value...)
	default:
		return value
	}
}
func copyObject(value object) object {
	if value == nil {
		return object{}
	}
	return clone(value).(object)
}
func equal(a, b any) bool {
	first, e1 := json.Marshal(a)
	second, e2 := json.Marshal(b)
	return e1 == nil && e2 == nil && bytes.Equal(first, second)
}
func digest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
func nodeLabel(value string) string {
	if len(value) <= 63 {
		return value
	}
	return "sha256-" + digest(value)[:40]
}
func className(value string) string {
	if len(value) <= 53 {
		return "diskfleet-" + value
	}
	return "diskfleet-" + value[:44] + "-" + digest(value)[:8]
}
func canManagePVC(metadata object) bool {
	manager := str(metadata, "labels", "app.kubernetes.io/managed-by")
	return str(metadata, "labels", label+"managed") == "true" && (manager == "" || manager == "diskfleet") && len(array(metadata["ownerReferences"])) == 0
}
func classMatches(actual, expected object) bool {
	for _, key := range classFields {
		if !equal(actual[key], expected[key]) {
			return false
		}
	}
	return true
}
func topology(hostnames []string) []any {
	values := make([]any, len(hostnames))
	for i, value := range hostnames {
		values[i] = value
	}
	return []any{object{"matchLabelExpressions": []any{object{"key": "kubernetes.io/hostname", "values": values}}}}
}
func storageClass(raw object, hostname string) object {
	name, node, vg := str(raw, "metadata", "name"), str(raw, "spec", "nodeName"), str(raw, "spec", "vgName")
	if hostname == "" {
		hostname = node
	}
	return object{"apiVersion": "storage.k8s.io/v1", "kind": "StorageClass",
		"metadata":    object{"name": className(name), "labels": object{label + "pool": nodeLabel(name), label + "node": nodeLabel(node)}, "annotations": object{label + "pool-uid": str(raw, "metadata", "uid")}},
		"provisioner": "local.csi.openebs.io", "parameters": object{"storage": "lvm", "vgpattern": "^" + vg + "$", "fsType": "ext4", "thinProvision": "no"},
		"reclaimPolicy": "Retain", "volumeBindingMode": "WaitForFirstConsumer", "allowVolumeExpansion": true, "allowedTopologies": topology([]string{hostname})}
}
func fleetStorageClass(name string, pools []object) (object, error) {
	if _, err := model.Name(name); err != nil {
		return nil, err
	}
	if len(pools) == 0 {
		return nil, model.NewProblem(400, "共享存储类至少需要一个已审核的存储池")
	}
	owners := object{}
	vgs, hosts := map[string]bool{}, map[string]bool{}
	for _, pool := range pools {
		id := str(pool, "id")
		if _, present := owners[id]; present {
			return nil, model.NewProblem(400, "共享存储类不能重复引用同一个池")
		}
		if str(pool, "state") != "Ready" || str(pool, "uid") == "" || str(pool, "node") == "" {
			return nil, model.NewProblem(409, "共享存储类只允许引用具有 UID 的 Ready 池")
		}
		if !vgPattern.MatchString(str(pool, "vgName")) || str(pool, "topologyHostname") == "" {
			return nil, model.NewProblem(400, "VG 或节点 hostname 拓扑无效")
		}
		owners[id] = pool["uid"]
		vgs[str(pool, "vgName")] = true
		hosts[str(pool, "topologyHostname")] = true
	}
	vgNames, hostnames := sortedKeys(vgs), sortedKeys(hosts)
	refs, _ := json.Marshal(owners)
	return object{"apiVersion": "storage.k8s.io/v1", "kind": "StorageClass", "metadata": object{"name": name, "labels": object{label + "fleet": "true"}, "annotations": object{label + "pool-uids": string(refs)}},
		"provisioner": "local.csi.openebs.io", "parameters": object{"storage": "lvm", "vgpattern": "^(" + strings.Join(vgNames, "|") + ")$", "fsType": "ext4", "thinProvision": "no"},
		"reclaimPolicy": "Retain", "volumeBindingMode": "WaitForFirstConsumer", "allowVolumeExpansion": true, "allowedTopologies": topology(hostnames)}, nil
}

// FleetStorageClass renders the same immutable reviewed pool snapshot used by
// the observation verifier. It performs no Kubernetes writes or device IO.
func FleetStorageClass(name string, pools []model.Object) (*unstructured.Unstructured, error) {
	value, err := fleetStorageClass(name, pools)
	if err != nil {
		return nil, err
	}
	return &unstructured.Unstructured{Object: value}, nil
}
func sortedKeys(values map[string]bool) []string {
	result := make([]string, 0, len(values))
	for key := range values {
		result = append(result, key)
	}
	sort.Strings(result)
	return result
}
func claimsOverlap(a, b object) bool {
	if str(a, "nodeName") != str(b, "nodeName") {
		return false
	}
	if str(a, "vgName") == str(b, "vgName") {
		return true
	}
	for _, first := range array(a["devices"]) {
		for _, second := range array(b["devices"]) {
			for _, key := range []string{"path", "serial"} {
				if str(first, key) != "" && str(first, key) == str(second, key) {
					return true
				}
			}
		}
	}
	return false
}
func conflictingPools(pools []object) map[string]bool {
	seen := map[[3]string]string{}
	conflicts := map[string]bool{}
	for _, pool := range pools {
		spec := asObject(pool["spec"])
		name := str(pool, "metadata", "name")
		node := str(spec, "nodeName")
		identities := [][2]string{{"vg", str(spec, "vgName")}}
		for _, device := range array(spec["devices"]) {
			for _, key := range []string{"path", "serial"} {
				identities = append(identities, [2]string{key, str(device, key)})
			}
		}
		for _, identity := range identities {
			if identity[1] == "" {
				continue
			}
			key := [3]string{node, identity[0], identity[1]}
			if old, ok := seen[key]; ok && old != name {
				conflicts[old] = true
				conflicts[name] = true
			} else {
				seen[key] = name
			}
		}
	}
	return conflicts
}
func quantity(value any) int64 {
	parsed, err := resource.ParseQuantity(model.String(value))
	if err != nil {
		return 0
	}
	decimal := parsed.AsDec()
	number := new(big.Int).Set(decimal.UnscaledBig())
	scale := int64(decimal.Scale())
	if scale > 0 {
		number.Quo(number, new(big.Int).Exp(big.NewInt(10), big.NewInt(scale), nil))
	} else if scale < 0 {
		number.Mul(number, new(big.Int).Exp(big.NewInt(10), big.NewInt(-scale), nil))
	}
	if !number.IsInt64() || number.Sign() < 0 {
		return 0
	}
	return number.Int64()
}
func boundPool(pvc, pv object, members []object) (object, string) {
	if pv == nil {
		return object{}, "绑定的 PV 不存在或尚不可读取"
	}
	for _, key := range []string{"uid", "namespace", "name"} {
		if str(pv, "spec", "claimRef", key) != str(pvc, "metadata", key) {
			return object{}, "PV claimRef 与 PVC 身份不匹配"
		}
	}
	if str(pv, "spec", "csi", "driver") != "local.csi.openebs.io" || str(pv, "spec", "storageClassName") != str(pvc, "spec", "storageClassName") {
		return object{}, "PV 驱动或 StorageClass 与已审核绑定不匹配"
	}
	terms := array(at(pv, "spec", "nodeAffinity", "required", "nodeSelectorTerms"))
	matches := []object{}
	for _, pool := range members {
		if str(pv, "spec", "csi", "volumeAttributes", "openebs.io/volgroup") != str(pool, "vgName") {
			continue
		}
		valid := len(terms) > 0
		expected := map[string]string{"openebs.io/nodename": str(pool, "node"), "kubernetes.io/hostname": str(pool, "topologyHostname")}
		for _, term := range terms {
			constrained := false
			for _, expression := range array(at(term, "matchExpressions")) {
				if wanted, known := expected[str(expression, "key")]; known {
					constrained = true
					if str(expression, "operator") != "In" || !equal(at(expression, "values"), []string{wanted}) {
						valid = false
					}
				}
			}
			if !constrained {
				valid = false
			}
		}
		if valid {
			matches = append(matches, pool)
		}
	}
	if len(matches) != 1 {
		return object{}, "PV 的 VG/节点拓扑不能唯一对应已审核存储池"
	}
	return matches[0], ""
}
func deviceKey(node string, device object) string {
	return node + "\x00" + str(device, "path") + "\x00" + str(device, "serial") + "\x00" + jsonNumber(model.Int64(device["sizeBytes"]))
}
func jsonNumber(value int64) string { data, _ := json.Marshal(value); return string(data) }
func diskBase(path string) string   { return filepath.Base(path) }
