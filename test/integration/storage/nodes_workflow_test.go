package storage_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/SongOf/pulsar-operator/internal/storage/httpapi"
	"github.com/SongOf/pulsar-operator/internal/storage/model"
	"github.com/SongOf/pulsar-operator/internal/storage/store"
)

// Enrollment is observable through the same HTTP and persisted-state boundary
// as plan approval. This demo workflow never starts a process or accesses disks.
func TestNodeEnrollmentHTTPIdentityAndPlanWorkflow(t *testing.T) {
	root := t.TempDir()
	state, err := store.New(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = state.Close() }()
	if err := state.Seed(context.Background()); err != nil {
		t.Fatal(err)
	}
	const token = "storage-node-test-secret"
	handler := (&httpapi.Server{Mode: "demo", Token: token, Backend: state}).Handler()
	call := func(method, path string, body model.Object, authorized bool, want int) model.Object {
		t.Helper()
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(method, path, bytes.NewReader(raw))
		if authorized {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if method == http.MethodPost {
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Content-Length", strconv.Itoa(len(raw)))
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Fatalf("%s %s: got %d want %d: %s", method, path, rec.Code, want, rec.Body.String())
		}
		var out model.Object
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	call("GET", "/api/nodes/baremetal-024", nil, false, 401)
	detail := call("GET", "/api/nodes/baremetal-024?limit=2&offset=1", nil, true, 200)
	node := detail["node"].(map[string]any)
	disks := detail["disks"].(map[string]any)
	if node["enrolled"] != false || node["mode"] != "demo" || node["canEnableAgent"] != true || disks["total"] != float64(6) || len(disks["items"].([]any)) != 2 {
		t.Fatal(detail)
	}
	disk := disks["items"].([]any)[0].(map[string]any)
	if disk["canCreatePool"] != false {
		t.Fatal(disk)
	}
	planBody := model.Object{"name": "new-physical-node-pool", "node": "baremetal-024", "diskIds": []string{"disk-024-1"}}
	call("POST", "/api/pools/plan", planBody, true, 409)
	call("POST", "/api/nodes/baremetal-024/agent", model.Object{"uid": "stale", "resourceVersion": node["resourceVersion"]}, true, 409)
	call("POST", "/api/nodes/baremetal-024/agent", model.Object{"uid": node["uid"]}, true, 400)
	enabled := call("POST", "/api/nodes/baremetal-024/agent", model.Object{"uid": node["uid"], "resourceVersion": node["resourceVersion"]}, true, 200)
	if enabled["enrolled"] != true || enabled["resourceVersion"] == node["resourceVersion"] || enabled["mode"] != "demo" {
		t.Fatal(enabled)
	}
	call("POST", "/api/nodes/baremetal-024/agent", model.Object{"uid": node["uid"], "resourceVersion": node["resourceVersion"]}, true, 409)
	if err := state.Close(); err != nil {
		t.Fatal(err)
	}
	state, err = store.New(root)
	if err != nil {
		t.Fatal(err)
	}
	handler = (&httpapi.Server{Mode: "demo", Token: token, Backend: state}).Handler()
	detail = call("GET", "/api/nodes/baremetal-024?state=Available&limit=1", nil, true, 200)
	node = detail["node"].(map[string]any)
	if node["enrolled"] != true || node["canCreatePool"] != true {
		t.Fatal(node)
	}
	plan := call("POST", "/api/pools/plan", planBody, true, 200)
	pool := call("POST", "/api/pools", model.Object{"planId": plan["planId"], "confirmation": plan["confirmation"]}, true, 200)
	if pool["state"] != "AwaitingReview" || pool["canCreateVolume"] != false {
		t.Fatal(pool)
	}
	// A system disk remains protected even after enabling the physical node.
	call("POST", "/api/pools/plan", model.Object{"name": "system", "node": "baremetal-024", "diskIds": []string{"disk-024-0"}}, true, 409)
	call("GET", "/api/nodes/missing", nil, true, 404)
	call("POST", "/api/nodes/baremetal-024/agent/extra", model.Object{}, true, 404)
	call("GET", "/api/nodes/baremetal-024?limit=501", nil, true, 400)
}
