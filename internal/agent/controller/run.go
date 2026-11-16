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
	r.ReconcileInterval = interval
	workerCtx, cancel := context.WithCancel(ctx)
	discoveryDone := make(chan struct{})
	go func() {
		defer close(discoveryDone)
		r.runDiscovery(workerCtx, interval)
	}()
	defer func() {
		cancel()
		<-discoveryDone
	}()
	logger := r.Logger
	if logger == nil {
		logger = slog.Default()
	}
	if !wait(ctx, time.Duration(rand.Float64()*float64(min(interval, 10*time.Second)))) {
		return nil
	}
	for ctx.Err() == nil {
		// The node has one serialized pool worker. Long fio runs or waiting for
		// its host LVM lock never delay the independent inventory heartbeat.
		if err := r.reconcile(ctx, false); err != nil && ctx.Err() == nil {
			logger.Error("node reconciliation failed; will retry", "node", r.NodeName, "error", err)
		}
		if !wait(ctx, interval+time.Duration(rand.Float64()*0.2*float64(interval))) {
			break
		}
	}
	return nil
}

func (r *Reconciler) runDiscovery(ctx context.Context, interval time.Duration) {
	logger := r.Logger
	if logger == nil {
		logger = slog.Default()
	}
	for ctx.Err() == nil {
		claims, err := r.claims(ctx)
		if err == nil {
			local := make([]pool, 0)
			for _, claim := range claims {
				if claim.spec.NodeName == r.NodeName && claim.object.GetLabels()[NodeLabel] == NodeLabelValue(r.NodeName) {
					local = append(local, claim)
				}
			}
			err = r.reconcileDisks(ctx, local)
		}
		if err != nil && ctx.Err() == nil {
			logger.Error("disk inventory failed; will retry", "node", r.NodeName, "error", err)
		}
		if !wait(ctx, interval+time.Duration(rand.Float64()*0.2*float64(interval))) {
			return
		}
	}
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
