package cluster

import (
	"context"
	"errors"
	"net"

	"github.com/SongOf/pulsar-operator/internal/storage/model"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// kubeError is applied only where an individual Kubernetes request returns.
// Upstream availability failures are distinct from HTTP request-body timeouts
// and local SQLite errors. Preserve API-server authorization/conflict responses.
func kubeError(err error) error {
	if err == nil {
		return nil
	}
	var status apierrors.APIStatus
	if errors.As(err, &status) {
		return err
	}
	var network net.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &network) {
		return model.NewProblem(503, "Kubernetes API 暂不可用，请稍后重试")
	}
	return err
}
