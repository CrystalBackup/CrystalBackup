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
	"os"
	"slices"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

// The env var names the operator writes into a mover Job's pod template and the shim reads back.
// Upper-case rather than the header spelling: they are environment variables, and W3C names the
// FIELD, not the transport. The shim accepts either case (see [FromEnv]).
const (
	EnvTraceparent = "TRACEPARENT"
	EnvTracestate  = "TRACESTATE"
)

const (
	// otelEnvPrefix is the allow-list rule for what the operator forwards to its movers.
	otelEnvPrefix = "OTEL_"
	// envServiceName is the one OTEL_* variable the mover is given rather than handed down.
	envServiceName = "OTEL_SERVICE_NAME"
)

// otelEnvNotForwarded are the OTEL_* variables a mover must NOT inherit from the operator.
//
// OTEL_SERVICE_NAME is the whole point: the operator's is crystal-backup-operator, and a mover
// that inherited it would report restic's timings as the operator's own, merging two services
// with completely different lifetimes into one. The mover's name is set explicitly instead.
//
// OTEL_SDK_DISABLED is excluded for the opposite reason — it is never SET when the operator's
// tracing is active (Init would have bailed), so forwarding it can only ever propagate a stale
// "false" that means nothing.
var otelEnvNotForwarded = map[string]bool{
	envServiceName:      true,
	"OTEL_SDK_DISABLED": true,
}

// JobEnv returns the environment one mover Job needs to continue the trace in ctx: the W3C
// context of the span it belongs to, plus the operator's own OTLP configuration so the shim
// exports to the same collector.
//
// It returns nil when tracing is inactive or ctx carries no span context — and nil is the point.
// A mover Job spec is compared byte-for-byte in this codebase's tests and rebuilt on every
// reconcile; an install with no collector must produce EXACTLY the Job it produced before this
// lot existed, not the same Job with two empty variables on it.
//
// # Why the operator's own environment, rather than a Helm value
//
// spec/05-observability.md §5 says the mover's OTEL_* variables come from `mover.extraEnv`. That
// value does not exist in the chart — the `mover` block holds an image and nothing else — so
// there is no wiring to reuse. Inheriting the operator's own settings is the better shape anyway:
// it is one place to configure instead of two, it cannot drift into a mover exporting to a
// collector the operator does not use, and it needs no chart change to work. A future
// `mover.extraEnv` still wins, because JobRequest appends ExtraEnv AFTER this block and a later
// duplicate env var is the one the kubelet keeps.
func JobEnv(ctx context.Context) map[string]string {
	if !Active() {
		return nil
	}
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)

	traceparent := carrier["traceparent"]
	if traceparent == "" {
		return nil // no span context to continue: inject nothing.
	}

	env := map[string]string{
		EnvTraceparent: traceparent,
		// The mover's service identity, set rather than inherited (see otelEnvNotForwarded).
		envServiceName: ServiceMover,
	}
	if ts := carrier["tracestate"]; ts != "" {
		env[EnvTracestate] = ts
	}
	for _, kv := range os.Environ() {
		name, value, ok := strings.Cut(kv, "=")
		if !ok || !strings.HasPrefix(name, otelEnvPrefix) || otelEnvNotForwarded[name] {
			continue
		}
		env[name] = value
	}
	return env
}

// SortedEnvKeys returns a map's keys in a stable order. Mover Job specs must be byte-reproducible
// (internal/mover's contract), and ranging a Go map is not.
func SortedEnvKeys(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// FromEnv rebuilds the parent span context from the process environment — the shim's side of the
// handover. It returns ctx unchanged when TRACEPARENT is absent or malformed, which is exactly
// what a mover Job created by an untraced operator gets.
//
// Both spellings are accepted. The operator writes TRACEPARENT; the lower-case form is what an
// OTLP-aware sidecar or an operator's own `mover.extraEnv` would plausibly set, and refusing it
// would be a gratuitous way to lose a trace.
func FromEnv(ctx context.Context) context.Context {
	carrier := propagation.MapCarrier{}
	if v := firstEnv(EnvTraceparent, "traceparent"); v != "" {
		carrier["traceparent"] = v
	} else {
		return ctx
	}
	if v := firstEnv(EnvTracestate, "tracestate"); v != "" {
		carrier["tracestate"] = v
	}
	// The propagator is global state Init sets; a shim that extracts before Init would silently
	// get nothing back, so set it here too. It is idempotent and costs one assignment.
	setPropagator()
	return otel.GetTextMapPropagator().Extract(ctx, carrier)
}

func firstEnv(names ...string) string {
	for _, n := range names {
		if v := strings.TrimSpace(os.Getenv(n)); v != "" {
			return v
		}
	}
	return ""
}
