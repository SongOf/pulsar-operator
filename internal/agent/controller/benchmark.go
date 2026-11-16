package controller

import (
	"context"
	"errors"
	"fmt"
	"regexp"

	storagev1 "github.com/SongOf/pulsar-operator/api/storage/v1alpha1"
	"github.com/SongOf/pulsar-operator/internal/agent/model"
	"github.com/SongOf/pulsar-operator/internal/storage/poolstate"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
)

type benchmarkBackend interface {
	EnsureBenchmark(context.Context, model.BenchmarkRequest, func(context.Context) error) (storagev1.DiskPoolBenchmark, error)
}

type guardedPoolBackend interface {
	EnsurePoolGuarded(context.Context, string, []model.Device, bool, string, bool, func(context.Context) error) (model.PoolResult, error)
}

func (c *cycle) ensurePool(ctx context.Context, p pool, legacy, requireApproval bool) (model.PoolResult, error) {
	guard := func(ctx context.Context) error { return c.r.guardPool(ctx, p, c.nodeUID, c.hostname, requireApproval) }
	if err := guard(ctx); err != nil {
		return model.PoolResult{}, err
	}
	if backend, ok := c.r.Backend.(guardedPoolBackend); ok {
		return backend.EnsurePoolGuarded(ctx, p.spec.VGName, p.spec.Devices, true, string(p.object.GetUID()), legacy, guard)
	}
	// Non-device backends used by contract tests preserve the original interface.
	// The production LVM backend always implements the guarded operation.
	return c.r.Backend.EnsurePool(ctx, p.spec.VGName, p.spec.Devices, true, string(p.object.GetUID()), legacy)
}

func decodePool(obj *unstructured.Unstructured) (*storagev1.DiskPool, error) {
	var result storagev1.DiskPool
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *cycle) reconcileAllocation(ctx context.Context, p pool, status map[string]interface{}) error {
	current, err := decodePool(p.object)
	if err != nil {
		return err
	}
	if current.Status.Benchmark == nil && (current.Status.StorageClass != "" || current.Status.Phase == "Ready" || current.Status.Phase == "LegacyUnreviewed") {
		status["phase"], status["message"] = "LegacyUnreviewed", "Existing pool has no benchmark record. Existing data is retained; automatic write benchmarking and new managed allocations are disabled."
		return nil
	}
	node, err := c.r.Client.Resource(Nodes).Get(ctx, c.r.NodeName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	c.hostname, c.nodeUID = node.GetLabels()["kubernetes.io/hostname"], string(node.GetUID())
	if c.nodeUID == "" || node.GetDeletionTimestamp() != nil || c.hostname == "" || len(validation.IsValidLabelValue(c.hostname)) != 0 {
		return errors.New("a live Node UID and valid hostname topology are required")
	}
	b := current.Status.Benchmark
	if b == nil || b.Phase == "Running" || !b.CleanupVerified {
		backend, ok := c.r.Backend.(benchmarkBackend)
		if !ok {
			return errors.New("Agent backend does not support the required single-disk benchmark")
		}
		if len(p.spec.Devices) != 1 || p.object.GetUID() == "" {
			return errors.New("a single device and pool UID are required")
		}
		for _, other := range c.claims {
			if other.object.GetUID() != p.object.GetUID() && overlaps(p.spec, other.spec) {
				return fmt.Errorf("media is also claimed by DiskPool %s", other.object.GetName())
			}
		}
		if err := c.r.guardPool(ctx, p, c.nodeUID, c.hostname, false); err != nil {
			return err
		}
		// A saved benchmark (including cleanup-pending) already has an owned
		// VG. Re-entering initialization could block recovery of the test LV.
		if b == nil {
			status["phase"], status["message"] = "Preparing", "Preparing the approved single-disk volume group."
			if err := c.r.status(ctx, Pools, p.object, status); err != nil {
				return err
			}
			result, err := c.ensurePool(ctx, p, false, false)
			if err != nil {
				return err
			}
			status["capacityBytes"], status["availableBytes"] = result.CapacityBytes, result.AvailableBytes
		}
		request := model.BenchmarkRequest{PoolUID: string(p.object.GetUID()), NodeUID: c.nodeUID, VGName: p.spec.VGName, Device: p.spec.Devices[0], RunID: poolstate.RunID(string(p.object.GetUID())), Approved: true}
		if b == nil {
			b = &storagev1.DiskPoolBenchmark{PoolUID: request.PoolUID, NodeUID: request.NodeUID, VGName: request.VGName, Device: request.Device, RunID: request.RunID, Profile: poolstate.Profile, Phase: "Running", TestSizeBytes: poolstate.TestSizeBytes}
		}
		status["phase"], status["message"] = "Benchmarking", "Running the temporary-LV benchmark or recovering its cleanup; allocation is blocked."
		status["topologyHostname"] = c.hostname
		if err := setBenchmark(status, b); err != nil {
			return err
		}
		if err := c.r.status(ctx, Pools, p.object, status); err != nil {
			return err
		}
		result, err := backend.EnsureBenchmark(ctx, request, func(ctx context.Context) error {
			return c.r.guardPool(ctx, p, request.NodeUID, c.hostname, false)
		})
		if err != nil {
			// Never approve an uncommitted result, even if device IO succeeded.
			// The durable journal is retried next cycle for recovery/publication.
			status["phase"], status["message"] = "BenchmarkFailed", "Benchmark result could not be safely persisted or recovered; allocation remains blocked."
			return nil
		}
		b = &result
		if err := setBenchmark(status, b); err != nil {
			return err
		}
		current.Status.Benchmark = b
	}
	if err := poolstate.Reviewable(current, c.nodeUID); err != nil {
		status["phase"], status["message"] = "BenchmarkFailed", "Benchmark is incomplete, failed, or does not match the current device and node; allocation remains blocked."
		return nil
	}
	if err := poolstate.Approved(current, c.nodeUID); err != nil {
		status["phase"], status["message"] = "AwaitingReview", "Benchmark succeeded and the temporary LV was removed. Human review is required before allocation."
		if current.Spec.AllocationApproval != nil && current.Spec.AllocationApproval.Decision == "Rejected" {
			status["phase"], status["message"] = "Rejected", "The benchmark was rejected; allocation remains blocked."
		}
		return nil
	}
	// preparePool also reads the latest approval after LVM verification and
	// validates any existing StorageClass before making the pool Ready.
	result, class, hostname, err := c.preparePool(ctx, p)
	if err != nil {
		return err
	}
	status["phase"], status["message"] = "Ready", "Benchmark reviewed and approved; OpenEBS CSI may provision business volumes."
	status["storageClass"], status["topologyHostname"] = class, hostname
	status["capacityBytes"], status["availableBytes"] = result.CapacityBytes, result.AvailableBytes
	return nil
}

func setBenchmark(status map[string]interface{}, value *storagev1.DiskPoolBenchmark) error {
	object, err := runtime.DefaultUnstructuredConverter.ToUnstructured(value)
	if err == nil {
		status["benchmark"] = object
	}
	return err
}

func (r *Reconciler) guardPool(ctx context.Context, expected pool, nodeUID, hostname string, requireApproval bool) error {
	if !r.WritesEnabled {
		return errors.New("Agent disk writes are disabled")
	}
	obj, err := r.Client.Resource(Pools).Get(ctx, expected.object.GetName(), metav1.GetOptions{})
	if err != nil {
		return errors.New("current DiskPool could not be verified")
	}
	current, err := decodePool(obj)
	if err != nil || obj.GetUID() != expected.object.GetUID() || obj.GetDeletionTimestamp() != nil || !current.Spec.Approved || current.Spec.NodeName != r.NodeName || current.Spec.VGName != expected.spec.VGName || len(current.Spec.Devices) != 1 || len(expected.spec.Devices) != 1 || current.Spec.Devices[0] != expected.spec.Devices[0] {
		return errors.New("DiskPool identity or initialization authorization changed")
	}
	node, err := r.Client.Resource(Nodes).Get(ctx, r.NodeName, metav1.GetOptions{})
	if err != nil || string(node.GetUID()) != nodeUID || node.GetDeletionTimestamp() != nil || node.GetLabels()["kubernetes.io/hostname"] != hostname {
		return errors.New("Node identity or hostname topology changed")
	}
	if requireApproval {
		return poolstate.Approved(current, nodeUID)
	}
	return r.requireUnpublished(ctx, current.Spec.VGName)
}

// CSI does not read DiskPool status. New pools remain outside every existing
// StorageClass selector until review. An administrator must not introduce an
// unmanaged broad selector concurrently; that requires cluster admission/RBAC.
func (r *Reconciler) requireUnpublished(ctx context.Context, vg string) error {
	classes, err := r.list(ctx, Classes, "")
	if err != nil {
		return errors.New("StorageClass allocation selectors could not be verified")
	}
	for _, class := range classes {
		provisioner, _, _ := unstructured.NestedString(class.Object, "provisioner")
		if provisioner != "local.csi.openebs.io" {
			continue
		}
		storage, _, _ := unstructured.NestedString(class.Object, "parameters", "storage")
		if storage != "lvm" && storage != "" {
			continue
		}
		volumeGroup, _, _ := unstructured.NestedString(class.Object, "parameters", "volgroup")
		pattern, _, _ := unstructured.NestedString(class.Object, "parameters", "vgpattern")
		if volumeGroup == vg {
			return errors.New("an existing StorageClass can select this unreviewed VG")
		}
		if pattern != "" {
			selector, err := regexp.Compile(pattern)
			if err != nil || selector.MatchString(vg) {
				return errors.New("an existing StorageClass may select this unreviewed VG")
			}
		} else if volumeGroup == "" {
			return errors.New("an unconstrained LVM StorageClass prevents benchmarking an unreviewed VG")
		}
	}
	return nil
}
