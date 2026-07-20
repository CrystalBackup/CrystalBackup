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
	"github.com/CrystalBackup/CrystalBackup/internal/manifests"
	"github.com/CrystalBackup/CrystalBackup/internal/mover"
	"github.com/CrystalBackup/CrystalBackup/internal/restic"
	"github.com/CrystalBackup/CrystalBackup/internal/status"
)

// The cluster-manifests capture (R22, adr/0011 §1): one Job per ClusterBackup RUN dumps the
// cluster's cluster-scoped objects into a kind=cluster-manifests snapshot. It is the run-level
// twin of the per-namespace manifest capture (manifests_phase.go), and it obeys the same two
// rules that half had to learn: teardown runs only AFTER the terminal status write is durable,
// and the run does not go terminal while the capture is still in flight.
//
// It is the ClusterBackup's OWN half, not a child's: the snapshot belongs to no tenant or
// namespace (restic.ClusterManifestsIdentity carries neither), so it is captured once for the
// run rather than fanned out.

// ConditionClusterManifestsComplete reports whether the cluster-scoped capture enumerated
// everything. Separate from the run's phase for the same reason as the namespaced condition: a
// partial cluster capture is not a failed run — the namespaces' data is there — but it is not
// the complete platform recovery the admin will assume either.
const ConditionClusterManifestsComplete = "ClusterManifestsComplete"

const (
	reasonClusterManifestsCaptured = "Captured"
	reasonClusterManifestsPartial  = "PartiallyEnumerated"
	reasonClusterManifestsFailed   = "CaptureFailed"
)

// clusterManifestsJobName is the deterministic name of a run's one capture Job, so a reconcile
// after a restart finds the Job it already created instead of starting a second capture.
func clusterManifestsJobName(runName string) string {
	return sanitizeDNSName(runName+"-cluster-manifests", moverNamePrefixMax) + "-mover"
}

// captureClusterManifests resolves the run's opt-in, which DEFAULTS TO TRUE on the cluster plane
// (adr/0011 §1): a platform backup without its cluster-scoped objects restores namespaces that
// reference StorageClasses and CRDs which do not exist, so the safe default is to capture and the
// explicit act is to opt out.
func captureClusterManifests(cb *cbv1.ClusterBackup) bool {
	e := cb.Spec.ClusterResources.Enabled
	return e == nil || *e
}

// clusterCaptureContext is the run's resolved, repository-facing inputs for the capture.
type clusterCaptureContext struct {
	clusterID     string
	scheduleRef   string
	run           string
	repoURL       string
	dek           string
	s3CredsSecret string
	include       []string
	exclude       []string
}

// advanceClusterManifests drives the run's cluster-scoped capture one step. It returns true when
// the capture has reached a terminal state (captured or failed) and — on the pass that records
// that result — the Job name whose residue is due for teardown.
//
// It does NOT tear down itself: the Job and its cluster-scoped grant are the only durable record
// that the capture happened, so they must outlive an unpersisted status write, exactly as the
// namespaced half does.
func (r *ClusterBackupReconciler) advanceClusterManifests(
	ctx context.Context,
	cb *cbv1.ClusterBackup,
	cc *clusterCaptureContext,
) (done bool, teardownJob string, err error) {
	log := logf.FromContext(ctx).WithName("cluster-manifests")

	// Already terminal: a snapshot recorded, or a recorded failure. Re-entering would start a
	// second capture of a run that already has one.
	if cb.Status.ClusterManifests != nil {
		return true, "", nil
	}
	if c := status.FindCondition(cb.Status.Conditions, ConditionClusterManifestsComplete); c != nil &&
		c.Reason == reasonClusterManifestsFailed {
		return true, "", nil
	}

	jobName := clusterManifestsJobName(cb.Name)

	var job batchv1.Job
	err = r.Get(ctx, client.ObjectKey{Namespace: r.OperatorNamespace, Name: jobName}, &job)
	switch {
	case apierrors.IsNotFound(err):
		return false, "", r.startClusterManifestsJob(ctx, cb, cc, jobName)
	case err != nil:
		return false, "", fmt.Errorf("get cluster-manifest Job %s/%s: %w", r.OperatorNamespace, jobName, err)
	}

	if job.Status.Succeeded == 0 && job.Status.Failed == 0 {
		return false, "", nil // still running
	}

	// Terminal. Read the verdict BEFORE tearing anything down: it lives in the pod's termination
	// message, which disappears with the pod.
	result, _, readErr := readMoverResult(ctx, r.Client, r.OperatorNamespace, jobName)
	switch {
	case readErr != nil:
		// A blank message means the mover was killed before it could report (OOM, SIGKILL). That
		// is a failure, not an empty success — a cluster-manifests snapshot nobody can vouch for
		// is worse than none, because it looks like one.
		r.recordClusterManifestsFailure(cb, fmt.Sprintf("could not read the mover result: %v", readErr))
	case !result.OK:
		r.recordClusterManifestsFailure(cb, result.Error)
	default:
		cb.Status.ClusterManifests = &cbv1.ManifestsStatus{
			SnapshotID:    result.SnapshotID,
			ResourceCount: result.ResourceCount,
		}
		if result.IncompleteManifests {
			status.SetCondition(&cb.Status.Conditions, ConditionClusterManifestsComplete, metav1.ConditionFalse, reasonClusterManifestsPartial,
				fmt.Sprintf("captured %d cluster-scoped resource(s); some kinds could not be enumerated — "+
					"see the warnings in index.json of snapshot %s", result.ResourceCount, result.SnapshotID), cb.Generation)
		} else {
			status.SetCondition(&cb.Status.Conditions, ConditionClusterManifestsComplete, metav1.ConditionTrue, reasonClusterManifestsCaptured,
				fmt.Sprintf("captured %d cluster-scoped resource(s)", result.ResourceCount), cb.Generation)
		}
		log.Info("cluster manifests captured", "run", cb.Name,
			"snapshot", result.SnapshotID, "resources", result.ResourceCount,
			"complete", !result.IncompleteManifests)
	}

	// The verdict is recorded in memory but not yet persisted, so hand the teardown back to the
	// caller to run once the status write lands.
	return true, jobName, nil
}

// startClusterManifestsJob creates the creds Secret, then the cluster-scoped grant, then the Job.
// The order is not arbitrary: a Job that starts without its binding fails on its first API call,
// having already consumed an attempt against the backoff limit.
func (r *ClusterBackupReconciler) startClusterManifestsJob(
	ctx context.Context,
	cb *cbv1.ClusterBackup,
	cc *clusterCaptureContext,
	jobName string,
) error {
	// Run-identity labels. The mover-role label is what the NetworkPolicy selects on to grant
	// API-server egress to this pod and no other mover (spec/03 §7). No namespace label: the
	// capture belongs to the whole cluster, not a tenant.
	labels := map[string]string{
		apiconst.LabelManagedBy:     apiconst.ManagedByValue,
		apiconst.LabelClusterBackup: cb.Name,
		apiconst.LabelMoverRole:     apiconst.MoverRoleManifest,
	}

	if err := ensureMoverCredsSecret(ctx, repoMaintenanceDeps{
		Client: r.Client, Secrets: r.Secrets, OperatorNamespace: r.OperatorNamespace,
	}, jobName, cc.dek, cc.s3CredsSecret, labels); err != nil {
		return err
	}

	if err := ensureClusterManifestBinding(ctx, r.Client, clusterManifestRBACRequest{
		JobName:            jobName,
		ClusterRoleName:    r.ClusterManifestReaderClusterRole,
		ServiceAccountName: r.ManifestMoverServiceAccount,
		OperatorNamespace:  r.OperatorNamespace,
	}); err != nil {
		return err
	}

	selection, err := manifests.EncodeClusterCaptureOptions(manifests.ClusterCaptureOptions{
		Include: cc.include,
		Exclude: cc.exclude,
	})
	if err != nil {
		return err
	}

	id := restic.ClusterManifestsIdentity(cc.clusterID, cc.scheduleRef, cc.run)
	job := mover.BuildJob(mover.JobRequest{
		Name:      jobName,
		Namespace: r.OperatorNamespace,
		Image:     r.MoverImage,
		Operation: mover.OpClusterManifestsBackup,
		// The identity's Path (/cluster-manifests) is both what restic records and where the dump
		// writes — one string, derived once.
		ResticArgs: resticBackupArgs(id),
		RepoURL:    cc.repoURL,
		SecretName: jobName,
		Labels:     labels,
		// The one mover that reaches the API server (I6's sole exception).
		ServiceAccountName: r.ManifestMoverServiceAccount,
		ManifestsVolume:    true,
		// The dump writes to /cluster-manifests, so the scratch emptyDir must be mounted there —
		// the default (ManifestsRoot) would leave the dump writing to a read-only filesystem.
		ManifestsMountPath: mover.ClusterManifestsRoot,
		ExtraEnv: []corev1.EnvVar{
			{Name: mover.EnvClusterManifestsClusterID, Value: cc.clusterID},
			{Name: mover.EnvClusterManifestsBackupName, Value: cc.run},
			{Name: mover.EnvClusterManifestsSelection, Value: selection},
		},
		TTLSeconds: moverJobTTLSeconds,
	})
	if err := r.Create(ctx, job); err != nil && !apierrors.IsAlreadyExists(err) {
		// Leave no grant behind for a Job that never started.
		if delErr := deleteClusterManifestBinding(ctx, r.Client, jobName); delErr != nil {
			logf.FromContext(ctx).Error(delErr, "rolling back the transient ClusterRoleBinding after a failed Job create")
		}
		return fmt.Errorf("create cluster-manifest Job %s/%s: %w", r.OperatorNamespace, jobName, err)
	}
	return nil
}

// teardownClusterManifests reclaims one finished capture's residue, AFTER the terminal status
// write has persisted. The grant goes first: a leftover Job is inert, a leftover
// ClusterRoleBinding is a live (enumerated) read of the whole cluster.
func (r *ClusterBackupReconciler) teardownClusterManifests(ctx context.Context, jobName string) {
	if err := deleteClusterManifestBinding(ctx, r.Client, jobName); err != nil {
		logf.FromContext(ctx).WithName("cluster-manifests").Error(err,
			"deleting the transient ClusterRoleBinding; the reaper will pick it up", "job", jobName)
	}
	deleteJobAndSecret(ctx, r.Client, r.OperatorNamespace, jobName)
}

func (r *ClusterBackupReconciler) recordClusterManifestsFailure(cb *cbv1.ClusterBackup, reason string) {
	if reason == "" {
		reason = "the cluster-manifest mover reported a failure with no reason"
	}
	status.SetCondition(&cb.Status.Conditions, ConditionClusterManifestsComplete, metav1.ConditionFalse,
		reasonClusterManifestsFailed, reason, cb.Generation)
}
