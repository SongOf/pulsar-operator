package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// PhysicalDiskSpec is the immutable hardware identity reported by an Agent.
type PhysicalDiskSpec struct {
	NodeName  string `json:"nodeName"`
	Path      string `json:"path"`
	Serial    string `json:"serial"`
	SizeBytes int64  `json:"sizeBytes"`
	Model     string `json:"model"`
	Media     string `json:"media"`
}

type PhysicalDiskStatus struct {
	Phase    string   `json:"phase,omitempty"`
	Reasons  []string `json:"reasons,omitempty"`
	PoolName string   `json:"poolName,omitempty"`
}

// PhysicalDisk records discovery; its existence does not approve device writes.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=pdisk
type PhysicalDisk struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              PhysicalDiskSpec   `json:"spec"`
	Status            PhysicalDiskStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type PhysicalDiskList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PhysicalDisk `json:"items"`
}

func init() { SchemeBuilder.Register(&PhysicalDisk{}, &PhysicalDiskList{}) }
