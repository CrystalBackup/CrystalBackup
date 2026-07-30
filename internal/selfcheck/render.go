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
	"bytes"
	_ "embed"
	"fmt"
	"html/template"
	"strings"
	"time"
)

// reportCSS is the project's own report stylesheet, the one website/public/reports/*.html has used
// since M1. It is EMBEDDED rather than linked for the property the whole `report` subcommand exists
// for: the output has to be one file that renders identically on a laptop with no network, because
// the reader is a maintainer opening an attachment from an issue.
//
// No CDN, no web font, no remote image, no script. The dark-mode block is kept because a report is
// read at three in the morning as often as not.
//
//go:embed report.css
var reportCSS string

//go:embed report.html.tmpl
var reportTemplate string

// Render turns a report — parsed from JSON, with no cluster anywhere in sight — into one
// self-contained HTML page.
//
// The separation is the point of the two subcommands. `selfcheck` needs a cluster and produces
// JSON; `report` needs a file and produces a page. Someone attaches the JSON to an issue and a
// maintainer regenerates the page at their desk, which is only possible because nothing in here
// reaches for a client, a network or a clock.
func Render(rep *Report) ([]byte, error) {
	t, err := template.New("report").Funcs(template.FuncMap{
		"css":        func() template.CSS { return template.CSS(reportCSS) },
		"ts":         formatTime,
		"tsp":        formatTimePtr,
		"bytes":      humanBytes,
		"hours":      humanHours,
		"days":       humanDays,
		"statusWord": statusWord,
		"statusRank": statusRank,
		"sevClass":   sevClass,
		"boolWord":   boolWord,
		"labelList":  labelList,
		"pct":        func(a, b int) int { return percent(a, b) },
		"has":        positive,
		"trip":       tristate,
	}).Parse(reportTemplate)
	if err != nil {
		return nil, fmt.Errorf("parse report template: %w", err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, rep); err != nil {
		return nil, fmt.Errorf("render report: %w", err)
	}
	return buf.Bytes(), nil
}

// neverWord is what an absent timestamp reads as. One spelling, because "never", "-" and "n/a"
// scattered across a document make a reader wonder whether they mean different things.
const neverWord = "never"

func formatTime(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.UTC().Format("2006-01-02 15:04:05 MST")
}

func formatTimePtr(t *time.Time) string {
	if t == nil {
		return neverWord
	}
	return formatTime(*t)
}

// humanHours renders the -1 sentinel the collector uses for "no such timestamp" as the word, not as
// a negative number. A report that printed "-1.0 h ago" would be worse than one that printed
// nothing.
func humanHours(h float64) string {
	switch {
	case h < 0:
		return neverWord
	case h < 1:
		return fmt.Sprintf("%.0f min", h*60)
	case h < 48:
		return fmt.Sprintf("%.1f h", h)
	default:
		return fmt.Sprintf("%.1f d", h/24)
	}
}

func humanDays(d int) string {
	if d < 0 {
		return neverWord
	}
	return fmt.Sprintf("%d d", d)
}

func humanBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}

// statusWord and sevClass map the machine values onto the stylesheet's own vocabulary. They live
// here rather than in the template so that the template stays a layout and the words stay
// reviewable.
func statusWord(s string) string {
	switch s {
	case StatusOK:
		return "OK"
	case StatusBreached:
		return "BREACHED"
	case StatusNotEvaluated:
		return "NOT EVALUATED"
	case StatusError:
		return "ERROR"
	default:
		return strings.ToUpper(s)
	}
}

// statusRank gives the stylesheet a severity class per verdict. Not-evaluated deliberately gets the
// same visual weight as a warning rather than the muted grey of a pass: it is the one status a
// reader is most tempted to skim past, and skimming past it is exactly how an unmeasured OK gets
// read as an OK.
func statusRank(s string) string {
	switch s {
	case StatusOK:
		return "ok"
	case StatusBreached:
		return "crit"
	case StatusNotEvaluated:
		return "warn"
	case StatusError:
		return "warn"
	default:
		return "low"
	}
}

func sevClass(sev string) string {
	if sev == "critical" {
		return "c"
	}
	return "h"
}

func boolWord(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// tristate renders the *bool fields whose nil means "never measured". The whole reason those
// fields are pointers is that nil is not false, and a renderer that printed "no" for both would
// undo it.
func tristate(b *bool) string {
	switch {
	case b == nil:
		return "never scanned"
	case *b:
		return "clean"
	default:
		return "FAILED"
	}
}

func labelList(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	// Sorted so two reports of the same breach render identically.
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		v := labels[k]
		if v == "" {
			v = "∅"
		}
		parts = append(parts, k+"="+v)
	}
	return strings.Join(parts, " · ")
}

// positive is the template's "is this count non-zero" test. It takes any of the integer widths the
// report carries (int32 comes straight off the API types) rather than forcing conversions into the
// template, where a type mismatch surfaces as a render error at the worst possible moment.
func positive(v any) bool {
	switch n := v.(type) {
	case int:
		return n > 0
	case int32:
		return n > 0
	case int64:
		return n > 0
	case float64:
		return n > 0
	default:
		return false
	}
}

func percent(a, b int) int {
	if b == 0 {
		return 0
	}
	return a * 100 / b
}
