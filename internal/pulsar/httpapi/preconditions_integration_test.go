//go:build integration

package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	v1 "github.com/SongOf/pulsar-operator/api/pulsar/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

type apiServerRaceClient struct {
	client.Client
	recreate bool
	changed  bool
}

func (c *apiServerRaceClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if err := c.Client.Get(ctx, key, obj, opts...); err != nil {
		return err
	}
	if c.changed {
		return nil
	}
	c.changed = true
	updated := obj.DeepCopyObject().(client.Object)
	updated.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: v1.GroupVersion.String(), Kind: "PulsarCluster", Name: obj.GetName(), UID: "parent-uid", Controller: ptr.To(true)}})
	if c.recreate {
		if err := c.Client.Delete(ctx, obj.DeepCopyObject().(client.Object)); err != nil {
			return err
		}
		updated.SetUID("")
		updated.SetResourceVersion("")
		updated.SetCreationTimestamp(metav1.Time{})
		updated.SetManagedFields(nil)
		return c.Client.Create(ctx, updated)
	}
	return c.Client.Update(ctx, updated)
}

// This supplements the deterministic fake tests with actual apiserver JSON
// Patch tests and DeleteOptions preconditions. It never uses a user cluster.
func TestIntegrationMutationPreconditionsAgainstAPIServer(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Fatal("integration tests require KUBEBUILDER_ASSETS; run the module integration script")
	}
	useExisting := false
	env := &envtest.Environment{UseExistingCluster: &useExisting, CRDDirectoryPaths: []string{filepath.Join("..", "..", "..", "config", "pulsar", "crd")}, ErrorIfCRDPathMissing: true}
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Error(err)
		}
	})
	scheme := runtime.NewScheme()
	if err := v1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	backend, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := backend.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "console-preconditions"}}); err != nil {
		t.Fatal(err)
	}
	index := 0
	for _, recreate := range []bool{true, false} {
		for _, action := range mutationMethods {
			index++
			name := fmt.Sprintf("bk-%d", index)
			t.Run(fmt.Sprintf("%s/recreate=%v", action.method, recreate), func(t *testing.T) {
				obj := &v1.BookKeeperCluster{ObjectMeta: metav1.ObjectMeta{Namespace: "console-preconditions", Name: name},
					Spec: v1.BookKeeperClusterSpec{ZooKeeper: "zookeeper:2181", BookKeeperSpec: v1.BookKeeperSpec{Replicas: 3,
						Storage: v1.BookKeeperStorage{Journal: v1.VolumeSpec{Size: resource.MustParse("1Gi")}, Ledger: v1.VolumeSpec{Size: resource.MustParse("1Gi")}}}}}
				if err := backend.Create(ctx, obj); err != nil {
					t.Fatal(err)
				}
				uid, version := obj.UID, obj.ResourceVersion
				srv := &Server{Client: &apiServerRaceClient{Client: backend, recreate: recreate}}
				r := httptest.NewRequest(action.method, "/api/bookkeeperclusters/console-preconditions/"+name+action.suffix, strings.NewReader(`{"spec":{"replicas":5}}`))
				r.Header.Set("X-Resource-UID", string(uid))
				r.Header.Set("X-Resource-Version", version)
				w := httptest.NewRecorder()
				srv.Handler().ServeHTTP(w, r)
				if w.Code != http.StatusConflict {
					t.Fatalf("status %d: %s", w.Code, w.Body.String())
				}
				current := &v1.BookKeeperCluster{}
				if err := backend.Get(ctx, client.ObjectKeyFromObject(obj), current); err != nil {
					t.Fatalf("resource removed: %v", err)
				}
				if current.Spec.Replicas != 3 || current.Spec.RestartedAt != "" || len(current.OwnerReferences) != 1 {
					t.Fatalf("resource changed: %+v", current)
				}
				if recreate && current.UID == uid {
					t.Fatal("test did not recreate the object")
				}
			})
		}
	}
	t.Run("valid update and schema rejection remain distinct", func(t *testing.T) {
		obj := &v1.BookKeeperCluster{ObjectMeta: metav1.ObjectMeta{Namespace: "console-preconditions", Name: "ordinary"},
			Spec: v1.BookKeeperClusterSpec{ZooKeeper: "zookeeper:2181", BookKeeperSpec: v1.BookKeeperSpec{Replicas: 3,
				Storage: v1.BookKeeperStorage{Journal: v1.VolumeSpec{Size: resource.MustParse("1Gi")}, Ledger: v1.VolumeSpec{Size: resource.MustParse("1Gi")}}}}}
		if err := backend.Create(ctx, obj); err != nil {
			t.Fatal(err)
		}
		srv := &Server{Client: backend}
		request := func(body string) *httptest.ResponseRecorder {
			r := httptest.NewRequest("PATCH", "/api/bookkeeperclusters/console-preconditions/ordinary", strings.NewReader(body))
			r.Header.Set("X-Resource-UID", string(obj.UID))
			r.Header.Set("X-Resource-Version", obj.ResourceVersion)
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, r)
			return w
		}
		if w := request(`{"spec":{"replicas":4}}`); w.Code != http.StatusOK {
			t.Fatalf("valid patch: %d: %s", w.Code, w.Body.String())
		}
		if err := backend.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
			t.Fatal(err)
		}
		if obj.Spec.Replicas != 4 {
			t.Fatalf("valid patch not applied: %d", obj.Spec.Replicas)
		}
		if w := request(`{"spec":{"replicas":-1}}`); w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("schema violation should retain field details: %d: %s", w.Code, w.Body.String())
		}
	})
}
