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

// createMaintenanceJob (backup_maintenance.go) — the deterministic-name collision has three
// faces, and a validation lane hit the one AlreadyExists-tolerance gets wrong: op B queued
// behind op A adopted A's foreground-deleting Job, which vanished mid-poll and recorded a prune
// as Failed with "get maintenance job …: not found". These pins fix each face in place.

import (
	"context"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/CrystalBackup/CrystalBackup/internal/mover"
)

func graveTestJob(name string) *batchv1.Job {
	return &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Namespace: reaperTestOperatorNS, Name: name}}
}

func graveTestDeps(c client.Client) repoMaintenanceDeps {
	return repoMaintenanceDeps{Client: c, OperatorNamespace: reaperTestOperatorNS}
}

// TestCreateMaintenanceJobFreshName: no leftover — plain create.
func TestCreateMaintenanceJobFreshName(t *testing.T) {
	ctx := context.Background()
	c := newReaperClient(t)

	if err := createMaintenanceJob(ctx, graveTestDeps(c), mover.OpPrune, graveTestJob("dr-prune")); err != nil {
		t.Fatalf("createMaintenanceJob on a fresh name: %v", err)
	}
	var got batchv1.Job
	if err := c.Get(ctx, client.ObjectKey{Namespace: reaperTestOperatorNS, Name: "dr-prune"}, &got); err != nil {
		t.Fatalf("the job was not created: %v", err)
	}
}

// TestCreateMaintenanceJobAdoptsLiveLeftover: a LIVE same-name Job (a crashed previous run) is
// adopted, not recreated — the restart contract deterministic names exist for.
func TestCreateMaintenanceJobAdoptsLiveLeftover(t *testing.T) {
	ctx := context.Background()
	c := newReaperClient(t)

	leftover := graveTestJob("dr-prune")
	// The fake client does not mint UIDs; set one so adoption-vs-recreation is observable.
	leftover.UID = "uid-live-leftover"
	if err := c.Create(ctx, leftover); err != nil {
		t.Fatal(err)
	}
	uid := leftover.UID

	if err := createMaintenanceJob(ctx, graveTestDeps(c), mover.OpPrune, graveTestJob("dr-prune")); err != nil {
		t.Fatalf("createMaintenanceJob over a live leftover: %v", err)
	}
	var got batchv1.Job
	if err := c.Get(ctx, client.ObjectKey{Namespace: reaperTestOperatorNS, Name: "dr-prune"}, &got); err != nil {
		t.Fatal(err)
	}
	if got.UID != uid {
		t.Fatalf("the live leftover was recreated (UID changed) instead of adopted")
	}
}

// TestCreateMaintenanceJobNeverAdoptsAGrave: a TERMINATING same-name Job must not be adopted —
// the call waits (bounded by ctx) and, once the grave empties, creates a FRESH Job.
func TestCreateMaintenanceJobNeverAdoptsAGrave(t *testing.T) {
	ctx := context.Background()
	c := newReaperClient(t)

	// A finalizer keeps the deleted Job visible — exactly a foreground delete mid-collection.
	grave := graveTestJob("dr-prune")
	grave.Finalizers = []string{"crystalbackup.io/test-hold"}
	// The fake client does not mint UIDs; set one so adoption-vs-recreation is observable.
	grave.UID = "uid-grave"
	if err := c.Create(ctx, grave); err != nil {
		t.Fatal(err)
	}
	graveUID := grave.UID
	if err := c.Delete(ctx, grave); err != nil {
		t.Fatal(err)
	}

	// Phase 1 — the grave persists: the call must WAIT, not adopt; with an already-cancelled
	// context it reports the wait, naming what it was waiting for.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	err := createMaintenanceJob(cancelled, graveTestDeps(c), mover.OpPrune, graveTestJob("dr-prune"))
	if err == nil {
		t.Fatalf("createMaintenanceJob adopted (or recreated over) a terminating grave")
	}
	if !strings.Contains(err.Error(), "finish deleting") {
		t.Fatalf("error should say it was waiting out the grave, got: %v", err)
	}

	// Phase 2 — the grave empties (finalizer released): the retry creates a FRESH Job.
	var dying batchv1.Job
	if err := c.Get(ctx, client.ObjectKey{Namespace: reaperTestOperatorNS, Name: "dr-prune"}, &dying); err != nil {
		t.Fatal(err)
	}
	dying.Finalizers = nil
	if err := c.Update(ctx, &dying); err != nil {
		t.Fatal(err)
	}

	bounded, cancelBounded := context.WithTimeout(ctx, 30*time.Second)
	defer cancelBounded()
	if err := createMaintenanceJob(bounded, graveTestDeps(c), mover.OpPrune, graveTestJob("dr-prune")); err != nil {
		t.Fatalf("createMaintenanceJob after the grave emptied: %v", err)
	}
	var fresh batchv1.Job
	if err := c.Get(ctx, client.ObjectKey{Namespace: reaperTestOperatorNS, Name: "dr-prune"}, &fresh); err != nil {
		t.Fatal(err)
	}
	if fresh.UID == graveUID {
		t.Fatalf("the new Job carries the grave's UID — it was adopted, not recreated")
	}
	if !fresh.DeletionTimestamp.IsZero() {
		t.Fatalf("the new Job is itself terminating")
	}
}
