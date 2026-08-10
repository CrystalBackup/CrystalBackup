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
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
	"github.com/CrystalBackup/CrystalBackup/internal/metrics"
	"github.com/CrystalBackup/CrystalBackup/internal/mover"
	"github.com/CrystalBackup/CrystalBackup/internal/repo/queue"
	"github.com/CrystalBackup/CrystalBackup/internal/restic"
	"github.com/CrystalBackup/CrystalBackup/internal/schedule"
	"github.com/CrystalBackup/CrystalBackup/internal/status"
)

// External sync (R28, adr/0013) copies snapshots from one repository to another with
// `restic copy`, RE-ENCRYPTING them to the destination's own key. This file is the part both
// planes share; the two controllers above it differ only in how they resolve their endpoints and
// which RBAC they run under.
//
// Three properties are load-bearing, and each shapes the code below.
//
//   - THE DESTINATION IS AN INDEPENDENT REPOSITORY, not a byte clone. It opens with its own
//     password, so a client secondary stays opaque to the platform key. That is why a sync Job
//     carries two passwords, and why the copy is worth its bandwidth over server-side replication.
//   - THE TWO REPOSITORIES MAY LIVE IN DIFFERENT ACCOUNTS. restic holds two keys but only one set
//     of backend credentials, so both endpoints are addressed as rclone remotes, each with its own
//     credentials (see build/apko/sync.yaml for why that needs a separate image).
//   - THE COPY IS IDEMPOTENT IN RESTIC ITSELF. A copied snapshot records its source's ID in
//     `original`, and restic skips one that is already there. The controller therefore never has
//     to work out "what is missing" — it re-runs the same copy and lets restic move only the
//     delta. `original` is also the only sound key for Mirror's other half.

const (
	// ExternalSync phases (both CRDs pin this enum).
	syncPhasePending         = "Pending"
	syncPhaseRunning         = "Running"
	syncPhaseCompleted       = "Completed"
	syncPhasePartiallyFailed = "PartiallyFailed"
	syncPhaseFailed          = "Failed"

	// conditionSyncComplete is the terminal condition a completed sync sets.
	conditionSyncComplete = "SyncComplete"

	// syncRequeueInterval is how often a Running sync re-checks its in-flight copy.
	syncRequeueInterval = 15 * time.Second
	// syncMinRequeue / syncMaxRequeue bound the wait until the next cron activation, so a distant
	// schedule still re-reconciles occasionally (picking up spec edits and location changes) and a
	// near one is not polled tighter than it can fire.
	syncMinRequeue = 30 * time.Second
	syncMaxRequeue = 10 * time.Minute

	// syncJobBackoffLimit: a copy is long and its failures are usually persistent (credentials,
	// an unreachable endpoint). One pod-level retry absorbs a transient blip; beyond that the run
	// is reported failed and the next cron tick retries from a clean state — which costs nothing,
	// because restic resumes exactly where the destination already is.
	syncJobBackoffLimit int32 = 1
	// syncJobTTLSeconds self-cleans a finished copy Job if the explicit cleanup is missed.
	syncJobTTLSeconds int32 = 600
	// syncJobDeadline bounds one copy. Generous: a first sync moves the whole selected dataset.
	syncJobDeadline = 12 * time.Hour
)

// syncEndpoint is one side of a copy: which repository, reached how, opened with which password.
type syncEndpoint struct {
	// Remote is the rclone remote name this side is addressed through
	// (mover.SyncRemoteSource or mover.SyncRemoteDest).
	Remote string
	// Binding is the resolved location, plane-independent.
	Binding *locationBinding
	// Repo is the BackupRepository backing that location.
	Repo *cbv1.BackupRepository
	// Password is the restic repository password: the unwrapped platform DEK on the cluster
	// plane, the user's own key on the namespace plane. Held only for the duration of one
	// reconcile, and written only into the per-Job Secret.
	Password string
}

// RepoURL is the rclone-addressed repository URL for this endpoint.
//
// Deliberately NOT repo.Status.RepositoryURL: that is the `s3:` spelling every other operation
// uses, and reusing it here would put both repositories on the s3 backend — the exact
// one-credential-set limitation adr/0013 moved to rclone to escape. Same bytes, different
// addressing, and the difference is the whole point.
func (e *syncEndpoint) RepoURL() string {
	return mover.RcloneRepoURL(e.Remote, e.Binding.S3.Bucket, e.Binding.S3.Prefix, e.Binding.ClusterID)
}

// rcloneConfigEnv renders this endpoint's NON-SECRET rclone remote configuration. The two
// credential keys are absent on purpose: they travel by secretKeyRef from the per-Job Secret
// (mover.SyncCredentialKeys), never as inline values.
func (e *syncEndpoint) rcloneConfigEnv() []corev1.EnvVar {
	s3 := e.Binding.S3
	env := []corev1.EnvVar{
		{Name: mover.RcloneRemoteEnv(e.Remote, mover.RcloneKeyType), Value: mover.RcloneTypeS3},
		// "Other" rather than a guess at the vendor: it disables the AWS-specific behaviours a
		// wrong guess would switch on, and it is the honest answer for the MinIO/Ceph/Garage/
		// Scaleway endpoints this project targets.
		{Name: mover.RcloneRemoteEnv(e.Remote, mover.RcloneKeyProvider), Value: mover.RcloneProviderOther},
		{Name: mover.RcloneRemoteEnv(e.Remote, mover.RcloneKeyEndpoint), Value: s3.Endpoint},
		// Path-style addressing, matching what the s3: spelling of the same repository uses. A
		// virtual-hosted URL would need per-bucket DNS the on-prem endpoints do not have.
		{Name: mover.RcloneRemoteEnv(e.Remote, mover.RcloneKeyForcePathStyle), Value: mover.EnvTrue},
		// The bucket exists; do not probe it and above all do not create it. See the constant —
		// this is what lets a destination be reached with credentials scoped to that bucket alone,
		// which is the whole shape of a least-privilege secondary.
		{Name: mover.RcloneRemoteEnv(e.Remote, mover.RcloneKeyNoCheckBucket), Value: mover.EnvTrue},
	}
	if s3.Region != "" {
		env = append(env, corev1.EnvVar{
			Name: mover.RcloneRemoteEnv(e.Remote, mover.RcloneKeyRegion), Value: s3.Region,
		})
	}
	return env
}

// syncRun is one resolved sync: both endpoints, the scope, and the effective mode.
type syncRun struct {
	Source *syncEndpoint
	Dest   *syncEndpoint
	// Namespaces narrows the copy by namespace tag; empty means the whole repository (still
	// scoped to CrystalBackup's own snapshots — see restic.SyncArgs).
	Namespaces []string
	// Mode is the EFFECTIVE mode, after an Immutable destination has forced AppendOnly.
	Mode cbv1.ExternalSyncMode
}

// effectiveSyncMode resolves the requested mode against the destination.
//
// An Immutable destination FORCES AppendOnly, and this is a correction rather than a rejection:
// Object Lock physically forbids the delete half, so a Mirror there would report a reconciliation
// it cannot perform. Forcing it keeps the object honest about what will happen, and the caller
// surfaces the substitution as an Event so it is not silent.
func effectiveSyncMode(requested cbv1.ExternalSyncMode, dest *locationBinding) (cbv1.ExternalSyncMode, bool) {
	if dest.Mode == cbv1.LocationModeImmutable && requested != cbv1.ExternalSyncModeAppendOnly {
		return cbv1.ExternalSyncModeAppendOnly, true
	}
	if requested == "" {
		return cbv1.ExternalSyncModeMirror, false
	}
	return requested, false
}

// syncResourceName names the copy Job and its per-Job Secret. The kind prefix keeps a cluster-plane
// sync from colliding with a namespace-plane one of the same name — both land in the operator
// namespace, which has no other separator between them.
func syncResourceName(prefix, name, step string) string {
	return prefix + "-" + name + "-" + step
}

// syncQueueKey is the queue lane a copy runs on, and it is deliberately NOT the destination
// repository's name.
//
// The per-repo queue is a single EXCLUSIVE lane (adr/0015): init, forget, prune, check and erase
// take turns because each rewrites the snapshot set or the index. A copy does not — it writes new
// packs and a new snapshot, exactly like a backup, under a non-exclusive restic lock. Putting a
// multi-hour copy on the exclusive lane would stall the destination's prune and check for its whole
// duration, which is the opposite of what a secondary needs.
//
// What the lane IS for here is stopping two copies into the same destination from overlapping,
// which would have them racing to write the same snapshots. One key per destination repository,
// namespaced away from the maintenance lane.
func syncQueueKey(destRepo string) string { return "sync/" + destRepo }

// syncJobLabels stamps the copy Job and its pod template. Same shape as the maintenance labels so
// the orphan reaper reclaims a leaked copy Job like any other stray mover resource.
func syncJobLabels() map[string]string {
	return map[string]string{
		labelAppName:      moverAppName,
		labelAppManagedBy: moverManagedBy,
		labelAppComponent: "sync",
	}
}

// ensureSyncCredsSecret writes the per-Job Secret a copy needs: TWO restic passwords and FOUR
// rclone credential values, read from the two locations' own credential Secrets.
//
// Where each credential Secret lives is the tenancy-sensitive part, and it is not a guess: it is
// binding.CredsNamespace — the operator namespace for a cluster location, the location's OWN
// namespace for a namespaced one. Reading the wrong one would not fail; on the namespace plane it
// would silently find an admin Secret of the same name and copy a tenant's data using platform
// credentials.
func ensureSyncCredsSecret(ctx context.Context, deps repoMaintenanceDeps, name string, run *syncRun,
	labels map[string]string,
) error {
	data := map[string][]byte{
		// The unqualified key is the DESTINATION's, mirroring restic's own asymmetry
		// (RESTIC_PASSWORD_FILE vs RESTIC_FROM_PASSWORD_FILE).
		mover.SecretKeyResticPassword:     []byte(run.Dest.Password),
		mover.SecretKeyResticFromPassword: []byte(run.Source.Password),
	}
	for _, e := range []*syncEndpoint{run.Source, run.Dest} {
		accessKey, err := deps.Secrets.GetValue(ctx, e.Binding.CredsNamespace,
			e.Binding.S3.CredentialsSecretRef.Name, mover.SecretKeyAWSAccessKeyID)
		if err != nil {
			return fmt.Errorf("read S3 access key for %s from secret %s/%s: %w",
				e.Binding.Describe(), e.Binding.CredsNamespace, e.Binding.S3.CredentialsSecretRef.Name, err)
		}
		secretKey, err := deps.Secrets.GetValue(ctx, e.Binding.CredsNamespace,
			e.Binding.S3.CredentialsSecretRef.Name, mover.SecretKeyAWSSecretAccessKey)
		if err != nil {
			return fmt.Errorf("read S3 secret key for %s from secret %s/%s: %w",
				e.Binding.Describe(), e.Binding.CredsNamespace, e.Binding.S3.CredentialsSecretRef.Name, err)
		}
		// The AWS_* keys of the source Secret become the SRC remote's keys, and likewise for the
		// destination — one account per remote, which is the entire reason for the rclone detour.
		data[mover.RcloneRemoteEnv(e.Remote, mover.RcloneKeyAccessKeyID)] = accessKey
		data[mover.RcloneRemoteEnv(e.Remote, mover.RcloneKeySecretAccessKey)] = secretKey
	}

	creds := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: deps.OperatorNamespace, Labels: labels},
		Type:       corev1.SecretTypeOpaque,
		Data:       data,
	}
	if err := deps.Create(ctx, creds); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create sync creds secret %s/%s: %w", deps.OperatorNamespace, name, err)
	}
	return nil
}

// buildSyncJobRequest assembles the copy Job. Every field that decides the DIRECTION is set here,
// once: RepoURL is the destination, EnvFromRepository is the source. restic.SyncArgs cannot name a
// repository, so this function is the only place the direction exists.
func buildSyncJobRequest(deps repoMaintenanceDeps, syncImage, name string, run *syncRun) mover.JobRequest {
	sourceEnv, destEnv := run.Source.rcloneConfigEnv(), run.Dest.rcloneConfigEnv()
	env := make([]corev1.EnvVar, 0, 2+len(sourceEnv)+len(destEnv))
	env = append(env,
		corev1.EnvVar{Name: mover.EnvFromRepository, Value: run.Source.RepoURL()},
		// Pin the config file to nothing, so the remotes below are the ONLY definition of "src"
		// and "dst" — not merely the usual one.
		corev1.EnvVar{Name: mover.EnvRcloneConfig, Value: mover.RcloneConfigNone},
	)
	env = append(env, sourceEnv...)
	env = append(env, destEnv...)

	return mover.JobRequest{
		Name:      name,
		Namespace: deps.OperatorNamespace,
		Image:     syncImage,
		Operation: mover.OpSync,
		Profiles:  deps.MoverProfiles,
		// Set, unlike S3Connections a few lines down, and the two absences must not be confused:
		// that one is excluded because rclone speaks neither of this Job's backends, whereas the
		// placement is about which NODE the pod runs on, which is the same question for a copy as
		// for any other mover. An admin who reserved a node pool for backup egress reserved it for
		// this Job most of all — it is the one that talks to two object stores at once.
		Placement:  deps.MoverPlacement,
		ResticArgs: restic.SyncArgs(run.Namespaces),
		// The DESTINATION. See RepoURL's comment for why this is not repo.Status.RepositoryURL.
		RepoURL: run.Dest.RepoURL(),
		// S3Connections is DELIBERATELY absent, and this is the one mover Job where that is true.
		//
		// Both of this Job's repositories are addressed as rclone remotes (RepoURL() renders
		// `rclone:` on either side, which is why CredentialKeys is overridden to the
		// RCLONE_CONFIG_* pair just below), so `-o s3.connections` would name a backend that
		// nothing in this pod is speaking. restic would not complain — it silently ignores an
		// option whose namespace it never applies — which is exactly why the absence is written
		// down here rather than left to be noticed: a future reader comparing this Job to the
		// other nine must be able to tell "considered and excluded" from "forgotten", and the
		// argv itself gives no such signal. BuildJob's own scheme guard enforces this
		// independently, so setting the field here would be inert rather than wrong; leaving it
		// out keeps the request an honest description of the Job.
		//
		// Concurrency toward the two object stores IS tunable for this Job — but through rclone's
		// own knobs, on the rclone remotes, which is a different mechanism and not this field.
		SecretName:       name,
		PVC:              nil, // a copy reads and writes repositories, never a volume
		FromPasswordFile: true,
		CredentialKeys:   mover.SyncCredentialKeys(),
		ExtraEnv:         env,
		BackoffLimit:     syncJobBackoffLimit,
		TTLSeconds:       syncJobTTLSeconds,
		Labels:           syncJobLabels(),
	}
}

// runSyncCopy executes one copy to completion. It is called from inside a queue op, so blocking is
// correct here — the reconcile goroutine polls the handle instead.
func runSyncCopy(opCtx context.Context, deps repoMaintenanceDeps, syncImage, name string, run *syncRun) error {
	ctx, cancel := context.WithTimeout(opCtx, syncJobDeadline)
	defer cancel()
	// LIFO defer: this runs BEFORE cancel(), so the delete still has a live context on the normal
	// path; the detached context inside covers deadline and shutdown.
	defer deleteMaintenanceResources(opCtx, deps, name)

	if err := ensureSyncCredsSecret(ctx, deps, name, run, syncJobLabels()); err != nil {
		return err
	}
	job := mover.BuildJob(buildSyncJobRequest(deps, syncImage, name, run))
	if err := createMaintenanceJob(ctx, deps, mover.OpSync, job); err != nil {
		return err
	}
	return waitForMaintenanceJob(ctx, deps.Client, deps.OperatorNamespace, name)
}

// syncInventory is what one side of the copy currently holds, reduced to what reconciliation
// needs: the set of snapshot IDs, and (at the destination) the source IDs they were copied from.
type syncInventory struct {
	// IDs are the full snapshot IDs present in this repository, within the sync's scope.
	IDs []string
	// Origins maps a destination snapshot's own ID to the SOURCE ID it was copied from (restic's
	// `original`). Empty for a natively-taken snapshot, which at a destination means "this was
	// backed up here directly, not copied" — and Mirror must never touch those.
	Origins map[string]string
}

// inventorySide lists one repository and reduces it to a syncInventory, narrowed to the sync's
// scope.
//
// The listing itself is filtered only by the base CrystalBackup tag, and the namespace narrowing is
// applied here rather than in restic. One listing per repository regardless of how many namespaces
// the selection names — restic's --tag cannot express an OR without repeating the flag, and running
// one inventory Job per namespace would multiply pods for an answer a single pass already holds.
func inventorySide(ctx context.Context, lister FilteredSnapshotLister, repo *cbv1.BackupRepository,
	jobName string, namespaces []string,
) (syncInventory, error) {
	snaps, err := lister.ListFiltered(ctx, repo, jobName)
	if err != nil {
		return syncInventory{}, err
	}
	inv := syncInventory{Origins: map[string]string{}}
	for _, s := range snaps {
		if !snapshotInSyncScope(s, namespaces) {
			continue
		}
		inv.IDs = append(inv.IDs, s.ID)
		if s.Original != "" {
			inv.Origins[s.ID] = s.Original
		}
	}
	return inv, nil
}

// snapshotInSyncScope reports whether a snapshot falls inside the sync's namespace selection. An
// empty selection is "the whole repository", so everything CrystalBackup wrote is in scope.
//
// A snapshot with NO namespace tag — the cluster-manifests capture is the one that exists
// (adr/0011) — is in scope only when there is no selection. A narrowed sync asked for specific
// namespaces, and cluster-scoped resources belong to none of them.
func snapshotInSyncScope(s restic.Snapshot, namespaces []string) bool {
	if len(namespaces) == 0 {
		return true
	}
	ns, ok := restic.TagValue(s.Tags, restic.TagKeyNamespace)
	return ok && slices.Contains(namespaces, ns)
}

// syncProgress is what a completed run reports: how much of the source is now at the destination,
// and how much is not.
type syncProgress struct {
	// Copied is the number of source snapshots present at the destination as copies.
	Copied int32
	// Lag is the number of in-scope source snapshots NOT yet at the destination. Zero is the
	// property a secondary is for; a non-zero value that never falls is what the M5-D4 alert
	// watches.
	Lag int32
	// Extra are destination snapshots that came from this source but whose source snapshot is
	// gone — what Mirror forgets and AppendOnly keeps.
	Extra []string
}

// compareSync computes progress from the two inventories.
//
// Everything keys on `original`, never on tags or timestamps. Two runs of the same schedule carry
// the same tags and differ only by a timestamp that the copy preserves, so matching on either
// would confuse them — and for Mirror, confusing them means forgetting a snapshot that is still
// live at the source.
//
// A destination snapshot with NO `original` was backed up there natively rather than copied. It is
// never counted and never listed as extra: this sync did not put it there, so it is not this
// sync's to remove. That is what makes a repository that receives both native backups and a sync
// safe to use as a destination.
func compareSync(source, dest syncInventory) syncProgress {
	sourceIDs := make(map[string]struct{}, len(source.IDs))
	for _, id := range source.IDs {
		sourceIDs[id] = struct{}{}
	}
	copiedOrigins := make(map[string]struct{}, len(dest.Origins))
	var progress syncProgress
	for destID, origin := range dest.Origins {
		if _, live := sourceIDs[origin]; live {
			copiedOrigins[origin] = struct{}{}
			continue
		}
		progress.Extra = append(progress.Extra, destID)
	}
	slices.Sort(progress.Extra)                 // deterministic, so a message and a forget name the same set
	progress.Copied = int32(len(copiedOrigins)) //nolint:gosec // snapshot counts are not int32-overflow territory
	// max(0, …) because a destination can legitimately hold two copies of one source snapshot (a
	// re-copy after the first was forgotten there), and a negative lag on a status an operator
	// reads to decide whether their DR copy is current would be worse than useless.
	progress.Lag = max(int32(len(sourceIDs))-progress.Copied, 0) //nolint:gosec // same
	return progress
}

// mirrorForgetArgs is the restic argv that removes the destination snapshots a Mirror has
// determined are no longer at the source.
//
// It forgets by explicit SNAPSHOT ID, not by tag, and that is the safety property: a tag filter
// would describe a scope, and a scope computed from a stale inventory can widen. A list of IDs
// cannot mean more than the snapshots that were actually examined. There is no --prune here — the
// space is reclaimed by the destination repository's own maintenance, on its own exclusive lane,
// rather than by tying a multi-hour repack to every sync tick.
func mirrorForgetArgs(snapshotIDs []string) []string {
	return append([]string{"forget"}, snapshotIDs...)
}

// syncEndpointReady reports whether an endpoint can be used, with a reason when it cannot.
func syncEndpointReady(e *syncEndpoint, side string) (string, string, bool) {
	switch {
	case e.Repo == nil || !e.Repo.Status.Initialized:
		return "RepositoryNotReady",
			fmt.Sprintf("the %s repository behind %s is not initialized yet", side, e.Binding.Describe()),
			false
	case e.Binding.ClusterID == "":
		return "WaitingForClusterID",
			fmt.Sprintf("the %s location %s has no effective cluster ID yet", side, e.Binding.Describe()),
			false
	case e.Binding.S3.Bucket == "":
		return "IncompleteLocation",
			fmt.Sprintf("the %s location %s declares no bucket", side, e.Binding.Describe()),
			false
	case e.Password == "":
		return "KeyUnavailable",
			fmt.Sprintf("the %s repository password for %s could not be resolved", side, e.Binding.Describe()),
			false
	}
	return "", "", true
}

// sameEndpoint reports whether both sides resolve to the SAME repository — a copy onto itself.
//
// Admission rule 9 rejects source == destination by name, but names are not the identity that
// matters: two differently-named locations can point at the same bucket, prefix and cluster ID, and
// then the "copy" would be restic reading and writing one repository through two locks.
//
// So this compares BOTH, and the second comparison is the one that earns its keep. A repository
// object's NAME is derived from its location's on either plane (the location name for a cluster
// one, "<namespace>--<location>" for a namespaced one), so name equality only ever reproduces the
// same-name case admission already denies — a useful backstop for a cluster running with the policy
// set disabled, and blind to the aliased case on its own. The repository URL is the actual
// identity: it is the bucket, prefix and cluster ID the location resolved to, which is what restic
// would open. Two locations that differ in every field except those three are one repository, and a
// Mirror between them would copy a repository into itself.
//
// An endpoint whose repository has no URL yet is not compared on it: unresolved is not "the same",
// and reporting SameRepository there would send an operator looking for a configuration error that
// does not exist.
func sameEndpoint(source, dest *syncEndpoint) bool {
	if source.Repo == nil || dest.Repo == nil {
		return false
	}
	if source.Repo.Name == dest.Repo.Name {
		return true
	}
	return source.Repo.Status.RepositoryURL != "" &&
		source.Repo.Status.RepositoryURL == dest.Repo.Status.RepositoryURL
}

// syncScopeDescription renders the copy's scope for status and events.
func syncScopeDescription(namespaces []string) string {
	if len(namespaces) == 0 {
		return "the whole repository"
	}
	return "namespace(s) " + strings.Join(namespaces, ", ")
}

// enqueueSyncCopy places one copy on the destination's sync lane and returns its handle.
func enqueueSyncCopy(q *queue.Manager, deps repoMaintenanceDeps, syncImage, jobName string, run *syncRun) (*queue.Handle, error) {
	return q.Enqueue(syncQueueKey(run.Dest.Repo.Name), queue.OpSync, func(opCtx context.Context) error {
		return runSyncCopy(opCtx, deps, syncImage, jobName, run)
	})
}

// enqueueMirrorForget places the Mirror delete half on the DESTINATION's exclusive lane — unlike
// the copy, this one rewrites the snapshot set, so it takes turns with prune, check and init.
func enqueueMirrorForget(q *queue.Manager, deps repoMaintenanceDeps, jobName string, run *syncRun,
	snapshotIDs []string,
) (*queue.Handle, error) {
	dek, credsSecret := run.Dest.Password, run.Dest.Binding.S3.CredentialsSecretRef.Name
	credsNamespace := run.Dest.Binding.CredsNamespace
	repoURL := run.Dest.Repo.Status.RepositoryURL
	// The DESTINATION's cap, not the source's. Unlike the copy beside it, this half is a real
	// restic op against a real S3 repository (Mirror deletes by snapshot ID on the destination),
	// so it is tuned by the location it actually talks to.
	s3Connections := run.Dest.Binding.S3.Connections
	args := mirrorForgetArgs(snapshotIDs)
	return q.Enqueue(run.Dest.Repo.Name, queue.OpForget, func(opCtx context.Context) error {
		return runRepoMaintenance(opCtx, deps, jobName, mover.OpForget, args,
			repoURL, dek, credsSecret, credsNamespace, s3Connections)
	})
}

// syncStatusView is the subset of either CRD's status this file writes, so the shared driver can
// record a result without knowing which plane it is on.
type syncStatusView struct {
	Phase           *string
	LastSuccessTime **metav1.Time
	SnapshotsCopied *int32
	BytesCopied     *int64
	LagSnapshots    *int32
	Conditions      *[]metav1.Condition
	Generation      int64
}

// applySyncProgress records a completed run into the status view.
//
// BytesCopied is deliberately left untouched, and the reason is worth stating: `restic copy --json`
// emits no summary — verified, not assumed — so there is no byte count to report. Writing a
// fabricated or stale number would be worse than the field staying zero, because a secondary's
// whole value rests on its numbers being believable.
func applySyncProgress(v syncStatusView, progress syncProgress, now metav1.Time, message string) {
	*v.Phase = syncPhaseCompleted
	*v.LastSuccessTime = &now
	*v.SnapshotsCopied = progress.Copied
	*v.LagSnapshots = progress.Lag
	setSyncCondition(v, metav1.ConditionTrue, "Synced", message)
}

// setSyncCondition writes both the shared Ready condition and the sync-specific completion one.
func setSyncCondition(v syncStatusView, s metav1.ConditionStatus, reason, message string) {
	status.SetCondition(v.Conditions, ConditionReady, s, reason, message, v.Generation)
	status.SetCondition(v.Conditions, conditionSyncComplete, s, reason, message, v.Generation)
}

// commitSyncStatus is the single point an external-sync reconcile leaves through, on EITHER plane: it
// persists the status the pass built, then returns the pass's result or its error. Both controllers
// funnel every `return` of Reconcile into it, and that one-pass-one-write discipline is the reason it
// exists at all.
//
// It is shared rather than written once per plane deliberately. The two status types are different Go
// types and nothing but a generic could hold them both, but the DECISION below is identical on both
// planes, and a correction applied to one plane and not the other is precisely how this project once
// grew a namespace plane that its own teardown verification could not see.
//
// # Why an errored pass writes, and why only sometimes
//
// This used to be a per-plane method named `write`, documented as "persists status once per
// reconcile", whose first statement was `if err != nil { return ctrl.Result{}, err }`. The name and
// the doc promised a write the first branch did not perform.
//
// That was NOT a live defect, and the audit that found it verified so rather than assuming: the only
// non-nil errors that reach here are the two apiserver reads inside each plane's endpoint() — a
// location or a BackupRepository answering something other than NotFound — and both sit upstream of
// every mutation of the syncStatusView. drive, finish and settle never return a non-nil error at all:
// every accounting failure there goes through syncDriver.partial, which records the cause on the view
// and returns nil. That is the right shape and it is left exactly as it is.
//
// It was closed anyway because of WHERE this function sits. The view ALIASES the CR's own status
// fields — Phase, LastSuccessTime, SnapshotsCopied, BytesCopied, LagSnapshots, Conditions — and this
// is a single funnel for every exit, so its error branch is downstream-capable of everything either
// plane will ever do. The day somebody adds an error return below applySyncProgress, a completed
// copy's snapshot count and lag vanish on that pass, and nothing in the code, the doc or the tests
// said they must not.
//
// # The two shapes that were rejected
//
//   - PERSIST-THEN-PROPAGATE, UNCONDITIONALLY. Rejected: it spends a status write on a pass whose only
//     news is "the apiserver would not answer" — a write on exactly the path where writes are already
//     failing, to persist something the next pass recomputes for free. That trade is already argued and
//     rejected in restore_controller.go, at the comment beginning "THIS IS THE PASS'S FIRST IN-MEMORY
//     STATUS MUTATION". This function must not quietly overrule it.
//   - KEEP THE SKIP, RENAME IT HONESTLY, AND PIN THE REACHABLE ERROR PATHS WITH A TEST. Rejected as
//     insufficient, and the asymmetry with Restore is the whole point. Restore's skip is safe because
//     of WHERE its error returns are: structurally upstream of anything expensive, discarding only a
//     condition that is a pure function of spec and metadata and costs nothing to recompute. No such
//     structural argument exists here, because a funnel has no "upstream". A test asserting "these are
//     the paths allowed to reach the error branch" guards a CONVENTION, and guards on conventions rot:
//     the next author adding an error return downstream has no reason to go looking for that test.
//
// # Comparing before writing is the synthesis
//
// Persist on an errored pass ONLY IF the pass actually changed the status. A future mutation upstream
// of a future error return WILL be persisted without anybody having to remember a rule, and it costs
// no write on either of today's two paths — on those, nothing has changed — so Restore's argument
// keeps holding rather than being overridden.
//
// equality.Semantic.DeepEqual is the comparison, and it is safe against reading as different on every
// errored pass, which would have silently implemented option 1 wearing a comparison. status.SetCondition
// hands meta.SetStatusCondition a ZERO LastTransitionTime, and that function stamps a fresh one only
// when the condition's Status actually transitions — verified in apimachinery's source, not taken from
// its doc comment. Re-asserting an identical condition therefore compares equal. Semantic rather than
// reflect DeepEqual buys two more things worth having: metav1.Time compares by instant, so a
// LastSuccessTime that round-tripped through the apiserver does not read as changed, and a nil slice
// equals an empty one, so a Conditions field that materialised without gaining a condition is not news.
//
// The comparison governs the ERROR BRANCH ONLY, and that limit is deliberate rather than an oversight
// waiting to be tidied up. A pass that did not error writes unconditionally, even when it recomputed
// the same answer as last time: that is the pass whose result somebody is waiting on, its cost is
// bounded to one write per reconcile, and hoisting the comparison over it would trade a known-cheap
// write for a class of "the object never updated and nobody knows why" that is far harder to diagnose
// than the write is to pay for.
func commitSyncStatus[S any](ctx context.Context, sw client.StatusWriter, obj client.Object,
	before, after *S, res ctrl.Result, err error, describe string,
) (ctrl.Result, error) {
	if err != nil {
		// before != after guards against the ONE way a caller can silently disable everything below:
		// handing over the LIVE status (`before := &cs.Status`) instead of a snapshot of it. That line
		// compiles, type-checks and reads plausibly, and it makes every comparison report "unchanged" —
		// because the syncStatusView mutates through exactly that pointer, and meta.SetStatusCondition
		// edits an existing condition IN PLACE, so even a shallow struct copy would not be enough
		// (Conditions would share its backing array). The plane that got it wrong would quietly keep the
		// defect this function was rewritten to close, on a path no behavioural test can reach today:
		// neither plane has a route that mutates the view and then errors.
		//
		// It fails toward PERSISTING rather than toward skipping, which is the safe direction — a
		// mis-snapshotted plane degrades to the unconditional persist-then-propagate that was rejected
		// as wasteful, not to the silent loss that was rejected as wrong. And unlike a convention, it is
		// observable: a spec can hand this an aliased snapshot and watch the write happen.
		if before != after && equality.Semantic.DeepEqual(before, after) {
			// Nothing this pass computed is at stake; the error is the entire news, and the next pass
			// recomputes the rest. No write.
			return ctrl.Result{}, err
		}
		if uErr := sw.Update(ctx, obj); uErr != nil {
			// Both causes travel. The pass's own error is what an operator needs to act on; the failed
			// write is why the object does not show it. Dropping either would leave a silent gap on the
			// one path where the apiserver is already unhappy.
			return ctrl.Result{}, errors.Join(err,
				fmt.Errorf("persist status for %s on an errored pass: %w", describe, uErr))
		}
		// res is deliberately dropped: controller-runtime backs off a pass that returned an error, and
		// honouring a RequeueAfter computed before the failure would replace that exponential backoff
		// with a fixed poll.
		return ctrl.Result{}, err
	}
	if uErr := sw.Update(ctx, obj); uErr != nil {
		return ctrl.Result{}, fmt.Errorf("update status for %s: %w", describe, uErr)
	}
	return res, nil
}

// syncDriver is the execution half both ExternalSync controllers share: cron activation, the
// in-flight copy, Mirror's delete pass, and the numbers a completed run reports.
//
// It holds no CR type. Each controller resolves its own endpoints — the difference between the
// planes is entirely in WHERE a location and its key come from — then hands over a fully-resolved
// syncRun and a view onto its status. That keeps the one piece of logic that is genuinely
// dangerous (deciding what to delete at a destination) written once.
type syncDriver struct {
	deps      repoMaintenanceDeps
	Queue     *queue.Manager
	Lister    FilteredSnapshotLister
	SyncImage string
	Clock     clock.PassiveClock

	mu sync.Mutex
	// inflight tracks the running copy per CR, AND the run it was started for.
	//
	// The run travels with the handle rather than being re-resolved when the copy finishes, for
	// two reasons. A spec edit mid-copy must not retarget the accounting — the numbers have to
	// describe the copy that actually ran. And the finishing path needs both endpoints to
	// inventory them, which a caller observing "something is in flight" has no reason to have
	// resolved.
	//
	// It is deliberately NOT durable. An operator restart loses it, the next reconcile re-resolves
	// and re-enqueues, and that is safe precisely because restic's copy is idempotent: the new Job
	// adopts the old one by its deterministic name, or starts a copy that moves only what is still
	// missing.
	inflight map[string]*syncInflight
}

// syncInflight is one running copy: the queue handle to poll, and the run it belongs to.
type syncInflight struct {
	handle *queue.Handle
	run    *syncRun
	// startedAt is when the copy was enqueued, and it is the ONLY record of it: neither sync CRD
	// carries a start time, and the queue hands back no timestamp. It lives in memory, so an
	// operator restart mid-copy loses the observation rather than reporting a duration measured
	// from a start it did not witness — which is the right trade for a latency histogram.
	startedAt time.Time
	// copied records that the COPY finished successfully and only the accounting after it is
	// outstanding.
	//
	// Without it, a settle() failure dropped the entry, the next reconcile saw nothing in flight,
	// found the sync due (an on-demand sync is due whenever it has never SUCCEEDED) and enqueued a
	// whole new copy — a Job every requeue interval, forever, for a bookkeeping error. Keeping the
	// entry makes the retry what it should always have been: re-inventory, do not re-copy.
	copied bool
}

// newSyncDriver builds the driver with its in-flight map initialised.
func newSyncDriver(deps repoMaintenanceDeps, q *queue.Manager, lister FilteredSnapshotLister,
	syncImage string, cl clock.PassiveClock,
) *syncDriver {
	return &syncDriver{
		deps: deps, Queue: q, Lister: lister, SyncImage: syncImage, Clock: cl,
		inflight: map[string]*syncInflight{},
	}
}

// syncActivation is the schedule decision for one reconcile.
type syncActivation struct {
	// Due is true when a run should start now.
	Due bool
	// NextFire is when the schedule next activates; zero for an on-demand sync.
	NextFire time.Time
	// Wait is how long until NextFire, measured from the same "now" the decision used. Carried
	// rather than recomputed by the caller, because recomputing it needs the clock a second time
	// and the two reads would not be the same instant.
	Wait time.Duration
	// Err is a spec-static cron/timezone fault.
	Err error
}

// syncSchedule decides whether this reconcile should start a copy.
//
// The baseline is status.lastSuccessTime, not a separate run object: an ExternalSync IS its own run
// record — there are no children to list, so the last success is the only tick boundary there is.
// A sync that has never succeeded is due immediately, which is what an operator creating one
// expects: the secondary starts filling now, not at the next cron boundary.
//
// An EMPTY schedule means on-demand: run once, then never again on its own. That is a deliberate
// reading of "empty ⇒ on-demand only" — the object records one copy and stops, and re-running it is
// an explicit act (delete and recreate, or set a schedule).
func syncSchedule(expr, timezone string, lastSuccess *metav1.Time, now time.Time) syncActivation {
	if expr == "" {
		return syncActivation{Due: lastSuccess == nil}
	}
	cronSched, err := schedule.Parse(expr, timezone)
	if err != nil {
		return syncActivation{Err: err}
	}
	next := cronSched.Next(now)
	if lastSuccess == nil {
		return syncActivation{Due: true, NextFire: next, Wait: next.Sub(now)}
	}
	_, due := cronSched.DueTick(lastSuccess.Time, now, nil)
	return syncActivation{Due: due, NextFire: next, Wait: next.Sub(now)}
}

// hasInflight reports whether a copy is already running for this CR. Checked BEFORE pause and
// schedule, so a paused sync still finishes the copy it started rather than abandoning a
// half-populated destination.
func (d *syncDriver) hasInflight(key string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.inflight[key] != nil
}

// drive runs the in-flight state machine for one sync: start a copy if none is running, poll it,
// and on success measure the result and run Mirror's delete pass.
//
// key identifies this CR across reconciles (kind + namespace + name); it is the in-flight map's
// key, so two CRs syncing the same pair never share a handle.
func (d *syncDriver) drive(ctx context.Context, key string, run *syncRun, view syncStatusView,
	jobPrefix, name string, rec syncRecorder, ident syncIdentity,
) (ctrl.Result, error) {
	d.mu.Lock()
	f := d.inflight[key]
	d.mu.Unlock()

	if f == nil {
		if run == nil {
			// Nothing in flight and nothing to run: the caller believed a copy was live and it is
			// not. Requeue and let the next pass resolve from scratch rather than dereference a
			// run that was never built.
			return ctrl.Result{RequeueAfter: syncRequeueInterval}, nil
		}
		jobName := syncResourceName(jobPrefix, name, "copy")
		handle, err := enqueueSyncCopy(d.Queue, d.deps, d.SyncImage, jobName, run)
		if err != nil {
			// The queue is shutting down, or refused. Neither is the sync's fault; retry.
			return ctrl.Result{RequeueAfter: syncRequeueInterval}, nil
		}
		d.mu.Lock()
		d.inflight[key] = &syncInflight{handle: handle, run: run, startedAt: d.Clock.Now()}
		d.mu.Unlock()
		*view.Phase = syncPhaseRunning
		setSyncCondition(view, metav1.ConditionFalse, "Copying",
			fmt.Sprintf("copying %s from %s to %s", syncScopeDescription(run.Namespaces),
				run.Source.Binding.Describe(), run.Dest.Binding.Describe()))
		rec.event(corev1.EventTypeNormal, "SyncStarted",
			"copying %s from %s to %s", syncScopeDescription(run.Namespaces),
			run.Source.Binding.Describe(), run.Dest.Binding.Describe())
		return ctrl.Result{RequeueAfter: syncRequeueInterval}, nil
	}

	// The copy is already done and only its accounting is outstanding: retry THAT, and nothing else.
	if f.copied {
		return d.finish(ctx, key, f, view, jobPrefix, name, rec, ident)
	}

	select {
	case <-f.handle.Done():
		if err := f.handle.Err(); err != nil {
			d.mu.Lock()
			delete(d.inflight, key)
			d.mu.Unlock()
			*view.Phase = syncPhaseFailed
			setSyncCondition(view, metav1.ConditionFalse, "SyncFailed", err.Error())
			rec.event(corev1.EventTypeWarning, "SyncFailed", "%s", err.Error())
			// A failed copy is still a run that took time, and how long a sync runs before giving
			// up is exactly what an operator sizing a window needs. Recording only successes would
			// hide the pathology (a copy that grinds for six hours and dies) behind an empty
			// histogram.
			d.recordSyncDuration(ctx, f, ident)
			return ctrl.Result{RequeueAfter: syncRequeueInterval}, nil
		}
		d.mu.Lock()
		f.copied = true
		d.mu.Unlock()
		// f.run, not the caller's run: the accounting must describe the copy that ran.
		return d.finish(ctx, key, f, view, jobPrefix, name, rec, ident)
	default:
		return ctrl.Result{RequeueAfter: syncRequeueInterval}, nil
	}
}

// finish runs the post-copy accounting and releases the in-flight entry ONLY when it completed.
//
// The entry is what stops a retry from re-copying, so it must outlive a partial failure and must
// not outlive a success — a retained entry after Completed would make the NEXT scheduled tick
// think a copy it never started is still running.
func (d *syncDriver) finish(ctx context.Context, key string, f *syncInflight, view syncStatusView,
	jobPrefix, name string, rec syncRecorder, ident syncIdentity,
) (ctrl.Result, error) {
	res, err := d.settle(ctx, f.run, view, jobPrefix, name, rec)
	if err == nil && *view.Phase == syncPhaseCompleted {
		d.mu.Lock()
		delete(d.inflight, key)
		d.mu.Unlock()
		// Recorded exactly where the in-flight entry dies, which is the one point that happens
		// once per run: a PartiallyFailed settle KEEPS the entry and retries the accounting, and
		// observing there would add a sample per retry for a copy that ran once.
		d.recordSyncDuration(ctx, f, ident)
	}
	return res, err
}

// syncIdentity is what the METRIC needs and the driver otherwise has no business knowing: which
// CR this run belongs to and where it sits. The locations and the cluster come off the run itself,
// so the two controllers supply only the part that differs between the planes.
type syncIdentity struct {
	// Name is the sync CR's own name.
	Name string
	// Scope is apiconst.OriginCluster or apiconst.OriginNamespace; Namespace is empty for the
	// former — a cluster sync belongs to the platform, not to a tenant.
	Scope, Namespace string
}

// recordSyncDuration observes one finished sync run against the external-sync histogram.
//
// The `cluster` label is resolved from the SOURCE binding and only for a cluster-plane sync,
// matching how the state-derived collector resolves it: a namespaced sync's locations are
// namespaced, the collector cannot resolve a clusterID for them, and emitting one here would
// split every namespace-plane sync into two series that no dashboard could rejoin.
func (d *syncDriver) recordSyncDuration(ctx context.Context, f *syncInflight, ident syncIdentity) {
	if f == nil || f.run == nil || f.startedAt.IsZero() {
		return
	}
	clusterID := ""
	if ident.Scope == apiconst.OriginCluster {
		clusterID = f.run.Source.Binding.ClusterID
	}
	metrics.RecordExternalSyncDuration(ctx, metrics.ExternalSyncSeries{
		Sync:        ident.Name,
		Source:      f.run.Source.Binding.Name,
		Destination: f.run.Dest.Binding.Name,
		Scope:       ident.Scope,
		Namespace:   ident.Namespace,
		Cluster:     clusterID,
	}, d.Clock.Now().Sub(f.startedAt))
}

// settle measures both sides after a successful copy and, in Mirror mode, forgets the destination
// snapshots whose source is gone.
//
// The measurement runs AFTER the copy rather than being derived from it, because `restic copy`
// reports nothing machine-readable — verified against the pinned restic, not assumed. Two listings
// is the honest cost of a believable number.
//
// Both listings BLOCK this reconcile goroutine (JobSnapshotLister runs a real Job and waits, bounded
// by its own deadline), as does the Mirror forget below. That is a deliberate trade and its cost is
// bounded to this controller: controller-runtime gives each controller its own worker, so a sync
// stalled on an inventory delays other SYNCS and nothing else. It is the same shape the erasure
// controller uses, and the alternative — a second in-flight state machine for the accounting — buys
// concurrency this workload has no use for, at the price of more ways to lose track of a copy.
func (d *syncDriver) settle(ctx context.Context, run *syncRun, view syncStatusView,
	jobPrefix, name string, rec syncRecorder,
) (ctrl.Result, error) {
	source, err := inventorySide(ctx, d.Lister, run.Source.Repo,
		syncResourceName(jobPrefix, name, "src-inv"), run.Namespaces)
	if err != nil {
		// The copy SUCCEEDED; only the accounting did not. Reporting Failed here would claim the
		// data did not move, which is worse than admitting the numbers are unknown — so the phase
		// says partially failed and the next tick recomputes.
		return d.partial(view, rec, fmt.Sprintf("the copy succeeded but the source inventory failed: %v", err))
	}
	dest, err := inventorySide(ctx, d.Lister, run.Dest.Repo,
		syncResourceName(jobPrefix, name, "dst-inv"), run.Namespaces)
	if err != nil {
		return d.partial(view, rec, fmt.Sprintf("the copy succeeded but the destination inventory failed: %v", err))
	}

	progress := compareSync(source, dest)

	if run.Mode == cbv1.ExternalSyncModeMirror && len(progress.Extra) > 0 {
		if err := d.mirrorForget(ctx, run, jobPrefix, name, progress.Extra); err != nil {
			// Mirror's copy half is done and correct; only the prune of extras is outstanding. The
			// destination holds MORE than the source, never less, so the data is safe — say so
			// precisely rather than reporting a failed sync.
			return d.partial(view, rec, fmt.Sprintf(
				"copied %d snapshot(s); %d extra snapshot(s) at the destination could not be forgotten "+
					"(the destination holds more than the source, never less): %v",
				progress.Copied, len(progress.Extra), err))
		}
		rec.event(corev1.EventTypeNormal, "MirrorPruned",
			"forgot %d destination snapshot(s) whose source snapshot no longer exists", len(progress.Extra))
	}

	message := fmt.Sprintf("%d snapshot(s) at the destination, %d still to copy (%s, mode %s)",
		progress.Copied, progress.Lag, syncScopeDescription(run.Namespaces), run.Mode)
	applySyncProgress(view, progress, metav1.Time{Time: d.Clock.Now()}, message)
	rec.event(corev1.EventTypeNormal, "SyncCompleted", "%s", message)
	return ctrl.Result{}, nil
}

// partial records a run that moved data but could not finish its bookkeeping.
func (d *syncDriver) partial(view syncStatusView, rec syncRecorder, message string) (ctrl.Result, error) {
	*view.Phase = syncPhasePartiallyFailed
	setSyncCondition(view, metav1.ConditionFalse, "PartiallyFailed", message)
	rec.event(corev1.EventTypeWarning, "SyncPartiallyFailed", "%s", message)
	return ctrl.Result{RequeueAfter: syncRequeueInterval}, nil
}

// mirrorForget runs the delete half synchronously on the destination's EXCLUSIVE lane. Unlike the
// copy this rewrites the snapshot set, so it takes turns with prune, check and init.
//
// It blocks the reconcile goroutine, which is acceptable where the copy's would not be: a forget by
// explicit ID is seconds, against a copy's hours.
func (d *syncDriver) mirrorForget(ctx context.Context, run *syncRun, jobPrefix, name string,
	extra []string,
) error {
	handle, err := enqueueMirrorForget(d.Queue, d.deps, syncResourceName(jobPrefix, name, "mirror-forget"),
		run, extra)
	if err != nil {
		return err
	}
	select {
	case <-handle.Done():
		return handle.Err()
	case <-ctx.Done():
		// The op keeps running on the queue; only our wait ended. Reported as outstanding so the
		// next reconcile re-measures rather than assuming either outcome.
		return ctx.Err()
	}
}

// syncRecorder is the event sink, narrowed to what this file emits so the driver does not need the
// concrete CR type to record against.
type syncRecorder struct {
	emit func(eventType, reason, messageFmt string, args ...any)
}

func (r syncRecorder) event(eventType, reason, messageFmt string, args ...any) {
	if r.emit != nil {
		r.emit(eventType, reason, messageFmt, args...)
	}
}
