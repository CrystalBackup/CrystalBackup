//go:build crucible

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

package crucible

import (
	"cmp"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	"github.com/onsi/ginkgo/v2/types"
)

// This file turns Ginkgo's developer-oriented output into a short, plain-language
// report aimed at whoever *triggered* the run (via `mise run test`). Ginkgo already
// tells a developer what happened; this tells an operator what it MEANS:
//
//   - a one-line verdict (is the platform good? did the milestone pass?),
//   - checks grouped by area ("Platform", "Milestone M0", ...) in prose,
//   - every failure with a concrete reason and a next step,
//   - a closing interpretation so the reader knows what to do next.
//
// It is written to $CRUCIBLE_REPORT_PATH (the mise `test` task points this at
// test/crucible/artifacts/crucible-report.md and prints it last), so the reader
// sees it even though `go test` hides a passing binary's stdout.

var milestoneLabel = regexp.MustCompile(`^m\d+$`)

// areaTitle maps a milestone label to a human section heading + the plain-English
// question that area answers.
func areaTitle(label string) (heading, question string) {
	switch label {
	case "infra":
		return "Platform", "is the live cluster itself healthy enough to trust the rest?"
	case "m0":
		return "Milestone M0", "are the CRDs and the Helm chart correctly installed on a real cluster?"
	}
	if milestoneLabel.MatchString(label) {
		return "Milestone " + strings.ToUpper(label),
			"does the " + strings.ToUpper(label) + " acceptance suite pass on real infrastructure?"
	}
	return "Other checks", "additional checks not tied to a milestone."
}

// areaOf returns the milestone/area label a spec belongs to (infra, m0, m1, ...),
// or "" when it carries none of them.
func areaOf(labels []string) string {
	best := ""
	for _, l := range labels {
		if l == "infra" {
			return "infra" // platform always sorts first and wins ties
		}
		if milestoneLabel.MatchString(l) && (best == "" || l < best) {
			best = l
		}
	}
	return best
}

// areaRank orders sections: platform first, then milestones numerically, then the rest.
func areaRank(label string) int {
	if label == "infra" {
		return -1
	}
	if milestoneLabel.MatchString(label) {
		n := 0
		_, _ = fmt.Sscanf(label, "m%d", &n)
		return n
	}
	return 1 << 20
}

type checkLine struct {
	desc     string
	state    types.SpecState
	dur      time.Duration
	detail   string // skip reason or failure headline
	location string
}

func mark(s types.SpecState) string {
	switch {
	case s.Is(types.SpecStatePassed):
		return "✅"
	case s.Is(types.SpecStateSkipped):
		return "⏭️ "
	case s.Is(types.SpecStatePending):
		return "🚧"
	default: // Failed, Panicked, Aborted, Interrupted, Timedout
		return "❌"
	}
}

// firstLine trims a possibly-multiline Ginkgo message to a single readable line.
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:197] + "…"
	}
	return s
}

var _ = ReportAfterSuite("crucible readable report", func(report Report) {
	out := renderCrucibleReport(report)

	// Surface it in the Ginkgo stream (visible with --ginkgo.v)...
	_, _ = fmt.Fprintln(GinkgoWriter, "\n"+out)

	// ...and write the file the mise `test` task prints last (visible even when
	// `go test` swallows a passing binary's stdout).
	if path := os.Getenv("CRUCIBLE_REPORT_PATH"); path != "" {
		if err := os.WriteFile(path, []byte(out), 0o644); err != nil {
			_, _ = fmt.Fprintf(GinkgoWriter, "warning: could not write %s: %v\n", path, err)
		}
	}
})

// renderCrucibleReport turns a Ginkgo suite report into the operator-facing
// markdown described above. Pure (no I/O) so it can be unit-tested offline.
func renderCrucibleReport(report types.Report) string {
	var b strings.Builder

	// ── Header ────────────────────────────────────────────────────────────
	b.WriteString("# Crystal Backup — Crucible report\n\n")
	b.WriteString("_Real-conditions end-to-end run against a live Hetzner cluster._\n\n")

	// ── Tally + verdict ───────────────────────────────────────────────────
	var passed, failed, skipped, pending int
	var setupFailed bool
	byArea := map[string][]checkLine{}
	failures := []checkLine{}

	for _, sr := range report.SpecReports {
		switch sr.LeafNodeType {
		case types.NodeTypeIt:
			// a real check — accounted below
		case types.NodeTypeBeforeSuite, types.NodeTypeSynchronizedBeforeSuite:
			if sr.State.Is(types.SpecStateFailureStates) {
				setupFailed = true
				failures = append(failures, checkLine{
					desc:     "suite setup (BeforeSuite)",
					state:    sr.State,
					detail:   firstLine(sr.Failure.Message),
					location: sr.Failure.Location.String(),
				})
			}
			continue
		default:
			continue // AfterSuite, ReportAfterSuite, container nodes, ...
		}

		line := checkLine{
			desc:     sr.LeafNodeText,
			state:    sr.State,
			dur:      sr.RunTime,
			location: sr.LeafNodeLocation.String(),
		}
		switch {
		case sr.State.Is(types.SpecStatePassed):
			passed++
		case sr.State.Is(types.SpecStateSkipped):
			skipped++
			line.detail = firstLine(sr.Failure.Message) // Skip() reason lands here
		case sr.State.Is(types.SpecStatePending):
			pending++
		default:
			failed++
			line.detail = firstLine(sr.Failure.Message)
			if loc := sr.Failure.Location.String(); loc != "" {
				line.location = loc // where the assertion actually blew up
			}
			failures = append(failures, line)
		}

		area := areaOf(sr.Labels())
		byArea[area] = append(byArea[area], line)
	}

	verdict := "✅ PASS"
	switch {
	case setupFailed:
		verdict = "❌ SETUP FAILED"
	case failed > 0:
		verdict = "❌ FAIL"
	}

	fmt.Fprintf(&b, "## Verdict: %s\n\n", verdict)
	fmt.Fprintf(&b, "**%d passed · %d failed · %d skipped**", passed, failed, skipped)
	if pending > 0 {
		fmt.Fprintf(&b, " · %d pending", pending)
	}
	fmt.Fprintf(&b, "  —  ran in %s\n\n", report.RunTime.Round(time.Second))

	// ── Per-area detail ───────────────────────────────────────────────────
	areas := make([]string, 0, len(byArea))
	for a := range byArea {
		areas = append(areas, a)
	}
	slices.SortFunc(areas, func(a, b string) int { return cmp.Compare(areaRank(a), areaRank(b)) })

	for _, area := range areas {
		heading, question := areaTitle(area)
		label := area
		if label == "" {
			label = "unlabeled"
		}
		fmt.Fprintf(&b, "## %s  `[%s]`\n\n", heading, label)
		fmt.Fprintf(&b, "_%s_\n\n", question)
		for _, c := range byArea[area] {
			fmt.Fprintf(&b, "- %s  %s", mark(c.state), c.desc)
			switch {
			case c.state.Is(types.SpecStatePassed):
				fmt.Fprintf(&b, "  _(%s)_", c.dur.Round(100*time.Millisecond))
			case c.state.Is(types.SpecStateSkipped):
				if c.detail != "" {
					fmt.Fprintf(&b, "  — skipped: %s", c.detail)
				} else {
					b.WriteString("  — skipped")
				}
			default:
				if c.detail != "" {
					fmt.Fprintf(&b, "  — %s", c.detail)
				}
			}
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}

	// ── Failures, with where to look ──────────────────────────────────────
	if len(failures) > 0 {
		fmt.Fprintf(&b, "## Failures (%d)\n\n", len(failures))
		for i, f := range failures {
			fmt.Fprintf(&b, "%d. **%s**\n", i+1, f.desc)
			if f.detail != "" {
				fmt.Fprintf(&b, "   - reason: %s\n", f.detail)
			}
			if f.location != "" {
				fmt.Fprintf(&b, "   - at: %s\n", f.location)
			}
		}
		b.WriteString("\n")
	}

	// ── What this means (the interpretation the operator actually needs) ───
	b.WriteString("## What this means\n\n")
	infraFailed := false
	for _, c := range byArea["infra"] {
		if c.state.Is(types.SpecStateFailureStates) {
			infraFailed = true
		}
	}
	switch {
	case setupFailed:
		b.WriteString("The suite could not even connect to the cluster. Check that `mise run up` " +
			"finished and that `artifacts/kubeconfig` points at a reachable cluster " +
			"(`mise run status`).\n")
	case infraFailed:
		b.WriteString("**The platform itself is unhealthy** — one or more `infra` checks failed. " +
			"Fix the platform before reading any milestone result: a failing storage class or a " +
			"degraded Ceph will make product tests fail for reasons that have nothing to do with " +
			"Crystal Backup. Start with `mise run status`, then the Troubleshooting section of " +
			"`test/crucible/README.md`.\n")
	case failed > 0:
		b.WriteString("The platform is healthy but **a milestone acceptance check failed** — this is " +
			"a real regression or a genuine gap for that milestone. See the Failures section above " +
			"for the exact check and location.\n")
	default:
		b.WriteString("All platform and milestone checks passed on real infrastructure — the cluster " +
			"is sound and the current milestone's acceptance criteria hold. Safe to treat this run " +
			"as a green non-regression gate.\n")
	}
	if skipped > 0 {
		b.WriteString("\n_Skipped checks are conditional (e.g. the operator-readiness check waits for a " +
			"released image on GHCR); each line above says why it was skipped — a skip is not a failure._\n")
	}

	return b.String()
}
