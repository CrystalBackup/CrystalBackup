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

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	crystalbackupiov1alpha1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
	"github.com/CrystalBackup/CrystalBackup/internal/client/secrets"
	"github.com/CrystalBackup/CrystalBackup/internal/controller"
	"github.com/CrystalBackup/CrystalBackup/internal/exposer"
	"github.com/CrystalBackup/CrystalBackup/internal/metrics"
	"github.com/CrystalBackup/CrystalBackup/internal/repo/queue"
	"github.com/CrystalBackup/CrystalBackup/internal/rexposer"
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

	// The REST config is captured (not passed inline to NewManager) so the discovery lister can build
	// a client-go clientset from it: reading a pod's log is a subresource STREAM the controller-runtime
	// client does not support, so that one path needs a raw clientset alongside the manager's client.
	restConfig := ctrl.GetConfigOrDie()

	mgr, err := ctrl.NewManager(restConfig, ctrl.Options{
		Scheme: scheme,
		// The mover / repository-init / maintenance Jobs the operator creates all live in ITS
		// OWN namespace (invariant I5, spec/03 §5): every Job Get/List/Create/Delete in the
		// controllers is scoped to operatorNamespace, and the Helm chart grants batch/jobs
		// through a namespaced Role, NOT the ClusterRole. Scope the Job informer to that one
		// namespace so the manager cache asks only for a namespaced Job list/watch (which the
		// Role allows) instead of the cluster-wide list/watch the default cache would demand —
		// that exact mismatch CrashLoops the operator against the least-privilege chart RBAC.
		// Every other watched kind keeps the default cluster-wide cache; their ClusterRole
		// grants already cover it.
		Cache: cache.Options{
			ByObject: map[client.Object]cache.ByObject{
				&batchv1.Job{}: {
					Namespaces: map[string]cache.Config{operatorNamespace: {}},
				},
			},
		},
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
		Recorder:          mgr.GetEventRecorder("clusterbackuplocation"),
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
		mgr.GetEventRecorder("backuprepository"),
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
		mgr.GetEventRecorder("backup"),
		// The SAME per-repository exclusive queue the BackupRepository controller uses: the Backup
		// controller enqueues retention-forget and stale-lock-unlock on it, serialised per repository
		// against init and each other.
		repoQueue,
	).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Unable to create controller", "controller", "Backup")
		os.Exit(1)
	}

	if err := controller.NewClusterBackupReconciler(
		mgr.GetClient(),
		mgr.GetScheme(),
		operatorNamespace,
		mgr.GetEventRecorder("clusterbackup"),
	).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Unable to create controller", "controller", "ClusterBackup")
		os.Exit(1)
	}

	if err := controller.NewClusterBackupScheduleReconciler(
		mgr.GetClient(),
		mgr.GetScheme(),
		clock.RealClock{},
		mgr.GetEventRecorder("clusterbackupschedule"),
	).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Unable to create controller", "controller", "ClusterBackupSchedule")
		os.Exit(1)
	}

	// The discovery reconciler projects a shared repository's snapshots back into read-only Backup
	// CRs so a DR repository is restorable with no pre-existing objects. Its production lister runs a
	// real `restic snapshots` mover Job and reads the inventory off the pod log via the clientset;
	// the uncached Secret reader (I3) resolves the cluster KEK + DR S3 credentials for that Job.
	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		setupLog.Error(err, "Unable to build the clientset for the discovery pod-log reader")
		os.Exit(1)
	}
	// One production lister serves discovery's full inventory AND the restore controllers'
	// mediated, tag-filtered resolutions (adr/0016 §3) — same Job mechanics, distinct Job names.
	snapshotLister := controller.NewJobSnapshotLister(
		mgr.GetClient(),
		clientset,
		secrets.NewByNameReader(mgr.GetAPIReader()),
		mgr.GetScheme(),
		operatorNamespace,
		moverImage,
	)
	if err := controller.NewDiscoveryReconciler(
		mgr.GetClient(),
		mgr.GetScheme(),
		snapshotLister,
		mgr.GetEventRecorder("discovery"),
	).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Unable to create controller", "controller", "Discovery")
		os.Exit(1)
	}

	// The restore pair (M2, adr/0016): the namespaced self-service Restore and the admin
	// ClusterRestore share the target exposer (pvc-transplant / pv-twin) and the mediated
	// lister; each holds its own engine over the same primitives, including the shared
	// exclusive queue (a crashed restore mover enqueues the same stale-lock unlock a backup
	// mover does).
	targetExposer := rexposer.NewTargetExposer(mgr.GetClient(), operatorNamespace)
	if err := controller.NewRestoreReconciler(
		mgr.GetClient(),
		mgr.GetScheme(),
		secrets.NewByNameReader(mgr.GetAPIReader()),
		targetExposer,
		snapshotLister,
		operatorNamespace,
		moverImage,
		mgr.GetEventRecorder("restore"),
		repoQueue,
	).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Unable to create controller", "controller", "Restore")
		os.Exit(1)
	}
	if err := controller.NewClusterRestoreReconciler(
		mgr.GetClient(),
		mgr.GetScheme(),
		secrets.NewByNameReader(mgr.GetAPIReader()),
		targetExposer,
		snapshotLister,
		operatorNamespace,
		moverImage,
		mgr.GetEventRecorder("clusterrestore"),
		repoQueue,
	).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Unable to create controller", "controller", "ClusterRestore")
		os.Exit(1)
	}

	// The orphan reaper is a periodic Runnable (not a reconciler): it sweeps the operator namespace
	// for leftover native per-PVC exposure objects (temp clone PVCs, mover Jobs, creds Secrets) a
	// crashed teardown left behind, backstopping the leak-check invariant.
	if err := mgr.Add(&controller.OrphanReaper{
		Client:            mgr.GetClient(),
		OperatorNamespace: operatorNamespace,
	}); err != nil {
		setupLog.Error(err, "Unable to add the orphan reaper")
		os.Exit(1)
	}

	// The crystalbackup_ metric collector derives its series from live Backup/ClusterBackup state on
	// each scrape (restart-safe), served on the controller-runtime metrics endpoint.
	if err := ctrlmetrics.Registry.Register(metrics.NewCollector(mgr.GetClient())); err != nil {
		setupLog.Error(err, "Unable to register the crystalbackup metrics collector")
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
