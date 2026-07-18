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

// Package controller hosts CrystalBackup's controller-runtime reconcilers — one file per
// controller, one shared envtest harness (suite_test.go) for all of them.
//
// ClusterBackupLocationReconciler, in this file, is the FIRST controller and the M1
// pattern-setter: the six that follow (BackupRepository, ClusterBackup,
// ClusterBackupSchedule, BackupLocation, Backup, BackupSchedule, ...) are expected to mirror
// its shape —
//
//   - a thin Reconcile that reads the object once, handles deletion first, then delegates
//     each concern (an election, a validation, a probe, an owned-object upsert) to a small
//     private method that sets its OWN status condition(s) and returns a plain bool/error;
//   - every condition written through internal/status.SetCondition, never a hand-built
//     metav1.Condition, so ObservedGeneration and LastTransitionTime stay correct for free;
//   - Secrets read ONLY through internal/client/secrets.ByNameReader — never
//     mgr.GetClient() — because that package's whole reason to exist is keeping the operator
//     from standing up a cluster-wide Secret informer (tenancy invariant I3); a controller
//     that reaches for GetClient().Get on a Secret has broken the pattern;
//   - any dependency on the outside world (here, "is the S3 endpoint reachable") isolated
//     behind a small seam interface (S3Prober) with a dependency-free production
//     implementation, so envtest — which has no real S3 — can supply a trivial stub instead
//     of skipping the behaviour it gates.
package controller

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
	"github.com/CrystalBackup/CrystalBackup/internal/client/secrets"
	"github.com/CrystalBackup/CrystalBackup/internal/keys"
	"github.com/CrystalBackup/CrystalBackup/internal/status"
)

// Condition types written by ClusterBackupLocationReconciler. Exact strings — the M1
// crucible (test/crucible/tests/m1_repository_test.go) asserts against them verbatim, so
// changing any of these is a cross-repo breaking change, not a local rename.
const (
	// ConditionReachable reports whether Spec.S3.Endpoint answered an HTTP probe.
	ConditionReachable = "Reachable"
	// ConditionEncryptionValid reports whether the cluster KEK Secret holds a parseable age identity.
	ConditionEncryptionValid = "EncryptionValid"
	// ConditionMultipleDefaults reports a controller-side-detected conflict: this location is
	// Spec.Default==true while at least one sibling ClusterBackupLocation also is. M1 only
	// flags the conflict (via this condition); hard-rejecting a second default is the M2
	// admission webhook's job.
	ConditionMultipleDefaults = "MultipleDefaults"
	// ConditionReady rolls up Reachable, EncryptionValid, MultipleDefaults and repository
	// provisioning into the single top-level verdict.
	ConditionReady = "Ready"
	// ConditionRetentionIgnored is the advisory set when spec.retention requests a keep policy but
	// the location is Immutable, where restic keep*/forget is inert (object-lock governs expiry).
	// It is controller-side (it needs the location's own mode) and never fail-fast: an Immutable
	// location with an ignored retention is still Ready.
	ConditionRetentionIgnored = "RetentionIgnored"
)

// locationPhaseDegraded is the Status.Phase a ClusterBackupLocation records when a fail-fast
// check (encryption or reachability) fails, or when the readiness rollup is otherwise not met.
const locationPhaseDegraded = "Degraded"

const (
	// kekIdentityDataKey is the data key inside the Secret named by
	// Spec.Encryption.ClusterKEKSecretRef that holds the cluster KEK: an age X25519 identity
	// string ("AGE-SECRET-KEY-1..."), as produced by `age-keygen` or age.GenerateX25519Identity
	// (see internal/keys package doc for the DEK/KEK envelope this feeds).
	kekIdentityDataKey = "identity"

	// shortRequeueInterval paces retries for a location stuck on a fixable configuration
	// fault (a missing/invalid KEK, an unreachable endpoint): frequent enough that a fix
	// lands quickly, sparse enough not to hammer the API server or the S3 endpoint.
	shortRequeueInterval = 30 * time.Second

	// periodicRequeueInterval re-evaluates an otherwise-healthy location on a steady cadence.
	// This is the ONLY thing that refreshes Reachable and MultipleDefaults absent a spec
	// change to this object: Reachable depends on the outside world (an HTTP probe) and
	// MultipleDefaults depends on SIBLING objects, so no Watch on this object alone would
	// ever re-trigger either (see the SetupWithManager doc comment).
	periodicRequeueInterval = 5 * time.Minute
)

// S3Prober is the reachability seam ClusterBackupLocationReconciler probes Spec.S3 through.
// It exists so envtest (and any other unit test) can supply a stub that never touches the
// network, while production wires in the default httpS3Prober. Implementations report
// reachable with a nil error and unreachable with a non-nil one; see Reachable's doc for
// exactly what counts as which.
type S3Prober interface {
	// Reachable probes s3.Endpoint and returns nil if it is reachable, or a non-nil error
	// naming why not. It must not mutate s3 or retain it beyond the call.
	Reachable(ctx context.Context, s3 cbv1.S3Spec) error
}

// s3ProbeTimeout bounds a single reachability probe so a black-holed endpoint cannot stall a
// reconcile indefinitely.
const s3ProbeTimeout = 5 * time.Second

// httpS3Prober is the production S3Prober. It is deliberately dependency-free (net/http,
// crypto/tls, crypto/x509 only — no S3 SDK): a reachability probe needs nothing more than
// "can an HTTP round trip complete against this endpoint", so pulling in an S3 client here
// would add credential handling and a heavier import graph for a check that never
// authenticates. The zero value is ready to use.
type httpS3Prober struct{}

// NewHTTPS3Prober returns the production S3Prober, wired into main.go.
func NewHTTPS3Prober() S3Prober {
	return httpS3Prober{}
}

// Reachable issues an HTTP HEAD to s3.Endpoint and treats ANY completed HTTP response — 200,
// 400, 401, 403, whatever — as reachable: this is a liveness probe on the endpoint, not a
// credentials or bucket-existence check (that happens later, when the BackupRepository
// controller actually opens the restic repository). Only a transport-level failure — DNS,
// TCP connect, TLS handshake — is reported as unreachable.
//
// s3.CABundle, when set, is used as the sole trusted root for the TLS handshake (a
// self-signed or private-CA endpoint); s3.ForcePathStyle affects how a bucket is addressed,
// not whether the endpoint answers HTTP at all, so it plays no part in this probe.
func (httpS3Prober) Reachable(ctx context.Context, s3 cbv1.S3Spec) error {
	httpClient := &http.Client{Timeout: s3ProbeTimeout}
	if s3.CABundle != "" {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(s3.CABundle)) {
			return fmt.Errorf("s3prober: caBundle for %q contains no usable PEM certificates", s3.Endpoint)
		}
		httpClient.Transport = &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodHead, s3.Endpoint, nil)
	if err != nil {
		return fmt.Errorf("s3prober: build request for %q: %w", s3.Endpoint, err)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("s3prober: %q did not respond: %w", s3.Endpoint, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	return nil
}

// ClusterBackupLocationReconciler reconciles a ClusterBackupLocation: the platform's shared
// cluster-DR object storage (adr/0009 — one repository, tenancy by restic tag). It validates
// the location's encryption and reachability, elects at most one healthy default, and owns
// the (empty-spec) BackupRepository that names the repository backing it. It does NOT drive
// restic — provisioning/initializing the actual repository is the BackupRepository
// controller's job (M1 task #16); this controller only ensures that object exists.
type ClusterBackupLocationReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Secrets is the ONLY path this controller reads Secrets through: an uncached,
	// GET-by-name reader (see internal/client/secrets package doc, invariant I3). It reads
	// the cluster KEK from OperatorNamespace.
	Secrets *secrets.ByNameReader
	// Prober is the reachability seam: httpS3Prober in production, a stub in envtest.
	Prober S3Prober
	// OperatorNamespace is where the cluster KEK Secret (and every other cluster-plane
	// platform Secret) lives — apiconst.DefaultOperatorNamespace by default, overridable via
	// main.go's --operator-namespace flag.
	OperatorNamespace string
	Recorder          events.EventRecorder
}

// +kubebuilder:rbac:groups=crystalbackup.io,resources=clusterbackuplocations,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=crystalbackup.io,resources=clusterbackuplocations/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=crystalbackup.io,resources=clusterbackuplocations/finalizers,verbs=update
// +kubebuilder:rbac:groups=crystalbackup.io,resources=backuprepositories,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile drives one ClusterBackupLocation towards the state described in this file's
// package doc: finalizer ensured, sibling defaults counted, encryption validated,
// reachability probed, its BackupRepository ensured, and Ready rolled up from all of the
// above. Each concern after deletion-handling and finalizer-ensuring is delegated to a
// dedicated method that sets its own condition(s); Reconcile itself only sequences them and
// decides, on a failure, whether to stop early (encryption and reachability are fail-fast:
// there is no point creating/owning a BackupRepository for a location that cannot yet be
// trusted) or to fall through to the final Ready rollup (a MultipleDefaults conflict is
// flagged, never fail-fast — adr/0009-era M1 scope is "surface it", not "reject it").
func (r *ClusterBackupLocationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var loc cbv1.ClusterBackupLocation
	if err := r.Get(ctx, req.NamespacedName, &loc); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("get ClusterBackupLocation %s: %w", req.Name, err)
	}

	if !loc.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, &loc)
	}

	if controllerutil.AddFinalizer(&loc, apiconst.FinalizerLocation) {
		if err := r.Update(ctx, &loc); err != nil {
			return ctrl.Result{}, fmt.Errorf("add finalizer to ClusterBackupLocation %s: %w", loc.Name, err)
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Sibling-defaults election runs unconditionally, and never fail-fasts: M1 only flags a
	// conflict (this condition, folded into Ready below), it never rejects one.
	multipleDefaults, err := r.evaluateMultipleDefaults(ctx, &loc)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("evaluate MultipleDefaults for ClusterBackupLocation %s: %w", loc.Name, err)
	}

	// Retention advisory (static, non-fail-fast): flag a spec.retention that an Immutable location
	// will ignore. Evaluated before the fail-fast checks so it is recorded even on an otherwise
	// degraded location — it depends only on spec.mode + spec.retention.
	r.reconcileRetentionAdvisory(&loc)

	// Encryption and reachability are fail-fast: on either failure, stop here, persist what
	// we know, and retry soon — provisioning a BackupRepository for a location whose
	// encryption or storage is not yet trustworthy would be premature.
	if encryptionValid := r.validateEncryption(ctx, &loc); !encryptionValid {
		status.SetCondition(&loc.Status.Conditions, ConditionReady, metav1.ConditionFalse, "EncryptionInvalid",
			"cluster encryption is not valid; see condition EncryptionValid", loc.Generation)
		loc.Status.Phase = locationPhaseDegraded
		if err := r.Status().Update(ctx, &loc); err != nil {
			return ctrl.Result{}, fmt.Errorf("update status for ClusterBackupLocation %s: %w", loc.Name, err)
		}
		return ctrl.Result{RequeueAfter: shortRequeueInterval}, nil
	}

	if reachable := r.checkReachability(ctx, &loc); !reachable {
		status.SetCondition(&loc.Status.Conditions, ConditionReady, metav1.ConditionFalse, "Unreachable",
			"object storage endpoint is not reachable; see condition Reachable", loc.Generation)
		loc.Status.Phase = locationPhaseDegraded
		if err := r.Status().Update(ctx, &loc); err != nil {
			return ctrl.Result{}, fmt.Errorf("update status for ClusterBackupLocation %s: %w", loc.Name, err)
		}
		return ctrl.Result{RequeueAfter: shortRequeueInterval}, nil
	}

	if err := r.ensureRepository(ctx, &loc); err != nil {
		log.Error(err, "ensure BackupRepository")
		return ctrl.Result{}, fmt.Errorf("ensure BackupRepository for ClusterBackupLocation %s: %w", loc.Name, err)
	}

	// Reachable and EncryptionValid are re-read from the conditions (rather than trusted from
	// the local bools above) so this rollup stays correct even if the fail-fast early returns
	// above are ever relaxed — Ready's contract is exactly this AND, independent of how many
	// of the four other checks are allowed to run in the same pass.
	reachable := status.IsConditionTrue(loc.Status.Conditions, ConditionReachable)
	encryptionValid := status.IsConditionTrue(loc.Status.Conditions, ConditionEncryptionValid)
	ready := reachable && encryptionValid && loc.Status.RepositoryRef != "" && !multipleDefaults

	switch {
	case ready:
		status.SetCondition(&loc.Status.Conditions, ConditionReady, metav1.ConditionTrue, "Ready",
			"location is reachable, encryption is valid, and the repository is provisioned", loc.Generation)
		loc.Status.Phase = "Ready"
	case multipleDefaults:
		status.SetCondition(&loc.Status.Conditions, ConditionReady, metav1.ConditionFalse, "MultipleDefaults",
			"this location conflicts with another default ClusterBackupLocation", loc.Generation)
		loc.Status.Phase = "Degraded: multiple default ClusterBackupLocations"
	default:
		status.SetCondition(&loc.Status.Conditions, ConditionReady, metav1.ConditionFalse, "NotReady",
			"location is not ready", loc.Generation)
		loc.Status.Phase = locationPhaseDegraded
	}

	if err := r.Status().Update(ctx, &loc); err != nil {
		return ctrl.Result{}, fmt.Errorf("update status for ClusterBackupLocation %s: %w", loc.Name, err)
	}

	return ctrl.Result{RequeueAfter: periodicRequeueInterval}, nil
}

// finalize handles a ClusterBackupLocation with a non-zero DeletionTimestamp. Per adr/0009,
// delete never erases: no S3 object is touched here, and the repository is never told to
// prune or forget anything (that is ClusterErasure, an explicit M5 action). The owned
// BackupRepository is left for Kubernetes' own garbage collector to collect via its
// controller ownerReference — this method's only job is to stop guarding the delete once
// there is nothing left for THIS controller to do, by removing its finalizer.
func (r *ClusterBackupLocationReconciler) finalize(ctx context.Context, loc *cbv1.ClusterBackupLocation) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(loc, apiconst.FinalizerLocation) {
		return ctrl.Result{}, nil
	}

	r.Recorder.Eventf(loc, nil, corev1.EventTypeNormal, "Finalizing", "Finalize",
		"removing finalizer; no S3 data is erased (adr/0009) and the owned BackupRepository, "+
			"if any, is left for garbage collection via its ownerReference")

	controllerutil.RemoveFinalizer(loc, apiconst.FinalizerLocation)
	if err := r.Update(ctx, loc); err != nil {
		return ctrl.Result{}, fmt.Errorf("remove finalizer from ClusterBackupLocation %s: %w", loc.Name, err)
	}
	return ctrl.Result{}, nil
}

// evaluateMultipleDefaults implements the M1 controller-side single-default election: it
// counts every ClusterBackupLocation with Spec.Default==true across the cluster and sets
// ConditionMultipleDefaults on loc accordingly. It never rejects anything — a hard-reject
// admission webhook is M2 — so a conflict is only ever surfaced, not prevented; callers fold
// the returned bool into the Ready rollup so a flagged location cannot also read Ready.
func (r *ClusterBackupLocationReconciler) evaluateMultipleDefaults(ctx context.Context, loc *cbv1.ClusterBackupLocation) (bool, error) {
	var list cbv1.ClusterBackupLocationList
	if err := r.List(ctx, &list); err != nil {
		return false, fmt.Errorf("list ClusterBackupLocations: %w", err)
	}

	var defaultNames []string
	for i := range list.Items {
		if list.Items[i].Spec.Default {
			defaultNames = append(defaultNames, list.Items[i].Name)
		}
	}

	switch {
	case !loc.Spec.Default:
		status.SetCondition(&loc.Status.Conditions, ConditionMultipleDefaults, metav1.ConditionFalse, "NotDefault",
			"this location is not marked default", loc.Generation)
		return false, nil
	case len(defaultNames) > 1:
		status.SetCondition(&loc.Status.Conditions, ConditionMultipleDefaults, metav1.ConditionTrue, "MultipleDefaults",
			fmt.Sprintf("%d ClusterBackupLocations are marked default: %s", len(defaultNames), strings.Join(defaultNames, ", ")),
			loc.Generation)
		return true, nil
	default:
		status.SetCondition(&loc.Status.Conditions, ConditionMultipleDefaults, metav1.ConditionFalse, "SingleDefault",
			"this is the only default ClusterBackupLocation", loc.Generation)
		return false, nil
	}
}

// reconcileRetentionAdvisory sets ConditionRetentionIgnored on the location: True when
// spec.retention requests a keep policy but the location is Immutable (object-lock governs expiry,
// so restic keep*/forget is inert), False otherwise. This is the authoritative home of the advisory
// now that retention lives on the location, not on schedules/runs: the location knows its own mode,
// so no cross-object lookup is needed. The Warning Event fires only on the transition INTO the
// ignored state, so a steady misconfiguration is not re-logged every reconcile.
func (r *ClusterBackupLocationReconciler) reconcileRetentionAdvisory(loc *cbv1.ClusterBackupLocation) {
	if loc.Spec.Mode == cbv1.LocationModeImmutable && retentionRequested(loc.Spec.Retention) {
		wasIgnored := status.IsConditionTrue(loc.Status.Conditions, ConditionRetentionIgnored)
		status.SetCondition(&loc.Status.Conditions, ConditionRetentionIgnored, metav1.ConditionTrue,
			"ImmutableLocation",
			"spec.retention keep* is ignored on an Immutable location (object-lock governs expiry)", loc.Generation)
		if !wasIgnored {
			r.Recorder.Eventf(loc, nil, corev1.EventTypeWarning, "RetentionIgnored", "IgnoreRetention",
				"spec.retention is set but the location is Immutable; keep* is ignored until object-lock expiry")
		}
		return
	}
	status.SetCondition(&loc.Status.Conditions, ConditionRetentionIgnored, metav1.ConditionFalse,
		"RetentionActive",
		"retention (if any) is applied by a restic forget after each successful backup", loc.Generation)
}

// retentionRequested reports whether a RetentionSpec sets any keep* field.
func retentionRequested(r cbv1.RetentionSpec) bool {
	return r.KeepLast > 0 || r.KeepHourly > 0 || r.KeepDaily > 0 ||
		r.KeepWeekly > 0 || r.KeepMonthly > 0 || r.KeepYearly > 0
}

// validateEncryption reads the cluster KEK Secret named by
// Spec.Encryption.ClusterKEKSecretRef and confirms it parses as an age identity, setting
// ConditionEncryptionValid accordingly. It never returns the identity: this method's only
// output is whether the KEK is usable, not the KEK itself — the plaintext identity lives only
// in the local variables of this call and is discarded when it returns.
func (r *ClusterBackupLocationReconciler) validateEncryption(ctx context.Context, loc *cbv1.ClusterBackupLocation) bool {
	kekName := loc.Spec.Encryption.ClusterKEKSecretRef.Name

	identity, err := r.Secrets.GetValue(ctx, r.OperatorNamespace, kekName, kekIdentityDataKey)
	if err != nil {
		status.SetCondition(&loc.Status.Conditions, ConditionEncryptionValid, metav1.ConditionFalse, "KEKMissing",
			fmt.Sprintf("read cluster KEK secret %s/%s: %v", r.OperatorNamespace, kekName, err), loc.Generation)
		return false
	}

	if _, err := keys.NewAgeWrapper(string(identity)); err != nil {
		// keys.NewAgeWrapper's error never echoes the identity itself (see that package's
		// doc), so it is safe to fold verbatim into a status message.
		status.SetCondition(&loc.Status.Conditions, ConditionEncryptionValid, metav1.ConditionFalse, "KEKInvalid",
			fmt.Sprintf("parse cluster KEK secret %s/%s: %v", r.OperatorNamespace, kekName, err), loc.Generation)
		return false
	}

	status.SetCondition(&loc.Status.Conditions, ConditionEncryptionValid, metav1.ConditionTrue, "KEKValid",
		"cluster KEK identity parsed successfully", loc.Generation)
	return true
}

// checkReachability probes Spec.S3 through r.Prober and sets ConditionReachable accordingly.
func (r *ClusterBackupLocationReconciler) checkReachability(ctx context.Context, loc *cbv1.ClusterBackupLocation) bool {
	if err := r.Prober.Reachable(ctx, loc.Spec.S3); err != nil {
		status.SetCondition(&loc.Status.Conditions, ConditionReachable, metav1.ConditionFalse, "Unreachable",
			fmt.Sprintf("probe %s: %v", loc.Spec.S3.Endpoint, err), loc.Generation)
		return false
	}
	status.SetCondition(&loc.Status.Conditions, ConditionReachable, metav1.ConditionTrue, "Reachable",
		fmt.Sprintf("endpoint %s responded", loc.Spec.S3.Endpoint), loc.Generation)
	return true
}

// ensureRepository ensures the cluster-scoped BackupRepository that will back loc exists,
// named identically to loc (a one-to-one, deterministic mapping — no lookup ever needed) and
// controller-owned by it (legal: both are cluster-scoped, so there is no cross-namespace
// ownership question). Creation is idempotent: on every reconcile it attempts a Create and
// tolerates AlreadyExists, rather than Get-then-Create, because the only state this method
// must ensure — the object exists, owned by loc — is exactly what a successful Create OR a
// pre-existing owned object both already guarantee.
//
// It deliberately never touches the BackupRepository's status: that is entirely the
// BackupRepository controller's responsibility (M1 task #16, restic init included).
func (r *ClusterBackupLocationReconciler) ensureRepository(ctx context.Context, loc *cbv1.ClusterBackupLocation) error {
	repo := &cbv1.BackupRepository{
		ObjectMeta: metav1.ObjectMeta{Name: loc.Name},
	}
	if err := controllerutil.SetControllerReference(loc, repo, r.Scheme); err != nil {
		return fmt.Errorf("set controller reference on BackupRepository %s: %w", repo.Name, err)
	}

	if err := r.Create(ctx, repo); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create BackupRepository %s: %w", repo.Name, err)
	}

	loc.Status.RepositoryRef = repo.Name
	return nil
}

// SetupWithManager registers this reconciler with mgr: it watches ClusterBackupLocation
// directly and BackupRepository as an owned type (so a Create/Update/Delete of the owned
// repository re-triggers its owning location). There is deliberately no Watch on the cluster
// KEK Secret or on sibling ClusterBackupLocations — a Secret watch would need list/watch RBAC
// on Secrets, which invariant I3 forbids, and a self-referential List inside
// evaluateMultipleDefaults would fire on every sibling's every change if watched naively.
// periodicRequeueInterval is the one mechanism that refreshes Reachable and MultipleDefaults
// absent a spec change to THIS object (see that constant's doc); this is a deliberate M1
// trade-off; a Secret watch is a fine future enhancement, not a correctness requirement.
func (r *ClusterBackupLocationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&cbv1.ClusterBackupLocation{}).
		Owns(&cbv1.BackupRepository{}).
		Named("clusterbackuplocation").
		Complete(r)
}
