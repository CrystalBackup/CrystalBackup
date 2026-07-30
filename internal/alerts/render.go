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
	"strconv"
	"strings"
	"time"
)

// RulesFile is the chart path the generated rule group is written to, relative to the chart
// directory. It sits OUTSIDE templates/ on purpose: the annotations are full of Prometheus's own
// {{ $labels.namespace }} Go templates, and anything under templates/ would be run through Helm's
// engine, which would either error or silently blank them. templates/prometheusrule.yaml pulls it
// in with .Files.Get, exactly as templates/dashboards.yaml does for the dashboard JSON.
const RulesFile = "rules/crystalbackup.rules.yaml"

const generatedHeader = `# GENERATED FILE — do not edit.
#
# Source of truth: internal/alerts/rules.go. Regenerate with ` + "`make alert-rules`" + `.
#
# These rules are generated rather than written because five of the nine shipped as prose in
# spec/05-observability.md §3 read series the operator has never emitted: valid PromQL, evaluated
# without error, unable to fire, and invisible to every check in the build. Every expression below
# is assembled from the constants in internal/metrics/names.go, so a renamed series is now a
# compile error instead of an alert that quietly stops firing.
#
# Editing this file by hand puts that guarantee back where it was.
`

// Render turns the rule table into the PrometheusRule `spec` body: a `groups:` document, ready to
// be dropped under `spec:` in the chart template.
//
// Hand-rolled rather than marshalled through a YAML library for one reason: the per-rule Rationale
// comments. They are the only place an operator being paged at 03:00 can read why a rule exists
// before deciding to silence it, and a struct marshaller drops comments on the floor.
func Render() []byte {
	var b strings.Builder
	b.WriteString(generatedHeader)
	b.WriteString("groups:\n")
	fmt.Fprintf(&b, "  - name: %s\n", GroupName)
	b.WriteString("    rules:\n")

	for _, r := range Rules() {
		for line := range strings.SplitSeq(r.Rationale, "\n") {
			if line == "" {
				b.WriteString("      #\n")
				continue
			}
			fmt.Fprintf(&b, "      # %s\n", line)
		}
		fmt.Fprintf(&b, "      - alert: %s\n", r.Name)
		writeExpr(&b, r.Expr)
		if r.For > 0 {
			fmt.Fprintf(&b, "        for: %s\n", promDuration(r.For))
		}
		b.WriteString("        labels:\n")
		fmt.Fprintf(&b, "          severity: %s\n", r.Severity)
		b.WriteString("        annotations:\n")
		fmt.Fprintf(&b, "          summary: %s\n", yamlString(r.Summary))
		if r.Description != "" {
			fmt.Fprintf(&b, "          description: %s\n", yamlString(r.Description))
		}
	}
	return []byte(b.String())
}

// writeExpr emits the expression as a literal block scalar. Always a block, even for a one-liner:
// a PromQL expression full of `{`, `"` and `>` is a minefield of YAML quoting rules, and a block
// scalar has none of them.
func writeExpr(b *strings.Builder, expr string) {
	b.WriteString("        expr: |\n")
	for line := range strings.SplitSeq(strings.TrimRight(expr, "\n"), "\n") {
		if line == "" {
			b.WriteString("\n")
			continue
		}
		fmt.Fprintf(b, "          %s\n", line)
	}
}

// yamlString double-quotes a scalar. strconv.Quote's escaping is a subset of YAML's double-quoted
// style (\" \\ \n \t and \uXXXX all mean the same thing in both) and it leaves printable non-ASCII
// — the arrow in the external-sync summary — alone. render_test.go parses the result back with a
// real YAML parser and compares it to the table, so this is checked rather than assumed.
func yamlString(s string) string { return strconv.Quote(s) }

// promDuration formats a duration the way Prometheus writes them: `15m`, not Go's `15m0s`. Both
// parse, but a rule file is read by people.
func promDuration(d time.Duration) string {
	switch {
	case d == 0:
		return "0s"
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", int64(d/time.Hour))
	case d%time.Minute == 0:
		return fmt.Sprintf("%dm", int64(d/time.Minute))
	default:
		return fmt.Sprintf("%ds", int64(d/time.Second))
	}
}
