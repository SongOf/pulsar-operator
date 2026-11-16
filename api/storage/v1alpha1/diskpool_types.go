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
	// AllocationApproval is independent of permission to initialize the disk.
	AllocationApproval *DiskPoolAllocationApproval `json:"allocationApproval,omitempty"`
}

// DiskPoolAllocationApproval authorizes new allocations from one immutable result.
type DiskPoolAllocationApproval struct {
	PoolUID      string      `json:"poolUID"`
	RunID        string      `json:"runID"`
	ResultDigest string      `json:"resultDigest"`
	Decision     string      `json:"decision"`
	ReviewedAt   metav1.Time `json:"reviewedAt"`
	Note         string      `json:"note,omitempty"`
}

// DiskBenchmarkWorkload records fio total latency (submission plus completion),
// in nanoseconds, and throughput in bytes/second. Missing samples are not zeroes.
type DiskBenchmarkWorkload struct {
	Name                    string  `json:"name"`
	BlockSizeBytes          int64   `json:"blockSizeBytes"`
	QueueDepth              int32   `json:"queueDepth"`
	DurationSeconds         float64 `json:"durationSeconds"`
	Bytes                   int64   `json:"bytes"`
	BandwidthBytesPerSecond int64   `json:"bandwidthBytesPerSecond"`
	IOPS                    float64 `json:"iops"`
	LatencyMeanNS           float64 `json:"latencyMeanNs"`
	LatencyP95NS            float64 `json:"latencyP95Ns"`
	LatencyP99NS            float64 `json:"latencyP99Ns"`
	Errors                  int64   `json:"errors"`
}

// DiskPoolBenchmark is a bounded, durable summary of the Agent's temporary-LV
// benchmark. Only a successful result with verified cleanup may be approved.
type DiskPoolBenchmark struct {
	PoolUID         string                  `json:"poolUID"`
	NodeUID         string                  `json:"nodeUID"`
	VGName          string                  `json:"vgName"`
	Device          Device                  `json:"device"`
	RunID           string                  `json:"runID"`
	Profile         string                  `json:"profile"`
	Phase           string                  `json:"phase"`
	StartedAt       *metav1.Time            `json:"startedAt,omitempty"`
	CompletedAt     *metav1.Time            `json:"completedAt,omitempty"`
	TestSizeBytes   int64                   `json:"testSizeBytes"`
	Workloads       []DiskBenchmarkWorkload `json:"workloads,omitempty"`
	CleanupVerified bool                    `json:"cleanupVerified"`
	ErrorCode       string                  `json:"errorCode,omitempty"`
	Message         string                  `json:"message,omitempty"`
	ResultDigest    string                  `json:"resultDigest,omitempty"`
}

type DiskPoolStatus struct {
	Phase              string             `json:"phase,omitempty"`
	CapacityBytes      int64              `json:"capacityBytes,omitempty"`
	AvailableBytes     int64              `json:"availableBytes,omitempty"`
	Message            string             `json:"message,omitempty"`
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	StorageClass       string             `json:"storageClass,omitempty"`
	TopologyHostname   string             `json:"topologyHostname,omitempty"`
	Benchmark          *DiskPoolBenchmark `json:"benchmark,omitempty"`
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
