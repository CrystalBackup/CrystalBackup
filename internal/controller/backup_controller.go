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

// backupReasonExposerUnresolvable is the reason for a volume whose exposer cannot be resolved for any
// cause that is neither "this storage cannot snapshot" (ErrUnsupported -> Skipped) nor a refused
// pre-check (ErrPrecheckFailed -> Failed): a StorageClass deleted out from under an unbound PVC that
// still names it, a PersistentVolume that cannot be read, an API server having a bad minute.
//
// It exists because the alternative was a permanent block, and "permanent" is measured: on a real
// cluster one PVC naming a StorageClass that had been deleted held its Backup at Pending for
// THIRTY HOURS. Reconcile advances the FIRST non-terminal volume, so a volume that can never
// become terminal is a head-of-line block; the Backup never went terminal, so its ClusterBackup
// never did, so concurrencyPolicy: Forbid skipped every following nightly. One mis-referenced
// class in one namespace stopped the entire cluster's backups, and the three volumes queued behind
// it were never even looked at — which is exactly why they carried no reason and why neither
// per-phase deadline could fire on them: a deadline needs a phase, and they had never left Pending.
//
// This reason does NOT decide the volume's fate, and that restraint is deliberate — it is the second
// version of this fix. The first classified the error: a NotFound became an immediate Failed on the
// grounds that a missing reference is a configuration fact no retry can change. That is wrong in both
// directions. A StorageClass absent this second can be created the next, and a bound PVC whose
// PersistentVolume reads NotFound may be looking at a cache that has not caught up — the operator has
// been bitten by exactly that before — while conversely an error that looks transient can be
// permanent. Guessing costs a backup somebody needed whenever the guess goes the harsh way.
//
// So a volume carrying this reason stays PENDING and is retried. What changes is that it no longer
// blocks: firstNonTerminalVolume treats it as parked and prefers any volume that has not been tried
// (see volumeIsParked), and pendingResolveDeadline eventually turns it into a Failed so the run can
// reach a terminal phase. Nothing here judges whether the cause is permanent; the clock does, and it
// is the only honest arbiter available.
const backupReasonExposerUnresolvable = "ExposerUnresolvable"

// backupReasonAdvanceRetrying is the VolumeStatus.reason a volume PAST Pending carries when the pass
// could not advance it: the step for its current phase returned an error — reconstructing the
// exposure, reading its readiness, the mover-admission gate, the per-Job creds Secret, creating the
// mover Job, reading it back. It is the Snapshotting/Uploading sibling of
// backupReasonExposerUnresolvable, and it shares that constant's restraint: it decides nothing about
// the volume, it only records what stopped this pass.
//
// It exists because the head-of-line block the release is named for SURVIVED the fix that was
// supposed to close it, one and two phases later, and mutation-testing that fix is what found it. The
// Pending phase was converted to record-and-continue; Snapshotting and Uploading still returned bare
// errors, and Reconcile returns a volume error at step (10) BEFORE writeStatus at step (11). So a
// durable failure in any of those steps — the snapshot CRDs removed under a running operator, RBAC
// narrowed, a webhook refusing Jobs — reproduced the incident in full through a different door: the
// same volume re-driven every pass, every volume behind it in the namespace never attempted, and
// NOTHING the pass computed persisted (the assertion that failed when the already-fixed Expose path
// was mutated back was that `status.volumes` was nil — not that a timestamp was missing, but that
// the enumeration itself never reached etcd).
//
// It is a DIFFERENT token from the parking one, and that is deliberate rather than tidy.
// volumeIsParked requires the Pending phase and matches "ExposerUnresolvable:" exactly, because
// parking means "defer this volume behind every volume that has not been tried". A volume in
// Snapshotting or Uploading has an exposure, possibly a snapshot the storage system is already
// holding, possibly a mover reading it — work in flight that must keep its turn. What such a volume
// needs is not a place in the queue, it is a BOUND, and it already has one on a clock that survives a
// crash because it hangs off the origin VolumeSnapshot rather than off status
// (snapshotProgressDeadline / snapshotReadyDeadline / moverStartDeadline). This lot's job was to make
// those bounds REACHABLE on the paths that error, not to give the volume a new field.
const backupReasonAdvanceRetrying = "AdvanceRetrying"

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

// backupReasonPendingDeadline is the VolumeStatus.reason a volume carries when it never left Pending
// within pendingResolveDeadline and nothing more specific was ever recorded about why. It is the
// fallback, not the usual outcome: a resolution that fails writes its own cause (see
// backupReasonExposerUnresolvable), and that cause is kept in preference to this string because it
// names the problem instead of naming the clock.
const backupReasonPendingDeadline = "PendingDeadlineExceeded"

// pendingResolveDeadline bounds how long a volume may sit in Pending — the phase BEFORE anything has
// been created for it, where the only work is resolving which exposer serves its PVC.
//
// This is the fourth deadline, and it closes the hole the other three left. They each hang off an
// object with its own creation time — the origin VolumeSnapshot, the mover Job — so a volume that
// never got as far as creating anything was bounded by nothing at all. On a real cluster that hole
// cost thirty hours: one PVC named a StorageClass that did not exist, resolution returned an error
// every pass, the volume never left Pending, and because volumes advance one at a time it held five
// others behind it. The Backup stayed non-terminal, so the schedule's Forbid policy skipped every
// night that followed, and thirty-one hours of a nightly schedule produced nothing — over a defect
// affecting ONE volume of thirty-three namespaces.
//
// ONE HOUR, and the reasoning is the mirror image of moverStartDeadline's. Nothing has been created,
// so giving up costs nothing: no bytes uploaded, no snapshot consumed, no repository lock held.
// Meanwhile the legitimate reasons resolution is slow are all short — an API server briefly
// unreachable, a cache that has not caught up, a VolumeSnapshotClass being installed as the cluster
// comes up. An hour covers every one of them several times over while still failing inside the first
// night rather than on the second morning.
//
// It is deliberately LONGER than it strictly needs to be for a reason worth stating: a volume failed
// here is a volume nobody backed up, and the fix for a premature failure is a phone call at 3am,
// whereas the fix for a late one is a slightly longer report. When in doubt this number goes up.
//
// READ THIS AS A LOWER BOUND, NOT AN UPPER ONE. The clock only advances when the volume is actually
// picked, and firstNonTerminalVolume defers a parked volume behind every volume that has not been
// tried — so the real time to failure is this hour PLUS however long the others take to go terminal,
// which for a volume of their own can be snapshotReadyDeadline's two hours each. That is the correct
// ordering (a broken volume must never be served before a healthy one) and it means the guarantee
// here is "eventually, and not before an hour of trying", not "within an hour of enumeration".
// Anything that needs a true upper bound on a whole run belongs on the run, not on one volume.
const pendingResolveDeadline = time.Hour

// backupReasonAdvanceDeadline is the VolumeStatus.reason a volume PAST Pending carries when it spent
// advanceRetryDeadline in one phase without a single pass managing to advance it AND without any of
// the per-object bounds being able to judge it. It is the terminal sibling of
// backupReasonAdvanceRetrying — that reason is what the volume wears while the retrying goes on, this
// one is what ends it — and it carries the last observed cause after a colon, exactly as the other
// deadline reasons carry the kubelet's or the storage system's own words.
//
// It is the LAST resort and its name says so. A volume that can be judged on its own merits is judged
// on them: SnapshotProgressDeadlineExceeded means nothing ever picked its snapshot up,
// SnapshotReadyDeadlineExceeded means something did and never finished, MoverStartDeadlineExceeded
// means its pod never ran. Each of those sends an operator to a specific component. This one can only
// say "no pass could get anywhere with this volume and we could not read the object that would have
// told us why", which is a genuinely weaker diagnosis — hence the much longer bound, and hence the
// cause being appended verbatim, because the cause is all the diagnosis there is.
const backupReasonAdvanceDeadline = "AdvanceDeadlineExceeded"

// advanceRetryDeadline is the OUTER bound on a volume past Pending: how long it may stay in one phase
// while every pass fails to advance it.
//
// IT IS A BACKSTOP, NOT A DEADLINE, and reading it as the latter is how it would become dangerous.
// The three per-phase bounds above hang off the object being waited on — the origin VolumeSnapshot's
// creationTimestamp, the mover Job's — on purpose, because that object carries evidence a wall clock
// cannot: whether anything ever ACKNOWLEDGED the snapshot request, whether a pod ever reached
// Running. Those bounds are strictly better than this one and they must always win. This exists only
// for the case they cannot reach at all:
//
//   - an exposure that cannot be RECONSTRUCTED, so there is no exposer and no derived origin
//     snapshot name to read a clock through (the snapshot CRDs removed under a running operator, a
//     ResourceQuota that will not re-admit the temp clone, RBAC narrowed);
//   - a mover Job that cannot be read, or that is ABSENT — no Job, no creationTimestamp, and
//     advanceUploading's own note recorded that this branch was bounded by nothing and that the only
//     honest fix was a phase-entry timestamp on VolumeStatus;
//   - an origin VolumeSnapshot whose progress is unreadable, so snapshotDeadlineExceeded correctly
//     declines to draw any conclusion and the volume waits on a verdict that can never come.
//
// Each of those persists its cause since the previous lot, so nothing is invisible any more — but a
// Snapshotting or Uploading volume is NOT deferred by firstNonTerminalVolume (correctly: it
// legitimately holds work in flight), so it keeps the head of its namespace's queue with no clock
// that can ever end it. That is the thirty-hour incident with the phase changed, and this is the only
// clock left once the per-object ones are unreachable.
//
// FOUR HOURS, sized against the ladder it sits on top of — pendingResolveDeadline 1h,
// moverStartDeadline 30m, snapshotReadyDeadline 2h — by one rule: a backstop must never pre-empt a
// bound that can give a better answer. snapshotReadyDeadline is the longest of them at two hours and
// its clock starts within seconds of the phase being entered (Expose creates the origin snapshot on
// the pass that sets Snapshotting), so four hours gives the specific verdict a clear factor of two to
// arrive in. Below that margin the two would race and the weaker diagnosis would sometimes win, which
// is the one outcome that would make this constant a regression rather than a fix.
//
// It must also stay inside the window a nightly run is judged in, for the reason every bound here
// gives: the failure has to be reported by THAT run, hours before the next one, instead of rolling
// into the next night — which is what the incident did, thirty-one hours of it.
//
// WHAT IT MUST NEVER TOUCH, and the anti-regression that protects it: a mover pod that is RUNNING is
// not bounded by anything, by design (see moverStartDeadline — a multi-terabyte volume legitimately
// uploads for hours, and a wall-clock cap dressed up as a stall detector destroys the largest, most
// valuable backups in the fleet first). Safety here comes from the PREDICATE, not the number: this
// clock is consulted ONLY on a pass that could not advance the volume, never on the "still running;
// requeue" and "not ready yet" answers that a healthy slow backup produces. Read the call sites
// before changing the constant.
//
// Err long, always, like every other bound in this file. Four hours late is a slower report; four
// hours early is a backup somebody needed.
const advanceRetryDeadline = 4 * time.Hour

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
	// PendingResolveDeadline overrides pendingResolveDeadline, on exactly the terms above: zero in
	// production, set only by tests, plumbed to no flag or chart value.
	//
	// This one is testable in envtest for a reason the mover deadline is not — its clock is
	// status.volumes[].firstAttemptAt, a field this controller writes rather than one the apiserver
	// owns, so a spec CAN backdate it. The override still exists because backdating a status field is
	// a test reaching inside the mechanism it is checking: a spec that sets this instead observes the
	// controller stamping its own clock and then honouring it, which is the behaviour that matters.
	PendingResolveDeadline time.Duration
	// AdvanceRetryDeadline overrides advanceRetryDeadline, on the same terms as the two above: zero in
	// production, set only by tests, plumbed to no flag and no chart value.
	//
	// The reason it is not a knob is sharper here than for the others. This bound's safety comes
	// entirely from WHERE it is consulted (only on a pass that could not advance the volume) and not
	// from its value, so an administrator who lowered it because a run took too long would not get a
	// faster diagnosis — they would get healthy volumes failed on whichever bad minute the cluster was
	// having. Its clock is status.volumes[].phaseEnteredAt, which this controller writes, so a spec
	// could backdate it instead; shortening the bound is still the better instrument, because it makes
	// the controller stamp its own clock and then honour it.
	AdvanceRetryDeadline time.Duration
}

// effectiveMoverStartDeadline is the bound moverStalled compares against: the injected one when a
// test set it, the constant otherwise.
func (r *BackupReconciler) effectiveMoverStartDeadline() time.Duration {
	if r.MoverStartDeadline > 0 {
		return r.MoverStartDeadline
	}
	return moverStartDeadline
}

// effectivePendingResolveDeadline is the bound advancePending compares firstAttemptAt against: the
// injected one when a test set it, the constant otherwise.
func (r *BackupReconciler) effectivePendingResolveDeadline() time.Duration {
	if r.PendingResolveDeadline > 0 {
		return r.PendingResolveDeadline
	}
	return pendingResolveDeadline
}

// effectiveAdvanceRetryDeadline is the outer bound failVolumeOnAdvanceBackstop compares
// status.volumes[].phaseEnteredAt against: the injected one when a test set it, the constant
// otherwise.
func (r *BackupReconciler) effectiveAdvanceRetryDeadline() time.Duration {
	if r.AdvanceRetryDeadline > 0 {
		return r.AdvanceRetryDeadline
	}
	return advanceRetryDeadline
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
	//
	// A FAILURE TO ADVANCE ONE VOLUME NEVER ABANDONS THE PASS. This is the structural half of the
	// head-of-line fix, and it is written here — at the one call site of the whole per-PVC state
	// machine — rather than at each step that can fail, because the per-call-site shape IS the defect:
	// three separate lots have now found the same bug in this same function family (resolution, then
	// Expose + the pre-check's transient branch + the unreadable-PVC read, then the whole of
	// Snapshotting and Uploading), and each time the fix covered the doors that were known about and
	// left the next one open. Anything advanceVolume grows in future inherits this by construction.
	//
	// What returning the error cost, precisely: this line is upstream of writeStatus at step (11), so
	// an errored pass persists NOTHING it computed — not the volume's phase, not the recorded cause,
	// not even the enumeration (mutating the already-fixed Expose path back to a bare return is
	// caught by an assertion that `status.volumes` is NIL). The retry that controller-runtime then
	// performs re-picks the SAME volume, because firstNonTerminalVolume reads the stored status and
	// the stored status never changed — so the namespace's other volumes are not merely delayed, they
	// are unreachable, and every per-phase deadline meant to end the hang is unreachable with them.
	//
	// Shape chosen: record the cause on the volume, continue the pass, return nil. Two alternatives
	// were considered and rejected.
	//
	//   - Record, continue, then PROPAGATE the error after the status write so controller-runtime
	//     still backs off. Rejected because the backoff is charged to the wrong object. It is the
	//     BACKUP that is requeued, not the volume, so one volume erroring durably would stretch the
	//     poll every OTHER volume of that namespace is driven by from backupPollInterval's 5s to the
	//     rate limiter's ceiling — a head-of-line block in the time domain instead of the queue
	//     domain, which is the same defect wearing a different hat.
	//   - Convert every failing step inside advanceVolume the way advancePending was converted.
	//     Rejected as the shape that has already failed three times, for the reason given above.
	//
	// Retrying is not lost: a non-terminal Backup requeues on backupPollInterval, exactly as a parked
	// Pending volume does (see parkVolume, which made the same trade for the same reason).
	//
	// CONTINUING INTO STEPS (10b) AND (10c) IS SAFE, and the second one needed a real answer rather
	// than a convention:
	//
	//   - (10b) advanceManifests is documented as deliberately independent of the volume half — the
	//     two halves fail for unrelated reasons and coupling them loses one to the other's bad day —
	//     so a volume that could not be advanced must not withhold a namespace's manifests either.
	//   - (10c) closeFreezeWindow fires the post hooks, and a pass where a volume's snapshot could not
	//     be cut is emphatically NOT a pass where a database should be thawed. It does not become one:
	//     the release is already gated on snapshotsCut, which requires EVERY volume to have LEFT
	//     Pending/Snapshotting, and "could not advance" means precisely that no phase transition
	//     happened — the volume is still in one of those two phases, so the window stays open by the
	//     guard that is already there. The invariant is "no volume is still in the snapshot phase",
	//     never "this pass had no error", and the difference is not academic: a volume that goes
	//     Skipped or Failed on the same pass DOES close the window today, correctly, because R16's
	//     release is unconditional (01-architecture.md §5 — a failed snapshot leaves the application
	//     just as quiesced, so gating the thaw on success strands exactly the workload whose backup
	//     went wrong). Adding a "not on an errored pass" gate would invert that guarantee: for a
	//     DURABLE error the window would then never close at all, and the workload would stay frozen
	//     for as long as the cluster stayed broken. Which is also why the deadline reachability below
	//     is load-bearing twice over — it is the only thing that ever ends such a volume's
	//     Snapshotting, and therefore the only thing that ever releases the freeze window.
	//
	// STEPS (10b) AND (10c) NO LONGER DISCARD THIS PASS EITHER, and they close it the OTHER way — the
	// way that was rejected for volumes. See their own note below the call sites.
	teardownPVC := ""
	if idx := firstNonTerminalVolume(backup.Status.Volumes); idx >= 0 {
		vol := &backup.Status.Volumes[idx]
		tp, err := r.advanceVolume(ctx, &backup, vol, rc)
		if err != nil {
			// No teardown: a step that errored did not make this volume terminal, so there is
			// nothing whose result has just been persisted and whose objects may now be collected.
			r.recordAdvanceFailure(ctx, vol, err)
		} else {
			teardownPVC = tp
		}
	}

	// (10b) The manifest half, driven independently of the volumes. A PVC the CSI driver cannot
	// snapshot is reported Skipped and the namespace still gets its manifests (02-api.md): the
	// two halves fail for unrelated reasons, and coupling them would lose one to the other's bad
	// day.
	//
	// (10c) The freeze window CLOSES here, on the snapshots being cut.
	//
	// NEITHER ERROR LEAVES THIS FUNCTION HERE. Both used to, and both sat between step (10) and the
	// status write, so a manifest half that could not start its Job — or a release phase that could
	// not list pods — threw away everything the volume half had just computed on the same pass. That
	// is exactly the coupling (10b)'s own doc says must not exist, stated there about the volume half
	// and violated in the other direction.
	//
	// PERSIST FIRST, THEN PROPAGATE — the shape that was REJECTED for a volume error at step (10) and
	// is the right one here, for one reason: the failure is not per-volume. The objection at step (10)
	// was that controller-runtime charges the backoff to the BACKUP, so one bad volume would stretch
	// the poll driving every OTHER volume of that namespace from backupPollInterval's 5s to the rate
	// limiter's ceiling — the same head-of-line block moved into the time domain. A manifest Job that
	// cannot be created and a release that cannot be resolved are properties of the run as a whole, so
	// the object being backed off IS the object at fault. The retry is charged correctly, and
	// reconcile_errors_total keeps a failure that deserves to be counted.
	//
	// The deferred errors are returned after the status write, the teardowns and the retention
	// enqueue, at the bottom of this function. Order matters there and is argued at that line.
	//
	// closeFreezeWindow also reports whether a release is still OWED, which writeStatus needs in order
	// not to let the run reach a terminal phase over an application that may still be quiesced. It is
	// threaded through as a value rather than re-derived from status because status cannot tell "a
	// retry is owed" from "no post hooks were ever declared" — see closeFreezeWindow.
	manifestsDone, teardownManifests, manifestsErr := r.advanceManifests(ctx, &backup, rc,
		includeManifests(run), run.ManifestOptions.ExcludeSecretData)
	releaseOwed, releaseErr := r.closeFreezeWindow(ctx, &backup, run.Hooks)

	// (11) Single status writer: roll the per-volume phases up, record a terminal condition +
	// backupTime once, and write status exactly once.
	res, err := r.writeStatus(ctx, &backup, rc, manifestsDone, releaseOwed)
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

	// The deferred propagation of steps (10b) and (10c), and it is LAST on purpose.
	//
	// Everything above it is work whose result is already decided and must not be skipped by an
	// unrelated half's bad day: the status write that makes this pass durable, the teardown of a
	// volume whose terminal result has just been persisted, the teardown of a finished manifest
	// capture's grant, and the retention forget a successful run owes its repository. Returning the
	// error before any of those would have reproduced the very defect being closed — a pass that
	// computed a result and then dropped it — one step further down the function.
	//
	// ctrl.Result is deliberately zeroed rather than carried: controller-runtime ignores a result
	// returned alongside an error (and logs a warning about it), so returning res here would only
	// misstate the intent. The error IS the requeue: with backoff, which is the point of propagating
	// at all.
	//
	// A FAILED STATUS WRITE SUPERSEDES THESE, by returning above, and that is right rather than a
	// leak: the write error already requeues this pass, and a pass that could not persist anything
	// will meet the same manifest or release failure again next time. Reporting both would only mean
	// choosing which one controller-runtime logs.
	if deferred := errors.Join(manifestsErr, releaseErr); deferred != nil {
		return ctrl.Result{}, deferred
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
//
// manifestsDone and releaseOwed are the two things a volume roll-up cannot see, and both HOLD the
// terminal phase back for the same structural reason — see below.
func (r *BackupReconciler) writeStatus(ctx context.Context, backup *cbv1.Backup, rc *backupRunContext,
	manifestsDone, releaseOwed bool,
) (ctrl.Result, error) {
	phase := string(status.RollUpVolumePhases(backup.Status.Volumes))
	// A Backup is not finished while its manifest half is still running, even when every volume
	// is. Letting the roll-up go terminal here would trip the already-terminal short-circuit at
	// the top of Reconcile, and the capture in flight would never have its result recorded —
	// leaving a snapshot in the repository that no Backup object points at, which is exactly the
	// kind of silent loss the discovery model cannot repair (the run tag would be orphaned).
	//
	// A Backup IS ALSO NOT FINISHED WHILE A FREEZE WINDOW IS STILL OPEN, and it is the same argument
	// with a heavier consequence: what does not get recorded there is not a snapshot id, it is the
	// UNFREEZE of somebody's database. The release fires on snapshotsCut, so on a run whose volumes
	// all went Skipped or Failed it fires on the very pass the roll-up becomes terminal; a failed
	// attempt on that pass is owed up to postHookMaxAttempts more, and the short-circuit at the top of
	// Reconcile guaranteed none of them ever happened. See closeFreezeWindow for what "owed" means,
	// why it cannot be derived from status (a run with no post hooks would be held forever), and why
	// the hold is bounded by the attempt budget rather than by a clock.
	//
	// Uploading is the phase used for both, and for the manifest case it is literally true: bytes are
	// still going to the repository. For the release case it is a small imprecision accepted
	// deliberately. SnapshottingHooks was the accurate-sounding alternative and was rejected twice
	// over: it would move a run whose volumes had already reached Uploading BACKWARDS through the
	// phase enum, and it is the phase the pre-snapshot quiesce owns — reusing it would make "we are
	// about to snapshot" and "we are trying to unfreeze" indistinguishable in every dashboard, alert
	// rule and `kubectl get backup` that reads the phase. Uploading already means "in flight, not
	// finished" to all of them, which is the fact that has to travel.
	if isTerminalBackupPhase(phase) && (!manifestsDone || releaseOwed) {
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
		return "", reasonKEKUnavailable, fmt.Sprintf("read cluster KEK secret %s/%s: %v", operatorNamespace, kekName, err), false
	}
	wrapper, err := keys.NewAgeWrapper(string(identity))
	if err != nil {
		return "", reasonKEKInvalid, fmt.Sprintf("parse cluster KEK secret %s/%s: %v", operatorNamespace, kekName, err), false
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
//
// It also owns two small pieces of status hygiene that only it is in a position to do, both keyed on
// the phase the volume ENTERED the step with: a volume that MOVED has left whatever was blocking it
// behind, so the recorded obstacle is dropped (clearStaleAdvanceReason), and the phase-entry clock is
// stamped (stampVolumePhaseEntry). Both live here rather than in the steps because here is the one
// place that can see the before and the after.
func (r *BackupReconciler) advanceVolume(ctx context.Context, backup *cbv1.Backup, vol *cbv1.VolumeStatus, rc *backupRunContext) (string, error) {
	entryPhase := vol.Phase
	teardownPVC, err := r.advanceVolumePhase(ctx, backup, vol, rc)
	// Unconditional, and BEFORE the error check: a phase change must be stamped whatever else the pass
	// concluded. No step today changes the phase and errors in the same breath (recordAdvanceFailure's
	// doc explains why that separation is load-bearing), but a clock that depended on that staying true
	// would be a clock that silently stops the day it does not.
	stampVolumePhaseEntry(vol, entryPhase)
	if err == nil {
		clearStaleAdvanceReason(vol, entryPhase)
	}
	return teardownPVC, err
}

// stampVolumePhaseEntry is the SINGLE writer of status.volumes[].phaseEnteredAt — the clock
// advanceRetryDeadline is measured against.
//
// One writer, at the one call site of the whole per-PVC state machine, is the point rather than a
// tidy-up. The alternative was a stamp inside each phase function, which is the per-call-site shape
// that has now produced this release's defect three times over: a phase added later would simply not
// stamp, its volume would carry no clock, and the outer bound would be silently unreachable for
// exactly the newest and least-exercised path. Written this way a new phase inherits the clock by
// construction, and forgetting it is not something a reflex can do — it would take editing this
// function.
//
// Two rules, and the second is a migration:
//
//   - EVERY transition is stamped, terminal ones included. Not because a terminal volume needs a
//     clock, but because "stamp on any change of phase" is a rule with no exceptions to get wrong,
//     whereas "stamp on the phases that are bounded" is a list that would go stale the moment the set
//     of bounded phases changed.
//   - A volume that did NOT move is stamped only when it has no clock at all AND is in a phase the
//     backstop bounds. That is the upgrade case: a volume already in Snapshotting or Uploading when
//     the operator started writing this field has no transition left to observe, so it would be
//     permanently unbounded. Stamping it on the first pass that touches it gives the bound a clock
//     starting now, which can only make the backstop LATE — never early, which is the direction that
//     costs a backup. It is deliberately not applied to Pending: that phase is bounded by
//     firstAttemptAt, whose doc records at length why a clock started at enumeration is the wrong one,
//     and giving a Pending volume a second, differently-meaning timestamp would re-open exactly that.
func stampVolumePhaseEntry(vol *cbv1.VolumeStatus, entryPhase status.VolumePhase) {
	if vol.Phase == entryPhase {
		if vol.PhaseEnteredAt != nil || !advanceBackstopApplies(vol.Phase) {
			return
		}
	}
	now := metav1.NewTime(time.Now())
	vol.PhaseEnteredAt = &now
}

// advanceBackstopApplies reports whether a phase is one the outer bound covers: the two that hold work
// in flight and are therefore NOT deferred by firstNonTerminalVolume, so a volume stuck in one of them
// keeps the head of its namespace's queue.
//
// Pending is excluded because it already has a better bound (pendingResolveDeadline, on a clock that
// measures time spent TRYING rather than wall time) and because a parked Pending volume is deferred
// behind its un-tried neighbours, so it starves nobody while it waits. The terminal phases are
// excluded because there is nothing left to bound.
func advanceBackstopApplies(phase status.VolumePhase) bool {
	switch phase {
	case status.VolumePhaseSnapshotting, status.VolumePhaseUploading:
		return true
	default:
		return false
	}
}

// failVolumeOnAdvanceBackstop is the outer bound's verdict, and it is called ONLY from a point where
// this pass has established that it cannot advance the volume AND cannot reach a bound that would
// judge it better. It returns the PVC name (to request teardown, exactly as the per-phase deadlines
// do) and true when it took the verdict; ("", false) means the caller should carry on with whatever it
// was going to do — record the cause and retry.
//
// READ THE CALL SITES, NOT THIS FUNCTION, TO JUDGE THE SAFETY. Every bound in this controller is safe
// because of its predicate rather than its number, and this one's predicate is entirely in its
// callers: an exposure that could not be reconstructed, a mover-admission gate that errored, a creds
// Secret that could not be written, a mover Job that could not be created, read, or found at all, an
// exposure whose readiness is unknown and whose origin snapshot cannot be judged. It is NOT called on
// "not ready yet", not on "blocked waiting for a mover slot", and above all not on "the Job exists and
// its pod is running": a mover that got off the ground has as long as it likes, and breaking that
// turns this backstop into a wall-clock cap on the largest volumes in the fleet.
//
// No clock, no verdict — the same asymmetry snapshotDeadlineExceeded is built on. An absent
// phaseEnteredAt is not evidence that a volume has been stuck a long time, and this branch terminates
// somebody's backup, so the burden of proof sits on the evidence and never on its absence.
func (r *BackupReconciler) failVolumeOnAdvanceBackstop(ctx context.Context, backup *cbv1.Backup,
	vol *cbv1.VolumeStatus, cause error,
) (string, bool) {
	if vol.PhaseEnteredAt == nil {
		return "", false
	}
	bound := r.effectiveAdvanceRetryDeadline()
	if time.Since(vol.PhaseEnteredAt.Time) <= bound {
		return "", false
	}
	stuckIn := string(vol.Phase)
	vol.Phase = status.VolumePhaseFailed
	// The cause travels into the reason, capped, for the same reason the mover-start deadline carries
	// the kubelet's own sentence: a reason that names only our decision leaves the reader as blind as
	// the hang did. Here it matters more than anywhere else, because this deadline's own diagnosis is
	// the weakest of the four — the cause is the only actionable thing it has.
	vol.Reason = deadlineReason(backupReasonAdvanceDeadline, cause.Error())
	r.Recorder.Eventf(backup, nil, corev1.EventTypeWarning, "VolumeFailed", "AdvanceBackstop",
		"backup of PVC %s gave up after %s in %s: every pass since then failed to advance it and none "+
			"of the per-phase deadlines could judge it, because the object they measure — the origin "+
			"VolumeSnapshot or the mover Job — could not be read at all. The volume is failed so the rest "+
			"of this namespace can proceed. The last cause was: %v",
		vol.Pvc, bound, stuckIn, cause)
	// The per-pass log line the two retry paths already write (parkVolume, recordAdvanceFailure), at the
	// point where the retrying finally stops. It records the phase the volume was stuck in, which the
	// status no longer will once the phase is Failed — so the one fact this verdict destroys is the one
	// fact an operator reconstructing the sequence needs.
	logf.FromContext(ctx).Info("volume bounded by the outer advance deadline; its namespace's queue is released",
		"pvc", vol.Pvc, "phase", stuckIn, "bound", bound.String(), "err", cause.Error())
	return vol.Pvc, true
}

// advanceVolumePhase is advanceVolume's dispatch, split out so the wrapper can see the phase the
// volume entered the step with.
func (r *BackupReconciler) advanceVolumePhase(ctx context.Context, backup *cbv1.Backup, vol *cbv1.VolumeStatus, rc *backupRunContext) (string, error) {
	switch vol.Phase {
	case status.VolumePhasePending, "":
		// NO ERROR RESULT, and that absence is the invariant rather than a tidy-up. Every one of
		// advancePending's outcomes is now a recorded verdict or a parked retry, so there is no error
		// left for it to return — and a phase function that STRUCTURALLY cannot fail the pass documents
		// "one volume may not stop the others" better than any comment could, because the next person
		// cannot reintroduce the bug by writing `return err`: they would have to change the signature
		// and this line with it, which is a decision rather than a reflex. `unparam` is what pointed at
		// it (result 0 is always nil), and it was right.
		r.advancePending(ctx, backup, vol)
		return "", nil
	case status.VolumePhaseSnapshotting:
		return r.advanceSnapshotting(ctx, backup, vol, rc)
	case status.VolumePhaseUploading:
		return r.advanceUploading(ctx, backup, vol, rc)
	default:
		return "", nil
	}
}

// clearStaleAdvanceReason drops a recorded obstacle from a volume that has just CHANGED PHASE,
// unless the new phase is one whose reason IS the verdict (Failed, Skipped).
//
// Both writers of a reason on a non-terminal volume — parkVolume and recordAdvanceFailure — describe
// something that is in the way RIGHT NOW: "ExposerUnresolvable: storageclasses… not found",
// "AdvanceRetrying: check exposure readiness…". Once the volume moves, that sentence has stopped
// being true, and leaving it behind is not a cosmetic wart: a volume reported as Uploading while
// carrying "ExposerUnresolvable" tells an operator (and the alert rules, and `kubectl get backup`)
// that the operator cannot resolve an exposer it demonstrably just used. The parked-then-recovered
// case is entirely realistic — a StorageClass created a minute after the run started, a quota
// raised — and it was already possible before this lot; adding a second reason-writing path made it
// worth closing rather than living with.
//
// Failed and Skipped are excluded because there the reason is the whole point (CSISnapshotUnsupported,
// SnapshotPrecheckFailed, the deadline reasons, a mover's own failure) and every one of them is
// written by the same step that set the phase. Completed is NOT excluded: a completed volume has
// nothing left to explain, and a stale obstacle on a successful backup is the most misleading place
// of all to leave one.
func clearStaleAdvanceReason(vol *cbv1.VolumeStatus, entryPhase status.VolumePhase) {
	if vol.Phase == entryPhase || vol.Reason == "" {
		return
	}
	switch vol.Phase {
	case status.VolumePhaseFailed, status.VolumePhaseSkipped:
		return
	}
	vol.Reason = ""
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
//
// IT RETURNS NOTHING, and that is the strongest statement this file makes about the head-of-line
// incident. Every outcome it can reach is either a verdict recorded on the volume (Skipped, Failed) or
// a parked retry (parkVolume) — so there is no error to return, and no way for a Pending volume to
// cost its namespace a status write or its neighbours their turn. The signature is the invariant: see
// advanceVolume's Pending case for why it is better than the comment that used to stand in for it.
func (r *BackupReconciler) advancePending(ctx context.Context, backup *cbv1.Backup, vol *cbv1.VolumeStatus) {
	// The Pending clock starts on the first ATTEMPT, so it measures time spent trying rather than
	// time spent queued behind other volumes.
	if vol.FirstAttemptAt == nil {
		now := metav1.NewTime(time.Now())
		vol.FirstAttemptAt = &now
	} else if elapsed := time.Since(vol.FirstAttemptAt.Time); elapsed > r.effectivePendingResolveDeadline() {
		// Out of patience, and that is a feature. Everything below this line can fail in a way that
		// looks transient forever; this is the one place that guarantees a volume eventually stops
		// being the reason its namespace has no backup. The recorded reason is preserved when there
		// is one, because it names the actual cause and is far more useful than the deadline itself.
		vol.Phase = status.VolumePhaseFailed
		if vol.Reason == "" {
			vol.Reason = backupReasonPendingDeadline
		}
		r.Recorder.Eventf(backup, nil, corev1.EventTypeWarning, "VolumeFailed", "ExposeVolume",
			"PVC %s never left Pending within %s and is failed so the rest of this namespace can proceed: %s",
			vol.Pvc, r.effectivePendingResolveDeadline(), vol.Reason)
		return
	}

	var pvc corev1.PersistentVolumeClaim
	if err := r.Get(ctx, client.ObjectKey{Namespace: backup.Namespace, Name: vol.Pvc}, &pvc); err != nil {
		if apierrors.IsNotFound(err) {
			vol.Phase = status.VolumePhaseFailed
			vol.Reason = "SourcePVCMissing"
			return
		}
		// A PVC that is there but unreadable — RBAC changed under a running operator, the apiserver
		// having a bad minute. Parked, not errored: see parkVolume. This one is the least likely of the
		// four to be durable and the most likely to be argued for as a plain requeue, which is exactly
		// why it goes through the same door. The guarantee "one volume cannot stop the others" is worth
		// nothing if it holds on three paths out of four.
		r.parkVolume(ctx, vol, "get source PVC", err)
		return
	}

	ex, err := r.Exposers.For(ctx, &pvc)
	if err != nil {
		if errors.Is(err, exposer.ErrUnsupported) {
			vol.Phase = status.VolumePhaseSkipped
			vol.Reason = backupReasonSkippedUnsupported
			r.Recorder.Eventf(backup, nil, corev1.EventTypeNormal, "VolumeSkipped", "SkipVolume",
				"PVC %s is on storage without CSI snapshot support; skipped", vol.Pvc)
			return
		}
		// Resolution failed for a reason that is neither "this storage cannot snapshot" nor a refused
		// pre-check. Deliberately NOT classified by error kind here: a missing StorageClass looks
		// permanent and can be created a minute later, an unreadable PersistentVolume looks transient
		// and can be gone for good, and a cached client answers NotFound for objects that exist. Every
		// such guess is wrong in one direction, and the direction that hurts is failing a volume whose
		// data was fine.
		//
		// So this does not decide. It records the cause on the volume, leaves it Pending, and returns
		// WITHOUT an error to return — which is the whole fix. Returning an error re-drove this same
		// volume forever and held every volume queued behind it in its namespace; the run stayed
		// non-terminal, its schedule's Forbid policy skipped every subsequent night, and a cluster
		// went thirty hours with no backups because one PVC named a StorageClass that did not exist.
		// The bound on how long this may repeat is pendingResolveDeadline, applied above.
		r.parkVolume(ctx, vol, "resolve exposer", err)
		return
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
			// being added. Parked rather than errored, for the reason parkVolume gives.
			r.parkVolume(ctx, vol, "pre-check exposure", err)
			return
		}
		vol.Phase = status.VolumePhaseFailed
		vol.Reason = backupReasonPrecheckFailed
		// Warning, not Normal, and carrying the failing check's own detail verbatim: the reason
		// string says which gate refused, the Event says what is actually missing (the Secret's
		// namespace/name and the class that asked for it), which is the difference between an
		// alert an operator can act on and one they have to go investigate.
		r.Recorder.Eventf(backup, nil, corev1.EventTypeWarning, "VolumeFailed", "SnapshotPrecheck",
			"backup of PVC %s cannot start: %v", vol.Pvc, err)
		return
	}

	if _, err := ex.Expose(ctx, r.exposeRequest(backup, &pvc)); err != nil {
		// The path that made this whole helper necessary. Expose can fail durably for causes that have
		// nothing to do with the volume's snapshot capability and everything to do with the cluster
		// today: a ResourceQuota exhausted so the temp clone PVC cannot be created, a validating
		// webhook refusing VolumeSnapshot objects, the snapshot CRDs absent. Each of those errored
		// every pass, so the volume was re-picked forever and its neighbours never advanced — the
		// thirty-hour incident exactly, reached by a different door.
		r.parkVolume(ctx, vol, "expose", err)
		return
	}
	vol.Phase = status.VolumePhaseSnapshotting
}

// parkVolume records why a volume could not be started, leaves it PENDING, and expects its caller to
// return nil. It is the single shape every non-verdict failure in advancePending takes.
//
// Returning an error instead is what the incident was made of, and the damage is in two parts that
// have to be fixed together. The obvious part: Reconcile advances one volume per pass, so an error
// re-drives this same volume forever and the volumes queued behind it in the namespace are never
// attempted. The part that is easy to miss, and that made the first version of this fix incomplete:
// Reconcile returns the error BEFORE writeStatus, so nothing this function wrote is ever persisted —
// including firstAttemptAt. The Pending deadline's clock never reaches etcd, and the deadline that
// exists precisely to end this can never fire. An error here does not merely delay the queue; it
// disables the mechanism meant to rescue it.
//
// Retrying is not lost. A non-terminal Backup requeues on backupPollInterval, so a parked volume is
// re-attempted on a timer. What is lost is controller-runtime's exponential backoff, and that is an
// acceptable trade: the interval is bounded and predictable, whereas the backoff was buying nothing
// for a cause that is usually not transient.
//
// step names the phase of the attempt that failed ("resolve exposer", "pre-check exposure",
// "expose") and goes into the log, not the reason. The reason stays one recognised token plus the
// cause, because volumeIsParked matches on that token and status.volumes[].reason is read by
// administrators and by the alert rules.
func (r *BackupReconciler) parkVolume(ctx context.Context, vol *cbv1.VolumeStatus, step string, cause error) {
	vol.Reason = clampMessage(backupReasonExposerUnresolvable + ": " + cause.Error())
	logf.FromContext(ctx).Info("volume parked and retried, queue continues",
		"pvc", vol.Pvc, "step", step, "deadline", r.effectivePendingResolveDeadline(), "err", cause.Error())
}

// recordAdvanceFailure turns a failed advance into RECORDED STATE, which is the whole of what step
// (10) needs in order to keep going: see the long note there for why the error must not be returned
// and what was rejected.
//
// It writes a reason and NOTHING ELSE. It does not touch the phase, because the phase is what the
// next pass dispatches on and what the volume's own deadline is measured against — a volume in
// Snapshotting whose readiness check errored still has an exposure, possibly a snapshot the storage
// system is holding, and a clock running on the origin VolumeSnapshot's creationTimestamp. Demoting
// it (to Pending, say, so parking applied) would re-drive Expose against a live exposure and throw
// that clock away, which is a worse bug than the one being fixed. Promoting it (to Failed) would
// terminate somebody's backup on a transient apiserver blip. Recording is the only honest move: the
// verdict belongs to the deadlines, which are the only arbiter here with a durable clock.
//
// No Event, deliberately. This is reached once per backupPollInterval for as long as the cause
// lasts, so an Event per pass would be a flood, and the volume's eventual deadline emits exactly one
// Warning carrying the diagnosis. The log line is the per-pass record. parkVolume made the same
// choice.
func (r *BackupReconciler) recordAdvanceFailure(ctx context.Context, vol *cbv1.VolumeStatus, cause error) {
	switch vol.Phase {
	case status.VolumePhasePending, "":
		// UNREACHABLE from today's dispatch, and provably so: advancePending has no error result at
		// all (every one of its four failure paths parks or records a verdict), so advanceVolumePhase's
		// Pending case cannot hand this function anything. It is here for the day somebody gives the
		// Pending phase a step that CAN fail — a snapshotting hook, a quota pre-flight — because such a
		// path must land on the PARKING door rather than this one:
		// parking is what defers a volume behind its un-tried neighbours (volumeIsParked matches the
		// parking token and the Pending phase, and nothing else), and a Pending volume that kept the
		// head of its namespace's queue is the original thirty-hour defect verbatim. The dispatch in
		// advanceVolume accepts "" as Pending, so this must too.
		r.parkVolume(ctx, vol, "advance", cause)
		return
	}
	vol.Reason = clampMessage(backupReasonAdvanceRetrying + ": " + cause.Error())
	logf.FromContext(ctx).Info("volume advance failed and is retried; the pass continues and persists",
		"pvc", vol.Pvc, "phase", string(vol.Phase), "err", cause.Error())
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
		// STILL A RETURN, and deliberately, unlike the readiness check below. Reconcile step (10) records
		// this cause and persists the pass, so the error no longer costs the namespace its status write —
		// but it cannot be turned into "not ready and let the deadline decide", because the deadline's
		// clock is read THROUGH the exposure this call is what failed to produce (Progress needs both the
		// exposer and the derived origin VolumeSnapshot name).
		//
		// So this is the first of the sites where NO per-object bound is reachable, and where the outer
		// backstop is the only thing that can ever end the volume. Of the two ways out that were
		// identified — deriving the origin snapshot's identity without calling Expose (a change inside
		// internal/exposer) or a phase-entry timestamp on VolumeStatus — this lot took the timestamp,
		// as a BACKSTOP and explicitly not as a replacement for the snapshot-derived bounds: those can
		// tell "nobody picked it up" from "picked up and never finished" and a wall clock cannot.
		cause := fmt.Errorf("reconstruct exposure for PVC %s/%s: %w", backup.Namespace, vol.Pvc, err)
		if pvc, bounded := r.failVolumeOnAdvanceBackstop(ctx, backup, vol, cause); bounded {
			return pvc, nil // request teardown once Reconcile has persisted this terminal result
		}
		return "", cause
	}
	ready, err := ex.Ready(ctx, exposure)
	if err != nil {
		// A READINESS CHECK THAT FAILED IS NOT READY — it is NOT a reason to leave this function, and
		// the difference is the one specific, checkable consequence this lot owes.
		//
		// The old shape returned the error here, which skipped the not-ready branch below — so
		// snapshotProgressDeadline and snapshotReadyDeadline, the two bounds whose entire purpose is
		// to end a volume that is stuck in Snapshotting, were consulted ONLY when Ready cleanly
		// answered "no". On the path where Ready itself keeps failing — the snapshot CRDs removed
		// under a running operator, the VolumeSnapshot RBAC narrowed, an apiserver refusing that
		// resource — they were unreachable, and unreachable is worse than absent: an operator reads
		// the deadline in the docs, sees the run hang past it, and concludes the deadline is a lie.
		//
		// Recording the cause and falling through is safe in the direction that matters. ready=false
		// can only lead to the deadline evaluation and a requeue; the one thing a wrong answer here
		// could do is create a mover Job against an exposure that is not actually mountable, and that
		// needs ready=TRUE, which an error can never now produce. snapshotDeadlineExceeded is built
		// for exactly this uncertainty (read its doc: every "I do not know" returns "", so a pass that
		// cannot tell simply waits) — its verdict comes from Progress, an independent read of the
		// origin VolumeSnapshot, so a broken Ready does not silently arm it either.
		r.recordAdvanceFailure(ctx, vol,
			fmt.Errorf("check exposure readiness for PVC %s/%s: %w", backup.Namespace, vol.Pvc, err))
		ready = false
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
		// THE OUTER BOUND ON THE WAIT ITSELF, and it closes the last way a Snapshotting volume could
		// wait forever. snapshotDeadlineExceeded returns "" for two very different situations: the wait
		// is still legitimate (a young snapshot), or nothing about the origin snapshot could be READ, so
		// there is no evidence either way — and its doc is emphatic that the absence of evidence must
		// never be a verdict. That restraint is right and stays; what it leaves behind is a volume whose
		// clock is permanently unreadable, waiting on a judgement that can never arrive.
		//
		// The backstop can be applied here without weakening either specific bound, because it is longer
		// than both: an origin snapshot that IS readable is judged at 15 minutes or 2 hours, hours before
		// this can fire, so the better diagnosis always wins and this one is reached only when there was
		// none to be had.
		if pvc, bounded := r.failVolumeOnAdvanceBackstop(ctx, backup, vol,
			fmt.Errorf("the exposure for PVC %s/%s never became ready and nothing could be read about "+
				"its origin VolumeSnapshot, so neither snapshot deadline could judge it",
				backup.Namespace, vol.Pvc)); bounded {
			return pvc, nil // request teardown once Reconcile has persisted this terminal result
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
	//
	// The ERROR path is backstopped and the BLOCKED path is not, which is the whole distinction this
	// bound rests on. An errored gate is a pass that learned nothing and will keep learning nothing;
	// a blocked gate is the mechanism working — the volume is waiting for a slot or for a stale-lock
	// unlock to clear, both of which legitimately take as long as the cascade ahead of it does, and
	// failing a volume for queueing politely would be a defect of its own.
	if blocked, err := r.moverSlotBlocked(ctx, moverName, rc.repoName, rc.maxConcurrentMovers); err != nil {
		if pvc, bounded := r.failVolumeOnAdvanceBackstop(ctx, backup, vol, err); bounded {
			return pvc, nil // request teardown once Reconcile has persisted this terminal result
		}
		return "", err
	} else if blocked {
		return "", nil
	}

	if err := ensureMoverCredsSecret(ctx, r.maintenanceDeps(), moverName, rc.dek, rc.s3CredsSecret, rc.credsNamespace, labels); err != nil {
		// No mover Job exists yet, so no clock exists yet either: nothing but the phase-entry timestamp
		// can ever end a volume whose per-Job creds Secret the apiserver keeps refusing.
		if pvc, bounded := r.failVolumeOnAdvanceBackstop(ctx, backup, vol, err); bounded {
			return pvc, nil // request teardown once Reconcile has persisted this terminal result
		}
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
			// The same wall as the creds Secret above: moverStartDeadline is measured from the mover
			// Job's own creationTimestamp, so a Job that cannot be created has no clock. A validating
			// webhook refusing Jobs, or a narrowed RBAC, therefore used to hold the volume — and the
			// namespace's queue behind it — with nothing that could end it.
			cause := fmt.Errorf("create mover Job %s/%s: %w", r.OperatorNamespace, moverName, err)
			if pvc, bounded := r.failVolumeOnAdvanceBackstop(ctx, backup, vol, cause); bounded {
				return pvc, nil // request teardown once Reconcile has persisted this terminal result
			}
			return "", cause
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
			// nothing about this volume) or a new phase-entry timestamp on VolumeStatus (a CRD field).
			//
			// THAT FIELD NOW EXISTS, and this branch is the clearest case for it: informer lag is
			// seconds, our own teardown racing a stale reconcile is seconds, and a Job that is still
			// missing four hours later is not lagging — it is gone, and the volume is waiting on an
			// object that is never coming back. So the wait stays exactly as it was (never re-drive to
			// Snapshotting, which would re-create the exposure and, mid-deletion, leak a clone; never
			// false-fail on lag) and it is now bounded at the far end. The alert remains the faster
			// signal — CrystalbackupBackupStalled reads a state-derived series precisely so it fires on
			// the phases no in-controller clock can see — but the run no longer depends on somebody
			// reading it in order to finish.
			if pvc, bounded := r.failVolumeOnAdvanceBackstop(ctx, backup, vol,
				fmt.Errorf("mover Job %s/%s does not exist, so there is nothing to poll and no clock to "+
					"measure: it was never created, or it was deleted from under this run",
					r.OperatorNamespace, moverName)); bounded {
				return pvc, nil // request teardown once Reconcile has persisted this terminal result
			}
			return "", nil
		}
		// The Job may well be there; we cannot READ it, so moverStalled cannot run and moverStartDeadline
		// is as unreachable as if the Job did not exist. Recorded and retried by Reconcile step (10),
		// bounded here.
		cause := fmt.Errorf("get mover Job %s/%s: %w", r.OperatorNamespace, moverName, err)
		if pvc, bounded := r.failVolumeOnAdvanceBackstop(ctx, backup, vol, cause); bounded {
			return pvc, nil // request teardown once Reconcile has persisted this terminal result
		}
		return "", cause
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
		// NO BACKSTOP HERE. THIS IS THE LINE THE WHOLE DESIGN OF advanceRetryDeadline PROTECTS.
		//
		// Reaching this return means the Job was read successfully and moverStalled said the mover is
		// fine: a pod of it has reached Running, or it has not yet had moverStartDeadline to try. From
		// here the mover has as long as it likes — a multi-terabyte volume legitimately uploads for many
		// hours, and every other branch that ends a volume in this function is careful to cost nothing
		// when it fires (no bytes uploaded, no repository lock taken).
		//
		// Applying the phase-entry clock here would convert it, in one line, from a backstop on volumes
		// nothing can be learned about into a WALL-CLOCK CAP ON BACKUP DURATION. That would destroy the
		// largest and most valuable volumes in the fleet first, on the nights they matter most, while
		// looking like a working feature. The anti-regression spec that pins this is the most important
		// test in this lot.
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
// verification read: the one thing standing between a missed delete and AnnotationExposuresCleaned,
// which silences the sweep forever. It deliberately checks what the crucible leak-check checks.
//
// EVERY WAY OF NOT KNOWING REPORTS RESIDUE. That principle already covered an unreadable cluster
// (an errored LIST is residue, never clean); 0.6.5 proved it had to cover an unaddressable one too:
//
//   - the selector was managed-by + cluster-backup + namespace, and on the namespace plane the run
//     value is the empty string. `cluster-backup=` selects objects whose value for that key is "",
//     which the leaked origin content did not carry at all (see exposureLabels rule 2). So the read
//     returned nothing, the sweep called itself clean without having looked at the object it exists
//     to verify, and the marker sealed it. A verification that cannot fail is worse than none,
//     because it is believed — that is the defect class this release exists to remove, and it was
//     sitting inside the release's own safety net;
//   - so the selector is now VALIDATED before use, and a coordinate that cannot address this
//     Backup's objects is reported as loudly as an API error rather than as a clean bill of health.
//
// Selection is (managed-by + namespace) — coordinates that are non-empty for every Backup on both
// planes — and ATTRIBUTION to this specific Backup happens in Go, via exposureResidueBelongsTo.
// Doing it that way rather than by putting the owner name in the selector is deliberate: the
// attribution has to accept the pre-upgrade shape (no owner label at all), which no label selector
// can express as "belongs to this one".
//
// A cluster without the snapshot CRDs vacuously has no VS/VSC residue (NoMatch tolerated).
func (r *BackupReconciler) exposureResidueRemains(ctx context.Context, backup *cbv1.Backup) string {
	sel := client.MatchingLabels{
		apiconst.LabelManagedBy: apiconst.ManagedByValue,
		apiconst.LabelNamespace: backup.Namespace,
	}
	// The validation is structural, not a check per known field: every coordinate must be non-empty,
	// so a coordinate added here later cannot silently degrade into `key=` and take the read's ability
	// to fail with it. backup.Name is validated too — it is not in the selector, but attribution is
	// impossible without it, and "cannot attribute" must not read as "nothing to attribute".
	for key, value := range sel {
		if value == "" {
			return "exposure residue selector unresolvable: label " + key +
				" has no value for this Backup, so no read can address its exposure objects"
		}
	}
	if backup.Name == "" {
		return "exposure residue selector unresolvable: the Backup has no name to attribute residue to"
	}

	vscs := exposer.VolumeSnapshotContentList()
	switch err := r.List(ctx, vscs, sel); {
	case err == nil:
		for i := range vscs.Items {
			if r.exposureResidueBelongsTo(backup, vscs.Items[i].GetLabels()) {
				return "VolumeSnapshotContent " + vscs.Items[i].GetName()
			}
		}
	case !apimeta.IsNoMatchError(err):
		return "VolumeSnapshotContent list unreadable: " + err.Error()
	}

	for _, ns := range []string{backup.Namespace, r.OperatorNamespace} {
		vss := exposer.VolumeSnapshotList()
		switch err := r.List(ctx, vss, sel, client.InNamespace(ns)); {
		case err == nil:
			for i := range vss.Items {
				if r.exposureResidueBelongsTo(backup, vss.Items[i].GetLabels()) {
					return "VolumeSnapshot " + ns + "/" + vss.Items[i].GetName()
				}
			}
		case !apimeta.IsNoMatchError(err):
			return "VolumeSnapshot list unreadable: " + err.Error()
		}
	}

	var pvcs corev1.PersistentVolumeClaimList
	if err := r.List(ctx, &pvcs, sel, client.InNamespace(r.OperatorNamespace)); err != nil {
		return "temp clone PVC list unreadable: " + err.Error()
	}
	for i := range pvcs.Items {
		if r.exposureResidueBelongsTo(backup, pvcs.Items[i].Labels) {
			return "temp clone PVC " + r.OperatorNamespace + "/" + pvcs.Items[i].Name
		}
	}
	return ""
}

// exposureResidueBelongsTo decides whether one labelled leftover in this Backup's namespace is
// THIS Backup's residue. The caller has already narrowed the set to managed-by + our namespace, so
// the only question left is which Backup in that namespace it belonged to.
//
// The three cases are the three label shapes that exist in the field, in decreasing precision:
//
//  1. an owner name (LabelBackup): exact, and the only shape this version creates. A sibling run's
//     object is correctly disowned here — attribution must stay tight on the shape that can be
//     attributed, or every terminal Backup in a busy namespace would wedge on its neighbours' junk.
//  2. no owner name but a run label: a pre-upgrade CLUSTER-plane object, matched on the run, which
//     is exactly the selector this function used to carry. Note this also disowns a cluster-plane
//     object when we are a namespace-plane Backup (our run is "", theirs is not) — different owner.
//  3. neither: a pre-upgrade NAMESPACE-plane object, which carries no owner identity anywhere, so
//     the best available evidence is that it is an exposure of a PVC WE backed up. That is
//     deliberately over-inclusive: a sibling run's leftover for the same PVC in the same namespace
//     is indistinguishable and will be attributed here too.
//
// Over-inclusion is the right error for THIS caller and would be the wrong one for the reaper. The
// consequence here is a false "not clean": the marker is withheld and the sweep re-verifies every
// exposureDrainRecheckInterval, deleting only its OWN objects (the sweep is derive-only) — no
// unrelated object is ever touched. The consequence there would be deleting a live run's snapshot,
// which is why reaper.go pays for the same case with an explicit no-claimant scan instead. And the
// loop is not forever: the reaper collects that legacy leftover, after which this read goes clean.
func (r *BackupReconciler) exposureResidueBelongsTo(backup *cbv1.Backup, labels map[string]string) bool {
	if name := labels[apiconst.LabelBackup]; name != "" {
		return name == backup.Name
	}
	if run := labels[apiconst.LabelClusterBackup]; run != "" {
		return run == backup.Labels[apiconst.LabelClusterBackup]
	}
	pvc := labels[apiconst.LabelPVC]
	if pvc == "" {
		return false // not a per-PVC exposure object; nothing here can attribute it.
	}
	for i := range backup.Status.Volumes {
		if backup.Status.Volumes[i].Pvc == pvc {
			return true
		}
	}
	return false
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
// managed-by marker gates it to CrystalBackup mover Jobs, and (owner name, namespace) locate the
// Backup. A Job that is not one of ours, or is missing either coordinate, maps to nothing.
//
// The owner name goes through ownerBackupNameFromLabels rather than reading the run label directly,
// which is what this function used to do — and which meant a NAMESPACE-plane mover Job (no run to
// stamp, so the value was "") mapped to nothing at all. The requeue interval was still driving that
// plane's progress, so nothing looked broken; what it cost was the faster nudge on every mover
// transition, including the completion that starts teardown. Same key, same fix as the reaper's.
func (r *BackupReconciler) mapJobToBackup(_ context.Context, obj client.Object) []reconcile.Request {
	labels := obj.GetLabels()
	if labels[apiconst.LabelManagedBy] != apiconst.ManagedByValue {
		return nil
	}
	name := ownerBackupNameFromLabels(labels)
	namespace := labels[apiconst.LabelNamespace]
	if name == "" || namespace == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: namespace, Name: name}}}
}

// ---------------------------------------------------------------------------
// Pure helpers (no client, no context): selection, naming, argv, phase rollup.
// ---------------------------------------------------------------------------

// exposureLabels are stamped on every object a per-PVC backup creates (the exposure's VS/VSC/temp
// PVC, the mover Job, its creds Secret) or PATCHES (the externally-created origin
// VolumeSnapshotContent). LabelManagedBy makes them all reaper-selectable, while the
// crystalbackup.io/* keys both link them to their origin and satisfy the crucible leak-check (which
// flags any residual object carrying crystalbackup.io/pvc). They deliberately omit
// app.kubernetes.io/name=crystal-backup — the operator pod's own label, which the crucible's
// operator-restart test selects on.
//
// TWO RULES here are the whole 0.6.5 leak, and neither is cosmetic.
//
// (1) The owner is named by LabelBackup — the Backup's own name — NOT by the run. On the cluster
// plane the two are the same string (a fan-out child is named after its run), but on the namespace
// plane there is no run, and every reader keyed on the run label went blind on that plane: the
// terminal sweep's verification read, the orphan reaper, and the exposer's crash-window reclaim.
// The name is the identity both planes share, so it is what gets stamped, unconditionally.
//
// (2) NO KEY IS EVER STAMPED WITH AN EMPTY VALUE. The run key is added only when there is a run to
// name. Previously it was stamped as "" on the namespace plane, which is not the harmless no-op it
// looks like:
//
//   - a selector built from this map (client.MatchingLabels) then carries `cluster-backup=`, which
//     selects the objects whose value for that key is the empty STRING — it does not mean "any";
//   - and the objects do not even agree on carrying it: SetLabels persists the empty value, while
//     the origin content's handover patch goes through exposer.mergeLabels, which skips a key whose
//     desired value equals the (missing ⇒ "") current one and therefore never writes it at all.
//
// So the created objects matched that selector and the PATCHED one — the cluster-scoped origin
// VolumeSnapshotContent, the single most expensive object to leak — did not. That is the exact
// object the campaign leaked, and the exact reason three separate collectors could not see it.
func exposureLabels(backup *cbv1.Backup, pvcName string) map[string]string {
	labels := map[string]string{
		apiconst.LabelManagedBy: apiconst.ManagedByValue,
		apiconst.LabelBackup:    backup.Name,
		apiconst.LabelNamespace: backup.Namespace,
		apiconst.LabelPVC:       pvcName,
	}
	// Kept for the cluster plane: it is the run-wide coordinate humans and the crucible query by
	// ("show me everything last night's run left behind"), which the per-Backup name cannot answer
	// for a fan-out of many namespaces. Added only when it has a value — see rule (2).
	if run := backup.Labels[apiconst.LabelClusterBackup]; run != "" {
		labels[apiconst.LabelClusterBackup] = run
	}
	return labels
}

// ownerBackupNameFromLabels resolves the NAME of the Backup that owns an exposure object, reading
// only the object's own labels; its namespace is LabelNamespace, and the two together are the owner's
// object key. Returns "" when the object carries no owner name at all, which callers must treat as
// "not attributable by name" — never as "no owner".
//
// The fallback chain IS the upgrade path. LabelBackup is stamped by this version on both planes;
// objects created by an earlier one do not have it, and for those the run label is a complete
// substitute ON THE CLUSTER PLANE ONLY, because its documented value contract is that the run name
// equals the child Backup's metadata.name (see apiconst.LabelClusterBackup). A pre-upgrade
// NAMESPACE-plane object has neither key — the run label was stamped empty there and, on the
// merge-patched origin content, not stamped at all — so it resolves to "" here and needs the
// by-exclusion fallback the reaper implements. Without that, upgrading the operator would convert
// every piece of already-leaked namespace-plane residue into permanent residue.
func ownerBackupNameFromLabels(labels map[string]string) string {
	if name := labels[apiconst.LabelBackup]; name != "" {
		return name
	}
	return labels[apiconst.LabelClusterBackup]
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

// matchPVC reports whether a PVC is selected by sel.
//
// The rule itself lives on the API type (cbv1.PVCSelector.Matches) rather than here, because it is
// part of the API contract and more than one package needs to answer the same question — the
// controller that acts on it, and `selfcheck` which tells an administrator which PVCs a schedule
// covers. Two implementations of that question is a product that can say one thing and do another.
func matchPVC(pvc *corev1.PersistentVolumeClaim, sel cbv1.PVCSelector) bool {
	return sel.Matches(pvc.Name, pvc.Labels)
}

// firstNonTerminalVolume returns the index of the volume to advance this reconcile, or -1 if every
// volume is terminal.
//
// It is the first non-terminal volume (phase not Completed/Skipped/Failed) that is not PARKED — a
// parked volume being one still in Pending with a resolution failure already recorded against it.
// Parked volumes are returned only when there is nothing else left to do, so they are still retried,
// just never at the expense of a volume that has not been tried yet.
//
// That preference is the fix for the way this went wrong in production, and it is worth stating
// plainly because the naive reading of "advance one volume per reconcile" hides it. One PVC named a
// StorageClass that did not exist. It sat at the head of its namespace's queue, and because the head
// is chosen by position alone, the five volumes behind it were never even attempted — for thirty
// hours. The product owner's rule is the one encoded here: blocking a backup that could have
// succeeded because something scheduled ahead of it is broken is not acceptable.
//
// Parked volumes are not abandoned. They keep their turn once the queue drains, and
// pendingResolveDeadline eventually fails them so the run can reach a terminal phase at all — the two
// mechanisms are halves of one guarantee, and neither is sufficient alone: the preference stops a
// broken volume from starving healthy ones, the deadline stops it from keeping the run alive forever.
func firstNonTerminalVolume(vols []cbv1.VolumeStatus) int {
	parked := -1
	for i := range vols {
		switch vols[i].Phase {
		case status.VolumePhaseCompleted, status.VolumePhaseSkipped, status.VolumePhaseFailed:
			continue
		}
		if volumeIsParked(&vols[i]) {
			if parked < 0 {
				parked = i
			}
			continue
		}
		return i
	}
	return parked
}

// volumeIsParked reports whether vol is a Pending volume whose exposer resolution has already failed
// at least once. The recorded reason IS the state — no extra field — because advancePending is the
// only writer that leaves a reason on a still-Pending volume, and it writes exactly this one.
// The empty phase is accepted as Pending because advanceVolume's own dispatch does
// (`case status.VolumePhasePending, ""`), and a predicate that disagreed with the dispatch would
// leave exactly one shape of volume — one seeded before the phase was written — able to park itself
// and then hold the head of the queue anyway, which is the entire bug this is here to prevent.
func volumeIsParked(vol *cbv1.VolumeStatus) bool {
	switch vol.Phase {
	case status.VolumePhasePending, "":
		// The separator is part of the match on purpose: parkVolume writes
		// "ExposerUnresolvable: <cause>", and a bare prefix test would silently adopt any FUTURE reason
		// that merely starts with the same word (an "ExposerUnresolvableAfterExpose", say) into a
		// parking rule that was never reasoned about for it.
		return strings.HasPrefix(vol.Reason, backupReasonExposerUnresolvable+":")
	default:
		return false
	}
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
