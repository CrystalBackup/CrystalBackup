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
	"slices"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"k8s.io/client-go/tools/events"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
	"github.com/CrystalBackup/CrystalBackup/internal/client/secrets"
	"github.com/CrystalBackup/CrystalBackup/internal/repo/queue"
	"github.com/CrystalBackup/CrystalBackup/internal/restic"
	"github.com/CrystalBackup/CrystalBackup/internal/rexposer"
	"github.com/CrystalBackup/CrystalBackup/internal/status"
)

// FilteredSnapshotLister is the mediated-resolution seam (adr/0016 §3): a tag-filtered
// repository listing under a caller-chosen Job name. *JobSnapshotLister implements it in
// production; the envtest suite injects a stub (no kubelet ⇒ no listing Jobs).
type FilteredSnapshotLister interface {
	ListFiltered(ctx context.Context, repo *cbv1.BackupRepository, jobName string, filterTags ...string) ([]restic.Snapshot, error)
}

// RestoreReconciler reconciles a Restore: the SELF-SERVICE restore of one namespace's own
// data (R2/R14). Its whole tenancy stance is structural: the source is a Backup in the
// Restore's OWN namespace (no locationRef, no target-namespace field), and when that Backup
// is cluster-origin the repository is consulted only through a listing whose tag filter
// namespace=<metadata.namespace> is derived server-side — no spec field can widen it. The
// per-volume execution is the shared restoreEngine; this controller owns source resolution,
// the R23 confirmation gate (re-checked here at execution, VAP being only the fast gate),
// plan building, and the single status write per reconcile.
type RestoreReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Engine is the shared restore execution engine (exposure, mover Jobs, handover).
	Engine *restoreEngine
	// Lister resolves cluster-origin sources against the repository (mediated, filtered).
	Lister FilteredSnapshotLister
	// OperatorNamespace is where every operator-side restore object lives.
	OperatorNamespace string
	Recorder          events.EventRecorder
}

// NewRestoreReconciler wires the reconciler and its engine from the shared primitives.
func NewRestoreReconciler(
	c client.Client,
	scheme *runtime.Scheme,
	secretsReader *secrets.ByNameReader,
	targets *rexposer.TargetExposer,
	lister FilteredSnapshotLister,
	operatorNamespace, moverImage string,
	recorder events.EventRecorder,
	q *queue.Manager,
) *RestoreReconciler {
	return &RestoreReconciler{
		Client:            c,
		Scheme:            scheme,
		Engine:            newRestoreEngine(c, secretsReader, targets, operatorNamespace, moverImage, q),
		Lister:            lister,
		OperatorNamespace: operatorNamespace,
		Recorder:          recorder,
	}
}

// +kubebuilder:rbac:groups=crystalbackup.io,resources=restores,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=crystalbackup.io,resources=restores/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=crystalbackup.io,resources=restores/finalizers,verbs=update
// +kubebuilder:rbac:groups=crystalbackup.io,resources=backups;clusterbackuplocations;backuplocations;backuprepositories,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=persistentvolumes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=storage.k8s.io,resources=storageclasses;volumeattachments,verbs=get;list;watch

// Reconcile drives one Restore to a terminal outcome: resolve the source Backup, hold at
// AwaitingConfirmation until spec.confirmation equals the target namespace (R23, re-checked
// at execution), resolve snapshots (mediated for cluster-origin), then drive every planned
// volume through the engine and write status ONCE per pass. Teardown of the operator-side
// objects happens only AFTER the terminal status write persists — the live Jobs are the
// restore's only durable per-volume state (adr/0016).
func (r *RestoreReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var restore cbv1.Restore
	if err := r.Get(ctx, req.NamespacedName, &restore); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("get Restore %s/%s: %w", req.Namespace, req.Name, err)
	}

	if !restore.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, &restore)
	}

	if controllerutil.AddFinalizer(&restore, apiconst.FinalizerRestore) {
		if err := r.Update(ctx, &restore); err != nil {
			return ctrl.Result{}, fmt.Errorf("add finalizer to Restore %s/%s: %w", restore.Namespace, restore.Name, err)
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Terminal restores are done for good; a stray watch event must not re-execute them.
	if isTerminalRestorePhase(restore.Status.Phase) {
		return ctrl.Result{}, nil
	}

	// (1) Resolve the source Backup IN THIS NAMESPACE (by name, or by time/origin).
	source, ok, err := r.resolveSource(ctx, &restore)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !ok {
		return r.gate(ctx, &restore, "SourceBackupNotFound",
			"no Backup in this namespace matches spec.source (it may not be projected yet)")
	}

	// (2) R23 confirmation, re-checked at execution (defense in depth beyond the VAP): every
	// Recreate/Overwrite requires spec.confirmation == the target namespace — this namespace.
	if restore.Spec.Confirmation != restore.Namespace {
		return r.awaitConfirmation(ctx, &restore)
	}
	if !meta_HasConditionTrue(restore.Status.Conditions, ConditionConfirmed) {
		status.SetCondition(&restore.Status.Conditions, ConditionConfirmed, metav1.ConditionTrue,
			"ConfirmationAccepted", "spec.confirmation matches the target namespace", restore.Generation)
		r.Recorder.Eventf(&restore, nil, corev1.EventTypeNormal, "ConfirmationAccepted", "Confirm",
			"destructive restore confirmed for namespace %s", restore.Namespace)
	}

	// (3) Resolve the repository behind the source Backup's location.
	rc, plans, res, err := r.prepare(ctx, &restore, source)
	if err != nil || plans == nil {
		return res, err
	}

	// (4) Drive every planned volume one step; collect outcomes.
	settled, completed, failedCount := 0, 0, 0
	var restoredBytes int64
	var failures []string
	for i := range plans {
		outcome, err := r.Engine.adviseVolume(ctx, rc, &plans[i])
		if err != nil {
			return ctrl.Result{}, err
		}
		if !outcome.settled {
			continue
		}
		settled++
		if outcome.failed {
			failedCount++
			failures = append(failures, plans[i].pvc+": "+outcome.reason)
			continue
		}
		completed++
		restoredBytes += outcome.restoredBytes
	}

	// (5) Single status write; terminal only when EVERY volume settled.
	if settled < len(plans) {
		restore.Status.Phase = string(status.RestorePhaseRunning)
		status.SetCondition(&restore.Status.Conditions, ConditionReady, metav1.ConditionFalse, "InProgress",
			fmt.Sprintf("restoring: %d/%d volumes settled", settled, len(plans)), restore.Generation)
		if err := r.Status().Update(ctx, &restore); err != nil {
			return ctrl.Result{}, fmt.Errorf("update status for Restore %s/%s: %w", restore.Namespace, restore.Name, err)
		}
		return ctrl.Result{RequeueAfter: restorePollInterval}, nil
	}

	phase := status.RollUpRestoreOutcomes(completed, failedCount)
	restore.Status.Phase = string(phase)
	restore.Status.RestoredVolumes = int32(completed)
	restore.Status.RestoredBytes = restoredBytes
	setRestoreTerminalCondition(&restore.Status.Conditions, phase, failures, restore.Generation)
	if err := r.Status().Update(ctx, &restore); err != nil {
		// Status not persisted: return WITHOUT tearing down, so the Jobs (the durable
		// per-volume state) survive and the next pass re-derives the same terminal result.
		return ctrl.Result{}, fmt.Errorf("update status for Restore %s/%s: %w", restore.Namespace, restore.Name, err)
	}

	// (6) The terminal result is durable: reclaim the operator-side residue.
	r.Engine.teardownAll(ctx, rc, plans)
	r.Engine.forgetResolution(restore.UID)
	if phase == status.RestorePhaseCompleted {
		r.Recorder.Eventf(&restore, nil, corev1.EventTypeNormal, "RestoreCompleted", "Restore",
			"restored %d volume(s), %d bytes", completed, restoredBytes)
	} else {
		r.Recorder.Eventf(&restore, nil, corev1.EventTypeWarning, "RestoreFailed", "Restore",
			"restore ended %s: %s", string(phase), clampMessage(joinFailures(failures)))
	}
	return ctrl.Result{}, nil
}

// prepare resolves the repository coordinates and builds the volume plans. A nil plans
// return with a nil error means a gate was written (res carries the requeue).
func (r *RestoreReconciler) prepare(ctx context.Context, restore *cbv1.Restore, source *cbv1.Backup) (*restoreExecContext, []restoreVolumePlan, ctrl.Result, error) {
	if source.Spec.LocationRef.Kind != string(kindClusterBackupLocationRef) {
		// The namespace plane's own repositories (BackupLocation + user key) land with M5;
		// until then a user-plane Backup cannot be executed or restored.
		res, err := r.gate(ctx, restore, "NamespacePlaneRestoreUnavailable",
			"restoring from a namespace-plane BackupLocation lands with the namespace plane (M5); "+
				"this Backup references location kind BackupLocation")
		return nil, nil, res, err
	}

	var loc cbv1.ClusterBackupLocation
	if err := r.Get(ctx, client.ObjectKey{Name: source.Spec.LocationRef.Name}, &loc); err != nil {
		if apierrors.IsNotFound(err) {
			res, gerr := r.gate(ctx, restore, "LocationNotFound",
				fmt.Sprintf("ClusterBackupLocation %q not found", source.Spec.LocationRef.Name))
			return nil, nil, res, gerr
		}
		return nil, nil, ctrl.Result{}, fmt.Errorf("get ClusterBackupLocation %s: %w", source.Spec.LocationRef.Name, err)
	}
	var repo cbv1.BackupRepository
	if err := r.Get(ctx, client.ObjectKey{Name: loc.Name}, &repo); err != nil || !repo.Status.Initialized {
		if err != nil && !apierrors.IsNotFound(err) {
			return nil, nil, ctrl.Result{}, fmt.Errorf("get BackupRepository %s: %w", loc.Name, err)
		}
		res, gerr := r.gate(ctx, restore, "RepositoryNotReady",
			fmt.Sprintf("BackupRepository %q is not initialized", loc.Name))
		return nil, nil, res, gerr
	}

	dek, reason, message, ok := ensurePlatformDEKFor(ctx, r.Client, r.Engine.Secrets, r.OperatorNamespace, &loc)
	if !ok {
		res, gerr := r.gate(ctx, restore, reason, message)
		return nil, nil, res, gerr
	}

	rc := &restoreExecContext{
		ownerName:       restore.Name,
		ownerLabelKey:   apiconst.LabelRestore,
		targetNamespace: restore.Namespace,
		deleteExtras:    restore.Spec.Mode == cbv1.RestoreModeRecreate,
		restoredFromRun: source.Name,
		repoName:        loc.Name,
		repoURL:         repo.Status.RepositoryURL,
		dek:             dek,
		s3CredsSecret:   loc.Spec.S3.CredentialsSecretRef.Name,
	}

	// Mediated resolution (I1, the R2/R14 cornerstone): the snapshots this restore may read
	// are ONLY those the repository returns under the server-derived filter
	// namespace=<metadata.namespace> AND run=<source>. The Backup's own status.volumes IDs
	// are deliberately not trusted for cluster-origin data — the projection is
	// operator-authored and user-read-only (I7), but the execution boundary re-derives
	// anyway (adr/0010 "controllers re-derive at execution").
	byPVC, cached := r.Engine.cachedResolution(restore.UID, source.Name)
	if !cached {
		snaps, err := r.Lister.ListFiltered(ctx, &repo, restoreResolveJobName(restore.Name),
			restic.Tag(restic.TagKeyNamespace, restore.Namespace),
			restic.Tag(restic.TagKeyRun, source.Name))
		if err != nil {
			res, gerr := r.gate(ctx, restore, "SnapshotResolutionFailed",
				fmt.Sprintf("mediated snapshot resolution failed: %v", err))
			return nil, nil, res, gerr
		}
		byPVC = dataSnapshotsByPVC(snaps)
		r.Engine.storeResolution(restore.UID, source.Name, byPVC)
	}

	plans, missing := r.buildPlans(ctx, restore, source, byPVC)
	if len(missing) > 0 {
		// A selected PVC with no snapshot under the mediated filter cannot be restored —
		// fail CLOSED (never fall back to an unmediated identifier).
		res, gerr := r.gate(ctx, restore, "SnapshotNotFound",
			fmt.Sprintf("no snapshot under the namespace filter for PVC(s): %s", clampMessage(joinFailures(missing))))
		return nil, nil, res, gerr
	}
	if plans == nil {
		// An empty selection is a VALID, immediately-terminal outcome (02-api: a present-but-
		// empty list restores nothing) — non-nil so the caller's nil-means-gated check never
		// mistakes it for a blocker and strands the CR without a terminal status.
		plans = []restoreVolumePlan{}
	}
	return rc, plans, ctrl.Result{}, nil
}

// buildPlans intersects the restore's selection with the source Backup's restorable volumes
// and attaches the mediated snapshot IDs + sizing. missing lists selected PVCs absent from
// the mediated resolution (fail closed).
func (r *RestoreReconciler) buildPlans(ctx context.Context, restore *cbv1.Restore, source *cbv1.Backup, byPVC map[string]restic.Snapshot) (plans []restoreVolumePlan, missing []string) {
	sourcePVCs := restorableVolumes(source)
	selected := planVolumes(restore.Spec.Volumes, sourcePVCs, restore.Spec.Volumes != nil)

	slices.Sort(sourcePVCs)
	for _, pvc := range sourcePVCs {
		item, ok := selected[pvc]
		if !ok {
			continue
		}
		snap, found := byPVC[pvc]
		if !found {
			missing = append(missing, pvc)
			continue
		}
		plan := restoreVolumePlan{
			pvc:          pvc,
			snapshotID:   snap.ID,
			snapshotPath: "/data/" + restore.Namespace + "/" + pvc,
		}
		if item != nil {
			plan.include = item.Include
			plan.exclude = item.Exclude
			plan.targetPath = item.TargetPath
		}
		r.sizePlan(ctx, &plan, restore.Namespace, source, &snap)
		plans = append(plans, plan)
	}
	return plans, missing
}

// sizePlan fills a plan's provisioning inputs (used only when the target PVC is absent —
// the transplant path): the live target claim's spec when it exists, else the snapshot's
// PVC-meta tags, else the documented fallback (the source's recorded logical size rounded
// up with headroom, the cluster default class, RWO).
func (r *RestoreReconciler) sizePlan(ctx context.Context, plan *restoreVolumePlan, namespace string, source *cbv1.Backup, snap *restic.Snapshot) {
	// A live target claim overrides everything: restoring INTO it never resizes it.
	var target corev1.PersistentVolumeClaim
	if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: plan.pvc}, &target); err == nil {
		plan.capacity = target.Spec.Resources.Requests[corev1.ResourceStorage]
		if target.Spec.StorageClassName != nil {
			plan.storageClass = *target.Spec.StorageClassName
		}
		plan.accessModes = target.Spec.AccessModes
		return
	}

	meta := restic.ParsePVCMeta(snap.Tags)
	if meta.CapacityBytes > 0 {
		plan.capacity = *resource.NewQuantity(meta.CapacityBytes, resource.BinarySI)
	} else {
		plan.capacity = fallbackRestoreCapacity(sourceVolumeSize(source, plan.pvc))
	}
	plan.storageClass = meta.StorageClass
	for _, m := range meta.AccessModes {
		plan.accessModes = append(plan.accessModes, corev1.PersistentVolumeAccessMode(m))
	}
}

// resolveSource finds the source Backup in the Restore's namespace: by name
// (spec.source.backup), or the newest successful one at/before spec.source.time (with the
// optional origin filter). ok=false means "not resolvable yet" — a gate, never a hard
// failure (discovery may still be projecting).
func (r *RestoreReconciler) resolveSource(ctx context.Context, restore *cbv1.Restore) (*cbv1.Backup, bool, error) {
	if name := restore.Spec.Source.Backup; name != "" {
		var b cbv1.Backup
		if err := r.Get(ctx, client.ObjectKey{Namespace: restore.Namespace, Name: name}, &b); err != nil {
			if apierrors.IsNotFound(err) {
				return nil, false, nil
			}
			return nil, false, fmt.Errorf("get source Backup %s/%s: %w", restore.Namespace, name, err)
		}
		return &b, true, nil
	}

	var cutoff *time.Time
	if t := restore.Spec.Source.Time; t != "" && t != "latest" {
		parsed, err := time.Parse(time.RFC3339, t)
		if err != nil {
			return nil, false, nil // CRD CEL bounds the shape; an unparseable instant resolves nothing.
		}
		cutoff = &parsed
	}

	var backups cbv1.BackupList
	if err := r.List(ctx, &backups, client.InNamespace(restore.Namespace)); err != nil {
		return nil, false, fmt.Errorf("list Backups in %s: %w", restore.Namespace, err)
	}
	var best *cbv1.Backup
	for i := range backups.Items {
		b := &backups.Items[i]
		if o := restore.Spec.Source.Origin; o != "" && b.Labels[apiconst.LabelOrigin] != o {
			continue
		}
		if !backupSucceeded(b.Status.Phase) || b.Status.BackupTime == nil {
			continue
		}
		if cutoff != nil && b.Status.BackupTime.After(*cutoff) {
			continue
		}
		if best == nil || b.Status.BackupTime.After(best.Status.BackupTime.Time) {
			best = b
		}
	}
	return best, best != nil, nil
}

// awaitConfirmation parks the Restore in AwaitingConfirmation (R23): the user edits
// spec.confirmation to the target namespace to proceed. The spec-edit watch nudges the next
// reconcile; the requeue is a robustness backstop.
func (r *RestoreReconciler) awaitConfirmation(ctx context.Context, restore *cbv1.Restore) (ctrl.Result, error) {
	first := restore.Status.Phase != string(status.RestorePhaseAwaitingConfirmation)
	restore.Status.Phase = string(status.RestorePhaseAwaitingConfirmation)
	status.SetCondition(&restore.Status.Conditions, ConditionReady, metav1.ConditionFalse, "ConfirmationRequired",
		fmt.Sprintf("a %s restore modifies existing data: set spec.confirmation to %q to proceed",
			restore.Spec.Mode, restore.Namespace), restore.Generation)
	if err := r.Status().Update(ctx, restore); err != nil {
		return ctrl.Result{}, fmt.Errorf("update status for Restore %s/%s: %w", restore.Namespace, restore.Name, err)
	}
	if first {
		r.Recorder.Eventf(restore, nil, corev1.EventTypeNormal, "ConfirmationRequired", "Confirm",
			"restore parked: set spec.confirmation to %q to proceed (R23)", restore.Namespace)
	}
	return ctrl.Result{RequeueAfter: shortRequeueInterval}, nil
}

// gate records a non-terminal blocker on the Ready condition, keeps the Restore Pending,
// and requeues on the fixable-fault cadence — the same shape as the Backup controller.
func (r *RestoreReconciler) gate(ctx context.Context, restore *cbv1.Restore, reason, message string) (ctrl.Result, error) {
	restore.Status.Phase = string(status.RestorePhasePending)
	status.SetCondition(&restore.Status.Conditions, ConditionReady, metav1.ConditionFalse, reason, message, restore.Generation)
	if err := r.Status().Update(ctx, restore); err != nil {
		return ctrl.Result{}, fmt.Errorf("update status for Restore %s/%s: %w", restore.Namespace, restore.Name, err)
	}
	return ctrl.Result{RequeueAfter: shortRequeueInterval}, nil
}

// finalize tears down anything the restore left live — mover Jobs, staging claims, twin
// PVs, a mid-handover transplant volume — before dropping the finalizer. Ownership is
// re-derived from the deterministic names + labels (the source Backup may be long gone), so
// the sweep works from the live Jobs when plans cannot be rebuilt.
func (r *RestoreReconciler) finalize(ctx context.Context, restore *cbv1.Restore) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(restore, apiconst.FinalizerRestore) {
		return ctrl.Result{}, nil
	}
	r.Engine.forgetResolution(restore.UID)

	rc := &restoreExecContext{
		ownerName:       restore.Name,
		ownerLabelKey:   apiconst.LabelRestore,
		targetNamespace: restore.Namespace,
	}
	teardownRestoreResidue(ctx, r.Engine, rc)

	r.Recorder.Eventf(restore, nil, corev1.EventTypeNormal, "Finalizing", "Finalize",
		"tearing down restore movers and target exposures; no repository data is erased")
	controllerutil.RemoveFinalizer(restore, apiconst.FinalizerRestore)
	if err := r.Update(ctx, restore); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("remove finalizer from Restore %s/%s: %w", restore.Namespace, restore.Name, err)
	}
	return ctrl.Result{}, nil
}

// SetupWithManager registers the reconciler: the Restore watch plus a label-mapped Job
// watch (mover Jobs live in the operator namespace and cannot be owned by a namespaced
// Restore — same rationale as the Backup controller).
func (r *RestoreReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&cbv1.Restore{}).
		Watches(&batchv1.Job{}, handler.EnqueueRequestsFromMapFunc(r.mapJobToRestore)).
		Named("restore").
		Complete(r)
}

// mapJobToRestore maps a restore mover Job back to its owning Restore via the owner labels.
func (r *RestoreReconciler) mapJobToRestore(_ context.Context, obj client.Object) []reconcile.Request {
	labels := obj.GetLabels()
	if labels[apiconst.LabelManagedBy] != apiconst.ManagedByValue {
		return nil
	}
	name := labels[apiconst.LabelRestore]
	namespace := labels[apiconst.LabelNamespace]
	if name == "" || namespace == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: namespace, Name: name}}}
}

// ---------------------------------------------------------------------------
// Shared restore helpers (used by both restore controllers).
// ---------------------------------------------------------------------------

// kindClusterBackupLocationRef is the LocationReference.Kind value naming the cluster plane.
const kindClusterBackupLocationRef = "ClusterBackupLocation"

// ConditionConfirmed records the accepted R23 confirmation (audit trail: the
// ConfirmationAccepted event fires once, on the transition).
const ConditionConfirmed = "Confirmed"

// restoreResolveJobName names the mediated-resolution listing Job for one restore —
// distinct from every discovery inventory name ("<repo>-discovery") and deterministic per
// owner so a restarted resolution re-adopts.
func restoreResolveJobName(ownerName string) string {
	return sanitizeDNSName(ownerName, restoreNamePrefixMax) + "-resolve"
}

// restorableVolumes lists the PVCs a source Backup can restore: volumes that completed
// with a recorded snapshot.
func restorableVolumes(source *cbv1.Backup) []string {
	out := make([]string, 0, len(source.Status.Volumes))
	for i := range source.Status.Volumes {
		v := &source.Status.Volumes[i]
		if v.Phase == status.VolumePhaseCompleted && v.SnapshotID != "" {
			out = append(out, v.Pvc)
		}
	}
	return out
}

// dataSnapshotsByPVC indexes a (filtered) snapshot listing's kind=data snapshots by their
// pvc= tag, newest first winning (a run normally holds exactly one per PVC; on duplicates
// the most recent is authoritative).
func dataSnapshotsByPVC(snaps []restic.Snapshot) map[string]restic.Snapshot {
	byPVC := make(map[string]restic.Snapshot, len(snaps))
	for _, s := range snaps {
		if kind, _ := restic.TagValue(s.Tags, restic.TagKeyKind); kind != restic.KindData {
			continue
		}
		pvc, ok := restic.TagValue(s.Tags, restic.TagKeyPVC)
		if !ok || pvc == "" {
			continue
		}
		if prev, exists := byPVC[pvc]; !exists || s.Time.After(prev.Time) {
			byPVC[pvc] = s
		}
	}
	return byPVC
}

// sourceVolumeSize returns the recorded logical size of one source volume (0 if unknown).
func sourceVolumeSize(source *cbv1.Backup, pvc string) int64 {
	for i := range source.Status.Volumes {
		if source.Status.Volumes[i].Pvc == pvc {
			return source.Status.Volumes[i].SizeBytes
		}
	}
	return 0
}

// fallbackRestoreCapacity sizes a transplant provisioning when no PVC-meta tag and no live
// claim exist (pre-0.2 snapshots): the logical data size plus 20% headroom, rounded up to
// the next GiB, minimum 1Gi — documented in adr/0016 and always overridable by pre-creating
// the target PVC.
func fallbackRestoreCapacity(dataSizeBytes int64) resource.Quantity {
	const gib = int64(1) << 30
	padded := dataSizeBytes + dataSizeBytes/5
	gibs := max((padded+gib-1)/gib, 1)
	return *resource.NewQuantity(gibs*gib, resource.BinarySI)
}

// teardownRestoreResidue sweeps EVERY operator-side object an owner's restore created, by
// the owner labels — usable when the plans cannot be rebuilt (finalize: the source Backup
// may be gone). Jobs and Secrets are found by label; exposures are cleaned per found PVC
// label (kind-agnostic double cleanup); a labeled transplant PV with no delivered claim is
// reclaimed.
func teardownRestoreResidue(ctx context.Context, e *restoreEngine, rc *restoreExecContext) {
	sel := client.MatchingLabels{
		apiconst.LabelManagedBy: apiconst.ManagedByValue,
		rc.ownerLabelKey:        rc.ownerName,
	}
	var jobs batchv1.JobList
	if err := e.List(ctx, &jobs, client.InNamespace(e.OperatorNamespace), sel); err == nil {
		for i := range jobs.Items {
			if pvc := jobs.Items[i].Labels[apiconst.LabelPVC]; pvc != "" {
				e.teardownVolume(ctx, rc, &restoreVolumePlan{pvc: pvc})
			}
		}
	}
	// Staging claims whose Job is already gone (crash between job delete and cleanup).
	var pvcs corev1.PersistentVolumeClaimList
	if err := e.List(ctx, &pvcs, client.InNamespace(e.OperatorNamespace), sel); err == nil {
		for i := range pvcs.Items {
			if pvc := pvcs.Items[i].Labels[apiconst.LabelPVC]; pvc != "" {
				e.teardownVolume(ctx, rc, &restoreVolumePlan{pvc: pvc})
			}
		}
	}
	// Twin/transplant PVs whose staging claim is already gone.
	var pvs corev1.PersistentVolumeList
	if err := e.List(ctx, &pvs, sel); err == nil {
		for i := range pvs.Items {
			if pvc := pvs.Items[i].Labels[apiconst.LabelPVC]; pvc != "" {
				e.teardownVolume(ctx, rc, &restoreVolumePlan{pvc: pvc})
			}
		}
	}
}

// setRestoreTerminalCondition records the headline Ready condition of a terminal restore.
func setRestoreTerminalCondition(conds *[]metav1.Condition, phase status.RestorePhase, failures []string, generation int64) {
	switch phase {
	case status.RestorePhaseCompleted:
		status.SetCondition(conds, ConditionReady, metav1.ConditionTrue, "Completed",
			"all selected volumes were restored", generation)
	default:
		status.SetCondition(conds, ConditionReady, metav1.ConditionFalse, string(phase),
			clampMessage(joinFailures(failures)), generation)
	}
}

// isTerminalRestorePhase reports whether a Restore/ClusterRestore phase is terminal.
func isTerminalRestorePhase(phase string) bool {
	switch status.RestorePhase(phase) {
	case status.RestorePhaseCompleted, status.RestorePhasePartiallyFailed, status.RestorePhaseFailed:
		return true
	default:
		return false
	}
}

// joinFailures folds per-volume failure notes into one advisory line.
func joinFailures(failures []string) string {
	if len(failures) == 0 {
		return "no volumes restored"
	}
	var out strings.Builder
	out.WriteString(failures[0])
	for _, f := range failures[1:] {
		out.WriteString("; " + f)
	}
	return out.String()
}

// meta_HasConditionTrue reports whether the condition of the given type is present & True.
func meta_HasConditionTrue(conds []metav1.Condition, condType string) bool {
	c := status.FindCondition(conds, condType)
	return c != nil && c.Status == metav1.ConditionTrue
}

// ensurePlatformDEKFor is the shared DEK resolution both restore controllers use (the
// read-mostly twin of the Backup controller's ensureDEK, package-scoped so ClusterRestore
// reuses it without a reconciler receiver).
func ensurePlatformDEKFor(ctx context.Context, c client.Client, secretsReader *secrets.ByNameReader,
	operatorNamespace string, loc *cbv1.ClusterBackupLocation,
) (dek, reason, message string, ok bool) {
	return resolvePlatformDEKCommon(ctx, c, secretsReader, operatorNamespace, loc)
}
