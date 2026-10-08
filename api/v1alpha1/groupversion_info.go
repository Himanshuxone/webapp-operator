// +kubebuilder:object:generate=true
// +groupName=apps.example.io
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// GroupVersion is the identity of our API: apiVersion "apps.example.io/v1alpha1".
	GroupVersion = schema.GroupVersion{Group: "apps.example.io", Version: "v1alpha1"}

	// SchemeBuilder collects our Go types so they can be added to a runtime Scheme.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme is called from main.go so the client can encode/decode our types.
	AddToScheme = SchemeBuilder.AddToScheme
)
