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
	"fmt"
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
// ClusterBackupSchedule-suite helpers.
// ---------------------------------------------------------------------------

// newSchedule builds a schedule whose template points at location (which the tests deliberately
// leave absent unless a scenario needs it — a stamped run then blocks on LocationNotFound and stays
// non-terminal, which is exactly what the concurrency scenario relies on).
func newSchedule(name, cronExpr, location string) *cbv1.ClusterBackupSchedule {
	return &cbv1.ClusterBackupSchedule{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: cbv1.ClusterBackupScheduleSpec{
			Schedule: cronExpr,
			Template: cbv1.ClusterBackupTemplate{
				Spec: cbv1.ClusterBackupRunSpec{
					LocationRef: cbv1.LocalObjectReference{Name: location},
					Namespaces:  cbv1.NamespaceSelector{MatchLabels: map[string]string{"cbs-never": "match"}},
				},
			},
		},
	}
}

// createSchedule creates s and registers cleanup of it and its label-linked run records (which are
// never owned in a way envtest — with no GC controller — would collect).
func createSchedule(s *cbv1.ClusterBackupSchedule) {
	GinkgoHelper()
	Expect(k8sClient.Create(ctx, s)).To(Succeed())
	DeferCleanup(func() {
		_ = k8sClient.Delete(context.Background(), s)
		_ = k8sClient.DeleteAllOf(context.Background(), &cbv1.ClusterBackup{},
			client.MatchingLabels{apiconst.LabelSchedule: s.Name})
	})
}

// getScheduleNow fetches a schedule with a hard assertion (used right after create to read the
// apiserver-assigned creationTimestamp the fake clock is anchored to).
func getScheduleNow(name string) cbv1.ClusterBackupSchedule {
	GinkgoHelper()
	var s cbv1.ClusterBackupSchedule
	Expect(k8sClient.Get(ctx, client.ObjectKey{Name: name}, &s)).To(Succeed())
	return s
}

// getScheduleG fetches a schedule inside an Eventually block.
func getScheduleG(g Gomega, name string) cbv1.ClusterBackupSchedule {
	var s cbv1.ClusterBackupSchedule
	g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: name}, &s)).To(Succeed())
	return s
}

// listScheduleRuns returns the run records the schedule owns (by label).
func listScheduleRuns(name string) []cbv1.ClusterBackup {
	GinkgoHelper()
	var runs cbv1.ClusterBackupList
	Expect(k8sClient.List(ctx, &runs, client.MatchingLabels{apiconst.LabelSchedule: name})).To(Succeed())
	return runs.Items
}

// pokeSchedule forces a reconcile after the fake clock is advanced by bumping a throwaway
// annotation (envtest's requeue runs on real time, so the schedule must be nudged to re-evaluate at
// the new fake "now").
func pokeSchedule(name string) {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		var s cbv1.ClusterBackupSchedule
		g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: name}, &s)).To(Succeed())
		if s.Annotations == nil {
			s.Annotations = map[string]string{}
		}
		s.Annotations["test.crystalbackup.io/poke"] = fmt.Sprintf("%d", time.Now().UnixNano())
		g.Expect(k8sClient.Update(ctx, &s)).To(Succeed())
	}, eventuallyTimeout, eventuallyPoll).Should(Succeed())
}

// createTerminalScheduleRun creates a Completed run record linked to scheduleName (no ownerReference
// needed — history GC selects by label and orders by creation time). It patches the run terminal so
// the live ClusterBackup reconciler freezes it (isTerminalClusterBackupPhase) and GC classifies it
// as successful.
func createTerminalScheduleRun(scheduleName string, tick time.Time) *cbv1.ClusterBackup {
	GinkgoHelper()
	run := &cbv1.ClusterBackup{
		ObjectMeta: metav1.ObjectMeta{
			Name:   apiconst.RunName(scheduleName, tick),
			Labels: map[string]string{apiconst.LabelSchedule: scheduleName},
		},
		Spec: cbv1.ClusterBackupSpec{
			ScheduleRef: scheduleName,
			ClusterBackupRunSpec: cbv1.ClusterBackupRunSpec{
				LocationRef: cbv1.LocalObjectReference{Name: "cbs-gc-missing-location"},
				Namespaces:  cbv1.NamespaceSelector{MatchLabels: map[string]string{"cbs-never": "match"}},
			},
		},
	}
	Expect(k8sClient.Create(ctx, run)).To(Succeed())
	DeferCleanup(func() { _ = k8sClient.Delete(context.Background(), run) })
	Eventually(func(g Gomega) {
		var r cbv1.ClusterBackup
		g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: run.Name}, &r)).To(Succeed())
		r.Status.Phase = string(status.ClusterBackupPhaseCompleted)
		now := metav1.Now()
		r.Status.CompletionTime = &now
		g.Expect(k8sClient.Status().Update(ctx, &r)).To(Succeed())
	}, initTimeout, initPoll).Should(Succeed())
	return run
}

var _ = Describe("ClusterBackupScheduleReconciler", func() {
	// Re-anchor the fake clock to real-time-ish before each spec (it is shared process-wide), so a
	// freshly created schedule's creationTimestamp ≈ now and it does not appear overdue on apply.
	BeforeEach(func() {
		scheduleClock.SetTime(time.Now())
	})

	It("does not stamp a run on apply, and reports a future nextScheduleTime", func() {
		const name = "cbs-apply"
		createSchedule(newSchedule(name, "* * * * *", "cbs-loc-apply"))

		By("no run is stamped immediately after creation")
		Consistently(func(g Gomega) {
			g.Expect(listScheduleRuns(name)).To(BeEmpty())
		}, 3*time.Second, 500*time.Millisecond).Should(Succeed())

		By("the schedule becomes Active with a next scheduled time")
		Eventually(func(g Gomega) {
			s := getScheduleG(g, name)
			g.Expect(s.Status.Phase).To(Equal("Active"))
			g.Expect(s.Status.NextScheduleTime).NotTo(BeNil())
		}, eventuallyTimeout, eventuallyPoll).Should(Succeed())
	})

	It("stamps a deterministically-named, owned, label-linked run at the due tick", func() {
		const name = "cbs-fire"
		const loc = "cbs-loc-fire"
		createSchedule(newSchedule(name, "* * * * *", loc))
		created := getScheduleNow(name)

		tick := created.CreationTimestamp.Time.UTC().Truncate(time.Minute).Add(time.Minute)
		scheduleClock.SetTime(tick.Add(30 * time.Second))
		pokeSchedule(name)

		want := apiconst.RunName(name, tick)
		By("a ClusterBackup with the run name appears, carrying the template + schedule link + owner ref")
		Eventually(func(g Gomega) {
			var run cbv1.ClusterBackup
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: want}, &run)).To(Succeed())
			g.Expect(run.Spec.ScheduleRef).To(Equal(name))
			g.Expect(run.Labels).To(HaveKeyWithValue(apiconst.LabelSchedule, name))
			g.Expect(run.Spec.LocationRef.Name).To(Equal(loc))
			owner := metav1.GetControllerOf(&run)
			g.Expect(owner).NotTo(BeNil())
			g.Expect(owner.Kind).To(Equal("ClusterBackupSchedule"))
			g.Expect(owner.Name).To(Equal(name))
		}, eventuallyTimeout, eventuallyPoll).Should(Succeed())

		By("status.lastRunName records it")
		Eventually(func(g Gomega) {
			g.Expect(getScheduleG(g, name).Status.LastRunName).To(Equal(want))
		}, eventuallyTimeout, eventuallyPoll).Should(Succeed())
	})

	It("collapses several missed ticks into a single run (catch-up bounded to one)", func() {
		const name = "cbs-catchup"
		createSchedule(newSchedule(name, "* * * * *", "cbs-loc-catchup"))
		created := getScheduleNow(name)

		base := created.CreationTimestamp.Time.UTC().Truncate(time.Minute)
		scheduleClock.SetTime(base.Add(5*time.Minute + 30*time.Second))
		pokeSchedule(name)

		By("exactly one run exists — the latest missed tick, not one per minute")
		Eventually(func(g Gomega) {
			g.Expect(listScheduleRuns(name)).To(HaveLen(1))
		}, eventuallyTimeout, eventuallyPoll).Should(Succeed())
		Consistently(func(g Gomega) {
			g.Expect(listScheduleRuns(name)).To(HaveLen(1))
		}, 2*time.Second, 500*time.Millisecond).Should(Succeed())

		latest := apiconst.RunName(name, base.Add(5*time.Minute))
		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: latest}, &cbv1.ClusterBackup{})).To(Succeed())
	})

	It("suppresses firing while paused", func() {
		const name = "cbs-paused"
		s := newSchedule(name, "* * * * *", "cbs-loc-paused")
		s.Spec.Paused = true
		createSchedule(s)
		created := getScheduleNow(name)

		scheduleClock.SetTime(created.CreationTimestamp.Time.UTC().Add(5 * time.Minute))
		pokeSchedule(name)

		By("no run is stamped and the schedule reports Paused")
		Consistently(func(g Gomega) {
			g.Expect(listScheduleRuns(name)).To(BeEmpty())
		}, 3*time.Second, 500*time.Millisecond).Should(Succeed())
		Eventually(func(g Gomega) {
			g.Expect(getScheduleG(g, name).Status.Phase).To(Equal("Paused"))
		}, eventuallyTimeout, eventuallyPoll).Should(Succeed())
	})

	It("does not stamp an overlapping run while the previous one is still active", func() {
		const name = "cbs-forbid"
		// Location absent ⇒ the stamped run blocks on LocationNotFound and stays non-terminal (active).
		createSchedule(newSchedule(name, "* * * * *", "cbs-loc-forbid-absent"))
		created := getScheduleNow(name)
		base := created.CreationTimestamp.Time.UTC().Truncate(time.Minute)

		By("firing the first run")
		scheduleClock.SetTime(base.Add(time.Minute + 30*time.Second))
		pokeSchedule(name)
		Eventually(func(g Gomega) {
			g.Expect(listScheduleRuns(name)).To(HaveLen(1))
		}, eventuallyTimeout, eventuallyPoll).Should(Succeed())

		By("advancing to the next tick while the first run is still active — no second run is stamped")
		scheduleClock.SetTime(base.Add(2*time.Minute + 30*time.Second))
		pokeSchedule(name)
		Consistently(func(g Gomega) {
			g.Expect(listScheduleRuns(name)).To(HaveLen(1))
		}, 3*time.Second, 500*time.Millisecond).Should(Succeed())
	})

	It("garbage-collects run records past the history limit while their child backups survive", func() {
		const name = "cbs-gc"
		s := newSchedule(name, "0 0 1 1 *", "cbs-loc-gc") // Jan 1 — never fires during the test
		s.Spec.SuccessfulRunsHistoryLimit = 1
		createSchedule(s)
		created := getScheduleNow(name)
		base := created.CreationTimestamp.Time.UTC()

		// Three Completed run records, created oldest→newest (GC orders by creation time).
		oldest := createTerminalScheduleRun(name, base.Add(-72*time.Hour))
		_ = createTerminalScheduleRun(name, base.Add(-48*time.Hour))
		newest := createTerminalScheduleRun(name, base.Add(-24*time.Hour))

		// A child Backup projected from the OLDEST run — it must outlive that run's GC (adr/0009).
		createTenantNamespace("cbs-gc-ns")
		child := &cbv1.Backup{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "cbs-gc-ns",
				Name:      oldest.Name,
				Labels: map[string]string{
					apiconst.LabelClusterBackup: oldest.Name,
					apiconst.LabelOrigin:        apiconst.OriginCluster,
				},
			},
			Spec: cbv1.BackupSpec{LocationRef: cbv1.LocationReference{Kind: "ClusterBackupLocation", Name: "cbs-loc-gc"}},
		}
		Expect(k8sClient.Create(ctx, child)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(context.Background(), child) })

		pokeSchedule(name)

		By("only the newest run record survives (limit 1)")
		Eventually(func(g Gomega) {
			g.Expect(listScheduleRuns(name)).To(HaveLen(1))
		}, eventuallyTimeout, eventuallyPoll).Should(Succeed())
		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: newest.Name}, &cbv1.ClusterBackup{})).To(Succeed())
		Eventually(func(g Gomega) {
			err := k8sClient.Get(ctx, client.ObjectKey{Name: oldest.Name}, &cbv1.ClusterBackup{})
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
		}, eventuallyTimeout, eventuallyPoll).Should(Succeed())

		By("the child backup of the GC'd oldest run survives")
		Consistently(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: "cbs-gc-ns", Name: oldest.Name}, &cbv1.Backup{})).To(Succeed())
		}, 2*time.Second, 500*time.Millisecond).Should(Succeed())
	})

	It("sets the RetentionIgnored advisory for retention against an Immutable location", func() {
		const name = "cbs-retention"
		const loc = "cbs-loc-immutable"
		l := newTestLocation(loc, "kek-ret", "s3-ret", false)
		l.Spec.Mode = cbv1.LocationModeImmutable
		createTestLocation(l)

		s := newSchedule(name, "0 0 * * *", loc)
		s.Spec.Template.Spec.Retention = cbv1.RetentionSpec{KeepDaily: 7}
		createSchedule(s)
		pokeSchedule(name)

		By("the RetentionIgnored condition is set True")
		Eventually(func(g Gomega) {
			c := status.FindCondition(getScheduleG(g, name).Status.Conditions, ConditionRetentionIgnored)
			g.Expect(c).NotTo(BeNil())
			g.Expect(c.Status).To(Equal(metav1.ConditionTrue))
		}, eventuallyTimeout, eventuallyPoll).Should(Succeed())
	})

	It("reports InvalidSchedule for an unparseable cron expression", func() {
		const name = "cbs-invalid"
		createSchedule(newSchedule(name, "definitely not a cron", "cbs-loc-invalid"))

		Eventually(func(g Gomega) {
			s := getScheduleG(g, name)
			g.Expect(s.Status.Phase).To(Equal("InvalidSchedule"))
			c := status.FindCondition(s.Status.Conditions, ConditionReady)
			g.Expect(c).NotTo(BeNil())
			g.Expect(c.Reason).To(Equal("InvalidSchedule"))
		}, eventuallyTimeout, eventuallyPoll).Should(Succeed())
	})
})
