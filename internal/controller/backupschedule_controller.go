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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
	"github.com/CrystalBackup/CrystalBackup/internal/schedule"
	"github.com/CrystalBackup/CrystalBackup/internal/status"
)

// BackupScheduleReconciler is the namespace plane's cron: it stamps Backup objects into the
// user's own namespace against a namespaced BackupLocation, with no fan-out in between.
//
// It shares the cron mechanics with ClusterBackupScheduleReconciler — never fires on apply,
// deterministic jitter, Forbid-overlap with a skip Event, one-run catch-up bounded by
// startingDeadlineSeconds, single status writer, all time from an injected Clock — and differs on
// the two points where the object it stamps is not the same kind of thing:
//
//   - IT STAMPS RESTORE POINTS, NOT RUN RECORDS, so there is NO history GC. A cluster schedule
//     creates ClusterBackup records that merely describe a run, and prunes them past a limit; the
//     Backups a namespace schedule creates ARE the restore points (CR lifetime = data lifetime,
//     adr/0009 §3), and deleting one would remove a user's view of snapshots that still exist in
//     their bucket. BackupScheduleSpec accordingly has no history-limit fields, and this
//     controller never deletes a Backup.
//   - IT DOES NOT OWN WHAT IT STAMPS, for the same reason. A namespaced schedule COULD own a
//     namespaced Backup — unlike the cross-scope cases elsewhere in this package, the
//     ownerReference would be legal — and that is exactly the trap: `kubectl delete
//     backupschedule` would cascade and take every restore point with it. The link is
//     crystalbackup.io/schedule, and deletion of the schedule leaves the backups standing.
type BackupScheduleReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Clock is the only source of "now": clock.RealClock in production, a fake clock in tests.
	Clock    clock.PassiveClock
	Recorder events.EventRecorder
}

// NewBackupScheduleReconciler builds the reconciler, mirroring the cluster-plane constructor so
// the wiring (notably the injected clock) stays in one place.
func NewBackupScheduleReconciler(
	c client.Client,
	scheme *runtime.Scheme,
	cl clock.PassiveClock,
	recorder events.EventRecorder,
) *BackupScheduleReconciler {
	return &BackupScheduleReconciler{Client: c, Scheme: scheme, Clock: cl, Recorder: recorder}
}

// +kubebuilder:rbac:groups=crystalbackup.io,resources=backupschedules,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=crystalbackup.io,resources=backupschedules/status,verbs=get;update;patch
// patch on backups is for ONE thing: stamping crystalbackup.io/parent-uid onto a Backup this
// schedule fired before the stamp existed (the operator-upgrade adoption path in stampBackup). The
// controller never otherwise mutates a Backup — it does not even own the ones it stamps.
// +kubebuilder:rbac:groups=crystalbackup.io,resources=backups,verbs=get;list;watch;create;patch
// backups/status is for the OTHER thing, and only for it: writing the terminal phase onto a backup
// this schedule terminated as ABANDONED (schedule_abandonment.go), which is what lets the next tick
// run instead of being skipped forever behind a wedged one. The Backup controller remains the single
// writer of that subresource on every other path; see terminateAbandonedBackup for why this one
// exception is a safe one-way latch.
// +kubebuilder:rbac:groups=crystalbackup.io,resources=backups/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=crystalbackup.io,resources=backuplocations,verbs=get;list;watch

// Reconcile stamps the due Backup (if any) and refreshes the schedule's status.
func (r *BackupScheduleReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var sched cbv1.BackupSchedule
	if err := r.Get(ctx, req.NamespacedName, &sched); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	// Snapshot before mutating: this is a perpetual cron, so the status write must be skipped when
	// nothing changed or it re-triggers its own watch and spins.
	originalStatus := sched.Status.DeepCopy()

	// Paused: stamp nothing, and TOUCH NOTHING ELSE. lastSuccessTime and lastRunName survive the
	// pause untouched, which is the entire reason this field exists on a tenant-facing type — before
	// it, the only way a namespace could suspend its own backups was to delete the BackupSchedule,
	// and that took its history and its baseline tick with it (see baselineTick: a recreated
	// schedule restarts from its new creationTimestamp).
	//
	// Checked BEFORE listBackups, unlike the cluster plane which lists first: nothing below this
	// branch reads the list, and a paused schedule should not pay for a List every requeue. The
	// cluster plane keeps its ordering because gcHistory sits in the same reconcile there.
	if sched.Spec.Paused {
		return r.writeStatus(ctx, &sched, originalStatus, "Paused", metav1.ConditionFalse, "Paused",
			"schedule is paused; no backups are stamped", ctrl.Result{})
	}

	backups, err := r.listBackups(ctx, &sched)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("list backups for schedule %s/%s: %w", sched.Namespace, sched.Name, err)
	}

	cronSched, err := schedule.Parse(sched.Spec.Schedule, sched.Spec.Timezone)
	if err != nil {
		// Spec-static: surface it and wait for an edit rather than hot-looping.
		return r.writeStatus(ctx, &sched, originalStatus, "InvalidSchedule", metav1.ConditionFalse,
			"InvalidSchedule", err.Error(), ctrl.Result{})
	}

	now := r.Clock.Now()
	offset := time.Duration(0)
	if sched.Spec.Jitter {
		offset = schedule.JitterOffset(string(sched.UID), scheduleJitterWindow)
	}
	effectiveNow := now.Add(-offset)

	var deadline *time.Duration
	if sched.Spec.StartingDeadlineSeconds != nil {
		d := time.Duration(*sched.Spec.StartingDeadlineSeconds) * time.Second
		deadline = &d
	}

	stampedName := ""
	if tick, due := cronSched.DueTick(r.baselineTick(&sched, backups), effectiveNow, deadline); due {
		// The overlap gate, and it now asks TWO questions instead of one (see
		// schedule_abandonment.go for the incident and the predicate). "Is a previous backup
		// non-terminal?" is the right question only while that backup is genuinely working; a wedged
		// one used to hold this gate shut for every night that followed, forever.
		blocked, err := r.resolveOverlap(ctx, &sched, backups, tick, now)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !blocked {
			name, err := r.stampBackup(ctx, &sched, tick)
			if c := asRunNameCollision(err); c != nil {
				// The tick's name is already taken by a Backup this schedule did not stamp — almost
				// always the cluster plane's fan-out under an identically named ClusterBackupSchedule.
				// This tick's backup DID NOT RUN, and saying so is the whole point: the old code
				// returned the foreign name as if it had fired, so the tenant saw a lastRunName, a
				// green schedule, and no backup. Surfaced as a spec-static fault (the name clash is
				// not going to fix itself) rather than an error return, which would only hot-loop.
				r.Recorder.Eventf(&sched, nil, corev1.EventTypeWarning, reasonRunNameCollision, "StampBackup",
					"tick %s did NOT run: %s", tick.UTC().Format(time.RFC3339), c.Error())
				return r.writeStatus(ctx, &sched, originalStatus, reasonRunNameCollision,
					metav1.ConditionFalse, reasonRunNameCollision, clampMessage(c.Error()),
					ctrl.Result{RequeueAfter: shortRequeueInterval})
			}
			if err != nil {
				return ctrl.Result{}, err
			}
			stampedName = name
		}
	}

	nextFire := cronSched.Next(effectiveNow).Add(offset)
	requeue := clampDuration(nextFire.Sub(now), scheduleMinRequeue, scheduleMaxRequeue)
	r.applyBackupSummary(&sched, backups, stampedName, nextFire)

	return r.writeStatus(ctx, &sched, originalStatus, "Active", metav1.ConditionTrue, "Scheduled",
		"schedule active; next backup at "+nextFire.UTC().Format(time.RFC3339),
		ctrl.Result{RequeueAfter: requeue})
}

// listBackups returns the Backups this schedule stamped, found by the schedule label WITHIN the
// schedule's own namespace. The namespace scoping is not an optimisation: two tenants may both
// have a schedule called "daily", and a cluster-wide list would let one see — and count as
// "active", and derive its baseline tick from — the other's backups.
func (r *BackupScheduleReconciler) listBackups(ctx context.Context, sched *cbv1.BackupSchedule) ([]cbv1.Backup, error) {
	var backups cbv1.BackupList
	if err := r.List(ctx, &backups,
		client.InNamespace(sched.Namespace),
		client.MatchingLabels{apiconst.LabelSchedule: sched.Name}); err != nil {
		return nil, err
	}
	// ...and to THIS PLANE. The schedule label is not plane-qualified, so a cluster-DR fan-out from
	// a ClusterBackupSchedule of the SAME name stamps crystalbackup.io/schedule with that same
	// value on its child in this very namespace — as does the discovery projection that later
	// replaces it. Read as the tenant's own history, the other plane's objects poison everything
	// derived from it: baselineTick advances to a tick this schedule never fired (so it SKIPS that
	// tick, silently), activeBackup holds the Forbid gate shut behind someone else's in-flight run,
	// and lastRunName ends up naming a Backup in a repository the tenant may not be able to read.
	//
	// The filter EXCLUDES what is provably the other plane's rather than including only what is
	// provably this one's: an object with no origin label at all keeps its previous treatment,
	// so nothing predating the label silently drops out of a schedule's history.
	kept := backups.Items[:0]
	for _, b := range backups.Items {
		if b.Labels[apiconst.LabelOrigin] == apiconst.OriginCluster {
			continue
		}
		kept = append(kept, b)
	}
	return kept, nil
}

// baselineTick is the last already-fired activation: the latest of the schedule's creation time,
// its recorded lastRunName, and the newest surviving Backup's tick. Deriving it from the objects
// (not only status) makes it robust to a lost status, and on a fresh schedule it equals
// creationTimestamp — so the first fire is the first tick AFTER apply, never on apply.
//
// Unlike the cluster plane's, this baseline is durable in a stronger sense: the Backups it reads
// are never garbage-collected by this controller, so the schedule cannot forget how far it got by
// pruning its own history.
func (r *BackupScheduleReconciler) baselineTick(sched *cbv1.BackupSchedule, backups []cbv1.Backup) time.Time {
	baseline := sched.CreationTimestamp.Time
	if t, ok := parseRunTick(sched.Name, sched.Status.LastRunName); ok && t.After(baseline) {
		baseline = t
	}
	for i := range backups {
		if t, ok := parseRunTick(sched.Name, backups[i].Name); ok && t.After(baseline) {
			baseline = t
		}
	}
	return baseline
}

// stampBackup creates the Backup for tick, named deterministically so re-firing the same tick is
// a harmless AlreadyExists no-op, and MATERIALIZES the schedule's run configuration into
// spec.run (adr/0017 §5) — the only way a schedule-stamped Backup can carry its intent, since it
// has no ClusterBackup parent to pull from. The Backup is STAMPED with this schedule's UID
// (apiconst.AnnotationParentUID), which is what makes "already fired this tick" answerable.
//
// AlreadyExists used to be read as "already fired this tick" outright. It is not: run names are
// built by the SAME apiconst.RunName() on both planes, so a ClusterBackupSchedule named "daily"
// covering this namespace stamps a child of exactly this name at exactly this tick. Whichever
// plane got there first, the second silently did nothing — and on this side that meant the
// tenant's backup NEVER RAN while the schedule reported a lastRunName pointing at the other
// plane's object. A foreign occupant now returns a *runNameCollisionError, which the caller
// surfaces on the schedule.
//
// No ownerReference: see the type doc. The Backup outlives the schedule because it is a restore
// point, not a record of a run.
func (r *BackupScheduleReconciler) stampBackup(ctx context.Context, sched *cbv1.BackupSchedule, tick time.Time) (string, error) {
	name := apiconst.RunName(sched.Name, tick)
	backup := &cbv1.Backup{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: sched.Namespace,
			Name:      name,
			Labels: map[string]string{
				apiconst.LabelSchedule:  sched.Name,
				apiconst.LabelOrigin:    apiconst.OriginNamespace,
				apiconst.LabelNamespace: sched.Namespace,
			},
			Annotations: map[string]string{apiconst.AnnotationParentUID: string(sched.UID)},
		},
		Spec: cbv1.BackupSpec{
			ScheduleRef: sched.Name,
			// Always the namespaced kind. A BackupSchedule's locationRef is a BackupLocation in
			// its own namespace by construction (02-api.md; admission rule 2 states it too) —
			// pinning the kind here means nothing downstream has to infer it.
			LocationRef: cbv1.LocationReference{
				Kind: kindBackupLocation,
				Name: sched.Spec.LocationRef.Name,
			},
			Run: backupRunSpecFrom(&sched.Spec),
		},
	}
	if err := r.Create(ctx, backup); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return "", fmt.Errorf("create Backup %s/%s: %w", sched.Namespace, name, err)
		}
		var existing cbv1.Backup
		if gerr := r.Get(ctx, client.ObjectKey{Namespace: sched.Namespace, Name: name}, &existing); gerr != nil {
			return "", fmt.Errorf("re-read Backup %s/%s after AlreadyExists: %w", sched.Namespace, name, gerr)
		}
		switch owner, detail := classifyCoordinate(&existing, sched.UID); owner {
		case coordinateMine:
			return name, nil // genuinely already fired this tick.
		case coordinateAdoptable:
			// A pre-stamp build fired this tick and the Backup has not produced anything yet:
			// claim it rather than treat the upgrade as a collision (see coordinateAdoptable).
			patch := client.MergeFrom(existing.DeepCopy())
			if existing.Annotations == nil {
				existing.Annotations = map[string]string{}
			}
			existing.Annotations[apiconst.AnnotationParentUID] = string(sched.UID)
			if perr := r.Patch(ctx, &existing, patch); perr != nil {
				return "", fmt.Errorf("stamp parent UID on Backup %s/%s: %w", sched.Namespace, name, perr)
			}
			return name, nil
		default:
			return "", &runNameCollisionError{Namespace: sched.Namespace, Name: name, Detail: detail}
		}
	}
	r.Recorder.Eventf(sched, nil, corev1.EventTypeNormal, "BackupStamped", "StampBackup",
		"created Backup %q for tick %s", name, tick.UTC().Format(time.RFC3339))
	return name, nil
}

// backupRunSpecFrom projects a BackupSchedule's flat run fields into the shared BackupRunSpec.
//
// maxConcurrentMovers is deliberately absent, and its absence is the point: the cap is enforced
// against EVERY mover Job in the operator namespace, so it is a platform-wide limit. A tenant
// setting it on their own schedule would be setting it for the whole cluster (adr/0017 §5).
func backupRunSpecFrom(spec *cbv1.BackupScheduleSpec) *cbv1.BackupRunSpec {
	return &cbv1.BackupRunSpec{
		PVCSelector:      spec.PVCSelector,
		IncludeManifests: spec.IncludeManifests,
		ManifestOptions:  spec.ManifestOptions,
		Hooks:            spec.Hooks,
		BackoffLimit:     spec.BackoffLimit,
	}
}

// applyBackupSummary refreshes the run-derived status fields from the snapshot plus any Backup
// just stamped, and sets nextScheduleTime.
func (r *BackupScheduleReconciler) applyBackupSummary(sched *cbv1.BackupSchedule, backups []cbv1.Backup, stampedName string, nextFire time.Time) {
	if stampedName != "" {
		sched.Status.LastRunName = stampedName
	} else {
		var newest time.Time
		for i := range backups {
			if t, ok := parseRunTick(sched.Name, backups[i].Name); ok && t.After(newest) {
				newest = t
				sched.Status.LastRunName = backups[i].Name
			}
		}
	}
	for i := range backups {
		b := &backups[i]
		// backupTime is the point-in-time of the snapshot set, and it is only stamped once a
		// Backup reaches a terminal phase — so it is the honest "when did this last work".
		if backupSucceeded(b.Status.Phase) && b.Status.BackupTime != nil {
			if sched.Status.LastSuccessTime == nil || b.Status.BackupTime.After(sched.Status.LastSuccessTime.Time) {
				sched.Status.LastSuccessTime = b.Status.BackupTime
			}
		}
	}
	sched.Status.NextScheduleTime = &metav1.Time{Time: nextFire}
}

// writeStatus is the single status writer: phase + the headline Ready condition, persisted only
// when something actually changed so this perpetual controller does not re-trigger its own watch.
func (r *BackupScheduleReconciler) writeStatus(
	ctx context.Context, sched *cbv1.BackupSchedule, original *cbv1.BackupScheduleStatus,
	phase string, ready metav1.ConditionStatus, reason, message string, result ctrl.Result,
) (ctrl.Result, error) {
	sched.Status.Phase = phase
	status.SetCondition(&sched.Status.Conditions, ConditionReady, ready, reason, message, sched.Generation)
	if equality.Semantic.DeepEqual(original, &sched.Status) {
		return result, nil
	}
	if err := r.Status().Update(ctx, sched); err != nil {
		return ctrl.Result{}, fmt.Errorf("update status for BackupSchedule %s/%s: %w", sched.Namespace, sched.Name, err)
	}
	return result, nil
}

// SetupWithManager registers this reconciler. The Backup watch is a mapped Watch, not an Owns,
// because this controller deliberately does not own what it stamps (see the type doc) — but the
// watch still matters: it is what refreshes lastSuccessTime and releases the Forbid-overlap gate
// as a backup reaches a terminal phase, instead of waiting out the requeue.
func (r *BackupScheduleReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&cbv1.BackupSchedule{}).
		Watches(&cbv1.Backup{}, handler.EnqueueRequestsFromMapFunc(mapBackupToSchedule)).
		Named("backupschedule").
		Complete(r)
}

// mapBackupToSchedule maps a Backup back to the BackupSchedule that stamped it, in its own
// namespace. A Backup with no schedule label (a manual one, or a discovery projection) maps to
// nothing.
func mapBackupToSchedule(_ context.Context, obj client.Object) []reconcile.Request {
	name := obj.GetLabels()[apiconst.LabelSchedule]
	if name == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: client.ObjectKey{Namespace: obj.GetNamespace(), Name: name}}}
}

// resolveOverlap is the namespace plane's two-way overlap decision for a due tick. It reports
// whether the tick is BLOCKED (a previous backup is genuinely progressing, so skip exactly as
// before) and, when it is not, may first have terminated the abandoned backups that were holding the
// gate shut.
//
// The three outcomes:
//
//   - no active backup: not blocked, nothing to do. The overwhelmingly common path.
//   - at least one active backup that is still in flight: BLOCKED. The skip Event now carries the
//     EVIDENCE of progress ("PVC data-1 is Uploading"), which is the question an operator staring at
//     a skipped tick actually has, and which used to be unanswerable without reading the run.
//   - every active backup is abandoned: each is terminated (schedule_abandonment.go) and the tick
//     proceeds. One in-flight backup among several vetoes the whole decision — the gate is about
//     whether ANY previous work is live, and killing the dead ones while a live one still holds the
//     gate would buy nothing and destroy a record.
//
// The decision is applied for EVERY concurrency policy value, not only Forbid. Forbid and Skip are
// still handled identically in this build (see concurrencyPolicyForbid), so branching on the policy
// here would only mean that setting the other value silently restores a defect that took a cluster
// off backups for thirty-one hours.
func (r *BackupScheduleReconciler) resolveOverlap(
	ctx context.Context, sched *cbv1.BackupSchedule, backups []cbv1.Backup, tick, now time.Time,
) (bool, error) {
	active := activeBackups(backups)
	if len(active) == 0 {
		return false, nil
	}

	type verdict struct {
		backup   *cbv1.Backup
		evidence string
	}
	abandoned := make([]verdict, 0, len(active))
	for _, b := range active {
		evidence, isAbandoned := backupAbandonment(b, now, pendingResolveDeadline, scheduleAbandonmentGrace)
		if !isAbandoned {
			r.Recorder.Eventf(sched, nil, corev1.EventTypeWarning, eventReasonConcurrencySkip, "SkipRun",
				"skipping run for tick %s: previous backup %q is still making progress — %s "+
					"(concurrencyPolicy %s)",
				tick.UTC().Format(time.RFC3339), b.Name, evidence, backupConcurrencyPolicyOf(sched))
			return true, nil
		}
		abandoned = append(abandoned, verdict{backup: b, evidence: evidence})
	}

	for _, v := range abandoned {
		b, evidence := v.backup, v.evidence
		if err := terminateAbandonedBackup(ctx, r.Client, r.Recorder, b, evidence); err != nil {
			return false, err
		}
		// Recorded on the SCHEDULE too, and this is the copy an administrator finds first: `kubectl
		// describe backupschedule` is where they go to ask why last night has no backup, and the
		// answer — that a wedged run was shot so this one could start — belongs there rather than only
		// on an object that is now terminal history.
		r.Recorder.Eventf(sched, nil, corev1.EventTypeWarning, eventReasonAbandonedRunTerminated, "TerminateAbandonedRun",
			"tick %s: previous backup %q was TERMINATED as abandoned so this run could start — %s",
			tick.UTC().Format(time.RFC3339), b.Name, evidence)
	}
	return false, nil
}

// activeBackups returns every non-terminal Backup in the snapshot. Discovery PROJECTIONS are
// skipped: they are materialized views of snapshots that already exist, never executions, so one
// sitting at an empty phase must not be mistaken for a run in flight and block every future tick
// forever.
//
// The pointers alias backups, so a caller that mutates one (the abandoned-run kill does) is mutating
// the snapshot the rest of the reconcile reads — which is what we want: applyBackupSummary then sees
// the terminal phase it was just given.
func activeBackups(backups []cbv1.Backup) []*cbv1.Backup {
	var active []*cbv1.Backup
	for i := range backups {
		b := &backups[i]
		if b.Annotations[apiconst.AnnotationProjected] == apiconst.AnnotationProjectedValue {
			continue
		}
		if !isTerminalBackupPhase(b.Status.Phase) {
			active = append(active, b)
		}
	}
	return active
}

// backupConcurrencyPolicyOf returns the schedule's concurrency policy, defaulting to Forbid (see
// concurrencyPolicyForbid).
func backupConcurrencyPolicyOf(sched *cbv1.BackupSchedule) cbv1.ConcurrencyPolicy {
	if sched.Spec.ConcurrencyPolicy == "" {
		return concurrencyPolicyForbid
	}
	return sched.Spec.ConcurrencyPolicy
}
