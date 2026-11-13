package controller

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "github.com/SongOf/pulsar-operator/api/pulsar/v1alpha1"
	"github.com/SongOf/pulsar-operator/internal/pulsar/resources"
)

func bookieClaimLabels(pvc *corev1.PersistentVolumeClaim, cluster string) bool {
	for key, value := range resources.Labels(cluster, resources.Bookie) {
		if pvc.Labels[key] != value {
			return false
		}
	}
	return true
}

// A claim name alone is not proof of ownership: it may have been recreated for
// another workload. Retain claims have no controller reference, so also require
// the current owned bookie Pod to reference the claim before changing its size.
func (r *BookKeeperReconciler) requireBookiePVC(ctx context.Context, b resources.BookKeeper, sts *appsv1.StatefulSet, pvc *corev1.PersistentVolumeClaim, ordinal int32) error {
	if err := (applier{owner: b.C}).requireOwned(sts); err != nil {
		return err
	}
	if !bookieClaimLabels(pvc, b.C.Name) || pvc.DeletionTimestamp != nil {
		return fmt.Errorf("refusing to expand PVC %s: labels or lifecycle do not match the bookie", pvc.Name)
	}
	for _, owner := range pvc.OwnerReferences {
		if owner.UID != sts.UID && owner.UID != b.C.UID {
			return fmt.Errorf("refusing to expand PVC %s: unrelated owner", pvc.Name)
		}
	}
	pod := &corev1.Pod{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: b.C.Namespace, Name: b.PodName(ordinal)}, pod); err != nil {
		return fmt.Errorf("cannot establish workload ownership for PVC %s: %w", pvc.Name, err)
	}
	if !metav1.IsControlledBy(pod, sts) || pod.DeletionTimestamp != nil {
		return fmt.Errorf("refusing to expand PVC %s: bookie Pod is not an active member of the owned StatefulSet", pvc.Name)
	}
	for _, volume := range pod.Spec.Volumes {
		if volume.PersistentVolumeClaim != nil && volume.PersistentVolumeClaim.ClaimName == pvc.Name {
			return nil
		}
	}
	return fmt.Errorf("refusing to expand PVC %s: the owned bookie Pod does not reference it", pvc.Name)
}

// Retain PVCs normally have no ownerReference. Capture their identity while the
// healthy, owned bookie Pod still proves which claims this cluster actually uses.
func (r *BookKeeperReconciler) decommissionPVCs(ctx context.Context, b resources.BookKeeper, a applier, sts *appsv1.StatefulSet, ordinal int32) (map[string]string, error) {
	if err := a.requireOwned(sts); err != nil {
		return nil, err
	}
	pod := &corev1.Pod{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: b.C.Namespace, Name: b.PodName(ordinal)}, pod); err != nil {
		return nil, err
	}
	if !metav1.IsControlledBy(pod, sts) || pod.DeletionTimestamp != nil {
		return nil, fmt.Errorf("cannot decommission %s: Pod is not an active member of the owned StatefulSet", pod.Name)
	}
	identities := map[string]string{}
	for _, role := range []string{resources.JournalVolume, resources.LedgerVolume} {
		name := b.PVCName(role, ordinal)
		mounted := false
		for _, volume := range pod.Spec.Volumes {
			if volume.PersistentVolumeClaim != nil && volume.PersistentVolumeClaim.ClaimName == name {
				mounted = true
			}
		}
		pvc := &corev1.PersistentVolumeClaim{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: b.C.Namespace, Name: name}, pvc); err != nil {
			return nil, err
		}
		if !mounted || !bookieClaimLabels(pvc, b.C.Name) || pvc.DeletionTimestamp != nil {
			return nil, fmt.Errorf("cannot establish decommission ownership of PVC %s", name)
		}
		for _, owner := range pvc.OwnerReferences {
			if owner.UID != sts.UID && owner.UID != b.C.UID {
				return nil, fmt.Errorf("PVC %s has an unrelated owner", name)
			}
		}
		identities[name] = string(pvc.UID)
	}
	return identities, nil
}

func (r *BookKeeperReconciler) deleteDecommissionPVC(ctx context.Context, b resources.BookKeeper, d *v1.DecommissionStatus, pvc *corev1.PersistentVolumeClaim) (bool, error) {
	key := client.ObjectKeyFromObject(pvc)
	if err := r.Get(ctx, key, pvc); err != nil {
		return apierrors.IsNotFound(err), client.IgnoreNotFound(err)
	}
	if d.PVCUIDs[pvc.Name] == "" || string(pvc.UID) != d.PVCUIDs[pvc.Name] || !bookieClaimLabels(pvc, b.C.Name) {
		return false, fmt.Errorf("refusing to delete PVC %s: decommission identity/ownership changed", pvc.Name)
	}
	if len(pvc.OwnerReferences) != 0 {
		sts := &appsv1.StatefulSet{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: b.C.Namespace, Name: b.StatefulSetName()}, sts); err != nil {
			return false, err
		}
		if !metav1.IsControlledBy(sts, b.C) {
			return false, fmt.Errorf("refusing PVC cleanup without the owned StatefulSet")
		}
		for _, owner := range pvc.OwnerReferences {
			if owner.UID != sts.UID && owner.UID != b.C.UID {
				return false, fmt.Errorf("PVC %s has an unrelated owner", pvc.Name)
			}
		}
	}
	if err := deleteObserved(ctx, r.Client, pvc, client.PropagationPolicy("Background")); err != nil {
		return false, err
	}
	err := r.Get(ctx, key, &corev1.PersistentVolumeClaim{})
	return apierrors.IsNotFound(err), client.IgnoreNotFound(err)
}
