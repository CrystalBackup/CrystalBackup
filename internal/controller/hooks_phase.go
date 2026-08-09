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
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/hooks"
	"github.com/CrystalBackup/CrystalBackup/internal/status"
	"github.com/CrystalBackup/CrystalBackup/internal/tracing"
	"go.opentelemetry.io/otel/attribute"
)

// This file is the Backup controller's CONSISTENCY-HOOK phase (R16): the pre-snapshot quiesce and
// the post-snapshot release that bound the freeze window.
//
// The window is bounded by the SNAPSHOT phase, not the upload (01-architecture.md §5). A snapshot
// is a point in time that takes seconds to cut; the upload that follows can take hours. Holding a
// database frozen for the upload would turn every backup into an outage, so the post hooks fire as
// soon as the last snapshot exists — while the movers are still reading from it.
//
// Everything here treats the release as more important than the backup. A failed PRE hook aborts BY
// DEFAULT: the quiesce did not happen, so a snapshot taken anyway would look application-consistent
// while not being so, which is worse than no snapshot. A failed POST hook does NOT abort — the data
// is already captured, and the outstanding problem is an application that may still be quiesced. It
// retries, and when it runs out of retries it says so loudly, because at that point a human has to
// go and unfreeze something by hand.
//
// # onError: Continue, and why "by default" is the whole of R16's claim
//
// The abort is the DEFAULT, not the rule. `onError: Continue` is part of the API contract — "records
// the failure and proceeds, for best-effort hooks whose absence degrades consistency without
// invalidating the backup" (HookErrorPolicyContinue's own doc) — and 0.6.4 did not honour it for pre
// hooks: recordHookResults wrote the same `Failed` result whatever the policy said, hookState
// aborted on any failed pre entry, and a user who had explicitly asked for the backup to proceed got
// no backup at all. The policy was erased at the moment it was recorded.
//
// The alternative was genuinely weighed and rejected: REMOVE `Continue` from the enum for pre hooks
// and refuse it at admission, on the R16 argument that a snapshot behind a failed quiesce is a lie.
// It was rejected for two reasons.
//
//   - R16's argument is already correctly scoped, and scoped by this codebase's own words. It is the
//     reason `Fail` is the DEFAULT (see HookErrorPolicyFail), not a reason the other value cannot
//     exist. Plenty of real pre hooks are best-effort — flushing a cache, rotating a log, pausing a
//     sidecar — and their failure genuinely does not invalidate the data.
//   - It repeats 0.6.4's own lesson in reverse. That release's escrow gate blocked a run because it
//     could not prove a key was recoverable, and the entry recording the fix says it plainly: over-
//     blocking is not erring on the safe side, it MOVES the outage. Refusing `Continue` moves this
//     one from "a degraded backup" to "no backup", which is the worse of the two for the operator who
//     asked for the first.
//
// What makes proceeding honest rather than merely permissive is the ApplicationConsistent condition
// below: a durable, queryable record ON THE BACKUP that this restore point is crash-consistent only,
// naming the hook that failed. Without it, `Continue` would be a way for users to silently downgrade
// their own restore points — worse than the bug, because the downgrade would be invisible at exactly
// the moment it matters, which is a restore.

const (
	// postHookMaxAttempts bounds the unfreeze retries. Three is enough to ride out a container
	// restart or an API blip and few enough that a genuinely broken release surfaces in minutes
	// rather than being retried into the next backup window.
	postHookMaxAttempts = 3

	// hookPhaseBudget caps the WHOLE hook phase, over and above each hook's own timeout. Hooks run
	// inline on the reconcile worker — they are short, bounded and once per backup, unlike the
	// repository I/O M3.1 moved off the worker — but a namespace with many annotated pods could
	// still add up, so the phase as a whole has a ceiling.
	hookPhaseBudget = 5 * time.Minute

	// hookMessageLimit truncates a recorded failure to the CRD's MaxLength.
	hookMessageLimit = 1024
)

// ConditionApplicationConsistent reports whether the pre-snapshot quiesce this run ASKED FOR
// actually happened. It is the durable, queryable record that makes `onError: Continue` honest
// instead of merely permissive.
//
// It is TRI-STATE ON PURPOSE, and the absent state carries as much meaning as the other two:
//
//   - ABSENT — no pre hooks resolved for this run, so no application consistency was ever claimed.
//     Every backup without hooks is crash-consistent and always was; saying so on each of them
//     would be a False condition on the overwhelming majority of runs in a cluster, which is the
//     alert fatigue this release fights elsewhere. Absent means "the question was not asked".
//   - True (Quiesced) — pre hooks ran and every one of them succeeded. This is the claim R16
//     exists to make, and it is worth stating positively: an operator auditing restore points
//     needs to distinguish "quiesced" from "never asked", and a condition that only ever appeared
//     on failure could not.
//   - False (CrashConsistent) — a pre hook FAILED and its onError policy tolerated the failure, so
//     the snapshot was taken anyway. The message names the hook, the pod and the container, and
//     says the words "crash-consistent" so the reader does not have to derive them.
//
// WHY A CONDITION AND NOT ONLY A HOOK ENTRY. status.hooks already records the failure, and it was
// not enough: it says a hook failed, never that the backup CONTINUED, and it is nested inside a
// list nothing surfaces — not `kubectl get backup`, not a dashboard, not a roll-up. Conditions are
// the one part of status the whole ecosystem already reads and alerts on, and this is a fact whose
// audience is a human deciding, months later, whether this restore point can be trusted for a
// database. It also survives the retry/replace mechanics that legitimately rewrite status.hooks.
//
// The PHASE deliberately stays Completed. Nothing happened partially — every volume was captured,
// which is exactly what the user asked for when they wrote `Continue` — and PartiallyCompleted
// would mean a run that alarms on every single execution forever for a degradation its owner chose.
const ConditionApplicationConsistent = "ApplicationConsistent"

const (
	// reasonQuiesced: pre hooks ran and all succeeded.
	reasonQuiesced = "Quiesced"
	// reasonCrashConsistent names the CONSEQUENCE rather than the event ("PreHookFailed" would be
	// the event, and it is already the abort path's reason on the Ready condition). What the reader
	// of a months-old restore point needs from a reason string is what the backup IS.
	reasonCrashConsistent = "CrashConsistent"
)

// hookPhaseState is what Reconcile needs to know about the freeze window this pass.
type hookPhaseState struct {
	// preRan is true once the pre phase has been executed and recorded (successfully or not).
	preRan bool
	// aborted is true when a pre hook failed with onError=Fail: the backup must not snapshot.
	aborted bool
	// abortMessage explains the abort, for the terminal condition.
	abortMessage string
}

// hookState derives the freeze window's state from the durable record in status, so it survives an
// operator restart: a controller that dies between the quiesce and the release comes back, sees a
// recorded pre phase and no post phase, and releases. That crash case is the feature's single most
// important behaviour, and deriving state from status rather than memory is what makes it work.
//
// IT IS ALSO WHY THE POLICY HAD TO BECOME PART OF THE RECORD. The abort is decided HERE, on a later
// pass than the execution — advancePreHooks records, openFreezeWindow persists and requeues, and
// this function reads the result back — so whatever status does not carry is a distinction the
// controller does not have when it makes the decision. Before HookStatus.OnError existed, a failed
// pre entry was indistinguishable from an aborting one and every one of them aborted.
func hookState(backup *cbv1.Backup) hookPhaseState {
	var st hookPhaseState
	for i := range backup.Status.Hooks {
		h := &backup.Status.Hooks[i]
		if h.Phase != string(hooks.PhasePre) {
			continue
		}
		st.preRan = true
		if h.Result == cbv1.HookFailed && !hookFailureTolerated(h) {
			st.aborted = true
			st.abortMessage = fmt.Sprintf("pre-backup hook failed in pod %s: %s", h.Pod, h.Message)
		}
	}
	return st
}

// hookFailureTolerated reports whether a recorded failure was one its author asked the run to
// survive: onError=Continue, from the spec or from the pod's `on-error` annotation.
//
// The EMPTY policy is NOT tolerated, and that is the load-bearing half. It matches
// HookErrorPolicy's own documented zero-value rule ("callers must treat as Fail … so a hook whose
// policy was never set fails closed"), and it is what makes an upgrade over a Backup already
// mid-freeze-window safe: entries written by a binary that recorded no policy keep aborting exactly
// as they did, rather than being reinterpreted as tolerated by a newer one.
func hookFailureTolerated(h *cbv1.HookStatus) bool {
	return h.OnError == cbv1.HookErrorPolicyContinue
}

// postHooksRan reports whether the release phase has already been recorded as done — either every
// hook still owed succeeded, or the retries were exhausted.
//
// A TOLERATED failure counts as done, and this is the post-hook mirror of the same defect A fixed
// for the pre phase. recordHookResults is shared by both phases, so a post hook with
// `onError: Continue` used to be recorded exactly like an intolerable one: this function reported
// the release as still owed, the run spent its whole unfreeze budget re-running a command whose
// failure its author had explicitly said to tolerate, and — since lot 10 — the terminal phase was
// HELD for the duration as well. Fixing only the pre half would have been worse than fixing
// neither, because the shared function would have looked done.
//
// It stays honest about the thing that actually matters here: a NON-tolerated post failure still
// owes another attempt, because the outstanding problem is an application that may still be frozen.
func postHooksRan(backup *cbv1.Backup) bool {
	if backup.Status.PostHookAttempts >= postHookMaxAttempts {
		return true
	}
	seen := false
	for i := range backup.Status.Hooks {
		h := &backup.Status.Hooks[i]
		if h.Phase != string(hooks.PhasePost) {
			continue
		}
		seen = true
		if h.Result == cbv1.HookFailed && !hookFailureTolerated(h) {
			return false // a recorded failure nobody agreed to tolerate means another attempt is owed
		}
	}
	return seen
}

// snapshotsCut reports whether every volume has left the snapshot phase — succeeded, skipped or
// failed alike. This is the release trigger, and using the volumes' EXIT from snapshotting rather
// than their success is the whole "unconditional unfreeze" guarantee: a snapshot that failed still
// leaves an application quiesced, and a release conditioned on success would strand it.
func snapshotsCut(volumes []cbv1.VolumeStatus) bool {
	for i := range volumes {
		switch string(volumes[i].Phase) {
		case string(status.VolumePhasePending), "", string(status.VolumePhaseSnapshotting):
			return false
		}
	}
	return true
}

// openFreezeWindow is step (9b) of Reconcile: it runs the quiesce ONCE, before any VolumeSnapshot
// exists, and reports whether this reconcile is over (done=true → the caller returns res, err
// verbatim).
//
// When the hooks ran it persists the record and stops the pass deliberately: the snapshots must
// not start until the quiesce is durable in status, because a controller that died between the two
// has no other way to come back knowing it froze something.
func (r *BackupReconciler) openFreezeWindow(ctx context.Context, backup *cbv1.Backup, rc *backupRunContext,
	st hookPhaseState, spec cbv1.HooksSpec,
) (res ctrl.Result, done bool, err error) {
	if st.aborted {
		// rc rides along solely so the Failed terminal transition can be recorded with the same
		// metric identity writeStatus would have given it — a hook abort is the one terminal path
		// that never reaches the status writer.
		return ctrl.Result{}, true, r.failHooks(ctx, backup, rc, st.abortMessage)
	}
	if st.preRan {
		return ctrl.Result{}, false, nil
	}
	ran, err := r.advancePreHooks(ctx, backup, spec, backup.Status.Volumes)
	if err != nil {
		// A QUIESCE THAT COULD NOT EVEN BE ARRANGED IS A FAILED QUIESCE, and it goes out through the
		// door a failed quiesce already has rather than through a second one of its own.
		//
		// This line is the worst instance of the errored-pass class, and its signature is the
		// thirty-hour incident exactly. advancePreHooks fails when the hooks cannot be RESOLVED (the
		// pods-list refused, because RBAC was narrowed under a running operator) or when hooks are
		// declared and no exec path is wired (r.Hooks == nil, a misconfigured operator process).
		// Both error on EVERY pass, and the error used to be returned from here — upstream of the
		// single status write at Reconcile step (11) — so status.volumes never reached etcd AT ALL.
		// The run therefore never terminated, its ClusterBackup never did, its schedule's Forbid
		// policy skipped every following night, and there was nothing per-volume for an
		// administrator to read while it happened: not a phase, not a cause, not even the
		// enumeration.
		//
		// failHooks is the right door for three reasons, in ascending order of importance.
		//
		//   - It already exists and already means this. It writes the terminal Failed phase, stamps
		//     completionTime, records the PreHookFailed condition with the cause, emits the
		//     BackupFailed Warning Event, and counts the run in the terminal metrics. A second path
		//     would have had to reproduce all of that and would have drifted from it.
		//   - Its status write is what persists status.volumes. The enumeration from step (9) is on
		//     the same in-memory object, so routing here fixes the discarded-pass half of the defect
		//     for free, without inventing a write of its own.
		//   - It keeps R16's distinction intact, which is the constraint that outranks everything
		//     else here (01-architecture.md §5). The pre hooks exist to make the snapshot
		//     TRUSTWORTHY. Continuing past a quiesce that did not happen would produce a backup that
		//     LOOKS application-consistent and is not — discovered at restore time, when it is far
		//     too late — so the only outcomes available are "quiesce, then snapshot" and "no
		//     snapshot". Never "snapshot anyway".
		//
		// AND `onError: Continue` DOES NOT REACH THIS PATH, which is worth stating now that the policy
		// is honoured everywhere else. A resolution failure means the hooks were never resolved, so
		// there is no policy to consult: nobody said "proceed if THIS fails", because the operator
		// cannot even say which hooks THIS would have been. Failing closed is not the strict reading of
		// a tolerant policy, it is the absence of one — and the alternative would be to infer consent
		// from a spec whose hooks were never matched against a single pod.
		//
		// NO RETRIES, AND THAT IS THE DECLARED CONTRACT rather than an omission: "post hooks are
		// retried where pre hooks are not, and the asymmetry is deliberate" (BackupStatus.
		// postHookAttempts' own doc). A pre hook whose EXEC fails once — a transient error inside the
		// exec stream — already fails the backup with no second attempt, so giving a failed pods-LIST
		// patience the failed exec does not have would be incoherent, and it would need a durable
		// attempt counter on the API to be honest about it. A transient cause therefore costs this
		// run and this run only: the next scheduled one resolves its hooks and quiesces normally,
		// which is the same trade backupReasonPrecheckFailed documents for a structural refusal — a
		// visible, alertable failure now beats a hang nobody is watching.
		//
		// The message carries the cause VERBATIM (capped at the CRD's own limit, as every recorded hook
		// message is). failHooks' Event already prefixes "no snapshot was taken", so this says what could
		// not be done and then quotes the API server; a message that said only "hooks failed" would leave
		// the reader exactly as blind as thirty hours of silence did.
		return ctrl.Result{}, true, r.failHooks(ctx, backup, rc,
			"the pre-backup quiesce could not be performed: "+truncate(err.Error(), hookMessageLimit))
	}
	if !ran {
		// No hooks apply to this run: fall through to the snapshots with no window open.
		return ctrl.Result{}, false, nil
	}
	backup.Status.Phase = string(status.BackupPhaseSnapshottingHooks)
	if err := r.Status().Update(ctx, backup); err != nil {
		return ctrl.Result{}, true, fmt.Errorf("record pre-backup hooks for Backup %s/%s: %w",
			backup.Namespace, backup.Name, err)
	}
	return ctrl.Result{Requeue: true}, true, nil
}

// closeFreezeWindow is step (10c) of Reconcile: the release, fired on the snapshots being CUT —
// not on their having succeeded, and NOT on anything having been frozen first.
//
// A snapshot that failed still leaves an application quiesced, so gating the release on success
// would strand exactly the workload whose backup just went wrong (R16, the delta-8 guarantee). It
// also runs long before the upload finishes: hooks bound the snapshot phase, and a database held
// frozen for a multi-hour upload would be an outage rather than a backup.
//
// # A run that declares ONLY post hooks (defect B)
//
// This used to gate on st.preRan, and advancePreHooks records nothing when zero pre hooks resolve —
// so a `hooks` spec with `post` and no `pre`, which the API offers as two independent fields and
// admission accepts without a murmur, never ran a single post hook and never said so. A declared
// hook that silently never executes is the same defect class as a policy the recorder throws away.
//
// THE SEMANTICS CHOSEN: post hooks fire once the snapshots are cut, whether or not this operator
// froze anything. "post-backup" is what the field is called and what the annotation says
// (`crystalbackup.io/post-backup-command`); it is not named "unfreeze". The two obvious real uses
// need exactly this — a "the backup is done" notification or checkpoint-rotation command, and a thaw
// for an application quiesced OUT OF BAND by something that is not us — and both are unreachable if
// the release is conditioned on our own pre phase. Nothing is lost by widening it: a run that
// declares no post hooks still resolves none, and the guard below still answers "no" before any I/O.
//
// It deliberately does NOT resurrect the unbounded-hold trap lot 10 closed. The gate that keeps the
// hold safe was never `preRan`: it is the pair of guards below — the SPEC-derived one, which answers
// "no release can ever be owed" without touching the apiserver, and the `!ran` one, which separates
// "an attempt failed" from "there was nothing to run". Lot 10's report is explicit that the
// spec-derived guard ALONE was insufficient and that a mutation survived until both existed; both
// are still here, both are still tested, and every path that can report owed=true still spends an
// attempt.
//
// ITS ERROR IS RETURNED, AND THE CALLER PERSISTS FIRST. That ordering is the whole of this step's
// share of the errored-pass fix: the error used to be returned from Reconcile step (10c), upstream
// of the single status write, so a release phase that could not list pods discarded everything the
// volume half of the same pass had computed — the precise coupling the manifest half's own doc says
// must not exist. Reconcile now writes status and propagates afterwards (see step (10c) there), so
// the failure keeps controller-runtime's backoff and its reconcile_errors_total visibility without
// costing the pass its record.
//
// Propagating is SAFE ONLY BECAUSE THE FAILURE IS ALSO BOUNDED, which is what advancePostHooks does
// with it (see recordUnresolvedRelease): an unresolvable release consumes a release ATTEMPT, so
// after postHookMaxAttempts postHooksRan reports the phase as done, this function short-circuits at
// the guard below, and the errors stop. Without that bound, propagating would have been a Backup
// re-driven with backoff forever — the same defect in the time domain.
//
// # owed: why this is three-state and not a boolean error
//
// owed=true means A RELEASE IS STILL DUE ON THIS RUN, and the caller must not let the Backup reach a
// terminal phase while it is (writeStatus holds the phase exactly as it does for an in-flight
// manifest half). Without that hold there is a gap with teeth: the release fires on snapshotsCut, and
// on a run whose volumes all went Skipped or Failed that is the SAME pass on which the roll-up
// becomes terminal — so a failed attempt on that pass is owed a retry that the already-terminal
// short-circuit at the top of Reconcile guarantees will never happen. The product promises three
// attempts at unfreezing an application and delivered one, and the only durable trace was a Warning
// Event saying another attempt would follow. An application could be left quiesced with no loud
// signal at all, while the run reported Completed.
//
// The alternative considered — and it is a reasonable one — was to emit the loud UnfreezeFailed Event
// from the pass that first goes terminal, leaving the phase alone. It was rejected for three reasons.
// The codebase has already decided this question one concern over: writeStatus holds the terminal
// phase for an unfinished manifest half, with the same argument verbatim ("would trip the
// already-terminal short-circuit … and the capture in flight would never have its result recorded"),
// and two answers to one question is how a codebase acquires a contradiction. The loud Event already
// exists and was merely UNREACHABLE, so adding a second signal would have been compensating for the
// designed one not firing — treating a broken retry policy as a reporting problem. And reporting
// Completed while an application is still frozen is the same species of dishonesty this whole release
// is about.
//
// THE HOLD IS BOUNDED, which is the only thing that makes it safe, and the bound is the attempt
// budget rather than a clock: every path that can report owed=true has already spent one attempt —
// advancePostHooks increments PostHookAttempts before running a command, and recordUnresolvedRelease
// increments it for an attempt that never got that far — so after at most postHookMaxAttempts passes
// postHooksRan reports the phase done, owed goes false, and the run terminates. The one path that
// increments nothing (nothing resolved, or no executor wired) reports owed=FALSE, so it cannot hold.
//
// The hookPhaseState the caller derived is NOT a parameter here, and its absence is the fix for
// defect B rather than a tidy-up: nothing about this decision depends on whether a pre phase ran.
// Passing it in would leave the next reader — and the next mutation — with a value whose only
// plausible use is the gate that had to be removed.
func (r *BackupReconciler) closeFreezeWindow(ctx context.Context, backup *cbv1.Backup,
	spec cbv1.HooksSpec,
) (owed bool, err error) {
	// The release is already settled or spent. postHooksRan covers both: a record with no
	// intolerable failure left in it, and an exhausted attempt budget.
	if postHooksRan(backup) {
		return false, nil
	}
	// NO RELEASE CAN EVER BE OWED BY THIS RUN, and this guard is what keeps the hold from becoming a
	// catastrophe. advancePostHooks returns ran=false for two unrelated situations — a failure that
	// owes a retry, and "there was nothing to run, ever" — and postHooksRan cannot tell them apart
	// either, because nothing is recorded when zero hooks resolve. A hold keyed on status alone would
	// therefore pin EVERY run that declares pre hooks and no post hooks non-terminal forever, which is
	// far worse than the gap being closed. The distinction is not derivable from status, but it does
	// not need to be: it is in the run spec the caller already resolved and already passes in.
	//
	// honorAnnotations counts as "possible": a pod may declare post-backup annotations that only a pod
	// listing can discover, so a run with an empty spec.post can still owe a release.
	if len(spec.Post) == 0 && !spec.HonorAnnotations {
		return false, nil
	}
	if !snapshotsCut(backup.Status.Volumes) {
		// The window is open and the release has not been tried, but a volume is still in
		// Pending/Snapshotting so nothing can be released yet. Reported as owed because it IS owed;
		// the caller's hold is inert here anyway, since a run with a volume in either of those phases
		// does not roll up to a terminal phase.
		return true, nil
	}
	ran, err := r.advancePostHooks(ctx, backup, spec, backup.Status.Volumes)
	if err != nil {
		// The hooks could not be RESOLVED, so we cannot know whether an application is frozen. Assume
		// the worst and hold; it costs at most the attempt budget.
		//
		// It holds for a run that declared no PRE hooks too, and that is deliberate rather than
		// overlooked. The run declared a release, so something was expected to be released — an
		// out-of-band quiesce is the documented reason to write post hooks without pre ones — and this
		// operator has no way to know from here whether it was. "We froze nothing ourselves" is not
		// evidence that nothing is frozen; it is only evidence about who froze it.
		return true, err
	}
	if !ran {
		// Nothing resolved, or no exec path is wired. Nothing is owed and nothing was spent, so this
		// must not hold: see the guard above for why that matters.
		return false, nil
	}
	// The attempt executed. It is owed another one only if it left a failure behind and the budget is
	// not yet spent, which is exactly what postHooksRan answers.
	return !postHooksRan(backup), nil
}

// advancePreHooks runs the quiesce once, before any VolumeSnapshot exists.
//
// It returns ran=true when it executed this pass (the caller must persist status and requeue
// without touching a volume — the snapshots must not start until the record is durable).
func (r *BackupReconciler) advancePreHooks(ctx context.Context, backup *cbv1.Backup,
	spec cbv1.HooksSpec, volumes []cbv1.VolumeStatus,
) (ran bool, err error) {
	resolved, err := r.resolveHooks(ctx, backup, spec, volumes, hooks.PhasePre)
	if err != nil {
		return false, err
	}
	if len(resolved) == 0 {
		// Nothing to quiesce. Record NOTHING and let hookState report preRan=false forever: a
		// backup with no hooks must not carry an empty freeze-window record implying one happened.
		return false, nil
	}
	if r.Hooks == nil {
		// Hooks are declared but the operator has no exec path (a misconfiguration, not a
		// workload problem). Failing loudly beats silently taking a crash-consistent snapshot the
		// operator believes is application-consistent.
		return false, fmt.Errorf("hooks are configured for Backup %s/%s but no pod-exec executor is wired",
			backup.Namespace, backup.Name)
	}

	phaseCtx, cancel := context.WithTimeout(ctx, hookPhaseBudget)
	defer cancel()
	started := time.Now()
	results := hooks.Run(phaseCtx, r.Hooks, resolved)
	r.recordHookResults(backup, results)
	// The consistency verdict is stamped HERE, on the same in-memory object the caller is about to
	// persist, so it lands in the SAME status write as the hook entries it is derived from. A second
	// write would have opened a window in which a Backup was proceeding past a failed quiesce with
	// nothing on it saying so — and that window is precisely where a crashing operator loses things.
	r.recordConsistencyVerdict(backup, results)
	emitHookSpan(ctx, backup, tracing.StepHooksPre, "hooks.pre", started, resolved, results)
	return true, nil
}

// recordConsistencyVerdict sets ConditionApplicationConsistent from the pre phase's results, and
// makes a tolerated quiesce failure LOUD once — see the condition's own doc for the tri-state and
// for why a hook entry alone was not enough.
//
// It is called with the results of ONE pre phase (the phase never retries: "post hooks are retried
// where pre hooks are not, and the asymmetry is deliberate"), so there is no accumulation to reason
// about and no attempt number to disambiguate.
//
// A NON-tolerated failure sets nothing, deliberately. That run is about to be terminated by
// failHooks, which records Ready=False/PreHookFailed and takes no snapshot at all; adding
// "ApplicationConsistent=False" to it would be labelling the consistency of a restore point that
// does not exist. The condition's audience is someone holding a backup and asking whether to trust
// it, and a Failed run has nothing to hold.
func (r *BackupReconciler) recordConsistencyVerdict(backup *cbv1.Backup, results []hooks.Result) {
	var tolerated []string
	for _, res := range results {
		if res.Hook.Phase != hooks.PhasePre || res.Err == nil || isSkipped(res) {
			continue
		}
		if !res.Aborts() {
			tolerated = append(tolerated, fmt.Sprintf("pod %s [%s] (%s): %v",
				res.Hook.Pod.Name, res.Hook.Container, res.Hook.Source, res.Err))
			continue
		}
		return // the run is about to fail; see the doc above
	}

	if len(tolerated) == 0 {
		status.SetCondition(&backup.Status.Conditions, ConditionApplicationConsistent, metav1.ConditionTrue,
			reasonQuiesced, "every pre-backup hook succeeded: the snapshots are application-consistent",
			backup.Generation)
		return
	}

	message := truncate(fmt.Sprintf(
		"THIS RESTORE POINT IS CRASH-CONSISTENT ONLY: %d pre-backup hook(s) failed and onError=Continue "+
			"tolerated the failure, so the snapshots were taken without the quiesce having happened — %s",
		len(tolerated), strings.Join(tolerated, "; ")), hookMessageLimit)
	status.SetCondition(&backup.Status.Conditions, ConditionApplicationConsistent, metav1.ConditionFalse,
		reasonCrashConsistent, message, backup.Generation)
	// One Event per run, and it says the thing status.hooks does not: that the backup CONTINUED. The
	// per-hook HookFailed Warning recordHookResults already emitted says a command failed, which on
	// every other path means the run is over — so on its own it would leave a reader with exactly the
	// wrong expectation about what happened next.
	r.Recorder.Eventf(backup, nil, corev1.EventTypeWarning, "ConsistencyDegraded", "RunHooks",
		"proceeding with a CRASH-CONSISTENT backup of %s/%s: %s",
		backup.Namespace, backup.Name, message)
}

// emitHookSpan emits §5's `hooks.pre` / `hooks.post` span.
//
// These two are the only spans in the operator's tree timed by a clock in this process rather than
// read back off object state, and they can be: a hook phase runs to completion inside ONE
// reconcile, under hookPhaseBudget, so there is no cross-reconcile window to reconstruct. It is
// still emitted after the fact rather than held open, for uniformity — one shape for every span in
// this codebase means one place to reason about what a dying process loses.
//
// step is qualified for post-hooks (see the caller): the release phase RETRIES, up to
// postHookMaxAttempts, and a derived span id reused across attempts would publish several
// different spans under one identity.
func emitHookSpan(
	ctx context.Context, backup *cbv1.Backup, step, name string,
	started time.Time, resolved []hooks.Resolved, results []hooks.Result,
) {
	anchor := backupAnchor(backup)
	if !anchor.Valid() {
		return
	}
	attrs := backupSpanAttrs(backup, nil)
	attrs = append(attrs, attribute.Int("crystalbackup.hooks", len(resolved)))

	var spanErr error
	if failed := failedResults(results); failed > 0 {
		spanErr = fmt.Errorf("%d of %d %s hook(s) failed", failed, len(resolved), name)
	}
	anchor.EmitStep(ctx, step, name, started, time.Now(), spanErr, attrs...)
}

// advancePostHooks runs the release as soon as every snapshot is cut, whatever their outcome — and
// whatever the pre phase did, including not existing (see closeFreezeWindow, defect B).
func (r *BackupReconciler) advancePostHooks(ctx context.Context, backup *cbv1.Backup,
	spec cbv1.HooksSpec, volumes []cbv1.VolumeStatus,
) (ran bool, err error) {
	resolved, err := r.resolveHooks(ctx, backup, spec, volumes, hooks.PhasePost)
	if err != nil {
		// A release that could not be RESOLVED is a release ATTEMPT that failed, and it is counted as
		// one. Everything below already knows how to handle a release that does not work — count the
		// attempt, warn per attempt, and say so loudly once the retries are gone because a human now
		// has to unfreeze something by hand — and a resolution failure needs exactly that treatment.
		// The alternative shape, returning the bare error, is what made this the second-worst instance
		// of the errored-pass class: it discarded the whole pass AND was retried forever, so the run
		// never terminated and nothing ever said that an application might still be quiesced.
		//
		// Counting it is deliberate even though it spends a budget sized for exec failures. Post hooks
		// ARE retried (unlike pre hooks), so some patience is owed to a transient pods-list error; but
		// the budget has to be spent by every kind of failed attempt or it is not a bound at all, and
		// an unbounded release is how a Backup is held non-terminal forever by a cluster nobody is
		// repairing.
		r.recordUnresolvedRelease(backup, err)
		return false, err
	}
	if len(resolved) == 0 || r.Hooks == nil {
		return false, nil
	}

	backup.Status.PostHookAttempts++
	// Drop the previous attempt's records so the list shows this attempt, not an accumulation of
	// every retry — the useful question is "is it released NOW", not "how many times did we try".
	backup.Status.Hooks = dropHookPhase(backup.Status.Hooks, hooks.PhasePost)

	phaseCtx, cancel := context.WithTimeout(ctx, hookPhaseBudget)
	defer cancel()
	started := time.Now()
	results := hooks.Run(phaseCtx, r.Hooks, resolved)
	r.recordHookResults(backup, results)
	// The attempt number is part of the step key: a retried release must produce a SECOND span,
	// not overwrite the first with the same derived id — and a trace showing three attempts is
	// exactly what an operator investigating a workload that will not unfreeze needs to see.
	emitHookSpan(ctx, backup,
		fmt.Sprintf("%s/%d", tracing.StepHooksPost, backup.Status.PostHookAttempts),
		"hooks.post", started, resolved, results)

	// The phase-level Events are gated on the failures NOBODY AGREED TO TOLERATE, not on every
	// failure. A post hook with onError=Continue that fails is not an application left frozen: its
	// author said in the spec that its failure is survivable, postHooksRan agrees and books no
	// further attempt, so "retrying" would be a promise this run will not keep and "an application
	// may remain quiesced and needs manual attention" would be a page raised over a documented
	// non-event. The failure is not hidden — recordHookResults has already written the entry and
	// emitted a per-hook HookFailed Warning; what changes is that the run stops claiming a release
	// is outstanding when none is.
	if failed := untoleratedResults(results); failed > 0 {
		if backup.Status.PostHookAttempts >= postHookMaxAttempts {
			// Out of retries. This is the loud one: the backup itself is fine — the data is
			// captured — but an application may still be quiesced, and only a human can undo that.
			r.Recorder.Eventf(backup, nil, corev1.EventTypeWarning, "UnfreezeFailed", "RunPostHooks",
				"%d post-backup hook(s) still failing after %d attempts on Backup %s/%s — an application may remain quiesced and needs manual attention",
				failed, backup.Status.PostHookAttempts, backup.Namespace, backup.Name)
		} else {
			r.Recorder.Eventf(backup, nil, corev1.EventTypeWarning, "HookFailed", "RunPostHooks",
				"%d post-backup hook(s) failed on attempt %d; retrying", failed, backup.Status.PostHookAttempts)
		}
	}
	return true, nil
}

// recordUnresolvedRelease books a release attempt that never got as far as running a command, and
// makes it as loud as one that ran and failed.
//
// It exists so that a release can NEVER be abandoned silently, which propagating the error alone
// would not have guaranteed. The window's release fires on snapshotsCut, and on a run whose volumes
// all went Skipped or Failed that is the same pass on which the Backup reaches a terminal phase — so
// the next reconcile short-circuits on the terminal phase and there is no later pass to retry in.
// An error returned into that gap would be a frozen application and not one word about it anywhere.
// A counted attempt plus a Warning Event per attempt survives the run ending, whichever pass it ends
// on.
//
// It records NO HookStatus entry, and that restraint matters: status.hooks is the durable account of
// what ran in which pod, every entry of it written by recordHookResults from a real execution. An
// entry naming a pod no command reached would be a fabricated record in the one place an operator
// trusts to be literal. What an administrator sees instead is postHookAttempts counting up with no
// post entries beside it — "the release was attempted N times and never even started" — plus the
// Events, which carry the cause.
func (r *BackupReconciler) recordUnresolvedRelease(backup *cbv1.Backup, cause error) {
	backup.Status.PostHookAttempts++
	if backup.Status.PostHookAttempts >= postHookMaxAttempts {
		// The loud one, on exactly the terms advancePostHooks uses it: the data is captured, so the
		// BACKUP is fine, and the outstanding problem is a workload that may still be quiesced with
		// nothing left that will try to release it.
		r.Recorder.Eventf(backup, nil, corev1.EventTypeWarning, "UnfreezeFailed", "RunPostHooks",
			"the post-backup release could not be attempted after %d tries on Backup %s/%s — an "+
				"application may remain quiesced and needs manual attention; the hooks could not be "+
				"resolved: %v",
			backup.Status.PostHookAttempts, backup.Namespace, backup.Name, cause)
		return
	}
	// Deliberately NOT worded as a promise. Another attempt follows only while the run is still in
	// flight, and the release fires on the snapshots being CUT — which on a run whose volumes all went
	// Skipped or Failed is the same pass that reaches a terminal phase, after which the
	// already-terminal short-circuit means there is no later pass to retry in. That gap predates this
	// change (an EXEC failure on the last pass has always been able to land in it) and is not this
	// lot's to close; what is this lot's is to not print "retrying" over it.
	r.Recorder.Eventf(backup, nil, corev1.EventTypeWarning, "HookFailed", "RunPostHooks",
		"the post-backup release could not be attempted (try %d of %d): %v — another attempt follows "+
			"while this run is still in flight; if the run ends first, an application may remain quiesced",
		backup.Status.PostHookAttempts, postHookMaxAttempts, cause)
}

// resolveHooks lists the pods that MOUNT the backed-up volumes and resolves the phase's hooks
// against them.
//
// The pod set is the security boundary as much as the semantic one: it is built by listing pods in
// the Backup's OWN namespace and keeping those referencing one of its PVCs, so a hook can never
// reach a pod elsewhere, and never reaches a pod that holds none of the data being captured
// (03-security-and-tenancy.md §5).
func (r *BackupReconciler) resolveHooks(ctx context.Context, backup *cbv1.Backup, spec cbv1.HooksSpec,
	volumes []cbv1.VolumeStatus, phase hooks.Phase,
) ([]hooks.Resolved, error) {
	if !hookPhaseDeclared(spec, phase) {
		return nil, nil // nothing this phase could run: skip the pod listing entirely
	}
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(backup.Namespace)); err != nil {
		return nil, fmt.Errorf("list pods for hooks in %s: %w", backup.Namespace, err)
	}
	claimed := map[string]struct{}{}
	for i := range volumes {
		claimed[volumes[i].Pvc] = struct{}{}
	}
	mounting := make([]corev1.Pod, 0, len(pods.Items))
	for i := range pods.Items {
		if podMountsAny(&pods.Items[i], claimed) {
			mounting = append(mounting, pods.Items[i])
		}
	}
	return hooks.Resolve(mounting, spec, phase), nil
}

// hookPhaseDeclared reports whether THIS PHASE could possibly have a hook to run — and it is
// per-phase rather than per-run, which is a third instance of the same defect class as A and B.
//
// The test used to be "does this run declare any hook at all", so the PRE phase of a run declaring
// only POST hooks listed pods, resolved zero hooks, and dropped the answer. Harmless while nothing
// downstream cared — except that the listing can FAIL, and a failed resolution in the pre phase is
// routed to failHooks (see openFreezeWindow, and the long argument there for why that is the right
// door). A post-only run in a namespace whose pods the operator may not list therefore died with
// "the pre-backup quiesce could not be performed", terminally, over a quiesce it had never asked
// for — one more thing the controller said that was not true. It also spent a pods-list per
// reconcile pass for the whole run, which is the smaller half of the same mistake.
//
// honorAnnotations makes BOTH phases possible whatever the spec lists: the commands live on the
// pods, and only a listing can discover them.
func hookPhaseDeclared(spec cbv1.HooksSpec, phase hooks.Phase) bool {
	if spec.HonorAnnotations {
		return true
	}
	if phase == hooks.PhasePost {
		return len(spec.Post) > 0
	}
	return len(spec.Pre) > 0
}

// podMountsAny reports whether a pod references one of the named PVCs.
func podMountsAny(pod *corev1.Pod, claims map[string]struct{}) bool {
	for i := range pod.Spec.Volumes {
		src := pod.Spec.Volumes[i].PersistentVolumeClaim
		if src == nil {
			continue
		}
		if _, ok := claims[src.ClaimName]; ok {
			return true
		}
	}
	return false
}

// recordHookResults appends one status entry per result and emits the Events 05-observability §5
// specifies (HookExecuted / HookFailed on the Backup).
//
// IT IS SHARED BY BOTH PHASES, which is why the OnError it records had to be fixed for both at once.
// This function is where defect A actually lived: it wrote `Failed` for every failure whatever the
// policy said, so the user's answer to "what should happen if this fails" was discarded at the exact
// moment it was being written down — and everything downstream that needed it (hookState for the
// pre phase, postHooksRan for the post one) had no choice but to assume the strictest reading.
func (r *BackupReconciler) recordHookResults(backup *cbv1.Backup, results []hooks.Result) {
	now := metav1.Now()
	for _, res := range results {
		entry := cbv1.HookStatus{
			Phase:     string(res.Hook.Phase),
			Pod:       res.Hook.Pod.Name,
			Container: res.Hook.Container,
			Source:    string(res.Hook.Source),
			Result:    cbv1.HookSucceeded,
			// Recorded on EVERY entry, not only the failed ones. It costs nothing, it keeps the
			// record uniform, and it means a reader never has to wonder whether an empty policy on a
			// succeeded entry is "Fail" or "this operator did not record policies".
			OnError:    res.Hook.OnError,
			FinishedAt: &now,
		}
		switch {
		case res.Err == nil:
			r.Recorder.Eventf(backup, nil, corev1.EventTypeNormal, "HookExecuted", "RunHooks",
				"%s-backup hook succeeded in pod %s [%s]", res.Hook.Phase, res.Hook.Pod.Name, res.Hook.Container)
		case isSkipped(res):
			entry.Result = cbv1.HookSkipped
			entry.Message = res.Err.Error()
		default:
			entry.Result = cbv1.HookFailed
			entry.Message = truncate(res.Err.Error(), hookMessageLimit)
			r.Recorder.Eventf(backup, nil, corev1.EventTypeWarning, "HookFailed", "RunHooks",
				"%s-backup hook failed in pod %s [%s]: %v", res.Hook.Phase, res.Hook.Pod.Name, res.Hook.Container, res.Err)
		}
		backup.Status.Hooks = append(backup.Status.Hooks, entry)
	}
}

// isSkipped reports whether a result is the "an earlier hook aborted the phase" marker.
func isSkipped(res hooks.Result) bool {
	return res.Err != nil && res.Err == hooks.ErrSkipped //nolint:errorlint // sentinel identity, never wrapped
}

// failedResults counts genuine failures, not the skipped ones behind them. It counts TOLERATED
// failures too, and that is right for its one caller: the span records what happened, and a hook
// that failed under onError=Continue did fail. A trace that hid it would send the next reader
// looking for a hook execution that has no span error while its command exited non-zero.
func failedResults(results []hooks.Result) int {
	n := 0
	for _, res := range results {
		if res.Err != nil && !isSkipped(res) {
			n++
		}
	}
	return n
}

// untoleratedResults counts the failures whose policy did NOT say to survive them — the ones that
// still owe somebody something. It is the count a decision hangs off (retry the release? page a
// human?), where failedResults is the count a record hangs off.
//
// Result.Aborts() is the single rule, reused rather than re-derived: it is the same predicate
// hooks.Run stops the chain on, and two implementations of "does this failure count" is how the
// policy came to be honoured in one place and dropped in three others.
func untoleratedResults(results []hooks.Result) int {
	n := 0
	for _, res := range results {
		if res.Aborts() && !isSkipped(res) {
			n++
		}
	}
	return n
}

// dropHookPhase removes one phase's entries, so a retried phase replaces its record rather than
// appending to it.
func dropHookPhase(entries []cbv1.HookStatus, phase hooks.Phase) []cbv1.HookStatus {
	out := entries[:0]
	for _, e := range entries {
		if e.Phase != string(phase) {
			out = append(out, e)
		}
	}
	return out
}
