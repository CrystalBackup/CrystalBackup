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
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

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
	Recorder events.EventRecorder
	// Inventory runs Lister passes off the reconcile worker, single-flight per repository.
	Inventory *inventoryTracker
}

// NewDiscoveryReconciler builds the reconciler. Callers wire the production or stub lister here.
func NewDiscoveryReconciler(c client.Client, scheme *runtime.Scheme, lister SnapshotLister, recorder events.EventRecorder) *DiscoveryReconciler {
	return &DiscoveryReconciler{
		Client:    c,
		Scheme:    scheme,
		Lister:    lister,
		Recorder:  recorder,
		Inventory: newInventoryTracker(),
	}
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

	// The inventory runs OFF this worker (inventoryTracker): a pass creates a `restic snapshots`
	// Job and waits on it — seconds, cold every time — and holding the controller's single worker
	// for that made every other repository queue behind it. Consume a finished pass if one is
	// waiting, otherwise start one and return immediately; the tracker re-enqueues us when it
	// lands, with a watchdog requeue in case that wake is ever lost.
	res, state := r.Inventory.take(repo.Name)
	switch state {
	case inventoryPending:
		return ctrl.Result{RequeueAfter: inventoryWatchdogInterval}, nil
	case inventoryIdle:
		r.Inventory.start(&repo, r.Lister)
		return ctrl.Result{RequeueAfter: inventoryWatchdogInterval}, nil
	case inventoryReady:
		// fall through to consume res
	}

	if res.err != nil {
		log.Error(res.err, "discovery: inventory failed; will retry", "repository", repo.Name)
		r.Recorder.Eventf(&repo, nil, corev1.EventTypeWarning, "InventoryFailed", "InventoryRepository",
			"repository inventory failed: %v", res.err)
		// A failed listing measured nothing, so the inventory counts and lastDiscoveryTime keep
		// their previous values — but the OUTCOME is recorded, because that is what
		// crystalbackup_discovery_last_success reports and what DiscoveryFailed alerts on. A
		// discovery that has been failing for an hour is exactly the state where every other
		// field on this status is stale and no longer says so.
		// Logged, not returned: the pass is already scheduled to retry in discoveryRetryInterval,
		// and turning a status conflict on a health FLAG into a reconcile error would replace that
		// steady cadence with exponential backoff — slowing down the recovery of the very thing
		// the flag is reporting on.
		if err := r.recordDiscoveryOutcome(ctx, &repo, false); err != nil {
			log.Error(err, "discovery: could not record the failed outcome", "repository", repo.Name)
		}
		return ctrl.Result{RequeueAfter: discoveryRetryInterval}, nil
	}
	snaps := res.snaps

	groups := restic.GroupByNamespaceRun(snaps)

	// The live set of (namespace, run) keys the repository still holds, for the GC pass.
	repoKeys := make(map[restic.NamespaceRun]struct{}, len(groups))
	for key := range groups {
		if key.Namespace != "" {
			repoKeys[key] = struct{}{}
		}
	}

	// (1) Project one Backup per (namespace, run) group whose namespace exists.
	//
	// A per-group failure is ACCUMULATED, never returned early. Returning here discarded the whole
	// inventory the pass had just paid for: step (3) never ran, so lastDiscoveryTime froze and the
	// retry re-listed the entire repository from S3 to learn exactly the same thing. One namespace
	// refusing a projection must cost that namespace — not the other groups, and not the inventory
	// (docs/audit-m3.1-throughput.md, the flakiness chain).
	var failures []error
	// projected / orphans are the census crystalbackup_discovery_projected_backups and
	// _orphan_snapshots report. They are counted HERE, from the pass that actually resolved each
	// namespace, rather than re-derived later: projectGroup already performs the namespace lookup
	// that decides which of the two a group is, and a second pass would both pay for the lookups
	// twice and be able to disagree with the projections that were just written.
	var projected, orphans int32
	for key, groupSnaps := range groups {
		if key.Namespace == "" {
			continue // a cluster-manifests group has no namespace to project into (admin ClusterRestore only).
		}
		outcome, err := r.projectGroup(ctx, &repo, key, groupSnaps)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if outcome == groupOrphan {
			orphans++
		} else {
			projected++
		}
	}

	// (2) GC projections whose snapshots are gone (post-forget); never touch execution Backups.
	// Scoped to THIS repository's location so multiple locations never GC each other's projections.
	// Accumulated for the same reason: a stale projection surviving one extra pass is a far smaller
	// problem than a frozen inventory.
	if err := r.gcProjections(ctx, repo.Name, repoKeys); err != nil {
		failures = append(failures, err)
	}

	// (3) Record the inventory on the repository status — ALWAYS, partial pass included. What the
	// repository holds is what the listing measured, independently of whether every projection
	// landed, and it is stamped with the time the pass actually ran (res.at) rather than now, so a
	// reused inventory never claims to be fresher than it is. A failure to PERSIST it is the one
	// error worth returning: nothing downstream can trust a pass whose result was never written.
	if err := r.updateInventoryStatus(ctx, &repo, snaps, res.at, discoveryCensus{
		projected: projected,
		orphans:   orphans,
		// A pass that could not reconcile every group did not succeed, even though its inventory
		// is sound. The question discovery_last_success answers is "are the Backup projections
		// current with the repository", and after a partial pass some of them are not.
		success: len(failures) == 0,
	}); err != nil {
		return ctrl.Result{}, err
	}

	if len(failures) > 0 {
		return r.reportPartialPass(ctx, &repo, res, failures, interval), nil
	}

	return ctrl.Result{RequeueAfter: interval}, nil
}

// recordDiscoveryOutcome writes ONLY the success flag, for the path where the inventory listing
// itself failed and there is no census to record. It re-reads nothing: the caller holds the
// repository it just reconciled, and a conflict simply costs the next pass a retry — this field
// is a health signal, not a lock.
func (r *DiscoveryReconciler) recordDiscoveryOutcome(ctx context.Context, repo *cbv1.BackupRepository, success bool) error {
	if repo.Status.LastDiscoverySuccess != nil && *repo.Status.LastDiscoverySuccess == success {
		return nil // already says this; writing it again is pure API churn.
	}
	repo.Status.LastDiscoverySuccess = &success
	if err := r.Status().Update(ctx, repo); err != nil {
		return fmt.Errorf("record discovery outcome on repository %s: %w", repo.Name, err)
	}
	return nil
}

// reportPartialPass handles a pass that inventoried the repository but could not project (or GC)
// every group: it reports the failures and schedules a prompt retry that reuses the inventory
// already in hand.
//
// Retention is what makes the retry cheap. Without it the next reconcile finds the tracker idle and
// starts a full `restic snapshots` Job — the O(snapshots) S3 round trip — every retry interval, for
// as long as one namespace stays unhappy. With it, the retry re-attempts only the projection, and a
// fresh listing is paid at most once per discovery interval (inventoryTracker.retain bounds the
// reuse by exactly that age).
func (r *DiscoveryReconciler) reportPartialPass(ctx context.Context, repo *cbv1.BackupRepository,
	res *inventoryResult, failures []error, interval time.Duration,
) ctrl.Result {
	err := kerrors.NewAggregate(failures)
	logf.FromContext(ctx).Error(err, "discovery: inventory recorded, but some groups could not be reconciled",
		"repository", repo.Name, "failures", len(failures))
	r.Recorder.Eventf(repo, nil, corev1.EventTypeWarning, "ProjectionIncomplete", "ProjectRepository",
		"repository inventory recorded, but %d group(s) could not be reconciled: %v", len(failures), err)

	r.Inventory.retain(repo.Name, res, interval)
	return ctrl.Result{RequeueAfter: discoveryRetryInterval}
}

// projectGroup ensures the read-only projected Backup for one (namespace, run) group, unless the
// namespace is absent (skip: the restore point stays repository-only, reachable via ClusterRestore)
// or a NON-projected, still-in-flight execution Backup occupies the name (skip: never disturb a run
// in progress). Otherwise it server-side-applies the projection — creating it, refreshing a prior
// projection, or ADOPTING a now-terminal execution Backup into a projection — under discovery's own
// field manager.
func (r *DiscoveryReconciler) projectGroup(ctx context.Context, repo *cbv1.BackupRepository, key restic.NamespaceRun, snaps []restic.Snapshot) (groupOutcome, error) {
	var ns corev1.Namespace
	if err := r.Get(ctx, client.ObjectKey{Name: key.Namespace}, &ns); err != nil {
		if apierrors.IsNotFound(err) {
			return groupOrphan, nil // namespace gone: do not fabricate a Backup for it.
		}
		return groupProjected, fmt.Errorf("get namespace %s: %w", key.Namespace, err)
	}
	// A TERMINATING namespace still resolves through the Get above, but the API server rejects
	// every create in it ("unable to create new content in namespace X because it is being
	// terminated"). Treated as a hard error that aborts the whole reconcile, that transient and
	// entirely expected state stopped the pass before it could record the inventory, so the retry
	// re-ran a FULL re-inventory — a fresh `restic snapshots` Job every few seconds for as long as
	// the namespace took to disappear, with lastDiscoveryTime frozen throughout. It is the same
	// situation as a namespace that is already gone: skip it, and let the next pass (or the GC)
	// settle it once the deletion completes.
	if ns.Status.Phase == corev1.NamespaceTerminating || ns.DeletionTimestamp != nil {
		// Counted as projected, not orphaned: the namespace still exists and its Backups are
		// still listed, so this is the census `kubectl get backups` would agree with right now.
		// The next pass, once the deletion completes, moves it to orphan.
		return groupProjected, nil
	}

	// recorded is the status an EXECUTION already wrote at this coordinate, if any. It is the input
	// to the merge below: the projection COMPLETES an execution report, it never replaces one.
	var recorded cbv1.BackupStatus
	var existing cbv1.Backup
	err := r.Get(ctx, client.ObjectKey{Namespace: key.Namespace, Name: key.Run}, &existing)
	switch {
	case apierrors.IsNotFound(err):
		// no object yet → create the projection below.
	case err != nil:
		return groupProjected, fmt.Errorf("get Backup %s/%s: %w", key.Namespace, key.Run, err)
	default:
		projected := existing.Annotations[apiconst.AnnotationProjected] == apiconst.AnnotationProjectedValue
		if !projected && !isTerminalBackupPhase(existing.Status.Phase) {
			// An execution Backup is still running here — never touch it. It IS a Backup the
			// repository's data is visible through, so it counts as projected.
			return groupProjected, nil
		}
		existing.Status.DeepCopyInto(&recorded)
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
		// spec.run is deliberately ABSENT, and must stay that way (adr/0017 §2). This apply runs
		// with ForceOwnership, so every field named here is claimed by discovery's field manager —
		// and a manager that owns a field has to be able to reproduce it on the next pass.
		// Discovery's only input is the repository, and no pvcSelector, manifestOptions or hook
		// command was ever written to a restic snapshot: naming spec.run here would mean forcing it
		// empty on every discovery pass, permanently fighting whoever materialized it. Omitting it
		// also means an ADOPTION (a terminal execution Backup turning into a projection) leaves the
		// materialized run untouched — SSA only removes fields the applier previously owned.
		Spec: cbv1.BackupSpec{
			LocationRef: cbv1.LocationReference{Kind: kindClusterBackupLocation, Name: repo.Name},
		},
	}
	u, err := runtime.DefaultUnstructuredConverter.ToUnstructured(proj)
	if err != nil {
		return groupProjected, fmt.Errorf("convert projected Backup %s/%s to unstructured: %w", key.Namespace, key.Run, err)
	}
	if err := r.Apply(ctx, client.ApplyConfigurationFromUnstructured(&unstructured.Unstructured{Object: u}),
		client.FieldOwner(discoveryFieldManager), client.ForceOwnership); err != nil {
		return groupProjected, fmt.Errorf("project Backup %s/%s: %w", key.Namespace, key.Run, err)
	}

	// Apply the derived status as a separate status-scoped apply targeting the same object. This
	// apply runs with ForceOwnership, so what it names it OWNS and overwrites — which is exactly how
	// an adopted execution Backup used to lose its own report of itself: status.volumes has no
	// listType marker, so the apply replaced the whole list, and a Skipped volume with its
	// CSISnapshotUnsupported reason (a row `restic snapshots` cannot produce, because nothing was
	// snapshotted) disappeared seconds after the run finished, along with the PartiallyCompleted
	// phase that went with it. The value applied here is therefore the MERGE of what the repository
	// shows with what the execution recorded, and the recorded phase is never raised to a better one.
	statusObj := &cbv1.Backup{
		TypeMeta:   metav1.TypeMeta{APIVersion: cbv1.SchemeGroupVersion.String(), Kind: "Backup"},
		ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Run},
		Status: cbv1.BackupStatus{
			Phase:   projectedPhase(recorded.Phase),
			Volumes: discovery.MergeProjectedVolumes(recorded.Volumes, discovery.VolumesFromSnapshots(snaps)),
		},
	}
	su, err := runtime.DefaultUnstructuredConverter.ToUnstructured(statusObj)
	if err != nil {
		return groupProjected, fmt.Errorf("convert projected Backup status %s/%s to unstructured: %w", key.Namespace, key.Run, err)
	}
	if err := r.Status().Apply(ctx, client.ApplyConfigurationFromUnstructured(&unstructured.Unstructured{Object: su}),
		client.FieldOwner(discoveryFieldManager), client.ForceOwnership); err != nil {
		return groupProjected, fmt.Errorf("project Backup status %s/%s: %w", key.Namespace, key.Run, err)
	}
	return groupProjected, nil
}

// projectedPhase is the phase a projection may write over an existing record. A recorded TERMINAL
// phase is kept verbatim — never raised. A projection is rebuilt from `restic snapshots`, which by
// construction lists only what succeeded: it cannot see the volume that was skipped or the manifest
// dump that failed, so its opinion of the run is Completed no matter how the run actually ended.
// Overwriting a recorded PartiallyCompleted/PartiallyFailed with that opinion made the failure
// disappear from the cluster entirely within about thirty seconds of the run finishing.
//
// Only a coordinate with no recorded terminal result (a fresh projection, or a leftover in-flight
// phase on an object discovery is allowed to touch) gets Completed: there, the repository IS the
// only truth there is.
func projectedPhase(recorded string) string {
	if isTerminalBackupPhase(recorded) {
		return recorded
	}
	return string(status.BackupPhaseCompleted)
}

// groupOutcome is what one snapshot (namespace, run) group resolved to. The two values ARE the
// two discovery census gauges: a group is visible in the API as a Backup, or its namespace is
// gone and it is reachable only through a ClusterRestore (spec/05-observability.md §2.5).
type groupOutcome int

const (
	groupProjected groupOutcome = iota
	groupOrphan
)

// gcProjections deletes every projected (cluster-origin, AnnotationProjected) Backup of THIS
// location's repository whose (namespace, run) group is no longer in the repository — its snapshots
// were forgotten, so the projection must go (CR lifetime = data lifetime). It filters to projections
// by annotation, so a still-executing (non-projected) Backup is never deleted here, AND by
// locationName: discovery runs one reconcile per BackupRepository, and repoKeys is only this
// repository's inventory, so without the location filter a cluster with two or more
// ClusterBackupLocations would have each location's discovery delete the OTHER location's
// projections every pass (a false "its repository snapshots are gone"), the two mutually flapping
// each other's restore points in and out of the API. Deleting a Backup never runs restic forget;
// this only removes the now-meaningless view.
func (r *DiscoveryReconciler) gcProjections(ctx context.Context, locationName string, repoKeys map[restic.NamespaceRun]struct{}) error {
	var projections cbv1.BackupList
	if err := r.List(ctx, &projections, client.MatchingLabels{apiconst.LabelOrigin: apiconst.OriginCluster}); err != nil {
		return fmt.Errorf("list projected Backups: %w", err)
	}
	for i := range projections.Items {
		b := &projections.Items[i]
		if b.Annotations[apiconst.AnnotationProjected] != apiconst.AnnotationProjectedValue {
			continue // an execution Backup, not a discovery projection — leave it alone.
		}
		if b.Spec.LocationRef.Name != locationName {
			continue // a projection of a DIFFERENT location's repository — its own discovery owns it.
		}
		key := restic.NamespaceRun{Namespace: b.Namespace, Run: b.Name}
		if _, live := repoKeys[key]; live {
			continue
		}
		if err := r.Delete(ctx, b); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete stale projection %s/%s: %w", b.Namespace, b.Name, err)
		}
		r.Recorder.Eventf(b, nil, corev1.EventTypeNormal, "ProjectionRemoved", "RemoveProjection",
			"removed projected Backup %s/%s: its repository snapshots are gone", b.Namespace, b.Name)
	}
	return nil
}

// updateInventoryStatus records the snapshot count, distinct-namespace count and discovery time on
// the repository. It read-modify-writes (the BackupRepository controller owns other status fields
// and its Get-modify-Update preserves these, as this one preserves its), so a concurrent write only
// costs a conflict retry.
//
// at is when the listing itself ran, not when it is being recorded: a partial pass whose inventory
// is reused by the retry (inventoryTracker.retain) must not keep stamping a fresher time onto data
// it did not re-measure — and re-writing the identical timestamp makes that retry's status update a
// server-side no-op instead of pointless churn.
// discoveryCensus is what one pass learned about the SHAPE of the repository's projection into
// the cluster, as opposed to the raw snapshot inventory: how many groups became visible Backups,
// how many have no namespace left to become one in, and whether the pass was clean.
type discoveryCensus struct {
	projected, orphans int32
	success            bool
}

func (r *DiscoveryReconciler) updateInventoryStatus(ctx context.Context, repo *cbv1.BackupRepository,
	snaps []restic.Snapshot, at time.Time, census discoveryCensus,
) error {
	discovered := metav1.NewTime(at)
	repo.Status.SnapshotCount = int32(len(snaps))
	repo.Status.NamespacesPresent = int32(discovery.DistinctNamespaces(snaps))
	repo.Status.LastDiscoveryTime = &discovered
	repo.Status.ProjectedBackups = census.projected
	repo.Status.OrphanSnapshots = census.orphans
	success := census.success
	repo.Status.LastDiscoverySuccess = &success
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
	// nil means the admin did not say, and discovery defaults ON — the CRD default covers the
	// API-server path, this covers a Go client that never round-tripped through it.
	return loc.Spec.Discovery.Enabled == nil || *loc.Spec.Discovery.Enabled, interval, nil
}

// inventoryChurnPredicate filters the BackupRepository stream feeding discovery. Discovery WRITES
// this same object's inventory status (snapshotCount, namespacesPresent, lastDiscoveryTime) at the
// end of every successful pass; unfiltered, that write returns as an Update event and re-enqueues
// discovery immediately, so it never rests at `discovery.interval` — it spins back-to-back, each
// pass blocking its single worker on a fresh `restic snapshots` Job. Measured on the crucible:
// ~5.7 s per pass, one worker ~100 % saturated for a whole run, at THREE snapshots
// (docs/audit-m3.1-throughput.md). Cadence is meant to come from `RequeueAfter: interval` plus the
// ClusterBackup post-run nudge.
//
// The filter drops an update when the change is confined to fields discovery has no interest in:
// the three it writes itself, plus the maintenance controller's (M4). Discovery's own writes are
// the self-trigger loop above. The maintenance fields are here for the same reason one milestone
// later — that controller refreshes the repository's physical size and stale-lock count on a
// cadence of its own, and every one of those writes would otherwise cost a full `restic snapshots`
// Job for an inventory nobody asked to refresh.
//
// Everything else still wakes discovery: the `Initialized` flip that makes the repository
// inventoriable, keySlots, conditions, and any metadata change (the envtest specs nudge discovery
// with an annotation, and BackupRepositorySpec is empty so generation never moves). Create/Delete/
// Generic stay unfiltered (predicate.Funcs defaults them to true).
func inventoryChurnPredicate() predicate.Predicate {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldRepo, okOld := e.ObjectOld.(*cbv1.BackupRepository)
			newRepo, okNew := e.ObjectNew.(*cbv1.BackupRepository)
			if !okOld || !okNew {
				return true // not a BackupRepository — not ours to filter
			}
			if oldRepo.GetGeneration() != newRepo.GetGeneration() ||
				!equality.Semantic.DeepEqual(oldRepo.GetLabels(), newRepo.GetLabels()) ||
				!equality.Semantic.DeepEqual(oldRepo.GetAnnotations(), newRepo.GetAnnotations()) {
				return true
			}
			// Mask the fields discovery does not act on — its own inventory fields, and the
			// maintenance controller's. If the two statuses match once those are zeroed, this event
			// carries nothing discovery would do anything about; swallow it.
			oldStatus, newStatus := oldRepo.Status.DeepCopy(), newRepo.Status.DeepCopy()
			for _, s := range []*cbv1.BackupRepositoryStatus{oldStatus, newStatus} {
				s.SnapshotCount = 0
				s.NamespacesPresent = 0
				s.LastDiscoveryTime = nil
				s.ProjectedBackups = 0
				s.OrphanSnapshots = 0
				s.LastDiscoverySuccess = nil

				s.LastMaintenanceTime = nil
				s.LastCheckTime = nil
				s.LastCheckResult = ""
				s.ApproximateSizeBytes = 0
				s.StaleLocks = 0
				s.RecentMaintenance = nil
			}
			return !equality.Semantic.DeepEqual(oldStatus, newStatus)
		},
	}
}

// SetupWithManager registers this reconciler. It reconciles BackupRepositories and, via a mapping
// from a ClusterBackup to its location's repository, re-inventories promptly right after a run
// completes (rather than waiting for the next interval tick). The For watch is filtered so
// discovery's own inventory status writes do not re-trigger it (inventoryChurnPredicate).
func (r *DiscoveryReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&cbv1.BackupRepository{}, builder.WithPredicates(inventoryChurnPredicate())).
		Watches(&cbv1.ClusterBackup{}, handler.EnqueueRequestsFromMapFunc(r.mapRunToRepository)).
		// A background inventory pass re-enqueues its repository through this channel when it
		// finishes, so the result is consumed as soon as it exists rather than at the next tick.
		WatchesRawSource(source.Channel(r.Inventory.wake, &handler.EnqueueRequestForObject{})).
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
