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
	"time"

	. "github.com/onsi/ginkgo/v2" //nolint:revive,staticcheck
	. "github.com/onsi/gomega"    //nolint:revive,staticcheck

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
	"github.com/CrystalBackup/CrystalBackup/internal/status"
)

// ---------------------------------------------------------------------------
// A projection COMPLETES an execution report; it never erases one.
//
// projectGroup adopts a terminal execution Backup into a projection and then applies
// a status rebuilt from the repository under SSA ForceOwnership. status.volumes has
// no listType marker, so that apply used to REPLACE the whole list — and the
// rebuild's only input is `restic snapshots`, which by construction lists only what
// SUCCEEDED. Measured on a live cluster four seconds apart: a Skipped volume with its
// CSISnapshotUnsupported reason vanished, taking addedBytes, sizeBytes and node with
// it, and the recorded phase was raised to Completed. Thirty seconds after a run
// ended partially, the failure existed nowhere in the cluster.
//
// A Backup is both a catalogue of restore points (R26 — it must survive the loss of
// the cluster) and the report of an execution. When the two disagree the EXECUTION
// REPORT WINS: it is the more specific truth, and the reconstruction cannot even see
// what it is contradicting.
// ---------------------------------------------------------------------------

var _ = Describe("DiscoveryReconciler projection merge", func() {
	BeforeEach(func() { discoveryLister.set() })

	It("completes an adopted execution report without erasing it", func() {
		const loc = "disc-loc-merge"
		const run = "disc-run-merge"
		const ns = "disc-merge-ns"
		seedInitializedRepo(loc, "kek-disc-m", "s3-disc-m")
		createTenantNamespace(ns)

		By("Given a finished run that partially failed: one volume moved, one skipped, one failed")
		exec := &cbv1.Backup{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: ns,
				Name:      run,
				Labels: map[string]string{
					apiconst.LabelOrigin:        apiconst.OriginCluster,
					apiconst.LabelClusterBackup: run,
					apiconst.LabelNamespace:     ns,
				},
			},
			Spec: cbv1.BackupSpec{LocationRef: cbv1.LocationReference{Kind: kindClusterBackupLocation, Name: loc}},
		}
		Expect(k8sClient.Create(ctx, exec)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(context.Background(), exec) })

		recorded := []cbv1.VolumeStatus{
			{Pvc: "data", Phase: status.VolumePhaseCompleted, SnapshotID: "id-merge",
				AddedBytes: 4096, SizeBytes: 8192, Node: "node-a"},
			{Pvc: "broken", Phase: status.VolumePhaseFailed, Reason: "MoverFailed"},
			{Pvc: "scratch", Phase: status.VolumePhaseSkipped, Reason: backupReasonSkippedUnsupported},
		}
		Eventually(func(g Gomega) {
			var b cbv1.Backup
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: run}, &b)).To(Succeed())
			b.Status.Phase = string(status.BackupPhasePartiallyFailed)
			b.Status.Volumes = recorded
			g.Expect(k8sClient.Status().Update(ctx, &b)).To(Succeed())
		}, initTimeout, initPoll).Should(Succeed())

		By("When discovery projects the repository, which only knows about the volume that succeeded")
		discoveryLister.set(discDataSnap("id-merge", ns, "data", run))
		pokeRepository(loc)

		By("Then the Backup is adopted into a projection")
		Eventually(func(g Gomega) {
			var b cbv1.Backup
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: run}, &b)).To(Succeed())
			g.Expect(b.Annotations[apiconst.AnnotationProjected]).To(Equal(apiconst.AnnotationProjectedValue))
		}, eventuallyTimeout, eventuallyPoll).Should(Succeed())

		By("And the execution report survives it, phase included — repeatedly, over several passes")
		pokeRepository(loc)
		Consistently(func(g Gomega) {
			var b cbv1.Backup
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: run}, &b)).To(Succeed())
			g.Expect(b.Status.Phase).To(Equal(string(status.BackupPhasePartiallyFailed)),
				"a projection rebuilt from snapshots must never raise a recorded phase to a better one")

			byPVC := map[string]cbv1.VolumeStatus{}
			for _, v := range b.Status.Volumes {
				byPVC[v.Pvc] = v
			}
			g.Expect(byPVC).To(HaveLen(3), "the projection must not drop the volumes it cannot see")

			g.Expect(byPVC["scratch"].Phase).To(Equal(status.VolumePhaseSkipped))
			g.Expect(byPVC["scratch"].Reason).To(Equal(backupReasonSkippedUnsupported),
				"a skip has no snapshot by definition; only the execution record holds it")
			g.Expect(byPVC["broken"].Phase).To(Equal(status.VolumePhaseFailed))
			g.Expect(byPVC["broken"].Reason).To(Equal("MoverFailed"))

			g.Expect(byPVC["data"].SnapshotID).To(Equal("id-merge"))
			g.Expect(byPVC["data"].AddedBytes).To(Equal(int64(4096)))
			g.Expect(byPVC["data"].SizeBytes).To(Equal(int64(8192)))
			g.Expect(byPVC["data"].Node).To(Equal("node-a"))
		}, 6*time.Second, 500*time.Millisecond).Should(Succeed())
	})

	It("still adds a restore point the record never knew about", func() {
		const loc = "disc-loc-mergeadd"
		const run = "disc-run-mergeadd"
		const ns = "disc-mergeadd-ns"
		seedInitializedRepo(loc, "kek-disc-ma", "s3-disc-ma")
		createTenantNamespace(ns)

		// Two snapshots in the repository, one recorded volume: the catalogue half must still
		// surface the second, or a Backup rebuilt after losing the cluster would under-report.
		discoveryLister.set(
			discDataSnap("id-a", ns, "pvc-a", run),
			discDataSnap("id-b", ns, "pvc-b", run),
		)
		pokeRepository(loc)

		Eventually(func(g Gomega) {
			var b cbv1.Backup
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: run}, &b)).To(Succeed())
			g.Expect(b.Status.Volumes).To(HaveLen(2))
			g.Expect(b.Status.Volumes[0].Pvc).To(Equal("pvc-a"))
			g.Expect(b.Status.Volumes[1].Pvc).To(Equal("pvc-b"))
			g.Expect(b.Status.Phase).To(Equal(string(status.BackupPhaseCompleted)))
		}, eventuallyTimeout, eventuallyPoll).Should(Succeed())
	})
})
