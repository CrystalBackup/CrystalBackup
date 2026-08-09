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
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/status"
)

// abandonmentNow is the fixed "now" every case below is judged at, so the arithmetic in a failure
// message is readable rather than relative to whenever the suite happened to run.
var abandonmentNow = time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)

func atTime(offset time.Duration) *metav1.Time {
	t := metav1.NewTime(abandonmentNow.Add(offset))
	return &t
}

// TestVolumeInFlightReason is the unit test of the predicate the whole abandoned-run decision rests
// on, and — exactly like TestMoverPodEverStarted one layer down — it is worth more than the decision
// itself.
//
// Get it wrong in the "not in flight" direction and a schedule starts shooting healthy runs: a
// multi-terabyte first backup that has been uploading for six hours is declared abandoned, its
// mover Job is torn down, and the terabytes are re-uploaded from scratch next time. That is a far
// worse defect than the thirty-one-hour wedge this closes. Get it wrong in the "in flight"
// direction and the wedge comes back with a predicate on top of it.
//
// So the cases are two lists: everything that MUST count as in flight, and everything that must not.
func TestVolumeInFlightReason(t *testing.T) {
	const pendingDeadline = time.Hour

	tests := []struct {
		name       string
		vol        cbv1.VolumeStatus
		wantInFlig bool
	}{
		// ── IN FLIGHT: the shapes the decision must never kill ─────────────
		{
			// THE dangerous case. A running mover has no bound anywhere in this product, on purpose
			// (see moverStartDeadline): the phase alone is the veto, and no elapsed time, however
			// absurd, may override it.
			name:       "Uploading for six hours — a large volume legitimately does this",
			vol:        cbv1.VolumeStatus{Pvc: "big-data", Phase: status.VolumePhaseUploading, FirstAttemptAt: atTime(-6 * time.Hour)},
			wantInFlig: true,
		},
		{
			name:       "Uploading for thirty days — still not ours to judge",
			vol:        cbv1.VolumeStatus{Pvc: "huge", Phase: status.VolumePhaseUploading, FirstAttemptAt: atTime(-30 * 24 * time.Hour)},
			wantInFlig: true,
		},
		{
			// snapshotReadyDeadline is two hours because a ceph-csi flatten or a cloud-disk snapshot
			// of a multi-terabyte volume legitimately takes that long, and the controller that owns
			// the origin VolumeSnapshot's clock fails the volume itself at the bound.
			name:       "Snapshotting for ninety minutes — inside the controller's own bound",
			vol:        cbv1.VolumeStatus{Pvc: "ceph", Phase: status.VolumePhaseSnapshotting, FirstAttemptAt: atTime(-90 * time.Minute)},
			wantInFlig: true,
		},
		{
			name:       "Snapshotting for three days — bounded elsewhere, never here",
			vol:        cbv1.VolumeStatus{Pvc: "ceph", Phase: status.VolumePhaseSnapshotting, FirstAttemptAt: atTime(-72 * time.Hour)},
			wantInFlig: true,
		},
		{
			// Volumes advance one per reconcile, so most of a wide namespace sits un-attempted while
			// the head of the queue works. Reading that as evidence would make every wide backup
			// killable the moment its first volume stalled.
			name:       "Pending, never attempted (firstAttemptAt nil) — queued, not stuck",
			vol:        cbv1.VolumeStatus{Pvc: "queued", Phase: status.VolumePhasePending},
			wantInFlig: true,
		},
		{
			name:       "empty phase, never attempted — a freshly enumerated volume",
			vol:        cbv1.VolumeStatus{Pvc: "fresh"},
			wantInFlig: true,
		},
		{
			name:       "Pending for fifty-nine minutes — inside pendingResolveDeadline",
			vol:        cbv1.VolumeStatus{Pvc: "resolving", Phase: status.VolumePhasePending, FirstAttemptAt: atTime(-59 * time.Minute)},
			wantInFlig: true,
		},
		{
			name:       "Pending for exactly pendingResolveDeadline — the boundary is inclusive",
			vol:        cbv1.VolumeStatus{Pvc: "resolving", Phase: status.VolumePhasePending, FirstAttemptAt: atTime(-time.Hour)},
			wantInFlig: true,
		},

		// ── NOT in flight: settled, or out of time ─────────────────────────
		{
			// The incident's own volume: one PVC naming a StorageClass that did not exist, resolution
			// failing every pass, Pending for thirty hours.
			name:       "Pending for thirty hours — the incident",
			vol:        cbv1.VolumeStatus{Pvc: "broken-class", Phase: status.VolumePhasePending, FirstAttemptAt: atTime(-30 * time.Hour)},
			wantInFlig: false,
		},
		{
			name:       "Pending just past the deadline",
			vol:        cbv1.VolumeStatus{Pvc: "broken-class", Phase: status.VolumePhasePending, FirstAttemptAt: atTime(-61 * time.Minute)},
			wantInFlig: false,
		},
		{
			name:       "Completed — settled, and its snapshot survives the kill",
			vol:        cbv1.VolumeStatus{Pvc: "done", Phase: status.VolumePhaseCompleted, SnapshotID: "abc123"},
			wantInFlig: false,
		},
		{
			name:       "Skipped — unsnapshottable storage, a documented outcome",
			vol:        cbv1.VolumeStatus{Pvc: "nfs", Phase: status.VolumePhaseSkipped},
			wantInFlig: false,
		},
		{
			name:       "Failed — the controller already gave up on it",
			vol:        cbv1.VolumeStatus{Pvc: "bad", Phase: status.VolumePhaseFailed},
			wantInFlig: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := volumeInFlightReason(&tc.vol, abandonmentNow, pendingDeadline)
			if (got != "") != tc.wantInFlig {
				t.Fatalf("volumeInFlightReason(%s/%s) = %q; want in-flight=%v",
					tc.vol.Pvc, tc.vol.Phase, got, tc.wantInFlig)
			}
			if tc.wantInFlig && !strings.Contains(got, tc.vol.Pvc) {
				t.Fatalf("the in-flight reason must name the PVC so an operator can go look at it; got %q", got)
			}
		})
	}
}

// TestBackupAbandonment checks the two-of-two structure of the verdict: the evidence half and the
// grace half are both REQUIRED, and neither alone may terminate a run.
func TestBackupAbandonment(t *testing.T) {
	const (
		pendingDeadline = time.Hour
		grace           = 4 * time.Hour
	)

	// ready builds the Ready condition a non-terminal run carries, with its lastTransitionTime — the
	// run's own durable "when did I first report in" clock.
	ready := func(offset time.Duration) []metav1.Condition {
		return []metav1.Condition{{
			Type:               ConditionReady,
			Status:             metav1.ConditionFalse,
			Reason:             "InProgress",
			LastTransitionTime: *atTime(offset),
		}}
	}

	tests := []struct {
		name          string
		backup        cbv1.Backup
		wantAbandoned bool
	}{
		{
			// The cluster-plane wedge that is left after the per-phase deadlines: a run gated before
			// it ever enumerated a PVC (its location absent, its repository not initialized) has
			// created nothing and produced nothing, so there is provably no work to lose.
			name: "gated with no volumes for thirty-one hours — the wedge",
			backup: cbv1.Backup{
				ObjectMeta: metav1.ObjectMeta{Name: "nightly", CreationTimestamp: *atTime(-31 * time.Hour)},
				Status:     cbv1.BackupStatus{Phase: "Pending", Conditions: ready(-31 * time.Hour)},
			},
			wantAbandoned: true,
		},
		{
			// THE test that protects against the dangerous failure mode. Everything about this run
			// looks abandoned — five hours of silence at the run level, well past the grace — and it
			// must NOT be, because one volume is uploading.
			name: "one volume Uploading, run silent for five hours — never abandoned",
			backup: cbv1.Backup{
				ObjectMeta: metav1.ObjectMeta{Name: "nightly", CreationTimestamp: *atTime(-5 * time.Hour)},
				Status: cbv1.BackupStatus{
					Phase:      "Uploading",
					Conditions: ready(-5 * time.Hour),
					Volumes: []cbv1.VolumeStatus{
						{Pvc: "done", Phase: status.VolumePhaseCompleted, SnapshotID: "abc"},
						{Pvc: "big-data", Phase: status.VolumePhaseUploading, FirstAttemptAt: atTime(-5 * time.Hour)},
					},
				},
			},
			wantAbandoned: false,
		},
		{
			name: "one volume Snapshotting, run silent for a week — never abandoned",
			backup: cbv1.Backup{
				ObjectMeta: metav1.ObjectMeta{Name: "nightly", CreationTimestamp: *atTime(-7 * 24 * time.Hour)},
				Status: cbv1.BackupStatus{
					Phase:      "Snapshotting",
					Conditions: ready(-7 * 24 * time.Hour),
					Volumes:    []cbv1.VolumeStatus{{Pvc: "ceph", Phase: status.VolumePhaseSnapshotting}},
				},
			},
			wantAbandoned: false,
		},
		{
			// The grace half on its own. Nothing is in flight, but the run is ten minutes old — a
			// repository still running its `restic init` looks exactly like this, and killing it
			// would turn a transient into a nightly that never starts.
			name: "nothing in flight but only ten minutes old — grace withholds the kill",
			backup: cbv1.Backup{
				ObjectMeta: metav1.ObjectMeta{Name: "nightly", CreationTimestamp: *atTime(-10 * time.Minute)},
				Status:     cbv1.BackupStatus{Phase: "Pending", Conditions: ready(-10 * time.Minute)},
			},
			wantAbandoned: false,
		},
		{
			name: "nothing in flight, exactly at the grace boundary — inclusive, so no kill",
			backup: cbv1.Backup{
				ObjectMeta: metav1.ObjectMeta{Name: "nightly", CreationTimestamp: *atTime(-grace)},
				Status:     cbv1.BackupStatus{Phase: "Pending", Conditions: ready(-grace)},
			},
			wantAbandoned: false,
		},
		{
			// The progress clock earning its keep. The run is fifty hours old, every volume it has
			// finished with failed, and the volume at the head of the queue was picked up ten minutes
			// ago — a namespace of fifty unresolvable PVCs failing one per hour is WORKING, and must
			// survive a decision whose only other input is the run's age.
			name: "fifty hours old but the head volume was attempted ten minutes ago — progressing",
			backup: cbv1.Backup{
				ObjectMeta: metav1.ObjectMeta{Name: "nightly", CreationTimestamp: *atTime(-50 * time.Hour)},
				Status: cbv1.BackupStatus{
					Phase:      "Pending",
					Conditions: ready(-50 * time.Hour),
					Volumes: []cbv1.VolumeStatus{
						{Pvc: "gone-1", Phase: status.VolumePhaseFailed, FirstAttemptAt: atTime(-70 * time.Minute)},
						{Pvc: "gone-2", Phase: status.VolumePhasePending, FirstAttemptAt: atTime(-10 * time.Minute)},
					},
				},
			},
			wantAbandoned: false,
		},
		{
			// ...and the same namespace once the last volume has ALSO run out of its own budget and
			// nothing has been touched since. Now every part of it is out of time.
			name: "every volume out of time and no progress since — abandoned",
			backup: cbv1.Backup{
				ObjectMeta: metav1.ObjectMeta{Name: "nightly", CreationTimestamp: *atTime(-50 * time.Hour)},
				Status: cbv1.BackupStatus{
					Phase:      "Pending",
					Conditions: ready(-50 * time.Hour),
					Volumes: []cbv1.VolumeStatus{
						{Pvc: "gone-1", Phase: status.VolumePhaseFailed, FirstAttemptAt: atTime(-30 * time.Hour)},
						{Pvc: "gone-2", Phase: status.VolumePhasePending, FirstAttemptAt: atTime(-29 * time.Hour)},
					},
				},
			},
			wantAbandoned: true,
		},
		{
			// A run whose volumes are all settled but which never went terminal: the manifest half
			// hung, or the roll-up never landed. Nothing is in flight, nothing has moved for a day.
			name: "all volumes settled, run non-terminal for a day — abandoned",
			backup: cbv1.Backup{
				ObjectMeta: metav1.ObjectMeta{Name: "nightly", CreationTimestamp: *atTime(-24 * time.Hour)},
				Status: cbv1.BackupStatus{
					Phase:      "Uploading",
					Conditions: ready(-24 * time.Hour),
					Volumes: []cbv1.VolumeStatus{
						{Pvc: "done", Phase: status.VolumePhaseCompleted, SnapshotID: "abc", FirstAttemptAt: atTime(-24 * time.Hour)},
					},
				},
			},
			wantAbandoned: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			evidence, abandoned := backupAbandonment(&tc.backup, abandonmentNow, pendingDeadline, grace)
			if abandoned != tc.wantAbandoned {
				t.Fatalf("backupAbandonment = %v (evidence %q); want %v", abandoned, evidence, tc.wantAbandoned)
			}
			if evidence == "" {
				t.Fatal("the verdict must always carry its evidence: it is what the Event and the status " +
					"reason are made of, and a kill with no grounds is the unactionable report this lot closes")
			}
		})
	}
}

// TestAbandonedTerminalPhase pins the rule that a killed run is never reported as a success. A
// schedule that recorded lastSuccessTime for a run somebody had to shoot would be the same class of
// lie as the silent hang.
func TestAbandonedTerminalPhase(t *testing.T) {
	tests := []struct {
		name    string
		volumes []cbv1.VolumeStatus
		want    status.BackupPhase
	}{
		{
			name: "no volumes at all — gated before enumeration, nothing was produced",
			want: status.BackupPhaseFailed,
		},
		{
			name:    "one volume completed — that data is real and restorable",
			volumes: []cbv1.VolumeStatus{{Pvc: "a", Phase: status.VolumePhaseCompleted}, {Pvc: "b", Phase: status.VolumePhasePending}},
			want:    status.BackupPhasePartiallyFailed,
		},
		{
			name: "every volume completed but the run hung anyway — still NOT a success",
			// RollUpVolumePhases would say Completed here, and that is precisely the answer this
			// function exists to refuse.
			volumes: []cbv1.VolumeStatus{{Pvc: "a", Phase: status.VolumePhaseCompleted}},
			want:    status.BackupPhasePartiallyFailed,
		},
		{
			name:    "nothing completed",
			volumes: []cbv1.VolumeStatus{{Pvc: "a", Phase: status.VolumePhaseFailed}, {Pvc: "b", Phase: status.VolumePhaseSkipped}},
			want:    status.BackupPhaseFailed,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := abandonedTerminalPhase(tc.volumes)
			if got != string(tc.want) {
				t.Fatalf("abandonedTerminalPhase = %q; want %q", got, tc.want)
			}
			if !isTerminalBackupPhase(got) {
				t.Fatalf("%q is not a terminal phase, so the Backup controller would keep executing the run", got)
			}
		})
	}
}

// TestBackupLastProgressAt pins the progress clock: the newest durable sign of life, never the
// oldest, and never merely the object's age.
func TestBackupLastProgressAt(t *testing.T) {
	b := cbv1.Backup{
		ObjectMeta: metav1.ObjectMeta{CreationTimestamp: *atTime(-50 * time.Hour)},
		Status: cbv1.BackupStatus{
			Conditions: []metav1.Condition{{
				Type: ConditionReady, Status: metav1.ConditionFalse, Reason: "InProgress",
				LastTransitionTime: *atTime(-49 * time.Hour),
			}},
			Volumes: []cbv1.VolumeStatus{
				{Pvc: "a", FirstAttemptAt: atTime(-40 * time.Hour)},
				{Pvc: "b", FirstAttemptAt: atTime(-3 * time.Minute)}, // the newest sign of life
				{Pvc: "c"}, // never attempted; contributes nothing
			},
		},
	}
	if got, want := backupLastProgressAt(&b), abandonmentNow.Add(-3*time.Minute); !got.Equal(want) {
		t.Fatalf("backupLastProgressAt = %s; want the NEWEST sign of life %s", got, want)
	}

	// With no status at all, the object's own creation is the only honest floor.
	bare := cbv1.Backup{ObjectMeta: metav1.ObjectMeta{CreationTimestamp: *atTime(-2 * time.Hour)}}
	if got, want := backupLastProgressAt(&bare), abandonmentNow.Add(-2*time.Hour); !got.Equal(want) {
		t.Fatalf("backupLastProgressAt (no status) = %s; want %s", got, want)
	}
}

// TestClusterRunLastProgressAt pins the run-level clock, whose whole point is that it keeps moving
// on its CHILDREN's evidence: a ClusterBackup's own status stops changing once the fan-out is done,
// and every observable sign of progress after that belongs to a namespace.
func TestClusterRunLastProgressAt(t *testing.T) {
	cb := cbv1.ClusterBackup{
		ObjectMeta: metav1.ObjectMeta{CreationTimestamp: *atTime(-9 * time.Hour)},
		Status:     cbv1.ClusterBackupStatus{StartTime: atTime(-9 * time.Hour)},
	}
	children := []cbv1.Backup{
		{
			ObjectMeta: metav1.ObjectMeta{Namespace: "quiet", CreationTimestamp: *atTime(-9 * time.Hour)},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Namespace: "busy", CreationTimestamp: *atTime(-9 * time.Hour)},
			Status: cbv1.BackupStatus{
				Volumes: []cbv1.VolumeStatus{{Pvc: "d", FirstAttemptAt: atTime(-4 * time.Minute)}},
			},
		},
	}
	if got, want := clusterRunLastProgressAt(&cb, children), abandonmentNow.Add(-4*time.Minute); !got.Equal(want) {
		t.Fatalf("clusterRunLastProgressAt = %s; want the busiest child's clock %s", got, want)
	}
}

// TestScheduleAbandonmentGraceCoversTheDeadlineLadder is a pinning test on the ONE number in this
// decision, and it is here because the number is derived rather than chosen.
//
// The longest a single volume can legitimately hold a run without any observable progress is the
// controller's own ladder walked end to end: Pending until pendingResolveDeadline, Snapshotting
// until snapshotReadyDeadline, then a mover that never starts until moverStartDeadline — after
// which the controller has failed that volume itself and the run has moved on. A grace shorter than
// that ladder would let a schedule judge a run that is still inside the mechanism.
//
// If somebody lengthens one of those deadlines, this test is what tells them the grace has to move
// too.
func TestScheduleAbandonmentGraceCoversTheDeadlineLadder(t *testing.T) {
	ladder := pendingResolveDeadline + snapshotReadyDeadline + moverStartDeadline
	if scheduleAbandonmentGrace <= ladder {
		t.Fatalf("scheduleAbandonmentGrace (%s) must exceed the controller's own deadline ladder (%s = %s + %s + %s), "+
			"or a run still inside its own bounds can be terminated as abandoned",
			scheduleAbandonmentGrace, ladder, pendingResolveDeadline, snapshotReadyDeadline, moverStartDeadline)
	}
}
