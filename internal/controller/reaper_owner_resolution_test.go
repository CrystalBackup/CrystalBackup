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

// OrphanReaper.orphaned's OWNER RESOLUTION, per plane. reaper_snapshot_test.go pins which objects
// the sweep selects and in what order; this file pins the question asked of each one — "whose is
// this, and is that owner done with it?" — because the 0.6.5 campaign's leaked VolumeSnapshotContent
// was refused by the backstop for the sole reason that the reaper looked up its owner under a label
// only the cluster plane populates. The refusal happened one branch above the NotFound verdict that
// was the answer it needed.
//
// Fake-client tests: resolution is pure label reading plus one owner GET, and the envtest suite runs
// without the snapshot CRDs the interesting objects belong to.

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
	"github.com/CrystalBackup/CrystalBackup/internal/status"
)

// orphanCutoff is far enough in the future that every seeded object is "old enough" to reap, so
// these tests measure resolution and never accidentally measure the MinAge race guard (which
// reaper_test.go owns).
func orphanCutoff() time.Time { return time.Now().Add(time.Hour) }

// labelledContent builds a cluster-scoped VolumeSnapshotContent carrying exactly labels — the
// campaign's leaked object is one of these, and its label set is the whole input to resolution.
func labelledContent(name string, labels map[string]string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion("snapshot.storage.k8s.io/v1")
	u.SetKind("VolumeSnapshotContent")
	u.SetName(name)
	u.SetCreationTimestamp(metav1.Now())
	u.SetLabels(labels)
	return u
}

// namespacePlaneExposureLabels is what this version stamps on a namespace-plane exposure object:
// managed-by, the plane-agnostic owner name, the owner's namespace, the PVC. No cluster-backup key —
// there is no ClusterBackup run to name, and stamping the key with an empty value is precisely the
// trap this whole change removes.
func namespacePlaneExposureLabels(backupName, ns, pvc string) map[string]string {
	return map[string]string{
		apiconst.LabelManagedBy: apiconst.ManagedByValue,
		apiconst.LabelBackup:    backupName,
		apiconst.LabelNamespace: ns,
		apiconst.LabelPVC:       pvc,
	}
}

// seedBackup writes a Backup with the given volume phase for pvc (phase "" ⇒ no volumes at all).
func seedBackup(t *testing.T, c client.Client, ns, name, pvc string, phase status.VolumePhase) *cbv1.Backup {
	t.Helper()
	b := &cbv1.Backup{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
	if err := c.Create(context.Background(), b); err != nil {
		t.Fatalf("seed Backup %s/%s: %v", ns, name, err)
	}
	if phase == "" {
		return b
	}
	b.Status.Volumes = []cbv1.VolumeStatus{{Pvc: pvc, Phase: phase}}
	if err := c.Status().Update(context.Background(), b); err != nil {
		t.Fatalf("seed Backup %s/%s status: %v", ns, name, err)
	}
	return b
}

// TestOrphanedResolvesTheNamespacePlane is the backstop half of the 0.6.5 leak. Each case is a
// namespace-plane exposure object — no cluster-backup label, because that plane has no run — and the
// verdict the reaper must reach about it.
func TestOrphanedResolvesTheNamespacePlane(t *testing.T) {
	const ns, name, pvc = "m5-tenant", "m5-np-run-tjip7a", "tenant-data"

	t.Run("owning Backup is gone: reap", func(t *testing.T) {
		// The campaign's exact situation: the spec's teardown deleted the tenant namespace, taking the
		// Backup with it, and the cluster-scoped content survived with no owner and no deletionTimestamp.
		c := newReaperClient(t)
		obj := labelledContent("snapcontent-owner-gone", namespacePlaneExposureLabels(name, ns, pvc))
		if err := c.Create(context.Background(), obj); err != nil {
			t.Fatal(err)
		}
		r := &OrphanReaper{Client: c, OperatorNamespace: reaperTestOperatorNS}
		got, err := r.orphaned(context.Background(), obj, orphanCutoff())
		if err != nil {
			t.Fatalf("orphaned: %v", err)
		}
		if !got {
			t.Fatal("orphaned = false for a namespace-plane content whose Backup no longer exists.\n" +
				"This is the object that failed five leak checks and sat on the cluster for three hours: " +
				"nothing else in the system will ever collect it.")
		}
	})

	t.Run("owning Backup is gone while a sibling run is live on the same PVC: reap", func(t *testing.T) {
		// This case is what makes the verdict above a NAME resolution rather than a lucky one. Restore
		// the pre-fix lookup (read the run label, find nothing on this plane) and every other
		// namespace-plane case here still passes, because the by-exclusion fallback happens to reach
		// the same answers when the namespace holds exactly one Backup — which is how a mutation run
		// caught this file measuring less than it claimed.
		//
		// A busy namespace separates them. The object names an owner that is GONE, so it is residue;
		// by-exclusion would see the live sibling mid-snapshot on the same PVC and spare it for as long
		// as backups keep running in that namespace. Precision is not a nicety here: the leaked object
		// class is a cluster-scoped content holding a storage snapshot, and "collected eventually,
		// once the tenant stops taking backups" is not a backstop.
		c := newReaperClient(t)
		seedBackup(t, c, ns, "sibling-run", pvc, status.VolumePhaseSnapshotting)
		obj := labelledContent("snapcontent-owner-gone-busy-ns", namespacePlaneExposureLabels(name, ns, pvc))
		if err := c.Create(context.Background(), obj); err != nil {
			t.Fatal(err)
		}
		r := &OrphanReaper{Client: c, OperatorNamespace: reaperTestOperatorNS}
		got, err := r.orphaned(context.Background(), obj, orphanCutoff())
		if err != nil {
			t.Fatalf("orphaned: %v", err)
		}
		if !got {
			t.Fatalf("orphaned = false for a content whose OWN owner (%s/%s) is gone, because another "+
				"Backup in the namespace is busy. The object names its owner; that name is the answer.",
				ns, name)
		}
	})

	t.Run("owning Backup is live and the volume is not terminal: leave it alone", func(t *testing.T) {
		c := newReaperClient(t)
		seedBackup(t, c, ns, name, pvc, status.VolumePhaseSnapshotting)
		obj := labelledContent("snapcontent-live", namespacePlaneExposureLabels(name, ns, pvc))
		if err := c.Create(context.Background(), obj); err != nil {
			t.Fatal(err)
		}
		r := &OrphanReaper{Client: c, OperatorNamespace: reaperTestOperatorNS}
		got, err := r.orphaned(context.Background(), obj, orphanCutoff())
		if err != nil {
			t.Fatalf("orphaned: %v", err)
		}
		if got {
			t.Fatal("orphaned = true for an exposure the owning Backup is still USING. " +
				"Reaping a live run's snapshot mid-backup is far worse than the leak this fix removes.")
		}
	})

	t.Run("owning Backup is live and the volume is terminal: reap", func(t *testing.T) {
		c := newReaperClient(t)
		seedBackup(t, c, ns, name, pvc, status.VolumePhaseCompleted)
		obj := labelledContent("snapcontent-done", namespacePlaneExposureLabels(name, ns, pvc))
		if err := c.Create(context.Background(), obj); err != nil {
			t.Fatal(err)
		}
		r := &OrphanReaper{Client: c, OperatorNamespace: reaperTestOperatorNS}
		got, err := r.orphaned(context.Background(), obj, orphanCutoff())
		if err != nil {
			t.Fatalf("orphaned: %v", err)
		}
		if !got {
			t.Fatal("orphaned = false for an exposure whose volume is Completed; its teardown was " +
				"supposed to have removed it and did not")
		}
	})

	t.Run("owning Backup is being deleted: leave it to its finalizer", func(t *testing.T) {
		c := newReaperClient(t)
		b := &cbv1.Backup{ObjectMeta: metav1.ObjectMeta{
			Namespace: ns, Name: name,
			Finalizers: []string{apiconst.FinalizerBackup},
		}}
		if err := c.Create(context.Background(), b); err != nil {
			t.Fatal(err)
		}
		if err := c.Delete(context.Background(), b); err != nil { // finalizer held ⇒ deletionTimestamp only
			t.Fatal(err)
		}
		obj := labelledContent("snapcontent-deleting", namespacePlaneExposureLabels(name, ns, pvc))
		if err := c.Create(context.Background(), obj); err != nil {
			t.Fatal(err)
		}
		r := &OrphanReaper{Client: c, OperatorNamespace: reaperTestOperatorNS}
		got, err := r.orphaned(context.Background(), obj, orphanCutoff())
		if err != nil {
			t.Fatalf("orphaned: %v", err)
		}
		if got {
			t.Fatal("orphaned = true while the owning Backup's finalizer is running its teardown; " +
				"two collectors on one object is how a teardown races itself")
		}
	})
}

// TestOrphanedResolvesPreUpgradeNamespacePlaneResidue is the UPGRADE path. An object created before
// the owner label existed carries neither owner label, so the preferred lookup has nothing to read.
// Refusing there would be defensible in isolation and wrong in practice: it strands, permanently,
// every piece of residue an earlier version already leaked — including the object this release exists
// to collect. The fallback resolves the owner by exclusion instead: no Backup left in that namespace
// can still be using an exposure for that PVC.
func TestOrphanedResolvesPreUpgradeNamespacePlaneResidue(t *testing.T) {
	const ns, pvc = "m5-tenant", "tenant-data"
	// The campaign's leaked label set, verbatim: no owner label of either kind.
	legacy := map[string]string{
		apiconst.LabelManagedBy: apiconst.ManagedByValue,
		apiconst.LabelNamespace: ns,
		apiconst.LabelPVC:       pvc,
	}

	t.Run("no Backup left in the namespace: reap", func(t *testing.T) {
		c := newReaperClient(t)
		obj := labelledContent("snapcontent-legacy-orphan", legacy)
		if err := c.Create(context.Background(), obj); err != nil {
			t.Fatal(err)
		}
		r := &OrphanReaper{Client: c, OperatorNamespace: reaperTestOperatorNS}
		got, err := r.orphaned(context.Background(), obj, orphanCutoff())
		if err != nil {
			t.Fatalf("orphaned: %v", err)
		}
		if !got {
			t.Fatal("orphaned = false for pre-upgrade residue whose namespace holds no Backup at all. " +
				"Upgrading the operator must not turn existing residue into permanent residue.")
		}
	})

	t.Run("a Backup in the namespace is still using that PVC: leave it alone", func(t *testing.T) {
		c := newReaperClient(t)
		seedBackup(t, c, ns, "sibling-run", pvc, status.VolumePhaseSnapshotting)
		obj := labelledContent("snapcontent-legacy-live", legacy)
		if err := c.Create(context.Background(), obj); err != nil {
			t.Fatal(err)
		}
		r := &OrphanReaper{Client: c, OperatorNamespace: reaperTestOperatorNS}
		got, err := r.orphaned(context.Background(), obj, orphanCutoff())
		if err != nil {
			t.Fatalf("orphaned: %v", err)
		}
		if got {
			t.Fatal("orphaned = true for an unattributable object while a Backup in that namespace is " +
				"mid-snapshot on that very PVC. Without a name to check, the only safe question is " +
				"'could ANY owner still want this?' — and the answer here is yes.")
		}
	})

	t.Run("every Backup in the namespace is done with that PVC: reap", func(t *testing.T) {
		c := newReaperClient(t)
		seedBackup(t, c, ns, "finished-run", pvc, status.VolumePhaseCompleted)
		obj := labelledContent("snapcontent-legacy-done", legacy)
		if err := c.Create(context.Background(), obj); err != nil {
			t.Fatal(err)
		}
		r := &OrphanReaper{Client: c, OperatorNamespace: reaperTestOperatorNS}
		got, err := r.orphaned(context.Background(), obj, orphanCutoff())
		if err != nil {
			t.Fatalf("orphaned: %v", err)
		}
		if !got {
			t.Fatal("orphaned = false although every Backup in the namespace has finished with that PVC")
		}
	})

	t.Run("a Backup with no status yet: leave it alone", func(t *testing.T) {
		// Status not populated means "may be about to expose this PVC", not "finished with it". The
		// MinAge guard already covers most of this window; the resolution must not undo it.
		c := newReaperClient(t)
		seedBackup(t, c, ns, "just-created-run", pvc, "")
		obj := labelledContent("snapcontent-legacy-young-owner", legacy)
		if err := c.Create(context.Background(), obj); err != nil {
			t.Fatal(err)
		}
		r := &OrphanReaper{Client: c, OperatorNamespace: reaperTestOperatorNS}
		got, err := r.orphaned(context.Background(), obj, orphanCutoff())
		if err != nil {
			t.Fatalf("orphaned: %v", err)
		}
		if got {
			t.Fatal("orphaned = true while a Backup in the namespace has not yet reported any volume; " +
				"it may be the object's owner and about to use it")
		}
	})
}

// TestOrphanedStillRefusesTheGenuinelyUnresolvable is the guard the fix must not loosen. Resolving
// the namespace plane was the goal; making the reaper braver was not. An object with no namespace to
// resolve an owner IN has no resolvable owner shape, and the reaper leaves it alone — a labelled
// object nobody can attribute is a bug report, not a delete authorisation.
func TestOrphanedStillRefusesTheGenuinelyUnresolvable(t *testing.T) {
	for _, tc := range []struct {
		what   string
		labels map[string]string
	}{
		{
			what: "no namespace label: there is nowhere to look for an owner",
			labels: map[string]string{
				apiconst.LabelManagedBy: apiconst.ManagedByValue,
				apiconst.LabelBackup:    "some-run",
				apiconst.LabelPVC:       "data",
			},
		},
		{
			what: "no PVC label: not a per-PVC exposure object at all",
			labels: map[string]string{
				apiconst.LabelManagedBy: apiconst.ManagedByValue,
				apiconst.LabelBackup:    "some-run",
				apiconst.LabelNamespace: "m5-tenant",
			},
		},
	} {
		t.Run(tc.what, func(t *testing.T) {
			c := newReaperClient(t)
			obj := labelledContent("snapcontent-unresolvable", tc.labels)
			if err := c.Create(context.Background(), obj); err != nil {
				t.Fatal(err)
			}
			r := &OrphanReaper{Client: c, OperatorNamespace: reaperTestOperatorNS}
			got, err := r.orphaned(context.Background(), obj, orphanCutoff())
			if err != nil {
				t.Fatalf("orphaned: %v", err)
			}
			if got {
				t.Fatalf("orphaned = true for an object with no resolvable owner (%s); the reaper must "+
					"refuse what it cannot attribute", tc.what)
			}
		})
	}
}

// TestOrphanedStillResolvesTheClusterPlaneByRunLabel keeps the pre-existing plane honest across the
// change: a fan-out child's objects created BEFORE the owner label existed carry only the run label,
// whose value is the child Backup's name — so that label remains a complete fallback for them, and
// the cluster plane needs no by-exclusion scan at all.
func TestOrphanedStillResolvesTheClusterPlaneByRunLabel(t *testing.T) {
	const ns, run, pvc = "c-db", "nightly-20260803", "data-db-1"
	c := newReaperClient(t)
	obj := labelledContent("snapcontent-legacy-cluster-plane", exposureLabelsFor(run, ns, pvc))
	if err := c.Create(context.Background(), obj); err != nil {
		t.Fatal(err)
	}
	r := &OrphanReaper{Client: c, OperatorNamespace: reaperTestOperatorNS}

	// Owner absent ⇒ orphan.
	got, err := r.orphaned(context.Background(), obj, orphanCutoff())
	if err != nil {
		t.Fatalf("orphaned: %v", err)
	}
	if !got {
		t.Fatal("orphaned = false for a cluster-plane object whose child Backup is gone")
	}

	// Owner present and mid-flight ⇒ spared, resolved through the same run label.
	seedBackup(t, c, ns, run, pvc, status.VolumePhaseSnapshotting)
	got, err = r.orphaned(context.Background(), obj, orphanCutoff())
	if err != nil {
		t.Fatalf("orphaned: %v", err)
	}
	if got {
		t.Fatal("orphaned = true for a cluster-plane object whose child Backup is mid-snapshot")
	}
}
