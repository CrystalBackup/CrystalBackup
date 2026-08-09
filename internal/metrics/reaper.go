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
	"slices"

	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// kindLabel names the object kind a reaper figure is about (VolumeSnapshot, Job, …). It is a
// code-chosen constant from orphanReapStuckKinds, never a value read off an object, so §1's
// cardinality bound holds by construction.
const kindLabel = "kind"

// The orphan reaper's HONESTY gauge, and the incident that made it necessary.
//
// A production cluster ran for 31 hours with the reaper logging, every 10 minutes, that it had
// "reaped" the same three VolumeSnapshots. It had not. Their deletion was blocked on
// external-snapshotter's bound-protection finalizers
// (snapshot.storage.kubernetes.io/volumesnapshot-bound-protection and its content-side sibling), so
// each object carried a deletionTimestamp and was going nowhere. Meanwhile the operator's OWN
// self-check reported `leakIndicators: VolumeSnapshot — total 8, residual 8, oldestAgeHours 31`.
// Two components in one binary, one saying the residue was collected and the other saying it was
// still there — and the one that was wrong was the reaper, because it reported the outcome of a
// request it had only issued.
//
// Issuing a DELETE means the apiserver ACCEPTED the deletion for processing. With a finalizer in
// play, "accepted" and "gone" can be separated by forever. So the reaper now reads the object back
// and reports three distinct facts (see internal/controller/reaper.go's reapOutcome), and this
// gauge publishes the one that needs a human: an object whose deletion has been requested and
// which is still there, held by somebody else's finalizer. Only an administrator can break a
// finalizer deadlock — the reaper deliberately does not strip a finalizer belonging to another
// controller — so this number existing at all is the whole point.
//
// A GAUGE, not a counter, and set imperatively rather than derived at scrape time. The condition it
// describes is a STANDING STATE ("there is residue stuck right now"), not an event: a counter
// incremented once per sweep would say "186 stuck things" after 31 hours of the same three, which
// is the same overstatement in a different currency. It cannot be a scrape-time gauge either,
// because deciding an object is stuck requires the reaper's orphan vetting (owner gone / volume
// terminal / past MinAge) — that is the reaper's judgement, not a property readable off the object.
// The cost of the choice is stated plainly: the value is only as fresh as the last completed sweep
// (default 10 minutes), and after a restart it reads zero until the first sweep lands.
var orphanReapStuck = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: NameOrphanReapStuck,
	Help: "Orphaned managed objects whose deletion was requested and has NOT completed: still present, " +
		"with a deletionTimestamp, held by a finalizer. Refreshed by each orphan-reaper sweep. " +
		"Non-zero requires an administrator — the operator will not strip another controller's finalizer.",
}, []string{kindLabel})

// orphanReapStuckKinds enumerates every kind the reaper can report on, and every one of them is
// published on every sweep — including at zero.
//
// That is not cosmetic. names.go records the lesson in full: a Vec child only materialises on its
// first write, so a family that is only touched when something is wrong is ABSENT rather than 0 the
// rest of the time, and `absent()` is not what an operator writes when they mean "is anything
// stuck". Publishing the complete set means `crystalbackup_orphan_reap_stuck > 0` is a sufficient
// and complete expression, and a series dropping back to 0 is visible as recovery rather than as a
// disappearance.
var orphanReapStuckKinds = []string{
	"ClusterRoleBinding",
	"Job",
	"PersistentVolume",
	"PersistentVolumeClaim",
	"RoleBinding",
	"Secret",
	kindVolumeSnapshot,
	kindVolumeSnapshot + "Content",
}

// kindVolumeSnapshot is spelled out once because it is the kind the incident behind this file was
// about, and because both halves of the snapshot pair derive from it. internal/controller keeps its
// own copy of these strings deliberately — this package depends on the API types and nothing else,
// so a controller-side refactor can never reshape a published label value by accident; the two are
// held together by the reaper's TestStuckKindsAreAllPublished rather than by an import.
const kindVolumeSnapshot = "VolumeSnapshot"

func init() {
	ctrlmetrics.Registry.MustRegister(orphanReapStuck)
	// Materialise every child at zero, so the series exists from the very first scrape — before the
	// reaper's first sweep has run — rather than appearing only once something is wrong.
	SetOrphanReapStuck(nil)
}

// OrphanReapStuckKinds exposes the enumerated kind set to the reaper's tests, which must be able to
// assert that a kind they report on is one this package actually publishes: a kind the reaper names
// and the catalogue does not enumerate would still be published (see SetOrphanReapStuck), but it
// would never be reset to zero, and a stuck count that can only ever go up is the overstatement
// this whole file exists to prevent.
func OrphanReapStuckKinds() []string { return slices.Clone(orphanReapStuckKinds) }

// SetOrphanReapStuck publishes ONE COMPLETE sweep result: for each kind, how many orphaned objects
// that sweep found with a deletion requested and not completed. A kind absent from stuck is
// published as 0 — the sweep looked and found none, which is a measurement, not a gap.
//
// It must be called only at the END of a sweep that ran to completion. A sweep that aborted early
// (a List failure) must NOT call it: the honest reading of a half-finished sweep is "unknown", and
// publishing its partial tally as if it were the whole picture would understate the leak in exactly
// the direction that hid the original one for 31 hours. The previous sweep's value staying put,
// alongside the logged error, is the accurate state of knowledge.
func SetOrphanReapStuck(stuck map[string]int) {
	for _, kind := range orphanReapStuckKinds {
		orphanReapStuck.WithLabelValues(kind).Set(float64(stuck[kind]))
	}
	// A kind the reaper reports that this package does not enumerate is still published rather than
	// dropped. Silently discarding it would be a new instance of the very defect being fixed — a
	// stuck object nobody can see — and the values are code-chosen constants, so there is no
	// cardinality exposure. The absence from orphanReapStuckKinds is the bug to fix when it happens.
	for kind, n := range stuck {
		if !slices.Contains(orphanReapStuckKinds, kind) {
			orphanReapStuck.WithLabelValues(kind).Set(float64(n))
		}
	}
}
