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

package metrics

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// pathProbe exists only so a test can ask the compiler for this package's real import path.
type pathProbe struct{}

// xFlag captures the symbol path of a `-X <path>.Version=` linker flag.
var xFlag = regexp.MustCompile(`-X\s+([A-Za-z0-9_./-]+)\.Version=`)

// TestVersionStampTargetsThisPackage checks that every build path stamps the symbol that actually
// exists here.
//
// The linker's -X flag is silent about a wrong target. `-X some/wrong/path.Version=1.2.3` is not
// an error, not a warning, and not a failed build: the flag is simply ignored and the binary keeps
// its compiled-in default. So renaming this package, or moving Version out of it, would break all
// three build paths at once and the only symptom would be crystalbackup_build_info reporting
// "dev" again — which is exactly the state M6 found and fixed, having gone unnoticed through five
// releases.
//
// The expected path is asked of the compiler rather than written down, so this test cannot drift
// from the package it guards.
func TestVersionStampTargetsThisPackage(t *testing.T) {
	want := reflect.TypeOf(pathProbe{}).PkgPath()
	root := moduleRoot(t)

	// Every file that links the operator binary. The mover and sync images build
	// ./cmd/crystal-mover, which serves no metrics and carries no Version symbol.
	for _, rel := range []string{"Makefile", "Dockerfile", ".github/workflows/images.yml"} {
		src, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		matches := xFlag.FindAllStringSubmatch(string(src), -1)
		if len(matches) == 0 {
			t.Errorf("%s links the operator without -X …Version=: crystalbackup_build_info will "+
				"report the compiled-in default and name no build", rel)
			continue
		}
		for _, m := range matches {
			if got := m[1]; got != want {
				t.Errorf("%s stamps %q, but Version lives in %q. The linker ignores a -X flag "+
					"whose target does not exist — silently, with a successful build — so this "+
					"binary would ship reporting the default.", rel, got, want)
			}
		}
	}
}

// TestVersionDefaultIsNotAVersion pins the default to something no release could be mistaken for.
//
// It deliberately does NOT assert Version == "dev": the test binary is itself linkable with the
// flag, and a test that forbids a stamped build would fail for the one person doing the right
// thing. What matters is that whatever the default is, it cannot be read as a release.
func TestVersionDefaultIsNotAVersion(t *testing.T) {
	if strings.HasPrefix(Version, "v") || strings.Contains(Version, ".") {
		return // stamped by -X; nothing to check.
	}
	if Version != "dev" {
		t.Errorf("the unstamped default is %q; it should be %q, which every dashboard and the "+
			"self-check recognise as 'this binary was never told what it is'", Version, "dev")
	}
}

// moduleRoot walks up from the test's working directory until it finds go.mod.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found walking up from the test directory")
		}
		dir = parent
	}
}
