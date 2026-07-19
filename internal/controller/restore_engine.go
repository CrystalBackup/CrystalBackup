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
	"encoding/json"
	"fmt"
	"maps"
	"path"
	"slices"
	"strings"
	"sync"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
	clientsecrets "github.com/CrystalBackup/CrystalBackup/internal/client/secrets"
	"github.com/CrystalBackup/CrystalBackup/internal/concurrency"
	"github.com/CrystalBackup/CrystalBackup/internal/mover"
	"github.com/CrystalBackup/CrystalBackup/internal/repo/queue"
	"github.com/CrystalBackup/CrystalBackup/internal/restic"
	"github.com/CrystalBackup/CrystalBackup/internal/rexposer"
)

// This file is the CR-AGNOSTIC restore execution engine both restore controllers drive
// (Restore in restore_controller.go, ClusterRestore in clusterrestore_controller.go). One
// engine, one mechanics: per-volume, it exposes the target (internal/rexposer), runs one
// OpRestore mover Job, finalizes the handover, and derives every state transition from LIVE
// objects — a restore keeps NO persisted per-volume phase list (its status carries only
// aggregate counters, spec/02-api.md), so the mover Jobs and the exposure objects ARE the
// durable state, and teardown happens only AFTER the terminal status write (adr/0016).

const (
	// restorePollInterval paces re-reconciles while a restore is driving its volumes.
	restorePollInterval = backupPollInterval

	// restoreMoverBackoffLimit is the restore mover Job's spec.backoffLimit. Neither restore
	// CRD exposes a knob (a restore is a one-shot recovery action, not a tunable pipeline);
	// two pod-level retries absorb a transient S3/attach blip.
	restoreMoverBackoffLimit int32 = 2

	// restoreNamePrefixMax caps a per-PVC restore NamePrefix so the LONGEST derived name —
	// "<prefix>-restore" (8 chars) — stays within the 63-char DNS label limit; the staging
	// "-target" (7) and twin "-twin" (5) suffixes fit under the same cap.
	restoreNamePrefixMax = 55

	// restoreTargetMountPath is where every restore mover mounts the staging PVC, and the
	// base restic --target resolves under. A NEUTRAL constant path (unlike backup, whose
	// mount path IS the snapshot identity): the snapshot subtree is addressed on the restic
	// side (<snapshotID>:/data/<ns>/<pvc>), never by the mount location.
	restoreTargetMountPath = "/crystal/target"
)

// restoreJobName derives the restore mover Job (and creds Secret) name from a per-PVC
// prefix. The "-restore" suffix keeps it disjoint from every backup mover ("-mover") even
// when a Backup and a Restore share a name.
func restoreJobName(prefix string) string { return prefix + "-restore" }

// restoreNamePrefix is the deterministic per-PVC NamePrefix "<ownerID>-<pvc>" for restore
// objects, sanitized exactly like moverNamePrefix but under the restore cap. ownerID is the
// UNIQUE owner identity (restoreOwnerID/clusterRestoreOwnerID), never a bare CR name — two
// namespaced Restores may share a name across namespaces while their objects share the one
// operator namespace.
func restoreNamePrefix(ownerID, pvcName string) string {
	return sanitizeDNSName(ownerID+"-"+pvcName, restoreNamePrefixMax)
}

// restoreOwnerID / clusterRestoreOwnerID derive the cluster-unique identity every
// operator-namespace object name is built from. The kind prefix keeps a Restore and a
// ClusterRestore sharing a bare name apart; the namespace qualifies a Restore's name,
// which is only unique within its namespace.
func restoreOwnerID(namespace, name string) string { return "rst-" + namespace + "-" + name }
func clusterRestoreOwnerID(name string) string     { return "crst-" + name }

// restoreVolumePlan is one PVC's fully-resolved restore work order. Every field is derived
// server-side by the owning controller (snapshot IDs from the mediated listing or the
// operator-authored Backup status; capacity/class/modes from PVC-meta tags, the live target
// claim, or the documented fallback) — never from a user-writable free field (I1).
type restoreVolumePlan struct {
	// pvc is the target PVC name (== the source PVC name; restores never rename volumes).
	pvc string
	// snapshotID is the server-resolved restic snapshot holding this PVC's data.
	snapshotID string
	// snapshotPath is the snapshot's recorded subtree (/data/<origin-ns>/<pvc>).
	snapshotPath string
	// include/exclude are the user's file globs (R7); targetPath the sanitized restore root
	// within the PVC ("" = the PVC root).
	include, exclude []string
	targetPath       string
	// capacity/storageClass/accessModes drive a transplant provisioning (ignored for a twin).
	capacity     resource.Quantity
	storageClass string
	accessModes  []corev1.PersistentVolumeAccessMode
}

// restoreExecContext carries one restore run's resolved, CR-agnostic inputs.
type restoreExecContext struct {
	// ownerName / ownerLabelKey identify the owning CR on every created object:
	// apiconst.LabelRestore for a Restore, apiconst.LabelClusterRestore for a ClusterRestore.
	ownerName     string
	ownerLabelKey string
	// ownerID is the UNIQUE identity every operator-namespace object name derives from. Two
	// namespaced Restores may share a NAME across namespaces (and a ClusterRestore may share
	// one with either), while all their staging PVCs, twin PVs and mover Jobs share the ONE
	// operator namespace — so a bare name would collide and cross-adopt another tenant's
	// objects. Callers set it to "rst-<namespace>-<name>" for a Restore and "crst-<name>"
	// for a (cluster-scoped, so cluster-unique) ClusterRestore.
	ownerID string
	// targetNamespace is where the restored PVCs live.
	targetNamespace string
	// deleteExtras selects the Recreate reconciliation (restic --delete).
	deleteExtras bool
	// restoredFromRun is the originating run (provenance annotation on transplanted PVCs).
	restoredFromRun string
	// Repository access, resolved by the owning controller.
	repoName, repoURL, dek, s3CredsSecret string
	// startBudget is set by driveVolumes each pass to (restoreOwnerMoverCap - running
	// movers of this owner); startVolume consumes one unit per mover Job it creates and
	// holds further volumes back at zero. Per-reconcile state — never shared or persisted.
	startBudget int
}

// volumeOutcome is the DERIVED state of one planned volume this pass.
type volumeOutcome struct {
	// settled is true once the volume needs no further driving (success or failure).
	settled bool
	// failed is meaningful when settled; reason explains it (short, secret-free).
	failed bool
	reason string
	// restoredBytes is the mover-reported payload of a successful volume.
	restoredBytes int64
}

// restoreEngine executes restore plans. Both restore reconcilers hold one, wired with the
// same primitives as the Backup controller (uncached Secret reader, the shared exclusive
// queue) plus the target exposer.
type restoreEngine struct {
	client.Client
	Secrets           *clientsecrets.ByNameReader
	Targets           *rexposer.TargetExposer
	OperatorNamespace string
	MoverImage        string
	Queue             *queue.Manager

	// resolved caches one mediated snapshot resolution per owner (adr/0016 §3): the listing
	// Job is seconds, the reconcile poll is 5s, so resolution runs once per (owner, run) and
	// re-runs only after a restart or run change. Guarded by mu; forgotten on terminal.
	mu       sync.Mutex
	resolved map[types.UID]resolvedSnapshots
	// pinnedSource pins a time-resolved ("latest"/cutoff) source per owner so a NEWER Backup
	// completing mid-restore can never flip remaining volumes onto a different run (all
	// volumes of one restore come from ONE point in time). In-memory: a restart re-pins, a
	// window accepted and documented. Guarded by mu; forgotten with the resolution.
	pinnedSource map[types.UID]string
	// unlockOnce dedupes the stale-lock unlock per (owner, pvc): the restore keeps no
	// persisted per-volume phase, so without it a hard-killed mover would re-enqueue an
	// exclusive unlock EVERY 5s pass, flooding the repository lane and holding
	// QuiescenceRequired forever. In-memory: a restart re-enqueues once more (idempotent op).
	unlockOnce map[string]struct{}
	// errCounts is the per-(owner, pvc) budget for TRANSIENT advise errors: a volume whose
	// expose/finalize keeps erroring eventually settles as failed instead of wedging the
	// whole restore in Running forever. In-memory; a restart restarts the budget.
	errCounts map[string]int
}

// maxVolumeAdviseErrors is how many consecutive reconcile-visible errors one volume may
// produce before it is settled as failed (~5 minutes at the 5s poll, more under backoff).
const maxVolumeAdviseErrors = 60

// restoreOwnerMoverCap bounds how many of ONE owner's restore movers run at once: a
// many-volume ClusterRestore must not stampede node attach limits and the S3 endpoint with
// every volume simultaneously, and — because restore movers count in the shared mover
// census (listMoverJobs) — an unbounded restore would hold a capped backup cascade's
// admission gate shut for its whole duration. Per OWNER, not cluster-wide: the global
// cross-kind semaphore stays deferred (see the backup controller's task #22 note).
const restoreOwnerMoverCap = 4

// resolvedSnapshots is one cached mediated resolution: the run it was resolved for and the
// kind=data snapshots by PVC name.
type resolvedSnapshots struct {
	run   string
	byPVC map[string]restic.Snapshot
}

// newRestoreEngine wires an engine from the owning reconciler's primitives.
func newRestoreEngine(c client.Client, secretsReader *clientsecrets.ByNameReader, targets *rexposer.TargetExposer,
	operatorNamespace, moverImage string, q *queue.Manager,
) *restoreEngine {
	return &restoreEngine{
		Client:            c,
		Secrets:           secretsReader,
		Targets:           targets,
		OperatorNamespace: operatorNamespace,
		MoverImage:        moverImage,
		Queue:             q,
		resolved:          make(map[types.UID]resolvedSnapshots),
		pinnedSource:      make(map[types.UID]string),
		unlockOnce:        make(map[string]struct{}),
		errCounts:         make(map[string]int),
	}
}

// pinSource records (once) and returns the pinned source Backup name for a time-resolved
// restore; returns the existing pin when present.
func (e *restoreEngine) pinSource(owner types.UID, name string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if pinned, ok := e.pinnedSource[owner]; ok {
		return pinned
	}
	e.pinnedSource[owner] = name
	return name
}

// pinnedSourceFor returns the pinned source for owner, if any.
func (e *restoreEngine) pinnedSourceFor(owner types.UID) (string, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	name, ok := e.pinnedSource[owner]
	return name, ok
}

// shouldEnqueueUnlock returns true exactly once per (ownerID, pvc) — the unlock dedupe.
func (e *restoreEngine) shouldEnqueueUnlock(key string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, done := e.unlockOnce[key]; done {
		return false
	}
	e.unlockOnce[key] = struct{}{}
	return true
}

// noteVolumeError counts a transient advise error for (ownerID, pvc); giveUp turns true
// once the budget is exhausted, and the caller settles the volume as failed.
func (e *restoreEngine) noteVolumeError(key string) (giveUp bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.errCounts[key]++
	return e.errCounts[key] >= maxVolumeAdviseErrors
}

// clearVolumeError resets a volume's transient-error budget after a clean advise (the
// budget counts CONSECUTIVE errors, not lifetime ones).
func (e *restoreEngine) clearVolumeError(key string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.errCounts, key)
}

// cachedResolution returns the cached mediated resolution for owner iff it matches run.
func (e *restoreEngine) cachedResolution(owner types.UID, run string) (map[string]restic.Snapshot, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	r, ok := e.resolved[owner]
	if !ok || r.run != run {
		return nil, false
	}
	return r.byPVC, true
}

// storeResolution caches a mediated resolution; forgetResolution drops it (terminal/finalize).
func (e *restoreEngine) storeResolution(owner types.UID, run string, byPVC map[string]restic.Snapshot) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.resolved[owner] = resolvedSnapshots{run: run, byPVC: byPVC}
}

// forgetListing drops ONLY the cached snapshot listing — the source pin and the per-volume
// bookkeeping survive. Used when the cached map proves INCOMPLETE (a selected PVC has no
// snapshot in it): the run may still be uploading, so the next pass must re-list instead of
// replaying the stale map until an operator restart.
func (e *restoreEngine) forgetListing(owner types.UID) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.resolved, owner)
}

func (e *restoreEngine) forgetResolution(owner types.UID, ownerID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.resolved, owner)
	delete(e.pinnedSource, owner)
	prefix := ownerID + "/"
	for k := range e.unlockOnce {
		if strings.HasPrefix(k, prefix) {
			delete(e.unlockOnce, k)
		}
	}
	for k := range e.errCounts {
		if strings.HasPrefix(k, prefix) {
			delete(e.errCounts, k)
		}
	}
}

// volumeLabels are stamped on every object one volume's restore creates (staging PVC, twin
// PV, mover Job, creds Secret): the reaper/leak-check selector (managed-by + per-PVC)
// plus the owner identity the reaper resolves (ownerLabelKey + the target namespace).
func (e *restoreEngine) volumeLabels(rc *restoreExecContext, pvc string) map[string]string {
	return map[string]string{
		apiconst.LabelManagedBy: apiconst.ManagedByValue,
		rc.ownerLabelKey:        rc.ownerName,
		apiconst.LabelNamespace: rc.targetNamespace,
		apiconst.LabelPVC:       pvc,
	}
}

// exposureFor rebuilds a volume's TargetExposure from its plan and a KNOWN mechanism kind —
// every field is deterministic (names from the prefix, labels from the context), so the
// engine can drive Finalize/Cleanup without re-calling Expose (whose mechanism resolution
// reads the LIVE target state, which the handover itself is mutating — re-resolving
// mid-handover would misclassify the volume; the kind is therefore pinned on the mover Job
// at creation, see the exposure-kind label).
func (e *restoreEngine) exposureFor(rc *restoreExecContext, plan *restoreVolumePlan, kind string) *rexposer.TargetExposure {
	prefix := restoreNamePrefix(rc.ownerID, plan.pvc)
	return &rexposer.TargetExposure{
		Kind:              kind,
		TargetNamespace:   rc.targetNamespace,
		TargetPVCName:     plan.pvc,
		OperatorNamespace: e.OperatorNamespace,
		StagingPVCName:    rexposer.StagingPVCName(prefix),
		TwinPVName:        rexposer.TwinPVName(prefix),
		Labels:            e.volumeLabels(rc, plan.pvc),
		RestoredFromRun:   rc.restoredFromRun,
	}
}

// volumeDrive aggregates one reconcile pass over all planned volumes.
type volumeDrive struct {
	settled, completed, failedCount int
	restoredBytes                   int64
	failures                        []string
	// err is the first TRANSIENT advise error of the pass (budget not yet exhausted),
	// surfaced after every volume has been driven so one flaky volume never stalls its
	// siblings' progress.
	err error
}

// ownerRunningMovers counts this owner's live, still-running mover Jobs — the supply side
// of the restoreOwnerMoverCap admission in startVolume.
func (e *restoreEngine) ownerRunningMovers(ctx context.Context, rc *restoreExecContext) (int, error) {
	var jobs batchv1.JobList
	if err := e.List(ctx, &jobs, client.InNamespace(e.OperatorNamespace), client.MatchingLabels{
		apiconst.LabelManagedBy: apiconst.ManagedByValue,
		rc.ownerLabelKey:        rc.ownerName,
		apiconst.LabelNamespace: rc.targetNamespace,
	}); err != nil {
		return 0, fmt.Errorf("list restore mover Jobs for the concurrency gate: %w", err)
	}
	live := jobs.Items[:0]
	for _, j := range jobs.Items {
		if j.Labels[apiconst.LabelPVC] != "" && j.DeletionTimestamp == nil {
			live = append(live, j)
		}
	}
	return concurrency.RunningMoverJobs(live), nil
}

// driveVolumes advances every plan one step and aggregates the outcomes — the shared drive
// loop of both restore controllers. An advise error settles the volume as failed once its
// per-volume budget is exhausted; before that it is only remembered (drive.err) for the
// caller's usual backoff, and the remaining volumes still advance this pass.
func (e *restoreEngine) driveVolumes(ctx context.Context, rc *restoreExecContext, plans []restoreVolumePlan) volumeDrive {
	var d volumeDrive
	running, err := e.ownerRunningMovers(ctx, rc)
	if err != nil {
		d.err = err
		return d
	}
	rc.startBudget = restoreOwnerMoverCap - running
	for i := range plans {
		key := rc.ownerID + "/" + plans[i].pvc
		outcome, err := e.adviseVolume(ctx, rc, &plans[i])
		if err != nil {
			if e.noteVolumeError(key) {
				d.settled++
				d.failedCount++
				d.failures = append(d.failures, plans[i].pvc+": gave up after repeated errors, last: "+err.Error())
				continue
			}
			if d.err == nil {
				d.err = err
			}
			continue
		}
		e.clearVolumeError(key)
		if !outcome.settled {
			continue
		}
		d.settled++
		if outcome.failed {
			d.failedCount++
			d.failures = append(d.failures, plans[i].pvc+": "+outcome.reason)
			continue
		}
		d.completed++
		d.restoredBytes += outcome.restoredBytes
	}
	return d
}

// resolveMoverOutcome returns a terminal restore mover Job's result. On SUCCESS it stamps the
// result on the Job (AnnotationMoverResult) and deletes the completed mover pod — the pod
// otherwise keeps the staging claim pinned by the pvc-protection finalizer, deadlocking the
// pvc-transplant handover (which must delete the staging claim to re-point the PV). The Job
// (Succeeded=1) stays as the durable "mover ran" marker, and the annotation is the durable
// record RestoredBytes is read from once the pod is gone (a later pass, or after a restart).
//
// Returns (result, moverErr, transientErr): moverErr is a mover-side failure (unreadable /
// not-OK) that settles the volume failed; transientErr is an infra error (the stamp patch)
// the caller requeues on. On a FAILED Job the pod is left in place so the hard-kill / blank-
// message detection still works and the unlock path can fire.
func (e *restoreEngine) resolveMoverOutcome(ctx context.Context, job *batchv1.Job, jobFailed bool) (mover.MoverResult, error, error) {
	if enc := job.Annotations[apiconst.AnnotationMoverResult]; enc != "" {
		var cached mover.MoverResult
		if json.Unmarshal([]byte(enc), &cached) == nil {
			return cached, nil, nil // captured on an earlier pass; the pod is already gone.
		}
	}
	result, _, rerr := readMoverResult(ctx, e.Client, e.OperatorNamespace, job.Name)
	if jobFailed || rerr != nil || !result.OK {
		return result, rerr, nil // settle failed; leave the pod for diagnostics/hard-kill detection.
	}
	enc, err := json.Marshal(result)
	if err != nil {
		return result, nil, fmt.Errorf("encode mover result for Job %s: %w", job.Name, err)
	}
	patch, err := json.Marshal(map[string]any{
		"metadata": map[string]any{"annotations": map[string]string{apiconst.AnnotationMoverResult: string(enc)}},
	})
	if err != nil {
		return result, nil, fmt.Errorf("build mover-result patch for Job %s: %w", job.Name, err)
	}
	if err := e.Patch(ctx, job, client.RawPatch(types.MergePatchType, patch)); err != nil {
		return result, nil, fmt.Errorf("stamp mover result on Job %s: %w", job.Name, err)
	}
	return result, nil, nil
}

// deleteJobPods best-effort deletes a Job's pods (by the job-name label) so a COMPLETED mover
// pod stops pinning the staging claim's pvc-protection finalizer. Best-effort: the reaper and
// the eventual Job teardown backstop it.
func (e *restoreEngine) deleteJobPods(ctx context.Context, jobName string) {
	if err := e.DeleteAllOf(ctx, &corev1.Pod{}, client.InNamespace(e.OperatorNamespace),
		client.MatchingLabels{batchv1.JobNameLabel: jobName}); err != nil && !apierrors.IsNotFound(err) {
		logf.FromContext(ctx).Error(err, "Best-effort delete of completed restore mover pod failed", "job", jobName)
	}
}

// adviseVolume derives one volume's state from live objects and advances it by at most one
// step: expose the target, admit + create the mover Job, poll it, then finalize the
// handover. It never writes CR status (the caller aggregates outcomes and is the single
// status writer). Every path is idempotent — a restarted controller re-derives the same
// step from the same objects.
func (e *restoreEngine) adviseVolume(ctx context.Context, rc *restoreExecContext, plan *restoreVolumePlan) (volumeOutcome, error) {
	prefix := restoreNamePrefix(rc.ownerID, plan.pvc)
	jobName := restoreJobName(prefix)

	var job batchv1.Job
	err := e.Get(ctx, client.ObjectKey{Namespace: e.OperatorNamespace, Name: jobName}, &job)
	switch {
	case apierrors.IsNotFound(err):
		return e.startVolume(ctx, rc, plan, prefix, jobName)
	case err != nil:
		return volumeOutcome{}, fmt.Errorf("get restore mover Job %s/%s: %w", e.OperatorNamespace, jobName, err)
	}

	complete := job.Status.Succeeded >= 1 || jobConditionTrue(&job, batchv1.JobComplete)
	failed := jobConditionTrue(&job, batchv1.JobFailed) || job.Status.Failed > restoreMoverBackoffLimit
	if !complete && !failed {
		return volumeOutcome{}, nil // still restoring; requeue.
	}

	result, rerr, terr := e.resolveMoverOutcome(ctx, &job, failed)
	if terr != nil {
		return volumeOutcome{}, terr // transient (e.g. the annotation patch) — requeue, do not settle.
	}
	if failed || rerr != nil || !result.OK {
		// A BLANK/unparseable termination message means the mover was hard-killed and may
		// have died holding the repository lock — clear it exactly as the backup path does.
		// ONCE per volume: the failure is re-derived every pass (no persisted per-volume
		// state), and re-enqueuing an exclusive unlock each 5s would flood the repository
		// lane and hold mover admission shut for its whole tail.
		if rerr != nil && e.shouldEnqueueUnlock(rc.ownerID+"/"+plan.pvc) {
			enqueueRepoMaintenance(ctx, e.Queue, e.maintenanceDeps(), rc.repoName, queue.OpUnlock,
				maintenanceResourceName(prefix, "unlock"), mover.OpUnlock, restic.UnlockArgs(),
				rc.repoURL, rc.dek, rc.s3CredsSecret)
		}
		return volumeOutcome{settled: true, failed: true, reason: moverFailureReason(result, rerr)}, nil
	}

	// The mover succeeded and its result is captured durably on the Job. Delete the completed
	// mover pod NOW — a finished pod keeps the staging claim pinned by the pvc-protection
	// finalizer, which deadlocks the pvc-transplant handover (Finalize can never delete the
	// staging claim). Idempotent and retried every pass: a delete that lost a race, or that
	// was forbidden before the pods/delete grant reached the API server, self-heals on the
	// next reconcile. The Job (Succeeded=1) stays as the durable "mover ran" marker.
	e.deleteJobPods(ctx, jobName)

	// The data landed in the staging volume; complete the handover. The mechanism kind was
	// pinned on the Job at creation — never re-resolved from the (mutating) target state.
	kind := job.Labels[apiconst.LabelExposureKind]
	if kind != rexposer.KindTransplant && kind != rexposer.KindTwin {
		// Pin label lost (a mutated or hand-rebuilt Job). Re-derive from the durable
		// exposure objects — only the twin mechanism creates the twin PV — rather than
		// silently no-opping Finalize, which would strand a transplant's restored volume
		// in the operator namespace while the restore reports success.
		var twin corev1.PersistentVolume
		switch gerr := e.Get(ctx, client.ObjectKey{Name: rexposer.TwinPVName(prefix)}, &twin); {
		case gerr == nil:
			kind = rexposer.KindTwin
		case apierrors.IsNotFound(gerr):
			kind = rexposer.KindTransplant
		default:
			return volumeOutcome{}, fmt.Errorf("derive exposure kind for %s/%s: %w", rc.targetNamespace, plan.pvc, gerr)
		}
		logf.FromContext(ctx).Info("Exposure-kind label missing on restore mover Job; derived from live objects",
			"job", jobName, "kind", kind)
	}
	ex := e.exposureFor(rc, plan, kind)
	done, err := e.Targets.Finalize(ctx, ex)
	if err != nil {
		return volumeOutcome{}, fmt.Errorf("finalize restore target %s/%s: %w", rc.targetNamespace, plan.pvc, err)
	}
	if !done {
		return volumeOutcome{}, nil // handover settling; requeue.
	}
	return volumeOutcome{settled: true, restoredBytes: result.RestoredBytes}, nil
}

// startVolume exposes the target and creates the mover Job once the exposure is ready and
// the repository admits a new reader (the mover⇄unlock mutex). A pending return (zero
// outcome, nil error) means "requeue and re-derive".
func (e *restoreEngine) startVolume(ctx context.Context, rc *restoreExecContext, plan *restoreVolumePlan, prefix, jobName string) (volumeOutcome, error) {
	labels := e.volumeLabels(rc, plan.pvc)
	ex, err := e.Targets.Expose(ctx, rexposer.TargetRequest{
		TargetNamespace: rc.targetNamespace,
		PVCName:         plan.pvc,
		StorageClass:    plan.storageClass,
		Capacity:        plan.capacity,
		AccessModes:     plan.accessModes,
		NamePrefix:      prefix,
		Labels:          labels,
		RestoredFromRun: rc.restoredFromRun,
	})
	switch {
	case err == rexposer.ErrBlockUnsupported:
		return volumeOutcome{settled: true, failed: true, reason: "RestoreBlockUnsupported"}, nil
	case err != nil:
		return volumeOutcome{}, fmt.Errorf("expose restore target %s/%s: %w", rc.targetNamespace, plan.pvc, err)
	}

	ready, err := e.Targets.Ready(ctx, ex)
	if err != nil {
		return volumeOutcome{}, fmt.Errorf("check restore target readiness %s/%s: %w", rc.targetNamespace, plan.pvc, err)
	}
	if !ready {
		return volumeOutcome{}, nil
	}

	// Reader-side admission of the mover⇄unlock mutex: never take a repository lock an
	// enqueued `unlock --remove-all` is about to strip (adr/0015). Re-adoption is already
	// handled by the caller (this path runs only when the Job does not exist yet).
	if e.Queue != nil && rc.repoName != "" && e.Queue.QuiescenceRequired(rc.repoName) {
		return volumeOutcome{}, nil
	}

	// Per-owner concurrency admission (restoreOwnerMoverCap): hold this volume in its
	// pre-mover state — the exposure above is idempotent and cheap — until a sibling's
	// mover finishes. The pass budget was primed by driveVolumes from the live Job census.
	if rc.startBudget <= 0 {
		return volumeOutcome{}, nil
	}

	if err := ensureMoverCredsSecret(ctx, e.maintenanceDeps(), jobName, rc.dek, rc.s3CredsSecret, labels); err != nil {
		return volumeOutcome{}, err
	}

	target, err := restoreTargetPath(plan.targetPath)
	if err != nil {
		return volumeOutcome{settled: true, failed: true, reason: "InvalidTargetPath"}, nil
	}
	jobLabels := maps.Clone(labels)
	// Pin the exposure mechanism on the Job so the success path finalizes with the SAME
	// mechanism this volume started with, however the target state has changed since.
	jobLabels[apiconst.LabelExposureKind] = ex.Kind
	job := mover.BuildJob(mover.JobRequest{
		Name:      jobName,
		Namespace: e.OperatorNamespace,
		Image:     e.MoverImage,
		Operation: mover.OpRestore,
		ResticArgs: restic.RestoreArgs(plan.snapshotID, plan.snapshotPath, target,
			rc.deleteExtras, plan.include, plan.exclude),
		RepoURL:    rc.repoURL,
		SecretName: jobName,
		PVC: &mover.PVCMount{
			ClaimName: ex.StagingPVCName,
			MountPath: restoreTargetMountPath,
			ReadWrite: true,
		},
		BackoffLimit: restoreMoverBackoffLimit,
		// NO TTL: per adr/0016 the mover Jobs ARE the restore's only durable per-volume
		// state (there is no persisted per-volume phase list), so the TTL controller erasing
		// a settled volume's Job while a slow sibling still runs would make the next pass
		// re-derive "never started" and RE-RESTORE a delivered volume. Teardown deletes the
		// Jobs after the terminal status write; the reaper backstops leaks by owner labels.
		TTLSeconds: mover.NoTTL,
		Labels:     jobLabels,
		// Same-node pin for a singly-attached target volume (twin path); empty otherwise.
		NodeName: ex.NodeName,
		// Soft-spread restore movers across nodes with the rest of the mover family.
		SpreadOverLabels: map[string]string{apiconst.LabelManagedBy: apiconst.ManagedByValue},
	})
	// No ownerReference: the Job lives in the operator namespace; the owning CR is namespaced
	// (Restore) or cluster-scoped (ClusterRestore). Deterministic name + labels track it.
	if err := e.Create(ctx, job); err != nil && !apierrors.IsAlreadyExists(err) {
		return volumeOutcome{}, fmt.Errorf("create restore mover Job %s/%s: %w", e.OperatorNamespace, jobName, err)
	}
	rc.startBudget--
	return volumeOutcome{}, nil
}

// teardownVolume removes one volume's operator-side residue AFTER its terminal outcome is
// durably recorded: the mover Job + creds Secret, then the exposure objects of BOTH
// mechanisms — the kind-agnostic double cleanup is safe by construction (each Cleanup
// tolerates absence, a delivered transplant is never reclaimed, and a twin cleanup deletes
// only the alias PV object).
func (e *restoreEngine) teardownVolume(ctx context.Context, rc *restoreExecContext, plan *restoreVolumePlan) {
	prefix := restoreNamePrefix(rc.ownerID, plan.pvc)
	deleteJobAndSecret(ctx, e.Client, e.OperatorNamespace, restoreJobName(prefix))
	log := logf.FromContext(ctx)
	for _, kind := range []string{rexposer.KindTwin, rexposer.KindTransplant} {
		if err := e.Targets.Cleanup(ctx, e.exposureFor(rc, plan, kind)); err != nil {
			log.Error(err, "Best-effort restore exposure cleanup failed; leaving to the orphan reaper",
				"owner", rc.ownerName, "pvc", plan.pvc, "kind", kind)
		}
	}
}

// teardownAll tears down every planned volume (post-terminal-write, or on finalize).
func (e *restoreEngine) teardownAll(ctx context.Context, rc *restoreExecContext, plans []restoreVolumePlan) {
	for i := range plans {
		e.teardownVolume(ctx, rc, &plans[i])
	}
	// The mediated-resolution listing Job + Secret self-clean after a successful listing;
	// this catches one still live (or failed) when the restore settles. Name-addressed —
	// the lister's objects carry repository labels, not this owner's.
	deleteJobAndSecret(ctx, e.Client, e.OperatorNamespace, restoreResolveJobName(rc.ownerID))
}

// maintenanceDeps assembles the shared maintenance/creds dependency bundle from the engine.
func (e *restoreEngine) maintenanceDeps() repoMaintenanceDeps {
	return repoMaintenanceDeps{
		Client:            e.Client,
		Secrets:           e.Secrets,
		OperatorNamespace: e.OperatorNamespace,
		MoverImage:        e.MoverImage,
	}
}

// restoreTargetPath resolves a VolumeSelectorItem.targetPath into the restic --target under
// the fixed staging mount. Empty (or "/") means the PVC root. Any ".." SEGMENT is rejected
// outright — the same rule the CRD's CEL enforces at admission, re-checked here as defense
// in depth. The check runs on the RAW segments, before path.Clean: Clean would silently
// neutralize a leading ".." against the root ("../x" → "/x"), and a traversal attempt must
// fail loudly, never be quietly rewritten into something else.
func restoreTargetPath(targetPath string) (string, error) {
	if targetPath == "" || targetPath == "/" {
		return restoreTargetMountPath, nil
	}
	if slices.Contains(strings.Split(targetPath, "/"), "..") {
		return "", fmt.Errorf("targetPath %q contains a '..' segment", targetPath)
	}
	cleaned := path.Clean("/" + targetPath)
	if cleaned == "/" {
		return restoreTargetMountPath, nil
	}
	return restoreTargetMountPath + cleaned, nil
}

// planVolumes intersects a restore's volume selection with the restorable source volumes,
// producing one plan per selected PVC. Selection semantics (spec/02-api.md): a nil volumes
// list selects EVERYTHING; a present list selects a PVC iff ANY item matches, and the FIRST
// matching item's include/exclude/targetPath apply. An item with no names matches every
// PVC. sources maps pvc -> (snapshotID, snapshotPath); ordering follows the caller's sorted
// source iteration so plans are deterministic.
func planVolumes(volumes []cbv1.VolumeSelectorItem, sourcePVCs []string, hasSelection bool) map[string]*cbv1.VolumeSelectorItem {
	selected := make(map[string]*cbv1.VolumeSelectorItem, len(sourcePVCs))
	for _, pvc := range sourcePVCs {
		if !hasSelection {
			selected[pvc] = nil // whole-PVC restore, no narrowing.
			continue
		}
		for i := range volumes {
			if volumeItemMatches(&volumes[i], pvc) {
				selected[pvc] = &volumes[i]
				break // first matching item wins.
			}
		}
	}
	return selected
}

// volumeItemMatches reports whether one selection item selects a PVC: an empty names list
// matches every PVC; otherwise the name must be listed.
func volumeItemMatches(item *cbv1.VolumeSelectorItem, pvc string) bool {
	return len(item.Names) == 0 || slices.Contains(item.Names, pvc)
}

// deleteJobAndSecret best-effort deletes a Job (Background propagation — see
// deleteMoverJobAndSecret's rationale) and its same-named creds Secret, tolerating NotFound.
func deleteJobAndSecret(ctx context.Context, c client.Client, namespace, name string) {
	log := logf.FromContext(ctx)
	background := metav1.DeletePropagationBackground
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
	if err := c.Delete(ctx, job, &client.DeleteOptions{PropagationPolicy: &background}); err != nil && !apierrors.IsNotFound(err) {
		log.Error(err, "Best-effort delete of mover Job failed", "job", name)
	}
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
	if err := c.Delete(ctx, sec); err != nil && !apierrors.IsNotFound(err) {
		log.Error(err, "Best-effort delete of mover creds Secret failed", "secret", name)
	}
}
