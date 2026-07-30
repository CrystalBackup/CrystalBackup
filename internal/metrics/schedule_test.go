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
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
)

func backupSchedule(name, cron string, created time.Time) *cbv1.BackupSchedule {
	return &cbv1.BackupSchedule{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "team-a", Name: name, CreationTimestamp: metav1.NewTime(created),
		},
		Spec: cbv1.BackupScheduleSpec{
			Schedule:    cron,
			LocationRef: cbv1.LocalObjectReference{Name: "own"},
		},
	}
}

func scheduleSeriesLabels(name string) map[string]string {
	return map[string]string{
		"namespace": "team-a", "tenant": "team-a", "schedule": name,
		"origin": "namespace", "location": "own", "cluster": "",
	}
}

// TestSchedulePeriodIsTheSchedulesOwn is the series that let CrystalbackupBackupMissed stop
// carrying a hardcoded 26h deadline (spec §8 question 3). A weekly schedule under the old fixed
// threshold paged every single Tuesday until someone silenced it permanently, and an hourly one
// could be a full day dead in silence.
func TestSchedulePeriodIsTheSchedulesOwn(t *testing.T) {
	created := time.Now().Add(-72 * time.Hour)
	reg := prometheus.NewRegistry()
	reg.MustRegister(NewCollector(newFakeClient(t,
		drLocation(),
		backupSchedule("nightly", "0 3 * * *", created),
		backupSchedule("weekly", "0 3 * * 0", created),
		backupSchedule("hourly", "0 * * * *", created),
		namespaceObj("team-a", nil),
	), testOperatorNamespace))

	for name, want := range map[string]float64{
		"nightly": 24 * 3600,
		"weekly":  7 * 24 * 3600,
		"hourly":  3600,
	} {
		got, ok := gatherValue(t, reg, NameSchedulePeriod, scheduleSeriesLabels(name))
		if !ok || got != want {
			t.Errorf("%s period = %v (found=%v), want %v", name, got, ok, want)
		}
	}
}

// TestSchedulePeriodAbsentForAnUnparseableCron pins the absence the alert rule has an explicit
// branch for. Emitting a made-up period here would be quieter and worse: the rule would derive a
// deadline for a schedule that is never going to run, instead of falling back to the fixed one and
// paging.
func TestSchedulePeriodAbsentForAnUnparseableCron(t *testing.T) {
	reg := prometheus.NewRegistry()
	reg.MustRegister(NewCollector(newFakeClient(t,
		drLocation(),
		backupSchedule("broken", "not a cron expression", time.Now()),
		namespaceObj("team-a", nil),
	), testOperatorNamespace))

	if n := countSamples(t, reg, NameSchedulePeriod); n != 0 {
		t.Errorf("schedule_period samples = %d, want 0 for an unparseable cron", n)
	}
	// schedule_active and schedule_created must still be there: a broken schedule is exactly the
	// one that has to keep paging, and both of those are what the fallback branch joins on.
	if n := countSamples(t, reg, NameScheduleActive); n != 1 {
		t.Errorf("schedule_active samples = %d, want 1 — a broken schedule must still be alertable", n)
	}
	if n := countSamples(t, reg, NameScheduleCreated); n != 1 {
		t.Errorf("schedule_created samples = %d, want 1", n)
	}
}

// TestScheduleCreatedIsTheNeverSucceededFallback covers the hole M6 found in BackupMissed.
//
// spec §2.1 claims crystalbackup_backup_last_success_timestamp_seconds is "initialized to the
// schedule's creation time on first reconcile, so BackupMissed fires even if no backup ever
// succeeded". The collector does no such thing — last_success comes off Backup objects, and a
// schedule that has never produced a successful one emits NOTHING. `time() - <nothing>` is
// nothing, so an installation that was broken from the day it was applied was the single case the
// rule could never see. This series is what the rule falls back to.
func TestScheduleCreatedIsTheNeverSucceededFallback(t *testing.T) {
	created := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	reg := prometheus.NewRegistry()
	reg.MustRegister(NewCollector(newFakeClient(t,
		drLocation(),
		backupSchedule("nightly", "0 3 * * *", created),
		namespaceObj("team-a", nil),
	), testOperatorNamespace))

	got, ok := gatherValue(t, reg, NameScheduleCreated, scheduleSeriesLabels("nightly"))
	if !ok || got != float64(created.Unix()) {
		t.Fatalf("schedule_created = %v (found=%v), want %d", got, ok, created.Unix())
	}
	// And the premise: no Backup exists, so there is no last_success to measure from.
	if n := countSamples(t, reg, NameBackupLastSuccess); n != 0 {
		t.Fatalf("last_success samples = %d — the premise of this fallback no longer holds; if the "+
			"collector now seeds last_success, simplify the BackupMissed rule", n)
	}
}

// TestSchedulePeriodAndCreatedShareScheduleActivesIdentity: the three series are joined on in one
// expression, so they must carry the same label set for the same schedule. A single transposed
// label here and the join silently finds no partner.
func TestSchedulePeriodAndCreatedShareScheduleActivesIdentity(t *testing.T) {
	reg := prometheus.NewRegistry()
	reg.MustRegister(NewCollector(newFakeClient(t,
		drLocation(),
		backupSchedule("nightly", "0 3 * * *", time.Now()),
		namespaceObj("team-a", nil),
	), testOperatorNamespace))

	want := scheduleSeriesLabels("nightly")
	for _, name := range []string{NameScheduleActive, NameSchedulePeriod, NameScheduleCreated} {
		if _, ok := gatherValue(t, reg, name, want); !ok {
			t.Errorf("%s carries a different label set from the others for the same schedule", name)
		}
	}
}
