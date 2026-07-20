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

package mover

import (
	"fmt"
	"strings"
	"testing"
)

// The stake here is higher than it looks. The kubelet TRUNCATES an oversized termination
// message rather than rejecting it, so an over-budget result reaches the controller as
// malformed JSON — and ParseMoverResult (correctly) reads malformed JSON as "the mover died
// before reporting". A restore that actually succeeded would be recorded as a failure.

func entries(n int, outcome string) []ResourceEntry {
	out := make([]ResourceEntry, 0, n)
	for i := range n {
		out = append(out, ResourceEntry{
			Group: "apps", Kind: "Deployment", Name: fmt.Sprintf("workload-number-%03d", i),
			Outcome: outcome,
			Changed: []string{"spec.template.spec.containers", "spec.replicas"},
		})
	}
	return out
}

func TestFitKeepsResultUnderTheKubeletCap(t *testing.T) {
	for _, n := range []int{0, 1, 10, 100, 1000} {
		t.Run(fmt.Sprintf("%d entries", n), func(t *testing.T) {
			got := MoverResult{
				OK: true, Operation: string(OpManifestsRestore),
				RestoredResources: int32(n), ResourceEntries: entries(n, "Configured"),
			}.Fit()

			encoded, err := got.Encode()
			if err != nil {
				t.Fatalf("Encode() = %v", err)
			}
			if len(encoded) > TerminationMessageLimit {
				t.Errorf("encoded length %d exceeds the %d-byte kubelet cap", len(encoded), TerminationMessageLimit)
			}
			// Whatever survives must still parse — a half-written entry is worse than none.
			if _, err := ParseMoverResult(encoded); err != nil {
				t.Errorf("ParseMoverResult() = %v, want a clean round trip", err)
			}
		})
	}
}

func TestFitNeverDropsTheCounts(t *testing.T) {
	// The counts are what a controller reports as restoredResources/failedCount. They cost a
	// few bytes and they are the one thing that must survive any amount of trimming.
	got := MoverResult{
		OK: true, Operation: string(OpManifestsRestore),
		RestoredResources: 812, FailedResources: 7, SkippedResources: 40,
		ResourceEntries: entries(500, "Configured"),
	}.Fit()

	if got.RestoredResources != 812 || got.FailedResources != 7 || got.SkippedResources != 40 {
		t.Errorf("counts = %d/%d/%d, want 812/7/40",
			got.RestoredResources, got.FailedResources, got.SkippedResources)
	}
	if !got.ResourcesTruncated {
		t.Error("ResourcesTruncated = false; a report that dropped entries must say so")
	}
}

func TestFitEvictsSuccessesBeforeFailures(t *testing.T) {
	// A user staring at a partial restore needs the failures. The objects that merely got
	// updated are the ones to lose.
	mixed := append(entries(200, "Configured"), ResourceEntry{
		Group: "", Kind: "Service", Name: "web",
		Outcome: OutcomeFailedWire, Reason: "nodePort 30080 already allocated",
	})

	got := MoverResult{OK: true, Operation: string(OpManifestsRestore), ResourceEntries: mixed}.Fit()

	if !got.ResourcesTruncated {
		t.Fatal("expected this many entries to be truncated")
	}
	var keptFailure bool
	for _, e := range got.ResourceEntries {
		if e.Outcome == OutcomeFailedWire && e.Name == "web" {
			keptFailure = true
		}
	}
	if !keptFailure {
		t.Error("the failure was evicted while successes survived; failures must be kept last")
	}
}

func TestFitClampsAVerboseReason(t *testing.T) {
	// One admission webhook can return a paragraph. Unclamped, a single reason would evict
	// every other entry in the report.
	got := MoverResult{
		OK: true, Operation: string(OpManifestsRestore),
		ResourceEntries: []ResourceEntry{{
			Group: "apps", Kind: "Deployment", Name: "web",
			Outcome: OutcomeFailedWire, Reason: strings.Repeat("verbose webhook denial. ", 200),
		}},
	}.Fit()

	if len(got.ResourceEntries) != 1 {
		t.Fatalf("entries = %d, want the single failure kept", len(got.ResourceEntries))
	}
	if l := len(got.ResourceEntries[0].Reason); l > maxReasonLength+4 {
		t.Errorf("reason length %d, want it clamped near %d", l, maxReasonLength)
	}
}

func TestFitLeavesASmallReportAlone(t *testing.T) {
	// The common case: a handful of interesting outcomes, well under budget, reported whole.
	in := MoverResult{
		OK: true, Operation: string(OpManifestsRestore),
		RestoredResources: 141,
		ResourceEntries:   entries(3, "Configured"),
	}
	got := in.Fit()
	if got.ResourcesTruncated {
		t.Error("ResourcesTruncated = true for a report that fits")
	}
	if len(got.ResourceEntries) != 3 {
		t.Errorf("entries = %d, want all 3 kept", len(got.ResourceEntries))
	}
}
