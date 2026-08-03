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

package controller

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// The wiring guard: every mover Job this package builds must carry the LOCATION's
// repository tuning, not merely most of them.
// ---------------------------------------------------------------------------

// jobRequestSite is one mover.JobRequest composite literal found in the package source.
type jobRequestSite struct {
	file string
	line int
	fn   string // the enclosing function's name
	lit  *ast.CompositeLit
}

// s3ConnectionsExempt lists the enclosing functions whose mover.JobRequest may omit
// S3Connections, each with the reason it may.
//
// It is a map rather than a bool on the check because an exemption must be ARGUED, and an
// argument has to be written somewhere a reviewer will read it. A boolean skip is a decision
// nobody can audit a year later; a required string is one they can disagree with.
//
// Keep it short. Every entry here is a mover Job that runs untuned against whatever it talks to,
// so an entry that stops being true is a silent regression — which is why the test below also
// fails on an entry that no longer matches any call site.
var s3ConnectionsExempt = map[string]string{
	"buildSyncJobRequest": "the external-sync copy addresses BOTH its repositories as rclone remotes " +
		"(syncEndpoint.RepoURL renders `rclone:`), so an s3 backend option would name a backend " +
		"nothing in that pod speaks. Concurrency there is an rclone knob, not this field.",
}

// TestEveryMoverJobRequestCarriesTheS3Tuning is the answer to "a knob that reaches nine of ten
// call sites".
//
// mover.JobRequest.S3Connections is nil-tolerant by design — nil means "restic's own default",
// which is a perfectly good Job — so a call site that forgets it compiles, runs, backs data up
// correctly, and is deaf to the one field the operator edited. Nothing at runtime would ever
// say so. That is the exact shape of JobRequest.GoMemLimit, which existed, was consumed by
// moverEnv, was covered by tests, and which no caller assigned from M1 until 0.6.1; and of the
// three M5 features that shipped documented and completely inert.
//
// A compile error cannot catch it (a struct literal may omit any field), so this reads the
// package's own source. It parses the AST rather than grepping, so that an `S3Connections:`
// inside a comment or a string literal cannot satisfy it — this file is full of both.
//
// This is the middle layer of delta 13's three, and the one meant to survive a refactor: the
// mover unit test proves the option can reach an argv, the crucible proves it reached a real
// Job in a real cluster, and this proves nobody left a call site behind in between.
func TestEveryMoverJobRequestCarriesTheS3Tuning(t *testing.T) {
	sites := jobRequestSites(t)

	// A guard that inspects nothing passes. This package builds a Job for the backup, the
	// restore, the four manifest shapes, init, the maintenance ops, discovery's inventory and
	// the sync copy; if the count collapses, the walk broke rather than the code.
	if len(sites) < 8 {
		t.Fatalf("found only %d mover.JobRequest literals — the AST walk is not finding them, "+
			"so this guard is asserting nothing", len(sites))
	}

	used := map[string]bool{}
	for _, s := range sites {
		if reason, exempt := s3ConnectionsExempt[s.fn]; exempt {
			used[s.fn] = true
			// An exemption must still be VISIBLE at the call site. The reason lives here; the
			// code must at least mention the field, or the next reader sees only an absence.
			if !mentionsS3Connections(t, s.file, s.fn) {
				t.Errorf("%s:%d: %s is exempt from S3Connections (%s) — but its body never names "+
					"the field, so at the call site the omission is indistinguishable from an "+
					"oversight. Say so in a comment there.", s.file, s.line, s.fn, reason)
			}
			continue
		}
		if !hasField(s.lit, "S3Connections") {
			t.Errorf("%s:%d: mover.JobRequest in %s() sets no S3Connections — this Job runs against "+
				"the repository with restic's built-in connection default and ignores the location's "+
				"spec.s3.connections entirely. Thread it, or add %q to s3ConnectionsExempt with the "+
				"reason it cannot apply.", s.file, s.line, s.fn, s.fn)
		}
	}

	// The exemption list must not outlive what it exempts. A stale entry silently re-opens the
	// hole for whatever function later takes that name.
	for fn := range s3ConnectionsExempt {
		if !used[fn] {
			t.Errorf("s3ConnectionsExempt names %q, but no mover.JobRequest literal in this package "+
				"is built there any more — drop the entry rather than leaving a standing exemption "+
				"for a function that could come back meaning something else", fn)
		}
	}
}

// jobRequestSites returns every mover.JobRequest composite literal in the package's non-test
// sources, each tagged with the function that builds it.
//
// It walks per-FuncDecl rather than per-file so that a failure can NAME the call site. "Some Job
// somewhere is untuned" sends the reader hunting through ten controllers; "restore_engine.go:743
// in startVolume()" is a fix. That is not a cosmetic difference for a guard whose whole purpose
// is to be actionable the one time it fires, years from now, against code nobody remembers.
func jobRequestSites(t *testing.T) []jobRequestSite {
	t.Helper()

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the controller package directory: %v", err)
	}
	fset := token.NewFileSet()
	var sites []jobRequestSite

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				lit, ok := n.(*ast.CompositeLit)
				if !ok || !isJobRequestType(lit.Type) {
					return true
				}
				sites = append(sites, jobRequestSite{
					file: name,
					line: fset.Position(lit.Pos()).Line,
					fn:   fn.Name.Name,
					lit:  lit,
				})
				return true
			})
		}
	}
	return sites
}

// mentionsS3Connections reports whether the named function's source text names the field at all —
// in a comment, since by construction it does not set it. Deliberately a text search over the one
// function's byte range: the thing being checked here IS the prose, so there is nothing in the
// AST to match.
func mentionsS3Connections(t *testing.T, file, fnName string) bool {
	t.Helper()

	src, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, file, src, parser.ParseComments)
	if err != nil {
		t.Fatalf("re-parse %s with comments: %v", file, err)
	}
	for _, decl := range parsed.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != fnName || fn.Body == nil {
			continue
		}
		// From the doc comment (if any) through the closing brace, so a reason written above the
		// function counts as much as one written inside it.
		start := fn.Pos()
		if fn.Doc != nil {
			start = fn.Doc.Pos()
		}
		from, to := fset.Position(start).Offset, fset.Position(fn.End()).Offset
		if from >= 0 && to <= len(src) && from < to {
			return strings.Contains(string(src[from:to]), "S3Connections")
		}
	}
	return false
}
