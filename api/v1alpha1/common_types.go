package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// DefaultImage is the Pulsar image every component runs unless a spec names another.
const DefaultImage = "apachepulsar/pulsar:4.0.14"

// AntiAffinityMode selects how pods of one component are spread across nodes.
// +kubebuilder:validation:Enum=None;Preferred;Required
type AntiAffinityMode string

const (
	AntiAffinityNone      AntiAffinityMode = "None"
	AntiAffinityPreferred AntiAffinityMode = "Preferred"
	AntiAffinityRequired  AntiAffinityMode = "Required"
)

// JVMSpec configures the JVM of a component.
type JVMSpec struct {
	// Heap is the -Xms/-Xmx size, for example 1g or 512m.
	// +optional
	Heap string `json:"heap,omitempty"`
	// DirectMemory is -XX:MaxDirectMemorySize, for example 1g.
	// +optional
	DirectMemory string `json:"directMemory,omitempty"`
	// ExtraOptions are appended to the JVM options as written.
	// +optional
	ExtraOptions string `json:"extraOptions,omitempty"`
}

// PodPolicy holds scheduling and pod metadata settings shared by every component.
type PodPolicy struct {
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`
	// AntiAffinity spreads the component's pods across nodes. Preferred by default.
	// +kubebuilder:default=Preferred
	// +optional
	AntiAffinity AntiAffinityMode `json:"antiAffinity,omitempty"`
	// Annotations are added to every pod of the component.
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`
	// PriorityClassName of the component's pods.
	// +optional
	PriorityClassName string `json:"priorityClassName,omitempty"`
}

// ComponentSpec is what every Pulsar component (ZooKeeper, bookie, broker, proxy) shares.
type ComponentSpec struct {
	// Image overrides the cluster image for this component.
	// +optional
	Image string `json:"image,omitempty"`
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
	// +optional
	JVM JVMSpec `json:"jvm,omitempty"`
	// Config entries are written into the component's configuration file (broker.conf, bookkeeper.conf,
	// zookeeper.conf or proxy.conf). Changing them triggers a rolling restart.
	// +optional
	Config map[string]string `json:"config,omitempty"`
	// +optional
	Pod PodPolicy `json:"pod,omitempty"`
	// RestartedAt triggers a rolling restart whenever it changes, like `kubectl rollout restart`.
	// +optional
	RestartedAt string `json:"restartedAt,omitempty"`
}

// VolumeSpec describes one persistent volume claim per pod.
type VolumeSpec struct {
	// Size of the volume. Increasing it expands the existing PVCs when the StorageClass allows expansion;
	// decreasing it is refused.
	Size resource.Quantity `json:"size"`
	// StorageClassName of the PVCs; the cluster default when empty. Cannot be changed after creation.
	// +optional
	StorageClassName *string `json:"storageClassName,omitempty"`
}

// ReclaimPolicy decides what happens to PVCs when the cluster is deleted.
// +kubebuilder:validation:Enum=Retain;Delete
type ReclaimPolicy string

const (
	ReclaimRetain ReclaimPolicy = "Retain"
	ReclaimDelete ReclaimPolicy = "Delete"
)

// DefaultPrometheusImage is the Prometheus the operator deploys for monitoring.
const DefaultPrometheusImage = "prom/prometheus:v3.13.5"

// MonitoringSpec configures metrics collection. Metrics are kept by Prometheus on a persistent volume, so
// they survive restarts of the operator and of Prometheus; the console dashboard queries it.
type MonitoringSpec struct {
	// Enabled deploys a Prometheus for this cluster that scrapes every component.
	// +optional
	Enabled bool `json:"enabled,omitempty"`
	// PrometheusURL uses an existing Prometheus instead, for example http://prometheus.monitoring:9090. It must
	// scrape the pods with the labels pulsar_cluster (the cluster name), component and pod; see
	// config/monitoring. When set, no Prometheus is deployed.
	// +optional
	PrometheusURL string `json:"prometheusURL,omitempty"`
	// +optional
	Prometheus PrometheusSpec `json:"prometheus,omitempty"`
}

// PrometheusSpec configures the Prometheus the operator deploys.
type PrometheusSpec struct {
	// +optional
	Image string `json:"image,omitempty"`
	// Retention is how long samples are kept, for example 15d.
	// +kubebuilder:default="15d"
	// +optional
	Retention string `json:"retention,omitempty"`
	// RetentionSize caps the samples kept on disk, for example 16GB; the oldest blocks are deleted first. It
	// defaults to 80% of the storage size, so a growing series count cannot fill the volume.
	// +optional
	RetentionSize string `json:"retentionSize,omitempty"`
	// Storage is the volume holding the samples; 20Gi on the default StorageClass when unset.
	// +optional
	Storage *VolumeSpec `json:"storage,omitempty"`
	// RemoteWrite also sends every sample to long-term storage such as VictoriaMetrics, Thanos Receive or Mimir.
	// Point monitoring.prometheusURL at that store to let the console query beyond the local retention.
	// +optional
	RemoteWrite []RemoteWriteSpec `json:"remoteWrite,omitempty"`
	// ScrapeInterval, for example 15s.
	// +kubebuilder:default="15s"
	// +optional
	ScrapeInterval string `json:"scrapeInterval,omitempty"`
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
}

// RemoteWriteSpec is one remote-write endpoint.
type RemoteWriteSpec struct {
	// URL of the remote-write receiver, for example http://vminsert.monitoring:8480/insert/0/prometheus/api/v1/write.
	URL string `json:"url"`
	// BasicAuthSecret names a Secret in the cluster's namespace with username and password keys.
	// +optional
	BasicAuthSecret string `json:"basicAuthSecret,omitempty"`
}

// MonitoringStatus reports where the cluster's metrics are.
type MonitoringStatus struct {
	// URL is the Prometheus the console queries.
	URL string `json:"url,omitempty"`
	// Ready is true when the deployed Prometheus is running, or an external one is configured.
	Ready bool `json:"ready"`
}

// ComponentStatus is the observed state of one workload.
type ComponentStatus struct {
	Replicas        int32  `json:"replicas"`
	ReadyReplicas   int32  `json:"readyReplicas"`
	UpdatedReplicas int32  `json:"updatedReplicas"`
	Image           string `json:"image,omitempty"`
	// RolledOut is true when every replica runs the current template and is ready.
	RolledOut bool `json:"rolledOut"`
}

// Phase summarises the state of a cluster.
type Phase string

const (
	PhaseInitializing    Phase = "Initializing"
	PhaseRunning         Phase = "Running"
	PhaseUpdating        Phase = "Updating"
	PhaseScaling         Phase = "Scaling"
	PhaseDecommissioning Phase = "Decommissioning"
	PhaseDegraded        Phase = "Degraded"
	PhaseFailed          Phase = "Failed"
)

// Condition types reported by both kinds.
const (
	ConditionReady               = "Ready"
	ConditionMetadataInitialized = "MetadataInitialized"
	ConditionStorageResized      = "StorageResized"
	ConditionScaleInBlocked      = "ScaleInBlocked"
	ConditionSpecRejected        = "SpecRejected"
)
