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

// M5 — right to erasure (R21, spec/adr/0009-shared-cluster-repo-tag-tenancy.md): the PHYSICAL
// removal of a namespace's data from a shared repository, `restic forget` followed by `prune` on
// the repository's exclusive lane.
//
// Erasure is the one operation in this system with no undo, and "the deletion happened" is the only
// thing a GDPR request actually asks. Neither half can be established anywhere but on real object
// storage: a fake or an envtest can only show that the operator issued the commands, and issuing
// them is not the claim. So this container erases for real and then RE-SCANS the repository with
// upstream restic, which is the same evidence a data-protection officer would ask for.
//
// Three properties, and the negative ones carry the weight:
//
//   - IT IS SCOPED. Erasing namespace X removes exactly X's snapshots. A tenant that shares the
//     repository loses nothing, and the repository is still sound afterwards (`restic check`) —
//     a prune that corrupted the neighbours would be a far worse outcome than a failed erasure.
//   - IT IS TWO-STEP (R23). An absent confirmation PARKS the object rather than rejecting it, so
//     an operator sees what will go before typing the identity back; a WRONG confirmation is
//     denied by admission before it ever reaches a controller.
//   - IT REFUSES RATHER THAN LIES ON IMMUTABLE LOCATIONS. Object Lock forbids the deletion, and
//     "reported success while the objects are still there" is the worst possible answer to an
//     erasure request. The erasure parks in Blocked (adr/0005).
//
// It runs on its OWN repository. Everything here is destructive to the repository it touches, and
// the shared "dr" one is read by every other milestone's specs.

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/restic"
)

var _ = Describe("M5 — right to erasure (forget + prune, verified by re-scanning the repository)",
	Ordered, Label("m5"), func() {

		const (
			locationName          = "m5-erase"
			immutableLocationName = "m5-erase-immutable"

			// erasedNS is the namespace whose snapshots must physically go; keptNS shares the
			// repository and must come through untouched. Both are manifest-only contributors, so
			// each run adds exactly one namespace-tagged snapshot.
			erasedNS = "c-empty"
			keptNS   = "c-edge"

			// forget + prune are two mover Jobs on the exclusive lane, behind whatever else that
			// lane is holding.
			erasureTimeout = 20 * time.Minute
		)

		var (
			clusterID string
			repoURL   string
			dek       string
		)

		// waitErasurePhase waits for a ClusterErasure to report a phase, surfacing its conditions
		// when it does not — an erasure that stalls says why on the object itself.
		waitErasurePhase := func(name, phase string, timeout time.Duration) cbv1.ClusterErasure {
			GinkgoHelper()
			var got cbv1.ClusterErasure
			Eventually(func(g Gomega) {
				g.Expect(k8s.Get(ctx, client.ObjectKey{Name: name}, &got)).To(Succeed())
				g.Expect(got.Status.Phase).To(Equal(phase),
					"ClusterErasure %s is %q, not %q — %s",
					name, got.Status.Phase, phase, m5DescribeConditions(got.Status.Conditions))
			}, timeout, 5*time.Second).Should(Succeed())
			return got
		}

		BeforeAll(func() {
			m1SkipIfNoS3()
			m1EnsurePlatformSecrets()

			clusterID = m5ClusterIDFor("erase")
			repoURL = m5RepoURL(m1S3Prefix, clusterID)

			By("Given a repository of this scenario's own, holding two tenants' snapshots")
			m5CreateClusterLocation(locationName, clusterID, m1S3Prefix)
			dek = m1UnwrapDEK(locationName)

			// Two runs for the erased namespace and one for its neighbour: the count the erasure
			// reports is then a number that could have been wrong (2, not "all" and not 1).
			m5RunManifestBackup("m5-erase-seed-1-"+m5RunID, locationName, erasedNS, keptNS)
			m5RunManifestBackup("m5-erase-seed-2-"+m5RunID, locationName, erasedNS)

			snaps := m5ResticSnapshotsOn(repoURL, dek)
			Expect(m5SnapshotByNamespace(snaps, erasedNS)).To(HaveLen(2),
				"expected two %s snapshots to erase, got %v", erasedNS, m5SnapshotIDs(snaps))
			Expect(m5SnapshotByNamespace(snaps, keptNS)).To(HaveLen(1),
				"expected one %s snapshot to survive, got %v", keptNS, m5SnapshotIDs(snaps))
		})

		It("Parks an unconfirmed erasure and names what it would remove (R23)", func() {
			By("When an erasure is created with no confirmation")
			er := &cbv1.ClusterErasure{
				ObjectMeta: metav1.ObjectMeta{Name: "m5-erase-gate-" + m5RunID},
				Spec: cbv1.ClusterErasureSpec{
					LocationRef: cbv1.LocalObjectReference{Name: locationName},
					Target:      cbv1.ErasureTarget{Namespace: erasedNS},
				},
			}
			Expect(k8s.Create(ctx, er)).To(Succeed(),
				"an EMPTY confirmation must be admitted — R23 parks the erasure, it does not reject it")
			DeferCleanup(func() { _ = k8s.Delete(ctx, er) })

			By("Then it parks in AwaitingConfirmation, telling the operator the scope and the word to type")
			got := waitErasurePhase(er.Name, "AwaitingConfirmation", 5*time.Minute)
			conds := m5DescribeConditions(got.Status.Conditions)
			Expect(conds).To(ContainSubstring(erasedNS),
				"the parked erasure must name its target so an operator can read it before confirming: %s", conds)
			Expect(conds).To(ContainSubstring("PERMANENTLY"),
				"the parked erasure must say what it will do: %s", conds)

			By("And nothing has been erased while it waits")
			snaps := m5ResticSnapshotsOn(repoURL, dek)
			Expect(m5SnapshotByNamespace(snaps, erasedNS)).To(HaveLen(2),
				"an unconfirmed erasure removed data: %v", m5SnapshotIDs(snaps))
		})

		It("Refuses a confirmation that does not name the target, at admission", func() {
			By("When an erasure is created with a confirmation naming the WRONG namespace")
			er := &cbv1.ClusterErasure{
				ObjectMeta: metav1.ObjectMeta{Name: "m5-erase-wrong-" + m5RunID},
				Spec: cbv1.ClusterErasureSpec{
					LocationRef:  cbv1.LocalObjectReference{Name: locationName},
					Target:       cbv1.ErasureTarget{Namespace: erasedNS},
					Confirmation: keptNS,
				},
			}
			err := k8s.Create(ctx, er)

			By("Then the API server rejects it — the guard is static admission, so it holds even with the operator down")
			if err == nil {
				_ = k8s.Delete(ctx, er)
			}
			Expect(err).To(HaveOccurred(),
				"a confirmation naming %q for a target of %q was ADMITTED; rule 1's ClusterErasure policy "+
					"(chart admission.vap.enabled) is not in force on this cluster", keptNS, erasedNS)
			Expect(err.Error()).To(ContainSubstring("confirmation"))
		})

		It("Physically removes exactly the confirmed namespace, leaving its neighbour and the repository sound", func() {
			By("When a confirmed erasure of " + erasedNS + " runs")
			er := &cbv1.ClusterErasure{
				ObjectMeta: metav1.ObjectMeta{Name: "m5-erase-run-" + m5RunID},
				Spec: cbv1.ClusterErasureSpec{
					LocationRef:  cbv1.LocalObjectReference{Name: locationName},
					Target:       cbv1.ErasureTarget{Namespace: erasedNS},
					Confirmation: erasedNS,
				},
			}
			Expect(k8s.Create(ctx, er)).To(Succeed())
			DeferCleanup(func() { _ = k8s.Delete(ctx, er) })

			By("Then it completes, and its record says how many snapshots it removed")
			got := waitErasurePhase(er.Name, "Completed", erasureTimeout)
			// The count is written BEFORE the forget, because afterwards the evidence is gone. It is
			// the compliance record, so it has to be a number that could have been wrong.
			Expect(got.Status.SnapshotsForgotten).To(BeEquivalentTo(2),
				"the erasure claims %d snapshots; %s contributed exactly 2 (%s)",
				got.Status.SnapshotsForgotten, erasedNS, m5DescribeConditions(got.Status.Conditions))

			By("And a re-scan of the repository with upstream restic finds none of them")
			// The re-scan is the assertion. The controller reporting Completed only says it ran the
			// commands; this says the snapshots are not there.
			after := m5ResticSnapshotsOn(repoURL, dek)
			Expect(m5SnapshotByNamespace(after, erasedNS)).To(BeEmpty(),
				"snapshots of the erased namespace are still in the repository: %v", m5SnapshotIDs(after))

			By("And the neighbouring tenant sharing the repository lost nothing")
			Expect(m5SnapshotByNamespace(after, keptNS)).To(HaveLen(1),
				"erasing %s took %s's snapshot with it — a shared repository's tag scoping is the only thing "+
					"standing between one tenant's erasure request and another tenant's backups (adr/0009): %v",
				erasedNS, keptNS, m5SnapshotIDs(after))
			for _, s := range after {
				ns, ok := restic.TagValue(s.Tags, restic.TagKeyNamespace)
				Expect(ok && ns == erasedNS).To(BeFalse(),
					"snapshot %s still carries namespace=%s after the erasure", s.ID, erasedNS)
			}

			By("And the repository is still sound after the prune repacked it")
			// forget and prune run as ONE queued op precisely so an erasure cannot report success
			// between them, with the tenant's bytes still in the packs. A prune that damaged the
			// surviving packs would be the other way this could go wrong, and only a real check
			// against real object storage can tell the two apart.
			out, ok := m1ResticRun(repoURL, dek, "check")
			Expect(ok).To(BeTrue(), "`restic check` failed after the erasure's prune: %s", out)
		})

		It("Is Blocked, not Completed, on an Immutable location", func() {
			By("Given an Immutable location, where object lock forbids deletion")
			// Never initialized on purpose: the controller decides Blocked from the location's MODE,
			// before it ever looks at a repository, so provisioning one would only cost a mover Job.
			// (Immutable window-repo rotation itself lands with M8 — adr/0005; what is under test
			// here is only that erasure refuses rather than pretends.)
			Expect(k8s.Create(ctx, m5ClusterLocationObject(immutableLocationName,
				m5ClusterIDFor("erase-immutable"), m1S3Prefix, cbv1.LocationModeImmutable))).
				To(Succeed(), "create the Immutable location")
			DeferCleanup(func() { m1DeleteLocation(immutableLocationName) })

			By("When a fully confirmed erasure targets it")
			er := &cbv1.ClusterErasure{
				ObjectMeta: metav1.ObjectMeta{Name: "m5-erase-blocked-" + m5RunID},
				Spec: cbv1.ClusterErasureSpec{
					LocationRef:  cbv1.LocalObjectReference{Name: immutableLocationName},
					Target:       cbv1.ErasureTarget{Namespace: erasedNS},
					Confirmation: erasedNS,
				},
			}
			Expect(k8s.Create(ctx, er)).To(Succeed())
			DeferCleanup(func() { _ = k8s.Delete(ctx, er) })

			By("Then it reports Blocked and says nothing has been erased")
			got := waitErasurePhase(er.Name, "Blocked", 5*time.Minute)
			conds := m5DescribeConditions(got.Status.Conditions)
			Expect(conds).To(ContainSubstring("Immutable"),
				"a blocked erasure must say WHY it is blocked: %s", conds)
			Expect(got.Status.SnapshotsForgotten).To(BeEquivalentTo(0),
				"a Blocked erasure reported %d snapshots forgotten — on an Immutable location nothing is "+
					"removed, and a non-zero count would be the report of a deletion that did not happen",
				got.Status.SnapshotsForgotten)
		})
	})
