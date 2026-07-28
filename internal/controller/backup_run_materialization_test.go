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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
)

// Run-configuration materialization (adr/0017 §5). The cascade used to PULL every run knob from
// the parent ClusterBackup at each reconcile; a Backup now carries its own spec.run, copied down
// once at creation, with the pull kept only as the compatibility path for objects created before
// the field existed.
//
// What these specs pin is not the copying itself but the three properties it buys, each of which
// silently regresses if someone "simplifies" resolveRun back to a single source:
//   - the materialized copy makes a Backup survive its parent (run records are GC'd, children are
//     not, and the link is a label so nothing cascades);
//   - the copy WINS over a live parent, so editing a finished run's parent no longer rewrites what
//     that run appears to have done;
//   - with neither, the Backup gates instead of inventing a run.
var _ = Describe("Backup run materialization", func() {

	// createRunBackup creates a Backup carrying a MATERIALIZED spec.run. parentLabel selects
	// whether it also links to a parent ClusterBackup — the two axes this file varies.
	createRunBackup := func(namespace, name, location, parentLabel string, run *cbv1.BackupRunSpec) *cbv1.Backup {
		GinkgoHelper()
		labels := map[string]string{apiconst.LabelOrigin: apiconst.OriginCluster}
		if parentLabel != "" {
			labels[apiconst.LabelClusterBackup] = parentLabel
		}
		b := &cbv1.Backup{
			ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, Labels: labels},
			Spec: cbv1.BackupSpec{
				LocationRef: cbv1.LocationReference{Kind: "ClusterBackupLocation", Name: location},
				Run:         run,
			},
		}
		Expect(k8sClient.Create(ctx, b)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(context.Background(), b) })
		return b
	}

	It("materializes the parent's run configuration into every child at fan-out", func() {
		const (
			location = "bkrun-fanout"
			ns       = "bkrun-fanout-ns"
			run      = "bkrun-fanout-run"
			label    = "bkrun-fanout"
		)
		seedInitializedRepo(location, "kek-bkrun-fanout", "s3-bkrun-fanout")
		createLabelledNamespace(ns, map[string]string{label: "yes"})

		// A run whose knobs are all NON-default, so a child that merely inherited zero values
		// would be indistinguishable from one that inherited nothing.
		off := false
		cb := &cbv1.ClusterBackup{
			ObjectMeta: metav1.ObjectMeta{Name: run},
			Spec: cbv1.ClusterBackupSpec{
				ClusterBackupRunSpec: cbv1.ClusterBackupRunSpec{
					LocationRef:      cbv1.LocalObjectReference{Name: location},
					Namespaces:       cbv1.NamespaceSelector{MatchLabels: map[string]string{label: "yes"}},
					ClusterResources: cbv1.ClusterResourceCaptureSpec{Enabled: &off},
					BackupRunSpec: cbv1.BackupRunSpec{
						PVCSelector:      cbv1.PVCSelector{Exclude: []string{"*"}},
						IncludeManifests: &off,
						BackoffLimit:     7,
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, cb)).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(context.Background(), cb)
			_ = k8sClient.DeleteAllOf(context.Background(), &cbv1.Backup{},
				client.MatchingLabels{apiconst.LabelClusterBackup: run})
		})

		By("the fanned-out child carries the run configuration in its own spec")
		Eventually(func(g Gomega) {
			child := getBackupG(g, ns, run)
			g.Expect(child.Spec.Run).NotTo(BeNil(), "the child must carry a materialized spec.run")
			g.Expect(child.Spec.Run.PVCSelector.Exclude).To(Equal([]string{"*"}))
			g.Expect(child.Spec.Run.IncludeManifests).To(Equal(&off))
			g.Expect(child.Spec.Run.BackoffLimit).To(Equal(int32(7)))
		}, initTimeout, initPoll).Should(Succeed())

		By("the copy is independent: editing the parent afterwards does not rewrite the child")
		Eventually(func(g Gomega) {
			var parent cbv1.ClusterBackup
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: run}, &parent)).To(Succeed())
			parent.Spec.BackoffLimit = 99
			g.Expect(k8sClient.Update(ctx, &parent)).To(Succeed())
		}, initTimeout, initPoll).Should(Succeed())
		Consistently(func(g Gomega) {
			child := getBackupG(g, ns, run)
			g.Expect(child.Spec.Run).NotTo(BeNil())
			g.Expect(child.Spec.Run.BackoffLimit).To(Equal(int32(7)))
		}, consistentlyWindow, initPoll).Should(Succeed())
	})

	It("executes a Backup whose parent is gone, because the run travelled with it", func() {
		const (
			location = "bkrun-orphan"
			ns       = "bkrun-orphan-ns"
			run      = "bkrun-orphan-run"
			pvcName  = "orphan-vol"
		)
		seedInitializedRepo(location, "kek-bkrun-orphan", "s3-bkrun-orphan")
		createTenantNamespace(ns)
		createSourcePVC(ns, pvcName, "ceph-block")

		// No parent ClusterBackup exists at all — the exact state a child reaches once run
		// history GC collects its parent. Before materialization this gated on NoParent forever.
		off := false
		createRunBackup(ns, run, location, "", &cbv1.BackupRunSpec{IncludeManifests: &off})

		By("the reconciler drives it from spec.run alone, up to a real mover Job")
		jobName := waitForMoverJob(ns, run, pvcName)
		Expect(jobName).NotTo(BeEmpty())
		Eventually(func(g Gomega) {
			b := getBackupG(g, ns, run)
			vol := volumeByPVC(b, pvcName)
			g.Expect(vol).NotTo(BeNil(), "the PVC must have been enumerated from spec.run's selector")
		}, initTimeout, initPoll).Should(Succeed())
	})

	It("prefers spec.run over a live parent, and never re-reads the parent", func() {
		const (
			location = "bkrun-wins"
			ns       = "bkrun-wins-ns"
			run      = "bkrun-wins-run"
			pvcName  = "wins-vol"
		)
		seedInitializedRepo(location, "kek-bkrun-wins", "s3-bkrun-wins")
		createTenantNamespace(ns)
		createSourcePVC(ns, pvcName, "ceph-block")

		// The parent excludes EVERY PVC; the child's materialized run excludes none. If the pull
		// were still authoritative, the volume set would come out empty.
		createVolumeOnlyParent(run, location, cbv1.PVCSelector{Exclude: []string{"*"}})
		off := false
		createRunBackup(ns, run, location, run, &cbv1.BackupRunSpec{IncludeManifests: &off})

		By("the PVC the parent excluded is backed up, because spec.run decided")
		Eventually(func(g Gomega) {
			b := getBackupG(g, ns, run)
			vol := volumeByPVC(b, pvcName)
			g.Expect(vol).NotTo(BeNil(), "spec.run must win over the parent's selector")
		}, initTimeout, initPoll).Should(Succeed())
	})

	It("gates with NoRunSpec when there is neither a materialized run nor a resolvable parent", func() {
		const (
			location = "bkrun-none"
			ns       = "bkrun-none-ns"
			run      = "bkrun-none-run"
		)
		seedInitializedRepo(location, "kek-bkrun-none", "s3-bkrun-none")
		createTenantNamespace(ns)

		// No spec.run, and the parent link label points at a ClusterBackup that does not exist.
		createRunBackup(ns, run, location, "bkrun-none-missing-parent", nil)

		Eventually(func(g Gomega) {
			b := getBackupG(g, ns, run)
			cond := apimeta.FindStatusCondition(b.Status.Conditions, ConditionReady)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Reason).To(Equal("NoRunSpec"))
		}, initTimeout, initPoll).Should(Succeed())
	})
})
