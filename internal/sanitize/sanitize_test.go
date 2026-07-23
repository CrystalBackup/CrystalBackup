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
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

// The corpus is the contract. Sanitization decides what a tenant gets back after a disaster,
// so its behaviour is pinned byte-for-byte rather than described: a diff in expected.yaml is
// the only honest way to review a change to what survives a restore (spec/adr/0007).
//
// Layout, per case directory under testdata/:
//
//	input.yaml     the live object, as read from the API server
//	expected.yaml  the sanitized result, byte-exact          (transform cases)
//	excluded       the id of the exclusion rule that fires   (exclusion cases)
//	context.yaml   optional ExcludeContext for the case
//
// Regenerate expected.yaml after an intended rule change with:  go test ./internal/sanitize -update

var update = false

func init() {
	// A plain flag would collide with `go test -update` on older toolchains in CI images;
	// reading the env var keeps the regeneration path explicit and scriptable.
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		update = true
	}
}

type corpusCase struct {
	name    string
	dir     string
	input   *unstructured.Unstructured
	ctx     ExcludeContext
	exclude string // non-empty ⇒ this case asserts exclusion
}

func loadCorpus(t *testing.T) []corpusCase {
	t.Helper()
	entries, err := os.ReadDir("testdata")
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}
	var cases []corpusCase
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join("testdata", e.Name())
		raw, err := os.ReadFile(filepath.Join(dir, "input.yaml"))
		if err != nil {
			t.Fatalf("%s: read input.yaml: %v", e.Name(), err)
		}
		obj := &unstructured.Unstructured{}
		if err := yaml.Unmarshal(raw, &obj.Object); err != nil {
			t.Fatalf("%s: parse input.yaml: %v", e.Name(), err)
		}

		c := corpusCase{name: e.Name(), dir: dir, input: obj}

		if b, err := os.ReadFile(filepath.Join(dir, "excluded")); err == nil {
			c.exclude = strings.TrimSpace(string(b))
		}
		if b, err := os.ReadFile(filepath.Join(dir, "context.yaml")); err == nil {
			var raw struct {
				ServicesWithSelector []string `json:"servicesWithSelector"`
			}
			if err := yaml.Unmarshal(b, &raw); err != nil {
				t.Fatalf("%s: parse context.yaml: %v", e.Name(), err)
			}
			c.ctx.ServicesWithSelector = map[string]bool{}
			for _, n := range raw.ServicesWithSelector {
				c.ctx.ServicesWithSelector[n] = true
			}
		}
		cases = append(cases, c)
	}
	if len(cases) == 0 {
		t.Fatal("corpus is empty")
	}
	return cases
}

func TestRulesetLoads(t *testing.T) {
	rs, err := LoadRuleset()
	if err != nil {
		t.Fatalf("LoadRuleset: %v", err)
	}
	if rs.Version == "" {
		t.Fatal("ruleset version is empty; it is recorded in snapshot metadata")
	}
}

func TestGoldenCorpus(t *testing.T) {
	s, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, c := range loadCorpus(t) {
		t.Run(c.name, func(t *testing.T) {
			excluded, ruleID := ShouldExclude(c.input, c.ctx)

			if c.exclude != "" {
				if !excluded {
					t.Fatalf("expected exclusion by %s, but the object was kept", c.exclude)
				}
				if ruleID != c.exclude {
					t.Fatalf("excluded by %s, expected %s", ruleID, c.exclude)
				}
				return
			}
			if excluded {
				t.Fatalf("object was excluded by %s, but this case expects it to be kept and sanitized", ruleID)
			}

			got, _, err := s.Sanitize(c.input)
			if err != nil {
				t.Fatalf("Sanitize: %v", err)
			}
			out, err := Marshal(got)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}

			golden := filepath.Join(c.dir, "expected.yaml")
			if update {
				if err := os.WriteFile(golden, out, 0o600); err != nil {
					t.Fatalf("write golden: %v", err)
				}
				return
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("read expected.yaml (run with UPDATE_GOLDEN=1 to create it): %v", err)
			}
			if string(out) != string(want) {
				t.Errorf("sanitized output differs from %s\n--- got ---\n%s\n--- want ---\n%s", golden, out, want)
			}
		})
	}
}

// A rule that is not idempotent means the stored manifest depends on how many times the
// pipeline happened to run over it — which would make the golden files a coincidence rather
// than a contract.
func TestIdempotence(t *testing.T) {
	s, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, c := range loadCorpus(t) {
		if c.exclude != "" {
			continue
		}
		t.Run(c.name, func(t *testing.T) {
			once, _, err := s.Sanitize(c.input)
			if err != nil {
				t.Fatalf("first pass: %v", err)
			}
			twice, res, err := s.Sanitize(once)
			if err != nil {
				t.Fatalf("second pass: %v", err)
			}
			a, err := Marshal(once)
			if err != nil {
				t.Fatalf("marshal first: %v", err)
			}
			b, err := Marshal(twice)
			if err != nil {
				t.Fatalf("marshal second: %v", err)
			}
			if string(a) != string(b) {
				t.Errorf("sanitize is not idempotent; second pass mutated via %v\n--- once ---\n%s\n--- twice ---\n%s",
					res.Mutated, a, b)
			}
		})
	}
}

// Byte-stability is a storage property: an unchanged object must serialise identically on
// every backup or restic's dedup stores a fresh copy each run, and the repository grows for
// no reason (R13).
func TestDeterministicSerialization(t *testing.T) {
	s, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, c := range loadCorpus(t) {
		if c.exclude != "" {
			continue
		}
		t.Run(c.name, func(t *testing.T) {
			var prev []byte
			for i := range 8 {
				got, _, err := s.Sanitize(c.input)
				if err != nil {
					t.Fatalf("pass %d: %v", i, err)
				}
				out, err := Marshal(got)
				if err != nil {
					t.Fatalf("pass %d marshal: %v", i, err)
				}
				if prev != nil && string(out) != string(prev) {
					t.Fatalf("serialization is not deterministic at pass %d", i)
				}
				prev = out
			}
		})
	}
}

// The DoD gate (spec/08-testing-and-dod.md, spec/adr/0007): a rule with no corpus case fails
// the build. An untested rule is an unreviewed claim about what a tenant gets back.
func TestRuleCoverage(t *testing.T) {
	s, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	matched := map[string]bool{}
	mutated := map[string]bool{}
	for _, c := range loadCorpus(t) {
		if c.exclude != "" {
			continue
		}
		_, res, err := s.Sanitize(c.input)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		for _, id := range res.Matched {
			matched[id] = true
		}
		for _, id := range res.Mutated {
			mutated[id] = true
		}
	}

	var missing, inert []string
	for _, r := range s.Ruleset().Rules {
		if !matched[r.ID] {
			missing = append(missing, r.ID)
			continue
		}
		// A rule that can change objects but never does across the whole corpus is either
		// dead or untested; both are review failures, and telling them apart is the point of
		// adding a case.
		if r.Mutating() && !mutated[r.ID] {
			inert = append(inert, r.ID)
		}
	}
	slices.Sort(missing)
	slices.Sort(inert)
	if len(missing) > 0 {
		t.Errorf("rules never matched by any corpus case: %v — add a case exercising them", missing)
	}
	if len(inert) > 0 {
		t.Errorf("rules matched but never changed anything: %v — the case does not exercise the rule", inert)
	}
}

// Same gate for the exclusion list: every E-rule must be demonstrated by a case.
func TestExclusionCoverage(t *testing.T) {
	fired := map[string]bool{}
	for _, c := range loadCorpus(t) {
		if excluded, id := ShouldExclude(c.input, c.ctx); excluded {
			fired[id] = true
		}
	}
	all := []string{
		ExcludeControllerOwnedPod, ExcludeDeploymentOwnedReplicaSet, ExcludeCronJobOwnedJob,
		ExcludeManagedEndpoints, ExcludeEvent, ExcludeLease, ExcludeServiceAccountToken,
		ExcludeCertManagerTransient, ExcludeVolumeSnapshot, ExcludeOwnRunRecord,
		ExcludeRootCAConfigMap,
	}
	var missing []string
	for _, id := range all {
		if !fired[id] {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		t.Errorf("exclusion rules never exercised: %v — add a corpus case for each", missing)
	}
}

// The input object belongs to the caller — in the mover it comes straight off a list response
// and may be shared. Mutating it in place would corrupt the caller's copy in a way that only
// shows up as a wrong manifest much later.
func TestSanitizeDoesNotMutateInput(t *testing.T) {
	s, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, c := range loadCorpus(t) {
		if c.exclude != "" {
			continue
		}
		t.Run(c.name, func(t *testing.T) {
			before, err := yaml.Marshal(c.input.Object)
			if err != nil {
				t.Fatalf("marshal before: %v", err)
			}
			if _, _, err := s.Sanitize(c.input); err != nil {
				t.Fatalf("Sanitize: %v", err)
			}
			after, err := yaml.Marshal(c.input.Object)
			if err != nil {
				t.Fatalf("marshal after: %v", err)
			}
			if string(before) != string(after) {
				t.Error("Sanitize mutated its input object")
			}
		})
	}
}

// A keep must survive a remove of any ancestor. Without this, adding a broad rule later would
// silently drop a field an earlier rule promised to preserve — the exact over-stripping
// failure adr/0007 lists as its first risk.
func TestKeepProtectsAgainstAncestorRemoval(t *testing.T) {
	protected := map[string]bool{"spec.storageClassName": true, "spec.ports[].nodePort": true}
	for _, tc := range []struct {
		path string
		want bool
	}{
		{"spec.storageClassName", true},
		{"spec", true},       // ancestor of a kept field
		{"spec.ports", true}, // ancestor via the [] form
		{"spec.volumeName", false},
		{"metadata.uid", false},
	} {
		if got := isProtected(tc.path, protected); got != tc.want {
			t.Errorf("isProtected(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

func TestRulesetValidationRejectsBadInput(t *testing.T) {
	for name, rs := range map[string]Ruleset{
		"no version": {Rules: []Rule{{ID: "X", Description: "d", Match: Match{Kind: "*"}, Ops: []Op{{Op: OpRemove, Path: "a"}}}}},
		"no rules":   {Version: "1"},
		"duplicate id": {Version: "1", Rules: []Rule{
			{ID: "X", Description: "d", Match: Match{Kind: "*"}, Ops: []Op{{Op: OpRemove, Path: "a"}}},
			{ID: "X", Description: "d", Match: Match{Kind: "*"}, Ops: []Op{{Op: OpRemove, Path: "b"}}},
		}},
		"no description": {Version: "1", Rules: []Rule{{ID: "X", Match: Match{Kind: "*"}, Ops: []Op{{Op: OpRemove, Path: "a"}}}}},
		"no kind":        {Version: "1", Rules: []Rule{{ID: "X", Description: "d", Ops: []Op{{Op: OpRemove, Path: "a"}}}}},
		"no ops":         {Version: "1", Rules: []Rule{{ID: "X", Description: "d", Match: Match{Kind: "*"}}}},
		"unknown op":     {Version: "1", Rules: []Rule{{ID: "X", Description: "d", Match: Match{Kind: "*"}, Ops: []Op{{Op: "nope", Path: "a"}}}}},
		"empty path":     {Version: "1", Rules: []Rule{{ID: "X", Description: "d", Match: Match{Kind: "*"}, Ops: []Op{{Op: OpRemove}}}}},
	} {
		if err := rs.validate(); err == nil {
			t.Errorf("%s: expected a validation error, got nil", name)
		}
	}
}
