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
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2" //nolint:revive,staticcheck
	. "github.com/onsi/gomega"    //nolint:revive,staticcheck

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
	"github.com/CrystalBackup/CrystalBackup/internal/mover"
	"github.com/CrystalBackup/CrystalBackup/internal/restic"
	"github.com/CrystalBackup/CrystalBackup/internal/rexposer"
	"github.com/CrystalBackup/CrystalBackup/internal/status"
)

// ---------------------------------------------------------------------------
// The mediated-lister stub. It APPLIES the filter tags itself with restic's
// AND semantics, so what a spec seeds as the full repository and what the
// controller receives are related exactly the way a real filtered listing
// relates to the bucket: the controller can never see past the filter. Every
// call's filter tags are recorded so the server-side derivation (I1) is
// assertable.
// ---------------------------------------------------------------------------

type stubFilteredLister struct {
	mu    sync.Mutex
	snaps []restic.Snapshot
	calls [][]string
}

var _ FilteredSnapshotLister = (*stubFilteredLister)(nil)

func (s *stubFilteredLister) ListFiltered(_ context.Context, _ *cbv1.BackupRepository, _ string, filterTags ...string) ([]restic.Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, append([]string{}, filterTags...))
	var out []restic.Snapshot
	for _, sn := range s.snaps {
		matches := true
		for _, want := range filterTags {
			found := false
			for _, have := range sn.Tags {
				if have == want {
					found = true
					break
				}
			}
			if !found {
				matches = false
				break
			}
		}
		if matches {
			out = append(out, sn)
		}
	}
	return out, nil
}

func (s *stubFilteredLister) seed(snaps []restic.Snapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snaps = snaps
	s.calls = nil
}

func (s *stubFilteredLister) recordedCalls() [][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([][]string, len(s.calls))
	copy(out, s.calls)
	return out
}

// dataSnapshot builds one kind=data snapshot fixture with the standard identity tags.
func dataSnapshot(id, namespace, pvc, run string, extraTags ...string) restic.Snapshot {
	tags := make([]string, 0, 6+len(extraTags))
	tags = append(tags,
		restic.TagBase,
		restic.Tag(restic.TagKeyKind, restic.KindData),
		restic.Tag(restic.TagKeyTenant, namespace),
		restic.Tag(restic.TagKeyNamespace, namespace),
		restic.Tag(restic.TagKeyPVC, pvc),
		restic.Tag(restic.TagKeyRun, run),
	)
	return restic.Snapshot{
		ID:   id,
		Time: time.Now(),
		Host: "test-cluster",
		Tags: append(tags, extraTags...),
	}
}

// createProjectedBackup creates a cluster-origin DISCOVERY PROJECTION Backup — the realistic
// restorable shape: the projection annotation keeps the Backup controller entirely away from
// it (it never executes), and status carries the restorable volumes.
func createProjectedBackup(namespace, name, location string, volumes []cbv1.VolumeStatus) {
	GinkgoHelper()
	b := &cbv1.Backup{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      name,
			Labels: map[string]string{
				apiconst.LabelClusterBackup: name,
				apiconst.LabelOrigin:        apiconst.OriginCluster,
			},
			Annotations: map[string]string{apiconst.AnnotationProjected: apiconst.AnnotationProjectedValue},
		},
		Spec: cbv1.BackupSpec{
			LocationRef: cbv1.LocationReference{Kind: "ClusterBackupLocation", Name: location},
		},
	}
	Expect(k8sClient.Create(ctx, b)).To(Succeed())
	DeferCleanup(func() { _ = k8sClient.Delete(context.Background(), b) })

	now := metav1.Now()
	b.Status.Phase = string(status.BackupPhaseCompleted)
	b.Status.BackupTime = &now
	b.Status.Volumes = volumes
	Expect(k8sClient.Status().Update(ctx, b)).To(Succeed())
}

// createBoundTargetPVC creates a Bound PVC + its PV in a tenant namespace — the twin-path
// target shape. envtest has no binder, so the pair is created pre-bound with status patched.
func createBoundTargetPVC(namespace, name, pvName string) {
	GinkgoHelper()
	fsMode := corev1.PersistentVolumeFilesystem
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: pvName},
		Spec: corev1.PersistentVolumeSpec{
			Capacity:    corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{Driver: "test.csi", VolumeHandle: "vol-" + pvName},
			},
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimDelete,
			StorageClassName:              "std",
			VolumeMode:                    &fsMode,
			ClaimRef:                      &corev1.ObjectReference{Namespace: namespace, Name: name},
		},
	}
	Expect(k8sClient.Create(ctx, pv)).To(Succeed())
	DeferCleanup(func() {
		_ = k8sClient.Delete(context.Background(), pv)
		// envtest runs no PV controller: strip the protection finalizer so the object goes.
		patch := client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"finalizers":null}}`))
		_ = k8sClient.Patch(context.Background(), pv, patch)
	})
	pv.Status.Phase = corev1.VolumeBound
	Expect(k8sClient.Status().Update(ctx, pv)).To(Succeed())

	sc := "std"
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: &sc,
			VolumeName:       pvName,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
			},
		},
	}
	Expect(k8sClient.Create(ctx, pvc)).To(Succeed())
	DeferCleanup(func() {
		_ = k8sClient.Delete(context.Background(), pvc)
		patch := client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"finalizers":null}}`))
		_ = k8sClient.Patch(context.Background(), pvc, patch)
	})
	pvc.Status.Phase = corev1.ClaimBound
	Expect(k8sClient.Status().Update(ctx, pvc)).To(Succeed())
}

// bindStagingPVC plays the binder for a pre-bound staging claim (the twin path): flip it Bound.
func bindStagingPVC(name string) {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		var pvc corev1.PersistentVolumeClaim
		g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: suiteOperatorNamespace, Name: name}, &pvc)).To(Succeed())
		pvc.Status.Phase = corev1.ClaimBound
		g.Expect(k8sClient.Status().Update(ctx, &pvc)).To(Succeed())
	}, initTimeout, initPoll).Should(Succeed())
}

// restoreJobArgs fetches a restore mover Job's restic argv.
func restoreJobArgs(g Gomega, jobName string) []string {
	var job batchv1.Job
	g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: suiteOperatorNamespace, Name: jobName}, &job)).To(Succeed())
	return job.Spec.Template.Spec.Containers[0].Args
}

var _ = Describe("Restore controller", func() {
	It("walks AwaitingConfirmation, mediates snapshot resolution server-side, and completes a twin restore", func() {
		const (
			nsName   = "restore-happy"
			location = "restore-happy-loc"
			run      = "dr-daily-20260718-010000"
			realID   = "aaaa1111aaaa1111aaaa1111aaaa1111aaaa1111aaaa1111aaaa1111aaaa1111"
			forgedID = "ffff9999ffff9999ffff9999ffff9999ffff9999ffff9999ffff9999ffff9999"
		)
		seedInitializedRepo(location, "restore-happy-kek", "restore-happy-s3")
		createTenantNamespace(nsName)
		createBoundTargetPVC(nsName, "data", "pv-restore-happy")

		// The repository truth: tenant "restore-happy" has ONE data snapshot under this run.
		// A sibling namespace's snapshot exists too — the filter must make it unreachable.
		restoreLister.seed([]restic.Snapshot{
			dataSnapshot(realID, nsName, "data", run),
			dataSnapshot(forgedID, "restore-other", "data", run),
		})

		// The projection carries a FORGED snapshot id (tamper simulation): the controller must
		// use the mediated listing's id, never this one.
		createProjectedBackup(nsName, run, location, []cbv1.VolumeStatus{
			{Pvc: "data", Phase: status.VolumePhaseCompleted, SnapshotID: forgedID},
		})

		By("parking without a confirmation (R23)")
		restoreObj := &cbv1.Restore{
			ObjectMeta: metav1.ObjectMeta{Namespace: nsName, Name: "recover-data"},
			Spec: cbv1.RestoreSpec{
				Source: cbv1.RestoreSource{Backup: run},
				Mode:   cbv1.RestoreModeOverwrite,
			},
		}
		Expect(k8sClient.Create(ctx, restoreObj)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(context.Background(), restoreObj) })

		Eventually(func(g Gomega) {
			var r cbv1.Restore
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: nsName, Name: "recover-data"}, &r)).To(Succeed())
			g.Expect(r.Status.Phase).To(Equal(string(status.RestorePhaseAwaitingConfirmation)))
		}, initTimeout, initPoll).Should(Succeed())

		By("proceeding once the confirmation names the namespace")
		Eventually(func(g Gomega) {
			var r cbv1.Restore
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: nsName, Name: "recover-data"}, &r)).To(Succeed())
			r.Spec.Confirmation = nsName
			g.Expect(k8sClient.Update(ctx, &r)).To(Succeed())
		}, initTimeout, initPoll).Should(Succeed())

		By("exposing the bound target via a twin PV and pre-bound staging claim")
		prefix := restoreNamePrefix(restoreOwnerID(nsName, "recover-data"), "data")
		stagingName := rexposer.StagingPVCName(prefix)
		Eventually(func(g Gomega) {
			var twin corev1.PersistentVolume
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: rexposer.TwinPVName(prefix)}, &twin)).To(Succeed())
			g.Expect(twin.Spec.PersistentVolumeReclaimPolicy).To(Equal(corev1.PersistentVolumeReclaimRetain))
			g.Expect(twin.Spec.CSI.VolumeHandle).To(Equal("vol-pv-restore-happy"))
		}, initTimeout, initPoll).Should(Succeed())
		bindStagingPVC(stagingName)

		By("creating the restore mover with the MEDIATED snapshot id, not the projection's")
		jobName := restoreJobName(prefix)
		Eventually(func(g Gomega) {
			args := restoreJobArgs(g, jobName)
			g.Expect(args).To(ContainElement("restore"))
			g.Expect(strings.Join(args, " ")).To(ContainSubstring(realID + ":/data/" + nsName + "/data"))
			g.Expect(strings.Join(args, " ")).NotTo(ContainSubstring(forgedID))
			// Overwrite mode: --overwrite always, and NO --delete.
			g.Expect(args).To(ContainElement("--overwrite"))
			g.Expect(args).NotTo(ContainElement("--delete"))
		}, initTimeout, initPoll).Should(Succeed())

		By("having derived the filter from metadata.namespace alone (I1)")
		calls := restoreLister.recordedCalls()
		Expect(calls).NotTo(BeEmpty())
		Expect(calls[0]).To(ConsistOf(
			restic.Tag(restic.TagKeyNamespace, nsName),
			restic.Tag(restic.TagKeyRun, run),
		))

		By("completing once the mover succeeds (twin: no handover needed)")
		simulateMoverSucceeded(jobName, "node-1", mover.MoverResult{
			OK: true, Operation: string(mover.OpRestore), RestoredBytes: 2048,
		})
		Eventually(func(g Gomega) {
			var r cbv1.Restore
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: nsName, Name: "recover-data"}, &r)).To(Succeed())
			g.Expect(r.Status.Phase).To(Equal(string(status.RestorePhaseCompleted)))
			g.Expect(r.Status.RestoredVolumes).To(Equal(int32(1)))
			g.Expect(r.Status.RestoredBytes).To(Equal(int64(2048)))
		}, initTimeout, initPoll).Should(Succeed())

		By("tearing the operator-side residue down after the terminal write")
		Eventually(func(g Gomega) {
			err := k8sClient.Get(ctx, client.ObjectKey{Namespace: suiteOperatorNamespace, Name: jobName}, &batchv1.Job{})
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "restore mover Job should be deleted")
			var staging corev1.PersistentVolumeClaim
			serr := k8sClient.Get(ctx, client.ObjectKey{Namespace: suiteOperatorNamespace, Name: stagingName}, &staging)
			// envtest never clears the pvc-protection finalizer, so "deleted" may surface as
			// a deletion timestamp rather than NotFound.
			g.Expect(apierrors.IsNotFound(serr) || staging.DeletionTimestamp != nil).To(BeTrue(),
				"staging claim should be deleted or terminating")
		}, initTimeout, initPoll).Should(Succeed())
	})

	It("fails closed when a selected PVC has no snapshot under the namespace filter (R14 negative)", func() {
		const (
			nsName   = "restore-neg"
			location = "restore-neg-loc"
			run      = "dr-neg-20260718-020000"
			otherID  = "bbbb2222bbbb2222bbbb2222bbbb2222bbbb2222bbbb2222bbbb2222bbbb2222"
		)
		seedInitializedRepo(location, "restore-neg-kek", "restore-neg-s3")
		createTenantNamespace(nsName)

		// The repository holds this run's data ONLY for another namespace: the mediated
		// filter must resolve nothing for ours, and the restore must gate — never borrow
		// the sibling's snapshot.
		restoreLister.seed([]restic.Snapshot{
			dataSnapshot(otherID, "restore-neg-other", "data", run),
		})
		createProjectedBackup(nsName, run, location, []cbv1.VolumeStatus{
			{Pvc: "data", Phase: status.VolumePhaseCompleted, SnapshotID: otherID},
		})

		restoreObj := &cbv1.Restore{
			ObjectMeta: metav1.ObjectMeta{Namespace: nsName, Name: "recover-neg"},
			Spec: cbv1.RestoreSpec{
				Source:       cbv1.RestoreSource{Backup: run},
				Mode:         cbv1.RestoreModeOverwrite,
				Confirmation: nsName,
			},
		}
		Expect(k8sClient.Create(ctx, restoreObj)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(context.Background(), restoreObj) })

		Eventually(func(g Gomega) {
			var r cbv1.Restore
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: nsName, Name: "recover-neg"}, &r)).To(Succeed())
			cond := status.FindCondition(r.Status.Conditions, ConditionReady)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Reason).To(Equal("SnapshotNotFound"))
			g.Expect(r.Status.Phase).To(Equal(string(status.RestorePhasePending)))
		}, initTimeout, initPoll).Should(Succeed())

		// No mover Job may exist for this restore — nothing was restorable.
		prefix := restoreNamePrefix(restoreOwnerID(nsName, "recover-neg"), "data")
		err := k8sClient.Get(ctx, client.ObjectKey{Namespace: suiteOperatorNamespace, Name: restoreJobName(prefix)}, &batchv1.Job{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
	})
})

var _ = Describe("ClusterRestore controller", func() {
	It("recreates a gone namespace from the repo coordinate, sizing PVCs from PVC-meta tags", func() {
		const (
			location = "crestore-loc"
			srcNS    = "crestore-gone"
			dstNS    = "crestore-target"
			run      = "dr-cr-20260718-030000"
			snapID   = "cccc3333cccc3333cccc3333cccc3333cccc3333cccc3333cccc3333cccc3333"
		)
		seedInitializedRepo(location, "crestore-kek", "crestore-s3")

		// The source namespace does NOT exist; the repository alone carries the run, with
		// PVC-meta tags recording the original claim's shape.
		restoreLister.seed([]restic.Snapshot{
			dataSnapshot(snapID, srcNS, "data", run,
				restic.PVCMetaTags(2*1024*1024*1024, "fast", []string{"ReadWriteOnce"})...),
		})

		cr := &cbv1.ClusterRestore{
			ObjectMeta: metav1.ObjectMeta{Name: "recover-gone"},
			Spec: cbv1.ClusterRestoreSpec{
				Source: cbv1.ClusterRestoreSource{
					LocationRef: cbv1.LocalObjectReference{Name: location},
					Namespace:   srcNS,
					Backup:      run,
				},
				Target: cbv1.ClusterRestoreTarget{
					Namespace:           dstNS,
					CreateNamespace:     true,
					StorageClassMapping: map[string]string{"fast": "standard"},
				},
				Mode:         cbv1.RestoreModeRecreate,
				Confirmation: dstNS,
			},
		}
		Expect(k8sClient.Create(ctx, cr)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(context.Background(), cr) })

		By("creating the target namespace")
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: dstNS}, &corev1.Namespace{})).To(Succeed())
		}, initTimeout, initPoll).Should(Succeed())

		By("provisioning the transplant staging claim from the PVC-meta tags, class mapped")
		prefix := restoreNamePrefix(clusterRestoreOwnerID("recover-gone"), "data")
		stagingName := rexposer.StagingPVCName(prefix)
		Eventually(func(g Gomega) {
			var staging corev1.PersistentVolumeClaim
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: suiteOperatorNamespace, Name: stagingName}, &staging)).To(Succeed())
			g.Expect(staging.Spec.StorageClassName).NotTo(BeNil())
			g.Expect(*staging.Spec.StorageClassName).To(Equal("standard"), "storageClassMapping must rewrite the class")
			capacity := staging.Spec.Resources.Requests[corev1.ResourceStorage]
			g.Expect(capacity.Cmp(resource.MustParse("2Gi"))).To(BeZero(), "capacity must come from the pvcsize tag")
			g.Expect(staging.Spec.VolumeName).To(BeEmpty(), "transplant staging is dynamically provisioned")
		}, initTimeout, initPoll).Should(Succeed())

		By("running the mover with Recreate semantics against the SOURCE namespace subtree")
		jobName := restoreJobName(prefix)
		Eventually(func(g Gomega) {
			args := restoreJobArgs(g, jobName)
			g.Expect(strings.Join(args, " ")).To(ContainSubstring(snapID + ":/data/" + srcNS + "/data"))
			g.Expect(args).To(ContainElement("--delete"))
		}, initTimeout, initPoll).Should(Succeed())

		By("playing the provisioner: a PV appears and the staging claim binds")
		pv := &corev1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{Name: "pvc-crestore-1"},
			Spec: corev1.PersistentVolumeSpec{
				Capacity:    corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("2Gi")},
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				PersistentVolumeSource: corev1.PersistentVolumeSource{
					CSI: &corev1.CSIPersistentVolumeSource{Driver: "test.csi", VolumeHandle: "vol-crestore"},
				},
				PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimDelete,
				StorageClassName:              "standard",
				ClaimRef:                      &corev1.ObjectReference{Namespace: suiteOperatorNamespace, Name: stagingName},
			},
		}
		Expect(k8sClient.Create(ctx, pv)).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(context.Background(), pv)
			patch := client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"finalizers":null}}`))
			_ = k8sClient.Patch(context.Background(), pv, patch)
		})
		pv.Status.Phase = corev1.VolumeBound
		Expect(k8sClient.Status().Update(ctx, pv)).To(Succeed())
		Eventually(func(g Gomega) {
			var staging corev1.PersistentVolumeClaim
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: suiteOperatorNamespace, Name: stagingName}, &staging)).To(Succeed())
			staging.Spec.VolumeName = pv.Name
			g.Expect(k8sClient.Update(ctx, &staging)).To(Succeed())
			staging.Status.Phase = corev1.ClaimBound
			g.Expect(k8sClient.Status().Update(ctx, &staging)).To(Succeed())
		}, initTimeout, initPoll).Should(Succeed())

		simulateMoverSucceeded(jobName, "node-2", mover.MoverResult{
			OK: true, Operation: string(mover.OpRestore), RestoredBytes: 4096,
		})

		By("driving the transplant handover: labels+Retain land, then the staging claim goes")
		Eventually(func(g Gomega) {
			var gotPV corev1.PersistentVolume
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: pv.Name}, &gotPV)).To(Succeed())
			g.Expect(gotPV.Spec.PersistentVolumeReclaimPolicy).To(Equal(corev1.PersistentVolumeReclaimRetain))
			g.Expect(gotPV.Labels[apiconst.LabelPVRole]).To(Equal(apiconst.PVRoleTransplant))
			var staging corev1.PersistentVolumeClaim
			serr := k8sClient.Get(ctx, client.ObjectKey{Namespace: suiteOperatorNamespace, Name: stagingName}, &staging)
			g.Expect(apierrors.IsNotFound(serr) || staging.DeletionTimestamp != nil).To(BeTrue())
		}, initTimeout, initPoll).Should(Succeed())

		By("playing the missing kube controllers: clear pvc-protection, mark the volume Released")
		patch := client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"finalizers":null}}`))
		_ = k8sClient.Patch(ctx, &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Namespace: suiteOperatorNamespace, Name: stagingName},
		}, patch)
		Eventually(func(g Gomega) {
			var gotPV corev1.PersistentVolume
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: pv.Name}, &gotPV)).To(Succeed())
			gotPV.Status.Phase = corev1.VolumeReleased
			g.Expect(k8sClient.Status().Update(ctx, &gotPV)).To(Succeed())
		}, initTimeout, initPoll).Should(Succeed())

		By("creating the final claim pre-bound in the target namespace, with provenance")
		Eventually(func(g Gomega) {
			var final corev1.PersistentVolumeClaim
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: dstNS, Name: "data"}, &final)).To(Succeed())
			g.Expect(final.Spec.VolumeName).To(Equal(pv.Name))
			g.Expect(final.Annotations[apiconst.AnnotationRestoredFrom]).To(Equal(run))
			g.Expect(final.Labels).To(BeEmpty(), "a restored PVC is the user's object — never operator-labeled")
		}, initTimeout, initPoll).Should(Succeed())

		By("completing once the final claim binds; the PV is handed over unlabeled")
		Eventually(func(g Gomega) {
			var final corev1.PersistentVolumeClaim
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: dstNS, Name: "data"}, &final)).To(Succeed())
			final.Status.Phase = corev1.ClaimBound
			g.Expect(k8sClient.Status().Update(ctx, &final)).To(Succeed())
		}, initTimeout, initPoll).Should(Succeed())

		Eventually(func(g Gomega) {
			var got cbv1.ClusterRestore
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: "recover-gone"}, &got)).To(Succeed())
			g.Expect(got.Status.Phase).To(Equal(string(status.RestorePhaseCompleted)))
			g.Expect(got.Status.RestoredVolumes).To(Equal(int32(1)))
			g.Expect(got.Status.RestoredBytes).To(Equal(int64(4096)))
		}, initTimeout, initPoll).Should(Succeed())

		Eventually(func(g Gomega) {
			var gotPV corev1.PersistentVolume
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: pv.Name}, &gotPV)).To(Succeed())
			g.Expect(gotPV.Labels).NotTo(HaveKey(apiconst.LabelPVRole), "handover strips the operator labels")
			g.Expect(gotPV.Spec.PersistentVolumeReclaimPolicy).To(Equal(corev1.PersistentVolumeReclaimDelete),
				"the class policy is restored after handover")
		}, initTimeout, initPoll).Should(Succeed())
	})
})
