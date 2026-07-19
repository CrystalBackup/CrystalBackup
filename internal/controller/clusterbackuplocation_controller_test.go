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

	"filippo.io/age"
	. "github.com/onsi/ginkgo/v2" //nolint:revive,staticcheck
	. "github.com/onsi/gomega"    //nolint:revive,staticcheck

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
)

// Timings for Eventually() throughout this file: envtest reconciles are typically
// sub-second, but a generous bound keeps the suite from flaking under CI load.
const (
	eventuallyTimeout = 15 * time.Second
	eventuallyPoll    = 200 * time.Millisecond
)

// generateAgeIdentity returns a freshly minted age X25519 identity string
// ("AGE-SECRET-KEY-1..."), suitable for a KEK Secret's data["identity"] — mirrors
// test/crucible/tests/m1_helpers_test.go's m1EnsurePlatformSecrets so the two suites exercise
// the same real key material shape.
func generateAgeIdentity() string {
	GinkgoHelper()
	id, err := age.GenerateX25519Identity()
	Expect(err).NotTo(HaveOccurred())
	return id.String()
}

// createKEKSecret creates a Secret named name in the suite operator namespace holding
// identity under data key "identity" (validateEncryption's kekIdentityDataKey), and
// registers its deletion for after the spec regardless of pass/fail.
func createKEKSecret(name, identity string) {
	GinkgoHelper()
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: suiteOperatorNamespace},
		Data:       map[string][]byte{kekIdentityDataKey: []byte(identity)},
	}
	Expect(k8sClient.Create(ctx, sec)).To(Succeed())
	DeferCleanup(func() { _ = k8sClient.Delete(context.Background(), sec) })
}

// createS3CredsSecret creates a plausible S3-credentials Secret in the suite operator
// namespace. ClusterBackupLocationReconciler never reads it (only the BackupRepository
// controller, M1 task #16, will) but Spec.S3.CredentialsSecretRef is a required field, so
// every test location needs a name to point at.
func createS3CredsSecret(name string) {
	GinkgoHelper()
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: suiteOperatorNamespace},
		Data: map[string][]byte{
			"AWS_ACCESS_KEY_ID":     []byte("test-access-key"),
			"AWS_SECRET_ACCESS_KEY": []byte("test-secret-key"),
		},
	}
	Expect(k8sClient.Create(ctx, sec)).To(Succeed())
	DeferCleanup(func() { _ = k8sClient.Delete(context.Background(), sec) })
}

// newTestLocation builds a schema-valid, not-yet-created ClusterBackupLocation pointing at
// kekSecret/s3Secret in the suite operator namespace.
func newTestLocation(name, kekSecret, s3Secret string, isDefault bool) *cbv1.ClusterBackupLocation {
	return &cbv1.ClusterBackupLocation{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: cbv1.ClusterBackupLocationSpec{
			Default:   isDefault,
			ClusterID: "envtest-cluster",
			S3: cbv1.S3Spec{
				Endpoint:             "https://s3.example.test",
				Bucket:               "test-bucket",
				CredentialsSecretRef: cbv1.LocalObjectReference{Name: s3Secret},
			},
			Encryption: cbv1.ClusterEncryptionSpec{
				ClusterKEKSecretRef: cbv1.LocalObjectReference{Name: kekSecret},
			},
		},
	}
}

// createTestLocation creates loc and registers a best-effort delete for after the spec, so a
// failed assertion mid-It still leaves the cluster-scoped object space clean for the next spec.
func createTestLocation(loc *cbv1.ClusterBackupLocation) {
	GinkgoHelper()
	Expect(k8sClient.Create(ctx, loc)).To(Succeed())
	DeferCleanup(func() { _ = k8sClient.Delete(context.Background(), loc) })
	DeferCleanup(func() {
		_ = k8sClient.Delete(context.Background(), &cbv1.BackupRepository{ObjectMeta: metav1.ObjectMeta{Name: loc.Name}})
	})
}

var _ = Describe("ClusterBackupLocationReconciler", func() {

	It("rejects mutating the repository identity or mode (CEL immutability), but allows retention edits", func() {
		const name = "cbl-immutable"
		createKEKSecret("kek-immutable", generateAgeIdentity())
		createS3CredsSecret("s3-immutable")
		createTestLocation(newTestLocation(name, "kek-immutable", "s3-immutable", false))

		// A helper that re-Gets the location, applies a mutation, and returns the Update error.
		mutate := func(f func(l *cbv1.ClusterBackupLocation)) error {
			var l cbv1.ClusterBackupLocation
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: name}, &l)).To(Succeed())
			f(&l)
			return k8sClient.Update(ctx, &l)
		}

		By("mode, clusterID and the s3 identity fields are immutable")
		Expect(mutate(func(l *cbv1.ClusterBackupLocation) { l.Spec.Mode = cbv1.LocationModeImmutable })).
			To(HaveOccurred(), "flipping spec.mode must be rejected (WORM downgrade / R18)")
		Expect(mutate(func(l *cbv1.ClusterBackupLocation) { l.Spec.ClusterID = "different-cluster" })).
			To(HaveOccurred(), "changing spec.clusterID must be rejected (silent repository re-point)")
		Expect(mutate(func(l *cbv1.ClusterBackupLocation) { l.Spec.S3.Bucket = "other-bucket" })).
			To(HaveOccurred(), "changing spec.s3.bucket must be rejected (repository identity)")
		Expect(mutate(func(l *cbv1.ClusterBackupLocation) { l.Spec.S3.Endpoint = "https://other.example.test" })).
			To(HaveOccurred(), "changing spec.s3.endpoint must be rejected (repository identity)")
		Expect(mutate(func(l *cbv1.ClusterBackupLocation) { l.Spec.S3.Prefix = "different-prefix" })).
			To(HaveOccurred(), "adding/changing spec.s3.prefix must be rejected (repository identity)")

		By("a mutable field (retention) still updates — the rule is scoped, not a blanket freeze")
		Expect(mutate(func(l *cbv1.ClusterBackupLocation) { l.Spec.Retention = cbv1.RetentionSpec{KeepDaily: 14} })).
			To(Succeed(), "retention must remain editable on an existing location")
	})

	It("provisions an owned BackupRepository and reports Ready for a valid location", func() {
		const name = "cbl-happy"
		createKEKSecret("kek-happy", generateAgeIdentity())
		createS3CredsSecret("s3-happy")
		createTestLocation(newTestLocation(name, "kek-happy", "s3-happy", false))

		By("a cluster-scoped BackupRepository is created, owned by the location")
		var repo cbv1.BackupRepository
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: name}, &repo)).To(Succeed())
		}, eventuallyTimeout, eventuallyPoll).Should(Succeed())
		Expect(repo.Namespace).To(BeEmpty(), "a BackupRepository is cluster-scoped")

		owner := metav1.GetControllerOf(&repo)
		Expect(owner).NotTo(BeNil(), "BackupRepository has no controller ownerReference")
		Expect(owner.Kind).To(Equal("ClusterBackupLocation"))
		Expect(owner.Name).To(Equal(name))

		By("the location reports repositoryRef, Reachable, EncryptionValid, Ready, and carries the finalizer")
		Eventually(func(g Gomega) {
			var got cbv1.ClusterBackupLocation
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: name}, &got)).To(Succeed())
			g.Expect(got.Status.RepositoryRef).To(Equal(name))
			g.Expect(apimeta.IsStatusConditionTrue(got.Status.Conditions, ConditionReachable)).To(BeTrue())
			g.Expect(apimeta.IsStatusConditionTrue(got.Status.Conditions, ConditionEncryptionValid)).To(BeTrue())
			g.Expect(apimeta.IsStatusConditionTrue(got.Status.Conditions, ConditionReady)).To(BeTrue())
			g.Expect(got.Status.Phase).To(Equal("Ready"))
			g.Expect(controllerutil.ContainsFinalizer(&got, apiconst.FinalizerLocation)).To(BeTrue())
		}, eventuallyTimeout, eventuallyPoll).Should(Succeed())
	})

	It("reports EncryptionValid=False and Ready=False for a malformed KEK, without crashing", func() {
		const name = "cbl-badkek"
		createKEKSecret("kek-bad", "not-a-valid-age-identity")
		createS3CredsSecret("s3-bad")
		createTestLocation(newTestLocation(name, "kek-bad", "s3-bad", false))

		Eventually(func(g Gomega) {
			var got cbv1.ClusterBackupLocation
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: name}, &got)).To(Succeed())
			g.Expect(apimeta.IsStatusConditionTrue(got.Status.Conditions, ConditionEncryptionValid)).To(BeFalse())
			g.Expect(apimeta.IsStatusConditionTrue(got.Status.Conditions, ConditionReady)).To(BeFalse())
			g.Expect(got.Status.Phase).To(Equal("Degraded"))
		}, eventuallyTimeout, eventuallyPoll).Should(Succeed())

		// No BackupRepository should have been provisioned for a location that never got
		// past the fail-fast encryption gate.
		Consistently(func(g Gomega) {
			var repo cbv1.BackupRepository
			err := k8sClient.Get(ctx, client.ObjectKey{Name: name}, &repo)
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
		}, 2*time.Second, 200*time.Millisecond).Should(Succeed())
	})

	It("never generates a cluster KEK itself: an absent KEK degrades the location and creates no Secret", func() {
		const name = "cbl-nokek"
		const kekName = "kek-absent" // deliberately NEVER created — the admin must provide it
		createS3CredsSecret("s3-nokek")
		createTestLocation(newTestLocation(name, kekName, "s3-nokek", false))

		By("the location reports EncryptionValid=False/KEKMissing and is not Ready")
		Eventually(func(g Gomega) {
			var got cbv1.ClusterBackupLocation
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: name}, &got)).To(Succeed())
			c := apimeta.FindStatusCondition(got.Status.Conditions, ConditionEncryptionValid)
			g.Expect(c).NotTo(BeNil())
			g.Expect(c.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(c.Reason).To(Equal("KEKMissing"))
			g.Expect(apimeta.IsStatusConditionTrue(got.Status.Conditions, ConditionReady)).To(BeFalse())
		}, eventuallyTimeout, eventuallyPoll).Should(Succeed())

		// The load-bearing guard: the operator must NEVER mint the referenced KEK Secret. The cluster
		// KEK is the admin's root of trust — a silently generated key would vanish with the cluster and
		// leave every backup unrecoverable (spec/03). It stays absent; the location just waits.
		By("the operator never creates the referenced KEK Secret itself")
		Consistently(func(g Gomega) {
			var sec corev1.Secret
			err := k8sClient.Get(ctx, client.ObjectKey{Namespace: suiteOperatorNamespace, Name: kekName}, &sec)
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "operator must not generate a cluster KEK Secret")
		}, 2*time.Second, 200*time.Millisecond).Should(Succeed())
	})

	It("flags MultipleDefaults for two default locations and clears it for a lone default", func() {
		const nameA = "cbl-default-a"
		const nameB = "cbl-default-b"
		createKEKSecret("kek-default", generateAgeIdentity())
		createS3CredsSecret("s3-default")

		By("a lone default location reports MultipleDefaults=False")
		createTestLocation(newTestLocation(nameA, "kek-default", "s3-default", true))
		Eventually(func(g Gomega) {
			var got cbv1.ClusterBackupLocation
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: nameA}, &got)).To(Succeed())
			g.Expect(apimeta.IsStatusConditionTrue(got.Status.Conditions, ConditionMultipleDefaults)).To(BeFalse())
			g.Expect(apimeta.IsStatusConditionTrue(got.Status.Conditions, ConditionReady)).To(BeTrue())
		}, eventuallyTimeout, eventuallyPoll).Should(Succeed())

		By("a second default location causes at least one of the two to flag MultipleDefaults=True")
		createTestLocation(newTestLocation(nameB, "kek-default", "s3-default", true))
		Eventually(func(g Gomega) {
			flagged, readyDefaults := 0, 0
			for _, name := range []string{nameA, nameB} {
				var got cbv1.ClusterBackupLocation
				g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: name}, &got)).To(Succeed())
				multi := apimeta.IsStatusConditionTrue(got.Status.Conditions, ConditionMultipleDefaults)
				isReady := apimeta.IsStatusConditionTrue(got.Status.Conditions, ConditionReady)
				if multi {
					flagged++
				}
				if isReady {
					readyDefaults++
				}
				// A location known to conflict must never simultaneously read Ready — that
				// would be silently treating it as the default anyway.
				g.Expect(multi && isReady).To(BeFalse(),
					"location %q reports MultipleDefaults yet also Ready", name)
			}
			g.Expect(flagged).To(BeNumerically(">=", 1), "neither location reports MultipleDefaults=true")
			g.Expect(readyDefaults).To(BeNumerically("<=", 1), "both default locations report Ready")
		}, eventuallyTimeout, eventuallyPoll).Should(Succeed())
	})

	It("reports Reachable=False for an endpoint the prober marks unreachable", func() {
		const name = "cbl-unreachable"
		createKEKSecret("kek-unreachable", generateAgeIdentity())
		createS3CredsSecret("s3-unreachable")

		loc := newTestLocation(name, "kek-unreachable", "s3-unreachable", false)
		loc.Spec.S3.Endpoint = unreachableTestEndpoint
		createTestLocation(loc)

		Eventually(func(g Gomega) {
			var got cbv1.ClusterBackupLocation
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: name}, &got)).To(Succeed())
			g.Expect(apimeta.IsStatusConditionTrue(got.Status.Conditions, ConditionReachable)).To(BeFalse())
			g.Expect(apimeta.IsStatusConditionTrue(got.Status.Conditions, ConditionReady)).To(BeFalse())
			g.Expect(got.Status.Phase).To(Equal("Degraded"))
		}, eventuallyTimeout, eventuallyPoll).Should(Succeed())
	})

	It("flags RetentionIgnored on an Immutable location with a retention policy, not on a Standard one", func() {
		createKEKSecret("kek-retention", generateAgeIdentity())
		createS3CredsSecret("s3-retention")

		By("an Immutable location whose spec.retention requests keep* reports RetentionIgnored=True")
		immutable := newTestLocation("cbl-retention-immutable", "kek-retention", "s3-retention", false)
		immutable.Spec.Mode = cbv1.LocationModeImmutable
		immutable.Spec.Retention = cbv1.RetentionSpec{KeepDaily: 7}
		createTestLocation(immutable)
		Eventually(func(g Gomega) {
			var got cbv1.ClusterBackupLocation
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: immutable.Name}, &got)).To(Succeed())
			g.Expect(apimeta.IsStatusConditionTrue(got.Status.Conditions, ConditionRetentionIgnored)).To(BeTrue())
		}, eventuallyTimeout, eventuallyPoll).Should(Succeed())

		By("a Standard location with the same retention reports RetentionIgnored=False (retention is applied)")
		standard := newTestLocation("cbl-retention-standard", "kek-retention", "s3-retention", false)
		standard.Spec.Mode = cbv1.LocationModeStandard
		standard.Spec.Retention = cbv1.RetentionSpec{KeepDaily: 7}
		createTestLocation(standard)
		Eventually(func(g Gomega) {
			var got cbv1.ClusterBackupLocation
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: standard.Name}, &got)).To(Succeed())
			// Present AND False (not merely absent): the advisory is actively evaluated and cleared.
			g.Expect(apimeta.IsStatusConditionFalse(got.Status.Conditions, ConditionRetentionIgnored)).To(BeTrue())
		}, eventuallyTimeout, eventuallyPoll).Should(Succeed())
	})

	It("removes the finalizer and deletes the object on delete, without erasing the owned BackupRepository", func() {
		const name = "cbl-delete"
		createKEKSecret("kek-delete", generateAgeIdentity())
		createS3CredsSecret("s3-delete")
		loc := newTestLocation(name, "kek-delete", "s3-delete", false)
		createTestLocation(loc)

		By("waiting for the repository to be provisioned and the finalizer to land")
		Eventually(func(g Gomega) {
			var got cbv1.ClusterBackupLocation
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: name}, &got)).To(Succeed())
			g.Expect(got.Status.RepositoryRef).To(Equal(name))
			g.Expect(controllerutil.ContainsFinalizer(&got, apiconst.FinalizerLocation)).To(BeTrue())
		}, eventuallyTimeout, eventuallyPoll).Should(Succeed())

		By("deleting the location")
		Expect(k8sClient.Delete(ctx, loc)).To(Succeed())

		By("the finalizer clears and the object is fully removed")
		Eventually(func(g Gomega) {
			var got cbv1.ClusterBackupLocation
			err := k8sClient.Get(ctx, client.ObjectKey{Name: name}, &got)
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
		}, eventuallyTimeout, eventuallyPoll).Should(Succeed())

		// NOTE: envtest runs only kube-apiserver + etcd, never kube-controller-manager, so
		// the built-in garbage collector that would cascade-delete the owned BackupRepository
		// via its controller ownerReference does not run in this suite — that cascade is
		// real, independently-tested Kubernetes behaviour, not this controller's to re-prove.
		// What IS this controller's responsibility — setting a correct controller
		// ownerReference, and NOT touching the repository itself on delete (adr/0009: delete
		// never erases) — is asserted here: the BackupRepository this location owned is still
		// present and untouched by finalize().
		var repo cbv1.BackupRepository
		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: name}, &repo)).To(Succeed())
	})
})
