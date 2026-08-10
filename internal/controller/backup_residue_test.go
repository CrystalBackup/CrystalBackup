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
	"sigs.k8s.io/controller-runtime/pkg/client"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
	"github.com/CrystalBackup/CrystalBackup/internal/status"
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

// seedLabelledVSC creates one cluster-scoped VolumeSnapshotContent with exactly the given labels —
// the shape a leaked origin content has: created by the snapshot-controller, then label-stamped by
// the exposer's handover patch, which is why its label set is whatever that patch managed to write
// and not necessarily the full exposure trio.
func seedLabelledVSC(t *testing.T, c client.Client, name string, labels map[string]string) {
	t.Helper()
	u := vscUnstructured(name)
	u.SetCreationTimestamp(metav1.Now())
	u.SetLabels(labels)
	if err := c.Create(context.Background(), u); err != nil {
		t.Fatalf("seed VolumeSnapshotContent %s: %v", name, err)
	}
}

// namespacePlaneBackup builds the terminal namespace-plane Backup the sweep runs on: origin=namespace,
// NO cluster-backup label (it is not a fan-out child), and one volume in its status.
func namespacePlaneBackup(ns, name, pvc string) *cbv1.Backup {
	b := &cbv1.Backup{ObjectMeta: metav1.ObjectMeta{
		Namespace: ns, Name: name,
		Labels: map[string]string{apiconst.LabelOrigin: apiconst.OriginNamespace},
	}}
	b.Status.Volumes = []cbv1.VolumeStatus{{Pvc: pvc, Phase: status.VolumePhaseCompleted}}
	return b
}

// TestExposureResidueRemainsSeesNamespacePlaneResidue is the test that would have caught the 0.6.5
// campaign failure, and it is written against the EXACT object that leaked: a labelled, cluster-scoped
// VolumeSnapshotContent belonging to a namespace-plane Backup, with no deletionTimestamp and no owner.
//
// Five leak checks in four milestones failed on that one object, and the sweep that was supposed to
// notice it had already stamped "exposures cleaned". It had stamped it because its verification read
// selected on crystalbackup.io/cluster-backup — a label the namespace plane does not have — so the
// read could not match the object it exists to find. A verification that cannot fail is worse than
// no verification, because it is believed.
//
// The two sub-cases are the two label shapes a namespace-plane content can have in the field:
//   - "stamped": created by this version, carrying the plane-agnostic owner label;
//   - "legacy": created by an earlier version, carrying NEITHER owner label (the exposer's handover
//     patch merges labels and skips a key whose desired value is empty, which is exactly how the
//     campaign's object ended up with only managed-by + namespace + pvc).
//
// Both must read as residue. The legacy case is the upgrade path: an operator that only understood
// the new label would stamp "clean" over residue an earlier version left behind.
func TestExposureResidueRemainsSeesNamespacePlaneResidue(t *testing.T) {
	const ns, name, pvc = "m5-tenant", "m5-np-run-tjip7a", "tenant-data"

	for _, tc := range []struct {
		what   string
		labels map[string]string
	}{
		{
			what: "stamped by this version",
			labels: map[string]string{
				apiconst.LabelManagedBy: apiconst.ManagedByValue,
				apiconst.LabelBackup:    name,
				apiconst.LabelNamespace: ns,
				apiconst.LabelPVC:       pvc,
			},
		},
		{
			what: "left by an earlier version, with no owner label at all",
			labels: map[string]string{
				apiconst.LabelManagedBy: apiconst.ManagedByValue,
				apiconst.LabelNamespace: ns,
				apiconst.LabelPVC:       pvc,
			},
		},
	} {
		t.Run(tc.what, func(t *testing.T) {
			ctx := context.Background()
			c := newReaperClient(t)
			seedLabelledVSC(t, c, "snapcontent-23f7c113-23f3-4148-a3a1-ccaba8c0aef6", tc.labels)

			r := &BackupReconciler{Client: c, OperatorNamespace: reaperTestOperatorNS}
			got := r.exposureResidueRemains(ctx, namespacePlaneBackup(ns, name, pvc))
			if !strings.Contains(got, "VolumeSnapshotContent") {
				t.Fatalf("residue = %q; want the leaked VolumeSnapshotContent named.\n"+
					"An empty result here is the sweep stamping %s over a live leak, which is what "+
					"failed five leak checks and burned the campaign's four-hour budget.",
					got, apiconst.AnnotationExposuresCleaned)
			}
		})
	}
}

// TestExposureResidueRemainsIgnoresAnotherBackupsResidue is the other half, and without it the test
// above is satisfied by a read that reports residue for anything labelled — which would wedge every
// terminal Backup in a namespace on its neighbour's leftovers forever. Attribution has to stay tight
// on the shape that CAN be attributed.
func TestExposureResidueRemainsIgnoresAnotherBackupsResidue(t *testing.T) {
	ctx := context.Background()
	const ns, mine, theirs, pvc = "m5-tenant", "run-mine", "run-theirs", "tenant-data"
	c := newReaperClient(t)
	seedLabelledVSC(t, c, "snapcontent-theirs", map[string]string{
		apiconst.LabelManagedBy: apiconst.ManagedByValue,
		apiconst.LabelBackup:    theirs,
		apiconst.LabelNamespace: ns,
		apiconst.LabelPVC:       pvc,
	})

	r := &BackupReconciler{Client: c, OperatorNamespace: reaperTestOperatorNS}
	if got := r.exposureResidueRemains(ctx, namespacePlaneBackup(ns, mine, pvc)); got != "" {
		t.Fatalf("residue = %q; want empty — that content belongs to Backup %s/%s, not to this one",
			got, ns, theirs)
	}
}

// TestExposureResidueRemainsIsLoudWhenItCannotAddressTheObjects pins the deeper defect class, not
// just the missing label: a verification read must never be able to report "clean" because its own
// selector was malformed. The function's comment always promised "never let an unreadable cluster
// read as a clean one"; an unaddressable one is the same failure with a quieter cause, and the
// original code silently accepted an empty label value — which selects the objects whose value for
// that key is literally "", not "any".
func TestExposureResidueRemainsIsLoudWhenItCannotAddressTheObjects(t *testing.T) {
	ctx := context.Background()
	c := newReaperClient(t)
	r := &BackupReconciler{Client: c, OperatorNamespace: reaperTestOperatorNS}

	// A Backup with no namespace cannot have its objects addressed at all. Nothing is seeded, so a
	// selector-blind implementation returns "" — "clean" — from a read that never looked.
	got := r.exposureResidueRemains(ctx, &cbv1.Backup{ObjectMeta: metav1.ObjectMeta{Name: "no-ns"}})
	if got == "" {
		t.Fatal("residue = \"\" for a Backup whose exposure objects cannot be addressed; " +
			"want a loud, non-empty report — a selector that cannot match must not read as clean")
	}
	if !strings.Contains(got, "unresolvable") {
		t.Errorf("residue = %q; want it to say the selector is unresolvable so the log names the cause", got)
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
