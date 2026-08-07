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
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/yaml"
)

// ---------------------------------------------------------------------------
// Where mover Jobs are allowed to run.
//
// This is a PLATFORM decision, not a tenant one, and that is why it lives in an operator flag
// and a Helm value rather than in a CR field. A namespace owner choosing which nodes the
// platform's backup pods land on is the wrong side of the line the rest of this project draws
// (spec/03 §1): the admin decides where the platform runs, the tenant decides what gets backed
// up. There is deliberately no per-Backup and no per-schedule override.
//
// What made it necessary. A mover mounts the tenant's volume, so it is subject to every
// constraint the CSI driver puts on the node that mounts it — and those constraints are not
// uniform across a cluster. The case that produced this field: a Rook-Ceph cluster where RBD
// clones (which is what a CSI snapshot restore is) can only be mapped by a kernel new enough to
// carry the clone-child feature bit. Eight nodes on 5.4 could not map them; one node on 5.15
// could. Nothing in this operator could express "run the movers over there", so the answer was
// "upgrade every kernel first", which is not an answer anybody can act on this afternoon.
//
// The same shape covers the ordinary cases too: a tainted node pool reserved for I/O-heavy work,
// a zone whose egress to the object store is cheap, a set of nodes with the bandwidth to spare.
// ---------------------------------------------------------------------------

// Placement is the operator-wide scheduling policy applied to EVERY mover Job — data, manifests,
// discovery, maintenance and external sync alike.
//
// Every mover, rather than only the ones that mount a volume, on purpose. "Backup pods run on
// the backup nodes" is a sentence an administrator can hold in their head and verify with one
// `kubectl get pods -o wide`; "backup pods run on the backup nodes except the maintenance ones"
// is a footnote they will meet for the first time while debugging. The finer-grained knob can be
// added later if somebody wants it; it cannot be taken back once the coarse one is ambiguous.
//
// The zero value is the whole product's history up to 0.6.3: no nodeSelector, no tolerations, no
// affinity, and a Job byte-identical to the one this builder produced before this field existed.
// That equality is asserted by TestUnconfiguredPlacementChangesNothing.
type Placement struct {
	// NodeSelector is a HARD requirement: a mover pod is only schedulable on a node carrying
	// every one of these labels. There is no soft form of this field — use Affinity's
	// preferredDuringSchedulingIgnoredDuringExecution when a preference is what you mean.
	//
	// The distinction matters more than it looks. A nodeSelector naming a label that only one
	// node in nine carries does not "prefer" that node: it serialises every backup in the
	// cluster through it, and turns that node's absence into a cluster with no backups at all.
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
	// Tolerations let movers run on tainted nodes — the usual reason being a node pool reserved
	// for something, of which backup I/O is now one.
	//
	// Unlike the other two, these are applied EVEN to a node-pinned Job (see JobRequest.NodeName).
	// Pinning bypasses the scheduler, and taints are a scheduler concern, so a pin already
	// defeats a NoSchedule taint; but a NoExecute taint is enforced by the taint manager against
	// running pods however they were placed, and would evict a restore mid-copy. A toleration is
	// the only one of the three that still does work once the node is chosen.
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`
	// Affinity is passed to the pod as written. Node affinity is the field this exists for, and
	// its preferred form is the graceful answer to a partially-capable cluster: movers land on
	// the nodes that can do the work when those nodes have room, and elsewhere rather than
	// nowhere when they do not.
	//
	// Pod affinity and anti-affinity are accepted and are not interfered with, but note that
	// BuildJob already sets a soft topology spread over kubernetes.io/hostname for fan-out
	// (JobRequest.SpreadOverLabels); a hand-written pod anti-affinity is likely to be arguing
	// with it rather than adding to it.
	Affinity *corev1.Affinity `json:"affinity,omitempty"`
}

// IsZero reports whether this placement asks for nothing — the default, and the state in which
// mover pods are scheduled exactly as they were before this field existed.
func (p Placement) IsZero() bool {
	return len(p.NodeSelector) == 0 && len(p.Tolerations) == 0 && p.Affinity == nil
}

// String renders the placement for the one-line startup log. It reports SHAPE, not content: the
// question an administrator asks of that line is "did my placement reach the operator at all",
// and a full YAML dump of an affinity tree in a log line answers it worse than a count does.
func (p Placement) String() string {
	if p.IsZero() {
		return "none (movers schedule anywhere)"
	}
	var parts []string
	if len(p.NodeSelector) > 0 {
		keys := make([]string, 0, len(p.NodeSelector))
		for k := range p.NodeSelector {
			keys = append(keys, k+"="+p.NodeSelector[k])
		}
		slices.Sort(keys) // a map iteration order in a log line is a diff nobody can read
		parts = append(parts, "nodeSelector["+strings.Join(keys, ",")+"]")
	}
	if n := len(p.Tolerations); n > 0 {
		parts = append(parts, fmt.Sprintf("tolerations[%d]", n))
	}
	if p.Affinity != nil {
		parts = append(parts, "affinity[set]")
	}
	return strings.Join(parts, " ")
}

// forJob narrows the placement for one Job and returns independent copies of everything it
// yields, so that the single Placement the operator holds can never be aliased — let alone
// mutated — through a Job object a client library has been handed.
//
// pinned is JobRequest.NodeName != "": the same-node restore path (adr/0016 §2), where an RWO
// volume attached on exactly one node must be mounted from that node and the pod therefore names
// it directly, bypassing the scheduler.
//
// A pinned pod drops the nodeSelector and the affinity, and this is the load-bearing line of the
// file. The kubelet re-checks BOTH on admission (PodMatchesNodeSelectorAndAffinityTerms in its
// general predicates) even for a pod it never scheduled, so leaving them on would not place the
// pod anywhere better — it would get the pod REJECTED by the kubelet with `Predicate NodeAffinity
// failed`, on the one operation that has no second choice of node. Dropping them does not make
// the restore succeed on a node that cannot mount the volume; it makes the failure be the real
// one (the CSI driver's) instead of a scheduling error about a pod that was never scheduled.
func (p Placement) forJob(pinned bool) (map[string]string, []corev1.Toleration, *corev1.Affinity) {
	tolerations := p.Tolerations
	if len(tolerations) > 0 {
		tolerations = make([]corev1.Toleration, len(p.Tolerations))
		for i := range p.Tolerations {
			p.Tolerations[i].DeepCopyInto(&tolerations[i])
		}
	} else {
		tolerations = nil
	}
	if pinned {
		return nil, tolerations, nil
	}
	return copyLabels(p.NodeSelector), tolerations, p.Affinity.DeepCopy()
}

// LoadPlacement parses the platform's mover placement file.
//
// It is strict for the reason LoadProfiles is strict, and the reason is sharper here: a sizing
// override that never reaches a pod produces a pod with the wrong limits, which is bad; a
// PLACEMENT that never reaches a pod produces a pod on a node that cannot mount the volume, which
// is a backup that does not exist. Both are read back identically in `helm get values`.
//
// So: an unknown field is an error (`nodeselector:` does not parse as nothing), a nodeSelector
// key that is not a valid label key is an error, and a toleration the API server would reject is
// an error HERE — at startup, in front of whoever ran `helm upgrade` — rather than at 3am on a
// Job creation nobody is watching.
//
// Empty input is not an error. It is the normal case: the chart always renders the ConfigMap, and
// an install that configures nothing renders an empty document.
func LoadPlacement(data []byte) (Placement, error) {
	var p Placement
	if err := yaml.UnmarshalStrict(data, &p); err != nil {
		return Placement{}, fmt.Errorf("parse mover placement: %w", err)
	}
	if err := p.validate(); err != nil {
		return Placement{}, err
	}
	return p.normalize(), nil
}

// LoadPlacementFile is LoadPlacement over a path, with the same two non-error cases as
// LoadProfilesFile: an empty path (nobody configured placement) and a MISSING file both yield the
// zero placement. A file that EXISTS and is broken is still fatal.
func LoadPlacementFile(path string) (Placement, error) {
	if path == "" {
		return Placement{}, nil
	}
	data, err := os.ReadFile(path) // #nosec G304 -- an operator flag, not user input
	if err != nil {
		if os.IsNotExist(err) {
			return Placement{}, nil
		}
		return Placement{}, fmt.Errorf("read mover placement %s: %w", path, err)
	}
	return LoadPlacement(data)
}

// normalize collapses "present but empty" to absent, so that a chart rendering
// `affinity: {}` — which is what `toYaml` does with an empty default in values.yaml — does not
// put an empty affinity object into every mover pod in the cluster.
//
// It is done here rather than in the template because it must also hold for the kustomize
// install, for a hand-written ConfigMap, and for anyone calling LoadPlacement directly. A
// normalisation that lives in one of several producers is a normalisation that does not hold.
func (p Placement) normalize() Placement {
	if len(p.NodeSelector) == 0 {
		p.NodeSelector = nil
	}
	if len(p.Tolerations) == 0 {
		p.Tolerations = nil
	}
	if p.Affinity != nil && p.Affinity.NodeAffinity == nil &&
		p.Affinity.PodAffinity == nil && p.Affinity.PodAntiAffinity == nil {
		p.Affinity = nil
	}
	return p
}

// validate checks what can be checked without an API server. The three fields are not equally
// checkable and this does not pretend otherwise:
//
//   - nodeSelector is fully checkable — it is a label map, and the label rules are in apimachinery.
//   - tolerations are fully checkable — the API server's rules for them are four lines.
//   - affinity is checked for NODE affinity only, which is the part an administrator writes by
//     hand and the part this feature exists for. Pod (anti-)affinity is passed through and
//     validated by the API server on Job creation; that is stated in Placement.Affinity's comment
//     rather than silently true.
func (p Placement) validate() error {
	for key, value := range p.NodeSelector {
		if errs := validation.IsQualifiedName(key); len(errs) > 0 {
			return fmt.Errorf("mover placement nodeSelector key %q: %s", key, strings.Join(errs, "; "))
		}
		if errs := validation.IsValidLabelValue(value); len(errs) > 0 {
			return fmt.Errorf("mover placement nodeSelector %q value %q: %s", key, value, strings.Join(errs, "; "))
		}
	}
	for i := range p.Tolerations {
		if err := validateToleration(p.Tolerations[i]); err != nil {
			return fmt.Errorf("mover placement tolerations[%d]: %w", i, err)
		}
	}
	if p.Affinity != nil && p.Affinity.NodeAffinity != nil {
		if err := validateNodeAffinity(p.Affinity.NodeAffinity); err != nil {
			return fmt.Errorf("mover placement affinity.nodeAffinity: %w", err)
		}
	}
	return nil
}

// tolerationEffects is the closed set the API server accepts, plus the empty string, which means
// "every effect" and is a legitimate and common thing to write.
var tolerationEffects = []corev1.TaintEffect{
	corev1.TaintEffectNoSchedule,
	corev1.TaintEffectPreferNoSchedule,
	corev1.TaintEffectNoExecute,
}

func validateToleration(t corev1.Toleration) error {
	// An empty Operator defaults to Equal, which is why this switch has to name it.
	switch t.Operator {
	case "", corev1.TolerationOpEqual:
		if t.Key == "" && t.Value != "" {
			return fmt.Errorf("an empty key with operator %q matches nothing; use operator Exists "+
				"to tolerate every taint", corev1.TolerationOpEqual)
		}
	case corev1.TolerationOpExists:
		if t.Value != "" {
			return fmt.Errorf("operator Exists takes no value, but value is %q — the API server "+
				"rejects this, so it would fail on the first mover Job rather than here", t.Value)
		}
	default:
		return fmt.Errorf("operator %q is not one of %q, %q (or empty, which means %q)",
			t.Operator, corev1.TolerationOpEqual, corev1.TolerationOpExists, corev1.TolerationOpEqual)
	}
	if t.Key != "" {
		if errs := validation.IsQualifiedName(t.Key); len(errs) > 0 {
			return fmt.Errorf("key %q: %s", t.Key, strings.Join(errs, "; "))
		}
	}
	if t.Effect != "" && !slices.Contains(tolerationEffects, t.Effect) {
		return fmt.Errorf("effect %q is not one of %v (or empty, which tolerates every effect)",
			t.Effect, tolerationEffects)
	}
	if t.TolerationSeconds != nil && t.Effect != corev1.TaintEffectNoExecute {
		return fmt.Errorf("tolerationSeconds only applies to effect %q, but effect is %q",
			corev1.TaintEffectNoExecute, t.Effect)
	}
	return nil
}

func validateNodeAffinity(na *corev1.NodeAffinity) error {
	if req := na.RequiredDuringSchedulingIgnoredDuringExecution; req != nil {
		if len(req.NodeSelectorTerms) == 0 {
			return fmt.Errorf("requiredDuringSchedulingIgnoredDuringExecution has no " +
				"nodeSelectorTerms; an empty term list matches NO node, so every mover would be " +
				"unschedulable")
		}
		for i := range req.NodeSelectorTerms {
			if err := validateNodeSelectorTerm(req.NodeSelectorTerms[i]); err != nil {
				return fmt.Errorf("requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[%d]: %w", i, err)
			}
		}
	}
	for i, pref := range na.PreferredDuringSchedulingIgnoredDuringExecution {
		if pref.Weight < 1 || pref.Weight > 100 {
			return fmt.Errorf("preferredDuringSchedulingIgnoredDuringExecution[%d]: weight %d is "+
				"outside 1..100", i, pref.Weight)
		}
		if err := validateNodeSelectorTerm(pref.Preference); err != nil {
			return fmt.Errorf("preferredDuringSchedulingIgnoredDuringExecution[%d].preference: %w", i, err)
		}
	}
	return nil
}

func validateNodeSelectorTerm(term corev1.NodeSelectorTerm) error {
	for i := range term.MatchExpressions {
		if err := validateNodeSelectorRequirement(term.MatchExpressions[i]); err != nil {
			return fmt.Errorf("matchExpressions[%d]: %w", i, err)
		}
	}
	for i := range term.MatchFields {
		if err := validateNodeSelectorRequirement(term.MatchFields[i]); err != nil {
			return fmt.Errorf("matchFields[%d]: %w", i, err)
		}
	}
	if len(term.MatchExpressions) == 0 && len(term.MatchFields) == 0 {
		return fmt.Errorf("has neither matchExpressions nor matchFields; an empty term matches no node")
	}
	return nil
}

func validateNodeSelectorRequirement(r corev1.NodeSelectorRequirement) error {
	switch r.Operator {
	case corev1.NodeSelectorOpIn, corev1.NodeSelectorOpNotIn:
		if len(r.Values) == 0 {
			return fmt.Errorf("operator %q requires at least one value", r.Operator)
		}
	case corev1.NodeSelectorOpExists, corev1.NodeSelectorOpDoesNotExist:
		if len(r.Values) > 0 {
			return fmt.Errorf("operator %q takes no values, but %d were given", r.Operator, len(r.Values))
		}
	case corev1.NodeSelectorOpGt, corev1.NodeSelectorOpLt:
		if len(r.Values) != 1 {
			return fmt.Errorf("operator %q takes exactly one value, but %d were given", r.Operator, len(r.Values))
		}
		if _, err := strconv.ParseInt(r.Values[0], 10, 64); err != nil {
			return fmt.Errorf("operator %q compares integers, but the value is %q", r.Operator, r.Values[0])
		}
	default:
		return fmt.Errorf("operator %q is not one of In, NotIn, Exists, DoesNotExist, Gt, Lt", r.Operator)
	}
	if errs := validation.IsQualifiedName(r.Key); len(errs) > 0 {
		return fmt.Errorf("key %q: %s", r.Key, strings.Join(errs, "; "))
	}
	return nil
}
