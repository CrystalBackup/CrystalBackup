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
	corev1 "k8s.io/api/core/v1"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
	"github.com/CrystalBackup/CrystalBackup/internal/nsselector"
)

// crystalbackup_schedule_active (spec/05-observability.md §2.1) — the series the BackupMissed
// alert joins against, and the reason that alert could not fire before M6.
//
//	(time() - crystalbackup_backup_last_success_timestamp_seconds > 26h)
//	  and on (namespace, schedule, cluster) crystalbackup_schedule_active == 1
//
// The `and on` is what makes the rule usable: without it, every namespace whose schedule was
// deliberately removed, paused or never created keeps paging forever on a last_success that will
// never advance again. schedule_active is the operator's answer to "is anything still SUPPOSED to
// back this up" — and nothing else can answer it, because a Backup object records what happened,
// never what was expected to.
//
// Its hard part is the cluster plane. A namespaced BackupSchedule names its own namespace, so its
// series is one lookup. A ClusterBackupSchedule names a SELECTION, and the fan-out it drives
// produces per-namespace Backups whose last_success series are per-namespace — so an active gauge
// emitted once per schedule would join against nothing. The selection is therefore RESOLVED here,
// against the live namespaces, exactly as the ClusterBackup controller resolves it at run time,
// and one series is emitted per matched namespace.
var scheduleActiveDesc = prometheus.NewDesc(
	NameScheduleActive,
	"1 when an unpaused schedule is expected to back up this (namespace, schedule): a namespaced BackupSchedule, or a ClusterBackupSchedule whose namespace selection matches.",
	backupLabels, nil)

// collectSchedules emits crystalbackup_schedule_active from both planes.
//
// The value is always 1: this is a presence series, and a paused or deleted schedule is ABSENT
// rather than 0. That is not a stylistic choice — `== 1` in the alert would also be satisfied by
// nothing at all if the series went to 0, but the join would still MATCH, and a paused schedule
// would keep the missed-backup rule alive on a namespace whose owner deliberately turned it off.
// Absence removes the join partner, which is the semantics the rule was written against.
func collectSchedules(ch chan<- prometheus.Metric,
	schedules []cbv1.BackupSchedule, clusterSchedules []cbv1.ClusterBackupSchedule,
	namespaces []corev1.Namespace, clusterByLocation map[string]string,
) {
	// De-duplicated by series: two ClusterBackupSchedules cannot collide (names are unique and
	// carry into the label), but a malformed cluster with duplicate-looking state must not make
	// the registry panic on a repeated label set mid-scrape — a metrics bug that takes /metrics
	// down with it is worse than any series it could have emitted.
	emitted := map[backupSeriesKey]struct{}{}
	emit := func(key backupSeriesKey) {
		if _, dup := emitted[key]; dup {
			return
		}
		emitted[key] = struct{}{}
		ch <- prometheus.MustNewConstMetric(scheduleActiveDesc, prometheus.GaugeValue, 1, key.values()...)
	}

	tenants := tenantsByNamespace(namespaces)

	// The namespace plane. BackupSchedule has NO paused field — the tenant-facing surface never
	// grew one (spec/02-api.md) — so a BackupSchedule that exists is a BackupSchedule that is
	// expected to run, and deleting it is how a user turns it off. Its location is a namespaced
	// BackupLocation, which carries no clusterID, so `cluster` is empty here; the Backups it
	// stamps out resolve their own cluster label from the same (absent) mapping, so the join in
	// BackupMissed still lines up. An invented cluster ID would be the thing that broke it.
	for i := range schedules {
		s := &schedules[i]
		location := s.Spec.LocationRef.Name
		emit(backupSeriesKey{
			Namespace: s.Namespace,
			Tenant:    tenants.of(s.Namespace),
			Schedule:  s.Name,
			Origin:    apiconst.OriginNamespace,
			Location:  location,
			Cluster:   clusterByLocation[location],
		})
	}

	// The cluster plane, resolved.
	for i := range clusterSchedules {
		cs := &clusterSchedules[i]
		if cs.Spec.Paused {
			continue
		}
		matched, err := nsselector.Match(namespaces, cs.Spec.Template.Spec.Namespaces)
		if err != nil {
			// A selector this schedule's own controller will refuse to fan out on. Emitting
			// nothing is the honest reading: no namespace is being backed up by it, and claiming
			// otherwise would suppress BackupMissed on namespaces that are genuinely unprotected.
			// The misconfiguration itself surfaces as a status condition on the schedule.
			continue
		}
		location := cs.Spec.Template.Spec.LocationRef.Name
		for _, ns := range matched {
			emit(backupSeriesKey{
				Namespace: ns,
				Tenant:    tenants.of(ns),
				Schedule:  cs.Name,
				Origin:    apiconst.OriginCluster,
				Location:  location,
				Cluster:   clusterByLocation[location],
			})
		}
	}
}

// namespaceTenants resolves a namespace to its tenant the same way the backup controllers do:
// the crystalbackup.io/tenant label if set, else the namespace name (one tenant per namespace,
// R19). It is a map rather than a per-lookup Get because a scrape resolves it for every matched
// namespace of every cluster schedule.
type namespaceTenants map[string]string

func tenantsByNamespace(namespaces []corev1.Namespace) namespaceTenants {
	out := make(namespaceTenants, len(namespaces))
	for i := range namespaces {
		ns := &namespaces[i]
		if t := ns.Labels[apiconst.LabelTenant]; t != "" {
			out[ns.Name] = t
		}
	}
	return out
}

func (t namespaceTenants) of(namespace string) string {
	if tenant, ok := t[namespace]; ok {
		return tenant
	}
	return namespace
}
