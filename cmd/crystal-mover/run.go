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

package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/CrystalBackup/CrystalBackup/internal/mover"
)

const (
	// maxErrorBytes caps the length of MoverResult.Error. The whole termination message is
	// bounded by the kubelet at 4096 bytes and must remain valid JSON with the other fields, so
	// the free-text reason is kept well under that; a restic failure reason is a short sentence,
	// never a transcript.
	maxErrorBytes = 512

	// maxStderrTailBytes is how many trailing bytes of restic's stderr the shim retains to
	// extract the fatal final line from. It is generous enough to keep restic's multi-line
	// "Fatal:" epilogue yet bounded so a chatty `restic check` on a corrupt repository cannot
	// grow the buffer without limit.
	maxStderrTailBytes = 4096
)

// knownOperation reports whether op is one of the five mover operations. It is written as an
// exhaustive switch over the mover.Op* constants (not a map) so adding a sixth operation is a
// compile-time prompt to decide how the shim should treat it, rather than a silent default.
func knownOperation(op string) bool {
	switch mover.Operation(op) {
	case mover.OpBackup, mover.OpInit, mover.OpForget, mover.OpPrune, mover.OpCheck,
		mover.OpSnapshots, mover.OpUnlock:
		return true
	default:
		return false
	}
}

// ensureBackupJSON guarantees `restic backup` runs with --json, so the shim can parse a
// machine-readable summary (snapshot id, sizes) off stdout. It is:
//   - a no-op for every non-backup operation — their outcome is just the exit code, and forcing
//     --json there would only add noise to the pod log; and
//   - a no-op when the caller already supplied --json, so the flag is never passed twice.
//
// The append uses a full-slice expression (cap == len) so it always allocates a fresh backing
// array and can never mutate the caller's argv in place — the caller keeps ownership of the
// slice it passed in. --json is restic's global (persistent) flag, recognised anywhere after the
// subcommand, so appending it at the end is safe regardless of the other args.
func ensureBackupJSON(operation string, resticArgv []string) []string {
	if operation != string(mover.OpBackup) {
		return resticArgv
	}
	for _, a := range resticArgv {
		if a == "--json" {
			return resticArgv
		}
	}
	return append(resticArgv[:len(resticArgv):len(resticArgv)], "--json")
}

// buildResult is the shim's decision logic, isolated from the exec so it is unit-testable
// without a live restic. It maps (operation, restic's captured stdout, restic's run error) to
// the MoverResult the controller reads back off the termination message.
//
// resticErr is examined FIRST and dominates: a non-zero restic exit is a failure for EVERY
// operation, backup included. A backup that ultimately failed may still have flushed a partial
// `--json` summary to stdout, and trusting that would report a snapshot the repository does not
// actually hold — so only a clean (nil-error) run is ever eligible for success.
//
// On a clean backup a parseable summary is REQUIRED. `restic backup` can exit 0 having merely
// skipped unreadable files, but with no summary object there is no snapshot id to record, so
// "exit 0, no summary" is deliberately a failure rather than a success with an empty id — the
// caller must be able to tell "backup finished" from "backup produced nothing". Every other
// operation carries no payload, so a clean exit is simply OK with just the operation echoed.
func buildResult(operation string, resticStdout []byte, resticErr error) mover.MoverResult {
	if resticErr != nil {
		return mover.MoverResult{
			OK:        false,
			Operation: operation,
			Error:     clampError(resticErr),
		}
	}
	if operation == string(mover.OpBackup) {
		summary, err := mover.ParseBackupSummary(resticStdout)
		if err != nil {
			// restic returned success but we could not read a summary: refuse to fabricate a
			// snapshot identity from a truncated/empty stream.
			return mover.MoverResult{
				OK:        false,
				Operation: operation,
				Error:     clampError(err),
			}
		}
		return mover.SummaryToResult(summary)
	}
	return mover.MoverResult{OK: true, Operation: operation}
}

// writeResult marshals result and writes it to path, creating or TRUNCATING it. In the pod, path
// is mover.TerminationMessagePath (/dev/termination-log) — one of the only writable paths under
// the mover's read-only root filesystem — and the bytes written there become the container's
// termination message, the controller's sole channel for the snapshot id and size. Truncation
// matters: a shorter second result must fully replace a longer first one, never leave stale
// trailing bytes that would corrupt the JSON the controller parses. Tests point path at a temp
// file to round-trip the encoding.
func writeResult(path string, result mover.MoverResult) error {
	encoded, err := result.Encode()
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(encoded), 0o644); err != nil {
		return fmt.Errorf("write termination message to %q: %w", path, err)
	}
	return nil
}

// clampError normalises an error into a compact, single-line, length-bounded reason safe to
// store in MoverResult.Error (and thus in the 4096-byte termination message and the object's
// status). It collapses all interior whitespace/newlines to single spaces — so a multi-line
// restic error can neither smuggle a fake boundary into the message nor bloat it — and truncates
// to maxErrorBytes. restic itself never echoes the repository password (read from a file) or the
// AWS secret key on stderr, so the folded-in stderr tail carries no credential and no further
// redaction is required here.
func clampError(err error) string {
	msg := strings.Join(strings.Fields(err.Error()), " ")
	if msg == "" {
		msg = "restic exited with an unspecified error"
	}
	if len(msg) > maxErrorBytes {
		msg = msg[:maxErrorBytes]
	}
	return msg
}

// lastLine returns the last non-empty, whitespace-trimmed line of b, or "" when there is none.
// restic prints its fatal reason as the final line of stderr, so this is what the shim quotes
// into a failure MoverResult to make the controller's status specific.
func lastLine(b []byte) string {
	lines := strings.Split(string(b), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if s := strings.TrimSpace(lines[i]); s != "" {
			return s
		}
	}
	return ""
}

// tailWriter retains only the last max bytes written through it and discards the rest. The shim
// streams restic's stderr straight to its own stderr (so progress and warnings show up in the
// pod log) but also needs restic's FINAL line to explain a failure; wrapping a tailWriter in an
// io.MultiWriter captures that tail without buffering an entire `restic check` transcript in
// memory. Write never short-writes — it always reports the full input length — so it is a
// well-behaved io.MultiWriter sink that never aborts the stream to os.Stderr.
type tailWriter struct {
	max int
	buf []byte
}

// Write appends p and keeps only the trailing max bytes. It always reports len(p) written.
func (w *tailWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	if len(w.buf) > w.max {
		// Keep the tail (restic's fatal line lives at the end), dropping the older head. The old
		// backing array is reclaimed on the next growth-triggering append.
		w.buf = w.buf[len(w.buf)-w.max:]
	}
	return len(p), nil
}

// Bytes returns the retained tail (the last <=max bytes of the stream), from which lastLine
// extracts restic's final message.
func (w *tailWriter) Bytes() []byte { return w.buf }
