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

package metrics

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/trace"
)

// The exemplar half of spec/05-observability.md §5: a duration histogram carries a pointer to the
// trace that produced the sample, so a p99 spike in Grafana is one click from the backup behind
// it. These tests exercise `observe` directly — it is the single funnel every histogram in this
// package goes through, so covering it covers all five families without five near-identical tests.

// sampledContext returns a context carrying a valid, SAMPLED span context, as the Backup
// controller's anchor installs on every reconcile when tracing is active.
func sampledContext(t *testing.T) (context.Context, string) {
	t.Helper()
	tid, err := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	if err != nil {
		t.Fatalf("trace id: %v", err)
	}
	sid, err := trace.SpanIDFromHex("00f067aa0ba902b7")
	if err != nil {
		t.Fatalf("span id: %v", err)
	}
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: tid, SpanID: sid, TraceFlags: trace.FlagsSampled, Remote: true,
	})
	return trace.ContextWithSpanContext(context.Background(), sc), tid.String()
}

// gatherExemplar returns the exemplar attached to the (single) sample in a freshly registered
// histogram, or nil when there is none.
func gatherExemplar(t *testing.T, h prometheus.Histogram) map[string]string {
	t.Helper()
	reg := prometheus.NewRegistry()
	reg.MustRegister(h)
	fams, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range fams {
		for _, m := range f.GetMetric() {
			for _, b := range m.GetHistogram().GetBucket() {
				if b.GetExemplar() == nil {
					continue
				}
				out := map[string]string{}
				for _, l := range b.GetExemplar().GetLabel() {
					out[l.GetName()] = l.GetValue()
				}
				return out
			}
		}
	}
	return nil
}

func newTestHistogram() prometheus.Histogram {
	return prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "crystalbackup_exemplar_probe_seconds",
		Help:    "A throwaway histogram for the exemplar tests.",
		Buckets: durationBuckets,
	})
}

// TestObserveAttachesTheCurrentTraceAsAnExemplar is the positive case: with tracing active, the
// sample carries a `trace_id` exemplar naming the trace that produced it.
func TestObserveAttachesTheCurrentTraceAsAnExemplar(t *testing.T) {
	ctx, traceID := sampledContext(t)
	h := newTestHistogram()
	observe(ctx, h, 120)

	ex := gatherExemplar(t, h)
	if ex == nil {
		t.Fatal("no exemplar was attached to a sample recorded under an active trace")
	}
	if got := ex[exemplarTraceIDLabel]; got != traceID {
		t.Errorf("exemplar %s = %q, want %q", exemplarTraceIDLabel, got, traceID)
	}
	if len(ex) != 1 {
		t.Errorf("the exemplar carries %d labels (%v); it must carry only the trace id — anything "+
			"else is unbounded data riding into the TSDB beside the bucket", len(ex), ex)
	}
}

// TestObserveWithoutTracingRecordsNoExemplar is the default case, and the one that matters most:
// with no trace in context the sample is recorded exactly as it always was.
func TestObserveWithoutTracingRecordsNoExemplar(t *testing.T) {
	h := newTestHistogram()
	observe(context.Background(), h, 120)

	if ex := gatherExemplar(t, h); ex != nil {
		t.Errorf("an exemplar %v was attached with no trace in context", ex)
	}
	// The sample itself is still there — the point is that only the exemplar is conditional.
	reg := prometheus.NewRegistry()
	reg.MustRegister(h)
	if got := countSamples(t, reg, "crystalbackup_exemplar_probe_seconds"); got == 0 {
		t.Error("the observation itself was dropped when tracing was inactive")
	}
}

// TestObserveSkipsAnUnsampledTrace. A trace id that was dropped at the head links to a trace the
// backend never received, which is worse than no link: it turns a working "jump to trace" button
// into one that reliably 404s, and an operator only has to hit that twice before they stop
// trusting exemplars altogether.
func TestObserveSkipsAnUnsampledTrace(t *testing.T) {
	tid, _ := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	sid, _ := trace.SpanIDFromHex("00f067aa0ba902b7")
	unsampled := trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: sid}) // no FlagsSampled
	ctx := trace.ContextWithSpanContext(context.Background(), unsampled)

	h := newTestHistogram()
	observe(ctx, h, 120)

	if ex := gatherExemplar(t, h); ex != nil {
		t.Errorf("an exemplar %v was attached for an UNSAMPLED trace the backend will never hold", ex)
	}
}
