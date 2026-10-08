package main

import (
	"flag"
	"os"

	"github.com/Himanshuxone/webapp-operator/internal/controller"

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	appsv1alpha1 "github.com/Himanshuxone/webapp-operator/api/v1alpha1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

// scheme is the registry of every Go type this process can encode or decode.
var scheme = runtime.NewScheme()

func init() {
	// Built-in types (Deployment, Pod, ...). Forgetting this gives
	// "no kind is registered for the type v1.Deployment".
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	// Our WebApp and WebAppList types.
	utilruntime.Must(appsv1alpha1.AddToScheme(scheme))
}

func main() {
	var metricsAddr, probeAddr string
	var enableLeaderElection bool

	// Loopback defaults avoid the Windows Firewall prompt during local dev.
	// Use ":8080" and ":8081" when running inside a cluster.
	flag.StringVar(&metricsAddr, "metrics-bind-address", "127.0.0.1:8080", "Metrics endpoint address")
	flag.StringVar(&probeAddr, "health-probe-bind-address", "127.0.0.1:8081", "Health probe address")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election so only one replica reconciles at a time")
	flag.Parse()

	// Human-readable logs for development.
	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))
	setupLog := ctrl.Log.WithName("setup")

	// GetConfigOrDie finds credentials: --kubeconfig flag, $KUBECONFIG,
	// %USERPROFILE%\.kube\config, or in-cluster service account.
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		// Leader election uses a Lease object. Replicas race for it; only the winner
		// runs controllers, which prevents two replicas fighting over the same objects.
		LeaderElection:   enableLeaderElection,
		LeaderElectionID: "webapp-operator.apps.example.io",
	})
	if err != nil {
		setupLog.Error(err, "unable to create manager")
		os.Exit(1)
	}

	// Hand the reconciler the manager's cached client and shared scheme.
	if err := (&controller.WebAppReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "WebApp")
		os.Exit(1)
	}

	// Second controller in the same manager: shares the same cache, client and leader election.
	if err := (&controller.BatchJobReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "BatchJob")
		os.Exit(1)
	}

	// /healthz = process is alive, /readyz = ready to serve. Kubernetes probes use these.
	_ = mgr.AddHealthzCheck("healthz", healthz.Ping)
	_ = mgr.AddReadyzCheck("readyz", healthz.Ping)

	setupLog.Info("starting manager")
	// SetupSignalHandler cancels the context on Ctrl+C, giving a graceful shutdown.
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "manager exited with error")
		os.Exit(1)
	}
}
