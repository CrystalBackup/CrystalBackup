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

package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/CrystalBackup/CrystalBackup/internal/mover"
	"github.com/CrystalBackup/CrystalBackup/internal/tracing"
)

// The shim's half of spec/05-observability.md §5's propagation: pick the operator's span up out of
// the environment, hang `restic.<operation>` off it, and flush before the process exits.
//
// The shim is the one place in this codebase where a LIVE span is the right shape. Its whole life
// is one restic invocation in one process — there is no reconcile to survive, no leader to hand
// over to, nothing to reconstruct — so the span opens when restic starts and closes when it exits,
// which is both simpler and more accurate than anything the operator can do from outside.
//
// Everything here no-ops when the operator was not tracing: with no TRACEPARENT in the environment
// there is no parent to continue, and with no OTEL_* there is no provider to record into. A mover
// Job created by an untraced operator carries neither, and runs exactly as it did before this
// existed.

// moverFlushTimeout bounds the final span flush.
//
// A mover Job's contract with the controller is its TERMINATION MESSAGE, and the pod dying before
// it writes one is read (correctly) as a hard failure that triggers a stale-lock unlock. So the
// flush is short and it happens BEFORE the exit path, never at the cost of it: three seconds is
// ample for a batch of two spans to a collector in the same cluster, and a collector that cannot
// take them in three seconds does not get to make this Job look OOMKilled.
const moverFlushTimeout = 3 * time.Second

// tracingShutdown is set by startTracing and drained by flushTracing. A package-level function
// rather than a value threaded through main, because the shim's single exit point (report) calls
// os.Exit and a deferred flush would never run.
var tracingShutdown tracing.ShutdownFunc

// startTracing brings the shim's tracer provider up and returns the context carrying the operator's
// span as this process's parent.
//
// A failure to initialise is DELIBERATELY not fatal, which is the opposite of the operator's own
// policy on the same error. The reasoning is what the two processes are for: the operator failing
// to reach its collector is a misconfiguration an administrator should see at startup and fix,
// while this process exists to move a tenant's data, and refusing to run a backup because a
// telemetry endpoint is unreachable would be an outage caused by observability.
func startTracing(ctx context.Context) context.Context {
	shutdown, err := tracing.Init(ctx, tracing.ServiceMover)
	if err != nil {
		// Not through report(): that path writes a MoverResult and exits. This is a warning about
		// telemetry on a Job that is about to do its real work regardless.
		warnf("crystal-mover: tracing disabled (%v)", err)
		return ctx
	}
	tracingShutdown = shutdown
	return tracing.FromEnv(ctx)
}

// flushTracing exports whatever is buffered. Called from report(), the shim's single exit point,
// before os.Exit — a deferred flush in main would be skipped by every path that matters.
func flushTracing() {
	if tracingShutdown == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), moverFlushTimeout)
	defer cancel()
	if err := tracingShutdown(ctx); err != nil {
		warnf("crystal-mover: flushing spans: %v", err)
	}
	tracingShutdown = nil
}

// warnf writes a single diagnostic line to the pod log. Telemetry problems are reported this way
// and never through report(): report writes the controller's MoverResult and exits, and a
// collector being unreachable must not be able to fail a backup that worked.
func warnf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
}

// startResticSpan opens the `restic.<operation>` span §5 nests inside the operator's `mover` span,
// and returns the function that closes it with the outcome.
//
// The span name carries the operation — restic.backup, restic.forget, restic.check — because a
// trace that called them all "restic" would make the one interesting question ("what was this pod
// actually doing for forty minutes?") unanswerable without opening the attributes.
func startResticSpan(ctx context.Context, operation string) (context.Context, func(mover.MoverResult, error)) {
	if !tracing.Active() {
		return ctx, func(mover.MoverResult, error) {}
	}
	ctx, span := tracing.Tracer().Start(ctx, "restic."+operation,
		// Client: this span measures a call OUT of the process, to object storage.
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attribute.String(tracing.AttrOperation, operation)))

	return ctx, func(result mover.MoverResult, err error) {
		// The numbers §5 names on this span, and they are only knowable HERE: they come off
		// restic's own --json summary, which the controller sees only as a 4096-byte termination
		// message it parses for status.
		if result.SnapshotID != "" {
			span.SetAttributes(attribute.String(tracing.AttrSnapshotID, result.SnapshotID))
		}
		if result.AddedBytes > 0 {
			span.SetAttributes(attribute.Int64(tracing.AttrBytesAdded, result.AddedBytes))
		}
		if result.SizeBytes > 0 {
			span.SetAttributes(attribute.Int64("crystalbackup.bytes_processed", result.SizeBytes))
		}
		if result.ResourceCount > 0 {
			span.SetAttributes(attribute.Int(tracing.AttrResourceCount, int(result.ResourceCount)))
		}
		switch {
		case err != nil:
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		case !result.OK:
			span.SetStatus(codes.Error, result.Error)
		}
		span.End()
	}
}
