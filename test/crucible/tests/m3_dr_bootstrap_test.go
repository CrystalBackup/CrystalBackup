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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/status"
)

// ---------------------------------------------------------------------------
// M3 acceptance — DR bootstrap into a fresh namespace (DoD case 9, R15/R26).
//
// A ClusterRestore is the disaster-recovery entry point: given only a repository
// coordinate (location + namespace + run) it lands a namespace's volume data into a
// BRAND-NEW namespace on a cluster whose storage classes may differ from the source's.
// This spec proves the two DR-specific target knobs on real Ceph→Longhorn storage:
// target.createNamespace materializes the destination, and target.storageClassMapping
// rewrites the PVC's class so the restored volume provisions on a DIFFERENT class than it
// was captured from. A verifying consumer then reaches Running on that volume — a workload
// running on the DR-restored data.
//
// Scope note (adr/0011 §2): a ClusterRestore today restores cluster-scoped objects and
// volume data; restoring the source namespace's OWN manifests through a ClusterRestore is a
// documented follow-up. This spec is therefore scoped to the createNamespace +
// storageClassMapping + volume-data path — the manifest round-trip lives in
// m3_manifest_test.go on the namespaced Restore.
// ---------------------------------------------------------------------------

var _ = Describe("M3 — DR bootstrap (ClusterRestore into a fresh namespace)", Label("m3"), Ordered, func() {
	const (
		srcNS       = "m3-dr-src"
		rebornNS    = "m3-dr-reborn"
		pvcName     = "data"
		sourceClass = "ceph-block"
		targetClass = "longhorn"
	)
	// Unique per run so a re-run on the shared "dr" repo never collides with a prior snapshot (see m3RunID).
	runName := "m3-dr-src-" + m3RunID

	BeforeAll(func() {
		m3EnsureDRLocation()

		By("seeding a source namespace on the default (ceph-block) class with a checksummed corpus")
		m2SeedVolume(srcNS, pvcName, sourceClass, "1Gi")

		By("backing it up into the shared DR repository")
		var existing cbv1.ClusterBackup
		if err := k8s.Get(ctx, client.ObjectKey{Name: runName}, &existing); err != nil {
			m1RunClusterBackup(runName, m1LocationName,
				cbv1.NamespaceSelector{MatchNames: []string{srcNS}})
		}
		cb := m1WaitClusterBackupTerminal(runName, 20*time.Minute)
		Expect(cb.Status.Phase).To(Equal("Completed"),
			"the source backup must complete (phase=%q)", cb.Status.Phase)
	})

	AfterAll(func() {
		_ = k8s.Delete(ctx, &cbv1.ClusterRestore{ObjectMeta: metav1.ObjectMeta{Name: "m3-dr-bootstrap"}})
		_ = k8s.Delete(ctx, &cbv1.ClusterBackup{ObjectMeta: metav1.ObjectMeta{Name: runName}})
		deleteNamespace(rebornNS)
		deleteNamespace(srcNS)
		m1AssertNoResidualSnapshotObjects(srcNS, rebornNS)
		m2AssertNoResidualRestoreObjects()
	})

	It("bootstraps DR into a created namespace, remaps the storage class, and a workload runs on the restored volume", func() {
		By("creating the ClusterRestore against the repo coordinate, createNamespace + storageClassMapping")
		cr := &cbv1.ClusterRestore{
			ObjectMeta: metav1.ObjectMeta{Name: "m3-dr-bootstrap"},
			Spec: cbv1.ClusterRestoreSpec{
				Source: cbv1.ClusterRestoreSource{
					LocationRef: cbv1.LocalObjectReference{Name: m1LocationName},
					Namespace:   srcNS,
					Backup:      runName,
				},
				Target: cbv1.ClusterRestoreTarget{
					Namespace:           rebornNS,
					CreateNamespace:     true,
					StorageClassMapping: map[string]string{sourceClass: targetClass},
				},
				Mode:         cbv1.RestoreModeRecreate,
				Confirmation: rebornNS,
			},
		}
		Expect(k8s.Create(ctx, cr)).To(Succeed())

		done := m2WaitClusterRestoreTerminal("m3-dr-bootstrap", 30*time.Minute)
		Expect(done.Status.Phase).To(Equal(string(status.RestorePhaseCompleted)),
			"DR bootstrap must complete (restored=%d)", done.Status.RestoredVolumes)
		Expect(done.Status.RestoredVolumes).To(Equal(int32(1)))
		Expect(done.Status.RestoredBytes).To(BeNumerically(">", 0))

		By("the reborn namespace exists and its PVC landed on the MAPPED class, Bound")
		var ns corev1.Namespace
		Expect(k8s.Get(ctx, client.ObjectKey{Name: rebornNS}, &ns)).To(Succeed(),
			"target.createNamespace must have materialized %s", rebornNS)

		var pvc corev1.PersistentVolumeClaim
		Expect(k8s.Get(ctx, client.ObjectKey{Namespace: rebornNS, Name: pvcName}, &pvc)).To(Succeed())
		Expect(pvc.Spec.StorageClassName).NotTo(BeNil())
		Expect(*pvc.Spec.StorageClassName).To(Equal(targetClass),
			"storageClassMapping must rewrite %s → %s on the restored PVC", sourceClass, targetClass)
		Expect(pvc.Status.Phase).To(Equal(corev1.ClaimBound), "the remapped claim must be Bound")

		By("a workload reaches Running on the DR-restored volume (data verified against the seed manifest)")
		m3RunVerifyingConsumer(rebornNS, "dr-consumer", pvcName)
	})
})
