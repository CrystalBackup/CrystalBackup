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
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/version"
	discoveryfake "k8s.io/client-go/discovery/fake"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/utils/clock"
	clocktesting "k8s.io/utils/clock/testing"

	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
	"github.com/CrystalBackup/CrystalBackup/internal/sanitize"
)

const testNamespace = "c-team-x"

var fixedTime = time.Date(2026, 7, 12, 1, 0, 0, 0, time.UTC)

func obj(apiVersion, kind, name string, fields map[string]any) *unstructured.Unstructured {
	o := map[string]any{
		"apiVersion": apiVersion,
		"kind":       kind,
		"metadata": map[string]any{
			"name":            name,
			"namespace":       testNamespace,
			"uid":             "uid-" + name,
			"resourceVersion": "1",
		},
	}
	for k, v := range fields {
		o[k] = v
	}
	return &unstructured.Unstructured{Object: o}
}

// listKinds tells the fake dynamic client how to name the List type of each resource; without
// it the fake panics rather than returning an empty list.
func listKinds() map[schema.GroupVersionResource]string {
	return map[schema.GroupVersionResource]string{
		{Version: "v1", Resource: "services"}:                      "ServiceList",
		{Version: "v1", Resource: "configmaps"}:                    "ConfigMapList",
		{Version: "v1", Resource: "secrets"}:                       "SecretList",
		{Version: "v1", Resource: "pods"}:                          "PodList",
		{Version: "v1", Resource: "endpoints"}:                     "EndpointsList",
		{Group: "apps", Version: "v1", Resource: "deployments"}:    "DeploymentList",
		{Group: "batch", Version: "v1", Resource: "cronjobs"}:      "CronJobList",
		{Group: "example.com", Version: "v1", Resource: "widgets"}: "WidgetList",
	}
}

func apiResources() []*metav1.APIResourceList {
	rw := []string{"get", "list", "watch"}
	return []*metav1.APIResourceList{
		{
			GroupVersion: "v1",
			APIResources: []metav1.APIResource{
				{Name: "services", Namespaced: true, Kind: "Service", Verbs: rw},
				{Name: "configmaps", Namespaced: true, Kind: "ConfigMap", Verbs: rw},
				{Name: "secrets", Namespaced: true, Kind: "Secret", Verbs: rw},
				{Name: "pods", Namespaced: true, Kind: "Pod", Verbs: rw},
				{Name: "endpoints", Namespaced: true, Kind: "Endpoints", Verbs: rw},
				// Must be skipped: a subresource, not an object.
				{Name: "pods/log", Namespaced: true, Kind: "Pod", Verbs: rw},
				// Must be skipped: cluster-scoped belongs to the cluster plane (adr/0011).
				{Name: "nodes", Namespaced: false, Kind: "Node", Verbs: rw},
				// Must be skipped: not listable.
				{Name: "bindings", Namespaced: true, Kind: "Binding", Verbs: []string{"create"}},
			},
		},
		{
			GroupVersion: "apps/v1",
			APIResources: []metav1.APIResource{
				{Name: "deployments", Namespaced: true, Kind: "Deployment", Verbs: rw},
			},
		},
		{
			GroupVersion: "example.com/v1",
			APIResources: []metav1.APIResource{
				{Name: "widgets", Namespaced: true, Kind: "Widget", Verbs: rw},
			},
		},
	}
}

// client-go's FakeDiscovery stubs ServerPreferredResources to (nil, nil) — a harness gap, not
// a statement about the API. Since preferred-version selection is client-go's job and not
// ours, standing in a fixture that already carries one version per group is faithful; the
// real call is exercised against a live API server by the envtest suite.
type fakeDiscovery struct {
	*discoveryfake.FakeDiscovery
	preferred []*metav1.APIResourceList
	err       error
}

func (f *fakeDiscovery) ServerPreferredResources() ([]*metav1.APIResourceList, error) {
	return f.preferred, f.err
}

func newDumper(t *testing.T, objs ...runtime.Object) (*Dumper, *dynamicfake.FakeDynamicClient) {
	t.Helper()
	s, err := sanitize.New()
	if err != nil {
		t.Fatalf("sanitize.New: %v", err)
	}
	scheme := runtime.NewScheme()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, listKinds(), objs...)
	disco := &fakeDiscovery{
		FakeDiscovery: &discoveryfake.FakeDiscovery{
			Fake:               &clienttesting.Fake{Resources: apiResources()},
			FakedServerVersion: &version.Info{GitVersion: "v1.36.2"},
		},
		preferred: apiResources(),
	}
	return &Dumper{
		Disco:     disco,
		Dynamic:   dyn,
		Sanitizer: s,
		Clock:     clocktesting.NewFakePassiveClock(fixedTime),
	}, dyn
}

func defaultOpts() Options {
	return Options{Namespace: testNamespace, ClusterID: "prod-eu-1", BackupName: "dr-daily-20260712-010000"}
}

func readTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(p) //nolint:gosec // test-controlled path
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = string(b)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return out
}

func TestDumpLayoutAndIndex(t *testing.T) {
	d, _ := newDumper(t,
		obj("v1", "Service", "web", map[string]any{
			"spec": map[string]any{"clusterIP": "10.43.0.1", "selector": map[string]any{"app": "web"}},
		}),
		obj("v1", "ConfigMap", "app-config", map[string]any{"data": map[string]any{"K": "V"}}),
		obj("apps/v1", "Deployment", "web", map[string]any{"spec": map[string]any{"replicas": int64(2)}}),
		obj("example.com/v1", "Widget", "thing", map[string]any{"spec": map[string]any{"size": "L"}}),
	)
	dir := t.TempDir()
	idx, err := d.Dump(context.Background(), defaultOpts(), dir)
	if err != nil {
		t.Fatalf("Dump: %v", err)
	}

	tree := readTree(t, dir)
	for _, want := range []string{
		IndexFileName,
		"core/Service/web.yaml",
		"core/ConfigMap/app-config.yaml",
		"apps/Deployment/web.yaml",
		"example.com/Widget/thing.yaml",
	} {
		if _, ok := tree[want]; !ok {
			t.Errorf("missing %s; tree = %v", want, slices.Sorted(maps(tree)))
		}
	}

	if idx.ResourceCount != 4 {
		t.Errorf("resourceCount = %d, want 4", idx.ResourceCount)
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

	// The stored manifest must already be sanitized — a restore does no stripping.
	svc := tree["core/Service/web.yaml"]
	if strings.Contains(svc, "clusterIP") {
		t.Error("Service kept its clusterIP; the stored manifest must already be clean")
	}
	if strings.Contains(svc, "uid:") || strings.Contains(svc, "resourceVersion") {
		t.Error("Service kept server-owned identity fields")
	}
	if strings.Contains(svc, "namespace:") {
		t.Error("Service kept its namespace; stripping it is what enables cross-namespace restore")
	}
}

func maps[K comparable, V any](m map[K]V) func(func(K) bool) {
	return func(yield func(K) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

// §6 gate: dump the same namespace twice and assert byte-identical output. Unstable bytes
// defeat restic's dedup, so a manifest snapshot would cost fresh storage on every run even
// when nothing changed (R13).
func TestDumpIsByteDeterministic(t *testing.T) {
	build := func() map[string]string {
		d, _ := newDumper(t,
			obj("v1", "Service", "web", map[string]any{"spec": map[string]any{"clusterIP": "10.43.0.1"}}),
			obj("v1", "ConfigMap", "a", map[string]any{"data": map[string]any{"x": "1"}}),
			obj("v1", "ConfigMap", "b", map[string]any{"data": map[string]any{"y": "2"}}),
			obj("apps/v1", "Deployment", "web", map[string]any{"spec": map[string]any{"replicas": int64(1)}}),
			obj("example.com/v1", "Widget", "w", map[string]any{"spec": map[string]any{"k": "v"}}),
		)
		dir := t.TempDir()
		if _, err := d.Dump(context.Background(), defaultOpts(), dir); err != nil {
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

func TestDumpSkipsSubresourcesClusterScopedAndUnlistable(t *testing.T) {
	d, dyn := newDumper(t, obj("v1", "ConfigMap", "only", nil))
	if _, err := d.Dump(context.Background(), defaultOpts(), t.TempDir()); err != nil {
		t.Fatalf("Dump: %v", err)
	}
	for _, action := range dyn.Actions() {
		res := action.GetResource().Resource
		switch res {
		case "pods/log":
			t.Error("listed a subresource")
		case "nodes":
			t.Error("listed a cluster-scoped resource; that belongs to the cluster plane (adr/0011)")
		case "bindings":
			t.Error("listed a resource that does not support list")
		}
	}
}

// A kind that cannot be listed must cost that kind, not the namespace. Losing one CRD's
// objects is bad; losing every manifest because one aggregated API is down is worse.
func TestDumpRecordsListFailureAsWarningAndContinues(t *testing.T) {
	d, dyn := newDumper(t,
		obj("v1", "ConfigMap", "survivor", map[string]any{"data": map[string]any{"k": "v"}}),
	)
	dyn.PrependReactor("list", "widgets", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.NewForbidden(
			schema.GroupResource{Group: "example.com", Resource: "widgets"}, "", fmt.Errorf("RBAC says no"))
	})

	dir := t.TempDir()
	idx, err := d.Dump(context.Background(), defaultOpts(), dir)
	if err != nil {
		t.Fatalf("Dump must not fail because one kind is unreadable: %v", err)
	}
	if _, ok := readTree(t, dir)["core/ConfigMap/survivor.yaml"]; !ok {
		t.Error("the readable kinds must still be captured")
	}

	var found bool
	for _, w := range idx.Warnings {
		if strings.Contains(w.Message, "RBAC says no") {
			found = true
		}
	}
	if !found {
		t.Errorf("the failure must be recorded so a restore can tell "+
			"'no widgets existed' from 'we could not read the widgets'; warnings = %v", idx.Warnings)
	}
}

func TestDumpAppliesExclusions(t *testing.T) {
	controller := true
	ownedPod := obj("v1", "Pod", "web-abc", nil)
	ownedPod.SetOwnerReferences([]metav1.OwnerReference{
		{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "web", UID: "rs", Controller: &controller},
	})
	saToken := obj("v1", "Secret", "default-token-x", map[string]any{"type": "kubernetes.io/service-account-token"})
	rootCA := obj("v1", "ConfigMap", "kube-root-ca.crt", map[string]any{"data": map[string]any{"ca.crt": "x"}})

	d, _ := newDumper(t,
		ownedPod, saToken, rootCA,
		obj("v1", "Pod", "naked", nil),
		obj("v1", "Secret", "db-creds", map[string]any{"type": "Opaque"}),
	)
	dir := t.TempDir()
	if _, err := d.Dump(context.Background(), defaultOpts(), dir); err != nil {
		t.Fatalf("Dump: %v", err)
	}
	tree := readTree(t, dir)
	for _, gone := range []string{"core/Pod/web-abc.yaml", "core/Secret/default-token-x.yaml", "core/ConfigMap/kube-root-ca.crt.yaml"} {
		if _, ok := tree[gone]; ok {
			t.Errorf("%s should have been excluded", gone)
		}
	}
	for _, kept := range []string{"core/Pod/naked.yaml", "core/Secret/db-creds.yaml"} {
		if _, ok := tree[kept]; !ok {
			t.Errorf("%s must be kept — dropping it would be silent data loss", kept)
		}
	}
}

// E4 needs a namespace-wide fact. A selectorless Service's Endpoints are hand-maintained and
// are the only record of those addresses; a selector-backed Service's are rebuilt.
func TestDumpClassifiesEndpointsByServiceSelector(t *testing.T) {
	d, _ := newDumper(t,
		obj("v1", "Service", "web", map[string]any{
			"spec": map[string]any{"selector": map[string]any{"app": "web"}},
		}),
		obj("v1", "Service", "external-api", map[string]any{"spec": map[string]any{}}),
		obj("v1", "Endpoints", "web", nil),
		obj("v1", "Endpoints", "external-api", nil),
	)
	dir := t.TempDir()
	if _, err := d.Dump(context.Background(), defaultOpts(), dir); err != nil {
		t.Fatalf("Dump: %v", err)
	}
	tree := readTree(t, dir)
	if _, ok := tree["core/Endpoints/web.yaml"]; ok {
		t.Error("Endpoints for a selector-backed Service are rebuilt by the control plane and should be dropped")
	}
	if _, ok := tree["core/Endpoints/external-api.yaml"]; !ok {
		t.Error("Endpoints for a selectorless Service are hand-maintained and must be captured")
	}
}

func TestDumpPagesThroughLargeLists(t *testing.T) {
	d, dyn := newDumper(t)
	d.PageSize = 2

	page := 0
	dyn.PrependReactor("list", "configmaps", func(clienttesting.Action) (bool, runtime.Object, error) {
		page++
		items := []unstructured.Unstructured{
			*obj("v1", "ConfigMap", fmt.Sprintf("cm-%d-a", page), nil),
			*obj("v1", "ConfigMap", fmt.Sprintf("cm-%d-b", page), nil),
		}
		list := &unstructured.UnstructuredList{Object: map[string]any{
			"apiVersion": "v1", "kind": "ConfigMapList",
		}, Items: items}
		if page < 3 {
			list.SetContinue(fmt.Sprintf("token-%d", page))
		}
		return true, list, nil
	})

	dir := t.TempDir()
	idx, err := d.Dump(context.Background(), defaultOpts(), dir)
	if err != nil {
		t.Fatalf("Dump: %v", err)
	}
	if page != 3 {
		t.Errorf("expected 3 pages to be fetched, got %d — continue tokens are not being followed", page)
	}
	var cms int
	for _, e := range idx.Resources {
		if e.Kind == "ConfigMap" {
			cms++
		}
	}
	if cms != 6 {
		t.Errorf("captured %d ConfigMaps across 3 pages, want 6", cms)
	}
}

func TestExcludeSecretDataEmptiesAndAnnotates(t *testing.T) {
	d, _ := newDumper(t,
		obj("v1", "Secret", "db-creds", map[string]any{
			"type": "Opaque",
			"data": map[string]any{"password": "c2VjcmV0"},
		}),
	)
	opts := defaultOpts()
	opts.ExcludeSecretData = true

	dir := t.TempDir()
	idx, err := d.Dump(context.Background(), opts, dir)
	if err != nil {
		t.Fatalf("Dump: %v", err)
	}
	body := readTree(t, dir)["core/Secret/db-creds.yaml"]
	if body == "" {
		t.Fatal("the Secret object must still be captured — only its values are excluded")
	}
	if strings.Contains(body, "c2VjcmV0") || strings.Contains(body, "password") {
		t.Errorf("secret data survived:\n%s", body)
	}
	if !strings.Contains(body, apiconst.AnnotationSecretDataExcluded) {
		t.Errorf("the exclusion must be annotated so a restore can explain the empty Secret:\n%s", body)
	}
	if !strings.Contains(body, "type: Opaque") {
		t.Errorf("the Secret's type must survive:\n%s", body)
	}
	if !idx.SecretDataExcluded {
		t.Error("index must record that secret data was excluded")
	}
}

func TestDumpRejectsUnusableOptions(t *testing.T) {
	d, _ := newDumper(t)
	if _, err := d.Dump(context.Background(), Options{}, t.TempDir()); err == nil {
		t.Error("expected an error with no namespace")
	}
	noSanitizer := &Dumper{Disco: d.Disco, Dynamic: d.Dynamic, Clock: d.Clock}
	if _, err := noSanitizer.Dump(context.Background(), defaultOpts(), t.TempDir()); err == nil {
		t.Error("expected an error with no sanitizer")
	}
}

func TestDumperDefaultsClockAndPageSize(t *testing.T) {
	d := &Dumper{}
	if d.pageSize() != DefaultPageSize {
		t.Errorf("pageSize() = %d, want %d", d.pageSize(), DefaultPageSize)
	}
	if d.now().IsZero() {
		t.Error("now() must fall back to the wall clock")
	}
	var _ clock.PassiveClock = clocktesting.NewFakePassiveClock(fixedTime)
}
