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
	dto "github.com/prometheus/client_model/go"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
	"github.com/CrystalBackup/CrystalBackup/internal/status"
)

// gatherValue gathers reg and returns the value of the sample of metric `name` whose labels equal
// `labels`, and whether it was found.
func gatherValue(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string) (float64, bool) {
	t.Helper()
	fams, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, fam := range fams {
		if fam.GetName() != name {
			continue
		}
		for _, m := range fam.GetMetric() {
			if labelsEqual(m, labels) {
				return m.GetGauge().GetValue(), true
			}
		}
	}
	return 0, false
}

func labelsEqual(m *dto.Metric, want map[string]string) bool {
	got := map[string]string{}
	for _, lp := range m.GetLabel() {
		got[lp.GetName()] = lp.GetValue()
	}
	if len(got) != len(want) {
		return false
	}
	for k, v := range want {
		if got[k] != v {
			return false
		}
	}
	return true
}

func newFakeClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := cbv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

func TestCollectorBackupSeries(t *testing.T) {
	backupTime := metav1.Date(2026, 7, 17, 2, 5, 0, 0, time.UTC)
	created := metav1.Date(2026, 7, 17, 2, 0, 0, 0, time.UTC) // 300s before success

	loc := &cbv1.ClusterBackupLocation{
		ObjectMeta: metav1.ObjectMeta{Name: "dr"},
		Spec:       cbv1.ClusterBackupLocationSpec{ClusterID: "c1"},
	}
	// Two runs in the SAME series (namespace c-db, schedule daily): an older and a newer success,
	// plus a failure. The collector must collapse them to one series carrying the LATEST success and
	// a failure count of 1.
	seriesLabels := map[string]string{
		apiconst.LabelOrigin:        apiconst.OriginCluster,
		apiconst.LabelSchedule:      "daily",
		apiconst.LabelNamespace:     "c-db",
		apiconst.LabelClusterBackup: "daily-old",
	}
	older := &cbv1.Backup{
		ObjectMeta: metav1.ObjectMeta{Namespace: "c-db", Name: "daily-old", Labels: seriesLabels, CreationTimestamp: created},
		Spec:       cbv1.BackupSpec{LocationRef: cbv1.LocationReference{Name: "dr"}},
		Status: cbv1.BackupStatus{
			Phase:      string(status.BackupPhaseCompleted),
			BackupTime: &metav1.Time{Time: created.Add(60 * time.Second)},
			Volumes:    []cbv1.VolumeStatus{{Pvc: "a", SizeBytes: 1, AddedBytes: 1, Phase: status.VolumePhaseCompleted}},
		},
	}
	newer := older.DeepCopy()
	newer.Name = "daily-new"
	newer.Labels = map[string]string{apiconst.LabelOrigin: apiconst.OriginCluster, apiconst.LabelSchedule: "daily", apiconst.LabelNamespace: "c-db", apiconst.LabelClusterBackup: "daily-new"}
	newer.Status.BackupTime = &backupTime
	newer.Status.Volumes = []cbv1.VolumeStatus{{Pvc: "a", SizeBytes: 100, AddedBytes: 30}, {Pvc: "b", SizeBytes: 50, AddedBytes: 20}}
	failed := &cbv1.Backup{
		ObjectMeta: metav1.ObjectMeta{Namespace: "c-db", Name: "daily-bad",
			Labels: map[string]string{apiconst.LabelOrigin: apiconst.OriginCluster, apiconst.LabelSchedule: "daily", apiconst.LabelNamespace: "c-db", apiconst.LabelClusterBackup: "daily-bad"}},
		Spec:   cbv1.BackupSpec{LocationRef: cbv1.LocationReference{Name: "dr"}},
		Status: cbv1.BackupStatus{Phase: string(status.BackupPhaseFailed)},
	}

	reg := prometheus.NewRegistry()
	reg.MustRegister(NewCollector(newFakeClient(t, loc, older, newer, failed)))

	want := map[string]string{"namespace": "c-db", "tenant": "c-db", "schedule": "daily", "origin": "cluster", "location": "dr", "cluster": "c1"}

	if got, ok := gatherValue(t, reg, "crystalbackup_backup_last_success_timestamp_seconds", want); !ok || got != float64(backupTime.Unix()) {
		t.Fatalf("last_success = %v (found=%v), want %d (the NEWER success)", got, ok, backupTime.Unix())
	}
	if got, ok := gatherValue(t, reg, "crystalbackup_backup_last_size_bytes", want); !ok || got != 150 {
		t.Fatalf("last_size_bytes = %v (found=%v), want 150", got, ok)
	}
	if got, ok := gatherValue(t, reg, "crystalbackup_backup_last_added_bytes", want); !ok || got != 50 {
		t.Fatalf("last_added_bytes = %v (found=%v), want 50", got, ok)
	}
	if got, ok := gatherValue(t, reg, "crystalbackup_backup_last_duration_seconds", want); !ok || got != 300 {
		t.Fatalf("last_duration_seconds = %v (found=%v), want 300", got, ok)
	}
	if got, ok := gatherValue(t, reg, "crystalbackup_backup_failures", want); !ok || got != 1 {
		t.Fatalf("failures = %v (found=%v), want 1", got, ok)
	}
}

func TestCollectorBuildInfoAlwaysPresent(t *testing.T) {
	// With no CRs at all, crystalbackup_build_info must still be emitted, so /metrics always carries
	// a crystalbackup_ series (the M1 hard-assertion exit criterion).
	reg := prometheus.NewRegistry()
	reg.MustRegister(NewCollector(newFakeClient(t)))
	if got, ok := gatherValue(t, reg, "crystalbackup_build_info", map[string]string{"version": Version}); !ok || got != 1 {
		t.Fatalf("build_info = %v (found=%v), want 1 even with no backups", got, ok)
	}
}

func TestCollectorClusterBackupSeries(t *testing.T) {
	loc := &cbv1.ClusterBackupLocation{
		ObjectMeta: metav1.ObjectMeta{Name: "dr"},
		Spec:       cbv1.ClusterBackupLocationSpec{ClusterID: "c1"},
	}
	completion := metav1.Date(2026, 7, 17, 2, 10, 0, 0, time.UTC)
	run := &cbv1.ClusterBackup{
		ObjectMeta: metav1.ObjectMeta{Name: "daily-1", CreationTimestamp: metav1.Date(2026, 7, 17, 2, 0, 0, 0, time.UTC)},
		Spec:       cbv1.ClusterBackupSpec{ScheduleRef: "daily", ClusterBackupRunSpec: cbv1.ClusterBackupRunSpec{LocationRef: cbv1.LocalObjectReference{Name: "dr"}}},
		Status: cbv1.ClusterBackupStatus{
			Phase:             string(status.ClusterBackupPhaseCompleted),
			CompletionTime:    &completion,
			NamespacesMatched: 6,
			NamespacesFailed:  1,
		},
	}

	reg := prometheus.NewRegistry()
	reg.MustRegister(NewCollector(newFakeClient(t, loc, run)))

	want := map[string]string{"schedule": "daily", "location": "dr", "cluster": "c1"}
	if got, ok := gatherValue(t, reg, "crystalbackup_clusterbackup_last_success_timestamp_seconds", want); !ok || got != float64(completion.Unix()) {
		t.Fatalf("cb last_success = %v (found=%v), want %d", got, ok, completion.Unix())
	}
	if got, ok := gatherValue(t, reg, "crystalbackup_clusterbackup_namespaces_matched", want); !ok || got != 6 {
		t.Fatalf("namespaces_matched = %v (found=%v), want 6", got, ok)
	}
	if got, ok := gatherValue(t, reg, "crystalbackup_clusterbackup_namespaces_failed", want); !ok || got != 1 {
		t.Fatalf("namespaces_failed = %v (found=%v), want 1", got, ok)
	}
}
