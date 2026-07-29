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
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
	"github.com/CrystalBackup/CrystalBackup/internal/keys"
	"github.com/CrystalBackup/CrystalBackup/internal/status"
)

// BackupLocationReconciler is the namespace plane's location controller: the tenant-facing twin
// of ClusterBackupLocationReconciler, and deliberately shaped like it (same condition names,
// same fail-fast ordering, same "provision the object, let the BackupRepository controller run
// restic init" division of labour).
//
// Three things genuinely differ, and each is a tenancy property rather than a style choice:
//
//   - THE KEY IS THE USER'S. There is no KEK and no wrapping: the repository password is the
//     user's own Secret, or one the operator generates IN THEIR NAMESPACE (adr/0004 §2). What
//     this controller validates is therefore not "can I parse the platform KEK" but "does the
//     user's key resolve" — same condition, different meaning.
//   - THE REPOSITORY CANNOT BE OWNED. BackupRepository is cluster-scoped and this location is
//     namespaced, so an ownerReference would be read as dangling and Kubernetes would delete the
//     repository out from under it. The link is a pair of labels, and the lifecycle is explicit
//     (see ensureRepository and finalize).
//   - THE CLUSTER ID IS STICKY. spec.clusterID may be omitted, in which case it defaults from
//     the default ClusterBackupLocation — but only ONCE, recorded in status. Re-deriving it every
//     pass would let an admin changing the default silently re-point every tenant repository.
type BackupLocationReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Prober is the reachability seam, shared with the cluster-plane controller: httpS3Prober in
	// production, a stub in envtest.
	Prober S3Prober
	// UserKeys resolves (or generates) the tenant's repository password. Its client reads and
	// writes Secrets in TENANT namespaces, which is safe only because the manager client bypasses
	// its cache for Secrets (cmd/main.go) — a cached read here would stand up the cluster-wide
	// Secret informer invariant I3 forbids.
	UserKeys *keys.UserKeyManager
	Recorder events.EventRecorder
}

// backupLocationPhaseWaitingClusterID is the phase of a location that declared no clusterID and
// found no default ClusterBackupLocation to inherit one from. Like locationPhaseInitializing it
// is NOT Degraded: on a fresh cluster the admin simply has not created their DR location yet,
// and the tenant has nothing to fix.
const backupLocationPhaseWaitingClusterID = "WaitingForClusterID"

// +kubebuilder:rbac:groups=crystalbackup.io,resources=backuplocations,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=crystalbackup.io,resources=backuplocations/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=crystalbackup.io,resources=backuplocations/finalizers,verbs=update
// +kubebuilder:rbac:groups=crystalbackup.io,resources=backuprepositories,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=crystalbackup.io,resources=clusterbackuplocations,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;create
// +kubebuilder:rbac:groups="";events.k8s.io,resources=events,verbs=create;patch

// Reconcile drives one BackupLocation: finalizer ensured, effective cluster ID resolved and
// pinned, the user's key resolved, the endpoint probed, its BackupRepository ensured, and Ready
// rolled up from all of it. Encryption and reachability are fail-fast for the same reason they
// are on the cluster plane — provisioning a repository for a location whose key or storage is
// not yet trustworthy is premature.
func (r *BackupLocationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var loc cbv1.BackupLocation
	if err := r.Get(ctx, req.NamespacedName, &loc); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("get BackupLocation %s/%s: %w", req.Namespace, req.Name, err)
	}

	if !loc.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, &loc)
	}

	if controllerutil.AddFinalizer(&loc, apiconst.FinalizerLocation) {
		if err := r.Update(ctx, &loc); err != nil {
			return ctrl.Result{}, fmt.Errorf("add finalizer to BackupLocation %s/%s: %w", loc.Namespace, loc.Name, err)
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// The effective cluster ID composes the repository path, so nothing downstream can run
	// without it. Resolved and PINNED here, before any of the checks that could provision.
	resolved, err := r.resolveClusterID(ctx, &loc)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !resolved {
		status.SetCondition(&loc.Status.Conditions, ConditionReady, metav1.ConditionFalse, "NoClusterID",
			"spec.clusterID is unset and no default ClusterBackupLocation declares one to inherit; "+
				"set spec.clusterID or ask an administrator for a default location", loc.Generation)
		loc.Status.Phase = backupLocationPhaseWaitingClusterID
		if err := r.Status().Update(ctx, &loc); err != nil {
			return ctrl.Result{}, fmt.Errorf("update status for BackupLocation %s/%s: %w", loc.Namespace, loc.Name, err)
		}
		return ctrl.Result{RequeueAfter: shortRequeueInterval}, nil
	}

	if encryptionValid := r.validateEncryption(ctx, &loc); !encryptionValid {
		status.SetCondition(&loc.Status.Conditions, ConditionReady, metav1.ConditionFalse, "EncryptionInvalid",
			"the repository key is not available; see condition EncryptionValid", loc.Generation)
		loc.Status.Phase = locationPhaseDegraded
		if err := r.Status().Update(ctx, &loc); err != nil {
			return ctrl.Result{}, fmt.Errorf("update status for BackupLocation %s/%s: %w", loc.Namespace, loc.Name, err)
		}
		return ctrl.Result{RequeueAfter: shortRequeueInterval}, nil
	}

	if reachable := r.checkReachability(ctx, &loc); !reachable {
		status.SetCondition(&loc.Status.Conditions, ConditionReady, metav1.ConditionFalse, "Unreachable",
			"object storage endpoint is not reachable; see condition Reachable", loc.Generation)
		loc.Status.Phase = locationPhaseDegraded
		if err := r.Status().Update(ctx, &loc); err != nil {
			return ctrl.Result{}, fmt.Errorf("update status for BackupLocation %s/%s: %w", loc.Namespace, loc.Name, err)
		}
		return ctrl.Result{RequeueAfter: shortRequeueInterval}, nil
	}

	if err := r.ensureRepository(ctx, &loc); err != nil {
		log.Error(err, "ensure BackupRepository")
		status.SetCondition(&loc.Status.Conditions, ConditionReady, metav1.ConditionFalse, "RepositoryUnavailable",
			err.Error(), loc.Generation)
		loc.Status.Phase = locationPhaseDegraded
		if serr := r.Status().Update(ctx, &loc); serr != nil {
			log.Error(serr, "Persist status before returning the repository error failed")
		}
		return ctrl.Result{RequeueAfter: shortRequeueInterval}, nil
	}

	repoInitialized, err := r.repositoryInitialized(ctx, &loc)
	if err != nil {
		return ctrl.Result{}, err
	}

	reachable := status.IsConditionTrue(loc.Status.Conditions, ConditionReachable)
	encryptionValid := status.IsConditionTrue(loc.Status.Conditions, ConditionEncryptionValid)
	provisioned := loc.Status.RepositoryRef != ""
	requeue := periodicRequeueInterval

	switch {
	case reachable && encryptionValid && provisioned && repoInitialized:
		status.SetCondition(&loc.Status.Conditions, ConditionReady, metav1.ConditionTrue, "Ready",
			"location is reachable, its key is available, and the repository is initialized and ready to accept backups",
			loc.Generation)
		loc.Status.Phase = "Ready"
	case reachable && encryptionValid && provisioned:
		status.SetCondition(&loc.Status.Conditions, ConditionReady, metav1.ConditionFalse, "RepositoryInitializing",
			fmt.Sprintf("repository provisioned; initialization is still in progress — see BackupRepository %q "+
				"(condition Initialized) for its progress", loc.Status.RepositoryRef), loc.Generation)
		loc.Status.Phase = locationPhaseInitializing
		requeue = shortRequeueInterval
	default:
		status.SetCondition(&loc.Status.Conditions, ConditionReady, metav1.ConditionFalse, "NotReady",
			"location is not ready", loc.Generation)
		loc.Status.Phase = locationPhaseDegraded
	}

	if err := r.Status().Update(ctx, &loc); err != nil {
		return ctrl.Result{}, fmt.Errorf("update status for BackupLocation %s/%s: %w", loc.Namespace, loc.Name, err)
	}
	return ctrl.Result{RequeueAfter: requeue}, nil
}

// resolveClusterID pins the effective cluster ID into status, once. It reports resolved=false
// (with a nil error) when the location declared none AND no default ClusterBackupLocation offers
// one — a transient, admin-fixable state, not a fault of the tenant's.
//
// Once status.ClusterID is set it is never recomputed. That is the whole point: spec.clusterID is
// immutable so an edit cannot re-point a location at a different repository, and re-deriving the
// DEFAULTED value on every pass would reopen exactly that hole from the other side — an admin
// changing which ClusterBackupLocation is default would silently move every tenant repository
// that inherited from it, abandoning the snapshots already written under the old path.
func (r *BackupLocationReconciler) resolveClusterID(ctx context.Context, loc *cbv1.BackupLocation) (bool, error) {
	if loc.Status.ClusterID != "" {
		return true, nil
	}
	if loc.Spec.ClusterID != "" {
		loc.Status.ClusterID = loc.Spec.ClusterID
		return true, nil
	}
	inherited, err := defaultClusterID(ctx, r.Client)
	if err != nil {
		return false, err
	}
	if inherited == "" {
		return false, nil
	}
	loc.Status.ClusterID = inherited
	r.Recorder.Eventf(loc, nil, corev1.EventTypeNormal, "ClusterIDDefaulted", "ResolveClusterID",
		"inherited cluster ID %q from the default ClusterBackupLocation; it is pinned in status and "+
			"will not follow a later change of default", inherited)
	return true, nil
}

// validateEncryption resolves the tenant's repository password and reports it on
// ConditionEncryptionValid. On the namespace plane "valid encryption" means the key EXISTS and is
// usable — either the Secret the user referenced, or the one the operator generated beside it.
//
// The plaintext password is discarded immediately: this method answers a yes/no question, and the
// only component that needs the value is the mover Job the BackupRepository controller builds.
func (r *BackupLocationReconciler) validateEncryption(ctx context.Context, loc *cbv1.BackupLocation) bool {
	ref := passwordSecretRefName(loc)
	if _, err := r.UserKeys.EnsureUserPassword(ctx, loc.Namespace, loc.Name, ref); err != nil {
		reason := "PasswordUnavailable"
		if ref != "" {
			reason = "PasswordSecretUnusable"
		}
		// keys' errors name the Secret and never the material, so folding one in verbatim is safe.
		status.SetCondition(&loc.Status.Conditions, ConditionEncryptionValid, metav1.ConditionFalse, reason,
			err.Error(), loc.Generation)
		return false
	}
	if ref != "" {
		status.SetCondition(&loc.Status.Conditions, ConditionEncryptionValid, metav1.ConditionTrue, "UserKey",
			fmt.Sprintf("repository password read from the referenced Secret %s/%s", loc.Namespace, ref), loc.Generation)
	} else {
		status.SetCondition(&loc.Status.Conditions, ConditionEncryptionValid, metav1.ConditionTrue, "GeneratedKey",
			fmt.Sprintf("repository password generated and stored in Secret %s/%s — it is YOUR key: back it up, "+
				"because without it these backups cannot be read, by you or by the platform",
				loc.Namespace, keys.UserPasswordSecretName(loc.Name)), loc.Generation)
	}
	return true
}

// checkReachability probes Spec.S3 through r.Prober and sets ConditionReachable accordingly.
func (r *BackupLocationReconciler) checkReachability(ctx context.Context, loc *cbv1.BackupLocation) bool {
	if err := r.Prober.Reachable(ctx, loc.Spec.S3); err != nil {
		status.SetCondition(&loc.Status.Conditions, ConditionReachable, metav1.ConditionFalse, "Unreachable",
			fmt.Sprintf("probe %s: %v", loc.Spec.S3.Endpoint, err), loc.Generation)
		return false
	}
	status.SetCondition(&loc.Status.Conditions, ConditionReachable, metav1.ConditionTrue, "Reachable",
		fmt.Sprintf("endpoint %s responded", loc.Spec.S3.Endpoint), loc.Generation)
	return true
}

// ensureRepository ensures the cluster-scoped BackupRepository backing loc exists, named
// deterministically as "<namespace>--<name>" and carrying the back-link labels that stand in for
// the ownerReference a namespaced object cannot place on a cluster-scoped one.
//
// The ADOPTION GUARD is the part that matters. The name mapping is not injective — namespace and
// object names may both contain "--", so ("a--b", "c") and ("a", "b--c") collide — and a
// cluster-scoped name space is shared by every tenant. So a pre-existing repository of that name
// is adopted ONLY if its labels say it already belongs to this exact location. Anything else
// (another tenant's location, or a cluster-plane repository) is refused with a message naming the
// conflict, because the alternative is writing one tenant's backups into another tenant's
// repository — under the other tenant's key, in the other tenant's bucket.
func (r *BackupLocationReconciler) ensureRepository(ctx context.Context, loc *cbv1.BackupLocation) error {
	name := namespacedRepositoryName(loc.Namespace, loc.Name)

	var existing cbv1.BackupRepository
	err := r.Get(ctx, client.ObjectKey{Name: name}, &existing)
	switch {
	case err == nil:
		if ns, ln := existing.Labels[apiconst.LabelNamespace], existing.Labels[apiconst.LabelLocation]; ns != loc.Namespace || ln != loc.Name {
			return fmt.Errorf("BackupRepository %q already exists and belongs to a different location "+
				"(labels namespace=%q location=%q); rename this BackupLocation to claim a distinct repository",
				name, ns, ln)
		}
		loc.Status.RepositoryRef = name
		return nil
	case !apierrors.IsNotFound(err):
		return fmt.Errorf("get BackupRepository %s: %w", name, err)
	}

	repo := &cbv1.BackupRepository{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: repositoryBackLinkLabels(loc.Namespace, loc.Name),
		},
	}
	// Deliberately NO SetControllerReference: see apiconst.LabelLocation. The lifecycle is
	// explicit — created here, deleted in finalize.
	if err := r.Create(ctx, repo); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create BackupRepository %s: %w", name, err)
	}
	loc.Status.RepositoryRef = name
	return nil
}

// repositoryInitialized reports whether the BackupRepository named by loc.Status.RepositoryRef
// has finished `restic init`. An absent repository — the normal case on the pass that just
// created it — reads as not-initialized rather than as an error; the Watch re-triggers this
// location the moment the object materialises.
func (r *BackupLocationReconciler) repositoryInitialized(ctx context.Context, loc *cbv1.BackupLocation) (bool, error) {
	if loc.Status.RepositoryRef == "" {
		return false, nil
	}
	var repo cbv1.BackupRepository
	if err := r.Get(ctx, client.ObjectKey{Name: loc.Status.RepositoryRef}, &repo); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("get BackupRepository %s: %w", loc.Status.RepositoryRef, err)
	}
	return repo.Status.Initialized, nil
}

// finalize deletes the BackupRepository this location created — the half of the lifecycle
// Kubernetes' garbage collector would have handled if the ownership were expressible — and drops
// the finalizer.
//
// What it does NOT delete is anything holding data or the means to read it: no S3 object is
// touched, and the repository password Secret is left in place even when the operator generated
// it. That asymmetry is the point. Deleting a BackupLocation is a Kubernetes-side action; the
// user's backups live in the user's bucket and outlive it, and the password is the only thing
// that can still open them. Destroying it here would turn `kubectl delete backuplocation` into
// irreversible data loss — the same stickiness rule adr/0009 states for the platform DEK.
func (r *BackupLocationReconciler) finalize(ctx context.Context, loc *cbv1.BackupLocation) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(loc, apiconst.FinalizerLocation) {
		return ctrl.Result{}, nil
	}

	name := namespacedRepositoryName(loc.Namespace, loc.Name)
	var repo cbv1.BackupRepository
	err := r.Get(ctx, client.ObjectKey{Name: name}, &repo)
	switch {
	case err == nil:
		// Only ever delete a repository that is ours: the same adoption guard as ensureRepository,
		// applied to the destructive direction, so a name collision cannot make one tenant's
		// delete remove another tenant's repository object.
		if repo.Labels[apiconst.LabelNamespace] == loc.Namespace && repo.Labels[apiconst.LabelLocation] == loc.Name {
			if derr := r.Delete(ctx, &repo); derr != nil && !apierrors.IsNotFound(derr) {
				return ctrl.Result{}, fmt.Errorf("delete BackupRepository %s: %w", name, derr)
			}
		}
	case !apierrors.IsNotFound(err):
		return ctrl.Result{}, fmt.Errorf("get BackupRepository %s: %w", name, err)
	}

	r.Recorder.Eventf(loc, nil, corev1.EventTypeNormal, "Finalizing", "Finalize",
		"removing finalizer; no object-storage data is erased and the repository password Secret is retained — "+
			"it is the only key that can still read these backups")

	controllerutil.RemoveFinalizer(loc, apiconst.FinalizerLocation)
	if err := r.Update(ctx, loc); err != nil {
		return ctrl.Result{}, fmt.Errorf("remove finalizer from BackupLocation %s/%s: %w", loc.Namespace, loc.Name, err)
	}
	return ctrl.Result{}, nil
}

// SetupWithManager registers this reconciler. The BackupRepository watch is a mapped Watch rather
// than an Owns because the link is a label pair, not an ownerReference (see
// apiconst.LabelLocation) — and it is load-bearing for the same reason it is on the cluster
// plane: Ready folds in the repository's Initialized, so this is what flips a location to Ready
// promptly when `restic init` succeeds, instead of waiting out shortRequeueInterval.
func (r *BackupLocationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&cbv1.BackupLocation{}).
		Watches(&cbv1.BackupRepository{}, handler.EnqueueRequestsFromMapFunc(mapRepositoryToBackupLocation)).
		Named("backuplocation").
		Complete(r)
}

// mapRepositoryToBackupLocation maps a BackupRepository back to the namespaced BackupLocation its
// back-link labels name. A repository without both labels is a cluster-plane one and maps to
// nothing.
func mapRepositoryToBackupLocation(_ context.Context, obj client.Object) []reconcile.Request {
	labels := obj.GetLabels()
	ns, name := labels[apiconst.LabelNamespace], labels[apiconst.LabelLocation]
	if ns == "" || name == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: client.ObjectKey{Namespace: ns, Name: name}}}
}
