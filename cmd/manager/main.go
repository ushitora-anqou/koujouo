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
	"fmt"
	"os"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"github.com/go-logr/logr"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"go.uber.org/zap/zapcore"
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

// defaultConfigPath is where the manager reads its configuration when
// --config is not given. The deployment of the Helm chart mounts the
// configuration there as a ConfigMap.
const defaultConfigPath = "/etc/koujouo/manager.yaml"

// Defaults of the configuration. examples/manager.yaml lists the same keys
// with the same values.
const (
	defaultMetricsBindAddress     = "0"
	defaultHealthProbeBindAddress = ":8081"
	defaultLeaderElection         = false
	defaultSecureMetrics          = true
	defaultWebhookCertPath        = ""
	defaultWebhookCertName        = "tls.crt"
	defaultWebhookCertKey         = "tls.key"
	defaultMetricsCertPath        = ""
	defaultMetricsCertName        = "tls.crt"
	defaultMetricsCertKey         = "tls.key"
	defaultControllerImage        = ""
	defaultLogDevelopment         = true
	defaultLogLevel               = "info"
)

// logConfig is the log section of the configuration.
type logConfig struct {
	// Development selects the human-readable console encoding instead of
	// JSON.
	Development bool `mapstructure:"development"`

	// Level is the minimum level the manager logs: debug, info, warn,
	// error, dpanic, panic or fatal.
	Level string `mapstructure:"level"`
}

// managerConfig is the configuration of the manager, read from the YAML file
// given with --config.
type managerConfig struct {
	MetricsBindAddress string `mapstructure:"metricsBindAddress"`

	HealthProbeBindAddress string `mapstructure:"healthProbeBindAddress"`

	LeaderElection bool `mapstructure:"leaderElection"`

	SecureMetrics bool `mapstructure:"secureMetrics"`

	WebhookCertPath string `mapstructure:"webhookCertPath"`
	WebhookCertName string `mapstructure:"webhookCertName"`
	WebhookCertKey  string `mapstructure:"webhookCertKey"`

	MetricsCertPath string `mapstructure:"metricsCertPath"`
	MetricsCertName string `mapstructure:"metricsCertName"`
	MetricsCertKey  string `mapstructure:"metricsCertKey"`

	// ControllerImage is the image the agent StatefulSets run. The
	// controller deploys it for the agent Pods and streams /kojouo/agent
	// from its own image to the agents of another build.
	ControllerImage string `mapstructure:"controllerImage"`

	Log logConfig `mapstructure:"log"`
}

func main() {
	err := newRootCommand().Execute()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)

		os.Exit(1)
	}
}

// newRootCommand builds the root command of the manager, whose only flag
// names the configuration file.
func newRootCommand() *cobra.Command {
	var configPath string

	cmd := &cobra.Command{ //nolint:exhaustruct_v5 // The zero values of cobra.Command are valid.
		Use:   "manager",
		Short: "Run the koujouo controller manager",
		Long: "Run the koujouo controller manager, the Kubernetes operator that hosts\n" +
			"headless Factorio servers through FactorioServer objects in the cluster\n" +
			"of the current kubeconfig context.",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			cfg, err := loadConfig(configPath)
			if err != nil {
				return err
			}

			logger, err := newLogger(cfg.Log)
			if err != nil {
				return err
			}

			ctrl.SetLogger(logger)

			return run(cfg, ctrl.Log.WithName("setup"))
		},
	}

	cmd.Flags().StringVar(&configPath, "config", defaultConfigPath,
		"path to the YAML configuration file")

	return cmd
}

// loadConfig reads the YAML configuration file at path over the built-in
// defaults, which fill the keys the file leaves out.
func loadConfig(path string) (managerConfig, error) {
	var cfg managerConfig

	settings := viper.New()

	settings.SetDefault("metricsBindAddress", defaultMetricsBindAddress)
	settings.SetDefault("healthProbeBindAddress", defaultHealthProbeBindAddress)
	settings.SetDefault("leaderElection", defaultLeaderElection)
	settings.SetDefault("secureMetrics", defaultSecureMetrics)
	settings.SetDefault("webhookCertPath", defaultWebhookCertPath)
	settings.SetDefault("webhookCertName", defaultWebhookCertName)
	settings.SetDefault("webhookCertKey", defaultWebhookCertKey)
	settings.SetDefault("metricsCertPath", defaultMetricsCertPath)
	settings.SetDefault("metricsCertName", defaultMetricsCertName)
	settings.SetDefault("metricsCertKey", defaultMetricsCertKey)
	settings.SetDefault("controllerImage", defaultControllerImage)
	settings.SetDefault("log.development", defaultLogDevelopment)
	settings.SetDefault("log.level", defaultLogLevel)

	settings.SetConfigFile(path)
	settings.SetConfigType("yaml")

	err := settings.ReadInConfig()
	if err != nil {
		return cfg, fmt.Errorf("could not read the configuration file %s: %w", path, err)
	}

	err = settings.Unmarshal(&cfg)
	if err != nil {
		return cfg, fmt.Errorf("could not decode the configuration file %s: %w", path, err)
	}

	return cfg, nil
}

// newLogger builds the logger of the manager from the log section of the
// configuration.
func newLogger(cfg logConfig) (logr.Logger, error) {
	var (
		level  zapcore.Level
		logger logr.Logger
	)

	err := level.Set(cfg.Level)
	if err != nil {
		return logger, fmt.Errorf("could not parse the log level %q: %w", cfg.Level, err)
	}

	options := zap.Options{
		Development: cfg.Development,
		Level:       level,
	}

	return zap.New(zap.UseFlagOptions(&options)), nil
}

// run builds the manager and its servers, then blocks until the manager stops.
func run(cfg managerConfig, setupLog logr.Logger) error {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(koujouoanqounetv1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme

	webhookServer := newWebhookServer(cfg, setupLog)

	metricsServerOptions := newMetricsServerOptions(cfg, setupLog)

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{ //nolint:exhaustruct_v5
		Scheme:                 scheme,
		Metrics:                metricsServerOptions,
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: cfg.HealthProbeBindAddress,
		LeaderElection:         cfg.LeaderElection,
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

	if cfg.ControllerImage == "" {
		setupLog.Info("controllerImage is not set; agent StatefulSets cannot be created")
	}

	// The poll, retry and progress intervals fall back to the reconciler
	// defaults.
	err = (&controller.FactorioServerReconciler{
		Client:                    mgr.GetClient(),
		Scheme:                    mgr.GetScheme(),
		ControllerImage:           cfg.ControllerImage,
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
// certificate named by the configuration.
//
//nolint:ireturn // webhook.NewServer returns the webhook.Server interface.
func newWebhookServer(
	cfg managerConfig,
	setupLog logr.Logger,
) webhook.Server {
	webhookServerOptions := webhook.Options{}

	if len(cfg.WebhookCertPath) > 0 {
		setupLog.Info("Initializing webhook certificate watcher using provided certificates",
			"webhook-cert-path", cfg.WebhookCertPath,
			"webhook-cert-name", cfg.WebhookCertName,
			"webhook-cert-key", cfg.WebhookCertKey)

		webhookServerOptions.CertDir = cfg.WebhookCertPath
		webhookServerOptions.CertName = cfg.WebhookCertName
		webhookServerOptions.KeyName = cfg.WebhookCertKey
	}

	return webhook.NewServer(webhookServerOptions)
}

// newMetricsServerOptions builds the metrics server options, optionally
// using the certificate named by the configuration.
func newMetricsServerOptions(
	cfg managerConfig,
	setupLog logr.Logger,
) metricsserver.Options {
	// Metrics endpoint is enabled in 'config/default/kustomization.yaml'.
	// The Metrics options configure the server. More info:
	// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.24.1/pkg/metrics/server
	// - https://book.kubebuilder.io/reference/metrics.html
	metricsServerOptions := metricsserver.Options{
		BindAddress:   cfg.MetricsBindAddress,
		SecureServing: cfg.SecureMetrics,
	}

	if cfg.SecureMetrics {
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
	if len(cfg.MetricsCertPath) > 0 {
		setupLog.Info("Initializing metrics certificate watcher using provided certificates",
			"metrics-cert-path", cfg.MetricsCertPath, "metrics-cert-name", cfg.MetricsCertName,
			"metrics-cert-key", cfg.MetricsCertKey)

		metricsServerOptions.CertDir = cfg.MetricsCertPath
		metricsServerOptions.CertName = cfg.MetricsCertName
		metricsServerOptions.KeyName = cfg.MetricsCertKey
	}

	return metricsServerOptions
}
