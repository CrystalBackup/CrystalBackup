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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// resource is one candidate under test.
type resource struct {
	group, kind, name string
	labels            map[string]string
}

var (
	webDeploy   = resource{"apps", "Deployment", "web", map[string]string{"app": "web"}}
	apiDeploy   = resource{"apps", "Deployment", "api", map[string]string{"app": "api"}}
	legacyDep   = resource{"apps", "Deployment", "legacy-cron", nil}
	pgStateful  = resource{"apps", "StatefulSet", "postgres", nil}
	dbSecret    = resource{"", "Secret", "db-creds", nil}
	webService  = resource{"", "Service", "web", map[string]string{"app": "web"}}
	cnpgCluster = resource{"postgresql.cnpg.io", "Cluster", "db", nil}
)

func TestSelectionMatching(t *testing.T) {
	tests := []struct {
		name      string
		selection Selection
		selected  []resource
		rejected  []resource
	}{
		{
			// spec/02-api.md § Restore selection model: both lists omitted ⇒ whole namespace.
			// The operator resolves that to All before the mover ever sees it.
			name:      "All selects everything",
			selection: Selection{All: true},
			selected:  []resource{webDeploy, dbSecret, cnpgCluster, webService},
		},
		{
			// The counterpart, and the one a nil-means-everything implementation gets wrong:
			// `resources: []` is a PRESENT field listing nothing, so nothing is restored.
			name:      "an empty selection selects nothing",
			selection: Selection{},
			rejected:  []resource{webDeploy, dbSecret, cnpgCluster},
		},
		{
			name:      "include on group/Kind matches every object of that kind",
			selection: Selection{Items: []SelectionItem{{Include: []string{"apps/Deployment"}}}},
			selected:  []resource{webDeploy, apiDeploy, legacyDep},
			rejected:  []resource{pgStateful, dbSecret, webService},
		},
		{
			name:      "include on group/Kind/name pins one object",
			selection: Selection{Items: []SelectionItem{{Include: []string{"apps/StatefulSet/postgres"}}}},
			selected:  []resource{pgStateful},
			rejected:  []resource{webDeploy, {"apps", "StatefulSet", "redis", nil}},
		},
		{
			// The core group elided — "Secret/db-creds" is Kind/name, not group/Kind. Getting
			// this backwards would silently select nothing.
			name:      "the core group may be elided",
			selection: Selection{Items: []SelectionItem{{Include: []string{"Secret/db-creds"}}}},
			selected:  []resource{dbSecret},
			rejected:  []resource{webDeploy, {"", "Secret", "other", nil}},
		},
		{
			name:      "the core group may be spelled core",
			selection: Selection{Items: []SelectionItem{{Include: []string{"core/Service/web"}}}},
			selected:  []resource{webService},
			rejected:  []resource{webDeploy},
		},
		{
			// A group-wide wildcard. Deciding the group/Kind split on the SECOND segment would
			// read the "*" as a name and match nothing.
			name:      "a wildcard kind selects a whole group",
			selection: Selection{Items: []SelectionItem{{Include: []string{"apps/*"}}}},
			selected:  []resource{webDeploy, pgStateful},
			rejected:  []resource{dbSecret, cnpgCluster},
		},
		{
			name:      "a custom resource is selected by its own group",
			selection: Selection{Items: []SelectionItem{{Include: []string{"postgresql.cnpg.io/Cluster/db"}}}},
			selected:  []resource{cnpgCluster},
			rejected:  []resource{webDeploy},
		},
		{
			// AND within an item: the label narrows what include selected.
			name: "selector and include are ANDed within an item",
			selection: Selection{Items: []SelectionItem{{
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}},
				Include:  []string{"apps/Deployment"},
			}}},
			selected: []resource{webDeploy},
			rejected: []resource{apiDeploy, webService},
		},
		{
			name: "an item may select by label alone",
			selection: Selection{Items: []SelectionItem{{
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}},
			}}},
			selected: []resource{webDeploy, webService},
			rejected: []resource{apiDeploy, dbSecret},
		},
		{
			// Exclude runs after both, inside the item.
			name: "exclude removes from what the item selected",
			selection: Selection{Items: []SelectionItem{{
				Include: []string{"apps/Deployment"},
				Exclude: []string{"apps/Deployment/legacy-*"},
			}}},
			selected: []resource{webDeploy, apiDeploy},
			rejected: []resource{legacyDep},
		},
		{
			// OR between items — and the property that makes OR meaningful: an exclude in one
			// item must not veto a match in another, or items would silently interfere.
			name: "items are ORed and one item's exclude does not veto another's match",
			selection: Selection{Items: []SelectionItem{
				{Include: []string{"apps/Deployment"}, Exclude: []string{"apps/Deployment/legacy-*"}},
				{Include: []string{"apps/Deployment/legacy-cron"}},
			}},
			selected: []resource{webDeploy, legacyDep},
			rejected: []resource{dbSecret},
		},
		{
			name: "an item with only an exclude selects everything else",
			selection: Selection{Items: []SelectionItem{{
				Exclude: []string{"Secret"},
			}}},
			selected: []resource{webDeploy, webService},
			rejected: []resource{dbSecret},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			compiled, err := tc.selection.Compile()
			if err != nil {
				t.Fatalf("Compile() = %v, want no error", err)
			}
			for _, r := range tc.selected {
				if !compiled.Matches(r.group, r.kind, r.name, r.labels) {
					t.Errorf("%s/%s/%s: not selected, want selected", r.group, r.kind, r.name)
				}
			}
			for _, r := range tc.rejected {
				if compiled.Matches(r.group, r.kind, r.name, r.labels) {
					t.Errorf("%s/%s/%s: selected, want rejected", r.group, r.kind, r.name)
				}
			}
		})
	}
}

func TestSelectionCompileRejectsBadPatterns(t *testing.T) {
	// A typo in an include is the difference between "restored two objects" and "restored the
	// namespace", and both look like success from outside. Rejecting at compile time is what
	// makes the mistake visible.
	for _, pattern := range []string{
		"",
		"a/b/c/d",
		"apps//Deployment",
		"Secret/db/creds", // a Kind-first pattern carries at most a name
		"apps/[Deployment",
	} {
		t.Run(pattern, func(t *testing.T) {
			_, err := Selection{Items: []SelectionItem{{Include: []string{pattern}}}}.Compile()
			if err == nil {
				t.Errorf("Compile() with include %q = nil error, want a rejection", pattern)
			}
		})
	}
}

func TestSelectionRoundTrip(t *testing.T) {
	// The selection crosses into the mover as an environment variable, so the encoding has to
	// preserve the one distinction the whole tri-state rests on: All versus an empty item list.
	for _, tc := range []struct {
		name string
		in   Selection
	}{
		{"all", Selection{All: true}},
		{"nothing", Selection{}},
		{"items", Selection{Items: []SelectionItem{{Include: []string{"apps/Deployment"}, Exclude: []string{"apps/Deployment/x"}}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			encoded, err := EncodeSelection(tc.in)
			if err != nil {
				t.Fatalf("EncodeSelection() = %v", err)
			}
			got, err := DecodeSelection(encoded)
			if err != nil {
				t.Fatalf("DecodeSelection() = %v", err)
			}
			if got.All != tc.in.All || len(got.Items) != len(tc.in.Items) {
				t.Errorf("round trip = %+v, want %+v", got, tc.in)
			}
		})
	}
}

func TestDecodeSelectionEmptyMeansAll(t *testing.T) {
	// An operator that predates narrowing sets no variable at all. Restoring the whole snapshot
	// is what that operator meant; restoring nothing would be a silently empty restore.
	got, err := DecodeSelection("")
	if err != nil {
		t.Fatalf("DecodeSelection(\"\") = %v", err)
	}
	if !got.All {
		t.Errorf("DecodeSelection(\"\").All = false, want true")
	}
}

func TestApplyPhaseOrdering(t *testing.T) {
	// The ordering exists so an object's dependencies are admitted before it is
	// (04-manifest-backup.md §5.1). These are the relations that actually matter.
	mustPrecede := []struct{ before, after resource }{
		{resource{group: "", kind: kindServiceAccount}, resource{group: groupRBAC, kind: "RoleBinding"}},
		{resource{group: groupRBAC, kind: "RoleBinding"}, resource{group: "", kind: "ConfigMap"}},
		{resource{group: "", kind: "Secret"}, resource{group: "", kind: kindPVC}},
		{resource{group: "", kind: kindPVC}, resource{group: groupApps, kind: "Deployment"}},
		// The catch-all sits before workloads: a CNPG Cluster or a cert-manager Certificate is
		// usually what a Deployment is waiting on, not the other way round.
		{resource{group: "postgresql.cnpg.io", kind: "Cluster"}, resource{group: groupApps, kind: "StatefulSet"}},
		{resource{group: groupApps, kind: "Deployment"}, resource{group: "", kind: "Service"}},
	}
	for _, rel := range mustPrecede {
		b := applyPhase(rel.before.group, rel.before.kind)
		a := applyPhase(rel.after.group, rel.after.kind)
		if b >= a {
			t.Errorf("%s/%s (phase %d) must precede %s/%s (phase %d)",
				rel.before.group, rel.before.kind, b, rel.after.group, rel.after.kind, a)
		}
	}

	// A custom Kind that happens to share a built-in's name belongs with its own CRD's kinds,
	// not among the workloads — which is why the table is keyed on group AND kind.
	if got := applyPhase("acme.example.com", "Job"); got != phaseEverythingElse {
		t.Errorf("applyPhase(acme.example.com, Job) = %d, want the generic phase %d", got, phaseEverythingElse)
	}
}
