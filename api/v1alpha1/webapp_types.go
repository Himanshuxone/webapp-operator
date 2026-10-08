package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type WebAppSpec struct {
	// Image is the container image to run, e.g. "nginx:1.27".
	// +kubebuilder:validation:MinLength=1
	Image string `json:"image"`

	// Replicas is how many pods we want. A pointer lets us tell "unset" apart from 0.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=1
	Replicas *int32 `json:"replicas,omitempty"`

	// Port is the container port the app listens on.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +kubebuilder:default=8080
	Port int32 `json:"port,omitempty"`
}

// WebAppStatus is the OBSERVED state, written only by the controller
type WebAppStatus struct {
	// ReadyReplicas mirrors the child Deployment's ready count.
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`

	// ObservedGeneration is the spec generation the controller last acted on.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions follow the standard Kubernetes convention (Available, Progressing...).
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Image",type=string,JSONPath=`.spec.image`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyReplicas`
type WebApp struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   WebAppSpec   `json:"spec,omitempty"`
	Status WebAppStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
// WebAppList is required so clients can LIST WebApps.
type WebAppList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []WebApp `json:"items"`
}

// init registers both types with the SchemeBuilder defined earlier.
func init() {
	SchemeBuilder.Register(&WebApp{}, &WebAppList{})
}
