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

package manifests

import (
	"encoding/json"
	"fmt"
	"path"
	"slices"
	"strings"
	"unicode"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

// Selection is the resolved `resources[]` of a Restore, in the form the MOVER receives it —
// the operator resolves the CRD's tri-state before serialising, so the mover never has to
// reason about which field was omitted.
//
// That tri-state is load-bearing and easy to lose. spec/02-api.md § Restore selection model:
// both `resources` and `volumes` omitted ⇒ the whole namespace; a present field, EVEN `[]`,
// restores only what it lists. `[]` and "omitted" therefore mean opposite things, and JSON
// `omitempty` erases exactly that difference — hence an explicit All rather than "nil Items
// means everything".
type Selection struct {
	// All restores every manifest in the snapshot. Set by the operator when the Restore
	// names neither resources nor volumes.
	All bool `json:"all,omitempty"`
	// Items are OR'd: a manifest is restored iff ANY item matches. With All false and no
	// items, nothing is restored — which is what `resources: []` asks for.
	Items []SelectionItem `json:"items,omitempty"`
}

// SelectionItem is one resources[] entry. Within an item the parts are AND'd: the label
// selector and include both select, exclude then removes (04-manifest-backup.md §5.4).
type SelectionItem struct {
	// Selector matches the manifest's own labels. Nil or empty matches everything.
	Selector *metav1.LabelSelector `json:"selector,omitempty"`
	// Include are globs over the stored path <group>/<Kind>[/<name>]. Empty matches every
	// kind, so an item may select by label alone.
	Include []string `json:"include,omitempty"`
	// Exclude is applied after Selector and Include, so an item reads "these, minus those".
	// The backup-time default exclusions (§2.2) already happened and cannot be re-included.
	Exclude []string `json:"exclude,omitempty"`
}

// EncodeSelection renders a Selection for the mover's environment. It is JSON rather than a
// flag because the argv after the shim's "--" separator belongs to restic verbatim.
func EncodeSelection(s Selection) (string, error) {
	b, err := json.Marshal(s)
	if err != nil {
		return "", fmt.Errorf("encode manifest selection: %w", err)
	}
	return string(b), nil
}

// DecodeSelection parses what EncodeSelection wrote. An EMPTY string decodes to All — the
// mover is only ever handed a selection by an operator that resolved the tri-state, so the
// absence of the variable means an older operator that had no concept of narrowing, and
// restoring the whole snapshot is what that operator meant.
func DecodeSelection(encoded string) (Selection, error) {
	if strings.TrimSpace(encoded) == "" {
		return Selection{All: true}, nil
	}
	var s Selection
	if err := json.Unmarshal([]byte(encoded), &s); err != nil {
		return Selection{}, fmt.Errorf("decode manifest selection: %w", err)
	}
	return s, nil
}

// CompiledSelection is a Selection with its globs and label selectors parsed once, so a
// namespace with thousands of manifests does not re-parse them per object.
type CompiledSelection struct {
	all   bool
	items []compiledItem
}

type compiledItem struct {
	labels  labels.Selector
	include []resourcePattern
	exclude []resourcePattern
}

// Compile validates and prepares a Selection. It rejects a malformed pattern rather than
// silently matching nothing: a typo in an include is the difference between "restored two
// objects" and "restored the namespace", and both look like success from the outside.
func (s Selection) Compile() (*CompiledSelection, error) {
	c := &CompiledSelection{all: s.All}
	for i, item := range s.Items {
		sel := labels.Everything()
		if item.Selector != nil {
			var err error
			if sel, err = metav1.LabelSelectorAsSelector(item.Selector); err != nil {
				return nil, fmt.Errorf("resources[%d].selector: %w", i, err)
			}
		}
		inc, err := compilePatterns(item.Include)
		if err != nil {
			return nil, fmt.Errorf("resources[%d].include: %w", i, err)
		}
		exc, err := compilePatterns(item.Exclude)
		if err != nil {
			return nil, fmt.Errorf("resources[%d].exclude: %w", i, err)
		}
		c.items = append(c.items, compiledItem{labels: sel, include: inc, exclude: exc})
	}
	return c, nil
}

// Matches reports whether one captured resource is selected. objectLabels are the labels on
// the stored manifest, which is what a label selector in a restore must match — the object as
// it was captured, not as it may exist in the target.
func (c *CompiledSelection) Matches(group, kind, name string, objectLabels map[string]string) bool {
	if c.all {
		return true
	}
	set := labels.Set(objectLabels)
	for i := range c.items {
		if c.items[i].matches(group, kind, name, set) {
			return true
		}
	}
	return false
}

func (it *compiledItem) matches(group, kind, name string, set labels.Set) bool {
	if !it.labels.Matches(set) {
		return false
	}
	// An item with no include selects every kind, so `selector:` alone is a valid item.
	if len(it.include) > 0 && !anyPattern(it.include, group, kind, name) {
		return false
	}
	return !anyPattern(it.exclude, group, kind, name)
}

func anyPattern(pats []resourcePattern, group, kind, name string) bool {
	for i := range pats {
		if pats[i].matches(group, kind, name) {
			return true
		}
	}
	return false
}

// resourcePattern is a parsed <group>/<Kind>[/<name>] glob. Each segment is matched with
// path.Match, which does not cross "/" — matching segment by segment keeps a glob from
// leaking across the group/kind boundary.
type resourcePattern struct {
	group string
	kind  string
	name  string
}

// coreGroupAlias is how the unnamed core group is written in a pattern, matching the
// directory the dump uses for it (StoragePath). The spec also allows eliding it entirely.
const coreGroupAlias = CoreGroupDir

// matchAll is the segment that matches anything, and the value a pattern's omitted trailing
// segments take.
const matchAll = "*"

func compilePatterns(raw []string) ([]resourcePattern, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	out := make([]resourcePattern, 0, len(raw))
	for _, r := range raw {
		p, err := parseResourcePattern(r)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// parseResourcePattern reads a <group>/<Kind>[/<name>] glob, where the core group may be
// written "core" or elided (04-manifest-backup.md §5.4).
//
// Eliding the group makes the two-segment form ambiguous on its face: "apps/Deployment" is
// group/Kind while "Secret/db-creds" is Kind/name. It is resolved by the FIRST segment's
// case, and that rule is total rather than a heuristic: a Kubernetes API group is a DNS
// subdomain and so is always lowercase, while a Kind is always PascalCase. So an
// uppercase-initial first segment can only be a Kind, and anything else can only be a group.
//
// Deciding on the SECOND segment instead would look equally reasonable and would break on
// "apps/*" — a spec example — by reading the "*" as a name.
func parseResourcePattern(raw string) (resourcePattern, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return resourcePattern{}, fmt.Errorf("empty pattern")
	}
	// A leading "/" is the elided core group written explicitly ("/Secret"); drop it so the
	// segment count below reflects the meaningful parts.
	trimmed = strings.TrimPrefix(trimmed, "/")

	segs := strings.Split(trimmed, "/")
	if len(segs) > 3 {
		return resourcePattern{}, fmt.Errorf(
			"pattern %q has %d segments; the form is <group>/<Kind>[/<name>]", raw, len(segs))
	}
	if slices.Contains(segs, "") {
		return resourcePattern{}, fmt.Errorf("pattern %q has an empty segment", raw)
	}

	p := resourcePattern{group: matchAll, kind: matchAll, name: matchAll}
	if startsUpper(segs[0]) {
		// Core group elided: <Kind>[/<name>].
		if len(segs) > 2 {
			return resourcePattern{}, fmt.Errorf(
				"pattern %q starts with the Kind %q, so it may carry at most a name after it", raw, segs[0])
		}
		p.group = ""
		p.kind = segs[0]
		if len(segs) == 2 {
			p.name = segs[1]
		}
	} else {
		p.group = normalizeGroup(segs[0])
		if len(segs) > 1 {
			p.kind = segs[1]
		}
		if len(segs) > 2 {
			p.name = segs[2]
		}
	}

	// Reject a bad glob here rather than at match time, where a returned error would have to
	// be either ignored (silently selecting nothing) or fatal on every object.
	for _, s := range []string{p.group, p.kind, p.name} {
		if _, err := path.Match(s, ""); err != nil {
			return resourcePattern{}, fmt.Errorf("pattern %q: bad glob %q: %w", raw, s, err)
		}
	}
	return p, nil
}

// normalizeGroup maps the "core" spelling onto the unnamed group the API server actually
// uses, so "core/Service/web" and "Service/web" select the same object.
func normalizeGroup(g string) string {
	if g == coreGroupAlias {
		return ""
	}
	return g
}

func startsUpper(s string) bool {
	for _, r := range s {
		return unicode.IsUpper(r)
	}
	return false
}

func (p resourcePattern) matches(group, kind, name string) bool {
	return segMatch(p.group, group) && segMatch(p.kind, kind) && segMatch(p.name, name)
}

// segMatch compares one segment. The empty pattern is the core group, which only an empty
// group matches; path.Match's error is impossible here because parseResourcePattern already
// rejected a bad glob.
func segMatch(pattern, value string) bool {
	if pattern == matchAll {
		return true
	}
	if pattern == "" {
		return value == ""
	}
	ok, _ := path.Match(pattern, value)
	return ok
}
