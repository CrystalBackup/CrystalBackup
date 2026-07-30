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

package tracing

import (
	"context"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

const testUID = "6f1c2b8e-0d3a-4a11-9f2e-7c5d4b3a2190"

// TestAnchorIsDerivedNotRemembered is the property the whole cross-reconcile design rests on: two
// independent computations of an object's anchor — which in production are two different
// reconciles, often in two different PROCESSES after a leader-election handover — agree on the
// trace id and on the root span id, having exchanged nothing.
func TestAnchorIsDerivedNotRemembered(t *testing.T) {
	withRecorder(t)

	first := AnchorFor(testUID)
	second := AnchorFor(testUID)

	if !first.Valid() || !second.Valid() {
		t.Fatal("AnchorFor returned an invalid anchor while tracing is active")
	}
	if first.sc.TraceID() != second.sc.TraceID() {
		t.Errorf("trace ids differ across derivations: %s != %s", first.sc.TraceID(), second.sc.TraceID())
	}
	if first.sc.SpanID() != second.sc.SpanID() {
		t.Errorf("root span ids differ across derivations: %s != %s", first.sc.SpanID(), second.sc.SpanID())
	}

	// A different object — a Backup recreated under the same name, say — is a different trace.
	other := AnchorFor("11111111-2222-3333-4444-555555555555")
	if other.sc.TraceID() == first.sc.TraceID() {
		t.Error("two different UIDs derived the same trace id")
	}
	// The root span id is not simply a slice of the trace id: the two are hashed under different
	// domain prefixes, so that adding a third derivation later cannot collide with either.
	if first.sc.TraceID().String()[:16] == first.sc.SpanID().String() {
		t.Error("the root span id is the trace id's prefix; the domain separation is not working")
	}
}

// TestRootSpanAdoptsTheChildrenEmittedBeforeIt reproduces the ordering that makes this design
// unusual and that a naive implementation gets wrong: the children are emitted DURING the backup,
// the root only at the end, and the root must still come out carrying the exact span id those
// children already named as their parent.
func TestRootSpanAdoptsTheChildrenEmittedBeforeIt(t *testing.T) {
	rec := withRecorder(t)
	ctx := context.Background()

	a := AnchorFor(testUID)
	start := time.Now().Add(-10 * time.Minute)

	// Reconcile #4, say: a volume's exposure finished.
	a.EmitStep(ctx, StepPVC(StepExpose, "data-postgres-0"), "expose",
		start.Add(time.Minute), start.Add(2*time.Minute), nil,
		StringAttr(AttrPVC, "data-postgres-0")...)
	// Reconcile #37, much later: its mover finished.
	a.EmitStep(ctx, StepPVC(StepMover, "data-postgres-0"), "mover",
		start.Add(2*time.Minute), start.Add(9*time.Minute), nil,
		StringAttr(AttrPVC, "data-postgres-0")...)
	// The final reconcile: the Backup went terminal and its root is emitted last.
	a.EmitRoot(ctx, "backup", start, start.Add(10*time.Minute), nil, nil,
		StringAttr(AttrNamespace, "c-team-x")...)

	spans := rec.Ended()
	if len(spans) != 3 {
		t.Fatalf("recorded %d spans, want 3", len(spans))
	}

	byName := map[string]sdktrace.ReadOnlySpan{}
	for _, s := range spans {
		byName[s.Name()] = s
	}
	root, ok := byName["backup"]
	if !ok {
		t.Fatal("no root span named backup was recorded")
	}
	if root.Parent().IsValid() {
		t.Errorf("the backup span has parent %s; it must be a root", root.Parent().SpanID())
	}
	if root.SpanContext().SpanID() != a.sc.SpanID() {
		t.Errorf("root span id = %s, want the derived %s — the children already point at the derived one",
			root.SpanContext().SpanID(), a.sc.SpanID())
	}

	for _, name := range []string{"expose", "mover"} {
		child, ok := byName[name]
		if !ok {
			t.Fatalf("no span named %s was recorded", name)
		}
		if child.Parent().SpanID() != root.SpanContext().SpanID() {
			t.Errorf("%s span's parent = %s, want the root's %s",
				name, child.Parent().SpanID(), root.SpanContext().SpanID())
		}
		if child.SpanContext().TraceID() != root.SpanContext().TraceID() {
			t.Errorf("%s is in trace %s, the root is in %s", name,
				child.SpanContext().TraceID(), root.SpanContext().TraceID())
		}
		if child.SpanContext().SpanID() == root.SpanContext().SpanID() {
			t.Errorf("%s took the root's span id; the pin leaked through the context", name)
		}
	}
	// The two children are distinct spans, not one id fought over twice.
	if byName["expose"].SpanContext().SpanID() == byName["mover"].SpanContext().SpanID() {
		t.Error("expose and mover share a span id")
	}
}

// TestStepContextMatchesTheSpanEmittedLater pins the agreement the mover propagation depends on:
// the TRACEPARENT written into a Job's pod template at CREATION time names the very span id the
// `mover` span is emitted with when the Job FINISHES, minutes or hours later.
func TestStepContextMatchesTheSpanEmittedLater(t *testing.T) {
	rec := withRecorder(t)

	a := AnchorFor(testUID)
	step := StepPVC(StepMover, "data-postgres-0")

	// What the operator injects when it creates the Job.
	injected := trace.SpanContextFromContext(a.StepContext(context.Background(), step))
	if !injected.IsValid() {
		t.Fatal("StepContext produced an invalid span context")
	}
	if !injected.IsSampled() {
		t.Error("the injected span context is not sampled; the shim's spans would be dropped")
	}

	// What is emitted once the Job is terminal.
	a.EmitStep(context.Background(), step, "mover", time.Now().Add(-time.Minute), time.Now(), nil)

	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("recorded %d spans, want 1", len(spans))
	}
	if got := spans[0].SpanContext().SpanID(); got != injected.SpanID() {
		t.Errorf("the mover span was emitted as %s but the Job was told its parent is %s; "+
			"the shim's restic spans would dangle", got, injected.SpanID())
	}
	if got := spans[0].SpanContext().TraceID(); got != injected.TraceID() {
		t.Errorf("trace id mismatch: emitted %s, injected %s", got, injected.TraceID())
	}
}

// TestJobEnvCarriesTheStepAndTheMoversOwnServiceName checks the shape of what lands in the pod
// template, including the one variable that must NOT be inherited.
func TestJobEnvCarriesTheStepAndTheMoversOwnServiceName(t *testing.T) {
	withRecorder(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://collector.observability:4317")
	t.Setenv("OTEL_SERVICE_NAME", ServiceOperator)

	a := AnchorFor(testUID)
	step := StepPVC(StepMover, "data-postgres-0")
	env := JobEnv(a.StepContext(context.Background(), step))

	if env[EnvTraceparent] == "" {
		t.Fatal("JobEnv produced no TRACEPARENT")
	}
	if got := env["OTEL_SERVICE_NAME"]; got != ServiceMover {
		t.Errorf("OTEL_SERVICE_NAME = %q, want %q — a mover must not report as the operator", got, ServiceMover)
	}
	if got := env["OTEL_EXPORTER_OTLP_ENDPOINT"]; got != "http://collector.observability:4317" {
		t.Errorf("OTEL_EXPORTER_OTLP_ENDPOINT = %q, want the operator's own endpoint forwarded", got)
	}

	// Round-trip: the shim rebuilds exactly the span context the operator injected.
	t.Setenv(EnvTraceparent, env[EnvTraceparent])
	got := trace.SpanContextFromContext(FromEnv(context.Background()))
	want := trace.SpanContextFromContext(a.StepContext(context.Background(), step))
	if got.TraceID() != want.TraceID() || got.SpanID() != want.SpanID() {
		t.Errorf("round trip lost the context: got %s/%s, want %s/%s",
			got.TraceID(), got.SpanID(), want.TraceID(), want.SpanID())
	}
	if !got.IsRemote() {
		t.Error("the extracted parent is not marked remote")
	}
}

// TestFromEnvWithoutTraceparent covers the ordinary case for a mover Job created by an untraced
// operator: nothing to extract, and no attempt to invent a parent.
func TestFromEnvWithoutTraceparent(t *testing.T) {
	t.Setenv(EnvTraceparent, "")
	t.Setenv("traceparent", "")
	if sc := trace.SpanContextFromContext(FromEnv(context.Background())); sc.IsValid() {
		t.Errorf("FromEnv invented a span context %s with no TRACEPARENT set", sc.SpanID())
	}
}

// TestClampWindowSurvivesSkewedTimestamps. Every span in the operator is timed from timestamps
// written by different actors — the API server, the Job controller, a CSI driver — at
// second granularity. A step that legitimately took 300ms routinely comes back looking as though
// it ended before it began, and that must degrade to a zero-length span, never to a negative one
// (which backends variously reject or render as nonsense) and never to a dropped span.
func TestClampWindowSurvivesSkewedTimestamps(t *testing.T) {
	rec := withRecorder(t)
	a := AnchorFor(testUID)

	now := time.Now()
	a.EmitStep(context.Background(), StepManifests, "manifests", now, now.Add(-2*time.Second), nil)

	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("an inverted window dropped the span: recorded %d, want 1", len(spans))
	}
	if spans[0].EndTime().Before(spans[0].StartTime()) {
		t.Errorf("emitted a negative-duration span: %s -> %s", spans[0].StartTime(), spans[0].EndTime())
	}
}

// TestEmitStepRecordsAnError checks that a failed step is visibly failed in the trace, carrying
// the same error the object's status does.
func TestEmitStepRecordsAnError(t *testing.T) {
	rec := withRecorder(t)
	a := AnchorFor(testUID)

	a.EmitStep(context.Background(), StepHooksPre, "hooks.pre", time.Now(), time.Now(), errBoom)

	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("recorded %d spans, want 1", len(spans))
	}
	if spans[0].Status().Code.String() != "Error" {
		t.Errorf("span status = %s, want Error", spans[0].Status().Code)
	}
	if len(spans[0].Events()) == 0 {
		t.Error("the error was not recorded as a span event")
	}
}

// TestRootSpanLinksRatherThanParentsTheRun pins the cluster-plane fan-out shape (§5): a
// per-namespace Backup root LINKS to its ClusterBackup run. Parenting would put two hundred
// namespaces' worth of spans into a single trace that no backend renders usefully.
func TestRootSpanLinksRatherThanParentsTheRun(t *testing.T) {
	rec := withRecorder(t)

	run := AnchorFor("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
	link, ok := run.Link()
	if !ok {
		t.Fatal("Link() refused on a valid anchor")
	}

	backup := AnchorFor(testUID)
	backup.EmitRoot(context.Background(), "backup", time.Now().Add(-time.Minute), time.Now(), nil,
		[]trace.Link{link})

	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("recorded %d spans, want 1", len(spans))
	}
	s := spans[0]
	if s.Parent().IsValid() {
		t.Error("the Backup root is PARENTED to the run; it must only be linked")
	}
	if s.SpanContext().TraceID() == run.sc.TraceID() {
		t.Error("the Backup root shares the run's trace; the fan-out must produce one trace per namespace")
	}
	if len(s.Links()) != 1 {
		t.Fatalf("the Backup root carries %d links, want 1", len(s.Links()))
	}
	if s.Links()[0].SpanContext.SpanID() != run.sc.SpanID() {
		t.Errorf("the link points at %s, want the run's root %s",
			s.Links()[0].SpanContext.SpanID(), run.sc.SpanID())
	}
}

// TestStringAttrOmitsEmptyValues. An absent attribute and an empty one are different claims, and
// a trace search for "backups with no tenant" must not match every span that merely had none to
// report.
func TestStringAttrOmitsEmptyValues(t *testing.T) {
	if got := StringAttr(AttrTenant, ""); got != nil {
		t.Errorf("StringAttr with an empty value returned %v, want nil", got)
	}
	if got := StringAttr(AttrTenant, "team-x"); len(got) != 1 || got[0].Value.AsString() != "team-x" {
		t.Errorf("StringAttr = %v, want one team-x attribute", got)
	}
}
