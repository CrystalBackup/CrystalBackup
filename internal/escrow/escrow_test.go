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

package escrow

import (
	"strings"
	"testing"
)

// TestObjectKey pins the escrow object key — part of the DR contract (02-api.md): a
// SIBLING of the repository prefix (never inside "<prefix>/<clusterID>/", so restic and
// the movers' repo-scoped credentials never see it), byte-stable across releases.
func TestObjectKey(t *testing.T) {
	cases := []struct {
		prefix, clusterID, want string
	}{
		{"prod", "prod-eu-1", "prod/prod-eu-1.crystal-meta/wrapped-dek.age"},
		{"", "prod-eu-1", "prod-eu-1.crystal-meta/wrapped-dek.age"},
		{"/prod/", "prod-eu-1", "prod/prod-eu-1.crystal-meta/wrapped-dek.age"},
	}
	for _, tc := range cases {
		if got := ObjectKey(tc.prefix, tc.clusterID); got != tc.want {
			t.Errorf("ObjectKey(%q, %q) = %q, want %q", tc.prefix, tc.clusterID, got, tc.want)
		}
	}
}

// TestObjectKeyOutsideRepoPrefix proves the sibling property structurally: the escrow key
// never falls under the repository's own "<prefix>/<clusterID>/" subtree, so the movers'
// repo-scoped credential prefix (I4) can never reach it and restic never lists it.
func TestObjectKeyOutsideRepoPrefix(t *testing.T) {
	for _, tc := range []struct{ prefix, clusterID string }{
		{"prod", "prod-eu-1"},
		{"", "c1"},
		{"a/b", "cluster"},
	} {
		key := ObjectKey(tc.prefix, tc.clusterID)
		repoSubtree := strings.TrimPrefix(tc.prefix+"/"+tc.clusterID+"/", "/")
		if strings.HasPrefix(key, repoSubtree) {
			t.Errorf("ObjectKey(%q, %q) = %q falls under the repository subtree %q", tc.prefix, tc.clusterID, key, repoSubtree)
		}
	}
}
