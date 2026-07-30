package controller

import (
	"strings"
	"testing"
)

// TestCollisionMessageSurvivesTheStatusClamp: the FailureRecord message is clamped to
// clusterBackupMessageCap runes. The clamp must not cut away the part that says what happened —
// an operator reading a truncated code and nothing else is exactly the failure this replaces.
func TestCollisionMessageSurvivesTheStatusClamp(t *testing.T) {
	longest := &runNameCollisionError{
		Namespace: "some-fairly-long-tenant-namespace",
		Name:      "dr-daily-20260730-020000",
		Detail:    "it was created by a different run (parent 3b282d8e-ca67-4a2d-bb4a-1e98f29c567c)",
	}
	got := clampMessage(longest.Error())
	// The invariant half — the part no operator can reconstruct from the object — must survive
	// the clamp whatever the namespace and name are. The occupant's identity may be cut: it is
	// carried structurally in FailureRecord.Namespace and .Backup.
	for _, want := range []string{
		reasonRunNameCollision,
		"nothing was backed up here",
		"already designates data this run did not write",
		"Re-run under a name no earlier run or schedule has used",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("the clamp cut away %q; message is:\n%s", want, got)
		}
	}
	if len([]rune(got)) > clusterBackupMessageCap {
		t.Fatalf("clamped message is %d runes, cap is %d", len([]rune(got)), clusterBackupMessageCap)
	}
}
