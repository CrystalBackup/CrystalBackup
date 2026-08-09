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
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
	"github.com/CrystalBackup/CrystalBackup/internal/status"
)

// TestRestoreSeries proves the UNIFIED restore family (05-observability §2.3): a Completed
// namespaced Restore resolves tenant/origin/location/cluster through its source Backup, a
// failed sibling tallies into the same series' failures gauge, and a ClusterRestore is
// recorded under its SOURCE namespace with origin=cluster — one family for both kinds.
func TestRestoreSeries(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := cbv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	terminalAt := metav1.NewTime(time.Unix(1_700_000_000, 0))
	readyCond := []metav1.Condition{{
		Type: "Ready", Status: metav1.ConditionTrue, Reason: "Completed",
		LastTransitionTime: terminalAt,
	}}

	loc := &cbv1.ClusterBackupLocation{
		ObjectMeta: metav1.ObjectMeta{Name: "dr-primary"},
		Spec: cbv1.ClusterBackupLocationSpec{
			ClusterID: "prod-eu-1",
			S3:        cbv1.S3Spec{Endpoint: "e", Bucket: "b", CredentialsSecretRef: cbv1.LocalObjectReference{Name: "s"}},
			Encryption: cbv1.ClusterEncryptionSpec{
				ClusterKEKSecretRef: cbv1.LocalObjectReference{Name: "kek"},
			},
		},
	}
	sourceBackup := &cbv1.Backup{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "tenant-a", Name: "run-1",
			Labels: map[string]string{
				apiconst.LabelTenant: "team-a",
				apiconst.LabelOrigin: apiconst.OriginCluster,
			},
		},
		Spec: cbv1.BackupSpec{LocationRef: cbv1.LocationReference{Kind: "ClusterBackupLocation", Name: "dr-primary"}},
	}
	completed := &cbv1.Restore{
		ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-a", Name: "recover-1"},
		Spec:       cbv1.RestoreSpec{Source: cbv1.RestoreSource{Backup: "run-1"}},
		Status: cbv1.RestoreStatus{
			Phase: string(status.RestorePhaseCompleted), RestoredBytes: 2048, Conditions: readyCond,
		},
	}
	failed := &cbv1.Restore{
		ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-a", Name: "recover-2"},
		Spec:       cbv1.RestoreSpec{Source: cbv1.RestoreSource{Backup: "run-1"}},
		Status:     cbv1.RestoreStatus{Phase: string(status.RestorePhaseFailed), Conditions: readyCond},
	}
	clusterRestore := &cbv1.ClusterRestore{
		ObjectMeta: metav1.ObjectMeta{Name: "recover-gone"},
		Spec: cbv1.ClusterRestoreSpec{
			Source: cbv1.ClusterRestoreSource{LocationRef: cbv1.LocalObjectReference{Name: "dr-primary"}, Namespace: "gone", Backup: "run-1"},
			Target: cbv1.ClusterRestoreTarget{Namespace: "restored"},
		},
		Status: cbv1.ClusterRestoreStatus{
			Phase: string(status.RestorePhaseCompleted), RestoredBytes: 4096, Conditions: readyCond,
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(loc, sourceBackup, completed, failed, clusterRestore).Build()

	want := `
# HELP crystalbackup_restore_last_success_timestamp_seconds Unix time of the last Completed Restore/ClusterRestore for this series.
# TYPE crystalbackup_restore_last_success_timestamp_seconds gauge
crystalbackup_restore_last_success_timestamp_seconds{cluster="prod-eu-1",location="dr-primary",namespace="tenant-a",origin="cluster",tenant="team-a"} 1.7e+09
crystalbackup_restore_last_success_timestamp_seconds{cluster="prod-eu-1",location="dr-primary",namespace="gone",origin="cluster",tenant="gone"} 1.7e+09
# HELP crystalbackup_restore_last_restored_bytes status.restoredBytes of the last Completed Restore/ClusterRestore for this series.
# TYPE crystalbackup_restore_last_restored_bytes gauge
crystalbackup_restore_last_restored_bytes{cluster="prod-eu-1",location="dr-primary",namespace="tenant-a",origin="cluster",tenant="team-a"} 2048
crystalbackup_restore_last_restored_bytes{cluster="prod-eu-1",location="dr-primary",namespace="gone",origin="cluster",tenant="gone"} 4096
# HELP crystalbackup_restore_failures Number of Restores/ClusterRestores currently in a failed terminal phase (Failed or PartiallyFailed) for this series.
# TYPE crystalbackup_restore_failures gauge
crystalbackup_restore_failures{cluster="prod-eu-1",location="dr-primary",namespace="tenant-a",origin="cluster",tenant="team-a"} 1
crystalbackup_restore_failures{cluster="prod-eu-1",location="dr-primary",namespace="gone",origin="cluster",tenant="gone"} 0
`
	if err := testutil.CollectAndCompare(NewCollector(c, testOperatorNamespace), strings.NewReader(want),
		"crystalbackup_restore_last_success_timestamp_seconds",
		"crystalbackup_restore_last_restored_bytes",
		"crystalbackup_restore_failures",
	); err != nil {
		t.Fatal(err)
	}
}

// TestCollectorRestoreVolumesFailed covers the series that answers "what did not come back".
//
// Before it, restore_failures counted OBJECTS and nothing counted VOLUMES: one restore that lost a
// single volume out of nine and one that lost all nine were the same number, and the difference lived
// only in a condition message and in mover Jobs the controller deletes moments later.
//
// The assertions deliberately do not stop at "the value is 5". They re-derive the expected total from
// the restore objects' own phases and status.failedVolumes, which is the check the ClusterBackup
// incident actually needed — there, the published counters added up perfectly and were still
// describing a different set of children than the ones sitting next to them.
func TestCollectorRestoreVolumesFailed(t *testing.T) {
	loc := &cbv1.ClusterBackupLocation{
		ObjectMeta: metav1.ObjectMeta{Name: "dr"},
		Spec:       cbv1.ClusterBackupLocationSpec{ClusterID: "c1"},
	}
	// A ClusterRestore's series identity comes entirely from its spec (source namespace + location),
	// which keeps this test on one series without a source-Backup join.
	newClusterRestore := func(name, phase string, failedVolumes int32) *cbv1.ClusterRestore {
		return &cbv1.ClusterRestore{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: cbv1.ClusterRestoreSpec{
				Source: cbv1.ClusterRestoreSource{Namespace: "team-db", LocationRef: cbv1.LocalObjectReference{Name: "dr"}},
				Target: cbv1.ClusterRestoreTarget{Namespace: "team-db"},
			},
			Status: cbv1.ClusterRestoreStatus{
				Phase:           phase,
				PlannedVolumes:  9,
				RestoredVolumes: 9 - failedVolumes,
				FailedVolumes:   failedVolumes,
			},
		}
	}
	// Three restores of the same namespace: one that lost two volumes, one that lost three, and one
	// that lost one but is STILL RUNNING. The running one must not be counted — it is not one of the
	// restores restore_failures counts, and a partial failure that is still being worked on is not yet
	// an outcome. That is also why the counters on it are non-zero at all now: they move every pass.
	partial := newClusterRestore("cr-partial", string(status.RestorePhasePartiallyFailed), 2)
	failed := newClusterRestore("cr-failed", string(status.RestorePhaseFailed), 3)
	running := newClusterRestore("cr-running", string(status.RestorePhaseRunning), 1)
	// And a Completed one, which contributes zero and must not remove the series.
	done := newClusterRestore("cr-done", string(status.RestorePhaseCompleted), 0)

	reg := prometheus.NewRegistry()
	reg.MustRegister(NewCollector(newFakeClient(t, loc, partial, failed, running, done), testOperatorNamespace))

	labels := map[string]string{
		namespaceLabel: "team-db",
		tenantLabel:    "team-db",
		originLabel:    apiconst.OriginCluster,
		locationLabel:  "dr",
		clusterLabel:   "c1",
	}

	// Re-derive both expectations from the objects themselves, exactly as a reader would by hand.
	var wantVolumes, wantObjects float64
	for _, cr := range []*cbv1.ClusterRestore{partial, failed, running, done} {
		switch status.RestorePhase(cr.Status.Phase) {
		case status.RestorePhaseFailed, status.RestorePhasePartiallyFailed:
			wantObjects++
			wantVolumes += float64(cr.Status.FailedVolumes)
		}
	}
	if wantVolumes != 5 || wantObjects != 2 {
		t.Fatalf("fixture drifted: want 5 volumes over 2 objects, derived %v/%v", wantVolumes, wantObjects)
	}

	got, ok := gatherValue(t, reg, NameRestoreVolumesFailed, labels)
	if !ok {
		t.Fatalf("%s is absent for %v", NameRestoreVolumesFailed, labels)
	}
	if got != wantVolumes {
		t.Errorf("%s = %v, want %v (the failedVolumes of the restores in a failed terminal phase)",
			NameRestoreVolumesFailed, got, wantVolumes)
	}
	// The two series describe the SAME restores, and reading them side by side is the point: 5 volumes
	// lost across 2 failed restores says something neither number says alone.
	objects, ok := gatherValue(t, reg, NameRestoreFailures, labels)
	if !ok {
		t.Fatalf("%s is absent for %v", NameRestoreFailures, labels)
	}
	if objects != wantObjects {
		t.Errorf("%s = %v, want %v", NameRestoreFailures, objects, wantObjects)
	}
}

// TestCollectorRestoreVolumesFailedIsZeroNotAbsent pins the deliberate zero. A series with restore
// activity and no lost volume publishes 0, matching restore_failures beside it: absence in this
// package means "never measured", and a namespace that HAS restored is measured.
func TestCollectorRestoreVolumesFailedIsZeroNotAbsent(t *testing.T) {
	loc := &cbv1.ClusterBackupLocation{
		ObjectMeta: metav1.ObjectMeta{Name: "dr"},
		Spec:       cbv1.ClusterBackupLocationSpec{ClusterID: "c1"},
	}
	clean := &cbv1.ClusterRestore{
		ObjectMeta: metav1.ObjectMeta{Name: "cr-clean"},
		Spec: cbv1.ClusterRestoreSpec{
			Source: cbv1.ClusterRestoreSource{Namespace: "team-ok", LocationRef: cbv1.LocalObjectReference{Name: "dr"}},
			Target: cbv1.ClusterRestoreTarget{Namespace: "team-ok"},
		},
		Status: cbv1.ClusterRestoreStatus{
			Phase: string(status.RestorePhaseCompleted), PlannedVolumes: 3, RestoredVolumes: 3,
		},
	}

	reg := prometheus.NewRegistry()
	reg.MustRegister(NewCollector(newFakeClient(t, loc, clean), testOperatorNamespace))

	labels := map[string]string{
		namespaceLabel: "team-ok",
		tenantLabel:    "team-ok",
		originLabel:    apiconst.OriginCluster,
		locationLabel:  "dr",
		clusterLabel:   "c1",
	}
	got, ok := gatherValue(t, reg, NameRestoreVolumesFailed, labels)
	if !ok {
		t.Fatalf("%s is absent for a series with a completed restore; zero here is a measurement", NameRestoreVolumesFailed)
	}
	if got != 0 {
		t.Errorf("%s = %v, want 0", NameRestoreVolumesFailed, got)
	}
}
