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

package controller

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2" //nolint:revive,staticcheck
	. "github.com/onsi/gomega"    //nolint:revive,staticcheck

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	clocktesting "k8s.io/utils/clock/testing"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/client/secrets"
	"github.com/CrystalBackup/CrystalBackup/internal/keys"
	"github.com/CrystalBackup/CrystalBackup/internal/mover"
	"github.com/CrystalBackup/CrystalBackup/internal/repo/queue"
	"github.com/CrystalBackup/CrystalBackup/internal/rexposer"
)

// suiteOperatorNamespace stands in for apiconst.DefaultOperatorNamespace ("crystal-backup-system")
// across every controller test in this package: the one namespace where cluster-plane
// platform Secrets (the KEK, DR S3 credentials, wrapped DEKs) live. It is created once here,
// in BeforeSuite, because it is shared infrastructure every future controller's tests will
// also need — not a per-spec concern.
const suiteOperatorNamespace = "crystal-backup-system"

// TestControllers is the single entry point `go test` (and so `make test`) drives for every
// Ginkgo spec in this package — one envtest API server, started once in BeforeSuite and
// stopped once in AfterSuite, shared by every controller's *_test.go file.
func TestControllers(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Controller Suite")
}

// Package-level so every *_test.go file in this package (present and future) can drive the
// same envtest API server without re-deriving a client or a context: cfg/k8sClient are the
// direct (uncached) envtest wiring specs assert through, and ctx/cancel bound the manager
// goroutine's lifetime to the suite.
var (
	cfg       *rest.Config
	k8sClient client.Client
	testEnv   *envtest.Environment
	ctx       context.Context
	cancel    context.CancelFunc
	// cachedReader is the manager's client — the SHARED INFORMER CACHE every reconciler below
	// Gets and Lists through, as opposed to k8sClient, which talks straight to the apiserver.
	//
	// A spec that seeds a fixture with k8sClient and then immediately provokes a reconcile has
	// created an object the reconciler cannot see yet: the two paths are different connections,
	// and nothing orders the fixture's watch event ahead of the poke's. Asserting on what the
	// controller did with that fixture is then a coin toss — measured at ~10% wrong on this
	// suite (see the Forbid spec in backupschedule_controller_test.go). Waiting on this reader
	// is how a spec states "the controller can now see it" instead of assuming it.
	cachedReader client.Reader
	// repoQueue is the process-wide per-repository exclusive work queue the BackupRepository
	// reconciler drives init through. It is created in BeforeSuite (bound to the suite ctx) and
	// Stop()ped in AfterSuite so its worker goroutines are joined — a leaked worker would keep
	// the suite process alive and mask a shutdown bug.
	repoQueue *queue.Manager
	// scheduleClock is the fake clock the ClusterBackupSchedule reconciler reads "now" from, so the
	// schedule specs drive cron activations deterministically. A BeforeEach in the schedule Describe
	// resets it to real-time-ish before each spec (it is shared process-wide).
	scheduleClock *clocktesting.FakeClock
	// maintenanceClock is the fake clock the MaintenanceReconciler reads "now" from, so a spec can
	// make a daily prune window fire without waiting a day. Separate from scheduleClock: the two
	// controllers are advanced independently, and sharing one would couple unrelated specs.
	maintenanceClock *clocktesting.FakeClock
	// hookExecutor is the stub the Backup reconciler execs consistency hooks through. envtest has
	// no kubelet, so a real pods/exec is impossible; the stub records every call and lets a spec
	// make a specific pod's hook fail or hang.
	hookExecutor *stubHookExecutor
	// backupExposers is the stub exposer registry the Backup reconciler is wired with, hoisted
	// to a suite var so the teardown-sweep specs can read its recorded TeardownExposure calls
	// and arm per-call failures.
	backupExposers *stubExposerRegistry
	// backupStatusFailer arms the AMBIGUOUS status write for one Backup: the terminal status
	// Update is performed for real, then reported to the reconciler as a transport error —
	// exercising terminalPhaseCommitted's committed-despite-error path deterministically.
	backupStatusFailer *statusUpdateFailer
	// discoveryLister is the stub inventory the DiscoveryReconciler reads; the discovery specs feed
	// it canned snapshots (mutex-guarded, since the manager reconciles on another goroutine).
	discoveryLister *stubSnapshotLister
	// restoreLister is the stub MEDIATED lister both restore reconcilers resolve through. It
	// applies the filter tags itself (AND semantics, like restic) so a spec can prove the
	// controller only ever sees what the server-side filter returns, and it records every
	// call's filter tags so the derivation is assertable.
	restoreLister *stubFilteredLister
)

// suiteMoverImage is the placeholder mover image the envtest BackupRepository reconciler builds
// its init Jobs with. envtest has no kubelet, so the image is never pulled or run — the tests
// SIMULATE the Job's outcome by patching its status.
const suiteMoverImage = "crystal-mover:test"

// suiteSyncImage is the placeholder external-sync image (crystal-mover + restic + rclone). It is
// DIFFERENT from suiteMoverImage so a spec that asserts a sync Job runs the sync image is actually
// testing something; identical values would make that assertion pass by accident.
const suiteSyncImage = "crystal-sync:test"

// suiteMoverProfilesYAML is the sizing override the whole suite runs with — the exact shape the
// chart renders into its ConfigMap. Every value here is DELIBERATELY UNLIKE the built-in table
// (odd CPU millicores, a cache cap nothing else uses): a spec asserting a mover Job's requests
// would pass on the built-in defaults if the override were merely plausible, which is how a knob
// ships documented and inert.
const suiteMoverProfilesYAML = `
default:
  requests:
    cpu: 33m
init:
  requests:
    cpu: 77m
    memory: 111Mi
  limits:
    memory: 999Mi
  cacheSizeLimit: 7Gi
`

// suiteMoverProfiles is the parsed form, resolved once in BeforeSuite.
var suiteMoverProfiles mover.Profiles

// The manifest mover identity and grant, as the chart would resolve them. envtest has no
// kubelet so no Job ever runs, but the RoleBinding IS really created against the API server,
// which is what exercises the transient-grant path.
const (
	suiteManifestMoverSA           = "crystal-backup-manifest-mover"
	suiteManifestWriterRole        = "crystal-backup-manifest-writer"
	suiteManifestReaderRole        = "crystal-backup-manifest-reader"
	suiteClusterManifestReaderRole = "crystal-backup-cluster-manifest-reader"
	suiteClusterManifestWriterRole = "crystal-backup-cluster-manifest-writer"
)

var _ = BeforeSuite(func() {
	logf.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true)))

	ctx, cancel = context.WithCancel(context.Background())

	By("starting the envtest control plane")
	testEnv = &envtest.Environment{
		// Mirrors test/crd/roundtrip_test.go's relative path, two levels up from
		// internal/controller/ to the repo root, then into config/crd/bases.
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}

	var err error
	cfg, err = testEnv.Start()
	Expect(err).NotTo(HaveOccurred())
	Expect(cfg).NotTo(BeNil())

	scheme := runtime.NewScheme()
	Expect(clientgoscheme.AddToScheme(scheme)).To(Succeed())
	Expect(cbv1.AddToScheme(scheme)).To(Succeed())

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme})
	Expect(err).NotTo(HaveOccurred())
	Expect(k8sClient).NotTo(BeNil())

	By("creating the operator namespace")
	Expect(k8sClient.Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: suiteOperatorNamespace},
	})).To(Succeed())

	By("starting the manager")
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme,
		// Mirror production (cmd/main.go): never cache Secrets (tenancy invariant I3), so the
		// tests exercise the same uncached Secret reads as the real operator.
		Client: client.Options{Cache: &client.CacheOptions{DisableFor: []client.Object{&corev1.Secret{}}}},
		// Metrics and health/readiness endpoints are pure overhead in envtest — no scraper,
		// no probe, ever reads them here — so both are switched off.
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
	})
	Expect(err).NotTo(HaveOccurred())
	cachedReader = mgr.GetClient()

	Expect((&ClusterBackupLocationReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		// The uncached API reader, per internal/client/secrets' package doc — GetClient()
		// here would silently stand up a cluster-wide Secret informer.
		Secrets: secrets.NewByNameReader(mgr.GetAPIReader()),
		// A stub: envtest has no real S3 to probe. See its doc for how a spec can still
		// exercise the Reachable=False path deterministically.
		Prober:            stubS3Prober{},
		OperatorNamespace: suiteOperatorNamespace,
		Recorder:          mgr.GetEventRecorder("clusterbackuplocation"),
	}).SetupWithManager(mgr)).To(Succeed())

	// The namespace plane's location controller (M5). Same stub prober; no KEK, no escrow — a
	// tenant repository is protected by the tenant's own key, which UserKeyManager resolves or
	// generates in their namespace.
	Expect((&BackupLocationReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Prober:   stubS3Prober{},
		UserKeys: keys.NewUserKeyManager(mgr.GetClient()),
		Recorder: mgr.GetEventRecorder("backuplocation"),
	}).SetupWithManager(mgr)).To(Succeed())

	// The mover sizing table every reconciler below is wired with, parsed from the chart-shaped
	// override above exactly as main.go parses the mounted ConfigMap.
	var profilesErr error
	suiteMoverProfiles, profilesErr = mover.LoadProfiles([]byte(suiteMoverProfilesYAML))
	Expect(profilesErr).NotTo(HaveOccurred())

	// The per-repository exclusive queue, bound to the suite ctx (cancel() also stops it) and
	// explicitly Stop()ped in AfterSuite.
	repoQueue = queue.NewManager(ctx)
	Expect(NewBackupRepositoryReconciler(
		mgr.GetClient(),
		mgr.GetScheme(),
		secrets.NewByNameReader(mgr.GetAPIReader()),
		repoQueue,
		suiteOperatorNamespace,
		suiteMoverImage,
		suiteMoverProfiles,
		mgr.GetEventRecorder("backuprepository"),
	).SetupWithManager(mgr)).To(Succeed())

	// The maintenance reconciler (M4), on the SAME exclusive queue so prune/check serialise against
	// init/forget/unlock exactly as in production. It reads "now" from its own fake clock: the specs
	// advance it to make a daily cron fire without waiting a day. It stays inert unless a spec sets
	// maintenance on a location — no schedule, nothing due, nothing submitted.
	maintenanceClock = clocktesting.NewFakeClock(time.Now())
	Expect(NewMaintenanceReconciler(
		mgr.GetClient(),
		mgr.GetScheme(),
		secrets.NewByNameReader(mgr.GetAPIReader()),
		repoQueue,
		suiteOperatorNamespace,
		suiteMoverImage,
		suiteMoverProfiles,
		mgr.GetEventRecorder("maintenance"),
		maintenanceClock,
	).SetupWithManager(mgr)).To(Succeed())

	// The Backup reconciler under test. Its exposer seam is a STUB (stubExposerRegistry, defined
	// in backup_controller_test.go) so the suite needs neither the external snapshot CRDs nor a
	// CSI driver: the stub creates a real temp clone PVC in the operator namespace (so the mover
	// Job has something to mount) and reports Ready immediately. envtest has no kubelet, so specs
	// SIMULATE each mover Job's outcome exactly as the BackupRepository specs do.
	hookExecutor = &stubHookExecutor{}
	backupExposers = &stubExposerRegistry{client: mgr.GetClient(), operatorNamespace: suiteOperatorNamespace}
	backupStatusFailer = &statusUpdateFailer{}
	backupReconciler := NewBackupReconciler(
		// The manager client, with ONE seam added: statusFailingClient lets a spec make a single
		// Backup status Update commit server-side yet error client-side (the ambiguous write).
		// Disarmed — the default — it is a pure passthrough.
		&statusFailingClient{Client: mgr.GetClient(), failer: backupStatusFailer},
		mgr.GetScheme(),
		secrets.NewByNameReader(mgr.GetAPIReader()),
		backupExposers,
		suiteOperatorNamespace,
		suiteMoverImage,
		suiteMoverProfiles,
		suiteManifestMoverSA,
		suiteManifestReaderRole,
		mgr.GetEventRecorder("backup"),
		// The same shared exclusive queue as the BackupRepository controller. In envtest no backup
		// sets a retention policy and no mover is simulated as hard-killed, so the forget/unlock
		// triggers stay inert here; the real ops are crucible-validated.
		repoQueue,
	)
	// envtest has no kubelet, so pods/exec cannot work: the hook specs drive a stub that records
	// what would have been exec'd and replays canned outcomes.
	backupReconciler.Hooks = hookExecutor
	// The uncached reader behind the writeStatus ambiguity check, as production wires it.
	backupReconciler.APIReader = mgr.GetAPIReader()
	Expect(backupReconciler.SetupWithManager(mgr)).To(Succeed())

	// The ClusterBackup fan-out reconciler. It creates child Backups (which the registered Backup
	// reconciler above then drives via the stub exposer), so a ClusterBackup spec exercises the
	// whole cascade end-to-end in envtest.
	Expect(NewClusterBackupReconciler(
		mgr.GetClient(),
		mgr.GetScheme(),
		suiteOperatorNamespace,
		secrets.NewByNameReader(mgr.GetAPIReader()),
		suiteMoverImage,
		suiteMoverProfiles,
		suiteManifestMoverSA,
		suiteClusterManifestReaderRole,
		mgr.GetEventRecorder("clusterbackup"),
	).SetupWithManager(mgr)).To(Succeed())

	// The ClusterBackupSchedule reconciler, reading "now" from a fake clock the schedule specs
	// advance to drive cron activations deterministically (envtest requeues run on real time, so
	// the specs poke the schedule to re-reconcile after moving the clock).
	scheduleClock = clocktesting.NewFakeClock(time.Now())
	Expect(NewClusterBackupScheduleReconciler(
		mgr.GetClient(),
		mgr.GetScheme(),
		scheduleClock,
		mgr.GetEventRecorder("clusterbackupschedule"),
	).SetupWithManager(mgr)).To(Succeed())

	// The namespace plane's cron (M5), on the SAME fake clock so a spec advancing time drives
	// both planes' activations from one place.
	Expect(NewBackupScheduleReconciler(
		mgr.GetClient(),
		mgr.GetScheme(),
		scheduleClock,
		mgr.GetEventRecorder("backupschedule"),
	).SetupWithManager(mgr)).To(Succeed())

	// The discovery reconciler, reading the repository inventory from a stub lister the specs feed
	// canned snapshots to (production runs a restic Job — internal/controller's jobSnapshotLister,
	// wired with the mover image in M1 task #24 — which envtest cannot exercise).
	discoveryLister = &stubSnapshotLister{}
	Expect(NewDiscoveryReconciler(
		mgr.GetClient(),
		mgr.GetScheme(),
		discoveryLister,
		mgr.GetEventRecorder("discovery"),
	).SetupWithManager(mgr)).To(Succeed())

	// The restore pair (M2). Both share the REAL target exposer — rexposer only touches core
	// types (PVC/PV/StorageClass/VolumeAttachment), which envtest serves natively; the specs
	// play the missing kube controllers (binder, PV lifecycle) by patching status, exactly as
	// the rexposer unit tests do against the fake client. The mediated lister is the stub.
	restoreLister = &stubFilteredLister{}

	// The right-to-erasure reconciler (M5, R21), on the SAME exclusive queue so its forget+prune
	// serialises against init/backup exactly as in production. It counts its scope through the
	// stub filtered lister the restore specs already feed.
	Expect(NewClusterErasureReconciler(
		mgr.GetClient(),
		mgr.GetScheme(),
		secrets.NewByNameReader(mgr.GetAPIReader()),
		repoQueue,
		restoreLister,
		suiteOperatorNamespace,
		suiteMoverImage,
		suiteMoverProfiles,
		mgr.GetEventRecorder("clustererasure"),
	).SetupWithManager(mgr)).To(Succeed())

	// The two external-sync reconcilers (M5, R28). They share the exclusive queue (their Mirror
	// forget half runs on it) and read the same stub filtered lister, whose seeded snapshots the
	// specs use to drive the copied/lag accounting. suiteSyncImage is distinct from the mover
	// image on purpose: a spec asserting the Job's image would pass either way if they were equal.
	Expect(NewClusterBackupExternalSyncReconciler(
		mgr.GetClient(),
		mgr.GetScheme(),
		secrets.NewByNameReader(mgr.GetAPIReader()),
		repoQueue,
		restoreLister,
		suiteOperatorNamespace,
		suiteMoverImage,
		suiteSyncImage,
		suiteMoverProfiles,
		scheduleClock,
		mgr.GetEventRecorder("clusterbackupexternalsync"),
	).SetupWithManager(mgr)).To(Succeed())
	Expect(NewBackupExternalSyncReconciler(
		mgr.GetClient(),
		mgr.GetScheme(),
		secrets.NewByNameReader(mgr.GetAPIReader()),
		keys.NewUserKeyManager(mgr.GetClient()),
		repoQueue,
		restoreLister,
		suiteOperatorNamespace,
		suiteMoverImage,
		suiteSyncImage,
		suiteMoverProfiles,
		scheduleClock,
		mgr.GetEventRecorder("backupexternalsync"),
	).SetupWithManager(mgr)).To(Succeed())

	restoreTargets := rexposer.NewTargetExposer(mgr.GetClient(), suiteOperatorNamespace)
	Expect(NewRestoreReconciler(
		mgr.GetClient(),
		mgr.GetScheme(),
		secrets.NewByNameReader(mgr.GetAPIReader()),
		restoreTargets,
		restoreLister,
		suiteOperatorNamespace,
		suiteMoverImage,
		suiteMoverProfiles,
		suiteManifestMoverSA,
		suiteManifestWriterRole,
		mgr.GetEventRecorder("restore"),
		repoQueue,
	).SetupWithManager(mgr)).To(Succeed())
	Expect(NewClusterRestoreReconciler(
		mgr.GetClient(),
		mgr.GetScheme(),
		secrets.NewByNameReader(mgr.GetAPIReader()),
		restoreTargets,
		restoreLister,
		suiteOperatorNamespace,
		suiteMoverImage,
		suiteMoverProfiles,
		suiteManifestMoverSA,
		suiteClusterManifestWriterRole,
		mgr.GetEventRecorder("clusterrestore"),
		repoQueue,
	).SetupWithManager(mgr)).To(Succeed())

	go func() {
		defer GinkgoRecover()
		Expect(mgr.Start(ctx)).To(Succeed())
	}()
})

var _ = AfterSuite(func() {
	By("stopping the repository queue and joining its workers")
	// Stop() cancels every in-flight init op and joins the worker goroutines before returning,
	// so no queue goroutine outlives the suite.
	if repoQueue != nil {
		repoQueue.Stop()
	}
	cancel()
	By("tearing down the envtest control plane")
	Expect(testEnv.Stop()).To(Succeed())
})

// unreachableTestEndpoint is a magic Spec.S3.Endpoint value stubS3Prober treats as
// unreachable. Keying the stub's answer off the endpoint value (rather than off a shared
// mutable flag) means a spec can deterministically exercise Reachable=False for ONE location
// without affecting any other spec's locations running concurrently in the same suite.
const unreachableTestEndpoint = "https://unreachable.invalid.test"

// stubS3Prober is the envtest S3Prober: reachable for every endpoint except
// unreachableTestEndpoint, so most specs get an unconditional Reachable=True while a spec
// that specifically wants Reachable=False can opt in by name.
type stubS3Prober struct{}

func (stubS3Prober) Reachable(_ context.Context, s3 cbv1.S3Spec) error {
	if s3.Endpoint == unreachableTestEndpoint {
		return fmt.Errorf("stub: %q is marked unreachable for this test", s3.Endpoint)
	}
	return nil
}
