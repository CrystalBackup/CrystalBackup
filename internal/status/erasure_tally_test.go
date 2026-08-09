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
// The tests for the erasure compliance record.
//
// There is one property here that outranks the others, and it is not "the numbers add up": it is
// that Forgotten NEVER exceeds what was established to be gone. A ClusterErasure is the artifact
// somebody points at to assert that data was destroyed, so an overstatement is a false attestation —
// while an understatement merely sends someone to look again. Every case below is checked against
// that asymmetry, not only against the arithmetic.
// ---------------------------------------------------------------------------------------------

// TestErasureRecordNeverOverstatesDestruction sweeps the whole residue range against a fixed scope
// and asserts the one invariant that matters: whatever the listing found, the record claims no more
// destruction than targeted-minus-residue, and never a negative or an inflated count.
func TestErasureRecordNeverOverstatesDestruction(t *testing.T) {
	const targeted = int32(10)
	for remaining := int32(0); remaining <= 20; remaining++ {
		rec := ErasureResidueObserved(targeted, remaining)
		if rec.Targeted != targeted {
			t.Fatalf("remaining=%d: Targeted=%d, want %d", remaining, rec.Targeted, targeted)
		}
		if rec.Remaining != remaining {
			t.Fatalf("remaining=%d: Remaining=%d — the observed residue must be published as observed",
				remaining, rec.Remaining)
		}
		if rec.Forgotten < 0 {
			t.Fatalf("remaining=%d: Forgotten=%d, negative", remaining, rec.Forgotten)
		}
		if want := max(targeted-remaining, 0); rec.Forgotten != want {
			t.Fatalf("remaining=%d: Forgotten=%d, want %d", remaining, rec.Forgotten, want)
		}
		if rec.Forgotten > targeted {
			t.Fatalf("remaining=%d: Forgotten=%d overstates a scope of %d", remaining, rec.Forgotten, targeted)
		}
		// The books balance for every residue within the scope, and only fail to balance when the
		// residue exceeds it — which is a real event (data written since the measurement), reported
		// rather than smoothed over.
		if remaining <= targeted && !rec.SumsUp() {
			t.Fatalf("remaining=%d: %+v does not sum up", remaining, rec)
		}
		if remaining > targeted && rec.SumsUp() {
			t.Fatalf("remaining=%d: %+v claims to balance against a scope of %d", remaining, rec, targeted)
		}
	}
}

// TestPartialErasureIsLegible is the case that was silently overstating. Ten snapshots targeted, the
// forget failed with six still there: the record must read 4 forgotten and 6 remaining — not the
// pre-erasure 10 the object used to publish beside a Failed phase.
func TestPartialErasureIsLegible(t *testing.T) {
	rec := ErasureResidueObserved(10, 6)
	if rec.Forgotten != 4 {
		t.Fatalf("Forgotten=%d, want 4: a failed erasure must report what actually went", rec.Forgotten)
	}
	if rec.Remaining != 6 {
		t.Fatalf("Remaining=%d, want 6: the record has to say what is left to erase", rec.Remaining)
	}
	if rec.Targeted != 10 || !rec.SumsUp() {
		t.Fatalf("%+v: 4 + 6 must account for the 10 targeted", rec)
	}
}

// TestForgetSucceededPruneFailed is the other half of a failed erasure, and it is the one an
// operator must not be alarmed about wrongly: the snapshots ARE gone, only their space was not
// reclaimed. The residue listing finds nothing, so the record credits the full scope even though the
// erasure ends Failed.
func TestForgetSucceededPruneFailed(t *testing.T) {
	rec := ErasureResidueObserved(7, 0)
	if rec.Forgotten != 7 || rec.Remaining != 0 {
		t.Fatalf("%+v: an empty residue means every targeted snapshot is gone", rec)
	}
	if !rec.SumsUp() {
		t.Fatalf("%+v does not sum up", rec)
	}
}

// TestUnverifiableErasureClaimsNothing covers the listing that could not be read. Nothing is
// established, so nothing is claimed — and, crucially, Remaining is the WHOLE scope rather than the
// unmeasured zero. Leaving it at zero would publish "nothing forgotten, nothing left", which reads
// as an empty repository: the same overstatement wearing a different field.
func TestUnverifiableErasureClaimsNothing(t *testing.T) {
	rec := ErasureResidueUnverifiable(10)
	if rec.Forgotten != 0 {
		t.Fatalf("Forgotten=%d, want 0: an unverifiable outcome establishes no destruction", rec.Forgotten)
	}
	if rec.Remaining != 10 {
		t.Fatalf("Remaining=%d, want 10: an unread residue must read as everything still there", rec.Remaining)
	}
	if !rec.SumsUp() {
		t.Fatalf("%+v does not sum up", rec)
	}
}

// TestRunningErasureClaimsNothingYet pins the record an erasure holds while its forget is in flight —
// which is also the record a crash leaves behind. The old code wrote the pre-erasure count into
// snapshotsForgotten at exactly this point, so an operator reading a Running (or crashed) object saw
// a destruction that had not begun.
func TestRunningErasureClaimsNothingYet(t *testing.T) {
	rec := ErasureScopeMeasured(10)
	if rec.Forgotten != 0 {
		t.Fatalf("Forgotten=%d, want 0: nothing has been erased when the scope is merely measured", rec.Forgotten)
	}
	if rec.Targeted != 10 || rec.Remaining != 10 || !rec.SumsUp() {
		t.Fatalf("%+v: the whole measured scope must still be reported as present", rec)
	}
}

// TestFullyForgottenErasure is the success path: forget and prune both reported success over the
// measured scope, so the whole scope is credited and nothing is left.
func TestFullyForgottenErasure(t *testing.T) {
	rec := ErasureFullyForgotten(10)
	if rec.Forgotten != 10 || rec.Remaining != 0 || !rec.SumsUp() {
		t.Fatalf("%+v: a completed erasure accounts for its whole scope", rec)
	}
	// An erasure that matched nothing is a legitimate success, and its record is empty in every
	// field rather than absent.
	if empty := ErasureFullyForgotten(0); empty.Forgotten != 0 || empty.Remaining != 0 || !empty.SumsUp() {
		t.Fatalf("%+v: an empty scope must produce an empty, consistent record", empty)
	}
}
