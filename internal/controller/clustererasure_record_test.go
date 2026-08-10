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
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/restic"
)

// ---------------------------------------------------------------------------------------------
// The compliance record of a FAILED erasure.
//
// A ClusterErasure wrote its PRE-erasure scope straight into status.snapshotsForgotten and never
// re-derived it, so an erasure whose forget failed sat in a Failed phase still attesting that it had
// removed N snapshots. On this object that is not a wrong counter — it is a false attestation, since
// the object exists to be pointed at when somebody asserts that a GDPR erasure, a contractual
// deletion or a tenant offboarding was carried out.
//
// These specs drive observeErasureResidue, which is the seam where the record stops being an
// intention and becomes a measurement.
// ---------------------------------------------------------------------------------------------

// residueLister is a FilteredSnapshotLister that answers one canned residue, or one canned error,
// and records the Job names it was asked to list under.
type residueLister struct {
	snaps    []restic.Snapshot
	err      error
	jobNames []string
	tags     [][]string
}

var _ FilteredSnapshotLister = (*residueLister)(nil)

func (l *residueLister) ListFiltered(_ context.Context, _ *cbv1.BackupRepository, jobName string, filterTags ...string) ([]restic.Snapshot, error) {
	l.jobNames = append(l.jobNames, jobName)
	l.tags = append(l.tags, append([]string{}, filterTags...))
	if l.err != nil {
		return nil, l.err
	}
	return l.snaps, nil
}

// snapshotsMatching builds n placeholder snapshots — only the COUNT is load-bearing here.
func snapshotsMatching(n int) []restic.Snapshot {
	out := make([]restic.Snapshot, 0, n)
	for i := range n {
		out = append(out, restic.Snapshot{ID: string(rune('a'+i)) + "-residue"})
	}
	return out
}

// newRecordReconciler builds the minimum ClusterErasureReconciler observeErasureResidue needs: a
// lister and a recorder. No client — the function reads status and lists, and writes nothing.
func newRecordReconciler(l FilteredSnapshotLister) *ClusterErasureReconciler {
	return &ClusterErasureReconciler{Lister: l, Recorder: events.NewFakeRecorder(8)}
}

func erasureWithScope(name string, targeted int32) *cbv1.ClusterErasure {
	return &cbv1.ClusterErasure{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: cbv1.ClusterErasureSpec{
			LocationRef: cbv1.LocalObjectReference{Name: "dr"},
			Target:      cbv1.ErasureTarget{Namespace: "team-x"},
		},
		Status: cbv1.ClusterErasureStatus{
			Phase:              erasurePhaseRunning,
			SnapshotsTargeted:  targeted,
			SnapshotsRemaining: targeted,
		},
	}
}

// TestFailedErasureRecordsWhatActuallyWentPartially is the defect, in its exact shape: ten snapshots
// targeted, the erasure failed, six are still in the repository. The record must say 4 forgotten and
// 6 remaining — the number this object used to publish here was 10.
func TestFailedErasureRecordsWhatActuallyWentPartially(t *testing.T) {
	l := &residueLister{snaps: snapshotsMatching(6)}
	r := newRecordReconciler(l)
	er := erasureWithScope("cer-partial", 10)
	repo := &cbv1.BackupRepository{ObjectMeta: metav1.ObjectMeta{Name: "dr"}}

	rec, note := r.observeErasureResidue(context.Background(), er, repo,
		[]string{restic.Tag(restic.TagKeyNamespace, "team-x")})
	r.stampErasureRecord(er, rec)

	if er.Status.SnapshotsForgotten != 4 {
		t.Fatalf("snapshotsForgotten = %d, want 4: a failed erasure must not attest the scope it "+
			"intended to remove", er.Status.SnapshotsForgotten)
	}
	if er.Status.SnapshotsRemaining != 6 {
		t.Fatalf("snapshotsRemaining = %d, want 6", er.Status.SnapshotsRemaining)
	}
	if er.Status.SnapshotsTargeted != 10 {
		t.Fatalf("snapshotsTargeted = %d, want the measured scope 10", er.Status.SnapshotsTargeted)
	}
	if !rec.SumsUp() {
		t.Fatalf("%+v does not account for the targeted scope", rec)
	}
	// The note reaches the failure condition and the event: a compliance reader has to be told the
	// erasure is partial in words, not only in three integers.
	if note == "" || !containsAll(note, "4", "6", "forgotten") {
		t.Fatalf("note %q must state how many went and how many remain", note)
	}
	// The residue is listed under its OWN Job name. Re-using the count's name would re-adopt the
	// completed pre-erasure listing and read the old repository back as the new one.
	if len(l.jobNames) != 1 || l.jobNames[0] != erasureVerifyJobName("cer-partial") {
		t.Fatalf("residue listed under %v, want %q", l.jobNames, erasureVerifyJobName("cer-partial"))
	}
	// And under the erasure's OWN filter — a wider listing would count another tenant's snapshots as
	// this erasure's residue.
	if len(l.tags) != 1 || len(l.tags[0]) != 1 || l.tags[0][0] != restic.Tag(restic.TagKeyNamespace, "team-x") {
		t.Fatalf("residue listed with tags %v, want the erasure's own filter", l.tags)
	}
}

// TestFailedErasureAfterASuccessfulForget is the other half of a failed erasure and the one that must
// not alarm wrongly: the forget went through and the PRUNE failed, so the snapshots really are gone
// and only their space was not reclaimed. The residue listing finds nothing and the record credits
// the whole scope even though the phase is Failed.
func TestFailedErasureAfterASuccessfulForget(t *testing.T) {
	r := newRecordReconciler(&residueLister{snaps: nil})
	er := erasureWithScope("cer-prune-failed", 7)

	rec, note := r.observeErasureResidue(context.Background(), er,
		&cbv1.BackupRepository{ObjectMeta: metav1.ObjectMeta{Name: "dr"}}, nil)
	r.stampErasureRecord(er, rec)

	if er.Status.SnapshotsForgotten != 7 || er.Status.SnapshotsRemaining != 0 {
		t.Fatalf("forgotten=%d remaining=%d, want 7/0: an empty residue means the forget landed",
			er.Status.SnapshotsForgotten, er.Status.SnapshotsRemaining)
	}
	if !containsAll(note, "7") {
		t.Fatalf("note %q must state the count", note)
	}
}

// TestUnreadableResidueClaimsNoDestruction covers the listing that itself fails. Nothing is
// established, so the record claims nothing and reports the whole scope as possibly still present.
// Leaving remaining at the unmeasured zero would publish "nothing forgotten, nothing left" — an empty
// repository — which is the same overstatement in a different field.
func TestUnreadableResidueClaimsNoDestruction(t *testing.T) {
	r := newRecordReconciler(&residueLister{err: errors.New("snapshots Job deadline exceeded")})
	er := erasureWithScope("cer-unreadable", 10)

	rec, note := r.observeErasureResidue(context.Background(), er,
		&cbv1.BackupRepository{ObjectMeta: metav1.ObjectMeta{Name: "dr"}}, nil)
	r.stampErasureRecord(er, rec)

	if er.Status.SnapshotsForgotten != 0 {
		t.Fatalf("snapshotsForgotten = %d, want 0: an unverifiable outcome establishes no destruction",
			er.Status.SnapshotsForgotten)
	}
	if er.Status.SnapshotsRemaining != 10 {
		t.Fatalf("snapshotsRemaining = %d, want the whole scope 10", er.Status.SnapshotsRemaining)
	}
	if !containsAll(note, "could not be listed", "10") {
		t.Fatalf("note %q must admit that the residue is unverified", note)
	}
}

// TestResidueLargerThanTheScopeClaimsNothing covers the one case whose books legitimately do not
// balance: snapshots matching the target were written between the measurement and the verification
// (a nightly backup of a namespace being offboarded). The record claims no destruction, publishes the
// residue as observed, and the operator says why in an event rather than adjusting a number to make
// the arithmetic work.
func TestResidueLargerThanTheScopeClaimsNothing(t *testing.T) {
	fake := events.NewFakeRecorder(8)
	r := &ClusterErasureReconciler{Lister: &residueLister{snaps: snapshotsMatching(12)}, Recorder: fake}
	er := erasureWithScope("cer-grew", 10)

	rec, note := r.observeErasureResidue(context.Background(), er,
		&cbv1.BackupRepository{ObjectMeta: metav1.ObjectMeta{Name: "dr"}}, nil)
	r.stampErasureRecord(er, rec)

	if er.Status.SnapshotsForgotten != 0 {
		t.Fatalf("snapshotsForgotten = %d, want 0", er.Status.SnapshotsForgotten)
	}
	if er.Status.SnapshotsRemaining != 12 {
		t.Fatalf("snapshotsRemaining = %d, want the observed 12", er.Status.SnapshotsRemaining)
	}
	if rec.SumsUp() {
		t.Fatalf("%+v must not claim to balance against a scope of 10", rec)
	}
	if !containsAll(note, "12", "10") {
		t.Fatalf("note %q must name both the residue and the measured scope", note)
	}
	select {
	case ev := <-fake.Events:
		if !containsAll(ev, "ErasureScopeGrew") {
			t.Fatalf("event %q, want ErasureScopeGrew", ev)
		}
	default:
		t.Fatal("no event: a record that cannot be reconciled has to be announced, not just published")
	}
}

// TestErasureScopeSurvivesAnUpgradeMidErasure covers the one object shape this release inherits: an
// erasure that was already Running when the operator was upgraded carries its measured scope in the
// OLD snapshotsForgotten field, because that is where the pre-erasure count used to be written.
//
// Reading it back matters because it is the record's denominator. Without it, such an erasure would
// fail and report "0 targeted, 0 forgotten, 0 remaining" — an object that says nothing at all about a
// destruction that was in progress.
func TestErasureScopeSurvivesAnUpgradeMidErasure(t *testing.T) {
	// The pre-upgrade shape: phase Running, the scope sitting in snapshotsForgotten, nothing in the
	// two new fields.
	legacy := &cbv1.ClusterErasure{Status: cbv1.ClusterErasureStatus{
		Phase: erasurePhaseRunning, SnapshotsForgotten: 10,
	}}
	if got := erasureTargetedScope(legacy); got != 10 {
		t.Fatalf("erasureTargetedScope on a pre-upgrade object = %d, want the 10 it had measured", got)
	}
	// And it never overrides a scope this release measured — including the honest zero of an erasure
	// whose target matched nothing.
	current := &cbv1.ClusterErasure{Status: cbv1.ClusterErasureStatus{
		SnapshotsTargeted: 4, SnapshotsForgotten: 3, SnapshotsRemaining: 1,
	}}
	if got := erasureTargetedScope(current); got != 4 {
		t.Fatalf("erasureTargetedScope = %d, want the measured 4", got)
	}
	empty := &cbv1.ClusterErasure{}
	if got := erasureTargetedScope(empty); got != 0 {
		t.Fatalf("erasureTargetedScope on an untouched object = %d, want 0", got)
	}

	// End to end: such an erasure failing with six snapshots left still produces a legible record.
	r := newRecordReconciler(&residueLister{snaps: snapshotsMatching(6)})
	rec, _ := r.observeErasureResidue(context.Background(), legacy,
		&cbv1.BackupRepository{ObjectMeta: metav1.ObjectMeta{Name: "dr"}}, nil)
	r.stampErasureRecord(legacy, rec)
	if legacy.Status.SnapshotsTargeted != 10 || legacy.Status.SnapshotsForgotten != 4 ||
		legacy.Status.SnapshotsRemaining != 6 {
		t.Fatalf("record %+v, want targeted 10 / forgotten 4 / remaining 6", legacy.Status)
	}
}

// containsAll reports whether s contains every substring.
func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}
