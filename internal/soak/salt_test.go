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
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/CrystalBackup/CrystalBackup/internal/selfcheck"
)

// uidOf is the namespace reader ResolveSalt takes, wired to a constant.
func uidOf(uid string) func() (string, error) {
	return func() (string, error) { return uid, nil }
}

func uidFails(msg string) func() (string, error) {
	return func() (string, error) { return "", errors.New(msg) }
}

func writeSalt(t *testing.T, n int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "salt")
	if err := os.WriteFile(path, bytes.Repeat([]byte{7}, n), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestThreeMethodsThreeSaltSources. The three ways a report can be salted make three DIFFERENT
// promises, and a reader deciding whether a file is safe to attach to a public issue is deciding
// on that one field. Two of them sharing a value would be a false provenance line.
func TestThreeMethodsThreeSaltSources(t *testing.T) {
	auto, source, err := ResolveSalt(SaltMethodAuto, "", uidOf("f2e1d0c9-4b3a-4e5f-8a7b-6c5d4e3f2a1b"))
	if err != nil {
		t.Fatalf("auto: %v", err)
	}
	if source != selfcheck.SaltNamespaceUID {
		t.Errorf("auto saltSource = %q, want %q", source, selfcheck.SaltNamespaceUID)
	}

	fixed, source, err := ResolveSalt(SaltMethodFromSecret, writeSalt(t, 32), uidOf("unused"))
	if err != nil {
		t.Fatalf("from-secret: %v", err)
	}
	if source != selfcheck.SaltCallerSupplied {
		t.Errorf("from-secret saltSource = %q, want %q", source, selfcheck.SaltCallerSupplied)
	}
	if bytes.Equal(auto, fixed) {
		t.Error("the two methods produced the same salt")
	}

	// And the third: no salt at all is what a one-shot selfcheck does, and it must still describe
	// itself as random-per-report rather than inheriting either of the above.
	red, err := selfcheck.NewRedactor(false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := red.Describe().SaltSource; got != selfcheck.SaltRandomPerReport {
		t.Errorf("no salt describes itself as %q, want %q", got, selfcheck.SaltRandomPerReport)
	}

	// Three sources, three notes, each stating its own guarantee.
	notes := map[string]string{}
	for _, tc := range []struct {
		source string
		salt   []byte
	}{
		{selfcheck.SaltNamespaceUID, auto},
		{selfcheck.SaltCallerSupplied, fixed},
		{selfcheck.SaltRandomPerReport, nil},
	} {
		r, err := selfcheck.NewRedactorWithSource(false, tc.salt, tc.source)
		if err != nil {
			t.Fatalf("%s: %v", tc.source, err)
		}
		d := r.Describe()
		if len(tc.salt) > 0 && d.SaltSource != tc.source {
			t.Errorf("saltSource = %q, want %q", d.SaltSource, tc.source)
		}
		if prev, seen := notes[d.Note]; seen {
			t.Errorf("%q and %q share one note, so the two guarantees read the same", tc.source, prev)
		}
		notes[d.Note] = tc.source
	}
}

// TestTheDerivedNoteSaysWhoCanReverseIt is the honesty requirement, and it is the part that
// matters more than the derivation.
//
// A derived salt is a stable pseudonym against a stranger and NOTHING against anyone who can read
// the namespace — namespace names come from a small guessable set, so the tokens fall to a
// dictionary in seconds. The note is written for somebody about to paste the file somewhere.
func TestTheDerivedNoteSaysWhoCanReverseIt(t *testing.T) {
	salt, _, err := ResolveSalt(SaltMethodAuto, "", uidOf("f2e1d0c9-4b3a-4e5f-8a7b-6c5d4e3f2a1b"))
	if err != nil {
		t.Fatal(err)
	}
	red, err := selfcheck.NewRedactorWithSource(false, salt, selfcheck.SaltNamespaceUID)
	if err != nil {
		t.Fatal(err)
	}
	d := red.Describe()
	for _, want := range []string{
		"DERIVED FROM THIS CLUSTER",
		"REVERSIBLE BY DICTIONARY",
		"`get` THE OPERATOR'S NAMESPACE",
		"re-run it WITHOUT a fixed salt",
	} {
		if !strings.Contains(d.Note, want) {
			t.Errorf("the note is missing %q:\n%s", want, d.Note)
		}
	}
	if d.SaltDisclosed {
		t.Error("saltDisclosed = true")
	}
	// The algorithm line names the derivation, so a reader can reason about it without trusting
	// the one-word source.
	if !strings.Contains(d.Algorithm, selfcheck.SaltNamespaceUIDDomain) {
		t.Errorf("the algorithm line does not name the domain separator: %q", d.Algorithm)
	}
}

// TestTheDerivedSaltIsStableAndPerCluster: stable across runs in the same namespace — which is the
// entire point, a fortnight read as one series — and different across two clusters, which is what
// keeps two archives from correlating with each other.
func TestTheDerivedSaltIsStableAndPerCluster(t *testing.T) {
	const uidA = "3b8f1c2d-9e4a-4f60-8b71-2c3d4e5f6a7b"
	const uidB = "7a1e5d3c-2b9f-4081-9c62-1d2e3f4a5b6c"

	first, _, err := ResolveSalt(SaltMethodAuto, "", uidOf(uidA))
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := ResolveSalt(SaltMethodAuto, "", uidOf(uidA))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Error("two runs in the same namespace derived different salts; the correlation a soak " +
			"exists for would break every time the collector restarted")
	}
	other, _, err := ResolveSalt(SaltMethodAuto, "", uidOf(uidB))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(first, other) {
		t.Error("two different namespace UIDs derived the same salt")
	}
	// 32 bytes exactly, by construction rather than by luck — the floor is met by SHA256's output
	// width and not by the length a UUID happens to have.
	if len(first) != selfcheck.MinSaltBytes {
		t.Errorf("derived salt is %d bytes, want %d", len(first), selfcheck.MinSaltBytes)
	}
	// And it is not the UID itself, nor anything containing it.
	if strings.Contains(string(first), uidA) {
		t.Error("the derived salt carries the UID verbatim")
	}
	// A GOLDEN value, and it is not ceremony. Every archive ever produced under `auto` stays
	// readable only while this construction is identical: change the domain separator, the
	// concatenation order or the hash, and yesterday's tokens stop matching today's — silently,
	// for everyone. If this line ever has to change, the domain separator's "-v1" changes with it.
	const want = "287679b34879a062d163dcbf4d07eabd8c31221a6bcf47a2374bc77c9734766c"
	if got := hex.EncodeToString(first); got != want {
		t.Errorf("the derivation changed:\n got %s\nwant %s\nSHA256(%q || uid) is the pinned "+
			"construction; a new one needs a new domain separator, not a new hash under the old name",
			got, want, selfcheck.SaltNamespaceUIDDomain)
	}
}

// TestDerivingFromNothingIsRefused. An unreadable namespace yields an empty UID, and hashing that
// would produce a valid-looking salt that is IDENTICAL on every cluster where the read failed —
// silently making unrelated archives correlate with one another.
func TestDerivingFromNothingIsRefused(t *testing.T) {
	for _, uid := range []string{"", "   ", "\n"} {
		if _, err := DeriveNamespaceSalt(uid); err == nil {
			t.Errorf("DeriveNamespaceSalt(%q) succeeded", uid)
		}
	}
}

// TestNoSilentFallbackBetweenMethods is the rule this whole file is organised around: a report
// that claims one guarantee and holds another is worse than one that refuses to exist.
func TestNoSilentFallbackBetweenMethods(t *testing.T) {
	for _, tc := range []struct {
		name     string
		method   string
		saltFile string
		uid      func() (string, error)
		wantSaid string
	}{
		{
			"from-secret with a Secret that is not there",
			SaltMethodFromSecret, "/nonexistent/salt", uidOf("valid-uid"),
			"read redaction salt file",
		},
		{
			"from-secret with a Secret that is too short",
			SaltMethodFromSecret, writeSalt(t, 8), uidOf("valid-uid"),
			"need at least 32",
		},
		{
			"from-secret with no Secret named",
			SaltMethodFromSecret, "", uidOf("valid-uid"),
			"needs --redaction-salt-file",
		},
		{
			"auto when the namespace cannot be read",
			SaltMethodAuto, "", uidFails("namespaces \"x\" is forbidden"),
			"forbidden",
		},
		{
			"a method nobody defined",
			"random", "", uidOf("valid-uid"),
			`valid values are "auto" and "from-secret"`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			salt, source, err := ResolveSalt(tc.method, tc.saltFile, tc.uid)
			if err == nil {
				t.Fatalf("ResolveSalt succeeded with source %q and %d bytes; it must refuse rather "+
					"than fall back to another method", source, len(salt))
			}
			if salt != nil || source != "" {
				t.Errorf("a refusal still returned salt=%d bytes source=%q", len(salt), source)
			}
			if !strings.Contains(err.Error(), tc.wantSaid) {
				t.Errorf("the refusal does not say %q:\n%v", tc.wantSaid, err)
			}
		})
	}
}

// TestTheSaltNeverReachesTheOutput, for the DERIVED path as well as the supplied one. The note
// says "the salt is not in this file in any form", and that sentence has to be true of a salt
// whose input is a UID the reader might separately be able to obtain.
func TestTheSaltNeverReachesTheOutput(t *testing.T) {
	const uid = "3b8f1c2d-9e4a-4f60-8b71-2c3d4e5f6a7b"
	salt, source, err := ResolveSalt(SaltMethodAuto, "", uidOf(uid))
	if err != nil {
		t.Fatal(err)
	}
	red, err := selfcheck.NewRedactorWithSource(false, salt, source)
	if err != nil {
		t.Fatal(err)
	}
	// Tokenise something, so the report carries real output of the salted construction.
	token := red.Namespace("production")
	rendered, err := json.Marshal(struct {
		Redaction selfcheck.Redaction `json:"redaction"`
		Token     string              `json:"token"`
	}{red.Describe(), token})
	if err != nil {
		t.Fatal(err)
	}
	out := string(rendered)
	if strings.Contains(out, uid) {
		t.Errorf("the namespace UID — the salt's only input — is in the output:\n%s", out)
	}
	if strings.Contains(out, string(salt)) {
		t.Error("the salt bytes are in the output")
	}
	// The identifier itself is gone from the TOKEN. It is deliberately not asserted over the whole
	// document: the note names `production` as an example of the guessable set that makes a
	// derived salt reversible, and that sentence is the warning, not a leak.
	if token == "" || !strings.HasPrefix(token, "ns-") || strings.Contains(token, "production") {
		t.Errorf("token = %q, want a kind-prefixed token with no identifier in it", token)
	}
}

// TestCheckSaltFlagsRefusesAnImplicitChoice: a --redaction-salt-file under the derived method
// would be silently ignored, and a running collector would look exactly like one using it.
func TestCheckSaltFlagsRefusesAnImplicitChoice(t *testing.T) {
	if err := checkSaltFlags(SaltMethodAuto, "/etc/crystal-backup/soak/salt"); err == nil {
		t.Fatal("a salt file under --salt-method=auto was accepted; the file would be ignored and " +
			"nothing about the collector would show it")
	} else if !strings.Contains(err.Error(), "would IGNORE that file") {
		t.Errorf("the refusal does not say the file is ignored: %v", err)
	}
	// The two coherent configurations are accepted.
	if err := checkSaltFlags(SaltMethodAuto, ""); err != nil {
		t.Errorf("the default configuration was refused: %v", err)
	}
	if err := checkSaltFlags(SaltMethodFromSecret, "/etc/crystal-backup/soak/salt"); err != nil {
		t.Errorf("fromSecret with a file was refused: %v", err)
	}
}
