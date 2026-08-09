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
	"slices"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
	"github.com/CrystalBackup/CrystalBackup/internal/restic"
)

// ---------------------------------------------------------------------------
// M6 acceptance — ONE BROKEN VOLUME MUST NOT COST A NAMESPACE ITS BACKUP.
//
// THE FIELD REPORT THIS SPEC IS. On a real cluster, one PVC named a StorageClass that did not
// exist. Exposer resolution errored on every reconcile, the volume never left Pending, and because
// the Backup controller advances exactly ONE volume per reconcile — picking the first non-terminal
// one BY POSITION — the volumes queued behind it were never even attempted. For thirty hours. The
// Backup never reached a terminal phase, so its ClusterBackup did not either, so the nightly
// schedule's `Forbid` concurrency policy skipped every subsequent run: thirty-one hours with zero
// backups across thirty-three namespaces, caused by one volume of one of them.
//
// The fix has three parts, and this spec exists for the consequence of all three together:
//
//   - exposer resolution for a BOUND PVC takes the CSI driver from its PersistentVolume, not from
//     its StorageClass (internal/exposer.Registry.driverFor), so a static non-CSI PV resolves to
//     ErrUnsupported — a verdict about the storage — instead of erroring on a class that was only
//     ever a matching label between claim and volume;
//   - a volume whose resolution fails for any OTHER reason is PARKED (left Pending, carrying
//     `ExposerUnresolvable: <cause>` as its reason) rather than erroring the reconcile, and
//     firstNonTerminalVolume prefers volumes that have not been tried yet, so a broken volume can
//     never starve healthy ones;
//   - pendingResolveDeadline (one hour) eventually fails a volume that never leaves Pending, so
//     the run reaches a terminal phase no matter what.
//
// WHY THIS BELONGS ON PAID INFRASTRUCTURE AND NOT IN ENVTEST. Unit tests and envtest can prove the
// mechanism completely: which volume gets advanced, the phases, the reasons, the counters, the
// deadline. They cannot prove the only thing the user of the broken cluster actually lost. envtest
// has no CSI driver, no kubelet and no object store, so its "Completed" volume is a fixture's
// assertion about itself. The claim this spec exists to make is narrower and harder:
//
//	THE NEIGHBOURING VOLUMES' BYTES ARE IN THE RESTIC REPOSITORY AND CAN BE READ BACK OUT.
//
// So the phases below are asserted in passing, and the weight is on the last It: three snapshots,
// each dumped through the crucible's independent restic oracle, each yielding the distinguishable
// marker its own volume was seeded with and NOT its siblings'. A head-of-line fix that let the
// queue drain while writing the wrong bytes — or nothing at all — would pass every envtest in the
// tree and fail here.
//
// THE POISON VOLUME'S EXACT SHAPE, and what it does and does not reproduce. It is the field
// report's shape, not an approximation of it: a statically provisioned PersistentVolume that is
// NOT a CSI volume (an NFS source pointing at a server nothing will ever dial), bound to a PVC
// that names a StorageClass WHICH DOES NOT EXIST. That combination is legal, common and not a
// misconfiguration — for a static binding, storageClassName is only a matching label between claim
// and volume, and Kubernetes never requires the class object to exist — and it is precisely the
// combination the pre-fix code turned into "StorageClass not found" forever.
//
//	reproduces:      the resolution input that broke (bound PVC + absent class), the queue
//	                 position that made it fatal (it sorts FIRST — see the PVC names, they are
//	                 load-bearing), and the end-to-end consequence for its neighbours.
//	does NOT reproduce: the thirty-hour clock. Under the fix this volume resolves to
//	                 ErrUnsupported on its FIRST attempt and goes terminal (Skipped) immediately,
//	                 which is the honest verdict for storage that cannot be snapshotted at all.
//	                 The PARKED path — a resolution failure that is neither "unsupported" nor a
//	                 refused pre-check, e.g. an unbound PVC naming an absent class — is
//	                 deliberately NOT exercised here, because its terminal outcome is only reached
//	                 through pendingResolveDeadline: ONE HOUR, a compile-time constant of the
//	                 operator, plumbed to no flag and no Helm value on purpose (a per-cluster knob
//	                 on that bound would be a knob whose wrong setting silently destroys backups).
//	                 This suite does not shorten a production bound to make a spec cheap, and it
//	                 does not burn an hour of paid infrastructure to watch a clock either. The
//	                 deadline is testable where its clock can be injected — the controller's
//	                 PendingResolveDeadline field, which exists for exactly that and nothing else.
//
// The mover's NFS volume is never mounted by anything: the poison PVC has no consumer pod, and a
// Skipped volume produces no exposure and no mover Job. The unroutable server address is therefore
// not a hazard but a statement — if any future code path DID try to mount it, this spec would go
// red rather than quietly succeed against a volume that happened to be reachable.
//
// THE ANTI-REGRESSION HEART is the terminal-phase assertion, and its budget is chosen so it cannot
// pass for the wrong reason: m6HeadlineTerminalBudget is well UNDER pendingResolveDeadline. If the
// Skipped verdict ever regresses into a park, the run would still terminate eventually — after an
// hour — and a generous timeout here would call that a pass. It is not one: the incident's
// signature is a run that does not terminate promptly, and thirty minutes of a nightly window is
// the whole difference between a backup and a phone call.
//
// Like the fidelity gate beside it, this spec cannot self-disable: no enable flag, no conditional
// Skip(), no tolerance. A missing bucket FAILS.
//
// COST. Nothing here waits out a deadline, so the wall clock is real work: three Ceph volumes
// seeded by three short Jobs, then three exposures and three data movers driven ONE AT A TIME (a
// Backup advances one volume per reconcile, which is the very property under test and must not be
// worked around), then four restic oracle Jobs to read the bytes back. Budget 15–25 minutes for the
// whole container on a healthy cluster. Three healthy neighbours rather than one is a deliberate
// purchase: the defect was that the queue never drained, and a single neighbour only shows the head
// moved once.
// ---------------------------------------------------------------------------

const (
	// m6HeadlineNS is this spec's own tenant namespace — its own, and not one of the seeded ones,
	// because it plants a cluster-scoped PersistentVolume and a PVC bound to a class that must not
	// exist. Neither belongs in a namespace another spec measures.
	m6HeadlineNS = "m6-headline"

	// m6HeadlinePoisonPVC is the unsnapshottable volume, and ITS NAME IS PART OF THE TEST.
	// ensureVolumes seeds status.volumes in SORTED PVC-name order and firstNonTerminalVolume picks
	// by position, so "aaa-…" puts this volume genuinely at the head of the queue, ahead of every
	// healthy one. A spec whose poison happened to sort last would pass while proving nothing —
	// the whole defect was about what sits BEHIND the broken volume. m6HeadlineOrderIsHostile
	// asserts the ordering rather than trusting these literals to stay in this relation.
	m6HeadlinePoisonPVC = "aaa-legacy-archive"

	// m6HeadlineStorageClass is the crucible's Rook-Ceph RBD class: real CSI, real snapshots, real
	// bytes on real disks. The healthy volumes are the measurement; they must not be on anything
	// simulated.
	m6HeadlineStorageClass = "ceph-block"

	// m6HeadlineCapacity sizes every volume here, poison and healthy alike. Small on purpose: this
	// scenario is about queue behaviour and retrievability, not throughput.
	m6HeadlineCapacity = "100Mi"

	// m6HeadlineTerminalBudget is how long the child Backup gets to reach a terminal phase. Sized
	// to cover three real Ceph snapshots, three clone PVCs, three mover Jobs and the namespace's
	// manifest capture on a loaded cluster — and, far more importantly, sized to stay well under
	// the operator's one-hour pendingResolveDeadline, so a Skipped verdict that regressed into a
	// parked-until-the-deadline one fails here instead of passing late.
	m6HeadlineTerminalBudget = 30 * time.Minute
)

// m6HeadlineGoodPVCs are the healthy neighbours: three of them, all sorting AFTER the poison, each
// seeded with content that identifies it. Three rather than one because the defect was a queue
// defect — one neighbour proves the head moved, three prove the queue drained.
var m6HeadlineGoodPVCs = []string{"data-alpha", "data-beta", "data-gamma"}

var _ = Describe("M6 — an unsnapshottable volume at the head of the queue does not cost its neighbours their backup",
	Label("m6"), Ordered, func() {

		// Unique per campaign, like every run name in this suite: the shared "dr" repository
		// outlives the cluster, and a fixed name meets a previous campaign's snapshots already
		// projected onto its coordinate (see crucible_suite_test.go, and the build-time guard in
		// runname_hermeticity_test.go).
		headlineRun := crucibleRunName("m6-headline")

		// The class that does not exist, and the PV that names it. Both are suffixed with the
		// campaign id: the class so that nothing anybody creates on this cluster can accidentally
		// make it resolvable (the poison depends on its absence, and BeforeAll asserts it), the PV
		// because it is cluster-scoped and a re-run against the same cluster must not inherit a
		// half-deleted one from the attempt before.
		vanishedClass := "m6-headline-vanished-" + crucibleRunID
		legacyPV := "m6-headline-legacy-" + crucibleRunID

		BeforeAll(func() {
			m6RequireS3()
			m1EnsurePlatformSecrets()

			By("Given the shared cluster-DR repository")
			var loc cbv1.ClusterBackupLocation
			if apierrors.IsNotFound(k8s.Get(ctx, client.ObjectKey{Name: m1LocationName}, &loc)) {
				m1CreateLocation(m1LocationName, true)
			}
			m1WaitRepositoryInitialized(m1LocationName)

			ensureNamespace(m6HeadlineNS)

			By("And three healthy Rook-Ceph RBD volumes, each holding content that identifies it")
			for _, pvc := range m6HeadlineGoodPVCs {
				m6HeadlineSeedHealthyVolume(pvc)
			}

			By("And one statically provisioned NFS volume bound to a PVC naming a StorageClass that does not exist")
			m6HeadlineBindLegacyVolume(legacyPV, vanishedClass)

			// Asserted, not assumed, and for the same reason m6_runname asserts its coordinate is
			// empty before manufacturing a collision: if this run name were already occupied, the
			// fan-out would refuse the namespace with RunNameCollision and every assertion below
			// would fail for a reason that has nothing to do with head-of-line blocking.
			By("And nothing already occupies this run's coordinate")
			var preexisting cbv1.Backup
			Expect(apierrors.IsNotFound(k8s.Get(ctx,
				client.ObjectKey{Namespace: m6HeadlineNS, Name: headlineRun}, &preexisting))).To(BeTrue(),
				"a Backup already sits at %s/%s before this run created anything (projected=%q) — the run "+
					"name is not unique to this campaign", m6HeadlineNS, headlineRun,
				preexisting.Annotations[apiconst.AnnotationProjected])
		})

		AfterAll(func() {
			_ = k8s.Delete(ctx, &cbv1.ClusterBackup{ObjectMeta: metav1.ObjectMeta{Name: headlineRun}})
			// Children are label-linked, never owned, so they do not cascade with the run record.
			_ = k8s.DeleteAllOf(ctx, &cbv1.Backup{}, client.InNamespace(m6HeadlineNS),
				client.MatchingLabels{apiconst.LabelOrigin: apiconst.OriginCluster})

			// ORDER MATTERS. The namespace goes first: the PV carries the pv-protection finalizer
			// and cannot be collected while a claim is still bound to it, so deleting the PV before
			// its PVC would leave exactly the cluster-scoped residue this spec must not create.
			// And it waits for the namespace to be GONE rather than merely deleted — a Terminating
			// namespace holding a PVC nothing will release is residue too, invisible to every
			// fire-and-forget cleanup.
			deleteNamespaceAndWaitGone(m6HeadlineNS, 10*time.Minute)

			_ = k8s.Delete(ctx, &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: legacyPV}})
			Eventually(func(g Gomega) {
				err := k8s.Get(ctx, client.ObjectKey{Name: legacyPV}, &corev1.PersistentVolume{})
				g.Expect(apierrors.IsNotFound(err)).To(BeTrue(),
					"this spec's static PersistentVolume %s outlived it: %v", legacyPV, err)
			}, 5*time.Minute, 5*time.Second).Should(Succeed())
		})

		It("reaches a terminal phase, promptly, with the broken volume first in the queue", func() {
			By("Given the unsnapshottable volume sorts ahead of every healthy one")
			m6HeadlineOrderIsHostile()

			By("When a ClusterBackup runs over the namespace")
			m1RunClusterBackup(headlineRun, m1LocationName,
				cbv1.NamespaceSelector{MatchNames: []string{m6HeadlineNS}})

			By("Then the child Backup reaches a TERMINAL phase — a run that never does IS the incident")
			var b cbv1.Backup
			Eventually(func(g Gomega) {
				g.Expect(k8s.Get(ctx, client.ObjectKey{Namespace: m6HeadlineNS, Name: headlineRun}, &b)).To(Succeed())
				g.Expect(b.Status.Phase).To(BeElementOf(
					"Completed", "PartiallyCompleted", "PartiallyFailed", "Failed"),
					"the Backup is still non-terminal after %s. This is the shape of the incident: one "+
						"volume the operator cannot resolve, and a namespace that never gets a backup. %s",
					m6HeadlineTerminalBudget, m6HeadlineDescribe(&b))
			}, m6HeadlineTerminalBudget, 15*time.Second).Should(Succeed())

			By("And the volume queue was seeded in sorted order, so the broken volume really was the head")
			names := make([]string, 0, len(b.Status.Volumes))
			for _, v := range b.Status.Volumes {
				names = append(names, v.Pvc)
			}
			Expect(names).To(HaveLen(len(m6HeadlineGoodPVCs)+1),
				"every PVC in the namespace must be tracked: %s", m6HeadlineDescribe(&b))
			Expect(slices.IsSorted(names)).To(BeTrue(),
				"ensureVolumes seeds status.volumes in sorted PVC-name order; if that changed, this "+
					"spec's poison is no longer at the head of the queue and proves nothing: %v", names)
			Expect(names[0]).To(Equal(m6HeadlinePoisonPVC),
				"the broken volume must be FIRST — the defect was about what sits behind it: %v", names)

			By("And the broken volume is reported honestly: Skipped / CSISnapshotUnsupported, with no snapshot")
			poison := m6HeadlineVolume(&b, m6HeadlinePoisonPVC)
			// The product's own words, read out of the code rather than guessed: a non-CSI
			// PersistentVolume resolves to exposer.ErrUnsupported, and advancePending turns that —
			// and only that — into Skipped/CSISnapshotUnsupported. Skipped is terminal in the queue
			// and NEUTRAL in the roll-up, which is the pair of properties that lets the namespace
			// through: terminal so it stops being the head, neutral so it never alarms.
			Expect(string(poison.Phase)).To(Equal("Skipped"),
				"a statically provisioned non-CSI volume can never be snapshotted; that is a verdict "+
					"about the storage, not a cluster failure: %s", m6HeadlineDescribe(&b))
			Expect(poison.Reason).To(Equal("CSISnapshotUnsupported"),
				"the reason must name the storage's capability, not a missing StorageClass: %s", poison.Reason)
			Expect(poison.SnapshotID).To(BeEmpty(),
				"a Skipped volume saved nothing and must not claim a snapshot id")

			By("And every healthy volume Completed and carries its restic snapshot id")
			for _, pvc := range m6HeadlineGoodPVCs {
				vol := m6HeadlineVolume(&b, pvc)
				Expect(string(vol.Phase)).To(Equal("Completed"),
					"volume %s was queued BEHIND the broken one — this is the assertion the incident "+
						"failed for thirty hours: %s", pvc, m6HeadlineDescribe(&b))
				Expect(vol.SnapshotID).NotTo(BeEmpty(), "a completed volume must carry its snapshot id")
			}

			By("And the Backup is Completed — a Skipped volume is neutral, never a degradation")
			Expect(b.Status.Phase).To(Equal("Completed"),
				"three snapshots landed and nothing failed; a permanently unsnapshottable PVC must not "+
					"make its namespace alarm on every run, forever: %s", m6HeadlineDescribe(&b))

			By("And the skip was announced as an Event naming the volume, not left to be inferred")
			Eventually(func(g Gomega) {
				notes := m6BackupEventNotes(g, m6HeadlineNS, headlineRun, "Normal")
				g.Expect(strings.Join(notes, "\n")).To(ContainSubstring(m6HeadlinePoisonPVC),
					"no Normal Event mentions %s; the skip has to be visible without reading status "+
						"field by field. Events seen:\n%s", m6HeadlinePoisonPVC, strings.Join(notes, "\n"))
			}, 2*time.Minute, 10*time.Second).Should(Succeed())

			By("And the platform run counts three successes and no failures")
			cb := m1WaitClusterBackupTerminal(headlineRun, 10*time.Minute)
			Expect(cb.Status.Phase).To(Equal("Completed"))
			Expect(cb.Status.NamespacesMatched).To(Equal(int32(1)))
			Expect(cb.Status.PVCsSucceeded).To(Equal(int32(len(m6HeadlineGoodPVCs))),
				"every healthy volume must be counted: %s", m6HeadlineDescribe(&b))
			Expect(cb.Status.PVCsFailed).To(Equal(int32(0)),
				"nothing failed here — the unsnapshottable volume was skipped, not failed")
		})

		// THE REASON THIS SPEC IS ON PAID INFRASTRUCTURE. Everything above is provable in envtest.
		// This is not: the bytes of the volumes that were queued behind the broken one are read back
		// out of the object store, through the crucible's independent restic oracle, with the
		// operator's own wrapped DEK — and each one must yield ITS OWN marker and none of its
		// siblings'. A queue fix that drained the queue while writing the wrong bytes, or writing
		// nothing, is green everywhere else and red here.
		It("puts the neighbouring volumes' actual bytes in the repository, retrievable", func() {
			snaps := m1ResticSnapshots(m1LocationName)
			Expect(snaps).NotTo(BeEmpty(), "the run must have written snapshots to the shared repository")

			var b cbv1.Backup
			Expect(k8s.Get(ctx, client.ObjectKey{Namespace: m6HeadlineNS, Name: headlineRun}, &b)).To(Succeed())

			for _, pvc := range m6HeadlineGoodPVCs {
				By(fmt.Sprintf("Then %s has a data snapshot at its own coordinate", pvc))
				snap, ok := m1CascadeDataSnapshot(snaps, m6HeadlineNS, pvc, headlineRun)
				Expect(ok).To(BeTrue(),
					"no kind=data snapshot tagged namespace=%s pvc=%s run=%s — the volume reported "+
						"Completed but the repository has nothing at its coordinate",
					m6HeadlineNS, pvc, headlineRun)
				Expect(snap.Host).To(Equal(m1ClusterID))
				Expect(snap.Paths).To(ContainElement("/data/" + m6HeadlineNS + "/" + pvc))

				By(fmt.Sprintf("And the id %s reported is the id the repository holds", pvc))
				Expect(m6HeadlineVolume(&b, pvc).SnapshotID).To(Equal(snap.ID),
					"the Backup's snapshot id must address the snapshot that exists, or a restore "+
						"would follow a handle to nothing")

				By(fmt.Sprintf("And %s's own bytes come back out of the repository", pvc))
				dumped := m6HeadlineDump(snap.ID,
					"/data/"+m6HeadlineNS+"/"+pvc+"/payload/marker.txt")
				Expect(dumped).To(ContainSubstring(m6HeadlineMarker(pvc)),
					"the snapshot for %s does not contain the content that volume was seeded with. "+
						"`restic dump` returned:\n%s", pvc, dumped)
				// The cross-check that makes the one above mean something: markers are per-volume,
				// so a mover that snapshotted the wrong clone — entirely possible when several
				// exposures are in flight for one namespace — would still return SOME marker.
				for _, other := range m6HeadlineGoodPVCs {
					if other == pvc {
						continue
					}
					Expect(dumped).NotTo(ContainSubstring(m6HeadlineMarker(other)),
						"the snapshot for %s carries %s's content: the volumes were crossed", pvc, other)
				}
			}

			By("And the unsnapshottable volume wrote nothing — a skip must not invent data")
			for _, s := range snaps {
				kind, _ := restic.TagValue(s.Tags, restic.TagKeyKind)
				ns, _ := restic.TagValue(s.Tags, restic.TagKeyNamespace)
				gotRun, _ := restic.TagValue(s.Tags, restic.TagKeyRun)
				gotPVC, _ := restic.TagValue(s.Tags, restic.TagKeyPVC)
				Expect(kind == restic.KindData && ns == m6HeadlineNS &&
					gotRun == headlineRun && gotPVC == m6HeadlinePoisonPVC).To(BeFalse(),
					"snapshot %s claims to hold data for the volume the operator reported Skipped", s.ID)
			}
		})

		It("leaves no residue behind, skipped volume included", func() {
			By("Then no exposure object outlived the run")
			m1AssertNoResidualSnapshotObjects(m6HeadlineNS)

			By("And no mover Job of this run is still in the operator namespace")
			Eventually(func(g Gomega) {
				jobs := m6MoverJobsForRun(g, headlineRun)
				names := make([]string, 0, len(jobs))
				for i := range jobs {
					names = append(names, jobs[i].Name)
				}
				g.Expect(names).To(BeEmpty(),
					"mover Jobs of a terminal run survived it: %s", strings.Join(names, " "))
			}, 10*time.Minute, 15*time.Second).Should(Succeed())
		})
	})

// ---------------------------------------------------------------------------
// Spec-local helpers (thin, file-scoped; the shared vocabulary lives in
// m1_helpers_test.go, m1_restic_test.go, m2_helpers_test.go and m6_*_helpers and
// is reused, never redefined).
// ---------------------------------------------------------------------------

// m6HeadlineMarker is the content that identifies one volume. It carries the PVC name AND the
// campaign id, so a dumped file proves both which volume it came from and that it came from THIS
// campaign rather than a snapshot the shared repository kept from an older one.
func m6HeadlineMarker(pvc string) string {
	return "crucible-m6-headline-marker:" + pvc + ":" + crucibleRunID
}

// m6HeadlineSeedHealthyVolume provisions one PVC on the crucible's Rook-Ceph RBD class and writes a
// small corpus into it whose marker file names the volume. The random blob beside it exists so the
// snapshot has real bytes to move rather than a single 64-byte file that any code path could
// accidentally get right.
func m6HeadlineSeedHealthyVolume(pvc string) {
	GinkgoHelper()

	sc := m6HeadlineStorageClass
	claim := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: pvc, Namespace: m6HeadlineNS},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: &sc,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(m6HeadlineCapacity)},
			},
		},
	}
	Expect(client.IgnoreAlreadyExists(k8s.Create(ctx, claim))).To(Succeed(),
		"create PVC %s/%s on %s", m6HeadlineNS, pvc, sc)

	script := fmt.Sprintf(`set -e
cd /data
mkdir -p payload
printf '%%s\n' %q > payload/marker.txt
head -c 65536 /dev/urandom > payload/blob.bin
find . -type f ! -name 'MANIFEST.*' | sort | xargs sha256sum > MANIFEST.sha256
sync`, m6HeadlineMarker(pvc))

	ok, log := m2VolumeJob(m6HeadlineNS, pvc, "m6-headline-seed", script)
	Expect(ok).To(BeTrue(), "seeding %s/%s must succeed:\n%s", m6HeadlineNS, pvc, log)
}

// m6HeadlineBindLegacyVolume builds the poison: a statically provisioned, NON-CSI PersistentVolume
// and a PVC pre-bound to it by name, both naming a StorageClass that does not exist.
//
// This is the field report's input reproduced rather than approximated, and each half is load-bearing:
//
//   - NFS (not CSI) is what makes exposer resolution return ErrUnsupported once it reads the
//     PersistentVolume — there is no CSI driver anywhere to ask for a snapshot;
//   - the ABSENT StorageClass is what made the pre-fix code error forever, because it resolved the
//     driver through the class instead of through the volume holding the data;
//   - the class name being absent is asserted, not assumed. If somebody ever created a class by
//     this name the poison would quietly resolve and this spec would measure nothing.
//
// The claim reaching Bound is also asserted: a Pending PVC would take the OTHER resolution path
// (the class, for a volume that names none) and this spec would silently become a test of the
// parked path it explicitly declares out of scope.
func m6HeadlineBindLegacyVolume(pvName, className string) {
	GinkgoHelper()

	var sc storagev1.StorageClass
	Expect(apierrors.IsNotFound(k8s.Get(ctx, client.ObjectKey{Name: className}, &sc))).To(BeTrue(),
		"StorageClass %q must NOT exist — the whole point of this fixture is a claim naming a class "+
			"that is only a matching label, never an object", className)

	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: pvName},
		Spec: corev1.PersistentVolumeSpec{
			Capacity: corev1.ResourceList{
				corev1.ResourceStorage: resource.MustParse(m6HeadlineCapacity),
			},
			AccessModes:                   []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
			StorageClassName:              className,
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				// Deliberately unroutable, and deliberately never mounted: the poison PVC has no
				// consumer pod, and a Skipped volume produces no exposure and no mover Job. If any
				// code path ever DID try to mount this, the failure is loud instead of accidental.
				NFS: &corev1.NFSVolumeSource{
					Server: "10.255.255.1",
					Path:   "/exports/legacy-archive",
				},
			},
		},
	}
	Expect(client.IgnoreAlreadyExists(k8s.Create(ctx, pv))).To(Succeed(),
		"create static non-CSI PersistentVolume %s", pvName)

	claim := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: m6HeadlinePoisonPVC, Namespace: m6HeadlineNS},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			// Pre-bound by name: with spec.volumeName set the binder never attempts dynamic
			// provisioning, which is exactly the static flow — and the only flow that works at all
			// when the named class does not exist.
			VolumeName:       pvName,
			StorageClassName: &className,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(m6HeadlineCapacity)},
			},
		},
	}
	Expect(client.IgnoreAlreadyExists(k8s.Create(ctx, claim))).To(Succeed(),
		"create statically bound PVC %s/%s", m6HeadlineNS, m6HeadlinePoisonPVC)

	Eventually(func(g Gomega) {
		var got corev1.PersistentVolumeClaim
		g.Expect(k8s.Get(ctx, client.ObjectKeyFromObject(claim), &got)).To(Succeed())
		g.Expect(got.Status.Phase).To(Equal(corev1.ClaimBound),
			"PVC %s/%s never bound to the static PV (phase=%q). Unbound, it would take the "+
				"StorageClass resolution path and this spec would measure the wrong half of the fix",
			m6HeadlineNS, m6HeadlinePoisonPVC, got.Status.Phase)
	}, 3*time.Minute, 3*time.Second).Should(Succeed())
}

// m6HeadlineOrderIsHostile asserts the fixture is actually hostile: the poison PVC sorts before
// every healthy one. ensureVolumes seeds status.volumes in sorted name order and
// firstNonTerminalVolume picks by position, so this is what puts the broken volume at the head of
// the queue. Asserted rather than left to the literals, because renaming a PVC for readability is
// exactly the kind of harmless-looking edit that would turn this scenario into a green no-op.
func m6HeadlineOrderIsHostile() {
	GinkgoHelper()
	for _, good := range m6HeadlineGoodPVCs {
		Expect(m6HeadlinePoisonPVC < good).To(BeTrue(),
			"PVC %q must sort before %q, or the broken volume is not at the head of the queue and "+
				"this spec proves nothing about the volumes behind it", m6HeadlinePoisonPVC, good)
	}
}

// m6HeadlineVolume returns one named volume's status, failing with the whole roll-up when it is
// absent — a missing volume is itself a finding (ensureVolumes must track every matched PVC).
func m6HeadlineVolume(b *cbv1.Backup, pvc string) cbv1.VolumeStatus {
	GinkgoHelper()
	for i := range b.Status.Volumes {
		if b.Status.Volumes[i].Pvc == pvc {
			return b.Status.Volumes[i]
		}
	}
	Expect(b.Status.Volumes).To(ContainElement(HaveField("Pvc", pvc)),
		"Backup %s/%s does not track volume %s: %s", b.Namespace, b.Name, pvc, m6HeadlineDescribe(b))
	return cbv1.VolumeStatus{}
}

// m6HeadlineDescribe renders the Backup's phase and every volume's phase+reason on one line. Every
// failure message in this file carries it, because the incident's signature is legible only in the
// whole set: one volume stuck with a resolution error and the others never attempted.
func m6HeadlineDescribe(b *cbv1.Backup) string {
	parts := make([]string, 0, len(b.Status.Volumes))
	for _, v := range b.Status.Volumes {
		if v.Reason == "" {
			parts = append(parts, fmt.Sprintf("%s=%s", v.Pvc, v.Phase))
			continue
		}
		parts = append(parts, fmt.Sprintf("%s=%s(%s)", v.Pvc, v.Phase, v.Reason))
	}
	return fmt.Sprintf("phase=%s volumes[%s]", b.Status.Phase, strings.Join(parts, " "))
}

// m6HeadlineDump reads one file out of a snapshot with `restic dump`, through the crucible's
// independent restic oracle (the operator's own repository URL and unwrapped DEK, a real restic
// binary in a Job). It asserts restic EXITED ZERO, which is the difference between "the bytes are
// not there" and "the bytes are there and are these": m1ResticExec returns the pod log either way,
// so a caller that only searched the log for its marker would read a failed dump as a mismatch and
// a wrong path as a regression.
func m6HeadlineDump(snapshotID, path string) string {
	GinkgoHelper()
	repoURL := restic.RepoURL(os.Getenv("S3_ENDPOINT"), os.Getenv("S3_BUCKET"), m1S3Prefix, m1ClusterID)
	out, ok := m1ResticRun(repoURL, m1UnwrapDEK(m1LocationName), "dump", snapshotID, path)
	Expect(ok).To(BeTrue(),
		"`restic dump %s %s` failed: the data the operator reported Completed is not retrievable "+
			"from the repository. restic said:\n%s", snapshotID, path, out)
	return out
}
