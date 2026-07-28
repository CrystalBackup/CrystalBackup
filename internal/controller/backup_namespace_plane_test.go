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
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
	"github.com/CrystalBackup/CrystalBackup/internal/keys"
	"github.com/CrystalBackup/CrystalBackup/internal/mover"
)

// The namespace plane's EXECUTION path. The location and its repository are covered in
// backuplocation_controller_test.go; what these specs pin is that a Backup pointing at a
// namespaced BackupLocation runs through the very same machinery as a cluster-DR one, while
// reading its key and its credentials from the TENANT's namespace and nowhere else.
var _ = Describe("Backup on the namespace plane", func() {

	// seedTenantRepo brings a namespaced location all the way to an initialized repository and
	// returns the repository's name.
	seedTenantRepo := func(ns, locName, s3Secret string) string {
		GinkgoHelper()
		createTenantLocation(newTenantLocation(ns, locName, s3Secret, "", "envtest-cluster"))
		repoName := namespacedRepositoryName(ns, locName)
		simulateRepositoryInitialized(repoName)
		return repoName
	}

	// createTenantBackup creates a namespace-plane Backup: locationRef.kind=BackupLocation, and a
	// materialized spec.run, exactly as a BackupSchedule stamp will produce.
	createTenantBackup := func(ns, name, locName string) *cbv1.Backup {
		GinkgoHelper()
		off := false
		b := &cbv1.Backup{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: ns,
				Name:      name,
				Labels:    map[string]string{apiconst.LabelOrigin: apiconst.OriginNamespace},
			},
			Spec: cbv1.BackupSpec{
				LocationRef: cbv1.LocationReference{Kind: kindBackupLocation, Name: locName},
				Run:         &cbv1.BackupRunSpec{IncludeManifests: &off},
			},
		}
		Expect(k8sClient.Create(ctx, b)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(context.Background(), b) })
		return b
	}

	It("backs a tenant PVC up to the tenant's own repository, end to end", func() {
		const (
			ns      = "bnp-happy-ns"
			locName = "own-storage"
			run     = "bnp-happy-run"
			pvcName = "tenant-data"
		)
		createTenantNamespace(ns)
		createTenantS3CredsSecret(ns, "own-s3")
		createSourcePVC(ns, pvcName, "ceph-block")
		repoName := seedTenantRepo(ns, locName, "own-s3")
		createTenantBackup(ns, run, locName)

		jobName := waitForMoverJob(ns, run, pvcName)
		simulateMoverSucceeded(jobName, "node-t", mover.MoverResult{
			OK: true, Operation: string(mover.OpBackup), SnapshotID: "tenant-snap-1", SizeBytes: 2048, AddedBytes: 512,
		})

		Eventually(func(g Gomega) {
			b := getBackupG(g, ns, run)
			g.Expect(b.Status.Phase).To(Equal("Completed"))
			vol := volumeByPVC(b, pvcName)
			g.Expect(vol).NotTo(BeNil())
			g.Expect(vol.SnapshotID).To(Equal("tenant-snap-1"))
			g.Expect(apimeta.IsStatusConditionTrue(b.Status.Conditions, ConditionReady)).To(BeTrue())
		}, initTimeout, initPoll).Should(Succeed())

		// The run went to the TENANT's repository, not the shared cluster one.
		Expect(repoName).To(Equal(ns + "--" + locName))
	})

	It("reads S3 credentials from the TENANT's namespace, never a same-named platform Secret", func() {
		const (
			ns         = "bnp-creds-ns"
			locName    = "creds-storage"
			run        = "bnp-creds-run"
			pvcName    = "creds-data"
			sharedName = "bnp-colliding-creds"
		)
		createTenantNamespace(ns)
		createSourcePVC(ns, pvcName, "ceph-block")

		// The SAME Secret name in both namespaces, with different values. This is the whole test:
		// if the credentials namespace were defaulted to the operator's, the tenant's data would
		// be shipped to whatever bucket the PLATFORM credentials reach — and nothing would error,
		// because a Secret of that name exists there too.
		platformCreds := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: suiteOperatorNamespace, Name: sharedName},
			Data: map[string][]byte{
				mover.SecretKeyAWSAccessKeyID:     []byte("PLATFORM-KEY"),
				mover.SecretKeyAWSSecretAccessKey: []byte("PLATFORM-SECRET"),
			},
		}
		Expect(k8sClient.Create(ctx, platformCreds)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(context.Background(), platformCreds) })
		createTenantS3CredsSecret(ns, sharedName) // values: tenant-access-key / tenant-secret-key

		seedTenantRepo(ns, locName, sharedName)
		createTenantBackup(ns, run, locName)

		jobName := waitForMoverJob(ns, run, pvcName)

		var creds corev1.Secret
		Expect(k8sClient.Get(ctx,
			client.ObjectKey{Namespace: suiteOperatorNamespace, Name: jobName}, &creds)).To(Succeed())
		Expect(string(creds.Data[mover.SecretKeyAWSAccessKeyID])).To(Equal("tenant-access-key"),
			"the mover must carry the TENANT's S3 credentials")
		Expect(string(creds.Data[mover.SecretKeyAWSSecretAccessKey])).To(Equal("tenant-secret-key"))

		// And the restic password is the tenant's generated key, not a platform DEK.
		var pw corev1.Secret
		Expect(k8sClient.Get(ctx,
			client.ObjectKey{Namespace: ns, Name: keys.UserPasswordSecretName(locName)}, &pw)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(context.Background(), &pw) })
		Expect(creds.Data[mover.SecretKeyResticPassword]).
			To(Equal(pw.Data[keys.UserPasswordSecretKey]),
				"the mover's restic password must be the tenant's own key")
	})

	It("resolves the location in its OWN namespace: a same-named one next door is not reachable", func() {
		const (
			ownerNS   = "bnp-confine-owner"
			strangeNS = "bnp-confine-stranger"
			locName   = "shared-location-name"
			run       = "bnp-confine-run"
		)
		createTenantNamespace(ownerNS)
		createTenantNamespace(strangeNS)

		// A perfectly good, initialized location — in someone ELSE's namespace.
		createTenantS3CredsSecret(ownerNS, "confine-s3")
		seedTenantRepo(ownerNS, locName, "confine-s3")

		// The Backup lives next door and names the same location name. Structural confinement
		// means the lookup is scoped to its own namespace, so this must not resolve — not
		// "resolve and then get rejected by a check", which a future refactor could drop.
		createTenantBackup(strangeNS, run, locName)

		Eventually(func(g Gomega) {
			b := getBackupG(g, strangeNS, run)
			cond := apimeta.FindStatusCondition(b.Status.Conditions, ConditionReady)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(cond.Reason).To(Equal("LocationNotFound"))
			g.Expect(cond.Message).To(ContainSubstring(strangeNS),
				"the message must name the namespace the lookup was scoped to")
		}, initTimeout, initPoll).Should(Succeed())
	})

	It("waits for the location to pin its cluster ID before writing anything", func() {
		const (
			ns      = "bnp-noid-ns"
			locName = "unpinned"
			run     = "bnp-noid-run"
		)
		createTenantNamespace(ns)
		createTenantS3CredsSecret(ns, "noid-s3")
		// No spec.clusterID and (in this spec) no default ClusterBackupLocation to inherit from,
		// so status.clusterID never gets pinned and the repository path is undefined.
		createTenantLocation(newTenantLocation(ns, locName, "noid-s3", "", ""))
		createTenantBackup(ns, run, locName)

		Eventually(func(g Gomega) {
			b := getBackupG(g, ns, run)
			cond := apimeta.FindStatusCondition(b.Status.Conditions, ConditionReady)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Reason).To(Equal("LocationNotReady"))
		}, initTimeout, initPoll).Should(Succeed())
	})
})
