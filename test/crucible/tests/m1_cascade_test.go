//go:build crucible

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

package crucible

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
	"github.com/CrystalBackup/CrystalBackup/internal/restic"
)

// ---------------------------------------------------------------------------
// M1 acceptance — features/m1_cascade.feature, one Ginkgo It per Scenario.
//
// The four scenarios share one expensive fact: a completed cluster-DR run that
// a ClusterBackupSchedule fanned out over the six seeded tenant namespaces. That
// run is produced once in BeforeAll (Ordered container) and every scenario reads
// it back — the fan-out shape (Scenario 1), the repository identity of each
// snapshottable PVC (Scenario 2), the Skipped-not-Failed local-path volume
// (Scenario 3), and the clean terminal phase of a PVC-less namespace (Scenario 4).
//
// These specs are written BEFORE the M1 controllers (TDD red): with no controller
// the schedule never stamps out a run, so BeforeAll times out and the whole
// container goes red — intended. The assertions target the true M1 contract
// (pinned labels/phases/restic identity), never a weakened shape.
// ---------------------------------------------------------------------------

var (
	// m1CascadeScheduleName is the ClusterBackupSchedule this feature drives; the run it
	// stamps out is named "<schedule>-<UTC timestamp>" (apiconst.RunName) and reused verbatim
	// as the ClusterBackup name, every child Backup name, and the restic "run" tag.
	m1CascadeScheduleName = "crucible-cascade"
	// m1CascadeRunName is the pinned run R, captured once in BeforeAll and read by every scenario.
	m1CascadeRunName string
	// m1CascadeLocationCreated records whether THIS suite created the "dr" location (so AfterAll
	// only deletes what it made, leaving a pre-existing / sibling-owned location alone).
	m1CascadeLocationCreated bool
)

// m1CascadeSeedNamespaces are the six seeded tenant namespaces of the cascade feature's
// Background table, in feature order.
var m1CascadeSeedNamespaces = []string{"c-db", "c-media", "c-edge", "c-legacy", "c-web", "c-empty"}

var _ = Describe("M1 — cluster-DR backup cascade", Label("m1"), Ordered, func() {

	BeforeAll(func() {
		m1SkipIfNoS3()

		By("Given an initialized ClusterBackupLocation \"dr\" for the shared repository")
		m1EnsurePlatformSecrets()
		var loc cbv1.ClusterBackupLocation
		err := k8s.Get(ctx, client.ObjectKey{Name: m1LocationName}, &loc)
		switch {
		case apierrors.IsNotFound(err):
			m1CreateLocation(m1LocationName, true)
			m1CascadeLocationCreated = true
		default:
			Expect(err).NotTo(HaveOccurred(), "get ClusterBackupLocation %s", m1LocationName)
		}
		m1WaitRepositoryInitialized(m1LocationName)

		By("And the seeded tenant namespaces labelled crystalbackup.io/seed=crucible")
		for _, ns := range m1CascadeSeedNamespaces {
			var n corev1.Namespace
			Expect(k8s.Get(ctx, client.ObjectKey{Name: ns}, &n)).To(Succeed(),
				"seed namespace %s must exist (run the crucible seed step first)", ns)
			Expect(n.Labels).To(HaveKeyWithValue(m1SeedLabel, m1SeedValue),
				"seed namespace %s must carry %s=%s", ns, m1SeedLabel, m1SeedValue)
		}

		// Create the ClusterBackupSchedule selecting the seed label. A leftover from an aborted
		// run is cleared first (its cascade GC's any old runs), then we retry Create until the
		// slot is free.
		sched := &cbv1.ClusterBackupSchedule{
			ObjectMeta: metav1.ObjectMeta{Name: m1CascadeScheduleName},
			Spec: cbv1.ClusterBackupScheduleSpec{
				Schedule: "* * * * *", // fire every minute; Forbid (the CRD default) keeps runs from overlapping
				Template: cbv1.ClusterBackupTemplate{
					Spec: cbv1.ClusterBackupRunSpec{
						LocationRef: cbv1.LocalObjectReference{Name: m1LocationName},
						Namespaces: cbv1.NamespaceSelector{
							MatchLabels: map[string]string{m1SeedLabel: m1SeedValue},
						},
					},
				},
			},
		}
		_ = k8s.Delete(ctx, &cbv1.ClusterBackupSchedule{ObjectMeta: metav1.ObjectMeta{Name: m1CascadeScheduleName}})
		Eventually(func() error {
			err := k8s.Create(ctx, sched)
			if apierrors.IsAlreadyExists(err) {
				return fmt.Errorf("ClusterBackupSchedule %s still terminating from a previous run", m1CascadeScheduleName)
			}
			return err
		}, time.Minute, 2*time.Second).Should(Succeed())

		// Capture the first ClusterBackup the schedule stamps out — the run R every scenario shares.
		Eventually(func(g Gomega) {
			var runs cbv1.ClusterBackupList
			g.Expect(k8s.List(ctx, &runs)).To(Succeed())
			found := ""
			var foundTime metav1.Time
			for i := range runs.Items {
				r := runs.Items[i]
				owned := r.Spec.ScheduleRef == m1CascadeScheduleName ||
					r.Labels[apiconst.LabelSchedule] == m1CascadeScheduleName ||
					strings.HasPrefix(r.Name, m1CascadeScheduleName+"-")
				if owned && (found == "" || r.CreationTimestamp.Before(&foundTime)) {
					found = r.Name
					foundTime = r.CreationTimestamp
				}
			}
			g.Expect(found).NotTo(BeEmpty(),
				"ClusterBackupSchedule %s has not stamped out a ClusterBackup run yet", m1CascadeScheduleName)
			m1CascadeRunName = found
		}, 5*time.Minute, 5*time.Second).Should(Succeed())

		// Pause the schedule so exactly ONE run backs the assertions (the in-flight run R keeps going).
		Eventually(func(g Gomega) {
			var s cbv1.ClusterBackupSchedule
			g.Expect(k8s.Get(ctx, client.ObjectKey{Name: m1CascadeScheduleName}, &s)).To(Succeed())
			s.Spec.Paused = true
			g.Expect(k8s.Update(ctx, &s)).To(Succeed())
		}, time.Minute, 3*time.Second).Should(Succeed())

		// Scenario 1: the run reaches a terminal phase within 20 minutes.
		m1WaitClusterBackupTerminal(m1CascadeRunName, 20*time.Minute)
	})

	AfterAll(func() {
		_ = k8s.Delete(ctx, &cbv1.ClusterBackupSchedule{ObjectMeta: metav1.ObjectMeta{Name: m1CascadeScheduleName}})
		if m1CascadeRunName != "" {
			_ = k8s.Delete(ctx, &cbv1.ClusterBackup{ObjectMeta: metav1.ObjectMeta{Name: m1CascadeRunName}})
		}
		// Child Backups are label-linked, not owned, so they never cascade — delete them explicitly.
		for _, ns := range m1CascadeSeedNamespaces {
			_ = k8s.DeleteAllOf(ctx, &cbv1.Backup{}, client.InNamespace(ns),
				client.MatchingLabels{apiconst.LabelOrigin: apiconst.OriginCluster})
		}
		if m1CascadeLocationCreated {
			m1DeleteLocation(m1LocationName)
		}
	})

	It("A ClusterBackupSchedule fans out a Backup into every matched namespace", func() {
		By("When I create a ClusterBackupSchedule selecting crystalbackup.io/seed=crucible")
		By("And a run is triggered")

		By("Then a ClusterBackup run is created and reaches a terminal phase within 20 minutes")
		var run cbv1.ClusterBackup
		Expect(k8s.Get(ctx, client.ObjectKey{Name: m1CascadeRunName}, &run)).To(Succeed())
		Expect(run.Status.Phase).To(BeElementOf("Completed", "PartiallyFailed", "Failed"),
			"run %s must be in a terminal phase", m1CascadeRunName)

		By("And a Backup named after the run exists in each of the 6 matched namespaces")
		Expect(m1CascadeSeedNamespaces).To(HaveLen(6))
		children := make(map[string]cbv1.Backup, len(m1CascadeSeedNamespaces))
		for _, ns := range m1CascadeSeedNamespaces {
			var child cbv1.Backup
			Expect(k8s.Get(ctx, client.ObjectKey{Namespace: ns, Name: m1CascadeRunName}, &child)).To(Succeed(),
				"child Backup %s/%s (named after the run) must exist", ns, m1CascadeRunName)
			children[ns] = child
		}

		By("And every child Backup carries labels crystalbackup.io/cluster-backup=<run> and crystalbackup.io/origin=cluster")
		for ns, child := range children {
			Expect(child.Labels).To(HaveKeyWithValue(apiconst.LabelClusterBackup, m1CascadeRunName),
				"child Backup %s/%s must link to its run by label", ns, child.Name)
			Expect(child.Labels).To(HaveKeyWithValue(apiconst.LabelOrigin, apiconst.OriginCluster),
				"child Backup %s/%s must be marked cluster-origin", ns, child.Name)
		}

		By("And no child Backup has an ownerReference to the ClusterBackup (the cross-namespace link is by label only)")
		for ns, child := range children {
			for _, ref := range child.OwnerReferences {
				Expect(ref.Kind).NotTo(Equal("ClusterBackup"),
					"child Backup %s/%s must not be owned by the ClusterBackup", ns, child.Name)
				Expect(ref.UID).NotTo(Equal(run.UID),
					"child Backup %s/%s must not reference the ClusterBackup UID", ns, child.Name)
			}
		}

		By("And the ClusterBackup status has namespacesMatched=6 and pvcsSucceeded greater than zero")
		Expect(run.Status.NamespacesMatched).To(Equal(int32(6)))
		Expect(run.Status.PVCsSucceeded).To(BeNumerically(">", 0))
	})

	It("Every snapshottable PVC lands in the shared repository with the correct restic identity", func() {
		By("Given a completed ClusterBackup run \"R\"")
		Expect(m1CascadeRunName).NotTo(BeEmpty())

		By("When I list the repository snapshots with the platform DEK")
		snaps := m1ResticSnapshots(m1LocationName)
		Expect(snaps).NotTo(BeEmpty(), "the run must have written snapshots to the shared repository")

		By("Then there is a data snapshot for c-db's PVC with host \"crucible\", path \"/data/c-db/<pvc>\", and tags including \"namespace=c-db\", \"pvc=<pvc>\", \"kind=data\", \"run=R\"")
		dbPVCs := m1CascadePVCs("c-db")
		Expect(dbPVCs).NotTo(BeEmpty(), "c-db's StatefulSet PVCs must exist")
		for _, pvc := range dbPVCs {
			snap, ok := m1CascadeDataSnapshot(snaps, "c-db", pvc.Name, m1CascadeRunName)
			Expect(ok).To(BeTrue(), "no data snapshot for c-db/%s in run %s", pvc.Name, m1CascadeRunName)
			Expect(snap.Host).To(Equal(m1ClusterID))
			Expect(snap.Paths).To(ContainElement("/data/c-db/" + pvc.Name))
			Expect(snap.Tags).To(ContainElement(restic.Tag(restic.TagKeyNamespace, "c-db")))
			Expect(snap.Tags).To(ContainElement(restic.Tag(restic.TagKeyPVC, pvc.Name)))
			Expect(snap.Tags).To(ContainElement(restic.Tag(restic.TagKeyKind, restic.KindData)))
			Expect(snap.Tags).To(ContainElement(restic.Tag(restic.TagKeyRun, m1CascadeRunName)))
		}

		By("And there is a data snapshot for c-media's RWX cephfs volume")
		mediaRWX := ""
		for _, pvc := range m1CascadePVCs("c-media") {
			for _, mode := range pvc.Spec.AccessModes {
				if mode == corev1.ReadWriteMany {
					mediaRWX = pvc.Name
				}
			}
		}
		Expect(mediaRWX).NotTo(BeEmpty(), "c-media must have an RWX (cephfs) PVC")
		_, ok := m1CascadeDataSnapshot(snaps, "c-media", mediaRWX, m1CascadeRunName)
		Expect(ok).To(BeTrue(), "no data snapshot for c-media/%s (RWX cephfs) in run %s", mediaRWX, m1CascadeRunName)

		By("And there is a data snapshot for c-edge's longhorn volume")
		Expect(m1CascadePVCNames("c-edge")).To(ContainElement("edge-data"),
			"c-edge must have the longhorn edge-data PVC")
		edgeSnap, ok := m1CascadeDataSnapshot(snaps, "c-edge", "edge-data", m1CascadeRunName)
		Expect(ok).To(BeTrue(), "no data snapshot for c-edge/edge-data (longhorn) in run %s", m1CascadeRunName)

		By("And c-edge's exotic data is preserved (the mover stored xattrs and kept hardlinks/symlinks)")
		nodes := m1CascadeLsNodes(m1LocationName, edgeSnap.ID)
		Expect(nodes).NotTo(BeEmpty(), "restic ls must return the snapshot tree")
		base := "/data/c-edge/edge-data/exotic/"

		sym, ok := m1CascadeNodeByPath(nodes, base+"plain.symlink")
		Expect(ok).To(BeTrue(), "the symlink must be in the snapshot tree")
		Expect(sym.Type).To(Equal("symlink"), "plain.symlink must be kept as a symlink, not dereferenced")
		Expect(sym.LinkTarget).To(Equal("plain.txt"))

		broken, ok := m1CascadeNodeByPath(nodes, base+"broken.symlink")
		Expect(ok).To(BeTrue(), "the dangling symlink must be in the snapshot tree")
		Expect(broken.Type).To(Equal("symlink"))
		Expect(broken.LinkTarget).To(Equal("/nonexistent/target"))

		plain, ok := m1CascadeNodeByPath(nodes, base+"plain.txt")
		Expect(ok).To(BeTrue(), "the hardlink target must be in the snapshot tree")
		hard, ok := m1CascadeNodeByPath(nodes, base+"plain.hardlink")
		Expect(ok).To(BeTrue(), "the hard-linked file must be in the snapshot tree")
		Expect(plain.Inode).NotTo(BeZero(), "restic must record inode numbers to reconstruct hardlinks")
		Expect(hard.Inode).To(Equal(plain.Inode),
			"the hardlink must share its target's inode (link kept, not copied)")

		unicode := false
		for _, n := range nodes {
			if strings.Contains(n.Path, "übergroß") {
				unicode = true
			}
		}
		Expect(unicode).To(BeTrue(), "unicode filenames must be preserved")
	})

	It("A volume on storage without snapshot support is Skipped, not Failed", func() {
		By("Given the run backs up c-legacy whose PVC is on local-path with no VolumeSnapshotClass")
		var legacy cbv1.Backup
		Expect(k8s.Get(ctx, client.ObjectKey{Namespace: "c-legacy", Name: m1CascadeRunName}, &legacy)).To(Succeed())

		By("Then c-legacy's Backup lists that volume with phase Skipped and reason CSISnapshotUnsupported")
		var vol *cbv1.VolumeStatus
		for i := range legacy.Status.Volumes {
			if legacy.Status.Volumes[i].Pvc == "legacy-data" {
				vol = &legacy.Status.Volumes[i]
			}
		}
		Expect(vol).NotTo(BeNil(), "c-legacy's Backup must list the legacy-data volume")
		Expect(string(vol.Phase)).To(Equal("Skipped"))
		Expect(vol.Reason).To(Equal("CSISnapshotUnsupported"))

		By("And c-legacy's Backup is Completed (a Skipped volume is neutral — it never degrades the phase)")
		// c-legacy holds a single local-path PVC (Skipped). Skipped is NEUTRAL in the roll-up
		// (status.RollUpVolumePhases): a PVC on a CSI without a VolumeSnapshotClass is a
		// deterministic property of the environment, not a degradation, so it never pulls the
		// Backup down to PartiallyCompleted — otherwise this namespace would alarm on every run.
		// This holds across milestones: once M3 adds the namespace-manifests snapshot,
		// manifests-done + one skipped volume is still Completed (only a FAILED manifests
		// snapshot would make it partial).
		Expect(legacy.Status.Phase).To(Equal("Completed"))

		By("And one unsupported volume does not fail the whole platform run")
		var run cbv1.ClusterBackup
		Expect(k8s.Get(ctx, client.ObjectKey{Name: m1CascadeRunName}, &run)).To(Succeed())
		Expect(run.Status.Phase).NotTo(Equal("Failed"),
			"a single Skipped volume must not fail the whole platform run")
	})

	It("A namespace with no PVC completes cleanly", func() {
		By("Given c-web and c-empty have no PersistentVolumeClaims")
		for _, ns := range []string{"c-web", "c-empty"} {
			Expect(m1CascadePVCs(ns)).To(BeEmpty(), "%s must have no PersistentVolumeClaims", ns)
		}

		By("Then their Backups reach a terminal, non-failed phase with zero volume failures")
		for _, ns := range []string{"c-web", "c-empty"} {
			var b cbv1.Backup
			Expect(k8s.Get(ctx, client.ObjectKey{Namespace: ns, Name: m1CascadeRunName}, &b)).To(Succeed())
			Expect(b.Status.Phase).To(BeElementOf("Completed", "PartiallyCompleted"),
				"%s Backup must reach a terminal, non-failed phase", ns)
			for _, v := range b.Status.Volumes {
				Expect(string(v.Phase)).NotTo(Equal("Failed"), "%s volume %s must not be Failed", ns, v.Pvc)
			}
		}
	})
})

// ---------------------------------------------------------------------------
// Cascade-local helpers (thin, file-scoped; the shared vocabulary lives in
// m1_helpers_test.go / m1_restic_test.go and is reused, never redefined).
// ---------------------------------------------------------------------------

// m1CascadePVCs lists the PersistentVolumeClaims in a namespace.
func m1CascadePVCs(ns string) []corev1.PersistentVolumeClaim {
	GinkgoHelper()
	var list corev1.PersistentVolumeClaimList
	Expect(k8s.List(ctx, &list, client.InNamespace(ns))).To(Succeed(), "list PVCs in %s", ns)
	return list.Items
}

// m1CascadePVCNames returns the PVC names in a namespace.
func m1CascadePVCNames(ns string) []string {
	GinkgoHelper()
	pvcs := m1CascadePVCs(ns)
	names := make([]string, 0, len(pvcs))
	for _, p := range pvcs {
		names = append(names, p.Name)
	}
	return names
}

// m1CascadeDataSnapshot finds the kind=data snapshot for (namespace, pvc, run) among snaps,
// matching the restic identity the M1 mover writes (host=clusterID, tags namespace=/pvc=/kind=/run=).
func m1CascadeDataSnapshot(snaps []restic.Snapshot, namespace, pvc, run string) (restic.Snapshot, bool) {
	for _, s := range snaps {
		if s.Host != m1ClusterID {
			continue
		}
		kind, _ := restic.TagValue(s.Tags, restic.TagKeyKind)
		ns, _ := restic.TagValue(s.Tags, restic.TagKeyNamespace)
		p, _ := restic.TagValue(s.Tags, restic.TagKeyPVC)
		r, _ := restic.TagValue(s.Tags, restic.TagKeyRun)
		if kind == restic.KindData && ns == namespace && p == pvc && r == run {
			return s, true
		}
	}
	return restic.Snapshot{}, false
}

// m1CascadeNode is the subset of a `restic ls --json` node the cascade reads to prove
// restore-fidelity of exotic data (type/target for symlinks, inode for hardlinks).
type m1CascadeNode struct {
	Type       string `json:"type"`
	Path       string `json:"path"`
	LinkTarget string `json:"linktarget"`
	Inode      uint64 `json:"inode"`
}

// m1CascadeLsNodes runs `restic ls --json <snapshot>` through the crucible's restic oracle and
// decodes the NDJSON node stream (the leading snapshot summary line has no path and is dropped).
//
// restic's `ls --json` node object omits the symlink TARGET — verified against restic 0.17.3 and
// 0.18.0, the versions the oracle runs (m1_restic_test.go), whose node JSON carries type/inode/path
// but no linktarget field — so every symlink's LinkTarget is filled in from a second `restic ls -l`
// (long/human) pass, the only restic-native source for the target. The JSON pass stays authoritative
// for type/inode/path; the -l coupling is scoped to symlink targets alone.
func m1CascadeLsNodes(locationName, snapshotID string) []m1CascadeNode {
	GinkgoHelper()
	out := m1ResticExec(locationName, "ls", "--json", snapshotID)
	var nodes []m1CascadeNode
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var n m1CascadeNode
		if err := json.Unmarshal([]byte(line), &n); err != nil {
			continue
		}
		if n.Path != "" {
			nodes = append(nodes, n)
		}
	}
	targets := m1CascadeSymlinkTargets(locationName, snapshotID)
	for i := range nodes {
		if nodes[i].Type == "symlink" {
			nodes[i].LinkTarget = targets[nodes[i].Path]
		}
	}
	return nodes
}

// m1CascadeSymlinkTargets returns a path→target map for the snapshot's symlinks, parsed from
// `restic ls -l` (the long/human format prints each symlink as `<columns...> <path> -> <target>`).
// restic's machine-readable `ls --json` does not expose the symlink target at all, so the long
// format is the only restic-native way to read it back. Only symlink rows carry ` -> ` and a target
// never contains that token, so a single split per line is unambiguous; the path is the last column
// before the arrow (the crucible's symlink names are space-free).
func m1CascadeSymlinkTargets(locationName, snapshotID string) map[string]string {
	GinkgoHelper()
	out := m1ResticExec(locationName, "ls", "-l", snapshotID)
	targets := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		idx := strings.Index(line, " -> ")
		if idx < 0 {
			continue
		}
		fields := strings.Fields(line[:idx])
		if len(fields) == 0 {
			continue
		}
		targets[fields[len(fields)-1]] = strings.TrimSpace(line[idx+len(" -> "):])
	}
	return targets
}

// m1CascadeNodeByPath returns the node at an exact restic path.
func m1CascadeNodeByPath(nodes []m1CascadeNode, path string) (m1CascadeNode, bool) {
	for _, n := range nodes {
		if n.Path == path {
			return n, true
		}
	}
	return m1CascadeNode{}, false
}
