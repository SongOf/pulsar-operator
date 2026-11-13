package cluster

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SongOf/pulsar-operator/internal/storage/model"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

// Exercise client-go's actual wire format against an HTTP API, including
// pagination, merge-patch resourceVersion and DeleteOptions preconditions. The
// server delegates state to the fake tracker; no user Kubernetes cluster is used.
func TestHTTPPaginationMutationPreconditionsAndLostDeleteRecovery(t *testing.T) {
	f := newFixture(t)
	f.boundVolume(true)
	var pages, patches, deletes atomic.Int32
	writeError := func(w http.ResponseWriter, err error) {
		if status, ok := err.(apierrors.APIStatus); ok {
			body := status.Status()
			body.APIVersion, body.Kind = "v1", "Status"
			w.WriteHeader(int(body.Code))
			_ = json.NewEncoder(w).Encode(body)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(object{"kind": "Status", "apiVersion": "v1", "status": "Failure", "message": err.Error(), "code": 500})
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		parts := strings.Split(strings.Trim(req.URL.Path, "/"), "/")
		var resource schema.GroupVersionResource
		namespace, name := "", ""
		switch {
		case len(parts) >= 4 && parts[0] == "apis":
			resource = schema.GroupVersionResource{Group: parts[1], Version: parts[2], Resource: parts[3]}
			if len(parts) > 4 {
				name = parts[4]
			}
		case len(parts) >= 5 && parts[0] == "api" && parts[2] == "namespaces":
			resource = schema.GroupVersionResource{Version: parts[1], Resource: parts[4]}
			namespace = parts[3]
			if len(parts) > 5 {
				name = parts[5]
			}
		case len(parts) >= 3 && parts[0] == "api":
			resource = schema.GroupVersionResource{Version: parts[1], Resource: parts[2]}
			if len(parts) > 3 {
				name = parts[3]
			}
		default:
			http.NotFound(w, req)
			return
		}
		client := f.client.Resource(resource).Namespace(namespace)
		switch req.Method {
		case http.MethodGet:
			if name != "" {
				value, err := client.Get(req.Context(), name, metav1.GetOptions{})
				if err != nil {
					writeError(w, err)
					return
				}
				_ = json.NewEncoder(w).Encode(value)
				return
			}
			if req.URL.Query().Get("limit") != "500" {
				t.Errorf("missing bounded API list page size: %s", req.URL)
			}
			values, err := client.List(req.Context(), metav1.ListOptions{LabelSelector: req.URL.Query().Get("labelSelector")})
			if err != nil {
				writeError(w, err)
				return
			}
			if resource == poolsResource {
				pages.Add(1)
				if req.URL.Query().Get("continue") == "" {
					values.SetContinue("pool-page-2")
				} else {
					if req.URL.Query().Get("continue") != "pool-page-2" {
						t.Error("incorrect pagination cursor")
					}
					values.Items = nil
					values.SetContinue("")
				}
			}
			_ = json.NewEncoder(w).Encode(values)
		case http.MethodPatch:
			patches.Add(1)
			body, err := io.ReadAll(req.Body)
			if err != nil {
				t.Error(err)
				return
			}
			var decoded object
			if err := json.Unmarshal(body, &decoded); err != nil {
				t.Error(err)
				return
			}
			if req.Header.Get("Content-Type") != string(types.MergePatchType) || str(decoded, "metadata", "resourceVersion") != "17" {
				t.Errorf("unsafe expansion patch: %s", body)
			}
			value, err := client.Patch(req.Context(), name, types.MergePatchType, body, metav1.PatchOptions{})
			if err != nil {
				writeError(w, err)
				return
			}
			_ = json.NewEncoder(w).Encode(value)
		case http.MethodDelete:
			deletes.Add(1)
			var options metav1.DeleteOptions
			if err := json.NewDecoder(req.Body).Decode(&options); err != nil {
				t.Error(err)
				return
			}
			if options.Preconditions == nil || options.Preconditions.UID == nil || options.Preconditions.ResourceVersion == nil || string(*options.Preconditions.UID) != "pvc-uid" || *options.Preconditions.ResourceVersion != "17" {
				t.Errorf("unsafe DELETE options: %+v", options)
			}
			if err := client.Delete(req.Context(), name, options); err != nil {
				writeError(w, err)
				return
			}
			_ = json.NewEncoder(w).Encode(metav1.Status{Status: "Success"})
		default:
			t.Errorf("unexpected HTTP method %s", req.Method)
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()
	client, err := dynamic.NewForConfig(&rest.Config{Host: server.URL, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	backend := New(f.store, client)
	if _, err := backend.Expand(context.Background(), "default~data", object{"sizeGiB": int64(2)}); err != nil {
		t.Fatal(err)
	}
	f.deleteMode = "lost"
	if _, err := backend.Release(context.Background(), "default~data", "pvc-uid"); err == nil {
		t.Fatal("expected error after committed remote DELETE")
	}
	if f.count("release-intents") != 1 {
		t.Fatal("lost HTTP response erased durable intent")
	}
	recovered, err := backend.Release(context.Background(), "default~data", "pvc-uid")
	if err != nil || str(recovered, "status") != "Released" || model.Int64(recovered["sizeBytes"]) != 2*model.GiB {
		t.Fatal(recovered, err)
	}
	if pages.Load() < 2 || patches.Load() != 1 || deletes.Load() != 1 {
		t.Fatalf("pages=%d patches=%d deletes=%d", pages.Load(), patches.Load(), deletes.Load())
	}
}
