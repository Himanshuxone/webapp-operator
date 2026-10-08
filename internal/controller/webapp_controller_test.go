package controller

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	appsv1alpha1 "github.com/Himanshuxone/webapp-operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

func TestWebAppLifecycle(t *testing.T) {
	g := gomega.NewWithT(t)

	// Build a scheme that knows both built-in types and WebApp.
	scheme := runtime.NewScheme()
	g.Expect(clientgoscheme.AddToScheme(scheme)).To(gomega.Succeed())
	g.Expect(appsv1alpha1.AddToScheme(scheme)).To(gomega.Succeed())

	// Start a throwaway control plane and install our generated CRD into it.
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	g.Expect(err).NotTo(gomega.HaveOccurred())
	t.Cleanup(func() { _ = env.Stop() }) // runs last (cleanups are LIFO)

	// Run our real controller inside a manager, just like main.go does.
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:  scheme,
		Metrics: metricsserver.Options{BindAddress: "0"}, // "0" disables metrics; avoids port clashes
	})
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect((&WebAppReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme()}).
		SetupWithManager(mgr)).To(gomega.Succeed())

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel) // runs before env.Stop(), so the manager stops first
	go func() { _ = mgr.Start(ctx) }()

	// A direct (uncached) client for assertions, so we never read stale cache data.
	k8s, err := client.New(cfg, client.Options{Scheme: scheme})
	g.Expect(err).NotTo(gomega.HaveOccurred())

	key := types.NamespacedName{Name: "demo", Namespace: "default"}
	wait := func(f func(gomega.Gomega)) {
		g.Eventually(f, 15*time.Second, 200*time.Millisecond).Should(gomega.Succeed())
	}

	// 1. Create a WebApp.
	webapp := &appsv1alpha1.WebApp{
		ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
		Spec:       appsv1alpha1.WebAppSpec{Image: "nginx:1.27", Replicas: ptr.To(int32(2)), Port: 80},
	}
	g.Expect(k8s.Create(ctx, webapp)).To(gomega.Succeed())

	// 2. The controller should create an owned Deployment matching the spec.
	deploy := &appsv1.Deployment{}
	wait(func(g gomega.Gomega) {
		g.Expect(k8s.Get(ctx, key, deploy)).To(gomega.Succeed())
		g.Expect(*deploy.Spec.Replicas).To(gomega.Equal(int32(2)))
		g.Expect(deploy.Spec.Template.Spec.Containers[0].Image).To(gomega.Equal("nginx:1.27"))
		g.Expect(metav1.IsControlledBy(deploy, webapp)).To(gomega.BeTrue())
	})

	// 3. envtest has no kube-controller-manager, so pods never start.
	//    We play that role: report 2 ready replicas on the Deployment status.
	deploy.Status.Replicas = 2
	deploy.Status.ReadyReplicas = 2
	deploy.Status.AvailableReplicas = 2
	g.Expect(k8s.Status().Update(ctx, deploy)).To(gomega.Succeed())

	// 4. Owns() should wake the reconciler, which copies that into WebApp.status.
	wait(func(g gomega.Gomega) {
		got := &appsv1alpha1.WebApp{}
		g.Expect(k8s.Get(ctx, key, got)).To(gomega.Succeed())
		g.Expect(got.Status.ReadyReplicas).To(gomega.Equal(int32(2)))
		g.Expect(meta.IsStatusConditionTrue(got.Status.Conditions, conditionAvailable)).To(gomega.BeTrue())
	})

	// 5. Delete: the finalizer must be released so the object actually disappears.
	g.Expect(k8s.Delete(ctx, webapp)).To(gomega.Succeed())
	wait(func(g gomega.Gomega) {
		err := k8s.Get(ctx, key, &appsv1alpha1.WebApp{})
		g.Expect(apierrors.IsNotFound(err)).To(gomega.BeTrue())
	})
	// Not asserted: Deployment garbage collection. That is done by kube-controller-manager,
	// which envtest doesn't run. Checking the ownerRef in step 2 is the correct proxy.
}
