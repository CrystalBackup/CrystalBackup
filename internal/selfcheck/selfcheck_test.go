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
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/version"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
)

// secrets are the identifiers the fixture cluster is built out of: the customer's name, their
// internal S3 host, their tenants, their volumes. Every test that asserts on redaction asserts
// against THIS list, so adding a field to the report that forgets to redact something fails
// immediately rather than at the first issue somebody files.
var secrets = []string{
	"acme-corp",
	"acme-billing-prod",
	"acme-analytics",
	"s3.acme-internal.example",
	"acme-backups-eu",
	"acme-eu-west-cluster",
	"customer-ledger-db",
	"acme-nightly",
	"acme-dr-copy",
	"acme-s3-credentials",
	"acme-offsite",
}

const operatorNS = "crystal-backup-system"

// TestDefaultModeLeavesNoPlaintextIdentifier is the test this lot's third deliverable exists for.
//
// It is written against the SERIALISED bytes, not against the struct, and against both outputs. A
// field-by-field assertion would pass forever while a new field quietly carried a namespace name
// into the JSON, and it is the file that gets attached to the issue, not the struct.
func TestDefaultModeLeavesNoPlaintextIdentifier(t *testing.T) {
	rep := collectFixture(t, false)

	body, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	assertNoSecrets(t, "JSON", string(body))

	page, err := Render(rep)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	assertNoSecrets(t, "HTML", string(page))
}

// TestBreachDetailsAreRedactedToo covers the half that is easiest to get wrong: a Breach.Detail is
// a SENTENCE containing an object name, not a field, and no amount of per-field redaction reaches
// it. It is also the most valuable line in the report, so dropping it was never an option.
func TestBreachDetailsAreRedactedToo(t *testing.T) {
	rep := collectFixture(t, false)
	var details []string
	for _, r := range rep.Rules {
		for _, b := range r.Breaches {
			details = append(details, b.Detail)
		}
	}
	if len(details) == 0 {
		t.Fatal("the fixture produced no breach, so this test proves nothing about breach details")
	}
	assertNoSecrets(t, "breach details", strings.Join(details, "\n"))
}

// TestPVCNameInADetailIsRedacted is that gap, pinned by name. A PVC is the one identifier class
// this report has no table for — it appears only inside the pileup rule's Detail — so nothing but
// LearnLabels would ever have registered it.
func TestPVCNameInADetailIsRedacted(t *testing.T) {
	rep := collectFixture(t, false)
	for _, r := range rep.Rules {
		if r.Name != "CrystalbackupPVCSnapshotPileup" {
			continue
		}
		if len(r.Breaches) == 0 {
			t.Fatal("the fixture should breach the pileup rule")
		}
		for _, b := range r.Breaches {
			if strings.Contains(b.Detail, "customer-ledger-db") {
				t.Fatalf("the PVC name survived into a breach detail: %q", b.Detail)
			}
			if !strings.Contains(b.Detail, "pvc-") {
				t.Errorf("expected a pvc- token in the detail, got %q", b.Detail)
			}
		}
		return
	}
	t.Fatal("no pileup rule in the report")
}

// TestCorrelationSurvivesRedaction is the property that makes a redacted report worth reading at
// all: the same namespace has to be the same token in the inventory, in the census and in every
// breach, or a maintainer cannot follow a finding back to the object it is about.
func TestCorrelationSurvivesRedaction(t *testing.T) {
	rep := collectFixture(t, false)

	var scheduleNS string
	for _, s := range rep.Inventory.Schedules {
		if s.Namespace != "" {
			scheduleNS = s.Namespace
			break
		}
	}
	if scheduleNS == "" {
		t.Fatal("no namespaced schedule in the fixture report")
	}
	if _, ok := rep.Inventory.Backups.ByNamespace[scheduleNS]; !ok {
		t.Errorf("namespace token %q from the schedule table does not appear in the backup census "+
			"keys %v — the tokens are not stable within the report", scheduleNS, rep.Inventory.Backups.ByNamespace)
	}
	found := false
	for _, r := range rep.Rules {
		for _, b := range r.Breaches {
			if b.Labels["namespace"] == scheduleNS {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("namespace token %q never appears in a breach label; a reader could not tie a "+
			"finding back to the row it is about", scheduleNS)
	}

	// The operator namespace is one string that appears in the header AND in every leak sample.
	// Rendering it in clear in one place and as a token in the other would be both a leak and a
	// document that contradicts itself.
	if rep.Operator.Namespace == operatorNS {
		t.Error("the operator namespace is in clear in hashed mode")
	}
	for _, s := range rep.Leaks.Samples {
		if s.Namespace != rep.Operator.Namespace && s.Namespace != "" {
			t.Errorf("leak sample namespace %q does not match the header's %q — the same namespace "+
				"is rendered two ways in one report", s.Namespace, rep.Operator.Namespace)
		}
	}
}

// TestSaltIsPerReport: two reports of the SAME cluster must share no tokens. Without this the hash
// is a stable pseudonym across every issue an operator ever files, which is a different and worse
// privacy property than the one advertised.
func TestSaltIsPerReport(t *testing.T) {
	a := collectFixture(t, false)
	b := collectFixture(t, false)
	if len(a.Inventory.Locations) == 0 {
		t.Fatal("fixture has no location")
	}
	if a.Inventory.Locations[0].Name == b.Inventory.Locations[0].Name {
		t.Errorf("two reports produced the same token %q for the same location: the salt is not "+
			"per-report, so the tokens are a stable pseudonym across every report this cluster ever "+
			"emits", a.Inventory.Locations[0].Name)
	}
	if a.Operator.Namespace == b.Operator.Namespace {
		t.Errorf("two reports produced the same token %q for the same namespace: the default salt is "+
			"no longer per-report", a.Operator.Namespace)
	}
	if a.Redaction.SaltSource != SaltRandomPerReport {
		t.Errorf("saltSource = %q, want %q — a reader cannot tell which of the two hashed modes "+
			"produced this file", a.Redaction.SaltSource, SaltRandomPerReport)
	}
}

// --- caller-supplied salt -----------------------------------------------------------------------
//
// The salt is 40 bytes rather than 32 so the tests below cannot pass by accident on a boundary, and
// it is printable ASCII so that a leak of it into the JSON or the HTML is visible in a diff rather
// than being a run of bytes nobody recognises.
var soakSalt = []byte("SOAK-SALT-DO-NOT-LEAK-0123456789abcdefgh")

func writeSalt(t *testing.T, name string, body []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write salt fixture: %v", err)
	}
	return path
}

// tokens is every identifier token in a report that two collections can be compared on. Taking
// several rather than one means a test cannot pass because a single field happened to match.
func tokens(rep *Report) []string {
	out := make([]string, 0, 2+len(rep.Inventory.Locations)+2*len(rep.Inventory.Schedules))
	out = append(out, rep.Operator.Namespace, rep.Cluster.ClusterID)
	for _, l := range rep.Inventory.Locations {
		out = append(out, l.Name)
	}
	for _, s := range rep.Inventory.Schedules {
		out = append(out, s.Name, s.Namespace)
	}
	return out
}

// TestSuppliedSaltMakesTokensCorrelateAcrossReports is the whole reason the flag exists: a soak
// takes one of these a day for a fortnight, and unless the same namespace is the same token in all
// fourteen there is no series to read — no drift, no growth, and no way to see the namespace that
// stopped being backed up on day nine.
func TestSuppliedSaltMakesTokensCorrelateAcrossReports(t *testing.T) {
	a := collectFixtureSalted(t, false, soakSalt)
	b := collectFixtureSalted(t, false, soakSalt)

	got, want := tokens(a), tokens(b)
	if len(got) == 0 {
		t.Fatal("the fixture yielded no tokens to compare, so this test proves nothing")
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("two reports from the same salt disagree at token %d: %q vs %q — the supplied "+
				"salt is not being used, so a soak's reports still share nothing", i, got[i], want[i])
		}
	}
	// And the tokens must still be tokens, not the names.
	body, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	assertNoSecrets(t, "JSON (supplied salt)", string(body))
}

// TestADifferentSaltIsADifferentPseudonym. Correlation must be scoped to the salt, or two unrelated
// installations salted differently could still be matched against each other.
func TestADifferentSaltIsADifferentPseudonym(t *testing.T) {
	a := collectFixtureSalted(t, false, soakSalt)
	b := collectFixtureSalted(t, false, []byte("A-COMPLETELY-DIFFERENT-SALT-0123456789ab"))
	if a.Inventory.Locations[0].Name == b.Inventory.Locations[0].Name {
		t.Errorf("two different salts produced the same token %q: the salt is not reaching the HMAC",
			a.Inventory.Locations[0].Name)
	}
}

// TestSuppliedSaltSaysSoInTheReport is the honesty requirement. The two hashed modes make different
// promises and are otherwise indistinguishable from the outside, so the file has to say which one
// it is — a report that claims "a 32-byte random salt" over a fixed one is worse than one that
// claims nothing.
func TestSuppliedSaltSaysSoInTheReport(t *testing.T) {
	fixed := collectFixtureSalted(t, false, soakSalt).Redaction
	random := collectFixture(t, false).Redaction

	if fixed.SaltSource != SaltCallerSupplied {
		t.Errorf("saltSource = %q, want %q", fixed.SaltSource, SaltCallerSupplied)
	}
	if fixed.Algorithm == random.Algorithm {
		t.Errorf("both modes report the same algorithm %q — the fixed-salt report is claiming a "+
			"random salt it does not have", fixed.Algorithm)
	}
	if strings.Contains(fixed.Algorithm, "random") {
		t.Errorf("the fixed-salt algorithm string still says random: %q", fixed.Algorithm)
	}
	if fixed.Note == random.Note {
		t.Error("both modes carry the same note; a reader cannot tell what they are about to paste")
	}
	// The note has to warn about the property that differs, in words, not by omission.
	for _, want := range []string{"ACROSS EVERY REPORT", "--redaction-salt-file", "public issue"} {
		if !strings.Contains(fixed.Note, want) {
			t.Errorf("the fixed-salt note does not mention %q: %q", want, fixed.Note)
		}
	}
	if fixed.SaltDisclosed || random.SaltDisclosed {
		t.Error("saltDisclosed is true in a mode that does not disclose the salt")
	}
	// And it has to survive into the page, which is what a maintainer actually reads.
	page, err := Render(collectFixtureSalted(t, false, soakSalt))
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(string(page), "caller-supplied salt") {
		t.Error("the rendered page does not say the salt was caller-supplied")
	}
}

// TestSuppliedSaltNeverReachesTheOutput. The salt is the one secret this feature introduces: with
// it, every token in every report built on it is reversible by dictionary. It is asserted on the
// SERIALISED bytes of both outputs, so a field added later leaks into this test's view rather than
// past it, and against the encodings something well-meaning might have used.
func TestSuppliedSaltNeverReachesTheOutput(t *testing.T) {
	path := writeSalt(t, "soak.salt", soakSalt)
	salt, err := LoadRedactionSalt(path)
	if err != nil {
		t.Fatalf("load salt: %v", err)
	}
	rep := collectFixtureSalted(t, false, salt)

	body, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	page, err := Render(rep)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	forbidden := map[string]string{
		"the salt verbatim":  string(soakSalt),
		"the salt in hex":    hex.EncodeToString(soakSalt),
		"the salt in base64": base64.StdEncoding.EncodeToString(soakSalt),
		"SHA-256 of the salt": func() string {
			sum := sha256.Sum256(soakSalt)
			return hex.EncodeToString(sum[:])
		}(),
		"the salt file path": path,
	}
	for _, out := range []struct{ where, body string }{{"JSON", string(body)}, {"HTML", string(page)}} {
		for what, s := range forbidden {
			if strings.Contains(out.body, s) {
				t.Errorf("%s carries %s (%q): every token in every report built on this salt is now "+
					"reversible by whoever reads the file", out.where, what, s)
			}
		}
	}
}

// TestShortSaltFileIsRefusedByLength. The salt somebody types by hand is the failure mode here, and
// it fails silently: a four-character salt still produces tokens that LOOK exactly like good ones.
// The error names the length found because "too short" leaves the admin guessing at both the
// requirement and what they actually gave.
func TestShortSaltFileIsRefusedByLength(t *testing.T) {
	for _, n := range []int{0, 1, MinSaltBytes - 1} {
		path := writeSalt(t, "short.salt", bytes.Repeat([]byte("x"), n))
		_, err := LoadRedactionSalt(path)
		if err == nil {
			t.Fatalf("a %d-byte salt file was accepted", n)
		}
		if !strings.Contains(err.Error(), fmt.Sprintf("%d bytes", n)) {
			t.Errorf("the error for a %d-byte salt does not name the length found: %v", n, err)
		}
		if !strings.Contains(err.Error(), fmt.Sprintf("%d", MinSaltBytes)) {
			t.Errorf("the error for a %d-byte salt does not name the requirement: %v", n, err)
		}
	}
}

// TestMissingSaltFileIsRefusedRatherThanFallingBack is the one that matters most, because the
// fallback would be invisible: a report built on a fresh random salt is indistinguishable at a
// glance from one built on the soak's, and the gap is discovered a fortnight later by the person
// who cannot work out why day nine correlates with nothing.
func TestMissingSaltFileIsRefusedRatherThanFallingBack(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist.salt")
	if _, err := LoadRedactionSalt(missing); err == nil {
		t.Fatal("a missing salt file was accepted; the run would have silently used a random salt")
	}

	// A directory stands in for the unreadable case: it exists, and reading it fails.
	if _, err := LoadRedactionSalt(t.TempDir()); err == nil {
		t.Fatal("an unreadable salt file was accepted")
	}

	// And the command exits non-zero rather than producing a report — checked through the CLI,
	// because that is where a fallback would have been written.
	var stdout, stderr bytes.Buffer
	code := RunSelfcheck(context.Background(), []string{"--redaction-salt-file", missing}, &stdout, &stderr)
	if code == 0 {
		t.Errorf("selfcheck exited 0 with an unreadable salt file")
	}
	if stdout.Len() > 0 {
		t.Errorf("a report was written despite the unreadable salt file: %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "redaction salt file") {
		t.Errorf("stderr does not name the problem: %q", stderr.String())
	}
}

// TestSaltFileAndFullAreMutuallyExclusive. Accepting both would mean a soak script that still
// carried --full from a debugging session emitted a fortnight of VERBATIM namespace inventories
// while its author believed they were pseudonyms.
func TestSaltFileAndFullAreMutuallyExclusive(t *testing.T) {
	path := writeSalt(t, "soak.salt", soakSalt)
	var stdout, stderr bytes.Buffer
	code := RunSelfcheck(context.Background(),
		[]string{"--full", "--redaction-salt-file", path}, &stdout, &stderr)
	if code != 2 {
		t.Errorf("exit code = %d, want 2 (usage error)", code)
	}
	if stdout.Len() > 0 {
		t.Errorf("a report was written for a rejected flag combination: %q", stdout.String())
	}
	for _, want := range []string{"--full", "--redaction-salt-file"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr does not name %s: %q", want, stderr.String())
		}
	}
	// The same rule, held by the type that would otherwise carry a salt it never uses.
	if _, err := NewRedactor(true, soakSalt); err == nil {
		t.Error("NewRedactor accepted a salt in full mode")
	}
}

// TestSaltIsNeverWrittenIntoTheReport. A disclosed salt makes every token reversible by dictionary,
// which is the whole reason the salt is random in the first place.
func TestSaltIsNeverWrittenIntoTheReport(t *testing.T) {
	rep := collectFixture(t, false)
	if rep.Redaction.SaltDisclosed {
		t.Error("saltDisclosed is true")
	}
	body, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	if regexp.MustCompile(`(?i)"?salt"?\s*:\s*"[0-9a-f]{16,}`).Match(body) {
		t.Error("something that looks like a salt was serialised into the report")
	}
}

// TestFullModeIsOptInAndComplete: --full has to actually produce the unredacted document, or
// nobody will use it and everyone will paste raw kubectl output into the issue instead.
func TestFullModeIsOptInAndComplete(t *testing.T) {
	rep := collectFixture(t, true)
	body, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"acme-billing-prod", "s3.acme-internal.example", "acme-backups-eu"} {
		if !strings.Contains(string(body), s) {
			t.Errorf("--full did not pass %q through", s)
		}
	}
	if rep.Redaction.Mode != "full" {
		t.Errorf("mode = %q, want full", rep.Redaction.Mode)
	}
}

// TestNoCredentialSurvivesEitherMode. The Secret NAME is reported (redacted) because "which Secret
// is this location pointing at" is a real triage question; its CONTENTS are not read in any mode,
// and neither is the CA bundle. This checks the whole document in the loudest mode there is.
func TestNoCredentialSurvivesEitherMode(t *testing.T) {
	for _, full := range []bool{false, true} {
		rep := collectFixture(t, full)
		body, err := json.Marshal(rep)
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{
			"AKIAIOSFODNN7EXAMPLE",        // the S3 access key in the fixture Secret
			"wJalrXUtnFEMI/K7MDENG",       // the secret key
			"-----BEGIN CERTIFICATE-----", // the CA bundle
			"hunter2",                     // the restic password
		} {
			if strings.Contains(string(body), forbidden) {
				t.Errorf("full=%v: credential material %q appears in the report", full, forbidden)
			}
		}
	}
}

// TestImagesComeFromImageIDNotFromTheSpec is the 0.5.1 lesson made testable. The fixture's pod
// declares a mutable tag and the kubelet resolved it to a DIFFERENT digest; a report built from
// spec.containers[].image would describe an artifact nobody is running.
func TestImagesComeFromImageIDNotFromTheSpec(t *testing.T) {
	rep := collectFixture(t, false)
	if len(rep.Images.Running) == 0 {
		t.Fatal("no running image in the report")
	}
	var operator *RunningImage
	for i := range rep.Images.Running {
		if rep.Images.Running[i].Role == roleOperator {
			operator = &rep.Images.Running[i]
		}
	}
	if operator == nil {
		t.Fatal("the operator pod was not classified as such")
	}
	if operator.Digest != fixtureRunningDigest {
		t.Errorf("digest = %q, want the RESOLVED one %q — this is read from imageID, not from the "+
			"spec's mutable tag", operator.Digest, fixtureRunningDigest)
	}
	if operator.Tag != "0.6.0" {
		t.Errorf("tag = %q, want the declared 0.6.0 kept alongside the digest", operator.Tag)
	}
	// The digest survives redaction: it is a content hash of a public artifact, and it is the single
	// most useful field here.
	page, err := Render(rep)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(page), fixtureRunningDigest) {
		t.Error("the running digest did not survive into the rendered page")
	}
}

// TestSyncImageIsNotCollapsedIntoMover: three images is the design (adr/0013), and a report that
// showed two could not surface the one thing that split exists for.
func TestSyncImageIsNotCollapsedIntoMover(t *testing.T) {
	rep := collectFixture(t, false)
	roles := map[string]bool{}
	for _, img := range rep.Images.Running {
		roles[img.Role] = true
	}
	for _, want := range []string{roleOperator, roleMover, roleSync} {
		if !roles[want] {
			t.Errorf("no %s image in the report; roles seen: %v", want, roles)
		}
	}
}

// TestNotEvaluatedIsNeverReportedAsOK. The whole lot turns on this one: a rule with no predicate
// must not contribute to the OK count, must not be StatusOK, and must say why.
func TestNotEvaluatedIsNeverReportedAsOK(t *testing.T) {
	rules := []RuleResult{
		{Name: "a", Status: StatusOK},
		{Name: "b", Status: StatusNotEvaluated},
		{Name: "c", Status: StatusError},
	}
	v := verdictOf(rules)
	if v.OK != 1 {
		t.Errorf("OK = %d, want 1: only the evaluated pass counts", v.OK)
	}
	if v.NotEvaluated != 1 || v.Errored != 1 {
		t.Errorf("notEvaluated=%d errored=%d, want 1 and 1", v.NotEvaluated, v.Errored)
	}
	if !strings.Contains(v.Summary, "not passes") {
		t.Errorf("the summary must say the unevaluated rules are not passes: %q", v.Summary)
	}

	// And on the real report: every not-evaluated rule carries a reason.
	rep := collectFixture(t, false)
	for _, r := range rep.Rules {
		if r.Status == StatusNotEvaluated && r.Reason == "" {
			t.Errorf("rule %s is not evaluated and says nothing about why", r.Name)
		}
		if r.Status == StatusNotEvaluated && strings.Contains(strings.ToLower(r.Reason), "pass") &&
			!strings.Contains(r.Reason, "NOT a pass") {
			t.Errorf("rule %s's reason reads like a pass: %q", r.Name, r.Reason)
		}
	}
}

// TestCriticalBreachMakesTheVerdictUnhealthy: only a critical rule says the RESTORE PATH is
// compromised, and only that should be able to produce the strongest word in the report.
func TestCriticalBreachMakesTheVerdictUnhealthy(t *testing.T) {
	warn := verdictOf([]RuleResult{{Status: StatusBreached, Severity: "warning"}})
	if warn.Status != "degraded" {
		t.Errorf("a warning breach gave %q, want degraded", warn.Status)
	}
	crit := verdictOf([]RuleResult{{Status: StatusBreached, Severity: "critical"}})
	if crit.Status != "unhealthy" {
		t.Errorf("a critical breach gave %q, want unhealthy", crit.Status)
	}
}

// TestFidelityCaveatsReachTheReader. A blind spot that stays in a Go comment is not a disclosed
// blind spot; it has to be in the JSON and on the page, next to the verdict it qualifies.
func TestFidelityCaveatsReachTheReader(t *testing.T) {
	rep := collectFixture(t, false)
	var caveated string
	for _, r := range rep.Rules {
		if r.Fidelity != "" {
			caveated = r.Fidelity
		}
	}
	if caveated == "" {
		t.Fatal("no rule carried a fidelity caveat into the report, but alerts declares some")
	}
	page, err := Render(rep)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(page), "Measurement caveat") {
		t.Error("the rendered page does not surface the measurement caveat")
	}
}

// TestRoundTripJSONToHTML is the workflow, end to end and with no cluster in the second half:
// collect, serialise, hand the bytes to a fresh Parse, render. That second half is what a
// maintainer runs on a file from an issue.
func TestRoundTripJSONToHTML(t *testing.T) {
	rep := collectFixture(t, false)
	body, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		t.Fatal(err)
	}

	parsed, err := Parse(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if parsed.ReportVersion != ReportVersion {
		t.Errorf("reportVersion = %d, want %d", parsed.ReportVersion, ReportVersion)
	}
	if len(parsed.Rules) != len(rep.Rules) {
		t.Errorf("round trip lost rules: %d vs %d", len(parsed.Rules), len(rep.Rules))
	}

	page, err := Render(parsed)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	html := string(page)
	for _, want := range []string{
		"<!DOCTYPE html>",
		"Installation self-check",
		"CrystalbackupBackupMissed",
		"Rule verdicts",
		"Leak indicators",
		parsed.Verdict.Summary,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("rendered page is missing %q", want)
		}
	}
}

// TestRenderedPageIsSelfContained. The page is opened from an email attachment on a machine that
// may have no network at all, and a report that phones home is also a report that tells a third
// party who is reading it.
func TestRenderedPageIsSelfContained(t *testing.T) {
	rep := collectFixture(t, false)
	page, err := Render(rep)
	if err != nil {
		t.Fatal(err)
	}
	html := string(page)
	for _, forbidden := range []string{
		"<script", "<link", "<iframe", "@import", "src=", "url(http",
	} {
		if strings.Contains(strings.ToLower(html), forbidden) {
			t.Errorf("the page contains %q — it must make no outbound request and load no remote asset", forbidden)
		}
	}
	// The stylesheet has to be genuinely inlined, not merely not-linked.
	if !strings.Contains(html, "--paper:") {
		t.Error("the stylesheet is not inlined")
	}
}

// TestRenderIsDeterministic: two renders of one report must be byte-identical, so a maintainer
// diffing two attachments sees only what changed in the cluster.
func TestRenderIsDeterministic(t *testing.T) {
	rep := collectFixture(t, false)
	first, err := Render(rep)
	if err != nil {
		t.Fatal(err)
	}
	for range 5 {
		again, err := Render(rep)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(first, again) {
			t.Fatal("two renders of the same report differ")
		}
	}
}

// TestParseRejectsSomethingThatIsNotAReport. An empty or truncated file unmarshals into a zero
// Report without error, and rendering that would produce a plausible page describing nothing —
// which for a document whose only job is to be trusted is the worst possible failure.
func TestParseRejectsSomethingThatIsNotAReport(t *testing.T) {
	for _, in := range []string{`{}`, `{"foo":1}`, `null`} {
		if _, err := Parse([]byte(in)); err == nil {
			t.Errorf("Parse(%s) accepted a document with no reportVersion", in)
		}
	}
	if _, err := Parse([]byte(`not json`)); err == nil {
		t.Error("Parse accepted invalid JSON")
	}
}

// TestReportCommandRendersFromAFileWithNoCluster is the second deliverable's contract, exercised
// through the actual CLI entry point: no client is built, nothing is read but the file.
func TestReportCommandRendersFromAFileWithNoCluster(t *testing.T) {
	rep := collectFixture(t, false)
	body, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	in := filepath.Join(dir, "health.json")
	out := filepath.Join(dir, "report.html")
	if err := os.WriteFile(in, body, 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if code := RunReport([]string{"--from", in, "--output", out}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	page, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(page, []byte("Installation self-check")) {
		t.Error("the rendered file is not the report")
	}
	assertNoSecrets(t, "rendered file", string(page))
}

// TestReportCommandRefusesWithoutFrom keeps the usage honest — the flag is the whole command.
func TestReportCommandRefusesWithoutFrom(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := RunReport(nil, &stdout, &stderr); code == 0 {
		t.Error("report with no --from exited 0")
	}
	if !strings.Contains(stderr.String(), "--from") {
		t.Errorf("the error does not name the missing flag: %q", stderr.String())
	}
}

// TestLeakResidualIsAgeSplit. The raw count of managed exposure objects is meaningless — a running
// backup owns several — so the number that means something is the one past the reaper's grace.
func TestLeakResidualIsAgeSplit(t *testing.T) {
	rep := collectFixture(t, false)
	byKind := map[string]LeakKind{}
	for _, k := range rep.Leaks.Kinds {
		byKind[k.Kind] = k
	}
	job, ok := byKind["Job"]
	if !ok {
		t.Fatal("no Job leak census in the report")
	}
	if job.Total != 2 {
		t.Errorf("Job total = %d, want both the in-flight and the stranded one", job.Total)
	}
	if job.Residual != 1 {
		t.Errorf("Job residual = %d, want only the stranded one — the in-flight Job is younger "+
			"than the reaper's grace and is not a leak", job.Residual)
	}
	if rep.Leaks.Totals.Residual < 1 {
		t.Error("the residual total does not reflect the stranded Job")
	}
}

// TestUnreadableSectionBecomesADiagnostic. "Empty" and "not allowed to look" must never render the
// same, which is the only reason the diagnostics section exists.
func TestUnreadableSectionBecomesADiagnostic(t *testing.T) {
	rep, err := Collect(context.Background(), Options{
		Reader:            forbiddenReader{Reader: emptyClient(t), kinds: map[string]bool{"PodList": true}},
		OperatorNamespace: operatorNS,
		Now:               fixtureNow(),
	})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, d := range rep.Diagnostics {
		if d.Area == "images" || d.Area == "operator" {
			found = true
			if d.Impact == "" {
				t.Errorf("diagnostic %+v does not say what is missing from the report", d)
			}
		}
	}
	if !found {
		t.Error("a forbidden pod list produced no diagnostic; the empty images section would read " +
			"as 'nothing is running'")
	}
	page, err := Render(rep)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(page), "What could not be read") {
		t.Error("the page does not surface the diagnostics")
	}
}

// TestARuleThatCannotBeEvaluatedBecomesADiagnostic is the rule-side half of the property above,
// and it matters more, because a rule entry is one row among eleven.
//
// A predicate that errors leaves a HOLE in the verdict. The row says `error`, but a reader scanning
// a report under pressure — which is the only condition this report is ever read in — sees ten
// greens and does not audit the eleventh. The diagnostics section is where this document states
// what it could not measure, so a rule it could not evaluate belongs there too, named.
//
// Forbidding BackupRepositoryList is the realistic shape: an operator running the self-check with
// their own credentials rather than the operator's. The same path carries alerts.ErrUnknownRule,
// which used to be a panic in this binary — see thresholdOf.
func TestARuleThatCannotBeEvaluatedBecomesADiagnostic(t *testing.T) {
	rep, err := Collect(context.Background(), Options{
		Reader: forbiddenReader{
			Reader: emptyClient(t),
			kinds:  map[string]bool{"BackupRepositoryList": true},
		},
		OperatorNamespace: operatorNS,
		Now:               fixtureNow(),
	})
	if err != nil {
		t.Fatalf("a cluster the reader cannot fully read must still produce a report: %v", err)
	}

	errored := map[string]bool{}
	for _, r := range rep.Rules {
		if r.Status == StatusError {
			errored[r.Name] = true
			if r.Reason == "" {
				t.Errorf("rule %s is errored with no reason: the row says something is wrong and "+
					"refuses to say what", r.Name)
			}
		}
		if r.Status == StatusOK && r.Reason != "" {
			t.Errorf("rule %s is OK but carries a reason %q", r.Name, r.Reason)
		}
	}
	if len(errored) == 0 {
		t.Fatal("no rule errored though every BackupRepository read was refused; either the " +
			"predicates stopped reading repositories or a failure is being swallowed into a pass")
	}

	for name := range errored {
		want := "rules/" + name
		found := false
		for _, d := range rep.Diagnostics {
			if d.Area == want {
				found = true
				if d.Impact == "" {
					t.Errorf("diagnostic %+v does not say what is missing from the report", d)
				}
			}
		}
		if !found {
			t.Errorf("rule %s errored but produced no %q diagnostic; the gap is then visible only "+
				"to someone who reads all %d rule rows", name, want, len(rep.Rules))
		}
	}
}

// TestCRDsFallBackToDiscovery covers the expected case on a chart install: the operator's
// ClusterRole does not grant read on customresourcedefinitions, so the richer source is refused and
// the report has to say which weaker one answered.
func TestCRDsFallBackToDiscovery(t *testing.T) {
	rep, err := Collect(context.Background(), Options{
		Reader: forbiddenReader{
			Reader: emptyClient(t),
			kinds:  map[string]bool{"CustomResourceDefinitionList": true},
		},
		OperatorNamespace: operatorNS,
		Now:               fixtureNow(),
		Discovery:         fakeDiscovery{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.CRDs.Source != "discovery" {
		t.Fatalf("source = %q, want discovery", rep.CRDs.Source)
	}
	if rep.CRDs.Reason == "" {
		t.Error("the degraded source is not explained, so a reader cannot tell it is weaker")
	}
	if len(rep.CRDs.Items) != 1 || len(rep.CRDs.Items[0].Versions) == 0 {
		t.Errorf("discovery fallback found nothing: %+v", rep.CRDs)
	}
	if rep.CRDs.Items[0].StorageVersion != "" {
		t.Error("discovery cannot know the storage version and must not claim one")
	}
}

// TestThresholdsAreCarriedFromTheRuleTable: the number in the report is the rule's own, so nobody
// reading the page has to go and check whether it is still the number the alert uses.
func TestThresholdsAreCarriedFromTheRuleTable(t *testing.T) {
	rep := collectFixture(t, false)
	for _, r := range rep.Rules {
		if r.Threshold.Kind == "" {
			t.Errorf("rule %s carries no threshold kind", r.Name)
		}
		if r.Threshold.Description == "" {
			t.Errorf("rule %s carries no readable bound", r.Name)
		}
	}
	var stale ThresholdView
	for _, r := range rep.Rules {
		if r.Name == "CrystalbackupMaintenanceStalled" {
			stale = r.Threshold
		}
	}
	if stale.AgeHours != 26 {
		t.Errorf("MaintenanceStalled bound = %v h, want the table's 26 h", stale.AgeHours)
	}
}

// --- fixture --------------------------------------------------------------------------------

const (
	fixtureRunningDigest = "sha256:f9bd4ed18e7ced47c085592d97a45c6732b17bfce964ea50372c553c7c99335a"
	fixtureMoverDigest   = "sha256:b160211d9aaa6755be21f4df7eac44d81c8c8992fa61cbb36f1830b74eb9dfba"
	fixtureSyncDigest    = "sha256:aa11bb22cc33dd44ee55ff6600112233445566778899aabbccddeeff00112233"
)

func fixtureNow() time.Time { return time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC) }

func collectFixture(t *testing.T, full bool) *Report {
	t.Helper()
	return collectFixtureSalted(t, full, nil)
}

// collectFixtureSalted is collectFixture with the salt exposed: nil is the default random one, and
// a non-nil salt is what the --redaction-salt-file tests hold constant between two collections.
func collectFixtureSalted(t *testing.T, full bool, salt []byte) *Report {
	t.Helper()
	rep, err := Collect(context.Background(), Options{
		Reader:            fixtureClient(t),
		OperatorNamespace: operatorNS,
		Now:               fixtureNow(),
		Full:              full,
		RedactionSalt:     salt,
		Discovery:         fakeDiscovery{},
		DeclaredImages: map[string]string{
			"mover": "registry.acme-internal.example/crystal-backup/mover:0.6.0",
			"sync":  "registry.acme-internal.example/crystal-backup/sync:0.6.0",
		},
	})
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	return rep
}

func assertNoSecrets(t *testing.T, where, body string) {
	t.Helper()
	for _, s := range secrets {
		if strings.Contains(body, s) {
			t.Errorf("%s: the plaintext identifier %q survived the default (hashed) mode", where, s)
		}
	}
}

// fixtureClient is one deliberately unhealthy installation, built so that every section of the
// report has content and every redaction path is exercised. It uses recognisable customer-shaped
// names on purpose: the redaction tests assert against exactly those strings.
func fixtureClient(t *testing.T) client.Client {
	t.Helper()
	now := fixtureNow()
	old := metav1.NewTime(now.Add(-40 * 24 * time.Hour))
	stale := metav1.NewTime(now.Add(-30 * 24 * time.Hour))
	no := false

	objs := make([]client.Object, 0, 32)
	objs = append(objs,
		nsObj("acme-billing-prod", "acme-corp"),
		nsObj("acme-analytics", "acme-corp"),

		// The location, with everything a location carries that must not leak.
		&cbv1.ClusterBackupLocation{
			ObjectMeta: metav1.ObjectMeta{Name: "acme-offsite", CreationTimestamp: old},
			Spec: cbv1.ClusterBackupLocationSpec{
				Default:   true,
				ClusterID: "acme-eu-west-cluster",
				Mode:      "Standard",
				S3: cbv1.S3Spec{
					Endpoint:             "https://s3.acme-internal.example:9000",
					Bucket:               "acme-backups-eu",
					Prefix:               "acme-corp",
					Region:               "eu-west-1",
					CABundle:             "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----",
					CredentialsSecretRef: cbv1.LocalObjectReference{Name: "acme-s3-credentials"},
				},
				Encryption: cbv1.ClusterEncryptionSpec{
					ClusterKEKSecretRef: cbv1.LocalObjectReference{Name: "acme-s3-credentials"},
				},
			},
			Status: cbv1.ClusterBackupLocationStatus{Phase: "Ready"},
		},
		// The credentials themselves exist in the cluster and must never be read. If a future
		// field ever did read them, TestNoCredentialSurvivesEitherMode fails.
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "acme-s3-credentials", Namespace: operatorNS},
			Data: map[string][]byte{
				"accessKeyID":     []byte("AKIAIOSFODNN7EXAMPLE"),
				"secretAccessKey": []byte("wJalrXUtnFEMI/K7MDENG"),
				"resticPassword":  []byte("hunter2"),
			},
		},

		// A repository in every reportable state of disrepair.
		&cbv1.BackupRepository{
			ObjectMeta: metav1.ObjectMeta{Name: "acme-offsite", CreationTimestamp: old},
			Status: cbv1.BackupRepositoryStatus{
				Location:             cbv1.RepositoryLocationRef{Name: "acme-offsite"},
				Scope:                "Cluster",
				Initialized:          true,
				KeySlots:             []string{"platform"},
				SnapshotCount:        412,
				ApproximateSizeBytes: 987654321,
				StaleLocks:           2,
				LastCheckTime:        &stale,
				LastCheckResult:      "Failed",
				LastMaintenanceTime:  &stale,
				LastDiscoverySuccess: &no,
				LastDiscoveryTime:    &stale,
			},
		},

		// Schedules: one active and overdue, one paused for far too long.
		&cbv1.BackupSchedule{
			ObjectMeta: metav1.ObjectMeta{
				Name: "acme-nightly", Namespace: "acme-billing-prod", CreationTimestamp: old,
			},
			Spec: cbv1.BackupScheduleSpec{
				Schedule:    "0 2 * * *",
				LocationRef: cbv1.LocalObjectReference{Name: "acme-offsite"},
			},
		},
		&cbv1.ClusterBackupSchedule{
			ObjectMeta: metav1.ObjectMeta{Name: "acme-dr-copy", CreationTimestamp: old},
			Spec: cbv1.ClusterBackupScheduleSpec{
				Schedule: "0 4 * * *",
				Paused:   true,
			},
			Status: cbv1.ClusterBackupScheduleStatus{
				Phase: "Paused",
				Conditions: []metav1.Condition{{
					Type: "Ready", Status: metav1.ConditionFalse, Reason: "Paused",
					LastTransitionTime: old, Message: "paused",
				}},
			},
		},

		// A retained restore point, and a failure inside the BackupFailed window.
		backupObj("acme-billing-prod", "acme-nightly-20260620", "acme-nightly", "Completed",
			metav1.NewTime(now.Add(-40*24*time.Hour))),
		backupObj("acme-billing-prod", "acme-nightly-20260730", "acme-nightly", "Failed",
			metav1.NewTime(now.Add(-20*time.Minute))),

		// An external sync that has never completed.
		&cbv1.ClusterBackupExternalSync{
			ObjectMeta: metav1.ObjectMeta{Name: "acme-dr-copy", CreationTimestamp: old},
			Spec: cbv1.ClusterBackupExternalSyncSpec{
				SourceLocationRef:      cbv1.LocalObjectReference{Name: "acme-offsite"},
				DestinationLocationRef: cbv1.LocalObjectReference{Name: "acme-backups-eu"},
				Mode:                   "Mirror",
			},
			Status: cbv1.ClusterBackupExternalSyncStatus{Phase: "Pending", LagSnapshots: 118},
		},

		// The running pods: the operator on a mutable tag resolved to a real digest, plus one mover
		// and one sync so the three-image split is visible.
		operatorPod(now),
		moverPod("crystal-mover-acme-nightly-abc", now.Add(-5*time.Minute), fixtureMoverDigest, false),
		syncPod(now.Add(-2*time.Minute)),

		// Leak census: one in-flight Job (young) and one stranded (old).
		moverJob("crystal-mover-inflight", now.Add(-3*time.Minute)),
		moverJob("crystal-mover-stranded", now.Add(-72*time.Hour)),
		exposurePVC("crystal-clone-stranded", now.Add(-96*time.Hour)),
	)

	// A PVC piled high with snapshots, which is the one breach whose Detail names an object that
	// appears nowhere else in the report.
	bound := 21
	for i := range bound {
		objs = append(objs, volumeSnapshot("acme-billing-prod",
			fmt.Sprintf("customer-ledger-db-snap-%03d", i), "customer-ledger-db", now.Add(-time.Hour)))
	}

	return snapshotScheme(t, objs...)
}

func snapshotScheme(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := cbv1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"VolumeSnapshot", "VolumeSnapshotContent", "VolumeSnapshotClass"} {
		gv := schema.GroupVersion{Group: "snapshot.storage.k8s.io", Version: "v1"}
		s.AddKnownTypeWithName(gv.WithKind(kind), &unstructured.Unstructured{})
		s.AddKnownTypeWithName(gv.WithKind(kind+"List"), &unstructured.UnstructuredList{})
	}
	return fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
}

func emptyClient(t *testing.T) client.Client {
	t.Helper()
	return snapshotScheme(t)
}

func nsObj(name, tenant string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: name, Labels: map[string]string{apiconst.LabelTenant: tenant},
	}}
}

func backupObj(namespace, name, schedule, phase string, at metav1.Time) *cbv1.Backup {
	b := &cbv1.Backup{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: namespace, CreationTimestamp: at,
			Labels: map[string]string{
				apiconst.LabelSchedule: schedule,
				apiconst.LabelOrigin:   apiconst.OriginNamespace,
			},
		},
	}
	b.Spec.LocationRef.Name = "acme-offsite"
	b.Status.Phase = phase
	switch phase {
	case "Completed":
		b.Status.BackupTime = &at
	default:
		b.Status.Conditions = []metav1.Condition{{
			Type: "Ready", Status: metav1.ConditionFalse, Reason: "Failed",
			LastTransitionTime: at, Message: "mover failed",
		}}
	}
	return b
}

func operatorPod(now time.Time) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "crystal-backup-operator-7d9f-abcde", Namespace: operatorNS,
			CreationTimestamp: metav1.NewTime(now.Add(-6 * time.Hour)),
			Labels: map[string]string{
				"app.kubernetes.io/part-of":   "crystal-backup",
				"app.kubernetes.io/component": "operator",
				"app.kubernetes.io/version":   "0.6.0",
				"helm.sh/chart":               "crystal-backup-0.6.0",
			},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "manager",
			// A MUTABLE tag, deliberately. The digest below is what the kubelet resolved, and the
			// two have nothing to do with each other once the tag is re-pushed.
			Image: "ghcr.io/crystalbackup/operator:0.6.0",
		}}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name:    "manager",
			Ready:   true,
			ImageID: "ghcr.io/crystalbackup/operator@" + fixtureRunningDigest,
			State:   corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		}}},
	}
}

func moverPod(name string, created time.Time, digest string, sync bool) *corev1.Pod {
	labels := map[string]string{
		apiconst.LabelManagedBy: apiconst.ManagedByValue,
		apiconst.LabelPVC:       "customer-ledger-db",
	}
	if sync {
		labels["app.kubernetes.io/component"] = "sync"
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: operatorNS,
			CreationTimestamp: metav1.NewTime(created), Labels: labels,
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name:  "mover",
			Image: "registry.acme-internal.example/crystal-backup/mover:0.6.0",
		}}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name:    "mover",
			Ready:   true,
			ImageID: "registry.acme-internal.example/crystal-backup/mover@" + digest,
			State:   corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		}}},
	}
}

func syncPod(created time.Time) *corev1.Pod {
	p := moverPod("crystal-sync-acme-dr-copy-xyz", created, fixtureSyncDigest, true)
	p.Spec.Containers[0].Image = "registry.acme-internal.example/crystal-backup/sync:0.6.0"
	p.Status.ContainerStatuses[0].ImageID =
		"registry.acme-internal.example/crystal-backup/sync@" + fixtureSyncDigest
	return p
}

func moverJob(name string, created time.Time) *batchv1.Job {
	return &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: operatorNS, CreationTimestamp: metav1.NewTime(created),
		Labels: map[string]string{
			apiconst.LabelManagedBy: apiconst.ManagedByValue,
			apiconst.LabelPVC:       "customer-ledger-db",
		},
	}}
}

func exposurePVC(name string, created time.Time) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: operatorNS, CreationTimestamp: metav1.NewTime(created),
			Labels: map[string]string{
				apiconst.LabelManagedBy: apiconst.ManagedByValue,
				apiconst.LabelPVC:       "customer-ledger-db",
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
			},
		},
	}
}

func volumeSnapshot(namespace, name, pvc string, created time.Time) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshot",
	})
	u.SetNamespace(namespace)
	u.SetName(name)
	u.SetCreationTimestamp(metav1.NewTime(created))
	_ = unstructured.SetNestedField(u.Object, pvc, "spec", "source", "persistentVolumeClaimName")
	return u
}

// forbiddenReader denies specific list kinds, so the diagnostics path can be exercised without an
// API server that has been told to refuse.
type forbiddenReader struct {
	client.Reader
	kinds map[string]bool
}

func (f forbiddenReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	kind := fmt.Sprintf("%T", list)
	if i := strings.LastIndex(kind, "."); i >= 0 {
		kind = kind[i+1:]
	}
	if u, ok := list.(*unstructured.UnstructuredList); ok {
		kind = u.GetObjectKind().GroupVersionKind().Kind
	}
	if f.kinds[kind] {
		return fmt.Errorf("forbidden: cannot list %s at the cluster scope", kind)
	}
	return f.Reader.List(ctx, list, opts...)
}

type fakeDiscovery struct{}

func (fakeDiscovery) ServerVersion() (*version.Info, error) {
	return &version.Info{GitVersion: "v1.33.1"}, nil
}

func (fakeDiscovery) ServerGroups() (*metav1.APIGroupList, error) {
	return &metav1.APIGroupList{Groups: []metav1.APIGroup{{
		Name:     apiconst.Domain,
		Versions: []metav1.GroupVersionForDiscovery{{GroupVersion: apiconst.Domain + "/v1alpha1", Version: "v1alpha1"}},
	}}}, nil
}
