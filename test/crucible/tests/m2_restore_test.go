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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
	"github.com/CrystalBackup/CrystalBackup/internal/status"
)

// ---------------------------------------------------------------------------
// M2 acceptance — self-service restore in real conditions (R6/R7/R14/R23).
//
// One dedicated namespace, one Ceph RBD volume, everything byte-verified:
// Overwrite must keep files the backup does not know, Recreate must produce an
// exact match, a partial restore must heal ONLY the selected file, and the
// mediated resolution must FAIL CLOSED when a (simulated tampered) projection
// points at a run whose snapshots do not belong to this namespace. Every
// mutate/verify runs as a Job mounting the volume — RWO-safe, no exec.
// ---------------------------------------------------------------------------

var _ = Describe("M2 — self-service restore (modes × selection × mediation)", Label("m2"), Ordered, func() {
	const (
		nsName  = "m2-restore"
		pvcName = "data"
		runName = "m2-restore-src"
	)

	BeforeAll(func() {
		m1SkipIfNoS3()
		m1EnsurePlatformSecrets()
		var loc cbv1.ClusterBackupLocation
		if apierrors.IsNotFound(k8s.Get(ctx, client.ObjectKey{Name: m1LocationName}, &loc)) {
			m1CreateLocation(m1LocationName, true)
		}
		m1WaitRepositoryInitialized(m1LocationName)

		By("seeding a dedicated RBD volume with a checksummed corpus")
		m2SeedVolume(nsName, pvcName, "ceph-block", "1Gi")

		By("backing the namespace up into the shared repository")
		var existing cbv1.ClusterBackup
		if apierrors.IsNotFound(k8s.Get(ctx, client.ObjectKey{Name: runName}, &existing)) {
			m1RunClusterBackup(runName, m1LocationName,
				cbv1.NamespaceSelector{MatchNames: []string{nsName}})
		}
		cb := m1WaitClusterBackupTerminal(runName, 20*time.Minute)
		Expect(cb.Status.Phase).To(Equal("Completed"),
			"the source backup must complete (phase=%q)", cb.Status.Phase)
	})

	AfterAll(func() {
		_ = k8s.Delete(ctx, &cbv1.ClusterBackup{ObjectMeta: metav1.ObjectMeta{Name: runName}})
		deleteNamespace(nsName)
		m1AssertNoResidualSnapshotObjects(nsName)
		m2AssertNoResidualRestoreObjects()
	})

	restore := func(name string, spec cbv1.RestoreSpec) *cbv1.Restore {
		r := &cbv1.Restore{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: nsName},
			Spec:       spec,
		}
		Expect(k8s.Create(ctx, r)).To(Succeed())
		return r
	}

	It("parks without a confirmation, then Overwrite-restores while KEEPING local extras (R23 + Overwrite semantics)", func() {
		By("mutating the live volume: add an extra file and corrupt a seeded one")
		ok, log := m2VolumeJob(nsName, pvcName, "m2-mutate",
			`set -e; cd /data; echo local-only > EXTRA.local; echo corrupted >> alpha/file1.bin; sync`)
		Expect(ok).To(BeTrue(), "mutation must succeed:\n%s", log)

		By("creating the Restore WITHOUT a confirmation (R23 parks it)")
		restore("m2-overwrite", cbv1.RestoreSpec{
			Source: cbv1.RestoreSource{Backup: runName},
			Mode:   cbv1.RestoreModeOverwrite,
		})
		Eventually(func(g Gomega) {
			var r cbv1.Restore
			g.Expect(k8s.Get(ctx, client.ObjectKey{Namespace: nsName, Name: "m2-overwrite"}, &r)).To(Succeed())
			g.Expect(r.Status.Phase).To(Equal(string(status.RestorePhaseAwaitingConfirmation)))
		}, 3*time.Minute, 3*time.Second).Should(Succeed())

		By("confirming with the namespace name and waiting for completion")
		Eventually(func(g Gomega) {
			var r cbv1.Restore
			g.Expect(k8s.Get(ctx, client.ObjectKey{Namespace: nsName, Name: "m2-overwrite"}, &r)).To(Succeed())
			r.Spec.Confirmation = nsName
			g.Expect(k8s.Update(ctx, &r)).To(Succeed())
		}, time.Minute, 2*time.Second).Should(Succeed())
		done := m2WaitRestoreTerminal(nsName, "m2-overwrite", 20*time.Minute)
		Expect(done.Status.Phase).To(Equal(string(status.RestorePhaseCompleted)))
		Expect(done.Status.RestoredVolumes).To(Equal(int32(1)))

		By("byte-verifying: seeded content healed, the local extra KEPT")
		ok, log = m2VolumeJob(nsName, pvcName, "m2-verify-overwrite",
			`set -e; cd /data; sha256sum -c MANIFEST.sha256; test -f EXTRA.local`)
		Expect(ok).To(BeTrue(), "Overwrite must heal seeded files and keep extras:\n%s", log)
	})

	It("partial-restores ONLY the selected file (R7)", func() {
		By("corrupting two known files")
		ok, log := m2VolumeJob(nsName, pvcName, "m2-corrupt",
			`set -e; cd /data; echo drift >> MANIFEST.sha256; echo drift >> beta/blob.bin; sync`)
		Expect(ok).To(BeTrue(), "corruption step must succeed:\n%s", log)

		By("restoring ONLY the manifest file")
		restore("m2-partial", cbv1.RestoreSpec{
			Source:       cbv1.RestoreSource{Backup: runName},
			Mode:         cbv1.RestoreModeOverwrite,
			Confirmation: nsName,
			Volumes: []cbv1.VolumeSelectorItem{{
				Names:   []string{pvcName},
				Include: []string{"MANIFEST.sha256"},
			}},
		})
		done := m2WaitRestoreTerminal(nsName, "m2-partial", 20*time.Minute)
		Expect(done.Status.Phase).To(Equal(string(status.RestorePhaseCompleted)))

		By("byte-verifying: the manifest healed, the unselected file still corrupted")
		ok, log = m2VolumeJob(nsName, pvcName, "m2-verify-partial",
			`set -e; cd /data
cmp MANIFEST.sha256 MANIFEST.copy
if sha256sum -c MANIFEST.sha256 2>/dev/null; then echo "blob.bin should still be corrupted"; exit 1; fi
exit 0`)
		Expect(ok).To(BeTrue(), "partial restore must heal only the selected file:\n%s", log)
	})

	It("Recreate-restores to an EXACT match (extras removed)", func() {
		restore("m2-recreate", cbv1.RestoreSpec{
			Source:       cbv1.RestoreSource{Backup: runName},
			Mode:         cbv1.RestoreModeRecreate,
			Confirmation: nsName,
		})
		done := m2WaitRestoreTerminal(nsName, "m2-recreate", 20*time.Minute)
		Expect(done.Status.Phase).To(Equal(string(status.RestorePhaseCompleted)))

		By("byte-verifying: every seeded file matches and the extras are GONE")
		ok, log := m2VolumeJob(nsName, pvcName, "m2-verify-recreate",
			`set -e; cd /data; sha256sum -c MANIFEST.sha256; test ! -e EXTRA.local`)
		Expect(ok).To(BeTrue(), "Recreate must reconcile the volume to an exact match:\n%s", log)
	})

	It("fails CLOSED when a tampered projection points at another namespace's run (R14 storage negative)", func() {
		// Simulate the worst-case projection tamper: a cluster-origin Backup in THIS namespace
		// claiming the c-db run and its snapshot IDs. The M2 user-isolation VAP already forbids
		// ordinary identities from writing cluster-origin Backups (the first line of defense),
		// so the tamper is authored AS THE OPERATOR — modelling a buggy/compromised projection
		// that slipped past admission. This spec proves the LAST line of defense (I1, the
		// storage mediation): the resolution lists the repository under namespace=m2-restore +
		// run=crucible-restore — which holds NOTHING — so the restore gates on SnapshotNotFound
		// and never borrows the foreign snapshot.
		foreignRun := "crucible-restore" // the m1 off-cluster spec's run of c-db
		tampered := &cbv1.Backup{
			ObjectMeta: metav1.ObjectMeta{
				Name:      foreignRun,
				Namespace: nsName,
				Labels: map[string]string{
					apiconst.LabelClusterBackup: foreignRun,
					apiconst.LabelOrigin:        apiconst.OriginCluster,
				},
				Annotations: map[string]string{apiconst.AnnotationProjected: apiconst.AnnotationProjectedValue},
			},
			Spec: cbv1.BackupSpec{
				LocationRef: cbv1.LocationReference{Kind: "ClusterBackupLocation", Name: m1LocationName},
			},
		}
		Expect(k8sAsOperator.Create(ctx, tampered)).To(Succeed())
		DeferCleanup(func() { _ = k8s.Delete(ctx, tampered) })
		tampered.Status.Phase = "Completed"
		now := metav1.Now()
		tampered.Status.BackupTime = &now
		tampered.Status.Volumes = []cbv1.VolumeStatus{{
			Pvc: "data", Phase: "Completed", SnapshotID: "0000000000000000000000000000000000000000000000000000000000000000",
		}}
		Expect(k8sAsOperator.Status().Update(ctx, tampered)).To(Succeed())

		restore("m2-negative", cbv1.RestoreSpec{
			Source:       cbv1.RestoreSource{Backup: foreignRun},
			Mode:         cbv1.RestoreModeOverwrite,
			Confirmation: nsName,
		})
		DeferCleanup(func() {
			_ = k8s.Delete(ctx, &cbv1.Restore{ObjectMeta: metav1.ObjectMeta{Name: "m2-negative", Namespace: nsName}})
		})

		Eventually(func(g Gomega) {
			var r cbv1.Restore
			g.Expect(k8s.Get(ctx, client.ObjectKey{Namespace: nsName, Name: "m2-negative"}, &r)).To(Succeed())
			cond := status.FindCondition(r.Status.Conditions, "Ready")
			g.Expect(cond).NotTo(BeNil())
			// Fail CLOSED — the restore never borrows the foreign snapshot. TWO defense layers
			// can catch the tamper and the faster one wins: discovery reconciles projected
			// Backups against repository state and PRUNES this fake (its run has no snapshots in
			// THIS namespace) ⇒ SourceBackupNotFound; if the fake outlived discovery, the
			// mediated resolution lists namespace=<ns> + run=<foreign> and finds nothing ⇒
			// SnapshotNotFound. The storage-mediation layer is isolated in envtest
			// (TestMediatedFilterNamespaceIndependence); here we assert the end-to-end result.
			g.Expect(cond.Reason).To(BeElementOf("SourceBackupNotFound", "SnapshotNotFound"),
				"the tamper must be caught fail-closed (phase=%q, reason=%q)", r.Status.Phase, cond.Reason)
			g.Expect(r.Status.Phase).To(Equal(string(status.RestorePhasePending)),
				"a fail-closed restore stays Pending — it must never restore foreign data")
		}, 3*time.Minute, 5*time.Second).Should(Succeed())
	})
})
