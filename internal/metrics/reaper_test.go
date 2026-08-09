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
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// TestOrphanReapStuckPublishesEveryKindIncludingZero is the contract that makes
// `crystalbackup_orphan_reap_stuck > 0` a complete expression rather than a half of one.
//
// names.go records why this matters in the general case (a Vec child materialises on its first
// write, so a family only touched when something is wrong is ABSENT rather than 0 the rest of the
// time). Here it matters twice over: the series exists to tell an administrator that a deletion is
// deadlocked, and a series that vanishes when the deadlock clears looks identical to a series that
// vanished because the operator stopped looking.
func TestOrphanReapStuckPublishesEveryKindIncludingZero(t *testing.T) {
	t.Cleanup(func() { SetOrphanReapStuck(nil) })

	SetOrphanReapStuck(map[string]int{kindVolumeSnapshot: 3})

	if got := testutil.ToFloat64(orphanReapStuck.WithLabelValues(kindVolumeSnapshot)); got != 3 {
		t.Errorf("%s = %v, want 3", kindVolumeSnapshot, got)
	}
	for _, kind := range OrphanReapStuckKinds() {
		if kind == kindVolumeSnapshot {
			continue
		}
		if got := testutil.ToFloat64(orphanReapStuck.WithLabelValues(kind)); got != 0 {
			t.Errorf("%s = %v, want an explicit 0 — the sweep looked and found none, which is a "+
				"measurement and not a gap", kind, got)
		}
	}
}

// TestOrphanReapStuckReturnsToZero: the deadlock clearing must be VISIBLE. A stuck count that only
// ever ratchets up is the same class of untrustworthy figure as a reap that was only ever requested
// — it just fails in the opposite direction, and an operator who learns to ignore it is back to
// where the 31-hour incident started.
func TestOrphanReapStuckReturnsToZero(t *testing.T) {
	t.Cleanup(func() { SetOrphanReapStuck(nil) })

	SetOrphanReapStuck(map[string]int{kindVolumeSnapshot + "Content": 2})
	if got := testutil.ToFloat64(orphanReapStuck.WithLabelValues(kindVolumeSnapshot + "Content")); got != 2 {
		t.Fatalf("VolumeSnapshotContent = %v, want 2", got)
	}
	// The next completed sweep found nothing stuck.
	SetOrphanReapStuck(map[string]int{})
	if got := testutil.ToFloat64(orphanReapStuck.WithLabelValues(kindVolumeSnapshot + "Content")); got != 0 {
		t.Errorf("VolumeSnapshotContent = %v after a clean sweep, want 0", got)
	}
}

// TestOrphanReapStuckPublishesAnUnenumeratedKind: dropping a kind this package does not know about
// would be a new instance of the defect being fixed — a stuck object nobody can see. It is published
// anyway; the missing entry in orphanReapStuckKinds is the thing to go and fix.
func TestOrphanReapStuckPublishesAnUnenumeratedKind(t *testing.T) {
	t.Cleanup(func() {
		orphanReapStuck.DeleteLabelValues("SomeFutureKind")
		SetOrphanReapStuck(nil)
	})

	SetOrphanReapStuck(map[string]int{"SomeFutureKind": 1})
	if got := testutil.ToFloat64(orphanReapStuck.WithLabelValues("SomeFutureKind")); got != 1 {
		t.Errorf("SomeFutureKind = %v, want 1 — an unenumerated kind must still be published", got)
	}
}
