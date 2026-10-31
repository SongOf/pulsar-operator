// Package v1alpha1 contains the API types of the pulsar.songof.dev group.
// +kubebuilder:object:generate=true
// +groupName=pulsar.songof.dev
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// GroupVersion is the group and version of these objects.
	GroupVersion = schema.GroupVersion{Group: "pulsar.songof.dev", Version: "v1alpha1"}

	// SchemeBuilder registers the types of this group with a scheme.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme adds the types of this group to a scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)
