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

// Package tracing is CrystalBackup's OpenTelemetry integration: the tracer provider, the
// deterministic span identities that let one Backup's trace survive a hundred reconciles, and the
// W3C context propagation into mover Jobs (spec/05-observability.md §5).
//
// # Configuration is EXCLUSIVELY environmental
//
// There is no Helm value, no flag and no API field that turns tracing on. The only inputs are the
// standard OTEL_* variables — OTEL_EXPORTER_OTLP_ENDPOINT, OTEL_SERVICE_NAME,
// OTEL_TRACES_SAMPLER, OTEL_RESOURCE_ATTRIBUTES, OTEL_SDK_DISABLED and the rest, all read by the
// SDK itself. That is not minimalism for its own sake: it means an operator who already runs a
// collector configures CrystalBackup the same way they configure everything else, and an operator
// who does not has nothing to discover, nothing to leave misconfigured, and nothing to turn off.
//
// # Absent configuration is a HARD no-op, not a quiet one
//
// The overwhelming majority of installations will never have an OTLP collector, and they must pay
// neither the cost nor the risk of one. So when no endpoint is configured, [Init] installs NO SDK
// tracer provider at all: the global provider stays the API's built-in no-op, no exporter is
// constructed, no batch-processor goroutine is started, no connection is dialled, and Shutdown is
// a function that returns nil.
//
// It goes one step further than that. Every helper in this package short-circuits on the [Active]
// flag BEFORE doing any work — no SHA-256 of an object UID, no attribute slice, no SpanContext.
// A no-op tracer would already discard the span, but it would not save the arguments' cost, and
// the emit sites here are on the reconcile hot path. TestNoOpWithoutEnv and BenchmarkInactive pin
// both properties.
//
// # Why every span here is emitted AFTER the fact
//
// An operator does not run a backup inside one function call. It observes an object, takes one
// step, writes status and returns; the next reconcile — possibly in a different process, after a
// leader-election handover — picks the object back up. A live trace.Span held across that is a
// span held in a map for hours, lost on restart (never ended, therefore never exported, leaving
// every child orphaned), and one more thing that leaks.
//
// So no span in the operator is ever "open". Each one is emitted at the moment its outcome
// becomes durable, with explicit start and end timestamps read back from cluster state that was
// already there — an object's creationTimestamp, a VolumeSnapshot's cut time, a Job's
// completionTime. What holds the tree together instead of process memory is [Anchor]: span
// identities DERIVED from the object's UID, so any process, at any time, computes the same trace
// ID and the same parent span ID without having been told. See anchor.go.
package tracing

import (
	"context"
	"os"
	"strings"
	"sync/atomic"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.30.0"
	"go.opentelemetry.io/otel/trace"
)

// The two service names spec/05-observability.md §5 fixes. They are DEFAULTS: OTEL_SERVICE_NAME
// overrides either one, because a fleet that already has a naming convention should keep it.
const (
	// ServiceOperator names the long-lived controller-manager process.
	ServiceOperator = "crystal-backup-operator"
	// ServiceMover names the short-lived mover Job shim. A distinct service, not a distinct
	// instance: its spans come from a different binary with a different lifetime, and merging
	// the two would make the operator's latency histograms include restic's.
	ServiceMover = "crystal-backup-mover"
)

// The environment this package reads DIRECTLY. Everything else (sampler, resource attributes,
// headers, timeouts, TLS) is read by the SDK from its own standard variables and is deliberately
// not restated here — restating one would be a second place for it to drift.
const (
	envEndpoint       = "OTEL_EXPORTER_OTLP_ENDPOINT"
	envTracesEndpoint = "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"
	envSDKDisabled    = "OTEL_SDK_DISABLED"
	envProtocol       = "OTEL_EXPORTER_OTLP_PROTOCOL"
	envTracesProtocol = "OTEL_EXPORTER_OTLP_TRACES_PROTOCOL"
)

// active reports whether an SDK tracer provider is installed. It is the guard every helper in
// this package checks first, so that an unconfigured install pays a single atomic load per emit
// site rather than the cost of building a span's worth of arguments for a provider that will
// throw them away.
var active atomic.Bool

// Active reports whether tracing is on: an SDK provider was installed by [Init] and spans are
// really being recorded. Callers use it to skip work that only exists to feed a span — resolving
// a node name, hashing a UID, reading a Job's timestamps back.
func Active() bool { return active.Load() }

// ShutdownFunc flushes and stops the exporter. It is always non-nil, and is a no-op returning nil
// when tracing was never activated.
type ShutdownFunc func(context.Context) error

// Init installs the tracer provider for one process, using defaultServiceName unless
// OTEL_SERVICE_NAME says otherwise, and returns the shutdown to call on the way out.
//
// It returns (no-op, nil) — never an error — when tracing is not configured, which is the common
// case. It returns an error only when tracing IS configured and could not be brought up: an
// operator who asked for a collector and did not get one must find out at startup, not by
// noticing empty dashboards a week later.
//
// The caller MUST invoke the returned shutdown with a BOUNDED context on the way out. A batch
// span processor's ForceFlush blocks on the collector, and a collector that has gone away would
// otherwise turn an orderly SIGTERM into a kill — see cmd/main.go.
func Init(ctx context.Context, defaultServiceName string) (ShutdownFunc, error) {
	noop := func(context.Context) error { return nil }

	// OTEL_SDK_DISABLED is the specified kill switch and outranks a configured endpoint: it is
	// what an operator reaches for to silence telemetry without editing the rest of the config.
	if strings.EqualFold(strings.TrimSpace(os.Getenv(envSDKDisabled)), "true") {
		return noop, nil
	}
	// No endpoint, no tracing. Checked before anything is constructed, because the point of this
	// branch is that nothing IS constructed: the OTLP exporters happily default to
	// localhost:4317/4318, so leaving them to their defaults would have every unconfigured
	// operator in the world dialling a collector that is not there, retrying forever, on a
	// goroutine nobody asked for.
	if os.Getenv(envEndpoint) == "" && os.Getenv(envTracesEndpoint) == "" {
		return noop, nil
	}

	res, err := buildResource(ctx, defaultServiceName)
	if err != nil {
		return noop, err
	}
	exp, err := buildExporter(ctx)
	if err != nil {
		return noop, err
	}

	// No WithSampler: NewTracerProvider reads OTEL_TRACES_SAMPLER / OTEL_TRACES_SAMPLER_ARG
	// itself, and passing one here would OVERRIDE the environment this package exists to obey.
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
		// The pinned generator is what lets a span be emitted with an identity computed from an
		// object's UID rather than from randomness — the mechanism the whole cross-reconcile
		// design rests on. See idgen.go.
		sdktrace.WithIDGenerator(pinnedIDGenerator{}),
	)
	otel.SetTracerProvider(tp)
	setPropagator()
	active.Store(true)

	return func(shutdownCtx context.Context) error {
		// Order matters: clear the flag FIRST so any reconcile still in flight stops building
		// spans for a provider that is being torn down, then flush what is already buffered.
		active.Store(false)
		return tp.Shutdown(shutdownCtx)
	}, nil
}

// setPropagator installs W3C trace-context + baggage as the global propagator. The Go SDK does
// not read OTEL_PROPAGATORS, and this pair is both the specified default and the only format the
// TRACEPARENT/TRACESTATE variables injected into mover Jobs speak.
func setPropagator() {
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))
}

// buildResource assembles the resource, with the environment LAST so OTEL_SERVICE_NAME and
// OTEL_RESOURCE_ATTRIBUTES override the built-in service name rather than the other way round.
//
// resource.Default() is deliberately not used: it applies WithFromEnv itself and falls back to
// "unknown_service:<binary>" when OTEL_SERVICE_NAME is unset, which would silently displace the
// name §5 fixes.
func buildResource(ctx context.Context, defaultServiceName string) (*resource.Resource, error) {
	return resource.New(ctx,
		resource.WithAttributes(semconv.ServiceName(defaultServiceName)),
		resource.WithTelemetrySDK(),
		resource.WithFromEnv(),
	)
}

// buildExporter constructs the OTLP exporter for the configured protocol. Both transports are
// supported because both are commonly the only one a given collector exposes, and an operator who
// points OTEL_EXPORTER_OTLP_ENDPOINT at an :4318 HTTP receiver should not have to discover from a
// connection error that this build only spoke gRPC.
//
// Neither exporter is given an endpoint here: they read OTEL_EXPORTER_OTLP_ENDPOINT (and the
// _TRACES_ override), the headers, the timeout and the TLS settings from the environment
// themselves, which is the contract this package promises.
func buildExporter(ctx context.Context) (sdktrace.SpanExporter, error) {
	switch protocol() {
	case "http/protobuf", "http/json", "http":
		return otlptracehttp.New(ctx)
	default:
		return otlptracegrpc.New(ctx)
	}
}

// protocol resolves the OTLP protocol, traces-specific variable first. It defaults to grpc rather
// than to the OTLP specification's http/protobuf: a Kubernetes collector's gRPC receiver is the
// near-universal deployment, and an endpoint given without a protocol is overwhelmingly a :4317.
func protocol() string {
	if p := strings.TrimSpace(os.Getenv(envTracesProtocol)); p != "" {
		return strings.ToLower(p)
	}
	return strings.ToLower(strings.TrimSpace(os.Getenv(envProtocol)))
}

// Tracer returns CrystalBackup's tracer, under one instrumentation scope for the whole project.
//
// The operator never calls it: its spans all go through an [Anchor], because none of them can be
// held open across a reconcile. The mover shim does — it is one process running one restic
// invocation, so a live span there is both correct and simpler than reconstructing a window from
// timestamps nobody wrote down.
func Tracer() trace.Tracer {
	return otel.GetTracerProvider().Tracer("github.com/CrystalBackup/CrystalBackup")
}
