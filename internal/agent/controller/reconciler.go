// Package controller reconciles the storage CRDs on a single node. It does not
// depend on the storage console or Pulsar controllers, and does not implement CSI.
package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"

	storagev1 "github.com/SongOf/pulsar-operator/api/storage/v1alpha1"
	"github.com/SongOf/pulsar-operator/internal/agent/model"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/dynamic"
)

const (
	Group     = storagev1.GroupName
	Version   = storagev1.Version
	NodeLabel = Group + "/node"
	Finalizer = Group + "/retain-media"
)

var (
	Pools   = storagev1.DiskPoolResource
	Disks   = storagev1.PhysicalDiskResource
	Classes = schema.GroupVersionResource{Group: "storage.k8s.io", Version: "v1", Resource: "storageclasses"}
	Nodes   = schema.GroupVersionResource{Version: "v1", Resource: "nodes"}
)

type Backend interface {
	EnsurePool(context.Context, string, []model.Device, bool, string, bool) (model.PoolResult, error)
}

type Reconciler struct {
	Client        dynamic.Interface
	Backend       Backend
	Discover      func(context.Context) ([]model.Disk, error)
	NodeName      string
	WritesEnabled bool
	Logger        *slog.Logger
}

type poolSpec = storagev1.DiskPoolSpec

type pool struct {
	object *unstructured.Unstructured
	spec   poolSpec
}

// ReconcileOnce reads one complete claim snapshot, then reconciles local pools
// and physical disks. All nodes' claims are included in conflict detection;
// mutable labels must never hide a competing claim.
func (r *Reconciler) ReconcileOnce(ctx context.Context) error {
	if r.NodeName == "" || r.Client == nil || r.Backend == nil || r.Discover == nil {
		return errors.New("node name, Kubernetes client, backend and discovery are required")
	}
	objects, err := r.list(ctx, Pools, "")
	if err != nil {
		return err
	}
	claims := make([]pool, 0, len(objects))
	for i := range objects {
		var spec poolSpec
		value, ok := objects[i].Object["spec"].(map[string]interface{})
		if !ok {
			return fmt.Errorf("DiskPool %s has no spec", objects[i].GetName())
		}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(value, &spec); err != nil {
			return fmt.Errorf("decode DiskPool %s: %w", objects[i].GetName(), err)
		}
		claims = append(claims, pool{object: &objects[i], spec: spec})
	}
	c := cycle{r: r, claims: claims}
	var failures []error
	var local []pool
	for _, claim := range claims {
		if claim.object.GetLabels()[NodeLabel] != NodeLabelValue(r.NodeName) || claim.spec.NodeName != r.NodeName {
			continue
		}
		if err := c.reconcilePool(ctx, claim); err != nil {
			failures = append(failures, fmt.Errorf("pool %s: %w", claim.object.GetName(), err))
		}
		local = append(local, claim)
	}
	if err := r.reconcileDisks(ctx, local); err != nil {
		failures = append(failures, err)
	}
	return errors.Join(failures...)
}

type cycle struct {
	r        *Reconciler
	claims   []pool
	hostname string
}

func (c *cycle) reconcilePool(ctx context.Context, p pool) error {
	status, _, err := unstructured.NestedMap(p.object.Object, "status")
	if err != nil {
		return err
	}
	if status == nil {
		status = map[string]interface{}{}
	}
	status["observedGeneration"] = p.object.GetGeneration()
	if p.object.GetDeletionTimestamp() != nil {
		status["phase"] = "Retained"
		status["message"] = "Media and existing volumes are retained. Administrator must assess data and explicitly remove the retain-media finalizer."
		return c.r.status(ctx, Pools, p.object, status)
	}
	if err := c.r.ensureFinalizer(ctx, p.object); err != nil {
		return err
	}
	switch {
	case !p.spec.Approved:
		status["phase"], status["message"] = "PendingApproval", "Device initialization has not been approved."
	case !c.r.WritesEnabled:
		status["phase"], status["message"] = "WritesDisabled", "Agent ENABLE_DISK_WRITES is false; no disk commands executed."
	default:
		result, class, hostname, err := c.preparePool(ctx, p)
		if err != nil {
			message := []rune(err.Error())
			if len(message) > 1500 {
				message = message[:1500]
			}
			status["phase"], status["message"] = "Error", string(message)
			logger := c.r.Logger
			if logger == nil {
				logger = slog.Default()
			}
			logger.Error("pool reconciliation failed", "pool", p.object.GetName(), "error", err)
		} else {
			status["phase"], status["message"] = "Ready", "Volume group ready; OpenEBS CSI provisions PVCs."
			status["storageClass"], status["topologyHostname"] = class, hostname
			status["capacityBytes"], status["availableBytes"] = result.CapacityBytes, result.AvailableBytes
		}
	}
	return c.r.status(ctx, Pools, p.object, status)
}

func (c *cycle) preparePool(ctx context.Context, p pool) (model.PoolResult, string, string, error) {
	var empty model.PoolResult
	uid := string(p.object.GetUID())
	if uid == "" {
		return empty, "", "", errors.New("DiskPool UID is required")
	}
	for _, other := range c.claims {
		if other.object.GetUID() == p.object.GetUID() && other.object.GetName() == p.object.GetName() {
			continue
		}
		if overlaps(p.spec, other.spec) {
			return empty, "", "", fmt.Errorf("media is also claimed by DiskPool %s", other.object.GetName())
		}
	}
	if c.hostname == "" {
		node, err := c.r.Client.Resource(Nodes).Get(ctx, c.r.NodeName, metav1.GetOptions{})
		if err != nil {
			return empty, "", "", err
		}
		hostname := node.GetLabels()["kubernetes.io/hostname"]
		if hostname == "" || len(validation.IsValidLabelValue(hostname)) != 0 {
			return empty, "", "", errors.New("Node requires a valid kubernetes.io/hostname topology label")
		}
		c.hostname = hostname
	}
	desired := storageClass(p, c.hostname)
	class := desired.GetName()
	existing, err := c.r.Client.Resource(Classes).Get(ctx, class, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return empty, "", "", err
	}
	if err == nil {
		if err := validateClass(existing, desired); err != nil {
			return empty, "", "", err
		}
	}
	previousClass, _, _ := unstructured.NestedString(p.object.Object, "status", "storageClass")
	previousHost, _, _ := unstructured.NestedString(p.object.Object, "status", "topologyHostname")
	legacy := existing != nil && previousClass == class && previousHost == c.hostname
	result, err := c.r.Backend.EnsurePool(ctx, p.spec.VGName, p.spec.Devices, true, uid, legacy)
	if err != nil {
		return empty, "", "", err
	}
	// Disk initialization can take time. Re-read the class afterwards: the
	// preflight observation is insufficient if it was replaced during LVM IO.
	existing, err = c.r.Client.Resource(Classes).Get(ctx, class, metav1.GetOptions{})
	if err == nil {
		if err := validateClass(existing, desired); err != nil {
			return empty, "", "", err
		}
	} else if !apierrors.IsNotFound(err) {
		return empty, "", "", err
	} else {
		_, err := c.r.Client.Resource(Classes).Create(ctx, desired, metav1.CreateOptions{})
		if apierrors.IsAlreadyExists(err) {
			existing, err = c.r.Client.Resource(Classes).Get(ctx, class, metav1.GetOptions{})
			if err == nil {
				err = validateClass(existing, desired)
			}
		}
		if err != nil {
			return empty, "", "", err
		}
	}
	return result, class, c.hostname, nil
}

func overlaps(a, b poolSpec) bool {
	if a.NodeName != b.NodeName {
		return false
	}
	if a.VGName == b.VGName {
		return true
	}
	for _, first := range a.Devices {
		for _, second := range b.Devices {
			if first.Path != "" && first.Path == second.Path || first.Serial != "" && first.Serial == second.Serial {
				return true
			}
		}
	}
	return false
}

func (r *Reconciler) list(ctx context.Context, resource schema.GroupVersionResource, selector string) ([]unstructured.Unstructured, error) {
	options := metav1.ListOptions{Limit: 500, LabelSelector: selector}
	var items []unstructured.Unstructured
	seen := map[string]bool{}
	for {
		page, err := r.Client.Resource(resource).List(ctx, options)
		if err != nil {
			return nil, err
		}
		items = append(items, page.Items...)
		options.Continue = page.GetContinue()
		if options.Continue == "" {
			return items, nil
		}
		if seen[options.Continue] {
			return nil, fmt.Errorf("repeated pagination cursor for %s", resource.Resource)
		}
		seen[options.Continue] = true
	}
}

func (r *Reconciler) ensureFinalizer(ctx context.Context, obj *unstructured.Unstructured) error {
	for _, existing := range obj.GetFinalizers() {
		if existing == Finalizer {
			return nil
		}
	}
	if obj.GetResourceVersion() == "" {
		return errors.New("DiskPool resourceVersion is required")
	}
	patch, err := json.Marshal(map[string]interface{}{"metadata": map[string]interface{}{
		"resourceVersion": obj.GetResourceVersion(), "finalizers": append(obj.GetFinalizers(), Finalizer),
	}})
	if err != nil {
		return err
	}
	updated, err := r.Client.Resource(Pools).Patch(ctx, obj.GetName(), types.MergePatchType, patch, metav1.PatchOptions{})
	if err == nil {
		obj.Object = updated.Object
	}
	return err
}

func (r *Reconciler) status(ctx context.Context, resource schema.GroupVersionResource, obj *unstructured.Unstructured, status map[string]interface{}) error {
	if reflect.DeepEqual(obj.Object["status"], status) {
		return nil
	}
	if obj.GetResourceVersion() == "" {
		return fmt.Errorf("%s resourceVersion is required", resource.Resource)
	}
	patch, err := json.Marshal(map[string]interface{}{
		"metadata": map[string]interface{}{"resourceVersion": obj.GetResourceVersion()}, "status": status,
	})
	if err != nil {
		return err
	}
	updated, err := r.Client.Resource(resource).Patch(ctx, obj.GetName(), types.MergePatchType, patch, metav1.PatchOptions{}, "status")
	if err == nil {
		obj.Object = updated.Object
	}
	return err
}

func (r *Reconciler) reconcileDisks(ctx context.Context, pools []pool) error {
	discovered, err := r.Discover(ctx)
	if err != nil {
		return fmt.Errorf("discover disks: %w", err)
	}
	objects, err := r.list(ctx, Disks, NodeLabel+"="+NodeLabelValue(r.NodeName))
	if err != nil {
		return err
	}
	existing := map[string]*unstructured.Unstructured{}
	for i := range objects {
		existing[objects[i].GetName()] = &objects[i]
	}
	seen := map[string]bool{}
	managed := map[model.Device]string{}
	for _, p := range pools {
		phase, _, _ := unstructured.NestedString(p.object.Object, "status", "phase")
		if phase == "Ready" || phase == "Retained" {
			for _, device := range p.spec.Devices {
				managed[device] = p.object.GetName()
			}
		}
	}
	for _, disk := range discovered {
		name := DiskName(r.NodeName, disk)
		seen[name] = true
		modelName, media := disk.Model, disk.Media
		if modelName == "" {
			modelName = "unknown"
		}
		if media == "" {
			media = "unknown"
		}
		spec, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&storagev1.PhysicalDiskSpec{
			NodeName: r.NodeName, Path: disk.Path, Serial: disk.Serial,
			SizeBytes: disk.SizeBytes, Model: modelName, Media: media,
		})
		if err != nil {
			return err
		}
		phase, reasons := disk.State, append([]string{}, disk.Reasons...)
		if phase != "Available" && phase != "Protected" {
			phase = "Protected"
			reasons = append(reasons, "Discovery returned an unknown device state.")
		}
		owner := managed[model.Device{Path: disk.Path, Serial: disk.Serial, SizeBytes: disk.SizeBytes}]
		if owner != "" {
			phase, reasons = "Managed", []string{}
		}
		current := existing[name]
		if current == nil {
			current, err = r.Client.Resource(Disks).Create(ctx, &unstructured.Unstructured{Object: map[string]interface{}{
				"apiVersion": Group + "/" + Version, "kind": "PhysicalDisk",
				"metadata": map[string]interface{}{"name": name, "labels": map[string]interface{}{NodeLabel: NodeLabelValue(r.NodeName)}},
				"spec":     spec,
			}}, metav1.CreateOptions{})
			if apierrors.IsAlreadyExists(err) {
				continue
			}
			if err != nil {
				return err
			}
		} else if !reflect.DeepEqual(current.Object["spec"], spec) {
			phase, reasons, owner = "Protected", []string{"Device identity or capacity changed; inventory spec is immutable."}, ""
		}
		values := make([]interface{}, len(reasons))
		for i, reason := range reasons {
			values[i] = reason
		}
		if err := r.status(ctx, Disks, current, map[string]interface{}{"phase": phase, "reasons": values, "poolName": owner}); err != nil {
			return err
		}
	}
	for name, disk := range existing {
		node, _, _ := unstructured.NestedString(disk.Object, "spec", "nodeName")
		if seen[name] || node != r.NodeName {
			continue
		}
		status, _, err := unstructured.NestedMap(disk.Object, "status")
		if err != nil {
			return err
		}
		if status == nil {
			status = map[string]interface{}{}
		}
		status["phase"], status["reasons"] = "Offline", []interface{}{"Device absent from current node discovery."}
		if err := r.status(ctx, Disks, disk, status); err != nil {
			return err
		}
	}
	return nil
}

func NodeLabelValue(value string) string {
	if len(value) <= 63 {
		return value
	}
	return "sha256-" + digest(value)[:40]
}

func StorageClassName(poolName string) string {
	if len(poolName) <= 53 {
		return "diskfleet-" + poolName
	}
	return "diskfleet-" + poolName[:44] + "-" + digest(poolName)[:8]
}

func DiskName(node string, disk model.Disk) string {
	return "disk-" + digest(node + "\x00" + disk.Serial + "\x00" + disk.Path)[:32]
}

func digest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func storageClass(p pool, hostname string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "storage.k8s.io/v1", "kind": "StorageClass",
		"metadata": map[string]interface{}{
			"name":        StorageClassName(p.object.GetName()),
			"labels":      map[string]interface{}{Group + "/pool": NodeLabelValue(p.object.GetName()), NodeLabel: NodeLabelValue(p.spec.NodeName)},
			"annotations": map[string]interface{}{Group + "/pool-uid": string(p.object.GetUID())},
		},
		"provisioner":   "local.csi.openebs.io",
		"parameters":    map[string]interface{}{"storage": "lvm", "vgpattern": "^" + p.spec.VGName + "$", "fsType": "ext4", "thinProvision": "no"},
		"reclaimPolicy": "Retain", "volumeBindingMode": "WaitForFirstConsumer", "allowVolumeExpansion": true,
		"allowedTopologies": []interface{}{map[string]interface{}{"matchLabelExpressions": []interface{}{
			map[string]interface{}{"key": "kubernetes.io/hostname", "values": []interface{}{hostname}},
		}}},
	}}
}

func validateClass(actual, desired *unstructured.Unstructured) error {
	if actual.GetAnnotations()[Group+"/pool-uid"] != desired.GetAnnotations()[Group+"/pool-uid"] {
		return fmt.Errorf("StorageClass %s already belongs to another pool", desired.GetName())
	}
	for _, key := range strings.Fields("provisioner parameters reclaimPolicy volumeBindingMode allowVolumeExpansion allowedTopologies") {
		if !reflect.DeepEqual(actual.Object[key], desired.Object[key]) {
			return fmt.Errorf("StorageClass %s differs in %s; refusing to overwrite", desired.GetName(), key)
		}
	}
	return nil
}
