package controller

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	v1 "github.com/SongOf/pulsar-operator/api/v1alpha1"
)

// The tests drive the reconcilers step by step against a real API server. envtest runs no kubelet or
// controllers, so the tests play their part: they mark jobs complete, create and delete pods, and set
// StatefulSet status.

var (
	k8s    client.Client
	scheme = runtime.NewScheme()
)

func TestMain(m *testing.M) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		fmt.Println("KUBEBUILDER_ASSETS is not set; run `make test` to run the controller tests")
		os.Exit(0)
	}
	_ = clientgoscheme.AddToScheme(scheme)
	_ = v1.AddToScheme(scheme)
	env := &envtest.Environment{CRDDirectoryPaths: []string{filepath.Join("..", "..", "config", "crd")}, ErrorIfCRDPathMissing: true}
	cfg, err := env.Start()
	if err != nil {
		panic(err)
	}
	k8s, err = client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		panic(err)
	}
	code := m.Run()
	_ = env.Stop()
	os.Exit(code)
}

var nsCounter int

func namespace(t *testing.T) string {
	t.Helper()
	nsCounter++
	name := fmt.Sprintf("t%d-%d", nsCounter, time.Now().UnixNano()%100000)
	if err := k8s.Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}); err != nil {
		t.Fatal(err)
	}
	return name
}

func bkReconciler() *BookKeeperReconciler {
	return &BookKeeperReconciler{Client: k8s, Scheme: scheme, Recorder: record.NewFakeRecorder(100)}
}

func pcReconciler() *PulsarReconciler {
	return &PulsarReconciler{Client: k8s, Scheme: scheme, Recorder: record.NewFakeRecorder(100)}
}

func reconcileBK(t *testing.T, ns, name string) {
	t.Helper()
	if _, err := bkReconciler().Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

func reconcilePC(t *testing.T, ns, name string) {
	t.Helper()
	if _, err := pcReconciler().Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

func get[T client.Object](t *testing.T, ns, name string, obj T) T {
	t.Helper()
	if err := k8s.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, obj); err != nil {
		t.Fatalf("get %T %s/%s: %v", obj, ns, name, err)
	}
	return obj
}

func absent(t *testing.T, ns, name string, obj client.Object) bool {
	t.Helper()
	err := k8s.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, obj)
	if err == nil {
		// An object being deleted with finalizers still counts as gone for these tests.
		return obj.GetDeletionTimestamp() != nil
	}
	return client.IgnoreNotFound(err) == nil
}

func completeJob(t *testing.T, ns, name string) {
	t.Helper()
	job := get(t, ns, name, &batchv1.Job{})
	now := metav1.Now()
	job.Status.StartTime = &now
	job.Status.CompletionTime = &now
	job.Status.Succeeded = 1
	job.Status.Conditions = []batchv1.JobCondition{
		{Type: batchv1.JobSuccessCriteriaMet, Status: corev1.ConditionTrue, LastTransitionTime: now},
		{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, LastTransitionTime: now},
	}
	if err := k8s.Status().Update(context.Background(), job); err != nil {
		t.Fatalf("complete job %s: %v", name, err)
	}
}

// rollOut marks every replica of a StatefulSet ready and current.
func rollOut(t *testing.T, ns, name string) {
	t.Helper()
	sts := get(t, ns, name, &appsv1.StatefulSet{})
	n := *sts.Spec.Replicas
	sts.Status = appsv1.StatefulSetStatus{ObservedGeneration: sts.Generation, Replicas: n, ReadyReplicas: n, AvailableReplicas: n,
		UpdatedReplicas: n, CurrentReplicas: n, CurrentRevision: "r1", UpdateRevision: "r1"}
	if err := k8s.Status().Update(context.Background(), sts); err != nil {
		t.Fatalf("roll out %s: %v", name, err)
	}
}

func rollOutDeployment(t *testing.T, ns, name string) {
	t.Helper()
	d := get(t, ns, name, &appsv1.Deployment{})
	n := *d.Spec.Replicas
	d.Status = appsv1.DeploymentStatus{ObservedGeneration: d.Generation, Replicas: n, ReadyReplicas: n, AvailableReplicas: n, UpdatedReplicas: n}
	if err := k8s.Status().Update(context.Background(), d); err != nil {
		t.Fatalf("roll out %s: %v", name, err)
	}
}

func createPod(t *testing.T, ns, name string) {
	t.Helper()
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "x"}}}}
	if err := k8s.Create(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
}

func deletePod(t *testing.T, ns, name string) {
	t.Helper()
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
	if err := k8s.Delete(context.Background(), pod, client.GracePeriodSeconds(0)); err != nil {
		t.Fatal(err)
	}
}

// boundPVC creates a bound PVC, which is the only kind Kubernetes lets a client expand.
func boundPVC(t *testing.T, ns, name, size string, class *string) {
	t.Helper()
	ctx := context.Background()
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv-" + ns + "-" + name},
		Spec: corev1.PersistentVolumeSpec{
			Capacity:                      corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(size)},
			AccessModes:                   []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			PersistentVolumeSource:        corev1.PersistentVolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/tmp/" + name}},
			StorageClassName:              derefString(class),
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimDelete,
		},
	}
	if err := k8s.Create(ctx, pv); err != nil {
		t.Fatal(err)
	}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: class,
			VolumeName:       pv.Name,
			Resources:        corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(size)}},
		},
	}
	if err := k8s.Create(ctx, pvc); err != nil {
		t.Fatal(err)
	}
	pvc.Status = corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound, AccessModes: pvc.Spec.AccessModes,
		Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(size)}}
	if err := k8s.Status().Update(ctx, pvc); err != nil {
		t.Fatal(err)
	}
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// gcOrphan finishes an orphaning delete the way the garbage collector would.
func gcOrphan(t *testing.T, obj client.Object) {
	t.Helper()
	obj.SetFinalizers(nil)
	if err := k8s.Update(context.Background(), obj); err != nil {
		t.Fatal(err)
	}
}
