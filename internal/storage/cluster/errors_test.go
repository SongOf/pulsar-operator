package cluster

import (
	"context"
	"errors"
	"net"
	"net/url"
	"syscall"
	"testing"

	"github.com/SongOf/pulsar-operator/internal/storage/model"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"
)

func TestKubernetesAvailabilityErrorsAre503AndAPIStatusIsPreserved(t *testing.T) {
	ctx := context.Background()
	operations := []struct {
		name, verb, resource string
		run                  func(*Cluster) error
	}{
		{"inventory-list", "list", "diskpools", func(c *Cluster) error { _, err := c.Overview(ctx); return err }},
		{"class-get", "get", "storageclasses", func(c *Cluster) error {
			_, err := c.CreateVolume(ctx, object{"name": "new", "poolId": "pool-one", "sizeGiB": int64(1)})
			return err
		}},
		{"pvc-get", "get", "persistentvolumeclaims", func(c *Cluster) error {
			_, err := c.Expand(ctx, "default~data", object{"sizeGiB": int64(2)})
			return err
		}},
		{"pvc-create", "create", "persistentvolumeclaims", func(c *Cluster) error {
			_, err := c.CreateVolume(ctx, object{"name": "new", "poolId": "pool-one", "sizeGiB": int64(1)})
			return err
		}},
		{"pvc-patch", "patch", "persistentvolumeclaims", func(c *Cluster) error {
			_, err := c.Expand(ctx, "default~data", object{"sizeGiB": int64(2)})
			return err
		}},
		{"pvc-delete", "delete", "persistentvolumeclaims", func(c *Cluster) error { _, err := c.Release(ctx, "default~data", "pvc-uid"); return err }},
	}
	failures := []struct {
		name      string
		err       error
		transport bool
	}{
		{"connection-refused", &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}, true},
		{"upstream-timeout", &url.Error{Op: "Get", URL: "https://kubernetes.default.svc", Err: context.DeadlineExceeded}, true},
		{"api-conflict", apierrors.NewConflict(pvcsResource.GroupResource(), "data", errors.New("resourceVersion changed")), false},
		{"api-forbidden", apierrors.NewForbidden(pvcsResource.GroupResource(), "data", errors.New("denied")), false},
		{"api-invalid", &apierrors.StatusError{ErrStatus: metav1.Status{Code: 422, Reason: metav1.StatusReasonInvalid, Message: "invalid resource"}}, false},
	}
	for _, operation := range operations {
		for _, failure := range failures {
			t.Run(operation.name+"/"+failure.name, func(t *testing.T) {
				f := newFixture(t)
				f.boundVolume(true)
				f.client.PrependReactor(operation.verb, operation.resource, func(k8stesting.Action) (bool, runtime.Object, error) { return true, nil, failure.err })
				err := operation.run(f.cluster)
				if failure.transport {
					assertProblem(t, err, 503)
				} else if err != failure.err {
					t.Fatalf("API server status was replaced: want=%v got=%v", failure.err, err)
				}
				if operation.verb == "delete" && (f.count("release-intents") != 1 || f.count("retained") != 0) {
					t.Fatal("failed remote DELETE lost intent or falsely completed retention")
				}
			})
		}
	}
}

func TestLocalStoreFailureIsNotKubernetesUnavailable(t *testing.T) {
	f := newFixture(t)
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	_, err := f.cluster.Overview(context.Background())
	if err == nil {
		t.Fatal("closed local store unexpectedly succeeded")
	}
	var problem *model.Problem
	if errors.As(err, &problem) && problem.Status == 503 {
		t.Fatal("local SQLite error misclassified as Kubernetes unavailable", err)
	}
}
