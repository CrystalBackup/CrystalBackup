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
	"hash/fnv"
	"path"
	"slices"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
	"github.com/CrystalBackup/CrystalBackup/internal/client/secrets"
	"github.com/CrystalBackup/CrystalBackup/internal/concurrency"
	"github.com/CrystalBackup/CrystalBackup/internal/exposer"
	"github.com/CrystalBackup/CrystalBackup/internal/keys"
	"github.com/CrystalBackup/CrystalBackup/internal/mover"
	"github.com/CrystalBackup/CrystalBackup/internal/repo/queue"
	"github.com/CrystalBackup/CrystalBackup/internal/restic"
	"github.com/CrystalBackup/CrystalBackup/internal/status"
)

// backupPollInterval paces re-reconciles while a Backup is still driving its volumes forward:
// short, because progress is polled (an exposure becoming ready, a mover Job finishing). The
// label-based Job watch (see SetupWithManager) is a faster secondary nudge; this requeue is the
// primary, watch-independent driver so a Backup never stalls waiting on an event that a
// cross-namespace Owns() cannot deliver.
const backupPollInterval = 5 * time.Second

const (
	// moverJobTTLSeconds is the data-mover Job's ttlSecondsAfterFinished: a finished mover Job
	// self-cleans after an hour even if the explicit post-result delete is missed. The
	// reconciler deletes it eagerly on the happy/fail path; this is only the backstop.
	moverJobTTLSeconds int32 = 3600

	// moverNamePrefixMax caps a per-PVC NamePrefix so the derived mover Job name
	// (<prefix>-mover) stays within the 63-char DNS-1123 label limit that Kubernetes enforces
	// on a Job's name (it becomes the batch.kubernetes.io/job-name label value on its pods).
	// Truncation past this cap appends a deterministic hash so two long PVC names never collide.
	moverNamePrefixMax = 56
)

// backupReasonSkippedUnsupported is the VolumeStatus.reason a volume on storage without CSI
// snapshot support carries. It is asserted VERBATIM by the crucible
// (test/crucible/tests/m1_cascade_test.go, "A volume ... is Skipped, not Failed"), so the exact
// string is a cross-repo contract.
const backupReasonSkippedUnsupported = "CSISnapshotUnsupported"

// ExposerRegistry is the seam the Backup controller resolves a per-PVC SnapshotExposer through.
// It is the one method of internal/exposer.Registry the controller needs, extracted as an
// interface so envtest — which has no external snapshot CRDs or CSI driver — can inject a stub
// registry that returns a stub exposer. Production wires in *exposer.Registry.
type ExposerRegistry interface {
	For(ctx context.Context, pvc *corev1.PersistentVolumeClaim) (exposer.SnapshotExposer, error)
}

// BackupReconciler reconciles a Backup: CrystalBackup's single, plane-agnostic UNIT OF
// EXECUTION. For each PVC in its namespace that the run selects, it exposes a read-only
// point-in-time copy (internal/exposer, ADR 0003's static VS/VSC re-bind), backs that copy up
// with a data-mover Job (internal/mover), and records the per-volume result. It is the mirror of
// the BackupRepository controller's shape — a thin Reconcile that handles deletion first, then
// resolves its inputs (run config, location, repository, DEK, tenant) and drives a small
// per-PVC state machine — with one deliberate difference: the mover Jobs it creates live in the
// OPERATOR namespace (they carry the platform DEK) while the Backup itself is namespaced, so a
// mover Job can NOT be an owned object (a cross-namespace ownerReference is illegal). The Jobs
// are therefore tracked by deterministic name + labels and re-adopted by Get, and a label-based
// Job watch (not Owns) maps a finished Job back to its Backup.
//
// It is the single writer of Backup.status: every status mutation happens in Reconcile (the
// per-PVC steps mutate the in-memory VolumeStatus and perform I/O, but never write status
// themselves), so the status subresource has exactly one writer per object — the one reconcile
// goroutine controller-runtime runs for it.
type BackupReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Secrets is the ONLY path this controller reads Secrets through: the uncached GET-by-name
	// reader (internal/client/secrets, invariant I3). It reads the cluster KEK and the DR S3
	// credentials from OperatorNamespace.
	Secrets *secrets.ByNameReader
	// Exposers resolves the SnapshotExposer for a PVC. *exposer.Registry in production; a stub
	// in envtest (which cannot stand up real VolumeSnapshots).
	Exposers ExposerRegistry
	// OperatorNamespace is where the mover Jobs, their per-Job creds Secrets, the temp clone
	// PVCs and every cluster-plane platform Secret (KEK, DR S3 creds, wrapped DEKs) live.
	OperatorNamespace string
	// MoverImage is the image the mover Jobs run. Required for real backups; empty is tolerated
	// only because envtest simulates the Job outcome and never runs it.
	MoverImage string
	Recorder   events.EventRecorder
	// Queue is the per-repository exclusive work queue, SHARED with the BackupRepository controller
	// (main.go constructs one and passes it to both). The Backup controller enqueues the two
	// repository maintenance ops it triggers — retention forget after a successful backup, and a
	// stale-lock unlock after a hard-killed mover — on the repository's lane (keyed by the
	// BackupRepository name == the location name), so they can never race an init or another
	// maintenance op on the same repository (adr/0010).
	Queue *queue.Manager
}

// NewBackupReconciler builds a BackupReconciler. Callers (main.go, the envtest suite) go through
// this constructor to keep the wiring in one place, mirroring NewBackupRepositoryReconciler.
func NewBackupReconciler(
	c client.Client,
	scheme *runtime.Scheme,
	secretsReader *secrets.ByNameReader,
	exposers ExposerRegistry,
	operatorNamespace, moverImage string,
	recorder events.EventRecorder,
	q *queue.Manager,
) *BackupReconciler {
	return &BackupReconciler{
		Client:            c,
		Scheme:            scheme,
		Secrets:           secretsReader,
		Exposers:          exposers,
		OperatorNamespace: operatorNamespace,
		MoverImage:        moverImage,
		Recorder:          recorder,
		Queue:             q,
	}
}

// backupRunContext bundles the per-reconcile resolved inputs the per-PVC state machine needs, so
// each advance step reads them from one value instead of re-resolving. Everything here is a pure
// function of the Backup, its parent run, its location and repository — resolved once at the top
// of Reconcile.
type backupRunContext struct {
	scheduleRef   string // Backup.spec.scheduleRef -> restic "schedule=" tag (omitted if empty)
	run           string // the run == parent ClusterBackup name == Backup.name -> restic "run=" tag
	clusterID     string // location.spec.clusterID -> restic --host
	tenant        string // resolved tenant -> restic "tenant=" tag (security-load-bearing)
	repoName      string // BackupRepository name (== location name) -> the exclusive queue's repoKey
	repoURL       string // BackupRepository.status.repositoryURL -> RESTIC_REPOSITORY
	dek           string // the platform DEK == the restic repository password
	s3CredsSecret string // location.spec.s3.credentialsSecretRef.name (operator ns)
	// retention is the LOCATION's per-PVC keep policy (R24), read from the resolved
	// ClusterBackupLocation — not from the run — because one shared repository has one
	// authoritative policy (adr/0009). A `restic forget` applying it is enqueued once, on the
	// repository's exclusive queue, after the Backup finishes successfully (Standard mode only).
	retention cbv1.RetentionSpec
	// mode is the location's LocationMode; a retention forget runs in Standard mode only (an
	// Immutable location forbids prune/forget until object-lock expiry).
	mode         cbv1.LocationMode
	backoffLimit int32 // run.backoffLimit -> the mover Job's spec.backoffLimit
	// maxConcurrentMovers caps how many mover Jobs may run at once across the whole cascade
	// (0 == unlimited). Enforced as a best-effort cluster-wide semaphore before a mover is created
	// (internal/concurrency), so a wide fan-out paces its data movement instead of stampeding.
	maxConcurrentMovers int32
}

// +kubebuilder:rbac:groups=crystalbackup.io,resources=backups,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=crystalbackup.io,resources=backups/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=crystalbackup.io,resources=backups/finalizers,verbs=update
// +kubebuilder:rbac:groups=crystalbackup.io,resources=clusterbackups;clusterbackuplocations;backuprepositories,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=snapshot.storage.k8s.io,resources=volumesnapshots;volumesnapshotcontents,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=snapshot.storage.k8s.io,resources=volumesnapshotclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=storage.k8s.io,resources=storageclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups="";events.k8s.io,resources=events,verbs=create;patch

// Reconcile drives one Backup towards a terminal per-namespace result. After deletion-handling
// and finalizer-ensuring it short-circuits two inert cases (a discovery projection, an
// already-terminal Backup), resolves the effective run config + repository + DEK + tenant,
// enumerates the matching PVCs, advances ONE non-terminal volume through the per-PVC state
// machine, then rolls the per-volume phases up into the Backup's phase and writes status ONCE.
func (r *BackupReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var backup cbv1.Backup
	if err := r.Get(ctx, req.NamespacedName, &backup); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("get Backup %s/%s: %w", req.Namespace, req.Name, err)
	}

	if !backup.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, &backup)
	}

	// A discovery projection (M1 task #21) is a read-only materialized view of snapshots that
	// already exist in the repository, never a unit of execution. Never re-execute it — and,
	// checked BEFORE the finalizer is added, never even attach the execution finalizer: a
	// projection has no exposure or mover Job to tear down, and discovery owns its whole lifecycle
	// (it deletes the projection outright when the snapshots are gone), so an execution finalizer
	// would only delay that GC by a needless finalize round-trip.
	if backup.Annotations[apiconst.AnnotationProjected] == apiconst.AnnotationProjectedValue {
		return ctrl.Result{}, nil
	}

	if controllerutil.AddFinalizer(&backup, apiconst.FinalizerBackup) {
		if err := r.Update(ctx, &backup); err != nil {
			return ctrl.Result{}, fmt.Errorf("add finalizer to Backup %s/%s: %w", backup.Namespace, backup.Name, err)
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Terminal Backups are done: they neither re-execute nor requeue. Re-entry (e.g. a stray
	// Job watch event) returns here without touching status, preserving the terminal record.
	if isTerminalBackupPhase(backup.Status.Phase) {
		return ctrl.Result{}, nil
	}

	// (6) Resolve the effective run spec from the parent ClusterBackup named by the link label.
	// In M1 an execution Backup ALWAYS has a cluster parent (the namespace plane is M2); a
	// truly parentless manual Backup defaulting to all-PVCs is deferred to M2. Absent the
	// parent, degrade and requeue rather than invent a run.
	run, ok, err := r.resolveRun(ctx, &backup)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !ok {
		return r.gate(ctx, &backup, "NoParent",
			"no parent ClusterBackup resolved from label "+apiconst.LabelClusterBackup+
				" (an execution Backup requires a cluster parent in M1)")
	}

	// (7) Resolve the location and its repository; gate on the repository being initialized.
	var loc cbv1.ClusterBackupLocation
	if err := r.Get(ctx, client.ObjectKey{Name: backup.Spec.LocationRef.Name}, &loc); err != nil {
		if apierrors.IsNotFound(err) {
			return r.gate(ctx, &backup, "LocationNotFound",
				fmt.Sprintf("ClusterBackupLocation %q not found", backup.Spec.LocationRef.Name))
		}
		return ctrl.Result{}, fmt.Errorf("get ClusterBackupLocation %s: %w", backup.Spec.LocationRef.Name, err)
	}

	var repo cbv1.BackupRepository
	if err := r.Get(ctx, client.ObjectKey{Name: loc.Name}, &repo); err != nil {
		if apierrors.IsNotFound(err) {
			return r.gate(ctx, &backup, "RepositoryNotReady",
				fmt.Sprintf("BackupRepository %q does not exist yet", loc.Name))
		}
		return ctrl.Result{}, fmt.Errorf("get BackupRepository %s: %w", loc.Name, err)
	}
	if !repo.Status.Initialized {
		return r.gate(ctx, &backup, "RepositoryNotReady",
			fmt.Sprintf("BackupRepository %q is not initialized yet", loc.Name))
	}

	// The platform DEK is the restic repository password the mover needs.
	dek, reason, message, ok := r.ensureDEK(ctx, &loc)
	if !ok {
		return r.gate(ctx, &backup, reason, message)
	}

	rc := &backupRunContext{
		scheduleRef:         backup.Spec.ScheduleRef,
		run:                 backup.Name,
		clusterID:           loc.Spec.ClusterID,
		tenant:              r.tenantFor(ctx, backup.Namespace),
		repoName:            loc.Name,
		repoURL:             repo.Status.RepositoryURL,
		dek:                 dek,
		s3CredsSecret:       loc.Spec.S3.CredentialsSecretRef.Name,
		retention:           loc.Spec.Retention,
		mode:                loc.Spec.Mode,
		backoffLimit:        run.BackoffLimit,
		maxConcurrentMovers: run.MaxConcurrentMovers,
	}

	// (9) Enumerate matching PVCs and (idempotently) seed one VolumeStatus each.
	if err := r.ensureVolumes(ctx, &backup, run.PVCSelector); err != nil {
		return ctrl.Result{}, fmt.Errorf("enumerate PVCs for Backup %s/%s: %w", backup.Namespace, backup.Name, err)
	}

	// (10) Drive ONE non-terminal PVC forward this reconcile (sequential in M1; intra-Backup
	// parallelism + the global maxConcurrentMovers semaphore are deferred to task #22).
	teardownPVC := ""
	if idx := firstNonTerminalVolume(backup.Status.Volumes); idx >= 0 {
		tp, err := r.advanceVolume(ctx, &backup, &backup.Status.Volumes[idx], rc)
		if err != nil {
			return ctrl.Result{}, err
		}
		teardownPVC = tp
	}

	// (11) Single status writer: roll the per-volume phases up, record a terminal condition +
	// backupTime once, and write status exactly once.
	res, err := r.writeStatus(ctx, &backup)
	if err != nil {
		// Status not persisted (e.g. a write conflict): return WITHOUT tearing down, so the mover
		// Job survives and the next reconcile re-reads and re-records the same terminal result.
		return res, err
	}
	// The terminal result is now durable: safe to tear the just-finished volume's exposure + Job
	// down (best-effort; idempotent).
	if teardownPVC != "" {
		r.teardownVolume(ctx, &backup, teardownPVC)
	}
	// (12) Retention: once the Backup has reached a successful terminal phase, apply the LOCATION's
	// per-PVC keep policy with one `restic forget` on the repository's exclusive queue (skipped on
	// an Immutable location). This is reached at most once per Backup — the already-terminal
	// early-return at the top of Reconcile bars re-entry once writeStatus has persisted the terminal
	// phase — so no marker is needed to keep it from re-enqueuing.
	if backupSucceeded(backup.Status.Phase) {
		r.maybeEnqueueRetentionForget(ctx, &backup, rc)
	}
	return res, nil
}

// writeStatus rolls the per-volume phases up into the Backup phase, records the headline
// condition (and backupTime on first reaching a terminal phase), writes status once, and returns
// the requeue decision: none once terminal, a short poll while volumes are still in flight.
func (r *BackupReconciler) writeStatus(ctx context.Context, backup *cbv1.Backup) (ctrl.Result, error) {
	phase := string(status.RollUpVolumePhases(backup.Status.Volumes))
	backup.Status.Phase = phase

	terminal := isTerminalBackupPhase(phase)
	if terminal {
		if backup.Status.BackupTime == nil {
			now := metav1.Now()
			backup.Status.BackupTime = &now
		}
		setTerminalCondition(backup, phase)
	} else {
		status.SetCondition(&backup.Status.Conditions, ConditionReady, metav1.ConditionFalse, "InProgress",
			"backup is in progress ("+phase+")", backup.Generation)
	}

	if err := r.Status().Update(ctx, backup); err != nil {
		return ctrl.Result{}, fmt.Errorf("update status for Backup %s/%s: %w", backup.Namespace, backup.Name, err)
	}
	if terminal {
		return ctrl.Result{}, nil
	}
	return ctrl.Result{RequeueAfter: backupPollInterval}, nil
}

// gate records a non-terminal blocker (no parent, missing location, repository not ready, KEK/DEK
// unavailable) on the headline Ready condition, keeps the Backup Pending, and requeues on the
// fixable-fault cadence. It never advances a volume — the blocker must clear first.
func (r *BackupReconciler) gate(ctx context.Context, backup *cbv1.Backup, reason, message string) (ctrl.Result, error) {
	backup.Status.Phase = string(status.BackupPhasePending)
	status.SetCondition(&backup.Status.Conditions, ConditionReady, metav1.ConditionFalse, reason, message, backup.Generation)
	if err := r.Status().Update(ctx, backup); err != nil {
		return ctrl.Result{}, fmt.Errorf("update status for Backup %s/%s: %w", backup.Namespace, backup.Name, err)
	}
	return ctrl.Result{RequeueAfter: shortRequeueInterval}, nil
}

// finalize tears down anything a Backup left live before dropping its finalizer — the
// "effective cancel / no leak on delete" guarantee. For every volume it best-effort
// foreground-deletes the mover Job + its creds Secret by deterministic name (a no-op for a
// volume that never got one), and for the two phases where an exposure is provably still live
// (Snapshotting, Uploading) it reconstructs and Cleanup()s that exposure. A Completed/Failed
// volume's exposure was already torn down by the state machine, and a Pending/Skipped one never
// created any — re-exposing those just to clean them would spuriously create a fresh CSI
// snapshot, so they are left to the state machine's own teardown (and, for any residual crash
// window, the orphan reaper, M1 task #22). Nothing in the repository is ever erased (adr/0009).
func (r *BackupReconciler) finalize(ctx context.Context, backup *cbv1.Backup) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(backup, apiconst.FinalizerBackup) {
		return ctrl.Result{}, nil
	}
	log := logf.FromContext(ctx)

	for i := range backup.Status.Volumes {
		vol := &backup.Status.Volumes[i]
		if vol.Phase == status.VolumePhaseSkipped {
			continue // never exposed, never had a Job
		}
		if vol.Phase == status.VolumePhaseSnapshotting || vol.Phase == status.VolumePhaseUploading {
			if err := r.cleanupVolumeExposure(ctx, backup, vol.Pvc); err != nil {
				log.Error(err, "best-effort exposure cleanup on delete failed; leaving to the orphan reaper",
					"backup", backup.Name, "pvc", vol.Pvc)
			}
		}
		r.deleteMoverJobAndSecret(ctx, moverNamePrefix(backup.Name, vol.Pvc))
	}

	r.Recorder.Eventf(backup, nil, corev1.EventTypeNormal, "Finalizing", "Finalize",
		"tearing down live exposures and mover Jobs; no repository data is erased (adr/0009)")

	controllerutil.RemoveFinalizer(backup, apiconst.FinalizerBackup)
	if err := r.Update(ctx, backup); err != nil {
		if apierrors.IsNotFound(err) {
			// A concurrent finalize pass already removed the finalizer and the object is gone.
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("remove finalizer from Backup %s/%s: %w", backup.Namespace, backup.Name, err)
	}
	return ctrl.Result{}, nil
}

// resolveRun reads the effective ClusterBackupRunSpec from the parent ClusterBackup named by the
// crystalbackup.io/cluster-backup link label. ok=false (with a nil error) means "no parent
// resolvable yet" — either the label is absent or the ClusterBackup is gone — which the caller
// treats as a degrade-and-requeue, never a hard failure.
func (r *BackupReconciler) resolveRun(ctx context.Context, backup *cbv1.Backup) (*cbv1.ClusterBackupRunSpec, bool, error) {
	runName := backup.Labels[apiconst.LabelClusterBackup]
	if runName == "" {
		return nil, false, nil
	}
	var cb cbv1.ClusterBackup
	if err := r.Get(ctx, client.ObjectKey{Name: runName}, &cb); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("get parent ClusterBackup %s: %w", runName, err)
	}
	return &cb.Spec.ClusterBackupRunSpec, true, nil
}

// ensureDEK reads the cluster KEK and returns the plaintext platform DEK (the restic repository
// password) for loc, minting-and-wrapping it once via keys.DEKManager and reusing it forever
// after. On any failure it returns ok=false with a Secret-naming reason/message (never key
// material) for the caller to fold into the Ready condition.
func (r *BackupReconciler) ensureDEK(ctx context.Context, loc *cbv1.ClusterBackupLocation) (dek, reason, message string, ok bool) {
	kekName := loc.Spec.Encryption.ClusterKEKSecretRef.Name

	identity, err := r.Secrets.GetValue(ctx, r.OperatorNamespace, kekName, kekIdentityDataKey)
	if err != nil {
		return "", "KEKUnavailable", fmt.Sprintf("read cluster KEK secret %s/%s: %v", r.OperatorNamespace, kekName, err), false
	}
	wrapper, err := keys.NewAgeWrapper(string(identity))
	if err != nil {
		return "", "KEKInvalid", fmt.Sprintf("parse cluster KEK secret %s/%s: %v", r.OperatorNamespace, kekName, err), false
	}
	d, err := keys.NewDEKManager(r.Client, wrapper, r.OperatorNamespace).EnsureDEK(ctx, loc.Name)
	if err != nil {
		return "", "DEKUnavailable", fmt.Sprintf("ensure platform DEK for location %s: %v", loc.Name, err), false
	}
	return d, "", "", true
}

// tenantFor resolves the tenant of a namespace for the security-load-bearing restic "tenant="
// tag: the namespace's crystalbackup.io/tenant label if set, else the namespace name itself.
// The whole tenant derivation is kept behind this one helper deliberately — a richer tenant
// registry (M2/M5) replaces only this function, not every call site.
func (r *BackupReconciler) tenantFor(ctx context.Context, namespace string) string {
	var ns corev1.Namespace
	if err := r.Get(ctx, client.ObjectKey{Name: namespace}, &ns); err == nil {
		if t := ns.Labels[apiconst.LabelTenant]; t != "" {
			return t
		}
	}
	return namespace
}

// ensureVolumes lists the PVCs in the Backup's namespace, keeps those the run's PVCSelector
// matches, and appends a Pending VolumeStatus for any not already tracked — idempotently, so a
// re-reconcile preserves every existing per-PVC phase and only ever ADDS newly-appeared PVCs.
// Matched names are seeded in sorted order so the sequential drive is deterministic. A namespace
// with zero matching PVCs leaves status.Volumes empty, which rolls up to Completed.
func (r *BackupReconciler) ensureVolumes(ctx context.Context, backup *cbv1.Backup, sel cbv1.PVCSelector) error {
	var pvcs corev1.PersistentVolumeClaimList
	if err := r.List(ctx, &pvcs, client.InNamespace(backup.Namespace)); err != nil {
		return err
	}

	matched := make([]string, 0, len(pvcs.Items))
	for i := range pvcs.Items {
		if matchPVC(&pvcs.Items[i], sel) {
			matched = append(matched, pvcs.Items[i].Name)
		}
	}
	slices.Sort(matched)

	tracked := make(map[string]bool, len(backup.Status.Volumes))
	for i := range backup.Status.Volumes {
		tracked[backup.Status.Volumes[i].Pvc] = true
	}
	for _, name := range matched {
		if !tracked[name] {
			backup.Status.Volumes = append(backup.Status.Volumes,
				cbv1.VolumeStatus{Pvc: name, Phase: status.VolumePhasePending})
		}
	}
	return nil
}

// advanceVolume advances ONE volume by ONE step of the per-PVC state machine, keyed on its
// current phase. It mutates vol in place and performs I/O; it never writes Backup status (that is
// Reconcile's job). A non-error return with an unchanged phase means "still waiting — requeue".
// The returned string, when non-empty, is the PVC name of a volume that JUST reached a terminal
// phase this step and whose exposure + mover Job must be torn down — but only AFTER Reconcile has
// persisted the terminal result, so a status-write conflict never leaves the result unrecorded
// while its Job is already gone (the same "persist before delete" ordering the BackupRepository
// controller uses for its init Job).
func (r *BackupReconciler) advanceVolume(ctx context.Context, backup *cbv1.Backup, vol *cbv1.VolumeStatus, rc *backupRunContext) (string, error) {
	switch vol.Phase {
	case status.VolumePhasePending, "":
		return "", r.advancePending(ctx, backup, vol)
	case status.VolumePhaseSnapshotting:
		return "", r.advanceSnapshotting(ctx, backup, vol, rc)
	case status.VolumePhaseUploading:
		return r.advanceUploading(ctx, backup, vol, rc)
	default:
		return "", nil
	}
}

// advancePending resolves the exposer for the source PVC and starts the exposure. A storage
// class with no CSI snapshot support (exposer.ErrUnsupported) makes the volume Skipped /
// CSISnapshotUnsupported — a Skipped volume makes the Backup PartiallyCompleted, never Failed
// (status.RollUpVolumePhases encodes this). SnapshottingHooks (M4) are skipped in M1: Pending
// goes straight to Snapshotting.
func (r *BackupReconciler) advancePending(ctx context.Context, backup *cbv1.Backup, vol *cbv1.VolumeStatus) error {
	var pvc corev1.PersistentVolumeClaim
	if err := r.Get(ctx, client.ObjectKey{Namespace: backup.Namespace, Name: vol.Pvc}, &pvc); err != nil {
		if apierrors.IsNotFound(err) {
			vol.Phase = status.VolumePhaseFailed
			vol.Reason = "SourcePVCMissing"
			return nil
		}
		return fmt.Errorf("get source PVC %s/%s: %w", backup.Namespace, vol.Pvc, err)
	}

	ex, err := r.Exposers.For(ctx, &pvc)
	if err != nil {
		if errors.Is(err, exposer.ErrUnsupported) {
			vol.Phase = status.VolumePhaseSkipped
			vol.Reason = backupReasonSkippedUnsupported
			r.Recorder.Eventf(backup, nil, corev1.EventTypeNormal, "VolumeSkipped", "SkipVolume",
				"PVC %s is on storage without CSI snapshot support; skipped", vol.Pvc)
			return nil
		}
		return fmt.Errorf("resolve exposer for PVC %s/%s: %w", backup.Namespace, vol.Pvc, err)
	}

	if _, err := ex.Expose(ctx, r.exposeRequest(backup, &pvc)); err != nil {
		return fmt.Errorf("expose PVC %s/%s: %w", backup.Namespace, vol.Pvc, err)
	}
	vol.Phase = status.VolumePhaseSnapshotting
	return nil
}

// advanceSnapshotting waits for the exposure to be ready, then creates the data-mover Job. The
// exposure is reconstructed deterministically from the same NamePrefix (Expose is idempotent —
// it tolerates AlreadyExists and returns the same Exposure), which is what lets a restarted
// controller re-drive the handover without persisting the Exposure. Once ready it ensures the
// per-Job creds Secret (DEK + S3 keys) and the mover Job, both tolerating AlreadyExists so a
// re-reconcile re-adopts rather than duplicates.
func (r *BackupReconciler) advanceSnapshotting(ctx context.Context, backup *cbv1.Backup, vol *cbv1.VolumeStatus, rc *backupRunContext) error {
	ex, exposure, err := r.reconstructExposure(ctx, backup, vol.Pvc)
	if err != nil {
		return fmt.Errorf("reconstruct exposure for PVC %s/%s: %w", backup.Namespace, vol.Pvc, err)
	}
	ready, err := ex.Ready(ctx, exposure)
	if err != nil {
		return fmt.Errorf("check exposure readiness for PVC %s/%s: %w", backup.Namespace, vol.Pvc, err)
	}
	if !ready {
		return nil // still binding the static re-bind / temp PVC; requeue
	}

	identity := restic.DataIdentity(rc.clusterID, rc.tenant, backup.Namespace, vol.Pvc, rc.scheduleRef, rc.run)
	prefix := moverNamePrefix(backup.Name, vol.Pvc)
	moverName := prefix + "-mover"
	labels := exposureLabels(backup, vol.Pvc)

	// Cluster-wide mover concurrency gate. If this volume's mover Job does not exist yet and the
	// cascade is already at maxConcurrentMovers, hold the volume in Snapshotting (its exposure stays
	// ready) and requeue for a free slot. An already-existing Job means we are re-adopting after a
	// restart, never blocking — so an in-flight mover is never counted out of its own slot.
	if blocked, err := r.moverSlotBlocked(ctx, moverName, rc.repoName, rc.maxConcurrentMovers); err != nil {
		return err
	} else if blocked {
		return nil
	}

	if err := r.ensureMoverCredsSecret(ctx, moverName, rc.dek, rc.s3CredsSecret, labels); err != nil {
		return err
	}

	job := mover.BuildJob(mover.JobRequest{
		Name:         moverName,
		Namespace:    r.OperatorNamespace,
		Image:        r.MoverImage,
		Operation:    mover.OpBackup,
		ResticArgs:   resticBackupArgs(identity),
		RepoURL:      rc.repoURL,
		SecretName:   moverName,
		PVC:          &mover.PVCMount{ClaimName: exposure.ExposedPVCName, MountPath: identity.Path},
		BackoffLimit: rc.backoffLimit,
		TTLSeconds:   moverJobTTLSeconds,
		Labels:       labels,
		// Soft-spread the cascade's movers across nodes so a wide fan-out does not pile its data
		// movement onto one kubelet.
		SpreadOverLabels: map[string]string{apiconst.LabelManagedBy: apiconst.ManagedByValue},
	})
	// No ownerReference: the mover Job is in the operator namespace and the Backup in a tenant
	// namespace, so a cross-namespace ownerRef is illegal. The Job is tracked by its
	// deterministic name + labels and re-adopted by Get (AlreadyExists on Create).
	if err := r.Create(ctx, job); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create mover Job %s/%s: %w", r.OperatorNamespace, moverName, err)
	}

	vol.Phase = status.VolumePhaseUploading
	return nil
}

// moverSlotBlocked is the admission gate for one PVC's mover. It combines the per-repo backup⇄unlock
// mutex (reader side) with the cluster-wide concurrency cap. Re-adoption of an already-existing Job
// always proceeds (blocking a live mover would strand it and does nothing for either gate). For a
// NEW mover it blocks when either (a) an op that force-removes repository locks — a stale-lock
// unlock; queue.blocksMovers — is pending or in-flight for this repo (so a backup never takes a lock
// the unlock is about to nuke; the unlock's own drain-wait covers movers already running), or (b)
// the cascade is already at maxConcurrentMovers. The repository-mutex check runs even when the limit
// is unset (the default), so it is evaluated before the limit short-circuit.
func (r *BackupReconciler) moverSlotBlocked(ctx context.Context, moverName, repoName string, limit int32) (bool, error) {
	err := r.Get(ctx, client.ObjectKey{Namespace: r.OperatorNamespace, Name: moverName}, &batchv1.Job{})
	if err == nil {
		return false, nil // our Job already exists — re-adopting, never blocked.
	}
	if !apierrors.IsNotFound(err) {
		return false, fmt.Errorf("get mover Job %s/%s for the mover admission gate: %w", r.OperatorNamespace, moverName, err)
	}

	// (a) Repository backup⇄unlock mutex (reader side): hold a new mover back while a lock-removing
	// op is queued/running for this repo. Independent of the concurrency limit (unset by default).
	if r.Queue != nil && repoName != "" && r.Queue.QuiescenceRequired(repoName) {
		return true, nil
	}

	// (b) Cluster-wide concurrency cap. Unset ⇒ unlimited ⇒ the common single-tenant case pays for
	// nothing beyond the mutex check above.
	if limit <= 0 {
		return false, nil
	}
	movers, err := r.listMoverJobs(ctx)
	if err != nil {
		return false, fmt.Errorf("list mover Jobs for the concurrency gate: %w", err)
	}
	return !concurrency.CanStartMover(concurrency.RunningMoverJobs(movers), limit), nil
}

// listMoverJobs returns the per-PVC data-mover Jobs in the operator namespace — those carrying the
// managed-by AND a per-PVC label, so repository-init/maintenance Jobs (managed-by, no PVC label) are
// excluded. Shared by the concurrency gate and the unlock drain-wait.
func (r *BackupReconciler) listMoverJobs(ctx context.Context) ([]batchv1.Job, error) {
	var jobs batchv1.JobList
	if err := r.List(ctx, &jobs, client.InNamespace(r.OperatorNamespace),
		client.MatchingLabels{apiconst.LabelManagedBy: apiconst.ManagedByValue}); err != nil {
		return nil, err
	}
	movers := jobs.Items[:0]
	for _, j := range jobs.Items {
		if j.Labels[apiconst.LabelPVC] != "" { // per-PVC ⇒ a mover, not a repository-init/maintenance Job
			movers = append(movers, j)
		}
	}
	return movers, nil
}

// activeMoverCount counts the data-mover Jobs still occupying a slot: per-PVC, not terminal, and not
// being deleted — a torn-down crashed mover (DeletionTimestamp set by teardownVolume) must not hold
// the unlock drain-wait open. It is the reader census the backup⇄unlock mutex drains before an
// exclusive lock-removal runs.
func (r *BackupReconciler) activeMoverCount(ctx context.Context) (int, error) {
	movers, err := r.listMoverJobs(ctx)
	if err != nil {
		return 0, err
	}
	live := movers[:0]
	for _, j := range movers {
		if j.DeletionTimestamp == nil {
			live = append(live, j)
		}
	}
	return concurrency.RunningMoverJobs(live), nil
}

// advanceUploading polls the mover Job and, once it is terminal, RECORDS the result on the
// volume (but does NOT tear anything down — that is deferred to after Reconcile persists the
// result; see advanceVolume's return contract). Success (Job complete AND a well-formed ok=true
// MoverResult) records the snapshot id/sizes/node and Completes the volume. Any failure — the Job
// failing, or an EMPTY termination message (OOMKilled / SIGKILL: the mover died before it could
// report, which ParseMoverResult surfaces as an error) — Fails the volume with a short,
// secret-free reason. It returns the PVC name on either terminal outcome to request teardown.
func (r *BackupReconciler) advanceUploading(ctx context.Context, backup *cbv1.Backup, vol *cbv1.VolumeStatus, rc *backupRunContext) (string, error) {
	moverName := moverNamePrefix(backup.Name, vol.Pvc) + "-mover"

	var job batchv1.Job
	if err := r.Get(ctx, client.ObjectKey{Namespace: r.OperatorNamespace, Name: moverName}, &job); err != nil {
		if apierrors.IsNotFound(err) {
			// The Job is momentarily absent. This is almost always the Job informer lagging the
			// create we just issued; occasionally it is our own teardown (during finalize) racing
			// a stale reconcile. We deliberately neither re-drive to Snapshotting (which would
			// RE-CREATE the exposure + Job and, if the Backup is being deleted, leak a clone that
			// outlives it) NOR mark the volume Failed (which would false-fail on informer lag).
			// We simply wait and requeue; a genuinely lost Job is caught by the per-phase timeout
			// (deferred to task #22).
			return "", nil
		}
		return "", fmt.Errorf("get mover Job %s/%s: %w", r.OperatorNamespace, moverName, err)
	}

	complete := job.Status.Succeeded >= 1 || jobConditionTrue(&job, batchv1.JobComplete)
	failed := jobConditionTrue(&job, batchv1.JobFailed) || job.Status.Failed > rc.backoffLimit
	if !complete && !failed {
		return "", nil // still running; requeue
	}

	result, node, rerr := r.readMoverResult(ctx, moverName)
	vol.Node = node
	switch {
	case complete && rerr == nil && result.OK:
		vol.SnapshotID = result.SnapshotID
		vol.SizeBytes = result.SizeBytes
		vol.AddedBytes = result.AddedBytes
		vol.Phase = status.VolumePhaseCompleted
	default:
		vol.Phase = status.VolumePhaseFailed
		vol.Reason = moverFailureReason(result, rerr)
		r.Recorder.Eventf(backup, nil, corev1.EventTypeWarning, "VolumeFailed", "BackupVolume",
			"backup of PVC %s failed: %s", vol.Pvc, vol.Reason)
		// A BLANK or unparseable termination message (rerr != nil) is the load-bearing signal that
		// the mover was hard-killed (OOMKilled / SIGKILL) before it could report — so it may have
		// died holding the repository lock. Clear that stale lock so the next backup is not wedged.
		// A clean ok=false result (rerr == nil) needs no unlock: restic releases its own lock on any
		// orderly exit, a handled failure included.
		if rerr != nil {
			r.enqueueStaleLockUnlock(ctx, backup, rc)
		}
	}
	return vol.Pvc, nil // request teardown once Reconcile has persisted this terminal result
}

// teardownVolume tears an exposure + mover Job + creds Secret down after its terminal result has
// been persisted, best-effort (the orphan reaper backstops any residue). Called by Reconcile
// AFTER the status write so a status-write conflict never deletes the Job before the result it
// carries is recorded.
func (r *BackupReconciler) teardownVolume(ctx context.Context, backup *cbv1.Backup, pvcName string) {
	if err := r.cleanupVolumeExposure(ctx, backup, pvcName); err != nil {
		logf.FromContext(ctx).Error(err, "best-effort exposure cleanup after mover finish failed",
			"backup", backup.Name, "pvc", pvcName)
	}
	r.deleteMoverJobAndSecret(ctx, moverNamePrefix(backup.Name, pvcName))
}

// exposeRequest builds the ExposeRequest for one source PVC, deterministically from the
// Backup+PVC so that Expose, Ready and Cleanup — potentially across process restarts — all
// address the same objects. The stamped Labels are the reaper/leak-check selector.
func (r *BackupReconciler) exposeRequest(backup *cbv1.Backup, pvc *corev1.PersistentVolumeClaim) exposer.ExposeRequest {
	storageClass := ""
	if pvc.Spec.StorageClassName != nil {
		storageClass = *pvc.Spec.StorageClassName
	}
	return exposer.ExposeRequest{
		Namespace:    backup.Namespace,
		PVCName:      pvc.Name,
		StorageClass: storageClass,
		Capacity:     pvc.Spec.Resources.Requests[corev1.ResourceStorage],
		NamePrefix:   moverNamePrefix(backup.Name, pvc.Name),
		Labels:       exposureLabels(backup, pvc.Name),
	}
}

// reconstructExposure re-derives an exposer and its Exposure for a PVC without persisting either:
// it re-reads the PVC, re-resolves the exposer (Registry.For), and calls the idempotent Expose to
// obtain the deterministic Exposure (Expose tolerates AlreadyExists, so this converges on an
// existing exposure instead of duplicating it). Used by both the Snapshotting Ready() poll and
// the Cleanup teardown so they always operate on identically-named objects.
func (r *BackupReconciler) reconstructExposure(ctx context.Context, backup *cbv1.Backup, pvcName string) (exposer.SnapshotExposer, *exposer.Exposure, error) {
	var pvc corev1.PersistentVolumeClaim
	if err := r.Get(ctx, client.ObjectKey{Namespace: backup.Namespace, Name: pvcName}, &pvc); err != nil {
		return nil, nil, err
	}
	ex, err := r.Exposers.For(ctx, &pvc)
	if err != nil {
		return nil, nil, err
	}
	exposure, err := ex.Expose(ctx, r.exposeRequest(backup, &pvc))
	if err != nil {
		return nil, nil, err
	}
	return ex, exposure, nil
}

// cleanupVolumeExposure reconstructs the exposure and tears it down (exposer.Cleanup, which is
// idempotent and NotFound-tolerant). A source PVC that no longer exists, or one whose storage is
// unsupported (never exposed), is treated as "nothing to clean" — the orphan reaper backstops any
// residual objects those cases could leave.
func (r *BackupReconciler) cleanupVolumeExposure(ctx context.Context, backup *cbv1.Backup, pvcName string) error {
	ex, exposure, err := r.reconstructExposure(ctx, backup, pvcName)
	if err != nil {
		if apierrors.IsNotFound(err) || errors.Is(err, exposer.ErrUnsupported) {
			return nil
		}
		return err
	}
	return ex.Cleanup(ctx, exposure)
}

// ensureMoverCredsSecret creates the per-Job Secret the mover consumes: the DEK as the restic
// password (mounted file) and the two S3 credentials as env (secretKeyRef). It reads the S3
// credentials from the location's credentials Secret through the uncached reader (I3) and
// tolerates AlreadyExists so a re-reconcile re-adopts. The exposure labels are stamped so the
// reaper can find it.
func (r *BackupReconciler) ensureMoverCredsSecret(ctx context.Context, name, dek, s3CredsSecret string, labels map[string]string) error {
	accessKey, err := r.Secrets.GetValue(ctx, r.OperatorNamespace, s3CredsSecret, mover.SecretKeyAWSAccessKeyID)
	if err != nil {
		return fmt.Errorf("read S3 access key from secret %s/%s: %w", r.OperatorNamespace, s3CredsSecret, err)
	}
	secretKey, err := r.Secrets.GetValue(ctx, r.OperatorNamespace, s3CredsSecret, mover.SecretKeyAWSSecretAccessKey)
	if err != nil {
		return fmt.Errorf("read S3 secret key from secret %s/%s: %w", r.OperatorNamespace, s3CredsSecret, err)
	}
	creds := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: r.OperatorNamespace, Labels: labels},
		Type:       corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			mover.SecretKeyResticPassword:     []byte(dek),
			mover.SecretKeyAWSAccessKeyID:     accessKey,
			mover.SecretKeyAWSSecretAccessKey: secretKey,
		},
	}
	if err := r.Create(ctx, creds); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create mover creds secret %s/%s: %w", r.OperatorNamespace, name, err)
	}
	return nil
}

// readMoverResult finds the mover Job's pod (by the batch job-name label), reads the terminated
// container's termination message and parses it (mover.ParseMoverResult), returning the result
// and the node the pod ran on. A blank message parses to an error — the load-bearing signal that
// the mover was killed before it could report (OOMKilled/SIGKILL) — which the caller turns into a
// volume failure.
func (r *BackupReconciler) readMoverResult(ctx context.Context, jobName string) (mover.MoverResult, string, error) {
	var pods corev1.PodList
	if err := r.List(ctx, &pods,
		client.InNamespace(r.OperatorNamespace),
		client.MatchingLabels{batchv1.JobNameLabel: jobName}); err != nil {
		return mover.MoverResult{}, "", fmt.Errorf("list mover pods for job %s: %w", jobName, err)
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		cs := pod.Status.ContainerStatuses
		if len(cs) > 0 && cs[0].State.Terminated != nil {
			result, err := mover.ParseMoverResult(cs[0].State.Terminated.Message)
			return result, pod.Spec.NodeName, err
		}
	}
	return mover.MoverResult{}, "", fmt.Errorf("no terminated mover pod found for job %s/%s", r.OperatorNamespace, jobName)
}

// deleteMoverJobAndSecret best-effort deletes the mover Job and its creds Secret (both named
// <prefix>-mover in the operator namespace), tolerating NotFound. Errors are logged, not
// returned — teardown is best-effort and must never wedge the caller.
//
// Propagation is Background, not Foreground, deliberately: Background removes the Job object
// immediately and lets the garbage collector reap its pod asynchronously, whereas Foreground
// blocks the Job's removal on the GC controller deleting the pod first — which never happens in
// envtest (it runs only apiserver + etcd, no GC controller), leaving the Job wedged in
// Terminating forever. Background achieves the same teardown in both environments.
func (r *BackupReconciler) deleteMoverJobAndSecret(ctx context.Context, prefix string) {
	log := logf.FromContext(ctx)
	name := prefix + "-mover"
	background := metav1.DeletePropagationBackground
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: r.OperatorNamespace}}
	if err := r.Delete(ctx, job, &client.DeleteOptions{PropagationPolicy: &background}); err != nil && !apierrors.IsNotFound(err) {
		log.Error(err, "best-effort delete of mover job failed", "job", name)
	}
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: r.OperatorNamespace}}
	if err := r.Delete(ctx, sec); err != nil && !apierrors.IsNotFound(err) {
		log.Error(err, "best-effort delete of mover creds secret failed", "secret", name)
	}
}

// SetupWithManager registers this reconciler. It watches Backup directly and, via a label-based
// mapping (NOT Owns — the mover Jobs are in the operator namespace and cannot be owned by a
// namespaced Backup), maps a mover Job change back to its Backup. The map keys off the labels the
// mover Job carries: crystalbackup.io/cluster-backup (== the run == the Backup's own name; see
// apiconst.LabelClusterBackup) and crystalbackup.io/namespace (the Backup's namespace). The
// backupPollInterval requeue is the primary progress driver; this watch is a faster secondary
// nudge.
func (r *BackupReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&cbv1.Backup{}).
		Watches(&batchv1.Job{}, handler.EnqueueRequestsFromMapFunc(r.mapJobToBackup)).
		Named("backup").
		Complete(r)
}

// mapJobToBackup maps a mover Job to the Backup that created it, using only the Job's labels: our
// managed-by marker gates it to CrystalBackup mover Jobs, and (cluster-backup, namespace) locate
// the Backup — its name EQUALS the run (apiconst.LabelClusterBackup's value contract). A Job that
// is not one of ours, or is missing either coordinate, maps to nothing.
func (r *BackupReconciler) mapJobToBackup(_ context.Context, obj client.Object) []reconcile.Request {
	labels := obj.GetLabels()
	if labels[apiconst.LabelManagedBy] != apiconst.ManagedByValue {
		return nil
	}
	run := labels[apiconst.LabelClusterBackup]
	namespace := labels[apiconst.LabelNamespace]
	if run == "" || namespace == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: namespace, Name: run}}}
}

// ---------------------------------------------------------------------------
// Pure helpers (no client, no context): selection, naming, argv, phase rollup.
// ---------------------------------------------------------------------------

// exposureLabels are stamped on every object a per-PVC backup creates (the exposure's VS/VSC/temp
// PVC, the mover Job, its creds Secret). LabelManagedBy makes them all reaper-selectable, while
// the crystalbackup.io/* trio (cluster-backup=run, namespace, pvc) both links them to their
// origin and satisfies the crucible leak-check (which flags any residual object carrying a
// crystalbackup.io/* label). They deliberately omit app.kubernetes.io/name=crystal-backup — the
// operator pod's own label, which the crucible's operator-restart test selects on.
func exposureLabels(backup *cbv1.Backup, pvcName string) map[string]string {
	return map[string]string{
		apiconst.LabelManagedBy:     apiconst.ManagedByValue,
		apiconst.LabelClusterBackup: backup.Labels[apiconst.LabelClusterBackup],
		apiconst.LabelNamespace:     backup.Namespace,
		apiconst.LabelPVC:           pvcName,
	}
}

// resticBackupArgs builds the restic argv (after the mover shim's "--") for one PVC-data backup:
// the backup subcommand, the single backup path, the --host, one --tag per identity tag, then the
// tuning flags. Secrets never appear here — the repository, password and S3 creds reach restic
// via env and the mounted Secret (internal/mover).
//
// --pack-size takes a BARE INTEGER of MiB (restic parses it as a uint), not a human-readable size:
// "64" means 64 MiB. Passing "64M" makes restic exit 1 with `invalid argument "64M" for
// "--pack-size" flag`, which failed every real data backup on the crucible.
func resticBackupArgs(id restic.Identity) []string {
	args := []string{"backup", id.Path, "--host", id.Host}
	for _, tag := range id.Tags {
		args = append(args, "--tag", tag)
	}
	return append(args, "--pack-size", "64", "--retry-lock", "5m")
}

// moverNamePrefix is the deterministic per-PVC NamePrefix "<backup>-<pvc>", sanitized to a
// DNS-1123 name and capped (with a hash suffix on overflow) so the derived Job name stays within
// the 63-char label limit. Deterministic in (backup, pvc), so every reconcile — and a restarted
// controller — derives identical exposure/Job/Secret names.
func moverNamePrefix(backupName, pvcName string) string {
	return sanitizeDNSName(backupName+"-"+pvcName, moverNamePrefixMax)
}

// sanitizeDNSName lowercases raw, collapses every run of non-[a-z0-9] into a single '-', trims
// leading/trailing '-', and — if the result exceeds max — truncates it and appends a short,
// deterministic fnv-32a hash of the ORIGINAL input so two distinct long inputs cannot collide.
// The output is a valid DNS-1123 subdomain of length <= max (>= 1).
func sanitizeDNSName(raw string, max int) string {
	var b strings.Builder
	prevHyphen := false
	for _, c := range strings.ToLower(raw) {
		switch {
		case (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9'):
			b.WriteRune(c)
			prevHyphen = false
		default:
			if !prevHyphen {
				b.WriteByte('-')
				prevHyphen = true
			}
		}
	}
	s := strings.Trim(b.String(), "-")
	if s == "" {
		s = "x"
	}
	if len(s) <= max {
		return s
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(raw))
	suffix := fmt.Sprintf("%08x", h.Sum32())
	keep := max - len(suffix) - 1
	if keep < 1 {
		keep = 1
	}
	return strings.TrimRight(s[:keep], "-") + "-" + suffix
}

// matchPVC reports whether a PVC is selected by sel: every matchLabels pair must be present, the
// name must match at least one include glob when include is non-empty, and it must match no
// exclude glob. An empty selector matches every PVC.
func matchPVC(pvc *corev1.PersistentVolumeClaim, sel cbv1.PVCSelector) bool {
	for k, v := range sel.MatchLabels {
		if pvc.Labels[k] != v {
			return false
		}
	}
	if len(sel.Include) > 0 && !matchAnyGlob(pvc.Name, sel.Include) {
		return false
	}
	if matchAnyGlob(pvc.Name, sel.Exclude) {
		return false
	}
	return true
}

// matchAnyGlob reports whether name matches any of the shell globs (path.Match semantics; PVC
// names carry no '/'). A malformed pattern is treated as no-match rather than an error, so a bad
// glob can never crash a reconcile.
func matchAnyGlob(name string, globs []string) bool {
	for _, g := range globs {
		if ok, err := path.Match(g, name); err == nil && ok {
			return true
		}
	}
	return false
}

// firstNonTerminalVolume returns the index of the first volume still in flight (phase not
// Completed/Skipped/Failed), or -1 if every volume is terminal.
func firstNonTerminalVolume(vols []cbv1.VolumeStatus) int {
	for i := range vols {
		switch vols[i].Phase {
		case status.VolumePhaseCompleted, status.VolumePhaseSkipped, status.VolumePhaseFailed:
			continue
		default:
			return i
		}
	}
	return -1
}

// isTerminalBackupPhase reports whether a Backup phase is one of the four terminal aggregates.
func isTerminalBackupPhase(phase string) bool {
	switch status.BackupPhase(phase) {
	case status.BackupPhaseCompleted, status.BackupPhasePartiallyCompleted,
		status.BackupPhasePartiallyFailed, status.BackupPhaseFailed:
		return true
	default:
		return false
	}
}

// setTerminalCondition records the headline Ready condition for a terminal Backup: True for a
// Completed or PartiallyCompleted (skips are a clean outcome, not a failure), False for a
// PartiallyFailed or Failed.
func setTerminalCondition(backup *cbv1.Backup, phase string) {
	switch status.BackupPhase(phase) {
	case status.BackupPhaseCompleted:
		status.SetCondition(&backup.Status.Conditions, ConditionReady, metav1.ConditionTrue, "Completed",
			"all selected volumes were backed up", backup.Generation)
	case status.BackupPhasePartiallyCompleted:
		status.SetCondition(&backup.Status.Conditions, ConditionReady, metav1.ConditionTrue, "PartiallyCompleted",
			"some volumes were skipped (unsupported storage); none failed", backup.Generation)
	case status.BackupPhasePartiallyFailed:
		status.SetCondition(&backup.Status.Conditions, ConditionReady, metav1.ConditionFalse, "PartiallyFailed",
			"at least one volume failed; some data was backed up", backup.Generation)
	default: // BackupPhaseFailed
		status.SetCondition(&backup.Status.Conditions, ConditionReady, metav1.ConditionFalse, "Failed",
			"every volume failed", backup.Generation)
	}
}

// moverFailureReason turns a failed mover outcome into a short, secret-free VolumeStatus.reason. A
// parse error means the termination message was empty (the mover was killed before it could
// report — OOMKilled/SIGKILL); an ok=false result carries the mover's own advisory error; a
// Job-level failure with neither is a generic mover-job failure.
func moverFailureReason(result mover.MoverResult, parseErr error) string {
	switch {
	case parseErr != nil:
		return "MoverCrashed"
	case result.Error != "":
		return shortReason(result.Error)
	default:
		return "MoverJobFailed"
	}
}

// shortReason trims and caps a free-text reason so a status field never carries an unbounded
// blob. Mover-authored errors are advisory and secret-free by contract (internal/mover).
func shortReason(msg string) string {
	const max = 200
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return "MoverJobFailed"
	}
	if len(msg) > max {
		return msg[:max]
	}
	return msg
}
