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
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
)

// M4 — repository maintenance and verification on real infrastructure (R17,
// spec/08-testing-and-dod.md §5 cases 11, 11b, 12).
//
// Everything below runs against a real S3 endpoint and a real restic binary, which is the
// only place these behaviours mean anything: an exclusive lock is an object-store fact, and
// `check --read-data-subset` only tells the truth when the bytes it re-reads came off a
// bucket rather than a fake.
var _ = Describe("Milestone M4 — repository maintenance & verification", Label("m4"), Ordered, func() {
	BeforeAll(func() {
		m1SkipIfNoS3()
		m1EnsurePlatformSecrets()
	})

	// ------------------------------------------------------------------
	// Case 11 — runs on the SHARED "dr" repository, on purpose.
	//
	// "One shared cluster repository means ONE cluster-wide prune window"
	// (adr/0009) is the property under test, and a dedicated repository
	// would test a weaker statement than production makes. It is safe to
	// run here because a prune is non-destructive to snapshots: it reclaims
	// unreferenced data, and the check afterwards proves it.
	// ------------------------------------------------------------------
	Describe("a prune and a live backup on the shared repository", func() {
		const seedNS = "m4-prune-race"

		It("holds the prune until the backup's movers have drained, then runs it exclusively", func() {
			repo := m1WaitRepositoryInitialized(m1LocationName)
			startedAt := time.Now()

			By("Given a namespace with data to back up")
			ensureNamespace(seedNS)
			DeferCleanup(func() { deleteNamespaceAndWaitGone(seedNS, 5*time.Minute) })
			m2SeedVolume(seedNS, "m4-race-data", m4StorageClass, "1Gi")

			By("When a backup is running and a prune becomes due on the same repository")
			run := "m4-race-" + m4RunID
			m1RunClusterBackup(run, m1LocationName, cbv1.NamespaceSelector{MatchNames: []string{seedNS}})
			DeferCleanup(func() {
				_ = k8s.Delete(ctx, &cbv1.ClusterBackup{ObjectMeta: metav1.ObjectMeta{Name: run}})
			})

			// The schedule is added while the run is in flight and REMOVED in cleanup: the shared
			// location belongs to every other spec, and leaving a once-a-minute prune on it would
			// have this spec quietly reshape the rest of the suite.
			m4SetSharedPruneSchedule(m4EverySecondCron)
			DeferCleanup(func() { m4SetSharedPruneSchedule("") })

			By("Then no prune Job starts while a data mover is still live")
			// This is the drain, and it is the reason prune sets blocksMovers at all: without it a
			// prune and the mover fleet would sit on --retry-lock staring at each other, and on the
			// shared repository that is every namespace's backup (adr/0015 §3).
			// Wait for at least one mover to be live first. Without this the Consistently below can
			// spend its whole window before any mover pod reaches Running, and "no prune started
			// while movers ran" would be asserted against a period in which nothing ran at all.
			Eventually(func() int { return len(m4LiveDataMovers(run)) },
				10*time.Minute, 2*time.Second).Should(BeNumerically(">", 0),
				"no data mover ever started, so the drain assertion would prove nothing")
			Consistently(func(g Gomega) {
				if len(m4LiveDataMovers(run)) == 0 {
					return // the backup finished; the drain no longer has anything to prove
				}
				for _, job := range m4MaintenanceJobs() {
					g.Expect(job.Name).NotTo(ContainSubstring("prune"),
						"a prune started while data movers were still running on the same repository")
				}
			}, 90*time.Second, 3*time.Second).Should(Succeed())

			By("And the backup reaches a terminal phase")
			cb := m1WaitClusterBackupTerminal(run, 15*time.Minute)
			// PartiallyFailed, not "PartiallyCompleted": the latter is a Backup phase and does not
			// exist on ClusterBackup (clusterbackup_types.go enumerates
			// Pending;Running;Completed;PartiallyFailed;Failed), so asserting it would have made the
			// tolerance unreachable and the realistic mixed outcome a failure.
			Expect(cb.Status.Phase).To(BeElementOf("Completed", "PartiallyFailed"),
				"the backup did not survive sharing its repository with a prune")

			By("And the prune then runs and succeeds")
			rec := m4WaitMaintenanceRecord(repo.Name, "prune", startedAt, 20*time.Minute)
			Expect(rec.Result).To(Equal(cbv1.MaintenanceSucceeded), "prune failed: %s", rec.Message)

			By("And the repository is structurally intact afterwards")
			// The ground truth is restic's own verdict, not our status: a controller bug that both
			// pruned wrongly and reported success would otherwise pass.
			out := m1ResticExec(m1LocationName, "check")
			Expect(out).To(ContainSubstring("no errors were found"),
				"restic check is unhappy after a prune that raced a backup:\n%s", out)
		})
	})

	// ------------------------------------------------------------------
	// Cases 11b and 12 — each on its OWN repository.
	//
	// One kills a prune and orphans an exclusive lock; the other writes
	// garbage into a pack. On the shared repository either would fail every
	// later spec, including M1's, turning one real finding into a cascade
	// nobody can read. The behaviour under test is identical on an isolated
	// repository: same lane, same lock, same pack format.
	// ------------------------------------------------------------------
	Describe("a prune killed mid-flight", func() {
		const seedNS = "m4-killed-prune"

		It("records the failure and lets the next scheduled prune succeed anyway", func() {
			locName := "m4-killed-" + m4RunID
			repo := m4CreateIsolatedLocation(locName, m4DedicatedClusterID("killed"),
				&cbv1.MaintenanceSpec{PruneSchedule: m4EverySecondCron})

			By("Given an isolated repository holding one backup")
			ensureNamespace(seedNS)
			DeferCleanup(func() { deleteNamespaceAndWaitGone(seedNS, 5*time.Minute) })
			m2SeedVolume(seedNS, "m4-killed-data", m4StorageClass, "1Gi")
			run := "m4-killed-run-" + m4RunID
			m1RunClusterBackup(run, locName, cbv1.NamespaceSelector{MatchNames: []string{seedNS}})
			DeferCleanup(func() {
				_ = k8s.Delete(ctx, &cbv1.ClusterBackup{ObjectMeta: metav1.ObjectMeta{Name: run}})
			})
			m1WaitClusterBackupTerminal(run, 15*time.Minute)

			By("When its prune is killed mid-flight, orphaning restic's exclusive lock")
			killedAt := time.Now()
			killed := m4KillNextPrunePod(20 * time.Minute)
			Expect(killed).To(BeTrue(), "no prune pod appeared to kill within the window")

			By("Then the failure is recorded rather than swallowed")
			// A prune that dies must not look like one that never ran: recentMaintenance records
			// ATTEMPTS precisely so a repository failing every night is distinguishable from one
			// with no schedule, and so the cron baseline does not treat it as permanently due.
			rec := m4WaitMaintenanceRecord(repo.Name, "prune", killedAt, 15*time.Minute)
			Expect(rec.Result).To(Equal(cbv1.MaintenanceFailed),
				"a prune whose pod was killed was recorded as %q", rec.Result)

			By("And a read of the repository never blocks on the orphaned lock")
			// Discovery and the restore's source resolution both pass --no-lock. Without it this
			// call would sit behind the dead lock for restic's full staleness horizon, and "the
			// restore hangs, sometimes, at night" is not a diagnosable report.
			done := make(chan string, 1)
			go func() {
				defer GinkgoRecover()
				done <- m1ResticExecOn(m4DedicatedClusterID("killed"), locName, "snapshots", "--json", "--no-lock")
			}()
			var readOut string
			Eventually(done, 6*time.Minute, 5*time.Second).Should(Receive(&readOut),
				"a --no-lock read blocked behind the orphaned exclusive lock")
			// m1ResticExecOn returns the Job log without asserting the Job succeeded, so a read that
			// died instantly (wrong repository, wrong password) would satisfy the Receive above while
			// proving nothing. Requiring a snapshot array makes the read's SUCCESS the assertion.
			Expect(readOut).To(ContainSubstring("["), "the --no-lock read returned no snapshot list:\n%s", readOut)

			By("And a later prune completes despite the lock left behind")
			// Deliberately agnostic about WHO cleared the lock. Two mechanisms can: restic treats a
			// lock older than 30 minutes as stale and removes it on the next acquisition, and the
			// operator's own reap fires once the probe (15 min) has seen it cross the same 30-minute
			// threshold. Both land inside this window, and pinning the assertion on one of them would
			// make the spec fail when the other got there first. The user-visible property is the
			// same either way: the repository is not wedged.
			Eventually(func(g Gomega) {
				var live cbv1.BackupRepository
				g.Expect(k8s.Get(ctx, client.ObjectKey{Name: repo.Name}, &live)).To(Succeed())
				var succeeded bool
				for _, r := range live.Status.RecentMaintenance {
					if r.Operation == "prune" && r.Result == cbv1.MaintenanceSucceeded {
						succeeded = true
					}
				}
				g.Expect(succeeded).To(BeTrue(), "no prune has succeeded since the kill")
			}, 45*time.Minute, 30*time.Second).Should(Succeed())
		})
	})

	Describe("a silently corrupted pack", func() {
		const seedNS = "m4-corrupt"

		It("is caught by check --read-data-subset and surfaced on the repository", func() {
			locName := "m4-corrupt-" + m4RunID
			clusterID := m4DedicatedClusterID("corrupt")
			// A structural check would pass: it only verifies that the index and the objects agree
			// about what exists. Reading a sample of the pack data is the only thing that catches a
			// bucket handing back the right length and the wrong bytes.
			repo := m4CreateIsolatedLocation(locName, clusterID, &cbv1.MaintenanceSpec{
				CheckSchedule:       m4EverySecondCron,
				CheckReadDataSubset: "100%",
			})

			By("Given an isolated repository holding real pack data")
			ensureNamespace(seedNS)
			DeferCleanup(func() { deleteNamespaceAndWaitGone(seedNS, 5*time.Minute) })
			m2SeedVolume(seedNS, "m4-corrupt-data", m4StorageClass, "1Gi")
			run := "m4-corrupt-run-" + m4RunID
			m1RunClusterBackup(run, locName, cbv1.NamespaceSelector{MatchNames: []string{seedNS}})
			DeferCleanup(func() {
				_ = k8s.Delete(ctx, &cbv1.ClusterBackup{ObjectMeta: metav1.ObjectMeta{Name: run}})
			})
			m1WaitClusterBackupTerminal(run, 15*time.Minute)
			seededAt := time.Now()

			By("And a check that passes before any tampering")
			// Asserting the clean state FIRST is what makes the failure below mean something: without
			// it, a check that fails for an unrelated reason would read as a caught corruption.
			clean := m4WaitMaintenanceRecord(repo.Name, "check", seededAt, 20*time.Minute)
			Expect(clean.Result).To(Equal(cbv1.MaintenanceSucceeded),
				"the repository was already unhealthy before the test corrupted anything: %s", clean.Message)

			By("When bytes inside one pack object are replaced, keeping its name and length")
			key := m4CorruptOnePack(clusterID)

			By("Then the next check fails and says so on the repository")
			Eventually(func(g Gomega) {
				var live cbv1.BackupRepository
				g.Expect(k8s.Get(ctx, client.ObjectKey{Name: repo.Name}, &live)).To(Succeed())
				g.Expect(live.Status.LastCheckResult).To(Equal("Failed"),
					"pack %s was corrupted and check still reports %q", key, live.Status.LastCheckResult)
			}, 30*time.Minute, 15*time.Second).Should(Succeed())

			By("And the RepositoryCheckFailed condition is raised for the alert to fire on")
			var live cbv1.BackupRepository
			Expect(k8s.Get(ctx, client.ObjectKey{Name: repo.Name}, &live)).To(Succeed())
			var found bool
			for _, c := range live.Status.Conditions {
				if c.Type == "RepositoryCheckFailed" && c.Status == metav1.ConditionTrue {
					found = true
				}
			}
			Expect(found).To(BeTrue(),
				"lastCheckResult is Failed but no RepositoryCheckFailed condition — the alert would never fire")
		})
	})
})

// m4SetSharedPruneSchedule patches the SHARED dr location's prune schedule ("" clears it). Kept
// separate so both the set and the restore go through one place: a schedule left behind on the
// shared location would silently change every other spec's repository.
func m4SetSharedPruneSchedule(cron string) {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		var loc cbv1.ClusterBackupLocation
		g.Expect(k8s.Get(ctx, client.ObjectKey{Name: m1LocationName}, &loc)).To(Succeed())
		switch {
		case cron == "" && loc.Spec.Maintenance == nil:
			return
		case cron == "":
			loc.Spec.Maintenance.PruneSchedule = ""
		default:
			if loc.Spec.Maintenance == nil {
				loc.Spec.Maintenance = &cbv1.MaintenanceSpec{}
			}
			loc.Spec.Maintenance.PruneSchedule = cron
		}
		g.Expect(k8s.Update(ctx, &loc)).To(Succeed())
	}, time.Minute, 3*time.Second).Should(Succeed())
}

// m4LiveDataMovers returns the run's still-running DATA mover pods. Scoped by the run label so it
// never observes — or is confused by — another spec's movers, and filtered on the per-PVC label so
// a maintenance Job is never miscounted as a data mover.
func m4LiveDataMovers(run string) []corev1.Pod {
	GinkgoHelper()
	var pods corev1.PodList
	Expect(k8s.List(ctx, &pods, client.InNamespace(operatorNS),
		client.MatchingLabels{apiconst.LabelClusterBackup: run})).To(Succeed())
	var live []corev1.Pod
	for i := range pods.Items {
		p := pods.Items[i]
		if p.Labels[apiconst.LabelPVC] != "" && p.Status.Phase == corev1.PodRunning {
			live = append(live, p)
		}
	}
	return live
}

// m4KillNextPrunePod waits for a prune pod to start and deletes it with grace 0, so restic dies
// without releasing its exclusive lock. Reports whether one was caught.
func m4KillNextPrunePod(timeout time.Duration) bool {
	GinkgoHelper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var pods corev1.PodList
		if err := k8s.List(ctx, &pods, client.InNamespace(operatorNS),
			client.MatchingLabels{"app.kubernetes.io/component": "maintenance"}); err == nil {
			for i := range pods.Items {
				p := pods.Items[i]
				if p.Status.Phase != corev1.PodRunning || !strings.Contains(p.Name, "prune") {
					continue
				}
				// SIGKILL, not a graceful stop: a prune given time to clean up would release its
				// lock, and the whole scenario is about the lock that outlives its holder.
				Expect(k8s.Delete(ctx, &p, client.GracePeriodSeconds(0))).To(Succeed(),
					"delete prune pod %s", p.Name)
				return true
			}
		}
		time.Sleep(2 * time.Second)
	}
	return false
}
