package controller

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "github.com/SongOf/pulsar-operator/api/v1alpha1"
)

func newPC(t *testing.T, ns string) {
	t.Helper()
	pc := &v1.PulsarCluster{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "pc"},
		Spec: v1.PulsarClusterSpec{
			Image:     "apachepulsar/pulsar:4.0.14",
			ZooKeeper: v1.ZooKeeperSpec{Replicas: 3, Storage: v1.VolumeSpec{Size: resource.MustParse("1Gi")}},
			BookKeeper: v1.BookKeeperSpec{Replicas: 3, MinReplicas: 2, Storage: v1.BookKeeperStorage{
				Journal: v1.VolumeSpec{Size: resource.MustParse("1Gi")}, Ledger: v1.VolumeSpec{Size: resource.MustParse("2Gi")},
			}},
			Broker: v1.BrokerSpec{Replicas: 2},
			Proxy:  v1.ProxySpec{Enabled: true, Replicas: 1},
		},
	}
	if err := k8s.Create(context.Background(), pc); err != nil {
		t.Fatal(err)
	}
}

// markBKReady stands in for the child BookKeeperCluster controller.
func markBKReady(t *testing.T, ns string, initialized, ready bool) {
	t.Helper()
	bk := get(t, ns, "pc", &v1.BookKeeperCluster{})
	bk.Status.ObservedGeneration = bk.Generation
	set := func(typ string, ok bool) {
		s := metav1.ConditionFalse
		if ok {
			s = metav1.ConditionTrue
		}
		meta.SetStatusCondition(&bk.Status.Conditions, metav1.Condition{Type: typ, Status: s, Reason: "Test"})
	}
	set(v1.ConditionMetadataInitialized, initialized)
	set(v1.ConditionReady, ready)
	bk.Status.Phase = v1.PhaseRunning
	if err := k8s.Status().Update(context.Background(), bk); err != nil {
		t.Fatal(err)
	}
}

// runningPC brings a PulsarCluster up through every stage.
func runningPC(t *testing.T, ns string) {
	t.Helper()
	newPC(t, ns)
	reconcilePC(t, ns, "pc")
	rollOut(t, ns, "pc-zookeeper")
	reconcilePC(t, ns, "pc")
	markBKReady(t, ns, true, true)
	reconcilePC(t, ns, "pc")
	completeJob(t, ns, "pc-metadata-init")
	reconcilePC(t, ns, "pc")
	rollOut(t, ns, "pc-broker")
	rollOutDeployment(t, ns, "pc-proxy")
	reconcilePC(t, ns, "pc")
}

func TestPulsarStartsInOrder(t *testing.T) {
	ns := namespace(t)
	newPC(t, ns)
	reconcilePC(t, ns, "pc")

	zk := get(t, ns, "pc-zookeeper", &appsv1.StatefulSet{})
	if *zk.Spec.Replicas != 3 {
		t.Fatalf("zookeeper replicas = %d", *zk.Spec.Replicas)
	}
	if !absent(t, ns, "pc", &v1.BookKeeperCluster{}) {
		t.Fatal("bookies created before ZooKeeper is ready")
	}

	rollOut(t, ns, "pc-zookeeper")
	reconcilePC(t, ns, "pc")
	bk := get(t, ns, "pc", &v1.BookKeeperCluster{})
	if bk.Spec.Replicas != 3 || !strings.HasPrefix(bk.Spec.ZooKeeper, "pc-zookeeper-0.pc-zookeeper."+ns) || bk.Spec.Image != "apachepulsar/pulsar:4.0.14" {
		t.Fatalf("child = %+v", bk.Spec)
	}
	if len(bk.OwnerReferences) != 1 || bk.OwnerReferences[0].Kind != "PulsarCluster" {
		t.Fatal("child is not owned by the PulsarCluster")
	}
	if !absent(t, ns, "pc-metadata-init", &batchv1.Job{}) {
		t.Fatal("cluster metadata initialised before BookKeeper metadata")
	}

	markBKReady(t, ns, true, false)
	reconcilePC(t, ns, "pc")
	job := get(t, ns, "pc-metadata-init", &batchv1.Job{})
	if !strings.Contains(strings.Join(job.Spec.Template.Spec.Containers[0].Command, " "), "initialize-cluster-metadata --cluster pc") {
		t.Fatal("metadata job does not initialise the cluster")
	}
	if !absent(t, ns, "pc-broker", &appsv1.StatefulSet{}) {
		t.Fatal("brokers started before the cluster metadata exists")
	}

	completeJob(t, ns, "pc-metadata-init")
	reconcilePC(t, ns, "pc")
	broker := get(t, ns, "pc-broker", &appsv1.StatefulSet{})
	if *broker.Spec.Replicas != 2 {
		t.Fatalf("broker replicas = %d", *broker.Spec.Replicas)
	}
	get(t, ns, "pc-broker", &corev1.Service{})
	get(t, ns, "pc-broker-headless", &corev1.Service{})
	get(t, ns, "pc-proxy", &appsv1.Deployment{})
	pc := get(t, ns, "pc", &v1.PulsarCluster{})
	if pc.Status.ServiceURL != "pulsar://pc-broker."+ns+".svc.cluster.local:6650" {
		t.Fatalf("serviceURL = %q", pc.Status.ServiceURL)
	}
}

func TestPulsarRunning(t *testing.T) {
	ns := namespace(t)
	runningPC(t, ns)
	pc := get(t, ns, "pc", &v1.PulsarCluster{})
	if pc.Status.Phase != v1.PhaseRunning || !meta.IsStatusConditionTrue(pc.Status.Conditions, v1.ConditionReady) {
		t.Fatalf("phase = %s conditions = %+v", pc.Status.Phase, pc.Status.Conditions)
	}
}

func TestBrokerScaleAndBookKeeperPropagation(t *testing.T) {
	ns := namespace(t)
	runningPC(t, ns)
	pc := get(t, ns, "pc", &v1.PulsarCluster{})
	pc.Spec.Broker.Replicas = 4
	pc.Spec.BookKeeper.Replicas = 5
	pc.Spec.BookKeeper.Storage.Ledger.Size = resource.MustParse("8Gi")
	if err := k8s.Update(context.Background(), pc); err != nil {
		t.Fatal(err)
	}
	reconcilePC(t, ns, "pc")
	if n := *get(t, ns, "pc-broker", &appsv1.StatefulSet{}).Spec.Replicas; n != 4 {
		t.Fatalf("brokers = %d", n)
	}
	bk := get(t, ns, "pc", &v1.BookKeeperCluster{})
	if bk.Spec.Replicas != 5 || bk.Spec.Storage.Ledger.Size.String() != "8Gi" {
		t.Fatalf("child bookkeeper = %d %s", bk.Spec.Replicas, bk.Spec.Storage.Ledger.Size.String())
	}
}

func TestUpgradeRollsComponentsInOrder(t *testing.T) {
	ns := namespace(t)
	runningPC(t, ns)
	pc := get(t, ns, "pc", &v1.PulsarCluster{})
	pc.Spec.Image = "apachepulsar/pulsar:4.0.15"
	if err := k8s.Update(context.Background(), pc); err != nil {
		t.Fatal(err)
	}
	reconcilePC(t, ns, "pc")
	image := func(name string) string {
		return get(t, ns, name, &appsv1.StatefulSet{}).Spec.Template.Spec.Containers[0].Image
	}
	if image("pc-zookeeper") != "apachepulsar/pulsar:4.0.15" {
		t.Fatal("ZooKeeper is not upgraded first")
	}
	// ZooKeeper's new revision has not rolled out: the bookies and brokers keep the old image.
	if img := get(t, ns, "pc", &v1.BookKeeperCluster{}).Spec.Image; img != "apachepulsar/pulsar:4.0.14" {
		t.Fatalf("bookies upgraded to %s before ZooKeeper finished", img)
	}
	if image("pc-broker") != "apachepulsar/pulsar:4.0.14" {
		t.Fatal("brokers upgraded before ZooKeeper finished")
	}

	rollOut(t, ns, "pc-zookeeper")
	reconcilePC(t, ns, "pc")
	if img := get(t, ns, "pc", &v1.BookKeeperCluster{}).Spec.Image; img != "apachepulsar/pulsar:4.0.15" {
		t.Fatalf("bookies not upgraded after ZooKeeper: %s", img)
	}
	if image("pc-broker") != "apachepulsar/pulsar:4.0.14" {
		t.Fatal("brokers upgraded before the bookies finished")
	}

	markBKReady(t, ns, true, true)
	reconcilePC(t, ns, "pc")
	if image("pc-broker") != "apachepulsar/pulsar:4.0.15" {
		t.Fatal("brokers not upgraded after the bookies")
	}
	if img := get(t, ns, "pc-proxy", &appsv1.Deployment{}).Spec.Template.Spec.Containers[0].Image; img != "apachepulsar/pulsar:4.0.14" {
		t.Fatal("proxy upgraded before the brokers finished")
	}
	rollOut(t, ns, "pc-broker")
	reconcilePC(t, ns, "pc")
	if img := get(t, ns, "pc-proxy", &appsv1.Deployment{}).Spec.Template.Spec.Containers[0].Image; img != "apachepulsar/pulsar:4.0.15" {
		t.Fatal("proxy not upgraded last")
	}
}

func TestZooKeeperReplicasAreImmutable(t *testing.T) {
	ns := namespace(t)
	runningPC(t, ns)
	pc := get(t, ns, "pc", &v1.PulsarCluster{})
	pc.Spec.ZooKeeper.Replicas = 5
	if err := k8s.Update(context.Background(), pc); err != nil {
		t.Fatal(err)
	}
	reconcilePC(t, ns, "pc")
	if n := *get(t, ns, "pc-zookeeper", &appsv1.StatefulSet{}).Spec.Replicas; n != 3 {
		t.Fatalf("ensemble resized to %d", n)
	}
	pc = get(t, ns, "pc", &v1.PulsarCluster{})
	if !meta.IsStatusConditionTrue(pc.Status.Conditions, v1.ConditionSpecRejected) {
		t.Fatal("rejection not reported")
	}
}

func TestBrokerConfigChangeRestartsBrokers(t *testing.T) {
	ns := namespace(t)
	runningPC(t, ns)
	before := get(t, ns, "pc-broker", &appsv1.StatefulSet{}).Spec.Template.Annotations
	pc := get(t, ns, "pc", &v1.PulsarCluster{})
	pc.Spec.Broker.Config = map[string]string{"maxUnackedMessagesPerConsumer": "1000"}
	pc.Spec.Broker.RestartedAt = "2026-11-01T00:00:00Z"
	if err := k8s.Update(context.Background(), pc); err != nil {
		t.Fatal(err)
	}
	reconcilePC(t, ns, "pc")
	after := get(t, ns, "pc-broker", &appsv1.StatefulSet{}).Spec.Template.Annotations
	if after["pulsar.songof.dev/config-hash"] == before["pulsar.songof.dev/config-hash"] || after["pulsar.songof.dev/restartedAt"] != "2026-11-01T00:00:00Z" {
		t.Fatalf("annotations before %v after %v", before, after)
	}
}

func TestProxyCanBeDisabled(t *testing.T) {
	ns := namespace(t)
	runningPC(t, ns)
	pc := get(t, ns, "pc", &v1.PulsarCluster{})
	pc.Spec.Proxy.Enabled = false
	if err := k8s.Update(context.Background(), pc); err != nil {
		t.Fatal(err)
	}
	reconcilePC(t, ns, "pc")
	if !absent(t, ns, "pc-proxy", &appsv1.Deployment{}) || !absent(t, ns, "pc-proxy", &corev1.Service{}) {
		t.Fatal("proxy not removed")
	}
}

func setMonitoring(t *testing.T, ns string, m v1.MonitoringSpec) {
	t.Helper()
	pc := get(t, ns, "pc", &v1.PulsarCluster{})
	pc.Spec.Monitoring = m
	if err := k8s.Update(context.Background(), pc); err != nil {
		t.Fatal(err)
	}
	reconcilePC(t, ns, "pc")
}

func TestMonitoringLifecycle(t *testing.T) {
	ns := namespace(t)
	runningPC(t, ns)
	if !absent(t, ns, "pc-prometheus", &appsv1.StatefulSet{}) {
		t.Fatal("prometheus deployed without monitoring enabled")
	}

	setMonitoring(t, ns, v1.MonitoringSpec{Enabled: true})
	sts := get(t, ns, "pc-prometheus", &appsv1.StatefulSet{})
	if len(sts.Spec.VolumeClaimTemplates) != 1 || sts.Spec.Template.Spec.ServiceAccountName != "pc-prometheus" {
		t.Fatal("prometheus has no persistent volume or service account")
	}
	get(t, ns, "pc-prometheus", &corev1.Service{})
	get(t, ns, "pc-prometheus", &rbacv1.Role{})
	get(t, ns, "pc-prometheus", &rbacv1.RoleBinding{})
	cm := get(t, ns, "pc-prometheus-config", &corev1.ConfigMap{})
	if !strings.Contains(cm.Data["prometheus.yml"], "app.kubernetes.io/instance=pc") {
		t.Fatal("scrape config does not select the cluster")
	}
	pc := get(t, ns, "pc", &v1.PulsarCluster{})
	if pc.Status.Monitoring.URL != "http://pc-prometheus."+ns+".svc.cluster.local:9090" || pc.Status.Monitoring.Ready {
		t.Fatalf("monitoring status = %+v", pc.Status.Monitoring)
	}
	rollOut(t, ns, "pc-prometheus")
	reconcilePC(t, ns, "pc")
	if !get(t, ns, "pc", &v1.PulsarCluster{}).Status.Monitoring.Ready {
		t.Fatal("monitoring not ready after rollout")
	}

	// An external Prometheus replaces the deployed one.
	setMonitoring(t, ns, v1.MonitoringSpec{Enabled: true, PrometheusURL: "http://prom.monitoring:9090"})
	if !absent(t, ns, "pc-prometheus", &appsv1.StatefulSet{}) || !absent(t, ns, "pc-prometheus", &rbacv1.Role{}) {
		t.Fatal("deployed prometheus kept next to an external one")
	}
	if s := get(t, ns, "pc", &v1.PulsarCluster{}).Status.Monitoring; s.URL != "http://prom.monitoring:9090" || !s.Ready {
		t.Fatalf("external status = %+v", s)
	}
}
