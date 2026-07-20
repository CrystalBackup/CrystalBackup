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
	"testing"
	"time"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/manifests"
	"github.com/CrystalBackup/CrystalBackup/internal/mover"
	"github.com/CrystalBackup/CrystalBackup/internal/restic"
)

func manifestSnap(id, path string, at int64) restic.Snapshot {
	return restic.Snapshot{
		ID: id, Time: time.Unix(at, 0), Paths: []string{path},
		Tags: []string{restic.Tag(restic.TagKeyKind, restic.KindManifests)},
	}
}

// TestResolveResourcesPlanTriState is the load-bearing one. spec/02-api.md distinguishes an
// OMITTED resources field (restore every manifest) from a PRESENT EMPTY one (restore none),
// and Go's zero value plus JSON omitempty conspire to erase that difference. Getting it
// backwards means either silently restoring nothing, or applying a whole namespace over a
// live one in Overwrite mode — from a CR that asked for neither.
func TestResolveResourcesPlanTriState(t *testing.T) {
	snaps := []restic.Snapshot{
		manifestSnap("m1", "/manifests/team-x", 100),
		{ID: "d1", Time: time.Unix(100, 0), Tags: []string{
			restic.Tag(restic.TagKeyKind, restic.KindData), restic.Tag(restic.TagKeyPVC, "data")}},
	}

	t.Run("omitted restores everything", func(t *testing.T) {
		plan, _ := resolveResourcesPlan(nil, "Overwrite", false, snaps, nil)
		if plan == nil {
			t.Fatal("plan = nil; an omitted resources field restores the whole namespace")
		}
		if !plan.selection.All {
			t.Error("selection.All = false, want the whole snapshot selected")
		}
	})

	t.Run("a present empty list restores nothing", func(t *testing.T) {
		plan, ok := resolveResourcesPlan([]cbv1.ResourceSelectorItem{}, "Overwrite", false, snaps, nil)
		if plan != nil {
			t.Errorf("plan = %+v, want nil: `resources: []` is a deliberate no-manifests", plan)
		}
		if !ok {
			t.Error("ok = false; asking for no manifests is a valid, settled outcome")
		}
	})

	t.Run("a non-empty list carries its items", func(t *testing.T) {
		plan, _ := resolveResourcesPlan([]cbv1.ResourceSelectorItem{
			{Include: []string{"apps/Deployment"}},
		}, "Recreate", false, snaps, nil)
		if plan == nil {
			t.Fatal("plan = nil")
		}
		if plan.selection.All {
			t.Error("selection.All = true; a listed selection must not widen to everything")
		}
		if len(plan.selection.Items) != 1 || len(plan.selection.Items[0].Include) != 1 {
			t.Errorf("items = %+v, want the single include carried through", plan.selection.Items)
		}
		if plan.mode != "Recreate" {
			t.Errorf("mode = %q, want Recreate", plan.mode)
		}
	})
}

// TestResolveResourcesPlanWithoutASnapshot pins the shape of a run captured with
// includeManifests: false. There is nothing to restore, and that is not an error — the user
// asked for the volumes of a run that never had a manifest half.
func TestResolveResourcesPlanWithoutASnapshot(t *testing.T) {
	dataOnly := []restic.Snapshot{{ID: "d1", Time: time.Unix(100, 0), Tags: []string{
		restic.Tag(restic.TagKeyKind, restic.KindData), restic.Tag(restic.TagKeyPVC, "data")}}}

	plan, ok := resolveResourcesPlan(nil, "Overwrite", false, dataOnly, nil)
	if plan != nil {
		t.Errorf("plan = %+v, want nil when the run has no manifest snapshot", plan)
	}
	if ok {
		t.Error("ok = true; a missing snapshot is not the same answer as a deliberate opt-out")
	}
}

// TestResolveResourcesPlanReadsTheRecordedPath pins that the subtree comes off the SNAPSHOT,
// not from rebuilding "/manifests/<ns>" out of the CR. A ClusterRestore reads another
// cluster's namespace into a differently-named one, and a rebuilt path would silently point at
// a subtree that does not exist — restic would restore nothing and the apply would find an
// empty tree, which looks exactly like a namespace that had no manifests.
func TestResolveResourcesPlanReadsTheRecordedPath(t *testing.T) {
	plan, _ := resolveResourcesPlan(nil, "Overwrite", false,
		[]restic.Snapshot{manifestSnap("m1", "/manifests/origin-ns", 100)}, nil)
	if plan == nil {
		t.Fatal("plan = nil")
	}
	if plan.snapshotPath != "/manifests/origin-ns" {
		t.Errorf("snapshotPath = %q, want the path the snapshot recorded", plan.snapshotPath)
	}
}

// TestManifestsSnapshotPicksTheNewest guards the same newest-wins rule the data path uses: a
// run name can be reused, and pairing a fresh restore with a stale capture would restore last
// week's namespace without saying so.
func TestManifestsSnapshotPicksTheNewest(t *testing.T) {
	got, ok := manifestsSnapshot([]restic.Snapshot{
		manifestSnap("old", "/manifests/x", 100),
		manifestSnap("new", "/manifests/x", 200),
	})
	if !ok || got.ID != "new" {
		t.Errorf("manifestsSnapshot() = %q/%v, want new/true", got.ID, ok)
	}
}

// TestResourcesJobEnvCarriesTheContract pins what the mover is actually told. Every one of
// these is a silent-wrong-answer if it goes missing: no restore dir and the apply reads an
// empty tree; no selection and it applies the whole namespace; no mode and it falls out of the
// Overwrite/Recreate switch.
func TestResourcesJobEnvCarriesTheContract(t *testing.T) {
	env, err := resourcesJobEnv("team-x", &resourcesPlan{
		snapshotID: "abc", snapshotPath: "/manifests/team-x",
		mode: "Overwrite", dryRun: true,
		selection:           manifests.Selection{Items: []manifests.SelectionItem{{Include: []string{"apps/Deployment"}}}},
		storageClassMapping: map[string]string{"fast": "standard"},
	})
	if err != nil {
		t.Fatalf("resourcesJobEnv() = %v", err)
	}

	byName := map[string]string{}
	for _, e := range env {
		byName[e.Name] = e.Value
	}
	for _, name := range []string{
		mover.EnvManifestsNamespace,
		mover.EnvManifestsRestoreDir,
		mover.EnvManifestsMode,
		mover.EnvManifestsSelection,
		mover.EnvManifestsDryRun,
		mover.EnvManifestsStorageClassMapping,
	} {
		if byName[name] == "" {
			t.Errorf("%s is unset", name)
		}
	}
	if byName[mover.EnvManifestsRestoreDir] != mover.ManifestsRestoreDir {
		t.Errorf("restore dir = %q, want %q", byName[mover.EnvManifestsRestoreDir], mover.ManifestsRestoreDir)
	}

	// The selection must survive the trip intact — this is the one env value whose corruption
	// widens a narrow restore instead of failing it.
	decoded, err := manifests.DecodeSelection(byName[mover.EnvManifestsSelection])
	if err != nil {
		t.Fatalf("DecodeSelection() = %v", err)
	}
	if decoded.All || len(decoded.Items) != 1 {
		t.Errorf("decoded selection = %+v, want the single item, not All", decoded)
	}
}

// TestResourcesJobEnvOmitsWhatWasNotAsked keeps a namespaced Restore's Job free of a
// storageClassMapping it has no field for (04-manifest-backup.md §5.3) and of a dry-run flag
// nobody set — an env var that exists is one a future reader has to reason about.
func TestResourcesJobEnvOmitsWhatWasNotAsked(t *testing.T) {
	env, err := resourcesJobEnv("team-x", &resourcesPlan{
		mode: "Overwrite", selection: manifests.Selection{All: true},
	})
	if err != nil {
		t.Fatalf("resourcesJobEnv() = %v", err)
	}
	for _, e := range env {
		if e.Name == mover.EnvManifestsStorageClassMapping || e.Name == mover.EnvManifestsDryRun {
			t.Errorf("%s is set to %q, want it absent", e.Name, e.Value)
		}
	}
}

// TestResourcesStatusFromCapsTheReport pins the SECOND, independent bound on the report. The
// mover already trimmed to the 4096-byte termination message; this cap protects etcd, where a
// status too large to write loses the whole report rather than its tail.
func TestResourcesStatusFromCapsTheReport(t *testing.T) {
	entries := make([]mover.ResourceEntry, 0, cbv1.MaxRestoreResourceEntries+40)
	for i := range cbv1.MaxRestoreResourceEntries + 40 {
		changed := make([]string, cbv1.MaxRestoreChangedPaths+5)
		for j := range changed {
			changed[j] = "spec.field"
		}
		entries = append(entries, mover.ResourceEntry{
			Group: "apps", Kind: "Deployment", Name: string(rune('a' + i%26)),
			Outcome: "Configured", Changed: changed,
		})
	}

	got := resourcesStatusFrom(mover.MoverResult{RestoredResources: 500, ResourceEntries: entries})

	if len(got.Entries) > cbv1.MaxRestoreResourceEntries {
		t.Errorf("entries = %d, want at most %d", len(got.Entries), cbv1.MaxRestoreResourceEntries)
	}
	for _, e := range got.Entries {
		if len(e.Changed) > cbv1.MaxRestoreChangedPaths {
			t.Errorf("changed paths = %d, want at most %d", len(e.Changed), cbv1.MaxRestoreChangedPaths)
		}
	}
	if !got.Truncated {
		t.Error("Truncated = false; a capped report must say it dropped something")
	}
}
