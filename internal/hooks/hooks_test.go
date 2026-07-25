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

package hooks

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
)

// fakeExec records calls and replays canned outcomes, keyed by pod name.
type fakeExec struct {
	calls  []string
	fail   map[string]error
	blockD time.Duration
}

func (f *fakeExec) Exec(ctx context.Context, pod types.NamespacedName, container string, command []string) (string, string, error) {
	f.calls = append(f.calls, pod.Name+"/"+container)
	if f.blockD > 0 {
		select {
		case <-time.After(f.blockD):
		case <-ctx.Done():
			return "", "", ctx.Err()
		}
	}
	if err, ok := f.fail[pod.Name]; ok {
		return "", "boom", err
	}
	return "ok", "", nil
}

func runningPod(name string, labels, annotations map[string]string, containers ...string) corev1.Pod {
	if len(containers) == 0 {
		containers = []string{"app"}
	}
	cs := make([]corev1.Container, 0, len(containers))
	for _, c := range containers {
		cs = append(cs, corev1.Container{Name: c})
	}
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-x", Name: name, Labels: labels, Annotations: annotations},
		Spec:       corev1.PodSpec{Containers: cs},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

// TestResolveAnnotationsWinPerPod pins the precedence rule taken from Velero
// (internal/hook/item_hook_handler.go: "If the pod has the hook specified via annotations, that
// takes priority"). It is per-POD and it REPLACES rather than merges: the pod owner who declared a
// quiesce knows what their application needs, and running the platform's hook as well would
// produce a freeze window neither party designed.
func TestResolveAnnotationsWinPerPod(t *testing.T) {
	annotated := runningPod("db-0", map[string]string{"app": "postgres"}, map[string]string{
		apiconst.AnnotationPreBackupPrefix + "command":   `["psql","-c","CHECKPOINT"]`,
		apiconst.AnnotationPreBackupPrefix + "container": "postgres",
	}, "sidecar", "postgres")
	plain := runningPod("db-1", map[string]string{"app": "postgres"}, nil)

	spec := cbv1.HooksSpec{
		HonorAnnotations: true,
		Pre: []cbv1.Hook{{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "postgres"}},
			Command:     []string{"/bin/true"},
		}},
	}

	got := Resolve([]corev1.Pod{annotated, plain}, spec, PhasePre)
	if len(got) != 2 {
		t.Fatalf("resolved %d hooks, want 2 (one per pod)", len(got))
	}
	if got[0].Source != SourceAnnotation || !reflect.DeepEqual(got[0].Command, []string{"psql", "-c", "CHECKPOINT"}) {
		t.Errorf("annotated pod resolved to %+v; the annotation must win and its JSON argv must parse", got[0])
	}
	if got[0].Container != "postgres" {
		t.Errorf("container = %q, want the annotated one", got[0].Container)
	}
	if got[1].Source != SourceSpec || got[1].Container != "sidecar" && got[1].Container != "app" {
		t.Errorf("unannotated pod resolved to %+v; it must fall back to the spec hook", got[1])
	}
}

// TestResolveIgnoresAnnotationsWhenNotHonoured: honorAnnotations is opt-in. A cluster admin who has
// not enabled it must not have tenant-authored commands executed by the operator on their behalf.
func TestResolveIgnoresAnnotationsWhenNotHonoured(t *testing.T) {
	pod := runningPod("db-0", map[string]string{"app": "postgres"}, map[string]string{
		apiconst.AnnotationPreBackupPrefix + "command": "rm -rf /",
	})
	spec := cbv1.HooksSpec{Pre: []cbv1.Hook{{Command: []string{"/bin/true"}}}}

	got := Resolve([]corev1.Pod{pod}, spec, PhasePre)
	if len(got) != 1 || got[0].Source != SourceSpec {
		t.Fatalf("resolved %+v; with honorAnnotations off the annotation must be ignored", got)
	}
}

// TestResolveSkipsPodsThatCannotBeExecd: exec into a Pending or Terminating pod fails with an
// opaque API error, and "the database was not running" is not a consistency failure worth aborting
// a backup over.
func TestResolveSkipsPodsThatCannotBeExecd(t *testing.T) {
	pending := runningPod("pending", nil, nil)
	pending.Status.Phase = corev1.PodPending
	deleting := runningPod("deleting", nil, nil)
	now := metav1.Now()
	deleting.DeletionTimestamp = &now
	live := runningPod("live", nil, nil)

	spec := cbv1.HooksSpec{Pre: []cbv1.Hook{{Command: []string{"/bin/true"}}}}
	got := Resolve([]corev1.Pod{pending, deleting, live}, spec, PhasePre)
	if len(got) != 1 || got[0].Pod.Name != "live" {
		t.Fatalf("resolved %+v, want only the running pod", got)
	}
}

// TestResolvePostPhaseUsesItsOwnAnnotations: a pre-backup quiesce needs a matching release, and the
// two must be independently declarable on the same pod.
func TestResolvePostPhaseUsesItsOwnAnnotations(t *testing.T) {
	pod := runningPod("db-0", nil, map[string]string{
		apiconst.AnnotationPreBackupPrefix + "command":  "freeze",
		apiconst.AnnotationPostBackupPrefix + "command": "thaw",
	})
	spec := cbv1.HooksSpec{HonorAnnotations: true}

	pre := Resolve([]corev1.Pod{pod}, spec, PhasePre)
	post := Resolve([]corev1.Pod{pod}, spec, PhasePost)
	if len(pre) != 1 || pre[0].Command[0] != "freeze" {
		t.Fatalf("pre = %+v, want the freeze command", pre)
	}
	if len(post) != 1 || post[0].Command[0] != "thaw" {
		t.Fatalf("post = %+v, want the thaw command", post)
	}
}

// TestResolveDefaults covers the two defaults that are load-bearing rather than cosmetic: an unset
// timeout must become DefaultTimeout (Hook.Timeout is a non-pointer metav1.Duration, so "unset"
// arrives as 0s, and context.WithTimeout(ctx, 0) expires INSTANTLY — every unset hook would fail
// immediately), and an unset policy must be Fail, matching the CRD default.
func TestResolveDefaults(t *testing.T) {
	pod := runningPod("db-0", nil, nil)
	spec := cbv1.HooksSpec{Pre: []cbv1.Hook{{Command: []string{"/bin/true"}}}}

	got := Resolve([]corev1.Pod{pod}, spec, PhasePre)
	if len(got) != 1 {
		t.Fatalf("resolved %d hooks, want 1", len(got))
	}
	if got[0].Timeout != DefaultTimeout {
		t.Errorf("timeout = %v, want the %v default — a zero timeout would expire instantly", got[0].Timeout, DefaultTimeout)
	}
	if got[0].OnError != cbv1.HookErrorPolicyFail {
		t.Errorf("onError = %q, want Fail — an unset policy must fail closed", got[0].OnError)
	}
}

// TestResolveMalformedSelectorMatchesNothing: a typo must never silently widen a hook's blast
// radius from "the postgres pods" to "every pod holding this data".
func TestResolveMalformedSelectorMatchesNothing(t *testing.T) {
	pod := runningPod("db-0", map[string]string{"app": "postgres"}, nil)
	spec := cbv1.HooksSpec{Pre: []cbv1.Hook{{
		PodSelector: metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{
			Key: "app", Operator: "NotAnOperator", Values: []string{"postgres"},
		}}},
		Command: []string{"/bin/true"},
	}}}

	if got := Resolve([]corev1.Pod{pod}, spec, PhasePre); len(got) != 0 {
		t.Fatalf("resolved %+v; a malformed selector must match nothing", got)
	}
}

// TestRunHonoursTheTimeout is the dedicated timeout test the M4 roadmap demands by name.
//
// A hook holds a quiesced — possibly frozen — application open for exactly as long as it runs, so
// an unbounded hook is an outage rather than a slow backup. The deadline must be real: the call
// returns at roughly the timeout, not at the hook's own duration, and the error names the deadline
// so an operator is sent to their own command rather than to the network.
func TestRunHonoursTheTimeout(t *testing.T) {
	exec := &fakeExec{blockD: 10 * time.Second}
	hook := Resolved{
		Pod:     types.NamespacedName{Namespace: "team-x", Name: "db-0"},
		Command: []string{"sleep", "forever"},
		Timeout: 50 * time.Millisecond,
		OnError: cbv1.HookErrorPolicyFail,
	}

	start := time.Now()
	results := Run(context.Background(), exec, []Resolved{hook})
	elapsed := time.Since(start)

	if len(results) != 1 || !results[0].Failed() {
		t.Fatalf("results = %+v, want one failure", results)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("Run took %v for a 50ms timeout — the deadline is not being enforced", elapsed)
	}
	if msg := results[0].Err.Error(); !strings.Contains(msg, "timed out after 50ms") {
		t.Errorf("error %q does not name the deadline", msg)
	}
	if !results[0].Aborts() {
		t.Error("a timed-out hook with onError=Fail must abort the phase")
	}
}

// TestRunOnErrorContinue: a best-effort hook records its failure and the run proceeds.
func TestRunOnErrorContinue(t *testing.T) {
	exec := &fakeExec{fail: map[string]error{"db-0": errors.New("exit status 1")}}
	hooks := []Resolved{
		{Pod: types.NamespacedName{Name: "db-0"}, Command: []string{"a"}, OnError: cbv1.HookErrorPolicyContinue},
		{Pod: types.NamespacedName{Name: "db-1"}, Command: []string{"b"}, OnError: cbv1.HookErrorPolicyFail},
	}

	results := Run(context.Background(), exec, hooks)
	if len(results) != 2 {
		t.Fatalf("results = %+v, want both hooks to have run", results)
	}
	if !results[0].Failed() || results[0].Aborts() {
		t.Errorf("first result = %+v, want a recorded failure that does not abort", results[0])
	}
	if results[1].Failed() {
		t.Errorf("second hook = %+v, want it to have run and passed", results[1])
	}
	if len(exec.calls) != 2 {
		t.Errorf("exec calls = %v, want both hooks executed", exec.calls)
	}
}

// TestRunFailStopsAndReportsSkipped: a status listing three hooks where the CR declares five
// invites the reader to assume the other two passed. Every hook gets a Result.
func TestRunFailStopsAndReportsSkipped(t *testing.T) {
	exec := &fakeExec{fail: map[string]error{"db-0": errors.New("exit status 1")}}
	hooks := []Resolved{
		{Pod: types.NamespacedName{Name: "db-0"}, Command: []string{"a"}, OnError: cbv1.HookErrorPolicyFail},
		{Pod: types.NamespacedName{Name: "db-1"}, Command: []string{"b"}, OnError: cbv1.HookErrorPolicyFail},
		{Pod: types.NamespacedName{Name: "db-2"}, Command: []string{"c"}, OnError: cbv1.HookErrorPolicyFail},
	}

	results := Run(context.Background(), exec, hooks)
	if len(results) != 3 {
		t.Fatalf("got %d results for 3 hooks; every hook must be accounted for", len(results))
	}
	if !results[0].Aborts() {
		t.Error("the failing hook must abort")
	}
	for _, r := range results[1:] {
		if !errors.Is(r.Err, ErrSkipped) {
			t.Errorf("result %+v, want ErrSkipped", r)
		}
	}
	if len(exec.calls) != 1 {
		t.Errorf("exec calls = %v; nothing may run after an aborting hook", exec.calls)
	}
}

// TestRunEmptyCommand: an empty argv would exec successfully-looking nothing, or fail with an
// opaque API error. Reject it with a message that says what is wrong.
func TestRunEmptyCommand(t *testing.T) {
	exec := &fakeExec{}
	results := Run(context.Background(), exec, []Resolved{{Pod: types.NamespacedName{Name: "db-0"}}})
	if len(results) != 1 || !errors.Is(results[0].Err, ErrNoCommand) {
		t.Fatalf("results = %+v, want ErrNoCommand", results)
	}
	if len(exec.calls) != 0 {
		t.Error("an empty command must not reach the executor")
	}
}

func TestParseCommand(t *testing.T) {
	cases := []struct {
		raw  string
		want []string
	}{
		{`["psql","-c","CHECKPOINT"]`, []string{"psql", "-c", "CHECKPOINT"}},
		{`  ["a","b"]  `, []string{"a", "b"}},
		// Not JSON: a single-element argv, executed DIRECTLY. Wrapping in `sh -c` would give every
		// hook shell metacharacter semantics and assume a shell exists in a distroless image.
		{"/usr/bin/flush", []string{"/usr/bin/flush"}},
		// Malformed JSON degrades to the literal rather than erroring: the annotation has no
		// admission behind it, and a broken quiesce should be visible as a failing command.
		{`["unclosed`, []string{`["unclosed`}},
		{`[]`, []string{`[]`}},
	}
	for _, tc := range cases {
		if got := parseCommand(tc.raw); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("parseCommand(%q) = %v, want %v", tc.raw, got, tc.want)
		}
	}
}

// TestAnnotationTimeoutAndPolicy covers the two optional annotations, including the deliberate
// choice that an UNPARSEABLE timeout keeps the default: refusing to quiesce a database because
// someone typed "30" instead of "30s" is the wrong trade for an unvalidated field.
func TestAnnotationTimeoutAndPolicy(t *testing.T) {
	pod := runningPod("db-0", nil, map[string]string{
		apiconst.AnnotationPreBackupPrefix + "command":  "flush",
		apiconst.AnnotationPreBackupPrefix + "timeout":  "2m",
		apiconst.AnnotationPreBackupPrefix + "on-error": "continue", // case-insensitive
	})
	got := Resolve([]corev1.Pod{pod}, cbv1.HooksSpec{HonorAnnotations: true}, PhasePre)
	if len(got) != 1 || got[0].Timeout != 2*time.Minute || got[0].OnError != cbv1.HookErrorPolicyContinue {
		t.Fatalf("resolved %+v, want a 2m timeout and Continue", got)
	}

	pod.Annotations[apiconst.AnnotationPreBackupPrefix+"timeout"] = "30"
	got = Resolve([]corev1.Pod{pod}, cbv1.HooksSpec{HonorAnnotations: true}, PhasePre)
	if len(got) != 1 || got[0].Timeout != DefaultTimeout {
		t.Fatalf("resolved %+v, want the default timeout for an unparseable value", got)
	}
}
