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

	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/status"
)

// TestRestoreSeries proves the restore families are state-derived: a Completed Restore
// yields last-success + restored-bytes with the cluster label joined through its source
// Backup's location, failed restores tally into the failures gauge, and a ClusterRestore
// series carries its target namespace + location + cluster.
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
		ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-a", Name: "run-1"},
		Spec:       cbv1.BackupSpec{LocationRef: cbv1.LocationReference{Kind: "ClusterBackupLocation", Name: "dr-primary"}},
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
		Status: cbv1.ClusterRestoreStatus{Phase: string(status.RestorePhaseCompleted), Conditions: readyCond},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(loc, sourceBackup, completed, failed, clusterRestore).Build()

	want := `
# HELP crystalbackup_restore_last_success_timestamp_seconds Unix time the last Completed Restore in this namespace reached its terminal phase.
# TYPE crystalbackup_restore_last_success_timestamp_seconds gauge
crystalbackup_restore_last_success_timestamp_seconds{cluster="prod-eu-1",namespace="tenant-a"} 1.7e+09
# HELP crystalbackup_restore_last_restored_bytes Bytes the last Completed Restore in this namespace wrote (status.restoredBytes).
# TYPE crystalbackup_restore_last_restored_bytes gauge
crystalbackup_restore_last_restored_bytes{cluster="prod-eu-1",namespace="tenant-a"} 2048
# HELP crystalbackup_restore_failures Number of Restores currently in a failed terminal phase (Failed or PartiallyFailed) for this series.
# TYPE crystalbackup_restore_failures gauge
crystalbackup_restore_failures{cluster="prod-eu-1",namespace="tenant-a"} 1
# HELP crystalbackup_clusterrestore_last_success_timestamp_seconds Unix time the last Completed ClusterRestore into this target namespace reached its terminal phase.
# TYPE crystalbackup_clusterrestore_last_success_timestamp_seconds gauge
crystalbackup_clusterrestore_last_success_timestamp_seconds{cluster="prod-eu-1",location="dr-primary",namespace="restored"} 1.7e+09
# HELP crystalbackup_clusterrestore_failures Number of ClusterRestores currently in a failed terminal phase for this series.
# TYPE crystalbackup_clusterrestore_failures gauge
crystalbackup_clusterrestore_failures{cluster="prod-eu-1",location="dr-primary",namespace="restored"} 0
`
	if err := testutil.CollectAndCompare(NewCollector(c), strings.NewReader(want),
		"crystalbackup_restore_last_success_timestamp_seconds",
		"crystalbackup_restore_last_restored_bytes",
		"crystalbackup_restore_failures",
		"crystalbackup_clusterrestore_last_success_timestamp_seconds",
		"crystalbackup_clusterrestore_failures",
	); err != nil {
		t.Fatal(err)
	}
}
