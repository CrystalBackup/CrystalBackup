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

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
)

// The drivers the coverage fixture is built out of. Real-world names, so a reader can tell at a glance
// which branch of Registry.For each PVC is meant to exercise.
const (
	covRBD       = "rook-ceph.rbd.csi.ceph.com"
	covCephFS    = "rook-ceph.cephfs.csi.ceph.com"
	covLocalPath = "rancher.io/local-path"
	covBroken    = "broken.csi.example.com"
)

// TestCoverageClassifiesEveryClass is the census's central test: one fixture cluster carrying one PVC
// per class, asserted class by class.
//
// It is written as a name→class map rather than as one test per class because the classes are decided
// by a SINGLE resolver over a SINGLE cluster, and the interesting failure is one PVC's answer
// contaminating another's — a cached PersistentVolume served to the wrong claim, a snapshot class
// tie-break that depends on which PVC asked first. Separate fixtures would hide exactly that.
func TestCoverageClassifiesEveryClass(t *testing.T) {
	cov := collectCoverage(t, coverageFixture(t))

	want := map[string]string{
		"tenant-a/rbd-data":      CoverageClone,
		"tenant-a/cephfs-media":  CoverageDirect,
		"tenant-a/local-scratch": CoverageUnsupported,
		"tenant-a/legacy-nfs":    CoverageUnsupported,
		"tenant-a/broken-snap":   CoveragePrecheckFailed,
		"tenant-a/ghost-volume":  CoverageUnresolved,
		"tenant-a/never-bound":   CoverageUnresolved,
		"tenant-a/pending-rbd":   CoverageClone,
		"tenant-b/orphan-data":   CoverageClone,
		"tenant-b/paused-data":   CoverageClone,
	}
	got := coverageByName(cov)
	for name, class := range want {
		if got[name] == "" {
			t.Errorf("%s: no row in the census at all", name)
			continue
		}
		if got[name] != class {
			t.Errorf("%s: class %q, want %q", name, got[name], class)
		}
	}
	// The operator's own temporary exposure PVC must not appear: it is the operator's working set, it
	// is already counted as residue in leakIndicators, and reporting it here would invent an
	// unprotected user volume out of nothing.
	if _, present := got[operatorNS+"/crystal-clone-temp"]; present {
		t.Error("the operator's own exposure PVC was counted as user data in the census")
	}
	if cov.PVCs != len(want) {
		t.Errorf("counted %d PVCs, want %d (%v)", cov.PVCs, len(want), got)
	}
}

// TestCoverageReportsPVCsNoScheduleSelects is the finding this whole section was added for.
//
// "Nothing in this cluster will ever back this volume up" is invisible in every other view: the PVC is
// healthy, its storage snapshots perfectly, its namespace has a Ready location, and no schedule
// touches it. The fixture provides three shapes of it — a namespace no selector reaches, a PVC an
// exclude glob drops, and a PVC whose only cover is a suspended schedule — because they are three
// different mistakes with three different remedies.
func TestCoverageReportsPVCsNoScheduleSelects(t *testing.T) {
	cov := collectCoverage(t, coverageFixture(t))
	rows := coverageRows(cov)

	// tenant-b is matched by no namespace selector at all.
	if r := rows["tenant-b/orphan-data"]; r.Selected || len(r.InertSchedules) > 0 {
		t.Errorf("tenant-b/orphan-data should be selected by nothing, got selected=%v inert=%v",
			r.Selected, r.InertSchedules)
	}
	// local-scratch is dropped by the cluster schedule's pvcSelector exclude glob, in a namespace the
	// selector DOES match — the case a namespace-granularity answer gets wrong.
	if r := rows["tenant-a/local-scratch"]; r.Selected {
		t.Error("tenant-a/local-scratch is excluded by the schedule's pvcSelector but reads as selected")
	}
	// paused-data is selected only by a suspended schedule: covered on paper, unprotected in fact.
	r := rows["tenant-b/paused-data"]
	if r.Selected {
		t.Error("tenant-b/paused-data is selected only by a suspended schedule but reads as protected")
	}
	if len(r.InertSchedules) == 0 {
		t.Error("tenant-b/paused-data lost the suspended schedule that selects it; " +
			"'nothing selects it' and 'the schedule is switched off' are different remedies")
	}

	if cov.Unselected != 2 {
		t.Errorf("Unselected = %d, want 2 (tenant-b/orphan-data and tenant-a/local-scratch)", cov.Unselected)
	}
	if cov.InertOnly != 1 {
		t.Errorf("InertOnly = %d, want 1 (tenant-b/paused-data)", cov.InertOnly)
	}
	// And the one that IS covered must read as covered, or the assertions above prove nothing.
	if !rows["tenant-a/rbd-data"].Selected {
		t.Error("tenant-a/rbd-data is selected by an active schedule but reads as unprotected")
	}
}

// TestCoverageStaticNonCSIVolume pins the case that makes a PER-PVC answer necessary rather than
// merely nicer than a per-StorageClass one.
//
// legacy-nfs is bound to a hand-made NFS PersistentVolume and names a StorageClass that does not
// exist — which is legal, common, and not a misconfiguration: for a static binding storageClassName is
// only a matching label between claim and volume, and Kubernetes never requires the class object. A
// report built on StorageClasses would either fail to find the class or read a class that has nothing
// to do with the volume. The resolver reads the PV, so the answer here is the honest one.
func TestCoverageStaticNonCSIVolume(t *testing.T) {
	cov := collectCoverage(t, coverageFixture(t))
	r := coverageRows(cov)["tenant-a/legacy-nfs"]

	if r.Class != CoverageUnsupported {
		t.Fatalf("class %q, want %q", r.Class, CoverageUnsupported)
	}
	// The resolver's own sentence, carried through, is what tells an administrator WHY — and it names
	// the volume source, which no field of this report carries.
	if !strings.Contains(r.Detail, "nfs") {
		t.Errorf("the detail does not name the non-CSI volume source, so the row is unactionable: %q",
			r.Detail)
	}
	if !r.Bound {
		t.Error("legacy-nfs is bound to a PersistentVolume but reads as unbound")
	}
}

// TestCoverageSeparatesPrecheckFromUnsupported is the distinction the whole taxonomy turns on.
//
// "This storage can never be snapshotted" is a note; "this cluster cannot snapshot it today" is work
// somebody has to do. Merging them — which is the obvious simplification, since both mean no backup
// tonight — would make the section useless for the only reader it has.
func TestCoverageSeparatesPrecheckFromUnsupported(t *testing.T) {
	cov := collectCoverage(t, coverageFixture(t))
	rows := coverageRows(cov)

	broken, unsupported := rows["tenant-a/broken-snap"], rows["tenant-a/local-scratch"]
	if broken.Class == unsupported.Class {
		t.Fatalf("a missing snapshotter Secret and a driver with no snapshot class collapsed into "+
			"one class (%q)", broken.Class)
	}
	if broken.Verdict != CoverageVerdictFailed {
		t.Errorf("broken-snap verdict %q, want %q — a fixable cluster fault is a failure, not a skip",
			broken.Verdict, CoverageVerdictFailed)
	}
	if unsupported.Verdict != CoverageVerdictSkipped {
		t.Errorf("local-scratch verdict %q, want %q", unsupported.Verdict, CoverageVerdictSkipped)
	}
	if !strings.Contains(broken.Detail, "does not exist") {
		t.Errorf("the pre-check detail does not say what is missing: %q", broken.Detail)
	}
}

// TestCoverageSnapshotAPIAbsentIsOneStatementNotN pins the state a fresh cluster is most often in:
// the VolumeSnapshot API cannot be used at all, so no volume's treatment is knowable.
//
// Letting each PVC fail on its own would produce one identical error per volume — three thousand of
// them on a real cluster — and a report nobody can read. One statement about the cluster is both
// smaller and truer, and it must not read as "backed up" in the meantime.
//
// WHICH statement is the part 0.6.7 had to split. The two causes were reported identically:
//
//   - the CRDs are not installed. The apiserver ANSWERED, and its answer is grave and true — nothing
//     in this cluster can be backed up until somebody installs the snapshot API.
//   - this identity may not list VolumeSnapshotClasses. The apiserver refused, and the report knows
//     precisely nothing. The operator has its own, wider role and may be backing everything up
//     perfectly.
//
// Sending a reader after a storage stack that was never broken is the failure this release exists to
// end, so the two are asserted apart here — while the "one statement, not N" property, which is
// orthogonal and was never wrong, is asserted for both.
//
// Both are simulated by failing the LIST rather than by omitting the CRDs from the fake client's
// scheme: controller-runtime's fake client answers an unregistered unstructured list with an empty
// result, not an error, so a scheme-based fixture would exercise the "zero snapshot classes exist"
// path (correctly CSISnapshotUnsupported) and prove nothing about either of these.
func TestCoverageSnapshotAPIAbsentIsOneStatementNotN(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   error
		class string
	}{
		{
			name: "the CRDs are not installed",
			err: &meta.NoKindMatchError{
				GroupKind:        schema.GroupKind{Group: "snapshot.storage.k8s.io", Kind: "VolumeSnapshotClass"},
				SearchedVersions: []string{"v1"},
			},
			class: CoverageSnapshotAPIAbsent,
		},
		{
			name: "the identity may not list them",
			err: apierrors.NewForbidden(
				schema.GroupResource{Group: "snapshot.storage.k8s.io", Resource: "volumesnapshotclasses"},
				"", fmt.Errorf("cannot list resource at the cluster scope")),
			class: CoverageUndetermined,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := coverageClient(t,
				nsObj("tenant-a", "acme"),
				covPVC("tenant-a", "rbd-data", "ceph-block", "pv-rbd", "1Gi"),
				covCSIPV("pv-rbd", covRBD, "ceph-block"),
				covPVC("tenant-a", "more-data", "ceph-block", "pv-rbd-2", "1Gi"),
				covCSIPV("pv-rbd-2", covRBD, "ceph-block"),
			)
			rep := collectUnreadable(t, base, map[string]error{"VolumeSnapshotClassList": tc.err})

			cov := rep.Coverage
			if cov == nil {
				t.Fatal("no coverage section at all; an unusable snapshot API must still produce a census")
			}
			if len(cov.Classes) != 1 || cov.Classes[0].Class != tc.class {
				t.Fatalf("classes %+v, want exactly one %q", cov.Classes, tc.class)
			}
			if cov.Classes[0].Count != 2 {
				t.Errorf("the single class covers %d PVCs, want 2", cov.Classes[0].Count)
			}
			if cov.Classes[0].Verdict == CoverageVerdictBackedUp {
				t.Error("an unusable snapshot API rendered as 'backed up', which is the one reading " +
					"this package exists to refuse")
			}
			// One diagnostic, not one per PVC.
			var covDiags int
			for _, d := range rep.Diagnostics {
				if d.Area == "coverage" {
					covDiags++
				}
			}
			if covDiags != 1 {
				t.Errorf("%d coverage diagnostics, want exactly 1 — the point is not to repeat the "+
					"same error per volume", covDiags)
			}
		})
	}
}

// TestARefusedReadIsNotAVerdictAboutTheData is this release's reason for existing, reproduced from
// the report that found it.
//
// A resident soak collector ran `selfcheck` daily for NINE DAYS under a ServiceAccount whose
// ClusterRole had no read on PersistentVolumes. Since 0.6.5 the exposer resolves a bound PVC's driver
// from its PV, so every resolution failed with the same Forbidden — and the census called all 30
// volumes ExposerUnresolvable and the headline said they "will NOT be backed up". Twenty-eight of
// them were being backed up successfully every single night. The report was not wrong about a fact;
// it reported the limits of its own eyesight as a property of somebody's data.
//
// So: a read that could not be answered gets its own class, and the headline may not count those
// volumes among the ones that will not be backed up. The error is a REAL apierrors Forbidden here,
// exactly as the apiserver sends it, because that is the shape the field failure had.
func TestARefusedReadIsNotAVerdictAboutTheData(t *testing.T) {
	rep := collectUnreadable(t, syntheticCoverageCluster(t, 30), map[string]error{
		"PersistentVolumeList": apierrors.NewForbidden(
			schema.GroupResource{Resource: "persistentvolumes"}, "",
			fmt.Errorf("User \"system:serviceaccount:crystal-backup-system:crystal-backup-soak\" "+
				"cannot list resource \"persistentvolumes\" in API group \"\" at the cluster scope")),
	})
	cov := rep.Coverage
	if cov == nil {
		t.Fatal("no census at all; a refused read must still produce one")
	}

	if len(cov.Classes) != 1 || cov.Classes[0].Class != CoverageUndetermined {
		t.Fatalf("classes %+v, want exactly one %q: the volumes' treatment was not determined, and "+
			"'unresolvable' is a claim about the storage this command is in no position to make",
			cov.Classes, CoverageUndetermined)
	}
	if cov.Classes[0].Count != 30 {
		t.Errorf("the class covers %d PVCs, want 30", cov.Classes[0].Count)
	}
	if cov.Classes[0].Verdict != CoverageVerdictUnknown {
		t.Errorf("verdict %q, want %q", cov.Classes[0].Verdict, CoverageVerdictUnknown)
	}
	// The API error survives into the row: without it the reader cannot tell a missing grant from an
	// apiserver having a bad minute, and those are different jobs.
	if d := coverageRows(cov)["bulk/data-0000"].Detail; !strings.Contains(d, "forbidden") {
		t.Errorf("the row does not carry the error that stopped the read: %q", d)
	}

	// The headline. This is the sentence nine days of reports got wrong.
	if strings.Contains(rep.Verdict.Summary, "will NOT be backed up") {
		t.Errorf("the verdict still condemns volumes whose treatment was never determined:\n  %s",
			rep.Verdict.Summary)
	}
	if !strings.Contains(rep.Verdict.Summary, "could not be determined") {
		t.Errorf("the verdict says nothing about the reads that were refused, so a reader has no "+
			"way to know the census is blind:\n  %s", rep.Verdict.Summary)
	}
}

// TestARefusedStorageClassReadIsNotAVerdictEither is the second read the census depends on, and the
// one the collector's role was ALSO missing (the same audit struck storageclasses off the same list
// on the same day). It reaches only unbound PVCs — there the class is the only evidence of a driver
// there is — but the failure mode was identical, so the answer has to be.
func TestARefusedStorageClassReadIsNotAVerdictEither(t *testing.T) {
	base := coverageClient(t,
		nsObj("tenant-a", "acme"),
		// Unbound: the one shape that makes the resolver read a StorageClass at all.
		covPVC("tenant-a", "pending-rbd", "ceph-block", "", "1Gi"),
		&storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "ceph-block"}, Provisioner: covRBD},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "csi-snap-creds", Namespace: "rook-ceph"}},
		// A schedule that takes it, so the refusal is the ONLY thing the headline can be reacting to.
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
	)
	rep := collectUnreadable(t, base, map[string]error{
		"StorageClassList": apierrors.NewForbidden(
			schema.GroupResource{Group: "storage.k8s.io", Resource: "storageclasses"}, "",
			fmt.Errorf("cannot list resource at the cluster scope")),
	})

	got := coverageByName(rep.Coverage)
	if got["tenant-a/pending-rbd"] != CoverageUndetermined {
		t.Errorf("class %q, want %q: the StorageClass was not readable, which says nothing about "+
			"whether this volume will be backed up", got["tenant-a/pending-rbd"], CoverageUndetermined)
	}
	if strings.Contains(rep.Verdict.Summary, "will NOT be backed up") {
		t.Errorf("the verdict condemns a volume whose class it could not read:\n  %s", rep.Verdict.Summary)
	}
}

// TestARefusedScheduleReadDoesNotInventUnprotectedVolumes is the THIRD place the same conflation
// lived, and it is not a treatment class at all — which is why it survived the first pass.
//
// "Selected by NO schedule" is the single most valuable line this section prints, and the census
// derives it by listing the schedules and seeing which PVCs they take. List refused: every PVC in the
// cluster reads as selected by nothing, the headline reports them among the volumes that will NOT be
// backed up, and the sentence is exactly as false as the class was — with no class involved. The
// count is not a verdict this report is entitled to reach when it could not read the schedules.
func TestARefusedScheduleReadDoesNotInventUnprotectedVolumes(t *testing.T) {
	rep := collectUnreadable(t, syntheticCoverageCluster(t, 5), map[string]error{
		"ClusterBackupScheduleList": apierrors.NewForbidden(
			schema.GroupResource{Group: "crystalbackup.io", Resource: "clusterbackupschedules"}, "",
			fmt.Errorf("cannot list resource at the cluster scope")),
	})

	if strings.Contains(rep.Verdict.Summary, "selected by NO schedule") {
		t.Errorf("the verdict reports volumes as selected by nothing when the schedules could not be "+
			"read; the schedule that selects all five is right there in the fixture:\n  %s",
			rep.Verdict.Summary)
	}
	if !strings.Contains(rep.Verdict.Summary, "which schedules select") {
		t.Errorf("the verdict does not say the selection could not be determined, so the missing "+
			"clause reads as good news:\n  %s", rep.Verdict.Summary)
	}
	if !rep.Coverage.SelectionUndetermined {
		t.Error("Coverage does not record that the selection is unknown, so every consumer of the " +
			"JSON reads Unselected as a measured zero")
	}

	// Both renderers, because the flag has to survive into the two things a human actually reads. The
	// page is where the earlier version of this sentence was boldest ("nothing in this cluster will
	// ever back them up"), so a JSON field nobody rendered would have fixed the least-read output.
	page, err := Render(rep)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Contains(string(page), "are selected by NO schedule") {
		t.Error("the page still states the unmeasured count as a finding")
	}
	if !strings.Contains(string(page), "could not be determined") {
		t.Error("the page does not say the selection is unknown")
	}
	if txt := string(RenderText(rep, TextOptions{})); !strings.Contains(txt, "COULD NOT BE DETERMINED") {
		t.Errorf("the text report does not qualify the selection counts:\n%s", txt)
	}
}

// TestCoverageKeepsGenuineUnresolvableApart is the guard on the other side of the same line: the new
// class must not swallow the finding it was split off from.
//
// A PVC bound to a PersistentVolume that is NOT THERE is not a refusal — the apiserver answered, and
// what it answered is that the object is gone. That is a real (if rare) finding about the cluster,
// the controller parks the volume and retries it to a deadline, and it must keep reading as
// ExposerUnresolvable. If this test ever goes green by turning everything into "undetermined", the
// census has stopped saying anything at all.
func TestCoverageKeepsGenuineUnresolvableApart(t *testing.T) {
	cov := collectCoverage(t, coverageFixture(t))
	got := coverageByName(cov)
	for _, name := range []string{"tenant-a/ghost-volume", "tenant-a/never-bound"} {
		if got[name] != CoverageUnresolved {
			t.Errorf("%s: class %q, want %q — a NotFound is the API ANSWERING, not refusing",
				name, got[name], CoverageUnresolved)
		}
	}
}

// TestCoverageScanCostIsIndependentOfPVCCount is the efficiency claim, measured rather than asserted
// in a comment.
//
// Registry.For LISTs every VolumeSnapshotClass and GETs a PersistentVolume per call. Called naively
// once per PVC that is 2N requests; the read-through reader is what makes it a constant. The bound is
// checked at two sizes so a per-PVC cost cannot hide inside a small fixture, and the constant is
// checked against the documented breakdown so a new List added to the scan has to be accounted for.
func TestCoverageScanCostIsIndependentOfPVCCount(t *testing.T) {
	small := collectCoverage(t, syntheticCoverageCluster(t, 10))
	large := collectCoverage(t, syntheticCoverageCluster(t, 400))

	if small.APIReads != large.APIReads {
		t.Errorf("the census cost %d requests for 10 PVCs and %d for 400: it scales with the number "+
			"of volumes, which is what coverage_reader.go exists to prevent",
			small.APIReads, large.APIReads)
	}
	// 5 Lists (PVC, namespace, BackupSchedule, ClusterBackupSchedule, PersistentVolume) + 1 List of
	// VolumeSnapshotClasses + 1 Get of the snapshotter Secret. StorageClasses are not listed here
	// because every PVC in the synthetic cluster is bound, so the resolver never consults a class.
	const wantMax = 8
	if large.APIReads > wantMax {
		t.Errorf("the census made %d requests for 400 PVCs, more than the %d the documented "+
			"breakdown accounts for", large.APIReads, wantMax)
	}
	if large.PVCs != 400 {
		t.Fatalf("the synthetic cluster produced %d rows, not 400 — the cost claim is meaningless "+
			"if the scan did not walk them", large.PVCs)
	}
}

// TestCoverageTruncationNeverHidesAFinding pins the interaction between the row cap and the ordering.
//
// maxCoverageItems is a hard cap on the rows the document carries, and on a cluster with more PVCs than
// that the truncation decides what an administrator gets to see. If the cap dropped the one PVC nothing
// selects while keeping five hundred that are fine, every count in the section would still say the
// finding was there and no row would show it — the exact shape of "reported and invisible" this section
// exists to end.
func TestCoverageTruncationNeverHidesAFinding(t *testing.T) {
	// The bulk sits on CephFS, i.e. in the cephfs-shallow class, which sorts ahead of csi-generic. That
	// is deliberate: it is what makes an ordering that keys on the treatment class before the finding
	// push the row below off the end of the cap, and therefore what makes this test able to fail.
	c := syntheticCoverageClusterOn(t, maxCoverageItems+200, covCephFS, "ceph-fs")
	// One healthy csi-generic volume in a namespace no schedule reaches. Its name also sorts LAST
	// alphabetically, so identity order alone would put it off the end of the cap too.
	base := coverageClient(t, nsObj("forgotten", "acme"),
		covPVC("forgotten", "zzz-nobody-selects-me", "ceph-block", "pv-forgotten", "1Gi"),
		covCSIPV("pv-forgotten", covRBD, "ceph-block"))
	cov := collectCoverage(t, twoClusterReader{c, base})

	if cov.ItemsOmitted == 0 {
		t.Fatal("the fixture did not exceed the row cap, so this test proves nothing about truncation")
	}
	for _, it := range cov.Items {
		if it.Name == "zzz-nobody-selects-me" {
			return
		}
	}
	t.Errorf("the unselected PVC was truncated away: %d rows kept, %d omitted",
		len(cov.Items), cov.ItemsOmitted)
}

// twoClusterReader reads from two fake clients, first one then the other. It is the cheapest way to
// bolt one extra object onto a synthetic cluster of five hundred without rebuilding the fixture
// builder around a variadic tail — and, incidentally, it exercises the census over a Reader that is not
// a full client.Client, which is what Options.Reader's type promises.
type twoClusterReader struct {
	primary, secondary client.Client
}

func (r twoClusterReader) Get(
	ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption,
) error {
	if err := r.primary.Get(ctx, key, obj, opts...); err == nil {
		return nil
	}
	return r.secondary.Get(ctx, key, obj, opts...)
}

func (r twoClusterReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if err := r.primary.List(ctx, list, opts...); err != nil {
		return err
	}
	items, err := meta.ExtractList(list)
	if err != nil {
		return err
	}
	second := list.DeepCopyObject().(client.ObjectList) //nolint:errcheck // DeepCopyObject of a list is a list
	if err := r.secondary.List(ctx, second, opts...); err != nil {
		return err
	}
	more, err := meta.ExtractList(second)
	if err != nil {
		return err
	}
	return meta.SetList(list, append(items, more...))
}

// TestCoverageReasonsMatchTheController pins the three reason strings this package repeats.
//
// They are the Backup controller's own constants and they are unexported, in a file this lot does not
// own. Repeating a string is survivable; repeating it and letting it drift is not — a report claiming
// the controller will record CSISnapshotUnsupported when it records something else is worse than a
// report that says nothing. Two of the three are already documented there as verbatim cross-repo
// contracts asserted by the crucible, which is what makes this check cheap.
func TestCoverageReasonsMatchTheController(t *testing.T) {
	const source = "../controller/backup_controller.go"
	raw, err := os.ReadFile(source)
	if err != nil {
		t.Fatalf("read %s: %v", source, err)
	}
	body := string(raw)
	for _, want := range []string{CoverageUnsupported, CoveragePrecheckFailed, CoverageUnresolved} {
		if !strings.Contains(body, `= "`+want+`"`) {
			t.Errorf("%s no longer declares the reason %q, so this package's coverage class is "+
				"describing a phase/reason pair the controller will never record. Move the constant "+
				"somewhere both can import.", source, want)
		}
	}
}

// TestCoverageAndPlanAreRedacted is the guard that makes the whole-document redaction test mean
// something for these two new sections.
//
// TestDefaultModeLeavesNoPlaintextIdentifier asserts that no plaintext identifier survives the default
// mode, and it would pass just as happily over a report whose coverage section was EMPTY. So this test
// asserts the sections have content, that the content is tokens, and — the part per-field redaction
// cannot reach — that a resolver's sentence carried into a row has had the PVC and namespace it names
// substituted out of the prose.
func TestCoverageAndPlanAreRedacted(t *testing.T) {
	rep := collectFixture(t, false)

	if rep.Coverage == nil || rep.Coverage.PVCs == 0 {
		t.Fatal("the fixture produced no PVC census, so the redaction assertions below prove nothing")
	}
	if rep.Plan == nil || len(rep.Plan.Schedules) == 0 {
		t.Fatal("the fixture produced no plan, so the redaction assertions below prove nothing")
	}

	for _, it := range rep.Coverage.Items {
		if !strings.HasPrefix(it.Namespace, "ns-") {
			t.Errorf("a census row carries a plaintext namespace: %q", it.Namespace)
		}
		if !strings.HasPrefix(it.Name, "pvc-") {
			t.Errorf("a census row carries a plaintext PVC name: %q", it.Name)
		}
		// The StorageClass survives redaction on purpose: it is a platform-chosen identifier and the
		// most useful column in the table. If that ever stops being true, this is where to argue it.
		if it.StorageClass == "" {
			t.Errorf("%s/%s lost its StorageClass, which is the column this table is read by",
				it.Namespace, it.Name)
		}
	}

	var details int
	for _, it := range rep.Coverage.Items {
		if it.Detail == "" {
			continue
		}
		details++
		assertNoSecrets(t, "coverage row detail", it.Detail)
	}
	if details == 0 {
		t.Error("no census row carried a resolver sentence, so the free-text redaction path — the " +
			"one per-field redaction cannot reach — was never exercised")
	}

	for _, s := range rep.Plan.Schedules {
		assertNoSecrets(t, "plan schedule sentences", strings.Join(s.Sentences, "\n"))
		assertNoSecrets(t, "plan schedule problems", strings.Join(s.Problems, "\n"))
	}
	for _, l := range rep.Plan.Locations {
		assertNoSecrets(t, "plan location sentences", strings.Join(l.Sentences, "\n"))
	}
}

// --- fixtures -------------------------------------------------------------------------------

// collectCoverageReport runs a full collection over c. Full: true so the assertions can name real
// PVCs; the redaction of these sections is asserted separately, over the shared fixture, by the tests
// that own that subject.
func collectCoverageReport(t *testing.T, c client.Reader) *Report {
	t.Helper()
	rep, err := Collect(context.Background(), Options{
		Reader: c, OperatorNamespace: operatorNS, Now: fixtureNow(), Full: true,
	})
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	return rep
}

// collectUnreadable collects over a cluster some of whose kinds cannot be read, each with the exact
// error the apiserver would send. The error matters as much as the kind: a Forbidden, a NotFound and
// a timeout are three different statements, and this package's whole job in this area is to stop
// treating them as one.
func collectUnreadable(t *testing.T, c client.Reader, kinds map[string]error) *Report {
	t.Helper()
	rep, err := Collect(context.Background(), Options{
		Reader:            unreadableReader{Reader: c, kinds: kinds},
		OperatorNamespace: operatorNS, Now: fixtureNow(), Full: true,
	})
	if err != nil {
		t.Fatalf("a cluster this command cannot fully read must still produce a report: %v", err)
	}
	return rep
}

// unreadableReader is forbiddenReader with the error chosen by the caller. The two are kept apart
// rather than merged because forbiddenReader's plain fmt error is itself a useful fixture — it is
// what proves the new class does not depend on apierrors.IsForbidden being able to recognise the
// failure.
type unreadableReader struct {
	client.Reader
	kinds map[string]error
}

func (u unreadableReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	kind := fmt.Sprintf("%T", list)
	if i := strings.LastIndex(kind, "."); i >= 0 {
		kind = kind[i+1:]
	}
	if ul, ok := list.(*unstructured.UnstructuredList); ok {
		kind = ul.GetObjectKind().GroupVersionKind().Kind
	}
	if err := u.kinds[kind]; err != nil {
		return err
	}
	return u.Reader.List(ctx, list, opts...)
}

// collectCoverage is collectCoverageReport narrowed to the census, failing the test when there is none.
func collectCoverage(t *testing.T, c client.Reader) *Coverage {
	t.Helper()
	rep := collectCoverageReport(t, c)
	if rep.Coverage == nil {
		t.Fatal("the report carries no coverage section")
	}
	return rep.Coverage
}

// cbv1Retention / cbv1RetentionEmpty build the keep policies the retention-sentence test asserts over.
func cbv1Retention(daily, weekly, monthly int32) cbv1.RetentionSpec {
	return cbv1.RetentionSpec{KeepDaily: daily, KeepWeekly: weekly, KeepMonthly: monthly}
}

func cbv1RetentionEmpty() cbv1.RetentionSpec { return cbv1.RetentionSpec{} }

func coverageByName(cov *Coverage) map[string]string {
	out := map[string]string{}
	for _, it := range cov.Items {
		out[it.Namespace+"/"+it.Name] = it.Class
	}
	return out
}

func coverageRows(cov *Coverage) map[string]CoveredPVC {
	out := map[string]CoveredPVC{}
	for _, it := range cov.Items {
		out[it.Namespace+"/"+it.Name] = it
	}
	return out
}

// coverageFixture is one cluster carrying one PVC per class, plus the three shapes of "nothing selects
// this".
//
// tenant-a is selected by an active ClusterBackupSchedule whose pvcSelector excludes `local-*`;
// tenant-b is selected by nothing except a SUSPENDED schedule that takes only `paused-*`.
func coverageFixture(t *testing.T) client.Client {
	t.Helper()
	return coverageClient(t,
		nsObj("tenant-a", "acme"),
		nsObj("tenant-b", "acme"),

		// A location, so the plan section has something to describe and the schedules below resolve.
		&cbv1.ClusterBackupLocation{
			ObjectMeta: metav1.ObjectMeta{Name: "dr", CreationTimestamp: metav1.NewTime(fixtureNow())},
			Spec: cbv1.ClusterBackupLocationSpec{
				Default: true, ClusterID: "c1", Mode: cbv1.LocationModeStandard,
				S3: cbv1.S3Spec{Endpoint: "https://s3.example:9000", Bucket: "b"},
				Maintenance: &cbv1.MaintenanceSpec{
					CheckSchedule: "0 3 * * *", PruneSchedule: "0 4 * * 0",
				},
				Retention: cbv1.RetentionSpec{KeepDaily: 7, KeepWeekly: 4},
			},
		},

		// tenant-a: active, everything except local-* .
		&cbv1.ClusterBackupSchedule{
			ObjectMeta: metav1.ObjectMeta{Name: "nightly", CreationTimestamp: metav1.NewTime(fixtureNow())},
			Spec: cbv1.ClusterBackupScheduleSpec{
				Schedule: "0 2 * * *",
				Template: cbv1.ClusterBackupTemplate{Spec: cbv1.ClusterBackupRunSpec{
					LocationRef: cbv1.LocalObjectReference{Name: "dr"},
					Namespaces:  cbv1.NamespaceSelector{MatchNames: []string{"tenant-a"}},
					BackupRunSpec: cbv1.BackupRunSpec{
						PVCSelector: cbv1.PVCSelector{Exclude: []string{"local-*"}},
					},
				}},
			},
		},
		// tenant-b: suspended, and only takes paused-* .
		&cbv1.ClusterBackupSchedule{
			ObjectMeta: metav1.ObjectMeta{Name: "switched-off", CreationTimestamp: metav1.NewTime(fixtureNow())},
			Spec: cbv1.ClusterBackupScheduleSpec{
				Schedule: "0 5 * * *",
				Paused:   true,
				Template: cbv1.ClusterBackupTemplate{Spec: cbv1.ClusterBackupRunSpec{
					LocationRef: cbv1.LocalObjectReference{Name: "dr"},
					Namespaces:  cbv1.NamespaceSelector{MatchNames: []string{"tenant-b"}},
					BackupRunSpec: cbv1.BackupRunSpec{
						PVCSelector: cbv1.PVCSelector{Include: []string{"paused-*"}},
					},
				}},
			},
		},

		// The snapshotter Secret the healthy classes name. Its presence is what makes the
		// SnapshotPrecheckFailed row below a statement about the OTHER class rather than about the
		// fixture being incomplete.
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "csi-snap-creds", Namespace: "rook-ceph"}},

		// One PVC per class.
		covPVC("tenant-a", "rbd-data", "ceph-block", "pv-rbd", "10Gi"),
		covCSIPV("pv-rbd", covRBD, "ceph-block"),
		covPVC("tenant-a", "cephfs-media", "ceph-fs", "pv-cephfs", "50Gi"),
		covCSIPV("pv-cephfs", covCephFS, "ceph-fs"),
		covPVC("tenant-a", "local-scratch", "local-path", "pv-local", "1Gi"),
		covCSIPV("pv-local", covLocalPath, "local-path"),
		covPVC("tenant-a", "broken-snap", "broken", "pv-broken", "5Gi"),
		covCSIPV("pv-broken", covBroken, "broken"),
		// Bound to a hand-made NFS volume, naming a StorageClass that does not exist. Legal, common,
		// and the case a per-StorageClass answer cannot get right.
		covPVC("tenant-a", "legacy-nfs", "nfs-legacy", "pv-nfs", "100Gi"),
		covNFSPV("pv-nfs", "nfs-legacy"),
		// Bound to a PersistentVolume that is not there: neither a verdict about the storage nor a
		// refused pre-check, so ExposerUnresolvable.
		covPVC("tenant-a", "ghost-volume", "ceph-block", "pv-vanished", "1Gi"),
		// Unbound, naming a StorageClass that does not exist: the other ExposerUnresolvable shape.
		covPVC("tenant-a", "never-bound", "deleted-class", "", "1Gi"),
		// Unbound but naming a class that DOES exist. This is the one path on which the resolver reads
		// a StorageClass rather than a PersistentVolume, and it must still produce a real verdict —
		// there is no data to back up yet, but the treatment this volume will get is knowable now.
		covPVC("tenant-a", "pending-rbd", "ceph-block", "", "1Gi"),
		&storagev1.StorageClass{
			ObjectMeta:  metav1.ObjectMeta{Name: "ceph-block"},
			Provisioner: covRBD,
		},

		covPVC("tenant-b", "orphan-data", "ceph-block", "pv-orphan", "20Gi"),
		covCSIPV("pv-orphan", covRBD, "ceph-block"),
		covPVC("tenant-b", "paused-data", "ceph-block", "pv-paused", "20Gi"),
		covCSIPV("pv-paused", covRBD, "ceph-block"),

		// The operator's own temporary exposure PVC, which must never be counted as user data.
		covOperatorPVC("crystal-clone-temp"),
	)
}

// syntheticCoverageCluster is n identical, healthy, selected PVCs. It exists for the cost and
// compactness tests, which need scale and nothing else.
func syntheticCoverageCluster(t *testing.T, n int) client.Client {
	t.Helper()
	return syntheticCoverageClusterOn(t, n, covRBD, "ceph-block")
}

// syntheticCoverageClusterOn is syntheticCoverageCluster with the driver chosen, so a test can put the
// bulk of a cluster in a treatment class that sorts BEFORE csi-generic. That is what makes the
// truncation ordering testable at all: with everything in one class, class-first and attention-first
// orderings produce the same answer and a mutation to either survives.
func syntheticCoverageClusterOn(t *testing.T, n int, driver, storageClass string) client.Client {
	t.Helper()
	objs := make([]client.Object, 0, 4+2*n)
	objs = append(objs,
		nsObj("bulk", "acme"),
		&cbv1.ClusterBackupLocation{
			ObjectMeta: metav1.ObjectMeta{Name: "dr", CreationTimestamp: metav1.NewTime(fixtureNow())},
			Spec: cbv1.ClusterBackupLocationSpec{
				Default: true, ClusterID: "c1", Mode: cbv1.LocationModeStandard,
				S3:        cbv1.S3Spec{Endpoint: "https://s3.example:9000", Bucket: "b"},
				Retention: cbv1.RetentionSpec{KeepDaily: 7},
			},
			Status: cbv1.ClusterBackupLocationStatus{
				Phase: "Ready",
				Conditions: []metav1.Condition{{
					Type: conditionReady, Status: metav1.ConditionTrue, Reason: "Ready",
					LastTransitionTime: metav1.NewTime(fixtureNow()),
				}},
			},
		},
		&cbv1.ClusterBackupSchedule{
			ObjectMeta: metav1.ObjectMeta{Name: "nightly", CreationTimestamp: metav1.NewTime(fixtureNow())},
			Spec: cbv1.ClusterBackupScheduleSpec{
				Schedule: "0 2 * * *",
				Template: cbv1.ClusterBackupTemplate{Spec: cbv1.ClusterBackupRunSpec{
					LocationRef: cbv1.LocalObjectReference{Name: "dr"},
					Namespaces:  cbv1.NamespaceSelector{MatchNames: []string{"bulk"}},
				}},
			},
			Status: cbv1.ClusterBackupScheduleStatus{
				LastSuccessTime: ptrTime(fixtureNow().Add(-2 * time.Hour)),
				Conditions: []metav1.Condition{{
					Type: conditionReady, Status: metav1.ConditionTrue, Reason: "Scheduled",
					LastTransitionTime: metav1.NewTime(fixtureNow()),
				}},
			},
		},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "csi-snap-creds", Namespace: "rook-ceph"}},
	)
	for i := range n {
		pv := fmt.Sprintf("pv-bulk-%04d", i)
		objs = append(objs,
			covPVC("bulk", fmt.Sprintf("data-%04d", i), storageClass, pv, "1Gi"),
			covCSIPV(pv, driver, storageClass))
	}
	return coverageClient(t, objs...)
}

// coverageClient builds the fake client, seeding the unstructured VolumeSnapshotClasses through
// Create for the reason internal/exposer's own tests document: the fake client registers an
// unstructured object's GVK when it first sees it, so no upfront scheme entry is needed for the
// out-of-module snapshot CRDs.
func coverageClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	c := snapshotScheme(t, objs...)
	ctx := context.Background()
	for _, vsc := range []*unstructured.Unstructured{
		covSnapClass("csi-rbdplugin-snapclass", covRBD, "rook-ceph", "csi-snap-creds"),
		covSnapClass("csi-cephfsplugin-snapclass", covCephFS, "rook-ceph", "csi-snap-creds"),
		// Names a Secret that does not exist: the SnapshotPrecheckFailed class.
		covSnapClass("broken-snapclass", covBroken, "rook-ceph", "vanished-creds"),
	} {
		if err := c.Create(ctx, vsc); err != nil {
			t.Fatalf("seed VolumeSnapshotClass %s: %v", vsc.GetName(), err)
		}
	}
	return c
}

func covSnapClass(name, driver, secretNS, secretName string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshotClass",
	})
	u.SetName(name)
	if err := unstructured.SetNestedField(u.Object, driver, "driver"); err != nil {
		panic(err)
	}
	params := map[string]any{
		"csi.storage.k8s.io/snapshotter-secret-name":      secretName,
		"csi.storage.k8s.io/snapshotter-secret-namespace": secretNS,
	}
	if err := unstructured.SetNestedMap(u.Object, params, "parameters"); err != nil {
		panic(err)
	}
	return u
}

func covPVC(namespace, name, storageClass, volumeName, capacity string) *corev1.PersistentVolumeClaim {
	sc := storageClass
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: corev1.PersistentVolumeClaimSpec{
			StorageClassName: &sc,
			VolumeName:       volumeName,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(capacity)},
			},
		},
	}
}

func covOperatorPVC(name string) *corev1.PersistentVolumeClaim {
	p := covPVC(operatorNS, name, "ceph-block", "", "1Gi")
	p.Labels = map[string]string{apiconst.LabelManagedBy: apiconst.ManagedByValue}
	return p
}

func covCSIPV(name, driver, storageClass string) *corev1.PersistentVolume {
	return &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: corev1.PersistentVolumeSpec{
			StorageClassName: storageClass,
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{Driver: driver},
			},
		},
	}
}

func covNFSPV(name, storageClass string) *corev1.PersistentVolume {
	return &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: corev1.PersistentVolumeSpec{
			StorageClassName: storageClass,
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				NFS: &corev1.NFSVolumeSource{Server: "nfs.internal", Path: "/exports/legacy"},
			},
		},
	}
}

func ptrTime(t time.Time) *metav1.Time {
	mt := metav1.NewTime(t)
	return &mt
}
