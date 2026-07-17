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

package main

import (
	"crypto/tls"
	"flag"
	"os"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	crystalbackupiov1alpha1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
	"github.com/CrystalBackup/CrystalBackup/internal/client/secrets"
	"github.com/CrystalBackup/CrystalBackup/internal/controller"
	"github.com/CrystalBackup/CrystalBackup/internal/exposer"
	"github.com/CrystalBackup/CrystalBackup/internal/repo/queue"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(crystalbackupiov1alpha1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

// nolint:gocyclo
func main() {
	var metricsAddr string
	var metricsCertPath, metricsCertName, metricsCertKey string
	var webhookCertPath, webhookCertName, webhookCertKey string
	var enableLeaderElection bool
	var probeAddr string
	var secureMetrics bool
	var enableHTTP2 bool
	var tlsOpts []func(*tls.Config)
	var operatorNamespace string
	var moverImage string
	// defaultOperatorNamespace resolves the operator's own namespace before flags are parsed:
	// $POD_NAMESPACE (set via the downward API in the Helm chart / manifest) if present, else
	// the Helm chart's own default, apiconst.DefaultOperatorNamespace. This is where every
	// cluster-plane platform Secret (the KEK, DR S3 credentials, wrapped DEKs) lives.
	defaultOperatorNamespace := os.Getenv("POD_NAMESPACE")
	if defaultOperatorNamespace == "" {
		defaultOperatorNamespace = apiconst.DefaultOperatorNamespace
	}
	flag.StringVar(&operatorNamespace, "operator-namespace", defaultOperatorNamespace,
		"Namespace where cluster-plane platform Secrets (the cluster KEK, DR S3 credentials, wrapped DEKs) live. "+
			"Defaults to $POD_NAMESPACE (downward API) or, if unset, "+apiconst.DefaultOperatorNamespace+".")
	flag.StringVar(&moverImage, "mover-image", "",
		"Container image for the mover Jobs (repository init and, later, backup/restore/maintenance). "+
			"REQUIRED for real backups — the Helm chart and the crucible set it; an empty value is tolerated "+
			"only because envtest never runs a mover Job.")
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&secureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	flag.StringVar(&webhookCertPath, "webhook-cert-path", "", "The directory that contains the webhook certificate.")
	flag.StringVar(&webhookCertName, "webhook-cert-name", "tls.crt", "The name of the webhook certificate file.")
	flag.StringVar(&webhookCertKey, "webhook-cert-key", "tls.key", "The name of the webhook key file.")
	flag.StringVar(&metricsCertPath, "metrics-cert-path", "",
		"The directory that contains the metrics server certificate.")
	flag.StringVar(&metricsCertName, "metrics-cert-name", "tls.crt", "The name of the metrics server certificate file.")
	flag.StringVar(&metricsCertKey, "metrics-cert-key", "tls.key", "The name of the metrics server key file.")
	flag.BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// if the enable-http2 flag is false (the default), http/2 should be disabled
	// due to its vulnerabilities. More specifically, disabling http/2 will
	// prevent from being vulnerable to the HTTP/2 Stream Cancellation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("Disabling HTTP/2")
		c.NextProtos = []string{"http/1.1"}
	}

	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	// Initial webhook TLS options
	webhookTLSOpts := tlsOpts
	webhookServerOptions := webhook.Options{
		TLSOpts: webhookTLSOpts,
	}

	if len(webhookCertPath) > 0 {
		setupLog.Info("Initializing webhook certificate watcher using provided certificates",
			"webhook-cert-path", webhookCertPath, "webhook-cert-name", webhookCertName, "webhook-cert-key", webhookCertKey)

		webhookServerOptions.CertDir = webhookCertPath
		webhookServerOptions.CertName = webhookCertName
		webhookServerOptions.KeyName = webhookCertKey
	}

	webhookServer := webhook.NewServer(webhookServerOptions)

	// Metrics endpoint is enabled in 'config/default/kustomization.yaml'. The Metrics options configure the server.
	// More info:
	// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.24.1/pkg/metrics/server
	// - https://book.kubebuilder.io/reference/metrics.html
	metricsServerOptions := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
		TLSOpts:       tlsOpts,
	}

	if secureMetrics {
		// FilterProvider is used to protect the metrics endpoint with authn/authz.
		// These configurations ensure that only authorized users and service accounts
		// can access the metrics endpoint. The RBAC are configured in 'config/rbac/kustomization.yaml'. More info:
		// https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.24.1/pkg/metrics/filters#WithAuthenticationAndAuthorization
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	// If the certificate is not specified, controller-runtime will automatically
	// generate self-signed certificates for the metrics server. While convenient for development and testing,
	// this setup is not recommended for production.
	//
	// TODO(user): If you enable certManager, uncomment the following lines:
	// - [METRICS-WITH-CERTS] at config/default/kustomization.yaml to generate and use certificates
	// managed by cert-manager for the metrics server.
	// - [PROMETHEUS-WITH-CERTS] at config/prometheus/kustomization.yaml for TLS certification.
	if len(metricsCertPath) > 0 {
		setupLog.Info("Initializing metrics certificate watcher using provided certificates",
			"metrics-cert-path", metricsCertPath, "metrics-cert-name", metricsCertName, "metrics-cert-key", metricsCertKey)

		metricsServerOptions.CertDir = metricsCertPath
		metricsServerOptions.CertName = metricsCertName
		metricsServerOptions.KeyName = metricsCertKey
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		// The operator must never build a cluster-wide Secret cache/informer (tenancy
		// invariant I3): it holds Secrets GET-only, no list/watch. Bypassing the cache for
		// Secrets makes every manager-client Get(Secret) a direct API read, so the
		// controllers and internal/keys.DEKManager can read Secrets through mgr.GetClient()
		// without starting a Secret informer (which needs list/watch RBAC and would cache
		// other namespaces' Secrets).
		Client:                 client.Options{Cache: &client.CacheOptions{DisableFor: []client.Object{&corev1.Secret{}}}},
		Metrics:                metricsServerOptions,
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "082fad01.crystalbackup.io",
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
		setupLog.Error(err, "Failed to start manager")
		os.Exit(1)
	}

	// The process shutdown signal, obtained ONCE (SetupSignalHandler must not be called twice)
	// and shared by the repository queue and the manager: a SIGINT/SIGTERM cancels this context,
	// which shuts the queue's workers down alongside the manager.
	signalCtx := ctrl.SetupSignalHandler()

	// One per-repository exclusive work queue for the whole process (adr/0010): it serialises
	// init/forget/prune/check/erase per repository so a single leader never races itself (the
	// K8up #1055 init race). Bound to signalCtx so shutdown cancels in-flight ops; Stop is
	// deferred to join the worker goroutines on a clean exit.
	repoQueue := queue.NewManager(signalCtx)
	defer repoQueue.Stop()

	if err := (&controller.ClusterBackupLocationReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		// The uncached, GET-by-name reader — never mgr.GetClient() — per
		// internal/client/secrets' package doc (tenancy invariant I3).
		Secrets:           secrets.NewByNameReader(mgr.GetAPIReader()),
		Prober:            controller.NewHTTPS3Prober(),
		OperatorNamespace: operatorNamespace,
		Recorder:          mgr.GetEventRecorderFor("clusterbackuplocation"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Unable to create controller", "controller", "ClusterBackupLocation")
		os.Exit(1)
	}
	if err := controller.NewBackupRepositoryReconciler(
		mgr.GetClient(),
		mgr.GetScheme(),
		// Same uncached Secret reader (the cluster KEK + DR S3 credentials); never GetClient().
		secrets.NewByNameReader(mgr.GetAPIReader()),
		repoQueue,
		operatorNamespace,
		moverImage,
		mgr.GetEventRecorderFor("backuprepository"),
	).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Unable to create controller", "controller", "BackupRepository")
		os.Exit(1)
	}
	if err := controller.NewBackupReconciler(
		mgr.GetClient(),
		mgr.GetScheme(),
		// Same uncached Secret reader (the cluster KEK + DR S3 credentials); never GetClient().
		secrets.NewByNameReader(mgr.GetAPIReader()),
		// The production exposer registry, over the cached client (it reads StorageClasses and
		// VolumeSnapshotClasses, and creates the exposure objects), scoped to the operator
		// namespace where the static re-bind pair and temp clone PVCs land.
		exposer.NewRegistry(mgr.GetClient(), operatorNamespace),
		operatorNamespace,
		moverImage,
		mgr.GetEventRecorderFor("backup"),
	).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Unable to create controller", "controller", "Backup")
		os.Exit(1)
	}

	if err := controller.NewClusterBackupReconciler(
		mgr.GetClient(),
		mgr.GetScheme(),
		operatorNamespace,
		mgr.GetEventRecorderFor("clusterbackup"),
	).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Unable to create controller", "controller", "ClusterBackup")
		os.Exit(1)
	}

	if err := controller.NewClusterBackupScheduleReconciler(
		mgr.GetClient(),
		mgr.GetScheme(),
		clock.RealClock{},
		mgr.GetEventRecorderFor("clusterbackupschedule"),
	).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Unable to create controller", "controller", "ClusterBackupSchedule")
		os.Exit(1)
	}
	// +kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("Starting manager")
	if err := mgr.Start(signalCtx); err != nil {
		setupLog.Error(err, "Failed to run manager")
		os.Exit(1)
	}
}
