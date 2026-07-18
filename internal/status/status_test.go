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
	"strconv"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
)

// TestPhaseConstantsPinned pins every phase constant to the exact string that the
// CRD +kubebuilder:validation:Enum markers declare (mirroring how internal/apiconst
// pins its keys). If someone edits a const here the wire value drifts from the CRD
// schema, and this test breaks loudly before that reaches a cluster.
func TestPhaseConstantsPinned(t *testing.T) {
	// Compile-time proof that VolumePhase is a true alias for the API type: an
	// api-typed variable accepts our const without conversion.
	var _ v1alpha1.VolumePhase = VolumePhaseCompleted //nolint:staticcheck // explicit type is the assertion: proves VolumePhase aliases the API type

	cases := []struct {
		got  string
		want string
	}{
		// VolumePhase — api/v1alpha1/common_types.go.
		{string(VolumePhasePending), "Pending"},
		{string(VolumePhaseSnapshotting), "Snapshotting"},
		{string(VolumePhaseUploading), "Uploading"},
		{string(VolumePhaseCompleted), "Completed"},
		{string(VolumePhaseSkipped), "Skipped"},
		{string(VolumePhaseFailed), "Failed"},

		// BackupPhase — api/v1alpha1/backup_types.go.
		{string(BackupPhasePending), "Pending"},
		{string(BackupPhaseSnapshottingHooks), "SnapshottingHooks"},
		{string(BackupPhaseSnapshotting), "Snapshotting"},
		{string(BackupPhaseUploading), "Uploading"},
		{string(BackupPhaseCompleted), "Completed"},
		{string(BackupPhasePartiallyCompleted), "PartiallyCompleted"},
		{string(BackupPhasePartiallyFailed), "PartiallyFailed"},
		{string(BackupPhaseFailed), "Failed"},

		// ClusterBackupPhase — api/v1alpha1/clusterbackup_types.go.
		{string(ClusterBackupPhasePending), "Pending"},
		{string(ClusterBackupPhaseRunning), "Running"},
		{string(ClusterBackupPhaseCompleted), "Completed"},
		{string(ClusterBackupPhasePartiallyFailed), "PartiallyFailed"},
		{string(ClusterBackupPhaseFailed), "Failed"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("phase constant = %q, want %q", c.got, c.want)
		}
	}

	if DefaultFailureCap != 10 {
		t.Errorf("DefaultFailureCap = %d, want 10", DefaultFailureCap)
	}
}

// vols builds a VolumeStatus slice from a list of phases, one dummy PVC per phase.
func vols(phases ...VolumePhase) []v1alpha1.VolumeStatus {
	out := make([]v1alpha1.VolumeStatus, len(phases))
	for i, p := range phases {
		out[i] = v1alpha1.VolumeStatus{Pvc: "pvc-" + strconv.Itoa(i), Phase: p}
	}
	return out
}

// TestRollUpVolumePhases is the per-PVC → Backup roll-up truth table. It covers
// every documented branch, including n==0, all-skipped, mixed skipped+completed,
// mixed failed+completed, all-failed, and the in-progress precedence
// (Uploading > Snapshotting > Pending) that must beat any terminal sibling.
func TestRollUpVolumePhases(t *testing.T) {
	tests := []struct {
		name string
		in   []v1alpha1.VolumeStatus
		want BackupPhase
	}{
		// n == 0 (manifests-only): Completed.
		{"nil-manifests-only", nil, BackupPhaseCompleted},
		{"empty-manifests-only", vols(), BackupPhaseCompleted},

		// Terminal: all completed.
		{"single-completed", vols(VolumePhaseCompleted), BackupPhaseCompleted},
		{"all-completed", vols(VolumePhaseCompleted, VolumePhaseCompleted), BackupPhaseCompleted},

		// Terminal: Skipped is NEUTRAL — it never lowers the outcome. With nothing
		// failed (f == 0) the result is always Completed, whatever the skipped/completed mix.
		{"all-skipped", vols(VolumePhaseSkipped, VolumePhaseSkipped), BackupPhaseCompleted},
		{"mixed-skipped-completed", vols(VolumePhaseCompleted, VolumePhaseSkipped), BackupPhaseCompleted},

		// Terminal: failures present. A Skipped volume saved no data, so it neither
		// softens a failure (failed+skipped stays Failed) nor is needed for a partial
		// (only a real Completed volume beside a failure yields PartiallyFailed).
		{"mixed-failed-completed", vols(VolumePhaseCompleted, VolumePhaseFailed), BackupPhasePartiallyFailed},
		{"failed-completed-skipped", vols(VolumePhaseFailed, VolumePhaseCompleted, VolumePhaseSkipped), BackupPhasePartiallyFailed},
		{"mixed-failed-skipped", vols(VolumePhaseSkipped, VolumePhaseFailed), BackupPhaseFailed},
		{"single-failed", vols(VolumePhaseFailed), BackupPhaseFailed},
		{"all-failed", vols(VolumePhaseFailed, VolumePhaseFailed), BackupPhaseFailed},

		// In-progress precedence; the "" phase counts toward Pending.
		{"inprogress-pending-only", vols(VolumePhasePending, VolumePhaseCompleted), BackupPhasePending},
		{"inprogress-empty-counts-as-pending", vols(VolumePhase(""), VolumePhaseCompleted), BackupPhasePending},
		{"inprogress-snapshotting-beats-pending", vols(VolumePhasePending, VolumePhaseSnapshotting), BackupPhaseSnapshotting},
		{"inprogress-uploading-beats-snapshotting", vols(VolumePhaseSnapshotting, VolumePhaseUploading), BackupPhaseUploading},
		{"inprogress-uploading-beats-pending", vols(VolumePhasePending, VolumePhaseUploading), BackupPhaseUploading},
		{"inprogress-precedence-all-three", vols(VolumePhasePending, VolumePhaseSnapshotting, VolumePhaseUploading), BackupPhaseUploading},

		// In-progress must win even when a sibling has already failed / been skipped.
		{"inprogress-uploading-beats-terminal-failed", vols(VolumePhaseUploading, VolumePhaseFailed), BackupPhaseUploading},
		{"inprogress-snapshotting-with-skipped", vols(VolumePhaseSnapshotting, VolumePhaseSkipped), BackupPhaseSnapshotting},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := RollUpVolumePhases(tc.in); got != tc.want {
				t.Errorf("RollUpVolumePhases(%s) = %q, want %q", tc.name, got, tc.want)
			}
		})
	}
}

// TestRollUpBackupPhases is the child-Backup → ClusterBackup roll-up truth table:
// empty (Pending), any-in-flight (Running, even beside a failed sibling), all-ok
// (Completed), all-bad (Failed), and mixed ok/bad (PartiallyFailed).
func TestRollUpBackupPhases(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want ClusterBackupPhase
	}{
		// Empty: Pending.
		{"nil-pending", nil, ClusterBackupPhasePending},
		{"empty-pending", []string{}, ClusterBackupPhasePending},

		// Any in-flight child: Running.
		{"running-pending-child", []string{"Pending", "Completed"}, ClusterBackupPhaseRunning},
		{"running-empty-child", []string{"", "Completed"}, ClusterBackupPhaseRunning},
		{"running-snapshottinghooks", []string{"SnapshottingHooks"}, ClusterBackupPhaseRunning},
		{"running-snapshotting", []string{"Snapshotting", "Completed"}, ClusterBackupPhaseRunning},
		{"running-uploading-beats-failed-sibling", []string{"Uploading", "Failed"}, ClusterBackupPhaseRunning},

		// All terminal, no failures: Completed (PartiallyCompleted still counts as ok).
		{"all-completed", []string{"Completed", "Completed"}, ClusterBackupPhaseCompleted},
		{"completed-with-partiallycompleted", []string{"Completed", "PartiallyCompleted"}, ClusterBackupPhaseCompleted},

		// All terminal, no successes: Failed (PartiallyFailed alone still means ok==0).
		{"all-failed", []string{"Failed", "Failed"}, ClusterBackupPhaseFailed},
		{"all-partiallyfailed-is-failed", []string{"PartiallyFailed", "PartiallyFailed"}, ClusterBackupPhaseFailed},

		// Mixed success and failure: PartiallyFailed.
		{"mixed-ok-bad", []string{"Completed", "Failed"}, ClusterBackupPhasePartiallyFailed},
		{"mixed-partials", []string{"PartiallyCompleted", "PartiallyFailed"}, ClusterBackupPhasePartiallyFailed},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := RollUpBackupPhases(tc.in); got != tc.want {
				t.Errorf("RollUpBackupPhases(%s) = %q, want %q", tc.name, got, tc.want)
			}
		})
	}
}

// TestSetConditionRecordsFields checks that a freshly set condition records every
// field, stamps a non-zero LastTransitionTime, is reported True by IsConditionTrue,
// and that absent types read as not-True / nil.
func TestSetConditionRecordsFields(t *testing.T) {
	var conds []metav1.Condition
	SetCondition(&conds, "Ready", metav1.ConditionTrue, "AllGood", "everything ok", 5)

	if !IsConditionTrue(conds, "Ready") {
		t.Fatalf("IsConditionTrue(Ready) = false, want true")
	}
	c := FindCondition(conds, "Ready")
	if c == nil {
		t.Fatalf("FindCondition(Ready) = nil, want a condition")
	}
	if c.Status != metav1.ConditionTrue || c.Reason != "AllGood" || c.Message != "everything ok" {
		t.Errorf("condition fields = {Status:%q Reason:%q Message:%q}", c.Status, c.Reason, c.Message)
	}
	if c.ObservedGeneration != 5 {
		t.Errorf("ObservedGeneration = %d, want 5", c.ObservedGeneration)
	}
	if c.LastTransitionTime.IsZero() {
		t.Errorf("LastTransitionTime is zero, want stamped by SetStatusCondition")
	}

	// A type that was never set is neither True nor found.
	if IsConditionTrue(conds, "Absent") {
		t.Errorf("IsConditionTrue(Absent) = true, want false")
	}
	if FindCondition(conds, "Absent") != nil {
		t.Errorf("FindCondition(Absent) != nil, want nil")
	}
}

// TestSetConditionMessageOnlyKeepsTransitionTime verifies that re-asserting the
// same Status with a fresher Reason/Message updates those fields but does NOT move
// LastTransitionTime — the property that keeps a no-op reconcile from churning the
// object.
func TestSetConditionMessageOnlyKeepsTransitionTime(t *testing.T) {
	var conds []metav1.Condition
	SetCondition(&conds, "Ready", metav1.ConditionTrue, "R1", "M1", 1)
	first := FindCondition(conds, "Ready").LastTransitionTime

	SetCondition(&conds, "Ready", metav1.ConditionTrue, "R2", "M2", 1)
	c := FindCondition(conds, "Ready")
	if !c.LastTransitionTime.Time.Equal(first.Time) {
		t.Errorf("LastTransitionTime moved on message-only change: got %v, first %v", c.LastTransitionTime, first)
	}
	if c.Message != "M2" || c.Reason != "R2" {
		t.Errorf("Reason/Message not updated: {Reason:%q Message:%q}", c.Reason, c.Message)
	}
}

// TestSetConditionStatusChangeBumpsTransitionTime verifies that a real Status
// transition bumps LastTransitionTime and records the new ObservedGeneration. A
// known past timestamp is seeded so the bump is unambiguous regardless of clock
// resolution.
func TestSetConditionStatusChangeBumpsTransitionTime(t *testing.T) {
	var conds []metav1.Condition
	SetCondition(&conds, "Ready", metav1.ConditionTrue, "R1", "M1", 1)

	past := metav1.NewTime(time.Now().Add(-time.Hour))
	FindCondition(conds, "Ready").LastTransitionTime = past

	SetCondition(&conds, "Ready", metav1.ConditionFalse, "R2", "M2", 2)
	c := FindCondition(conds, "Ready")
	if c.Status != metav1.ConditionFalse {
		t.Fatalf("Status = %q, want False", c.Status)
	}
	if !c.LastTransitionTime.After(past.Time) {
		t.Errorf("LastTransitionTime not bumped on status change: got %v, seeded %v", c.LastTransitionTime, past)
	}
	if c.ObservedGeneration != 2 {
		t.Errorf("ObservedGeneration = %d, want 2", c.ObservedGeneration)
	}
	if IsConditionTrue(conds, "Ready") {
		t.Errorf("IsConditionTrue(Ready) = true after transition to False")
	}
}

// fr builds a FailureRecord tagged with a namespace, so tests can assert WHICH
// records survived the cap.
func fr(ns string) v1alpha1.FailureRecord {
	return v1alpha1.FailureRecord{Namespace: ns}
}

// TestAppendCappedFailure exercises the boundaries: under cap (appends), at cap
// (drops the new record, keeps the FIRST cap), over cap (unchanged — never trims),
// and cap<=0 (falls back to DefaultFailureCap and enforces it as the ceiling).
func TestAppendCappedFailure(t *testing.T) {
	// Under cap: appends, new record lands last.
	got := AppendCappedFailure([]v1alpha1.FailureRecord{fr("0")}, fr("1"), 3)
	if len(got) != 2 || got[1].Namespace != "1" {
		t.Errorf("under cap: got %+v, want 2 records ending in ns=1", got)
	}

	// At cap: does not append, and keeps the FIRST cap records (not the newest).
	full := []v1alpha1.FailureRecord{fr("0"), fr("1"), fr("2")}
	got = AppendCappedFailure(full, fr("3"), 3)
	if len(got) != 3 {
		t.Fatalf("at cap: len = %d, want 3", len(got))
	}
	for i, want := range []string{"0", "1", "2"} {
		if got[i].Namespace != want {
			t.Errorf("at cap: record %d ns = %q, want %q (first records must be kept)", i, got[i].Namespace, want)
		}
	}

	// Over cap: already beyond cap, returned unchanged (no trim, no append).
	over := []v1alpha1.FailureRecord{fr("0"), fr("1"), fr("2"), fr("3"), fr("4")}
	got = AppendCappedFailure(over, fr("5"), 3)
	if len(got) != 5 {
		t.Errorf("over cap: len = %d, want 5 (unchanged)", len(got))
	}

	// cap == 0 falls back to DefaultFailureCap: 1 < 10, so it appends.
	got = AppendCappedFailure([]v1alpha1.FailureRecord{fr("0")}, fr("1"), 0)
	if len(got) != 2 {
		t.Errorf("cap=0 fallback: len = %d, want 2", len(got))
	}

	// cap < 0 behaves the same as cap == 0.
	got = AppendCappedFailure([]v1alpha1.FailureRecord{fr("0")}, fr("1"), -5)
	if len(got) != 2 {
		t.Errorf("cap<0 fallback: len = %d, want 2", len(got))
	}

	// cap <= 0 still enforces DefaultFailureCap as the ceiling: a full list is
	// not appended to.
	ten := make([]v1alpha1.FailureRecord, DefaultFailureCap)
	got = AppendCappedFailure(ten, fr("x"), 0)
	if len(got) != DefaultFailureCap {
		t.Errorf("cap=0 at DefaultFailureCap: len = %d, want %d", len(got), DefaultFailureCap)
	}
}
