package controller

import (
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"

	appsv1alpha1 "github.com/Himanshuxone/webapp-operator/api/v1alpha1"
)

// TestComputePhase is table-driven: one row per scenario, no cluster needed.
func TestComputePhase(t *testing.T) {
	cond := func(typ batchv1.JobConditionType, reason string) batchv1.JobCondition {
		return batchv1.JobCondition{Type: typ, Status: corev1.ConditionTrue, Reason: reason}
	}

	tests := []struct {
		name       string
		job        batchv1.Job
		workers    int32
		wantPhase  appsv1alpha1.BatchJobPhase
		wantReason string
	}{
		{"fresh job, no pods yet", batchv1.Job{}, 3,
			appsv1alpha1.PhaseQueued, "WaitingForWorkers"},
		{"suspended by queue manager", batchv1.Job{Spec: batchv1.JobSpec{Suspend: ptr.To(true)}}, 3,
			appsv1alpha1.PhaseQueued, "AdmissionPending"},
		{"partial gang: 2 of 3 ready", batchv1.Job{Status: batchv1.JobStatus{Ready: ptr.To(int32(2))}}, 3,
			appsv1alpha1.PhaseQueued, "WaitingForWorkers"},
		{"full gang ready", batchv1.Job{Status: batchv1.JobStatus{Ready: ptr.To(int32(3))}}, 3,
			appsv1alpha1.PhaseRunning, "AllWorkersReady"},
		{"job complete", batchv1.Job{Status: batchv1.JobStatus{
			Conditions: []batchv1.JobCondition{cond(batchv1.JobComplete, "")}}}, 3,
			appsv1alpha1.PhaseSucceeded, "AllWorkersSucceeded"},
		{"walltime exceeded", batchv1.Job{Status: batchv1.JobStatus{
			Conditions: []batchv1.JobCondition{cond(batchv1.JobFailed, "DeadlineExceeded")}}}, 3,
			appsv1alpha1.PhaseFailed, "DeadlineExceeded"},
		{"failed without a reason", batchv1.Job{Status: batchv1.JobStatus{
			Conditions: []batchv1.JobCondition{cond(batchv1.JobFailed, "")}}}, 3,
			appsv1alpha1.PhaseFailed, "JobFailed"},
		{"terminal state beats stale ready count", batchv1.Job{Status: batchv1.JobStatus{
			Ready:      ptr.To(int32(3)),
			Conditions: []batchv1.JobCondition{cond(batchv1.JobComplete, "")}}}, 3,
			appsv1alpha1.PhaseSucceeded, "AllWorkersSucceeded"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			phase, reason := computePhase(&tc.job, tc.workers)
			if phase != tc.wantPhase || reason != tc.wantReason {
				t.Errorf("got (%s, %s), want (%s, %s)", phase, reason, tc.wantPhase, tc.wantReason)
			}
		})
	}
}
