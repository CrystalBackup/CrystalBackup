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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/client/secrets"
	"github.com/CrystalBackup/CrystalBackup/internal/metrics"
	"github.com/CrystalBackup/CrystalBackup/internal/mover"
	"github.com/CrystalBackup/CrystalBackup/internal/repo/queue"
	"github.com/CrystalBackup/CrystalBackup/internal/restic"
	"github.com/CrystalBackup/CrystalBackup/internal/status"
)

// ClusterErasurePhase values (the CRD pins this enum).
const (
	erasurePhasePending      = "Pending"
	erasurePhaseAwaiting     = "AwaitingConfirmation"
	erasurePhaseRunning      = "Running"
	erasurePhaseCompleted    = "Completed"
	erasurePhaseBlocked      = "Blocked"
	erasurePhaseFailed       = "Failed"
	erasureRequeueInterval   = 15 * time.Second
	erasureBlockedRecheck    = 1 * time.Hour
	conditionErasureComplete = "ErasureComplete"
)

// ClusterErasureReconciler executes the right to erasure (R21): the PHYSICAL removal of a
// tenant's, a namespace's or a single PVC's data from a shared repository.
//
// It is the most destructive controller in the system, so its shape is defensive on purpose.
//
//   - IT IS NOT CRYPTO-SHREDDING. A shared cluster repository has exactly ONE restic master key
//     protecting every namespace (adr/0004 §4), so there is no per-tenant key to destroy. Erasure
//     is `forget` + `prune`: the snapshots go, and prune reclaims the pack space no surviving
//     snapshot references. Nothing about it is reversible.
//   - IT REQUIRES A TYPED CONFIRMATION (R23). An absent confirmation parks the object in
//     AwaitingConfirmation rather than rejecting it, so the destructive act is deliberately two
//     steps: create the intent, read what it says it will remove, then type the identity back.
//   - IT IS BLOCKED ON IMMUTABLE LOCATIONS. Object Lock forbids deletion until expiry, and
//     "reported success while the objects are still there" is the worst possible outcome for a
//     GDPR request. The erasure parks in Blocked with blockedUntil and re-checks.
//   - IT NEVER WIDENS. An under-specified target selects NOTHING (restic.ErasureFilterTags), and
//     the count of what will go is measured and recorded BEFORE the forget runs.
//
// forget and prune are two separate ops on the repository's exclusive queue (adr/0015), driven the
// same way the repository controller drives init: at most one in flight, tracked by Handle, polled
// without blocking the reconcile goroutine.
type ClusterErasureReconciler struct {
	client.Client
	Scheme            *runtime.Scheme
	Secrets           *secrets.ByNameReader
	Queue             *queue.Manager
	Lister            FilteredSnapshotLister
	OperatorNamespace string
	MoverImage        string
	// MoverProfiles is the resolved per-operation sizing table: an erasure's forget takes the
	// light row, the prune that follows it the heavy one. Nil ⇒ built-in.
	MoverProfiles mover.Profiles
	// MoverPlacement is the operator-wide scheduling policy, applied to both halves of an erasure.
	// The prune is the heaviest S3 conversation the product has, so a node pool chosen for its
	// egress is chosen for this op above all others.
	MoverPlacement mover.Placement
	Recorder       events.EventRecorder

	mu       sync.Mutex
	inflight map[string]*queue.Handle
}

// NewClusterErasureReconciler builds the reconciler with its in-flight map initialised.
func NewClusterErasureReconciler(
	c client.Client, scheme *runtime.Scheme, secretsReader *secrets.ByNameReader,
	q *queue.Manager, lister FilteredSnapshotLister, operatorNamespace, moverImage string,
	moverProfiles mover.Profiles,
	moverPlacement mover.Placement,
	recorder events.EventRecorder,
) *ClusterErasureReconciler {
	return &ClusterErasureReconciler{
		Client: c, Scheme: scheme, Secrets: secretsReader, Queue: q, Lister: lister,
		OperatorNamespace: operatorNamespace, MoverImage: moverImage, MoverProfiles: moverProfiles,
		MoverPlacement: moverPlacement,
		Recorder:       recorder,
		inflight:       map[string]*queue.Handle{},
	}
}

// +kubebuilder:rbac:groups=crystalbackup.io,resources=clustererasures,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=crystalbackup.io,resources=clustererasures/status,verbs=get;update;patch

// Reconcile drives one ClusterErasure from Pending to a terminal phase.
func (r *ClusterErasureReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var er cbv1.ClusterErasure
	if err := r.Get(ctx, req.NamespacedName, &er); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Terminal is terminal. An erasure is not idempotent in any useful sense — re-running a
	// completed one would forget snapshots written SINCE, under a confirmation typed for the
	// earlier scope. Once Completed or Failed, this object is a record, not an instruction.
	if er.Status.Phase == erasurePhaseCompleted || er.Status.Phase == erasurePhaseFailed {
		return ctrl.Result{}, nil
	}

	// The scope, resolved once. An under-specified target never reaches restic.
	filterTags, ok := restic.ErasureFilterTags(er.Spec.Target)
	if !ok {
		return r.fail(ctx, &er, "InvalidTarget",
			"spec.target selects nothing: set tenant, namespace, or namespace+pvc (a bare pvc is not an identity)")
	}

	// R23: an ABSENT confirmation parks; a WRONG one fails. The admission policy admits empty and
	// denies a non-matching non-empty value, so reaching here with a mismatch means admission was
	// bypassed — treat it as a hard failure rather than proceeding.
	want := erasureConfirmationFor(er.Spec.Target)
	switch er.Spec.Confirmation {
	case "":
		return r.park(ctx, &er, erasurePhaseAwaiting, "AwaitingConfirmation",
			fmt.Sprintf("this will PERMANENTLY remove every snapshot matching %v from location %q. "+
				"To proceed, set spec.confirmation to %q", filterTags, er.Spec.LocationRef.Name, want))
	case want:
		// confirmed
	default:
		return r.fail(ctx, &er, "ConfirmationMismatch",
			fmt.Sprintf("spec.confirmation %q does not match the target identity %q", er.Spec.Confirmation, want))
	}

	var loc cbv1.ClusterBackupLocation
	if err := r.Get(ctx, client.ObjectKey{Name: er.Spec.LocationRef.Name}, &loc); err != nil {
		if apierrors.IsNotFound(err) {
			return r.park(ctx, &er, erasurePhasePending, "LocationNotFound",
				fmt.Sprintf("ClusterBackupLocation %q not found", er.Spec.LocationRef.Name))
		}
		return ctrl.Result{}, fmt.Errorf("get ClusterBackupLocation %s: %w", er.Spec.LocationRef.Name, err)
	}

	// Immutable: Object Lock forbids the deletion, so the erasure waits rather than reporting a
	// success that did not happen (adr/0005). blockedUntil is left to the location's rotation
	// bookkeeping, which lands with M8 — until then the field says "unknown" by being empty, which
	// is honest, where a fabricated date would not be.
	if loc.Spec.Mode == cbv1.LocationModeImmutable {
		r.Recorder.Eventf(&er, nil, corev1.EventTypeWarning, "ErasureBlocked", "Erase",
			"location %q is Immutable; erasure is blocked until object-lock expiry (adr/0005)", loc.Name)
		res, err := r.park(ctx, &er, erasurePhaseBlocked, "ImmutableLocation",
			"the location is Immutable: object lock forbids deletion until expiry, so nothing has been erased")
		if err != nil || !res.IsZero() {
			return res, err
		}
		return ctrl.Result{RequeueAfter: erasureBlockedRecheck}, nil
	}

	var repo cbv1.BackupRepository
	if err := r.Get(ctx, client.ObjectKey{Name: loc.Name}, &repo); err != nil || !repo.Status.Initialized {
		return r.park(ctx, &er, erasurePhasePending, "RepositoryNotReady",
			fmt.Sprintf("BackupRepository %q is not initialized yet", loc.Name))
	}

	return r.drive(ctx, &er, &loc, &repo, filterTags)
}

// drive is the in-flight state machine: count, forget, prune, verify — each step recorded before
// the next begins, so a restart resumes rather than restarts.
func (r *ClusterErasureReconciler) drive(ctx context.Context, er *cbv1.ClusterErasure,
	loc *cbv1.ClusterBackupLocation, repo *cbv1.BackupRepository, filterTags []string,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	key := er.Name

	// Measure the scope BEFORE erasing, and persist it. This is the compliance record — "how many
	// snapshots were removed" has to be answerable afterwards, and afterwards they are gone. It is
	// also the last chance to discover that the scope is empty, which is a legitimate Completed
	// (nothing matched) rather than a failure.
	if er.Status.Phase != erasurePhaseRunning {
		snaps, err := r.Lister.ListFiltered(ctx, repo, erasureCountJobName(er.Name), filterTags...)
		if err != nil {
			log.Error(err, "erasure: count the scope", "erasure", er.Name)
			return r.park(ctx, er, erasurePhasePending, "ScopeUnreadable",
				fmt.Sprintf("could not list the snapshots this erasure would remove: %v", err))
		}
		er.Status.SnapshotsForgotten = int32(len(snaps)) //nolint:gosec // a repository's snapshot count is not int32-overflow territory
		if len(snaps) == 0 {
			return r.complete(ctx, er, loc, "nothing matched the target; no snapshot was removed")
		}
		er.Status.Phase = erasurePhaseRunning
		status.SetCondition(&er.Status.Conditions, ConditionReady, metav1.ConditionFalse, "Erasing",
			fmt.Sprintf("erasing %d snapshot(s) from repository %s", len(snaps), repo.Name), er.Generation)
		if err := r.Status().Update(ctx, er); err != nil {
			return ctrl.Result{}, fmt.Errorf("update status for ClusterErasure %s: %w", er.Name, err)
		}
		return ctrl.Result{RequeueAfter: erasureRequeueInterval}, nil
	}

	r.mu.Lock()
	h := r.inflight[key]
	r.mu.Unlock()

	if h == nil {
		forgetArgs, err := restic.ErasureForgetArgs(er.Spec.Target)
		if err != nil {
			return r.fail(ctx, er, "InvalidTarget", err.Error())
		}
		// spec.maintenance is OPTIONAL and a POINTER, and erasure is the one caller that reaches
		// prune without it. The maintenance controller dereferences the same field safely only
		// because it gets there from a SCHEDULE, which cannot exist without the block; an erasure
		// arrives from a ClusterErasure and knows nothing about the location's maintenance
		// configuration. Dereferencing unconditionally panicked the reconciler — and a panic
		// retries, so it panicked again, on every requeue, on the most destructive path in the
		// system. An absent block simply means no repack cap.
		pruneArgs, err := restic.PruneCommand(erasurePruneMaxRepackSize(loc))
		if err != nil {
			return r.fail(ctx, er, "InvalidPruneOptions", err.Error())
		}
		deps := repoMaintenanceDeps{
			Client: r.Client, Secrets: r.Secrets, OperatorNamespace: r.OperatorNamespace,
			MoverImage: r.MoverImage, MoverProfiles: r.MoverProfiles,
			MoverPlacement: r.MoverPlacement,
		}
		dek, reason, dekErr := resolvePlatformDEK(ctx, r.Client, r.Secrets, r.OperatorNamespace, loc)
		if dekErr != nil {
			return r.park(ctx, er, erasurePhaseRunning, reason, dekErr.Error())
		}
		owner := er.DeepCopy()
		repoURL, credsSecret := repo.Status.RepositoryURL, loc.Spec.S3.CredentialsSecretRef.Name
		// The erasure prune is the single heaviest S3 conversation in the product — it repacks
		// every pack a forgotten tenant touched — so it is the last op that should ignore the
		// location's connection cap.
		s3Connections := loc.Spec.S3.Connections

		// ONE queued op runs forget THEN prune. They are inseparable: a forget without its prune
		// leaves the tenant's bytes in the packs, so an erasure that reported Completed between
		// the two would be claiming a deletion that has not happened. Enqueued together, they also
		// hold the repository's exclusive lane once instead of racing another op into the gap.
		handle, enqErr := r.Queue.Enqueue(repo.Name, queue.OpPrune, func(opCtx context.Context) error {
			if err := runRepoMaintenance(opCtx, deps, erasureResourceName(owner.Name, "forget"),
				mover.OpForget, forgetArgs, repoURL, dek, credsSecret, "", s3Connections); err != nil {
				return fmt.Errorf("erasure forget: %w", err)
			}
			if err := runRepoMaintenance(opCtx, deps, erasureResourceName(owner.Name, "prune"),
				mover.OpPrune, pruneArgs, repoURL, dek, credsSecret, "", s3Connections); err != nil {
				return fmt.Errorf("erasure prune (the snapshots are forgotten; their space is not yet reclaimed): %w", err)
			}
			return nil
		})
		if enqErr != nil {
			return ctrl.Result{RequeueAfter: erasureRequeueInterval}, nil
		}
		r.mu.Lock()
		r.inflight[key] = handle
		r.mu.Unlock()
		r.Recorder.Eventf(er, nil, corev1.EventTypeWarning, "ErasureStarted", "Erase",
			"forget+prune enqueued on repository %s for %d snapshot(s) — this is irreversible",
			repo.Name, er.Status.SnapshotsForgotten)
		return ctrl.Result{RequeueAfter: erasureRequeueInterval}, nil
	}

	select {
	case <-h.Done():
		r.mu.Lock()
		delete(r.inflight, key)
		r.mu.Unlock()
		if e := h.Err(); e != nil {
			return r.fail(ctx, er, "ErasureFailed", e.Error())
		}
		return r.complete(ctx, er, loc, fmt.Sprintf("%d snapshot(s) forgotten and their space pruned",
			er.Status.SnapshotsForgotten))
	default:
		return ctrl.Result{RequeueAfter: erasureRequeueInterval}, nil
	}
}

// park records a non-terminal phase and requeues. It is the shape every "not yet" answer takes,
// so the object always says what it is waiting for.
func (r *ClusterErasureReconciler) park(ctx context.Context, er *cbv1.ClusterErasure,
	phase, reason, message string,
) (ctrl.Result, error) {
	er.Status.Phase = phase
	status.SetCondition(&er.Status.Conditions, ConditionReady, metav1.ConditionFalse, reason, message, er.Generation)
	if err := r.Status().Update(ctx, er); err != nil {
		return ctrl.Result{}, fmt.Errorf("update status for ClusterErasure %s: %w", er.Name, err)
	}
	return ctrl.Result{RequeueAfter: erasureRequeueInterval}, nil
}

// fail records a terminal failure. Reached only for faults no requeue can fix.
func (r *ClusterErasureReconciler) fail(ctx context.Context, er *cbv1.ClusterErasure, reason, message string) (ctrl.Result, error) {
	er.Status.Phase = erasurePhaseFailed
	status.SetCondition(&er.Status.Conditions, ConditionReady, metav1.ConditionFalse, reason, message, er.Generation)
	status.SetCondition(&er.Status.Conditions, conditionErasureComplete, metav1.ConditionFalse, reason, message, er.Generation)
	r.Recorder.Eventf(er, nil, corev1.EventTypeWarning, "ErasureFailed", "Erase", "%s", message)
	if err := r.Status().Update(ctx, er); err != nil {
		return ctrl.Result{}, fmt.Errorf("update status for ClusterErasure %s: %w", er.Name, err)
	}
	return ctrl.Result{}, nil
}

// complete records the terminal success.
func (r *ClusterErasureReconciler) complete(ctx context.Context, er *cbv1.ClusterErasure,
	loc *cbv1.ClusterBackupLocation, message string,
) (ctrl.Result, error) {
	justCompleted := er.Status.Phase != erasurePhaseCompleted
	er.Status.Phase = erasurePhaseCompleted
	status.SetCondition(&er.Status.Conditions, ConditionReady, metav1.ConditionTrue, "Erased", message, er.Generation)
	status.SetCondition(&er.Status.Conditions, conditionErasureComplete, metav1.ConditionTrue, "Erased", message, er.Generation)
	r.Recorder.Eventf(er, nil, corev1.EventTypeNormal, "ErasureCompleted", "Erase", "%s", message)
	if err := r.Status().Update(ctx, er); err != nil {
		return ctrl.Result{}, fmt.Errorf("update status for ClusterErasure %s: %w", er.Name, err)
	}
	// The counters go up only once the Completed phase is durable, and only on the transition
	// into it. They are the compliance record's metric half — "how much was physically destroyed
	// on this location" — and they are true counters for the plainest of reasons: the snapshots
	// they count no longer exist to be counted again.
	//
	// A Completed erasure that matched nothing adds zero, which the recorder skips: it is a
	// legitimate outcome, not an erasure event.
	if justCompleted {
		clusterID := ""
		if loc != nil {
			clusterID = loc.Spec.ClusterID
		}
		metrics.RecordErasureCompleted(er.Spec.LocationRef.Name, clusterID,
			er.Status.SnapshotsForgotten, er.Status.ReclaimedBytes)
	}
	return ctrl.Result{}, nil
}

// SetupWithManager registers this reconciler.
func (r *ClusterErasureReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&cbv1.ClusterErasure{}).
		Named("clustererasure").
		Complete(r)
}

// erasureConfirmationFor renders the string an operator must type back (R23): the target's own
// identity. It mirrors the admission policy's expression exactly — if the two ever disagree,
// admission would admit a value this controller then rejects, which reads as a bug in the CR.
func erasureConfirmationFor(t cbv1.ErasureTarget) string {
	switch {
	case t.Namespace != "" && t.PVC != "":
		return t.Namespace + "/" + t.PVC
	case t.Namespace != "":
		return t.Namespace
	default:
		return t.Tenant
	}
}

// erasurePruneMaxRepackSize reads the location's prune repack cap, tolerating an absent
// spec.maintenance block — which is the DEFAULT, not an edge case: nothing about running an
// erasure requires the location to have configured scheduled maintenance at all.
func erasurePruneMaxRepackSize(loc *cbv1.ClusterBackupLocation) string {
	if loc.Spec.Maintenance == nil {
		return ""
	}
	return loc.Spec.Maintenance.PruneMaxRepackSize
}

// erasureResourceName names the per-step mover Job and its creds Secret.
func erasureResourceName(erasure, step string) string {
	return "er-" + erasure + "-" + step
}

// erasureCountJobName names the snapshots Job that measures the scope. Distinct from the step
// names so a count never collides with (or re-adopts) a forget.
func erasureCountJobName(erasure string) string {
	return "er-" + erasure + "-count"
}
