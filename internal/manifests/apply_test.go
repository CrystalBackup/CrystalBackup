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

package manifests

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
)

// writeTree lays out a one-manifest snapshot tree (index.json + the manifest) and returns its dir.
func writeTree(t *testing.T, manifest string) string {
	t.Helper()
	dir := t.TempDir()
	const rel = "core/persistentvolumeclaims/data.yaml"
	if err := os.MkdirAll(filepath.Join(dir, filepath.Dir(rel)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, rel), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	idx := `{"formatVersion":1,"clusterID":"c","namespace":"src","backupName":"b","capturedAt":"t",
	  "rulesetVersion":"1","resourceCount":1,
	  "resources":[{"group":"","version":"v1","kind":"PersistentVolumeClaim","name":"data",
	  "path":"` + rel + `"}]}`
	if err := os.WriteFile(filepath.Join(dir, IndexFileName), []byte(idx), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestPlanStripsFinalizers is the M3.2 regression guard for the leak that wedged a namespace in
// Terminating forever. Sanitization rule S8 now drops finalizers at CAPTURE time, but every
// snapshot taken before it still carries them — so the restore must refuse to transplant a claim
// no controller in the target cluster holds. The concrete case: a PVC captured inside the ~2 s
// window where the CSI snapshot-controller holds pvc-as-source-protection on a snapshot source.
func TestPlanStripsFinalizers(t *testing.T) {
	dir := writeTree(t, `apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: data
  finalizers:
    - snapshot.storage.kubernetes.io/pvc-as-source-protection
    - kubernetes.io/pvc-protection
spec:
  accessModes: [ReadWriteOnce]
  resources:
    requests:
      storage: 1Gi
  storageClassName: ceph-block
`)

	a := &Applier{}
	idx, err := a.readIndex(dir)
	if err != nil {
		t.Fatalf("readIndex: %v", err)
	}
	planned, report := a.plan(idx, ApplyOptions{SourceDir: dir, TargetNamespace: "target"})

	if report.Failed != 0 || len(planned) != 1 {
		t.Fatalf("plan = %d resources, %d failed; want 1 resource, 0 failed", len(planned), report.Failed)
	}
	obj := planned[0].obj
	if fin := obj.GetFinalizers(); len(fin) != 0 {
		t.Errorf("finalizers = %v, want none — a restored object must carry no claim the target "+
			"cluster cannot release (the namespace would never finish deleting)", fin)
	}
	// The transformation next to it must still happen, so the strip cannot be read as "plan
	// stopped early".
	if obj.GetNamespace() != "target" {
		t.Errorf("namespace = %q, want the restore target", obj.GetNamespace())
	}
}

// TestPlanLeavesFinalizerFreeObjectsAlone checks the strip does not invent an empty list, which
// would serialise as `finalizers: []` and churn every apply diff.
func TestPlanLeavesFinalizerFreeObjectsAlone(t *testing.T) {
	dir := writeTree(t, `apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: data
spec:
  accessModes: [ReadWriteOnce]
`)

	a := &Applier{}
	idx, err := a.readIndex(dir)
	if err != nil {
		t.Fatalf("readIndex: %v", err)
	}
	planned, _ := a.plan(idx, ApplyOptions{SourceDir: dir, TargetNamespace: "target"})
	if len(planned) != 1 {
		t.Fatalf("plan = %d resources, want 1", len(planned))
	}
	objectMeta, _ := planned[0].obj.Object["metadata"].(map[string]any)
	if _, present := objectMeta["finalizers"]; present {
		t.Error("metadata.finalizers was created by the strip; it must only ever remove")
	}
}

// applyTestMapper resolves just the two kinds these tests apply.
func applyTestMapper() meta.RESTMapper {
	m := meta.NewDefaultRESTMapper(nil)
	m.Add(schema.GroupVersionKind{Version: "v1", Kind: kindPVC}, meta.RESTScopeNamespace)
	m.Add(schema.GroupVersionKind{Version: "v1", Kind: kindConfigMap}, meta.RESTScopeNamespace)
	return m
}

func liveObj(kind, name, ns string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": kind,
		"metadata": map[string]any{"name": name, "namespace": ns, "uid": "uid-" + name},
	}}
}

// verbsOn returns the verbs the fake client saw for one resource.
func verbsOn(actions []clienttesting.Action, resource string) []string {
	var out []string
	for _, a := range actions {
		if a.GetResource().Resource == resource {
			out = append(out, a.GetVerb())
		}
	}
	return out
}

// TestRecreateNeverDeletesAVolumeClaim is the M3.2 guard against a DATA-LOSS bug, proven on the
// crucible before it was fixed: Recreate mode deleted the live PVC, its dynamically-provisioned PV
// (reclaimPolicy Delete) took the user's RBD image down with it, and the data restore — already
// mounting a twin of that very volumeHandle — hung forever on "internal RBD image not found".
//
// A PVC must therefore be reconciled in place, never deleted. The ConfigMap alongside it proves
// the guard is narrow: Recreate still means recreate for everything that is only a definition.
func TestRecreateNeverDeletesAVolumeClaim(t *testing.T) {
	const ns = "m2-restore"
	dyn := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(),
		liveObj(kindPVC, "data", ns), liveObj(kindConfigMap, "cfg", ns))
	a := &Applier{Dynamic: dyn, Mapper: applyTestMapper()}
	opts := ApplyOptions{TargetNamespace: ns, Mode: ModeRecreate}
	report := &Report{}

	for _, tc := range []struct{ kind, name string }{{kindPVC, "data"}, {kindConfigMap, "cfg"}} {
		p := &plannedResource{
			entry: IndexEntry{Version: "v1", Kind: tc.kind, Name: tc.name},
			obj:   liveObj(tc.kind, tc.name, ns),
		}
		a.applyOne(context.Background(), p, opts, report)
	}

	// THE assertion: the claim is reconciled in place (the apply reaches the server as a patch),
	// and no delete is ever issued for it. The apply itself errors under the fake dynamic client,
	// which does not implement server-side apply for unstructured objects — irrelevant here, since
	// what this test pins is which VERB the mode chose, and the real path is covered by the m2
	// crucible spec end to end.
	pvcVerbs := verbsOn(dyn.Actions(), "persistentvolumeclaims")
	if slicesContains(pvcVerbs, "delete") {
		t.Errorf("PVC verbs = %v; a Recreate restore must NEVER delete a PersistentVolumeClaim — "+
			"releasing it destroys the user's volume under a Delete reclaim policy", pvcVerbs)
	}
	if !slicesContains(pvcVerbs, "patch") {
		t.Errorf("PVC verbs = %v, want the in-place apply (patch) the guard reroutes to", pvcVerbs)
	}
	if got := verbsOn(dyn.Actions(), "configmaps"); !slicesContains(got, "delete") {
		t.Errorf("ConfigMap verbs = %v, want a delete — the guard must be narrow, not disable "+
			"Recreate for everything", got)
	}
}

func slicesContains(hay []string, needle string) bool {
	for _, s := range hay {
		if s == needle {
			return true
		}
	}
	return false
}

// TestIsStorageHandle pins the narrow set: only the two kinds that ARE a handle to data.
func TestIsStorageHandle(t *testing.T) {
	for _, tc := range []struct {
		group, kind string
		want        bool
	}{
		{"", kindPVC, true},
		{"", kindPersistentVolume, true},
		{"", kindConfigMap, false},
		{"apps", "StatefulSet", false},
		{"example.com", kindPVC, false}, // a CRD that merely reuses the name is not ours to spare
	} {
		if got := isStorageHandle(tc.group, tc.kind); got != tc.want {
			t.Errorf("isStorageHandle(%q, %q) = %v, want %v", tc.group, tc.kind, got, tc.want)
		}
	}
}

// TestRecreateConvergesWhenTheNameComesBack pins the third M3.2 restore bug: the control plane
// recreates a namespace's `default` ServiceAccount the instant it is deleted, so Recreate's
// create always found the replacement already there. That single object reported Failed and every
// whole-namespace Recreate restore came back PartiallyFailed — the crucible's m2 Recreate spec had
// never once passed. Reconciling in place converges on exactly what Recreate asked for.
func TestRecreateConvergesWhenTheNameComesBack(t *testing.T) {
	const ns = "m2-restore"
	dyn := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), liveObj(kindConfigMap, "cfg", ns))
	// Let the delete through, then have the object reappear before the create — the shape of an
	// auto-recreated control-plane object.
	dyn.PrependReactor("create", "configmaps", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewAlreadyExists(
			schema.GroupResource{Resource: "configmaps"}, "cfg")
	})

	a := &Applier{Dynamic: dyn, Mapper: applyTestMapper()}
	report := &Report{}
	p := &plannedResource{
		entry: IndexEntry{Version: "v1", Kind: kindConfigMap, Name: "cfg"},
		obj:   liveObj(kindConfigMap, "cfg", ns),
	}
	a.applyOne(context.Background(), p, ApplyOptions{TargetNamespace: ns, Mode: ModeRecreate}, report)

	verbs := verbsOn(dyn.Actions(), "configmaps")
	if !slicesContains(verbs, "delete") || !slicesContains(verbs, "create") {
		t.Fatalf("verbs = %v, want the Recreate delete+create attempt", verbs)
	}
	if !slicesContains(verbs, "patch") {
		t.Errorf("verbs = %v; an AlreadyExists create must fall back to the in-place apply, not "+
			"fail the resource (that is what made every namespace Recreate PartiallyFailed)", verbs)
	}
}
