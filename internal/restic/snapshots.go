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
	"encoding/json"
	"fmt"
	"time"
)

// Snapshot mirrors one entry of `restic snapshots --json`. Only the fields the operator
// actually consumes are decoded; restic emits more (tree, username, program_version, …)
// and unknown fields are ignored, so a newer restic that adds fields still parses.
//
// The struct tags map restic's JSON names — note "hostname" carries the restic --host we
// wrote as the clusterID (Identity.Host), and "short_id" is restic's abbreviated ID used in
// human-facing CLI output. Paths and Tags are the identity written by the constructors in
// restic.go; TagValue reads scope (namespace=, run=, …) back off Tags.
type Snapshot struct {
	// ID is the full restic snapshot ID (64 hex chars); the stable handle for restore.
	ID string `json:"id"`
	// ShortID is restic's abbreviated ID (first 8 hex chars) for display.
	ShortID string `json:"short_id"`
	// Time is when the snapshot was taken (restic emits RFC3339 with sub-second precision).
	Time time.Time `json:"time"`
	// Host is the restic --host, i.e. the clusterID this snapshot's Identity carried.
	Host string `json:"hostname"`
	// Paths are the backed-up paths; for CrystalBackup exactly one Identity.Path.
	Paths []string `json:"paths"`
	// Tags are the restic tags; the crystalbackup marker plus the identity tags.
	Tags []string `json:"tags"`
	// Summary is restic's per-snapshot statistics block (emitted since restic 0.17; the
	// mover pins ≥0.19.1). Optional and best-effort: an older repository's snapshots may
	// lack it, so consumers (the restore capacity fallback) treat a nil Summary as
	// "size unknown".
	Summary *SnapshotSummary `json:"summary,omitempty"`
}

// SnapshotSummary is the subset of a snapshot's statistics the operator consumes.
type SnapshotSummary struct {
	// TotalBytesProcessed is the logical size of everything the snapshot backed up — the
	// sizing input of the restore capacity fallback (adr/0016) when PVC-meta tags are absent.
	TotalBytesProcessed int64 `json:"total_bytes_processed"`
}

// ParseSnapshots decodes the JSON array printed by `restic snapshots --json` into Snapshots.
// It is a thin, total wrapper over encoding/json: an empty/`null` output yields a nil slice
// and no error (restic prints an empty array when there are no snapshots), and a malformed
// document returns a wrapped error naming this package for the caller's logs. It performs no
// filtering — callers pass `--tag crystalbackup` to restic, and GroupByNamespaceRun does the
// bucketing.
func ParseSnapshots(data []byte) ([]Snapshot, error) {
	var snaps []Snapshot
	if err := json.Unmarshal(data, &snaps); err != nil {
		return nil, fmt.Errorf("restic: parsing snapshots JSON: %w", err)
	}
	return snaps, nil
}

// NamespaceRun is the (namespace, run) discovery key of spec/02-api.md §Discovery: the
// controller groups `restic snapshots --tag crystalbackup` by this pair and ensures one
// Backup object named <run> per existing namespace. Both fields come from the snapshot's own
// tags (namespace=, run=) via TagValue, so grouping never trusts a path or a filename.
//
// It is a plain comparable struct so it can be a map key. A cluster-manifests snapshot has
// no namespace= tag, so it groups under Namespace == "" — deliberately distinct from any
// real namespace and skipped by namespace projection, since cluster-scoped objects are
// restored only by an admin ClusterRestore.
type NamespaceRun struct {
	Namespace string
	Run       string
}

// GroupByNamespaceRun buckets snapshots by their (namespace=, run=) tags — the primitive
// the discovery controller builds Backup projections from. The returned map preserves, for
// each key, the input order of the snapshots that fell into it (a namespace's data +
// manifests snapshots for one run land in the same bucket in the order restic listed them).
// A snapshot missing the namespace= or run= tag contributes an empty string for that half
// of the key rather than being dropped, so nothing is silently lost; the caller decides what
// to do with a "" namespace (skip projection) or "" run.
func GroupByNamespaceRun(snaps []Snapshot) map[NamespaceRun][]Snapshot {
	groups := make(map[NamespaceRun][]Snapshot)
	for _, s := range snaps {
		ns, _ := TagValue(s.Tags, TagKeyNamespace)
		run, _ := TagValue(s.Tags, TagKeyRun)
		key := NamespaceRun{Namespace: ns, Run: run}
		groups[key] = append(groups[key], s)
	}
	return groups
}
