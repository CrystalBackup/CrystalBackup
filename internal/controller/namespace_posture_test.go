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
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// postureReader is a client.Reader over one namespace, or an error. Narrow on purpose: the check
// makes exactly one Get and a fake with more surface would invite testing something else.
type postureReader struct {
	ns  *corev1.Namespace
	err error
}

func (r postureReader) Get(_ context.Context, _ client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
	if r.err != nil {
		return r.err
	}
	*(obj.(*corev1.Namespace)) = *r.ns
	return nil
}

func (r postureReader) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return errors.New("the posture check must not list anything")
}

func namespaceWith(labels map[string]string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "crystal-backup-system", Labels: labels}}
}

// runPosture executes the check and returns the events it emitted.
func runPosture(t *testing.T, r client.Reader) []string {
	t.Helper()
	rec := record.NewFakeRecorder(4)
	c := &NamespacePostureCheck{Reader: r, Namespace: "crystal-backup-system", Recorder: rec}
	if err := c.Start(t.Context()); err != nil {
		t.Fatalf("the posture check returned an error (%v). It must never be able to take the "+
			"operator down: an operator that exits on an upgrade of a cluster that has been "+
			"running without the labels turns a latent problem into an outage", err)
	}
	close(rec.Events)
	var out []string
	for e := range rec.Events {
		out = append(out, e)
	}
	return out
}

// TestAnUnlabelledOperatorNamespaceIsReportedLoudly is the case the chart's own guard structurally
// cannot see: `helm install --create-namespace` makes the namespace AFTER rendering, and
// `helm template` (Argo CD, Flux) has no cluster to look at. Both leave the namespace with no Pod
// Security labels and the template guard passing vacuously.
//
// The symptom without this check appears later and elsewhere — a mover pod refused by admission
// during a backup, or during a restore, which is the worst moment. So the operator says it about
// itself, on startup, where nobody has to have been watching.
func TestAnUnlabelledOperatorNamespaceIsReportedLoudly(t *testing.T) {
	events := runPosture(t, postureReader{ns: namespaceWith(nil)})

	if len(events) != 1 {
		t.Fatalf("got %d event(s), want exactly 1: %v", len(events), events)
	}
	e := events[0]
	for _, want := range []string{
		"Warning", "PodSecurityPostureWrong",
		"no pod-security.kubernetes.io/enforce label at all",
		"kubectl label namespace crystal-backup-system",
		"baseline",
	} {
		if !strings.Contains(e, want) {
			t.Errorf("the event does not contain %q — a reader must not have to translate the\n"+
				"diagnosis into an action:\n%s", want, e)
		}
	}
}

// TestARestrictedOperatorNamespaceIsReportedToo: the other real shape. `restricted` is what a
// security-conscious platform team applies by default, and it is precisely the level that DENIES
// the movers — they run uid 0 with DAC_OVERRIDE to preserve file ownership on restore. A namespace
// that looks MORE secure is the one that silently stops backups working.
func TestARestrictedOperatorNamespaceIsReportedToo(t *testing.T) {
	events := runPosture(t, postureReader{ns: namespaceWith(map[string]string{
		psaEnforceLabel: "restricted",
	})})
	if len(events) != 1 {
		t.Fatalf("a `restricted` operator namespace raised %d event(s), want 1: %v", len(events), events)
	}
	if !strings.Contains(events[0], `"restricted"`) {
		t.Errorf("the event does not name the level it found:\n%s", events[0])
	}
}

// TestACorrectlyLabelledNamespaceSaysNothing. An alarm that fires on a healthy install is an alarm
// nobody reads on the day it is real — the same reason the soak's `unknown` bucket stopped being
// flagged.
func TestACorrectlyLabelledNamespaceSaysNothing(t *testing.T) {
	events := runPosture(t, postureReader{ns: namespaceWith(map[string]string{
		psaEnforceLabel: psaRequiredEnforce,
		"audit":         "restricted",
	})})
	if len(events) != 0 {
		t.Errorf("a correctly labelled namespace raised %d event(s): %v", len(events), events)
	}
}

// TestAnUnreadableNamespaceConcludesNothing. An RBAC gap or an apiserver having a bad second says
// nothing about the labels, and reporting "posture wrong" there would be the mirror of the
// absence-reads-as-health mistake this project keeps finding: a failure invented from an absence.
func TestAnUnreadableNamespaceConcludesNothing(t *testing.T) {
	events := runPosture(t, postureReader{err: errors.New("namespaces is forbidden")})
	if len(events) != 0 {
		t.Errorf("an unreadable namespace produced a posture verdict: %v", events)
	}
}

// TestPostureProblemAlwaysCarriesTheFix pins the one property the sentence must keep however it is
// reworded. It is reached by somebody who did not choose to be there.
func TestPostureProblemAlwaysCarriesTheFix(t *testing.T) {
	for _, got := range []string{"", "restricted", "privileged", "nonsense"} {
		msg := PostureProblem("some-ns", got)
		if !strings.Contains(msg, "kubectl label namespace some-ns "+psaEnforceLabel+"="+psaRequiredEnforce) {
			t.Errorf("PostureProblem(%q) does not end with the command that fixes it:\n%s", got, msg)
		}
	}
}
