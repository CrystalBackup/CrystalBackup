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
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2" //nolint:revive,staticcheck
	. "github.com/onsi/gomega"    //nolint:revive,staticcheck

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/mover"
	"github.com/CrystalBackup/CrystalBackup/internal/status"
)

// ---------------------------------------------------------------------------
// THE HEAD-OF-LINE BLOCK IN Pending, and the three mechanisms that close it.
//
// The incident these tests pin, in the order the operator lived it: one PVC in one namespace named
// a StorageClass that had been deleted. Resolution returned an error on every pass, so advancePending
// returned an error on every pass, so the volume never left Pending. Reconcile advances ONE volume
// per pass and used to pick the first non-terminal one BY POSITION — so the five volumes queued
// behind that one were never even attempted. For thirty hours. The Backup never reached a terminal
// phase, so its ClusterBackup never did either, so the schedule's concurrencyPolicy: Forbid skipped
// every following night: thirty-one hours of a nightly schedule produced nothing, over a defect
// affecting ONE volume out of thirty-three namespaces.
//
// Three things had to change, and each of the three is worthless without the other two:
//
//  1. advancePending records the cause instead of erroring, so the pass completes and the status write
//     happens (nothing is learned about a volume whose reconcile always errors — the enumeration
//     itself is not even persisted). It no longer HAS an error result: see advanceVolume's Pending
//     case, and backup_snapshotting_stall_test.go for the same defect one and two phases later;
//  2. firstNonTerminalVolume prefers un-tried volumes over PARKED ones, so a broken volume cannot
//     starve healthy ones — the product owner's rule, verbatim: blocking a backup that could have
//     succeeded because something scheduled ahead of it is broken is not acceptable;
//  3. pendingResolveDeadline bounds Pending, so a run can still reach a terminal phase and stop
//     poisoning its schedule.
//
// (1) alone retries forever behind the same broken head. (2) alone re-orders a queue that still never
// terminates. (3) alone fails the whole namespace on the clock of its worst volume. The specs below
// are written so that breaking any ONE of the three turns at least one of them red — see the
// mutation notes on each.
//
// HOW A RESOLUTION FAILURE IS REACHED IN envtest. The suite's exposer registry is a stub (envtest has
// no snapshot CRDs and no CSI driver), and it keys its verdicts off MAGIC VALUES rather than shared
// mutable flags, so specs running against one shared manager cannot perturb each other. This lot adds
// one more magic storage class — the same convention, and it models the incident exactly: a PVC that
// names a class which does not exist.
// ---------------------------------------------------------------------------

// stubUnresolvableStorageClass is the magic storageClassName stubExposerRegistry.For cannot resolve at
// all: not "this storage cannot snapshot" (that is stubUnsupportedStorageClass -> ErrUnsupported ->
// Skipped) and not a refused pre-check, but the third case — the class the PVC names is simply not
// there. It is the shape of the production incident.
const stubUnresolvableStorageClass = "deleted-out-from-under-us.test"

// stubUnresolvableExposerError is the error the stub registry answers for that class, and it is
// deliberately an apiserver NotFound.
//
// That choice is itself an assertion. The FIRST version of this fix classified the error kind and
// turned a NotFound into an immediate Failed, on the reasoning that a missing reference is a
// configuration fact no retry can change. It is wrong in both directions — a StorageClass absent this
// second can be created the next, and this operator has already been bitten by a cached client
// answering NotFound for an object that existed (see the read-after-write incident) — so the shipped
// behaviour must treat the most permanent-LOOKING error there is exactly like any other: record it,
// park the volume, retry, and let the clock be the only arbiter of permanence. Handing the stub a
// NotFound is how these specs prove that no special case came back.
func stubUnresolvableExposerError() error {
	return apierrors.NewNotFound(
		schema.GroupResource{Group: "storage.k8s.io", Resource: "storageclasses"},
		stubUnresolvableStorageClass)
}

// pendingDeadlineReconciler is a shallow copy of the registered Backup reconciler with a shortened
// Pending bound — the same device (and the same shallow-on-purpose sharing of client, recorder,
// exposer registry and queue) as deadlineReconciler in backup_progress_deadline_test.go, which see
// for why a copy rather than a mutation of the registered object.
//
// Unlike the mover-start bound, this clock IS a field the controller writes itself
// (status.volumes[].firstAttemptAt), so a spec could backdate it instead. Shortening the bound is
// still the better instrument: it makes the controller stamp its own clock and then honour it, which
// is the behaviour that matters, rather than asserting on a timestamp the test wrote.
func pendingDeadlineReconciler(bound time.Duration) *BackupReconciler {
	GinkgoHelper()
	Expect(backupReconciler).NotTo(BeNil(), "the suite's Backup reconciler was never wired")
	copied := *backupReconciler
	copied.PendingResolveDeadline = bound
	return &copied
}

// ---------------------------------------------------------------------------
// The scheduler, as a pure function.
// ---------------------------------------------------------------------------

// TestFirstNonTerminalVolumeSkipsParkedVolumes is the unit-level guard on the queue discipline that
// the thirty-hour block came down to. firstNonTerminalVolume is pure over []VolumeStatus, so the
// cases that mattered in production can be stated directly instead of being provoked through a
// controller.
//
// Every case here is a real shape, not a permutation for its own sake:
//
//   - "parked head, healthy behind" IS the incident: the broken volume is at index 0 and the volume
//     that could have been backed up is behind it. The answer must be 1.
//   - "all parked" is the case that decides whether parking is a deferral or a drop. Returning -1
//     would read as "nothing to do", the Backup would roll up on volumes that were never retried,
//     and pendingResolveDeadline — whose clock only advances when advancePending is actually called —
//     would never fire, which puts us straight back to a run that never terminates.
//   - the empty phase is the one that would quietly re-open the bug: advanceVolume's own dispatch
//     accepts "" as Pending (`case status.VolumePhasePending, ""`), so a predicate that did not would
//     leave exactly one shape of volume able to park itself and hold the head of the queue anyway.
//   - a Pending volume with SOME OTHER reason must NOT be parked. Parking is earned by a recorded
//     resolution failure, not by having a reason at all; treating any annotated Pending volume as
//     parked would de-prioritise volumes that are making progress.
func TestFirstNonTerminalVolumeSkipsParkedVolumes(t *testing.T) {
	parked := func(pvc string, phase status.VolumePhase) cbv1.VolumeStatus {
		return cbv1.VolumeStatus{
			Pvc:    pvc,
			Phase:  phase,
			Reason: backupReasonExposerUnresolvable + `: storageclasses.storage.k8s.io "gone" not found`,
		}
	}

	cases := []struct {
		name string
		vols []cbv1.VolumeStatus
		want int
	}{
		{
			// The incident, minimised: one broken volume at the head, one healthy volume behind it.
			name: "parked head is skipped for the untried volume behind it",
			vols: []cbv1.VolumeStatus{
				parked("vol-a-broken", status.VolumePhasePending),
				{Pvc: "vol-b-ok", Phase: status.VolumePhasePending},
			},
			want: 1,
		},
		{
			// Parked volumes are DEFERRED, never dropped: with nothing else to do, the first of them
			// is returned so it is retried and its Pending clock keeps advancing toward the deadline.
			name: "every non-terminal volume parked returns the first parked index",
			vols: []cbv1.VolumeStatus{
				{Pvc: "vol-done", Phase: status.VolumePhaseCompleted},
				parked("vol-b-broken", status.VolumePhasePending),
				parked("vol-c-broken", status.VolumePhasePending),
			},
			want: 1,
		},
		{
			// A volume already in flight is not parked and keeps the head — the preference re-orders
			// around BROKEN volumes only, it does not turn the queue into a scheduler.
			name: "an in-flight volume outranks a parked one regardless of position",
			vols: []cbv1.VolumeStatus{
				parked("vol-a-broken", status.VolumePhasePending),
				{Pvc: "vol-b-snapshotting", Phase: status.VolumePhaseSnapshotting},
			},
			want: 1,
		},
		{
			// The dispatch treats "" as Pending, so the parking predicate must too.
			name: "empty phase with a resolution failure is parked, like the dispatch's Pending",
			vols: []cbv1.VolumeStatus{
				parked("vol-a-broken", ""),
				{Pvc: "vol-b-ok", Phase: status.VolumePhasePending},
			},
			want: 1,
		},
		{
			// Parking is earned by the resolution-failure reason specifically.
			name: "a Pending volume with an unrelated reason is not parked",
			vols: []cbv1.VolumeStatus{
				{Pvc: "vol-a", Phase: status.VolumePhasePending, Reason: "WaitingForHooks"},
				{Pvc: "vol-b-ok", Phase: status.VolumePhasePending},
			},
			want: 0,
		},
		{
			name: "all terminal returns -1",
			vols: []cbv1.VolumeStatus{
				{Pvc: "vol-a", Phase: status.VolumePhaseCompleted},
				{Pvc: "vol-b", Phase: status.VolumePhaseSkipped},
				{Pvc: "vol-c", Phase: status.VolumePhaseFailed},
			},
			want: -1,
		},
		{
			// A terminal volume that happens to carry a resolution failure (the deadline preserves the
			// reason when it fails a parked volume) must stay terminal, not be resurrected as parked.
			name: "a Failed volume keeping its resolution reason is terminal, not parked",
			vols: []cbv1.VolumeStatus{
				parked("vol-a-failed", status.VolumePhaseFailed),
				{Pvc: "vol-b-ok", Phase: status.VolumePhasePending},
			},
			want: 1,
		},
		{
			// THE SEPARATOR. Parking is granted to one exact token — "ExposerUnresolvable:" — and not to
			// any reason that merely begins with the same word. A bare prefix test would enrol a FUTURE
			// reason like this one into a queue rule nobody reasoned about for it: whoever adds
			// "ExposerUnresolvableAfterExpose" is describing a volume that HAS an exposure, and
			// de-prioritising that behind every un-tried volume in the namespace would delay a snapshot
			// already cut (and already counting against the storage) with no one having decided so.
			name: "a reason starting with the token but continuing into another word is not parked",
			vols: []cbv1.VolumeStatus{
				{
					Pvc:    "vol-a",
					Phase:  status.VolumePhasePending,
					Reason: backupReasonExposerUnresolvable + "AfterExpose: quota exceeded",
				},
				{Pvc: "vol-b-ok", Phase: status.VolumePhasePending},
			},
			want: 0,
		},
		{
			name: "no volumes at all returns -1",
			vols: nil,
			want: -1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := firstNonTerminalVolume(tc.vols); got != tc.want {
				t.Errorf("firstNonTerminalVolume = %d, want %d — a volume that cannot resolve must "+
					"never be chosen over one that has not been tried, and must never be dropped either",
					got, tc.want)
			}
		})
	}
}

// TestVolumeIsParkedRequiresTheSeparator states the separator rule directly on the predicate rather
// than only through the scheduler, because the scheduler could stop calling it and the rule would
// evaporate silently. It is the same property as the table case above, said where a reader looking for
// "what counts as parked" will find it.
func TestVolumeIsParkedRequiresTheSeparator(t *testing.T) {
	cases := []struct {
		reason string
		want   bool
	}{
		{backupReasonExposerUnresolvable + ": storageclasses.storage.k8s.io \"x\" not found", true},
		// The exact token with nothing after it: no separator, no parking. parkVolume always writes the
		// colon, so this shape can only come from somewhere that has not been reasoned about.
		{backupReasonExposerUnresolvable, false},
		{backupReasonExposerUnresolvable + "AfterExpose: quota exceeded", false},
		{backupReasonExposerUnresolvable + "Transient: connection refused", false},
		{"", false},
		{backupReasonPendingDeadline, false},
	}
	for _, tc := range cases {
		vol := &cbv1.VolumeStatus{Phase: status.VolumePhasePending, Reason: tc.reason}
		if got := volumeIsParked(vol); got != tc.want {
			t.Errorf("volumeIsParked(reason=%q) = %v, want %v", tc.reason, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// The Pending deadline's two verdicts, at the unit level.
//
// The deadline branch sits at the very top of advancePending and returns before any I/O, so both
// verdicts can be reached with no client at all. That is why they are unit tests: the FALLBACK one in
// particular is not reachable end to end today — every path that leaves a volume Pending records a
// reason on the same pass that stamps firstAttemptAt — so the only honest way to pin it is to build
// the state directly and say so. It is not dead code: it is what keeps a FUTURE Pending-preserving
// branch from producing a Failed volume with no reason at all.
// ---------------------------------------------------------------------------

// pendingDeadlineFixture builds the (reconciler, backup, volume) triple for the deadline branch: a
// volume first attempted long ago, with whatever reason the case is about.
func pendingDeadlineFixture(reason string) (*BackupReconciler, *cbv1.Backup, *cbv1.VolumeStatus) {
	longAgo := metav1.NewTime(time.Now().Add(-2 * time.Hour))
	r := &BackupReconciler{
		// Buffered: the branch emits exactly one Event, and an unbuffered fake would deadlock.
		Recorder:               events.NewFakeRecorder(4),
		PendingResolveDeadline: time.Minute,
	}
	backup := &cbv1.Backup{ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-a", Name: "nightly-run"}}
	vol := &cbv1.VolumeStatus{
		Pvc:            "data-vol",
		Phase:          status.VolumePhasePending,
		Reason:         reason,
		FirstAttemptAt: &longAgo,
	}
	return r, backup, vol
}

// TestPendingDeadlineKeepsTheRecordedCause pins the choice between naming the clock and naming the
// fault. When the deadline fires on a volume whose resolution failure was already recorded, the
// reason the operator reads must still be that failure — "ExposerUnresolvable: storageclasses… not
// found" sends them to the PVC that names a class nobody created, while "PendingDeadlineExceeded"
// only tells them we gave up, which is the same blindness the incident's thirty silent hours had.
func TestPendingDeadlineKeepsTheRecordedCause(t *testing.T) {
	cause := backupReasonExposerUnresolvable +
		`: storageclasses.storage.k8s.io "deleted-out-from-under-us.test" not found`
	r, backup, vol := pendingDeadlineFixture(cause)

	// advancePending RETURNS NOTHING, and the assertion that used to stand here ("it must not error
	// its reconcile — a Pending volume that errors takes its whole namespace down with it") is now
	// made by the compiler instead. That is strictly stronger: an error result that is always nil can
	// be filled in again by anyone, whereas re-adding one is a signature change that has to be argued
	// for at advanceVolume's Pending case. `unparam` is what pointed at the vestigial result.
	r.advancePending(context.Background(), backup, vol)
	if vol.Phase != status.VolumePhaseFailed {
		t.Errorf("phase = %q, want Failed: a volume past the Pending deadline must go terminal so the "+
			"run can too", vol.Phase)
	}
	if vol.Reason != cause {
		t.Errorf("reason = %q, want the recorded cause %q — the deadline must not overwrite the "+
			"diagnosis with its own name", vol.Reason, cause)
	}
}

// TestPendingDeadlineFallbackReason is the other half: a volume that reaches the deadline having
// recorded nothing must still say why it failed, and "PendingDeadlineExceeded" is the honest answer —
// it names what we know (it never left Pending) rather than inventing a cause.
func TestPendingDeadlineFallbackReason(t *testing.T) {
	r, backup, vol := pendingDeadlineFixture("")

	r.advancePending(context.Background(), backup, vol)
	if vol.Phase != status.VolumePhaseFailed {
		t.Errorf("phase = %q, want Failed", vol.Phase)
	}
	if vol.Reason != backupReasonPendingDeadline {
		t.Errorf("reason = %q, want %q: a Failed volume with an empty reason is a volume nobody can "+
			"diagnose", vol.Reason, backupReasonPendingDeadline)
	}
}

// TestPendingDeadlineIsLongEnoughToBeSafe guards the number itself, in the one direction that costs
// data. This deadline FAILS volumes; a value shortened to a few minutes would start failing volumes
// whose resolution was merely slow (an apiserver having a bad minute, a VolumeSnapshotClass being
// installed while the cluster comes up), and the fix for a premature failure is a phone call at 3am
// whereas the fix for a late one is a slightly longer report. The upper bound is the other half of
// the contract: past a night, the deadline stops rescuing the schedule it exists to rescue.
func TestPendingDeadlineIsLongEnoughToBeSafe(t *testing.T) {
	if pendingResolveDeadline < 30*time.Minute {
		t.Errorf("pendingResolveDeadline = %v, short enough to fail volumes whose resolution was "+
			"merely slow", pendingResolveDeadline)
	}
	if pendingResolveDeadline > 8*time.Hour {
		t.Errorf("pendingResolveDeadline = %v: a bound that outlives the night cannot stop a Forbid "+
			"schedule from skipping the next one", pendingResolveDeadline)
	}
	// And an unset override must resolve to the constant, or every production install would run on
	// whatever a test last left behind.
	if got := (&BackupReconciler{}).effectivePendingResolveDeadline(); got != pendingResolveDeadline {
		t.Errorf("effectivePendingResolveDeadline with no override = %v, want the constant %v",
			got, pendingResolveDeadline)
	}
	if got := (&BackupReconciler{PendingResolveDeadline: time.Second}).effectivePendingResolveDeadline(); got != time.Second {
		t.Errorf("effectivePendingResolveDeadline ignored the injected override: %v", got)
	}
}

// ---------------------------------------------------------------------------
// The fourth parking path: a source PVC that is there but cannot be READ.
//
// UNIT-ONLY, and deliberately so. Every other parking path has an envtest spec because the exposer
// registry is an injectable seam; this one is not reachable there. Making the manager's own cached Get
// fail for one PVC would mean wrapping the client the REGISTERED reconciler uses — the suite's
// reconciler co-drives every Backup in the package, so a wrapper installed on a copy would be
// contradicted by the real client a second later, and a wrapper installed on the shared object would
// change the client every other spec in the suite reads through. The interceptor fake client below
// exercises the real advancePending against a Get that fails, which is the whole of the path.
//
// It matters more than its unlikelihood suggests: an unreadable PVC is the path a reviewer is most
// likely to argue should be a plain requeue-with-error, since RBAC changing under a running operator
// or an apiserver having a bad minute really are transient. But "one volume cannot stop the others" is
// worth nothing if it holds on three paths out of four, and which door the error came through makes no
// difference to the namespace waiting behind it.
// ---------------------------------------------------------------------------

// TestAdvancePendingParksAnUnreadableSourcePVC pins that a non-NotFound Get parks rather than errors.
func TestAdvancePendingParksAnUnreadableSourcePVC(t *testing.T) {
	// etcdserver: request timed out is the real shape: the object exists, the read failed.
	getErr := apierrors.NewInternalError(errors.New("etcdserver: request timed out"))
	c := fake.NewClientBuilder().
		WithScheme(podScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(_ context.Context, _ client.WithWatch, key client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
				if key.Name == "data-vol" {
					return getErr
				}
				return nil
			},
		}).
		Build()
	r := &BackupReconciler{Client: c, Recorder: events.NewFakeRecorder(4)}
	backup := &cbv1.Backup{ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-a", Name: "nightly-run"}}
	vol := &cbv1.VolumeStatus{Pvc: "data-vol", Phase: status.VolumePhasePending}

	// No error result to check: an error here would be returned by Reconcile BEFORE writeStatus, so it
	// would strand every volume queued behind this one AND discard the firstAttemptAt stamp the Pending
	// deadline needs in order to ever fire — which is why advancePending no longer has one to return.
	r.advancePending(context.Background(), backup, vol)
	if vol.Phase != status.VolumePhasePending {
		t.Errorf("phase = %q, want Pending: an unreadable PVC is not a verdict about the volume's data",
			vol.Phase)
	}
	if !volumeIsParked(vol) {
		t.Errorf("reason = %q: a volume left Pending without the parking token holds the head of its "+
			"namespace's queue, which is the whole defect", vol.Reason)
	}
	if !strings.Contains(vol.Reason, "etcdserver") {
		t.Errorf("reason = %q, want the reader's own words in it", vol.Reason)
	}
	if vol.FirstAttemptAt == nil {
		t.Error("firstAttemptAt was not stamped, so this volume can never reach the Pending deadline")
	}
}

// TestAdvancePendingStillFailsAMissingSourcePVC is the anti-regression that keeps the parking door from
// swallowing a VERDICT. A PVC that is NotFound has been deleted since enumeration: there is nothing to
// back up, ever, and the honest answer is Failed/SourcePVCMissing on the first pass. Parking it instead
// would hold a namespace open for an hour waiting for a PVC nobody is going to recreate, and would
// report "ExposerUnresolvable" for a volume whose exposer was never the problem.
func TestAdvancePendingStillFailsAMissingSourcePVC(t *testing.T) {
	// Empty client: every Get is a real NotFound.
	r := &BackupReconciler{
		Client:   fake.NewClientBuilder().WithScheme(podScheme(t)).Build(),
		Recorder: events.NewFakeRecorder(4),
	}
	backup := &cbv1.Backup{ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-a", Name: "nightly-run"}}
	vol := &cbv1.VolumeStatus{Pvc: "data-vol", Phase: status.VolumePhasePending}

	r.advancePending(context.Background(), backup, vol)
	if vol.Phase != status.VolumePhaseFailed || vol.Reason != "SourcePVCMissing" {
		t.Errorf("phase/reason = %q/%q, want Failed/SourcePVCMissing: a deleted PVC is a decided case and "+
			"must not be parked for an hour first", vol.Phase, vol.Reason)
	}
}

// ---------------------------------------------------------------------------
// The same three properties, driven through the real controller.
// ---------------------------------------------------------------------------

var _ = Describe("BackupReconciler unresolvable exposer", func() {

	// THE spec of this lot. Everything else here is a detail of the mechanism; this is the promise:
	// a volume nobody can resolve does not take its namespace's other volumes down with it.
	//
	// The layout is the incident's, not a convenience: the broken PVC sorts FIRST
	// (ensureVolumes sorts matched names), so it occupies the head of the queue exactly as the
	// mis-referenced one did. Under the old code the two healthy volumes behind it were never
	// attempted at all — no exposure, no mover Job, no reason recorded, nothing to alert on.
	//
	// Mutations that must turn this spec red: making firstNonTerminalVolume ignore parking (the mover
	// Jobs are never created), and making advancePending return an error again (the reconcile errors
	// before writeStatus, so not even the enumeration is persisted).
	It("keeps the rest of the namespace moving when one volume's exposer cannot be resolved", func() {
		const (
			location  = "bk-park"
			ns        = "bk-park-ns"
			run       = "bk-park-run"
			brokenPVC = "vol-a-broken" // sorted first: the head of the queue, as in the incident
			okPVC1    = "vol-b-ok"
			okPVC2    = "vol-c-ok"
		)
		seedInitializedRepo(location, "kek-bk-park", "s3-bk-park")
		createTenantNamespace(ns)
		createSourcePVC(ns, brokenPVC, stubUnresolvableStorageClass)
		createSourcePVC(ns, okPVC1, "ceph-block")
		createSourcePVC(ns, okPVC2, "ceph-block")
		createVolumeOnlyParent(run, location, cbv1.PVCSelector{})
		createChildBackup(ns, run, location)

		By("the broken volume records the cause, stays Pending, and starts its own clock")
		Eventually(func(g Gomega) {
			vol := volumeByPVC(getBackupG(g, ns, run), brokenPVC)
			g.Expect(vol).NotTo(BeNil())
			g.Expect(string(vol.Phase)).To(Equal("Pending"),
				"an unresolvable exposer must not be judged permanent on the first pass — a NotFound "+
					"StorageClass can be created a minute later, and a cached client answers NotFound "+
					"for objects that exist")
			g.Expect(vol.Reason).To(HavePrefix(backupReasonExposerUnresolvable+":"),
				"a parked volume with no recorded cause is invisible: it is neither progressing nor "+
					"alarming, which is precisely how thirty hours went by")
			g.Expect(vol.Reason).To(ContainSubstring("storageclasses.storage.k8s.io"),
				"the reason must carry the resolver's own words, not just our verdict")
			g.Expect(vol.FirstAttemptAt).NotTo(BeNil(),
				"the Pending clock must start on the first ATTEMPT, or the deadline can never fire")
		}, initTimeout, initPoll).Should(Succeed())

		// The load-bearing observation. These two Jobs existing at all is the whole fix: they are the
		// backups that the old code silently did not take.
		//
		// They are driven ONE AT A TIME, and that is the controller's shape rather than the spec's
		// convenience: Reconcile advances exactly one volume per pass, so the second healthy volume is
		// not even looked at until the first has reached a terminal phase (intra-Backup parallelism is
		// deferred). Which is precisely why the parked volume was so expensive: a queue that only ever
		// works on its head cannot afford a head that never finishes.
		By("the two volumes queued BEHIND it are exposed and get their mover Jobs anyway")
		job1 := waitForMoverJob(ns, run, okPVC1)
		simulateMoverSucceeded(job1, "node-a", mover.MoverResult{
			OK: true, Operation: string(mover.OpBackup), SnapshotID: "snap-park-b", SizeBytes: 20, AddedBytes: 10,
		})
		job2 := waitForMoverJob(ns, run, okPVC2)
		simulateMoverSucceeded(job2, "node-b", mover.MoverResult{
			OK: true, Operation: string(mover.OpBackup), SnapshotID: "snap-park-c", SizeBytes: 30, AddedBytes: 15,
		})

		By("and they run all the way to Completed while the broken one is still being retried")
		Eventually(func(g Gomega) {
			b := getBackupG(g, ns, run)
			g.Expect(string(volumeByPVC(b, okPVC1).Phase)).To(Equal("Completed"))
			g.Expect(string(volumeByPVC(b, okPVC2).Phase)).To(Equal("Completed"))
		}, initTimeout, initPoll).Should(Succeed())

		// The parked volume is deferred, not abandoned: with the queue drained it is chosen again on
		// every pass, keeps its recorded cause, and keeps accumulating time toward the (production,
		// one-hour) deadline this spec deliberately does not shorten.
		By("and the parked volume is still Pending with its cause, retried rather than dropped")
		Consistently(func(g Gomega) {
			vol := volumeByPVC(getBackupG(g, ns, run), brokenPVC)
			g.Expect(vol).NotTo(BeNil())
			g.Expect(string(vol.Phase)).To(Equal("Pending"))
			g.Expect(vol.Reason).To(HavePrefix(backupReasonExposerUnresolvable + ":"))
		}, 3*time.Second, 500*time.Millisecond).Should(Succeed())
	})

	// The deadline, end to end, and the honesty of what it writes. Two things are being asserted at
	// once and both matter: the run REACHES A TERMINAL PHASE (without which its ClusterBackup never
	// does either, and a Forbid schedule keeps skipping — the actual cost of the incident), and the
	// reason an operator reads still names the fault rather than the clock.
	//
	// Mutation that must turn this spec red: having the deadline branch overwrite the reason
	// unconditionally (the reason becomes PendingDeadlineExceeded and the ContainSubstring fails), or
	// removing the deadline branch (the volume stays Pending forever and the Backup never terminates).
	It("fails a volume that never resolves, keeping the cause rather than naming the clock", func() {
		const (
			location = "bk-pdl"
			ns       = "bk-pdl-ns"
			run      = "bk-pdl-run"
			pvcName  = "data-vol"
		)
		seedInitializedRepo(location, "kek-bk-pdl", "s3-bk-pdl")
		createTenantNamespace(ns)
		createSourcePVC(ns, pvcName, stubUnresolvableStorageClass)
		createVolumeOnlyParent(run, location, cbv1.PVCSelector{})
		createChildBackup(ns, run, location)

		By("When reconciles run with the Pending bound shortened to a millisecond")
		r := pendingDeadlineReconciler(time.Millisecond)
		req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: run}}
		Eventually(func(g Gomega) {
			_, err := r.Reconcile(ctx, req)
			// A conflict is expected and uninteresting: the suite's registered reconciler drives the
			// same object on its own poll (with the production one-hour bound, so it can never be the
			// one that fails this volume). The retry loop IS the assertion.
			if err != nil && !apierrors.IsConflict(err) {
				g.Expect(err).NotTo(HaveOccurred())
			}
			vol := volumeByPVC(getBackupG(g, ns, run), pvcName)
			g.Expect(vol).NotTo(BeNil())
			g.Expect(string(vol.Phase)).To(Equal("Failed"),
				"a volume that can never leave Pending must eventually be failed, or its run stays "+
					"non-terminal and its schedule's Forbid policy skips every following night")
		}, initTimeout, initPoll).Should(Succeed())

		By("Then the reason still names the fault, not the deadline")
		vol := volumeByPVC(getBackupG(Default, ns, run), pvcName)
		Expect(vol.Reason).To(HavePrefix(backupReasonExposerUnresolvable+":"),
			"the recorded cause must survive the deadline: it names the PVC's missing StorageClass, "+
				"whereas the deadline's own name only says that we gave up")
		Expect(vol.Reason).To(ContainSubstring(stubUnresolvableStorageClass))
		Expect(vol.Reason).NotTo(Equal(backupReasonPendingDeadline))
		// Bounded, because a status field is not a log — the Event carries the long form.
		Expect(len(vol.Reason)).To(BeNumerically("<=", clusterBackupMessageCap))

		By("And the run itself reaches a terminal phase, which is what unblocks the schedule")
		Eventually(func(g Gomega) {
			b := getBackupG(g, ns, run)
			g.Expect(isTerminalBackupPhase(b.Status.Phase)).To(BeTrue(),
				"phase=%q — a run held non-terminal by one unresolvable volume is what made a Forbid "+
					"schedule skip thirty-one hours of nightlies", b.Status.Phase)
			g.Expect(b.Status.CompletionTime).NotTo(BeNil())
		}, initTimeout, initPoll).Should(Succeed())

		By("And a Warning Event says which PVC was given up on, and why")
		Eventually(func(g Gomega) {
			g.Expect(backupWarningNotes(g, ns, run)).To(ContainElement(And(
				ContainSubstring(pvcName),
				ContainSubstring("never left Pending"),
				ContainSubstring(stubUnresolvableStorageClass),
			)))
		}, initTimeout, initPoll).Should(Succeed())
	})

	// firstAttemptAt measures ATTEMPTS, not enumeration, and this is the spec that proves the
	// difference is real rather than a comment. Volumes are advanced one per reconcile, so a volume
	// can be enumerated and then wait its turn for a long time with nothing tried on it; a clock
	// started at enumeration would be running the whole while, and the very first pass that finally
	// reached that volume would find it already past the deadline and fail it — a backup destroyed
	// for the crime of being second in a queue. That regression would be invisible in production
	// (a volume failed with PendingDeadlineExceeded looks exactly like a volume that was tried and
	// could not be resolved) which is why it is pinned here.
	//
	// The queue is held at the head by a STALLED exposure — a volume legitimately in flight, not a
	// parked one, so the parked-preference does not re-order around it and the second volume really
	// does wait. That doubles as the anti-regression for the preference itself: it must skip broken
	// volumes, not reshuffle working ones.
	//
	// Mutation that must turn this spec red: stamping FirstAttemptAt in ensureVolumes.
	It("does not start a queued volume's Pending clock before anything has been tried on it", func() {
		const (
			location  = "bk-queued"
			ns        = "bk-queued-ns"
			run       = "bk-queued-run"
			headPVC   = "vol-a-head"   // sorted first: takes the head and never lets go
			queuedPVC = "vol-b-queued" // never reached, and must not be aging
		)
		// Never ready, and started NOW — well inside snapshotProgressDeadline, so the head volume sits
		// in Snapshotting for the whole spec instead of being failed by a different deadline.
		backupExposers.stallSnapshot(ns+"/"+headPVC, time.Now())

		seedInitializedRepo(location, "kek-bk-queued", "s3-bk-queued")
		createTenantNamespace(ns)
		createSourcePVC(ns, headPVC, "ceph-block")
		createSourcePVC(ns, queuedPVC, "ceph-block")
		createVolumeOnlyParent(run, location, cbv1.PVCSelector{})
		createChildBackup(ns, run, location)

		By("the head volume is attempted — it is Snapshotting and its clock is stamped")
		Eventually(func(g Gomega) {
			vol := volumeByPVC(getBackupG(g, ns, run), headPVC)
			g.Expect(vol).NotTo(BeNil())
			g.Expect(string(vol.Phase)).To(Equal("Snapshotting"))
			g.Expect(vol.FirstAttemptAt).NotTo(BeNil(),
				"the volume that WAS tried must have a clock, or the deadline is inert")
		}, initTimeout, initPoll).Should(Succeed())

		By("while the volume queued behind it is enumerated with no clock running at all")
		Consistently(func(g Gomega) {
			vol := volumeByPVC(getBackupG(g, ns, run), queuedPVC)
			g.Expect(vol).NotTo(BeNil(), "the queued volume must still be enumerated — the fix is about "+
				"WHEN its clock starts, not about hiding it")
			g.Expect(string(vol.Phase)).To(Equal("Pending"))
			g.Expect(vol.Reason).To(BeEmpty())
			g.Expect(vol.FirstAttemptAt).To(BeNil(),
				"a volume nothing has been tried on must not be aging toward a deadline that would "+
					"fail it on the first pass that ever looked at it")
		}, 3*time.Second, 500*time.Millisecond).Should(Succeed())
	})

	// ---------------------------------------------------------------------------
	// THE PATH THAT SHIPPED BROKEN. The first version of this lot moved only the exposer-RESOLUTION
	// failure onto the parking door and left Expose, the pre-check's transient branch and the
	// unreadable-PVC read still returning errors — so the incident was reproducible in full, through a
	// different door, in the release that was supposed to have fixed it.
	//
	// Expose is the worst of the three, for two independent reasons:
	//
	//   - It is the one with realistic durable causes. Resolution fails when a StorageClass is missing,
	//     which is rare; Expose fails when a ResourceQuota will not admit the temp clone PVC, when a
	//     validating webhook refuses VolumeSnapshot objects, when the snapshot CRDs are absent — all
	//     cluster-wide states that persist for as long as nobody notices, and all of which fail EVERY
	//     volume in the cluster rather than one.
	//   - The damage is doubled. Reconcile returns the error before writeStatus, so nothing the pass
	//     wrote reaches etcd — including firstAttemptAt. The volume is re-picked forever with a clock
	//     that resets to nil on every pass, so pendingResolveDeadline, the mechanism that exists
	//     precisely to end this, can NEVER fire. Not delayed: disabled.
	//
	// The persistence assertion below is therefore not a detail. It reads firstAttemptAt back through
	// k8sClient — the direct, UNCACHED envtest connection — which is the only way a spec can say "this
	// reached the apiserver" rather than "the controller set a field in memory".
	// ---------------------------------------------------------------------------
	It("keeps the namespace moving and still fails the volume when Expose fails durably", func() {
		const (
			location  = "bk-expfail"
			ns        = "bk-expfail-ns"
			run       = "bk-expfail-run"
			brokenPVC = "vol-a-broken" // sorted first: the head of the queue
			okPVC     = "vol-b-ok"
		)
		// Armed BEFORE the Backup exists, so the very first pass hits it, and DURABLE: the same error on
		// every call, which is what a quota or a webhook actually does.
		backupExposers.failExpose(ns+"/"+brokenPVC, apierrors.NewForbidden(
			schema.GroupResource{Resource: "persistentvolumeclaims"}, "vol-a-broken-clone",
			errors.New(`exceeded quota: tenant-quota, requested: requests.storage=1Gi, used: requests.storage=10Gi, limited: requests.storage=10Gi`)))

		seedInitializedRepo(location, "kek-bk-expfail", "s3-bk-expfail")
		createTenantNamespace(ns)
		createSourcePVC(ns, brokenPVC, "ceph-block")
		createSourcePVC(ns, okPVC, "ceph-block")
		createVolumeOnlyParent(run, location, cbv1.PVCSelector{})
		createChildBackup(ns, run, location)

		By("the volume whose exposure cannot be created is parked, not errored")
		Eventually(func(g Gomega) {
			vol := volumeByPVC(getBackupG(g, ns, run), brokenPVC)
			g.Expect(vol).NotTo(BeNil())
			g.Expect(string(vol.Phase)).To(Equal("Pending"))
			g.Expect(vol.Reason).To(HavePrefix(backupReasonExposerUnresolvable+":"),
				"a failure to CREATE the exposure must take the same door as a failure to resolve one: "+
					"the namespace queued behind it cannot tell the difference, and neither can the operator")
			g.Expect(vol.Reason).To(ContainSubstring("exceeded quota"),
				"the reason must carry the apiserver's own words — an operator reading this must learn "+
					"that a quota refused the clone, not merely that we could not expose")
		}, initTimeout, initPoll).Should(Succeed())

		// THE assertion the shipped version failed. An errored pass never reaches writeStatus, so this
		// field would be absent from the stored object no matter how many times the volume was tried.
		By("and its firstAttemptAt is PERSISTED, so the deadline is armed rather than merely intended")
		Eventually(func(g Gomega) {
			vol := volumeByPVC(getBackupG(g, ns, run), brokenPVC)
			g.Expect(vol).NotTo(BeNil())
			g.Expect(vol.FirstAttemptAt).NotTo(BeNil(),
				"the clock never reached etcd: every pass re-stamped it in memory and threw it away with "+
					"the error, which is how the deadline meant to rescue this run was silently disabled")
		}, initTimeout, initPoll).Should(Succeed())

		By("and Expose really was attempted, rather than the volume being parked without trying")
		Expect(backupExposers.exposeCalls()).To(ContainElement(ns + "/" + brokenPVC))

		By("and a failed Expose leaves no residue: no clone PVC for the volume that never got one")
		Consistently(func(g Gomega) {
			err := k8sClient.Get(ctx, client.ObjectKey{
				Namespace: suiteOperatorNamespace, Name: tempCloneNameFor(ns, run, brokenPVC),
			}, &corev1.PersistentVolumeClaim{})
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "residual clone PVC: %v", err)
		}, 2*time.Second, 250*time.Millisecond).Should(Succeed())

		By("while the volume queued behind it is backed up all the way to Completed")
		jobName := waitForMoverJob(ns, run, okPVC)
		simulateMoverSucceeded(jobName, "node-a", mover.MoverResult{
			OK: true, Operation: string(mover.OpBackup), SnapshotID: "snap-expfail-b", SizeBytes: 40, AddedBytes: 20,
		})
		Eventually(func(g Gomega) {
			g.Expect(string(volumeByPVC(getBackupG(g, ns, run), okPVC).Phase)).To(Equal("Completed"))
		}, initTimeout, initPoll).Should(Succeed())

		// ONLY NOW, and the ordering is the assertion. A parked volume is deferred behind every
		// non-terminal volume, so while the healthy one was in flight the broken one was not retried at
		// all — measured: exactly one Expose call for the whole of that stretch. This is why
		// pendingResolveDeadline is a LOWER bound and not an upper one: its clock advances only on passes
		// that actually pick the volume, so time-to-failure is the bound PLUS however long the rest of
		// the namespace takes to go terminal. With the queue drained, the parked volume is picked on every
		// poll again, which is what "deferred, not dropped" has to mean if the deadline is ever to fire.
		By("and with the queue drained the parked volume is retried again, not abandoned")
		Eventually(func(g Gomega) {
			var attempts int
			for _, call := range backupExposers.exposeCalls() {
				if call == ns+"/"+brokenPVC {
					attempts++
				}
			}
			g.Expect(attempts).To(BeNumerically(">=", 2),
				"one attempt and then silence would mean the volume was quietly dropped: nothing would "+
					"ever advance its Pending clock, and the deadline could not fire")
		}, initTimeout, initPoll).Should(Succeed())

		// And now the other half of the guarantee, on the clock that the persisted stamp made reachable:
		// the volume is failed, the cause is kept, and the run can finally go terminal.
		By("and past the Pending deadline the volume Fails, keeping the quota as its cause")
		r := pendingDeadlineReconciler(time.Millisecond)
		req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: run}}
		Eventually(func(g Gomega) {
			_, err := r.Reconcile(ctx, req)
			if err != nil && !apierrors.IsConflict(err) {
				g.Expect(err).NotTo(HaveOccurred())
			}
			vol := volumeByPVC(getBackupG(g, ns, run), brokenPVC)
			g.Expect(vol).NotTo(BeNil())
			g.Expect(string(vol.Phase)).To(Equal("Failed"))
			g.Expect(vol.Reason).To(ContainSubstring("exceeded quota"),
				"reason=%q — the deadline must not replace the diagnosis with its own name", vol.Reason)
		}, initTimeout, initPoll).Should(Succeed())

		By("and the run reaches a terminal phase: one Completed, one Failed => PartiallyFailed")
		Eventually(func(g Gomega) {
			b := getBackupG(g, ns, run)
			g.Expect(isTerminalBackupPhase(b.Status.Phase)).To(BeTrue(),
				"phase=%q — the run must terminate, or its ClusterBackup does not either and a Forbid "+
					"schedule skips every following night", b.Status.Phase)
		}, initTimeout, initPoll).Should(Succeed())
	})

	// The pre-check's OTHER branch, and the distinction is the point. A refusal that wraps
	// exposer.ErrPrecheckFailed is a VERDICT — a cluster somebody broke, somebody can fix, and the
	// volume is Failed on the first pass (that spec lives in backup_controller_test.go). An error that
	// does NOT wrap it is not a verdict at all: it is a check that could not be carried out, and
	// today's Precheck cannot produce one. The branch exists so that a FUTURE check whose failure is
	// transient does not silently inherit "fail this volume" — and it must not inherit "error the
	// reconcile and block the namespace" either, which is what it did until the second pass at this fix.
	//
	// Reachable through the EXISTING seam: failPrecheck takes an arbitrary error, so a spec can simply
	// arm one that does not wrap the sentinel.
	It("parks, rather than fails or errors, when the pre-check itself cannot be carried out", func() {
		const (
			location  = "bk-prechk-err"
			ns        = "bk-prechk-err-ns"
			run       = "bk-prechk-err-run"
			brokenPVC = "vol-a-broken"
			okPVC     = "vol-b-ok"
		)
		// NOT wrapping exposer.ErrPrecheckFailed: the check could not be performed, which is a different
		// statement from "the check says no".
		backupExposers.failPrecheck(ns+"/"+brokenPVC,
			errors.New("stub: reading VolumeSnapshotClass: connection refused"))

		seedInitializedRepo(location, "kek-bk-prechk-err", "s3-bk-prechk-err")
		createTenantNamespace(ns)
		createSourcePVC(ns, brokenPVC, "ceph-block")
		createSourcePVC(ns, okPVC, "ceph-block")
		createVolumeOnlyParent(run, location, cbv1.PVCSelector{})
		createChildBackup(ns, run, location)

		By("the volume is parked with the check's own words, NOT failed as if the check had refused it")
		Eventually(func(g Gomega) {
			vol := volumeByPVC(getBackupG(g, ns, run), brokenPVC)
			g.Expect(vol).NotTo(BeNil())
			g.Expect(string(vol.Phase)).To(Equal("Pending"))
			g.Expect(vol.Reason).To(HavePrefix(backupReasonExposerUnresolvable + ":"))
			g.Expect(vol.Reason).To(ContainSubstring("connection refused"))
			g.Expect(vol.Reason).NotTo(Equal(backupReasonPrecheckFailed),
				"a check that could not RUN must not be reported as a check that REFUSED: the first is "+
					"retryable and says nothing about the volume, the second is a verdict about the cluster")
			g.Expect(vol.FirstAttemptAt).NotTo(BeNil(),
				"persisted, or the deadline can never end this volume's Pending")
		}, initTimeout, initPoll).Should(Succeed())

		By("and nothing was exposed for it — the pre-check still runs strictly before Expose")
		Expect(backupExposers.exposeCalls()).NotTo(ContainElement(ns+"/"+brokenPVC),
			"parking must not have moved the pre-check after Expose: that would leak a VolumeSnapshot "+
				"on every retry, once per poll interval, for as long as the check keeps failing")

		By("while the volume behind it completes normally")
		jobName := waitForMoverJob(ns, run, okPVC)
		simulateMoverSucceeded(jobName, "node-b", mover.MoverResult{
			OK: true, Operation: string(mover.OpBackup), SnapshotID: "snap-prechk-b", SizeBytes: 50, AddedBytes: 25,
		})
		Eventually(func(g Gomega) {
			g.Expect(string(volumeByPVC(getBackupG(g, ns, run), okPVC).Phase)).To(Equal("Completed"))
		}, initTimeout, initPoll).Should(Succeed())
	})
})
