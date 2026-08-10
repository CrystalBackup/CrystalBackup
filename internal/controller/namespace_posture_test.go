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
//
// The recorder is eventCapture (reaper_honesty_test.go), this package's fake for the events.k8s.io
// API. It replaced record.NewFakeRecorder, which belongs to the core/v1 events API the check no
// longer uses — and it is an improvement rather than a translation: the fake recorder rendered each
// Event into one string, so these tests could only assert substrings of it and could not tell the
// reason from the note. The assertions below are now on the fields they were always aiming at.
func runPosture(t *testing.T, r client.Reader) []capturedEvent {
	t.Helper()
	rec := &eventCapture{}
	c := &NamespacePostureCheck{Reader: r, Namespace: "crystal-backup-system", Recorder: rec}
	if err := c.Start(t.Context()); err != nil {
		t.Fatalf("the posture check returned an error (%v). It must never be able to take the "+
			"operator down: an operator that exits on an upgrade of a cluster that has been "+
			"running without the labels turns a latent problem into an outage", err)
	}
	return rec.all()
}

// TestANilRecorderIsSafe. The field is documented as OPTIONAL, and the reason is not convenience:
// the check is a Runnable that must never be able to take the operator down, so a wiring mistake
// that left the recorder unset has to degrade to logs rather than panic on the one code path that
// only runs when something is already wrong.
func TestANilRecorderIsSafe(t *testing.T) {
	c := &NamespacePostureCheck{
		Reader:    postureReader{ns: namespaceWith(nil)},
		Namespace: "crystal-backup-system",
	}
	if err := c.Start(t.Context()); err != nil {
		t.Fatalf("the posture check with no recorder returned an error: %v", err)
	}
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
	if e.eventType != corev1.EventTypeWarning {
		t.Errorf("eventType = %q, want %q", e.eventType, corev1.EventTypeWarning)
	}
	if e.reason != "PodSecurityPostureWrong" {
		t.Errorf("reason = %q, want PodSecurityPostureWrong", e.reason)
	}
	// The `action` parameter exists only on events.k8s.io/v1 and is required to be non-empty by the
	// apiserver's own validation, so an empty one is a rejected Event: a finding that never arrives.
	if e.action == "" {
		t.Error("the Event carries no action; events.k8s.io/v1 validation rejects an empty one, " +
			"so this finding would be dropped by the apiserver rather than shown to anybody")
	}
	if e.objName != "crystal-backup-system" {
		t.Errorf("the Event is about %q, want the operator namespace itself", e.objName)
	}
	for _, want := range []string{
		"no pod-security.kubernetes.io/enforce label at all",
		"kubectl label namespace crystal-backup-system",
		"baseline",
	} {
		if !strings.Contains(e.note, want) {
			t.Errorf("the event does not contain %q — a reader must not have to translate the\n"+
				"diagnosis into an action:\n%s", want, e.note)
		}
	}
	// The note is what `kubectl describe` prints. eventCapture renders it through fmt.Sprintf the
	// way the apiserver will, so a stray verb in the built sentence shows up here as %!(NOVERB) or
	// %!s(MISSING) rather than in a customer's terminal.
	if strings.Contains(e.note, "%!") {
		t.Errorf("the note came out of formatting mangled:\n%s", e.note)
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
	if !strings.Contains(events[0].note, `"restricted"`) {
		t.Errorf("the event does not name the level it found:\n%s", events[0].note)
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
