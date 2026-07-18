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
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
	"github.com/CrystalBackup/CrystalBackup/internal/client/secrets"
	"github.com/CrystalBackup/CrystalBackup/internal/repo/queue"
	"github.com/CrystalBackup/CrystalBackup/internal/restic"
	"github.com/CrystalBackup/CrystalBackup/internal/rexposer"
	"github.com/CrystalBackup/CrystalBackup/internal/status"
)

// ClusterRestoreReconciler reconciles a ClusterRestore: the ADMIN restore of any namespace
// from a REPO COORDINATE (location + origin namespace + run/time) — the disaster-recovery
// path that works when the source namespace, its Backup projections, and even the whole
// cluster's CRs are gone (R14, R26). Everything is resolved from the repository itself
// through the filtered listing (never from in-cluster projections), the target namespace
// can be created, storage classes are remapped, and the per-volume execution is the shared
// restoreEngine. The admin's identity IS the authorization (R14); R23 still demands the
// typed confirmation, re-checked here at execution.
type ClusterRestoreReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Engine is the shared restore execution engine.
	Engine *restoreEngine
	// Lister resolves the repo coordinate against the repository (filtered listing).
	Lister FilteredSnapshotLister
	// OperatorNamespace is where every operator-side restore object lives.
	OperatorNamespace string
	Recorder          events.EventRecorder
}

// NewClusterRestoreReconciler wires the reconciler and its engine.
func NewClusterRestoreReconciler(
	c client.Client,
	scheme *runtime.Scheme,
	secretsReader *secrets.ByNameReader,
	targets *rexposer.TargetExposer,
	lister FilteredSnapshotLister,
	operatorNamespace, moverImage string,
	recorder events.EventRecorder,
	q *queue.Manager,
) *ClusterRestoreReconciler {
	return &ClusterRestoreReconciler{
		Client:            c,
		Scheme:            scheme,
		Engine:            newRestoreEngine(c, secretsReader, targets, operatorNamespace, moverImage, q),
		Lister:            lister,
		OperatorNamespace: operatorNamespace,
		Recorder:          recorder,
	}
}

// +kubebuilder:rbac:groups=crystalbackup.io,resources=clusterrestores,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=crystalbackup.io,resources=clusterrestores/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=crystalbackup.io,resources=clusterrestores/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch;create

// Reconcile drives one ClusterRestore: confirmation (R23), target-namespace ensure, repo-
// coordinate resolution (one filtered listing serves run selection AND per-PVC snapshots),
// then the shared engine drive with the same single-status-writer / teardown-after-terminal
// discipline as the namespaced Restore.
func (r *ClusterRestoreReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var cr cbv1.ClusterRestore
	if err := r.Get(ctx, req.NamespacedName, &cr); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("get ClusterRestore %s: %w", req.Name, err)
	}

	if !cr.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, &cr)
	}
	if controllerutil.AddFinalizer(&cr, apiconst.FinalizerClusterRestore) {
		if err := r.Update(ctx, &cr); err != nil {
			return ctrl.Result{}, fmt.Errorf("add finalizer to ClusterRestore %s: %w", cr.Name, err)
		}
		return ctrl.Result{Requeue: true}, nil
	}
	if isTerminalRestorePhase(cr.Status.Phase) {
		return ctrl.Result{}, nil
	}

	// (1) R23 confirmation — the conservative superset, re-checked at execution: every
	// Recreate/Overwrite requires spec.confirmation == the TARGET namespace.
	if cr.Spec.Confirmation != cr.Spec.Target.Namespace {
		return r.awaitConfirmation(ctx, &cr)
	}
	if !meta_HasConditionTrue(cr.Status.Conditions, ConditionConfirmed) {
		status.SetCondition(&cr.Status.Conditions, ConditionConfirmed, metav1.ConditionTrue,
			"ConfirmationAccepted", "spec.confirmation matches the target namespace", cr.Generation)
		r.Recorder.Eventf(&cr, nil, corev1.EventTypeNormal, "ConfirmationAccepted", "Confirm",
			"destructive restore confirmed for target namespace %s", cr.Spec.Target.Namespace)
	}

	// (2) Ensure the target namespace exists (creating it is the DR case's whole point).
	if res, gated, err := r.ensureTargetNamespace(ctx, &cr); gated || err != nil {
		return res, err
	}

	// (3) Resolve location, repository, DEK, and the repo coordinate.
	rc, plans, res, err := r.prepare(ctx, &cr)
	if err != nil || plans == nil {
		return res, err
	}

	// (4) Drive the volumes; (5) single status write; (6) teardown after terminal.
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

	if settled < len(plans) {
		cr.Status.Phase = string(status.RestorePhaseRunning)
		status.SetCondition(&cr.Status.Conditions, ConditionReady, metav1.ConditionFalse, "InProgress",
			fmt.Sprintf("restoring: %d/%d volumes settled", settled, len(plans)), cr.Generation)
		if err := r.Status().Update(ctx, &cr); err != nil {
			return ctrl.Result{}, fmt.Errorf("update status for ClusterRestore %s: %w", cr.Name, err)
		}
		return ctrl.Result{RequeueAfter: restorePollInterval}, nil
	}

	phase := status.RollUpRestoreOutcomes(completed, failedCount)
	cr.Status.Phase = string(phase)
	cr.Status.RestoredVolumes = int32(completed)
	cr.Status.RestoredBytes = restoredBytes
	setRestoreTerminalCondition(&cr.Status.Conditions, phase, failures, cr.Generation)
	if err := r.Status().Update(ctx, &cr); err != nil {
		return ctrl.Result{}, fmt.Errorf("update status for ClusterRestore %s: %w", cr.Name, err)
	}

	r.Engine.teardownAll(ctx, rc, plans)
	r.Engine.forgetResolution(cr.UID)
	if phase == status.RestorePhaseCompleted {
		r.Recorder.Eventf(&cr, nil, corev1.EventTypeNormal, "RestoreCompleted", "Restore",
			"restored %d volume(s), %d bytes into namespace %s", completed, restoredBytes, cr.Spec.Target.Namespace)
	} else {
		r.Recorder.Eventf(&cr, nil, corev1.EventTypeWarning, "RestoreFailed", "Restore",
			"restore ended %s: %s", string(phase), clampMessage(joinFailures(failures)))
	}
	return ctrl.Result{}, nil
}

// ensureTargetNamespace creates (createNamespace) or requires the target namespace.
// gated=true means a gate condition was written and res carries its requeue.
func (r *ClusterRestoreReconciler) ensureTargetNamespace(ctx context.Context, cr *cbv1.ClusterRestore) (res ctrl.Result, gated bool, err error) {
	var ns corev1.Namespace
	getErr := r.Get(ctx, client.ObjectKey{Name: cr.Spec.Target.Namespace}, &ns)
	if getErr == nil {
		return ctrl.Result{}, false, nil
	}
	if !apierrors.IsNotFound(getErr) {
		return ctrl.Result{}, false, fmt.Errorf("get target namespace %s: %w", cr.Spec.Target.Namespace, getErr)
	}
	if !cr.Spec.Target.CreateNamespace {
		res, err = r.gate(ctx, cr, "TargetNamespaceNotFound",
			fmt.Sprintf("target namespace %q does not exist and target.createNamespace is false", cr.Spec.Target.Namespace))
		return res, true, err
	}
	created := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: cr.Spec.Target.Namespace}}
	if err := r.Create(ctx, created); err != nil && !apierrors.IsAlreadyExists(err) {
		return ctrl.Result{}, false, fmt.Errorf("create target namespace %s: %w", cr.Spec.Target.Namespace, err)
	}
	r.Recorder.Eventf(cr, nil, corev1.EventTypeNormal, "TargetNamespaceCreated", "EnsureNamespace",
		"created target namespace %s", cr.Spec.Target.Namespace)
	return ctrl.Result{}, false, nil
}

// prepare resolves the repository and the repo coordinate into volume plans. One filtered
// listing (namespace=<source.namespace>) serves BOTH the run selection (named run, latest,
// or time cutoff) and the per-PVC snapshot mapping — the repository, not any in-cluster
// object, is the source of truth (R26).
func (r *ClusterRestoreReconciler) prepare(ctx context.Context, cr *cbv1.ClusterRestore) (*restoreExecContext, []restoreVolumePlan, ctrl.Result, error) {
	var loc cbv1.ClusterBackupLocation
	if err := r.Get(ctx, client.ObjectKey{Name: cr.Spec.Source.LocationRef.Name}, &loc); err != nil {
		if apierrors.IsNotFound(err) {
			res, gerr := r.gate(ctx, cr, "LocationNotFound",
				fmt.Sprintf("ClusterBackupLocation %q not found", cr.Spec.Source.LocationRef.Name))
			return nil, nil, res, gerr
		}
		return nil, nil, ctrl.Result{}, fmt.Errorf("get ClusterBackupLocation %s: %w", cr.Spec.Source.LocationRef.Name, err)
	}
	var repo cbv1.BackupRepository
	if err := r.Get(ctx, client.ObjectKey{Name: loc.Name}, &repo); err != nil || !repo.Status.Initialized {
		if err != nil && !apierrors.IsNotFound(err) {
			return nil, nil, ctrl.Result{}, fmt.Errorf("get BackupRepository %s: %w", loc.Name, err)
		}
		res, gerr := r.gate(ctx, cr, "RepositoryNotReady",
			fmt.Sprintf("BackupRepository %q is not initialized", loc.Name))
		return nil, nil, res, gerr
	}
	dek, reason, message, ok := ensurePlatformDEKFor(ctx, r.Client, r.Engine.Secrets, r.OperatorNamespace, &loc)
	if !ok {
		res, gerr := r.gate(ctx, cr, reason, message)
		return nil, nil, res, gerr
	}

	// One filtered listing for the whole source namespace, cached per (owner, coordinate).
	coordinate := cr.Spec.Source.Namespace + "/" + cr.Spec.Source.Backup + "/" + cr.Spec.Source.Time
	byPVC, cached := r.Engine.cachedResolution(cr.UID, coordinate)
	if !cached {
		snaps, err := r.Lister.ListFiltered(ctx, &repo, restoreResolveJobName(cr.Name),
			restic.Tag(restic.TagKeyNamespace, cr.Spec.Source.Namespace))
		if err != nil {
			res, gerr := r.gate(ctx, cr, "SnapshotResolutionFailed",
				fmt.Sprintf("repository listing for namespace %q failed: %v", cr.Spec.Source.Namespace, err))
			return nil, nil, res, gerr
		}
		run, runSnaps, found := selectRun(snaps, cr.Spec.Source.Backup, cr.Spec.Source.Time)
		if !found {
			res, gerr := r.gate(ctx, cr, "RunNotFound",
				fmt.Sprintf("no run matching spec.source in the repository for namespace %q", cr.Spec.Source.Namespace))
			return nil, nil, res, gerr
		}
		r.Recorder.Eventf(cr, nil, corev1.EventTypeNormal, "RunResolved", "ResolveRun",
			"restoring run %q of namespace %s", run, cr.Spec.Source.Namespace)
		byPVC = dataSnapshotsByPVC(runSnaps)
		r.Engine.storeResolution(cr.UID, coordinate, byPVC)
	}

	if len(byPVC) == 0 {
		res, gerr := r.gate(ctx, cr, "RunNotFound",
			fmt.Sprintf("the resolved run holds no data snapshots for namespace %q", cr.Spec.Source.Namespace))
		return nil, nil, res, gerr
	}

	run := runOf(byPVC)
	rc := &restoreExecContext{
		ownerName:       cr.Name,
		ownerLabelKey:   apiconst.LabelClusterRestore,
		targetNamespace: cr.Spec.Target.Namespace,
		deleteExtras:    cr.Spec.Mode == cbv1.RestoreModeRecreate,
		restoredFromRun: run,
		repoName:        loc.Name,
		repoURL:         repo.Status.RepositoryURL,
		dek:             dek,
		s3CredsSecret:   loc.Spec.S3.CredentialsSecretRef.Name,
	}
	plans := r.buildPlans(ctx, cr, byPVC)
	if plans == nil {
		// Empty selection ⇒ valid, immediately-terminal (see the Restore controller's twin
		// comment): non-nil so nil keeps meaning "a gate was written".
		plans = []restoreVolumePlan{}
	}
	return rc, plans, ctrl.Result{}, nil
}

// buildPlans intersects the selection with the resolved snapshots and sizes each plan
// (live target claim > PVC-meta tags > snapshot-size fallback), applying the target's
// storageClassMapping to whatever class was resolved.
func (r *ClusterRestoreReconciler) buildPlans(ctx context.Context, cr *cbv1.ClusterRestore, byPVC map[string]restic.Snapshot) []restoreVolumePlan {
	sourcePVCs := make([]string, 0, len(byPVC))
	for pvc := range byPVC {
		sourcePVCs = append(sourcePVCs, pvc)
	}
	slices.Sort(sourcePVCs)
	selected := planVolumes(cr.Spec.Volumes, sourcePVCs, cr.Spec.Volumes != nil)

	var plans []restoreVolumePlan
	for _, pvc := range sourcePVCs {
		item, ok := selected[pvc]
		if !ok {
			continue
		}
		snap := byPVC[pvc]
		plan := restoreVolumePlan{
			pvc:          pvc,
			snapshotID:   snap.ID,
			snapshotPath: "/data/" + cr.Spec.Source.Namespace + "/" + pvc,
		}
		if item != nil {
			plan.include = item.Include
			plan.exclude = item.Exclude
			plan.targetPath = item.TargetPath
		}
		r.sizePlan(ctx, cr, &plan, &snap)
		plans = append(plans, plan)
	}
	return plans
}

// sizePlan fills provisioning inputs: an existing target claim wins; else the snapshot's
// PVC-meta tags (class run through storageClassMapping); else the documented fallback.
func (r *ClusterRestoreReconciler) sizePlan(ctx context.Context, cr *cbv1.ClusterRestore, plan *restoreVolumePlan, snap *restic.Snapshot) {
	var target corev1.PersistentVolumeClaim
	if err := r.Get(ctx, client.ObjectKey{Namespace: cr.Spec.Target.Namespace, Name: plan.pvc}, &target); err == nil {
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
		plan.capacity = fallbackRestoreCapacity(snapshotDataSize(snap))
	}
	plan.storageClass = meta.StorageClass
	if mapped, ok := cr.Spec.Target.StorageClassMapping[plan.storageClass]; ok && plan.storageClass != "" {
		plan.storageClass = mapped
	}
	for _, m := range meta.AccessModes {
		plan.accessModes = append(plan.accessModes, corev1.PersistentVolumeAccessMode(m))
	}
}

// awaitConfirmation parks the ClusterRestore in AwaitingConfirmation (R23).
func (r *ClusterRestoreReconciler) awaitConfirmation(ctx context.Context, cr *cbv1.ClusterRestore) (ctrl.Result, error) {
	first := cr.Status.Phase != string(status.RestorePhaseAwaitingConfirmation)
	cr.Status.Phase = string(status.RestorePhaseAwaitingConfirmation)
	status.SetCondition(&cr.Status.Conditions, ConditionReady, metav1.ConditionFalse, "ConfirmationRequired",
		fmt.Sprintf("a %s restore modifies existing data: set spec.confirmation to %q to proceed",
			cr.Spec.Mode, cr.Spec.Target.Namespace), cr.Generation)
	if err := r.Status().Update(ctx, cr); err != nil {
		return ctrl.Result{}, fmt.Errorf("update status for ClusterRestore %s: %w", cr.Name, err)
	}
	if first {
		r.Recorder.Eventf(cr, nil, corev1.EventTypeNormal, "ConfirmationRequired", "Confirm",
			"restore parked: set spec.confirmation to %q to proceed (R23)", cr.Spec.Target.Namespace)
	}
	return ctrl.Result{RequeueAfter: shortRequeueInterval}, nil
}

// gate records a non-terminal blocker and requeues on the fixable-fault cadence.
func (r *ClusterRestoreReconciler) gate(ctx context.Context, cr *cbv1.ClusterRestore, reason, message string) (ctrl.Result, error) {
	cr.Status.Phase = string(status.RestorePhasePending)
	status.SetCondition(&cr.Status.Conditions, ConditionReady, metav1.ConditionFalse, reason, message, cr.Generation)
	if err := r.Status().Update(ctx, cr); err != nil {
		return ctrl.Result{}, fmt.Errorf("update status for ClusterRestore %s: %w", cr.Name, err)
	}
	return ctrl.Result{RequeueAfter: shortRequeueInterval}, nil
}

// finalize tears down the restore's operator-side residue before dropping the finalizer.
func (r *ClusterRestoreReconciler) finalize(ctx context.Context, cr *cbv1.ClusterRestore) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(cr, apiconst.FinalizerClusterRestore) {
		return ctrl.Result{}, nil
	}
	r.Engine.forgetResolution(cr.UID)
	rc := &restoreExecContext{
		ownerName:       cr.Name,
		ownerLabelKey:   apiconst.LabelClusterRestore,
		targetNamespace: cr.Spec.Target.Namespace,
	}
	teardownRestoreResidue(ctx, r.Engine, rc)
	r.Recorder.Eventf(cr, nil, corev1.EventTypeNormal, "Finalizing", "Finalize",
		"tearing down restore movers and target exposures; no repository data is erased")
	controllerutil.RemoveFinalizer(cr, apiconst.FinalizerClusterRestore)
	if err := r.Update(ctx, cr); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("remove finalizer from ClusterRestore %s: %w", cr.Name, err)
	}
	return ctrl.Result{}, nil
}

// SetupWithManager registers the reconciler with its label-mapped Job watch.
func (r *ClusterRestoreReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&cbv1.ClusterRestore{}).
		Watches(&batchv1.Job{}, handler.EnqueueRequestsFromMapFunc(r.mapJobToClusterRestore)).
		Named("clusterrestore").
		Complete(r)
}

// mapJobToClusterRestore maps a restore mover Job back to its owning ClusterRestore.
func (r *ClusterRestoreReconciler) mapJobToClusterRestore(_ context.Context, obj client.Object) []reconcile.Request {
	labels := obj.GetLabels()
	if labels[apiconst.LabelManagedBy] != apiconst.ManagedByValue {
		return nil
	}
	name := labels[apiconst.LabelClusterRestore]
	if name == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: name}}}
}

// selectRun picks the run to restore from a namespace's snapshot listing: the NAMED run
// when spec.source.backup is set; else the newest run whose most recent snapshot is at or
// before the time cutoff ("latest" or empty ⇒ no cutoff). Returns the run name and its
// snapshots.
func selectRun(snaps []restic.Snapshot, namedRun, timeSpec string) (string, []restic.Snapshot, bool) {
	byRun := make(map[string][]restic.Snapshot)
	newest := make(map[string]time.Time)
	for _, s := range snaps {
		run, ok := restic.TagValue(s.Tags, restic.TagKeyRun)
		if !ok || run == "" {
			continue
		}
		byRun[run] = append(byRun[run], s)
		if s.Time.After(newest[run]) {
			newest[run] = s.Time
		}
	}

	if namedRun != "" {
		rs, ok := byRun[namedRun]
		return namedRun, rs, ok
	}

	var cutoff *time.Time
	if timeSpec != "" && timeSpec != "latest" {
		parsed, err := time.Parse(time.RFC3339, timeSpec)
		if err != nil {
			return "", nil, false
		}
		cutoff = &parsed
	}
	best := ""
	for run, t := range newest {
		if cutoff != nil && t.After(*cutoff) {
			continue
		}
		if best == "" || t.After(newest[best]) {
			best = run
		}
	}
	if best == "" {
		return "", nil, false
	}
	return best, byRun[best], true
}

// runOf extracts the (single) run tag of a resolved per-PVC snapshot set.
func runOf(byPVC map[string]restic.Snapshot) string {
	for _, s := range byPVC {
		if run, ok := restic.TagValue(s.Tags, restic.TagKeyRun); ok {
			return run
		}
	}
	return ""
}

// snapshotDataSize returns a snapshot's recorded logical size (restic ≥0.17 summary), or 0.
func snapshotDataSize(s *restic.Snapshot) int64 {
	if s.Summary == nil {
		return 0
	}
	return s.Summary.TotalBytesProcessed
}
