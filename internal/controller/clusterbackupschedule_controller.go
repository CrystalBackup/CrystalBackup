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
	"slices"
	"strings"
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
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
	"github.com/CrystalBackup/CrystalBackup/internal/schedule"
	"github.com/CrystalBackup/CrystalBackup/internal/status"
)

const (
	// scheduleJitterWindow bounds the deterministic per-schedule firing offset (spec.jitter): a
	// modest spread that de-synchronises many schedules sharing a cron tick without materially
	// delaying any single run.
	scheduleJitterWindow = 60 * time.Second

	// scheduleMinRequeue / scheduleMaxRequeue clamp the time-to-next-tick requeue. The upper
	// bound keeps a sparse (e.g. daily) schedule re-checking often enough that a live every-minute
	// schedule fires promptly; the lower bound avoids a hot loop when a tick was just skipped.
	scheduleMinRequeue = 5 * time.Second
	scheduleMaxRequeue = 60 * time.Second

	// defaultRunsHistoryLimit is the fallback kept-run count when a history limit is unset (the CRD
	// defaults both to 10; this guards objects that predate the default so a 0 never means "delete
	// all history").
	defaultRunsHistoryLimit = 10
)

// ClusterBackupScheduleReconciler reconciles a ClusterBackupSchedule: a CronJob-style stamp that
// creates ClusterBackup DR runs named "<schedule>-<UTC-timestamp>" from a template at each cron
// activation. It never fires on apply (the first run is the first tick strictly after creation),
// honours paused/timezone/deterministic jitter, forbids overlapping runs (skip + Event), bounds
// post-downtime catch-up to a single run via startingDeadlineSeconds, and garbage-collects old run
// RECORDS past the history limits — never the label-linked child Backups those runs produced, which
// are restore points whose lifetime is the data's, not the schedule's (adr/0009).
//
// It is the single writer of ClusterBackupSchedule.status. Time comes exclusively from Clock so the
// whole schedule can be driven deterministically by a fake clock in envtest.
type ClusterBackupScheduleReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Clock is the only source of "now": clock.RealClock in production, a fake clock in tests.
	Clock    clock.PassiveClock
	Recorder events.EventRecorder
}

// NewClusterBackupScheduleReconciler builds the reconciler. Callers go through this constructor to
// keep the wiring (notably the injected clock) in one place, mirroring the sibling reconcilers.
func NewClusterBackupScheduleReconciler(
	c client.Client,
	scheme *runtime.Scheme,
	cl clock.PassiveClock,
	recorder events.EventRecorder,
) *ClusterBackupScheduleReconciler {
	return &ClusterBackupScheduleReconciler{Client: c, Scheme: scheme, Clock: cl, Recorder: recorder}
}

// +kubebuilder:rbac:groups=crystalbackup.io,resources=clusterbackupschedules,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=crystalbackup.io,resources=clusterbackupschedules/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=crystalbackup.io,resources=clusterbackups,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups=crystalbackup.io,resources=clusterbackuplocations,verbs=get;list;watch

// Reconcile stamps out the due run (if any), garbage-collects old run records, and refreshes the
// schedule's status.
func (r *ClusterBackupScheduleReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var sched cbv1.ClusterBackupSchedule
	if err := r.Get(ctx, req.NamespacedName, &sched); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	// Snapshot the status before any mutation: this controller is a perpetual cron (it never
	// reaches a terminal, write-once state like the run controllers), so writeStatus persists only
	// when the status actually changed — otherwise its own status-subresource write would re-trigger
	// the watch and spin a hot reconcile loop.
	originalStatus := sched.Status.DeepCopy()

	// The run records this schedule owns — listed once and reused for baseline, concurrency, GC and
	// status, so the whole reconcile works off one consistent snapshot.
	runs, err := r.listRuns(ctx, &sched)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("list runs for schedule %s: %w", sched.Name, err)
	}

	if sched.Spec.Paused {
		return r.writeStatus(ctx, &sched, originalStatus, "Paused", metav1.ConditionFalse, "Paused",
			"schedule is paused; no runs are stamped", nil, ctrl.Result{})
	}

	cronSched, err := schedule.Parse(sched.Spec.Schedule, sched.Spec.Timezone)
	if err != nil {
		// A cron/timezone error is spec-static: surface it and wait for an edit (generation change
		// re-triggers), rather than hot-looping.
		return r.writeStatus(ctx, &sched, originalStatus, "InvalidSchedule", metav1.ConditionFalse, "InvalidSchedule",
			err.Error(), nil, ctrl.Result{})
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

	// Fire the single due tick, if any, unless a previous run is still active.
	stampedName := ""
	if tick, due := cronSched.DueTick(r.baselineTick(&sched, runs), effectiveNow, deadline); due {
		if active := activeRun(runs); active != nil {
			r.Recorder.Eventf(&sched, nil, corev1.EventTypeWarning, "ConcurrencySkip", "SkipRun",
				"skipping run for tick %s: previous run %q still active (concurrencyPolicy %s)",
				tick.UTC().Format(time.RFC3339), active.Name, concurrencyPolicyOf(&sched))
		} else {
			name, err := r.stampRun(ctx, &sched, tick)
			if err != nil {
				return ctrl.Result{}, err
			}
			stampedName = name
		}
	}

	// GC old run RECORDS (never their label-linked child Backups).
	r.gcHistory(ctx, &sched, runs)

	// Compute the next (jittered) activation and requeue to fire it.
	nextFire := cronSched.Next(effectiveNow).Add(offset)
	requeue := clampDuration(nextFire.Sub(now), scheduleMinRequeue, scheduleMaxRequeue)
	r.applyRunSummary(&sched, runs, stampedName, nextFire)

	return r.writeStatus(ctx, &sched, originalStatus, "Active", metav1.ConditionTrue, "Scheduled",
		"schedule active; next run at "+nextFire.UTC().Format(time.RFC3339),
		&metav1.Time{Time: nextFire}, ctrl.Result{RequeueAfter: requeue})
}

// listRuns returns the ClusterBackup run records this schedule stamped, found by the schedule label.
func (r *ClusterBackupScheduleReconciler) listRuns(ctx context.Context, sched *cbv1.ClusterBackupSchedule) ([]cbv1.ClusterBackup, error) {
	var runs cbv1.ClusterBackupList
	if err := r.List(ctx, &runs, client.MatchingLabels{apiconst.LabelSchedule: sched.Name}); err != nil {
		return nil, err
	}
	return runs.Items, nil
}

// baselineTick is the last already-fired activation: the latest of the schedule's creation time,
// its recorded lastRunName, and the newest surviving run's tick. Deriving it from the runs (not only
// status) makes it robust to a lost status — and, because it is at most one period behind now in
// steady state, keeps DueTick's forward scan short. On a fresh schedule it equals creationTimestamp,
// so the first fire is the first tick AFTER apply (never on apply).
func (r *ClusterBackupScheduleReconciler) baselineTick(sched *cbv1.ClusterBackupSchedule, runs []cbv1.ClusterBackup) time.Time {
	baseline := sched.CreationTimestamp.Time
	if t, ok := parseRunTick(sched.Name, sched.Status.LastRunName); ok && t.After(baseline) {
		baseline = t
	}
	for i := range runs {
		if t, ok := parseRunTick(sched.Name, runs[i].Name); ok && t.After(baseline) {
			baseline = t
		}
	}
	return baseline
}

// stampRun creates the run for tick, named deterministically so re-firing the same tick is a
// harmless AlreadyExists no-op. The run is owned by the schedule (both cluster-scoped — a legal
// ownerReference), which drives the Owns() re-reconcile on run status changes and cascades run
// records (not their label-linked children) on schedule deletion.
func (r *ClusterBackupScheduleReconciler) stampRun(ctx context.Context, sched *cbv1.ClusterBackupSchedule, tick time.Time) (string, error) {
	name := apiconst.RunName(sched.Name, tick)
	run := &cbv1.ClusterBackup{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: map[string]string{apiconst.LabelSchedule: sched.Name},
		},
		Spec: cbv1.ClusterBackupSpec{
			ScheduleRef:          sched.Name,
			ClusterBackupRunSpec: sched.Spec.Template.Spec,
		},
	}
	if err := controllerutil.SetControllerReference(sched, run, r.Scheme); err != nil {
		return "", fmt.Errorf("set owner reference on run %s: %w", name, err)
	}
	if err := r.Create(ctx, run); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return name, nil // already fired this tick.
		}
		return "", fmt.Errorf("create run %s: %w", name, err)
	}
	r.Recorder.Eventf(sched, nil, corev1.EventTypeNormal, "RunStamped", "StampRun",
		"created ClusterBackup run %q for tick %s", name, tick.UTC().Format(time.RFC3339))
	return name, nil
}

// gcHistory deletes run RECORDS beyond the successful/failed history limits, oldest first. It never
// touches a non-terminal run, and never touches the child Backups a run produced: deleting a
// ClusterBackup does not cascade to its label-linked (not owned) children, so restore points
// outlive the run-history pruning that is only about bounding the number of record objects.
func (r *ClusterBackupScheduleReconciler) gcHistory(ctx context.Context, sched *cbv1.ClusterBackupSchedule, runs []cbv1.ClusterBackup) {
	var successful, failed []cbv1.ClusterBackup
	for i := range runs {
		switch status.ClusterBackupPhase(runs[i].Status.Phase) {
		case status.ClusterBackupPhaseCompleted:
			successful = append(successful, runs[i])
		case status.ClusterBackupPhaseFailed, status.ClusterBackupPhasePartiallyFailed:
			failed = append(failed, runs[i])
		}
	}
	r.trimHistory(ctx, sched, successful, historyLimit(sched.Spec.SuccessfulRunsHistoryLimit))
	r.trimHistory(ctx, sched, failed, historyLimit(sched.Spec.FailedRunsHistoryLimit))
}

// trimHistory deletes all but the newest keep records in group. Ordering is by the run's
// ACTIVATION time (the tick encoded in its name), not the object's creationTimestamp: the tick is
// the run's logical identity, is always distinct between runs, and — unlike creationTimestamp, which
// k8s truncates to whole seconds — gives a deterministic order even for runs created within the same
// second (as backfilled history is). It falls back to creationTimestamp then name so the sort is a
// total order for any object.
func (r *ClusterBackupScheduleReconciler) trimHistory(ctx context.Context, sched *cbv1.ClusterBackupSchedule, group []cbv1.ClusterBackup, keep int) {
	if len(group) <= keep {
		return
	}
	log := logf.FromContext(ctx)
	slices.SortFunc(group, func(a, b cbv1.ClusterBackup) int {
		ta, oka := parseRunTick(sched.Name, a.Name)
		tb, okb := parseRunTick(sched.Name, b.Name)
		if oka && okb && !ta.Equal(tb) {
			if ta.After(tb) {
				return -1 // newer activation first
			}
			return 1
		}
		if !a.CreationTimestamp.Time.Equal(b.CreationTimestamp.Time) {
			if a.CreationTimestamp.After(b.CreationTimestamp.Time) {
				return -1
			}
			return 1
		}
		switch {
		case a.Name > b.Name:
			return -1
		case a.Name < b.Name:
			return 1
		default:
			return 0
		}
	})
	for i := keep; i < len(group); i++ {
		old := group[i]
		if err := r.Delete(ctx, &old); err != nil && !apierrors.IsNotFound(err) {
			log.Error(err, "run-history GC: delete old run record failed", "run", old.Name)
			continue
		}
		r.Recorder.Eventf(sched, nil, corev1.EventTypeNormal, "RunHistoryGC", "PruneRunHistory",
			"deleted old run record %q (history limit %d); its child backups are unaffected", old.Name, keep)
	}
}

// applyRunSummary refreshes the run-derived status fields (lastRunName, lastSuccessTime) from the
// snapshot of runs plus any run just stamped, and sets nextScheduleTime.
func (r *ClusterBackupScheduleReconciler) applyRunSummary(sched *cbv1.ClusterBackupSchedule, runs []cbv1.ClusterBackup, stampedName string, nextFire time.Time) {
	if stampedName != "" {
		sched.Status.LastRunName = stampedName
	} else {
		var newest time.Time
		for i := range runs {
			if t, ok := parseRunTick(sched.Name, runs[i].Name); ok && t.After(newest) {
				newest = t
				sched.Status.LastRunName = runs[i].Name
			}
		}
	}
	for i := range runs {
		run := &runs[i]
		if run.Status.Phase == string(status.ClusterBackupPhaseCompleted) && run.Status.CompletionTime != nil {
			if sched.Status.LastSuccessTime == nil || run.Status.CompletionTime.After(sched.Status.LastSuccessTime.Time) {
				sched.Status.LastSuccessTime = run.Status.CompletionTime
			}
		}
	}
	sched.Status.NextScheduleTime = &metav1.Time{Time: nextFire}
}

// writeStatus is the single status writer: it sets phase, the headline Ready condition, and
// (optionally) nextScheduleTime, then persists status once — but ONLY if it actually changed from
// original — and returns the caller's requeue. Skipping the no-op write keeps this perpetual
// controller from re-triggering its own watch and spinning.
func (r *ClusterBackupScheduleReconciler) writeStatus(
	ctx context.Context, sched *cbv1.ClusterBackupSchedule, original *cbv1.ClusterBackupScheduleStatus,
	phase string, ready metav1.ConditionStatus, reason, message string,
	nextScheduleTime *metav1.Time, result ctrl.Result,
) (ctrl.Result, error) {
	sched.Status.Phase = phase
	if nextScheduleTime != nil {
		sched.Status.NextScheduleTime = nextScheduleTime
	}
	status.SetCondition(&sched.Status.Conditions, ConditionReady, ready, reason, message, sched.Generation)
	if equality.Semantic.DeepEqual(original, &sched.Status) {
		return result, nil
	}
	if err := r.Status().Update(ctx, sched); err != nil {
		return ctrl.Result{}, fmt.Errorf("update status for ClusterBackupSchedule %s: %w", sched.Name, err)
	}
	return result, nil
}

// SetupWithManager registers this reconciler. It owns the ClusterBackup runs it stamps (both are
// cluster-scoped, a legal ownerReference), so Owns re-reconciles the schedule when a run's status
// changes — refreshing lastSuccessTime and letting history GC react as runs reach terminal phases.
func (r *ClusterBackupScheduleReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&cbv1.ClusterBackupSchedule{}).
		Owns(&cbv1.ClusterBackup{}).
		Named("clusterbackupschedule").
		Complete(r)
}

// ---------------------------------------------------------------------------
// Pure helpers.
// ---------------------------------------------------------------------------

// parseRunTick recovers the activation instant encoded in a run name "<schedule>-<UTC-timestamp>"
// (apiconst.RunName). ok=false for a name that is not one of this schedule's runs or whose suffix
// does not parse.
func parseRunTick(scheduleName, runName string) (time.Time, bool) {
	prefix := scheduleName + "-"
	if runName == "" || !strings.HasPrefix(runName, prefix) {
		return time.Time{}, false
	}
	t, err := time.ParseInLocation(apiconst.RunTimestampLayout, strings.TrimPrefix(runName, prefix), time.UTC)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// activeRun returns the first non-terminal run in the snapshot, or nil if every run is terminal.
func activeRun(runs []cbv1.ClusterBackup) *cbv1.ClusterBackup {
	for i := range runs {
		if !isTerminalClusterBackupPhase(runs[i].Status.Phase) {
			return &runs[i]
		}
	}
	return nil
}

// concurrencyPolicyOf returns the effective policy (defaulting to Forbid, the CRD default).
func concurrencyPolicyOf(sched *cbv1.ClusterBackupSchedule) cbv1.ConcurrencyPolicy {
	if sched.Spec.ConcurrencyPolicy == "" {
		return cbv1.ConcurrencyPolicy("Forbid")
	}
	return sched.Spec.ConcurrencyPolicy
}

// historyLimit resolves a run-history limit, falling back to the default when unset so a 0 never
// silently deletes all history.
func historyLimit(v int32) int {
	if v <= 0 {
		return defaultRunsHistoryLimit
	}
	return int(v)
}

// clampDuration bounds d to [lo, hi].
func clampDuration(d, lo, hi time.Duration) time.Duration {
	if d < lo {
		return lo
	}
	if d > hi {
		return hi
	}
	return d
}
