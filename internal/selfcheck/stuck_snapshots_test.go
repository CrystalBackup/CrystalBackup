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

package selfcheck

// The tests for the snapshot OBSERVATION, and for the one place it touches the per-PVC census.
//
// The incident: on a production cluster no CephFS volume had ever been backed up successfully — eight
// volumes, four namespaces, every run ending SnapshotReadyDeadlineExceeded, for the life of the
// cluster. The verdict the operator gave was exactly right. What nothing said was that this had NEVER
// worked: this report was silent, and its per-PVC census classified all eight `ok`.
//
// Every test below is about one of the two halves of getting that right: seeing the symptom, and not
// inventing it where there is no evidence.

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
)

// The two VolumeSnapshotClass names the incident fixture uses.
//
// They are named CONSTANTS and their alphabetical order is load-bearing: fixtureHealthySnapClass sorts
// BEFORE fixtureStalledSnapClass, so an implementation that failed to sort the class census
// stalled-first would put the healthy class at the top. Both are names a rook-ceph cluster really
// carries. With the two swapped, a mutation removing the primary sort key survives — which it did, once.
const (
	fixtureStalledSnapClass = "csi-cephfsplugin-snapclass"
	fixtureHealthySnapClass = "ceph-block-snapclass"
)

// TestStuckSnapshotIsSeenAsTheSymptom is the central test: two snapshots bound to a content and never
// ready, beside one that IS ready on another class.
//
// The pair is the whole assertion. A count of stuck snapshots alone would be satisfied by an
// implementation that called every not-yet-ready snapshot stuck; what makes the finding mean something
// is that the ready one is counted as evidence in the other direction, on its own class.
func TestStuckSnapshotIsSeenAsTheSymptom(t *testing.T) {
	rep := collectSnapshotReport(t, stuckSnapshotCluster(t))
	got := rep.StuckSnapshots

	if got.Stuck != 2 {
		t.Errorf("Stuck = %d, want 2 (both CephFS snapshots are bound and have never become ready)", got.Stuck)
	}
	if got.Ready != 1 {
		t.Errorf("Ready = %d, want 1 — the readyToUse snapshot is the evidence that keeps a working "+
			"class from being maligned", got.Ready)
	}
	if got.Bound != 3 || got.Unbound != 0 {
		t.Errorf("Bound/Unbound = %d/%d, want 3/0", got.Bound, got.Unbound)
	}
	if got.WithinGrace != 0 {
		t.Errorf("WithinGrace = %d, want 0: both stuck snapshots are far past the grace", got.WithinGrace)
	}
	if got.OldestStuckHours != 9 {
		t.Errorf("OldestStuckHours = %d, want 9 — the age is what separates a snapshot that lost a "+
			"race from a snapshotter that has never worked", got.OldestStuckHours)
	}
	if got.GraceMinutes != 60 {
		t.Errorf("GraceMinutes = %d, want 60; the report has to state the bound its own verdict was "+
			"reached against", got.GraceMinutes)
	}

	byClass := map[string]StuckSnapshotClass{}
	for _, c := range got.Classes {
		byClass[c.Class] = c
	}
	if c := byClass[fixtureStalledSnapClass]; c.Stuck != 2 || c.Ready != 0 {
		t.Errorf("the CephFS class census is %+v, want 2 stuck and 0 ready — that pair IS the "+
			"incident's signature", c)
	}
	if c := byClass[fixtureHealthySnapClass]; c.Stuck != 0 || c.Ready != 1 {
		t.Errorf("the RBD class census is %+v, want 0 stuck and 1 ready: a stall on one driver must "+
			"not be attributed to another", c)
	}
	// Stuck classes first, so the row a reader has to act on is the first one.
	//
	// The fixture's two class NAMES are chosen so that alphabetical order and stall order disagree (see
	// fixtureHealthySnapClass). Without that, an implementation with no primary sort at all would put
	// the stalled class first by luck and this assertion would prove nothing.
	if len(got.Classes) == 0 || got.Classes[0].Class != fixtureStalledSnapClass {
		t.Errorf("the class carrying the stall is not the first row: %+v", got.Classes)
	}

	if len(got.Samples) != 2 {
		t.Fatalf("Samples = %d, want both stuck snapshots named so an operator can go and look: %+v",
			len(got.Samples), got.Samples)
	}
	// Oldest first: on a cluster with more stuck snapshots than the sample cap, the ones that prove
	// this has never worked are the ones that must survive the cap.
	if got.Samples[0].AgeHours < got.Samples[1].AgeHours {
		t.Errorf("the samples are not oldest-first: %+v", got.Samples)
	}
	s := got.Samples[0]
	if s.Content == "" || s.SourcePVC == "" || s.Class == "" {
		t.Errorf("a sample is missing the fields an operator would describe next: %+v", s)
	}
}

// TestStuckSnapshotReachesTheHeadlineAndBothRenderers. A finding no alert rule fires on is a finding
// only the document can carry, and a reader who sees "healthy" at the top does not scroll — which is
// precisely how this defect survived a whole incident.
func TestStuckSnapshotReachesTheHeadlineAndBothRenderers(t *testing.T) {
	rep := collectSnapshotReport(t, stuckSnapshotCluster(t))

	if !strings.Contains(rep.Verdict.Summary, "bound to a VolumeSnapshotContent") {
		t.Errorf("the headline does not mention the stalled snapshots: %q", rep.Verdict.Summary)
	}
	if !strings.Contains(rep.Verdict.Summary, "not a breached rule") {
		t.Errorf("the headline does not say the finding is not an alert, so it can be quoted into a "+
			"ticket as one: %q", rep.Verdict.Summary)
	}
	if rep.Verdict.Status == verdictHealthy {
		t.Error("the verdict still reads as plainly healthy over a cluster whose snapshotter is not " +
			"advancing")
	}
	// The rule TALLY must be untouched. No alert rule fires on a stalled snapshotter, and a report that
	// moved Breached or Critical for it would claim an alert the alerting side would never send — which
	// is the one thing that would make this section unquotable.
	bare := collectSnapshotReport(t, coverageClient(t, stuckSnapshotBase()...))
	if rep.Verdict.Breached != bare.Verdict.Breached || rep.Verdict.Critical != bare.Verdict.Critical ||
		rep.Verdict.OK != bare.Verdict.OK {
		t.Errorf("the stalled snapshots moved the rule tally (breached/critical/ok = %d/%d/%d, want "+
			"%d/%d/%d): this is a finding, not a breach",
			rep.Verdict.Breached, rep.Verdict.Critical, rep.Verdict.OK,
			bare.Verdict.Breached, bare.Verdict.Critical, bare.Verdict.OK)
	}

	text := string(RenderText(rep, TextOptions{}))
	if !strings.Contains(text, "see stuckSnapshots") {
		t.Errorf("the text renderer does not carry the finding:\n%s", text)
	}
	if !strings.Contains(text, fixtureStalledSnapClass) {
		t.Errorf("the text renderer names no stuck snapshot, so a terminal reader cannot act:\n%s", text)
	}

	page, err := Render(rep)
	if err != nil {
		t.Fatal(err)
	}
	html := string(page)
	for _, want := range []string{
		// The verdict paragraph, which is what a reader who does not scroll sees.
		"OBSERVATION and not a diagnosis",
		// The dedicated section's own heading, asserted WITH its tag. The words alone also appear in
		// the verdict paragraph above ("See <b>Snapshots that are not advancing</b> below"), so a bare
		// text match cannot tell a present section from a deleted one — a mutation renaming the
		// heading survived exactly that assertion, once.
		"<h2>Snapshots that are not advancing",
		// And the section's body, so a heading over nothing is caught too.
		"Bound, not ready, past grace",
		"Bound, not ready, within grace",
		"<th>VolumeSnapshotClass</th>",
		// The per-snapshot sample table, which is where the operator gets an object to describe.
		fixtureStalledSnapClass,
		"cephfs-media-snap-old",
		"snapcontent-old",
		// And the census row's own qualification, in the coverage table.
		"OBSERVED, not predicted",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("the HTML report does not contain %q", want)
		}
	}
}

// TestBoundButYoungIsNotYetAFinding is the false-positive half, and it is why the check has a grace at
// all rather than reporting every snapshot that is not ready yet.
//
// A snapshot bound half an hour ago is work in flight. The crucible measured the external
// snapshot-controller taking just over five minutes on a content whose teardown was already complete,
// and a cloud-disk driver may legitimately take much longer. Reporting those would put a red line on
// every busy cluster, and a check that cries wolf is a check nobody reads on the night it is right.
func TestBoundButYoungIsNotYetAFinding(t *testing.T) {
	c := coverageClient(t,
		nsObj("tenant-a", "acme"),
		covPVC("tenant-a", "rbd-data", "ceph-block", "pv-rbd", "1Gi"),
		covCSIPV("pv-rbd", covRBD, "ceph-block"),
		snapObj("tenant-a", "rbd-data-snap", "rbd-data", "csi-rbdplugin-snapclass",
			"snapcontent-young", nil, fixtureNow().Add(-30*time.Minute)),
	)
	rep := collectSnapshotReport(t, c)

	if rep.StuckSnapshots.Stuck != 0 {
		t.Errorf("Stuck = %d, want 0: a snapshot bound thirty minutes ago is inside the grace",
			rep.StuckSnapshots.Stuck)
	}
	if rep.StuckSnapshots.WithinGrace != 1 {
		t.Errorf("WithinGrace = %d, want 1 — a zero here would make 'nothing has been waiting long "+
			"enough to judge' indistinguishable from 'nothing is happening'", rep.StuckSnapshots.WithinGrace)
	}
	if strings.Contains(rep.Verdict.Summary, "bound to a VolumeSnapshotContent") {
		t.Errorf("a snapshot inside the grace reached the headline: %q", rep.Verdict.Summary)
	}
	if rep.Coverage.StalledStorage != 0 {
		t.Errorf("the census qualified a prediction on the strength of a snapshot that is still "+
			"plausibly in flight (StalledStorage = %d)", rep.Coverage.StalledStorage)
	}
}

// TestUnboundSnapshotIsNotStuck keeps the two halves of a failed snapshot apart, however old the
// object is.
//
// No boundVolumeSnapshotContentName means the CLUSTER-WIDE snapshot controller never acted on it:
// nothing is listening to that VolumeSnapshotClass. A bound one means something IS listening and the
// copy is not arriving. The two send a reader to different components — which is exactly why the
// controller has two different deadline reasons for them — and one finding pointing at both places
// would be worse than either.
func TestUnboundSnapshotIsNotStuck(t *testing.T) {
	c := coverageClient(t,
		nsObj("tenant-a", "acme"),
		covPVC("tenant-a", "rbd-data", "ceph-block", "pv-rbd", "1Gi"),
		covCSIPV("pv-rbd", covRBD, "ceph-block"),
		snapObj("tenant-a", "never-picked-up", "rbd-data", "csi-rbdplugin-snapclass",
			"", nil, fixtureNow().Add(-80*time.Hour)),
	)
	rep := collectSnapshotReport(t, c)

	if rep.StuckSnapshots.Stuck != 0 {
		t.Errorf("Stuck = %d, want 0: an unbound snapshot is a different finding", rep.StuckSnapshots.Stuck)
	}
	if rep.StuckSnapshots.Unbound != 1 || rep.StuckSnapshots.Bound != 0 {
		t.Errorf("Unbound/Bound = %d/%d, want 1/0 — the object must still be counted somewhere or it "+
			"has vanished from the report", rep.StuckSnapshots.Unbound, rep.StuckSnapshots.Bound)
	}
	if rep.Coverage.StalledStorage != 0 {
		t.Errorf("the census qualified a prediction from an unbound snapshot (StalledStorage = %d)",
			rep.Coverage.StalledStorage)
	}
}

// TestNoSnapshotsAtAllClaimsNothing. A cluster with no VolumeSnapshot has told this section nothing,
// and it must report exactly that: no finding, and no claim that snapshots work either.
//
// GraceMinutes is the field that carries the difference between "looked and saw nothing" and "was
// produced by a version that never looked", which matters because this struct is not a pointer.
func TestNoSnapshotsAtAllClaimsNothing(t *testing.T) {
	rep := collectSnapshotReport(t, coverageFixture(t))
	got := rep.StuckSnapshots

	if got.Total != 0 || got.Stuck != 0 {
		t.Errorf("a cluster with no VolumeSnapshot produced Total=%d Stuck=%d", got.Total, got.Stuck)
	}
	if got.GraceMinutes == 0 {
		t.Error("GraceMinutes is zero on a report that DID look, so nothing distinguishes it from a " +
			"report produced by a version that never looked")
	}
	if len(got.Unreadable) != 0 {
		t.Errorf("an empty cluster was reported as unreadable: %v", got.Unreadable)
	}
	if strings.Contains(rep.Verdict.Summary, "bound to a VolumeSnapshotContent") {
		t.Errorf("an empty cluster produced a stall finding in the headline: %q", rep.Verdict.Summary)
	}
}

// TestCensusQualificationDoesNotFireWithoutSnapshots is the "absence of evidence is not evidence of
// failure" requirement, and it is the one this section would be worthless without.
//
// The coverage fixture has ten PVCs across every treatment class and not one VolumeSnapshot — which is
// the state of every cluster on the day CrystalBackup is installed. If a StorageClass with no snapshots
// were treated as a StorageClass that has failed to produce one, the section would qualify every row on
// every fresh installation and be ignored within a week.
func TestCensusQualificationDoesNotFireWithoutSnapshots(t *testing.T) {
	cov := collectCoverage(t, coverageFixture(t))

	if cov.StalledStorage != 0 {
		t.Errorf("StalledStorage = %d on a cluster with no VolumeSnapshot at all", cov.StalledStorage)
	}
	for _, it := range cov.Items {
		if it.SnapshotEvidence != "" {
			t.Errorf("%s/%s was qualified with a snapshot observation on a cluster that has no "+
				"snapshots: %q", it.Namespace, it.Name, it.SnapshotEvidence)
		}
		if it.StuckOnStorageClass != 0 || it.ReadyOnStorageClass != 0 {
			t.Errorf("%s/%s carries snapshot counts on a cluster with no snapshots: %d stuck, %d ready",
				it.Namespace, it.Name, it.StuckOnStorageClass, it.ReadyOnStorageClass)
		}
	}
}

// TestCensusQualifiesButDoesNotRevoke is the line this lot must not cross, asserted from both sides.
//
// The CephFS PVC's prediction is qualified — the row says what was OBSERVED beside what was predicted —
// and the prediction itself is untouched: the class is still cephfs-shallow and the verdict is still
// backedUp. Turning it into Skipped or Failed would be this report diagnosing somebody else's
// controller from a symptom, and it would move a phase real automation reacts to.
//
// The RBD PVC in the same cluster, on a StorageClass with no snapshots at all, must come out
// completely unqualified: the stall belongs to one class and must not spread.
func TestCensusQualifiesButDoesNotRevoke(t *testing.T) {
	rep := collectSnapshotReport(t, stuckSnapshotCluster(t))
	rows := coverageRows(rep.Coverage)

	cephfs := rows["tenant-a/cephfs-media"]
	if cephfs.Verdict != CoverageVerdictBackedUp {
		t.Errorf("the CephFS row's verdict is %q: an observation about the storage must not rewrite "+
			"the treatment the operator will attempt", cephfs.Verdict)
	}
	if cephfs.Class != CoverageDirect {
		t.Errorf("the CephFS row's class is %q, want %q — unchanged", cephfs.Class, CoverageDirect)
	}
	if cephfs.SnapshotEvidence == "" {
		t.Fatal("the CephFS row carries no observation, so the report is once again predicting `ok` " +
			"over storage that has never completed a snapshot")
	}
	for _, want := range []string{"OBSERVED", "NONE is readyToUse", "ceph-fs", "csi-snapshotter",
		"changes no verdict"} {
		if !strings.Contains(cephfs.SnapshotEvidence, want) {
			t.Errorf("the observation does not contain %q: %q", want, cephfs.SnapshotEvidence)
		}
	}
	if cephfs.StuckOnStorageClass != 2 || cephfs.ReadyOnStorageClass != 0 {
		t.Errorf("the row's counts are %d stuck / %d ready, want 2/0",
			cephfs.StuckOnStorageClass, cephfs.ReadyOnStorageClass)
	}

	rbd := rows["tenant-a/rbd-data"]
	if rbd.SnapshotEvidence != "" {
		t.Errorf("the RBD row was qualified by a stall on a different StorageClass: %q",
			rbd.SnapshotEvidence)
	}

	if rep.Coverage.StalledStorage != 1 {
		t.Errorf("StalledStorage = %d, want 1 (the CephFS volume only)", rep.Coverage.StalledStorage)
	}
	// A qualified row must sort into the attention block, or the cap that keeps this document small
	// would silently eat the finding on any cluster with more than five hundred volumes.
	if coverageNeedsAttention(cephfs) != 0 {
		t.Error("a qualified row does not sort as needing attention, so maxCoverageItems can drop it")
	}
	if !textNeedsAttention(cephfs) {
		t.Error("a qualified row is hidden from the compact text output")
	}
	text := string(RenderText(rep, TextOptions{}))
	if !strings.Contains(text, "OBSERVED") {
		t.Errorf("the qualification never reaches the terminal:\n%s", text)
	}
}

// TestCensusQualificationIsTheSmallerClaimWhenTheClassWorks. A StorageClass with ready snapshots AND
// stuck ones is a working snapshotter with something wrong in it. Wording that as "this has never
// worked" would be an over-claim, and over-claiming on the weaker case is how the whole section earns
// a reputation for crying wolf and stops being read.
func TestCensusQualificationIsTheSmallerClaimWhenTheClassWorks(t *testing.T) {
	ready := true
	notReady := false
	c := coverageClient(t,
		nsObj("tenant-a", "acme"),
		// Without the snapshotter Secret the class names, both PVCs land in SnapshotPrecheckFailed and
		// there is no clean prediction left to qualify.
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "csi-snap-creds", Namespace: "rook-ceph"}},
		covPVC("tenant-a", "rbd-one", "ceph-block", "pv-one", "1Gi"),
		covCSIPV("pv-one", covRBD, "ceph-block"),
		covPVC("tenant-a", "rbd-two", "ceph-block", "pv-two", "1Gi"),
		covCSIPV("pv-two", covRBD, "ceph-block"),
		snapObj("tenant-a", "rbd-one-snap", "rbd-one", "csi-rbdplugin-snapclass",
			"snapcontent-ok", &ready, fixtureNow().Add(-3*time.Hour)),
		snapObj("tenant-a", "rbd-two-snap", "rbd-two", "csi-rbdplugin-snapclass",
			"snapcontent-bad", &notReady, fixtureNow().Add(-3*time.Hour)),
	)
	rep := collectSnapshotReport(t, c)
	rows := coverageRows(rep.Coverage)

	// BOTH rows are qualified: the evidence is aggregated per StorageClass, and both volumes are on the
	// class that is not finishing a snapshot.
	for _, name := range []string{"tenant-a/rbd-one", "tenant-a/rbd-two"} {
		ev := rows[name].SnapshotEvidence
		if ev == "" {
			t.Errorf("%s carries no observation", name)
			continue
		}
		if strings.Contains(ev, "NONE is readyToUse") {
			t.Errorf("%s was given the never-worked wording over a class that has a ready snapshot: %q",
				name, ev)
		}
		if !strings.Contains(ev, "The class works") {
			t.Errorf("%s is not worded as the smaller claim: %q", name, ev)
		}
	}
}

// TestUnreadableSnapshotListDegradesRatherThanReportingClean is this package's oldest rule applied to
// the newest section: an empty section because the operator could not look must never render the same
// as one that is empty because there was nothing there.
//
// `list volumesnapshots` at the cluster scope is exactly the read a least-privilege identity is most
// likely to be missing, and "0 stuck" is what a naive implementation prints when it is denied — a
// clean bill of health issued by a refusal.
func TestUnreadableSnapshotListDegradesRatherThanReportingClean(t *testing.T) {
	base := stuckSnapshotCluster(t)
	rep, err := Collect(context.Background(), Options{
		Reader: forbiddenReader{
			Reader: base,
			kinds:  map[string]bool{"VolumeSnapshotList": true},
		},
		OperatorNamespace: operatorNS, Now: fixtureNow(), Full: true,
	})
	if err != nil {
		t.Fatalf("a cluster the reader cannot fully read must still produce a report: %v", err)
	}

	if len(rep.StuckSnapshots.Unreadable) == 0 {
		t.Error("a refused VolumeSnapshot list left no trace in the section, so 0 stuck reads as a " +
			"cluster where nothing is stuck")
	}
	found := false
	for _, d := range rep.Diagnostics {
		if d.Area == "stuckSnapshots" {
			found = true
			if d.Impact == "" {
				t.Errorf("the diagnostic does not say what is missing from the report: %+v", d)
			}
			if !strings.Contains(d.Impact, "unqualified") {
				t.Errorf("the diagnostic does not say that the census's predictions went unqualified, "+
					"which is the consequence a reader has to know: %+v", d)
			}
		}
	}
	if !found {
		t.Error("a refused VolumeSnapshot list produced no diagnostic: the only mechanism this report " +
			"has for 'the operator was not allowed to look' was not used")
	}
	// And the census must qualify NOTHING rather than qualify everything as fine.
	if rep.Coverage.StalledStorage != 0 {
		t.Errorf("StalledStorage = %d over a refused snapshot list", rep.Coverage.StalledStorage)
	}
	page, err := Render(rep)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(page), "a floor and not an answer") {
		t.Error("the HTML page does not tell the reader that the counts in this section are a floor")
	}
}

// TestSnapshotObservationCostIsIndependentOfSnapshotCount. The whole design rests on the symptom being
// visible in a SINGLE LIST — no write, no per-object read, no permission the operator did not already
// have — which is what makes it affordable on a cluster holding thousands of retained snapshots and
// safe to run against production.
//
// The property is scale-invariance and it is measured, not asserted in prose: the same collection over
// a cluster with three snapshots and one with three hundred must make the same number of API calls. An
// implementation that read a VolumeSnapshotContent per snapshot, or resolved each source PVC with a
// Get, would still produce every verdict in this file correctly and would multiply the cost of the
// self-check by the size of the cluster.
func TestSnapshotObservationCostIsIndependentOfSnapshotCount(t *testing.T) {
	small := &countingReader{Reader: snapshotHeavyCluster(t, 3)}
	large := &countingReader{Reader: snapshotHeavyCluster(t, 300)}

	for _, r := range []*countingReader{small, large} {
		if _, err := Collect(context.Background(), Options{
			Reader: r, OperatorNamespace: operatorNS, Now: fixtureNow(), Full: true,
		}); err != nil {
			t.Fatalf("collect: %v", err)
		}
	}
	if small.calls != large.calls {
		t.Errorf("the collection made %d API calls over a cluster with 3 VolumeSnapshots and %d over "+
			"one with 300: the observation must cost one LIST, not one read per snapshot",
			small.calls, large.calls)
	}
	// Counted separately from the total rather than asserted to be zero: the exposer's own pre-check
	// legitimately Gets a snapshotter Secret, and this test has no business pinning that. What it does
	// pin is that no Get is issued PER SNAPSHOT, which is the shape a per-object implementation takes.
	if small.gets != large.gets {
		t.Errorf("the collection issued %d single-object Gets over 3 VolumeSnapshots and %d over 300: "+
			"the observation must not read an object per snapshot", small.gets, large.gets)
	}
}

// countingReader counts every API call a collection makes, so a cost claim can be measured rather than
// asserted in a comment.
type countingReader struct {
	client.Reader
	calls int
	gets  int
}

func (c *countingReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	c.calls++
	return c.Reader.List(ctx, list, opts...)
}

func (c *countingReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	c.calls++
	c.gets++
	return c.Reader.Get(ctx, key, obj, opts...)
}

// TestStuckSnapshotSectionIsRedacted. This section carries three new classes of identifier into a
// document whose default mode is hashed and whose whole point is to be attachable to a public issue: a
// namespace, a VolumeSnapshot name, a source PVC name, a VolumeSnapshotContent name, and free text
// quoted from a storage driver.
//
// The VolumeSnapshotClass survives redaction on purpose — it is a platform-chosen identifier
// ("csi-cephfsplugin-snapclass"), it is the field that makes this section readable, and it is the same
// decision already taken for a StorageClass name and an image tag. If that ever stops being true, this
// is where to argue it.
//
// The snapshot NAME matters more than it looks: the operator derives its origin VolumeSnapshot's name
// from the backup and the PVC, so a section that printed it verbatim would leak a PVC name through a
// field nobody was auditing.
func TestStuckSnapshotSectionIsRedacted(t *testing.T) {
	rep, err := Collect(context.Background(), Options{
		Reader: stuckSnapshotCluster(t), OperatorNamespace: operatorNS, Now: fixtureNow(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.StuckSnapshots.Samples) == 0 {
		t.Fatal("the fixture produced no stuck-snapshot sample, so the assertions below prove nothing")
	}
	for _, s := range rep.StuckSnapshots.Samples {
		if !strings.HasPrefix(s.Namespace, "ns-") {
			t.Errorf("a sample carries a plaintext namespace: %q", s.Namespace)
		}
		if strings.Contains(s.Name, "cephfs-media") {
			t.Errorf("a sample carries a plaintext VolumeSnapshot name, which is derived from the PVC "+
				"name and therefore leaks it: %q", s.Name)
		}
		if !strings.HasPrefix(s.SourcePVC, "pvc-") {
			t.Errorf("a sample carries a plaintext source PVC name: %q", s.SourcePVC)
		}
		if strings.Contains(s.Content, "snapcontent-") {
			t.Errorf("a sample carries a plaintext VolumeSnapshotContent name: %q", s.Content)
		}
		if s.Class != fixtureStalledSnapClass {
			t.Errorf("the VolumeSnapshotClass was redacted (%q): it is platform-chosen and is the field "+
				"this section is read by", s.Class)
		}
	}

	// The FREE-TEXT path, which per-field redaction cannot reach. A driver's own error message quotes
	// object names, and the fixture's does: it names the source PVC, exactly as a real ceph-csi message
	// would. Redactor.Detail is what has to substitute it out.
	var errs int
	for _, s := range rep.StuckSnapshots.Samples {
		if s.Error == "" {
			continue
		}
		errs++
		if strings.Contains(s.Error, "cephfs-media") {
			t.Errorf("a driver's error message carried a plaintext PVC name through to the report: %q",
				s.Error)
		}
	}
	if errs == 0 {
		t.Error("no sample carried a driver error message, so the free-text redaction path — the one " +
			"per-field redaction cannot reach — was never exercised")
	}

	// And the census's qualification, which is free text too. It names the StorageClass on purpose and
	// must name nothing else.
	var quals int
	for _, it := range rep.Coverage.Items {
		if it.SnapshotEvidence == "" {
			continue
		}
		quals++
		if strings.Contains(it.SnapshotEvidence, "cephfs-media") ||
			strings.Contains(it.SnapshotEvidence, "tenant-a") {
			t.Errorf("the qualification sentence carries a plaintext identifier: %q", it.SnapshotEvidence)
		}
		if !strings.Contains(it.SnapshotEvidence, "ceph-fs") {
			t.Errorf("the qualification sentence lost the StorageClass, which is the one identifier it "+
				"is supposed to name: %q", it.SnapshotEvidence)
		}
	}
	if quals == 0 {
		t.Error("no census row was qualified, so the assertions above prove nothing")
	}
}

// --- fixtures -------------------------------------------------------------------------------

// collectSnapshotReport collects over c with identifiers in clear, so the assertions can name a
// StorageClass and a VolumeSnapshotClass. The redaction of these fields is asserted separately.
func collectSnapshotReport(t *testing.T, c client.Reader) *Report {
	t.Helper()
	rep, err := Collect(context.Background(), Options{
		Reader: c, OperatorNamespace: operatorNS, Now: fixtureNow(), Full: true,
	})
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if rep.Coverage == nil {
		t.Fatal("the report carries no coverage section")
	}
	return rep
}

// stuckSnapshotCluster is the incident, reduced to two StorageClasses.
//
// ceph-fs carries two VolumeSnapshots that are bound to a content and have never become ready — nine
// and four hours old. ceph-block carries one that IS ready. Both PVCs resolve to a real exposer and are
// selected by an active schedule, so both rows are `backed up` before the observation is applied: that
// is the point of the fixture. Without a clean prediction to qualify, a test of the qualification
// proves nothing.
func stuckSnapshotCluster(t *testing.T) client.Client {
	t.Helper()
	ready := true
	return coverageClient(t, append(stuckSnapshotBase(),
		// readyToUse ABSENT on one and explicitly false on the other: a snapshotter that never runs
		// writes no status at all, which is the shape the incident actually had, and a snapshotter that
		// ran and failed writes false. Both are the same finding and the fixture carries both.
		snapObj("tenant-a", "cephfs-media-snap-old", "cephfs-media", fixtureStalledSnapClass,
			"snapcontent-old", nil, fixtureNow().Add(-9*time.Hour)),
		// This one carries a driver error message that QUOTES the source PVC's name, the way a real
		// ceph-csi message does. It is the only thing exercising the free-text redaction path in this
		// section, and it is also the shape a snapshotter that ran and failed leaves behind — as opposed
		// to one that never ran at all, which is the snapshot above with no status whatsoever.
		withSnapError(
			snapObj("tenant-a", "cephfs-media-snap-new", "cephfs-media", fixtureStalledSnapClass,
				"snapcontent-new", boolPtr(false), fixtureNow().Add(-4*time.Hour)),
			// The PVC name appears as a STANDALONE token, which is the only shape Redactor.Detail
			// substitutes: replaceIdentifier deliberately leaves a name embedded inside a longer
			// identifier alone (see its doc). Real driver messages quote the claim this way.
			"failed to create snapshot for source PVC cephfs-media: kernel does not support required "+
				"features"),
		snapObj("tenant-a", "rbd-data-snap", "rbd-data", fixtureHealthySnapClass,
			"snapcontent-good", &ready, fixtureNow().Add(-5*time.Hour)),
	)...)
}

// stuckSnapshotBase is stuckSnapshotCluster's cluster WITHOUT any VolumeSnapshot. Split out so the
// cost test can compare two collections that differ in exactly one thing.
func stuckSnapshotBase() []client.Object {
	return []client.Object{
		nsObj("tenant-a", "acme"),
		&cbv1.ClusterBackupLocation{
			ObjectMeta: metav1.ObjectMeta{Name: "dr", CreationTimestamp: metav1.NewTime(fixtureNow())},
			Spec: cbv1.ClusterBackupLocationSpec{
				Default: true, ClusterID: "c1", Mode: cbv1.LocationModeStandard,
				S3: cbv1.S3Spec{Endpoint: "https://s3.example:9000", Bucket: "b"},
			},
		},
		&cbv1.ClusterBackupSchedule{
			ObjectMeta: metav1.ObjectMeta{Name: "nightly", CreationTimestamp: metav1.NewTime(fixtureNow())},
			Spec: cbv1.ClusterBackupScheduleSpec{
				Schedule: "0 2 * * *",
				Template: cbv1.ClusterBackupTemplate{Spec: cbv1.ClusterBackupRunSpec{
					LocationRef: cbv1.LocalObjectReference{Name: "dr"},
					Namespaces:  cbv1.NamespaceSelector{MatchNames: []string{"tenant-a"}},
				}},
			},
		},
		// The snapshotter Secret the healthy classes name, so neither PVC lands in
		// SnapshotPrecheckFailed and both predictions start out clean.
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "csi-snap-creds", Namespace: "rook-ceph"}},

		covPVC("tenant-a", "cephfs-media", "ceph-fs", "pv-cephfs", "50Gi"),
		covCSIPV("pv-cephfs", covCephFS, "ceph-fs"),
		covPVC("tenant-a", "rbd-data", "ceph-block", "pv-rbd", "10Gi"),
		covCSIPV("pv-rbd", covRBD, "ceph-block"),
	}
}

// snapshotHeavyCluster is stuckSnapshotBase carrying n stuck VolumeSnapshots on the CephFS PVC. It
// exists for the cost test and for nothing else: it needs scale and no variety at all.
func snapshotHeavyCluster(t *testing.T, n int) client.Client {
	t.Helper()
	objs := stuckSnapshotBase()
	for i := range n {
		objs = append(objs, snapObj("tenant-a",
			fmt.Sprintf("cephfs-media-snap-%04d", i), "cephfs-media", "csi-cephfsplugin-snapclass",
			fmt.Sprintf("snapcontent-%04d", i), nil, fixtureNow().Add(-9*time.Hour)))
	}
	return coverageClient(t, objs...)
}

func boolPtr(b bool) *bool { return &b }

// snapObj builds a VolumeSnapshot with the four fields the observation reads. A nil ready leaves
// status.readyToUse ABSENT, which is not the same as false: it is what a snapshotter that never
// processed the object leaves behind, and it is the state the incident was in.
func snapObj(
	namespace, name, sourcePVC, class, content string, ready *bool, created time.Time,
) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshot",
	})
	u.SetNamespace(namespace)
	u.SetName(name)
	u.SetCreationTimestamp(metav1.NewTime(created))
	must(unstructured.SetNestedField(u.Object, sourcePVC, "spec", "source", "persistentVolumeClaimName"))
	if class != "" {
		must(unstructured.SetNestedField(u.Object, class, "spec", "volumeSnapshotClassName"))
	}
	if content != "" {
		must(unstructured.SetNestedField(u.Object, content, "status", "boundVolumeSnapshotContentName"))
	}
	if ready != nil {
		must(unstructured.SetNestedField(u.Object, *ready, "status", "readyToUse"))
	}
	return u
}

// withSnapError adds a status.error.message to a VolumeSnapshot. Kept apart from snapObj because most
// stuck snapshots have none — a snapshotter that never runs records nothing at all, which is why the
// age and not the error is the evidence this section rests on.
func withSnapError(u *unstructured.Unstructured, msg string) *unstructured.Unstructured {
	must(unstructured.SetNestedField(u.Object, msg, "status", "error", "message"))
	return u
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
