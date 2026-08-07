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
	"slices"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
)

// ---------------------------------------------------------------------------
// M6 acceptance — the MOVER START DEADLINE, reproduced on real storage.
//
// THE FIELD REPORT THIS SPEC IS. A user on RKE2 + Rook-Ceph ran 0.6.2. At 04:00
// a nightly ClusterBackup fanned out over 33 namespaces and created six data
// movers. Their pods went ContainerCreating and STAYED THERE FOR THIRTY-SIX
// HOURS, because the temp clone PVC could not be mapped:
//
//	Warning FailedMount 69s (x1069 over 36h) kubelet
//	  MountVolume.MountDevice failed ... rbd: map failed with error (exit
//	  status 22) ... rbd: sysfs write failed / rbd: map failed: (22) Invalid
//	  argument
//
// The root cause was theirs — an RBD clone with op_features: clone-child their
// kernel's krbd will not map. Ours were: nothing ever gave up, no alert could
// fire (all eleven rules watched for FAILURES and nothing failed), and
// concurrencyPolicy: Forbid then meant no further nightly ever ran. One backup
// in fifteen days.
//
// The kubelet published the exact cause 1069 times, starting one minute in.
// Nobody read it because nothing surfaced it. So this spec asserts TWO things
// and the second is the one that closes the incident:
//
//  1. the volume Fails on its own, at the deadline, rather than hanging;
//  2. its status.volumes[].reason CARRIES THE KUBELET'S OWN WORDS, so
//     `kubectl get backup` is as informative as `kubectl describe pod` was.
//
// THE FAULT INJECTED. The user's exact krbd failure is not reproducible on
// demand, so this reproduces its SHAPE, which is what the operator actually
// keys off: a clone PVC the mover pod cannot mount. The rook-ceph RBD NODE
// plugin (the DaemonSet that registers the CSI driver with every kubelet) is
// parked with an impossible nodeSelector. The mover pod then sits in
// ContainerCreating and the cluster records a Warning saying so, which is what
// this spec reads back out of the reason.
//
// The STAGE at which it fails is not the field report's, and the spec must not
// assume it is. Parking the node plugin also removes the driver-REGISTRAR, so
// the driver leaves the CSINode object and the attach-detach controller refuses
// before the kubelet ever reaches MountDevice:
//
//	Warning FailedAttachVolume  AttachVolume.Attach failed for volume "pvc-…":
//	  CSINode crucible-worker-1 does not contain driver rook-ceph.rbd.csi.ceph.com
//
// The field report's was FailedMount — there the volume attached and then could
// not be mapped. Both are the cluster saying why; only the stage differs. The
// assertions below therefore compare the operator's reason against the Events
// this cluster actually recorded, rather than against a word. Written the other
// way, hardcoding FailedMount, this spec asserted how its own fault injection
// happens to fail and cost a campaign to say so.
//
// Parking the NODE plugin and not the provisioner is deliberate and load-bearing:
// the CONTROLLER-side plugin keeps running, so the VolumeSnapshot is still cut,
// the temp clone PVC is still provisioned and bound, and the exposure still goes
// Ready. The run therefore reaches Uploading — which is the phase under test.
// Killing the provisioner instead would stall it in Snapshotting and this spec
// would be green against a completely different guard.
//
// It is also NON-DESTRUCTIVE to volumes already mounted: a kubelet needs the node
// plugin to stage a NEW mount, not to keep an existing one. The other tenants'
// consumer pods keep running; only new mounts fail, for the length of this spec.
//
// COST. The deadline is a compile-time constant of the operator
// (moverStartDeadline, 30 minutes) and there is no knob to shorten it —
// deliberately, this suite's standing rule: no tunable tolerances, because a
// test that can shorten a bound is testing a product nobody ships. Budget ~45
// minutes of wall clock for the first It and ~15 for the second.
// ---------------------------------------------------------------------------

const (
	// m6StallNS is this spec's own tenant namespace. Its own, not the seed's, so parking a
	// cluster-wide CSI node plugin cannot corrupt another spec's data.
	m6StallNS = "m6-stall"
	// m6StallPVC is the one volume backed up. Small: this spec is about a pod that never starts,
	// so there is no throughput to measure.
	m6StallPVC = "probe"
	// m6StallStorageClass is the crucible's Ceph RBD class (deploy/manifests/ceph-storage.yaml) —
	// the same storage the field report came from, and the one whose node plugin is parked below.
	m6StallStorageClass = "ceph-block"

	// m6RBDNodePluginNS is where the RBD node plugin DaemonSet lives — the thing that runs a
	// csi-rbdplugin + driver-registrar on every node, and therefore the thing whose absence makes
	// a kubelet unable to stage an RBD volume.
	//
	// Its NAME is discovered, not declared, for the same reason m6FindSnapshotController discovers
	// the snapshot-controller's: rook names it after the CSI driver, and the convention has
	// changed. This spec was written against `csi-rbdplugin` and the first campaign that ran it
	// found `rook-ceph.rbd.csi.ceph.com-nodeplugin` — a red spec against a correct product, which
	// is the second-worst outcome after a green one against a broken product. The failure was at
	// least loud, because BeforeAll verifies the object exists rather than parking a name and
	// hoping; a silent no-op fault injection would have made this spec pass while testing nothing.
	m6RBDNodePluginNS = "rook-ceph"
	// m6RBDNodePluginMatch is the substring that identifies it across rook versions: the RBD
	// driver's own name is in it either way, and the cephfs plugin — the other DaemonSet in that
	// namespace — is not matched by it.
	m6RBDNodePluginMatch = "rbd.csi.ceph.com"

	// m6CSIOperatorDeploy is the ceph-csi-operator's controller-manager, and it has to be stopped
	// before the DaemonSet below is touched at all.
	//
	// rook ≥ 1.17 hands the CSI driver plane to it: the node plugin DaemonSet is OWNED by a
	// csi.ceph.io/v1 Driver CR, and this Deployment reconciles it on an Owns() watch. Parking the
	// DaemonSet directly is therefore undone before the next poll — the campaign that first ran
	// this spec recorded SuccessfulDelete and SuccessfulCreate for all six pods in the SAME
	// second, and "node plugin daemonset updated successfully" in the csi-operator's log at that
	// instant. The park did take; it lasted a fraction of a second.
	//
	// That is worth stating precisely, because the failure LOOKED like a slow cluster: the spec
	// timed out waiting for the pods to go away, which reads as "the DaemonSet is draining
	// slowly" and invites a bigger timeout. No timeout would ever have worked. A reconciler is
	// not a race you can wait out.
	//
	// Setting the Driver CR's own nodePlugin affinity instead — the semantically correct knob —
	// was rejected: that CR is written by rook-ceph-operator on every CephCluster reconcile, so a
	// reconcile landing inside this spec's 45-minute window would silently un-inject the fault
	// and make the spec flaky rather than red. Stopping the reconciler has no such window, and it
	// is the pattern m6_precheck_test.go already uses on rook-ceph-operator for the same reason.
	//
	// Blast radius: this stops RECONCILIATION only. The RBD controller plugin (provisioner and
	// snapshotter sidecars) keeps running, so the snapshot half of the cascade — cut, clone,
	// exposure Ready — still works, which is exactly what this spec needs it to do.
	m6CSIOperatorDeploy = "ceph-csi-controller-manager"

	// m6StallParkLabel is the nodeSelector key the DaemonSet is parked with. A DaemonSet has no
	// replica count, so "scale it to zero" means "select no node"; this key exists on nothing.
	m6StallParkLabel = "crystalbackup.io/crucible-parked"

	// m6StallDeadline is the operator's own moverStartDeadline, restated here because the crucible
	// links no operator internals. THIS IS A CROSS-REPO CONTRACT: if internal/controller changes
	// the bound, this constant and the Eventually budgets below have to move with it.
	m6StallDeadline = 30 * time.Minute
	// m6StallReason is backupReasonMoverStartDeadline, likewise restated and likewise a contract.
	// The volume's reason must START with it — the kubelet's own message follows a colon.
	m6StallReason = "MoverStartDeadlineExceeded"
)

var _ = Describe("M6 — the mover start deadline", Label("m6", "stall"), Ordered, func() {
	// One run name per case, campaign-scoped (see crucibleRunName): a fixed name would collide
	// with a previous campaign's snapshots in the shared repository.
	var (
		stalledRun   = crucibleRunName("m6-stall-stuck")
		recoveredRun = crucibleRunName("m6-stall-recovered")
	)

	// The node plugin's original nodeSelector, captured so the restore puts back what was there
	// rather than what this spec assumes was there. rook sets none today; a distro overlay may.
	var originalNodeSelector map[string]string
	var captured bool
	// Resolved in BeforeAll, once, and reused: see m6RBDNodePluginMatch for why it is discovered.
	var rbdNodePlugin string
	// The csi-operator's replica count before this spec touched it — captured, never assumed, the
	// same way m6_precheck_test.go captures rook-ceph-operator's. Restoring "to 1" would be a
	// guess about someone else's cluster.
	var csiOperatorReplicas int32

	BeforeAll(func() {
		m1RequireS3()
		m1EnsurePlatformSecrets()

		By("Given the shared cluster-DR repository")
		var loc cbv1.ClusterBackupLocation
		if apierrors.IsNotFound(k8s.Get(ctx, client.ObjectKey{Name: m1LocationName}, &loc)) {
			m1CreateLocation(m1LocationName, true)
		}
		m1WaitRepositoryInitialized(m1LocationName)

		By("And a small Ceph RBD volume in this spec's own namespace")
		ensureNamespace(m6StallNS)
		startPVCConsumer(m6StallNS, m6StallPVC, m6StallStorageClass)

		By("And the RBD node plugin DaemonSet, whose absence is the fault this spec injects")
		rbdNodePlugin = m6FindRBDNodePlugin()
		originalNodeSelector = m6CaptureDaemonSetNodeSelector(m6RBDNodePluginNS, rbdNodePlugin)
		csiOperatorReplicas = m6DeploymentReplicas(m6RookOperatorNS, m6CSIOperatorDeploy)
		captured = true
	})

	AfterAll(func() {
		// Belt and braces: the It restores what it parked, but a spec that fails mid-way must not
		// leave the cluster unable to mount an RBD volume for the rest of the campaign. Guarded on
		// `captured`, because AfterAll runs even when BeforeAll failed and "restore to nil" would
		// be indistinguishable from a legitimate empty selector — see m6_precheck_test.go's
		// AfterAll for the same trap, where an unguarded restore would have scaled the storage
		// operator to zero as a cleanup step.
		if captured {
			m6RestoreDaemonSetNodeSelector(m6RBDNodePluginNS, rbdNodePlugin, originalNodeSelector)
			// Order matters even here: unpark first, then let the reconciler back in. And it is
			// guarded on the captured count being non-zero for the reason above — the zero value
			// of an int32 is a perfectly plausible replica count to restore, and restoring it on
			// a run where BeforeAll failed would leave the cluster's CSI plane switched off for
			// every campaign that followed.
			if csiOperatorReplicas > 0 {
				m6ScaleDeployment(m6RookOperatorNS, m6CSIOperatorDeploy, csiOperatorReplicas)
			}
		}
		deleteNamespace(m6StallNS)
	})

	It("fails a volume whose mover pod never started, quoting the kubelet, and leaves nothing behind", func() {
		// Before the park, not after: the csi-operator owns the DaemonSet and would put it back
		// within the second (see m6CSIOperatorDeploy). Registered first so that DeferCleanup's
		// LIFO order unparks the DaemonSet BEFORE the reconciler is allowed back — the reverse
		// order would have the operator racing the unpark over the same object.
		By("Given the ceph-csi operator is stopped — it owns the node plugin and would revert the park")
		m6ScaleDeployment(m6RookOperatorNS, m6CSIOperatorDeploy, 0)
		DeferCleanup(func() {
			m6ScaleDeployment(m6RookOperatorNS, m6CSIOperatorDeploy, csiOperatorReplicas)
		})

		By("And the RBD node plugin is parked — no kubelet can stage an RBD volume any more")
		m6ParkDaemonSet(m6RBDNodePluginNS, rbdNodePlugin)
		DeferCleanup(func() {
			m6RestoreDaemonSetNodeSelector(m6RBDNodePluginNS, rbdNodePlugin, originalNodeSelector)
		})

		By("When a backup runs over the namespace")
		m1RunClusterBackup(stalledRun, m1LocationName, cbv1.NamespaceSelector{MatchNames: []string{m6StallNS}})

		// The precondition that makes this spec about the RIGHT guard. The snapshot side is
		// untouched, so the volume must get all the way to Uploading with a real mover Job; if it
		// stopped in Snapshotting the fault injection hit the wrong component and every assertion
		// below would be measuring the snapshot deadline instead.
		By("Then the volume reaches Uploading with a real mover Job — the snapshot half is unaffected")
		var moverJob string
		Eventually(func(g Gomega) {
			vol := m6PrecheckVolume(g, m6StallNS, stalledRun, m6StallPVC)
			g.Expect(string(vol.Phase)).To(Equal("Uploading"),
				"reason=%q — the fault must land on the MOUNT, not on the snapshot", vol.Reason)
			moverJob = m6StallMoverJob(g, stalledRun)
		}, 10*time.Minute, 10*time.Second).Should(Succeed())

		// The pod's NAME is kept, because the assertions further down compare the operator's reason
		// against the Events the cluster recorded on this exact pod. Reading it once here, while
		// the pod is demonstrably the wedged one, is what makes that comparison about the right
		// object even after the Job has been torn down around it.
		var moverPod string
		By("And its pod is wedged in ContainerCreating, with the kubelet saying why")
		Eventually(func(g Gomega) {
			pod := m6StallMoverPod(g, moverJob)
			moverPod = pod.Name
			g.Expect(pod.Status.Phase).To(Equal(corev1.PodPending),
				"the mover pod started after all — the node plugin was not parked, or something "+
					"re-created it, and this spec is no longer reproducing the incident")
			g.Expect(m6PodWarningMessages(g, pod.Name)).NotTo(BeEmpty(),
				"the kubelet recorded no Warning about a pod it cannot start; without one there is "+
					"no observable cause for the reason to carry, and the whole point of this lot is "+
					"that there always is one")
		}, 10*time.Minute, 15*time.Second).Should(Succeed())

		// The series that makes the stall visible to Prometheus at all. It is asserted HERE, while
		// the run is genuinely in flight, because that is the only window in which it exists —
		// the collector stops emitting it the moment the phase goes terminal. Its alert
		// (CrystalbackupBackupStalled) has an eight-hour bound and is out of reach of a paid
		// campaign; the series being present and non-zero is the half that can be certified.
		By("And crystalbackup_backup_in_progress_since_timestamp_seconds is published for it")
		if m6PrometheusConfigured() {
			Eventually(func(g Gomega) {
				samples, err := m6Query(fmt.Sprintf(
					`crystalbackup_backup_in_progress_since_timestamp_seconds{namespace=%q}`, m6StallNS))
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(m6PositiveSamples(samples)).NotTo(BeEmpty(),
					"a run that has not finished must publish its start instant, or no rule can ever "+
						"see a hang: %s", m6DescribeSamples(samples))
			}, 5*time.Minute, 15*time.Second).Should(Succeed())
		}

		// THE assertion. 40 minutes covers the operator's 30-minute moverStartDeadline plus its
		// 5-second poll and the cascade's own scheduling. The verdict is that the operator gives up
		// ON ITS OWN — before this spec's own budget, and thirty-five and a half hours before the
		// field report did.
		By("Then, past the mover start deadline, the volume Fails — it does not wait forever")
		Eventually(func(g Gomega) {
			vol := m6PrecheckVolume(g, m6StallNS, stalledRun, m6StallPVC)
			g.Expect(string(vol.Phase)).To(Equal("Failed"),
				"a mover pod that never reached Running must be given up on after %s", m6StallDeadline)
			g.Expect(vol.Reason).To(HavePrefix(m6StallReason),
				"the reason must name which deadline fired: %q", vol.Reason)
		}, 40*time.Minute, 30*time.Second).Should(Succeed())

		// And THE point of the lot. A reason that said only "MoverStartDeadlineExceeded" would name
		// our decision and not their cluster, leaving the operator exactly as blind as thirty-six
		// hours of silence did. So the reason must carry the Event reason the CLUSTER recorded —
		// the first column `kubectl describe pod` prints — and this asserts that the two commands
		// agree by reading both, rather than by naming a string.
		//
		// It was written the other way and it was wrong. It asserted `FailedMount`, which is the
		// word from the field report: there the volume attached and then could not be mapped. This
		// spec injects a different fault. Parking the node plugin removes the driver-REGISTRAR, so
		// the driver leaves the CSINode object and the attach-detach controller fails before the
		// kubelet ever reaches MountDevice:
		//
		//	Warning FailedAttachVolume  AttachVolume.Attach failed for volume "pvc-…":
		//	  CSINode crucible-worker-1 does not contain driver rook-ceph.rbd.csi.ceph.com
		//
		// Both are the cluster saying why, at different stages, and a spec that hardcodes either
		// one is asserting how the fault was injected rather than what the operator promised. It
		// cost a campaign to learn that, and the fix is to compare against the live Events.
		By("And the reason CARRIES the cluster's own account of why the pod could not start")
		var failed cbv1.VolumeStatus
		Eventually(func(g Gomega) {
			failed = m6PrecheckVolume(g, m6StallNS, stalledRun, m6StallPVC)

			// The colon is the whole claim in one character: bare, the reason is our verdict; with
			// a detail appended, it is theirs. Asserted separately from the match below so that
			// "no detail at all" and "a detail that is not the cluster's" fail differently.
			g.Expect(failed.Reason).To(ContainSubstring(m6StallReason+":"),
				"reason=%q — it names the deadline and nothing else. `kubectl get backup` has to be "+
					"as informative as `kubectl describe pod` was, or this whole change converts a "+
					"silent hang into a silent failure", failed.Reason)

			reasons := m6PodWarningReasons(g, moverPod)
			// Not a formality. An empty set would make the ContainSubstring below vacuous, and this
			// spec would then certify the operator against a cluster that recorded nothing.
			g.Expect(reasons).NotTo(BeEmpty(),
				"the cluster recorded no Warning Event on mover pod %s, so there is nothing for the "+
					"reason to have carried and nothing here to compare against", moverPod)

			matched := false
			for _, r := range reasons {
				if strings.Contains(failed.Reason, r) {
					matched = true
				}
			}
			g.Expect(matched).To(BeTrue(),
				"reason=%q carries a detail, but not one the cluster recorded. `kubectl describe "+
					"pod %s` says %v, and the two commands have to agree — that agreement IS the "+
					"deliverable", failed.Reason, moverPod, reasons)
		}, 2*time.Minute, 10*time.Second).Should(Succeed())
		Expect(len(failed.Reason)).To(BeNumerically("<=", 200),
			"a status field must stay bounded; the Event carries the long form")

		// The Event carries the long form the 200-character status field cannot. Same rule as
		// above: the cluster's own word is read from the cluster, not named here.
		By("And the Event names the pod to describe and quotes the cluster in full")
		Eventually(func(g Gomega) {
			notes := m6BackupEventNotes(g, m6StallNS, stalledRun, corev1.EventTypeWarning)
			g.Expect(notes).NotTo(BeEmpty(), "no Warning Event was recorded on the Backup at all")
			joined := strings.Join(notes, "\n")
			// The three things the reader needs and cannot get anywhere else: which volume, what
			// the operator concluded, and the exact command that shows them the rest. The verdict
			// phrase is the operator's own, taken from backup_progress_deadline_test.go rather than
			// paraphrased — the first draft of this line looked for "never reached Running" while
			// the product says "no pod of it EVER reached Running", and a 33-minute lane failed on
			// the difference between two words that mean the same thing.
			g.Expect(joined).To(And(
				ContainSubstring(m6StallPVC),
				ContainSubstring("no pod of it ever reached Running"),
				ContainSubstring("kubectl describe pod"),
				ContainSubstring(moverJob),
			), "the Event must name the PVC, state the verdict, and hand over the command that "+
				"shows the reader the rest")

			reasons := m6PodWarningReasons(g, moverPod)
			g.Expect(reasons).NotTo(BeEmpty(),
				"the cluster recorded no Warning Event on mover pod %s to compare against", moverPod)
			matched := false
			for _, r := range reasons {
				if strings.Contains(joined, r) {
					matched = true
				}
			}
			g.Expect(matched).To(BeTrue(),
				"the Event does not quote any Warning the cluster recorded on pod %s (%v). It reads:\n%s",
				moverPod, reasons, joined)
		}, 2*time.Minute, 10*time.Second).Should(Succeed())

		By("And the run reaches a terminal phase rather than hanging")
		cb := m1WaitClusterBackupTerminal(stalledRun, 15*time.Minute)
		Expect(cb.Status.Phase).To(BeElementOf("Failed", "PartiallyFailed"))

		By("Restoring the RBD node plugin so the teardown of this run's objects can drain")
		m6RestoreDaemonSetNodeSelector(m6RBDNodePluginNS, rbdNodePlugin, originalNodeSelector)
		// The reconciler goes back after the unpark, never before: it would otherwise rewrite the
		// DaemonSet from the Driver CR while this spec is still restoring it, and whichever write
		// landed second would decide the cluster's state for the rest of the campaign.
		m6ScaleDeployment(m6RookOperatorNS, m6CSIOperatorDeploy, csiOperatorReplicas)

		// A volume failed by a deadline is a terminal volume like any other and goes through the
		// same teardown. The leak-check is the authority on what residue means; the mover Job and
		// its creds Secret are checked separately because they live in the operator namespace,
		// outside its per-tenant scope.
		By("And nothing leaked — a failed-by-deadline volume is torn down like any other")
		m1AssertNoResidualSnapshotObjects(m6StallNS)
		Eventually(func(g Gomega) {
			err := k8s.Get(ctx, client.ObjectKey{Namespace: operatorNS, Name: moverJob}, &batchv1.Job{})
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue(),
				"the mover Job of a failed-by-deadline volume survived: %v", err)
			err = k8s.Get(ctx, client.ObjectKey{Namespace: operatorNS, Name: moverJob}, &corev1.Secret{})
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue(),
				"the mover creds Secret survived: %v", err)
		}, 10*time.Minute, 15*time.Second).Should(Succeed())
	})

	// The other half of the contract, and the anti-regression on real infrastructure: the deadline
	// stops refusing the moment the cluster is fixed, with no CrystalBackup-side intervention — no
	// cached verdict, no cooldown, no object to delete first. It is also the closest a paid campaign
	// can get to the envtest anti-regression (a Running mover left alone): a real mover here runs
	// for as long as ceph and S3 take, with the deadline in force the whole time, and is not touched.
	It("completes the very next run once the node plugin is back", func() {
		By("Given every node can stage an RBD volume again")
		Eventually(func(g Gomega) {
			var ds appsv1.DaemonSet
			g.Expect(k8s.Get(ctx, client.ObjectKey{Namespace: m6RBDNodePluginNS, Name: rbdNodePlugin}, &ds)).To(Succeed())
			g.Expect(ds.Status.NumberReady).To(BeNumerically(">", 0),
				"the RBD node plugin has no ready pod; the restore in the previous It did not take")
		}, 10*time.Minute, 10*time.Second).Should(Succeed())

		By("When a backup runs over the same namespace")
		m1RunClusterBackup(recoveredRun, m1LocationName, cbv1.NamespaceSelector{MatchNames: []string{m6StallNS}})

		By("Then the volume completes — the refusal was a property of the cluster, not a sticky state")
		Eventually(func(g Gomega) {
			vol := m6PrecheckVolume(g, m6StallNS, recoveredRun, m6StallPVC)
			g.Expect(string(vol.Phase)).To(Equal("Completed"),
				"reason=%q — the deadline must stop firing as soon as mounting works again", vol.Reason)
			g.Expect(vol.SnapshotID).NotTo(BeEmpty(), "a completed volume must carry its restic snapshot id")
		}, 20*time.Minute, 15*time.Second).Should(Succeed())

		Expect(m1WaitClusterBackupTerminal(recoveredRun, 20*time.Minute).Status.Phase).To(Equal("Completed"))

		By("And nothing leaked")
		m1AssertNoResidualSnapshotObjects(m6StallNS)
	})
})

// ---------------------------------------------------------------------------
// Spec-local helpers (thin, file-scoped; the shared vocabulary lives in
// m1_helpers_test.go, m6_alerts_helpers_test.go and crucible_suite_test.go and
// is reused, never redefined).
// ---------------------------------------------------------------------------

// m6CaptureDaemonSetNodeSelector reads a DaemonSet's current nodeSelector, and FAILS the spec if the
// DaemonSet does not exist. The failure message matters: without it, a rook release that renames
// csi-rbdplugin would turn the fault injection below into a silent no-op, the backup would succeed,
// and the spec would report that the mover start deadline works — from a run in which no mover was
// ever prevented from starting.
func m6CaptureDaemonSetNodeSelector(namespace, name string) map[string]string {
	GinkgoHelper()
	var ds appsv1.DaemonSet
	Expect(k8s.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &ds)).To(Succeed(),
		"DaemonSet %s/%s not found — this spec injects its fault by parking the RBD NODE plugin, and "+
			"cannot reproduce the incident without it. If rook renamed it, point this spec at the new "+
			"name; do not let it pass by parking nothing.", namespace, name)

	out := map[string]string{}
	for k, v := range ds.Spec.Template.Spec.NodeSelector {
		out[k] = v
	}
	return out
}

// m6ParkDaemonSet stops a DaemonSet by selecting a node label that exists nowhere, then waits for
// its pods to actually be gone. A DaemonSet has no replica count, so this is the equivalent of
// scaling to zero — and the WAIT is not optional: the kubelet keeps serving CSI requests until the
// plugin pod is really terminated, so a spec that proceeded on the patch alone would race its own
// fault injection and occasionally observe a perfectly healthy backup.
func m6ParkDaemonSet(namespace, name string) {
	GinkgoHelper()
	m6SetDaemonSetNodeSelector(namespace, name, map[string]string{m6StallParkLabel: "true"})

	Eventually(func(g Gomega) {
		var ds appsv1.DaemonSet
		g.Expect(k8s.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &ds)).To(Succeed())
		g.Expect(ds.Status.CurrentNumberScheduled).To(BeZero(),
			"DaemonSet %s/%s still has %d scheduled pod(s)", namespace, name, ds.Status.CurrentNumberScheduled)
		g.Expect(ds.Status.NumberReady).To(BeZero())
	}, 5*time.Minute, 5*time.Second).Should(Succeed())
}

// m6RestoreDaemonSetNodeSelector puts the captured selector back. Idempotent, so both the It's
// DeferCleanup and the AfterAll can call it.
func m6RestoreDaemonSetNodeSelector(namespace, name string, selector map[string]string) {
	GinkgoHelper()
	m6SetDaemonSetNodeSelector(namespace, name, selector)
}

// m6SetDaemonSetNodeSelector replaces a DaemonSet's pod-template nodeSelector, retrying on the
// conflict a controller writing the same object concurrently produces.
func m6SetDaemonSetNodeSelector(namespace, name string, selector map[string]string) {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		var ds appsv1.DaemonSet
		g.Expect(k8s.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &ds)).To(Succeed())
		if len(selector) == 0 {
			ds.Spec.Template.Spec.NodeSelector = nil
		} else {
			ds.Spec.Template.Spec.NodeSelector = selector
		}
		g.Expect(k8s.Update(ctx, &ds)).To(Succeed())
	}, 2*time.Minute, 5*time.Second).Should(Succeed())
}

// m6StallMoverJob returns the name of the data-mover Job for this run, found by the labels every
// per-PVC object carries — never by reconstructing the operator's name derivation, which is a
// second implementation of a rule this suite does not own.
func m6StallMoverJob(g Gomega, run string) string {
	var jobs batchv1.JobList
	g.Expect(k8s.List(ctx, &jobs, client.InNamespace(operatorNS), client.MatchingLabels{
		apiconst.LabelManagedBy:     apiconst.ManagedByValue,
		apiconst.LabelClusterBackup: run,
		apiconst.LabelNamespace:     m6StallNS,
		apiconst.LabelPVC:           m6StallPVC,
	})).To(Succeed())
	g.Expect(jobs.Items).To(HaveLen(1), "expected exactly one mover Job for %s/%s in run %s",
		m6StallNS, m6StallPVC, run)
	return jobs.Items[0].Name
}

// m6StallMoverPod returns the pod of a mover Job, by the batch job-name label the Job controller
// stamps.
func m6StallMoverPod(g Gomega, jobName string) *corev1.Pod {
	var pods corev1.PodList
	g.Expect(k8s.List(ctx, &pods, client.InNamespace(operatorNS),
		client.MatchingLabels{batchv1.JobNameLabel: jobName})).To(Succeed())
	g.Expect(pods.Items).NotTo(BeEmpty(), "mover Job %s/%s has no pod yet", operatorNS, jobName)
	return &pods.Items[0]
}

// m6PodWarningMessages returns the messages of the Warning Events about one pod, read through the
// core/v1 view — the same view internal/controller's latestWarningEvent reads, and the one a
// kubelet's FailedMount lands in.
func m6PodWarningMessages(g Gomega, podName string) []string {
	var list corev1.EventList
	g.Expect(k8s.List(ctx, &list, client.InNamespace(operatorNS),
		client.MatchingFields{"involvedObject.name": podName})).To(Succeed())

	var out []string
	for i := range list.Items {
		e := &list.Items[i]
		if e.Type != corev1.EventTypeWarning {
			continue
		}
		out = append(out, strings.TrimSpace(e.Message))
	}
	return out
}

// m6PodWarningReasons returns the REASON of every Warning Event the cluster recorded about one
// pod — `FailedAttachVolume`, `FailedMount`, `FailedScheduling`, whatever this cluster's failure
// stage happens to produce.
//
// Reasons rather than messages, and read live rather than named as a constant, because that is
// the only honest way to state the property under test: the operator promised that
// `kubectl get backup` would be as informative as `kubectl describe pod`, and the way to check
// that two commands agree is to run both. A constant here would be this spec asserting how its
// own fault injection happens to fail — which is what it did, and what cost a campaign.
func m6PodWarningReasons(g Gomega, podName string) []string {
	var list corev1.EventList
	g.Expect(k8s.List(ctx, &list, client.InNamespace(operatorNS),
		client.MatchingFields{"involvedObject.name": podName})).To(Succeed())

	var out []string
	for i := range list.Items {
		e := &list.Items[i]
		if e.Type != corev1.EventTypeWarning {
			continue
		}
		if reason := strings.TrimSpace(e.Reason); reason != "" && !slices.Contains(out, reason) {
			out = append(out, reason)
		}
	}
	return out
}

// m6PrometheusConfigured reports whether the crucible's Prometheus is reachable, so the one
// metrics assertion in this spec can be skipped on a cluster deployed without it rather than
// failing a storage spec for a monitoring reason. The alert lane (m6_alerts_test.go) is where a
// missing Prometheus is a hard failure — see m6RequirePrometheus.
func m6PrometheusConfigured() bool {
	_, err := m6ActiveTargets()
	if err != nil {
		AddReportEntry("prometheus-unavailable",
			fmt.Sprintf("skipping the in-progress-series assertion: %v", err))
	}
	return err == nil
}

// m6FindRBDNodePlugin resolves the RBD node plugin DaemonSet by driver name rather than by a
// hardcoded object name.
//
// rook names it after the CSI driver and the convention has changed: this spec was written
// against `csi-rbdplugin` and the first campaign to run it met
// `rook-ceph.rbd.csi.ceph.com-nodeplugin`. Discovering it is the same decision, for the same
// reason, as m6FindSnapshotController — and the failure below is deliberately a hard one: a
// spec that could not find the thing whose absence IS its fault injection must not proceed to
// park nothing and then assert a deadline that would fire for some other reason entirely.
func m6FindRBDNodePlugin() string {
	GinkgoHelper()
	var sets appsv1.DaemonSetList
	Expect(k8s.List(ctx, &sets, client.InNamespace(m6RBDNodePluginNS))).To(Succeed())

	var names []string
	for i := range sets.Items {
		if strings.Contains(sets.Items[i].Name, m6RBDNodePluginMatch) {
			names = append(names, sets.Items[i].Name)
		}
	}
	Expect(names).NotTo(BeEmpty(),
		"no DaemonSet in %s matches %q. This spec injects its fault by parking the RBD NODE "+
			"plugin, and without it there is nothing to park: it would assert a mover-start "+
			"deadline against a cluster where mounting works, and the only honest outcome is "+
			"this failure rather than a green.",
		m6RBDNodePluginNS, m6RBDNodePluginMatch)
	slices.Sort(names) // deterministic if a distro ships more than one match
	return names[0]
}
