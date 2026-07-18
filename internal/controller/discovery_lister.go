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
	"bytes"
	"context"
	"fmt"
	"io"
	"slices"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/client/secrets"
	"github.com/CrystalBackup/CrystalBackup/internal/keys"
	"github.com/CrystalBackup/CrystalBackup/internal/mover"
	"github.com/CrystalBackup/CrystalBackup/internal/restic"
)

const (
	// discoveryJobBackoffLimit is the snapshots Job's spec.backoffLimit. `restic snapshots` is a
	// read-only listing; a couple of pod-level retries absorb a transient S3 blip, after which the
	// inventory is treated as failed and discovery retries the WHOLE List on its own cadence.
	discoveryJobBackoffLimit int32 = 2

	// discoveryJobTTLSeconds is the snapshots Job's ttlSecondsAfterFinished: a finished inventory
	// Job self-cleans after ten minutes even if the explicit post-read cleanup is missed (e.g. the
	// operator restarts between the Job completing and List deleting it).
	discoveryJobTTLSeconds int32 = 600

	// discoveryJobPollInterval is how often List re-reads the snapshots Job while awaiting completion.
	discoveryJobPollInterval = 2 * time.Second

	// discoveryJobDeadline bounds one List: the reconcile goroutine blocks on the inventory Job for
	// at most this long, so a black-holed `restic snapshots` (unreachable S3) cannot wedge the
	// discovery reconciler forever — List returns an error and discovery retries on its own cadence.
	// Generous, because a listing against reachable S3 is seconds; the operator context cancels
	// sooner on shutdown.
	discoveryJobDeadline = 5 * time.Minute
)

// discoveryResourceName is the deterministic name of BOTH the per-repository snapshots (inventory)
// Job and its job-scoped credentials Secret in the operator namespace. Fixed (not per-List) so an
// operator that restarts mid-inventory RE-ADOPTS the still-running Job on the next List (Create
// tolerates AlreadyExists) instead of leaking a second one; the post-read cleanup then removes it.
func discoveryResourceName(repoName string) string {
	return repoName + "-discovery"
}

// discoveryJobLabels stamps the snapshots Job (and its pod template). Like initJobLabels it
// deliberately avoids app.kubernetes.io/name=crystal-backup (the operator pod's own label, which
// the crucible's operator-restart tests select on) and carries the managed-by label the orphan
// reaper sweeps by, so a leaked inventory Job is reclaimed like any other stray mover resource.
func discoveryJobLabels() map[string]string {
	return map[string]string{
		labelAppName:      moverAppName,
		labelAppManagedBy: moverManagedBy,
		labelAppComponent: "discovery",
	}
}

// JobSnapshotLister is the PRODUCTION SnapshotLister: it inventories a repository by running a real
// `restic snapshots --json --tag crystalbackup` mover Job and reading the JSON array back off the
// completed pod's log. It is the discovery controller's window onto ground truth in a live cluster;
// the envtest suite injects a stub lister instead (see SnapshotLister), so none of the projection,
// GC or status logic depends on this type — only the crucible exercises it end to end.
//
// It reuses the maintenance-op-Job shape the BackupRepository controller's runInit established: a
// job-scoped creds Secret (the platform DEK as the restic password, the two S3 keys as env) plus a
// mover Job with no data volume, both owned by the BackupRepository so a repository delete GCs them.
// The ONE thing init does not need and this does is reading the Job's STDOUT: a snapshot inventory
// is far larger than the 4096-byte termination message, so OpSnapshots tees restic's stdout to the
// pod log (see internal/mover) and List slices the JSON array out of that log.
//
// List BLOCKS the discovery reconcile goroutine until the inventory Job terminates (bounded by
// discoveryJobDeadline). That is acceptable for M1: discovery runs on a slow cadence over a handful
// of DR repositories, a `restic snapshots` is seconds, and the deadline caps a stuck S3. A future
// milestone can make it async through the exclusive queue like init if the repository count grows.
type JobSnapshotLister struct {
	client.Client
	// Clientset reads pod LOGS — the one thing the controller-runtime client cannot do (it has no
	// subresource-stream support). Everything else (Get/Create/Delete of Jobs, Secrets, Pods) goes
	// through the cached Client above.
	Clientset kubernetes.Interface
	// Secrets is the uncached GET-by-name reader (invariant I3): it reads the cluster KEK and the DR
	// S3 credentials from OperatorNamespace, never through a cache/informer.
	Secrets *secrets.ByNameReader
	Scheme  *runtime.Scheme
	// OperatorNamespace is where the cluster KEK, the wrapped DEK, the DR S3 credentials, the
	// job-scoped creds Secret and the snapshots Job all live.
	OperatorNamespace string
	// MoverImage is the CrystalBackup image the snapshots Job runs (crystal-mover + restic).
	MoverImage string
}

// NewJobSnapshotLister builds the production lister. main.go wires the cached client, a clientset
// (for pod logs), the uncached Secret reader, the scheme, the operator namespace and the mover image.
func NewJobSnapshotLister(
	c client.Client,
	clientset kubernetes.Interface,
	secretsReader *secrets.ByNameReader,
	scheme *runtime.Scheme,
	operatorNamespace, moverImage string,
) *JobSnapshotLister {
	return &JobSnapshotLister{
		Client:            c,
		Clientset:         clientset,
		Secrets:           secretsReader,
		Scheme:            scheme,
		OperatorNamespace: operatorNamespace,
		MoverImage:        moverImage,
	}
}

// List runs the inventory Job and returns the repository's CrystalBackup snapshots. It resolves the
// owning location and platform DEK, ensures the creds Secret and the snapshots Job (both idempotent,
// re-adopted on restart), waits for the Job to terminate, reads the JSON array off the completed
// pod's log and parses it. On success it best-effort deletes the one-shot Job + Secret so a stale
// inventory can never be re-read; on any failure it returns a wrapped error and discovery retries.
func (l *JobSnapshotLister) List(ctx context.Context, repo *cbv1.BackupRepository) ([]restic.Snapshot, error) {
	return l.list(ctx, repo, discoveryResourceName(repo.Name), restic.SnapshotsArgs())
}

// ListFiltered is List with a server-side tag filter and a caller-chosen Job name: the
// mediated-restore resolution primitive (adr/0016 §3). The filter tags are AND-combined
// with the base crystalbackup marker INSIDE the restic invocation
// (restic.SnapshotsFilterArgs), so the repository itself only ever returns the filtered
// snapshots — for a cluster-origin restore that filter is namespace=<the CR's namespace>,
// derived server-side and unforgeable. The caller supplies a deterministic per-purpose
// jobName so a restore resolution never collides with (or re-adopts) a discovery inventory.
func (l *JobSnapshotLister) ListFiltered(ctx context.Context, repo *cbv1.BackupRepository, jobName string, filterTags ...string) ([]restic.Snapshot, error) {
	return l.list(ctx, repo, jobName, restic.SnapshotsFilterArgs(filterTags...))
}

// list is the shared body of List/ListFiltered: one snapshots Job named jobName running
// resticArgs, its log parsed as the snapshot array.
func (l *JobSnapshotLister) list(ctx context.Context, repo *cbv1.BackupRepository, jobName string, resticArgs []string) ([]restic.Snapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, discoveryJobDeadline)
	defer cancel()

	repoURL := repo.Status.RepositoryURL
	if repoURL == "" {
		return nil, fmt.Errorf("BackupRepository %s has no status.repositoryURL yet", repo.Name)
	}

	cbl, err := l.resolveOwningLocation(ctx, repo)
	if err != nil {
		return nil, fmt.Errorf("resolve owning location for repository %s: %w", repo.Name, err)
	}

	dek, err := l.resolvePlatformDEK(ctx, cbl)
	if err != nil {
		return nil, err
	}

	if err := l.ensureCredsSecret(ctx, repo, jobName, dek, cbl.Spec.S3.CredentialsSecretRef.Name); err != nil {
		return nil, err
	}
	if err := l.ensureSnapshotsJob(ctx, repo, jobName, repoURL, resticArgs); err != nil {
		return nil, err
	}

	if err := l.waitForJob(ctx, jobName); err != nil {
		return nil, err
	}

	log, err := l.readPodLog(ctx, jobName)
	if err != nil {
		return nil, err
	}

	snaps, err := restic.ParseSnapshots(extractJSONArray(log))
	if err != nil {
		return nil, fmt.Errorf("parse snapshots inventory for repository %s: %w", repo.Name, err)
	}

	// The inventory is captured: reclaim the one-shot Job and Secret so the NEXT List builds a fresh
	// Job (never re-reads this run's stale log). Best-effort — the TTL and the orphan reaper backstop.
	l.cleanup(ctx, jobName)
	return snaps, nil
}

// resolveOwningLocation mirrors the BackupRepository controller: a repository's controller owner is
// its ClusterBackupLocation, fetched here for the KEK reference and the S3 credentials Secret name.
func (l *JobSnapshotLister) resolveOwningLocation(ctx context.Context, repo *cbv1.BackupRepository) (*cbv1.ClusterBackupLocation, error) {
	owner := metav1.GetControllerOf(repo)
	if owner == nil || owner.Kind != kindClusterBackupLocation {
		return nil, apierrors.NewNotFound(
			cbv1.GroupVersion.WithResource("clusterbackuplocations").GroupResource(), "<none>")
	}
	var cbl cbv1.ClusterBackupLocation
	if err := l.Get(ctx, client.ObjectKey{Name: owner.Name}, &cbl); err != nil {
		return nil, err
	}
	return &cbl, nil
}

// resolvePlatformDEK reads the cluster KEK and returns the plaintext platform DEK (the restic
// repository password) via keys.DEKManager (mint-once, reuse-forever). It is the read-only twin of
// the BackupRepository controller's ensurePlatformDEK without the status side effects: the lister
// has no CR status of its own, so a failure is just a wrapped error naming the Secret (never key
// material) for discovery's logs and the retry.
func (l *JobSnapshotLister) resolvePlatformDEK(ctx context.Context, cbl *cbv1.ClusterBackupLocation) (string, error) {
	kekName := cbl.Spec.Encryption.ClusterKEKSecretRef.Name
	identity, err := l.Secrets.GetValue(ctx, l.OperatorNamespace, kekName, kekIdentityDataKey)
	if err != nil {
		return "", fmt.Errorf("read cluster KEK secret %s/%s: %w", l.OperatorNamespace, kekName, err)
	}
	wrapper, err := keys.NewAgeWrapper(string(identity))
	if err != nil {
		return "", fmt.Errorf("parse cluster KEK secret %s/%s: %w", l.OperatorNamespace, kekName, err)
	}
	dek, err := keys.NewDEKManager(l.Client, wrapper, l.OperatorNamespace).EnsureDEK(ctx, cbl.Name)
	if err != nil {
		return "", fmt.Errorf("ensure platform DEK for location %s: %w", cbl.Name, err)
	}
	return dek, nil
}

// ensureCredsSecret creates the job-scoped creds Secret the snapshots mover consumes: the DEK as the
// restic password (mounted file) and the two S3 keys as env (secretKeyRef). Owned by the repository
// so a repository delete GCs it; tolerates AlreadyExists so a re-List re-adopts.
func (l *JobSnapshotLister) ensureCredsSecret(ctx context.Context, repo *cbv1.BackupRepository, name, dek, credsSecretName string) error {
	accessKey, err := l.Secrets.GetValue(ctx, l.OperatorNamespace, credsSecretName, mover.SecretKeyAWSAccessKeyID)
	if err != nil {
		return fmt.Errorf("read S3 access key from secret %s/%s: %w", l.OperatorNamespace, credsSecretName, err)
	}
	secretKey, err := l.Secrets.GetValue(ctx, l.OperatorNamespace, credsSecretName, mover.SecretKeyAWSSecretAccessKey)
	if err != nil {
		return fmt.Errorf("read S3 secret key from secret %s/%s: %w", l.OperatorNamespace, credsSecretName, err)
	}
	creds := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: l.OperatorNamespace, Labels: discoveryJobLabels()},
		Type:       corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			mover.SecretKeyResticPassword:     []byte(dek),
			mover.SecretKeyAWSAccessKeyID:     accessKey,
			mover.SecretKeyAWSSecretAccessKey: secretKey,
		},
	}
	if err := controllerutil.SetControllerReference(repo, creds, l.Scheme); err != nil {
		return fmt.Errorf("set controller reference on discovery creds secret %s: %w", name, err)
	}
	if err := l.Create(ctx, creds); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create discovery creds secret %s/%s: %w", l.OperatorNamespace, name, err)
	}
	return nil
}

// ensureSnapshotsJob creates the maintenance mover Job that runs the given `restic
// snapshots` argv (no data volume) — the base listing for discovery, a tag-filtered one for
// a mediated restore resolution. Owned by the repository (GC on repository delete);
// tolerates AlreadyExists so a restart mid-inventory re-adopts the running Job rather than
// leaking a second.
func (l *JobSnapshotLister) ensureSnapshotsJob(ctx context.Context, repo *cbv1.BackupRepository, name, repoURL string, resticArgs []string) error {
	job := mover.BuildJob(mover.JobRequest{
		Name:         name,
		Namespace:    l.OperatorNamespace,
		Image:        l.MoverImage,
		Operation:    mover.OpSnapshots,
		ResticArgs:   resticArgs,
		RepoURL:      repoURL,
		SecretName:   name,
		PVC:          nil,
		BackoffLimit: discoveryJobBackoffLimit,
		TTLSeconds:   discoveryJobTTLSeconds,
		Labels:       discoveryJobLabels(),
	})
	if err := controllerutil.SetControllerReference(repo, job, l.Scheme); err != nil {
		return fmt.Errorf("set controller reference on discovery job %s: %w", name, err)
	}
	if err := l.Create(ctx, job); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create discovery job %s/%s: %w", l.OperatorNamespace, name, err)
		}
		// Re-adoption guard: the Job NAME is deterministic per purpose, but its restic argv
		// was baked at creation — a leftover Job can carry a DIFFERENT filter than this
		// call's (a restarted controller re-pinning "latest" onto a newer run), and adopting
		// it would parse the OLD filter's output as the new filter's result: a restore
		// executing one run while status and provenance attest another. Identical argv ⇒
		// adopt (the normal restart path); anything else is superseded and retried.
		var existing batchv1.Job
		if gerr := l.Get(ctx, client.ObjectKey{Namespace: l.OperatorNamespace, Name: name}, &existing); gerr != nil {
			return fmt.Errorf("inspect existing discovery job %s/%s: %w", l.OperatorNamespace, name, gerr)
		}
		if len(existing.Spec.Template.Spec.Containers) == 0 ||
			!slices.Equal(existing.Spec.Template.Spec.Containers[0].Args, job.Spec.Template.Spec.Containers[0].Args) {
			l.cleanup(ctx, name)
			return fmt.Errorf("discovery job %s/%s carried stale arguments and was superseded; retrying",
				l.OperatorNamespace, name)
		}
	}
	return nil
}

// waitForJob polls the snapshots Job until terminal success (Succeeded >= 1 / the Complete
// condition) or terminal failure (the Failed condition, or Failed pods past the backoffLimit),
// honouring ctx (Manager.Stop or the List deadline). It mirrors the BackupRepository controller's
// waitForInitJob; a failed inventory Job is an error the caller turns into a discovery retry.
func (l *JobSnapshotLister) waitForJob(ctx context.Context, jobName string) error {
	key := client.ObjectKey{Namespace: l.OperatorNamespace, Name: jobName}
	ticker := time.NewTicker(discoveryJobPollInterval)
	defer ticker.Stop()

	for {
		var job batchv1.Job
		err := l.Get(ctx, key, &job)
		switch {
		case apierrors.IsNotFound(err):
			// The Job was just created (ensureSnapshotsJob) but the cached informer may not have it
			// yet, or a predecessor from the previous List is still terminating. A transient NotFound
			// is "not ready", not a failure: keep polling until the Job appears or the List deadline
			// (discoveryJobDeadline) elapses, so an inventory — and thus a whole discovery pass — is
			// never failed by a cache lag. This is why List blocks synchronously right after creating
			// the Job (unlike the Backup controller, which re-GETs the mover Job in a later reconcile
			// once the cache has caught up).
		case err != nil:
			return fmt.Errorf("get discovery job %s/%s: %w", l.OperatorNamespace, jobName, err)
		case job.Status.Succeeded >= 1 || jobConditionTrue(&job, batchv1.JobComplete):
			return nil
		case jobConditionTrue(&job, batchv1.JobFailed) || job.Status.Failed > discoveryJobBackoffLimit:
			return fmt.Errorf("discovery job %s/%s failed (failed pods=%d, backoffLimit=%d)",
				l.OperatorNamespace, jobName, job.Status.Failed, discoveryJobBackoffLimit)
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("discovery job %s/%s did not complete: %w", l.OperatorNamespace, jobName, ctx.Err())
		case <-ticker.C:
		}
	}
}

// readPodLog returns the stdout+stderr log of the snapshots Job's SUCCEEDED pod. It finds the pod by
// the batch job-name label (as readMoverResult does) and streams its log through the clientset — the
// controller-runtime client cannot read a pod log subresource. The mover container's stdout (the
// `restic snapshots --json` array, tee'd to the pod log) is what extractJSONArray then slices out.
func (l *JobSnapshotLister) readPodLog(ctx context.Context, jobName string) ([]byte, error) {
	var pods corev1.PodList
	// l.Client.List, not l.List: this type's own List method (the SnapshotLister entry point) shadows
	// the embedded client's promoted List, so the client's is reached through the embedded field.
	if err := l.Client.List(ctx, &pods, client.InNamespace(l.OperatorNamespace),
		client.MatchingLabels{batchv1.JobNameLabel: jobName}); err != nil {
		return nil, fmt.Errorf("list discovery pods for job %s: %w", jobName, err)
	}
	pod := succeededPod(pods.Items)
	if pod == nil {
		return nil, fmt.Errorf("no succeeded pod for discovery job %s/%s", l.OperatorNamespace, jobName)
	}

	// The mover pod has exactly one container (BuildJob), so an empty PodLogOptions.Container selects
	// it — matching the mover convention of addressing that container by index, never by name.
	stream, err := l.Clientset.CoreV1().
		Pods(l.OperatorNamespace).
		GetLogs(pod.Name, &corev1.PodLogOptions{}).
		Stream(ctx)
	if err != nil {
		return nil, fmt.Errorf("stream logs of discovery pod %s/%s: %w", l.OperatorNamespace, pod.Name, err)
	}
	defer func() { _ = stream.Close() }()

	data, err := io.ReadAll(stream)
	if err != nil {
		return nil, fmt.Errorf("read logs of discovery pod %s/%s: %w", l.OperatorNamespace, pod.Name, err)
	}
	return data, nil
}

// cleanup best-effort deletes the one-shot snapshots Job (foreground, so its pod goes too) and the
// job-scoped creds Secret. Failures are logged, never returned: the Job's TTL and the orphan reaper
// are the backstop, and a leftover inventory resource is harmless (it is re-adopted next List).
func (l *JobSnapshotLister) cleanup(ctx context.Context, name string) {
	// Background (not Foreground) propagation: discovery re-creates a Job of the SAME name every
	// inventory pass, so the object name must be freed IMMEDIATELY — a Foreground delete leaves the
	// Job in Terminating until its pods are gone (slow under storage load), and the next pass's Create
	// then races that terminating predecessor (AlreadyExists, no fresh Job, then a NotFound as it
	// finally vanishes). Background frees the name at once; the k8s GC reclaims the completed pods
	// asynchronously. The one-shot maintenance ops keep Foreground because their names are unique.
	bg := metav1.DeletePropagationBackground
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: l.OperatorNamespace}}
	if err := l.Delete(ctx, job, &client.DeleteOptions{PropagationPolicy: &bg}); err != nil && !apierrors.IsNotFound(err) {
		logf.FromContext(ctx).Error(err, "best-effort delete of discovery job failed", "job", name)
	}
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: l.OperatorNamespace}}
	if err := l.Delete(ctx, sec); err != nil && !apierrors.IsNotFound(err) {
		logf.FromContext(ctx).Error(err, "best-effort delete of discovery creds secret failed", "secret", name)
	}
}

// succeededPod returns the first pod whose mover container terminated with exit code 0, or nil.
// `restic snapshots` is idempotent, so any succeeded pod's log carries the full inventory; a Job
// that retried leaves earlier Failed pods whose logs must be ignored.
func succeededPod(pods []corev1.Pod) *corev1.Pod {
	for i := range pods {
		cs := pods[i].Status.ContainerStatuses
		if len(cs) > 0 && cs[0].State.Terminated != nil && cs[0].State.Terminated.ExitCode == 0 {
			return &pods[i]
		}
	}
	return nil
}

// extractJSONArray returns the outermost JSON array from a mover pod log. The log interleaves
// restic's stderr (progress, warnings) with its stdout (the `--json` array), so the raw bytes are
// not parseable as-is; the array is the region from the first '[' to the last ']'. `restic
// snapshots --json` always prints an array (an empty repository yields "[]"), so both brackets are
// present on a clean run. When a bracket is missing (an unexpected, non-JSON log) the raw input is
// returned so ParseSnapshots surfaces a naming parse error rather than this helper hiding it.
func extractJSONArray(log []byte) []byte {
	start := bytes.IndexByte(log, '[')
	end := bytes.LastIndexByte(log, ']')
	if start < 0 || end < start {
		return log
	}
	return log[start : end+1]
}
