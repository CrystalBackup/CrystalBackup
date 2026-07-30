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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/status"
)

// ---------------------------------------------------------------------------
// M6 acceptance — the RESTORE-FIDELITY GATE (spec/90-roadmap.md §M6, delta 14).
//
// This is the beta bar for 0.6. A backup tool whose restores have not been proven
// to give back exactly what was taken has no value, whatever else it does well —
// so this scenario takes a corpus engineered to be HARD to restore faithfully,
// runs it through the real product path on real Rook-Ceph RBD, and compares the
// two sides property by property.
//
//	source volume  ──ClusterBackup──▶  shared restic repo  ──ClusterRestore──▶  fresh PVC
//	      │                                                                       │
//	   manifest (before)  ────────────── diffed in Go ────────────────  manifest (after)
//
// Why a ClusterRestore into a namespace that does not exist yet, rather than an
// in-place Restore: the target PVC is created from the snapshot's own tags and
// formatted fresh, so every byte and every attribute in the "after" manifest was
// put there by the restore. Restoring over the source volume would make this
// scenario dishonest — an xattr, an ACL or a mode that the restore failed to
// re-apply would still be present from before, and the diff would come back green.
//
// Why the corpus contains what it contains: see m6_helpers_test.go, where each
// block of the seed says which real failure mode it catches. In short — file
// content is the least interesting half. What breaks in practice is the metadata:
// setuid bits cleared by a chown that ran after the chmod, xattrs and POSIX ACLs
// dropped because nothing ever asserted them, a sparse file restored dense (every
// checksum still matches, and the volume quietly fills up), hard links restored as
// independent copies, directory mtimes rewritten by the restore's own writes.
//
// Two properties of this file are load-bearing and must survive future edits:
//
//   - It cannot self-disable. No enable flag, no conditional Skip(), no tunable
//     tolerance. Missing S3 config, a missing tool, a truncated manifest — each
//     FAILS. The alternative was tried elsewhere in this suite and the result was
//     a check that skipped itself in every published run for four milestones.
//   - It does not shrink to stay green. restic#5543 (deterministic content
//     corruption when restoring large data sets to Rook-Ceph, open upstream) is
//     precisely what the content facet is pointed at. If it fires here, the right
//     response is a red line naming the corrupted byte range — not a smaller corpus.
// ---------------------------------------------------------------------------

var _ = Describe("M6 — restore-fidelity gate on Rook-Ceph RBD", Label("m6"), Ordered, func() {
	const (
		sourceNS   = "m6-fidelity"
		restoredNS = "m6-fidelity-restored"
		pvcName    = "corpus"
		capacity   = "8Gi"
	)

	// Per CAMPAIGN, not per milestone. These two were fixed strings, and that is what made the gate
	// red on a cluster whose only fault was pointing at the bucket a previous campaign had used:
	// discovery projected the older "m6-fidelity-src" snapshots into the namespace as a Completed
	// Backup before this spec created anything, and the fan-out — correctly — refused to write over
	// a coordinate it did not own. The run below wants a NEW restore point, never an existing one,
	// so it must never ask for a coordinate the repository already knows.
	var (
		runName   = crucibleRunName("m6-fidelity-src")
		restoreCR = crucibleRunName("m6-fidelity-restore")
	)

	// The two manifests and the classified diff between them. Computed once in BeforeAll; every
	// It below is a pure assertion over one facet of the same measurement, so a broken facet costs
	// one red line and the others still report what they found.
	var (
		pre, post map[string]*m6Entry
		deltas    []m6Delta
	)

	BeforeAll(func() {
		m6RequireS3()
		m1EnsurePlatformSecrets()

		By("Given the shared cluster-DR repository")
		var loc cbv1.ClusterBackupLocation
		if apierrors.IsNotFound(k8s.Get(ctx, client.ObjectKey{Name: m1LocationName}, &loc)) {
			m1CreateLocation(m1LocationName, true)
		}
		m1WaitRepositoryInitialized(m1LocationName)

		By("And a Rook-Ceph RBD volume seeded with the fidelity corpus")
		m6SeedCorpus(sourceNS, pvcName, capacity)

		By("measuring the source: one manifest record per path, content + every restorable attribute")
		pre = m6CaptureManifest(sourceNS, pvcName, "pre")

		By("backing the namespace up into the shared repository")
		var existing cbv1.ClusterBackup
		if apierrors.IsNotFound(k8s.Get(ctx, client.ObjectKey{Name: runName}, &existing)) {
			m1RunClusterBackup(runName, m1LocationName,
				cbv1.NamespaceSelector{MatchNames: []string{sourceNS}})
		}
		cb := m1WaitClusterBackupTerminal(runName, 45*time.Minute)
		Expect(cb.Status.Phase).To(Equal("Completed"),
			"the source backup must complete before fidelity means anything (phase=%q)", cb.Status.Phase)

		By("restoring the repository coordinate into a namespace that does not exist yet")
		cr := &cbv1.ClusterRestore{
			ObjectMeta: metav1.ObjectMeta{Name: restoreCR},
			Spec: cbv1.ClusterRestoreSpec{
				Source: cbv1.ClusterRestoreSource{
					LocationRef: cbv1.LocalObjectReference{Name: m1LocationName},
					Namespace:   sourceNS,
					Backup:      runName,
				},
				Target: cbv1.ClusterRestoreTarget{
					Namespace:       restoredNS,
					CreateNamespace: true,
				},
				Mode:         cbv1.RestoreModeRecreate,
				Confirmation: restoredNS,
			},
		}
		Expect(k8s.Create(ctx, cr)).To(Succeed(), "create ClusterRestore %s", restoreCR)

		done := m2WaitClusterRestoreTerminal(restoreCR, 45*time.Minute)
		Expect(done.Status.Phase).To(Equal(string(status.RestorePhaseCompleted)),
			"the restore must complete (phase=%q, restoredVolumes=%d)", done.Status.Phase, done.Status.RestoredVolumes)
		Expect(done.Status.RestoredVolumes).To(Equal(int32(1)))

		By("the restored PVC is a FRESH volume of the original shape — nothing pre-existing to hide behind")
		var claim corev1.PersistentVolumeClaim
		Expect(k8s.Get(ctx, client.ObjectKey{Namespace: restoredNS, Name: pvcName}, &claim)).To(Succeed())
		got := claim.Spec.Resources.Requests[corev1.ResourceStorage]
		Expect(got.Cmp(resource.MustParse(capacity))).To(BeZero(),
			"capacity must come from the pvcsize tag, got %s", got.String())
		Expect(claim.Spec.StorageClassName).NotTo(BeNil())
		Expect(*claim.Spec.StorageClassName).To(Equal("ceph-block"),
			"the gate is specified against Rook-Ceph — a restore onto anything else proves nothing here")

		By("measuring the restored volume with the identical generator")
		post = m6CaptureManifest(restoredNS, pvcName, "post")

		deltas = m6Diff(pre, post)
	})

	AfterAll(func() {
		_ = k8s.Delete(ctx, &cbv1.ClusterRestore{ObjectMeta: metav1.ObjectMeta{Name: restoreCR}})
		_ = k8s.Delete(ctx, &cbv1.ClusterBackup{ObjectMeta: metav1.ObjectMeta{Name: runName}})
		// The restored namespace is entirely the restore's output: if the transplant left a
		// finalizer nothing will release, this is where it strands the claim forever (M3.2, S8).
		deleteNamespaceAndWaitGone(restoredNS, 10*time.Minute)
		deleteNamespaceAndWaitGone(sourceNS, 10*time.Minute)
		m1AssertNoResidualSnapshotObjects(sourceNS, restoredNS)
		m2AssertNoResidualRestoreObjects()
	})

	// ── the measurement itself ────────────────────────────────────────────────

	It("compares source and restore by per-file manifest, not by file count", func() {
		Expect(pre).NotTo(BeEmpty(), "the source manifest is empty — nothing was measured")
		Expect(post).NotTo(BeEmpty(), "the restored manifest is empty — nothing was measured")
		// A count match is emphatically NOT the assertion — every facet below is. This spec exists
		// so the report states the size of the evidence the other lines rest on.
		Expect(len(pre)).To(BeNumerically(">=", 200),
			"the corpus shrank to %d entries: the gate is only as strong as what it seeds", len(pre))
		AddReportEntry("corpus", fmt.Sprintf("%d source entries, %d restored entries, %d deviation(s)",
			len(pre), len(post), len(deltas)))
	})

	It("gives every path back, and invents none", func() {
		found := m6DeltasFor(deltas, m6FacetPresence)
		Expect(found).To(BeEmpty(),
			"the restored tree does not have the same shape as the source.\n%s\nsource paths: %d, restored paths: %d",
			m6Report(found), len(m6SortedPaths(pre)), len(m6SortedPaths(post)))
	})

	It("gives every byte back — per-file digests, including a 384 MiB file spanning several restic packs (restic#5543)", func() {
		found := m6DeltasFor(deltas, m6FacetContent)
		Expect(found).To(BeEmpty(),
			"RESTORED CONTENT DIFFERS FROM THE SOURCE. This is the failure mode restic#5543 describes "+
				"(deterministic corruption at an offset inside a file when restoring to Rook-Ceph); the "+
				"byte ranges below are the regions that differ, at %d-byte granularity.\n%s",
			m6ChunkBytes, m6Report(found))
	})

	It("keeps modes, setuid/setgid/sticky bits and numeric ownership", func() {
		found := m6DeltasFor(deltas, m6FacetPermissions)
		Expect(found).To(BeEmpty(),
			"permissions or ownership did not survive the restore. A cleared setuid/setgid bit usually "+
				"means a chown ran after the chmod; a reset uid/gid means the ownership was never "+
				"re-applied at all.\n%s", m6Report(found))
	})

	It("keeps extended attributes on files and on directories", func() {
		found := m6DeltasFor(deltas, m6FacetXattrs)
		Expect(found).To(BeEmpty(),
			"extended attributes did not survive the restore. The mover stores xattrs unconditionally "+
				"and passes NO xattr filter at restore time (internal/restic/restore.go), so this is a "+
				"promise the product makes (R10) and a real regression if it fires.\n%s", m6Report(found))
	})

	It("keeps POSIX ACLs, including the default ACL on a directory", func() {
		found := m6DeltasFor(deltas, m6FacetACL)
		Expect(found).To(BeEmpty(),
			"POSIX ACLs did not survive the restore. ACLs travel as system.posix_acl_access / "+
				"system.posix_acl_default xattrs; a directory's DEFAULT ACL is the half that gets lost "+
				"first, because nothing in the directory's own mode hints that it exists.\n%s",
			m6Report(found))
	})

	It("keeps modification times to the nanosecond, on files, symlinks and directories", func() {
		found := m6DeltasFor(deltas, m6FacetTimestamps)
		Expect(found).To(BeEmpty(),
			"modification times did not survive the restore. A directory whose mtime is 'now' means the "+
				"restore never made the second pass that re-stamps directories after writing their "+
				"children; a symlink whose mtime moved means the timestamp was applied through the link "+
				"instead of to it.\n%s", m6Report(found))
	})

	It("keeps symbolic links as links, with their exact targets — including the dangling one", func() {
		found := m6DeltasFor(deltas, m6FacetSymlinks)
		Expect(found).To(BeEmpty(),
			"symbolic links did not survive the restore. A restore that resolves targets instead of "+
				"copying them fails on the dangling link first.\n%s", m6Report(found))
	})

	It("keeps hard links hard — the same inode, not three independent copies", func() {
		found := m6DeltasFor(deltas, m6FacetHardlinks)
		Expect(found).To(BeEmpty(),
			"the hard-link structure did not survive the restore. Note what this catches that a checksum "+
				"cannot: three separate copies of the same bytes match every digest in the manifest and "+
				"still triple the volume's footprint.\n%s", m6Report(found))
	})

	It("keeps sparse files sparse — a densified restore silently fills the volume", func() {
		found := m6DeltasFor(deltas, m6FacetSparse)
		Expect(found).To(BeEmpty(),
			"a sparse file came back dense (or the corpus failed to seed one). The mover passes --sparse "+
				"on restore for exactly this reason; when it stops working, every checksum still matches "+
				"and a 128 KiB file occupies 2 GiB.\n%s", m6Report(found))
	})

	// ── named hazards: same measurement, called out so the report names them ──

	It("restores a multi-pack 384 MiB file and a 0-byte file, both exactly", func() {
		m6RequirePaths(pre, post, []string{
			"content/large.bin",
			"content/empty.bin",
			"content/one-byte.bin",
		}, "large and degenerate files")
		large := pre["content/large.bin"]
		Expect(large.Size).To(Equal("402653184"),
			"content/large.bin must stay large enough to span several 64 MiB restic packs (got %s bytes)", large.Size)
	})

	It("restores non-ASCII, whitespace, quote and glob-metacharacter file names", func() {
		m6RequirePaths(pre, post, []string{
			"noms/éclair ☃ übergroß.txt",
			"noms/日本語ファイル.txt",
			"noms/Ωμέγα-ç-√.txt",
			"noms/space and\ttab.txt",
			"noms/with\nnewline.txt",
			"noms/quote'and\"backslash\\.txt",
			"noms/-leading-dash.txt",
			"noms/trailing-space .txt",
			"noms/asterisk*and?question.txt",
			"noms/colon:and|pipe.txt",
		}, "hostile file names")
	})

	It("restores a 16-level deep tree and a 240-character file name", func() {
		deep := "deep/level-1/level-2/level-3/level-4/level-5/level-6/level-7/level-8/" +
			"level-9/level-10/level-11/level-12/level-13/level-14/level-15/level-16/leaf.txt"
		m6RequirePaths(pre, post, []string{deep}, "deep tree")

		wide := ""
		for p := range pre {
			if len(p) > len(wide) {
				wide = p
			}
		}
		Expect(len(wide)).To(BeNumerically(">=", 240),
			"the corpus no longer contains a long-named entry (longest is %d chars)", len(wide))
		m6RequirePaths(pre, post, []string{wide}, "long file name")
	})

	It("restores a FIFO as a FIFO and an empty directory as a directory", func() {
		fifo, ok := pre["special/fifo"]
		Expect(ok).To(BeTrue(), "the corpus never seeded special/fifo")
		Expect(fifo.Type).To(Equal("p"), "special/fifo is not a FIFO in the source manifest")
		Expect(post).To(HaveKey("special/fifo"), "the FIFO did not come back")
		Expect(post["special/fifo"].Type).To(Equal("p"),
			"special/fifo came back as inode type %q — a restore that turns a FIFO into a regular file "+
				"can hang the workload that opens it", post["special/fifo"].Type)

		Expect(post).To(HaveKey("special/empty-dir"), "the empty directory did not come back")
		Expect(post["special/empty-dir"].Type).To(Equal("d"))
	})
})
