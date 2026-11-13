// Package v1alpha1 contains the Kubernetes API types shared by Storage and Agent.
// The deployed schemas remain in config/storage/crd.
// +kubebuilder:object:generate=true
// +groupName=storage.diskfleet.io
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

const (
	GroupName = "storage.diskfleet.io"
	Version   = "v1alpha1"
)

var (
	GroupVersion         = schema.GroupVersion{Group: GroupName, Version: Version}
	SchemeBuilder        = &scheme.Builder{GroupVersion: GroupVersion}
	AddToScheme          = SchemeBuilder.AddToScheme
	DiskPoolResource     = GroupVersion.WithResource("diskpools")
	PhysicalDiskResource = GroupVersion.WithResource("physicaldisks")
)
