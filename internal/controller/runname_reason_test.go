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
	"fmt"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
	"github.com/CrystalBackup/CrystalBackup/internal/status"
)

// ---------------------------------------------------------------------------------------------
// What a run records about the namespaces it did NOT back up.
//
// Nine consecutive nights on a production cluster: every ClusterBackup PartiallyFailed with
// namespacesBlocked: 32, beside 32 namespace-level Backups reading Completed with 28 of 29 volumes
// in the repository. The counters were honest — this run did not write that data — and completely
// undiagnosable: status.failures held ten prose sentences, all identical, none of which named the
// FACT that decided the classification. Which branch of classifyCoordinate fired, whether the
// occupant carried this run's own stamp, whether it held results, how its creationTimestamp
// compared to the run's: none of it reached the object, so nine nights of evidence answered
// nothing that a single reconcile would not have.
//
// These tests pin the instrumentation, and only the instrumentation. Every assertion below is
// about what is RECORDED; not one of them asserts an ownership verdict, because this lot must not
// move one. TestClassifyCoordinate (runname_test.go) remains the verdict's test.
// ---------------------------------------------------------------------------------------------

// reasonBackup builds an occupant of a coordinate with the given annotations, phase, creation skew
// against the run object (incidentRunCreated), and volumes.
func reasonBackup(anns map[string]string, phase string, createdAfterRun time.Duration, vols ...cbv1.VolumeStatus) *cbv1.Backup {
	b := backupWith(anns, phase, vols...)
	b.CreationTimestamp = metav1.NewTime(incidentRunCreated.Add(createdAfterRun))
	return b
}

// TestCoordinateReasonNamesTheClassificationReached: the four foreign branches and the two
// non-foreign ones all render one reason today (RunNameCollision) differing only in free prose, so
// a night's blocked namespaces cannot be counted by cause. Each branch must carry a STABLE token.
func TestCoordinateReasonNamesTheClassificationReached(t *testing.T) {
	cases := []struct {
		name   string
		backup *cbv1.Backup
		want   string
	}{{
		name:   "my own child seen again",
		backup: reasonBackup(map[string]string{apiconst.AnnotationParentUID: string(testOwnerUID)}, "", time.Minute),
		want:   coordinateCodeMine,
	}, {
		name: "a different owner's stamp",
		backup: reasonBackup(map[string]string{apiconst.AnnotationParentUID: string(testOtherUID)},
			string(status.BackupPhaseCompleted), -24*time.Hour),
		want: coordinateCodeForeignParent,
	}, {
		name: "a discovery projection",
		backup: reasonBackup(map[string]string{apiconst.AnnotationProjected: apiconst.AnnotationProjectedValue},
			string(status.BackupPhaseCompleted), -time.Hour,
			cbv1.VolumeStatus{Pvc: "data", Phase: status.VolumePhaseCompleted, SnapshotID: "s1"}),
		want: coordinateCodeProjection,
	}, {
		// The path the backlog entry is written about.
		name:   "an unstamped terminal Backup",
		backup: reasonBackup(nil, string(status.BackupPhaseCompleted), -14*time.Hour),
		want:   coordinateCodeUnstampedTerminal,
	}, {
		name: "an unstamped, non-terminal Backup already holding a snapshot",
		backup: reasonBackup(nil, string(status.BackupPhaseUploading), -time.Minute,
			cbv1.VolumeStatus{Pvc: "data", Phase: status.VolumePhaseCompleted, SnapshotID: "s1"}),
		want: coordinateCodeUnstampedWithResults,
	}, {
		name:   "an unstamped, in-flight Backup holding nothing",
		backup: reasonBackup(nil, string(status.BackupPhasePending), time.Second),
		want:   coordinateCodeAdoptable,
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, reason := classifyCoordinate(tc.backup, testOwnerUID, incidentRunCreated)
			if reason.Code != tc.want {
				t.Fatalf("reason code = %q, want %q", reason.Code, tc.want)
			}
			if !strings.Contains(reason.Facts(), "class="+tc.want) {
				t.Fatalf("facts %q must name the classification reached", reason.Facts())
			}
		})
	}
}

// TestCoordinateReasonCarriesTheDiscriminators: the three facts the next lot needs in order to
// enumerate the OTHER ways a namespace gets blocked — the occupant's own stamp, whether it carried
// a result, and how its creationTimestamp compares to the run object's. The last one is the
// discriminator the candidate fix in the backlog turns on ("at or after the run object's"), and it
// is the one the object has never carried at all: classifyCoordinate could not see it.
func TestCoordinateReasonCarriesTheDiscriminators(t *testing.T) {
	// The production shape: an unstamped terminal child, created fourteen hours before the run
	// object, holding a full set of snapshots.
	occupant := reasonBackup(nil, string(status.BackupPhaseCompleted), -14*time.Hour,
		cbv1.VolumeStatus{Pvc: "data", Phase: status.VolumePhaseCompleted, SnapshotID: "s1", AddedBytes: 4096})
	_, reason := classifyCoordinate(occupant, testOwnerUID, incidentRunCreated)

	facts := reason.Facts()
	for _, want := range []string{
		"class=" + coordinateCodeUnstampedTerminal,
		"stamp=" + coordinateStampNone,
		"phase=" + string(status.BackupPhaseCompleted),
		"data=yes",
		"age=-14h0m0s",
	} {
		if !strings.Contains(facts, want) {
			t.Fatalf("facts must contain %q; got %q", want, facts)
		}
	}

	// The falsified hypothesis' counterpart: the SAME occupant carrying this run's own stamp must
	// render stamp=mine, so a blocked namespace whose child is correctly stamped is legible as
	// exactly that and not confused with the unstamped-upgrade path.
	stamped := reasonBackup(map[string]string{apiconst.AnnotationParentUID: string(testOwnerUID)},
		string(status.BackupPhaseCompleted), time.Minute)
	_, mine := classifyCoordinate(stamped, testOwnerUID, incidentRunCreated)
	if !strings.Contains(mine.Facts(), "stamp="+coordinateStampMine) {
		t.Fatalf("facts must render the run's own stamp; got %q", mine.Facts())
	}
	if !strings.Contains(mine.Facts(), "age=+1m0s") {
		t.Fatalf("facts must render a child created AFTER the run object with a sign; got %q", mine.Facts())
	}
}

// TestCollisionMessageKeepsTheFactsWhenItTruncates: the FailureRecord message is clamped to
// clusterBackupMessageCap runes and a namespace plus a Backup name are 253 each, so something has to
// be truncatable however high that cap goes. The facts must not be it — they are the only part of
// the message an operator cannot reconstruct from FailureRecord.Namespace and .Backup.
func TestCollisionMessageKeepsTheFactsWhenItTruncates(t *testing.T) {
	err := &runNameCollisionError{
		Namespace: strings.Repeat("n", 253),
		Name:      strings.Repeat("b", 253),
		Detail:    "it is the terminal record of an earlier backup (phase Completed)",
		Facts:     "class=UnstampedTerminalChild stamp=none phase=Completed data=yes age=-14h0m0s",
	}
	msg := clampMessage(err.Error())
	if len([]rune(msg)) > clusterBackupMessageCap {
		t.Fatalf("clamped message is %d runes, cap is %d", len([]rune(msg)), clusterBackupMessageCap)
	}
	for _, want := range []string{reasonRunNameCollision, "class=UnstampedTerminalChild", "stamp=none", "age=-14h0m0s"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("clamped message must survive with %q; got:\n%s", want, msg)
		}
	}
}

// TestBlockedNamespaceReasonNamesTheDiscriminatorOnAStampedTerminalChild reproduces the live
// observation that FALSIFIED the backlog entry's stated cause.
//
// A fresh run, children stamped with its own UID, every one of them terminal and Completed — and
// the fan-out still refused the namespace, so it counted blocked. The unstamped-child story cannot
// explain that, and nothing in the run object said so: the recorded message was the same prose as
// an unstamped occupant's.
//
// What is asserted here is ONLY the record. The namespace stays blocked — the fan-out's refusal is
// authoritative over an object re-read one pass later, and reading the occupant's phase instead is
// precisely the false success d3d2659 exists to stop. What must change is that the run says which
// facts it saw: that the object now at the coordinate carries THIS run's stamp, and that it holds
// data. That pair is what the next lot needs to enumerate the path.
func TestBlockedNamespaceReasonNamesTheDiscriminatorOnAStampedTerminalChild(t *testing.T) {
	const ns = "tenant-a"
	child := ledgerChild(ns, incidentRun, string(status.BackupPhaseCompleted), true, nil)
	child.CreationTimestamp = incidentRunCreated

	collided := map[string]blockedCoordinate{ns: {
		reason:  coordinateCodeUnstampedTerminal,
		hasData: true,
		err: runNameCollisionError{
			Namespace: ns, Name: incidentRun,
			Detail: "it is the terminal record of an earlier backup (phase Completed)",
			Facts:  "class=" + coordinateCodeUnstampedTerminal + " stamp=none phase=Completed data=yes age=-14h0m0s",
		},
	}}

	l := buildRunLedger(incidentRun, incidentRunUID, incidentRunCreated,
		[]string{ns}, []cbv1.Backup{child}, collided)

	if len(l.verdicts) != 1 {
		t.Fatalf("ledger has %d verdicts, want 1", len(l.verdicts))
	}
	v := l.verdicts[0]
	if v.outcome != status.NamespaceBlocked {
		t.Fatalf("outcome = %v, want Blocked — this lot does not move the classification", v.outcome)
	}
	if v.blocked == nil {
		t.Fatal("a blocked namespace must carry the reason that blocked it")
	}
	if !v.blocked.stampedByRun {
		t.Error("the occupant carries this run's own UID; the record must say so — that fact is what " +
			"falsifies the unstamped-child hypothesis")
	}
	if !v.blocked.dataAtCoordinate {
		t.Error("the occupant holds snapshots; the record must say the coordinate holds data")
	}
	if v.failure == nil || !strings.Contains(v.failure.Message, "class=") {
		t.Fatalf("the sampled failure must carry the facts; got %+v", v.failure)
	}
	if !strings.Contains(v.failure.Message, "recheck=stampedByThisRun") {
		t.Errorf("the message must record that the coordinate disagrees with the fan-out's verdict; got:\n%s",
			v.failure.Message)
	}
}

// TestBlockedReasonsBreakdownIsBoundedByReasonNotByNamespace is the reason the breakdown is a
// status FIELD and not more entries in status.failures.
//
// status.failures is capped at ten, deliberately (adr/0009: no unbounded per-namespace map). Thirty
// two blocked namespaces therefore yield ten sampled messages and twenty-two silences, and "were
// these namespaces protected or not?" is a question about all thirty-two. A breakdown keyed by the
// classification code answers it for every one of them while staying bounded by a CLOSED set of
// codes — five today — however many namespaces the run fans out to.
func TestBlockedReasonsBreakdownIsBoundedByReasonNotByNamespace(t *testing.T) {
	const blocked = 32
	matched := make([]string, 0, blocked)
	children := make([]cbv1.Backup, 0, blocked)
	collided := make(map[string]blockedCoordinate, blocked)
	for i := range blocked {
		ns := fmt.Sprintf("tenant-%02d", i)
		matched = append(matched, ns)
		children = append(children, ledgerChild(ns, incidentRun, string(status.BackupPhaseCompleted), false, nil))
		collided[ns] = blockedCoordinate{
			reason:  coordinateCodeUnstampedTerminal,
			hasData: true,
			err:     runNameCollisionError{Namespace: ns, Name: incidentRun, Detail: "d", Facts: "f"},
		}
	}

	l := buildRunLedger(incidentRun, incidentRunUID, incidentRunCreated, matched, children, collided)
	summary := summariseBlockedReasons(l.blockedFacts())

	if len(summary) != 1 {
		t.Fatalf("breakdown has %d entries, want 1 — it is keyed by cause, not by namespace: %+v", len(summary), summary)
	}
	got := summary[0]
	if got.Reason != coordinateCodeUnstampedTerminal {
		t.Errorf("reason = %q, want %q", got.Reason, coordinateCodeUnstampedTerminal)
	}
	if got.Namespaces != blocked {
		t.Errorf("namespaces = %d, want %d — the breakdown must account for every blocked namespace, "+
			"not the ten status.failures samples", got.Namespaces, blocked)
	}
	// The counter-side honesty: the run says "blocked", and the coordinates say "there is a backup
	// here". A single object must be enough to see that contradiction.
	if got.WithDataAtCoordinate != blocked {
		t.Errorf("withDataAtCoordinate = %d, want %d: every one of these coordinates holds snapshots",
			got.WithDataAtCoordinate, blocked)
	}
	if got.StampedByThisRun != 0 {
		t.Errorf("stampedByThisRun = %d, want 0: none of these occupants carries the run's UID", got.StampedByThisRun)
	}
}

// TestRunObjectAloneAnswersWhetherTheBlockedNamespacesWereProtected is the end-to-end version, over
// the production incident's own fixture, and it is the acceptance criterion for this whole change:
// ONE ClusterBackup object, no logs, no events, no cluster access, must answer "why were these
// namespaces blocked, and is there a backup there or not?".
//
// Before, it could not. namespacesBlocked said 32; status.failures held ten identical sentences
// naming a conclusion; and whether those thirty-two coordinates held data — the difference between
// "nothing protected these namespaces" and "something did, this run just cannot prove it was it" —
// appeared nowhere at all. Ten nights of that told an administrator nothing the first night had not.
func TestRunObjectAloneAnswersWhetherTheBlockedNamespacesWereProtected(t *testing.T) {
	st, _ := runTheIncident(t, false)

	if st.NamespacesBlocked != 32 {
		t.Fatalf("namespacesBlocked = %d, want 32 — the fixture no longer reproduces the incident", st.NamespacesBlocked)
	}
	if len(st.BlockedReasons) == 0 {
		t.Fatal("32 blocked namespaces and no breakdown of why")
	}
	// The breakdown accounts for EVERY blocked namespace, not the ten status.failures sampled. It is
	// the same invariant ClusterTally.SumsUp enforces one level up, applied to the explanation: a
	// reason list that does not add up to the counter is describing different namespaces from the
	// ones being counted.
	var total, withData int32
	for _, e := range st.BlockedReasons {
		total += e.Namespaces
		withData += e.WithDataAtCoordinate
	}
	if total != st.NamespacesBlocked {
		t.Errorf("blockedReasons accounts for %d namespaces, namespacesBlocked is %d", total, st.NamespacesBlocked)
	}
	if len(st.Failures) > status.DefaultFailureCap {
		t.Errorf("failures = %d entries, cap is %d", len(st.Failures), status.DefaultFailureCap)
	}
	// The cause, named once rather than repeated thirty-two times.
	if st.BlockedReasons[0].Reason != coordinateCodeUnstampedTerminal {
		t.Errorf("reason = %q, want %q", st.BlockedReasons[0].Reason, coordinateCodeUnstampedTerminal)
	}
	// And the counter-side honesty. Every one of these coordinates holds a Completed Backup with a
	// snapshot ID. The run must be able to say so while still refusing to count it as its own work.
	if withData != st.NamespacesBlocked {
		t.Errorf("withDataAtCoordinate totals %d of %d blocked namespaces; the object still cannot say "+
			"whether they were protected", withData, st.NamespacesBlocked)
	}
	if st.PVCsSucceeded != 0 || st.AddedBytes != 0 {
		t.Errorf("pvcsSucceeded/addedBytes = %d/%d, want 0/0: recording that data EXISTS at a coordinate "+
			"must never turn into claiming it", st.PVCsSucceeded, st.AddedBytes)
	}
}

// TestBlockedReasonsBreakdownSplitsByCause: two causes, two lines, sorted — so the breakdown is
// stable between passes and comparable between nights.
func TestBlockedReasonsBreakdownSplitsByCause(t *testing.T) {
	facts := []blockedNamespaceFacts{
		{reason: coordinateCodeUnstampedTerminal, dataAtCoordinate: true},
		{reason: coordinateCodeProjection, dataAtCoordinate: true},
		{reason: coordinateCodeUnstampedTerminal, dataAtCoordinate: false, stampedByRun: true},
	}
	got := summariseBlockedReasons(facts)
	if len(got) != 2 {
		t.Fatalf("breakdown = %+v, want 2 entries", got)
	}
	if got[0].Reason != coordinateCodeProjection || got[1].Reason != coordinateCodeUnstampedTerminal {
		t.Fatalf("breakdown must be sorted by reason for a stable status; got %+v", got)
	}
	if got[1].Namespaces != 2 || got[1].WithDataAtCoordinate != 1 || got[1].StampedByThisRun != 1 {
		t.Fatalf("per-cause counters wrong: %+v", got[1])
	}
	if summariseBlockedReasons(nil) != nil {
		t.Fatal("no blocked namespace must leave the field absent, not an empty list")
	}
}
