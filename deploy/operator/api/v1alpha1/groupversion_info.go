// Package v1alpha1 contains the Tenant API's Go types -- kubebuilder's
// standard api/<version>/ layout, hand-written rather than scaffolded
// (no kubebuilder/controller-gen binary available in this environment;
// see /deploy/README.md's verification
// section for what that means for this package specifically: it's real,
// compiling, unit-tested Go code, never reconciled against a live
// cluster).
//
// +kubebuilder:object:generate=true
// +groupName=cairnobs.io
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// GroupVersion is group cairnobs.io, version v1alpha1.
	GroupVersion = schema.GroupVersion{Group: "cairnobs.io", Version: "v1alpha1"}

	// SchemeBuilder is used to add go types to the GroupVersionKind scheme.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)
