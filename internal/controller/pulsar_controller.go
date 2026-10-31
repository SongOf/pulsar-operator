package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	v1 "github.com/SongOf/pulsar-operator/api/v1alpha1"
	"github.com/SongOf/pulsar-operator/internal/resources"
)

// PulsarReconciler reconciles a PulsarCluster.
type PulsarReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// Reconcile brings up, in order, ZooKeeper, the bookies, the cluster metadata, the brokers and the proxy. An image
// change rolls the components out in that same order, each after the previous one finished.
func (r *PulsarReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	pc := &v1.PulsarCluster{}
	if err := r.Get(ctx, req.NamespacedName, pc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if pc.Spec.Paused {
		return ctrl.Result{}, nil
	}
	status := pc.Status.DeepCopy()
	status.ObservedGeneration = pc.Generation
	res, err := r.reconcile(ctx, pc, status)
	if errors.Is(err, errRecreating) {
		res, err = ctrl.Result{RequeueAfter: time.Second}, nil
	}
	if err != nil {
		meta.SetStatusCondition(&status.Conditions, metav1.Condition{Type: v1.ConditionReady, Status: metav1.ConditionFalse, Reason: "ReconcileError", Message: err.Error(), ObservedGeneration: pc.Generation})
	}
	if !equalStatus(pc.Status, *status) {
		pc.Status = *status
		if uerr := client.IgnoreNotFound(r.Status().Update(ctx, pc)); uerr != nil && err == nil {
			err = uerr
		}
	}
	return res, err
}

func (r *PulsarReconciler) reconcile(ctx context.Context, pc *v1.PulsarCluster, status *v1.PulsarClusterStatus) (ctrl.Result, error) {
	a := applier{c: r.Client, scheme: r.Scheme, owner: pc}
	gen := pc.Generation
	p := resources.Pulsar{C: pc}
	status.ServiceURL, status.WebServiceURL = p.ServiceURL(), p.WebServiceURL()

	// Monitoring runs independently of the other components, so it also records a failing start.
	mon, err := reconcileMonitoring(ctx, a, resources.Monitoring{Cluster: pc.Name, Namespace: pc.Namespace, Domain: pc.Spec.ClusterDomain, Spec: pc.Spec.Monitoring})
	if err != nil {
		return ctrl.Result{}, err
	}
	status.Monitoring = mon

	// The ensemble size is fixed once created.
	zkKey := types.NamespacedName{Namespace: pc.Namespace, Name: p.ZooKeeperName()}
	existingZK, zkExists, err := getOptional(ctx, r.Client, zkKey, &appsv1.StatefulSet{})
	if err != nil {
		return ctrl.Result{}, err
	}
	if zkExists && ptr.Deref(existingZK.Spec.Replicas, 0) != p.ZooKeeperReplicas() {
		meta.SetStatusCondition(&status.Conditions, metav1.Condition{Type: v1.ConditionSpecRejected, Status: metav1.ConditionTrue, Reason: "ZooKeeperReplicasImmutable",
			Message: fmt.Sprintf("zookeeper.replicas cannot change from %d", ptr.Deref(existingZK.Spec.Replicas, 0)), ObservedGeneration: gen})
		fixed := pc.DeepCopy()
		fixed.Spec.ZooKeeper.Replicas = ptr.Deref(existingZK.Spec.Replicas, 0)
		p = resources.Pulsar{C: fixed}
	} else {
		meta.RemoveStatusCondition(&status.Conditions, v1.ConditionSpecRejected)
	}

	// 1. ZooKeeper.
	zkCM := p.ZooKeeperConfigMap()
	if err := a.configMap(ctx, zkCM); err != nil {
		return ctrl.Result{}, err
	}
	if err := a.service(ctx, p.ZooKeeperService()); err != nil {
		return ctrl.Result{}, err
	}
	zk, err := a.statefulSet(ctx, p.ZooKeeperStatefulSet(resources.Hash(zkCM.Data)), false)
	if err != nil {
		return ctrl.Result{}, err
	}
	for _, pdb := range p.PDBs() {
		if err := a.pdb(ctx, pdb); err != nil {
			return ctrl.Result{}, err
		}
	}
	status.ZooKeeper = stsStatus(zk)
	if !zkExists || zk.Status.ReadyReplicas < ptr.Deref(zk.Spec.Replicas, 0) && !metadataDone(status) {
		status.Phase = v1.PhaseInitializing
		r.setReady(status, gen, false, "WaitingForZooKeeper")
		return ctrl.Result{RequeueAfter: requeueBusy}, nil
	}

	// 2. Bookies, through the child BookKeeperCluster. An image change waits until ZooKeeper rolled out.
	child := p.ChildBookKeeper()
	existing := &v1.BookKeeperCluster{}
	childExists := true
	if err := r.Get(ctx, client.ObjectKeyFromObject(child), existing); err != nil {
		if client.IgnoreNotFound(err) != nil {
			return ctrl.Result{}, err
		}
		childExists = false
	}
	zkCurrent := status.ZooKeeper.RolledOut && status.ZooKeeper.Image == resources.ImageOf(pc.Spec.ZooKeeper.Image, pc.Spec.Image)
	if childExists && existing.Spec.Image != child.Spec.Image && !zkCurrent {
		child.Spec.Image = existing.Spec.Image
	}
	bk := &v1.BookKeeperCluster{ObjectMeta: metav1.ObjectMeta{Name: child.Name, Namespace: child.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, bk, func() error {
		bk.Labels = mergeMap(bk.Labels, child.Labels)
		bk.Spec = child.Spec
		return controllerutil.SetControllerReference(pc, bk, r.Scheme)
	}); err != nil {
		return ctrl.Result{}, err
	}
	status.BookKeeper = bk.Status.Bookies
	status.BookKeeperPhase = bk.Status.Phase
	bkRolledOut := bk.Status.ObservedGeneration == bk.Generation && meta.IsStatusConditionTrue(bk.Status.Conditions, v1.ConditionReady)
	bkCurrent := zkCurrent && bkRolledOut && bk.Spec.Image == child.Spec.Image
	if !meta.IsStatusConditionTrue(bk.Status.Conditions, v1.ConditionMetadataInitialized) {
		status.Phase = v1.PhaseInitializing
		r.setReady(status, gen, false, "WaitingForBookKeeperMetadata")
		return ctrl.Result{RequeueAfter: requeueBusy}, nil
	}

	// 3. Cluster metadata, once.
	job, err := a.job(ctx, p.MetadataJob())
	if err != nil {
		return ctrl.Result{}, err
	}
	switch {
	case jobSucceeded(job):
		meta.SetStatusCondition(&status.Conditions, metav1.Condition{Type: v1.ConditionMetadataInitialized, Status: metav1.ConditionTrue, Reason: "Initialized", ObservedGeneration: gen})
	case jobFailed(job):
		meta.SetStatusCondition(&status.Conditions, metav1.Condition{Type: v1.ConditionMetadataInitialized, Status: metav1.ConditionFalse, Reason: "JobFailed", Message: "job " + job.Name + " failed; delete it to retry", ObservedGeneration: gen})
		status.Phase = v1.PhaseFailed
		r.setReady(status, gen, false, "MetadataJobFailed")
		return ctrl.Result{RequeueAfter: requeueSteady}, nil
	default:
		meta.SetStatusCondition(&status.Conditions, metav1.Condition{Type: v1.ConditionMetadataInitialized, Status: metav1.ConditionFalse, Reason: "Initializing", ObservedGeneration: gen})
		status.Phase = v1.PhaseInitializing
		r.setReady(status, gen, false, "InitializingMetadata")
		return ctrl.Result{RequeueAfter: requeueBusy}, nil
	}

	// 4. Brokers. An image change waits until the bookies rolled out.
	brokerCM := p.BrokerConfigMap()
	if err := a.configMap(ctx, brokerCM); err != nil {
		return ctrl.Result{}, err
	}
	for _, svc := range []*corev1.Service{p.BrokerHeadlessService(), p.BrokerService()} {
		if err := a.service(ctx, svc); err != nil {
			return ctrl.Result{}, err
		}
	}
	brokerSTS := p.BrokerStatefulSet(resources.Hash(brokerCM.Data))
	if err := r.holdImage(ctx, brokerSTS, bkCurrent); err != nil {
		return ctrl.Result{}, err
	}
	broker, err := a.statefulSet(ctx, brokerSTS, false)
	if err != nil {
		return ctrl.Result{}, err
	}
	status.Broker = stsStatus(broker)

	// 5. Proxy. An image change waits until the brokers rolled out.
	proxyReady := true
	if pc.Spec.Proxy.Enabled {
		proxyCM := p.ProxyConfigMap()
		if err := a.configMap(ctx, proxyCM); err != nil {
			return ctrl.Result{}, err
		}
		if err := a.service(ctx, p.ProxyService()); err != nil {
			return ctrl.Result{}, err
		}
		dep := p.ProxyDeployment(resources.Hash(proxyCM.Data))
		brokerCurrent := bkCurrent && status.Broker.RolledOut && status.Broker.Image == resources.ImageOf(pc.Spec.Broker.Image, pc.Spec.Image)
		if err := r.holdDeploymentImage(ctx, dep, brokerCurrent); err != nil {
			return ctrl.Result{}, err
		}
		proxy, err := a.deployment(ctx, dep)
		if err != nil {
			return ctrl.Result{}, err
		}
		status.Proxy = deployStatus(proxy)
		proxyReady = status.Proxy.RolledOut
	} else {
		for _, obj := range []client.Object{
			&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: pc.Namespace, Name: p.ProxyName()}},
			&corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: pc.Namespace, Name: p.ProxyName()}},
			&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: pc.Namespace, Name: p.ProxyName() + "-config"}},
		} {
			if err := deleteIfExists(ctx, r.Client, obj); err != nil {
				return ctrl.Result{}, err
			}
		}
		status.Proxy = v1.ComponentStatus{}
	}

	ready := status.ZooKeeper.RolledOut && bkRolledOut && status.Broker.RolledOut && proxyReady
	switch {
	case bk.Status.Phase == v1.PhaseDecommissioning || bk.Status.Phase == v1.PhaseFailed:
		status.Phase = bk.Status.Phase
	case bk.Status.Phase == v1.PhaseScaling || broker.Status.Replicas != status.Broker.Replicas:
		status.Phase = v1.PhaseScaling
	case ready:
		status.Phase = v1.PhaseRunning
	default:
		status.Phase = v1.PhaseUpdating
	}
	r.setReady(status, gen, ready, string(status.Phase))
	if !ready {
		return ctrl.Result{RequeueAfter: requeueBusy}, nil
	}
	return ctrl.Result{RequeueAfter: requeueSteady}, nil
}

func metadataDone(s *v1.PulsarClusterStatus) bool {
	return meta.IsStatusConditionTrue(s.Conditions, v1.ConditionMetadataInitialized)
}

func (r *PulsarReconciler) setReady(s *v1.PulsarClusterStatus, gen int64, ready bool, reason string) {
	c := metav1.Condition{Type: v1.ConditionReady, Status: metav1.ConditionFalse, Reason: reason, ObservedGeneration: gen}
	if ready {
		c.Status, c.Reason = metav1.ConditionTrue, "AllComponentsReady"
	}
	meta.SetStatusCondition(&s.Conditions, c)
}

// holdImage keeps the running image of an existing StatefulSet until the components before it rolled out.
func (r *PulsarReconciler) holdImage(ctx context.Context, want *appsv1.StatefulSet, allow bool) error {
	if allow {
		return nil
	}
	got, ok, err := getOptional(ctx, r.Client, client.ObjectKeyFromObject(want), &appsv1.StatefulSet{})
	if err != nil || !ok {
		return err
	}
	if img := containerImage(got.Spec.Template.Spec); img != "" {
		want.Spec.Template.Spec.Containers[0].Image = img
	}
	return nil
}

func (r *PulsarReconciler) holdDeploymentImage(ctx context.Context, want *appsv1.Deployment, allow bool) error {
	if allow {
		return nil
	}
	got, ok, err := getOptional(ctx, r.Client, client.ObjectKeyFromObject(want), &appsv1.Deployment{})
	if err != nil || !ok {
		return err
	}
	if img := containerImage(got.Spec.Template.Spec); img != "" {
		want.Spec.Template.Spec.Containers[0].Image = img
	}
	return nil
}

// SetupWithManager registers the reconciler.
func (r *PulsarReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1.PulsarCluster{}).
		Owns(&v1.BookKeeperCluster{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&appsv1.Deployment{}).
		Owns(&batchv1.Job{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&corev1.Service{}).
		Owns(&policyv1.PodDisruptionBudget{}).
		Owns(&corev1.ServiceAccount{}).
		Owns(&rbacv1.Role{}).
		Owns(&rbacv1.RoleBinding{}).
		Named("pulsarcluster").
		Complete(r)
}
