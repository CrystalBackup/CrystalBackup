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

package preflight

// This package drives website/public/preflight.sh — the read-only script an administrator runs
// BEFORE installing anything — against fixed cluster output, with no cluster anywhere near it.
//
// # Why a Go test around a shell script
//
// preflight.sh is the artefact with the widest reach and the least coverage in this repository: it is
// published, signed, curl-piped into strangers' shells, and it is the thing an administrator forms
// their belief about their cluster from. `make preflight-table-verify` proves its GENERATED exposer
// rule agrees with internal/exposer; nothing proved anything at all about the checks written by hand
// around it. This is that.
//
// # How it works: the script is SOURCED, not executed
//
// The script's every cluster call goes through one function — k_get, which sets K_OUT and K_ERR and
// returns non-zero when the call failed. So the harness reads the script up to (and not including) its
// `--- main ---` section, appends a k_get STUB that answers from a fixture table, and then calls one
// check function and prints the record it produced. Nothing forks kubectl, nothing needs a cluster,
// and the check under test is the real one, byte for byte, including its generated region.
//
// Cutting at `--- main ---` is what makes that possible: everything below that marker is argument
// parsing and the report, and everything above it is definitions. A test that ran the whole script
// would run every check against a stub that answers for one.

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// scriptPath is the published script itself, not a copy. A test that asserted against a fixture copy
// of a shell script would pass forever after the published one changed.
func scriptPath(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs("../../website/public/preflight.sh")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// mainMarker is the line the harness truncates at. Asserted to exist (see TestHarnessCutsAtMain)
// rather than assumed, because a silently-missing marker would mean sourcing the whole script — which
// would run every check against the stub and produce confident nonsense.
const mainMarker = "# --- main ---"

// stubbedRun sources the script's definitions, installs a k_get stub over the fixture, calls one
// check function, and returns the RESULTS record it appended.
//
// The fixture maps a substring of the k_get argument list onto the stdout that call should produce.
// A value of the sentinel errValue makes the call FAIL with that message on K_ERR, which is how the
// "the cluster refused the listing" paths are reached — the paths where this script must degrade to
// UNKNOWN instead of concluding anything.
func stubbedRun(t *testing.T, fn string, fixture map[string]string) []record {
	t.Helper()
	src := readDefinitions(t)

	var stub strings.Builder
	stub.WriteString("\nk_get() {\n  _args=\"$*\"\n  K_ERR=''\n  K_OUT=''\n  case \"$_args\" in\n")
	for match, out := range fixture {
		// The literal part is DOUBLE-QUOTED inside the glob: a `case` pattern is parsed as a shell
		// word, so an unquoted fragment containing a space ("volumesnapshot -A") is two words and a
		// syntax error.
		fmt.Fprintf(&stub, "  *\"%s\"*)\n", shellCaseQuote(match))
		if strings.HasPrefix(out, errValue) {
			fmt.Fprintf(&stub, "    K_ERR=%s\n    return 1\n    ;;\n",
				shellSingleQuote(strings.TrimPrefix(out, errValue)))
			continue
		}
		fmt.Fprintf(&stub, "    K_OUT=%s\n    return 0\n    ;;\n", shellSingleQuote(out))
	}
	// An unmatched call is a FIXTURE BUG and says so. Returning empty success instead would let a
	// check silently read an empty cluster and still be asserted against, which is the failure mode a
	// stub exists to prevent.
	stub.WriteString("  *)\n    printf 'FIXTURE MISS: %s\\n' \"$_args\" >&2\n    return 1\n    ;;\n  esac\n}\n")

	// The record dump: one line per finding, US replaced by a separator a Go test can split on
	// without inventing a second encoding of the script's own.
	stub.WriteString(fn + "\n")
	stub.WriteString("printf '%s' \"$RESULTS\" | tr \"$US\" '|'\n")

	cmd := exec.Command("sh", "-s")
	cmd.Stdin = strings.NewReader(src + stub.String())
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("running %s: %v\nstderr: %s", fn, err, errb.String())
	}
	if strings.Contains(errb.String(), "FIXTURE MISS") {
		t.Fatalf("%s made a cluster call the fixture does not answer: %s", fn, errb.String())
	}
	return parseRecords(t, out.String())
}

// errValue prefixes a fixture value that must make k_get FAIL, carrying the rest as the message the
// cluster would have printed on stderr.
const errValue = "\x00ERR:"

func readDefinitions(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(scriptPath(t))
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	i := strings.Index(body, mainMarker)
	if i < 0 {
		t.Fatalf("preflight.sh no longer contains %q: this harness would source the whole script, "+
			"running every check against a stub written for one", mainMarker)
	}
	return body[:i]
}

// record is one `record ID STATUS TITLE DETAIL` the script appended.
type record struct {
	ID     string
	Status string
	Title  string
	Detail string
}

func parseRecords(t *testing.T, out string) []record {
	t.Helper()
	var recs []record
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		f := strings.SplitN(line, "|", 4)
		if len(f) != 4 {
			t.Fatalf("unparseable record %q", line)
		}
		recs = append(recs, record{ID: f[0], Status: f[1], Title: f[2], Detail: f[3]})
	}
	return recs
}

// only returns the single record a check produced, failing when a check that must record exactly one
// finding recorded none or several.
func only(t *testing.T, recs []record) record {
	t.Helper()
	if len(recs) != 1 {
		t.Fatalf("expected exactly one record, got %d: %+v", len(recs), recs)
	}
	return recs[0]
}

func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// shellCaseQuote escapes a `case` pattern. The fixture keys are plain kubectl argument fragments, so
// the only metacharacters worth refusing are the ones that would silently widen the pattern.
func shellCaseQuote(s string) string {
	if strings.ContainsAny(s, "*?[]|()\\'\"") {
		panic("fixture key must be a literal kubectl argument fragment: " + s)
	}
	return s
}
