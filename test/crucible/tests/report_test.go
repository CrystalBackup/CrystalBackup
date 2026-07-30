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
//   - what the run COVERED, and explicitly what it did not,
//   - checks grouped by area ("Platform", "Milestone M0", ...) in prose,
//   - every failure with a concrete reason and a next step,
//   - a closing interpretation so the reader knows what to do next.
//
// It is written to $CRUCIBLE_REPORT_PATH (the mise `test` task points this at
// test/crucible/artifacts/crucible-report.md and prints it last), so the reader
// sees it even though `go test` hides a passing binary's stdout.
//
// # Green is not the same thing as covered
//
// This report used to answer one question — "did anything fail?" — and print
// "Safe to treat this run as a green non-regression gate" whenever the answer was
// no. `mise run test m4` therefore produced, in its own headline, "2 passed ·
// 0 failed · 81 skipped" followed by a declaration that the whole platform was
// sound. Two specs out of eighty-three, and the document that gets re-read three
// weeks later to decide a release said everything was fine.
//
// That is this project's most expensive recurring bug, in its fourth incarnation:
// a probe skipped for twelve releases behind an undocumented variable, a seed that
// swallowed a setfattr failure, an empty variable that turned into a wall of skips,
// a `command -v promtool || exit 0`. Every time, silence read as health.
//
// So the report now separates two questions that were conflated:
//
//	did everything that RAN pass?      → the verdict's colour
//	did everything RUN?                → the verdict's SCOPE
//
// and only a run that answers yes to both is allowed to use the words
// "non-regression gate". A filtered run is reported as a PARTIAL PASS: nothing
// failed, nothing is alarming, and it proves exactly the checks it ran and no
// others — which it then names.
//
// The distinction rests on a fact about Ginkgo: a spec deselected by a label
// filter, a --focus or a --skip lands in the report as Skipped with an EMPTY
// failure message, whereas a spec that reached its body and called Skip("reason")
// carries that reason. Two very different events that the old tally added into one
// "81 skipped" number. See specSelection.

var milestoneLabel = regexp.MustCompile(`^m\d+$`)

// fullRunSkipTolerance — on an UNFILTERED run, up to this share of the suite may skip
// itself conditionally (no S3 configured, no snapshot-capable class, ...) and the run
// still counts as a non-regression gate for what it covered. Past it, the suite is
// mostly abstaining and "green" gets qualified rather than announced.
const fullRunSkipTolerance = 0.10

// specSelection classifies a spec report into the four outcomes the reader cares
// about — and, crucially, splits Ginkgo's single "skipped" state in two.
type specSelection int

const (
	selRan        specSelection = iota // executed: passed, or failed
	selSkippedFor                      // reached its body and called Skip("reason") — a stated abstention
	selDeselected                      // never ran: filtered out, or cut short by fail-fast/interrupt
	selPending                         // XIt / Pending — never written to run
)

// classify returns how a spec report should be counted.
//
// The load-bearing line is the Skipped case: Ginkgo reports a filter-deselected spec
// as Skipped with an empty Failure (internal/group.go evaluateSkipStatus returns
// `types.SpecStateSkipped, types.Failure{}` for spec.Skip), while Skip("reason") goes
// through the Failer and arrives with the reason in Failure.Message. Adding the two
// together is what let a run of 2 specs describe itself as a whole-platform gate.
func classify(sr types.SpecReport) specSelection {
	switch {
	case sr.State.Is(types.SpecStatePending):
		return selPending
	case sr.State.Is(types.SpecStateSkipped):
		if strings.TrimSpace(sr.Failure.Message) == "" {
			return selDeselected
		}
		return selSkippedFor
	default:
		return selRan
	}
}

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
	sel      specSelection
	dur      time.Duration
	detail   string // skip reason or failure headline
	location string
}

func mark(c checkLine) string {
	switch {
	case c.sel == selDeselected:
		return "➖"
	case c.state.Is(types.SpecStatePassed):
		return "✅"
	case c.state.Is(types.SpecStateSkipped):
		return "⏭️ "
	case c.state.Is(types.SpecStatePending):
		return "🚧"
	default: // Failed, Panicked, Aborted, Interrupted, Timedout
		return "❌"
	}
}

// areaStats is the per-area coverage tally the "not exercised" section reads.
type areaStats struct {
	label      string
	total      int
	ran        int
	deselected int
}

// plural is a tiny helper so the prose reads like prose ("1 check", "12 checks").
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// pct renders a coverage share the way an operator skims it.
func pct(part, whole int) string {
	if whole == 0 {
		return "0%"
	}
	return fmt.Sprintf("%.0f%%", 100*float64(part)/float64(whole))
}

// hasFilter reports whether this run was deliberately narrowed. It matters because
// Ginkgo also reports fail-fast/interrupt casualties as "skipped with no reason", and
// telling an operator their interrupted run "was filtered" would be a second lie on
// top of the one this file exists to remove.
func hasFilter(cfg types.SuiteConfig) bool {
	return strings.TrimSpace(cfg.LabelFilter) != "" ||
		len(cfg.FocusStrings) > 0 || len(cfg.SkipStrings) > 0 ||
		len(cfg.FocusFiles) > 0 || len(cfg.SkipFiles) > 0
}

// filterDescription names, in the operator's words, WHY specs were left out — so the
// reader can tell a deliberate `mise run test m4` from an accidental over-narrow filter.
func filterDescription(cfg types.SuiteConfig) string {
	var parts []string
	if f := strings.TrimSpace(cfg.LabelFilter); f != "" {
		parts = append(parts, fmt.Sprintf("label filter `%s`", f))
	}
	if len(cfg.FocusStrings) > 0 {
		parts = append(parts, fmt.Sprintf("--focus `%s`", strings.Join(cfg.FocusStrings, "`, `")))
	}
	if len(cfg.SkipStrings) > 0 {
		parts = append(parts, fmt.Sprintf("--skip `%s`", strings.Join(cfg.SkipStrings, "`, `")))
	}
	if len(cfg.FocusFiles) > 0 || len(cfg.SkipFiles) > 0 {
		parts = append(parts, "a file filter")
	}
	if len(parts) == 0 {
		return "a filter"
	}
	return strings.Join(parts, " + ")
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
	// Four counters, not three: `deselected` (never selected by this run's filters)
	// is kept apart from `skippedFor` (the spec ran and abstained with a reason).
	// Only the second is a statement about the system under test.
	var passed, failed, skippedFor, deselected, pending int
	var setupFailed bool
	byArea := map[string][]checkLine{}
	areaOrder := []string{}
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
					sel:      selRan,
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
			sel:      classify(sr),
			dur:      sr.RunTime,
			location: sr.LeafNodeLocation.String(),
		}
		switch line.sel {
		case selDeselected:
			deselected++
		case selSkippedFor:
			skippedFor++
			line.detail = firstLine(sr.Failure.Message) // Skip() reason lands here
		case selPending:
			pending++
		default:
			if sr.State.Is(types.SpecStatePassed) {
				passed++
			} else {
				failed++
				line.detail = firstLine(sr.Failure.Message)
				if loc := sr.Failure.Location.String(); loc != "" {
					line.location = loc // where the assertion actually blew up
				}
				failures = append(failures, line)
			}
		}

		area := areaOf(sr.Labels())
		if _, seen := byArea[area]; !seen {
			areaOrder = append(areaOrder, area)
		}
		byArea[area] = append(byArea[area], line)
	}

	// ── Coverage: the second question the verdict must answer ─────────────
	total := passed + failed + skippedFor + deselected + pending
	ran := passed + failed       // checks that actually exercised the system
	selected := ran + skippedFor // checks this run was ASKED to exercise
	incomplete := deselected > 0 // some checks never ran at all
	narrowed := hasFilter(report.SuiteConfig)
	filtered := incomplete && narrowed // ...and it was on purpose
	cutShort := incomplete && !narrowed // ...or the run was stopped before the end
	nothingRan := ran == 0              // a filter that matched nothing must not read as PASS
	skipHeavy := total > 0 &&           // most of the suite abstained on its own
		float64(skippedFor+pending) > fullRunSkipTolerance*float64(total)

	// A "gate" is the ONLY state entitled to the words non-regression: the whole suite
	// was selected, and enough of it actually ran.
	isGate := !incomplete && !nothingRan && !skipHeavy && failed == 0 && !setupFailed

	// The verdict now carries BOTH axes. "PASS" alone is reserved for the run that
	// executed the whole suite; anything narrower says so in the same breath, because
	// the headline is the only line most readers read.
	var verdict string
	switch {
	case setupFailed:
		verdict = "❌ SETUP FAILED"
	case failed > 0 && incomplete:
		verdict = fmt.Sprintf("❌ FAIL — and this was a PARTIAL run (%d of %d checks)", selected, total)
	case failed > 0:
		verdict = "❌ FAIL"
	case nothingRan:
		verdict = "⚠️ NOTHING RAN — no check was executed, so nothing was proven"
	case cutShort:
		verdict = fmt.Sprintf("⚠️ INCOMPLETE — the run stopped after %d of %d checks", ran, total)
	case filtered:
		verdict = fmt.Sprintf("⚠️ PARTIAL PASS — %d of %d checks ran; NOT a non-regression gate", ran, total)
	case skipHeavy:
		verdict = fmt.Sprintf("⚠️ PARTIAL PASS — the whole suite was selected but only %d of %d checks ran", ran, total)
	default:
		verdict = "✅ PASS — full suite"
	}

	fmt.Fprintf(&b, "## Verdict: %s\n\n", verdict)

	// Coverage first, outcome second — in that order on purpose. "2 passed · 0 failed"
	// read as a verdict on the platform precisely because nothing next to it said how
	// much of the platform had been looked at.
	fmt.Fprintf(&b, "**Coverage: %d of %d checks ran (%s of the suite)**\n\n",
		ran, total, pct(ran, total))
	fmt.Fprintf(&b, "**Result: %d passed · %d failed", passed, failed)
	if skippedFor > 0 {
		fmt.Fprintf(&b, " · %d skipped with a stated reason", skippedFor)
	}
	if deselected > 0 {
		if narrowed {
			fmt.Fprintf(&b, " · %d never selected", deselected)
		} else {
			fmt.Fprintf(&b, " · %d never reached", deselected)
		}
	}
	if pending > 0 {
		fmt.Fprintf(&b, " · %d pending", pending)
	}
	fmt.Fprintf(&b, "**  —  ran in %s\n\n", report.RunTime.Round(time.Second))

	if filtered {
		fmt.Fprintf(&b, "> **This run was filtered.** %s selected %d of the suite's %d %s; "+
			"the other %d %s never executed. This report proves what it ran and makes no claim "+
			"about the rest — see \"Not exercised by this run\" below. "+
			"Only an unfiltered `mise run test` can be read as a non-regression gate.\n\n",
			filterDescription(report.SuiteConfig), selected, total,
			plural(total, "check", "checks"), deselected,
			plural(deselected, "check was", "checks were"))
	} else if cutShort {
		fmt.Fprintf(&b, "> **This run did not finish the suite.** %d of %d %s never ran — the suite "+
			"was interrupted, timed out, or stopped at the first failure. Whatever it says below is "+
			"about the %s it reached; the rest is unknown, not fine.\n\n",
			deselected, total, plural(deselected, "check", "checks"), plural(ran, "check", "checks"))
	} else if skipHeavy && failed == 0 && !setupFailed {
		fmt.Fprintf(&b, "> **The whole suite was selected, but %d of %d %s skipped %s.** "+
			"Nothing failed; most of the suite simply did not look. Read this as green for the "+
			"%s that ran, not for the suite — the stated skip reasons are on the lines below.\n\n",
			skippedFor+pending, total, plural(skippedFor+pending, "check", "checks"),
			plural(skippedFor+pending, "itself", "themselves"), plural(ran, "check", "checks"))
	}

	// ── Per-area detail ───────────────────────────────────────────────────
	areas := make([]string, 0, len(byArea))
	for a := range byArea {
		areas = append(areas, a)
	}
	slices.SortFunc(areas, func(a, b string) int { return cmp.Compare(areaRank(a), areaRank(b)) })

	stats := map[string]areaStats{}
	for _, area := range areas {
		st := areaStats{label: area}
		for _, c := range byArea[area] {
			st.total++
			switch c.sel {
			case selDeselected:
				st.deselected++
			case selRan:
				st.ran++
			}
		}
		stats[area] = st
	}

	for _, area := range areas {
		st := stats[area]
		// An area nothing selected gets no section of its own: eighty lines that all
		// read "skipped" is exactly the wall of noise the eye slides past. It is named
		// instead, once, in "Not exercised by this run".
		if st.ran == 0 && st.deselected == st.total {
			continue
		}

		heading, question := areaTitle(area)
		label := area
		if label == "" {
			label = "unlabeled"
		}
		fmt.Fprintf(&b, "## %s  `[%s]`\n\n", heading, label)
		fmt.Fprintf(&b, "_%s_\n\n", question)
		for _, c := range byArea[area] {
			if c.sel == selDeselected {
				continue // summarised at the end of the section
			}
			fmt.Fprintf(&b, "- %s  %s", mark(c), c.desc)
			switch {
			case c.state.Is(types.SpecStatePassed):
				fmt.Fprintf(&b, "  _(%s)_", c.dur.Round(100*time.Millisecond))
			case c.sel == selSkippedFor:
				if c.detail != "" {
					fmt.Fprintf(&b, "  — skipped: %s", c.detail)
				} else {
					b.WriteString("  — skipped (no reason given)")
				}
			case c.sel == selPending:
				b.WriteString("  — pending (marked as not-yet-written)")
			default:
				if c.detail != "" {
					fmt.Fprintf(&b, "  — %s", c.detail)
				}
			}
			b.WriteString("\n")
		}
		if st.deselected > 0 {
			fmt.Fprintf(&b, "- ➖  _%d further %s in this area did not run — untested here._\n",
				st.deselected, plural(st.deselected, "check", "checks"))
		}
		b.WriteString("\n")
	}

	// ── What this run did NOT cover ───────────────────────────────────────
	// The counterpart to the tally, and the reason the tally can no longer mislead:
	// whatever the run left out is named here, by area, in the same document.
	if deselected > 0 {
		b.WriteString("## Not exercised by this run\n\n")
		b.WriteString("_These checks were never executed. This report makes no claim, good or bad, " +
			"about anything they cover._\n\n")
		for _, area := range areas {
			st := stats[area]
			if st.deselected == 0 {
				continue
			}
			heading, _ := areaTitle(area)
			label := area
			if label == "" {
				label = "unlabeled"
			}
			switch {
			case st.deselected == st.total && st.total == 1:
				fmt.Fprintf(&b, "- **%s** `[%s]` — its single check was not run\n", heading, label)
			case st.deselected == st.total:
				fmt.Fprintf(&b, "- **%s** `[%s]` — all %d checks not run\n", heading, label, st.total)
			default:
				fmt.Fprintf(&b, "- **%s** `[%s]` — %d of %d %s not run\n",
					heading, label, st.deselected, st.total, plural(st.total, "check", "checks"))
			}
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
	// infraChecked matters as much as infraFailed. The old text opened a milestone
	// failure with "The platform is healthy but…" on the strength of zero infra
	// failures — which, on a `mise run test m4`, meant zero infra CHECKS. The same
	// absence-reads-as-health mistake, one paragraph further down the page.
	infraFailed, infraChecked := false, false
	for _, c := range byArea["infra"] {
		if c.sel == selRan {
			infraChecked = true
		}
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
		if infraChecked {
			b.WriteString("The platform is healthy but **a milestone acceptance check failed** — this " +
				"is a real regression or a genuine gap for that milestone. See the Failures section " +
				"above for the exact check and location.\n")
		} else {
			b.WriteString("**A milestone acceptance check failed** — see the Failures section above " +
				"for the exact check and location. Note that no `infra` check ran in this run, so the " +
				"platform's own health is unverified here: if the failure looks like storage or " +
				"networking, re-run with `mise run test infra` before hunting a product bug.\n")
		}
		// A red run is honest about what failed but silent about what it never looked at —
		// and "we ran m4, it went red, we fixed it, it went green" is exactly how a filtered
		// run ends up standing in for a gate.
		switch {
		case filtered:
			fmt.Fprintf(&b, "\nAnd note the scope: only %d of the suite's %d checks were selected "+
				"(%s). Fixing this failure and re-running the same filter will produce a PARTIAL "+
				"PASS, not a gate — the full suite still has to run before the fix can be called safe.\n",
				selected, total, filterDescription(report.SuiteConfig))
		case cutShort:
			fmt.Fprintf(&b, "\nAnd note the scope: the run stopped early, so %d of the suite's %d "+
				"checks never ran. There may be more wrong than the failures listed above.\n",
				deselected, total)
		}
	case nothingRan && narrowed:
		fmt.Fprintf(&b, "**Nothing was verified.** %s matched none of the suite's %d checks, so the "+
			"run ended without exercising anything. This is not a pass — it is an empty result, and "+
			"it is almost always a typo in the filter. Check the label (`infra`, `m0`, `m1`, …) and "+
			"re-run.\n", filterDescription(report.SuiteConfig), total)
	case nothingRan:
		fmt.Fprintf(&b, "**Nothing was verified.** Not one of the suite's %d checks executed, and no "+
			"filter explains it — the run was interrupted or died before it started working. This is "+
			"not a pass; it is an empty result. Check the output above `mise run status`.\n", total)
	case cutShort:
		fmt.Fprintf(&b, "**Nothing failed, but the run did not finish.** %d of %d checks ran and passed; "+
			"the other %d never executed because the suite was interrupted or timed out. A truncated "+
			"run is not a gate — the missing checks are unknown, not green. Re-run the suite to "+
			"completion before drawing a conclusion.\n", ran, total, deselected)
	case filtered:
		fmt.Fprintf(&b, "**Nothing failed — and this is not a non-regression gate.** The run was asked "+
			"for %d of the suite's %d checks (%s) and all of them passed on real infrastructure, so it "+
			"proves those %d and strictly nothing else. The %d %s never executed: their state is "+
			"unknown, not good. Use this to debug or to confirm one area; run `mise run test` with no "+
			"label before treating a run as a release gate.\n",
			selected, total, filterDescription(report.SuiteConfig), ran, deselected,
			plural(deselected, "check that was not selected", "checks that were not selected"))
	case skipHeavy:
		fmt.Fprintf(&b, "**Nothing failed, but the suite mostly abstained.** Every check was selected, "+
			"yet only %d of %d actually ran — the rest skipped themselves for the reasons printed above. "+
			"That is green for what it covered, not for the suite: before calling this a non-regression "+
			"gate, resolve the conditions those checks are waiting on (credentials, a released image, a "+
			"storage capability) so they run for real.\n", ran, total)
	default:
		b.WriteString("**Every check in the suite ran on real infrastructure and passed** — the cluster " +
			"is sound and the current milestone's acceptance criteria hold. This run is a valid green " +
			"non-regression gate.\n")
	}

	// The gate claim appears exactly once in this document, and only in the branch above
	// that earns it. Anything else states, in the same words each time, that it is a sample.
	if isGate && skippedFor > 0 {
		fmt.Fprintf(&b, "\n_%d %s skipped %s for a stated reason (see the lines above); the gate "+
			"covers the %d %s that ran._\n",
			skippedFor, plural(skippedFor, "check", "checks"),
			plural(skippedFor, "itself", "themselves"), ran, plural(ran, "check", "checks"))
	} else if skippedFor > 0 {
		b.WriteString("\n_A check that skips itself states its reason on its line above (waiting on a " +
			"released image, on credentials, on a storage capability). A conditional skip is not a " +
			"failure — but it is not a pass either: nothing was verified._\n")
	}

	return b.String()
}
