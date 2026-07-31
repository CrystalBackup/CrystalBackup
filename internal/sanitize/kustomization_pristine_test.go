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

package sanitize

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestManagerKustomizationCarriesNoImageOverride keeps a build artifact out of the source tree.
//
// kubebuilder scaffolds `make deploy` and `make build-installer` as
// `cd config/manager && kustomize edit set image controller=$IMG`, which rewrites a TRACKED file
// with whatever tag the caller happened to use. `make e2e` goes through that path, so an e2e run
// left config/manager/kustomization.yaml holding `newTag: v0.0.0-e2e` — a throwaway example.com
// reference — and the repository dirty.
//
// The consequence that made this worth a test rather than a habit: `git describe --dirty` is what
// stamps crystalbackup_build_info. Preparing 0.6.1 right after an e2e run produced an operator
// image labelled `v0.6.0-7-g6a3bb60-dirty`, which is precisely the "build_info names no build"
// defect the version-stamping lot was written to remove. The residue was also caught in
// `git status` twice while staging a release, once for 0.6.0 and once here.
//
// The Makefile now renders from a temporary copy so nothing in the tree is touched. This test is
// what stops the scaffolded form coming back — through a kubebuilder re-scaffold, a merge, or
// somebody restoring the "simpler" two-line target — because the reintroduction is silent: the
// targets keep working, and only the next release notices.
func TestManagerKustomizationCarriesNoImageOverride(t *testing.T) {
	path := filepath.Join(repoRootFromThisFile(t), "config", "manager", "kustomization.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	text := string(data)

	// Not a vacuous pass: the file must be the one we think it is.
	if !strings.Contains(text, "manager.yaml") {
		t.Fatalf("%s does not list manager.yaml; this test is reading the wrong file:\n%s", path, text)
	}

	for _, marker := range []string{"images:", "newTag:", "newName:"} {
		if strings.Contains(text, marker) {
			t.Errorf("config/manager/kustomization.yaml contains %q — a build artifact is in the source tree.\n"+
				"`kustomize edit set image` was run against the tracked file instead of a copy, which "+
				"leaves the repo dirty and makes `git describe --dirty` stamp a -dirty version into "+
				"crystalbackup_build_info. Restore the file (git checkout -- config/manager/kustomization.yaml) "+
				"and render through the Makefile's kustomize-build-with-image helper.\nFile:\n%s", marker, text)
		}
	}
}

// repoRootFromThisFile locates the repository root from this source file's own path, so the test
// is independent of the working directory `go test` was invoked from.
func repoRootFromThisFile(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed; cannot locate the repository root")
	}
	// internal/sanitize/<this file> -> repo root
	return filepath.Dir(filepath.Dir(filepath.Dir(thisFile)))
}
