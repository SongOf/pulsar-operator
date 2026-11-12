package console

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	v1 "github.com/SongOf/pulsar-operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func mutationRequest(method, suffix, uid, version string) *http.Request {
	r := httptest.NewRequest(method, "/api/bookkeeperclusters/ns/bk"+suffix, strings.NewReader(`{"spec":{"replicas":5}}`))
	if uid != "" {
		r.Header.Set("X-Resource-UID", uid)
	}
	if version != "" {
		r.Header.Set("X-Resource-Version", version)
	}
	return r
}

var mutationMethods = []struct{ method, suffix string }{{"PATCH", ""}, {"DELETE", ""}, {"POST", "/restart?component=bookie"}}

func TestMutationRequiresReviewedIdentityAndVersion(t *testing.T) {
	for _, action := range mutationMethods {
		for _, tc := range []struct {
			name, uid, version string
			code               int
		}{
			{"missing both", "", "", 428}, {"missing UID", "", "10", 428}, {"missing version", "old", "", 428},
			{"different UID", "replacement", "10", 409}, {"different version", "old", "9", 409},
		} {
			t.Run(action.method+"/"+tc.name, func(t *testing.T) {
				obj := &v1.BookKeeperCluster{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "bk", UID: "old", ResourceVersion: "10"}}
				srv := inventoryServer(t, obj)
				w := httptest.NewRecorder()
				srv.Handler().ServeHTTP(w, mutationRequest(action.method, action.suffix, tc.uid, tc.version))
				if w.Code != tc.code {
					t.Fatalf("status %d: %s", w.Code, w.Body.String())
				}
				current := &v1.BookKeeperCluster{}
				if err := srv.Client.Get(context.Background(), client.ObjectKeyFromObject(obj), current); err != nil {
					t.Fatal(err)
				}
				if current.ResourceVersion != "10" || current.UID != "old" {
					t.Fatalf("resource changed: %+v", current.ObjectMeta)
				}
			})
		}
	}
}

// A real manager Get may return cached state after the live object has changed.
// Keep its snapshot intact while changing the API-side fake store.
type changedAfterRead struct {
	client.Client
	recreate bool
	changed  bool
}

func (c *changedAfterRead) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if err := c.Client.Get(ctx, key, obj, opts...); err != nil {
		return err
	}
	if c.changed {
		return nil
	}
	c.changed = true
	updated := obj.DeepCopyObject().(client.Object)
	updated.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: v1.GroupVersion.String(), Kind: "PulsarCluster", Name: "bk", UID: "parent", Controller: ptr.To(true)}})
	if c.recreate {
		if err := c.Client.Delete(ctx, obj.DeepCopyObject().(client.Object)); err != nil {
			return err
		}
		updated.SetUID("replacement")
		// A new object must not carry the old version. The fake store starts
		// newly created objects at 1; UID independently protects replacement.
		updated.SetResourceVersion("")
		return c.Client.Create(ctx, updated)
	}
	return c.Client.Update(ctx, updated)
}

func TestMutationsNeverChangeRecreatedOrNewlyOwnedResources(t *testing.T) {
	for _, recreate := range []bool{true, false} {
		for _, action := range mutationMethods {
			name := "owner changed"
			if recreate {
				name = "same-name replacement"
			}
			t.Run(action.method+"/"+name, func(t *testing.T) {
				obj := &v1.BookKeeperCluster{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "bk", UID: "old", ResourceVersion: "10"},
					Spec: v1.BookKeeperClusterSpec{BookKeeperSpec: v1.BookKeeperSpec{Replicas: 3}}}
				srv := inventoryServer(t, obj)
				backend := srv.Client
				srv.Client = &changedAfterRead{Client: backend, recreate: recreate}
				w := httptest.NewRecorder()
				srv.Handler().ServeHTTP(w, mutationRequest(action.method, action.suffix, "old", "10"))
				if w.Code != http.StatusConflict {
					t.Fatalf("status %d: %s", w.Code, w.Body.String())
				}
				current := &v1.BookKeeperCluster{}
				if err := backend.Get(context.Background(), client.ObjectKeyFromObject(obj), current); err != nil {
					t.Fatalf("replacement removed: %v", err)
				}
				if current.Spec.Replicas != 3 || current.Spec.RestartedAt != "" || len(current.OwnerReferences) != 1 {
					t.Fatalf("replacement changed: %+v", current)
				}
				if recreate && current.UID != "replacement" {
					t.Fatalf("replacement UID changed: %s", current.UID)
				}
			})
		}
	}
}

func TestDeleteReviewedStandaloneResource(t *testing.T) {
	obj := &v1.BookKeeperCluster{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "bk", UID: "old", ResourceVersion: "10"}}
	srv := inventoryServer(t, obj)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, mutationRequest("DELETE", "", "old", "10"))
	if w.Code != http.StatusNoContent {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
}

func TestMutationRejectsDuplicatePreconditionHeaders(t *testing.T) {
	obj := &v1.BookKeeperCluster{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "bk", UID: "old", ResourceVersion: "10"}}
	for _, header := range []string{"X-Resource-UID", "X-Resource-Version"} {
		srv := inventoryServer(t, obj)
		r := mutationRequest("PATCH", "", "old", "10")
		r.Header.Add(header, "other")
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, r)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status %d: %s", w.Code, w.Body.String())
		}
	}
}
