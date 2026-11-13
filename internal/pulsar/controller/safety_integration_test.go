//go:build integration

package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "github.com/SongOf/pulsar-operator/api/pulsar/v1alpha1"
	"github.com/SongOf/pulsar-operator/internal/pulsar/resources"
)

type checkpointClient struct {
	client.Client
	statusConflict bool
	beforePatch    func(client.Object) error
}
type checkpointStatusWriter struct {
	client.SubResourceWriter
	conflict bool
}

func (c *checkpointClient) Status() client.SubResourceWriter {
	return &checkpointStatusWriter{c.Client.Status(), c.statusConflict}
}
func (w *checkpointStatusWriter) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	if w.conflict {
		return apierrors.NewConflict(schema.GroupResource{Group: "pulsar.songof.dev", Resource: "bookkeeperclusters"}, obj.GetName(), fmt.Errorf("injected status conflict"))
	}
	return w.SubResourceWriter.Update(ctx, obj, opts...)
}
func (c *checkpointClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	if c.beforePatch != nil {
		if err := c.beforePatch(obj); err != nil {
			return err
		}
	}
	return c.Client.Patch(ctx, obj, patch, opts...)
}

func scaleFixture(t *testing.T) string {
	t.Helper()
	ns := namespace(t)
	runningBK(t, ns, 4, nil)
	for i := 0; i < 4; i++ {
		createPod(t, ns, fmt.Sprintf("bk-bookie-%d", i))
		boundPVC(t, ns, fmt.Sprintf("journal-bk-bookie-%d", i), "1Gi", nil)
		boundPVC(t, ns, fmt.Sprintf("ledgers-bk-bookie-%d", i), "2Gi", nil)
	}
	setReplicas(t, ns, 2)
	return ns
}
func runBK(r *BookKeeperReconciler, ns string) error {
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "bk"}})
	return err
}

func TestIntegrationDecommissionCheckpointConflictKeepsReplicas(t *testing.T) {
	ns := scaleFixture(t)
	r := bkReconciler()
	r.Client = &checkpointClient{Client: k8s, statusConflict: true}
	if err := runBK(r, ns); !apierrors.IsConflict(err) {
		t.Fatalf("expected conflict, got %v", err)
	}
	if got := *get(t, ns, "bk-bookie", &appsv1.StatefulSet{}).Spec.Replicas; got != 4 {
		t.Fatalf("scaled to %d before checkpoint", got)
	}
	if get(t, ns, "bk", &v1.BookKeeperCluster{}).Status.Decommission != nil {
		t.Fatal("failed checkpoint was persisted")
	}
	checked := false
	r.Client = &checkpointClient{Client: k8s, beforePatch: func(obj client.Object) error {
		if sts, ok := obj.(*appsv1.StatefulSet); ok && *sts.Spec.Replicas == 3 {
			d := get(t, ns, "bk", &v1.BookKeeperCluster{}).Status.Decommission
			if d == nil || d.Ordinal != 3 || len(d.PVCUIDs) != 2 {
				t.Fatalf("missing durable checkpoint before scale: %+v", d)
			}
			checked = true
		}
		return nil
	}}
	if err := runBK(r, ns); err != nil {
		t.Fatal(err)
	}
	if !checked {
		t.Fatal("scale-down was not observed")
	}
}

func TestIntegrationDecommissionResumesAfterCheckpointButFailedScale(t *testing.T) {
	ns := scaleFixture(t)
	r := bkReconciler()
	r.Client = &checkpointClient{Client: k8s, beforePatch: func(obj client.Object) error {
		if sts, ok := obj.(*appsv1.StatefulSet); ok && *sts.Spec.Replicas == 3 {
			return fmt.Errorf("injected scale failure")
		}
		return nil
	}}
	if err := runBK(r, ns); err == nil {
		t.Fatal("scale failure missing")
	}
	if d := get(t, ns, "bk", &v1.BookKeeperCluster{}).Status.Decommission; d == nil || d.Ordinal != 3 {
		t.Fatalf("intent lost: %+v", d)
	}
	if *get(t, ns, "bk-bookie", &appsv1.StatefulSet{}).Spec.Replicas != 4 {
		t.Fatal("failed patch changed replicas")
	}
	reconcileBK(t, ns, "bk")
	if *get(t, ns, "bk-bookie", &appsv1.StatefulSet{}).Spec.Replicas != 3 {
		t.Fatal("did not resume recorded ordinal")
	}
}

func TestIntegrationOrphanRecreatePreservesDecommissionReplicaBoundary(t *testing.T) {
	ns := scaleFixture(t)
	reconcileBK(t, ns, "bk")
	setStorage(t, ns, "1Gi", "5Gi")
	reconcileBK(t, ns, "bk")
	sts := get(t, ns, "bk-bookie", &appsv1.StatefulSet{})
	if sts.DeletionTimestamp == nil {
		t.Fatal("expected orphan recreation")
	}
	gcOrphan(t, sts)
	reconcileBK(t, ns, "bk")
	sts = get(t, ns, "bk-bookie", &appsv1.StatefulSet{})
	d := get(t, ns, "bk", &v1.BookKeeperCluster{}).Status.Decommission
	if *sts.Spec.Replicas != 3 || d == nil || d.Ordinal != 3 {
		t.Fatalf("lost boundary: replicas=%d state=%+v", *sts.Spec.Replicas, d)
	}
}

func TestIntegrationLegacyDecommissionWithoutIdentitiesStopsSafely(t *testing.T) {
	ns := scaleFixture(t)
	bk := get(t, ns, "bk", &v1.BookKeeperCluster{})
	bk.Status.Decommission = &v1.DecommissionStatus{Ordinal: 3, BookieID: "legacy", Phase: v1.DecommissionReplicating, StartedAt: metav1.Now()}
	if err := k8s.Status().Update(context.Background(), bk); err != nil {
		t.Fatal(err)
	}
	if err := runBK(bkReconciler(), ns); err == nil || !strings.Contains(err.Error(), "durable PVC") {
		t.Fatalf("expected legacy recovery error: %v", err)
	}
	if *get(t, ns, "bk-bookie", &appsv1.StatefulSet{}).Spec.Replicas != 4 {
		t.Fatal("legacy operation changed replicas")
	}
	get(t, ns, "ledgers-bk-bookie-3", &corev1.PersistentVolumeClaim{})
}

func TestIntegrationDecommissionDoesNotDeleteReplacementPVC(t *testing.T) {
	ns := scaleFixture(t)
	reconcileBK(t, ns, "bk")
	deletePod(t, ns, "bk-bookie-3")
	reconcileBK(t, ns, "bk")
	completeJob(t, ns, "bk-bookie-decommission-3")
	old := get(t, ns, "ledgers-bk-bookie-3", &corev1.PersistentVolumeClaim{})
	if err := k8s.Delete(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	// Complete the externally requested deletion; envtest has no garbage collector.
	remaining := &corev1.PersistentVolumeClaim{}
	if err := k8s.Get(context.Background(), client.ObjectKeyFromObject(old), remaining); err == nil {
		if remaining.DeletionTimestamp == nil {
			t.Fatal("external PVC delete did not start")
		}
		gcOrphan(t, remaining)
	} else if !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
	replacement := old.DeepCopy()
	replacement.ObjectMeta = metav1.ObjectMeta{Name: old.Name, Namespace: ns, Labels: old.Labels}
	replacement.Status = corev1.PersistentVolumeClaimStatus{}
	if err := k8s.Create(context.Background(), replacement); err != nil {
		t.Fatal(err)
	}
	if err := runBK(bkReconciler(), ns); err == nil || !strings.Contains(err.Error(), "identity/ownership") {
		t.Fatalf("replacement cleanup was not rejected: %v", err)
	}
	got := get(t, ns, old.Name, &corev1.PersistentVolumeClaim{})
	if got.UID != replacement.UID || got.DeletionTimestamp != nil {
		t.Fatal("replacement PVC was deleted")
	}
	d := get(t, ns, "bk", &v1.BookKeeperCluster{}).Status.Decommission
	if d == nil || d.Phase != v1.DecommissionCleaning || d.PVCUIDs[old.Name] != string(old.UID) {
		t.Fatalf("cleanup checkpoint lost: %+v", d)
	}
}

func TestIntegrationCleaningCheckpointConflictKeepsBothPVCs(t *testing.T) {
	ns := scaleFixture(t)
	reconcileBK(t, ns, "bk")
	deletePod(t, ns, "bk-bookie-3")
	reconcileBK(t, ns, "bk")
	completeJob(t, ns, "bk-bookie-decommission-3")
	r := bkReconciler()
	r.Client = &checkpointClient{Client: k8s, statusConflict: true}
	if err := runBK(r, ns); !apierrors.IsConflict(err) {
		t.Fatalf("expected cleaning checkpoint conflict: %v", err)
	}
	for _, role := range []string{"journal", "ledgers"} {
		pvc := get(t, ns, role+"-bk-bookie-3", &corev1.PersistentVolumeClaim{})
		if pvc.DeletionTimestamp != nil {
			t.Fatal("PVC deleted before cleaning checkpoint")
		}
	}
	if d := get(t, ns, "bk", &v1.BookKeeperCluster{}).Status.Decommission; d == nil || d.Phase != v1.DecommissionReplicating {
		t.Fatalf("failed cleaning checkpoint changed durable state: %+v", d)
	}
}

func TestIntegrationStatefulSetRecreationRejectsForeignOrUnownedObjects(t *testing.T) {
	for _, owned := range []bool{false, true} {
		t.Run(fmt.Sprint(owned), func(t *testing.T) {
			ns := namespace(t)
			bk := runningBK(t, ns, 2, nil)
			sts := get(t, ns, "bk-bookie", &appsv1.StatefulSet{})
			sts.OwnerReferences = nil
			if owned {
				sts.OwnerReferences = []metav1.OwnerReference{{APIVersion: "pulsar.songof.dev/v1alpha1", Kind: "BookKeeperCluster", Name: "foreign", UID: "foreign", Controller: ptr.To(true)}}
			}
			if err := k8s.Update(context.Background(), sts); err != nil {
				t.Fatal(err)
			}
			bk.Spec.Storage.Ledger.Size = resource.MustParse("8Gi")
			b := resources.BookKeeper{C: bk}
			a := applier{c: k8s, scheme: scheme, owner: bk}
			if _, err := a.statefulSet(context.Background(), b.StatefulSet(2, "x"), true); err == nil {
				t.Fatal("foreign object accepted")
			}
			if got := get(t, ns, sts.Name, &appsv1.StatefulSet{}); got.UID != sts.UID || got.DeletionTimestamp != nil {
				t.Fatal("foreign object deleted")
			}
		})
	}
}

type replaceAfterReadClient struct {
	client.Client
	replaced bool
}

func (c *replaceAfterReadClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if err := c.Client.Get(ctx, key, obj, opts...); err != nil {
		return err
	}
	if _, ok := obj.(*corev1.ConfigMap); ok && !c.replaced {
		c.replaced = true
		if err := c.Client.Delete(ctx, obj); err != nil {
			return err
		}
		return c.Client.Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace}})
	}
	return nil
}

func TestIntegrationDeleteOwnedUsesUIDPreconditions(t *testing.T) {
	ns := namespace(t)
	bk := runningBK(t, ns, 2, nil)
	cm := get(t, ns, "bk-bookie-config", &corev1.ConfigMap{})
	a := applier{c: &replaceAfterReadClient{Client: k8s}, scheme: scheme, owner: bk}
	if err := a.deleteOwned(context.Background(), &corev1.ConfigMap{ObjectMeta: cm.ObjectMeta}); !apierrors.IsConflict(err) {
		t.Fatalf("expected UID conflict, got %v", err)
	}
	got := get(t, ns, cm.Name, &corev1.ConfigMap{})
	if got.UID == cm.UID || got.DeletionTimestamp != nil {
		t.Fatal("replacement object deleted")
	}
}

func TestIntegrationDisabledComponentsKeepForeignResources(t *testing.T) {
	for _, component := range []string{"proxy", "prometheus", "autorecovery"} {
		t.Run(component, func(t *testing.T) {
			ns := namespace(t)
			var obj client.Object
			if component == "autorecovery" {
				runningBK(t, ns, 3, nil)
				obj = get(t, ns, "bk-autorecovery", &appsv1.Deployment{})
			} else {
				runningPC(t, ns)
				if component == "prometheus" {
					setMonitoring(t, ns, v1.MonitoringSpec{Enabled: true})
					obj = get(t, ns, "pc-prometheus", &appsv1.StatefulSet{})
				} else {
					obj = get(t, ns, "pc-proxy", &appsv1.Deployment{})
				}
			}
			obj.SetOwnerReferences(nil)
			if err := k8s.Update(context.Background(), obj); err != nil {
				t.Fatal(err)
			}
			var err error
			if component == "autorecovery" {
				bk := get(t, ns, "bk", &v1.BookKeeperCluster{})
				bk.Spec.AutoRecovery.Enabled = ptr.To(false)
				if err := k8s.Update(context.Background(), bk); err != nil {
					t.Fatal(err)
				}
				err = runBK(bkReconciler(), ns)
			} else {
				pc := get(t, ns, "pc", &v1.PulsarCluster{})
				if component == "proxy" {
					pc.Spec.Proxy.Enabled = false
				} else {
					pc.Spec.Monitoring.Enabled = false
				}
				if err := k8s.Update(context.Background(), pc); err != nil {
					t.Fatal(err)
				}
				_, err = pcReconciler().Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pc)})
			}
			if err == nil {
				t.Fatal("foreign cleanup was accepted")
			}
			if err := k8s.Get(context.Background(), client.ObjectKeyFromObject(obj), obj); err != nil || obj.GetDeletionTimestamp() != nil {
				t.Fatalf("foreign resource deleted: %v", err)
			}
		})
	}
}

func TestIntegrationBookKeeperCannotClearStorageClass(t *testing.T) {
	for _, cleared := range []*string{nil, ptr.To("")} {
		t.Run(fmt.Sprint(cleared), func(t *testing.T) {
			ns := namespace(t)
			class := storageClass(t, "class-"+ns, true)
			runningBK(t, ns, 2, class)
			boundPVC(t, ns, "ledgers-bk-bookie-0", "2Gi", class)
			before := get(t, ns, "bk-bookie", &appsv1.StatefulSet{})
			bk := get(t, ns, "bk", &v1.BookKeeperCluster{})
			bk.Spec.Storage.Ledger.StorageClassName = cleared
			if err := k8s.Update(context.Background(), bk); err != nil {
				t.Fatal(err)
			}
			reconcileBK(t, ns, "bk")
			bk = get(t, ns, "bk", &v1.BookKeeperCluster{})
			if !meta.IsStatusConditionTrue(bk.Status.Conditions, v1.ConditionSpecRejected) || meta.IsStatusConditionTrue(bk.Status.Conditions, v1.ConditionReady) {
				t.Fatal("class clearing not visibly rejected")
			}
			got := get(t, ns, "bk-bookie", &appsv1.StatefulSet{})
			if got.UID != before.UID || got.DeletionTimestamp != nil || ptr.Deref(got.Spec.VolumeClaimTemplates[1].Spec.StorageClassName, "") != *class {
				t.Fatal("existing template changed")
			}
		})
	}
}

func TestIntegrationDefaultClassAdmissionDoesNotTriggerRejection(t *testing.T) {
	ns := namespace(t)
	class := storageClass(t, "default-resolved-"+ns, true)
	runningBK(t, ns, 2, nil)
	boundPVC(t, ns, "ledgers-bk-bookie-0", "2Gi", class)
	reconcileBK(t, ns, "bk")
	if meta.IsStatusConditionTrue(get(t, ns, "bk", &v1.BookKeeperCluster{}).Status.Conditions, v1.ConditionSpecRejected) {
		t.Fatal("resolved default class was rejected")
	}
}

func TestIntegrationZooKeeperStorageChangeIsVisiblyRejected(t *testing.T) {
	ns := namespace(t)
	runningPC(t, ns)
	boundPVC(t, ns, "data-pc-zookeeper-0", "1Gi", nil)
	pc := get(t, ns, "pc", &v1.PulsarCluster{})
	pc.Spec.ZooKeeper.Storage.Size = resource.MustParse("5Gi")
	if err := k8s.Update(context.Background(), pc); err != nil {
		t.Fatal(err)
	}
	if _, err := pcReconciler().Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pc)}); err == nil {
		t.Fatal("storage change accepted")
	}
	pc = get(t, ns, "pc", &v1.PulsarCluster{})
	if pc.Status.Phase == v1.PhaseRunning || !meta.IsStatusConditionTrue(pc.Status.Conditions, v1.ConditionSpecRejected) || meta.IsStatusConditionTrue(pc.Status.Conditions, v1.ConditionReady) {
		t.Fatalf("invalid success status: %+v", pc.Status)
	}
	sts := get(t, ns, "pc-zookeeper", &appsv1.StatefulSet{})
	q := sts.Spec.VolumeClaimTemplates[0].Spec.Resources.Requests[corev1.ResourceStorage]
	if q.String() != "1Gi" {
		t.Fatal("ZooKeeper template changed")
	}
	pc.Spec.ZooKeeper.Storage.Size = resource.MustParse("1Gi")
	if err := k8s.Update(context.Background(), pc); err != nil {
		t.Fatal(err)
	}
	reconcilePC(t, ns, "pc")
	if !meta.IsStatusConditionTrue(get(t, ns, "pc", &v1.PulsarCluster{}).Status.Conditions, v1.ConditionReady) {
		t.Fatal("corrected spec did not recover")
	}
}

func TestIntegrationPrometheusStorageChangeDoesNotRaiseRetention(t *testing.T) {
	ns := namespace(t)
	runningPC(t, ns)
	class := storageClass(t, "monitor-expand-"+ns, true)
	setMonitoring(t, ns, v1.MonitoringSpec{Enabled: true, Prometheus: v1.PrometheusSpec{Storage: &v1.VolumeSpec{Size: resource.MustParse("1Gi"), StorageClassName: class}}})
	boundPVC(t, ns, "data-pc-prometheus-0", "1Gi", class)
	before := get(t, ns, "pc-prometheus", &appsv1.StatefulSet{})
	pc := get(t, ns, "pc", &v1.PulsarCluster{})
	pc.Spec.Monitoring.Prometheus.Storage.Size = resource.MustParse("4Gi")
	if err := k8s.Update(context.Background(), pc); err != nil {
		t.Fatal(err)
	}
	if _, err := pcReconciler().Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pc)}); err == nil {
		t.Fatal("monitoring storage change accepted")
	}
	after := get(t, ns, "pc-prometheus", &appsv1.StatefulSet{})
	if before.UID != after.UID || !equalStatus(before.Spec, after.Spec) {
		t.Fatal("Prometheus workload/retention changed despite rejected size")
	}
	pc = get(t, ns, "pc", &v1.PulsarCluster{})
	if pc.Status.Phase == v1.PhaseRunning || pc.Status.Monitoring.Ready || !meta.IsStatusConditionTrue(pc.Status.Conditions, v1.ConditionSpecRejected) {
		t.Fatal("monitoring rejection not reflected in status")
	}
}

func TestIntegrationPulsarDoesNotAdoptIndependentOrForeignBookKeeper(t *testing.T) {
	for _, foreign := range []bool{false, true} {
		t.Run(fmt.Sprint(foreign), func(t *testing.T) {
			ns := namespace(t)
			runningPC(t, ns)
			previous := get(t, ns, "pc", &v1.BookKeeperCluster{})
			if err := k8s.Delete(context.Background(), previous); err != nil {
				t.Fatal(err)
			}
			standalone := &v1.BookKeeperCluster{ObjectMeta: metav1.ObjectMeta{Name: "pc", Namespace: ns}, Spec: *previous.Spec.DeepCopy()}
			standalone.Spec.ZooKeeper = "external-zookeeper:2181"
			standalone.Spec.Replicas = 7
			standalone.Spec.Storage.Ledger.Size = resource.MustParse("99Gi")
			if foreign {
				standalone.OwnerReferences = []metav1.OwnerReference{{APIVersion: "pulsar.songof.dev/v1alpha1", Kind: "PulsarCluster", Name: "other", UID: "other", Controller: ptr.To(true)}}
			}
			if err := k8s.Create(context.Background(), standalone); err != nil {
				t.Fatal(err)
			}
			before := get(t, ns, "pc", &v1.BookKeeperCluster{})
			if _, err := pcReconciler().Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(before)}); err == nil {
				t.Fatal("existing independent cluster accepted")
			}
			after := get(t, ns, "pc", &v1.BookKeeperCluster{})
			if before.UID != after.UID || !equalStatus(before.Spec, after.Spec) || !equalStatus(before.OwnerReferences, after.OwnerReferences) || !equalStatus(before.Labels, after.Labels) {
				t.Fatal("independent cluster was modified or adopted")
			}
		})
	}
}

func expansionFixture(t *testing.T) (string, *string) {
	t.Helper()
	ns := namespace(t)
	class := storageClass(t, "owned-expand-"+ns, true)
	runningBK(t, ns, 4, class)
	for i := 0; i < 4; i++ {
		createPod(t, ns, fmt.Sprintf("bk-bookie-%d", i))
		boundPVC(t, ns, fmt.Sprintf("journal-bk-bookie-%d", i), "1Gi", class)
		boundPVC(t, ns, fmt.Sprintf("ledgers-bk-bookie-%d", i), "2Gi", class)
	}
	return ns, class
}

func TestIntegrationExpansionRejectsUnrelatedPVC(t *testing.T) {
	for _, changed := range []string{"owner", "labels", "missing-pod"} {
		t.Run(changed, func(t *testing.T) {
			ns, _ := expansionFixture(t)
			pvc := get(t, ns, "ledgers-bk-bookie-0", &corev1.PersistentVolumeClaim{})
			switch changed {
			case "owner":
				pvc.OwnerReferences = []metav1.OwnerReference{{APIVersion: "v1", Kind: "ConfigMap", Name: "unrelated", UID: "foreign", Controller: ptr.To(true)}}
			case "labels":
				pvc.Labels = nil
			case "missing-pod":
				deletePod(t, ns, "bk-bookie-0")
			}
			if err := k8s.Update(context.Background(), pvc); err != nil {
				t.Fatal(err)
			}
			setStorage(t, ns, "1Gi", "5Gi")
			if err := runBK(bkReconciler(), ns); err == nil {
				t.Fatal("unproven PVC ownership was accepted")
			}
			after := get(t, ns, pvc.Name, &corev1.PersistentVolumeClaim{})
			q := after.Spec.Resources.Requests[corev1.ResourceStorage]
			if after.UID != pvc.UID || q.String() != "2Gi" {
				t.Fatal("foreign PVC was expanded")
			}
			if get(t, ns, "bk-bookie", &appsv1.StatefulSet{}).DeletionTimestamp != nil {
				t.Fatal("workload replaced after ownership failure")
			}
		})
	}
}

func TestIntegrationOrphanRecreationPreservesBlockedScaleBoundary(t *testing.T) {
	for _, blocker := range []string{"unhealthy", "minimum"} {
		t.Run(blocker, func(t *testing.T) {
			ns, _ := expansionFixture(t)
			if blocker == "unhealthy" {
				sts := get(t, ns, "bk-bookie", &appsv1.StatefulSet{})
				sts.Status.ReadyReplicas, sts.Status.AvailableReplicas = 3, 3
				if err := k8s.Status().Update(context.Background(), sts); err != nil {
					t.Fatal(err)
				}
			} else {
				bk := get(t, ns, "bk", &v1.BookKeeperCluster{})
				bk.Spec.MinReplicas = 4
				if err := k8s.Update(context.Background(), bk); err != nil {
					t.Fatal(err)
				}
			}
			setReplicas(t, ns, 2)
			setStorage(t, ns, "1Gi", "5Gi")
			reconcileBK(t, ns, "bk")
			bk := get(t, ns, "bk", &v1.BookKeeperCluster{})
			if bk.Status.Decommission != nil || bk.Status.Recreation == nil || bk.Status.Recreation.Replicas != 4 {
				t.Fatalf("unsafe checkpoint: %+v", bk.Status)
			}
			old := get(t, ns, "bk-bookie", &appsv1.StatefulSet{})
			if old.DeletionTimestamp == nil {
				t.Fatal("expected orphaning")
			}
			gcOrphan(t, old)
			reconcileBK(t, ns, "bk")
			if *get(t, ns, "bk-bookie", &appsv1.StatefulSet{}).Spec.Replicas != 4 {
				t.Fatal("recreation bypassed scale-in guard")
			}
			reconcileBK(t, ns, "bk")
			if *get(t, ns, "bk-bookie", &appsv1.StatefulSet{}).Spec.Replicas != 4 {
				t.Fatal("restored unready cluster was scaled down")
			}
		})
	}
}

func TestIntegrationOrphanRecreationRestoresCheckpointBeforeNewSpec(t *testing.T) {
	ns, class := expansionFixture(t)
	setStorage(t, ns, "1Gi", "5Gi")
	reconcileBK(t, ns, "bk")
	old := get(t, ns, "bk-bookie", &appsv1.StatefulSet{})
	if old.DeletionTimestamp == nil {
		t.Fatal("expected orphaning")
	}
	gcOrphan(t, old)
	bk := get(t, ns, "bk", &v1.BookKeeperCluster{})
	bk.Spec.Replicas = 2
	bk.Spec.Storage.Ledger.StorageClassName = nil
	if err := k8s.Update(context.Background(), bk); err != nil {
		t.Fatal(err)
	}
	reconcileBK(t, ns, "bk")
	restored := get(t, ns, "bk-bookie", &appsv1.StatefulSet{})
	if *restored.Spec.Replicas != 4 || ptr.Deref(restored.Spec.VolumeClaimTemplates[1].Spec.StorageClassName, "") != *class {
		t.Fatal("new spec bypassed durable layout")
	}
	reconcileBK(t, ns, "bk")
	if !meta.IsStatusConditionTrue(get(t, ns, "bk", &v1.BookKeeperCluster{}).Status.Conditions, v1.ConditionSpecRejected) {
		t.Fatal("class clearing was not rejected after restoration")
	}
	if *get(t, ns, "bk-bookie", &appsv1.StatefulSet{}).Spec.Replicas != 4 {
		t.Fatal("recovery bypassed healthy scale-in")
	}
}

func TestIntegrationRecreationCheckpointConflictKeepsStatefulSet(t *testing.T) {
	ns, _ := expansionFixture(t)
	before := get(t, ns, "bk-bookie", &appsv1.StatefulSet{})
	setStorage(t, ns, "1Gi", "5Gi")
	r := bkReconciler()
	r.Client = &checkpointClient{Client: k8s, statusConflict: true}
	if err := runBK(r, ns); !apierrors.IsConflict(err) {
		t.Fatalf("expected recreation checkpoint conflict: %v", err)
	}
	after := get(t, ns, "bk-bookie", &appsv1.StatefulSet{})
	if before.UID != after.UID || after.DeletionTimestamp != nil {
		t.Fatal("StatefulSet deleted before durable recovery checkpoint")
	}
	if get(t, ns, "bk", &v1.BookKeeperCluster{}).Status.Recreation != nil {
		t.Fatal("failed checkpoint persisted")
	}
}
