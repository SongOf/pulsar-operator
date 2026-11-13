package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// Device is the immutable disk identity approved in DiskPool.spec.devices.
type Device struct {
	Path      string `json:"path"`
	Serial    string `json:"serial"`
	SizeBytes int64  `json:"sizeBytes"`
}

// DiskPoolSpec declares one approved device and its owning node and VG.
type DiskPoolSpec struct {
	NodeName string   `json:"nodeName"`
	VGName   string   `json:"vgName"`
	Approved bool     `json:"approved"`
	Devices  []Device `json:"devices"`
}

type DiskPoolStatus struct {
	Phase              string `json:"phase,omitempty"`
	CapacityBytes      int64  `json:"capacityBytes,omitempty"`
	AvailableBytes     int64  `json:"availableBytes,omitempty"`
	Message            string `json:"message,omitempty"`
	ObservedGeneration int64  `json:"observedGeneration,omitempty"`
	StorageClass       string `json:"storageClass,omitempty"`
	TopologyHostname   string `json:"topologyHostname,omitempty"`
}

// DiskPool is a cluster-scoped request reconciled by the owning node Agent.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=dpool
type DiskPool struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              DiskPoolSpec   `json:"spec"`
	Status            DiskPoolStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type DiskPoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DiskPool `json:"items"`
}

func init() { SchemeBuilder.Register(&DiskPool{}, &DiskPoolList{}) }
