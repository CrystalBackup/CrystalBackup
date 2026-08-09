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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
	"github.com/CrystalBackup/CrystalBackup/internal/status"
)

// ---------------------------------------------------------------------------
// The abandoned-run decision, driven end to end through both planes' schedules.
//
// HOW THESE SPECS MAKE A RUN LOOK OLD, and why it is not a cheat. Every clock the decision reads
// belongs to the objects themselves (creationTimestamp, the Ready condition's lastTransitionTime,
// status.volumes[].firstAttemptAt) and the apiserver owns the first of those — a spec cannot backdate
// it. What a spec CAN move is the schedule's own "now", which is the injected fake clock these
// controllers already read every other decision from. Parking it six hours ahead of the fixtures is
// therefore the same arithmetic production does at 04:00 the next morning, run six hours early.
//
// It also means the two specs that matter here differ in ONE input. Both have a six-hour-old
// previous run, both are past the grace, both are judged by the same code at the same fake instant.
// The only difference is whether one volume of that run is Uploading — and that single bit has to be
// the difference between a terminated run and a skipped tick, or the predicate is not doing its job.
// ---------------------------------------------------------------------------

// abandonmentSkew is how far ahead of its fixtures a spec parks the schedule clock. Comfortably past
// scheduleAbandonmentGrace, and — with a `* * * * *` cron — inside DueTick's catch-up scan, so
// exactly ONE tick is due: the run stamped for it advances the baseline and no further tick fires
// while the clock stays parked, which is what keeps these specs from watching a kill/stamp treadmill.
const abandonmentSkew = 6 * time.Hour

// abandonedReasonOf returns the Ready condition's reason on a Backup, or "" when it has none. It is
// the one field an administrator reads to tell "we killed a stuck run" from "the run failed on its
// own", so it is what these specs assert on.
func abandonedReasonOf(b *cbv1.Backup) string {
	if c := status.FindCondition(b.Status.Conditions, ConditionReady); c != nil {
		return c.Reason
	}
	return ""
}

// abandonedReasonOfRun is abandonedReasonOf for a ClusterBackup.
func abandonedReasonOfRun(cb *cbv1.ClusterBackup) string {
	if c := status.FindCondition(cb.Status.Conditions, ConditionReady); c != nil {
		return c.Reason
	}
	return ""
}

// seedBackupVolumes writes a per-volume status onto an out-of-band Backup and does not return until
// the reconciler's cache has observed it.
//
// The retry loop is not politeness. The live Backup reconciler gates this fixture (it has no run
// spec) and writes the WHOLE status subresource when it does, so a reconcile that read the object
// before this write and landed after it would drop the volumes — and the spec would then be judged
// against a premise it never established, which is the failure mode seedScheduleBackup's doc
// describes at length. Re-asserting until the cache shows the volumes closes it.
func seedBackupVolumes(namespace, name string, volumes []cbv1.VolumeStatus) {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		var b cbv1.Backup
		g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &b)).To(Succeed())
		b.Status.Volumes = volumes
		g.Expect(k8sClient.Status().Update(ctx, &b)).To(Succeed())

		var cached cbv1.Backup
		g.Expect(cachedReader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &cached)).To(Succeed())
		g.Expect(cached.Status.Volumes).To(HaveLen(len(volumes)))
		g.Expect(cached.Status.Volumes[0].Phase).To(Equal(volumes[0].Phase))
	}, initTimeout, initPoll).Should(Succeed())
}

var _ = Describe("The abandoned-run decision on the namespace plane", func() {
	BeforeEach(func() { scheduleClock.SetTime(time.Now()) })

	It("TERMINATES a wedged previous backup and runs the due tick", func() {
		const (
			ns   = "bs-abandoned-ns"
			name = "nightly-wedged"
		)
		createTenantNamespace(ns)
		firstTick, _ := scheduleTickPair()

		// The incident's shape, reduced to its essentials: a run that never got as far as enumerating
		// a PVC (bs-abandoned-loc does not exist and it carries no spec.run, so the Backup reconciler
		// parks it on a gate forever) and therefore has nothing in flight and nothing to lose.
		// Seeded BEFORE the schedule, for the reason spelled out in seedScheduleBackup.
		wedged := apiconst.RunName(name, firstTick)
		seedScheduleBackup(&cbv1.Backup{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: ns,
				Name:      wedged,
				Labels:    map[string]string{apiconst.LabelSchedule: name},
			},
			Spec: cbv1.BackupSpec{
				LocationRef: cbv1.LocationReference{Kind: kindBackupLocation, Name: "bs-abandoned-loc"},
			},
		})
		createTenantSchedule(newTenantSchedule(ns, name, "* * * * *", "bs-abandoned-loc"))

		scheduleClock.SetTime(firstTick.Add(abandonmentSkew + 30*time.Second))
		pokeTenantSchedule(ns, name)

		By("the wedged backup is made terminal, and SAYS it was terminated as abandoned")
		Eventually(func(g Gomega) {
			var b cbv1.Backup
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: wedged}, &b)).To(Succeed())
			g.Expect(isTerminalBackupPhase(b.Status.Phase)).To(BeTrue(),
				"a run left non-terminal would go on blocking every future tick, which is the whole defect")
			// The distinction an admin needs: not "Failed", which is what a run that broke on its own
			// reports, but the reason naming the kill.
			g.Expect(abandonedReasonOf(&b)).To(Equal(reasonTerminatedAsAbandoned))
			g.Expect(b.Status.CompletionTime).NotTo(BeNil())
		}, eventuallyTimeout, eventuallyPoll).Should(Succeed())

		By("and the due tick actually RUNS — the point of the whole exercise")
		Eventually(func(g Gomega) {
			g.Expect(len(listScheduleBackups(ns, name))).To(BeNumerically(">=", 2))
		}, eventuallyTimeout, eventuallyPoll).Should(Succeed())
	})

	It("does NOT terminate a previous backup whose mover is still uploading", func() {
		// THE SPEC THAT GUARDS THE DANGEROUS FAILURE MODE.
		//
		// A first full backup of a multi-terabyte volume legitimately uploads for many hours with
		// nothing visibly changing. Killing it tears down a mover mid-flight and re-uploads terabytes
		// on the next run — strictly worse than the wedge this feature closes, and the kind of defect
		// that is discovered by a customer rather than by us.
		//
		// Every input here is identical to the spec above — same fixture shape, same six-hour skew,
		// same code path, same fake instant — EXCEPT that one volume is Uploading. That one bit must
		// be enough.
		const (
			ns   = "bs-uploading-ns"
			name = "nightly-uploading"
		)
		createTenantNamespace(ns)
		firstTick, _ := scheduleTickPair()

		uploading := apiconst.RunName(name, firstTick)
		seedScheduleBackup(&cbv1.Backup{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: ns,
				Name:      uploading,
				Labels:    map[string]string{apiconst.LabelSchedule: name},
			},
			Spec: cbv1.BackupSpec{
				LocationRef: cbv1.LocationReference{Kind: kindBackupLocation, Name: "bs-uploading-loc"},
			},
		})
		seedBackupVolumes(ns, uploading, []cbv1.VolumeStatus{
			// The 4 TB volume the whole safety argument is about.
			{Pvc: "big-data", Phase: status.VolumePhaseUploading},
			// ...beside a sibling that IS out of time, so the spec cannot pass merely because nothing
			// in the run looked abandonable.
			{Pvc: "already-failed", Phase: status.VolumePhaseFailed, Reason: backupReasonMoverStartDeadline},
		})
		createTenantSchedule(newTenantSchedule(ns, name, "* * * * *", "bs-uploading-loc"))

		scheduleClock.SetTime(firstTick.Add(abandonmentSkew + 30*time.Second))
		pokeTenantSchedule(ns, name)

		By("the uploading backup is left strictly alone and the tick is SKIPPED")
		Consistently(func(g Gomega) {
			var b cbv1.Backup
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: uploading}, &b)).To(Succeed())
			g.Expect(abandonedReasonOf(&b)).NotTo(Equal(reasonTerminatedAsAbandoned),
				"a running mover was terminated: this is the failure mode that destroys real backups")
			g.Expect(isTerminalBackupPhase(b.Status.Phase)).To(BeFalse())
			g.Expect(volumeByPVC(b, "big-data").Phase).To(Equal(status.VolumePhaseUploading))
			g.Expect(listScheduleBackups(ns, name)).To(HaveLen(1))
		}, consistentlyWindow, initPoll).Should(Succeed())
	})

	It("does NOT terminate a previous backup that is still inside its Pending budget", func() {
		// The other half of the veto, on the one phase the decision does judge. A volume picked up a
		// moment ago is resolving, not stuck: pendingResolveDeadline is the controller's business, and
		// the schedule must not pre-empt it.
		const (
			ns   = "bs-resolving-ns"
			name = "nightly-resolving"
		)
		createTenantNamespace(ns)
		firstTick, _ := scheduleTickPair()

		resolving := apiconst.RunName(name, firstTick)
		seedScheduleBackup(&cbv1.Backup{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: ns,
				Name:      resolving,
				Labels:    map[string]string{apiconst.LabelSchedule: name},
			},
			Spec: cbv1.BackupSpec{
				LocationRef: cbv1.LocationReference{Kind: kindBackupLocation, Name: "bs-resolving-loc"},
			},
		})
		// firstAttemptAt is stamped at the fake instant the spec parks the clock at, so the volume is
		// zero seconds into its one-hour Pending budget when the decision runs.
		attempt := metav1.NewTime(firstTick.Add(abandonmentSkew + 30*time.Second))
		seedBackupVolumes(ns, resolving, []cbv1.VolumeStatus{
			{Pvc: "slow-to-resolve", Phase: status.VolumePhasePending, FirstAttemptAt: &attempt},
		})
		createTenantSchedule(newTenantSchedule(ns, name, "* * * * *", "bs-resolving-loc"))

		scheduleClock.SetTime(firstTick.Add(abandonmentSkew + 30*time.Second))
		pokeTenantSchedule(ns, name)

		Consistently(func(g Gomega) {
			var b cbv1.Backup
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: resolving}, &b)).To(Succeed())
			g.Expect(abandonedReasonOf(&b)).NotTo(Equal(reasonTerminatedAsAbandoned))
			g.Expect(listScheduleBackups(ns, name)).To(HaveLen(1))
		}, consistentlyWindow, initPoll).Should(Succeed())
	})
})

var _ = Describe("The abandoned-run decision on the cluster plane", func() {
	BeforeEach(func() { scheduleClock.SetTime(time.Now()) })

	It("TERMINATES a wedged run AND its children, then runs the due tick", func() {
		const (
			name     = "cbs-abandoned"
			childNS  = "cbs-abandoned-ns"
			location = "cbs-abandoned-loc" // deliberately absent: the run blocks on LocationNotFound
		)
		createTenantNamespace(childNS)
		firstTick, _ := scheduleTickPair()
		wedged := apiconst.RunName(name, firstTick)

		// The fan-out's child. Killing the parent while this is still executing would strand its
		// exposure, its mover Job and its creds Secret — so the kill goes through the child first and
		// lets the child's OWN controller run the re-entrant teardown sweep.
		child := &cbv1.Backup{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: childNS,
				Name:      wedged,
				Labels: map[string]string{
					apiconst.LabelClusterBackup: wedged,
					apiconst.LabelOrigin:        apiconst.OriginCluster,
					apiconst.LabelNamespace:     childNS,
				},
			},
			Spec: cbv1.BackupSpec{
				LocationRef: cbv1.LocationReference{Kind: kindClusterBackupLocation, Name: location},
			},
		}
		seedScheduleBackup(child)

		run := &cbv1.ClusterBackup{
			ObjectMeta: metav1.ObjectMeta{
				Name:   wedged,
				Labels: map[string]string{apiconst.LabelSchedule: name},
			},
			Spec: cbv1.ClusterBackupSpec{
				ScheduleRef: name,
				ClusterBackupRunSpec: cbv1.ClusterBackupRunSpec{
					LocationRef: cbv1.LocalObjectReference{Name: location},
					Namespaces:  cbv1.NamespaceSelector{MatchNames: []string{childNS}},
				},
			},
		}
		Expect(k8sClient.Create(ctx, run)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(context.Background(), run) })
		Eventually(func(g Gomega) {
			g.Expect(cachedReader.Get(ctx, client.ObjectKey{Name: wedged}, &cbv1.ClusterBackup{})).To(Succeed())
		}, initTimeout, initPoll).Should(Succeed())

		createSchedule(newSchedule(name, "* * * * *", location))
		scheduleClock.SetTime(firstTick.Add(abandonmentSkew + 30*time.Second))
		pokeSchedule(name)

		By("the child backup is terminated first, so nothing of it is left in flight")
		Eventually(func(g Gomega) {
			var c cbv1.Backup
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: childNS, Name: wedged}, &c)).To(Succeed())
			g.Expect(isTerminalBackupPhase(c.Status.Phase)).To(BeTrue())
			g.Expect(abandonedReasonOf(&c)).To(Equal(reasonTerminatedAsAbandoned))
		}, eventuallyTimeout, eventuallyPoll).Should(Succeed())

		By("the run itself goes terminal, naming the kill and carrying the evidence")
		Eventually(func(g Gomega) {
			var r cbv1.ClusterBackup
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: wedged}, &r)).To(Succeed())
			g.Expect(isTerminalClusterBackupPhase(r.Status.Phase)).To(BeTrue())
			g.Expect(abandonedReasonOfRun(&r)).To(Equal(reasonTerminatedAsAbandoned))
			g.Expect(r.Status.CompletionTime).NotTo(BeNil())
			g.Expect(r.Status.Failures).NotTo(BeEmpty())
		}, eventuallyTimeout, eventuallyPoll).Should(Succeed())

		By("and the due tick runs")
		Eventually(func(g Gomega) {
			g.Expect(len(listScheduleRuns(name))).To(BeNumerically(">=", 2))
		}, eventuallyTimeout, eventuallyPoll).Should(Succeed())
	})

	It("does NOT terminate a run when ONE of its namespaces is still uploading", func() {
		// The fleet-scale version of the dangerous failure mode: a 33-namespace run must not be shot
		// because 32 of its namespaces have finished. One live upload anywhere vetoes the kill for the
		// whole run.
		const (
			name     = "cbs-uploading"
			doneNS   = "cbs-uploading-done"
			busyNS   = "cbs-uploading-busy"
			location = "cbs-uploading-loc"
		)
		createTenantNamespace(doneNS)
		createTenantNamespace(busyNS)
		firstTick, _ := scheduleTickPair()
		live := apiconst.RunName(name, firstTick)

		childLabels := func(ns string) map[string]string {
			return map[string]string{
				apiconst.LabelClusterBackup: live,
				apiconst.LabelOrigin:        apiconst.OriginCluster,
				apiconst.LabelNamespace:     ns,
			}
		}
		for _, ns := range []string{doneNS, busyNS} {
			seedScheduleBackup(&cbv1.Backup{
				ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: live, Labels: childLabels(ns)},
				Spec: cbv1.BackupSpec{
					LocationRef: cbv1.LocationReference{Kind: kindClusterBackupLocation, Name: location},
				},
			})
		}
		// One namespace finished long ago; the other is four hours into a multi-terabyte upload.
		seedBackupVolumes(doneNS, live, []cbv1.VolumeStatus{
			{Pvc: "small", Phase: status.VolumePhaseCompleted, SnapshotID: "deadbeef"},
		})
		seedBackupVolumes(busyNS, live, []cbv1.VolumeStatus{
			{Pvc: "big-data", Phase: status.VolumePhaseUploading},
		})

		run := &cbv1.ClusterBackup{
			ObjectMeta: metav1.ObjectMeta{Name: live, Labels: map[string]string{apiconst.LabelSchedule: name}},
			Spec: cbv1.ClusterBackupSpec{
				ScheduleRef: name,
				ClusterBackupRunSpec: cbv1.ClusterBackupRunSpec{
					LocationRef: cbv1.LocalObjectReference{Name: location},
					Namespaces:  cbv1.NamespaceSelector{MatchNames: []string{doneNS, busyNS}},
				},
			},
		}
		Expect(k8sClient.Create(ctx, run)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(context.Background(), run) })
		Eventually(func(g Gomega) {
			g.Expect(cachedReader.Get(ctx, client.ObjectKey{Name: live}, &cbv1.ClusterBackup{})).To(Succeed())
		}, initTimeout, initPoll).Should(Succeed())

		createSchedule(newSchedule(name, "* * * * *", location))
		scheduleClock.SetTime(firstTick.Add(abandonmentSkew + 30*time.Second))
		pokeSchedule(name)

		By("neither the run nor the uploading namespace is touched, and the tick is skipped")
		Consistently(func(g Gomega) {
			var r cbv1.ClusterBackup
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: live}, &r)).To(Succeed())
			g.Expect(abandonedReasonOfRun(&r)).NotTo(Equal(reasonTerminatedAsAbandoned),
				"a run with a live mover was terminated: this destroys backups people need")

			var busy cbv1.Backup
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: busyNS, Name: live}, &busy)).To(Succeed())
			g.Expect(abandonedReasonOf(&busy)).NotTo(Equal(reasonTerminatedAsAbandoned))
			g.Expect(volumeByPVC(busy, "big-data").Phase).To(Equal(status.VolumePhaseUploading))

			g.Expect(listScheduleRuns(name)).To(HaveLen(1))
		}, consistentlyWindow, initPoll).Should(Succeed())
	})
})
