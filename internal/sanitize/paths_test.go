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
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

// The path helpers are where a sanitization bug is silent. Over-stripping restores a
// half-configured object; under-stripping fails the apply. The golden corpus covers what the
// current rules do; these cover what the helpers would do for a rule someone adds later.

func mustParse(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := yaml.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("parse: %v", err)
	}
	return m
}

func TestRemovePath(t *testing.T) {
	for name, tc := range map[string]struct {
		doc     string
		path    string
		want    bool
		checkFn func(*testing.T, map[string]any)
	}{
		"nested field": {
			doc:  "spec:\n  claimRef:\n    uid: abc\n    name: pvc\n",
			path: "spec.claimRef.uid",
			want: true,
			checkFn: func(t *testing.T, m map[string]any) {
				cr := m["spec"].(map[string]any)["claimRef"].(map[string]any)
				if _, ok := cr["uid"]; ok {
					t.Error("uid survived")
				}
				if _, ok := cr["name"]; !ok {
					t.Error("name was collateral damage")
				}
			},
		},
		"absent path is not a change": {
			doc:  "spec:\n  a: 1\n",
			path: "spec.missing",
			want: false,
		},
		"intermediate is not a map": {
			doc:  "spec: notamap\n",
			path: "spec.claimRef.uid",
			want: false,
		},
		"missing intermediate": {
			doc:  "metadata:\n  name: x\n",
			path: "spec.claimRef.uid",
			want: false,
		},
		"array element field": {
			doc:  "spec:\n  ports:\n  - name: a\n    nodePort: 30080\n  - name: b\n    nodePort: 30081\n",
			path: "spec.ports[].nodePort",
			want: true,
			checkFn: func(t *testing.T, m map[string]any) {
				ports := m["spec"].(map[string]any)["ports"].([]any)
				for i, p := range ports {
					pm := p.(map[string]any)
					if _, ok := pm["nodePort"]; ok {
						t.Errorf("port %d kept its nodePort", i)
					}
					if _, ok := pm["name"]; !ok {
						t.Errorf("port %d lost its name", i)
					}
				}
			},
		},
		"array form on a missing array": {
			doc:  "spec:\n  a: 1\n",
			path: "spec.ports[].nodePort",
			want: false,
		},
		"array form on a non-array": {
			doc:  "spec:\n  ports: notanarray\n",
			path: "spec.ports[].nodePort",
			want: false,
		},
		"array form with no trailing field removes nothing": {
			doc:  "spec:\n  ports:\n  - name: a\n",
			path: "spec.ports[]",
			want: false,
		},
		"array of non-maps is skipped": {
			doc:  "spec:\n  ports:\n  - 1\n  - 2\n",
			path: "spec.ports[].nodePort",
			want: false,
		},
	} {
		t.Run(name, func(t *testing.T) {
			m := mustParse(t, tc.doc)
			if got := removePath(m, tc.path); got != tc.want {
				t.Fatalf("removePath(%q) = %v, want %v", tc.path, got, tc.want)
			}
			if tc.checkFn != nil {
				tc.checkFn(t, m)
			}
		})
	}
}

func TestRemoveFinalizer(t *testing.T) {
	t.Run("drops the entry and keeps the rest", func(t *testing.T) {
		m := mustParse(t, "metadata:\n  finalizers:\n  - kubernetes.io/pvc-protection\n  - example.com/keepme\n")
		if !removeFinalizer(m, "kubernetes.io/pvc-protection") {
			t.Fatal("expected a change")
		}
		f := m["metadata"].(map[string]any)["finalizers"].([]any)
		if len(f) != 1 || f[0] != "example.com/keepme" {
			t.Errorf("unexpected finalizers: %v", f)
		}
	})
	t.Run("drops the whole key when it empties", func(t *testing.T) {
		m := mustParse(t, "metadata:\n  finalizers:\n  - kubernetes.io/pvc-protection\n")
		if !removeFinalizer(m, "kubernetes.io/pvc-protection") {
			t.Fatal("expected a change")
		}
		if _, ok := m["metadata"].(map[string]any)["finalizers"]; ok {
			t.Error("an emptied finalizers array should be removed entirely")
		}
	})
	t.Run("absent value is not a change", func(t *testing.T) {
		m := mustParse(t, "metadata:\n  finalizers:\n  - other\n")
		if removeFinalizer(m, "kubernetes.io/pvc-protection") {
			t.Error("expected no change")
		}
	})
	t.Run("missing and mistyped are not changes", func(t *testing.T) {
		if removeFinalizer(mustParse(t, "metadata: {}\n"), "x") {
			t.Error("missing key reported a change")
		}
		if removeFinalizer(mustParse(t, "metadata:\n  finalizers: notalist\n"), "x") {
			t.Error("non-list reported a change")
		}
	})
}

func TestPruneEmptyMapAndRemoveMapKey(t *testing.T) {
	t.Run("prunes only when empty", func(t *testing.T) {
		m := mustParse(t, "metadata:\n  annotations: {}\n  labels:\n    app: web\n")
		if !pruneEmptyMap(m, "metadata.annotations") {
			t.Error("empty annotations should be pruned")
		}
		if pruneEmptyMap(m, "metadata.labels") {
			t.Error("non-empty labels must survive")
		}
		if pruneEmptyMap(m, "metadata.missing") {
			t.Error("missing key reported a change")
		}
	})
	t.Run("removeMapKey", func(t *testing.T) {
		m := mustParse(t, "metadata:\n  annotations:\n    a: '1'\n    b: '2'\n")
		if !removeMapKey(m, "metadata.annotations", "a") {
			t.Error("expected a change")
		}
		if removeMapKey(m, "metadata.annotations", "nope") {
			t.Error("absent key reported a change")
		}
		if removeMapKey(m, "metadata.missing", "a") {
			t.Error("missing map reported a change")
		}
	})
}

func TestValueMatches(t *testing.T) {
	for name, tc := range map[string]struct {
		doc  string
		path string
		want bool
	}{
		"scalar match":    {"spec:\n  clusterIP: None\n", "spec.clusterIP", true},
		"scalar mismatch": {"spec:\n  clusterIP: 10.43.0.1\n", "spec.clusterIP", false},
		"list contains":   {"spec:\n  clusterIPs:\n  - None\n", "spec.clusterIPs", true},
		"list without":    {"spec:\n  clusterIPs:\n  - 10.43.0.1\n", "spec.clusterIPs", false},
		"list of non-str": {"spec:\n  clusterIPs:\n  - 1\n", "spec.clusterIPs", false},
		"absent":          {"spec: {}\n", "spec.clusterIP", false},
		"missing parent":  {"metadata: {}\n", "spec.clusterIP", false},
		"unexpected type": {"spec:\n  clusterIP: 5\n", "spec.clusterIP", false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := valueMatches(mustParse(t, tc.doc), tc.path, "None"); got != tc.want {
				t.Errorf("valueMatches = %v, want %v", got, tc.want)
			}
		})
	}
}

// A user volume that happens to match the injected-token glob is THEIRS. Removing it would
// silently break their pod, and the name pattern alone is not evidence of who created it —
// the projected source is.
func TestRemoveProjectedTokenVolumesSparesUserVolumes(t *testing.T) {
	m := mustParse(t, `
spec:
  volumes:
  - name: kube-api-access-mine
    configMap:
      name: my-config
  - name: kube-api-access-x7f2q
    projected:
      sources:
      - serviceAccountToken:
          path: token
  containers:
  - name: app
    volumeMounts:
    - name: kube-api-access-mine
      mountPath: /mine
    - name: kube-api-access-x7f2q
      mountPath: /var/run/secrets/kubernetes.io/serviceaccount
`)
	if !removeProjectedTokenVolumes(m, "kube-api-access-*") {
		t.Fatal("expected the injected volume to be removed")
	}
	vols := m["spec"].(map[string]any)["volumes"].([]any)
	if len(vols) != 1 || vols[0].(map[string]any)["name"] != "kube-api-access-mine" {
		t.Fatalf("the user's configMap volume should survive, got %v", vols)
	}
	mounts := m["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)["volumeMounts"].([]any)
	if len(mounts) != 1 || mounts[0].(map[string]any)["name"] != "kube-api-access-mine" {
		t.Fatalf("only the injected mount should go, got %v", mounts)
	}
}

func TestRemoveProjectedTokenVolumesEdgeCases(t *testing.T) {
	t.Run("no spec", func(t *testing.T) {
		if removeProjectedTokenVolumes(mustParse(t, "metadata: {}\n"), "kube-api-access-*") {
			t.Error("expected no change")
		}
	})
	t.Run("no volumes", func(t *testing.T) {
		if removeProjectedTokenVolumes(mustParse(t, "spec:\n  containers: []\n"), "kube-api-access-*") {
			t.Error("expected no change")
		}
	})
	t.Run("nothing matches", func(t *testing.T) {
		if removeProjectedTokenVolumes(mustParse(t, "spec:\n  volumes:\n  - name: data\n    emptyDir: {}\n"), "kube-api-access-*") {
			t.Error("expected no change")
		}
	})
	t.Run("volumes key disappears when all are dropped", func(t *testing.T) {
		m := mustParse(t, "spec:\n  volumes:\n  - name: kube-api-access-a\n    projected: {}\n")
		if !removeProjectedTokenVolumes(m, "kube-api-access-*") {
			t.Fatal("expected a change")
		}
		if _, ok := m["spec"].(map[string]any)["volumes"]; ok {
			t.Error("an emptied volumes list should be removed entirely")
		}
	})
}

func TestSanitizeRejectsUnusableInput(t *testing.T) {
	s, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, _, err := s.Sanitize(nil); err == nil {
		t.Error("expected an error for a nil object")
	}
	noKind := &unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{"name": "x"}}}
	if _, _, err := s.Sanitize(noKind); err == nil {
		t.Error("expected an error for an object with no kind")
	}
	if _, err := Marshal(nil); err == nil {
		t.Error("expected an error marshalling nil")
	}
}

func TestRulesetVersionIsExposed(t *testing.T) {
	s, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if s.RulesetVersion() == "" {
		t.Error("RulesetVersion is empty; it is recorded in the snapshot index so a restore " +
			"can tell which rules produced its input")
	}
}

func TestRuleMatching(t *testing.T) {
	core := ""
	apps := "apps"
	for name, tc := range map[string]struct {
		rule        Rule
		group, kind string
		want        bool
	}{
		"generic matches anything":     {Rule{Match: Match{Kind: "*"}}, "apps", "Deployment", true},
		"kind must match":              {Rule{Match: Match{Kind: "Service"}}, "", "ConfigMap", false},
		"nil group matches any group":  {Rule{Match: Match{Kind: "Deployment"}}, "apps", "Deployment", true},
		"empty group means core only":  {Rule{Match: Match{Group: &core, Kind: "Service"}}, "", "Service", true},
		"empty group rejects a group":  {Rule{Match: Match{Group: &core, Kind: "Service"}}, "apps", "Service", false},
		"named group must match":       {Rule{Match: Match{Group: &apps, Kind: "Deployment"}}, "apps", "Deployment", true},
		"named group rejects mismatch": {Rule{Match: Match{Group: &apps, Kind: "Deployment"}}, "", "Deployment", false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := tc.rule.matches(tc.group, tc.kind); got != tc.want {
				t.Errorf("matches(%q,%q) = %v, want %v", tc.group, tc.kind, got, tc.want)
			}
		})
	}
}

// Generic-after-per-kind ordering is rejected because the pipeline's meaning depends on it: a
// per-kind rule refines the generic pass, so a generic rule running later would undo that.
func TestRulesetRejectsGenericAfterPerKind(t *testing.T) {
	rs := Ruleset{Version: "1", Rules: []Rule{
		{ID: "K", Description: "d", Match: Match{Kind: "Service"}, Ops: []Op{{Op: OpRemove, Path: "a"}}},
		{ID: "G", Description: "d", Match: Match{Kind: "*"}, Ops: []Op{{Op: OpRemove, Path: "b"}}},
	}}
	if err := rs.validate(); err == nil {
		t.Error("expected generic-after-per-kind to be rejected")
	}
}

func TestRulesetRejectsKeepIfValueWithoutValue(t *testing.T) {
	rs := Ruleset{Version: "1", Rules: []Rule{
		{ID: "X", Description: "d", Match: Match{Kind: "*"}, Ops: []Op{{Op: OpKeepIfValue, Path: "a"}}},
	}}
	if err := rs.validate(); err == nil {
		t.Error("expected keepIfValue with no value to be rejected")
	}
}
