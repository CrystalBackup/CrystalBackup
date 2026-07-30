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

package refdocs

import (
	"fmt"
	"strconv"
	"strings"
)

// MetricsPage is the site path the metrics reference is written to, relative to the repository
// root.
const MetricsPage = "website/src/content/docs/reference/metrics.md"

// section groups the families for reading. The grouping is presentation, not fact — the facts all
// come from the Descs — but an unclaimed family is still an error: a new family must appear
// somewhere on the page rather than silently not being documented, which is the failure mode this
// whole package exists to close.
type section struct {
	title    string
	prefixes []string
	// note is the one thing on this page a reader could not derive from the table above it.
	// Written by hand, deliberately short, and only where the table would otherwise mislead.
	note string
}

var sections = []section{
	{
		title:    "Operator identity",
		prefixes: []string{"crystalbackup_build_info"},
		note: "Always present, on every operator, from the first scrape. It is the series to " +
			"check first when `/metrics` looks empty: if this one is missing the scrape is not " +
			"reaching the operator at all, and if it is the only one there is simply nothing to " +
			"report yet.",
	},
	{
		title:    "Backup and schedules",
		prefixes: []string{"crystalbackup_backup_", "crystalbackup_schedule_"},
		note: "Three series carry the word `failure` and none of them is a spelling of another. " +
			"`crystalbackup_backup_failures` is a gauge counting failed Backup objects that still " +
			"EXIST, so it drops back to zero when a schedule's history limit deletes them — the " +
			"question it answers is what wreckage is lying around right now, which is a dashboard " +
			"question. `crystalbackup_backup_failures_total` is an in-process counter of every " +
			"Backup that reached a failed phase; read it with `increase()`, and know that an " +
			"operator restart does not zero it but makes it VANISH (a counter series only exists " +
			"once something has incremented it), which `increase()` cannot see across. " +
			"`crystalbackup_backup_last_failure_timestamp_seconds` is the Unix time of the most recent failure, " +
			"derived from the Backup objects at scrape time and therefore rebuilt intact after a " +
			"restart; it is ABSENT for a series that has never failed. " +
			"`CrystalbackupBackupFailed` reads the last two together, because the counter cannot " +
			"survive a restart and the timestamp cannot survive the object being " +
			"garbage-collected.\n\n" +
			"`crystalbackup_schedule_active` is `1` for an unpaused schedule and `0` for one that " +
			"exists and is paused — it is not absent while paused, which is what makes " +
			"`CrystalbackupSchedulePausedTooLong` possible at all.",
	},
	{
		title:    "Cluster backup runs",
		prefixes: []string{"crystalbackup_clusterbackup_"},
		note: "Run-level, with no `namespace` label: a ClusterBackup fans out over many " +
			"namespaces, and its per-namespace detail is in the Backup families above. " +
			"`namespaces_failed` above zero on a run that otherwise reports success is the " +
			"signal — the run completed, and some namespace in it did not.",
	},
	{
		title:    "Restore",
		prefixes: []string{"crystalbackup_restore_"},
		note: "One family covers Restore and ClusterRestore alike. `mode` appears only on the " +
			"duration histogram and the failure counter, because Recreate and Overwrite fail for " +
			"entirely different reasons and a merged count hides which one is broken.",
	},
	{
		title:    "Repository",
		prefixes: []string{"crystalbackup_repository_"},
		note: "This is the family where absence carries the most meaning. A repository that has " +
			"never been verified emits no `last_check_*` series at all rather than a zero, and an " +
			"Immutable location — which never prunes, by design — emits no " +
			"`last_maintenance_timestamp_seconds`. Both absences are what keep " +
			"`CrystalbackupRepositoryCheckFailed` and `CrystalbackupMaintenanceStalled` silent on " +
			"repositories they have nothing true to say about.\n\n" +
			"There is no separate \"stored bytes\" series. `restic stats --mode raw-data` is not " +
			"something the operator runs, so the only number available would have been the one " +
			"`crystalbackup_repository_size_bytes` already publishes, and two names for one " +
			"reading invite a reader to reason about a gap that does not exist.",
	},
	{
		title:    "Discovery",
		prefixes: []string{"crystalbackup_discovery_"},
		note: "Discovery is the repository→Backup projection: what `kubectl get backups` lists " +
			"comes from here. `orphan_snapshots` above zero is not an error — it is DR data for " +
			"namespaces that no longer exist, restorable through ClusterRestore.",
	},
	{
		title:    "Right to erasure",
		prefixes: []string{"crystalbackup_erasure_"},
		note: "`crystalbackup_erasure_blocked` above zero is also not an error: it is an erasure " +
			"waiting, correctly, on an object-lock window that exists to make deletion " +
			"impossible. It is worth an alert anyway, because a right-to-erasure request parked " +
			"for weeks is a compliance clock running regardless of whose fault it is.",
	},
	{
		title:    "Movers, concurrency and queueing",
		prefixes: []string{"crystalbackup_mover_"},
		note: "A census of the mover Jobs in the operator's own namespace. `queue_depth` is work " +
			"admitted by a controller with no Job running yet: sustained above zero while " +
			"`active` sits at `concurrency_limit` means the limit, not the storage, is what backups " +
			"are waiting on. A `concurrency_limit` of `0` means unlimited.",
	},
	{
		title:    "Admission",
		prefixes: []string{"crystalbackup_webhook_"},
		note: "The operator's own dynamic webhook only. Denials from the static " +
			"ValidatingAdmissionPolicies are counted by the apiserver's own admission metrics and " +
			"never appear here, so a zero on this series does not mean nothing was rejected. See " +
			"[Admission rules](/CrystalBackup/docs/reference/admission/) for which check lives " +
			"where.",
	},
	{
		title:    "Snapshot exposure and coexistence",
		prefixes: []string{"crystalbackup_exposure_", "crystalbackup_pvc_"},
		note: "`crystalbackup_pvc_volumesnapshot_count` is the one family carrying a per-PVC " +
			"label, and it is a deliberate exception: its cardinality is bounded by the number of " +
			"live PVCs, and the pile-up it detects is worth that cost. It counts VolumeSnapshots " +
			"from **every** tool, an incumbent backup product's included — the ceph-csi flatten " +
			"thresholds do not care who created the snapshot.",
	},
	{
		title:    "External sync",
		prefixes: []string{"crystalbackup_externalsync_"},
		note: "The one family that deliberately breaks the absence rule above: a sync that has " +
			"never completed publishes `last_success_timestamp_seconds` as **0**, not as nothing, " +
			"so that a secondary broken from the day it was created is visible rather than being " +
			"the one case an alert cannot see. Treat that `0` as a sentinel, never as a " +
			"timestamp — `time() - 0` is fifty-odd years, and any staleness expression written " +
			"against it needs the `> 0` guard the shipped rule uses.\n\n" +
			"There is no bytes-copied counter. `restic copy --json` emits no machine-readable " +
			"summary, so there is no byte count to publish, and an estimate presented as a " +
			"measurement is worse for a secondary than an honest gap.",
	},
}

// RenderMetrics builds the whole Metrics reference page.
func RenderMetrics(families []Family) ([]byte, error) {
	var b strings.Builder

	b.WriteString(frontMatter(
		"Metrics",
		"Every crystalbackup_ Prometheus series the operator publishes, with the labels it "+
			"actually carries — generated from internal/metrics.",
	))
	b.WriteString(generatedNotice)
	b.WriteString(metricsIntro)

	claimed := map[string]bool{}
	for _, s := range sections {
		var in []Family
		for _, f := range families {
			if claimed[f.Name] {
				continue
			}
			for _, p := range s.prefixes {
				if strings.HasPrefix(f.Name, p) {
					in = append(in, f)
					claimed[f.Name] = true
					break
				}
			}
		}
		if len(in) == 0 {
			return nil, fmt.Errorf("section %q matches no family; its prefixes %v are stale",
				s.title, s.prefixes)
		}

		fmt.Fprintf(&b, "## %s\n\n", s.title)
		if s.note != "" {
			b.WriteString(s.note + "\n\n")
		}
		writeFamilyTable(&b, in)
		writeBuckets(&b, in)
	}

	for _, f := range families {
		if !claimed[f.Name] {
			return nil, fmt.Errorf("%s belongs to no section of the metrics page; add a prefix to "+
				"refdocs.sections rather than letting a published series go undocumented", f.Name)
		}
	}

	b.WriteString(metricsOutro)
	return []byte(b.String()), nil
}

func writeFamilyTable(b *strings.Builder, in []Family) {
	b.WriteString("| Series | Type | Labels | Meaning |\n")
	b.WriteString("| --- | --- | --- | --- |\n")
	for _, f := range in {
		fmt.Fprintf(b, "| `%s` | %s | %s | %s |\n",
			f.Name, f.Kind, labelList(f.Labels), escapeCell(f.Help))
	}
	b.WriteString("\n")
}

func labelList(labels []string) string {
	if len(labels) == 0 {
		return "*(none)*"
	}
	quoted := make([]string, 0, len(labels))
	for _, l := range labels {
		quoted = append(quoted, "`"+l+"`")
	}
	return strings.Join(quoted, " ")
}

// writeBuckets prints the real upper bounds of any histogram in the section. They come from a
// Gather of the operator's own registry, so they are the boundaries a scrape returns.
func writeBuckets(b *strings.Builder, in []Family) {
	for _, f := range in {
		if f.Kind != KindHistogram {
			continue
		}
		bounds := make([]string, 0, len(f.Buckets))
		for _, v := range f.Buckets {
			bounds = append(bounds, "`"+strconv.FormatFloat(v, 'f', -1, 64)+"`")
		}
		fmt.Fprintf(b, "Buckets of `%s`, in seconds: %s.\n\n", f.Name, strings.Join(bounds, ", "))
	}
}

// escapeCell keeps a help string from breaking the markdown table it sits in.
func escapeCell(s string) string { return strings.ReplaceAll(s, "|", `\|`) }

const generatedNotice = "<!-- GENERATED FILE — do not edit. Run `make observability-docs` after " +
	"changing internal/metrics or internal/alerts. -->\n\n"

const metricsIntro = `Every series the operator publishes, with the label set it really carries.

This page is generated from ` + "`internal/metrics`" + `: the names, the label sets and the
descriptions below are read back out of the ` + "`prometheus.Desc`" + ` values the collectors
register, and the histogram buckets out of a real exposition. It is the same source a scrape is
served from, so it cannot describe a series that does not exist or a label that is not there.

That matters more than tidiness. A label that is documented and not emitted produces an alert
expression which is valid PromQL, evaluates without error, and matches nothing — forever, in
silence. On a backup tool, silence is indistinguishable from health.

## How to read this page

### Two kinds of series, and only two

**Derived at scrape.** Every gauge here is recomputed from live object state each time
Prometheus scrapes, through the operator's cached client. The operator holds no counter for
them, so restarting it changes nothing: the value after a restart is the value before it, because
both are read from the same objects.

**Event-driven.** The ` + "`_total`" + ` counters and the histograms are the exception. They are
incremented once at the transition into a terminal phase, after the status write that makes the
transition durable, and they **reset to zero when the operator restarts**. Write expressions over
them with ` + "`increase()`" + ` or ` + "`rate()`" + `, never against the raw value.

The split is exact, and the generator enforces it: every ` + "`_total`" + ` family and every
histogram is event-driven, and every other family is derived at scrape. If that ever stops being
true, this page fails to regenerate rather than shipping the wrong rule.

Histograms expose the usual ` + "`_bucket`" + `, ` + "`_sum`" + ` and ` + "`_count`" + ` series.
Their bucket boundaries are printed under each table.

### Absence is a value

A series that is not there does not mean zero. It means *not measured*, and the distinction is
deliberate in several places:

- a repository that has never been verified emits **no** ` + "`crystalbackup_repository_last_check_*`" + `
  series — not a zero, and not a 1970 timestamp;
- an Immutable location emits no ` + "`crystalbackup_repository_last_maintenance_timestamp_seconds`" + `,
  because it never prunes;
- a schedule whose cron expression cannot be parsed emits no
  ` + "`crystalbackup_schedule_period_seconds`" + `.

This is what keeps the shipped alerts quiet about things they have nothing true to say about: a
location created five minutes ago does not page critical, because ` + "`== 0`" + ` cannot match a
series that is absent. It also means that if you want to alert on *unmeasured*, you have to say so
with ` + "`absent()`" + ` — nothing else will.

The one deliberate exception is ` + "`crystalbackup_externalsync_last_success_timestamp_seconds`" + `,
which publishes ` + "`0`" + ` rather than nothing for a sync that has never completed. See
[External sync](#external-sync).

### The label contract

Every series carries the ` + "`crystalbackup_`" + ` prefix. Beyond that, the label sets are per
family — the tables below are the authority, not this list — but the shared names mean the same
thing everywhere:

| Label | Values | Notes |
| --- | --- | --- |
| ` + "`namespace`" + ` | a namespace name, or empty | Empty on cluster-scoped repository, discovery and external-sync series: those describe the cluster plane, which belongs to no namespace. |
| ` + "`tenant`" + ` | the namespace's ` + "`crystalbackup.io/tenant`" + ` label, else the namespace name | One tenant per namespace. It is what you route a tenant-visible alert on. |
| ` + "`cluster`" + ` | a ` + "`ClusterBackupLocation`" + `'s ` + "`spec.clusterID`" + `, or empty | So one Prometheus can hold several clusters without collision. **It is resolved only from cluster locations**: a series whose location is a namespaced ` + "`BackupLocation`" + `, or a cluster location with no ` + "`clusterID`" + ` set, carries an empty ` + "`cluster`" + ` — a gap, deliberately, rather than a borrowed identity. |
| ` + "`scope`" + ` | ` + "`cluster`" + ` or ` + "`namespace`" + ` | **Lowercase, and deliberately not the API enum.** ` + "`BackupRepositoryStatus.Scope`" + ` is ` + "`Cluster;Namespaced`" + `; the label is neither of those spellings. One vocabulary across every family that carries it, matching ` + "`origin`" + `. |
| ` + "`origin`" + ` | ` + "`cluster`" + ` or ` + "`namespace`" + ` | Which plane produced the object: a cluster-DR fan-out, or the user plane. |
| ` + "`location`" + ` | a location name | The repository the data went to. |
| ` + "`schedule`" + ` | a schedule name, or empty | A CR name, so a ` + "`BackupSchedule`" + ` and a ` + "`ClusterBackupSchedule`" + ` may legitimately share one — they are told apart by ` + "`origin`" + `, not by this. Empty for a one-off Backup. |
| ` + "`result`" + ` | ` + "`completed`" + `, ` + "`partiallycompleted`" + `, ` + "`partiallyfailed`" + `, ` + "`failed`" + ` | A terminal phase, lowercased. The only label whose value is an outcome rather than an identity. |
| ` + "`mode`" + ` | ` + "`Recreate`" + `, ` + "`Overwrite`" + `, or empty | A Restore's ` + "`spec.mode`" + `, in the API's own casing — unlike ` + "`scope`" + `. Empty on an object written before the field was required. |
| ` + "`webhook`" + `, ` + "`reason`" + ` | code-chosen constants | Never request-derived. A reason taken from user input would be an unbounded-cardinality hole. |
| ` + "`exposer`" + ` | ` + "`csi-generic`" + `, ` + "`cephfs-shallow`" + ` | The SnapshotExposer kind. Comparing the two is the point of the histogram. |
| ` + "`pvc`" + ` | a PVC name | The single per-PVC label in the catalogue. See [Snapshot exposure and coexistence](#snapshot-exposure-and-coexistence). |

Label *values* are object names and bounded enums. No user-supplied free text becomes a label
value anywhere in this catalogue.

### Reaching the endpoint

Port **8443**, HTTPS, with API-server authentication and authorisation. See
[Observability](/CrystalBackup/docs/guides/observability/) for the ServiceMonitor, the
` + "`crystal-backup-metrics-reader`" + ` ClusterRole, and the NetworkPolicy knob.

`

const metricsOutro = `## What these metrics are not for

The per-tenant series exist so a platform team can see who is generating what. The operator does
no accounting and no billing with them, and there is **no quota mechanism anywhere in the tool** —
a namespace generating far more data than anyone expected is visible, not bounded.

Deduplicated bytes per tenant are best-effort. restic deduplicates at the *repository* level, so
attributing the saving to one namespace is an estimate by construction; the exact number is the
repository total in ` + "`crystalbackup_repository_size_bytes`" + `.

And none of this tells you a restore works. ` + "`restic check`" + ` verifies that a repository is
readable, which is not the same claim as an application coming back up. Restore drills are the
administrator's job, on a real cadence — see
[Maintenance and verification](/CrystalBackup/docs/guides/maintenance/#restore-drills-are-yours).

## See also

- [Alerts](/CrystalBackup/docs/reference/alerts/) — the eleven rules built on these series.
- [Observability](/CrystalBackup/docs/guides/observability/) — scraping, logs, and the conditions
  that say *why*.
`

// frontMatter writes the page's YAML header.
//
// Both scalars are double-quoted, always. An unquoted YAML scalar containing `: ` is a mapping,
// not a string, and the site build fails on it — which is how the first generated Alerts page
// broke, on a description that simply had a colon in it. strconv.Quote's escaping is a subset of
// YAML's double-quoted style, so this is safe for anything a title or description can hold.
func frontMatter(title, description string) string {
	return fmt.Sprintf("---\ntitle: %s\ndescription: %s\ntableOfContents:\n  minHeadingLevel: 2\n  maxHeadingLevel: 3\n---\n\n",
		strconv.Quote(title), strconv.Quote(description))
}
