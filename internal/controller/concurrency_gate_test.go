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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
)

var _ = Describe("Backup mover concurrency gate", func() {
	It("holds a volume in Snapshotting at the mover limit, then proceeds when a slot frees", func() {
		const location = "bk-sem-loc"
		const run = "bk-sem-run"
		const ns = "bk-sem-ns"
		const pvcName = "sem-data"

		seedInitializedRepo(location, "kek-sem", "s3-sem")
		createTenantNamespace(ns)
		createSourcePVC(ns, pvcName, "ceph-block")

		// Occupy the single mover slot with an unrelated, non-terminal mover Job. It carries the
		// managed-by AND per-PVC labels the gate counts (a repository-init Job, managed-by but no
		// PVC, is deliberately not counted). envtest has no Job controller, so it never completes.
		blocker := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: suiteOperatorNamespace,
				Name:      "sem-blocker-mover",
				Labels: map[string]string{
					apiconst.LabelManagedBy:     apiconst.ManagedByValue,
					apiconst.LabelPVC:           "other-pvc",
					apiconst.LabelClusterBackup: "other-run",
					apiconst.LabelNamespace:     "other-ns",
				},
			},
			Spec: batchv1.JobSpec{
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						RestartPolicy: corev1.RestartPolicyNever,
						Containers:    []corev1.Container{{Name: "blocker", Image: "busybox"}},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, blocker)).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(context.Background(), blocker, client.PropagationPolicy(metav1.DeletePropagationBackground))
		})

		// The parent run is capped at one concurrent mover.
		parent := &cbv1.ClusterBackup{
			ObjectMeta: metav1.ObjectMeta{Name: run},
			Spec: cbv1.ClusterBackupSpec{ClusterBackupRunSpec: cbv1.ClusterBackupRunSpec{
				LocationRef:         cbv1.LocalObjectReference{Name: location},
				MaxConcurrentMovers: 1,
			}},
		}
		Expect(k8sClient.Create(ctx, parent)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(context.Background(), parent) })

		createChildBackup(ns, run, location)
		moverName := moverJobNameFor(run, pvcName)

		By("the volume reaches Snapshotting but its mover Job is withheld while the slot is taken")
		Eventually(func(g Gomega) {
			v := volumeByPVC(getBackupG(g, ns, run), pvcName)
			g.Expect(v).NotTo(BeNil())
			g.Expect(string(v.Phase)).To(Equal("Snapshotting"))
		}, initTimeout, initPoll).Should(Succeed())
		Consistently(func(g Gomega) {
			err := k8sClient.Get(ctx, client.ObjectKey{Namespace: suiteOperatorNamespace, Name: moverName}, &batchv1.Job{})
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
		}, 3*time.Second, 500*time.Millisecond).Should(Succeed())

		By("freeing the slot lets the mover Job be created")
		Expect(k8sClient.Delete(ctx, blocker, client.PropagationPolicy(metav1.DeletePropagationBackground))).To(Succeed())
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: suiteOperatorNamespace, Name: moverName}, &batchv1.Job{})).To(Succeed())
		}, initTimeout, initPoll).Should(Succeed())
	})
})
