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
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
	"github.com/CrystalBackup/CrystalBackup/internal/status"
)

// ---------------------------------------------------------------------------
// THE ABANDONED-RUN DECISION — the second half of the Forbid overlap gate.
//
// THE INCIDENT. A nightly ClusterBackup (nightly-20260808-040000) fanned out over 33 namespaces.
// One PVC in one namespace named a StorageClass that did not exist, so exposer resolution failed
// every pass, the volume never left Pending, and the run never reached a terminal phase. It stayed
// Running for THIRTY-ONE HOURS. Its schedule's concurrencyPolicy is Forbid, and the gate asked one
// question — "is a previous run non-terminal?" — so every subsequent nightly was skipped. A cluster
// produced no backups at all for a day and a half because of a defect affecting one PVC.
//
// The old gate is right when the previous run is genuinely working and catastrophic when it is
// wedged, and it cannot tell the two apart. This file is what tells them apart:
//
//	N-1 still progressing  ->  skip run N          (unchanged; the whole point of Forbid)
//	N-1 abandoned          ->  terminate N-1, run N
//
// WHY THE PREDICATE IS THE HARD PART, AND WHAT IT IS NOT. "Not finished yet" is not evidence of
// abandonment. A first full backup of a multi-terabyte volume legitimately uploads for many hours
// with nothing visibly changing, and killing it destroys real work and re-uploads terabytes — a
// worse defect than the hang. So there is NO wall-clock cap on a run anywhere in this file. The
// elapsed-time term that does appear (scheduleAbandonmentGrace) can only DELAY a kill that the
// evidence has already authorised; it can never authorise one on its own.
//
// The evidence is the per-phase deadlines the Backup controller already enforces
// (backup_controller.go): pendingResolveDeadline (1h) bounds Pending, snapshotProgressDeadline
// (15m) and snapshotReadyDeadline (2h) bound Snapshotting, moverStartDeadline (30m) bounds a mover
// that never started. Each of those FAILS the volume itself, on a clock that belongs to the object
// being waited on. This file therefore does not re-derive them and must never try to: it asks only
// whether any part of the run is in a phase that somebody else is still legitimately bounding.
//
// Uploading is the load-bearing case and it is treated as ALWAYS in flight. That is deliberate and
// it is the clause that makes a legitimately slow multi-terabyte upload un-killable: once a mover
// pod is Running, nothing in this product bounds it, by design (see moverStartDeadline's doc for
// the asymmetry). A run with one volume in Uploading is never abandoned, however long it has been
// there, however little else is happening.
// ---------------------------------------------------------------------------

const (
	// scheduleAbandonmentGrace is the FLOOR under the abandonment decision: a run may only be
	// terminated as abandoned once it has recorded no progress for this long. It is not a cap on how
	// long a run may take — it is only ever consulted AFTER the evidence in
	// backupInFlightReason/clusterRunInFlightReason has already established that no part of the run
	// is legitimately in flight. On its own it can never kill anything.
	//
	// FOUR HOURS, and the number is derived rather than picked. The longest a single volume can
	// legitimately hold a run without any observable progress is the full ladder of the controller's
	// own bounds walked end to end: pendingResolveDeadline (1h) + snapshotReadyDeadline (2h) +
	// moverStartDeadline (30m) = 3h30m, after which the controller has failed that volume itself and
	// the run has moved. Four hours is that ladder plus room, so a run still inside the mechanism can
	// never be judged outside it — while a nightly schedule still gets its next run the same night
	// instead of on the second morning.
	//
	// The clock is not the run's age. It is scheduleLastProgressAt: the newest durable timestamp the
	// run itself wrote. A run that is working through fifty broken volumes at one failure per hour
	// advances that clock every hour and is never a candidate, even after fifty hours.
	scheduleAbandonmentGrace = 4 * time.Hour

	// reasonTerminatedAsAbandoned is the status reason a run — and every volume of it that was still
	// non-terminal — carries when the schedule killed it to unblock the next tick.
	//
	// It exists so an administrator can tell "we killed a stuck run" from "the run failed on its
	// own", which are different incidents with different next steps: the first says the run stopped
	// making progress and the schedule reclaimed the slot, the second says a specific volume or
	// repository reported a specific fault. Where a volume already had a cause of its own recorded
	// (backupReasonExposerUnresolvable and friends), that cause is PRESERVED after this prefix —
	// naming the clock is worth much less than naming the fault.
	reasonTerminatedAsAbandoned = "TerminatedAsAbandoned"

	// eventReasonAbandonedRunTerminated is the Event reason on all three objects involved: the
	// schedule (so `kubectl describe backupschedule` explains the gap in its history), the run that
	// was killed, and — on the cluster plane — every child Backup that was killed with it.
	eventReasonAbandonedRunTerminated = "AbandonedRunTerminated"

	// eventReasonConcurrencySkip is the Event reason for the OTHER branch: a previous run that is
	// genuinely progressing, so this tick is skipped exactly as before. Its note now carries the
	// evidence of progress, which is the question an operator staring at a skipped tick actually has.
	eventReasonConcurrencySkip = "ConcurrencySkip"
)

// ---------------------------------------------------------------------------
// The predicate — pure functions over an object's own status. No client, no clock of our own.
// ---------------------------------------------------------------------------

// volumeInFlightReason answers, for ONE volume of a run, "is somebody still legitimately working on
// this?". A non-empty return is the human-readable reason it counts as in flight; "" means the
// volume is settled or out of time and is no obstacle to killing the run.
//
// Read the cases in the order they are written, because each one encodes a decision:
//
//   - Terminal (Completed / Skipped / Failed): settled. Not in flight, and nothing about it can be
//     lost — a Completed volume keeps its snapshot id through the kill.
//   - Uploading: ALWAYS in flight. This is the clause that protects a legitimately slow upload, and
//     it is unconditional on purpose. Once the mover pod is Running nothing bounds it (by design),
//     and the case where it never started is bounded by moverStartDeadline in the controller that
//     owns it. Neither outcome is ours to second-guess, and guessing wrong here destroys terabytes.
//   - Snapshotting: ALWAYS in flight, for the same reason one level up. snapshotReadyDeadline is TWO
//     HOURS because a ceph-csi flatten or a cloud-disk snapshot of a multi-terabyte volume can
//     legitimately take that long, and the controller fails the volume itself at the bound. A
//     schedule that judged Snapshotting would be re-litigating that bound with less evidence (it
//     cannot see the origin VolumeSnapshot's clock from here).
//   - Pending: the ONE phase this function judges, and the only one it can, because Pending is the
//     one phase whose clock lives on the volume itself (firstAttemptAt — see its doc: the other
//     three deadlines hang off an object that has its own creationTimestamp, a Pending volume has
//     created nothing). A volume never attempted (firstAttemptAt nil) is QUEUED, not stuck: volumes
//     advance one per reconcile, so most of a large namespace sits there legitimately, and treating
//     it as evidence would make every wide backup killable.
//
// Note what a "" for a Pending volume past its deadline really means: the controller is about to
// fail that volume on its own, on the very next pass. This function agreeing is not a race — the
// two conclusions are the same conclusion, and if the controller gets there first the run goes
// terminal and this whole path is never entered.
//
// ON THE QUOTES AROUND THE PVC NAME. They are not decoration and they are not free to drop. This
// string travels into an Event, into a status message, and — for a cluster running the soak
// collector — into an archive that LEAVES the cluster and goes to a maintainer. The name itself
// stays: the Event is addressed to the administrator of the cluster the volume lives on, their own
// PVC name is not a secret from them, and naming which volume is holding the run is the entire
// diagnostic value of the message. What makes the same sentence safe to export is that
// internal/selfcheck's RedactQuoted substitutes an identifier out of free text only where it is
// QUOTED and matches exactly — so `"data"` is tokenised and the English word `data` beside it is
// not. The convention was already here for object names: both ConcurrencySkip emitters
// (backupschedule_controller.go, clusterbackupschedule_controller.go) already write the previous
// run's name with %q, which is why the run in the leaking message came out tokenised while the PVC
// beside it did not. These four sites are that convention made uniform, not a new one.
func volumeInFlightReason(v *cbv1.VolumeStatus, now time.Time, pendingDeadline time.Duration) string {
	switch v.Phase {
	case status.VolumePhaseCompleted, status.VolumePhaseSkipped, status.VolumePhaseFailed:
		return ""
	case status.VolumePhaseUploading:
		return "PVC " + quoteName(v.Pvc) + " is Uploading (a running mover is never bounded by this gate)"
	case status.VolumePhaseSnapshotting:
		return "PVC " + quoteName(v.Pvc) + " is Snapshotting (bounded by the controller's own snapshot deadlines)"
	default: // Pending, or the empty phase a freshly enumerated volume carries.
		if v.FirstAttemptAt == nil {
			return "PVC " + quoteName(v.Pvc) + " is queued and has not been attempted yet"
		}
		if waited := now.Sub(v.FirstAttemptAt.Time); waited <= pendingDeadline {
			return fmt.Sprintf("PVC %s has been Pending for %s of its %s budget",
				quoteName(v.Pvc), waited.Round(time.Second), pendingDeadline)
		}
		return ""
	}
}

// quoteName wraps an identifier in the double quotes the redaction convention keys on.
//
// Plain concatenation rather than %q: a Kubernetes object name is RFC 1123 and has nothing in it
// for %q to escape, while %q on a name that somehow did would emit `\"` — which is exactly the
// shape RedactQuoted's span pairing cannot read, turning a redactable identifier into a missed one.
// Being unable to escape is the property we want here.
func quoteName(name string) string { return `"` + name + `"` }

// backupInFlightReason returns the first reason ANY volume of a Backup is still legitimately in
// flight, or "" when not one of them is. It is the whole of the evidence half of the predicate for
// the namespace plane, and one volume is enough to veto the kill: a run holding thirty-two healthy
// namespaces' worth of finished volumes and one live upload is a run that is working.
//
// A Backup with NO volumes returns "" — vacuously nothing in flight — and that is the case the
// incident's cluster-plane sibling actually needs: a run gated before ensureVolumes ever ran (its
// location absent, its repository not initialized, no run spec resolvable) has enumerated nothing
// and produced nothing, so there is provably no work to lose. The grace period is what keeps that
// from firing on a run whose repository is merely still initializing.
func backupInFlightReason(b *cbv1.Backup, now time.Time, pendingDeadline time.Duration) string {
	for i := range b.Status.Volumes {
		if reason := volumeInFlightReason(&b.Status.Volumes[i], now, pendingDeadline); reason != "" {
			return reason
		}
	}
	return ""
}

// backupLastProgressAt is the run's PROGRESS clock: the newest instant at which this Backup durably
// recorded that something happened to it. It is deliberately assembled from fields that already
// exist — no new CRD field, and every one of them survives an operator restart:
//
//   - creationTimestamp, the floor: a run that has never been reconciled has still existed since
//     then, and a gated run with no status at all has nothing else to offer.
//   - the Ready condition's lastTransitionTime. meta.SetStatusCondition moves it only on a real
//     Status change (see status.SetCondition), so for a non-terminal run it marks when the run
//     first reported in — which for the gated case IS its start.
//   - every volume's firstAttemptAt. This is the term that makes a wide, slowly-failing namespace
//     safe: each volume the controller picks up stamps a fresh instant, so a run grinding through
//     fifty unresolvable PVCs at one per hour keeps proving it is alive.
//
// Using the maximum, not the minimum, is the point: the question is "when did this run last show a
// sign of life", and any one sign is enough.
func backupLastProgressAt(b *cbv1.Backup) time.Time {
	latest := b.CreationTimestamp.Time
	if c := status.FindCondition(b.Status.Conditions, ConditionReady); c != nil && c.LastTransitionTime.After(latest) {
		latest = c.LastTransitionTime.Time
	}
	for i := range b.Status.Volumes {
		if t := b.Status.Volumes[i].FirstAttemptAt; t != nil && t.After(latest) {
			latest = t.Time
		}
	}
	return latest
}

// backupAbandonment is the namespace plane's verdict on a non-terminal Backup. It returns the
// EVIDENCE it used — which travels verbatim into the Event and the status message, because "we
// killed it" without the grounds is exactly the unactionable report this whole lot exists to stop —
// and whether that evidence is sufficient.
//
// Both halves are required, in this order:
//
//  1. no part of the run is legitimately in flight (backupInFlightReason);
//  2. and the run has recorded no progress for scheduleAbandonmentGrace.
//
// Drop (1) and this becomes a wall-clock cap on backups. Drop (2) and a run gated for five seconds
// on a repository that is still initializing gets killed on the next tick. Neither is shippable.
func backupAbandonment(b *cbv1.Backup, now time.Time, pendingDeadline, grace time.Duration) (string, bool) {
	if inFlight := backupInFlightReason(b, now, pendingDeadline); inFlight != "" {
		return inFlight, false
	}
	last := backupLastProgressAt(b)
	quiet := now.Sub(last)
	if quiet <= grace {
		return fmt.Sprintf("no volume is in flight, but the run last recorded progress %s ago (grace %s)",
			quiet.Round(time.Second), grace), false
	}
	return fmt.Sprintf("no volume of it is in flight (%d enumerated) and it has recorded no progress "+
		"since %s — %s ago, past the %s grace",
		len(b.Status.Volumes), last.UTC().Format(time.RFC3339), quiet.Round(time.Second), grace), true
}

// ---------------------------------------------------------------------------
// The kill — namespace plane.
// ---------------------------------------------------------------------------

// terminateAbandonedBackup ends one abandoned Backup and hands its residue to the teardown the
// codebase already has.
//
// IT DELETES NOTHING ITSELF, and that is the design. It fails the still-non-terminal volumes and
// writes a terminal phase; the Backup controller's already-terminal short-circuit then runs
// ensureTerminalTeardown on the very next pass, which is the RELIABLE, re-entrant half of teardown:
// it re-runs cleanupVolumeExposure for every volume that may have exposed, deletes the mover Job and
// its creds Secret, reclaims the manifest Job and its transient RoleBinding, and refuses to stamp
// AnnotationExposuresCleaned until exposureResidueRemains reports the VolumeSnapshot /
// VolumeSnapshotContent / temp-clone-PVC residue genuinely gone. Re-implementing any of that here
// would be a second, worse teardown that the crucible's leak check would eventually catch out.
//
// WHAT SURVIVES THE KILL. Every already-Completed volume keeps its phase, its snapshot id and its
// byte counts: those snapshots are in the repository and the object is the only thing pointing at
// them. Only volumes that were still non-terminal are failed, and each keeps its own recorded cause
// after the reasonTerminatedAsAbandoned prefix.
//
// ON THE SINGLE-WRITER RULE. The Backup controller is documented as the single writer of
// Backup.status and this is the one deliberate exception, on three properties:
//
//   - the write is a one-way LATCH. It moves the run to a terminal phase, and a terminal Backup is
//     never re-executed or re-opened (the short-circuit at the top of Reconcile), so there is no
//     state the two writers can oscillate between.
//   - it is resourceVersion-checked (Status().Update, not a patch). If the controller wrote between
//     our read and our write we lose with a Conflict, change nothing, and re-evaluate on the next
//     schedule pass against the status the controller actually persisted.
//   - the reverse race is equally safe. If the controller's in-flight pass writes after us it gets
//     the Conflict, requeues, re-reads a terminal object, and goes to the teardown sweep — which is
//     precisely what we want it to do.
func terminateAbandonedBackup(
	ctx context.Context, c client.Client, recorder events.EventRecorder,
	b *cbv1.Backup, evidence string,
) error {
	killed := 0
	for i := range b.Status.Volumes {
		vol := &b.Status.Volumes[i]
		switch vol.Phase {
		case status.VolumePhaseCompleted, status.VolumePhaseSkipped, status.VolumePhaseFailed:
			continue
		}
		vol.Phase = status.VolumePhaseFailed
		// Prefix, never replace. A volume that already knew why it was stuck (e.g.
		// "ExposerUnresolvable: storageclass.storage.k8s.io \"gone\" not found") is far more useful
		// to the person reading it than the fact that we gave up, so both are recorded.
		if vol.Reason == "" {
			vol.Reason = reasonTerminatedAsAbandoned
		} else {
			vol.Reason = shortReason(reasonTerminatedAsAbandoned + ": " + vol.Reason)
		}
		killed++
	}

	b.Status.Phase = abandonedTerminalPhase(b.Status.Volumes)
	// Mirror the controller's own terminal bookkeeping so downstream readers (the failure-timestamp
	// metric, the schedule's lastSuccessTime roll-up, discovery) see an ordinary terminal Backup.
	// backupTime is stamped for the same reason the controller stamps it on ANY terminal phase; the
	// roll-up only promotes it to lastSuccessTime for a SUCCESS phase, and this phase never is one.
	if b.Status.BackupTime == nil {
		now := metav1.Now()
		b.Status.BackupTime = &now
	}
	setCompletionTime(b)
	// Deliberately NOT setTerminalCondition: its reasons are Failed / PartiallyFailed, which is the
	// account of a run that failed on its own. The distinction an administrator needs is exactly the
	// one this reason draws.
	status.SetCondition(&b.Status.Conditions, ConditionReady, metav1.ConditionFalse,
		reasonTerminatedAsAbandoned,
		clampMessage("terminated by its schedule as abandoned so the next run could start: "+evidence),
		b.Generation)

	if err := c.Status().Update(ctx, b); err != nil {
		return fmt.Errorf("terminate abandoned Backup %s/%s: %w", b.Namespace, b.Name, err)
	}
	// After the write, never before: an Event announcing a kill the apiserver rejected is a fact
	// nobody can retract.
	recorder.Eventf(b, nil, corev1.EventTypeWarning, eventReasonAbandonedRunTerminated, "TerminateAbandonedRun",
		"terminated as abandoned so the next scheduled run could start: %s; %d volume(s) that were "+
			"still non-terminal were failed with reason %s (already-completed volumes keep their snapshots)",
		evidence, killed, reasonTerminatedAsAbandoned)
	return nil
}

// abandonedTerminalPhase picks the terminal phase of a killed run. A killed run is NEVER reported as
// a success, which is why this is not simply status.RollUpVolumePhases: a run whose volumes all
// completed but which hung on its manifest half or on a gate would roll up to Completed, and a
// schedule reporting lastSuccessTime for a run somebody had to shoot is the same class of lie as the
// silent hang this lot closes.
//
// So: PartiallyFailed when at least one volume did land data (that data is real and restorable, and
// the phase has to say so), Failed otherwise — including the no-volumes case, where the run was
// gated before it ever enumerated a PVC and produced nothing at all.
func abandonedTerminalPhase(volumes []cbv1.VolumeStatus) string {
	for i := range volumes {
		if volumes[i].Phase == status.VolumePhaseCompleted {
			return string(status.BackupPhasePartiallyFailed)
		}
	}
	return string(status.BackupPhaseFailed)
}

// ---------------------------------------------------------------------------
// The predicate and the kill — cluster plane.
// ---------------------------------------------------------------------------

// clusterRunInFlightReason returns the first reason a non-terminal ClusterBackup still has work
// legitimately in flight, or "" when it has none. A run fans out into one child Backup per matched
// namespace plus one cluster-manifests capture Job of its own, so both halves are asked:
//
//   - every non-terminal child is put through backupInFlightReason. ONE child with a live upload
//     vetoes the kill for the whole run, which is the fleet-scale version of the same asymmetry: a
//     33-namespace run must not be shot because 32 of its namespaces finished.
//   - the run's own cluster-manifests capture Job, if it exists and has not finished, counts as in
//     flight. It is the one piece of the run this controller can see directly, and nothing else
//     bounds it: a capture whose pod never starts is a residual hole this decision deliberately errs
//     INTO (it declines to kill) rather than out of. Closing that hole means a deadline in the
//     ClusterBackup controller, which is a different lot.
//
// Children are listed by the parent link label, and discovery PROJECTIONS are excluded: a projection
// is a materialized view of snapshots that already exist, never an execution, and its permanently
// empty phase would otherwise read as a namespace forever in flight.
func (r *ClusterBackupScheduleReconciler) clusterRunInFlightReason(
	ctx context.Context, cb *cbv1.ClusterBackup, now time.Time,
) (string, error) {
	children, err := r.runChildren(ctx, cb)
	if err != nil {
		return "", err
	}
	for i := range children {
		child := &children[i]
		if isTerminalBackupPhase(child.Status.Phase) {
			continue
		}
		if reason := backupInFlightReason(child, now, pendingResolveDeadline); reason != "" {
			return "namespace " + child.Namespace + ": " + reason, nil
		}
	}

	jobName := clusterManifestsJobName(cb.Name)
	var job batchv1.Job
	switch err := r.Get(ctx, client.ObjectKey{Namespace: r.OperatorNamespace, Name: jobName}, &job); {
	case err == nil:
		if job.Status.Succeeded == 0 && job.Status.Failed == 0 {
			return "the run's cluster-manifests capture Job " + r.OperatorNamespace + "/" + jobName +
				" has not finished", nil
		}
	case !apierrors.IsNotFound(err):
		// An unreadable Job is not evidence of anything, and the alternative branch terminates
		// somebody's backup. Report it as in flight — the same asymmetry snapshotDeadlineExceeded and
		// moverStalled use: a pass that cannot tell waits for the next one.
		return "the run's cluster-manifests capture Job could not be read: " + err.Error(), nil
	}
	return "", nil
}

// runChildren lists the child Backups of one run by the parent-link label, dropping discovery
// projections. Cluster-wide by construction: the children live in the tenant namespaces.
func (r *ClusterBackupScheduleReconciler) runChildren(ctx context.Context, cb *cbv1.ClusterBackup) ([]cbv1.Backup, error) {
	var children cbv1.BackupList
	if err := r.List(ctx, &children, client.MatchingLabels{apiconst.LabelClusterBackup: cb.Name}); err != nil {
		return nil, fmt.Errorf("list child Backups of run %s: %w", cb.Name, err)
	}
	kept := children.Items[:0]
	for _, child := range children.Items {
		if child.Annotations[apiconst.AnnotationProjected] == apiconst.AnnotationProjectedValue {
			continue
		}
		kept = append(kept, child)
	}
	return kept, nil
}

// clusterRunLastProgressAt is the run's progress clock, assembled exactly like the namespaced one
// (backupLastProgressAt) plus the two run-level facts: status.startTime, and the newest progress
// clock of any CHILD. The children matter because the run's own status stops moving once the fan-out
// is done — all the observable progress after that point is theirs.
func clusterRunLastProgressAt(cb *cbv1.ClusterBackup, children []cbv1.Backup) time.Time {
	latest := cb.CreationTimestamp.Time
	if c := status.FindCondition(cb.Status.Conditions, ConditionReady); c != nil && c.LastTransitionTime.After(latest) {
		latest = c.LastTransitionTime.Time
	}
	if t := cb.Status.StartTime; t != nil && t.After(latest) {
		latest = t.Time
	}
	for i := range children {
		if t := backupLastProgressAt(&children[i]); t.After(latest) {
			latest = t
		}
	}
	return latest
}

// clusterRunAbandonment is the cluster plane's verdict on a non-terminal ClusterBackup: the same two
// required halves as backupAbandonment, over the run and its children.
func (r *ClusterBackupScheduleReconciler) clusterRunAbandonment(
	ctx context.Context, cb *cbv1.ClusterBackup, now time.Time,
) (string, bool, error) {
	inFlight, err := r.clusterRunInFlightReason(ctx, cb, now)
	if err != nil {
		return "", false, err
	}
	if inFlight != "" {
		return inFlight, false, nil
	}
	children, err := r.runChildren(ctx, cb)
	if err != nil {
		return "", false, err
	}
	last := clusterRunLastProgressAt(cb, children)
	quiet := now.Sub(last)
	if quiet <= scheduleAbandonmentGrace {
		return fmt.Sprintf("nothing is in flight, but the run last recorded progress %s ago (grace %s)",
			quiet.Round(time.Second), scheduleAbandonmentGrace), false, nil
	}
	return fmt.Sprintf("no part of it is in flight (%d child backup(s), phase %q) and it has recorded no "+
		"progress since %s — %s ago, past the %s grace",
		len(children), cb.Status.Phase, last.UTC().Format(time.RFC3339), quiet.Round(time.Second),
		scheduleAbandonmentGrace), true, nil
}

// terminateAbandonedRun ends one abandoned ClusterBackup. The ORDER is the whole of it, and it is
// the project's standing lifecycle rule read for a run whose result is being discarded rather than
// recorded: nothing goes terminal while work is still in flight.
//
//  1. every non-terminal child Backup is terminated first, through terminateAbandonedBackup, so each
//     child's own controller runs its re-entrant teardown sweep over that namespace's exposures,
//     mover Job, creds Secret and manifest residue. Nothing here reaches into a tenant namespace.
//  2. the run's OWN residue — the cluster-manifests capture Job, its creds Secret and, first, the
//     transient cluster-wide ClusterRoleBinding that Job reads through — is reclaimed with the same
//     two derive-only calls teardownClusterManifests uses. The grant goes first for the reason
//     recorded there: a leftover Job is inert, a leftover ClusterRoleBinding is a live read of the
//     whole cluster.
//  3. only then is the run's terminal status written.
//
// The teardown running BEFORE the durable terminal write inverts the usual rule, and the inversion
// is deliberate. That rule exists because the residue is the only record of a result until status is
// persisted — here there is no result to lose, we are destroying it on purpose, and the inverted
// order is the one with no crash window: this controller is a perpetual cron, so a process that dies
// between step 2 and step 3 simply finds the same abandoned run on the next pass and re-runs both
// idempotent steps. Written the other way round, a crash in that window would leave a frozen
// terminal run with a live cluster-wide grant and nothing that ever re-entered.
func (r *ClusterBackupScheduleReconciler) terminateAbandonedRun(
	ctx context.Context, cb *cbv1.ClusterBackup, evidence string,
) error {
	children, err := r.runChildren(ctx, cb)
	if err != nil {
		return err
	}
	childEvidence := "its parent run " + cb.Name + " was terminated as abandoned: " + evidence
	for i := range children {
		child := &children[i]
		if isTerminalBackupPhase(child.Status.Phase) {
			continue
		}
		if err := terminateAbandonedBackup(ctx, r.Client, r.Recorder, child, childEvidence); err != nil {
			return err
		}
	}

	jobName := clusterManifestsJobName(cb.Name)
	if err := deleteClusterManifestBinding(ctx, r.Client, jobName); err != nil {
		// Returned, not swallowed: the grant is a cluster-wide read and the next pass must retry
		// rather than freeze the run terminal with it still standing.
		return fmt.Errorf("reclaim the cluster-manifests grant of abandoned run %s: %w", cb.Name, err)
	}
	deleteJobAndSecret(ctx, r.Client, r.OperatorNamespace, jobName)

	// PartiallyFailed when some namespaces had already succeeded, Failed otherwise — the run-level
	// reading of abandonedTerminalPhase's rule, and for the same reason: those namespaces' snapshots
	// are in the repository and the phase has to say so, while a killed run is never a success.
	if cb.Status.NamespacesSucceeded > 0 {
		cb.Status.Phase = string(status.ClusterBackupPhasePartiallyFailed)
	} else {
		cb.Status.Phase = string(status.ClusterBackupPhaseFailed)
	}
	if cb.Status.StartTime == nil {
		now := metav1.Now()
		cb.Status.StartTime = &now
	}
	if cb.Status.CompletionTime == nil {
		now := metav1.Now()
		cb.Status.CompletionTime = &now
	}
	cb.Status.Failures = status.AppendCappedFailure(cb.Status.Failures, cbv1.FailureRecord{
		Backup:  cb.Name,
		Message: clampMessage(reasonTerminatedAsAbandoned + ": " + evidence),
	}, status.DefaultFailureCap)
	status.SetCondition(&cb.Status.Conditions, ConditionReady, metav1.ConditionFalse,
		reasonTerminatedAsAbandoned,
		clampMessage("terminated by its schedule as abandoned so the next run could start: "+evidence),
		cb.Generation)

	if err := r.Status().Update(ctx, cb); err != nil {
		return fmt.Errorf("terminate abandoned ClusterBackup %s: %w", cb.Name, err)
	}
	r.Recorder.Eventf(cb, nil, corev1.EventTypeWarning, eventReasonAbandonedRunTerminated, "TerminateAbandonedRun",
		"terminated as abandoned so the next scheduled run could start: %s; %d child backup(s) were "+
			"terminated with it and their exposures torn down by their own controllers",
		evidence, len(children))
	return nil
}
