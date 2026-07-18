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
	"github.com/CrystalBackup/CrystalBackup/internal/repo/queue"
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
	// repoQueue is the process-wide per-repository exclusive work queue the BackupRepository
	// reconciler drives init through. It is created in BeforeSuite (bound to the suite ctx) and
	// Stop()ped in AfterSuite so its worker goroutines are joined — a leaked worker would keep
	// the suite process alive and mask a shutdown bug.
	repoQueue *queue.Manager
	// scheduleClock is the fake clock the ClusterBackupSchedule reconciler reads "now" from, so the
	// schedule specs drive cron activations deterministically. A BeforeEach in the schedule Describe
	// resets it to real-time-ish before each spec (it is shared process-wide).
	scheduleClock *clocktesting.FakeClock
	// discoveryLister is the stub inventory the DiscoveryReconciler reads; the discovery specs feed
	// it canned snapshots (mutex-guarded, since the manager reconciles on another goroutine).
	discoveryLister *stubSnapshotLister
)

// suiteMoverImage is the placeholder mover image the envtest BackupRepository reconciler builds
// its init Jobs with. envtest has no kubelet, so the image is never pulled or run — the tests
// SIMULATE the Job's outcome by patching its status.
const suiteMoverImage = "crystal-mover:test"

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
		mgr.GetEventRecorder("backuprepository"),
	).SetupWithManager(mgr)).To(Succeed())

	// The Backup reconciler under test. Its exposer seam is a STUB (stubExposerRegistry, defined
	// in backup_controller_test.go) so the suite needs neither the external snapshot CRDs nor a
	// CSI driver: the stub creates a real temp clone PVC in the operator namespace (so the mover
	// Job has something to mount) and reports Ready immediately. envtest has no kubelet, so specs
	// SIMULATE each mover Job's outcome exactly as the BackupRepository specs do.
	Expect(NewBackupReconciler(
		mgr.GetClient(),
		mgr.GetScheme(),
		secrets.NewByNameReader(mgr.GetAPIReader()),
		&stubExposerRegistry{client: mgr.GetClient(), operatorNamespace: suiteOperatorNamespace},
		suiteOperatorNamespace,
		suiteMoverImage,
		mgr.GetEventRecorder("backup"),
		// The same shared exclusive queue as the BackupRepository controller. In envtest no backup
		// sets a retention policy and no mover is simulated as hard-killed, so the forget/unlock
		// triggers stay inert here; the real ops are crucible-validated.
		repoQueue,
	).SetupWithManager(mgr)).To(Succeed())

	// The ClusterBackup fan-out reconciler. It creates child Backups (which the registered Backup
	// reconciler above then drives via the stub exposer), so a ClusterBackup spec exercises the
	// whole cascade end-to-end in envtest.
	Expect(NewClusterBackupReconciler(
		mgr.GetClient(),
		mgr.GetScheme(),
		suiteOperatorNamespace,
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
