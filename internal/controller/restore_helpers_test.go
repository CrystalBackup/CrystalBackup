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

import (
	"math/rand"
	"slices"
	"strings"
	"testing"
	"time"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/restic"
	"github.com/CrystalBackup/CrystalBackup/internal/status"
)

// TestParseRestoreTime pins the runtime discriminator behind the InvalidSourceTime gate: every
// form the CRD CEL admits and the controller reads must parse, and a SHAPE-valid but impossible
// instant (which the anchored CEL regex cannot reject — it only checks shape) must return ok=false
// so the caller gates with a clear reason instead of the misleading "not projected yet".
func TestParseRestoreTime(t *testing.T) {
	valid := []string{
		"2026-07-01T12:00:00",              // zone-less, read as UTC
		"2026-07-01T12:00:00Z",             // RFC3339 UTC
		"2026-07-01T12:00:00+02:00",        // RFC3339 offset
		"2026-07-01T12:00:00.123456+02:00", // fractional seconds
	}
	for _, v := range valid {
		if _, ok := parseRestoreTime(v); !ok {
			t.Errorf("parseRestoreTime(%q) = ok:false, want a parsed instant", v)
		}
	}

	invalid := []string{
		"2026-13-45T99:99:99", // shape-valid per the CEL regex, but an impossible date/time
		"2026-07-01T12:00:00garbage",
		"not-a-time",
		"latest", // the sentinel is handled by the caller, never passed to parseRestoreTime
		"",
	}
	for _, v := range invalid {
		if _, ok := parseRestoreTime(v); ok {
			t.Errorf("parseRestoreTime(%q) = ok:true, want rejected", v)
		}
	}
}

// TestPlanVolumesSelection pins the 02-api selection semantics: nil ⇒ everything, [] ⇒
// nothing, OR between items with the FIRST matching item's narrowing winning, an item
// without names matching every PVC.
func TestPlanVolumesSelection(t *testing.T) {
	sources := []string{"data", "logs", "uploads"}

	t.Run("nil selects everything, whole-PVC", func(t *testing.T) {
		got := planVolumes(nil, sources, false)
		if len(got) != 3 {
			t.Fatalf("selected %d PVCs, want 3", len(got))
		}
		for pvc, item := range got {
			if item != nil {
				t.Errorf("PVC %s carries a narrowing item, want whole-PVC nil", pvc)
			}
		}
	})

	t.Run("present-but-empty selects nothing", func(t *testing.T) {
		if got := planVolumes([]cbv1.VolumeSelectorItem{}, sources, true); len(got) != 0 {
			t.Errorf("selected %v, want nothing (a present [] restores nothing of that kind)", got)
		}
	})

	t.Run("first matching item wins", func(t *testing.T) {
		items := []cbv1.VolumeSelectorItem{
			{Names: []string{"uploads"}, Include: []string{"images/**"}},
			{Include: []string{"everything/**"}}, // no names ⇒ matches every PVC
		}
		got := planVolumes(items, sources, true)
		if len(got) != 3 {
			t.Fatalf("selected %d PVCs, want 3 (the nameless item matches all)", len(got))
		}
		if got["uploads"] != &items[0] {
			t.Errorf("uploads matched item %+v, want the FIRST item", got["uploads"])
		}
		if got["data"] != &items[1] {
			t.Errorf("data matched item %+v, want the second (nameless) item", got["data"])
		}
	})
}

// TestRestoreTargetPath pins the targetPath resolution: empty and "/" mean the mount root,
// a relative path is confined under it, and traversal can never escape (defense in depth
// behind the CRD's CEL).
func TestRestoreTargetPath(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"", restoreTargetMountPath, false},
		{"/", restoreTargetMountPath, false},
		{"sub/dir", restoreTargetMountPath + "/sub/dir", false},
		{"/rooted", restoreTargetMountPath + "/rooted", false},
		{"a/./b//c", restoreTargetMountPath + "/a/b/c", false},
		{"../escape", "", true},
		{"a/../../b", "", true},
	}
	for _, tc := range cases {
		got, err := restoreTargetPath(tc.in)
		if (err != nil) != tc.wantErr || got != tc.want {
			t.Errorf("restoreTargetPath(%q) = %q, %v; want %q, err=%v", tc.in, got, err, tc.want, tc.wantErr)
		}
		if got != "" && !strings.HasPrefix(got, restoreTargetMountPath) {
			t.Errorf("restoreTargetPath(%q) = %q escapes the mount root", tc.in, got)
		}
	}
}

// TestFallbackRestoreCapacity pins the adr/0016 pre-0.2 sizing fallback: data size + 20%
// headroom, rounded up to a whole GiB, never below 1Gi.
func TestFallbackRestoreCapacity(t *testing.T) {
	gib := int64(1) << 30
	cases := []struct {
		data int64
		want int64 // in GiB
	}{
		{0, 1},
		{100, 1},
		{gib, 2},         // 1Gi data + 20% > 1Gi ⇒ 2Gi
		{5 * gib, 6},     // 6Gi exactly for 5Gi + 1Gi headroom
		{10*gib - 1, 12}, // just under 10Gi + 20% ⇒ ceil(11.999…) = 12Gi
		{800 * 1024, 1},  // small data stays at the 1Gi floor
	}
	for _, tc := range cases {
		got := fallbackRestoreCapacity(tc.data)
		if got.Value() != tc.want*gib {
			t.Errorf("fallbackRestoreCapacity(%d) = %d bytes, want %dGi", tc.data, got.Value(), tc.want)
		}
	}
}

// TestSelectRun pins the repo-coordinate run selection: a named run wins outright, "latest"
// (or empty) picks the newest run, and an RFC3339 cutoff excludes newer runs.
func TestSelectRun(t *testing.T) {
	at := func(s string) time.Time {
		ts, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatalf("parse %q: %v", s, err)
		}
		return ts
	}
	snap := func(run string, ts time.Time) restic.Snapshot {
		return restic.Snapshot{
			ID:   run + "-id",
			Time: ts,
			Tags: []string{restic.TagBase, restic.Tag(restic.TagKeyRun, run)},
		}
	}
	snaps := []restic.Snapshot{
		snap("run-old", at("2026-07-16T02:00:00Z")),
		snap("run-mid", at("2026-07-17T02:00:00Z")),
		snap("run-new", at("2026-07-18T02:00:00Z")),
	}

	if run, _, ok := selectRun(snaps, "run-mid", ""); !ok || run != "run-mid" {
		t.Errorf("named run = %q/%v, want run-mid/true", run, ok)
	}
	if _, _, ok := selectRun(snaps, "run-ghost", ""); ok {
		t.Error("a named run absent from the repo must not resolve")
	}
	if run, _, ok := selectRun(snaps, "", "latest"); !ok || run != "run-new" {
		t.Errorf("latest = %q/%v, want run-new/true", run, ok)
	}
	if run, _, ok := selectRun(snaps, "", "2026-07-17T12:00:00Z"); !ok || run != "run-mid" {
		t.Errorf("cutoff = %q/%v, want run-mid/true (run-new is after the cutoff)", run, ok)
	}
	if _, _, ok := selectRun(snaps, "", "2026-07-15T00:00:00Z"); ok {
		t.Error("a cutoff before every run must resolve nothing")
	}
	if _, _, ok := selectRun(snaps, "", "not-a-timestamp"); ok {
		t.Error("an unparseable time must resolve nothing")
	}
}

// TestDataSnapshotsByPVC pins the per-PVC indexing: only kind=data snapshots with a pvc=
// tag are indexed, and the newest wins a duplicate.
func TestDataSnapshotsByPVC(t *testing.T) {
	older := restic.Snapshot{ID: "old", Time: time.Unix(100, 0), Tags: []string{
		restic.Tag(restic.TagKeyKind, restic.KindData), restic.Tag(restic.TagKeyPVC, "data")}}
	newer := restic.Snapshot{ID: "new", Time: time.Unix(200, 0), Tags: []string{
		restic.Tag(restic.TagKeyKind, restic.KindData), restic.Tag(restic.TagKeyPVC, "data")}}
	manifests := restic.Snapshot{ID: "m", Time: time.Unix(300, 0), Tags: []string{
		restic.Tag(restic.TagKeyKind, restic.KindManifests)}}
	tagless := restic.Snapshot{ID: "t", Time: time.Unix(300, 0), Tags: []string{
		restic.Tag(restic.TagKeyKind, restic.KindData)}}

	got := dataSnapshotsByPVC([]restic.Snapshot{older, newer, manifests, tagless})
	if len(got) != 1 {
		t.Fatalf("indexed %d PVCs, want 1 (manifests and pvc-less snapshots excluded)", len(got))
	}
	if got["data"].ID != "new" {
		t.Errorf("data snapshot = %s, want the newest (new)", got["data"].ID)
	}
}

// TestRestorableVolumes pins that a source Backup projecting DUPLICATE entries for one PVC
// (a repository holding several snapshots of it under one run — reused run name, retried
// backup) yields ONE restorable name, so the restore builds one plan per PVC. Incomplete /
// snapshot-less entries are excluded.
func TestRestorableVolumes(t *testing.T) {
	source := &cbv1.Backup{}
	source.Status.Volumes = []cbv1.VolumeStatus{
		{Pvc: "data", Phase: status.VolumePhaseCompleted, SnapshotID: "aaa"},
		{Pvc: "data", Phase: status.VolumePhaseCompleted, SnapshotID: "bbb"}, // duplicate PVC
		{Pvc: "logs", Phase: status.VolumePhaseCompleted, SnapshotID: "ccc"},
		{Pvc: "cache", Phase: status.VolumePhaseFailed, SnapshotID: "ddd"}, // not completed
		{Pvc: "tmp", Phase: status.VolumePhaseCompleted, SnapshotID: ""},   // no snapshot
	}
	got := restorableVolumes(source)
	slices.Sort(got)
	want := []string{"data", "logs"}
	if !slices.Equal(got, want) {
		t.Errorf("restorableVolumes = %v, want %v (dedupe by PVC; only completed+snapshotted)", got, want)
	}
}

// TestMediatedFilterNamespaceIndependence is the R2/R14 cornerstone property for the
// restore path (08-testing §2 "user-input independence"): for a FIXED namespace, no
// user-writable Restore spec content can change the namespace half of the mediated filter
// or the snapshot subtree path — both derive from metadata.namespace alone. The spec's
// source.backup legitimately narrows the run WITHIN the namespace and is therefore outside
// the property.
func TestMediatedFilterNamespaceIndependence(t *testing.T) {
	r := rand.New(rand.NewSource(42))
	junk := func() string {
		const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789-./*"
		n := 1 + r.Intn(30)
		b := make([]byte, n)
		for i := range b {
			b[i] = alphabet[r.Intn(len(alphabet))]
		}
		return string(b)
	}

	const namespace = "tenant-a"
	wantFilter := restic.Tag(restic.TagKeyNamespace, namespace)
	wantPathPrefix := "/data/" + namespace + "/"

	for i := 0; i < 500; i++ {
		// Fuzz every user-writable field a Restore spec carries.
		spec := cbv1.RestoreSpec{
			Source:       cbv1.RestoreSource{Backup: junk(), Origin: "cluster"},
			Mode:         cbv1.RestoreModeRecreate,
			Confirmation: junk(),
			Volumes: []cbv1.VolumeSelectorItem{{
				Names:      []string{junk()},
				Include:    []string{junk()},
				Exclude:    []string{junk()},
				TargetPath: junk(),
			}},
		}

		// The namespace filter tag depends on metadata.namespace ONLY.
		if got := restic.Tag(restic.TagKeyNamespace, namespace); got != wantFilter {
			t.Fatalf("filter drifted to %q under spec %+v", got, spec)
		}
		// The snapshot subtree path likewise: the PVC segment is repo-derived (the mediated
		// listing's pvc= tag), never a free-form spec value.
		pvc := junk()
		path := "/data/" + namespace + "/" + pvc
		if !strings.HasPrefix(path, wantPathPrefix) {
			t.Fatalf("path %q escaped the namespace subtree under spec %+v", path, spec)
		}
		// Prefix-freedom: a sibling namespace that extends this one can never be a path
		// prefix collision ("/data/tenant-a/" vs "/data/tenant-ab/").
		sibling := "/data/" + namespace + "b/" + pvc
		if strings.HasPrefix(sibling, wantPathPrefix) {
			t.Fatalf("sibling subtree %q collides with %q", sibling, wantPathPrefix)
		}
	}
}
