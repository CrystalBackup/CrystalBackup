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

package v1alpha1

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	crystalbackupiov1alpha1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
)

// location builds a minimal valid ClusterBackupLocation fixture for the webhook suite (the
// suite serves the REAL webhook against envtest, so every Create below crosses the actual
// admission path registered in config/webhook).
func location(name string, isDefault bool) *crystalbackupiov1alpha1.ClusterBackupLocation {
	return &crystalbackupiov1alpha1.ClusterBackupLocation{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: crystalbackupiov1alpha1.ClusterBackupLocationSpec{
			Default:   isDefault,
			ClusterID: "test-cluster",
			S3: crystalbackupiov1alpha1.S3Spec{
				Endpoint:             "https://s3.test",
				Bucket:               "b",
				CredentialsSecretRef: crystalbackupiov1alpha1.LocalObjectReference{Name: "s3"},
			},
			Encryption: crystalbackupiov1alpha1.ClusterEncryptionSpec{
				ClusterKEKSecretRef: crystalbackupiov1alpha1.LocalObjectReference{Name: "kek"},
			},
		},
	}
}

var _ = Describe("ClusterBackupLocation Webhook", func() {
	AfterEach(func() {
		// Best-effort sweep so specs stay independent (cluster-scoped objects persist).
		var list crystalbackupiov1alpha1.ClusterBackupLocationList
		Expect(k8sClient.List(ctx, &list)).To(Succeed())
		for i := range list.Items {
			_ = k8sClient.Delete(context.Background(), &list.Items[i])
		}
	})

	Context("single-default uniqueness (admission rule 4, adr/0010)", func() {
		It("admits the first default and denies a competing second one", func() {
			Expect(k8sClient.Create(ctx, location("primary", true))).To(Succeed())

			err := k8sClient.Create(ctx, location("pretender", true))
			Expect(err).To(HaveOccurred(), "a second default must be denied")
			Expect(apierrors.IsInvalid(err)).To(BeTrue())
			Expect(err.Error()).To(ContainSubstring("primary"))
		})

		It("admits any number of non-default locations alongside the default", func() {
			Expect(k8sClient.Create(ctx, location("primary", true))).To(Succeed())
			Expect(k8sClient.Create(ctx, location("secondary", false))).To(Succeed())
			Expect(k8sClient.Create(ctx, location("tertiary", false))).To(Succeed())
		})

		It("denies flipping a non-default location to default while another default exists", func() {
			Expect(k8sClient.Create(ctx, location("primary", true))).To(Succeed())
			secondary := location("secondary", false)
			Expect(k8sClient.Create(ctx, secondary)).To(Succeed())

			secondary.Spec.Default = true
			err := k8sClient.Update(ctx, secondary)
			Expect(err).To(HaveOccurred(), "flipping to a second default must be denied")

			By("allowing the flip once the previous default steps down")
			var primary crystalbackupiov1alpha1.ClusterBackupLocation
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: "primary"}, &primary)).To(Succeed())
			primary.Spec.Default = false
			Expect(k8sClient.Update(ctx, &primary)).To(Succeed())

			Eventually(func() error {
				var s crystalbackupiov1alpha1.ClusterBackupLocation
				if err := k8sClient.Get(ctx, client.ObjectKey{Name: "secondary"}, &s); err != nil {
					return err
				}
				s.Spec.Default = true
				return k8sClient.Update(ctx, &s)
			}).Should(Succeed(), "the webhook's cached list must observe the step-down")
		})

		It("admits an update that keeps an existing default the default", func() {
			primary := location("primary", true)
			Expect(k8sClient.Create(ctx, primary)).To(Succeed())
			// Mutate a MUTABLE field: the repository identity (clusterID/s3.*) and mode are pinned by
			// CEL immutability, so a self-update must touch something editable like retention.
			primary.Spec.Retention = crystalbackupiov1alpha1.RetentionSpec{KeepDaily: 7}
			Expect(k8sClient.Update(ctx, primary)).To(Succeed(),
				"the default location must be able to update itself")
		})
	})
})
