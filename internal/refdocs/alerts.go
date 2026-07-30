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
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/CrystalBackup/CrystalBackup/internal/alerts"
)

// AlertsPage is the site path the alerts reference is written to, relative to the repository root.
const AlertsPage = "website/src/content/docs/reference/alerts.md"

// RuleCount is how many rules the page documents, for the generator's summary line.
func RuleCount() int { return len(alerts.Rules()) }

// RenderAlerts builds the whole Alerts reference page from the rule table.
func RenderAlerts() ([]byte, error) {
	rules := alerts.Rules()
	if len(rules) == 0 {
		return nil, fmt.Errorf("alerts.Rules() is empty; the page would document no rules at all")
	}

	var b strings.Builder
	b.WriteString(frontMatter(
		"Alerts",
		"The alert rules shipped in the chart: what each one fires on, and why its threshold is "+
			"the number it is — generated from internal/alerts.",
	))
	b.WriteString(generatedNotice)
	fmt.Fprintf(&b, alertsIntro, len(rules))

	b.WriteString("## The rules at a glance\n\n")
	b.WriteString("| Alert | Severity | `for` | Fires when |\n| --- | --- | --- | --- |\n")
	for _, r := range rules {
		fmt.Fprintf(&b, "| [%s](#%s) | %s | %s | %s |\n",
			r.Name, anchor(r.Name), severityCell(r.Severity), forCell(r.For), escapeCell(threshold(r)))
	}
	b.WriteString("\n")

	for _, r := range rules {
		writeRule(&b, r)
	}

	b.WriteString(alertsOutro)
	return []byte(b.String()), nil
}

func writeRule(b *strings.Builder, r alerts.Rule) {
	fmt.Fprintf(b, "## %s\n\n", r.Name)
	fmt.Fprintf(b, "**Severity** %s · **`for`** %s · **Threshold** %s\n\n",
		severityCell(r.Severity), forCell(r.For), threshold(r))

	// The summary annotation verbatim, template placeholders and all: it is the line Alertmanager
	// will actually put in front of whoever is woken up.
	fmt.Fprintf(b, "> %s\n\n", prose(r.Summary))
	if r.Description != "" {
		fmt.Fprintf(b, "**What to do.** %s\n\n", prose(r.Description))
	}

	b.WriteString("```promql\n" + strings.TrimRight(r.Expr, "\n") + "\n```\n\n")

	b.WriteString(":::note[Why this threshold]\n")
	b.WriteString(strings.TrimRight(markdownRationale(r.Rationale), "\n") + "\n")
	b.WriteString(":::\n\n")

	if f := alerts.Fidelity(r.Name); f != "" {
		b.WriteString(":::caution[The offline self-check answers this one approximately]\n")
		b.WriteString(prose(f) + "\n")
		b.WriteString(":::\n\n")
	}
}

// threshold renders a rule's bound in words. It reads the Threshold field rather than the
// expression, which is the point of that field existing: the number is declared once and every
// consumer — the PromQL, the offline predicate, and now this page — reads the same one.
func threshold(r alerts.Rule) string {
	t := r.Threshold
	switch t.Kind {
	case alerts.ThresholdState:
		return "a state gauge reports the bad state (no numeric bound)"
	case alerts.ThresholdAge:
		return "nothing has happened for " + promDuration(t.Age)
	case alerts.ThresholdCount:
		return "the measured value goes above " + strconv.FormatFloat(t.Count, 'f', -1, 64)
	case alerts.ThresholdPeriod:
		return fmt.Sprintf("nothing has happened for %s × the schedule's own period + %s (falling back to %s)",
			strconv.FormatFloat(t.Factor, 'f', -1, 64), promDuration(t.Grace), promDuration(t.Age))
	default:
		return string(t.Kind)
	}
}

func severityCell(s alerts.Severity) string {
	if s == alerts.SeverityCritical {
		return "**critical**"
	}
	return string(s)
}

// forCell renders the hold. Zero is not "unset": it is the deliberate choice for a rule whose
// expression is already an over-time aggregation, where a hold would only delay a signal that is
// an hour old by construction.
func forCell(d time.Duration) string {
	if d == 0 {
		return "none — fires on the first evaluation"
	}
	return "`" + promDuration(d) + "`"
}

// seriesInProse matches a crystalbackup_ series name written bare in a rationale or an annotation.
// The rationales name series constantly — that is what makes them worth reading — and they are Go
// string literals, so nothing there is marked up.
var seriesInProse = regexp.MustCompile(`crystalbackup_[a-z0-9_]+`)

// prose prepares a Go string literal for markdown: series names become code spans, and a stray
// `<` is escaped so a future rationale mentioning `<nothing>` outside backticks cannot silently
// eat the rest of a sentence as an HTML tag.
//
// Both transformations skip anything already inside backticks, so an author who marks something up
// by hand is left alone.
func prose(s string) string {
	if strings.Count(s, "`")%2 != 0 {
		// Unbalanced backticks in the source string. Rewriting around them would guess at where
		// the code span was meant to end; refdocs_test.go fails the build on this instead.
		return s
	}
	var out strings.Builder
	inCode := false
	for segment := range strings.SplitSeq(s, "`") {
		if inCode {
			out.WriteString("`" + segment + "`")
		} else {
			segment = strings.ReplaceAll(segment, "<", "&lt;")
			out.WriteString(seriesInProse.ReplaceAllString(segment, "`$0`"))
		}
		inCode = !inCode
	}
	// An odd number of backticks would have left the last span unterminated; Go string literals
	// with unbalanced backticks are a bug in the source, not something to paper over.
	return out.String()
}

// anchor mirrors the id Starlight derives from a heading, for the overview table's links.
func anchor(heading string) string {
	return strings.ToLower(heading)
}

// markdownRationale turns a Rationale — hard-wrapped prose with occasional `  * ` bullets — into
// markdown that reflows, without losing the bullet structure.
//
// The Rationale is the single most valuable thing on this page, and the reason it is worth
// generating rather than summarising. It is what an operator being paged at 03:00 reads before
// deciding whether to silence a rule, and it is the only place the answer to "why 26 hours and not
// 24" is written down at all.
func markdownRationale(rationale string) string {
	var out strings.Builder
	var para []string
	var bullets [][]string

	flushPara := func() {
		if len(para) > 0 {
			out.WriteString(prose(strings.Join(para, " ")) + "\n\n")
			para = nil
		}
	}
	flushBullets := func() {
		for _, item := range bullets {
			out.WriteString("- " + prose(strings.Join(item, " ")) + "\n")
		}
		if len(bullets) > 0 {
			out.WriteString("\n")
			bullets = nil
		}
	}

	for line := range strings.SplitSeq(rationale, "\n") {
		trimmed := strings.TrimSpace(line)
		indented := strings.HasPrefix(line, "  ")
		switch {
		case trimmed == "":
			flushBullets()
			flushPara()
		case indented && strings.HasPrefix(trimmed, "* "):
			flushPara()
			bullets = append(bullets, []string{strings.TrimPrefix(trimmed, "* ")})
		case indented && len(bullets) > 0:
			bullets[len(bullets)-1] = append(bullets[len(bullets)-1], trimmed)
		default:
			flushBullets()
			// A CAVEAT is the one thing in a rationale that must not be buried mid-paragraph: it
			// is where a rule admits what it cannot see. Own paragraph, and the marker in bold.
			if strings.HasPrefix(trimmed, "CAVEAT:") {
				flushPara()
				trimmed = "**CAVEAT:**" + strings.TrimPrefix(trimmed, "CAVEAT:")
			}
			para = append(para, trimmed)
		}
	}
	flushBullets()
	flushPara()
	return out.String()
}

// %d is the rule count, read from the table rather than typed.
const alertsIntro = `%d alert rules ship with the chart. This page is generated from
` + "`internal/alerts`" + `, so every expression, threshold and annotation below is the one the
chart actually installs — not a transcription of it.

Each rule's expression is assembled in Go from the series-name constants the collectors use, which
is why they can be trusted to *match something*. Before that was true, five of the nine rules this
table replaced read series the operator has never emitted: valid PromQL, evaluated without error,
unable to fire, and invisible to every check in the build.

## Turning them on

` + "```yaml" + `
metrics:
  rules:
    enabled: true
    labels:
      release: kube-prometheus-stack   # must match your Prometheus' ruleSelector
` + "```" + `

They are **off by default**, like the ServiceMonitor, for two reasons: they need the
` + "`monitoring.coreos.com`" + ` CRDs, and thresholds are platform policy. Turn them on
deliberately, having read what they will page you about.

The chart installs them as a single ` + "`PrometheusRule`" + ` for the Prometheus Operator, in one
group named ` + "`crystalbackup`" + `. Two knobs matter more than they look:

- ` + "`metrics.rules.labels`" + ` — a Prometheus Operator only picks up rules matching its own
  ` + "`ruleSelector`" + `. An unlabelled PrometheusRule installs cleanly, validates, and is
  completely ignored.
- ` + "`metrics.rules.namespace`" + ` — set it when the operator's ` + "`ruleNamespaceSelector`" + `
  only covers the monitoring namespace. Empty means the operator's own namespace.

If you do not run the Prometheus Operator, the rule bodies are a plain rule group in the chart at
` + "`rules/crystalbackup.rules.yaml`" + `. ` + "`helm show`" + ` it, or read it in the repository,
and load it into Prometheus however you normally do.

## Routing by tenant

Every rule that can name a tenant carries ` + "`namespace`" + ` and ` + "`tenant`" + ` labels
through to the alert, and the repository, discovery and external-sync rules carry
` + "`scope`" + ` as well. That is what a per-tenant route matches on:

` + "```yaml" + `
routes:
  - matchers: [ 'alertname=~"Crystalbackup.*"', 'scope="namespace"', 'namespace=~"team-.*"' ]
    receiver: tenant-oncall
  - matchers: [ 'alertname=~"Crystalbackup.*"' ]
    receiver: platform-oncall
` + "```" + `

Order matters: cluster-plane series carry an empty ` + "`namespace`" + ` and
` + "`scope=\"cluster\"`" + `, so the tenant route must be the specific one and the platform route
the catch-all. See the [label contract](/CrystalBackup/docs/reference/metrics/#the-label-contract)
for what each label can hold — in particular that ` + "`scope`" + ` is lowercase and is *not* the
API's ` + "`Cluster|Namespaced`" + ` enum.

## These expressions have been run

Every rule below has promtool unit tests under ` + "`internal/alerts/testdata/`" + `, run in CI by
` + "`make test-alert-rules`" + `. Each one is fed synthetic series and asserted in both
directions: a case that makes it fire, and a case just under its threshold — or just inside its
` + "`for`" + ` hold — that must stay silent. The absence cases are tested too, because "a
repository that has never been checked must not page" is a property of the collector and the rule
*together*, and only an evaluation can hold them to it.

A rule with no test fails the build rather than passing by not being mentioned. That is enforced
separately, by ` + "`make alert-rules-covered`" + `.

`

const alertsOutro = `## What no alert here can tell you

None of these verify that a **restore works**. ` + "`restic check`" + ` verifies that a repository
is readable; it does not verify that restoring it produces a working application, and no expression
over these series ever will.

Restore drills are the administrator's job, on a real cadence. See
[Maintenance and verification](/CrystalBackup/docs/guides/maintenance/#restore-drills-are-yours).

## See also

- [Metrics](/CrystalBackup/docs/reference/metrics/) — the series these rules read, and what the
  absence of one means.
- [Helm values](/CrystalBackup/docs/reference/helm-values/) — every ` + "`metrics.rules.*`" + ` knob.
- [Observability](/CrystalBackup/docs/guides/observability/) — scraping, logs, and the conditions
  that say *why*.
`
