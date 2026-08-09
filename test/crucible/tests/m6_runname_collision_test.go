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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
	"github.com/CrystalBackup/CrystalBackup/internal/restic"
)

// ---------------------------------------------------------------------------
// M6 — a run must never report success over data it did not write.
//
// A ClusterBackup fans one child Backup, named after the run, into every matched
// namespace. Deciding "my child already exists" from a bare Get on (namespace, run
// name) was wrong, because the name is a COORDINATE and not an identity: a discovery
// projection, an earlier same-named run's terminal Backup, and the namespace plane's
// own stamped Backup all satisfy it. The run then skipped the namespace outright and
// aggregated the occupant's Completed volumes as its own — namespacesSucceeded up,
// pvcsSucceeded up, phase Completed, over snapshots written days or weeks earlier.
// The only observable difference was an addedBytes of zero, and nothing alerted on it.
//
// This scenario reproduces the vector that needs no unusual action beyond a reused
// name — GitOps prune-and-recreate, `kubectl replace --force`, a convenience name
// like "pre-upgrade" used twice. It runs the real product path on real Rook-Ceph RBD:
//
//	run #1 (honest)   ─▶ writes a data snapshot,   addedBytes > 0
//	   volume rewritten with different content
//	run #2 (same name)─▶ MUST fail, write nothing, and count nothing
//
// The assertion that matters is not that a failure is recorded. It is that the second
// run does NOT report success, and that the repository gained no snapshot to justify
// one. Both are checked, because a green phase over an unchanged repository is
// precisely the shape of the defect.
//
// Like the fidelity gate beside it, this spec cannot self-disable: no enable flag, no
// conditional Skip(), no tolerance. A missing bucket FAILS — a collision check that
// quietly does nothing still reports green, which is the failure mode this whole file
// exists to make impossible.
// ---------------------------------------------------------------------------

var _ = Describe("M6 — a re-used run name fails loudly instead of reporting a false success",
	Label("m6"), Ordered, func() {
		const (
			collideNS  = "m6-runname"
			collidePVC = "payload"
			capacity   = "1Gi"
		)

		// THE ONE SPEC IN THE SUITE THAT WANTS A COLLISION — and precisely why its run name must
		// still be unique per campaign.
		//
		// The collision this scenario measures is BUILT, here, in this run: an honest run #1 writes
		// a snapshot, then m6RecreateClusterBackup deletes the ClusterBackup and re-creates it under
		// the same name, leaving run #1's child Backup on the coordinate. That is the whole
		// reproduction, and it is self-contained.
		//
		// A fixed name imported a SECOND, uninvited collision from the campaign before: last month's
		// "m6-runname-collide" snapshots, projected back by discovery, met run #1 — the honest one,
		// which must succeed for anything below to mean anything — and made it fail. When it did get
		// through, `ConsistOf(firstSnapshotID)` at the end saw last month's snapshot beside this
		// month's and failed on the count. Both are the fixture leaking, not the product.
		//
		// So: unique per campaign, collided deliberately within it. A spec that needs a collision
		// must MANUFACTURE one, never inherit it — an inherited one proves nothing about the code,
		// only that the bucket is old.
		collideRun := crucibleRunName("m6-runname-collide")

		// firstRunUID is the identity of the honest run. The second run gets a fresh one, which is
		// exactly what makes the distinction exact rather than heuristic.
		var firstRunUID string
		var firstSnapshotID string

		BeforeAll(func() {
			m6RequireS3()
			m1EnsurePlatformSecrets()

			By("Given the shared cluster-DR repository")
			var loc cbv1.ClusterBackupLocation
			if apierrors.IsNotFound(k8s.Get(ctx, client.ObjectKey{Name: m1LocationName}, &loc)) {
				m1CreateLocation(m1LocationName, true)
			}
			m1WaitRepositoryInitialized(m1LocationName)

			By("And a Rook-Ceph RBD volume holding real data")
			m2SeedVolume(collideNS, collidePVC, "ceph-block", capacity)

			// The collision below is manufactured, so the starting position must be clean. Asserted
			// rather than assumed: when this file's run name was fixed, the repository handed it a
			// second collision from a previous campaign and the failures landed several minutes
			// later, on assertions about the product. This one names the fixture instead.
			By("And nothing already occupies this run's coordinate — the collision must be ours to build")
			var preexisting cbv1.Backup
			Expect(apierrors.IsNotFound(k8s.Get(ctx,
				client.ObjectKey{Namespace: collideNS, Name: collideRun}, &preexisting))).To(BeTrue(),
				"a Backup already sits at %s/%s before this run created anything (projected=%q). The run "+
					"name is not unique to this campaign: the shared repository is never emptied, and "+
					"discovery projects an older run's snapshots straight back onto the coordinate.",
				collideNS, collideRun, preexisting.Annotations[apiconst.AnnotationProjected])
			Expect(m6SnapshotIDsForRun(collideNS, collidePVC, collideRun)).To(BeEmpty(),
				"the repository already holds data snapshots tagged run=%s — an inherited collision "+
					"proves nothing about the operator, only that the bucket outlived the cluster", collideRun)

			By("When an honest run backs it up")
			m6RecreateClusterBackup(collideRun, collideNS)
			cb := m1WaitClusterBackupTerminal(collideRun, 20*time.Minute)
			Expect(cb.Status.Phase).To(Equal("Completed"),
				"the FIRST run must succeed, or this scenario proves nothing about the second: %s",
				m6CollideDescribe(cb))
			Expect(cb.Status.PVCsSucceeded).To(Equal(int32(1)))
			Expect(cb.Status.AddedBytes).To(BeNumerically(">", 0),
				"an honest run always moves bytes; a run that moved none is the collision signature")
			firstRunUID = string(cb.UID)

			snap, ok := m1CascadeDataSnapshot(m1ResticSnapshots(m1LocationName), collideNS, collidePVC, collideRun)
			Expect(ok).To(BeTrue(), "the honest run must have written a data snapshot for %s/%s", collideNS, collidePVC)
			firstSnapshotID = snap.ID
		})

		AfterAll(func() {
			_ = k8s.Delete(ctx, &cbv1.ClusterBackup{ObjectMeta: metav1.ObjectMeta{Name: collideRun}})
			// Children are label-linked, never owned, so they do not cascade.
			_ = k8s.DeleteAllOf(ctx, &cbv1.Backup{}, client.InNamespace(collideNS),
				client.MatchingLabels{apiconst.LabelOrigin: apiconst.OriginCluster})
			_ = k8s.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: collideNS}})
		})

		It("fails the re-created run instead of adopting the earlier run's result", func() {
			By("rewriting the volume, so a second honest run would have new bytes to move")
			ok, log := m2VolumeJob(collideNS, collidePVC, "m6-rewrite", `set -e
cd /data
rm -rf alpha beta MANIFEST.sha256 MANIFEST.copy
mkdir -p gamma
for i in 1 2 3 4; do head -c $((i * 2048)) /dev/urandom > gamma/rewritten$i.bin; done
find . -type f | sort | xargs sha256sum > REWRITTEN.sha256
sync`)
			Expect(ok).To(BeTrue(), "rewriting %s/%s must succeed:\n%s", collideNS, collidePVC, log)

			By("And the earlier run's child Backup still occupying the coordinate")
			var occupant cbv1.Backup
			Expect(k8s.Get(ctx, client.ObjectKey{Namespace: collideNS, Name: collideRun}, &occupant)).To(Succeed(),
				"the child of run #1 must survive the run record; it is a restore point, not a log line")
			Expect(occupant.Annotations[apiconst.AnnotationParentUID]).To(Equal(firstRunUID),
				"the child must carry the UID of the run that created it")

			By("When a ClusterBackup of the SAME name is re-created")
			second := m6RecreateClusterBackup(collideRun, collideNS)
			Expect(string(second.UID)).NotTo(Equal(firstRunUID),
				"a re-created object always gets a fresh UID — that is what makes the check exact")
			cb := m1WaitClusterBackupTerminal(collideRun, 20*time.Minute)

			By("Then it FAILS, and counts nothing it did not write")
			Expect(cb.Status.Phase).To(Equal("Failed"),
				"the re-created run announced a success over the earlier run's snapshots: %s", m6CollideDescribe(cb))
			Expect(cb.Status.NamespacesMatched).To(Equal(int32(1)))
			Expect(cb.Status.NamespacesSucceeded).To(Equal(int32(0)),
				"a namespace this run never backed up must not be counted as succeeded: %s", m6CollideDescribe(cb))
			// Blocked, not failed: nothing of this run's ran in that namespace, so there is no
			// backup of its own whose failure it could report. The phase above is still Failed —
			// the namespace went unprotected — but the counter now says which of the two happened.
			Expect(cb.Status.NamespacesBlocked).To(Equal(int32(1)))
			Expect(cb.Status.NamespacesFailed).To(Equal(int32(0)),
				"a namespace this run never backed up is blocked, not a failed backup: %s", m6CollideDescribe(cb))
			Expect(cb.Status.PVCsSucceeded).To(Equal(int32(0)),
				"the occupant's volumes belong to run #1: %s", m6CollideDescribe(cb))
			Expect(cb.Status.AddedBytes).To(Equal(int64(0)))

			By("And it says what happened and what to do about it")
			Expect(cb.Status.Failures).NotTo(BeEmpty(), "a collision must leave a diagnosable record")
			Expect(cb.Status.Failures[0].Namespace).To(Equal(collideNS))
			Expect(cb.Status.Failures[0].Message).To(ContainSubstring("RunNameCollision"))
			Expect(cb.Status.Failures[0].Message).To(ContainSubstring("did not write"))

			// Scoped to the namespace's DATA snapshots on purpose: the run-level cluster-manifests
			// capture is the run's other half and legitimately writes its own snapshot even when
			// every namespace collided. What must not have moved is the tenant's volume.
			By("And the repository holds no NEW data snapshot for the volume — which is why success was a lie")
			Expect(m6SnapshotIDsForRun(collideNS, collidePVC, collideRun)).To(ConsistOf(firstSnapshotID),
				"the second run wrote nothing (correct — it collided); the defect was reporting Completed anyway")

			By("And the earlier run's child was never mutated or re-stamped by the second run")
			var after cbv1.Backup
			Expect(k8s.Get(ctx, client.ObjectKey{Namespace: collideNS, Name: collideRun}, &after)).To(Succeed())
			Expect(after.Annotations[apiconst.AnnotationParentUID]).To(Equal(firstRunUID),
				"the second run must never claim an object it did not create")
		})
	})

// m6SnapshotIDsForRun reads the shared repository through the independent restic oracle and returns
// the IDs of the DATA snapshots at the exact coordinate (namespace, pvc, run).
//
// Scoped to DATA on purpose: the run-level cluster-manifests capture is the run's other half and
// legitimately writes its own snapshot even when every namespace collided. What must not have moved
// is the tenant's volume.
//
// Used twice, and the second use is the point: once BEFORE anything runs, to prove the coordinate
// starts empty, and once after, to prove the collided run added nothing to it. The same measurement
// answers "is my fixture clean?" and "did the product write?" — so a dirty fixture cannot be read as
// a product defect.
func m6SnapshotIDsForRun(namespace, pvc, run string) []string {
	GinkgoHelper()
	var ids []string
	for _, s := range m1ResticSnapshots(m1LocationName) {
		kind, _ := restic.TagValue(s.Tags, restic.TagKeyKind)
		ns, _ := restic.TagValue(s.Tags, restic.TagKeyNamespace)
		gotRun, _ := restic.TagValue(s.Tags, restic.TagKeyRun)
		gotPVC, _ := restic.TagValue(s.Tags, restic.TagKeyPVC)
		if kind == restic.KindData && ns == namespace && gotRun == run && gotPVC == pvc {
			ids = append(ids, s.ID)
		}
	}
	return ids
}

// m6RecreateClusterBackup deletes any ClusterBackup of this name, waits for it to be gone, and
// creates a fresh one over the given namespace. This IS the reproduction: GitOps prune-and-recreate
// and `kubectl replace --force` do exactly this, and the new object carries a new UID while the
// previous run's child Backups (restore points, deliberately not owned) stay where they are.
func m6RecreateClusterBackup(name, namespace string) *cbv1.ClusterBackup {
	GinkgoHelper()
	_ = k8s.Delete(ctx, &cbv1.ClusterBackup{ObjectMeta: metav1.ObjectMeta{Name: name}})
	Eventually(func() error {
		err := k8s.Get(ctx, client.ObjectKey{Name: name}, &cbv1.ClusterBackup{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		return fmt.Errorf("ClusterBackup %s is still present", name)
	}, 5*time.Minute, 2*time.Second).Should(Succeed())

	cb := m1RunClusterBackup(name, m1LocationName,
		cbv1.NamespaceSelector{MatchNames: []string{namespace}})
	Eventually(func(g Gomega) {
		g.Expect(k8s.Get(ctx, client.ObjectKey{Name: name}, cb)).To(Succeed())
		g.Expect(string(cb.UID)).NotTo(BeEmpty())
	}, time.Minute, 2*time.Second).Should(Succeed())
	return cb
}

// m6CollideDescribe renders the counters a false success moves, so a red line names the lie rather
// than only the phase that carried it.
func m6CollideDescribe(cb *cbv1.ClusterBackup) string {
	msg := fmt.Sprintf("phase=%s matched=%d succeeded=%d failed=%d blocked=%d pvcsSucceeded=%d addedBytes=%d",
		cb.Status.Phase, cb.Status.NamespacesMatched, cb.Status.NamespacesSucceeded,
		cb.Status.NamespacesFailed, cb.Status.NamespacesBlocked, cb.Status.PVCsSucceeded,
		cb.Status.AddedBytes)
	for _, f := range cb.Status.Failures {
		msg += fmt.Sprintf("\n  failure[%s]: %s", f.Namespace, f.Message)
	}
	return msg
}
