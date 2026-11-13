package httpapi

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	v1 "github.com/SongOf/pulsar-operator/api/pulsar/v1alpha1"
	"github.com/SongOf/pulsar-operator/internal/pulsar/audit"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestAuditRequestsMutationsQueriesAndDeletedResources(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	_ = v1.AddToScheme(scheme)
	pc := &v1.PulsarCluster{ObjectMeta: metav1.ObjectMeta{Name: "pc", Namespace: "ns", UID: "original-uid", Generation: 1}, Spec: v1.PulsarClusterSpec{Broker: v1.BrokerSpec{Replicas: 2}}}
	raw := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pc).Build()
	store, err := audit.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	const token = "ADMIN_CREDENTIAL_SECRET"
	server := &Server{Client: &audit.Client{Client: raw, Reader: raw, Store: store}, Audit: store, Token: token}
	handler := server.Handler()
	call := func(method, path, body string, authorized, stale bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if authorized {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if method == "PATCH" || method == "DELETE" || strings.Contains(path, "/restart") {
			current := &v1.PulsarCluster{}
			if err := raw.Get(ctx, client.ObjectKeyFromObject(pc), current); err != nil {
				t.Fatal(err)
			}
			req.Header.Set("X-Resource-UID", string(current.UID))
			req.Header.Set("X-Resource-Version", current.ResourceVersion)
			if stale {
				req.Header.Set("X-Resource-Version", "stale")
			}
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		return response
	}
	patch := call("PATCH", "/api/pulsarclusters/ns/pc", `{"spec":{"broker":{"replicas":3},"bookkeeper":{"config":{"authenticationParameters":"CONFIG_SECRET"}}}}`, true, false)
	if patch.Code != 200 || patch.Header().Get("X-Operation-ID") == "" {
		t.Fatalf("patch: %d %s", patch.Code, patch.Body)
	}
	page, err := store.List(ctx, audit.Query{OperationID: patch.Header().Get("X-Operation-ID")})
	if err != nil || len(page.Items) != 2 {
		t.Fatalf("request and mutation are not linked: %#v %v", page, err)
	}
	for _, record := range page.Items {
		if record.Result != "Succeeded" || record.Resource.UID != "original-uid" {
			t.Fatalf("wrong outcome: %#v", record)
		}
	}
	stale := call("PATCH", "/api/pulsarclusters/ns/pc", `{"spec":{"broker":{"replicas":9}}}`, true, true)
	if stale.Code != 409 {
		t.Fatal(stale.Code)
	}
	rejected, _ := store.List(ctx, audit.Query{OperationID: stale.Header().Get("X-Operation-ID")})
	if len(rejected.Items) != 1 || rejected.Items[0].Result != "Failed" {
		t.Fatalf("precondition rejection was not audited: %#v", rejected)
	}
	created := call("POST", "/api/bookkeeperclusters", `{"metadata":{"name":"new-bk","namespace":"ns"}}`, true, false)
	if created.Code != 201 {
		t.Fatalf("create: %d %s", created.Code, created.Body)
	}
	createPage, _ := store.List(ctx, audit.Query{OperationID: created.Header().Get("X-Operation-ID")})
	if len(createPage.Items) != 2 {
		t.Fatalf("create not audited: %#v", createPage)
	}
	restart := call("POST", "/api/pulsarclusters/ns/pc/restart?component=broker", "", true, false)
	if restart.Code != 200 {
		t.Fatalf("restart: %d %s", restart.Code, restart.Body)
	}
	deleted := call("DELETE", "/api/pulsarclusters/ns/pc", "", true, false)
	if deleted.Code != 204 {
		t.Fatalf("delete: %d %s", deleted.Code, deleted.Body)
	}
	if call("GET", "/api/audit", "", false, false).Code != 401 {
		t.Fatal("audit endpoint is unauthenticated")
	}
	query := call("GET", "/api/audit?uid=original-uid", "", true, false)
	if query.Code != 200 || !strings.Contains(query.Body.String(), "request.delete") || strings.Contains(query.Body.String(), "CONFIG_SECRET") || strings.Contains(query.Body.String(), token) {
		t.Fatalf("unsafe or missing audit: %s", query.Body)
	}
	for _, path := range []string{"/api/audit?limit=0", "/api/audit?cursor=-1", "/api/audit?limit=201"} {
		if call("GET", path, "", true, false).Code != 400 {
			t.Fatal("invalid audit pagination accepted")
		}
	}
	_ = store.Close()
	blocked := call("POST", "/api/pulsarclusters", `{"metadata":{"name":"blocked","namespace":"ns"}}`, true, false)
	if blocked.Code != 503 {
		t.Fatal("missing audit did not block mutation")
	}
	var unexpected v1.PulsarCluster
	if err := raw.Get(ctx, client.ObjectKey{Namespace: "ns", Name: "blocked"}, &unexpected); err == nil {
		t.Fatal("created resource without audit intent")
	}
	encoded, _ := json.Marshal(page)
	if strings.Contains(string(encoded), "CONFIG_SECRET") {
		t.Fatal("stored secret")
	}
}
