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

// Package hooks resolves and runs application-consistency hooks (R16): the commands executed
// inside a workload's own containers immediately before and after its volumes are snapshotted, so
// a database can flush or freeze and produce an application-consistent point in time rather than a
// merely crash-consistent one.
//
// # Two sources, and why annotations win
//
// A hook can be declared in two places, and this package mirrors Velero's precedence rule
// deliberately: a pod carrying hook ANNOTATIONS uses those, and the spec-declared hooks are
// skipped FOR THAT POD (never merged). The reason is not compatibility, it is authority. The
// person who knows that this particular Postgres needs `CHECKPOINT` before a snapshot is the one
// who ships the Deployment, not the platform admin writing a cluster-wide backup schedule. When
// both have spoken, the one closer to the application is the one who is right, and merging would
// produce a freeze window neither of them designed.
//
// # Which pods
//
// This is where CrystalBackup diverges from Velero, and the divergence is the point. Velero
// matches pods by namespace and label selector, because its unit of work is "back up these
// resources" and pods are among them. Our unit of work is a PVC: the caller passes exactly the
// pods that MOUNT the volumes about to be snapshotted, so a hook is bound to the data it is
// protecting rather than to a label someone remembered to apply. A pod in the namespace that
// mounts none of the selected volumes is not consistency-relevant and is never exec'd into.
//
// # Confinement
//
// Every hook runs in a pod the caller supplied, and the callers only ever list pods in the
// backed-up namespace (03-security-and-tenancy.md §5). This package never looks a pod up by name;
// it cannot reach outside the set it is given. That is the whole enforcement of the invariant, so
// callers must not hand it pods from elsewhere.
package hooks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
)

// Phase is when a hook runs relative to the snapshot.
type Phase string

const (
	// PhasePre runs before any VolumeSnapshot is created — the moment the application is asked to
	// quiesce.
	PhasePre Phase = "pre"
	// PhasePost runs as soon as every snapshot is cut, NOT after the upload. Hooks bound the
	// freeze window, and a frozen database held for the duration of a multi-hour upload would be
	// an outage (01-architecture.md §5).
	PhasePost Phase = "post"
)

// Source records where a resolved hook was declared, so events and status can say which one ran
// when an operator is wondering why their spec hook did not.
type Source string

const (
	SourceAnnotation Source = "annotation"
	SourceSpec       Source = "spec"
)

const (
	// DefaultTimeout applies when a hook sets none. Hook.Timeout is a non-pointer metav1.Duration
	// with no zero-distinguishing wrapper, so "unset" arrives as 0s — and a literal
	// context.WithTimeout(ctx, 0) expires instantly, turning every unset hook into an immediate
	// failure. Zero therefore MEANS default here, and that is load-bearing rather than cosmetic.
	DefaultTimeout = 30 * time.Second

	// annotation key suffixes, appended to apiconst.AnnotationPreBackupPrefix /
	// AnnotationPostBackupPrefix.
	annCommand   = "command"
	annContainer = "container"
	annOnError   = "on-error"
	annTimeout   = "timeout"
)

// Resolved is one hook bound to a concrete pod and container, with every default already applied.
// Nothing downstream needs to re-consult the spec or the annotations.
type Resolved struct {
	Pod       types.NamespacedName
	Container string
	Command   []string
	Timeout   time.Duration
	OnError   cbv1.HookErrorPolicy
	Source    Source
	Phase     Phase
	// ServiceAccountName is the ServiceAccount the operator impersonates to run this command. It
	// is resolved IN Pod.Namespace and nowhere else — the namespace is derived from the target
	// pod, never configured, so no value in any spec can point this at another tenant.
	//
	// It is carried on the resolved hook rather than passed alongside the batch because it must
	// hold for ANNOTATION-sourced hooks too: an annotation supplies the command, never the
	// identity. That is the one place this design differs from Velero's, whose pod annotations
	// let anyone able to annotate a pod choose what the backup controller execs as itself.
	//
	// Empty means "as the operator", which is the cluster plane's admin-authored default.
	ServiceAccountName string
}

// Result is one hook's outcome.
type Result struct {
	Hook   Resolved
	Err    error
	Stdout string
	Stderr string
}

// Failed reports whether the hook errored.
func (r Result) Failed() bool { return r.Err != nil }

// Aborts reports whether this failure must stop the backup: a failed hook with onError Fail. A
// hook with onError Continue records its error and the run proceeds.
//
// IT DOES NOT MEAN "ABANDON THE REST OF THE CHAIN", and conflating the two was a real defect (0.6.6
// lot 2). Run used to treat this predicate as both questions at once, which is correct for the PRE
// phase and wrong for the POST phase — see chainStopsOnAbort. Every OTHER caller wants exactly this
// question and nothing else: untoleratedResults counts the failures that still owe somebody
// something, hookFailureTolerated decides whether a recorded failure aborts the run. The contract
// here is unchanged; what moved out of it is the chain decision.
func (r Result) Aborts() bool {
	return r.Err != nil && r.Hook.OnError != cbv1.HookErrorPolicyContinue
}

// Executor runs one command in one container. The seam that keeps this package testable without a
// cluster, and the only thing in it that performs I/O.
type Executor interface {
	// Exec runs command in one container. serviceAccountName, when non-empty, is a ServiceAccount
	// in pod.Namespace that the call must be made AS — the API server then authorises the exec
	// against that identity's rights, not the operator's.
	Exec(ctx context.Context, pod types.NamespacedName, container string, command []string,
		serviceAccountName string) (stdout, stderr string, err error)
}

// Resolve computes the hooks to run for one phase against the given pods, applying the
// annotation-wins precedence per pod.
//
// pods MUST already be scoped to the backed-up namespace and to those mounting the volumes being
// snapshotted — see the package doc. Pods that are not Running are skipped: exec into a pod that
// is Pending or Succeeded fails with an opaque API error, and "the database was not running" is
// not a consistency failure worth aborting a backup over.
func Resolve(pods []corev1.Pod, spec cbv1.HooksSpec, phase Phase) []Resolved {
	var out []Resolved
	for i := range pods {
		pod := &pods[i]
		if pod.Status.Phase != corev1.PodRunning || !pod.DeletionTimestamp.IsZero() {
			continue
		}
		if spec.HonorAnnotations {
			if h, ok := fromAnnotations(pod, phase); ok {
				out = append(out, h)
				continue // annotations win for this pod; the spec hooks do not also run
			}
		}
		out = append(out, fromSpec(pod, spec, phase)...)
	}
	// The impersonated identity is stamped HERE, once, on every hook from either source, rather
	// than inside each builder. An annotation-sourced hook that missed it would run as the
	// operator — the exact escalation this field exists to close — and a single assignment site
	// is one that cannot be half-applied.
	for i := range out {
		out[i].ServiceAccountName = spec.ServiceAccountName
	}
	return out
}

// fromAnnotations builds the hook a pod declares itself, if it declares one. The command
// annotation is the trigger: without it there is nothing to run and the other three are inert.
func fromAnnotations(pod *corev1.Pod, phase Phase) (Resolved, bool) {
	prefix := apiconst.AnnotationPreBackupPrefix
	if phase == PhasePost {
		prefix = apiconst.AnnotationPostBackupPrefix
	}
	raw, ok := pod.Annotations[prefix+annCommand]
	if !ok || strings.TrimSpace(raw) == "" {
		return Resolved{}, false
	}

	h := Resolved{
		Pod:       types.NamespacedName{Namespace: pod.Namespace, Name: pod.Name},
		Container: defaultContainer(pod, pod.Annotations[prefix+annContainer]),
		Command:   parseCommand(raw),
		Timeout:   DefaultTimeout,
		OnError:   cbv1.HookErrorPolicyFail,
		Source:    SourceAnnotation,
		Phase:     phase,
	}
	if v := pod.Annotations[prefix+annTimeout]; v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			h.Timeout = d
		}
		// An unparseable timeout keeps the default rather than failing the hook: the annotation is
		// written by an application owner with no admission webhook behind it, and refusing to
		// quiesce a database because someone typed "30" instead of "30s" is the wrong trade.
	}
	if v := pod.Annotations[prefix+annOnError]; strings.EqualFold(v, string(cbv1.HookErrorPolicyContinue)) {
		h.OnError = cbv1.HookErrorPolicyContinue
	}
	return h, true
}

// fromSpec builds the hooks the CR declares for a pod, honouring each hook's podSelector. An empty
// selector matches every candidate pod — which, given the candidates are already the pods mounting
// the backed-up volumes, is a sensible "quiesce whatever holds this data" default rather than a
// dangerous wildcard.
func fromSpec(pod *corev1.Pod, spec cbv1.HooksSpec, phase Phase) []Resolved {
	declared := spec.Pre
	if phase == PhasePost {
		declared = spec.Post
	}
	var out []Resolved
	for i := range declared {
		h := &declared[i]
		if !selectorMatches(&h.PodSelector, pod.Labels) {
			continue
		}
		timeout := h.Timeout.Duration
		if timeout <= 0 {
			timeout = DefaultTimeout
		}
		onError := h.OnError
		if onError == "" {
			onError = cbv1.HookErrorPolicyFail
		}
		out = append(out, Resolved{
			Pod:       types.NamespacedName{Namespace: pod.Namespace, Name: pod.Name},
			Container: defaultContainer(pod, h.Container),
			Command:   append([]string(nil), h.Command...),
			Timeout:   timeout,
			OnError:   onError,
			Source:    SourceSpec,
			Phase:     phase,
		})
	}
	return out
}

// selectorMatches reports whether a pod's labels satisfy a selector. A nil or empty selector
// matches everything; a malformed one matches nothing, so a typo silently widening a hook's blast
// radius is impossible.
func selectorMatches(sel *metav1.LabelSelector, podLabels map[string]string) bool {
	if sel == nil || (len(sel.MatchLabels) == 0 && len(sel.MatchExpressions) == 0) {
		return true
	}
	s, err := metav1.LabelSelectorAsSelector(sel)
	if err != nil {
		return false
	}
	return s.Matches(labels.Set(podLabels))
}

// defaultContainer resolves which container to exec into: the named one when given, else the pod's
// FIRST container — the same convention Velero uses, and the one most single-workload pods make
// correct by construction.
func defaultContainer(pod *corev1.Pod, named string) string {
	if named != "" {
		return named
	}
	if len(pod.Spec.Containers) > 0 {
		return pod.Spec.Containers[0].Name
	}
	return ""
}

// parseCommand turns an annotation value into an argv. A leading '[' is parsed as a JSON array;
// anything else becomes a single-element argv, executed DIRECTLY rather than through a shell.
// That last part matters: wrapping in `sh -c` would silently give every hook shell metacharacter
// semantics — and a shell that may not exist in a distroless image.
func parseCommand(raw string) []string {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "[") {
		var argv []string
		if err := json.Unmarshal([]byte(raw), &argv); err == nil && len(argv) > 0 {
			return argv
		}
	}
	return []string{raw}
}

// ErrNoCommand is returned for a hook whose command resolved to nothing — a spec hook that passed
// validation with an empty list, say. Running it would exec an empty argv, whose API error is
// unhelpfully opaque.
var ErrNoCommand = errors.New("hook has no command")

// chainStopsOnAbort reports whether a failure whose policy is Fail must abandon the hooks BEHIND it
// in the same phase. It is the per-phase half of what Result.Aborts() used to answer alone, and
// separating them is 0.6.6 lot 2's second fix.
//
// # Why the answer differs by phase, and it is not a preference
//
// A PRE chain is one collective act with one product: a point in time that is worth trusting. The
// hooks in it are not independent — hook 3 quiescing a cache is meaningless once hook 2 failed to
// quiesce the database the cache fronts — and the moment any of them fails under `Fail`, R16 says
// there will be no snapshot at all (01-architecture.md §5). Continuing would be quiescing more
// applications, for longer, to produce nothing. Stopping is not merely allowed there, it is the
// kindest thing available.
//
// A POST chain is the exact opposite shape: every entry is a THAW OWED TO A DIFFERENT APPLICATION,
// and they have nothing to do with one another. Stopping at the first failure meant a permanently
// broken hook on the first pod left every later pod frozen — across all three attempts, because the
// retry restarts the chain from the beginning and dies at the same place. `UnfreezeFailed` did fire,
// so the operator was told loudly that something was stuck; it named the ONE pod whose command
// failed, and said nothing about the others, which were the ones a human could actually have
// unfrozen by hand. That is the defect this closes: the chain runs to the end, every entry gets a
// real result, and a broken thaw costs its own pod only.
//
// THE TEST IS `!= PhasePost` RATHER THAN `== PhasePre`, deliberately. A phase added later inherits
// the strict, chain-stopping behaviour until somebody argues otherwise for it, and the zero Phase —
// which unit tests and any future caller that forgets to stamp it produce — inherits it too. Failing
// closed on chain semantics costs at most some unrun hooks; failing open costs a quiesce nobody
// wanted, held while the rest of a doomed chain runs.
func chainStopsOnAbort(phase Phase) bool { return phase != PhasePost }

// Run executes hooks in order and returns one Result each, ALWAYS one per hook — including the
// ones skipped after an abort, so a caller reporting status can distinguish "did not run" from
// "ran and passed".
//
// In the PRE phase it stops at the first failure whose policy is Fail; in the POST phase it runs
// every hook whatever the earlier ones did (see chainStopsOnAbort for why the two phases cannot
// share one answer). Ordering is the caller's: Resolve preserves pod order and, within a pod, spec
// order.
//
// The per-hook phase is consulted rather than a phase parameter, and that is on purpose: Resolve
// stamps Phase on every hook it produces, so the fact is already carried by the data. A parameter
// would be a second source for it, and a caller that passed the wrong one would get a chain that
// stops in the release phase — which is precisely the bug being fixed, reintroduced by an argument
// list instead of by a predicate.
func Run(ctx context.Context, exec Executor, hooks []Resolved) []Result {
	results := make([]Result, 0, len(hooks))
	for i, h := range hooks {
		res := runOne(ctx, exec, h)
		results = append(results, res)
		if res.Aborts() && chainStopsOnAbort(h.Phase) {
			// Everything after an aborting hook is reported as not-run rather than dropped: a
			// status listing three hooks where the CR declares five invites the reader to assume
			// the other two passed.
			for _, skipped := range hooks[i+1:] {
				results = append(results, Result{Hook: skipped, Err: ErrSkipped})
			}
			break
		}
	}
	return results
}

// ErrSkipped marks a hook that never ran because an earlier one aborted the phase. Since 0.6.6 it
// can only appear in the PRE phase — a POST chain runs to the end (chainStopsOnAbort) — and the
// controller relies on that in both directions: it must not thaw a pod whose quiesce carries this
// marker (nothing ever froze it), and it must never see it on a release entry, because a thaw that
// "did not run" is a frozen application nobody is coming back for.
var ErrSkipped = errors.New("skipped: an earlier hook failed with onError=Fail")

// runOne executes a single hook under its own deadline.
//
// The deadline is the feature's sharpest edge. A hook that hangs holds a frozen database open for
// as long as it runs, so "timeout honoured" is not a nicety: the context is derived per hook, it
// is what the executor streams under, and a hook that overruns is failed rather than waited for.
func runOne(ctx context.Context, exec Executor, h Resolved) Result {
	if len(h.Command) == 0 {
		return Result{Hook: h, Err: ErrNoCommand}
	}
	timeout := h.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	hookCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	stdout, stderr, err := exec.Exec(hookCtx, h.Pod, h.Container, h.Command, h.ServiceAccountName)
	if err != nil {
		// Name the deadline explicitly. "context deadline exceeded" on its own sends an operator
		// looking at the network; "hook timed out after 30s" sends them to their own command.
		if errors.Is(hookCtx.Err(), context.DeadlineExceeded) {
			err = fmt.Errorf("hook timed out after %s: %w", timeout, err)
		}
		return Result{Hook: h, Err: err, Stdout: stdout, Stderr: stderr}
	}
	return Result{Hook: h, Stdout: stdout, Stderr: stderr}
}
