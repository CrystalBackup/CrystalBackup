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

package mover

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// MoverResult is the payload the shim writes to the container's termination message and
// the controller reads back. It is deliberately tiny: the kubelet caps a termination
// message at 4096 bytes, and this is the ONLY channel the controller uses to learn a
// snapshot's identity and size (it does not scrape the pod logs). `omitempty` on every
// field but OK keeps a maintenance result down to `{"ok":true}` and a failure to
// `{"ok":false,"error":"..."}`.
type MoverResult struct {
	// OK is the single source of truth for success. It has no omitempty so that `ok:false`
	// is always emitted explicitly — a result must never be able to serialise to something
	// that decodes back as a zero-value success by omission.
	OK bool `json:"ok"`
	// Operation echoes the Operation that ran (e.g. "backup"), for logging and to let a
	// controller sanity-check it got the result it expected.
	Operation string `json:"operation,omitempty"`
	// SnapshotID is the restic snapshot id a successful backup produced; empty for
	// maintenance operations and for failures.
	SnapshotID string `json:"snapshotID,omitempty"`
	// SizeBytes is the total logical bytes the backup processed (restic
	// total_bytes_processed) — the snapshot's apparent size, not its incremental cost.
	SizeBytes int64 `json:"sizeBytes,omitempty"`
	// AddedBytes is the incremental bytes this backup actually wrote to the repository
	// (restic data_added) — near zero for an unchanged PVC, the real storage cost.
	AddedBytes int64 `json:"addedBytes,omitempty"`
	// RestoredBytes is the bytes a successful restore actually wrote into the target PVC
	// (restic restore's bytes_restored); zero for every other operation.
	RestoredBytes int64 `json:"restoredBytes,omitempty"`
	// ResourceCount is how many objects a manifest backup actually captured; zero for every
	// other operation. It feeds Backup.status.manifests.resourceCount.
	ResourceCount int32 `json:"resourceCount,omitempty"`
	// IncompleteManifests is true when a manifest dump could not enumerate everything — an
	// RBAC 403 on one kind, an aggregated API that was down. The dump deliberately continues
	// past those, so without this flag a partial capture would be indistinguishable from a
	// complete one, and a restore would silently be missing kinds nobody knew about. The
	// controller turns it into ManifestsComplete=False; the detail lives in the snapshot's
	// index.json, which is too large for the 4096-byte termination message.
	IncompleteManifests bool `json:"incompleteManifests,omitempty"`
	// RestoredResources is how many manifests a manifest restore applied; zero for every other
	// operation. It feeds Restore.status.restoredResources.
	RestoredResources int32 `json:"restoredResources,omitempty"`
	// FailedResources is how many manifests could not be applied. A manifest restore reports
	// per-resource failures and CONTINUES (adr/0007), so this being non-zero on an OK result is
	// the normal shape of a partial restore, not a contradiction.
	FailedResources int32 `json:"failedResources,omitempty"`
	// SkippedResources is how many manifests the selection excluded, so "applied 3" is
	// distinguishable from "the snapshot only had 3".
	SkippedResources int32 `json:"skippedResources,omitempty"`
	// ResourceEntries are the non-trivial per-resource outcomes, already trimmed to fit the
	// termination message (see Fit).
	ResourceEntries []ResourceEntry `json:"resourceEntries,omitempty"`
	// ResourcesTruncated is true when entries were dropped to fit, so a reader can tell an
	// empty tail from a complete report.
	ResourcesTruncated bool `json:"resourcesTruncated,omitempty"`
	// Error is a human-readable failure reason, set only when OK is false. It is advisory
	// (for status/events); control flow keys off OK, not off this string.
	Error string `json:"error,omitempty"`

	// ------------------------------------------------------------------------------------
	// What this mover cost, measured by the mover itself.
	//
	// WHY IT IS HERE AND NOT IN A METRIC. Peak mover memory cannot be obtained by polling.
	// metrics-server exposes a pod only after its own scrape has covered it, on its own ~15s
	// cadence, and a mover lives seconds: measured on a real cluster, 63 mover pods with
	// lifetimes from 0.9s to 17s produced ZERO samples each, with metrics-server healthy and
	// answering for long-lived pods throughout. Sampling faster changes nothing — the data is
	// not there to be sampled. The process that CAN answer is the one that ran, from its own
	// kernel accounting, and the termination message is a channel it already writes to.
	//
	// Every field below is omitted when it could not be read. Absence is "not measured",
	// never zero — see MemorySource, which is the field that says which readers succeeded.
	// ------------------------------------------------------------------------------------

	// PeakRSSBytes is the high-water RESIDENT SET SIZE of the restic process, from the kernel's
	// own per-process accounting (wait4's ru_maxrss, exact rather than sampled).
	//
	// THIS IS THE SIZING NUMBER. RSS is anonymous memory plus mapped file pages; it does NOT
	// include the page cache a backup streams through, which is what makes it — and not the
	// cgroup peak below — the figure that predicts an OOM kill. The kernel reclaims page cache
	// before it kills anything, so what a memory limit must cover is the part that cannot be
	// reclaimed, and that is essentially this.
	//
	// It is restic's alone. The shim's own footprint is ShimPeakRSSBytes and the two are
	// resident AT THE SAME TIME (the shim waits while restic runs), so a limit has to cover
	// their sum.
	PeakRSSBytes int64 `json:"peakRSSBytes,omitempty"`
	// ShimPeakRSSBytes is the same measurement for the crystal-mover process itself, reported
	// separately so nobody has to wonder whether PeakRSSBytes includes it. It is small for a
	// data or maintenance operation and is NOT small for a manifest capture, which holds a
	// namespace's objects in this process before restic ever starts.
	ShimPeakRSSBytes int64 `json:"shimPeakRSSBytes,omitempty"`
	// CgroupPeakBytes is the peak of the container cgroup's memory.current (cgroup v2
	// memory.peak): anonymous memory PLUS page cache plus kernel memory, for the whole
	// container.
	//
	// IT IS NOT A SIZING TARGET, and it can exceed PeakRSSBytes by an order of magnitude for
	// exactly the reason this operator exists: a backup streams a volume through the page cache,
	// every one of those pages is charged to this cgroup, and the cgroup is free to keep them
	// right up to its limit because they are RECLAIMABLE. Sizing a limit to this number raises a
	// ceiling that was never the constraint.
	//
	// NOR IS IT AN UPPER BOUND ON PeakRSSBytes, which is what this comment used to claim. The two
	// count different populations: memory.peak counts what was CHARGED to this cgroup, and RSS
	// counts what the process had MAPPED. A file page is charged to whichever cgroup first
	// faulted it in, so a mover whose image pages were already resident — brought in by an
	// earlier mover on the same node — maps them without being charged for them.
	//
	// Measured on the crucible, and it is the common case rather than a corner: across eight data
	// movers, memory.peak sat 20-22Mi BELOW restic's ru_maxrss on every single one (75Mi RSS
	// against 53Mi charged), the gap closing to ~3Mi on manifest movers where anonymous memory —
	// which is always charged — dominates. Neither figure bounds the other in general. Report
	// both, name what each counts, and never present one as the ceiling of the other.
	CgroupPeakBytes int64 `json:"cgroupPeakBytes,omitempty"`
	// MemoryLimitHits is cgroup v2 memory.events `max`: how many times the cgroup reached its
	// memory limit and the kernel had to reclaim to stay under it.
	//
	// This is what turns the pair above into an answer. Zero means the limit was never pressed
	// at all, so a high CgroupPeakBytes is cache the cgroup was merely ALLOWED to keep. Non-zero
	// with MemoryOOMKills at zero means the limit WAS reached and reclaim was enough — the
	// workload is running at its ceiling and paying for it in I/O.
	MemoryLimitHits int64 `json:"memoryLimitHits,omitempty"`
	// MemoryOOMKills is cgroup v2 memory.events `oom_kill`: processes the cgroup OOM killer
	// killed. It is not redundant with the kubelet's OOMKilled container reason: when restic is
	// the one killed and this shim survives to report, the container exits 1 and Kubernetes
	// records no OOM anywhere. This is then the only trace.
	MemoryOOMKills int64 `json:"memoryOOMKills,omitempty"`
	// MemorySource names which readers succeeded, so a reader never has to infer measurement
	// from a zero: "rusage" (the two RSS peaks), "cgroup2" (the cgroup peak and the two
	// counters), "rusage+cgroup2" for both, and ABSENT when nothing could be read at all.
	//
	// The counters in particular are only meaningful when this names cgroup2: without it,
	// MemoryLimitHits and MemoryOOMKills are absent because nothing looked, not because
	// nothing happened.
	MemorySource string `json:"memorySource,omitempty"`
}

// The MemorySource values. Named here rather than spelled at each end, because the shim writes
// them and internal/soak reads them: a misspelling would not fail, it would silently file a
// measured peak as unmeasured provenance.
const (
	// MemorySourceRusage — the two RSS peaks came from the kernel's per-process accounting.
	MemorySourceRusage = "rusage"
	// MemorySourceCgroup2 — the cgroup peak and the two memory.events counters came from the
	// container's own cgroup v2 directory.
	MemorySourceCgroup2 = "cgroup2"
	// MemorySourceBoth — both readers succeeded, which is the normal case on a cgroup v2 node.
	MemorySourceBoth = MemorySourceRusage + "+" + MemorySourceCgroup2
)

// ResourceEntry is one manifest's outcome on the wire. It mirrors the API's
// RestoreResourceEntry but is declared here because this is the transport format, and the
// mover must not import the API types.
type ResourceEntry struct {
	Group   string   `json:"g,omitempty"`
	Kind    string   `json:"k,omitempty"`
	Name    string   `json:"n,omitempty"`
	Outcome string   `json:"o,omitempty"`
	Reason  string   `json:"r,omitempty"`
	Changed []string `json:"c,omitempty"`
}

// TerminationMessageLimit is the kubelet's cap on a termination message. Past it the message
// is TRUNCATED, not rejected — which for a JSON payload means the controller reads a string
// that fails to parse and reports "the mover never reported" for a run that in fact succeeded.
// Everything below exists to make sure that cannot happen.
const TerminationMessageLimit = 4096

// resultSizeMargin leaves room under the cap. The exact encoded length depends on characters
// this code does not control (a server error string, a resource name), so the budget is met
// with slack rather than to the byte.
const resultSizeMargin = 256

// maxReasonLength bounds one server error before it is ever considered for the budget. A
// webhook rejection can run to hundreds of characters and a single verbose one would otherwise
// evict every other entry.
const maxReasonLength = 180

// Fit trims the per-resource report until the encoded result fits the termination message.
//
// The counts (RestoredResources, FailedResources) are NEVER dropped: they are the accounting a
// controller reports, and they are a handful of bytes. Only entries go, and failures go LAST —
// a user with a partial restore needs the failures far more than the list of objects that were
// merely updated. Whatever is dropped is recorded in the pod log by the caller and flagged by
// ResourcesTruncated, so a truncated report never passes as a complete one.
func (r MoverResult) Fit() MoverResult {
	for i := range r.ResourceEntries {
		if len(r.ResourceEntries[i].Reason) > maxReasonLength {
			r.ResourceEntries[i].Reason = r.ResourceEntries[i].Reason[:maxReasonLength] + "…"
		}
	}
	if r.encodedLen() <= TerminationMessageLimit-resultSizeMargin {
		return r
	}

	// Failures last: sort them to the front and drop from the tail.
	ordered := make([]ResourceEntry, 0, len(r.ResourceEntries))
	for _, e := range r.ResourceEntries {
		if e.Outcome == OutcomeFailedWire {
			ordered = append(ordered, e)
		}
	}
	for _, e := range r.ResourceEntries {
		if e.Outcome != OutcomeFailedWire {
			ordered = append(ordered, e)
		}
	}
	r.ResourceEntries = ordered

	for len(r.ResourceEntries) > 0 && r.encodedLen() > TerminationMessageLimit-resultSizeMargin {
		r.ResourceEntries = r.ResourceEntries[:len(r.ResourceEntries)-1]
		r.ResourcesTruncated = true
	}
	return r
}

// OutcomeFailedWire is the Failed outcome as it travels on the wire. Declared here so Fit can
// prioritise failures without importing the package that produces them.
const OutcomeFailedWire = "Failed"

func (r MoverResult) encodedLen() int {
	encoded, err := r.Encode()
	if err != nil {
		// A MoverResult cannot fail to marshal (only scalars, strings and slices of them). If
		// it somehow did, claim the budget is blown so the caller trims rather than emits
		// something oversized.
		return TerminationMessageLimit + 1
	}
	return len(encoded)
}

// Encode marshals the result to the compact JSON string the shim writes to
// TerminationMessagePath. json.Marshal produces no trailing newline and no indentation,
// which keeps the message well under the 4096-byte kubelet cap. The error is part of the
// signature for symmetry with the decode side, though a MoverResult (only scalars and
// strings) cannot actually fail to marshal.
func (r MoverResult) Encode() (string, error) {
	b, err := json.Marshal(r)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// ParseMoverResult decodes the container's termination message. A blank or whitespace-only
// message is treated as a hard error, NOT as an empty success: the kubelet leaves the
// termination message empty when the container is killed before it can write (OOMKilled,
// SIGKILL, node eviction), so an empty message means "the mover never reported" and MUST
// be surfaced as a failure. Any non-JSON content is likewise an error — the only success
// path is a well-formed MoverResult the shim actually wrote.
func ParseMoverResult(msg string) (MoverResult, error) {
	if strings.TrimSpace(msg) == "" {
		return MoverResult{}, errors.New(
			"empty mover termination message: the container terminated without writing a result " +
				"(e.g. OOMKilled, SIGKILL or eviction); treat as failure")
	}
	var r MoverResult
	if err := json.Unmarshal([]byte(msg), &r); err != nil {
		return MoverResult{}, fmt.Errorf("decode mover termination message %q: %w", msg, err)
	}
	return r, nil
}

// messageTypeSummary is the message_type value restic stamps on the final object of a
// `backup --json` stream. Every other line (progress "status" objects, the occasional
// non-JSON warning) is ignored by ParseBackupSummary.
const messageTypeSummary = "summary"

// ResticBackupSummary is the subset of restic's `backup --json` summary object the mover
// cares about. restic emits many more fields (file/dir counts, blob counts, duration);
// unlisted fields are ignored by the JSON decoder. Kept in this package (not the shim) so
// the parsing is unit-tested here, decoupled from any live restic.
type ResticBackupSummary struct {
	// MessageType is "summary" on the object this parser wants; the discriminator used to
	// pick the final summary out of the mixed status/summary stream.
	MessageType string `json:"message_type"`
	// SnapshotID is the id of the snapshot restic just created.
	SnapshotID string `json:"snapshot_id"`
	// TotalBytesProcessed is the logical size of everything backed up -> MoverResult.SizeBytes.
	TotalBytesProcessed int64 `json:"total_bytes_processed"`
	// DataAdded is the incremental bytes written to the repository -> MoverResult.AddedBytes.
	DataAdded int64 `json:"data_added"`
}

// scanSummaryLines walks a restic --json stream line by line and hands each candidate
// object line to tryDecode; the caller's closure keeps the LAST line it could FULLY decode
// as its summary type (last-valid-wins is defensive: a summary-typed line that does not
// decode — injected chatter, a truncation artifact — is skipped, never fatal, exactly like
// any other noise). restic emits ONE JSON object per line: a run of "status" progress
// objects, then a single {"message_type":"summary",...} at the end. Returns whether any
// candidate decoded. Shared by the backup and restore summary parsers so the scanning rules
// can never diverge.
func scanSummaryLines(resticStdout []byte, tryDecode func(line []byte) bool) (found bool) {
	// bufio.Reader.ReadBytes has no line-length cap (unlike bufio.Scanner's default 64KiB),
	// so an unusually long status line — restic can list many current files — never
	// truncates the scan and hides a later summary.
	r := bufio.NewReader(bytes.NewReader(resticStdout))
	for {
		candidate, readErr := r.ReadBytes('\n')
		// Process the bytes read so far BEFORE acting on readErr: ReadBytes returns the final
		// line together with io.EOF when the stream does not end in a newline, so the summary
		// object (often the last line, sometimes unterminated) is still seen.
		if trimmed := bytes.TrimSpace(candidate); len(trimmed) > 0 && trimmed[0] == '{' {
			// Guard on a leading '{' so restic's non-JSON output is cheaply skipped; the
			// closure decides whether the line fully decodes as its summary shape.
			if tryDecode(trimmed) {
				found = true
			}
		}
		if readErr != nil {
			return found
		}
	}
}

// ParseBackupSummary extracts the final summary from a `restic backup --json` stream (see
// scanSummaryLines for the scanning rules). The total absence of a summary is an error,
// because that means the backup never reported success and the caller must not fabricate a
// snapshot id from a truncated stream.
func ParseBackupSummary(resticStdout []byte) (ResticBackupSummary, error) {
	var summary ResticBackupSummary
	found := scanSummaryLines(resticStdout, func(line []byte) bool {
		var candidate ResticBackupSummary
		if json.Unmarshal(line, &candidate) == nil && candidate.MessageType == messageTypeSummary {
			summary = candidate
			return true
		}
		return false
	})
	if !found {
		return ResticBackupSummary{}, fmt.Errorf(
			"no restic backup summary (message_type=%q) in %d bytes of --json output",
			messageTypeSummary, len(resticStdout))
	}
	return summary, nil
}

// SummaryToResult maps a parsed restic summary to the successful MoverResult the shim
// reports for a backup. It fixes the field translation in one place so producer and
// tests agree: total_bytes_processed -> SizeBytes (apparent size),
// data_added -> AddedBytes (incremental cost), and Operation is always "backup" (only a
// backup produces this summary shape).
// The operation is a parameter rather than a hardcoded OpBackup because more than one
// operation now ends in `restic backup` and therefore lands here: a manifest backup produces
// the same summary shape. Echoing "backup" for a manifests-backup would defeat the one thing
// MoverResult.Operation exists for — letting the controller check it got the result it asked
// for — and it would do so silently.
func SummaryToResult(op Operation, s ResticBackupSummary) MoverResult {
	return MoverResult{
		OK:         true,
		Operation:  string(op),
		SnapshotID: s.SnapshotID,
		SizeBytes:  s.TotalBytesProcessed,
		AddedBytes: s.DataAdded,
	}
}

// ResticRestoreSummary is the subset of restic's `restore --json` summary object the mover
// cares about (restic ≥ 0.17 emits it; the mover pins ≥ 0.19.1). Unlisted fields
// (files_skipped, seconds_elapsed, ...) are ignored by the JSON decoder.
type ResticRestoreSummary struct {
	// MessageType is "summary" on the object this parser wants.
	MessageType string `json:"message_type"`
	// TotalBytes is the logical size of the selected restore set.
	TotalBytes int64 `json:"total_bytes"`
	// BytesRestored is what was actually written to the target -> MoverResult.RestoredBytes.
	BytesRestored int64 `json:"bytes_restored"`
	// FilesRestored counts the files written; logged, not carried into CR status.
	FilesRestored int64 `json:"files_restored"`
}

// ParseRestoreSummary extracts the final summary from a `restic restore --json` stream (see
// scanSummaryLines for the scanning rules). Like the backup parser, the total absence of a
// summary is an error: restic always emits one on a clean restore, so a summary-less clean
// exit means the stream was truncated and the caller must not report an unverified success.
func ParseRestoreSummary(resticStdout []byte) (ResticRestoreSummary, error) {
	var summary ResticRestoreSummary
	found := scanSummaryLines(resticStdout, func(line []byte) bool {
		var candidate ResticRestoreSummary
		if json.Unmarshal(line, &candidate) == nil && candidate.MessageType == messageTypeSummary {
			summary = candidate
			return true
		}
		return false
	})
	if !found {
		return ResticRestoreSummary{}, fmt.Errorf(
			"no restic restore summary (message_type=%q) in %d bytes of --json output",
			messageTypeSummary, len(resticStdout))
	}
	return summary, nil
}

// RestoreSummaryToResult maps a parsed restore summary to the successful MoverResult the
// shim reports for a restore: bytes_restored -> RestoredBytes, total_bytes -> SizeBytes
// (the selected set's apparent size), no snapshot id (a restore creates none).
func RestoreSummaryToResult(s ResticRestoreSummary) MoverResult {
	return MoverResult{
		OK:            true,
		Operation:     string(OpRestore),
		SizeBytes:     s.TotalBytes,
		RestoredBytes: s.BytesRestored,
	}
}
