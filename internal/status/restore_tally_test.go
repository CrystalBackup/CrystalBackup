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

import "testing"

// ---------------------------------------------------------------------------------------------
// The tests for a restore's per-volume accounting.
//
// They are written in the order the ClusterBackup incident taught: the INVARIANT first, then the
// buckets read back against the per-volume verdicts themselves — because in that incident the
// published numbers DID add up (0 + 32 + 1 = 33) and were false anyway. Summing correctly proves
// only that nothing was dropped; it says nothing about whether the buckets are counting the right
// volumes, which is what TestRestoreTallyMatchesTheVolumeVerdicts is for.
// ---------------------------------------------------------------------------------------------

// TestRestoreTallyAlwaysSumsUp sweeps every mix of 0..3 volumes per outcome — 64 mixes — rather than
// a handful of hand-picked ones, so an outcome added later without a bucket fails here.
func TestRestoreTallyAlwaysSumsUp(t *testing.T) {
	all := []VolumeRestoreOutcome{VolumeRestoreInFlight, VolumeRestoreRestored, VolumeRestoreFailed}
	for a := 0; a <= 3; a++ {
		for b := 0; b <= 3; b++ {
			for c := 0; c <= 3; c++ {
				counts := []int{a, b, c}
				var outcomes []VolumeRestoreOutcome
				for i, n := range counts {
					for range n {
						outcomes = append(outcomes, all[i])
					}
				}
				got := TallyVolumeRestoreOutcomes(outcomes)
				if !got.SumsUp() {
					t.Fatalf("mix %v: buckets do not add up: planned=%d counted=%d (%+v)",
						counts, got.Planned, got.Counted(), got)
				}
				if int(got.Planned) != len(outcomes) {
					t.Fatalf("mix %v: Planned=%d, want %d", counts, got.Planned, len(outcomes))
				}
				if int(got.InFlight) != a || int(got.Restored) != b || int(got.Failed) != c {
					t.Fatalf("mix %v: got %+v", counts, got)
				}
				// Settled is the progress numerator both controllers gate the terminal write on. A
				// restore that went terminal with a volume still in flight would tear down the mover
				// Jobs that are the volume's only durable state.
				if int(got.Settled()) != b+c {
					t.Fatalf("mix %v: Settled()=%d, want %d", counts, got.Settled(), b+c)
				}
			}
		}
	}
}

// TestRestoreTallyMatchesTheVolumeVerdicts is the test the incident actually needed: it reads the
// published buckets back against the per-volume verdicts, one volume at a time, instead of trusting
// that a total which adds up is a total that is true.
//
// The mix is the realistic one an operator meets mid-restore and could not previously see: some
// volumes back, some lost, some still moving.
func TestRestoreTallyMatchesTheVolumeVerdicts(t *testing.T) {
	// Nine volumes: 4 restored, 2 failed, 3 still in flight — deliberately in an interleaved order,
	// because a tally that depended on grouping would be a tally that depended on map iteration.
	outcomes := []VolumeRestoreOutcome{
		VolumeRestoreRestored,
		VolumeRestoreInFlight,
		VolumeRestoreFailed,
		VolumeRestoreRestored,
		VolumeRestoreInFlight,
		VolumeRestoreRestored,
		VolumeRestoreFailed,
		VolumeRestoreInFlight,
		VolumeRestoreRestored,
	}
	got := TallyVolumeRestoreOutcomes(outcomes)

	// Re-derive every bucket independently of the type under test, from the verdicts themselves.
	var restored, failed, inFlight int32
	for _, o := range outcomes {
		switch o {
		case VolumeRestoreRestored:
			restored++
		case VolumeRestoreFailed:
			failed++
		case VolumeRestoreInFlight:
			inFlight++
		}
	}
	if got.Restored != restored || got.Failed != failed || got.InFlight != inFlight {
		t.Fatalf("tally %+v disagrees with the volume verdicts: restored=%d failed=%d inFlight=%d",
			got, restored, failed, inFlight)
	}
	if got.Restored != 4 || got.Failed != 2 || got.InFlight != 3 || got.Planned != 9 {
		t.Fatalf("got %+v, want restored 4 / failed 2 / in flight 3 of 9 planned", got)
	}
	// And the reader's own arithmetic: what is still moving is the denominator minus what settled.
	if inflight := got.Planned - got.Restored - got.Failed; inflight != got.InFlight {
		t.Fatalf("planned-restored-failed = %d, but InFlight = %d", inflight, got.InFlight)
	}
}

// TestSumsUpDetectsADivergentRestoreTotal proves SumsUp is a real check and not a tautology over the
// struct's own fields. The constructor cannot produce a divergent tally; the controllers check
// anyway, and this is what that check is worth if a future edit ever hand-builds one.
func TestSumsUpDetectsADivergentRestoreTotal(t *testing.T) {
	consistent := RestoreTally{Planned: 9, Restored: 4, Failed: 2, InFlight: 3}
	if !consistent.SumsUp() {
		t.Fatal("a consistent tally must sum up")
	}
	// The exact shape of the incident: buckets that add up to something other than the volumes they
	// claim to describe.
	divergent := RestoreTally{Planned: 9, Restored: 4, Failed: 2, InFlight: 0}
	if divergent.SumsUp() {
		t.Fatalf("SumsUp accepted %+v: 6 counted for 9 planned", divergent)
	}
}

// TestNoOutcomeIsSilentlyDroppedAsSuccess guards the classification's default arm. An outcome value
// a future edit adds without a bucket must land in InFlight — the absence of a verdict — and never
// be absorbed into Restored, which is the only bucket whose overstatement tells somebody their data
// came back when it did not.
func TestNoOutcomeIsSilentlyDroppedAsSuccess(t *testing.T) {
	future := VolumeRestoreOutcome(99)
	got := TallyVolumeRestoreOutcomes([]VolumeRestoreOutcome{future, VolumeRestoreRestored})
	if got.Restored != 1 {
		t.Fatalf("Restored=%d, want 1: an unknown outcome must not count as a restored volume", got.Restored)
	}
	if got.InFlight != 1 {
		t.Fatalf("InFlight=%d, want 1: an unknown outcome is the absence of a verdict", got.InFlight)
	}
	if !got.SumsUp() {
		t.Fatalf("an unknown outcome broke the total: %+v", got)
	}
	if future.String() != "InFlight" {
		t.Fatalf("an unknown outcome renders as %q, want InFlight", future.String())
	}
}

// TestVolumeOutcomeStrings pins the three names that reach logs and events.
func TestVolumeOutcomeStrings(t *testing.T) {
	for _, tc := range []struct {
		outcome VolumeRestoreOutcome
		want    string
	}{
		{VolumeRestoreInFlight, "InFlight"},
		{VolumeRestoreRestored, "Restored"},
		{VolumeRestoreFailed, "Failed"},
	} {
		if got := tc.outcome.String(); got != tc.want {
			t.Errorf("String() = %q, want %q", got, tc.want)
		}
	}
}
