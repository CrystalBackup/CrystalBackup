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
	"crypto/sha256"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// The domain-separation prefixes. Every derived id is a hash of one of these plus the object's
// UID, so a trace id and a span id can never collide even when they hash the same UID, and a
// future derivation added to this list cannot accidentally reproduce an existing one.
const (
	domainTrace = "crystalbackup/trace/v1\x00"
	domainRoot  = "crystalbackup/span/root/v1\x00"
	domainStep  = "crystalbackup/span/step/v1\x00"
)

// The step keys. A step key names ONE span within an object's trace and must be stable for the
// object's whole life, because it is what an emission at the end of a backup and an injection at
// the start of a Job independently hash to arrive at the same span id. Per-PVC steps append the
// PVC name; see [StepPVC].
const (
	StepHooksPre  = "hooks.pre"
	StepHooksPost = "hooks.post"
	StepSnapshot  = "snapshot"
	StepExpose    = "expose"
	StepMover     = "mover"
	StepManifests = "manifests"
	StepForget    = "forget"
)

// StepPVC qualifies a per-PVC step with the volume it belongs to, so one Backup's five volumes
// yield five distinct `mover` spans rather than five emissions fighting over one id.
func StepPVC(step, pvc string) string { return step + "\x00" + pvc }

// The crystalbackup.* attribute keys, spelled exactly as spec/05-observability.md §5 names them.
// They mirror the metric label set (§2) on purpose: the same words mean the same things whether a
// reader arrives from a dashboard or from a trace.
const (
	AttrNamespace     = "crystalbackup.namespace"
	AttrTenant        = "crystalbackup.tenant"
	AttrBackup        = "crystalbackup.backup"
	AttrSchedule      = "crystalbackup.schedule"
	AttrOrigin        = "crystalbackup.origin"
	AttrLocation      = "crystalbackup.location"
	AttrCluster       = "crystalbackup.cluster"
	AttrClusterBackup = "crystalbackup.clusterbackup"
	AttrPVC           = "crystalbackup.pvc"
	AttrNode          = "crystalbackup.node"
	AttrSnapshotID    = "crystalbackup.snapshot_id"
	AttrBytesAdded    = "crystalbackup.bytes_added"
	AttrResourceCount = "crystalbackup.resource_count"
	AttrPhase         = "crystalbackup.phase"
	AttrExposer       = "crystalbackup.exposer"
	AttrOperation     = "crystalbackup.operation"

	// §5 also names crystalbackup.snapshots_removed on the `forget` span. It is NOT declared
	// here, because nothing in this operator can fill it: `restic forget` reports its removals
	// only as human-readable stdout, and the mover's termination-message protocol (a 4096-byte
	// MoverResult) carries no field for a count. Reserving the name while emitting nothing would
	// be a promise the code cannot keep.
)

// Anchor is one long-lived object's place in the trace graph, DERIVED rather than remembered.
//
// A Backup is reconciled dozens of times across minutes or hours, by a process that may not be
// the process that started it. Nothing in memory survives that, and nothing in the Backup's API
// can be written to carry a trace id (the CRD is not this lot's to change). So the identity is
// computed: trace id and root span id are both hashes of the object's UID, which every process
// reads off the object it is already holding.
//
// The consequences are the reason this shape was chosen over a registry of live spans:
//
//   - A restart or a leader-election handover loses NOTHING. The next process derives the same
//     ids from the same UID and its spans land in the same trace as the previous one's.
//   - There is no map to evict from, so there is no leak — which matters in a codebase whose
//     last two milestones were spent closing leaks.
//   - A span is emitted only when its outcome is durable, so a span is never left open by a
//     process that dies, and therefore never left unexported with orphaned children.
//
// The zero Anchor is inert: every method on it is a no-op. That is what an unconfigured install
// gets, and it is why call sites need no `if tracing.Active()` of their own.
type Anchor struct {
	sc trace.SpanContext
}

// AnchorFor derives the anchor of the object with this UID. It returns the zero Anchor — inert —
// when tracing is off or the UID is empty, so a caller may hold and use one unconditionally.
//
// The UID, not the namespace/name: a Backup deleted and recreated under the same name is a
// different run and must not have its spans merged into the old one's trace.
func AnchorFor(uid string) Anchor {
	if !Active() || uid == "" {
		return Anchor{}
	}
	return Anchor{sc: trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: deriveTraceID(uid),
		SpanID:  deriveSpanID(domainRoot, uid, ""),
		// Sampled, unconditionally, and this is a real limitation worth stating rather than
		// hiding. The children of this anchor are emitted BEFORE the root is, so they cannot
		// consult the root's sampling decision — it has not been taken yet. Marking the
		// synthesized parent sampled keeps the tree whole under the SDK's default
		// parentbased_always_on; under a ratio or always_off sampler the root and its children
		// would take independent decisions, and the honest fix for that is a persisted decision
		// on the object, which needs an API field this lot does not own.
		TraceFlags: trace.FlagsSampled,
		// Remote: the parent was not produced by a span in this process. It is the truthful
		// encoding — for most children it was produced by a DIFFERENT process — and it also
		// selects the ParentBased sampler's remote-parent branch, which is the one that keeps a
		// child sampled because its parent is.
		Remote: true,
	})}
}

// Valid reports whether this anchor can emit anything.
func (a Anchor) Valid() bool { return a.sc.IsValid() }

// TraceID renders the anchor's trace id for log correlation, or "" when inert. This is the value
// the `traceID` log key carries (§4), and it is what joins a Loki line to a Tempo trace.
func (a Anchor) TraceID() string {
	if !a.sc.IsValid() {
		return ""
	}
	return a.sc.TraceID().String()
}

// Context returns ctx with this anchor installed as the current span context.
//
// Two things read it. Anything started from the returned context becomes a child of the root
// span, which is how a live span (were one ever wanted) would attach. And
// internal/metrics reads the trace id off it to hang an exemplar on a histogram observation, so
// a latency spike in Grafana links straight to the backup that produced it.
func (a Anchor) Context(ctx context.Context) context.Context {
	if !a.sc.IsValid() {
		return ctx
	}
	return trace.ContextWithSpanContext(ctx, a.sc)
}

// StepContext returns ctx carrying the span context of the named step — the span that has not
// been emitted yet.
//
// This is how a mover Job is told its parent before that parent exists. The `mover` span is
// emitted when the Job finishes, but its TRACEPARENT must be written into the pod template when
// the Job is created; because the step's span id is derived, both sides compute it independently
// and agree. [Anchor.EmitStep] later emits the span with exactly this identity.
func (a Anchor) StepContext(ctx context.Context, step string) context.Context {
	if !a.sc.IsValid() {
		return ctx
	}
	return trace.ContextWithSpanContext(ctx, a.stepSpanContext(step))
}

func (a Anchor) stepSpanContext(step string) trace.SpanContext {
	return trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    a.sc.TraceID(),
		SpanID:     deriveSpanID(domainStep, a.sc.TraceID().String(), step),
		TraceFlags: trace.FlagsSampled,
		Remote:     true,
	})
}

// EmitStep emits one finished child span of the root: name it, span it from start to end, hang
// the attributes on it. A zero or inverted [start, end] is clamped to a zero-length span at start
// rather than dropped — the step DID happen, and a clock skew between two objects' timestamps
// must not make it disappear from the trace.
//
// err, when non-nil, sets the span's status to Error and records it. Callers pass the same error
// they put on the object's status, so a trace and a `kubectl describe` tell the same story.
func (a Anchor) EmitStep(ctx context.Context, step, name string, start, end time.Time, err error, attrs ...attribute.KeyValue) {
	if !a.sc.IsValid() {
		return
	}
	a.emit(a.Context(ctx), a.stepSpanContext(step).SpanID(), name, start, end, err, attrs)
}

// EmitRoot emits the object's ROOT span, pinned to the anchor's own span id so the children
// emitted throughout the object's life — each of which named that id as its parent — hang off it.
//
// Called exactly once per object, at the transition into a terminal phase and only after that
// phase has been persisted. That guard is not this package's to enforce; it rides on the same
// justTerminal condition the terminal counters use, for the same reason (a conflict retry that
// re-emitted would produce a second, duplicate root).
//
// links carries the cluster-plane fan-out: a per-namespace Backup root LINKS to its ClusterBackup
// run rather than being parented by it, because a run over two hundred namespaces parented into
// one tree is a trace no backend renders and no human reads (§5).
func (a Anchor) EmitRoot(
	ctx context.Context, name string, start, end time.Time, err error,
	links []trace.Link, attrs ...attribute.KeyValue,
) {
	if !a.sc.IsValid() {
		return
	}
	// A ROOT span: strip any ambient span context so the SDK takes the no-parent path and calls
	// NewIDs, where both the trace id and the span id can be pinned.
	root := trace.ContextWithSpanContext(ctx, trace.SpanContext{})
	root = withPinnedIDs(root, a.sc.TraceID(), a.sc.SpanID())
	a.emitIn(root, name, start, end, err, links, attrs)
}

// Link returns a span link to this anchor's root span, for another object's root to carry.
func (a Anchor) Link(attrs ...attribute.KeyValue) (trace.Link, bool) {
	if !a.sc.IsValid() {
		return trace.Link{}, false
	}
	return trace.Link{SpanContext: a.sc, Attributes: attrs}, true
}

func (a Anchor) emit(
	parentCtx context.Context, spanID trace.SpanID, name string,
	start, end time.Time, err error, attrs []attribute.KeyValue,
) {
	a.emitIn(withPinnedIDs(parentCtx, a.sc.TraceID(), spanID), name, start, end, err, nil, attrs)
}

func (a Anchor) emitIn(
	ctx context.Context, name string, start, end time.Time, err error,
	links []trace.Link, attrs []attribute.KeyValue,
) {
	start, end = clampWindow(start, end)

	opts := []trace.SpanStartOption{
		trace.WithTimestamp(start),
		trace.WithAttributes(attrs...),
		// Internal, not Server/Client: these spans describe work the operator drove, not a
		// request it served. The one Client-ish span in the tree is the shim's restic call, and
		// it is emitted by the shim.
		trace.WithSpanKind(trace.SpanKindInternal),
	}
	if len(links) > 0 {
		opts = append(opts, trace.WithLinks(links...))
	}

	// The returned context is DISCARDED, and that is load-bearing rather than incidental: it
	// still carries the id pin, so a span started from it would be born with the same span id as
	// the one just created. Nothing in this design ever needs it — every span here is emitted
	// complete, with its window already known — so the safe thing and the natural thing coincide.
	_, span := Tracer().Start(ctx, name, opts...)

	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	span.End(trace.WithTimestamp(end))
}

// clampWindow makes a [start, end] pair safe to hand the SDK.
//
// Both ends come from timestamps written by DIFFERENT actors — an object's creationTimestamp from
// the API server, a Job's completionTime from the Job controller, a VolumeSnapshot's cut time
// from a CSI driver — so they are subject to genuine skew, and a Kubernetes metav1.Time is
// second-granular besides, which makes a sub-second step routinely look like it ended before it
// began. A negative-duration span is rejected outright by some backends and rendered as a
// nonsense bar by others; a zero-length one at the right instant is the honest degradation.
func clampWindow(start, end time.Time) (time.Time, time.Time) {
	if start.IsZero() && end.IsZero() {
		now := time.Now()
		return now, now
	}
	if start.IsZero() {
		start = end
	}
	if end.IsZero() || end.Before(start) {
		end = start
	}
	return start, end
}

// deriveTraceID hashes a UID into a trace id.
func deriveTraceID(uid string) trace.TraceID {
	sum := sha256.Sum256([]byte(domainTrace + uid))
	var id trace.TraceID
	copy(id[:], sum[:])
	// An all-zero id is invalid to OTel and would silently disable the whole trace. It is a
	// 1-in-2^128 event, so this is not a real branch — it is here so that if it ever happened the
	// failure would be a degraded trace id rather than a trace that vanishes.
	if !id.IsValid() {
		id[len(id)-1] = 1
	}
	return id
}

// deriveSpanID hashes a domain, a key and a step into a span id.
func deriveSpanID(domain, key, step string) trace.SpanID {
	h := sha256.New()
	h.Write([]byte(domain))
	h.Write([]byte(key))
	h.Write([]byte{0})
	h.Write([]byte(step))
	var id trace.SpanID
	copy(id[:], h.Sum(nil))
	if !id.IsValid() {
		id[len(id)-1] = 1
	}
	return id
}

// StringAttr returns the attribute, or nothing at all when the value is empty.
//
// An absent attribute and an attribute set to "" are different claims: the first says the
// operator had no value, the second says the value IS empty. The metric catalogue already draws
// this distinction for the `cluster` label, and a trace search for a missing tenant should not
// match every span that merely had none.
func StringAttr(key, value string) []attribute.KeyValue {
	if value == "" {
		return nil
	}
	return []attribute.KeyValue{attribute.String(key, value)}
}
