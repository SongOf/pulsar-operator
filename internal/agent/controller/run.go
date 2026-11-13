package controller

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"time"
)

// Run retains periodic node discovery even when no Kubernetes object changes.
// The daemon is node-local; a cluster-wide leader lock would disable other nodes.
func (r *Reconciler) Run(ctx context.Context, interval time.Duration) error {
	if interval < 10*time.Second {
		interval = 10 * time.Second
	}
	logger := r.Logger
	if logger == nil {
		logger = slog.Default()
	}
	if !wait(ctx, time.Duration(rand.Float64()*float64(min(interval, 10*time.Second)))) {
		return nil
	}
	for ctx.Err() == nil {
		if err := r.ReconcileOnce(ctx); err != nil && ctx.Err() == nil {
			logger.Error("node reconciliation failed; will retry", "node", r.NodeName, "error", err)
		}
		if !wait(ctx, interval+time.Duration(rand.Float64()*0.2*float64(interval))) {
			break
		}
	}
	return nil
}

func wait(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return ctx.Err() == nil
	}
}
