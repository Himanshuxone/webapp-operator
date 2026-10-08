package controller

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	appsv1alpha1 "github.com/Himanshuxone/webapp-operator/api/v1alpha1"
)

const (
	// finalizerName blocks deletion of a WebApp until we finish external cleanup.
	finalizerName = "apps.example.io/finalizer"

	// conditionAvailable is the Type of the condition we publish in status.
	conditionAvailable = "Available"

	// Fallbacks used if the API server did not apply the CRD defaults.
	defaultReplicas int32 = 1
	defaultPort     int32 = 8080
)

// WebAppReconciler reconciles a WebApp Object
type WebAppReconciler struct {
	// Embedding client.Client gives us Get/List/Create/Update/Delete and Status()
	// Reads are served from the informer cache; writes go to the API Server
	client.Client

	// Scheme maps Go types to GVKs. SetController needs it to build ownerRefs.
	Scheme *runtime.Scheme
}

// RBAC markers: controller-gen turns these into a ClusterRole (see Step 3).
// +kubebuilder:rbac:groups=apps.example.io,resources=webapps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps.example.io,resources=webapps/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps.example.io,resources=webapps/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete

// Reconcile drives the actual state of the cluster toward the WebApp spec.
// It is LEVEL-TRIGGERED: it receives only a name, never "what changed",
// and must be safe to run any number of times (idempotent).

func (r *WebAppReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := logf.FromContext(ctx)

	// 1. Fetch the WebApp named in the request (from the cache).
	webapp := &appsv1alpha1.WebApp{}
	if err := r.Get(ctx, req.NamespacedName, webapp); err != nil {
		if apierrors.IsNotFound(err) {
			// Deleted before we got here. Children are garbage-collected via ownerRefs.
			return ctrl.Result{}, nil
		}
		// Any other error: return it so the workqueue retries with backoff.
		return ctrl.Result{}, err
	}

	// 2. Deletion path: DeletionTimestamp is set while our finalizer holds the object.
	if !webapp.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, webapp)
	}

	// 3. Make sure our finalizer is present BEFORE creating anything that needs cleanup.
	if !controllerutil.ContainsFinalizer(webapp, finalizerName) {
		controllerutil.AddFinalizer(webapp, finalizerName)
		// Update writes back and refreshes webapp in place (new resourceVersion).
		if err := r.Update(ctx, webapp); err != nil {
			return ctrl.Result{}, err
		}
	}

	// 4. Create or update the child Deployment.
	deploy, err := r.reconcileDeployment(ctx, webapp)
	if err != nil {
		logger.Error(err, "failed to reconcile the Deployment")
		return ctrl.Result{}, err
	}

	// Report what we observed
	if err := r.updateStatus(ctx, webapp, deploy); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// reconcileDeployment makes the child Deployment match the WebApp spec.
func (r *WebAppReconciler) reconcileDeployment(ctx context.Context, webapp *appsv1alpha1.WebApp) (*appsv1.Deployment, error) {
	// Only the identity (name/namespace) is set here. CreateOrUpdate fills in the rest.
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: webapp.Name, Namespace: webapp.Namespace},
	}

	// CreateOrUpdate: GET the object; if missing, run mutate then CREATE;
	// if present, run mutate and UPDATE only if something actually changed.
	result, err := controllerutil.CreateOrUpdate(ctx, r.Client, deploy, func() error {
		return r.mutateDeployment(webapp, deploy)
	})
	if err != nil {
		return nil, err
	}

	if result != controllerutil.OperationResultNone {
		logf.FromContext(ctx).Info("Deployment reconcile", "operation", result)
	}

	return deploy, nil
}

// mutateDeployment writes the DESIRED fields onto the Deployment, in place.
// It must only set fields we own, so it can run on both new and existing objects.
func (r *WebAppReconciler) mutateDeployment(webapp *appsv1alpha1.WebApp, deploy *appsv1.Deployment) error {
	labels := map[string]string{
		"app.kubernetes.io/name":     "webapp",
		"app.kubernetes.io/instance": webapp.Name,
	}

	deploy.Labels = labels
	// spec.selector is IMMUTABLE after creation, so set it only on a brand-new object.
	// A zero CreationTimestamp means the object has not been created yet.
	if deploy.CreationTimestamp.IsZero() {
		deploy.Spec.Selector = &metav1.LabelSelector{MatchLabels: labels}
	}

	// ptr.To copies the value, so we never alias memory owned by the cached WebApp
	deploy.Spec.Replicas = ptr.To(desiredReplicas(webapp))
	deploy.Spec.Template.Labels = labels

	// Edit container[0] IN PLACE instead of replacing the slice. This keeps
	// server-applied defaults (terminationMessagePath etc.), so CreateOrUpdate
	// sees "no change" on later runs and skips the Update call.
	if len(deploy.Spec.Template.Spec.Containers) == 0 {
		deploy.Spec.Template.Spec.Containers = make([]corev1.Container, 1)
	}

	// this will only work in case of a single image and container. In case of a sidecar container this will not work
	// and you have to create a new loop to update the specific container by name
	c := &deploy.Spec.Template.Spec.Containers[0]
	c.Name = "app"
	c.Image = webapp.Spec.Image

	// Protocol is set explicitly because the API server defaults it to TCP,
	// and leaving it empty would look like a diff on every reconcile.
	c.Ports = []corev1.ContainerPort{{ContainerPort: desiredPort(webapp), Protocol: corev1.ProtocolTCP}}

	// Sets metadata.ownerReferences with controller=true and blockOwnerDeletion=true.
	// This gives us garbage collection AND lets Owns() map Deployment events back to the WebApp.
	return controllerutil.SetControllerReference(webapp, deploy, r.Scheme)

}

// updateStatus copies observed facts from the Deployment into WebApp.status.
func (r *WebAppReconciler) updateStatus(ctx context.Context, webapp *appsv1alpha1.WebApp, deploy *appsv1.Deployment) error {
	// Snapshot before editing so we can skip the API call if nothing changed.
	before := webapp.DeepCopy()

	desired := desiredReplicas(webapp)
	ready := deploy.Status.ReadyReplicas

	cond := metav1.Condition{
		Type:               conditionAvailable,
		ObservedGeneration: webapp.Generation,
	}
	if ready == desired {
		cond.Status = metav1.ConditionTrue
		cond.Reason = "AllReplicasReady"
	} else {
		cond.Status = metav1.ConditionFalse
		cond.Reason = "ReplicasNotReady"
	}
	cond.Message = fmt.Sprintf("%d/%d replicas ready", ready, desired)

	// SetStatusCondition upserts by Type and only bumps LastTransitionTime
	// when Status actually flips, which is the convention clients rely on.
	meta.SetStatusCondition(&webapp.Status.Conditions, cond)
	webapp.Status.ReadyReplicas = ready
	webapp.Status.ObservedGeneration = webapp.Generation

	if equality.Semantic.DeepEqual(before.Status, webapp.Status) {
		return nil
	}
	// Status() targets the /status subresource, so this cannot modify spec.
	return r.Status().Update(ctx, webapp)
}

// reconcileDelete runs the clean up and then releases the finalizer so deletion can finish.
func (r *WebAppReconciler) reconcileDelete(ctx context.Context, webapp *appsv1alpha1.WebApp) (ctrl.Result, error) {
	if controllerutil.ContainsFinalizer(webapp, finalizerName) {
		if err := r.cleanupExternalResources(ctx, webapp); err != nil {
			// Keep the finalizer and retry later; the object stays in Terminating.
			return ctrl.Result{}, err
		}
		controllerutil.RemoveFinalizer(webapp, finalizerName)
		if err := r.Update(ctx, webapp); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
	}
	// The Deployment is deleted by the garbage collector via its ownerReference.
	return ctrl.Result{}, nil
}

// cleanupExternalResources is the hook for things Kubernetes cannot GC for us:
// a DNS record, a cloud load balancer, a row in a database. It is a stub here.
func (r *WebAppReconciler) cleanupExternalResources(ctx context.Context, webapp *appsv1alpha1.WebApp) error {
	logf.FromContext(ctx).Info("running external cleanup", "webapp", webapp.Name)
	return nil
}

// SetupWithManager registers the controller and declares what it watches.
func (r *WebAppReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		// For: every WebApp event enqueues that WebApp's namespace/name.
		For(&appsv1alpha1.WebApp{}).
		// Owns: events on Deployments whose controller ownerRef is a WebApp
		// enqueue the OWNER's key. This is how status refreshes when pods become ready.
		Owns(&appsv1.Deployment{}).
		Complete(r)
}

// desiredReplicas returns spec.replicas, falling back to the default if unset.
func desiredReplicas(w *appsv1alpha1.WebApp) int32 {
	if w.Spec.Replicas == nil {
		return defaultReplicas
	}
	return *w.Spec.Replicas
}

// desiredPort returns spec.port, falling back to the default if zero.
func desiredPort(w *appsv1alpha1.WebApp) int32 {
	if w.Spec.Port == 0 {
		return defaultPort
	}
	return w.Spec.Port
}
