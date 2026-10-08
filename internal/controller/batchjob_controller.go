package controller

import (
	"context"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	appsv1alpha1 "github.com/Himanshuxone/webapp-operator/api/v1alpha1"
)

const (
	// Condition types published in status.conditions.
	condQueued    = "Queued"
	condRunning   = "Running"
	condCompleted = "Completed"

	// ownerLabel ties pods and the Service back to their BatchJob (used as the Service selector).
	ownerLabel = "apps.example.io/batchjob"

	// queueLabel is the label Kueue watches to decide which queue manages a Job.
	queueLabel = "kueue.x-k8s.io/queue-name"
)

// BatchJobReconciler reconciles a BatchJob object.
type BatchJobReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// RBAC markers. Least privilege: we never delete Jobs or Services ourselves, the garbage collector does.
// +kubebuilder:rbac:groups=apps.example.io,resources=batchjobs,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps.example.io,resources=batchjobs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch

// Reconcile is level-triggered: it looks at current state and converges, whatever the event was.
// There is deliberately NO finalizer: everything we create is cluster-internal, so owner
// references and the garbage collector clean up. Add a finalizer only for external resources.
func (r *BatchJobReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	bj := &appsv1alpha1.BatchJob{}
	if err := r.Get(ctx, req.NamespacedName, bj); err != nil {
		// NotFound means it was deleted; children go away via ownerReferences.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !bj.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil // already being deleted: do not create anything new
	}

	// Service first, so pod DNS records exist before workers start.
	if err := r.reconcileService(ctx, bj); err != nil {
		return ctrl.Result{}, err
	}

	job, err := r.reconcileJob(ctx, bj)
	if err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, r.updateStatus(ctx, bj, job)
}

// reconcileService keeps a headless Service so worker i is reachable as <name>-<i>.<name>.
func (r *BatchJobReconciler) reconcileService(ctx context.Context, bj *appsv1alpha1.BatchJob) error {
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: bj.Name, Namespace: bj.Namespace}}

	// A Service is mutable, so CreateOrUpdate is the right tool here.
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		svc.Spec.ClusterIP = corev1.ClusterIPNone // headless: DNS returns pod IPs, no virtual IP
		svc.Spec.Selector = map[string]string{ownerLabel: bj.Name}
		// Publish DNS records for pods that are not Ready yet. Workers need to find
		// each other DURING startup, before any readiness probe could pass.
		svc.Spec.PublishNotReadyAddresses = true
		return controllerutil.SetControllerReference(bj, svc, r.Scheme)
	})
	return err
}

// reconcileJob creates the Job once and afterwards only reads it.
func (r *BatchJobReconciler) reconcileJob(ctx context.Context, bj *appsv1alpha1.BatchJob) (*batchv1.Job, error) {
	job := &batchv1.Job{}
	err := r.Get(ctx, client.ObjectKeyFromObject(bj), job)
	if err == nil {
		return job, nil // exists: Job.spec.template is immutable, so there is nothing to update
	}
	if !apierrors.IsNotFound(err) {
		return nil, err
	}

	job = r.buildJob(bj)
	if err := controllerutil.SetControllerReference(bj, job, r.Scheme); err != nil {
		return nil, err
	}
	// If the cache is stale and the Job already exists, Create returns AlreadyExists.
	// We return that error; the retry runs after the cache has caught up.
	if err := r.Create(ctx, job); err != nil {
		return nil, err
	}
	logf.FromContext(ctx).Info("created Job", "job", job.Name, "workers", bj.Spec.Workers)
	return job, nil
}

// buildJob translates the BatchJob spec into a batch/v1 Job.
func (r *BatchJobReconciler) buildJob(bj *appsv1alpha1.BatchJob) *batchv1.Job {
	// Deep copy so nothing we build aliases memory owned by the shared informer cache.
	spec := bj.Spec.DeepCopy()

	podLabels := map[string]string{ownerLabel: bj.Name}
	jobLabels := map[string]string{ownerLabel: bj.Name}
	if spec.QueueName != "" {
		// Kueue's webhook sees this label, creates the Job suspended, and unsuspends it
		// once quota for ALL workers is available. We do not set spec.suspend ourselves,
		// so the Job still runs normally if Kueue is not installed.
		jobLabels[queueLabel] = spec.QueueName
	}

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: bj.Name, Namespace: bj.Namespace, Labels: jobLabels},
		Spec: batchv1.JobSpec{
			// Indexed: each pod gets a stable rank in $JOB_COMPLETION_INDEX and hostname <name>-<rank>.
			CompletionMode: ptr.To(batchv1.IndexedCompletion),
			// completions == parallelism == workers: all ranks run at once and each must succeed.
			Completions:           ptr.To(spec.Workers),
			Parallelism:           ptr.To(spec.Workers),
			BackoffLimit:          ptr.To(spec.MaxRetries),
			ActiveDeadlineSeconds: spec.WalltimeSeconds, // the walltime limit
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: podLabels},
				Spec: corev1.PodSpec{
					// Never restart in place: a failed rank is replaced by the Job controller,
					// which makes failures visible in status and counts them toward the backoff limit.
					RestartPolicy: corev1.RestartPolicyNever,
					// Must equal the headless Service name to get per-pod DNS records.
					Subdomain: bj.Name,
					Containers: []corev1.Container{{
						Name:      "worker",
						Image:     spec.Image,
						Command:   spec.Command,
						Args:      spec.Args,
						Resources: spec.Resources,
					}},
				},
			},
		},
	}
}

// computePhase is a PURE function: Job in, phase and reason out. No client, no cluster.
// Keeping the decision logic separate makes it unit-testable on any OS.
func computePhase(job *batchv1.Job, workers int32) (appsv1alpha1.BatchJobPhase, string) {
	// Terminal states first: once the Job reports Complete or Failed, that is final.
	for _, c := range job.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		switch c.Type {
		case batchv1.JobComplete:
			return appsv1alpha1.PhaseSucceeded, "AllWorkersSucceeded"
		case batchv1.JobFailed:
			if c.Reason == "" {
				return appsv1alpha1.PhaseFailed, "JobFailed"
			}
			return appsv1alpha1.PhaseFailed, c.Reason // e.g. DeadlineExceeded, BackoffLimitExceeded
		}
	}

	// "Running" means the whole gang is up, not just some of it.
	if ptr.Deref(job.Status.Ready, 0) >= workers {
		return appsv1alpha1.PhaseRunning, "AllWorkersReady"
	}

	// Suspended by a queue manager such as Kueue: waiting for quota.
	if ptr.Deref(job.Spec.Suspend, false) {
		return appsv1alpha1.PhaseQueued, "AdmissionPending"
	}
	return appsv1alpha1.PhaseQueued, "WaitingForWorkers"
}

// updateStatus mirrors the Job's progress into BatchJob.status.
func (r *BatchJobReconciler) updateStatus(ctx context.Context, bj *appsv1alpha1.BatchJob, job *batchv1.Job) error {
	before := bj.DeepCopy() // snapshot, so we can skip the write when nothing changed

	phase, reason := computePhase(job, bj.Spec.Workers)
	ready := ptr.Deref(job.Status.Ready, 0)

	bj.Status.Phase = phase
	bj.Status.ReadyWorkers = ready
	bj.Status.SucceededWorkers = job.Status.Succeeded
	bj.Status.FailedWorkers = job.Status.Failed
	bj.Status.StartTime = copyTime(job.Status.StartTime)
	bj.Status.CompletionTime = copyTime(job.Status.CompletionTime)
	bj.Status.ObservedGeneration = bj.Generation

	// Exactly one of the three conditions is True at any time.
	msg := fmt.Sprintf("%d/%d workers ready", ready, bj.Spec.Workers)
	finished := phase == appsv1alpha1.PhaseSucceeded || phase == appsv1alpha1.PhaseFailed
	setCondition(bj, condQueued, phase == appsv1alpha1.PhaseQueued, reason, "NotQueued", msg)
	setCondition(bj, condRunning, phase == appsv1alpha1.PhaseRunning, reason, "NotRunning", msg)
	setCondition(bj, condCompleted, finished, reason, "NotFinished", msg)

	if equality.Semantic.DeepEqual(before.Status, bj.Status) {
		return nil
	}
	// Writes through the /status subresource, so it can never modify spec.
	return r.Status().Update(ctx, bj)
}

// setCondition upserts one condition. Reason is required to be non-empty CamelCase,
// so the False case gets its own fixed reason.
func setCondition(bj *appsv1alpha1.BatchJob, condType string, on bool, onReason, offReason, message string) {
	status, reason := metav1.ConditionFalse, offReason
	if on {
		status, reason = metav1.ConditionTrue, onReason
	}
	// LastTransitionTime only moves when Status flips, which is what clients expect.
	meta.SetStatusCondition(&bj.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: bj.Generation,
	})
}

// copyTime copies a possibly-nil timestamp pointer so we never share memory with the Job object.
func copyTime(t *metav1.Time) *metav1.Time {
	if t == nil {
		return nil
	}
	c := *t
	return &c
}

// SetupWithManager declares what this controller watches.
func (r *BatchJobReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&appsv1alpha1.BatchJob{}).
		// Job status changes (pods becoming ready or finishing) wake the OWNER BatchJob.
		Owns(&batchv1.Job{}).
		// If someone deletes the Service by hand, the owner is reconciled and recreates it.
		Owns(&corev1.Service{}).
		Complete(r)
}
