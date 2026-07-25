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
	"context"
	"testing"
	"time"

	"github.com/CrystalBackup/CrystalBackup/internal/mover"
	"github.com/CrystalBackup/CrystalBackup/internal/repo/queue"
)

// TestMaintenanceMutexHalvesAgree is the regression guard for the one line adr/0015's forward
// contract stars: the per-repository mover mutex has TWO halves, enforced by two different
// predicates in two different packages, and marking one without the other "is only half the mutex".
//
//   - queue.blocksMovers (by OpKind) drives Manager.QuiescenceRequired, the READER side — it holds
//     NEW movers back while such an op is pending or in-flight.
//   - maintenanceOpBlocksMovers (by mover.Operation) drives waitForMoverQuiescence, the WRITER
//     side — it drains the movers ALREADY running before the op starts.
//
// Mark only the reader side and the op runs against movers that were already in flight. Mark only
// the writer side and it drains once, then new movers start underneath it. Both failures are
// silent: nothing errors, the repository just gets rewritten while someone is reading it.
//
// queue.blocksMovers is unexported, so this asserts against the exported signal that actually
// gates admission rather than against the predicate — which is the stronger test anyway.
func TestMaintenanceMutexHalvesAgree(t *testing.T) {
	pairs := []struct {
		kind queue.OpKind
		op   mover.Operation
	}{
		{queue.OpInit, mover.OpInit},
		{queue.OpForget, mover.OpForget},
		{queue.OpUnlock, mover.OpUnlock},
		{queue.OpPrune, mover.OpPrune},
		{queue.OpCheck, mover.OpCheck},
	}

	m := queue.NewManager(context.Background())
	defer m.Stop()

	for _, p := range pairs {
		repoKey := "repo-" + string(p.kind)
		block := make(chan struct{})
		entered := make(chan struct{})
		h, err := m.Enqueue(repoKey, p.kind, func(ctx context.Context) error {
			close(entered)
			<-block
			return nil
		})
		if err != nil {
			t.Fatalf("enqueue %s: %v", p.kind, err)
		}
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Fatalf("%s never started on the queue", p.kind)
		}

		readerSide := m.QuiescenceRequired(repoKey)
		writerSide := maintenanceOpBlocksMovers(p.op)
		if readerSide != writerSide {
			t.Errorf("mover mutex halves disagree for %s/%s: admission gate (QuiescenceRequired) = %v, "+
				"writer drain (maintenanceOpBlocksMovers) = %v — adr/0015 requires both or neither",
				p.kind, p.op, readerSide, writerSide)
		}

		close(block)
		if err := h.Wait(); err != nil {
			t.Fatalf("%s op: %v", p.kind, err)
		}
	}
}

// TestMaintenanceOpDeadlineSeparatesShortFromLong pins that prune and check do NOT inherit the
// ten-minute budget sized for forget and unlock. A prune of the shared cluster repository repacks
// real data and a sampled check re-downloads part of every pack; truncating either at ten minutes
// would kill it before it converges — every run, forever, leaving the repository permanently
// un-pruned while reporting a timeout each time.
func TestMaintenanceOpDeadlineSeparatesShortFromLong(t *testing.T) {
	short := []mover.Operation{mover.OpForget, mover.OpUnlock, mover.OpInit}
	long := []mover.Operation{mover.OpPrune, mover.OpCheck}

	for _, op := range short {
		if got := maintenanceOpDeadline(op); got != maintenanceJobDeadline {
			t.Errorf("maintenanceOpDeadline(%s) = %v, want the short budget %v", op, got, maintenanceJobDeadline)
		}
	}
	for _, op := range long {
		if got := maintenanceOpDeadline(op); got != maintenanceLongOpDeadline {
			t.Errorf("maintenanceOpDeadline(%s) = %v, want the long backstop %v", op, got, maintenanceLongOpDeadline)
		}
	}
	if maintenanceLongOpDeadline <= maintenanceJobDeadline {
		t.Fatalf("the long-op backstop (%v) must exceed the short-op budget (%v)",
			maintenanceLongOpDeadline, maintenanceJobDeadline)
	}
}
