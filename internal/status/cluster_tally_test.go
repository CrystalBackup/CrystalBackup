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

package status

import (
	"testing"

	"github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
)

// ---------------------------------------------------------------------------------------------
// The tests for the run-level namespace accounting.
//
// The aggregation these cover had unit tests for its PHASE and none at all for its COUNTERS, and
// that is precisely how a production run came to publish `namespacesFailed: 32` beside 33 children
// reading 29 Completed / 3 PartiallyFailed / 1 Pending. So the tests below are written the other
// way round from the usual: the INVARIANT tests come first and matter most, because they fail for a
// branch nobody thought to add a row for.
// ---------------------------------------------------------------------------------------------

// allBackupPhases is every phase a child Backup can carry, plus the empty string (not yet
// reconciled) and a string from no vocabulary at all.
var allBackupPhases = []string{
	"",
	string(BackupPhasePending),
	string(BackupPhaseSnapshottingHooks),
	string(BackupPhaseSnapshotting),
	string(BackupPhaseUploading),
	string(BackupPhaseCompleted),
	string(BackupPhasePartiallyCompleted),
	string(BackupPhasePartiallyFailed),
	string(BackupPhaseFailed),
	"SomePhaseFromAFutureVersion",
}

// TestClusterTallyAlwaysSumsUp is THE test the incident asked for: whatever the mix of outcomes, the
// four buckets must account for every namespace folded in. A counter that does not add up is
// describing something other than the objects it claims to summarise.
//
// It sweeps every combination of 0..3 namespaces per outcome — 256 mixes — rather than a handful of
// hand-picked ones, so an outcome added later without a bucket fails here.
func TestClusterTallyAlwaysSumsUp(t *testing.T) {
	all := []NamespaceOutcome{NamespaceInFlight, NamespaceSucceeded, NamespaceFailed, NamespaceBlocked}
	for a := 0; a <= 3; a++ {
		for b := 0; b <= 3; b++ {
			for c := 0; c <= 3; c++ {
				for d := 0; d <= 3; d++ {
					counts := []int{a, b, c, d}
					var outcomes []NamespaceOutcome
					for i, n := range counts {
						for range n {
							outcomes = append(outcomes, all[i])
						}
					}
					got := TallyNamespaceOutcomes(outcomes)
					if !got.SumsUp() {
						t.Fatalf("mix %v: buckets do not add up: namespaces=%d counted=%d (%+v)",
							counts, got.Namespaces, got.Counted(), got)
					}
					if int(got.Namespaces) != len(outcomes) {
						t.Fatalf("mix %v: Namespaces=%d, want %d", counts, got.Namespaces, len(outcomes))
					}
					if int(got.InFlight) != a || int(got.Succeeded) != b ||
						int(got.Failed) != c || int(got.Blocked) != d {
						t.Fatalf("mix %v: got %+v", counts, got)
					}
				}
			}
		}
	}
}

// TestPhaseNeverContradictsTheCounters is the second half of the same invariant. The phase and the
// counters are the two things an administrator reads together; the incident's run showed a
// non-terminal phase beside numbers that described a different set of children entirely. Since both
// are now derived from the same outcome list, every legal combination must satisfy the four
// implications below — and no future edit to either mapper can break one without breaking this.
func TestPhaseNeverContradictsTheCounters(t *testing.T) {
	all := []NamespaceOutcome{NamespaceInFlight, NamespaceSucceeded, NamespaceFailed, NamespaceBlocked}
	for a := 0; a <= 2; a++ {
		for b := 0; b <= 2; b++ {
			for c := 0; c <= 2; c++ {
				for d := 0; d <= 2; d++ {
					counts := []int{a, b, c, d}
					var outcomes []NamespaceOutcome
					for i, n := range counts {
						for range n {
							outcomes = append(outcomes, all[i])
						}
					}
					tally := TallyNamespaceOutcomes(outcomes)
					phase := RollUpNamespaceOutcomes(outcomes)
					switch phase {
					case ClusterBackupPhasePending:
						if len(outcomes) != 0 {
							t.Fatalf("mix %v: Pending with %d namespaces accounted for", counts, len(outcomes))
						}
					case ClusterBackupPhaseCompleted:
						// The one that matters: Completed may never sit beside an unprotected namespace.
						if tally.Failed+tally.Blocked > 0 {
							t.Fatalf("mix %v: Completed with failed=%d blocked=%d", counts, tally.Failed, tally.Blocked)
						}
						if tally.InFlight > 0 {
							t.Fatalf("mix %v: Completed with inFlight=%d", counts, tally.InFlight)
						}
					case ClusterBackupPhaseFailed:
						if tally.Succeeded > 0 {
							t.Fatalf("mix %v: Failed with succeeded=%d", counts, tally.Succeeded)
						}
						if tally.Failed+tally.Blocked == 0 {
							t.Fatalf("mix %v: Failed with nothing failed or blocked", counts)
						}
					case ClusterBackupPhasePartiallyFailed:
						if tally.Succeeded == 0 || tally.Failed+tally.Blocked == 0 {
							t.Fatalf("mix %v: PartiallyFailed with succeeded=%d failed=%d blocked=%d",
								counts, tally.Succeeded, tally.Failed, tally.Blocked)
						}
					case ClusterBackupPhaseRunning:
						if tally.InFlight == 0 {
							t.Fatalf("mix %v: Running with nothing in flight", counts)
						}
					}
				}
			}
		}
	}
}

// TestOutcomeForBackupPhase pins the classification every child phase gets. It is the single
// mapping behind both the counters and the phase, so a phase reclassified here moves both together
// — which is the point.
func TestOutcomeForBackupPhase(t *testing.T) {
	want := map[string]NamespaceOutcome{
		"":                                    NamespaceInFlight,
		string(BackupPhasePending):            NamespaceInFlight,
		string(BackupPhaseSnapshottingHooks):  NamespaceInFlight,
		string(BackupPhaseSnapshotting):       NamespaceInFlight,
		string(BackupPhaseUploading):          NamespaceInFlight,
		string(BackupPhaseCompleted):          NamespaceSucceeded,
		string(BackupPhasePartiallyCompleted): NamespaceSucceeded,
		string(BackupPhasePartiallyFailed):    NamespaceFailed,
		string(BackupPhaseFailed):             NamespaceFailed,
		"SomePhaseFromAFutureVersion":         NamespaceInFlight,
	}
	for _, p := range allBackupPhases {
		if got := OutcomeForBackupPhase(p); got != want[p] {
			t.Errorf("OutcomeForBackupPhase(%q) = %v, want %v", p, got, want[p])
		}
	}
	// No child phase may ever produce Blocked: "this run never backed this namespace up" is not a
	// statement any child's status can make, and the day it becomes one, a foreign object's phase
	// would be able to declare the run's own namespaces unprotected.
	for _, p := range allBackupPhases {
		if OutcomeForBackupPhase(p) == NamespaceBlocked {
			t.Errorf("phase %q classified as Blocked; only the fan-out may say a namespace was not backed up", p)
		}
	}
}

// TestUnknownChildPhaseHoldsTheRunNonTerminal covers the behaviour the shared classification
// changed. The old roll-up counted an unrecognized phase in neither the ok nor the bad tally, so a
// run made entirely of phases it did not understand rolled up to Completed — a green run over
// namespaces whose state nothing could read. It is now the absence of a verdict.
func TestUnknownChildPhaseHoldsTheRunNonTerminal(t *testing.T) {
	if got := RollUpBackupPhases([]string{"SomePhaseFromAFutureVersion"}); got != ClusterBackupPhaseRunning {
		t.Fatalf("unknown phase rolled up to %q, want Running", got)
	}
	if got := RollUpBackupPhases([]string{
		string(BackupPhaseCompleted), "SomePhaseFromAFutureVersion",
	}); got != ClusterBackupPhaseRunning {
		t.Fatalf("Completed + unknown rolled up to %q, want Running", got)
	}
}

// TestBlockedNamespaceIsNotBenign: a namespace nothing backed up must degrade the run exactly as a
// failed one does, whatever the object squatting on its coordinate says about itself. Splitting
// blocked out of failed was about naming the two facts apart, never about softening one of them.
func TestBlockedNamespaceIsNotBenign(t *testing.T) {
	if got := RollUpNamespaceOutcomes([]NamespaceOutcome{NamespaceBlocked}); got != ClusterBackupPhaseFailed {
		t.Errorf("a run whose only namespace was never backed up rolled up to %q, want Failed", got)
	}
	if got := RollUpNamespaceOutcomes([]NamespaceOutcome{
		NamespaceSucceeded, NamespaceBlocked,
	}); got != ClusterBackupPhasePartiallyFailed {
		t.Errorf("succeeded + blocked rolled up to %q, want PartiallyFailed", got)
	}
}

// TestSkippedVolumeNamespaceCountsAsSucceeded ties the two levels together. RollUpVolumePhases
// treats a Skipped volume as NEUTRAL so that a namespace holding one permanently unsnapshottable
// PVC does not alarm on every run, forever. The cluster level must not undo that: the Completed
// child it produces has to land in Succeeded, and a run made only of such namespaces has to read
// Completed with a zero failed count.
func TestSkippedVolumeNamespaceCountsAsSucceeded(t *testing.T) {
	childPhase := RollUpVolumePhases([]v1alpha1.VolumeStatus{
		{Pvc: "data", Phase: VolumePhaseCompleted},
		{Pvc: "scratch", Phase: VolumePhaseSkipped, Reason: "CSISnapshotUnsupported"},
	})
	if childPhase != BackupPhaseCompleted {
		t.Fatalf("volume roll-up gave %q; the premise of this test has changed", childPhase)
	}
	if got := OutcomeForBackupPhase(string(childPhase)); got != NamespaceSucceeded {
		t.Fatalf("a namespace with an unsnapshottable PVC counted as %v, want Succeeded", got)
	}

	// The whole run, three such namespaces, every single night.
	outcomes := OutcomesForBackupPhases([]string{
		string(childPhase), string(childPhase), string(childPhase),
	})
	tally := TallyNamespaceOutcomes(outcomes)
	if tally.Succeeded != 3 || tally.Failed != 0 || tally.Blocked != 0 {
		t.Errorf("got %+v, want 3 succeeded and nothing else", tally)
	}
	if got := RollUpNamespaceOutcomes(outcomes); got != ClusterBackupPhaseCompleted {
		t.Errorf("run phase %q, want Completed — a permanently unsnapshottable PVC must not alarm forever", got)
	}
}

// TestSumsUpDetectsADivergentTotal proves SumsUp is a real check and not a tautology over the
// struct, so the guard in the controller is not decorative.
//
// It also records the uncomfortable half of the incident, because it bears on what these tests can
// and cannot promise: the run that published `namespacesFailed: 32` DID add up — 0 succeeded + 32
// failed + 1 in flight is exactly the 33 namespaces it was accounting for. Summing is necessary and
// it is not sufficient. What was wrong was WHICH bucket 32 namespaces went into, and only a test
// that reads the counters back against the child objects catches that; see
// internal/controller/clusterbackup_aggregate_test.go.
func TestSumsUpDetectsADivergentTotal(t *testing.T) {
	consistent := ClusterTally{Namespaces: 33, Succeeded: 0, Failed: 32, Blocked: 0, InFlight: 1}
	if !consistent.SumsUp() {
		t.Errorf("%+v reported as inconsistent; counted=%d", consistent, consistent.Counted())
	}
	divergent := ClusterTally{Namespaces: 33, Succeeded: 29, Failed: 32, Blocked: 0, InFlight: 1}
	if divergent.SumsUp() {
		t.Errorf("%+v reported as consistent; counted=%d", divergent, divergent.Counted())
	}
}
