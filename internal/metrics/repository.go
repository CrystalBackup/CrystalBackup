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

package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
)

// The repository family (05-observability.md §2.4). These are PER-REPOSITORY, not per-namespace:
// the cluster plane shares ONE repository across every namespace (adr/0009), so `namespace` is
// EMPTY here for the shared cluster repository and set only for a scope=Namespaced tenant
// repository. That is what makes a check failure on the shared repo a platform-wide signal that
// routes to admins, while the same metric with a namespace routes to that tenant.
const scopeLabel = "scope"

var (
	repositoryLabels = []string{locationLabel, scopeLabel, namespaceLabel, clusterLabel}

	repositorySizeDesc = prometheus.NewDesc(
		NameRepositorySize,
		"Physical size of the repository in object storage, post-dedup and post-compression (status.approximateSizeBytes).",
		repositoryLabels, nil)
	repositorySnapshotCountDesc = prometheus.NewDesc(
		NameRepositorySnapshots,
		"Snapshots present in the repository (status.snapshotCount).",
		repositoryLabels, nil)
	repositoryLastCheckDesc = prometheus.NewDesc(
		NameRepositoryLastCheck,
		"Unix time of the last completed restic check, whether it passed or failed (status.lastCheckTime).",
		repositoryLabels, nil)
	repositoryLastCheckSuccessDesc = prometheus.NewDesc(
		NameRepositoryCheckSuccess,
		"1 if the last restic check passed, 0 if it found repository damage. Drives RepositoryCheckFailed.",
		repositoryLabels, nil)
	repositoryLastMaintenanceDesc = prometheus.NewDesc(
		NameRepositoryLastPrune,
		"Unix time of the last SUCCESSFUL prune (status.lastMaintenanceTime). Absent on Immutable locations, which never prune.",
		repositoryLabels, nil)
	repositoryStaleLocksDesc = prometheus.NewDesc(
		NameRepositoryStaleLocks,
		"Repository lock objects older than restic's 30-minute staleness threshold (status.staleLocks).",
		repositoryLabels, nil)
)

// locksReaped backs crystalbackup_repository_locks_reaped_total.
//
// It is the one imperative metric in this package, and the exception proves the rule. Everything
// else here is recomputed from object state on each scrape, which is what makes it restart-safe.
// A reaped lock has no state to recompute FROM: the whole point of the operation is that the lock
// object no longer exists, and status.recentMaintenance is a capped window, not a total. So this
// one is a real counter, incremented where the reaping happens. A restart resets it, which is
// exactly what Prometheus counters are designed to survive (rate/increase handle the reset).
var locksReaped = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: NameRepositoryLocksReaped,
	Help: "Stale repository locks removed by an unlock operation.",
}, repositoryLabels)

func init() { ctrlmetrics.Registry.MustRegister(locksReaped) }

// RecordLockReaped counts one successful stale-lock removal on the shared cluster repository.
// Called from the unlock path, which runs after a hard-killed mover left a lock behind.
func RecordLockReaped(location, cluster string) {
	locksReaped.WithLabelValues(location, scopeCluster, "", cluster).Inc()
}

// The `scope` LABEL VALUES, which are deliberately not the API's enum values.
//
// BackupRepositoryStatus.Scope is a kubebuilder enum of `Cluster;Namespaced`. Publishing it
// verbatim is what this collector used to do, and it made `scope` mean two different things
// depending on which family you read: repository and discovery emitted `Cluster|Namespaced`
// while external sync emitted `cluster|namespace` (it derives from apiconst.Origin*). An
// operator writing one alert expression across both had to know which vocabulary each family
// spoke — and the mismatch is invisible until a rule silently matches nothing.
//
// One vocabulary, lowercase, matching what §2.4/§2.5 always specified and what the `origin`
// label — the parallel dimension that `scope` REPLACES on these families (§2) — already uses.
// Lowercase is also the Prometheus convention for enumerated label values.
const (
	scopeCluster   = "cluster"
	scopeNamespace = "namespace"
)

// metricScope maps a BackupRepository's API scope onto the label vocabulary above. An empty
// status scope means the shared cluster repository: it is the one that exists before anything
// sets the field.
func metricScope(apiScope string) string {
	if apiScope == "Namespaced" {
		return scopeNamespace
	}
	return scopeCluster
}

// collectRepositories emits one series per repository per gauge. A repository that has never been
// checked emits no check series at all — deliberately: a zero timestamp would render as 1970 on a
// dashboard, and a last_check_success of 0 would fire RepositoryCheckFailed for a repository whose
// only sin is being new. Absent means "not measured"; present means "measured, here is the answer".
func collectRepositories(ch chan<- prometheus.Metric, repos []cbv1.BackupRepository, clusterByLocation map[string]string) {
	for i := range repos {
		repo := &repos[i]
		location := repo.Status.Location.Name
		if location == "" {
			location = repo.Name // the repository is named after its location
		}
		labels := []string{location, metricScope(repo.Status.Scope), repo.Status.OwnerNamespace, clusterByLocation[location]}

		gauge := func(desc *prometheus.Desc, v float64) {
			ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, v, labels...)
		}
		gauge(repositorySizeDesc, float64(repo.Status.ApproximateSizeBytes))
		gauge(repositorySnapshotCountDesc, float64(repo.Status.SnapshotCount))
		gauge(repositoryStaleLocksDesc, float64(repo.Status.StaleLocks))

		if t := repo.Status.LastCheckTime; t != nil {
			gauge(repositoryLastCheckDesc, float64(t.Unix()))
			success := 0.0
			if repo.Status.LastCheckResult == "Passed" {
				success = 1
			}
			gauge(repositoryLastCheckSuccessDesc, success)
		}
		if t := repo.Status.LastMaintenanceTime; t != nil {
			gauge(repositoryLastMaintenanceDesc, float64(t.Unix()))
		}
	}
}
