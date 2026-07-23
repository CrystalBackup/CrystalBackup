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
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
	"github.com/CrystalBackup/CrystalBackup/internal/manifests"
	"github.com/CrystalBackup/CrystalBackup/internal/mover"
	"github.com/CrystalBackup/CrystalBackup/internal/restic"
)

// The CLUSTER-SCOPED half of a ClusterRestore (adr/0011 §2): a manifest-mover Job restores the
// run's kind=cluster-manifests snapshot and APPLIES its cluster-scoped objects — CRDs,
// StorageClasses, IngressClasses, cluster RBAC, PVs — under a transient ClusterRoleBinding to
// crystal-cluster-manifest-writer.
//
// It is the cluster-plane twin of the namespaced resources[] half (restore_resources.go), and it
// runs BEFORE the volumes: a namespace's PVCs reference StorageClasses this half brings back, so
// the classes must exist first (which is why the ClusterRestore sequences the two — see Reconcile).
//
// That write grant — create/update/delete on CRDs and cluster RBAC — is the most privileged act
// this tool performs, and unlike the namespaced writer a ClusterRoleBinding has no namespace to
// confine it (cluster_manifest_rbac.go): the enumerated crystal-cluster-manifest-writer role IS the
// boundary. Everything here keeps it transient — created immediately before the Job, reclaimed the
// moment the Job reaches a terminal state — and, as with every other mover half, the teardown runs
// only AFTER the durable status write, never inline.

// clusterRestoreResourcesPlan is the resolved cluster-scoped half of one ClusterRestore, or nil
// when the restore opts out (spec.clusterResources omitted).
type clusterRestoreResourcesPlan struct {
	// snapshotID is the run's kind=cluster-manifests snapshot, resolved from a separate listing.
	snapshotID string
	// snapshotPath is its recorded subtree (/cluster-manifests), read off the snapshot itself.
	snapshotPath string
	// selection is what the mover will narrow the tree to.
	selection manifests.Selection
	// mode and dryRun come straight from the CR. There is NO storageClassMapping: a PV's class is
	// not remapped on the cluster plane (a PV is a cluster object, restored as captured — the
	// per-namespace class remap applies to the volumes' PVCs, an engine decision, not here).
	mode   string
	dryRun bool
}

// resolveClusterRestorePlan decides whether this ClusterRestore has a cluster-scoped half and, if
// so, pins the snapshot it reads.
//
// The opt-in is a POINTER, not a tri-state list: spec.clusterResources omitted is the safe default
// of "nothing cluster-scoped" (adr/0011 §2), and its mere presence is the explicit, admin-only
// opt-in. So a non-nil clusterResources with an EMPTY include restores every cluster-scoped object
// in the snapshot — the snapshot is already the curated capture, and narrowing is what a non-empty
// include is for. Exclude always subtracts.
func resolveClusterRestorePlan(cr *cbv1.ClusterRestore, snaps []restic.Snapshot) (*clusterRestoreResourcesPlan, bool) {
	if cr.Spec.ClusterResources == nil {
		// Opt-out (the default): nothing cluster-scoped is restored, and that is not an error.
		return nil, true
	}

	snap, ok := clusterManifestsRestoreSnapshot(snaps)
	if !ok {
		// Opted in, but this run has no cluster-manifests snapshot — the normal shape of a run
		// captured with clusterResources disabled. Not an error; there is simply nothing
		// cluster-scoped to restore, and the volumes still are exactly what the admin asked for.
		return nil, false
	}

	plan := &clusterRestoreResourcesPlan{
		snapshotID:   snap.ID,
		snapshotPath: clusterManifestsRestoreSnapshotPath(snap),
		mode:         string(cr.Spec.Mode),
		dryRun:       cr.Spec.DryRun,
	}

	// The arbitration. An empty include with no exclude is "restore the whole curated capture"
	// (All) — NOT "restore nothing", because the pointer's presence already said "restore the
	// cluster-scoped objects". A present include narrows; an exclude subtracts. Exclude-only must
	// NOT set All (a compiled selection short-circuits to true on All and never consults the
	// exclude), so it is expressed as one item with an empty include — which matches every kind —
	// minus the exclude, exactly as the namespaced resources[] half expresses a present list.
	include := cr.Spec.ClusterResources.Include
	exclude := cr.Spec.ClusterResources.Exclude
	if len(include) == 0 && len(exclude) == 0 {
		plan.selection = manifests.Selection{All: true}
	} else {
		plan.selection = manifests.Selection{Items: []manifests.SelectionItem{{
			Include: include,
			Exclude: exclude,
		}}}
	}
	return plan, true
}

// clusterManifestsRestoreSnapshot picks the kind=cluster-manifests snapshot out of a listing,
// newest wins. Unlike manifestsSnapshot, this listing is NOT namespace-filtered (a cluster-manifests
// snapshot carries no namespace tag), so the kind check here is what separates it from anything else
// the run=<run> filter returned.
func clusterManifestsRestoreSnapshot(snaps []restic.Snapshot) (restic.Snapshot, bool) {
	var newest restic.Snapshot
	var found bool
	for _, s := range snaps {
		if kind, _ := restic.TagValue(s.Tags, restic.TagKeyKind); kind != restic.KindClusterManifests {
			continue
		}
		if !found || s.Time.After(newest.Time) {
			newest, found = s, true
		}
	}
	return newest, found
}

// clusterManifestsRestoreSnapshotPath is the subtree to restore. restic records the absolute path
// it backed up (/cluster-manifests), and the snapshot carries it — so read it rather than rebuilding
// the constant from the mover package, which keeps the restore addressing the exact path the capture
// wrote even if the fixed path ever changes across releases.
func clusterManifestsRestoreSnapshotPath(snap restic.Snapshot) string {
	if len(snap.Paths) > 0 {
		return snap.Paths[0]
	}
	return mover.ClusterManifestsRoot
}

// clusterRestoreJobName is the deterministic name of a ClusterRestore's one cluster-scoped Job, so a
// reconcile after a restart finds the Job it already created instead of applying the cluster objects
// twice. Distinct from resourcesJobPrefix's "-resources" stem so a ClusterRestore's (future)
// namespaced half and its cluster-scoped half can never derive the same name.
func clusterRestoreJobName(ownerID string) string {
	return sanitizeDNSName(ownerID+"-cluster-resources", moverNamePrefixMax) + "-mover"
}

// advanceClusterRestore drives the cluster-scoped half one step, returning true once it has settled
// and — on the pass that records that result — the Job name whose residue is due for teardown.
//
// Like every other mover half it does NOT tear down itself: the Job and its cluster-wide grant are
// the only durable record that the apply happened, so they must outlive an unpersisted status write.
// Deleting them before the write lands would let the next reconcile find no Job and RE-APPLY every
// cluster-scoped object — in Overwrite or Recreate mode, against CRDs and cluster RBAC it just wrote.
func (r *ClusterRestoreReconciler) advanceClusterRestore(
	ctx context.Context,
	cr *cbv1.ClusterRestore,
	rc *restoreExecContext,
	plan *clusterRestoreResourcesPlan,
) (done bool, teardownJob string, err error) {
	if plan == nil {
		return true, "", nil
	}
	// Already terminal: a report recorded. Re-entering would apply the cluster objects a second time.
	if cr.Status.Resources != nil {
		return true, "", nil
	}

	jobName := clusterRestoreJobName(rc.ownerID)

	var job batchv1.Job
	err = r.Get(ctx, client.ObjectKey{Namespace: r.OperatorNamespace, Name: jobName}, &job)
	switch {
	case apierrors.IsNotFound(err):
		return false, "", r.startClusterRestoreJob(ctx, rc, plan, jobName)
	case err != nil:
		return false, "", fmt.Errorf("get cluster-manifest restore Job %s/%s: %w", r.OperatorNamespace, jobName, err)
	}

	if job.Status.Succeeded == 0 && job.Status.Failed == 0 {
		return false, "", nil // still applying
	}

	// Terminal. Read the verdict BEFORE anything is torn down: it lives in the pod's termination
	// message, which disappears with the pod.
	result, _, readErr := readMoverResult(ctx, r.Client, r.OperatorNamespace, jobName)
	switch {
	case readErr != nil:
		// A blank message means the mover was killed mid-apply (OOM, SIGKILL). The cluster is in an
		// UNKNOWN state — some objects applied, some not — and saying so is the only honest answer.
		// Reporting zero applied would be a specific claim we cannot make.
		cr.Status.Resources = &cbv1.RestoreResourcesStatus{
			FailedCount: 1,
			Entries: []cbv1.RestoreResourceEntry{{
				Outcome: cbv1.RestoreResourceFailed,
				Reason: fmt.Sprintf("the cluster-scoped restore did not report a result (%v); "+
					"some resources may have been applied — see the mover's pod log", readErr),
			}},
		}
	case !result.OK:
		cr.Status.Resources = &cbv1.RestoreResourcesStatus{
			FailedCount: 1,
			Entries: []cbv1.RestoreResourceEntry{{
				Outcome: cbv1.RestoreResourceFailed,
				Reason:  clampMessage(result.Error),
			}},
		}
	default:
		cr.Status.RestoredResources = result.RestoredResources
		cr.Status.Resources = resourcesStatusFrom(result)
		logf.FromContext(ctx).WithName("cluster-resources").Info("cluster-scoped resources applied",
			"applied", result.RestoredResources, "failed", result.FailedResources,
			"skipped", result.SkippedResources, "dryRun", plan.dryRun)
	}
	return true, jobName, nil
}

// startClusterRestoreJob creates the creds Secret, then the cluster-scoped grant, then the Job. The
// order is not arbitrary: a Job that starts without its binding fails on its first API call, having
// already consumed an attempt against the backoff limit.
func (r *ClusterRestoreReconciler) startClusterRestoreJob(
	ctx context.Context,
	rc *restoreExecContext,
	plan *clusterRestoreResourcesPlan,
	jobName string,
) error {
	// Owner-identity labels. The mover-role label is what the NetworkPolicy selects on to grant
	// API-server egress to this pod and no other mover (spec/03 §7). No namespace label: the
	// cluster-scoped apply belongs to the whole cluster, not a tenant.
	labels := map[string]string{
		apiconst.LabelManagedBy: apiconst.ManagedByValue,
		rc.ownerLabelKey:        rc.ownerName,
		apiconst.LabelMoverRole: apiconst.MoverRoleManifest,
	}

	if err := ensureMoverCredsSecret(ctx, repoMaintenanceDeps{
		Client: r.Client, Secrets: r.Engine.Secrets, OperatorNamespace: r.OperatorNamespace,
	}, jobName, rc.dek, rc.s3CredsSecret, labels); err != nil {
		return err
	}

	if err := ensureClusterManifestBinding(ctx, r.Client, clusterManifestRBACRequest{
		JobName:            jobName,
		ClusterRoleName:    r.ClusterManifestWriterClusterRole,
		ServiceAccountName: r.ManifestMoverServiceAccount,
		OperatorNamespace:  r.OperatorNamespace,
	}); err != nil {
		return err
	}

	env, err := clusterRestoreJobEnv(plan)
	if err != nil {
		return err
	}

	job := mover.BuildJob(mover.JobRequest{
		Name:      jobName,
		Namespace: r.OperatorNamespace,
		Image:     r.Engine.MoverImage,
		Operation: mover.OpClusterManifestsRestore,
		ResticArgs: restic.ManifestsRestoreArgs(plan.snapshotID, plan.snapshotPath,
			mover.ClusterManifestsRestoreDir),
		RepoURL:    rc.repoURL,
		SecretName: jobName,
		Labels:     labels,
		// The one mover that reaches the API server (I6's sole exception).
		ServiceAccountName: r.ManifestMoverServiceAccount,
		ManifestsVolume:    true,
		// The restore lands the tree under ClusterManifestsRestoreDir, a subdirectory of
		// ClusterManifestsRoot, so the scratch emptyDir must be mounted at ClusterManifestsRoot —
		// the default (ManifestsRoot) would leave the restore writing to a read-only filesystem.
		ManifestsMountPath: mover.ClusterManifestsRoot,
		ExtraEnv:           env,
		BackoffLimit:       restoreMoverBackoffLimit,
		// NO TTL, for the same reason as the namespaced resources[] half: this Job IS the durable
		// record that the apply ran. If the TTL controller erased it before the status write
		// landed, the next pass would re-derive "never started" and re-apply every cluster object.
		TTLSeconds: mover.NoTTL,
	})
	if err := r.Create(ctx, job); err != nil && !apierrors.IsAlreadyExists(err) {
		// Leave no grant behind for a Job that never started.
		if delErr := deleteClusterManifestBinding(ctx, r.Client, jobName); delErr != nil {
			logf.FromContext(ctx).Error(delErr, "rolling back the transient ClusterRoleBinding after a failed Job create")
		}
		return fmt.Errorf("create cluster-manifest restore Job %s/%s: %w", r.OperatorNamespace, jobName, err)
	}
	return nil
}

// clusterRestoreJobEnv builds the cluster-manifest mover's environment for a restore. Unlike the
// namespaced resources[] half it sets NO target-namespace (the objects are cluster-scoped) and NO
// storageClassMapping (a PV is restored as captured), so the mover's applyClusterManifests reads
// only the restore dir, mode, selection and the dry-run flag.
func clusterRestoreJobEnv(plan *clusterRestoreResourcesPlan) ([]corev1.EnvVar, error) {
	selection, err := manifests.EncodeSelection(plan.selection)
	if err != nil {
		return nil, err
	}
	env := []corev1.EnvVar{
		{Name: mover.EnvManifestsRestoreDir, Value: mover.ClusterManifestsRestoreDir},
		{Name: mover.EnvManifestsMode, Value: plan.mode},
		{Name: mover.EnvManifestsSelection, Value: selection},
	}
	if plan.dryRun {
		env = append(env, corev1.EnvVar{Name: mover.EnvManifestsDryRun, Value: "true"})
	}
	return env, nil
}

// teardownClusterRestore reclaims one finished cluster-scoped apply's residue, AFTER the terminal
// status write has persisted. The grant goes first: a leftover Job is inert, a leftover
// ClusterRoleBinding is a live create/update/delete on CRDs and cluster RBAC across the whole cluster.
func (r *ClusterRestoreReconciler) teardownClusterRestore(ctx context.Context, jobName string) {
	if err := deleteClusterManifestBinding(ctx, r.Client, jobName); err != nil {
		logf.FromContext(ctx).WithName("cluster-resources").Error(err,
			"deleting the transient ClusterRoleBinding; the reaper will pick it up", "job", jobName)
	}
	deleteJobAndSecret(ctx, r.Client, r.OperatorNamespace, jobName)
}
