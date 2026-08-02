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
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A salt that is 32 bytes and is not the KAT vector, so a test that accidentally used the wrong
// one would not pass by coincidence.
var testSalt = []byte("0123456789abcdef0123456789abcdef")

// TestSuppliedSaltNeverReachesTheOutput is the guard on the one secret this whole design has.
//
// The tokens in a soak archive are HMACs under a salt the admin holds, over a value space small
// enough (`production`, `staging`, the customer's own name) that anyone holding the salt reverses
// the whole archive by dictionary in seconds. A redactor that wrote its salt into what it
// redacted would therefore produce a document that LOOKS pseudonymised and is plaintext to
// whoever receives it — which is strictly worse than not redacting, because nobody would think to
// check.
//
// Checked in three encodings, because the ways it could leak are not the same: a struct field
// marshalled by accident (raw), a hex dump in a diagnostic (hex), a base64 blob in a note.
func TestSuppliedSaltNeverReachesTheOutput(t *testing.T) {
	red, err := NewRedactorWithSalt(testSalt, false)
	if err != nil {
		t.Fatalf("NewRedactorWithSalt: %v", err)
	}
	// Exercise every path that can put bytes into a document.
	red.Namespace("production")
	red.PVC("data-postgres-0")
	red.Endpoint("https://s3.internal.example:9000")
	red.Learn(KindLocation, "primary")
	detail := red.Detail("backup of production/data-postgres-0 failed against primary")

	body, err := json.Marshal(struct {
		R Redaction `json:"redaction"`
		D string    `json:"detail"`
		// The redactor itself, marshalled: an exported salt field would show up here.
		Whole *Redactor `json:"redactor"`
	}{R: red.Describe(), D: detail, Whole: red})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	for _, enc := range []struct {
		name string
		want []byte
	}{
		{"raw", testSalt},
		{"hex", []byte(hex.EncodeToString(testSalt))},
	} {
		if bytes.Contains(body, enc.want) {
			t.Errorf("the supplied salt appears in the output in %s form: %s", enc.name, body)
		}
	}
	if red.Describe().SaltDisclosed {
		t.Error("Describe() claims the salt is disclosed; it must never be")
	}
	if !strings.Contains(detail, "ns-") {
		t.Errorf("Detail did not tokenise the namespace it was taught: %q", detail)
	}
}

// TestTokenConstructionMatchesCollectSh pins the Go redactor to the known-answer vector
// hack/soak/collect.sh proves its own backend against before it will run the token check.
//
// The two implementations have to agree BYTE FOR BYTE or the leak check in that script reports a
// perfectly redacted archive as unrecognisable — or, worse, reports a broken one as fine. The
// script hardcodes this vector for exactly that reason; this is the other half of the contract,
// and it is what would fail if anyone changed the message framing, the truncation length or the
// prefix on this side.
func TestTokenConstructionMatchesCollectSh(t *testing.T) {
	const (
		katSalt   = "crystalbackup-soak-self-test-vec" // 32 bytes
		katExpect = "ns-6756b3ef"
	)
	red, err := NewRedactorWithSalt([]byte(katSalt), false)
	if err != nil {
		t.Fatalf("NewRedactorWithSalt: %v", err)
	}
	if got := red.Namespace("production"); got != katExpect {
		t.Errorf("token for ns/production = %q, want %q — hack/soak/collect.sh will not be able to "+
			"verify any archive this binary produces", got, katExpect)
	}
}

func TestNewRedactorWithSaltRefusesAShortSalt(t *testing.T) {
	for _, tc := range []struct {
		name string
		salt []byte
		ok   bool
	}{
		{"empty", nil, false},
		{"one byte short", bytes.Repeat([]byte("a"), MinSaltBytes-1), false},
		{"exactly the minimum", bytes.Repeat([]byte("a"), MinSaltBytes), true},
		{"longer", bytes.Repeat([]byte("a"), 64), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewRedactorWithSalt(tc.salt, false)
			if tc.ok && err != nil {
				t.Fatalf("salt of %d bytes was refused: %v", len(tc.salt), err)
			}
			if !tc.ok && err == nil {
				t.Fatalf("salt of %d bytes was accepted; a guessable salt makes every token in the "+
					"document reversible by dictionary", len(tc.salt))
			}
		})
	}
}

// TestStableSaltCorrelatesAcrossRedactors is the property the soak is built on: the same
// namespace is the same token in a metric, in an event, in a log line and in day 9's report —
// which are produced by different processes on different days.
func TestStableSaltCorrelatesAcrossRedactors(t *testing.T) {
	a, err := NewRedactorWithSalt(testSalt, false)
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewRedactorWithSalt(testSalt, false)
	if err != nil {
		t.Fatal(err)
	}
	if a.Namespace("production") != b.Namespace("production") {
		t.Error("two redactors under the same salt produced different tokens: nothing in a soak " +
			"archive would cross-reference")
	}
	random, err := NewRedactor(false)
	if err != nil {
		t.Fatal(err)
	}
	if random.Namespace("production") == a.Namespace("production") {
		t.Error("a random-salt redactor produced the same token as the supplied-salt one")
	}
}

// TestScrapeLabelsAreRedacted covers §6.1: the four labels a SCRAPE adds, which no collector in
// this operator emits and which therefore had no mapping until the soak started reading /metrics
// directly.
//
// exported_namespace is the one that matters. A ServiceMonitor without honorLabels renames every
// `namespace` this operator emits to `exported_namespace`, and a redactor that knew one and not
// the other would pass tenant names through in clear on exactly the clusters where that mistake
// had already been made.
func TestScrapeLabelsAreRedacted(t *testing.T) {
	red, err := NewRedactorWithSalt(testSalt, false)
	if err != nil {
		t.Fatal(err)
	}
	out := red.Labels(map[string]string{
		labelNamespace:       "production",
		"exported_namespace": "production",
		"pod":                "crystal-backup-mover-abc",
		"instance":           "10.42.0.7:8443",
		"node":               "worker-3.internal",
		"result":             "success",
	})
	for _, label := range []string{labelNamespace, labelExportedNamespace, labelPod, labelInstance, labelNode} {
		for _, raw := range []string{"production", "crystal-backup-mover-abc", "10.42.0.7:8443", "worker-3.internal"} {
			if out[label] == raw {
				t.Errorf("label %q was passed through verbatim as %q", label, raw)
			}
		}
	}
	if out[labelNamespace] != out[labelExportedNamespace] {
		t.Errorf("namespace=%q and exported_namespace=%q got different tokens for the same value: "+
			"a series renamed by a ServiceMonitor would not correlate with one that was not",
			out[labelNamespace], out[labelExportedNamespace])
	}
	if out["result"] != "success" {
		t.Errorf("result was redacted to %q; API enums carry the MEANING of a series and must pass "+
			"through", out["result"])
	}
}

func TestReadSaltFile(t *testing.T) {
	dir := t.TempDir()
	short := filepath.Join(dir, "short.bin")
	good := filepath.Join(dir, "good.bin")
	if err := os.WriteFile(short, bytes.Repeat([]byte("a"), 16), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(good, testSalt, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		path    string
		wantLen int
		wantErr bool
	}{
		{"empty path means a random salt", "", 0, false},
		{"missing file", filepath.Join(dir, "nope.bin"), 0, true},
		{"short file", short, 0, true},
		{"good file", good, len(testSalt), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ReadSaltFile(tc.path)
			if tc.wantErr != (err != nil) {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if len(got) != tc.wantLen {
				t.Errorf("len = %d, want %d", len(got), tc.wantLen)
			}
		})
	}
}
