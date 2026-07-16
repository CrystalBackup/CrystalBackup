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

package utils

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
)

const (
	drPrimary = "dr-primary"
	cTeamX    = "c-team-x"
	myOffsite = "my-offsite"
)

// SampleObjects returns one minimal, schema-valid CR for each of the twelve
// CrystalBackup kinds. Namespaced kinds land in the given namespace. Shared by
// the envtest CRD round-trip (test/crd) and the live crucible suite
// (test/crucible) so both always cover the full API surface.
func SampleObjects(namespace string) []client.Object {
	s3 := cbv1.S3Spec{
		Endpoint:             "https://s3.example.test",
		Bucket:               "b",
		CredentialsSecretRef: cbv1.LocalObjectReference{Name: "s3-creds"},
	}
	return []client.Object{
		&cbv1.ClusterBackupLocation{
			ObjectMeta: metav1.ObjectMeta{Name: drPrimary},
			Spec: cbv1.ClusterBackupLocationSpec{
				ClusterID:  "test-cluster",
				S3:         s3,
				Encryption: cbv1.ClusterEncryptionSpec{ClusterKEKSecretRef: cbv1.LocalObjectReference{Name: "cluster-kek"}},
			},
		},
		&cbv1.ClusterBackupSchedule{
			ObjectMeta: metav1.ObjectMeta{Name: "dr-daily"},
			Spec: cbv1.ClusterBackupScheduleSpec{
				Schedule: "0 2 * * *",
				Template: cbv1.ClusterBackupTemplate{Spec: cbv1.ClusterBackupRunSpec{
					LocationRef: cbv1.LocalObjectReference{Name: drPrimary},
					Namespaces:  cbv1.NamespaceSelector{Regexp: "^c-.+$"},
				}},
			},
		},
		&cbv1.ClusterBackup{
			ObjectMeta: metav1.ObjectMeta{Name: "dr-run-1"},
			Spec: cbv1.ClusterBackupSpec{ClusterBackupRunSpec: cbv1.ClusterBackupRunSpec{
				LocationRef: cbv1.LocalObjectReference{Name: drPrimary},
			}},
		},
		&cbv1.ClusterRestore{
			ObjectMeta: metav1.ObjectMeta{Name: "recover-x"},
			Spec: cbv1.ClusterRestoreSpec{
				Source:       cbv1.ClusterRestoreSource{LocationRef: cbv1.LocalObjectReference{Name: drPrimary}, Namespace: cTeamX},
				Target:       cbv1.ClusterRestoreTarget{Namespace: "c-team-x-restored", CreateNamespace: true},
				Confirmation: "c-team-x-restored",
			},
		},
		&cbv1.ClusterErasure{
			ObjectMeta: metav1.ObjectMeta{Name: "gdpr-x"},
			Spec: cbv1.ClusterErasureSpec{
				LocationRef:  cbv1.LocalObjectReference{Name: drPrimary},
				Target:       cbv1.ErasureTarget{Namespace: cTeamX},
				Confirmation: cTeamX,
			},
		},
		&cbv1.ClusterBackupExternalSync{
			ObjectMeta: metav1.ObjectMeta{Name: "dr-to-b"},
			Spec: cbv1.ClusterBackupExternalSyncSpec{
				SourceLocationRef:      cbv1.LocalObjectReference{Name: drPrimary},
				DestinationLocationRef: cbv1.LocalObjectReference{Name: "dr-secondary"},
			},
		},
		&cbv1.BackupLocation{
			ObjectMeta: metav1.ObjectMeta{Name: myOffsite, Namespace: namespace},
			Spec:       cbv1.BackupLocationSpec{S3: s3, Encryption: cbv1.NamespaceEncryptionSpec{}},
		},
		&cbv1.BackupSchedule{
			ObjectMeta: metav1.ObjectMeta{Name: "daily", Namespace: namespace},
			Spec:       cbv1.BackupScheduleSpec{LocationRef: cbv1.LocalObjectReference{Name: myOffsite}, Schedule: "0 1 * * *"},
		},
		&cbv1.Backup{
			ObjectMeta: metav1.ObjectMeta{Name: "daily-1", Namespace: namespace},
			Spec:       cbv1.BackupSpec{LocationRef: cbv1.LocationReference{Name: myOffsite}},
		},
		&cbv1.Restore{
			ObjectMeta: metav1.ObjectMeta{Name: "recover-uploads", Namespace: namespace},
			Spec:       cbv1.RestoreSpec{Source: cbv1.RestoreSource{Backup: "daily-1"}},
		},
		&cbv1.BackupExternalSync{
			ObjectMeta: metav1.ObjectMeta{Name: "offsite-mirror", Namespace: namespace},
			Spec: cbv1.BackupExternalSyncSpec{
				SourceLocationRef:      cbv1.LocalObjectReference{Name: myOffsite},
				DestinationLocationRef: cbv1.LocalObjectReference{Name: "my-offsite-2"},
			},
		},
		&cbv1.BackupRepository{
			ObjectMeta: metav1.ObjectMeta{Name: "repo-1"},
			Spec:       cbv1.BackupRepositorySpec{},
		},
	}
}
