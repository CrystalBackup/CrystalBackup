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

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
	"github.com/CrystalBackup/CrystalBackup/internal/mover"
	"github.com/CrystalBackup/CrystalBackup/internal/restic"
	"github.com/CrystalBackup/CrystalBackup/internal/status"
)

// ConditionManifestsComplete reports whether the manifest dump enumerated everything it set out
// to. It is separate from the Backup's phase on purpose: a namespace whose manifests are
// PARTIAL is not a failed backup — the volumes are there and most kinds captured — but it is
// also not the complete recovery the user will assume they have. Only a condition can say
// "succeeded, with something you need to know" (spec/04-manifest-backup.md §2.1.5).
const ConditionManifestsComplete = "ManifestsComplete"

// manifestsJobPrefix is the deterministic name stem of a namespace's manifest mover Job, so a
// reconcile after a restart finds the Job it already created instead of starting a second one.
func manifestsJobPrefix(namespace, backupName string) string {
	return sanitizeDNSName(namespace+"-"+backupName+"-manifests", moverNamePrefixMax)
}

// manifestsJobName is the Job that a name stem owns. Derived in ONE place so the reconcile
// that creates it and the teardown that removes its grant cannot drift apart — the grant is
// named after the Job, so a mismatch here would leak a privilege silently.
func manifestsJobName(prefix string) string { return prefix + "-mover" }

// manifestsLabels are the run-identity labels on the manifest Job and its creds Secret. The
// mover-role label is what the NetworkPolicy selects on to grant API-server egress to this pod
// and to no other mover (spec/03 §7).
func manifestsLabels(backup *cbv1.Backup) map[string]string {
	return map[string]string{
		apiconst.LabelManagedBy:     apiconst.ManagedByValue,
		apiconst.LabelClusterBackup: backup.Labels[apiconst.LabelClusterBackup],
		apiconst.LabelNamespace:     backup.Namespace,
		apiconst.LabelMoverRole:     apiconst.MoverRoleManifest,
	}
}

// advanceManifests drives the namespace's manifest capture one step. It returns true when the
// capture has reached a terminal state (captured or failed), so the caller knows not to keep
// waiting on it, and — on the pass that records that terminal result — the Job name whose
// residue is now due for teardown.
//
// It does NOT tear down itself. The mover Job and its transient grant are the only durable
// record that this capture happened, so they must outlive an unpersisted status write: if the
// caller's Status().Update loses a conflict after we had already deleted the Job, the next
// reconcile would find no Job, start a SECOND dump of the namespace, and re-create the grant it
// had just removed. Teardown therefore belongs after the status write, exactly as it does for
// the volume half (backup_controller.go step 11).
//
// It runs INDEPENDENTLY of the per-PVC volume flow. A PVC the CSI driver cannot snapshot is
// reported Skipped and the namespace still gets its manifests (02-api.md): the two halves of a
// backup fail for unrelated reasons, and coupling them would lose one to the other's bad day.
func (r *BackupReconciler) advanceManifests(
	ctx context.Context,
	backup *cbv1.Backup,
	rc *backupRunContext,
	includeManifests bool,
	excludeSecretData bool,
) (done bool, teardownJob string, err error) {
	log := logf.FromContext(ctx).WithName("manifests")

	if !includeManifests {
		return true, "", nil
	}
	// Already terminal: a snapshot id recorded, or a recorded failure. Re-entering would start
	// a second dump of a namespace that already has one.
	if backup.Status.Manifests != nil {
		return true, "", nil
	}
	if c := status.FindCondition(backup.Status.Conditions, ConditionManifestsComplete); c != nil &&
		c.Reason == reasonManifestsFailed {
		return true, "", nil
	}

	prefix := manifestsJobPrefix(backup.Namespace, backup.Name)
	jobName := manifestsJobName(prefix)

	var job batchv1.Job
	err = r.Get(ctx, client.ObjectKey{Namespace: r.OperatorNamespace, Name: jobName}, &job)
	switch {
	case apierrors.IsNotFound(err):
		return false, "", r.startManifestsJob(ctx, backup, rc, jobName, excludeSecretData)
	case err != nil:
		return false, "", fmt.Errorf("get manifest mover Job %s/%s: %w", r.OperatorNamespace, jobName, err)
	}

	if job.Status.Succeeded == 0 && job.Status.Failed == 0 {
		return false, "", nil // still running
	}

	// Terminal. Read the result BEFORE tearing anything down: the verdict lives in the pod's
	// termination message, which disappears with the pod.
	result, _, readErr := readMoverResult(ctx, r.Client, r.OperatorNamespace, jobName)
	switch {
	case readErr != nil:
		// A blank message means the mover was killed before it could report (OOM, SIGKILL).
		// That is a failure, not an empty success — a manifest snapshot nobody can vouch for
		// is worse than none, because it looks like one.
		r.recordManifestsFailure(backup, fmt.Sprintf("could not read the mover result: %v", readErr))
	case !result.OK:
		r.recordManifestsFailure(backup, result.Error)
	default:
		backup.Status.Manifests = &cbv1.ManifestsStatus{
			SnapshotID:    result.SnapshotID,
			ResourceCount: result.ResourceCount,
		}
		if result.IncompleteManifests {
			// Captured, but not everything. The detail is in the snapshot's index.json, which
			// outlives this object; the condition is what makes someone go and look.
			status.SetCondition(&backup.Status.Conditions, ConditionManifestsComplete, metav1.ConditionFalse, reasonManifestsPartial,
				fmt.Sprintf("captured %d resource(s); some kinds could not be enumerated — "+
					"see the warnings in index.json of snapshot %s", result.ResourceCount, result.SnapshotID), backup.Generation)
		} else {
			status.SetCondition(&backup.Status.Conditions, ConditionManifestsComplete, metav1.ConditionTrue, reasonManifestsComplete,
				fmt.Sprintf("captured %d resource(s)", result.ResourceCount), backup.Generation)
		}
		log.Info("manifests captured", "namespace", backup.Namespace,
			"snapshot", result.SnapshotID, "resources", result.ResourceCount,
			"complete", !result.IncompleteManifests)
	}

	// The verdict is recorded in memory but not yet persisted, so hand the teardown back to the
	// caller to run once the status write lands. The name stem is enough: the Job, its creds
	// Secret and the grant are all derived from it.
	return true, prefix, nil
}

// teardownManifests reclaims one finished capture's residue. It is called ONLY after the
// terminal status write has persisted.
//
// The order matters and is not the obvious one: the grant goes FIRST. If the process dies
// between the two deletes, a leftover Job is inert — it has already run — while a leftover
// RoleBinding is a live read (or, on restore, write) privilege in a tenant namespace.
// Best-effort throughout; the reaper is the backstop for whatever a crash leaves behind
// (spec/03-security-and-tenancy.md §5).
func (r *BackupReconciler) teardownManifests(ctx context.Context, backup *cbv1.Backup, prefix string) {
	jobName := manifestsJobName(prefix)
	if err := deleteManifestRoleBinding(ctx, r.Client, backup.Namespace, jobName); err != nil {
		logf.FromContext(ctx).WithName("manifests").Error(err,
			"deleting the transient RoleBinding; the reaper will pick it up",
			"namespace", backup.Namespace, "job", jobName)
	}
	r.deleteMoverJobAndSecret(ctx, prefix)
}

// startManifestsJob creates the grant, then the Job. The order is not arbitrary: a Job that
// starts without its binding fails on its first API call, having already consumed an attempt
// against the backoff limit.
func (r *BackupReconciler) startManifestsJob(
	ctx context.Context,
	backup *cbv1.Backup,
	rc *backupRunContext,
	jobName string,
	excludeSecretData bool,
) error {
	labels := manifestsLabels(backup)

	if err := ensureMoverCredsSecret(ctx, repoMaintenanceDeps{
		Client: r.Client, Secrets: r.Secrets, OperatorNamespace: r.OperatorNamespace,
	}, jobName, rc.dek, rc.s3CredsSecret, labels); err != nil {
		return err
	}

	if err := ensureManifestRoleBinding(ctx, r.Client, manifestRBACRequest{
		TargetNamespace:    backup.Namespace,
		JobName:            jobName,
		ClusterRoleName:    r.ManifestReaderClusterRole,
		ServiceAccountName: r.ManifestMoverServiceAccount,
		OperatorNamespace:  r.OperatorNamespace,
	}); err != nil {
		return err
	}

	id := restic.ManifestsIdentity(rc.clusterID, rc.tenant, backup.Namespace, rc.scheduleRef, rc.run)
	job := mover.BuildJob(mover.JobRequest{
		Name:      jobName,
		Namespace: r.OperatorNamespace,
		Image:     r.MoverImage,
		Operation: mover.OpManifestsBackup,
		// The identity's Path is both what restic records and where the dump writes
		// (mover.ManifestsRoot + "/" + namespace) — one string, derived once.
		ResticArgs: resticBackupArgs(id),
		RepoURL:    rc.repoURL,
		SecretName: jobName,
		Labels:     labels,
		// The one mover that reaches the API server (I6's sole exception).
		ServiceAccountName: r.ManifestMoverServiceAccount,
		ManifestsVolume:    true,
		ExtraEnv: []corev1.EnvVar{
			{Name: mover.EnvManifestsNamespace, Value: backup.Namespace},
			{Name: mover.EnvManifestsClusterID, Value: rc.clusterID},
			{Name: mover.EnvManifestsBackupName, Value: rc.run},
			{Name: mover.EnvManifestsExcludeSecretData, Value: fmt.Sprintf("%t", excludeSecretData)},
		},
		BackoffLimit: rc.backoffLimit,
		TTLSeconds:   moverJobTTLSeconds,
	})
	if err := r.Create(ctx, job); err != nil && !apierrors.IsAlreadyExists(err) {
		// Leave no grant behind for a Job that never started.
		if delErr := deleteManifestRoleBinding(ctx, r.Client, backup.Namespace, jobName); delErr != nil {
			logf.FromContext(ctx).Error(delErr, "rolling back the transient RoleBinding after a failed Job create")
		}
		return fmt.Errorf("create manifest mover Job %s/%s: %w", r.OperatorNamespace, jobName, err)
	}
	return nil
}

func (r *BackupReconciler) recordManifestsFailure(backup *cbv1.Backup, reason string) {
	if reason == "" {
		reason = "the manifest mover reported a failure with no reason"
	}
	status.SetCondition(&backup.Status.Conditions, ConditionManifestsComplete, metav1.ConditionFalse,
		reasonManifestsFailed, reason, backup.Generation)
}

const (
	reasonManifestsComplete = "Captured"
	reasonManifestsPartial  = "PartiallyEnumerated"
	reasonManifestsFailed   = "CaptureFailed"
)
