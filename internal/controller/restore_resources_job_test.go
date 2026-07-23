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
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
	"github.com/CrystalBackup/CrystalBackup/internal/manifests"
	"github.com/CrystalBackup/CrystalBackup/internal/mover"
	"github.com/CrystalBackup/CrystalBackup/internal/restic"
	"github.com/CrystalBackup/CrystalBackup/internal/status"
)

// The resources[] half driven end to end through the reconciler. The applier's own behaviour
// is covered against a real API server elsewhere; what is at stake HERE is the lifecycle
// around it — that the Job is built with the identity and argv the mover needs, that the
// write grant exists exactly as long as the Job does, and that the Restore does not go
// terminal while the apply is still running.
var _ = Describe("Restore resources[] half", func() {

	manifestsSnapshotFixture := func(id, namespace, run string) restic.Snapshot {
		return restic.Snapshot{
			ID:    id,
			Time:  time.Now(),
			Paths: []string{"/manifests/" + namespace},
			Tags: []string{
				restic.TagBase,
				restic.Tag(restic.TagKeyKind, restic.KindManifests),
				restic.Tag(restic.TagKeyTenant, namespace),
				restic.Tag(restic.TagKeyNamespace, namespace),
				restic.Tag(restic.TagKeyRun, run),
			},
		}
	}

	// startRestore seeds a run that has ONLY a manifest snapshot, so the resources half is the
	// only thing in flight and nothing about the volume path can mask it.
	startRestore := func(ns, location, run, restoreName, snapID string, spec cbv1.RestoreSpec) *cbv1.Restore {
		GinkgoHelper()
		seedInitializedRepo(location, "kek-"+location, "s3-"+location)
		createTenantNamespace(ns)
		restoreLister.seed([]restic.Snapshot{manifestsSnapshotFixture(snapID, ns, run)})
		createProjectedBackup(ns, run, location, nil)

		spec.Source = cbv1.RestoreSource{Backup: run}
		spec.Confirmation = ns
		r := &cbv1.Restore{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: restoreName},
			Spec:       spec,
		}
		Expect(k8sClient.Create(ctx, r)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(context.Background(), r) })
		return r
	}

	resourcesJobName := func(ns, name string) string {
		return manifestsJobName(resourcesJobPrefix(restoreOwnerID(ns, name)))
	}

	waitForResourcesJob := func(jobName string) *batchv1.Job {
		GinkgoHelper()
		var job batchv1.Job
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx,
				client.ObjectKey{Namespace: suiteOperatorNamespace, Name: jobName}, &job)).To(Succeed())
		}, initTimeout, initPoll).Should(Succeed())
		return &job
	}

	// simulateHardKill reproduces a mover the kubelet killed before it could write a result
	// (OOM, SIGKILL, eviction): the Job fails and the termination message is BLANK.
	simulateHardKill := func(jobName string) {
		GinkgoHelper()
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
			g.Expect(k8sClient.Get(ctx,
				client.ObjectKey{Namespace: suiteOperatorNamespace, Name: jobName}, &job)).To(Succeed())
			job.Status.Failed = 1
			g.Expect(k8sClient.Status().Update(ctx, &job)).To(Succeed())
		}, initTimeout, initPoll).Should(Succeed())
	}

	envOf := func(job *batchv1.Job) map[string]string {
		out := map[string]string{}
		for _, e := range job.Spec.Template.Spec.Containers[0].Env {
			out[e.Name] = e.Value
		}
		return out
	}

	It("applies a namespace: writer grant, restic argv, report recorded, grant removed", func() {
		const (
			ns       = "rr-happy"
			location = "rr-happy-loc"
			run      = "dr-daily-20260719-010000"
			snapID   = "bbbb2222bbbb2222bbbb2222bbbb2222bbbb2222bbbb2222bbbb2222bbbb2222"
		)
		startRestore(ns, location, run, "recover-all", snapID, cbv1.RestoreSpec{
			Mode: cbv1.RestoreModeOverwrite,
		})

		job := waitForResourcesJob(resourcesJobName(ns, "recover-all"))
		spec := job.Spec.Template.Spec

		// I6's sole exception, asserted end to end: this is the ONE mover that reaches the API
		// server, and it needs a token to do it.
		Expect(spec.ServiceAccountName).To(Equal(suiteManifestMoverSA))
		Expect(spec.AutomountServiceAccountToken).NotTo(BeNil())
		Expect(*spec.AutomountServiceAccountToken).To(BeTrue())
		Expect(job.Labels).To(HaveKeyWithValue(apiconst.LabelMoverRole, apiconst.MoverRoleManifest))

		// The subtree comes off the SNAPSHOT's recorded path. Rebuilding it from the CR would
		// restore nothing and look exactly like a namespace that had no manifests.
		args := strings.Join(spec.Containers[0].Args, " ")
		Expect(args).To(ContainSubstring(snapID + ":/manifests/" + ns))
		Expect(args).To(ContainSubstring("--target " + mover.ManifestsRestoreDir))
		// Mode reconciles API objects, not files; the destination is a fresh emptyDir.
		Expect(spec.Containers[0].Args).NotTo(ContainElement("--delete"))

		env := envOf(job)
		Expect(env[mover.EnvManifestsNamespace]).To(Equal(ns))
		Expect(env[mover.EnvManifestsRestoreDir]).To(Equal(mover.ManifestsRestoreDir))
		Expect(env[mover.EnvManifestsMode]).To(Equal(string(cbv1.RestoreModeOverwrite)))
		Expect(env).NotTo(HaveKey(mover.EnvManifestsDryRun))
		// An omitted resources field restores everything — the tri-state resolved operator-side.
		decoded, err := manifests.DecodeSelection(env[mover.EnvManifestsSelection])
		Expect(err).NotTo(HaveOccurred())
		Expect(decoded.All).To(BeTrue())

		By("the WRITE grant exists in the target namespace while the Job runs")
		var rb rbacv1.RoleBinding
		Expect(k8sClient.Get(ctx, client.ObjectKey{
			Namespace: ns, Name: manifestBindingName(resourcesJobName(ns, "recover-all")),
		}, &rb)).To(Succeed())
		Expect(rb.RoleRef.Name).To(Equal(suiteManifestWriterRole),
			"a restore must bind the WRITER role, never the reader")
		Expect(rb.Subjects[0].Namespace).To(Equal(suiteOperatorNamespace))

		By("simulating the apply succeeding with one notable outcome")
		simulateMoverSucceeded(resourcesJobName(ns, "recover-all"), "node-a", mover.MoverResult{
			OK: true, Operation: string(mover.OpManifestsRestore),
			RestoredResources: 141, FailedResources: 0,
			ResourceEntries: []mover.ResourceEntry{{
				Group: "", Kind: "ConfigMap", Name: "app-config",
				Outcome: "Configured", Changed: []string{"data.LOG_LEVEL"},
			}},
		})

		Eventually(func(g Gomega) {
			var r cbv1.Restore
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "recover-all"}, &r)).To(Succeed())
			g.Expect(r.Status.Phase).To(Equal(string(status.RestorePhaseCompleted)))
			g.Expect(r.Status.RestoredResources).To(Equal(int32(141)))
			g.Expect(r.Status.Resources).NotTo(BeNil())
			g.Expect(r.Status.Resources.FailedCount).To(BeZero())
			g.Expect(r.Status.Resources.Entries).To(HaveLen(1))
			g.Expect(r.Status.Resources.Entries[0].Changed).To(ContainElement("data.LOG_LEVEL"))
		}, initTimeout, initPoll).Should(Succeed())

		// The grant is create/update/delete on arbitrary kinds — the largest in the system. It
		// must not outlive its Job, and the teardown runs only after the terminal status write,
		// so this also proves the teardown is not stranded behind the already-terminal
		// short-circuit at the top of Reconcile.
		By("the write grant does NOT outlive the Job")
		Eventually(func(g Gomega) {
			err := k8sClient.Get(ctx, client.ObjectKey{
				Namespace: ns, Name: manifestBindingName(resourcesJobName(ns, "recover-all")),
			}, &rbacv1.RoleBinding{})
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
		}, initTimeout, initPoll).Should(Succeed())
	})

	It("degrades to PartiallyFailed when resources failed to apply", func() {
		const (
			ns       = "rr-partial"
			location = "rr-partial-loc"
			run      = "dr-daily-20260719-020000"
			snapID   = "cccc3333cccc3333cccc3333cccc3333cccc3333cccc3333cccc3333cccc3333"
		)
		startRestore(ns, location, run, "recover-partial", snapID, cbv1.RestoreSpec{
			Mode: cbv1.RestoreModeOverwrite,
		})

		jobName := resourcesJobName(ns, "recover-partial")
		waitForResourcesJob(jobName)
		simulateMoverSucceeded(jobName, "node-a", mover.MoverResult{
			OK: true, Operation: string(mover.OpManifestsRestore),
			RestoredResources: 138, FailedResources: 3,
			ResourceEntries: []mover.ResourceEntry{{
				Group: "", Kind: "Service", Name: "web",
				Outcome: "Failed", Reason: "nodePort 30080 already allocated",
			}},
		})

		// A user who asked for a namespace back and got most of it has not had a Completed
		// restore. Reporting Completed over a non-zero failedCount is the one lie this status
		// must never tell.
		Eventually(func(g Gomega) {
			var r cbv1.Restore
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "recover-partial"}, &r)).To(Succeed())
			g.Expect(r.Status.Phase).To(Equal(string(status.RestorePhasePartiallyFailed)))
			g.Expect(r.Status.Resources.FailedCount).To(Equal(int32(3)))
			g.Expect(r.Status.Resources.Entries[0].Reason).To(ContainSubstring("nodePort"))
		}, initTimeout, initPoll).Should(Succeed())
	})

	It("carries the narrowed selection and the dry-run flag to the mover", func() {
		const (
			ns       = "rr-narrow"
			location = "rr-narrow-loc"
			run      = "dr-daily-20260719-030000"
			snapID   = "dddd4444dddd4444dddd4444dddd4444dddd4444dddd4444dddd4444dddd4444"
		)
		startRestore(ns, location, run, "recover-narrow", snapID, cbv1.RestoreSpec{
			Mode:   cbv1.RestoreModeRecreate,
			DryRun: true,
			Resources: []cbv1.ResourceSelectorItem{{
				Include: []string{"apps/Deployment"},
				Exclude: []string{"apps/Deployment/legacy-*"},
			}},
		})

		job := waitForResourcesJob(resourcesJobName(ns, "recover-narrow"))
		env := envOf(job)

		Expect(env[mover.EnvManifestsMode]).To(Equal(string(cbv1.RestoreModeRecreate)))
		Expect(env[mover.EnvManifestsDryRun]).To(Equal("true"))

		// This env value is the one whose corruption WIDENS a narrow restore instead of failing
		// it — in Recreate mode, over a live namespace.
		decoded, err := manifests.DecodeSelection(env[mover.EnvManifestsSelection])
		Expect(err).NotTo(HaveOccurred())
		Expect(decoded.All).To(BeFalse())
		Expect(decoded.Items).To(HaveLen(1))
		Expect(decoded.Items[0].Include).To(ConsistOf("apps/Deployment"))
		Expect(decoded.Items[0].Exclude).To(ConsistOf("apps/Deployment/legacy-*"))
	})

	It("starts no manifest Job when the restore asks for none", func() {
		const (
			ns       = "rr-none"
			location = "rr-none-loc"
			run      = "dr-daily-20260719-040000"
			snapID   = "eeee5555eeee5555eeee5555eeee5555eeee5555eeee5555eeee5555eeee5555"
		)
		// `resources: []` is a PRESENT field listing nothing — the CLI's --data-only. A snapshot
		// exists and must still be left alone.
		startRestore(ns, location, run, "data-only", snapID, cbv1.RestoreSpec{
			Mode:      cbv1.RestoreModeOverwrite,
			Resources: []cbv1.ResourceSelectorItem{},
		})

		// Consistently rather than Eventually: proving absence needs a window, not a first look.
		Consistently(func(g Gomega) {
			err := k8sClient.Get(ctx, client.ObjectKey{
				Namespace: suiteOperatorNamespace, Name: resourcesJobName(ns, "data-only"),
			}, &batchv1.Job{})
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
		}, "3s", initPoll).Should(Succeed())
	})

	It("treats an unreadable mover result as an unknown namespace state, not an empty success", func() {
		const (
			ns       = "rr-blank"
			location = "rr-blank-loc"
			run      = "dr-daily-20260719-050000"
			snapID   = "ffff6666ffff6666ffff6666ffff6666ffff6666ffff6666ffff6666ffff6666"
		)
		startRestore(ns, location, run, "recover-blank", snapID, cbv1.RestoreSpec{
			Mode: cbv1.RestoreModeOverwrite,
		})

		jobName := resourcesJobName(ns, "recover-blank")
		waitForResourcesJob(jobName)
		simulateHardKill(jobName)

		// A mover killed mid-apply (OOM, SIGKILL) leaves the namespace in an UNKNOWN state:
		// some objects applied, some not. Recording "0 applied" would be a specific claim we
		// cannot make, and recording success would be worse.
		Eventually(func(g Gomega) {
			var r cbv1.Restore
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "recover-blank"}, &r)).To(Succeed())
			g.Expect(r.Status.Resources).NotTo(BeNil())
			g.Expect(r.Status.Resources.FailedCount).NotTo(BeZero())
			g.Expect(r.Status.RestoredResources).To(BeZero())
			g.Expect(status.IsTerminalRestorePhase(r.Status.Phase)).To(BeTrue())
			g.Expect(r.Status.Phase).NotTo(Equal(string(status.RestorePhaseCompleted)))
		}, initTimeout, initPoll).Should(Succeed())

		By("the write grant is removed even on the failure path")
		Eventually(func(g Gomega) {
			err := k8sClient.Get(ctx,
				client.ObjectKey{Namespace: ns, Name: manifestBindingName(jobName)}, &rbacv1.RoleBinding{})
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
		}, initTimeout, initPoll).Should(Succeed())
	})
})
