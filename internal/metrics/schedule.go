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
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
	"github.com/CrystalBackup/CrystalBackup/internal/nsselector"
	"github.com/CrystalBackup/CrystalBackup/internal/schedule"
)

// crystalbackup_schedule_active (spec/05-observability.md §2.1) — the series the BackupMissed
// alert joins against, and the reason that alert could not fire before M6.
//
//	<age of the last success, per schedule>
//	  and on (namespace, schedule, origin, location, cluster)
//	    crystalbackup_schedule_active == 1
//
// The `and on` is what makes the rule usable: without it, every namespace whose schedule was
// deliberately removed, paused or never created keeps paging forever on a last_success that will
// never advance again. schedule_active is the operator's answer to "is anything still SUPPOSED to
// back this up" — and nothing else can answer it, because a Backup object records what happened,
// never what was expected to.
//
// The join key is the FULL series identity minus tenant, not the (namespace, schedule, cluster) an
// earlier draft of spec §3 used: `schedule` is a CR name, and a BackupSchedule and a
// ClusterBackupSchedule may share one, in which case the narrow key lets a namesake on the other
// plane answer for a schedule that is paused. internal/alerts/rules.go owns the expression and the
// full reasoning; this comment exists so a change HERE is made knowing what reads it.
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

// crystalbackup_schedule_period_seconds — the schedule's own cron period, and the reason
// CrystalbackupBackupMissed no longer carries a hardcoded 26 h deadline (spec §8 question 3).
//
// A fixed threshold is only ever right for one schedule shape. At 26 h an hourly schedule can be
// twenty-five hours dead before anyone hears about it, and a weekly one pages every Tuesday. The
// deadline belongs to the schedule, so the schedule publishes it and the rule reads it — which
// also means changing a cron expression moves its alert automatically, with no rule edit.
//
// ABSENT when the cron expression cannot be parsed. That is deliberate and the alert rule has an
// explicit branch for it: a schedule with a broken expression will never run, so it is exactly the
// case that must still page — under the old fixed deadline, since there is no period to derive one
// from. Emitting a made-up period here would be the quieter and worse choice.
var schedulePeriodDesc = prometheus.NewDesc(
	NameSchedulePeriod,
	"Longest gap between two consecutive activations of this schedule's cron expression, in seconds. Absent when the expression cannot be parsed.",
	backupLabels, nil)

// crystalbackup_schedule_created_timestamp_seconds — when the schedule object was created.
//
// This exists because of a hole M6 found in the BackupMissed rule that no amount of PromQL could
// have closed. spec §2.1 claims last_success is "initialized to the schedule's creation time on
// first reconcile, so BackupMissed fires even if no backup ever succeeded". The collector does no
// such thing, and cannot honestly: last_success is derived from Backup objects, and a schedule
// that has never produced one emits no series at all. `time() - <nothing>` is nothing, so a
// schedule that was broken from the day it was applied — the single most likely way for a new
// installation to be silently unprotected — was the one case the rule could never see.
//
// Writing a fake success timestamp into last_success would have closed it while lying to every
// dashboard that shows "last backup". A separate series says the true thing: this is when we
// started expecting backups, and the rule falls back to it when there is no success to measure
// from.
var scheduleCreatedDesc = prometheus.NewDesc(
	NameScheduleCreated,
	"Unix time the schedule object was created — the instant from which backups started being expected for this series.",
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
	// now is read once for the whole scrape so every schedule's period is derived from the same
	// instant: two series computed a few milliseconds apart across a DST boundary would otherwise
	// disagree by an hour for no reason a reader could ever explain.
	now := time.Now()
	emit := func(key backupSeriesKey, spec scheduleTiming) {
		if _, dup := emitted[key]; dup {
			return
		}
		emitted[key] = struct{}{}
		vals := key.values()
		ch <- prometheus.MustNewConstMetric(scheduleActiveDesc, prometheus.GaugeValue, 1, vals...)
		ch <- prometheus.MustNewConstMetric(scheduleCreatedDesc, prometheus.GaugeValue,
			float64(spec.created.Unix()), vals...)
		if parsed, err := schedule.Parse(spec.cron, spec.timezone); err == nil {
			ch <- prometheus.MustNewConstMetric(schedulePeriodDesc, prometheus.GaugeValue,
				parsed.Period(now).Seconds(), vals...)
		}
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
		}, scheduleTiming{cron: s.Spec.Schedule, timezone: s.Spec.Timezone, created: s.CreationTimestamp.Time})
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
		timing := scheduleTiming{
			cron: cs.Spec.Schedule, timezone: cs.Spec.Timezone, created: cs.CreationTimestamp.Time,
		}
		for _, ns := range matched {
			emit(backupSeriesKey{
				Namespace: ns,
				Tenant:    tenants.of(ns),
				Schedule:  cs.Name,
				Origin:    apiconst.OriginCluster,
				Location:  location,
				Cluster:   clusterByLocation[location],
			}, timing)
		}
	}
}

// scheduleTiming is the part of a schedule the period/creation series are derived from, carried
// separately so the two planes reach the same emit() with one shape.
type scheduleTiming struct {
	cron     string
	timezone string
	created  time.Time
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
