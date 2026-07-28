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

package controller

// exposureResidueRemains is the terminal sweep's verification read: the exposures-cleaned marker
// may only be stamped once NOTHING labelled remains — a round-1 validation lane proved the
// deletes-succeeded criterion alone stamps the marker while the external snapshot-controller is
// still draining a VolumeSnapshotContent. Fake-client tests (the envtest suite has no snapshot
// CRDs; there the VS/VSC lists NoMatch-skip, which is itself the correct vacuous behaviour).

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
)

func TestExposureResidueRemainsDrainsInOrder(t *testing.T) {
	ctx := context.Background()
	c := newReaperClient(t)
	const run, ns, pvc = "run-res", "c-db", "data-db-1"
	originVSC, _, originVS := seedSnapshotResidue(t, c, run, ns, pvc)

	// A temp clone PVC in the operator namespace, exposure-labelled.
	clone := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: reaperTestOperatorNS,
			Name:      moverNamePrefix(ns, run, pvc) + "-clone",
			Labels:    exposureLabelsFor(run, ns, pvc),
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
			},
		},
	}
	if err := c.Create(ctx, clone); err != nil {
		t.Fatal(err)
	}

	r := &BackupReconciler{Client: c, OperatorNamespace: reaperTestOperatorNS}
	backup := &cbv1.Backup{ObjectMeta: metav1.ObjectMeta{
		Namespace: ns, Name: run,
		Labels: map[string]string{apiconst.LabelClusterBackup: run},
	}}

	// Full residue: the cluster-scoped content is reported first.
	if got := r.exposureResidueRemains(ctx, backup); !strings.Contains(got, "VolumeSnapshotContent") {
		t.Fatalf("with a lingering VSC, residue = %q; want it named", got)
	}

	// Contents gone → the VolumeSnapshot is what remains.
	for _, name := range []string{originVSC, moverNamePrefix(ns, run, pvc) + "-vsc"} {
		u := vscUnstructured(name)
		if err := c.Delete(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	if got := r.exposureResidueRemains(ctx, backup); !strings.Contains(got, "VolumeSnapshot "+ns+"/"+originVS) {
		t.Fatalf("with a lingering VS, residue = %q; want VolumeSnapshot %s/%s", got, ns, originVS)
	}

	// VS gone → the Terminating-style clone PVC is what remains.
	vs := vsUnstructured(ns, originVS)
	if err := c.Delete(ctx, vs); err != nil {
		t.Fatal(err)
	}
	if got := r.exposureResidueRemains(ctx, backup); !strings.Contains(got, "temp clone PVC") {
		t.Fatalf("with a lingering clone PVC, residue = %q; want it named", got)
	}

	// Everything drained → clean, the marker may be stamped.
	if err := c.Delete(ctx, clone); err != nil {
		t.Fatal(err)
	}
	if got := r.exposureResidueRemains(ctx, backup); got != "" {
		t.Fatalf("with nothing left, residue = %q; want empty", got)
	}
}

func vscUnstructured(name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion("snapshot.storage.k8s.io/v1")
	u.SetKind("VolumeSnapshotContent")
	u.SetName(name)
	return u
}

func vsUnstructured(ns, name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion("snapshot.storage.k8s.io/v1")
	u.SetKind("VolumeSnapshot")
	u.SetNamespace(ns)
	u.SetName(name)
	return u
}
