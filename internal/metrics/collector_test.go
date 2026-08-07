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
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
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

// testOperatorNamespace stands in for the operator's own namespace, where the mover census looks
// for Jobs.
const testOperatorNamespace = "crystal-backup-system"

func newFakeClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := cbv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	// corev1 (namespaces, for the schedule selection) and batchv1 (mover Jobs, for the
	// concurrency census) are as much a part of the collector's inputs as the CRDs are.
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("add client-go scheme: %v", err)
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
	reg.MustRegister(NewCollector(newFakeClient(t, loc, older, newer, failed), testOperatorNamespace))

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

// TestCollectorBackupLastFailure covers the series that exists because the failure COUNTER cannot
// survive the operator process that owns it.
//
// Three properties, and each of them is load-bearing for CrystalbackupBackupFailed:
//
//   - the newest failure wins, mirroring last_success — the alert asks when this series last broke,
//     not when it first did;
//   - status.completionTime is preferred, and creationTimestamp is the fallback for the objects
//     that cannot have one (terminal before the field existed, and discovery projections). The
//     fallback reads EARLY, never late, which is the safe direction for a recency test;
//   - a series that has never failed emits NOTHING. That is what keeps the alert silent on a
//     healthy install: publishing a 0 would put the Unix epoch on the wire, and the rule's
//     `time() - last_failure < 3600` would then be comparing against 1970 for every namespace in
//     the fleet.
func TestCollectorBackupLastFailure(t *testing.T) {
	loc := &cbv1.ClusterBackupLocation{
		ObjectMeta: metav1.ObjectMeta{Name: "dr"},
		Spec:       cbv1.ClusterBackupLocationSpec{ClusterID: "c1"},
	}
	older := metav1.Date(2026, 7, 17, 1, 0, 0, 0, time.UTC)
	newer := metav1.Date(2026, 7, 17, 3, 0, 0, 0, time.UTC)
	// A run whose completionTime is absent: only its creation is available, and it is deliberately
	// EARLIER than the newest failure so that preferring it would be visible as a wrong answer.
	projected := metav1.Date(2026, 7, 17, 2, 0, 0, 0, time.UTC)

	failedLabels := func(name string) map[string]string {
		return map[string]string{
			apiconst.LabelOrigin: apiconst.OriginNamespace, apiconst.LabelSchedule: "nightly",
			apiconst.LabelNamespace: "team-a", apiconst.LabelClusterBackup: name,
		}
	}
	mk := func(name string, phase status.BackupPhase, created metav1.Time, completed *metav1.Time) *cbv1.Backup {
		return &cbv1.Backup{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "team-a", Name: name, Labels: failedLabels(name), CreationTimestamp: created,
			},
			Spec:   cbv1.BackupSpec{LocationRef: cbv1.LocationReference{Name: "dr"}},
			Status: cbv1.BackupStatus{Phase: string(phase), CompletionTime: completed},
		}
	}

	reg := prometheus.NewRegistry()
	reg.MustRegister(NewCollector(newFakeClient(t, loc,
		mk("old-fail", status.BackupPhaseFailed, older, &older),
		mk("new-fail", status.BackupPhasePartiallyFailed, older, &newer),
		mk("no-completion", status.BackupPhaseFailed, projected, nil),
	), testOperatorNamespace))

	want := map[string]string{"namespace": "team-a", "tenant": "team-a", "schedule": "nightly", "origin": "namespace", "location": "dr", "cluster": "c1"}
	if got, ok := gatherValue(t, reg, "crystalbackup_backup_last_failure_timestamp_seconds", want); !ok || got != float64(newer.Unix()) {
		t.Fatalf("last_failure = %v (found=%v), want %d (the NEWEST failure, from status.completionTime)",
			got, ok, newer.Unix())
	}

	// The same three objects, minus the two that carry a completionTime: what is left must fall
	// back to the object's creation rather than contributing nothing.
	fallbackReg := prometheus.NewRegistry()
	fallbackReg.MustRegister(NewCollector(newFakeClient(t, loc,
		mk("no-completion", status.BackupPhaseFailed, projected, nil),
	), testOperatorNamespace))
	if got, ok := gatherValue(t, fallbackReg, "crystalbackup_backup_last_failure_timestamp_seconds", want); !ok || got != float64(projected.Unix()) {
		t.Fatalf("last_failure with no completionTime = %v (found=%v), want the creation timestamp %d — "+
			"a failed Backup that predates the field must still be visible to the alert", got, ok, projected.Unix())
	}

	// The healthy series: one Completed Backup, and NO last_failure series at all.
	healthy := mk("good", status.BackupPhaseCompleted, older, &newer)
	healthy.Status.BackupTime = &newer
	healthyReg := prometheus.NewRegistry()
	healthyReg.MustRegister(NewCollector(newFakeClient(t, loc, healthy), testOperatorNamespace))
	if got, ok := gatherValue(t, healthyReg, "crystalbackup_backup_last_failure_timestamp_seconds", want); ok {
		t.Fatalf("last_failure = %v was emitted for a series that has never failed; absence is the "+
			"contract, and a 0 here would make time() - last_failure read as fifty-four years", got)
	}
}

// TestCollectorBackupInProgressSince is the collector half of CrystalbackupBackupStalled. The
// promtool tests prove the RULE fires correctly given the series; only this proves the series is
// the one the rule was written against, and the three properties below are each a way for the
// alert to be silently useless:
//
//   - the OLDEST unfinished run wins. Take the newest and every nightly cascade resets the clock on
//     last night's hang, forever — the exact silence the rule exists to break;
//   - a series with nothing in flight emits NOTHING. Emit a 0 and `time() - 0` is fifty-four years,
//     which pages every namespace in the fleet permanently and gets the rule deleted by lunchtime;
//   - a discovery PROJECTION never counts. It is a materialised view of snapshots already in the
//     repository, never executed by anything, and it carries no phase — so without the exclusion it
//     would sit "unfinished" forever and page about a run that never ran.
func TestCollectorBackupInProgressSince(t *testing.T) {
	loc := &cbv1.ClusterBackupLocation{
		ObjectMeta: metav1.ObjectMeta{Name: "dr"},
		Spec:       cbv1.ClusterBackupLocationSpec{ClusterID: "c1"},
	}
	wedged := metav1.Date(2026, 7, 17, 2, 0, 0, 0, time.UTC)
	tonight := metav1.Date(2026, 7, 18, 2, 0, 0, 0, time.UTC)

	seriesLabels := map[string]string{
		apiconst.LabelOrigin: apiconst.OriginNamespace, apiconst.LabelSchedule: "nightly",
		apiconst.LabelNamespace: "team-a",
	}
	mk := func(name string, phase status.BackupPhase, created metav1.Time) *cbv1.Backup {
		labels := map[string]string{}
		for k, v := range seriesLabels {
			labels[k] = v
		}
		labels[apiconst.LabelClusterBackup] = name
		return &cbv1.Backup{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "team-a", Name: name, Labels: labels, CreationTimestamp: created,
			},
			Spec:   cbv1.BackupSpec{LocationRef: cbv1.LocationReference{Name: "dr"}},
			Status: cbv1.BackupStatus{Phase: string(phase)},
		}
	}
	want := map[string]string{"namespace": "team-a", "tenant": "team-a", "schedule": "nightly", "origin": "namespace", "location": "dr", "cluster": "c1"}

	// Last night's run wedged in Uploading; tonight's has just started. One series, and it must
	// report the older instant.
	reg := prometheus.NewRegistry()
	reg.MustRegister(NewCollector(newFakeClient(t, loc,
		mk("wedged", status.BackupPhaseUploading, wedged),
		mk("tonight", status.BackupPhaseSnapshotting, tonight),
	), testOperatorNamespace))
	if got, ok := gatherValue(t, reg, "crystalbackup_backup_in_progress_since_timestamp_seconds", want); !ok || got != float64(wedged.Unix()) {
		t.Fatalf("in_progress_since = %v (found=%v), want the OLDEST unfinished run's creation %d — "+
			"taking the newest lets tonight's cascade reset the clock on last night's hang",
			got, ok, wedged.Unix())
	}

	// Every run finished: no series at all.
	doneReg := prometheus.NewRegistry()
	doneReg.MustRegister(NewCollector(newFakeClient(t, loc,
		mk("done", status.BackupPhaseCompleted, wedged),
		mk("also-done", status.BackupPhasePartiallyFailed, tonight),
	), testOperatorNamespace))
	if got, ok := gatherValue(t, doneReg, "crystalbackup_backup_in_progress_since_timestamp_seconds", want); ok {
		t.Fatalf("in_progress_since = %v was emitted for a series with nothing in flight; absence is "+
			"the contract, and anything here pages the whole fleet forever", got)
	}

	// A projection, which carries no phase and is never executed.
	projection := mk("projected", "", wedged)
	projection.Annotations = map[string]string{
		apiconst.AnnotationProjected: apiconst.AnnotationProjectedValue,
	}
	projReg := prometheus.NewRegistry()
	projReg.MustRegister(NewCollector(newFakeClient(t, loc, projection), testOperatorNamespace))
	if got, ok := gatherValue(t, projReg, "crystalbackup_backup_in_progress_since_timestamp_seconds", want); ok {
		t.Fatalf("in_progress_since = %v was emitted for a discovery projection; it is a view of "+
			"snapshots that already exist, so it is neither running nor stalled", got)
	}
}

func TestCollectorBuildInfoAlwaysPresent(t *testing.T) {
	// With no CRs at all, crystalbackup_build_info must still be emitted, so /metrics always carries
	// a crystalbackup_ series (the M1 hard-assertion exit criterion).
	reg := prometheus.NewRegistry()
	reg.MustRegister(NewCollector(newFakeClient(t), testOperatorNamespace))
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
	reg.MustRegister(NewCollector(newFakeClient(t, loc, run), testOperatorNamespace))

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
