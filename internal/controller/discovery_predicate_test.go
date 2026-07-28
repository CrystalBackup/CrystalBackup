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
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
)

// TestInventoryChurnPredicate is the regression guard for the discovery self-trigger loop
// (docs/audit-m3.1-throughput.md). Discovery writes snapshotCount / namespacesPresent /
// lastDiscoveryTime onto the very BackupRepository it watches; unfiltered, each write came back as
// an Update event and re-enqueued discovery at once, so it never rested at `discovery.interval` —
// it spun back-to-back, blocking its single worker on a fresh `restic snapshots` Job every pass
// (measured: ~5.7 s/pass, one worker ~100 % saturated for a whole crucible run, at THREE
// snapshots). The predicate must swallow that churn while still letting through the two updates
// discovery cannot afford to miss: a spec change, and Initialized flipping true (the repository
// only becomes inventoriable then, and dropping it would stall the first inventory until the
// retry requeue).
func TestInventoryChurnPredicate(t *testing.T) {
	pred := inventoryChurnPredicate()

	// repoAt builds a BackupRepository at a given generation / Initialized / inventory state.
	repoAt := func(gen int64, initialized bool, snaps int32, discovered *metav1.Time) *cbv1.BackupRepository {
		return &cbv1.BackupRepository{
			ObjectMeta: metav1.ObjectMeta{Name: "dr", Generation: gen},
			Status: cbv1.BackupRepositoryStatus{
				Initialized:       initialized,
				SnapshotCount:     snaps,
				LastDiscoveryTime: discovered,
			},
		}
	}
	// annotated returns a copy carrying an annotation — the shape of the envtest specs' nudge
	// (pokeRepository), which must survive the filter.
	annotated := func(r *cbv1.BackupRepository, v string) *cbv1.BackupRepository {
		out := r.DeepCopy()
		out.Annotations = map[string]string{"test.crystalbackup.io/poke": v}
		return out
	}

	t0 := metav1.Now()
	t1 := metav1.NewTime(t0.Add(6 * 1000 * 1000 * 1000)) // +6s: the observed self-trigger cadence

	tests := []struct {
		name string
		old  *cbv1.BackupRepository
		new  *cbv1.BackupRepository
		want bool
	}{{
		// THE defect: discovery's own inventory write must not wake discovery.
		name: "inventory status write is swallowed",
		old:  repoAt(3, true, 3, &t0),
		new:  repoAt(3, true, 3, &t1),
		want: false,
	}, {
		// Same write, but the snapshot count moved too — still discovery's own write, still churn.
		name: "inventory write with a changed snapshot count is swallowed",
		old:  repoAt(3, true, 3, &t0),
		new:  repoAt(3, true, 7, &t1),
		want: false,
	}, {
		// The repository just became usable: inventory it now, not at the next retry tick.
		name: "Initialized false->true passes",
		old:  repoAt(3, false, 0, nil),
		new:  repoAt(3, true, 0, nil),
		want: true,
	}, {
		name: "spec change (generation bump) passes",
		old:  repoAt(3, true, 3, &t0),
		new:  repoAt(4, true, 3, &t0),
		want: true,
	}, {
		// BackupRepositorySpec is empty, so generation never moves for this kind: an annotation
		// IS the nudge (pokeRepository in the envtest specs). Swallowing it stalls those specs
		// until the next interval tick — the regression this case pins.
		name: "annotation poke passes",
		old:  repoAt(1, true, 3, &t0),
		new:  annotated(repoAt(1, true, 3, &t0), "42"),
		want: true,
	}, {
		// The maintenance controller's writes are filtered too (M4). A prune recording
		// lastMaintenanceTime, and especially the periodic physical-size and stale-lock refresh,
		// would otherwise cost a full `restic snapshots` Job apiece for an inventory nobody asked
		// to refresh — the same waste as discovery's own self-trigger, one milestone later.
		name: "the maintenance controller's status write is filtered",
		old:  repoAt(1, true, 3, &t0),
		new: func() *cbv1.BackupRepository {
			r := repoAt(1, true, 3, &t0)
			r.Status.LastMaintenanceTime = &t1
			r.Status.LastCheckTime = &t1
			r.Status.LastCheckResult = "Passed"
			r.Status.ApproximateSizeBytes = 1 << 30
			r.Status.StaleLocks = 2
			r.Status.RecentMaintenance = []cbv1.MaintenanceRecord{{Operation: "prune", StartTime: t0}}
			return r
		}(),
		want: false,
	}, {
		// A status write OUTSIDE both controllers' field sets still wakes discovery: keySlots is
		// the BackupRepository controller's, and discovery has no way to know it is irrelevant.
		name: "an unrelated status write still passes",
		old:  repoAt(1, true, 3, &t0),
		new: func() *cbv1.BackupRepository {
			r := repoAt(1, true, 3, &t0)
			r.Status.KeySlots = []string{"platform", "tenant"}
			return r
		}(),
		want: true,
	}, {
		// A non-BackupRepository update is not ours to judge — fail open.
		name: "foreign object passes",
		old:  nil,
		new:  nil,
		want: true,
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var ev event.UpdateEvent
			if tc.old == nil {
				ev = event.UpdateEvent{ObjectOld: &batchv1.Job{}, ObjectNew: &batchv1.Job{}}
			} else {
				ev = event.UpdateEvent{ObjectOld: tc.old, ObjectNew: tc.new}
			}
			if got := pred.Update(ev); got != tc.want {
				t.Errorf("Update() = %v, want %v", got, tc.want)
			}
		})
	}

	// Create/Delete/Generic must stay unfiltered: a new repository, or one resurfacing after a
	// cache resync, still needs an inventory pass.
	if !pred.Create(event.CreateEvent{Object: repoAt(1, true, 0, nil)}) {
		t.Error("Create() = false, want true (a new repository must be inventoried)")
	}
	if !pred.Generic(event.GenericEvent{Object: repoAt(1, true, 0, nil)}) {
		t.Error("Generic() = false, want true")
	}
	if !pred.Delete(event.DeleteEvent{Object: repoAt(1, true, 0, nil)}) {
		t.Error("Delete() = false, want true")
	}
}
