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

package controller

import (
	"context"
	"fmt"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2" //nolint:revive,staticcheck
	. "github.com/onsi/gomega"    //nolint:revive,staticcheck

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
	"github.com/CrystalBackup/CrystalBackup/internal/restic"
	"github.com/CrystalBackup/CrystalBackup/internal/status"
)

// stubSnapshotLister is the envtest SnapshotLister: the discovery specs feed it canned snapshots
// (mutex-guarded, since the manager reconciles on another goroutine while a spec mutates it).
type stubSnapshotLister struct {
	mu    sync.Mutex
	snaps []restic.Snapshot
	err   error
}

func (s *stubSnapshotLister) List(context.Context, *cbv1.BackupRepository) ([]restic.Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]restic.Snapshot(nil), s.snaps...), s.err
}

func (s *stubSnapshotLister) set(snaps ...restic.Snapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snaps = snaps
	s.err = nil
}

// discDataSnap builds a data snapshot with the pinned restic identity tags for (ns, pvc, run).
func discDataSnap(id, ns, pvc, run string) restic.Snapshot {
	return restic.Snapshot{
		ID:    id,
		Paths: []string{"/data/" + ns + "/" + pvc},
		Tags: []string{
			restic.TagBase,
			restic.Tag(restic.TagKeyKind, restic.KindData),
			restic.Tag(restic.TagKeyNamespace, ns),
			restic.Tag(restic.TagKeyPVC, pvc),
			restic.Tag(restic.TagKeyRun, run),
		},
	}
}

// pokeRepository nudges the discovery controller to re-inventory now (the interval is minutes; the
// specs drive it by bumping a throwaway annotation on the repository).
func pokeRepository(name string) {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		var repo cbv1.BackupRepository
		g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: name}, &repo)).To(Succeed())
		if repo.Annotations == nil {
			repo.Annotations = map[string]string{}
		}
		repo.Annotations["test.crystalbackup.io/poke"] = fmt.Sprintf("%d", time.Now().UnixNano())
		g.Expect(k8sClient.Update(ctx, &repo)).To(Succeed())
	}, eventuallyTimeout, eventuallyPoll).Should(Succeed())
}

var _ = Describe("DiscoveryReconciler", func() {
	// Each spec sets the stub explicitly; reset between them so a spec that lists nothing does not
	// inherit a prior spec's inventory.
	BeforeEach(func() { discoveryLister.set() })

	It("projects a Backup per (namespace, run) into existing namespaces, skipping absent ones", func() {
		const loc = "disc-loc-project"
		const run = "disc-run-project"
		seedInitializedRepo(loc, "kek-disc-p", "s3-disc-p")
		createTenantNamespace("disc-present")

		discoveryLister.set(
			discDataSnap("id-present", "disc-present", "pvc-a", run),
			discDataSnap("id-absent", "disc-absent-ns", "pvc-b", run), // namespace does not exist
		)
		pokeRepository(loc)

		By("a projection appears in the existing namespace, labelled and annotated, with derived volumes")
		Eventually(func(g Gomega) {
			var b cbv1.Backup
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: "disc-present", Name: run}, &b)).To(Succeed())
			g.Expect(b.Labels[apiconst.LabelOrigin]).To(Equal(apiconst.OriginCluster))
			g.Expect(b.Labels[apiconst.LabelClusterBackup]).To(Equal(run))
			g.Expect(b.Labels[apiconst.LabelNamespace]).To(Equal("disc-present"))
			g.Expect(b.Annotations[apiconst.AnnotationProjected]).To(Equal(apiconst.AnnotationProjectedValue))
			g.Expect(b.Spec.LocationRef.Name).To(Equal(loc))
			g.Expect(b.Status.Volumes).To(HaveLen(1))
			g.Expect(b.Status.Volumes[0].Pvc).To(Equal("pvc-a"))
			g.Expect(b.Status.Volumes[0].SnapshotID).To(Equal("id-present"))
			g.Expect(string(b.Status.Volumes[0].Phase)).To(Equal("Completed"))
			for _, or := range b.OwnerReferences {
				g.Expect(or.Kind).NotTo(Equal("ClusterBackup"))
			}
		}, eventuallyTimeout, eventuallyPoll).Should(Succeed())

		By("no projection is fabricated for the non-existent namespace")
		Consistently(func(g Gomega) {
			err := k8sClient.Get(ctx, client.ObjectKey{Namespace: "disc-absent-ns", Name: run}, &cbv1.Backup{})
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
		}, 2*time.Second, 500*time.Millisecond).Should(Succeed())

		By("the repository records the inventory")
		Eventually(func(g Gomega) {
			var repo cbv1.BackupRepository
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: loc}, &repo)).To(Succeed())
			g.Expect(repo.Status.SnapshotCount).To(Equal(int32(2)))
			g.Expect(repo.Status.NamespacesPresent).To(Equal(int32(2)))
			g.Expect(repo.Status.LastDiscoveryTime).NotTo(BeNil())
		}, eventuallyTimeout, eventuallyPoll).Should(Succeed())
	})

	It("removes a projection once its repository snapshots are gone (post-forget)", func() {
		const loc = "disc-loc-gc"
		const run = "disc-run-gc"
		seedInitializedRepo(loc, "kek-disc-gc", "s3-disc-gc")
		createTenantNamespace("disc-gc-ns")

		discoveryLister.set(discDataSnap("id-gc", "disc-gc-ns", "pvc-a", run))
		pokeRepository(loc)
		By("the projection exists while its snapshots are present")
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: "disc-gc-ns", Name: run}, &cbv1.Backup{})).To(Succeed())
		}, eventuallyTimeout, eventuallyPoll).Should(Succeed())

		By("after the snapshots are forgotten, discovery removes the projection")
		discoveryLister.set() // repository now empty for this run
		pokeRepository(loc)
		Eventually(func(g Gomega) {
			err := k8sClient.Get(ctx, client.ObjectKey{Namespace: "disc-gc-ns", Name: run}, &cbv1.Backup{})
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
		}, eventuallyTimeout, eventuallyPoll).Should(Succeed())
	})

	It("leaves another location's projections alone (multi-location GC isolation)", func() {
		const loc = "disc-loc-iso"
		const foreignRun = "disc-run-foreign"
		seedInitializedRepo(loc, "kek-disc-iso", "s3-disc-iso")
		createTenantNamespace("disc-iso-ns")

		// A projection owned by a DIFFERENT location's repository. loc's discovery must never GC it,
		// even though its (namespace, run) is absent from loc's inventory — the other location's own
		// discovery owns its lifecycle. Before the location-scoped GC, loc's pass would delete it with
		// a false "its repository snapshots are gone", and the two locations would flap each other's
		// projections every interval.
		foreign := &cbv1.Backup{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "disc-iso-ns",
				Name:      foreignRun,
				Labels: map[string]string{
					apiconst.LabelOrigin:        apiconst.OriginCluster,
					apiconst.LabelClusterBackup: foreignRun,
					apiconst.LabelNamespace:     "disc-iso-ns",
				},
				Annotations: map[string]string{apiconst.AnnotationProjected: apiconst.AnnotationProjectedValue},
			},
			Spec: cbv1.BackupSpec{LocationRef: cbv1.LocationReference{Kind: "ClusterBackupLocation", Name: "disc-other-location"}},
		}
		Expect(k8sClient.Create(ctx, foreign)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(context.Background(), foreign) })

		// loc's repository holds an unrelated run (never the foreign key), so loc's GC pass sees the
		// foreign projection as "not in my inventory".
		discoveryLister.set(discDataSnap("id-iso", "disc-iso-ns", "pvc-z", "disc-run-iso-local"))
		pokeRepository(loc)

		By("loc's discovery completed a full pass (its OWN projection appears)")
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: "disc-iso-ns", Name: "disc-run-iso-local"}, &cbv1.Backup{})).To(Succeed())
		}, eventuallyTimeout, eventuallyPoll).Should(Succeed())

		By("yet the foreign-location projection is untouched")
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: "disc-iso-ns", Name: foreignRun}, &cbv1.Backup{})).
			To(Succeed(), "loc's GC must not delete a projection belonging to another location")
	})

	It("never touches an in-flight execution Backup, but adopts a terminal one", func() {
		const loc = "disc-loc-adopt"
		const run = "disc-run-adopt"
		seedInitializedRepo(loc, "kek-disc-a", "s3-disc-a")
		createTenantNamespace("disc-adopt-ns")

		// An execution Backup (no projected annotation) occupies the name. With no parent
		// ClusterBackup it gates to a non-terminal phase — an in-flight run discovery must not touch.
		exec := &cbv1.Backup{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "disc-adopt-ns",
				Name:      run,
				Labels: map[string]string{
					apiconst.LabelOrigin:        apiconst.OriginCluster,
					apiconst.LabelClusterBackup: run,
					apiconst.LabelNamespace:     "disc-adopt-ns",
				},
			},
			Spec: cbv1.BackupSpec{LocationRef: cbv1.LocationReference{Kind: "ClusterBackupLocation", Name: loc}},
		}
		Expect(k8sClient.Create(ctx, exec)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(context.Background(), exec) })

		discoveryLister.set(discDataSnap("id-adopt", "disc-adopt-ns", "pvc-a", run))
		pokeRepository(loc)

		By("discovery leaves the in-flight execution Backup unprojected")
		Consistently(func(g Gomega) {
			var b cbv1.Backup
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: "disc-adopt-ns", Name: run}, &b)).To(Succeed())
			g.Expect(b.Annotations[apiconst.AnnotationProjected]).To(BeEmpty())
		}, 3*time.Second, 500*time.Millisecond).Should(Succeed())

		By("once the execution Backup is terminal, discovery adopts it into a projection")
		Eventually(func(g Gomega) {
			var b cbv1.Backup
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: "disc-adopt-ns", Name: run}, &b)).To(Succeed())
			b.Status.Phase = string(status.BackupPhaseCompleted)
			g.Expect(k8sClient.Status().Update(ctx, &b)).To(Succeed())
		}, initTimeout, initPoll).Should(Succeed())
		pokeRepository(loc)
		Eventually(func(g Gomega) {
			var b cbv1.Backup
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: "disc-adopt-ns", Name: run}, &b)).To(Succeed())
			g.Expect(b.Annotations[apiconst.AnnotationProjected]).To(Equal(apiconst.AnnotationProjectedValue))
			g.Expect(b.Status.Volumes).To(HaveLen(1))
			g.Expect(b.Status.Volumes[0].SnapshotID).To(Equal("id-adopt"))
		}, eventuallyTimeout, eventuallyPoll).Should(Succeed())
	})

	It("re-creates a projection a client deletes", func() {
		const loc = "disc-loc-recreate"
		const run = "disc-run-recreate"
		seedInitializedRepo(loc, "kek-disc-r", "s3-disc-r")
		createTenantNamespace("disc-recreate-ns")

		discoveryLister.set(discDataSnap("id-r", "disc-recreate-ns", "pvc-a", run))
		pokeRepository(loc)

		var first cbv1.Backup
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: "disc-recreate-ns", Name: run}, &first)).To(Succeed())
			g.Expect(first.Annotations[apiconst.AnnotationProjected]).To(Equal(apiconst.AnnotationProjectedValue))
		}, eventuallyTimeout, eventuallyPoll).Should(Succeed())

		By("deleting the projection and re-inventorying re-creates it as a fresh object")
		Expect(k8sClient.Delete(ctx, &first)).To(Succeed())
		Eventually(func(g Gomega) {
			// Nudge discovery BEFORE checking: it does not watch Backups, so a fresh inventory is
			// what re-creates the deleted projection. (Best-effort — a conflict just retries.)
			var repo cbv1.BackupRepository
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: loc}, &repo)).To(Succeed())
			if repo.Annotations == nil {
				repo.Annotations = map[string]string{}
			}
			repo.Annotations["test.crystalbackup.io/poke"] = fmt.Sprintf("%d", time.Now().UnixNano())
			_ = k8sClient.Update(ctx, &repo)

			var re cbv1.Backup
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: "disc-recreate-ns", Name: run}, &re)).To(Succeed())
			g.Expect(re.UID).NotTo(Equal(first.UID))
		}, initTimeout, initPoll).Should(Succeed())
	})
})
