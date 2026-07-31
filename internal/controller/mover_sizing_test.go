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
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/CrystalBackup/CrystalBackup/internal/mover"
)

// podScheme is the core scheme the pod-reading specs below build their fake client with.
func podScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	return s
}

// TestEveryMoverJobRequestCarriesTheProfileTable guards the one failure this whole feature is
// exposed to: a Job builder that does not pass Profiles.
//
// Nothing else would notice. mover.JobRequest.Profiles is nil-tolerant BY DESIGN (a nil table
// yields the built-in defaults, which is what keeps envtest and the unit tests from building
// BestEffort pods), so a call site that forgets it produces a perfectly valid, perfectly sized
// Job — sized from the BUILT-IN table, deaf to every override the platform configured. That is
// precisely the shape of the three M5 features that shipped "documented and completely inert":
// working code, correct-looking output, and a knob wired to nothing.
//
// A compile error is impossible here (a struct literal may omit any field), so this reads the
// package's own source and requires the field at every call. It parses the AST rather than
// grepping so that a `Profiles:` inside a comment or a string cannot satisfy it.
func TestEveryMoverJobRequestCarriesTheProfileTable(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the controller package directory: %v", err)
	}

	found := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || !isMoverBuildJob(call.Fun) || len(call.Args) != 1 {
				return true
			}
			found++
			lit, ok := unwrapJobRequestLiteral(call.Args[0])
			if !ok {
				// A caller that hands BuildJob a value built elsewhere (buildSyncJobRequest
				// does exactly this) is checked at that literal instead; find it there.
				return true
			}
			if !hasField(lit, "Profiles") {
				t.Errorf("%s:%d: mover.BuildJob(mover.JobRequest{…}) with no Profiles field — this Job "+
					"would be sized from the built-in table and ignore every platform override",
					name, fset.Position(call.Pos()).Line)
			}
			return true
		})
		// The indirect shape: any composite literal of type mover.JobRequest anywhere in the
		// package (buildSyncJobRequest returns one) must carry the field too.
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok || !isJobRequestType(lit.Type) {
				return true
			}
			if !hasField(lit, "Profiles") {
				t.Errorf("%s:%d: mover.JobRequest literal with no Profiles field — the Job it builds "+
					"would ignore every platform override", name, fset.Position(lit.Pos()).Line)
			}
			return true
		})
	}

	// A guard that inspects nothing passes. This package builds a Job for the backup, the restore,
	// the four manifest shapes, init, the maintenance ops, discovery's inventory and the sync copy;
	// if the count collapses, the walk broke, not the code.
	if found < 8 {
		t.Fatalf("found only %d mover.BuildJob call sites — the AST walk is not finding them", found)
	}
}

// isMoverBuildJob matches the selector `mover.BuildJob`.
func isMoverBuildJob(fun ast.Expr) bool {
	sel, ok := fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "BuildJob" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "mover"
}

// isJobRequestType matches the type expression `mover.JobRequest`.
func isJobRequestType(expr ast.Expr) bool {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "JobRequest" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "mover"
}

// unwrapJobRequestLiteral returns the composite literal a BuildJob call was given, if it was given
// one inline rather than a variable.
func unwrapJobRequestLiteral(arg ast.Expr) (*ast.CompositeLit, bool) {
	lit, ok := arg.(*ast.CompositeLit)
	if !ok || !isJobRequestType(lit.Type) {
		return nil, false
	}
	return lit, true
}

// hasField reports whether a composite literal sets the named field.
func hasField(lit *ast.CompositeLit, field string) bool {
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if ident, ok := kv.Key.(*ast.Ident); ok && ident.Name == field {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// The other half of shipping limits: what the operator SAYS when one of them bites.
// ---------------------------------------------------------------------------

// moverPod builds a pod of a mover Job, in the shape the kubelet leaves it in.
func moverPod(name, jobName string, mutate func(*corev1.Pod)) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: suiteOperatorNamespace,
			Labels:    map[string]string{batchv1.JobNameLabel: jobName},
		},
		Spec: corev1.PodSpec{NodeName: "node-1"},
	}
	mutate(pod)
	return pod
}

// TestEvictedMoverIsReportedAsEvicted is the legibility half of the cache sizeLimit.
//
// A pod that breaches an emptyDir sizeLimit is EVICTED: no ENOSPC, no error from restic, no
// termination message — the kubelet takes the pod away seconds later and (frequently) leaves no
// container status at all. Before this, that arrived on the Backup as "MoverCrashed", i.e. the
// operator's own cache cap diagnosed as a mover bug. The kubelet's message names the volume and
// the limit; it is what the operator needs and it is now what they get.
func TestEvictedMoverIsReportedAsEvicted(t *testing.T) {
	const jobName = "backup-team-a-pvc-1-mover"
	const kubeletMessage = `Usage of EmptyDir volume "restic-cache" exceeds the limit "20Gi".`

	c := fake.NewClientBuilder().WithScheme(podScheme(t)).WithObjects(
		moverPod("evicted-pod", jobName, func(p *corev1.Pod) {
			// An evicted pod: phase Failed, the reason on the POD, and no container status —
			// the shape that used to fall through to "no terminated mover pod found".
			p.Status = corev1.PodStatus{
				Phase:   corev1.PodFailed,
				Reason:  podReasonEvicted,
				Message: kubeletMessage,
			}
		}),
	).Build()

	_, node, err := readMoverResult(context.Background(), c, suiteOperatorNamespace, jobName)
	if err == nil {
		t.Fatal("readMoverResult() = nil error for an evicted mover, want a failure")
	}
	if node != "node-1" {
		t.Errorf("node = %q, want node-1 (the node that evicted it is part of the diagnosis)", node)
	}
	reason := moverFailureReason(mover.MoverResult{}, err)
	if !strings.HasPrefix(reason, "MoverEvicted") {
		t.Errorf("reason = %q, want it to start with MoverEvicted", reason)
	}
	if !strings.Contains(reason, "restic-cache") || !strings.Contains(reason, "20Gi") {
		t.Errorf("reason = %q, want the kubelet's own message naming the volume and the limit", reason)
	}
}

// TestOOMKilledMoverIsReportedAsOOMKilled — the memory-limit twin. Same silence from the mover,
// same need to point at the limit rather than at the code.
func TestOOMKilledMoverIsReportedAsOOMKilled(t *testing.T) {
	const jobName = "prune-repo-a-mover"

	c := fake.NewClientBuilder().WithScheme(podScheme(t)).WithObjects(
		moverPod("oom-pod", jobName, func(p *corev1.Pod) {
			p.Status = corev1.PodStatus{
				Phase: corev1.PodFailed,
				ContainerStatuses: []corev1.ContainerStatus{{
					State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
						ExitCode: 137,
						Reason:   containerReasonOOMKilled,
						Message:  "", // hard-killed: nothing written
					}},
				}},
			}
		}),
	).Build()

	_, _, err := readMoverResult(context.Background(), c, suiteOperatorNamespace, jobName)
	if err == nil {
		t.Fatal("readMoverResult() = nil error for an OOM-killed mover, want a failure")
	}
	if reason := moverFailureReason(mover.MoverResult{}, err); !strings.HasPrefix(reason, "MoverOOMKilled") {
		t.Errorf("reason = %q, want it to start with MoverOOMKilled", reason)
	}
}

// TestUnexplainedHardKillStaysMoverCrashed — the regression guard on the other side. A mover that
// died with a blank message and NO kubelet reason is still the pre-existing signal, and the
// stale-lock unlock still keys off it. Making eviction legible must not make everything "evicted".
func TestUnexplainedHardKillStaysMoverCrashed(t *testing.T) {
	const jobName = "backup-team-b-pvc-9-mover"

	c := fake.NewClientBuilder().WithScheme(podScheme(t)).WithObjects(
		moverPod("crashed-pod", jobName, func(p *corev1.Pod) {
			p.Status = corev1.PodStatus{
				Phase: corev1.PodFailed,
				ContainerStatuses: []corev1.ContainerStatus{{
					State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
						ExitCode: 137, Reason: "Error", Message: "",
					}},
				}},
			}
		}),
	).Build()

	_, _, err := readMoverResult(context.Background(), c, suiteOperatorNamespace, jobName)
	if err == nil {
		t.Fatal("readMoverResult() = nil error for a blank termination message, want a failure")
	}
	if reason := moverFailureReason(mover.MoverResult{}, err); reason != "MoverCrashed" {
		t.Errorf("reason = %q, want MoverCrashed", reason)
	}
}
