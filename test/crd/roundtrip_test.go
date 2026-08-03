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
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/test/utils"
)

const ns = "default"

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

	all := utils.SampleObjects(ns)
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

	assertS3ConnectionsRoundTrip(t, ctx, k)
}

// assertS3ConnectionsRoundTrip proves S3Spec.Connections survives a real API server, on BOTH
// planes, and that its bounds are enforced there rather than only in a Go tag.
//
// The loop above cannot cover this: it compares names and UIDs, so a CRD schema that silently
// discarded an unknown or mis-generated field would still round-trip every sample "successfully".
// A pointer field makes that failure mode completely quiet — the value simply comes back nil, and
// every consumer downstream reads that as "the operator did not configure a cap".
//
// The MAXIMUM is asserted, not just the happy path, because it is the field's security half:
// BackupLocation is tenant-writable and every namespace shares one gateway, so the ceiling is
// what stops `connections: 100000` from being a tenant-authored denial of service against every
// other tenant. A ceiling that lives only in a marker comment and never reached the installed CRD
// is not a ceiling.
func assertS3ConnectionsRoundTrip(t *testing.T, ctx context.Context, k client.Client) {
	t.Helper()

	// --- the value persists, on both planes -------------------------------------------------
	var cbl cbv1.ClusterBackupLocation
	if err := k.Get(ctx, client.ObjectKey{Name: "dr-primary"}, &cbl); err != nil {
		t.Fatalf("get ClusterBackupLocation back: %v", err)
	}
	if got := cbl.Spec.S3.Connections; got == nil || *got != utils.SampleConnections {
		t.Errorf("ClusterBackupLocation spec.s3.connections = %v, want %d — the field did not "+
			"survive the API server", fmtConnections(got), utils.SampleConnections)
	}

	var loc cbv1.BackupLocation
	if err := k.Get(ctx, client.ObjectKey{Namespace: ns, Name: "my-offsite"}, &loc); err != nil {
		t.Fatalf("get BackupLocation back: %v", err)
	}
	if got := loc.Spec.S3.Connections; got == nil || *got != utils.SampleConnections {
		t.Errorf("BackupLocation spec.s3.connections = %v, want %d — the tenant-writable plane "+
			"lost the field", fmtConnections(got), utils.SampleConnections)
	}

	// --- the bounds are enforced by the installed CRD, not merely by a marker ----------------
	for _, tc := range []struct {
		name  string
		value int32
	}{
		{"above the maximum", 101},
		{"below the minimum", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := loc.DeepCopy()
			bad.ObjectMeta = metav1.ObjectMeta{Name: "bad-connections", Namespace: ns}
			bad.Spec.S3.Connections = ptr.To(tc.value)
			err := k.Create(ctx, bad)
			if err == nil {
				_ = k.Delete(ctx, bad)
				t.Errorf("the API server ACCEPTED spec.s3.connections=%d — a tenant can aim "+
					"arbitrary concurrency at the shared gateway", tc.value)
				return
			}
			if !apierrors.IsInvalid(err) {
				t.Errorf("create with connections=%d failed with %v, want an Invalid (schema "+
					"validation) error — a different failure would pass this test for the wrong "+
					"reason", tc.value, err)
			}
		})
	}
}

// fmtConnections renders the optional cap for a failure message, so a nil reads as "nil" rather
// than as a pointer address the reader has to decode.
func fmtConnections(v *int32) string {
	if v == nil {
		return "nil"
	}
	return fmt.Sprintf("%d", *v)
}
