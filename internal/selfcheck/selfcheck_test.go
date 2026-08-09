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
	"html/template"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
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
	"acme-ledger-archive",
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

// TestADerivedSaltLeaksNothingEither runs the SAME leak assertions over the third mode.
//
// A derived salt is the soak collector's default, so it is the mode most archives will actually
// be built with — and the one whose salt has an input (a namespace UID) that a reader might
// separately be able to obtain. Neither the salt nor its input may appear anywhere in the report,
// and the report must say which mode produced it rather than inheriting a sentence about a random
// salt it does not have.
func TestADerivedSaltLeaksNothingEither(t *testing.T) {
	// Whatever soak.DeriveNamespaceSalt produces is 32 bytes; this stands in for it, because this
	// package must not import the one that derives it.
	derived := []byte("0123456789abcdef0123456789abcdef")
	rep := collectFixtureSaltedFrom(t, false, derived, SaltNamespaceUID)

	if rep.Redaction.SaltSource != SaltNamespaceUID {
		t.Errorf("saltSource = %q, want %q: a derived-salt report claiming another mode is a false "+
			"provenance line", rep.Redaction.SaltSource, SaltNamespaceUID)
	}
	body, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	assertNoSecrets(t, "JSON (derived salt)", string(body))
	if strings.Contains(string(body), string(derived)) {
		t.Error("the derived salt is in the report")
	}
	// It correlates like the supplied mode does — that is its purpose — and differs from it, so
	// two archives salted by different methods cannot be matched against each other.
	again := collectFixtureSaltedFrom(t, false, derived, SaltNamespaceUID)
	supplied := collectFixtureSalted(t, false, soakSalt)
	if tokens(rep)[0] != tokens(again)[0] {
		t.Error("two reports from the same derived salt disagree; there is no series to read")
	}
	if tokens(rep)[0] == tokens(supplied)[0] {
		t.Error("a derived salt and a supplied one produced the same token")
	}
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
	v := verdictOf(rules, noLeaks, nil)
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
	warn := verdictOf([]RuleResult{{Status: StatusBreached, Severity: "warning"}}, noLeaks, nil)
	if warn.Status != "degraded" {
		t.Errorf("a warning breach gave %q, want degraded", warn.Status)
	}
	crit := verdictOf([]RuleResult{{Status: StatusBreached, Severity: "critical"}}, noLeaks, nil)
	if crit.Status != "unhealthy" {
		t.Errorf("a critical breach gave %q, want unhealthy", crit.Status)
	}
	// The SENTENCE, not only the status word. "none critical" is what the 2026-08-09 report said over
	// a cluster with no backup for thirty-one hours (see TestMissedBackupsEscalateOutOfNoneCritical),
	// and the sentence is the part a human quotes into a ticket.
	if strings.Contains(crit.Summary, "none critical") {
		t.Errorf("the summary says \"none critical\" over a critical breach: %q", crit.Summary)
	}
	if !strings.Contains(crit.Summary, "CRITICAL") {
		t.Errorf("the summary of an unhealthy verdict does not use the word CRITICAL: %q", crit.Summary)
	}
}

// TestMissedBackupsEscalateOutOfNoneCritical is the 2026-08-09 incident, end to end through the
// real rule table.
//
// The self-check on a live cluster produced, verbatim:
//
//	verdict: degraded — "2 rule(s) breached, none critical." (breached 2, ok 10, critical 0)
//
// over an installation that had captured NOTHING for thirty-one hours. The words were accurate about
// the rule tally and wrong as an answer: an administrator reading them learns that two warnings are
// up, which is what they would have read if a nightly had been an hour late.
//
// This drives the WHOLE table over a fake cluster rather than hand-building RuleResults, because the
// verdict arithmetic was never the bug — verdictOf has escalated on Critical > 0 since it was
// written. The bug was that no rule in the table could ever be critical about missing backups, so
// the escalation was unreachable. A test over synthetic RuleResults would have passed throughout the
// incident, which is precisely why this one goes through alerts.Rules().
func TestMissedBackupsEscalateOutOfNoneCritical(t *testing.T) {
	now := fixtureNow()
	// One namespace, one location, one active nightly schedule, and one successful Backup — the whole
	// cluster, so that the ONLY rules that can breach are the two missed tiers. Anything else breaching
	// here would make the assertion below true for the wrong reason.
	cluster := func(lastSuccess time.Duration) client.Client {
		at := metav1.NewTime(now.Add(-lastSuccess))
		return snapshotScheme(t,
			nsObj("acme-billing-prod", "acme-corp"),
			&cbv1.ClusterBackupLocation{
				ObjectMeta: metav1.ObjectMeta{
					Name: "acme-offsite", CreationTimestamp: metav1.NewTime(now.Add(-90 * 24 * time.Hour)),
				},
				Spec: cbv1.ClusterBackupLocationSpec{Default: true, ClusterID: "acme-eu-west-cluster"},
			},
			&cbv1.BackupSchedule{
				ObjectMeta: metav1.ObjectMeta{
					Name: "acme-nightly", Namespace: "acme-billing-prod",
					CreationTimestamp: metav1.NewTime(now.Add(-90 * 24 * time.Hour)),
				},
				Spec: cbv1.BackupScheduleSpec{
					Schedule:    "0 2 * * *",
					LocationRef: cbv1.LocalObjectReference{Name: "acme-offsite"},
				},
			},
			backupObj("acme-billing-prod", "acme-nightly-last", "acme-nightly", "Completed", at),
		)
	}
	verdict := func(t *testing.T, lastSuccess time.Duration) (Verdict, map[string]string) {
		t.Helper()
		rep, err := Collect(context.Background(), Options{
			Reader:            cluster(lastSuccess),
			OperatorNamespace: operatorNS,
			Now:               now,
			Discovery:         fakeDiscovery{},
		})
		if err != nil {
			t.Fatalf("collect: %v", err)
		}
		byRule := map[string]string{}
		for _, r := range rep.Rules {
			byRule[r.Name] = r.Status
		}
		return rep.Verdict, byRule
	}

	// THE INCIDENT: 111735 seconds. One missed nightly period. Still degraded, still "none critical",
	// and that is now the CORRECT answer for one missed run — the report is allowed to say it, and
	// this case is here so the fix cannot be "call everything critical".
	v, rules := verdict(t, 111735*time.Second)
	if v.Status != verdictDegraded || v.Critical != 0 {
		t.Errorf("one missed nightly period gave %q with critical=%d, want degraded/0: escalating "+
			"here would make critical mean nothing", v.Status, v.Critical)
	}
	if rules["CrystalbackupBackupMissed"] != StatusBreached {
		t.Fatalf("the warning tier did not breach at 31h (%q); the fixture, not the rule, is wrong",
			rules["CrystalbackupBackupMissed"])
	}

	// FOUR DAYS: three of the schedule's own periods have gone by with nothing captured.
	v, rules = verdict(t, 4*24*time.Hour)
	if rules["CrystalbackupBackupMissedCritical"] != StatusBreached {
		t.Fatalf("CrystalbackupBackupMissedCritical is %q after four days with no backup — the "+
			"escalation is unreachable and the verdict can never leave degraded",
			rules["CrystalbackupBackupMissedCritical"])
	}
	if v.Critical == 0 {
		t.Errorf("critical=%d with a critical rule breached: the tally the reader judges severity by "+
			"is the one that stayed at zero all through the incident", v.Critical)
	}
	if v.Status != verdictUnhealthy {
		t.Errorf("status = %q, want %q: four days of nothing is not the same news as being an hour late",
			v.Status, verdictUnhealthy)
	}
	if strings.Contains(v.Summary, "none critical") {
		t.Errorf("the headline still reads \"none critical\" over four days with no backup — this is "+
			"the exact sentence the incident produced: %q", v.Summary)
	}
	if !strings.Contains(v.Summary, "CRITICAL") {
		t.Errorf("the headline does not say CRITICAL: %q", v.Summary)
	}
}

// TestResidualLeaksStopTheVerdictReadingHealthy is the 2026-08-07 defect, pinned.
//
// That report said `status: "healthy"` and "No rule breached among the 12 evaluated." over a
// leakIndicators section carrying seven residual VolumeSnapshots, the oldest 65 hours old. Both
// halves were true and the top line was the wrong answer to the question being asked, because a
// reader who sees "healthy" does not scroll to find out what it was a verdict ON.
func TestResidualLeaksStopTheVerdictReadingHealthy(t *testing.T) {
	clean := []RuleResult{
		{Name: "a", Status: StatusOK},
		{Name: "b", Status: StatusOK},
	}
	incident := Leaks{
		GraceMinutes: 30,
		Kinds:        []LeakKind{{Kind: "VolumeSnapshot", Total: 9, Residual: 7, OldestAgeHours: 65}},
		Totals:       LeakSummary{Residual: 7},
	}

	v := verdictOf(clean, incident, nil)
	if v.Status == verdictHealthy {
		t.Error(`status is the bare word "healthy" beside 7 residual objects — this is the exact ` +
			"report that sent somebody looking for the real state by hand")
	}
	if v.Status != verdictFindings {
		t.Errorf("status = %q, want %q: a residual is a finding, and giving it degraded or unhealthy "+
			"would claim a rule breach that never happened", v.Status, verdictFindings)
	}
	// One sentence, both facts. A reader who reads only the summary must come away knowing the rules
	// are clean AND that there is residue, with enough of a number to judge it.
	for _, want := range []string{"No rule breached", "7 managed object(s)", "VolumeSnapshot", "65 h"} {
		if !strings.Contains(v.Summary, want) {
			t.Errorf("the summary does not state %q: %q", want, v.Summary)
		}
	}
	if !strings.Contains(v.Summary, "not a breached rule") {
		t.Errorf("the summary does not say the residue is NOT a breached rule, which is how it ends "+
			"up quoted into a ticket as an alert that fired: %q", v.Summary)
	}

	// And with nothing residual, the plain word is still available: a report that could never say
	// "healthy" would be as useless as one that always did.
	quiet := verdictOf(clean, noLeaks, nil)
	if quiet.Status != verdictHealthy {
		t.Errorf("status = %q with a zero residual, want %q", quiet.Status, verdictHealthy)
	}
	if strings.Contains(quiet.Summary, "residual") || strings.Contains(quiet.Summary, "outlived") {
		t.Errorf("the summary invents a leak clause with nothing to report: %q", quiet.Summary)
	}
}

// TestTheRuleTallyIgnoresTheLeakResidual is the other half of the fix, and the one that would break
// silently. The framing had to change; the ARITHMETIC must not. A residual that added itself to
// `breached` would put a fired alert in a report the alerting side never sent, and would move the
// number the operator is reading to decide how bad this is.
func TestTheRuleTallyIgnoresTheLeakResidual(t *testing.T) {
	rules := []RuleResult{
		{Name: "a", Status: StatusOK},
		{Name: "b", Status: StatusBreached, Severity: "warning"},
		{Name: "c", Status: StatusNotEvaluated},
		{Name: "d", Status: StatusError},
	}
	leaky := Leaks{
		GraceMinutes: 30,
		Kinds: []LeakKind{
			{Kind: "Job", Total: 4, Residual: 2, OldestAgeHours: 9},
			{Kind: "VolumeSnapshotContent", Total: 3, Residual: 3, OldestAgeHours: 71},
		},
		Totals: LeakSummary{Residual: 5},
	}

	without := verdictOf(rules, noLeaks, nil)
	with := verdictOf(rules, leaky, nil)
	if with.OK != without.OK || with.Breached != without.Breached ||
		with.Critical != without.Critical || with.NotEvaluated != without.NotEvaluated ||
		with.Errored != without.Errored {
		t.Errorf("the residual moved the rule tally: %+v vs %+v", with, without)
	}
	// The word stays "degraded": a breached rule is strictly worse news than residue, and the residue
	// does not get to upgrade or downgrade it.
	if with.Status != verdictDegraded {
		t.Errorf("status = %q, want %q — the breach decides the word", with.Status, verdictDegraded)
	}
	// It is still stated, because the reader who has just been told about a breach is the one about
	// to go looking for what else is wrong.
	for _, want := range []string{"5 managed object(s)", "2 Job", "3 VolumeSnapshotContent", "71 h"} {
		if !strings.Contains(with.Summary, want) {
			t.Errorf("a degraded verdict drops the residue from its summary (%q missing): %q",
				want, with.Summary)
		}
	}
}

// TestTheVerdictOnTheFixtureAccountsForItsResidue runs the same property through the real collector
// rather than a hand-built Verdict, because the wiring is where this would be lost: Collect has to
// pass the census it just took to the verdict, and it computes the two in separate passes.
func TestTheVerdictOnTheFixtureAccountsForItsResidue(t *testing.T) {
	rep := collectFixture(t, false)
	if rep.Leaks.Totals.Residual == 0 {
		t.Fatal("the fixture no longer carries residual objects; this test can no longer see anything")
	}
	if rep.Verdict.Status == verdictHealthy {
		t.Errorf("the fixture reports %q with %d residual object(s)",
			rep.Verdict.Status, rep.Leaks.Totals.Residual)
	}
	if !strings.Contains(rep.Verdict.Summary,
		fmt.Sprintf("%d managed object(s)", rep.Leaks.Totals.Residual)) {
		t.Errorf("the summary does not carry the residual count: %q", rep.Verdict.Summary)
	}
	// And onto the page, inside the verdict box, so the reader who does not scroll sees it there.
	page, err := Render(rep)
	if err != nil {
		t.Fatal(err)
	}
	head, _, ok := strings.Cut(string(page), "<h2>Installation</h2>")
	if !ok {
		t.Fatal("the rendered page has no Installation heading to cut at")
	}
	if !strings.Contains(head, "past the orphan reaper's grace period") {
		t.Error("the residue is not mentioned above the fold; it is only in the section a reader " +
			"who trusts the headline never reaches")
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
		// The two sections this release added. They are asserted here, on the ROUND-TRIPPED report,
		// because that is the only path that proves they survive the JSON: both are pointers, and a
		// pointer section that failed to decode would render as absent rather than as an error.
		//
		// Matched WITH the opening <h2>, not on the words alone: the verdict block cross-references the
		// census section by name, so a bare phrase match is satisfied by a page that has the reference
		// and not the section — which is exactly the state a broken template would leave.
		"<h2>What the CRs will do",
		"<h2>What will and will not be backed up",
		// ESCAPED, through the same function the renderer uses. The summary is data — it carries an
		// apostrophe ("the reaper's grace") — and a page that contained it byte-for-byte would be a
		// page that had not escaped it, which is the bug this assertion would otherwise hide.
		template.HTMLEscapeString(parsed.Verdict.Summary),
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
//
// The connector handed in FAILS THE TEST if it is called. That is the assertion — this mode has to
// keep working on a maintainer's laptop that has never seen the cluster, and a --from path which
// merely tolerated a client would break that the day it started building one first.
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
	code := runReport(context.Background(), []string{"--from", in, "--output", out},
		&stdout, &stderr, forbiddenConnector(t))
	if code != 0 {
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

// TestReportWithNoFromCollectsTheSelfCheckItself is the 0.6.4 change, and the incident behind it is
// worth restating: on 2026-08-07 the only way to see the state of an installation was to run
// `selfcheck`, capture 17 KB of JSON and read it by hand, because the command that knows how to
// format that document demanded a file nobody had. Both halves are in one binary with one RBAC.
func TestReportWithNoFromCollectsTheSelfCheckItself(t *testing.T) {
	out := filepath.Join(t.TempDir(), "report.html")
	var stdout, stderr bytes.Buffer
	code := runReport(context.Background(), []string{"--output", out}, &stdout, &stderr, fixtureConnector(t))
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	page, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	html := string(page)
	// A whole report, not a stub: the same page `report --from` produces, because it came through
	// the same Collect and the same Render.
	for _, want := range []string{"Installation self-check", "Rule verdicts", "Leak indicators"} {
		if !strings.Contains(html, want) {
			t.Errorf("the self-generated page is missing %q", want)
		}
	}
	// Redaction defaults exactly as `selfcheck` does, which is the property that makes this safe to
	// suggest to somebody who is about to attach the output to an issue.
	assertNoSecrets(t, "self-generated page", html)
	if !strings.Contains(html, "Identifiers are redacted") {
		t.Error("the self-generated page does not state that it is redacted")
	}
	if !strings.Contains(stderr.String(), "re-run with --full") {
		t.Errorf("nothing on the terminal says what was done to the identifiers: %q", stderr.String())
	}

	// --full reaches the collection through the same flag it does on `selfcheck`; a flag that parsed
	// and then did nothing would be the worst of the three possible outcomes.
	full := filepath.Join(t.TempDir(), "full.html")
	stdout.Reset()
	stderr.Reset()
	code = runReport(context.Background(), []string{"--full", "--output", full},
		&stdout, &stderr, fixtureConnector(t))
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	body, err := os.ReadFile(full)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), operatorNS) {
		t.Errorf("--full produced a page with no verbatim identifier in it: the flag parsed and "+
			"changed nothing, which is exactly how somebody ends up reading tokens they believe "+
			"are names (looking for %q)", operatorNS)
	}
}

// TestReportWithNoClusterSaysWhichModeItIsIn. This is the one subcommand with a mode that needs a
// cluster and a mode that pointedly does not, so "no kubeconfig" on its own is a true message and
// no help at all: the answer is four characters the reader did not type.
func TestReportWithNoClusterSaysWhichModeItIsIn(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runReport(context.Background(), nil, &stdout, &stderr,
		func() (client.Reader, ServerInfo, error) {
			return nil, nil, fmt.Errorf("no Kubernetes configuration: no kubeconfig and no service account")
		})
	if code == 0 {
		t.Error("report exited 0 having produced nothing")
	}
	if stdout.Len() > 0 {
		t.Errorf("something was written to stdout: %q", stdout.String())
	}
	for _, want := range []string{"no Kubernetes configuration", "--from", "COLLECT"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("the error does not mention %q, so it does not say which mode failed: %q",
				want, stderr.String())
		}
	}
}

// TestReportRefusesCollectionFlagsInFromMode. A file handed to --from was collected somewhere else,
// at another instant, with the redaction whoever ran it chose — so --full cannot do anything to it.
// Ignoring the flag would print a fully redacted page to somebody who typed --full and believed they
// were looking at real names, which is worse than either mode failing.
func TestReportRefusesCollectionFlagsInFromMode(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "health.json")
	body, err := json.Marshal(collectFixture(t, false))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(in, body, 0o600); err != nil {
		t.Fatal(err)
	}

	for _, arg := range []string{"--full", "--operator-namespace=other", "--mover-image=x:1"} {
		var stdout, stderr bytes.Buffer
		code := runReport(context.Background(), []string{"--from", in, arg},
			&stdout, &stderr, forbiddenConnector(t))
		if code != 2 {
			t.Errorf("%s alongside --from exited %d, want 2 (usage error)", arg, code)
		}
		if stdout.Len() > 0 {
			t.Errorf("%s alongside --from still produced a page: %q", arg, stdout.String())
		}
		if !strings.Contains(stderr.String(), "ALREADY collected") {
			t.Errorf("the refusal for %s does not explain why the flag cannot apply: %q",
				arg, stderr.String())
		}
	}

	// The namespace default must NOT trip this: it comes from $POD_NAMESPACE, so a `report --from`
	// run inside the operator pod would otherwise be refused for a flag nobody typed.
	t.Setenv("POD_NAMESPACE", "crystal-backup-elsewhere")
	var stdout, stderr bytes.Buffer
	if code := runReport(context.Background(), []string{"--from", in},
		&stdout, &stderr, forbiddenConnector(t)); code != 0 {
		t.Errorf("--from alone exited %d inside a pod namespace: %s", code, stderr.String())
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

// noLeaks is the census of an installation with nothing stranded: the state in which the verdict is
// allowed to say the bare word "healthy". Named because most verdict tests are about the rules and
// should not have to construct one.
var noLeaks = Leaks{GraceMinutes: 30}

// fixtureConnector hands the collecting paths the fixture cluster instead of a real one, which is
// what lets `report` with no --from be tested at all.
func fixtureConnector(t *testing.T) clusterConnector {
	t.Helper()
	return func() (client.Reader, ServerInfo, error) {
		return fixtureClient(t), fakeDiscovery{}, nil
	}
}

// forbiddenConnector fails the test if a cluster is opened. It is the whole assertion behind
// `report --from`: that mode has to work on a machine which has never seen the cluster, and the way
// that property dies is quietly, on the day the file path starts building a client "just in case".
func forbiddenConnector(t *testing.T) clusterConnector {
	t.Helper()
	return func() (client.Reader, ServerInfo, error) {
		t.Error("a cluster connection was opened on a path that must never need one")
		return nil, nil, fmt.Errorf("no cluster available")
	}
}

func collectFixture(t *testing.T, full bool) *Report {
	t.Helper()
	return collectFixtureSalted(t, full, nil)
}

// collectFixtureSalted is collectFixture with the salt exposed: nil is the default random one, and
// a non-nil salt is what the --redaction-salt-file tests hold constant between two collections.
// collectFixtureSalted passes NO source, exactly as `crystal-backup selfcheck
// --redaction-salt-file` does. The default has to be caller-supplied, because that is what the
// flag means; a default that drifted to another mode would put a false provenance line on every
// report taken through that path.
func collectFixtureSalted(t *testing.T, full bool, salt []byte) *Report {
	t.Helper()
	return collectFixtureSaltedFrom(t, full, salt, "")
}

// collectFixtureSaltedFrom is collectFixtureSalted with the salt's PROVENANCE stated, so the
// leak assertions can be run over every mode rather than only the one that existed first.
func collectFixtureSaltedFrom(t *testing.T, full bool, salt []byte, source string) *Report {
	t.Helper()
	rep, err := Collect(context.Background(), Options{
		Reader:              fixtureClient(t),
		OperatorNamespace:   operatorNS,
		Now:                 fixtureNow(),
		Full:                full,
		RedactionSalt:       salt,
		RedactionSaltSource: source,
		Discovery:           fakeDiscovery{},
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

	// The PVC census. Two tenant volumes, deliberately on opposite sides of the verdict: the ledger
	// is selected by the nightly BackupSchedule and snapshots fine, the archive is in a namespace no
	// schedule reaches and sits on storage that cannot snapshot at all. Both are here so the coverage
	// section has content whose identifiers the redaction tests can hold to account — a PVC name and
	// a namespace name appear in that section's rows AND inside a resolver sentence, which is the
	// shape per-field redaction cannot reach.
	objs = append(objs,
		&storagev1.StorageClass{
			ObjectMeta:  metav1.ObjectMeta{Name: "ceph-block"},
			Provisioner: fixtureRBDDriver,
		},
		fixtureSnapClass("csi-rbdplugin-snapclass", fixtureRBDDriver),
		tenantPVC("acme-billing-prod", "customer-ledger-db", "ceph-block", "pv-ledger"),
		fixtureCSIPV("pv-ledger", fixtureRBDDriver, "ceph-block"),
		tenantPVC("acme-analytics", "acme-ledger-archive", "local-path", "pv-archive"),
		fixtureCSIPV("pv-archive", "rancher.io/local-path", "local-path"),
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

// fixtureRBDDriver is the CSI driver the fixture's healthy volume is served by. A real name, because
// the coverage section quotes the resolver's own sentences and those name the driver.
const fixtureRBDDriver = "rook-ceph.rbd.csi.ceph.com"

// tenantPVC is one user volume, bound to volumeName and requesting a plausible size.
func tenantPVC(namespace, name, storageClass, volumeName string) *corev1.PersistentVolumeClaim {
	sc := storageClass
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: corev1.PersistentVolumeClaimSpec{
			StorageClassName: &sc,
			VolumeName:       volumeName,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("20Gi")},
			},
		},
	}
}

func fixtureCSIPV(name, driver, storageClass string) *corev1.PersistentVolume {
	return &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: corev1.PersistentVolumeSpec{
			StorageClassName: storageClass,
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{Driver: driver},
			},
		},
	}
}

// fixtureSnapClass declares no snapshotter Secret, which exposer.Precheck treats as a positive
// statement that this driver authenticates some other way — an OK, not an unverified guess. That keeps
// the fixture's healthy volume healthy without seeding a Secret this report must never read.
func fixtureSnapClass(name, driver string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshotClass",
	})
	u.SetName(name)
	if err := unstructured.SetNestedField(u.Object, driver, "driver"); err != nil {
		panic(err)
	}
	return u
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
