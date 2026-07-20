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

// The resources[] half of a restore (spec/04-manifest-backup.md §5): a manifest-mover Job
// restores the kind=manifests snapshot and APPLIES the tree to the target namespace, under a
// transient RoleBinding to crystal-manifest-writer.
//
// That grant — create/update/delete on arbitrary kinds — is the largest in the system
// (spec/03-security-and-tenancy.md §5). It exists for one Job's lifetime, in one namespace,
// and everything here is arranged so that stays true even when a reconcile is interrupted.

// resourcesPlan is the resolved resources[] half of one restore, or nil when the restore asks
// for no manifests at all.
type resourcesPlan struct {
	// snapshotID is the server-resolved kind=manifests snapshot.
	snapshotID string
	// snapshotPath is its recorded subtree (/manifests/<origin-ns>).
	snapshotPath string
	// selection is what the mover will narrow the tree to.
	selection manifests.Selection
	// mode and dryRun come straight from the CR.
	mode   string
	dryRun bool
	// storageClassMapping is empty for a namespaced Restore, which exposes no such field —
	// same cluster, same classes (§5.3). ClusterRestore fills it.
	storageClassMapping map[string]string
}

// resolveResourcesPlan decides whether this restore has a manifest half and, if so, pins the
// snapshot it reads.
//
// The tri-state is resolved HERE, once, rather than in the mover: `resources` omitted restores
// every manifest, a present-but-empty list restores none (spec/02-api.md § Restore selection
// model). The two are indistinguishable after JSON omitempty, so the decision has to be made
// where the CR is still in hand.
func resolveResourcesPlan(
	resources []cbv1.ResourceSelectorItem,
	mode string,
	dryRun bool,
	snaps []restic.Snapshot,
	// classMapping is nil for every caller today: only ClusterRestore exposes
	// storageClassMapping (04-manifest-backup.md §5.3), and its half lands with L9. The
	// parameter is here so both callers share ONE resolution, which is what the spec requires
	// of the mapping — not two implementations that can drift.
	classMapping map[string]string, //nolint:unparam // ClusterRestore fills this at L9
) (*resourcesPlan, bool) {
	// A present-but-empty list is a deliberate "no manifests" (the CLI's --data-only writes
	// exactly this), and must NOT be confused with the omitted field above it.
	if resources != nil && len(resources) == 0 {
		return nil, true
	}

	snap, ok := manifestsSnapshot(snaps)
	if !ok {
		// No manifest snapshot under the mediated filter. That is the normal shape of a run
		// captured with includeManifests: false, not an error — restoring its volumes is still
		// exactly what the user asked for.
		return nil, false
	}

	plan := &resourcesPlan{
		snapshotID:          snap.ID,
		snapshotPath:        manifestsSnapshotPath(snap),
		mode:                mode,
		dryRun:              dryRun,
		storageClassMapping: classMapping,
	}
	if resources == nil {
		plan.selection = manifests.Selection{All: true}
	} else {
		plan.selection = manifests.Selection{Items: toSelectionItems(resources)}
	}
	return plan, true
}

// toSelectionItems converts the CRD's selector items into the mover's wire form. The two types
// are deliberately separate: the mover must not import the API package, and the wire form is
// free to change without a CRD revision.
func toSelectionItems(items []cbv1.ResourceSelectorItem) []manifests.SelectionItem {
	out := make([]manifests.SelectionItem, 0, len(items))
	for i := range items {
		item := manifests.SelectionItem{
			Include: items[i].Include,
			Exclude: items[i].Exclude,
		}
		// An empty LabelSelector matches everything, which is also what a nil one means here —
		// but only a nil one skips the (cheap) match, so normalise rather than always sending
		// an empty selector the mover would have to special-case.
		if len(items[i].Selector.MatchLabels) > 0 || len(items[i].Selector.MatchExpressions) > 0 {
			sel := items[i].Selector
			item.Selector = &sel
		}
		out = append(out, item)
	}
	return out
}

// manifestsSnapshot picks the kind=manifests snapshot out of a mediated listing, newest wins.
// The listing is already filtered server-side by namespace= and run= (I1), so this only has to
// separate the manifest snapshot from the per-PVC data ones.
func manifestsSnapshot(snaps []restic.Snapshot) (restic.Snapshot, bool) {
	var newest restic.Snapshot
	var found bool
	for _, s := range snaps {
		if kind, _ := restic.TagValue(s.Tags, restic.TagKeyKind); kind != restic.KindManifests {
			continue
		}
		if !found || s.Time.After(newest.Time) {
			newest, found = s, true
		}
	}
	return newest, found
}

// manifestsSnapshotPath is the subtree to restore. restic records the absolute path it backed
// up, and the snapshot carries it — so read it rather than rebuilding "/manifests/<ns>" from
// the CR, which would be a guess about the ORIGIN namespace and wrong the moment a
// ClusterRestore reads another cluster's snapshot into a differently-named namespace.
func manifestsSnapshotPath(snap restic.Snapshot) string {
	if len(snap.Paths) > 0 {
		return snap.Paths[0]
	}
	return mover.ManifestsRoot
}

// doneWord renders the manifest half's state for the in-progress condition message, so a user
// watching a slow restore can tell which half they are waiting on.
func doneWord(done bool) string {
	if done {
		return "settled"
	}
	return "applying"
}

// resourcesJobPrefix is the deterministic name stem of a restore's manifest Job, so a reconcile
// after a restart finds the Job it already created instead of applying the namespace twice.
func resourcesJobPrefix(ownerID string) string {
	return sanitizeDNSName(ownerID+"-resources", moverNamePrefixMax)
}

// advanceResources drives the manifest half one step, returning true once it has settled and —
// on the pass that records that result — the name stem whose residue is due for teardown.
//
// Like the backup half it does NOT tear down itself: the Job and its grant are the only durable
// record that the apply happened, so they must outlive an unpersisted status write. Deleting
// them before the write lands would let the next reconcile find no Job and RE-APPLY the whole
// namespace — in Overwrite or Recreate mode, against objects it just wrote.
func (r *RestoreReconciler) advanceResources(
	ctx context.Context,
	restore *cbv1.Restore,
	rc *restoreExecContext,
	plan *resourcesPlan,
) (done bool, teardownPrefix string, err error) {
	if plan == nil {
		return true, "", nil
	}
	// Already terminal: a report recorded. Re-entering would apply the namespace a second time.
	if restore.Status.Resources != nil {
		return true, "", nil
	}

	prefix := resourcesJobPrefix(rc.ownerID)
	jobName := manifestsJobName(prefix)

	var job batchv1.Job
	err = r.Get(ctx, client.ObjectKey{Namespace: r.OperatorNamespace, Name: jobName}, &job)
	switch {
	case apierrors.IsNotFound(err):
		return false, "", r.startResourcesJob(ctx, rc, plan, jobName)
	case err != nil:
		return false, "", fmt.Errorf("get manifest restore Job %s/%s: %w", r.OperatorNamespace, jobName, err)
	}

	if job.Status.Succeeded == 0 && job.Status.Failed == 0 {
		return false, "", nil // still applying
	}

	// Terminal. Read the verdict BEFORE anything is torn down: it lives in the pod's
	// termination message, which disappears with the pod.
	result, _, readErr := readMoverResult(ctx, r.Client, r.OperatorNamespace, jobName)
	switch {
	case readErr != nil:
		// A blank message means the mover was killed mid-apply (OOM, SIGKILL). The namespace
		// is in an UNKNOWN state — some objects applied, some not — and saying so is the only
		// honest answer. Reporting zero applied would be a specific claim we cannot make.
		restore.Status.Resources = &cbv1.RestoreResourcesStatus{
			FailedCount: 1,
			Entries: []cbv1.RestoreResourceEntry{{
				Outcome: cbv1.RestoreResourceFailed,
				Reason: fmt.Sprintf("the manifest restore did not report a result (%v); "+
					"some resources may have been applied — see the mover's pod log", readErr),
			}},
		}
	case !result.OK:
		restore.Status.Resources = &cbv1.RestoreResourcesStatus{
			FailedCount: 1,
			Entries: []cbv1.RestoreResourceEntry{{
				Outcome: cbv1.RestoreResourceFailed,
				Reason:  clampMessage(result.Error),
			}},
		}
	default:
		restore.Status.RestoredResources = result.RestoredResources
		restore.Status.Resources = resourcesStatusFrom(result)
		logf.FromContext(ctx).WithName("resources").Info("manifests applied",
			"namespace", rc.targetNamespace, "applied", result.RestoredResources,
			"failed", result.FailedResources, "skipped", result.SkippedResources,
			"dryRun", plan.dryRun)
	}
	return true, prefix, nil
}

// resourcesStatusFrom maps the mover's wire report onto the CR status, re-applying the API's
// caps. The mover already trimmed to the termination message's budget; this is the second,
// independent bound — status lives in etcd, and a status too large to write loses the WHOLE
// report rather than its tail.
func resourcesStatusFrom(result mover.MoverResult) *cbv1.RestoreResourcesStatus {
	out := &cbv1.RestoreResourcesStatus{
		FailedCount: result.FailedResources,
		Truncated:   result.ResourcesTruncated,
	}
	for _, e := range result.ResourceEntries {
		if len(out.Entries) >= cbv1.MaxRestoreResourceEntries {
			out.Truncated = true
			break
		}
		changed := e.Changed
		if len(changed) > cbv1.MaxRestoreChangedPaths {
			changed = changed[:cbv1.MaxRestoreChangedPaths]
			out.Truncated = true
		}
		out.Entries = append(out.Entries, cbv1.RestoreResourceEntry{
			Group:   e.Group,
			Kind:    e.Kind,
			Name:    e.Name,
			Outcome: cbv1.RestoreResourceOutcome(e.Outcome),
			Reason:  clampMessage(e.Reason),
			Changed: changed,
		})
	}
	return out
}

// startResourcesJob creates the creds Secret, then the grant, then the Job. The order is not
// arbitrary: a Job that starts without its binding fails on its first API call, having already
// consumed an attempt against the backoff limit.
func (r *RestoreReconciler) startResourcesJob(
	ctx context.Context,
	rc *restoreExecContext,
	plan *resourcesPlan,
	jobName string,
) error {
	labels := map[string]string{
		apiconst.LabelManagedBy: apiconst.ManagedByValue,
		rc.ownerLabelKey:        rc.ownerName,
		apiconst.LabelNamespace: rc.targetNamespace,
		// What the NetworkPolicy selects on to grant API-server egress to THIS pod and no other
		// mover (spec/03 §7).
		apiconst.LabelMoverRole: apiconst.MoverRoleManifest,
	}

	if err := ensureMoverCredsSecret(ctx, repoMaintenanceDeps{
		Client: r.Client, Secrets: r.Engine.Secrets, OperatorNamespace: r.OperatorNamespace,
	}, jobName, rc.dek, rc.s3CredsSecret, labels); err != nil {
		return err
	}

	if err := ensureManifestRoleBinding(ctx, r.Client, manifestRBACRequest{
		TargetNamespace:    rc.targetNamespace,
		JobName:            jobName,
		ClusterRoleName:    r.ManifestWriterClusterRole,
		ServiceAccountName: r.ManifestMoverServiceAccount,
		OperatorNamespace:  r.OperatorNamespace,
	}); err != nil {
		return err
	}

	env, err := resourcesJobEnv(rc.targetNamespace, plan)
	if err != nil {
		return err
	}

	job := mover.BuildJob(mover.JobRequest{
		Name:      jobName,
		Namespace: r.OperatorNamespace,
		Image:     r.Engine.MoverImage,
		Operation: mover.OpManifestsRestore,
		ResticArgs: restic.ManifestsRestoreArgs(plan.snapshotID, plan.snapshotPath,
			mover.ManifestsRestoreDir),
		RepoURL:    rc.repoURL,
		SecretName: jobName,
		Labels:     labels,
		// The one mover that reaches the API server (I6's sole exception).
		ServiceAccountName: r.ManifestMoverServiceAccount,
		ManifestsVolume:    true,
		ExtraEnv:           env,
		BackoffLimit:       restoreMoverBackoffLimit,
		// NO TTL, for the same reason as the volume Jobs (adr/0016): this Job IS the durable
		// record that the apply ran. If the TTL controller erased it before the status write
		// landed, the next pass would re-derive "never started" and re-apply the namespace.
		TTLSeconds: mover.NoTTL,
	})
	if err := r.Create(ctx, job); err != nil && !apierrors.IsAlreadyExists(err) {
		// Leave no grant behind for a Job that never started.
		if delErr := deleteManifestRoleBinding(ctx, r.Client, rc.targetNamespace, jobName); delErr != nil {
			logf.FromContext(ctx).Error(delErr, "rolling back the transient RoleBinding after a failed Job create")
		}
		return fmt.Errorf("create manifest restore Job %s/%s: %w", r.OperatorNamespace, jobName, err)
	}
	return nil
}

// resourcesJobEnv builds the manifest mover's environment for a restore.
func resourcesJobEnv(targetNamespace string, plan *resourcesPlan) ([]corev1.EnvVar, error) {
	selection, err := manifests.EncodeSelection(plan.selection)
	if err != nil {
		return nil, err
	}
	env := []corev1.EnvVar{
		{Name: mover.EnvManifestsNamespace, Value: targetNamespace},
		{Name: mover.EnvManifestsRestoreDir, Value: mover.ManifestsRestoreDir},
		{Name: mover.EnvManifestsMode, Value: plan.mode},
		{Name: mover.EnvManifestsSelection, Value: selection},
	}
	if plan.dryRun {
		env = append(env, corev1.EnvVar{Name: mover.EnvManifestsDryRun, Value: "true"})
	}
	if len(plan.storageClassMapping) > 0 {
		encoded, err := json.Marshal(plan.storageClassMapping)
		if err != nil {
			return nil, fmt.Errorf("encode storageClassMapping: %w", err)
		}
		env = append(env, corev1.EnvVar{
			Name: mover.EnvManifestsStorageClassMapping, Value: string(encoded),
		})
	}
	return env, nil
}

// teardownResources reclaims one finished apply's residue, AFTER the terminal status write has
// persisted. The grant goes first: a leftover Job is inert, a leftover RoleBinding is a live
// write privilege on arbitrary kinds in a tenant namespace.
func (r *RestoreReconciler) teardownResources(ctx context.Context, rc *restoreExecContext, prefix string) {
	jobName := manifestsJobName(prefix)
	if err := deleteManifestRoleBinding(ctx, r.Client, rc.targetNamespace, jobName); err != nil {
		logf.FromContext(ctx).WithName("resources").Error(err,
			"deleting the transient RoleBinding; the reaper will pick it up",
			"namespace", rc.targetNamespace, "job", jobName)
	}
	deleteJobAndSecret(ctx, r.Client, r.OperatorNamespace, jobName)
}
