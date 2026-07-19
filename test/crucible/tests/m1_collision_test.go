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
	"fmt"
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
)

// ---------------------------------------------------------------------------
// M1 regression — cluster-DR mover object-name collision (fixed in 0.2.1).
//
// A ClusterBackup run fans out one child Backup of the SAME name into every
// matched namespace. Every per-PVC mover/exposure object (mover Job, creds
// Secret, temp clone PVC, static VolumeSnapshot — and the CLUSTER-SCOPED static
// VolumeSnapshotContent) lives in the single shared operator namespace, named
// deterministically from (run, pvc). Before 0.2.1 that name omitted the origin
// namespace, so two namespaces holding a SAME-NAMED PVC in one run derived
// colliding object names; because every Create tolerates AlreadyExists, the
// second namespace silently adopted the first's Job/exposure — its own volume
// never backed up (data loss + a Backup that falsely records the first's
// snapshot, or hangs once the first tears down).
//
// The seed deliberately uses distinct PVC names per namespace, so the full
// cascade suite never exercised this. This spec provisions the missing case
// directly: two throwaway namespaces, ONE ClusterBackup, a PVC named identically
// in both. The fix (namespace-qualified object names) means each namespace gets
// its OWN distinct data snapshot; the pre-fix code left one namespace's snapshot
// missing or made the two share a snapshot ID.
// ---------------------------------------------------------------------------

var _ = Describe("M1 — same-named PVCs across namespaces do not collide (0.2.1 regression)", Label("m1"), Ordered, func() {
	const (
		collisionRun = "crucible-collision"
		collisionKey = "crystalbackup.io/crucible-collision"
		collisionVal = "on"
		homonymPVC   = "shared-data" // the SAME PVC name in both namespaces — the archetypal collision
	)
	// Two throwaway tenant namespaces matched by a selector unique to this spec (never the seed
	// label), so nothing here perturbs the cascade run or its leak-check.
	collisionNamespaces := []string{"c-collide-a", "c-collide-b"}

	BeforeAll(func() {
		m1SkipIfNoS3()

		By("Given an initialized ClusterBackupLocation \"dr\" for the shared repository")
		m1EnsurePlatformSecrets()
		var loc cbv1.ClusterBackupLocation
		if err := k8s.Get(ctx, client.ObjectKey{Name: m1LocationName}, &loc); apierrors.IsNotFound(err) {
			// Ensure-only: never delete the shared location in AfterAll — a sibling container may
			// depend on it, and the crucible teardown nukes the bucket regardless.
			m1CreateLocation(m1LocationName, true)
		} else {
			Expect(err).NotTo(HaveOccurred(), "get ClusterBackupLocation %s", m1LocationName)
		}
		m1WaitRepositoryInitialized(m1LocationName)

		By("And two namespaces each holding a ceph-block PVC of the IDENTICAL name " + homonymPVC)
		for _, ns := range collisionNamespaces {
			collisionEnsureNamespace(ns, collisionKey, collisionVal)
			collisionEnsurePVC(ns, homonymPVC)
		}

		By("When one ClusterBackup run backs up both namespaces")
		_ = k8s.Delete(ctx, &cbv1.ClusterBackup{ObjectMeta: metav1.ObjectMeta{Name: collisionRun}})
		Eventually(func() error {
			err := k8s.Create(ctx, &cbv1.ClusterBackup{
				ObjectMeta: metav1.ObjectMeta{Name: collisionRun},
				Spec: cbv1.ClusterBackupSpec{ClusterBackupRunSpec: cbv1.ClusterBackupRunSpec{
					LocationRef: cbv1.LocalObjectReference{Name: m1LocationName},
					Namespaces:  cbv1.NamespaceSelector{MatchLabels: map[string]string{collisionKey: collisionVal}},
				}},
			})
			if apierrors.IsAlreadyExists(err) {
				return fmt.Errorf("ClusterBackup %s still terminating from a previous run", collisionRun)
			}
			return err
		}, time.Minute, 2*time.Second).Should(Succeed())

		m1WaitClusterBackupTerminal(collisionRun, 15*time.Minute)
	})

	AfterAll(func() {
		_ = k8s.Delete(ctx, &cbv1.ClusterBackup{ObjectMeta: metav1.ObjectMeta{Name: collisionRun}})
		for _, ns := range collisionNamespaces {
			// Child Backups are label-linked, not owned — delete them, then the namespace.
			_ = k8s.DeleteAllOf(ctx, &cbv1.Backup{}, client.InNamespace(ns),
				client.MatchingLabels{apiconst.LabelOrigin: apiconst.OriginCluster})
			_ = k8s.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}
	})

	It("backs up same-named PVCs in different namespaces to DISTINCT snapshots (no object-name collision)", func() {
		By("Then the repository holds a data snapshot for the homonym PVC in EACH namespace, with distinct IDs")
		snaps := m1ResticSnapshots(m1LocationName)
		Expect(snaps).NotTo(BeEmpty(), "the collision run must have written snapshots")

		snapByNS := map[string]string{} // namespace -> snapshot ID
		idOwner := map[string]string{}  // snapshot ID -> namespace (to catch a shared ID)
		for _, ns := range collisionNamespaces {
			snap, ok := m1CascadeDataSnapshot(snaps, ns, homonymPVC, collisionRun)
			Expect(ok).To(BeTrue(),
				"namespace %s has NO data snapshot for the homonym PVC %q in run %s — this is the collision bug: its mover was never run (it adopted the other namespace's colliding Job)",
				ns, homonymPVC, collisionRun)
			Expect(snap.Paths).To(ContainElement("/data/"+ns+"/"+homonymPVC),
				"the snapshot for %s must carry ITS OWN path, not the other namespace's", ns)
			if other, dup := idOwner[snap.ID]; dup {
				Fail(fmt.Sprintf("namespaces %s and %s share snapshot ID %s — one namespace's data was substituted for the other's", other, ns, snap.ID))
			}
			idOwner[snap.ID] = ns
			snapByNS[ns] = snap.ID
		}
		Expect(snapByNS).To(HaveLen(len(collisionNamespaces)), "each namespace must own exactly one distinct snapshot")

		By("And each namespace's child Backup reports the homonym volume Completed (not hung / false-success)")
		for _, ns := range collisionNamespaces {
			var b cbv1.Backup
			Expect(k8s.Get(ctx, client.ObjectKey{Namespace: ns, Name: collisionRun}, &b)).To(Succeed(),
				"child Backup %s/%s must exist", ns, collisionRun)
			var vol *cbv1.VolumeStatus
			for i := range b.Status.Volumes {
				if b.Status.Volumes[i].Pvc == homonymPVC {
					vol = &b.Status.Volumes[i]
				}
			}
			Expect(vol).NotTo(BeNil(), "%s's Backup must list the homonym volume", ns)
			Expect(string(vol.Phase)).To(Equal("Completed"),
				"%s/%s must complete on its OWN mover, not adopt the other namespace's or hang", ns, homonymPVC)
		}
	})
})

// collisionEnsureNamespace creates (idempotently) a tenant namespace carrying the collision
// selector label so the dedicated ClusterBackup matches exactly these two namespaces.
func collisionEnsureNamespace(name, labelKey, labelVal string) {
	GinkgoHelper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   name,
		Labels: map[string]string{labelKey: labelVal},
	}}
	err := k8s.Create(ctx, ns)
	if apierrors.IsAlreadyExists(err) {
		var existing corev1.Namespace
		Expect(k8s.Get(ctx, client.ObjectKey{Name: name}, &existing)).To(Succeed())
		if existing.Labels == nil {
			existing.Labels = map[string]string{}
		}
		existing.Labels[labelKey] = labelVal
		Expect(k8s.Update(ctx, &existing)).To(Succeed(), "label existing namespace %s", name)
		return
	}
	Expect(err).NotTo(HaveOccurred(), "create namespace %s", name)
}

// collisionEnsurePVC creates (idempotently) a 1Gi ceph-block PVC. ceph-block is Immediate-binding,
// so the claim binds and is CSI-snapshottable with no pod — enough to exercise the mover naming.
func collisionEnsurePVC(namespace, name string) {
	GinkgoHelper()
	sc := "ceph-block"
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: &sc,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
			},
		},
	}
	err := k8s.Create(ctx, pvc)
	if apierrors.IsAlreadyExists(err) {
		return
	}
	Expect(err).NotTo(HaveOccurred(), "create PVC %s/%s", namespace, name)
}
