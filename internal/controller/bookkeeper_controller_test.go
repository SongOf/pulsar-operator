package controller

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	v1 "github.com/SongOf/pulsar-operator/api/v1alpha1"
	"github.com/SongOf/pulsar-operator/internal/resources"
)

func newBK(t *testing.T, ns string, replicas int32, class *string) *v1.BookKeeperCluster {
	t.Helper()
	bk := &v1.BookKeeperCluster{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "bk"},
		Spec: v1.BookKeeperClusterSpec{
			ZooKeeper: "zk:2181",
			BookKeeperSpec: v1.BookKeeperSpec{
				Replicas: replicas, MinReplicas: 2,
				Storage: v1.BookKeeperStorage{
					Journal: v1.VolumeSpec{Size: resource.MustParse("1Gi"), StorageClassName: class},
					Ledger:  v1.VolumeSpec{Size: resource.MustParse("2Gi"), StorageClassName: class},
				},
			},
		},
	}
	if err := k8s.Create(context.Background(), bk); err != nil {
		t.Fatal(err)
	}
	return bk
}

// runningBK creates a BookKeeperCluster and drives it to a running state with replicas bookies.
func runningBK(t *testing.T, ns string, replicas int32, class *string) *v1.BookKeeperCluster {
	t.Helper()
	newBK(t, ns, replicas, class)
	reconcileBK(t, ns, "bk")
	completeJob(t, ns, "bk-bookie-init")
	reconcileBK(t, ns, "bk")
	rollOut(t, ns, "bk-bookie")
	rollOutDeployment(t, ns, "bk-autorecovery")
	reconcileBK(t, ns, "bk")
	return get(t, ns, "bk", &v1.BookKeeperCluster{})
}

func setReplicas(t *testing.T, ns string, replicas int32) {
	t.Helper()
	bk := get(t, ns, "bk", &v1.BookKeeperCluster{})
	bk.Spec.Replicas = replicas
	if err := k8s.Update(context.Background(), bk); err != nil {
		t.Fatal(err)
	}
}

func TestBookiesWaitForMetadata(t *testing.T) {
	ns := namespace(t)
	newBK(t, ns, 3, nil)
	reconcileBK(t, ns, "bk")

	job := get(t, ns, "bk-bookie-init", &batchv1.Job{})
	if !strings.Contains(strings.Join(job.Spec.Template.Spec.Containers[0].Command, " "), "initnewcluster") {
		t.Fatal("init job does not format the metadata")
	}
	if !absent(t, ns, "bk-bookie", &appsv1.StatefulSet{}) {
		t.Fatal("bookies started before the BookKeeper metadata exists")
	}
	bk := get(t, ns, "bk", &v1.BookKeeperCluster{})
	if bk.Status.Phase != v1.PhaseInitializing {
		t.Fatalf("phase = %s", bk.Status.Phase)
	}

	completeJob(t, ns, "bk-bookie-init")
	reconcileBK(t, ns, "bk")
	sts := get(t, ns, "bk-bookie", &appsv1.StatefulSet{})
	if *sts.Spec.Replicas != 3 || len(sts.Spec.VolumeClaimTemplates) != 2 {
		t.Fatalf("statefulset replicas=%d templates=%d", *sts.Spec.Replicas, len(sts.Spec.VolumeClaimTemplates))
	}
	get(t, ns, "bk-bookie", &corev1.Service{})
	get(t, ns, "bk-bookie-config", &corev1.ConfigMap{})
	get(t, ns, "bk-autorecovery", &appsv1.Deployment{})
	bk = get(t, ns, "bk", &v1.BookKeeperCluster{})
	if !meta.IsStatusConditionTrue(bk.Status.Conditions, v1.ConditionMetadataInitialized) {
		t.Fatal("MetadataInitialized not reported")
	}
}

func TestRunningClusterIsReady(t *testing.T) {
	ns := namespace(t)
	bk := runningBK(t, ns, 3, nil)
	if bk.Status.Phase != v1.PhaseRunning || !meta.IsStatusConditionTrue(bk.Status.Conditions, v1.ConditionReady) {
		t.Fatalf("phase = %s conditions = %+v", bk.Status.Phase, bk.Status.Conditions)
	}
	if len(bk.Status.BookieDetails) != 3 || bk.Status.BookieDetails[2].BookieID != "bk-bookie-2.bk-bookie."+ns+".svc.cluster.local:3181" {
		t.Fatalf("bookie details = %+v", bk.Status.BookieDetails)
	}
}

func TestScaleOut(t *testing.T) {
	ns := namespace(t)
	runningBK(t, ns, 3, nil)
	setReplicas(t, ns, 5)
	reconcileBK(t, ns, "bk")
	if n := *get(t, ns, "bk-bookie", &appsv1.StatefulSet{}).Spec.Replicas; n != 5 {
		t.Fatalf("replicas = %d", n)
	}
	if bk := get(t, ns, "bk", &v1.BookKeeperCluster{}); bk.Status.Decommission != nil {
		t.Fatal("scale-out started a decommission")
	}
}

func TestScaleInDecommissionsOneBookieAtATime(t *testing.T) {
	ns := namespace(t)
	runningBK(t, ns, 4, nil)
	for i := 0; i < 4; i++ {
		createPod(t, ns, "bk-bookie-"+string(rune('0'+i)))
	}
	boundPVC(t, ns, "journal-bk-bookie-3", "1Gi", nil)
	boundPVC(t, ns, "ledgers-bk-bookie-3", "2Gi", nil)

	setReplicas(t, ns, 2)
	reconcileBK(t, ns, "bk")

	// Step 1: only the highest bookie stops.
	if n := *get(t, ns, "bk-bookie", &appsv1.StatefulSet{}).Spec.Replicas; n != 3 {
		t.Fatalf("statefulset shrank to %d; it must remove one bookie at a time", n)
	}
	bk := get(t, ns, "bk", &v1.BookKeeperCluster{})
	d := bk.Status.Decommission
	if d == nil || d.Ordinal != 3 || d.Phase != v1.DecommissionStopping || bk.Status.Phase != v1.PhaseDecommissioning {
		t.Fatalf("decommission = %+v phase = %s", d, bk.Status.Phase)
	}
	// While the pod still runs, nothing is decommissioned.
	reconcileBK(t, ns, "bk")
	if !absent(t, ns, "bk-bookie-decommission-3", &batchv1.Job{}) {
		t.Fatal("decommission started while the bookie still runs")
	}

	// Step 2: once stopped, its ledgers are re-replicated.
	deletePod(t, ns, "bk-bookie-3")
	reconcileBK(t, ns, "bk")
	job := get(t, ns, "bk-bookie-decommission-3", &batchv1.Job{})
	if !strings.Contains(strings.Join(job.Spec.Template.Spec.Containers[0].Command, " "), "decommissionbookie -bookieid bk-bookie-3.bk-bookie."+ns) {
		t.Fatal("decommission job targets the wrong bookie")
	}
	if d := get(t, ns, "bk", &v1.BookKeeperCluster{}).Status.Decommission; d.Phase != v1.DecommissionReplicating {
		t.Fatalf("phase = %s", d.Phase)
	}
	// The PVCs stay until replication finished.
	get(t, ns, "ledgers-bk-bookie-3", &corev1.PersistentVolumeClaim{})

	// Step 3: after replication, its PVCs are deleted and the next bookie can go.
	completeJob(t, ns, "bk-bookie-decommission-3")
	reconcileBK(t, ns, "bk")
	if !absent(t, ns, "journal-bk-bookie-3", &corev1.PersistentVolumeClaim{}) || !absent(t, ns, "ledgers-bk-bookie-3", &corev1.PersistentVolumeClaim{}) {
		t.Fatal("PVCs of the decommissioned bookie were kept")
	}
	if d := get(t, ns, "bk", &v1.BookKeeperCluster{}).Status.Decommission; d != nil {
		t.Fatalf("decommission not finished: %+v", d)
	}

	// The next bookie goes only once the remaining ones are ready.
	reconcileBK(t, ns, "bk")
	if d := get(t, ns, "bk", &v1.BookKeeperCluster{}).Status.Decommission; d != nil {
		t.Fatal("next decommission started before the statefulset was ready")
	}
	rollOut(t, ns, "bk-bookie")
	reconcileBK(t, ns, "bk")
	d = get(t, ns, "bk", &v1.BookKeeperCluster{}).Status.Decommission
	if d == nil || d.Ordinal != 2 {
		t.Fatalf("second decommission = %+v", d)
	}
	if n := *get(t, ns, "bk-bookie", &appsv1.StatefulSet{}).Spec.Replicas; n != 2 {
		t.Fatalf("replicas = %d", n)
	}
}

func TestScaleInStopsAtMinReplicas(t *testing.T) {
	ns := namespace(t)
	runningBK(t, ns, 2, nil)
	setReplicas(t, ns, 1)
	reconcileBK(t, ns, "bk")
	bk := get(t, ns, "bk", &v1.BookKeeperCluster{})
	if !meta.IsStatusConditionTrue(bk.Status.Conditions, v1.ConditionScaleInBlocked) || bk.Status.Decommission != nil {
		t.Fatalf("scale below minReplicas not blocked: %+v %+v", bk.Status.Conditions, bk.Status.Decommission)
	}
	if n := *get(t, ns, "bk-bookie", &appsv1.StatefulSet{}).Spec.Replicas; n != 2 {
		t.Fatalf("replicas = %d", n)
	}
}

func storageClass(t *testing.T, name string, expandable bool) *string {
	t.Helper()
	sc := &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: name}, Provisioner: "example.com/test", AllowVolumeExpansion: ptr.To(expandable)}
	if err := k8s.Create(context.Background(), sc); err != nil {
		t.Fatal(err)
	}
	return ptr.To(name)
}

func setStorage(t *testing.T, ns, journal, ledger string) {
	t.Helper()
	bk := get(t, ns, "bk", &v1.BookKeeperCluster{})
	bk.Spec.Storage.Journal.Size = resource.MustParse(journal)
	bk.Spec.Storage.Ledger.Size = resource.MustParse(ledger)
	if err := k8s.Update(context.Background(), bk); err != nil {
		t.Fatal(err)
	}
}

func TestStorageExpansion(t *testing.T) {
	ns := namespace(t)
	class := storageClass(t, "expandable-"+ns, true)
	runningBK(t, ns, 2, class)
	for i := 0; i < 2; i++ {
		boundPVC(t, ns, "journal-bk-bookie-"+string(rune('0'+i)), "1Gi", class)
		boundPVC(t, ns, "ledgers-bk-bookie-"+string(rune('0'+i)), "2Gi", class)
	}
	before := get(t, ns, "bk-bookie", &appsv1.StatefulSet{}).UID

	setStorage(t, ns, "1Gi", "5Gi")
	reconcileBK(t, ns, "bk")

	for i := 0; i < 2; i++ {
		pvc := get(t, ns, "ledgers-bk-bookie-"+string(rune('0'+i)), &corev1.PersistentVolumeClaim{})
		if got := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; got.String() != "5Gi" {
			t.Fatalf("ledger PVC %d requests %s", i, got.String())
		}
		journal := get(t, ns, "journal-bk-bookie-"+string(rune('0'+i)), &corev1.PersistentVolumeClaim{})
		if got := journal.Spec.Resources.Requests[corev1.ResourceStorage]; got.String() != "1Gi" {
			t.Fatalf("journal PVC %d changed to %s", i, got.String())
		}
	}
	bk := get(t, ns, "bk", &v1.BookKeeperCluster{})
	if c := meta.FindStatusCondition(bk.Status.Conditions, v1.ConditionStorageResized); c == nil || c.Reason != "Resizing" {
		t.Fatalf("StorageResized = %+v", c)
	}
	if !ptr.Deref(bk.Status.ExpansionSupported, false) {
		t.Fatal("expansion support not reported")
	}
	// The StatefulSet is recreated so new bookies get the new size; envtest has no garbage collector, so the
	// orphaning delete leaves it terminating or gone.
	sts := get(t, ns, "bk-bookie", &appsv1.StatefulSet{})
	if sts.UID != before || sts.DeletionTimestamp == nil {
		t.Fatal("statefulset not deleted for recreation")
	}
	if sts.Spec.Template.Spec.Containers == nil || len(sts.Finalizers) == 0 || sts.Finalizers[0] != "orphan" {
		t.Fatalf("statefulset not deleted with orphan propagation: %v", sts.Finalizers)
	}
	gcOrphan(t, sts)
	reconcileBK(t, ns, "bk")
	sts = get(t, ns, "bk-bookie", &appsv1.StatefulSet{})
	if got := sts.Spec.VolumeClaimTemplates[1].Spec.Resources.Requests[corev1.ResourceStorage]; got.String() != "5Gi" {
		t.Fatalf("ledger template = %s", got.String())
	}
}

func TestStorageShrinkIsRejected(t *testing.T) {
	ns := namespace(t)
	class := storageClass(t, "shrink-"+ns, true)
	runningBK(t, ns, 2, class)
	boundPVC(t, ns, "ledgers-bk-bookie-0", "2Gi", class)
	setStorage(t, ns, "1Gi", "1Gi")
	reconcileBK(t, ns, "bk")
	bk := get(t, ns, "bk", &v1.BookKeeperCluster{})
	c := meta.FindStatusCondition(bk.Status.Conditions, v1.ConditionSpecRejected)
	if c == nil || !strings.Contains(c.Message, "cannot shrink") {
		t.Fatalf("SpecRejected = %+v", c)
	}
	pvc := get(t, ns, "ledgers-bk-bookie-0", &corev1.PersistentVolumeClaim{})
	if got := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; got.String() != "2Gi" {
		t.Fatalf("PVC shrank to %s", got.String())
	}
	sts := get(t, ns, "bk-bookie", &appsv1.StatefulSet{})
	if got := sts.Spec.VolumeClaimTemplates[1].Spec.Resources.Requests[corev1.ResourceStorage]; got.String() != "2Gi" {
		t.Fatalf("template changed to %s despite the rejection", got.String())
	}
}

func TestStorageExpansionUnsupported(t *testing.T) {
	ns := namespace(t)
	class := storageClass(t, "fixed-"+ns, false)
	runningBK(t, ns, 2, class)
	boundPVC(t, ns, "ledgers-bk-bookie-0", "2Gi", class)
	setStorage(t, ns, "1Gi", "4Gi")
	reconcileBK(t, ns, "bk")
	bk := get(t, ns, "bk", &v1.BookKeeperCluster{})
	c := meta.FindStatusCondition(bk.Status.Conditions, v1.ConditionStorageResized)
	if c == nil || c.Reason != "ExpansionNotSupported" || ptr.Deref(bk.Status.ExpansionSupported, true) {
		t.Fatalf("StorageResized = %+v expansionSupported = %v", c, bk.Status.ExpansionSupported)
	}
	pvc := get(t, ns, "ledgers-bk-bookie-0", &corev1.PersistentVolumeClaim{})
	if got := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; got.String() != "2Gi" {
		t.Fatalf("PVC changed to %s", got.String())
	}
}

func TestConfigChangeRestartsBookies(t *testing.T) {
	ns := namespace(t)
	runningBK(t, ns, 2, nil)
	before := get(t, ns, "bk-bookie", &appsv1.StatefulSet{}).Spec.Template.Annotations[resources.AnnotationConfigHash]
	bk := get(t, ns, "bk", &v1.BookKeeperCluster{})
	bk.Spec.Config = map[string]string{"journalSyncData": "false"}
	if err := k8s.Update(context.Background(), bk); err != nil {
		t.Fatal(err)
	}
	reconcileBK(t, ns, "bk")
	cm := get(t, ns, "bk-bookie-config", &corev1.ConfigMap{})
	if cm.Data["PULSAR_PREFIX_journalSyncData"] != "false" {
		t.Fatal("config not written")
	}
	after := get(t, ns, "bk-bookie", &appsv1.StatefulSet{}).Spec.Template.Annotations[resources.AnnotationConfigHash]
	if after == before {
		t.Fatal("config change did not change the pod template")
	}
}

func TestAutoRecoveryCanBeDisabled(t *testing.T) {
	ns := namespace(t)
	runningBK(t, ns, 2, nil)
	bk := get(t, ns, "bk", &v1.BookKeeperCluster{})
	bk.Spec.AutoRecovery.Enabled = ptr.To(false)
	if err := k8s.Update(context.Background(), bk); err != nil {
		t.Fatal(err)
	}
	reconcileBK(t, ns, "bk")
	if !absent(t, ns, "bk-autorecovery", &appsv1.Deployment{}) {
		t.Fatal("auto-recovery still deployed")
	}
}
