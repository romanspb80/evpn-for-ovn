package main

import (
	"os"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	evpncorev1alpha1 "github.com/romanspb80/evpn-for-ovn/deploy/operator/api/v1alpha1"
	"github.com/romanspb80/evpn-for-ovn/deploy/operator/controllers"
)

var scheme = runtime.NewScheme()

func init() {
	_ = clientgoscheme.AddToScheme(scheme)
	_ = evpncorev1alpha1.AddToScheme(scheme)
}

func main() {
	logger := zap.New(zap.UseDevMode(true))
	ctrl.SetLogger(logger)

	cfg := ctrl.GetConfigOrDie()
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 scheme,
		LeaderElection:         false,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
	})
	if err != nil {
		logger.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if err = (&controllers.EvpnCoreReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme()}).SetupWithManager(mgr); err != nil {
		logger.Error(err, "unable to create controller")
		os.Exit(1)
	}

	logger.Info("starting evpn core operator")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		logger.Error(err, "problem running manager")
		os.Exit(1)
	}
}
