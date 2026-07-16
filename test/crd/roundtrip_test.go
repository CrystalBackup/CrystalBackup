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
	"k8s.io/apimachinery/pkg/runtime"
	clientscheme "k8s.io/client-go/kubernetes/scheme"
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
}
