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

// The terminal re-entry sweep pins (the leak audit's envtest plan): a terminal Backup must never
// go quiet with its exposure teardown unverified. These specs pin the crash windows
// DETERMINISTICALLY — what the crucible only reproduced one run in three:
//
//   - the sweep re-runs teardown and stamps AnnotationExposuresCleaned only on success;
//   - a failing teardown withholds the marker (and finalize its finalizer) until it succeeds;
//   - a status write that COMMITS server-side while erroring client-side (the confirmed
//     "ambiguous write" seam) still ends in a torn-down, marker-stamped Backup.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2" //nolint:revive,staticcheck
	. "github.com/onsi/gomega"    //nolint:revive,staticcheck

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
	"github.com/CrystalBackup/CrystalBackup/internal/mover"
)

// consistentlyWindow is how long a "must NOT happen" assertion watches. Short on purpose: the
// negative it guards (a marker stamped despite a failing teardown) would land within one or two
// backoff retries — milliseconds to a second — if the bug existed.
const consistentlyWindow = 2 * time.Second

// ---------------------------------------------------------------------------
// The ambiguous-status-write seam. statusFailingClient wraps the manager
// client the Backup reconciler runs on; disarmed it is a pure passthrough.
// Armed for one Backup, the next TERMINAL status Update is performed for real
// and THEN reported as a transport error — exactly the seam the audit
// confirmed (SIGTERM cancelling an in-flight Update the apiserver commits).
// ---------------------------------------------------------------------------

// statusUpdateFailer arms the one-shot commit-then-error behaviour for a single Backup.
type statusUpdateFailer struct {
	mu    sync.Mutex
	key   types.NamespacedName // zero = disarmed
	fired int                  // how many times the injected error was returned
}

// arm targets the next terminal status Update of the given Backup.
func (f *statusUpdateFailer) arm(namespace, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.key = types.NamespacedName{Namespace: namespace, Name: name}
}

// timesFired reports how often the injected error was returned to the reconciler.
func (f *statusUpdateFailer) timesFired() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fired
}

// shouldFail consumes the armed key when obj is that Backup reaching a TERMINAL phase — the
// only write worth failing, since the sealed-forever hazard needs a committed terminal phase.
func (f *statusUpdateFailer) shouldFail(obj client.Object) bool {
	b, ok := obj.(*cbv1.Backup)
	if !ok || !isTerminalBackupPhase(b.Status.Phase) {
		return false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.key.Namespace != b.Namespace || f.key.Name != b.Name {
		return false
	}
	f.key = types.NamespacedName{}
	f.fired++
	return true
}

// statusFailingClient overrides ONLY Status() on the embedded manager client.
type statusFailingClient struct {
	client.Client
	failer *statusUpdateFailer
}

func (c *statusFailingClient) Status() client.SubResourceWriter {
	return &statusFailingWriter{inner: c.Client.Status(), failer: c.failer}
}

type statusFailingWriter struct {
	inner  client.SubResourceWriter
	failer *statusUpdateFailer
}

func (w *statusFailingWriter) Create(ctx context.Context, obj, subResource client.Object, opts ...client.SubResourceCreateOption) error {
	return w.inner.Create(ctx, obj, subResource, opts...)
}

func (w *statusFailingWriter) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
	return w.inner.Patch(ctx, obj, patch, opts...)
}

func (w *statusFailingWriter) Apply(ctx context.Context, obj runtime.ApplyConfiguration, opts ...client.SubResourceApplyOption) error {
	return w.inner.Apply(ctx, obj, opts...)
}

// Update performs the REAL update first: when the failer is armed for this object, the write is
// committed server-side and the returned error is a lie the reconciler must see through.
func (w *statusFailingWriter) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	if err := w.inner.Update(ctx, obj, opts...); err != nil {
		return err
	}
	if w.failer.shouldFail(obj) {
		return errors.New("injected: connection reset mid-flight (the update DID commit server-side)")
	}
	return nil
}

// ---------------------------------------------------------------------------
// Specs.
// ---------------------------------------------------------------------------

// driveBackupToCompleted seeds a full happy-path run (repo, tenant ns, source PVC, volume-only
// parent, child Backup) and simulates the mover succeeding, returning once the mover Job exists.
func driveBackupToCompleted(location, ns, run, pvcName string) {
	GinkgoHelper()
	seedInitializedRepo(location, "kek-"+location, "s3-"+location)
	createTenantNamespace(ns)
	createSourcePVC(ns, pvcName, "ceph-block")
	createVolumeOnlyParent(run, location, cbv1.PVCSelector{})
	createChildBackup(ns, run, location)
	jobName := waitForMoverJob(ns, run, pvcName)
	simulateMoverSucceeded(jobName, "node-a", mover.MoverResult{
		OK: true, Operation: string(mover.OpBackup), SnapshotID: "snap-" + run, SizeBytes: 4096, AddedBytes: 512,
	})
}

var _ = Describe("Terminal re-entry teardown sweep", func() {

	AfterEach(func() {
		// Never let one spec's armed failure leak into the next.
		backupExposers.setFailTeardown(nil)
	})

	It("stamps exposures-cleaned on a terminal Backup only after re-running the teardown", func() {
		const (
			location = "sweep-ok"
			ns       = "sweep-ok-ns"
			run      = "sweep-ok-run"
			pvcName  = "data-vol"
		)
		driveBackupToCompleted(location, ns, run, pvcName)

		By("the Backup goes terminal and the sweep verifies + stamps the marker")
		Eventually(func(g Gomega) {
			b := getBackupG(g, ns, run)
			g.Expect(b.Status.Phase).To(Equal("Completed"))
			g.Expect(b.Annotations).To(HaveKeyWithValue(
				apiconst.AnnotationExposuresCleaned, apiconst.AnnotationExposuresCleanedValue))
		}, initTimeout, initPoll).Should(Succeed())

		By("the sweep really swept this volume's exposure (derive-only teardown was invoked)")
		Expect(backupExposers.teardownCalls()).To(ContainElement(ns + "/" + moverNamePrefix(ns, run, pvcName)))

		By("the temp clone PVC is gone — teardown completed, not just marked")
		var clone corev1.PersistentVolumeClaim
		err := k8sClient.Get(ctx,
			client.ObjectKey{Namespace: suiteOperatorNamespace, Name: tempCloneNameFor(ns, run, pvcName)}, &clone)
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "temp clone PVC should be deleted")
	})

	It("withholds the marker while teardown fails, then heals by re-entry once it succeeds", func() {
		const (
			location = "sweep-fail"
			ns       = "sweep-fail-ns"
			run      = "sweep-fail-run"
			pvcName  = "data-vol"
		)
		prefix := moverNamePrefix(ns, run, pvcName)
		backupExposers.setFailTeardown(func(_, namePrefix string) error {
			if namePrefix == prefix {
				return fmt.Errorf("injected: exposure deletes failing for %s", namePrefix)
			}
			return nil
		})

		driveBackupToCompleted(location, ns, run, pvcName)

		By("the Backup is terminal but the marker is WITHHELD while the teardown cannot complete")
		Eventually(func(g Gomega) {
			g.Expect(getBackupG(g, ns, run).Status.Phase).To(Equal("Completed"))
		}, initTimeout, initPoll).Should(Succeed())
		Consistently(func(g Gomega) {
			g.Expect(getBackupG(g, ns, run).Annotations).NotTo(HaveKey(apiconst.AnnotationExposuresCleaned))
		}, consistentlyWindow, initPoll).Should(Succeed())

		By("once the teardown can succeed, the backoff-requeued sweep converges and stamps the marker")
		backupExposers.setFailTeardown(nil)
		Eventually(func(g Gomega) {
			b := getBackupG(g, ns, run)
			g.Expect(b.Annotations).To(HaveKeyWithValue(
				apiconst.AnnotationExposuresCleaned, apiconst.AnnotationExposuresCleanedValue))
			g.Expect(b.Status.Phase).To(Equal("Completed"), "healing must never disturb the terminal record")
		}, initTimeout, initPoll).Should(Succeed())
	})

	It("survives the ambiguous write: a terminal status Update that commits server-side but errors client-side still ends torn down", func() {
		const (
			location = "sweep-ambig"
			ns       = "sweep-ambig-ns"
			run      = "sweep-ambig-run"
			pvcName  = "data-vol"
		)
		backupStatusFailer.arm(ns, run)

		driveBackupToCompleted(location, ns, run, pvcName)

		By("the injected commit-then-error fired against the terminal write")
		Eventually(backupStatusFailer.timesFired, initTimeout, initPoll).Should(BeNumerically(">=", 1))

		By("despite the lying error, the Backup converges: terminal phase preserved, teardown done, marker stamped")
		Eventually(func(g Gomega) {
			b := getBackupG(g, ns, run)
			g.Expect(b.Status.Phase).To(Equal("Completed"))
			g.Expect(b.Annotations).To(HaveKeyWithValue(
				apiconst.AnnotationExposuresCleaned, apiconst.AnnotationExposuresCleanedValue))
		}, initTimeout, initPoll).Should(Succeed())
		Expect(backupExposers.teardownCalls()).To(ContainElement(ns + "/" + moverNamePrefix(ns, run, pvcName)))
	})

	It("holds the finalizer on delete until the exposure teardown succeeds", func() {
		const (
			location = "sweep-fin"
			ns       = "sweep-fin-ns"
			run      = "sweep-fin-run"
			pvcName  = "data-vol"
		)
		driveBackupToCompleted(location, ns, run, pvcName)
		Eventually(func(g Gomega) {
			g.Expect(getBackupG(g, ns, run).Status.Phase).To(Equal("Completed"))
		}, initTimeout, initPoll).Should(Succeed())

		By("arming a teardown failure, then deleting the Backup")
		prefix := moverNamePrefix(ns, run, pvcName)
		backupExposers.setFailTeardown(func(_, namePrefix string) error {
			if namePrefix == prefix {
				return fmt.Errorf("injected: exposure deletes failing for %s", namePrefix)
			}
			return nil
		})
		var b cbv1.Backup
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: run}, &b)).To(Succeed())
		Expect(k8sClient.Delete(ctx, &b)).To(Succeed())

		By("the finalizer HOLDS: unswept residue must not be orphaned by the CR vanishing")
		Consistently(func(g Gomega) {
			var got cbv1.Backup
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: run}, &got)).To(Succeed())
			g.Expect(got.DeletionTimestamp.IsZero()).To(BeFalse())
		}, consistentlyWindow, initPoll).Should(Succeed())

		By("once teardown succeeds, finalize completes and the Backup is released")
		backupExposers.setFailTeardown(nil)
		Eventually(func(g Gomega) {
			err := k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: run}, &cbv1.Backup{})
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
		}, initTimeout, initPoll).Should(Succeed())
	})
})
