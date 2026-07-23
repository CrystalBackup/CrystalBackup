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

package sanitize

import (
	"fmt"
	"path"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

// Sanitizer applies the embedded ruleset to live objects.
type Sanitizer struct {
	rs Ruleset
}

// New loads and validates the embedded ruleset.
func New() (*Sanitizer, error) {
	rs, err := LoadRuleset()
	if err != nil {
		return nil, err
	}
	return &Sanitizer{rs: rs}, nil
}

// RulesetVersion is recorded in the snapshot's index.json so a restore can tell which rules
// produced its input.
func (s *Sanitizer) RulesetVersion() string { return s.rs.Version }

// Ruleset exposes the loaded rules (the corpus test walks them for the coverage gate).
func (s *Sanitizer) Ruleset() Ruleset { return s.rs }

// Result reports what the pipeline did to one object, for the coverage gate and for
// debugging a surprising output.
type Result struct {
	// Matched are the rules whose match selected this object.
	Matched []string
	// Mutated are the rules that actually changed it.
	Mutated []string
}

// Sanitize returns a cleaned deep copy of obj. The input is never modified: callers hold live
// objects read from a watch cache, and mutating one would corrupt an informer's store.
func (s *Sanitizer) Sanitize(obj *unstructured.Unstructured) (*unstructured.Unstructured, Result, error) {
	if obj == nil {
		return nil, Result{}, fmt.Errorf("sanitize: nil object")
	}
	out := obj.DeepCopy()
	gvk := out.GroupVersionKind()
	if gvk.Kind == "" {
		return nil, Result{}, fmt.Errorf("sanitize: object has no kind")
	}

	applicable := make([]Rule, 0, len(s.rs.Rules))
	for _, r := range s.rs.Rules {
		if r.matches(gvk.Group, gvk.Kind) {
			applicable = append(applicable, r)
		}
	}

	// Collect every keep BEFORE applying anything. adr/0007 describes a later rule
	// "re-protecting" a field an earlier rule would drop; collecting up front makes that
	// independent of where the keep sits in the file, so reordering rules cannot silently
	// change what survives.
	protected := make(map[string]bool)
	for _, r := range applicable {
		for _, op := range r.Ops {
			switch op.Op {
			case OpKeep:
				protected[op.Path] = true
			case OpKeepIfValue:
				if valueMatches(out.Object, op.Path, op.Value) {
					protected[op.Path] = true
				}
			}
		}
	}

	res := Result{}
	for _, r := range applicable {
		res.Matched = append(res.Matched, r.ID)
		changed := false
		for _, op := range r.Ops {
			c, err := s.applyOp(out.Object, op, protected)
			if err != nil {
				return nil, Result{}, fmt.Errorf("sanitize %s/%s rule %s: %w", gvk.Kind, out.GetName(), r.ID, err)
			}
			changed = changed || c
		}
		if changed {
			res.Mutated = append(res.Mutated, r.ID)
		}
	}
	return out, res, nil
}

func (s *Sanitizer) applyOp(obj map[string]any, op Op, protected map[string]bool) (bool, error) {
	switch op.Op {
	case OpRemove:
		if isProtected(op.Path, protected) {
			return false, nil
		}
		return removePath(obj, op.Path), nil
	case OpRemoveAnnotation:
		return removeMapKey(obj, "metadata.annotations", op.Path), nil
	case OpRemoveFinalizer:
		return removeFinalizer(obj, op.Path), nil
	case OpPruneEmptyMap:
		return pruneEmptyMap(obj, op.Path), nil
	case OpRemoveProjectedTokenVolumes:
		return removeProjectedTokenVolumes(obj, op.Path), nil
	case OpKeep, OpKeepIfValue:
		return false, nil
	default:
		return false, fmt.Errorf("unknown op %q", op.Op)
	}
}

// valueMatches reports whether the value at a dotted path equals want — either directly, or
// as the sole meaningful entry of a list. The list form is needed because Kubernetes carries
// the same intent in both shapes: a headless Service has clusterIP: None AND
// clusterIPs: ["None"], and dropping either one changes what gets restored.
func valueMatches(obj map[string]any, dotted, want string) bool {
	parent, key, ok := walkParent(obj, dotted)
	if !ok {
		return false
	}
	switch v := parent[key].(type) {
	case string:
		return v == want
	case []any:
		for _, it := range v {
			if s, ok := it.(string); ok && s == want {
				return true
			}
		}
	}
	return false
}

// isProtected reports whether a removal at removePath would take a kept field with it —
// either the path itself is kept, or something kept lives underneath it. Without the second
// case a keep would be defeated by any rule removing an ancestor, which is exactly the
// silent-over-stripping failure the keep mechanism exists to prevent.
func isProtected(removePath string, protected map[string]bool) bool {
	if protected[removePath] {
		return true
	}
	for p := range protected {
		if strings.HasPrefix(p, removePath+".") || strings.HasPrefix(p, removePath+"[]") {
			return true
		}
	}
	return false
}

// walkParent resolves all but the last segment of a dotted path, returning the parent map and
// the final key. It never creates missing levels: sanitization only ever removes.
func walkParent(obj map[string]any, dotted string) (map[string]any, string, bool) {
	segs := strings.Split(dotted, ".")
	cur := obj
	for _, seg := range segs[:len(segs)-1] {
		next, ok := cur[seg].(map[string]any)
		if !ok {
			return nil, "", false
		}
		cur = next
	}
	return cur, segs[len(segs)-1], true
}

// removePath deletes the value at a dotted path. The `a[].b` form removes field b from every
// element of the array at a.
func removePath(obj map[string]any, dotted string) bool {
	if arrPath, after, found := strings.Cut(dotted, "[]"); found {
		rest := strings.TrimPrefix(after, ".")
		parent, key, ok := walkParent(obj, arrPath)
		if !ok {
			return false
		}
		items, ok := parent[key].([]any)
		if !ok {
			return false
		}
		changed := false
		for _, it := range items {
			m, ok := it.(map[string]any)
			if !ok {
				continue
			}
			if rest == "" {
				continue
			}
			if removePath(m, rest) {
				changed = true
			}
		}
		return changed
	}

	parent, key, ok := walkParent(obj, dotted)
	if !ok {
		return false
	}
	if _, present := parent[key]; !present {
		return false
	}
	delete(parent, key)
	return true
}

// removeMapKey deletes one key from the map at dotted (annotations, labels, ...).
func removeMapKey(obj map[string]any, dotted, key string) bool {
	parent, last, ok := walkParent(obj, dotted)
	if !ok {
		return false
	}
	m, ok := parent[last].(map[string]any)
	if !ok {
		return false
	}
	if _, present := m[key]; !present {
		return false
	}
	delete(m, key)
	return true
}

// finalizersPath is where finalizers live on every Kubernetes object. Hardcoded rather than
// parameterised: there is exactly one such list, and a configurable path would be false
// generality inviting a rule to point somewhere meaningless.
const finalizersPath = "metadata.finalizers"

// removeFinalizer deletes one entry from metadata.finalizers, dropping the list entirely once
// empty (an empty finalizers array is pure noise in a stored manifest, and it serialises
// differently from an absent one, which would defeat dedup).
func removeFinalizer(obj map[string]any, value string) bool {
	parent, last, ok := walkParent(obj, finalizersPath)
	if !ok {
		return false
	}
	items, ok := parent[last].([]any)
	if !ok {
		return false
	}
	kept := make([]any, 0, len(items))
	changed := false
	for _, it := range items {
		if s, ok := it.(string); ok && s == value {
			changed = true
			continue
		}
		kept = append(kept, it)
	}
	if !changed {
		return false
	}
	if len(kept) == 0 {
		delete(parent, last)
	} else {
		parent[last] = kept
	}
	return true
}

// pruneEmptyMap removes the map at dotted if it has no entries left.
func pruneEmptyMap(obj map[string]any, dotted string) bool {
	parent, last, ok := walkParent(obj, dotted)
	if !ok {
		return false
	}
	m, ok := parent[last].(map[string]any)
	if !ok || len(m) != 0 {
		return false
	}
	delete(parent, last)
	return true
}

// removeProjectedTokenVolumes drops API-server-injected projected token volumes whose name
// matches glob, and every volumeMount referencing them across all container lists.
//
// Only volumes with a `projected` source are touched: a user volume that happens to match the
// glob is theirs, and removing it would silently break their pod.
func removeProjectedTokenVolumes(obj map[string]any, glob string) bool {
	spec, ok := obj["spec"].(map[string]any)
	if !ok {
		return false
	}
	volumes, ok := spec["volumes"].([]any)
	if !ok {
		return false
	}

	dropped := make(map[string]bool)
	kept := make([]any, 0, len(volumes))
	for _, v := range volumes {
		vm, ok := v.(map[string]any)
		if !ok {
			kept = append(kept, v)
			continue
		}
		name, _ := vm["name"].(string)
		_, isProjected := vm["projected"]
		if matched, _ := path.Match(glob, name); matched && isProjected {
			dropped[name] = true
			continue
		}
		kept = append(kept, v)
	}
	if len(dropped) == 0 {
		return false
	}
	if len(kept) == 0 {
		delete(spec, "volumes")
	} else {
		spec["volumes"] = kept
	}

	for _, listKey := range []string{"containers", "initContainers", "ephemeralContainers"} {
		list, ok := spec[listKey].([]any)
		if !ok {
			continue
		}
		for _, c := range list {
			cm, ok := c.(map[string]any)
			if !ok {
				continue
			}
			mounts, ok := cm["volumeMounts"].([]any)
			if !ok {
				continue
			}
			keptMounts := make([]any, 0, len(mounts))
			for _, mnt := range mounts {
				mm, ok := mnt.(map[string]any)
				if !ok {
					keptMounts = append(keptMounts, mnt)
					continue
				}
				if n, _ := mm["name"].(string); dropped[n] {
					continue
				}
				keptMounts = append(keptMounts, mnt)
			}
			if len(keptMounts) == 0 {
				delete(cm, "volumeMounts")
			} else {
				cm["volumeMounts"] = keptMounts
			}
		}
	}
	return true
}

// Marshal serialises a sanitized object for storage.
//
// Determinism is a storage property, not a cosmetic one: sigs.k8s.io/yaml round-trips through
// JSON, so map keys come out lexicographically sorted and an unchanged object produces
// byte-identical output on every backup. Combined with the stripping of churn fields
// (resourceVersion, managedFields, ...), restic's content-defined dedup then stores a stable
// manifest exactly once instead of once per run (R13, spec/adr/0007).
func Marshal(obj *unstructured.Unstructured) ([]byte, error) {
	if obj == nil {
		return nil, fmt.Errorf("marshal: nil object")
	}
	b, err := yaml.Marshal(obj.Object)
	if err != nil {
		return nil, fmt.Errorf("marshal %s/%s: %w", obj.GetKind(), obj.GetName(), err)
	}
	return b, nil
}
