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
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
)

var _ = Describe("transient manifest-mover RoleBinding", func() {
	var (
		tenantNS string
		jobName  string
	)

	BeforeEach(func() {
		tenantNS = fmt.Sprintf("c-tenant-%d", GinkgoRandomSeed()%100000+int64(GinkgoParallelProcess()))
		jobName = "manifests-" + tenantNS
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: tenantNS},
		}))).To(Succeed())
	})

	req := func(ns, job string) manifestRBACRequest {
		return manifestRBACRequest{
			TargetNamespace:    ns,
			JobName:            job,
			ClusterRoleName:    "crystal-backup-manifest-reader",
			ServiceAccountName: "crystal-backup-manifest-mover",
			OperatorNamespace:  suiteOperatorNamespace,
		}
	}

	get := func(ns, job string) (*rbacv1.RoleBinding, error) {
		rb := &rbacv1.RoleBinding{}
		err := k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: manifestBindingName(job)}, rb)
		return rb, err
	}

	It("lands in the TARGET namespace with a subject from the operator namespace", func() {
		Expect(ensureManifestRoleBinding(ctx, k8sClient, req(tenantNS, jobName))).To(Succeed())

		rb, err := get(tenantNS, jobName)
		Expect(err).NotTo(HaveOccurred())

		// The binding is namespaced into the tenant; that is what confines a ClusterRole whose
		// rules say apiGroups "*" to exactly one namespace.
		Expect(rb.Namespace).To(Equal(tenantNS))
		Expect(rb.RoleRef.Kind).To(Equal("ClusterRole"))

		// The subject lives somewhere else entirely. A RoleBinding may name a ServiceAccount
		// from another namespace, and that asymmetry is the whole reason a mover in
		// crystal-backup-system can be granted rights inside a tenant namespace without any
		// operator object living there permanently.
		Expect(rb.Subjects).To(HaveLen(1))
		Expect(rb.Subjects[0].Namespace).To(Equal(suiteOperatorNamespace))
		Expect(rb.Subjects[0].Kind).To(Equal(rbacv1.ServiceAccountKind))
	})

	It("carries the labels the reaper needs to find it and to know whose it is", func() {
		Expect(ensureManifestRoleBinding(ctx, k8sClient, req(tenantNS, jobName))).To(Succeed())
		rb, err := get(tenantNS, jobName)
		Expect(err).NotTo(HaveOccurred())

		Expect(rb.Labels).To(HaveKeyWithValue(apiconst.LabelManagedBy, apiconst.ManagedByValue))
		Expect(rb.Labels).To(HaveKeyWithValue(apiconst.LabelMoverRole, apiconst.MoverRoleManifest))
		// Without the job label the reaper cannot check liveness; without the operator-namespace
		// label two operators would reap each other's in-flight grants.
		Expect(rb.Labels).To(HaveKeyWithValue(apiconst.LabelMoverJob, jobName))
		Expect(rb.Labels).To(HaveKeyWithValue(apiconst.LabelOperatorNS, suiteOperatorNamespace))
	})

	It("is idempotent, because a reconcile retries", func() {
		Expect(ensureManifestRoleBinding(ctx, k8sClient, req(tenantNS, jobName))).To(Succeed())
		Expect(ensureManifestRoleBinding(ctx, k8sClient, req(tenantNS, jobName))).To(Succeed())
	})

	It("deletes on the nominal path, and a second delete is not an error", func() {
		Expect(ensureManifestRoleBinding(ctx, k8sClient, req(tenantNS, jobName))).To(Succeed())
		Expect(deleteManifestRoleBinding(ctx, k8sClient, tenantNS, jobName)).To(Succeed())

		_, err := get(tenantNS, jobName)
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the grant must not outlive its job")

		// The reaper may have won the race, or a previous attempt succeeded before its status
		// update did. Either way the grant is gone, which is all that matters.
		Expect(deleteManifestRoleBinding(ctx, k8sClient, tenantNS, jobName)).To(Succeed())
	})

	Context("the reaper backstop", func() {
		// This is the only automatic cleanup: the Job lives in the operator namespace and the
		// binding in a tenant namespace, so no ownerReference can span them.
		reaper := func() *OrphanReaper {
			return &OrphanReaper{Client: k8sClient, OperatorNamespace: suiteOperatorNamespace}
		}

		makeJob := func(name string, finished bool) {
			job := &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: suiteOperatorNamespace},
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							RestartPolicy: corev1.RestartPolicyNever,
							Containers:    []corev1.Container{{Name: "c", Image: "busybox"}},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, job)).To(Succeed())
			if finished {
				job.Status.Succeeded = 1
				Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())
			}
		}

		It("spares a binding whose job is still running", func() {
			makeJob(jobName, false)
			Expect(ensureManifestRoleBinding(ctx, k8sClient, req(tenantNS, jobName))).To(Succeed())

			reaper().reapManifestBindings(ctx)

			_, err := get(tenantNS, jobName)
			Expect(err).NotTo(HaveOccurred(), "reaping a live grant would break an in-flight backup")
		})

		It("spares a binding with no job label rather than guessing", func() {
			Expect(ensureManifestRoleBinding(ctx, k8sClient, req(tenantNS, jobName))).To(Succeed())
			rb, err := get(tenantNS, jobName)
			Expect(err).NotTo(HaveOccurred())
			delete(rb.Labels, apiconst.LabelMoverJob)
			Expect(k8sClient.Update(ctx, rb)).To(Succeed())

			reaper().reapManifestBindings(ctx)

			_, err = get(tenantNS, jobName)
			Expect(err).NotTo(HaveOccurred(), "an erroneous delete here breaks a live backup; refuse to guess")
		})

		It("ignores a binding belonging to a different operator namespace", func() {
			Expect(ensureManifestRoleBinding(ctx, k8sClient, req(tenantNS, jobName))).To(Succeed())
			rb, err := get(tenantNS, jobName)
			Expect(err).NotTo(HaveOccurred())
			rb.Labels[apiconst.LabelOperatorNS] = "some-other-operator"
			Expect(k8sClient.Update(ctx, rb)).To(Succeed())

			reaper().reapManifestBindings(ctx)

			_, err = get(tenantNS, jobName)
			Expect(err).NotTo(HaveOccurred(), "one operator must not reap another's in-flight grant")
		})
	})
})
