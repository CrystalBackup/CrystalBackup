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

package v1alpha1

import "testing"

// TestPVCSelectorMatches pins the four behaviours of the selector rule: matchLabels is a conjunction,
// a non-empty include is a requirement, exclude wins over include, and a malformed glob is a no-match
// rather than an error.
//
// It lives HERE, next to the rule, and that placement is the point. Until 0.6.5 the predicate was
// unexported inside the Backup controller, so `selfcheck` — which has to tell an administrator which
// PVCs a schedule covers — carried a second copy of these fourteen lines and a test asserting the two
// agreed. Two implementations of "which PVCs does this schedule cover" is a product that can say one
// thing and do another; a test that they agree is a smoke alarm, not a fix. There is now one
// implementation, on the type whose API contract this is, and this table is the only place the four
// behaviours are stated.
func TestPVCSelectorMatches(t *testing.T) {
	const name = "postgres-data"
	labels := map[string]string{"tier": "db", "env": "prod"}

	cases := []struct {
		desc string
		sel  PVCSelector
		want bool
	}{
		{"an empty selector takes everything", PVCSelector{}, true},
		{"every matchLabels pair must be present",
			PVCSelector{MatchLabels: map[string]string{"tier": "db", "env": "prod"}}, true},
		{"one wrong matchLabels value rejects",
			PVCSelector{MatchLabels: map[string]string{"tier": "cache"}}, false},
		// Stated because it DIVERGES from Kubernetes label-selector semantics, where
		// `matchLabels: {absent: ""}` requires the label to be present with an empty value. Here the
		// rule compares labels[k] against the required value, and a missing key reads as "", so an
		// empty required value is satisfied by a PVC that does not carry the key at all. Pinned as
		// the behaviour that ships rather than silently corrected: changing it would change which
		// PVCs an existing schedule covers, which is not a thing to do as a side effect of moving a
		// function. Recorded in 90-roadmap.md's backlog.
		{"a matchLabels entry with an empty value is satisfied by a PVC lacking the key",
			PVCSelector{MatchLabels: map[string]string{"absent": ""}}, true},
		{"a matchLabels entry with a value still requires the key",
			PVCSelector{MatchLabels: map[string]string{"absent": "x"}}, false},
		{"include is a glob over the name",
			PVCSelector{Include: []string{"postgres-*"}}, true},
		{"a non-empty include that matches nothing rejects",
			PVCSelector{Include: []string{"mysql-*"}}, false},
		{"exclude wins over include",
			PVCSelector{Include: []string{"postgres-*"}, Exclude: []string{"*-data"}}, false},
		{"exclude alone rejects",
			PVCSelector{Exclude: []string{"postgres-*"}}, false},
		{"a malformed include glob is a no-match, never an error",
			PVCSelector{Include: []string{"[bad"}}, false},
		{"a malformed exclude glob cannot exclude, and cannot panic",
			PVCSelector{Exclude: []string{"[bad"}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			if got := tc.sel.Matches(name, labels); got != tc.want {
				t.Errorf("Matches(%q) = %v, want %v", name, got, tc.want)
			}
		})
	}
}

// TestPVCSelectorMatchesToleratesNilLabels covers the shape a real PVC often has — no labels at all —
// because the rule reads a map that may be nil and a nil map read is only safe by convention.
func TestPVCSelectorMatchesToleratesNilLabels(t *testing.T) {
	if !(PVCSelector{}).Matches("data", nil) {
		t.Error("an empty selector must take a PVC with no labels")
	}
	if (PVCSelector{MatchLabels: map[string]string{"tier": "db"}}).Matches("data", nil) {
		t.Error("a matchLabels requirement must reject a PVC with no labels")
	}
}
