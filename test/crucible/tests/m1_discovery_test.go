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

// m1_discovery_test.go implements test/crucible/features/m1_discovery.feature 1:1
// (one It per Scenario). Discovery is what makes a shared DR repository restorable
// with NO prior CRs: adding a ClusterBackupLocation must be enough to see every
// (namespace, run) restore point projected as a read-only, cluster-origin Backup.
//
// The three scenarios share one expensive fixture — a completed manual ClusterBackup
// run across c-db, c-media and a throwaway ghost namespace — built once in BeforeAll,
// so the container is Ordered. The independent restic oracle (m1ResticSnapshots /
// m1ResticExec) reads the object store as ground truth, so a controller that both
// writes and reports the same wrong thing cannot make these pass.

// m1DiscoveryGhostNS is a namespace created, backed up, then DELETED before the
// assertions run: its (namespace, run) group persists in the repository while the
// namespace is gone, which is the "run whose namespace does not exist" fixture.
const m1DiscoveryGhostNS = "c-ghost"

var _ = Describe("M1 — discovery projects restorable backups", Label("m1"), Ordered, func() {
	// Shared across the Ordered scenarios; assigned in BeforeAll.
	var (
		m1DiscoveryRun  string
		createdLocation bool
	)

	BeforeAll(func() {
		m1SkipIfNoS3()
		m1EnsurePlatformSecrets()

		// An initialized ClusterBackupLocation "dr" for the shared repository —
		// reused if a sibling feature already created it, so we only own (and later
		// tear down) the location when this suite created it.
		var loc cbv1.ClusterBackupLocation
		err := k8s.Get(ctx, client.ObjectKey{Name: m1LocationName}, &loc)
		switch {
		case apierrors.IsNotFound(err):
			m1CreateLocation(m1LocationName, false)
			createdLocation = true
		default:
			Expect(err).NotTo(HaveOccurred(), "get ClusterBackupLocation %s", m1LocationName)
		}
		m1WaitRepositoryInitialized(m1LocationName)

		// A namespace with a SNAPSHOTTABLE PVC (ceph-block), backed up then deleted so its data
		// snapshot (tagged namespace=c-ghost) is the repo group that must outlive the namespace.
		// In M1 the only durable per-namespace artifact is a DATA snapshot — the namespace-manifests
		// snapshot is M3 — so a PVC-less ghost would leave nothing in the repository and this fixture
		// would be vacuous. startPVCConsumer writes /data/probe.txt and waits Running (PVC bound with
		// content) before the run below snapshots the volume.
		ensureNamespace(m1DiscoveryGhostNS)
		startPVCConsumer(m1DiscoveryGhostNS, "ghost-data", "ceph-block")

		// A unique run name keeps each execution hermetic (its snapshots are distinct),
		// while discovery's set semantics still hold across any accumulated runs.
		m1DiscoveryRun = "m1-discovery-" + time.Now().UTC().Format(apiconst.RunTimestampLayout)

		m1RunClusterBackup(m1DiscoveryRun, m1LocationName, cbv1.NamespaceSelector{
			MatchNames: []string{"c-db", "c-media", m1DiscoveryGhostNS},
		})
		m1WaitClusterBackupTerminal(m1DiscoveryRun, 20*time.Minute)

		// The run is captured — drop the ghost namespace so its restore point is now
		// only reachable through the repository (and an admin ClusterRestore).
		deleteNamespace(m1DiscoveryGhostNS)
	})

	AfterAll(func() {
		if m1DiscoveryRun != "" {
			_ = k8s.Delete(ctx, &cbv1.ClusterBackup{ObjectMeta: metav1.ObjectMeta{Name: m1DiscoveryRun}})
			for _, ns := range []string{"c-db", "c-media"} {
				_ = k8s.Delete(ctx, &cbv1.Backup{ObjectMeta: metav1.ObjectMeta{Name: m1DiscoveryRun, Namespace: ns}})
			}
		}
		deleteNamespace(m1DiscoveryGhostNS)
		if createdLocation {
			m1DeleteLocation(m1LocationName)
		}
	})

	It("Discovery projects a Backup per (namespace, run) into existing namespaces", func() {
		By(`Given an initialized ClusterBackupLocation "dr" whose repository already holds snapshots for c-db and c-media`)
		snaps := m1ResticSnapshots(m1LocationName)
		Expect(m1DiscoverySnapshotsInNamespace(snaps, "c-db")).NotTo(BeEmpty(),
			"repository should already hold snapshots for c-db")
		Expect(m1DiscoverySnapshotsInNamespace(snaps, "c-media")).NotTo(BeEmpty(),
			"repository should already hold snapshots for c-media")

		// c-db's data snapshots carry the pinned restic identity (host=clusterID,
		// path /data/c-db/<pvc>, namespace=c-db) — the ground truth discovery projects.
		dataCDB := m1DiscoveryDataSnapshots(snaps, "c-db", m1DiscoveryRun)
		Expect(dataCDB).NotTo(BeEmpty(), "expected data snapshots for c-db/%s", m1DiscoveryRun)
		for _, s := range dataCDB {
			Expect(s.Host).To(Equal(m1ClusterID), "data snapshot host must be the clusterID")
			pvc, ok := restic.TagValue(s.Tags, restic.TagKeyPVC)
			Expect(ok).To(BeTrue(), "data snapshot must carry a pvc= tag")
			Expect(s.Paths).To(ContainElement("/data/c-db/"+pvc), "data snapshot path")
			nsTag, _ := restic.TagValue(s.Tags, restic.TagKeyNamespace)
			Expect(nsTag).To(Equal("c-db"), "data snapshot namespace= tag")
		}

		By("When the discovery controller inventories the repository")
		// The discovery controller reconciles continuously; the assertions below
		// Eventually observe its projection converge.

		By("Then a Backup named after each run appears in c-db and in c-media")
		Eventually(func(g Gomega) {
			g.Expect(m1DiscoveryBackupNames(m1ListBackups("c-db"), apiconst.OriginCluster)).
				To(HaveKey(m1DiscoveryRun), "no projected Backup %q in c-db yet", m1DiscoveryRun)
			g.Expect(m1DiscoveryBackupNames(m1ListBackups("c-media"), apiconst.OriginCluster)).
				To(HaveKey(m1DiscoveryRun), "no projected Backup %q in c-media yet", m1DiscoveryRun)
		}, 10*time.Minute, 15*time.Second).Should(Succeed())

		By("And each projected Backup has origin=cluster and status.volumes derived from the repository snapshots")
		var cdb cbv1.Backup
		Expect(k8s.Get(ctx, client.ObjectKey{Namespace: "c-db", Name: m1DiscoveryRun}, &cdb)).To(Succeed())
		Expect(cdb.Labels[apiconst.LabelOrigin]).To(Equal(apiconst.OriginCluster),
			"projected Backup must be labelled origin=cluster (read-only to users)")
		Expect(cdb.Labels[apiconst.LabelClusterBackup]).To(Equal(m1DiscoveryRun),
			"projected Backup must carry the cluster-backup=<run> link label")
		Expect(cdb.Labels[apiconst.LabelNamespace]).To(Equal("c-db"),
			"projected Backup must carry the namespace= label")
		for _, or := range cdb.OwnerReferences {
			Expect(or.Kind).NotTo(Equal("ClusterBackup"),
				"the cross-namespace link is by label only — never an ownerReference to the ClusterBackup")
		}

		// status.volumes is derived from the repository: one Completed volume per
		// c-db data snapshot, with the snapshot's own restic ID.
		volByPVC := map[string]cbv1.VolumeStatus{}
		for _, v := range cdb.Status.Volumes {
			volByPVC[v.Pvc] = v
		}
		for _, s := range dataCDB {
			pvc, _ := restic.TagValue(s.Tags, restic.TagKeyPVC)
			v, ok := volByPVC[pvc]
			Expect(ok).To(BeTrue(), "projected Backup missing status.volumes entry for pvc %s", pvc)
			Expect(v.SnapshotID).To(Equal(s.ID), "volume %s snapshotID must match its repository snapshot", pvc)
			Expect(string(v.Phase)).To(Equal("Completed"), "derived volume %s phase", pvc)
		}

		By("And a run whose namespace does not exist on the cluster is NOT projected (it stays available only to ClusterRestore)")
		Eventually(func() bool {
			var ns corev1.Namespace
			return apierrors.IsNotFound(k8s.Get(ctx, client.ObjectKey{Name: m1DiscoveryGhostNS}, &ns))
		}, 2*time.Minute, 5*time.Second).Should(BeTrue(), "ghost namespace %s must be gone", m1DiscoveryGhostNS)

		Expect(m1DiscoverySnapshotsInNamespace(snaps, m1DiscoveryGhostNS)).NotTo(BeEmpty(),
			"the deleted namespace's snapshots must remain in the repository (reachable only by ClusterRestore)")

		Consistently(func(g Gomega) {
			g.Expect(m1ListBackups(m1DiscoveryGhostNS)).To(BeEmpty(),
				"discovery must not fabricate a Backup for a namespace that does not exist")
		}, 30*time.Second, 10*time.Second).Should(Succeed())
	})

	It("A projected Backup is a view, not the source of truth", func() {
		By(`Given an initialized ClusterBackupLocation "dr" whose repository already holds snapshots for c-db and c-media`)

		By(`Given a projected Backup "R" in c-db`)
		var bk cbv1.Backup
		Eventually(func() error {
			return k8s.Get(ctx, client.ObjectKey{Namespace: "c-db", Name: m1DiscoveryRun}, &bk)
		}, 10*time.Minute, 15*time.Second).Should(Succeed(), "projected Backup %q should exist in c-db", m1DiscoveryRun)

		// The repository's c-db/R snapshot IDs as ground truth before we touch the CR.
		before := m1DiscoverySnapshotIDs(m1ResticSnapshots(m1LocationName), "c-db", m1DiscoveryRun)
		Expect(before).NotTo(BeEmpty(), "expected repository snapshots for c-db/%s", m1DiscoveryRun)

		By(`When I delete the Backup CR "R"`)
		Expect(k8s.Delete(ctx, &bk)).To(Succeed(), "delete projected Backup c-db/%s", m1DiscoveryRun)

		By("Then the repository snapshots for c-db/R are unchanged (deleting a Backup never runs restic forget)")
		after := m1DiscoverySnapshotIDs(m1ResticSnapshots(m1LocationName), "c-db", m1DiscoveryRun)
		Expect(after).To(Equal(before), "deleting a Backup CR must not forget any repository snapshot")

		By(`And discovery re-creates the projected Backup "R" on its next pass`)
		Eventually(func(g Gomega) {
			var re cbv1.Backup
			g.Expect(k8s.Get(ctx, client.ObjectKey{Namespace: "c-db", Name: m1DiscoveryRun}, &re)).To(Succeed())
			g.Expect(re.Labels[apiconst.LabelOrigin]).To(Equal(apiconst.OriginCluster))
			g.Expect(re.UID).NotTo(Equal(bk.UID), "a re-created projection is a fresh object, not the deleted one")
		}, 10*time.Minute, 15*time.Second).Should(Succeed())
	})

	It("kubectl get backups lists exactly the restorable set", func() {
		By(`Given an initialized ClusterBackupLocation "dr" whose repository already holds snapshots for c-db and c-media`)

		By("Given the repository holds exactly the run set {R} for c-db")
		snaps := m1ResticSnapshots(m1LocationName)
		repoRuns := m1DiscoveryRunsInNamespace(snaps, "c-db")
		Expect(repoRuns).To(HaveKey(m1DiscoveryRun), "the repository must hold run %q for c-db", m1DiscoveryRun)

		By(`Then "kubectl get backups -n c-db" lists exactly {R} — no fabricated and no missing entries`)
		// The projected (cluster-origin) Backups in c-db are a bijection with the
		// repository's run set for c-db: no fabricated Backup (one without a run in the
		// repo) and no missing run (one in the repo without a Backup).
		Eventually(func(g Gomega) {
			g.Expect(m1DiscoveryBackupNames(m1ListBackups("c-db"), apiconst.OriginCluster)).
				To(Equal(repoRuns), "projected Backups in c-db must match the repository run set exactly")
		}, 10*time.Minute, 15*time.Second).Should(Succeed())

		By("And after the snapshots for a run are removed from the repository, discovery removes that projected Backup")
		// Demonstrated on c-media so c-db's asserted set is left intact: forget the
		// run's c-media snapshots via the independent restic oracle, then observe
		// discovery drop the now-unbacked projection.
		var mediaIDs []string
		for _, s := range m1DiscoverySnapshotsInNamespace(snaps, "c-media") {
			if r, _ := restic.TagValue(s.Tags, restic.TagKeyRun); r == m1DiscoveryRun {
				mediaIDs = append(mediaIDs, s.ID)
			}
		}
		Expect(mediaIDs).NotTo(BeEmpty(), "expected repository snapshots for c-media/%s to remove", m1DiscoveryRun)

		Eventually(func(g Gomega) {
			g.Expect(m1DiscoveryBackupNames(m1ListBackups("c-media"), apiconst.OriginCluster)).
				To(HaveKey(m1DiscoveryRun), "the c-media projection must exist before we remove its snapshots")
		}, 10*time.Minute, 15*time.Second).Should(Succeed())

		m1ResticExec(m1LocationName, append([]string{"forget"}, mediaIDs...)...)

		Eventually(func(g Gomega) {
			g.Expect(m1DiscoveryBackupNames(m1ListBackups("c-media"), apiconst.OriginCluster)).
				NotTo(HaveKey(m1DiscoveryRun), "discovery must remove the projected Backup once its snapshots are gone")
		}, 10*time.Minute, 15*time.Second).Should(Succeed())
	})
})

// ---------------------------------------------------------------------------
// File-local read helpers over the independent restic oracle's Snapshots and
// over a namespace's Backups. They only read (never redefine) the shared m1
// helpers and the pinned restic tag contract.
// ---------------------------------------------------------------------------

// m1DiscoverySnapshotsInNamespace returns the snapshots whose namespace= tag == ns.
func m1DiscoverySnapshotsInNamespace(snaps []restic.Snapshot, ns string) []restic.Snapshot {
	var out []restic.Snapshot
	for _, s := range snaps {
		if v, _ := restic.TagValue(s.Tags, restic.TagKeyNamespace); v == ns {
			out = append(out, s)
		}
	}
	return out
}

// m1DiscoveryDataSnapshots returns the kind=data snapshots for (ns, run).
func m1DiscoveryDataSnapshots(snaps []restic.Snapshot, ns, run string) []restic.Snapshot {
	var out []restic.Snapshot
	for _, s := range m1DiscoverySnapshotsInNamespace(snaps, ns) {
		r, _ := restic.TagValue(s.Tags, restic.TagKeyRun)
		k, _ := restic.TagValue(s.Tags, restic.TagKeyKind)
		if r == run && k == restic.KindData {
			out = append(out, s)
		}
	}
	return out
}

// m1DiscoveryRunsInNamespace returns the set of distinct run= tags among ns's snapshots.
func m1DiscoveryRunsInNamespace(snaps []restic.Snapshot, ns string) map[string]bool {
	runs := map[string]bool{}
	for _, s := range m1DiscoverySnapshotsInNamespace(snaps, ns) {
		if r, ok := restic.TagValue(s.Tags, restic.TagKeyRun); ok {
			runs[r] = true
		}
	}
	return runs
}

// m1DiscoverySnapshotIDs returns the set of restic snapshot IDs for (ns, run).
func m1DiscoverySnapshotIDs(snaps []restic.Snapshot, ns, run string) map[string]bool {
	ids := map[string]bool{}
	for _, s := range m1DiscoverySnapshotsInNamespace(snaps, ns) {
		if r, _ := restic.TagValue(s.Tags, restic.TagKeyRun); r == run {
			ids[s.ID] = true
		}
	}
	return ids
}

// m1DiscoveryBackupNames returns the set of Backup names carrying the given origin
// label value (pass "" for all origins).
func m1DiscoveryBackupNames(backups []cbv1.Backup, origin string) map[string]bool {
	names := map[string]bool{}
	for i := range backups {
		if origin == "" || backups[i].Labels[apiconst.LabelOrigin] == origin {
			names[backups[i].Name] = true
		}
	}
	return names
}
