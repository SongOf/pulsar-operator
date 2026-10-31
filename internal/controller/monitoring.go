package controller

import (
	"context"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	v1 "github.com/SongOf/pulsar-operator/api/v1alpha1"
	"github.com/SongOf/pulsar-operator/internal/resources"
)

// +kubebuilder:rbac:groups=core,resources=serviceaccounts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles;rolebindings,verbs=get;list;watch;create;update;patch;delete

// reconcileMonitoring deploys or removes the cluster's Prometheus and reports where its metrics are. Turning
// monitoring off deletes Prometheus but keeps its PVC, so turning it on again keeps the history.
func reconcileMonitoring(ctx context.Context, a applier, m resources.Monitoring) (v1.MonitoringStatus, error) {
	status := v1.MonitoringStatus{URL: m.URL()}
	if !m.Deployed() {
		for _, obj := range []client.Object{
			&appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Namespace: m.Namespace, Name: m.Name()}},
			&corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: m.Namespace, Name: m.Name()}},
			&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: m.Namespace, Name: m.Name() + "-config"}},
			&rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Namespace: m.Namespace, Name: m.Name()}},
			&rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Namespace: m.Namespace, Name: m.Name()}},
			&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: m.Namespace, Name: m.Name()}},
		} {
			if err := deleteIfExists(ctx, a.c, obj); err != nil {
				return status, err
			}
		}
		status.Ready = m.Spec.PrometheusURL != ""
		return status, nil
	}
	sa := m.ServiceAccount()
	if _, err := controllerutil.CreateOrUpdate(ctx, a.c, sa, func() error { sa.Labels = mergeMap(sa.Labels, m.ServiceAccount().Labels); return a.own(sa) }); err != nil {
		return status, err
	}
	want := m.Role()
	role := &rbacv1.Role{ObjectMeta: want.ObjectMeta}
	if _, err := controllerutil.CreateOrUpdate(ctx, a.c, role, func() error { role.Rules = want.Rules; role.Labels = want.Labels; return a.own(role) }); err != nil {
		return status, err
	}
	wantRB := m.RoleBinding()
	rb := &rbacv1.RoleBinding{ObjectMeta: wantRB.ObjectMeta}
	if _, err := controllerutil.CreateOrUpdate(ctx, a.c, rb, func() error {
		if rb.CreationTimestamp.IsZero() {
			rb.RoleRef = wantRB.RoleRef
		}
		rb.Subjects, rb.Labels = wantRB.Subjects, wantRB.Labels
		return a.own(rb)
	}); err != nil {
		return status, err
	}
	cm := m.ConfigMap()
	if err := a.configMap(ctx, cm); err != nil {
		return status, err
	}
	if err := a.service(ctx, m.Service()); err != nil {
		return status, err
	}
	sts, err := a.statefulSet(ctx, m.StatefulSet(resources.Hash(cm.Data)), false)
	if err != nil {
		return status, err
	}
	status.Ready = sts.Status.ReadyReplicas == ptr.Deref(sts.Spec.Replicas, 1)
	return status, nil
}
