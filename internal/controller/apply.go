// Package controller holds the reconcilers of PulsarCluster and BookKeeperCluster.
package controller

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	v1 "github.com/SongOf/pulsar-operator/api/v1alpha1"
	"github.com/SongOf/pulsar-operator/internal/resources"
)

// applier creates or updates the objects a cluster owns.
type applier struct {
	c      client.Client
	scheme *runtime.Scheme
	owner  client.Object
}

func (a applier) own(obj client.Object) error {
	return controllerutil.SetControllerReference(a.owner, obj, a.scheme)
}

func mergeMap(dst, src map[string]string) map[string]string {
	if dst == nil {
		dst = map[string]string{}
	}
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

func (a applier) configMap(ctx context.Context, want *corev1.ConfigMap) error {
	got := &corev1.ConfigMap{ObjectMeta: want.ObjectMeta}
	_, err := controllerutil.CreateOrUpdate(ctx, a.c, got, func() error {
		got.Labels = mergeMap(got.Labels, want.Labels)
		got.Data = want.Data
		return a.own(got)
	})
	return err
}

// service keeps the cluster IP and node ports Kubernetes assigned.
func (a applier) service(ctx context.Context, want *corev1.Service) error {
	got := &corev1.Service{ObjectMeta: want.ObjectMeta}
	_, err := controllerutil.CreateOrUpdate(ctx, a.c, got, func() error {
		got.Labels = mergeMap(got.Labels, want.Labels)
		nodePorts := map[string]int32{}
		for _, p := range got.Spec.Ports {
			nodePorts[p.Name] = p.NodePort
		}
		ports := want.Spec.Ports
		if want.Spec.Type == corev1.ServiceTypeNodePort || want.Spec.Type == corev1.ServiceTypeLoadBalancer {
			for i := range ports {
				ports[i].NodePort = nodePorts[ports[i].Name]
			}
		}
		got.Spec.Ports = ports
		got.Spec.Selector = want.Spec.Selector
		got.Spec.PublishNotReadyAddresses = want.Spec.PublishNotReadyAddresses
		if got.CreationTimestamp.IsZero() {
			got.Spec.ClusterIP = want.Spec.ClusterIP
		}
		if want.Spec.Type != "" {
			got.Spec.Type = want.Spec.Type
		}
		return a.own(got)
	})
	return err
}

func (a applier) pdb(ctx context.Context, want *policyv1.PodDisruptionBudget) error {
	got := &policyv1.PodDisruptionBudget{ObjectMeta: want.ObjectMeta}
	_, err := controllerutil.CreateOrUpdate(ctx, a.c, got, func() error {
		got.Labels = mergeMap(got.Labels, want.Labels)
		got.Spec = want.Spec
		return a.own(got)
	})
	return err
}

// statefulSet creates the StatefulSet or updates its mutable fields. Volume claim templates are immutable; when
// they change and recreate is true, the StatefulSet is deleted with its pods and PVCs orphaned, so the next
// reconcile recreates it with the new templates and adopts the running pods. It returns the current object.
func (a applier) statefulSet(ctx context.Context, want *appsv1.StatefulSet, recreate bool) (*appsv1.StatefulSet, error) {
	got := &appsv1.StatefulSet{}
	err := a.c.Get(ctx, client.ObjectKeyFromObject(want), got)
	if apierrors.IsNotFound(err) {
		if err := a.own(want); err != nil {
			return nil, err
		}
		return want, a.c.Create(ctx, want)
	}
	if err != nil {
		return nil, err
	}
	wantHash := want.Annotations[resources.AnnotationVolumeTemplateHash]
	if recreate && wantHash != "" && got.Annotations[resources.AnnotationVolumeTemplateHash] != wantHash {
		orphan := metav1DeleteOrphan()
		if err := a.c.Delete(ctx, got, orphan); err != nil && !apierrors.IsNotFound(err) {
			return nil, err
		}
		return nil, errRecreating
	}
	patch := client.MergeFrom(got.DeepCopy())
	got.Labels = mergeMap(got.Labels, want.Labels)
	got.Spec.Replicas = want.Spec.Replicas
	got.Spec.Template = want.Spec.Template
	got.Spec.UpdateStrategy = want.Spec.UpdateStrategy
	got.Spec.PersistentVolumeClaimRetentionPolicy = want.Spec.PersistentVolumeClaimRetentionPolicy
	if err := a.own(got); err != nil {
		return nil, err
	}
	return got, a.c.Patch(ctx, got, patch)
}

var errRecreating = fmt.Errorf("statefulset is being recreated with new volume claim templates")

func metav1DeleteOrphan() client.DeleteOption {
	return client.PropagationPolicy("Orphan")
}

func (a applier) deployment(ctx context.Context, want *appsv1.Deployment) (*appsv1.Deployment, error) {
	got := &appsv1.Deployment{ObjectMeta: want.ObjectMeta}
	_, err := controllerutil.CreateOrUpdate(ctx, a.c, got, func() error {
		got.Labels = mergeMap(got.Labels, want.Labels)
		got.Spec.Replicas = want.Spec.Replicas
		if got.CreationTimestamp.IsZero() {
			got.Spec.Selector = want.Spec.Selector
		}
		got.Spec.Template = want.Spec.Template
		return a.own(got)
	})
	return got, err
}

// job creates the job unless it exists; jobs are never updated.
func (a applier) job(ctx context.Context, want *batchv1.Job) (*batchv1.Job, error) {
	got := &batchv1.Job{}
	err := a.c.Get(ctx, client.ObjectKeyFromObject(want), got)
	if apierrors.IsNotFound(err) {
		if err := a.own(want); err != nil {
			return nil, err
		}
		return want, a.c.Create(ctx, want)
	}
	return got, err
}

// deleteIfExists removes an object, ignoring absence.
func deleteIfExists(ctx context.Context, c client.Client, obj client.Object) error {
	err := c.Delete(ctx, obj, client.PropagationPolicy("Background"))
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

func jobSucceeded(j *batchv1.Job) bool { return jobCondition(j, batchv1.JobComplete) }
func jobFailed(j *batchv1.Job) bool    { return jobCondition(j, batchv1.JobFailed) }

func jobCondition(j *batchv1.Job, t batchv1.JobConditionType) bool {
	for _, c := range j.Status.Conditions {
		if c.Type == t && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// stsStatus summarises a StatefulSet; RolledOut means every replica runs the current revision and is ready.
func stsStatus(s *appsv1.StatefulSet) v1.ComponentStatus {
	if s == nil {
		return v1.ComponentStatus{}
	}
	want := int32(1)
	if s.Spec.Replicas != nil {
		want = *s.Spec.Replicas
	}
	st := s.Status
	return v1.ComponentStatus{
		Replicas:        want,
		ReadyReplicas:   st.ReadyReplicas,
		UpdatedReplicas: st.UpdatedReplicas,
		Image:           containerImage(s.Spec.Template.Spec),
		RolledOut: st.ObservedGeneration >= s.Generation && st.Replicas == want && st.ReadyReplicas == want &&
			st.UpdatedReplicas == want && (st.CurrentRevision == st.UpdateRevision || st.UpdateRevision == ""),
	}
}

func deployStatus(d *appsv1.Deployment) v1.ComponentStatus {
	if d == nil {
		return v1.ComponentStatus{}
	}
	want := int32(1)
	if d.Spec.Replicas != nil {
		want = *d.Spec.Replicas
	}
	st := d.Status
	return v1.ComponentStatus{
		Replicas: want, ReadyReplicas: st.ReadyReplicas, UpdatedReplicas: st.UpdatedReplicas,
		Image:     containerImage(d.Spec.Template.Spec),
		RolledOut: st.ObservedGeneration >= d.Generation && st.UpdatedReplicas == want && st.ReadyReplicas == want && st.Replicas == want,
	}
}

func containerImage(p corev1.PodSpec) string {
	if len(p.Containers) == 0 {
		return ""
	}
	return p.Containers[0].Image
}

func getOptional[T client.Object](ctx context.Context, c client.Client, key types.NamespacedName, obj T) (T, bool, error) {
	err := c.Get(ctx, key, obj)
	if apierrors.IsNotFound(err) {
		var zero T
		return zero, false, nil
	}
	return obj, err == nil, err
}
