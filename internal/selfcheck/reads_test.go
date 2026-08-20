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

package selfcheck

import (
	"context"
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
)

// TestSelfcheckReadsAreDeclared is one half of the chain that replaces an audit somebody did once by
// hand (see reads.go for what that audit cost).
//
// It runs a whole Collect over a reader that RECORDS every request instead of asserting a list of
// call sites, because the reads are made from a dozen places and one of them — the exposer registry
// the census hands a client to — is not even in this package. A grep-based or reviewer-based
// inventory misses exactly that one, which is the one 0.6.5 changed.
//
// Its sibling is test/chart's TestSoakCollectorRoleCoversEverySelfcheckRead: this test says "you did
// not declare that read", the chart test says "you declared it and did not grant it". Both have to
// be satisfied for a new read to ship working.
func TestSelfcheckReadsAreDeclared(t *testing.T) {
	declared := map[string]bool{}
	for _, r := range APIReads {
		declared[r.Group+"/"+r.Kind] = true
	}

	// Two fixtures, unioned. The coverage fixture is the only one that reaches the PersistentVolume,
	// StorageClass and Secret reads (a bound PVC, an unbound one and a snapshot class naming an
	// absent Secret respectively); the report fixture is the only one carrying every product CR. A
	// test run over either alone would leave half the inventory unexercised and quietly stop being a
	// gate.
	seen := map[string]bool{}
	for _, c := range []client.Reader{coverageFixture(t), fixtureClient(t)} {
		rec := &recordingReader{Reader: c, seen: seen}
		if _, err := Collect(context.Background(), Options{
			Reader: rec, OperatorNamespace: operatorNS, Now: fixtureNow(), Full: true,
		}); err != nil {
			t.Fatalf("collect: %v", err)
		}
	}

	for _, gk := range sortedKeys(seen) {
		if !declared[gk] {
			t.Errorf("the self-check reads %q and internal/selfcheck/reads.go does not declare it.\n"+
				"Declare it there — with the section that stops working without it — and grant it in "+
				"charts/crystal-backup/templates/soak.yaml. A read that only one of those two files "+
				"knows about is the 0.6.5 defect: nine days of reports condemning volumes that were "+
				"being backed up nightly.", gk)
		}
	}

	// And the other direction, loosely: a declaration nobody exercises is a claim this test cannot
	// keep honest, so it is named rather than failed. Loosely, because a read can be real and still
	// not reachable from a fixture — the fixtures are a cluster, not a proof.
	for _, r := range APIReads {
		if !seen[r.Group+"/"+r.Kind] {
			t.Logf("declared but not exercised by either fixture: %s/%s (%s)", r.Group, r.Kind, r.Why)
		}
	}
}

// TestSelfcheckReadsAreWellFormed pins the shape the two consumers rely on: the chart test matches on
// Resource and this file matches on Kind, so an entry missing either is an entry that silently opts
// out of one of the two gates.
func TestSelfcheckReadsAreWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, r := range APIReads {
		switch {
		case r.Kind == "":
			t.Errorf("%+v has no Kind, so the recorder can never match it", r)
		case r.Resource == "":
			t.Errorf("%+v has no Resource, so the chart test can never match it", r)
		case len(r.Verbs) == 0:
			t.Errorf("%+v grants nothing, so it asserts nothing", r)
		case r.Why == "":
			t.Errorf("%+v does not say what stops working without it", r)
		}
		if r.Resource != strings.ToLower(r.Resource) {
			t.Errorf("%q is not an RBAC resource name (they are lower case plurals)", r.Resource)
		}
		key := r.Group + "/" + r.Kind
		if seen[key] {
			t.Errorf("%s is declared twice; the two copies will disagree", key)
		}
		seen[key] = true
	}
	if len(GrantedAPIReads()) == len(APIReads) {
		t.Error("no read is marked Ungranted, but the Secret read is one and must stay one — a " +
			"report-only identity that can read every Secret in the cluster is invariant I3 breached " +
			"by a test nobody re-read")
	}
}

// recordingReader notes the group and Kind of everything asked of it, whether or not the answer is an
// error: a read that fails is still a read the role has to allow, and the refused ones are precisely
// the case this whole lot is about.
type recordingReader struct {
	client.Reader
	seen map[string]bool
}

func (r *recordingReader) Get(
	ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption,
) error {
	r.note(obj)
	return r.Reader.Get(ctx, key, obj, opts...)
}

func (r *recordingReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	r.note(list)
	return r.Reader.List(ctx, list, opts...)
}

// note resolves the GVK the way the client itself does — the object's own for an unstructured, the
// scheme for a typed one — and records the singular Kind. A List of PodList and a Get of a Pod are
// one permission, so the "List" suffix is stripped rather than declared twice.
func (r *recordingReader) note(obj any) {
	var gvk schema.GroupVersionKind
	switch o := obj.(type) {
	case *unstructured.Unstructured:
		gvk = o.GroupVersionKind()
	case *unstructured.UnstructuredList:
		gvk = o.GroupVersionKind()
	case client.Object:
		var err error
		if gvk, err = apiutil.GVKForObject(o, buildScheme()); err != nil {
			return
		}
	case client.ObjectList:
		var err error
		if gvk, err = apiutil.GVKForObject(o, buildScheme()); err != nil {
			return
		}
	default:
		return
	}
	r.seen[gvk.Group+"/"+strings.TrimSuffix(gvk.Kind, "List")] = true
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
