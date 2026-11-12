package controller

import (
	"context"
	"errors"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "github.com/SongOf/pulsar-operator/api/v1alpha1"
)

type storageChangeRejected struct{ message string }

func (e *storageChangeRejected) Error() string { return e.message }

func markStorageRejection(conditions *[]metav1.Condition, generation int64, err error) {
	var rejected *storageChangeRejected
	if errors.As(err, &rejected) {
		meta.SetStatusCondition(conditions, metav1.Condition{Type: v1.ConditionSpecRejected,
			Status: metav1.ConditionTrue, Reason: "StorageChangeRejected", Message: rejected.Error(), ObservedGeneration: generation})
	}
}

func sameStorageClass(a, b *string) bool {
	return (a == nil) == (b == nil) && ptr.Deref(a, "") == ptr.Deref(b, "")
}

// ZooKeeper and Prometheus do not implement a volume expansion/migration state
// machine. Fail explicitly before changing workloads (notably retention limits).
// Also check retained PVCs when monitoring has been disabled and re-enabled.
func (a applier) requireUnchangedStorage(ctx context.Context, want *appsv1.StatefulSet) error {
	got := &appsv1.StatefulSet{}
	err := a.c.Get(ctx, client.ObjectKeyFromObject(want), got)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	exists := err == nil
	if exists {
		if err := a.requireOwned(got); err != nil {
			return err
		}
	}
	reject := func(claim string) error {
		return &storageChangeRejected{message: fmt.Sprintf("storage size/class changes for %s/%s (%s) are not supported; restore the existing layout or perform an explicit data migration", want.Namespace, want.Name, claim)}
	}
	for _, desired := range want.Spec.VolumeClaimTemplates {
		wantSize := desired.Spec.Resources.Requests[corev1.ResourceStorage]
		if exists {
			found := false
			for _, original := range got.Spec.VolumeClaimTemplates {
				if original.Name != desired.Name {
					continue
				}
				found = true
				originalSize := original.Spec.Resources.Requests[corev1.ResourceStorage]
				if originalSize.Cmp(wantSize) != 0 || !sameStorageClass(original.Spec.StorageClassName, desired.Spec.StorageClassName) {
					return reject(desired.Name)
				}
			}
			if !found {
				return reject(desired.Name)
			}
		}
		for i := int32(0); i < ptr.Deref(want.Spec.Replicas, 1); i++ {
			pvc := &corev1.PersistentVolumeClaim{}
			key := client.ObjectKey{Namespace: want.Namespace, Name: fmt.Sprintf("%s-%s-%d", desired.Name, want.Name, i)}
			if err := a.c.Get(ctx, key, pvc); err != nil {
				if apierrors.IsNotFound(err) {
					continue
				}
				return err
			}
			actual := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
			// A nil template class is resolved by default-class admission on the PVC.
			if actual.Cmp(wantSize) != 0 || desired.Spec.StorageClassName != nil && !sameStorageClass(desired.Spec.StorageClassName, pvc.Spec.StorageClassName) {
				return reject(pvc.Name)
			}
		}
	}
	return nil
}
