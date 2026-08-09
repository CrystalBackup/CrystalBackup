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
	"sync"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2" //nolint:revive,staticcheck
	. "github.com/onsi/gomega"    //nolint:revive,staticcheck

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
	"github.com/CrystalBackup/CrystalBackup/internal/hooks"
	"github.com/CrystalBackup/CrystalBackup/internal/mover"
	"github.com/CrystalBackup/CrystalBackup/internal/status"
)

// ---------------------------------------------------------------------------
// THE LAST THREE INSTANCES OF THE ERRORED-PASS CLASS.
//
// The class, in one sentence: Reconcile advances ONE volume per pass, and a step that returns an
// error returns it BEFORE the single status write at step (11) — so nothing that pass computed is
// persisted, the same volume is re-picked forever (firstNonTerminalVolume reads the stored status and
// the stored status never changed), every other volume of the namespace is unreachable, and every
// per-phase deadline meant to end the hang is unreachable with them. On a real cluster it cost
// thirty-one hours of no backups at all, over one PVC naming a StorageClass that did not exist.
//
// backup_pending_test.go closed the Pending half. backup_snapshotting_stall_test.go closed the volume
// half of Snapshotting and Uploading. Three instances survived both, and this file closes them:
//
//  1. STEP (9b), openFreezeWindow. The worst: a hooks-resolution failure errors on EVERY pass, so
//     status.volumes never reaches etcd AT ALL — not a phase, not a cause, not even the enumeration —
//     and the run never terminates. It is now routed into failHooks, the mechanism that already knows
//     how to record a failed quiesce and decide its consequence, which keeps R16's distinction intact:
//     a quiesce that could not be performed never becomes a backup that claims consistency.
//  2. STEPS (10b) AND (10c), the manifest half and the freeze-window release. Both sat between step
//     (10) and the status write, so either one's bad day discarded everything the volume half had just
//     computed — the exact coupling (10b)'s own doc says must not exist. They now PERSIST FIRST AND
//     PROPAGATE AFTER: the shape that was rejected for a volume error, and the right one here, because
//     the failure is not per-volume so the backoff is charged to the object at fault.
//  3. THE SITES THAT PERSIST BUT REMAIN UNBOUNDED. Since the previous lot each of them records its
//     cause and survives the pass — but a Snapshotting or Uploading volume is NOT deferred by
//     firstNonTerminalVolume (correctly: it holds work in flight), so it keeps the head of its
//     namespace's queue with no clock that can ever end it. status.volumes[].phaseEnteredAt plus
//     advanceRetryDeadline is the outer bound, and it is a BACKSTOP: the snapshot- and Job-derived
//     deadlines are strictly better and always win.
//
// The most important spec in the file is the LAST one, and it tests none of the above. It tests that
// the backstop does NOT fire on a mover whose pod is running, because that is the failure mode this
// change could have: a wall-clock cap on backup duration, destroying the largest volumes in the fleet
// first, while looking like a working feature.
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// The fault-injection seam.
//
// Two of the three instances are provoked by an apiserver call being REFUSED — a pods-list a narrowed
// ClusterRole would refuse, a Job a validating webhook would refuse — and neither is reachable through
// the exposer stub. So the Backup reconciler's client carries one more wrapper, disarmed by default
// and keyed PER TENANT NAMESPACE, on the same convention every other verdict in this suite uses: one
// shared manager reconciles every Backup here, so a global switch would fail its neighbours.
//
// It is installed on the REGISTERED reconciler (suite_test.go) rather than on a shallow copy, and that
// is not a convenience. A copy alone seeing the refusal would race the registered reconciler for the
// same object: whichever got there first would decide, and for a pre-hook spec the registered one
// would resolve the hooks successfully and the spec would silently be testing the happy path.
// ---------------------------------------------------------------------------

// backupFaults holds the armed refusals. Per-namespace maps, mutex-guarded because the manager
// reconciles on its own goroutine while specs arm and read them.
type backupFaults struct {
	mu sync.Mutex
	// podLists refuses `list pods` in these namespaces — the shape of an operator whose pods RBAC was
	// narrowed under a running run, which is what makes resolveHooks fail durably.
	podLists map[string]error
	// manifestJobs refuses the CREATE of a namespace's manifest mover Job — the shape of a validating
	// webhook or a narrowed batch RBAC. Keyed off the Job's own crystalbackup.io/namespace label
	// rather than off its name, so the refusal cannot drift with the name-sanitising rules.
	manifestJobs map[string]error
	// manifestJobRefusals counts how many times each namespace's manifest Job create was refused. It
	// is how a spec proves the manifest half is STILL BEING RETRIED after the pass stopped returning
	// its error — the half of "persist first, then propagate" that a status assertion cannot see.
	manifestJobRefusals map[string]int
}

// refusePodList arms (err != nil) or disarms (err == nil) the pods-list refusal for one namespace.
func (f *backupFaults) refusePodList(namespace string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.podLists == nil {
		f.podLists = map[string]error{}
	}
	if err == nil {
		delete(f.podLists, namespace)
		return
	}
	f.podLists[namespace] = err
}

func (f *backupFaults) podListVerdict(namespace string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.podLists[namespace]
}

// refuseManifestJob arms a durable manifest-mover-Job create refusal for one namespace.
func (f *backupFaults) refuseManifestJob(namespace string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.manifestJobs == nil {
		f.manifestJobs = map[string]error{}
	}
	f.manifestJobs[namespace] = err
}

func (f *backupFaults) manifestJobVerdict(namespace string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.manifestJobs[namespace]; err != nil {
		if f.manifestJobRefusals == nil {
			f.manifestJobRefusals = map[string]int{}
		}
		f.manifestJobRefusals[namespace]++
		return err
	}
	return nil
}

func (f *backupFaults) manifestJobRefusalCount(namespace string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.manifestJobRefusals[namespace]
}

// faultInjectingClient overrides ONLY List (for pods) and Create (for manifest mover Jobs) on the
// embedded client. Everything else, including every read the specs themselves perform through
// k8sClient, is untouched: the refusals have to be narrow enough that a spec's own setup and its
// assertions are never affected by them.
type faultInjectingClient struct {
	client.Client
	faults *backupFaults
}

func (c *faultInjectingClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if _, isPods := list.(*corev1.PodList); isPods {
		var lo client.ListOptions
		lo.ApplyOptions(opts)
		if err := c.faults.podListVerdict(lo.Namespace); err != nil {
			return err
		}
	}
	return c.Client.List(ctx, list, opts...)
}

func (c *faultInjectingClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if job, isJob := obj.(*batchv1.Job); isJob &&
		job.Labels[apiconst.LabelMoverRole] == apiconst.MoverRoleManifest {
		if err := c.faults.manifestJobVerdict(job.Labels[apiconst.LabelNamespace]); err != nil {
			return err
		}
	}
	return c.Client.Create(ctx, obj, opts...)
}

// forbiddenPodList is the error the pods-list refusal answers with, and the choice is an assertion.
//
// It is a Forbidden carrying the API server's own sentence, because the cause this stands in for is a
// ClusterRole narrowed under a running operator — durable, cluster-wide, and not a fact about any
// volume's data. An operator reading `kubectl describe backup` has to meet that sentence, not our
// paraphrase of it: a message that says only "hooks failed" leaves them exactly as blind as thirty
// hours of silence did.
func forbiddenPodList(namespace string) error {
	return apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "",
		errors.New(`User "system:serviceaccount:crystalbackup-system:crystal-backup-controller-manager" `+
			`cannot list resource "pods" in API group "" in the namespace "`+namespace+`"`))
}

// advanceBackstopReconciler is a shallow copy of the registered Backup reconciler with a shortened
// OUTER bound — the same device (and the same shallow-on-purpose sharing of client, recorder, exposer
// registry and queue) as deadlineReconciler and pendingDeadlineReconciler, which see for why a copy
// rather than a mutation of the registered object.
//
// The clock this shortens is status.volumes[].phaseEnteredAt, a field the controller writes itself, so
// a spec could backdate it instead. Shortening the bound is still the better instrument for the reason
// pendingDeadlineReconciler gives: it makes the controller stamp its own clock and then honour it,
// which is the behaviour under test, rather than asserting on a timestamp the test wrote.
func advanceBackstopReconciler(bound time.Duration) *BackupReconciler {
	GinkgoHelper()
	Expect(backupReconciler).NotTo(BeNil(), "the suite's Backup reconciler was never wired")
	copied := *backupReconciler
	copied.AdvanceRetryDeadline = bound
	return &copied
}

// reconcileTolerantOfConflicts drives one manual reconcile, ignoring the Conflict the suite's
// registered reconciler causes by driving the same object on its own five-second poll. Every spec
// below that uses it does so inside an Eventually/Consistently, so the retry IS the assertion loop.
func reconcileTolerantOfConflicts(g Gomega, r *BackupReconciler, namespace, name string) {
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: namespace, Name: name}})
	if err != nil && !apierrors.IsConflict(err) {
		g.Expect(err).NotTo(HaveOccurred())
	}
}

// ---------------------------------------------------------------------------
// The ladder, as a pure statement about the constants.
// ---------------------------------------------------------------------------

// TestAdvanceRetryDeadlineIsTheOutermostBound is the guard that keeps the backstop a backstop.
//
// Its whole safety argument is that a volume whose own object can be READ is always judged by the
// bound that hangs off that object — 15 minutes for a snapshot nobody picked up, two hours for one
// that was picked up and never finished, thirty minutes for a mover pod that never ran, an hour for a
// volume that never left Pending — and that this weaker verdict is reached only when there was no
// better one to be had. That argument is false the moment any of those numbers passes this one, and
// the failure would be silent: the run would report "we could not tell" for a volume the cluster had
// told us about in detail, sending an operator to the wrong component.
func TestAdvanceRetryDeadlineIsTheOutermostBound(t *testing.T) {
	ladder := []struct {
		name  string
		bound time.Duration
	}{
		{"snapshotProgressDeadline", snapshotProgressDeadline},
		{"snapshotReadyDeadline", snapshotReadyDeadline},
		{"moverStartDeadline", moverStartDeadline},
		{"pendingResolveDeadline", pendingResolveDeadline},
	}
	for _, rung := range ladder {
		if rung.bound >= advanceRetryDeadline {
			t.Errorf("%s (%v) must stay strictly under advanceRetryDeadline (%v): the backstop exists "+
				"only for volumes no per-object bound can judge, and a rung that reaches it would let the "+
				"weakest diagnosis win a race it should always lose", rung.name, rung.bound, advanceRetryDeadline)
		}
	}
	// A factor of two over the longest rung, so the two cannot race on a slow reconcile queue. The
	// longest rung's clock starts within seconds of the phase being entered (Expose creates the origin
	// VolumeSnapshot on the pass that sets Snapshotting), which is what makes the comparison fair.
	if advanceRetryDeadline < 2*snapshotReadyDeadline {
		t.Errorf("advanceRetryDeadline (%v) should keep a clear factor of two over snapshotReadyDeadline "+
			"(%v); below that the specific verdict and the backstop start to race",
			advanceRetryDeadline, snapshotReadyDeadline)
	}
	if got := (&BackupReconciler{}).effectiveAdvanceRetryDeadline(); got != advanceRetryDeadline {
		t.Errorf("effectiveAdvanceRetryDeadline with no override = %v, want the constant %v",
			got, advanceRetryDeadline)
	}
	if got := (&BackupReconciler{AdvanceRetryDeadline: time.Second}).effectiveAdvanceRetryDeadline(); got != time.Second {
		t.Errorf("effectiveAdvanceRetryDeadline ignored the injected override: %v", got)
	}
}

// TestStampVolumePhaseEntryHasOneRulePerCase pins the single writer of the backstop's clock.
//
// One writer, at the one call site of the per-PVC state machine, is the point: a stamp inside each
// phase function is the per-call-site shape that produced this release's defect three times over, and
// a phase added later would simply not stamp — leaving the newest, least-exercised path silently
// unbounded. The cases below are the four shapes that exist, and the third and fourth are the ones
// that would go wrong quietly.
func TestStampVolumePhaseEntryHasOneRulePerCase(t *testing.T) {
	old := metav1.NewTime(time.Now().Add(-9 * time.Hour))

	cases := []struct {
		name       string
		entryPhase status.VolumePhase
		vol        cbv1.VolumeStatus
		wantStamp  bool // true = phaseEnteredAt must now be recent
		wantNil    bool
	}{
		{
			name:       "a transition is stamped",
			entryPhase: status.VolumePhasePending,
			vol:        cbv1.VolumeStatus{Phase: status.VolumePhaseSnapshotting},
			wantStamp:  true,
		},
		{
			// Not because a terminal volume needs a clock, but because "stamp on any change of phase" is
			// a rule with no exceptions to get wrong, where "stamp on the bounded phases" is a list that
			// goes stale the moment that set changes.
			name:       "a transition to a terminal phase is stamped too",
			entryPhase: status.VolumePhaseUploading,
			vol:        cbv1.VolumeStatus{Phase: status.VolumePhaseCompleted},
			wantStamp:  true,
		},
		{
			// The clock must NOT restart while the volume sits still, or the bound can never be crossed:
			// the backstop is reached only on passes that failed to advance the volume, which is exactly
			// when this function is called with no phase change.
			name:       "no move with a clock already set leaves it alone",
			entryPhase: status.VolumePhaseSnapshotting,
			vol:        cbv1.VolumeStatus{Phase: status.VolumePhaseSnapshotting, PhaseEnteredAt: &old},
		},
		{
			// The upgrade case: a volume already in flight when the operator started writing this field
			// has no transition left to observe, so without this backfill it would be permanently
			// unbounded. Stamping it now can only make the backstop late, never early.
			name:       "no move and no clock backfills, for a phase the backstop bounds",
			entryPhase: status.VolumePhaseUploading,
			vol:        cbv1.VolumeStatus{Phase: status.VolumePhaseUploading},
			wantStamp:  true,
		},
		{
			// Pending is bounded by firstAttemptAt, whose doc records at length why a clock started at
			// enumeration is the wrong one. A second, differently-meaning timestamp on the same volume
			// would re-open exactly that.
			name:       "no move and no clock does NOT backfill a Pending volume",
			entryPhase: status.VolumePhasePending,
			vol:        cbv1.VolumeStatus{Phase: status.VolumePhasePending},
			wantNil:    true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vol := tc.vol
			stampVolumePhaseEntry(&vol, tc.entryPhase)
			switch {
			case tc.wantNil:
				if vol.PhaseEnteredAt != nil {
					t.Errorf("phaseEnteredAt = %v, want it left absent", vol.PhaseEnteredAt)
				}
			case tc.wantStamp:
				if vol.PhaseEnteredAt == nil {
					t.Fatal("phaseEnteredAt was not stamped: the volume has no clock, so the outer bound " +
						"can never fire on it")
				}
				if time.Since(vol.PhaseEnteredAt.Time) > time.Minute {
					t.Errorf("phaseEnteredAt = %v, want a fresh stamp", vol.PhaseEnteredAt)
				}
			default:
				if vol.PhaseEnteredAt == nil || !vol.PhaseEnteredAt.Time.Equal(old.Time) {
					t.Errorf("phaseEnteredAt = %v, want the original %v preserved", vol.PhaseEnteredAt, old)
				}
			}
		})
	}
}

// TestFailVolumeOnAdvanceBackstopNeedsEvidence states the asymmetry the verdict rests on, which is the
// same one snapshotDeadlineExceeded is built around: this branch terminates somebody's backup, so the
// burden of proof sits entirely on the evidence and never on its absence.
func TestFailVolumeOnAdvanceBackstopNeedsEvidence(t *testing.T) {
	long := strings.Repeat("x", 4096)
	old := metav1.NewTime(time.Now().Add(-9 * time.Hour))
	young := metav1.NewTime(time.Now())

	t.Run("no clock is never a verdict", func(t *testing.T) {
		r := &BackupReconciler{Recorder: events.NewFakeRecorder(4), AdvanceRetryDeadline: time.Nanosecond}
		vol := cbv1.VolumeStatus{Pvc: "v", Phase: status.VolumePhaseUploading}
		if pvc, bounded := r.failVolumeOnAdvanceBackstop(context.Background(), &cbv1.Backup{}, &vol,
			errors.New("boom")); bounded || pvc != "" {
			t.Errorf("an absent phaseEnteredAt produced a verdict (%q, %v): absence of evidence is not "+
				"evidence that a volume has been stuck", pvc, bounded)
		}
		if vol.Phase != status.VolumePhaseUploading {
			t.Errorf("phase = %q, want it untouched", vol.Phase)
		}
	})

	t.Run("inside the bound is never a verdict", func(t *testing.T) {
		r := &BackupReconciler{Recorder: events.NewFakeRecorder(4)}
		vol := cbv1.VolumeStatus{Pvc: "v", Phase: status.VolumePhaseSnapshotting, PhaseEnteredAt: &young}
		if _, bounded := r.failVolumeOnAdvanceBackstop(context.Background(), &cbv1.Backup{}, &vol,
			errors.New("boom")); bounded {
			t.Error("a volume inside the bound was failed")
		}
	})

	t.Run("past the bound fails the volume and carries the cause, capped", func(t *testing.T) {
		r := &BackupReconciler{Recorder: events.NewFakeRecorder(4)}
		vol := cbv1.VolumeStatus{Pvc: "v", Phase: status.VolumePhaseUploading, PhaseEnteredAt: &old}
		pvc, bounded := r.failVolumeOnAdvanceBackstop(context.Background(), &cbv1.Backup{}, &vol,
			errors.New("mover Job crystalbackup-system/x-mover does not exist: "+long))
		if !bounded || pvc != "v" {
			t.Fatalf("failVolumeOnAdvanceBackstop = (%q, %v), want the verdict and the PVC name for teardown",
				pvc, bounded)
		}
		if vol.Phase != status.VolumePhaseFailed {
			t.Errorf("phase = %q, want Failed", vol.Phase)
		}
		if !strings.HasPrefix(vol.Reason, backupReasonAdvanceDeadline+":") {
			t.Errorf("reason = %q, want it to name the deadline that fired", vol.Reason)
		}
		if !strings.Contains(vol.Reason, "does not exist") {
			t.Errorf("reason = %q, want it to carry the observed cause — this deadline's own diagnosis is "+
				"the weakest of the four, so the cause is all the diagnosis there is", vol.Reason)
		}
		// A status field is not a log: the Event carries the long form.
		if len(vol.Reason) > 256 {
			t.Errorf("reason is %d bytes: a status field must never carry an unbounded blob", len(vol.Reason))
		}
	})
}

// ---------------------------------------------------------------------------
// (1) Step (9b): a quiesce that could not even be arranged.
// ---------------------------------------------------------------------------

var _ = Describe("BackupReconciler unresolvable consistency hooks", func() {

	// THE WORST INSTANCE OF THE CLASS, and the one whose signature matched the production incident
	// exactly: nothing at all in status.volumes, forever.
	//
	// The failure injected is the real one named in the audit: the pods list refused, which is what a
	// ClusterRole narrowed under a running operator does to resolveHooks. It errors on every pass, so
	// the old code returned from step (9b) on every pass, upstream of the only status write — and a
	// spec cannot assert "nothing was persisted" more sharply than by asserting the enumeration itself
	// reached etcd, read back through k8sClient, the direct envtest connection with no cache in front
	// of it.
	//
	// Note what makes this spec airtight rather than merely green: with the refusal disarmed, this run
	// resolves ZERO hooks (there is no pod mounting the PVC), advancePreHooks returns ran=false, and
	// the backup proceeds normally to Completed. So every assertion below is caused by the refusal and
	// by nothing else.
	//
	// Mutations that must turn this spec red: restoring `return ctrl.Result{}, true, err` in
	// openFreezeWindow (status.volumes is nil and the phase never becomes terminal — the incident); and
	// making the resolution failure fall through to the snapshots instead of failing (Expose is called,
	// which is a backup claiming application consistency after a quiesce that never happened).
	It("fails the run without snapshotting, and the enumeration still reaches etcd", func() {
		const (
			location = "bk-hkunres"
			ns       = "bk-hkunres-ns"
			run      = "bk-hkunres-run"
			pvcName  = "data-vol"
		)
		// Armed BEFORE the Backup exists, so the very first pass through step (9b) hits it, and durable:
		// the cause it stands in for is a property of the cluster, not of the attempt.
		backupAPIFaults.refusePodList(ns, forbiddenPodList(ns))
		DeferCleanup(func() { backupAPIFaults.refusePodList(ns, nil) })

		seedInitializedRepo(location, "kek-bk-hkunres", "s3-bk-hkunres")
		createTenantNamespace(ns)
		createSourcePVC(ns, pvcName, "ceph-block")
		createHookedParent(run, location, cbv1.PVCSelector{}, cbv1.HooksSpec{
			Pre:  []cbv1.Hook{{Command: []string{"psql", "-c", "CHECKPOINT"}}},
			Post: []cbv1.Hook{{Command: []string{"psql", "-c", "SELECT pg_backup_stop()"}}},
		})
		createChildBackup(ns, run, location)

		By("the run reaches a TERMINAL phase instead of erroring forever")
		Eventually(func(g Gomega) {
			b := getBackupG(g, ns, run)
			g.Expect(b.Status.Phase).To(Equal("Failed"),
				"phase=%q — a run held non-terminal by a step that errors every pass is what made a Forbid "+
					"schedule skip thirty-one hours of nightlies", b.Status.Phase)
			g.Expect(isTerminalBackupPhase(b.Status.Phase)).To(BeTrue())
			g.Expect(b.Status.CompletionTime).NotTo(BeNil(),
				"a Failed run must say WHEN it failed, or the alert window reads the wrong clock")
		}, initTimeout, initPoll).Should(Succeed())

		By("and status.volumes REACHED ETCD — the assertion an errored pass could never satisfy")
		Eventually(func(g Gomega) {
			b := getBackupG(g, ns, run)
			vol := volumeByPVC(b, pvcName)
			g.Expect(vol).NotTo(BeNil(),
				"status.volumes is nil: the pass returned its error before writeStatus, so not even the "+
					"enumeration was persisted and there was nothing per-volume for an administrator to read")
			g.Expect(string(vol.Phase)).To(Or(Equal("Pending"), Equal("")),
				"no volume may leave Pending once the quiesce could not be performed")
		}, initTimeout, initPoll).Should(Succeed())

		By("and the recorded cause is the API server's own sentence, not our paraphrase of it")
		b := getBackupG(Default, ns, run)
		cond := status.FindCondition(b.Status.Conditions, ConditionReady)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Reason).To(Equal("PreHookFailed"),
			"the failure is routed through failHooks, which already means exactly this — a second path "+
				"would have had to reproduce the phase, the clock, the condition, the Event and the metrics")
		Expect(cond.Message).To(ContainSubstring("quiesce could not be performed"))
		Expect(cond.Message).To(ContainSubstring(`cannot list resource "pods"`),
			"an operator must learn that RBAC refused the pod listing, not merely that hooks failed")

		By("and NO snapshot was taken: a quiesce that did not happen never becomes a consistent backup")
		Consistently(func(g Gomega) {
			g.Expect(backupExposers.exposeCalls()).NotTo(ContainElement(ns+"/"+pvcName),
				"a snapshot taken after a quiesce that could not be performed LOOKS application-consistent "+
					"and is not — which is discovered at restore time, when it is far too late (R16, "+
					"01-architecture.md §5)")
			g.Expect(getBackupG(g, ns, run).Status.Hooks).To(BeEmpty(),
				"no hook ran, so status.hooks must claim none: a fabricated entry in the one place an "+
					"operator trusts to be literal is worse than an empty list")
		}, 3*time.Second, 500*time.Millisecond).Should(Succeed())

		By("and a Warning Event says the quiet part out loud")
		Eventually(func(g Gomega) {
			g.Expect(backupWarningNotes(g, ns, run)).To(ContainElement(And(
				ContainSubstring("no snapshot was taken"),
				ContainSubstring(`cannot list resource "pods"`),
			)))
		}, initTimeout, initPoll).Should(Succeed())
	})

	// (2), THE RELEASE HALF — step (10c). The window is already OPEN here, which is what makes this the
	// dangerous one: the data is captured, so failing the run would be wrong, and retrying forever is
	// the class. Both used to happen at once — the error was returned upstream of the status write, so
	// the run never terminated AND every pass discarded the volume half's progress.
	//
	// It is now routed into the vocabulary that already exists for a release that does not work: the
	// attempt is COUNTED against postHookMaxAttempts (so the retrying is bounded, which is what makes
	// propagating the error safe), each attempt emits a Warning, and the last one emits the loud
	// "an application may remain quiesced" that only a human can act on.
	//
	// Mutations that must turn this spec red: restoring `return ctrl.Result{}, err` at step (10c) (the
	// volume never reaches Uploading in etcd, and the run never completes); and dropping the attempt
	// count from the resolution failure (postHookAttempts stays 0, no loud Event ever fires, and the
	// release is retried for as long as the cluster stays broken).
	It("bounds a release it cannot even attempt, and keeps the volume half's progress", func() {
		const (
			location = "bk-hkrel"
			ns       = "bk-hkrel-ns"
			run      = "bk-hkrel-run"
			pvcName  = "data-vol"
		)
		hookExecutor.reset()
		// STALLED FROM THE START, and this is the mechanism rather than a convenience. The pre phase must
		// SUCCEED for this spec to be about the release at all — arming the refusal first would produce
		// the previous spec instead — and a healthy volume goes Pending → Uploading inside a second, so
		// the pre hooks and the post hooks both fire before a spec can get a word in (they did: the first
		// draft of this spec armed the refusal a second too late and watched the release succeed). Holding
		// the volume in Snapshotting keeps snapshotsCut false, which keeps the release from firing at all,
		// until the spec lets go.
		backupExposers.stallSnapshot(ns+"/"+pvcName, time.Now())

		seedInitializedRepo(location, "kek-bk-hkrel", "s3-bk-hkrel")
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
			b := getBackupG(g, ns, run)
			g.Expect(hookPhaseCount(b, hooks.PhasePre)).To(Equal(1))
			g.Expect(string(volumeByPVC(b, pvcName).Phase)).To(Equal("Snapshotting"))
			g.Expect(hookPhaseCount(b, hooks.PhasePost)).To(Equal(0),
				"the release must not have fired yet, or this spec is not testing what it claims")
		}, initTimeout, initPoll).Should(Succeed())

		By("when the pod listing is then refused, so the release cannot even be resolved")
		backupAPIFaults.refusePodList(ns, forbiddenPodList(ns))
		DeferCleanup(func() { backupAPIFaults.refusePodList(ns, nil) })
		backupExposers.unstallSnapshot(ns + "/" + pvcName)

		By("the volume half still advances and is PERSISTED, pass by pass")
		Eventually(func(g Gomega) {
			vol := volumeByPVC(getBackupG(g, ns, run), pvcName)
			g.Expect(vol).NotTo(BeNil())
			g.Expect(string(vol.Phase)).To(Equal("Uploading"),
				"phase=%q — the release fires once the snapshots are CUT, so its error used to be returned "+
					"on the very pass that moved this volume, and everything that pass computed was thrown "+
					"away with it", vol.Phase)
		}, initTimeout, initPoll).Should(Succeed())

		By("and the release is retried a BOUNDED number of times, then says so loudly")
		Eventually(func(g Gomega) {
			b := getBackupG(g, ns, run)
			g.Expect(b.Status.PostHookAttempts).To(BeNumerically(">=", int32(postHookMaxAttempts)),
				"postHookAttempts=%d — a release that cannot be resolved must spend the same budget as one "+
					"that runs and fails, or it is not bounded at all", b.Status.PostHookAttempts)
			g.Expect(hookPhaseCount(b, hooks.PhasePost)).To(Equal(0),
				"no command ran, so status.hooks must not claim a post entry for a pod nothing reached")
		}, initTimeout, initPoll).Should(Succeed())

		Eventually(func(g Gomega) {
			g.Expect(backupWarningNotes(g, ns, run)).To(ContainElement(And(
				ContainSubstring("may remain quiesced and needs manual attention"),
				ContainSubstring(`cannot list resource "pods"`),
			)), "the backup is fine and an application may not be: that is the one thing only a human can fix")
		}, initTimeout, initPoll).Should(Succeed())

		By("and once the retries are spent the run finishes normally: the propagation is self-limiting")
		jobName := waitForMoverJob(ns, run, pvcName)
		simulateMoverSucceeded(jobName, "node-a", mover.MoverResult{
			OK: true, Operation: string(mover.OpBackup), SnapshotID: "snap-hkrel", SizeBytes: 12, AddedBytes: 6,
		})
		Eventually(func(g Gomega) {
			b := getBackupG(g, ns, run)
			g.Expect(b.Status.Phase).To(Equal("Completed"),
				"phase=%q — propagating the release's error is only safe because the attempt count ends it; "+
					"an unbounded propagation is the same head-of-line block in the time domain", b.Status.Phase)
			g.Expect(volumeByPVC(b, pvcName).SnapshotID).To(Equal("snap-hkrel"))
		}, initTimeout, initPoll).Should(Succeed())
	})
})

// ---------------------------------------------------------------------------
// The terminal phase is held while a release is OWED.
// ---------------------------------------------------------------------------

// listOnlyClient satisfies client.Client for the two closeFreezeWindow paths that perform I/O at all,
// and for nothing else: the embedded interface is nil, so any other method would panic — which is the
// assertion, not a shortcut. Three of the cases below must reach their verdict WITHOUT touching the
// apiserver, and a client that answered politely would hide it if they stopped.
type listOnlyClient struct {
	client.Client
	err error
}

func (c *listOnlyClient) List(_ context.Context, _ client.ObjectList, _ ...client.ListOption) error {
	return c.err
}

// TestCloseFreezeWindowOwedIsThreeState pins the answer writeStatus holds the terminal phase on, and
// the second case is the one that matters most.
//
// A hold keyed on status alone would pin every run that declares PRE hooks and no POST hooks
// non-terminal forever, because advancePostHooks returns ran=false both for a failure that owes a
// retry and for "there was nothing to run, ever", and postHooksRan cannot tell them apart either
// (nothing is recorded when zero hooks resolve). That regression is far worse than the gap being
// closed, so the distinction is taken from the run SPEC, which the caller already resolved.
//
// 0.6.5 lot 11 removed the hookPhaseState parameter this table used to pass: gating the release on a
// pre phase having run was defect B — a run declaring only post hooks released nothing, silently. The
// first case below is that defect, inverted into the assertion that it stays fixed. Every OTHER case
// is unchanged, which is the point: the two guards that bound the hold are the spec-derived one and
// the `!ran` one, and neither of them was ever `preRan`.
func TestCloseFreezeWindowOwedIsThreeState(t *testing.T) {
	postDeclared := cbv1.HooksSpec{
		Pre:  []cbv1.Hook{{Command: []string{"freeze"}}},
		Post: []cbv1.Hook{{Command: []string{"thaw"}}},
	}
	skipped := []cbv1.VolumeStatus{{Pvc: "v", Phase: status.VolumePhaseSkipped}}

	cases := []struct {
		name         string
		spec         cbv1.HooksSpec
		volumes      []cbv1.VolumeStatus
		attempts     int32
		listErr      error
		wantOwed     bool
		wantErr      bool
		wantAttempts int32
	}{
		{
			// DEFECT B. No pre phase ran — this run declares none — and the release is owed anyway: the
			// field is "post-backup", not "unfreeze", so a notification command or a thaw for something
			// quiesced out of band must still fire once the snapshots are cut. Under the old gate this
			// returned "nothing owed" and ran nothing, forever, without a word.
			name: "post hooks only, no quiesce ran: the release still fires",
			spec: cbv1.HooksSpec{Post: []cbv1.Hook{{Command: []string{"thaw"}}}}, volumes: skipped,
			listErr:      apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "", errors.New("nope")),
			wantOwed:     true,
			wantErr:      true,
			wantAttempts: 1,
		},
		{
			// THE TRAP. Pre hooks ran, no post hooks are declared, so no release will ever be owed —
			// and this must be decided from the spec, before any I/O, or a refused pods-list would
			// hold the run too.
			name: "pre hooks but no post hooks declared: nothing can ever be owed",
			spec: cbv1.HooksSpec{Pre: []cbv1.Hook{{Command: []string{"freeze"}}}}, volumes: skipped,
			listErr: errors.New("the apiserver must not be reached on this path"),
		},
		{
			// And the same answer for a run with NO hooks at all, which must never pay for the hook
			// phase: widening the release to post-only runs must not have widened it to every run.
			name: "no hooks at all: nothing owed, and no pods-list",
			spec: cbv1.HooksSpec{}, volumes: skipped,
			listErr: errors.New("the apiserver must not be reached on this path"),
		},
		{
			name:     "the attempt budget is spent: the release is over, however it went",
			spec:     postDeclared,
			volumes:  skipped,
			attempts: postHookMaxAttempts,
			listErr:  errors.New("the apiserver must not be reached on this path"),
			// postHooksRan short-circuits on the budget, so the bound is what ends the hold.
			wantAttempts: postHookMaxAttempts,
		},
		{
			name:     "a volume is still in the snapshot phase: owed, but nothing to release yet",
			spec:     postDeclared,
			volumes:  []cbv1.VolumeStatus{{Pvc: "v", Phase: status.VolumePhaseSnapshotting}},
			listErr:  errors.New("the apiserver must not be reached on this path"),
			wantOwed: true,
		},
		{
			// The hooks could not be resolved: we cannot know whether an application is frozen, and a
			// pre hook demonstrably ran. Assume the worst, hold — and SPEND an attempt, because the
			// spend is the only thing bounding the hold.
			name: "resolution failed: hold, and spend an attempt doing it",
			spec: postDeclared, volumes: skipped,
			listErr:      apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "", errors.New("nope")),
			wantOwed:     true,
			wantErr:      true,
			wantAttempts: 1,
		},
		{
			// The pods list succeeds and resolves NOTHING (no candidate pod mounts the volumes, or no exec
			// path is wired), so advancePostHooks does nothing and spends nothing. Reporting owed here
			// would be an UNBOUNDED hold — the one shape that must not exist, since only a spent attempt
			// can end one.
			name: "nothing resolves: nothing owed, and nothing spent",
			spec: postDeclared, volumes: skipped,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &BackupReconciler{
				Client:   &listOnlyClient{err: tc.listErr},
				Recorder: events.NewFakeRecorder(8),
			}
			backup := &cbv1.Backup{
				ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "run"},
				Status: cbv1.BackupStatus{
					Volumes:          tc.volumes,
					PostHookAttempts: tc.attempts,
				},
			}
			owed, err := r.closeFreezeWindow(context.Background(), backup, tc.spec)
			if owed != tc.wantOwed {
				t.Errorf("owed = %v, want %v", owed, tc.wantOwed)
			}
			if (err != nil) != tc.wantErr {
				t.Errorf("err = %v, want an error: %v", err, tc.wantErr)
			}
			if backup.Status.PostHookAttempts != tc.wantAttempts {
				t.Errorf("postHookAttempts = %d, want %d — an owed hold that spends nothing is an "+
					"UNBOUNDED hold, which is worse than the gap it closes",
					backup.Status.PostHookAttempts, tc.wantAttempts)
			}
		})
	}
}

var _ = Describe("BackupReconciler terminal phase held while a freeze window is owed", func() {

	// THE GAP THIS CLOSES, and it is the one place in the release where the thing lost is not a
	// snapshot id but somebody's database still being frozen.
	//
	// closeFreezeWindow fires on snapshotsCut — every volume having LEFT Pending/Snapshotting. This run
	// has ONE volume, which the progress deadline fails, so the roll-up becomes terminal on the VERY
	// PASS the release first fires. A failed attempt there is owed up to postHookMaxAttempts more, and
	// the already-terminal short-circuit at the top of Reconcile guaranteed none of them happened: the
	// product promised three attempts at unfreezing an application and delivered one, reported
	// Completed, and left behind only a Warning Event saying another attempt would follow.
	//
	// HOW THE HOLD IS PROVED, since "the phase was Uploading for 40ms" is not observable: arithmetic.
	// Reaching postHookMaxAttempts requires at least three passes AFTER the volume went terminal, and
	// without the hold there can be at most one — the pass that goes terminal. So the attempts reaching
	// three, and the loud UnfreezeFailed Event that is gated on them, can only happen if the run was
	// held non-terminal. The last assertion is the other half: the hold is BOUNDED by that same budget,
	// so the run must then finish.
	//
	// Mutations that must turn this spec red: removing releaseOwed from writeStatus's hold condition
	// (attempts stop at 1, no loud Event, and the run reports terminal immediately); and making
	// closeFreezeWindow report owed=false on a resolution failure (identical outcome by the other
	// route).
	It("keeps retrying the release, says so loudly, and only then reports terminal", func() {
		const (
			location = "bk-hkhold"
			ns       = "bk-hkhold-ns"
			run      = "bk-hkhold-run"
			pvcName  = "data-vol"
		)
		hookExecutor.reset()
		// Stalled young: the volume sits in Snapshotting, so the window stays open and the release cannot
		// fire until this spec is ready for it.
		backupExposers.stallSnapshot(ns+"/"+pvcName, time.Now())

		seedInitializedRepo(location, "kek-bk-hkhold", "s3-bk-hkhold")
		createTenantNamespace(ns)
		createSourcePVC(ns, pvcName, "ceph-block")
		createMountingPod(ns, "db-0", pvcName, nil)
		createHookedParent(run, location, cbv1.PVCSelector{}, cbv1.HooksSpec{
			Pre:  []cbv1.Hook{{Command: []string{"psql", "-c", "CHECKPOINT"}}},
			Post: []cbv1.Hook{{Command: []string{"psql", "-c", "SELECT pg_backup_stop()"}}},
		})
		createChildBackup(ns, run, location)

		By("the quiesce runs: the window is OPEN and the release has not fired")
		Eventually(func(g Gomega) {
			b := getBackupG(g, ns, run)
			g.Expect(hookPhaseCount(b, hooks.PhasePre)).To(Equal(1))
			g.Expect(string(volumeByPVC(b, pvcName).Phase)).To(Equal("Snapshotting"))
			g.Expect(hookPhaseCount(b, hooks.PhasePost)).To(Equal(0))
		}, initTimeout, initPoll).Should(Succeed())

		By("when the release becomes unresolvable, and the volume's only outcome is Failed")
		backupAPIFaults.refusePodList(ns, forbiddenPodList(ns))
		DeferCleanup(func() { backupAPIFaults.refusePodList(ns, nil) })
		// Backdated past the progress deadline: the volume fails, the roll-up turns terminal, and the
		// release fires — all on the same pass. That coincidence IS the defect.
		backupExposers.stallSnapshot(ns+"/"+pvcName, time.Now().Add(-2*snapshotProgressDeadline))

		By("the volume fails, and the release is nonetheless attempted its full budget of times")
		Eventually(func(g Gomega) {
			b := getBackupG(g, ns, run)
			g.Expect(string(volumeByPVC(b, pvcName).Phase)).To(Equal("Failed"))
			g.Expect(b.Status.PostHookAttempts).To(BeNumerically(">=", int32(postHookMaxAttempts)),
				"postHookAttempts=%d, phase=%q — reaching %d needs at least that many passes AFTER the "+
					"roll-up went terminal, and without the hold there is exactly one: the run reported "+
					"finished over an application that may still be quiesced",
				b.Status.PostHookAttempts, b.Status.Phase, postHookMaxAttempts)
		}, initTimeout, initPoll).Should(Succeed())

		By("and the operator gets the LOUD signal, which was previously unreachable on such a run")
		Eventually(func(g Gomega) {
			g.Expect(backupWarningNotes(g, ns, run)).To(ContainElement(And(
				ContainSubstring("may remain quiesced and needs manual attention"),
				ContainSubstring(`cannot list resource "pods"`),
			)), "this Event exists and was simply never reached: only a human can unfreeze the application")
		}, initTimeout, initPoll).Should(Succeed())

		By("and the hold is BOUNDED by that same budget: the run then reaches a terminal phase")
		Eventually(func(g Gomega) {
			b := getBackupG(g, ns, run)
			g.Expect(isTerminalBackupPhase(b.Status.Phase)).To(BeTrue(),
				"phase=%q — a hold that outlives the attempt budget would be a run that never terminates, "+
					"which is a worse bug than the one being fixed", b.Status.Phase)
			g.Expect(b.Status.CompletionTime).NotTo(BeNil())
		}, initTimeout, initPoll).Should(Succeed())
	})

	// THE REGRESSION THAT MATTERS MORE THAN THE FEATURE ABOVE.
	//
	// The hold's condition cannot be "a pre hook ran and no post hook has been recorded", because that
	// is also true, forever, of every run that declares pre hooks and no post hooks — nothing is
	// recorded when zero hooks resolve. Keying the hold on status alone would therefore pin an entire
	// category of perfectly healthy runs non-terminal for good: no Completed phase, no backupTime, no
	// completionTime, a Forbid schedule that skips every following night. That is the thirty-hour
	// incident manufactured on purpose, and it would have shipped looking like a safety feature.
	//
	// Mutation that must turn this spec red: conflating advancePostHooks' two ran=false cases, i.e.
	// reporting owed=true where closeFreezeWindow reports it for "nothing resolved". Nothing is spent on
	// that path, so nothing can ever end the hold: the run never leaves Uploading and this spec times
	// out. (Dropping the SPEC-DERIVED guard alone does NOT turn this spec red, and that is worth
	// knowing rather than papering over — the `!ran` branch already covers the happy path. The guard
	// earns its place on the path where resolution FAILS, which the next spec is for.)
	It("still reports terminal for a run that declares pre hooks and NO post hooks", func() {
		const (
			location = "bk-hkonlypre"
			ns       = "bk-hkonlypre-ns"
			run      = "bk-hkonlypre-run"
			pvcName  = "data-vol"
		)
		hookExecutor.reset()

		seedInitializedRepo(location, "kek-bk-hkonlypre", "s3-bk-hkonlypre")
		createTenantNamespace(ns)
		createSourcePVC(ns, pvcName, "ceph-block")
		createMountingPod(ns, "db-0", pvcName, nil)
		createHookedParent(run, location, cbv1.PVCSelector{}, cbv1.HooksSpec{
			Pre: []cbv1.Hook{{Command: []string{"psql", "-c", "CHECKPOINT"}}},
			// No Post, and no honorAnnotations: there is nothing to release, ever.
		})
		createChildBackup(ns, run, location)

		By("the quiesce runs")
		Eventually(func(g Gomega) {
			g.Expect(hookPhaseCount(getBackupG(g, ns, run), hooks.PhasePre)).To(Equal(1))
		}, initTimeout, initPoll).Should(Succeed())

		jobName := waitForMoverJob(ns, run, pvcName)
		simulateMoverSucceeded(jobName, "node-a", mover.MoverResult{
			OK: true, Operation: string(mover.OpBackup), SnapshotID: "snap-onlypre", SizeBytes: 3, AddedBytes: 2,
		})

		By("and the run COMPLETES: no release is owed, so nothing may hold it")
		Eventually(func(g Gomega) {
			b := getBackupG(g, ns, run)
			g.Expect(b.Status.Phase).To(Equal("Completed"),
				"phase=%q — a run with no post hooks was held non-terminal. postHooksRan() is false for it "+
					"forever, so a hold derived from status alone never lets go: no backupTime, no "+
					"completionTime, and a Forbid schedule that skips every night that follows.", b.Status.Phase)
			g.Expect(b.Status.BackupTime).NotTo(BeNil())
			g.Expect(b.Status.CompletionTime).NotTo(BeNil())
			g.Expect(hookPhaseCount(b, hooks.PhasePost)).To(Equal(0))
		}, initTimeout, initPoll).Should(Succeed())
	})

	// THE SECOND HALF OF THE TRAP, and the one the spec-derived guard does NOT cover.
	//
	// advancePostHooks reports ran=false for two unrelated situations, and only one of them owes a
	// retry. A run can declare post hooks — so the guard above lets it through — and still resolve NONE
	// of them: a podSelector that matches nothing, honorAnnotations with no annotated pod, no Running
	// pod mounting the volumes, or no exec path wired. Nothing is executed and nothing is SPENT, so
	// treating that as owed would be a hold with no clock and no budget behind it: non-terminal forever,
	// which is strictly worse than the gap the hold closes.
	//
	// Mutation that must turn this spec red: reporting owed=true from closeFreezeWindow's `!ran` branch.
	It("still reports terminal when a declared post hook resolves to no pod at all", func() {
		const (
			location = "bk-hknomatch"
			ns       = "bk-hknomatch-ns"
			run      = "bk-hknomatch-run"
			pvcName  = "data-vol"
		)
		hookExecutor.reset()

		seedInitializedRepo(location, "kek-bk-hknomatch", "s3-bk-hknomatch")
		createTenantNamespace(ns)
		createSourcePVC(ns, pvcName, "ceph-block")
		createMountingPod(ns, "db-0", pvcName, nil) // no labels: the selector below matches nothing
		createHookedParent(run, location, cbv1.PVCSelector{}, cbv1.HooksSpec{
			Pre: []cbv1.Hook{{Command: []string{"psql", "-c", "CHECKPOINT"}}},
			Post: []cbv1.Hook{{
				PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "not-this-one"}},
				Command:     []string{"psql", "-c", "SELECT pg_backup_stop()"},
			}},
		})
		createChildBackup(ns, run, location)

		Eventually(func(g Gomega) {
			g.Expect(hookPhaseCount(getBackupG(g, ns, run), hooks.PhasePre)).To(Equal(1))
		}, initTimeout, initPoll).Should(Succeed())

		jobName := waitForMoverJob(ns, run, pvcName)
		simulateMoverSucceeded(jobName, "node-a", mover.MoverResult{
			OK: true, Operation: string(mover.OpBackup), SnapshotID: "snap-nomatch", SizeBytes: 7, AddedBytes: 6,
		})

		By("and the run COMPLETES: a post hook that resolved to nothing owes nothing and spent nothing")
		Eventually(func(g Gomega) {
			b := getBackupG(g, ns, run)
			g.Expect(b.Status.Phase).To(Equal("Completed"),
				"phase=%q — a release that resolved to no pod was treated as owed. Nothing was spent on that "+
					"path, so no attempt budget can ever end the hold: the run is non-terminal forever.",
				b.Status.Phase)
			g.Expect(b.Status.PostHookAttempts).To(Equal(int32(0)))
			g.Expect(hookPhaseCount(b, hooks.PhasePost)).To(Equal(0))
		}, initTimeout, initPoll).Should(Succeed())
	})

	// The narrow case the SPEC-DERIVED guard exists for, isolated so it cannot pass by accident.
	//
	// A run with no post hooks reaches "nothing resolved" only if the pods list SUCCEEDS. When that list
	// is refused, resolution fails before anything can be resolved — and a failure is treated as "assume
	// the worst and hold", because normally we genuinely cannot know whether an application is frozen.
	// Here we can know, from the run spec: no post hooks are declared and annotations are not honoured,
	// so no release was ever owed and no attempt should be booked. Without the guard this run is held
	// for its whole attempt budget and an operator is told three times, once of them loudly, that an
	// application may be stuck — about a release that could not have existed.
	//
	// Mutation that must turn this spec red: removing the `len(spec.Post) == 0 && !spec.HonorAnnotations`
	// guard from closeFreezeWindow.
	It("books no release attempt for a run with no post hooks, even when the pod listing is refused", func() {
		const (
			location = "bk-hknopost"
			ns       = "bk-hknopost-ns"
			run      = "bk-hknopost-run"
			pvcName  = "data-vol"
		)
		hookExecutor.reset()
		// Stalled so the pre phase can succeed before the listing is refused (see the release spec above
		// for why the ordering cannot be won by racing the reconciler).
		backupExposers.stallSnapshot(ns+"/"+pvcName, time.Now())

		seedInitializedRepo(location, "kek-bk-hknopost", "s3-bk-hknopost")
		createTenantNamespace(ns)
		createSourcePVC(ns, pvcName, "ceph-block")
		createMountingPod(ns, "db-0", pvcName, nil)
		createHookedParent(run, location, cbv1.PVCSelector{}, cbv1.HooksSpec{
			Pre: []cbv1.Hook{{Command: []string{"psql", "-c", "CHECKPOINT"}}},
		})
		createChildBackup(ns, run, location)

		Eventually(func(g Gomega) {
			b := getBackupG(g, ns, run)
			g.Expect(hookPhaseCount(b, hooks.PhasePre)).To(Equal(1))
			g.Expect(string(volumeByPVC(b, pvcName).Phase)).To(Equal("Snapshotting"))
		}, initTimeout, initPoll).Should(Succeed())

		By("when the pod listing is refused and the volume is then let go")
		backupAPIFaults.refusePodList(ns, forbiddenPodList(ns))
		DeferCleanup(func() { backupAPIFaults.refusePodList(ns, nil) })
		backupExposers.unstallSnapshot(ns + "/" + pvcName)

		By("the volume proceeds and NO release attempt is ever booked against this run")
		Eventually(func(g Gomega) {
			g.Expect(string(volumeByPVC(getBackupG(g, ns, run), pvcName).Phase)).To(Equal("Uploading"))
		}, initTimeout, initPoll).Should(Succeed())
		Consistently(func(g Gomega) {
			b := getBackupG(g, ns, run)
			g.Expect(b.Status.PostHookAttempts).To(Equal(int32(0)),
				"postHookAttempts=%d — a release that was never declared cannot be owed, so the run must "+
					"not spend its unfreeze budget on one", b.Status.PostHookAttempts)
			for _, note := range backupWarningNotes(g, ns, run) {
				g.Expect(note).NotTo(ContainSubstring("release could not be attempted"),
					"an operator was warned about an application that may be quiesced by a hook this run "+
						"never declared")
			}
		}, consistentlyWindow, 500*time.Millisecond).Should(Succeed())

		By("and the run completes normally")
		jobName := waitForMoverJob(ns, run, pvcName)
		simulateMoverSucceeded(jobName, "node-a", mover.MoverResult{
			OK: true, Operation: string(mover.OpBackup), SnapshotID: "snap-nopost", SizeBytes: 5, AddedBytes: 4,
		})
		Eventually(func(g Gomega) {
			g.Expect(getBackupG(g, ns, run).Status.Phase).To(Equal("Completed"))
		}, initTimeout, initPoll).Should(Succeed())
	})
})

// ---------------------------------------------------------------------------
// (2) Step (10b): the manifest half.
// ---------------------------------------------------------------------------

var _ = Describe("BackupReconciler manifest half failing beside a healthy volume half", func() {

	// THE COUPLING (10b)'S OWN DOC FORBIDS, in the direction nobody had looked.
	//
	// advanceManifests documents that it runs INDEPENDENTLY of the per-PVC flow, because "the two
	// halves fail for unrelated reasons, and coupling them would lose one to the other's bad day" — and
	// then returned its error from a line sitting between step (10) and the status write, which lost the
	// volume half to the manifest half's bad day on every single pass.
	//
	// The fix is the shape REJECTED for a volume error: persist first, propagate after. The rejection
	// there was that controller-runtime charges the backoff to the BACKUP, so one bad volume would
	// stretch the poll driving every other volume of that namespace to the rate limiter's ceiling. A
	// manifest Job that cannot be created is a property of the run, not of one volume, so the object
	// being backed off is the object at fault, and reconcile_errors_total keeps a failure that deserves
	// counting.
	//
	// Mutations that must turn this spec red: restoring `return ctrl.Result{}, err` at step (10b) (the
	// volume never leaves Pending in etcd — it is recomputed in memory and discarded every pass —
	// so no mover Job is ever created and the second half of the spec times out); and dropping the
	// deferred propagation at the bottom of Reconcile (the refusal count stops climbing on the poll,
	// and, more importantly, a failure that used to be counted silently stops being).
	It("persists the volume half's progress and still retries the manifests", func() {
		const (
			location = "bk-mfunres"
			ns       = "bk-mfunres-ns"
			run      = "bk-mfunres-run"
			pvcName  = "data-vol"
		)
		// Durable, and armed before the run: a webhook refusing Jobs does not change its mind.
		backupAPIFaults.refuseManifestJob(ns, apierrors.NewForbidden(
			schema.GroupResource{Group: "batch", Resource: "jobs"}, "",
			errors.New(`admission webhook "policy.example.com" denied the request: `+
				`this namespace may not run Jobs with hostPath-free manifest access`)))

		seedInitializedRepo(location, "kek-bk-mfunres", "s3-bk-mfunres")
		createTenantNamespace(ns)
		createSourcePVC(ns, pvcName, "ceph-block")
		// Manifests ON — the default, and the point of the spec.
		createParentClusterBackup(run, location, cbv1.PVCSelector{})
		createChildBackup(ns, run, location)

		By("the volume half runs to Completion and every step of it is PERSISTED")
		jobName := waitForMoverJob(ns, run, pvcName)
		simulateMoverSucceeded(jobName, "node-a", mover.MoverResult{
			OK: true, Operation: string(mover.OpBackup), SnapshotID: "snap-mfunres", SizeBytes: 90, AddedBytes: 45,
		})
		Eventually(func(g Gomega) {
			vol := volumeByPVC(getBackupG(g, ns, run), pvcName)
			g.Expect(vol).NotTo(BeNil(),
				"status.volumes is nil: the manifest half's error was returned upstream of writeStatus, so "+
					"the volume half's whole pass — the enumeration included — was discarded")
			g.Expect(string(vol.Phase)).To(Equal("Completed"))
			g.Expect(vol.SnapshotID).To(Equal("snap-mfunres"))
		}, initTimeout, initPoll).Should(Succeed())

		By("and the manifest failure is STILL PROPAGATED — persisting must not cost it its retry")
		before := backupAPIFaults.manifestJobRefusalCount(ns)
		Expect(before).To(BeNumerically(">=", 1))
		// The sharpest single statement of "persist first, then propagate": one pass, driven by hand,
		// that both leaves the durable record above intact AND hands controller-runtime the manifest
		// error — which is what buys the backoff and the reconcile_errors_total observation that a
		// logged-and-swallowed failure would have thrown away. A Conflict from the suite's registered
		// reconciler driving the same object simply fails the substring check and the loop retries.
		Eventually(func(g Gomega) {
			_, err := backupReconciler.Reconcile(ctx,
				ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: run}})
			g.Expect(err).To(HaveOccurred(),
				"a manifest half that cannot start its Job must still be retried with backoff, not merely "+
					"logged: swallowing it would trade a counted failure for a log line nobody reads")
			g.Expect(err.Error()).To(ContainSubstring("create manifest mover Job"),
				"the error handed to controller-runtime must be the manifest failure itself; got %v", err)
		}, initTimeout, initPoll).Should(Succeed())
		Expect(backupAPIFaults.manifestJobRefusalCount(ns)).To(BeNumerically(">", before),
			"the manifest half stopped being ATTEMPTED at all")

		By("and the run does NOT claim to be finished while a half of it has produced nothing")
		Consistently(func(g Gomega) {
			b := getBackupG(g, ns, run)
			g.Expect(isTerminalBackupPhase(b.Status.Phase)).To(BeFalse(),
				"phase=%q — a terminal phase here would trip the already-terminal short-circuit and the "+
					"namespace's manifests would never be captured or reported", b.Status.Phase)
			g.Expect(b.Status.Manifests).To(BeNil())
		}, 3*time.Second, 500*time.Millisecond).Should(Succeed())
	})
})

// ---------------------------------------------------------------------------
// (3) The outer bound, and the anti-regression that makes it safe to ship.
// ---------------------------------------------------------------------------

var _ = Describe("BackupReconciler outer bound on a volume that cannot be advanced", func() {

	// THE BOUND. The layout is the incident's: the broken PVC sorts FIRST, so it holds the head of its
	// namespace's queue — and unlike a parked Pending volume, a volume in Snapshotting is NOT deferred
	// by firstNonTerminalVolume, because it legitimately holds work in flight. So the only thing that
	// can ever free the queue is that volume going terminal, and until this lot nothing could make it.
	//
	// The door used is reconstructExposure, which is the site where NO per-object bound is reachable:
	// the snapshot deadlines read their clock THROUGH the exposure this call is what failed to produce.
	// The previous lot named that as its honest residual, and this is it being closed.
	//
	// Mutations that must turn this spec red: removing the backstop call from reconstructExposure's
	// error path (the broken volume is recorded and retried forever, vol-b never gets a mover Job, and
	// the run never terminates — the thirty-hour incident, reproduced); and dropping the stamp of
	// phaseEnteredAt in advanceVolume (no clock, so the verdict can never be taken).
	It("bounds a volume whose exposure can never be reconstructed, and backs up the one behind it", func() {
		const (
			location  = "bk-advdl"
			ns        = "bk-advdl-ns"
			run       = "bk-advdl-run"
			brokenPVC = "vol-a-broken" // sorted first: the head of the queue, as in the incident
			okPVC     = "vol-b-ok"
		)
		// Young and not ready: the volume reaches Snapshotting (Expose succeeds once) and then stays
		// there with no snapshot deadline a candidate, which is the state the outer bound is about.
		backupExposers.stallSnapshot(ns+"/"+brokenPVC, time.Now())

		seedInitializedRepo(location, "kek-bk-advdl", "s3-bk-advdl")
		createTenantNamespace(ns)
		createSourcePVC(ns, brokenPVC, "ceph-block")
		createSourcePVC(ns, okPVC, "ceph-block")
		createVolumeOnlyParent(run, location, cbv1.PVCSelector{})
		createChildBackup(ns, run, location)

		By("the broken volume reaches Snapshotting and the controller stamps its own clock")
		Eventually(func(g Gomega) {
			vol := volumeByPVC(getBackupG(g, ns, run), brokenPVC)
			g.Expect(vol).NotTo(BeNil())
			g.Expect(string(vol.Phase)).To(Equal("Snapshotting"))
			g.Expect(vol.PhaseEnteredAt).NotTo(BeNil(),
				"without a phase-entry timestamp there is no clock at all for this volume, and the outer "+
					"bound is unreachable — which is exactly the state the previous lot left it in")
		}, initTimeout, initPoll).Should(Succeed())

		By("when the exposure can no longer be reconstructed — a quota that will not re-admit the clone")
		backupExposers.failExpose(ns+"/"+brokenPVC, apierrors.NewForbidden(
			schema.GroupResource{Resource: "persistentvolumeclaims"}, brokenPVC+"-clone",
			errors.New(`exceeded quota: tenant-quota, requested: requests.storage=1Gi, limited: requests.storage=10Gi`)))

		By("then the OUTER bound ends it, because no per-object deadline could ever judge it")
		r := advanceBackstopReconciler(time.Millisecond)
		Eventually(func(g Gomega) {
			reconcileTolerantOfConflicts(g, r, ns, run)
			vol := volumeByPVC(getBackupG(g, ns, run), brokenPVC)
			g.Expect(vol).NotTo(BeNil())
			g.Expect(string(vol.Phase)).To(Equal("Failed"),
				"phase=%q reason=%q — the snapshot deadlines read their clock through the exposure this "+
					"volume could not reconstruct, so it kept the head of its namespace's queue with nothing "+
					"that could ever end it", vol.Phase, vol.Reason)
			g.Expect(vol.Reason).To(HavePrefix(backupReasonAdvanceDeadline+":"),
				"the verdict must name the bound that fired, and it is the weakest of the four")
			g.Expect(vol.Reason).To(ContainSubstring("exceeded quota"),
				"the cause is all the diagnosis this deadline has: without it the reason names our decision "+
					"and nothing about the cluster")
		}, initTimeout, initPoll).Should(Succeed())

		// The load-bearing observation, and the one the old code silently did not produce.
		By("and the volume queued BEHIND it is exposed, moved and Completed anyway")
		jobName := waitForMoverJob(ns, run, okPVC)
		simulateMoverSucceeded(jobName, "node-a", mover.MoverResult{
			OK: true, Operation: string(mover.OpBackup), SnapshotID: "snap-advdl-b", SizeBytes: 70, AddedBytes: 35,
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

		By("and the bounded volume's exposure is torn down like any other terminal volume's")
		Eventually(func(g Gomega) {
			g.Expect(backupExposers.teardownCalls()).To(ContainElement(ns + "/" + moverNamePrefix(ns, run, brokenPVC)))
		}, initTimeout, initPoll).Should(Succeed())
	})

	// THE ANTI-REGRESSION, AND THE MOST IMPORTANT SPEC IN THIS LOT. Read it together with the one above
	// or neither means anything: that one proves a volume nothing can be learned about is eventually
	// ended, and this one proves a volume that is quietly doing its job is not.
	//
	// This is the case that costs data if it regresses, and the regression is one line away. The
	// backstop's clock is the volume's PHASE, and an Uploading volume's phase lasts exactly as long as
	// the upload does — so consulting the clock on the "still running; requeue" return would convert a
	// bound on volumes nobody can read into a WALL-CLOCK CAP ON BACKUP DURATION. It would fail the
	// largest volumes in the fleet first, on the nights they matter most, and it would look like a
	// working feature while doing it. Safety comes from the CALL SITES, never from the number.
	//
	// The bound is set to a MILLISECOND and the mover has been running for six hours, so the only thing
	// standing between a legitimate upload and destruction is that the backstop is not consulted here.
	//
	// Mutation that must turn this spec red: adding a failVolumeOnAdvanceBackstop call to
	// advanceUploading's `return "", nil // still running; requeue` — the single most plausible edit
	// somebody tidying this file would make, and the one that must never be made.
	It("leaves a mover that IS running alone, however far past the outer bound its phase is", func() {
		const (
			location = "bk-advslow"
			ns       = "bk-advslow-ns"
			run      = "bk-advslow-run"
			pvcName  = "data-vol"
		)
		jobName := driveToUploading(location, ns, run, pvcName, "kek-bk-advslow", "s3-bk-advslow")

		By("Given the volume has been in Uploading with a clock the outer bound would long since have crossed")
		Eventually(func(g Gomega) {
			vol := volumeByPVC(getBackupG(g, ns, run), pvcName)
			g.Expect(vol).NotTo(BeNil())
			g.Expect(vol.PhaseEnteredAt).NotTo(BeNil(),
				"the clock must exist for this spec to be testing anything: with no phaseEnteredAt the "+
					"backstop cannot fire on ANY volume, and this anti-regression would pass vacuously")
		}, initTimeout, initPoll).Should(Succeed())

		// Named "-running", not "-pod": the last step of this spec lets the upload finish through
		// simulateMoverSucceeded, which creates its own "<job>-pod". readMoverResult prefers the exit-0
		// attempt and skips a pod with no terminated container, so the two coexist exactly as a real
		// Job's attempts do.
		By("And its mover pod is Running — restic is reading a large volume and has been for hours")
		createMoverPod(jobName, jobName+"-running", func(pod *corev1.Pod) {
			pod.Status.Phase = corev1.PodRunning
			pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
				Name: "mover",
				State: corev1.ContainerState{
					Running: &corev1.ContainerStateRunning{
						StartedAt: metav1.NewTime(time.Now().Add(-6 * time.Hour)),
					},
				},
			}}
		})

		By("When reconciles run repeatedly with the outer bound set to a millisecond")
		r := advanceBackstopReconciler(time.Millisecond)
		Consistently(func(g Gomega) {
			reconcileTolerantOfConflicts(g, r, ns, run)
			vol := volumeByPVC(getBackupG(g, ns, run), pvcName)
			g.Expect(vol).NotTo(BeNil())
			g.Expect(string(vol.Phase)).To(Equal("Uploading"),
				"reason=%q — a RUNNING mover was failed by the outer bound. This is the regression that "+
					"turns a backstop on unreadable volumes into a wall-clock cap on backups, and it destroys "+
					"the largest volumes in the fleet first.", vol.Reason)
			g.Expect(vol.Reason).To(BeEmpty())
		}, 3*time.Second, 200*time.Millisecond).Should(Succeed())

		By("And its mover Job is still there, still running")
		Expect(k8sClient.Get(ctx,
			client.ObjectKey{Namespace: suiteOperatorNamespace, Name: jobName}, &batchv1.Job{})).To(Succeed())

		By("And the upload still completes normally, hours later, exactly as it should")
		simulateMoverSucceeded(jobName, "node-a", mover.MoverResult{
			OK: true, Operation: string(mover.OpBackup), SnapshotID: "snap-advslow", SizeBytes: 4 << 40, AddedBytes: 1 << 40,
		})
		Eventually(func(g Gomega) {
			b := getBackupG(g, ns, run)
			g.Expect(b.Status.Phase).To(Equal("Completed"))
			g.Expect(volumeByPVC(b, pvcName).SnapshotID).To(Equal("snap-advslow"))
		}, initTimeout, initPoll).Should(Succeed())
	})
})
