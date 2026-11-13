package audit

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	v1 "github.com/SongOf/pulsar-operator/api/pulsar/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func auditedFixture(t *testing.T) (*Client, *v1.PulsarCluster) {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = v1.AddToScheme(scheme)
	pc := &v1.PulsarCluster{ObjectMeta: metav1.ObjectMeta{Name: "pc", Namespace: "ns", UID: "root-uid", Generation: 1}, Spec: v1.PulsarClusterSpec{Broker: v1.BrokerSpec{Replicas: 2}}}
	raw := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v1.PulsarCluster{}).WithObjects(pc).Build()
	if err := raw.Get(context.Background(), client.ObjectKeyFromObject(pc), pc); err != nil {
		t.Fatal(err)
	}
	return &Client{Client: raw, Reader: raw, Store: openTestStore(t)}, pc
}

type observingClient struct {
	client.Client
	store  *Store
	t      *testing.T
	reject bool
}

func (c *observingClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	page, err := c.store.List(ctx, Query{})
	if err != nil || len(page.Items) == 0 || page.Items[0].Result != "Started" {
		c.t.Fatalf("write happened without durable intent: %#v %v", page, err)
	}
	if c.reject {
		return apierrors.NewConflict(schema.GroupResource{Group: "pulsar.songof.dev", Resource: "pulsarclusters"}, obj.GetName(), errors.New("PASSWORD_SECRET"))
	}
	return c.Client.Update(ctx, obj, opts...)
}

func TestClientRecordsIntentOutcomeAndPreventsUnauditedWrites(t *testing.T) {
	c, pc := auditedFixture(t)
	c.Client = &observingClient{Client: c.Client, store: c.Store, t: t}
	ctx := ControllerContext(context.Background(), "pulsar", pc)
	pc.Spec.Broker.Replicas = 3
	if err := c.Update(ctx, pc); err != nil {
		t.Fatal(err)
	}
	page, _ := c.Store.List(ctx, Query{})
	if len(page.Items) != 1 || page.Items[0].Result != "Succeeded" || page.Items[0].Resource.UID != "root-uid" {
		t.Fatalf("missing outcome: %#v", page)
	}
	if err := c.Update(ctx, pc); err != nil {
		t.Fatal(err)
	}
	page, _ = c.Store.List(ctx, Query{})
	if len(page.Items) != 1 {
		t.Fatal("unchanged reconcile was audited")
	}
	_ = c.Store.Close()
	pc.Spec.Broker.Replicas = 4
	if err := c.Update(ctx, pc); err == nil {
		t.Fatal("write succeeded without audit storage")
	}
	current := &v1.PulsarCluster{}
	_ = c.Reader.Get(ctx, client.ObjectKeyFromObject(pc), current)
	if current.Spec.Broker.Replicas != 3 {
		t.Fatal("mutated Kubernetes after audit failed")
	}
}

func TestClientRedactsFailureAndRecordsCriticalStatusOnly(t *testing.T) {
	c, pc := auditedFixture(t)
	raw := c.Client
	c.Client = &observingClient{Client: raw, store: c.Store, t: t, reject: true}
	ctx := ControllerContext(context.Background(), "pulsar", pc)
	pc.Spec.Broker.Replicas = 3
	if err := c.Update(ctx, pc); !apierrors.IsConflict(err) {
		t.Fatalf("lost API conflict: %v", err)
	}
	page, _ := c.Store.List(ctx, Query{})
	encoded, _ := json.Marshal(page)
	if strings.Contains(string(encoded), "PASSWORD_SECRET") || page.Items[0].Result != "Failed" {
		t.Fatalf("unsafe failure record: %s", encoded)
	}
	c.Client = raw
	_ = raw.Get(ctx, client.ObjectKeyFromObject(pc), pc)
	pc.Status.Phase = v1.PhaseRunning
	if err := c.Status().Update(ctx, pc); err != nil {
		t.Fatal(err)
	}
	pc.Status.ObservedGeneration++
	if err := c.Status().Update(ctx, pc); err != nil {
		t.Fatal(err)
	}
	page, _ = c.Store.List(ctx, Query{})
	if len(page.Items) != 2 || page.Items[0].Action != "status.update" {
		t.Fatalf("incorrect status audit: %#v", page)
	}
}

func TestOperationGenerationDoesNotMisattributeLaterEdits(t *testing.T) {
	_, pc := auditedFixture(t)
	ctx := RequestContext(context.Background(), "ADMIN_SECRET", "127.0.0.1:1234")
	Stamp(ctx, pc, 2)
	pc.Generation = 2
	controller := FromContext(ControllerContext(ctx, "pulsar", pc))
	if controller.OperationID != FromContext(ctx).OperationID || controller.Source != "controller" {
		t.Fatal("lost request correlation")
	}
	pc.Generation = 3
	controller = FromContext(ControllerContext(ctx, "pulsar", pc))
	if controller.OperationID == FromContext(ctx).OperationID {
		t.Fatal("attributed kubectl edit to an earlier API request")
	}
	if strings.Contains(FromContext(ctx).Actor, "ADMIN_SECRET") {
		t.Fatal("actor contains credential")
	}
}
