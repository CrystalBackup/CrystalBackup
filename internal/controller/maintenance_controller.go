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
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/client/secrets"
	"github.com/CrystalBackup/CrystalBackup/internal/mover"
	"github.com/CrystalBackup/CrystalBackup/internal/repo/queue"
	"github.com/CrystalBackup/CrystalBackup/internal/restic"
	"github.com/CrystalBackup/CrystalBackup/internal/schedule"
	"github.com/CrystalBackup/CrystalBackup/internal/status"
)

const (
	// ConditionRepositoryCheckFailed is TRUE when the most recent `restic check` found repository
	// damage. It is a deliberately NEGATIVE condition, against the usual positive-polarity
	// convention, because the spec names it that way end to end — 08-testing case 12 asserts "the
	// RepositoryCheckFailed condition", and the alert in 05-observability §3 carries the same name.
	// A repository that has never been checked has no condition at all, which is distinct from one
	// that was checked and passed.
	ConditionRepositoryCheckFailed = "RepositoryCheckFailed"

	// maintenanceHistoryLimit caps status.recentMaintenance. Ten entries is roughly a week of a
	// daily prune plus a weekly check — long enough to see a pattern, short enough that the status
	// subresource stays small on a cluster with many repositories.
	maintenanceHistoryLimit = 10

	// maintenanceJitterWindow spreads maintenance starts so that several clusters sharing one
	// bucket (R20: one repository can serve several clusters, keyed by clusterID) do not all open
	// their prune window on the same cron minute and collide on the repository lock. Seeded on the
	// repository UID and the operation, so a given repository's prune and check drift apart too.
	maintenanceJitterWindow = 10 * time.Minute

	// maintenanceMinRequeue / maintenanceMaxRequeue clamp the idle requeue. A reconcile that has
	// nothing to do computes two cron ticks and returns in microseconds, so re-checking every ten
	// minutes at worst costs nothing and removes any chance of oversleeping a window because the
	// cron expression, the timezone or the clock moved underneath us.
	maintenanceMinRequeue = 1 * time.Minute
	maintenanceMaxRequeue = 10 * time.Minute

	// maintenanceWatchdogInterval re-checks a repository whose op is in flight. Pure insurance
	// against a lost wake event; the wake normally arrives first.
	maintenanceWatchdogInterval = 30 * time.Second

	// maintenanceMessageLimit truncates a failure message to the CRD's MaxLength.
	maintenanceMessageLimit = 512
)

// MaintenanceReconciler runs a repository's SCHEDULED maintenance: `restic prune` to reclaim space
// and `restic check` to verify integrity (R17), each on its own cron from the backing
// ClusterBackupLocation's maintenance block.
//
// Three properties are load-bearing.
//
// It never blocks. Both ops are submitted to the per-repository exclusive queue and watched by
// maintenanceTracker on a background goroutine; Reconcile returns immediately and writes the
// outcome on a later pass. A prune can hold the lane for hours, and a reconcile worker waiting on
// it is exactly the pathology M3.1 removed from discovery.
//
// It is not the only writer of BackupRepository.status. The BackupRepository controller declares
// itself the single writer of the fields IT owns; discovery already writes the inventory fields
// with a read-modify-write plus conflict retry, and this controller does the same for the
// maintenance fields. Each controller's Get-modify-Update preserves the others' fields, so a
// concurrent write costs a retry, never a lost update. The two watch predicates are the other half
// of that arrangement: neither controller may wake on the other's writes.
//
// The schedule baseline is the ATTEMPT history, not the success timestamps. status.lastMaintenance
// Time records only a successful prune (that is what it means), so using it as the cron baseline
// would make a failing prune permanently due — a tight retry loop hammering the repository. The
// baseline comes from status.recentMaintenance instead, which records attempts.
type MaintenanceReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Secrets is the uncached GET-by-name Secret reader (invariant I3): the cluster KEK and the
	// DR S3 credentials, both in OperatorNamespace.
	Secrets *secrets.ByNameReader
	// Queue is the per-repository exclusive lane. prune and check take a turn on it exactly like
	// init, forget and unlock, so maintenance can never race them (adr/0015).
	Queue             *queue.Manager
	OperatorNamespace string
	MoverImage        string
	Recorder          events.EventRecorder
	// Clock is the only source of "now": clock.RealClock in production, a fake clock in envtest so
	// a test can step a daily cron without waiting a day.
	Clock clock.PassiveClock

	tracker *maintenanceTracker
}

// NewMaintenanceReconciler builds a MaintenanceReconciler with its tracker initialised. Callers
// must go through this constructor so the tracker is never nil.
func NewMaintenanceReconciler(
	c client.Client,
	scheme *runtime.Scheme,
	secretsReader *secrets.ByNameReader,
	q *queue.Manager,
	operatorNamespace, moverImage string,
	recorder events.EventRecorder,
	cl clock.PassiveClock,
) *MaintenanceReconciler {
	return &MaintenanceReconciler{
		Client:            c,
		Scheme:            scheme,
		Secrets:           secretsReader,
		Queue:             q,
		OperatorNamespace: operatorNamespace,
		MoverImage:        moverImage,
		Recorder:          recorder,
		Clock:             cl,
		tracker:           newMaintenanceTracker(),
	}
}

// +kubebuilder:rbac:groups=crystalbackup.io,resources=backuprepositories,verbs=get;list;watch
// +kubebuilder:rbac:groups=crystalbackup.io,resources=backuprepositories/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=crystalbackup.io,resources=clusterbackuplocations,verbs=get;list;watch

// Reconcile drives one repository's maintenance schedule. In order: consume a finished op and
// record it, then — if nothing is running — decide whether prune or check is due and submit one.
func (r *MaintenanceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var repo cbv1.BackupRepository
	if err := r.Get(ctx, req.NamespacedName, &repo); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("get BackupRepository %s: %w", req.Name, err)
	}
	// A repository being torn down keeps no maintenance schedule. This controller sets no
	// finalizer of its own: an in-flight op holds the queue lane, not the object.
	if !repo.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	outcome, running := r.tracker.take(repo.Name)
	if outcome != nil {
		if err := r.recordOutcome(ctx, &repo, outcome); err != nil {
			return ctrl.Result{}, err
		}
	}
	if running {
		// An op is on the lane. Nothing to decide until it lands; the wake channel will bring us
		// back, and the watchdog covers a lost wake.
		return ctrl.Result{RequeueAfter: maintenanceWatchdogInterval}, nil
	}

	// Nothing to maintain until the repository exists on the far end.
	if !repo.Status.Initialized {
		return ctrl.Result{}, nil
	}

	loc, err := r.resolveLocation(ctx, &repo)
	if err != nil {
		return ctrl.Result{}, err
	}
	if loc == nil || loc.Spec.Maintenance == nil {
		// No location yet, or no maintenance configured. Both are steady states, not errors: the
		// ClusterBackupLocation watch wakes us if that changes, so requeueing would be pure churn.
		return ctrl.Result{}, nil
	}

	due, nextAt := r.nextDue(&repo, loc)
	if due == nil {
		return ctrl.Result{RequeueAfter: r.clampRequeue(nextAt)}, nil
	}

	if err := r.submit(ctx, &repo, loc, *due); err != nil {
		// A submission failure is reported and retried on the normal cadence — never returned as a
		// reconcile error, which would hot-loop against a repository whose credentials are missing.
		log.Info("maintenance op not submitted; will retry", "repository", repo.Name,
			"operation", string(due.op), "err", err.Error())
		r.Recorder.Eventf(&repo, nil, corev1.EventTypeWarning, "MaintenanceNotStarted", "SubmitMaintenance",
			"%s could not be started on repository %s: %v", due.op, repo.Name, err)
		return ctrl.Result{RequeueAfter: maintenanceMinRequeue}, nil
	}
	return ctrl.Result{RequeueAfter: maintenanceWatchdogInterval}, nil
}

// maintenanceDue is one operation whose cron said "now", carried with the tick it fired for so the
// record and the log say which window this run belongs to.
type maintenanceDue struct {
	op   mover.Operation
	kind queue.OpKind
	tick time.Time
}

// nextDue picks the operation to run now, or reports when to look again.
//
// When both are due, PRUNE wins. Prune is the one that reclaims money and the one that holds a
// cluster-wide exclusive window; a check that waits one reconcile loses nothing, whereas a prune
// repeatedly displaced by a long check would never reclaim anything. Only one op is submitted per
// pass: they would serialise on the lane anyway, and submitting both would let a slow prune
// accumulate a backlog of ops behind it.
func (r *MaintenanceReconciler) nextDue(repo *cbv1.BackupRepository, loc *cbv1.ClusterBackupLocation) (*maintenanceDue, time.Time) {
	now := r.Clock.Now()
	m := loc.Spec.Maintenance
	var earliest time.Time

	consider := func(expr string, op mover.Operation, kind queue.OpKind, skip bool) *maintenanceDue {
		if expr == "" || skip {
			return nil
		}
		cron, err := schedule.Parse(expr, m.Timezone)
		if err != nil {
			// A malformed cron is spec-static: surfacing it once and waiting for an edit beats
			// hot-looping on an expression that cannot become valid on its own.
			return nil
		}
		offset := schedule.JitterOffset(string(repo.UID)+string(op), maintenanceJitterWindow)
		effectiveNow := now.Add(-offset)
		if tick, ok := cron.DueTick(r.lastAttempt(repo, op), effectiveNow, nil); ok {
			return &maintenanceDue{op: op, kind: kind, tick: tick}
		}
		next := cron.Next(effectiveNow).Add(offset)
		if earliest.IsZero() || next.Before(earliest) {
			earliest = next
		}
		return nil
	}

	// Immutable locations never prune: object-lock forbids it until expiry (adr/0005). Admission
	// rule 6 already denies pruneSchedule there, so this is the defence for a location whose mode
	// changed after the schedule was set.
	immutable := loc.Spec.Mode == cbv1.LocationModeImmutable
	if due := consider(m.PruneSchedule, mover.OpPrune, queue.OpPrune, immutable); due != nil {
		return due, earliest
	}
	if due := consider(m.CheckSchedule, mover.OpCheck, queue.OpCheck, false); due != nil {
		return due, earliest
	}
	return nil, earliest
}

// lastAttempt is the cron baseline for one operation: when it was last TRIED, from the attempt
// history. Falling back to the repository's creation time means a freshly created repository does
// not immediately prune on apply — the same "no run on apply" property the schedules have.
//
// It must not be lastMaintenanceTime/lastCheckTime. lastMaintenanceTime advances only on SUCCESS,
// so a failing prune would stay permanently due and retry as fast as the controller could
// resubmit it, hammering a repository that is already unhealthy.
func (r *MaintenanceReconciler) lastAttempt(repo *cbv1.BackupRepository, op mover.Operation) time.Time {
	for i := range repo.Status.RecentMaintenance {
		if repo.Status.RecentMaintenance[i].Operation == string(op) {
			return repo.Status.RecentMaintenance[i].StartTime.Time // newest-first: first match wins
		}
	}
	return repo.CreationTimestamp.Time
}

// clampRequeue bounds the wait until the next window so a daily cron does not translate into a
// day-long sleep, without making an idle repository reconcile in a tight loop.
func (r *MaintenanceReconciler) clampRequeue(next time.Time) time.Duration {
	if next.IsZero() {
		return maintenanceMaxRequeue
	}
	d := next.Sub(r.Clock.Now())
	if d < maintenanceMinRequeue {
		return maintenanceMinRequeue
	}
	if d > maintenanceMaxRequeue {
		return maintenanceMaxRequeue
	}
	return d
}

// resolveLocation fetches the ClusterBackupLocation backing this repository (they share a name).
// A missing location means the repository is mid-teardown or not yet linked — nil, not an error.
func (r *MaintenanceReconciler) resolveLocation(ctx context.Context, repo *cbv1.BackupRepository) (*cbv1.ClusterBackupLocation, error) {
	var loc cbv1.ClusterBackupLocation
	if err := r.Get(ctx, client.ObjectKey{Name: repo.Name}, &loc); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("get ClusterBackupLocation %s: %w", repo.Name, err)
	}
	return &loc, nil
}

// submit builds the argv, resolves the credentials and puts one op on the repository's lane.
func (r *MaintenanceReconciler) submit(ctx context.Context, repo *cbv1.BackupRepository,
	loc *cbv1.ClusterBackupLocation, due maintenanceDue,
) error {
	var (
		argv []string
		err  error
	)
	switch due.op {
	case mover.OpPrune:
		argv, err = restic.PruneCommand(loc.Spec.Maintenance.PruneMaxRepackSize)
	case mover.OpCheck:
		argv, err = restic.CheckCommand(loc.Spec.Maintenance.CheckReadDataSubset)
	default:
		return fmt.Errorf("unsupported maintenance operation %q", due.op)
	}
	if err != nil {
		return err // a malformed admin value, rejected before it costs a Job and a queue turn
	}

	dek, _, err := resolvePlatformDEK(ctx, r.Client, r.Secrets, r.OperatorNamespace, loc)
	if err != nil {
		return err
	}

	req := repoMaintenanceRequest{
		RepoName:      repo.Name,
		Kind:          due.kind,
		Name:          maintenanceResourceName(repo.Name, string(due.op)),
		Op:            due.op,
		ResticArgs:    argv,
		RepoURL:       repo.Status.RepositoryURL,
		DEK:           dek,
		S3CredsSecret: loc.Spec.S3.CredentialsSecretRef.Name,
	}
	deps := repoMaintenanceDeps{
		Client:            r.Client,
		Secrets:           r.Secrets,
		OperatorNamespace: r.OperatorNamespace,
		MoverImage:        r.MoverImage,
	}

	started, err := r.tracker.start(repo, due.op, r.Clock.Now(), func() (*queue.Handle, error) {
		return submitRepoMaintenance(r.Queue, deps, req)
	})
	if err != nil {
		return err
	}
	if !started {
		return nil // another op raced onto the lane; the next pass re-decides.
	}
	r.Recorder.Eventf(repo, nil, corev1.EventTypeNormal, "MaintenanceStarted", "StartMaintenance",
		"%s enqueued on repository %s for the %s window", due.op, repo.Name, due.tick.Format(time.RFC3339))
	return nil
}

// recordOutcome writes one finished op into status: the history entry, the operation's own
// timestamp fields, the check condition, and an event. It read-modify-writes so a concurrent
// discovery or BackupRepository write costs a conflict retry rather than a lost update.
func (r *MaintenanceReconciler) recordOutcome(ctx context.Context, repo *cbv1.BackupRepository, out *maintenanceOutcome) error {
	completion := metav1.NewTime(out.endedAt)
	record := cbv1.MaintenanceRecord{
		Operation:      string(out.op),
		StartTime:      metav1.NewTime(out.startedAt),
		CompletionTime: &completion,
		Result:         cbv1.MaintenanceSucceeded,
	}
	if out.err != nil {
		record.Result = cbv1.MaintenanceFailed
		record.Message = truncate(out.err.Error(), maintenanceMessageLimit)
	}

	repo.Status.RecentMaintenance = append([]cbv1.MaintenanceRecord{record}, repo.Status.RecentMaintenance...)
	if len(repo.Status.RecentMaintenance) > maintenanceHistoryLimit {
		repo.Status.RecentMaintenance = repo.Status.RecentMaintenance[:maintenanceHistoryLimit]
	}

	switch out.op {
	case mover.OpPrune:
		// Only a SUCCESSFUL prune moves lastMaintenanceTime: the field answers "how long since this
		// repository was actually reclaimed", and the staleness signal depends on failure leaving
		// it alone.
		if out.err == nil {
			repo.Status.LastMaintenanceTime = &completion
		}
	case mover.OpCheck:
		// Every check moves lastCheckTime, pass or fail, so "verified recently and it was bad" is
		// distinguishable from "not verified in weeks".
		repo.Status.LastCheckTime = &completion
		if out.err == nil {
			repo.Status.LastCheckResult = checkResultPassed
		} else {
			repo.Status.LastCheckResult = checkResultFailed
		}
		r.setCheckCondition(repo, out.err)
	}

	if err := r.Status().Update(ctx, repo); err != nil {
		return fmt.Errorf("record %s outcome on repository %s: %w", out.op, repo.Name, err)
	}
	r.emitOutcomeEvent(repo, out)
	return nil
}

// Values of BackupRepositoryStatus.LastCheckResult (CRD enum Passed;Failed).
const (
	checkResultPassed = "Passed"
	checkResultFailed = "Failed"
)

// setCheckCondition reflects the latest check into ConditionRepositoryCheckFailed, and counts how
// many checks in a row have failed so the message says "failing since when", not just "failing".
// That count is what turns a one-off S3 blip into an actionable "this repository has been damaged
// for three checks" without needing a separate counter field.
func (r *MaintenanceReconciler) setCheckCondition(repo *cbv1.BackupRepository, opErr error) {
	if opErr == nil {
		status.SetCondition(&repo.Status.Conditions, ConditionRepositoryCheckFailed, metav1.ConditionFalse,
			"CheckPassed", "restic check found no repository damage", repo.Generation)
		return
	}
	consecutive := 0
	for i := range repo.Status.RecentMaintenance {
		rec := repo.Status.RecentMaintenance[i]
		if rec.Operation != string(mover.OpCheck) {
			continue
		}
		if rec.Result != cbv1.MaintenanceFailed {
			break
		}
		consecutive++
	}
	msg := fmt.Sprintf("restic check failed: %s", truncate(opErr.Error(), maintenanceMessageLimit/2))
	if consecutive > 1 {
		msg = fmt.Sprintf("%s (%d consecutive failures)", msg, consecutive)
	}
	status.SetCondition(&repo.Status.Conditions, ConditionRepositoryCheckFailed, metav1.ConditionTrue,
		"CheckFailed", msg, repo.Generation)
}

// emitOutcomeEvent mirrors the outcome as a Kubernetes Event. The reasons are the ones
// 05-observability §5 names for BackupRepository (CheckPassed, PruneCompleted).
func (r *MaintenanceReconciler) emitOutcomeEvent(repo *cbv1.BackupRepository, out *maintenanceOutcome) {
	took := out.endedAt.Sub(out.startedAt).Round(time.Second)
	if out.err != nil {
		reason := "PruneFailed"
		if out.op == mover.OpCheck {
			reason = "CheckFailed"
		}
		r.Recorder.Eventf(repo, nil, corev1.EventTypeWarning, reason, "RunMaintenance",
			"%s failed on repository %s after %s: %v", out.op, repo.Name, took, out.err)
		return
	}
	reason := "PruneCompleted"
	if out.op == mover.OpCheck {
		reason = "CheckPassed"
	}
	r.Recorder.Eventf(repo, nil, corev1.EventTypeNormal, reason, "RunMaintenance",
		"%s completed on repository %s in %s", out.op, repo.Name, took)
}

// truncate bounds a message to the CRD's MaxLength, marking that it was cut.
func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	if limit <= 3 {
		return s[:limit]
	}
	return s[:limit-3] + "..."
}

// maintenanceChurnPredicate filters the BackupRepository stream feeding maintenance. It is the
// mirror of inventoryChurnPredicate and exists for the same reason: two controllers write one
// object's status, and unfiltered each would wake on the other's writes.
//
// This one is deliberately much narrower than discovery's, because maintenance needs almost
// nothing from the object. Its cadence comes entirely from cron plus RequeueAfter, and the only
// status transitions that change what it should do are the Initialized flip (nothing to maintain
// before the repository exists) and a change of identity (repositoryURL, location, mode). Every
// other status write — discovery's inventory, its own history, another controller's conditions —
// is noise to it.
func maintenanceChurnPredicate() predicate.Predicate {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldRepo, okOld := e.ObjectOld.(*cbv1.BackupRepository)
			newRepo, okNew := e.ObjectNew.(*cbv1.BackupRepository)
			if !okOld || !okNew {
				return true // not a BackupRepository — not ours to filter
			}
			if oldRepo.GetGeneration() != newRepo.GetGeneration() ||
				!equality.Semantic.DeepEqual(oldRepo.GetLabels(), newRepo.GetLabels()) ||
				!equality.Semantic.DeepEqual(oldRepo.GetAnnotations(), newRepo.GetAnnotations()) {
				return true
			}
			return oldRepo.Status.Initialized != newRepo.Status.Initialized ||
				oldRepo.Status.RepositoryURL != newRepo.Status.RepositoryURL ||
				oldRepo.Status.Mode != newRepo.Status.Mode ||
				!equality.Semantic.DeepEqual(oldRepo.Status.Location, newRepo.Status.Location)
		},
	}
}

// SetupWithManager registers this reconciler. The ClusterBackupLocation watch matters: the
// maintenance schedule lives on the location, and without it an admin editing pruneSchedule would
// wait until the next requeue — or, for a schedule that just moved out of reach, until the old
// one fired.
func (r *MaintenanceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&cbv1.BackupRepository{}, builder.WithPredicates(maintenanceChurnPredicate())).
		Watches(&cbv1.ClusterBackupLocation{}, handler.EnqueueRequestsFromMapFunc(mapLocationToRepository)).
		// A finished op re-enqueues its repository through this channel, so the outcome is written
		// as soon as it exists rather than at the next watchdog tick.
		WatchesRawSource(source.Channel(r.tracker.wake, &handler.EnqueueRequestForObject{})).
		Named("maintenance").
		Complete(r)
}

// mapLocationToRepository maps a ClusterBackupLocation to the BackupRepository it backs; they
// share a name.
func mapLocationToRepository(_ context.Context, obj client.Object) []reconcile.Request {
	loc, ok := obj.(*cbv1.ClusterBackupLocation)
	if !ok {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: loc.GetName()}}}
}
