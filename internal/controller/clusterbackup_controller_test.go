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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
	"github.com/CrystalBackup/CrystalBackup/internal/status"
)

// cbTestCaseLabel is the single label key every ClusterBackup spec selects on. Each scenario uses
// a DISTINCT value so its selector matches only the namespaces it created, immune to the namespaces
// other specs leave lingering (envtest has no namespace GC).
const cbTestCaseLabel = "cbtestcase"

// ---------------------------------------------------------------------------
// ClusterBackup-suite helpers.
// ---------------------------------------------------------------------------

// createLabelledNamespace creates a namespace carrying labels (best-effort deleted after the spec;
// envtest has no namespace controller, so it lingers Terminating — harmless, names are unique).
func createLabelledNamespace(name string, labels map[string]string) {
	GinkgoHelper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
	Expect(k8sClient.Create(ctx, ns)).To(Succeed())
	DeferCleanup(func() { _ = k8sClient.Delete(context.Background(), ns) })
}

// createClusterRun creates a (cluster-scoped) ClusterBackup manual run selecting namespaces by sel
// and writing to location. Registers a best-effort delete of the run and its label-linked children.
func createClusterRun(name, location string, sel cbv1.NamespaceSelector) {
	GinkgoHelper()
	cb := &cbv1.ClusterBackup{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: cbv1.ClusterBackupSpec{
			ClusterBackupRunSpec: cbv1.ClusterBackupRunSpec{
				LocationRef: cbv1.LocalObjectReference{Name: location},
				Namespaces:  sel,
			},
		},
	}
	Expect(k8sClient.Create(ctx, cb)).To(Succeed())
	DeferCleanup(func() {
		_ = k8sClient.Delete(context.Background(), cb)
		// Children are label-linked, not owned, so they never cascade — delete them explicitly.
		_ = k8sClient.DeleteAllOf(context.Background(), &cbv1.Backup{},
			client.MatchingLabels{apiconst.LabelClusterBackup: name})
	})
}

// getClusterRunG fetches a ClusterBackup inside an Eventually block (retry-on-missing).
func getClusterRunG(g Gomega, name string) cbv1.ClusterBackup {
	var cb cbv1.ClusterBackup
	g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: name}, &cb)).To(Succeed())
	return cb
}

// waitChildrenExist waits until the run has fanned a child named after it into every namespace.
func waitChildrenExist(run string, namespaces ...string) {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		for _, ns := range namespaces {
			g.Expect(k8sClient.Get(ctx,
				client.ObjectKey{Namespace: ns, Name: run}, &cbv1.Backup{})).To(Succeed())
		}
	}, initTimeout, initPoll).Should(Succeed())
}

// patchChildTerminal forces a child Backup into a terminal phase, retrying on the write conflicts
// the live Backup reconciler produces while it gates the child. Once the child is terminal that
// reconciler freezes it (isTerminalBackupPhase), so the write sticks and the ClusterBackup can
// aggregate a stable result. It also stamps the child's Ready condition, which the aggregator reads
// for a failed child's failure message.
func patchChildTerminal(
	namespace, name string, phase status.BackupPhase, volumes []cbv1.VolumeStatus,
	ready metav1.ConditionStatus, reason, message string,
) {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		var child cbv1.Backup
		g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &child)).To(Succeed())
		child.Status.Phase = string(phase)
		child.Status.Volumes = volumes
		status.SetCondition(&child.Status.Conditions, ConditionReady, ready, reason, message, child.Generation)
		g.Expect(k8sClient.Status().Update(ctx, &child)).To(Succeed())
	}, initTimeout, initPoll).Should(Succeed())
}

var _ = Describe("ClusterBackupReconciler", func() {

	It("fans out one label-linked child Backup per matched namespace, with no ownerReference to the run", func() {
		const loc = "cb-loc-fanout"
		const run = "cb-run-fanout"
		lbl := map[string]string{cbTestCaseLabel: "fanout"}
		createTestLocation(newTestLocation(loc, "kek-fanout", "s3-fanout", false))

		nsA, nsB, nsC := "cbns-fanout-a", "cbns-fanout-b", "cbns-fanout-c"
		for _, n := range []string{nsA, nsB, nsC} {
			createLabelledNamespace(n, lbl)
		}
		// An unmatched namespace (different label value) must NOT receive a child.
		createLabelledNamespace("cbns-fanout-other", map[string]string{cbTestCaseLabel: "fanoutother"})

		createClusterRun(run, loc, cbv1.NamespaceSelector{MatchLabels: lbl})

		By("a child Backup named after the run appears in each matched namespace")
		waitChildrenExist(run, nsA, nsB, nsC)

		By("each child is correctly labelled, points at the ClusterBackupLocation, and is not owned by the run")
		var runObj cbv1.ClusterBackup
		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: run}, &runObj)).To(Succeed())
		for _, ns := range []string{nsA, nsB, nsC} {
			var child cbv1.Backup
			Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: run}, &child)).To(Succeed())
			Expect(child.Labels).To(HaveKeyWithValue(apiconst.LabelClusterBackup, run))
			Expect(child.Labels).To(HaveKeyWithValue(apiconst.LabelOrigin, apiconst.OriginCluster))
			Expect(child.Labels).To(HaveKeyWithValue(apiconst.LabelNamespace, ns))
			Expect(child.Spec.LocationRef.Kind).To(Equal("ClusterBackupLocation"))
			Expect(child.Spec.LocationRef.Name).To(Equal(loc))
			for _, ref := range child.OwnerReferences {
				Expect(ref.Kind).NotTo(Equal("ClusterBackup"),
					"child %s/%s must not be owned by the ClusterBackup", ns, child.Name)
				Expect(ref.UID).NotTo(Equal(runObj.UID))
			}
		}

		By("the unmatched namespace receives no child")
		Consistently(func(g Gomega) {
			err := k8sClient.Get(ctx, client.ObjectKey{Namespace: "cbns-fanout-other", Name: run}, &cbv1.Backup{})
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
		}, 2*time.Second, 500*time.Millisecond).Should(Succeed())

		By("the run reports namespacesMatched=3")
		Eventually(func(g Gomega) {
			g.Expect(getClusterRunG(g, run).Status.NamespacesMatched).To(Equal(int32(3)))
		}, initTimeout, initPoll).Should(Succeed())
	})

	It("blocks with LocationNotFound and fans out nothing when the location is absent", func() {
		const run = "cb-run-noloc"
		createLabelledNamespace("cbns-noloc", map[string]string{cbTestCaseLabel: "noloc"})
		createClusterRun(run, "cb-loc-does-not-exist",
			cbv1.NamespaceSelector{MatchLabels: map[string]string{cbTestCaseLabel: "noloc"}})

		By("the run reports Ready=False/LocationNotFound and stays Pending")
		Eventually(func(g Gomega) {
			cb := getClusterRunG(g, run)
			g.Expect(cb.Status.Phase).To(Equal(string(status.ClusterBackupPhasePending)))
			c := status.FindCondition(cb.Status.Conditions, ConditionReady)
			g.Expect(c).NotTo(BeNil())
			g.Expect(c.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(c.Reason).To(Equal("LocationNotFound"))
		}, initTimeout, initPoll).Should(Succeed())

		By("no child Backup is created")
		Consistently(func(g Gomega) {
			var kids cbv1.BackupList
			g.Expect(k8sClient.List(ctx, &kids, client.MatchingLabels{apiconst.LabelClusterBackup: run})).To(Succeed())
			g.Expect(kids.Items).To(BeEmpty())
		}, 2*time.Second, 500*time.Millisecond).Should(Succeed())
	})

	It("blocks with SelectorInvalid when the selector sets no positive form", func() {
		const loc = "cb-loc-badsel"
		const run = "cb-run-badsel"
		createTestLocation(newTestLocation(loc, "kek-badsel", "s3-badsel", false))
		// exclude alone is not a positive form → rule 8 violation.
		createClusterRun(run, loc, cbv1.NamespaceSelector{Exclude: []string{"kube-*"}})

		By("the run reports Ready=False/SelectorInvalid and stays Pending")
		Eventually(func(g Gomega) {
			cb := getClusterRunG(g, run)
			g.Expect(cb.Status.Phase).To(Equal(string(status.ClusterBackupPhasePending)))
			c := status.FindCondition(cb.Status.Conditions, ConditionReady)
			g.Expect(c).NotTo(BeNil())
			g.Expect(c.Reason).To(Equal("SelectorInvalid"))
		}, initTimeout, initPoll).Should(Succeed())

		By("no child Backup is created")
		Consistently(func(g Gomega) {
			var kids cbv1.BackupList
			g.Expect(k8sClient.List(ctx, &kids, client.MatchingLabels{apiconst.LabelClusterBackup: run})).To(Succeed())
			g.Expect(kids.Items).To(BeEmpty())
		}, 2*time.Second, 500*time.Millisecond).Should(Succeed())
	})

	It("rolls up to Completed and counts successes when every child succeeds", func() {
		const loc = "cb-loc-aggok"
		const run = "cb-run-aggok"
		lbl := map[string]string{cbTestCaseLabel: "aggok"}
		createTestLocation(newTestLocation(loc, "kek-aggok", "s3-aggok", false))
		nsA, nsB := "cbns-aggok-a", "cbns-aggok-b"
		createLabelledNamespace(nsA, lbl)
		createLabelledNamespace(nsB, lbl)
		createClusterRun(run, loc, cbv1.NamespaceSelector{MatchLabels: lbl})

		waitChildrenExist(run, nsA, nsB)

		okVol := []cbv1.VolumeStatus{{Pvc: "data", Phase: status.VolumePhaseCompleted, SnapshotID: "snap-x", AddedBytes: 1024}}
		patchChildTerminal(nsA, run, status.BackupPhaseCompleted, okVol, metav1.ConditionTrue, "Completed", "done")
		patchChildTerminal(nsB, run, status.BackupPhaseCompleted, okVol, metav1.ConditionTrue, "Completed", "done")

		By("the run reaches Completed with full success counts, summed bytes, and a completion time")
		Eventually(func(g Gomega) {
			cb := getClusterRunG(g, run)
			g.Expect(cb.Status.Phase).To(Equal(string(status.ClusterBackupPhaseCompleted)))
			g.Expect(cb.Status.NamespacesMatched).To(Equal(int32(2)))
			g.Expect(cb.Status.NamespacesSucceeded).To(Equal(int32(2)))
			g.Expect(cb.Status.NamespacesFailed).To(Equal(int32(0)))
			g.Expect(cb.Status.PVCsSucceeded).To(Equal(int32(2)))
			g.Expect(cb.Status.PVCsFailed).To(Equal(int32(0)))
			g.Expect(cb.Status.AddedBytes).To(Equal(int64(2048)))
			g.Expect(cb.Status.StartTime).NotTo(BeNil())
			g.Expect(cb.Status.CompletionTime).NotTo(BeNil())
			c := status.FindCondition(cb.Status.Conditions, ConditionReady)
			g.Expect(c).NotTo(BeNil())
			g.Expect(c.Status).To(Equal(metav1.ConditionTrue))
		}, initTimeout, initPoll).Should(Succeed())
	})

	It("rolls up to PartiallyFailed with a failure record when one namespace fails", func() {
		const loc = "cb-loc-aggmix"
		const run = "cb-run-aggmix"
		lbl := map[string]string{cbTestCaseLabel: "aggmix"}
		createTestLocation(newTestLocation(loc, "kek-aggmix", "s3-aggmix", false))
		nsOK, nsBad := "cbns-aggmix-ok", "cbns-aggmix-bad"
		createLabelledNamespace(nsOK, lbl)
		createLabelledNamespace(nsBad, lbl)
		createClusterRun(run, loc, cbv1.NamespaceSelector{MatchLabels: lbl})

		waitChildrenExist(run, nsOK, nsBad)

		patchChildTerminal(nsOK, run, status.BackupPhaseCompleted,
			[]cbv1.VolumeStatus{{Pvc: "data", Phase: status.VolumePhaseCompleted, AddedBytes: 512}},
			metav1.ConditionTrue, "Completed", "done")
		patchChildTerminal(nsBad, run, status.BackupPhaseFailed,
			[]cbv1.VolumeStatus{{Pvc: "data", Phase: status.VolumePhaseFailed, Reason: "MoverFailed"}},
			metav1.ConditionFalse, "Failed", "mover job failed on data")

		By("the run reaches PartiallyFailed with split counts and one captured failure")
		Eventually(func(g Gomega) {
			cb := getClusterRunG(g, run)
			g.Expect(cb.Status.Phase).To(Equal(string(status.ClusterBackupPhasePartiallyFailed)))
			g.Expect(cb.Status.NamespacesSucceeded).To(Equal(int32(1)))
			g.Expect(cb.Status.NamespacesFailed).To(Equal(int32(1)))
			g.Expect(cb.Status.PVCsSucceeded).To(Equal(int32(1)))
			g.Expect(cb.Status.PVCsFailed).To(Equal(int32(1)))
			g.Expect(cb.Status.Failures).To(HaveLen(1))
			g.Expect(cb.Status.Failures[0].Namespace).To(Equal(nsBad))
			g.Expect(cb.Status.Failures[0].Backup).To(Equal(run))
			g.Expect(cb.Status.Failures[0].Message).To(Equal("mover job failed on data"))
			c := status.FindCondition(cb.Status.Conditions, ConditionReady)
			g.Expect(c).NotTo(BeNil())
			g.Expect(c.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(c.Reason).To(Equal("PartiallyFailed"))
		}, initTimeout, initPoll).Should(Succeed())
	})
})
