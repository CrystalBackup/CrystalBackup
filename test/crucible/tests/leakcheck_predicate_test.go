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
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"testing"

	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
)

// The leak-check predicate decides what counts as an exposure object a backup failed to collect.
// It runs only under the `crucible` build tag, against paid infrastructure, ten minutes at a time
// — so a mistake in it is discovered at the end of a two-hour campaign and costs the campaign.
//
// It cost exactly that once. The predicate used to be a domain-prefix match: any
// crystalbackup.io/* label on an object in the operator namespace meant residue. That was correct
// only while nothing permanent lived there. 0.6.2's soak collector is the first permanent
// resident, its PVC carries crystalbackup.io/soak=collector, and three leak-checks spent their
// full ten-minute budget failing on a component that was doing its job, on a cluster with zero
// actual residue. Seven checks timed out; the campaign was red for a reason that was not a defect
// in the product.
//
// These tests run in the ORDINARY suite, with no cluster, so the next such mistake costs a
// `make test` rather than a campaign. That is the same reason runname_hermeticity_test.go is
// `//go:build !crucible` while inspecting the tagged suite.

// TestLeakPredicateIgnoresPermanentOperatorNamespaceResidents pins the regression directly: the
// objects the chart installs alongside the operator are not backup residue, however they are
// labelled. The soak collector's PVC is the case that broke a campaign; the others are the shapes
// a future chart addition is likely to take.
func TestLeakPredicateIgnoresPermanentOperatorNamespaceResidents(t *testing.T) {
	permanent := []struct {
		what   string
		labels map[string]string
	}{
		{
			"the soak collector's PVC — the exact object that failed seven checks",
			map[string]string{
				"app.kubernetes.io/name":       "crystal-backup",
				"app.kubernetes.io/component":  "soak",
				"app.kubernetes.io/managed-by": "Helm",
				"crystalbackup.io/soak":        "collector",
			},
		},
		{
			"a chart-managed object carrying only app.kubernetes.io labels",
			map[string]string{"app.kubernetes.io/managed-by": "Helm"},
		},
		{
			"the restic oracle Job, which needs managed-by to pass the egress NetworkPolicy",
			map[string]string{"app.kubernetes.io/managed-by": "crystal-backup"},
		},
		{"no labels at all", map[string]string{}},
		{"nil labels", nil},
	}

	for _, p := range permanent {
		if m1IsExposureResidue(p.labels) {
			t.Errorf("the leak check would flag %s as residue.\n"+
				"It is a permanent part of the deployment and will never go away, so every "+
				"leak-check calling this predicate burns its full timeout and fails on a healthy "+
				"cluster. Labels: %v", p.what, p.labels)
		}
	}
}

// TestLeakPredicateCatchesEveryExposureObject is the other half, and without it the test above is
// satisfied by a predicate that always returns false — which would turn every leak-check into a
// green that measures nothing. That is the failure mode this repository has been bitten by five
// times, so the pair is the point, not either test alone.
func TestLeakPredicateCatchesEveryExposureObject(t *testing.T) {
	// exposureLabels() in the Backup controller stamps this set on the temp clone PVC, the static
	// VolumeSnapshot and its VolumeSnapshotContent alike.
	residue := map[string]string{
		"app.kubernetes.io/managed-by": "crystal-backup",
		apiconst.LabelClusterBackup:    "nightly-20260803",
		apiconst.LabelNamespace:        "team-a",
		apiconst.LabelPVC:              "data-0",
	}
	if !m1IsExposureResidue(residue) {
		t.Fatalf("the leak check would MISS a real exposure object: %v\n"+
			"A leak-check that cannot see residue passes on a leaking cluster, which is worse "+
			"than the false positive this predicate was narrowed to remove.", residue)
	}

	// And the seed PVCs the harness creates must still be distinguishable — the PVC rule pairs
	// this predicate with the seed label, so a seed carrying the exposure label would be wrong.
	if m1IsExposureResidue(map[string]string{m1SeedLabel: "true"}) {
		t.Error("a seed PVC was flagged as exposure residue")
	}
}

// TestLeakPredicateReadsTheLabelTheControllerWrites is the coupling that keeps the two halves
// honest. The predicate is only correct because exposureLabels() stamps apiconst.LabelPVC on
// every exposure object; if somebody stops stamping it, the predicate silently stops seeing
// residue and every leak-check goes green while leaking.
//
// So this reads the controller's source and fails if that label leaves exposureLabels(). It is a
// source-level assertion rather than a behavioural one because the alternative — booting a
// controller — is exactly what this file exists to avoid.
func TestLeakPredicateReadsTheLabelTheControllerWrites(t *testing.T) {
	const src = "../../../internal/controller/backup_controller.go"
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read %s: %v", src, err)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, src, data, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", src, err)
	}

	// Walk the AST for the identifier rather than grepping the source text: a body containing
	// the words "LabelPVC" only inside a comment — which is exactly what a half-finished removal
	// looks like — would satisfy a text match while stamping nothing.
	var stamped bool
	var found bool
	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "exposureLabels" || fn.Body == nil {
			return true
		}
		found = true
		ast.Inspect(fn.Body, func(inner ast.Node) bool {
			sel, ok := inner.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "LabelPVC" {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "apiconst" {
				stamped = true
			}
			return true
		})
		return false
	})

	if !found {
		t.Fatal("could not find exposureLabels() in " + src + " — this guard has gone blind")
	}
	if !stamped {
		t.Error("exposureLabels() no longer stamps apiconst.LabelPVC.\n" +
			"m1IsExposureResidue tests for exactly that label, so every leak-check in the crucible " +
			"suite would now pass on a leaking cluster without a single one of them failing.")
	}
}
