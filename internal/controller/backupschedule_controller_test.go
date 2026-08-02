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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
)

// ---------------------------------------------------------------------------
// BackupSchedule helpers.
// ---------------------------------------------------------------------------

// newTenantSchedule builds a not-yet-created BackupSchedule with a non-default run
// configuration, so a stamped Backup that merely inherited zero values is distinguishable from
// one that inherited the schedule's intent.
func newTenantSchedule(namespace, name, cronExpr, location string) *cbv1.BackupSchedule {
	off := false
	return &cbv1.BackupSchedule{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec: cbv1.BackupScheduleSpec{
			LocationRef:      cbv1.LocalObjectReference{Name: location},
			Schedule:         cronExpr,
			PVCSelector:      cbv1.PVCSelector{Exclude: []string{"scratch-*"}},
			IncludeManifests: &off,
			BackoffLimit:     4,
		},
	}
}

// createTenantSchedule creates s and registers cleanup of it AND of the Backups it stamps —
// which is the whole point of this controller's contract: nothing else would collect them,
// because the schedule deliberately does not own them.
func createTenantSchedule(s *cbv1.BackupSchedule) {
	GinkgoHelper()
	Expect(k8sClient.Create(ctx, s)).To(Succeed())
	DeferCleanup(func() {
		_ = k8sClient.Delete(context.Background(), s)
		_ = k8sClient.DeleteAllOf(context.Background(), &cbv1.Backup{},
			client.InNamespace(s.Namespace), client.MatchingLabels{apiconst.LabelSchedule: s.Name})
	})
}

// getTenantScheduleNow fetches a BackupSchedule outside an Eventually block.
func getTenantScheduleNow(namespace, name string) cbv1.BackupSchedule {
	GinkgoHelper()
	var s cbv1.BackupSchedule
	Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &s)).To(Succeed())
	return s
}

// getTenantScheduleG fetches a BackupSchedule inside an Eventually block.
func getTenantScheduleG(g Gomega, namespace, name string) cbv1.BackupSchedule {
	var s cbv1.BackupSchedule
	g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &s)).To(Succeed())
	return s
}

// listScheduleBackups returns the Backups a schedule stamped in its namespace.
func listScheduleBackups(namespace, name string) []cbv1.Backup {
	GinkgoHelper()
	var list cbv1.BackupList
	Expect(k8sClient.List(ctx, &list,
		client.InNamespace(namespace), client.MatchingLabels{apiconst.LabelSchedule: name})).To(Succeed())
	return list.Items
}

// seedScheduleBackup creates a Backup out-of-band — one no schedule stamped — and does not return
// until the reconciler's cache has OBSERVED it.
//
// The wait is the whole helper, and the specs that use it must call it BEFORE creating the schedule
// whose behaviour they measure. Both halves of that are load-bearing.
//
// A spec seeds a fixture Backup to put the schedule controller in a particular state — a run in
// flight, a projection sitting at an empty phase, another tenant's run — and then pokes the schedule
// to make it reconcile in that state. But k8sClient writes straight to the apiserver while the
// controller Lists from the shared informer cache, and the fixture's watch event and the poke's
// arrive on two different connections with nothing sequencing them. Whenever the poke wins, the
// controller reconciles against a list that does NOT contain the fixture: it is not in the state the
// spec set up, and whatever it does next is being judged against a premise that was never
// established. Instrumented on the Forbid fixture, that was 21 wrong answers in 200 runs — every one
// with the seeded Backup non-terminal (Pending) and the schedule emitting BackupStamped where the
// spec expected ConcurrencySkip. Not a slow controller: a blind one.
//
// Waiting on cachedReader closes that, but not the rest of it. Reconcile Lists the backups and only
// THEN reads the clock, so a pass that listed an empty namespace before the seed can still be
// sitting in that window when scheduleClock jumps, wake up with a tick due and nothing to block it,
// and stamp. No barrier reaches a reconcile that already started — measured, that residue was 1 in
// 200. Seeding first removes the window instead of narrowing it: with no schedule object there is
// nothing to reconcile, so every pass that ever runs began after the cache had the fixture.
func seedScheduleBackup(b *cbv1.Backup) {
	GinkgoHelper()
	Expect(k8sClient.Create(ctx, b)).To(Succeed())
	DeferCleanup(func() { _ = k8sClient.Delete(context.Background(), b) })
	key := client.ObjectKeyFromObject(b)
	Eventually(func(g Gomega) {
		g.Expect(cachedReader.Get(ctx, key, &cbv1.Backup{})).To(Succeed())
	}, eventuallyTimeout, eventuallyPoll).Should(Succeed())
}

// scheduleTickPair returns the next two minute activations of a `* * * * *` schedule about to be
// created, so a spec can name its fixture Backup before the schedule it belongs to exists.
//
// Deriving the ticks from wall-clock rather than from the schedule's creationTimestamp costs
// nothing in precision: DueTick returns the LATEST activation at or before now, so with the clock
// parked at second+30s it answers `second` whether the schedule's creationTimestamp landed in this
// minute or the next one. The spec is therefore immune to a minute rolling over mid-setup — which
// the creationTimestamp form was not, one tick being all it had.
func scheduleTickPair() (first, second time.Time) {
	base := time.Now().UTC().Truncate(time.Minute)
	return base.Add(time.Minute), base.Add(2 * time.Minute)
}

// pokeTenantSchedule forces a reconcile by writing an annotation (envtest requeues run on real
// time, so moving the fake clock alone would not re-trigger the controller).
func pokeTenantSchedule(namespace, name string) {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		var s cbv1.BackupSchedule
		g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &s)).To(Succeed())
		if s.Annotations == nil {
			s.Annotations = map[string]string{}
		}
		s.Annotations["test.crystalbackup.io/poke"] = time.Now().Format(time.RFC3339Nano)
		g.Expect(k8sClient.Update(ctx, &s)).To(Succeed())
	}, eventuallyTimeout, eventuallyPoll).Should(Succeed())
}

var _ = Describe("BackupScheduleReconciler", func() {
	BeforeEach(func() {
		// The fake clock is shared process-wide; re-anchor it so a freshly created schedule does
		// not look overdue on apply.
		scheduleClock.SetTime(time.Now())
	})

	It("does not stamp on apply, and reports a future nextScheduleTime", func() {
		const (
			ns   = "bs-apply-ns"
			name = "daily"
		)
		createTenantNamespace(ns)
		createTenantSchedule(newTenantSchedule(ns, name, "* * * * *", "bs-apply-loc"))

		Consistently(func(g Gomega) {
			g.Expect(listScheduleBackups(ns, name)).To(BeEmpty())
		}, 3*time.Second, 500*time.Millisecond).Should(Succeed())

		Eventually(func(g Gomega) {
			s := getTenantScheduleG(g, ns, name)
			g.Expect(s.Status.Phase).To(Equal("Active"))
			g.Expect(s.Status.NextScheduleTime).NotTo(BeNil())
		}, eventuallyTimeout, eventuallyPoll).Should(Succeed())
	})

	It("stamps an unowned Backup carrying the materialized run configuration", func() {
		const (
			ns   = "bs-fire-ns"
			name = "hourly"
			loc  = "bs-fire-loc"
		)
		createTenantNamespace(ns)
		createTenantSchedule(newTenantSchedule(ns, name, "* * * * *", loc))
		created := getTenantScheduleNow(ns, name)

		tick := created.CreationTimestamp.UTC().Truncate(time.Minute).Add(time.Minute)
		scheduleClock.SetTime(tick.Add(30 * time.Second))
		pokeTenantSchedule(ns, name)

		want := apiconst.RunName(name, tick)
		var b cbv1.Backup
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: want}, &b)).To(Succeed())
		}, eventuallyTimeout, eventuallyPoll).Should(Succeed())

		By("it points at the NAMESPACED location kind, in its own namespace")
		Expect(b.Spec.LocationRef.Kind).To(Equal(kindBackupLocation))
		Expect(b.Spec.LocationRef.Name).To(Equal(loc))
		Expect(b.Spec.ScheduleRef).To(Equal(name))
		Expect(b.Labels).To(HaveKeyWithValue(apiconst.LabelSchedule, name))
		Expect(b.Labels).To(HaveKeyWithValue(apiconst.LabelOrigin, apiconst.OriginNamespace))

		By("the run configuration travelled with it — there is no parent to pull from")
		Expect(b.Spec.Run).NotTo(BeNil())
		Expect(b.Spec.Run.PVCSelector.Exclude).To(Equal([]string{"scratch-*"}))
		Expect(b.Spec.Run.IncludeManifests).NotTo(BeNil())
		Expect(*b.Spec.Run.IncludeManifests).To(BeFalse())
		Expect(b.Spec.Run.BackoffLimit).To(Equal(int32(4)))
		// maxConcurrentMovers is a CLUSTER-WIDE cap, so it is not on the tenant surface at all and
		// must never be materialized from one.
		Expect(b.Spec.Run.MaxConcurrentMovers).To(BeZero())

		By("it is NOT owned: a Backup is a restore point, and must outlive its schedule")
		Expect(metav1.GetControllerOf(&b)).To(BeNil())
		Expect(b.OwnerReferences).To(BeEmpty())

		Eventually(func(g Gomega) {
			g.Expect(getTenantScheduleG(g, ns, name).Status.LastRunName).To(Equal(want))
		}, eventuallyTimeout, eventuallyPoll).Should(Succeed())
	})

	// The reason spec.paused exists on a tenant-facing type at all. Before it, the only way a
	// namespace could suspend its own backups was `kubectl delete backupschedule`, which took the
	// history AND the baseline tick with it — a recreated schedule restarts from its new
	// creationTimestamp and has no memory of when it last succeeded. Pausing has to be the
	// reversible operation deleting is not, and that is a claim about STATUS, not about stamping.
	It("stamps nothing while paused, and preserves the status it had", func() {
		const (
			ns   = "bs-paused-ns"
			name = "pausable"
		)
		createTenantNamespace(ns)
		createTenantSchedule(newTenantSchedule(ns, name, "* * * * *", "bs-paused-loc"))
		created := getTenantScheduleNow(ns, name)

		By("firing once so there is a status worth preserving")
		tick := created.CreationTimestamp.UTC().Truncate(time.Minute).Add(time.Minute)
		scheduleClock.SetTime(tick.Add(30 * time.Second))
		pokeTenantSchedule(ns, name)

		firstRun := apiconst.RunName(name, tick)
		Eventually(func(g Gomega) {
			g.Expect(getTenantScheduleG(g, ns, name).Status.LastRunName).To(Equal(firstRun))
		}, eventuallyTimeout, eventuallyPoll).Should(Succeed())

		// lastSuccessTime is written here rather than driven through a real Backup because the
		// property under test is that the pause branch does not TOUCH it. Its provenance is
		// irrelevant; its survival is the whole point.
		success := metav1.NewTime(tick.Add(time.Minute).UTC().Truncate(time.Second))
		Eventually(func(g Gomega) {
			s := getTenantScheduleG(g, ns, name)
			s.Status.LastSuccessTime = &success
			g.Expect(k8sClient.Status().Update(ctx, &s)).To(Succeed())
		}, eventuallyTimeout, eventuallyPoll).Should(Succeed())

		By("pausing it")
		Eventually(func(g Gomega) {
			s := getTenantScheduleG(g, ns, name)
			s.Spec.Paused = true
			g.Expect(k8sClient.Update(ctx, &s)).To(Succeed())
		}, eventuallyTimeout, eventuallyPoll).Should(Succeed())

		scheduleClock.SetTime(tick.Add(10 * time.Minute))
		pokeTenantSchedule(ns, name)

		By("the schedule reports Paused and keeps every status field it had")
		Eventually(func(g Gomega) {
			s := getTenantScheduleG(g, ns, name)
			g.Expect(s.Status.Phase).To(Equal("Paused"))
			g.Expect(s.Status.LastRunName).To(Equal(firstRun))
			g.Expect(s.Status.LastSuccessTime).NotTo(BeNil())
			g.Expect(s.Status.LastSuccessTime.Time.UTC()).To(Equal(success.Time.UTC()))
		}, eventuallyTimeout, eventuallyPoll).Should(Succeed())

		By("and no further tick is stamped, though ten of them have passed")
		Consistently(func(g Gomega) {
			g.Expect(listScheduleBackups(ns, name)).To(HaveLen(1))
		}, 3*time.Second, 500*time.Millisecond).Should(Succeed())
	})

	It("leaves its Backups standing when the schedule itself is deleted", func() {
		const (
			ns   = "bs-delete-ns"
			name = "doomed"
		)
		createTenantNamespace(ns)
		sched := newTenantSchedule(ns, name, "* * * * *", "bs-delete-loc")
		createTenantSchedule(sched)
		created := getTenantScheduleNow(ns, name)

		tick := created.CreationTimestamp.UTC().Truncate(time.Minute).Add(time.Minute)
		scheduleClock.SetTime(tick.Add(30 * time.Second))
		pokeTenantSchedule(ns, name)

		want := apiconst.RunName(name, tick)
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: want}, &cbv1.Backup{})).To(Succeed())
		}, eventuallyTimeout, eventuallyPoll).Should(Succeed())

		Expect(k8sClient.Delete(ctx, sched)).To(Succeed())
		Eventually(func(g Gomega) {
			err := k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &cbv1.BackupSchedule{})
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
		}, eventuallyTimeout, eventuallyPoll).Should(Succeed())

		// The whole reason the schedule does not own its Backups: an ownerReference here would be
		// LEGAL (both namespaced), and would turn `kubectl delete backupschedule` into deleting
		// every restore point the user has — while the snapshots stay in their bucket, now
		// invisible from the cluster.
		Consistently(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: want}, &cbv1.Backup{})).To(Succeed())
		}, consistentlyWindow, initPoll).Should(Succeed())
	})

	It("skips a tick while a previous Backup is still running (Forbid)", func() {
		const (
			ns   = "bs-forbid-ns"
			name = "overlapping"
		)
		createTenantNamespace(ns)
		firstTick, secondTick := scheduleTickPair()

		// A non-terminal Backup carrying the schedule label: exactly what an in-flight run looks
		// like to this controller. It stays non-terminal for the whole spec by construction —
		// bs-forbid-loc does not exist and the Backup carries no spec.run, so the Backup reconciler
		// parks it at Pending on a gate and never advances it.
		//
		// Seeded BEFORE the schedule exists, which is the only ordering that makes the assertion
		// below sound. See seedScheduleBackup.
		seedScheduleBackup(&cbv1.Backup{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: ns,
				Name:      apiconst.RunName(name, firstTick),
				Labels:    map[string]string{apiconst.LabelSchedule: name},
			},
			Spec: cbv1.BackupSpec{
				LocationRef: cbv1.LocationReference{Kind: kindBackupLocation, Name: "bs-forbid-loc"},
			},
		})
		createTenantSchedule(newTenantSchedule(ns, name, "* * * * *", "bs-forbid-loc"))

		scheduleClock.SetTime(secondTick.Add(30 * time.Second))
		pokeTenantSchedule(ns, name)

		Consistently(func(g Gomega) {
			err := k8sClient.Get(ctx,
				client.ObjectKey{Namespace: ns, Name: apiconst.RunName(name, secondTick)}, &cbv1.Backup{})
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
		}, consistentlyWindow, initPoll).Should(Succeed())
	})

	It("is not blocked by a discovery PROJECTION sitting at an empty phase", func() {
		const (
			ns   = "bs-projection-ns"
			name = "unblocked"
		)
		createTenantNamespace(ns)
		firstTick, secondTick := scheduleTickPair()

		// A projection has no phase and never will: it is a materialized view of snapshots that
		// already exist. Counting one as "active" would wedge the schedule permanently — every
		// future tick skipped, forever, with a Forbid Event as the only clue.
		//
		// Seeded before the schedule for the reason spelled out in seedScheduleBackup, and here that
		// ordering is what gives the spec any teeth at all: a projection the controller has not
		// observed is indistinguishable from no projection, so the tick would be stamped whether the
		// skip in activeBackup existed or not, and the spec would go green on a controller that had
		// lost it.
		seedScheduleBackup(&cbv1.Backup{
			ObjectMeta: metav1.ObjectMeta{
				Namespace:   ns,
				Name:        apiconst.RunName(name, firstTick),
				Labels:      map[string]string{apiconst.LabelSchedule: name},
				Annotations: map[string]string{apiconst.AnnotationProjected: apiconst.AnnotationProjectedValue},
			},
			Spec: cbv1.BackupSpec{
				LocationRef: cbv1.LocationReference{Kind: kindBackupLocation, Name: "bs-projection-loc"},
			},
		})
		createTenantSchedule(newTenantSchedule(ns, name, "* * * * *", "bs-projection-loc"))

		scheduleClock.SetTime(secondTick.Add(30 * time.Second))
		pokeTenantSchedule(ns, name)

		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx,
				client.ObjectKey{Namespace: ns, Name: apiconst.RunName(name, secondTick)}, &cbv1.Backup{})).To(Succeed())
		}, eventuallyTimeout, eventuallyPoll).Should(Succeed())
	})

	It("does not see another namespace's schedule of the same name", func() {
		const (
			nsA  = "bs-isolation-a"
			nsB  = "bs-isolation-b"
			name = "daily"
		)
		createTenantNamespace(nsA)
		createTenantNamespace(nsB)
		firstTick, secondTick := scheduleTickPair()

		// An in-flight Backup in A must not gate B's tick. A cluster-wide list would make one
		// tenant's slow backup silently suppress another tenant's schedule. Seeded before either
		// schedule exists, per seedScheduleBackup: a Backup in A that no reconcile has observed gates
		// nothing anywhere, so B firing over one would prove nothing about the namespace scoping.
		seedScheduleBackup(&cbv1.Backup{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: nsA,
				Name:      apiconst.RunName(name, firstTick),
				Labels:    map[string]string{apiconst.LabelSchedule: name},
			},
			Spec: cbv1.BackupSpec{
				LocationRef: cbv1.LocationReference{Kind: kindBackupLocation, Name: "bs-iso-loc"},
			},
		})
		createTenantSchedule(newTenantSchedule(nsA, name, "* * * * *", "bs-iso-loc"))
		createTenantSchedule(newTenantSchedule(nsB, name, "* * * * *", "bs-iso-loc"))

		scheduleClock.SetTime(secondTick.Add(30 * time.Second))
		pokeTenantSchedule(nsB, name)

		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx,
				client.ObjectKey{Namespace: nsB, Name: apiconst.RunName(name, secondTick)}, &cbv1.Backup{})).To(Succeed())
		}, eventuallyTimeout, eventuallyPoll).Should(Succeed())
	})

	It("surfaces an invalid cron expression instead of hot-looping", func() {
		const (
			ns   = "bs-badcron-ns"
			name = "broken"
		)
		createTenantNamespace(ns)
		createTenantSchedule(newTenantSchedule(ns, name, "not a cron expression", "bs-badcron-loc"))

		Eventually(func(g Gomega) {
			s := getTenantScheduleG(g, ns, name)
			g.Expect(s.Status.Phase).To(Equal("InvalidSchedule"))
		}, eventuallyTimeout, eventuallyPoll).Should(Succeed())
	})
})
