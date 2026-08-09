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

// This file is the restore side of the same lesson cluster_tally.go records, and it exists because a
// restore could not answer the three questions somebody asks while running one on their worst day:
// is it working, how far along is it, and what did not come back.
//
// Before it, a Restore/ClusterRestore published `restoredVolumes` and NOTHING ELSE about its
// volumes. Two consequences, both of them the reporting failure this release is about:
//
//   - There was no failed-volume count anywhere — not in status, not in the metrics. A restore that
//     brought back 7 of 9 volumes reported `restoredVolumes: 7` and left the reader to discover the
//     other two from the Ready condition's message, or from the mover Jobs, which are torn down
//     immediately afterwards. A count with no denominator and no failure bucket is not a report.
//   - `restoredVolumes` was written ONLY on the terminal pass. Every non-terminal pass wrote the
//     phase and the condition and left the counter at zero, so an operator watching a nine-volume
//     restore for forty minutes saw `0` the whole way and then `9`. The one number that could have
//     told them it was moving was the one number deliberately withheld until it no longer mattered.
//
// So the shape follows cluster_tally.go: ONE classification per volume, ONE pass, ONE tally whose
// buckets are exhaustive by construction, and a total that is asserted rather than assumed. The
// counters the controllers publish are then views of that single tally instead of three fields
// incremented from three places — which is the precise mechanism that let a ClusterBackup run
// publish 32 failed namespaces over children reading Completed.
//
// It is a SIBLING of ClusterTally rather than the same type on purpose. That one accounts for
// namespaces and carries a Blocked bucket, which has no meaning here: a restore's volumes come from
// its own plan, so there is no coordinate another object can be squatting on. Reusing the type
// would have meant publishing a field that is structurally always zero, and a reader is entitled to
// assume a published bucket can be non-zero.

// VolumeRestoreOutcome is what one reconcile pass has to say about ONE volume of a restore's plan.
// It is the single classification behind the run's volume counters: they are computed from these
// values, in the pass that produced them, so the counters cannot describe a different set of
// volumes than the one the engine just drove.
//
// The three members are exhaustive and mutually exclusive — every planned volume lands in exactly
// one, on every pass — which is what makes the total assertable (see RestoreTally.SumsUp).
type VolumeRestoreOutcome int

const (
	// VolumeRestoreInFlight: no verdict yet. The volume is being exposed, staged, or its mover is
	// still running — or the pass hit a transient error on it that has not yet exhausted the
	// volume's error budget. An unresolved error is the ABSENCE of a verdict, not a failure: the
	// next pass may well succeed, and holding the restore non-terminal is recoverable where
	// declaring a volume lost is not.
	VolumeRestoreInFlight VolumeRestoreOutcome = iota
	// VolumeRestoreRestored: the volume's data landed in the target claim and the volume needs no
	// further driving.
	VolumeRestoreRestored
	// VolumeRestoreFailed: the volume settled without its data — a failed mover, an unsupported
	// block target, a timed-out exposure, or an error budget the volume exhausted. This is the
	// bucket that did not exist at all, and it is the one an operator most needs: it is the answer
	// to "what did not come back".
	VolumeRestoreFailed
)

// The three rendered names, as named constants because they are a published vocabulary: they reach
// logs and events an operator greps, and the tests pin them.
const (
	volumeRestoreRestoredName = "Restored"
	volumeRestoreFailedName   = "Failed"
	volumeRestoreInFlightName = "InFlight"
)

// String renders an outcome for logs and events.
func (o VolumeRestoreOutcome) String() string {
	switch o {
	case VolumeRestoreRestored:
		return volumeRestoreRestoredName
	case VolumeRestoreFailed:
		return volumeRestoreFailedName
	default:
		return volumeRestoreInFlightName
	}
}

// RestoreTally is a restore's per-volume accounting: how many volumes the restore planned, and how
// they split across the three outcomes. The counts are int32 because that is what
// RestoreStatus/ClusterRestoreStatus store.
//
// Planned is recorded independently of the three buckets — it is the LENGTH of the outcome list the
// tally was built from, not their sum — so that SumsUp is a real check and not a tautology.
//
// It counts VOLUMES ONLY. A restore's manifest halves (resources[] and, on the cluster plane,
// clusterResources) are counted separately in status.restoredResources / status.resources
// .failedCount, while the terminal PHASE rolls up both halves together. That asymmetry is
// deliberate and is documented on the API fields themselves, where a reader of the number will see
// it: a restore whose volumes all landed and whose manifests failed reads Failed beside
// failedVolumes 0, and that has to be legible from the object rather than from folklore.
type RestoreTally struct {
	// Planned is the number of volumes this restore's plan covers: the number of outcomes folded in.
	Planned  int32
	Restored int32
	Failed   int32
	InFlight int32
}

// Counted is the sum of the three buckets.
func (t RestoreTally) Counted() int32 {
	return t.Restored + t.Failed + t.InFlight
}

// SumsUp reports whether the buckets account for every planned volume. TallyVolumeRestoreOutcomes
// guarantees it by construction; the controllers check it anyway, on every pass, because these are
// figures somebody acts on in the middle of a disaster — and the day it goes false is the day the
// operator has to say so rather than publish a number it cannot stand behind.
//
// Note what this check is and is NOT. In the ClusterBackup incident the published numbers DID add
// up (0 + 32 + 1 = 33) and were still false, because they were counting the wrong things. Summing
// correctly is necessary and nowhere near sufficient, which is why the tests for this type also
// read the buckets back against the per-volume outcomes themselves.
func (t RestoreTally) SumsUp() bool {
	return t.Counted() == t.Planned
}

// Settled is the number of volumes that need no further driving: restored plus failed. It is the
// progress numerator the controllers compare against Planned to decide whether the restore may go
// terminal, and it is a method rather than a fourth field so it can never fall out of step with the
// buckets it is made of.
func (t RestoreTally) Settled() int32 {
	return t.Restored + t.Failed
}

// TallyVolumeRestoreOutcomes folds one pass's per-volume outcomes into the restore's counters in
// ONE pass.
//
// This is the whole point of the type: the counters are a pure function of one ordered list of
// verdicts, so there is no second increment site to fall out of step with the first, and no way for
// a bucket to move without the total moving with it.
func TallyVolumeRestoreOutcomes(outcomes []VolumeRestoreOutcome) RestoreTally {
	t := RestoreTally{Planned: int32(len(outcomes))} //nolint:gosec // a restore's plan is capped at 128 volumes by the CRD.
	for _, o := range outcomes {
		switch o {
		case VolumeRestoreRestored:
			t.Restored++
		case VolumeRestoreFailed:
			t.Failed++
		default: // VolumeRestoreInFlight, and any value a future edit adds without a bucket.
			t.InFlight++
		}
	}
	return t
}
