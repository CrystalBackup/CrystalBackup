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
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/metrics"
)

// M6 lot I — the alert rules, watched firing.
//
// The other half of this lot is internal/alerts/testdata: promtool unit tests that feed every rule
// synthetic series and assert both directions, in three seconds, with no cluster. Those prove the
// EXPRESSIONS are right, which is the defect class that started M6 — five of the nine shipped rules
// read series the operator never emitted, valid PromQL that could never match anything.
//
// They cannot prove the CHAIN. Between a correct expression and a page there is: a collector that
// actually emits the series, a ServiceMonitor the Prometheus Operator selects, a scrape that gets
// past the metrics endpoint's authn/authz and the chart's own NetworkPolicy, and a PrometheusRule
// that a real Prometheus loads and evaluates. Every one of those links has failed silently in some
// project, and none of them is visible to a rule-file test. So a handful of conditions are provoked
// for real here and the alert is waited for through Prometheus's API.
//
// WHY ONLY THREE PROVOCATIONS. The holds are the budget. BackupFailed has none (increase() over an
// hour is its own hold), RepositoryCheckFailed holds 5m and PVCSnapshotPileup 30m — under an hour
// wall-clock together, because the pile-up's clock is started first and runs while the repository
// work happens. The rest are out of reach at any sane cost and are named in the report rather than
// faked: MaintenanceStalled needs 26h, SchedulePausedTooLong 7 days, BackupMissed at least 76
// minutes (its deadline floor is period×1.1 + 1h, so even a once-a-minute cron cannot be missed
// sooner), ExternalSyncStale and ErasureBlocked hold an hour each. Shortening them would mean
// shipping one rule text and testing another, which is worse than not testing them here at all —
// they are covered exhaustively in the promtool layer, where time is free.
//
// NOTHING IN THIS FILE SKIPS. If Prometheus is unreachable, if the ServiceMonitor was not selected,
// if the scrape is denied — the spec FAILS. Three self-disabling guards were deleted from this
// suite in the two milestones before this one, each of which had been reporting success by not
// running for months.
var _ = Describe("Milestone M6 — alert rules fire on real conditions", Ordered, Label("m6", "alerts"), func() {
	var (
		runID     = strconv.FormatInt(time.Now().Unix(), 36)
		pileupNS  = "m6-alerts-pileup"
		failNS    = "m6-alerts-failed"
		corruptNS = "m6-alerts-corrupt"
	)

	BeforeAll(func() {
		m1RequireS3()
		m1EnsurePlatformSecrets()
		// FIRST, before anything is provoked. An alert spec that finds out the scrape is broken
		// after twenty minutes of corrupting a repository has spent twenty minutes producing a
		// confusing failure; here the message names the broken link directly.
		m6RequirePrometheus()
	})

	// ------------------------------------------------------------------
	// FIRST, and before anything is provoked.
	//
	// This is here because of what the lane's own first real run found. The pile-up spec failed on
	// "0 series", and the metric was being emitted and Prometheus did know it — but the query
	// filtered on `namespace="m6-alerts-pileup"` and that label did not hold what everyone assumed.
	//
	// A ServiceMonitor attaches the TARGET's labels (namespace, pod, service, container, job), and
	// with honorLabels false — the default, and what the chart shipped — the target wins every
	// collision. Every crystalbackup_ series arrived carrying namespace="crystal-backup-system",
	// for every tenant on the cluster; the real value survived only as `exported_namespace`, which
	// nothing queries and nothing alerts on.
	//
	// Nothing looked broken, and that is the whole difficulty. The alert expressions still joined,
	// because both sides of `on (namespace, …)` carried the same wrong value — so BackupMissed
	// fired at the right moment and then named the operator's namespace, routing every tenant's
	// page to one place. Every series this project emits is specified to be tenant-attributable
	// (R19, spec/05-observability.md §1); at the scrape, none of them were.
	//
	// It runs before any provocation because it invalidates every measurement downstream: a lane
	// that discovers this after twenty minutes of corrupting a repository has spent twenty minutes
	// earning a confusing failure.
	// ------------------------------------------------------------------
	It("emits every series under the tenant's own labels, not the scraper's", func() {
		By("Given the crystalbackup_ series Prometheus holds right now")
		series, err := m6AllCrystalbackupSeries()
		Expect(err).NotTo(HaveOccurred())
		// Not a vacuous pass. Zero series would satisfy both assertions below while measuring
		// nothing, which is the shape of guard this suite keeps deleting.
		Expect(series).NotTo(BeEmpty(),
			"Prometheus holds no crystalbackup_ series at all — the scrape is not landing, and "+
				"every assertion in this lane would pass by having nothing to check")

		By("Then not one of them carries an `exported_` label")
		// The universal signature. `exported_<x>` is what Prometheus renames OUR label to when the
		// target's own label of that name wins the collision, so its presence means a series is
		// wearing the scraper's identity instead of its own. Stated generally rather than as
		// `exported_namespace`: the same collision is waiting for anything we ever add called
		// `pod`, `service`, `container` or `job`, and it would arrive exactly as silently.
		var shadowed []string
		for _, s := range series {
			for label := range s.Metric {
				if strings.HasPrefix(label, "exported_") {
					shadowed = append(shadowed, fmt.Sprintf("%s has %s (its own %s was overwritten by the target's)",
						s.Metric["__name__"], label, strings.TrimPrefix(label, "exported_")))
				}
			}
		}
		Expect(shadowed).To(BeEmpty(),
			"a scrape label is masking one of ours — set honorLabels on the chart's ServiceMonitor.\n"+
				"Every alert would still fire, still join, and still name the wrong object:\n  %s",
			strings.Join(m6Unique(shadowed), "\n  "))

		By("And no tenant-scoped series wears the operator's own namespace")
		// The complement, and it is not redundant. The `exported_` invariant catches the collision
		// being RESOLVED against us; it would not catch someone one day "fixing" it by relabelling
		// our namespace out of the way instead of honouring it, which leaves no exported_ behind
		// and the same wrong value sitting in `namespace`.
		//
		// TENANT-SCOPED, and the qualifier is load-bearing — the naive version of this check fails
		// on a perfectly healthy cluster. A ServiceMonitor attaches the target's `namespace` to any
		// series that does not already declare one, so crystalbackup_build_info,
		// crystalbackup_clusterbackup_* and crystalbackup_mover_* legitimately carry
		// "crystal-backup-system": they have no namespace dimension of their own and nothing was
		// overwritten. (The cluster-scoped repository and discovery families are a third case again:
		// they DO declare `namespace`, expose it empty, and Prometheus drops the empty label — so
		// they end up with no namespace at all rather than the target's.)
		//
		// metrics.Catalogue() decides which is which, rather than a list maintained here. It is the
		// same []string the collectors hand to prometheus.NewDesc and the same source the alert
		// rules and the dashboard checker read, so a family that gains or loses its namespace label
		// moves this assertion with it instead of drifting away from it.
		tenantScoped := map[string]bool{}
		for family, labels := range metrics.Catalogue() {
			if slices.Contains(labels, "namespace") {
				tenantScoped[family] = true
			}
		}
		var stolen []string
		for _, s := range series {
			if s.Metric["namespace"] != operatorNS {
				continue
			}
			if tenantScoped[m6FamilyOf(s.Metric["__name__"])] {
				stolen = append(stolen, s.Metric["__name__"])
			}
		}
		Expect(stolen).To(BeEmpty(),
			"these families declare a `namespace` label of their own, so the only way they can report "+
				"the scraper's namespace %q is by having had it overwritten: %v",
			operatorNS, m6Unique(stolen))
	})

	It("loads every rule the chart ships, and evaluates all of them without error", func() {
		By("Given the chart installed with metrics.rules.enabled")
		loaded, err := m6LoadedRules()
		Expect(err).NotTo(HaveOccurred())

		By("Then Prometheus has selected the PrometheusRule and knows every alert in it")
		for _, name := range m6ShippedAlertNames {
			rule, ok := loaded[name]
			Expect(ok).To(BeTrue(),
				"Prometheus has not loaded %s. A PrometheusRule the operator does not select is "+
					"installed, valid and completely inert — which is indistinguishable from a working "+
					"one until the day it should have paged. Loaded: %v", name, m6RuleNames(loaded))

			// health/lastError is the thing promtool cannot tell us. A unit test runs the expression
			// against a handful of hand-written series; here it meets the cluster's real cardinality,
			// where a many-to-one join can find the "one" side carrying two members and abort the
			// whole evaluation with a duplicate-match error — silently, forever, in the rule's own
			// health field. BackupMissed is exactly that shape.
			Expect(rule.Health).To(Equal("ok"),
				"rule %s is unhealthy in Prometheus (%s) — it is loaded and it is not evaluating: %s",
				name, rule.Health, rule.LastError)
			Expect(rule.LastError).To(BeEmpty(),
				"rule %s last evaluation error: %s", name, rule.LastError)
		}

		By("And the operator's own series are actually in the database")
		// The one series that is always present (§2.10). If this is empty the scrape is arriving
		// with no crystalbackup_ content at all, which no alert-level assertion would explain.
		samples, err := m6Query("crystalbackup_build_info")
		Expect(err).NotTo(HaveOccurred())
		Expect(samples).NotTo(BeEmpty(),
			"Prometheus has the target up but no crystalbackup_build_info — the scrape is reaching "+
				"something that is not the operator's /metrics")
	})

	// ------------------------------------------------------------------
	// PVCSnapshotPileup — started first because it holds for 30 minutes.
	//
	// The condition is set up here and asserted at the very end of the container, so the hold runs
	// underneath the repository work instead of after it. That is the only reason this lane fits in
	// under an hour.
	//
	// THE TEARDOWN IS AN AfterAll, AND IT HAS TO BE. A DeferCleanup registered inside this It runs
	// when THIS It ends, not when the container does — so the namespace, the PVC and all 21
	// VolumeSnapshots were being destroyed seconds after they were created, the gauge fell back
	// through the threshold, and the alert at the bottom of this container could never fire. The
	// first real run showed the series going 21 → 3 while the namespace drained. Anything whose
	// condition has to survive into a later spec belongs in AfterAll.
	// ------------------------------------------------------------------
	AfterAll(func() { deleteNamespaceAndWaitGone(pileupNS, 15*time.Minute) })

	It("counts another tool's VolumeSnapshots piling up on one PVC", func() {
		By("Given a tenant PVC on the snapshot-capable class")
		ensureNamespace(pileupNS)
		m2SeedVolume(pileupNS, "m6-pileup-data", m4StorageClass, "1Gi")

		By("When 21 VolumeSnapshots accumulate on it — one past the rule's threshold")
		// 21, not 40. `> 20` is a strict comparison with a deliberate one-snapshot margin
		// (adr/0006), and provoking it with a comfortable 40 would pass just as well against a
		// rule whose threshold had drifted to 30. The promtool layer tests 20-vs-21 as an
		// arithmetic fact; this tests that the real object count is what reaches the series.
		//
		// These are NOT the operator's own snapshots, and the metric is documented to count
		// everyone's: the scenario in adr/0006 is coexistence with an incumbent tool, and a count
		// restricted to our own exposures would read reassuringly low on exactly the cluster where
		// the pile-up is happening.
		for i := range 21 {
			name := fmt.Sprintf("m6-pileup-%s-%02d", runID, i)
			snap := newVolumeSnapshot(pileupNS, name, "m6-pileup-data", m6SnapshotClass())
			Expect(k8s.Create(ctx, snap)).To(Succeed(), "create VolumeSnapshot %s/%s", pileupNS, name)
		}

		By("Then the series reaches 21 in Prometheus")
		// Asserted separately from the alert, and before it, so a failure says which half broke:
		// the collector not counting, or the rule not matching what it counted.
		Eventually(func(g Gomega) {
			samples, err := m6Query(fmt.Sprintf(
				`crystalbackup_pvc_volumesnapshot_count{namespace=%q,pvc="m6-pileup-data"}`, pileupNS))
			g.Expect(err).NotTo(HaveOccurred())
			// This is the assertion that caught the honorLabels collision, and its message says so:
			// "0 series" here does NOT mean the collector is silent. It meant the series existed
			// under namespace="crystal-backup-system" with the real value hidden in
			// exported_namespace. The invariant at the top of this container now fails first and
			// says that directly — this line stays specific about the alternative so the next
			// reader is not sent looking at the collector.
			g.Expect(samples).To(HaveLen(1),
				"expected exactly one series for %s/m6-pileup-data, got %d. If this is 0, check the "+
					"label VALUES before the collector: `{__name__=\"crystalbackup_pvc_volumesnapshot_count\"}` "+
					"shows what namespace the series actually carries", pileupNS, len(samples))
			g.Expect(samples[0].Value).To(HaveLen(2))
			g.Expect(samples[0].Value[1]).To(Equal("21"),
				"the collector counts %v VolumeSnapshots for %s/m6-pileup-data, 21 were created",
				samples[0].Value[1], pileupNS)
		}, 5*time.Minute, 10*time.Second).Should(Succeed())
	})

	// ------------------------------------------------------------------
	// BackupFailed — the cheapest end-to-end proof in the table.
	//
	// No `for:` at all, so it is the one rule whose page arrives within one evaluation of the
	// condition. If the chain is broken anywhere, this is where it shows first and fastest.
	// ------------------------------------------------------------------
	Describe("a backup that fails", Ordered, func() {
		var (
			locName    = "m6-alerts-failed-" + runID
			secretName = "m6-alerts-s3-" + runID
		)

		It("increments the failure counter and pages, with no hold to wait out", func() {
			By("Given an isolated repository with its OWN S3 credentials Secret")
			// Its own Secret, and that is not tidiness. The provocation below replaces the
			// credentials with garbage; doing that to the shared crucible-dr-s3 Secret would break
			// every other spec in the run and turn one finding into a cascade nobody can read.
			m6CreateS3Secret(secretName, os.Getenv("AWS_ACCESS_KEY_ID"), os.Getenv("AWS_SECRET_ACCESS_KEY"))
			DeferCleanup(func() { m6DeleteSecret(secretName) })

			loc := m6IsolatedLocation(locName, "m6-alerts-failed-"+runID, secretName)
			Expect(k8s.Create(ctx, loc)).To(Succeed(), "create ClusterBackupLocation %s", locName)
			DeferCleanup(func() { m1DeleteLocation(locName) })
			m1WaitRepositoryInitialized(locName)

			By("And one backup that SUCCEEDS, so the failure below means what it says")
			ensureNamespace(failNS)
			DeferCleanup(func() { deleteNamespaceAndWaitGone(failNS, 10*time.Minute) })
			m2SeedVolume(failNS, "m6-failed-data", m4StorageClass, "1Gi")

			okRun := "m6-alerts-ok-" + runID
			m1RunClusterBackup(okRun, locName, cbv1.NamespaceSelector{MatchNames: []string{failNS}})
			DeferCleanup(func() {
				_ = k8s.Delete(ctx, &cbv1.ClusterBackup{ObjectMeta: metav1.ObjectMeta{Name: okRun}})
			})
			first := m1WaitClusterBackupTerminal(okRun, 20*time.Minute)
			Expect(first.Status.Phase).To(Equal("Completed"),
				"the baseline backup must succeed before anything is broken, phase=%q", first.Status.Phase)

			By("When the location's S3 credentials are replaced with garbage")
			// Replaced, not deleted. A deleted Secret leaves the mover Job uncreatable and the
			// Backup sitting in Pending, which is a different (and untested) failure; garbage
			// credentials let the Job run and be rejected by the object store, which is the
			// ordinary shape of this incident — a rotated key nobody updated.
			m6CreateS3Secret(secretName, "revoked-by-the-alert-lane", "revoked-by-the-alert-lane")

			By("And a second backup runs against it")
			badRun := "m6-alerts-fail-" + runID
			m1RunClusterBackup(badRun, locName, cbv1.NamespaceSelector{MatchNames: []string{failNS}})
			DeferCleanup(func() {
				_ = k8s.Delete(ctx, &cbv1.ClusterBackup{ObjectMeta: metav1.ObjectMeta{Name: badRun}})
			})

			By("Then a Backup reaches a failed phase")
			// The intermediate assertion, stated on its own. Without it, a timeout on the alert
			// below would be ambiguous between "the backup never failed" and "it failed and the
			// alert did not fire", which are opposite bugs.
			Eventually(func(g Gomega) {
				var failed []string
				for _, bk := range m1ListBackups(failNS) {
					if bk.Status.Phase == "Failed" || bk.Status.Phase == "PartiallyFailed" {
						failed = append(failed, bk.Name+"="+bk.Status.Phase)
					}
				}
				g.Expect(failed).NotTo(BeEmpty(),
					"no Backup in %s has failed yet — with unusable S3 credentials the mover Job "+
						"cannot reach the repository; check the Jobs in %s", failNS, operatorNS)
			}, 25*time.Minute, 15*time.Second).Should(Succeed())

			By("And the counter series moves")
			Eventually(func(g Gomega) {
				samples, err := m6Query(fmt.Sprintf(
					`increase(crystalbackup_backup_failures_total{namespace=%q}[1h])`, failNS))
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(samples).NotTo(BeEmpty(),
					"crystalbackup_backup_failures_total has no increase for %s — the Backup failed "+
						"but the counter did not move, which is the exact gap the counter replaced the "+
						"gauge to close", failNS)
			}, 5*time.Minute, 15*time.Second).Should(Succeed())

			By("And CrystalbackupBackupFailed fires, naming the namespace")
			alert := m6WaitAlertFiring("CrystalbackupBackupFailed",
				map[string]string{"namespace": failNS}, 10*time.Minute)
			Expect(alert.Labels).To(HaveKeyWithValue("severity", "warning"))
			Expect(alert.Annotations["description"]).To(ContainSubstring("kubectl get backups -n "+failNS),
				"the description must tell the operator what to run, annotations=%v", alert.Annotations)
		})
	})

	// ------------------------------------------------------------------
	// RepositoryCheckFailed — the only critical rule, and the only one that says the RESTORE PATH
	// is compromised. Provoked exactly the way M4 provokes it, because that provocation is already
	// known to reach `restic check`: bytes rewritten inside a pack object, name and length intact.
	// ------------------------------------------------------------------
	Describe("a repository whose check fails", Ordered, func() {
		var locName = "m6-alerts-corrupt-" + runID

		It("pages critical after the 5m hold, and only for the damaged repository", func() {
			By("Given an isolated repository that checks its own data on every run")
			clusterID := "m6-alerts-corrupt-" + runID
			repo := m4CreateIsolatedLocation(locName, clusterID, &cbv1.MaintenanceSpec{
				CheckSchedule:       m4EverySecondCron,
				CheckReadDataSubset: "100%",
			})

			ensureNamespace(corruptNS)
			DeferCleanup(func() { deleteNamespaceAndWaitGone(corruptNS, 10*time.Minute) })
			m2SeedVolume(corruptNS, "m6-corrupt-data", m4StorageClass, "1Gi")
			run := "m6-alerts-corrupt-" + runID
			m1RunClusterBackup(run, locName, cbv1.NamespaceSelector{MatchNames: []string{corruptNS}})
			DeferCleanup(func() {
				_ = k8s.Delete(ctx, &cbv1.ClusterBackup{ObjectMeta: metav1.ObjectMeta{Name: run}})
			})
			m1WaitClusterBackupTerminal(run, 20*time.Minute)
			seededAt := time.Now()

			By("And a check that passes first, so the alert below is caused by the corruption")
			clean := m4WaitMaintenanceRecord(repo.Name, "check", seededAt, 20*time.Minute)
			Expect(clean.Result).To(Equal(cbv1.MaintenanceSucceeded),
				"the repository was already unhealthy before anything was corrupted: %s", clean.Message)

			By("And the rule is silent while the repository is healthy")
			// The negative, taken at the moment it is true. Without it, "the alert fires after the
			// corruption" is compatible with an alert that had been firing since the location was
			// created — which is precisely what spec §2.4's absence semantics exist to prevent, and
			// precisely what a spec that only ever looks after the damage cannot see.
			healthy, err := m6Alerts()
			Expect(err).NotTo(HaveOccurred())
			Expect(m6AlertsNamed(healthy, "CrystalbackupRepositoryCheckFailed",
				map[string]string{"location": locName})).To(BeEmpty(),
				"CrystalbackupRepositoryCheckFailed is already up for a repository whose check just "+
					"passed: %s", m6DescribeAlerts(healthy))

			By("When bytes inside one pack are replaced, keeping its name and its length")
			key := m4CorruptOnePack(clusterID)

			By("Then the check fails on the repository object")
			Eventually(func(g Gomega) {
				var live cbv1.BackupRepository
				g.Expect(k8s.Get(ctx, client.ObjectKey{Name: repo.Name}, &live)).To(Succeed())
				g.Expect(live.Status.LastCheckResult).To(Equal("Failed"),
					"pack %s was corrupted and check still reports %q", key, live.Status.LastCheckResult)
			}, 30*time.Minute, 15*time.Second).Should(Succeed())

			By("And CrystalbackupRepositoryCheckFailed fires — critical, for THIS location")
			alert := m6WaitAlertFiring("CrystalbackupRepositoryCheckFailed",
				map[string]string{"location": locName}, 15*time.Minute)
			Expect(alert.Labels).To(HaveKeyWithValue("severity", "critical"),
				"this is the one rule that says restores may not work; warning is not enough")
			Expect(alert.Labels).To(HaveKeyWithValue("scope", "cluster"),
				"the label vocabulary is lowercase (spec §2.4), not the API enum")

			By("And the shared dr repository, which nobody damaged, is not paging")
			// The rule matches on a series, not on a global condition. If a corrupted repository
			// took the whole fleet's alerting with it, an operator would have no way to tell which
			// repository to go and look at — and the m4 lane corrupts one on every run.
			all, err := m6Alerts()
			Expect(err).NotTo(HaveOccurred())
			Expect(m6AlertsNamed(all, "CrystalbackupRepositoryCheckFailed",
				map[string]string{"location": m1LocationName})).To(BeEmpty(),
				"the healthy shared repository %q is paging too: %s", m1LocationName, m6DescribeAlerts(all))
		})
	})

	// ------------------------------------------------------------------
	// And now the 30-minute hold started at the top of the container has had the repository work
	// running underneath it.
	// ------------------------------------------------------------------
	It("pages on the snapshot pile-up once its 30m hold has elapsed", func() {
		alert := m6WaitAlertFiring("CrystalbackupPVCSnapshotPileup",
			map[string]string{"namespace": pileupNS, "pvc": "m6-pileup-data"}, 35*time.Minute)
		Expect(alert.Labels).To(HaveKeyWithValue("severity", "warning"))
		// $value in the summary is the reason the per-PVC label exception is worth paying for: the
		// operator is told which claim and how many, not that "a namespace has too many snapshots".
		Expect(alert.Annotations["summary"]).To(ContainSubstring("21 VolumeSnapshots piling up on PVC "+pileupNS+"/m6-pileup-data"),
			"summary=%q", alert.Annotations["summary"])
	})
})

// ---------------------------------------------------------------------------
// Fixtures used only by the alert lane.
// ---------------------------------------------------------------------------

// m6FamilyOf maps a scraped series name back to its catalogue family.
//
// Prometheus derives crystalbackup_backup_duration_seconds_{bucket,sum,count} from ONE histogram
// family, and only the undecorated name is a key in metrics.Catalogue(). Stripping the suffix
// blindly would be wrong in the other direction — crystalbackup_backup_failures_total and
// crystalbackup_backup_added_bytes_total are counters whose full name IS the family — so the strip
// only applies when what is left is a declared histogram.
func m6FamilyOf(series string) string {
	for _, suffix := range []string{"_bucket", "_sum", "_count"} {
		base, found := strings.CutSuffix(series, suffix)
		if found && slices.Contains(metrics.Histograms(), base) {
			return base
		}
	}
	return series
}

// m6Unique de-duplicates and sorts a list for a failure message. A collision affects every series
// in a family at once, so the raw list is the same sentence repeated thirty times.
func m6Unique(in []string) []string {
	out := slices.Clone(in)
	slices.Sort(out)
	return slices.Compact(out)
}

// m6RuleNames lists the alert names Prometheus has loaded, for a failure message.
func m6RuleNames(loaded map[string]m6Rule) []string {
	out := make([]string, 0, len(loaded))
	for name := range loaded {
		out = append(out, name)
	}
	return out
}

// m6SnapshotClass is the VolumeSnapshotClass matching m4StorageClass. Both default to the Ceph RBD
// pair the crucible provisions and both are overridable together — a storage class overridden
// without its snapshot class would produce 21 VolumeSnapshots that never bind, which the pile-up
// metric would still count (it counts objects) and which would make this spec pass while measuring
// nothing real.
func m6SnapshotClass() string { return envOr("CRUCIBLE_SNAPSHOT_CLASS", "ceph-block") }

// newVolumeSnapshot builds a dynamic VolumeSnapshot of a PVC. Unstructured, like every other
// snapshot fixture in this suite: the type is not in the operator's scheme.
//
// Unlike snapshotAndWaitReady it does NOT wait for readyToUse, and that is correct rather than
// lazy: crystalbackup_pvc_volumesnapshot_count counts VolumeSnapshot OBJECTS pointing at a source
// claim, because the ceph-csi chain that leads to a flatten is built from requests, not from
// completions. Waiting for twenty-one Ceph snapshots to bind would add minutes and test the CSI
// driver instead of the alert.
func newVolumeSnapshot(ns, name, pvcName, snapClass string) *unstructured.Unstructured {
	GinkgoHelper()
	snap := &unstructured.Unstructured{}
	snap.SetGroupVersionKind(volumeSnapshotGVK())
	snap.SetName(name)
	snap.SetNamespace(ns)
	Expect(unstructured.SetNestedField(snap.Object, snapClass, "spec", "volumeSnapshotClassName")).To(Succeed())
	Expect(unstructured.SetNestedField(snap.Object, pvcName, "spec", "source", "persistentVolumeClaimName")).To(Succeed())
	return snap
}

// m6CreateS3Secret creates or REPLACES the S3 credentials Secret named for this lane.
//
// Replace-in-place rather than delete-and-recreate: the provocation is a credential rotation nobody
// propagated, and a Secret that briefly does not exist is a different situation the operator treats
// differently (no Job at all, versus a Job that runs and is refused).
func m6CreateS3Secret(name, accessKey, secretKey string) {
	GinkgoHelper()
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: operatorNS},
		Type:       corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"AWS_ACCESS_KEY_ID":     []byte(accessKey),
			"AWS_SECRET_ACCESS_KEY": []byte(secretKey),
		},
	}
	err := k8s.Create(ctx, secret)
	if apierrors.IsAlreadyExists(err) {
		var live corev1.Secret
		Expect(k8s.Get(ctx, client.ObjectKeyFromObject(secret), &live)).To(Succeed())
		live.Data = secret.Data
		Expect(k8s.Update(ctx, &live)).To(Succeed(), "update S3 Secret %s/%s", operatorNS, name)
		return
	}
	Expect(err).NotTo(HaveOccurred(), "create S3 Secret %s/%s", operatorNS, name)
}

func m6DeleteSecret(name string) {
	_ = k8s.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: operatorNS}})
}

// m6IsolatedLocation is m4LocationObject with one difference that matters: its own credentials
// Secret, so this lane can break those credentials without breaking the whole run.
func m6IsolatedLocation(name, clusterID, secretName string) *cbv1.ClusterBackupLocation {
	return &cbv1.ClusterBackupLocation{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: cbv1.ClusterBackupLocationSpec{
			Mode:      cbv1.LocationModeStandard,
			ClusterID: clusterID,
			S3: cbv1.S3Spec{
				Endpoint:             os.Getenv("S3_ENDPOINT"),
				Bucket:               os.Getenv("S3_BUCKET"),
				Prefix:               m1S3Prefix,
				Region:               envOr("S3_REGION", "fsn1"),
				CredentialsSecretRef: cbv1.LocalObjectReference{Name: secretName},
				ForcePathStyle:       true,
			},
			Encryption: cbv1.ClusterEncryptionSpec{
				ClusterKEKSecretRef: cbv1.LocalObjectReference{Name: m1KEKSecretName},
			},
			// Off, explicitly. The field defaults to true, and a discovery pass against a
			// repository whose credentials have just been made unusable would raise
			// CrystalbackupDiscoveryFailed for this location as well — a second alert this spec
			// does not assert and whose presence would confuse the reader of a failure.
			Discovery: cbv1.DiscoverySpec{Enabled: ptr.To(false)},
		},
	}
}
