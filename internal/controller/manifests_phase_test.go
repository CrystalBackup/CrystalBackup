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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
	"github.com/CrystalBackup/CrystalBackup/internal/mover"
)

var _ = Describe("BackupReconciler manifest capture", func() {

	manifestJobName := func(ns, run string) string {
		return manifestsJobPrefix(ns, run) + "-mover"
	}

	waitForManifestJob := func(ns, run string) *batchv1.Job {
		GinkgoHelper()
		var job batchv1.Job
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx,
				client.ObjectKey{Namespace: suiteOperatorNamespace, Name: manifestJobName(ns, run)}, &job)).To(Succeed())
		}, initTimeout, initPoll).Should(Succeed())
		return &job
	}

	It("captures manifests: transient grant, API-reading Job, snapshot recorded, grant removed", func() {
		const (
			location = "mf-happy"
			ns       = "mf-happy-ns"
			run      = "mf-happy-run"
		)
		seedInitializedRepo(location, "kek-mf-happy", "s3-mf-happy")
		createTenantNamespace(ns)
		createParentClusterBackup(run, location, cbv1.PVCSelector{})
		createChildBackup(ns, run, location)

		By("a manifest mover Job appears, carrying the identity that reaches the API server")
		job := waitForManifestJob(ns, run)
		spec := job.Spec.Template.Spec

		// I6's sole exception, asserted end to end rather than only at the BuildJob unit level.
		Expect(spec.ServiceAccountName).To(Equal(suiteManifestMoverSA))
		Expect(spec.AutomountServiceAccountToken).NotTo(BeNil())
		Expect(*spec.AutomountServiceAccountToken).To(BeTrue())

		// The label the NetworkPolicy selects on to grant API-server egress to THIS pod and no
		// other mover. Without it §7's "manifest movers only" clause selects nothing.
		Expect(job.Labels).To(HaveKeyWithValue(apiconst.LabelMoverRole, apiconst.MoverRoleManifest))
		Expect(job.OwnerReferences).To(BeEmpty(), "a mover Job must not be owned across namespaces")

		// The dump target and the restic path are one string. If these diverge the snapshot is
		// filed where no retention group or restore looks, and nothing fails loudly.
		Expect(spec.Containers[0].Args).To(ContainElement("/manifests/" + ns))
		var mounted bool
		for _, m := range spec.Containers[0].VolumeMounts {
			if m.MountPath == mover.ManifestsRoot {
				mounted = true
			}
		}
		Expect(mounted).To(BeTrue(), "the dump needs a writable volume under a read-only root filesystem")

		By("the transient grant exists in the TENANT namespace while the Job runs")
		var rb rbacv1.RoleBinding
		Expect(k8sClient.Get(ctx,
			client.ObjectKey{Namespace: ns, Name: manifestBindingName(manifestJobName(ns, run))}, &rb)).To(Succeed())
		Expect(rb.RoleRef.Name).To(Equal(suiteManifestReaderRole))
		Expect(rb.Subjects[0].Namespace).To(Equal(suiteOperatorNamespace))

		By("simulating the manifest mover succeeding")
		simulateMoverSucceeded(manifestJobName(ns, run), "node-a", mover.MoverResult{
			OK: true, Operation: string(mover.OpManifestsBackup),
			SnapshotID: "snap-manifests-1", ResourceCount: 141,
		})

		By("the snapshot and resource count land in status, and ManifestsComplete is True")
		Eventually(func(g Gomega) {
			b := getBackupG(g, ns, run)
			g.Expect(b.Status.Manifests).NotTo(BeNil())
			g.Expect(b.Status.Manifests.SnapshotID).To(Equal("snap-manifests-1"))
			g.Expect(b.Status.Manifests.ResourceCount).To(Equal(int32(141)))
			g.Expect(apimeta.IsStatusConditionTrue(b.Status.Conditions, ConditionManifestsComplete)).To(BeTrue())
		}, initTimeout, initPoll).Should(Succeed())

		By("the grant does NOT outlive the Job — it is the largest one in the system")
		Eventually(func(g Gomega) {
			err := k8sClient.Get(ctx,
				client.ObjectKey{Namespace: ns, Name: manifestBindingName(manifestJobName(ns, run))}, &rbacv1.RoleBinding{})
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
		}, initTimeout, initPoll).Should(Succeed())
	})

	It("holds the Backup non-terminal until BOTH halves settle, then tears the grant down", func() {
		const (
			location = "mf-both"
			ns       = "mf-both-ns"
			run      = "mf-both-run"
			pvcName  = "mf-both-vol"
		)
		seedInitializedRepo(location, "kek-mf-both", "s3-mf-both")
		createTenantNamespace(ns)
		createSourcePVC(ns, pvcName, "ceph-block")
		createParentClusterBackup(run, location, cbv1.PVCSelector{})
		createChildBackup(ns, run, location)

		By("finishing the VOLUME half first, leaving the manifest capture in flight")
		volJob := waitForMoverJob(ns, run, pvcName)
		simulateMoverSucceeded(volJob, "node-a", mover.MoverResult{
			OK: true, Operation: string(mover.OpBackup), SnapshotID: "snap-vol", SizeBytes: 10, AddedBytes: 5,
		})

		// The bug this pins: the volume roll-up alone would report Completed here, and the
		// already-terminal short-circuit at the top of Reconcile would then stop the reconciles
		// that read the manifest result — stranding a snapshot in the repository that no Backup
		// points at. Consistently, not Eventually: the claim is that it NEVER goes terminal.
		By("the Backup does NOT report Completed while its manifests are still running")
		Consistently(func(g Gomega) {
			b := getBackupG(g, ns, run)
			g.Expect(b.Status.Phase).NotTo(Equal("Completed"))
			g.Expect(b.Status.BackupTime).To(BeNil(), "backupTime stamps a finished backup")
		}, "3s", initPoll).Should(Succeed())

		By("finishing the MANIFEST half")
		mfJob := manifestJobName(ns, run)
		waitForManifestJob(ns, run)
		simulateMoverSucceeded(mfJob, "node-a", mover.MoverResult{
			OK: true, Operation: string(mover.OpManifestsBackup),
			SnapshotID: "snap-mf-both", ResourceCount: 12,
		})

		By("only now does the Backup complete, carrying both results")
		Eventually(func(g Gomega) {
			b := getBackupG(g, ns, run)
			g.Expect(b.Status.Phase).To(Equal("Completed"))
			g.Expect(b.Status.BackupTime).NotTo(BeNil())
			g.Expect(volumeByPVC(b, pvcName).SnapshotID).To(Equal("snap-vol"))
			g.Expect(b.Status.Manifests).NotTo(BeNil())
			g.Expect(b.Status.Manifests.SnapshotID).To(Equal("snap-mf-both"))
		}, initTimeout, initPoll).Should(Succeed())

		// Teardown runs only after the terminal status write persists. Asserting it here — on the
		// path where the Backup goes terminal in the SAME pass that records the manifest result —
		// is what proves the teardown is not stranded behind the already-terminal short-circuit.
		By("the transient grant is gone even though the Backup went terminal in that same pass")
		Eventually(func(g Gomega) {
			err := k8sClient.Get(ctx,
				client.ObjectKey{Namespace: ns, Name: manifestBindingName(mfJob)}, &rbacv1.RoleBinding{})
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
		}, initTimeout, initPoll).Should(Succeed())
	})

	It("records a partial capture as ManifestsComplete=False without failing the backup", func() {
		const (
			location = "mf-partial"
			ns       = "mf-partial-ns"
			run      = "mf-partial-run"
		)
		seedInitializedRepo(location, "kek-mf-partial", "s3-mf-partial")
		createTenantNamespace(ns)
		createParentClusterBackup(run, location, cbv1.PVCSelector{})
		createChildBackup(ns, run, location)

		waitForManifestJob(ns, run)
		simulateMoverSucceeded(manifestJobName(ns, run), "node-a", mover.MoverResult{
			OK: true, Operation: string(mover.OpManifestsBackup),
			SnapshotID: "snap-partial", ResourceCount: 90, IncompleteManifests: true,
		})

		// A namespace whose manifests are partial is not a failed backup — the data is there and
		// most kinds captured — but it is not the complete recovery the user will assume either.
		// Only a condition can say "succeeded, with something you need to know".
		Eventually(func(g Gomega) {
			b := getBackupG(g, ns, run)
			g.Expect(b.Status.Manifests).NotTo(BeNil())
			g.Expect(b.Status.Manifests.SnapshotID).To(Equal("snap-partial"))
			c := apimeta.FindStatusCondition(b.Status.Conditions, ConditionManifestsComplete)
			g.Expect(c).NotTo(BeNil())
			g.Expect(string(c.Status)).To(Equal("False"))
			g.Expect(c.Reason).To(Equal(reasonManifestsPartial))
		}, initTimeout, initPoll).Should(Succeed())
	})

	It("does not capture manifests when the run opts out", func() {
		const (
			location = "mf-off"
			ns       = "mf-off-ns"
			run      = "mf-off-run"
		)
		seedInitializedRepo(location, "kek-mf-off", "s3-mf-off")
		createTenantNamespace(ns)

		off := false
		cb := &cbv1.ClusterBackup{}
		createParentClusterBackup(run, location, cbv1.PVCSelector{})
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: run}, cb)).To(Succeed())
			cb.Spec.IncludeManifests = &off
			g.Expect(k8sClient.Update(ctx, cb)).To(Succeed())
		}, initTimeout, initPoll).Should(Succeed())
		createChildBackup(ns, run, location)

		// Consistently rather than Eventually: proving absence needs a window, not a first look.
		Consistently(func(g Gomega) {
			err := k8sClient.Get(ctx,
				client.ObjectKey{Namespace: suiteOperatorNamespace, Name: manifestJobName(ns, run)}, &batchv1.Job{})
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
		}, "3s", initPoll).Should(Succeed())
	})

	It("treats an unreadable mover result as a failure, not an empty success", func() {
		const (
			location = "mf-blank"
			ns       = "mf-blank-ns"
			run      = "mf-blank-run"
		)
		seedInitializedRepo(location, "kek-mf-blank", "s3-mf-blank")
		createTenantNamespace(ns)
		createParentClusterBackup(run, location, cbv1.PVCSelector{})
		createChildBackup(ns, run, location)

		jobName := manifestJobName(ns, run)
		waitForManifestJob(ns, run)

		// A hard-killed mover (OOM, SIGKILL) leaves a BLANK termination message. Recording that
		// as a successful capture with zero resources would be the worst possible outcome: a
		// snapshot nobody can vouch for, that looks like one.
		pod := &corev1.Pod{}
		pod.Name = jobName + "-pod"
		pod.Namespace = suiteOperatorNamespace
		pod.Labels = map[string]string{batchv1.JobNameLabel: jobName}
		pod.Spec = corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers:    []corev1.Container{{Name: "mover", Image: suiteMoverImage}},
		}
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		pod.Status.Phase = corev1.PodFailed
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name:  "mover",
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 137, Message: ""}},
		}}
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
		Eventually(func(g Gomega) {
			var job batchv1.Job
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: suiteOperatorNamespace, Name: jobName}, &job)).To(Succeed())
			job.Status.Failed = 1
			g.Expect(k8sClient.Status().Update(ctx, &job)).To(Succeed())
		}, initTimeout, initPoll).Should(Succeed())

		Eventually(func(g Gomega) {
			b := getBackupG(g, ns, run)
			g.Expect(b.Status.Manifests).To(BeNil(), "no snapshot may be recorded for a capture nobody can vouch for")
			c := apimeta.FindStatusCondition(b.Status.Conditions, ConditionManifestsComplete)
			g.Expect(c).NotTo(BeNil())
			g.Expect(c.Reason).To(Equal(reasonManifestsFailed))
		}, initTimeout, initPoll).Should(Succeed())

		By("the grant is removed even on the failure path")
		Eventually(func(g Gomega) {
			err := k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: manifestBindingName(jobName)}, &rbacv1.RoleBinding{})
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
		}, initTimeout, initPoll).Should(Succeed())
	})
})
