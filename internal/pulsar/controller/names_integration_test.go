//go:build integration

package controller

import (
	"context"
	"strings"
	"testing"

	v1 "github.com/SongOf/pulsar-operator/api/pulsar/v1alpha1"
	"github.com/SongOf/pulsar-operator/internal/pulsar/resources"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestIntegrationClusterNameAdmission(t *testing.T) {
	ctx := context.Background()
	ns := namespace(t)
	newPC(t, ns)
	pc := get(t, ns, "pc", &v1.PulsarCluster{})
	bk := newBK(t, ns, 3, nil)
	for _, template := range []client.Object{pc, bk} {
		limit, validate := 47, v1.ValidateClusterName
		if _, ok := template.(*v1.BookKeeperCluster); ok {
			limit, validate = 51, v1.ValidateBookKeeperName
		}
		for _, name := range []string{strings.Repeat("a", limit), strings.Repeat("a", limit+1), strings.Repeat("a", 60), "1cluster", "cluster.name"} {
			obj := template.DeepCopyObject().(client.Object)
			metadata := metav1.ObjectMeta{Name: name, Namespace: ns}
			switch obj := obj.(type) {
			case *v1.PulsarCluster:
				obj.ObjectMeta = metadata
			case *v1.BookKeeperCluster:
				obj.ObjectMeta = metadata
			}
			err := k8s.Create(ctx, obj)
			if validate(name) != nil {
				if !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), "cluster name") {
					t.Fatalf("%T name %q should fail CRD admission, got %v", obj, name, err)
				}
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			var children []client.Object
			switch obj := obj.(type) {
			case *v1.PulsarCluster:
				p := resources.Pulsar{C: obj}
				children = []client.Object{p.ZooKeeperService(), p.BrokerHeadlessService(), p.MetadataJob()}
			case *v1.BookKeeperCluster:
				b := resources.BookKeeper{C: obj}
				children = []client.Object{b.HeadlessService(), b.InitJob(), b.DecommissionJob(0), b.DecommissionJob(2147483646)}
			}
			for _, child := range children {
				if err := k8s.Create(ctx, child); err != nil {
					t.Fatalf("valid boundary name generated invalid %T %s: %v", child, child.GetName(), err)
				}
			}
		}
	}
}
