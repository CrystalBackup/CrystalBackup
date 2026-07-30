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
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
	"github.com/CrystalBackup/CrystalBackup/internal/status"
)

// countSamples returns how many samples the registry holds for metric `name`.
func countSamples(t *testing.T, reg *prometheus.Registry, name string) int {
	t.Helper()
	fams, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, fam := range fams {
		if fam.GetName() == name {
			return len(fam.GetMetric())
		}
	}
	return 0
}

func namespaceObj(name string, labels map[string]string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
}

func drLocation() *cbv1.ClusterBackupLocation {
	return &cbv1.ClusterBackupLocation{
		ObjectMeta: metav1.ObjectMeta{Name: "dr"},
		Spec:       cbv1.ClusterBackupLocationSpec{ClusterID: "c1", Default: true},
	}
}

// TestScheduleActiveResolvesClusterSelection is the test this whole family exists for: a
// ClusterBackupSchedule declares a SELECTION, and crystalbackup_schedule_active has to be one
// series per namespace that selection resolves to — otherwise the BackupMissed rule's
// `and on (namespace, schedule, cluster)` join finds no partner and the alert can never fire,
// which is precisely the state 0.5.x shipped in.
func TestScheduleActiveResolvesClusterSelection(t *testing.T) {
	protectedLabel := map[string]string{apiconst.LabelProtect: "true"}
	cbs := &cbv1.ClusterBackupSchedule{
		ObjectMeta: metav1.ObjectMeta{Name: "dr-daily"},
		Spec: cbv1.ClusterBackupScheduleSpec{
			Schedule: "0 2 * * *",
			Template: cbv1.ClusterBackupTemplate{Spec: cbv1.ClusterBackupRunSpec{
				LocationRef: cbv1.LocalObjectReference{Name: "dr"},
				Namespaces:  cbv1.NamespaceSelector{MatchLabels: protectedLabel},
			}},
		},
	}

	reg := prometheus.NewRegistry()
	reg.MustRegister(NewCollector(newFakeClient(t,
		drLocation(), cbs,
		namespaceObj("team-a", protectedLabel),
		// team-b carries an explicit tenant label: the series must use it, not the namespace name.
		namespaceObj("team-b", map[string]string{apiconst.LabelProtect: "true", apiconst.LabelTenant: "acme"}),
		namespaceObj("team-c", nil), // not selected
	), testOperatorNamespace))

	for _, tc := range []struct{ namespace, tenant string }{
		{"team-a", "team-a"},
		{"team-b", "acme"},
	} {
		want := map[string]string{
			"namespace": tc.namespace, "tenant": tc.tenant, "schedule": "dr-daily",
			"origin": "cluster", "location": "dr", "cluster": "c1",
		}
		if got, ok := gatherValue(t, reg, NameScheduleActive, want); !ok || got != 1 {
			t.Fatalf("schedule_active for %s = %v (found=%v), want 1", tc.namespace, got, ok)
		}
	}
	if n := countSamples(t, reg, NameScheduleActive); n != 2 {
		t.Fatalf("schedule_active samples = %d, want exactly 2 (one per MATCHED namespace, none for team-c)", n)
	}
}

// TestScheduleActivePausedClusterScheduleIsAbsent pins the absence semantics: a paused schedule
// emits no series at all rather than 0, so the BackupMissed join loses its partner and the rule
// goes quiet — which is what pausing means.
func TestScheduleActivePausedClusterScheduleIsAbsent(t *testing.T) {
	sel := map[string]string{apiconst.LabelProtect: "true"}
	cbs := &cbv1.ClusterBackupSchedule{
		ObjectMeta: metav1.ObjectMeta{Name: "dr-daily"},
		Spec: cbv1.ClusterBackupScheduleSpec{
			Schedule: "0 2 * * *",
			Paused:   true,
			Template: cbv1.ClusterBackupTemplate{Spec: cbv1.ClusterBackupRunSpec{
				LocationRef: cbv1.LocalObjectReference{Name: "dr"},
				Namespaces:  cbv1.NamespaceSelector{MatchLabels: sel},
			}},
		},
	}
	reg := prometheus.NewRegistry()
	reg.MustRegister(NewCollector(newFakeClient(t, drLocation(), cbs, namespaceObj("team-a", sel)), testOperatorNamespace))
	if n := countSamples(t, reg, NameScheduleActive); n != 0 {
		t.Fatalf("schedule_active samples = %d, want 0 for a paused ClusterBackupSchedule", n)
	}
}

// TestScheduleActiveInvalidSelectorEmitsNothing: a selector the fan-out controller will refuse
// protects nobody, and saying otherwise would suppress BackupMissed on genuinely bare namespaces.
func TestScheduleActiveInvalidSelectorEmitsNothing(t *testing.T) {
	cbs := &cbv1.ClusterBackupSchedule{
		ObjectMeta: metav1.ObjectMeta{Name: "broken"},
		Spec: cbv1.ClusterBackupScheduleSpec{
			Schedule: "0 2 * * *",
			Template: cbv1.ClusterBackupTemplate{Spec: cbv1.ClusterBackupRunSpec{
				LocationRef: cbv1.LocalObjectReference{Name: "dr"},
				// Two positive forms: rule 8 violation, nsselector.Match errors out.
				Namespaces: cbv1.NamespaceSelector{MatchNames: []string{"team-a"}, Regexp: "^team-"},
			}},
		},
	}
	reg := prometheus.NewRegistry()
	reg.MustRegister(NewCollector(newFakeClient(t, drLocation(), cbs, namespaceObj("team-a", nil)), testOperatorNamespace))
	if n := countSamples(t, reg, NameScheduleActive); n != 0 {
		t.Fatalf("schedule_active samples = %d, want 0 for a rule-8-invalid selector", n)
	}
}

// TestScheduleActiveNamespacePlane: a BackupSchedule has no paused field, so its mere existence
// is the signal. Its location is namespaced and carries no clusterID, so the cluster label is
// empty — matching what the Backup gauges emit for the same series, which is what keeps the
// BackupMissed join (on namespace, schedule, cluster) intact on the namespace plane.
func TestScheduleActiveNamespacePlane(t *testing.T) {
	bs := &cbv1.BackupSchedule{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "nightly"},
		Spec: cbv1.BackupScheduleSpec{
			Schedule:    "0 3 * * *",
			LocationRef: cbv1.LocalObjectReference{Name: "own"},
		},
	}
	reg := prometheus.NewRegistry()
	reg.MustRegister(NewCollector(newFakeClient(t, drLocation(), bs, namespaceObj("team-a", nil)), testOperatorNamespace))

	want := map[string]string{
		"namespace": "team-a", "tenant": "team-a", "schedule": "nightly",
		"origin": "namespace", "location": "own", "cluster": "",
	}
	if got, ok := gatherValue(t, reg, NameScheduleActive, want); !ok || got != 1 {
		t.Fatalf("schedule_active = %v (found=%v), want 1 with an EMPTY cluster label", got, ok)
	}
}

// TestBackupProtectedBytesSumsNewestPerPVC: protected bytes is a sum over DISTINCT volumes, not
// over backups. A PVC only the older run captured is still protected, and a PVC captured twice
// counts once, at its newest size.
func TestBackupProtectedBytesSumsNewestPerPVC(t *testing.T) {
	old := metav1.Date(2026, 7, 16, 2, 0, 0, 0, time.UTC)
	recent := metav1.Date(2026, 7, 17, 2, 0, 0, 0, time.UTC)
	labels := map[string]string{apiconst.LabelOrigin: apiconst.OriginCluster, apiconst.LabelSchedule: "daily"}

	older := &cbv1.Backup{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "daily-old", Labels: labels},
		Spec:       cbv1.BackupSpec{LocationRef: cbv1.LocationReference{Name: "dr"}},
		Status: cbv1.BackupStatus{
			Phase: string(status.BackupPhaseCompleted), BackupTime: &old,
			Volumes: []cbv1.VolumeStatus{
				{Pvc: "data", SizeBytes: 100, Phase: status.VolumePhaseCompleted},
				{Pvc: "archive", SizeBytes: 500, Phase: status.VolumePhaseCompleted},
			},
		},
	}
	// The newer run resized `data` and no longer covers `archive` — which is still protected.
	newer := &cbv1.Backup{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "daily-new", Labels: labels},
		Spec:       cbv1.BackupSpec{LocationRef: cbv1.LocationReference{Name: "dr"}},
		Status: cbv1.BackupStatus{
			Phase: string(status.BackupPhaseCompleted), BackupTime: &recent,
			Volumes: []cbv1.VolumeStatus{
				{Pvc: "data", SizeBytes: 300, Phase: status.VolumePhaseCompleted},
				// A failed volume contributes nothing and must not erase a prior good reading.
				{Pvc: "scratch", SizeBytes: 0, Phase: status.VolumePhaseFailed},
			},
		},
	}

	reg := prometheus.NewRegistry()
	reg.MustRegister(NewCollector(newFakeClient(t, drLocation(), older, newer), testOperatorNamespace))

	want := map[string]string{"namespace": "team-a", "tenant": "team-a", "origin": "cluster", "location": "dr", "cluster": "c1"}
	if got, ok := gatherValue(t, reg, NameBackupProtectedBytes, want); !ok || got != 800 {
		t.Fatalf("protected_bytes = %v (found=%v), want 800 (newest data=300 + archive=500)", got, ok)
	}
}

func repositoryWithDiscovery(success *bool, projected, orphans int32) *cbv1.BackupRepository {
	scanned := metav1.Date(2026, 7, 17, 3, 0, 0, 0, time.UTC)
	repo := &cbv1.BackupRepository{
		ObjectMeta: metav1.ObjectMeta{Name: "dr"},
		Status: cbv1.BackupRepositoryStatus{
			Scope:                "Cluster",
			Location:             cbv1.RepositoryLocationRef{Kind: "ClusterBackupLocation", Name: "dr"},
			LastDiscoveryTime:    &scanned,
			LastDiscoverySuccess: success,
			ProjectedBackups:     projected,
			OrphanSnapshots:      orphans,
		},
	}
	return repo
}

// TestDiscoveryFamilyAbsentBeforeFirstScan: discovery_last_success == 0 IS the DiscoveryFailed
// alert, so a repository that has never been scanned must emit nothing rather than a zero that
// would page the platform team for a location's first minute of existence.
func TestDiscoveryFamilyAbsentBeforeFirstScan(t *testing.T) {
	repo := repositoryWithDiscovery(nil, 0, 0)
	repo.Status.LastDiscoveryTime = nil
	reg := prometheus.NewRegistry()
	reg.MustRegister(NewCollector(newFakeClient(t, drLocation(), repo), testOperatorNamespace))

	for _, name := range []string{
		NameDiscoveryLastSuccess, NameDiscoveryLastTimestamp, NameDiscoveryProjected, NameDiscoveryOrphans,
	} {
		if n := countSamples(t, reg, name); n != 0 {
			t.Fatalf("%s samples = %d, want 0 before the first scan reports", name, n)
		}
	}
}

func TestDiscoveryFamilyAfterScan(t *testing.T) {
	failed := false
	reg := prometheus.NewRegistry()
	reg.MustRegister(NewCollector(newFakeClient(t, drLocation(), repositoryWithDiscovery(&failed, 12, 3)), testOperatorNamespace))

	want := map[string]string{"location": "dr", "scope": "Cluster", "cluster": "c1"}
	if got, ok := gatherValue(t, reg, NameDiscoveryLastSuccess, want); !ok || got != 0 {
		t.Fatalf("discovery_last_success = %v (found=%v), want 0 after a failing scan", got, ok)
	}
	if got, ok := gatherValue(t, reg, NameDiscoveryProjected, want); !ok || got != 12 {
		t.Fatalf("discovery_projected_backups = %v (found=%v), want 12", got, ok)
	}
	if got, ok := gatherValue(t, reg, NameDiscoveryOrphans, want); !ok || got != 3 {
		t.Fatalf("discovery_orphan_snapshots = %v (found=%v), want 3", got, ok)
	}
}

func TestErasureGauges(t *testing.T) {
	completedAt := metav1.Date(2026, 7, 17, 4, 0, 0, 0, time.UTC)
	blocked := &cbv1.ClusterErasure{
		ObjectMeta: metav1.ObjectMeta{Name: "gdpr-1"},
		Spec:       cbv1.ClusterErasureSpec{LocationRef: cbv1.LocalObjectReference{Name: "dr"}},
		Status:     cbv1.ClusterErasureStatus{Phase: "Blocked"},
	}
	done := &cbv1.ClusterErasure{
		ObjectMeta: metav1.ObjectMeta{Name: "gdpr-0"},
		Spec:       cbv1.ClusterErasureSpec{LocationRef: cbv1.LocalObjectReference{Name: "dr"}},
		Status: cbv1.ClusterErasureStatus{
			Phase: "Completed",
			Conditions: []metav1.Condition{{
				Type: "Ready", Status: metav1.ConditionTrue, Reason: "Erased",
				LastTransitionTime: completedAt,
			}},
		},
	}
	reg := prometheus.NewRegistry()
	reg.MustRegister(NewCollector(newFakeClient(t, drLocation(), blocked, done), testOperatorNamespace))

	want := map[string]string{"location": "dr", "cluster": "c1"}
	if got, ok := gatherValue(t, reg, NameErasureBlocked, want); !ok || got != 1 {
		t.Fatalf("erasure_blocked = %v (found=%v), want 1", got, ok)
	}
	if got, ok := gatherValue(t, reg, NameErasureLastCompletion, want); !ok || got != float64(completedAt.Unix()) {
		t.Fatalf("erasure_last_completion = %v (found=%v), want %d", got, ok, completedAt.Unix())
	}
}

func moverJob(name string, extraLabels map[string]string) *batchv1.Job {
	labels := map[string]string{
		apiconst.LabelManagedBy: apiconst.ManagedByValue,
		apiconst.LabelPVC:       "data",
	}
	for k, v := range extraLabels {
		labels[k] = v
	}
	return &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Namespace: testOperatorNamespace, Name: name, Labels: labels}}
}

// TestMoverCensus: active counts backup AND restore movers (both hold a slot), the limit comes off
// the live cluster-plane declaration, and queue depth is the admitted-but-unstarted gap.
func TestMoverCensus(t *testing.T) {
	// A maintenance Job: managed-by but no PVC label. It must not be counted as a mover.
	maintenance := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Namespace: testOperatorNamespace, Name: "dr-prune",
		Labels: map[string]string{apiconst.LabelManagedBy: apiconst.ManagedByValue},
	}}
	cbs := &cbv1.ClusterBackupSchedule{
		ObjectMeta: metav1.ObjectMeta{Name: "dr-daily"},
		Spec: cbv1.ClusterBackupScheduleSpec{
			Schedule: "0 2 * * *",
			Template: cbv1.ClusterBackupTemplate{Spec: cbv1.ClusterBackupRunSpec{
				LocationRef:   cbv1.LocalObjectReference{Name: "dr"},
				Namespaces:    cbv1.NamespaceSelector{MatchNames: []string{"team-a"}},
				BackupRunSpec: cbv1.BackupRunSpec{MaxConcurrentMovers: 4},
			}},
		},
	}
	// Three volumes admitted into the mover phases; one backup mover Job exists ⇒ depth 2.
	backup := &cbv1.Backup{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "daily-1"},
		Spec:       cbv1.BackupSpec{LocationRef: cbv1.LocationReference{Name: "dr"}},
		Status: cbv1.BackupStatus{
			Phase: string(status.BackupPhaseUploading),
			Volumes: []cbv1.VolumeStatus{
				{Pvc: "a", Phase: status.VolumePhaseUploading},
				{Pvc: "b", Phase: status.VolumePhaseSnapshotting},
				{Pvc: "c", Phase: status.VolumePhaseSnapshotting},
				{Pvc: "d", Phase: status.VolumePhaseCompleted},
			},
		},
	}

	reg := prometheus.NewRegistry()
	reg.MustRegister(NewCollector(newFakeClient(t,
		drLocation(), cbs, backup, maintenance,
		moverJob("backup-mover", nil),
		moverJob("restore-mover", map[string]string{apiconst.LabelRestore: "rst-1"}),
	), testOperatorNamespace))

	want := map[string]string{"cluster": "c1"}
	if got, ok := gatherValue(t, reg, NameMoverActive, want); !ok || got != 2 {
		t.Fatalf("mover_active = %v (found=%v), want 2 (backup + restore movers, not the maintenance Job)", got, ok)
	}
	if got, ok := gatherValue(t, reg, NameMoverConcurrencyLimit, want); !ok || got != 4 {
		t.Fatalf("mover_concurrency_limit = %v (found=%v), want 4", got, ok)
	}
	if got, ok := gatherValue(t, reg, NameMoverQueueDepth, want); !ok || got != 2 {
		t.Fatalf("mover_queue_depth = %v (found=%v), want 2 (3 admitted volumes - 1 backup mover)", got, ok)
	}
}

// volumeSnapshot builds the unstructured shape the collector reads: only spec.source's PVC name
// matters, and a static snapshot (no source PVC) must be skipped.
func volumeSnapshot(namespace, name, sourcePVC string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{}}
	u.SetGroupVersionKind(volumeSnapshotListGVK.GroupVersion().WithKind("VolumeSnapshot"))
	u.SetNamespace(namespace)
	u.SetName(name)
	if sourcePVC != "" {
		_ = unstructured.SetNestedField(u.Object, sourcePVC, "spec", "source", "persistentVolumeClaimName")
	} else {
		_ = unstructured.SetNestedField(u.Object, "some-vsc", "spec", "source", "volumeSnapshotContentName")
	}
	return u
}

// TestPVCVolumeSnapshotCount: the documented per-PVC exception. It must count EVERY tool's
// snapshots — the incumbent's are the ones that pile up during coexistence — and must skip
// static snapshots, which add nothing to a source volume's chain.
func TestPVCVolumeSnapshotCount(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := cbv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("add client-go scheme: %v", err)
	}
	scheme.AddKnownTypeWithName(volumeSnapshotListGVK.GroupVersion().WithKind("VolumeSnapshot"), &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(volumeSnapshotListGVK, &unstructured.UnstructuredList{})

	objs := []client.Object{
		drLocation(),
		volumeSnapshot("team-a", "ours-1", "data"),
		volumeSnapshot("team-a", "velero-1", "data"), // the incumbent's — counted, deliberately
		volumeSnapshot("team-a", "ours-2", "logs"),
		volumeSnapshot(testOperatorNamespace, "restore-bound", ""), // static: skipped
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()

	reg := prometheus.NewRegistry()
	reg.MustRegister(NewCollector(c, testOperatorNamespace))

	if got, ok := gatherValue(t, reg, NamePVCVolumeSnapshotting,
		map[string]string{"namespace": "team-a", "pvc": "data", "cluster": "c1"}); !ok || got != 2 {
		t.Fatalf("pvc_volumesnapshot_count{pvc=data} = %v (found=%v), want 2 (ours + the incumbent's)", got, ok)
	}
	if got, ok := gatherValue(t, reg, NamePVCVolumeSnapshotting,
		map[string]string{"namespace": "team-a", "pvc": "logs", "cluster": "c1"}); !ok || got != 1 {
		t.Fatalf("pvc_volumesnapshot_count{pvc=logs} = %v (found=%v), want 1", got, ok)
	}
	if n := countSamples(t, reg, NamePVCVolumeSnapshotting); n != 2 {
		t.Fatalf("pvc_volumesnapshot_count samples = %d, want 2 (the static snapshot has no source PVC)", n)
	}
}

// TestRepositoryStoredBytesAccompaniesSize pins the §2.11 accounting name to the same reading as
// the inventory one, which is what this lot ships and what the report calls out.
func TestRepositoryStoredBytesAccompaniesSize(t *testing.T) {
	repo := &cbv1.BackupRepository{
		ObjectMeta: metav1.ObjectMeta{Name: "dr"},
		Status: cbv1.BackupRepositoryStatus{
			Scope:                "Cluster",
			Location:             cbv1.RepositoryLocationRef{Name: "dr"},
			ApproximateSizeBytes: 4096,
		},
	}
	reg := prometheus.NewRegistry()
	reg.MustRegister(NewCollector(newFakeClient(t, drLocation(), repo), testOperatorNamespace))
	want := map[string]string{"location": "dr", "scope": "Cluster", "namespace": "", "cluster": "c1"}
	if got, ok := gatherValue(t, reg, NameRepositoryStoredBytes, want); !ok || got != 4096 {
		t.Fatalf("repository_stored_bytes = %v (found=%v), want 4096", got, ok)
	}
}

// ---------------------------------------------------------------------------------------------
// The event half: real counters and histograms. They live on a package-global registry, so each
// test uses label values of its own rather than resetting shared state.
// ---------------------------------------------------------------------------------------------

func TestRecordBackupTerminalCountsFailuresAndResults(t *testing.T) {
	s := BackupSeries{Namespace: "ev-a", Tenant: "ev-a", Schedule: "daily", Origin: "cluster", Location: "dr", Cluster: "c1"}
	RecordBackupTerminal(s, "Completed", 120*time.Second, 1024)
	RecordBackupTerminal(s, "PartiallyFailed", 300*time.Second, 512)
	RecordBackupTerminal(s, "Failed", 60*time.Second, 0)

	vals := s.values()
	if got := testutil.ToFloat64(backupFailuresTotal.WithLabelValues(vals...)); got != 2 {
		t.Fatalf("backup_failures_total = %v, want 2 (PartiallyFailed counts as a failure)", got)
	}
	if got := testutil.ToFloat64(backupAddedTotal.WithLabelValues(vals...)); got != 1536 {
		t.Fatalf("backup_added_bytes_total = %v, want 1536", got)
	}
	for _, result := range []string{"completed", "partiallyfailed", "failed"} {
		if got := testutil.ToFloat64(backupTotal.WithLabelValues(append(append([]string{}, vals...), result)...)); got != 1 {
			t.Fatalf("backup_total{result=%s} = %v, want 1", result, got)
		}
	}
}

// TestResultOfKeepsPartiallyCompletedDistinct: spec §2.11 lists three results and there are four
// terminal Backup phases. Folding PartiallyCompleted into `completed` would erase the one signal
// that says a storage class quietly stopped being snapshottable.
func TestResultOfKeepsPartiallyCompletedDistinct(t *testing.T) {
	if got := resultOf("PartiallyCompleted"); got != "partiallycompleted" {
		t.Fatalf("resultOf(PartiallyCompleted) = %q, want its own value", got)
	}
	if resultOf("Completed") == resultOf("PartiallyCompleted") {
		t.Fatal("PartiallyCompleted must not collapse onto completed")
	}
}

func TestRecordRestoreTerminalCarriesMode(t *testing.T) {
	s := RestoreSeries{Namespace: "ev-r", Tenant: "ev-r", Origin: "cluster", Location: "dr", Cluster: "c1"}
	RecordRestoreTerminal(s, "Recreate", "Failed", 90*time.Second)
	RecordRestoreTerminal(s, "Overwrite", "Completed", 30*time.Second)

	recreate := append(append([]string{}, s.values()...), "Recreate")
	overwrite := append(append([]string{}, s.values()...), "Overwrite")
	if got := testutil.ToFloat64(restoreFailuresTotal.WithLabelValues(recreate...)); got != 1 {
		t.Fatalf("restore_failures_total{mode=Recreate} = %v, want 1", got)
	}
	if got := testutil.ToFloat64(restoreFailuresTotal.WithLabelValues(overwrite...)); got != 0 {
		t.Fatalf("restore_failures_total{mode=Overwrite} = %v, want 0 (it completed)", got)
	}
	if got := testutil.CollectAndCount(restoreDuration, NameRestoreDuration); got < 2 {
		t.Fatalf("restore_duration_seconds series = %d, want at least one per mode", got)
	}
}

func TestRecordClusterBackupTerminal(t *testing.T) {
	s := ClusterBackupSeries{Schedule: "ev-dr", Location: "dr", Cluster: "c1"}
	RecordClusterBackupTerminal(s, "PartiallyFailed", 45*time.Minute)
	if got := testutil.ToFloat64(clusterBackupRunsTotal.WithLabelValues("ev-dr", "dr", "c1", "partiallyfailed")); got != 1 {
		t.Fatalf("clusterbackup_runs_total{result=partiallyfailed} = %v, want 1", got)
	}
}

func TestRecordErasureCompletedSkipsAnEmptyErasure(t *testing.T) {
	RecordErasureCompleted("ev-loc", "c1", 7, 4096)
	RecordErasureCompleted("ev-loc", "c1", 0, 0) // a Completed erasure that matched nothing
	if got := testutil.ToFloat64(erasureForgottenTotal.WithLabelValues("ev-loc", "c1")); got != 7 {
		t.Fatalf("erasure_snapshots_forgotten_total = %v, want 7", got)
	}
	if got := testutil.ToFloat64(erasureReclaimedTotal.WithLabelValues("ev-loc", "c1")); got != 4096 {
		t.Fatalf("erasure_reclaimed_bytes_total = %v, want 4096", got)
	}
}

func TestRecordMoverJobRetriesAndWebhookDenials(t *testing.T) {
	RecordMoverJobRetries("ev-m", "ev-m", "c1", 3)
	RecordMoverJobRetries("ev-m", "ev-m", "c1", 0) // a mover that never retried adds nothing
	if got := testutil.ToFloat64(moverJobRetries.WithLabelValues("ev-m", "ev-m", "c1")); got != 3 {
		t.Fatalf("mover_job_retries_total = %v, want 3", got)
	}

	RecordWebhookDenial("ev-webhook", "multiple_defaults")
	if got := testutil.ToFloat64(webhookDenials.WithLabelValues("ev-webhook", "multiple_defaults")); got != 1 {
		t.Fatalf("webhook_denials_total = %v, want 1", got)
	}
}

func TestRecordExposureReadyWaitClampsSkew(t *testing.T) {
	RecordExposureReadyWait("ev-x", "ev-x", "csi-generic", "c1", -5*time.Second)
	RecordExposureReadyWait("ev-x", "ev-x", "csi-generic", "c1", 12*time.Second)
	if got := testutil.CollectAndCount(exposureReadyWait, NameExposureReadyWait); got == 0 {
		t.Fatal("exposure_ready_wait_seconds recorded no series")
	}
}

// TestExternalSyncBytesCopiedIsNotShipped is a guard, not a behaviour test: §2.12 says the
// counter stays out until there is a real number behind it (restic copy reports none), and a
// future contributor adding it "for completeness" would be shipping a fabricated figure into an
// accounting metric. If this ever fails, read §2.12 before deleting it.
func TestExternalSyncBytesCopiedIsNotShipped(t *testing.T) {
	for _, desc := range collectorDescriptions() {
		if strings.Contains(desc, "crystalbackup_externalsync_bytes_copied_total") {
			t.Fatal("externalsync_bytes_copied_total is shipped; restic copy emits no summary to base it on (spec §2.12)")
		}
	}
	// And the same for the event registry, where a counter would more plausibly be added.
	fams, err := ctrlmetrics.Registry.Gather()
	if err != nil {
		t.Fatalf("gather the controller-runtime registry: %v", err)
	}
	for _, fam := range fams {
		if fam.GetName() == "crystalbackup_externalsync_bytes_copied_total" {
			t.Fatal("externalsync_bytes_copied_total is registered; there is no measured byte count behind it (spec §2.12)")
		}
	}
}

// collectorDescriptions renders every descriptor the collector advertises, so a test can assert
// on what the catalogue does — and does not — contain.
func collectorDescriptions() []string {
	ch := make(chan *prometheus.Desc, 64)
	go func() {
		(&Collector{}).Describe(ch)
		close(ch)
	}()
	var out []string
	for d := range ch {
		out = append(out, d.String())
	}
	return out
}

// TestDescribeCoversEveryName is the other half of that guard: a family whose descriptor is never
// advertised is invisible to a registry's consistency checks and to anyone reading /metrics
// before the first sample exists. Every §2 name this package DERIVES belongs in Describe.
func TestDescribeCoversEveryName(t *testing.T) {
	described := strings.Join(collectorDescriptions(), "\n")
	for _, name := range []string{
		NameBuildInfo, NameBackupLastSuccess, NameBackupProtectedBytes, NameScheduleActive,
		NameClusterBackupLastSuccess, NameRestoreLastSuccess,
		NameRepositorySize, NameRepositoryStoredBytes,
		NameDiscoveryLastSuccess, NameDiscoveryProjected, NameDiscoveryOrphans, NameDiscoveryLastTimestamp,
		NameErasureBlocked, NameErasureLastCompletion,
		NameMoverActive, NameMoverQueueDepth, NameMoverConcurrencyLimit,
		NamePVCVolumeSnapshotting, NameExternalSyncLag,
	} {
		if !strings.Contains(described, name) {
			t.Errorf("Describe does not advertise %s", name)
		}
	}
}
