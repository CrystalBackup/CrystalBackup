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
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/client/secrets"
	"github.com/CrystalBackup/CrystalBackup/internal/metrics"
	"github.com/CrystalBackup/CrystalBackup/internal/mover"
	"github.com/CrystalBackup/CrystalBackup/internal/repo/queue"
	"github.com/CrystalBackup/CrystalBackup/internal/restic"
	"github.com/CrystalBackup/CrystalBackup/internal/status"
)

// This file holds the Backup controller's two REPOSITORY MAINTENANCE ops — retention forget after a
// successful backup, and a stale-lock unlock after a hard-killed mover. Both are best-effort mover
// Jobs run through the repository's exclusive queue (shared with the BackupRepository controller),
// so they can never race an init or another maintenance op on the same repository. Neither touches
// Backup status: they are fire-and-forget side effects of a terminal transition, not part of the
// per-PVC state machine. Like the real backup data path, they run a real restic Job and are
// therefore exercised by the crucible, not envtest (which simulates mover outcomes).

const (
	// maintenanceJobBackoffLimit is the maintenance Job's spec.backoffLimit. forget/unlock are quick
	// idempotent operations; one pod-level retry absorbs a transient S3 blip, after which the op is
	// treated as failed (and, being best-effort, simply reapplied by the next trigger).
	maintenanceJobBackoffLimit int32 = 1

	// maintenanceJobTTLSeconds is the maintenance Job's ttlSecondsAfterFinished: a finished Job
	// self-cleans after ten minutes even if the explicit post-run delete is missed.
	maintenanceJobTTLSeconds int32 = 600

	// maintenanceJobPollInterval is how often the queue worker re-reads a maintenance Job while
	// awaiting completion.
	maintenanceJobPollInterval = 2 * time.Second

	// maintenanceJobDeadline bounds one SHORT maintenance op (forget/unlock): the repository's
	// exclusive lane is held for at most this long, so a black-holed op can never wedge the queue.
	maintenanceJobDeadline = 10 * time.Minute

	// maintenanceLongOpDeadline bounds prune and check, whose cost scales with total repository
	// size rather than with one backup. See maintenanceOpDeadline for why these get their own,
	// much larger, backstop.
	maintenanceLongOpDeadline = 4 * time.Hour

	// maintenanceCleanupTimeout bounds the post-run best-effort delete of the Job + creds Secret. It
	// uses a context detached from the (possibly already-cancelled) op context so cleanup runs even
	// when the op hit its deadline or the manager is shutting down.
	maintenanceCleanupTimeout = 30 * time.Second

	// moverQuiescencePoll is how often the drain-wait re-reads the live data-mover count while
	// waiting for the repository to go quiet before a lock force-removal or a prune.
	moverQuiescencePoll = 2 * time.Second

	// moverQuiescenceDeadline caps the DRAIN alone, independently of the op's own deadline.
	//
	// While a blocksMovers op is pending or in-flight, mover admission is shut cluster-wide for
	// that repository (queue.QuiescenceRequired) — so the drain-wait is the window during which no
	// namespace can start a backup. When unlock was the only such op it inherited the ten-minute op
	// deadline and that was the whole bound; prune's four-hour backstop would inherit FOUR HOURS of
	// shut admission if one mover hangs. Thirty minutes is long enough for any healthy mover to
	// finish and short enough that a wedged one costs a maintenance window, not a day of backups:
	// the drain gives up, the op fails, the lane and admission are released, and the next scheduled
	// run tries again against a repository that has meanwhile had a chance to unwedge.
	moverQuiescenceDeadline = 30 * time.Minute
)

// maintenanceResourceName is the deterministic name of BOTH a maintenance Job and its job-scoped
// creds Secret in the operator namespace, e.g. "<backup>-forget" / "<backup>-unlock". Deterministic
// so a duplicate enqueue (serialised on the repository lane) re-creates cleanly after the prior run
// deleted its resources, rather than colliding.
func maintenanceResourceName(backupName, op string) string {
	return backupName + "-" + op
}

// Recommended Kubernetes label keys stamped on the operator's mover-family workloads (the
// repo-init, maintenance and discovery Jobs and their pods). Centralised so the three label
// builders share one definition rather than repeating the string keys. The name value is always
// crystal-mover, never crystal-backup (the operator pod's own app.kubernetes.io/name, which the
// crucible's operator-restart tests select on).
const (
	labelAppName      = "app.kubernetes.io/name"
	labelAppManagedBy = "app.kubernetes.io/managed-by"
	labelAppComponent = "app.kubernetes.io/component"

	moverAppName   = "crystal-mover"
	moverManagedBy = "crystal-backup"
)

// maintenanceJobLabels stamps a maintenance Job (and its pod template) and its creds Secret. Like
// initJobLabels it avoids app.kubernetes.io/name=crystal-backup (the operator pod's label). It
// carries the managed-by label but NO per-PVC label, which matters twice: the mover-concurrency
// semaphore (internal/concurrency) counts only per-PVC mover Jobs, and the orphan reaper only reaps
// per-PVC exposure objects — so a maintenance Job is neither miscounted as a data mover nor reaped
// out from under itself. Cleanup of these is runRepoMaintenance's own responsibility.
func maintenanceJobLabels() map[string]string {
	return map[string]string{
		labelAppName:      moverAppName,
		labelAppManagedBy: moverManagedBy,
		labelAppComponent: "maintenance",
	}
}

// maybeEnqueueRetentionForget applies the LOCATION's per-PVC retention policy after a Backup
// finishes successfully, by enqueuing ONE `restic forget` on the repository's exclusive queue. The
// policy is read from the resolved location (rc.retention) — one shared repository, one authoritative
// policy (adr/0009), never the run. It is skipped in two cases: an Immutable location (rc.mode),
// where object-lock forbids prune/forget until lock expiry, and a keep-less policy (ForgetCommand
// ok=false), where a forget with no --keep-* would drop every snapshot. Retention converges across
// runs: a forget lost to a crash, a queue shutdown or an Immutable/keep-less run is reapplied
// (idempotently, forget being a pure function of the keep policy) by the next run's forget.
func (r *BackupReconciler) maybeEnqueueRetentionForget(ctx context.Context, backup *cbv1.Backup, rc *backupRunContext) {
	if rc.mode == cbv1.LocationModeImmutable {
		return // object-lock repositories cannot prune/forget until lock expiry.
	}
	argv, ok := restic.ForgetCommand(rc.retention)
	if !ok {
		return // no keep policy set: never run a forget that would delete everything.
	}
	enqueueRepoMaintenance(ctx, r.Queue, r.maintenanceDeps(), repoMaintenanceRequest{
		RepoName:       rc.repoName,
		Kind:           queue.OpForget,
		Name:           maintenanceResourceName(backup.Name, "forget"),
		Op:             mover.OpForget,
		ResticArgs:     argv,
		RepoURL:        rc.repoURL,
		DEK:            rc.dek,
		S3CredsSecret:  rc.s3CredsSecret,
		CredsNamespace: rc.credsNamespace,
	})
	r.Recorder.Eventf(backup, nil, corev1.EventTypeNormal, "RetentionEnqueued", "EnqueueRetention",
		"retention forget enqueued on repository %s", rc.repoName)
}

// maintenanceDeps assembles the shared maintenance-op dependency bundle from this
// reconciler's fields.
func (r *BackupReconciler) maintenanceDeps() repoMaintenanceDeps {
	return repoMaintenanceDeps{
		Client:            r.Client,
		Secrets:           r.Secrets,
		OperatorNamespace: r.OperatorNamespace,
		MoverImage:        r.MoverImage,
	}
}

// enqueueStaleLockUnlock clears a repository lock a mover may have left behind when it was hard-
// killed (OOMKilled / SIGKILL), detected upstream as a BLANK or unparseable termination message. It
// is enqueued fire-and-forget on the repository's exclusive queue and is idempotent (restic unlock
// removes only locks past its staleness window), so an occasional duplicate — two volumes of one
// backup crashing, say — is harmless.
func (r *BackupReconciler) enqueueStaleLockUnlock(ctx context.Context, backup *cbv1.Backup, rc *backupRunContext) {
	enqueueRepoMaintenance(ctx, r.Queue, r.maintenanceDeps(), repoMaintenanceRequest{
		RepoName:       rc.repoName,
		Kind:           queue.OpUnlock,
		Name:           maintenanceResourceName(backup.Name, "unlock"),
		Op:             mover.OpUnlock,
		ResticArgs:     restic.UnlockArgs(),
		RepoURL:        rc.repoURL,
		DEK:            rc.dek,
		S3CredsSecret:  rc.s3CredsSecret,
		CredsNamespace: rc.credsNamespace,
		OnDone: func(err error) {
			if err == nil {
				metrics.RecordLockReaped(rc.repoName, rc.clusterID)
			}
		},
	})
	r.Recorder.Eventf(backup, nil, corev1.EventTypeWarning, "StaleLockUnlockEnqueued", "EnqueueUnlock",
		"a mover was hard-killed; stale-lock unlock enqueued on repository %s", rc.repoName)
}

// maintenanceOpBlocksMovers reports whether a maintenance op must run under mover quiescence — the
// writer side of the per-repo mutex. It is the mover.Operation counterpart of queue.blocksMovers
// (which gates ADMISSION by queue.OpKind), and the two MUST agree: adr/0015's forward contract
// calls marking one without the other "only half the mutex", because admission would hold movers
// back while the op itself never waits for the ones already running (or vice versa). Both name
// OpUnlock (M1) and OpPrune (M4); keep them in lockstep when OpErase lands in M5.
func maintenanceOpBlocksMovers(op mover.Operation) bool {
	return op == mover.OpUnlock || op == mover.OpPrune
}

// maintenanceOpDeadline bounds ONE maintenance op, and is deliberately NOT one number.
//
// forget and unlock are quick, idempotent, and a black-holed one must never wedge the repository's
// lane — ten minutes is generous. prune and check are a different animal: a prune of the shared
// cluster repository repacks real data and a `check --read-data-subset` re-downloads a sample of
// every pack, both scaling with total cluster data rather than with one backup. Truncating a
// legitimate multi-hour prune at ten minutes would leave the repository permanently un-pruned —
// each run killed before it converges, forever.
//
// So the long ops get a deadline that is a BACKSTOP against a black hole, not a budget: the real
// bound on prune's duration is maintenance.pruneMaxRepackSize, and the real bound on check's is
// checkReadDataSubset. Both are admin knobs, both are documented as the thing to turn down if the
// window is too long (01-architecture.md §7). Note this window is also how long prune holds movers
// back cluster-wide (queue.blocksMovers) — which is exactly the "single cluster-wide exclusive
// prune window" adr/0009 describes, not an accident.
func maintenanceOpDeadline(op mover.Operation) time.Duration {
	switch op {
	case mover.OpPrune, mover.OpCheck:
		return maintenanceLongOpDeadline
	default:
		return maintenanceJobDeadline
	}
}

// waitForMoverQuiescence blocks until no data-mover Job is running (or ctx expires), so an
// `unlock --remove-all` never strips a live backup's or restore's lock out from under it. It is
// the drain-wait half of the per-repo mover⇄unlock mutex; the reader side (each controller's
// mover-admission gate on QuiescenceRequired) holds NEW movers back while this op is in-flight,
// so the running movers it waits on monotonically drain. It conservatively waits on ALL data
// movers — backup AND restore movers both hold repository locks and both carry the per-PVC
// label the census selects on. A package function (not a method): the Backup controller and the
// restore engine share the one implementation of the adr/0015 writer-side drain. Bounded by
// ctx; a timeout is returned to the caller.
func waitForMoverQuiescence(ctx context.Context, c client.Client, operatorNamespace string) error {
	ticker := time.NewTicker(moverQuiescencePoll)
	defer ticker.Stop()
	for {
		n, err := activeMoverCount(ctx, c, operatorNamespace)
		if err != nil {
			return fmt.Errorf("counting in-flight movers before an exclusive lock removal: %w", err)
		}
		if n == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for %d in-flight mover(s) to drain: %w", n, ctx.Err())
		case <-ticker.C:
		}
	}
}

// repoMaintenanceDeps bundles the primitives a maintenance op body needs, so the shared
// package functions below take one value instead of five loose parameters. Both the Backup
// controller and the restore engine assemble it from their own fields; the queue itself is
// passed at the enqueue call because that is the only place it is used.
type repoMaintenanceDeps struct {
	client.Client
	Secrets           *secrets.ByNameReader
	OperatorNamespace string
	MoverImage        string
}

// repoMaintenanceRequest fully describes one maintenance op: which repository lane to take
// (RepoName/Kind), what to name the Job and its creds Secret (Name), and what the mover should run
// (Op/ResticArgs) against which repository with which credentials. It exists so the growing set of
// callers — retention forget, stale-lock unlock, and the M4 maintenance controller's prune and
// check — pass one value instead of ten positional arguments, three of which are same-typed
// strings that would silently transpose.
type repoMaintenanceRequest struct {
	RepoName      string
	Kind          queue.OpKind
	Name          string
	Op            mover.Operation
	ResticArgs    []string
	RepoURL       string
	DEK           string
	S3CredsSecret string
	// CredsNamespace is where S3CredsSecret is READ from: the operator namespace for a
	// cluster-plane repository, the tenant's namespace for a namespace-plane one. Empty means
	// the operator namespace, which keeps every cluster-plane caller unchanged.
	CredsNamespace string
	// OnDone, when set, is called with the op's result from the QUEUE WORKER goroutine, just after
	// the op body returns. It exists for the fire-and-forget paths, which drop the Handle and would
	// otherwise have no way to know an op succeeded — the stale-lock counter must count locks
	// actually removed, not unlock attempts submitted. It must not block: it is holding the
	// repository's exclusive lane while it runs.
	OnDone func(error)
}

// submitRepoMaintenance schedules a maintenance mover op on the repository's exclusive queue and
// returns its Handle without waiting. repoKey is RepoName (the BackupRepository name == the
// location name), exactly the key the BackupRepository controller uses, so init/forget/unlock/
// prune/check all share ONE serialisation lane per repository.
//
// The Handle matters to callers that must REPORT the outcome — the maintenance controller records
// lastCheckResult, so it cannot drop the result the way the best-effort paths do. It must also
// never BLOCK on the returned Handle from a reconcile goroutine: a prune holds the lane for hours
// (maintenanceOpDeadline), and blocking a reconcile worker on repository I/O is the exact failure
// M3.1 spent a milestone removing from discovery.
func submitRepoMaintenance(q *queue.Manager, deps repoMaintenanceDeps, req repoMaintenanceRequest) (*queue.Handle, error) {
	return q.Enqueue(req.RepoName, req.Kind, func(opCtx context.Context) error {
		err := runRepoMaintenance(opCtx, deps, req.Name, req.Op, req.ResticArgs, req.RepoURL, req.DEK, req.S3CredsSecret, req.CredsNamespace)
		if req.OnDone != nil {
			req.OnDone(err)
		}
		return err
	})
}

// enqueueRepoMaintenance is submitRepoMaintenance's FIRE-AND-FORGET wrapper: the Handle is
// intentionally dropped because these ops are best-effort and the queue runs the closure to
// completion whether or not anyone awaits it. The only enqueue error is ErrStopped (queue shutting
// down), which is logged and dropped — a best-effort op skipped at shutdown is reapplied by the
// next trigger. Shared by the Backup controller and the restore engine (a crashed restore mover
// leaves the same un-stale lock a crashed backup mover does — adr/0015).
func enqueueRepoMaintenance(ctx context.Context, q *queue.Manager, deps repoMaintenanceDeps, req repoMaintenanceRequest) {
	if _, err := submitRepoMaintenance(q, deps, req); err != nil {
		logf.FromContext(ctx).Info("repository maintenance op not enqueued (queue stopping); skipping",
			"op", string(req.Op), "repository", req.RepoName, "err", err.Error())
	}
}

// runRepoMaintenance is a maintenance op body, run in the queue worker goroutine (NOT the reconcile
// goroutine). It ensures the job-scoped creds Secret (the DEK as the restic password + the two S3
// keys), creates the maintenance mover Job (no data volume), waits for it to terminate, and ALWAYS
// best-effort deletes the Job + Secret before returning — the orphan reaper never touches these
// (they carry no per-PVC label), so cleanup is this function's own responsibility. It writes no CR
// status; its error resolves the (dropped) queue Handle and is logged by the worker.
func runRepoMaintenance(opCtx context.Context, deps repoMaintenanceDeps, name string, op mover.Operation, resticArgs []string, repoURL, dek, s3CredsSecret, credsNamespace string) error {
	ctx, cancel := context.WithTimeout(opCtx, maintenanceOpDeadline(op))
	defer cancel()
	// LIFO defer order: this runs BEFORE cancel(), so ctx is still live for the delete when the op
	// finished normally; the detached context inside covers the deadline/shutdown case.
	defer deleteMaintenanceResources(opCtx, deps, name)

	// Drain-wait half of the per-repo mover⇄unlock mutex: an op that force-removes locks
	// (`unlock --remove-all`) must not run while a backup or restore holds a live lock. The
	// reader side (each controller's admission gate on QuiescenceRequired) already holds NEW
	// movers back — this op is in-flight on the queue by now — so the in-flight movers this
	// waits on only drain. Bounded by ctx (maintenanceJobDeadline); a timeout returns an error
	// and, being best-effort, the op is reapplied by the next trigger.
	if maintenanceOpBlocksMovers(op) {
		// Capped by moverQuiescenceDeadline as well as by ctx: the drain is the window during which
		// mover admission is shut cluster-wide, so it must not inherit prune's multi-hour backstop.
		drainCtx, cancelDrain := context.WithTimeout(ctx, moverQuiescenceDeadline)
		err := waitForMoverQuiescence(drainCtx, deps.Client, deps.OperatorNamespace)
		cancelDrain()
		if err != nil {
			return err
		}
	}

	labels := maintenanceJobLabels()
	if err := ensureMoverCredsSecret(ctx, deps, name, dek, s3CredsSecret, credsNamespace, labels); err != nil {
		return err
	}
	job := mover.BuildJob(mover.JobRequest{
		Name:         name,
		Namespace:    deps.OperatorNamespace,
		Image:        deps.MoverImage,
		Operation:    op,
		ResticArgs:   resticArgs,
		RepoURL:      repoURL,
		SecretName:   name,
		PVC:          nil,
		BackoffLimit: maintenanceJobBackoffLimit,
		TTLSeconds:   maintenanceJobTTLSeconds,
		Labels:       labels,
	})
	// No ownerReference: the Job is in the operator namespace and the triggering CR is in a
	// tenant namespace (or cluster-scoped), so an ownerRef is illegal/impossible. It is tracked
	// by its deterministic name and cleaned up explicitly below.
	if err := createMaintenanceJob(ctx, deps, op, job); err != nil {
		return err
	}
	return waitForMaintenanceJob(ctx, deps.Client, deps.OperatorNamespace, name)
}

// createMaintenanceJob creates the op's Job, disambiguating the deterministic-name collision that
// AlreadyExists alone cannot: a leftover with that name has two very different faces.
//
//   - A LIVE leftover (no deletionTimestamp) is a crashed previous run's Job: adopt it — that
//     re-adoption is the restart contract deterministic names exist for.
//   - A TERMINATING one is the PREVIOUS op's foreground delete still collecting its pods, and
//     adopting it is how a real cluster recorded a prune as Failed with "get maintenance job …:
//     not found": back-to-back schedules queue op B behind op A on the exclusive queue, A's
//     deferred cleanup leaves its Job visible-but-dying, B's Create hits AlreadyExists, and the
//     grave vanishes mid-poll. Worse, a dying SUCCEEDED Job could hand B a success it never
//     earned. So a terminating grave is waited out (bounded by the op context), then the create
//     is retried against the emptied name.
func createMaintenanceJob(ctx context.Context, deps repoMaintenanceDeps, op mover.Operation, job *batchv1.Job) error {
	for {
		err := deps.Create(ctx, job)
		if err == nil {
			return nil
		}
		if !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create %s job %s/%s: %w", op, job.Namespace, job.Name, err)
		}
		var existing batchv1.Job
		switch getErr := deps.Get(ctx, client.ObjectKeyFromObject(job), &existing); {
		case apierrors.IsNotFound(getErr):
			continue // the grave emptied between Create and Get: retry the create immediately.
		case getErr != nil:
			return fmt.Errorf("inspect existing %s job %s/%s: %w", op, job.Namespace, job.Name, getErr)
		case existing.DeletionTimestamp.IsZero():
			return nil // a live leftover from a crashed run: adopt it.
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for the previous %s job %s/%s to finish deleting: %w",
				op, job.Namespace, job.Name, ctx.Err())
		case <-time.After(maintenanceJobPollInterval):
		}
	}
}

// waitForMaintenanceJob polls the maintenance Job until terminal success (Succeeded >= 1 / the
// Complete condition) or terminal failure (the Failed condition, or Failed pods past the backoff
// limit), honouring ctx (the op deadline or Manager.Stop). It mirrors the BackupRepository
// controller's waitForInitJob; a failed maintenance Job is a returned error the (dropped) Handle
// carries, and the op is reapplied by the next trigger.
func waitForMaintenanceJob(ctx context.Context, c client.Client, operatorNamespace, jobName string) error {
	key := client.ObjectKey{Namespace: operatorNamespace, Name: jobName}
	ticker := time.NewTicker(maintenanceJobPollInterval)
	defer ticker.Stop()

	for {
		var job batchv1.Job
		if err := c.Get(ctx, key, &job); err != nil {
			return fmt.Errorf("get maintenance job %s/%s: %w", operatorNamespace, jobName, err)
		}
		if job.Status.Succeeded >= 1 || jobConditionTrue(&job, batchv1.JobComplete) {
			return nil
		}
		if jobConditionTrue(&job, batchv1.JobFailed) || job.Status.Failed > maintenanceJobBackoffLimit {
			return fmt.Errorf("maintenance job %s/%s failed (failed pods=%d, backoffLimit=%d)",
				operatorNamespace, jobName, job.Status.Failed, maintenanceJobBackoffLimit)
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("maintenance job %s/%s did not complete: %w", operatorNamespace, jobName, ctx.Err())
		case <-ticker.C:
		}
	}
}

// deleteMaintenanceResources best-effort foreground-deletes a maintenance Job (so its pod goes too)
// and its job-scoped creds Secret. It runs on a context DETACHED from the op context (which may
// already be cancelled by the op deadline or Manager.Stop) so the DEK-bearing Secret is reclaimed
// promptly rather than lingering until its own delete; failures are logged, never returned.
func deleteMaintenanceResources(opCtx context.Context, deps repoMaintenanceDeps, name string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(opCtx), maintenanceCleanupTimeout)
	defer cancel()
	log := logf.FromContext(ctx)

	fg := metav1.DeletePropagationForeground
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: deps.OperatorNamespace}}
	if err := deps.Delete(ctx, job, &client.DeleteOptions{PropagationPolicy: &fg}); err != nil && !apierrors.IsNotFound(err) {
		log.Error(err, "best-effort delete of maintenance job failed", "job", name)
	}
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: deps.OperatorNamespace}}
	if err := deps.Delete(ctx, sec); err != nil && !apierrors.IsNotFound(err) {
		log.Error(err, "best-effort delete of maintenance creds secret failed", "secret", name)
	}
}

// backupSucceeded reports whether a rolled-up Backup phase is a terminal SUCCESS — all volumes
// completed, or some skipped with none failed — the phases after which retention should run.
// PartiallyFailed and Failed are excluded: a run that lost data does not drive a retention forget
// that could drop older, still-good snapshots.
func backupSucceeded(phase string) bool {
	return phase == string(status.BackupPhaseCompleted) ||
		phase == string(status.BackupPhasePartiallyCompleted)
}
