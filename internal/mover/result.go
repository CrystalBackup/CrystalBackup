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
	// Error is a human-readable failure reason, set only when OK is false. It is advisory
	// (for status/events); control flow keys off OK, not off this string.
	Error string `json:"error,omitempty"`
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

// ParseBackupSummary extracts the final summary from a `restic backup --json` stream.
// restic emits ONE JSON object per line: a run of "status" progress objects, then a
// single {"message_type":"summary",...} at the end. This scans every line and returns the
// LAST object whose message_type == "summary" (last-wins is defensive; there is normally
// exactly one). Blank lines and non-JSON chatter are skipped gracefully — only the total
// absence of a summary is an error, because that means the backup never reported success
// and the caller must not fabricate a snapshot id from a truncated stream.
func ParseBackupSummary(resticStdout []byte) (ResticBackupSummary, error) {
	var (
		summary ResticBackupSummary
		found   bool
	)
	// bufio.Reader.ReadBytes has no line-length cap (unlike bufio.Scanner's default 64KiB),
	// so an unusually long status line — restic can list many current files — never
	// truncates the scan and hides a later summary.
	r := bufio.NewReader(bytes.NewReader(resticStdout))
	for {
		line, readErr := r.ReadBytes('\n')
		// Process the bytes read so far BEFORE acting on readErr: ReadBytes returns the final
		// line together with io.EOF when the stream does not end in a newline, so the summary
		// object (often the last line, sometimes unterminated) is still seen.
		if trimmed := bytes.TrimSpace(line); len(trimmed) > 0 && trimmed[0] == '{' {
			// Guard on a leading '{' so restic's non-JSON output (progress text, warnings) is
			// cheaply skipped, and decode into a candidate: a line that starts with '{' but is
			// not a valid summary object is skipped too (Unmarshal error or wrong message_type)
			// rather than aborting the whole scan.
			var candidate ResticBackupSummary
			if json.Unmarshal(trimmed, &candidate) == nil && candidate.MessageType == messageTypeSummary {
				summary, found = candidate, true
			}
		}
		if readErr != nil {
			break
		}
	}
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
// backup produces a summary).
func SummaryToResult(s ResticBackupSummary) MoverResult {
	return MoverResult{
		OK:         true,
		Operation:  string(OpBackup),
		SnapshotID: s.SnapshotID,
		SizeBytes:  s.TotalBytesProcessed,
		AddedBytes: s.DataAdded,
	}
}
