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

package mover

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// samplePlacement is the shape the values.yaml example documents: a hard selector, a toleration
// for a reserved pool, and a soft node-affinity preference.
func samplePlacement() Placement {
	return Placement{
		NodeSelector: map[string]string{"crystalbackup.io/mover": "true"},
		Tolerations: []corev1.Toleration{{
			Key:      "dedicated",
			Operator: corev1.TolerationOpEqual,
			Value:    "backup",
			Effect:   corev1.TaintEffectNoSchedule,
		}},
		Affinity: &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
			PreferredDuringSchedulingIgnoredDuringExecution: []corev1.PreferredSchedulingTerm{{
				Weight: 100,
				Preference: corev1.NodeSelectorTerm{MatchExpressions: []corev1.NodeSelectorRequirement{{
					Key:      "node.kubernetes.io/instance-type",
					Operator: corev1.NodeSelectorOpIn,
					Values:   []string{"storage-optimised"},
				}}},
			}},
		}},
	}
}

// TestUnconfiguredPlacementChangesNothing is the compatibility pin the whole feature rests on.
//
// Placement is applied to EVERY mover Job in the product, so if the zero value were not exactly
// inert, this release would silently change the pod spec of every backup on every install that
// asked for nothing. The three fields are asserted nil rather than empty: an empty non-nil
// affinity object, or an empty tolerations slice, would serialise into the Job and show up in
// every `kubectl get job -o yaml` as a difference nobody chose.
func TestUnconfiguredPlacementChangesNothing(t *testing.T) {
	spec := BuildJob(JobRequest{Operation: OpBackup}).Spec.Template.Spec

	if spec.NodeSelector != nil {
		t.Errorf("NodeSelector = %v, want nil on an install that configures no placement", spec.NodeSelector)
	}
	if spec.Tolerations != nil {
		t.Errorf("Tolerations = %v, want nil on an install that configures no placement", spec.Tolerations)
	}
	if spec.Affinity != nil {
		t.Errorf("Affinity = %+v, want nil on an install that configures no placement", spec.Affinity)
	}
}

func TestPlacementReachesThePodSpec(t *testing.T) {
	p := samplePlacement()
	spec := BuildJob(JobRequest{Operation: OpBackup, Placement: p}).Spec.Template.Spec

	if !reflect.DeepEqual(spec.NodeSelector, p.NodeSelector) {
		t.Errorf("NodeSelector = %v, want %v", spec.NodeSelector, p.NodeSelector)
	}
	if !reflect.DeepEqual(spec.Tolerations, p.Tolerations) {
		t.Errorf("Tolerations = %+v, want %+v", spec.Tolerations, p.Tolerations)
	}
	if !reflect.DeepEqual(spec.Affinity, p.Affinity) {
		t.Errorf("Affinity = %+v, want %+v", spec.Affinity, p.Affinity)
	}
}

// TestPlacementAppliesToEveryOperation is the "every mover, not most movers" claim, checked
// rather than asserted in a comment. The failure this guards against is not a bug anybody would
// write on purpose — it is a future `if req.Operation == OpBackup` added for a good local reason,
// which would quietly leave prune scheduling on a node the administrator excluded.
func TestPlacementAppliesToEveryOperation(t *testing.T) {
	p := samplePlacement()
	for _, op := range Operations() {
		spec := BuildJob(JobRequest{Operation: op, Placement: p}).Spec.Template.Spec
		if spec.NodeSelector["crystalbackup.io/mover"] != "true" {
			t.Errorf("operation %q: NodeSelector = %v, want the platform's placement", op, spec.NodeSelector)
		}
		if len(spec.Tolerations) != 1 || spec.Affinity == nil {
			t.Errorf("operation %q: tolerations=%d affinity=%v, want both from the platform's placement",
				op, len(spec.Tolerations), spec.Affinity != nil)
		}
	}
}

// TestPinnedJobKeepsOnlyTheTolerations is the load-bearing interaction, and the one a reader is
// most likely to think is a mistake.
//
// A same-node restore names its node directly (adr/0016 §2) because an RWO volume attached on one
// node can only be mounted there. The kubelet re-checks nodeSelector and nodeAffinity on
// admission even for a pod it never scheduled, so carrying them onto a pinned pod does not move
// it anywhere better — it gets it REJECTED, on the one operation with no second choice. The
// toleration is the only one of the three that still does work after the node is chosen: a
// NoExecute taint is enforced by the taint manager against running pods however they were placed.
func TestPinnedJobKeepsOnlyTheTolerations(t *testing.T) {
	spec := BuildJob(JobRequest{
		Operation: OpRestore,
		NodeName:  "worker-7",
		Placement: samplePlacement(),
	}).Spec.Template.Spec

	if spec.NodeName != "worker-7" {
		t.Fatalf("NodeName = %q, want the pin to survive", spec.NodeName)
	}
	if spec.NodeSelector != nil {
		t.Errorf("NodeSelector = %v on a node-pinned Job, want nil: the kubelet would reject the "+
			"pod outright rather than place it better", spec.NodeSelector)
	}
	if spec.Affinity != nil {
		t.Errorf("Affinity = %+v on a node-pinned Job, want nil, for the same reason", spec.Affinity)
	}
	if len(spec.Tolerations) != 1 || spec.Tolerations[0].Key != "dedicated" {
		t.Errorf("Tolerations = %+v, want the platform's kept: a NoExecute taint evicts a pinned "+
			"pod mid-restore just as readily as a scheduled one", spec.Tolerations)
	}
}

// TestPlacementIsNotAliasedByTheJob matters because ONE Placement is shared by every Job the
// operator will ever build. A Job whose pod spec aliased it would let a client library's
// defaulting, a mutating webhook round-trip, or a controller editing "its own" object reach back
// into the operator's configuration and change where every subsequent mover in the cluster runs.
func TestPlacementIsNotAliasedByTheJob(t *testing.T) {
	p := samplePlacement()
	spec := BuildJob(JobRequest{Operation: OpBackup, Placement: p}).Spec.Template.Spec

	spec.NodeSelector["crystalbackup.io/mover"] = "tampered"
	spec.Tolerations[0].Value = "tampered"
	spec.Affinity.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution[0].Weight = 1

	if p.NodeSelector["crystalbackup.io/mover"] != "true" {
		t.Errorf("editing the Job's NodeSelector reached the operator's placement: %v", p.NodeSelector)
	}
	if p.Tolerations[0].Value != "backup" {
		t.Errorf("editing the Job's Tolerations reached the operator's placement: %+v", p.Tolerations)
	}
	if w := p.Affinity.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution[0].Weight; w != 100 {
		t.Errorf("editing the Job's Affinity reached the operator's placement: weight = %d", w)
	}
}

// --- loading -------------------------------------------------------------------------------

func TestLoadPlacementRoundTrip(t *testing.T) {
	got, err := LoadPlacement([]byte(`
nodeSelector:
  crystalbackup.io/mover: "true"
tolerations:
  - key: dedicated
    operator: Equal
    value: backup
    effect: NoSchedule
affinity:
  nodeAffinity:
    preferredDuringSchedulingIgnoredDuringExecution:
      - weight: 100
        preference:
          matchExpressions:
            - key: node.kubernetes.io/instance-type
              operator: In
              values: [storage-optimised]
`))
	if err != nil {
		t.Fatalf("LoadPlacement: %v", err)
	}
	if !reflect.DeepEqual(got, samplePlacement()) {
		t.Errorf("round trip differs:\n got %+v\nwant %+v", got, samplePlacement())
	}
}

// TestEmptyPlacementFileIsNotAnError covers the normal install: the chart renders the ConfigMap
// unconditionally, so on every cluster that configures nothing the operator reads an empty
// document — and must treat it as "schedule anywhere", not as a reason to refuse to start.
func TestEmptyPlacementFileIsNotAnError(t *testing.T) {
	for name, data := range map[string]string{
		"empty":         "",
		"empty mapping": "{}\n",
		"null":          "null\n",
		"comment only":  "# nothing configured\n",
	} {
		got, err := LoadPlacement([]byte(data))
		if err != nil {
			t.Errorf("%s: LoadPlacement: %v", name, err)
			continue
		}
		if !got.IsZero() {
			t.Errorf("%s: got %+v, want the zero placement", name, got)
		}
	}
}

// TestEmptyAffinityIsNormalisedAway is what stops `affinity: {}` — which is exactly what a Helm
// `toYaml` of an unset default produces — from putting an empty affinity object into every mover
// pod in the cluster. Normalising in the loader rather than the template is deliberate: the
// kustomize install and a hand-written ConfigMap go through here too.
func TestEmptyAffinityIsNormalisedAway(t *testing.T) {
	got, err := LoadPlacement([]byte("nodeSelector: {}\ntolerations: []\naffinity: {}\n"))
	if err != nil {
		t.Fatalf("LoadPlacement: %v", err)
	}
	if !got.IsZero() {
		t.Fatalf("got %+v, want the zero placement — an empty affinity must not reach a pod", got)
	}
}

// TestLoadPlacementRejects is the startup gate. Every case here is a placement that would
// otherwise be accepted by the chart, be visible in `helm get values`, and produce either no pod
// at all or a pod on a node that cannot mount the volume — at whatever hour the schedule fires.
func TestLoadPlacementRejects(t *testing.T) {
	cases := map[string]struct{ yaml, wantSubstr string }{
		// The typo that motivated strictness: a plural that reads correctly, parses as nothing,
		// and leaves every mover scheduling anywhere while `helm get values` shows the selector.
		// (A MIS-CASED key such as `nodeselector:` is accepted, because the decoder matches JSON
		// field names case-insensitively — as the API server does. That is leniency about how a
		// known field is spelled, not about whether it exists.)
		"unknown field": {
			yaml:       "nodeSelectors:\n  foo: bar\n",
			wantSubstr: "parse mover placement",
		},
		"invalid label key": {
			yaml:       "nodeSelector:\n  \"not a key\": bar\n",
			wantSubstr: "nodeSelector key",
		},
		"invalid label value": {
			yaml:       "nodeSelector:\n  kubernetes.io/os: \"a value with spaces\"\n",
			wantSubstr: "value",
		},
		"Exists with a value": {
			yaml:       "tolerations:\n  - key: dedicated\n    operator: Exists\n    value: backup\n",
			wantSubstr: "operator Exists takes no value",
		},
		"unknown toleration operator": {
			yaml:       "tolerations:\n  - key: dedicated\n    operator: Matches\n",
			wantSubstr: "is not one of",
		},
		"unknown taint effect": {
			yaml:       "tolerations:\n  - key: dedicated\n    operator: Exists\n    effect: NoBackups\n",
			wantSubstr: "effect",
		},
		"tolerationSeconds without NoExecute": {
			yaml:       "tolerations:\n  - key: dedicated\n    operator: Exists\n    effect: NoSchedule\n    tolerationSeconds: 30\n",
			wantSubstr: "tolerationSeconds only applies",
		},
		"required affinity with no terms": {
			yaml: "affinity:\n  nodeAffinity:\n    requiredDuringSchedulingIgnoredDuringExecution:\n" +
				"      nodeSelectorTerms: []\n",
			wantSubstr: "matches NO node",
		},
		"In with no values": {
			yaml: "affinity:\n  nodeAffinity:\n    requiredDuringSchedulingIgnoredDuringExecution:\n" +
				"      nodeSelectorTerms:\n        - matchExpressions:\n            - key: kubernetes.io/os\n" +
				"              operator: In\n",
			wantSubstr: "requires at least one value",
		},
		"weight out of range": {
			yaml: "affinity:\n  nodeAffinity:\n    preferredDuringSchedulingIgnoredDuringExecution:\n" +
				"      - weight: 0\n        preference:\n          matchExpressions:\n" +
				"            - key: kubernetes.io/os\n              operator: Exists\n",
			wantSubstr: "outside 1..100",
		},
		"Gt on a non-integer": {
			yaml: "affinity:\n  nodeAffinity:\n    preferredDuringSchedulingIgnoredDuringExecution:\n" +
				"      - weight: 10\n        preference:\n          matchExpressions:\n" +
				"            - key: cores\n              operator: Gt\n              values: [\"many\"]\n",
			wantSubstr: "compares integers",
		},
	}

	for name, tc := range cases {
		got, err := LoadPlacement([]byte(tc.yaml))
		if err == nil {
			t.Errorf("%s: LoadPlacement accepted it and returned %+v; the operator would start on "+
				"a placement that cannot work", name, got)
			continue
		}
		if !strings.Contains(err.Error(), tc.wantSubstr) {
			t.Errorf("%s: error %q does not contain %q — the message is what the admin reading the "+
				"crash-looping operator has to work from", name, err, tc.wantSubstr)
		}
	}
}

// TestLoadPlacementFileTolerance pins the two non-error cases, which exist for the kustomize
// install that mounts no ConfigMap at all. A file that EXISTS and is broken stays fatal.
func TestLoadPlacementFileTolerance(t *testing.T) {
	got, err := LoadPlacementFile("")
	if err != nil || !got.IsZero() {
		t.Errorf("empty path: got (%+v, %v), want the zero placement and no error", got, err)
	}

	missing := filepath.Join(t.TempDir(), "absent.yaml")
	got, err = LoadPlacementFile(missing)
	if err != nil || !got.IsZero() {
		t.Errorf("missing file: got (%+v, %v), want the zero placement and no error", got, err)
	}

	broken := filepath.Join(t.TempDir(), "placement.yaml")
	if err := os.WriteFile(broken, []byte("nodeSelector:\n  \"not a key\": x\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := LoadPlacementFile(broken); err == nil {
		t.Error("a file that exists and is broken must stop the operator, not be tolerated")
	}
}

// TestPlacementStringReportsShape covers the startup log line, which is the only way an
// administrator can answer "did my placement reach the operator" without catching a mover Job
// mid-flight — mover pods live seconds.
func TestPlacementStringReportsShape(t *testing.T) {
	if got := (Placement{}).String(); !strings.Contains(got, "none") {
		t.Errorf("zero placement logs %q, want it to say plainly that movers schedule anywhere", got)
	}
	got := samplePlacement().String()
	for _, want := range []string{"crystalbackup.io/mover=true", "tolerations[1]", "affinity[set]"} {
		if !strings.Contains(got, want) {
			t.Errorf("placement logs %q, missing %q", got, want)
		}
	}
}
