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

// The DEFAULT render — what a first-time installer actually gets, which until 0.6.3 nothing in
// this repository had ever looked at.
//
// Four defects reached a user in one hour because test/crucible/deploy/deploy.sh configured around
// every one of them: it set `namespace.create=false` and `networkPolicy.apiServerPort=6443` before
// the first crucible run ever happened, so the campaign proved that a chart nobody installs works.
// Those overrides are gone. These tests are what stops them coming back as defaults.
//
// The pattern is soak_test.go's, inverted where it has to be: soak_test moves every value OFF its
// default and finds it in the output, because a knob that stopped being wired looks identical to a
// hardcoded one. Here the DEFAULT is the thing under test, so several of these assert on a render
// with no --set at all.
package chart

import (
	"slices"
	"strings"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// TestDefaultRenderHasNoNamespace is the one this file exists for.
//
// The documented order provisions the cluster KEK Secret into crystal-backup-system BEFORE the
// chart is installed — it must, because the KEK is generated and escrowed out of band and this
// chart never creates a Secret. So the namespace exists by the time helm runs, and a rendered
// Namespace object asks Helm to adopt it:
//
//	Namespace "crystal-backup-system" ... exists and cannot be imported into the current
//	release: invalid ownership metadata; label validation error: missing key
//	"app.kubernetes.io/managed-by"
//
// That is not a warning. It is the install dying on the first command, for everyone who read the
// documentation and followed it in order.
func TestDefaultRenderHasNoNamespace(t *testing.T) {
	for _, o := range mustRender(t) {
		if o.GetKind() == "Namespace" {
			t.Errorf("the default render carries a Namespace (%s). Every installer who provisioned "+
				"the cluster KEK first — which requirements.md tells them to do — gets an ownership "+
				"error instead of an operator.", o.GetName())
		}
	}
}

// TestNamespaceCreateStillWorks: off by default is not the same as gone. A greenfield cluster
// where one command should do everything is a real case, and the PSA labels have to travel with
// the object when it is the object that carries them.
func TestNamespaceCreateStillWorks(t *testing.T) {
	objs := mustRender(t, "namespace.create=true")
	ns := find(t, objs, "Namespace", operatorNamespace)
	labels := ns.GetLabels()
	for k, want := range map[string]string{
		"pod-security.kubernetes.io/enforce":         "baseline",
		"pod-security.kubernetes.io/enforce-version": "latest",
		"pod-security.kubernetes.io/audit":           "restricted",
		"pod-security.kubernetes.io/warn":            "restricted",
	} {
		if labels[k] != want {
			t.Errorf("the created Namespace has %s=%q, want %q", k, labels[k], want)
		}
	}
}

// TestPodSecurityPostureRefusals — the loud failure that pays for dropping the Namespace object.
//
// `enforce: baseline` is not decoration: movers run runAsUser 0 with DAC_OVERRIDE in this
// namespace to preserve file ownership on restore, and `restricted` denies them. Nothing fails at
// install time — the operator itself IS restricted-compliant — so the cost of getting this wrong
// is paid weeks later by a mover pod that never starts. Both refusals below fire at template time,
// in every mode including `helm template`, because they are about the value and not the cluster.
//
// The third guard, the one that reads the LIVE namespace with `lookup` and refuses an install onto
// one whose enforce level disagrees, cannot be exercised here: `helm template` has no cluster and
// lookup returns nothing. It is the guard that covers the case that actually bit somebody, and it
// runs on `helm install`/`helm upgrade` only. NOTES.txt carries the same command for the renders
// where it cannot run.
func TestPodSecurityPostureRefusals(t *testing.T) {
	const enforce = `namespace.podSecurityLabels.pod-security\.kubernetes\.io/enforce`
	for name, tc := range map[string]struct {
		values []string
		expect string
	}{
		"an enforce level the movers cannot run under": {
			values: []string{enforce + "=restricted"},
			expect: "which the data movers cannot run under",
		},
		"a posture that names no enforce level": {
			values: []string{enforce + "=null"},
			expect: "would inherit whatever Pod Security Admission level this cluster defaults to",
		},
		"restricted while the chart creates the namespace too": {
			values: []string{"namespace.create=true", enforce + "=restricted"},
			expect: "which the data movers cannot run under",
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

	// Emptying the map is the one documented way to switch the checking off, for a cluster whose
	// PSA posture is managed somewhere else. It must render, and it must render no labels.
	objs := mustRender(t, "namespace.create=true", "namespace.podSecurityLabels=null")
	for k := range find(t, objs, "Namespace", operatorNamespace).GetLabels() {
		if strings.HasPrefix(k, "pod-security.kubernetes.io/") {
			t.Errorf("namespace.podSecurityLabels was emptied and %s was stamped anyway", k)
		}
	}
}

// TestOperatorEgressReachesTheAPIServerOnEveryCluster.
//
// `apiServerPort: 443` was the default, and on any cluster whose API server Endpoints are on 6443
// — k3s, RKE2, kubeadm, most of the world — it made the operator unable to start:
//
//	Failed to start manager: failed to get server groups:
//	Get "https://10.43.0.1:443/api": dial tcp: i/o timeout
//
// kube-proxy DNATs the `kubernetes` Service to the endpoint port BEFORE the CNI evaluates egress,
// so a rule naming 443 never matches the packet that leaves the pod. The default is now a superset
// and this test is what keeps it one.
func TestOperatorEgressReachesTheAPIServerOnEveryCluster(t *testing.T) {
	objs := mustRender(t)
	for _, policy := range []string{"crystal-backup-operator", "crystal-backup-manifest-mover-apiserver"} {
		ports := egressPorts(t, objs, policy)
		for _, want := range []int32{443, 6443} {
			if !hasPort(ports, want) {
				t.Errorf("%s does not allow egress to %d by default (allows %v). On k3s/RKE2/kubeadm "+
					"the API server's Endpoints are on 6443 and kube-proxy rewrites the destination "+
					"port before the CNI sees it, so the operator cannot start at all.",
					policy, want, ports)
			}
		}
	}
}

// TestOperatorEgressHasNoDuplicatePort. The rendered policy listed `port: 443` twice — once from
// apiServerPort, once hardcoded for the object-storage probes — with nothing saying which entry
// served which destination. Kubernetes did not mind; a human auditing the policy could not tell
// whether one of them was a leftover.
func TestOperatorEgressHasNoDuplicatePort(t *testing.T) {
	var p networkingv1.NetworkPolicy
	convert(t, find(t, mustRender(t), "NetworkPolicy", "crystal-backup-operator"), &p)
	if n := len(p.Spec.Egress); n != 1 {
		t.Fatalf("the default operator egress has %d rules, want 1: with no apiServerCIDRs both "+
			"destinations are 0.0.0.0/0, so they are the same allowance and writing them twice is "+
			"what produced the duplicate in the first place", n)
	}
	seen := map[int32]bool{}
	for _, port := range p.Spec.Egress[0].Ports {
		v := port.Port.IntVal
		if seen[v] {
			t.Errorf("port %d appears twice in the same egress rule, with nothing to say which "+
				"destination each entry serves", v)
		}
		seen[v] = true
	}
}

// TestAPIServerCIDRsNarrowsTheOperatorToo. The value narrowed only the manifest-mover policy while
// the operator's stayed at 0.0.0.0/0 — an asymmetry the name gave no hint of, and one that made
// the value worth less than it looked. Object storage keeps its own unnarrowed rule: a bucket
// endpoint is not the API server.
func TestAPIServerCIDRsNarrowsTheOperatorToo(t *testing.T) {
	const cidr = "10.43.0.1/32"
	objs := mustRender(t, "networkPolicy.apiServerCIDRs[0]="+cidr)

	var p networkingv1.NetworkPolicy
	convert(t, find(t, objs, "NetworkPolicy", "crystal-backup-operator"), &p)
	if len(p.Spec.Egress) != 2 {
		t.Fatalf("apiServerCIDRs must split the operator egress into the API server and object "+
			"storage; got %d rules", len(p.Spec.Egress))
	}

	var apiRule, storageRule *networkingv1.NetworkPolicyEgressRule
	for i := range p.Spec.Egress {
		r := &p.Spec.Egress[i]
		if hasPort(rulePorts(r), 6443) {
			apiRule = r
		} else {
			storageRule = r
		}
	}
	if apiRule == nil || storageRule == nil {
		t.Fatalf("could not tell the two rules apart: %+v", p.Spec.Egress)
	}
	if len(apiRule.To) != 1 || apiRule.To[0].IPBlock == nil || apiRule.To[0].IPBlock.CIDR != cidr {
		t.Errorf("apiServerCIDRs did not reach the operator's API-server rule: %+v", apiRule.To)
	}
	if len(storageRule.To) != 1 || storageRule.To[0].IPBlock == nil ||
		storageRule.To[0].IPBlock.CIDR != "0.0.0.0/0" {
		t.Errorf("the object-storage rule was narrowed by a list of API server addresses: %+v",
			storageRule.To)
	}

	// And the manifest mover, which is what the value always narrowed.
	var mover networkingv1.NetworkPolicy
	convert(t, find(t, objs, "NetworkPolicy", "crystal-backup-manifest-mover-apiserver"), &mover)
	if got := mover.Spec.Egress[0].To[0].IPBlock.CIDR; got != cidr {
		t.Errorf("the manifest-mover policy points at %q, not the configured CIDR", got)
	}
}

// TestAPIServerPortScalarStillNarrows. The deprecated scalar is accepted and REPLACES the list, so
// an install that had narrowed it to one port keeps exactly the posture it asked for rather than
// silently widening on upgrade. `deploy.sh` set precisely this, which is how the bug stayed
// invisible; the compatibility is for everyone who copied it.
func TestAPIServerPortScalarStillNarrows(t *testing.T) {
	objs := mustRender(t, "networkPolicy.apiServerPort=6443")

	if ports := egressPorts(t, objs, "crystal-backup-manifest-mover-apiserver"); len(ports) != 1 ||
		!hasPort(ports, 6443) {
		t.Errorf("the scalar did not replace the list on the manifest-mover policy: %v", ports)
	}
	// The operator additionally reaches object storage on 443, so 443 comes back there — from the
	// storage destination, not from the API server one.
	ports := egressPorts(t, objs, "crystal-backup-operator")
	for _, want := range []int32{443, 6443} {
		if !hasPort(ports, want) {
			t.Errorf("the operator lost egress to %d: %v", want, ports)
		}
	}
}

// TestAPIServerPortsEmptyIsRefused: an empty list is not a narrow policy, it is an operator that
// cannot make its first discovery call. Refused at template time rather than diagnosed from a
// CrashLoopBackOff.
func TestAPIServerPortsEmptyIsRefused(t *testing.T) {
	_, stderr, err := render(t, "networkPolicy.apiServerPorts=null")
	if err == nil {
		t.Fatal("an empty networkPolicy.apiServerPorts rendered successfully")
	}
	if !strings.Contains(stderr, "could reach the API server") {
		t.Errorf("the refusal does not explain what breaks:\n%s", stderr)
	}
}

// TestSoakEgressFollowsTheAPIServerPorts. The collector's own policy carried the literal pair, so
// an administrator who narrowed the value for the operator left the collector wide open on a port
// they had decided against. One value, one posture.
func TestSoakEgressFollowsTheAPIServerPorts(t *testing.T) {
	objs := mustRender(t, append(append([]string{}, soakEnabled...),
		"networkPolicy.apiServerPort=6443")...)
	ports := egressPorts(t, objs, "crystal-backup-soak-egress")
	if hasPort(ports, 443) {
		t.Errorf("the soak egress still allows 443 after the API server port was narrowed to "+
			"6443: %v", ports)
	}
	if !hasPort(ports, 6443) {
		t.Errorf("the soak egress does not reach the API server on the configured port: %v", ports)
	}
}

func egressPorts(t *testing.T, objs []*unstructured.Unstructured, name string) []int32 {
	t.Helper()
	var p networkingv1.NetworkPolicy
	convert(t, find(t, objs, "NetworkPolicy", name), &p)
	out := make([]int32, 0, len(p.Spec.Egress))
	for i := range p.Spec.Egress {
		out = append(out, rulePorts(&p.Spec.Egress[i])...)
	}
	return out
}

// rulePorts collects one rule's numeric ports. The metrics port of the soak policy is in here too,
// which is harmless: every assertion above is about presence or absence of a specific port.
func rulePorts(r *networkingv1.NetworkPolicyEgressRule) []int32 {
	var out []int32
	for _, port := range r.Ports {
		if port.Port != nil {
			out = append(out, port.Port.IntVal)
		}
	}
	return out
}

func hasPort(ports []int32, want int32) bool {
	for _, p := range ports {
		if p == want {
			return true
		}
	}
	return false
}

// TestOperatorCanReadTheEventsItQuotes is production-blocking if it fails, and it failed silently
// once already — in the gap between config/rbac/role.yaml and this chart, which are NOT generated
// from one source.
//
// The per-phase deadline fails a stalled volume and copies the kubelet's own Warning event into
// status.volumes[].reason: `rbd: map failed: (22) Invalid argument` instead of a bare timeout. The
// incident that motivated it published that sentence 1069 times over 36 hours while
// `kubectl get backup` said nothing.
//
// Without `list` on core events the deadline still fires and still fails the volume — with no
// cause attached. That is worse than the bug it replaces, because it looks like a working feature
// and the operator has no way to tell it is degraded. The kubebuilder marker keeps
// config/rbac/role.yaml right; nothing kept the chart right, which is what this test is for.
func TestOperatorCanReadTheEventsItQuotes(t *testing.T) {
	objs := mustRender(t)
	var cr rbacv1.ClusterRole
	convert(t, find(t, objs, "ClusterRole", "crystal-backup-operator"), &cr)

	for _, verb := range []string{"get", "list", "watch"} {
		granted := false
		for _, rule := range cr.Rules {
			if !slices.Contains(rule.APIGroups, "") || !slices.Contains(rule.Resources, "events") {
				continue
			}
			if slices.Contains(rule.Verbs, verb) {
				granted = true
			}
		}
		if !granted {
			t.Errorf("the operator ClusterRole cannot %q core events.\n"+
				"A stalled volume would then be failed with a bare deadline reason and no cause, "+
				"even though the kubelet published the exact reason on the mover pod. Add %q to "+
				"the [\"\"] events rule in charts/crystal-backup/templates/rbac.yaml.", verb, verb)
		}
	}
}
