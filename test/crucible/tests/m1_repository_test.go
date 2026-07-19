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

import (
	"fmt"
	"os"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/keys"
	"github.com/CrystalBackup/CrystalBackup/internal/restic"
)

// M1 acceptance — a 1:1 realisation of test/crucible/features/m1_repository.feature
// ("Shared cluster-DR repository lifecycle"). Each Scenario is one It; each Gherkin
// step is one By() so the readable report reproduces the feature prose.
//
// The three scenarios share one expensive fact — a provisioned, initialized shared
// repository behind the default ClusterBackupLocation "dr" — so they run Ordered with
// the location created once in BeforeAll. Assertions speak the pinned M1 contract
// (BackupRepository.status, the crystal-dek-<loc> envelope, restic.RepoURL, the
// Reachable/Ready/MultipleDefaults conditions) and read the object store through the
// crucible's independent restic oracle, never trusting a CrystalBackup CR to grade
// itself. They MUST compile now and WILL fail live until the M1 controllers exist.
var _ = Describe("M1 — shared cluster-DR repository lifecycle", Label("m1"), Ordered, func() {
	// secondName is the second, conflicting default location of the last scenario.
	// Declared here so BeforeAll (leftover cleanup) and AfterAll (teardown) see it too.
	const secondName = "dr2"

	BeforeAll(func() {
		m1SkipIfNoS3()
		m1EnsurePlatformSecrets()

		// A ClusterBackupLocation is cluster-scoped and survives a namespace wipe, so a
		// previous aborted run may have left "dr"/"dr2" behind. Clear them and wait for
		// the name to free up before (re)creating the shared default location.
		m1DeleteLocation(m1LocationName)
		m1DeleteLocation(secondName)
		Eventually(func() error {
			var l cbv1.ClusterBackupLocation
			err := k8s.Get(ctx, client.ObjectKey{Name: m1LocationName}, &l)
			switch {
			case apierrors.IsNotFound(err):
				return nil
			case err != nil:
				return err
			default:
				return fmt.Errorf("ClusterBackupLocation %q still present (terminating?)", m1LocationName)
			}
		}, 2*time.Minute, 3*time.Second).Should(Succeed())

		m1CreateLocation(m1LocationName, true)
	})

	AfterAll(func() {
		// Best-effort teardown of the two locations we created; leave the platform KEK/S3
		// Secrets and the DEK/repository in place (they are shared, reusable crucible state
		// — dropping the KEK would strand every DEK wrapped under it).
		m1DeleteLocation(secondName)
		m1DeleteLocation(m1LocationName)
	})

	It("A ClusterBackupLocation provisions one initialized shared repository", func() {
		By("Given an S3 bucket reachable at the crucible's S3 endpoint")
		By(`And a platform KEK (an age X25519 identity) stored as a Secret in "crystal-backup-system"`)
		// Both Background givens are established by BeforeAll (m1SkipIfNoS3 +
		// m1EnsurePlatformSecrets); the KEK identity is now in m1KEKIdentity.

		By(`When I create a ClusterBackupLocation "dr" for the bucket with clusterID "crucible"`)
		// Created once in BeforeAll (shared, Ordered). Assert it landed as configured.
		var loc cbv1.ClusterBackupLocation
		Expect(k8s.Get(ctx, client.ObjectKey{Name: m1LocationName}, &loc)).To(Succeed())
		Expect(loc.Spec.ClusterID).To(Equal(m1ClusterID))

		By(`Then a cluster-scoped BackupRepository is created for "dr"`)
		Eventually(func(g Gomega) {
			repo, ok := m1FindRepository(m1LocationName)
			g.Expect(ok).To(BeTrue(), "no BackupRepository backs location %q yet", m1LocationName)
			g.Expect(repo.Namespace).To(BeEmpty(), "a BackupRepository is cluster-scoped (no namespace)")
			g.Expect(repo.Status.Scope).To(Equal("Cluster"), "repository for a ClusterBackupLocation is Cluster-scoped")
			g.Expect(repo.Status.Location.Name).To(Equal(m1LocationName))
		}, 5*time.Minute, 5*time.Second).Should(Succeed())

		By("And the BackupRepository reaches Initialized=true within 5 minutes")
		repo := m1WaitRepositoryInitialized(m1LocationName)

		By(`And its status.repositoryURL is "s3:<endpoint>/<bucket>/<prefix>/crucible"`)
		wantURL := restic.RepoURL(os.Getenv("S3_ENDPOINT"), os.Getenv("S3_BUCKET"), m1S3Prefix, m1ClusterID)
		Expect(repo.Status.RepositoryURL).To(Equal(wantURL))

		By(`And exactly one Secret "crystal-dek-dr" exists in "crystal-backup-system"`)
		dekName := keys.DEKSecretName(m1LocationName)
		Expect(dekName).To(Equal("crystal-dek-dr"))
		var dek corev1.Secret
		Eventually(func(g Gomega) {
			var secrets corev1.SecretList
			g.Expect(k8s.List(ctx, &secrets, client.InNamespace(operatorNS))).To(Succeed())
			matches := 0
			for i := range secrets.Items {
				if secrets.Items[i].Name == dekName {
					matches++
					dek = secrets.Items[i]
				}
			}
			g.Expect(matches).To(Equal(1),
				"want exactly one Secret %q in %s, found %d", dekName, operatorNS, matches)
		}, 2*time.Minute, 5*time.Second).Should(Succeed())

		By(`And the Secret "crystal-dek-dr" holds only the age-wrapped DEK, never a plaintext password`)
		Expect(dek.Data).To(HaveLen(1), "DEK Secret must carry only the wrapped DEK, no other data key")
		Expect(dek.Data).To(HaveKey(keys.DEKSecretKey))
		wrapped := dek.Data[keys.DEKSecretKey]
		Expect(wrapped).NotTo(BeEmpty())
		Expect(string(wrapped)).To(ContainSubstring("age-encryption.org/v1"),
			"the stored DEK must be an age file (wrapped), not a plaintext password")
		plaintextDEK := m1UnwrapDEK(m1LocationName)
		Expect(plaintextDEK).NotTo(BeEmpty())
		Expect(string(wrapped)).NotTo(Equal(plaintextDEK),
			"the persisted blob must be the ciphertext, never the plaintext DEK")

		By("And the ClusterBackupLocation reports condition Reachable=true and Ready=true")
		Eventually(func(g Gomega) {
			var l cbv1.ClusterBackupLocation
			g.Expect(k8s.Get(ctx, client.ObjectKey{Name: m1LocationName}, &l)).To(Succeed())
			g.Expect(apimeta.IsStatusConditionTrue(l.Status.Conditions, "Reachable")).
				To(BeTrue(), "location %q: condition Reachable is not true", m1LocationName)
			g.Expect(apimeta.IsStatusConditionTrue(l.Status.Conditions, "Ready")).
				To(BeTrue(), "location %q: condition Ready is not true", m1LocationName)
		}, 5*time.Minute, 5*time.Second).Should(Succeed())
	})

	It("The shared repository is initialized exactly once, even under concurrent reconciles", func() {
		By(`Given a ClusterBackupLocation "dr" that has provisioned its repository`)
		m1WaitRepositoryInitialized(m1LocationName)
		// The DEK is the repository password and is minted exactly once; capturing it now
		// lets us prove a racing reconcile did not re-init the repo under a fresh key.
		dekBefore := m1UnwrapDEK(m1LocationName)

		By("When the operator reconciles the location repeatedly and concurrently")
		// Nudge the location in a tight burst to enqueue a stream of reconciles, then force
		// a cold operator restart so the init-once guard is re-evaluated from a fresh
		// process racing the already-provisioned repository (the classic init race).
		for i := 0; i < 8; i++ {
			Eventually(func() error {
				var l cbv1.ClusterBackupLocation
				if err := k8s.Get(ctx, client.ObjectKey{Name: m1LocationName}, &l); err != nil {
					return err
				}
				if l.Annotations == nil {
					l.Annotations = map[string]string{}
				}
				l.Annotations["crucible.test/reconcile-nudge"] = fmt.Sprintf("%d", i)
				return k8s.Update(ctx, &l)
			}, 30*time.Second, time.Second).Should(Succeed())
		}
		m1DeleteOperatorPod()

		By(`Then "restic cat config" succeeds against the repository with the platform DEK`)
		// m1ResticExec opens the repo with the unwrapped platform DEK; a valid single config
		// prints its JSON (with a "version"), a wrong password / missing repo prints "Fatal".
		catConfig := m1ResticExec(m1LocationName, "cat", "config")
		Expect(catConfig).To(ContainSubstring(`"version"`),
			"`restic cat config` did not return a repository config: %s", catConfig)
		Expect(catConfig).NotTo(ContainSubstring("Fatal"),
			"`restic cat config` reported a fatal error: %s", catConfig)

		By("And the repository was initialized exactly once, with no duplicate or corrupt config from an init race")
		// restic check verifies structural integrity — a duplicate/corrupt config from a
		// double-init would fail here rather than print the all-clear.
		check := m1ResticExec(m1LocationName, "check")
		Expect(check).To(ContainSubstring("no errors were found"),
			"`restic check` did not confirm an intact repository: %s", check)
		// The repo is still the same initialized repo...
		repo := m1WaitRepositoryInitialized(m1LocationName)
		Expect(repo.Status.RepositoryURL).To(
			Equal(restic.RepoURL(os.Getenv("S3_ENDPOINT"), os.Getenv("S3_BUCKET"), m1S3Prefix, m1ClusterID)))
		// ...and its password never changed, so init did not run a second time under a new DEK.
		Expect(m1UnwrapDEK(m1LocationName)).To(Equal(dekBefore),
			"the platform DEK changed — the repository was re-initialized, not initialized once")
	})

	It("Only one ClusterBackupLocation may be the default", func() {
		By(`Given a default ClusterBackupLocation "dr"`)
		var dr cbv1.ClusterBackupLocation
		Expect(k8s.Get(ctx, client.ObjectKey{Name: m1LocationName}, &dr)).To(Succeed())
		Expect(dr.Spec.Default).To(BeTrue(), "the shared location %q must be the default", m1LocationName)

		By(`When I create a second ClusterBackupLocation "dr2" also marked default`)
		// M2 adds a single-default admission WEBHOOK (adr/0010): a second default is rejected
		// at ADMISSION — the strongest enforcement of "only one default". When the webhook is
		// disabled or fails open (failurePolicy: Ignore), the create lands and the controller's
		// MultipleDefaults condition is the backstop, asserted below. Either satisfies rule 4.
		if err := m1TryCreateLocation(secondName, true); err != nil {
			Expect(err.Error()).To(ContainSubstring("default"),
				"the second-default create was rejected, but not for being a duplicate default: %v", err)
			return
		}

		By("Then one of the two locations reports condition MultipleDefaults=true")
		By("And the operator never silently treats both as the default")
		Eventually(func(g Gomega) {
			flagged := 0
			readyDefaults := 0
			for _, name := range []string{m1LocationName, secondName} {
				var l cbv1.ClusterBackupLocation
				g.Expect(k8s.Get(ctx, client.ObjectKey{Name: name}, &l)).To(Succeed())
				multi := apimeta.IsStatusConditionTrue(l.Status.Conditions, "MultipleDefaults")
				ready := apimeta.IsStatusConditionTrue(l.Status.Conditions, "Ready")
				if multi {
					flagged++
				}
				if ready {
					readyDefaults++
				}
				// A location known to be a conflicting default must not simultaneously be
				// advertised as a healthy (Ready) default — that would be treating it as the
				// default silently, in defiance of the surfaced conflict.
				g.Expect(multi && ready).To(BeFalse(),
					"location %q reports MultipleDefaults yet also Ready — silently treated as the default", name)
			}
			g.Expect(flagged).To(BeNumerically(">=", 1),
				"neither location reports MultipleDefaults=true after a second default was created")
			g.Expect(readyDefaults).To(BeNumerically("<=", 1),
				"both default locations report Ready — the operator is silently treating both as the default")
		}, 3*time.Minute, 5*time.Second).Should(Succeed())
	})
})
