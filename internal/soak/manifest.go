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

package soak

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/CrystalBackup/CrystalBackup/internal/selfcheck"
)

// ---------------------------------------------------------------------------------------------
// §8 — the anti-vacuity contract.
//
// hack/soak/collect.sh derives its ENTIRE verdict from this file. Every field below is read by it
// by name; the shapes are a contract, not a suggestion, and a deviation does not fail loudly —
// it makes collect.sh misgrade a real soak, quietly, in the direction of flattery.
//
// What the file is FOR is the same thing this whole kit is for: making "nothing was collected"
// impossible to mistake for "everything was fine". Hence the four rules that are not negotiable:
// a stream never enabled is DISABLED and never EMPTY; a stream that could not be measured is
// NOT_MEASURED with a reason and never 0; EMPTY means "read successfully and there was nothing",
// and each stream declares whether that is a healthy answer for it; and observedDays is computed
// from the DATA, because the gap between what was asked for and what is there is the single most
// important number in the file and deriving it from the flags would guarantee it read zero.
// ---------------------------------------------------------------------------------------------

// ManifestSchema is the `schema` field. Versioned so a future collector can change the shape
// without a reader silently misinterpreting the old one.
const ManifestSchema = "crystalbackup.soak/v1"

// The stream statuses collect.sh switches on. Any value outside this set makes it record the
// stream as FAILED with "unrecognised status", which is the correct behaviour and not one to
// discover in production.
const (
	statusOK          = "OK"
	statusEmpty       = "EMPTY"
	statusDegraded    = "DEGRADED"
	statusNotMeasured = "NOT_MEASURED"
	statusDisabled    = "DISABLED"
)

// Coverage is the gap between what was asked for and what is there.
type Coverage struct {
	RequestedDays float64 `json:"requestedDays"`
	ObservedDays  float64 `json:"observedDays"`
}

// StreamReport is one row of the manifest and one line of collect.sh's report.
type StreamReport struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Items  int    `json:"items"`
	// EmptyIsHealthy is the per-stream answer to "is nothing a possible right answer here?".
	// True for events and error logs — a fortnight with no Warning is a good fortnight — and
	// false for metrics and self-checks, where nothing means the collector was not working.
	// collect.sh reads it to decide whether EMPTY is an OK or an INCOMPLETE.
	EmptyIsHealthy bool      `json:"emptyIsHealthy"`
	FirstSample    string    `json:"firstSample,omitempty"`
	LastSample     string    `json:"lastSample,omitempty"`
	Coverage       *Coverage `json:"coverage,omitempty"`
	Drops          []Drop    `json:"drops"`
	Note           string    `json:"note,omitempty"`
}

// Manifest is MANIFEST.json.
type Manifest struct {
	Schema string `json:"schema"`
	// OperatorVersion is the build that was running when this archive was EXPORTED.
	//
	// It is NOT "the build that produced this archive", which is how it was documented and how it
	// was read. collector-id.json is rewritten in full on every collector start, so this field
	// carries whichever build happened to start LAST: a fortnight that ran six days of 0.6.2 and
	// eight of 0.6.3 came out as one flat version, while §5's mover table — built from a
	// highwater file that PERSISTS across restarts — silently mixed the two systems.
	//
	// Kept, and kept in this position, because collect.sh's field-by-field read of this file is a
	// contract and because one string is what a reader wants when there is only one build. The
	// question it cannot answer is answered by operatorVersions below.
	OperatorVersion string `json:"operatorVersion"`
	// OperatorVersions is every build that collected any part of this archive, in order. One entry
	// is the ordinary case and says nothing the field above does not; more than one is a FINDING,
	// because every figure derived across the whole span then describes more than one system.
	OperatorVersions []VersionSpan `json:"operatorVersions"`
	// OperatorVersionNote is that finding in the exact words COLLECTION-REPORT.txt, its §5 section
	// and the --status screen use, and it is a stored field rather than prose each reader builds
	// for itself for the reason unredactedNote is: the machine-readable half of this archive and
	// the human-readable half must not be able to say different things about the same fact.
	//
	// Empty when there is nothing to say, which is most archives.
	OperatorVersionNote string              `json:"operatorVersionNote,omitempty"`
	CollectorStartedAt  string              `json:"collectorStartedAt"`
	ExportedAt          string              `json:"exportedAt"`
	Mode                string              `json:"mode"`
	Redaction           selfcheck.Redaction `json:"redaction"`
	// UnredactedNote is §6's residual, in the words collect.sh prints back to the admin before
	// they send the archive. It is not a disclaimer: it is the reason step 4 of that script
	// exists, and the two directories it names are the only ones an admin genuinely has to read.
	UnredactedNote string `json:"unredactedNote"`
	// CollectorUptimeFraction is §9. collect.sh turns anything below 0.9 into a THIN verdict.
	CollectorUptimeFraction float64        `json:"collectorUptimeFraction"`
	Streams                 []StreamReport `json:"streams"`
}

// unredactedResidual is the sentence §6 requires and collect.sh reproduces. It names what could
// NOT be tokenised, because the honest boundary of this redaction is "every identifier the
// collector enumerated over fourteen days" and there are things nobody enumerates.
const unredactedResidual = "logs/ and events/ carry FREE TEXT. Every namespace, PVC, location, " +
	"bucket, schedule, sync, node and pod name the collector saw over the whole soak has been " +
	"substituted out of it, which is a far better registry than anything reconstructed " +
	"afterwards — but it cannot catch a name nobody enumerated: a path inside a volume, a restic " +
	"snapshot ID, a hostname inside a library's error string, an object created after the last " +
	"listing. Read those two directories before you send this."

// unredactedResidualFull is the same field when --full was used, because a manifest that said
// "only free text is unredacted" over an archive where nothing is redacted would be worse than
// saying nothing.
const unredactedResidualFull = "NOTHING in this archive is redacted. --full was requested: every " +
	"namespace, PVC, location, bucket, endpoint, node, pod name and log message is VERBATIM. " +
	"Read all of it before you send it to anyone."

// emptyIsHealthy is §8's third rule, as a table.
//
// It is a table and not a per-call argument so that the answer for a stream is written down once,
// in the place a reviewer can see all five at a time, rather than at each of the sites that
// happens to build a StreamReport.
var emptyIsHealthy = map[string]bool{
	// A fortnight with no Warning event and no operator error line is a GOOD fortnight, and
	// reporting it as an incomplete collection would train the reader to ignore the field.
	StreamEvents: true,
	StreamLogs:   true,
	// The rest cannot honestly be empty. No metrics means the scrape never worked; no self-check
	// means the daily job never ran; no high-water marks means no mover ever ran or nothing was
	// sampling, and both of those are findings rather than silence.
	StreamMetrics:   false,
	StreamState:     false,
	StreamSelfcheck: false,
	StreamHighwater: false,
	StreamAlerts:    false,
}

// newStreamReport applies §8's rules in one place, so no caller can construct a row that breaks
// them.
//
// The signature forces the decision: a caller must say whether the stream was ENABLED at all, and
// whether it was MEASURABLE, before it gets to talk about how many items it has. That ordering is
// the rule — never enabled is DISABLED, could not be measured is NOT_MEASURED, and only what is
// left over is allowed to be EMPTY.
func newStreamReport(name string, items int, note string) StreamReport {
	r := StreamReport{
		Name:           name,
		Items:          items,
		EmptyIsHealthy: emptyIsHealthy[name],
		Drops:          []Drop{},
		Note:           note,
	}
	if items > 0 {
		r.Status = statusOK
	} else {
		r.Status = statusEmpty
	}
	return r
}

func disabledStream(name, reason string) StreamReport {
	return StreamReport{
		Name: name, Status: statusDisabled, Items: 0,
		EmptyIsHealthy: emptyIsHealthy[name], Drops: []Drop{}, Note: reason,
	}
}

func notMeasuredStream(name, reason string) StreamReport {
	return StreamReport{
		Name: name, Status: statusNotMeasured, Items: 0,
		EmptyIsHealthy: emptyIsHealthy[name], Drops: []Drop{}, Note: reason,
	}
}

// withDrops attaches what the footprint cap took, and says so in the note as well as in the
// field. §4 is explicit that a deletion is "not a log line: it is a field in the manifest that
// collect.sh reads and reports", and the note is what a human reading MANIFEST.json by hand sees.
func (r StreamReport) withDrops(drops []Drop) StreamReport {
	if len(drops) == 0 {
		return r
	}
	r.Drops = drops
	days := make([]string, 0, len(drops))
	for _, d := range drops {
		if t, err := time.Parse(time.RFC3339, d.From); err == nil {
			days = append(days, t.Format(dayLayout))
		}
	}
	slices.Sort(days)
	r.Note = strings.TrimSpace(fmt.Sprintf(
		"%s The footprint cap deleted %d segment(s) of this stream — %s — to keep the collector from "+
			"filling the volume it runs on. That data is gone and nothing in this archive reconstructs it.",
		r.Note, len(drops), strings.Join(days, ", ")))
	return r
}

// withCoverage attaches §8's most important number.
func (r StreamReport) withCoverage(requested, observed float64) StreamReport {
	r.Coverage = &Coverage{RequestedDays: round2(requested), ObservedDays: round2(observed)}
	return r
}

func (r StreamReport) withSamples(first, last time.Time) StreamReport {
	if !first.IsZero() {
		r.FirstSample = first.UTC().Format(time.RFC3339)
	}
	if !last.IsZero() {
		r.LastSample = last.UTC().Format(time.RFC3339)
	}
	return r
}

// degrade marks a stream that was collected but not fully. It never upgrades a status: a stream
// that is EMPTY stays EMPTY, because "degraded" over nothing would read as "some data, imperfect"
// when there is none.
func (r StreamReport) degrade(reason string) StreamReport {
	if r.Status != statusOK {
		return r
	}
	r.Status = statusDegraded
	r.Note = strings.TrimSpace(r.Note + " " + reason)
	return r
}

func round2(f float64) float64 {
	return float64(int64(f*100+0.5)) / 100
}

// ---------------------------------------------------------------------------------------------
// which build produced this
// ---------------------------------------------------------------------------------------------

// versionNote is the ONE wording for "this archive does not describe a single system".
//
// Four places have to say it — MANIFEST.json, COLLECTION-REPORT.txt's header, the §5 mover section
// and the --status screen — and it is a function rather than four strings for the same reason
// emptyIsHealthy is one table: a reader who meets the same fact stated three ways in one archive
// has to work out whether they are looking at three facts.
//
// It returns "" when there is nothing to say, and that is load-bearing rather than tidy. An archive
// from a soak that ran one build must read EXACTLY as it did before this existed; a block that
// appeared on every archive to announce that nothing unusual happened is a block nobody reads on
// the day something did.
//
// `scraped` is the version label of every crystalbackup_build_info point in the archive: the
// MEASURED operator's account of itself, where the spans are the COLLECTOR binary's. They differ
// exactly when somebody changed the operator image without changing the collector's, and what
// follows states that disagreement rather than resolving it.
func versionNote(spans []VersionSpan, scraped []string) string {
	var parts []string
	if len(spans) > 1 {
		parts = append(parts, fmt.Sprintf(
			"MORE THAN ONE BUILD collected this archive — %s. Every figure derived across the whole "+
				"span — the mover high-water table above all — therefore describes more than one "+
				"system: highwater/marks.json PERSISTS across collector restarts, so a single peak in "+
				"it can have been set under a build that is not the one this archive names. No figure "+
				"here has been re-attributed to a version it cannot prove.", versionListing(spans)))
	}
	if unclaimed := unclaimedVersions(spans, scraped); len(unclaimed) > 0 {
		parts = append(parts, fmt.Sprintf(
			"The operator reported version %s in crystalbackup_build_info and no collector session "+
				"claims it. The session log records the COLLECTOR binary's build and build_info is the "+
				"MEASURED operator's; the two come apart exactly when the operator image was changed "+
				"without the collector's. Neither is corrected against the other here — the "+
				"disagreement IS the finding.", strings.Join(unclaimed, ", ")))
	}
	return strings.Join(parts, " ")
}

// versionListing names the builds with the time each was observed for, which is the pair that
// makes a mixed archive readable: "0.6.2 and 0.6.3" says a system changed, "0.6.2 (0.4 day(s)),
// 0.6.3 (13.6 day(s))" says which one the fortnight is mostly about.
func versionListing(spans []VersionSpan) string {
	parts := make([]string, 0, len(spans))
	for _, sp := range spans {
		parts = append(parts, fmt.Sprintf("%s (%.1f day(s))",
			versionWord(sp.Version), sp.ObservedSeconds/86400))
	}
	return strings.Join(parts, ", ")
}

// versionWord renders one build for a human. An unrecorded version is `unknown` and never blank:
// sessions written before the field existed are still time that was collected, and a blank in a
// list of versions reads as a formatting fault rather than as an absence.
func versionWord(v string) string {
	if v == "" {
		return "unknown"
	}
	return v
}

// unclaimedVersions is every scraped build_info version that no session accounts for.
//
// ONE direction, deliberately. A span whose version never appears in build_info is the ordinary
// case — the series may not have been scraped at all, and §3b only promises it survives the cap —
// but a version the OPERATOR reported over a window the collector was up for cannot be explained
// by anything the collector did.
func unclaimedVersions(spans []VersionSpan, scraped []string) []string {
	if len(scraped) == 0 {
		return nil
	}
	claimed := make(map[string]bool, len(spans))
	for _, sp := range spans {
		claimed[sp.Version] = true
	}
	out := []string{}
	for _, v := range scraped {
		if v == "" || claimed[v] {
			continue
		}
		out = append(out, v)
	}
	slices.Sort(out)
	return slices.Compact(out)
}
