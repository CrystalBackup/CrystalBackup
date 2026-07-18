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

package nsselector

import (
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
)

// ns is a terse constructor for a namespace object with a name and optional labels.
func ns(name string, labels map[string]string) corev1.Namespace {
	return corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
}

// cluster is the shared fixture: a representative mix of tenant, system and
// operator namespaces with labels, exercised by every positive form below.
func cluster() []corev1.Namespace {
	return []corev1.Namespace{
		ns("c-web", map[string]string{"crystalbackup.io/seed": "crucible", "tier": "front"}),
		ns("c-db", map[string]string{"crystalbackup.io/seed": "crucible", "tier": "data"}),
		ns("c-media", map[string]string{"crystalbackup.io/seed": "crucible"}),
		ns("c-legacy", map[string]string{"crystalbackup.io/seed": "crucible", "tier": "data"}),
		ns("kube-system", nil),
		ns("kube-public", nil),
		ns("crystal-backup-system", map[string]string{"tier": "front"}),
		ns("default", nil),
	}
}

func TestMatch(t *testing.T) {
	tests := []struct {
		name    string
		sel     cbv1.NamespaceSelector
		want    []string
		wantErr bool
	}{
		{
			name: "matchNames glob selects and sorts",
			sel:  cbv1.NamespaceSelector{MatchNames: []string{"c-*"}},
			want: []string{"c-db", "c-legacy", "c-media", "c-web"},
		},
		{
			name: "matchNames multiple globs union",
			sel:  cbv1.NamespaceSelector{MatchNames: []string{"c-web", "kube-*"}},
			want: []string{"c-web", "kube-public", "kube-system"},
		},
		{
			name: "matchNames matches whole name, not substring",
			sel:  cbv1.NamespaceSelector{MatchNames: []string{"db"}},
			want: []string{},
		},
		{
			name: "matchLabels selects by equality",
			sel:  cbv1.NamespaceSelector{MatchLabels: map[string]string{"crystalbackup.io/seed": "crucible"}},
			want: []string{"c-db", "c-legacy", "c-media", "c-web"},
		},
		{
			name: "matchLabels two keys AND together",
			sel: cbv1.NamespaceSelector{MatchLabels: map[string]string{
				"crystalbackup.io/seed": "crucible", "tier": "data",
			}},
			want: []string{"c-db", "c-legacy"},
		},
		{
			name: "matchExpressions In",
			sel: cbv1.NamespaceSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
				{Key: "tier", Operator: metav1.LabelSelectorOpIn, Values: []string{"data"}},
			}},
			want: []string{"c-db", "c-legacy"},
		},
		{
			name: "matchExpressions Exists",
			sel: cbv1.NamespaceSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
				{Key: "crystalbackup.io/seed", Operator: metav1.LabelSelectorOpExists},
			}},
			want: []string{"c-db", "c-legacy", "c-media", "c-web"},
		},
		{
			name: "matchExpressions NotIn excludes labelled, keeps unlabelled",
			sel: cbv1.NamespaceSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
				{Key: "tier", Operator: metav1.LabelSelectorOpNotIn, Values: []string{"front"}},
			}},
			// NotIn is satisfied by namespaces whose "tier" is absent or not "front".
			want: []string{"c-db", "c-legacy", "c-media", "default", "kube-public", "kube-system"},
		},
		{
			name: "regexp anchors as written",
			sel:  cbv1.NamespaceSelector{Regexp: "^c-.+$"},
			want: []string{"c-db", "c-legacy", "c-media", "c-web"},
		},
		{
			name: "regexp unanchored matches substring",
			sel:  cbv1.NamespaceSelector{Regexp: "kube"},
			want: []string{"kube-public", "kube-system"},
		},
		{
			name: "exclude is applied last",
			sel: cbv1.NamespaceSelector{
				MatchNames: []string{"c-*"},
				Exclude:    []string{"c-legacy"},
			},
			want: []string{"c-db", "c-media", "c-web"},
		},
		{
			name: "exclude glob removes a family",
			sel: cbv1.NamespaceSelector{
				Regexp:  ".+",
				Exclude: []string{"kube-*", "crystal-backup-system", "default"},
			},
			want: []string{"c-db", "c-legacy", "c-media", "c-web"},
		},
		{
			name: "no positive form is an error",
			sel:  cbv1.NamespaceSelector{Exclude: []string{"kube-*"}},
			// (exclude alone is not a positive form)
			wantErr: true,
		},
		{
			name: "two positive forms is an error",
			sel: cbv1.NamespaceSelector{
				MatchNames:  []string{"c-*"},
				MatchLabels: map[string]string{"tier": "data"},
			},
			wantErr: true,
		},
		{
			name:    "matchLabels and matchExpressions together are two forms (error)",
			sel:     cbv1.NamespaceSelector{MatchLabels: map[string]string{"tier": "data"}, MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "x", Operator: metav1.LabelSelectorOpExists}}},
			wantErr: true,
		},
		{
			name:    "malformed positive glob is an error",
			sel:     cbv1.NamespaceSelector{MatchNames: []string{"c-[a"}},
			wantErr: true,
		},
		{
			name:    "malformed exclude glob is an error",
			sel:     cbv1.NamespaceSelector{Regexp: ".+", Exclude: []string{"kube-[a"}},
			wantErr: true,
		},
		{
			name:    "malformed regexp is an error",
			sel:     cbv1.NamespaceSelector{Regexp: "c-(unterminated"},
			wantErr: true,
		},
		{
			name: "valid selector matching nothing returns empty, not error",
			sel:  cbv1.NamespaceSelector{MatchLabels: map[string]string{"nonexistent": "value"}},
			want: []string{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Match(cluster(), tc.sel)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Match() expected an error, got nil (result %v)", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Match() unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Match() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestMatchEmptyClusterStillValidatesShape proves the rule-8 shape check runs
// independently of the namespace set: an invalid selector errors even with no
// namespaces to scan, and a valid one over an empty cluster returns empty.
func TestMatchEmptyClusterStillValidatesShape(t *testing.T) {
	if _, err := Match(nil, cbv1.NamespaceSelector{}); err == nil {
		t.Fatal("empty selector over empty cluster must still error on the rule-8 shape")
	}
	got, err := Match(nil, cbv1.NamespaceSelector{MatchNames: []string{"c-*"}})
	if err != nil {
		t.Fatalf("valid selector over empty cluster errored: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("valid selector over empty cluster = %v, want empty", got)
	}
}
