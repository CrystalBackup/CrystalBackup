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

// The reaper's REPORTING contract, pinned outcome by outcome.
//
// The 0.6.5 defect these tests close was not a leak; it was a lie about a leak. On a production
// cluster the reaper logged "reaped leftover VolumeSnapshot" every 10 minutes, for the same three
// objects, for over 31 hours. It had reaped none of them: their deletion was blocked on
// external-snapshotter's bound-protection finalizers, so each object carried a deletionTimestamp and
// was going nowhere. The same binary's self-check reported them correctly the whole time
// (`leakIndicators: VolumeSnapshot — total 8, residual 8, oldestAgeHours 31`), so the operator was
// shipping two components that disagreed about whether a leak existed, and the one an administrator
// reads first was the one that was wrong.
//
// The rule the reaper had broken is the project's standing one: verify the artifact, not the job. A
// successful Delete call is an ACCEPTED REQUEST, not a completed deletion. So the three outcomes
// below are pinned separately, and — this is the part that must not rot — the STUCK case asserts the
// ABSENCE of a success claim, not merely the presence of a warning. A future change that starts
// reporting a stuck object as reaped again passes every "did it warn?" assertion and fails these.
//
// Fake-client tests, deliberately: the envtest suite runs without the external snapshot CRDs, and
// the fake client implements the finalizer semantics that are the whole subject here (a Delete on an
// object with finalizers sets deletionTimestamp and keeps the object).

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/metrics"
)

// boundProtectionFinalizer is the real finalizer that held the production objects. Written out in
// full rather than abbreviated: the actionable half of the fix is that the reaper NAMES the
// finalizer holding an object, and a test that matched a prefix would not notice it stopping.
const boundProtectionFinalizer = "snapshot.storage.kubernetes.io/volumesnapshot-bound-protection"

// --- log capture -------------------------------------------------------------------------------

// capturedLine is one log call: the message and its flattened key/value pairs.
type capturedLine struct {
	msg string
	kv  []any
}

// logCapture is a logr sink that keeps every line, so a test can assert on the WORDING the reaper
// chose. That is not over-specification here: the wording IS the defect. "reaped" and "deletion
// requested" describe two different states of the world, and the incident happened because one was
// printed for the other.
type logCapture struct {
	mu    sync.Mutex
	lines []capturedLine
}

func (c *logCapture) Init(logr.RuntimeInfo)          {}
func (c *logCapture) Enabled(int) bool               { return true }
func (c *logCapture) WithName(string) logr.LogSink   { return c }
func (c *logCapture) WithValues(...any) logr.LogSink { return c }

func (c *logCapture) Info(_ int, msg string, kv ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines = append(c.lines, capturedLine{msg: msg, kv: kv})
}

func (c *logCapture) Error(err error, msg string, kv ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		kv = append(kv, "error", err.Error())
	}
	c.lines = append(c.lines, capturedLine{msg: msg, kv: kv})
}

// claimsAReap reports whether ANY captured line claims a completed reap. The stuck and requested
// wordings deliberately avoid the past tense, so this is the single assertion that catches a
// regression into the original defect.
func (c *logCapture) claimsAReap() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, l := range c.lines {
		if strings.Contains(l.msg, "reaped") {
			return true
		}
	}
	return false
}

func (c *logCapture) contains(substr string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, l := range c.lines {
		if strings.Contains(l.msg, substr) {
			return true
		}
	}
	return false
}

// valueFor returns the first value logged under key, across all captured lines. Used to assert that
// the finalizers reach the log as data an administrator can act on, not just as prose.
func (c *logCapture) valueFor(key string) (any, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, l := range c.lines {
		for i := 0; i+1 < len(l.kv); i += 2 {
			if k, ok := l.kv[i].(string); ok && k == key {
				return l.kv[i+1], true
			}
		}
	}
	return nil, false
}

func captureLogs(t *testing.T) (context.Context, *logCapture) {
	t.Helper()
	sink := &logCapture{}
	return logf.IntoContext(context.Background(), logr.New(sink)), sink
}

// --- event capture -----------------------------------------------------------------------------

// capturedEvent is one Eventf call, kept whole so a test can assert both that it is a WARNING and
// that its note names the finalizer.
type capturedEvent struct {
	eventType string
	reason    string
	action    string
	note      string
	objName   string
}

type eventCapture struct {
	mu     sync.Mutex
	events []capturedEvent
}

func (e *eventCapture) Eventf(regarding runtime.Object, _ runtime.Object, eventtype, reason, action, note string, args ...any) {
	e.mu.Lock()
	defer e.mu.Unlock()
	name := ""
	if o, ok := regarding.(client.Object); ok {
		name = o.GetName()
	}
	e.events = append(e.events, capturedEvent{
		eventType: eventtype,
		reason:    reason,
		action:    action,
		// Rendered the way the apiserver will render it, so an assertion on the note is an assertion
		// on what a human reads in `kubectl describe`.
		note:    fmt.Sprintf(note, args...),
		objName: name,
	})
}

// all is every captured Event, unfiltered. A test asserting that NOTHING was emitted has to look at
// the whole slice: warnings(reason) would report "no events" for an Event that was emitted with the
// wrong reason or the wrong type, which is the case such a test most wants to catch.
func (e *eventCapture) all() []capturedEvent {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.events)
}

func (e *eventCapture) warnings(reason string) []capturedEvent {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []capturedEvent
	for _, ev := range e.events {
		if ev.eventType == corev1.EventTypeWarning && ev.reason == reason {
			out = append(out, ev)
		}
	}
	return out
}

// --- fixtures ----------------------------------------------------------------------------------

func honestyScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme(clientgo): %v", err)
	}
	if err := cbv1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme(cbv1): %v", err)
	}
	return scheme
}

// newOrphanVolumeSnapshot builds one exposure-labelled origin VolumeSnapshot whose owning Backup
// does not exist — a pure orphan, reap-eligible at MinAge 0 — with the given finalizers.
func newOrphanVolumeSnapshot(name string, finalizers ...string) *unstructured.Unstructured {
	vs := &unstructured.Unstructured{}
	vs.SetAPIVersion("snapshot.storage.k8s.io/v1")
	vs.SetKind("VolumeSnapshot")
	vs.SetNamespace("c-db")
	vs.SetName(name)
	// The fake client does not stamp creationTimestamp on unstructured objects, and a zero timestamp
	// reads as infinitely old — which would silently bypass the MinAge guard these tests rely on.
	vs.SetCreationTimestamp(metav1.Now())
	vs.SetLabels(exposureLabelsFor("honesty-run", "c-db", "data-db-1"))
	if len(finalizers) > 0 {
		vs.SetFinalizers(finalizers)
	}
	return vs
}

func getVolumeSnapshot(t *testing.T, c client.Client, name string) (*unstructured.Unstructured, bool) {
	t.Helper()
	u := &unstructured.Unstructured{}
	u.SetAPIVersion("snapshot.storage.k8s.io/v1")
	u.SetKind("VolumeSnapshot")
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "c-db", Name: name}, u); err != nil {
		return nil, false
	}
	return u, true
}

// --- the three outcomes ------------------------------------------------------------------------

// TestReapConfirmedGoneIsTheOnlyThingCalledAReaped: the happy path, and the baseline the other two
// are measured against. With no finalizer in the way the object really does disappear, the read-back
// says NotFound, and only then may the reaper use the word.
func TestReapConfirmedGoneIsTheOnlyThingCalledAReaped(t *testing.T) {
	scheme := honestyScheme(t)
	vs := newOrphanVolumeSnapshot("gone-snap")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(vs).Build()
	ctx, logs := captureLogs(t)
	rec := &eventCapture{}

	r := &OrphanReaper{Client: c, OperatorNamespace: reaperTestOperatorNS, MinAge: 0, Recorder: rec}
	tally := newReapTally()
	r.reapSnapshotObjects(ctx, time.Now(), tally)

	if _, present := getVolumeSnapshot(t, c, "gone-snap"); present {
		t.Fatal("the VolumeSnapshot survived a sweep that had nothing holding it")
	}
	if !logs.contains("confirmed gone") {
		t.Errorf("a completed reap must say it was CONFIRMED, not merely requested; lines: %+v", logs.lines)
	}
	if !logs.claimsAReap() {
		t.Error("a confirmed-gone object is the one case where the word \"reaped\" is earned, and it was not used")
	}
	if len(tally.stuck) != 0 {
		t.Errorf("a confirmed-gone object was tallied as stuck: %v", tally.stuck)
	}
	if got := rec.warnings(eventReasonReapStuck); len(got) != 0 {
		t.Errorf("a clean reap emitted %d stuck warning(s): %+v", len(got), got)
	}
}

// TestReapStuckOnFinalizerIsNeverReportedAsReaped is the incident, reproduced.
//
// The VolumeSnapshot carries external-snapshotter's bound-protection finalizer, exactly as the three
// production objects did. The DELETE is accepted; the object stays, with a deletionTimestamp. Every
// assertion here failed before this lot: the sweep logged a reap, raised nothing, and counted
// nothing, which is how eight residual objects stayed invisible for 31 hours next to a log that said
// they had been collected.
func TestReapStuckOnFinalizerIsNeverReportedAsReaped(t *testing.T) {
	scheme := honestyScheme(t)
	vs := newOrphanVolumeSnapshot("stuck-snap", boundProtectionFinalizer)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(vs).Build()
	ctx, logs := captureLogs(t)
	rec := &eventCapture{}

	r := &OrphanReaper{Client: c, OperatorNamespace: reaperTestOperatorNS, MinAge: 0, Recorder: rec}
	tally := newReapTally()
	r.reapSnapshotObjects(ctx, time.Now(), tally)

	// The premise: the object is still there, and Kubernetes is not going to remove it.
	got, present := getVolumeSnapshot(t, c, "stuck-snap")
	if !present {
		t.Fatal("the fixture no longer reproduces a finalizer-blocked deletion; this test proves nothing")
	}
	if got.GetDeletionTimestamp().IsZero() {
		t.Fatal("the fixture's deletion was never requested; this test proves nothing")
	}

	if logs.claimsAReap() {
		t.Error("the reaper reported a REAP for an object that is still there with a deletionTimestamp — " +
			"this is the 31-hour defect: a leak made invisible by the log that was supposed to reveal it")
	}
	if !logs.contains("STUCK") {
		t.Errorf("a blocked deletion was not reported as stuck; lines: %+v", logs.lines)
	}
	if v, ok := logs.valueFor("finalizers"); !ok || !strings.Contains(v.(string), boundProtectionFinalizer) {
		t.Errorf("the log does not name the finalizer holding the object (got %v) — that is the one "+
			"actionable detail, because only the finalizer's owner or an administrator can release it", v)
	}
	if n := tally.stuck[kindVolumeSnapshot]; n != 1 {
		t.Errorf("stuck tally for %s = %d, want 1 — the metric is what makes this visible without logs",
			kindVolumeSnapshot, n)
	}
	warnings := rec.warnings(eventReasonReapStuck)
	if len(warnings) != 1 {
		t.Fatalf("want exactly one Warning Event on the stuck object, got %d: %+v", len(warnings), warnings)
	}
	if warnings[0].objName != "stuck-snap" {
		t.Errorf("the Warning was raised on %q, not on the stuck object", warnings[0].objName)
	}
	if !strings.Contains(warnings[0].note, boundProtectionFinalizer) {
		t.Errorf("the Warning Event does not name the holding finalizer: %q", warnings[0].note)
	}
}

// TestStuckObjectIsNotRedeletedEverySweep pins the other half of the fix, and the reason the fix is
// not "keep trying": the production loop re-issued a DELETE on the same terminating objects every 10
// minutes for 31 hours. A finalizer is not waiting for a second request. So a sweep that finds a
// deletion already pending reports it and issues NOTHING — one read per sweep, no writes, no
// blocking, no unbounded retry — and it still reports it as stuck every time, because it still IS.
func TestStuckObjectIsNotRedeletedEverySweep(t *testing.T) {
	scheme := honestyScheme(t)
	vs := newOrphanVolumeSnapshot("sticky-snap", boundProtectionFinalizer)
	var deletes int
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(vs).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				deletes++
				return cl.Delete(ctx, obj, opts...)
			},
		}).Build()
	ctx, _ := captureLogs(t)

	r := &OrphanReaper{Client: c, OperatorNamespace: reaperTestOperatorNS, MinAge: 0}
	first := newReapTally()
	r.reapSnapshotObjects(ctx, time.Now(), first)
	second := newReapTally()
	r.reapSnapshotObjects(ctx, time.Now(), second)
	third := newReapTally()
	r.reapSnapshotObjects(ctx, time.Now(), third)

	if deletes != 1 {
		t.Errorf("the reaper issued %d DELETEs across three sweeps of one stuck object, want 1 — "+
			"re-asking cannot move a finalizer, and the repeated request is what produced the "+
			"repeated success claim", deletes)
	}
	for i, tally := range []*reapTally{first, second, third} {
		if tally.stuck[kindVolumeSnapshot] != 1 {
			t.Errorf("sweep %d reported %d stuck, want 1 — a stuck object must stay visible for as long "+
				"as it is stuck, not just on the sweep that first asked", i+1, tally.stuck[kindVolumeSnapshot])
		}
	}
}

// TestStuckContentStillGetsItsReclaimPolicyRestored guards the one thing "stop re-issuing the
// DELETE" could have broken.
//
// A dynamically-provisioned origin VolumeSnapshotContent owns its storage-side snapshot, and it only
// gets reclaimed if the object goes away with deletionPolicy=Delete. Such a content can be found
// ALREADY terminating and STILL Retain-parked (the crash window in exposer's reclaimOrigin: something
// else deleted it after the handover patch, with a finalizer holding it meanwhile). If the reaper
// skipped ALL work on a pending deletion, the object would eventually vanish with Retain and the
// backend snapshot would be orphaned in the bucket forever — a silent, billable leak, replacing a
// visible one. So the reclaim-policy restore runs on every sweep; only the DELETE is withheld.
func TestStuckContentStillGetsItsReclaimPolicyRestored(t *testing.T) {
	scheme := honestyScheme(t)

	vsc := &unstructured.Unstructured{}
	vsc.SetAPIVersion("snapshot.storage.k8s.io/v1")
	vsc.SetKind("VolumeSnapshotContent")
	vsc.SetName("stuck-dynamic-content")
	vsc.SetCreationTimestamp(metav1.Now())
	vsc.SetLabels(exposureLabelsFor("honesty-run", "c-db", "data-db-1"))
	vsc.SetFinalizers([]string{"snapshot.storage.kubernetes.io/volumesnapshotcontent-bound-protection"})
	if err := unstructured.SetNestedField(vsc.Object, "Retain", "spec", "deletionPolicy"); err != nil {
		t.Fatal(err)
	}
	// A dynamic content: it owns the backend snapshot (volumeHandle, not snapshotHandle).
	if err := unstructured.SetNestedField(vsc.Object, "csi-vol-1", "spec", "source", "volumeHandle"); err != nil {
		t.Fatal(err)
	}

	var deletes int
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(vsc).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				deletes++
				return cl.Delete(ctx, obj, opts...)
			},
		}).Build()
	ctx, _ := captureLogs(t)
	r := &OrphanReaper{Client: c, OperatorNamespace: reaperTestOperatorNS, MinAge: 0}

	// First sweep: requests the deletion (which sticks on the finalizer).
	r.reapSnapshotObjects(ctx, time.Now(), newReapTally())
	// Somebody re-parks it at Retain while it is terminating — the crash-window shape, forced here so
	// the second sweep has something to fix.
	got := &unstructured.Unstructured{}
	got.SetAPIVersion("snapshot.storage.k8s.io/v1")
	got.SetKind("VolumeSnapshotContent")
	if err := c.Get(ctx, client.ObjectKey{Name: "stuck-dynamic-content"}, got); err != nil {
		t.Fatalf("the content is gone; the fixture no longer reproduces a stuck content: %v", err)
	}
	if got.GetDeletionTimestamp().IsZero() {
		t.Fatal("the fixture's deletion was never requested; this test proves nothing")
	}
	if err := unstructured.SetNestedField(got.Object, "Retain", "spec", "deletionPolicy"); err != nil {
		t.Fatal(err)
	}
	if err := c.Update(ctx, got); err != nil {
		t.Fatalf("re-parking the terminating content at Retain: %v", err)
	}

	// Second sweep: the DELETE must not be re-issued, but the policy must be repaired.
	r.reapSnapshotObjects(ctx, time.Now(), newReapTally())

	after := &unstructured.Unstructured{}
	after.SetAPIVersion("snapshot.storage.k8s.io/v1")
	after.SetKind("VolumeSnapshotContent")
	if err := c.Get(ctx, client.ObjectKey{Name: "stuck-dynamic-content"}, after); err != nil {
		t.Fatalf("get the content after the second sweep: %v", err)
	}
	policy, _, err := unstructured.NestedString(after.Object, "spec", "deletionPolicy")
	if err != nil {
		t.Fatal(err)
	}
	if policy != "Delete" {
		t.Errorf("deletionPolicy = %q after a sweep over a stuck dynamic content, want Delete — "+
			"a Retain-parked content that vanishes when its finalizer clears takes its backend "+
			"snapshot permanently out of reach", policy)
	}
	if deletes != 1 {
		t.Errorf("the reaper issued %d DELETEs, want 1 — the policy repair must not drag the "+
			"withheld delete back with it", deletes)
	}
}

// TestReapRequestedButUnconfirmedIsNeitherSuccessNorLeak covers the third state, the one that is
// easy to get wrong in both directions.
//
// The DELETE is accepted and the object still reads back as present with NO deletionTimestamp. In
// production that is overwhelmingly a read that has not caught up yet, which is why it must not be
// shouted about — a Warning on every healthy delete is an alert that gets muted, and then the real
// stuck object goes unseen again. It must equally not be called a reap. So it is reported as a
// REQUEST, tallied as nothing, and left for the next sweep to resolve.
func TestReapRequestedButUnconfirmedIsNeitherSuccessNorLeak(t *testing.T) {
	scheme := honestyScheme(t)
	vs := newOrphanVolumeSnapshot("unconfirmed-snap")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(vs).
		WithInterceptorFuncs(interceptor.Funcs{
			// The apiserver accepted the deletion; nothing observable has happened yet. This is the
			// shape of a delete whose effect the reader has not seen.
			Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
				return nil
			},
		}).Build()
	ctx, logs := captureLogs(t)
	rec := &eventCapture{}

	r := &OrphanReaper{Client: c, OperatorNamespace: reaperTestOperatorNS, MinAge: 0, Recorder: rec}
	tally := newReapTally()
	r.reapSnapshotObjects(ctx, time.Now(), tally)

	if logs.claimsAReap() {
		t.Error("an unconfirmed deletion was reported as a completed reap — the exact overstatement " +
			"that hid eight residual objects for 31 hours")
	}
	if !logs.contains("REQUESTED") {
		t.Errorf("an accepted-but-unconfirmed deletion must be reported as a request; lines: %+v", logs.lines)
	}
	if len(tally.stuck) != 0 {
		t.Errorf("an unconfirmed deletion was counted as stuck %v — crying wolf on every delete whose "+
			"read-back has not caught up is how the stuck metric gets ignored", tally.stuck)
	}
	if got := rec.warnings(eventReasonReapStuck); len(got) != 0 {
		t.Errorf("an unconfirmed deletion raised %d Warning(s): %+v", len(got), got)
	}
}

// TestReapConfirmsGoneWhenASuccessorTookTheName: reaping by label can name an object that a later
// run has since recreated under the same name. A present-but-different UID means OUR object's
// deletion did complete, and reporting it as unconfirmed forever would keep a resolved reap
// permanently open.
func TestReapConfirmsGoneWhenASuccessorTookTheName(t *testing.T) {
	scheme := honestyScheme(t)
	stale := newOrphanVolumeSnapshot("recycled-snap")
	stale.SetUID("11111111-1111-1111-1111-111111111111")
	successor := newOrphanVolumeSnapshot("recycled-snap")
	successor.SetUID("22222222-2222-2222-2222-222222222222")

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(successor).Build()
	r := &OrphanReaper{Client: c, OperatorNamespace: reaperTestOperatorNS, MinAge: 0}

	outcome, _, err := r.confirmReap(context.Background(), stale)
	if err != nil {
		t.Fatalf("confirmReap: %v", err)
	}
	if outcome != reapConfirmedGone {
		t.Errorf("outcome = %v, want reapConfirmedGone: the name is taken by a different UID, so the "+
			"object the reaper deleted is gone", outcome)
	}
}

// TestStuckKindsAreAllPublished keeps this package's kind constants and the metric's enumerated
// label values in step. A kind named here and missing there is published once and never reset to
// zero, so its series would keep reporting a stuck object long after the deadlock cleared — a
// different lie, in the other direction.
func TestStuckKindsAreAllPublished(t *testing.T) {
	published := map[string]bool{}
	for _, k := range metrics.OrphanReapStuckKinds() {
		published[k] = true
	}
	for _, k := range []string{
		kindJob, kindPVC, kindSecret, kindPV,
		kindVolumeSnapshot, kindVolumeSnapshotContent,
		kindRoleBinding, kindClusterRoleBinding,
	} {
		if !published[k] {
			t.Errorf("kind %q is reported by the reaper but not enumerated by "+
				"metrics.OrphanReapStuckKinds — its series would never return to zero", k)
		}
	}
}
