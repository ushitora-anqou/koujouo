/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package main sets up and runs the koujouo controller manager.
package main

import (
	"flag"
	"fmt"
	"os"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	koujouoanqounetv1 "github.com/ushitora-anqou/koujouo/api/v1"
	"github.com/ushitora-anqou/koujouo/internal/controller"
	"github.com/ushitora-anqou/koujouo/internal/version"
	// +kubebuilder:scaffold:imports
)

// managerConfig holds the command-line configuration for the manager.
type managerConfig struct {
	metricsAddr          string
	probeAddr            string
	enableLeaderElection bool
	secureMetrics        bool
	webhookCertPath      string
	webhookCertName      string
	webhookCertKey       string
	metricsCertPath      string
	metricsCertName      string
	metricsCertKey       string
}

// controllerImageEnv names the environment variable that carries the
// controller image deployed to the agent StatefulSets. The Kustomize
// deployment injects it with the same value as the manager image.
const controllerImageEnv = "KOUJOUO_CONTROLLER_IMAGE"

func main() {
	cfg := managerConfig{} //nolint:exhaustruct_v5 // Fields are populated from flags.

	flag.StringVar(&cfg.metricsAddr, "metrics-bind-address", "0",
		"The address the metrics endpoint binds to. "+
			"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&cfg.probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&cfg.enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&cfg.secureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	flag.StringVar(&cfg.webhookCertPath, "webhook-cert-path", "", "The directory that contains the webhook certificate.")
	flag.StringVar(&cfg.webhookCertName, "webhook-cert-name", "tls.crt", "The name of the webhook certificate file.")
	flag.StringVar(&cfg.webhookCertKey, "webhook-cert-key", "tls.key", "The name of the webhook key file.")
	flag.StringVar(&cfg.metricsCertPath, "metrics-cert-path", "",
		"The directory that contains the metrics server certificate.")
	flag.StringVar(&cfg.metricsCertName, "metrics-cert-name", "tls.crt",
		"The name of the metrics server certificate file.")
	flag.StringVar(&cfg.metricsCertKey, "metrics-cert-key", "tls.key", "The name of the metrics server key file.")

	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	setupLog := ctrl.Log.WithName("setup")

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(koujouoanqounetv1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme

	err := run(cfg, scheme, setupLog)
	if err != nil {
		setupLog.Error(err, "Failed to run manager")
		os.Exit(1)
	}
}

// run builds the manager and its servers, then blocks until the manager stops.
func run(cfg managerConfig, scheme *runtime.Scheme, setupLog logr.Logger) error {
	webhookServer := newWebhookServer(cfg, setupLog)

	metricsServerOptions := newMetricsServerOptions(cfg, setupLog)

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{ //nolint:exhaustruct_v5
		Scheme:                 scheme,
		Metrics:                metricsServerOptions,
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: cfg.probeAddr,
		LeaderElection:         cfg.enableLeaderElection,
		LeaderElectionID:       "182dde0e.koujouo.anqou.net",
		// LeaderElectionReleaseOnCancel defines if the leader should step down voluntarily
		// when the Manager ends. This requires the binary to immediately end when the
		// Manager is stopped, otherwise, this setting is unsafe. Setting this significantly
		// speeds up voluntary leader transitions as the new leader don't have to wait
		// LeaseDuration time first.
		//
		// In the default scaffold provided, the program ends immediately after
		// the manager stops, so would be fine to enable this option. However,
		// if you are doing or is intended to do any operation such as perform cleanups
		// after the manager stops then its usage might be unsafe.
		// LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		return fmt.Errorf("failed to start manager: %w", err)
	}

	controllerImage := os.Getenv(controllerImageEnv)
	if controllerImage == "" {
		setupLog.Info(controllerImageEnv + " is not set; agent StatefulSets cannot be created")
	}

	// The poll, retry and progress intervals fall back to the reconciler
	// defaults.
	err = (&controller.FactorioServerReconciler{
		Client:                    mgr.GetClient(),
		Scheme:                    mgr.GetScheme(),
		ControllerImage:           controllerImage,
		AgentBinaryPath:           "", // The agent binary of the controller image.
		Dialer:                    &controller.TCPAgentDialer{},
		Recorder:                  mgr.GetEventRecorder("factorioserver"),
		PollInterval:              0,
		RetryInterval:             0,
		ConfigureProgressInterval: 0,
	}).SetupWithManager(mgr)
	if err != nil {
		return fmt.Errorf("failed to create controller: %w", err)
	}
	// +kubebuilder:scaffold:builder

	err = mgr.AddHealthzCheck("healthz", healthz.Ping)
	if err != nil {
		return fmt.Errorf("failed to set up health check: %w", err)
	}

	err = mgr.AddReadyzCheck("readyz", healthz.Ping)
	if err != nil {
		return fmt.Errorf("failed to set up ready check: %w", err)
	}

	setupLog.Info("Starting manager", "version", version.Agent)

	err = mgr.Start(ctrl.SetupSignalHandler())
	if err != nil {
		return fmt.Errorf("failed to run manager: %w", err)
	}

	return nil
}

// newWebhookServer builds the webhook server, optionally watching the
// certificate provided via flags.
//
//nolint:ireturn // webhook.NewServer returns the webhook.Server interface.
func newWebhookServer(
	cfg managerConfig,
	setupLog logr.Logger,
) webhook.Server {
	webhookServerOptions := webhook.Options{}

	if len(cfg.webhookCertPath) > 0 {
		setupLog.Info("Initializing webhook certificate watcher using provided certificates",
			"webhook-cert-path", cfg.webhookCertPath,
			"webhook-cert-name", cfg.webhookCertName,
			"webhook-cert-key", cfg.webhookCertKey)

		webhookServerOptions.CertDir = cfg.webhookCertPath
		webhookServerOptions.CertName = cfg.webhookCertName
		webhookServerOptions.KeyName = cfg.webhookCertKey
	}

	return webhook.NewServer(webhookServerOptions)
}

// newMetricsServerOptions builds the metrics server options, optionally
// using the certificate provided via flags.
func newMetricsServerOptions(
	cfg managerConfig,
	setupLog logr.Logger,
) metricsserver.Options {
	// Metrics endpoint is enabled in 'config/default/kustomization.yaml'.
	// The Metrics options configure the server. More info:
	// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.24.1/pkg/metrics/server
	// - https://book.kubebuilder.io/reference/metrics.html
	metricsServerOptions := metricsserver.Options{
		BindAddress:   cfg.metricsAddr,
		SecureServing: cfg.secureMetrics,
	}

	if cfg.secureMetrics {
		// FilterProvider is used to protect the metrics endpoint with authn/authz.
		// These configurations ensure that only authorized users and service accounts
		// can access the metrics endpoint. The RBAC are configured in
		// 'config/rbac/kustomization.yaml'. More info:
		// https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.24.1/pkg/metrics/filters#WithAuthenticationAndAuthorization
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	// If the certificate is not specified, controller-runtime will automatically
	// generate self-signed certificates for the metrics server. While convenient for development and testing,
	// this setup is not recommended for production.
	if len(cfg.metricsCertPath) > 0 {
		setupLog.Info("Initializing metrics certificate watcher using provided certificates",
			"metrics-cert-path", cfg.metricsCertPath, "metrics-cert-name", cfg.metricsCertName,
			"metrics-cert-key", cfg.metricsCertKey)

		metricsServerOptions.CertDir = cfg.metricsCertPath
		metricsServerOptions.CertName = cfg.metricsCertName
		metricsServerOptions.KeyName = cfg.metricsCertKey
	}

	return metricsServerOptions
}
