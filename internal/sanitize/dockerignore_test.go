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

package sanitize_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// embedDirective captures the operand of a //go:embed line.
var embedDirective = regexp.MustCompile(`(?m)^//go:embed\s+(.+)$`)

// TestDockerignoreCoversEveryEmbed walks the tree for //go:embed directives and fails if any of
// their targets is not re-included in .dockerignore.
//
// It exists because of a failure that reached a release gate. .dockerignore denies everything and
// re-includes an allowlist which, for source, is only **/*.go — so a file pulled in by go:embed is
// Go source to the compiler and invisible to the Docker context. The build then passes everywhere
// a developer looks (the file is on disk) and fails only inside the container, with
// "pattern report.css: no matching files found".
//
// The file already carried a comment saying "add every new embed target to this list". M6 added two
// and nobody did. That is the whole lesson: a comment addressed to a future reader is not a
// control, because the person who breaks it is by definition the one who did not read it. This test
// moves the reminder from prose to the build, where being wrong is loud and immediate.
func TestDockerignoreCoversEveryEmbed(t *testing.T) {
	root := repoRoot(t)

	ignore, err := os.ReadFile(filepath.Join(root, ".dockerignore"))
	if err != nil {
		t.Fatalf("read .dockerignore: %v", err)
	}
	allowed := map[string]bool{}
	for _, line := range strings.Split(string(ignore), "\n") {
		if after, ok := strings.CutPrefix(strings.TrimSpace(line), "!"); ok {
			allowed[after] = true
		}
	}

	var missing []string
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Skip trees that are not part of the image build context.
			switch d.Name() {
			case ".git", "bin", "node_modules", "website", "test", ".claude":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		src, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		for _, m := range embedDirective.FindAllStringSubmatch(string(src), -1) {
			for _, pattern := range strings.Fields(m[1]) {
				// Embed patterns are relative to the file's own directory; .dockerignore
				// paths are relative to the repository root.
				rel, relErr := filepath.Rel(root, filepath.Join(filepath.Dir(path), pattern))
				if relErr != nil {
					return relErr
				}
				if !allowed[rel] {
					missing = append(missing, rel)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	if len(missing) > 0 {
		t.Errorf("//go:embed targets missing from .dockerignore: %v\n"+
			"Add `!<path>` for each. Without it `go build` succeeds on your machine and the "+
			"container build fails with \"pattern <file>: no matching files found\" — the file "+
			"is on disk but never enters the Docker context.", missing)
	}
}

// repoRoot walks up from the test's working directory until it finds go.mod.
func repoRoot(t *testing.T) string {
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
