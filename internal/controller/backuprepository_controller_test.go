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
	"time"

	. "github.com/onsi/ginkgo/v2" //nolint:revive,staticcheck
	. "github.com/onsi/gomega"    //nolint:revive,staticcheck

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
	"github.com/CrystalBackup/CrystalBackup/internal/keys"
	"github.com/CrystalBackup/CrystalBackup/internal/mover"
	"github.com/CrystalBackup/CrystalBackup/internal/restic"
)

// The init flow is driven by the exclusive-queue Handle poll plus a ~10s in-flight requeue, so a
// completion the test SIMULATES (by patching the init Job's status) can take a couple of requeue
// cycles to be observed. A generous bound keeps the suite off the flake line under CI load.
const (
	initTimeout = 90 * time.Second
	initPoll    = 250 * time.Millisecond
)

// getRepository fetches the BackupRepository named name (equal to its owning location's name)
// with a HARD assertion — use only OUTSIDE Eventually/Consistently, where the object is known to
// exist.
func getRepository(name string) cbv1.BackupRepository {
	GinkgoHelper()
	var br cbv1.BackupRepository
	Expect(k8sClient.Get(ctx, client.ObjectKey{Name: name}, &br)).To(Succeed())
	return br
}

// getRepositoryG is the getRepository variant for use INSIDE an Eventually/Consistently block:
// it asserts through the polling Gomega g, so a not-yet-created repository is a retry, not a hard
// failure that aborts the spec.
func getRepositoryG(g Gomega, name string) cbv1.BackupRepository {
	var br cbv1.BackupRepository
	g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: name}, &br)).To(Succeed())
	return br
}

// waitForInitJob waits for the reconciler to have created the per-repository init Job (named
// <repo>-init in the operator namespace) and returns it.
func waitForInitJobCreated(repoName string) batchv1.Job {
	GinkgoHelper()
	var job batchv1.Job
	Eventually(func(g Gomega) {
		g.Expect(k8sClient.Get(ctx,
			client.ObjectKey{Namespace: suiteOperatorNamespace, Name: initResourceName(repoName)}, &job)).To(Succeed())
	}, initTimeout, initPoll).Should(Succeed())
	return job
}

// patchInitJobStatus re-reads the init Job and applies mutate to its status, retrying on the
// resourceVersion conflicts a concurrently-reconciling controller can cause. envtest has no
// kubelet, so patching status is the ONLY way an init Job ever reaches a terminal state here.
func patchInitJobStatus(repoName string, mutate func(*batchv1.Job)) {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		var job batchv1.Job
		g.Expect(k8sClient.Get(ctx,
			client.ObjectKey{Namespace: suiteOperatorNamespace, Name: initResourceName(repoName)}, &job)).To(Succeed())
		mutate(&job)
		g.Expect(k8sClient.Status().Update(ctx, &job)).To(Succeed())
	}, initTimeout, initPoll).Should(Succeed())
}

// nudgeRepository forces a reconcile of the BackupRepository by writing an annotation, retrying
// on conflict. Used to prove repeated reconciles do not enqueue a second init.
func nudgeRepository(repoName string, i int) {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		var br cbv1.BackupRepository
		g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: repoName}, &br)).To(Succeed())
		if br.Annotations == nil {
			br.Annotations = map[string]string{}
		}
		br.Annotations["test.crystalbackup.io/nudge"] = fmt.Sprintf("%d", i)
		g.Expect(k8sClient.Update(ctx, &br)).To(Succeed())
	}, initTimeout, initPoll).Should(Succeed())
}

// countInitJobs counts init Jobs named <repo>-init in the operator namespace (0 or 1, since the
// name is deterministic and the API enforces name uniqueness — the point is to prove the
// reconciler never TRIES to stand up a duplicate).
func countInitJobs(g Gomega, repoName string) int {
	var jobs batchv1.JobList
	g.Expect(k8sClient.List(ctx, &jobs, client.InNamespace(suiteOperatorNamespace))).To(Succeed())
	n := 0
	for i := range jobs.Items {
		if jobs.Items[i].Name == initResourceName(repoName) {
			n++
		}
	}
	return n
}

// registerRepoCleanup best-effort removes the operator-namespace artefacts a repository's init
// leaves behind (the init Job, its creds Secret, and the sticky DEK Secret) after a spec, so a
// later spec reusing operator-namespace state starts clean.
func registerRepoCleanup(locationName string) {
	DeferCleanup(func() {
		bg := context.Background()
		_ = k8sClient.Delete(bg, &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
			Name: initResourceName(locationName), Namespace: suiteOperatorNamespace}})
		_ = k8sClient.Delete(bg, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
			Name: initResourceName(locationName), Namespace: suiteOperatorNamespace}})
		_ = k8sClient.Delete(bg, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
			Name: keys.DEKSecretName(locationName), Namespace: suiteOperatorNamespace}})
	})
}

var _ = Describe("BackupRepositoryReconciler", func() {

	It("populates identity, ensures the platform DEK, and initializes once the init Job succeeds", func() {
		const name = "br-init-happy"
		createKEKSecret("kek-init-happy", generateAgeIdentity())
		createS3CredsSecret("s3-init-happy")
		registerRepoCleanup(name)
		// A valid location: the ClusterBackupLocation controller provisions the owned
		// BackupRepository, which this controller then drives to Initialized.
		createTestLocation(newTestLocation(name, "kek-init-happy", "s3-init-happy", false))

		wantURL := restic.RepoURL("https://s3.example.test", "test-bucket", "", "envtest-cluster")

		By("the reconciler populates status identity, ensures the DEK Secret, and creates the init Job")
		Eventually(func(g Gomega) {
			br := getRepositoryG(g, name)
			g.Expect(br.Status.Location.Kind).To(Equal("ClusterBackupLocation"))
			g.Expect(br.Status.Location.Name).To(Equal(name))
			g.Expect(br.Status.Scope).To(Equal("Cluster"))
			g.Expect(br.Status.RepositoryURL).To(Equal(wantURL))
			g.Expect(br.Status.KeySlots).To(Equal([]string{"platform"}))
			g.Expect(apimeta.IsStatusConditionTrue(br.Status.Conditions, ConditionInitialized)).To(BeFalse())

			// The sticky platform DEK Secret exists and holds the wrapped DEK.
			var dek corev1.Secret
			g.Expect(k8sClient.Get(ctx,
				client.ObjectKey{Namespace: suiteOperatorNamespace, Name: keys.DEKSecretName(name)}, &dek)).To(Succeed())
			g.Expect(dek.Data).To(HaveKey(keys.DEKSecretKey))

			// The init Job exists.
			g.Expect(k8sClient.Get(ctx,
				client.ObjectKey{Namespace: suiteOperatorNamespace, Name: initResourceName(name)}, &batchv1.Job{})).To(Succeed())
		}, initTimeout, initPoll).Should(Succeed())

		By("the init Job is an OpInit mover Job for this repository")
		job := waitForInitJobCreated(name)
		Expect(job.Spec.Template.Spec.Containers).To(HaveLen(1))
		c := job.Spec.Template.Spec.Containers[0]
		Expect(c.Command).To(Equal([]string{mover.MoverBinaryPath}))
		Expect(c.Args[:3]).To(Equal([]string{"--operation", string(mover.OpInit), "--"}))
		Expect(c.Args).To(ContainElement("init"))
		// RESTIC_REPOSITORY carries the repository URL; the per-Job Secret is the <repo>-init Secret.
		var repoEnv string
		for _, e := range c.Env {
			if e.Name == "RESTIC_REPOSITORY" {
				repoEnv = e.Value
			}
		}
		Expect(repoEnv).To(Equal(wantURL))
		// The Job is controller-owned by the BackupRepository (so the Owns(Job) watch maps back).
		owner := metav1.GetControllerOf(&job)
		Expect(owner).NotTo(BeNil())
		Expect(owner.Kind).To(Equal("BackupRepository"))
		Expect(owner.Name).To(Equal(name))

		By("repeated reconciles never enqueue a second init Job (the in-flight Handle map dedupes)")
		for i := 0; i < 5; i++ {
			nudgeRepository(name, i)
		}
		Consistently(func(g Gomega) {
			g.Expect(countInitJobs(g, name)).To(Equal(1))
			g.Expect(getRepositoryG(g, name).Status.Initialized).To(BeFalse())
		}, 3*time.Second, 500*time.Millisecond).Should(Succeed())

		By("simulating the init Job succeeding (Succeeded=1)")
		patchInitJobStatus(name, func(j *batchv1.Job) { j.Status.Succeeded = 1 })

		By("the repository reaches Initialized=true with ConditionInitialized=True and KeySlots=[platform]")
		Eventually(func(g Gomega) {
			br := getRepositoryG(g, name)
			g.Expect(br.Status.Initialized).To(BeTrue())
			g.Expect(apimeta.IsStatusConditionTrue(br.Status.Conditions, ConditionInitialized)).To(BeTrue())
			g.Expect(apimeta.IsStatusConditionTrue(br.Status.Conditions, ConditionReady)).To(BeTrue())
			g.Expect(br.Status.KeySlots).To(Equal([]string{"platform"}))
		}, initTimeout, initPoll).Should(Succeed())

		By("the BackupRepository carries the repository finalizer")
		Expect(getRepository(name).Finalizers).To(ContainElement(apiconst.FinalizerRepository))
	})

	It("reports InitFailed and stays uninitialized when the init Job fails terminally", func() {
		const name = "br-init-fail"
		createKEKSecret("kek-init-fail", generateAgeIdentity())
		createS3CredsSecret("s3-init-fail")
		registerRepoCleanup(name)
		createTestLocation(newTestLocation(name, "kek-init-fail", "s3-init-fail", false))

		By("waiting for the init Job to be created")
		waitForInitJobCreated(name)

		By("simulating the init Job failing terminally (Failed past the backoffLimit)")
		patchInitJobStatus(name, func(j *batchv1.Job) { j.Status.Failed = initJobBackoffLimit + 1 })

		By("the repository reports ConditionInitialized=False reason InitFailed and never initializes")
		Eventually(func(g Gomega) {
			br := getRepositoryG(g, name)
			cond := apimeta.FindStatusCondition(br.Status.Conditions, ConditionInitialized)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(cond.Reason).To(Equal("InitFailed"))
		}, initTimeout, initPoll).Should(Succeed())

		Consistently(func(g Gomega) {
			g.Expect(getRepositoryG(g, name).Status.Initialized).To(BeFalse())
		}, 3*time.Second, 500*time.Millisecond).Should(Succeed())
	})

	It("finalizes on delete without erasing the sticky platform DEK Secret", func() {
		const name = "br-delete"
		createKEKSecret("kek-br-delete", generateAgeIdentity())
		createS3CredsSecret("s3-br-delete")
		registerRepoCleanup(name)
		loc := newTestLocation(name, "kek-br-delete", "s3-br-delete", false)
		createTestLocation(loc)

		By("waiting for the DEK Secret to be minted and the finalizer to land")
		Eventually(func(g Gomega) {
			g.Expect(getRepositoryG(g, name).Finalizers).To(ContainElement(apiconst.FinalizerRepository))
			var dek corev1.Secret
			g.Expect(k8sClient.Get(ctx,
				client.ObjectKey{Namespace: suiteOperatorNamespace, Name: keys.DEKSecretName(name)}, &dek)).To(Succeed())
		}, initTimeout, initPoll).Should(Succeed())

		// Delete the owning location FIRST and wait for it to be gone: the ClusterBackupLocation
		// controller Owns(BackupRepository) and would immediately re-create the repository if we
		// deleted it while its owner still existed (envtest has no garbage collector to cascade
		// the delete, so this must be sequenced explicitly).
		By("deleting the owning location and waiting for it to be gone")
		Expect(k8sClient.Delete(ctx, loc)).To(Succeed())
		Eventually(func(g Gomega) {
			err := k8sClient.Get(ctx, client.ObjectKey{Name: name}, &cbv1.ClusterBackupLocation{})
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
		}, initTimeout, initPoll).Should(Succeed())

		By("deleting the BackupRepository")
		br := getRepository(name)
		Expect(k8sClient.Delete(ctx, &br)).To(Succeed())

		By("the BackupRepository is fully removed (finalizer cleared, not resurrected)")
		Eventually(func(g Gomega) {
			err := k8sClient.Get(ctx, client.ObjectKey{Name: name}, &cbv1.BackupRepository{})
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
		}, initTimeout, initPoll).Should(Succeed())

		By("the platform DEK Secret is retained (adr/0009: delete never erases key material)")
		var dek corev1.Secret
		Expect(k8sClient.Get(ctx,
			client.ObjectKey{Namespace: suiteOperatorNamespace, Name: keys.DEKSecretName(name)}, &dek)).To(Succeed())
	})
})
