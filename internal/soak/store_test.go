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

package soak

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// day0 is the soak's day one. Fixed, so every segment name and every drop record in these tests
// is reproducible rather than dependent on when the suite runs.
var day0 = time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

func newTestStore(t *testing.T, maxBytes int64) *Store {
	t.Helper()
	s, err := OpenStore(t.TempDir(), maxBytes)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	return s
}

// fill grows one day's segment of a stream until it occupies at least n bytes ON DISK.
//
// Measured rather than computed, because the metrics and state segments are gzipped and a
// predictable payload compresses to nothing — a fixture that wrote "16KiB of text" would produce
// a 1KiB file and a cap test that proved nothing. The payload is incompressible for the same
// reason.
func fill(t *testing.T, s *Store, stream string, day time.Time, n int64) {
	t.Helper()
	rnd := rand.New(rand.NewPCG(uint64(day.Unix()), 0x50AC)) //nolint:gosec // fixture entropy
	path := s.segmentPath(stream, day)
	for {
		if info, err := os.Stat(path); err == nil && info.Size() >= n {
			return
		}
		var lines [][]byte
		for range 16 {
			buf := make([]byte, 512)
			for i := range buf {
				buf[i] = byte(rnd.UintN(256))
			}
			lines = append(lines, []byte(hex.EncodeToString(buf)))
		}
		if err := s.Append(stream, day, lines); err != nil {
			t.Fatalf("Append(%s, %s): %v", stream, day.Format(dayLayout), err)
		}
		if info, err := os.Stat(path); err != nil || info.Size() == 0 {
			t.Fatalf("Append(%s, %s) wrote nothing", stream, day.Format(dayLayout))
		}
	}
}

// TestFootprintCapDropsTheOldestAndSaysSo is §4, which is the rule this pod's presence on a
// customer's cluster is justified by.
//
// The collector runs for a fortnight in the operator's namespace on the cluster under test. On a
// node-backed StorageClass its PVC IS node disk, so a collector that filled it would cause the
// very DiskPressure eviction it was deployed to observe. Dropping the oldest data is the
// defensible behaviour; running the node out of space is not.
//
// Three things are asserted together because any two without the third is a bug: the footprint
// comes back under the cap, the OLDEST segment is what went, and every deletion is recorded where
// collect.sh reads it.
func TestFootprintCapDropsTheOldestAndSaysSo(t *testing.T) {
	const cap = 64 << 10
	s := newTestStore(t, cap)

	for i := range 8 {
		fill(t, s, StreamMetrics, day0.AddDate(0, 0, i), 16<<10)
	}
	before, err := s.Footprint()
	if err != nil {
		t.Fatal(err)
	}
	if before <= cap {
		t.Fatalf("the fixture wrote %d bytes against a cap of %d: this test proves nothing", before, cap)
	}

	// "Now" is day 9, so all eight segments are closed and every one of them is a candidate.
	now := day0.AddDate(0, 0, 8)
	if err := s.EnforceCap(now); err != nil {
		t.Fatalf("EnforceCap: %v", err)
	}

	after, err := s.Footprint()
	if err != nil {
		t.Fatal(err)
	}
	if after > cap {
		t.Errorf("footprint is %d after the cap ran, above --max-bytes of %d: the volume can still "+
			"fill and the node can still be evicted", after, cap)
	}

	days := s.Days(StreamMetrics)
	if len(days) == 0 {
		t.Fatal("every segment was dropped; the cap should stop at the cap, not at zero")
	}
	// The oldest survivor must be strictly later than the oldest thing that was there.
	if !days[0].After(day0) {
		t.Errorf("the oldest segment (%s) survived: the cap dropped something other than the "+
			"oldest data", days[0].Format(dayLayout))
	}

	drops := s.DropsFor(StreamMetrics)
	if len(drops) == 0 {
		t.Fatal("the cap deleted data and recorded no drop. An archive that lost its first days " +
			"must say so on its first page; collect.sh reads this field and nothing else can tell it")
	}
	for _, d := range drops {
		if d.Reason != "cap" {
			t.Errorf("drop reason = %q, want %q (collect.sh reports the field verbatim)", d.Reason, "cap")
		}
		if _, err := time.Parse(time.RFC3339, d.From); err != nil {
			t.Errorf("drop.from = %q is not RFC3339: %v", d.From, err)
		}
		if _, err := time.Parse(time.RFC3339, d.To); err != nil {
			t.Errorf("drop.to = %q is not RFC3339: %v", d.To, err)
		}
	}
	// And the drops survive a restart, because the process that lost day 1 is rarely the process
	// that exports.
	reopened, err := OpenStore(s.Dir(), cap)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(reopened.DropsFor(StreamMetrics)); got != len(drops) {
		t.Errorf("after reopening, %d drop(s) recorded, want %d: the archive would understate what "+
			"it lost", got, len(drops))
	}
}

// TestCapNeverTouchesWhatCannotBeReconstructed is the other half of §4. The high-water marks, the
// self-checks and the event stream are two orders of magnitude smaller than the raw metrics and
// they cannot be rebuilt from anything, so they are what the raw metrics are sacrificed FOR.
func TestCapNeverTouchesWhatCannotBeReconstructed(t *testing.T) {
	const cap = 32 << 10
	s := newTestStore(t, cap)

	if err := s.WriteFileAtomic(fileMarks, bytes.Repeat([]byte("m"), 8<<10)); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteFileAtomic("selfcheck/2026-06-01T00:00:00Z.json", bytes.Repeat([]byte("s"), 8<<10)); err != nil {
		t.Fatal(err)
	}
	for i := range 4 {
		fill(t, s, StreamEvents, day0.AddDate(0, 0, i), 4<<10)
		fill(t, s, StreamMetrics, day0.AddDate(0, 0, i), 16<<10)
	}

	if err := s.EnforceCap(day0.AddDate(0, 0, 8)); err != nil {
		t.Fatal(err)
	}

	if _, err := readFile(s.Dir(), fileMarks); err != nil {
		t.Errorf("the high-water marks were deleted by the cap: %v. They are the one number the "+
			"maintainer cannot obtain any other way", err)
	}
	if names, _ := countSelfchecks(s.Dir()); len(names) != 1 {
		t.Errorf("%d self-check(s) survived, want 1", len(names))
	}
	if got := len(s.Days(StreamEvents)); got != 4 {
		t.Errorf("%d event segment(s) survived, want 4: events expire from the API after an hour "+
			"and nothing can recover them", got)
	}
	if len(s.DropsFor(StreamEvents)) != 0 {
		t.Error("an event segment was recorded as dropped")
	}
}

// TestCapDropOrderIsFixed proves the sacrifice order §4 fixes: raw metrics, then CR state, then
// the operator's error lines, and §3b's core metrics last of all.
//
// The order is the whole argument. Raw metrics are the bulkiest and the most reconstructable from
// their own aggregates; the core series are the ones whose TREND over a fortnight is the point,
// and they are kept at full resolution to the end.
func TestCapDropOrderIsFixed(t *testing.T) {
	// Sized so the cap is reachable by exhausting the three droppable streams ahead of the core
	// one. If core had to be touched to get under it, this test would be asserting the cap's
	// limits rather than its order.
	const cap = 64 << 10
	s := newTestStore(t, cap)

	for _, stream := range []string{StreamMetrics, StreamState, StreamLogs, streamMetricsCore} {
		for i := range 3 {
			fill(t, s, stream, day0.AddDate(0, 0, i), 12<<10)
		}
	}
	if err := s.EnforceCap(day0.AddDate(0, 0, 8)); err != nil {
		t.Fatal(err)
	}

	remaining := map[string]int{}
	for _, stream := range []string{StreamMetrics, StreamState, StreamLogs, streamMetricsCore} {
		remaining[stream] = len(s.Days(stream))
	}
	// Whatever the exact numbers, a stream may only have been touched once every stream ahead of
	// it in the order is exhausted.
	order := []string{StreamMetrics, StreamState, StreamLogs, streamMetricsCore}
	for i := 1; i < len(order); i++ {
		if remaining[order[i]] < 3 && remaining[order[i-1]] != 0 {
			t.Errorf("%s lost a segment while %s still had %d: the sacrifice order is not the one "+
				"§4 fixes (%v)", order[i], order[i-1], remaining[order[i-1]], remaining)
		}
	}
	if remaining[streamMetricsCore] != 3 {
		t.Errorf("§3b's core series lost %d segment(s) while %v remained; they must survive to the end",
			3-remaining[streamMetricsCore], remaining)
	}
}

// TestCapNeverDropsTheOpenSegment: today's segment is being appended to. Deleting it would drop
// data the collector is about to write more of, and would take the current window's aggregates
// with it.
func TestCapNeverDropsTheOpenSegment(t *testing.T) {
	const cap = 1 << 10
	s := newTestStore(t, cap)
	fill(t, s, StreamMetrics, day0, 32<<10)

	// "Now" IS day0, so the only segment on disk is the open one.
	if err := s.EnforceCap(day0.Add(12 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got := len(s.Days(StreamMetrics)); got != 1 {
		t.Fatalf("the open segment was dropped (%d remain)", got)
	}
	// With nothing droppable and the footprint still over, the collector must go degraded rather
	// than delete something it promised to keep or keep filling the volume.
	degraded, _, reason := s.DegradedSince()
	if !degraded {
		t.Fatal("the footprint is over the cap with nothing left to drop and the collector is not " +
			"degraded: it would go on filling the volume")
	}
	if !strings.Contains(reason, "no closed segment left to drop") {
		t.Errorf("degraded reason does not say why: %q", reason)
	}
	// And degraded mode is what actually stops the bulk streams.
	before, _ := s.Footprint()
	fill(t, s, StreamMetrics, day0, 8<<10)
	after, _ := s.Footprint()
	if after != before {
		t.Errorf("a degraded collector wrote %d more bytes of raw samples", after-before)
	}
	// While the streams that cannot be reconstructed keep going.
	fill(t, s, StreamEvents, day0, 1<<10)
	if got, _ := s.Footprint(); got <= after {
		t.Error("a degraded collector stopped capturing events; a degraded collector still holds " +
			"the parts that cannot be rebuilt")
	}
}

// TestDegradedSurvivesARestart: whether raw sampling was stopped for six hours on day nine is a
// property of the ARCHIVE, not of the process that happened to be running.
func TestDegradedSurvivesARestart(t *testing.T) {
	s := newTestStore(t, 1<<20)
	s.setDegraded(day0, "test")
	reopened, err := OpenStore(s.Dir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if !reopened.Degraded() {
		t.Error("degraded mode was forgotten across a restart")
	}
}

// TestScrapeFailureIsRecordedNotSwallowed is the counterweight to every `if err != nil` in this
// package.
//
// A hole in the metrics with no explanation reads as "the operator emitted nothing", which is the
// exact confusion internal/metrics/names.go documents an alert dying of. It has to be recorded,
// it has to survive a restart, and a failure that repeats every minute for eleven days has to
// cost one entry rather than 15,840 lines on a capped volume.
func TestScrapeFailureIsRecordedNotSwallowed(t *testing.T) {
	s := newTestStore(t, 1<<20)
	const msg = "scrape https://operator:8443/metrics: HTTP 401: Unauthorized"

	s.RecordError(StreamMetrics, msg, day0)
	got := s.ErrorsFor(StreamMetrics)
	if len(got) != 1 {
		t.Fatalf("%d error(s) recorded, want 1: a swallowed scrape failure is a hole in the "+
			"archive that reads as health", len(got))
	}
	if got[0].Message != msg {
		t.Errorf("message = %q, want %q", got[0].Message, msg)
	}

	for i := 1; i <= 400; i++ {
		s.RecordError(StreamMetrics, msg, day0.Add(time.Duration(i)*time.Minute))
	}
	got = s.ErrorsFor(StreamMetrics)
	if len(got) != 1 {
		t.Fatalf("a repeated failure produced %d entries; it must coalesce", len(got))
	}
	if got[0].Count != 401 {
		t.Errorf("count = %d, want 401", got[0].Count)
	}
	if !got[0].LastAt.After(got[0].FirstAt) {
		t.Error("firstAt and lastAt do not bracket the failure; how long it went on is the finding")
	}

	reopened, err := OpenStore(s.Dir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	after := reopened.ErrorsFor(StreamMetrics)
	if len(after) != 1 || after[0].Count != 401 {
		t.Errorf("after a restart the failure record is %+v; the process that was 401ing is rarely "+
			"the process that exports", after)
	}
}

// TestOpenStoreRefusals is §1: a collector that starts happily and collects nothing is the
// failure this whole kit exists to avoid, and it must not be possible to reach it by accident.
func TestOpenStoreRefusals(t *testing.T) {
	t.Run("an unwritable data dir is refused", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("running as root: every directory is writable")
		}
		dir := filepath.Join(t.TempDir(), "ro")
		if err := os.Mkdir(dir, 0o500); err != nil {
			t.Fatal(err)
		}
		_, err := OpenStore(dir, 1<<20)
		if err == nil {
			t.Fatal("OpenStore accepted an unwritable directory. It would run for a fortnight and " +
				"write nothing")
		}
		if !strings.Contains(err.Error(), dir) {
			t.Errorf("the refusal does not name the directory: %v", err)
		}
	})

	// The failure the MkdirAll above cannot catch, and the one that actually happens: a volume
	// whose directories already exist and that has since become read-only. MkdirAll returns nil
	// for a directory that is already there, so without the write probe this collector would run
	// for a fortnight against a read-only PVC and report nothing wrong.
	t.Run("a data dir that has gone read-only is refused", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("running as root: every directory is writable")
		}
		dir := t.TempDir()
		if _, err := OpenStore(dir, 1<<20); err != nil {
			t.Fatalf("the fixture could not be created: %v", err)
		}
		sub := filepath.Join(dir, dirCollector)
		if err := os.Chmod(sub, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(sub, 0o700) })

		if _, err := OpenStore(dir, 1<<20); err == nil {
			t.Fatal("OpenStore accepted a data directory it cannot write to. Every subdirectory " +
				"already existed, so nothing else in the startup path would have noticed")
		}
	})

	t.Run("a cap larger than the free space is refused", func(t *testing.T) {
		dir := t.TempDir()
		free, err := freeBytes(dir)
		if err != nil {
			t.Skipf("free space is not measurable here: %v", err)
		}
		_, err = OpenStore(dir, free*4)
		if err == nil {
			t.Fatal("OpenStore accepted a --max-bytes above the free space. The collector would " +
				"spend the soak in degraded mode rather than collecting, and would look healthy")
		}
		if !strings.Contains(err.Error(), "--max-bytes") {
			t.Errorf("the refusal does not name the flag to change: %v", err)
		}
	})

	t.Run("a usable directory is accepted", func(t *testing.T) {
		if _, err := OpenStore(t.TempDir(), 1<<20); err != nil {
			t.Fatalf("OpenStore refused a perfectly good directory: %v", err)
		}
	})
}

// TestAppendIsCrashSafeAcrossGzipMembers: each flush writes a complete gzip member, so a process
// killed mid-append loses at most that member rather than the whole day.
func TestAppendIsCrashSafeAcrossGzipMembers(t *testing.T) {
	s := newTestStore(t, 1<<20)
	for i := range 5 {
		line, err := json.Marshal(point{T: int64(i), Name: "x", Last: float64(i), N: 1})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Append(StreamMetrics, day0, [][]byte{line}); err != nil {
			t.Fatal(err)
		}
	}
	lines, err := readNDJSONGz(filepath.Join(s.Dir(), StreamMetrics, day0.Format(dayLayout)+".ndjson.gz"))
	if err != nil {
		t.Fatalf("a segment of five concatenated gzip members will not read back: %v", err)
	}
	if len(lines) != 5 {
		t.Errorf("read %d lines back, want 5", len(lines))
	}
}

func TestParseSize(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{"512Mi", 512 << 20, false},
		{"1Gi", 1 << 30, false},
		{"536870912", 536870912, false},
		{"", 0, true},
		{"512MB?", 0, true},
		{"0", 0, true},
		{"-1", 0, true},
	} {
		t.Run(tc.in, func(t *testing.T) {
			got, err := parseSize(tc.in)
			if tc.wantErr != (err != nil) {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if !tc.wantErr && got != tc.want {
				t.Errorf("got %d, want %d", got, tc.want)
			}
		})
	}
}
