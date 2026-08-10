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
	"errors"

	. "github.com/onsi/ginkgo/v2" //nolint:revive,staticcheck
	. "github.com/onsi/gomega"    //nolint:revive,staticcheck

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/hooks"
	"github.com/CrystalBackup/CrystalBackup/internal/mover"
)

// createHookedParent creates a volumes-only run carrying consistency hooks, scoped to sel.
func createHookedParent(name, location string, sel cbv1.PVCSelector, hooksSpec cbv1.HooksSpec) {
	GinkgoHelper()
	off := false
	cb := &cbv1.ClusterBackup{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: cbv1.ClusterBackupSpec{
			ClusterBackupRunSpec: cbv1.ClusterBackupRunSpec{
				LocationRef: cbv1.LocalObjectReference{Name: location},
				BackupRunSpec: cbv1.BackupRunSpec{
					PVCSelector:      sel,
					IncludeManifests: &off,
					Hooks:            hooksSpec,
				},
			},
		},
	}
	Expect(k8sClient.Create(ctx, cb)).To(Succeed())
	DeferCleanup(func() { _ = k8sClient.Delete(context.Background(), cb) })
}

// createMountingPod creates a Running pod that mounts pvc, so it becomes a hook candidate. envtest
// has no kubelet, so the Running phase is written by hand.
func createMountingPod(namespace, name, pvc string, annotations map[string]string) {
	GinkgoHelper()
	createLabelledMountingPod(namespace, name, pvc, nil, annotations)
}

// createLabelledMountingPod is the same, with labels — which a spec needs whenever it declares
// hooks with a podSelector, i.e. whenever DIFFERENT pods must run DIFFERENT commands. That is the
// shape of every real multi-workload namespace, and of the aborted-chain specs in
// hooks_abort_release_test.go, where one pod's quiesce must succeed and another's must fail.
func createLabelledMountingPod(namespace, name, pvc string, labels, annotations map[string]string) {
	GinkgoHelper()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace, Name: name, Labels: labels, Annotations: annotations,
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "db", Image: "busybox"}},
			Volumes: []corev1.Volume{{
				Name: "data",
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: pvc},
				},
			}},
		},
	}
	Expect(k8sClient.Create(ctx, pod)).To(Succeed())
	pod.Status.Phase = corev1.PodRunning
	Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
	DeferCleanup(func() { _ = k8sClient.Delete(context.Background(), pod) })
}

var _ = Describe("Consistency hooks (R16)", func() {
	BeforeEach(func() { hookExecutor.reset() })

	It("walks Pending → SnapshottingHooks → Snapshotting → Completed, quiescing before the snapshot and releasing after it", func() {
		const (
			location = "hk-happy"
			ns       = "hk-happy-ns"
			run      = "hk-happy-run"
			pvcName  = "data-vol"
		)
		seedInitializedRepo(location, "kek-hk-happy", "s3-hk-happy")
		createTenantNamespace(ns)
		createSourcePVC(ns, pvcName, "ceph-block")
		createMountingPod(ns, "db-0", pvcName, nil)
		// A pod in the same namespace holding NONE of the backed-up data. The run's selector
		// deliberately picks only data-vol, so this pod holds nothing being captured and must never
		// be exec'd into — the rule that makes hooks safe to run at all.
		createSourcePVC(ns, "unrelated-vol", "ceph-block")
		createMountingPod(ns, "other-0", "unrelated-vol", nil)

		createHookedParent(run, location, cbv1.PVCSelector{Include: []string{pvcName}}, cbv1.HooksSpec{
			Pre:  []cbv1.Hook{{Command: []string{"psql", "-c", "CHECKPOINT"}}},
			Post: []cbv1.Hook{{Command: []string{"psql", "-c", "SELECT pg_backup_stop()"}}},
		})
		createChildBackup(ns, run, location)

		By("the pre hook runs and the Backup reports the SnapshottingHooks phase")
		Eventually(func(g Gomega) {
			b := getBackupG(g, ns, run)
			g.Expect(b.Status.Hooks).NotTo(BeEmpty())
			g.Expect(b.Status.Hooks[0].Phase).To(Equal(string(hooks.PhasePre)))
			g.Expect(b.Status.Hooks[0].Pod).To(Equal("db-0"))
			g.Expect(b.Status.Hooks[0].Result).To(Equal(cbv1.HookSucceeded))
		}, initTimeout, initPoll).Should(Succeed())

		By("only the pod mounting the backed-up PVC was exec'd into")
		for _, c := range hookExecutor.recorded() {
			Expect(c.Pod.Name).To(Equal("db-0"),
				"a pod holding none of the backed-up data must never be exec'd into")
			Expect(c.Pod.Namespace).To(Equal(ns))
		}

		By("the snapshot then proceeds and the mover runs")
		jobName := waitForMoverJob(ns, run, pvcName)
		simulateMoverSucceeded(jobName, "node-a", mover.MoverResult{
			OK: true, Operation: string(mover.OpBackup), SnapshotID: "snap-hooked", SizeBytes: 1, AddedBytes: 1,
		})

		By("the post hook releases and the Backup Completes")
		Eventually(func(g Gomega) {
			b := getBackupG(g, ns, run)
			g.Expect(b.Status.Phase).To(Equal("Completed"))
			var post int
			for _, h := range b.Status.Hooks {
				if h.Phase == string(hooks.PhasePost) {
					post++
					g.Expect(h.Result).To(Equal(cbv1.HookSucceeded))
				}
			}
			g.Expect(post).To(Equal(1), "the release must have run exactly once")
		}, initTimeout, initPoll).Should(Succeed())
	})

	It("aborts the backup without snapshotting when a pre hook fails with onError=Fail", func() {
		const (
			location = "hk-fail"
			ns       = "hk-fail-ns"
			run      = "hk-fail-run"
			pvcName  = "data-vol"
		)
		seedInitializedRepo(location, "kek-hk-fail", "s3-hk-fail")
		createTenantNamespace(ns)
		createSourcePVC(ns, pvcName, "ceph-block")
		createMountingPod(ns, "db-0", pvcName, nil)

		hookExecutor.failPod = map[string]error{"db-0": errors.New("command terminated with exit code 1")}

		createHookedParent(run, location, cbv1.PVCSelector{}, cbv1.HooksSpec{
			Pre: []cbv1.Hook{{Command: []string{"false"}, OnError: cbv1.HookErrorPolicyFail}},
		})
		createChildBackup(ns, run, location)

		By("the Backup fails on the quiesce, and no snapshot is taken")
		Eventually(func(g Gomega) {
			b := getBackupG(g, ns, run)
			g.Expect(b.Status.Phase).To(Equal("Failed"))
			g.Expect(b.Status.Hooks).NotTo(BeEmpty())
			g.Expect(b.Status.Hooks[0].Result).To(Equal(cbv1.HookFailed))
			// The whole point: a snapshot taken after a failed quiesce would LOOK
			// application-consistent without being so, and that is only discovered at restore time.
			for _, v := range b.Status.Volumes {
				g.Expect(string(v.Phase)).To(Or(Equal("Pending"), Equal("")),
					"no volume may leave Pending once the quiesce failed")
			}
		}, initTimeout, initPoll).Should(Succeed())
	})

	It("still releases when no snapshot was taken at all — the unfreeze is unconditional", func() {
		const (
			location = "hk-unfreeze"
			ns       = "hk-unfreeze-ns"
			run      = "hk-unfreeze-run"
			pvcName  = "data-vol"
		)
		seedInitializedRepo(location, "kek-hk-unfreeze", "s3-hk-unfreeze")
		createTenantNamespace(ns)
		// Storage the CSI driver cannot snapshot: the volume goes straight to Skipped, so the
		// backup produces NOTHING for it — and the application was still quiesced by the pre hook.
		// Conditioning the release on a successful snapshot would strand exactly this case.
		createSourcePVC(ns, pvcName, stubUnsupportedStorageClass)
		createMountingPod(ns, "db-0", pvcName, nil)

		createHookedParent(run, location, cbv1.PVCSelector{}, cbv1.HooksSpec{
			Pre:  []cbv1.Hook{{Command: []string{"freeze"}}},
			Post: []cbv1.Hook{{Command: []string{"thaw"}}},
		})
		createChildBackup(ns, run, location)

		By("the release still runs: an unsnapshotted volume leaves the application just as quiesced")
		Eventually(func(g Gomega) {
			b := getBackupG(g, ns, run)
			var post int
			for _, h := range b.Status.Hooks {
				if h.Phase == string(hooks.PhasePost) {
					post++
				}
			}
			g.Expect(post).To(BeNumerically(">=", 1),
				"the unfreeze must not be conditional on the backup succeeding")
		}, initTimeout, initPoll).Should(Succeed())
	})
})
