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

// This file exists because a ClusterErasure could claim to have destroyed data it had not destroyed.
//
// The erasure controller measures its scope BEFORE erasing — it has to, since afterwards the
// evidence is gone — and it wrote that pre-erasure count straight into
// `status.snapshotsForgotten`, then never re-derived it. A forget that failed therefore left the
// object in a failed phase still reading `snapshotsForgotten: 10`.
//
// On any other object that would be a wrong counter. On this one it is a FALSE ATTESTATION. A
// ClusterErasure is the compliance record somebody points at to assert that data was destroyed — for
// a GDPR erasure request, a contractual deletion, a tenant offboarding — and a field that overstates
// destruction is the one defect a record like that must not have. Overstating is also not symmetric
// with understating: a record that under-claims sends someone to look again, while a record that
// over-claims ends the conversation.
//
// Hence the type below, and one rule that shapes all of it: every number an ErasureRecord publishes
// is either MEASURED after the work or is the conservative floor. It is never the optimistic
// assumption, and it never moves a snapshot into "forgotten" because an operation was expected to
// have removed it.

// ErasureRecord is a ClusterErasure's snapshot accounting: how many snapshots the erasure's scope
// covered, how many are established to be gone, and how many are still there. The counts are int32
// because that is what ClusterErasureStatus stores.
//
// Targeted is recorded independently of the two outcome buckets — it is the scope measured before
// the forget, not their sum — so that SumsUp is a real check and not a tautology.
//
// Every constructor in this file produces a record that sums up, at every phase of an erasure,
// including while it is still running (nothing forgotten yet, everything still there). The one
// exception is a residue listing that finds MORE snapshots than the scope measured, which is a real
// thing that can happen — new data written under the same target between the measurement and the
// verification — and which the controller reports rather than smooths over.
type ErasureRecord struct {
	// Targeted is how many snapshots matched the erasure's filter when the scope was measured,
	// before anything was removed. It is the denominator of the whole record.
	Targeted int32
	// Forgotten is how many of those snapshots are ESTABLISHED to be gone: either the forget+prune
	// reported success over the whole scope, or a post-failure listing found them absent. It is
	// never an expectation.
	Forgotten int32
	// Remaining is how many snapshots matching the filter are still in the repository. On a failed
	// erasure this is the number that says what is left to do; on an unverifiable one it is the
	// conservative assumption that nothing went.
	Remaining int32
}

// Counted is the sum of the two outcome buckets.
func (r ErasureRecord) Counted() int32 {
	return r.Forgotten + r.Remaining
}

// SumsUp reports whether the buckets account for every targeted snapshot. It is false in exactly one
// legitimate case — a residue larger than the measured scope, i.e. snapshots written under the same
// target since — and the controller says so instead of publishing a number it cannot reconcile.
func (r ErasureRecord) SumsUp() bool {
	return r.Counted() == r.Targeted
}

// ErasureScopeMeasured is the record at the moment the scope has been counted and NOTHING has been
// erased yet. Forgotten is zero and everything measured is still there, which is what an erasure
// parked in Running is entitled to claim: it is the state the object holds for as long as the
// forget+prune is in flight, and it is the state a crash leaves behind.
func ErasureScopeMeasured(targeted int32) ErasureRecord {
	return ErasureRecord{Targeted: targeted, Forgotten: 0, Remaining: targeted}
}

// ErasureFullyForgotten is the record of a forget+prune that reported success over the whole scope:
// every targeted snapshot is gone and nothing matching the filter is left.
//
// This is the ONE place the record trusts an operation's own success report rather than a listing,
// and the reason is that a verification listing cannot distinguish "not forgotten" from "written
// since": a namespace whose nightly backup ran between the forget and the check would show a residue
// on a genuinely complete erasure, and turning that into a partial record would be a new lie in the
// opposite direction. On the FAILURE path the same ambiguity biases the record toward claiming LESS
// destruction, which is the safe direction for an attestation, so there the listing is authoritative.
func ErasureFullyForgotten(targeted int32) ErasureRecord {
	return ErasureRecord{Targeted: targeted, Forgotten: targeted, Remaining: 0}
}

// ErasureResidueObserved is the record derived from a post-failure listing under the erasure's own
// filter: whatever the listing still finds is Remaining, and the rest of the measured scope is
// established as Forgotten.
//
// This is what makes a partial erasure legible — 4 of 10 forgotten reads "4 forgotten, 6 remaining"
// rather than "10 forgotten" on a failed object — and it is also what makes a forget that succeeded
// before its prune failed report the truth: the snapshots ARE gone, their space is not yet reclaimed.
//
// Forgotten has a floor of zero: a residue larger than the measured scope means snapshots were
// written under this target since the measurement, and the answer to that is to claim nothing was
// destroyed, not to publish a negative count or to invent destruction that would balance the books.
func ErasureResidueObserved(targeted, remaining int32) ErasureRecord {
	forgotten := max(targeted-remaining, 0)
	return ErasureRecord{Targeted: targeted, Forgotten: forgotten, Remaining: remaining}
}

// ErasureResidueUnverifiable is the record when the post-failure listing itself could not be read.
// Nothing is established, so nothing is claimed: zero forgotten, the whole measured scope possibly
// still there.
//
// The alternative — leaving Remaining at zero because it was not measured — would publish "nothing
// forgotten, nothing left", which reads as an empty repository and is the same overstatement in a
// different field. An erasure whose outcome is unknown must read as an erasure that did nothing.
func ErasureResidueUnverifiable(targeted int32) ErasureRecord {
	return ErasureRecord{Targeted: targeted, Forgotten: 0, Remaining: targeted}
}
