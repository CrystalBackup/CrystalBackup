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
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
	"github.com/CrystalBackup/CrystalBackup/internal/mover"
	"github.com/CrystalBackup/CrystalBackup/internal/restic"
	"github.com/CrystalBackup/CrystalBackup/internal/status"
)

var _ = Describe("ClusterBackup cluster-scoped capture", func() {

	// startClusterRun creates an initialized repo, a matched namespace, and a ClusterBackup whose
	// NAMESPACED manifest capture is off — so the only manifest work in flight is the CLUSTER
	// capture this file is about, and a matched namespace with no PVCs lets the child complete
	// immediately.
	startClusterRun := func(location, ns, run string, clusterResources *cbv1.ClusterResourceCaptureSpec) {
		GinkgoHelper()
		seedInitializedRepo(location, "kek-"+location, "s3-"+location)
		lbl := map[string]string{"cbcap": run}
		createLabelledNamespace(ns, lbl)

		off := false
		cb := &cbv1.ClusterBackup{
			ObjectMeta: metav1.ObjectMeta{Name: run},
			Spec: cbv1.ClusterBackupSpec{
				ClusterBackupRunSpec: cbv1.ClusterBackupRunSpec{
					LocationRef:      cbv1.LocalObjectReference{Name: location},
					Namespaces:       cbv1.NamespaceSelector{MatchLabels: lbl},
					IncludeManifests: &off,
				},
			},
		}
		if clusterResources != nil {
			cb.Spec.ClusterResources = *clusterResources
		}
		Expect(k8sClient.Create(ctx, cb)).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(context.Background(), cb)
			_ = k8sClient.DeleteAllOf(context.Background(), &cbv1.Backup{},
				client.MatchingLabels{apiconst.LabelClusterBackup: run})
		})
	}

	waitClusterJob := func(run string) *batchv1.Job {
		GinkgoHelper()
		var job batchv1.Job
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{
				Namespace: suiteOperatorNamespace, Name: clusterManifestsJobName(run),
			}, &job)).To(Succeed())
		}, initTimeout, initPoll).Should(Succeed())
		return &job
	}

	It("captures cluster resources: cluster-scoped grant, /cluster-manifests path, snapshot recorded, grant removed", func() {
		const (
			location = "cbc-happy"
			ns       = "cbc-happy-ns"
			run      = "cbc-happy-run"
		)
		startClusterRun(location, ns, run, nil) // default: capture enabled

		job := waitClusterJob(run)
		spec := job.Spec.Template.Spec

		// I6's sole exception, at the cluster plane: the ONE mover reaching the API server, with
		// a token, labelled so the NetworkPolicy grants it API-server egress.
		Expect(spec.ServiceAccountName).To(Equal(suiteManifestMoverSA))
		Expect(spec.AutomountServiceAccountToken).NotTo(BeNil())
		Expect(*spec.AutomountServiceAccountToken).To(BeTrue())
		Expect(job.Labels).To(HaveKeyWithValue(apiconst.LabelMoverRole, apiconst.MoverRoleManifest))
		Expect(job.OwnerReferences).To(BeEmpty())

		// The dump target and the restic path are one string; the emptyDir must be mounted at
		// /cluster-manifests or the dump writes to a read-only filesystem.
		Expect(strings.Join(spec.Containers[0].Args, " ")).To(ContainSubstring("/cluster-manifests"))
		var mounted bool
		for _, m := range spec.Containers[0].VolumeMounts {
			if m.MountPath == mover.ClusterManifestsRoot {
				mounted = true
			}
		}
		Expect(mounted).To(BeTrue(), "the cluster dump needs its emptyDir at ClusterManifestsRoot")

		// The grant is a CLUSTER-scoped ClusterRoleBinding, not a namespaced RoleBinding: a
		// cluster-scoped read has no namespace to be confined to.
		By("the transient ClusterRoleBinding exists while the Job runs")
		var crb rbacv1.ClusterRoleBinding
		Expect(k8sClient.Get(ctx, client.ObjectKey{
			Name: clusterManifestBindingName(clusterManifestsJobName(run)),
		}, &crb)).To(Succeed())
		Expect(crb.RoleRef.Name).To(Equal(suiteClusterManifestReaderRole))
		Expect(crb.Subjects[0].Namespace).To(Equal(suiteOperatorNamespace))

		By("simulating the capture succeeding")
		simulateMoverSucceeded(clusterManifestsJobName(run), "node-a", mover.MoverResult{
			OK: true, Operation: string(mover.OpClusterManifestsBackup),
			SnapshotID: "snap-cluster-1", ResourceCount: 47,
		})

		By("the snapshot and count land in status, the condition is True, and the run Completes")
		Eventually(func(g Gomega) {
			cb := getClusterRunG(g, run)
			g.Expect(cb.Status.ClusterManifests).NotTo(BeNil())
			g.Expect(cb.Status.ClusterManifests.SnapshotID).To(Equal("snap-cluster-1"))
			g.Expect(cb.Status.ClusterResourcesCaptured).To(Equal(int32(47)))
			g.Expect(apimeta.IsStatusConditionTrue(cb.Status.Conditions, ConditionClusterManifestsComplete)).To(BeTrue())
			g.Expect(cb.Status.Phase).To(Equal(string(status.ClusterBackupPhaseCompleted)))
		}, initTimeout, initPoll).Should(Succeed())

		By("the cluster-scoped grant does NOT outlive the Job — it is a read of the whole cluster")
		Eventually(func(g Gomega) {
			err := k8sClient.Get(ctx, client.ObjectKey{
				Name: clusterManifestBindingName(clusterManifestsJobName(run)),
			}, &rbacv1.ClusterRoleBinding{})
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
		}, initTimeout, initPoll).Should(Succeed())
	})

	It("holds the run non-terminal until the cluster capture settles", func() {
		const (
			location = "cbc-gate"
			ns       = "cbc-gate-ns"
			run      = "cbc-gate-run"
		)
		startClusterRun(location, ns, run, nil)
		waitClusterJob(run)

		// The child namespace has no PVCs and its manifest capture is off, so it completes almost
		// at once. The run must NOT report Completed while the cluster capture is still in flight
		// — that is the already-terminal-guard trap the namespaced half taught us, at the run
		// level. Consistently: the claim is that it never goes terminal.
		Consistently(func(g Gomega) {
			cb := getClusterRunG(g, run)
			g.Expect(cb.Status.Phase).NotTo(Equal(string(status.ClusterBackupPhaseCompleted)))
			g.Expect(cb.Status.CompletionTime).To(BeNil())
		}, "3s", initPoll).Should(Succeed())

		By("only once the capture is simulated does the run complete")
		simulateMoverSucceeded(clusterManifestsJobName(run), "node-a", mover.MoverResult{
			OK: true, Operation: string(mover.OpClusterManifestsBackup),
			SnapshotID: "snap-cluster-gate", ResourceCount: 9,
		})
		Eventually(func(g Gomega) {
			cb := getClusterRunG(g, run)
			g.Expect(cb.Status.Phase).To(Equal(string(status.ClusterBackupPhaseCompleted)))
		}, initTimeout, initPoll).Should(Succeed())
	})

	It("does not capture cluster resources when the run opts out", func() {
		const (
			location = "cbc-off"
			ns       = "cbc-off-ns"
			run      = "cbc-off-run"
		)
		off := false
		startClusterRun(location, ns, run, &cbv1.ClusterResourceCaptureSpec{Enabled: &off})

		// Consistently, not Eventually: proving absence needs a window. And with the capture off,
		// the run must reach Completed on its children alone.
		Consistently(func(g Gomega) {
			err := k8sClient.Get(ctx, client.ObjectKey{
				Namespace: suiteOperatorNamespace, Name: clusterManifestsJobName(run),
			}, &batchv1.Job{})
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
		}, "3s", initPoll).Should(Succeed())
		Eventually(func(g Gomega) {
			cb := getClusterRunG(g, run)
			g.Expect(cb.Status.Phase).To(Equal(string(status.ClusterBackupPhaseCompleted)))
			g.Expect(cb.Status.ClusterManifests).To(BeNil())
		}, initTimeout, initPoll).Should(Succeed())
	})
})

// TestClusterManifestsInvisibleToNamespacedRestore pins the tenancy invariant (I1) that makes a
// cluster-manifests snapshot safe: it carries NO namespace, tenant or pvc tag, so a namespaced
// Restore — whose repository listing is filtered server-side by namespace=<its own> and which
// resolves only kind=data and kind=manifests — can never see it. A cluster-scoped snapshot
// leaking into a self-service restore would be a cross-tenant disclosure.
func TestClusterManifestsInvisibleToNamespacedRestore(t *testing.T) {
	id := restic.ClusterManifestsIdentity("cluster-x", "sched", "run-1")
	for _, forbidden := range []string{
		restic.TagKeyNamespace, restic.TagKeyTenant, restic.TagKeyPVC,
	} {
		if _, ok := restic.TagValue(id.Tags, forbidden); ok {
			t.Errorf("ClusterManifestsIdentity carries a %q tag; it must carry none, or a namespaced filter could match it", forbidden)
		}
	}

	// The snapshot as the repository would return it.
	clusterSnap := restic.Snapshot{
		ID: "snap-cluster", Time: time.Unix(100, 0), Paths: []string{"/cluster-manifests"}, Tags: id.Tags,
	}

	// The two resolvers a namespaced Restore uses must both ignore it: it is neither data nor a
	// namespace's manifests.
	if got := dataSnapshotsByPVC([]restic.Snapshot{clusterSnap}); len(got) != 0 {
		t.Errorf("dataSnapshotsByPVC picked up a cluster-manifests snapshot: %v", got)
	}
	if _, ok := manifestsSnapshot([]restic.Snapshot{clusterSnap}); ok {
		t.Error("manifestsSnapshot picked up a cluster-manifests snapshot; a namespaced restore must never resolve one")
	}
}
