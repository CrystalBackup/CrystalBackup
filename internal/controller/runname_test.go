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
	"errors"
	"fmt"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
	"github.com/CrystalBackup/CrystalBackup/internal/status"
)

const (
	testOwnerUID = types.UID("11111111-1111-1111-1111-111111111111")
	testOtherUID = types.UID("22222222-2222-2222-2222-222222222222")
)

// backupWith builds a Backup carrying the given annotations, phase and volumes.
func backupWith(anns map[string]string, phase string, vols ...cbv1.VolumeStatus) *cbv1.Backup {
	return &cbv1.Backup{
		ObjectMeta: metav1.ObjectMeta{Namespace: "tenant", Name: "daily-20260730-030000", Annotations: anns},
		Status:     cbv1.BackupStatus{Phase: phase, Volumes: vols},
	}
}

func TestClassifyCoordinate(t *testing.T) {
	cases := []struct {
		name   string
		backup *cbv1.Backup
		want   coordinateOwnership
	}{{
		name:   "my own child seen again is an idempotent no-op",
		backup: backupWith(map[string]string{apiconst.AnnotationParentUID: string(testOwnerUID)}, ""),
		want:   coordinateMine,
	}, {
		// A crash-restarted run keeps its CR and therefore its UID, so this is also the
		// operator-restart case: the run must NOT declare a collision against itself.
		name: "my own child, already terminal, is still mine",
		backup: backupWith(map[string]string{apiconst.AnnotationParentUID: string(testOwnerUID)},
			string(status.BackupPhaseCompleted),
			cbv1.VolumeStatus{Pvc: "data", Phase: status.VolumePhaseCompleted, SnapshotID: "s1"}),
		want: coordinateMine,
	}, {
		// The reproduced vector: a ClusterBackup recreated at a name a previous run used.
		name: "a different owner's stamp is a collision",
		backup: backupWith(map[string]string{apiconst.AnnotationParentUID: string(testOtherUID)},
			string(status.BackupPhaseCompleted)),
		want: coordinateForeign,
	}, {
		name: "a discovery projection is a collision even with no stamp",
		backup: backupWith(map[string]string{apiconst.AnnotationProjected: apiconst.AnnotationProjectedValue},
			string(status.BackupPhaseCompleted),
			cbv1.VolumeStatus{Pvc: "data", Phase: status.VolumePhaseCompleted, SnapshotID: "s1"}),
		want: coordinateForeign,
	}, {
		name:   "an unstamped terminal Backup is a collision",
		backup: backupWith(nil, string(status.BackupPhasePartiallyFailed)),
		want:   coordinateForeign,
	}, {
		name: "an unstamped, non-terminal Backup that already holds a snapshot is a collision",
		backup: backupWith(nil, string(status.BackupPhaseUploading),
			cbv1.VolumeStatus{Pvc: "data", Phase: status.VolumePhaseCompleted, SnapshotID: "s1"}),
		want: coordinateForeign,
	}, {
		name: "an unstamped, non-terminal Backup that already moved bytes is a collision",
		backup: backupWith(nil, string(status.BackupPhaseUploading),
			cbv1.VolumeStatus{Pvc: "data", Phase: status.VolumePhaseUploading, AddedBytes: 1}),
		want: coordinateForeign,
	}, {
		// The ONLY adoption window: an upgrade caught a pre-stamp child mid-flight. It holds no
		// result of any kind, so the coordinate designates no data anyone could mistake for mine.
		name: "an unstamped, in-flight Backup holding no result at all is adoptable",
		backup: backupWith(nil, string(status.BackupPhasePending),
			cbv1.VolumeStatus{Pvc: "data", Phase: status.VolumePhasePending}),
		want: coordinateAdoptable,
	}, {
		name:   "an unstamped Backup with no status at all is adoptable",
		backup: backupWith(nil, ""),
		want:   coordinateAdoptable,
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, detail := classifyCoordinate(tc.backup, testOwnerUID)
			if got != tc.want {
				t.Fatalf("classifyCoordinate = %v, want %v (detail %q)", got, tc.want, detail)
			}
			if got == coordinateForeign && detail == "" {
				t.Fatal("a collision must carry a human-readable cause, not just a verdict")
			}
		})
	}
}

// TestClassifyCoordinateBackupTimeIsAResult: backupTime is stamped exactly once, when a Backup
// reaches a terminal phase, and is durable proof that a snapshot set landed under this name.
func TestClassifyCoordinateBackupTimeIsAResult(t *testing.T) {
	b := backupWith(nil, string(status.BackupPhaseUploading))
	now := metav1.Now()
	b.Status.BackupTime = &now
	if got, _ := classifyCoordinate(b, testOwnerUID); got != coordinateForeign {
		t.Fatalf("a Backup with a stamped backupTime must never be adopted: got %v", got)
	}
}

// TestRunNameCollisionErrorIsActionable: the operator reading this has to learn what happened and
// what to do, not decode a status code. The cross-plane name clash is named explicitly because it
// is the vector that needs no unusual action at all — two admins picking "daily".
func TestRunNameCollisionErrorIsActionable(t *testing.T) {
	err := &runNameCollisionError{Namespace: "tenant", Name: "daily-20260730-030000", Detail: "it is a discovery projection"}
	msg := err.Error()
	for _, want := range []string{
		reasonRunNameCollision,
		"tenant/daily-20260730-030000",
		"data this run did not write",
		"nothing was backed up",
		"Re-run under a name no earlier run or schedule has used",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("collision message must contain %q; got:\n%s", want, msg)
		}
	}
}

// TestAsRunNameCollisionUnwraps: the callers branch on the TYPE, so it has to survive wrapping.
func TestAsRunNameCollisionUnwraps(t *testing.T) {
	inner := &runNameCollisionError{Namespace: "ns", Name: "n", Detail: "d"}
	if got := asRunNameCollision(fmt.Errorf("fan out: %w", inner)); got != inner {
		t.Fatalf("asRunNameCollision did not unwrap a wrapped collision: %v", got)
	}
	if got := asRunNameCollision(errors.New("connection refused")); got != nil {
		t.Fatalf("asRunNameCollision matched an unrelated error: %v", got)
	}
}

// TestProjectedPhaseNeverRaisesARecordedResult: a projection is rebuilt from `restic snapshots`,
// which lists only what succeeded — so its opinion is always Completed. Letting that opinion win
// turned a PartiallyCompleted run into a Completed one within about thirty seconds, and the
// failure then existed nowhere in the cluster.
func TestProjectedPhaseNeverRaisesARecordedResult(t *testing.T) {
	for _, recorded := range []status.BackupPhase{
		status.BackupPhasePartiallyCompleted,
		status.BackupPhasePartiallyFailed,
		status.BackupPhaseFailed,
		status.BackupPhaseCompleted,
	} {
		if got := projectedPhase(string(recorded)); got != string(recorded) {
			t.Fatalf("projectedPhase(%q) = %q, want the recorded phase kept verbatim", recorded, got)
		}
	}
	// No recorded terminal result: the repository is the only truth there is.
	for _, recorded := range []string{"", string(status.BackupPhaseUploading)} {
		if got := projectedPhase(recorded); got != string(status.BackupPhaseCompleted) {
			t.Fatalf("projectedPhase(%q) = %q, want Completed", recorded, got)
		}
	}
}
