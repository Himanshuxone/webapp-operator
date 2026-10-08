package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// BatchJobPhase is a one-word summary of where the job is. Conditions carry the detail.
type BatchJobPhase string

const (
	PhaseQueued    BatchJobPhase = "Queued"    // waiting for admission or for all workers to be ready
	PhaseRunning   BatchJobPhase = "Running"   // every worker is up
	PhaseSucceeded BatchJobPhase = "Succeeded" // every worker exited 0
	PhaseFailed    BatchJobPhase = "Failed"    // retries exhausted or walltime exceeded
)

// BatchJobSpec is the DESIRED state. The controller reads it once, when it creates the Job.
type BatchJobSpec struct {
	// Image is the container image every worker runs.
	// The CEL rule makes this field read-only after creation, so the API server rejects edits.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=512
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="image is immutable"
	Image string `json:"image"`

	// Command and Args override the image entrypoint, like the same fields on a Pod.
	Command []string `json:"command,omitempty"`
	Args    []string `json:"args,omitempty"`

	// Workers is the number of pods that must run TOGETHER (the "gang" size, like MPI ranks).
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1024
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="workers is immutable"
	Workers int32 `json:"workers"`

	// Resources is requests/limits PER WORKER, e.g. cpu, memory, nvidia.com/gpu.
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// WalltimeSeconds is the maximum run time, like Slurm's --time. Nil means no limit.
	// +kubebuilder:validation:Minimum=1
	WalltimeSeconds *int64 `json:"walltimeSeconds,omitempty"`

	// MaxRetries is how many failed pods are tolerated before the whole job fails.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=0
	MaxRetries int32 `json:"maxRetries,omitempty"`

	// QueueName is a Kueue LocalQueue. Empty means "run immediately".
	// Requires Kueue to be installed in the cluster; otherwise the label is ignored.
	QueueName string `json:"queueName,omitempty"`
}

// BatchJobStatus is the OBSERVED state, written only by the controller.
type BatchJobStatus struct {
	Phase              BatchJobPhase      `json:"phase,omitempty"`
	ReadyWorkers       int32              `json:"readyWorkers,omitempty"`
	SucceededWorkers   int32              `json:"succeededWorkers,omitempty"`
	FailedWorkers      int32              `json:"failedWorkers,omitempty"`
	StartTime          *metav1.Time       `json:"startTime,omitempty"`
	CompletionTime     *metav1.Time       `json:"completionTime,omitempty"`
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=bj
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Workers",type=integer,JSONPath=`.spec.workers`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyWorkers`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// BatchJob runs a gang of identical workers to completion.
type BatchJob struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   BatchJobSpec   `json:"spec,omitempty"`
	Status BatchJobStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// BatchJobList is required so clients can LIST BatchJobs.
type BatchJobList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []BatchJob `json:"items"`
}

func init() {
	SchemeBuilder.Register(&BatchJob{}, &BatchJobList{})
}
