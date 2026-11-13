package audit

import (
	"bytes"
	"encoding/json"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

// Diff stores useful operational values but never stores arbitrary configuration,
// annotations, commands, env values, or ConfigMap data. Paths still identify the
// fields that changed. The same policy is used for both intent and result.
func Diff(before, after []byte, statusOnly bool) ([]Change, bool) {
	left, right := flatten(before, statusOnly), flatten(after, statusOnly)
	keys := map[string]bool{}
	for key := range left {
		keys[key] = true
	}
	for key := range right {
		keys[key] = true
	}
	paths := make([]string, 0, len(keys))
	for key := range keys {
		paths = append(paths, key)
	}
	sort.Strings(paths)
	changes := []Change{}
	for _, path := range paths {
		old, oldOK := left[path]
		value, newOK := right[path]
		if oldOK == newOK && reflect.DeepEqual(old, value) {
			continue
		}
		if len(changes) == 100 {
			return changes, true
		}
		change := Change{Path: path}
		if len(path) > 256 {
			change.Path = path[:256] + "…"
		}
		if safePath(path) {
			change.Before, change.After = bounded(old), bounded(value)
		} else {
			change.Redacted = true
			if oldOK {
				change.Before = "[redacted]"
			}
			if newOK {
				change.After = "[redacted]"
			}
		}
		changes = append(changes, change)
	}
	return changes, false
}

func bounded(value any) any {
	if text, ok := value.(string); ok && len(text) > 256 {
		return text[:256] + "…"
	}
	return value
}

func safePath(path string) bool {
	for _, segment := range []string{"/config/", "/data/", "/binaryData/", "/annotations/", "/env/", "/command/", "/args/"} {
		if strings.Contains(path, segment) {
			return false
		}
	}
	last := path[strings.LastIndex(path, "/")+1:]
	switch last {
	case "image", "replicas", "minReplicas", "size", "storageClassName", "reclaimPolicy", "enabled", "restartedAt", "paused":
		return strings.HasPrefix(path, "/spec/")
	case "storage":
		return strings.HasSuffix(path, "/resources/requests/storage")
	case "phase", "type", "status", "reason":
		return strings.HasPrefix(path, "/status/")
	case "bookieID", "ordinal", "job":
		return strings.HasPrefix(path, "/status/decommission/")
	}
	return false
}

func flatten(data []byte, statusOnly bool) map[string]any {
	result := map[string]any{}
	if len(data) == 0 {
		return result
	}
	var obj map[string]any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if decoder.Decode(&obj) != nil {
		return result
	}
	if statusOnly {
		status, _ := obj["status"].(map[string]any)
		critical := map[string]any{}
		for _, key := range []string{"phase", "decommission"} {
			if value, ok := status[key]; ok {
				critical[key] = value
			}
		}
		// Conditions contain critical readiness/rejection transitions, but their
		// timestamps and observedGeneration alone do not represent new actions.
		conditions, _ := status["conditions"].([]any)
		for _, value := range conditions {
			condition, _ := value.(map[string]any)
			kind, _ := condition["type"].(string)
			critical[kind] = map[string]any{"status": condition["status"], "reason": condition["reason"]}
		}
		obj = map[string]any{"status": critical}
	} else {
		delete(obj, "status")
		delete(obj, "apiVersion")
		delete(obj, "kind")
		if metadata, ok := obj["metadata"].(map[string]any); ok {
			for _, key := range []string{"managedFields", "resourceVersion", "generation", "creationTimestamp", "uid", "name", "namespace"} {
				delete(metadata, key)
			}
			if annotations, ok := metadata["annotations"].(map[string]any); ok {
				delete(annotations, OperationAnnotation)
				delete(annotations, GenerationAnnotation)
			}
		}
	}
	var walk func(string, any)
	walk = func(path string, value any) {
		switch value := value.(type) {
		case map[string]any:
			for key, item := range value {
				key = strings.ReplaceAll(strings.ReplaceAll(key, "~", "~0"), "/", "~1")
				walk(path+"/"+key, item)
			}
		case []any:
			for i, item := range value {
				walk(path+"/"+strconv.Itoa(i), item)
			}
		default:
			result[path] = value
		}
	}
	walk("", obj)
	return result
}
