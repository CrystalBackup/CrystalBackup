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

package preflight

// These are the tests for preflight.sh's one OBSERVATIONAL check — the one that asks whether any
// VolumeSnapshot in this cluster is bound to a VolumeSnapshotContent and still not readyToUse.
//
// The incident behind it: on a production cluster no CephFS volume had ever been backed up
// successfully, and preflight.sh reported that StorageClass as perfectly usable, because a
// VolumeSnapshotClass for its driver existed. Every predictive check passed. The stuck snapshots were
// sitting in the API the whole time, visible in a single LIST.

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// snapshotFixtureKey is the fragment of the check's own kubectl invocation the stub matches on. It is
// deliberately the RESOURCE and not the whole command: a test that pinned the jsonpath would break on
// every field added to it and prove nothing about the classification.
const snapshotFixtureKey = "volumesnapshot -A"

// snapRow builds one line of the check's expected jsonpath output:
// creationTimestamp TAB namespace/name TAB class TAB boundContent TAB readyToUse.
//
// The age is passed as a duration and turned into an absolute UTC timestamp here, because the script
// computes ages against the REAL clock — it has no injectable "now", by design: it is a script an
// administrator runs, not a library. Relative fixtures are therefore the only stable ones.
func snapRow(age time.Duration, id, class, content, ready string) string {
	ts := time.Now().UTC().Add(-age).Format(time.RFC3339)
	return strings.Join([]string{ts, id, class, content, ready}, "\t")
}

func snapshotFixture(rows ...string) map[string]string {
	return map[string]string{snapshotFixtureKey: strings.Join(rows, "\n")}
}

// TestPreflightStuckSnapshotIsAReservation is the CephFS incident, reduced: snapshots bound to a
// content, none ready, hours old.
//
// The assertions are on the STATUS and on the words a reader acts on. WARN and not FAIL is a decision,
// not an accident: a stuck VolumeSnapshot is somebody else's controller failing, observed from
// outside, and this script must not return BLOCKING on a diagnosis it has not earned. And it must not
// return PASS, which is what it did on the real cluster.
func TestPreflightStuckSnapshotIsAReservation(t *testing.T) {
	rec := only(t, stubbedRun(t, "check_stuck_snapshots", snapshotFixture(
		snapRow(9*time.Hour, "media/cephfs-media-snap", "csi-cephfsplugin-snapclass", "snapcontent-abc", ""),
		snapRow(4*time.Hour, "billing/cephfs-docs-snap", "csi-cephfsplugin-snapclass", "snapcontent-def", "false"),
	)))

	if rec.Status != "WARN" {
		t.Errorf("status = %s, want WARN — a stalled snapshotter is a reservation: PASS is the answer "+
			"that let eight volumes go unbacked-up for the life of a cluster, and FAIL would be this "+
			"script diagnosing another component from a symptom. Detail: %s", rec.Status, rec.Detail)
	}
	for _, want := range []string{
		"NONE is readyToUse",
		"csi-cephfsplugin-snapclass",
		"csi-snapshotter",
		"media/cephfs-media-snap",
	} {
		if !strings.Contains(rec.Detail, want) {
			t.Errorf("the finding does not contain %q, so it is not actionable: %s", want, rec.Detail)
		}
	}
	// The oldest age is the number that separates "a snapshot lost a race" from "this has never
	// worked", and it must be the OLDEST of the two rather than the first one listed.
	if !strings.Contains(rec.Detail, "oldest 9 h") {
		t.Errorf("the finding does not state the oldest stuck age: %s", rec.Detail)
	}
}

// TestPreflightBoundButYoungIsNotAFinding is the false-positive half, and it is the reason the check
// has a grace period at all rather than reporting every not-yet-ready snapshot.
//
// A snapshot bound two minutes ago is work in flight. The crucible has measured the external
// snapshot-controller taking just over five minutes on a content whose teardown was already complete,
// and a cloud-disk driver may legitimately take far longer. A check that called those stuck would cry
// wolf on every busy cluster and be ignored by the time it mattered.
func TestPreflightBoundButYoungIsNotAFinding(t *testing.T) {
	rec := only(t, stubbedRun(t, "check_stuck_snapshots", snapshotFixture(
		snapRow(2*time.Minute, "billing/ledger-snap", "csi-rbdplugin-snapclass", "snapcontent-123", ""),
		snapRow(30*time.Minute, "billing/ledger-snap-2", "csi-rbdplugin-snapclass", "snapcontent-456", "false"),
	)))

	if rec.Status != "PASS" {
		t.Errorf("status = %s, want PASS: both snapshots are inside the grace and are plausibly still "+
			"being taken. Detail: %s", rec.Status, rec.Detail)
	}
	if !strings.Contains(rec.Detail, "2 bound and still inside the grace") {
		t.Errorf("the young snapshots are not reported as young, so a reader cannot tell this apart "+
			"from a cluster where nothing was happening at all: %s", rec.Detail)
	}
	// The PASS must not read as a verdict on the storage. Two facts were established — nothing is
	// stalled — and one was not: whether any StorageClass without a snapshot works.
	if !strings.Contains(rec.Detail, "says nothing about a StorageClass no snapshot has ever been taken on") {
		t.Errorf("the PASS does not disclaim what it did not establish: %s", rec.Detail)
	}
}

// TestPreflightUnboundSnapshotIsNotStuck keeps the two halves of a failed snapshot apart.
//
// A VolumeSnapshot with no boundVolumeSnapshotContentName means the CLUSTER-WIDE snapshot controller
// has not acted on it — nothing is listening to that VolumeSnapshotClass. A bound one means something
// IS listening and the copy is not arriving. Those send a reader to different components, and the
// first is already the subject of the "snapshot controller" check above this one. Folding them
// together would produce one finding that points at two places.
func TestPreflightUnboundSnapshotIsNotStuck(t *testing.T) {
	rec := only(t, stubbedRun(t, "check_stuck_snapshots", snapshotFixture(
		snapRow(80*time.Hour, "billing/never-picked-up", "csi-rbdplugin-snapclass", "", ""),
	)))

	if rec.Status != "PASS" {
		t.Errorf("status = %s, want PASS: an unbound snapshot is not this check's finding — it means "+
			"nothing is listening to the class, which the snapshot-controller check reports. Detail: %s",
			rec.Status, rec.Detail)
	}
	if !strings.Contains(rec.Detail, "1 not bound to a content at all") {
		t.Errorf("the unbound snapshot is not counted anywhere, so it vanished from the report "+
			"entirely: %s", rec.Detail)
	}
}

// TestPreflightNoSnapshotsIsUnknownNotPass is the honesty requirement, and it is the one an
// administrator running this BEFORE installing will meet every time.
//
// A cluster with no VolumeSnapshot has told this check nothing. That must not render as a pass: "no
// snapshot is stuck" and "snapshots work here" are different statements, and the second one is the
// one a reader will take away from a green line.
func TestPreflightNoSnapshotsIsUnknownNotPass(t *testing.T) {
	rec := only(t, stubbedRun(t, "check_stuck_snapshots", snapshotFixture()))

	if rec.Status != "UNKNOWN" {
		t.Errorf("status = %s, want UNKNOWN on a cluster with no VolumeSnapshot at all: nothing was "+
			"observed, and this script does not report green on an absence. Detail: %s",
			rec.Status, rec.Detail)
	}
	if !strings.Contains(rec.Detail, "NOT evidence that snapshots work") {
		t.Errorf("the record does not say what it failed to establish: %s", rec.Detail)
	}
}

// TestPreflightUnreadableSnapshotListDegrades is the same property every other check in this script
// already has, asserted for the new one: a listing the cluster refuses is a fact about your token, and
// it must degrade to UNKNOWN rather than being read as an empty cluster.
//
// The distinction is not academic. `kubectl get volumesnapshot` is exactly the read a
// least-privilege service account is most likely to be missing, and "no snapshot is stuck" is what a
// naive implementation prints when it is denied.
func TestPreflightUnreadableSnapshotListDegrades(t *testing.T) {
	rec := only(t, stubbedRun(t, "check_stuck_snapshots", map[string]string{
		snapshotFixtureKey: errValue +
			`Error from server (Forbidden): volumesnapshots.snapshot.storage.k8s.io is forbidden`,
	}))

	if rec.Status != "UNKNOWN" {
		t.Errorf("status = %s, want UNKNOWN: the listing was refused, so nothing was established. "+
			"Detail: %s", rec.Status, rec.Detail)
	}
	if !strings.Contains(rec.Detail, "Forbidden") {
		t.Errorf("the record does not carry the cluster's own error, so a reader cannot tell a "+
			"permission problem from a missing CRD: %s", rec.Detail)
	}
	if !strings.Contains(rec.Detail, "NOT established") {
		t.Errorf("the record does not state that the check was not made: %s", rec.Detail)
	}
}

// TestPreflightStuckAlongsideReadyIsTheSmallerClaim. A class with ready snapshots AND stuck ones is a
// working snapshotter with something wrong in it, which is a much weaker claim than "this has never
// worked" — and it must be worded like one, because the remedies differ and because over-claiming on
// the weaker case is how a check earns a reputation for crying wolf.
func TestPreflightStuckAlongsideReadyIsTheSmallerClaim(t *testing.T) {
	rec := only(t, stubbedRun(t, "check_stuck_snapshots", snapshotFixture(
		snapRow(3*time.Hour, "billing/ledger-ok", "csi-rbdplugin-snapclass", "snapcontent-ok", "true"),
		snapRow(3*time.Hour, "billing/ledger-stuck", "csi-rbdplugin-snapclass", "snapcontent-bad", "false"),
	)))

	if rec.Status != "WARN" {
		t.Fatalf("status = %s, want WARN. Detail: %s", rec.Status, rec.Detail)
	}
	if strings.Contains(rec.Detail, "NONE is readyToUse") {
		t.Errorf("a class with a readyToUse snapshot was reported with the never-worked wording: %s",
			rec.Detail)
	}
	if !strings.Contains(rec.Detail, "Snapshots do complete here") {
		t.Errorf("the weaker claim is not worded as the weaker claim: %s", rec.Detail)
	}
	if !strings.Contains(rec.Detail, "1 readyToUse") {
		t.Errorf("the per-class breakdown does not carry the ready count, which is the evidence in "+
			"the other direction: %s", rec.Detail)
	}
}

// TestPreflightUnparseableTimestampIsNotYoung. The age arithmetic is done from first principles in
// awk, because no portable shell can parse RFC 3339. A timestamp that formula cannot read must be
// counted and reported, not silently bucketed as "inside the grace" — which is the direction a naive
// `age = now - 0` would fall in, and it would fall towards a clean report.
func TestPreflightUnparseableTimestampIsNotYoung(t *testing.T) {
	rec := only(t, stubbedRun(t, "check_stuck_snapshots", snapshotFixture(
		"not-a-timestamp\tbilling/weird-snap\tcsi-rbdplugin-snapclass\tsnapcontent-x\tfalse",
	)))

	if !strings.Contains(rec.Detail, "could not parse and were NOT judged either way") {
		t.Errorf("an unparseable creationTimestamp was absorbed silently: %s", rec.Detail)
	}
}

// TestPreflightAgeArithmeticIsExact pins the awk date conversion against a case a wrong implementation
// gets wrong: a snapshot just either side of the grace, across a leap-year February.
//
// The days_from_civil formula is the one part of this check that is real arithmetic rather than
// classification, and an off-by-a-day in it would move every verdict by 24 hours — in the forgiving
// direction on some dates and the alarming direction on others. Driving it through fixed absolute
// timestamps rather than relative ones is the only way to catch that, so the expected age is computed
// here in Go and compared against what the script printed.
func TestPreflightAgeArithmeticIsExact(t *testing.T) {
	// 2024 is a leap year: a snapshot created on 29 February exercises the branch that shifts March
	// to the start of the year.
	created := time.Date(2024, 2, 29, 23, 30, 0, 0, time.UTC)
	wantHours := int(time.Since(created).Hours())

	rec := only(t, stubbedRun(t, "check_stuck_snapshots", snapshotFixture(
		created.Format(time.RFC3339)+"\tbilling/ancient\tcsi-rbdplugin-snapclass\tsnapcontent-old\tfalse",
	)))

	if rec.Status != "WARN" {
		t.Fatalf("a snapshot bound since a leap day is not reported as stuck: %s / %s",
			rec.Status, rec.Detail)
	}
	// One hour of slack, because the test's clock and the script's are read a moment apart.
	if !strings.Contains(rec.Detail, fmt.Sprintf("oldest %d h", wantHours)) &&
		!strings.Contains(rec.Detail, fmt.Sprintf("oldest %d h", wantHours+1)) {
		t.Errorf("the computed age is not ~%d h, so the date arithmetic is wrong: %s",
			wantHours, rec.Detail)
	}
}

// TestHarnessCutsAtMain guards the harness itself. Every test above asserts on the output of ONE check
// function; if the marker the harness truncates at ever disappeared, it would source the whole script,
// run all thirteen checks against a stub written for one, and the assertions would keep passing on
// output that means nothing.
func TestHarnessCutsAtMain(t *testing.T) {
	src := readDefinitions(t)
	if strings.Contains(src, "check_kubectl\ncheck_server_version") {
		t.Error("the definitions region still contains the script's main body")
	}
	for _, fn := range []string{"check_stuck_snapshots()", "CB_SNAPSHOT_STALL_GRACE_MIN=", "k_get()"} {
		if !strings.Contains(src, fn) {
			t.Errorf("the definitions region does not contain %q, so the harness is not sourcing the "+
				"real script", fn)
		}
	}
}
