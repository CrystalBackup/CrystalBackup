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
	"crypto/rand"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// pinnedIDs is the value smuggled through the start context to tell the ID generator which
// identity the span about to be created must have.
type pinnedIDs struct {
	traceID trace.TraceID
	spanID  trace.SpanID
}

type pinnedIDsKey struct{}

// withPinnedIDs marks ctx so that the NEXT span started from it takes exactly these ids.
//
// The context the SDK hands back from Start still carries this pin, so it must never be used to
// start another span — a child born from it would take its parent's id and the subtree would
// collapse. [Anchor.emit] discards it for exactly that reason, which costs nothing here because
// every span in this design is emitted complete rather than kept open for children.
func withPinnedIDs(ctx context.Context, traceID trace.TraceID, spanID trace.SpanID) context.Context {
	return context.WithValue(ctx, pinnedIDsKey{}, pinnedIDs{traceID: traceID, spanID: spanID})
}

func pinnedFrom(ctx context.Context) (pinnedIDs, bool) {
	p, ok := ctx.Value(pinnedIDsKey{}).(pinnedIDs)
	if !ok || !p.spanID.IsValid() {
		return pinnedIDs{}, false
	}
	return p, true
}

// pinnedIDGenerator lets a caller DICTATE a span's identity instead of receiving a random one.
//
// This is the mechanism the cross-reconcile design needs and the SDK offers no other way to
// reach. Two distinct problems require it:
//
//   - The root `backup` span is emitted at the END of a backup, but its children were emitted
//     minutes or hours earlier by earlier reconciles and already named it as their parent. It
//     must therefore be born with the span id those children were told to expect.
//   - The `mover` span's id is written into a Job's TRACEPARENT at Job-creation time, while the
//     span itself is emitted once the Job finishes. The env var and the span have to agree, and
//     the env var is written first.
//
// Unpinned spans — anything started without going through an [Anchor] — fall through to the
// SDK's own random generator, so the provider behaves exactly as stock for every other caller.
type pinnedIDGenerator struct{}

var _ sdktrace.IDGenerator = pinnedIDGenerator{}

// NewIDs is called for a span with no parent in context: the root emissions.
func (pinnedIDGenerator) NewIDs(ctx context.Context) (trace.TraceID, trace.SpanID) {
	if p, ok := pinnedFrom(ctx); ok && p.traceID.IsValid() {
		return p.traceID, p.spanID
	}
	return randomTraceID(), randomSpanID()
}

// NewSpanID is called for a span that HAS a parent: every child emission.
func (pinnedIDGenerator) NewSpanID(ctx context.Context, _ trace.TraceID) trace.SpanID {
	if p, ok := pinnedFrom(ctx); ok {
		return p.spanID
	}
	return randomSpanID()
}

// randomTraceID / randomSpanID mirror the SDK's own fallback. crypto/rand.Read cannot fail on any
// platform this operator runs on (it panics inside the runtime if the OS entropy source is
// broken), so there is no error path to propagate into a telemetry id.
func randomTraceID() trace.TraceID {
	var id trace.TraceID
	_, _ = rand.Read(id[:])
	return id
}

func randomSpanID() trace.SpanID {
	var id trace.SpanID
	_, _ = rand.Read(id[:])
	return id
}
