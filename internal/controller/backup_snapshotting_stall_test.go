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
	"errors"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2" //nolint:revive,staticcheck
	. "github.com/onsi/gomega"    //nolint:revive,staticcheck

	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/exposer"
	"github.com/CrystalBackup/CrystalBackup/internal/hooks"
	"github.com/CrystalBackup/CrystalBackup/internal/mover"
	"github.com/CrystalBackup/CrystalBackup/internal/status"
)

// ---------------------------------------------------------------------------
// THE SAME HEAD-OF-LINE BLOCK, ONE AND TWO PHASES LATER.
//
// backup_pending_test.go pins the Pending half of this release: four failure paths that used to
// return an error from advancePending (which no longer has an error result at all — its signature is
// now the invariant), and the three mechanisms (park, prefer un-tried, bound with
// pendingResolveDeadline) that close it. This file pins the half that SURVIVED that fix, and the way
// it was found is worth recording: mutation-testing the Pending fix — putting the bare `return
// fmt.Errorf(...)` back on the Expose path — did not merely lose a timestamp. The assertion that went
// red was that `status.volumes` was NIL. Reconcile returns a volume error at step (10), which is
// UPSTREAM of writeStatus at step (11), so an errored pass persists nothing at all: not the phase,
// not the cause, not the enumeration.
//
// Which made the question obvious: what else returns an error from step (10)? advanceSnapshotting
// did, from reconstructExposure, from ex.Ready and from mover-Job creation; advanceUploading did,
// from the mover-Job read-back. Every one of them reproduced the incident in full through a
// different door — the snapshot CRDs removed under a running operator, RBAC narrowed, a webhook
// refusing Jobs. Worse than in Pending, in one specific way: the error returned BEFORE the deadline
// evaluation inside advanceSnapshotting's not-ready branch, so snapshotProgressDeadline and
// snapshotReadyDeadline — the two bounds that exist to end exactly this — were never consulted on the
// path that needed them. Not absent: UNREACHABLE, which is worse, because the operator reads the
// bound in the documentation and watches the run sail past it.
//
// The fix is structural and lives at the single call site (Reconcile step 10: record the cause,
// continue the pass, persist), plus one narrow reordering: a readiness check that ERRORS is treated
// as "not ready" so the deadline evaluation is reached. The specs below are written so that undoing
// either turns at least one of them red — see the mutation notes on each.
// ---------------------------------------------------------------------------

// stubReadyFailure is the error the readiness-check seam is armed with throughout this file, and the
// choice of error is itself an assertion.
//
// It is a Forbidden on volumesnapshots: the shape of an operator whose RBAC was narrowed (a
// tightened ClusterRole, a policy engine, a chart upgrade that dropped a rule) while a run was in
// flight. That is a DURABLE, CLUSTER-WIDE cause — it fails every volume of every namespace, not one —
// and it is not a verdict about any volume's data. The correct handling is therefore to record it,
// keep the volume where it is, and let the volume's own deadline be the arbiter; anything that
// classified this error kind would be guessing, exactly as the first version of the Pending fix did
// when it turned a NotFound into an immediate Failed.
func stubReadyFailure() error {
	return apierrors.NewForbidden(
		schema.GroupResource{Group: "snapshot.storage.k8s.io", Resource: "volumesnapshots"},
		"crystalbackup-origin",
		errors.New(`User "system:serviceaccount:crystal-backup-system:crystal-backup" cannot get resource "volumesnapshots"`))
}

// ---------------------------------------------------------------------------
// recordAdvanceFailure, at the unit level.
//
// The envtest specs below drive this through the one path where the deadline can then finish the job
// (ex.Ready). The other four sites that reach it — reconstructExposure, the mover-admission gate, the
// per-Job creds Secret, the mover-Job create/read-back — are all "the apiserver said no to a write we
// need", and provoking each of them end to end would mean five wrappers around the client the
// REGISTERED reconciler shares with every other spec in this package (see the note on
// TestAdvancePendingParksAnUnreadableSourcePVC for why that is not available). The behaviour they
// share is this function, so it is pinned directly, where it is also stated most plainly.
// ---------------------------------------------------------------------------

// TestRecordAdvanceFailureKeepsThePhase is the core of the shape: the cause is recorded, and the
// phase — which is what the next pass dispatches on and what the volume's deadline is measured
// against — is left exactly alone.
func TestRecordAdvanceFailureKeepsThePhase(t *testing.T) {
	cause := errors.New("reconstruct exposure for PVC team-x/data: volumesnapshots.snapshot.storage.k8s.io is forbidden")

	for _, phase := range []status.VolumePhase{status.VolumePhaseSnapshotting, status.VolumePhaseUploading} {
		r := &BackupReconciler{Recorder: events.NewFakeRecorder(4)}
		vol := &cbv1.VolumeStatus{Pvc: "data", Phase: phase}

		r.recordAdvanceFailure(context.Background(), vol, cause)

		if vol.Phase != phase {
			t.Errorf("phase = %q, want %q unchanged: demoting the volume would re-drive Expose against a "+
				"live exposure and throw away the origin snapshot's clock; promoting it to Failed would "+
				"terminate a backup on an apiserver blip", vol.Phase, phase)
		}
		if got, want := vol.Reason, backupReasonAdvanceRetrying+": "+cause.Error(); got != want {
			t.Errorf("reason = %q, want %q — a volume that is neither progressing nor explaining itself is "+
				"invisible, which is how thirty hours went by", got, want)
		}
		// And it must NOT be the parking token: parking is defined for Pending, and volumeIsParked
		// would otherwise be asked to reason about a volume holding work in flight.
		if volumeIsParked(vol) {
			t.Errorf("a %s volume was reported as parked", phase)
		}
	}
}

// TestRecordAdvanceFailureParksAPendingVolume pins the delegation that keeps the two mechanisms
// composed. No path reaches it today — advancePending has no error result at all, so the dispatch
// cannot produce one — and that is precisely the point: if a future Pending step ever errors, it must
// land on the PARKING door, because only the parking token defers a volume behind its un-tried
// neighbours. A Pending volume that kept the head of its namespace's queue is the original defect,
// verbatim.
func TestRecordAdvanceFailureParksAPendingVolume(t *testing.T) {
	cause := errors.New("etcdserver: request timed out")

	// Both shapes the dispatch treats as Pending, including the empty phase a volume can be seeded
	// with — a predicate that disagreed with the dispatch would leave exactly one shape able to
	// escape parking.
	for _, phase := range []status.VolumePhase{status.VolumePhasePending, ""} {
		r := &BackupReconciler{Recorder: events.NewFakeRecorder(4)}
		vol := &cbv1.VolumeStatus{Pvc: "data", Phase: phase}

		r.recordAdvanceFailure(context.Background(), vol, cause)

		if vol.Phase != phase {
			t.Errorf("phase = %q, want %q unchanged", vol.Phase, phase)
		}
		if !volumeIsParked(vol) {
			t.Errorf("reason = %q on a %q volume: not parked, so it holds the head of its namespace's queue",
				vol.Reason, phase)
		}
	}
}

// TestClearStaleAdvanceReasonDropsTheObstacleOnlyOnTheMove states the status-hygiene rule that comes
// with having two writers of a reason on a non-terminal volume.
//
// A recorded obstacle ("ExposerUnresolvable: …not found", "AdvanceRetrying: check exposure
// readiness…") is a statement about NOW. When the volume moves, that statement has stopped being
// true, and leaving it behind reports a volume as Uploading while claiming its exposer cannot be
// resolved — to an operator, to `kubectl get backup`, and to the alert rules. Failed and Skipped are
// the exception, because there the reason IS the verdict.
func TestClearStaleAdvanceReasonDropsTheObstacleOnlyOnTheMove(t *testing.T) {
	const obstacle = backupReasonExposerUnresolvable + `: storageclasses.storage.k8s.io "gone" not found`

	cases := []struct {
		name       string
		entryPhase status.VolumePhase
		vol        cbv1.VolumeStatus
		wantReason string
	}{
		{
			// The parked-then-recovered volume: the StorageClass was created a minute after the run
			// started, or the quota was raised. Entirely realistic, and the reason is now a lie.
			name:       "Pending to Snapshotting drops the resolution failure",
			entryPhase: status.VolumePhasePending,
			vol:        cbv1.VolumeStatus{Phase: status.VolumePhaseSnapshotting, Reason: obstacle},
			wantReason: "",
		},
		{
			name:       "Snapshotting to Uploading drops the readiness failure",
			entryPhase: status.VolumePhaseSnapshotting,
			vol: cbv1.VolumeStatus{Phase: status.VolumePhaseUploading,
				Reason: backupReasonAdvanceRetrying + ": check exposure readiness for PVC x/y: forbidden"},
			wantReason: "",
		},
		{
			// The most misleading place of all to leave one.
			name:       "Uploading to Completed drops it too",
			entryPhase: status.VolumePhaseUploading,
			vol:        cbv1.VolumeStatus{Phase: status.VolumePhaseCompleted, Reason: obstacle},
			wantReason: "",
		},
		{
			// The deadline preserves the recorded cause deliberately (it names the fault instead of
			// the clock) and this must never undo that.
			name:       "Pending to Failed KEEPS the reason: it is the verdict",
			entryPhase: status.VolumePhasePending,
			vol:        cbv1.VolumeStatus{Phase: status.VolumePhaseFailed, Reason: obstacle},
			wantReason: obstacle,
		},
		{
			name:       "Pending to Skipped KEEPS the reason: it is the verdict",
			entryPhase: status.VolumePhasePending,
			vol:        cbv1.VolumeStatus{Phase: status.VolumePhaseSkipped, Reason: backupReasonSkippedUnsupported},
			wantReason: backupReasonSkippedUnsupported,
		},
		{
			// No move, no clearing — this is the retry case, and erasing the cause every pass would
			// leave a stuck volume with nothing to read.
			name:       "no phase change keeps the recorded cause",
			entryPhase: status.VolumePhaseSnapshotting,
			vol:        cbv1.VolumeStatus{Phase: status.VolumePhaseSnapshotting, Reason: obstacle},
			wantReason: obstacle,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vol := tc.vol
			clearStaleAdvanceReason(&vol, tc.entryPhase)
			if vol.Reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", vol.Reason, tc.wantReason)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// The same properties through the real controller.
// ---------------------------------------------------------------------------

var _ = Describe("BackupReconciler unanswerable exposure readiness", func() {

	// THE PERSISTENCE SPEC. It says the one thing an errored pass could never say: that what the
	// controller learned about the volume reached etcd.
	//
	// The stall is armed at NOW, well inside snapshotProgressDeadline, so no deadline can fire and the
	// volume is observed in the state the old code left it in permanently and invisibly: Snapshotting,
	// retried every poll, with nothing recorded anywhere.
	//
	// Mutation that must turn this spec red: restoring `return "", fmt.Errorf("check exposure
	// readiness…")` in advanceSnapshotting (the reason never reaches the stored object, because the
	// pass returns before writeStatus). Removing the recordAdvanceFailure call at Reconcile step (10)
	// does NOT catch it — that is what the next spec is for.
	It("records why a volume's readiness cannot be checked, and PERSISTS it", func() {
		const (
			location = "bk-rdyerr"
			ns       = "bk-rdyerr-ns"
			run      = "bk-rdyerr-run"
			pvcName  = "data-vol"
		)
		// Armed BEFORE the Backup exists, so the first Snapshotting pass hits it, and durable.
		backupExposers.failReady(ns+"/"+pvcName, stubReadyFailure())
		// Progress must have an origin snapshot to report on, or the deadline branch has nothing to
		// judge. NOW: inside the bound, so this spec is about the record and not about the verdict.
		backupExposers.stallSnapshot(ns+"/"+pvcName, time.Now())

		seedInitializedRepo(location, "kek-bk-rdyerr", "s3-bk-rdyerr")
		createTenantNamespace(ns)
		createSourcePVC(ns, pvcName, "ceph-block")
		createVolumeOnlyParent(run, location, cbv1.PVCSelector{})
		createChildBackup(ns, run, location)

		By("the exposure IS created — this failure mode is after admission, not a refusal")
		Eventually(func(g Gomega) {
			g.Expect(backupExposers.exposeCalls()).To(ContainElement(ns + "/" + pvcName))
		}, initTimeout, initPoll).Should(Succeed())

		// getBackupG reads through k8sClient, the DIRECT envtest connection with no cache in front of
		// it, which is the only way a spec can say "this reached the apiserver" rather than "the
		// controller set a field in memory".
		By("and the volume stays Snapshotting with the reader's own words recorded against it")
		Eventually(func(g Gomega) {
			vol := volumeByPVC(getBackupG(g, ns, run), pvcName)
			g.Expect(vol).NotTo(BeNil())
			g.Expect(string(vol.Phase)).To(Equal("Snapshotting"),
				"a check that could not be CARRIED OUT is not a verdict: the volume has an exposure and a "+
					"clock running on the origin snapshot, and only its deadline may end that")
			g.Expect(vol.Reason).To(HavePrefix(backupReasonAdvanceRetrying+":"),
				"nothing recorded means nothing persisted: the pass returned its error before writeStatus, "+
					"so the volume was re-driven forever with an empty status nobody could diagnose")
			g.Expect(vol.Reason).To(ContainSubstring("volumesnapshots"),
				"the reason must carry the apiserver's own words — an operator must learn that RBAC refused "+
					"the read, not merely that we could not advance")
			// Bounded: a status field is not a log.
			g.Expect(len(vol.Reason)).To(BeNumerically("<=", clusterBackupMessageCap))
		}, initTimeout, initPoll).Should(Succeed())

		By("and no mover Job was created for an exposure nobody could vouch for")
		Consistently(func(g Gomega) {
			vol := volumeByPVC(getBackupG(g, ns, run), pvcName)
			g.Expect(vol).NotTo(BeNil())
			g.Expect(string(vol.Phase)).To(Equal("Snapshotting"))
			err := k8sClient.Get(ctx, client.ObjectKey{
				Namespace: suiteOperatorNamespace, Name: moverJobNameFor(ns, run, pvcName),
			}, &batchv1.Job{})
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue(),
				"an unanswerable readiness check must never be read as ready: %v", err)
		}, 3*time.Second, 500*time.Millisecond).Should(Succeed())
	})

	// THE SPEC OF THIS LOT, and it asserts the two halves that are worthless apart: the namespace
	// keeps moving, and the broken volume is BOUNDED rather than retried forever.
	//
	// The layout is the incident's. The broken PVC sorts FIRST, so it holds the head of the queue —
	// and unlike a parked Pending volume, a volume in Snapshotting is NOT deferred by
	// firstNonTerminalVolume (it legitimately holds work in flight), so the only thing that can ever
	// free the queue is that volume going terminal. Which is exactly why the deadline being reachable
	// is not a nicety: it is the sole mechanism by which the volume behind this one ever gets backed
	// up at all.
	//
	// Mutations that must turn this spec red: restoring the bare error return on ex.Ready (the
	// deadline is never consulted, the broken volume stays Snapshotting forever, and vol-b never gets
	// a mover Job — the thirty-hour incident, reproduced); and removing the deadline evaluation from
	// the not-ready branch (same outcome by a different route).
	It("bounds a volume whose readiness can never be checked, and backs up the one behind it", func() {
		const (
			location  = "bk-rdydl"
			ns        = "bk-rdydl-ns"
			run       = "bk-rdydl-run"
			brokenPVC = "vol-a-broken" // sorted first: the head of the queue, as in the incident
			okPVC     = "vol-b-ok"
		)
		backupExposers.failReady(ns+"/"+brokenPVC, stubReadyFailure())
		// Started well past snapshotProgressDeadline, so the bound is crossed on the first
		// Snapshotting pass: the spec is about the DECISION being reachable, not about waiting fifteen
		// real minutes for it.
		backupExposers.stallSnapshot(ns+"/"+brokenPVC, time.Now().Add(-2*snapshotProgressDeadline))

		seedInitializedRepo(location, "kek-bk-rdydl", "s3-bk-rdydl")
		createTenantNamespace(ns)
		createSourcePVC(ns, brokenPVC, "ceph-block")
		createSourcePVC(ns, okPVC, "ceph-block")
		createVolumeOnlyParent(run, location, cbv1.PVCSelector{})
		createChildBackup(ns, run, location)

		By("the deadline ends the broken volume even though the readiness check never answered")
		Eventually(func(g Gomega) {
			vol := volumeByPVC(getBackupG(g, ns, run), brokenPVC)
			g.Expect(vol).NotTo(BeNil())
			g.Expect(string(vol.Phase)).To(Equal("Failed"),
				"phase=%q — the two Snapshotting bounds used to be evaluated ONLY when Ready cleanly "+
					"answered 'not ready', so on the path where Ready itself fails they were unreachable and "+
					"the volume was retried forever", vol.Phase)
			g.Expect(vol.Reason).To(Equal(backupReasonSnapshotProgressDeadline),
				"the verdict must be the deadline's own diagnosis (nothing ever picked the snapshot up), "+
					"which names the components to go and look at")
		}, initTimeout, initPoll).Should(Succeed())

		// The load-bearing observation, and the one the old code silently did not produce.
		By("and the volume queued BEHIND it is exposed, moved and Completed anyway")
		jobName := waitForMoverJob(ns, run, okPVC)
		simulateMoverSucceeded(jobName, "node-a", mover.MoverResult{
			OK: true, Operation: string(mover.OpBackup), SnapshotID: "snap-rdydl-b", SizeBytes: 60, AddedBytes: 30,
		})
		Eventually(func(g Gomega) {
			g.Expect(string(volumeByPVC(getBackupG(g, ns, run), okPVC).Phase)).To(Equal("Completed"))
		}, initTimeout, initPoll).Should(Succeed())

		By("and the run reaches a terminal phase: one Failed, one Completed => PartiallyFailed")
		Eventually(func(g Gomega) {
			b := getBackupG(g, ns, run)
			g.Expect(isTerminalBackupPhase(b.Status.Phase)).To(BeTrue(),
				"phase=%q — a run held non-terminal by one volume is what made a Forbid schedule skip "+
					"thirty-one hours of nightlies", b.Status.Phase)
			g.Expect(b.Status.CompletionTime).NotTo(BeNil())
		}, initTimeout, initPoll).Should(Succeed())

		By("and the broken volume's exposure is torn down like any other terminal volume's")
		Eventually(func(g Gomega) {
			g.Expect(backupExposers.teardownCalls()).To(ContainElement(ns + "/" + moverNamePrefix(ns, run, brokenPVC)))
		}, initTimeout, initPoll).Should(Succeed())
	})

	// THE STRUCTURAL GUARD ITSELF, reached through a door the controller does NOT handle locally.
	//
	// The readiness path above is fixed twice over: the error is turned into "not ready" inside
	// advanceSnapshotting so the deadline is reachable, AND step (10) would have recorded it anyway.
	// This spec removes the first of those from the picture and tests the second alone, by making
	// reconstructExposure fail — the step that runs BEFORE the readiness check and whose failure
	// advanceSnapshotting still returns, because the exposure it could not reconstruct is the very
	// thing the deadline evaluation needs in order to read the origin snapshot's clock.
	//
	// The arming order is the mechanism, not a convenience. The stall goes on FIRST so the volume
	// reaches Snapshotting (Expose succeeds once) and then stays there — Ready answers "not ready" and
	// the origin snapshot is young, so no deadline can move it. Only then is Expose armed to fail, so
	// every later pass dies in reconstructExposure with the volume pinned in the phase under test.
	//
	// What it pins is exactly the invariant, and nothing more: a step that errors must not cost the
	// pass its status write. It deliberately does NOT claim the volume is bounded — it is not, and
	// that is the honest residual of this lot: for a Snapshotting volume the only clock lives on the
	// origin VolumeSnapshot, reachable only through the exposure this door could not reconstruct.
	//
	// Mutation that must turn this spec red: restoring `return ctrl.Result{}, err` at Reconcile step
	// (10). The reason is then computed in memory on every pass and thrown away with the error, and
	// the stored object shows a Snapshotting volume with nothing recorded against it — which is the
	// state the incident was invisible in.
	It("persists the cause when a step upstream of the readiness check errors", func() {
		const (
			location = "bk-rdyrec"
			ns       = "bk-rdyrec-ns"
			run      = "bk-rdyrec-run"
			pvcName  = "data-vol"
		)
		// Young and not ready: the volume parks itself in Snapshotting for the whole spec without any
		// deadline being a candidate.
		backupExposers.stallSnapshot(ns+"/"+pvcName, time.Now())

		seedInitializedRepo(location, "kek-bk-rdyrec", "s3-bk-rdyrec")
		createTenantNamespace(ns)
		createSourcePVC(ns, pvcName, "ceph-block")
		createVolumeOnlyParent(run, location, cbv1.PVCSelector{})
		createChildBackup(ns, run, location)

		By("the volume reaches Snapshotting with a clean, empty reason")
		Eventually(func(g Gomega) {
			vol := volumeByPVC(getBackupG(g, ns, run), pvcName)
			g.Expect(vol).NotTo(BeNil())
			g.Expect(string(vol.Phase)).To(Equal("Snapshotting"))
			g.Expect(vol.Reason).To(BeEmpty())
		}, initTimeout, initPoll).Should(Succeed())

		By("when the exposure can no longer be reconstructed — a quota that will not admit the clone")
		backupExposers.failExpose(ns+"/"+pvcName, apierrors.NewForbidden(
			schema.GroupResource{Resource: "persistentvolumeclaims"}, "data-vol-clone",
			errors.New(`exceeded quota: tenant-quota, requested: requests.storage=1Gi, limited: requests.storage=10Gi`)))

		By("then the cause is recorded on the volume and REACHES ETCD, read back uncached")
		Eventually(func(g Gomega) {
			vol := volumeByPVC(getBackupG(g, ns, run), pvcName)
			g.Expect(vol).NotTo(BeNil())
			g.Expect(string(vol.Phase)).To(Equal("Snapshotting"),
				"the phase is what the next pass dispatches on and what the volume's clock is measured "+
					"against: recording a cause must never move it")
			g.Expect(vol.Reason).To(HavePrefix(backupReasonAdvanceRetrying+":"),
				"the pass returned its error before writeStatus, so everything it computed — this reason "+
					"included — was discarded, every pass, forever")
			g.Expect(vol.Reason).To(ContainSubstring("exceeded quota"),
				"the reason must carry the apiserver's own words, not merely our verdict")
			g.Expect(len(vol.Reason)).To(BeNumerically("<=", clusterBackupMessageCap))
		}, initTimeout, initPoll).Should(Succeed())
	})

	// THE FREEZE WINDOW, which is the question this lot had to answer before it was allowed to change
	// control flow: Reconcile step (10c) closes the window, and continuing into it on a pass where a
	// volume's snapshot could not be cut sounds like thawing a database whose backup has not been
	// taken.
	//
	// It is not, and the answer is that the guard was already there and is the right one.
	// closeFreezeWindow fires on snapshotsCut — every volume having LEFT Pending/Snapshotting — and
	// "could not advance" means precisely that no phase transition happened, so the volume is still in
	// one of those two phases and the window stays open by construction. The invariant is "no volume
	// is still in the snapshot phase", never "this pass had no error": a volume that goes Skipped or
	// Failed on the same pass DOES release, deliberately, because R16's thaw is unconditional (a
	// failed snapshot leaves the application just as quiesced, so gating the release on success
	// strands exactly the workload whose backup went wrong).
	//
	// So this spec states both halves, in order, and the second is what makes the first safe rather
	// than merely cautious: while the snapshot cannot be cut the application stays frozen, and it is
	// the DEADLINE — reachable only because of this lot — that eventually ends the volume and releases
	// it. Had the honest answer been "add a not-on-an-errored-pass gate", the release would have been
	// conditional on a cluster being repaired, and a durable failure would have left a database frozen
	// for as long as nobody noticed.
	//
	// Mutations that must turn this spec red: gating closeFreezeWindow on the pass having had no error
	// (the release never runs, so the second half fails); dropping the snapshotsCut guard (the release
	// runs during the first half, which is a database thawed while its snapshot was still owed).
	It("holds the freeze window open while the snapshot cannot be cut, and releases it on the deadline", func() {
		const (
			location = "bk-rdyhook"
			ns       = "bk-rdyhook-ns"
			run      = "bk-rdyhook-run"
			pvcName  = "data-vol"
		)
		hookExecutor.reset()
		backupExposers.failReady(ns+"/"+pvcName, stubReadyFailure())
		// Inside the bound to begin with: the window must stay open for as long as the volume is still
		// owed a snapshot.
		backupExposers.stallSnapshot(ns+"/"+pvcName, time.Now())

		seedInitializedRepo(location, "kek-bk-rdyhook", "s3-bk-rdyhook")
		createTenantNamespace(ns)
		createSourcePVC(ns, pvcName, "ceph-block")
		createMountingPod(ns, "db-0", pvcName, nil)
		createHookedParent(run, location, cbv1.PVCSelector{}, cbv1.HooksSpec{
			Pre:  []cbv1.Hook{{Command: []string{"psql", "-c", "CHECKPOINT"}}},
			Post: []cbv1.Hook{{Command: []string{"psql", "-c", "SELECT pg_backup_stop()"}}},
		})
		createChildBackup(ns, run, location)

		By("the quiesce runs and is recorded: the window is OPEN")
		Eventually(func(g Gomega) {
			g.Expect(hookPhaseCount(getBackupG(g, ns, run), hooks.PhasePre)).To(Equal(1))
		}, initTimeout, initPoll).Should(Succeed())

		By("and it stays open while the volume is still owed a snapshot, pass after pass")
		Consistently(func(g Gomega) {
			b := getBackupG(g, ns, run)
			g.Expect(string(volumeByPVC(b, pvcName).Phase)).To(Equal("Snapshotting"),
				"the volume must still be in the snapshot phase — otherwise this spec is not testing what "+
					"it claims")
			g.Expect(hookPhaseCount(b, hooks.PhasePost)).To(Equal(0),
				"the release fired while a snapshot was still owed: a database thawed before the "+
					"point-in-time copy it was frozen for even exists")
		}, 4*time.Second, 500*time.Millisecond).Should(Succeed())

		// And now the other half, on the clock this lot made reachable. Nothing about the cluster is
		// repaired — Ready still refuses forever — but the origin snapshot is now old enough for the
		// progress deadline to end the volume, which is the ONLY way this window ever closes.
		By("when the deadline finally ends the volume, the release runs — the thaw is unconditional")
		backupExposers.stallSnapshot(ns+"/"+pvcName, time.Now().Add(-2*snapshotProgressDeadline))
		Eventually(func(g Gomega) {
			b := getBackupG(g, ns, run)
			g.Expect(string(volumeByPVC(b, pvcName).Phase)).To(Equal("Failed"))
			g.Expect(hookPhaseCount(b, hooks.PhasePost)).To(BeNumerically(">=", 1),
				"the application is still quiesced and its backup has just been given up on: a release "+
					"conditioned on success would strand exactly this workload")
		}, initTimeout, initPoll).Should(Succeed())
	})
})

// hookPhaseCount counts the recorded hook entries of one phase on a Backup — the durable record the
// freeze window's state is derived from (hookState/postHooksRan), so a spec asserting on it is
// asserting on what a restarted controller would read.
func hookPhaseCount(b cbv1.Backup, phase hooks.Phase) int {
	n := 0
	for i := range b.Status.Hooks {
		if b.Status.Hooks[i].Phase == string(phase) {
			n++
		}
	}
	return n
}

// TestSnapshotDeadlinesAreReachableFromAnUnansweredCheck is the guard on the ORDERING that this lot
// turned out to be about, stated where a reader will find it without reading an envtest spec.
//
// snapshotDeadlineExceeded's verdict comes from Progress — an independent read of the origin
// VolumeSnapshot — and never from Ready. That is what makes it safe to evaluate on a pass where Ready
// could not answer, and it is the reason the fix could be a reordering rather than a new mechanism.
// The two properties pinned here are the whole contract: evidence of abandonment produces a verdict,
// and the absence of evidence produces none, whatever Ready did.
func TestSnapshotDeadlinesAreReachableFromAnUnansweredCheck(t *testing.T) {
	// The bounds themselves must stay ordered and generous, or the reordering above becomes a way to
	// fail healthy volumes faster.
	if snapshotProgressDeadline >= snapshotReadyDeadline {
		t.Errorf("snapshotProgressDeadline (%v) must stay well under snapshotReadyDeadline (%v): "+
			"unacknowledged is strong evidence of abandonment, acknowledged-and-slow is not",
			snapshotProgressDeadline, snapshotReadyDeadline)
	}

	cases := []struct {
		name       string
		progress   exposer.SnapshotProgress
		known      bool
		wantReason string
	}{
		{
			name:       "nothing ever picked the snapshot up: the unattended diagnosis",
			progress:   exposer.SnapshotProgress{StartedAt: time.Now().Add(-2 * snapshotProgressDeadline)},
			known:      true,
			wantReason: backupReasonSnapshotProgressDeadline,
		},
		{
			name: "picked up and never finished: the other diagnosis, on a far longer bound",
			progress: exposer.SnapshotProgress{
				StartedAt: time.Now().Add(-2 * snapshotReadyDeadline), Acknowledged: true},
			known:      true,
			wantReason: backupReasonSnapshotReadyDeadline,
		},
		{
			name:       "young and untouched: no verdict, wait for the next pass",
			progress:   exposer.SnapshotProgress{StartedAt: time.Now()},
			known:      true,
			wantReason: "",
		},
		{
			// The asymmetry that matters: an unreadable origin snapshot is not evidence of anything,
			// and these branches terminate somebody's backup.
			name:       "nothing readable about the snapshot: never a verdict",
			known:      false,
			wantReason: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &BackupReconciler{}
			ex := &progressOnlyExposer{progress: tc.progress, known: tc.known}
			if got := r.snapshotDeadlineExceeded(context.Background(), ex, &exposer.Exposure{}); got != tc.wantReason {
				t.Errorf("snapshotDeadlineExceeded = %q, want %q", got, tc.wantReason)
			}
			if !ex.consulted {
				t.Error("Progress was never consulted, so the verdict cannot have come from the origin " +
					"snapshot — which is the only thing that makes this evaluation safe when Ready failed")
			}
		})
	}
}

// progressOnlyExposer is a SnapshotExposer whose Ready ALWAYS FAILS, so a test using it can only
// reach a verdict through Progress. That is the point: it is the shape of the cluster this lot is
// about (the readiness read refused, the origin snapshot still readable), and it makes the assertion
// "the deadline does not depend on Ready" structural rather than a matter of reading the code.
type progressOnlyExposer struct {
	progress  exposer.SnapshotProgress
	known     bool
	consulted bool
}

var _ exposer.SnapshotExposer = (*progressOnlyExposer)(nil)

func (e *progressOnlyExposer) Kind() string                   { return "progress-only" }
func (e *progressOnlyExposer) Precheck(context.Context) error { return nil }

func (e *progressOnlyExposer) Expose(context.Context, exposer.ExposeRequest) (*exposer.Exposure, error) {
	return nil, errors.New("progressOnlyExposer never exposes")
}

func (e *progressOnlyExposer) Ready(context.Context, *exposer.Exposure) (bool, error) {
	return false, errors.New("progressOnlyExposer: readiness is never answerable")
}

func (e *progressOnlyExposer) Progress(context.Context, *exposer.Exposure) (exposer.SnapshotProgress, bool) {
	e.consulted = true
	return e.progress, e.known
}

func (e *progressOnlyExposer) Cleanup(context.Context, *exposer.Exposure) error { return nil }
