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
	storagev1 "k8s.io/api/storage/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "github.com/SongOf/pulsar-operator/api/v1alpha1"
	"github.com/SongOf/pulsar-operator/internal/resources"
)

// BookKeeperReconciler reconciles a BookKeeperCluster.
type BookKeeperReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=pulsar.songof.dev,resources=bookkeeperclusters;pulsarclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=pulsar.songof.dev,resources=bookkeeperclusters/status;pulsarclusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps,resources=statefulsets;deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=configmaps;services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=persistentvolumeclaims,verbs=get;list;watch;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=events,verbs=create;patch;list;watch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=storage.k8s.io,resources=storageclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch;delete

const (
	requeueBusy   = 10 * time.Second
	requeueSteady = 60 * time.Second
)

// Reconcile drives the bookies toward the spec: metadata, configuration, storage, scale and auto-recovery.
func (r *BookKeeperReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	bk := &v1.BookKeeperCluster{}
	if err := r.Get(ctx, req.NamespacedName, bk); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if bk.Spec.Paused {
		return ctrl.Result{}, nil
	}
	status := bk.Status.DeepCopy()
	status.ObservedGeneration = bk.Generation
	res, err := r.reconcile(ctx, bk, status)
	if errors.Is(err, errRecreating) {
		res, err = ctrl.Result{RequeueAfter: time.Second}, nil
	}
	if err != nil {
		meta.SetStatusCondition(&status.Conditions, metav1.Condition{Type: v1.ConditionReady, Status: metav1.ConditionFalse, Reason: "ReconcileError", Message: err.Error(), ObservedGeneration: bk.Generation})
	}
	if uerr := r.updateStatus(ctx, bk, status); uerr != nil && err == nil {
		err = uerr
	}
	return res, err
}

func (r *BookKeeperReconciler) updateStatus(ctx context.Context, bk *v1.BookKeeperCluster, status *v1.BookKeeperClusterStatus) error {
	if equalStatus(bk.Status, *status) {
		return nil
	}
	bk.Status = *status
	return client.IgnoreNotFound(r.Status().Update(ctx, bk))
}

func (r *BookKeeperReconciler) reconcile(ctx context.Context, bk *v1.BookKeeperCluster, status *v1.BookKeeperClusterStatus) (ctrl.Result, error) {
	b := resources.BookKeeper{C: bk}
	a := applier{c: r.Client, scheme: r.Scheme, owner: bk}
	gen := bk.Generation

	cm := b.ConfigMap()
	if err := a.configMap(ctx, cm); err != nil {
		return ctrl.Result{}, err
	}
	hash := resources.Hash(cm.Data)
	if err := a.service(ctx, b.HeadlessService()); err != nil {
		return ctrl.Result{}, err
	}
	if err := a.pdb(ctx, b.PodDisruptionBudget()); err != nil {
		return ctrl.Result{}, err
	}
	// A BookKeeperCluster owned by a PulsarCluster is scraped by that cluster's Prometheus.
	if !ownedByPulsar(bk) {
		mon, err := reconcileMonitoring(ctx, a, resources.Monitoring{Cluster: bk.Name, Namespace: bk.Namespace, Domain: bk.Spec.ClusterDomain, Spec: bk.Spec.Monitoring})
		if err != nil {
			return ctrl.Result{}, err
		}
		status.Monitoring = mon
	}

	// 1. BookKeeper metadata must exist before any bookie starts.
	initJob, err := a.job(ctx, b.InitJob())
	if err != nil {
		return ctrl.Result{}, err
	}
	switch {
	case jobSucceeded(initJob):
		meta.SetStatusCondition(&status.Conditions, metav1.Condition{Type: v1.ConditionMetadataInitialized, Status: metav1.ConditionTrue, Reason: "Initialized", ObservedGeneration: gen})
	case jobFailed(initJob):
		meta.SetStatusCondition(&status.Conditions, metav1.Condition{Type: v1.ConditionMetadataInitialized, Status: metav1.ConditionFalse, Reason: "JobFailed", Message: "job " + initJob.Name + " failed; delete it to retry", ObservedGeneration: gen})
		status.Phase = v1.PhaseFailed
		return ctrl.Result{RequeueAfter: requeueSteady}, nil
	default:
		meta.SetStatusCondition(&status.Conditions, metav1.Condition{Type: v1.ConditionMetadataInitialized, Status: metav1.ConditionFalse, Reason: "Initializing", ObservedGeneration: gen})
		status.Phase = v1.PhaseInitializing
		return ctrl.Result{RequeueAfter: requeueBusy}, nil
	}

	current, exists, err := getOptional(ctx, r.Client, types.NamespacedName{Namespace: bk.Namespace, Name: b.StatefulSetName()}, &appsv1.StatefulSet{})
	if err != nil {
		return ctrl.Result{}, err
	}
	currentReplicas := bk.Spec.Replicas
	if exists {
		currentReplicas = ptr.Deref(current.Spec.Replicas, 0)
	}

	// 2. Storage: expand PVCs in place, refuse shrinking and class changes.
	recreate, err := r.reconcileStorage(ctx, b, status, currentReplicas)
	if err != nil {
		return ctrl.Result{}, err
	}

	// 3. Scale, one decommissioned bookie at a time.
	replicas, busy, err := r.reconcileScale(ctx, b, a, status, currentReplicas, exists)
	if err != nil {
		return ctrl.Result{}, err
	}

	sts, err := a.statefulSet(ctx, b.StatefulSet(replicas, hash), recreate)
	if err != nil {
		return ctrl.Result{}, err
	}
	status.Bookies = stsStatus(sts)

	// 4. Auto-recovery.
	ar := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: b.AutoRecoveryName(), Namespace: bk.Namespace}}
	if b.AutoRecoveryEnabled() {
		if ar, err = a.deployment(ctx, b.AutoRecovery(hash)); err != nil {
			return ctrl.Result{}, err
		}
		status.AutoRecovery = deployStatus(ar)
	} else {
		if err := deleteIfExists(ctx, r.Client, ar); err != nil {
			return ctrl.Result{}, err
		}
		status.AutoRecovery = v1.ComponentStatus{}
	}

	if err := r.observeBookies(ctx, b, status, replicas); err != nil {
		return ctrl.Result{}, err
	}

	ready := status.Bookies.RolledOut && (!b.AutoRecoveryEnabled() || status.AutoRecovery.RolledOut)
	switch {
	case status.Decommission != nil && status.Decommission.Phase == v1.DecommissionFailed:
		status.Phase = v1.PhaseFailed
	case status.Decommission != nil:
		status.Phase = v1.PhaseDecommissioning
	case replicas != bk.Spec.Replicas && !meta.IsStatusConditionTrue(status.Conditions, v1.ConditionScaleInBlocked):
		status.Phase = v1.PhaseScaling
	case status.Bookies.ReadyReplicas < status.Bookies.Replicas && status.Bookies.UpdatedReplicas == status.Bookies.Replicas:
		status.Phase = v1.PhaseDegraded
	case !ready:
		status.Phase = v1.PhaseUpdating
	default:
		status.Phase = v1.PhaseRunning
	}
	readyCond := metav1.Condition{Type: v1.ConditionReady, Status: metav1.ConditionFalse, Reason: string(status.Phase), ObservedGeneration: gen}
	if ready && status.Decommission == nil {
		readyCond.Status, readyCond.Reason = metav1.ConditionTrue, "AllBookiesReady"
	}
	meta.SetStatusCondition(&status.Conditions, readyCond)
	if busy || !ready {
		return ctrl.Result{RequeueAfter: requeueBusy}, nil
	}
	return ctrl.Result{RequeueAfter: requeueSteady}, nil
}

// reconcileScale returns the StatefulSet replicas to apply. A scale-in stops the highest bookie, waits for its
// ledgers to be re-replicated by `decommissionbookie`, deletes its PVCs, then moves to the next.
func (r *BookKeeperReconciler) reconcileScale(ctx context.Context, b resources.BookKeeper, a applier, status *v1.BookKeeperClusterStatus, current int32, exists bool) (int32, bool, error) {
	bk := b.C
	gen := bk.Generation
	if !exists {
		meta.RemoveStatusCondition(&status.Conditions, v1.ConditionScaleInBlocked)
		return bk.Spec.Replicas, false, nil
	}
	if d := status.Decommission; d != nil {
		return r.stepDecommission(ctx, b, a, status, d)
	}
	desired := bk.Spec.Replicas
	if desired >= current {
		meta.RemoveStatusCondition(&status.Conditions, v1.ConditionScaleInBlocked)
		return desired, desired != current, nil
	}
	floor := bk.Spec.MinReplicas
	if desired < floor {
		meta.SetStatusCondition(&status.Conditions, metav1.Condition{Type: v1.ConditionScaleInBlocked, Status: metav1.ConditionTrue, Reason: "BelowMinReplicas",
			Message: fmt.Sprintf("replicas %d is below minReplicas %d; scale-in stops at %d", desired, floor, max(floor, 0)), ObservedGeneration: gen})
		if current <= floor {
			return current, false, nil
		}
	} else {
		meta.RemoveStatusCondition(&status.Conditions, v1.ConditionScaleInBlocked)
	}
	// Only one bookie leaves at a time, and only from a healthy cluster: a bookie that is not ready may hold the
	// only remaining copy of some ledger fragments.
	sts := &appsv1.StatefulSet{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: bk.Namespace, Name: b.StatefulSetName()}, sts); err != nil {
		return current, false, err
	}
	if st := sts.Status; st.ObservedGeneration < sts.Generation || st.Replicas != current || st.ReadyReplicas < current {
		return current, true, nil
	}
	ordinal := current - 1
	status.Decommission = &v1.DecommissionStatus{Ordinal: ordinal, BookieID: b.BookieID(ordinal), Phase: v1.DecommissionStopping, StartedAt: metav1.Now(),
		Message: "stopping the bookie before re-replicating its ledgers"}
	r.event(bk, corev1.EventTypeNormal, "DecommissionStarted", "decommissioning bookie %s", status.Decommission.BookieID)
	return ordinal, true, nil
}

func (r *BookKeeperReconciler) stepDecommission(ctx context.Context, b resources.BookKeeper, a applier, status *v1.BookKeeperClusterStatus, d *v1.DecommissionStatus) (int32, bool, error) {
	bk := b.C
	switch d.Phase {
	case v1.DecommissionStopping:
		pod := &corev1.Pod{}
		err := r.Get(ctx, types.NamespacedName{Namespace: bk.Namespace, Name: b.PodName(d.Ordinal)}, pod)
		if err == nil {
			return d.Ordinal, true, nil
		}
		if !apierrors.IsNotFound(err) {
			return d.Ordinal, true, err
		}
		job, err := a.job(ctx, b.DecommissionJob(d.Ordinal))
		if err != nil {
			return d.Ordinal, true, err
		}
		d.Job, d.Phase, d.Message = job.Name, v1.DecommissionReplicating, "re-replicating the bookie's ledgers"
	case v1.DecommissionReplicating, v1.DecommissionFailed:
		job := &batchv1.Job{}
		err := r.Get(ctx, types.NamespacedName{Namespace: bk.Namespace, Name: b.DecommissionJobName(d.Ordinal)}, job)
		if apierrors.IsNotFound(err) {
			if _, err := a.job(ctx, b.DecommissionJob(d.Ordinal)); err != nil {
				return d.Ordinal, true, err
			}
			d.Phase, d.Message = v1.DecommissionReplicating, "re-replicating the bookie's ledgers"
			return d.Ordinal, true, nil
		}
		if err != nil {
			return d.Ordinal, true, err
		}
		switch {
		case jobSucceeded(job):
			d.Phase, d.Message = v1.DecommissionCleaning, "deleting the bookie's PVCs"
		case jobFailed(job):
			if d.Phase != v1.DecommissionFailed {
				r.event(bk, corev1.EventTypeWarning, "DecommissionFailed", "decommission job %s failed; retrying in 5 minutes", job.Name)
			}
			d.Phase, d.Message = v1.DecommissionFailed, "job "+job.Name+" failed; it is retried every 5 minutes, check its logs"
			if job.Status.CompletionTime == nil && time.Since(lastJobTransition(job)) > 5*time.Minute {
				if err := deleteIfExists(ctx, r.Client, job); err != nil {
					return d.Ordinal, true, err
				}
			}
		}
	}
	if d.Phase == v1.DecommissionCleaning {
		for _, vol := range []string{resources.JournalVolume, resources.LedgerVolume} {
			pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Namespace: bk.Namespace, Name: b.PVCName(vol, d.Ordinal)}}
			if err := deleteIfExists(ctx, r.Client, pvc); err != nil {
				return d.Ordinal, true, err
			}
		}
		if err := deleteIfExists(ctx, r.Client, &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Namespace: bk.Namespace, Name: b.DecommissionJobName(d.Ordinal)}}); err != nil {
			return d.Ordinal, true, err
		}
		r.event(bk, corev1.EventTypeNormal, "DecommissionCompleted", "bookie %s decommissioned and its PVCs deleted", d.BookieID)
		status.Decommission = nil
	}
	return d.Ordinal, true, nil
}

func lastJobTransition(j *batchv1.Job) time.Time {
	var t time.Time
	for _, c := range j.Status.Conditions {
		if c.LastTransitionTime.After(t) {
			t = c.LastTransitionTime.Time
		}
	}
	return t
}

// reconcileStorage expands existing bookie PVCs to the requested sizes when their StorageClass allows it. It
// returns whether the StatefulSet may be recreated to pick up new volume claim templates.
func (r *BookKeeperReconciler) reconcileStorage(ctx context.Context, b resources.BookKeeper, status *v1.BookKeeperClusterStatus, replicas int32) (bool, error) {
	bk := b.C
	gen := bk.Generation
	want := map[string]v1.VolumeSpec{resources.JournalVolume: bk.Spec.Storage.Journal, resources.LedgerVolume: bk.Spec.Storage.Ledger}
	var rejected, unsupported, resizing []string
	expandable := true
	seen := false
	for vol, spec := range want {
		for i := int32(0); i < replicas; i++ {
			pvc := &corev1.PersistentVolumeClaim{}
			err := r.Get(ctx, types.NamespacedName{Namespace: bk.Namespace, Name: b.PVCName(vol, i)}, pvc)
			if apierrors.IsNotFound(err) {
				continue
			}
			if err != nil {
				return false, err
			}
			seen = true
			if sc := ptr.Deref(spec.StorageClassName, ""); sc != "" && sc != ptr.Deref(pvc.Spec.StorageClassName, "") {
				rejected = append(rejected, fmt.Sprintf("%s: storageClassName cannot change from %q", pvc.Name, ptr.Deref(pvc.Spec.StorageClassName, "")))
				continue
			}
			requested := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
			switch requested.Cmp(spec.Size) {
			case 1:
				rejected = append(rejected, fmt.Sprintf("%s: cannot shrink from %s to %s", pvc.Name, requested.String(), spec.Size.String()))
			case -1:
				ok, err := r.expandable(ctx, pvc)
				if err != nil {
					return false, err
				}
				if !ok {
					expandable = false
					unsupported = append(unsupported, pvc.Name)
					continue
				}
				patch := client.MergeFrom(pvc.DeepCopy())
				pvc.Spec.Resources.Requests[corev1.ResourceStorage] = spec.Size
				if err := r.Patch(ctx, pvc, patch); err != nil {
					return false, err
				}
				resizing = append(resizing, pvc.Name)
				r.event(bk, corev1.EventTypeNormal, "VolumeExpanding", "expanding %s from %s to %s", pvc.Name, requested.String(), spec.Size.String())
			default:
				if capacity, ok := pvc.Status.Capacity[corev1.ResourceStorage]; ok && capacity.Cmp(spec.Size) < 0 {
					resizing = append(resizing, pvc.Name)
				}
			}
		}
	}
	if seen {
		status.ExpansionSupported = ptr.To(expandable)
	}
	switch {
	case len(rejected) > 0:
		meta.SetStatusCondition(&status.Conditions, metav1.Condition{Type: v1.ConditionSpecRejected, Status: metav1.ConditionTrue, Reason: "StorageChangeRejected", Message: joinMax(rejected), ObservedGeneration: gen})
		return false, nil
	default:
		meta.RemoveStatusCondition(&status.Conditions, v1.ConditionSpecRejected)
	}
	switch {
	case len(unsupported) > 0:
		meta.SetStatusCondition(&status.Conditions, metav1.Condition{Type: v1.ConditionStorageResized, Status: metav1.ConditionFalse, Reason: "ExpansionNotSupported",
			Message: "the StorageClass does not allow volume expansion; new bookies get the new size, existing PVCs keep theirs: " + joinMax(unsupported), ObservedGeneration: gen})
	case len(resizing) > 0:
		meta.SetStatusCondition(&status.Conditions, metav1.Condition{Type: v1.ConditionStorageResized, Status: metav1.ConditionFalse, Reason: "Resizing", Message: joinMax(resizing), ObservedGeneration: gen})
	default:
		meta.SetStatusCondition(&status.Conditions, metav1.Condition{Type: v1.ConditionStorageResized, Status: metav1.ConditionTrue, Reason: "SizesMatch", ObservedGeneration: gen})
	}
	return true, nil
}

func (r *BookKeeperReconciler) expandable(ctx context.Context, pvc *corev1.PersistentVolumeClaim) (bool, error) {
	name := ptr.Deref(pvc.Spec.StorageClassName, "")
	if name == "" {
		return false, nil
	}
	sc := &storagev1.StorageClass{}
	if err := r.Get(ctx, types.NamespacedName{Name: name}, sc); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	return ptr.Deref(sc.AllowVolumeExpansion, false), nil
}

// observeBookies reports each bookie with its readiness and PVCs.
func (r *BookKeeperReconciler) observeBookies(ctx context.Context, b resources.BookKeeper, status *v1.BookKeeperClusterStatus, replicas int32) error {
	bk := b.C
	n := replicas
	if d := status.Decommission; d != nil && d.Ordinal+1 > n {
		n = d.Ordinal + 1
	}
	details := make([]v1.BookieStatus, 0, n)
	for i := int32(0); i < n; i++ {
		bs := v1.BookieStatus{Name: b.PodName(i), BookieID: b.BookieID(i)}
		pod := &corev1.Pod{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: bk.Namespace, Name: bs.Name}, pod); err == nil {
			bs.Ready = podReady(pod)
		} else if !apierrors.IsNotFound(err) {
			return err
		}
		var err error
		if bs.Journal, err = r.volumeStatus(ctx, bk.Namespace, b.PVCName(resources.JournalVolume, i)); err != nil {
			return err
		}
		if bs.Ledger, err = r.volumeStatus(ctx, bk.Namespace, b.PVCName(resources.LedgerVolume, i)); err != nil {
			return err
		}
		details = append(details, bs)
	}
	status.BookieDetails = details
	return nil
}

func (r *BookKeeperReconciler) volumeStatus(ctx context.Context, ns, name string) (v1.VolumeStatus, error) {
	vs := v1.VolumeStatus{Name: name}
	pvc := &corev1.PersistentVolumeClaim{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, pvc); err != nil {
		return vs, client.IgnoreNotFound(err)
	}
	vs.Phase = string(pvc.Status.Phase)
	req := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	vs.Requested = req.String()
	if c, ok := pvc.Status.Capacity[corev1.ResourceStorage]; ok {
		vs.Capacity = c.String()
		vs.Resizing = c.Cmp(req) < 0
	}
	for _, c := range pvc.Status.Conditions {
		if c.Type == corev1.PersistentVolumeClaimResizing || c.Type == corev1.PersistentVolumeClaimFileSystemResizePending {
			vs.Resizing = true
		}
	}
	return vs, nil
}

func podReady(p *corev1.Pod) bool {
	if p.DeletionTimestamp != nil {
		return false
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func (r *BookKeeperReconciler) event(obj runtime.Object, kind, reason, format string, args ...any) {
	if r.Recorder != nil {
		r.Recorder.Eventf(obj, kind, reason, format, args...)
	}
}

func joinMax(items []string) string {
	const limit = 5
	out := ""
	for i, s := range items {
		if i == limit {
			return out + fmt.Sprintf("; and %d more", len(items)-limit)
		}
		if i > 0 {
			out += "; "
		}
		out += s
	}
	return out
}

// SetupWithManager registers the reconciler.
func (r *BookKeeperReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1.BookKeeperCluster{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&appsv1.Deployment{}).
		Owns(&batchv1.Job{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&corev1.Service{}).
		Owns(&policyv1.PodDisruptionBudget{}).
		Named("bookkeepercluster").
		Complete(r)
}


func equalStatus(a, b any) bool { return apiequality.Semantic.DeepEqual(a, b) }

func ownedByPulsar(bk *v1.BookKeeperCluster) bool {
	for _, ref := range bk.OwnerReferences {
		if ref.Kind == "PulsarCluster" && ref.Controller != nil && *ref.Controller {
			return true
		}
	}
	return false
}
