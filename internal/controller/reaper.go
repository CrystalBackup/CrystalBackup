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
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
	"github.com/CrystalBackup/CrystalBackup/internal/exposer"
	"github.com/CrystalBackup/CrystalBackup/internal/metrics"
	"github.com/CrystalBackup/CrystalBackup/internal/status"
)

const (
	// defaultReaperInterval is how often the orphan reaper sweeps.
	defaultReaperInterval = 10 * time.Minute

	// defaultReaperMinAge is the minimum age an object must reach before the reaper will touch it,
	// even when it already looks orphaned. It is a race guard: an exposure's temp PVC / mover Job
	// can exist for a few reconciles before the owning Backup's status catches up, and reaping one
	// out from under a slow-but-live reconcile would corrupt an in-flight backup. Nothing younger
	// than this is ever swept.
	defaultReaperMinAge = 30 * time.Minute
)

// The kinds the reaper reports on. They are the `kind` label values of
// crystalbackup_orphan_reap_stuck and must stay in step with metrics.OrphanReapStuckKinds — pinned
// by TestStuckKindsAreAllPublished, because a kind this package names and that package does not
// enumerate would never be reset to zero.
const (
	kindJob                   = "Job"
	kindPVC                   = "PersistentVolumeClaim"
	kindSecret                = "Secret"
	kindPV                    = "PersistentVolume"
	kindVolumeSnapshot        = "VolumeSnapshot"
	kindVolumeSnapshotContent = "VolumeSnapshotContent"
	kindRoleBinding           = "RoleBinding"
	kindClusterRoleBinding    = "ClusterRoleBinding"
)

// eventReasonReapStuck is the Event reason for a deletion the reaper requested and that Kubernetes
// has not completed. It is the ONE thing in this file that must reach a human without anybody
// reading a log line, so it is a Warning on the stuck object itself: `kubectl describe` on the
// object an administrator is already staring at explains why it will not go away, and names the
// finalizers to talk to.
const eventReasonReapStuck = "OrphanReapStuck"

// reapOutcome is what a DELETE the reaper issued ACTUALLY achieved — which is not the same question
// as whether the apiserver accepted it, and the 0.6.5 incident is the proof.
//
// For 31 hours the reaper logged "reaped leftover VolumeSnapshot" every 10 minutes for the same
// three objects, and reaped none of them: their deletion was blocked on external-snapshotter's
// bound-protection finalizers, so each carried a deletionTimestamp and was going nowhere. In the
// same binary, the self-check's leakIndicators reported those objects as residual — total 8,
// residual 8, oldest 31 h. One of the two was lying and it was the reaper, because a successful
// `Delete` call means the deletion was ACCEPTED FOR PROCESSING, and with a finalizer in play
// "accepted" and "gone" can be separated by forever.
//
// This is the project's standing rule applied to object deletion: verify the artifact, not the job.
// So the reaper reads the object back and reports which of these three actually happened. Nothing
// but reapConfirmedGone may ever be worded as a completed reap.
type reapOutcome int

const (
	// reapConfirmedGone: a read after the delete says the object is not there (NotFound, its kind
	// does not exist on this cluster, or the name now belongs to a DIFFERENT object — a successor
	// with another UID, which means ours did complete). This is the only outcome that is a success.
	reapConfirmedGone reapOutcome = iota

	// reapStuck: the object is still there and now carries a deletionTimestamp, i.e. one or more
	// finalizers are holding it. This is the case that was reported as success for 31 hours, and the
	// one that actually needs a person: the operator will not strip a finalizer belonging to another
	// controller (doing so on external-snapshotter's bound-protection would break the contract that
	// keeps a content from being destroyed under a live snapshot), so nothing in this process can
	// resolve it. It gets a Warning Event naming the finalizers, and a metric.
	reapStuck

	// reapRequested: the delete was accepted but the outcome could not be established — the object
	// still reads as present with NO deletionTimestamp (a stale cached read, the overwhelmingly
	// likely case immediately after a delete), or the confirming read itself failed. Deliberately
	// NOT reported as either success or a leak: it is one sweep's worth of not knowing, and the next
	// sweep resolves it into one of the other two. Calling it "reaped" is the original defect;
	// calling it stuck would cry wolf on every healthy delete whose watch had not caught up yet.
	reapRequested
)

// reapTally accumulates ONE sweep's stuck objects per kind, so the sweep publishes a complete
// picture at the end instead of a running one. A nil tally is valid and records nothing, which is
// what a caller driving a single sub-sweep in a test gets.
type reapTally struct {
	stuck map[string]int
}

func newReapTally() *reapTally { return &reapTally{stuck: map[string]int{}} }

func (t *reapTally) markStuck(kind string) {
	if t == nil {
		return
	}
	t.stuck[kind]++
}

// publish hands the tally to the metric. Called ONLY from a sweep that ran to completion — see
// metrics.SetOrphanReapStuck for why a partial tally must never be published as a whole one.
func (t *reapTally) publish() {
	if t == nil {
		return
	}
	metrics.SetOrphanReapStuck(t.stuck)
}

// OrphanReaper is the periodic backstop that keeps a run from leaking storage objects when the
// happy-path teardown was missed (an operator crash mid-cleanup, a namespace deleted out from under
// an in-flight backup). It sweeps the NATIVE per-PVC exposure objects a backup creates in the
// operator namespace — the temp clone PVC, the mover Job and its per-Job creds Secret — AND the
// labelled VolumeSnapshot / VolumeSnapshotContent residue cluster-wide, deleting whatever's owning
// Backup is gone, or whose volume for that PVC has already reached a terminal phase (so its
// teardown should already have run). It is a manager.Runnable (a timer loop), not a reconciler:
// there is no single object to reconcile, and a periodic full sweep is exactly the shape of a
// leak backstop.
//
// The VS/VSC half exists because the leak audit proved its absence was load-bearing: four
// comments across this codebase promised "the orphan reaper is the ultimate backstop" for
// crash-window snapshot residue while the reaper's charter explicitly excluded exactly that
// object class — so a single interrupted teardown stranded a cluster-scoped, Retain-parked,
// owner-less VolumeSnapshotContent forever (the fanout's residual). The sweep delegates the
// policy-correct semantics to the exposer (exposer.ReapOrphanVolumeSnapshotContent: object-only
// for the static re-bind alias, restore-then-delete for a dynamic origin content) so the storage
// snapshot is reclaimed exactly once; see reapSnapshotObjects for the ordering. The terminal
// re-entry sweep (ensureTerminalTeardown) is the FAST path — seconds after a restart, while the
// Backup CR still exists; this reaper is the SLOW, unconditional one that also covers a deleted
// CR, at MinAge distance.
type OrphanReaper struct {
	client.Client
	// OperatorNamespace is where the temp clone PVCs, mover Jobs and creds Secrets live.
	OperatorNamespace string
	// MinAge and Interval default to defaultReaperMinAge / defaultReaperInterval when zero.
	MinAge   time.Duration
	Interval time.Duration

	// APIReader is the UNCACHED reader used for the read-back that confirms a deletion. Optional:
	// when nil, Client is used.
	//
	// It matters because the whole fix rests on that read being believable, and the cached client is
	// exactly the wrong instrument for it: an informer cache lags the write it is confirming, so
	// "still present, no deletionTimestamp" is its normal answer immediately after a successful
	// delete. Reading through the cache cannot produce a false "gone" (an object absent from the
	// cache after a delete really is being deleted), but it can and does produce a false
	// "unconfirmed", which the reaper then has to carry to the next sweep. Going straight to the
	// apiserver — a handful of GETs per sweep, only for objects that were actually reap-eligible —
	// resolves most deletions inside the same sweep. See the read-after-write lesson: a transient
	// NotFound on a cached client is not an absence, and its mirror image is that a cached hit is
	// not a presence.
	APIReader client.Reader

	// Recorder puts a STUCK deletion where `kubectl describe` will show it, on the stuck object
	// itself. Optional: a nil Recorder degrades to logs only, which is what the envtest suites and
	// the fake-client tests that do not care about events get.
	Recorder events.EventRecorder
}

// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups="",resources=persistentvolumes,verbs=get;list;watch;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=rolebindings,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups=crystalbackup.io,resources=backups;restores;clusterrestores,verbs=get;list;watch
// +kubebuilder:rbac:groups="";events.k8s.io,resources=events,verbs=create;patch

// Start runs the sweep loop until ctx is cancelled. It satisfies manager.Runnable. It applies the
// production defaults for Interval and MinAge here (not in sweepOnce), so a caller can drive
// sweepOnce directly with an explicit MinAge of 0 to reap regardless of age.
func (r *OrphanReaper) Start(ctx context.Context) error {
	if r.Interval <= 0 {
		r.Interval = defaultReaperInterval
	}
	if r.MinAge <= 0 {
		r.MinAge = defaultReaperMinAge
	}
	ticker := time.NewTicker(r.Interval)
	defer ticker.Stop()

	log := logf.FromContext(ctx).WithName("orphan-reaper")
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := r.sweepOnce(ctx); err != nil {
				log.Error(err, "orphan reaper sweep failed; will retry next interval")
			}
		}
	}
}

// sweepOnce reaps every orphaned native exposure object in one pass. It is best-effort: a delete
// that races another actor (NotFound) is success, and a single object's failure is logged and does
// not abort the rest of the sweep.
//
// It also owns the per-sweep stuck tally: every sub-sweep records into the same reapTally, and the
// tally is published ONLY on the paths that reach the end. An early return (a List that failed) is
// a sweep whose picture is incomplete, and publishing it would be the same category of
// overstatement this file exists to stop.
func (r *OrphanReaper) sweepOnce(ctx context.Context) error {
	log := logf.FromContext(ctx).WithName("orphan-reaper")
	tally := newReapTally()
	sel := client.MatchingLabels{apiconst.LabelManagedBy: apiconst.ManagedByValue}
	inOperatorNS := client.InNamespace(r.OperatorNamespace)
	// r.MinAge is used literally: Start applies the production default, so a direct sweepOnce caller
	// (a test) may pass 0 to reap regardless of age.
	cutoff := time.Now().Add(-r.MinAge)

	// Mover Jobs and their per-Job creds Secrets share the managed-by + per-PVC labels; the temp
	// clone PVCs and restore staging PVCs carry the same shape. The selection requires the per-PVC
	// label POSITIVELY (HasLabels), not just the managed-by match — orphaned() also demands it, but
	// keeping it OUT of the candidate set is defense in depth: managed-by-only objects like the
	// repository-init/maintenance Jobs and, critically, the wrapped-DEK Secret (crystal-dek-<loc>,
	// the single most catastrophic object to delete) must never be a reap candidate, not merely
	// spared by one downstream guard.
	hasPVC := client.HasLabels{apiconst.LabelPVC}
	var jobs batchv1.JobList
	if err := r.List(ctx, &jobs, inOperatorNS, sel, hasPVC); err != nil {
		return err
	}
	var pvcs corev1.PersistentVolumeClaimList
	if err := r.List(ctx, &pvcs, inOperatorNS, sel, hasPVC); err != nil {
		return err
	}
	var secrets corev1.SecretList
	if err := r.List(ctx, &secrets, inOperatorNS, sel, hasPVC); err != nil {
		return err
	}
	// Restore-created PersistentVolumes (cluster-scoped) are swept ONLY when they carry the
	// pv-role marker: a twin PV is an alias whose deletion is always safe (reclaimPolicy
	// Retain by construction), and a still-labeled transplant PV is an unfinished handover
	// whose volume must be reclaimed — a DELIVERED transplant was unlabeled at Finalize, so
	// the reaper can never see (or touch) a restored user volume.
	var pvs corev1.PersistentVolumeList
	if err := r.List(ctx, &pvs, client.HasLabels{apiconst.LabelPVRole}); err != nil {
		return err
	}

	reap := func(kind string, obj client.Object) {
		orphaned, err := r.orphaned(ctx, obj, cutoff)
		if err != nil {
			log.Error(err, "orphan reaper: orphan check failed", "kind", kind, "name", obj.GetName())
			return
		}
		if !orphaned {
			return
		}
		r.reapAndReport(ctx, kind, obj, tally, "leftover exposure object",
			nil, // a Job/PVC/Secret needs no preparation: the object IS the whole thing to remove.
			func() error {
				return r.Delete(ctx, obj, client.PropagationPolicy(metav1.DeletePropagationBackground))
			},
			"backup", ownerBackupNameFromLabels(obj.GetLabels()), "pvc", obj.GetLabels()[apiconst.LabelPVC])
	}

	for i := range jobs.Items {
		reap(kindJob, &jobs.Items[i])
	}
	for i := range pvcs.Items {
		reap(kindPVC, &pvcs.Items[i])
	}
	for i := range secrets.Items {
		reap(kindSecret, &secrets.Items[i])
	}
	for i := range pvs.Items {
		r.reapRestorePV(ctx, &pvs.Items[i], cutoff, tally)
	}
	// The labelled snapshot residue (VolumeSnapshots + VolumeSnapshotContents) — the leak
	// audit's missing half. See reapSnapshotObjects for why its internal order matters.
	r.reapSnapshotObjects(ctx, cutoff, tally)
	// Transient manifest-mover RoleBindings, swept on their own (much shorter) clock — see
	// reapManifestBindings.
	r.reapManifestBindings(ctx, tally)
	// And their cluster-scoped counterparts from a cluster-manifests capture (adr/0011 §1),
	// which live in no namespace and so need a separate list.
	r.reapClusterManifestBindings(ctx, tally)
	// The sweep completed: this tally is the whole picture, so it may be published as such.
	tally.publish()
	return nil
}

// reapAndReport performs the honest reap of ONE object the caller has already vetted as orphaned,
// and is the single place in this file allowed to say a reap happened.
//
// The shape is prepare-then-delete-then-VERIFY, and each part is load-bearing:
//
//   - prepare (may be nil) is the idempotent, non-destructive work a reap needs that is NOT the
//     delete — today, restoring a dynamic VolumeSnapshotContent's deletionPolicy to Delete. It runs
//     on EVERY sweep, including when a deletion is already pending, because a content that is stuck
//     on a finalizer while still Retain-parked would orphan its storage-side snapshot in the backend
//     the instant the finalizer clears. See
//     exposer.PrepareOrphanVolumeSnapshotContentForReclaim.
//   - del is NOT re-issued when a deletion is already pending (the object arrived from the List with
//     a deletionTimestamp). That is the 31-hour loop: the same three objects, a fresh DELETE and a
//     fresh "reaped" line every 10 minutes, forever. Re-asking cannot help — a finalizer is not
//     waiting for a second request — so the reaper reports and moves on. This is also why the
//     function never blocks, never sleeps and never retries a stuck object within a sweep: the next
//     sweep is the retry, and a stuck object costs one GET per sweep and nothing else.
//   - the outcome is then READ BACK rather than assumed.
//
// The three outcomes are worded so that no reader can mistake one for another: only
// reapConfirmedGone gets the word "reaped". A stuck object additionally gets a Warning Event on
// itself, naming the finalizers, because that is the actionable detail and because an administrator
// must not have to read operator logs to discover that a deletion is deadlocked.
func (r *OrphanReaper) reapAndReport(
	ctx context.Context,
	kind string,
	obj client.Object,
	tally *reapTally,
	subject string,
	prepare func() error,
	del func() error,
	kv ...any,
) {
	log := logf.FromContext(ctx).WithName("orphan-reaper")
	// Built by appending rather than as one composite literal: logcheck requires every log KEY to be
	// an inlined literal (so no named constants), and goconst counts repeated literals unless they sit
	// in a call's argument list. Both linters are satisfied by this shape and by no other.
	base := make([]any, 0, 6+len(kv))
	base = append(base, "kind", kind, "namespace", obj.GetNamespace(), "name", obj.GetName())
	base = append(base, kv...)

	if prepare != nil {
		if err := prepare(); err != nil && !apierrors.IsNotFound(err) && !apimeta.IsNoMatchError(err) {
			log.Error(err, "orphan reaper: preparing the object for reclamation failed; "+
				"NOT deleting it — a delete on an unprepared object can orphan storage", base...)
			return
		}
	}

	alreadyPending := !obj.GetDeletionTimestamp().IsZero()
	if !alreadyPending {
		if err := del(); err != nil && !apierrors.IsNotFound(err) && !apimeta.IsNoMatchError(err) {
			log.Error(err, "orphan reaper: delete failed", base...)
			return
		}
	}

	outcome, finalizers, confirmErr := r.confirmReap(ctx, obj)
	switch outcome {
	case reapConfirmedGone:
		log.Info("orphan reaper: reaped "+subject+"; confirmed gone by a read-back", base...)

	case reapStuck:
		tally.markStuck(kind)
		held := strings.Join(finalizers, ",")
		stuckKV := append(append([]any{}, base...), "finalizers", held, "deletionRequestedAlready", alreadyPending)
		// Info, not Error: nothing failed and there is nothing here for the operator process to fix.
		// The severity lives on the Event and the metric, which are the two channels an administrator
		// actually watches.
		log.Info("orphan reaper: deletion is STUCK — the object still exists with a deletionTimestamp, "+
			"held by a finalizer; this is NOT a completed reap and only an administrator can clear it", stuckKV...)
		if r.Recorder != nil {
			r.Recorder.Eventf(obj, nil, corev1.EventTypeWarning, eventReasonReapStuck, "Reap",
				"Deletion of this orphaned CrystalBackup object has been requested but has not completed: "+
					"it still exists with a deletionTimestamp and is held by finalizer(s) %s. CrystalBackup "+
					"will not remove a finalizer it does not own, so this will not clear on its own — "+
					"the controller behind that finalizer must release it, or an administrator must.",
				held)
		}

	case reapRequested:
		// One sweep's worth of not knowing. Worded as a request, never as an outcome.
		requestedKV := base
		if confirmErr != nil {
			requestedKV = append(append([]any{}, base...), "confirmError", confirmErr.Error())
		}
		log.Info("orphan reaper: deletion REQUESTED and accepted, outcome not yet confirmed; "+
			"the next sweep resolves it as gone or as stuck", requestedKV...)
	}
}

// confirmReap reads obj back and reports which reapOutcome actually holds, plus the finalizers
// holding a stuck object.
//
// It reads through APIReader when one is set, deliberately (see the field's comment). The UID
// comparison is not defensive noise: reaping by label can name an object that has since been
// recreated with the same name by a later run, and a present-but-different UID means OUR object did
// in fact complete its deletion. Without that check the successor's own (absent) deletionTimestamp
// would be read as "unconfirmed" forever.
func (r *OrphanReaper) confirmReap(ctx context.Context, obj client.Object) (reapOutcome, []string, error) {
	reader := client.Reader(r.Client)
	if r.APIReader != nil {
		reader = r.APIReader
	}
	probe, ok := obj.DeepCopyObject().(client.Object)
	if !ok {
		return reapRequested, nil, fmt.Errorf("object %T is not a client.Object after a deep copy", obj)
	}
	if err := reader.Get(ctx, client.ObjectKeyFromObject(obj), probe); err != nil {
		if apierrors.IsNotFound(err) || apimeta.IsNoMatchError(err) {
			return reapConfirmedGone, nil, nil
		}
		return reapRequested, nil, err
	}
	if uid := obj.GetUID(); uid != "" && probe.GetUID() != "" && probe.GetUID() != uid {
		return reapConfirmedGone, nil, nil // a successor took the name; ours is gone.
	}
	if !probe.GetDeletionTimestamp().IsZero() {
		return reapStuck, probe.GetFinalizers(), nil
	}
	return reapRequested, nil, nil
}

// reapSnapshotObjects sweeps the labelled VolumeSnapshot / VolumeSnapshotContent residue whose
// happy-path teardown never completed. Selection mirrors the native sweep exactly — managed-by +
// a POSITIVE per-PVC label, then orphaned()'s owner-gone / volume-terminal / MinAge vetting — but
// the objects live cluster-wide (the origin VS in a tenant namespace, the VSCs in no namespace at
// all), so the lists are not namespace-scoped.
//
// The internal order is cleanup()'s reclamation-last, restated for a flat sweep:
//
//  1. pre-provisioned (static re-bind) contents — object-only deletes, always safe (Retain by
//     construction; the backend snapshot is the origin content's to reclaim);
//  2. VolumeSnapshots — plain deletes (a bound Delete-policy content cascades correctly via the
//     external snapshot-controller; a Retain-parked one becomes pass-3 residue);
//  3. dynamically-provisioned contents — exposer.ReapOrphanVolumeSnapshotContent's
//     restore-then-delete, so the storage-side snapshot is reclaimed exactly once, and never
//     while a static alias still references its handle.
//
// Best-effort per object, like the native sweep. A cluster without the snapshot CRDs (NoMatch)
// skips silently: no such kind, no such residue.
//
// This is the sweep the 0.6.5 honesty defect was found in, and it is where the stakes are highest:
// both halves of the snapshot pair carry external-snapshotter's bound-protection finalizers
// (snapshot.storage.kubernetes.io/volumesnapshot-bound-protection on the snapshot,
// volumesnapshotcontent-bound-protection on the content), which is precisely the mechanism that can
// hold a requested deletion indefinitely. Every delete here therefore goes through reapAndReport,
// and nothing here strips a finalizer: bound-protection exists to stop a content being destroyed
// under a live snapshot, and tearing it off would be this operator breaking another controller's
// contract on objects it does not own.
func (r *OrphanReaper) reapSnapshotObjects(ctx context.Context, cutoff time.Time, tally *reapTally) {
	log := logf.FromContext(ctx).WithName("orphan-reaper")
	sel := client.MatchingLabels{apiconst.LabelManagedBy: apiconst.ManagedByValue}
	hasPVC := client.HasLabels{apiconst.LabelPVC}

	vscs := exposer.VolumeSnapshotContentList()
	if err := r.List(ctx, vscs, sel, hasPVC); err != nil {
		if !apimeta.IsNoMatchError(err) {
			log.Error(err, "orphan reaper: listing VolumeSnapshotContents")
		}
		return
	}
	vss := exposer.VolumeSnapshotList()
	if err := r.List(ctx, vss, sel, hasPVC); err != nil {
		if !apimeta.IsNoMatchError(err) {
			log.Error(err, "orphan reaper: listing VolumeSnapshots")
		}
		return
	}

	var staticVSCs, dynamicVSCs []*unstructured.Unstructured
	for i := range vscs.Items {
		item := &vscs.Items[i]
		if exposer.IsPreProvisionedContent(item) {
			staticVSCs = append(staticVSCs, item)
		} else {
			dynamicVSCs = append(dynamicVSCs, item)
		}
	}

	reapVSC := func(item *unstructured.Unstructured) {
		orphaned, err := r.orphaned(ctx, item, cutoff)
		if err != nil {
			log.Error(err, "orphan reaper: VolumeSnapshotContent orphan check failed", "name", item.GetName())
			return
		}
		if !orphaned {
			return
		}
		// A content reaching this path means BOTH the inline teardown and the terminal re-entry
		// sweep missed it (or its Backup CR is long gone) — worth noticing, never routine.
		// The reclaim POLICY restore is the prepare half and runs even on an already-terminating
		// content (a Retain-parked content that vanishes when its finalizer clears takes its backend
		// snapshot out of anyone's reach); the object delete is the half that is not re-issued.
		r.reapAndReport(ctx, kindVolumeSnapshotContent, item, tally,
			"leftover VolumeSnapshotContent whose nominal teardown never completed",
			func() error { return exposer.PrepareOrphanVolumeSnapshotContentForReclaim(ctx, r.Client, item) },
			func() error { return r.Delete(ctx, item) },
			"preProvisioned", exposer.IsPreProvisionedContent(item),
			"backup", ownerBackupNameFromLabels(item.GetLabels()), "pvc", item.GetLabels()[apiconst.LabelPVC])
	}

	for _, item := range staticVSCs {
		reapVSC(item)
	}
	for i := range vss.Items {
		item := &vss.Items[i]
		orphaned, err := r.orphaned(ctx, item, cutoff)
		if err != nil {
			log.Error(err, "orphan reaper: VolumeSnapshot orphan check failed",
				"namespace", item.GetNamespace(), "name", item.GetName())
			continue
		}
		if !orphaned {
			continue
		}
		r.reapAndReport(ctx, kindVolumeSnapshot, item, tally,
			"leftover VolumeSnapshot whose nominal teardown never completed",
			nil, // deleting the snapshot object is the whole reap; its content is handled in its own pass.
			func() error { return r.Delete(ctx, item) },
			"backup", ownerBackupNameFromLabels(item.GetLabels()), "pvc", item.GetLabels()[apiconst.LabelPVC])
	}
	for _, item := range dynamicVSCs {
		reapVSC(item)
	}
}

// reapManifestBindings sweeps tenant namespaces for transient manifest-mover RoleBindings whose
// Job is gone or already finished.
//
// This is the ONLY automatic cleanup for those bindings. Everything else the reaper handles has
// an ownerReference somewhere as a first line of defence; a RoleBinding in a tenant namespace
// cannot have one, because its Job lives in the operator namespace and Kubernetes rejects
// cross-namespace ownerReferences (spec/03-security-and-tenancy.md §5). If the operator dies
// between "Job completed" and "delete RoleBinding", this sweep is what stops a
// read-on-all-Secrets grant standing in someone's namespace indefinitely.
//
// It uses manifestBindingMinAge rather than the reaper's general MinAge. The general default is
// calibrated for a temp clone PVC, where the cost of waiting is storage; here it is a live
// privilege.
func (r *OrphanReaper) reapManifestBindings(ctx context.Context, tally *reapTally) {
	log := logf.FromContext(ctx).WithName("orphan-reaper")

	var bindings rbacv1.RoleBindingList
	// Cluster-wide on purpose: the bindings live in arbitrary tenant namespaces. The label
	// selector keeps that from being a sweep of every RoleBinding in the cluster, and the
	// operator-namespace label keeps one operator from reaping another's in-flight grant.
	if err := r.List(ctx, &bindings,
		client.MatchingLabels{
			apiconst.LabelManagedBy:  apiconst.ManagedByValue,
			apiconst.LabelMoverRole:  apiconst.MoverRoleManifest,
			apiconst.LabelOperatorNS: r.OperatorNamespace,
		}); err != nil {
		log.Error(err, "listing transient manifest RoleBindings")
		return
	}

	cutoff := time.Now().Add(-manifestBindingMinAge)
	for i := range bindings.Items {
		rb := &bindings.Items[i]
		if rb.CreationTimestamp.After(cutoff) {
			continue
		}
		jobName := rb.Labels[apiconst.LabelMoverJob]
		if jobName == "" {
			// No Job label: this predates the label or was hand-made. Refuse to guess — an
			// erroneous delete here breaks an in-flight backup, and the operator can see it.
			log.Info("transient manifest RoleBinding has no job label; leaving it alone",
				"namespace", rb.Namespace, "name", rb.Name)
			continue
		}

		var job batchv1.Job
		err := r.Get(ctx, client.ObjectKey{Namespace: r.OperatorNamespace, Name: jobName}, &job)
		switch {
		case apierrors.IsNotFound(err):
			// The Job is gone (TTL-collected or deleted) and the grant outlived it.
		case err != nil:
			log.Error(err, "checking the job behind a transient manifest RoleBinding",
				"namespace", rb.Namespace, "name", rb.Name, "job", jobName)
			continue
		case job.Status.Succeeded == 0 && job.Status.Failed == 0:
			// Still running: this grant is doing its job.
			continue
		}

		// Through reapAndReport like every other delete in this file. A RoleBinding is the LEAST
		// likely object here to carry a finalizer — but the grant it holds is a live privilege, so
		// "the reaper said it removed the grant" is the single worst sentence in this codebase to be
		// wrong about, and it is now said only after a read-back confirms the object is gone.
		r.reapAndReport(ctx, kindRoleBinding, rb, tally,
			"an orphaned manifest-mover RoleBinding whose nominal teardown did not run",
			nil,
			func() error { return r.Delete(ctx, rb) },
			"job", jobName)
	}
}

// reapClusterManifestBindings is reapManifestBindings for the cluster-scoped ClusterRoleBindings
// a cluster-manifests capture creates. The logic is identical — spare a live grant, refuse to
// guess without a Job label, ignore another operator's — but the objects are cluster-scoped, so
// they carry no namespace and are listed as ClusterRoleBindings rather than RoleBindings. The
// grant they hold is a bounded (enumerated) read of the whole cluster, so the same MinAge that
// treats a leaked namespaced read as a security cost applies here.
func (r *OrphanReaper) reapClusterManifestBindings(ctx context.Context, tally *reapTally) {
	log := logf.FromContext(ctx).WithName("orphan-reaper")

	var bindings rbacv1.ClusterRoleBindingList
	if err := r.List(ctx, &bindings,
		client.MatchingLabels{
			apiconst.LabelManagedBy:  apiconst.ManagedByValue,
			apiconst.LabelMoverRole:  apiconst.MoverRoleManifest,
			apiconst.LabelOperatorNS: r.OperatorNamespace,
		}); err != nil {
		log.Error(err, "listing transient cluster-manifest ClusterRoleBindings")
		return
	}

	cutoff := time.Now().Add(-manifestBindingMinAge)
	for i := range bindings.Items {
		crb := &bindings.Items[i]
		if crb.CreationTimestamp.After(cutoff) {
			continue
		}
		jobName := crb.Labels[apiconst.LabelMoverJob]
		if jobName == "" {
			log.Info("transient cluster-manifest ClusterRoleBinding has no job label; leaving it alone",
				"name", crb.Name)
			continue
		}

		var job batchv1.Job
		err := r.Get(ctx, client.ObjectKey{Namespace: r.OperatorNamespace, Name: jobName}, &job)
		switch {
		case apierrors.IsNotFound(err):
			// The Job is gone and the grant outlived it.
		case err != nil:
			log.Error(err, "checking the job behind a transient cluster-manifest ClusterRoleBinding",
				"name", crb.Name, "job", jobName)
			continue
		case job.Status.Succeeded == 0 && job.Status.Failed == 0:
			continue // still running
		}

		r.reapAndReport(ctx, kindClusterRoleBinding, crb, tally,
			"an orphaned cluster-manifest ClusterRoleBinding whose nominal teardown did not run",
			nil,
			func() error { return r.Delete(ctx, crb) },
			"job", jobName)
	}
}

// reapRestorePV handles one labeled restore PV. A twin is deleted like any residue (the PV
// object only — Retain). An unfinished transplant is handed back to the provisioner by
// restoring reclaimPolicy Delete; the PV controller then reclaims the released volume and
// removes the object, so storage is freed exactly once and never by a bare object delete.
func (r *OrphanReaper) reapRestorePV(ctx context.Context, pv *corev1.PersistentVolume, cutoff time.Time, tally *reapTally) {
	log := logf.FromContext(ctx).WithName("orphan-reaper")
	orphaned, err := r.orphaned(ctx, pv, cutoff)
	if err != nil {
		log.Error(err, "Orphan reaper: restore PV orphan check failed", "pv", pv.Name)
		return
	}
	if !orphaned {
		return
	}
	switch pv.Labels[apiconst.LabelPVRole] {
	case apiconst.PVRoleTwin:
		// A PV is the other kind that routinely sits on a finalizer it did not choose
		// (kubernetes.io/pv-protection, held while a claim is still bound), so this delete gets the
		// same read-back as the snapshot pair rather than a bare "reaped" line.
		r.reapAndReport(ctx, kindPV, pv, tally, "leftover twin PV",
			nil, // a twin PV is an alias: reclaimPolicy is Retain by construction, nothing to restore.
			func() error { return r.Delete(ctx, pv) })
	case apiconst.PVRoleTransplant:
		// Defensive delivered-check (mirror of rexposer.Cleanup's): a final claim BOUND to
		// this PV outside the operator namespace means the handover in fact succeeded and
		// only the label-strip/policy-restore tail was missed (a crash between two writes).
		// Finish that tail — NEVER reclaim delivered user data.
		if ref := pv.Spec.ClaimRef; ref != nil && ref.Namespace != r.OperatorNamespace {
			var final corev1.PersistentVolumeClaim
			if err := r.Get(ctx, client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, &final); err == nil &&
				final.Spec.VolumeName == pv.Name {
				base := pv.DeepCopy()
				pv.Spec.PersistentVolumeReclaimPolicy = r.classReclaimPolicy(ctx, pv.Spec.StorageClassName)
				for k := range pv.Labels {
					if strings.HasPrefix(k, apiconst.Domain+"/") {
						delete(pv.Labels, k)
					}
				}
				if err := r.Patch(ctx, pv, client.MergeFrom(base)); err != nil {
					log.Error(err, "Orphan reaper: delivered-transplant handover tail failed", "pv", pv.Name)
					return
				}
				log.Info("Orphan reaper: finished a delivered transplant's handover tail", "pv", pv.Name)
				return
			}
		}
		if pv.Spec.PersistentVolumeReclaimPolicy == corev1.PersistentVolumeReclaimDelete {
			return // already handed back; the PV controller owns it from here.
		}
		base := pv.DeepCopy()
		pv.Spec.PersistentVolumeReclaimPolicy = corev1.PersistentVolumeReclaimDelete
		if err := r.Patch(ctx, pv, client.MergeFrom(base)); err != nil {
			log.Error(err, "Orphan reaper: transplant PV reclaim patch failed", "pv", pv.Name)
			return
		}
		log.Info("Orphan reaper: reclaimed unfinished transplant PV", "pv", pv.Name)
	}
}

// orphaned reports whether one exposure object should be reaped: it must be older than the
// cutoff, carry a per-PVC label (so it is a per-PVC exposure object, not a repository-init
// Job), and its OWNER must be gone or done with it. The owner is resolved by the object's
// own labels: a restore label (crystalbackup.io/restore or /cluster-restore) resolves a
// Restore/ClusterRestore whose terminal phase means its teardown should already have run;
// otherwise the owning Backup is resolved by NAME (ownerBackupNameFromLabels) within the labelled
// namespace, with the per-PVC volume-phase check. An owner that is being deleted is left to its
// finalizer; live work is never reaped.
//
// The name lookup used to read crystalbackup.io/cluster-backup directly, which made this backstop
// blind to the entire NAMESPACE plane: a Backup with no ClusterBackup parent stamps no run, the
// value arrived here as "", and the short-circuit below refused the object as unresolvable — one
// branch above the IsNotFound verdict that was precisely the answer it needed. That is how the
// 0.6.5 campaign's leaked VolumeSnapshotContent survived three hours of ten-minute sweeps with its
// owning Backup (and its whole namespace) long gone. The run label is now one entry in a fallback
// chain rather than the identity, and the chain's last resort — an object carrying no owner name at
// all, which is what an earlier version left on the namespace plane — goes through
// unattributedExposureOrphaned rather than being abandoned.
//
// What did NOT change is the refusal itself: the goal was to resolve the namespace plane, not to
// make the reaper braver. An object with no namespace to resolve an owner IN is still left alone.
func (r *OrphanReaper) orphaned(ctx context.Context, obj client.Object, cutoff time.Time) (bool, error) {
	labels := obj.GetLabels()
	if labels[apiconst.LabelPVC] == "" {
		return false, nil // not a per-PVC exposure object (e.g. a repository-init Job).
	}
	if obj.GetCreationTimestamp().After(cutoff) {
		return false, nil // too young — a live reconcile may still be settling its status.
	}

	switch {
	case labels[apiconst.LabelRestore] != "":
		return r.restoreOrphaned(ctx, labels[apiconst.LabelNamespace], labels[apiconst.LabelRestore])
	case labels[apiconst.LabelClusterRestore] != "":
		return r.clusterRestoreOrphaned(ctx, labels[apiconst.LabelClusterRestore])
	}

	ns := labels[apiconst.LabelNamespace]
	if ns == "" {
		return false, nil // no namespace to resolve an owner in: no resolvable owner shape.
	}
	name := ownerBackupNameFromLabels(labels)
	if name == "" {
		return r.unattributedExposureOrphaned(ctx, ns, labels[apiconst.LabelPVC])
	}
	var backup cbv1.Backup
	if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &backup); err != nil {
		if apierrors.IsNotFound(err) {
			return true, nil // owning Backup is gone: pure orphan.
		}
		return false, err
	}
	if !backup.DeletionTimestamp.IsZero() {
		return false, nil // being deleted — its finalizer owns the teardown.
	}
	for i := range backup.Status.Volumes {
		if backup.Status.Volumes[i].Pvc == labels[apiconst.LabelPVC] {
			return volumePhaseTerminal(backup.Status.Volumes[i].Phase), nil
		}
	}
	return true, nil // the Backup no longer tracks this PVC — its exposure is residue.
}

// unattributedExposureOrphaned is the reap verdict for an exposure object that names no owner: the
// shape an operator OLDER than apiconst.LabelBackup left on the namespace plane, where the run label
// was stamped empty on created objects and (through exposer.mergeLabels' skip-if-equal rule) not
// stamped at all on the patched origin content. There is no name to GET, and refusing on that basis
// would make an operator upgrade permanently strand every piece of residue the previous version had
// already leaked — including the object this release exists to collect.
//
// So the owner is resolved BY EXCLUSION instead: list the Backups in the labelled namespace and ask
// whether any of them could still want an exposure of this PVC. None ⇒ nobody owns it ⇒ reap. This
// is weaker than a name lookup and it is treated as such:
//
//   - an unreadable Backup list refuses the reap. An owner set we could not read is not an empty
//     one, and this is a DELETE decision — the same rule as the residue read's "unreadable is not
//     clean", with more at stake;
//   - a Backup with no volume status yet counts as a possible owner (it may be about to expose this
//     PVC), so the MinAge race guard the caller already applied is not quietly undone here;
//   - an empty PVC label cannot be excluded against and is refused. orphaned() rejects those before
//     we get here; this is the second lock on the same door, because the whole value of this path is
//     that it cannot be talked into deleting a live run's snapshot.
//
// It is scoped to objects with NO owner name, so it applies only to pre-upgrade residue and retires
// itself as that residue is collected. Everything this version stamps takes the exact name lookup.
func (r *OrphanReaper) unattributedExposureOrphaned(ctx context.Context, ns, pvc string) (bool, error) {
	if pvc == "" {
		return false, nil
	}
	var backups cbv1.BackupList
	if err := r.List(ctx, &backups, client.InNamespace(ns)); err != nil {
		return false, fmt.Errorf("list Backups in %s to resolve an unattributed exposure object: %w", ns, err)
	}
	for i := range backups.Items {
		if backupMayStillNeedExposure(&backups.Items[i], pvc) {
			return false, nil
		}
	}
	return true, nil
}

// backupMayStillNeedExposure reports whether b could still be the owner of a live exposure of pvc —
// the pessimistic half of unattributedExposureOrphaned's by-exclusion resolution. It answers "could
// this Backup still want it?", not "does this Backup own it?": without an owner label on the object,
// the second question has no answer and only the first is safe to act on.
func backupMayStillNeedExposure(b *cbv1.Backup, pvc string) bool {
	for i := range b.Status.Volumes {
		if b.Status.Volumes[i].Pvc != pvc {
			continue
		}
		// A candidate owner. Non-terminal means live work. Terminal but mid-deletion means its
		// finalizer is running the teardown, and two collectors on one object is how a teardown
		// races itself.
		return !volumePhaseTerminal(b.Status.Volumes[i].Phase) || !b.DeletionTimestamp.IsZero()
	}
	// It tracks no volume for this PVC. A Backup that has not reached a terminal phase may simply not
	// have got there yet, so it stays a possible owner; one that has finished without ever recording
	// this PVC is not.
	return !isTerminalBackupPhase(b.Status.Phase)
}

// restoreOrphaned resolves a restore-owned object: reap when the owning Restore is gone or
// terminal (its teardown should already have removed the object); leave live or deleting
// owners to their own machinery.
func (r *OrphanReaper) restoreOrphaned(ctx context.Context, namespace, name string) (bool, error) {
	if namespace == "" {
		return false, nil
	}
	var restore cbv1.Restore
	if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &restore); err != nil {
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	}
	if !restore.DeletionTimestamp.IsZero() {
		return false, nil
	}
	return status.IsTerminalRestorePhase(restore.Status.Phase), nil
}

// clusterRestoreOrphaned is restoreOrphaned's cluster-scoped sibling.
func (r *OrphanReaper) clusterRestoreOrphaned(ctx context.Context, name string) (bool, error) {
	var cr cbv1.ClusterRestore
	if err := r.Get(ctx, client.ObjectKey{Name: name}, &cr); err != nil {
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	}
	if !cr.DeletionTimestamp.IsZero() {
		return false, nil
	}
	return status.IsTerminalRestorePhase(cr.Status.Phase), nil
}

// classReclaimPolicy resolves the reclaim policy a delivered volume should end with: its
// StorageClass's policy, Delete when the class is unset/gone (mirrors
// rexposer.storageClassReclaimPolicy for the reaper's handover-tail path).
func (r *OrphanReaper) classReclaimPolicy(ctx context.Context, scName string) corev1.PersistentVolumeReclaimPolicy {
	if scName == "" {
		return corev1.PersistentVolumeReclaimDelete
	}
	var sc storagev1.StorageClass
	if err := r.Get(ctx, client.ObjectKey{Name: scName}, &sc); err != nil || sc.ReclaimPolicy == nil {
		return corev1.PersistentVolumeReclaimDelete
	}
	return *sc.ReclaimPolicy
}

// volumePhaseTerminal reports whether a per-PVC volume phase is terminal, so its exposure objects
// should already have been torn down.
func volumePhaseTerminal(phase status.VolumePhase) bool {
	switch phase {
	case status.VolumePhaseCompleted, status.VolumePhaseSkipped, status.VolumePhaseFailed:
		return true
	default:
		return false
	}
}
