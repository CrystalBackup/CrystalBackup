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
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/events"
	clocktesting "k8s.io/utils/clock/testing"
	ctrl "sigs.k8s.io/controller-runtime"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/mover"
)

// ---------------------------------------------------------------------------
// THE ERRORED-PASS CLASS, SWEPT ON BOTH EXTERNAL-SYNC PLANES.
//
// This instance was LATENT, and that is the whole reason the fix has the shape it has.
//
// commitSyncStatus (formerly a per-plane method named `write`, documented as "persists status once per
// reconcile") skipped the write entirely whenever err != nil. Today the only non-nil errors that reach
// it are the two apiserver reads inside each plane's endpoint(), both structurally upstream of any
// mutation of the syncStatusView — verified, and the specs below pin that verification rather than
// trusting it. drive, finish and settle never return a non-nil error: every accounting failure goes
// through syncDriver.partial, which records the cause on the view and returns nil.
//
// So nothing was being lost. What was wrong is that the function is a SINGLE FUNNEL for every exit of
// Reconcile, and the view it guards ALIASES cs.Status — Phase, LastSuccessTime, SnapshotsCopied,
// BytesCopied, LagSnapshots, Conditions. Its error branch is therefore downstream-capable of
// everything either plane will ever do, and the day an error return appears below applySyncProgress a
// completed copy's counters vanish on that pass.
//
// The fix is to compare before writing: an errored pass persists only when the pass actually changed
// something. That keeps restore_controller.go's rejection of an unconditional persist-then-propagate
// TRUE rather than overriding it (today's two paths change nothing, so they still cost no write),
// while making the funnel safe for a mutation nobody has written yet. These specs are in four layers,
// matching the four things that can rot, and every one of them was verified by a mutation that turns it
// red — including two that were only added BECAUSE the mutation campaign found the first draft blind to
// them (see layer 4, and the second pass in the happy-path spec):
//
//  1. THE NO-WRITE HALF, end to end on both planes. If it goes red, the change has quietly become
//     unconditional persist-then-propagate and restore's argument no longer holds.
//  2. THE LastTransitionTime PIN. status.SetCondition hands meta.SetStatusCondition a zero
//     LastTransitionTime, so re-asserting an identical condition does NOT move the timestamp — checked
//     in apimachinery's source, not taken from its doc comment. If that ever changed, a semantic
//     comparison would differ on every errored pass and (1) would be a no-op assertion passing for the
//     wrong reason. This layer makes that failure loud instead.
//  3. THE WRITE HALF, which has NO live trigger and is therefore constructed deliberately — see the
//     comment on the mutated-status specs for why it is a unit-level pin of the contract rather than
//     an end-to-end path, and why that is the honest way to write it.
//  4. THE ALIASED SNAPSHOT, which is the one-plane-only regression. The snapshot itself cannot be
//     shared between the planes (two different Go types, and only a DeepCopy will do), so it is the one
//     line that could still be written correctly on one plane and wrongly on the other. It is caught in
//     commitSyncStatus by pointer identity rather than by convention, and pinned here.
// ---------------------------------------------------------------------------

const (
	syncErpClusterName = "erp-offsite"
	syncErpNSName      = "erp-offsite"
	syncErpNamespace   = "erp-team"
	syncErpSourceLoc   = "erp-primary"
	syncErpDestLoc     = "erp-cold"
)

// syncErpWriteCounter counts status-subresource writes and is the instrument the no-write specs turn
// on. Counting is the only assertion that distinguishes "persisted nothing because nothing changed"
// from "persisted the same bytes back"; reading the stored object cannot tell those apart.
type syncErpWriteCounter struct {
	writes int
	// lastPhase is the phase each write carried, so a spec can say WHICH write it means.
	lastPhase string
	// updateErr, when set, is returned instead of performing the write.
	updateErr error
	// refuseLocations makes location reads answer Forbidden — see funcs for why this lives on the
	// counter rather than in a second interceptor.Funcs.
	refuseLocations bool
}

// funcs returns ONE interceptor.Funcs carrying every behaviour a spec in this file needs.
//
// It is one struct rather than several composed ones because fake.ClientBuilder.WithInterceptorFuncs
// OVERWRITES rather than chains: `b.WithInterceptorFuncs(a).WithInterceptorFuncs(b)` keeps only b, with
// no error and no warning. The first draft of this file did exactly that, and the write counter was the
// one silently discarded — so every no-write assertion here passed while counting nothing at all. The
// mutation campaign is what surfaced it: a mutation that should have tripped the no-write specs tripped
// only the LastTransitionTime pin. Hence one struct, and hence the write counter owning the location
// refusal it has no natural business owning.
func (w *syncErpWriteCounter) funcs() interceptor.Funcs {
	return interceptor.Funcs{
		Get: func(ctx context.Context, cl crclient.WithWatch, key crclient.ObjectKey,
			obj crclient.Object, opts ...crclient.GetOption,
		) error {
			if w.refuseLocations {
				switch obj.(type) {
				case *cbv1.ClusterBackupLocation, *cbv1.BackupLocation:
					// Forbidden, not NotFound: NotFound parks (a deliberate status change), and only a
					// non-NotFound answer is one of the exactly two error returns that reach
					// commitSyncStatus today.
					return apierrors.NewForbidden(
						schema.GroupResource{Group: cbv1.GroupVersion.Group, Resource: "backuplocations"},
						key.Name, errRefusedByTest)
				}
			}
			return cl.Get(ctx, key, obj, opts...)
		},
		SubResourceUpdate: func(ctx context.Context, cl crclient.Client, subResourceName string,
			obj crclient.Object, opts ...crclient.SubResourceUpdateOption,
		) error {
			if subResourceName == "status" {
				w.writes++
				switch o := obj.(type) {
				case *cbv1.ClusterBackupExternalSync:
					w.lastPhase = o.Status.Phase
				case *cbv1.BackupExternalSync:
					w.lastPhase = o.Status.Phase
				}
			}
			if w.updateErr != nil {
				return w.updateErr
			}
			return cl.SubResource(subResourceName).Update(ctx, obj, opts...)
		},
	}
}

// syncErpLastSuccess is a run that already happened. Every fixture below carries it, plus real
// counters, because "the status did not change" is only a meaningful claim about a status that has
// something in it to lose.
func syncErpLastSuccess() metav1.Time {
	return metav1.NewTime(time.Date(2026, 8, 9, 2, 0, 0, 0, time.UTC))
}

// syncErpSettledConditions is the pair of conditions a completed copy leaves behind. LastTransitionTime
// is backdated well before any spec runs, which is what makes layer (2) able to detect a bump.
func syncErpSettledConditions(generation int64) []metav1.Condition {
	stamped := metav1.NewTime(time.Date(2026, 8, 9, 2, 0, 5, 0, time.UTC))
	cond := func(condType string) metav1.Condition {
		return metav1.Condition{
			Type:               condType,
			Status:             metav1.ConditionTrue,
			Reason:             "Synced",
			Message:            "12 snapshot(s) at the destination, 0 still to copy (the whole repository, mode AppendOnly)",
			LastTransitionTime: stamped,
			ObservedGeneration: generation,
		}
	}
	return []metav1.Condition{cond(ConditionReady), cond(conditionSyncComplete)}
}

func syncErpClusterObject() *cbv1.ClusterBackupExternalSync {
	last := syncErpLastSuccess()
	return &cbv1.ClusterBackupExternalSync{
		ObjectMeta: metav1.ObjectMeta{Name: syncErpClusterName, Generation: 3},
		Spec: cbv1.ClusterBackupExternalSyncSpec{
			SourceLocationRef:      cbv1.LocalObjectReference{Name: syncErpSourceLoc},
			DestinationLocationRef: cbv1.LocalObjectReference{Name: syncErpDestLoc},
			Schedule:               "0 4 * * *",
		},
		Status: cbv1.ClusterBackupExternalSyncStatus{
			Phase:           syncPhaseCompleted,
			LastSuccessTime: &last,
			SnapshotsCopied: 12,
			LagSnapshots:    0,
			Conditions:      syncErpSettledConditions(3),
		},
	}
}

func syncErpNamespacedObject() *cbv1.BackupExternalSync {
	last := syncErpLastSuccess()
	return &cbv1.BackupExternalSync{
		ObjectMeta: metav1.ObjectMeta{Namespace: syncErpNamespace, Name: syncErpNSName, Generation: 3},
		Spec: cbv1.BackupExternalSyncSpec{
			SourceLocationRef:      cbv1.LocalObjectReference{Name: syncErpSourceLoc},
			DestinationLocationRef: cbv1.LocalObjectReference{Name: syncErpDestLoc},
			Schedule:               "0 4 * * *",
		},
		Status: cbv1.BackupExternalSyncStatus{
			Phase:           syncPhaseCompleted,
			LastSuccessTime: &last,
			SnapshotsCopied: 12,
			LagSnapshots:    0,
			Conditions:      syncErpSettledConditions(3),
		},
	}
}

// syncErpClusterReconciler builds a cluster-plane reconciler over a fake client whose only interception
// is w's — one Funcs, for the reason documented on (*syncErpWriteCounter).funcs.
func syncErpClusterReconciler(t *testing.T, w *syncErpWriteCounter) (*ClusterBackupExternalSyncReconciler, crclient.Client) {
	t.Helper()
	s := aggregateScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(syncErpClusterObject()).
		WithStatusSubresource(&cbv1.ClusterBackupExternalSync{}).
		WithInterceptorFuncs(w.funcs()).
		Build()
	r := NewClusterBackupExternalSyncReconciler(c, s, nil, nil, nil, suiteOperatorNamespace,
		"mover:test", "sync:test", nil, mover.Placement{},
		clocktesting.NewFakePassiveClock(syncErpLastSuccess().Add(48*time.Hour)),
		events.NewFakeRecorder(64))
	return r, c
}

func syncErpNamespacedReconciler(t *testing.T, w *syncErpWriteCounter) (*BackupExternalSyncReconciler, crclient.Client) {
	t.Helper()
	s := aggregateScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(syncErpNamespacedObject()).
		WithStatusSubresource(&cbv1.BackupExternalSync{}).
		WithInterceptorFuncs(w.funcs()).
		Build()
	r := NewBackupExternalSyncReconciler(c, s, nil, nil, nil, nil, suiteOperatorNamespace,
		"mover:test", "sync:test", nil, mover.Placement{},
		clocktesting.NewFakePassiveClock(syncErpLastSuccess().Add(48*time.Hour)),
		events.NewFakeRecorder(64))
	return r, c
}

func syncErpGetCluster(t *testing.T, c crclient.Client) cbv1.ClusterBackupExternalSync {
	t.Helper()
	var got cbv1.ClusterBackupExternalSync
	if err := c.Get(context.Background(), crclient.ObjectKey{Name: syncErpClusterName}, &got); err != nil {
		t.Fatalf("read the ClusterBackupExternalSync back: %v", err)
	}
	return got
}

func syncErpGetNamespaced(t *testing.T, c crclient.Client) cbv1.BackupExternalSync {
	t.Helper()
	var got cbv1.BackupExternalSync
	key := crclient.ObjectKey{Namespace: syncErpNamespace, Name: syncErpNSName}
	if err := c.Get(context.Background(), key, &got); err != nil {
		t.Fatalf("read the BackupExternalSync back: %v", err)
	}
	return got
}

// syncErpViewOnCluster builds the same aliasing view Reconcile builds, so a spec can mutate status
// exactly the way a real pass does rather than by poking fields.
func syncErpViewOnCluster(cs *cbv1.ClusterBackupExternalSync) syncStatusView {
	return syncStatusView{
		Phase:           &cs.Status.Phase,
		LastSuccessTime: &cs.Status.LastSuccessTime,
		SnapshotsCopied: &cs.Status.SnapshotsCopied,
		BytesCopied:     &cs.Status.BytesCopied,
		LagSnapshots:    &cs.Status.LagSnapshots,
		Conditions:      &cs.Status.Conditions,
		Generation:      cs.Generation,
	}
}

func syncErpViewOnNamespaced(bs *cbv1.BackupExternalSync) syncStatusView {
	return syncStatusView{
		Phase:           &bs.Status.Phase,
		LastSuccessTime: &bs.Status.LastSuccessTime,
		SnapshotsCopied: &bs.Status.SnapshotsCopied,
		BytesCopied:     &bs.Status.BytesCopied,
		LagSnapshots:    &bs.Status.LagSnapshots,
		Conditions:      &bs.Status.Conditions,
		Generation:      bs.Generation,
	}
}

// ---------------------------------------------------------------------------
// LAYER 1 — the no-write half, end to end.
// ---------------------------------------------------------------------------

// TestClusterSyncErroredPassWithNothingNewWritesNothing is the spec that keeps restore's argument true.
//
// The source ClusterBackupLocation answers Forbidden. That is one of the exactly two error returns
// that reach commitSyncStatus today, and it happens before anything touches the syncStatusView — so
// the pass's only news is "the apiserver would not answer", which is not worth a write on the very
// path where writes are already failing. If this spec goes red, the change has become the
// unconditional persist-then-propagate that restore_controller.go argues against, and that comment is
// no longer true.
//
// Mutation that must turn this red: making the comparison in commitSyncStatus always report a
// difference (e.g. `if false` in place of the DeepEqual).
func TestClusterSyncErroredPassWithNothingNewWritesNothing(t *testing.T) {
	ctx := context.Background()
	w := &syncErpWriteCounter{refuseLocations: true}
	r, c := syncErpClusterReconciler(t, w)

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: crclient.ObjectKey{Name: syncErpClusterName}})
	if err == nil {
		t.Fatal("Reconcile returned no error: a location read that answers something other than NotFound " +
			"is not a state the sync can record, and it must reach controller-runtime's backoff")
	}
	if !strings.Contains(err.Error(), "get ClusterBackupLocation") {
		t.Errorf("Reconcile error = %v, want the refused location read named", err)
	}
	if w.writes != 0 {
		t.Errorf("status writes = %d, want 0: this pass computed nothing, so persisting would spend a "+
			"write on the one path where writes are already failing, to store something the next pass "+
			"recomputes for free", w.writes)
	}

	// Belt and braces: the previous run's numbers are still exactly what they were.
	got := syncErpGetCluster(t, c)
	if got.Status.Phase != syncPhaseCompleted || got.Status.SnapshotsCopied != 12 {
		t.Errorf("stored status = phase %q, copied %d; want the previous run's Completed/12 untouched",
			got.Status.Phase, got.Status.SnapshotsCopied)
	}
}

// TestNamespacedSyncErroredPassWithNothingNewWritesNothing is the same spec on the other plane.
//
// It exists separately rather than as a table case because the two planes resolve DIFFERENT objects
// (BackupLocation in the CR's own namespace versus a cluster-scoped ClusterBackupLocation) and a fix
// landed on one plane and not the other is how this project grew a namespace plane its own teardown
// verification could not see.
func TestNamespacedSyncErroredPassWithNothingNewWritesNothing(t *testing.T) {
	ctx := context.Background()
	w := &syncErpWriteCounter{refuseLocations: true}
	r, c := syncErpNamespacedReconciler(t, w)

	_, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: crclient.ObjectKey{Namespace: syncErpNamespace, Name: syncErpNSName},
	})
	if err == nil {
		t.Fatal("Reconcile returned no error over a refused BackupLocation read")
	}
	if !strings.Contains(err.Error(), "get BackupLocation") {
		t.Errorf("Reconcile error = %v, want the refused location read named", err)
	}
	if w.writes != 0 {
		t.Errorf("status writes = %d, want 0 on a pass that computed nothing", w.writes)
	}
	got := syncErpGetNamespaced(t, c)
	if got.Status.Phase != syncPhaseCompleted || got.Status.SnapshotsCopied != 12 {
		t.Errorf("stored status = phase %q, copied %d; want the previous run's Completed/12 untouched",
			got.Status.Phase, got.Status.SnapshotsCopied)
	}
}

// ---------------------------------------------------------------------------
// LAYER 2 — the LastTransitionTime pin.
// ---------------------------------------------------------------------------

// TestSyncErroredPassReassertingTheSameConditionIsNotAChange is the spec that stops the fix from being
// unconditional persist-then-propagate wearing a comparison.
//
// The comparison is only worth anything if a pass that RE-ASSERTS the status it already has compares
// equal. That rests on one fact about a dependency: status.SetCondition hands meta.SetStatusCondition
// a zero LastTransitionTime, and apimachinery stamps a fresh one ONLY when the condition's Status
// actually transitions — not on every call, and not merely when Reason or Message change. Verified in
// k8s.io/apimachinery/pkg/api/meta/conditions.go, not taken from the wrapper's doc comment.
//
// Were that ever to change (a dependency bump, or somebody hand-building a condition), a semantic
// comparison would differ on every single errored pass, the no-write specs above would go red, and the
// operator would silently be doing the thing restore's comment forbids. This spec is what names the
// cause when that happens, instead of leaving it to be re-derived.
//
// It runs the two passes for real, in order, through the object's own writer: pass one establishes a
// Copying condition; pass two re-asserts the identical one and then errors.
func TestSyncErroredPassReassertingTheSameConditionIsNotAChange(t *testing.T) {
	ctx := context.Background()
	const reason, message = "Copying", "copying the whole repository from erp-primary to erp-cold"

	t.Run("cluster plane", func(t *testing.T) {
		w := &syncErpWriteCounter{}
		r, c := syncErpClusterReconciler(t, w)

		// Pass one: establish the condition. A normal, non-errored write.
		first := syncErpGetCluster(t, c)
		before := first.Status.DeepCopy()
		view := syncErpViewOnCluster(&first)
		*view.Phase = syncPhaseRunning
		setSyncCondition(view, metav1.ConditionFalse, reason, message)
		if _, err := r.persistStatus(ctx, &first, before, ctrl.Result{}, nil); err != nil {
			t.Fatalf("pass one: %v", err)
		}
		if w.writes != 1 {
			t.Fatalf("pass one status writes = %d, want 1", w.writes)
		}
		stamped := readySyncTransition(t, syncErpGetCluster(t, c).Status.Conditions)

		// Pass two: the identical condition, then an error. Nothing changed, so nothing is written.
		second := syncErpGetCluster(t, c)
		before2 := second.Status.DeepCopy()
		view2 := syncErpViewOnCluster(&second)
		*view2.Phase = syncPhaseRunning
		setSyncCondition(view2, metav1.ConditionFalse, reason, message)
		if _, err := r.persistStatus(ctx, &second, before2, ctrl.Result{}, errRefusedByTest); err == nil {
			t.Fatal("persistStatus swallowed the pass's error")
		}
		if w.writes != 1 {
			t.Errorf("status writes = %d after re-asserting the same condition on an errored pass, want "+
				"still 1: if LastTransitionTime moved on a call that changed nothing, every errored pass "+
				"would write and the no-write specs would be asserting nothing", w.writes)
		}
		if again := readySyncTransition(t, syncErpGetCluster(t, c).Status.Conditions); !again.Equal(&stamped) {
			t.Errorf("Ready LastTransitionTime moved from %v to %v on a re-assertion of the same Status: "+
				"apimachinery is documented and observed to stamp it only on a real transition", stamped, again)
		}
	})

	t.Run("namespace plane", func(t *testing.T) {
		w := &syncErpWriteCounter{}
		r, c := syncErpNamespacedReconciler(t, w)

		first := syncErpGetNamespaced(t, c)
		before := first.Status.DeepCopy()
		view := syncErpViewOnNamespaced(&first)
		*view.Phase = syncPhaseRunning
		setSyncCondition(view, metav1.ConditionFalse, reason, message)
		if _, err := r.persistStatus(ctx, &first, before, ctrl.Result{}, nil); err != nil {
			t.Fatalf("pass one: %v", err)
		}
		if w.writes != 1 {
			t.Fatalf("pass one status writes = %d, want 1", w.writes)
		}
		stamped := readySyncTransition(t, syncErpGetNamespaced(t, c).Status.Conditions)

		second := syncErpGetNamespaced(t, c)
		before2 := second.Status.DeepCopy()
		view2 := syncErpViewOnNamespaced(&second)
		*view2.Phase = syncPhaseRunning
		setSyncCondition(view2, metav1.ConditionFalse, reason, message)
		if _, err := r.persistStatus(ctx, &second, before2, ctrl.Result{}, errRefusedByTest); err == nil {
			t.Fatal("persistStatus swallowed the pass's error")
		}
		if w.writes != 1 {
			t.Errorf("status writes = %d after re-asserting the same condition on an errored pass, want still 1", w.writes)
		}
		if again := readySyncTransition(t, syncErpGetNamespaced(t, c).Status.Conditions); !again.Equal(&stamped) {
			t.Errorf("Ready LastTransitionTime moved from %v to %v on a re-assertion of the same Status", stamped, again)
		}
	})
}

// readySyncTransition returns the Ready condition's LastTransitionTime, failing the spec if the
// condition is absent — an absent condition would make the timestamp comparison vacuously pass.
func readySyncTransition(t *testing.T, conds []metav1.Condition) metav1.Time {
	t.Helper()
	for i := range conds {
		if conds[i].Type == ConditionReady {
			return conds[i].LastTransitionTime
		}
	}
	t.Fatal("no Ready condition on the stored status")
	return metav1.Time{}
}

// ---------------------------------------------------------------------------
// LAYER 3 — the write half, which has no live trigger.
// ---------------------------------------------------------------------------

// TestClusterSyncErroredPassPersistsAMutatedStatus is the spec with NO end-to-end path today, and it
// is written deliberately rather than approximated.
//
// There is no seam to inject the error through, and that is a fact about the code and not a gap in the
// harness: drive, finish and settle return a non-nil error on no branch whatsoever — every accounting
// failure goes through syncDriver.partial, which records the cause and returns nil. Faking one would
// mean adding a hook to production code that exists only for this spec, which is a worse trade than
// being explicit. So this is a UNIT-LEVEL PIN OF THE CONTRACT: the mutation is genuine (applySyncProgress,
// the exact call a completed copy makes) and only the error is synthetic.
//
// What it protects is the case the audit could not otherwise reach: the first author to add an error
// return below applySyncProgress will not lose a completed copy's snapshot count and lag to it, and
// will not have to have known a rule to get that right.
//
// Persistence is asserted by READING THE OBJECT BACK through the client rather than by inspecting the
// in-memory CR. The reconciler's own copy carries the mutation whether or not it was ever written; only
// a read-back proves it reached the store.
//
// Mutation that must turn this red: restoring the bare `if err != nil { return ctrl.Result{}, err }`
// as commitSyncStatus's first statement.
func TestClusterSyncErroredPassPersistsAMutatedStatus(t *testing.T) {
	ctx := context.Background()
	w := &syncErpWriteCounter{}
	r, c := syncErpClusterReconciler(t, w)

	cs := syncErpGetCluster(t, c)
	before := cs.Status.DeepCopy()
	view := syncErpViewOnCluster(&cs)

	// A copy that finished and was measured: exactly what settle() records on its success path.
	applySyncProgress(view, syncProgress{Copied: 41, Lag: 2},
		metav1.NewTime(time.Date(2026, 8, 11, 4, 0, 0, 0, time.UTC)),
		"41 snapshot(s) at the destination, 2 still to copy (the whole repository, mode AppendOnly)")
	// BytesCopied has no writer today (`restic copy --json` emits no summary), so it is set here by
	// hand: it is a view field, which makes it as losable as the rest the day something fills it.
	*view.BytesCopied = 1 << 34

	res, err := r.persistStatus(ctx, &cs, before, ctrl.Result{RequeueAfter: time.Minute}, errRefusedByTest)
	if err == nil {
		t.Fatal("persistStatus returned no error: persisting the pass must not swallow the reason it failed, " +
			"or controller-runtime never backs the object off and the cause never reaches a log")
	}
	if !strings.Contains(err.Error(), errRefusedByTest.Error()) {
		t.Errorf("returned error = %v, want the pass's own cause preserved", err)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("RequeueAfter = %s, want zero: an errored pass is backed off exponentially, and honouring "+
			"a requeue computed before the failure would flatten that into a fixed poll", res.RequeueAfter)
	}
	if w.writes != 1 {
		t.Fatalf("status writes = %d, want exactly 1", w.writes)
	}

	got := syncErpGetCluster(t, c)
	if got.Status.SnapshotsCopied != 41 || got.Status.LagSnapshots != 2 {
		t.Errorf("stored copied/lag = %d/%d, want 41/2: a completed copy's accounting was discarded because "+
			"a later step in the same pass errored", got.Status.SnapshotsCopied, got.Status.LagSnapshots)
	}
	if got.Status.BytesCopied != 1<<34 {
		t.Errorf("stored bytesCopied = %d, want %d", got.Status.BytesCopied, int64(1)<<34)
	}
	if got.Status.Phase != syncPhaseCompleted {
		t.Errorf("stored phase = %q, want %q", got.Status.Phase, syncPhaseCompleted)
	}
	if got.Status.LastSuccessTime == nil || got.Status.LastSuccessTime.UTC().Day() != 11 {
		t.Errorf("stored lastSuccessTime = %v, want the copy's own completion instant — the schedule's next "+
			"activation is computed from it, so losing it re-runs a copy that already succeeded",
			got.Status.LastSuccessTime)
	}
}

// TestNamespacedSyncErroredPassPersistsAMutatedStatus is the same unit-level pin on the namespace
// plane. See its cluster-plane twin for why it is unit-level and what it protects.
func TestNamespacedSyncErroredPassPersistsAMutatedStatus(t *testing.T) {
	ctx := context.Background()
	w := &syncErpWriteCounter{}
	r, c := syncErpNamespacedReconciler(t, w)

	bs := syncErpGetNamespaced(t, c)
	before := bs.Status.DeepCopy()
	view := syncErpViewOnNamespaced(&bs)

	applySyncProgress(view, syncProgress{Copied: 41, Lag: 2},
		metav1.NewTime(time.Date(2026, 8, 11, 4, 0, 0, 0, time.UTC)),
		"41 snapshot(s) at the destination, 2 still to copy (the whole repository, mode AppendOnly)")
	*view.BytesCopied = 1 << 34

	res, err := r.persistStatus(ctx, &bs, before, ctrl.Result{RequeueAfter: time.Minute}, errRefusedByTest)
	if err == nil {
		t.Fatal("persistStatus returned no error: the pass's cause must still propagate")
	}
	if res.RequeueAfter != 0 {
		t.Errorf("RequeueAfter = %s, want zero on an errored pass", res.RequeueAfter)
	}
	if w.writes != 1 {
		t.Fatalf("status writes = %d, want exactly 1", w.writes)
	}

	got := syncErpGetNamespaced(t, c)
	if got.Status.SnapshotsCopied != 41 || got.Status.LagSnapshots != 2 {
		t.Errorf("stored copied/lag = %d/%d, want 41/2", got.Status.SnapshotsCopied, got.Status.LagSnapshots)
	}
	if got.Status.BytesCopied != 1<<34 {
		t.Errorf("stored bytesCopied = %d, want %d", got.Status.BytesCopied, int64(1)<<34)
	}
	if got.Status.Phase != syncPhaseCompleted {
		t.Errorf("stored phase = %q, want %q", got.Status.Phase, syncPhaseCompleted)
	}
}

// TestSyncErroredPassReportsBothTheCauseAndAFailedWrite: when the pass errored AND the write it now
// attempts also fails, both reasons travel.
//
// The pass's own error is what an operator acts on; the failed write is why the object does not show
// it. Returning only one of them would leave a silent gap on the exact path where the apiserver is
// already unhappy — which is the whole situation this branch exists for.
//
// Mutation that must turn this red: returning only `err` (or only the wrapped write error) instead of
// joining them.
func TestSyncErroredPassReportsBothTheCauseAndAFailedWrite(t *testing.T) {
	ctx := context.Background()
	w := &syncErpWriteCounter{updateErr: apierrors.NewInternalError(errRefusedByTest)}
	r, c := syncErpClusterReconciler(t, w)

	cs := syncErpGetCluster(t, c)
	before := cs.Status.DeepCopy()
	view := syncErpViewOnCluster(&cs)
	applySyncProgress(view, syncProgress{Copied: 41, Lag: 2},
		metav1.NewTime(time.Date(2026, 8, 11, 4, 0, 0, 0, time.UTC)), "measured")

	_, err := r.persistStatus(ctx, &cs, before, ctrl.Result{}, errRefusedByTest)
	if err == nil {
		t.Fatal("persistStatus returned no error at all")
	}
	if !strings.Contains(err.Error(), errRefusedByTest.Error()) {
		t.Errorf("returned error = %v, want the pass's own cause in it", err)
	}
	if !strings.Contains(err.Error(), "persist status for ClusterBackupExternalSync") {
		t.Errorf("returned error = %v, want the failed write named too: without it the object silently "+
			"disagrees with the log and nothing says why", err)
	}
}

// TestSyncErroredPassWithAnAliasedSnapshotStillPersists is the spec that makes the ONE-PLANE-ONLY
// regression detectable, and it exists because the mutation campaign proved nothing else could see it.
//
// `before := &cs.Status` in place of `before := cs.Status.DeepCopy()` compiles, type-checks and reads
// plausibly, and it disables the entire fix on whichever plane it lands: the view mutates through that
// exact pointer and meta.SetStatusCondition edits conditions in place, so every comparison reports
// "unchanged" and the error branch is the old bug again. A mutation doing precisely that to the
// namespace plane's Reconcile was NOT caught by any other spec here, because neither plane has a route
// that mutates the view and then errors — so there is no end-to-end pass to observe it on. That is the
// same "guards on conventions rot" problem the fix was chosen to avoid, one level up.
//
// So commitSyncStatus checks pointer identity and fails toward PERSISTING, and this spec pins that
// direction. A mis-snapshotted plane then degrades to the unconditional persist-then-propagate that was
// rejected as merely wasteful, instead of to the silent loss that was rejected as wrong.
//
// Mutation that must turn this red: dropping `before != after` from the error branch's condition.
func TestSyncErroredPassWithAnAliasedSnapshotStillPersists(t *testing.T) {
	ctx := context.Background()
	w := &syncErpWriteCounter{}
	r, c := syncErpClusterReconciler(t, w)

	cs := syncErpGetCluster(t, c)
	// The mistake, made on purpose: the live status handed over as its own "before".
	before := &cs.Status
	view := syncErpViewOnCluster(&cs)
	applySyncProgress(view, syncProgress{Copied: 41, Lag: 2},
		metav1.NewTime(time.Date(2026, 8, 11, 4, 0, 0, 0, time.UTC)), "measured")

	if _, err := r.persistStatus(ctx, &cs, before, ctrl.Result{}, errRefusedByTest); err == nil {
		t.Fatal("persistStatus swallowed the pass's error")
	}
	if w.writes != 1 {
		t.Fatalf("status writes = %d, want 1: an aliased snapshot makes the comparison structurally "+
			"incapable of seeing a change, so it must not be trusted to authorise a skip", w.writes)
	}
	if got := syncErpGetCluster(t, c); got.Status.SnapshotsCopied != 41 {
		t.Errorf("stored copied = %d, want 41 persisted despite the useless snapshot", got.Status.SnapshotsCopied)
	}
}

// ---------------------------------------------------------------------------
// The happy path, unchanged.
// ---------------------------------------------------------------------------

// TestSyncHappyPassStillWritesExactlyOnce guards the property the errored-pass fix had to preserve
// rather than introduce: a pass that did not error writes its status, unconditionally and once.
//
// The comparison added to commitSyncStatus governs the ERROR branch only. A successful pass writes
// even when it changed nothing — deliberately, because that is the pass whose result an operator is
// waiting on and its cost is bounded to one write per reconcile. A comparison hoisted to cover both
// branches would be an unrelated change to a different property; the second reconcile in each subtest
// below is what catches it, and the first alone did not: a first pause pass DOES change the phase, so a
// hoisted comparison still writes on it and the mutation slipped through until the no-change pass was
// added.
//
// The pause path is the vehicle because it reaches the writer without resolving an endpoint, so
// nothing else in the world has to exist for the write to be the only thing under test.
func TestSyncHappyPassStillWritesExactlyOnce(t *testing.T) {
	ctx := context.Background()

	t.Run("cluster plane", func(t *testing.T) {
		w := &syncErpWriteCounter{}
		r, c := syncErpClusterReconciler(t, w)
		cs := syncErpGetCluster(t, c)
		cs.Spec.Paused = true
		if err := c.Update(ctx, &cs); err != nil {
			t.Fatalf("pause the sync: %v", err)
		}

		if _, err := r.Reconcile(ctx, ctrl.Request{
			NamespacedName: crclient.ObjectKey{Name: syncErpClusterName},
		}); err != nil {
			t.Fatalf("reconcile a paused sync: %v", err)
		}
		if w.writes != 1 {
			t.Errorf("status writes = %d, want exactly 1 — one pass, one write", w.writes)
		}
		if w.lastPhase != syncPhasePending {
			t.Errorf("the write carried phase %q, want %q", w.lastPhase, syncPhasePending)
		}
		if got := syncErpGetCluster(t, c); got.Status.Phase != syncPhasePending {
			t.Errorf("stored phase = %q, want %q", got.Status.Phase, syncPhasePending)
		}

		// A SECOND pass over the same paused sync: it re-derives the identical status, so it changes
		// nothing, and it must still write. This is the case that distinguishes "the comparison governs
		// the error branch" from "the comparison governs the writer" — the first pass cannot, because a
		// first pause pass does change the phase.
		if _, err := r.Reconcile(ctx, ctrl.Request{
			NamespacedName: crclient.ObjectKey{Name: syncErpClusterName},
		}); err != nil {
			t.Fatalf("second reconcile of a paused sync: %v", err)
		}
		if w.writes != 2 {
			t.Errorf("status writes after two passes = %d, want 2: a successful pass writes even when it "+
				"computed the same answer as last time, and hoisting the errored-pass comparison over "+
				"this branch would silently change that", w.writes)
		}
	})

	t.Run("namespace plane", func(t *testing.T) {
		w := &syncErpWriteCounter{}
		r, c := syncErpNamespacedReconciler(t, w)
		bs := syncErpGetNamespaced(t, c)
		bs.Spec.Paused = true
		if err := c.Update(ctx, &bs); err != nil {
			t.Fatalf("pause the sync: %v", err)
		}

		if _, err := r.Reconcile(ctx, ctrl.Request{
			NamespacedName: crclient.ObjectKey{Namespace: syncErpNamespace, Name: syncErpNSName},
		}); err != nil {
			t.Fatalf("reconcile a paused sync: %v", err)
		}
		if w.writes != 1 {
			t.Errorf("status writes = %d, want exactly 1 — one pass, one write", w.writes)
		}
		if w.lastPhase != syncPhasePending {
			t.Errorf("the write carried phase %q, want %q", w.lastPhase, syncPhasePending)
		}
		if got := syncErpGetNamespaced(t, c); got.Status.Phase != syncPhasePending {
			t.Errorf("stored phase = %q, want %q", got.Status.Phase, syncPhasePending)
		}

		// The no-change second pass; see the cluster-plane subtest for why it is the one that matters.
		if _, err := r.Reconcile(ctx, ctrl.Request{
			NamespacedName: crclient.ObjectKey{Namespace: syncErpNamespace, Name: syncErpNSName},
		}); err != nil {
			t.Fatalf("second reconcile of a paused sync: %v", err)
		}
		if w.writes != 2 {
			t.Errorf("status writes after two passes = %d, want 2", w.writes)
		}
	})
}
