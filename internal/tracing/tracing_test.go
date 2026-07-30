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
	"errors"
	"runtime"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// clearOTelEnv unsets every OTEL_* variable for the duration of a test, so a developer who
// happens to run the suite with a collector configured in their shell gets the same verdict CI
// does. t.Setenv restores them at test end and marks the test as non-parallel.
func clearOTelEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		envEndpoint, envTracesEndpoint, envSDKDisabled, envProtocol, envTracesProtocol,
		"OTEL_SERVICE_NAME", "OTEL_RESOURCE_ATTRIBUTES", "OTEL_TRACES_SAMPLER",
	} {
		// Setenv to empty rather than Unsetenv: every read in this package treats an empty
		// value as absent (os.Getenv cannot tell the two apart anyway), and t.Setenv is the
		// only one of the two that restores the previous value at test end.
		t.Setenv(k, "")
	}
}

// TestNoOpWithoutOTelEnv is the load-bearing test of this lot.
//
// The overwhelming majority of CrystalBackup installations will never run an OTLP collector, and
// the design promise is that those installations pay NOTHING: no exporter, no background
// goroutine, no connection attempt, no recorded span. Each of those is asserted separately below,
// because they fail independently — an exporter constructed with default settings would satisfy
// "no spans recorded" while quietly dialling localhost:4317 forever on a goroutine of its own,
// which is precisely the failure this test exists to make impossible.
func TestNoOpWithoutOTelEnv(t *testing.T) {
	clearOTelEnv(t)
	restoreGlobals(t)

	// Settle whatever the runtime is doing before the count is taken; a stray goroutine from an
	// earlier test finishing here would otherwise be blamed on Init.
	settle()
	before := runtime.NumGoroutine()

	shutdown, err := Init(context.Background(), ServiceOperator)
	if err != nil {
		t.Fatalf("Init with no OTEL_* env returned an error: %v", err)
	}
	if shutdown == nil {
		t.Fatal("Init returned a nil shutdown; it must always be callable")
	}

	if Active() {
		t.Error("Active() is true with no OTEL_* env: an SDK provider was installed when none was configured")
	}

	// No SDK provider installed: the global is still the API's built-in no-op. A *sdktrace.TracerProvider
	// here would mean an exporter exists, whatever it is pointed at.
	tp := otel.GetTracerProvider()
	if _, isSDK := tp.(*sdktrace.TracerProvider); isSDK {
		t.Error("an SDK TracerProvider is installed with no OTEL_* env; it must stay the no-op provider")
	}
	// And the spans it hands out record nothing. Asserting on the concrete type is not enough:
	// otel.GetTracerProvider returns a delegating wrapper, and what matters is what the delegate
	// does, not what it is called.
	_, probe := tp.Tracer("probe").Start(context.Background(), "probe")
	if probe.IsRecording() {
		t.Error("a span started with no OTEL_* env is recording; something is collecting spans")
	}
	probe.End()

	// No export goroutine. A batch span processor runs one; an OTLP gRPC exporter's connection
	// management runs more. Either would show up here.
	settle()
	if after := runtime.NumGoroutine(); after > before {
		t.Errorf("Init started %d goroutine(s) with no OTEL_* env (%d -> %d); an unconfigured "+
			"install must start none", after-before, before, after)
	}

	// Nothing an emit site does records anything, and nothing panics doing it.
	a := AnchorFor("11111111-2222-3333-4444-555555555555")
	if a.Valid() {
		t.Error("AnchorFor returned a valid anchor while tracing is inactive")
	}
	if got := a.TraceID(); got != "" {
		t.Errorf("TraceID() = %q while inactive, want empty so the log key stays ABSENT, not blank", got)
	}
	ctx := a.Context(context.Background())
	if trace.SpanContextFromContext(ctx).IsValid() {
		t.Error("Anchor.Context installed a span context while tracing is inactive")
	}
	a.EmitRoot(ctx, "backup", time.Now().Add(-time.Minute), time.Now(), nil, nil)
	a.EmitStep(ctx, StepManifests, "manifests", time.Now(), time.Now(), nil)

	// And nothing is injected into a mover Job, so its pod spec is byte-identical to the one an
	// operator without this lot would have produced.
	if env := JobEnv(ctx); env != nil {
		t.Errorf("JobEnv returned %v while inactive; a mover Job spec must be unchanged", env)
	}

	if err := shutdown(context.Background()); err != nil {
		t.Errorf("no-op shutdown returned %v, want nil", err)
	}
}

// TestSDKDisabledBeatsAConfiguredEndpoint pins OTEL_SDK_DISABLED as the kill switch: it wins over
// an endpoint that is present, which is the only reason to reach for it.
func TestSDKDisabledBeatsAConfiguredEndpoint(t *testing.T) {
	clearOTelEnv(t)
	restoreGlobals(t)
	t.Setenv(envEndpoint, "http://collector.invalid:4317")
	t.Setenv(envSDKDisabled, "TRUE") // case-insensitive per the specification

	if _, err := Init(context.Background(), ServiceOperator); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if Active() {
		t.Error("tracing is active despite OTEL_SDK_DISABLED=TRUE")
	}
}

// BenchmarkInactiveEmit measures what an unconfigured install pays at a span emit site. The
// helpers short-circuit on an atomic load before touching their arguments, so the answer should
// be a couple of nanoseconds and zero allocations — see the package doc.
func BenchmarkInactiveEmit(b *testing.B) {
	active.Store(false)
	a := AnchorFor("11111111-2222-3333-4444-555555555555")
	ctx := context.Background()
	start := time.Now().Add(-time.Minute)
	end := time.Now()

	b.ReportAllocs()
	for b.Loop() {
		a.EmitStep(ctx, StepMover, "mover", start, end, nil,
			attribute.String(AttrPVC, "data-postgres-0"))
	}
}

// BenchmarkInactiveGuardedEmit measures what the REAL emit sites cost, which is less than
// BenchmarkInactiveEmit above: the controllers check Anchor.Valid() before building an attribute
// slice, because building one to hand to a function that will discard it is the only cost this
// package cannot eliminate from the inside (a variadic call allocates at the CALLER).
func BenchmarkInactiveGuardedEmit(b *testing.B) {
	active.Store(false)
	a := AnchorFor("11111111-2222-3333-4444-555555555555")
	ctx := context.Background()
	start := time.Now().Add(-time.Minute)
	end := time.Now()

	b.ReportAllocs()
	for b.Loop() {
		if !a.Valid() {
			continue
		}
		a.EmitStep(ctx, StepMover, "mover", start, end, nil,
			attribute.String(AttrPVC, "data-postgres-0"))
	}
}

// BenchmarkInactiveAnchorFor measures the other half: the per-reconcile cost of asking for an
// anchor at all. It must not hash anything when tracing is off.
func BenchmarkInactiveAnchorFor(b *testing.B) {
	active.Store(false)
	b.ReportAllocs()
	for b.Loop() {
		_ = AnchorFor("11111111-2222-3333-4444-555555555555")
	}
}

// withRecorder installs a real SDK provider writing into an in-memory exporter, so the remaining
// tests can assert on the spans that were actually produced.
func withRecorder(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	restoreGlobals(t)

	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(rec),
		sdktrace.WithIDGenerator(pinnedIDGenerator{}),
	)
	otel.SetTracerProvider(tp)
	setPropagator()
	active.Store(true)
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	return rec
}

// restoreGlobals puts the process-global tracer provider, propagator and active flag back after a
// test. They are global by the OTel API's design, so a test that sets them must undo it or the
// next test inherits a provider it never asked for.
func restoreGlobals(t *testing.T) {
	t.Helper()
	prevTP := otel.GetTracerProvider()
	prevProp := otel.GetTextMapPropagator()
	prevActive := active.Load()
	t.Cleanup(func() {
		// Only restore what actually changed: the OTel globals log a warning when set to the
		// value they already hold, and a suite that trips it on every test buries real output.
		if otel.GetTracerProvider() != prevTP {
			otel.SetTracerProvider(prevTP)
		}
		if otel.GetTextMapPropagator() != prevProp {
			otel.SetTextMapPropagator(prevProp)
		}
		active.Store(prevActive)
	})
}

// settle gives finishing goroutines a chance to exit before a count is taken.
func settle() {
	for range 3 {
		runtime.Gosched()
		time.Sleep(time.Millisecond)
	}
}

var errBoom = errors.New("boom")
