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

	// maintenanceJobDeadline bounds one maintenance op: the repository's exclusive lane is held for
	// at most this long, so a black-holed forget/unlock can never wedge the repository's queue.
	maintenanceJobDeadline = 10 * time.Minute

	// maintenanceCleanupTimeout bounds the post-run best-effort delete of the Job + creds Secret. It
	// uses a context detached from the (possibly already-cancelled) op context so cleanup runs even
	// when the op hit its deadline or the manager is shutting down.
	maintenanceCleanupTimeout = 30 * time.Second

	// moverQuiescencePoll is how often the unlock drain-wait re-reads the live data-mover count while
	// waiting for the repository to go quiet before a lock force-removal.
	moverQuiescencePoll = 2 * time.Second
)

// maintenanceResourceName is the deterministic name of BOTH a maintenance Job and its job-scoped
// creds Secret in the operator namespace, e.g. "<backup>-forget" / "<backup>-unlock". Deterministic
// so a duplicate enqueue (serialised on the repository lane) re-creates cleanly after the prior run
// deleted its resources, rather than colliding.
func maintenanceResourceName(backupName, op string) string {
	return backupName + "-" + op
}

// maintenanceJobLabels stamps a maintenance Job (and its pod template) and its creds Secret. Like
// initJobLabels it avoids app.kubernetes.io/name=crystal-backup (the operator pod's label). It
// carries the managed-by label but NO per-PVC label, which matters twice: the mover-concurrency
// semaphore (internal/concurrency) counts only per-PVC mover Jobs, and the orphan reaper only reaps
// per-PVC exposure objects — so a maintenance Job is neither miscounted as a data mover nor reaped
// out from under itself. Cleanup of these is runRepoMaintenance's own responsibility.
func maintenanceJobLabels() map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       "crystal-mover",
		"app.kubernetes.io/managed-by": "crystal-backup",
		"app.kubernetes.io/component":  "maintenance",
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
	name := maintenanceResourceName(backup.Name, "forget")
	r.enqueueRepoMaintenance(ctx, rc, queue.OpForget, name, mover.OpForget, argv)
	r.Recorder.Eventf(backup, corev1.EventTypeNormal, "RetentionEnqueued",
		"retention forget enqueued on repository %s", rc.repoName)
}

// enqueueStaleLockUnlock clears a repository lock a mover may have left behind when it was hard-
// killed (OOMKilled / SIGKILL), detected upstream as a BLANK or unparseable termination message. It
// is enqueued fire-and-forget on the repository's exclusive queue and is idempotent (restic unlock
// removes only locks past its staleness window), so an occasional duplicate — two volumes of one
// backup crashing, say — is harmless.
func (r *BackupReconciler) enqueueStaleLockUnlock(ctx context.Context, backup *cbv1.Backup, rc *backupRunContext) {
	name := maintenanceResourceName(backup.Name, "unlock")
	r.enqueueRepoMaintenance(ctx, rc, queue.OpUnlock, name, mover.OpUnlock, restic.UnlockArgs())
	r.Recorder.Eventf(backup, corev1.EventTypeWarning, "StaleLockUnlockEnqueued",
		"a mover was hard-killed; stale-lock unlock enqueued on repository %s", rc.repoName)
}

// maintenanceOpBlocksMovers reports whether a maintenance op force-removes repository locks and so
// must run under mover quiescence — the writer side of the per-repo backup⇄unlock mutex. It is the
// mover.Operation counterpart of queue.blocksMovers (which gates admission by queue.OpKind): both
// name OpUnlock in M1, and both gain OpPrune when the maintenance controller lands (M2/M4).
func maintenanceOpBlocksMovers(op mover.Operation) bool {
	return op == mover.OpUnlock
}

// waitForMoverQuiescence blocks until no data-mover Job is running (or ctx expires), so an
// `unlock --remove-all` never strips a live backup's lock out from under it. It is the drain-wait
// half of the per-repo backup⇄unlock mutex; the reader side (moverSlotBlocked) holds NEW movers
// back while this op is in-flight, so the running movers it waits on monotonically drain. It
// conservatively waits on ALL data movers — M1 has one shared cluster repository; a per-repository
// wait arrives with the namespace plane (M5). Bounded by ctx; a timeout is returned to the caller.
func (r *BackupReconciler) waitForMoverQuiescence(ctx context.Context) error {
	ticker := time.NewTicker(moverQuiescencePoll)
	defer ticker.Stop()
	for {
		n, err := r.activeMoverCount(ctx)
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

// enqueueRepoMaintenance schedules a maintenance mover op on the repository's exclusive queue and
// returns immediately. It is FIRE-AND-FORGET: the Handle is intentionally dropped because these ops
// are best-effort and the queue runs the closure to completion whether or not anyone awaits it.
// repoKey is rc.repoName (the BackupRepository name == the location name), which is exactly the key
// the BackupRepository controller uses, so init/forget/unlock all share one serialisation lane per
// repository. The only enqueue error is ErrStopped (queue shutting down), which is logged and
// dropped — a best-effort op skipped at shutdown is reapplied by the next trigger.
func (r *BackupReconciler) enqueueRepoMaintenance(ctx context.Context, rc *backupRunContext, kind queue.OpKind, name string, op mover.Operation, resticArgs []string) {
	// Capture the primitives (not the per-reconcile rc pointer) so the closure, which runs later in
	// the queue worker goroutine, is a pure function of stable values.
	repoURL, dek, s3CredsSecret := rc.repoURL, rc.dek, rc.s3CredsSecret
	if _, err := r.Queue.Enqueue(rc.repoName, kind, func(opCtx context.Context) error {
		return r.runRepoMaintenance(opCtx, name, op, resticArgs, repoURL, dek, s3CredsSecret)
	}); err != nil {
		logf.FromContext(ctx).Info("repository maintenance op not enqueued (queue stopping); skipping",
			"op", string(op), "repository", rc.repoName, "err", err.Error())
	}
}

// runRepoMaintenance is a maintenance op body, run in the queue worker goroutine (NOT the reconcile
// goroutine). It ensures the job-scoped creds Secret (the DEK as the restic password + the two S3
// keys), creates the maintenance mover Job (no data volume), waits for it to terminate, and ALWAYS
// best-effort deletes the Job + Secret before returning — the orphan reaper never touches these
// (they carry no per-PVC label), so cleanup is this function's own responsibility. It writes no CR
// status; its error resolves the (dropped) queue Handle and is logged by the worker.
func (r *BackupReconciler) runRepoMaintenance(opCtx context.Context, name string, op mover.Operation, resticArgs []string, repoURL, dek, s3CredsSecret string) error {
	ctx, cancel := context.WithTimeout(opCtx, maintenanceJobDeadline)
	defer cancel()
	// LIFO defer order: this runs BEFORE cancel(), so ctx is still live for the delete when the op
	// finished normally; the detached context inside covers the deadline/shutdown case.
	defer r.deleteMaintenanceResources(opCtx, name)

	// Drain-wait half of the per-repo backup⇄unlock mutex: an op that force-removes locks
	// (`unlock --remove-all`) must not run while a backup holds a live lock. The reader side
	// (moverSlotBlocked → QuiescenceRequired) already holds NEW movers back — this op is in-flight
	// on the queue by now — so the in-flight movers this waits on only drain. Bounded by ctx
	// (maintenanceJobDeadline); a timeout returns an error and, being best-effort, the op is
	// reapplied by the next trigger.
	if maintenanceOpBlocksMovers(op) {
		if err := r.waitForMoverQuiescence(ctx); err != nil {
			return err
		}
	}

	labels := maintenanceJobLabels()
	if err := r.ensureMoverCredsSecret(ctx, name, dek, s3CredsSecret, labels); err != nil {
		return err
	}
	job := mover.BuildJob(mover.JobRequest{
		Name:         name,
		Namespace:    r.OperatorNamespace,
		Image:        r.MoverImage,
		Operation:    op,
		ResticArgs:   resticArgs,
		RepoURL:      repoURL,
		SecretName:   name,
		PVC:          nil,
		BackoffLimit: maintenanceJobBackoffLimit,
		TTLSeconds:   maintenanceJobTTLSeconds,
		Labels:       labels,
	})
	// No ownerReference: the Job is in the operator namespace and the triggering Backup is in a
	// tenant namespace, so a cross-namespace ownerRef is illegal. It is tracked by its deterministic
	// name and cleaned up explicitly below.
	if err := r.Create(ctx, job); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create %s job %s/%s: %w", op, r.OperatorNamespace, name, err)
	}
	return r.waitForMaintenanceJob(ctx, name)
}

// waitForMaintenanceJob polls the maintenance Job until terminal success (Succeeded >= 1 / the
// Complete condition) or terminal failure (the Failed condition, or Failed pods past the backoff
// limit), honouring ctx (the op deadline or Manager.Stop). It mirrors the BackupRepository
// controller's waitForInitJob; a failed maintenance Job is a returned error the (dropped) Handle
// carries, and the op is reapplied by the next trigger.
func (r *BackupReconciler) waitForMaintenanceJob(ctx context.Context, jobName string) error {
	key := client.ObjectKey{Namespace: r.OperatorNamespace, Name: jobName}
	ticker := time.NewTicker(maintenanceJobPollInterval)
	defer ticker.Stop()

	for {
		var job batchv1.Job
		if err := r.Get(ctx, key, &job); err != nil {
			return fmt.Errorf("get maintenance job %s/%s: %w", r.OperatorNamespace, jobName, err)
		}
		if job.Status.Succeeded >= 1 || jobConditionTrue(&job, batchv1.JobComplete) {
			return nil
		}
		if jobConditionTrue(&job, batchv1.JobFailed) || job.Status.Failed > maintenanceJobBackoffLimit {
			return fmt.Errorf("maintenance job %s/%s failed (failed pods=%d, backoffLimit=%d)",
				r.OperatorNamespace, jobName, job.Status.Failed, maintenanceJobBackoffLimit)
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("maintenance job %s/%s did not complete: %w", r.OperatorNamespace, jobName, ctx.Err())
		case <-ticker.C:
		}
	}
}

// deleteMaintenanceResources best-effort foreground-deletes a maintenance Job (so its pod goes too)
// and its job-scoped creds Secret. It runs on a context DETACHED from the op context (which may
// already be cancelled by the op deadline or Manager.Stop) so the DEK-bearing Secret is reclaimed
// promptly rather than lingering until its own delete; failures are logged, never returned.
func (r *BackupReconciler) deleteMaintenanceResources(opCtx context.Context, name string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(opCtx), maintenanceCleanupTimeout)
	defer cancel()
	log := logf.FromContext(ctx)

	fg := metav1.DeletePropagationForeground
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: r.OperatorNamespace}}
	if err := r.Delete(ctx, job, &client.DeleteOptions{PropagationPolicy: &fg}); err != nil && !apierrors.IsNotFound(err) {
		log.Error(err, "best-effort delete of maintenance job failed", "job", name)
	}
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: r.OperatorNamespace}}
	if err := r.Delete(ctx, sec); err != nil && !apierrors.IsNotFound(err) {
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
