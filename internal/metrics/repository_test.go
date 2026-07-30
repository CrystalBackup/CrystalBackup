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

func TestCollectorRepositorySeries(t *testing.T) {
	checked := metav1.Date(2026, 7, 25, 5, 0, 0, 0, time.UTC)
	pruned := metav1.Date(2026, 7, 25, 3, 0, 0, 0, time.UTC)

	loc := &cbv1.ClusterBackupLocation{
		ObjectMeta: metav1.ObjectMeta{Name: "dr"},
		Spec:       cbv1.ClusterBackupLocationSpec{ClusterID: "prod-eu-1"},
	}
	repo := &cbv1.BackupRepository{
		ObjectMeta: metav1.ObjectMeta{Name: "dr"},
		Status: cbv1.BackupRepositoryStatus{
			Location:             cbv1.RepositoryLocationRef{Kind: "ClusterBackupLocation", Name: "dr"},
			Scope:                "Cluster",
			Initialized:          true,
			SnapshotCount:        4123,
			ApproximateSizeBytes: 987654321,
			StaleLocks:           2,
			LastCheckTime:        &checked,
			LastCheckResult:      "Passed",
			LastMaintenanceTime:  &pruned,
		},
	}

	reg := prometheus.NewRegistry()
	reg.MustRegister(NewCollector(newFakeClient(t, loc, repo), testOperatorNamespace))

	// namespace is EMPTY for the shared cluster repository: these are per-repository series, and a
	// check result on the shared repo is a platform-wide signal, not one tenant's (05-obs §2.4).
	want := map[string]string{"location": "dr", "scope": "Cluster", "namespace": "", "cluster": "prod-eu-1"}

	cases := []struct {
		metric string
		value  float64
	}{
		{"crystalbackup_repository_size_bytes", 987654321},
		{"crystalbackup_repository_snapshot_count", 4123},
		{"crystalbackup_repository_stale_locks", 2},
		{"crystalbackup_repository_last_check_timestamp_seconds", float64(checked.Unix())},
		{"crystalbackup_repository_last_check_success", 1},
		{"crystalbackup_repository_last_maintenance_timestamp_seconds", float64(pruned.Unix())},
	}
	for _, tc := range cases {
		if got, ok := gatherValue(t, reg, tc.metric, want); !ok || got != tc.value {
			t.Errorf("%s = %v (found=%v), want %v", tc.metric, got, ok, tc.value)
		}
	}
}

// TestCollectorRepositoryNeverCheckedEmitsNoCheckSeries: absence must mean "not measured".
// A zero timestamp renders as 1970 on a dashboard, and a last_check_success of 0 would fire
// CrystalbackupRepositoryCheckFailed — a critical page — against a repository whose only sin is
// having been created five minutes ago.
func TestCollectorRepositoryNeverCheckedEmitsNoCheckSeries(t *testing.T) {
	loc := &cbv1.ClusterBackupLocation{
		ObjectMeta: metav1.ObjectMeta{Name: "dr"},
		Spec:       cbv1.ClusterBackupLocationSpec{ClusterID: "prod-eu-1"},
	}
	repo := &cbv1.BackupRepository{
		ObjectMeta: metav1.ObjectMeta{Name: "dr"},
		Status:     cbv1.BackupRepositoryStatus{Scope: "Cluster", Initialized: true},
	}

	reg := prometheus.NewRegistry()
	reg.MustRegister(NewCollector(newFakeClient(t, loc, repo), testOperatorNamespace))

	want := map[string]string{"location": "dr", "scope": "Cluster", "namespace": "", "cluster": "prod-eu-1"}
	for _, m := range []string{
		"crystalbackup_repository_last_check_timestamp_seconds",
		"crystalbackup_repository_last_check_success",
		"crystalbackup_repository_last_maintenance_timestamp_seconds",
	} {
		if _, ok := gatherValue(t, reg, m, want); ok {
			t.Errorf("%s was emitted for a repository that has never been checked or pruned", m)
		}
	}
	// The always-measurable gauges are still there, at zero.
	if _, ok := gatherValue(t, reg, "crystalbackup_repository_size_bytes", want); !ok {
		t.Error("size_bytes must be emitted even before the first probe")
	}
}

// TestCollectorRepositoryFailedCheck: the alert in 05-observability §3 fires on
// last_check_success == 0, so a Failed result must produce the series with value 0 — NOT omit it.
func TestCollectorRepositoryFailedCheck(t *testing.T) {
	checked := metav1.Date(2026, 7, 25, 5, 0, 0, 0, time.UTC)
	loc := &cbv1.ClusterBackupLocation{
		ObjectMeta: metav1.ObjectMeta{Name: "dr"},
		Spec:       cbv1.ClusterBackupLocationSpec{ClusterID: "prod-eu-1"},
	}
	repo := &cbv1.BackupRepository{
		ObjectMeta: metav1.ObjectMeta{Name: "dr"},
		Status: cbv1.BackupRepositoryStatus{
			Scope: "Cluster", Initialized: true,
			LastCheckTime: &checked, LastCheckResult: "Failed",
		},
	}

	reg := prometheus.NewRegistry()
	reg.MustRegister(NewCollector(newFakeClient(t, loc, repo), testOperatorNamespace))

	want := map[string]string{"location": "dr", "scope": "Cluster", "namespace": "", "cluster": "prod-eu-1"}
	got, ok := gatherValue(t, reg, "crystalbackup_repository_last_check_success", want)
	if !ok {
		t.Fatal("last_check_success was not emitted for a failed check — the alert would never fire")
	}
	if got != 0 {
		t.Errorf("last_check_success = %v, want 0", got)
	}
}
