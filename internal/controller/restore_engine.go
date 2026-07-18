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

// restoreNamePrefix is the deterministic per-PVC NamePrefix "<owner>-<pvc>" for restore
// objects, sanitized exactly like moverNamePrefix but under the restore cap.
func restoreNamePrefix(ownerName, pvcName string) string {
	return sanitizeDNSName(ownerName+"-"+pvcName, restoreNamePrefixMax)
}

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
	// targetNamespace is where the restored PVCs live.
	targetNamespace string
	// deleteExtras selects the Recreate reconciliation (restic --delete).
	deleteExtras bool
	// restoredFromRun is the originating run (provenance annotation on transplanted PVCs).
	restoredFromRun string
	// Repository access, resolved by the owning controller.
	repoName, repoURL, dek, s3CredsSecret string
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
}

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
	}
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

func (e *restoreEngine) forgetResolution(owner types.UID) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.resolved, owner)
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
func (e *restoreEngine) exposureFor(rc *restoreExecContext, plan *restoreVolumePlan, kind, node string) *rexposer.TargetExposure {
	prefix := restoreNamePrefix(rc.ownerName, plan.pvc)
	return &rexposer.TargetExposure{
		Kind:              kind,
		TargetNamespace:   rc.targetNamespace,
		TargetPVCName:     plan.pvc,
		OperatorNamespace: e.OperatorNamespace,
		StagingPVCName:    rexposer.StagingPVCName(prefix),
		TwinPVName:        rexposer.TwinPVName(prefix),
		NodeName:          node,
		Labels:            e.volumeLabels(rc, plan.pvc),
		RestoredFromRun:   rc.restoredFromRun,
	}
}

// adviseVolume derives one volume's state from live objects and advances it by at most one
// step: expose the target, admit + create the mover Job, poll it, then finalize the
// handover. It never writes CR status (the caller aggregates outcomes and is the single
// status writer). Every path is idempotent — a restarted controller re-derives the same
// step from the same objects.
func (e *restoreEngine) adviseVolume(ctx context.Context, rc *restoreExecContext, plan *restoreVolumePlan) (volumeOutcome, error) {
	prefix := restoreNamePrefix(rc.ownerName, plan.pvc)
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

	result, _, rerr := readMoverResult(ctx, e.Client, e.OperatorNamespace, jobName)
	if failed || rerr != nil || !result.OK {
		// A BLANK/unparseable termination message means the mover was hard-killed and may
		// have died holding the repository lock — clear it exactly as the backup path does.
		if rerr != nil {
			enqueueRepoMaintenance(ctx, e.Queue, e.maintenanceDeps(), rc.repoName, queue.OpUnlock,
				maintenanceResourceName(prefix, "unlock"), mover.OpUnlock, restic.UnlockArgs(),
				rc.repoURL, rc.dek, rc.s3CredsSecret)
		}
		return volumeOutcome{settled: true, failed: true, reason: moverFailureReason(result, rerr)}, nil
	}

	// The data landed in the staging volume; complete the handover. The mechanism kind was
	// pinned on the Job at creation — never re-resolved from the (mutating) target state.
	ex := e.exposureFor(rc, plan, job.Labels[apiconst.LabelExposureKind], "")
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
	case err == rexposer.ErrTargetUnbound:
		// An existing-but-unbound claim holds no volume and no data. The operation is
		// destructive by contract (R23 confirmation was required to get here), so replace it:
		// delete now, re-drive next pass into the transplant path, which recreates it bound.
		return volumeOutcome{}, e.deleteUnboundTarget(ctx, rc, plan.pvc)
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

	if err := ensureMoverCredsSecret(ctx, e.maintenanceDeps(), jobName, rc.dek, rc.s3CredsSecret, labels); err != nil {
		return volumeOutcome{}, err
	}

	target, err := restoreTargetPath(plan.targetPath)
	if err != nil {
		return volumeOutcome{settled: true, failed: true, reason: "InvalidTargetPath"}, nil
	}
	jobLabels := copyStringMap(labels)
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
		TTLSeconds:   moverJobTTLSeconds,
		Labels:       jobLabels,
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
	return volumeOutcome{}, nil
}

// deleteUnboundTarget removes an existing-but-unbound target claim so the transplant path
// can recreate it bound (see startVolume). Best-effort semantics are wrong here — a failed
// delete must surface, or the volume would wedge in the unbound gate forever.
func (e *restoreEngine) deleteUnboundTarget(ctx context.Context, rc *restoreExecContext, pvcName string) error {
	claim := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: pvcName, Namespace: rc.targetNamespace},
	}
	if err := e.Delete(ctx, claim); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete unbound restore target PVC %s/%s: %w", rc.targetNamespace, pvcName, err)
	}
	return nil
}

// teardownVolume removes one volume's operator-side residue AFTER its terminal outcome is
// durably recorded: the mover Job + creds Secret, then the exposure objects of BOTH
// mechanisms — the kind-agnostic double cleanup is safe by construction (each Cleanup
// tolerates absence, a delivered transplant is never reclaimed, and a twin cleanup deletes
// only the alias PV object).
func (e *restoreEngine) teardownVolume(ctx context.Context, rc *restoreExecContext, plan *restoreVolumePlan) {
	prefix := restoreNamePrefix(rc.ownerName, plan.pvc)
	deleteJobAndSecret(ctx, e.Client, e.OperatorNamespace, restoreJobName(prefix))
	log := logf.FromContext(ctx)
	for _, kind := range []string{rexposer.KindTwin, rexposer.KindTransplant} {
		if err := e.Targets.Cleanup(ctx, e.exposureFor(rc, plan, kind, "")); err != nil {
			log.Error(err, "best-effort restore exposure cleanup failed; leaving to the orphan reaper",
				"owner", rc.ownerName, "pvc", plan.pvc, "kind", kind)
		}
	}
}

// teardownAll tears down every planned volume (post-terminal-write, or on finalize).
func (e *restoreEngine) teardownAll(ctx context.Context, rc *restoreExecContext, plans []restoreVolumePlan) {
	for i := range plans {
		e.teardownVolume(ctx, rc, &plans[i])
	}
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
		log.Error(err, "best-effort delete of mover job failed", "job", name)
	}
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
	if err := c.Delete(ctx, sec); err != nil && !apierrors.IsNotFound(err) {
		log.Error(err, "best-effort delete of mover creds secret failed", "secret", name)
	}
}

// copyStringMap returns an independent copy of in (nil-safe).
func copyStringMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	maps.Copy(out, in)
	return out
}
