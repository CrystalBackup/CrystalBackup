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

package restic

import (
	"math/rand"
	"reflect"
	"strings"
	"testing"
)

// TestRestoreArgs pins the exact argv of both restore modes: the snapshot subtree address
// ("<id>:<path>"), --target, the shared --overwrite always, --delete ONLY for Recreate
// semantics (deleteExtras), one flag per include/exclude pattern in caller order, and the
// trailing --sparse / --retry-lock. These strings are the data half of the R6/R7 contract —
// a drifted flag silently changes what a restore writes or removes.
func TestRestoreArgs(t *testing.T) {
	cases := []struct {
		name               string
		deleteExtras       bool
		includes, excludes []string
		want               []string
	}{
		{
			name:         "overwrite mode, whole subtree",
			deleteExtras: false,
			want: []string{
				"restore", "ab12cd34:/data/team-x/uploads",
				"--target", "/crystal/target",
				"--overwrite", "always",
				"--sparse", "--retry-lock", "5m",
			},
		},
		{
			name:         "recreate mode adds --delete before the patterns",
			deleteExtras: true,
			includes:     []string{"images/2026/**"},
			excludes:     []string{"images/2026/tmp/**"},
			want: []string{
				"restore", "ab12cd34:/data/team-x/uploads",
				"--target", "/crystal/target",
				"--overwrite", "always",
				"--delete",
				"--include", "images/2026/**",
				"--exclude", "images/2026/tmp/**",
				"--sparse", "--retry-lock", "5m",
			},
		},
		{
			name:         "multiple includes keep caller order",
			deleteExtras: false,
			includes:     []string{"a/**", "b/**"},
			want: []string{
				"restore", "ab12cd34:/data/team-x/uploads",
				"--target", "/crystal/target",
				"--overwrite", "always",
				"--include", "a/**",
				"--include", "b/**",
				"--sparse", "--retry-lock", "5m",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RestoreArgs("ab12cd34", "/data/team-x/uploads", "/crystal/target", tc.deleteExtras, tc.includes, tc.excludes)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("RestoreArgs = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestSnapshotsFilterArgs pins the AND-combination contract of the mediated listing
// (adr/0016 §3): extra tags are comma-joined into ONE --tag value with the base marker —
// restic ANDs comma-joined tags, while repeated --tag flags would OR and so would WIDEN the
// filter. With no extra tags it must degenerate to exactly SnapshotsArgs, so discovery and
// a filtered listing can never diverge on the base form.
func TestSnapshotsFilterArgs(t *testing.T) {
	if got, want := SnapshotsFilterArgs(), SnapshotsArgs(); !reflect.DeepEqual(got, want) {
		t.Errorf("SnapshotsFilterArgs() = %v, want SnapshotsArgs() = %v", got, want)
	}

	got := SnapshotsFilterArgs(Tag(TagKeyNamespace, "team-x"), Tag(TagKeyRun, "daily-20260718-010000"))
	want := []string{"snapshots", "--json", "--tag", "crystalbackup,namespace=team-x,run=daily-20260718-010000"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("SnapshotsFilterArgs(namespace,run) = %v, want %v", got, want)
	}

	// Exactly one --tag flag: the AND form must never emit a second --tag (which would OR).
	count := 0
	for _, a := range got {
		if a == "--tag" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("SnapshotsFilterArgs emitted %d --tag flags, want exactly 1 (comma-AND)", count)
	}
}

// dnsName generates a random DNS-1123-label-shaped name (lowercase alphanumerics and
// interior hyphens) from a deterministic source — the actual input domain of every tag
// value (namespaces, PVCs, run names). Deterministic seeding keeps a failure reproducible,
// matching quickConfig's rationale.
func dnsName(r *rand.Rand) string {
	const inner = "abcdefghijklmnopqrstuvwxyz0123456789-"
	const edge = "abcdefghijklmnopqrstuvwxyz0123456789"
	n := 1 + r.Intn(24)
	b := make([]byte, n)
	for i := range b {
		b[i] = inner[r.Intn(len(inner))]
	}
	b[0] = edge[r.Intn(len(edge))]
	b[n-1] = edge[r.Intn(len(edge))]
	return string(b)
}

// TestSnapshotsFilterArgsProperty asserts, over generated DNS-1123 names, that the
// namespace filter value always round-trips intact inside the single --tag flag: the joined
// value contains namespace=<ns> as one comma-separated element, and never splits or absorbs
// a neighbour. Tag values are DNS names (no comma), which is exactly why comma-joining is
// safe; this property is the test's premise and its guard.
func TestSnapshotsFilterArgsProperty(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	for i := 0; i < 2000; i++ {
		ns, run := dnsName(r), dnsName(r)
		args := SnapshotsFilterArgs(Tag(TagKeyNamespace, ns), Tag(TagKeyRun, run))
		if len(args) != 4 || args[2] != "--tag" {
			t.Fatalf("unexpected shape: %v", args)
		}
		parts := strings.Split(args[3], ",")
		if len(parts) != 3 || parts[0] != TagBase || parts[1] != "namespace="+ns || parts[2] != "run="+run {
			t.Fatalf("joined tag value %q does not decompose to [crystalbackup namespace=%s run=%s]", args[3], ns, run)
		}
	}
}

// TestPVCMetaTagsRoundTrip pins the encode side (deterministic order, sorted "+"-joined
// modes, class omitted when empty, unknown modes skipped) and asserts ParsePVCMeta is its
// exact inverse for every well-formed input.
func TestPVCMetaTagsRoundTrip(t *testing.T) {
	cases := []struct {
		name     string
		capacity int64
		class    string
		modes    []string
		wantTags []string
		wantMeta PVCMeta
	}{
		{
			name:     "full meta, modes sorted by abbreviation",
			capacity: 10737418240,
			class:    "fast-rbd",
			modes:    []string{"ReadWriteMany", "ReadWriteOnce"},
			wantTags: []string{"pvcsize=10737418240", "pvcclass=fast-rbd", "pvcmodes=RWO+RWX"},
			wantMeta: PVCMeta{CapacityBytes: 10737418240, StorageClass: "fast-rbd", AccessModes: []string{"ReadWriteOnce", "ReadWriteMany"}},
		},
		{
			name:     "classless claim omits pvcclass entirely",
			capacity: 1073741824,
			modes:    []string{"ReadWriteOnce"},
			wantTags: []string{"pvcsize=1073741824", "pvcmodes=RWO"},
			wantMeta: PVCMeta{CapacityBytes: 1073741824, AccessModes: []string{"ReadWriteOnce"}},
		},
		{
			name:     "unknown mode names are skipped, never invented",
			capacity: 1,
			class:    "std",
			modes:    []string{"ReadWriteBogus"},
			wantTags: []string{"pvcsize=1", "pvcclass=std"},
			wantMeta: PVCMeta{CapacityBytes: 1, StorageClass: "std"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tags := PVCMetaTags(tc.capacity, tc.class, tc.modes)
			if !reflect.DeepEqual(tags, tc.wantTags) {
				t.Errorf("PVCMetaTags = %v, want %v", tags, tc.wantTags)
			}
			if got := ParsePVCMeta(tags); !reflect.DeepEqual(got, tc.wantMeta) {
				t.Errorf("ParsePVCMeta(%v) = %+v, want %+v", tags, got, tc.wantMeta)
			}
		})
	}
}

// TestParsePVCMetaBestEffort pins the degradation contract: absent tags leave zero values,
// a malformed or non-positive pvcsize yields 0 (the caller's fallback trigger), and unknown
// mode abbreviations are dropped — a corrupt tag may degrade sizing but must never error.
func TestParsePVCMetaBestEffort(t *testing.T) {
	if got := ParsePVCMeta([]string{TagBase, "kind=data", "namespace=x"}); !reflect.DeepEqual(got, PVCMeta{}) {
		t.Errorf("ParsePVCMeta(no meta tags) = %+v, want zero PVCMeta", got)
	}
	for _, bad := range []string{"pvcsize=abc", "pvcsize=-5", "pvcsize=0", "pvcsize="} {
		if got := ParsePVCMeta([]string{bad}); got.CapacityBytes != 0 {
			t.Errorf("ParsePVCMeta(%q).CapacityBytes = %d, want 0", bad, got.CapacityBytes)
		}
	}
	got := ParsePVCMeta([]string{"pvcmodes=RWO+NOPE+RWX"})
	want := []string{"ReadWriteOnce", "ReadWriteMany"}
	if !reflect.DeepEqual(got.AccessModes, want) {
		t.Errorf("ParsePVCMeta(pvcmodes with unknown abbrev).AccessModes = %v, want %v", got.AccessModes, want)
	}
}

// TestPVCMetaTagsNeverCollideWithIdentity asserts the meta tag KEYS stay disjoint from the
// identity tag keys — TagValue matches on "<key>=", so a meta key that were a prefix or
// duplicate of an identity key could shadow tenancy reads. This is the guard that keeps
// "informational, additive" true at the string level.
func TestPVCMetaTagsNeverCollideWithIdentity(t *testing.T) {
	identity := []string{TagKeyTenant, TagKeyNamespace, TagKeyPVC, TagKeyKind, TagKeySchedule, TagKeyRun}
	meta := []string{TagKeyPVCSize, TagKeyPVCClass, TagKeyPVCModes}
	for _, m := range meta {
		for _, id := range identity {
			if m == id {
				t.Errorf("meta tag key %q collides with identity key %q", m, id)
			}
		}
	}
	// A data identity plus meta tags must still resolve identity values unchanged.
	id := DataIdentity("prod-eu-1", "team-x", "c-team-x", "uploads", "daily", "daily-20260718-010000")
	tags := append(append([]string{}, id.Tags...), PVCMetaTags(1024, "std", []string{"ReadWriteOnce"})...)
	if v, ok := TagValue(tags, TagKeyNamespace); !ok || v != "c-team-x" {
		t.Errorf("namespace tag after appending meta tags = %q/%v, want c-team-x/true", v, ok)
	}
	if v, ok := TagValue(tags, TagKeyPVC); !ok || v != "uploads" {
		t.Errorf("pvc tag after appending meta tags = %q/%v, want uploads/true", v, ok)
	}
}
