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

// M5 — external sync (R28, spec/adr/0013-external-backup-sync.md, spec/08-testing-and-dod.md
// case 18) on real object storage.
//
// The claim external sync exists to make is not "the bytes arrived". It is that the SECOND COPY IS
// RE-ENCRYPTED: `restic copy` decrypts from the source and re-encrypts to the destination's own
// key, so the destination is an independent repository — usable with upstream restic under ITS key,
// and OPAQUE to the source's. That is why the whole feature was chosen over server-side object
// replication, which would have carried the source key into the secondary and broken client
// siloing (adr/0013's Context).
//
// A claim about keys can only be tested by trying the wrong one, and only a real repository on real
// object storage can answer. So this container asks restic directly, four times:
//
//	 the destination opens with the DESTINATION's DEK      → it must succeed
//	 the destination opens with the SOURCE's DEK           → it must FAIL
//	 the source opens with the DESTINATION's DEK           → it must FAIL
//	 `restic check` on the destination                     → it must pass
//
// The three other properties measured here are the ones a unit test can assert but not DEMONSTRATE:
// selectivity (a narrowed sync copies the selected namespace and leaves the others where they are),
// provenance (a copied snapshot records its source's ID in `original`, which is what makes the copy
// idempotent and Mirror sound), and Mirror itself (a snapshot forgotten at the source is forgotten
// at the destination on the next run — and nothing else is).
//
// Everything runs on an ISOLATED pair of repositories: a source under the standard prefix and a
// destination under the second one, both on a clusterID unique to this run. The Mirror scenario
// FORGETS a snapshot, and a forget aimed at the shared "dr" repository would be a mutation every
// later spec — including M1's — would have to live with. The behaviour under test is identical:
// the copy, the keys and the queue lanes do not know how many repositories exist.
//
// What is deliberately NOT here: cross-CREDENTIAL sync. Hetzner Object Storage credentials are
// project-wide, so every prefix and every bucket in this crucible opens with the same access key —
// there is no second identity to give the destination. That half of adr/0013 is proven in kind,
// where SeaweedFS provides a second identity scoped to the destination bucket alone
// (test/e2e/m5_externalsync_test.go). Neither harness can make both assertions; together they make
// case 18.

import (
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
)

// The sync image is the one deployment knob M5 adds, and the failure mode of getting it wrong is
// slow and mute: an ImagePullBackOff inside a Job nobody is watching, surfacing twenty minutes
// later as "the sync never completed". This check is outside the Ordered container below so it runs
// BEFORE anything provisions a repository, and reads as a deployment fault rather than a feature one.
var _ = Describe("M5 — external sync deployment", Label("m5"), func() {
	It("The operator was deployed with a real sync image, not the chart's placeholder digest", func() {
		By("Given the operator Deployment's own arguments")
		var deploys appsv1.DeploymentList
		Expect(k8s.List(ctx, &deploys, client.InNamespace(operatorNS),
			client.MatchingLabels{"app.kubernetes.io/name": "crystal-backup"})).To(Succeed())
		Expect(deploys.Items).NotTo(BeEmpty(), "no operator Deployment in %s", operatorNS)
		containers := deploys.Items[0].Spec.Template.Spec.Containers
		Expect(containers).NotTo(BeEmpty(), "the operator Deployment has no container")

		args := containers[0].Args
		flag := ""
		for _, a := range args {
			if strings.HasPrefix(a, "--sync-image=") {
				flag = strings.TrimPrefix(a, "--sync-image=")
			}
		}

		By("Then --sync-image names an image that can actually be pulled")
		Expect(flag).NotTo(BeEmpty(), "the operator was deployed without --sync-image (args=%v)", args)
		Expect(flag).NotTo(ContainSubstring(strings.Repeat("0", 64)),
			"--sync-image is still the chart's placeholder digest (%s) — deploy/deploy.sh must pass "+
				"sync.image.digest/tag the way it already passes the mover's", flag)
	})
})

var _ = Describe("M5 — external sync (restic copy, re-encrypted to the destination's own key)",
	Ordered, Label("m5"), func() {

		const (
			// The two locations of this scenario. Same clusterID, DIFFERENT prefix: that is what
			// makes them two repositories rather than one addressed twice.
			sourceLocation = "m5-sync-src"
			destLocation   = "m5-sync-dst"

			// Two seeded tenants, chosen for having no PVCs worth snapshotting: the runs below are
			// manifest-only, so what each contributes is exactly one namespace-tagged snapshot.
			// c-empty is the SELECTED namespace, c-edge the one that must stay behind.
			selectedNS = "c-empty"
			excludedNS = "c-edge"

			// A sync moves data through three Jobs (the copy, then one inventory per side) and, in
			// Mirror mode, a fourth. Generous rather than tight: the first copy also pulls the sync
			// image onto whichever node the Job lands on.
			syncTimeout = 20 * time.Minute
		)

		var (
			clusterID  string
			sourceRepo *cbv1.BackupRepository
			destRepo   *cbv1.BackupRepository

			sourceURL, destURL string
			sourceDEK, destDEK string
		)

		// runSync creates a one-shot ClusterBackupExternalSync (no schedule ⇒ it runs once and
		// stops), waits for it to complete, and returns it.
		//
		// A second sync is a second OBJECT, not a re-trigger of the first: "empty schedule means run
		// once, then never again on its own" is the controller's deliberate reading, and driving the
		// Mirror scenario by deleting and recreating is how an operator would actually re-run one.
		runSync := func(name string, namespaces []string) *cbv1.ClusterBackupExternalSync {
			GinkgoHelper()
			cs := &cbv1.ClusterBackupExternalSync{
				ObjectMeta: metav1.ObjectMeta{Name: name},
				Spec: cbv1.ClusterBackupExternalSyncSpec{
					SourceLocationRef:      cbv1.LocalObjectReference{Name: sourceLocation},
					DestinationLocationRef: cbv1.LocalObjectReference{Name: destLocation},
					Mode:                   cbv1.ExternalSyncModeMirror,
				},
			}
			if len(namespaces) > 0 {
				cs.Spec.Selection = &cbv1.ExternalSyncSelection{
					Namespaces: &cbv1.NamespaceSelector{MatchNames: namespaces},
				}
			}
			Expect(k8s.Create(ctx, cs)).To(Succeed(), "create ClusterBackupExternalSync %s", name)
			DeferCleanup(func() { _ = k8s.Delete(ctx, cs) })

			var got cbv1.ClusterBackupExternalSync
			Eventually(func(g Gomega) {
				g.Expect(k8s.Get(ctx, client.ObjectKey{Name: name}, &got)).To(Succeed())
				g.Expect(got.Status.Phase).To(Equal("Completed"),
					"sync %s is %q, not Completed — %s", name, got.Status.Phase,
					m5DescribeConditions(got.Status.Conditions))
			}, syncTimeout, 10*time.Second).Should(Succeed())
			return &got
		}

		BeforeAll(func() {
			m1RequireS3()
			m1EnsurePlatformSecrets()

			clusterID = m5ClusterIDFor("sync")
			sourceURL = m5RepoURL(m1S3Prefix, clusterID)
			destURL = m5RepoURL(m5SyncPrefix, clusterID)

			By("Given a source and a destination repository, each with its own platform DEK")
			sourceRepo = m5CreateClusterLocation(sourceLocation, clusterID, m1S3Prefix)
			destRepo = m5CreateClusterLocation(destLocation, clusterID, m5SyncPrefix)

			// Two locations under ONE cluster KEK still get two independent DEKs
			// (keys.DEKSecretName is per location). If these two ever came out equal, every
			// re-encryption assertion below would pass for the wrong reason.
			sourceDEK = m1UnwrapDEK(sourceLocation)
			destDEK = m1UnwrapDEK(destLocation)
			Expect(destDEK).NotTo(Equal(sourceDEK),
				"the two locations share a repository password; the re-encryption assertions would be vacuous")

			By("And three snapshots at the source: two for " + selectedNS + ", one for " + excludedNS)
			m5RunManifestBackup("m5-sync-seed-1-"+m5RunID, sourceLocation, selectedNS, excludedNS)
			m5RunManifestBackup("m5-sync-seed-2-"+m5RunID, sourceLocation, selectedNS)

			source := m5ResticSnapshotsOn(sourceURL, sourceDEK)
			Expect(m5SnapshotByNamespace(source, selectedNS)).To(HaveLen(2),
				"the source should hold two %s snapshots, got %v", selectedNS, m5SnapshotIDs(source))
			Expect(m5SnapshotByNamespace(source, excludedNS)).To(HaveLen(1),
				"the source should hold one %s snapshot, got %v", excludedNS, m5SnapshotIDs(source))
		})

		It("A narrowed sync copies only the selected namespace, and the copies are re-encrypted to the destination's own key", func() {
			By("When a Mirror sync copies namespace " + selectedNS + " from the source to the destination")
			cs := runSync("m5-sync-select-"+m5RunID, []string{selectedNS})

			By("Then the sync reports both copies present and nothing lagging")
			Expect(cs.Status.SnapshotsCopied).To(BeEquivalentTo(2),
				"expected both %s snapshots at the destination; status says %d (%s)",
				selectedNS, cs.Status.SnapshotsCopied, m5DescribeConditions(cs.Status.Conditions))
			Expect(cs.Status.LagSnapshots).To(BeEquivalentTo(0),
				"the destination is behind the source after a completed sync (lag=%d)", cs.Status.LagSnapshots)

			By("And the destination opens with ITS OWN key, holding exactly the selected namespace")
			dest := m5ResticSnapshotsOn(destURL, destDEK)
			Expect(dest).To(HaveLen(2),
				"the destination should hold exactly the two copied snapshots, got %v", m5SnapshotIDs(dest))
			Expect(m5SnapshotByNamespace(dest, excludedNS)).To(BeEmpty(),
				"the sync selected %s only, yet %s reached the destination — a narrowed selection that "+
					"widens is how a tenant excluded on purpose ends up in a secondary they were kept out of",
				selectedNS, excludedNS)

			By("And every copy records the SOURCE snapshot it came from (restic's `original`), " +
				"which is what makes the copy idempotent and Mirror sound")
			source := m5ResticSnapshotsOn(sourceURL, sourceDEK)
			sourceSelected := map[string]bool{}
			for _, s := range m5SnapshotByNamespace(source, selectedNS) {
				sourceSelected[s.ID] = true
			}
			for _, d := range dest {
				Expect(d.Original).NotTo(BeEmpty(),
					"destination snapshot %s has no `original` — it was not copied, it was written natively, "+
						"and Mirror must never treat such a snapshot as its own to remove", d.ID)
				Expect(sourceSelected).To(HaveKey(d.Original),
					"destination snapshot %s claims to come from %s, which is not a %s snapshot at the source (%v)",
					d.ID, d.Original, selectedNS, m5SnapshotIDs(source))
				Expect(d.ID).NotTo(Equal(d.Original),
					"a re-encrypted copy is content-addressed under the DESTINATION's key, so its ID cannot "+
						"equal the source's — identical IDs would mean a byte clone, which is the mechanism "+
						"adr/0013 rejected")
			}

			By("And the destination is a sound repository in its own right")
			out, ok := m1ResticRun(destURL, destDEK, "check")
			Expect(ok).To(BeTrue(), "`restic check` failed on the destination repository: %s", out)
		})

		It("The destination does NOT open with the source's key, nor the source with the destination's (case 18: the keys differ per repository)", func() {
			By("When the destination repository is opened with the SOURCE's DEK")
			out, ok := m5ResticOpens(destURL, sourceDEK)

			By("Then restic refuses it — the copy was re-encrypted, so the secondary is opaque to the primary's key")
			Expect(ok).To(BeFalse(),
				"the source's key opened the DESTINATION repository. That is the byte-clone behaviour "+
					"adr/0013 rejected: it would put the source's key into the secondary's silo. Output: %s", out)

			By("And symmetrically, the destination's DEK does not open the source")
			out, ok = m5ResticOpens(sourceURL, destDEK)
			Expect(ok).To(BeFalse(),
				"the destination's key opened the SOURCE repository — the two repositories are not "+
					"independently keyed. Output: %s", out)

			By("While each key still opens its own repository — so the refusals above are about the KEYS, " +
				"not about two unreachable buckets")
			out, ok = m5ResticOpens(sourceURL, sourceDEK)
			Expect(ok).To(BeTrue(), "the source's own key must open the source: %s", out)
			out, ok = m5ResticOpens(destURL, destDEK)
			Expect(ok).To(BeTrue(), "the destination's own key must open the destination: %s", out)
		})

		It("Mirror propagates a source forget to the destination, and removes nothing else", func() {
			By("Given the destination currently mirrors both source snapshots")
			before := m5ResticSnapshotsOn(destURL, destDEK)
			Expect(before).To(HaveLen(2), "expected the previous spec's two copies, got %v", m5SnapshotIDs(before))

			By("When one of the two source snapshots is forgotten at the source")
			source := m5SnapshotByNamespace(m5ResticSnapshotsOn(sourceURL, sourceDEK), selectedNS)
			Expect(source).To(HaveLen(2))
			// Forget the OLDER one and keep the newer, which is the shape a retention policy produces.
			// By explicit ID rather than by tag: a tag filter describes a scope, and a scope is exactly
			// what must not be at large in a spec that also asserts nothing else disappeared.
			forgotten, survivor := source[0], source[1]
			if survivor.Time.Before(forgotten.Time) {
				forgotten, survivor = survivor, forgotten
			}
			out, ok := m1ResticRun(sourceURL, sourceDEK, "forget", forgotten.ID)
			Expect(ok).To(BeTrue(), "`restic forget %s` failed at the source: %s", forgotten.ID, out)

			By("And a second Mirror sync runs over the same selection")
			cs := runSync("m5-sync-mirror-"+m5RunID, []string{selectedNS})
			Expect(cs.Status.SnapshotsCopied).To(BeEquivalentTo(1),
				"one source snapshot survives, so exactly one copy should remain accounted for; status says %d (%s)",
				cs.Status.SnapshotsCopied, m5DescribeConditions(cs.Status.Conditions))

			By("Then the destination has dropped the copy whose source is gone, and kept the other")
			after := m5ResticSnapshotsOn(destURL, destDEK)
			Expect(after).To(HaveLen(1),
				"Mirror should leave exactly the copy of the surviving source snapshot, got %v", m5SnapshotIDs(after))
			Expect(after[0].Original).To(Equal(survivor.ID),
				"the surviving destination snapshot came from %s, but the snapshot still live at the source "+
					"is %s — Mirror forgot the wrong one", after[0].Original, survivor.ID)

			By("And the destination is still a sound repository after the forget")
			out, ok = m1ResticRun(destURL, destDEK, "check")
			Expect(ok).To(BeTrue(), "`restic check` failed on the destination after Mirror's forget: %s", out)
		})

		It("A sync onto its own location is refused at admission (rule 9)", func() {
			// The cluster-plane half of case 18's admission requirement. Static
			// ValidatingAdmissionPolicy rather than a controller check on purpose (adr/0010): the
			// guard has to hold even while the operator is down.
			By("When a sync names one location as both its source and its destination")
			cs := &cbv1.ClusterBackupExternalSync{
				ObjectMeta: metav1.ObjectMeta{Name: "m5-sync-self-" + m5RunID},
				Spec: cbv1.ClusterBackupExternalSyncSpec{
					SourceLocationRef:      cbv1.LocalObjectReference{Name: sourceLocation},
					DestinationLocationRef: cbv1.LocalObjectReference{Name: sourceLocation},
					Mode:                   cbv1.ExternalSyncModeMirror,
				},
			}
			err := k8s.Create(ctx, cs)

			By("Then the API server rejects it")
			if err == nil {
				_ = k8s.Delete(ctx, cs)
			}
			Expect(err).To(HaveOccurred(),
				"a sync from a location to itself was ADMITTED; rule 9's policy (chart "+
					"admission.vap.enabled) is not in force on this cluster")
			Expect(err.Error()).To(ContainSubstring("must differ"))
		})

		It("A sync between two DIFFERENTLY-NAMED locations that address one repository is refused by the controller", func() {
			// The case admission cannot see. Rule 9 compares the two refs by NAME, and names are not
			// the identity that matters: an alias — a second location on the same bucket, prefix and
			// cluster ID — passes admission and would have restic open ONE repository as both ends
			// of the copy, duplicating every snapshot into itself under a Mirror that believes it is
			// tracking a separate source.
			//
			// Writing this spec is what found the gap: the controller's backstop compared the two
			// resolved BackupRepository NAMES, and a repository's name is derived from its
			// location's on either plane, so it only ever reproduced rule 9. It now compares the
			// resolved repository URL, which is what restic would actually open.
			By("Given a second location that addresses the SOURCE's repository under another name")
			const aliasLocation = "m5-sync-alias"
			Expect(k8s.Create(ctx, m5ClusterLocationObject(aliasLocation, clusterID, m1S3Prefix,
				cbv1.LocationModeStandard))).To(Succeed(), "create the aliasing location")
			DeferCleanup(func() { m1DeleteLocation(aliasLocation) })
			m1WaitRepositoryInitialized(aliasLocation)

			By("When a sync names the source and its alias as the two ends")
			cs := &cbv1.ClusterBackupExternalSync{
				ObjectMeta: metav1.ObjectMeta{Name: "m5-sync-alias-" + m5RunID},
				Spec: cbv1.ClusterBackupExternalSyncSpec{
					SourceLocationRef:      cbv1.LocalObjectReference{Name: sourceLocation},
					DestinationLocationRef: cbv1.LocalObjectReference{Name: aliasLocation},
					Mode:                   cbv1.ExternalSyncModeMirror,
				},
			}
			Expect(k8s.Create(ctx, cs)).To(Succeed(),
				"admission compares NAMES, and these two differ — the refusal under test is the controller's")
			DeferCleanup(func() { _ = k8s.Delete(ctx, cs) })

			By("Then it fails with SameRepository, and no copy is ever enqueued")
			Eventually(func(g Gomega) {
				var got cbv1.ClusterBackupExternalSync
				g.Expect(k8s.Get(ctx, client.ObjectKeyFromObject(cs), &got)).To(Succeed())
				g.Expect(got.Status.Phase).To(Equal("Failed"),
					"a copy onto its own repository must fail, phase is %q (%s)",
					got.Status.Phase, m5DescribeConditions(got.Status.Conditions))
				g.Expect(m5DescribeConditions(got.Status.Conditions)).To(ContainSubstring("SameRepository"))
			}, 5*time.Minute, 5*time.Second).Should(Succeed())

			By("And the source repository was not touched by it")
			source := m5ResticSnapshotsOn(sourceURL, sourceDEK)
			Expect(m5SnapshotByNamespace(source, selectedNS)).To(HaveLen(1),
				"the refused sync altered the source repository: %v", m5SnapshotIDs(source))
		})

		AfterAll(func() {
			// The repositories themselves stay in the bucket: they are this run's own paths (unique
			// clusterID), nothing else will meet them, and leaving them behind keeps a failed run's
			// evidence readable with nothing more than the DEK printed in the log. The CRs are
			// reaped by the DeferCleanups above.
			if sourceRepo != nil && destRepo != nil {
				_, _ = fmt.Fprintf(GinkgoWriter,
					"M5 sync repositories left in place for post-mortem: source=%s dest=%s\n",
					sourceRepo.Status.RepositoryURL, destRepo.Status.RepositoryURL)
			}
		})
	})
