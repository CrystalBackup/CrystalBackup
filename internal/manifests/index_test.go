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
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestStoragePath(t *testing.T) {
	for _, tc := range []struct{ group, kind, name, want string }{
		{"", "Service", "web", "core/Service/web.yaml"},
		{"apps", "Deployment", "web", "apps/Deployment/web.yaml"},
		{"postgresql.cnpg.io", "Cluster", "db", "postgresql.cnpg.io/Cluster/db.yaml"},
	} {
		if got := StoragePath(tc.group, tc.kind, tc.name); got != tc.want {
			t.Errorf("StoragePath(%q,%q,%q) = %q, want %q", tc.group, tc.kind, tc.name, got, tc.want)
		}
	}
}

func TestIndexRoundTrip(t *testing.T) {
	idx := &Index{
		FormatVersion:  IndexFormatVersion,
		ClusterID:      "prod-eu-1",
		Namespace:      "c-team-x",
		BackupName:     "run-1",
		CapturedAt:     "2026-07-12T01:00:00Z",
		RulesetVersion: "1",
		Resources: []IndexEntry{
			{Group: "apps", Version: "v1", Kind: "Deployment", Name: "web", Path: "apps/Deployment/web.yaml"},
			{Group: "", Version: "v1", Kind: "Service", Name: "web", Path: "core/Service/web.yaml"},
		},
	}
	raw, err := idx.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.HasSuffix(string(raw), "\n") {
		t.Error("index.json must end with a newline")
	}
	back, err := ParseIndex(raw)
	if err != nil {
		t.Fatalf("ParseIndex: %v", err)
	}
	if back.ResourceCount != 2 {
		t.Errorf("resourceCount = %d, want 2", back.ResourceCount)
	}
	// Marshal sorts, so the entry order is the path order regardless of how it was built.
	if back.Resources[0].Path != "apps/Deployment/web.yaml" {
		t.Errorf("entries are not sorted by path: %v", back.Resources)
	}
}

// The index is serialised into a deduplicating repository. If two runs over an unchanged
// namespace produce different bytes, restic stores a fresh copy of the index every run.
func TestIndexMarshalIsStableRegardlessOfInsertionOrder(t *testing.T) {
	build := func(reverse bool) string {
		entries := []IndexEntry{
			{Group: "", Kind: "Service", Name: "a", Path: "core/Service/a.yaml"},
			{Group: "apps", Kind: "Deployment", Name: "b", Path: "apps/Deployment/b.yaml"},
			{Group: "", Kind: "ConfigMap", Name: "c", Path: "core/ConfigMap/c.yaml"},
		}
		warns := []Warning{{Kind: "Widget", Message: "z"}, {Kind: "Gadget", Message: "a"}}
		if reverse {
			for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
				entries[i], entries[j] = entries[j], entries[i]
			}
			warns[0], warns[1] = warns[1], warns[0]
		}
		idx := &Index{FormatVersion: IndexFormatVersion, Resources: entries, Warnings: warns}
		raw, err := idx.Marshal()
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		return string(raw)
	}
	if build(false) != build(true) {
		t.Error("index.json depends on insertion order; that defeats dedup on an unchanged namespace")
	}
}

func TestParseIndexRejectsAnUnknownFormatVersion(t *testing.T) {
	if _, err := ParseIndex([]byte(`{"formatVersion": 99}`)); err == nil {
		t.Error("a future index layout must be rejected, not silently reinterpreted")
	}
	if _, err := ParseIndex([]byte(`not json`)); err == nil {
		t.Error("expected a parse error")
	}
}

// Partial discovery is the normal degraded state — one aggregated API down, a webhook timing
// out. It must cost that group, not the tenant's whole manifest backup.
func TestPartialDiscoveryIsAWarningNotAFailure(t *testing.T) {
	d, _ := newDumper(t, obj("v1", "ConfigMap", "survivor", nil))
	fd, ok := d.Disco.(*fakeDiscovery)
	if !ok {
		t.Fatalf("unexpected discovery type %T", d.Disco)
	}
	fd.err = fmt.Errorf("unable to retrieve the complete list of server APIs: metrics.k8s.io/v1beta1: the server is currently unable to handle the request")

	dir := t.TempDir()
	idx, err := d.Dump(context.Background(), defaultOpts(), dir)
	if err != nil {
		t.Fatalf("partial discovery must not fail the dump: %v", err)
	}
	if _, ok := readTree(t, dir)["core/ConfigMap/survivor.yaml"]; !ok {
		t.Error("the groups discovery DID return must still be captured")
	}
	var warned bool
	for _, w := range idx.Warnings {
		if strings.Contains(w.Message, "partial resource discovery") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("degraded discovery must be recorded; warnings = %v", idx.Warnings)
	}
}

// Total discovery failure is different: nothing came back, so the dump has no idea what it
// missed. It still writes an index — an empty snapshot that SAYS it is empty because
// discovery died is far more useful to a restore than no snapshot at all.
func TestTotalDiscoveryFailureIsRecorded(t *testing.T) {
	d, _ := newDumper(t)
	fd, ok := d.Disco.(*fakeDiscovery)
	if !ok {
		t.Fatalf("unexpected discovery type %T", d.Disco)
	}
	fd.preferred = nil
	fd.err = fmt.Errorf("connection refused")

	idx, err := d.Dump(context.Background(), defaultOpts(), t.TempDir())
	if err != nil {
		t.Fatalf("Dump: %v", err)
	}
	if idx.ResourceCount != 0 {
		t.Errorf("resourceCount = %d, want 0", idx.ResourceCount)
	}
	var warned bool
	for _, w := range idx.Warnings {
		if strings.Contains(w.Message, "discovery failed entirely") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("a total discovery failure must be recorded; warnings = %v", idx.Warnings)
	}
}

func TestKeepResource(t *testing.T) {
	rw := []string{"get", "list", "watch"}
	for name, tc := range map[string]struct {
		r    metav1.APIResource
		want bool
	}{
		"namespaced and listable": {metav1.APIResource{Name: "configmaps", Namespaced: true, Verbs: rw}, true},
		"cluster-scoped":          {metav1.APIResource{Name: "nodes", Namespaced: false, Verbs: rw}, false},
		"subresource":             {metav1.APIResource{Name: "pods/log", Namespaced: true, Verbs: rw}, false},
		"no list verb":            {metav1.APIResource{Name: "bindings", Namespaced: true, Verbs: []string{"create", "get"}}, false},
		"no get verb":             {metav1.APIResource{Name: "x", Namespaced: true, Verbs: []string{"list"}}, false},
		"no verbs at all":         {metav1.APIResource{Name: "y", Namespaced: true}, false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := keepResource(tc.r); got != tc.want {
				t.Errorf("keepResource(%q) = %v, want %v", tc.r.Name, got, tc.want)
			}
		})
	}
}

func TestEnumerateSkipsUnparsableGroupVersion(t *testing.T) {
	d, _ := newDumper(t)
	fd, ok := d.Disco.(*fakeDiscovery)
	if !ok {
		t.Fatalf("unexpected discovery type %T", d.Disco)
	}
	fd.preferred = []*metav1.APIResourceList{
		nil, // a nil entry must not panic the dump
		{GroupVersion: "a/b/c/broken", APIResources: []metav1.APIResource{{Name: "x", Namespaced: true, Verbs: []string{"get", "list"}}}},
	}
	_, warnings := d.enumerate()
	var found bool
	for _, w := range warnings {
		if strings.Contains(w.Message, "unparsable groupVersion") {
			found = true
		}
	}
	if !found {
		t.Errorf("an unparsable groupVersion must be recorded, not silently dropped; warnings = %v", warnings)
	}
}
