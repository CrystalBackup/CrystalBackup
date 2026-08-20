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
	"fmt"
	"slices"
	"strings"

	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
)

// This file is the human view: `crystal-backup selfcheck --format text`.
//
// # It renders the Report, and holds no facts of its own
//
// Every number, verdict and sentence below comes out of a *Report that Collect produced. Nothing here
// reads a cluster, recomputes a threshold or decides that something is fine. That is not tidiness: a
// text mode that reached its own conclusions would be a SECOND source of truth about the same
// installation, and the first time the two disagreed — the JSON saying degraded, the terminal saying
// healthy — neither would be believable again. TestTextAndJSONAgreeOnTheVerdict pins it.
//
// # Compactness is a requirement, not a preference
//
// The audience is somebody who has just run `helm install` and wants one screen. A real cluster has
// thousands of PVCs, and a command that answers "is everything OK?" with four thousand lines has not
// answered it. So the census is rendered summary-first — counts per class, then detail rows ONLY for
// the classes that need somebody's attention — and --all is what a reader types when they want the
// rest. TestTextStaysCompact asserts an upper bound on the line count for a synthetic cluster with
// hundreds of volumes, and asserts that --all lifts it, because a stated requirement with no test is
// a preference.
//
// # Real names here, tokens in the JSON, and never silently
//
// This output is somebody's own terminal, showing them their own cluster: hashed PVC names would make
// it useless, so text mode prints identifiers verbatim regardless of the redaction the report was
// collected under. That asymmetry with the JSON default is stated on its own line, every time, in
// textIdentifierNotice. An operator who pastes this into an issue must have been told, in the output
// itself, that it is not the redacted form — the one thing this package cannot afford is a reader who
// believed a document was anonymous.

// TextOptions configure the text rendering.
type TextOptions struct {
	// All lists every PVC in the census rather than only the ones needing attention. It lifts the
	// compactness rule deliberately and on request; nothing else does.
	All bool
	// Width is the terminal width to wrap prose at. Zero means textDefaultWidth.
	Width int
}

// minWrapWidth is the floor the wrapper will not go below, however narrow a prefix eats into the
// width. A limit of a few characters would put one word per line, which is not a rendering of prose so
// much as a column of it.
const minWrapWidth = 20

// textDefaultWidth is the wrap width for the prose blocks. 96 rather than 80: this output is read in
// a terminal that is nearly always wider than a punched card, and the per-PVC rows want the room.
const textDefaultWidth = 96

// textIdentifierNotice is the disclosure that makes the real-names decision non-silent. It names the
// exact flag that produces a shareable copy, because "use the JSON" is advice a reader has to act on
// at three in the morning.
const textIdentifierNotice = "Namespace, PVC, location and bucket names below are REAL — this is " +
	"your own terminal. For a copy you can attach to an issue, run the same command with " +
	"--format json: that redacts every identifier by default."

// RenderText renders a report as the compact human summary.
//
// It takes the same *Report the JSON encoder and the HTML renderer take, and it is a pure function of
// it: no cluster, no clock, no client. See this file's header for why that is the whole design.
func RenderText(rep *Report, opts TextOptions) []byte {
	w := &textWriter{width: opts.Width}
	if w.width <= 0 {
		w.width = textDefaultWidth
	}

	textHeader(w, rep)
	textVerdict(w, rep)
	textPlan(w, rep.Plan)
	textCoverage(w, rep.Coverage, opts.All)
	textFindings(w, rep)
	textDiagnostics(w, rep.Diagnostics)
	return w.bytes()
}

func textHeader(w *textWriter, rep *Report) {
	w.line("CrystalBackup self-check — " + formatTime(rep.GeneratedAt))
	parts := []string{"operator " + orDash(rep.Operator.BuildVersion)}
	if rep.Operator.ChartVersion != "" {
		parts = append(parts, "chart "+rep.Operator.ChartVersion)
	}
	parts = append(parts, "namespace "+rep.Operator.Namespace)
	if rep.Cluster.KubernetesVersion != "" {
		parts = append(parts, "Kubernetes "+rep.Cluster.KubernetesVersion)
	}
	if rep.Cluster.SnapshotAPI != "" {
		parts = append(parts, "snapshot API "+rep.Cluster.SnapshotAPI)
	}
	w.line(strings.Join(parts, " · "))
	if rep.Operator.VersionNote != "" {
		w.line("! " + rep.Operator.VersionNote)
	}
	w.blank()
	w.wrap("", textIdentifierNotice)
}

func textVerdict(w *textWriter, rep *Report) {
	w.section("VERDICT: " + textVerdictWord(rep.Verdict.Status))
	w.wrap("  ", rep.Verdict.Summary)
}

// textVerdictWord upper-cases the verdict for the headline and spells healthyWithFindings out, which
// is the one status whose camel-case name actively misleads at a glance — it is the status that exists
// because "healthy" was being read as a verdict on the whole installation rather than on the rules.
func textVerdictWord(status string) string {
	switch status {
	case "healthyWithFindings":
		return "HEALTHY, WITH FINDINGS"
	case "":
		return "NOT DETERMINED"
	default:
		return strings.ToUpper(status)
	}
}

func textPlan(w *textWriter, p *Plan) {
	w.section("WHAT THE CRs WILL DO")
	if p == nil {
		w.line("  (not collected: this report was produced by an operator too old to describe them)")
		return
	}
	if p.Note != "" {
		w.wrap("  ! ", p.Note)
	}
	for _, l := range p.Locations {
		w.line("  " + textLocationHeading(l))
		for _, s := range l.Sentences {
			w.wrap("      ", s)
		}
		for _, m := range l.Maintenance {
			w.wrap("      ", textMaintenanceLine(m))
		}
		for _, prob := range l.Problems {
			w.wrap("      ! ", prob)
		}
	}
	for _, s := range p.Schedules {
		w.line("  " + textScheduleHeading(s))
		for _, line := range s.Sentences {
			w.wrap("      ", line)
		}
		if s.Next != nil && !s.Suspended {
			w.line("      Next run " + formatTimePtr(s.Next) + ".")
		}
		for _, prob := range s.Problems {
			w.wrap("      ! ", prob)
		}
	}
}

func textLocationHeading(l PlannedLocation) string {
	kind := "ClusterBackupLocation"
	name := l.Name
	if l.Scope != apiconst.OriginCluster {
		kind = "BackupLocation"
		name = l.Namespace + "/" + l.Name
	}
	var tags []string
	if l.Default {
		tags = append(tags, "default")
	}
	if l.Mode != "" {
		tags = append(tags, l.Mode)
	}
	if l.Ready != "" {
		tags = append(tags, "Ready="+l.Ready)
	}
	return kind + " " + name + tagSuffix(tags)
}

func textScheduleHeading(s PlannedSchedule) string {
	kind := "ClusterBackupSchedule"
	name := s.Name
	if s.Origin != apiconst.OriginCluster {
		kind = "BackupSchedule"
		name = s.Namespace + "/" + s.Name
	}
	var tags []string
	if s.Suspended {
		tags = append(tags, "SUSPENDED")
	}
	if s.Ready != "" {
		tags = append(tags, "Ready="+s.Ready)
	}
	return kind + " " + name + tagSuffix(tags)
}

func tagSuffix(tags []string) string {
	if len(tags) == 0 {
		return ""
	}
	return "  [" + strings.Join(tags, ", ") + "]"
}

// textMaintenanceLine renders one maintenance window. The prune line says out loud that it is
// exclusive, because that is the whole reason somebody looks it up: nothing in the cluster can start a
// backup while it runs.
func textMaintenanceLine(m PlannedMaintenance) string {
	head := "restic " + m.Operation
	if m.Exclusive {
		head += " (CLUSTER-WIDE EXCLUSIVE WINDOW: no backup can start while it runs)"
	}
	when := m.InWords
	if when == "" {
		when = "on cron `" + m.Cron + "`"
	}
	line := head + " " + when + "."
	if m.Next != nil {
		line += " Next " + formatTimePtr(m.Next) + "."
	}
	if m.Detail != "" {
		line += " " + upperFirst(m.Detail) + "."
	}
	return line
}

func textCoverage(w *textWriter, cov *Coverage, all bool) {
	w.section("WHAT WILL AND WILL NOT BE BACKED UP")
	if cov == nil {
		w.wrap("  ", "(not collected: either this report predates the per-PVC census or the PVC list "+
			"could not be read — see DIAGNOSTICS)")
		return
	}
	w.line(fmt.Sprintf("  %s across %s, %s requested",
		plural(cov.PVCs, "PVC"), plural(cov.Namespaces, "namespace"), humanBytes(cov.CapacityBytes)))
	w.blank()
	for _, t := range cov.Classes {
		w.line(fmt.Sprintf("  %6d  %-7s %-23s %s",
			t.Count, textVerdictTag(t.Verdict), t.Class, textClassGist(t.Class)))
	}

	// The selection counts, and the one sentence that has to come BEFORE them when they are not
	// measurements. "Nothing in this cluster will ever back these up" is the strongest claim this
	// section makes; made on a schedule list that could not be read, it is a claim about the reader's
	// permissions in the voice of a claim about their data. Same rule as the ExposerUndetermined
	// class, applied to the numbers that are not a class.
	if cov.SelectionUndetermined {
		w.blank()
		w.wrap("  ! ", "WHICH SCHEDULES SELECT WHICH VOLUMES COULD NOT BE DETERMINED: a schedule or "+
			"namespace list this command needed was refused or failed (see DIAGNOSTICS). The counts "+
			"below are what could be seen from here and are NOT a finding — a volume reported as "+
			"selected by nothing may well be selected by a schedule in the list that was refused.")
	}
	if cov.Unselected > 0 {
		w.blank()
		if cov.SelectionUndetermined {
			w.wrap("  ! ", fmt.Sprintf(
				"%d of those PVCs appeared to be selected by no schedule — unconfirmed, see above.",
				cov.Unselected))
		} else {
			w.wrap("  ! ", fmt.Sprintf(
				"%d of those PVCs %s selected by NO schedule: whatever their storage can do, nothing in "+
					"this cluster will ever back %s up.",
				cov.Unselected, isAre(cov.Unselected), itThem(cov.Unselected)))
		}
	}
	if cov.InertOnly > 0 && !cov.SelectionUndetermined {
		w.wrap("  ! ", fmt.Sprintf(
			"%d %s selected only by a schedule that cannot fire (suspended, or an unparseable cron), "+
				"so %s covered on paper and unprotected in fact.",
			cov.InertOnly, isAre(cov.InertOnly), itTheyAre(cov.InertOnly)))
	}
	// The observation, immediately under the prediction it qualifies rather than in a section of its
	// own. The whole defect this closes was these two facts living far enough apart that nobody joined
	// them, and the tally above is exactly where a reader forms the belief this line has to interrupt.
	if cov.StalledStorage > 0 {
		w.wrap("  ! ", fmt.Sprintf(
			"%s counted as BACKED UP above %s on a StorageClass this cluster is OBSERVED not to be "+
				"finishing snapshots on — bound to a VolumeSnapshotContent and never ready. The treatment "+
				"is still what the operator will attempt; the rows below say what was seen.",
			plural(cov.StalledStorage, "PVC"), isAre(cov.StalledStorage)))
	}

	hidden := textCoverageRows(w, cov, all)
	if hidden > 0 && !all {
		w.line(fmt.Sprintf("  (%d further PVCs need nothing; --all lists every PVC)", hidden))
	}
	if cov.ItemsOmitted > 0 {
		w.wrap("  ", fmt.Sprintf(
			"%d PVCs are counted in the totals above but carry no row in this report: the document "+
				"caps its per-PVC rows at %d, keeping the ones needing attention.",
			cov.ItemsOmitted, maxCoverageItems))
	}
	w.blank()
	w.wrap("  ", cov.Note)
	w.line(fmt.Sprintf("  (the census cost %d API requests, independent of the number of PVCs)", cov.APIReads))
}

// textCoverageRows emits the per-PVC rows and returns how many were withheld.
//
// The default selection is the whole compactness rule in one place: a row is shown when somebody has
// to do something about it — its class is not a clean success, or nothing that can fire selects it.
// Everything else is a number in the tally above.
//
// A row's Detail goes on its OWN wrapped continuation line rather than on the end of the row. The
// resolver's sentences are long (they name the driver, the object and where the answer came from) and
// letting one run off the right edge would cost the reader the columns of every row below it — which
// on a terminal is the difference between a table and a wall.
func textCoverageRows(w *textWriter, cov *Coverage, all bool) (hidden int) {
	var shown int
	for _, it := range cov.Items {
		if !all && !textNeedsAttention(it) {
			hidden++
			continue
		}
		if shown == 0 {
			w.blank()
			if all {
				w.line("  Every PVC:")
			} else {
				w.line("  PVCs needing attention:")
			}
		}
		shown++
		w.line(fmt.Sprintf("    %-34s %-14s %s",
			truncate(it.Namespace+"/"+it.Name, 34), truncate(orDash(it.StorageClass), 14),
			textRowVerdict(it)))
		if len(it.InertSchedules) > 0 && !it.Selected {
			w.wrap("        ", "the only schedule(s) selecting it cannot fire: "+
				strings.Join(it.InertSchedules, ", "))
		}
		w.wrap("        ", it.Detail)
		// Marked with the same "!" the unselected and inert findings carry, because it is the same kind
		// of thing: a row whose columns all read as fine, next to a reason to doubt them.
		w.wrap("        ! ", it.SnapshotEvidence)
	}
	return hidden
}

// textNeedsAttention mirrors coverageNeedsAttention — see its doc for why the snapshot observation is
// one of the axes. The two are separate functions because one decides an ORDER over the whole census
// and the other decides what a compact terminal prints, but they must agree on which rows matter: a row
// the sort called healthy and the renderer called interesting would be a row nobody ever sees, because
// the sort put it past the cap first.
func textNeedsAttention(it CoveredPVC) bool {
	return it.Verdict != CoverageVerdictBackedUp || !it.Selected || it.SnapshotEvidence != ""
}

// textRowVerdict is the per-row verdict, and the unselected case leads.
//
// A PVC that snapshots perfectly and that no schedule selects is NOT BACKED UP, and the row has to say
// so where the eye lands. Showing "csi-generic" there and leaving "selected by nobody" to be inferred
// is how this finding stayed invisible in the first place.
//
// Both facts are shown, though, and never one instead of the other. The two axes are independent —
// a volume can be unselected AND unsnapshottable — and an administrator who fixes the selector on a
// volume whose storage cannot snapshot anyway has done work for nothing. "would be" is the wording for
// the treatment of an unselected volume, because nothing is going to do it.
func textRowVerdict(it CoveredPVC) string {
	treatment := textVerdictTag(it.Verdict) + " " + it.Class
	if it.Verdict == CoverageVerdictBackedUp {
		treatment = "would be " + it.Class
	}
	if it.Selected {
		return treatment
	}
	return "NOT SELECTED · " + treatment
}

func textVerdictTag(verdict string) string {
	switch verdict {
	case CoverageVerdictBackedUp:
		return "ok"
	case CoverageVerdictSkipped:
		return "SKIPPED"
	case CoverageVerdictFailed:
		return "FAILED"
	default:
		return "UNKNOWN"
	}
}

// textClassGist is the tally line's short gloss. It is deliberately shorter than
// CoverageTally.Summary: the tally is a table and the full sentence is in the JSON, so a reader who
// wants the whole story has somewhere to go.
func textClassGist(class string) string {
	switch class {
	case CoverageClone:
		return "snapshot, then a temporary volume provisioned from it"
	case CoverageDirect:
		return "snapshot mounted directly, read-only — no second copy"
	case CoverageUnsupported:
		return "this storage cannot snapshot: skipped on every run"
	case CoveragePrecheckFailed:
		return "the snapshot class cannot be served today — fixable"
	case CoverageUnresolved:
		return "the exposer could not be resolved; retried to a deadline"
	case CoverageUndetermined:
		return "THIS REPORT could not read what it needed — not a finding about the data"
	case CoverageSnapshotAPIAbsent:
		return "the VolumeSnapshot API could not be read at all"
	default:
		return "resolved to an exposer this build cannot describe"
	}
}

// textFindings lists the rule verdicts that are not a pass, one line each, worst first.
//
// Not-evaluated and errored rules are listed beside the breaches rather than summarised away, which
// is this package's oldest rule: an installation where seven of ten rules could not be evaluated has
// not been given a clean bill of health, and the reader who sees "healthy" at the top has to meet the
// number that qualifies it.
func textFindings(w *textWriter, rep *Report) {
	var lines []string
	for _, r := range rep.Rules {
		if r.Status == StatusOK {
			continue
		}
		line := fmt.Sprintf("  %-13s %-8s %s", statusWord(r.Status), r.Severity, r.Name)
		if r.Summary != "" {
			line += " — " + r.Summary
		}
		if r.Reason != "" {
			line += " (" + r.Reason + ")"
		}
		lines = append(lines, line)
	}
	// Sorted by the rendered status word so breaches group together and the order is stable between
	// two runs over unchanged state — the same property the HTML renderer's sorts give.
	slices.Sort(lines)
	if len(lines) == 0 && rep.Leaks.Totals.Residual == 0 && rep.StuckSnapshots.Stuck == 0 {
		return
	}
	w.section("FINDINGS")
	for _, l := range lines {
		w.line(l)
	}
	// The snapshot observation is a finding for the same reason the leak residual is: no alert rule
	// fires on it, so this section is the only place in the terminal output where a reader who is
	// skimming meets it at all.
	if rep.StuckSnapshots.Stuck > 0 {
		w.wrap("  ", fmt.Sprintf(
			"%d VolumeSnapshot(s) in this cluster are bound to a VolumeSnapshotContent and still not "+
				"readyToUse after %d minutes (oldest %d h) — the fingerprint of a snapshotter that is not "+
				"advancing. Nothing here diagnoses it and no verdict above changes because of it. No alert "+
				"rule fires on this; see stuckSnapshots.",
			rep.StuckSnapshots.Stuck, rep.StuckSnapshots.GraceMinutes,
			rep.StuckSnapshots.OldestStuckHours))
		for _, s := range rep.StuckSnapshots.Samples {
			w.wrap("      ", fmt.Sprintf("%s/%s on %s, %d h, source PVC %s, content %s%s",
				s.Namespace, s.Name, orDash(s.Class), s.AgeHours, orDash(s.SourcePVC),
				orDash(s.Content), errSuffix(s.Error)))
		}
	}
	if rep.Leaks.Totals.Residual > 0 {
		w.wrap("  ", fmt.Sprintf(
			"%d objects the operator manages are older than the orphan reaper's %d-minute grace "+
				"period and should not still exist. No alert rule fires on this; see leakIndicators.",
			rep.Leaks.Totals.Residual, rep.Leaks.GraceMinutes))
	}
}

// textDiagnostics is last and is never omitted when non-empty. A section this report left empty
// because it was not allowed to look must not read the same as one that is empty because there is
// nothing there, and in a compact rendering the diagnostics block is the only thing preserving that
// difference.
func textDiagnostics(w *textWriter, diags []Diagnostic) {
	if len(diags) == 0 {
		return
	}
	w.section("WHAT COULD NOT BE READ")
	for _, d := range diags {
		w.wrap("  ", d.Area+": "+d.Message)
		if d.Impact != "" {
			w.wrap("      ", "→ "+d.Impact)
		}
	}
}

// textWriter accumulates the output and owns the wrapping, so no caller has to think about width.
type textWriter struct {
	b     strings.Builder
	width int
	// blanks tracks whether the last emitted line was blank, so consecutive blank() calls from
	// sections that may or may not have produced anything collapse into one. Without it every
	// conditional block in this file would need to know whether the previous one printed.
	lastBlank bool
}

func (w *textWriter) line(s string) {
	w.b.WriteString(strings.TrimRight(s, " "))
	w.b.WriteByte('\n')
	w.lastBlank = strings.TrimSpace(s) == ""
}

func (w *textWriter) blank() {
	if w.lastBlank {
		return
	}
	w.line("")
}

func (w *textWriter) section(title string) {
	w.blank()
	w.line(title)
}

// wrap emits prose at the writer's width, indenting continuation lines to line up under the first
// one's text rather than under its bullet. An empty string emits nothing, which is what lets the
// sentence builders return "" for a fact that does not apply instead of every caller testing for it.
func (w *textWriter) wrap(prefix, s string) {
	s = strings.TrimSpace(s)
	if s == "" {
		return
	}
	cont := strings.Repeat(" ", len(prefix))
	limit := max(w.width-len(prefix), minWrapWidth)
	line := prefix
	col := 0
	for word := range strings.FieldsSeq(s) {
		switch {
		case col == 0:
			line += word
			col = len(word)
		case col+1+len(word) <= limit:
			line += " " + word
			col += 1 + len(word)
		default:
			w.line(line)
			line = cont + word
			col = len(word)
		}
	}
	if col > 0 {
		w.line(line)
	}
}

func (w *textWriter) bytes() []byte { return []byte(w.b.String()) }

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// errSuffix appends a stuck snapshot's own error message when it has one, and nothing at all when it
// does not. The distinction is worth the helper: a stalled snapshotter records NO error — that silence
// is why an age-based observation was needed — so an empty parenthesis on every sample line would be a
// column of noise around the one case that matters.
func errSuffix(msg string) string {
	if msg == "" {
		return ""
	}
	return " — " + msg
}

// truncate keeps a column a column. It cuts from the LEFT for over-long identifiers, keeping an
// ellipsis at the front, because the distinguishing part of `acme-billing-prod/postgres-data-2` is at
// the end and a right-truncation would render a screen of identical rows.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	if max <= 1 {
		return s[:max]
	}
	return "…" + s[len(s)-(max-1):]
}

// isAre / itThem / itTheyAre keep the generated sentences grammatical for a count of one, which is by
// far the commonest count in a report anybody is reading. "1 PVCs are selected by NO schedule" is the
// sort of line that makes a reader trust the rest of the document a little less.
func isAre(n int) string {
	if n == 1 {
		return "is"
	}
	return "are"
}

func itThem(n int) string {
	if n == 1 {
		return "it"
	}
	return "them"
}

func itTheyAre(n int) string {
	if n == 1 {
		return "it is"
	}
	return "they are"
}

func upperFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
