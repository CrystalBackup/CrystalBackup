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
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/client/secrets"
	"github.com/CrystalBackup/CrystalBackup/internal/mover"
	"github.com/CrystalBackup/CrystalBackup/internal/nsselector"
	"github.com/CrystalBackup/CrystalBackup/internal/repo/queue"
	"github.com/CrystalBackup/CrystalBackup/internal/status"
)

// syncJobPrefixCluster distinguishes a cluster-plane sync's Jobs and Secrets from a namespace-plane
// sync of the same name. Both land in the operator namespace, which has nothing else to tell them
// apart.
const syncJobPrefixCluster = "cbes"

// ClusterBackupExternalSyncReconciler replicates the SHARED DR repository to a secondary
// ClusterBackupLocation (R28, adr/0013).
//
// Its job is endpoint resolution; the copy itself is syncDriver, shared with the namespace plane.
// What is specific here is the trust position: both endpoints are admin-owned cluster locations,
// both keys are platform DEKs wrapped by the cluster KEK, and both credential Secrets live in the
// operator namespace. There is no tenant boundary to hold inside one repository — the whole shared
// repo, or a namespace-tagged slice of it, moves to a second admin-owned location.
//
// The one thing it must never do is copy a location onto itself. Admission compares the two refs by
// NAME; this controller compares the resolved REPOSITORIES, because two differently-named locations
// can address the same bucket, prefix and cluster ID.
type ClusterBackupExternalSyncReconciler struct {
	client.Client
	Scheme            *runtime.Scheme
	Secrets           *secrets.ByNameReader
	OperatorNamespace string
	Recorder          events.EventRecorder

	driver *syncDriver
}

// NewClusterBackupExternalSyncReconciler builds the reconciler and its shared driver.
func NewClusterBackupExternalSyncReconciler(
	c client.Client, scheme *runtime.Scheme, secretsReader *secrets.ByNameReader,
	q *queue.Manager, lister FilteredSnapshotLister, operatorNamespace, syncImage string,
	cl clock.PassiveClock, recorder events.EventRecorder,
) *ClusterBackupExternalSyncReconciler {
	deps := repoMaintenanceDeps{
		Client: c, Secrets: secretsReader, OperatorNamespace: operatorNamespace,
	}
	return &ClusterBackupExternalSyncReconciler{
		Client: c, Scheme: scheme, Secrets: secretsReader,
		OperatorNamespace: operatorNamespace, Recorder: recorder,
		driver: newSyncDriver(deps, q, lister, syncImage, cl),
	}
}

// +kubebuilder:rbac:groups=crystalbackup.io,resources=clusterbackupexternalsyncs,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=crystalbackup.io,resources=clusterbackupexternalsyncs/status,verbs=get;update;patch

// Reconcile drives one ClusterBackupExternalSync.
func (r *ClusterBackupExternalSyncReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var cs cbv1.ClusterBackupExternalSync
	if err := r.Get(ctx, req.NamespacedName, &cs); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	view := syncStatusView{
		Phase:           &cs.Status.Phase,
		LastSuccessTime: &cs.Status.LastSuccessTime,
		SnapshotsCopied: &cs.Status.SnapshotsCopied,
		BytesCopied:     &cs.Status.BytesCopied,
		LagSnapshots:    &cs.Status.LagSnapshots,
		Conditions:      &cs.Status.Conditions,
		Generation:      cs.Generation,
	}
	rec := syncRecorder{emit: func(eventType, reason, messageFmt string, args ...any) {
		r.Recorder.Eventf(&cs, nil, eventType, reason, "Sync", messageFmt, args...)
	}}
	key := "cbes/" + cs.Name

	// A run already in flight keeps going regardless of pause or schedule: pausing a sync means
	// "start no NEW copies", not "abandon the one moving data right now", and abandoning it would
	// leave a half-populated destination with no record of how far it got.
	if r.driver.hasInflight(key) {
		res, err := r.driver.drive(ctx, key, nil, view, syncJobPrefixCluster, cs.Name, rec)
		return r.write(ctx, &cs, res, err)
	}

	if cs.Spec.Paused {
		*view.Phase = syncPhasePending
		setSyncCondition(view, metav1.ConditionFalse, "Paused", "sync is paused; no new copy is started")
		return r.write(ctx, &cs, ctrl.Result{}, nil)
	}

	run, res, done, err := r.resolveRun(ctx, &cs, view, rec)
	if done {
		return r.write(ctx, &cs, res, err)
	}

	act := syncSchedule(cs.Spec.Schedule, cs.Spec.Timezone, cs.Status.LastSuccessTime, r.driver.Clock.Now())
	switch {
	case act.Err != nil:
		// Spec-static: surface it and wait for an edit rather than hot-looping on a cron string
		// that will never parse.
		*view.Phase = syncPhaseFailed
		setSyncCondition(view, metav1.ConditionFalse, "InvalidSchedule", act.Err.Error())
		return r.write(ctx, &cs, ctrl.Result{}, nil)
	case !act.Due:
		return r.write(ctx, &cs, idleSyncResult(view, act, cs.Spec.Schedule), nil)
	}

	res, err = r.driver.drive(ctx, key, run, view, syncJobPrefixCluster, cs.Name, rec)
	return r.write(ctx, &cs, res, err)
}

// resolveRun resolves both endpoints. done=true means the caller should write status and stop —
// the sync is not runnable this pass.
func (r *ClusterBackupExternalSyncReconciler) resolveRun(ctx context.Context,
	cs *cbv1.ClusterBackupExternalSync, view syncStatusView, rec syncRecorder,
) (run *syncRun, res ctrl.Result, done bool, err error) {
	source, res, done, err := r.endpoint(ctx, cs.Spec.SourceLocationRef.Name, mover.SyncRemoteSource, "source", view)
	if done {
		return nil, res, true, err
	}
	dest, res, done, err := r.endpoint(ctx, cs.Spec.DestinationLocationRef.Name, mover.SyncRemoteDest, "destination", view)
	if done {
		return nil, res, true, err
	}

	if sameEndpoint(source, dest) {
		// Not merely useless: restic would open one repository twice, as source and destination,
		// and contend with its own lock. Admission compares names; this compares what they resolve
		// to, which is the identity that matters.
		*view.Phase = syncPhaseFailed
		setSyncCondition(view, metav1.ConditionFalse, "SameRepository",
			fmt.Sprintf("source %s and destination %s resolve to the same repository %q; a sync must have two",
				source.Binding.Describe(), dest.Binding.Describe(), source.Repo.Name))
		return nil, ctrl.Result{}, true, nil
	}

	namespaces, err := r.resolveSelection(ctx, cs)
	if err != nil {
		*view.Phase = syncPhaseFailed
		setSyncCondition(view, metav1.ConditionFalse, "InvalidSelection", err.Error())
		return nil, ctrl.Result{}, true, nil
	}

	mode, forced := effectiveSyncMode(cs.Spec.Mode, dest.Binding)
	if forced {
		rec.event(corev1.EventTypeWarning, "ModeForced",
			"destination %s is Immutable: object lock forbids deletion, so this sync runs AppendOnly "+
				"regardless of spec.mode", dest.Binding.Describe())
	}
	return &syncRun{Source: source, Dest: dest, Namespaces: namespaces, Mode: mode}, ctrl.Result{}, false, nil
}

// endpoint resolves one ClusterBackupLocation into a usable syncEndpoint.
func (r *ClusterBackupExternalSyncReconciler) endpoint(ctx context.Context, name, remote, side string,
	view syncStatusView,
) (*syncEndpoint, ctrl.Result, bool, error) {
	var loc cbv1.ClusterBackupLocation
	if err := r.Get(ctx, client.ObjectKey{Name: name}, &loc); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, r.park(view, "LocationNotFound",
				fmt.Sprintf("the %s ClusterBackupLocation %q does not exist", side, name)), true, nil
		}
		return nil, ctrl.Result{}, true, fmt.Errorf("get ClusterBackupLocation %s: %w", name, err)
	}
	binding := bindingFromClusterLocation(&loc, r.OperatorNamespace)

	e := &syncEndpoint{Remote: remote, Binding: binding}
	var repo cbv1.BackupRepository
	if err := r.Get(ctx, client.ObjectKey{Name: loc.Name}, &repo); err == nil {
		e.Repo = &repo
	} else if !apierrors.IsNotFound(err) {
		return nil, ctrl.Result{}, true, fmt.Errorf("get BackupRepository %s: %w", loc.Name, err)
	}

	// The platform DEK, unwrapped with the cluster KEK. Resolved per reconcile and never cached:
	// it exists in memory only long enough to reach the per-Job Secret.
	if dek, reason, message, ok := resolvePlatformDEKCommon(ctx, r.Client, r.Secrets, r.OperatorNamespace, &loc); ok {
		e.Password = dek
	} else {
		return nil, r.park(view, reason, fmt.Sprintf("%s location %s: %s", side, binding.Describe(), message)), true, nil
	}

	if reason, message, ok := syncEndpointReady(e, side); !ok {
		return nil, r.park(view, reason, message), true, nil
	}
	return e, ctrl.Result{}, false, nil
}

// resolveSelection turns spec.selection into the namespace list the copy is scoped to. An omitted
// selection means the whole repository.
func (r *ClusterBackupExternalSyncReconciler) resolveSelection(ctx context.Context,
	cs *cbv1.ClusterBackupExternalSync,
) ([]string, error) {
	if cs.Spec.Selection == nil || cs.Spec.Selection.Namespaces == nil {
		return nil, nil
	}
	var namespaces corev1.NamespaceList
	if err := r.List(ctx, &namespaces); err != nil {
		return nil, fmt.Errorf("list namespaces to resolve the selection: %w", err)
	}
	matched, err := nsselector.Match(namespaces.Items, *cs.Spec.Selection.Namespaces)
	if err != nil {
		return nil, err
	}
	if len(matched) == 0 {
		// An empty match must NOT degrade to "the whole repository" — that is the difference
		// between copying nothing and copying every tenant's data to a secondary the selection was
		// written to exclude them from.
		return nil, fmt.Errorf("spec.selection.namespaces matched no namespace; refusing to fall back to a whole-repository sync")
	}
	return matched, nil
}

// park records a non-terminal "waiting for" state.
func (r *ClusterBackupExternalSyncReconciler) park(view syncStatusView, reason, message string) ctrl.Result {
	*view.Phase = syncPhasePending
	setSyncCondition(view, metav1.ConditionFalse, reason, message)
	return ctrl.Result{RequeueAfter: syncRequeueInterval}
}

// write persists status once per reconcile.
func (r *ClusterBackupExternalSyncReconciler) write(ctx context.Context, cs *cbv1.ClusterBackupExternalSync,
	res ctrl.Result, err error,
) (ctrl.Result, error) {
	if err != nil {
		return ctrl.Result{}, err
	}
	if uErr := r.Status().Update(ctx, cs); uErr != nil {
		return ctrl.Result{}, fmt.Errorf("update status for ClusterBackupExternalSync %s: %w", cs.Name, uErr)
	}
	return res, nil
}

// SetupWithManager registers this reconciler.
func (r *ClusterBackupExternalSyncReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&cbv1.ClusterBackupExternalSync{}).
		Named("clusterbackupexternalsync").
		Complete(r)
}

// idleSyncResult records "nothing to do now" and computes the wait until the next activation. It is
// shared by both planes: a schedule that is not due looks identical on either.
func idleSyncResult(view syncStatusView, act syncActivation, expr string) ctrl.Result {
	if expr == "" {
		// On-demand and already run. Nothing re-triggers it, so do not requeue at all — a
		// perpetual wake-up on an object that will never act again is pure noise.
		status.SetCondition(view.Conditions, ConditionReady, metav1.ConditionTrue, "Completed",
			"on-demand sync completed; set spec.schedule to repeat it", view.Generation)
		return ctrl.Result{}
	}
	status.SetCondition(view.Conditions, ConditionReady, metav1.ConditionTrue, "Scheduled",
		"sync scheduled; next copy at "+act.NextFire.UTC().Format(time.RFC3339), view.Generation)
	return ctrl.Result{RequeueAfter: clampDuration(act.Wait, syncMinRequeue, syncMaxRequeue)}
}
