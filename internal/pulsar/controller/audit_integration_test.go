//go:build integration

package controller

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	v1 "github.com/SongOf/pulsar-operator/api/pulsar/v1alpha1"
	"github.com/SongOf/pulsar-operator/internal/pulsar/audit"
	"github.com/SongOf/pulsar-operator/internal/pulsar/httpapi"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
)

func TestIntegrationAuditConnectsUserRequestAndReconcile(t *testing.T) {
	ctx := context.Background()
	ns := namespace(t)
	newPC(t, ns)
	pc := get(t, ns, "pc", &v1.PulsarCluster{})
	pc.ObjectMeta = metav1.ObjectMeta{Name: "audited", Namespace: ns}
	store, err := audit.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	c := &audit.Client{Client: k8s, Reader: k8s, Store: store}
	body, _ := json.Marshal(pc)
	response := httptest.NewRecorder()
	(&httpapi.Server{Client: c, Audit: store}).Handler().ServeHTTP(response, httptest.NewRequest("POST", "/api/pulsarclusters", strings.NewReader(string(body))))
	if response.Code != 201 {
		t.Fatalf("create: %d %s", response.Code, response.Body)
	}
	operation := response.Header().Get("X-Operation-ID")
	r := &PulsarReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(100)}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "audited"}}
	if _, err := r.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	page, err := store.List(ctx, audit.Query{OperationID: operation, Limit: 200})
	if err != nil {
		t.Fatal(err)
	}
	foundRequest, foundService, foundStatus := false, false, false
	for _, entry := range page.Items {
		foundRequest = foundRequest || entry.Action == "request.create"
		foundService = foundService || entry.Source == "controller" && entry.Action == "resource.create" && entry.Resource.Kind == "Service"
		foundStatus = foundStatus || entry.Action == "status.update"
		if entry.Result != "Succeeded" {
			t.Fatalf("audit outcome: %#v", entry)
		}
	}
	if !foundRequest || !foundService || !foundStatus {
		t.Fatalf("incomplete trace: %#v", page)
	}
	if _, err := r.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	again, _ := store.List(ctx, audit.Query{OperationID: operation, Limit: 200})
	if len(again.Items) != len(page.Items) {
		t.Fatalf("unchanged reconcile added audit noise: %d -> %d", len(page.Items), len(again.Items))
	}
}
