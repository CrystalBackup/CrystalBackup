//go:build !ignore

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
	"slices"
	"strings"

	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
)

// This file carries NO `crucible` build tag on purpose.
//
// The leak-check predicate used to live in m1_helpers_test.go, behind the tag, which meant it
// could only ever be exercised by a paid campaign — and a mistake in it surfaced as seven
// ten-minute timeouts two hours into that campaign. Moving it here is what lets
// leakcheck_predicate_test.go check it in the ordinary `make test`, with no cluster.
//
// m1SeedLabel comes along because the PVC rule pairs the two: an object is residue when it
// carries the exposure label AND is not one of the harness's seed volumes.

// m1SeedLabel marks the PVCs the crucible's own seed step creates. They live in the tenant
// namespaces for the whole run and are never residue.
const m1SeedLabel = "crystalbackup.io/seed"

// m1IsExposureResidue reports whether an object is one of the exposure objects a backup creates
// and must collect: the temp clone PVC, the static VolumeSnapshot, its VolumeSnapshotContent.
//
// It tests for the label the EXPOSER ACTUALLY STAMPS — exposureLabels() in the Backup controller
// puts crystalbackup.io/pvc on every one of them — rather than for any crystalbackup.io/* key.
//
// The difference is not pedantry, it is a real failure this cost a full paid campaign to find.
// The old predicate was a domain-prefix match, and it was correct only while NOTHING PERMANENT
// lived in the operator namespace: every crystalbackup.io-labelled object there was, by
// construction, mover residue in flight. 0.6.2 puts the first permanent resident in it — the soak
// collector, whose PVC carries crystalbackup.io/soak=collector and never goes away. The prefix
// matched it, so three leak-checks waited their full ten minutes and failed on a PVC that was
// doing exactly what it was deployed to do, on a cluster with zero actual residue.
//
// Narrowing to the stamped label makes this check STRICTER, not laxer: it now asserts the precise
// property it always meant — "no exposure object outlived its backup" — instead of a proxy that
// happened to coincide with it. Anything the chart adds to that namespace later is out of scope
// by construction rather than by an exclusion list somebody has to remember to extend.
func m1IsExposureResidue(labels map[string]string) bool {
	return labels[apiconst.LabelPVC] != ""
}

// m1ResidueLinkLabels are, in reading order, the four labels that name the run an exposure object
// belongs to. Every object m1IsExposureResidue can flag carries at least one of them when the
// controllers are behaving:
//
//   - LabelBackup — the owning Backup's name, stamped on BOTH planes (exposureLabels);
//   - LabelClusterBackup — the ClusterBackup run, cluster plane only. On the namespace plane it is
//     ABSENT, not empty: see apiconst.LabelBackup's comment for the three readers that went blind
//     on the difference, and for why that trio is the 0.6.5 leak;
//   - LabelRestore / LabelClusterRestore — the restore-side equivalents (restoreEngine.volumeLabels
//     stamps the owner key plus LabelPVC, so a restore's staging PVC and twin PV are residue by the
//     same predicate and must be attributable by the same rule).
var m1ResidueLinkLabels = []string{
	apiconst.LabelBackup,
	apiconst.LabelClusterBackup,
	apiconst.LabelRestore,
	apiconst.LabelClusterRestore,
}

// m1ResidueRuns lists every run name a residual object attributes to, deduplicated and in the
// reading order above. On the cluster plane the first two labels carry the SAME value (a fan-out
// child Backup's name equals the run), which is why the result is deduplicated rather than positional.
//
// An EMPTY result does not mean "no owner". It means "unattributable by label" — which is exactly
// the shape of the VolumeSnapshotContent the 0.6.5 campaign leaked, and the reason callers must
// treat it as foreign rather than as their own.
func m1ResidueRuns(labels map[string]string) []string {
	var runs []string
	for _, key := range m1ResidueLinkLabels {
		v := labels[key]
		if v == "" {
			continue
		}
		if !slices.Contains(runs, v) {
			runs = append(runs, v)
		}
	}
	return runs
}

// m1ResidueOwnedBy reports which of ownRuns a residual object belongs to, or "" when it belongs to
// none of them. It is the leak-check's fail-fast discriminator, and the property it tests is
// OWNERSHIP — never age.
//
// Age was the 0.6.5 rule and it was wrong: a spec creates its run before it checks that run for
// residue, so a spec's own still-draining objects ALWAYS predate their own leak check. See the long
// note above the Eventually in m1AssertNoResidualSnapshotObjects for the incident.
//
// names is the object's own name, plus (for a VolumeSnapshotContent) the name of the VolumeSnapshot
// it references. It backs a deliberate SECOND attempt at attribution, because label attribution has
// a real hole: a dynamic origin VolumeSnapshotContent is created by the external snapshot-controller,
// not by us, and carries our labels only once the handover patch lands — a content that leaked
// BEFORE that patch carries none. Its name, though, is derived from moverNamePrefix
// ("<namespace>-<backup>-<pvc>", internal/controller), so the run name is a substring of it. Run
// names are campaign-unique (crucibleRunName / crucibleRunID), so no other lane's object can contain
// one of ours by accident.
//
// The fallback is best-effort on purpose: moverNamePrefix truncates at 56 characters, so a long
// namespace+run+pvc triple can cut the run name out of the name entirely. When that happens the
// object is simply unattributable and the caller fails fast — the safe direction, and the one that
// keeps the 0.6.5 guard intact.
func m1ResidueOwnedBy(labels map[string]string, names []string, ownRuns []string) string {
	runs := m1ResidueRuns(labels)
	// ownRuns is iterated in the caller's order (never a map) so that an object attributable to two
	// of the caller's runs always reports the same one, run after run.
	for _, own := range ownRuns {
		if own == "" {
			continue
		}
		if slices.Contains(runs, own) {
			return own
		}
	}
	// Second pass, and only after every label has been tried: the name fallback is the weaker
	// evidence of the two, so a label that attributes an object elsewhere must always win over a
	// name that happens to contain one of our run names.
	for _, own := range ownRuns {
		if own == "" {
			continue
		}
		for _, n := range names {
			if n != "" && strings.Contains(n, own) {
				return own
			}
		}
	}
	return ""
}

// m1DescribeResidueOwner renders, for a failure message, what the object says about its own owner.
// The unattributable case gets a full sentence rather than an empty string: it is the single most
// expensive shape to misread, and the reader of a red campaign log should not have to know that.
func m1DescribeResidueOwner(labels map[string]string) string {
	if runs := m1ResidueRuns(labels); len(runs) > 0 {
		return "run " + strings.Join(runs, "/")
	}
	return "NO run at all — it carries none of " + strings.Join(m1ResidueLinkLabels, ", ") +
		", which is the unattributable shape the 0.6.5 campaign leaked"
}
