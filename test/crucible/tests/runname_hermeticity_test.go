//go:build !crucible

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

package crucible

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// THE GUARD: no crucible run name may be a fixed string.
//
// WHY THIS EXISTS AT BUILD TIME. The bug it prevents is invisible until a campaign runs on a
// cluster whose bucket an EARLIER campaign wrote to — different clusters, weeks apart, on paid
// infrastructure. The shared "dr" repository is never emptied (scripts/nuke.sh says so in as many
// words); the discovery controller projects every (namespace, run) group it finds back into the
// cluster as a Completed Backup; and a fan-out that finds a stranger on its coordinate refuses the
// namespace with RunNameCollision rather than reporting success over data it did not write.
//
// The operator is right every time. The spec that hard-coded its run name is wrong every time. And
// the failure surfaces as an unrelated red assertion, minutes into a run, at real cost — the last
// occurrence took a full campaign (60 specs passing where 76 had passed) and a from-scratch
// diagnosis to attribute, and it was a REPEAT: the same trap had already been recorded in the
// project's notes after M5, and new specs reintroduced it anyway.
//
// A note in a memory file did not hold. This does. It reads the suite as source — no cluster, no
// bucket, no build tag, a few milliseconds inside `make test` — and fails on the next fixed run
// name, in the pull request that introduces it.
//
// WHAT IT ACCEPTS. Any run name whose value cannot be determined by reading the source: a call to
// crucibleRunName, a concatenation involving crucibleRunID, a value assigned from a helper or read
// back off the cluster (m1_cascade reads the name its ClusterBackupSchedule stamped out — that one
// is unique by construction and unknowable here, which is exactly why it passes).
//
// WHAT IT REJECTS. A run name that reduces, following consts and assignments, to string literals:
// `runName = "m6-fidelity-src"`, or `const collideRun = "m6-runname-collide"`, or a var assigned
// only literals. That is the defect class, stated exactly.
//
// IF YOU GENUINELY NEED A FIXED NAME, say so where you write it:
//
//	//crucible:runname-fixed R26 — this spec must find the run a PREVIOUS campaign left behind
//	runName := "some-fixed-name"
//
// on the same line or the line above. The reason is mandatory and is printed by nothing — it is
// there for the next reader, who will otherwise spend a campaign finding out why. Reaching for it
// should feel like a decision. There are, today, zero uses.
// ---------------------------------------------------------------------------

// runNameSinks maps a suite helper to the index of the argument that becomes a run name — the
// ClusterBackup/Backup object name, which the mover writes verbatim into the restic `run` tag.
//
// Registration is not optional: TestEveryRunHelperIsRegistered fails if the package grows a helper
// that looks like one of these and is missing here, so the guard cannot silently stop covering the
// suite it is meant to cover.
var runNameSinks = map[string]int{
	"m1RunClusterBackup":      0, // (name, locationName, nsSelector)
	"m3RunClusterBackup":      0, // (name, locationName, nsSelector, captureClusterResources)
	"m5RunManifestBackup":     0, // (name, locationName, namespaces...)
	"m5RunNamespaceBackup":    1, // (namespace, name, locationName, timeout)
	"m6RecreateClusterBackup": 0, // (name, namespace)
}

// runNameHelperPattern is what a run-creating helper looks like: m<N>Run…Backup / m<N>Recreate…Backup.
// Any function declaration matching it must appear in runNameSinks.
func isRunHelperName(name string) bool {
	rest, ok := strings.CutPrefix(name, "m")
	if !ok || rest == "" || rest[0] < '0' || rest[0] > '9' {
		return false
	}
	rest = strings.TrimLeft(rest, "0123456789")
	if !strings.HasSuffix(rest, "Backup") {
		return false
	}
	return strings.HasPrefix(rest, "Run") || strings.HasPrefix(rest, "Recreate")
}

// fixedNameMarker opts one site out, with a mandatory reason after it.
const fixedNameMarker = "//crucible:runname-fixed"

// TestNoFixedRunNames is the guard. It parses every crucible spec file (they carry the `crucible`
// build tag, so they are read as SOURCE here — nothing is compiled, nothing is executed) and fails
// on any run name that reduces to a fixed string.
func TestNoFixedRunNames(t *testing.T) {
	files, fset := parseCruciblePackage(t)

	// Coverage FIRST. A guard that is no longer reading the suite must say so before it reports
	// anything else — otherwise "0 offences" and "0 run names inspected" look identical, which is
	// the exact shape of self-disabling check this suite has had to delete twice.
	var inspected int
	for _, f := range files {
		inspected += len(runNameSites(f.file))
	}
	if inspected < 15 {
		t.Fatalf("the guard found only %d run-name site(s) across %d files — it is no longer reading "+
			"the suite it is meant to protect (helpers renamed? runNameSinks stale?)", inspected, len(files))
	}

	var offences []string
	for _, f := range files {
		idx := newNameIndex(files, f.file)
		for _, site := range runNameSites(f.file) {
			if !isFixedString(site.expr, idx, 0) {
				continue
			}
			pos := fset.Position(site.expr.Pos())
			// The marker is honoured at the USE site or at the DECLARATION the name resolves to —
			// a run name is typically declared once and used three times, and forcing the opt-out
			// onto every use would make the escape hatch worse than the thing it excuses.
			if hasFixedMarker(f.file, fset, witnessLines(site.expr, idx, 0)) {
				continue
			}
			offences = append(offences, fmt.Sprintf(
				"  %s:%d  %s ← %s", filepath.Base(pos.Filename), pos.Line, site.what, exprText(site.expr)))
		}
	}

	if len(offences) > 0 {
		slices.Sort(offences)
		t.Fatalf(`%d crucible run name(s) are fixed strings:

%s

A run name is a COORDINATE in the shared "dr" repository, and that repository outlives every
cluster the crucible builds — scripts/nuke.sh never empties the bucket. Reuse one and the discovery
controller projects a PREVIOUS campaign's snapshots onto it as a Completed Backup before your spec
creates anything; the fan-out then refuses the namespace with RunNameCollision, correctly, and your
spec goes red for a reason that has nothing to do with what it tests.

Name it per campaign instead:

    runName := crucibleRunName("m6-fidelity-src")   // → m6-fidelity-src-<this campaign's id>

If this spec really must address a run a previous campaign left behind, say so on the line above:

    %s <why>

`, len(offences), strings.Join(offences, "\n"), fixedNameMarker)
	}

	t.Logf("%d run names inspected across %d crucible spec files, none fixed", inspected, len(files))
}

// TestEveryRunHelperIsRegistered keeps runNameSinks honest. A new milestone that adds, say,
// m7RunClusterBackup gets a failure here on the day it is written — not a silent hole in the guard
// discovered a campaign later.
func TestEveryRunHelperIsRegistered(t *testing.T) {
	files, fset := parseCruciblePackage(t)

	for _, f := range files {
		for _, decl := range f.file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || !isRunHelperName(fn.Name.Name) {
				continue
			}
			if _, registered := runNameSinks[fn.Name.Name]; !registered {
				pos := fset.Position(fn.Pos())
				t.Errorf("%s:%d: %s creates backup runs but is not in runNameSinks — add it with the "+
					"index of its run-name argument, or the guard stops covering every spec that calls it",
					filepath.Base(pos.Filename), pos.Line, fn.Name.Name)
			}
		}
	}

	// And the other direction: a registered helper that no longer exists is a stale entry, which is
	// how a guard quietly narrows until it covers nothing.
	declared := map[string]bool{}
	for _, f := range files {
		for _, decl := range f.file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil {
				declared[fn.Name.Name] = true
			}
		}
	}
	for name := range runNameSinks {
		if !declared[name] {
			t.Errorf("runNameSinks names %s, which no longer exists in the package — a stale entry "+
				"covers nothing", name)
		}
	}
}

// ---------------------------------------------------------------------------
// Reading the suite
// ---------------------------------------------------------------------------

type parsedFile struct {
	name string
	file *ast.File
}

// parseCruciblePackage parses every *_test.go in this directory EXCEPT this one. The spec files
// carry `//go:build crucible`, so they are excluded from this build — parsing them as text is how a
// tag-free test gets to inspect them at all.
func parseCruciblePackage(t *testing.T) ([]parsedFile, *token.FileSet) {
	t.Helper()

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the crucible suite directory: %v", err)
	}

	fset := token.NewFileSet()
	var files []parsedFile
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, "_test.go") || name == "runname_hermeticity_test.go" {
			continue
		}
		parsed, perr := parser.ParseFile(fset, name, nil, parser.ParseComments)
		if perr != nil {
			t.Fatalf("parse %s: %v", name, perr)
		}
		files = append(files, parsedFile{name: name, file: parsed})
	}

	if len(files) < 20 {
		t.Fatalf("only %d crucible spec file(s) found — this test must run from the suite directory", len(files))
	}
	return files, fset
}

// site is one place a run name is handed to the product.
type site struct {
	what string // human-readable origin, printed in the failure
	expr ast.Expr
}

// runNameSites finds every run name in a file: the registered helper arguments, plus the ObjectMeta
// name of any ClusterBackup / Backup composite literal (specs that build the object by hand).
func runNameSites(f *ast.File) []site {
	var found []site

	ast.Inspect(f, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.CallExpr:
			id, ok := node.Fun.(*ast.Ident)
			if !ok {
				return true
			}
			argIdx, registered := runNameSinks[id.Name]
			if !registered || argIdx >= len(node.Args) {
				return true
			}
			found = append(found, site{what: id.Name + "(…)", expr: node.Args[argIdx]})

		case *ast.CompositeLit:
			kind, ok := compositeKind(node)
			if !ok || (kind != "ClusterBackup" && kind != "Backup") {
				return true
			}
			if nameExpr, ok := objectMetaName(node); ok {
				found = append(found, site{what: "cbv1." + kind + "{ObjectMeta.Name}", expr: nameExpr})
			}
		}
		return true
	})

	return found
}

// compositeKind returns the type name of a `pkg.Type{…}` composite literal.
func compositeKind(lit *ast.CompositeLit) (string, bool) {
	sel, ok := lit.Type.(*ast.SelectorExpr)
	if !ok {
		return "", false
	}
	return sel.Sel.Name, true
}

// objectMetaName digs the Name field out of a `…{ObjectMeta: metav1.ObjectMeta{Name: X}}` literal.
func objectMetaName(lit *ast.CompositeLit) (ast.Expr, bool) {
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok || key.Name != "ObjectMeta" {
			continue
		}
		meta, ok := kv.Value.(*ast.CompositeLit)
		if !ok {
			continue
		}
		for _, field := range meta.Elts {
			fkv, ok := field.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			if fkey, ok := fkv.Key.(*ast.Ident); ok && fkey.Name == "Name" {
				return fkv.Value, true
			}
		}
	}
	return nil, false
}

// ---------------------------------------------------------------------------
// "Is this expression a fixed string?"
// ---------------------------------------------------------------------------

// nameIndex resolves an identifier to every value ever bound to it — file scope first (these vars
// are file-local, and several files reuse the name `runName` for different things), then package
// scope.
type nameIndex struct {
	file map[string][]ast.Expr
	pkg  map[string][]ast.Expr
}

func newNameIndex(all []parsedFile, current *ast.File) *nameIndex {
	idx := &nameIndex{file: map[string][]ast.Expr{}, pkg: map[string][]ast.Expr{}}
	collectBindings(current, idx.file)
	for _, f := range all {
		if f.file == current {
			continue
		}
		collectTopLevelBindings(f.file, idx.pkg)
	}
	return idx
}

func (i *nameIndex) lookup(name string) ([]ast.Expr, bool) {
	if v, ok := i.file[name]; ok {
		return v, true
	}
	v, ok := i.pkg[name]
	return v, ok
}

// collectBindings records every value bound to a name anywhere in a file — declarations (const/var,
// at any depth) and assignments alike. Assignments matter: several specs declare `var runName
// string` and fill it in a BeforeAll.
func collectBindings(f *ast.File, into map[string][]ast.Expr) {
	ast.Inspect(f, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.ValueSpec:
			recordValueSpec(node, into)
		case *ast.AssignStmt:
			for k, lhs := range node.Lhs {
				id, ok := lhs.(*ast.Ident)
				if !ok || id.Name == "_" || k >= len(node.Rhs) {
					continue
				}
				into[id.Name] = append(into[id.Name], node.Rhs[k])
			}
		}
		return true
	})
}

func collectTopLevelBindings(f *ast.File, into map[string][]ast.Expr) {
	for _, decl := range f.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || (gen.Tok != token.CONST && gen.Tok != token.VAR) {
			continue
		}
		for _, spec := range gen.Specs {
			if vs, ok := spec.(*ast.ValueSpec); ok {
				recordValueSpec(vs, into)
			}
		}
	}
}

func recordValueSpec(vs *ast.ValueSpec, into map[string][]ast.Expr) {
	for k, name := range vs.Names {
		if name.Name == "_" || k >= len(vs.Values) {
			continue
		}
		into[name.Name] = append(into[name.Name], vs.Values[k])
	}
}

// isFixedString reports whether an expression provably reduces to a constant string.
//
// The asymmetry is deliberate and is what makes the guard usable: it flags ONLY what it can prove
// is fixed. Anything it cannot follow — a function call, a value read off the cluster, a name bound
// somewhere it cannot see — passes. False negatives are cheap (the next campaign is no worse off
// than today); a false positive would be a lint failure the author cannot honestly satisfy, and the
// guard would be turned off within a week.
func isFixedString(e ast.Expr, idx *nameIndex, depth int) bool {
	if depth > 8 {
		return false
	}
	switch node := e.(type) {
	case *ast.BasicLit:
		return node.Kind == token.STRING

	case *ast.ParenExpr:
		return isFixedString(node.X, idx, depth)

	case *ast.BinaryExpr:
		// Concatenation is fixed only if BOTH halves are — `"m3-cs-src-" + crucibleRunID` is not.
		return node.Op == token.ADD &&
			isFixedString(node.X, idx, depth+1) &&
			isFixedString(node.Y, idx, depth+1)

	case *ast.Ident:
		bound, ok := idx.lookup(node.Name)
		if !ok || len(bound) == 0 {
			// Declared with no value and never assigned anywhere visible — unknowable, so allowed.
			return false
		}
		for _, v := range bound {
			if !isFixedString(v, idx, depth+1) {
				return false
			}
		}
		return true

	default:
		// Calls, selectors, conversions, anything else: not provably fixed.
		return false
	}
}

// witnessLines returns every line that explains why this expression is fixed: the expression
// itself, plus the declaration/assignment of each identifier it resolves through, within this file.
// They are the places an author would reasonably write the opt-out.
func witnessLines(e ast.Expr, idx *nameIndex, depth int) []ast.Expr {
	if depth > 8 {
		return nil
	}
	out := []ast.Expr{e}
	switch node := e.(type) {
	case *ast.ParenExpr:
		out = append(out, witnessLines(node.X, idx, depth+1)...)
	case *ast.BinaryExpr:
		out = append(out, witnessLines(node.X, idx, depth+1)...)
		out = append(out, witnessLines(node.Y, idx, depth+1)...)
	case *ast.Ident:
		if bound, ok := idx.file[node.Name]; ok {
			for _, v := range bound {
				out = append(out, witnessLines(v, idx, depth+1)...)
			}
		}
	}
	return out
}

// hasFixedMarker reports whether a deliberate opt-out sits on one of the given expressions' lines
// (or the line above it), WITH a reason. A bare marker does not count: the reason is the whole
// point of the escape hatch, and an unexplained one is how the next reader loses a campaign.
func hasFixedMarker(f *ast.File, fset *token.FileSet, witnesses []ast.Expr) bool {
	lines := map[int]bool{}
	for _, w := range witnesses {
		lines[fset.Position(w.Pos()).Line] = true
	}
	for _, group := range f.Comments {
		for _, c := range group.List {
			if !strings.HasPrefix(c.Text, fixedNameMarker) {
				continue
			}
			if strings.TrimSpace(strings.TrimPrefix(c.Text, fixedNameMarker)) == "" {
				continue // a marker with no reason explains nothing and excuses nothing
			}
			at := fset.Position(c.Pos()).Line
			if lines[at] || lines[at+1] {
				return true
			}
		}
	}
	return false
}

// exprText renders an expression compactly for the failure message.
func exprText(e ast.Expr) string {
	switch node := e.(type) {
	case *ast.BasicLit:
		return node.Value
	case *ast.Ident:
		return node.Name
	case *ast.BinaryExpr:
		return exprText(node.X) + " + " + exprText(node.Y)
	case *ast.ParenExpr:
		return "(" + exprText(node.X) + ")"
	default:
		return fmt.Sprintf("%T", e)
	}
}
