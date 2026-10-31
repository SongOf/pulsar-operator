package resources

import (
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	v1 "github.com/SongOf/pulsar-operator/api/v1alpha1"
)

// PrometheusComponent labels the Prometheus of a cluster.
const PrometheusComponent = "prometheus"

// Monitoring builds the Prometheus that keeps a cluster's metrics on a persistent volume. It scrapes the pods of
// one cluster (its instance label) that carry the prometheus.io/scrape annotation, and labels every series with
// namespace, pulsar_cluster, component and pod, which the console's queries select on.
type Monitoring struct {
	Cluster, Namespace, Domain string
	Spec                       v1.MonitoringSpec
}

// Name is the Prometheus StatefulSet, Service, ServiceAccount, Role and ConfigMap.
func (m Monitoring) Name() string { return Name(m.Cluster, PrometheusComponent) }

// Deployed reports whether the operator runs a Prometheus for the cluster.
func (m Monitoring) Deployed() bool { return m.Spec.Enabled && m.Spec.PrometheusURL == "" }

// URL is the Prometheus the console queries, or empty when monitoring is off.
func (m Monitoring) URL() string {
	switch {
	case m.Spec.PrometheusURL != "":
		return strings.TrimRight(m.Spec.PrometheusURL, "/")
	case m.Spec.Enabled:
		return fmt.Sprintf("http://%s.%s.svc.%s:9090", m.Name(), m.Namespace, domainOr(m.Domain))
	}
	return ""
}

func (m Monitoring) labels() map[string]string { return Labels(m.Cluster, PrometheusComponent) }

func (m Monitoring) retention() string {
	if r := m.Spec.Prometheus.Retention; r != "" {
		return r
	}
	return "15d"
}

func (m Monitoring) storage() v1.VolumeSpec {
	if m.Spec.Prometheus.Storage != nil {
		return *m.Spec.Prometheus.Storage
	}
	return v1.VolumeSpec{Size: resource.MustParse("20Gi")}
}

// RetentionSize is the on-disk cap: the configured one, or 80% of the volume.
func (m Monitoring) RetentionSize() string {
	if r := m.Spec.Prometheus.RetentionSize; r != "" {
		return r
	}
	size := m.storage().Size
	return fmt.Sprintf("%dMB", size.Value()*8/10/(1<<20))
}

func (m Monitoring) interval() string {
	if i := m.Spec.Prometheus.ScrapeInterval; i != "" {
		return i
	}
	return "15s"
}

// Config is prometheus.yml.
func (m Monitoring) Config() string {
	return fmt.Sprintf(`global:
  scrape_interval: %[1]s
  evaluation_interval: %[1]s
scrape_configs:
  - job_name: pulsar
    kubernetes_sd_configs:
      - role: pod
        namespaces:
          names: [%[2]q]
        selectors:
          - role: pod
            label: "app.kubernetes.io/managed-by=pulsar-operator,app.kubernetes.io/instance=%[3]s"
    relabel_configs:
      - source_labels: [__meta_kubernetes_pod_annotation_prometheus_io_scrape]
        regex: "true"
        action: keep
      - source_labels: [__meta_kubernetes_pod_phase]
        regex: Running
        action: keep
      # One target per pod: the pod IP with the annotated port, whatever container port discovery found.
      - source_labels: [__meta_kubernetes_pod_ip, __meta_kubernetes_pod_annotation_prometheus_io_port]
        regex: "(.+);(\\d+)"
        replacement: "$1:$2"
        target_label: __address__
      - source_labels: [__meta_kubernetes_pod_annotation_prometheus_io_path]
        target_label: __metrics_path__
      - source_labels: [__meta_kubernetes_namespace]
        target_label: namespace
      - source_labels: [__meta_kubernetes_pod_label_app_kubernetes_io_instance]
        target_label: pulsar_cluster
      - source_labels: [__meta_kubernetes_pod_label_app_kubernetes_io_component]
        target_label: component
      - source_labels: [__meta_kubernetes_pod_name]
        target_label: pod
`, m.interval(), m.Namespace, m.Cluster) + m.remoteWrite()
}

// remoteWrite renders the remote_write section; credentials are read from mounted Secrets, never inlined.
func (m Monitoring) remoteWrite() string {
	if len(m.Spec.Prometheus.RemoteWrite) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("remote_write:\n")
	for _, rw := range m.Spec.Prometheus.RemoteWrite {
		fmt.Fprintf(&b, "  - url: %q\n", rw.URL)
		if rw.BasicAuthSecret != "" {
			dir := "/etc/prometheus/secrets/" + rw.BasicAuthSecret
			fmt.Fprintf(&b, "    basic_auth:\n      username_file: %s/username\n      password_file: %s/password\n", dir, dir)
		}
	}
	return b.String()
}

// ConfigMap holds prometheus.yml.
func (m Monitoring) ConfigMap() *corev1.ConfigMap {
	return ConfigMap(m.Name()+"-config", m.Namespace, m.labels(), map[string]string{"prometheus.yml": m.Config()})
}

// ServiceAccount, Role and RoleBinding let Prometheus discover the pods of its namespace.
func (m Monitoring) ServiceAccount() *corev1.ServiceAccount {
	return &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: m.Name(), Namespace: m.Namespace, Labels: m.labels()}}
}

func (m Monitoring) Role() *rbacv1.Role {
	return &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: m.Name(), Namespace: m.Namespace, Labels: m.labels()},
		Rules:      []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get", "list", "watch"}}},
	}
}

func (m Monitoring) RoleBinding() *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: m.Name(), Namespace: m.Namespace, Labels: m.labels()},
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: m.Name()},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: m.Name(), Namespace: m.Namespace}},
	}
}

// Service exposes the Prometheus API to the console.
func (m Monitoring) Service() *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: m.Name(), Namespace: m.Namespace, Labels: m.labels()},
		Spec:       corev1.ServiceSpec{Selector: m.labels(), Ports: []corev1.ServicePort{Port("http", 9090)}},
	}
}

// StatefulSet runs Prometheus with its TSDB on a PVC, so samples survive restarts. A configuration change
// restarts it; it replays its write-ahead log and loses nothing.
func (m Monitoring) StatefulSet(configHash string) *appsv1.StatefulSet {
	p := m.Spec.Prometheus
	labels := m.labels()
	storage := m.storage()
	image := p.Image
	if image == "" {
		image = v1.DefaultPrometheusImage
	}
	probe := func(path string, delay int32) *corev1.Probe {
		return &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: path, Port: intstr.FromInt32(9090)}},
			InitialDelaySeconds: delay, PeriodSeconds: 10, TimeoutSeconds: 5, FailureThreshold: 6}
	}
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: m.Name(), Namespace: m.Namespace, Labels: labels},
		Spec: appsv1.StatefulSetSpec{
			Replicas:    ptr.To[int32](1),
			ServiceName: m.Name(),
			Selector:    &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels, Annotations: map[string]string{AnnotationConfigHash: configHash}},
				Spec: corev1.PodSpec{
					ServiceAccountName:            m.Name(),
					TerminationGracePeriodSeconds: ptr.To[int64](60),
					SecurityContext: &corev1.PodSecurityContext{
						RunAsUser: ptr.To[int64](65534), RunAsGroup: ptr.To[int64](65534), RunAsNonRoot: ptr.To(true),
						FSGroup: ptr.To[int64](65534), FSGroupChangePolicy: ptr.To(corev1.FSGroupChangeOnRootMismatch),
					},
					Containers: []corev1.Container{{
						Name:  PrometheusComponent,
						Image: image,
						Args: []string{
							"--config.file=/etc/prometheus/prometheus.yml",
							"--storage.tsdb.path=/prometheus",
							"--storage.tsdb.retention.time=" + m.retention(),
							"--storage.tsdb.retention.size=" + m.RetentionSize(),
							"--web.listen-address=:9090",
						},
						Ports:          []corev1.ContainerPort{{Name: "http", ContainerPort: 9090}},
						Resources:      p.Resources,
						ReadinessProbe: probe("/-/ready", 5),
						LivenessProbe:  probe("/-/healthy", 60),
						VolumeMounts: m.mounts(),
					}},
					Volumes: m.volumes(),
				},
			},
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{{
				ObjectMeta: metav1.ObjectMeta{Name: "data", Labels: labels},
				Spec: corev1.PersistentVolumeClaimSpec{
					AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
					StorageClassName: storage.StorageClassName,
					Resources:        corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: storage.Size}},
				},
			}},
		},
	}
}

func (m Monitoring) secrets() []string {
	seen := map[string]bool{}
	var out []string
	for _, rw := range m.Spec.Prometheus.RemoteWrite {
		if rw.BasicAuthSecret != "" && !seen[rw.BasicAuthSecret] {
			seen[rw.BasicAuthSecret] = true
			out = append(out, rw.BasicAuthSecret)
		}
	}
	return out
}

func (m Monitoring) mounts() []corev1.VolumeMount {
	mounts := []corev1.VolumeMount{
		{Name: "config", MountPath: "/etc/prometheus/prometheus.yml", SubPath: "prometheus.yml", ReadOnly: true},
		{Name: "data", MountPath: "/prometheus"},
	}
	for i, s := range m.secrets() {
		mounts = append(mounts, corev1.VolumeMount{Name: fmt.Sprintf("secret-%d", i), MountPath: "/etc/prometheus/secrets/" + s, ReadOnly: true})
	}
	return mounts
}

func (m Monitoring) volumes() []corev1.Volume {
	vols := []corev1.Volume{{Name: "config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
		LocalObjectReference: corev1.LocalObjectReference{Name: m.Name() + "-config"}}}}}
	for i, s := range m.secrets() {
		vols = append(vols, corev1.Volume{Name: fmt.Sprintf("secret-%d", i), VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: s}}})
	}
	return vols
}
