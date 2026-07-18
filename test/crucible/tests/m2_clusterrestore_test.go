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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
	"github.com/CrystalBackup/CrystalBackup/internal/status"
)

// ---------------------------------------------------------------------------
// M2 acceptance — ClusterRestore reconstitutes a DELETED namespace (the M2 exit
// criterion, R14/R26): back a dedicated namespace up, destroy it entirely, then
// restore from the repo coordinate alone into a NEW namespace. The recreated
// PVC must carry the ORIGINAL capacity and class (from the pvcsize/pvcclass
// snapshot tags — no CR survived to remember them), the provenance annotation,
// and byte-identical data. The transplant handover is exercised end to end on
// real Ceph RBD: dynamic provisioning in the operator namespace, PV re-bind
// into the target namespace, reclaim policy handed back.
// ---------------------------------------------------------------------------

var _ = Describe("M2 — ClusterRestore reconstitutes a deleted namespace", Label("m2"), Ordered, func() {
	const (
		doomedNS = "m2-doomed"
		rebornNS = "m2-reborn"
		pvcName  = "data"
		runName  = "m2-doomed-src"
	)

	BeforeAll(func() {
		m1SkipIfNoS3()
		m1EnsurePlatformSecrets()
		var loc cbv1.ClusterBackupLocation
		if apierrors.IsNotFound(k8s.Get(ctx, client.ObjectKey{Name: m1LocationName}, &loc)) {
			m1CreateLocation(m1LocationName, true)
		}
		m1WaitRepositoryInitialized(m1LocationName)

		By("seeding the doomed namespace on a 2Gi RBD volume")
		m2SeedVolume(doomedNS, pvcName, "ceph-block", "2Gi")

		By("backing it up into the shared repository")
		var existing cbv1.ClusterBackup
		if apierrors.IsNotFound(k8s.Get(ctx, client.ObjectKey{Name: runName}, &existing)) {
			m1RunClusterBackup(runName, m1LocationName,
				cbv1.NamespaceSelector{MatchNames: []string{doomedNS}})
		}
		cb := m1WaitClusterBackupTerminal(runName, 20*time.Minute)
		Expect(cb.Status.Phase).To(Equal("Completed"),
			"the source backup must complete (phase=%q)", cb.Status.Phase)

		By("DELETING the namespace — data now exists only in the repository")
		deleteNamespace(doomedNS)
		Eventually(func(g Gomega) {
			err := k8s.Get(ctx, client.ObjectKey{Name: doomedNS}, &corev1.Namespace{})
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "namespace %s must be fully gone", doomedNS)
		}, 10*time.Minute, 5*time.Second).Should(Succeed())
	})

	AfterAll(func() {
		_ = k8s.Delete(ctx, &cbv1.ClusterRestore{ObjectMeta: metav1.ObjectMeta{Name: "m2-reconstitute"}})
		_ = k8s.Delete(ctx, &cbv1.ClusterBackup{ObjectMeta: metav1.ObjectMeta{Name: runName}})
		deleteNamespace(rebornNS)
		m2AssertNoResidualRestoreObjects()
	})

	It("restores the repo coordinate into a freshly-created namespace, PVC sized from the snapshot tags, data byte-identical", func() {
		By("creating the ClusterRestore against (location, namespace, run) — no in-cluster source object exists")
		cr := &cbv1.ClusterRestore{
			ObjectMeta: metav1.ObjectMeta{Name: "m2-reconstitute"},
			Spec: cbv1.ClusterRestoreSpec{
				Source: cbv1.ClusterRestoreSource{
					LocationRef: cbv1.LocalObjectReference{Name: m1LocationName},
					Namespace:   doomedNS,
					Backup:      runName,
				},
				Target: cbv1.ClusterRestoreTarget{
					Namespace:       rebornNS,
					CreateNamespace: true,
				},
				Mode:         cbv1.RestoreModeRecreate,
				Confirmation: rebornNS,
			},
		}
		Expect(k8s.Create(ctx, cr)).To(Succeed())

		done := m2WaitClusterRestoreTerminal("m2-reconstitute", 30*time.Minute)
		Expect(done.Status.Phase).To(Equal(string(status.RestorePhaseCompleted)),
			"reconstitution must complete (restored=%d)", done.Status.RestoredVolumes)
		Expect(done.Status.RestoredVolumes).To(Equal(int32(1)))
		Expect(done.Status.RestoredBytes).To(BeNumerically(">", 0))

		By("the recreated PVC carries the ORIGINAL shape (pvcsize/pvcclass tags) and provenance")
		var pvc corev1.PersistentVolumeClaim
		Expect(k8s.Get(ctx, client.ObjectKey{Namespace: rebornNS, Name: pvcName}, &pvc)).To(Succeed())
		capacity := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
		Expect(capacity.Cmp(resource.MustParse("2Gi"))).To(BeZero(),
			"capacity must come from the pvcsize tag, got %s", capacity.String())
		Expect(pvc.Spec.StorageClassName).NotTo(BeNil())
		Expect(*pvc.Spec.StorageClassName).To(Equal("ceph-block"), "class must come from the pvcclass tag")
		Expect(pvc.Annotations[apiconst.AnnotationRestoredFrom]).To(Equal(runName))
		Expect(pvc.Labels[apiconst.LabelManagedBy]).To(BeEmpty(),
			"a restored PVC is the user's object — never operator-labeled")
		Expect(pvc.Status.Phase).To(Equal(corev1.ClaimBound), "the transplanted claim must be Bound")

		By("its PersistentVolume was handed over: unlabeled, class reclaim policy restored")
		var pv corev1.PersistentVolume
		Expect(k8s.Get(ctx, client.ObjectKey{Name: pvc.Spec.VolumeName}, &pv)).To(Succeed())
		Expect(pv.Labels).NotTo(HaveKey(apiconst.LabelPVRole))
		Expect(pv.Spec.PersistentVolumeReclaimPolicy).To(Equal(corev1.PersistentVolumeReclaimDelete))

		By("byte-verifying the reconstituted data against the seed manifest")
		ok, log := m2VolumeJob(rebornNS, pvcName, "m2-verify-reborn",
			`set -e; cd /data; sha256sum -c MANIFEST.sha256`)
		Expect(ok).To(BeTrue(), "every reconstituted byte must match the seed:\n%s", log)
	})
})
