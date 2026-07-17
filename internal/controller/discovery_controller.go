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
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
	"github.com/CrystalBackup/CrystalBackup/internal/discovery"
	"github.com/CrystalBackup/CrystalBackup/internal/restic"
	"github.com/CrystalBackup/CrystalBackup/internal/status"
)

const (
	// discoveryFieldManager is the DISTINCT server-side-apply field owner discovery projects
	// Backups under, so its declarative projection coexists with (and never fights) the execution
	// controllers that own their own fields on the same Backup kind (adr/0009: a projection is a
	// materialized view, not an execution).
	discoveryFieldManager = "crystalbackup-discovery"

	// discoveryDefaultInterval / discoveryMinInterval bound the inventory cadence when the location's
	// discovery.interval is unset or absurdly small. The interval is the primary driver for
	// picking up out-of-band repository changes (a forget run elsewhere); a ClusterBackup watch
	// additionally re-inventories promptly right after a run completes.
	discoveryDefaultInterval = time.Hour
	discoveryMinInterval     = time.Minute

	// discoveryRetryInterval paces a re-list after a transient inventory failure (the lister Job
	// failed, the location is not resolvable yet).
	discoveryRetryInterval = 30 * time.Second
)

// SnapshotLister inventories a repository's CrystalBackup snapshots
// (`restic snapshots --json --tag crystalbackup`). It is the seam the discovery controller reads
// ground truth through: production runs a restic Job and parses its output (internal/controller's
// jobSnapshotLister); envtest injects a stub returning canned snapshots, so the projection, GC and
// status logic is exercised without restic, S3 or a kubelet.
type SnapshotLister interface {
	List(ctx context.Context, repo *cbv1.BackupRepository) ([]restic.Snapshot, error)
}

// DiscoveryReconciler reconciles a BackupRepository into read-only Backup PROJECTIONS: it
// inventories the repository, groups snapshots by (namespace, run), and ensures exactly one
// projected Backup per group whose namespace still exists — the mechanism that makes a shared DR
// repository restorable with no pre-existing CRs (spec/02-api.md §Discovery). It is discovery's
// GC authority: a projection whose snapshots are gone (post-forget) is removed, so a CR's lifetime
// tracks its data's. It NEVER runs restic forget and NEVER touches an in-flight executing Backup.
type DiscoveryReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Lister   SnapshotLister
	Recorder record.EventRecorder
}

// NewDiscoveryReconciler builds the reconciler. Callers wire the production or stub lister here.
func NewDiscoveryReconciler(c client.Client, scheme *runtime.Scheme, lister SnapshotLister, recorder record.EventRecorder) *DiscoveryReconciler {
	return &DiscoveryReconciler{Client: c, Scheme: scheme, Lister: lister, Recorder: recorder}
}

// +kubebuilder:rbac:groups=crystalbackup.io,resources=backuprepositories,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=crystalbackup.io,resources=backuprepositories/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=crystalbackup.io,resources=clusterbackuplocations,verbs=get;list;watch
// +kubebuilder:rbac:groups=crystalbackup.io,resources=clusterbackups,verbs=get;list;watch
// +kubebuilder:rbac:groups=crystalbackup.io,resources=backups,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch
// The production JobSnapshotLister (discovery_lister.go) runs a `restic snapshots` mover Job and
// reads the inventory off the completed pod's log, so discovery also needs Jobs, the job-scoped
// creds Secret, and the pod log (pods/log) — the one subresource the cached client cannot stream.
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods/log,verbs=get

// Reconcile inventories the repository and reconciles the projected Backups against it.
func (r *DiscoveryReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var repo cbv1.BackupRepository
	if err := r.Get(ctx, req.NamespacedName, &repo); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	// Nothing to inventory until the repository exists in the object store.
	if !repo.Status.Initialized {
		return ctrl.Result{RequeueAfter: discoveryRetryInterval}, nil
	}

	enabled, interval, err := r.discoverySettings(ctx, &repo)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !enabled {
		// Discovery off for this location: re-triggered by a repository/location change, not polled.
		return ctrl.Result{}, nil
	}

	snaps, err := r.Lister.List(ctx, &repo)
	if err != nil {
		log.Error(err, "discovery: inventory failed; will retry", "repository", repo.Name)
		r.Recorder.Eventf(&repo, corev1.EventTypeWarning, "InventoryFailed",
			"repository inventory failed: %v", err)
		return ctrl.Result{RequeueAfter: discoveryRetryInterval}, nil
	}

	groups := restic.GroupByNamespaceRun(snaps)

	// The live set of (namespace, run) keys the repository still holds, for the GC pass.
	repoKeys := make(map[restic.NamespaceRun]struct{}, len(groups))
	for key := range groups {
		if key.Namespace != "" {
			repoKeys[key] = struct{}{}
		}
	}

	// (1) Project one Backup per (namespace, run) group whose namespace exists.
	for key, groupSnaps := range groups {
		if key.Namespace == "" {
			continue // a cluster-manifests group has no namespace to project into (admin ClusterRestore only).
		}
		if err := r.projectGroup(ctx, &repo, key, groupSnaps); err != nil {
			return ctrl.Result{}, err
		}
	}

	// (2) GC projections whose snapshots are gone (post-forget); never touch execution Backups.
	if err := r.gcProjections(ctx, repoKeys); err != nil {
		return ctrl.Result{}, err
	}

	// (3) Record the inventory on the repository status.
	if err := r.updateInventoryStatus(ctx, &repo, snaps); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: interval}, nil
}

// projectGroup ensures the read-only projected Backup for one (namespace, run) group, unless the
// namespace is absent (skip: the restore point stays repository-only, reachable via ClusterRestore)
// or a NON-projected, still-in-flight execution Backup occupies the name (skip: never disturb a run
// in progress). Otherwise it server-side-applies the projection — creating it, refreshing a prior
// projection, or ADOPTING a now-terminal execution Backup into a projection — under discovery's own
// field manager.
func (r *DiscoveryReconciler) projectGroup(ctx context.Context, repo *cbv1.BackupRepository, key restic.NamespaceRun, snaps []restic.Snapshot) error {
	var ns corev1.Namespace
	if err := r.Get(ctx, client.ObjectKey{Name: key.Namespace}, &ns); err != nil {
		if apierrors.IsNotFound(err) {
			return nil // namespace gone: do not fabricate a Backup for it.
		}
		return fmt.Errorf("get namespace %s: %w", key.Namespace, err)
	}

	var existing cbv1.Backup
	err := r.Get(ctx, client.ObjectKey{Namespace: key.Namespace, Name: key.Run}, &existing)
	switch {
	case apierrors.IsNotFound(err):
		// no object yet → create the projection below.
	case err != nil:
		return fmt.Errorf("get Backup %s/%s: %w", key.Namespace, key.Run, err)
	default:
		projected := existing.Annotations[apiconst.AnnotationProjected] == apiconst.AnnotationProjectedValue
		if !projected && !isTerminalBackupPhase(existing.Status.Phase) {
			return nil // an execution Backup is still running here — never touch it.
		}
	}

	proj := &cbv1.Backup{
		TypeMeta: metav1.TypeMeta{APIVersion: cbv1.SchemeGroupVersion.String(), Kind: "Backup"},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: key.Namespace,
			Name:      key.Run,
			Labels: map[string]string{
				apiconst.LabelOrigin:        apiconst.OriginCluster,
				apiconst.LabelClusterBackup: key.Run,
				apiconst.LabelNamespace:     key.Namespace,
			},
			Annotations: map[string]string{
				apiconst.AnnotationProjected: apiconst.AnnotationProjectedValue,
			},
		},
		Spec: cbv1.BackupSpec{
			LocationRef: cbv1.LocationReference{Kind: "ClusterBackupLocation", Name: repo.Name},
		},
	}
	if err := r.Patch(ctx, proj, client.Apply,
		client.FieldOwner(discoveryFieldManager), client.ForceOwnership); err != nil {
		return fmt.Errorf("project Backup %s/%s: %w", key.Namespace, key.Run, err)
	}

	// Apply the derived status (one Completed volume per data snapshot) as a separate status-scoped
	// apply targeting the same object.
	statusObj := &cbv1.Backup{
		TypeMeta:   metav1.TypeMeta{APIVersion: cbv1.SchemeGroupVersion.String(), Kind: "Backup"},
		ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Run},
		Status: cbv1.BackupStatus{
			Phase:   string(status.BackupPhaseCompleted),
			Volumes: discovery.VolumesFromSnapshots(snaps),
		},
	}
	if err := r.Status().Patch(ctx, statusObj, client.Apply,
		client.FieldOwner(discoveryFieldManager), client.ForceOwnership); err != nil {
		return fmt.Errorf("project Backup status %s/%s: %w", key.Namespace, key.Run, err)
	}
	return nil
}

// gcProjections deletes every projected (cluster-origin, AnnotationProjected) Backup whose
// (namespace, run) group is no longer in the repository — its snapshots were forgotten, so the
// projection must go (CR lifetime = data lifetime). It filters to projections by annotation, so a
// still-executing (non-projected) Backup is never deleted here. Deleting a Backup never runs restic
// forget; this only removes the now-meaningless view.
func (r *DiscoveryReconciler) gcProjections(ctx context.Context, repoKeys map[restic.NamespaceRun]struct{}) error {
	var projections cbv1.BackupList
	if err := r.List(ctx, &projections, client.MatchingLabels{apiconst.LabelOrigin: apiconst.OriginCluster}); err != nil {
		return fmt.Errorf("list projected Backups: %w", err)
	}
	for i := range projections.Items {
		b := &projections.Items[i]
		if b.Annotations[apiconst.AnnotationProjected] != apiconst.AnnotationProjectedValue {
			continue // an execution Backup, not a discovery projection — leave it alone.
		}
		key := restic.NamespaceRun{Namespace: b.Namespace, Run: b.Name}
		if _, live := repoKeys[key]; live {
			continue
		}
		if err := r.Delete(ctx, b); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete stale projection %s/%s: %w", b.Namespace, b.Name, err)
		}
		r.Recorder.Eventf(b, corev1.EventTypeNormal, "ProjectionRemoved",
			"removed projected Backup %s/%s: its repository snapshots are gone", b.Namespace, b.Name)
	}
	return nil
}

// updateInventoryStatus records the snapshot count, distinct-namespace count and discovery time on
// the repository. It read-modify-writes (the BackupRepository controller owns other status fields
// and its Get-modify-Update preserves these, as this one preserves its), so a concurrent write only
// costs a conflict retry.
func (r *DiscoveryReconciler) updateInventoryStatus(ctx context.Context, repo *cbv1.BackupRepository, snaps []restic.Snapshot) error {
	now := metav1.Now()
	repo.Status.SnapshotCount = int32(len(snaps))
	repo.Status.NamespacesPresent = int32(discovery.DistinctNamespaces(snaps))
	repo.Status.LastDiscoveryTime = &now
	if err := r.Status().Update(ctx, repo); err != nil {
		return fmt.Errorf("update repository inventory status %s: %w", repo.Name, err)
	}
	return nil
}

// discoverySettings resolves whether discovery is enabled and its interval from the repository's
// backing ClusterBackupLocation. A missing location means the repository is mid-teardown or not yet
// linked — treat as disabled rather than erroring.
func (r *DiscoveryReconciler) discoverySettings(ctx context.Context, repo *cbv1.BackupRepository) (bool, time.Duration, error) {
	var loc cbv1.ClusterBackupLocation
	if err := r.Get(ctx, client.ObjectKey{Name: repo.Name}, &loc); err != nil {
		if apierrors.IsNotFound(err) {
			return false, 0, nil
		}
		return false, 0, fmt.Errorf("get ClusterBackupLocation %s: %w", repo.Name, err)
	}
	interval := loc.Spec.Discovery.Interval.Duration
	if interval <= 0 {
		interval = discoveryDefaultInterval
	}
	if interval < discoveryMinInterval {
		interval = discoveryMinInterval
	}
	return loc.Spec.Discovery.Enabled, interval, nil
}

// SetupWithManager registers this reconciler. It reconciles BackupRepositories and, via a mapping
// from a ClusterBackup to its location's repository, re-inventories promptly right after a run
// completes (rather than waiting for the next interval tick).
func (r *DiscoveryReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&cbv1.BackupRepository{}).
		Watches(&cbv1.ClusterBackup{}, handler.EnqueueRequestsFromMapFunc(r.mapRunToRepository)).
		Named("discovery").
		Complete(r)
}

// mapRunToRepository maps a ClusterBackup run to the BackupRepository backing its location (the
// repository shares the location's name), so a completing run nudges a fresh inventory.
func (r *DiscoveryReconciler) mapRunToRepository(_ context.Context, obj client.Object) []reconcile.Request {
	cb, ok := obj.(*cbv1.ClusterBackup)
	if !ok {
		return nil
	}
	loc := cb.Spec.LocationRef.Name
	if loc == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: loc}}}
}
