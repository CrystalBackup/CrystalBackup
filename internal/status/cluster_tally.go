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

// This file exists because of one production incident, and it is worth spelling out because the
// shape of everything below follows from it.
//
// A nightly ClusterBackup over 33 namespaces reported `namespacesFailed: 32` while its 33 child
// Backups read 29 Completed, 3 PartiallyFailed and 1 Pending. The number was false twice over: 32
// is not 3, and it called namespaces that had finished successfully failures. The cause was not
// arithmetic. It was that the run's counters had TWO sources of truth — the child phases, and a
// map of namespaces the fan-out had refused to touch — and each of them incremented its own
// `st.NamespacesFailed++` at a different point in the loop. Nothing anywhere checked that the
// buckets still added up to the namespaces being counted, and nothing distinguished "this run
// backed this namespace up and it failed" from "this run never backed this namespace up at all".
// Both landed in the same field, and an administrator reading it could not tell which had
// happened — nor reconcile the number with the children sitting right next to it.
//
// So: one classification, used by BOTH the counters and the phase, over one pass, with a total
// that is checked. A namespace's outcome is decided exactly once; the phase is derived from the
// same outcomes the counters are; and the four buckets are exhaustive by construction, so the sum
// is a property of the type rather than a convention a future edit can break.

// NamespaceOutcome is what a ClusterBackup run has to say about ONE namespace it is accountable
// for. It is the single classification behind both the run's counters and the run's phase: they
// are computed from the same values, in the same pass, so the headline phase can never contradict
// the numbers printed beside it.
//
// The four members are exhaustive and mutually exclusive — every namespace the run is accountable
// for lands in exactly one — which is what makes the total assertable (see ClusterTally.SumsUp).
type NamespaceOutcome int

const (
	// NamespaceInFlight: no verdict yet. The child has not been observed, or its phase is one of
	// the in-progress phases. An unrecognized phase string also lands here, deliberately: an
	// unknown phase is the absence of a verdict, not a good one, and holding the run non-terminal
	// is recoverable where declaring it Completed is not.
	NamespaceInFlight NamespaceOutcome = iota
	// NamespaceSucceeded: the run's child in this namespace finished with data in the repository
	// (Completed or PartiallyCompleted).
	//
	// A namespace whose only PVC is permanently unsnapshottable belongs HERE, not in a "partial"
	// bucket. That follows from RollUpVolumePhases one level down, where a Skipped volume is
	// NEUTRAL: it produces a Completed child precisely so that a namespace holding one
	// CSISnapshotUnsupported PVC does not alarm on every single run, forever. The cluster level
	// must not undo that by inventing a degradation the namespace level deliberately refused to
	// report.
	NamespaceSucceeded
	// NamespaceFailed: the run's child in this namespace RAN and failed (Failed or
	// PartiallyFailed). This is the only bucket that asserts something about a backup this run
	// actually performed, and it is derived from nothing but the child's own phase.
	NamespaceFailed
	// NamespaceBlocked: the run never backed this namespace up. The coordinate was occupied by a
	// Backup the run did not create (a previous run's terminal record, a discovery projection, or
	// another plane's Backup), or the fan-out could not create the child at all.
	//
	// This is the bucket the incident above was missing. It is NOT a child-phase verdict: there is
	// no child of this run in that namespace whose status could be read, and the object sitting at
	// the coordinate belongs to somebody else — which is exactly why counting it as a FAILURE put
	// the run's number in flat contradiction with a child object reading Completed. It is also not
	// benign: nothing protected that namespace this run, so it degrades the phase just as a
	// failure does. It is named "blocked" and not "skipped" on purpose — Skipped already means
	// "neutral, does not alarm" one level down, and this is the opposite of neutral.
	NamespaceBlocked
)

// The rendered names of a NamespaceOutcome, declared rather than inlined.
//
// They are this type's OWN constants and deliberately not borrowed from elsewhere in the package,
// even though two of the strings coincide with a VolumePhase and with the restore tally's in-flight
// name. A namespace outcome is not a volume phase and not a restore verdict; they answer different
// questions at different levels, and the fact that two of them spell a word the same way today is a
// coincidence, not a shared vocabulary. Reusing VolumePhaseFailed here would couple the rendering of
// a cluster-level verdict to the volume enum, so that widening one would silently move the other.
const (
	namespaceOutcomeSucceededName = "Succeeded"
	namespaceOutcomeFailedName    = "Failed"
	namespaceOutcomeBlockedName   = "Blocked"
	namespaceOutcomeInFlightName  = "InFlight"
)

// String renders an outcome for logs and events.
func (o NamespaceOutcome) String() string {
	switch o {
	case NamespaceSucceeded:
		return namespaceOutcomeSucceededName
	case NamespaceFailed:
		return namespaceOutcomeFailedName
	case NamespaceBlocked:
		return namespaceOutcomeBlockedName
	default:
		return namespaceOutcomeInFlightName
	}
}

// OutcomeForBackupPhase maps one child Backup's status.phase to its namespace outcome. It is the
// ONLY place a Backup phase becomes a cluster-level verdict; RollUpNamespaceOutcomes and the
// run's counters both go through it, so a phase added to the Backup enum is classified once.
//
// NamespaceBlocked is unreachable from here by design: no Backup phase means "this run never ran
// here". That fact comes from the fan-out, not from a child's status, and the caller supplies it
// directly.
func OutcomeForBackupPhase(phase string) NamespaceOutcome {
	switch phase {
	case string(BackupPhaseCompleted), string(BackupPhasePartiallyCompleted):
		return NamespaceSucceeded
	case string(BackupPhaseFailed), string(BackupPhasePartiallyFailed):
		return NamespaceFailed
	default:
		// "", Pending, SnapshottingHooks, Snapshotting, Uploading — and anything unrecognized.
		return NamespaceInFlight
	}
}

// OutcomesForBackupPhases maps a list of child phases in order. It is the bridge for callers that
// hold phase strings rather than outcomes.
func OutcomesForBackupPhases(childPhases []string) []NamespaceOutcome {
	out := make([]NamespaceOutcome, 0, len(childPhases))
	for _, p := range childPhases {
		out = append(out, OutcomeForBackupPhase(p))
	}
	return out
}

// ClusterTally is a ClusterBackup run's namespace accounting: how many namespaces the run is
// accountable for, and how they split across the four outcomes. The counts are int32 because that
// is what ClusterBackupStatus stores.
//
// Namespaces is recorded independently of the four buckets — it is the LENGTH of the outcome list
// the tally was built from, not their sum — so that SumsUp is a real check and not a tautology.
type ClusterTally struct {
	// Namespaces the run is accountable for: the number of outcomes folded in.
	Namespaces int32
	Succeeded  int32
	Failed     int32
	Blocked    int32
	InFlight   int32
}

// Counted is the sum of the four buckets.
func (t ClusterTally) Counted() int32 {
	return t.Succeeded + t.Failed + t.Blocked + t.InFlight
}

// SumsUp reports whether the buckets account for every namespace folded in. It is the invariant
// the incident violated: a counter that does not add up is a counter that is describing something
// other than the objects it claims to summarise. TallyNamespaceOutcomes guarantees it by
// construction; callers still check it, because a status field an administrator acts on is worth
// one comparison, and the day it goes false is the day the operator must say so rather than
// publish the number anyway.
func (t ClusterTally) SumsUp() bool {
	return t.Counted() == t.Namespaces
}

// TallyNamespaceOutcomes folds a run's per-namespace outcomes into its counters in ONE pass.
//
// This is the whole point of the type: the counters are a pure function of one ordered list of
// verdicts, so there is no second increment site to fall out of step with the first, and no way
// for a bucket to move without the total moving with it.
func TallyNamespaceOutcomes(outcomes []NamespaceOutcome) ClusterTally {
	t := ClusterTally{Namespaces: int32(len(outcomes))}
	for _, o := range outcomes {
		switch o {
		case NamespaceSucceeded:
			t.Succeeded++
		case NamespaceFailed:
			t.Failed++
		case NamespaceBlocked:
			t.Blocked++
		default: // NamespaceInFlight, and any value a future edit adds without a bucket.
			t.InFlight++
		}
	}
	return t
}

// RollUpNamespaceOutcomes maps a run's per-namespace outcomes to the run's aggregate phase. It is
// the primitive RollUpBackupPhases is written in terms of, and it takes the SAME values the
// counters are tallied from — which is what makes "phase Completed beside a non-zero failure
// count" unrepresentable rather than merely unlikely.
//
// The mapping, in evaluation order:
//
//	len == 0                     -> Pending  (nothing fanned out yet)
//	any InFlight                 -> Running  (at least one namespace has no verdict, even
//	                                          alongside a failed sibling)
//	Failed + Blocked == 0        -> Completed
//	Succeeded == 0               -> Failed
//	otherwise                    -> PartiallyFailed
//
// Blocked counts against the run exactly as Failed does. A namespace nothing backed up is not a
// namespace that came out fine, whatever the object squatting on its coordinate says about itself:
// a run that quietly reported Completed over an unprotected namespace is the one outcome a DR
// fan-out cannot have.
func RollUpNamespaceOutcomes(outcomes []NamespaceOutcome) ClusterBackupPhase {
	if len(outcomes) == 0 {
		return ClusterBackupPhasePending
	}
	t := TallyNamespaceOutcomes(outcomes)
	switch {
	case t.InFlight > 0:
		return ClusterBackupPhaseRunning
	case t.Failed+t.Blocked == 0:
		return ClusterBackupPhaseCompleted
	case t.Succeeded == 0:
		return ClusterBackupPhaseFailed
	default:
		return ClusterBackupPhasePartiallyFailed
	}
}
