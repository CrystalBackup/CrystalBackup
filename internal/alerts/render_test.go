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

package alerts

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/yaml"
)

func parseGoDuration(s string) (time.Duration, error) { return time.ParseDuration(s) }

// renderedGroups is the shape the generated file must parse back into — the PrometheusRule `spec`
// body as the Prometheus Operator reads it.
type renderedGroups struct {
	Groups []struct {
		Name  string `json:"name"`
		Rules []struct {
			Alert       string            `json:"alert"`
			Expr        string            `json:"expr"`
			For         string            `json:"for"`
			Labels      map[string]string `json:"labels"`
			Annotations map[string]string `json:"annotations"`
		} `json:"rules"`
	} `json:"groups"`
}

func parseRendered(t *testing.T, raw []byte) renderedGroups {
	t.Helper()
	var g renderedGroups
	if err := yaml.Unmarshal(raw, &g); err != nil {
		t.Fatalf("the generated rule file is not valid YAML: %v", err)
	}
	return g
}

// TestRenderRoundTrips is the check that keeps Render's hand-rolled quoting honest. It emits YAML
// by hand rather than through a marshaller (the per-rule comments are the point), so the escaping
// of an annotation full of {{ }}, quotes and a unicode arrow is verified against a real parser
// instead of assumed.
func TestRenderRoundTrips(t *testing.T) {
	g := parseRendered(t, Render())

	if len(g.Groups) != 1 || g.Groups[0].Name != GroupName {
		t.Fatalf("want exactly one group named %q, got %+v", GroupName, g.Groups)
	}
	got := g.Groups[0].Rules
	want := Rules()
	if len(got) != len(want) {
		t.Fatalf("rendered %d rules, table has %d", len(got), len(want))
	}

	for i, w := range want {
		r := got[i]
		if r.Alert != w.Name {
			t.Errorf("rule %d: rendered %q, table says %q", i, r.Alert, w.Name)
			continue
		}
		if strings.TrimSpace(r.Expr) != strings.TrimSpace(w.Expr) {
			t.Errorf("%s: expression survived the round trip changed:\n--- rendered ---\n%s\n"+
				"--- table ---\n%s", w.Name, r.Expr, w.Expr)
		}
		if r.Annotations["summary"] != w.Summary {
			t.Errorf("%s: summary round-tripped as %q, table says %q — check yamlString's escaping",
				w.Name, r.Annotations["summary"], w.Summary)
		}
		if r.Annotations["description"] != w.Description {
			t.Errorf("%s: description round-tripped as %q, table says %q",
				w.Name, r.Annotations["description"], w.Description)
		}
		if r.Labels["severity"] != string(w.Severity) {
			t.Errorf("%s: severity round-tripped as %q, table says %q",
				w.Name, r.Labels["severity"], w.Severity)
		}
		if w.For == 0 {
			if r.For != "" {
				t.Errorf("%s: table declares no `for`, rendered %q", w.Name, r.For)
			}
		} else if r.For != promDuration(w.For) {
			t.Errorf("%s: rendered for=%q, table says %s", w.Name, r.For, promDuration(w.For))
		}
	}
}

// TestGeneratedFileIsUpToDate is the alert-rule equivalent of `make api-docs-verify`.
//
// Without it the generator is a suggestion: someone edits the table, forgets `make alert-rules`,
// and the chart keeps shipping the previous release's rules while the Go source says otherwise —
// which is the same class of divergence between the rule text and the metric definitions that this
// whole package was built to end, just moved one step downstream.
func TestGeneratedFileIsUpToDate(t *testing.T) {
	path := filepath.Join("..", "..", "charts", "crystal-backup", RulesFile)
	committed, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("cannot read the committed rule file (%s). Run `make alert-rules`: %v", path, err)
	}
	if string(committed) != string(Render()) {
		t.Fatalf("%s is out of date with internal/alerts/rules.go.\nRun `make alert-rules` and "+
			"commit the result.", path)
	}
}

// TestCommittedFileParsesAsRuleGroups checks the artifact itself, not just what Render returns:
// the file the chart actually ships has to be a rule-group document, since templates/
// prometheusrule.yaml drops it under `spec:` verbatim with no schema in between.
func TestCommittedFileParsesAsRuleGroups(t *testing.T) {
	path := filepath.Join("..", "..", "charts", "crystal-backup", RulesFile)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	g := parseRendered(t, raw)
	if len(g.Groups) == 0 || len(g.Groups[0].Rules) == 0 {
		t.Fatalf("%s parsed to no rules at all", path)
	}
	for _, r := range g.Groups[0].Rules {
		if r.Alert == "" || r.Expr == "" {
			t.Errorf("%s: a rule came back with an empty alert name or expression: %+v", path, r)
		}
	}
}

// TestPromDurationIsPrometheusSpelling — Go's "15m0s" parses, but nobody writes it.
func TestPromDurationIsPrometheusSpelling(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
	}{
		{"15m", "15m"}, {"1h", "1h"}, {"5m", "5m"}, {"30m", "30m"}, {"90s", "90s"},
	} {
		d, err := parseGoDuration(tc.in)
		if err != nil {
			t.Fatalf("%s: %v", tc.in, err)
		}
		if got := promDuration(d); got != tc.want {
			t.Errorf("promDuration(%s) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
