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
}

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
