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
	"time"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
	"github.com/CrystalBackup/CrystalBackup/internal/tracing"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// The controllers' side of spec/05-observability.md §5: deriving an object's trace anchor, hanging
// the `traceID` log key off it, and turning the timestamps Kubernetes already keeps into spans.
//
// Nothing here holds a span open. Every emission reads its start and end back from object state
// that outlives the process — a Backup's creationTimestamp, a VolumeSnapshot's cut time, a Job's
// completionTime — which is what lets a trace assembled over four hours by three different leader
// processes come out whole. See internal/tracing's package doc for why that shape, and not a
// registry of live spans, is the one this operator can actually run.

// traced installs an object's trace anchor on ctx, together with the `traceID` log key that joins
// its log lines to its trace (spec/05-observability.md §4).
//
// The key is added ONLY when tracing is active. §4 specifies it as "present when tracing is
// active", and an always-present empty `traceID` would be worse than useless: every log-shipping
// pipeline that indexes it would carry a dead column, and a Loki query for lines belonging to a
// trace would match every line that belonged to none.
func traced(ctx context.Context, uid types.UID) (context.Context, tracing.Anchor) {
	a := tracing.AnchorFor(string(uid))
	if !a.Valid() {
		return ctx, a
	}
	ctx = a.Context(ctx)
	return logf.IntoContext(ctx, logf.FromContext(ctx).WithValues("traceID", a.TraceID())), a
}

// backupAnchor is the Backup's place in the trace graph.
//
// Every emit site re-derives it rather than being handed one down a call chain, and that is not
// laziness about parameters: the derivation is a pure function of the UID, so two call sites in
// two different processes hours apart get the same answer, and threading an anchor through eight
// signatures would only create the possibility of one of them being handed the wrong object's.
func backupAnchor(backup *cbv1.Backup) tracing.Anchor {
	return tracing.AnchorFor(string(backup.UID))
}

// backupSpanAttrs is the identity every span in one Backup's tree carries, spelled exactly as §5
// names it. Empty values are omitted rather than emitted blank (tracing.StringAttr), so a search
// for spans missing a tenant does not match every span that simply had none to report.
//
// rc may be nil: a Backup can go terminal — failed on an unresolvable location, say — before its
// run context was ever built, and a span for that outcome is worth more than no span at all.
func backupSpanAttrs(backup *cbv1.Backup, rc *backupRunContext) []attribute.KeyValue {
	tenant := backup.Labels[apiconst.LabelTenant]
	if tenant == "" {
		tenant = backup.Namespace
	}
	cluster := ""
	if rc != nil {
		cluster = rc.clusterID
	}
	attrs := make([]attribute.KeyValue, 0, 8)
	attrs = append(attrs, tracing.StringAttr(tracing.AttrNamespace, backup.Namespace)...)
	attrs = append(attrs, tracing.StringAttr(tracing.AttrTenant, tenant)...)
	attrs = append(attrs, tracing.StringAttr(tracing.AttrBackup, backup.Name)...)
	attrs = append(attrs, tracing.StringAttr(tracing.AttrSchedule, backup.Labels[apiconst.LabelSchedule])...)
	attrs = append(attrs, tracing.StringAttr(tracing.AttrOrigin, backup.Labels[apiconst.LabelOrigin])...)
	attrs = append(attrs, tracing.StringAttr(tracing.AttrLocation, backup.Spec.LocationRef.Name)...)
	// The REAL cluster id, unlike the metric label of the same name. The collector's
	// name-keyed lookup forces an empty `cluster` on every namespace-plane metric series
	// (moverMetricClusterID explains why), but a span has no series to split and no dashboard
	// join to break, so it carries the value the controller actually resolved.
	attrs = append(attrs, tracing.StringAttr(tracing.AttrCluster, cluster)...)
	attrs = append(attrs, tracing.StringAttr(tracing.AttrClusterBackup, backup.Labels[apiconst.LabelClusterBackup])...)
	return attrs
}

// runLink resolves the span link from a cluster-plane child Backup to the ClusterBackup run that
// fanned it out (§5), returning nothing for a namespace-plane Backup or a run already garbage-
// collected by its schedule's history limit.
//
// A LINK, never a parent: a run over two hundred namespaces would otherwise produce one trace
// with tens of thousands of spans — past what any backend renders and well past what anyone
// reads. The link keeps the run reachable in one click from any namespace's trace while leaving
// each namespace its own, human-sized tree.
//
// It costs one cached Get, at most once per Backup, and only when tracing is on: the child carries
// its parent's NAME on a label but not its UID, and the UID is what the run's own anchor derives
// from. Reaching for the name instead would be a second derivation rule to keep in sync with the
// first, which is how the two sides end up disagreeing about a trace id.
func runLink(ctx context.Context, c client.Client, backup *cbv1.Backup) []trace.Link {
	runName := backup.Labels[apiconst.LabelClusterBackup]
	if runName == "" {
		return nil
	}
	var run cbv1.ClusterBackup
	if err := c.Get(ctx, client.ObjectKey{Name: runName}, &run); err != nil {
		return nil // the run has been GC'd, or is unreadable: no link, no error.
	}
	link, ok := tracing.AnchorFor(string(run.UID)).Link(
		attribute.String(tracing.AttrClusterBackup, runName))
	if !ok {
		return nil
	}
	return []trace.Link{link}
}

// jobWindow reads a Job's execution window off its status: when its first pod started and when the
// Job settled. Both are written by the Job controller and survive an operator restart, which is
// what lets a `mover` span cover a seven-hour upload that no single reconcile witnessed.
//
// The fallbacks matter more than the happy path. startTime is unset only for a Job that never
// scheduled a pod, where the object's own creationTimestamp is the honest start. completionTime is
// set only on SUCCESS — a Job that exhausted its backoffLimit has none — so a failed mover falls
// back to now(), which is within one poll interval (5s) of the truth because this is called from
// the pass that first observes the Job terminal.
func jobWindow(job *batchv1.Job) (start, end time.Time) {
	start = job.CreationTimestamp.Time
	if job.Status.StartTime != nil {
		start = job.Status.StartTime.Time
	}
	end = time.Now()
	if job.Status.CompletionTime != nil {
		end = job.Status.CompletionTime.Time
	}
	return start, end
}

// timeOrZero unwraps an optional Kubernetes timestamp.
func timeOrZero(t *metav1.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return t.Time
}
