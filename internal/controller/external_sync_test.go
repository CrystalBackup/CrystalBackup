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

package controller

import (
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/mover"
	"github.com/CrystalBackup/CrystalBackup/internal/restic"
)

// TestCompareSyncKeysOnOriginal is the assertion Mirror's delete half rests on.
//
// A copy re-encrypts, so the destination snapshot is a different object with a different
// content-addressed ID. `original` is the ONLY thing tying it back to its source. Tags and
// timestamps are both preserved by the copy and both identical across two runs of the same
// schedule, so matching on either would confuse two runs — and for Mirror, confusing them means
// forgetting a snapshot that is still live at the source.
func TestCompareSyncKeysOnOriginal(t *testing.T) {
	source := syncInventory{IDs: []string{"s1", "s2", "s3"}}
	dest := syncInventory{
		IDs: []string{"d1", "d2", "d9"},
		Origins: map[string]string{
			"d1": "s1", // still at the source
			"d2": "s2", // still at the source
			"d9": "gone-from-source",
		},
	}

	got := compareSync(source, dest)
	if got.Copied != 2 {
		t.Errorf("Copied = %d, want 2", got.Copied)
	}
	if got.Lag != 1 {
		t.Errorf("Lag = %d, want 1 (s3 is not at the destination)", got.Lag)
	}
	if !slices.Equal(got.Extra, []string{"d9"}) {
		t.Errorf("Extra = %v, want [d9]", got.Extra)
	}
}

// TestCompareSyncNeverTouchesNativeSnapshots is what makes a repository that receives BOTH native
// backups and a sync safe to use as a destination.
//
// A snapshot with no `original` was backed up there directly. This sync did not put it there, so
// it is not this sync's to remove — and it is not this sync's to count either, or the lag would
// read as zero while the source was never copied at all. Getting this wrong would have Mirror
// deleting a tenant's own backups because a sync happened to point at their repository.
func TestCompareSyncNeverTouchesNativeSnapshots(t *testing.T) {
	source := syncInventory{IDs: []string{"s1"}}
	dest := syncInventory{
		IDs: []string{"native-1", "native-2"},
		// No Origins at all: everything here was backed up natively.
		Origins: map[string]string{},
	}

	got := compareSync(source, dest)
	if len(got.Extra) != 0 {
		t.Fatalf("Extra = %v; Mirror would forget snapshots this sync never created", got.Extra)
	}
	if got.Copied != 0 {
		t.Errorf("Copied = %d, want 0 — a native snapshot is not a copy of anything", got.Copied)
	}
	if got.Lag != 1 {
		t.Errorf("Lag = %d, want 1; a destination full of unrelated snapshots is not a synced one", got.Lag)
	}
}

// TestCompareSyncLagNeverGoesNegative: a destination can legitimately hold copies of snapshots the
// source has since forgotten in the same pass, and arithmetic that dipped below zero would render
// as a nonsensical negative lag on a status an operator reads to decide whether their DR copy is
// current.
func TestCompareSyncLagNeverGoesNegative(t *testing.T) {
	source := syncInventory{IDs: []string{"s1"}}
	dest := syncInventory{
		IDs:     []string{"d1", "d2"},
		Origins: map[string]string{"d1": "s1", "d2": "s1"}, // two copies of one source snapshot
	}
	if got := compareSync(source, dest); got.Lag != 0 {
		t.Fatalf("Lag = %d, want 0", got.Lag)
	}
}

// TestSnapshotInSyncScope pins the narrowing rule, including the case with no namespace tag.
//
// The cluster-manifests capture (adr/0011) belongs to NO namespace. A narrowed sync named specific
// namespaces, so it is not in scope — including it would copy cluster-scoped resources into a
// secondary the selection was written to keep them out of.
func TestSnapshotInSyncScope(t *testing.T) {
	inNamespace := restic.Snapshot{Tags: []string{restic.TagBase, restic.Tag(restic.TagKeyNamespace, "team-x")}}
	otherNamespace := restic.Snapshot{Tags: []string{restic.TagBase, restic.Tag(restic.TagKeyNamespace, "team-y")}}
	clusterScoped := restic.Snapshot{Tags: []string{restic.TagBase, "kind=cluster-manifests"}}

	cases := []struct {
		name       string
		snap       restic.Snapshot
		namespaces []string
		want       bool
	}{
		{"no selection takes everything", inNamespace, nil, true},
		{"no selection takes the cluster capture too", clusterScoped, nil, true},
		{"selection matches", inNamespace, []string{"team-x"}, true},
		{"selection excludes another namespace", otherNamespace, []string{"team-x"}, false},
		{"selection excludes the untagged cluster capture", clusterScoped, []string{"team-x"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := snapshotInSyncScope(tc.snap, tc.namespaces); got != tc.want {
				t.Fatalf("snapshotInSyncScope = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestEffectiveSyncModeForcesAppendOnlyOnImmutable: Object Lock physically forbids the delete half,
// so a Mirror against an Immutable destination would report a reconciliation it cannot perform.
// Forcing the mode keeps the object honest; the caller emits an Event so the substitution is not
// silent.
func TestEffectiveSyncModeForcesAppendOnlyOnImmutable(t *testing.T) {
	immutable := &locationBinding{Mode: cbv1.LocationModeImmutable}
	standard := &locationBinding{Mode: cbv1.LocationModeStandard}

	if mode, forced := effectiveSyncMode(cbv1.ExternalSyncModeMirror, immutable); mode != cbv1.ExternalSyncModeAppendOnly || !forced {
		t.Errorf("Mirror onto Immutable = (%q, forced=%v), want (AppendOnly, true)", mode, forced)
	}
	if mode, forced := effectiveSyncMode(cbv1.ExternalSyncModeAppendOnly, immutable); mode != cbv1.ExternalSyncModeAppendOnly || forced {
		t.Errorf("AppendOnly onto Immutable = (%q, forced=%v); nothing was substituted, so nothing "+
			"should be reported as forced", mode, forced)
	}
	if mode, _ := effectiveSyncMode(cbv1.ExternalSyncModeMirror, standard); mode != cbv1.ExternalSyncModeMirror {
		t.Errorf("Mirror onto Standard = %q, want Mirror", mode)
	}
	// An unset mode must land on the CRD's default rather than on the empty string, which no
	// branch downstream would recognise.
	if mode, _ := effectiveSyncMode("", standard); mode != cbv1.ExternalSyncModeMirror {
		t.Errorf("unset mode = %q, want the Mirror default", mode)
	}
}

// TestMirrorForgetArgsNamesSnapshotsNotATag is the safety property of the delete half.
//
// A tag filter describes a SCOPE, and a scope computed from an inventory that has since moved can
// match more than what was examined — at a DR secondary, silently. A list of explicit IDs cannot
// mean more than the snapshots that were actually compared.
func TestMirrorForgetArgsNamesSnapshotsNotATag(t *testing.T) {
	argv := mirrorForgetArgs([]string{"d9", "d10"})
	if argv[0] != "forget" {
		t.Fatalf("argv[0] = %q, want forget: %v", argv[0], argv)
	}
	for _, a := range argv {
		if a == "--tag" {
			t.Fatalf("mirror forget uses a tag filter (%v); a stale scope could then match more "+
				"snapshots than were compared", argv)
		}
		if strings.HasPrefix(a, "--keep-") {
			t.Fatalf("mirror forget carries a keep flag (%q), which would turn it into a retention pass", a)
		}
	}
	if !slices.Equal(argv[1:], []string{"d9", "d10"}) {
		t.Fatalf("argv names %v, want exactly the two computed IDs", argv[1:])
	}
}

// TestSyncQueueKeyIsNotTheRepositoryName keeps a multi-hour copy off the exclusive maintenance lane.
//
// The per-repo queue serialises init, forget, prune and check because each rewrites the snapshot set
// or the index. A copy does not — it writes new packs under a non-exclusive restic lock, exactly
// like a backup. Enqueued under the repository's own name it would stall that repository's prune and
// check for the whole copy, which is the opposite of what a secondary needs. The key still has to be
// per-destination, so two copies into the same repository cannot overlap.
func TestSyncQueueKeyIsNotTheRepositoryName(t *testing.T) {
	const repo = "dr-secondary"
	key := syncQueueKey(repo)
	if key == repo {
		t.Fatal("the sync lane IS the repository's exclusive lane; a copy would block prune and check")
	}
	if !strings.Contains(key, repo) {
		t.Fatalf("syncQueueKey = %q; it must still be per-destination, or two copies could overlap", key)
	}
	if syncQueueKey("a") == syncQueueKey("b") {
		t.Fatal("two destinations share one lane")
	}
}

// TestSyncScheduleRunsImmediatelyWhenNeverSynced: an operator creating a sync expects the secondary
// to start filling now, not at the next cron boundary — which for a nightly schedule is up to a day
// of silence on an object that says it is active.
func TestSyncScheduleRunsImmediatelyWhenNeverSynced(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)

	if act := syncSchedule("0 2 * * *", "UTC", nil, now); !act.Due {
		t.Error("a scheduled sync that has never run is not due; the secondary would stay empty until 02:00")
	}
	if act := syncSchedule("", "", nil, now); !act.Due {
		t.Error("an on-demand sync that has never run is not due; it would never run at all")
	}
}

// TestSyncScheduleOnDemandRunsExactlyOnce: an empty schedule means on-demand. Re-running it on
// every reconcile would turn "copy this once" into a hot loop moving data forever.
func TestSyncScheduleOnDemandRunsExactlyOnce(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	done := &metav1.Time{Time: now.Add(-time.Hour)}

	if act := syncSchedule("", "", done, now); act.Due {
		t.Fatal("an on-demand sync re-fired after succeeding")
	}
}

// TestSyncScheduleWaitIsMeasuredFromTheSameNow: the requeue delay and the "next copy at" message
// must describe one instant. Recomputing the wait from a second clock read would let a status say
// 02:00 while the requeue targeted a slightly different moment — small, but the kind of drift that
// makes a schedule look like it fires at the wrong time.
func TestSyncScheduleWaitIsMeasuredFromTheSameNow(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	act := syncSchedule("0 2 * * *", "UTC", &metav1.Time{Time: now}, now)

	if act.NextFire.IsZero() {
		t.Fatal("a cron schedule reported no next activation")
	}
	if got := act.NextFire.Sub(now); got != act.Wait {
		t.Fatalf("Wait = %v but NextFire is %v away; they were measured from different instants", act.Wait, got)
	}
}

// TestSyncScheduleSurfacesABadCron: a cron string that will never parse is spec-static, so it has
// to reach the object as a stated fault rather than being retried forever in silence.
func TestSyncScheduleSurfacesABadCron(t *testing.T) {
	if act := syncSchedule("not a cron", "", nil, time.Now()); act.Err == nil {
		t.Fatal("an unparseable schedule was accepted")
	}
}

// TestSyncEndpointRepoURLIsRcloneAddressed: reusing status.repositoryURL would put both
// repositories on restic's own s3 backend, which reads ONE credential set for the process — the
// exact limitation adr/0013 moved to rclone to escape. The endpoint would be right and the
// credentials wrong, which fails as "unreachable" and reads as a network problem.
func TestSyncEndpointRepoURLIsRcloneAddressed(t *testing.T) {
	e := &syncEndpoint{
		Remote: mover.SyncRemoteSource,
		Binding: &locationBinding{
			S3:        cbv1.S3Spec{Endpoint: "https://s3.example.com", Bucket: "backups", Prefix: "team"},
			ClusterID: "cid-1",
		},
	}
	got := e.RepoURL()
	if !strings.HasPrefix(got, "rclone:src:") {
		t.Fatalf("RepoURL = %q, want an rclone:src: address", got)
	}
	if strings.Contains(got, "s3.example.com") {
		t.Fatalf("RepoURL = %q carries the endpoint; an rclone remote holds its own endpoint, and "+
			"repeating it here would be read as part of the bucket name", got)
	}
	if got != "rclone:src:backups/team/cid-1" {
		t.Fatalf("RepoURL = %q, want rclone:src:backups/team/cid-1", got)
	}
}

// TestSyncEndpointConfigKeepsCredentialsOutOfTheEnvValues: the non-secret rclone configuration is
// built inline, the two credential keys are not. An access key rendered as a plain env value would
// be readable in the pod spec by anyone who can get a Pod in the operator namespace.
func TestSyncEndpointConfigKeepsCredentialsOutOfTheEnvValues(t *testing.T) {
	e := &syncEndpoint{
		Remote:  mover.SyncRemoteDest,
		Binding: &locationBinding{S3: cbv1.S3Spec{Endpoint: "https://s3.example.com", Region: "eu-west"}},
	}
	for _, env := range e.rcloneConfigEnv() {
		if strings.HasSuffix(env.Name, mover.RcloneKeyAccessKeyID) ||
			strings.HasSuffix(env.Name, mover.RcloneKeySecretAccessKey) {
			t.Fatalf("%s is set inline; credentials must arrive by secretKeyRef", env.Name)
		}
		if env.ValueFrom != nil {
			t.Fatalf("%s is a reference; the non-secret configuration should be a plain value", env.Name)
		}
	}
	// The region is optional and must be omitted rather than sent empty: rclone treats an empty
	// region as a literal, and some S3 implementations reject it.
	noRegion := &syncEndpoint{Remote: mover.SyncRemoteDest, Binding: &locationBinding{S3: cbv1.S3Spec{}}}
	for _, env := range noRegion.rcloneConfigEnv() {
		if strings.HasSuffix(env.Name, mover.RcloneKeyRegion) {
			t.Fatalf("%s was emitted for a location with no region", env.Name)
		}
	}
}

// TestSameEndpointComparesRepositoriesNotNames: admission rejects source == destination by NAME,
// but two differently-named locations can address the same bucket, prefix and cluster ID. restic
// would then open one repository as both sides of the copy and contend with its own lock.
//
// The ALIASED case below is the one that matters, and it is written with two DISTINCT
// BackupRepository objects on purpose. An earlier version of this test handed both endpoints the
// same *BackupRepository, which no reconcile can produce: a repository's name is derived from its
// location's on either plane, so two locations always own two objects. Comparing names alone
// therefore only reproduced admission rule 9, and the alias — the very case this test is named
// after — went straight through. The identity that decides is the resolved repository URL.
func TestSameEndpointComparesRepositoriesNotNames(t *testing.T) {
	const oneRepoURL = "s3:https://s3.example/bucket/crystal/cid-1"

	withRepo := func(locName, repoName, url string) *syncEndpoint {
		repo := &cbv1.BackupRepository{}
		repo.Name = repoName
		repo.Status.RepositoryURL = url
		return &syncEndpoint{Binding: &locationBinding{Name: locName}, Repo: repo}
	}

	// Two locations, two repository objects, ONE repository in the bucket.
	if !sameEndpoint(withRepo("primary", "primary", oneRepoURL), withRepo("also-primary", "also-primary", oneRepoURL)) {
		t.Fatal("two differently-named locations addressing one repository were accepted as a sync pair")
	}

	// The same-name backstop still holds when neither side has resolved a URL yet — that is what
	// keeps a cluster running with the admission policies disabled from self-copying.
	if !sameEndpoint(withRepo("primary", "primary", ""), withRepo("primary", "primary", "")) {
		t.Fatal("one repository named on both sides was accepted as a sync pair")
	}

	if sameEndpoint(withRepo("primary", "primary", oneRepoURL),
		withRepo("secondary", "secondary", "s3:https://s3.example/other/crystal/cid-1")) {
		t.Fatal("two distinct repositories were rejected as the same")
	}

	// An unresolved side is not "the same" — it is not yet anything, and reporting SameRepository
	// there would send an operator looking for a configuration error that does not exist. Neither a
	// missing repository object nor a repository whose URL is still empty may match by URL.
	if sameEndpoint(withRepo("primary", "primary", oneRepoURL),
		&syncEndpoint{Binding: &locationBinding{Name: "pending"}}) {
		t.Fatal("an endpoint whose repository does not exist yet was reported as identical")
	}
	if sameEndpoint(withRepo("primary", "primary", ""), withRepo("pending", "pending", "")) {
		t.Fatal("two repositories that have not resolved a URL yet were reported as identical")
	}
}

// TestSyncEndpointReadyRefusesAnIncompleteEndpoint: each of these is a state a copy would otherwise
// enter and fail inside restic, where the reason reaches an operator as a mover exit code instead
// of a sentence on the object.
func TestSyncEndpointReadyRefusesAnIncompleteEndpoint(t *testing.T) {
	ready := &cbv1.BackupRepository{}
	ready.Status.Initialized = true
	base := func() *syncEndpoint {
		return &syncEndpoint{
			Binding:  &locationBinding{ClusterID: "cid-1", S3: cbv1.S3Spec{Bucket: "b"}},
			Repo:     ready,
			Password: "pw",
		}
	}

	if _, _, ok := syncEndpointReady(base(), "source"); !ok {
		t.Fatal("a fully-resolved endpoint was rejected")
	}

	uninit := base()
	uninit.Repo = &cbv1.BackupRepository{}
	noCluster := base()
	noCluster.Binding = &locationBinding{S3: cbv1.S3Spec{Bucket: "b"}}
	noBucket := base()
	noBucket.Binding = &locationBinding{ClusterID: "cid-1"}
	noKey := base()
	noKey.Password = ""

	for name, e := range map[string]*syncEndpoint{
		"repository not initialized": uninit,
		"no effective cluster ID":    noCluster,
		"no bucket":                  noBucket,
		"no password":                noKey,
	} {
		t.Run(name, func(t *testing.T) {
			reason, message, ok := syncEndpointReady(e, "source")
			if ok {
				t.Fatal("endpoint accepted")
			}
			if reason == "" || message == "" {
				t.Fatalf("rejected with reason=%q message=%q; the object would say nothing", reason, message)
			}
			if !strings.Contains(message, "source") {
				t.Errorf("message %q does not say WHICH side is at fault", message)
			}
		})
	}
}

// TestSyncResourceNamesAreDistinctPerPlane: both planes' Jobs and Secrets land in the operator
// namespace, which has nothing else to tell a cluster sync named "nightly" from a namespaced one.
// A collision would have two syncs adopting each other's Job — and, through the per-Job Secret,
// each other's credentials and keys.
func TestSyncResourceNamesAreDistinctPerPlane(t *testing.T) {
	cluster := syncResourceName(syncJobPrefixCluster, "nightly", "copy")
	namespaced := syncResourceName(syncJobPrefixNamespaced, "nightly", "copy")
	if cluster == namespaced {
		t.Fatalf("both planes name their copy Job %q", cluster)
	}
	// And the steps within one sync must not collide either, or an inventory would re-adopt the
	// copy Job it is meant to run after.
	steps := map[string]bool{}
	for _, step := range []string{"copy", "src-inv", "dst-inv", "mirror-forget"} {
		name := syncResourceName(syncJobPrefixCluster, "nightly", step)
		if steps[name] {
			t.Fatalf("step %q collides with an earlier one (%q)", step, name)
		}
		steps[name] = true
	}
}

// TestSyncScopeDescriptionSaysWhatWillMove: this string is what an operator reads to decide whether
// a sync is doing what they meant, so "the whole repository" and a namespace list must never render
// the same way.
func TestSyncScopeDescriptionSaysWhatWillMove(t *testing.T) {
	whole := syncScopeDescription(nil)
	narrowed := syncScopeDescription([]string{"team-x", "team-y"})
	if whole == narrowed {
		t.Fatal("a whole-repository sync and a narrowed one describe themselves identically")
	}
	if !strings.Contains(narrowed, "team-x") || !strings.Contains(narrowed, "team-y") {
		t.Fatalf("narrowed description %q does not name the namespaces", narrowed)
	}
}
