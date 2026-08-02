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

import "github.com/CrystalBackup/CrystalBackup/internal/apiconst"

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
