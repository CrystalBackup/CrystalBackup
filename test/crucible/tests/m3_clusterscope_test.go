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

	rbacv1 "k8s.io/api/rbac/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/restic"
	"github.com/CrystalBackup/CrystalBackup/internal/status"
)

// ---------------------------------------------------------------------------
// M3 acceptance — cluster-scoped capture & selective restore (DoD case 16, R22,
// adr/0011). A ClusterBackup with cluster-scoped capture ON writes ONE
// kind=cluster-manifests snapshot holding the cluster's application-level
// cluster-scoped objects; this spec plants three fixtures a real DR depends on — a
// CustomResourceDefinition, its StorageClass, and a non-system: ClusterRole — and proves
// the full capture→selective-restore contract on the live cluster:
//
//   - capture: status.clusterResourcesCaptured is set and `restic snapshots` shows the
//     kind=cluster-manifests snapshot (the repository is the oracle);
//   - selective restore: a ClusterRestore with clusterResources.include recreates ONLY the
//     named kinds (the CRD + StorageClass), leaving the un-included ClusterRole absent;
//   - opt-in: OMITTING clusterResources restores nothing cluster-scoped (the safe default);
//   - R23: a destructive cluster-scoped restore with a WRONG confirmation is denied;
//   - RBAC: a cluster-scoped restore attempted by a NON-admin identity is denied.
//
// Apply-ordering (cluster-scoped BEFORE volumes/namespaced — adr/0011 §2) is exercised at
// unit/envtest granularity; here the observable DR outcome (the cluster-scoped objects exist
// at completion) is asserted, mirroring how the M2 specs treat the storage-mediation layer.
// ---------------------------------------------------------------------------

const (
	m3CSGroup           = "m3.crystalbackup.io"
	m3CRDName           = "widgets.m3.crystalbackup.io"
	m3SCName            = "m3-fixture-sc"
	m3ClusterRoleName   = "m3-fixture-viewer"
	m3CSSourceNamespace = "m3-cs-src"
)

// m3CSClusterBackupRun is unique per run so a re-run on the shared "dr" repo never collides with a
// prior snapshot (see m3RunID). The source namespace stays fixed; only the run identity varies.
var m3CSClusterBackupRun = "m3-cs-src-" + m3RunID

var _ = Describe("M3 — cluster-scoped capture & selective restore", Label("m3"), Ordered, func() {
	// include globs over the stored <group>/<Kind>[/<name>] path (adr/0011 §2; spec/04 §5.4).
	crdInclude := "apiextensions.k8s.io/CustomResourceDefinition/" + m3CRDName
	scInclude := "storage.k8s.io/StorageClass/" + m3SCName

	crdExists := func() bool {
		crd := &unstructured.Unstructured{}
		crd.SetGroupVersionKind(crdGVK())
		return k8s.Get(ctx, client.ObjectKey{Name: m3CRDName}, crd) == nil
	}
	scExists := func() bool {
		var sc storagev1.StorageClass
		return k8s.Get(ctx, client.ObjectKey{Name: m3SCName}, &sc) == nil
	}
	clusterRoleExists := func() bool {
		var cr rbacv1.ClusterRole
		return k8s.Get(ctx, client.ObjectKey{Name: m3ClusterRoleName}, &cr) == nil
	}

	BeforeAll(func() {
		m3EnsureDRLocation()

		By("planting the cluster-scoped fixtures: a CRD, its StorageClass, and a non-system ClusterRole")
		Expect(client.IgnoreAlreadyExists(k8s.Create(ctx, m3FixtureCRD()))).To(Succeed())
		Expect(client.IgnoreAlreadyExists(k8s.Create(ctx, m3FixtureStorageClass()))).To(Succeed())
		Expect(client.IgnoreAlreadyExists(k8s.Create(ctx, m3FixtureClusterRole()))).To(Succeed())

		By("running a ClusterBackup with cluster-scoped capture enabled")
		// Seed a bound PVC (not just an empty namespace): a ClusterRestore resolves its source
		// by the run's per-namespace DATA snapshots and gates at RunNotFound when the run holds
		// none (clusterrestore_controller.go). The cluster-scoped restore below needs at least
		// one data snapshot to exist even though it restores none of it (volumes: []).
		m2SeedVolume(m3CSSourceNamespace, "data", "ceph-block", "1Gi")
		var existing cbv1.ClusterBackup
		if err := k8s.Get(ctx, client.ObjectKey{Name: m3CSClusterBackupRun}, &existing); err != nil {
			m3RunClusterBackup(m3CSClusterBackupRun, m1LocationName,
				cbv1.NamespaceSelector{MatchNames: []string{m3CSSourceNamespace}}, true)
		}
		cb := m1WaitClusterBackupTerminal(m3CSClusterBackupRun, 20*time.Minute)
		Expect(cb.Status.Phase).To(Equal("Completed"),
			"the capture backup must complete (phase=%q)", cb.Status.Phase)
	})

	AfterAll(func() {
		_ = k8s.Delete(ctx, &cbv1.ClusterRestore{ObjectMeta: metav1.ObjectMeta{Name: "m3-cs-selective"}})
		_ = k8s.Delete(ctx, &cbv1.ClusterRestore{ObjectMeta: metav1.ObjectMeta{Name: "m3-cs-optin"}})
		_ = k8s.Delete(ctx, &cbv1.ClusterBackup{ObjectMeta: metav1.ObjectMeta{Name: m3CSClusterBackupRun}})
		_ = k8s.Delete(ctx, m3FixtureCRD())
		_ = k8s.Delete(ctx, m3FixtureStorageClass())
		_ = k8s.Delete(ctx, m3FixtureClusterRole())
		deleteNamespace(m3CSSourceNamespace)
		m2AssertNoResidualRestoreObjects()
	})

	It("captures a kind=cluster-manifests snapshot; clusterResourcesCaptured is set", func() {
		cb := m1WaitClusterBackupTerminal(m3CSClusterBackupRun, time.Minute)
		Expect(cb.Status.ClusterResourcesCaptured).To(BeNumerically(">", 0),
			"the run must record a cluster-scoped object count (adr/0011 §1)")
		Expect(cb.Status.ClusterManifests).NotTo(BeNil(),
			"the run must record its one kind=cluster-manifests snapshot")

		By("the independent restic oracle shows the kind=cluster-manifests snapshot for this run")
		snap, ok := m3FindSnapshot(m1LocationName, restic.KindClusterManifests, "", m3CSClusterBackupRun)
		Expect(ok).To(BeTrue(), "no kind=cluster-manifests snapshot for run %q", m3CSClusterBackupRun)
		Expect(snap.Paths).To(ContainElement("/cluster-manifests"),
			"the cluster-manifests snapshot lives at the fixed /cluster-manifests path")
	})

	It("selectively restores ONLY the included CRD + StorageClass, leaving the un-included ClusterRole absent", func() {
		By("destroying all three fixtures so a restore has to recreate them")
		Expect(client.IgnoreNotFound(k8s.Delete(ctx, m3FixtureCRD()))).To(Succeed())
		Expect(client.IgnoreNotFound(k8s.Delete(ctx, m3FixtureStorageClass()))).To(Succeed())
		Expect(client.IgnoreNotFound(k8s.Delete(ctx, m3FixtureClusterRole()))).To(Succeed())
		Eventually(func(g Gomega) {
			g.Expect(crdExists()).To(BeFalse())
			g.Expect(scExists()).To(BeFalse())
			g.Expect(clusterRoleExists()).To(BeFalse())
		}, 3*time.Minute, 3*time.Second).Should(Succeed())

		By("restoring with clusterResources.include narrowed to the CRD and the StorageClass only")
		cr := &cbv1.ClusterRestore{
			ObjectMeta: metav1.ObjectMeta{Name: "m3-cs-selective"},
			Spec: cbv1.ClusterRestoreSpec{
				Source: cbv1.ClusterRestoreSource{
					LocationRef: cbv1.LocalObjectReference{Name: m1LocationName},
					Namespace:   m3CSSourceNamespace,
					Backup:      m3CSClusterBackupRun,
				},
				Target:           cbv1.ClusterRestoreTarget{Namespace: m3CSSourceNamespace},
				ClusterResources: &cbv1.ClusterResourceRestoreSpec{Include: []string{crdInclude, scInclude}},
				// resources/volumes PRESENT-but-empty ⇒ restore nothing namespaced; a nil slice
				// would mean "everything" and would drag the seeded PVC + manifests along.
				Resources:    []cbv1.ResourceSelectorItem{},
				Volumes:      []cbv1.VolumeSelectorItem{},
				Mode:         cbv1.RestoreModeOverwrite,
				Confirmation: m3CSSourceNamespace,
			},
		}
		Expect(k8s.Create(ctx, cr)).To(Succeed())
		done := m2WaitClusterRestoreTerminal("m3-cs-selective", 20*time.Minute)
		Expect(done.Status.Phase).To(Equal(string(status.RestorePhaseCompleted)))

		By("the two included kinds are recreated; the un-included ClusterRole stays absent (selection)")
		Eventually(func(g Gomega) {
			g.Expect(crdExists()).To(BeTrue(), "the included CRD must be recreated")
			g.Expect(scExists()).To(BeTrue(), "the included StorageClass must be recreated")
		}, 3*time.Minute, 3*time.Second).Should(Succeed())
		Expect(clusterRoleExists()).To(BeFalse(),
			"the un-included ClusterRole must NOT be restored (clusterResources.include is selective)")
	})

	It("restores NOTHING cluster-scoped when clusterResources is omitted (opt-in default)", func() {
		By("destroying the StorageClass again")
		Expect(client.IgnoreNotFound(k8s.Delete(ctx, m3FixtureStorageClass()))).To(Succeed())
		Eventually(func(g Gomega) { g.Expect(scExists()).To(BeFalse()) }, 2*time.Minute, 3*time.Second).Should(Succeed())

		By("restoring with clusterResources OMITTED entirely")
		cr := &cbv1.ClusterRestore{
			ObjectMeta: metav1.ObjectMeta{Name: "m3-cs-optin"},
			Spec: cbv1.ClusterRestoreSpec{
				Source: cbv1.ClusterRestoreSource{
					LocationRef: cbv1.LocalObjectReference{Name: m1LocationName},
					Namespace:   m3CSSourceNamespace,
					Backup:      m3CSClusterBackupRun,
				},
				Target:       cbv1.ClusterRestoreTarget{Namespace: m3CSSourceNamespace},
				Resources:    []cbv1.ResourceSelectorItem{},
				Volumes:      []cbv1.VolumeSelectorItem{},
				Mode:         cbv1.RestoreModeOverwrite,
				Confirmation: m3CSSourceNamespace,
				// clusterResources omitted (nil pointer) ⇒ nothing cluster-scoped is restored.
				// This restore is a confirmed no-op: it must reach Completed having restored nothing.
			},
		}
		Expect(k8s.Create(ctx, cr)).To(Succeed())
		done := m2WaitClusterRestoreTerminal("m3-cs-optin", 15*time.Minute)
		Expect(done.Status.Phase).To(Equal(string(status.RestorePhaseCompleted)))

		Expect(scExists()).To(BeFalse(),
			"omitting clusterResources must restore nothing cluster-scoped (opt-in; adr/0011 §2)")
	})

	It("denies a destructive cluster-scoped restore with a WRONG confirmation (R23 gate)", func() {
		cr := &cbv1.ClusterRestore{
			ObjectMeta: metav1.ObjectMeta{Name: "m3-cs-wrongconf"},
			Spec: cbv1.ClusterRestoreSpec{
				Source: cbv1.ClusterRestoreSource{
					LocationRef: cbv1.LocalObjectReference{Name: m1LocationName},
					Namespace:   m3CSSourceNamespace,
					Backup:      m3CSClusterBackupRun,
				},
				Target:           cbv1.ClusterRestoreTarget{Namespace: m3CSSourceNamespace},
				ClusterResources: &cbv1.ClusterResourceRestoreSpec{Include: []string{crdInclude}},
				Mode:             cbv1.RestoreModeRecreate,
				Confirmation:     "not-the-target-namespace",
			},
		}
		err := k8s.Create(ctx, cr)
		Expect(err).To(HaveOccurred(), "a confirmation that is not the target namespace must be denied")
		Expect(err.Error()).To(ContainSubstring("confirmation"),
			"the denial must cite the R23 confirmation rule (got: %v)", err)
	})

	It("denies a cluster-scoped restore to a NON-admin identity (RBAC)", func() {
		nonAdmin := m3NonAdminClient()
		cr := &cbv1.ClusterRestore{
			ObjectMeta: metav1.ObjectMeta{Name: "m3-cs-nonadmin"},
			Spec: cbv1.ClusterRestoreSpec{
				Source: cbv1.ClusterRestoreSource{
					LocationRef: cbv1.LocalObjectReference{Name: m1LocationName},
					Namespace:   m3CSSourceNamespace,
					Backup:      m3CSClusterBackupRun,
				},
				Target:           cbv1.ClusterRestoreTarget{Namespace: m3CSSourceNamespace},
				ClusterResources: &cbv1.ClusterResourceRestoreSpec{Include: []string{crdInclude}},
				Mode:             cbv1.RestoreModeOverwrite,
			},
		}
		err := nonAdmin.Create(ctx, cr)
		Expect(apierrors.IsForbidden(err)).To(BeTrue(),
			"a non-admin must be RBAC-denied from creating a ClusterRestore (got: %v)", err)
	})
})

// m3FixtureCRD is a minimal namespaced CRD (widgets.m3.crystalbackup.io) — a fixture
// application-level cluster-scoped object the default capture allow-list picks up (adr/0011).
func m3FixtureCRD() *unstructured.Unstructured {
	crd := &unstructured.Unstructured{}
	crd.SetGroupVersionKind(crdGVK())
	crd.SetName(m3CRDName)
	crd.Object["spec"] = map[string]interface{}{
		"group": m3CSGroup,
		"scope": "Namespaced",
		"names": map[string]interface{}{
			"plural":   "widgets",
			"singular": "widget",
			"kind":     "Widget",
			"listKind": "WidgetList",
		},
		"versions": []interface{}{
			map[string]interface{}{
				"name":    "v1",
				"served":  true,
				"storage": true,
				"schema": map[string]interface{}{
					"openAPIV3Schema": map[string]interface{}{
						"type":                                 "object",
						"x-kubernetes-preserve-unknown-fields": true,
					},
				},
			},
		},
	}
	return crd
}

// m3FixtureStorageClass is a fixture StorageClass with a no-op provisioner — it is captured
// and restored as a manifest; the crucible never provisions a real volume from it.
func m3FixtureStorageClass() *storagev1.StorageClass {
	return &storagev1.StorageClass{
		ObjectMeta:  metav1.ObjectMeta{Name: m3SCName},
		Provisioner: m3CSGroup + "/no-op",
	}
}

// m3FixtureClusterRole is a NON-system: ClusterRole (so the default capture keeps it, unlike
// the system:* names it excludes) used to prove selective restore leaves it out.
func m3FixtureClusterRole() *rbacv1.ClusterRole {
	return &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: m3ClusterRoleName},
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{""},
			Resources: []string{"configmaps"},
			Verbs:     []string{"get", "list", "watch"},
		}},
	}
}
