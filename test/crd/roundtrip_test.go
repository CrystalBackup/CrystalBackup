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

// Package crd contains an envtest that proves every CrystalBackup CRD installs
// into a real API server and round-trips through the typed client (M0 exit criterion).
package crd

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
)

const ns = "default"

// samples returns one minimal, schema-valid CR for each of the twelve kinds.
func samples() []client.Object {
	s3 := cbv1.S3Spec{Endpoint: "https://s3.example.test", Bucket: "b", CredentialsSecretRef: cbv1.LocalObjectReference{Name: "s3-creds"}}
	return []client.Object{
		&cbv1.ClusterBackupLocation{
			ObjectMeta: metav1.ObjectMeta{Name: "dr-primary"},
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
					LocationRef: cbv1.LocalObjectReference{Name: "dr-primary"},
					Namespaces:  cbv1.NamespaceSelector{Regexp: "^c-.+$"},
				}},
			},
		},
		&cbv1.ClusterBackup{
			ObjectMeta: metav1.ObjectMeta{Name: "dr-run-1"},
			Spec:       cbv1.ClusterBackupSpec{ClusterBackupRunSpec: cbv1.ClusterBackupRunSpec{LocationRef: cbv1.LocalObjectReference{Name: "dr-primary"}}},
		},
		&cbv1.ClusterRestore{
			ObjectMeta: metav1.ObjectMeta{Name: "recover-x"},
			Spec: cbv1.ClusterRestoreSpec{
				Source:       cbv1.ClusterRestoreSource{LocationRef: cbv1.LocalObjectReference{Name: "dr-primary"}, Namespace: "c-team-x"},
				Target:       cbv1.ClusterRestoreTarget{Namespace: "c-team-x-restored", CreateNamespace: true},
				Confirmation: "c-team-x-restored",
			},
		},
		&cbv1.ClusterErasure{
			ObjectMeta: metav1.ObjectMeta{Name: "gdpr-x"},
			Spec: cbv1.ClusterErasureSpec{
				LocationRef:  cbv1.LocalObjectReference{Name: "dr-primary"},
				Target:       cbv1.ErasureTarget{Namespace: "c-team-x"},
				Confirmation: "c-team-x",
			},
		},
		&cbv1.ClusterBackupExternalSync{
			ObjectMeta: metav1.ObjectMeta{Name: "dr-to-b"},
			Spec: cbv1.ClusterBackupExternalSyncSpec{
				SourceLocationRef:      cbv1.LocalObjectReference{Name: "dr-primary"},
				DestinationLocationRef: cbv1.LocalObjectReference{Name: "dr-secondary"},
			},
		},
		&cbv1.BackupLocation{
			ObjectMeta: metav1.ObjectMeta{Name: "my-offsite", Namespace: ns},
			Spec:       cbv1.BackupLocationSpec{S3: s3, Encryption: cbv1.NamespaceEncryptionSpec{}},
		},
		&cbv1.BackupSchedule{
			ObjectMeta: metav1.ObjectMeta{Name: "daily", Namespace: ns},
			Spec:       cbv1.BackupScheduleSpec{LocationRef: cbv1.LocalObjectReference{Name: "my-offsite"}, Schedule: "0 1 * * *"},
		},
		&cbv1.Backup{
			ObjectMeta: metav1.ObjectMeta{Name: "daily-1", Namespace: ns},
			Spec:       cbv1.BackupSpec{LocationRef: cbv1.LocationReference{Name: "my-offsite"}},
		},
		&cbv1.Restore{
			ObjectMeta: metav1.ObjectMeta{Name: "recover-uploads", Namespace: ns},
			Spec:       cbv1.RestoreSpec{Source: cbv1.RestoreSource{Backup: "daily-1"}},
		},
		&cbv1.BackupExternalSync{
			ObjectMeta: metav1.ObjectMeta{Name: "offsite-mirror", Namespace: ns},
			Spec: cbv1.BackupExternalSyncSpec{
				SourceLocationRef:      cbv1.LocalObjectReference{Name: "my-offsite"},
				DestinationLocationRef: cbv1.LocalObjectReference{Name: "my-offsite-2"},
			},
		},
		&cbv1.BackupRepository{
			ObjectMeta: metav1.ObjectMeta{Name: "repo-1"},
			Spec:       cbv1.BackupRepositorySpec{},
		},
	}
}

func TestCRDInstallAndRoundTrip(t *testing.T) {
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest (is KUBEBUILDER_ASSETS set? run via `make test`): %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })

	sc := runtime.NewScheme()
	if err := clientscheme.AddToScheme(sc); err != nil {
		t.Fatalf("add client-go scheme: %v", err)
	}
	if err := cbv1.AddToScheme(sc); err != nil {
		t.Fatalf("add crystalbackup scheme: %v", err)
	}

	k, err := client.New(cfg, client.Options{Scheme: sc})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	ctx := context.Background()

	all := samples()
	if len(all) != 12 {
		t.Fatalf("expected 12 sample kinds, got %d", len(all))
	}
	for _, obj := range all {
		kind := fmt.Sprintf("%T", obj)
		if err := k.Create(ctx, obj); err != nil {
			t.Errorf("create %s/%s: %v", kind, obj.GetName(), err)
			continue
		}
		// Get overwrites the destination with the server's copy → true round-trip.
		fresh := obj.DeepCopyObject().(client.Object)
		if err := k.Get(ctx, client.ObjectKeyFromObject(obj), fresh); err != nil {
			t.Errorf("get %s/%s after create: %v", kind, obj.GetName(), err)
			continue
		}
		if fresh.GetName() != obj.GetName() {
			t.Errorf("%s round-trip name mismatch: got %q want %q", kind, fresh.GetName(), obj.GetName())
		}
		if fresh.GetUID() == "" {
			t.Errorf("%s round-trip: empty UID (object was not persisted)", kind)
		}
		t.Logf("ok  %-32s %s installed + round-tripped (uid=%s)", kind, obj.GetName(), fresh.GetUID())
	}

	// Sanity: a missing object returns NotFound (client + scheme correctly wired).
	var bl cbv1.BackupLocation
	if err := k.Get(ctx, client.ObjectKey{Namespace: ns, Name: "nope"}, &bl); !apierrors.IsNotFound(err) {
		t.Errorf("expected NotFound for missing BackupLocation, got %v", err)
	}
}
