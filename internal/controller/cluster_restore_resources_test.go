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
	"testing"
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
	"github.com/CrystalBackup/CrystalBackup/internal/rexposer"
	"github.com/CrystalBackup/CrystalBackup/internal/status"
)

// The CLUSTER-scoped half of a ClusterRestore driven end to end through the reconciler. The
// applier's own behaviour is covered against a real API server elsewhere; what is at stake HERE
// is the lifecycle around it — that the Job is built with the cluster-scoped identity and argv the
// mover needs, that the cluster-WIDE write grant exists exactly as long as the Job does, and that
// the volumes do not start until the cluster-scoped objects are back (StorageClasses first).
var _ = Describe("ClusterRestore cluster-scoped half", func() {

	// clusterManifestsSnap is the run's kind=cluster-manifests snapshot as the repository returns
	// it: /cluster-manifests, NO namespace/tenant/pvc tag (restic.ClusterManifestsIdentity), so the
	// namespace-filtered volume listing can never surface it and the SECOND listing must.
	clusterManifestsSnap := func(id, run string) restic.Snapshot {
		return restic.Snapshot{
			ID:    id,
			Time:  time.Now(),
			Paths: []string{"/cluster-manifests"},
			Tags: []string{
				restic.TagBase,
				restic.Tag(restic.TagKeyKind, restic.KindClusterManifests),
				restic.Tag(restic.TagKeyRun, run),
			},
		}
	}

	// startClusterRestore seeds an initialized repo, ONE data snapshot for the run (so the run
	// resolves and prepare's len(byPVC)==0 gate passes) plus the run's cluster-manifests snapshot,
	// and creates a confirmed ClusterRestore. The caller shapes spec.Volumes / spec.ClusterResources.
	startClusterRestore := func(location, srcNS, dstNS, run, dataID, clusterID, name string, spec cbv1.ClusterRestoreSpec) *cbv1.ClusterRestore {
		GinkgoHelper()
		seedInitializedRepo(location, "kek-"+location, "s3-"+location)
		restoreLister.seed([]restic.Snapshot{
			dataSnapshot(dataID, srcNS, "data", run),
			clusterManifestsSnap(clusterID, run),
		})
		spec.Source = cbv1.ClusterRestoreSource{
			LocationRef: cbv1.LocalObjectReference{Name: location},
			Namespace:   srcNS,
			Backup:      run,
		}
		spec.Target = cbv1.ClusterRestoreTarget{Namespace: dstNS, CreateNamespace: true}
		spec.Confirmation = dstNS
		cr := &cbv1.ClusterRestore{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec:       spec,
		}
		Expect(k8sClient.Create(ctx, cr)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(context.Background(), cr) })
		return cr
	}

	clusterJobName := func(name string) string { return clusterRestoreJobName(clusterRestoreOwnerID(name)) }

	waitForClusterJob := func(name string) *batchv1.Job {
		GinkgoHelper()
		var job batchv1.Job
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx,
				client.ObjectKey{Namespace: suiteOperatorNamespace, Name: clusterJobName(name)}, &job)).To(Succeed())
		}, initTimeout, initPoll).Should(Succeed())
		return &job
	}

	envOf := func(job *batchv1.Job) map[string]string {
		out := map[string]string{}
		for _, e := range job.Spec.Template.Spec.Containers[0].Env {
			out[e.Name] = e.Value
		}
		return out
	}

	// simulateHardKill reproduces a mover the kubelet killed before it could write a result (OOM,
	// SIGKILL, eviction): the Job fails and the termination message is BLANK.
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

	It("applies cluster-scoped objects: writer grant, /cluster-manifests argv, report recorded, grant removed", func() {
		const (
			location  = "crr-happy-loc"
			srcNS     = "crr-happy-src"
			dstNS     = "crr-happy-dst"
			run       = "dr-cr-20260720-010000"
			dataID    = "1111aaaa1111aaaa1111aaaa1111aaaa1111aaaa1111aaaa1111aaaa1111aaaa"
			clusterID = "2222bbbb2222bbbb2222bbbb2222bbbb2222bbbb2222bbbb2222bbbb2222bbbb"
		)
		// resources: [] isolates the cluster-scoped half — no volume is selected, so nothing about
		// the volume path can mask it — but a data snapshot still exists so the run resolves.
		startClusterRestore(location, srcNS, dstNS, run, dataID, clusterID, "recover-cluster", cbv1.ClusterRestoreSpec{
			Mode:    cbv1.RestoreModeOverwrite,
			Volumes: []cbv1.VolumeSelectorItem{},
			ClusterResources: &cbv1.ClusterResourceRestoreSpec{
				Include: []string{"storage.k8s.io/StorageClass"},
			},
		})

		job := waitForClusterJob("recover-cluster")
		spec := job.Spec.Template.Spec

		// I6's sole exception, at the cluster plane: the ONE mover reaching the API server, with a
		// token, labelled so the NetworkPolicy grants it API-server egress.
		Expect(spec.ServiceAccountName).To(Equal(suiteManifestMoverSA))
		Expect(spec.AutomountServiceAccountToken).NotTo(BeNil())
		Expect(*spec.AutomountServiceAccountToken).To(BeTrue())
		Expect(job.Labels).To(HaveKeyWithValue(apiconst.LabelMoverRole, apiconst.MoverRoleManifest))
		Expect(job.Labels).To(HaveKeyWithValue(apiconst.LabelClusterRestore, "recover-cluster"))

		// The operation and the subtree come off the SNAPSHOT's recorded /cluster-manifests path;
		// the tree lands under ClusterManifestsRestoreDir.
		args := strings.Join(spec.Containers[0].Args, " ")
		Expect(spec.Containers[0].Args).To(ContainElement(string(mover.OpClusterManifestsRestore)))
		Expect(args).To(ContainSubstring(clusterID + ":/cluster-manifests"))
		Expect(args).To(ContainSubstring("--target " + mover.ClusterManifestsRestoreDir))

		// The restore lands under ClusterManifestsRoot, so its emptyDir must be mounted there or the
		// tree has nowhere to go under a read-only root filesystem.
		var mounted bool
		for _, m := range spec.Containers[0].VolumeMounts {
			if m.MountPath == mover.ClusterManifestsRoot {
				mounted = true
			}
		}
		Expect(mounted).To(BeTrue(), "the cluster restore needs its emptyDir at ClusterManifestsRoot")

		env := envOf(job)
		Expect(env[mover.EnvManifestsRestoreDir]).To(Equal(mover.ClusterManifestsRestoreDir))
		Expect(env[mover.EnvManifestsMode]).To(Equal(string(cbv1.RestoreModeOverwrite)))
		Expect(env).NotTo(HaveKey(mover.EnvManifestsDryRun))
		// Cluster-scoped: NO target namespace, and NO storageClassMapping (a PV is restored as
		// captured).
		Expect(env).NotTo(HaveKey(mover.EnvManifestsNamespace))
		Expect(env).NotTo(HaveKey(mover.EnvManifestsStorageClassMapping))
		decoded, err := manifests.DecodeSelection(env[mover.EnvManifestsSelection])
		Expect(err).NotTo(HaveOccurred())
		Expect(decoded.All).To(BeFalse())
		Expect(decoded.Items).To(HaveLen(1))
		Expect(decoded.Items[0].Include).To(ConsistOf("storage.k8s.io/StorageClass"))

		By("the CLUSTER-scoped write grant exists while the Job runs — a ClusterRoleBinding, not a RoleBinding")
		var crb rbacv1.ClusterRoleBinding
		Expect(k8sClient.Get(ctx, client.ObjectKey{
			Name: clusterManifestBindingName(clusterJobName("recover-cluster")),
		}, &crb)).To(Succeed())
		Expect(crb.RoleRef.Name).To(Equal(suiteClusterManifestWriterRole),
			"a cluster-scoped restore must bind the WRITER role, never the reader")
		Expect(crb.Subjects[0].Namespace).To(Equal(suiteOperatorNamespace))

		By("simulating the cluster-scoped apply succeeding")
		simulateMoverSucceeded(clusterJobName("recover-cluster"), "node-a", mover.MoverResult{
			OK: true, Operation: string(mover.OpClusterManifestsRestore),
			RestoredResources: 12, FailedResources: 0,
			ResourceEntries: []mover.ResourceEntry{{
				Group: "storage.k8s.io", Kind: "StorageClass", Name: "fast",
				Outcome: "Created",
			}},
		})

		Eventually(func(g Gomega) {
			var got cbv1.ClusterRestore
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: "recover-cluster"}, &got)).To(Succeed())
			g.Expect(got.Status.Phase).To(Equal(string(status.RestorePhaseCompleted)))
			g.Expect(got.Status.RestoredResources).To(Equal(int32(12)))
			g.Expect(got.Status.Resources).NotTo(BeNil())
			g.Expect(got.Status.Resources.FailedCount).To(BeZero())
			g.Expect(got.Status.Resources.Entries).To(HaveLen(1))
		}, initTimeout, initPoll).Should(Succeed())

		// The grant is create/update/delete on CRDs and cluster RBAC across the whole cluster — the
		// most privileged in the system. It must not outlive its Job, and the teardown runs only
		// after the terminal status write.
		By("the cluster-wide write grant does NOT outlive the Job")
		Eventually(func(g Gomega) {
			err := k8sClient.Get(ctx, client.ObjectKey{
				Name: clusterManifestBindingName(clusterJobName("recover-cluster")),
			}, &rbacv1.ClusterRoleBinding{})
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
		}, initTimeout, initPoll).Should(Succeed())
	})

	It("starts no cluster-scoped Job when the restore opts out (clusterResources omitted)", func() {
		const (
			location  = "crr-off-loc"
			srcNS     = "crr-off-src"
			dstNS     = "crr-off-dst"
			run       = "dr-cr-20260720-020000"
			dataID    = "3333cccc3333cccc3333cccc3333cccc3333cccc3333cccc3333cccc3333cccc"
			clusterID = "4444dddd4444dddd4444dddd4444dddd4444dddd4444dddd4444dddd4444dddd"
			pvName    = "pv-crr-off"
		)
		// A bound target PVC in the destination namespace makes the volume take the twin path, whose
		// exposure creates a twin PV — the earliest observable that the volumes are proceeding.
		createTenantNamespace(dstNS)
		createBoundTargetPVC(dstNS, "data", pvName)

		// clusterResources OMITTED: the opt-out (the safe default). A cluster-manifests snapshot
		// still exists in the repo and must be left entirely alone. Volumes omitted ⇒ the data PVC
		// is selected.
		startClusterRestore(location, srcNS, dstNS, run, dataID, clusterID, "recover-noopt", cbv1.ClusterRestoreSpec{
			Mode: cbv1.RestoreModeOverwrite,
		})

		By("proceeding to the volumes as today — the twin PV is exposed")
		twinPV := rexposer.TwinPVName(restoreNamePrefix(clusterRestoreOwnerID("recover-noopt"), "data"))
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: twinPV}, &corev1.PersistentVolume{})).To(Succeed())
		}, initTimeout, initPoll).Should(Succeed())

		// Consistently rather than Eventually: proving absence needs a window, not a first look.
		By("never creating a cluster-scoped restore Job")
		Consistently(func(g Gomega) {
			err := k8sClient.Get(ctx, client.ObjectKey{
				Namespace: suiteOperatorNamespace, Name: clusterJobName("recover-noopt"),
			}, &batchv1.Job{})
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
		}, "3s", initPoll).Should(Succeed())
	})

	It("holds the volumes until the cluster-scoped restore completes (StorageClasses first)", func() {
		const (
			location  = "crr-seq-loc"
			srcNS     = "crr-seq-src"
			dstNS     = "crr-seq-dst"
			run       = "dr-cr-20260720-030000"
			dataID    = "5555eeee5555eeee5555eeee5555eeee5555eeee5555eeee5555eeee5555eeee"
			clusterID = "6666ffff6666ffff6666ffff6666ffff6666ffff6666ffff6666ffff6666ffff"
			pvName    = "pv-crr-seq"
		)
		createTenantNamespace(dstNS)
		createBoundTargetPVC(dstNS, "data", pvName)

		// Opted in (cluster-scoped) AND a volume selected (Volumes omitted ⇒ the data PVC): the two
		// halves are both in flight, and the sequencing rule is that the cluster half runs first.
		startClusterRestore(location, srcNS, dstNS, run, dataID, clusterID, "recover-seq", cbv1.ClusterRestoreSpec{
			Mode: cbv1.RestoreModeOverwrite,
			ClusterResources: &cbv1.ClusterResourceRestoreSpec{
				Include: []string{"apiextensions.k8s.io/CustomResourceDefinition"},
			},
		})

		By("starting the cluster-scoped Job first")
		waitForClusterJob("recover-seq")

		// The volume must NOT expose while the cluster-scoped restore is unfinished: its PVCs may
		// reference StorageClasses the cluster half is still bringing back. Consistently: the claim
		// is that the twin PV never appears until the cluster half settles.
		twinPV := rexposer.TwinPVName(restoreNamePrefix(clusterRestoreOwnerID("recover-seq"), "data"))
		By("holding the volume exposure back while the cluster-scoped Job is unfinished")
		Consistently(func(g Gomega) {
			err := k8sClient.Get(ctx, client.ObjectKey{Name: twinPV}, &corev1.PersistentVolume{})
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
		}, "3s", initPoll).Should(Succeed())

		By("only once the cluster-scoped restore succeeds does the volume exposure begin")
		simulateMoverSucceeded(clusterJobName("recover-seq"), "node-a", mover.MoverResult{
			OK: true, Operation: string(mover.OpClusterManifestsRestore), RestoredResources: 3,
		})
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: twinPV}, &corev1.PersistentVolume{})).To(Succeed())
		}, initTimeout, initPoll).Should(Succeed())
	})

	It("treats an unreadable cluster mover result as a failure and removes the grant", func() {
		const (
			location  = "crr-kill-loc"
			srcNS     = "crr-kill-src"
			dstNS     = "crr-kill-dst"
			run       = "dr-cr-20260720-040000"
			dataID    = "7777aaaa7777aaaa7777aaaa7777aaaa7777aaaa7777aaaa7777aaaa7777aaaa"
			clusterID = "8888bbbb8888bbbb8888bbbb8888bbbb8888bbbb8888bbbb8888bbbb8888bbbb"
		)
		startClusterRestore(location, srcNS, dstNS, run, dataID, clusterID, "recover-kill", cbv1.ClusterRestoreSpec{
			Mode:    cbv1.RestoreModeOverwrite,
			Volumes: []cbv1.VolumeSelectorItem{},
			ClusterResources: &cbv1.ClusterResourceRestoreSpec{
				Include: []string{"apiextensions.k8s.io/CustomResourceDefinition"},
			},
		})

		jobName := clusterJobName("recover-kill")
		waitForClusterJob("recover-kill")
		simulateHardKill(jobName)

		// A mover killed mid-apply leaves the cluster in an UNKNOWN state: some objects applied,
		// some not. Recording "0 applied" would be a specific claim we cannot make, and success
		// would be worse.
		Eventually(func(g Gomega) {
			var got cbv1.ClusterRestore
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: "recover-kill"}, &got)).To(Succeed())
			g.Expect(got.Status.Resources).NotTo(BeNil())
			g.Expect(got.Status.Resources.FailedCount).NotTo(BeZero())
			g.Expect(got.Status.RestoredResources).To(BeZero())
			g.Expect(status.IsTerminalRestorePhase(got.Status.Phase)).To(BeTrue())
			g.Expect(got.Status.Phase).NotTo(Equal(string(status.RestorePhaseCompleted)))
		}, initTimeout, initPoll).Should(Succeed())

		By("the cluster-wide write grant is removed even on the failure path")
		Eventually(func(g Gomega) {
			err := k8sClient.Get(ctx,
				client.ObjectKey{Name: clusterManifestBindingName(jobName)}, &rbacv1.ClusterRoleBinding{})
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
		}, initTimeout, initPoll).Should(Succeed())
	})
})

// TestResolveClusterRestorePlan pins the tri-state resolution that decides the cluster-scoped half,
// resolved operator-side because a pointer's presence (opt-in) and its include list's emptiness
// (narrow vs. everything) mean different things that must not be confused.
func TestResolveClusterRestorePlan(t *testing.T) {
	clusterSnap := restic.Snapshot{
		ID:    "snap-cluster",
		Time:  time.Unix(100, 0),
		Paths: []string{"/cluster-manifests"},
		Tags: []string{
			restic.TagBase,
			restic.Tag(restic.TagKeyKind, restic.KindClusterManifests),
			restic.Tag(restic.TagKeyRun, "run-1"),
		},
	}

	t.Run("omitted clusterResources is the opt-out: no plan, no error", func(t *testing.T) {
		cr := &cbv1.ClusterRestore{Spec: cbv1.ClusterRestoreSpec{}}
		plan, ok := resolveClusterRestorePlan(cr, []restic.Snapshot{clusterSnap})
		if plan != nil || !ok {
			t.Fatalf("nil clusterResources: got (%v, %v), want (nil, true)", plan, ok)
		}
	})

	t.Run("opted in with an empty include restores everything (All)", func(t *testing.T) {
		cr := &cbv1.ClusterRestore{Spec: cbv1.ClusterRestoreSpec{
			ClusterResources: &cbv1.ClusterResourceRestoreSpec{},
		}}
		plan, ok := resolveClusterRestorePlan(cr, []restic.Snapshot{clusterSnap})
		if plan == nil || !ok {
			t.Fatalf("empty include: got (%v, %v), want a plan and true", plan, ok)
		}
		if !plan.selection.All {
			t.Errorf("empty include must restore everything (All), got %+v", plan.selection)
		}
		if len(plan.selection.Items) != 0 {
			t.Errorf("All selection must carry no items, got %+v", plan.selection.Items)
		}
		// The subtree comes off the snapshot's recorded path, not a rebuilt constant.
		if plan.snapshotPath != "/cluster-manifests" {
			t.Errorf("snapshotPath must be the snapshot's recorded path, got %q", plan.snapshotPath)
		}
		if plan.snapshotID != "snap-cluster" {
			t.Errorf("snapshotID = %q, want snap-cluster", plan.snapshotID)
		}
	})

	t.Run("opted in with an include narrows", func(t *testing.T) {
		cr := &cbv1.ClusterRestore{Spec: cbv1.ClusterRestoreSpec{
			ClusterResources: &cbv1.ClusterResourceRestoreSpec{
				Include: []string{"storage.k8s.io/StorageClass"},
				Exclude: []string{"storage.k8s.io/StorageClass/legacy-*"},
			},
		}}
		plan, ok := resolveClusterRestorePlan(cr, []restic.Snapshot{clusterSnap})
		if plan == nil || !ok {
			t.Fatalf("with include: got (%v, %v), want a plan and true", plan, ok)
		}
		if plan.selection.All {
			t.Error("a present include must NOT set All")
		}
		if len(plan.selection.Items) != 1 {
			t.Fatalf("want one selection item, got %+v", plan.selection.Items)
		}
		it := plan.selection.Items[0]
		if len(it.Include) != 1 || it.Include[0] != "storage.k8s.io/StorageClass" {
			t.Errorf("include not carried through: %+v", it.Include)
		}
		if len(it.Exclude) != 1 || it.Exclude[0] != "storage.k8s.io/StorageClass/legacy-*" {
			t.Errorf("exclude not carried through: %+v", it.Exclude)
		}
	})

	t.Run("opted in but no cluster-manifests snapshot: no plan, not found", func(t *testing.T) {
		cr := &cbv1.ClusterRestore{Spec: cbv1.ClusterRestoreSpec{
			ClusterResources: &cbv1.ClusterResourceRestoreSpec{},
		}}
		// Only a kind=data snapshot in the listing — nothing cluster-scoped to restore.
		dataOnly := restic.Snapshot{Tags: []string{restic.TagBase, restic.Tag(restic.TagKeyKind, restic.KindData)}}
		plan, ok := resolveClusterRestorePlan(cr, []restic.Snapshot{dataOnly})
		if plan != nil || ok {
			t.Fatalf("no cluster-manifests snapshot: got (%v, %v), want (nil, false)", plan, ok)
		}
	})
}
