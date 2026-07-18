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

// Package nsselector resolves a v1alpha1.NamespaceSelector against a set of
// namespaces into the concrete list of namespace names a cluster-plane backup
// run must fan out into. It is the pure, client-free core of the ClusterBackup
// (and, later, ClusterBackupSchedule/Discovery) fan-out: given the live
// namespace objects and a selector, it returns the selected names. Keeping it
// pure makes the four selection forms and the exclude precedence exhaustively
// unit-testable without an API server.
//
// The naming and contract is spec/02-api.md § "Namespace selection" and
// admission rule 8: EXACTLY ONE positive form — matchNames, matchLabels,
// matchExpressions, or regexp — must be set, and `exclude` is applied last.
package nsselector

import (
	"fmt"
	"path"
	"regexp"
	"slices"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
)

// Match resolves sel against the given namespace objects and returns the sorted,
// de-duplicated names of the namespaces it selects.
//
// Rule 8 (spec/02-api.md): exactly one positive form must be set, and `exclude`
// is applied last. In production a ValidatingAdmissionPolicy (adr/0010) enforces
// the "exactly one" shape in the API server; that policy exempts the operator's
// own ServiceAccount and is absent on clusters that install without the chart's
// admission layer, so Match re-checks the shape defensively and returns an
// error rather than guessing when zero or more than one positive form is set. A
// malformed selector must fail loudly — silently backing up the wrong (or every)
// namespace is the one outcome a DR fan-out cannot have. The caller surfaces the
// error as a status condition and does NOT fan out.
//
// The four positive forms are mutually exclusive by rule 8, so they are counted
// and matched independently — matchLabels and matchExpressions are DISTINCT forms
// here (unlike a raw metav1.LabelSelector, which combines them), matching the
// spec's enumeration and the VAP that enforces it.
func Match(namespaces []corev1.Namespace, sel cbv1.NamespaceSelector) ([]string, error) {
	match, err := positiveMatcher(sel)
	if err != nil {
		return nil, err
	}

	out := make([]string, 0, len(namespaces))
	for i := range namespaces {
		ns := &namespaces[i]
		ok, err := match(ns)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		// exclude is applied last: a name matched by the positive form but caught
		// by any exclude glob is dropped (spec/02-api.md).
		excluded, err := anyGlobMatches(sel.Exclude, ns.Name)
		if err != nil {
			return nil, fmt.Errorf("namespace selector exclude: %w", err)
		}
		if excluded {
			continue
		}
		out = append(out, ns.Name)
	}
	slices.Sort(out)
	return out, nil
}

// namespaceMatcher answers whether one namespace satisfies the positive form. It
// may return an error only for a structurally invalid pattern (a bad glob), which
// aborts the whole resolution.
type namespaceMatcher func(ns *corev1.Namespace) (bool, error)

// positiveMatcher validates the rule-8 shape (exactly one positive form) and
// returns the matcher for whichever single form is set.
func positiveMatcher(sel cbv1.NamespaceSelector) (namespaceMatcher, error) {
	var forms int
	if len(sel.MatchNames) > 0 {
		forms++
	}
	if len(sel.MatchLabels) > 0 {
		forms++
	}
	if len(sel.MatchExpressions) > 0 {
		forms++
	}
	if sel.Regexp != "" {
		forms++
	}
	if forms == 0 {
		return nil, fmt.Errorf("namespace selector sets no positive form " +
			"(rule 8: exactly one of matchNames/matchLabels/matchExpressions/regexp is required)")
	}
	if forms > 1 {
		return nil, fmt.Errorf("namespace selector sets %d positive forms "+
			"(rule 8: exactly one of matchNames/matchLabels/matchExpressions/regexp is allowed)", forms)
	}

	switch {
	case len(sel.MatchNames) > 0:
		return func(ns *corev1.Namespace) (bool, error) {
			return anyGlobMatches(sel.MatchNames, ns.Name)
		}, nil

	case len(sel.MatchLabels) > 0:
		selector, err := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{MatchLabels: sel.MatchLabels})
		if err != nil {
			return nil, fmt.Errorf("namespace selector matchLabels: %w", err)
		}
		return labelMatcher(selector), nil

	case len(sel.MatchExpressions) > 0:
		selector, err := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{MatchExpressions: sel.MatchExpressions})
		if err != nil {
			return nil, fmt.Errorf("namespace selector matchExpressions: %w", err)
		}
		return labelMatcher(selector), nil

	default: // sel.Regexp != ""
		re, err := regexp.Compile(sel.Regexp)
		if err != nil {
			return nil, fmt.Errorf("namespace selector regexp %q: %w", sel.Regexp, err)
		}
		return func(ns *corev1.Namespace) (bool, error) {
			return re.MatchString(ns.Name), nil
		}, nil
	}
}

// labelMatcher adapts a compiled labels.Selector to a namespaceMatcher over the
// namespace's own labels.
func labelMatcher(selector labels.Selector) namespaceMatcher {
	return func(ns *corev1.Namespace) (bool, error) {
		return selector.Matches(labels.Set(ns.Labels)), nil
	}
}

// anyGlobMatches reports whether name matches any of the shell-style glob
// patterns (via path.Match). Namespace names are DNS-1123 labels with no '/',
// so path.Match's '*' spans the whole name as intended. A structurally invalid
// pattern (e.g. an unterminated "[") is returned as an error, never treated as a
// non-match, so a typo cannot silently widen or narrow the selection.
func anyGlobMatches(patterns []string, name string) (bool, error) {
	for _, p := range patterns {
		ok, err := path.Match(p, name)
		if err != nil {
			return false, fmt.Errorf("invalid glob pattern %q: %w", p, err)
		}
		if ok {
			return true, nil
		}
	}
	return false, nil
}
