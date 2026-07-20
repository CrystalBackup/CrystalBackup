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

// Package sanitize transforms live Kubernetes objects into manifests that can be re-applied
// into a different namespace or a different cluster (R15, spec/adr/0007).
//
// The engine is a rules pipeline over unstructured maps, never typed structs: tenant custom
// resources (CNPG Clusters, cert-manager Certificates, whatever an operator installs) must
// flow through the generic rules without this package vendoring their types.
//
// It also owns the backup-time exclusion list (spec/04-manifest-backup.md §2.2), because
// "which objects are captured" and "what is stripped from them" are one versioned contract:
// a restore needs to know both to reason about what it received.
package sanitize

import (
	_ "embed"
	"fmt"
	"strings"

	"sigs.k8s.io/yaml"
)

// rulesYAML is the ruleset, embedded so the binary and its rules ship as one artefact and a
// mover can never run against a ruleset it was not built with.
//
//go:embed rules.yaml
var rulesYAML []byte

// OpKind is a field operation a rule performs.
type OpKind string

const (
	// OpRemove deletes the value at a dotted path, unless a keep protects it.
	OpRemove OpKind = "remove"
	// OpRemoveAnnotation deletes one metadata.annotations key.
	OpRemoveAnnotation OpKind = "removeAnnotation"
	// OpRemoveFinalizer deletes one metadata.finalizers entry.
	OpRemoveFinalizer OpKind = "removeFinalizer"
	// OpKeep protects a path from every remove in the pipeline, whatever the rule order.
	OpKeep OpKind = "keep"
	// OpKeepIfValue protects a path only when its current value equals Value (or, for a
	// list, when the list contains Value). It exists because some fields are a server
	// allocation for most values and user intent for one specific value — Service
	// clusterIP being the case that matters: an address is an allocation, "None" is a
	// declaration that the Service is headless.
	OpKeepIfValue OpKind = "keepIfValue"
	// OpPruneEmptyMap removes a map that the rules above emptied.
	OpPruneEmptyMap OpKind = "pruneEmptyMap"
	// OpRemoveProjectedTokenVolumes removes API-server-injected projected token volumes
	// matching a name glob, together with every volumeMount referencing them. Structural
	// rather than path-shaped, hence its own operation.
	OpRemoveProjectedTokenVolumes OpKind = "removeProjectedTokenVolumes"
)

// mutatingOps are the operations that can change an object. A rule built only from
// non-mutating operations (a bare keep) is a tripwire for future rules, not a transform, and
// the coverage gate judges it differently.
var mutatingOps = map[OpKind]bool{
	OpRemove:                      true,
	OpRemoveAnnotation:            true,
	OpRemoveFinalizer:             true,
	OpPruneEmptyMap:               true,
	OpRemoveProjectedTokenVolumes: true,
}

// Op is one field operation. Path is a dotted field path for most operations, an annotation
// key for OpRemoveAnnotation, a finalizer for OpRemoveFinalizer, and a name glob for
// OpRemoveProjectedTokenVolumes.
type Op struct {
	Op   OpKind `json:"op"`
	Path string `json:"path"`
	// Value is the comparison operand of OpKeepIfValue; unused by every other operation.
	Value string `json:"value,omitempty"`
}

// Match selects which objects a rule applies to.
type Match struct {
	// Group is the API group. Absent means any group; the empty string means the core group
	// specifically — a distinction that matters, so this is a pointer rather than a string.
	Group *string `json:"group,omitempty"`
	// Kind is the PascalCase kind, or "*" for any.
	Kind string `json:"kind"`
}

// Rule is one entry of the ordered pipeline.
type Rule struct {
	ID          string `json:"id"`
	Description string `json:"description"`
	Match       Match  `json:"match"`
	Ops         []Op   `json:"ops"`
}

// Generic reports whether the rule applies to every kind.
func (r Rule) Generic() bool { return r.Match.Kind == "*" }

// Mutating reports whether the rule can change an object at all.
func (r Rule) Mutating() bool {
	for _, op := range r.Ops {
		if mutatingOps[op.Op] {
			return true
		}
	}
	return false
}

// matches reports whether the rule applies to an object of this group and kind.
func (r Rule) matches(group, kind string) bool {
	if r.Match.Kind != "*" && r.Match.Kind != kind {
		return false
	}
	if r.Match.Group != nil && *r.Match.Group != group {
		return false
	}
	return true
}

// Ruleset is the versioned, ordered pipeline.
type Ruleset struct {
	// Version is recorded in the snapshot metadata so a restore knows which rules produced
	// its input (spec/adr/0007).
	Version string `json:"version"`
	Rules   []Rule `json:"rules"`
}

// LoadRuleset parses and validates the embedded ruleset. It is called once by New; a parse or
// validation failure is a build-time defect surfaced at startup, never at backup time.
func LoadRuleset() (Ruleset, error) {
	var rs Ruleset
	if err := yaml.Unmarshal(rulesYAML, &rs); err != nil {
		return Ruleset{}, fmt.Errorf("parse embedded ruleset: %w", err)
	}
	if err := rs.validate(); err != nil {
		return Ruleset{}, err
	}
	return rs, nil
}

func (rs Ruleset) validate() error {
	if strings.TrimSpace(rs.Version) == "" {
		return fmt.Errorf("ruleset: version is required (it is recorded in snapshot metadata)")
	}
	if len(rs.Rules) == 0 {
		return fmt.Errorf("ruleset: no rules")
	}
	seen := make(map[string]bool, len(rs.Rules))
	genericSeen := false
	for i, r := range rs.Rules {
		switch {
		case strings.TrimSpace(r.ID) == "":
			return fmt.Errorf("ruleset: rule %d has no id", i)
		case seen[r.ID]:
			return fmt.Errorf("ruleset: duplicate rule id %q", r.ID)
		case strings.TrimSpace(r.Description) == "":
			// A rule with no rationale is unreviewable: the corpus shows WHAT it does, the
			// description is the only place recording WHY.
			return fmt.Errorf("ruleset: rule %q has no description", r.ID)
		case r.Match.Kind == "":
			return fmt.Errorf("ruleset: rule %q has no match.kind (use \"*\" for any)", r.ID)
		case len(r.Ops) == 0:
			return fmt.Errorf("ruleset: rule %q has no ops", r.ID)
		}
		seen[r.ID] = true

		// Generic rules must precede per-kind rules: the pipeline's whole point is that a
		// per-kind rule refines what the generic pass did (spec/adr/0007).
		if r.Generic() {
			if genericSeen && !rs.Rules[i-1].Generic() {
				return fmt.Errorf("ruleset: generic rule %q appears after a per-kind rule; "+
					"generic rules must come first", r.ID)
			}
		}
		genericSeen = true

		for _, op := range r.Ops {
			if !mutatingOps[op.Op] && op.Op != OpKeep && op.Op != OpKeepIfValue {
				return fmt.Errorf("ruleset: rule %q has unknown op %q", r.ID, op.Op)
			}
			if strings.TrimSpace(op.Path) == "" {
				return fmt.Errorf("ruleset: rule %q has an op with no path", r.ID)
			}
			if op.Op == OpKeepIfValue && op.Value == "" {
				return fmt.Errorf("ruleset: rule %q has a keepIfValue with no value", r.ID)
			}
		}
	}
	return nil
}
