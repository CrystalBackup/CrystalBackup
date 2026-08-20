//go:build crucible

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

package crucible

// M5 — the namespace plane (R3/R5, spec/adr/0004-encryption-key-management.md and its 2026-07-28
// amendment): a namespace user backs up their OWN namespace to their OWN object storage under
// their OWN key, alongside cluster DR and independent of it.
//
// The milestone's headline guarantee is negative, and negative guarantees are the ones worth
// testing on real infrastructure: THE PLATFORM'S ACCESS ENDS WHEN THE USER'S KEY DOES. `platformAccess`
// was specified, never implemented, and dropped during M5 — so the guarantee is bought by the
// mechanism not existing rather than by a check that could be switched off. A claim of that shape
// cannot be verified by reading the operator's code (that only shows what today's code does); it is
// verified by asking the REPOSITORY how many keys it holds.
//
// Three things are established here, in order:
//
//   1. the round trip works — a real Ceph PVC reaches the user's own repository, and the user's own
//      password opens it;
//   2. the repository holds exactly ONE key slot, and the operator namespace holds no wrapped DEK
//      for it — there is no platform key, durable or otherwise;
//   3. deleting the user's password Secret takes the platform's access away immediately, while the
//      DATA is untouched and still opens with the key the user kept.
//
// Point 3's second half matters as much as the first. "The platform lost access" is only a good
// property if it is not a euphemism for "the backup is gone": the same act must leave the user in
// sole possession of readable backups, which is the reversibility R8 promises.

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
	"github.com/CrystalBackup/CrystalBackup/internal/keys"
	"github.com/CrystalBackup/CrystalBackup/internal/restic"
)

var _ = Describe("M5 — namespace plane (the user's own repository, under the user's own key)",
	Ordered, Label("m5"), func() {

		const (
			locationName = "my-own-storage"
			keySecret    = "my-own-restic-key"
			pvcName      = "tenant-data"

			// A real CSI snapshot plus a restic upload of the seeded volume, on a cluster that may be
			// running other milestones' movers at the same time.
			backupTimeout = 20 * time.Minute
		)

		// Per campaign, like every other run name in the suite. This one is already insulated by
		// m5ClusterIDFor("nsplane"), which puts the tenant's repository on a path no previous
		// campaign wrote to — but the insulation is the repository's, not the name's, and the next
		// person to copy this block will not be copying the repository.
		backupName := crucibleRunName("m5-np-run")

		var (
			clusterID    string
			userPassword string
			repoURL      string
		)

		BeforeAll(func() {
			m1RequireS3()

			clusterID = m5ClusterIDFor("nsplane")
			repoURL = m5RepoURL(m5TenantPrefix, clusterID)

			By("Given a namespace that owns its S3 credentials and its restic key")
			ensureNamespace(m5TenantNS)
			m5EnsureTenantS3Secret(m5TenantNS)
			userPassword = m5CreateUserKeySecret(m5TenantNS, keySecret)

			By("And a BackupLocation of their own, on their own prefix")
			m5CreateTenantLocation(m5TenantNS, locationName, clusterID, keySecret)

			By("And a real volume with data on it")
			startPVCConsumer(m5TenantNS, pvcName, m4StorageClass)
		})

		AfterAll(func() {
			// The tenant namespace is torn down through the strict helper on purpose: a namespace
			// that never finishes deleting is the shape M3.2's worst bug took, and a spec that
			// created a PVC, snapshotted it and exposed it is exactly where such a leak would land.
			deleteNamespaceAndWaitGone(m5TenantNS, 15*time.Minute)
		})

		It("Backs a tenant volume up to the tenant's own repository, readable with the tenant's own password", func() {
			By("When the user runs a Backup against their own location")
			backup := m5RunNamespaceBackup(m5TenantNS, backupName, locationName, backupTimeout)

			By("Then it completes with the volume snapshotted into their repository")
			Expect(backup.Status.Phase).To(Equal("Completed"),
				"the namespace-plane Backup did not complete: %s", m5DescribeConditions(backup.Status.Conditions))
			var volume *cbv1.VolumeStatus
			for i := range backup.Status.Volumes {
				if backup.Status.Volumes[i].Pvc == pvcName {
					volume = &backup.Status.Volumes[i]
				}
			}
			Expect(volume).NotTo(BeNil(), "the Backup reported no result for PVC %s", pvcName)
			Expect(volume.SnapshotID).NotTo(BeEmpty(),
				"PVC %s has no snapshot ID (phase=%q) — nothing reached the user's repository",
				pvcName, volume.Phase)

			By("And the repository opens with the USER's password — the one they hold, not one the platform minted")
			snaps := m5ResticSnapshotsOn(repoURL, userPassword)
			Expect(snaps).NotTo(BeEmpty(), "the user's repository holds no CrystalBackup snapshot")

			By("And the snapshots carry this namespace's identity and nothing else's")
			found := false
			for _, s := range snaps {
				ns, ok := restic.TagValue(s.Tags, restic.TagKeyNamespace)
				Expect(ok).To(BeTrue(), "snapshot %s carries no namespace tag", s.ID)
				Expect(ns).To(Equal(m5TenantNS),
					"the user's own repository holds a snapshot from namespace %q — a namespaced location's "+
						"repository must hold that namespace's data and nothing else", ns)
				if pvc, ok := restic.TagValue(s.Tags, restic.TagKeyPVC); ok && pvc == pvcName {
					found = true
					Expect(s.ID).To(Equal(volume.SnapshotID),
						"the repository's snapshot for %s (%s) is not the one the Backup reported (%s)",
						pvcName, s.ID, volume.SnapshotID)
				}
			}
			Expect(found).To(BeTrue(),
				"no snapshot tagged pvc=%s in the user's repository; got %v", pvcName, m5SnapshotIDs(snaps))
		})

		It("Holds exactly one key slot, and the platform holds no wrapped key for it (adr/0004 amendment)", func() {
			By("When the repository's key slots are counted with upstream restic")
			slots := m5ResticKeySlots(repoURL, userPassword)

			By("Then there is exactly ONE — the user's")
			// This is the amendment's whole content, asked of the artefact rather than of the code.
			// A second slot would be a password living in crystal-backup-system that keeps working
			// after the user rotates their key or deletes their Secret — and because removing a
			// restic key slot does not rotate the master key, one they could never take back.
			Expect(slots).To(Equal(1),
				"the user's repository has %d key slots; a namespace-plane repository must have exactly one, "+
					"and it must be the user's (adr/0004, 2026-07-28 amendment: no operator key slot on a "+
					"user repository)", slots)

			By("And the operator namespace holds no wrapped DEK for this location")
			// The cluster plane wraps a platform DEK under the cluster KEK and keeps it here. The
			// namespace plane must produce nothing of the kind: if such a Secret existed, the
			// single key slot above would only mean the platform had not USED its copy yet.
			var dek corev1.Secret
			err := k8s.Get(ctx, client.ObjectKey{
				Namespace: operatorNS, Name: keys.DEKSecretName(locationName),
			}, &dek)
			Expect(apierrors.IsNotFound(err)).To(BeTrue(),
				"a platform DEK Secret %s/%s exists for a NAMESPACED location (err=%v) — the platform is "+
					"holding a key to a user repository", operatorNS, keys.DEKSecretName(locationName), err)
		})

		It("Loses platform access the moment the user deletes their password Secret — while the data stays readable to them", func() {
			By("Given the user's repository currently holds their backup")
			before := m5ResticSnapshotsOn(repoURL, userPassword)
			Expect(before).NotTo(BeEmpty())

			By("When the user deletes their password Secret")
			Expect(k8s.Delete(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: keySecret, Namespace: m5TenantNS},
			})).To(Succeed(), "delete the user's repository password Secret")

			By("Then the location reports its key unusable, naming the Secret the user removed")
			Eventually(func(g Gomega) {
				var loc cbv1.BackupLocation
				g.Expect(k8s.Get(ctx, client.ObjectKey{Namespace: m5TenantNS, Name: locationName}, &loc)).To(Succeed())
				g.Expect(m5DescribeConditions(loc.Status.Conditions)).To(
					ContainSubstring(m5TenantNS+"/"+keySecret),
					"the location does not report the missing key: %s", m5DescribeConditions(loc.Status.Conditions))
			}, 5*time.Minute, 5*time.Second).Should(Succeed())

			By("And a new backup cannot run — there is no platform key to fall back on")
			// The important half of this assertion is what does NOT happen: the operator does not
			// generate a replacement password. EnsureUserPassword refuses to fall back when a
			// reference was given, so a dangling reference is an error and never a new key — which
			// is what stops a "recovered" backup from being written under a key the user never chose.
			denied := &cbv1.Backup{
				ObjectMeta: metav1.ObjectMeta{
					Name:      backupName + "-after-key-deletion",
					Namespace: m5TenantNS,
					Labels:    map[string]string{apiconst.LabelOrigin: apiconst.OriginNamespace},
				},
				Spec: cbv1.BackupSpec{
					LocationRef: cbv1.LocationReference{Kind: "BackupLocation", Name: locationName},
					Run:         &cbv1.BackupRunSpec{IncludeManifests: ptr.To(true)},
				},
			}
			Expect(k8s.Create(ctx, denied)).To(Succeed())
			DeferCleanup(func() { _ = k8s.Delete(ctx, denied) })

			Eventually(func(g Gomega) {
				var got cbv1.Backup
				g.Expect(k8s.Get(ctx, client.ObjectKeyFromObject(denied), &got)).To(Succeed())
				g.Expect(m5DescribeConditions(got.Status.Conditions)).To(
					ContainSubstring(m5TenantNS+"/"+keySecret),
					"the Backup does not name the missing key Secret: %s", m5DescribeConditions(got.Status.Conditions))
				g.Expect(got.Status.Phase).NotTo(Equal("Completed"),
					"a backup completed after the user's key was deleted — something opened the repository "+
						"without the key the user took away")
			}, 5*time.Minute, 5*time.Second).Should(Succeed())

			By("And no replacement password Secret was generated in the user's namespace")
			var generated corev1.Secret
			err := k8s.Get(ctx, client.ObjectKey{
				Namespace: m5TenantNS, Name: keys.UserPasswordSecretName(locationName),
			}, &generated)
			Expect(apierrors.IsNotFound(err)).To(BeTrue(),
				"a repository password was generated to replace the one the user deleted (%s/%s) — a dangling "+
					"reference must be an error, never a new key",
				m5TenantNS, keys.UserPasswordSecretName(locationName))

			By("While the backups themselves are intact and still open with the key the user kept")
			// The user's copy of the password is the ONLY one left, and it still works: what ended is
			// the platform's access, not the backup. Anything weaker than this would make the
			// previous assertions indistinguishable from having destroyed the repository.
			after := m5ResticSnapshotsOn(repoURL, userPassword)
			Expect(after).To(HaveLen(len(before)),
				"the user's snapshots changed when their key Secret was deleted: %v became %v",
				m5SnapshotIDs(before), m5SnapshotIDs(after))
			out, ok := m1ResticRun(repoURL, userPassword, "check")
			Expect(ok).To(BeTrue(), "`restic check` failed on the user's repository: %s", out)
		})

		It("Left no residual snapshot objects behind", func() {
			// The namespace-plane path runs the SAME exposure machinery as cluster DR — a
			// VolumeSnapshot in the tenant namespace, a static VS/VSC pair and a clone PVC in the
			// operator namespace — so it inherits the same leak surface, and gets the same check.
			// backupName is the namespace-plane Backup the first It ran. On this plane there is no
			// ClusterBackup, so its objects are attributable only through crystalbackup.io/backup —
			// which m1ResidueOwnedBy reads first, for exactly this case.
			m1AssertNoResidualSnapshotObjects([]string{backupName}, m5TenantNS)
		})
	})
