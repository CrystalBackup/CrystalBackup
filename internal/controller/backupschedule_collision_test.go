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

	. "github.com/onsi/ginkgo/v2" //nolint:revive,staticcheck
	. "github.com/onsi/gomega"    //nolint:revive,staticcheck

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
	"github.com/CrystalBackup/CrystalBackup/internal/status"
)

// ---------------------------------------------------------------------------
// The CROSS-PLANE run-name collision, from the tenant's side.
//
// Both planes build run names with the SAME apiconst.RunName(schedule, tick). A
// ClusterBackupSchedule named "daily" covering this namespace and a BackupSchedule
// named "daily" in it, on the same cron, therefore produce a byte-identical
// (namespace, name) — and nobody did anything unusual to get there. Two admins
// picked the same word.
//
// Whichever plane stamps first, the second used to do nothing and say nothing:
//   - cluster first  → the tenant's stampBackup got AlreadyExists and returned the
//     name as "already fired this tick", so the tenant's backup NEVER RAN while the
//     schedule stayed green and lastRunName pointed at the cluster plane's object,
//     in a repository the tenant may not even be able to read;
//   - tenant first    → the cluster fan-out no-oped, and the DR repository got
//     nothing for this namespace while the run reported success (covered in
//     clusterbackup_collision_test.go).
//
// A foreign occupant also poisoned everything the schedule derives from its own
// history: it carries crystalbackup.io/schedule with the same value, so it advanced
// baselineTick (silently SKIPPING ticks) and blocked the next run through
// activeBackup's Forbid gate.
// ---------------------------------------------------------------------------

// seedClusterPlaneBackup places a cluster-DR fan-out child at (ns, name): cluster origin, the
// same schedule label the tenant's cron selects on, and somebody else's parent UID.
func seedClusterPlaneBackup(ns, name, schedName, loc string) *cbv1.Backup {
	GinkgoHelper()
	b := &cbv1.Backup{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      name,
			Labels: map[string]string{
				apiconst.LabelSchedule:      schedName,
				apiconst.LabelOrigin:        apiconst.OriginCluster,
				apiconst.LabelClusterBackup: name,
				apiconst.LabelNamespace:     ns,
			},
			Annotations: map[string]string{apiconst.AnnotationParentUID: string(testOtherUID)},
		},
		Spec: cbv1.BackupSpec{
			LocationRef: cbv1.LocationReference{Kind: kindClusterBackupLocation, Name: loc},
		},
	}
	Expect(k8sClient.Create(ctx, b)).To(Succeed())
	DeferCleanup(func() { _ = k8sClient.Delete(context.Background(), b) })
	return b
}

var _ = Describe("BackupScheduleReconciler run-name collisions", func() {
	BeforeEach(func() { scheduleClock.SetTime(time.Now()) })

	It("stamps its own UID on the Backups it fires", func() {
		const (
			ns   = "bs-stamp-ns"
			name = "nightly"
			loc  = "bs-stamp-loc"
		)
		createTenantNamespace(ns)
		createTenantSchedule(newTenantSchedule(ns, name, "* * * * *", loc))
		created := getTenantScheduleNow(ns, name)

		tick := created.CreationTimestamp.UTC().Truncate(time.Minute).Add(time.Minute)
		scheduleClock.SetTime(tick.Add(30 * time.Second))
		pokeTenantSchedule(ns, name)

		want := apiconst.RunName(name, tick)
		Eventually(func(g Gomega) {
			var b cbv1.Backup
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: want}, &b)).To(Succeed())
			g.Expect(b.Annotations).To(HaveKeyWithValue(apiconst.AnnotationParentUID, string(created.UID)),
				"without the schedule's UID, AlreadyExists cannot be told from a foreign occupant")
		}, eventuallyTimeout, eventuallyPoll).Should(Succeed())
	})

	// THE regression spec on this plane: an identically named ClusterBackupSchedule got there
	// first. Before the fix, the schedule reported Active with a lastRunName it did not stamp and
	// the tenant's data was never backed up.
	It("reports RunNameCollision instead of silently treating a foreign Backup as its own run", func() {
		const (
			ns   = "bs-collide-ns"
			name = "daily"
			loc  = "bs-collide-loc"
		)
		createTenantNamespace(ns)
		createTenantSchedule(newTenantSchedule(ns, name, "* * * * *", loc))
		created := getTenantScheduleNow(ns, name)

		tick := created.CreationTimestamp.UTC().Truncate(time.Minute).Add(time.Minute)
		taken := apiconst.RunName(name, tick)

		By("Given the cluster plane already occupying this tick's name in the tenant namespace")
		seedClusterPlaneBackup(ns, taken, name, loc)

		By("When the tenant's schedule reaches that tick")
		scheduleClock.SetTime(tick.Add(30 * time.Second))
		pokeTenantSchedule(ns, name)

		By("Then the schedule says the tick did NOT run, and names the cause")
		Eventually(func(g Gomega) {
			s := getTenantScheduleG(g, ns, name)
			g.Expect(s.Status.Phase).To(Equal(reasonRunNameCollision),
				"a tick that did not run must never leave the schedule reading Active")
			c := status.FindCondition(s.Status.Conditions, ConditionReady)
			g.Expect(c).NotTo(BeNil())
			g.Expect(c.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(c.Reason).To(Equal(reasonRunNameCollision))
			g.Expect(c.Message).To(ContainSubstring(taken))
		}, eventuallyTimeout, eventuallyPoll).Should(Succeed())

		By("And it never claims the other plane's Backup as its own last run")
		Consistently(func(g Gomega) {
			s := getTenantScheduleG(g, ns, name)
			g.Expect(s.Status.LastRunName).NotTo(Equal(taken),
				"lastRunName must not point at a Backup this schedule did not stamp")
			g.Expect(s.Status.LastSuccessTime).To(BeNil())
		}, 3*time.Second, 500*time.Millisecond).Should(Succeed())

		By("And the occupant is left untouched — no adoption, no stamp")
		var occupant cbv1.Backup
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: taken}, &occupant)).To(Succeed())
		Expect(occupant.Annotations).To(HaveKeyWithValue(apiconst.AnnotationParentUID, string(testOtherUID)))
	})

	// The quieter half of the same bug: the schedule label is not plane-qualified, so the cluster
	// plane's child looked like the tenant's own history. It advanced baselineTick past ticks the
	// tenant never fired and held the Forbid gate shut behind an in-flight run that was not the
	// tenant's.
	It("does not read the other plane's Backups as its own history", func() {
		const (
			ns   = "bs-foreignhist-ns"
			name = "daily"
			loc  = "bs-foreignhist-loc"
		)
		createTenantNamespace(ns)
		createTenantSchedule(newTenantSchedule(ns, name, "* * * * *", loc))
		created := getTenantScheduleNow(ns, name)

		base := created.CreationTimestamp.UTC().Truncate(time.Minute)
		ownTick := base.Add(time.Minute)
		// A cluster-plane child for a LATER tick, still in flight. Under the old list it both
		// pushed the baseline past ownTick and tripped the Forbid concurrency gate.
		foreign := apiconst.RunName(name, base.Add(10*time.Minute))
		seedClusterPlaneBackup(ns, foreign, name, loc)

		scheduleClock.SetTime(ownTick.Add(30 * time.Second))
		pokeTenantSchedule(ns, name)

		By("the tenant's own tick still fires, stamped with the tenant's UID")
		want := apiconst.RunName(name, ownTick)
		Eventually(func(g Gomega) {
			var b cbv1.Backup
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: want}, &b)).To(Succeed())
			g.Expect(b.Labels[apiconst.LabelOrigin]).To(Equal(apiconst.OriginNamespace))
			g.Expect(b.Annotations).To(HaveKeyWithValue(apiconst.AnnotationParentUID, string(created.UID)))
		}, eventuallyTimeout, eventuallyPoll).Should(Succeed())

		By("and lastRunName is the tenant's run, not the foreign one")
		Eventually(func(g Gomega) {
			s := getTenantScheduleG(g, ns, name)
			g.Expect(s.Status.LastRunName).To(Equal(want))
		}, eventuallyTimeout, eventuallyPoll).Should(Succeed())
	})

	// Re-firing the same tick against the schedule's OWN Backup stays the harmless no-op it always
	// was: the collision check must not turn idempotence into a fault.
	It("treats its own already-stamped Backup as an ordinary re-fire", func() {
		const (
			ns   = "bs-refire-ns"
			name = "hourly"
			loc  = "bs-refire-loc"
		)
		createTenantNamespace(ns)
		createTenantSchedule(newTenantSchedule(ns, name, "* * * * *", loc))
		created := getTenantScheduleNow(ns, name)

		tick := created.CreationTimestamp.UTC().Truncate(time.Minute).Add(time.Minute)
		want := apiconst.RunName(name, tick)
		scheduleClock.SetTime(tick.Add(30 * time.Second))
		pokeTenantSchedule(ns, name)
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: want}, &cbv1.Backup{})).To(Succeed())
		}, eventuallyTimeout, eventuallyPoll).Should(Succeed())

		By("poking it repeatedly at the same tick never produces a collision")
		for range 3 {
			pokeTenantSchedule(ns, name)
		}
		Consistently(func(g Gomega) {
			s := getTenantScheduleG(g, ns, name)
			g.Expect(s.Status.Phase).NotTo(Equal(reasonRunNameCollision))
		}, 3*time.Second, 500*time.Millisecond).Should(Succeed())
	})

	// The operator-UPGRADE path on this plane. Unlike the cluster plane, a schedule derives its
	// baseline from the Backups themselves, so an unstamped Backup a pre-stamp build fired is
	// simply absorbed as history: the tick it belongs to is no longer due, nothing re-stamps it,
	// and the schedule advances to the NEXT tick and stamps that one with its UID. The upgrade
	// therefore never produces a collision here — which is what this asserts, since the failure
	// mode being guarded against is a fleet of schedules going red on an operator upgrade.
	It("absorbs an unstamped Backup from a pre-upgrade build as ordinary history", func() {
		const (
			ns   = "bs-adopt-ns"
			name = "weekly"
			loc  = "bs-adopt-loc"
		)
		createTenantNamespace(ns)
		createTenantSchedule(newTenantSchedule(ns, name, "* * * * *", loc))
		created := getTenantScheduleNow(ns, name)

		tick := created.CreationTimestamp.UTC().Truncate(time.Minute).Add(time.Minute)
		legacy := apiconst.RunName(name, tick)
		b := &cbv1.Backup{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: ns,
				Name:      legacy,
				Labels: map[string]string{
					apiconst.LabelSchedule:  name,
					apiconst.LabelOrigin:    apiconst.OriginNamespace,
					apiconst.LabelNamespace: ns,
				},
			},
			Spec: cbv1.BackupSpec{
				LocationRef: cbv1.LocationReference{Kind: kindBackupLocation, Name: loc},
			},
		}
		Expect(k8sClient.Create(ctx, b)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(context.Background(), b) })
		// Finished, as a pre-upgrade run would be by the time the new binary reconciles: an
		// unstamped TERMINAL Backup is precisely what classifyCoordinate calls foreign, so this is
		// the shape the upgrade must not turn into a collision.
		Eventually(func(g Gomega) {
			var got cbv1.Backup
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: legacy}, &got)).To(Succeed())
			got.Status.Phase = string(status.BackupPhaseCompleted)
			g.Expect(k8sClient.Status().Update(ctx, &got)).To(Succeed())
		}, eventuallyTimeout, eventuallyPoll).Should(Succeed())

		By("the next tick after the pre-upgrade one fires normally, stamped with the schedule's UID")
		next := tick.Add(time.Minute)
		scheduleClock.SetTime(next.Add(30 * time.Second))
		pokeTenantSchedule(ns, name)

		wanted := apiconst.RunName(name, next)
		Eventually(func(g Gomega) {
			var got cbv1.Backup
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: wanted}, &got)).To(Succeed())
			g.Expect(got.Annotations).To(HaveKeyWithValue(apiconst.AnnotationParentUID, string(created.UID)))
		}, eventuallyTimeout, eventuallyPoll).Should(Succeed())

		By("and the upgrade never turns the schedule red")
		Consistently(func(g Gomega) {
			s := getTenantScheduleG(g, ns, name)
			g.Expect(s.Status.Phase).To(Equal("Active"))
		}, 2*time.Second, 500*time.Millisecond).Should(Succeed())

		By("and the pre-upgrade Backup is left exactly as it was — never re-stamped, never re-fired")
		var legacyAfter cbv1.Backup
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: legacy}, &legacyAfter)).To(Succeed())
		Expect(legacyAfter.Annotations).NotTo(HaveKey(apiconst.AnnotationParentUID))
	})

	It("does not stamp a Backup at a coordinate a foreign object holds", func() {
		const (
			ns   = "bs-nostamp-ns"
			name = "daily"
			loc  = "bs-nostamp-loc"
		)
		createTenantNamespace(ns)
		createTenantSchedule(newTenantSchedule(ns, name, "* * * * *", loc))
		created := getTenantScheduleNow(ns, name)

		tick := created.CreationTimestamp.UTC().Truncate(time.Minute).Add(time.Minute)
		taken := apiconst.RunName(name, tick)
		seedClusterPlaneBackup(ns, taken, name, loc)

		scheduleClock.SetTime(tick.Add(30 * time.Second))
		pokeTenantSchedule(ns, name)

		By("no namespace-plane Backup ever appears for this schedule")
		Consistently(func(g Gomega) {
			var list cbv1.BackupList
			g.Expect(k8sClient.List(ctx, &list, client.InNamespace(ns), client.MatchingLabels{
				apiconst.LabelSchedule: name,
				apiconst.LabelOrigin:   apiconst.OriginNamespace,
			})).To(Succeed())
			g.Expect(list.Items).To(BeEmpty())
		}, 3*time.Second, 500*time.Millisecond).Should(Succeed())

		By("and the foreign object is still exactly one object, unmodified in kind")
		var occupant cbv1.Backup
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: taken}, &occupant)).To(Succeed())
		Expect(occupant.Labels[apiconst.LabelOrigin]).To(Equal(apiconst.OriginCluster))
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx,
			client.ObjectKey{Namespace: ns, Name: taken + "-x"}, &cbv1.Backup{}))).To(BeTrue())
	})
})
