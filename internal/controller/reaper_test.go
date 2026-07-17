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

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
	"github.com/CrystalBackup/CrystalBackup/internal/status"
)

// ---------------------------------------------------------------------------
// OrphanReaper-suite helpers.
// ---------------------------------------------------------------------------

func exposureLabelsFor(run, ns, pvc string) map[string]string {
	return map[string]string{
		apiconst.LabelManagedBy:     apiconst.ManagedByValue,
		apiconst.LabelClusterBackup: run,
		apiconst.LabelNamespace:     ns,
		apiconst.LabelPVC:           pvc,
	}
}

// makeExposureJob/PVC/Secret create the three native per-PVC exposure objects the reaper sweeps, in
// the operator namespace, with the given labels. Each registers a best-effort cleanup.
func makeExposureJob(name string, labels map[string]string) {
	GinkgoHelper()
	j := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Namespace: suiteOperatorNamespace, Name: name, Labels: labels},
		Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers:    []corev1.Container{{Name: "mover", Image: "busybox"}},
		}}},
	}
	Expect(k8sClient.Create(ctx, j)).To(Succeed())
	DeferCleanup(func() {
		_ = k8sClient.Delete(context.Background(), j, client.PropagationPolicy(metav1.DeletePropagationBackground))
	})
}

func makeExposurePVC(name string, labels map[string]string) {
	GinkgoHelper()
	p := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: suiteOperatorNamespace, Name: name, Labels: labels},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}},
		},
	}
	Expect(k8sClient.Create(ctx, p)).To(Succeed())
	DeferCleanup(func() { _ = k8sClient.Delete(context.Background(), p) })
}

func makeExposureSecret(name string, labels map[string]string) {
	GinkgoHelper()
	s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: suiteOperatorNamespace, Name: name, Labels: labels}}
	Expect(k8sClient.Create(ctx, s)).To(Succeed())
	DeferCleanup(func() { _ = k8sClient.Delete(context.Background(), s) })
}

// objReaped reports whether the reaper deleted the object: fully gone, OR requested-deleted
// (deletionTimestamp set). envtest runs no PVC-protection controller, so a temp clone PVC the
// reaper deletes keeps its kubernetes.io/pvc-protection finalizer and lingers Terminating — in
// production the real controller removes that finalizer, so requesting the delete IS the reap.
func objReaped(kind client.Object, name string) bool {
	err := k8sClient.Get(ctx, client.ObjectKey{Namespace: suiteOperatorNamespace, Name: name}, kind)
	if apierrors.IsNotFound(err) {
		return true
	}
	return err == nil && !kind.GetDeletionTimestamp().IsZero()
}

// objAlive reports whether the object is present AND not being deleted (a spared object).
func objAlive(kind client.Object, name string) bool {
	err := k8sClient.Get(ctx, client.ObjectKey{Namespace: suiteOperatorNamespace, Name: name}, kind)
	return err == nil && kind.GetDeletionTimestamp().IsZero()
}

var _ = Describe("OrphanReaper", func() {
	newReaper := func(minAge time.Duration) *OrphanReaper {
		return &OrphanReaper{Client: k8sClient, OperatorNamespace: suiteOperatorNamespace, MinAge: minAge}
	}

	It("reaps the native exposure objects of a Backup that is gone", func() {
		lbl := exposureLabelsFor("reap-gone-run", "reap-gone-ns", "pvc-a")
		makeExposureJob("reap-gone-mover", lbl)
		makeExposurePVC("reap-gone-clone", lbl)
		makeExposureSecret("reap-gone-mover", lbl) // (a creds Secret shares the Job's name in production)

		Expect(newReaper(0).sweepOnce(ctx)).To(Succeed())

		By("all three are reaped (no owning Backup exists)")
		Eventually(func(g Gomega) {
			g.Expect(objReaped(&batchv1.Job{}, "reap-gone-mover")).To(BeTrue())
			g.Expect(objReaped(&corev1.PersistentVolumeClaim{}, "reap-gone-clone")).To(BeTrue())
			g.Expect(objReaped(&corev1.Secret{}, "reap-gone-mover")).To(BeTrue())
		}, eventuallyTimeout, eventuallyPoll).Should(Succeed())
	})

	It("reaps exposure objects whose Backup volume is already terminal", func() {
		const run, ns, pvc = "reap-term-run", "reap-term-ns", "pvc-a"
		createTenantNamespace(ns)
		// A Backup that tracks the PVC as Completed — its teardown should already have removed the
		// exposure, so a lingering one is residue.
		b := createChildBackup(ns, run, "reap-term-loc")
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: run}, b)).To(Succeed())
			b.Status.Volumes = []cbv1.VolumeStatus{{Pvc: pvc, Phase: status.VolumePhaseCompleted}}
			g.Expect(k8sClient.Status().Update(ctx, b)).To(Succeed())
		}, initTimeout, initPoll).Should(Succeed())

		lbl := exposureLabelsFor(run, ns, pvc)
		makeExposureJob("reap-term-mover", lbl)
		makeExposurePVC("reap-term-clone", lbl)

		Expect(newReaper(0).sweepOnce(ctx)).To(Succeed())
		Eventually(func(g Gomega) {
			g.Expect(objReaped(&batchv1.Job{}, "reap-term-mover")).To(BeTrue())
			g.Expect(objReaped(&corev1.PersistentVolumeClaim{}, "reap-term-clone")).To(BeTrue())
		}, eventuallyTimeout, eventuallyPoll).Should(Succeed())
	})

	It("spares exposure objects of a live Backup with a non-terminal volume", func() {
		const run, ns, pvc = "reap-live-run", "reap-live-ns", "pvc-a"
		createTenantNamespace(ns)
		b := createChildBackup(ns, run, "reap-live-loc")
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: run}, b)).To(Succeed())
			b.Status.Volumes = []cbv1.VolumeStatus{{Pvc: pvc, Phase: status.VolumePhaseSnapshotting}}
			g.Expect(k8sClient.Status().Update(ctx, b)).To(Succeed())
		}, initTimeout, initPoll).Should(Succeed())
		// Confirm the non-terminal volume is durable before sweeping (the live Backup controller
		// gates a parentless Backup to Pending but never clears the volume set).
		Eventually(func(g Gomega) {
			var got cbv1.Backup
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: run}, &got)).To(Succeed())
			g.Expect(got.Status.Volumes).To(HaveLen(1))
			g.Expect(string(got.Status.Volumes[0].Phase)).To(Equal("Snapshotting"))
		}, eventuallyTimeout, eventuallyPoll).Should(Succeed())

		lbl := exposureLabelsFor(run, ns, pvc)
		makeExposureJob("reap-live-mover", lbl)
		makeExposurePVC("reap-live-clone", lbl)

		Expect(newReaper(0).sweepOnce(ctx)).To(Succeed())
		By("both survive — the volume is still in flight")
		Consistently(func(g Gomega) {
			g.Expect(objAlive(&batchv1.Job{}, "reap-live-mover")).To(BeTrue())
			g.Expect(objAlive(&corev1.PersistentVolumeClaim{}, "reap-live-clone")).To(BeTrue())
		}, 2*time.Second, 500*time.Millisecond).Should(Succeed())
	})

	It("spares a repository-init Job that carries no per-PVC label", func() {
		makeExposureJob("reap-init-job", map[string]string{apiconst.LabelManagedBy: apiconst.ManagedByValue})
		Expect(newReaper(0).sweepOnce(ctx)).To(Succeed())
		Consistently(func(g Gomega) {
			g.Expect(objAlive(&batchv1.Job{}, "reap-init-job")).To(BeTrue())
		}, 2*time.Second, 500*time.Millisecond).Should(Succeed())
	})

	It("spares objects younger than the minimum age even when they look orphaned", func() {
		lbl := exposureLabelsFor("reap-young-run", "reap-young-ns", "pvc-a") // no such Backup ⇒ would be orphan
		makeExposureJob("reap-young-mover", lbl)
		makeExposurePVC("reap-young-clone", lbl)

		Expect(newReaper(time.Hour).sweepOnce(ctx)).To(Succeed()) // just-created objects are far younger than 1h
		Consistently(func(g Gomega) {
			g.Expect(objAlive(&batchv1.Job{}, "reap-young-mover")).To(BeTrue())
			g.Expect(objAlive(&corev1.PersistentVolumeClaim{}, "reap-young-clone")).To(BeTrue())
		}, 2*time.Second, 500*time.Millisecond).Should(Succeed())
	})
})
