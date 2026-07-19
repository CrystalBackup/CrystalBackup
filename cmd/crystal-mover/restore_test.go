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
	"testing"

	"github.com/CrystalBackup/CrystalBackup/internal/mover"
)

// TestBuildResultRestore covers the shim's restore branch, mirroring the backup contract:
// a clean exit REQUIRES a parseable restore summary (exit 0 with no summary is a truncated
// stream, reported as failure, never an unverified success); a clean exit with a summary
// yields the translated result; and a restic error dominates regardless of stdout.
func TestBuildResultRestore(t *testing.T) {
	summary := []byte(`{"message_type":"summary","total_files":3,"files_restored":3,"total_bytes":300,"bytes_restored":300}`)

	got := buildResult(string(mover.OpRestore), summary, nil)
	if !got.OK || got.Operation != string(mover.OpRestore) || got.RestoredBytes != 300 || got.SizeBytes != 300 {
		t.Errorf("clean restore = %+v, want OK with RestoredBytes=300 SizeBytes=300", got)
	}
	if got.SnapshotID != "" {
		t.Errorf("clean restore reported SnapshotID %q, want empty (a restore creates none)", got.SnapshotID)
	}

	got = buildResult(string(mover.OpRestore), []byte("no summary here"), nil)
	if got.OK || got.Error == "" {
		t.Errorf("summary-less clean restore = %+v, want OK=false with an error", got)
	}

	got = buildResult(string(mover.OpRestore), summary, errors.New("exit status 1: Fatal: wrong password"))
	if got.OK {
		t.Errorf("failed restore with stale summary = %+v, want OK=false (the error dominates)", got)
	}
}
