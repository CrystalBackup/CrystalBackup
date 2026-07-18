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
	"reflect"
	"testing"
)

// TestMoverResultRoundTrip proves Encode and ParseMoverResult are exact inverses for the
// two shapes that actually cross the wire: a successful backup result (all fields set) and
// a failure result (OK false + Error). This is the operator↔mover contract, so a silent
// drift in a JSON tag would strand a controller unable to read its own mover's outcome.
func TestMoverResultRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		in   MoverResult
	}{
		{
			name: "backup success",
			in: MoverResult{
				OK:         true,
				Operation:  "backup",
				SnapshotID: "abc123def456",
				SizeBytes:  2048,
				AddedBytes: 1536,
			},
		},
		{
			name: "maintenance success",
			in:   MoverResult{OK: true, Operation: "init"},
		},
		{
			name: "failure",
			in:   MoverResult{OK: false, Operation: "backup", Error: "repository locked"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			enc, err := c.in.Encode()
			if err != nil {
				t.Fatalf("Encode() error: %v", err)
			}
			got, err := ParseMoverResult(enc)
			if err != nil {
				t.Fatalf("ParseMoverResult(%q) error: %v", enc, err)
			}
			if !reflect.DeepEqual(got, c.in) {
				t.Errorf("round-trip = %+v, want %+v (encoded %q)", got, c.in, enc)
			}
		})
	}
}

// TestMoverResultEncodeOmitsEmpty pins the compact, omitempty-driven wire form: a bare
// maintenance success must serialise to just {"ok":true}, and OK:false must always be
// emitted explicitly (no omitempty on OK) so a failure never decodes back as a zero-value
// success by omission.
func TestMoverResultEncodeOmitsEmpty(t *testing.T) {
	ok, err := MoverResult{OK: true}.Encode()
	if err != nil {
		t.Fatalf("Encode() error: %v", err)
	}
	if ok != `{"ok":true}` {
		t.Errorf("Encode(OK:true) = %q, want %q", ok, `{"ok":true}`)
	}

	fail, err := MoverResult{OK: false}.Encode()
	if err != nil {
		t.Fatalf("Encode() error: %v", err)
	}
	if fail != `{"ok":false}` {
		t.Errorf("Encode(OK:false) = %q, want %q", fail, `{"ok":false}`)
	}
}

// TestParseMoverResultBlank is the load-bearing failure case: the kubelet leaves the
// termination message empty when the container is killed before it writes (OOMKilled,
// SIGKILL, eviction). Every blank/whitespace variant MUST error so a controller treats the
// crash as a failure rather than a silent empty success.
func TestParseMoverResultBlank(t *testing.T) {
	for _, msg := range []string{"", "   ", "\n", "\t\n ", " \r\n\t "} {
		if _, err := ParseMoverResult(msg); err == nil {
			t.Errorf("ParseMoverResult(%q) = nil error, want error (empty message must fail)", msg)
		}
	}
}

// TestParseMoverResultInvalidJSON confirms non-JSON content is a hard error too: the only
// success path is a MoverResult the shim actually marshalled, never a truncated or garbled
// message.
func TestParseMoverResultInvalidJSON(t *testing.T) {
	for _, msg := range []string{"not json", `{"ok":true`, `{"ok":true} trailing`} {
		if _, err := ParseMoverResult(msg); err == nil {
			t.Errorf("ParseMoverResult(%q) = nil error, want error", msg)
		}
	}
}

// realisticBackupStream is a trimmed but faithful `restic backup --json` stream: two status
// progress objects, then the final summary. Field names match restic's real output.
const realisticBackupStream = `{"message_type":"status","seconds_elapsed":0,"percent_done":0,"total_files":3,"total_bytes":2048}
{"message_type":"status","seconds_elapsed":1,"percent_done":0.5,"files_done":1,"bytes_done":1024,"total_files":3,"total_bytes":2048}
{"message_type":"summary","files_new":3,"files_changed":0,"files_unmodified":0,"dirs_new":1,"dirs_changed":0,"dirs_unmodified":0,"data_blobs":5,"tree_blobs":1,"data_added":1536,"total_files_processed":3,"total_bytes_processed":2048,"total_duration":0.123,"snapshot_id":"abc123def456"}
`

// TestParseBackupSummary parses the realistic stream and pins every field the mover reports
// on. The status lines must be ignored and only the summary object's fields returned.
func TestParseBackupSummary(t *testing.T) {
	got, err := ParseBackupSummary([]byte(realisticBackupStream))
	if err != nil {
		t.Fatalf("ParseBackupSummary error: %v", err)
	}
	want := ResticBackupSummary{
		MessageType:         "summary",
		SnapshotID:          "abc123def456",
		TotalBytesProcessed: 2048,
		DataAdded:           1536,
	}
	if got != want {
		t.Errorf("ParseBackupSummary = %+v, want %+v", got, want)
	}
}

// TestParseBackupSummaryNoSummary confirms a stream that never emitted a summary (e.g. the
// backup was killed mid-run) is an error, not a zero-value "success" with an empty snapshot
// id. The caller must be able to tell "backup did not finish" from "backup finished empty".
func TestParseBackupSummaryNoSummary(t *testing.T) {
	statusOnly := `{"message_type":"status","percent_done":0.5}
{"message_type":"status","percent_done":0.9}
`
	if _, err := ParseBackupSummary([]byte(statusOnly)); err == nil {
		t.Error("ParseBackupSummary(status-only) = nil error, want error")
	}
	if _, err := ParseBackupSummary(nil); err == nil {
		t.Error("ParseBackupSummary(nil) = nil error, want error")
	}
}

// TestParseBackupSummaryNoiseAndLastWins proves the scan is robust: blank lines and
// non-JSON warning text are skipped gracefully, and when more than one summary appears the
// LAST one wins (defensive against a malformed stream).
func TestParseBackupSummaryNoiseAndLastWins(t *testing.T) {
	stream := `
warning: could not read some file, skipping
{"message_type":"status","percent_done":0.1}
{"message_type":"summary","data_added":10,"total_bytes_processed":20,"snapshot_id":"first"}

{"message_type":"summary","data_added":1536,"total_bytes_processed":2048,"snapshot_id":"last"}
`
	got, err := ParseBackupSummary([]byte(stream))
	if err != nil {
		t.Fatalf("ParseBackupSummary error: %v", err)
	}
	if got.SnapshotID != "last" || got.TotalBytesProcessed != 2048 || got.DataAdded != 1536 {
		t.Errorf("ParseBackupSummary = %+v, want last summary (snapshot_id=last, 2048, 1536)", got)
	}
}

// TestParseBackupSummaryUnterminatedFinalLine guards the bufio edge case: restic does not
// always end its last object with a newline, so a summary on the final unterminated line
// must still be parsed (ReadBytes returns the bytes together with io.EOF).
func TestParseBackupSummaryUnterminatedFinalLine(t *testing.T) {
	stream := `{"message_type":"status","percent_done":0.9}` + "\n" +
		`{"message_type":"summary","data_added":7,"total_bytes_processed":11,"snapshot_id":"noeol"}`
	got, err := ParseBackupSummary([]byte(stream))
	if err != nil {
		t.Fatalf("ParseBackupSummary error: %v", err)
	}
	if got.SnapshotID != "noeol" || got.TotalBytesProcessed != 11 || got.DataAdded != 7 {
		t.Errorf("ParseBackupSummary = %+v, want snapshot_id=noeol, 11, 7", got)
	}
}

// TestSummaryToResult pins the summary->result field mapping the shim relies on:
// total_bytes_processed -> SizeBytes, data_added -> AddedBytes, snapshot id carried
// through, Operation forced to "backup", OK true.
func TestSummaryToResult(t *testing.T) {
	got := SummaryToResult(ResticBackupSummary{
		MessageType:         "summary",
		SnapshotID:          "abc123def456",
		TotalBytesProcessed: 2048,
		DataAdded:           1536,
	})
	want := MoverResult{
		OK:         true,
		Operation:  "backup",
		SnapshotID: "abc123def456",
		SizeBytes:  2048,
		AddedBytes: 1536,
	}
	if got != want {
		t.Errorf("SummaryToResult = %+v, want %+v", got, want)
	}
	if got.Operation != string(OpBackup) {
		t.Errorf("Operation = %q, want %q", got.Operation, OpBackup)
	}
}

// TestParseMoverResultRejectsTrailingWhitespaceIsFine documents that surrounding
// whitespace around a valid object is tolerated (JSON ignores it), so a shim that writes a
// trailing newline is still parsed — only a wholly blank message is the failure signal.
func TestParseMoverResultToleratesSurroundingWhitespace(t *testing.T) {
	got, err := ParseMoverResult("  {\"ok\":true,\"operation\":\"check\"}\n")
	if err != nil {
		t.Fatalf("ParseMoverResult error: %v", err)
	}
	if !got.OK || got.Operation != "check" {
		t.Errorf("ParseMoverResult = %+v, want {OK:true, Operation:check}", got)
	}
}
