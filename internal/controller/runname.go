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
	"slices"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

// ---------------------------------------------------------------------------
// WHY a coordinate was refused — the instrumentation half.
//
// Nine consecutive nights of a production ClusterBackup came back PartiallyFailed with
// namespacesBlocked: 32, beside 32 namespace Backups reading Completed over 28 of 29 volumes.
// The counters were honest and the object was useless: status.failures held ten copies of one
// prose sentence, and the prose names the branch's CONCLUSION ("it is the terminal record of an
// earlier backup") without a single fact that led there. Which branch fired could not be counted,
// because all four render the same reason and differ only in free text a reword would break. The
// occupant's own stamp reached the message in exactly one branch. Whether the occupant held
// results never reached it at all. And the run object's creationTimestamp — the discriminator the
// candidate fix in the backlog turns on — was not even an INPUT here, so no amount of production
// evidence could confirm or refute it.
//
// The stated cause is already falsified: the same disagreement was seen on a fresh run whose
// children carried the correct stamp. So the trigger is an unenumerated set, and the thing to
// build is not a wider adoption window (that reopens d3d2659's guard, see the type doc above) but
// a record good enough that the next run to hit it enumerates the path instead of hypothesising
// one.
//
// Nothing below decides anything. classifyCoordinate's branch conditions and their order are
// unchanged, byte for byte; the codes are attached to branches that already existed.
// ---------------------------------------------------------------------------

// Coordinate classification codes: one STABLE token per branch of classifyCoordinate.
//
// They are deliberately not derived from the prose detail beside them. The detail is written for a
// human and may be reworded in any release; these are keys — ClusterBackupStatus.BlockedReasons is
// aggregated by them, and an operator diffs one night's breakdown against the next's. A reword that
// silently split a bucket in two would destroy exactly the comparison this exists for.
const (
	coordinateCodeMine                 = "OwnChild"
	coordinateCodeAdoptable            = "AdoptableUnstamped"
	coordinateCodeForeignParent        = "ForeignParentUID"
	coordinateCodeProjection           = "DiscoveryProjection"
	coordinateCodeUnstampedTerminal    = "UnstampedTerminalChild"
	coordinateCodeUnstampedWithResults = "UnstampedWithResults"
)

// How the occupant's own parent-UID stamp relates to the owner asking. Three values, because the
// interesting distinction is not "stamped or not" — the falsified hypothesis was precisely that
// unstamped was the only way in — but WHOSE stamp it is.
const (
	coordinateStampMine  = "mine"
	coordinateStampOther = "other"
	coordinateStampNone  = "none"
)

// How a boolean fact renders, and how an absent one does. Declared rather than inlined for the same
// reason the codes above are: the fact list is a vocabulary an operator greps and a future lot
// parses, so every token in it is named in one place.
const (
	coordinateFactYes     = "yes"
	coordinateFactNo      = "no"
	coordinateFactUnknown = "unknown"
	// coordinateFactNoPhase is what an occupant with an empty status.phase renders as. It is spelled
	// out rather than left blank because "phase=" followed by a space is indistinguishable from a
	// truncation, and this whole file exists because a record was ambiguous.
	coordinateFactNoPhase = "none"
)

// coordinateReason is everything classifyCoordinate SAW, not just what it concluded.
//
// The prose Detail is kept verbatim (it is what an administrator reads, and the crucible pins it),
// and the facts travel beside it rather than inside it, so the two can be truncated, aggregated and
// reworded independently.
type coordinateReason struct {
	// Code is the branch actually reached: one of the coordinateCode* tokens.
	Code string
	// Detail is the human-readable cause, empty for the two non-collision answers.
	Detail string
	// Stamp is the occupant's own parent-UID relation to the owner: mine, other, none.
	Stamp string
	// Phase is the occupant's status.phase verbatim, "" included.
	Phase string
	// HasResults is backupHasResults(occupant): whether the coordinate holds durable evidence that
	// a snapshot set landed under this name. It is the counter-side honesty — a run reporting a
	// namespace blocked while the coordinate holds data is asserting something very different from
	// one reporting it blocked over an empty coordinate.
	HasResults bool
	// Skew is the occupant's creationTimestamp MINUS the owner's, truncated to the second, and
	// SkewKnown says whether both were available. Negative means the occupant predates the run
	// object. This is here because it is the discriminator the backlog's candidate fix ("accept a
	// child whose creationTimestamp is at or after the run object's") turns on: recording it costs
	// nothing and lets the lot that takes that fix argue from measurements instead of from a
	// property of run names that only LOOKS sound.
	Skew      time.Duration
	SkewKnown bool
}

// Facts renders the discriminators as a compact, greppable key=value list in a fixed order. It is
// what goes into the collision message and the log, and it is deliberately terse: it shares a
// clusterBackupMessageCap-rune status message with prose that is already some 200 runes long, and
// the cap had to be raised once already to fit it.
func (r coordinateReason) Facts() string {
	phase := r.Phase
	if phase == "" {
		phase = coordinateFactNoPhase
	}
	data := coordinateFactNo
	if r.HasResults {
		data = coordinateFactYes
	}
	age := coordinateFactUnknown
	if r.SkewKnown {
		// Signed on purpose, and the sign is the whole point: "-" is an occupant older than the run
		// object (which the candidate fix would still refuse), "+" is one at or after it (which the
		// candidate fix would adopt). An unsigned duration would hide the only bit that matters.
		age = r.Skew.String()
		if r.Skew >= 0 {
			age = "+" + age
		}
	}
	return fmt.Sprintf("class=%s stamp=%s phase=%s data=%s age=%s", r.Code, r.Stamp, phase, data, age)
}

// newCoordinateReason gathers the facts about existing that do not depend on which branch fires, so
// the classifier and the aggregate's defence-in-depth check describe a coordinate identically.
func newCoordinateReason(existing *cbv1.Backup, owner types.UID, ownerCreated metav1.Time) coordinateReason {
	r := coordinateReason{
		Stamp:      coordinateStampNone,
		Phase:      existing.Status.Phase,
		HasResults: backupHasResults(existing),
	}
	// Mirrors classifyCoordinate's own first two branches, including their treatment of an EMPTY
	// owner UID: "" never counts as a match, so an unstamped occupant is never rendered as mine.
	if stamped := existing.Annotations[apiconst.AnnotationParentUID]; stamped != "" {
		r.Stamp = coordinateStampOther
		if stamped == string(owner) {
			r.Stamp = coordinateStampMine
		}
	}
	if !ownerCreated.IsZero() && !existing.CreationTimestamp.IsZero() {
		r.Skew = existing.CreationTimestamp.Sub(ownerCreated.Time).Truncate(time.Second)
		r.SkewKnown = true
	}
	return r
}

// classifyCoordinate decides whether existing is owner's own Backup, an adoptable
// pre-stamp remnant, or a foreign occupant of the coordinate. The returned coordinateReason carries
// the branch's stable code plus the facts it was decided on; its Detail is a short human-readable
// cause, empty when the answer is coordinateMine or coordinateAdoptable.
//
// ownerCreated is the OWNER OBJECT's creationTimestamp and is used for nothing but the record: no
// branch below reads it. It is a parameter rather than a field looked up later because the two
// callers hold different owner kinds, and because a fact gathered at the moment of the decision is
// the only kind that can be trusted to describe it.
func classifyCoordinate(existing *cbv1.Backup, owner types.UID, ownerCreated metav1.Time) (coordinateOwnership, coordinateReason) {
	r := newCoordinateReason(existing, owner, ownerCreated)
	stamped := existing.Annotations[apiconst.AnnotationParentUID]
	switch {
	case stamped != "" && stamped == string(owner):
		r.Code = coordinateCodeMine
		return coordinateMine, r
	case stamped != "":
		r.Code = coordinateCodeForeignParent
		r.Detail = "it was created by a different run (parent " + stamped + ")"
		return coordinateForeign, r
	case existing.Annotations[apiconst.AnnotationProjected] == apiconst.AnnotationProjectedValue:
		// A discovery projection: a materialized view of snapshots that already exist in
		// the repository. Its Completed volumes were derived from the repo, never executed.
		r.Code = coordinateCodeProjection
		r.Detail = "it is a discovery projection of snapshots already in the repository"
		return coordinateForeign, r
	case isTerminalBackupPhase(existing.Status.Phase):
		r.Code = coordinateCodeUnstampedTerminal
		r.Detail = "it is the terminal record of an earlier backup (phase " + existing.Status.Phase + ")"
		return coordinateForeign, r
	case backupHasResults(existing):
		r.Code = coordinateCodeUnstampedWithResults
		r.Detail = "it already holds results this run did not produce"
		return coordinateForeign, r
	default:
		r.Code = coordinateCodeAdoptable
		return coordinateAdoptable, r
	}
}

// blockedCoordinate is what the fan-out learned about ONE namespace it refused to back up. It is
// the value the collision map carries from the fan-out to the aggregate, and it exists because the
// map used to carry a single pre-clamped string: by the time the aggregate saw it, the cause was
// unrecoverable prose and the aggregate could add nothing to it.
type blockedCoordinate struct {
	// reason is the coordinateCode* the classifier reached.
	reason string
	// hasData is whether the occupant held results AT FAN-OUT TIME.
	hasData bool
	// err is the collision as raised, unclamped, so the aggregate can re-render it with what it
	// learns one pass later instead of appending to a string that is already at its cap. A VALUE and
	// not a pointer: the aggregate dereferences it on a path that runs for every blocked namespace of
	// every run, and "cannot be nil" is worth more here than the copy costs.
	err runNameCollisionError
}

// blockedNamespaceFacts is one blocked namespace's contribution to the run's breakdown: the cause,
// and the two counter-side facts an administrator needs in order to know whether "blocked" meant
// "unprotected".
type blockedNamespaceFacts struct {
	reason string
	// dataAtCoordinate: something holding snapshots sits at this coordinate. The run did not write
	// it and will not claim it — but "blocked" beside "there is a backup here" is a very different
	// report from "blocked" beside an empty coordinate, and the nine-night archive is entirely the
	// first kind.
	dataAtCoordinate bool
	// stampedByRun: the object at the coordinate, re-read at aggregation time, carries THIS run's
	// own UID. That combination is the live observation that falsified the backlog entry's stated
	// cause, and it had no way to reach the object before.
	stampedByRun bool
}

// summariseBlockedReasons folds one entry per blocked namespace into the run's per-cause breakdown.
//
// Its length is bounded by the number of coordinateCode* tokens — a closed set — and NOT by the
// number of namespaces, which is what makes it publishable on a run that fans out to hundreds.
// Sorted by cause so the field does not churn between passes and two nights can be diffed.
//
// nil for no blocked namespace, so a healthy run carries no field at all rather than an empty list.
func summariseBlockedReasons(facts []blockedNamespaceFacts) []cbv1.BlockedReason {
	if len(facts) == 0 {
		return nil
	}
	byReason := make(map[string]*cbv1.BlockedReason, len(facts))
	for _, f := range facts {
		e, ok := byReason[f.reason]
		if !ok {
			e = &cbv1.BlockedReason{Reason: f.reason}
			byReason[f.reason] = e
		}
		e.Namespaces++
		if f.dataAtCoordinate {
			e.WithDataAtCoordinate++
		}
		if f.stampedByRun {
			e.StampedByThisRun++
		}
	}
	out := make([]cbv1.BlockedReason, 0, len(byReason))
	for _, e := range byReason {
		out = append(out, *e)
	}
	slices.SortFunc(out, func(a, b cbv1.BlockedReason) int { return strings.Compare(a.Reason, b.Reason) })
	return out
}

// blockedBreakdownLine renders a per-cause breakdown for one Event/log line: "cause=n, cause=n".
// Bounded by the code set, like the field it mirrors.
func blockedBreakdownLine(summary []cbv1.BlockedReason) string {
	parts := make([]string, 0, len(summary))
	for _, e := range summary {
		parts = append(parts, fmt.Sprintf("%s=%d", e.Reason, e.Namespaces))
	}
	return strings.Join(parts, ", ")
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
	// Facts is coordinateReason.Facts(): the discriminators the classification was reached on. The
	// aggregate appends its own recheck token to it, which is why it is a plain string here rather
	// than the struct.
	Facts string
	// Reason is the coordinateCode* token, carried separately from the rendered Facts because the
	// run's breakdown is AGGREGATED by it and a substring match on a message is not an aggregation.
	Reason string
	// HasData is whether the occupant held results when the collision was raised — the fact behind
	// BlockedReason.WithDataAtCoordinate.
	HasData bool
}

// Error orders its sentences deliberately: WHY the classifier answered as it did, then what did NOT
// happen, then what to do, and only then WHICH object is in the way. The message is clamped to
// clusterBackupMessageCap runes before it reaches a status field, and a namespace and a Backup name
// are each up to 253 characters — so something has to be truncatable. The occupant's identity is the
// right thing to lose: it is already carried structurally in FailureRecord.Namespace and .Backup,
// and the full untruncated text goes out on the log line for this namespace. What must never be
// truncated away is the part an operator cannot reconstruct from the object: the facts the verdict
// turned on, that nothing was backed up, and what to do about it.
//
// The facts sit in front for exactly that reason. They used to not exist, and the nine-night archive
// that produced this change is ten identical prose sentences with the diagnosis truncated off the
// end — putting them after the boilerplate would have reproduced that outcome precisely.
func (e *runNameCollisionError) Error() string {
	facts := ""
	if e.Facts != "" {
		facts = "[" + e.Facts + "]"
	}
	return fmt.Sprintf(
		"%s%s: nothing was backed up here — this run name already designates data this run did not write. "+
			"Re-run under a name no earlier run or schedule has used (both planes build run names "+
			"identically). Occupant: %s/%s, %s",
		reasonRunNameCollision, facts, e.Namespace, e.Name, e.Detail)
}

// asRunNameCollision unwraps err to a *runNameCollisionError, or nil.
func asRunNameCollision(err error) *runNameCollisionError {
	var c *runNameCollisionError
	if errors.As(err, &c) {
		return c
	}
	return nil
}
