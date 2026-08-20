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
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
	"github.com/CrystalBackup/CrystalBackup/internal/status"
)

// ---------------------------------------------------------------------------------------------
// The tests for a ClusterBackup run's aggregate counters, reconstructed from the production
// incident they exist to prevent.
//
// A nightly run over 33 namespaces published `namespacesFailed: 32`. Its 33 children read 29
// Completed, 3 PartiallyFailed and 1 Pending. The number was false twice over — 32 is not 3, and it
// called finished namespaces failures — and NO test caught it, for two reasons worth naming because
// they shaped the tests below:
//
//  1. The counters had no unit test at all. Every existing assertion on them went through envtest,
//     over two or three namespaces, in a run whose children were all created by the run itself.
//     The incident needed thirty-three namespaces and children the run had NOT stamped.
//  2. The published numbers ADDED UP. 0 + 32 + 1 is exactly 33. A sum invariant alone would have
//     passed. What was wrong was which bucket the namespaces went into, and the only test that
//     catches that is one which reads the counters back against the child objects — which is what
//     assertCountersMatchChildren below does.
// ---------------------------------------------------------------------------------------------

const (
	incidentRun    = "nightly-20260808-040000"
	incidentRunUID = types.UID("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
)

// incidentRunCreated is the run OBJECT's creationTimestamp — the tick its name encodes. The ledger
// takes it for the record only: how an occupant's own creationTimestamp compares to it is the
// discriminator the backlog's candidate fix turns on, so a run writes it down rather than acting on
// it. Nothing in this file's assertions depends on its value.
var incidentRunCreated = metav1.Date(2026, 8, 8, 4, 0, 0, 0, time.UTC)

// aggregateScheme is the scheme the fake client needs: core (namespaces) plus the CRDs.
func aggregateScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("add client-go scheme: %v", err)
	}
	if err := cbv1.AddToScheme(s); err != nil {
		t.Fatalf("add crystalbackup scheme: %v", err)
	}
	return s
}

// incidentChildPhases is the exact child mix the production run had: 29 Completed, 3
// PartiallyFailed, 1 Pending, in 33 namespaces.
func incidentChildPhases() []string {
	phases := make([]string, 0, 33)
	for range 29 {
		phases = append(phases, string(status.BackupPhaseCompleted))
	}
	for range 3 {
		phases = append(phases, string(status.BackupPhasePartiallyFailed))
	}
	return append(phases, string(status.BackupPhasePending))
}

// childBackup builds one child of the run in namespace ns. stamped controls the parent-UID
// annotation — the single bit that decides whether the run recognizes the child as its own, and the
// bit the production run's children were missing.
func childBackup(ns, phase string, stamped bool) *cbv1.Backup {
	anns := map[string]string{}
	if stamped {
		anns[apiconst.AnnotationParentUID] = string(incidentRunUID)
	}
	return &cbv1.Backup{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:   ns,
			Name:        incidentRun,
			Annotations: anns,
			Labels: map[string]string{
				apiconst.LabelClusterBackup: incidentRun,
				apiconst.LabelOrigin:        apiconst.OriginCluster,
				apiconst.LabelNamespace:     ns,
			},
		},
		Status: cbv1.BackupStatus{
			Phase:   phase,
			Volumes: []cbv1.VolumeStatus{volumeForChild(ns, phase)},
		},
	}
}

// volumeForChild gives each child a volume consistent with its own phase. The still-Pending child
// deliberately holds NO result — no snapshot ID, no bytes — because that is what the production run's
// last child looked like, and it is the one bit that makes an unstamped child adoptable rather than a
// foreign occupant of the coordinate. Handing it a snapshot ID would quietly change the scenario.
func volumeForChild(ns, phase string) cbv1.VolumeStatus {
	v := cbv1.VolumeStatus{Pvc: "data", Phase: volumePhaseForChild(phase)}
	if phase == string(status.BackupPhasePending) {
		return v
	}
	v.SnapshotID = "snap-" + ns
	v.AddedBytes = 1024
	return v
}

// volumePhaseForChild gives each child a volume consistent with its own phase, so the PVC counters
// are checkable too rather than being fed a shape no real backup produces.
func volumePhaseForChild(phase string) status.VolumePhase {
	switch phase {
	case string(status.BackupPhaseCompleted):
		return status.VolumePhaseCompleted
	case string(status.BackupPhasePartiallyFailed):
		return status.VolumePhaseFailed
	default:
		return status.VolumePhasePending
	}
}

// runTheIncident builds the run and its 33 children over a fake client, reconciles once, and
// returns the written status alongside the children as they stand.
func runTheIncident(t *testing.T, stamped bool) (cbv1.ClusterBackupStatus, []cbv1.Backup) {
	t.Helper()
	ctx := context.Background()

	// Sized for the location, the run and two objects per child (its Namespace and the Backup),
	// which is exactly what the loop below appends.
	objs := make([]crclient.Object, 0, 2+2*len(incidentChildPhases()))
	objs = append(objs,
		&cbv1.ClusterBackupLocation{ObjectMeta: metav1.ObjectMeta{Name: "loc"}},
		&cbv1.ClusterBackup{
			ObjectMeta: metav1.ObjectMeta{Name: incidentRun, UID: incidentRunUID},
			Spec: cbv1.ClusterBackupSpec{
				ClusterBackupRunSpec: cbv1.ClusterBackupRunSpec{
					LocationRef: cbv1.LocalObjectReference{Name: "loc"},
					Namespaces:  cbv1.NamespaceSelector{MatchNames: []string{"ns-*"}},
				},
			},
		},
	)
	for i, phase := range incidentChildPhases() {
		ns := fmt.Sprintf("ns-%02d", i)
		objs = append(objs, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		objs = append(objs, childBackup(ns, phase, stamped))
	}

	c := fake.NewClientBuilder().
		WithScheme(aggregateScheme(t)).
		WithObjects(objs...).
		WithStatusSubresource(&cbv1.ClusterBackup{}, &cbv1.Backup{}).
		Build()

	r := &ClusterBackupReconciler{Client: c, Scheme: aggregateScheme(t), Recorder: noopRecorder{}}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: incidentRun}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var run cbv1.ClusterBackup
	if err := c.Get(ctx, crclient.ObjectKey{Name: incidentRun}, &run); err != nil {
		t.Fatalf("get run: %v", err)
	}
	var kids cbv1.BackupList
	if err := c.List(ctx, &kids, crclient.MatchingLabels{apiconst.LabelClusterBackup: incidentRun}); err != nil {
		t.Fatalf("list children: %v", err)
	}
	return run.Status, kids.Items
}

// assertCountersMatchChildren is the assertion the incident was missing. It re-derives the expected
// buckets from the CHILDREN THEMSELVES and compares, then checks the total. Both halves are needed:
// the incident's numbers summed correctly and still described the wrong children.
func assertCountersMatchChildren(t *testing.T, st cbv1.ClusterBackupStatus, children []cbv1.Backup) {
	t.Helper()

	var wantSucceeded, wantFailed, wantInFlight int32
	for i := range children {
		switch status.OutcomeForBackupPhase(children[i].Status.Phase) {
		case status.NamespaceSucceeded:
			wantSucceeded++
		case status.NamespaceFailed:
			wantFailed++
		default:
			wantInFlight++
		}
	}
	if st.NamespacesSucceeded != wantSucceeded {
		t.Errorf("namespacesSucceeded = %d, but %d children report a succeeded phase",
			st.NamespacesSucceeded, wantSucceeded)
	}
	if st.NamespacesFailed != wantFailed {
		t.Errorf("namespacesFailed = %d, but %d children report a failed phase",
			st.NamespacesFailed, wantFailed)
	}

	// The sum invariant: every namespace the run is answerable for lands in exactly one bucket.
	// namespacesMatched is the selector's answer and is the denominator here because every child in
	// this fixture sits in a matched namespace.
	counted := st.NamespacesSucceeded + st.NamespacesFailed + st.NamespacesBlocked + wantInFlight
	if counted != st.NamespacesMatched {
		t.Errorf("counters do not add up: succeeded %d + failed %d + blocked %d + inFlight %d = %d, matched %d",
			st.NamespacesSucceeded, st.NamespacesFailed, st.NamespacesBlocked, wantInFlight,
			counted, st.NamespacesMatched)
	}
	if int(counted) != len(children) {
		t.Errorf("counters sum to %d but the run has %d children", counted, len(children))
	}
}

// TestAggregateCountersMatchTheChildrenTheyCount is the incident, with the run's own stamp in place:
// 33 children reading 29 Completed / 3 PartiallyFailed / 1 Pending must produce 29 / 3 / 1, and the
// buckets must add up to the 33 children.
func TestAggregateCountersMatchTheChildrenTheyCount(t *testing.T) {
	st, children := runTheIncident(t, true)

	if len(children) != 33 {
		t.Fatalf("fixture has %d children, want 33", len(children))
	}
	assertCountersMatchChildren(t, st, children)

	if st.NamespacesSucceeded != 29 {
		t.Errorf("namespacesSucceeded = %d, want 29", st.NamespacesSucceeded)
	}
	if st.NamespacesFailed != 3 {
		t.Errorf("namespacesFailed = %d, want 3 — this is the field that published 32", st.NamespacesFailed)
	}
	if st.NamespacesBlocked != 0 {
		t.Errorf("namespacesBlocked = %d, want 0: the run created every one of these children", st.NamespacesBlocked)
	}
	// One child still Pending holds the whole run non-terminal, however many siblings have settled.
	if st.Phase != string(status.ClusterBackupPhaseRunning) {
		t.Errorf("phase = %q, want Running: one child is still Pending", st.Phase)
	}
	// 29 Completed volumes, 3 Failed, 1 Pending — the PVC counters are derived from the same pass.
	if st.PVCsSucceeded != 29 || st.PVCsFailed != 3 {
		t.Errorf("pvcsSucceeded/pvcsFailed = %d/%d, want 29/3", st.PVCsSucceeded, st.PVCsFailed)
	}
	// The failures list is capped, but it must only ever hold namespaces that really failed.
	for _, f := range st.Failures {
		if f.Namespace == "" {
			t.Errorf("failure record with no namespace: %+v", f)
		}
	}
	if len(st.Failures) != 3 {
		t.Errorf("failures = %d records, want 3 (one per failed namespace)", len(st.Failures))
	}
}

// TestUnstampedTerminalChildrenAreBlockedNotFailed is the production trigger, reproduced exactly.
//
// The run's 33 children carried no parent-UID stamp — they had been fanned out by a build from
// before the stamp existed, and the run was still in flight when the new operator took over. The
// adoption window in classifyCoordinate admits only a child holding no result of any kind, so the 32
// TERMINAL children were each declared a foreign occupant of the coordinate, and every one of them
// incremented namespacesFailed: the 32 that produced the incident.
//
// The misclassification is upstream of the counters and is deliberately NOT changed here — the run
// genuinely cannot tell an unstamped terminal child of its own from a previous same-named run's
// record, and the guard that refuses to guess is the one that stopped a run reporting success for
// data it never wrote. What this test pins is that the counters no longer LIE about it: the 32
// namespaces are reported as never backed up, not as backups that failed, and namespacesFailed no
// longer contradicts the 29 children reading Completed beside it.
func TestUnstampedTerminalChildrenAreBlockedNotFailed(t *testing.T) {
	st, children := runTheIncident(t, false)

	if st.NamespacesFailed != 0 {
		t.Errorf("namespacesFailed = %d, want 0: not one child of THIS run failed — 32 namespaces "+
			"were never backed up by it, which is a different fact", st.NamespacesFailed)
	}
	if st.NamespacesBlocked != 32 {
		t.Errorf("namespacesBlocked = %d, want 32", st.NamespacesBlocked)
	}
	// The 33rd child held no result, so it was adoptable: stamped and counted as in flight.
	if st.NamespacesSucceeded != 0 {
		t.Errorf("namespacesSucceeded = %d, want 0: no child of this run has succeeded yet", st.NamespacesSucceeded)
	}
	if st.PVCsSucceeded != 0 || st.AddedBytes != 0 {
		t.Errorf("pvcsSucceeded/addedBytes = %d/%d, want 0/0: the occupants' volumes are not this run's work",
			st.PVCsSucceeded, st.AddedBytes)
	}
	// The invariant still holds, over the same 33 namespaces.
	counted := st.NamespacesSucceeded + st.NamespacesFailed + st.NamespacesBlocked
	if counted+1 != st.NamespacesMatched { // +1: the adopted child, still in flight
		t.Errorf("counters do not add up: %d counted + 1 in flight != %d matched", counted, st.NamespacesMatched)
	}
	if len(children) != 33 {
		t.Errorf("children = %d, want 33", len(children))
	}
	// And the run must not be able to read Completed over 32 unprotected namespaces.
	if st.Phase == string(status.ClusterBackupPhaseCompleted) {
		t.Errorf("phase = Completed over %d namespaces this run never backed up", st.NamespacesBlocked)
	}
}

// ---------------------------------------------------------------------------------------------
// buildRunLedger directly. It is a pure function, so the cases that need an awkward cluster state
// are cheap here.
// ---------------------------------------------------------------------------------------------

// ledgerChild is a terse child builder for the ledger cases.
func ledgerChild(ns, name, phase string, stamped bool, anns map[string]string) cbv1.Backup {
	all := map[string]string{}
	for k, v := range anns {
		all[k] = v
	}
	if stamped {
		all[apiconst.AnnotationParentUID] = string(incidentRunUID)
	}
	return cbv1.Backup{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Annotations: all},
		Status: cbv1.BackupStatus{
			Phase:   phase,
			Volumes: []cbv1.VolumeStatus{{Pvc: "data", Phase: volumePhaseForChild(phase), AddedBytes: 512}},
		},
	}
}

// TestLedgerCountsEveryNamespaceExactlyOnce sweeps the four ways a namespace can enter the ledger and
// checks the total against the namespaces the run is answerable for. It is the invariant test at the
// controller boundary, where the collision map — the input that used to increment a counter of its
// own — is in play.
func TestLedgerCountsEveryNamespaceExactlyOnce(t *testing.T) {
	matched := []string{"ns-ok", "ns-bad", "ns-collided", "ns-projected", "ns-unseen"}
	children := []cbv1.Backup{
		ledgerChild("ns-ok", incidentRun, string(status.BackupPhaseCompleted), true, nil),
		ledgerChild("ns-bad", incidentRun, string(status.BackupPhaseFailed), true, nil),
		ledgerChild("ns-projected", incidentRun, string(status.BackupPhaseCompleted), false,
			map[string]string{apiconst.AnnotationProjected: apiconst.AnnotationProjectedValue}),
	}
	collided := map[string]blockedCoordinate{"ns-collided": {
		reason: coordinateCodeUnstampedTerminal,
		err: runNameCollisionError{
			Namespace: "ns-collided", Name: incidentRun,
			Detail: "occupied by a Backup this run did not create",
		},
	}}

	l := buildRunLedger(incidentRun, incidentRunUID, incidentRunCreated, matched, children, collided)
	tally := status.TallyNamespaceOutcomes(l.outcomes())

	if !tally.SumsUp() {
		t.Fatalf("ledger does not add up: %+v (counted %d)", tally, tally.Counted())
	}
	if int(tally.Namespaces) != len(matched) {
		t.Fatalf("ledger accounts for %d namespaces, want %d", tally.Namespaces, len(matched))
	}
	if tally.Succeeded != 1 || tally.Failed != 1 || tally.Blocked != 2 || tally.InFlight != 1 {
		t.Errorf("got %+v; want succeeded 1, failed 1, blocked 2 (collided + projected), inFlight 1", tally)
	}
	// A projection's volumes read Completed by construction and were written by nobody here.
	if l.pvcsSucceeded != 1 {
		t.Errorf("pvcsSucceeded = %d, want 1: only ns-ok's volume is this run's work", l.pvcsSucceeded)
	}
	// Every blocked and failed namespace carries its explanation with it.
	var explained int
	for _, v := range l.verdicts {
		switch v.outcome {
		case status.NamespaceFailed, status.NamespaceBlocked:
			if v.failure == nil {
				t.Errorf("namespace %s counted as %v with no failure record", v.namespace, v.outcome)
				continue
			}
			if v.failure.Namespace != v.namespace {
				t.Errorf("namespace %s carries a failure record for %s", v.namespace, v.failure.Namespace)
			}
			explained++
		case status.NamespaceSucceeded, status.NamespaceInFlight:
			if v.failure != nil {
				t.Errorf("namespace %s counted as %v yet carries a failure record", v.namespace, v.outcome)
			}
		}
	}
	if explained != 3 {
		t.Errorf("%d explained verdicts, want 3", explained)
	}
}

// TestLedgerKeepsItsOwnChildInADeselectedNamespace: what a run DID is not undone by the selector
// changing its mind afterwards. A namespace dropped from the selection mid-run used to fall out of
// the ledger entirely — its success and its bytes vanished from the totals, and a child still
// uploading there could no longer hold the run non-terminal.
func TestLedgerKeepsItsOwnChildInADeselectedNamespace(t *testing.T) {
	children := []cbv1.Backup{
		ledgerChild("ns-still-matched", incidentRun, string(status.BackupPhaseCompleted), true, nil),
		ledgerChild("ns-deselected", incidentRun, string(status.BackupPhaseUploading), true, nil),
	}

	l := buildRunLedger(incidentRun, incidentRunUID, incidentRunCreated, []string{"ns-still-matched"}, children, nil)
	tally := status.TallyNamespaceOutcomes(l.outcomes())

	if tally.Namespaces != 2 {
		t.Fatalf("ledger accounts for %d namespaces, want 2 (the matched one and the run's own stray)", tally.Namespaces)
	}
	if tally.InFlight != 1 {
		t.Errorf("got %+v, want the deselected child counted as in flight", tally)
	}
	if got := status.RollUpNamespaceOutcomes(l.outcomes()); got != status.ClusterBackupPhaseRunning {
		t.Errorf("phase %q, want Running: this run's own child is still uploading", got)
	}
}

// TestLedgerIgnoresAnUnstampedStranger is the other side of that: only the UID stamp admits a child
// from an unmatched namespace. An unmatched namespace gets no collision check on this pass, so an
// unstamped Backup there is an object of unknown provenance and must stay out of the accounting
// rather than being guessed at in either direction.
func TestLedgerIgnoresAnUnstampedStranger(t *testing.T) {
	children := []cbv1.Backup{
		ledgerChild("ns-matched", incidentRun, string(status.BackupPhaseCompleted), true, nil),
		ledgerChild("ns-stranger", incidentRun, string(status.BackupPhaseCompleted), false, nil),
	}

	l := buildRunLedger(incidentRun, incidentRunUID, incidentRunCreated, []string{"ns-matched"}, children, nil)
	tally := status.TallyNamespaceOutcomes(l.outcomes())

	if tally.Namespaces != 1 || tally.Succeeded != 1 {
		t.Errorf("got %+v, want exactly the one matched namespace counted", tally)
	}
	if l.addedBytes != 512 {
		t.Errorf("addedBytes = %d, want 512: the stranger's bytes are not this run's", l.addedBytes)
	}
}

// TestLedgerIgnoresABackupAtAnotherCoordinate: the parent→child link is a LABEL, and a label is not
// a coordinate. A Backup in a matched namespace carrying the run's cluster-backup label under a
// different NAME is not the run's child, and must not be able to stand in for it — which keying a
// map by namespace alone allowed, last write winning.
func TestLedgerIgnoresABackupAtAnotherCoordinate(t *testing.T) {
	children := []cbv1.Backup{
		ledgerChild("ns-a", "some-other-backup", string(status.BackupPhaseCompleted), true, nil),
	}

	l := buildRunLedger(incidentRun, incidentRunUID, incidentRunCreated, []string{"ns-a"}, children, nil)
	tally := status.TallyNamespaceOutcomes(l.outcomes())

	if tally.InFlight != 1 || tally.Succeeded != 0 {
		t.Errorf("got %+v, want the namespace in flight: the run's own child is not there yet", tally)
	}
	if l.addedBytes != 0 {
		t.Errorf("addedBytes = %d, want 0", l.addedBytes)
	}
}
