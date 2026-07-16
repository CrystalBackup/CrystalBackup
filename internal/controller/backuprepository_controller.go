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
	"sync"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
	"github.com/CrystalBackup/CrystalBackup/internal/client/secrets"
	"github.com/CrystalBackup/CrystalBackup/internal/keys"
	"github.com/CrystalBackup/CrystalBackup/internal/mover"
	"github.com/CrystalBackup/CrystalBackup/internal/repo/queue"
	"github.com/CrystalBackup/CrystalBackup/internal/restic"
	"github.com/CrystalBackup/CrystalBackup/internal/status"
)

// ConditionInitialized reports whether the shared restic repository behind this
// BackupRepository has been created (`restic init`). It is the SECOND controller's headline
// verdict, mirroring ConditionReachable/ConditionEncryptionValid on the ClusterBackupLocation:
// the M1 crucible (test/crucible/tests/m1_repository_test.go) grades a repository by
// status.initialized + this condition, so the string is a cross-repo contract, not a local name.
const ConditionInitialized = "Initialized"

const (
	// keySlotPlatform is the single key slot a cluster-DR repository advertises in M1: the
	// platform DEK (one wrapped DEK per ClusterBackupLocation). Tenant repositories add a
	// "tenant" slot later; the cluster plane has only this one.
	keySlotPlatform = "platform"

	// scopeCluster is BackupRepository.status.scope for a repository backing a
	// (cluster-scoped) ClusterBackupLocation, as asserted by the crucible.
	scopeCluster = "Cluster"

	// kindClusterBackupLocation is the status.location.kind a cluster-DR repository records.
	kindClusterBackupLocation = "ClusterBackupLocation"
)

const (
	// initInFlightRequeueInterval paces re-reconciles while an init operation is enqueued or
	// running: short, because the reconciler is polling the exclusive-queue Handle for the
	// op's completion (the Owns(Job) watch is a secondary, best-effort nudge). ~10s.
	initInFlightRequeueInterval = 10 * time.Second

	// initFailedRequeueInterval backs a repository off after a terminal init failure before the
	// reconciler re-enqueues a fresh init Job. ~30s.
	initFailedRequeueInterval = 30 * time.Second
)

const (
	// initJobBackoffLimit is the init Job's spec.backoffLimit: a couple of pod-level retries
	// absorb a transient S3 blip, after which restic-init failing is treated as terminal.
	initJobBackoffLimit int32 = 2

	// initJobTTLSeconds is the init Job's ttlSecondsAfterFinished: a finished init Job is
	// self-cleaning after an hour even if the explicit success/failure cleanup is missed.
	initJobTTLSeconds int32 = 3600

	// initJobPollInterval is how often runInit re-reads the init Job while awaiting completion.
	initJobPollInterval = 2 * time.Second

	// initJobDeadline bounds a single runInit: the exclusive queue slot for this repository is
	// held for at most this long, so a black-holed init can never wedge the repository's lane
	// forever. It is generous (restic init against reachable S3 is seconds) and only trips on a
	// genuinely stuck Job; the operator context (Manager.Stop) cancels sooner on shutdown.
	initJobDeadline = 10 * time.Minute
)

// initResourceName is the deterministic name of BOTH the per-repository init Job and its
// job-scoped credentials Secret in the operator namespace. One name, two kinds: fixed so a
// restarted operator RE-ADOPTS the already-running init (Create tolerates AlreadyExists)
// instead of racing a second `restic init` on the empty shared repo — the K8up #1055 fix.
func initResourceName(repoName string) string {
	return repoName + "-init"
}

// initJobLabels stamps the init Job (and its pod template) for discovery. It deliberately does
// NOT use app.kubernetes.io/name=crystal-backup — that is the operator pod's label, which the
// crucible's m1DeleteOperatorPod selects on; a mover pod carrying it would be swept up by an
// operator-restart test. The mover carries its own name instead.
func initJobLabels() map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       "crystal-mover",
		"app.kubernetes.io/managed-by": "crystal-backup",
		"app.kubernetes.io/component":  "repo-init",
	}
}

// BackupRepositoryReconciler reconciles a BackupRepository: the operator-internal state of the
// ONE shared restic repository behind a ClusterBackupLocation. Its single non-trivial job in M1
// is to LAZILY and EXACTLY-ONCE initialize that repository — a `restic init` that must never
// race another init on the same empty repo (K8up #1055). It achieves that by driving init
// through the per-repository exclusive queue (internal/repo/queue): the reconcile loop enqueues
// one OpInit whose op spans the whole create-Job-and-wait, and tracks the in-flight Handle in a
// process-local map so it neither piles up ops nor loses track of one across reconciles. On an
// operator restart the map is empty, so the reconciler re-enqueues and runInit RE-ADOPTS the
// still-running init Job (idempotent Create) rather than starting a second one.
//
// It is the single writer of BackupRepository.status: runInit (which runs in the queue worker
// goroutine) performs I/O only and returns an error, and Reconcile alone translates that outcome
// into status. That split keeps the status machine race-free despite the concurrent worker.
type BackupRepositoryReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Secrets is the ONLY path this controller reads Secrets through: the uncached GET-by-name
	// reader (internal/client/secrets, invariant I3). It reads the cluster KEK and the DR S3
	// credentials from OperatorNamespace.
	Secrets *secrets.ByNameReader
	// Queue is the per-repository exclusive work queue. Init is enqueued here so it serialises
	// against any future forget/prune/check on the same repository, and so two reconciles (or a
	// reconcile racing a restarted process) can never run two inits at once.
	Queue *queue.Manager
	// OperatorNamespace is where the cluster KEK, the wrapped DEK, the DR S3 credentials, the
	// per-Job creds Secret and the mover Jobs all live.
	OperatorNamespace string
	// MoverImage is the image the init Job runs (the CrystalBackup image carrying crystal-mover
	// + restic). Required for real backups; empty is tolerated only because envtest never runs
	// the Job.
	MoverImage string
	Recorder   record.EventRecorder

	// mu guards inflight. inflight tracks the in-flight init Handle per repoKey so the
	// reconciler is restart-safe (empty map after restart -> re-enqueue -> re-adopt) and never
	// piles up ops (a non-nil Handle means "one init is already enqueued for this repo; poll it,
	// do not enqueue another"). Distinct BackupRepositories reconcile concurrently and share this
	// one map, which is why it needs a lock; a single object is reconciled serially by
	// controller-runtime.
	mu       sync.Mutex
	inflight map[string]*queue.Handle
}

// NewBackupRepositoryReconciler builds a BackupRepositoryReconciler with its in-flight-handle
// map initialised. Callers (main.go, the envtest suite) must go through this constructor so the
// map is never nil.
func NewBackupRepositoryReconciler(
	c client.Client,
	scheme *runtime.Scheme,
	secretsReader *secrets.ByNameReader,
	q *queue.Manager,
	operatorNamespace, moverImage string,
	recorder record.EventRecorder,
) *BackupRepositoryReconciler {
	return &BackupRepositoryReconciler{
		Client:            c,
		Scheme:            scheme,
		Secrets:           secretsReader,
		Queue:             q,
		OperatorNamespace: operatorNamespace,
		MoverImage:        moverImage,
		Recorder:          recorder,
		inflight:          map[string]*queue.Handle{},
	}
}

// +kubebuilder:rbac:groups=crystalbackup.io,resources=backuprepositories,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=crystalbackup.io,resources=backuprepositories/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=crystalbackup.io,resources=backuprepositories/finalizers,verbs=update
// +kubebuilder:rbac:groups=crystalbackup.io,resources=clusterbackuplocations,verbs=get;list;watch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile drives one BackupRepository towards an initialized shared repository. After
// deletion-handling, finalizer-ensuring and resolving the owning ClusterBackupLocation, it
// populates the repository's identity in status (idempotently), ensures the platform DEK exists,
// and — if not yet initialized — drives a single exclusive-queue init through the in-flight
// Handle state machine. Every mutation of status flows through here (runInit only does I/O), so
// the status subresource has exactly one writer.
func (r *BackupRepositoryReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var br cbv1.BackupRepository
	if err := r.Get(ctx, req.NamespacedName, &br); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("get BackupRepository %s: %w", req.Name, err)
	}

	if !br.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, &br)
	}

	if controllerutil.AddFinalizer(&br, apiconst.FinalizerRepository) {
		if err := r.Update(ctx, &br); err != nil {
			return ctrl.Result{}, fmt.Errorf("add finalizer to BackupRepository %s: %w", br.Name, err)
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Resolve the owning ClusterBackupLocation from the controller ownerReference the
	// ClusterBackupLocation controller set. Without it there is nothing to initialize.
	cbl, err := r.resolveOwningLocation(ctx, &br)
	if err != nil {
		if apierrors.IsNotFound(err) {
			status.SetCondition(&br.Status.Conditions, ConditionInitialized, metav1.ConditionFalse, "NoOwningLocation",
				"owning ClusterBackupLocation not found; nothing to initialize", br.Generation)
			status.SetCondition(&br.Status.Conditions, ConditionReady, metav1.ConditionFalse, "NoOwningLocation",
				"owning ClusterBackupLocation not found", br.Generation)
			if uerr := r.Status().Update(ctx, &br); uerr != nil {
				return ctrl.Result{}, fmt.Errorf("update status for BackupRepository %s: %w", br.Name, uerr)
			}
			return ctrl.Result{RequeueAfter: shortRequeueInterval}, nil
		}
		return ctrl.Result{}, fmt.Errorf("resolve owning location for BackupRepository %s: %w", br.Name, err)
	}

	// Populate the repository's identity in status (idempotent — the same inputs every pass).
	br.Status.Location = cbv1.RepositoryLocationRef{Kind: kindClusterBackupLocation, Name: cbl.Name}
	br.Status.Scope = scopeCluster
	br.Status.Mode = cbl.Spec.Mode
	br.Status.RepositoryURL = restic.RepoURL(cbl.Spec.S3.Endpoint, cbl.Spec.S3.Bucket, cbl.Spec.S3.Prefix, cbl.Spec.ClusterID)

	// Ensure the platform DEK (the restic repository password) exists and is persisted wrapped.
	// The plaintext DEK lives only in this local variable and the queue closure that captures it.
	dek, ok := r.ensurePlatformDEK(ctx, &br, cbl)
	if !ok {
		if uerr := r.Status().Update(ctx, &br); uerr != nil {
			return ctrl.Result{}, fmt.Errorf("update status for BackupRepository %s: %w", br.Name, uerr)
		}
		return ctrl.Result{RequeueAfter: shortRequeueInterval}, nil
	}
	br.Status.KeySlots = []string{keySlotPlatform}

	// Already initialized: re-assert the terminal conditions and re-evaluate periodically.
	if br.Status.Initialized {
		status.SetCondition(&br.Status.Conditions, ConditionInitialized, metav1.ConditionTrue, "Initialized",
			"shared repository is initialized", br.Generation)
		status.SetCondition(&br.Status.Conditions, ConditionReady, metav1.ConditionTrue, "Ready",
			"repository is initialized and its key material is present", br.Generation)
		if err := r.Status().Update(ctx, &br); err != nil {
			return ctrl.Result{}, fmt.Errorf("update status for BackupRepository %s: %w", br.Name, err)
		}
		return ctrl.Result{RequeueAfter: periodicRequeueInterval}, nil
	}

	return r.driveInit(ctx, &br, cbl, dek)
}

// finalize handles a BackupRepository with a non-zero DeletionTimestamp. Per adr/0009 delete
// NEVER erases: no S3 object is touched, and — critically — the platform DEK Secret is left in
// place. The DEK is sticky: it is the ONLY key that can open the shared repository, so deleting
// it here would orphan every snapshot the moment the BackupRepository is recreated; decommission
// is an explicit M5 action, not a side effect of GC. The init Job and its creds Secret are owned
// by this BackupRepository and left for Kubernetes' garbage collector. This method only drops
// the finalizer once there is nothing left for THIS controller to do.
func (r *BackupRepositoryReconciler) finalize(ctx context.Context, br *cbv1.BackupRepository) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(br, apiconst.FinalizerRepository) {
		return ctrl.Result{}, nil
	}

	// Drop the in-flight tracking for this repo so a delete-then-recreate does not think an init
	// from the previous incarnation is still running.
	r.mu.Lock()
	delete(r.inflight, br.Name)
	r.mu.Unlock()

	r.Recorder.Event(br, corev1.EventTypeNormal, "Finalizing",
		"removing finalizer; no S3 data is erased and the platform DEK Secret is retained (adr/0009) — "+
			"decommissioning the repository is an explicit action, not a delete side effect")

	controllerutil.RemoveFinalizer(br, apiconst.FinalizerRepository)
	if err := r.Update(ctx, br); err != nil {
		return ctrl.Result{}, fmt.Errorf("remove finalizer from BackupRepository %s: %w", br.Name, err)
	}
	return ctrl.Result{}, nil
}

// resolveOwningLocation returns the ClusterBackupLocation named by br's controller
// ownerReference. It returns an apierrors NotFound error when there is no controller reference or
// when the referenced location is gone, so the caller can treat "no owner (yet)" the same as "a
// transiently missing owner": degrade and requeue, never hard-fail.
func (r *BackupRepositoryReconciler) resolveOwningLocation(ctx context.Context, br *cbv1.BackupRepository) (*cbv1.ClusterBackupLocation, error) {
	owner := metav1.GetControllerOf(br)
	if owner == nil || owner.Kind != kindClusterBackupLocation {
		return nil, apierrors.NewNotFound(
			cbv1.GroupVersion.WithResource("clusterbackuplocations").GroupResource(), "<none>")
	}
	var cbl cbv1.ClusterBackupLocation
	if err := r.Get(ctx, client.ObjectKey{Name: owner.Name}, &cbl); err != nil {
		return nil, err
	}
	return &cbl, nil
}

// ensurePlatformDEK reads the cluster KEK, ensures the wrapped platform DEK Secret exists (via
// keys.DEKManager, which mints-once and reuses-forever), and returns the plaintext DEK. On any
// failure it sets ConditionInitialized/Ready=False with a Secret-naming reason (never the key
// material) and returns ok=false; the caller persists status and requeues.
func (r *BackupRepositoryReconciler) ensurePlatformDEK(ctx context.Context, br *cbv1.BackupRepository, cbl *cbv1.ClusterBackupLocation) (string, bool) {
	kekName := cbl.Spec.Encryption.ClusterKEKSecretRef.Name

	identity, err := r.Secrets.GetValue(ctx, r.OperatorNamespace, kekName, kekIdentityDataKey)
	if err != nil {
		r.setInitBlocked(br, "KEKUnavailable",
			fmt.Sprintf("read cluster KEK secret %s/%s: %v", r.OperatorNamespace, kekName, err))
		return "", false
	}
	wrapper, err := keys.NewAgeWrapper(string(identity))
	if err != nil {
		r.setInitBlocked(br, "KEKInvalid",
			fmt.Sprintf("parse cluster KEK secret %s/%s: %v", r.OperatorNamespace, kekName, err))
		return "", false
	}
	dek, err := keys.NewDEKManager(r.Client, wrapper, r.OperatorNamespace).EnsureDEK(ctx, cbl.Name)
	if err != nil {
		r.setInitBlocked(br, "DEKUnavailable",
			fmt.Sprintf("ensure platform DEK for location %s: %v", cbl.Name, err))
		return "", false
	}
	return dek, true
}

// setInitBlocked records a pre-init blocker (missing KEK, unminted DEK) on both the Initialized
// and Ready conditions with the same reason/message.
func (r *BackupRepositoryReconciler) setInitBlocked(br *cbv1.BackupRepository, reason, message string) {
	status.SetCondition(&br.Status.Conditions, ConditionInitialized, metav1.ConditionFalse, reason, message, br.Generation)
	status.SetCondition(&br.Status.Conditions, ConditionReady, metav1.ConditionFalse, reason, message, br.Generation)
}

// driveInit is the in-flight-Handle state machine — the crux of this controller. It admits at
// most one enqueued init per repository (tracked in r.inflight), polls its Handle without
// blocking the reconcile goroutine, and translates the op's terminal outcome into status. It is
// only reached when the repository is not yet initialized.
func (r *BackupRepositoryReconciler) driveInit(ctx context.Context, br *cbv1.BackupRepository, cbl *cbv1.ClusterBackupLocation, dek string) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	repoKey := br.Name

	r.mu.Lock()
	h := r.inflight[repoKey]
	r.mu.Unlock()

	if h == nil {
		// No init in flight: enqueue one. The op SPANS create-Job-and-wait so the exclusive queue
		// serialises init against any future forget/prune on this repo, and so a restarted process
		// (empty inflight map) re-enqueues and RE-ADOPTS the running Job instead of racing a second
		// init. The op does NOT write BR status — Reconcile owns that.
		owner := br.DeepCopy()
		repoURL := br.Status.RepositoryURL
		s3 := cbl.Spec.S3
		handle, enqErr := r.Queue.Enqueue(repoKey, queue.OpInit, func(opCtx context.Context) error {
			return r.runInit(opCtx, owner, repoURL, dek, s3)
		})
		if enqErr != nil {
			// The queue is shutting down; retry soon rather than treating it as an init failure.
			log.Info("init not enqueued; repo queue is stopping, will retry", "repository", repoKey, "err", enqErr.Error())
			status.SetCondition(&br.Status.Conditions, ConditionInitialized, metav1.ConditionFalse, "Initializing",
				"init not yet enqueued (queue stopping); retrying", br.Generation)
			if err := r.Status().Update(ctx, br); err != nil {
				return ctrl.Result{}, fmt.Errorf("update status for BackupRepository %s: %w", br.Name, err)
			}
			return ctrl.Result{RequeueAfter: initInFlightRequeueInterval}, nil
		}

		r.mu.Lock()
		r.inflight[repoKey] = handle
		r.mu.Unlock()

		status.SetCondition(&br.Status.Conditions, ConditionInitialized, metav1.ConditionFalse, "Initializing",
			"restic init enqueued on the repository's exclusive queue", br.Generation)
		status.SetCondition(&br.Status.Conditions, ConditionReady, metav1.ConditionFalse, "Initializing",
			"repository is initializing", br.Generation)
		if err := r.Status().Update(ctx, br); err != nil {
			return ctrl.Result{}, fmt.Errorf("update status for BackupRepository %s: %w", br.Name, err)
		}
		return ctrl.Result{RequeueAfter: initInFlightRequeueInterval}, nil
	}

	// An init is in flight: poll its Handle without blocking the reconcile goroutine.
	select {
	case <-h.Done():
		// The op finished; clear the tracking slot before acting on the result so a subsequent
		// reconcile re-drives cleanly (re-enqueues on a failure, short-circuits on the persisted
		// success).
		r.mu.Lock()
		delete(r.inflight, repoKey)
		r.mu.Unlock()

		if e := h.Err(); e != nil {
			log.Error(e, "repository init failed", "repository", repoKey)
			status.SetCondition(&br.Status.Conditions, ConditionInitialized, metav1.ConditionFalse, "InitFailed",
				fmt.Sprintf("restic init failed: %v", e), br.Generation)
			status.SetCondition(&br.Status.Conditions, ConditionReady, metav1.ConditionFalse, "InitFailed",
				"repository init failed", br.Generation)
			if err := r.Status().Update(ctx, br); err != nil {
				return ctrl.Result{}, fmt.Errorf("update status for BackupRepository %s: %w", br.Name, err)
			}
			// Delete the failed Job (foreground, so its pods go too) so the next reconcile's
			// re-enqueue builds a FRESH init Job rather than re-adopting the failed one.
			r.deleteInitJob(ctx, br.Name, metav1.DeletePropagationForeground)
			return ctrl.Result{RequeueAfter: initFailedRequeueInterval}, nil
		}

		// Success. Persist Initialized=true BEFORE deleting the Job: the Job-delete watch event
		// re-triggers reconcile, and it must observe the persisted success so it short-circuits
		// instead of re-enqueueing a redundant init.
		br.Status.Initialized = true
		status.SetCondition(&br.Status.Conditions, ConditionInitialized, metav1.ConditionTrue, "Initialized",
			"shared repository initialized", br.Generation)
		status.SetCondition(&br.Status.Conditions, ConditionReady, metav1.ConditionTrue, "Ready",
			"repository is initialized and its key material is present", br.Generation)
		if err := r.Status().Update(ctx, br); err != nil {
			return ctrl.Result{}, fmt.Errorf("update status for BackupRepository %s: %w", br.Name, err)
		}
		r.Recorder.Event(br, corev1.EventTypeNormal, "Initialized",
			"shared restic repository initialized at "+br.Status.RepositoryURL)
		// Best-effort cleanup of the one-shot init resources now that init succeeded.
		r.deleteInitJob(ctx, br.Name, metav1.DeletePropagationBackground)
		r.deleteInitCredsSecret(ctx, br.Name)
		return ctrl.Result{RequeueAfter: periodicRequeueInterval}, nil

	default:
		// Still running: persist the (idempotent) identity fields set this pass and poll again soon.
		status.SetCondition(&br.Status.Conditions, ConditionInitialized, metav1.ConditionFalse, "Initializing",
			"restic init in progress on the repository's exclusive queue", br.Generation)
		status.SetCondition(&br.Status.Conditions, ConditionReady, metav1.ConditionFalse, "Initializing",
			"repository is initializing", br.Generation)
		if err := r.Status().Update(ctx, br); err != nil {
			return ctrl.Result{}, fmt.Errorf("update status for BackupRepository %s: %w", br.Name, err)
		}
		return ctrl.Result{RequeueAfter: initInFlightRequeueInterval}, nil
	}
}

// runInit is the init operation body, run in the queue worker goroutine (NOT the reconcile
// goroutine): it performs I/O only and returns an error, never touching BR status. It is
// IDEMPOTENT — every Create tolerates AlreadyExists — which is exactly what makes restart
// re-adoption safe: a fresh process that re-enqueues init finds the still-running Job and creds
// Secret already present and adopts them instead of racing a second `restic init`.
//
// It (1) reads the S3 credentials from the location's credentialsSecretRef, (2) ensures a
// job-scoped creds Secret holding the restic password (the DEK) and the two AWS keys, (3)
// ensures the init Job (a maintenance mover Job running `restic init`), then (4) polls the Job to
// terminal success or failure, honouring opCtx (Manager.Stop) and an overall deadline.
func (r *BackupRepositoryReconciler) runInit(opCtx context.Context, owner *cbv1.BackupRepository, repoURL, dek string, s3 cbv1.S3Spec) error {
	ctx, cancel := context.WithTimeout(opCtx, initJobDeadline)
	defer cancel()

	name := initResourceName(owner.Name)
	credsName := s3.CredentialsSecretRef.Name

	accessKey, err := r.Secrets.GetValue(ctx, r.OperatorNamespace, credsName, mover.SecretKeyAWSAccessKeyID)
	if err != nil {
		return fmt.Errorf("read S3 access key from secret %s/%s: %w", r.OperatorNamespace, credsName, err)
	}
	secretKey, err := r.Secrets.GetValue(ctx, r.OperatorNamespace, credsName, mover.SecretKeyAWSSecretAccessKey)
	if err != nil {
		return fmt.Errorf("read S3 secret key from secret %s/%s: %w", r.OperatorNamespace, credsName, err)
	}

	// The job-scoped creds Secret: the restic password (DEK) is consumed as a mounted file, the
	// AWS keys as env via secretKeyRef (see internal/mover). Owned by the BackupRepository so GC
	// reclaims it if the repository is deleted; also cleaned up explicitly on init success.
	credsSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: r.OperatorNamespace,
			Labels:    initJobLabels(),
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			mover.SecretKeyResticPassword:     []byte(dek),
			mover.SecretKeyAWSAccessKeyID:     accessKey,
			mover.SecretKeyAWSSecretAccessKey: secretKey,
		},
	}
	if err := controllerutil.SetControllerReference(owner, credsSecret, r.Scheme); err != nil {
		return fmt.Errorf("set controller reference on init creds secret %s: %w", name, err)
	}
	if err := r.Create(ctx, credsSecret); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create init creds secret %s/%s: %w", r.OperatorNamespace, name, err)
	}

	job := mover.BuildJob(mover.JobRequest{
		Name:         name,
		Namespace:    r.OperatorNamespace,
		Image:        r.MoverImage,
		Operation:    mover.OpInit,
		ResticArgs:   []string{"init"},
		RepoURL:      repoURL,
		SecretName:   name,
		PVC:          nil,
		BackoffLimit: initJobBackoffLimit,
		TTLSeconds:   initJobTTLSeconds,
		Labels:       initJobLabels(),
	})
	if err := controllerutil.SetControllerReference(owner, job, r.Scheme); err != nil {
		return fmt.Errorf("set controller reference on init job %s: %w", name, err)
	}
	if err := r.Create(ctx, job); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create init job %s/%s: %w", r.OperatorNamespace, name, err)
	}

	return r.waitForInitJob(ctx, name)
}

// waitForInitJob polls the init Job until it reports terminal success (Succeeded >= 1 or the
// Complete condition) or terminal failure (the Failed condition, or Failed pod count past the
// backoffLimit). It honours ctx (cancelled by Manager.Stop or the runInit deadline) and returns
// nil on success, a non-nil error on failure or cancellation. It writes NO status.
func (r *BackupRepositoryReconciler) waitForInitJob(ctx context.Context, jobName string) error {
	key := client.ObjectKey{Namespace: r.OperatorNamespace, Name: jobName}
	ticker := time.NewTicker(initJobPollInterval)
	defer ticker.Stop()

	for {
		var job batchv1.Job
		if err := r.Get(ctx, key, &job); err != nil {
			return fmt.Errorf("get init job %s/%s: %w", r.OperatorNamespace, jobName, err)
		}
		if job.Status.Succeeded >= 1 || jobConditionTrue(&job, batchv1.JobComplete) {
			return nil
		}
		if jobConditionTrue(&job, batchv1.JobFailed) || job.Status.Failed > initJobBackoffLimit {
			return fmt.Errorf("init job %s/%s failed (failed pods=%d, backoffLimit=%d)",
				r.OperatorNamespace, jobName, job.Status.Failed, initJobBackoffLimit)
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("init job %s/%s did not complete: %w", r.OperatorNamespace, jobName, ctx.Err())
		case <-ticker.C:
		}
	}
}

// jobConditionTrue reports whether job carries a condition of condType with status True.
func jobConditionTrue(job *batchv1.Job, condType batchv1.JobConditionType) bool {
	for i := range job.Status.Conditions {
		c := job.Status.Conditions[i]
		if c.Type == condType && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// deleteInitJob best-effort deletes the init Job with the given propagation policy, tolerating
// NotFound (already gone / never created).
func (r *BackupRepositoryReconciler) deleteInitJob(ctx context.Context, repoName string, propagation metav1.DeletionPropagation) {
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: initResourceName(repoName), Namespace: r.OperatorNamespace}}
	if err := r.Delete(ctx, job, &client.DeleteOptions{PropagationPolicy: &propagation}); err != nil && !apierrors.IsNotFound(err) {
		logf.FromContext(ctx).Error(err, "best-effort delete of init job failed", "job", job.Name)
	}
}

// deleteInitCredsSecret best-effort deletes the job-scoped creds Secret, tolerating NotFound.
func (r *BackupRepositoryReconciler) deleteInitCredsSecret(ctx context.Context, repoName string) {
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: initResourceName(repoName), Namespace: r.OperatorNamespace}}
	if err := r.Delete(ctx, sec); err != nil && !apierrors.IsNotFound(err) {
		logf.FromContext(ctx).Error(err, "best-effort delete of init creds secret failed", "secret", sec.Name)
	}
}

// SetupWithManager registers this reconciler: it watches BackupRepository directly and the init
// Job as an owned type, so a Job status change (the init Pod completing or failing) re-triggers
// the owning BackupRepository. The owned-Job watch is a secondary nudge; the in-flight Handle
// poll plus the short requeue while an init is running are the primary drivers of progress.
func (r *BackupRepositoryReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&cbv1.BackupRepository{}).
		Owns(&batchv1.Job{}).
		Named("backuprepository").
		Complete(r)
}
