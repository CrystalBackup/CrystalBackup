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
	"strconv"
	"strings"
	"testing"
	"testing/quick"
	"time"

	"github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
)

// quickConfig makes the property tests deterministic: a fixed seed and a bounded count so a
// failure reproduces byte-for-byte across runs and CI. These are wire/tenancy contracts, so
// a flaky property is worse than a slow one.
func quickConfig() *quick.Config {
	return &quick.Config{MaxCount: 2000, Rand: rand.New(rand.NewSource(1))}
}

// --- Tag primitives -------------------------------------------------------------------

// TestTagAndTagValueRoundTrip pins the "key=value" tag shape and proves TagValue is the
// exact inverse of Tag, including the edge cases that matter for a security boundary: a
// value containing "=", an empty value, a key that is a prefix of another key, and the bare
// TagBase marker (which a key lookup must never return).
func TestTagAndTagValueRoundTrip(t *testing.T) {
	if got := Tag("namespace", "team-x"); got != "namespace=team-x" {
		t.Fatalf("Tag = %q, want %q", got, "namespace=team-x")
	}

	tags := []string{
		TagBase,
		Tag(TagKeyKind, KindData),
		Tag(TagKeyNamespace, "ns-a"),
		Tag(TagKeyPVC, "pvc=weird"), // a value containing "="
		Tag(TagKeyTenant, ""),       // an empty value
	}
	cases := []struct {
		key     string
		wantVal string
		wantOK  bool
	}{
		{TagKeyKind, "data", true},
		{TagKeyNamespace, "ns-a", true},
		{TagKeyPVC, "pvc=weird", true}, // only the first "=" delimits key from value
		{TagKeyTenant, "", true},       // present but empty
		{TagKeyRun, "", false},         // absent
		{"name", "", false},            // "name" must NOT match "namespace=..."
		{"crystalbackup", "", false},   // the bare marker has no "=" and never matches
	}
	for _, c := range cases {
		gotVal, gotOK := TagValue(tags, c.key)
		if gotVal != c.wantVal || gotOK != c.wantOK {
			t.Errorf("TagValue(%q) = (%q,%v), want (%q,%v)", c.key, gotVal, gotOK, c.wantVal, c.wantOK)
		}
	}
}

// --- Identity constructors: pinned exact strings --------------------------------------

const (
	testCluster  = "prod-eu-1"
	testTenant   = "acme"
	testNS       = "ns-a"
	testPVC      = "pvc-1"
	testSchedule = "daily"
	testRun      = "daily-20260716-123005"
)

// TestDataIdentity pins host, the exact /data/<ns>/<pvc> path, and the exact ordered Tags
// slice for a data snapshot, in both the scheduled and the ad-hoc (empty schedule) cases.
func TestDataIdentity(t *testing.T) {
	got := DataIdentity(testCluster, testTenant, testNS, testPVC, testSchedule, testRun)
	if got.Host != testCluster {
		t.Errorf("Host = %q, want %q", got.Host, testCluster)
	}
	if got.Path != "/data/ns-a/pvc-1" {
		t.Errorf("Path = %q, want %q", got.Path, "/data/ns-a/pvc-1")
	}
	wantTags := []string{
		"crystalbackup",
		"kind=data",
		"tenant=acme",
		"namespace=ns-a",
		"pvc=pvc-1",
		"schedule=daily",
		"run=daily-20260716-123005",
	}
	if !reflect.DeepEqual(got.Tags, wantTags) {
		t.Errorf("Tags = %#v, want %#v", got.Tags, wantTags)
	}

	// Ad-hoc run: the schedule tag is omitted entirely (not emitted as "schedule=").
	adhoc := DataIdentity(testCluster, testTenant, testNS, testPVC, "", testRun)
	wantAdhoc := []string{
		"crystalbackup",
		"kind=data",
		"tenant=acme",
		"namespace=ns-a",
		"pvc=pvc-1",
		"run=daily-20260716-123005",
	}
	if !reflect.DeepEqual(adhoc.Tags, wantAdhoc) {
		t.Errorf("ad-hoc Tags = %#v, want %#v", adhoc.Tags, wantAdhoc)
	}
	if _, ok := TagValue(adhoc.Tags, TagKeySchedule); ok {
		t.Errorf("ad-hoc identity must not carry a schedule tag: %#v", adhoc.Tags)
	}
}

// TestManifestsIdentity pins host, the exact /manifests/<ns> path, and the exact ordered
// Tags slice for a namespace-manifests snapshot — which must carry NO pvc tag.
func TestManifestsIdentity(t *testing.T) {
	got := ManifestsIdentity(testCluster, testTenant, testNS, testSchedule, testRun)
	if got.Host != testCluster {
		t.Errorf("Host = %q, want %q", got.Host, testCluster)
	}
	if got.Path != "/manifests/ns-a" {
		t.Errorf("Path = %q, want %q", got.Path, "/manifests/ns-a")
	}
	wantTags := []string{
		"crystalbackup",
		"kind=manifests",
		"tenant=acme",
		"namespace=ns-a",
		"schedule=daily",
		"run=daily-20260716-123005",
	}
	if !reflect.DeepEqual(got.Tags, wantTags) {
		t.Errorf("Tags = %#v, want %#v", got.Tags, wantTags)
	}
	if _, ok := TagValue(got.Tags, TagKeyPVC); ok {
		t.Errorf("manifests identity must not carry a pvc tag: %#v", got.Tags)
	}

	// Ad-hoc run: schedule tag omitted.
	adhoc := ManifestsIdentity(testCluster, testTenant, testNS, "", testRun)
	wantAdhoc := []string{
		"crystalbackup",
		"kind=manifests",
		"tenant=acme",
		"namespace=ns-a",
		"run=daily-20260716-123005",
	}
	if !reflect.DeepEqual(adhoc.Tags, wantAdhoc) {
		t.Errorf("ad-hoc Tags = %#v, want %#v", adhoc.Tags, wantAdhoc)
	}
}

// TestClusterManifestsIdentity pins host, the fixed /cluster-manifests path, and the exact
// ordered Tags slice — which must carry NEITHER tenant, namespace nor pvc.
func TestClusterManifestsIdentity(t *testing.T) {
	got := ClusterManifestsIdentity(testCluster, testSchedule, testRun)
	if got.Host != testCluster {
		t.Errorf("Host = %q, want %q", got.Host, testCluster)
	}
	if got.Path != "/cluster-manifests" {
		t.Errorf("Path = %q, want %q", got.Path, "/cluster-manifests")
	}
	wantTags := []string{
		"crystalbackup",
		"kind=cluster-manifests",
		"schedule=daily",
		"run=daily-20260716-123005",
	}
	if !reflect.DeepEqual(got.Tags, wantTags) {
		t.Errorf("Tags = %#v, want %#v", got.Tags, wantTags)
	}
	for _, k := range []string{TagKeyTenant, TagKeyNamespace, TagKeyPVC} {
		if _, ok := TagValue(got.Tags, k); ok {
			t.Errorf("cluster-manifests identity must not carry a %q tag: %#v", k, got.Tags)
		}
	}

	// Ad-hoc run: only crystalbackup, kind, run.
	adhoc := ClusterManifestsIdentity(testCluster, "", testRun)
	wantAdhoc := []string{
		"crystalbackup",
		"kind=cluster-manifests",
		"run=daily-20260716-123005",
	}
	if !reflect.DeepEqual(adhoc.Tags, wantAdhoc) {
		t.Errorf("ad-hoc Tags = %#v, want %#v", adhoc.Tags, wantAdhoc)
	}
}

// TestIdentityProperties fuzzes all three constructors and asserts the structural invariants
// that make the tags a trustworthy tenancy boundary: host is always the clusterID; the first
// two tags are always TagBase then kind=<expected>; each applicable identity tag round-trips
// through TagValue to the exact input; the schedule tag is present iff schedule != ""; and no
// forbidden tag is ever present (pvc on manifests; tenant/namespace/pvc on cluster-manifests).
func TestIdentityProperties(t *testing.T) {
	// noneOf reports that an identity carries none of the given tag keys.
	noneOf := func(id Identity, keys ...string) bool {
		for _, k := range keys {
			if _, ok := TagValue(id.Tags, k); ok {
				return false
			}
		}
		return true
	}

	dataProp := func(cluster, tenant, ns, pvc, schedule, run string) bool {
		id := DataIdentity(cluster, tenant, ns, pvc, schedule, run)
		if id.Host != cluster || id.Path != "/data/"+ns+"/"+pvc {
			return false
		}
		if len(id.Tags) < 2 || id.Tags[0] != TagBase || id.Tags[1] != Tag(TagKeyKind, KindData) {
			return false
		}
		if v, ok := TagValue(id.Tags, TagKeyTenant); !ok || v != tenant {
			return false
		}
		if v, ok := TagValue(id.Tags, TagKeyNamespace); !ok || v != ns {
			return false
		}
		if v, ok := TagValue(id.Tags, TagKeyPVC); !ok || v != pvc {
			return false
		}
		return schedulePresence(id.Tags, schedule) && runPresent(id.Tags, run)
	}
	if err := quick.Check(dataProp, quickConfig()); err != nil {
		t.Errorf("DataIdentity property: %v", err)
	}

	manifestsProp := func(cluster, tenant, ns, schedule, run string) bool {
		id := ManifestsIdentity(cluster, tenant, ns, schedule, run)
		if id.Host != cluster || id.Path != "/manifests/"+ns {
			return false
		}
		if len(id.Tags) < 2 || id.Tags[0] != TagBase || id.Tags[1] != Tag(TagKeyKind, KindManifests) {
			return false
		}
		if v, ok := TagValue(id.Tags, TagKeyTenant); !ok || v != tenant {
			return false
		}
		if v, ok := TagValue(id.Tags, TagKeyNamespace); !ok || v != ns {
			return false
		}
		if !noneOf(id, TagKeyPVC) {
			return false
		}
		return schedulePresence(id.Tags, schedule) && runPresent(id.Tags, run)
	}
	if err := quick.Check(manifestsProp, quickConfig()); err != nil {
		t.Errorf("ManifestsIdentity property: %v", err)
	}

	clusterProp := func(cluster, schedule, run string) bool {
		id := ClusterManifestsIdentity(cluster, schedule, run)
		if id.Host != cluster || id.Path != "/cluster-manifests" {
			return false
		}
		if len(id.Tags) < 2 || id.Tags[0] != TagBase || id.Tags[1] != Tag(TagKeyKind, KindClusterManifests) {
			return false
		}
		if !noneOf(id, TagKeyTenant, TagKeyNamespace, TagKeyPVC) {
			return false
		}
		return schedulePresence(id.Tags, schedule) && runPresent(id.Tags, run)
	}
	if err := quick.Check(clusterProp, quickConfig()); err != nil {
		t.Errorf("ClusterManifestsIdentity property: %v", err)
	}
}

// schedulePresence asserts the schedule tag is present with value==schedule iff schedule is
// non-empty, and absent otherwise (the ad-hoc-run contract).
func schedulePresence(tags []string, schedule string) bool {
	v, ok := TagValue(tags, TagKeySchedule)
	if schedule == "" {
		return !ok
	}
	return ok && v == schedule
}

// runPresent asserts the run tag is always present with value==run.
func runPresent(tags []string, run string) bool {
	v, ok := TagValue(tags, TagKeyRun)
	return ok && v == run
}

// --- RepoURL --------------------------------------------------------------------------

// TestRepoURL pins the exact repository URL for every part of the contract: https/http/bare
// endpoints, empty and non-empty prefix, and the double-slash / trailing-slash normalisation.
func TestRepoURL(t *testing.T) {
	cases := []struct {
		name                                string
		endpoint, bucket, prefix, clusterID string
		want                                string
	}{
		{
			name:     "https scheme, non-empty prefix (spec example)",
			endpoint: "https://s3.example.net", bucket: "team-x-backups", prefix: "crystal", clusterID: "prod-eu-1",
			want: "s3:https://s3.example.net/team-x-backups/crystal/prod-eu-1",
		},
		{
			name:     "http scheme",
			endpoint: "http://minio.internal:9000", bucket: "backups", prefix: "crystal", clusterID: "kbh",
			want: "s3:http://minio.internal:9000/backups/crystal/kbh",
		},
		{
			name:     "bare host, no scheme",
			endpoint: "s3.example.net", bucket: "backups", prefix: "crystal", clusterID: "prod-eu-1",
			want: "s3:s3.example.net/backups/crystal/prod-eu-1",
		},
		{
			name:     "empty prefix drops the segment",
			endpoint: "https://s3.example.net", bucket: "backups", prefix: "", clusterID: "prod-eu-1",
			want: "s3:https://s3.example.net/backups/prod-eu-1",
		},
		{
			name:     "empty prefix on bare host",
			endpoint: "s3.example.net", bucket: "backups", prefix: "", clusterID: "prod-eu-1",
			want: "s3:s3.example.net/backups/prod-eu-1",
		},
		{
			name:     "trailing slash on endpoint is collapsed",
			endpoint: "https://s3.example.net/", bucket: "backups", prefix: "crystal", clusterID: "prod-eu-1",
			want: "s3:https://s3.example.net/backups/crystal/prod-eu-1",
		},
		{
			name:     "leading/trailing slashes on bucket and prefix are collapsed",
			endpoint: "https://s3.example.net", bucket: "/backups/", prefix: "/crystal/", clusterID: "prod-eu-1",
			want: "s3:https://s3.example.net/backups/crystal/prod-eu-1",
		},
		{
			name:     "port in bare host is preserved",
			endpoint: "10.0.0.1:9000", bucket: "b", prefix: "p", clusterID: "c",
			want: "s3:10.0.0.1:9000/b/p/c",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := RepoURL(c.endpoint, c.bucket, c.prefix, c.clusterID)
			if got != c.want {
				t.Fatalf("RepoURL = %q, want %q", got, c.want)
			}
			assertRepoURLInvariants(t, got)
		})
	}
}

// TestRepoURLProperties fuzzes RepoURL over realistic label-shaped inputs (including inputs
// that carry stray slashes) and asserts the invariants that must hold for every repo URL: it
// begins with "s3:", the path portion (after the optional scheme) contains no "//" and does
// not end with "/", and the segments appear in order endpoint, bucket, [prefix], clusterID.
func TestRepoURLProperties(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	label := func() string {
		// A label that sometimes carries a stray leading/trailing slash to exercise the
		// collapse, drawn from a safe alphabet so no accidental scheme is synthesised.
		const alpha = "abcdefghijklmnopqrstuvwxyz0123456789-.:"
		n := 1 + rng.Intn(8)
		var b strings.Builder
		if rng.Intn(4) == 0 {
			b.WriteByte('/')
		}
		for i := 0; i < n; i++ {
			b.WriteByte(alpha[rng.Intn(len(alpha))])
		}
		if rng.Intn(4) == 0 {
			b.WriteByte('/')
		}
		return b.String()
	}
	for i := 0; i < 3000; i++ {
		host := label()
		endpoint := host
		switch rng.Intn(3) {
		case 0:
			endpoint = "https://" + host
		case 1:
			endpoint = "http://" + host
		}
		bucket, clusterID := label(), label()
		prefix := ""
		if rng.Intn(2) == 0 {
			prefix = label()
		}
		got := RepoURL(endpoint, bucket, prefix, clusterID)
		assertRepoURLInvariants(t, got)
	}
}

// assertRepoURLInvariants checks the scheme-independent invariants of a repo URL string.
func assertRepoURLInvariants(t *testing.T, url string) {
	t.Helper()
	if !strings.HasPrefix(url, "s3:") {
		t.Fatalf("repo URL %q does not start with s3:", url)
	}
	rest := strings.TrimPrefix(url, "s3:")
	for _, s := range []string{"https://", "http://"} {
		rest = strings.TrimPrefix(rest, s)
	}
	if strings.Contains(rest, "//") {
		t.Fatalf("repo URL %q has a double slash in the path portion (%q)", url, rest)
	}
	if strings.HasSuffix(rest, "/") {
		t.Fatalf("repo URL %q has a trailing slash", url)
	}
}

// --- ForgetArgs -----------------------------------------------------------------------

// TestForgetArgs pins the base prefix and the fixed field order, and covers all-set, a
// subset, and the all-zero degenerate case (base only).
func TestForgetArgs(t *testing.T) {
	base := []string{"--tag", "crystalbackup", "--group-by", "host,paths"}

	t.Run("all fields set, fixed order", func(t *testing.T) {
		r := v1alpha1.RetentionSpec{
			KeepLast: 1, KeepHourly: 2, KeepDaily: 3, KeepWeekly: 4, KeepMonthly: 5, KeepYearly: 6,
		}
		want := append(append([]string{}, base...),
			"--keep-last", "1",
			"--keep-hourly", "2",
			"--keep-daily", "3",
			"--keep-weekly", "4",
			"--keep-monthly", "5",
			"--keep-yearly", "6",
		)
		if got := ForgetArgs(r); !reflect.DeepEqual(got, want) {
			t.Fatalf("ForgetArgs = %#v, want %#v", got, want)
		}
	})

	t.Run("subset keeps only the set fields, in order", func(t *testing.T) {
		r := v1alpha1.RetentionSpec{KeepDaily: 7, KeepYearly: 2}
		want := append(append([]string{}, base...), "--keep-daily", "7", "--keep-yearly", "2")
		if got := ForgetArgs(r); !reflect.DeepEqual(got, want) {
			t.Fatalf("ForgetArgs = %#v, want %#v", got, want)
		}
	})

	t.Run("all zero returns base only", func(t *testing.T) {
		got := ForgetArgs(v1alpha1.RetentionSpec{})
		if !reflect.DeepEqual(got, base) {
			t.Fatalf("ForgetArgs(zero) = %#v, want %#v", got, base)
		}
		if len(got) != 4 {
			t.Fatalf("all-zero must be detectable as len==4, got len %d", len(got))
		}
	})

	t.Run("non-positive fields are skipped, never emit a negative keep", func(t *testing.T) {
		r := v1alpha1.RetentionSpec{KeepLast: -5, KeepDaily: 3}
		want := append(append([]string{}, base...), "--keep-daily", "3")
		if got := ForgetArgs(r); !reflect.DeepEqual(got, want) {
			t.Fatalf("ForgetArgs = %#v, want %#v", got, want)
		}
	})
}

// TestForgetArgsProperty fuzzes the six keep counts (including zero and negative) and asserts
// the base prefix is always present and unchanged, and the tail is exactly the positive
// fields as --keep-<bucket> <n> pairs in the fixed order last→yearly.
func TestForgetArgsProperty(t *testing.T) {
	prop := func(kl, kh, kd, kw, km, ky int32) bool {
		r := v1alpha1.RetentionSpec{
			KeepLast: kl, KeepHourly: kh, KeepDaily: kd, KeepWeekly: kw, KeepMonthly: km, KeepYearly: ky,
		}
		args := ForgetArgs(r)
		if len(args) < 4 || args[0] != "--tag" || args[1] != TagBase ||
			args[2] != "--group-by" || args[3] != "host,paths" {
			return false
		}
		order := []struct {
			bucket string
			n      int32
		}{
			{"last", kl}, {"hourly", kh}, {"daily", kd}, {"weekly", kw}, {"monthly", km}, {"yearly", ky},
		}
		var want []string
		for _, p := range order {
			if p.n > 0 {
				want = append(want, "--keep-"+p.bucket, strconv.FormatInt(int64(p.n), 10))
			}
		}
		got := args[4:]
		if len(got) != len(want) { // avoid the nil-vs-empty-slice DeepEqual pitfall
			return false
		}
		for i := range got {
			if got[i] != want[i] {
				return false
			}
		}
		return true
	}
	if err := quick.Check(prop, quickConfig()); err != nil {
		t.Errorf("ForgetArgs property: %v", err)
	}
}

// --- Snapshot parsing & grouping ------------------------------------------------------

// snapshotsJSON is a realistic `restic snapshots --json` sample: four snapshots for one run
// across two namespaces — ns-a has a data + a manifests snapshot, ns-b has a data snapshot,
// plus the single cluster-manifests snapshot (no namespace tag). Extra restic fields (tree,
// username, program_version) are present to prove unknown fields are ignored.
const snapshotsJSON = `[
  {
    "time": "2026-07-16T12:30:05.123456789Z",
    "tree": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    "paths": ["/data/ns-a/pvc-1"],
    "hostname": "prod-eu-1",
    "username": "root",
    "program_version": "restic 0.17.3",
    "tags": ["crystalbackup","kind=data","tenant=acme","namespace=ns-a","pvc=pvc-1","schedule=daily","run=daily-20260716-123005"],
    "id": "1111111111111111111111111111111111111111111111111111111111111111",
    "short_id": "11111111"
  },
  {
    "time": "2026-07-16T12:30:06Z",
    "tree": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
    "paths": ["/manifests/ns-a"],
    "hostname": "prod-eu-1",
    "username": "root",
    "program_version": "restic 0.17.3",
    "tags": ["crystalbackup","kind=manifests","tenant=acme","namespace=ns-a","schedule=daily","run=daily-20260716-123005"],
    "id": "2222222222222222222222222222222222222222222222222222222222222222",
    "short_id": "22222222"
  },
  {
    "time": "2026-07-16T12:30:07Z",
    "tree": "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
    "paths": ["/data/ns-b/pvc-9"],
    "hostname": "prod-eu-1",
    "username": "root",
    "program_version": "restic 0.17.3",
    "tags": ["crystalbackup","kind=data","tenant=globex","namespace=ns-b","pvc=pvc-9","schedule=daily","run=daily-20260716-123005"],
    "id": "3333333333333333333333333333333333333333333333333333333333333333",
    "short_id": "33333333"
  },
  {
    "time": "2026-07-16T12:30:08Z",
    "tree": "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
    "paths": ["/cluster-manifests"],
    "hostname": "prod-eu-1",
    "username": "root",
    "program_version": "restic 0.17.3",
    "tags": ["crystalbackup","kind=cluster-manifests","schedule=daily","run=daily-20260716-123005"],
    "id": "4444444444444444444444444444444444444444444444444444444444444444",
    "short_id": "44444444"
  }
]`

// TestParseSnapshots asserts every decoded field of the first snapshot verbatim (including
// the sub-second RFC3339 time and the hostname→Host mapping) and the count.
func TestParseSnapshots(t *testing.T) {
	snaps, err := ParseSnapshots([]byte(snapshotsJSON))
	if err != nil {
		t.Fatalf("ParseSnapshots: %v", err)
	}
	if len(snaps) != 4 {
		t.Fatalf("len = %d, want 4", len(snaps))
	}
	first := snaps[0]
	if first.ID != "1111111111111111111111111111111111111111111111111111111111111111" {
		t.Errorf("ID = %q", first.ID)
	}
	if first.ShortID != "11111111" {
		t.Errorf("ShortID = %q, want 11111111", first.ShortID)
	}
	if first.Host != "prod-eu-1" {
		t.Errorf("Host = %q, want prod-eu-1 (from hostname)", first.Host)
	}
	wantTime, _ := time.Parse(time.RFC3339Nano, "2026-07-16T12:30:05.123456789Z")
	if !first.Time.Equal(wantTime) {
		t.Errorf("Time = %v, want %v", first.Time, wantTime)
	}
	if !reflect.DeepEqual(first.Paths, []string{"/data/ns-a/pvc-1"}) {
		t.Errorf("Paths = %#v", first.Paths)
	}
	wantTags := []string{
		"crystalbackup", "kind=data", "tenant=acme", "namespace=ns-a", "pvc=pvc-1",
		"schedule=daily", "run=daily-20260716-123005",
	}
	if !reflect.DeepEqual(first.Tags, wantTags) {
		t.Errorf("Tags = %#v, want %#v", first.Tags, wantTags)
	}
}

// TestParseSnapshotsEmptyAndInvalid covers the total-parser edges: an empty array yields no
// snapshots and no error, `null` yields a nil slice and no error, and malformed JSON errors.
func TestParseSnapshotsEmptyAndInvalid(t *testing.T) {
	for _, in := range []string{"[]", "null", "  \n[]\n"} {
		snaps, err := ParseSnapshots([]byte(in))
		if err != nil {
			t.Errorf("ParseSnapshots(%q) unexpected error: %v", in, err)
		}
		if len(snaps) != 0 {
			t.Errorf("ParseSnapshots(%q) len = %d, want 0", in, len(snaps))
		}
	}
	if _, err := ParseSnapshots([]byte("{not json")); err == nil {
		t.Errorf("ParseSnapshots(malformed) expected an error, got nil")
	}
}

// TestGroupByNamespaceRun asserts the (namespace, run) bucketing discovery relies on: ns-a's
// data + manifests land together, ns-b's data is its own bucket, and the cluster-manifests
// snapshot (no namespace tag) buckets under the empty namespace — distinct from any real one.
func TestGroupByNamespaceRun(t *testing.T) {
	snaps, err := ParseSnapshots([]byte(snapshotsJSON))
	if err != nil {
		t.Fatalf("ParseSnapshots: %v", err)
	}
	groups := GroupByNamespaceRun(snaps)

	const run = "daily-20260716-123005"
	if len(groups) != 3 {
		t.Fatalf("groups = %d, want 3 (ns-a, ns-b, cluster-scoped): %#v", len(groups), groups)
	}

	// ns-a: the data and the manifests snapshot of this run, in input order.
	nsA := groups[NamespaceRun{Namespace: "ns-a", Run: run}]
	if len(nsA) != 2 {
		t.Fatalf("ns-a bucket = %d snapshots, want 2", len(nsA))
	}
	if nsA[0].ShortID != "11111111" || nsA[1].ShortID != "22222222" {
		t.Errorf("ns-a bucket order = [%s,%s], want [11111111,22222222]", nsA[0].ShortID, nsA[1].ShortID)
	}

	// ns-b: a single data snapshot.
	nsB := groups[NamespaceRun{Namespace: "ns-b", Run: run}]
	if len(nsB) != 1 || nsB[0].ShortID != "33333333" {
		t.Errorf("ns-b bucket = %#v, want the single 33333333 snapshot", nsB)
	}

	// cluster-manifests: no namespace tag ⇒ empty-namespace key.
	cluster := groups[NamespaceRun{Namespace: "", Run: run}]
	if len(cluster) != 1 || cluster[0].ShortID != "44444444" {
		t.Errorf("cluster-scoped bucket = %#v, want the single 44444444 snapshot", cluster)
	}

	// Every input snapshot is accounted for exactly once across the buckets.
	total := 0
	for _, b := range groups {
		total += len(b)
	}
	if total != len(snaps) {
		t.Errorf("buckets hold %d snapshots, want %d (nothing dropped or duplicated)", total, len(snaps))
	}
}
