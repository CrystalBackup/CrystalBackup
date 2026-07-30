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

package refdocs

import (
	"strings"
	"testing"

	"github.com/CrystalBackup/CrystalBackup/internal/alerts"
	"github.com/CrystalBackup/CrystalBackup/internal/metrics"
)

// TestFamiliesMatchTheCatalogue is the harvest's own guard. Families() is built from the Descs the
// collectors register and cross-checked against metrics.Catalogue() on the way through, so this
// failing means one of the two moved without the other — which is exactly the divergence the page
// would otherwise print as fact.
func TestFamiliesMatchTheCatalogue(t *testing.T) {
	families, err := Families()
	if err != nil {
		t.Fatalf("Families(): %v", err)
	}
	catalogue := metrics.Catalogue()
	if len(families) != len(catalogue) {
		t.Fatalf("harvested %d families, metrics.Catalogue() declares %d", len(families), len(catalogue))
	}
	for _, f := range families {
		if f.Help == "" {
			t.Errorf("%s has no help string, so the page would print an empty Meaning cell", f.Name)
		}
		if f.Kind == KindHistogram && len(f.Buckets) == 0 {
			t.Errorf("%s is a histogram with no bucket bounds", f.Name)
		}
	}
}

// TestEventDrivenFamiliesAreCountersAndHistograms holds the sentence the Metrics page states as a
// rule: `_total` families and histograms reset when the operator restarts, everything else is
// recomputed at scrape and does not.
//
// buildFamilies refuses to generate when this breaks, so this test is the readable version of that
// refusal — it names the family and the direction rather than failing a page build.
func TestEventDrivenFamiliesAreCountersAndHistograms(t *testing.T) {
	families, err := Families()
	if err != nil {
		t.Fatalf("Families(): %v", err)
	}
	for _, f := range families {
		if f.Kind == KindGauge && !f.ScrapeDerived {
			t.Errorf("%s is a gauge but event-driven; the page says every gauge survives a restart", f.Name)
		}
		if f.Kind != KindGauge && f.ScrapeDerived {
			t.Errorf("%s is a %s but derived at scrape; the page tells readers to wrap it in increase()",
				f.Name, f.Kind)
		}
	}
}

// TestEveryFamilyLandsInASection: a family added to internal/metrics must appear on the page, not
// merely fail to break it. RenderMetrics returns an error rather than silently dropping one.
func TestEveryFamilyLandsInASection(t *testing.T) {
	families, err := Families()
	if err != nil {
		t.Fatalf("Families(): %v", err)
	}
	page, err := RenderMetrics(families)
	if err != nil {
		t.Fatalf("RenderMetrics(): %v", err)
	}
	for _, f := range families {
		if !strings.Contains(string(page), "`"+f.Name+"`") {
			t.Errorf("%s is published but does not appear on the metrics page", f.Name)
		}
	}

	orphan := append(families, Family{Name: "crystalbackup_nothing_claims_this", Kind: KindGauge, ScrapeDerived: true})
	if _, err := RenderMetrics(orphan); err == nil {
		t.Error("RenderMetrics accepted a family belonging to no section; a new family would ship undocumented")
	}
}

// TestRuleTextHasBalancedBackticks keeps prose() honest. It bails out unchanged on an odd number of
// backticks rather than guessing where a code span ends, so an unbalanced literal in rules.go would
// quietly stop the series names in that rationale from being marked up.
func TestRuleTextHasBalancedBackticks(t *testing.T) {
	for _, r := range alerts.Rules() {
		for _, field := range []struct{ name, text string }{
			{"Summary", r.Summary}, {"Description", r.Description}, {"Rationale", r.Rationale},
		} {
			if n := strings.Count(field.text, "`"); n%2 != 0 {
				t.Errorf("%s.%s has %d backticks (odd); prose() cannot mark it up", r.Name, field.name, n)
			}
		}
	}
}

func TestProse(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"the guard on crystalbackup_schedule_active is what stops it",
			"the guard on `crystalbackup_schedule_active` is what stops it"},
		{"already marked up: `crystalbackup_backup_failures` stays put",
			"already marked up: `crystalbackup_backup_failures` stays put"},
		// Only `<` needs escaping: a lone `>` mid-line is literal in markdown.
		{"a bare <nothing> must not become a tag", "a bare &lt;nothing> must not become a tag"},
		{"inside a span `time() - <nothing>` is left alone", "inside a span `time() - <nothing>` is left alone"},
	} {
		if got := prose(tc.in); got != tc.want {
			t.Errorf("prose(%q)\n got %q\nwant %q", tc.in, got, tc.want)
		}
	}
}

// TestMarkdownRationaleKeepsBullets: the rationales carry their reasoning in `  * ` lists with
// wrapped continuation lines. Reflowing those into one paragraph would lose the structure that
// makes them readable at 03:00, which is the only time they get read.
func TestMarkdownRationaleKeepsBullets(t *testing.T) {
	got := markdownRationale(strings.Join([]string{
		"A leading sentence",
		"wrapped across two lines.",
		"  * first point,",
		"    continued here;",
		"  * second point.",
		"A closing sentence.",
	}, "\n"))

	want := "A leading sentence wrapped across two lines.\n\n" +
		"- first point, continued here;\n" +
		"- second point.\n\n" +
		"A closing sentence.\n\n"
	if got != want {
		t.Errorf("markdownRationale()\n got %q\nwant %q", got, want)
	}
}

// TestAlertsPageDocumentsEveryRule: a rule added to the table must reach the page, with its
// rationale. The rationale is the reason this page is generated at all — it is where "why 26 hours
// and not 24" is written down.
func TestAlertsPageDocumentsEveryRule(t *testing.T) {
	page, err := RenderAlerts()
	if err != nil {
		t.Fatalf("RenderAlerts(): %v", err)
	}
	text := string(page)
	for _, r := range alerts.Rules() {
		if !strings.Contains(text, "## "+r.Name) {
			t.Errorf("%s has no section on the alerts page", r.Name)
		}
		if !strings.Contains(text, "](#"+anchor(r.Name)+")") {
			t.Errorf("%s is not linked from the overview table", r.Name)
		}
		first := strings.SplitN(r.Rationale, "\n", 2)[0]
		if !strings.Contains(text, prose(first)) {
			t.Errorf("%s: the rationale did not survive rendering (looked for %q)", r.Name, first)
		}
	}
}
