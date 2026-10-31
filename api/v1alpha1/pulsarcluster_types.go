package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ZooKeeperSpec configures the ZooKeeper ensemble that stores Pulsar and BookKeeper metadata.
type ZooKeeperSpec struct {
	ComponentSpec `json:",inline"`
	// Replicas of the ensemble; an odd number. It cannot change after creation, because resizing an ensemble
	// needs a dynamic reconfiguration this operator does not perform.
	// +kubebuilder:default=3
	// +kubebuilder:validation:Minimum=1
	// +optional
	Replicas int32 `json:"replicas,omitempty"`
	// Storage is the data volume of each ZooKeeper server.
	Storage VolumeSpec `json:"storage"`
}

// BrokerSpec configures the Pulsar brokers.
type BrokerSpec struct {
	ComponentSpec `json:",inline"`
	// Replicas is the number of brokers. Brokers are stateless: a scale-in unloads the topics of the removed
	// brokers, which other brokers take over.
	// +kubebuilder:validation:Minimum=0
	Replicas int32 `json:"replicas"`
}

// ProxySpec configures the optional Pulsar proxy, the entry point for clients outside the cluster.
type ProxySpec struct {
	ComponentSpec `json:",inline"`
	// +optional
	Enabled bool `json:"enabled,omitempty"`
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=0
	// +optional
	Replicas int32 `json:"replicas,omitempty"`
	// ServiceType of the proxy Service.
	// +kubebuilder:default=ClusterIP
	// +optional
	ServiceType corev1.ServiceType `json:"serviceType,omitempty"`
}

// PulsarClusterSpec defines a complete Pulsar cluster: ZooKeeper, BookKeeper, brokers and an optional proxy.
type PulsarClusterSpec struct {
	// Image is the default image of every component. Changing it upgrades the components one after another:
	// ZooKeeper, bookies, brokers, then the proxy, each only after the previous one rolled out.
	// +kubebuilder:default="apachepulsar/pulsar:4.0.14"
	// +optional
	Image string `json:"image,omitempty"`
	// ClusterDomain is the DNS domain of the Kubernetes cluster.
	// +kubebuilder:default="cluster.local"
	// +optional
	ClusterDomain string `json:"clusterDomain,omitempty"`
	// Paused stops reconciliation, for manual maintenance.
	// +optional
	Paused bool `json:"paused,omitempty"`

	ZooKeeper ZooKeeperSpec `json:"zookeeper"`
	// BookKeeper is managed through a child BookKeeperCluster with the same name; edit it here, not on the child.
	BookKeeper BookKeeperSpec `json:"bookkeeper"`
	Broker     BrokerSpec     `json:"broker"`
	// +optional
	Proxy ProxySpec `json:"proxy,omitempty"`
	// Monitoring collects the metrics of every component into Prometheus for the console dashboard.
	// +optional
	Monitoring MonitoringSpec `json:"monitoring,omitempty"`
}

// PulsarClusterStatus is the observed state of a Pulsar cluster.
type PulsarClusterStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +optional
	Phase Phase `json:"phase,omitempty"`
	// +optional
	ZooKeeper ComponentStatus `json:"zookeeper,omitempty"`
	// +optional
	BookKeeper ComponentStatus `json:"bookkeeper,omitempty"`
	// BookKeeperPhase mirrors the phase of the child BookKeeperCluster, such as Decommissioning.
	// +optional
	BookKeeperPhase Phase `json:"bookkeeperPhase,omitempty"`
	// +optional
	Broker ComponentStatus `json:"broker,omitempty"`
	// +optional
	Proxy ComponentStatus `json:"proxy,omitempty"`
	// ServiceURL and WebServiceURL are the in-cluster addresses clients use.
	// +optional
	ServiceURL string `json:"serviceURL,omitempty"`
	// +optional
	WebServiceURL string `json:"webServiceURL,omitempty"`
	// +optional
	Monitoring MonitoringStatus `json:"monitoring,omitempty"`
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// PulsarCluster is a Pulsar cluster managed by the operator.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=psc
// +kubebuilder:printcolumn:name="Brokers",type=integer,JSONPath=`.spec.broker.replicas`
// +kubebuilder:printcolumn:name="Bookies",type=integer,JSONPath=`.spec.bookkeeper.replicas`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type PulsarCluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PulsarClusterSpec   `json:"spec,omitempty"`
	Status PulsarClusterStatus `json:"status,omitempty"`
}

// PulsarClusterList is a list of PulsarClusters.
// +kubebuilder:object:root=true
type PulsarClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PulsarCluster `json:"items"`
}

func init() {
	SchemeBuilder.Register(&PulsarCluster{}, &PulsarClusterList{})
}
