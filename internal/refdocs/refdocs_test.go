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
// rule, in the three-way form it took in 0.6.5: `_total` families and histograms reset when the
// operator restarts; a gauge DECLARED in sweepSetFamilies is written by a periodic Runnable and is
// only as fresh as its last pass; every other gauge is recomputed at scrape and survives a restart
// intact.
//
// buildFamilies refuses to generate when this breaks, so this test is the readable version of that
// refusal — it names the family and the direction rather than failing a page build.
//
// The third arm is not a loosening. A sweep-set gauge has to be OPTED IN by name, and the opt-in is
// checked against the harvest in both directions, so the failure mode the two-way rule caught still
// fails: register a gauge on the event registry without declaring it, and this test says so.
func TestEventDrivenFamiliesAreCountersAndHistograms(t *testing.T) {
	families, err := Families()
	if err != nil {
		t.Fatalf("Families(): %v", err)
	}
	for _, f := range families {
		switch {
		case f.Kind != KindGauge:
			if f.ScrapeDerived {
				t.Errorf("%s is a %s but derived at scrape; the page tells readers to wrap it in increase()",
					f.Name, f.Kind)
			}
			if f.SweepSet {
				t.Errorf("%s is a %s declared as sweep-set; only a gauge can be", f.Name, f.Kind)
			}
		case f.SweepSet:
			if f.ScrapeDerived {
				t.Errorf("%s is declared sweep-set but the scrape collector publishes it; the "+
					"declaration is stale and the page would understate its freshness", f.Name)
			}
		default:
			if !f.ScrapeDerived {
				t.Errorf("%s is a gauge, is written outside the scrape path, and is NOT declared in "+
					"sweepSetFamilies; the page would tell a reader it survives a restart, which it "+
					"does not", f.Name)
			}
		}
	}
}

// TestSweepSetDeclarationIsNotStale: a name in sweepSetFamilies that no longer exists would be a
// silent no-op, and the next sweep-set gauge would then slip through undeclared and be documented as
// restart-safe.
func TestSweepSetDeclarationIsNotStale(t *testing.T) {
	families, err := Families()
	if err != nil {
		t.Fatalf("Families(): %v", err)
	}
	published := map[string]bool{}
	for _, f := range families {
		published[f.Name] = true
	}
	for _, name := range sweepSetFamilies {
		if !published[name] {
			t.Errorf("sweepSetFamilies declares %s, which nothing publishes", name)
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
