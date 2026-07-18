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
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/CrystalBackup/CrystalBackup/internal/mover"
)

// sampleBackupSummaryStream is a trimmed but faithful `restic backup --json` stream: a status
// progress object, then the final summary. Field names match restic's real output so buildResult
// exercises the same parse path the production shim does.
const sampleBackupSummaryStream = `{"message_type":"status","percent_done":0.5,"total_files":3,"total_bytes":2048}
{"message_type":"summary","files_new":3,"files_changed":0,"data_blobs":5,"tree_blobs":1,"data_added":1536,"total_files_processed":3,"total_bytes_processed":2048,"total_duration":0.12,"snapshot_id":"abc123def456"}
`

// TestBuildResultBackupSuccess pins the happy path: a clean backup whose stdout carries a summary
// yields OK with the snapshot id and the two sizes mapped through
// (total_bytes_processed -> SizeBytes, data_added -> AddedBytes).
func TestBuildResultBackupSuccess(t *testing.T) {
	got := buildResult(string(mover.OpBackup), []byte(sampleBackupSummaryStream), nil)
	want := mover.MoverResult{
		OK:         true,
		Operation:  "backup",
		SnapshotID: "abc123def456",
		SizeBytes:  2048,
		AddedBytes: 1536,
	}
	if got != want {
		t.Fatalf("buildResult(backup, summary, nil) = %+v, want %+v", got, want)
	}
}

// TestBuildResultBackupExitZeroNoSummary is the load-bearing "success but empty" case: restic
// exited 0 (nil error) yet emitted no summary object. There is no snapshot id to record, so the
// shim MUST report failure rather than a success with an empty id.
func TestBuildResultBackupExitZeroNoSummary(t *testing.T) {
	statusOnly := []byte(`{"message_type":"status","percent_done":0.9}` + "\n")
	got := buildResult(string(mover.OpBackup), statusOnly, nil)
	if got.OK {
		t.Fatalf("buildResult(backup, status-only, nil) = %+v, want OK==false", got)
	}
	if got.Operation != "backup" {
		t.Errorf("Operation = %q, want backup", got.Operation)
	}
	if got.Error == "" {
		t.Error("want a non-empty Error explaining the missing summary")
	}
	if got.SnapshotID != "" {
		t.Errorf("failed backup must not report a SnapshotID, got %q", got.SnapshotID)
	}
}

// TestBuildResultBackupExitError proves a non-zero restic exit dominates even when stdout happens
// to contain a summary: a backup that failed must never be reported as a snapshot the repository
// does not hold. It also checks the reported Error is present, single-lined, and free of any
// planted secret sentinel (the shim only ever stores restic's own credential-free message).
func TestBuildResultBackupExitError(t *testing.T) {
	// A multi-line error stands in for "exit code + folded-in stderr tail"; clampError must
	// collapse it to one line.
	runErr := errors.New("exit status 1\nFatal: repository is already locked")
	got := buildResult(string(mover.OpBackup), []byte(sampleBackupSummaryStream), runErr)
	if got.OK {
		t.Fatalf("buildResult(backup, summary, exitErr) = %+v, want OK==false", got)
	}
	if got.Operation != "backup" {
		t.Errorf("Operation = %q, want backup", got.Operation)
	}
	if got.Error == "" {
		t.Fatal("want a non-empty Error on exit failure")
	}
	if strings.Contains(got.Error, "\n") {
		t.Errorf("Error must be single-line, got %q", got.Error)
	}
	if got.SnapshotID != "" {
		t.Errorf("failed backup must not report a SnapshotID, got %q", got.SnapshotID)
	}
	// The shim never puts credentials in Error; a sentinel that was never in the error text must
	// not appear (documents intent — buildResult only echoes restic's message).
	const secretSentinel = "AKIAEXAMPLESECRET42"
	if strings.Contains(got.Error, secretSentinel) {
		t.Errorf("Error unexpectedly contains a secret sentinel: %q", got.Error)
	}
}

// TestBuildResultInitSuccess covers a maintenance op: a clean init is OK with the operation
// echoed and no snapshot payload.
func TestBuildResultInitSuccess(t *testing.T) {
	got := buildResult(string(mover.OpInit), nil, nil)
	if !got.OK || got.Operation != "init" {
		t.Fatalf("buildResult(init, nil, nil) = %+v, want OK==true Operation==init", got)
	}
	if got.SnapshotID != "" || got.SizeBytes != 0 || got.AddedBytes != 0 {
		t.Errorf("maintenance success must be payload-free, got %+v", got)
	}
}

// TestBuildResultInitAlreadyInitialized proves repository init is idempotent: restic exiting
// non-zero because the shared repository already carries a master key + config is a SUCCESS for
// the init op (the repo pre-exists — adr/0009), so the BackupRepository converges to Initialized
// instead of looping. The exact message mirrors restic's own epilogue.
func TestBuildResultInitAlreadyInitialized(t *testing.T) {
	resticErr := errors.New("exit status 1: Fatal: create key in repository at s3:... failed: " +
		"repository master key and config already initialized")
	got := buildResult(string(mover.OpInit), nil, resticErr)
	if !got.OK || got.Operation != "init" {
		t.Fatalf("buildResult(init, already-initialized) = %+v, want OK==true Operation==init", got)
	}
	if got.Error != "" {
		t.Errorf("an idempotent init success must carry no Error, got %q", got.Error)
	}
}

// TestBuildResultInitOtherErrorStillFails guards the scope of the idempotency: an init that failed
// for any OTHER reason (here, no S3 reachability) is still a hard failure — only the
// "already initialized" sentinel is swallowed.
func TestBuildResultInitOtherErrorStillFails(t *testing.T) {
	got := buildResult(string(mover.OpInit), nil,
		errors.New("exit status 1: Fatal: unable to open config file: Stat: RequestError: send request failed"))
	if got.OK {
		t.Fatalf("buildResult(init, s3-error) = %+v, want OK==false (only 'already initialized' is benign)", got)
	}
	if got.Error == "" {
		t.Error("want a non-empty Error on a genuine init failure")
	}
}

// TestBuildResultBackupAlreadyInitializedStillFails proves the idempotency is init-only: the same
// sentinel on a non-init op is NOT swallowed (a backup can never legitimately emit it, and must
// never be reported as a phantom success).
func TestBuildResultBackupAlreadyInitializedStillFails(t *testing.T) {
	got := buildResult(string(mover.OpBackup), nil, errors.New("already initialized"))
	if got.OK {
		t.Fatalf("buildResult(backup, 'already initialized') = %+v, want OK==false (init-only idempotency)", got)
	}
}

// TestBuildResultPruneExitError covers a maintenance op that failed: a non-zero prune is OK==false
// with the operation echoed and an Error set.
func TestBuildResultPruneExitError(t *testing.T) {
	got := buildResult(string(mover.OpPrune), nil, errors.New("exit status 2"))
	if got.OK {
		t.Fatalf("buildResult(prune, nil, exitErr) = %+v, want OK==false", got)
	}
	if got.Operation != "prune" {
		t.Errorf("Operation = %q, want prune", got.Operation)
	}
	if got.Error == "" {
		t.Error("want a non-empty Error on prune failure")
	}
}

// TestWriteResultRoundTrip proves writeResult and mover.ParseMoverResult are inverses across a
// real file, and that writeResult TRUNCATES: a shorter second result fully replaces a longer
// first one, leaving no stale trailing bytes that would corrupt the parsed JSON.
func TestWriteResultRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "termination-log")

	first := mover.MoverResult{
		OK:         true,
		Operation:  "backup",
		SnapshotID: "abc123def456",
		SizeBytes:  2048,
		AddedBytes: 1536,
	}
	if err := writeResult(path, first); err != nil {
		t.Fatalf("writeResult(first) error: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back first: %v", err)
	}
	got, err := mover.ParseMoverResult(string(raw))
	if err != nil {
		t.Fatalf("ParseMoverResult(first) error: %v", err)
	}
	if !reflect.DeepEqual(got, first) {
		t.Errorf("round-trip = %+v, want %+v (file %q)", got, first, raw)
	}

	second := mover.MoverResult{OK: false, Operation: "prune", Error: "boom"}
	if err := writeResult(path, second); err != nil {
		t.Fatalf("writeResult(second) error: %v", err)
	}
	raw2, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back second: %v", err)
	}
	got2, err := mover.ParseMoverResult(string(raw2))
	if err != nil {
		t.Fatalf("ParseMoverResult(second) error: %v (raw %q)", err, raw2)
	}
	if !reflect.DeepEqual(got2, second) {
		t.Errorf("after overwrite = %+v, want %+v (file %q)", got2, second, raw2)
	}
}

// TestEnsureBackupJSON pins the three behaviours the shim relies on: add --json for a backup that
// lacks it, never duplicate it, and never add it for a non-backup op. It also asserts the helper
// does not mutate the caller's backing array in place.
func TestEnsureBackupJSON(t *testing.T) {
	// adds --json when a backup lacks it
	if got := ensureBackupJSON(string(mover.OpBackup), []string{"backup", "/data/ns/pvc"}); countArg(got, "--json") != 1 {
		t.Errorf("ensureBackupJSON(backup without --json) = %v, want exactly one --json", got)
	}

	// leaves a backup that already has --json unchanged (no duplicate)
	if got := ensureBackupJSON(string(mover.OpBackup), []string{"backup", "/data/ns/pvc", "--json"}); countArg(got, "--json") != 1 {
		t.Errorf("ensureBackupJSON(backup with --json) = %v, want it kept exactly once", got)
	}

	// does not add --json for any non-backup operation
	for _, op := range []mover.Operation{mover.OpInit, mover.OpForget, mover.OpPrune, mover.OpCheck} {
		if got := ensureBackupJSON(string(op), []string{string(op)}); countArg(got, "--json") != 0 {
			t.Errorf("ensureBackupJSON(%s) = %v, want no --json added", op, got)
		}
	}

	// does not mutate the caller's backing array: give the input spare capacity so a naive append
	// would write --json into index 2, then confirm the backing array is untouched.
	orig := make([]string, 2, 4)
	copy(orig, []string{"backup", "/data"})
	_ = ensureBackupJSON(string(mover.OpBackup), orig)
	if full := orig[:cap(orig)]; full[2] == "--json" {
		t.Errorf("ensureBackupJSON mutated the caller's backing array: %v", full)
	}
}

// TestTailWriterKeepsLastLine covers the failure-reason path used by main(): the bounded tail
// writer retains only its last max bytes, and lastLine pulls restic's final line out of that
// tail. Together they turn a long stderr stream into the short "Fatal: ..." reason the shim
// folds into a MoverResult.
func TestTailWriterKeepsLastLine(t *testing.T) {
	w := &tailWriter{max: 16}
	for _, chunk := range []string{"aaaaaaaa\n", "warning one\n", "Fatal: boom\n"} {
		n, err := w.Write([]byte(chunk))
		if err != nil || n != len(chunk) {
			t.Fatalf("Write(%q) = %d, %v; want %d, nil", chunk, n, err, len(chunk))
		}
	}
	if got := len(w.Bytes()); got > 16 {
		t.Errorf("tailWriter retained %d bytes, want <= 16", got)
	}
	if got := lastLine(w.Bytes()); got != "Fatal: boom" {
		t.Errorf("lastLine(tail) = %q, want %q", got, "Fatal: boom")
	}
	if got := lastLine(nil); got != "" {
		t.Errorf("lastLine(nil) = %q, want empty", got)
	}
}

// TestKnownOperation guards the operation allow-list main() screens on before running restic:
// every mover.Op* value is accepted, and anything else (a typo, an empty flag) is rejected so a
// mistyped "backup" can never be silently treated as a maintenance success.
func TestKnownOperation(t *testing.T) {
	for _, op := range []mover.Operation{mover.OpBackup, mover.OpInit, mover.OpForget, mover.OpPrune, mover.OpCheck} {
		if !knownOperation(string(op)) {
			t.Errorf("knownOperation(%q) = false, want true", op)
		}
	}
	for _, bad := range []string{"", "backupp", "BACKUP", "restore", "--json"} {
		if knownOperation(bad) {
			t.Errorf("knownOperation(%q) = true, want false", bad)
		}
	}
}

// countArg reports how many times target appears in argv; used to assert --json presence/absence
// and non-duplication without depending on its position.
func countArg(argv []string, target string) int {
	n := 0
	for _, a := range argv {
		if a == target {
			n++
		}
	}
	return n
}
