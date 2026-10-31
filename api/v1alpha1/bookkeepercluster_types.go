package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// BookKeeperStorage is the disk layout of every bookie: one journal and one ledger PVC per bookie.
type BookKeeperStorage struct {
	Journal VolumeSpec `json:"journal"`
	Ledger  VolumeSpec `json:"ledger"`
	// ReclaimPolicy applies when the BookKeeperCluster is deleted. PVCs of a decommissioned bookie are always
	// deleted, because its data has been re-replicated and its cookie removed.
	// +kubebuilder:default=Retain
	// +optional
	ReclaimPolicy ReclaimPolicy `json:"reclaimPolicy,omitempty"`
}

// AutoRecoverySpec configures the BookKeeper auto-recovery workers, which re-replicate the ledgers of lost or
// decommissioned bookies.
type AutoRecoverySpec struct {
	// +kubebuilder:default=true
	// +optional
	Enabled *bool `json:"enabled,omitempty"`
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	// +optional
	Replicas int32 `json:"replicas,omitempty"`
	// +optional
	ComponentSpec `json:",inline"`
}

// BookKeeperSpec is the bookie part of a cluster; PulsarCluster embeds it.
type BookKeeperSpec struct {
	ComponentSpec `json:",inline"`
	// Replicas is the number of bookies. Lowering it decommissions bookies one at a time, from the highest ordinal.
	// +kubebuilder:validation:Minimum=0
	Replicas int32 `json:"replicas"`
	// MinReplicas is the floor a scale-in may not go below; it should be at least the ledger ensemble size.
	// +kubebuilder:default=3
	// +kubebuilder:validation:Minimum=0
	// +optional
	MinReplicas int32 `json:"minReplicas,omitempty"`
	Storage     BookKeeperStorage `json:"storage"`
	// +optional
	AutoRecovery AutoRecoverySpec `json:"autoRecovery,omitempty"`
}

// BookKeeperClusterSpec defines a BookKeeper cluster.
type BookKeeperClusterSpec struct {
	// ZooKeeper is the metadata store of the bookies, as host:port[,host:port].
	ZooKeeper string `json:"zookeeper"`
	// LedgersRootPath is the ZooKeeper path of the BookKeeper metadata.
	// +kubebuilder:default="/ledgers"
	// +optional
	LedgersRootPath string `json:"ledgersRootPath,omitempty"`
	// ClusterDomain is the DNS domain of the Kubernetes cluster, used to build bookie IDs.
	// +kubebuilder:default="cluster.local"
	// +optional
	ClusterDomain string `json:"clusterDomain,omitempty"`
	// Paused stops reconciliation, for manual maintenance.
	// +optional
	Paused bool `json:"paused,omitempty"`
	// Monitoring of a standalone BookKeeperCluster; one owned by a PulsarCluster is monitored by its Prometheus.
	// +optional
	Monitoring MonitoringSpec `json:"monitoring,omitempty"`

	BookKeeperSpec `json:",inline"`
}

// VolumeStatus is the observed state of one bookie PVC.
type VolumeStatus struct {
	Name      string `json:"name"`
	Requested string `json:"requested,omitempty"`
	Capacity  string `json:"capacity,omitempty"`
	Phase     string `json:"phase,omitempty"`
	// Resizing is true while the PVC has a pending or in-progress expansion.
	Resizing bool `json:"resizing,omitempty"`
}

// BookieStatus is the observed state of one bookie.
type BookieStatus struct {
	Name     string       `json:"name"`
	BookieID string       `json:"bookieID"`
	Ready    bool         `json:"ready"`
	Journal  VolumeStatus `json:"journal"`
	Ledger   VolumeStatus `json:"ledger"`
}

// DecommissionPhase is the step a bookie scale-in is at.
type DecommissionPhase string

const (
	// DecommissionStopping waits for the bookie pod to stop after the StatefulSet shrank.
	DecommissionStopping DecommissionPhase = "Stopping"
	// DecommissionReplicating runs `bookkeeper shell decommissionbookie`, which waits until every ledger of the
	// bookie is re-replicated and then removes its cookie.
	DecommissionReplicating DecommissionPhase = "Replicating"
	// DecommissionCleaning deletes the bookie's PVCs.
	DecommissionCleaning DecommissionPhase = "Cleaning"
	DecommissionFailed   DecommissionPhase = "Failed"
)

// DecommissionStatus tracks the bookie being removed.
type DecommissionStatus struct {
	Ordinal   int32             `json:"ordinal"`
	BookieID  string            `json:"bookieID"`
	Phase     DecommissionPhase `json:"phase"`
	Job       string            `json:"job,omitempty"`
	StartedAt metav1.Time       `json:"startedAt"`
	Message   string            `json:"message,omitempty"`
}

// BookKeeperClusterStatus is the observed state of a BookKeeper cluster.
type BookKeeperClusterStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +optional
	Phase Phase `json:"phase,omitempty"`
	// +optional
	Bookies ComponentStatus `json:"bookies,omitempty"`
	// +optional
	AutoRecovery ComponentStatus `json:"autoRecovery,omitempty"`
	// +optional
	BookieDetails []BookieStatus `json:"bookieDetails,omitempty"`
	// +optional
	Decommission *DecommissionStatus `json:"decommission,omitempty"`
	// ExpansionSupported reports whether the StorageClasses of the bookie PVCs allow online expansion.
	// +optional
	ExpansionSupported *bool `json:"expansionSupported,omitempty"`
	// +optional
	Monitoring MonitoringStatus `json:"monitoring,omitempty"`
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// BookKeeperCluster is a BookKeeper cluster managed by the operator.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=bkc
// +kubebuilder:printcolumn:name="Bookies",type=integer,JSONPath=`.spec.replicas`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.bookies.readyReplicas`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type BookKeeperCluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   BookKeeperClusterSpec   `json:"spec,omitempty"`
	Status BookKeeperClusterStatus `json:"status,omitempty"`
}

// BookKeeperClusterList is a list of BookKeeperClusters.
// +kubebuilder:object:root=true
type BookKeeperClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []BookKeeperCluster `json:"items"`
}

func init() {
	SchemeBuilder.Register(&BookKeeperCluster{}, &BookKeeperClusterList{})
}
