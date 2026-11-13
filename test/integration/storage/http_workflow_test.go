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
	storageweb "github.com/SongOf/pulsar-operator/web/storage"
)

// Exercise the public HTTP decoder, actual SQLite transactions and restart as
// one workflow. No socket, Kubernetes cluster or physical device is used.
func TestDemoHTTPWorkflowPreservesReleasedDataAcrossRestart(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	state, err := store.New(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = state.Close() }()
	if err := state.Seed(ctx); err != nil {
		t.Fatal(err)
	}
	const token = "integration-test-token-24-characters"
	var handler http.Handler
	resetHandler := func() {
		handler = (&httpapi.Server{Mode: "demo", Token: token, Backend: state, Static: storageweb.FS}).Handler()
	}
	resetHandler()
	call := func(method, path string, body model.Object) model.Object {
		t.Helper()
		var raw []byte
		if body != nil {
			raw, err = json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
		}
		req := httptest.NewRequest(method, path, bytes.NewReader(raw))
		req.Header.Set("Authorization", "Bearer "+token)
		if method == "POST" {
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Content-Length", strconv.Itoa(len(raw)))
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		if response.Code != 200 {
			t.Fatalf("%s %s: %d %s", method, path, response.Code, response.Body.String())
		}
		var value model.Object
		decoder := json.NewDecoder(response.Body)
		decoder.UseNumber()
		if err := decoder.Decode(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	plan := call("POST", "/api/pools/plan", model.Object{"name": "workflow-pool", "node": "baremetal-007", "diskIds": []string{"disk-007-1"}})
	pool := call("POST", "/api/pools", model.Object{"planId": plan["planId"], "confirmation": plan["confirmation"]})
	volume := call("POST", "/api/volumes", model.Object{"name": "workflow-data", "namespace": "default", "poolId": pool["id"], "sizeGiB": 4})
	path := "/api/volumes/" + model.String(volume["id"])
	expanded := call("POST", path+"/expand", model.Object{"sizeGiB": 6})
	if model.Int64(expanded["sizeBytes"]) != 6*model.GiB {
		t.Fatalf("incorrect capacity: %#v", expanded)
	}
	released := call("DELETE", path, nil)
	if released["status"] != "Released" {
		t.Fatalf("incorrect release: %#v", released)
	}
	if err := state.Close(); err != nil {
		t.Fatal(err)
	}
	state, err = store.New(directory)
	if err != nil {
		t.Fatal(err)
	}
	resetHandler()
	page := call("GET", "/api/volumes?q=workflow-data", nil)
	items := page["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["status"] != "Released" {
		t.Fatalf("lost release history: %#v", page)
	}
	page = call("GET", "/api/pools?q=workflow-pool", nil)
	retainedPool := page["items"].([]any)[0].(map[string]any)
	if model.Int64(retainedPool["allocatedBytes"]) != 6*model.GiB {
		t.Fatalf("Retain unexpectedly freed capacity: %#v", retainedPool)
	}
}
