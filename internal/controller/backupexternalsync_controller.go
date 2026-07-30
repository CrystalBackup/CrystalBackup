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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
	"github.com/CrystalBackup/CrystalBackup/internal/client/secrets"
	"github.com/CrystalBackup/CrystalBackup/internal/keys"
	"github.com/CrystalBackup/CrystalBackup/internal/mover"
	"github.com/CrystalBackup/CrystalBackup/internal/repo/queue"
)

// syncJobPrefixNamespaced distinguishes a namespace-plane sync's Jobs and Secrets from a
// cluster-plane sync's. Both land in the operator namespace.
const syncJobPrefixNamespaced = "bes"

// BackupExternalSyncReconciler copies a namespace's own backups from one BackupLocation to another
// (R28, adr/0013) — the namespace plane's half of external sync.
//
// The tenancy stance is STRUCTURAL, exactly as it is for Restore: both locations are resolved in
// the CR's OWN namespace, and there is no field that could name another one. Not "validated to be
// in the same namespace" — there is nowhere to write a different namespace. That is what makes the
// copy safe to run with the operator's cluster-wide credentials: a user cannot express a sync out
// of somebody else's data, so the controller never has to decide whether they may.
//
// Both keys are the TENANT's own restic passwords, resolved through UserKeyManager. The platform
// DEK is not involved on either side, so a client's secondary stays a client's — the same
// property that makes their primary opaque to the platform.
//
// There is no selection field here, and its absence is the design: the whole repository IS the
// namespace's data. A cluster sync needs spec.selection because its source holds every tenant;
// this one does not, and adding a narrowing knob would only invite a user to believe the omission
// meant something broader.
type BackupExternalSyncReconciler struct {
	client.Client
	Scheme            *runtime.Scheme
	Secrets           *secrets.ByNameReader
	UserKeys          *keys.UserKeyManager
	OperatorNamespace string
	Recorder          events.EventRecorder

	driver *syncDriver
}

// NewBackupExternalSyncReconciler builds the reconciler and its shared driver.
// moverImage is required alongside syncImage; see the cluster-plane constructor for why the pair is
// not redundant (the copy needs rclone, Mirror's forget does not and runs the mover image).
func NewBackupExternalSyncReconciler(
	c client.Client, scheme *runtime.Scheme, secretsReader *secrets.ByNameReader,
	userKeys *keys.UserKeyManager, q *queue.Manager, lister FilteredSnapshotLister,
	operatorNamespace, moverImage, syncImage string, cl clock.PassiveClock, recorder events.EventRecorder,
) *BackupExternalSyncReconciler {
	deps := repoMaintenanceDeps{
		Client: c, Secrets: secretsReader, OperatorNamespace: operatorNamespace, MoverImage: moverImage,
	}
	return &BackupExternalSyncReconciler{
		Client: c, Scheme: scheme, Secrets: secretsReader, UserKeys: userKeys,
		OperatorNamespace: operatorNamespace, Recorder: recorder,
		driver: newSyncDriver(deps, q, lister, syncImage, cl),
	}
}

// +kubebuilder:rbac:groups=crystalbackup.io,resources=backupexternalsyncs,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=crystalbackup.io,resources=backupexternalsyncs/status,verbs=get;update;patch

// Reconcile drives one BackupExternalSync.
func (r *BackupExternalSyncReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var bs cbv1.BackupExternalSync
	if err := r.Get(ctx, req.NamespacedName, &bs); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	view := syncStatusView{
		Phase:           &bs.Status.Phase,
		LastSuccessTime: &bs.Status.LastSuccessTime,
		SnapshotsCopied: &bs.Status.SnapshotsCopied,
		BytesCopied:     &bs.Status.BytesCopied,
		LagSnapshots:    &bs.Status.LagSnapshots,
		Conditions:      &bs.Status.Conditions,
		Generation:      bs.Generation,
	}
	rec := syncRecorder{emit: func(eventType, reason, messageFmt string, args ...any) {
		r.Recorder.Eventf(&bs, nil, eventType, reason, "Sync", messageFmt, args...)
	}}
	key := "bes/" + bs.Namespace + "/" + bs.Name

	// An in-flight copy finishes on its own terms; see the cluster-plane controller for why.
	ident := syncIdentity{Name: bs.Name, Scope: apiconst.OriginNamespace, Namespace: bs.Namespace}

	if r.driver.hasInflight(key) {
		res, err := r.driver.drive(ctx, key, nil, view, syncJobPrefixNamespaced, bs.Name, rec, ident)
		return r.write(ctx, &bs, res, err)
	}

	// Paused: start no NEW copy, and leave lastSuccessTime, snapshotsCopied and lagSnapshots exactly
	// where they are. The check sits AFTER hasInflight for the same reason it does on the cluster
	// plane — a copy already moving data finishes on its own terms, because abandoning it leaves a
	// half-populated destination with no record of how far it got.
	//
	// Note that endpoints are deliberately NOT resolved here: a sync is very often paused precisely
	// because one of its locations is being retired, and parking on LocationNotFound would overwrite
	// the pause with a failure the operator has already decided to stop caring about.
	if bs.Spec.Paused {
		*view.Phase = syncPhasePending
		setSyncCondition(view, metav1.ConditionFalse, "Paused", "sync is paused; no new copy is started")
		return r.write(ctx, &bs, ctrl.Result{}, nil)
	}

	run, res, done, err := r.resolveRun(ctx, &bs, view, rec)
	if done {
		return r.write(ctx, &bs, res, err)
	}

	act := syncSchedule(bs.Spec.Schedule, bs.Spec.Timezone, bs.Status.LastSuccessTime, r.driver.Clock.Now())
	switch {
	case act.Err != nil:
		*view.Phase = syncPhaseFailed
		setSyncCondition(view, metav1.ConditionFalse, "InvalidSchedule", act.Err.Error())
		return r.write(ctx, &bs, ctrl.Result{}, nil)
	case !act.Due:
		return r.write(ctx, &bs, idleSyncResult(view, act, bs.Spec.Schedule), nil)
	}

	res, err = r.driver.drive(ctx, key, run, view, syncJobPrefixNamespaced, bs.Name, rec, ident)
	return r.write(ctx, &bs, res, err)
}

// resolveRun resolves both endpoints, both within the CR's own namespace.
func (r *BackupExternalSyncReconciler) resolveRun(ctx context.Context, bs *cbv1.BackupExternalSync,
	view syncStatusView, rec syncRecorder,
) (run *syncRun, res ctrl.Result, done bool, err error) {
	source, res, done, err := r.endpoint(ctx, bs.Namespace, bs.Spec.SourceLocationRef.Name,
		mover.SyncRemoteSource, "source", view)
	if done {
		return nil, res, true, err
	}
	dest, res, done, err := r.endpoint(ctx, bs.Namespace, bs.Spec.DestinationLocationRef.Name,
		mover.SyncRemoteDest, "destination", view)
	if done {
		return nil, res, true, err
	}

	if sameEndpoint(source, dest) {
		*view.Phase = syncPhaseFailed
		setSyncCondition(view, metav1.ConditionFalse, "SameRepository",
			fmt.Sprintf("source %s and destination %s resolve to the same repository %q; a sync must have two",
				source.Binding.Describe(), dest.Binding.Describe(), source.Repo.Name))
		return nil, ctrl.Result{}, true, nil
	}

	mode, forced := effectiveSyncMode(bs.Spec.Mode, dest.Binding)
	if forced {
		rec.event(corev1.EventTypeWarning, "ModeForced",
			"destination %s is Immutable: object lock forbids deletion, so this sync runs AppendOnly "+
				"regardless of spec.mode", dest.Binding.Describe())
	}
	// No Namespaces: a namespaced location's repository holds this namespace's data and nothing
	// else, so the whole repository IS the scope.
	return &syncRun{Source: source, Dest: dest, Mode: mode}, ctrl.Result{}, false, nil
}

// endpoint resolves one BackupLocation IN THE CR'S OWN NAMESPACE into a usable syncEndpoint.
//
// namespace is the CR's, passed rather than taken from a spec field, because no spec field exists:
// this is where the structural confinement is enforced, by construction rather than by check.
func (r *BackupExternalSyncReconciler) endpoint(ctx context.Context, namespace, name, remote, side string,
	view syncStatusView,
) (*syncEndpoint, ctrl.Result, bool, error) {
	var loc cbv1.BackupLocation
	if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &loc); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, r.park(view, "LocationNotFound",
				fmt.Sprintf("the %s BackupLocation %q does not exist in namespace %q", side, name, namespace)), true, nil
		}
		return nil, ctrl.Result{}, true, fmt.Errorf("get BackupLocation %s/%s: %w", namespace, name, err)
	}
	binding := bindingFromNamespacedLocation(&loc)

	e := &syncEndpoint{Remote: remote, Binding: binding}
	repoName := namespacedRepositoryName(namespace, loc.Name)
	var repo cbv1.BackupRepository
	if err := r.Get(ctx, client.ObjectKey{Name: repoName}, &repo); err == nil {
		e.Repo = &repo
	} else if !apierrors.IsNotFound(err) {
		return nil, ctrl.Result{}, true, fmt.Errorf("get BackupRepository %s: %w", repoName, err)
	}

	// The USER's own key on both sides. A dangling password reference is an error, never a
	// fallback to a generated one — EnsureUserPassword enforces that, and it matters twice over
	// here: a generated password on the destination would write a secondary the user's own Secret
	// cannot open, which is a backup that looks fine until it is needed.
	password, err := r.UserKeys.EnsureUserPassword(ctx, namespace, loc.Name, binding.PasswordSecretRef)
	if err != nil {
		return nil, r.park(view, "KeyUnavailable",
			fmt.Sprintf("%s location %s: %v", side, binding.Describe(), err)), true, nil
	}
	e.Password = password

	if reason, message, ok := syncEndpointReady(e, side); !ok {
		return nil, r.park(view, reason, message), true, nil
	}
	return e, ctrl.Result{}, false, nil
}

// park records a non-terminal "waiting for" state.
func (r *BackupExternalSyncReconciler) park(view syncStatusView, reason, message string) ctrl.Result {
	*view.Phase = syncPhasePending
	setSyncCondition(view, metav1.ConditionFalse, reason, message)
	return ctrl.Result{RequeueAfter: syncRequeueInterval}
}

// write persists status once per reconcile.
func (r *BackupExternalSyncReconciler) write(ctx context.Context, bs *cbv1.BackupExternalSync,
	res ctrl.Result, err error,
) (ctrl.Result, error) {
	if err != nil {
		return ctrl.Result{}, err
	}
	if uErr := r.Status().Update(ctx, bs); uErr != nil {
		return ctrl.Result{}, fmt.Errorf("update status for BackupExternalSync %s/%s: %w",
			bs.Namespace, bs.Name, uErr)
	}
	return res, nil
}

// SetupWithManager registers this reconciler.
func (r *BackupExternalSyncReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&cbv1.BackupExternalSync{}).
		Named("backupexternalsync").
		Complete(r)
}
