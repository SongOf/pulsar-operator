package resources

import (
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	v1 "github.com/SongOf/pulsar-operator/api/v1alpha1"
)

func bookkeeper() *v1.BookKeeperCluster {
	return &v1.BookKeeperCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "pulsar"},
		Spec: v1.BookKeeperClusterSpec{
			ZooKeeper: "zk-0.zk:2181,zk-1.zk:2181",
			BookKeeperSpec: v1.BookKeeperSpec{
				Replicas: 3,
				Storage: v1.BookKeeperStorage{
					Journal: v1.VolumeSpec{Size: resource.MustParse("10Gi"), StorageClassName: ptr.To("fast")},
					Ledger:  v1.VolumeSpec{Size: resource.MustParse("50Gi")},
				},
			},
		},
	}
}

func TestBookieIdentity(t *testing.T) {
	b := BookKeeper{C: bookkeeper()}
	if got, want := b.BookieID(2), "demo-bookie-2.demo-bookie.pulsar.svc.cluster.local:3181"; got != want {
		t.Fatalf("BookieID = %q, want %q", got, want)
	}
	if got := b.PVCName(LedgerVolume, 1); got != "ledgers-demo-bookie-1" {
		t.Fatalf("PVCName = %q", got)
	}
	b.C.Spec.ClusterDomain = "k8s.local"
	if got := b.BookieID(0); !strings.HasSuffix(got, ".svc.k8s.local:3181") {
		t.Fatalf("BookieID ignores the cluster domain: %q", got)
	}
}

func TestMetadataServiceURIUsesSemicolons(t *testing.T) {
	b := BookKeeper{C: bookkeeper()}
	if got, want := b.MetadataServiceURI(), "zk+hierarchical://zk-0.zk:2181;zk-1.zk:2181/ledgers"; got != want {
		t.Fatalf("MetadataServiceURI = %q, want %q", got, want)
	}
}

func TestConfigDataPrecedence(t *testing.T) {
	data := ConfigData(
		map[string]string{"a": "default", "owned": "operator"},
		map[string]string{"a": "user", "owned": "user", "extra": "x"},
		[]string{"owned"},
		map[string]string{"PULSAR_MEM": "-Xmx1g"},
	)
	want := map[string]string{"PULSAR_PREFIX_a": "user", "PULSAR_PREFIX_owned": "operator", "PULSAR_PREFIX_extra": "x", "PULSAR_MEM": "-Xmx1g"}
	for k, v := range want {
		if data[k] != v {
			t.Errorf("%s = %q, want %q", k, data[k], v)
		}
	}
}

func TestHashIsStableAndSensitive(t *testing.T) {
	a := Hash(map[string]string{"x": "1", "y": "2"})
	if a != Hash(map[string]string{"y": "2", "x": "1"}) {
		t.Fatal("hash depends on map order")
	}
	if a == Hash(map[string]string{"x": "1", "y": "3"}) {
		t.Fatal("hash ignores a changed value")
	}
	if Hash(map[string]string{"ab": "c"}) == Hash(map[string]string{"a": "bc"}) {
		t.Fatal("hash confuses key and value boundaries")
	}
}

func TestBookieStatefulSet(t *testing.T) {
	bk := bookkeeper()
	bk.Spec.Storage.ReclaimPolicy = v1.ReclaimDelete
	sts := BookKeeper{C: bk}.StatefulSet(2, "h1")
	if *sts.Spec.Replicas != 2 {
		t.Fatalf("replicas = %d", *sts.Spec.Replicas)
	}
	vct := sts.Spec.VolumeClaimTemplates
	if len(vct) != 2 || vct[0].Name != JournalVolume || vct[1].Name != LedgerVolume {
		t.Fatalf("volume claim templates = %+v", vct)
	}
	if got := vct[0].Spec.Resources.Requests[corev1.ResourceStorage]; got.String() != "10Gi" || *vct[0].Spec.StorageClassName != "fast" {
		t.Fatalf("journal claim = %s %v", got.String(), vct[0].Spec.StorageClassName)
	}
	if vct[1].Spec.StorageClassName != nil {
		t.Fatal("ledger claim should use the default StorageClass")
	}
	rp := sts.Spec.PersistentVolumeClaimRetentionPolicy
	if rp.WhenDeleted != appsv1.DeletePersistentVolumeClaimRetentionPolicyType || rp.WhenScaled != appsv1.RetainPersistentVolumeClaimRetentionPolicyType {
		t.Fatalf("retention = %+v; scale-in must retain PVCs until decommissioned", rp)
	}
	if sts.Spec.Template.Annotations[AnnotationConfigHash] != "h1" {
		t.Fatal("config hash not on the pod template")
	}
	var advertised string
	for _, e := range sts.Spec.Template.Spec.Containers[0].Env {
		if e.Name == "PULSAR_PREFIX_advertisedAddress" {
			advertised = e.Value
		}
	}
	if advertised != "$(POD_NAME).demo-bookie.$(POD_NAMESPACE).svc.cluster.local" {
		t.Fatalf("advertisedAddress = %q", advertised)
	}
	if sts.Annotations[AnnotationVolumeTemplateHash] == "" {
		t.Fatal("volume template hash missing")
	}
}

func TestVolumeTemplateHashFollowsSizes(t *testing.T) {
	bk := bookkeeper()
	before := BookKeeper{C: bk}.VolumeTemplateHash()
	bk.Spec.Config = map[string]string{"x": "y"}
	if (BookKeeper{C: bk}).VolumeTemplateHash() != before {
		t.Fatal("config changed the volume template hash")
	}
	bk.Spec.Storage.Ledger.Size = resource.MustParse("100Gi")
	if (BookKeeper{C: bk}).VolumeTemplateHash() == before {
		t.Fatal("a new ledger size kept the volume template hash")
	}
}

func TestDecommissionJobTargetsTheBookie(t *testing.T) {
	b := BookKeeper{C: bookkeeper()}
	job := b.DecommissionJob(2)
	script := strings.Join(job.Spec.Template.Spec.Containers[0].Command, " ")
	if !strings.Contains(script, "decommissionbookie -bookieid "+b.BookieID(2)) {
		t.Fatalf("script = %q", script)
	}
	if job.Name != "demo-bookie-decommission-2" {
		t.Fatalf("name = %q", job.Name)
	}
}

func pulsar() *v1.PulsarCluster {
	return &v1.PulsarCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "pc", Namespace: "ns"},
		Spec: v1.PulsarClusterSpec{
			Image:     "apachepulsar/pulsar:4.0.14",
			ZooKeeper: v1.ZooKeeperSpec{Storage: v1.VolumeSpec{Size: resource.MustParse("5Gi")}},
			BookKeeper: v1.BookKeeperSpec{Replicas: 3, Storage: v1.BookKeeperStorage{
				Journal: v1.VolumeSpec{Size: resource.MustParse("10Gi")}, Ledger: v1.VolumeSpec{Size: resource.MustParse("20Gi")},
			}},
			Broker: v1.BrokerSpec{Replicas: 2, ComponentSpec: v1.ComponentSpec{Config: map[string]string{"clusterName": "hijack", "maxTopicsPerNamespace": "100"}}},
		},
	}
}

func TestZooKeeperEnsemble(t *testing.T) {
	p := Pulsar{C: pulsar()}
	if p.ZooKeeperReplicas() != 3 {
		t.Fatalf("default ensemble = %d", p.ZooKeeperReplicas())
	}
	cm := p.ZooKeeperConfigMap()
	if got := cm.Data["ZOOKEEPER_SERVERS"]; got != "pc-zookeeper-0,pc-zookeeper-1,pc-zookeeper-2" {
		t.Fatalf("ZOOKEEPER_SERVERS = %q", got)
	}
	if got := p.ZooKeeperConnect(); strings.Count(got, ",") != 2 || !strings.HasPrefix(got, "pc-zookeeper-0.pc-zookeeper.ns.svc.cluster.local:2181") {
		t.Fatalf("connect = %q", got)
	}
}

func TestBrokerConfigKeepsOwnedKeys(t *testing.T) {
	cm := Pulsar{C: pulsar()}.BrokerConfigMap()
	if cm.Data["PULSAR_PREFIX_clusterName"] != "pc" {
		t.Fatalf("clusterName = %q; the user cannot rename the cluster", cm.Data["PULSAR_PREFIX_clusterName"])
	}
	if cm.Data["PULSAR_PREFIX_maxTopicsPerNamespace"] != "100" {
		t.Fatal("user broker config not applied")
	}
	if !strings.HasPrefix(cm.Data["PULSAR_PREFIX_metadataStoreUrl"], "zk:pc-zookeeper-0.") {
		t.Fatalf("metadataStoreUrl = %q", cm.Data["PULSAR_PREFIX_metadataStoreUrl"])
	}
}

func TestChildBookKeeperInheritsImage(t *testing.T) {
	child := Pulsar{C: pulsar()}.ChildBookKeeper()
	if child.Name != "pc" || child.Spec.Image != "apachepulsar/pulsar:4.0.14" || child.Spec.Replicas != 3 {
		t.Fatalf("child = %s %q %d", child.Name, child.Spec.Image, child.Spec.Replicas)
	}
	if !strings.Contains(child.Spec.ZooKeeper, "pc-zookeeper-2.") {
		t.Fatalf("child zookeeper = %q", child.Spec.ZooKeeper)
	}
}

func TestJVMOptions(t *testing.T) {
	if got := JVMOptions(v1.JVMSpec{}, "1g", "2g"); got != "-Xms1g -Xmx1g -XX:MaxDirectMemorySize=2g" {
		t.Fatalf("defaults = %q", got)
	}
	if got := JVMOptions(v1.JVMSpec{Heap: "4g", ExtraOptions: "-XX:+UseG1GC"}, "1g", ""); got != "-Xms4g -Xmx4g -XX:+UseG1GC" {
		t.Fatalf("custom = %q", got)
	}
}

func TestAffinityModes(t *testing.T) {
	l := Labels("c", Bookie)
	if Affinity(v1.AntiAffinityNone, l) != nil {
		t.Fatal("None should not set affinity")
	}
	if a := Affinity(v1.AntiAffinityRequired, l); len(a.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution) != 1 {
		t.Fatal("Required should set a hard rule")
	}
	if a := Affinity("", l); len(a.PodAntiAffinity.PreferredDuringSchedulingIgnoredDuringExecution) != 1 {
		t.Fatal("default should be Preferred")
	}
}

func TestMonitoringPrometheus(t *testing.T) {
	m := Monitoring{Cluster: "demo", Namespace: "pulsar", Spec: v1.MonitoringSpec{Enabled: true}}
	if !m.Deployed() || m.URL() != "http://demo-prometheus.pulsar.svc.cluster.local:9090" {
		t.Fatalf("deployed=%v url=%q", m.Deployed(), m.URL())
	}
	cfg := m.Config()
	for _, want := range []string{`names: ["pulsar"]`, "app.kubernetes.io/instance=demo", "target_label: pulsar_cluster", "target_label: component", "target_label: pod", "prometheus_io_port"} {
		if !strings.Contains(cfg, want) {
			t.Fatalf("config misses %q:\n%s", want, cfg)
		}
	}
	if strings.Contains(cfg, "remote_write") {
		t.Fatal("remote_write without endpoints")
	}
	sts := m.StatefulSet("h")
	args := strings.Join(sts.Spec.Template.Spec.Containers[0].Args, " ")
	if !strings.Contains(args, "--storage.tsdb.retention.time=15d") || !strings.Contains(args, "--storage.tsdb.retention.size=16384MB") {
		t.Fatalf("args = %s; default retention is 15d and 80%% of 20Gi", args)
	}
	if got := sts.Spec.VolumeClaimTemplates[0].Spec.Resources.Requests[corev1.ResourceStorage]; got.String() != "20Gi" {
		t.Fatalf("volume = %s", got.String())
	}

	m.Spec.Prometheus = v1.PrometheusSpec{RetentionSize: "5GB", RemoteWrite: []v1.RemoteWriteSpec{{URL: "http://vm:8480/write", BasicAuthSecret: "vm-auth"}}}
	cfg = m.Config()
	if !strings.Contains(cfg, `- url: "http://vm:8480/write"`) || !strings.Contains(cfg, "password_file: /etc/prometheus/secrets/vm-auth/password") {
		t.Fatalf("remote_write = %s", cfg)
	}
	sts = m.StatefulSet("h")
	if !strings.Contains(strings.Join(sts.Spec.Template.Spec.Containers[0].Args, " "), "retention.size=5GB") || len(sts.Spec.Template.Spec.Volumes) != 2 {
		t.Fatal("custom retention size or secret volume missing")
	}

	ext := Monitoring{Cluster: "demo", Namespace: "pulsar", Spec: v1.MonitoringSpec{Enabled: true, PrometheusURL: "http://prom.monitoring:9090/"}}
	if ext.Deployed() || ext.URL() != "http://prom.monitoring:9090" {
		t.Fatalf("external: deployed=%v url=%q", ext.Deployed(), ext.URL())
	}
	if (Monitoring{Cluster: "demo", Namespace: "pulsar"}).URL() != "" {
		t.Fatal("disabled monitoring has a URL")
	}
}

func TestComponentsAdvertiseMetrics(t *testing.T) {
	sts := BookKeeper{C: bookkeeper()}.StatefulSet(3, "h")
	a := sts.Spec.Template.Annotations
	if a["prometheus.io/scrape"] != "true" || a["prometheus.io/port"] != "8000" || a["prometheus.io/path"] != "/metrics" {
		t.Fatalf("bookie annotations = %v", a)
	}
	b := Pulsar{C: pulsar()}.BrokerStatefulSet("h").Spec.Template.Annotations
	if b["prometheus.io/port"] != "8080" || b["prometheus.io/path"] != "/metrics/" {
		t.Fatalf("broker annotations = %v", b)
	}
}
