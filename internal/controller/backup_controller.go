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
	apimeta "k8s.io/apimachinery/pkg/api/meta"
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
	"github.com/CrystalBackup/CrystalBackup/internal/hooks"
	"github.com/CrystalBackup/CrystalBackup/internal/keys"
	"github.com/CrystalBackup/CrystalBackup/internal/metrics"
	"github.com/CrystalBackup/CrystalBackup/internal/mover"
	"github.com/CrystalBackup/CrystalBackup/internal/repo/queue"
	"github.com/CrystalBackup/CrystalBackup/internal/restic"
	"github.com/CrystalBackup/CrystalBackup/internal/status"
	"github.com/CrystalBackup/CrystalBackup/internal/tracing"
	"go.opentelemetry.io/otel/attribute"
)

// backupPollInterval paces re-reconciles while a Backup is still driving its volumes forward:
// short, because progress is polled (an exposure becoming ready, a mover Job finishing). The
// label-based Job watch (see SetupWithManager) is a faster secondary nudge; this requeue is the
// primary, watch-independent driver so a Backup never stalls waiting on an event that a
// cross-namespace Owns() cannot deliver.
const backupPollInterval = 5 * time.Second

// backupTeardownTimeout bounds the detached exposure/Job cleanup. Short: it is a handful of
// deletes, and a manager that is shutting down should not be held open by them — but it MUST have
// its own budget rather than inheriting a reconcile context that is already cancelled.
const backupTeardownTimeout = 30 * time.Second

// exposureDrainRecheckInterval is how soon the terminal teardown sweep re-verifies when the
// deletes succeeded but labelled residue is still DRAINING (the external snapshot-controller's
// cascade, a Terminating clone PVC). Short and error-free: each re-pass also re-drives the
// direct reclaim, so the sweep accelerates the drain instead of merely observing it.
const exposureDrainRecheckInterval = 15 * time.Second

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

// backupReasonPrecheckFailed is the VolumeStatus.reason a volume carries when the exposure
// pre-check refused it — today: the VolumeSnapshotClass names a snapshotter Secret that does not
// exist (internal/exposer.Precheck).
//
// FAIL, NOT GATE. A gate (hold the volume, requeue, wait for a human) is the wrong shape here for
// two reasons. First, every check behind this reason is STRUCTURAL: a Secret that is absent stays
// absent until somebody creates it, and no amount of waiting changes the verdict — so a gate is a
// backup that never finishes and never alarms, which is the precise failure this pre-check exists
// to prevent (an un-served VolumeSnapshot hangs the run in Snapshotting, looking like progress).
// Second, a per-volume Failed is VISIBLE and ALERTABLE: it rolls the Backup up to
// PartiallyCompleted/Failed, which is a state an operator's dashboards and the shipped alert rules
// already react to, whereas a gate waits for someone who is not watching.
//
// The one exception worth knowing about: a rook-ceph installation that is still converging can
// fail a FIRST run this way — the VolumeSnapshotClass lands before the CSI Secrets the operator
// creates. That run reports a real fact about the cluster at that moment, and the next scheduled
// run passes with no intervention. That is the correct trade: a transiently-red first backup on a
// half-installed storage cluster, against a silent hang on a permanently broken one.
//
// Asserted verbatim by test/crucible/tests/m6_precheck_test.go — a cross-repo contract.
const backupReasonPrecheckFailed = "SnapshotPrecheckFailed"

// backupReasonSnapshotProgressDeadline is the VolumeStatus.reason a volume carries when its origin
// VolumeSnapshot sat past snapshotProgressDeadline with nothing having touched it. The reason names
// the SYMPTOM, not a guess at the cause; the Event carries the diagnosis — the two components that
// could have picked the request up, most-likely first — plus the elapsed time and the snapshot's
// identity, so the investigation starts somewhere concrete.
const backupReasonSnapshotProgressDeadline = "SnapshotProgressDeadlineExceeded"

// snapshotProgressDeadline bounds how long a volume may sit in Snapshotting with its origin
// VolumeSnapshot completely UNACKNOWLEDGED — no bound VolumeSnapshotContent, no recorded error —
// before the volume is failed rather than waited on forever.
//
// This is the second half of delta 9 and it exists because the pre-check alone is not enough:
// snapshot machinery that dies AFTER a passing pre-check leaves the backup in exactly the hang the
// pre-check was built to prevent. It costs no new CRD field and no new state — the clock is the
// origin VolumeSnapshot's own creationTimestamp, which the exposure histogram already reads
// (exposer.StartedAt), so it survives an operator restart for free.
//
// FIFTEEN MINUTES, and the number is a compromise between two real risks rather than a round one.
// It must not fire on a genuinely slow first snapshot on a cold pool — but note what the condition
// actually requires: the object must be UNTOUCHED. The external snapshot-controller binds a
// VolumeSnapshotContent within seconds of a request it can see, long before the storage system has
// finished cutting anything, so a slow snapshot shows Acknowledged=true almost immediately and is
// never a candidate. What 15 minutes buys is headroom over the transients that can legitimately
// leave the object untouched for a while: a snapshot-controller Deployment rolling out, a leader
// election after a node drain, a rook-ceph operator mid-reconcile re-creating its CSI sidecars.
// Those are minutes, not tens of minutes. And it must stay well under the hour-scale window a
// nightly backup runs in, so the failure is REPORTED BY THAT RUN instead of by the next one.
//
// Erring long is the cheap direction: a deadline that is too generous delays a diagnosis, while
// one that is too tight fails healthy backups — so if this number is ever wrong, it should be
// wrong this way.
const snapshotProgressDeadline = 15 * time.Minute

// backupReasonSnapshotReadyDeadline is the VolumeStatus.reason a volume carries when its origin
// VolumeSnapshot WAS picked up — a VolumeSnapshotContent is bound, or an error was recorded — and
// then never became readyToUse within snapshotReadyDeadline. It is the other half of
// backupReasonSnapshotProgressDeadline, and the two are deliberately different strings: one says
// "nothing is listening to your VolumeSnapshotClass", the other says "something is listening and
// the copy is not arriving", which send an operator to different components.
//
// The reason carries the most recent Warning Event recorded ON THE ORIGIN VOLUMESNAPSHOT, appended
// after a colon, exactly as MoverEvicted carries the kubelet's own message. That is the whole point
// of the lot: a reason that says only "timed out" leaves the reader as blind as the 36-hour hang
// that produced this code did. When no such Event exists the reason is the bare string.
const backupReasonSnapshotReadyDeadline = "SnapshotReadyDeadlineExceeded"

// snapshotReadyDeadline bounds how long a volume may sit in Snapshotting with an ACKNOWLEDGED
// origin VolumeSnapshot that never becomes readyToUse.
//
// This is the gap internal/exposer's SnapshotProgress documents and deliberately declines to close
// (see its doc): Acknowledged only proves the cluster-wide snapshot-controller saw the request and
// bound a VolumeSnapshotContent, which it does within seconds, before the storage system has done
// any work at all. A dead per-driver csi-snapshotter sidecar leaves exactly that shape — content
// bound, readyToUse never — and from the object alone it is indistinguishable from a driver taking
// its time. Closing it therefore means picking a MAXIMUM TIME A SNAPSHOT MAY LEGITIMATELY TAKE ON
// STORAGE WE DO NOT OWN, which is a guess, and the honest thing to do with a guess is to say so and
// to err in the survivable direction.
//
// TWO HOURS. The number is chosen from the slowest legitimate snapshot this project has evidence
// of, with room on top:
//
//   - A CSI snapshot is usually metadata: ceph-csi and every thin-provisioning driver answer in
//     seconds regardless of volume size. Those are not what sets this bound.
//   - What sets it is the drivers that do real work before reporting ready. ceph-csi may FLATTEN a
//     deep clone chain first (adr/0006 — the same mechanism CrystalbackupPVCSnapshotPileup exists to
//     warn about), and the cloud-disk drivers hold readyToUse until the provider reports the
//     snapshot complete, which on a multi-terabyte disk is tens of minutes and occasionally more.
//   - Two hours is comfortably past all of those and still inside the window a nightly run is
//     judged in: the failure is reported by THAT run, hours before the next one, rather than
//     silently rolling into the next night.
//
// Err long, always. A deadline that is too generous delays a diagnosis by an hour; one that is too
// tight fails the biggest, slowest, most valuable volumes in the fleet — the ones whose backup
// matters most — and it does so on exactly the storage where the operator can least easily prove us
// wrong. Unlike the mover deadline below, this failure costs a snapshot that was genuinely being
// worked on, so the burden of proof is set correspondingly high.
const snapshotReadyDeadline = 2 * time.Hour

// backupReasonMoverStartDeadline is the VolumeStatus.reason a volume carries when its data-mover
// Job existed for moverStartDeadline without a single one of its pods ever reaching Running.
//
// It carries the most recent Warning Event about that pod, appended after a colon — the kubelet's
// own words, the way MoverEvicted carries them (see podKillReason). In the incident this closes,
// that string is:
//
//	FailedMount: MountVolume.MountDevice failed ... rbd: map failed with error (exit status 22)
//
// published by the kubelet 1069 times over 36 hours, starting one minute in, and read by nobody
// because nothing surfaced it. `kubectl get backup` has to be as informative as `kubectl describe
// pod` was, or the timeout merely converts a silent hang into a silent failure.
const backupReasonMoverStartDeadline = "MoverStartDeadlineExceeded"

// moverStartDeadline bounds how long a volume may sit in Uploading with its mover pod never having
// REACHED RUNNING.
//
// READ THE PREDICATE BEFORE THE NUMBER, because the predicate is what makes the number safe. This
// is NOT a cap on how long a backup may take, and it must never become one: a 500 GB volume
// legitimately uploads for hours, and failing it on a wall-clock bound would be a far worse defect
// than the hang this closes. Once the pod is Running, the mover has as long as it likes — nothing
// here ever looks at it again.
//
// What is bounded is the interval before restic has read a single byte. A pod stuck in
// ContainerCreating or Pending is not slow, it is STUCK: no image, no volume, no scheduling. And
// because no work has been done, giving up costs nothing — no bytes re-uploaded, no repository
// lock taken, no snapshot consumed. That asymmetry is what lets this deadline be short where
// snapshotReadyDeadline must be long.
//
// THIRTY MINUTES, sized against the legitimate reasons a pod is slow to start:
//
//   - pulling the mover image onto a cold node over a slow or rate-limited registry (minutes);
//   - the cluster autoscaler provisioning a node for it, which on most clouds is 2–5 minutes and
//     on a bad day is fifteen;
//   - the CSI driver attaching and mounting the temp clone PVC — the very step that failed in the
//     incident, and which on a healthy cluster is seconds.
//
// Thirty minutes covers all three happening at once, and still fails the volume inside the first
// hour of a nightly window instead of on the second morning. If this number is ever wrong it
// should be wrong LONG, for the same reason every other bound here is: a late failure is
// recoverable, a premature one is a backup somebody needed.
const moverStartDeadline = 30 * time.Minute

// ExposerRegistry is the seam the Backup controller reaches internal/exposer.Registry through,
// extracted as an interface so envtest — which has no external snapshot CRDs or CSI driver — can
// inject a stub. Production wires in *exposer.Registry. Its two methods are the two halves of an
// exposure's life, and they are deliberately asymmetric:
//
//   - For resolves the per-PVC SnapshotExposer that CREATES and polls an exposure — it must read
//     the live PVC (storage class → provisioner → exposer kind).
//   - TeardownExposure DESTROYS by derived identity alone (origin namespace + name prefix +
//     labels), never reading the PVC and never creating: teardown must work after the PVC or its
//     whole namespace is gone, and must be safe to re-run from the terminal re-entry sweep and
//     the orphan reaper.
type ExposerRegistry interface {
	For(ctx context.Context, pvc *corev1.PersistentVolumeClaim) (exposer.SnapshotExposer, error)
	TeardownExposure(ctx context.Context, originNamespace, namePrefix string, labels map[string]string) error
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
	// MoverProfiles is the resolved per-operation sizing table (internal/mover.Profiles): the
	// requests/limits and restic-cache cap every Job this controller builds carries. Nil means
	// the built-in defaults, which is what envtest runs with.
	MoverProfiles mover.Profiles
	// MoverPlacement is the operator-wide scheduling policy (internal/mover.Placement) every Job
	// this controller builds carries. The data movers here mount the exposed volume, so this is
	// the field that decides whether the pod lands on a node whose CSI driver can map it; the
	// same-node restore path pins its Job instead, and mover.Placement narrows itself for that.
	MoverPlacement mover.Placement
	// ManifestMoverServiceAccount and ManifestReaderClusterRole name the identity and grant of
	// the manifest mover. They are CONFIGURED, not derived: the chart release-prefixes every
	// cluster-scoped object so two installs cannot collide, so the operator must be told the
	// resolved names rather than reconstructing them from a convention it does not own.
	ManifestMoverServiceAccount string
	ManifestReaderClusterRole   string
	Recorder                    events.EventRecorder
	// Queue is the per-repository exclusive work queue, SHARED with the BackupRepository controller
	// (main.go constructs one and passes it to both). The Backup controller enqueues the two
	// repository maintenance ops it triggers — retention forget after a successful backup, and a
	// stale-lock unlock after a hard-killed mover — on the repository's lane (keyed by the
	// BackupRepository name == the location name), so they can never race an init or another
	// maintenance op on the same repository (adr/0010).
	Queue *queue.Manager
	// Hooks executes consistency hooks inside the workload's own containers (R16). A seam: the
	// production implementation is hooks.PodExecutor over pods/exec, envtest supplies a fake, and
	// nil means "no exec path wired" — which is a hard failure when a run declares hooks, never a
	// silent downgrade to a crash-consistent snapshot the operator believes is better than that.
	Hooks hooks.Executor
	// APIReader reads STRAIGHT from the apiserver, bypassing the cache. It has two callers, and
	// both need the bypass for a different reason:
	//
	//   - the writeStatus ambiguity check (terminalPhaseCommitted): after a status Update errors
	//     client-side, only an uncached read can tell whether the write nonetheless committed
	//     server-side — the cache may still be serving the pre-write object;
	//   - the per-phase deadlines' Warning-Event lookup (latestWarningEvent): reading Events through
	//     the cached client would start an informer over every Event in the cluster, which is the
	//     largest object stream a cluster has, to serve a handful of field-selected reads.
	//
	// Set post-construction (mgr.GetAPIReader()), like Hooks. Nil degrades both: the write check is
	// skipped (treat the error as not-persisted; the terminal re-entry sweep heals it later) and a
	// deadline's reason carries no cause detail.
	APIReader client.Reader
	// MoverStartDeadline overrides moverStartDeadline. ZERO — what production leaves it — means the
	// constant, and nothing outside the test suite ever sets it: it is not plumbed to a flag, a
	// Helm value or a CRD field, deliberately. A per-cluster knob on this bound would be a knob
	// whose wrong setting silently destroys large backups, and the whole safety of the bound comes
	// from the predicate (the pod never started) rather than from the number.
	//
	// It exists because the alternative is a deadline nothing can test. envtest runs an apiserver
	// and etcd and nothing else — no kubelet, no Job controller — so the only clock available to a
	// spec is the mover Job's real creationTimestamp, which the apiserver resets on every update
	// (rest.objectMetaSystemFieldsUpdate) and therefore cannot be backdated. Without this field the
	// only way to watch this decision happen is to wait out thirty real minutes, which means in
	// practice that nobody ever watches it, which is how a timeout ships broken.
	MoverStartDeadline time.Duration
}

// effectiveMoverStartDeadline is the bound moverStalled compares against: the injected one when a
// test set it, the constant otherwise.
func (r *BackupReconciler) effectiveMoverStartDeadline() time.Duration {
	if r.MoverStartDeadline > 0 {
		return r.MoverStartDeadline
	}
	return moverStartDeadline
}

// NewBackupReconciler builds a BackupReconciler. Callers (main.go, the envtest suite) go through
// this constructor to keep the wiring in one place, mirroring NewBackupRepositoryReconciler.
func NewBackupReconciler(
	c client.Client,
	scheme *runtime.Scheme,
	secretsReader *secrets.ByNameReader,
	exposers ExposerRegistry,
	operatorNamespace, moverImage string,
	moverProfiles mover.Profiles,
	moverPlacement mover.Placement,
	manifestMoverSA, manifestReaderRole string,
	recorder events.EventRecorder,
	q *queue.Manager,
) *BackupReconciler {
	return &BackupReconciler{
		Client:                      c,
		Scheme:                      scheme,
		Secrets:                     secretsReader,
		Exposers:                    exposers,
		OperatorNamespace:           operatorNamespace,
		MoverImage:                  moverImage,
		MoverProfiles:               moverProfiles,
		MoverPlacement:              moverPlacement,
		ManifestMoverServiceAccount: manifestMoverSA,
		ManifestReaderClusterRole:   manifestReaderRole,
		Recorder:                    recorder,
		Queue:                       q,
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
	repoName      string // BackupRepository name -> the exclusive queue's repoKey
	repoURL       string // BackupRepository.status.repositoryURL -> RESTIC_REPOSITORY
	dek           string // the restic repository password: platform DEK, or the tenant's own key
	s3CredsSecret string // location.spec.s3.credentialsSecretRef.name
	// credsNamespace is where s3CredsSecret lives: the operator namespace for a cluster-plane
	// run, the BACKUP'S OWN namespace for a namespace-plane one. Carried explicitly rather than
	// defaulted, because the failure mode of getting it wrong is silent: a tenant credentials
	// Secret whose name collides with a platform one would send the tenant's data to whatever
	// bucket the platform credentials reach.
	credsNamespace string
	// s3Connections is the location's spec.s3.connections (nil ⇒ restic's default). It rides on
	// the run context for the same reason s3CredsSecret does: it is a property of the LOCATION,
	// resolved once per reconcile, and every mover this run creates — data, manifests, and the
	// retention forget behind it — must be tuned the same way as the repository they all share.
	s3Connections *int32
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
// serviceaccounts/impersonate is how a hook runs as a TENANT identity instead of as the operator
// (M5). It is what makes the confinement invariant enforceable: the operator asks the API server
// to authorise the exec against system:serviceaccount:<backed-up-namespace>:<name>, and a
// ServiceAccount the namespace never granted pods/exec simply cannot run the command.
//
// The grant is broad on purpose — the ServiceAccount NAME is a user-chosen field, so it cannot be
// pinned with resourceNames without dictating a naming convention to every tenant. What bounds it
// is the code, not the RBAC: the namespace is always derived from the target pod and is not a
// field anywhere in the API. Administrators who prefer a convention can narrow this rule in their
// own overlay.
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=impersonate
//
// pods/exec is the consistency-hook grant (R16) — the ability to run arbitrary commands inside a
// tenant's containers, and the largest privilege in the backup path. It remains needed for
// admin-authored CLUSTER-plane hooks, which name no ServiceAccount and run as the operator. It is bounded by the
// controller invariant that a hook only ever execs into pods MOUNTING the volumes being
// snapshotted, in the CR's own namespace (03-security-and-tenancy.md §5).
//
// The marker itself is the fix for a real split: the Helm chart has granted pods/exec since M0,
// while config/rbac/role.yaml never has, because no marker existed for controller-gen to find. A
// kustomize install therefore could not run a hook at all, and `make manifests` could never
// discover that it should.
// +kubebuilder:rbac:groups="",resources=pods/exec,verbs=create
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=snapshot.storage.k8s.io,resources=volumesnapshots;volumesnapshotcontents,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=snapshot.storage.k8s.io,resources=volumesnapshotclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=storage.k8s.io,resources=storageclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups="";events.k8s.io,resources=events,verbs=create;patch
//
// events, READ side. The per-phase deadlines carry the observable cause into
// status.volumes[].reason — the kubelet's own "FailedMount … rbd: map failed" rather than a bare
// "timed out" — and an Event is the only place that sentence exists (a pod wedged in
// ContainerCreating carries nothing useful in its own status). The reads are field-selected by
// involvedObject.name, through the UNCACHED APIReader, and only on a path that is already failing a
// volume; see latestWarningEvent for why they must never go through the cache.
// +kubebuilder:rbac:groups="",resources=events,verbs=get;list;watch

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

	// Join this pass to the Backup's trace (spec/05-observability.md §5) and stamp `traceID` on
	// every log line it produces (§4). The anchor is DERIVED from the object's UID, so this pass
	// lands in the same trace as the forty passes before it without either of them having been
	// told — including across a process restart or a leader-election handover. Inert, and 4ns,
	// when tracing is not configured.
	ctx, _ = traced(ctx, backup.UID)

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

	// Terminal Backups are done: they neither re-execute nor requeue. But before this pass goes
	// quiet forever, the sweep verifies the exposure teardown actually completed — the leak audit
	// proved this short-circuit used to seal ANY missed teardown permanently (the one-shot pass
	// that wrote the terminal status was also the only one that ever deleted the VS/VSC pair, and
	// nothing, reaper included, ever retried). Once AnnotationExposuresCleaned is stamped, the
	// short-circuit returns without touching status, preserving the terminal record.
	if isTerminalBackupPhase(backup.Status.Phase) {
		return r.ensureTerminalTeardown(ctx, &backup)
	}

	// (6) Resolve the effective run spec: the materialized spec.run, or — for objects created
	// before materialization existed — a pull from the parent ClusterBackup named by the link
	// label. With neither, degrade and requeue rather than invent a run.
	run, ok, err := r.resolveRun(ctx, &backup)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !ok {
		return r.gate(ctx, &backup, "NoRunSpec",
			"no run configuration: spec.run is absent and no parent ClusterBackup resolved from label "+
				apiconst.LabelClusterBackup)
	}

	// (7) Resolve the location, its repository, its key and — on the namespace plane — the
	// identity its hooks run as, into the one value the per-PVC state machine reads from. Every
	// "not ready yet" answer in there is a gate, so done=true means the result is already decided.
	rc, gateRes, gated, err := r.resolveRunContext(ctx, &backup, run)
	if gated {
		return gateRes, err
	}

	// (9) Enumerate matching PVCs and (idempotently) seed one VolumeStatus each.
	if err := r.ensureVolumes(ctx, &backup, run.PVCSelector); err != nil {
		return ctrl.Result{}, fmt.Errorf("enumerate PVCs for Backup %s/%s: %w", backup.Namespace, backup.Name, err)
	}

	// (9b) The freeze window opens here (R16), before any VolumeSnapshot exists.
	hookSt := hookState(&backup)
	if res, done, err := r.openFreezeWindow(ctx, &backup, rc, hookSt, run.Hooks); done {
		return res, err
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

	// (10b) The manifest half, driven independently of the volumes. A PVC the CSI driver cannot
	// snapshot is reported Skipped and the namespace still gets its manifests (02-api.md): the
	// two halves fail for unrelated reasons, and coupling them would lose one to the other's bad
	// day.
	manifestsDone, teardownManifests, err := r.advanceManifests(ctx, &backup, rc,
		includeManifests(run), run.ManifestOptions.ExcludeSecretData)
	if err != nil {
		return ctrl.Result{}, err
	}

	// (10c) The freeze window CLOSES here, on the snapshots being cut.
	if err := r.closeFreezeWindow(ctx, &backup, hookSt, run.Hooks); err != nil {
		return ctrl.Result{}, err
	}

	// (11) Single status writer: roll the per-volume phases up, record a terminal condition +
	// backupTime once, and write status exactly once.
	res, err := r.writeStatus(ctx, &backup, rc, manifestsDone)
	if err != nil {
		// A status-write ERROR is not proof the status was not WRITTEN. A clean Conflict is (the
		// server rejected it), but a cancellation or connection reset in flight — SIGTERM cancels
		// this very context — can surface client-side while the apiserver commits anyway. If the
		// phase we just tried to persist was terminal and an uncached re-read shows it landed,
		// returning here would be the sealed-forever path the leak audit confirmed: the next pass
		// short-circuits on the committed terminal phase and this pass's teardown below never runs
		// (the terminal re-entry sweep would heal it, but only a process-lifetime later). So on an
		// ambiguous error, disambiguate and fall through to the teardown when the write really
		// committed. Otherwise: return WITHOUT tearing down, so the mover Job survives and the
		// next reconcile re-reads and re-records the same terminal result.
		if !r.terminalPhaseCommitted(ctx, &backup, err) {
			return res, err
		}
		res = ctrl.Result{} // the terminal write committed: proceed exactly as on success
	}
	// A Backup whose volumes are all terminal but whose manifests are still in flight must keep
	// being reconciled: writeStatus only reasons about volumes, so without this the run would go
	// quiet holding a running manifest Job and never record its snapshot.
	if !manifestsDone && res.IsZero() {
		res = ctrl.Result{RequeueAfter: backupPollInterval}
	}

	// The terminal result is now durable: safe to tear the just-finished volume's exposure + Job
	// down (best-effort; idempotent).
	if teardownPVC != "" {
		r.teardownVolume(ctx, &backup, teardownPVC)
	}
	// Same rule for the manifest half, and it matters more there: its residue includes the
	// transient RoleBinding, so tearing down before the write persisted would delete a grant the
	// very next reconcile re-creates for a second dump of the same namespace.
	if teardownManifests != "" {
		r.teardownManifests(ctx, &backup, teardownManifests)
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

// hooksDeclared reports whether a run asks for any hook execution at all. honorAnnotations counts:
// it is a standing instruction to exec whatever pods in the namespace declare, which needs an
// identity exactly as much as a spec-declared command does — more, arguably, since the command is
// chosen by whoever can annotate a pod.
func hooksDeclared(spec cbv1.HooksSpec) bool {
	return len(spec.Pre) > 0 || len(spec.Post) > 0 || spec.HonorAnnotations
}

// includeManifests resolves the run's includeManifests, which defaults to TRUE: a namespace
// backup without its manifests restores data into nothing, so the safe default is to capture
// them and the explicit act is to opt out.
func includeManifests(run *cbv1.BackupRunSpec) bool {
	return run.IncludeManifests == nil || *run.IncludeManifests
}

// writeStatus rolls the per-volume phases up into the Backup phase, records the headline
// condition (and backupTime on first reaching a terminal phase), writes status once, and returns
// the requeue decision: none once terminal, a short poll while volumes are still in flight.
func (r *BackupReconciler) writeStatus(ctx context.Context, backup *cbv1.Backup, rc *backupRunContext, manifestsDone bool) (ctrl.Result, error) {
	phase := string(status.RollUpVolumePhases(backup.Status.Volumes))
	// A Backup is not finished while its manifest half is still running, even when every volume
	// is. Letting the roll-up go terminal here would trip the already-terminal short-circuit at
	// the top of Reconcile, and the capture in flight would never have its result recorded —
	// leaving a snapshot in the repository that no Backup object points at, which is exactly the
	// kind of silent loss the discovery model cannot repair (the run tag would be orphaned).
	//
	// Uploading is the honest phase for it: bytes are still going to the repository.
	if !manifestsDone && isTerminalBackupPhase(phase) {
		phase = string(status.BackupPhaseUploading)
	}
	backup.Status.Phase = phase

	terminal := isTerminalBackupPhase(phase)
	// justTerminal is the FIRST arrival at a terminal phase, and it is the guard the event
	// metrics hang off: backupTime being nil is the only durable marker of "this transition has
	// not been recorded yet". Everything else re-runs — a conflict retry, a re-list, the
	// already-terminal sweep — and a counter incremented on any of those would inflate.
	justTerminal := terminal && backup.Status.BackupTime == nil
	if terminal {
		if backup.Status.BackupTime == nil {
			now := metav1.Now()
			backup.Status.BackupTime = &now
		}
		setCompletionTime(backup)
		setTerminalCondition(backup, phase)
	} else {
		status.SetCondition(&backup.Status.Conditions, ConditionReady, metav1.ConditionFalse, "InProgress",
			"backup is in progress ("+phase+")", backup.Generation)
	}

	if err := r.Status().Update(ctx, backup); err != nil {
		return ctrl.Result{}, fmt.Errorf("update status for Backup %s/%s: %w", backup.Namespace, backup.Name, err)
	}
	// AFTER the write, never before. A counter incremented against a status the API server
	// rejected records a backup that, as far as every other observer is concerned, has not
	// finished — and unlike a status field there is no way to take it back.
	//
	// The one gap this leaves is the ambiguous-write path the caller handles
	// (terminalPhaseCommitted): a SIGTERM that cancels the request mid-flight while the server
	// commits anyway loses this observation. Recording there instead would mean recording on a
	// path that cannot tell success from failure, which trades a rare undercount for a
	// systematic one.
	if justTerminal {
		recordBackupTerminalMetrics(ctx, backup, rc)
		r.emitBackupRootSpan(ctx, backup, rc)
	}
	if terminal {
		return ctrl.Result{}, nil
	}
	return ctrl.Result{RequeueAfter: backupPollInterval}, nil
}

// emitBackupRootSpan emits the `backup` span of spec/05-observability.md §5 — the ROOT of this
// Backup's trace, and the last span of it to be produced.
//
// The ordering is the whole design. Its children (hooks, per-PVC snapshot/expose/mover, manifests)
// were emitted one at a time over the preceding minutes or hours, by reconciles that had already
// ended; each named a parent span id it computed from this object's UID. This call finally emits
// that parent, pinned to the very id they were told, so the tree assembles at the collector rather
// than in any process's memory.
//
// It rides on justTerminal — backupTime having just been set, after the status write that made it
// durable — for exactly the reason the terminal counters do: everything else in this controller
// re-runs (a conflict retry, the already-terminal sweep), and a re-run here would publish a second
// root span for the same backup, which a backend renders as two conflicting versions of one trace.
func (r *BackupReconciler) emitBackupRootSpan(ctx context.Context, backup *cbv1.Backup, rc *backupRunContext) {
	anchor := backupAnchor(backup)
	if !anchor.Valid() {
		return
	}
	attrs := backupSpanAttrs(backup, rc)
	attrs = append(attrs, tracing.StringAttr(tracing.AttrPhase, backup.Status.Phase)...)

	// A failed backup is a failed span. The message is the headline condition's, so the trace and
	// `kubectl describe backup` give the same account of what went wrong.
	var spanErr error
	if backupPhaseIsFailure(backup.Status.Phase) {
		spanErr = fmt.Errorf("backup %s/%s ended %s: %s", backup.Namespace, backup.Name,
			backup.Status.Phase, terminalConditionMessage(backup))
	}

	anchor.EmitRoot(ctx, "backup",
		backup.CreationTimestamp.Time, timeOrZero(backup.Status.BackupTime), spanErr,
		runLink(ctx, r.Client, backup), attrs...)
}

// backupPhaseIsFailure reports whether a terminal phase should mark the root span errored.
// PartiallyFailed does: some of this namespace's data did not reach the repository, and a trace
// that renders that as a success is a trace nobody will think to open. PartiallyCompleted does
// NOT — volumes skipped for want of CSI snapshot support are a documented outcome, not a fault of
// this run — and it is legible on the span through crystalbackup.phase.
func backupPhaseIsFailure(phase string) bool {
	return phase == string(status.BackupPhaseFailed) || phase == string(status.BackupPhasePartiallyFailed)
}

// terminalConditionMessage returns the headline condition's message, for the span's error status.
func terminalConditionMessage(backup *cbv1.Backup) string {
	if c := status.FindCondition(backup.Status.Conditions, ConditionReady); c != nil && c.Message != "" {
		return c.Message
	}
	return "see the Backup's conditions"
}

// recordBackupTerminalMetrics emits the event half of the backup catalogue for ONE Backup that
// has just reached a terminal phase and had that phase persisted (05-observability.md §2.1/§2.11).
func recordBackupTerminalMetrics(ctx context.Context, backup *cbv1.Backup, rc *backupRunContext) {
	var added int64
	for _, v := range backup.Status.Volumes {
		added += v.AddedBytes
	}
	// Measured creation→backupTime, identical to crystalbackup_backup_last_duration_seconds, so
	// the gauge and the histogram's quantiles describe the same quantity.
	var duration time.Duration
	if backup.Status.BackupTime != nil {
		duration = backup.Status.BackupTime.Sub(backup.CreationTimestamp.Time)
	}
	// ctx carries the Backup's trace anchor (installed at the top of Reconcile), so this
	// observation lands in the histogram with an exemplar pointing at the trace that produced it:
	// a click from a p99 spike in Grafana to the eleven minutes one CSI driver spent thinking.
	metrics.RecordBackupTerminal(ctx, backupMetricSeries(backup, rc), backup.Status.Phase, duration, added)
}

// moverMetricClusterID resolves the `cluster` label for the platform-scope families, which carry
// it without the rest of the backup identity. It is also backupMetricSeries' cluster resolution,
// so the two can never disagree about what cluster a Backup belongs to.
//
// The label is dropped for a namespace-plane Backup even though rc.clusterID holds a perfectly
// good value for it: the collector resolves `cluster` by looking the location name up among the
// ClusterBackupLocations, finds nothing for a namespaced BackupLocation, and emits an empty
// label. Emitting the real ID here would split every namespace-plane series in two. (Teaching the
// collector to resolve namespaced locations is the better fix, and a change to already-published
// series — not this lot's to make.)
func moverMetricClusterID(backup *cbv1.Backup, rc *backupRunContext) string {
	if rc != nil && backup.Spec.LocationRef.Kind == kindClusterBackupLocation {
		return rc.clusterID
	}
	return ""
}

// backupMetricSeries derives a Backup's metric identity, matching internal/metrics' own
// state-derived resolution field for field — the counters here and the gauges the collector
// computes have to carry IDENTICAL label sets or a dashboard cannot put a failure rate next to
// the last success it belongs to.
func backupMetricSeries(backup *cbv1.Backup, rc *backupRunContext) metrics.BackupSeries {
	tenant := backup.Labels[apiconst.LabelTenant]
	if tenant == "" {
		tenant = backup.Namespace
	}
	clusterID := moverMetricClusterID(backup, rc)
	return metrics.BackupSeries{
		Namespace: backup.Namespace,
		Tenant:    tenant,
		Schedule:  backup.Labels[apiconst.LabelSchedule],
		Origin:    backup.Labels[apiconst.LabelOrigin],
		Location:  backup.Spec.LocationRef.Name,
		Cluster:   clusterID,
	}
}

// failHooks terminates a Backup whose pre-snapshot quiesce failed with onError=Fail.
//
// This is a hard Failed, not a partial: the point of a pre hook is to make the snapshot
// trustworthy, so capturing anyway would produce a backup that LOOKS application-consistent and is
// not — the one outcome worse than having no backup, because it is discovered at restore time.
// It never requeues: Failed is terminal, so the caller's pass simply ends here.
func (r *BackupReconciler) failHooks(ctx context.Context, backup *cbv1.Backup, rc *backupRunContext, message string) error {
	backup.Status.Phase = string(status.BackupPhaseFailed)
	justTerminal := backup.Status.BackupTime == nil
	if backup.Status.BackupTime == nil {
		now := metav1.Now()
		backup.Status.BackupTime = &now
	}
	// The other door into a terminal phase, and it has to stamp completionTime too. A pre-hook
	// abort is a Failed Backup like any other; leaving it unstamped would send the failure clock
	// back to the object's creation for exactly the runs whose hooks timed out — the ones where
	// creation and failure are furthest apart.
	setCompletionTime(backup)
	status.SetCondition(&backup.Status.Conditions, ConditionReady, metav1.ConditionFalse,
		"PreHookFailed", message, backup.Generation)
	if err := r.Status().Update(ctx, backup); err != nil {
		return fmt.Errorf("update status for Backup %s/%s: %w", backup.Namespace, backup.Name, err)
	}
	// A hook-aborted Backup is a Failed Backup and belongs in the same counters as any other:
	// the tenant asked for a restore point and does not have one. It carries no uploaded bytes
	// and a duration that is essentially the hook timeout, both of which are the truth about it.
	if justTerminal {
		recordBackupTerminalMetrics(ctx, backup, rc)
		r.emitBackupRootSpan(ctx, backup, rc)
	}
	r.Recorder.Eventf(backup, nil, corev1.EventTypeWarning, "BackupFailed", "PreHookFailed",
		"no snapshot was taken: %s", message)
	return nil
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
// "effective cancel / no leak on delete" guarantee. For EVERY volume that may have exposed
// (everything but Skipped — Pending included, since a crash between Expose and the first status
// write leaves a live origin VS on a still-Pending volume, and a Completed/Failed volume's
// inline teardown may itself have been interrupted) it tears the exposure down by derived
// identity and best-effort foreground-deletes the mover Job + its creds Secret. Teardown can no
// longer CREATE (cleanupVolumeExposure derives, never Exposes), which is what makes sweeping the
// never-exposed phases safe: for those it is a handful of tolerated NotFounds.
//
// An exposure-cleanup failure HOLDS the finalizer: the error requeues finalize with backoff
// until the deletes succeed, because removing the finalizer over unswept residue would orphan a
// cluster-scoped, Retain-parked VolumeSnapshotContent with its owning record gone — the exact
// leak shape the audit root-caused. Nothing in the repository is ever erased (adr/0009).
func (r *BackupReconciler) finalize(ctx context.Context, backup *cbv1.Backup) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(backup, apiconst.FinalizerBackup) {
		return ctrl.Result{}, nil
	}

	var errs []error
	for i := range backup.Status.Volumes {
		vol := &backup.Status.Volumes[i]
		if vol.Phase == status.VolumePhaseSkipped {
			continue // never exposed, never had a Job
		}
		if err := r.cleanupVolumeExposure(ctx, backup, vol.Pvc); err != nil {
			errs = append(errs, fmt.Errorf("exposure cleanup of PVC %s on delete: %w", vol.Pvc, err))
			continue
		}
		r.deleteMoverJobAndSecret(ctx, moverNamePrefix(backup.Namespace, backup.Name, vol.Pvc))
	}
	if len(errs) > 0 {
		return ctrl.Result{}, errors.Join(errs...)
	}

	// The manifest half leaves residue of its own, and one piece of it is a live privilege: the
	// transient RoleBinding in the tenant namespace. Unconditional because status may not name
	// it — a Backup deleted between the Job create and the first status write has a running
	// capture and nothing recorded — and because both deletes tolerate NotFound.
	r.teardownManifests(ctx, backup, manifestsJobPrefix(backup.Namespace, backup.Name))

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

// resolveRun returns the effective run configuration for this Backup, preferring the
// MATERIALIZED spec.run and falling back to a pull from the parent ClusterBackup named by the
// crystalbackup.io/cluster-backup link label (adr/0017 §5).
//
// The materialized copy wins because it is the only one that cannot dangle: run records are
// history-limited and garbage-collected while their children live as long as their snapshots,
// and the parent link is a label rather than an ownerReference precisely so GC never cascades.
// A Backup created before this field existed has no copy, so the pull stays as the compatibility
// path — an in-flight run at upgrade time must not be stranded on NoParent for the rest of its
// life. It is a fallback, not a second source of truth: nothing re-reads the parent once
// spec.run is set, so editing a finished run's parent no longer rewrites what that run appears
// to have done.
//
// ok=false (with a nil error) means "nothing resolvable yet" — no materialized run AND either no
// label or a vanished ClusterBackup — which the caller treats as a degrade-and-requeue, never a
// hard failure.
func (r *BackupReconciler) resolveRun(ctx context.Context, backup *cbv1.Backup) (*cbv1.BackupRunSpec, bool, error) {
	if backup.Spec.Run != nil {
		return backup.Spec.Run, true, nil
	}
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
	return &cb.Spec.BackupRunSpec, true, nil
}

// reasonLocationUnreadable is the gate reason for a location that exists as a reference but
// cannot be read — an API error rather than a NotFound. Distinct from LocationNotFound because
// the operator action differs: one is "create it", the other is "look at RBAC or the API server".
const reasonLocationUnreadable = "LocationUnreadable"

// resolveRunContext resolves everything a run needs before any volume moves: the location (either
// plane), its BackupRepository, the repository password, and — on the namespace plane — the
// identity its hooks will run as. done=true means the caller must return immediately, which covers
// every gate and every hard error; done=false means rc is usable.
//
// Extracted from Reconcile rather than inlined because each of these is a separate "is this run
// allowed to start" question with its own failure message, and Reconcile's job is to sequence the
// phases, not to enumerate preconditions.
func (r *BackupReconciler) resolveRunContext(ctx context.Context, backup *cbv1.Backup, run *cbv1.BackupRunSpec,
) (rc *backupRunContext, res ctrl.Result, done bool, err error) {
	binding, reason, message, ok := r.resolveBackupLocation(ctx, backup)
	if !ok {
		res, err = r.gate(ctx, backup, reason, message)
		return nil, res, true, err
	}

	repoName := binding.Name
	if binding.Namespaced() {
		repoName = namespacedRepositoryName(binding.Namespace, binding.Name)
	}
	var repo cbv1.BackupRepository
	if getErr := r.Get(ctx, client.ObjectKey{Name: repoName}, &repo); getErr != nil {
		if apierrors.IsNotFound(getErr) {
			res, err = r.gate(ctx, backup, "RepositoryNotReady",
				fmt.Sprintf("BackupRepository %q does not exist yet", repoName))
			return nil, res, true, err
		}
		return nil, ctrl.Result{}, true, fmt.Errorf("get BackupRepository %s: %w", repoName, getErr)
	}
	if !repo.Status.Initialized {
		res, err = r.gate(ctx, backup, "RepositoryNotReady",
			fmt.Sprintf("BackupRepository %q is not initialized yet", repoName))
		return nil, res, true, err
	}

	// Hooks on the NAMESPACE plane must name the ServiceAccount they run as (adr/0018). Without an
	// identity the exec would fall back to the operator's own, which is precisely the escalation:
	// a tenant who can write a BackupSchedule would be making the platform run commands with
	// privileges they do not hold. Gate rather than execute; the fix is one field.
	if binding.Namespaced() && hooksDeclared(run.Hooks) && run.Hooks.ServiceAccountName == "" {
		res, err = r.gate(ctx, backup, "HooksNeedServiceAccount",
			"hooks on a namespaced BackupLocation must set hooks.serviceAccountName — a ServiceAccount "+
				"in this namespace, granted `create pods/exec`, that the operator impersonates to run them")
		return nil, res, true, err
	}

	// The restic repository password the mover needs: the platform DEK on the cluster plane, the
	// tenant's own key on the namespace plane.
	password, reason, message, ok := r.ensureRepositoryPassword(ctx, binding)
	if !ok {
		res, err = r.gate(ctx, backup, reason, message)
		return nil, res, true, err
	}

	return &backupRunContext{
		scheduleRef:         backup.Spec.ScheduleRef,
		run:                 backup.Name,
		clusterID:           binding.ClusterID,
		tenant:              r.tenantFor(ctx, backup.Namespace),
		repoName:            repoName,
		repoURL:             repo.Status.RepositoryURL,
		dek:                 password,
		s3CredsSecret:       binding.S3.CredentialsSecretRef.Name,
		s3Connections:       binding.S3.Connections,
		credsNamespace:      binding.CredsNamespace,
		retention:           binding.Retention,
		mode:                binding.Mode,
		backoffLimit:        run.BackoffLimit,
		maxConcurrentMovers: run.MaxConcurrentMovers,
	}, ctrl.Result{}, false, nil
}

// resolveBackupLocation resolves the location a Backup names, from either plane, and reduces it
// to a locationBinding. ok=false carries a reason/message for the caller's gate.
//
// The namespace-plane lookup is deliberately scoped to the BACKUP'S OWN namespace and nothing
// else. That is the structural confinement the whole plane rests on (02-api.md): a Backup can
// only ever reach a location sitting beside it, so no reference — however it was written — can
// point at another tenant's storage or key. It is a property of the lookup, not a check that
// could be skipped.
func (r *BackupReconciler) resolveBackupLocation(ctx context.Context, backup *cbv1.Backup) (binding *locationBinding, reason, message string, ok bool) {
	name := backup.Spec.LocationRef.Name

	if backup.Spec.LocationRef.Kind == kindBackupLocation {
		var loc cbv1.BackupLocation
		if err := r.Get(ctx, client.ObjectKey{Namespace: backup.Namespace, Name: name}, &loc); err != nil {
			if apierrors.IsNotFound(err) {
				return nil, "LocationNotFound", fmt.Sprintf("BackupLocation %s/%s not found", backup.Namespace, name), false
			}
			return nil, reasonLocationUnreadable, fmt.Sprintf("get BackupLocation %s/%s: %v", backup.Namespace, name, err), false
		}
		// The effective cluster ID is pinned by the location controller and composes the
		// repository path; without it there is no repository to write to yet.
		if loc.Status.ClusterID == "" {
			return nil, "LocationNotReady",
				fmt.Sprintf("BackupLocation %s/%s has not resolved its cluster ID yet", backup.Namespace, name), false
		}
		return bindingFromNamespacedLocation(&loc), "", "", true
	}

	var cbl cbv1.ClusterBackupLocation
	if err := r.Get(ctx, client.ObjectKey{Name: name}, &cbl); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, "LocationNotFound", fmt.Sprintf("ClusterBackupLocation %q not found", name), false
		}
		return nil, reasonLocationUnreadable, fmt.Sprintf("get ClusterBackupLocation %s: %v", name, err), false
	}
	return bindingFromClusterLocation(&cbl, r.OperatorNamespace), "", "", true
}

// ensureRepositoryPassword returns the plaintext restic repository password for the run: the
// platform DEK on the cluster plane, the tenant's own key on the namespace plane. On any failure
// it returns ok=false with a Secret-naming reason/message (never key material) for the caller to
// fold into the Ready condition.
func (r *BackupReconciler) ensureRepositoryPassword(ctx context.Context, binding *locationBinding) (password, reason, message string, ok bool) {
	if binding.Namespaced() {
		p, err := keys.NewUserKeyManager(r.Client).
			EnsureUserPassword(ctx, binding.Namespace, binding.Name, binding.PasswordSecretRef)
		if err != nil {
			return "", "PasswordUnavailable", err.Error(), false
		}
		return p, "", "", true
	}
	var loc cbv1.ClusterBackupLocation
	if err := r.Get(ctx, client.ObjectKey{Name: binding.Name}, &loc); err != nil {
		return "", reasonLocationUnreadable, fmt.Sprintf("get ClusterBackupLocation %s: %v", binding.Name, err), false
	}
	return resolvePlatformDEKCommon(ctx, r.Client, r.Secrets, r.OperatorNamespace, &loc)
}

// resolvePlatformDEKCommon is the shared platform-DEK resolution: KEK Secret → age wrapper →
// mint-once/reuse-forever DEK. A package function because the Backup controller AND both
// restore controllers need it identically; failures carry a Secret-naming reason/message
// (never key material) for the caller's Ready condition.
func resolvePlatformDEKCommon(ctx context.Context, c client.Client, secretsReader *secrets.ByNameReader,
	operatorNamespace string, loc *cbv1.ClusterBackupLocation,
) (dek, reason, message string, ok bool) {
	kekName := loc.Spec.Encryption.ClusterKEKSecretRef.Name

	identity, err := secretsReader.GetValue(ctx, operatorNamespace, kekName, kekIdentityDataKey)
	if err != nil {
		return "", "KEKUnavailable", fmt.Sprintf("read cluster KEK secret %s/%s: %v", operatorNamespace, kekName, err), false
	}
	wrapper, err := keys.NewAgeWrapper(string(identity))
	if err != nil {
		return "", "KEKInvalid", fmt.Sprintf("parse cluster KEK secret %s/%s: %v", operatorNamespace, kekName, err), false
	}
	d, err := keys.NewDEKManager(c, wrapper, operatorNamespace).EnsureDEK(ctx, loc.Name)
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
		return r.advanceSnapshotting(ctx, backup, vol, rc)
	case status.VolumePhaseUploading:
		return r.advanceUploading(ctx, backup, vol, rc)
	default:
		return "", nil
	}
}

// advancePending resolves the exposer for the source PVC and starts the exposure. A storage
// class with no CSI snapshot support (exposer.ErrUnsupported) makes the volume Skipped /
// CSISnapshotUnsupported — and a Skipped volume is NEUTRAL in the roll-up: it counts neither for
// nor against the Backup's phase, so it never on its own makes the Backup PartiallyCompleted, and
// certainly never Failed (status.RollUpVolumePhases is the authority, and says so at length: a
// namespace holding one permanently unsnapshottable PVC must not alarm on every run, forever). The
// skip stays visible per-volume, in status.volumes[].phase + reason. SnapshottingHooks (M4) are
// skipped in M1: Pending goes straight to Snapshotting.
//
// Between resolution and exposure sits the ACTIVE pre-check (exposer.Precheck): a class can exist,
// name a live driver, and still be unserveable because its snapshotter credentials are absent.
// That verdict is Failed / SnapshotPrecheckFailed — NOT Skipped — because unlike "this CSI cannot
// snapshot", it describes a cluster somebody broke and somebody can fix. See
// backupReasonPrecheckFailed for why it fails rather than gates.
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

	// The ACTIVE pre-check, and it runs HERE — before Expose, on the same pass, with nothing
	// created yet. That ordering is the whole safety property: Expose cuts a real VolumeSnapshot in
	// the tenant namespace, so a pre-check that ran after it would turn every refusal into leaked
	// snapshot objects the crucible's leak-check would (rightly) report as a regression caused by
	// this very feature. Registry.For deliberately does NOT fold this in — see its doc.
	if err := ex.Precheck(ctx); err != nil {
		if !errors.Is(err, exposer.ErrPrecheckFailed) {
			// Precheck is total today — every verdict it can reach is a structural refusal — so
			// this branch is not reachable from the current implementation. It is not padding: it
			// is the guard that keeps a FUTURE check whose failure is transient (one that should
			// requeue, not fail the volume) from silently inheriting "fail this volume" simply by
			// being added.
			return fmt.Errorf("pre-check exposure for PVC %s/%s: %w", backup.Namespace, vol.Pvc, err)
		}
		vol.Phase = status.VolumePhaseFailed
		vol.Reason = backupReasonPrecheckFailed
		// Warning, not Normal, and carrying the failing check's own detail verbatim: the reason
		// string says which gate refused, the Event says what is actually missing (the Secret's
		// namespace/name and the class that asked for it), which is the difference between an
		// alert an operator can act on and one they have to go investigate.
		r.Recorder.Eventf(backup, nil, corev1.EventTypeWarning, "VolumeFailed", "SnapshotPrecheck",
			"backup of PVC %s cannot start: %v", vol.Pvc, err)
		return nil
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
//
// The not-ready branch is bounded twice, by two deadlines on one clock: snapshotProgressDeadline
// for an origin VolumeSnapshot nobody has touched (unattended, not slow), and the much longer
// snapshotReadyDeadline for one that WAS picked up and never became ready. Waiting on either
// forever is how a dead snapshotter turns into a run that never finishes and never alarms.
//
// Like advanceUploading, it returns the PVC name when it JUST made the volume terminal, so
// Reconcile can tear the exposure down after — never before — the terminal status write.
func (r *BackupReconciler) advanceSnapshotting(ctx context.Context, backup *cbv1.Backup, vol *cbv1.VolumeStatus, rc *backupRunContext) (string, error) {
	ex, exposure, err := r.reconstructExposure(ctx, backup, vol.Pvc)
	if err != nil {
		return "", fmt.Errorf("reconstruct exposure for PVC %s/%s: %w", backup.Namespace, vol.Pvc, err)
	}
	ready, err := ex.Ready(ctx, exposure)
	if err != nil {
		return "", fmt.Errorf("check exposure readiness for PVC %s/%s: %w", backup.Namespace, vol.Pvc, err)
	}
	if !ready {
		switch r.snapshotDeadlineExceeded(ctx, ex, exposure) {
		case backupReasonSnapshotProgressDeadline:
			vol.Phase = status.VolumePhaseFailed
			vol.Reason = backupReasonSnapshotProgressDeadline
			r.Recorder.Eventf(backup, nil, corev1.EventTypeWarning, "VolumeFailed", "SnapshotProgress",
				"backup of PVC %s gave up after %s: VolumeSnapshot %s/%s was never picked up — no "+
					"VolumeSnapshotContent was bound to it and no error was recorded on it, so nothing is "+
					"watching its VolumeSnapshotClass; check the cluster's CSI snapshot-controller, then the "+
					"csi-snapshotter sidecar of the driver behind that class",
				vol.Pvc, snapshotProgressDeadline, exposure.OriginNamespace, exposure.OriginVSName)
			return vol.Pvc, nil // request teardown once Reconcile has persisted this terminal result
		case backupReasonSnapshotReadyDeadline:
			// The snapshot WAS picked up and never finished. Whatever the storage system had to say
			// about that is on the origin VolumeSnapshot as a Warning Event, and it travels into the
			// reason so `kubectl get backup` answers the question instead of posing it.
			detail := r.latestWarningEvent(ctx, exposure.OriginNamespace, exposure.OriginVSName)
			vol.Phase = status.VolumePhaseFailed
			vol.Reason = deadlineReason(backupReasonSnapshotReadyDeadline, detail)
			r.Recorder.Eventf(backup, nil, corev1.EventTypeWarning, "VolumeFailed", "SnapshotReady",
				"backup of PVC %s gave up after %s: VolumeSnapshot %s/%s was acknowledged (a "+
					"VolumeSnapshotContent is bound) but never became readyToUse, so the request reached the "+
					"snapshot-controller and stopped there; check the csi-snapshotter sidecar of the driver "+
					"behind its VolumeSnapshotClass, and the driver's own logs%s",
				vol.Pvc, snapshotReadyDeadline, exposure.OriginNamespace, exposure.OriginVSName,
				eventSuffix(detail))
			return vol.Pvc, nil // request teardown once Reconcile has persisted this terminal result
		}
		return "", nil // still binding the static re-bind / temp PVC; requeue
	}

	identity := restic.DataIdentity(rc.clusterID, rc.tenant, backup.Namespace, vol.Pvc, rc.scheduleRef, rc.run)
	prefix := moverNamePrefix(backup.Namespace, backup.Name, vol.Pvc)
	moverName := prefix + "-mover"
	labels := exposureLabels(backup, vol.Pvc)

	resticArgs := resticBackupArgs(identity)
	// PVC-meta tags (adr/0016 §4, best-effort): record the source claim's requested
	// capacity/class/modes on the snapshot so ClusterRestore can recreate the PVC from the
	// repository alone. Informational and additive — a claim that vanished between exposure
	// and now simply yields a snapshot without them (the documented fallback covers it).
	var srcPVC corev1.PersistentVolumeClaim
	if err := r.Get(ctx, client.ObjectKey{Namespace: backup.Namespace, Name: vol.Pvc}, &srcPVC); err == nil {
		storageClass := ""
		if srcPVC.Spec.StorageClassName != nil {
			storageClass = *srcPVC.Spec.StorageClassName
		}
		modes := make([]string, 0, len(srcPVC.Spec.AccessModes))
		for _, m := range srcPVC.Spec.AccessModes {
			modes = append(modes, string(m))
		}
		capacity := srcPVC.Spec.Resources.Requests[corev1.ResourceStorage]
		for _, tag := range restic.PVCMetaTags(capacity.Value(), storageClass, modes) {
			resticArgs = append(resticArgs, "--tag", tag)
		}
	}

	// Cluster-wide mover concurrency gate. If this volume's mover Job does not exist yet and the
	// cascade is already at maxConcurrentMovers, hold the volume in Snapshotting (its exposure stays
	// ready) and requeue for a free slot. An already-existing Job means we are re-adopting after a
	// restart, never blocking — so an in-flight mover is never counted out of its own slot.
	if blocked, err := r.moverSlotBlocked(ctx, moverName, rc.repoName, rc.maxConcurrentMovers); err != nil {
		return "", err
	} else if blocked {
		return "", nil
	}

	if err := ensureMoverCredsSecret(ctx, r.maintenanceDeps(), moverName, rc.dek, rc.s3CredsSecret, rc.credsNamespace, labels); err != nil {
		return "", err
	}

	job := mover.BuildJob(mover.JobRequest{
		Name:          moverName,
		Namespace:     r.OperatorNamespace,
		Image:         r.MoverImage,
		Operation:     mover.OpBackup,
		Profiles:      r.MoverProfiles,
		Placement:     r.MoverPlacement,
		ResticArgs:    resticArgs,
		RepoURL:       rc.repoURL,
		S3Connections: rc.s3Connections,
		SecretName:    moverName,
		PVC:           &mover.PVCMount{ClaimName: exposure.ExposedPVCName, MountPath: identity.Path},
		BackoffLimit:  rc.backoffLimit,
		TTLSeconds:    moverJobTTLSeconds,
		Labels:        labels,
		// The W3C handover (spec/05-observability.md §5). The parent it names is the `mover` span
		// for THIS PVC — a span that does not exist yet and will not until the Job finishes, at
		// which point advanceUploading emits it with the very id derived here. Both sides compute
		// that id from the Backup's UID and the PVC name, so they agree without communicating,
		// which is the only way they could: the env var has to be written before the span it
		// points at can be emitted. Nil, and therefore absent from the pod spec entirely, when
		// tracing is off.
		TraceEnv: tracing.JobEnv(backupAnchor(backup).StepContext(ctx, tracing.StepPVC(tracing.StepMover, vol.Pvc))),
		// Soft-spread the cascade's movers across nodes so a wide fan-out does not pile its data
		// movement onto one kubelet.
		SpreadOverLabels: map[string]string{apiconst.LabelManagedBy: apiconst.ManagedByValue},
	})
	// No ownerReference: the mover Job is in the operator namespace and the Backup in a tenant
	// namespace, so a cross-namespace ownerRef is illegal. The Job is tracked by its
	// deterministic name + labels and re-adopted by Get (AlreadyExists on Create).
	created := false
	if err := r.Create(ctx, job); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return "", fmt.Errorf("create mover Job %s/%s: %w", r.OperatorNamespace, moverName, err)
		}
	} else {
		created = true
	}

	// Create-then-verify closes the mover⇄unlock TOCTOU (adr/0015): moverSlotBlocked's
	// QuiescenceRequired check and this Create are not atomic (separated by two Secret reads + a
	// create), so a stale-lock unlock can be enqueued in between. If we just created this Job and a
	// lock-removing op is now pending for the repo, undo the Create and hold the volume in
	// Snapshotting — otherwise the unlock's drain census (which reads the cached client and lags
	// informer propagation) could miss this fresh Job and run `unlock --remove-all` while it holds a
	// repository lock, the exact corruption the mutex exists to prevent. Only the fresh-create path
	// needs this: a re-adopted (pre-existing) Job is already cache-visible to the drain. moverBlocking
	// is incremented at unlock-enqueue and held for the whole pending+in-flight lifetime, so this
	// re-check cannot miss a concurrently-enqueued unlock.
	if created && r.Queue != nil && rc.repoName != "" && r.Queue.QuiescenceRequired(rc.repoName) {
		r.deleteMoverJobAndSecret(ctx, prefix)
		return "", nil // stay in Snapshotting; a requeue picks a clean slot once the unlock resolves.
	}

	// The exposure has done its job: the snapshot is readyToUse, the static VS/VSC re-bind is
	// done, the temp PVC is bound, and a mover now exists to mount it. This line is reached
	// EXACTLY ONCE per volume — every earlier return leaves the volume in Snapshotting for
	// another pass — which is what makes it the one honest place to close the timer.
	if start, ok := exposer.StartedAt(ctx, r.Client, exposure); ok {
		metrics.RecordExposureReadyWait(ctx, backup.Namespace, rc.tenant, ex.Kind(),
			moverMetricClusterID(backup, rc), time.Since(start))
		r.emitExposureSpans(ctx, backup, rc, vol.Pvc, ex.Kind(), exposure, start)
	}

	vol.Phase = status.VolumePhaseUploading
	return "", nil
}

// snapshotDeadlineExceeded classifies an exposure that is NOT ready yet: it returns the
// VolumeStatus.reason the volume should fail with, or "" while the wait is still legitimate.
//
// Two bounds on ONE clock — the origin VolumeSnapshot's own creationTimestamp, which costs no new
// CRD field and survives an operator restart for free — and the split between them is entirely
// about how much evidence of abandonment there is:
//
//   - UNACKNOWLEDGED past snapshotProgressDeadline (15m). Nothing has written to the object's
//     status at all: no bound VolumeSnapshotContent, no recorded error. The external
//     snapshot-controller binds content within seconds of any request it can see, so this is strong
//     evidence that nothing is watching the VolumeSnapshotClass. Cheap to be quick about.
//   - ACKNOWLEDGED past snapshotReadyDeadline (2h). Something did pick it up and readyToUse never
//     came. This is WEAK evidence — a slow driver looks identical — which is why the bound is eight
//     times longer and why exposer.SnapshotProgress declines to draw the conclusion itself. It is
//     drawn here, in the controller, because this is the layer that knows a backup is hanging.
//
// The order matters: unacknowledged is checked first, so a snapshot nobody touched is reported as
// unattended (the actionable diagnosis) at 15 minutes rather than as "not ready" at two hours.
//
// Every "I do not know" answer returns "", deliberately and asymmetrically. An unreadable origin
// snapshot (deleted, RBAC, an apiserver having a bad second) is not evidence of anything, and this
// function's non-empty branches terminate somebody's backup — so the burden of proof sits entirely
// on the evidence, never on its absence. A pass that cannot tell simply waits for the next one.
func (r *BackupReconciler) snapshotDeadlineExceeded(ctx context.Context, ex exposer.SnapshotExposer, exposure *exposer.Exposure) string {
	progress, ok := ex.Progress(ctx, exposure)
	if !ok || progress.StartedAt.IsZero() {
		return ""
	}
	elapsed := time.Since(progress.StartedAt)
	switch {
	case !progress.Acknowledged && elapsed > snapshotProgressDeadline:
		return backupReasonSnapshotProgressDeadline
	case progress.Acknowledged && elapsed > snapshotReadyDeadline:
		return backupReasonSnapshotReadyDeadline
	default:
		return ""
	}
}

// emitExposureSpans emits the `snapshot` and `expose` spans of spec/05-observability.md §5 for one
// volume, from the same exactly-once point the exposure histogram is closed at.
//
// §5 draws them as two nodes; the controller drives them as one phase (Snapshotting), because
// there is no reconcile boundary between "the driver has cut the snapshot" and "the cut is
// mountable by a mover in another namespace". The only observable boundary is the CSI driver's own
// status.creationTime on the VolumeSnapshot — and that field is OPTIONAL in the CSI spec, so
// several drivers never write it.
//
// So: with the cut time, two spans, split where the storage system says the copy was taken. Without
// it, ONE `expose` span covering the whole wait. Inventing the boundary would be worse than
// merging: it would put a fabricated number on how long someone's storage took, which is exactly
// the question the two spans exist to answer.
func (r *BackupReconciler) emitExposureSpans(
	ctx context.Context, backup *cbv1.Backup, rc *backupRunContext,
	pvc, exposerKind string, exposure *exposer.Exposure, start time.Time,
) {
	anchor := backupAnchor(backup)
	if !anchor.Valid() {
		return
	}
	attrs := backupSpanAttrs(backup, rc)
	attrs = append(attrs, tracing.StringAttr(tracing.AttrPVC, pvc)...)
	attrs = append(attrs, tracing.StringAttr(tracing.AttrExposer, exposerKind)...)

	end := time.Now()
	exposeFrom := start
	if cut, ok := exposer.CutAt(ctx, r.Client, exposure); ok && cut.After(start) && cut.Before(end) {
		anchor.EmitStep(ctx, tracing.StepPVC(tracing.StepSnapshot, pvc), "snapshot",
			start, cut, nil, attrs...)
		exposeFrom = cut
	}
	anchor.EmitStep(ctx, tracing.StepPVC(tracing.StepExpose, pvc), "expose",
		exposeFrom, end, nil, attrs...)
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

	// (a) Repository mover⇄unlock mutex (reader side): hold a new mover back while a lock-removing
	// op is queued/running for this repo. Independent of the concurrency limit (unset by default).
	if r.Queue != nil && repoName != "" && r.Queue.QuiescenceRequired(repoName) {
		return true, nil
	}

	// (b) Cluster-wide concurrency cap. Unset ⇒ unlimited ⇒ the common single-tenant case pays for
	// nothing beyond the mutex check above.
	if limit <= 0 {
		return false, nil
	}
	movers, err := listMoverJobs(ctx, r.Client, r.OperatorNamespace)
	if err != nil {
		return false, fmt.Errorf("list mover Jobs for the concurrency gate: %w", err)
	}
	return !concurrency.CanStartMover(concurrency.RunningMoverJobs(movers), limit), nil
}

// listMoverJobs returns the per-PVC data-mover Jobs in the operator namespace — those carrying the
// managed-by AND a per-PVC label, so repository-init/maintenance Jobs (managed-by, no PVC label) are
// excluded. Backup AND restore movers both carry these labels, so the census spans both — exactly
// what the concurrency gate and the unlock drain-wait need (a restore holds a repository lock like
// a backup does, adr/0015). A package function shared with the restore engine.
func listMoverJobs(ctx context.Context, c client.Client, operatorNamespace string) ([]batchv1.Job, error) {
	var jobs batchv1.JobList
	if err := c.List(ctx, &jobs, client.InNamespace(operatorNamespace),
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
// the unlock drain-wait open. It is the reader census the mover⇄unlock mutex drains before an
// exclusive lock-removal runs.
func activeMoverCount(ctx context.Context, c client.Client, operatorNamespace string) (int, error) {
	movers, err := listMoverJobs(ctx, c, operatorNamespace)
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
//
// A Job that is neither complete nor failed is normally "still running; requeue" — and that used to
// be unconditional, which is how six movers whose pods could not mount their temp clone PVC sat in
// ContainerCreating for THIRTY-SIX HOURS while the run reported ordinary progress the whole time.
// The bound added here is moverStartDeadline, and it is emphatically NOT a bound on the upload: see
// moverStalled, whose predicate is "the pod never started", never "the mover is slow".
func (r *BackupReconciler) advanceUploading(ctx context.Context, backup *cbv1.Backup, vol *cbv1.VolumeStatus, rc *backupRunContext) (string, error) {
	moverName := moverNamePrefix(backup.Namespace, backup.Name, vol.Pvc) + "-mover"

	var job batchv1.Job
	if err := r.Get(ctx, client.ObjectKey{Namespace: r.OperatorNamespace, Name: moverName}, &job); err != nil {
		if apierrors.IsNotFound(err) {
			// The Job is momentarily absent. This is almost always the Job informer lagging the
			// create we just issued; occasionally it is our own teardown (during finalize) racing
			// a stale reconcile. We deliberately neither re-drive to Snapshotting (which would
			// RE-CREATE the exposure + Job and, if the Backup is being deleted, leak a clone that
			// outlives it) NOR mark the volume Failed (which would false-fail on informer lag).
			// We simply wait and requeue.
			//
			// THE PER-PHASE TIMEOUT BELOW DOES NOT COVER THIS BRANCH, and the note that used to
			// promise it would ("deferred to task #22") was wrong for a structural reason worth
			// recording rather than re-deferring: every deadline in this controller is measured
			// against a durable clock that belongs to the object being waited on — the origin
			// VolumeSnapshot's creationTimestamp, the mover Job's. When the Job is ABSENT there is no
			// such clock, and the only candidates are the Backup's own creationTimestamp (which says
			// nothing about this volume) or a new phase-entry timestamp on VolumeStatus (a CRD field,
			// and a bigger change than this lot). A genuinely lost Job therefore still requeues
			// forever, and the alert is what catches it — CrystalbackupBackupStalled reads a
			// state-derived series precisely so it fires on the phases no in-controller clock can see.
			return "", nil
		}
		return "", fmt.Errorf("get mover Job %s/%s: %w", r.OperatorNamespace, moverName, err)
	}

	complete := job.Status.Succeeded >= 1 || jobConditionTrue(&job, batchv1.JobComplete)
	failed := jobConditionTrue(&job, batchv1.JobFailed) || job.Status.Failed > rc.backoffLimit
	if !complete && !failed {
		// Not finished — but is it running, or merely existing? A mover whose pod never got off the
		// ground has moved no data, so failing it here costs nothing and ends the hang; a mover whose
		// pod IS running is left entirely alone, however long it takes.
		if detail, stalled := r.moverStalled(ctx, &job); stalled {
			vol.Phase = status.VolumePhaseFailed
			vol.Reason = deadlineReason(backupReasonMoverStartDeadline, detail)
			r.Recorder.Eventf(backup, nil, corev1.EventTypeWarning, "VolumeFailed", "MoverStart",
				"backup of PVC %s gave up after %s: mover Job %s/%s was created but no pod of it ever "+
					"reached Running, so restic never read a byte — the pod is stuck before start "+
					"(image pull, scheduling, or mounting the temp clone PVC); `kubectl describe pod -n %s "+
					"-l %s=%s` has the kubelet's full account%s",
				vol.Pvc, r.effectiveMoverStartDeadline(), r.OperatorNamespace, moverName, r.OperatorNamespace,
				batchv1.JobNameLabel, moverName, eventSuffix(detail))
			// No stale-lock unlock is enqueued, unlike the hard-killed-mover path below: a mover that
			// never started never ran restic, so it cannot be holding a repository lock. Enqueuing an
			// `unlock --remove-all` here would take the repository's exclusive lane, and drain every
			// healthy mover on it, to clear a lock that by construction does not exist.
			return vol.Pvc, nil // request teardown once Reconcile has persisted this terminal result
		}
		return "", nil // still running; requeue
	}

	// The Job is terminal, so status.failed is final: it is the number of pods that died before
	// one succeeded (or gave up), i.e. the retries this mover consumed against its backoffLimit.
	// Read once, here, rather than tracked incrementally — see metrics.RecordMoverJobRetries for
	// why the terminal read is the one that cannot double-count. This is the only pass that sees
	// the volume go terminal (the caller persists that phase and never re-enters).
	metrics.RecordMoverJobRetries(backup.Namespace, rc.tenant, moverMetricClusterID(backup, rc), job.Status.Failed)

	result, node, rerr := readMoverResult(ctx, r.Client, r.OperatorNamespace, moverName)
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
	emitMoverSpan(ctx, backup, rc, vol, &job)
	return vol.Pvc, nil // request teardown once Reconcile has persisted this terminal result
}

// emitMoverSpan emits §5's per-PVC `mover` span, spanning the Job's whole execution — which no
// single reconcile witnessed, and which for a large volume may have outlived the process that
// started it. Its window comes off the Job's own status, written by the Job controller and
// therefore durable; the mover shim's `restic.backup` span nests inside it, having been handed
// this span's id in TRACEPARENT when the Job was created.
//
// Emitted from the pass that first observes the Job terminal — the same exactly-once point
// RecordMoverJobRetries fires at, and for the same reason: the caller persists this volume's
// terminal phase and the top-of-Reconcile short-circuit bars re-entry, so this line is reached
// once per volume per Backup.
func emitMoverSpan(ctx context.Context, backup *cbv1.Backup, rc *backupRunContext, vol *cbv1.VolumeStatus, job *batchv1.Job) {
	anchor := backupAnchor(backup)
	if !anchor.Valid() {
		return
	}
	attrs := backupSpanAttrs(backup, rc)
	attrs = append(attrs, tracing.StringAttr(tracing.AttrPVC, vol.Pvc)...)
	attrs = append(attrs, tracing.StringAttr(tracing.AttrNode, vol.Node)...)
	attrs = append(attrs, tracing.StringAttr(tracing.AttrSnapshotID, vol.SnapshotID)...)
	if vol.AddedBytes > 0 {
		attrs = append(attrs, attribute.Int64(tracing.AttrBytesAdded, vol.AddedBytes))
	}

	var spanErr error
	if vol.Phase == status.VolumePhaseFailed {
		spanErr = fmt.Errorf("mover for PVC %s failed: %s", vol.Pvc, vol.Reason)
	}
	start, end := jobWindow(job)
	anchor.EmitStep(ctx, tracing.StepPVC(tracing.StepMover, vol.Pvc), "mover", start, end, spanErr, attrs...)
}

// teardownVolume tears an exposure + mover Job + creds Secret down after its terminal result has
// been persisted, best-effort — it is the RESPONSIVE half of teardown (objects go the moment the
// volume finishes), while the terminal re-entry sweep (ensureTerminalTeardown) is the RELIABLE
// half that verifies and re-runs anything this pass missed. Called by Reconcile AFTER the status
// write so a status-write conflict never deletes the Job before the result it carries is
// recorded.
func (r *BackupReconciler) teardownVolume(reconcileCtx context.Context, backup *cbv1.Backup, pvcName string) {
	// DETACHED from the reconcile context: teardown runs on the same pass that made the volume
	// terminal, AFTER the status write — and if the manager is shutting down, controller-runtime
	// has already cancelled the reconcile context, which would fail every delete below on a pass
	// that (for a mid-run volume, Backup not yet terminal) may not be revisited for this PVC.
	// Detachment lets an orderly shutdown finish the deletes; the fanout proved it is NOT
	// sufficient alone (a killed process takes its detached contexts with it), which is why the
	// terminal re-entry sweep exists. The maintenance path does the same (see
	// maintenanceCleanupTimeout).
	ctx, cancel := context.WithTimeout(context.WithoutCancel(reconcileCtx), backupTeardownTimeout)
	defer cancel()

	if err := r.cleanupVolumeExposure(ctx, backup, pvcName); err != nil {
		logf.FromContext(ctx).Error(err, "best-effort exposure cleanup after mover finish failed",
			"backup", backup.Name, "pvc", pvcName)
	}
	r.deleteMoverJobAndSecret(ctx, moverNamePrefix(backup.Namespace, backup.Name, pvcName))
}

// ensureTerminalTeardown is the terminal short-circuit's re-entry sweep: before a terminal Backup
// goes quiet forever, verify its teardown COMPLETED, and re-run it if not. The per-pass teardown
// above (teardownVolume, called the moment each volume goes terminal) is best-effort by design —
// its failures are swallowed and, worse, no in-process effort survives the process: a kill at any
// instant between the durable terminal status write and the last delete used to strand the
// exposure objects permanently, because this short-circuit barred every later pass and the
// cluster-scoped, Retain-parked origin VolumeSnapshotContent has no owner to garbage-collect it.
// That is the leak the audit root-caused (the fanout's residual VSC), and re-entry — not a wider
// in-flight effort — is the only shape that closes it under SIGKILL at ANY instant.
//
// The sweep re-runs cleanupVolumeExposure for every volume that may have exposed (everything but
// Skipped; Pending included, because a crash between Expose and the first status write leaves a
// live origin VS on a still-Pending volume) plus the mover Job/Secret and manifest residue, all
// idempotent and derive-only (nothing here can create). Only when every exposure teardown
// SUCCEEDED is AnnotationExposuresCleaned stamped; from then on the short-circuit returns without
// touching anything, preserving the terminal record exactly as before. On any failure the marker
// is withheld and the error requeues this pass with backoff — and since controller-runtime
// re-reconciles every object on startup, a sweep the dying process could not finish is re-run by
// the next process within seconds of election.
//
// Runs on the live reconcile context deliberately: unlike teardownVolume there is nothing to
// detach FOR — if shutdown cancels the sweep mid-way, the marker stays absent and re-entry
// finishes the job. Cost: one extra reconcile pass per Backup lifetime (the terminal status write
// itself triggers it via the watch), a handful of idempotent deletes, then the marker seals it.
func (r *BackupReconciler) ensureTerminalTeardown(ctx context.Context, backup *cbv1.Backup) (ctrl.Result, error) {
	if backup.Annotations[apiconst.AnnotationExposuresCleaned] == apiconst.AnnotationExposuresCleanedValue {
		return ctrl.Result{}, nil
	}

	var errs []error
	for i := range backup.Status.Volumes {
		vol := &backup.Status.Volumes[i]
		if vol.Phase == status.VolumePhaseSkipped {
			continue // never exposed, never had a Job (finalize applies the same rule)
		}
		if err := r.cleanupVolumeExposure(ctx, backup, vol.Pvc); err != nil {
			errs = append(errs, fmt.Errorf("sweep exposure of PVC %s: %w", vol.Pvc, err))
			continue
		}
		r.deleteMoverJobAndSecret(ctx, moverNamePrefix(backup.Namespace, backup.Name, vol.Pvc))
	}
	// The manifest half's residue includes a live privilege (the transient RoleBinding), so the
	// sweep covers it unconditionally, exactly as finalize does. Best-effort: its objects are all
	// namespaced and label-stamped, squarely inside the orphan reaper's native charter.
	r.teardownManifests(ctx, backup, manifestsJobPrefix(backup.Namespace, backup.Name))

	if len(errs) > 0 {
		// Marker withheld: the error requeues this sweep with backoff until the deletes succeed.
		return ctrl.Result{}, errors.Join(errs...)
	}

	// The marker asserts "nothing REMAINS", not "deletes were issued" — the difference is the
	// external snapshot-controller's queue. A round-1 validation lane caught it: teardown had
	// done its whole job (origin VS deleted, content policy Delete), yet the VolumeSnapshotContent
	// lingered ~10 minutes under full-suite load waiting on that controller, with the marker
	// already stamped and the sweep gone quiet. Re-verifying instead ALSO accelerates the drain:
	// each pass re-runs reclaimOrphanOriginVSC, which deletes the labelled content directly the
	// moment its VolumeSnapshot is finally gone, rather than waiting on the external resync.
	// The requeue carries no error — draining is expected, not a fault; the reaper (MinAge)
	// remains the backstop if the external controller is broken outright.
	if residue := r.exposureResidueRemains(ctx, backup); residue != "" {
		logf.FromContext(ctx).Info("terminal teardown sweep: exposure residue still draining; re-verifying",
			"backup", backup.Namespace+"/"+backup.Name, "residue", residue)
		return ctrl.Result{RequeueAfter: exposureDrainRecheckInterval}, nil
	}

	base := backup.DeepCopy()
	if backup.Annotations == nil {
		backup.Annotations = map[string]string{}
	}
	backup.Annotations[apiconst.AnnotationExposuresCleaned] = apiconst.AnnotationExposuresCleanedValue
	if err := r.Patch(ctx, backup, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, fmt.Errorf("stamp %s on Backup %s/%s: %w",
			apiconst.AnnotationExposuresCleaned, backup.Namespace, backup.Name, err)
	}
	return ctrl.Result{}, nil
}

// exposureResidueRemains reports the first piece of STORAGE residue still present for this
// Backup's exposures — a labelled VolumeSnapshotContent (cluster-scoped), a labelled
// VolumeSnapshot in the tenant or operator namespace, or a temp clone PVC — as a short
// human-readable description, or "" when everything is genuinely gone. This is the sweep's
// verification read: it deliberately checks what the crucible leak-check checks, scoped by the
// exposure labels (managed-by + run + namespace). A cluster without the snapshot CRDs vacuously
// has no VS/VSC residue (NoMatch tolerated); an errored LIST reports residue rather than
// clean — never let an unreadable cluster read as a clean one.
func (r *BackupReconciler) exposureResidueRemains(ctx context.Context, backup *cbv1.Backup) string {
	sel := client.MatchingLabels{
		apiconst.LabelManagedBy:     apiconst.ManagedByValue,
		apiconst.LabelClusterBackup: backup.Labels[apiconst.LabelClusterBackup],
		apiconst.LabelNamespace:     backup.Namespace,
	}

	vscs := exposer.VolumeSnapshotContentList()
	switch err := r.List(ctx, vscs, sel); {
	case err == nil:
		if len(vscs.Items) > 0 {
			return "VolumeSnapshotContent " + vscs.Items[0].GetName()
		}
	case !apimeta.IsNoMatchError(err):
		return "VolumeSnapshotContent list unreadable: " + err.Error()
	}

	for _, ns := range []string{backup.Namespace, r.OperatorNamespace} {
		vss := exposer.VolumeSnapshotList()
		switch err := r.List(ctx, vss, sel, client.InNamespace(ns)); {
		case err == nil:
			if len(vss.Items) > 0 {
				return "VolumeSnapshot " + ns + "/" + vss.Items[0].GetName()
			}
		case !apimeta.IsNoMatchError(err):
			return "VolumeSnapshot list unreadable: " + err.Error()
		}
	}

	var pvcs corev1.PersistentVolumeClaimList
	if err := r.List(ctx, &pvcs, sel, client.InNamespace(r.OperatorNamespace)); err != nil {
		return "temp clone PVC list unreadable: " + err.Error()
	}
	if len(pvcs.Items) > 0 {
		return "temp clone PVC " + r.OperatorNamespace + "/" + pvcs.Items[0].Name
	}
	return ""
}

// terminalPhaseCommitted disambiguates a failed writeStatus whose intended phase was terminal:
// did the update error client-side yet commit server-side? That seam is real — SIGTERM cancels
// the reconcile context mid-round-trip, and the comment at the writeStatus call site used to
// assume "error ⇒ not persisted", which sealed the teardown forever once the committed terminal
// phase hit the short-circuit on the next pass (the audit's confirmed "ambiguous status write"
// finding, the one place the detached-context fix could not reach).
//
// A clean Conflict is a definitive rejection — no read needed. Anything else warrants one
// uncached GET (the cache may still serve the pre-write object) on a context detached from the
// possibly-already-cancelled reconcile: if the server shows exactly the phase we tried to write,
// the write committed and the caller proceeds to teardown in this same pass. Any doubt — reader
// unavailable, GET failed, phase differs — reports false, and the caller returns the original
// error; the terminal re-entry sweep still heals that path, just later.
func (r *BackupReconciler) terminalPhaseCommitted(reconcileCtx context.Context, backup *cbv1.Backup, writeErr error) bool {
	if r.APIReader == nil || !isTerminalBackupPhase(backup.Status.Phase) {
		return false
	}
	if apierrors.IsConflict(writeErr) {
		return false
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(reconcileCtx), backupTeardownTimeout)
	defer cancel()
	var fresh cbv1.Backup
	if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(backup), &fresh); err != nil {
		return false
	}
	if fresh.Status.Phase != backup.Status.Phase {
		return false
	}
	logf.FromContext(reconcileCtx).Info(
		"status write errored client-side but committed server-side; proceeding to teardown",
		"backup", backup.Namespace+"/"+backup.Name, "phase", backup.Status.Phase, "writeError", writeErr.Error())
	return true
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
		NamePrefix:   moverNamePrefix(backup.Namespace, backup.Name, pvc.Name),
		Labels:       exposureLabels(backup, pvc.Name),
	}
}

// reconstructExposure re-derives an exposer and its Exposure for a PVC without persisting either:
// it re-reads the PVC, re-resolves the exposer (Registry.For), and calls the idempotent Expose to
// obtain the deterministic Exposure (Expose tolerates AlreadyExists, so this converges on an
// existing exposure instead of duplicating it). Used ONLY by the ADVANCE path (the Snapshotting
// Ready() poll) — teardown goes through cleanupVolumeExposure's derive-only route instead,
// because a cleanup that can call Expose can re-CREATE the origin VolumeSnapshot mid-teardown
// and then leak it (the audit's "cleanup path can create" finding).
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

// cleanupVolumeExposure tears a volume's exposure down by DERIVED identity (namespace + name
// prefix + labels — every exposure name is deterministic from those), through the registry's
// TeardownExposure. Two properties are load-bearing, both audit findings:
//
//   - No PVC read: the old shape treated a missing source PVC as "nothing to clean", which is
//     exactly wrong late in a run — the PVC (or its namespace) being gone says nothing about the
//     cluster-scoped, Retain-parked VolumeSnapshotContent still holding a storage snapshot.
//   - No create: the old shape reconstructed via Expose, which can re-create the origin
//     VolumeSnapshot during teardown; a fresh unbound VS then defeats the Retain→Delete restore.
//
// Idempotent and NotFound-tolerant end to end, so the terminal re-entry sweep and finalize can
// re-run it freely.
func (r *BackupReconciler) cleanupVolumeExposure(ctx context.Context, backup *cbv1.Backup, pvcName string) error {
	return r.Exposers.TeardownExposure(ctx, backup.Namespace,
		moverNamePrefix(backup.Namespace, backup.Name, pvcName), exposureLabels(backup, pvcName))
}

// ensureMoverCredsSecret creates the per-Job Secret the mover consumes: the repository password
// as a mounted file and the two S3 credentials as env (secretKeyRef). It reads the S3 credentials
// from the location's credentials Secret through the uncached reader (I3) and tolerates
// AlreadyExists so a re-reconcile re-adopts. The exposure labels are stamped so the reaper can
// find it. A package function shared by the Backup controller, the maintenance ops and the
// restore engine — one definition of the per-Job credential shape.
//
// credsNamespace is where the SOURCE credentials Secret is read from — the operator namespace on
// the cluster plane, the tenant's own namespace on the namespace plane. The Secret this function
// WRITES always lands in the operator namespace, beside the Job that mounts it; only the read
// side varies. Passing the wrong read namespace fails silently rather than loudly, which is why
// it is an explicit parameter at every call site instead of a default.
func ensureMoverCredsSecret(ctx context.Context, deps repoMaintenanceDeps, name, dek, s3CredsSecret, credsNamespace string, labels map[string]string) error {
	if credsNamespace == "" {
		credsNamespace = deps.OperatorNamespace
	}
	accessKey, err := deps.Secrets.GetValue(ctx, credsNamespace, s3CredsSecret, mover.SecretKeyAWSAccessKeyID)
	if err != nil {
		return fmt.Errorf("read S3 access key from secret %s/%s: %w", credsNamespace, s3CredsSecret, err)
	}
	secretKey, err := deps.Secrets.GetValue(ctx, credsNamespace, s3CredsSecret, mover.SecretKeyAWSSecretAccessKey)
	if err != nil {
		return fmt.Errorf("read S3 secret key from secret %s/%s: %w", credsNamespace, s3CredsSecret, err)
	}
	creds := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: deps.OperatorNamespace, Labels: labels},
		Type:       corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			mover.SecretKeyResticPassword:     []byte(dek),
			mover.SecretKeyAWSAccessKeyID:     accessKey,
			mover.SecretKeyAWSSecretAccessKey: secretKey,
		},
	}
	if err := deps.Create(ctx, creds); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create mover creds secret %s/%s: %w", deps.OperatorNamespace, name, err)
	}
	return nil
}

// readMoverResult finds the mover Job's pod (by the batch job-name label), reads the terminated
// container's termination message and parses it (mover.ParseMoverResult), returning the result
// and the node the pod ran on. A blank message parses to an error — the load-bearing signal that
// the mover was killed before it could report (OOMKilled/SIGKILL) — which the caller turns into a
// volume failure. A package function shared with the restore engine.
//
// When the KUBELET did the killing it also left a reason on the pod, and that reason is returned
// as a moverKilledError instead of the generic parse failure. See podKillReason: with resource
// limits and a cache sizeLimit on every mover (0.6.1), "the pod vanished" became a thing the
// platform's own configuration can cause, and it must not read the same as a segfault.
func readMoverResult(ctx context.Context, c client.Client, operatorNamespace, jobName string) (mover.MoverResult, string, error) {
	var pods corev1.PodList
	if err := c.List(ctx, &pods,
		client.InNamespace(operatorNamespace),
		client.MatchingLabels{batchv1.JobNameLabel: jobName}); err != nil {
		return mover.MoverResult{}, "", fmt.Errorf("list mover pods for job %s: %w", jobName, err)
	}
	// With backoffLimit retries a Complete Job can retain BOTH a failed and a succeeded pod,
	// and list order is arbitrary — prefer the exit-0 attempt's message so a retried-then-
	// successful run is never misread as its own earlier failure; fall back to any
	// terminated pod (all attempts failed, or a hard kill left a blank message).
	var fallback, killed *corev1.Pod
	var killedReason string
	for i := range pods.Items {
		pod := &pods.Items[i]
		// Checked BEFORE the terminated-status filter: an evicted pod may carry no container
		// status at all, and then the loop below would skip it and the whole function would end
		// at "no terminated mover pod found" — the least informative sentence available about a
		// failure the operator caused by capping a volume.
		if reason := podKillReason(pod); reason != "" && killed == nil {
			killed, killedReason = pod, reason
		}
		cs := pod.Status.ContainerStatuses
		if len(cs) == 0 || cs[0].State.Terminated == nil {
			continue
		}
		if cs[0].State.Terminated.ExitCode == 0 {
			result, err := mover.ParseMoverResult(cs[0].State.Terminated.Message)
			return result, pod.Spec.NodeName, err
		}
		if fallback == nil {
			fallback = pod
		}
	}
	// A kubelet kill outranks the blank termination message it left behind: both mean "the mover
	// never reported", but only one of them says why, and the why is actionable.
	if killed != nil {
		return mover.MoverResult{}, killed.Spec.NodeName, &moverKilledError{reason: killedReason}
	}
	if fallback != nil {
		result, err := mover.ParseMoverResult(fallback.Status.ContainerStatuses[0].State.Terminated.Message)
		return result, fallback.Spec.NodeName, err
	}
	return mover.MoverResult{}, "", fmt.Errorf("no terminated mover pod found for job %s/%s", operatorNamespace, jobName)
}

// podReasonEvicted is the pod-level status reason the kubelet's eviction manager writes. The
// message beside it is the only place the CAUSE appears — "Usage of EmptyDir volume
// "restic-cache" exceeds the limit "20Gi"." — so the message is carried through, not just the
// reason.
const podReasonEvicted = "Evicted"

// containerReasonOOMKilled is the terminated-state reason the kubelet writes when the cgroup's
// memory limit killed the process.
const containerReasonOOMKilled = "OOMKilled"

// podKillReason classifies the two ways THIS OPERATOR'S OWN SIZING can end a mover, and returns
// the short reason to record on the CR — or "" when the pod died of something else.
//
// Both were possible before 0.6.1 (a node under memory pressure, an admin-set LimitRange) and both
// arrived at the same place: an empty termination message, reported as "MoverCrashed", a string
// that sends an operator looking for a bug in the mover. Now that the operator itself sets a
// memory limit and a cache cap on every Job, that would be its own configuration handing back a
// misleading diagnosis — which is why the sizeLimit was not shipped without this function.
//
//   - EVICTED: the restic cache emptyDir exceeded its sizeLimit (or the node ran out of disk).
//     The kubelet's own message names the volume and the number, so it is quoted verbatim.
//   - OOMKILLED: the container exceeded its memory limit.
func podKillReason(pod *corev1.Pod) string {
	if pod.Status.Reason == podReasonEvicted {
		message := strings.TrimSpace(pod.Status.Message)
		if message == "" {
			return "MoverEvicted"
		}
		return "MoverEvicted: " + message
	}
	for i := range pod.Status.ContainerStatuses {
		if t := pod.Status.ContainerStatuses[i].State.Terminated; t != nil && t.Reason == containerReasonOOMKilled {
			return "MoverOOMKilled: the mover exceeded its memory limit"
		}
	}
	return ""
}

// killDetail is podKillReason over a whole Job, formatted for appending to an error message: it
// returns ": MoverOOMKilled…" / ": MoverEvicted…" or the empty string. Used by the maintenance path,
// which has no MoverResult to read (its Jobs are watched to completion, not parsed) and would
// otherwise report a kubelet kill as a pod count.
//
// A List error yields "": this is decoration on a failure that is already being reported, and it
// must never turn into a second, unrelated error about listing pods.
func killDetail(ctx context.Context, c client.Client, operatorNamespace, jobName string) string {
	var pods corev1.PodList
	if err := c.List(ctx, &pods,
		client.InNamespace(operatorNamespace),
		client.MatchingLabels{batchv1.JobNameLabel: jobName}); err != nil {
		return ""
	}
	for i := range pods.Items {
		if reason := podKillReason(&pods.Items[i]); reason != "" {
			return ": " + reason
		}
	}
	return ""
}

// moverKilledError is the error readMoverResult returns for a mover the kubelet killed. It carries
// the reason to record verbatim; every existing behaviour keyed on "the mover did not report"
// (notably the stale-lock unlock) still fires, because it is still a non-nil error.
type moverKilledError struct{ reason string }

func (e *moverKilledError) Error() string { return e.reason }

// moverStalled reports whether an in-flight mover Job has failed to START, and returns the
// observable cause to carry into the volume's reason.
//
// The predicate is deliberately narrow and it is the whole safety argument for moverStartDeadline:
//
//  1. the Job has existed longer than moverStartDeadline (its own creationTimestamp — durable,
//     restart-proof, no new state), AND
//  2. not one of its pods has ever demonstrably run (moverPodEverStarted).
//
// Both halves are required. Drop the second and this becomes a wall-clock cap on backups, which
// would fail every large volume in the fleet — a far worse bug than the hang it closes.
//
// Every "I do not know" answer is FALSE, on the same asymmetry snapshotDeadlineExceeded uses: an
// unreadable pod list is not evidence that nothing started, and the true branch terminates
// somebody's backup. A pass that cannot tell waits for the next one, five seconds later.
func (r *BackupReconciler) moverStalled(ctx context.Context, job *batchv1.Job) (string, bool) {
	if job.CreationTimestamp.IsZero() || time.Since(job.CreationTimestamp.Time) <= r.effectiveMoverStartDeadline() {
		return "", false
	}
	var pods corev1.PodList
	if err := r.List(ctx, &pods,
		client.InNamespace(r.OperatorNamespace),
		client.MatchingLabels{batchv1.JobNameLabel: job.Name}); err != nil {
		logf.FromContext(ctx).Error(err, "mover start-deadline check could not list the Job's pods; waiting",
			"job", r.OperatorNamespace+"/"+job.Name)
		return "", false
	}
	if moverPodEverStarted(pods.Items) {
		return "", false
	}
	return r.stalledMoverDetail(ctx, pods.Items, job.Name), true
}

// moverPodEverStarted reports whether ANY pod of a mover Job has demonstrably begun executing its
// container. It is the difference between "stuck" and "slow", and therefore the difference between
// a timeout that closes a 36-hour hang and one that destroys large backups.
//
// A pod counts as started on any of:
//
//   - a pod phase past Pending. Running is the case this exists for; Succeeded and Failed are
//     included because a pod that reached a terminal phase is the Job controller's business — its
//     backoffLimit accounting owns that outcome, and a start-deadline that raced it would report a
//     crash-looping mover as "never started", which is both wrong and less useful.
//   - a container that is running, has terminated, or has a previous termination recorded. This
//     catches the pod whose phase has not been refreshed yet, and the restarting container whose
//     current state is Waiting but whose LastTerminationState proves it ran.
//
// What deliberately does NOT count is pod.status.startTime. The kubelet stamps it when it ACCEPTS
// the pod, before pulling an image or mounting a volume — a pod wedged in ContainerCreating for
// thirty-six hours has one, so reading it as a start signal would make this predicate always true
// and the deadline dead code. The empty slice is likewise NOT started: a Job that is half an hour
// old with no pod at all (quota rejection, an admission webhook refusing it) is stuck in the same
// sense and by the same evidence.
func moverPodEverStarted(pods []corev1.Pod) bool {
	for i := range pods {
		pod := &pods[i]
		switch pod.Status.Phase {
		case corev1.PodRunning, corev1.PodSucceeded, corev1.PodFailed:
			return true
		}
		for j := range pod.Status.ContainerStatuses {
			cs := &pod.Status.ContainerStatuses[j]
			if cs.State.Running != nil || cs.State.Terminated != nil || cs.LastTerminationState.Terminated != nil {
				return true
			}
		}
	}
	return false
}

// stalledMoverDetail finds the observable cause of a mover that never started: the most recent
// Warning Event about its pod, falling back to one about the Job itself when there is no pod (the
// Job controller records FailedCreate there — a quota denial, a refusing admission webhook).
//
// A mover Job runs one pod at a time, so the loop is over a one-element slice in every case anyone
// will ever see; it takes the first pod that has anything to say rather than the newest event
// across all of them, because a mover that produced two pods produced them SEQUENTIALLY and the
// list is small enough that the distinction is not worth a second sort.
func (r *BackupReconciler) stalledMoverDetail(ctx context.Context, pods []corev1.Pod, jobName string) string {
	for i := range pods {
		if msg := r.latestWarningEvent(ctx, r.OperatorNamespace, pods[i].Name); msg != "" {
			return msg
		}
	}
	return r.latestWarningEvent(ctx, r.OperatorNamespace, jobName)
}

// latestWarningEvent returns the most recent Warning Event about one object, rendered as
// "<reason>: <message>" — the two columns `kubectl describe` prints, and the two an operator reads.
//
// THIS IS THE POINT OF THE WHOLE DEADLINE. In the incident that produced it the kubelet published
// the exact cause — an RBD clone the node's krbd could not map — 1069 times over 36 hours, starting
// one minute in. Every deadline in this file could have been in place and the operator would still
// have been left with "timed out", which is a fact about us rather than about their cluster. So the
// cause travels into status.volumes[].reason, the same way podKillReason carries the kubelet's own
// eviction message verbatim.
//
// Read through APIReader — the UNCACHED, straight-to-apiserver reader — and never through the
// controller's cached client. Listing Events through the cache would make controller-runtime start
// an informer over every Event in the cluster, which on a busy cluster is the single largest object
// stream there is; this is a handful of field-selected reads on a path that only executes when a
// volume is already being failed. A nil APIReader (it is set post-construction, like Hooks)
// degrades to no detail rather than to a cache subscription: the reason keeps its bare form and the
// Event still names the pod to describe.
func (r *BackupReconciler) latestWarningEvent(ctx context.Context, namespace, name string) string {
	if r.APIReader == nil || name == "" {
		return ""
	}
	var warnings corev1.EventList
	if err := r.APIReader.List(ctx, &warnings,
		client.InNamespace(namespace),
		client.MatchingFields{"involvedObject.name": name}); err != nil {
		// Decoration on a failure that is already being reported. It must never become a second,
		// unrelated error about listing Events — the same rule killDetail follows.
		logf.FromContext(ctx).V(1).Info("could not read Warning Events for the stalled object; "+
			"the reason will carry no detail", "object", namespace+"/"+name, "error", err.Error())
		return ""
	}
	var newest *corev1.Event
	var newestAt time.Time
	for i := range warnings.Items {
		e := &warnings.Items[i]
		if e.Type != corev1.EventTypeWarning {
			continue
		}
		at := eventObservedAt(e)
		if newest == nil || at.After(newestAt) {
			newest, newestAt = e, at
		}
	}
	if newest == nil {
		return ""
	}
	message := strings.TrimSpace(newest.Message)
	if reason := strings.TrimSpace(newest.Reason); reason != "" && message != "" {
		return reason + ": " + message
	}
	if message != "" {
		return message
	}
	return strings.TrimSpace(newest.Reason)
}

// eventObservedAt is when an Event last happened, tolerating the two APIs that write it. A core/v1
// Event carries lastTimestamp; one written through events.k8s.io/v1 and read back through the core
// view carries eventTime (and, when aggregated, a series). Preferring the latest of the three and
// falling back to creation means an aggregated "x1069 over 36h" event sorts by its LAST occurrence
// rather than its first, which is the one that describes the cluster now.
func eventObservedAt(e *corev1.Event) time.Time {
	at := e.LastTimestamp.Time
	if e.Series != nil && e.Series.LastObservedTime.After(at) {
		at = e.Series.LastObservedTime.Time
	}
	if e.EventTime.After(at) {
		at = e.EventTime.Time
	}
	if at.IsZero() {
		at = e.CreationTimestamp.Time
	}
	return at
}

// deadlineReason composes a VolumeStatus.reason from a deadline's short name and the observable
// cause behind it, capped by shortReason so the status field never carries an unbounded blob. With
// no cause available the bare name is returned unchanged — which is what the crucible and the
// envtest suite assert verbatim for the reasons that predate this lot.
func deadlineReason(reason, detail string) string {
	if detail == "" {
		return reason
	}
	return shortReason(reason + ": " + detail)
}

// eventSuffix renders the observable cause for the tail of an Event message, or "" when there is
// none. The Event is not capped the way the reason is: it is the long form, and truncating the
// kubelet's own sentence is what leaves the reader guessing.
func eventSuffix(detail string) string {
	if detail == "" {
		return ""
	}
	return " — the most recent Warning was: " + detail
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
	deleteJobAndSecret(ctx, r.Client, r.OperatorNamespace, prefix+"-mover")
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

// moverNamePrefix is the deterministic per-PVC NamePrefix "<namespace>-<backup>-<pvc>",
// sanitized to a DNS-1123 name and capped (with a hash suffix on overflow) so the derived Job
// name stays within the 63-char label limit. Deterministic in (namespace, backup, pvc), so every
// reconcile — and a restarted controller — derives identical exposure/Job/Secret names.
//
// The namespace is LOAD-BEARING, not cosmetic: a cluster-DR run fans out one child Backup of the
// SAME name (the run) into every matched namespace, and all per-PVC mover/exposure objects live
// in the single shared operator namespace (plus the cluster-scoped static VSC). Without the
// namespace in the name, two namespaces holding a same-named PVC (the norm: "data", "redis-data")
// in one run would derive colliding names; every Create tolerates AlreadyExists, so the second
// namespace would silently adopt the first's Job/exposure and either record the first's snapshot
// as its own (data loss + false success) or hang once the first tore down. Qualifying by namespace
// keeps every (namespace, run, pvc) object unique. The restic snapshot itself was always correct
// (DataIdentity is namespace-scoped); only the k8s object names lacked the qualifier.
func moverNamePrefix(namespace, backupName, pvcName string) string {
	return sanitizeDNSName(namespace+"-"+backupName+"-"+pvcName, moverNamePrefixMax)
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

// setCompletionTime stamps status.completionTime on a Backup that has just reached a terminal
// phase, IDEMPOTENTLY: an already-stamped object keeps the instant it first finished.
//
// The idempotence is the whole of it. Every other path through this controller re-runs — a
// conflict retry, a re-list, the already-terminal sweep — and a timestamp rewritten on any of
// them would keep sliding forward, which is worse than having none at all: the metric derived
// from it (crystalbackup_backup_last_failure_timestamp_seconds) would report a failure as having just happened
// every time the object was touched, and an alert on a one-hour window would never clear.
func setCompletionTime(backup *cbv1.Backup) {
	if backup.Status.CompletionTime != nil {
		return
	}
	now := metav1.Now()
	backup.Status.CompletionTime = &now
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
	var killed *moverKilledError
	switch {
	// A kubelet kill (eviction on the cache sizeLimit, or an OOM on the memory limit) is reported
	// as what it is. It is still "the mover did not report", but "MoverCrashed" would point an
	// operator at the mover instead of at the limit they set.
	case errors.As(parseErr, &killed):
		return shortReason(killed.reason)
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
