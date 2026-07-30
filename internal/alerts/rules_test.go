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
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/CrystalBackup/CrystalBackup/internal/metrics"
)

// This file is the alert-side twin of hack/check-dashboard-metrics.py, which does the same job for
// the Grafana dashboards. The dashboards are JSON, so that check has to parse the label sets back
// out of the Go sources with regexes; the rules are Go, so this one reads metrics.Catalogue()
// directly — the same []string the collectors hand to prometheus.NewDesc, with no parser in
// between and nothing to keep in step.
//
// The guarantee is identical, and it is the one the 0.5.x cycle lacked: an expression naming a
// series nobody emits, or selecting on a label a family does not carry, is valid PromQL that
// matches nothing forever. On a backup alert that is indistinguishable from "all is well".

var (
	metricToken   = regexp.MustCompile(`\bcrystalbackup_[a-z0-9_]+\b`)
	matcherRe     = regexp.MustCompile(`([a-zA-Z_][a-zA-Z0-9_]*)\s*(=~|!~|=|!=)\s*"([^"]*)"`)
	selectorRe    = regexp.MustCompile(`(crystalbackup_[a-z0-9_]+)\s*\{([^}]*)\}`)
	groupingRe    = regexp.MustCompile(`\b(?:by|without|on|ignoring|group_left|group_right)\s*\(([^)]*)\)`)
	annotationRef = regexp.MustCompile(`\$labels\.([a-zA-Z_][a-zA-Z0-9_]*)`)
)

// histogramSuffixes are the series Prometheus derives from a histogram family, and only from one.
var histogramSuffixes = []string{"_bucket", "_sum", "_count"}

// syntheticLabels are attached by Prometheus itself or by the scrape config, so no collector
// declares them and grouping on them is legitimate. Same list as hack/check-dashboard-metrics.py's
// SYNTHETIC_LABELS, for the same reason.
var syntheticLabels = []string{
	"le", "job", "instance", "pod", "container", "endpoint", "service", "node", "__name__",
}

// resolveSeries maps a token in an expression back to its catalogue family, or "" when nothing
// emits it.
func resolveSeries(token string) string {
	cat := metrics.Catalogue()
	if _, ok := cat[token]; ok {
		return token
	}
	for _, suffix := range histogramSuffixes {
		base, found := strings.CutSuffix(token, suffix)
		if !found {
			continue
		}
		if slices.Contains(metrics.Histograms(), base) {
			return base
		}
	}
	return ""
}

// labelValueSets are the label values the COLLECTOR emits, addressed rather than restated: a
// selector on `scope="Namespaced"` names a real metric and a real label and still matches nothing,
// because metricScope() translates the kubebuilder enum before emission. That exact mistake
// shipped on the dashboards for an afternoon.
func labelValueSets() map[string]map[string]bool {
	return map[string]map[string]bool{
		"scope": {
			metrics.ScopeLabelValue("Cluster"):    true,
			metrics.ScopeLabelValue("Namespaced"): true,
		},
	}
}

// checkRule returns every way a rule's expression and annotations disagree with the real metric
// catalogue. It takes a Rule rather than reading the table so the fault-injection test below can
// prove it actually catches things: a checker nobody has ever seen fail is a checker nobody should
// trust.
func checkRule(r Rule) []string {
	var problems []string
	cat := metrics.Catalogue()

	var families []string
	for _, token := range metricToken.FindAllString(r.Expr, -1) {
		fam := resolveSeries(token)
		if fam == "" {
			problems = append(problems, fmt.Sprintf(
				"references %q, which is not in internal/metrics/names.go — valid PromQL against a "+
					"series nobody emits evaluates cleanly, matches nothing and never fires", token))
			continue
		}
		if !slices.Contains(families, fam) {
			families = append(families, fam)
		}
	}
	if len(families) == 0 {
		return append(problems, fmt.Sprintf("names no crystalbackup_ series at all: %q", r.Expr))
	}

	allowed := map[string]bool{}
	for _, l := range syntheticLabels {
		allowed[l] = true
	}
	for _, fam := range families {
		for _, l := range cat[fam] {
			allowed[l] = true
		}
	}
	carriedBy := strings.Join(families, "/")

	// Labels and values named in a selector: crystalbackup_x{label="value"}.
	values := labelValueSets()
	for _, sel := range selectorRe.FindAllStringSubmatch(r.Expr, -1) {
		fam := resolveSeries(sel[1])
		for _, m := range matcherRe.FindAllStringSubmatch(sel[2], -1) {
			label, op, value := m[1], m[2], m[3]
			if !allowed[label] {
				problems = append(problems, fmt.Sprintf(
					"selects on label %q, which %s does not carry (labels: %s)",
					label, fam, strings.Join(cat[fam], ", ")))
				continue
			}
			if permitted, known := values[label]; known && op == "=" && !permitted[value] {
				problems = append(problems, fmt.Sprintf(
					"selects %s=%q, but the collector only ever emits %s=%s",
					label, value, label, keysOf(permitted)))
			}
		}
	}

	// Labels named in a matching or grouping modifier: on(...), by(...), group_left(...).
	for _, g := range groupingRe.FindAllStringSubmatch(r.Expr, -1) {
		for _, raw := range strings.Split(g[1], ",") {
			label := strings.TrimSpace(raw)
			if label == "" { // group_left() with no labels is legal and names nothing.
				continue
			}
			if !allowed[label] {
				problems = append(problems, fmt.Sprintf(
					"matches on label %q, which none of %s carries — a join on a label that does not "+
						"exist finds no partner and the rule goes permanently quiet", label, carriedBy))
			}
		}
	}

	// {{ $labels.x }} in the annotations. A wrong name here does not stop the alert firing, it
	// renders empty — which is how a page arrives reading "Backup failed for /" and nobody can
	// tell whose namespace it was.
	for _, text := range []string{r.Summary, r.Description} {
		for _, m := range annotationRef.FindAllStringSubmatch(text, -1) {
			if !allowed[m[1]] {
				problems = append(problems, fmt.Sprintf(
					"annotation reads $labels.%s, which none of %s carries — it renders as an empty "+
						"string in the page", m[1], carriedBy))
			}
		}
	}
	return problems
}

func keysOf(m map[string]bool) string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return strings.Join(out, "|")
}

// TestExpressionsOnlyUseRealSeriesAndLabels is the check this whole package exists to enforce.
func TestExpressionsOnlyUseRealSeriesAndLabels(t *testing.T) {
	for _, r := range Rules() {
		for _, p := range checkRule(r) {
			t.Errorf("rule %s: %s", r.Name, p)
		}
	}
}

// TestCheckerCatchesInjectedFaults is the test of the test. Every case below is a REAL failure
// mode — five of them are literally what shipped in 0.5.x, or what nearly shipped on the
// dashboards — and every one is valid PromQL that Prometheus would accept without a murmur.
func TestCheckerCatchesInjectedFaults(t *testing.T) {
	base := Rules()[2] // RepositoryCheckFailed: a one-line expression, easy to corrupt precisely.

	for _, tc := range []struct {
		name string
		with func(*Rule)
	}{
		{
			// The original defect: a _total suffix on a gauge family.
			name: "series that does not exist",
			with: func(r *Rule) { r.Expr = "crystalbackup_repository_last_check_success_total == 0" },
		},
		{
			// A histogram suffix on a family that is not a histogram.
			name: "derived suffix on a non-histogram",
			with: func(r *Rule) { r.Expr = "crystalbackup_repository_stale_locks_bucket > 0" },
		},
		{
			name: "selector on a label the family does not carry",
			with: func(r *Rule) { r.Expr = `crystalbackup_repository_stale_locks{schedule="x"} > 0` },
		},
		{
			// The dashboards' near-miss: the API enum spelling instead of the emitted value.
			name: "label value the collector never emits",
			with: func(r *Rule) {
				r.Expr = `crystalbackup_repository_last_check_success{scope="Namespaced"} == 0`
			},
		},
		{
			name: "join on a label that does not exist",
			with: func(r *Rule) {
				r.Expr = "crystalbackup_repository_stale_locks > 0 and on (tenant) " +
					"crystalbackup_repository_size_bytes"
			},
		},
		{
			name: "annotation referencing a label of another family",
			with: func(r *Rule) { r.Summary = "stale locks on {{ $labels.pvc }}" },
		},
		{
			name: "expression naming no crystalbackup series at all",
			with: func(r *Rule) { r.Expr = "up == 0" },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			broken := base
			tc.with(&broken)
			if problems := checkRule(broken); len(problems) == 0 {
				t.Errorf("the checker passed %q — it would have let this through:\n  %s\n  %s",
					tc.name, broken.Expr, broken.Summary)
			}
		})
	}
}

// TestCheckerAcceptsWhatIsReal is the other half: a checker that rejects everything is no better
// than one that accepts everything, and would train people to work around it.
func TestCheckerAcceptsWhatIsReal(t *testing.T) {
	for _, tc := range []struct{ expr, summary string }{
		{"crystalbackup_repository_stale_locks > 0", "{{ $labels.location }}"},
		{`crystalbackup_repository_last_check_success{scope="namespace"} == 0`, "{{ $labels.scope }}"},
		// A histogram's derived _bucket series, and `le` — a label Prometheus attaches and no
		// collector declares.
		{"histogram_quantile(0.99, sum by (namespace, le) (rate(crystalbackup_backup_duration_seconds_bucket[1h])))",
			"{{ $labels.namespace }}"},
		{"increase(crystalbackup_backup_failures_total[1h]) > 0", "{{ $labels.schedule }}"},
		{"sum by (location, scope) (crystalbackup_repository_size_bytes)", "{{ $labels.location }}"},
	} {
		r := Rule{Name: "T", Expr: tc.expr, Summary: tc.summary}
		if problems := checkRule(r); len(problems) > 0 {
			t.Errorf("checker rejected a legitimate expression %q: %v", tc.expr, problems)
		}
	}
}

// TestTableIsWellFormed catches the boring mistakes that a generated file would otherwise carry
// straight into a cluster.
func TestTableIsWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, r := range Rules() {
		switch {
		case !strings.HasPrefix(r.Name, "Crystalbackup"):
			t.Errorf("%s: alert names are Crystalbackup-prefixed so they are unambiguous in a "+
				"shared Alertmanager", r.Name)
		case seen[r.Name]:
			t.Errorf("%s: duplicate alert name", r.Name)
		}
		seen[r.Name] = true

		if r.Severity != SeverityWarning && r.Severity != SeverityCritical {
			t.Errorf("%s: severity %q is neither warning nor critical", r.Name, r.Severity)
		}
		if r.Summary == "" || r.Description == "" {
			t.Errorf("%s: a rule with no summary or no remedy gets silenced instead of fixed", r.Name)
		}
		if r.Rationale == "" {
			t.Errorf("%s: no rationale — it is the only thing an operator can read at 03:00 before "+
				"deciding to silence this", r.Name)
		}
		if r.Threshold.Kind == "" {
			t.Errorf("%s: no threshold kind declared; lot J cannot evaluate it", r.Name)
		}
	}
	if len(seen) != 9 {
		t.Errorf("the table has %d rules; spec/05-observability.md §3 specifies 9. If a rule was "+
			"added or removed, update the spec in the same change.", len(seen))
	}
}

// TestThresholdsAreConsistentWithExpressions checks the half of the contract a compiler cannot:
// that the number in the expression is the number in Threshold. They are declared once and used
// twice, and this is what stops the second use drifting.
func TestThresholdsAreConsistentWithExpressions(t *testing.T) {
	for _, r := range Rules() {
		switch r.Threshold.Kind {
		case ThresholdAge:
			want := fmt.Sprintf("> %d", int64(r.Threshold.Age.Seconds()))
			if !strings.Contains(r.Expr, want) {
				t.Errorf("%s: declares Age=%s but the expression does not compare against %q",
					r.Name, r.Threshold.Age, want)
			}
		case ThresholdCount:
			want := fmt.Sprintf("> %d", int64(r.Threshold.Count))
			if !strings.Contains(r.Expr, want) {
				t.Errorf("%s: declares Count=%g but the expression does not compare against %q",
					r.Name, r.Threshold.Count, want)
			}
		case ThresholdPeriod:
			for _, want := range []string{
				fmt.Sprintf("* %g", r.Threshold.Factor),
				fmt.Sprintf("+ %d", int64(r.Threshold.Grace.Seconds())),
				fmt.Sprintf("> %d", int64(r.Threshold.Age.Seconds())),
			} {
				if !strings.Contains(r.Expr, want) {
					t.Errorf("%s: threshold declares %q, which the expression does not use", r.Name, want)
				}
			}
		case ThresholdState:
			if !strings.Contains(r.Expr, "== 0") {
				t.Errorf("%s: declared as a state test but the expression is not a boolean comparison",
					r.Name)
			}
		}
	}
}

// TestMatchingModifiersAgreeWithinARule is the invariant that would have caught the asymmetry M6
// found in the BackupMissed expression, where the threshold matched on five labels and the
// schedule_active guard on the three spec §3 had written.
//
// A rule that joins on one identity in one term and a coarser one in another is deciding, silently
// and per-term, which series count as the same thing. In BackupMissed the coarse guard let a
// `BackupSchedule` and a `ClusterBackupSchedule` sharing a name answer for each other. That
// mistake is invisible in review — both clauses read perfectly well on their own — and it is
// invisible in production too, because the wrong answer is still an answer.
//
// If a future rule genuinely needs different keys in different terms, this test has to be edited,
// and that is the point: the edit is where the reason gets written down.
func TestMatchingModifiersAgreeWithinARule(t *testing.T) {
	modifier := regexp.MustCompile(`\b(?:on|ignoring)\s*\(([^)]*)\)`)

	for _, r := range Rules() {
		var first []string
		for _, m := range modifier.FindAllStringSubmatch(r.Expr, -1) {
			keys := strings.Split(m[1], ",")
			for i := range keys {
				keys[i] = strings.TrimSpace(keys[i])
			}
			slices.Sort(keys)
			if first == nil {
				first = keys
				continue
			}
			if !slices.Equal(keys, first) {
				t.Errorf("rule %s matches on %v in one term and %v in another.\n"+
					"    Two identities in one expression means the rule decides per-term which "+
					"series are the same thing, and the looser term will eventually pair two that "+
					"are not.\n    If the difference is deliberate, say why here and in the "+
					"expression.", r.Name, first, keys)
			}
		}
	}
}

// TestBackupMissedGuardMatchesTheThresholdIdentity pins the specific correction, so a later
// "simplification" back to spec §3's three labels fails rather than reads fine.
func TestBackupMissedGuardMatchesTheThresholdIdentity(t *testing.T) {
	expr := ruleNamed(t, "CrystalbackupBackupMissed").Expr
	want := "on (" + strings.Join(missedJoinKeys, ", ") + ")"

	guard := "and " + want
	if !strings.Contains(expr, guard) {
		t.Errorf("the schedule_active guard must join on the same identity as the threshold (%s).\n"+
			"    On the narrower (namespace, schedule, cluster) of spec §3, a BackupSchedule and a "+
			"ClusterBackupSchedule sharing a name answer for each other.\n%s", want, expr)
	}
	// And there is no leftover of the narrow form anywhere in the expression.
	if strings.Contains(expr, "on ("+labelNamespace+", "+labelSchedule+", "+labelCluster+")") {
		t.Errorf("the three-label join from spec §3 is still present in the expression:\n%s", expr)
	}
}

// TestBackupMissedReadsTheSchedulesOwnPeriod pins the one behavioural correction this lot makes to
// spec §3: the deadline is derived, not fixed. If someone reverts it to a constant the rule still
// works, which is exactly why it needs a test — a weekly schedule paging every Tuesday is the kind
// of regression that gets fixed with a silence.
func TestBackupMissedReadsTheSchedulesOwnPeriod(t *testing.T) {
	missed := ruleNamed(t, "CrystalbackupBackupMissed")
	for _, series := range []string{
		metrics.NameSchedulePeriod,  // the per-schedule deadline
		metrics.NameScheduleCreated, // the never-succeeded fallback
		metrics.NameScheduleActive,  // the paused/deleted guard
		metrics.NameBackupLastSuccess,
	} {
		if !strings.Contains(missed.Expr, series) {
			t.Errorf("BackupMissed no longer reads %s; the expression is:\n%s", series, missed.Expr)
		}
	}
	if missed.Threshold.Kind != ThresholdPeriod {
		t.Errorf("BackupMissed threshold kind is %q, want %q — a fixed deadline is wrong for every "+
			"schedule that is not daily (spec §8 q3)", missed.Threshold.Kind, ThresholdPeriod)
	}
}

// ruleNamed fetches one rule from the table, failing loudly if it has been renamed away.
func ruleNamed(t *testing.T, name string) Rule {
	t.Helper()
	for _, r := range Rules() {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("%s is gone from the table", name)
	return Rule{}
}
