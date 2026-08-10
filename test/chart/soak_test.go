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

// Package chart proves what the Helm chart RENDERS, by rendering it — `helm template` against
// charts/crystal-backup, never a test-local copy of the YAML that could drift from it. The suite
// needs the `helm` binary and fails with a clear message when it is absent, exactly as
// test/admission does.
//
// What it is here to prevent: a value that reads back nicely and reaches nothing. That failure has
// happened in this repository more than once — three M5 features shipped documented and inert, and
// JobRequest.GoMemLimit sat unset from M1 until 0.6.1 — and it is invisible to `helm lint`, to
// `helm template` read by eye, and to every Go test that does not look at rendered output. So
// every `soak.*` value is set to a NON-DEFAULT here and then found in the rendered pod spec: a
// knob that stopped being wired fails this file rather than a fortnight of somebody's soak.
package chart

import (
	"bytes"
	"fmt"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

const (
	releaseName       = "soaktest"
	operatorNamespace = "crystal-backup-system"
)

func chartDir() string { return filepath.Join("..", "..", "charts", "crystal-backup") }

// render runs `helm template` and returns the objects, or the error output when it fails. A
// template-time refusal is a first-class outcome here, not an accident: three of soak.yaml's
// guards exist precisely so a broken combination dies at render rather than in a CrashLoopBackOff
// on somebody's cluster.
func render(t *testing.T, values ...string) ([]*unstructured.Unstructured, string, error) {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Fatalf("the helm binary is required to render the chart: %v", err)
	}
	args := make([]string, 0, 5+2*len(values))
	args = append(args, "template", releaseName, chartDir(), "--namespace", operatorNamespace)
	for _, v := range values {
		args = append(args, "--set", v)
	}
	cmd := exec.Command("helm", args...)
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		return nil, stderr.String(), err
	}

	var objs []*unstructured.Unstructured
	decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(out.Bytes()), 4096)
	for {
		var raw map[string]interface{}
		if err := decoder.Decode(&raw); err != nil {
			break
		}
		if len(raw) == 0 {
			continue
		}
		objs = append(objs, &unstructured.Unstructured{Object: raw})
	}
	if len(objs) == 0 {
		t.Fatalf("helm template produced no objects\n%s", stderr.String())
	}
	return objs, stderr.String(), nil
}

func mustRender(t *testing.T, values ...string) []*unstructured.Unstructured {
	t.Helper()
	objs, stderr, err := render(t, values...)
	if err != nil {
		t.Fatalf("helm template %v: %v\n%s", values, err, stderr)
	}
	return objs
}

func find(t *testing.T, objs []*unstructured.Unstructured, kind, name string) *unstructured.Unstructured {
	t.Helper()
	for _, o := range objs {
		if o.GetKind() == kind && o.GetName() == name {
			return o
		}
	}
	t.Fatalf("no %s named %q in the render", kind, name)
	return nil
}

func lookup(objs []*unstructured.Unstructured, kind, name string) *unstructured.Unstructured {
	for _, o := range objs {
		if o.GetKind() == kind && o.GetName() == name {
			return o
		}
	}
	return nil
}

func convert[T any](t *testing.T, u *unstructured.Unstructured, out *T) *T {
	t.Helper()
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, out); err != nil {
		t.Fatalf("convert %s/%s: %v", u.GetKind(), u.GetName(), err)
	}
	return out
}

// soakEnabled is the render every assertion below reads, with EVERY soak value moved off its
// default. Defaults would pass a test that asserts nothing: a hardcoded flag and a wired one look
// identical when the value happens to match.
//
// Two soak values are deliberately LEFT at their defaults here — soak.accessModes and
// soak.storageClassName — because the default is the assertion for both: this render is what a
// plain cluster with an RWO default class gets, and TestSoakDefaultsToWhatAPlainClusterHas pins that
// it stays that way. They are moved off their defaults in
// TestSoakAccessModeAndClassRemoveTheHandover, which is where "the knob reaches the manifest" is
// proved.
var soakEnabled = []string{
	"soak.enabled=true",
	"soak.storage=7Gi",
	"soak.maxBytes=333Mi",
	"soak.metricsInterval=97s",
	"soak.metricsResolution=11m",
	"soak.moverSampleInterval=17s",
	"soak.selfcheckInterval=13h",
	"soak.stateInterval=41m",
}

func soakDeployment(t *testing.T, objs []*unstructured.Unstructured) *appsv1.Deployment {
	t.Helper()
	var d appsv1.Deployment
	return convert(t, find(t, objs, "Deployment", "crystal-backup-soak"), &d)
}

// TestSoakValuesReachFlags is the one this file exists for. Every value, to the flag it must land
// on.
func TestSoakValuesReachFlags(t *testing.T) {
	objs := mustRender(t, soakEnabled...)
	d := soakDeployment(t, objs)
	if n := len(d.Spec.Template.Spec.Containers); n != 1 {
		t.Fatalf("the collector pod has %d containers, want 1", n)
	}
	args := d.Spec.Template.Spec.Containers[0].Args

	for value, flag := range map[string]string{
		"soak.maxBytes=333Mi":            "--max-bytes=333Mi",
		"soak.metricsInterval=97s":       "--metrics-interval=97s",
		"soak.metricsResolution=11m":     "--metrics-resolution=11m",
		"soak.moverSampleInterval=17s":   "--mover-sample-interval=17s",
		"soak.selfcheckInterval=13h":     "--selfcheck-interval=13h",
		"soak.stateInterval=41m":         "--state-interval=41m",
		"soak.saltMethod=auto (default)": "--salt-method=auto",
	} {
		if !hasArg(args, flag) {
			t.Errorf("%s did not reach the pod: no %q in %v", value, flag, args)
		}
	}
	if args[0] != "soak-collect" {
		t.Errorf("the collector's first arg is %q, want the subcommand soak-collect", args[0])
	}

	// soak.storage reaches the PVC rather than a flag; the cap that the collector honours is the
	// flag above, and these two being different numbers is deliberate (see the PVC's comment).
	var pvc corev1.PersistentVolumeClaim
	convert(t, find(t, objs, "PersistentVolumeClaim", "crystal-backup-soak-data"), &pvc)
	if got := pvc.Spec.Resources.Requests.Storage().String(); got != "7Gi" {
		t.Errorf("soak.storage did not reach the PVC: got %s, want 7Gi", got)
	}
}

// TestSoakImageIsTheOperatorImage is the whole reason this moved into the chart. Not "an image
// value that defaults to the same thing" — the same resolved reference, from the same helper, so
// there is nothing to hand-copy and no way for the two to disagree.
func TestSoakImageIsTheOperatorImage(t *testing.T) {
	const digest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	objs := mustRender(t, append(append([]string{}, soakEnabled...), "image.digest="+digest)...)

	var operator appsv1.Deployment
	convert(t, find(t, objs, "Deployment", "crystal-backup"), &operator)
	want := operator.Spec.Template.Spec.Containers[0].Image
	if !strings.HasSuffix(want, digest) {
		t.Fatalf("the operator Deployment did not take the digest under test: %s", want)
	}

	got := soakDeployment(t, objs).Spec.Template.Spec.Containers[0].Image
	if got != want {
		t.Errorf("the collector runs %s and the operator runs %s — soaking a build other than the "+
			"one under test is the defect this lot exists to remove", got, want)
	}
}

// TestSoakDisabledRendersNothing: off must mean absent, not merely inert.
func TestSoakDisabledRendersNothing(t *testing.T) {
	objs := mustRender(t)
	for _, o := range objs {
		if strings.Contains(strings.ToLower(o.GetName()), "soak") {
			t.Errorf("soak.enabled=false still rendered %s/%s", o.GetKind(), o.GetName())
		}
		for k, v := range o.GetLabels() {
			if strings.Contains(k, "soak") || v == "soak" {
				t.Errorf("soak.enabled=false left a soak label %s=%s on %s/%s",
					k, v, o.GetKind(), o.GetName())
			}
		}
	}
}

// TestSoakSaltMethods: both spellings of the Secret method reach the flag's own vocabulary, and
// the mount comes with it. The salt is never generated by the chart — there is no Secret in the
// render, only a reference to one the administrator made.
func TestSoakSaltMethods(t *testing.T) {
	for _, spelling := range []string{"fromSecret", "from-secret"} {
		t.Run(spelling, func(t *testing.T) {
			objs := mustRender(t, append(append([]string{}, soakEnabled...),
				"soak.saltMethod="+spelling, "soak.saltSecret=my-soak-salt")...)
			d := soakDeployment(t, objs)
			c := d.Spec.Template.Spec.Containers[0]
			if !hasArg(c.Args, "--salt-method=from-secret") {
				t.Errorf("soak.saltMethod=%s did not reach --salt-method=from-secret: %v", spelling, c.Args)
			}
			const saltFile = "--redaction-salt-file=/etc/crystal-backup/soak/salt"
			if !hasArg(c.Args, saltFile) {
				t.Errorf("no %s in %v", saltFile, c.Args)
			}
			var mounted bool
			for _, m := range c.VolumeMounts {
				if m.Name == "salt" && m.MountPath == "/etc/crystal-backup/soak" && m.ReadOnly {
					mounted = true
				}
			}
			if !mounted {
				t.Errorf("the salt Secret is not mounted read-only where the flag points: %v", c.VolumeMounts)
			}
			var referenced bool
			for _, v := range d.Spec.Template.Spec.Volumes {
				if v.Name == "salt" && v.Secret != nil && v.Secret.SecretName == "my-soak-salt" {
					referenced = true
				}
			}
			if !referenced {
				t.Errorf("soak.saltSecret did not reach the volume: %v", d.Spec.Template.Spec.Volumes)
			}
			// The chart generates NO salt, ever. A regenerated salt changes nothing visible and
			// silently destroys the correlation the whole soak exists for (internal/soak/salt.go).
			for _, o := range objs {
				if o.GetKind() == "Secret" && strings.Contains(o.GetName(), "soak") {
					t.Errorf("the chart rendered a soak Secret (%s); it must never generate the salt",
						o.GetName())
				}
			}
		})
	}

	// auto is the default, derives from the namespace UID, and mounts nothing.
	objs := mustRender(t, soakEnabled...)
	d := soakDeployment(t, objs)
	for _, v := range d.Spec.Template.Spec.Volumes {
		if v.Name == "salt" {
			t.Errorf("saltMethod=auto mounted a salt volume; it derives and reads no Secret")
		}
	}
	if hasArgPrefix(d.Spec.Template.Spec.Containers[0].Args, "--redaction-salt-file") {
		t.Errorf("saltMethod=auto passed --redaction-salt-file, which soak-collect refuses at startup")
	}
}

// TestSoakTemplateRefusals: every combination that would produce a collector which starts and
// collects the wrong thing (or nothing) has to die at template time.
func TestSoakTemplateRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		values []string
		expect string
	}{
		"fromSecret with no saltSecret": {
			values: []string{"soak.enabled=true", "soak.saltMethod=fromSecret"},
			expect: "soak.saltSecret",
		},
		"auto with a saltSecret": {
			values: []string{"soak.enabled=true", "soak.saltSecret=orphan"},
			expect: "would never read the Secret",
		},
		"an unknown salt method": {
			values: []string{"soak.enabled=true", "soak.saltMethod=random"},
			expect: "is not a salt method",
		},
		"no metrics Service to scrape": {
			values: []string{"soak.enabled=true", "metrics.service.create=false"},
			expect: "requires metrics.service.create",
		},
		"no metrics-reader ClusterRole": {
			values: []string{"soak.enabled=true", "rbac.create=false"},
			expect: "requires rbac.create",
		},
		// ReadOnlyMany binds and mounts and then the collector cannot write a byte to it, which is
		// a CrashLoopBackOff on somebody's cluster rather than a render error, and the archive it
		// would have produced does not exist. The other two real modes are refused for reasons the
		// message states; anything misspelled is refused with them.
		"a volume the collector could not write to": {
			values: []string{"soak.enabled=true", "soak.accessModes[0]=ReadOnlyMany"},
			expect: "is not an access mode this collector can use",
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, stderr, err := render(t, tc.values...)
			if err == nil {
				t.Fatalf("rendered successfully; it must refuse instead")
			}
			if !strings.Contains(stderr, tc.expect) {
				t.Errorf("the refusal does not say %q:\n%s", tc.expect, stderr)
			}
		})
	}
}

// TestSoakKubeletStats: off by default because nodes/proxy is access to the kubelet API, and on
// most clusters that transitively includes exec on nodes. The flag and the grant move together —
// the standalone manifest could not enforce that, and uncommenting one without the other bought a
// fortnight of NOT MEASURED.
func TestSoakKubeletStats(t *testing.T) {
	off := mustRender(t, soakEnabled...)
	if hasArg(soakDeployment(t, off).Spec.Template.Spec.Containers[0].Args, "--kubelet-stats") {
		t.Error("--kubelet-stats is on by default; it grants access to the kubelet API")
	}
	if lookup(off, "ClusterRoleBinding", "crystal-backup-soak-kubelet-stats") != nil {
		t.Error("the nodes/proxy ClusterRole is bound by default")
	}
	// Written down and left unbound, so an administrator who wants the cache figure can read
	// exactly what they would be granting.
	if lookup(off, "ClusterRole", "crystal-backup-soak-kubelet-stats") == nil {
		t.Error("the nodes/proxy ClusterRole is not rendered at all; it should be there, unbound")
	}

	on := mustRender(t, append(append([]string{}, soakEnabled...), "soak.kubeletStats=true")...)
	if !hasArg(soakDeployment(t, on).Spec.Template.Spec.Containers[0].Args, "--kubelet-stats") {
		t.Error("soak.kubeletStats=true did not reach the pod")
	}
	if lookup(on, "ClusterRoleBinding", "crystal-backup-soak-kubelet-stats") == nil {
		t.Error("soak.kubeletStats=true gave the flag without the grant: a fortnight of NOT MEASURED")
	}
}

// TestSoakCanWatchMoversOrItMeasuresNothing pins the two grants the §5 measurement rests on.
//
// A mover Job is deleted by the Backup controller on the same reconcile pass that reads its
// result — measured on the crucible, its Jobs were visible for 9.6s to 23.7s against a 15s sample
// interval — so the exact peak RSS a mover writes into its own termination message is only ever
// readable from a watch event. Without these verbs the collector silently falls back to polling,
// which is precisely the configuration that reported sizing classes `data` and `manifests` as
// NOT_MEASURED through a campaign that ran dozens of backups.
//
// It degrades SILENTLY: the poll keeps working, the archive still arrives, and the numbers are
// just quietly incomplete. That is why this is a test and not a comment — a well-meaning RBAC
// tightening would otherwise cost a fortnight of somebody's cluster time before anyone noticed.
func TestSoakCanWatchMoversOrItMeasuresNothing(t *testing.T) {
	objs := mustRender(t, soakEnabled...)
	var cr rbacv1.ClusterRole
	convert(t, find(t, objs, "ClusterRole", "crystal-backup-soak-collector"), &cr)

	for _, want := range []struct{ group, resource string }{
		{"", "pods"},
		{"batch", "jobs"},
	} {
		granted := false
		for _, rule := range cr.Rules {
			if !slices.Contains(rule.APIGroups, want.group) ||
				!slices.Contains(rule.Resources, want.resource) {
				continue
			}
			if slices.Contains(rule.Verbs, "watch") {
				granted = true
			}
		}
		if !granted {
			t.Errorf("the collector cannot watch %q in apiGroup %q.\n"+
				"Mover Jobs live ~10-20s and are deleted the moment the controller reads their "+
				"result, so without this verb the whole §5 memory measurement degrades to whatever "+
				"a 15s poll happens to catch — silently, with a full-looking archive.",
				want.resource, want.group)
		}
	}
}

// TestSoakRBACIsReadOnly. The collector writes nothing to the cluster — not a Lease, not an Event,
// not a ConfigMap (hack/soak/SPEC.md §10) — and reads no Secret anywhere. This asserts the shape
// rather than a copy of the rule list, so a rule added later still has to be a read.
//
// `watch` counts as a read, and the collector holds it on pods and batch/jobs — see
// TestSoakCanWatchMoversOrItMeasuresNothing for why it must.
func TestSoakRBACIsReadOnly(t *testing.T) {
	objs := mustRender(t, append(append([]string{}, soakEnabled...), "soak.kubeletStats=true")...)
	readVerbs := map[string]bool{"get": true, "list": true, "watch": true}

	for _, name := range []string{"crystal-backup-soak-collector", "crystal-backup-soak-kubelet-stats"} {
		var cr rbacv1.ClusterRole
		convert(t, find(t, objs, "ClusterRole", name), &cr)
		for _, rule := range cr.Rules {
			for _, v := range rule.Verbs {
				if !readVerbs[v] {
					t.Errorf("%s grants %q on %v — the collector writes nothing to the cluster it measures",
						name, v, rule.Resources)
				}
			}
			for _, r := range rule.Resources {
				if r == "secrets" || strings.HasPrefix(r, "secrets/") {
					t.Errorf("%s grants access to Secrets; credentials are not redacted in the "+
						"archive, they are never read", name)
				}
				if r == "*" && len(rule.APIGroups) > 0 && rule.APIGroups[0] == "" {
					t.Errorf("%s wildcards the core API group", name)
				}
			}
		}
	}

	// The two bindings that exist point at the collector's own ServiceAccount and at nothing else,
	// so revoking the soak is deleting bindings rather than editing the operator's.
	for _, name := range []string{
		"crystal-backup-soak-collector",
		"crystal-backup-soak-metrics-reader",
		"crystal-backup-soak-kubelet-stats",
	} {
		var crb rbacv1.ClusterRoleBinding
		convert(t, find(t, objs, "ClusterRoleBinding", name), &crb)
		if len(crb.Subjects) != 1 {
			t.Fatalf("%s binds %d subjects, want exactly the collector's SA", name, len(crb.Subjects))
		}
		s := crb.Subjects[0]
		if s.Kind != "ServiceAccount" || s.Name != "crystal-backup-soak" || s.Namespace != operatorNamespace {
			t.Errorf("%s binds %s/%s in %s, want the collector's own ServiceAccount",
				name, s.Kind, s.Name, s.Namespace)
		}
	}
}

// TestSoakMetricsURLMatchesTheRenderedService — the second and third hand-copies. The URL is not
// spelled out anywhere; it is built from the same helpers that name the Service, so a
// fullnameOverride moves both or neither.
func TestSoakMetricsURLMatchesTheRenderedService(t *testing.T) {
	objs := mustRender(t, append(append([]string{}, soakEnabled...),
		"fullnameOverride=second-install", "metrics.port=9999")...)

	svc := find(t, objs, "Service", "second-install-metrics")
	want := fmt.Sprintf("--metrics-url=https://%s.%s.svc:9999/metrics", svc.GetName(), operatorNamespace)
	d := soakDeployment2(t, objs, "second-install-soak")
	if !hasArg(d.Spec.Template.Spec.Containers[0].Args, want) {
		t.Errorf("no %q in %v", want, d.Spec.Template.Spec.Containers[0].Args)
	}

	// The metrics-reader ClusterRole is release-scoped too, and the standalone manifest's answer
	// to that was a kubectl command in a comment.
	var crb rbacv1.ClusterRoleBinding
	convert(t, find(t, objs, "ClusterRoleBinding", "second-install-soak-metrics-reader"), &crb)
	if crb.RoleRef.Name != "second-install-metrics-reader" {
		t.Errorf("the metrics-reader binding points at %q, not the ClusterRole this release rendered",
			crb.RoleRef.Name)
	}
}

func soakDeployment2(t *testing.T, objs []*unstructured.Unstructured, name string) *appsv1.Deployment {
	t.Helper()
	var d appsv1.Deployment
	return convert(t, find(t, objs, "Deployment", name), &d)
}

// TestSoakPodIsNotSelectedByTheOperatorPolicies. The collector must not wear
// `control-plane: controller-manager` (it would become a metrics scrape target serving no
// metrics) nor the operator's full selector (it would be confined by a NetworkPolicy written for
// a different pod).
func TestSoakPodIsNotSelectedByTheOperatorPolicies(t *testing.T) {
	objs := mustRender(t, soakEnabled...)
	labels := soakDeployment(t, objs).Spec.Template.Labels
	if _, ok := labels["control-plane"]; ok {
		t.Error("the collector pod wears control-plane; the metrics Service would select it")
	}
	if labels["app.kubernetes.io/name"] != "" {
		t.Error("the collector pod wears app.kubernetes.io/name; the operator's NetworkPolicy " +
			"podSelector (name + instance) would select it")
	}
	if labels["crystalbackup.io/soak"] != "collector" || labels["app.kubernetes.io/instance"] != releaseName {
		t.Errorf("the collector pod's identity is not release-scoped: %v", labels)
	}
}

// TestSoakSurvivesBeingRedeployed pins the two pod-level properties that a fortnight's archive now
// depends on, both of which read like formatting and are not.
//
// The incident, on a customer cluster: an autosync reconciler replaced the collector's pod, the
// volume reported `Multi-Attach error … already exclusively attached to one node`, attached to the
// new node, and then `rbd: map failed: (22) Invalid argument` left the replacement in
// ContainerCreating for good. collect.sh answered `NOT COLLECTED: … no Running pod`, exit 3, on an
// archive that was intact and unreachable.
//
//   - `Recreate` was ALREADY there and did not prevent it. It is still required, because a
//     RollingUpdate on an RWO volume deadlocks instead of handing over, and it is asserted here so
//     that "make the rollout smoother" cannot quietly reintroduce that. It is not the defence.
//   - `terminationGracePeriodSeconds` is the defence, and it is the reason this test exists at all:
//     on SIGTERM the collector flushes its open window and writes the per-class high-water figures
//     to its own log (internal/soak/shutdown.go), the only channel a terminating pod has that is not
//     the volume it is about to release. Trimmed to a couple of seconds, that block becomes a
//     SIGKILL and the next failed handover costs the figures again — silently, because the pod that
//     would have reported them is already gone.
func TestSoakSurvivesBeingRedeployed(t *testing.T) {
	d := soakDeployment(t, mustRender(t, soakEnabled...))

	if got := d.Spec.Strategy.Type; got != appsv1.RecreateDeploymentStrategyType {
		t.Errorf("the collector's strategy is %q, want Recreate. On a ReadWriteOnce volume a "+
			"RollingUpdate starts the replacement before the outgoing pod has released the PVC, so "+
			"it sits Pending on a multi-attach error with the soak silently stopped.", got)
	}

	grace := d.Spec.Template.Spec.TerminationGracePeriodSeconds
	if grace == nil {
		t.Fatal("terminationGracePeriodSeconds is unset. It is what gives the collector time to " +
			"write its shutdown report — the per-class peak figures, which exist on the PVC and " +
			"nowhere else — before the kubelet SIGKILLs it.")
	}
	if *grace < 20 {
		t.Errorf("terminationGracePeriodSeconds is %ds. The shutdown report is the only off-volume "+
			"copy of the high-water table; a grace period trimmed this far turns an orderly SIGTERM "+
			"into a SIGKILL and the block is never written.", *grace)
	}
}

// TestSoakDefaultsToWhatAPlainClusterHas. The kit has to stay installable where it is most useful:
// a small, single-node-capable cluster whose default StorageClass provides ReadWriteOnce and nothing
// else, and which has no class to name. Both of this release's new values default to exactly that,
// so an existing install renders byte-identically.
func TestSoakDefaultsToWhatAPlainClusterHas(t *testing.T) {
	var pvc corev1.PersistentVolumeClaim
	convert(t, find(t, mustRender(t, soakEnabled...),
		"PersistentVolumeClaim", "crystal-backup-soak-data"), &pvc)

	want := []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}
	if !slices.Equal(pvc.Spec.AccessModes, want) {
		t.Errorf("the collector's PVC defaults to %v, want %v. Requiring ReadWriteMany would make "+
			"this kit uninstallable on the clusters it is most useful on.", pvc.Spec.AccessModes, want)
	}
	if pvc.Spec.StorageClassName != nil {
		t.Errorf("the PVC names storageClassName %q by default; the cluster default is the right "+
			"choice and naming one is wrong on every cluster but the one it was named on",
			*pvc.Spec.StorageClassName)
	}
}

// TestSoakAccessModeAndClassRemoveTheHandover: the escape hatch for the administrators who can take
// it. A ReadWriteMany volume has no exclusive attachment to move, so the pod replacement that lost
// the archive cannot happen — but no cluster's DEFAULT class provides RWX, which is why the two
// values have to reach the manifest TOGETHER. A mode with no class is a PVC that stays Pending
// forever, and that is worse than the window it was closing.
func TestSoakAccessModeAndClassRemoveTheHandover(t *testing.T) {
	objs := mustRender(t, append(append([]string{}, soakEnabled...),
		"soak.accessModes[0]=ReadWriteMany", "soak.storageClassName=cephfs-shared")...)

	var pvc corev1.PersistentVolumeClaim
	convert(t, find(t, objs, "PersistentVolumeClaim", "crystal-backup-soak-data"), &pvc)

	want := []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}
	if !slices.Equal(pvc.Spec.AccessModes, want) {
		t.Errorf("soak.accessModes did not reach the PVC: got %v, want %v", pvc.Spec.AccessModes, want)
	}
	if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName != "cephfs-shared" {
		t.Errorf("soak.storageClassName did not reach the PVC: got %v. Without it the RWX mode "+
			"above is a claim nothing can bind.", pvc.Spec.StorageClassName)
	}

	// The size still lands, so the new fields did not displace the old one.
	if got := pvc.Spec.Resources.Requests.Storage().String(); got != "7Gi" {
		t.Errorf("soak.storage stopped reaching the PVC: got %s, want 7Gi", got)
	}
}

func hasArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

func hasArgPrefix(args []string, prefix string) bool {
	for _, a := range args {
		if strings.HasPrefix(a, prefix) {
			return true
		}
	}
	return false
}
