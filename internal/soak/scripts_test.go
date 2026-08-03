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
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The kit's shell scripts are the operator-facing half of this package, and the only half nothing
// else here can execute. hack/soak/collect.sh is run ONCE, by one person, at the end of a
// fortnight — so a defect in it does not cost a retry, it costs the retry window.
//
// These tests are text-level on purpose. Running collect.sh needs a cluster, which is exactly why
// its two real defects survived review: both were invisible to reading and obvious the first time
// anyone ran it.

// scriptDir is where the kit's scripts live, relative to this package.
const scriptDir = "../../hack/soak"

func kitScripts(t *testing.T) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(scriptDir)
	if err != nil {
		t.Fatalf("read %s: %v", scriptDir, err)
	}
	out := map[string]string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sh") {
			continue
		}
		path := filepath.Join(scriptDir, e.Name())
		body, err := os.ReadFile(path) // #nosec G304 -- path is built from a constant dir listing
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		out[path] = string(body)
	}
	if len(out) < 2 {
		t.Fatalf("found %d script(s) under %s — this guard has gone blind", len(out), scriptDir)
	}
	return out
}

// gluedExpansion matches an UNBRACED parameter expansion whose next character is non-ASCII.
//
// `[^\x00-\x7F]` rather than a list of punctuation: the hazard is the byte value, not which
// character it happens to be, so an em dash or an arrow is exactly as dangerous as the ellipsis
// that actually broke the script.
var gluedExpansion = regexp.MustCompile(`\$[A-Za-z_][A-Za-z0-9_]*[^\x00-\x7F]`)

// TestKitScriptsBraceExpansionsBeforeNonASCII pins a defect that made collect.sh unrunnable, in a
// way no amount of reading it would have caught.
//
// The script printed a progress line ending in an ellipsis, with the pod name expanded UNBRACED
// immediately before it. The ellipsis is three bytes above 0x7F. A shell whose locale does not
// classify those bytes as non-identifier characters reads them as part of the variable name, so
// `set -u` aborted the run with "<name><garbage>: unbound variable" — three lines before the
// export, on a cluster where everything was working. Reproduced on macOS with LANG=fr_FR.
//
// The syntax is valid, so `sh -n` passes it and shellcheck passes it. Only the locale decides,
// which means the author's machine can be fine and the operator's not. This test does not care
// about the locale: it forbids the construct.
func TestKitScriptsBraceExpansionsBeforeNonASCII(t *testing.T) {
	for path, body := range kitScripts(t) {
		for i, line := range strings.Split(body, "\n") {
			m := gluedExpansion.FindString(line)
			if m == "" {
				continue
			}
			t.Errorf("%s:%d has an unbraced expansion glued to a non-ASCII character (%q).\n"+
				"Under a locale that does not mark those bytes as non-identifier characters the "+
				"shell reads them as part of the variable name and `set -u` kills the script. "+
				"Brace it. Line:\n  %s", path, i+1, m, strings.TrimSpace(line))
		}
	}
}

// TestKitScriptsAreSetU is the other half: the guard above only matters because these scripts run
// under `set -u`, and it is what turns a mistyped variable into a loud failure instead of an empty
// string silently flowing into a kubectl invocation.
func TestKitScriptsAreSetU(t *testing.T) {
	for path, body := range kitScripts(t) {
		if !regexp.MustCompile(`(?m)^set -[a-z]*u`).MatchString(body) {
			t.Errorf("%s does not run under `set -u`. An unset variable would then expand to "+
				"nothing and flow into a kubectl invocation as a silently-empty argument, which "+
				"is how a collection ends up scoped to the wrong namespace rather than failing.",
				path)
		}
	}
}
