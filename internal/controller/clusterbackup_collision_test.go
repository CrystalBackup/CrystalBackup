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
// Run-name COLLISION on the cluster plane.
//
// A ClusterBackup's fan-out used to ask only "does a Backup exist at (namespace,
// run name)?" and read yes as "my child is already there". Three different things
// answer yes — a discovery projection, a previous same-named run's terminal Backup,
// and the namespace plane's own stamped Backup (both planes build run names with the
// same apiconst.RunName, so a ClusterBackupSchedule and a BackupSchedule both called
// "daily" produce byte-identical names). In every case the run then skipped the
// namespace ENTIRELY and aggregated the stranger's Completed volumes as its own:
// pvcsSucceeded up, namespacesSucceeded up, phase Completed, over data it never
// wrote. The only observable difference was an addedBytes of zero.
//
// The specs below are the ones that would have caught it. The load-bearing
// assertion is not "a failure is recorded" — it is that a collided run does NOT
// report success.
// ---------------------------------------------------------------------------

// seedOccupant creates a Backup already sitting at (ns, run) before the run exists, carrying the
// cluster-backup label so the run's aggregate List picks it up — which is exactly the danger.
func seedOccupant(ns, run, loc string, annotations map[string]string) *cbv1.Backup {
	GinkgoHelper()
	b := &cbv1.Backup{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      run,
			Labels: map[string]string{
				apiconst.LabelClusterBackup: run,
				apiconst.LabelOrigin:        apiconst.OriginCluster,
				apiconst.LabelNamespace:     ns,
			},
			Annotations: annotations,
		},
		Spec: cbv1.BackupSpec{
			LocationRef: cbv1.LocationReference{Kind: kindClusterBackupLocation, Name: loc},
		},
	}
	Expect(k8sClient.Create(ctx, b)).To(Succeed())
	DeferCleanup(func() { _ = k8sClient.Delete(context.Background(), b) })
	return b
}

// setOccupantStatus drives a seeded occupant to a phase and volume set, retrying past the
// conflicts the Backup reconciler produces while it gates the object.
func setOccupantStatus(ns, name string, phase status.BackupPhase, vols []cbv1.VolumeStatus) {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		var b cbv1.Backup
		g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &b)).To(Succeed())
		b.Status.Phase = string(phase)
		b.Status.Volumes = vols
		g.Expect(k8sClient.Status().Update(ctx, &b)).To(Succeed())
	}, initTimeout, initPoll).Should(Succeed())
}

// completedVolume is the shape an occupant advertises: a finished PVC with real bytes. If the run
// counts it, the counters below move.
func completedVolume(pvc, snapID string, bytes int64) []cbv1.VolumeStatus {
	return []cbv1.VolumeStatus{{
		Pvc: pvc, Phase: status.VolumePhaseCompleted, SnapshotID: snapID, AddedBytes: bytes,
	}}
}

// expectRunNeverReportsSuccess is the assertion the whole fix exists for.
func expectRunNeverReportsSuccess(run, ns string) {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		cb := getClusterRunG(g, run)
		g.Expect(cb.Status.Phase).To(Equal(string(status.ClusterBackupPhaseFailed)),
			"a run whose only namespace was never backed up must NOT end Completed")
		g.Expect(cb.Status.NamespacesSucceeded).To(Equal(int32(0)),
			"a namespace this run never wrote to must not be counted as succeeded")
		g.Expect(cb.Status.NamespacesFailed).To(Equal(int32(1)))
		g.Expect(cb.Status.PVCsSucceeded).To(Equal(int32(0)),
			"the occupant's volumes were written by somebody else; they are not this run's successes")
		g.Expect(cb.Status.AddedBytes).To(Equal(int64(0)))
		g.Expect(cb.Status.Failures).To(HaveLen(1))
		g.Expect(cb.Status.Failures[0].Namespace).To(Equal(ns))
		g.Expect(cb.Status.Failures[0].Message).To(ContainSubstring(reasonRunNameCollision))
		c := status.FindCondition(cb.Status.Conditions, ConditionReady)
		g.Expect(c).NotTo(BeNil())
		g.Expect(c.Status).To(Equal(metav1.ConditionFalse))
	}, initTimeout, initPoll).Should(Succeed())
}

var _ = Describe("ClusterBackupReconciler run-name collisions", func() {

	It("stamps its own UID on every child it fans out", func() {
		const loc = "cb-loc-stamp"
		const run = "cb-run-stamp"
		lbl := map[string]string{cbTestCaseLabel: "stamp"}
		createTestLocation(newTestLocation(loc, "kek-stamp", "s3-stamp", false))
		ns := "cbns-stamp-a"
		createLabelledNamespace(ns, lbl)
		createClusterRun(run, loc, cbv1.NamespaceSelector{MatchLabels: lbl})

		waitChildrenExist(run, ns)

		var runObj cbv1.ClusterBackup
		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: run}, &runObj)).To(Succeed())
		var child cbv1.Backup
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: run}, &child)).To(Succeed())
		Expect(child.Annotations).To(HaveKeyWithValue(apiconst.AnnotationParentUID, string(runObj.UID)),
			"without the parent UID the run cannot tell its own child from a homonym")
	})

	// THE regression spec. A ClusterBackup recreated at a name whose snapshots discovery has
	// already projected — GitOps prune-and-recreate, `kubectl replace --force`, a reused
	// convenience name like "pre-upgrade". Before the fix this run reported Completed with
	// pvcsSucceeded=1 over a snapshot written weeks earlier.
	It("FAILS instead of reporting success when a discovery projection occupies the run name", func() {
		const loc = "cb-loc-projcollide"
		const run = "cb-run-projcollide"
		lbl := map[string]string{cbTestCaseLabel: "projcollide"}
		createTestLocation(newTestLocation(loc, "kek-projcollide", "s3-projcollide", false))
		ns := "cbns-projcollide"
		createLabelledNamespace(ns, lbl)

		By("Given a discovery projection already sitting at the run's coordinate")
		seedOccupant(ns, run, loc, map[string]string{apiconst.AnnotationProjected: apiconst.AnnotationProjectedValue})
		setOccupantStatus(ns, run, status.BackupPhaseCompleted, completedVolume("data", "snap-old", 4096))

		By("When a ClusterBackup of the same name runs")
		createClusterRun(run, loc, cbv1.NamespaceSelector{MatchLabels: lbl})

		By("Then the run fails loudly and counts nothing it did not write")
		expectRunNeverReportsSuccess(run, ns)

		By("And the projection is left exactly as it was — the run never touched it")
		var occupant cbv1.Backup
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: run}, &occupant)).To(Succeed())
		Expect(occupant.Annotations).To(HaveKeyWithValue(apiconst.AnnotationProjected, apiconst.AnnotationProjectedValue))
		Expect(occupant.Annotations).NotTo(HaveKey(apiconst.AnnotationParentUID))
		Expect(occupant.Status.Volumes[0].SnapshotID).To(Equal("snap-old"))
	})

	It("FAILS when an earlier run's terminal Backup occupies the run name", func() {
		const loc = "cb-loc-prevcollide"
		const run = "cb-run-prevcollide"
		lbl := map[string]string{cbTestCaseLabel: "prevcollide"}
		createTestLocation(newTestLocation(loc, "kek-prevcollide", "s3-prevcollide", false))
		ns := "cbns-prevcollide"
		createLabelledNamespace(ns, lbl)

		By("Given the terminal child of a previous, differently-identified run of the same name")
		seedOccupant(ns, run, loc, map[string]string{apiconst.AnnotationParentUID: string(testOtherUID)})
		setOccupantStatus(ns, run, status.BackupPhaseCompleted, completedVolume("data", "snap-prev", 8192))

		By("When a ClusterBackup is recreated at that name")
		createClusterRun(run, loc, cbv1.NamespaceSelector{MatchLabels: lbl})

		By("Then the run fails instead of adopting the earlier run's result")
		expectRunNeverReportsSuccess(run, ns)
	})

	// Defence in depth (brief §4): even if the coordinate check were bypassed — here by stamping
	// the run's OWN UID onto a projection, which no production path produces — a projection must
	// still never increment pvcsSucceeded. Its volumes are derived from
	// discovery.VolumesFromSnapshots and read Completed by construction.
	It("never counts a projected child, even one carrying this run's own UID", func() {
		const loc = "cb-loc-projguard"
		const run = "cb-run-projguard"
		lbl := map[string]string{cbTestCaseLabel: "projguard"}
		createTestLocation(newTestLocation(loc, "kek-projguard", "s3-projguard", false))
		ns := "cbns-projguard"
		createLabelledNamespace(ns, lbl)

		createClusterRun(run, loc, cbv1.NamespaceSelector{MatchLabels: lbl})
		waitChildrenExist(run, ns)

		By("marking the run's own child as projected and giving it a completed volume")
		Eventually(func(g Gomega) {
			var child cbv1.Backup
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: run}, &child)).To(Succeed())
			if child.Annotations == nil {
				child.Annotations = map[string]string{}
			}
			child.Annotations[apiconst.AnnotationProjected] = apiconst.AnnotationProjectedValue
			g.Expect(k8sClient.Update(ctx, &child)).To(Succeed())
		}, initTimeout, initPoll).Should(Succeed())
		setOccupantStatus(ns, run, status.BackupPhaseCompleted, completedVolume("data", "snap-proj", 2048))

		By("the aggregate refuses to count it")
		expectRunNeverReportsSuccess(run, ns)
	})

	// The operator-UPGRADE path. A build without the UID stamp fanned this child out and the run
	// is still in flight when the new binary takes over. It holds no result, so it is adopted —
	// declaring a collision here would fail a run against its own child.
	It("adopts an unstamped, result-free child instead of colliding with it", func() {
		const loc = "cb-loc-adopt"
		const run = "cb-run-adopt"
		lbl := map[string]string{cbTestCaseLabel: "cbadopt"}
		createTestLocation(newTestLocation(loc, "kek-cbadopt", "s3-cbadopt", false))
		ns := "cbns-cbadopt"
		createLabelledNamespace(ns, lbl)

		By("Given a pre-stamp child mid-flight (no parent UID, no result of any kind)")
		seedOccupant(ns, run, loc, nil)

		By("When the run reconciles")
		createClusterRun(run, loc, cbv1.NamespaceSelector{MatchLabels: lbl})
		var runObj cbv1.ClusterBackup
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: run}, &runObj)).To(Succeed())
		}, initTimeout, initPoll).Should(Succeed())

		By("Then the child is claimed with the run's UID rather than declared foreign")
		Eventually(func(g Gomega) {
			var child cbv1.Backup
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: run}, &child)).To(Succeed())
			g.Expect(child.Annotations).To(HaveKeyWithValue(apiconst.AnnotationParentUID, string(runObj.UID)))
		}, initTimeout, initPoll).Should(Succeed())

		By("And the run proceeds normally: the adopted child's success IS this run's success")
		patchChildTerminal(ns, run, status.BackupPhaseCompleted,
			completedVolume("data", "snap-adopt", 1024), metav1.ConditionTrue, "Completed", "done")
		Eventually(func(g Gomega) {
			cb := getClusterRunG(g, run)
			g.Expect(cb.Status.Phase).To(Equal(string(status.ClusterBackupPhaseCompleted)))
			g.Expect(cb.Status.NamespacesSucceeded).To(Equal(int32(1)))
			g.Expect(cb.Status.PVCsSucceeded).To(Equal(int32(1)))
			g.Expect(cb.Status.AddedBytes).To(Equal(int64(1024)))
			g.Expect(cb.Status.Failures).To(BeEmpty())
		}, initTimeout, initPoll).Should(Succeed())
	})

	// A collision in one namespace must not cost the healthy ones their backup: the fan-out
	// records the failure per namespace and carries on.
	It("keeps backing up the healthy namespaces when one collides", func() {
		const loc = "cb-loc-mixcollide"
		const run = "cb-run-mixcollide"
		lbl := map[string]string{cbTestCaseLabel: "mixcollide"}
		createTestLocation(newTestLocation(loc, "kek-mixcollide", "s3-mixcollide", false))
		nsOK, nsBad := "cbns-mixcollide-ok", "cbns-mixcollide-bad"
		createLabelledNamespace(nsOK, lbl)
		createLabelledNamespace(nsBad, lbl)

		seedOccupant(nsBad, run, loc, map[string]string{apiconst.AnnotationProjected: apiconst.AnnotationProjectedValue})
		setOccupantStatus(nsBad, run, status.BackupPhaseCompleted, completedVolume("data", "snap-x", 999))

		createClusterRun(run, loc, cbv1.NamespaceSelector{MatchLabels: lbl})
		waitChildrenExist(run, nsOK)
		patchChildTerminal(nsOK, run, status.BackupPhaseCompleted,
			completedVolume("data", "snap-ok", 512), metav1.ConditionTrue, "Completed", "done")

		Eventually(func(g Gomega) {
			cb := getClusterRunG(g, run)
			g.Expect(cb.Status.Phase).To(Equal(string(status.ClusterBackupPhasePartiallyFailed)))
			g.Expect(cb.Status.NamespacesSucceeded).To(Equal(int32(1)))
			g.Expect(cb.Status.NamespacesFailed).To(Equal(int32(1)))
			g.Expect(cb.Status.PVCsSucceeded).To(Equal(int32(1)))
			g.Expect(cb.Status.AddedBytes).To(Equal(int64(512)), "only the healthy namespace's bytes count")
			g.Expect(cb.Status.Failures).To(HaveLen(1))
			g.Expect(cb.Status.Failures[0].Namespace).To(Equal(nsBad))
			g.Expect(cb.Status.Failures[0].Message).To(ContainSubstring(reasonRunNameCollision))
		}, initTimeout, initPoll).Should(Succeed())

		By("and the collided namespace never received a child of this run")
		var occupant cbv1.Backup
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: nsBad, Name: run}, &occupant)).To(Succeed())
		Expect(occupant.Annotations).NotTo(HaveKey(apiconst.AnnotationParentUID))
	})

	It("re-reconciles a run without collapsing into a self-collision", func() {
		const loc = "cb-loc-selfsame"
		const run = "cb-run-selfsame"
		lbl := map[string]string{cbTestCaseLabel: "selfsame"}
		createTestLocation(newTestLocation(loc, "kek-selfsame", "s3-selfsame", false))
		ns := "cbns-selfsame"
		createLabelledNamespace(ns, lbl)
		createClusterRun(run, loc, cbv1.NamespaceSelector{MatchLabels: lbl})
		waitChildrenExist(run, ns)

		// Many passes run over the poll interval and the child watch; a run must never accumulate
		// a collision against the child it created itself.
		Consistently(func(g Gomega) {
			cb := getClusterRunG(g, run)
			g.Expect(cb.Status.Failures).To(BeEmpty())
			g.Expect(cb.Status.NamespacesFailed).To(Equal(int32(0)))
		}, 3*time.Second, 500*time.Millisecond).Should(Succeed())
	})
})
