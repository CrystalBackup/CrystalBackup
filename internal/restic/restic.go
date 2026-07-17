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

// Package restic is the single source of truth for CrystalBackup's restic repository
// contract: how a snapshot is named and tagged, what S3 URL the one shared repository
// lives at, and how the per-PVC retention `forget` is invoked — plus the inverse
// parsers that read that identity back off `restic snapshots --json`.
//
// The authoritative contract is spec/02-api.md §"Repository layout & snapshot identity"
// (host = clusterID; paths /data/<ns>/<pvc>, /manifests/<ns>, /cluster-manifests; tags
// crystalbackup, tenant=, namespace=, pvc=, kind=, schedule=, run=). These strings are
// SECURITY-LOAD-BEARING: M2 restore scopes a tenant's data by matching the namespace=/
// pvc= tags and the /data/<ns>/<pvc> subtree, and M5 right-to-erasure runs
// `restic forget --tag tenant=<t>` (or namespace=/pvc=). A one-character drift here
// would silently mis-scope tenancy — restore or erase the wrong tenant's data — which is
// why every builder is pinned by property tests and asserted verbatim.
//
// Everything in this package is a pure, deterministic function of its arguments. Every
// argument is a value the operator has already RESOLVED (clusterID, tenant, namespace,
// pvc, kind, schedule, run) — never a user-writable free field — so the outputs can be
// trusted as tenancy boundaries. Nothing here imports controller-runtime or performs I/O;
// it depends only on the stdlib and the RetentionSpec CRD field type, so any controller
// or the CLI can import it without a cycle or a client.
package restic

import (
	"strconv"
	"strings"

	"github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
)

// TagBase is the bare marker tag stamped on every snapshot CrystalBackup writes. It is
// the anchor for every repo-wide operation: `restic snapshots --tag crystalbackup`
// (discovery) and `restic forget --tag crystalbackup` (retention) both filter on it, so a
// snapshot missing this tag is invisible to the operator and is never discovered, retained
// or erased. Unlike the identity tags below it has no "=value": it is a bare word.
const TagBase = "crystalbackup"

// Tag keys. A tag is written "<key>=<value>" (see Tag); these are the fixed keys of the
// snapshot-identity contract in spec/02-api.md. They are load-bearing far beyond this
// package — the restore path (M2) reads namespace=/pvc= back to scope a tenant, and the
// erasure path (M5) filters `forget` by tenant=/namespace=/pvc= — so they are centralised
// here and asserted verbatim rather than spelled out at each call site.
const (
	// TagKeyTenant labels the resolved tenant a snapshot belongs to; the erasure key
	// for tenant-wide right-to-erasure (ErasureTarget.Tenant → forget --tag tenant=<t>).
	TagKeyTenant = "tenant"
	// TagKeyNamespace labels the origin namespace; a discovery grouping key and an
	// erasure/restore scope (forget --tag namespace=<ns>).
	TagKeyNamespace = "namespace"
	// TagKeyPVC labels the PersistentVolumeClaim of a data snapshot (absent on manifests
	// and cluster-manifests); narrows erasure to a single volume (namespace=+pvc=).
	TagKeyPVC = "pvc"
	// TagKeyKind partitions snapshots into data | manifests | cluster-manifests (see the
	// Kind* values); distinguishes the three snapshot classes sharing one repository.
	TagKeyKind = "kind"
	// TagKeySchedule labels the originating schedule; OMITTED for an ad-hoc/manual run
	// (see the constructors), mirroring apiconst.LabelSchedule.
	TagKeySchedule = "schedule"
	// TagKeyRun labels the run this snapshot belongs to; equals the Backup.metadata.name
	// and is the identity discovery groups a namespace's snapshots by (namespace, run).
	TagKeyRun = "run"
)

// Kind tag values (the value of the kind= tag). They partition a repository's snapshots
// into the three classes, each with a distinct path shape and its own retention chain.
const (
	// KindData is a single PVC's data snapshot at /data/<namespace>/<pvc>.
	KindData = "data"
	// KindManifests is a namespace's captured manifests at /manifests/<namespace>.
	KindManifests = "manifests"
	// KindClusterManifests is the cluster-scoped objects snapshot at /cluster-manifests,
	// one per run (adr/0011); belongs to no tenant or namespace.
	KindClusterManifests = "cluster-manifests"
)

// Tag renders one restic tag as "key=value". Centralising the "="-joined format means the
// identity builders below and every reader (TagValue, discovery, restore, erasure) share a
// single definition of a tag's shape, so encode and decode can never drift apart.
func Tag(key, value string) string {
	return key + "=" + value
}

// TagValue returns the value of the first "key=value" tag in tags and whether such a tag
// was present. It is the exact inverse of Tag and the primitive discovery and restore use
// to read identity back off a snapshot (e.g. TagValue(s.Tags, TagKeyNamespace) to learn a
// snapshot's namespace). The match requires the trailing "=", so a key is never a prefix of
// a longer key (looking up "name" does not match "namespace=..."), and the bare TagBase
// marker (which has no "=") is never returned by a key lookup — test its presence with a
// direct string compare instead. A value that itself contains "=" is returned intact, since
// only the first "=" delimits key from value.
func TagValue(tags []string, key string) (string, bool) {
	prefix := key + "="
	for _, t := range tags {
		if strings.HasPrefix(t, prefix) {
			return t[len(prefix):], true
		}
	}
	return "", false
}

// Identity is the restic identity of one snapshot: the three things the mover hands to
// `restic backup` — the --host, the single backup path, and the ordered --tag list — that
// together make the snapshot addressable and correctly scoped. Every field is a pure
// function of operator-resolved inputs (no user-writable free field feeds it), which is
// precisely what lets M2 restore and M5 erasure treat these strings as tenancy boundaries.
type Identity struct {
	// Host is the restic --host, always the clusterID: the multi-cluster discriminator
	// inside one shared bucket and the leading key of per-PVC retention grouping
	// (`--group-by host,paths`).
	Host string
	// Path is the single restic backup path: /data/<ns>/<pvc>, /manifests/<ns>, or
	// /cluster-manifests. It is both the snapshot's retention-grouping identity and the
	// subtree a restore addresses (restic restore <id>:<path>).
	Path string
	// Tags are the restic --tag values in a fixed, deterministic order that always begins
	// with the bare TagBase marker, then kind=, then this kind's identity tags, then
	// schedule=/run=. Order carries no meaning to restic (tags are a set on a snapshot) but
	// is pinned so the mover invocation and the tests are byte-reproducible.
	Tags []string
}

// scheduleRunTags appends the trailing schedule=/run= tags shared by all three kinds.
//
// The schedule tag is OMITTED entirely for an ad-hoc / manual run (schedule == "") rather
// than emitted as an empty "schedule=". This mirrors apiconst.LabelSchedule ("absent for a
// manual/ad-hoc run") and apiconst.RunName's empty-schedule handling: a manual run still
// carries a run tag (its Backup name), but attaching a bare "schedule=" would create a
// distinct, meaningless tag value that pollutes `forget --tag schedule=` filtering and
// discovery grouping. The run tag is always present.
func scheduleRunTags(tags []string, schedule, run string) []string {
	if schedule != "" {
		tags = append(tags, Tag(TagKeySchedule, schedule))
	}
	return append(tags, Tag(TagKeyRun, run))
}

// DataIdentity builds the identity of a single PVC-data snapshot. Path is
// /data/<namespace>/<pvc>; tags are crystalbackup, kind=data, then the FULL
// tenant/namespace/pvc identity, then schedule/run. This is the snapshot M2 restores as a
// subtree (restic restore <id>:/data/<ns>/<pvc>) and the unit per-PVC retention thins,
// because `forget --group-by host,paths` groups it by (host,paths) == (clusterID,
// namespace, pvc). Host is the clusterID.
func DataIdentity(clusterID, tenant, namespace, pvc, schedule, run string) Identity {
	tags := []string{
		TagBase,
		Tag(TagKeyKind, KindData),
		Tag(TagKeyTenant, tenant),
		Tag(TagKeyNamespace, namespace),
		Tag(TagKeyPVC, pvc),
	}
	return Identity{
		Host: clusterID,
		Path: "/data/" + namespace + "/" + pvc,
		Tags: scheduleRunTags(tags, schedule, run),
	}
}

// ManifestsIdentity builds the identity of a namespace-manifests snapshot. Path is
// /manifests/<namespace>; tags are crystalbackup, kind=manifests, tenant, namespace, then
// schedule/run — deliberately NO pvc tag: manifests are a per-namespace chain, not
// per-PVC, so they form their own retention group ((clusterID, namespace) via the
// /manifests/<ns> path) independent of any volume. Host is the clusterID.
func ManifestsIdentity(clusterID, tenant, namespace, schedule, run string) Identity {
	tags := []string{
		TagBase,
		Tag(TagKeyKind, KindManifests),
		Tag(TagKeyTenant, tenant),
		Tag(TagKeyNamespace, namespace),
	}
	return Identity{
		Host: clusterID,
		Path: "/manifests/" + namespace,
		Tags: scheduleRunTags(tags, schedule, run),
	}
}

// ClusterManifestsIdentity builds the identity of the cluster-scoped-objects snapshot, one
// per run (adr/0011). Path is the fixed /cluster-manifests; tags are ONLY crystalbackup,
// kind=cluster-manifests, then schedule/run — it carries NEITHER tenant, namespace nor pvc.
// Cluster-scoped objects belong to no tenant or namespace, so this snapshot is deliberately
// invisible to tenant-scoped erasure (M5 forget --tag tenant=/namespace=) and to
// namespace-keyed discovery projection; it is restored only by an explicit admin
// ClusterRestore that reads the repo directly. Host is the clusterID.
func ClusterManifestsIdentity(clusterID, schedule, run string) Identity {
	tags := []string{
		TagBase,
		Tag(TagKeyKind, KindClusterManifests),
	}
	return Identity{
		Host: clusterID,
		Path: "/cluster-manifests",
		Tags: scheduleRunTags(tags, schedule, run),
	}
}

// schemes are the URL schemes RepoURL preserves on an endpoint. They are checked in order,
// and https:// must precede http:// only for readability (neither is a prefix of the other).
var schemes = []string{"https://", "http://"}

// RepoURL builds the restic S3 URL of the ONE shared repository a cluster writes to, at
// <prefix>/<clusterID>/ under the bucket. Every namespace and tenant of a cluster shares
// this single repository; tenancy is carried by tags, not by the path (R20, adr/0009), so
// there is deliberately no per-namespace or per-tenant suffix.
//
// The form is restic's "s3:<endpoint>/<bucket>/<prefix>/<clusterID>". For example:
//
//	RepoURL("https://s3.example.net", "team-x-backups", "crystal", "prod-eu-1")
//	  == "s3:https://s3.example.net/team-x-backups/crystal/prod-eu-1"
//
// Behaviour of the parts:
//   - A leading scheme (https:// or http://) on the endpoint is preserved; a bare host
//     endpoint (no scheme) is used as-is (s3:host/bucket/...).
//   - An empty prefix drops that segment entirely: s3:<endpoint>/<bucket>/<clusterID>.
//   - Accidental double slashes introduced by a trailing/leading slash on any input are
//     collapsed in the path portion, but the scheme's own "//" is never touched.
//   - There is no trailing slash.
//
// This exact string is published in BackupRepository.status.repositoryURL and is what
// `restic -r <url>` opens, so it must be byte-stable across releases.
func RepoURL(endpoint, bucket, prefix, clusterID string) string {
	// Detach a leading scheme so its "//" survives the double-slash collapse below.
	scheme, host := "", endpoint
	for _, s := range schemes {
		if strings.HasPrefix(endpoint, s) {
			scheme, host = s, endpoint[len(s):]
			break
		}
	}
	// Assemble the path segments in fixed order, omitting an empty prefix.
	segments := []string{host, bucket}
	if prefix != "" {
		segments = append(segments, prefix)
	}
	segments = append(segments, clusterID)
	path := strings.Join(segments, "/")
	// Collapse runs of slashes that trailing/leading slashes on the inputs introduced,
	// then trim any trailing slash. The scheme was split off above, so "https://" is safe.
	for strings.Contains(path, "//") {
		path = strings.ReplaceAll(path, "//", "/")
	}
	path = strings.TrimRight(path, "/")
	return "s3:" + scheme + path
}

// ForgetArgs builds the argument list for the per-PVC retention `restic forget`. It always
// starts with the filter + grouping prefix
//
//	--tag crystalbackup --group-by host,paths
//
// which scopes forget to CrystalBackup's snapshots and groups them by (host, paths) ==
// (clusterID, namespace, pvc) so each PVC's chain is thinned in isolation (manifests, keyed
// by their own /manifests/<ns> path, fall into separate groups). It then appends a
// --keep-<bucket> <n> pair for each SET retention field, in the fixed order last, hourly,
// daily, weekly, monthly, yearly; unset (zero) fields are skipped.
//
// A non-positive count is treated as unset and skipped: a keep is a non-negative
// cardinality, and this builder must never be able to emit a nonsensical "--keep-daily -1".
// For every real input this is identical to "skip zero fields" (admission never produces a
// negative keep).
//
// If EVERY field is zero, only the base --tag/--group-by prefix is returned, with no
// --keep-* flag. A `restic forget` with no keep policy forgets everything, so the CALLER
// MUST NOT run forget on this all-zero result. Admission rule 3 guarantees at least one keep
// for a Standard-mode schedule, so this degenerate all-zero case never reaches a real run;
// it is returned (rather than panicking) so callers can detect it as len(args) == 4.
func ForgetArgs(r v1alpha1.RetentionSpec) []string {
	args := []string{"--tag", TagBase, "--group-by", "host,paths"}
	keep := func(bucket string, n int32) {
		if n > 0 {
			args = append(args, "--keep-"+bucket, strconv.FormatInt(int64(n), 10))
		}
	}
	keep("last", r.KeepLast)
	keep("hourly", r.KeepHourly)
	keep("daily", r.KeepDaily)
	keep("weekly", r.KeepWeekly)
	keep("monthly", r.KeepMonthly)
	keep("yearly", r.KeepYearly)
	return args
}

// SnapshotsArgs is the complete restic argv (subcommand first) discovery inventories the
// repository with: `snapshots --json --tag crystalbackup`. The --tag filter scopes the listing
// to CrystalBackup's own snapshots (never a foreign tool's), and --json makes the output the
// machine-readable array ParseSnapshots decodes. Unlike ForgetArgs (retention flags a caller
// prepends "forget" to), this is a whole command with no dynamic parts.
func SnapshotsArgs() []string {
	return []string{"snapshots", "--json", "--tag", TagBase}
}

// ForgetCommand is the complete restic argv for the per-PVC retention forget: the "forget"
// subcommand followed by ForgetArgs(r). It is a convenience so callers do not re-prepend the
// subcommand (and cannot forget to). It returns ok=false when r requests NO keep (ForgetArgs
// degenerates to the bare prefix): a keep-less forget would drop every snapshot, so the caller
// must skip running it — never forget an empty policy.
func ForgetCommand(r v1alpha1.RetentionSpec) (argv []string, ok bool) {
	flags := ForgetArgs(r)
	if len(flags) <= 4 { // only the --tag/--group-by prefix ⇒ no keep policy set.
		return nil, false
	}
	return append([]string{"forget"}, flags...), true
}

// UnlockArgs is the complete restic argv that clears stale repository locks: `unlock`. restic
// removes only locks past its staleness window by default, so this is safe to run
// opportunistically after a mover crash without disturbing a genuinely in-progress operation.
func UnlockArgs() []string {
	return []string{"unlock"}
}
