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
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// textCompactBound is the line budget for a healthy installation of any size.
//
// It is a number in a test rather than a sentence in a doc because compactness is a stated
// REQUIREMENT of this command: the reader is somebody who has just run `helm install` and wants one
// screen, and a command that answers "is everything OK?" in four thousand lines has not answered it.
// 60 is a generous screen; the point of the bound is that it does not move with the number of PVCs.
const textCompactBound = 60

// TestTextStaysCompact drives the compactness requirement from both sides: the default must fit a
// screen for a cluster with hundreds of volumes, and --all must actually lift the limit.
//
// Both halves are needed. A renderer that printed nothing would pass the first assertion and be
// useless; one that printed everything would pass the second and defeat the requirement.
func TestTextStaysCompact(t *testing.T) {
	rep := collectSynthetic(t, 600)

	compact := textLines(RenderText(rep, TextOptions{}))
	if len(compact) > textCompactBound {
		t.Errorf("the default rendering of a 600-PVC cluster is %d lines, over the %d-line budget; "+
			"summary-first is a requirement, not a preference", len(compact), textCompactBound)
	}
	// And it must actually have reported the volumes rather than passed by omitting the section.
	joined := strings.Join(compact, "\n")
	if !strings.Contains(joined, "600 PVCs") {
		t.Errorf("the compact rendering never states how many PVCs there are, so it is compact by "+
			"saying nothing:\n%s", joined)
	}

	full := textLines(RenderText(rep, TextOptions{All: true}))
	// maxCoverageItems is the DOCUMENT's cap on per-PVC rows, so --all lists every row the report
	// carries and not more. Both halves of that are asserted: the flag must produce the rows, and the
	// rendering must admit to the ones the document does not hold — a listing that silently stopped at
	// five hundred while claiming to be "every PVC" would be the worst possible outcome for a command
	// whose subject is what is NOT covered.
	if len(full) < maxCoverageItems {
		t.Errorf("--all produced %d lines for 600 PVCs, fewer than the %d rows the report carries",
			len(full), maxCoverageItems)
	}
	if len(full) <= len(compact) {
		t.Errorf("--all (%d lines) is no longer than the default (%d): the flag does nothing",
			len(full), len(compact))
	}
	if !strings.Contains(collapse(strings.Join(full, "\n")), "carry no row in this report") {
		t.Error("--all listed a truncated set without saying that rows were omitted")
	}
}

// TestTextAndJSONAgreeOnTheVerdict is the anti-second-source-of-truth test.
//
// The strong form is a round-trip: rendering the text from a Report that has been through the JSON
// encoder and back must produce byte-identical output. That is only possible if the text is a pure
// function of the serialised document — no cluster, no clock, no recomputation — which is precisely the
// property that stops the terminal and the file ever disagreeing about the same installation.
func TestTextAndJSONAgreeOnTheVerdict(t *testing.T) {
	rep := collectCoverageReport(t, coverageFixture(t))

	direct := RenderText(rep, TextOptions{})

	body, err := json.Marshal(rep)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	roundTripped, err := Parse(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	viaJSON := RenderText(roundTripped, TextOptions{})

	if !bytes.Equal(direct, viaJSON) {
		t.Errorf("the text rendered from the report and from its own JSON differ, so text mode is "+
			"reading something the JSON does not carry:\n--- direct ---\n%s\n--- via JSON ---\n%s",
			direct, viaJSON)
	}

	// And the verdict itself has to be legible in the text, not merely equal internally: a reader who
	// only ever sees the terminal must get the same headline as a maintainer reading the JSON.
	out := string(direct)
	if !strings.Contains(out, textVerdictWord(rep.Verdict.Status)) {
		t.Errorf("the text does not state the report's verdict %q:\n%s", rep.Verdict.Status, out)
	}
	if !strings.Contains(out, rep.Verdict.Summary) {
		// Wrapping means the summary may be split across lines; compare on collapsed whitespace.
		if !strings.Contains(collapse(out), collapse(rep.Verdict.Summary)) {
			t.Errorf("the text does not carry the verdict summary %q", rep.Verdict.Summary)
		}
	}
}

// TestTextSaysItsIdentifiersAreReal pins the asymmetry that must never be silent.
//
// Text mode prints real names because hashed ones would be useless in the reader's own terminal. The
// danger is the reader who pastes that into a public issue believing it was the redacted form, so the
// output states the difference itself and names the flag that produces a shareable copy. A test rather
// than a convention, because a disclosure nobody asserts is a disclosure somebody deletes.
func TestTextSaysItsIdentifiersAreReal(t *testing.T) {
	// Collected in the DEFAULT (hashed) mode on purpose: the notice must appear regardless of how the
	// report was collected, because it is a statement about this rendering, not about that collection.
	rep := collectFixture(t, false)
	out := string(RenderText(rep, TextOptions{}))

	if !strings.Contains(collapse(out), collapse(textIdentifierNotice)) {
		t.Errorf("the text output does not disclose that its identifiers are real:\n%s", out)
	}
	if !strings.Contains(out, "--format json") {
		t.Error("the disclosure does not name the flag that produces a shareable copy, which is the " +
			"only part of it a reader can act on")
	}
}

// TestSelfcheckDefaultsToJSON is a compatibility test with a specific victim.
//
// hack/soak/manifests/fallback-selfcheck-cronjob.yaml redirects a bare `selfcheck`'s stdout into a
// .json file and is designed to run unattended for months. If the default ever becomes text, that
// CronJob silently writes prose into a file named .json and the failure surfaces weeks later as an
// unparseable soak series. This test is what stops that being a one-line change.
func TestSelfcheckDefaultsToJSON(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := runSelfcheck(context.Background(), nil, &stdout, &stderr, fixtureConnector(t)); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr.String())
	}
	if _, err := Parse(stdout.Bytes()); err != nil {
		t.Fatalf("a bare `selfcheck` no longer writes a parseable JSON report to stdout: %v\n%s",
			err, stdout.String())
	}
}

// TestSelfcheckFormatTextWritesText is the other half: the flag has to actually change the output.
func TestSelfcheckFormatTextWritesText(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runSelfcheck(context.Background(), []string{"--format", "text"},
		&stdout, &stderr, fixtureConnector(t))
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr.String())
	}
	if _, err := Parse(stdout.Bytes()); err == nil {
		t.Fatal("--format text still wrote a JSON report")
	}
	if !strings.HasPrefix(stdout.String(), "CrystalBackup self-check") {
		t.Errorf("--format text wrote something else entirely:\n%s", stdout.String())
	}
	// The redaction announcement belongs to the JSON path only: text mode has already said, in the
	// body the reader is looking at, that its names are real. Repeating a "identifiers redacted"
	// message beside output that is not redacted would be the worst of both.
	if strings.Contains(stderr.String(), "identifiers redacted") {
		t.Errorf("text mode announced a redaction it did not perform: %s", stderr.String())
	}
}

// TestSelfcheckRefusesAnUnknownFormat and TestSelfcheckRefusesAllWithoutText pin the two flag
// mistakes, and pin that they are REFUSED rather than ignored — the rule this package already applies
// to `report --from`, for the same reason: a flag that silently did nothing is how a reader ends up
// trusting output they did not ask for.
func TestSelfcheckRefusesAnUnknownFormat(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runSelfcheck(context.Background(), []string{"--format", "yaml"},
		&stdout, &stderr, forbiddenConnector(t))
	if code == 0 {
		t.Fatal("an unknown --format was accepted")
	}
	if stdout.Len() != 0 {
		t.Errorf("a refused run still wrote to stdout: %s", stdout.String())
	}
}

func TestSelfcheckRefusesAllWithoutText(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runSelfcheck(context.Background(), []string{"--all"},
		&stdout, &stderr, forbiddenConnector(t))
	if code == 0 {
		t.Fatal("--all was accepted on the JSON path, where it changes nothing")
	}
	if !strings.Contains(stderr.String(), "--format text") {
		t.Errorf("the refusal does not tell the reader what to type instead: %s", stderr.String())
	}
}

// TestCronInWordsAndNextOccurrence covers the translator and, more importantly, its refusal to guess.
//
// The empty string for an expression it cannot render exactly is the point: a wrong sentence about
// when somebody's backup runs is believed, and an absent one is looked up. Every case here is a shape
// that appears in real backup schedules, plus the two-activation trap that a report showing only one
// time would hide.
func TestCronInWordsAndNextOccurrence(t *testing.T) {
	cases := []struct {
		expr, tz, want string
	}{
		{"0 2 * * *", "", "every day at 02:00 UTC"},
		{"0 2 * * *", "Europe/Paris", "every day at 02:00 Europe/Paris"},
		{"30 4 * * 0", "", "every Sunday at 04:30 UTC"},
		{"0 6 * * 1-5", "", "every Monday, Tuesday, Wednesday, Thursday and Friday at 06:00 UTC"},
		{"0 5 * * SUN", "", "every Sunday at 05:00 UTC"},
		{"0 2,3 * * *", "", "every day at 02:00 and 03:00 UTC"},
		{"15 1 * * *", "", "every day at 01:15 UTC"},
		{"0 3 1 * *", "", "on day 1 of every month at 03:00 UTC"},
		{"*/15 * * * *", "", "every 15 minutes UTC"},
		{"@daily", "", "every day at 00:00 UTC"},
		{"@weekly", "", "every Sunday at 00:00 UTC"},
		// Refusals: a month restriction, a range of hours, and cron's day-of-month OR day-of-week
		// overlap. All three get a computed next occurrence instead of a sentence.
		{"0 2 * 3 *", "", ""},
		{"0 1-6 * * *", "", ""},
		{"0 2 1 * 0", "", ""},
	}
	for _, tc := range cases {
		if got := cronInWords(tc.expr, tc.tz); got != tc.want {
			t.Errorf("cronInWords(%q, %q) = %q, want %q", tc.expr, tc.tz, got, tc.want)
		}
	}

	// The next occurrence is the answer the words are only a convenience for, so it must exist even
	// where the words do not — including for the expressions above that this translator refuses.
	for _, expr := range []string{"0 2 * 3 *", "0 1-6 * * *", "0 2 1 * 0"} {
		if cronNext(expr, "", fixtureNow()) == nil {
			t.Errorf("cronNext(%q) is nil, so an expression with no English rendering has no answer "+
				"at all", expr)
		}
	}
	if cronNext("not a cron", "", fixtureNow()) != nil {
		t.Error("an unparseable expression produced a next occurrence, which would be a fabricated " +
			"prediction")
	}
}

// TestRetentionSentenceNamesTheUnboundedCase pins the finding that has no condition, no event and no
// alert anywhere else in the system.
//
// A location with an empty spec.retention never runs a `restic forget` — deliberately, because a
// keep-less forget would delete every snapshot — so its bucket grows for ever and nothing says so. A
// report of a new installation is the one place a reader will meet that fact in time to act on it.
func TestRetentionSentenceNamesTheUnboundedCase(t *testing.T) {
	empty := retentionSentence(cbv1RetentionEmpty(), "Standard")
	if !strings.Contains(empty, "without bound") {
		t.Errorf("an empty retention policy does not warn that the repository grows for ever: %q", empty)
	}

	set := retentionSentence(cbv1Retention(7, 4, 6), "Standard")
	if strings.Contains(set, "without bound") {
		t.Errorf("a configured retention policy was reported as unbounded: %q", set)
	}
	if !strings.Contains(set, "roughly 6 months") {
		t.Errorf("the sentence does not state how far back the history reaches, which is the number "+
			"no field carries: %q", set)
	}

	immutable := retentionSentence(cbv1Retention(7, 4, 6), "Immutable")
	if !strings.Contains(immutable, "IGNORED") {
		t.Errorf("retention on an Immutable location was not reported as ignored: %q", immutable)
	}
}

// --- helpers --------------------------------------------------------------------------------

func textLines(out []byte) []string {
	return strings.Split(strings.TrimRight(string(out), "\n"), "\n")
}

// collapse squashes all whitespace so an assertion about a sentence is not an assertion about where
// the renderer chose to wrap it.
func collapse(s string) string { return strings.Join(strings.Fields(s), " ") }

func collectSynthetic(t *testing.T, n int) *Report {
	t.Helper()
	return collectCoverageReport(t, syntheticCoverageCluster(t, n))
}
