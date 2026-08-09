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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/status"
)

// ---------------------------------------------------------------------------------------------
// The three volume counters both restore kinds publish.
//
// A restore used to publish restoredVolumes and nothing else: no failed-volume count anywhere, and
// that one counter written ONLY on the terminal pass. So a nine-volume restore read 0 for its whole
// duration and then 9, and a restore that lost two volumes said "7" and left the rest to a condition
// message and to mover Jobs that are deleted moments later.
//
// The specs below drive stampVolumeCounts, which is the single place any of the three is assigned —
// on every pass, from ONE tally.
// ---------------------------------------------------------------------------------------------

// driveWith builds a volumeDrive whose tally is the one classification of the given per-volume
// verdicts, exactly as driveVolumes produces it.
func driveWith(outcomes ...status.VolumeRestoreOutcome) volumeDrive {
	return volumeDrive{tally: status.TallyVolumeRestoreOutcomes(outcomes)}
}

// TestVolumeCountsArePublishedBeforeTheTerminalPass is the progress half of the defect. Mid-restore —
// four volumes back, two lost, three still moving — all three counters must already be on the object,
// because "is it moving" is a question asked DURING a restore and answered from `kubectl get restore`.
func TestVolumeCountsArePublishedBeforeTheTerminalPass(t *testing.T) {
	drive := driveWith(
		status.VolumeRestoreRestored, status.VolumeRestoreInFlight, status.VolumeRestoreFailed,
		status.VolumeRestoreRestored, status.VolumeRestoreInFlight, status.VolumeRestoreRestored,
		status.VolumeRestoreFailed, status.VolumeRestoreInFlight, status.VolumeRestoreRestored,
	)
	r := &cbv1.Restore{ObjectMeta: metav1.ObjectMeta{Namespace: "team-db", Name: "recover"}}

	stampVolumeCounts(context.Background(), events.NewFakeRecorder(4), r, drive.tally,
		&r.Status.PlannedVolumes, &r.Status.RestoredVolumes, &r.Status.FailedVolumes)

	if r.Status.RestoredVolumes != 4 {
		t.Errorf("restoredVolumes = %d, want 4 while the restore is still running", r.Status.RestoredVolumes)
	}
	if r.Status.FailedVolumes != 2 {
		t.Errorf("failedVolumes = %d, want 2 — the answer to what did not come back", r.Status.FailedVolumes)
	}
	if r.Status.PlannedVolumes != 9 {
		t.Errorf("plannedVolumes = %d, want 9 — a counter with no denominator is not a report", r.Status.PlannedVolumes)
	}
	// The reader's own arithmetic has to work on the published fields alone.
	if inflight := r.Status.PlannedVolumes - r.Status.RestoredVolumes - r.Status.FailedVolumes; inflight != 3 {
		t.Errorf("planned-restored-failed = %d, want the 3 volumes still in flight", inflight)
	}
	// The restore is NOT allowed to go terminal here, and the same tally is what decides that.
	if drive.settled() != 6 {
		t.Errorf("settled() = %d, want 6", drive.settled())
	}
	if drive.settled() >= int(drive.tally.Planned) {
		t.Error("a restore with volumes in flight must not be settled")
	}
}

// TestVolumeCountsMatchTheVolumeVerdictsOnBothKinds reads the published counters back against the
// per-volume verdicts themselves, on a ClusterRestore as well as a Restore.
//
// This is the check the ClusterBackup incident actually needed. There, 0 + 32 + 1 = 33 added up
// perfectly and the numbers were still false, because they were counting something other than the
// children beside them. A sum that balances is necessary and nowhere near sufficient.
func TestVolumeCountsMatchTheVolumeVerdictsOnBothKinds(t *testing.T) {
	verdicts := []status.VolumeRestoreOutcome{
		status.VolumeRestoreFailed, status.VolumeRestoreRestored, status.VolumeRestoreRestored,
		status.VolumeRestoreInFlight, status.VolumeRestoreFailed, status.VolumeRestoreRestored,
	}
	drive := driveWith(verdicts...)

	// Independently re-derived from the verdicts, not from the tally.
	var wantRestored, wantFailed, wantInFlight int32
	for _, v := range verdicts {
		switch v {
		case status.VolumeRestoreRestored:
			wantRestored++
		case status.VolumeRestoreFailed:
			wantFailed++
		case status.VolumeRestoreInFlight:
			wantInFlight++
		}
	}

	ns := &cbv1.Restore{ObjectMeta: metav1.ObjectMeta{Namespace: "team-db", Name: "recover"}}
	cluster := &cbv1.ClusterRestore{ObjectMeta: metav1.ObjectMeta{Name: "dr-recover"}}
	stampVolumeCounts(context.Background(), events.NewFakeRecorder(4), ns, drive.tally,
		&ns.Status.PlannedVolumes, &ns.Status.RestoredVolumes, &ns.Status.FailedVolumes)
	stampVolumeCounts(context.Background(), events.NewFakeRecorder(4), cluster, drive.tally,
		&cluster.Status.PlannedVolumes, &cluster.Status.RestoredVolumes, &cluster.Status.FailedVolumes)

	for _, got := range []struct {
		kind                      string
		planned, restored, failed int32
	}{
		{"Restore", ns.Status.PlannedVolumes, ns.Status.RestoredVolumes, ns.Status.FailedVolumes},
		{"ClusterRestore", cluster.Status.PlannedVolumes, cluster.Status.RestoredVolumes, cluster.Status.FailedVolumes},
	} {
		if got.restored != wantRestored || got.failed != wantFailed {
			t.Errorf("%s: restored=%d failed=%d, want %d/%d from the volume verdicts",
				got.kind, got.restored, got.failed, wantRestored, wantFailed)
		}
		if got.planned != int32(len(verdicts)) {
			t.Errorf("%s: plannedVolumes=%d, want %d", got.kind, got.planned, len(verdicts))
		}
		if got.restored+got.failed+wantInFlight != got.planned {
			t.Errorf("%s: %d+%d+%d does not account for %d planned volumes",
				got.kind, got.restored, got.failed, wantInFlight, got.planned)
		}
	}
}

// TestClassifyVolumeOutcome pins the one mapping every count descends from, arm by arm. The subtle
// one is the transient error: a volume whose advise call failed with its error budget intact has NO
// verdict, because the next pass may well restore it. Counting it as failed would tell somebody
// mid-disaster that data is gone while the operator is still retrying, and counting it as nothing at
// all would drop it out of the denominator.
func TestClassifyVolumeOutcome(t *testing.T) {
	boom := errors.New("transient: exposure not ready")
	for _, tc := range []struct {
		name            string
		outcome         volumeOutcome
		err             error
		budgetExhausted bool
		want            status.VolumeRestoreOutcome
	}{
		{"restored", volumeOutcome{settled: true, restoredBytes: 10}, nil, false, status.VolumeRestoreRestored},
		{"settled failed", volumeOutcome{settled: true, failed: true, reason: "RestoreTimedOut"}, nil, false, status.VolumeRestoreFailed},
		{"still driving", volumeOutcome{}, nil, false, status.VolumeRestoreInFlight},
		{"transient error, budget intact", volumeOutcome{}, boom, false, status.VolumeRestoreInFlight},
		{"error budget exhausted", volumeOutcome{}, boom, true, status.VolumeRestoreFailed},
	} {
		if got := classifyVolumeOutcome(tc.outcome, tc.err, tc.budgetExhausted); got != tc.want {
			t.Errorf("%s: classified as %s, want %s", tc.name, got, tc.want)
		}
	}
	// And the tally that follows from a mixed pass: the retrying volume is in flight, not lost.
	drive := driveWith(
		classifyVolumeOutcome(volumeOutcome{}, boom, false),
		classifyVolumeOutcome(volumeOutcome{settled: true}, nil, false),
	)
	if drive.failedCount() != 0 {
		t.Errorf("failedCount() = %d, want 0: an unresolved error is not a lost volume", drive.failedCount())
	}
	if drive.tally.InFlight != 1 || drive.tally.Planned != 2 {
		t.Errorf("tally %+v: the retrying volume must still be accounted for", drive.tally)
	}
}

// TestInconsistentVolumeCountsAreAnnounced is what the invariant check is worth. The constructor
// cannot produce a divergent tally, so this hand-builds one — the shape a future second counting site
// would produce — and requires the operator to SAY so rather than publish numbers it cannot stand
// behind. Note that the numbers are still written: refusing to write them would leave the object
// silent, which is worse than wrong-and-flagged.
func TestInconsistentVolumeCountsAreAnnounced(t *testing.T) {
	fake := events.NewFakeRecorder(4)
	r := &cbv1.Restore{ObjectMeta: metav1.ObjectMeta{Namespace: "team-db", Name: "recover"}}
	divergent := status.RestoreTally{Planned: 9, Restored: 4, Failed: 2, InFlight: 0}

	stampVolumeCounts(context.Background(), fake, r, divergent,
		&r.Status.PlannedVolumes, &r.Status.RestoredVolumes, &r.Status.FailedVolumes)

	select {
	case ev := <-fake.Events:
		if !containsAll(ev, "VolumeCountsInconsistent") {
			t.Fatalf("event %q, want VolumeCountsInconsistent", ev)
		}
	default:
		t.Fatal("no event: counters that do not add up must be announced, not quietly published")
	}
	if r.Status.PlannedVolumes != 9 || r.Status.RestoredVolumes != 4 || r.Status.FailedVolumes != 2 {
		t.Fatalf("the counters were not written: %+v", r.Status)
	}
}

// TestAConsistentTallyIsSilent is the negative half: the ordinary case must not emit a warning event
// on every pass of every restore.
func TestAConsistentTallyIsSilent(t *testing.T) {
	fake := events.NewFakeRecorder(4)
	r := &cbv1.Restore{ObjectMeta: metav1.ObjectMeta{Namespace: "team-db", Name: "recover"}}

	stampVolumeCounts(context.Background(), fake, r,
		driveWith(status.VolumeRestoreRestored, status.VolumeRestoreFailed, status.VolumeRestoreInFlight).tally,
		&r.Status.PlannedVolumes, &r.Status.RestoredVolumes, &r.Status.FailedVolumes)

	select {
	case ev := <-fake.Events:
		t.Fatalf("unexpected event on a consistent tally: %q", ev)
	default:
	}
}
