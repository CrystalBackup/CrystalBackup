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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
	"github.com/CrystalBackup/CrystalBackup/internal/keys"
	"github.com/CrystalBackup/CrystalBackup/internal/mover"
)

// ---------------------------------------------------------------------------
// Namespace-plane location helpers.
// ---------------------------------------------------------------------------

// createTenantS3CredsSecret creates an S3-credentials Secret in a TENANT namespace — the
// namespace-plane counterpart of createS3CredsSecret, which puts one in the operator namespace.
// The distinction is the point: a namespaced location's credentials live beside it, never in
// crystal-backup-system.
func createTenantS3CredsSecret(namespace, name string) {
	GinkgoHelper()
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Data: map[string][]byte{
			mover.SecretKeyAWSAccessKeyID:     []byte("tenant-access-key"),
			mover.SecretKeyAWSSecretAccessKey: []byte("tenant-secret-key"),
		},
	}
	Expect(k8sClient.Create(ctx, sec)).To(Succeed())
	DeferCleanup(func() { _ = k8sClient.Delete(context.Background(), sec) })
}

// createTenantPasswordSecret creates a user-owned restic password Secret in a tenant namespace.
func createTenantPasswordSecret(namespace, name, password string) {
	GinkgoHelper()
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Data:       map[string][]byte{keys.UserPasswordSecretKey: []byte(password)},
	}
	Expect(k8sClient.Create(ctx, sec)).To(Succeed())
	DeferCleanup(func() { _ = k8sClient.Delete(context.Background(), sec) })
}

// newTenantLocation builds a schema-valid, not-yet-created BackupLocation. passwordSecret empty
// means "let the operator generate one"; clusterID empty means "inherit from the default
// ClusterBackupLocation".
func newTenantLocation(namespace, name, s3Secret, passwordSecret, clusterID string) *cbv1.BackupLocation {
	loc := &cbv1.BackupLocation{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec: cbv1.BackupLocationSpec{
			ClusterID: clusterID,
			S3: cbv1.S3Spec{
				Endpoint:             "https://s3.tenant.test",
				Bucket:               "tenant-bucket",
				CredentialsSecretRef: cbv1.LocalObjectReference{Name: s3Secret},
			},
		},
	}
	if passwordSecret != "" {
		loc.Spec.Encryption.RepositoryPasswordSecretRef = &cbv1.LocalObjectReference{Name: passwordSecret}
	}
	return loc
}

// createTenantLocation creates loc and registers cleanup of it AND of the cluster-scoped
// repository it provisions — the repository is not owned, so nothing else would collect it.
func createTenantLocation(loc *cbv1.BackupLocation) {
	GinkgoHelper()
	Expect(k8sClient.Create(ctx, loc)).To(Succeed())
	DeferCleanup(func() {
		_ = k8sClient.Delete(context.Background(), loc)
		_ = k8sClient.Delete(context.Background(), &cbv1.BackupRepository{
			ObjectMeta: metav1.ObjectMeta{Name: namespacedRepositoryName(loc.Namespace, loc.Name)},
		})
	})
}

// getTenantLocationG fetches a BackupLocation inside an Eventually block.
func getTenantLocationG(g Gomega, namespace, name string) cbv1.BackupLocation {
	var loc cbv1.BackupLocation
	g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &loc)).To(Succeed())
	return loc
}

var _ = Describe("BackupLocationReconciler", func() {

	It("provisions a repository named <namespace>--<name>, back-linked by labels, and reaches Ready", func() {
		const (
			ns       = "bl-happy-ns"
			name     = "offsite"
			repoName = ns + "--" + name
		)
		createTenantNamespace(ns)
		createTenantS3CredsSecret(ns, "offsite-s3")
		createTenantPasswordSecret(ns, "offsite-key", "the-users-own-password")
		createTenantLocation(newTenantLocation(ns, name, "offsite-s3", "offsite-key", "envtest-cluster"))

		By("the repository is created cluster-scoped, with the back-link labels and NO ownerReference")
		var repo cbv1.BackupRepository
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: repoName}, &repo)).To(Succeed())
		}, initTimeout, initPoll).Should(Succeed())
		Expect(repo.Labels).To(HaveKeyWithValue(apiconst.LabelNamespace, ns))
		Expect(repo.Labels).To(HaveKeyWithValue(apiconst.LabelLocation, name))
		// A namespaced owner on a cluster-scoped dependent is read as DANGLING by the garbage
		// collector, which would delete the repository out from under the location that made it.
		Expect(repo.OwnerReferences).To(BeEmpty(),
			"a cluster-scoped BackupRepository must not be owned by a namespaced BackupLocation")

		By("the repository records namespace-plane identity: scope, owner namespace and key slots")
		Eventually(func(g Gomega) {
			r := getRepositoryG(g, repoName)
			g.Expect(r.Status.Scope).To(Equal(scopeNamespaced))
			g.Expect(r.Status.OwnerNamespace).To(Equal(ns))
			g.Expect(r.Status.Location.Kind).To(Equal(kindBackupLocation))
			g.Expect(r.Status.KeySlots).To(Equal([]string{keySlotTenant}),
				"without platformAccess the tenant slot is the ONLY slot")
		}, initTimeout, initPoll).Should(Succeed())

		By("the location is Initializing while restic init runs, then Ready once it succeeds")
		Eventually(func(g Gomega) {
			l := getTenantLocationG(g, ns, name)
			g.Expect(l.Status.RepositoryRef).To(Equal(repoName))
			g.Expect(l.Status.Phase).To(Equal(locationPhaseInitializing))
		}, initTimeout, initPoll).Should(Succeed())

		simulateRepositoryInitialized(repoName)

		Eventually(func(g Gomega) {
			l := getTenantLocationG(g, ns, name)
			g.Expect(apimeta.IsStatusConditionTrue(l.Status.Conditions, ConditionReady)).To(BeTrue())
			g.Expect(l.Status.Phase).To(Equal("Ready"))
		}, initTimeout, initPoll).Should(Succeed())
	})

	It("generates the repository password in the USER's namespace, unowned, when none is referenced", func() {
		const (
			ns   = "bl-genkey-ns"
			name = "generated"
		)
		createTenantNamespace(ns)
		createTenantS3CredsSecret(ns, "generated-s3")
		createTenantLocation(newTenantLocation(ns, name, "generated-s3", "", "envtest-cluster"))

		var sec corev1.Secret
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx,
				client.ObjectKey{Namespace: ns, Name: keys.UserPasswordSecretName(name)}, &sec)).To(Succeed())
		}, initTimeout, initPoll).Should(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(context.Background(), &sec) })

		// In the TENANT's namespace, not crystal-backup-system: their key, their reversibility (R8).
		Expect(sec.Namespace).To(Equal(ns))
		Expect(sec.Data).To(HaveKey(keys.UserPasswordSecretKey))
		Expect(sec.Data[keys.UserPasswordSecretKey]).NotTo(BeEmpty())
		// Unowned, so `kubectl delete backuplocation` cannot garbage-collect the only key that
		// opens the user's repository.
		Expect(sec.OwnerReferences).To(BeEmpty())

		By("the condition tells the user the key is theirs to keep")
		Eventually(func(g Gomega) {
			l := getTenantLocationG(g, ns, name)
			cond := apimeta.FindStatusCondition(l.Status.Conditions, ConditionEncryptionValid)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(cond.Reason).To(Equal("GeneratedKey"))
		}, initTimeout, initPoll).Should(Succeed())
	})

	It("fails closed on a dangling repositoryPasswordSecretRef: no repository, no generated key", func() {
		const (
			ns   = "bl-nokey-ns"
			name = "dangling"
		)
		createTenantNamespace(ns)
		createTenantS3CredsSecret(ns, "dangling-s3")
		createTenantLocation(newTenantLocation(ns, name, "dangling-s3", "not-created-yet", "envtest-cluster"))

		Eventually(func(g Gomega) {
			l := getTenantLocationG(g, ns, name)
			cond := apimeta.FindStatusCondition(l.Status.Conditions, ConditionEncryptionValid)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(cond.Reason).To(Equal("PasswordSecretUnusable"))
			g.Expect(l.Status.Phase).To(Equal(locationPhaseDegraded))
		}, initTimeout, initPoll).Should(Succeed())

		// Encryption is fail-fast: nothing was provisioned, and — critically — no password was
		// generated behind the user's back. A generated one would come up healthy under a key the
		// user has never seen, and their own Secret would then not open the repository.
		Consistently(func(g Gomega) {
			err := k8sClient.Get(ctx, client.ObjectKey{Name: namespacedRepositoryName(ns, name)}, &cbv1.BackupRepository{})
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
			err = k8sClient.Get(ctx,
				client.ObjectKey{Namespace: ns, Name: keys.UserPasswordSecretName(name)}, &corev1.Secret{})
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
		}, consistentlyWindow, initPoll).Should(Succeed())
	})

	It("waits, rather than degrading, when no clusterID is declared and no default location exists", func() {
		const (
			ns   = "bl-noid-ns"
			name = "no-cluster-id"
		)
		createTenantNamespace(ns)
		createTenantS3CredsSecret(ns, "noid-s3")
		createTenantPasswordSecret(ns, "noid-key", "pw")
		createTenantLocation(newTenantLocation(ns, name, "noid-s3", "noid-key", ""))

		// Nothing here is the tenant's fault — the admin has not created a default DR location —
		// so the phase must say "waiting", not "degraded".
		Eventually(func(g Gomega) {
			l := getTenantLocationG(g, ns, name)
			g.Expect(l.Status.Phase).To(Equal(backupLocationPhaseWaitingClusterID))
			cond := apimeta.FindStatusCondition(l.Status.Conditions, ConditionReady)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Reason).To(Equal("NoClusterID"))
		}, initTimeout, initPoll).Should(Succeed())

		Consistently(func(g Gomega) {
			err := k8sClient.Get(ctx, client.ObjectKey{Name: namespacedRepositoryName(ns, name)}, &cbv1.BackupRepository{})
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
		}, consistentlyWindow, initPoll).Should(Succeed())
	})

	It("pins an inherited clusterID once, so a later change of default cannot re-point the repository", func() {
		const (
			ns       = "bl-sticky-ns"
			name     = "inheriting"
			defaultA = "bl-sticky-default"
		)
		createTenantNamespace(ns)
		createTenantS3CredsSecret(ns, "sticky-s3")
		createTenantPasswordSecret(ns, "sticky-key", "pw")

		createKEKSecret("kek-bl-sticky", generateAgeIdentity())
		createS3CredsSecret("s3-bl-sticky")
		registerRepoCleanup(defaultA)
		defaultLoc := newTestLocation(defaultA, "kek-bl-sticky", "s3-bl-sticky", true)
		defaultLoc.Spec.ClusterID = "cluster-alpha"
		createTestLocation(defaultLoc)

		createTenantLocation(newTenantLocation(ns, name, "sticky-s3", "sticky-key", ""))

		By("the location inherits the default's cluster ID into status")
		Eventually(func(g Gomega) {
			g.Expect(getTenantLocationG(g, ns, name).Status.ClusterID).To(Equal("cluster-alpha"))
		}, initTimeout, initPoll).Should(Succeed())

		By("un-defaulting that location does NOT move the tenant's repository path")
		Eventually(func(g Gomega) {
			var l cbv1.ClusterBackupLocation
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: defaultA}, &l)).To(Succeed())
			l.Spec.Default = false
			g.Expect(k8sClient.Update(ctx, &l)).To(Succeed())
		}, initTimeout, initPoll).Should(Succeed())

		// Were the cluster ID re-derived each pass, it would now go empty and the repository URL
		// with it — abandoning every snapshot already written under the old path.
		Consistently(func(g Gomega) {
			g.Expect(getTenantLocationG(g, ns, name).Status.ClusterID).To(Equal("cluster-alpha"))
		}, consistentlyWindow, initPoll).Should(Succeed())
	})

	It("refuses to adopt a repository that belongs to a different location", func() {
		const (
			ns       = "bl-collide-ns"
			name     = "collide"
			repoName = ns + "--" + name
		)
		createTenantNamespace(ns)
		createTenantS3CredsSecret(ns, "collide-s3")
		createTenantPasswordSecret(ns, "collide-key", "pw")

		// A repository already sitting on the deterministic name, back-linked to SOMEONE ELSE.
		// The "<ns>--<name>" mapping is not injective, and the cluster-scoped name space is shared
		// by every tenant — so this is reachable, and adopting it would write this tenant's
		// backups into the other tenant's repository, under the other tenant's key.
		squatter := &cbv1.BackupRepository{
			ObjectMeta: metav1.ObjectMeta{
				Name:   repoName,
				Labels: repositoryBackLinkLabels("someone-else-ns", "someone-else-loc"),
			},
		}
		Expect(k8sClient.Create(ctx, squatter)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(context.Background(), squatter) })

		createTenantLocation(newTenantLocation(ns, name, "collide-s3", "collide-key", "envtest-cluster"))

		Eventually(func(g Gomega) {
			l := getTenantLocationG(g, ns, name)
			cond := apimeta.FindStatusCondition(l.Status.Conditions, ConditionReady)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(cond.Reason).To(Equal("RepositoryUnavailable"))
			g.Expect(l.Status.RepositoryRef).To(BeEmpty(), "a refused repository must not be claimed")
		}, initTimeout, initPoll).Should(Succeed())

		By("the other location's repository is left exactly as it was")
		var after cbv1.BackupRepository
		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: repoName}, &after)).To(Succeed())
		Expect(after.Labels).To(HaveKeyWithValue(apiconst.LabelNamespace, "someone-else-ns"))
		Expect(after.Labels).To(HaveKeyWithValue(apiconst.LabelLocation, "someone-else-loc"))
	})

	It("deletes its repository on finalize but KEEPS the password Secret", func() {
		const (
			ns       = "bl-delete-ns"
			name     = "deleting"
			repoName = ns + "--" + name
		)
		createTenantNamespace(ns)
		createTenantS3CredsSecret(ns, "delete-s3")
		loc := newTenantLocation(ns, name, "delete-s3", "", "envtest-cluster")
		createTenantLocation(loc)

		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: repoName}, &cbv1.BackupRepository{})).To(Succeed())
			g.Expect(k8sClient.Get(ctx,
				client.ObjectKey{Namespace: ns, Name: keys.UserPasswordSecretName(name)}, &corev1.Secret{})).To(Succeed())
		}, initTimeout, initPoll).Should(Succeed())

		Expect(k8sClient.Delete(ctx, loc)).To(Succeed())

		By("the repository goes — GC could not have done it, so the finalizer must")
		Eventually(func(g Gomega) {
			err := k8sClient.Get(ctx, client.ObjectKey{Name: repoName}, &cbv1.BackupRepository{})
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
			err = k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &cbv1.BackupLocation{})
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
		}, initTimeout, initPoll).Should(Succeed())

		By("the password Secret stays: it is the only key that can still read what is in the bucket")
		var sec corev1.Secret
		Expect(k8sClient.Get(ctx,
			client.ObjectKey{Namespace: ns, Name: keys.UserPasswordSecretName(name)}, &sec)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(context.Background(), &sec) })
	})

	// The inverse of the spec this replaces. A user repository advertised [tenant, platform] when
	// spec.encryption.platformAccess was set — while nothing ever ran `restic key add`, so the
	// status described a key that did not exist. The field is gone (adr/0004, 2026-07-28
	// amendment): the platform has no mechanism to hold a slot on a user's repository, so the
	// only honest value is [tenant], and it must stay the only reachable one.
	It("never advertises a platform key slot on a user repository", func() {
		const (
			ns       = "bl-slots-ns"
			name     = "shared-access"
			repoName = ns + "--" + name
		)
		createTenantNamespace(ns)
		createTenantS3CredsSecret(ns, "slots-s3")
		createTenantPasswordSecret(ns, "slots-key", "pw")
		createTenantLocation(newTenantLocation(ns, name, "slots-s3", "slots-key", "envtest-cluster"))

		Eventually(func(g Gomega) {
			g.Expect(getRepositoryG(g, repoName).Status.KeySlots).
				To(Equal([]string{keySlotTenant}),
					"a platform slot here would be a status describing a key nobody ever created")
		}, initTimeout, initPoll).Should(Succeed())
	})
})

// The repository password Secret's data key is one contract across the whole system: what a user
// writes into their own Secret, and what the mover projects to a file for restic. They are
// declared in different packages (keys does not import mover, deliberately), so nothing but this
// assertion keeps them from drifting into two names for one thing.
var _ = Describe("repository password data key", func() {
	It("is the same string the mover projects", func() {
		Expect(keys.UserPasswordSecretKey).To(Equal(mover.ResticPasswordFileName))
	})
})
