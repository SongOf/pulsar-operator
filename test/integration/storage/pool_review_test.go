package storage_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/SongOf/pulsar-operator/internal/storage/httpapi"
	"github.com/SongOf/pulsar-operator/internal/storage/model"
	"github.com/SongOf/pulsar-operator/internal/storage/store"
)

func TestPoolReviewHTTPRequiresExactResultAndPersistsDecision(t *testing.T) {
	root := t.TempDir()
	state, err := store.New(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = state.Close() }()
	if err = state.Seed(context.Background()); err != nil {
		t.Fatal(err)
	}
	handler := (&httpapi.Server{Mode: "demo", Token: "review-token", Backend: state}).Handler()
	call := func(method, path string, body model.Object, want int) model.Object {
		t.Helper()
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(method, path, bytes.NewReader(data))
		req.Header.Set("Authorization", "Bearer review-token")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Content-Length", strconv.Itoa(len(data)))
		out := httptest.NewRecorder()
		handler.ServeHTTP(out, req)
		if out.Code != want {
			t.Fatalf("%s %s: %d %s", method, path, out.Code, out.Body.String())
		}
		var result model.Object
		if err = json.Unmarshal(out.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	plan := call("POST", "/api/pools/plan", model.Object{"name": "review-http", "node": "baremetal-007", "diskIds": []string{"disk-007-1"}}, 200)
	pool := call("POST", "/api/pools", model.Object{"planId": plan["planId"], "confirmation": plan["confirmation"]}, 200)
	path := "/api/pools/" + model.String(pool["id"])
	pool = call("GET", path, nil, 200)
	if pool["state"] != "AwaitingReview" || pool["canReview"] != true || pool["canCreateVolume"] != false || pool["mode"] != "demo" {
		t.Fatal(pool)
	}
	volume := model.Object{"name": "review-data", "poolId": pool["id"], "sizeGiB": 1}
	call("POST", "/api/volumes", volume, 409)
	benchmark := pool["benchmark"].(map[string]any)
	if !strings.Contains(model.String(benchmark["message"]), "模拟") {
		t.Fatal("demo measurement presented as real")
	}
	review := model.Object{"uid": pool["uid"], "resourceVersion": pool["resourceVersion"], "runID": benchmark["runID"], "resultDigest": benchmark["resultDigest"], "decision": "Rejected"}
	call("POST", path+"/review", review, 400)
	review["note"] = strings.Repeat("字", 1025)
	call("POST", path+"/review", review, 400)
	review["note"] = "延迟未符合本业务要求"
	rejected := call("POST", path+"/review", review, 200)
	if rejected["allocationState"] != "Rejected" || rejected["canReview"] != true || rejected["canCreateVolume"] != false {
		t.Fatal(rejected)
	}
	review["decision"] = "Approved"
	call("POST", path+"/review", review, 409)
	review["resourceVersion"] = rejected["resourceVersion"]
	approved := call("POST", path+"/review", review, 200)
	if approved["canReview"] != false || approved["canCreateVolume"] != true || approved["state"] != "Ready" {
		t.Fatal(approved)
	}
	call("POST", "/api/volumes", volume, 200)
	review["resourceVersion"] = approved["resourceVersion"]
	review["decision"] = "Rejected"
	call("POST", path+"/review", review, 409)
	if err = state.Close(); err != nil {
		t.Fatal(err)
	}
	state, err = store.New(root)
	if err != nil {
		t.Fatal(err)
	}
	handler = (&httpapi.Server{Mode: "demo", Token: "review-token", Backend: state}).Handler()
	reopened := call("GET", path, nil, 200)
	if reopened["canCreateVolume"] != true || reopened["allocationState"] != "Approved" || reopened["allocationApproval"].(map[string]any)["reviewedAt"] == "" {
		t.Fatal(reopened)
	}
}
