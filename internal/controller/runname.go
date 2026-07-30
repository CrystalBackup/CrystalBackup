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

	"k8s.io/apimachinery/pkg/types"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
)

// ---------------------------------------------------------------------------
// Run-name coordinate ownership.
//
// A Backup's (namespace, name) is a COORDINATE, not an identity. Three different
// producers can land on the same one:
//
//   - the cluster-DR fan-out, which names every child after its ClusterBackup run;
//   - the namespace plane's cron, which names every stamped Backup with the SAME
//     apiconst.RunName(schedule, tick) function — so a ClusterBackupSchedule and a
//     BackupSchedule both named "daily", on the same cron, collide byte for byte in
//     every covered namespace, with nobody having done anything unusual;
//   - discovery, which PROJECTS a read-only view of the repository at (namespace, run)
//     and, once a run's own Backup is terminal, adopts it into that projection.
//
// Every one of those satisfies a bare Get on the coordinate. Reading "it exists, so
// it is mine" from that Get is how a run came to report success over snapshots it
// never wrote: it skipped the namespace, then aggregated the homonym's Completed
// volumes as its own. The only observable difference was an empty addedBytes.
//
// apiconst.AnnotationParentUID makes the test exact. The owning object's UID is
// stamped at creation; a UID is never reused, and a crash-restarted run keeps its CR
// and therefore its UID, so "same UID" means "the object I created, seen again" and
// nothing else. Anything else at the coordinate is a collision, and a collision is
// always loud: there is no reading under which "this name designates data I did not
// write" is a success.
// ---------------------------------------------------------------------------

// reasonRunNameCollision is the FailureRecord / condition / Event reason for a Backup
// coordinate that is already occupied by something this run did not create.
const reasonRunNameCollision = "RunNameCollision"

// coordinateOwnership classifies the Backup found at a run's (namespace, name).
type coordinateOwnership int

const (
	// coordinateMine: the object carries this owner's UID. A second reconcile pass, a
	// re-fired tick, a lost create race — the ordinary idempotent no-op.
	coordinateMine coordinateOwnership = iota
	// coordinateAdoptable: an UNSTAMPED, still-executing Backup that has demonstrably
	// written nothing yet (no backupTime, no snapshot, no bytes). This is the operator
	// UPGRADE path and only that: a child fanned out by a pre-stamp build whose run is
	// still in flight when the new binary takes over. Adopting it stamps the UID and
	// continues, because a coordinate holding no data cannot be "data this run did not
	// write". The window is deliberately narrow — anything terminal, projected, or
	// holding a snapshot ID is a collision, upgrade or not.
	coordinateAdoptable
	// coordinateForeign: a collision. A different UID, a discovery projection, or an
	// unstamped Backup that already holds results.
	coordinateForeign
)

// classifyCoordinate decides whether existing is owner's own Backup, an adoptable
// pre-stamp remnant, or a foreign occupant of the coordinate. detail is a short
// human-readable cause, empty when the answer is coordinateMine.
func classifyCoordinate(existing *cbv1.Backup, owner types.UID) (coordinateOwnership, string) {
	stamped := existing.Annotations[apiconst.AnnotationParentUID]
	switch {
	case stamped != "" && stamped == string(owner):
		return coordinateMine, ""
	case stamped != "":
		return coordinateForeign, "it was created by a different run (parent " + stamped + ")"
	case existing.Annotations[apiconst.AnnotationProjected] == apiconst.AnnotationProjectedValue:
		// A discovery projection: a materialized view of snapshots that already exist in
		// the repository. Its Completed volumes were derived from the repo, never executed.
		return coordinateForeign, "it is a discovery projection of snapshots already in the repository"
	case isTerminalBackupPhase(existing.Status.Phase):
		return coordinateForeign, "it is the terminal record of an earlier backup (phase " + existing.Status.Phase + ")"
	case backupHasResults(existing):
		return coordinateForeign, "it already holds results this run did not produce"
	default:
		return coordinateAdoptable, ""
	}
}

// backupHasResults reports whether a Backup has any durable evidence of execution: a
// stamped backupTime, or a volume carrying a snapshot ID or moved bytes. It is the
// "did anything land in the repository under this name" test that bounds adoption.
func backupHasResults(b *cbv1.Backup) bool {
	if b.Status.BackupTime != nil {
		return true
	}
	for i := range b.Status.Volumes {
		v := &b.Status.Volumes[i]
		if v.SnapshotID != "" || v.AddedBytes > 0 {
			return true
		}
	}
	return false
}

// runNameCollisionError is the typed error both planes raise when a Backup coordinate is
// occupied by something they did not create. It is a distinct type (not a formatted
// string) because the callers must be able to tell it from an ordinary API error: a
// collision is a permanent, per-namespace RESULT to record, not a transient fault to
// retry into a hot loop.
type runNameCollisionError struct {
	Namespace string
	Name      string
	Detail    string
}

// Error orders its sentences deliberately: what did NOT happen, then what to do, and only then
// WHICH object is in the way. The message is clamped to clusterBackupMessageCap runes before it
// reaches a status field, and a namespace and a Backup name are each up to 253 characters — so
// something has to be truncatable. The occupant's identity is the right thing to lose: it is
// already carried structurally in FailureRecord.Namespace and .Backup, and the full untruncated
// text goes out on the Event. What must never be truncated away is the part an operator cannot
// reconstruct from the object: that nothing was backed up, and what to do about it.
func (e *runNameCollisionError) Error() string {
	return fmt.Sprintf(
		"%s: nothing was backed up here — this run name already designates data this run did not write. "+
			"Re-run under a name no earlier run or schedule has used (both planes build run names "+
			"identically). Occupant: %s/%s, %s",
		reasonRunNameCollision, e.Namespace, e.Name, e.Detail)
}

// asRunNameCollision unwraps err to a *runNameCollisionError, or nil.
func asRunNameCollision(err error) *runNameCollisionError {
	var c *runNameCollisionError
	if errors.As(err, &c) {
		return c
	}
	return nil
}
