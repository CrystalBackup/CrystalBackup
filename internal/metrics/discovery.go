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

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
)

// The discovery family (spec/05-observability.md §2.5): the health of the repository→Backup
// projection that makes the REPOSITORY the source of truth (R26, adr/0009).
//
// These are state-derived like the repository family they sit beside, and for a stronger reason:
// discovery's whole promise is that a restore point survives the cluster that made it, so a
// metric about discovery that could not be rebuilt on a fresh cluster from the repository's own
// BackupRepository status would be undermining the property it reports on.
//
// No `namespace` label, unlike the repository family. §2.5 keys these on the repository alone,
// and rightly: projected and orphaned counts are properties of a SCAN, which covers every
// namespace in the repository at once. Splitting them per namespace would also make
// orphan_snapshots unrepresentable — an orphan's defining feature is that its namespace is gone.
var (
	discoveryLabels = []string{locationLabel, scopeLabel, clusterLabel}

	discoveryLastTimestampDesc = prometheus.NewDesc(
		NameDiscoveryLastTimestamp,
		"Unix time the last discovery scan of this repository completed (status.lastDiscoveryTime).",
		discoveryLabels, nil)
	discoveryLastSuccessDesc = prometheus.NewDesc(
		NameDiscoveryLastSuccess,
		"1 if the last discovery scan projected the whole repository cleanly, 0 if the listing failed or some groups could not be reconciled. Drives DiscoveryFailed.",
		discoveryLabels, nil)
	discoveryProjectedDesc = prometheus.NewDesc(
		NameDiscoveryProjected,
		"Snapshot (namespace, run) groups projected into existing namespaces by the last scan — what kubectl get backups lists for this repository.",
		discoveryLabels, nil)
	discoveryOrphansDesc = prometheus.NewDesc(
		NameDiscoveryOrphans,
		"Snapshot (namespace, run) groups whose namespace no longer exists. Non-zero is DR data for gone namespaces, restorable only via ClusterRestore — not an error.",
		discoveryLabels, nil)
)

// collectDiscovery emits the discovery gauges from each repository's status.
//
// A repository that has never been scanned emits NOTHING — not a zero. The reasoning is the one
// collectRepositories already documents for the check family, and it bites harder here:
// discovery_last_success == 0 is the DiscoveryFailed alert, so a fresh location whose first scan
// has not run yet would page the platform team for the crime of existing. And a projected count
// of 0 before a scan is indistinguishable, in a dashboard, from a repository the operator scanned
// and found empty — which is a real and different situation.
func collectDiscovery(ch chan<- prometheus.Metric, repos []cbv1.BackupRepository, clusterByLocation map[string]string) {
	for i := range repos {
		repo := &repos[i]
		// LastDiscoverySuccess is the marker for "a scan has reported", not lastDiscoveryTime:
		// the failing path records the outcome without a fresh time (the pass measured nothing),
		// so the boolean is the only field that is written on EVERY completed attempt.
		if repo.Status.LastDiscoverySuccess == nil {
			continue
		}
		location := repo.Status.Location.Name
		if location == "" {
			location = repo.Name // the repository is named after its location
		}
		scope := repo.Status.Scope
		if scope == "" {
			scope = scopeCluster
		}
		labels := []string{location, scope, clusterByLocation[location]}

		gauge := func(desc *prometheus.Desc, v float64) {
			ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, v, labels...)
		}
		success := 0.0
		if *repo.Status.LastDiscoverySuccess {
			success = 1
		}
		gauge(discoveryLastSuccessDesc, success)
		gauge(discoveryProjectedDesc, float64(repo.Status.ProjectedBackups))
		gauge(discoveryOrphansDesc, float64(repo.Status.OrphanSnapshots))
		if t := repo.Status.LastDiscoveryTime; t != nil {
			gauge(discoveryLastTimestampDesc, float64(t.Unix()))
		}
	}
}
