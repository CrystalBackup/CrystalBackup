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

package chart

import (
	"reflect"
	"strings"
	"testing"

	"github.com/CrystalBackup/CrystalBackup/internal/mover"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// The chart half of mover placement. The Go half proves the operator applies a placement it
// holds; these prove the chart is what puts one in its hands.
//
// The gap between the two is the whole defect class this milestone has been closing: a value an
// administrator sets, reads back in `helm get values`, and that never reaches a pod. Three of
// M5's features shipped documented and completely inert; `mover.profiles` is guarded against it
// by the same shape of test; this is the third knob to get the treatment.

const placementConfigMap = "crystal-backup-mover-placement"

// The chart's fullname helper is release-independent, so the Deployment is simply this.
const operatorDeployment = "crystal-backup"

// TestPlacementConfigMapIsAlwaysRendered: the volume, mount and flag are unconditional, so the
// object they point at has to be too. A missing ConfigMap here is not a missing feature — it is
// an operator Deployment that will not schedule, on every install that configures no placement,
// which is nearly all of them.
func TestPlacementConfigMapIsAlwaysRendered(t *testing.T) {
	data := placementDocument(t, mustRender(t))
	if strings.TrimSpace(data) != "" {
		t.Errorf("the default render's placement.yaml is %q, want an empty document — the "+
			"operator reads anything else as a placement it must apply", strings.TrimSpace(data))
	}
}

// TestPlacementReachesTheConfigMap moves all three fields off their defaults, because a knob
// whose default matches its wiring is a knob whose wiring is untested (soak_test.go's rule).
func TestPlacementReachesTheConfigMap(t *testing.T) {
	objs := mustRender(t,
		`mover.placement.nodeSelector.crystalbackup\.io/mover=true`,
		"mover.placement.tolerations[0].key=dedicated",
		"mover.placement.tolerations[0].operator=Equal",
		"mover.placement.tolerations[0].value=backup",
		"mover.placement.tolerations[0].effect=NoSchedule",
		"mover.placement.affinity.nodeAffinity.preferredDuringSchedulingIgnoredDuringExecution[0].weight=100",
		"mover.placement.affinity.nodeAffinity.preferredDuringSchedulingIgnoredDuringExecution[0]"+
			`.preference.matchExpressions[0].key=node\.kubernetes\.io/instance-type`,
		"mover.placement.affinity.nodeAffinity.preferredDuringSchedulingIgnoredDuringExecution[0]"+
			".preference.matchExpressions[0].operator=In",
		"mover.placement.affinity.nodeAffinity.preferredDuringSchedulingIgnoredDuringExecution[0]"+
			".preference.matchExpressions[0].values[0]=storage-optimised",
	)

	// Parsed by the OPERATOR'S OWN LOADER rather than matched with substrings. That is the whole
	// point of the test: the chart and the operator agree on this document or they do not, and a
	// grep for `key: dedicated` cannot tell the difference between agreement and a coincidence.
	// It also means the loader's strictness — invalid label keys, tolerations the API server would
	// refuse — is applied to what the chart actually produces.
	got, err := mover.LoadPlacement([]byte(placementDocument(t, objs)))
	if err != nil {
		t.Fatalf("the operator cannot parse what the chart rendered: %v", err)
	}

	want := mover.Placement{
		NodeSelector: map[string]string{"crystalbackup.io/mover": "true"},
		Tolerations: []corev1.Toleration{{
			Key: "dedicated", Operator: corev1.TolerationOpEqual,
			Value: "backup", Effect: corev1.TaintEffectNoSchedule,
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
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the placement the operator would hold is\n %+v\nwant\n %+v", got, want)
	}
}

// TestNodeSelectorValuesSurviveHelmSetTyping is the trap this template quotes its way out of.
//
// A Kubernetes label value is always a string, but neither YAML nor `--set` believes that:
// `--set mover.placement.nodeSelector.disk=true` produces a YAML boolean and `zone=3` an integer.
// The operator unmarshals into map[string]string and would refuse to start on either — correct
// behaviour, and a rotten first experience for a value that was never ambiguous.
func TestNodeSelectorValuesSurviveHelmSetTyping(t *testing.T) {
	objs := mustRender(t,
		"mover.placement.nodeSelector.disk=true",
		"mover.placement.nodeSelector.zone=3",
	)
	got, err := mover.LoadPlacement([]byte(placementDocument(t, objs)))
	if err != nil {
		t.Fatalf("a bare `--set nodeSelector.disk=true` stops the operator: %v", err)
	}
	if got.NodeSelector["disk"] != "true" || got.NodeSelector["zone"] != "3" {
		t.Errorf("NodeSelector = %v, want both values as strings", got.NodeSelector)
	}
}

// TestOperatorMountsAndReadsThePlacement walks the whole path in one test, because each link
// alone is satisfiable while the chain is broken: a ConfigMap nobody mounts, a mount nothing
// reads, a flag pointing at a path with no file behind it.
func TestOperatorMountsAndReadsThePlacement(t *testing.T) {
	var dep appsv1.Deployment
	convert(t, find(t, mustRender(t), "Deployment", operatorDeployment), &dep)

	const mountPath = "/etc/crystal-backup/mover-placement"
	const wantFlag = "--mover-placement-file=" + mountPath + "/placement.yaml"

	manager := dep.Spec.Template.Spec.Containers[0]
	if !containsString(manager.Args, wantFlag) {
		t.Errorf("the manager's args do not carry %q; they are %v", wantFlag, manager.Args)
	}

	var volumeName string
	for _, v := range dep.Spec.Template.Spec.Volumes {
		if v.ConfigMap != nil && v.ConfigMap.Name == placementConfigMap {
			volumeName = v.Name
		}
	}
	if volumeName == "" {
		t.Fatalf("no volume in the Deployment references ConfigMap %q", placementConfigMap)
	}

	var mounted *corev1.VolumeMount
	for i := range manager.VolumeMounts {
		if manager.VolumeMounts[i].Name == volumeName {
			mounted = &manager.VolumeMounts[i]
		}
	}
	if mounted == nil {
		t.Fatalf("volume %q is declared but never mounted into the manager — the flag above points "+
			"at a path with nothing behind it, and the operator would start with no placement at all "+
			"while `helm get values` showed one", volumeName)
	}
	if mounted.MountPath != mountPath {
		t.Errorf("volume %q is mounted at %q, but the flag reads %q", volumeName, mounted.MountPath, mountPath)
	}
}

// TestPlacementChangeRollsTheOperator: the file is read ONCE, at startup. Without a checksum
// annotation over it, a `helm upgrade` that adds a nodeSelector updates the ConfigMap, reports
// success, and changes nothing at all until the next unrelated restart.
func TestPlacementChangeRollsTheOperator(t *testing.T) {
	sum := func(values ...string) string {
		t.Helper()
		var dep appsv1.Deployment
		convert(t, find(t, mustRender(t, values...), "Deployment", operatorDeployment), &dep)
		got := dep.Spec.Template.Annotations["checksum/mover-placement"]
		if got == "" {
			t.Fatal("the pod template carries no checksum/mover-placement annotation")
		}
		return got
	}

	before := sum()
	after := sum(`mover.placement.nodeSelector.crystalbackup\.io/mover=true`)
	if before == after {
		t.Errorf("adding a nodeSelector left checksum/mover-placement at %q — the operator would "+
			"not restart, so the new placement would reach no mover until something else rolled it",
			before)
	}
}

// placementDocument returns the placement.yaml the chart rendered — the exact bytes the operator
// will parse at startup.
func placementDocument(t *testing.T, objs []*unstructured.Unstructured) string {
	t.Helper()
	cm := find(t, objs, "ConfigMap", placementConfigMap)
	data, found, err := unstructured.NestedString(cm.Object, "data", "placement.yaml")
	if err != nil || !found {
		t.Fatalf("reading data.placement.yaml from %s: found=%v err=%v", placementConfigMap, found, err)
	}
	return data
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
