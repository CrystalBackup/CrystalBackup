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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
)

// ---------------------------------------------------------------------------
// A BOUND THAT COULD NOT BE REACHED — found while sweeping this controller for the errored-pass class,
// and NOT an instance of it (ClusterErasure has none: every exit either writes status itself through
// park/fail/complete, or has computed nothing).
//
// The Immutable branch of Reconcile wanted an HOURLY re-check: an erasure blocked by S3 Object Lock
// stays blocked until the lock expires, which is weeks. It asked for that by calling park and then
// overriding park's Result — but park always answers {RequeueAfter: erasureRequeueInterval}, never a
// zero Result, so the `!res.IsZero()` guard in front of the override returned park's FIFTEEN SECONDS
// on every pass and the hour was dead code. Two consequences, and the second is the one an operator
// would have noticed:
//
//   - the location was re-read four times a minute, for weeks, for a decision that cannot change
//     without a bucket-policy edit;
//   - the Warning Event was emitted on every one of those passes, because it sat before the park.
//     Four Warnings a minute for weeks, on the most sensitive compliance path in the product, burying
//     every event somebody could act on.
//
// The bound being documented and unreachable is the part worth a test rather than a patch note: the
// next reader believes the cadence is an hour, and nothing in the code or the tests disagreed with
// them.
// ---------------------------------------------------------------------------

// TestImmutableErasureRechecksHourlyAndWarnsOnce pins both halves of the fix.
//
// Mutations that must turn this red: calling park instead of parkAt in the Immutable branch (the
// cadence silently returns to fifteen seconds); and moving the Recorder.Eventf back outside the
// phase-transition guard (the second pass emits a second Warning).
func TestImmutableErasureRechecksHourlyAndWarnsOnce(t *testing.T) {
	ctx := context.Background()
	const (
		erasure  = "gdpr-erp"
		location = "erp-immutable-loc"
		tenant   = "acme"
	)

	s := aggregateScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(
			&cbv1.ClusterBackupLocation{
				ObjectMeta: metav1.ObjectMeta{Name: location},
				Spec: cbv1.ClusterBackupLocationSpec{
					Mode:      cbv1.LocationModeImmutable,
					ClusterID: "erp-cluster",
				},
			},
			&cbv1.ClusterErasure{
				ObjectMeta: metav1.ObjectMeta{Name: erasure},
				Spec: cbv1.ClusterErasureSpec{
					LocationRef: cbv1.LocalObjectReference{Name: location},
					Target:      cbv1.ErasureTarget{Tenant: tenant},
					// The typed confirmation (R23): without it the erasure parks in AwaitingConfirmation
					// before the Immutable check is ever reached, and this test would pass vacuously.
					Confirmation: tenant,
				},
			},
		).
		WithStatusSubresource(&cbv1.ClusterErasure{}).
		Build()

	// A buffered fake recorder: what landed in it IS the assertion. inflight is left nil — the
	// Immutable branch returns long before the queue is reached, which is the whole point of it.
	rec := events.NewFakeRecorder(16)
	r := &ClusterErasureReconciler{Client: c, Scheme: s, Recorder: rec}

	drain := func() []string {
		var out []string
		for {
			select {
			case e := <-rec.Events:
				out = append(out, e)
			default:
				return out
			}
		}
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: erasure}}

	res, err := r.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if res.RequeueAfter != erasureBlockedRecheck {
		t.Errorf("RequeueAfter = %v, want the documented %v: an erasure blocked by an Object Lock cannot "+
			"become unblocked without a bucket-policy edit, and re-reading the location every %v achieves "+
			"nothing but load", res.RequeueAfter, erasureBlockedRecheck, res.RequeueAfter)
	}

	var er cbv1.ClusterErasure
	if err := c.Get(ctx, crclient.ObjectKey{Name: erasure}, &er); err != nil {
		t.Fatalf("get erasure: %v", err)
	}
	if er.Status.Phase != erasurePhaseBlocked {
		t.Fatalf("phase = %q, want %q", er.Status.Phase, erasurePhaseBlocked)
	}
	if first := drain(); len(first) != 1 {
		t.Fatalf("first pass emitted %d event(s) (%v), want exactly one Warning", len(first), first)
	}

	// The second pass finds the erasure already Blocked. It must re-check on the same slow cadence and
	// say NOTHING new: the situation has not changed, and an unchanged situation announced again is how
	// an event stream stops being readable.
	res, err = r.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if res.RequeueAfter != erasureBlockedRecheck {
		t.Errorf("second pass RequeueAfter = %v, want %v", res.RequeueAfter, erasureBlockedRecheck)
	}
	if second := drain(); len(second) != 0 {
		t.Errorf("second pass emitted %d event(s) (%v): the Warning belongs to the TRANSITION into "+
			"Blocked, and this erasure will sit here for weeks", len(second), second)
	}
}
