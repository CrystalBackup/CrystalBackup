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
	"fmt"
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/version"
	discoveryfake "k8s.io/client-go/discovery/fake"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
	clocktesting "k8s.io/utils/clock/testing"

	"github.com/CrystalBackup/CrystalBackup/internal/sanitize"
)

// clusterObj builds a CLUSTER-scoped fixture: like obj() but with NO namespace, because a
// cluster-scoped object does not have one (setting one would be a lie the sanitizer would then
// have to strip).
func clusterObj(apiVersion, kind, name string, fields map[string]any) *unstructured.Unstructured {
	o := map[string]any{
		"apiVersion": apiVersion,
		"kind":       kind,
		"metadata": map[string]any{
			"name":            name,
			"uid":             "uid-" + name,
			"resourceVersion": "1",
		},
	}
	for k, v := range fields {
		o[k] = v
	}
	return &unstructured.Unstructured{Object: o}
}

// clusterListKinds names the List type of each cluster-scoped resource the fake dynamic client
// serves. It is also what maps an added object's GVK to its GVR (the fake derives the GVK from
// each ListKind), so every kind a test constructs the client with must appear here.
func clusterListKinds() map[schema.GroupVersionResource]string {
	return map[schema.GroupVersionResource]string{
		{Version: "v1", Resource: "persistentvolumes"}:                                        "PersistentVolumeList",
		{Group: "storage.k8s.io", Version: "v1", Resource: "storageclasses"}:                  "StorageClassList",
		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"}:         "ClusterRoleList",
		{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}: "CustomResourceDefinitionList",
	}
}

// clusterAPIResources is the discovery fixture. It deliberately mixes what must be captured
// (cluster-scoped, listable, allow-listed) with what must be skipped for each distinct reason:
// a namespaced kind (belongs to the namespace plane), a subresource, a non-listable kind, and a
// cluster-scoped kind that is not in the curated allow-list.
func clusterAPIResources() []*metav1.APIResourceList {
	rw := []string{"get", "list", "watch"}
	return []*metav1.APIResourceList{
		{
			GroupVersion: "v1",
			APIResources: []metav1.APIResource{
				{Name: "persistentvolumes", Namespaced: false, Kind: "PersistentVolume", Verbs: rw},
				// Must be skipped: cluster-scoped but not in the curated allow-list (adr/0011 §1).
				{Name: "nodes", Namespaced: false, Kind: "Node", Verbs: rw},
				// Must be skipped: namespaced belongs to the namespace plane (dump.go), not here.
				{Name: "configmaps", Namespaced: true, Kind: "ConfigMap", Verbs: rw},
				// Must be skipped: a subresource, not an object.
				{Name: "persistentvolumes/status", Namespaced: false, Kind: "PersistentVolume", Verbs: rw},
				// Must be skipped: not listable.
				{Name: "componentstatuses", Namespaced: false, Kind: "ComponentStatus", Verbs: []string{"get"}},
			},
		},
		{
			GroupVersion: "storage.k8s.io/v1",
			APIResources: []metav1.APIResource{
				{Name: "storageclasses", Namespaced: false, Kind: "StorageClass", Verbs: rw},
			},
		},
		{
			GroupVersion: "rbac.authorization.k8s.io/v1",
			APIResources: []metav1.APIResource{
				{Name: "clusterroles", Namespaced: false, Kind: "ClusterRole", Verbs: rw},
			},
		},
		{
			GroupVersion: "apiextensions.k8s.io/v1",
			APIResources: []metav1.APIResource{
				{Name: "customresourcedefinitions", Namespaced: false, Kind: "CustomResourceDefinition", Verbs: rw},
			},
		},
	}
}

func newClusterDumper(
	t *testing.T, opts ClusterCaptureOptions, objs ...runtime.Object,
) (*ClusterDumper, *dynamicfake.FakeDynamicClient) {
	t.Helper()
	s, err := sanitize.New()
	if err != nil {
		t.Fatalf("sanitize.New: %v", err)
	}
	sel, err := CompileClusterSelector(opts)
	if err != nil {
		t.Fatalf("CompileClusterSelector: %v", err)
	}
	scheme := runtime.NewScheme()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, clusterListKinds(), objs...)
	disco := &fakeDiscovery{
		FakeDiscovery: &discoveryfake.FakeDiscovery{
			Fake:               &clienttesting.Fake{Resources: clusterAPIResources()},
			FakedServerVersion: &version.Info{GitVersion: "v1.36.2"},
		},
		preferred: clusterAPIResources(),
	}
	return &ClusterDumper{
		Disco:     disco,
		Dynamic:   dyn,
		Sanitizer: s,
		Selector:  sel,
		Clock:     clocktesting.NewFakePassiveClock(fixedTime),
	}, dyn
}

func defaultClusterOpts() ClusterOptions {
	return ClusterOptions{ClusterID: "prod-eu-1", BackupName: "dr-daily-20260712-010000"}
}

func TestClusterDumpLayoutAndIndex(t *testing.T) {
	d, _ := newClusterDumper(t, ClusterCaptureOptions{},
		clusterObj("storage.k8s.io/v1", "StorageClass", "fast", map[string]any{
			"provisioner": "rook-ceph.rbd.csi.ceph.com",
		}),
		clusterObj("v1", "PersistentVolume", "pv-1", map[string]any{
			"spec": map[string]any{
				"capacity": map[string]any{"storage": "10Gi"},
				"claimRef": map[string]any{
					"namespace":       "team-x",
					"name":            "data",
					"uid":             "claim-uid-abc",
					"resourceVersion": "999",
				},
			},
		}),
		clusterObj("rbac.authorization.k8s.io/v1", "ClusterRole", "app-operator", nil),
		clusterObj("apiextensions.k8s.io/v1", "CustomResourceDefinition", "widgets.example.com", nil),
		// A control-plane ClusterRole: its kind IS listed, but SelectsObject must drop it.
		clusterObj("rbac.authorization.k8s.io/v1", "ClusterRole", "system:node", nil),
	)
	dir := t.TempDir()
	idx, err := d.Dump(context.Background(), defaultClusterOpts(), dir)
	if err != nil {
		t.Fatalf("Dump: %v", err)
	}

	tree := readTree(t, dir)
	for _, want := range []string{
		IndexFileName,
		"storage.k8s.io/StorageClass/fast.yaml",
		"core/PersistentVolume/pv-1.yaml",
		"rbac.authorization.k8s.io/ClusterRole/app-operator.yaml",
		"apiextensions.k8s.io/CustomResourceDefinition/widgets.example.com.yaml",
	} {
		if _, ok := tree[want]; !ok {
			t.Errorf("missing %s; tree = %v", want, slices.Sorted(maps(tree)))
		}
	}
	// The system: ClusterRole is the SelectsObject integration: its kind was listed, the object
	// was dropped. Restoring the control plane's own RBAC means fighting the API server.
	if _, ok := tree["rbac.authorization.k8s.io/ClusterRole/system:node.yaml"]; ok {
		t.Error("system:node ClusterRole was captured; the control-plane exclusion must drop it")
	}

	if idx.ResourceCount != 4 {
		t.Errorf("resourceCount = %d, want 4", idx.ResourceCount)
	}
	// A cluster-manifests snapshot belongs to no namespace (adr/0011); its index must say so.
	if idx.Namespace != "" {
		t.Errorf("namespace = %q, want empty — a cluster-manifests capture belongs to no namespace", idx.Namespace)
	}
	if idx.ClusterID != "prod-eu-1" {
		t.Errorf("clusterID = %q, want prod-eu-1", idx.ClusterID)
	}
	if idx.KubernetesVersion != "v1.36.2" {
		t.Errorf("kubernetesVersion = %q", idx.KubernetesVersion)
	}
	if idx.CapturedAt != "2026-07-12T01:00:00Z" {
		t.Errorf("capturedAt = %q; the clock must be injectable", idx.CapturedAt)
	}
	if idx.RulesetVersion == "" {
		t.Error("rulesetVersion is empty; a restore cannot tell which rules produced this")
	}

	// The stored PV must already be sanitized — a restore does no stripping. S1 strips
	// metadata.uid; S20 strips spec.claimRef.uid but keeps the rest of claimRef so the PV can
	// rebind to the restored PVC.
	pv := tree["core/PersistentVolume/pv-1.yaml"]
	if strings.Contains(pv, "uid-pv-1") {
		t.Error("PV kept its metadata.uid; the stored manifest must already be clean")
	}
	if strings.Contains(pv, "claim-uid-abc") {
		t.Error("PV kept spec.claimRef.uid; S20 must strip it or the PV binds to nothing")
	}
	if !strings.Contains(pv, "team-x") || !strings.Contains(pv, "data") {
		t.Errorf("PV lost its claimRef namespace/name; those are kept for rebinding:\n%s", pv)
	}
}

// Dump the same cluster twice and assert byte-identical output. Unstable bytes defeat restic's
// dedup, so a cluster-manifests snapshot would cost fresh storage on every run even when nothing
// changed (R13), exactly as for the namespaced dump.
func TestClusterDumpIsByteDeterministic(t *testing.T) {
	build := func() map[string]string {
		d, _ := newClusterDumper(t, ClusterCaptureOptions{},
			clusterObj("storage.k8s.io/v1", "StorageClass", "fast", nil),
			clusterObj("storage.k8s.io/v1", "StorageClass", "slow", nil),
			clusterObj("v1", "PersistentVolume", "pv-1", nil),
			clusterObj("rbac.authorization.k8s.io/v1", "ClusterRole", "app-operator", nil),
			clusterObj("apiextensions.k8s.io/v1", "CustomResourceDefinition", "widgets.example.com", nil),
		)
		dir := t.TempDir()
		if _, err := d.Dump(context.Background(), defaultClusterOpts(), dir); err != nil {
			t.Fatalf("Dump: %v", err)
		}
		return readTree(t, dir)
	}
	first := build()
	for i := range 3 {
		next := build()
		if len(first) != len(next) {
			t.Fatalf("run %d produced %d files, first produced %d", i, len(next), len(first))
		}
		for path, want := range first {
			if next[path] != want {
				t.Fatalf("run %d differs at %s\n--- first ---\n%s\n--- next ---\n%s", i, path, want, next[path])
			}
		}
	}
}

// The namespaced==false filter AND the SelectsKind pre-skip, proven at the wire: the dynamic
// client must never be asked to List a namespaced kind, a subresource, a non-listable kind, or a
// cluster-scoped kind outside the allow-list. Skipping them before the List is the difference
// between one API call and paging thousands of objects nobody wants (adr/0011).
func TestClusterDumpSkipsNamespacedSubresourceAndUnselectedKinds(t *testing.T) {
	d, dyn := newClusterDumper(t, ClusterCaptureOptions{},
		clusterObj("storage.k8s.io/v1", "StorageClass", "only", nil))
	if _, err := d.Dump(context.Background(), defaultClusterOpts(), t.TempDir()); err != nil {
		t.Fatalf("Dump: %v", err)
	}
	for _, action := range dyn.Actions() {
		switch action.GetResource().Resource {
		case "configmaps":
			t.Error("listed a namespaced resource; that belongs to the namespace plane (dump.go)")
		case "persistentvolumes/status":
			t.Error("listed a subresource")
		case "componentstatuses":
			t.Error("listed a resource that does not support list")
		case "nodes":
			t.Error("listed a cluster-scoped kind outside the allow-list; SelectsKind must skip it before List")
		}
	}
}

// Partial discovery is survivable: one broken aggregated API must not cost the whole
// cluster-manifests capture. Whatever discovery returned is still captured, and the failure is
// recorded as a warning — which is exactly what the shim turns into MoverResult.IncompleteManifests.
func TestClusterDumpPartialDiscoveryWarnsAndContinues(t *testing.T) {
	d, _ := newClusterDumper(t, ClusterCaptureOptions{},
		clusterObj("storage.k8s.io/v1", "StorageClass", "fast", nil))
	d.Disco.(*fakeDiscovery).err = fmt.Errorf("aggregated API unavailable")

	dir := t.TempDir()
	idx, err := d.Dump(context.Background(), defaultClusterOpts(), dir)
	if err != nil {
		t.Fatalf("Dump must survive partial discovery: %v", err)
	}
	if _, ok := readTree(t, dir)["storage.k8s.io/StorageClass/fast.yaml"]; !ok {
		t.Error("what discovery DID return must still be captured")
	}
	if len(idx.Warnings) == 0 {
		t.Error("partial discovery must be recorded as a warning; it is what drives IncompleteManifests")
	}
}

// A total discovery failure (nil lists) is a hard error, not a silent empty snapshot: there is
// nothing to capture, and reporting success would produce a snapshot that restores to nothing.
func TestClusterDumpTotalDiscoveryFailureIsHardError(t *testing.T) {
	d, _ := newClusterDumper(t, ClusterCaptureOptions{})
	fd := d.Disco.(*fakeDiscovery)
	fd.preferred = nil
	fd.err = fmt.Errorf("apiserver down")

	// enumerate turns a nil-lists discovery failure into a single warning and no resources; the
	// dump still writes an index, but it carries the failure rather than masking it.
	idx, err := d.Dump(context.Background(), defaultClusterOpts(), t.TempDir())
	if err != nil {
		t.Fatalf("Dump: %v", err)
	}
	if idx.ResourceCount != 0 {
		t.Errorf("resourceCount = %d, want 0 on total discovery failure", idx.ResourceCount)
	}
	if len(idx.Warnings) == 0 {
		t.Error("a total discovery failure must surface as a warning, not an empty success")
	}
}

// A kind that cannot be listed must cost that kind, not the whole capture. Losing one CRD's
// objects is bad; losing every cluster-scoped manifest because one aggregated API is down is worse.
func TestClusterDumpRecordsListFailureAsWarningAndContinues(t *testing.T) {
	d, dyn := newClusterDumper(t, ClusterCaptureOptions{},
		clusterObj("storage.k8s.io/v1", "StorageClass", "survivor", nil))
	dyn.PrependReactor("list", "clusterroles", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.NewForbidden(
			schema.GroupResource{Group: "rbac.authorization.k8s.io", Resource: "clusterroles"}, "",
			fmt.Errorf("RBAC says no"))
	})

	dir := t.TempDir()
	idx, err := d.Dump(context.Background(), defaultClusterOpts(), dir)
	if err != nil {
		t.Fatalf("Dump must not fail because one kind is unreadable: %v", err)
	}
	if _, ok := readTree(t, dir)["storage.k8s.io/StorageClass/survivor.yaml"]; !ok {
		t.Error("the readable kinds must still be captured")
	}

	var found bool
	for _, w := range idx.Warnings {
		if strings.Contains(w.Message, "RBAC says no") {
			found = true
		}
	}
	if !found {
		t.Errorf("the failure must be recorded so a restore can tell 'no ClusterRoles existed' from "+
			"'we could not read the ClusterRoles'; warnings = %v", idx.Warnings)
	}
}

// An explicit include narrows the capture to exactly what it names (the ClusterSelector's job):
// a StorageClass include must capture StorageClasses and NOT the rest of the default allow-list.
func TestClusterDumpHonoursExplicitInclude(t *testing.T) {
	d, _ := newClusterDumper(t, ClusterCaptureOptions{Include: []string{"storage.k8s.io/StorageClass"}},
		clusterObj("storage.k8s.io/v1", "StorageClass", "fast", nil),
		clusterObj("v1", "PersistentVolume", "pv-1", nil),
		clusterObj("rbac.authorization.k8s.io/v1", "ClusterRole", "app-operator", nil),
	)
	dir := t.TempDir()
	idx, err := d.Dump(context.Background(), defaultClusterOpts(), dir)
	if err != nil {
		t.Fatalf("Dump: %v", err)
	}
	tree := readTree(t, dir)
	if _, ok := tree["storage.k8s.io/StorageClass/fast.yaml"]; !ok {
		t.Error("the included StorageClass must be captured")
	}
	for _, gone := range []string{"core/PersistentVolume/pv-1.yaml", "rbac.authorization.k8s.io/ClusterRole/app-operator.yaml"} {
		if _, ok := tree[gone]; ok {
			t.Errorf("%s captured; an explicit include must replace the default allow-list, not extend it", gone)
		}
	}
	if idx.ResourceCount != 1 {
		t.Errorf("resourceCount = %d, want 1 (only the included StorageClass)", idx.ResourceCount)
	}
}

func TestClusterDumpRejectsUnusableConfig(t *testing.T) {
	base, _ := newClusterDumper(t, ClusterCaptureOptions{})
	noSanitizer := &ClusterDumper{Disco: base.Disco, Dynamic: base.Dynamic, Selector: base.Selector, Clock: base.Clock}
	if _, err := noSanitizer.Dump(context.Background(), defaultClusterOpts(), t.TempDir()); err == nil {
		t.Error("expected an error with no sanitizer")
	}
	noSelector := &ClusterDumper{Disco: base.Disco, Dynamic: base.Dynamic, Sanitizer: base.Sanitizer, Clock: base.Clock}
	if _, err := noSelector.Dump(context.Background(), defaultClusterOpts(), t.TempDir()); err == nil {
		t.Error("expected an error with no selector")
	}
}
