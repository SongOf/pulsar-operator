package v1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

const GroupName = "pulsar.songof.dev"

// PulsarCluster describes a Pulsar deployment managed by the operator.
type PulsarCluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              PulsarClusterSpec   `json:"spec,omitempty"`
	Status            PulsarClusterStatus `json:"status,omitempty"`
}

type PulsarClusterSpec struct {
	Brokers   int32             `json:"brokers"`
	Bookies   int32             `json:"bookies"`
	Image     string            `json:"image,omitempty"`
	Resources ResourceProfile   `json:"resources,omitempty"`
	Storage   BookKeeperStorage `json:"storage"`
	Ops       Operations        `json:"ops,omitempty"`
}

type ResourceProfile struct {
	CPU    string `json:"cpu,omitempty"`
	Memory string `json:"memory,omitempty"`
}

type BookKeeperStorage struct {
	JournalPVC PVCSpec `json:"journalPVC"`
	LedgerPVC  PVCSpec `json:"ledgerPVC"`
}

type PVCSpec struct {
	StorageClassName string `json:"storageClassName,omitempty"`
	Size             string `json:"size"`
}

type Operations struct {
	RestartBrokerOrdinal *int32  `json:"restartBrokerOrdinal,omitempty"`
	DecommissionBookie   *string `json:"decommissionBookie,omitempty"`
}

type PulsarClusterStatus struct {
	Phase             string             `json:"phase,omitempty"`
	ObservedGeneration int64             `json:"observedGeneration,omitempty"`
	BrokerReady       int32              `json:"brokerReady,omitempty"`
	BookKeeperReady   int32              `json:"bookKeeperReady,omitempty"`
	Conditions        []metav1.Condition `json:"conditions,omitempty"`
}

type PulsarClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PulsarCluster `json:"items"`
}

// BookKeeperCluster describes standalone BookKeeper lifecycle management.
type BookKeeperCluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              BookKeeperClusterSpec   `json:"spec,omitempty"`
	Status            BookKeeperClusterStatus `json:"status,omitempty"`
}

type BookKeeperClusterSpec struct {
	Replicas  int32             `json:"replicas"`
	Image     string            `json:"image,omitempty"`
	Resources ResourceProfile   `json:"resources,omitempty"`
	Storage   BookKeeperStorage `json:"storage"`
	Ops       Operations        `json:"ops,omitempty"`
}

type BookKeeperClusterStatus struct {
	Phase              string             `json:"phase,omitempty"`
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	ReadyReplicas      int32              `json:"readyReplicas,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
}

type BookKeeperClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []BookKeeperCluster `json:"items"`
}
