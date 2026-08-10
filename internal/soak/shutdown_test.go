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
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The shutdown block exists because a pod replacement made a fortnight's archive unreachable and
// the only thing that survived was a human having copied `soak-export --status` down by hand. These
// tests are about that: the block has to carry the figures nobody can reconstruct, it has to be
// findable in a log, and it has to be produced even when there is nothing to say.

// shutdownMarks writes highwater/marks.json with one measured class, which is the payload the whole
// block exists to carry.
func shutdownMarks(t *testing.T, store *Store, peak int64) {
	t.Helper()
	body, err := json.Marshal(Marks{
		UpdatedAt:     day0,
		MetricsServer: MeasuredValue{Status: statusOK, Source: sourceSampled},
		KubeletStats:  MeasuredValue{Status: statusNotMeasured, Reason: "--kubelet-stats not set"},
		Classes: map[string]ClassMarks{
			"data": {
				Class: "data", Pods: 4,
				Memory:               MeasuredValue{Status: statusOK, Source: sourceMover},
				ReportedPeakRSSBytes: peak,
				ReportedPods:         4,
			},
		},
		Pods: []PodMark{{}, {}, {}, {}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.WriteFileAtomic(fileMarks, body); err != nil {
		t.Fatal(err)
	}
}

func testInfo() CollectorInfo {
	return CollectorInfo{
		OperatorVersion: "0.6.6", MaxBytes: 512 << 20,
		MetricsResolution: "5m", StateInterval: "1h", SelfcheckInterval: "24h",
		SelfcheckEnabled: true,
	}
}

// TestShutdownReportCarriesTheFiguresNothingElseKeeps is the point of the whole block.
//
// The per-class peak memory table is on the volume and in no other place: the daily heartbeat line
// carries mover COUNTS, MANIFEST.json only exists once an archive has been made, and the volume is
// exactly what may be unreachable after the handover this report is written for. If the peak is not
// in the log, the report is decoration.
func TestShutdownReportCarriesTheFiguresNothingElseKeeps(t *testing.T) {
	store := newTestStore(t, 1<<20)
	const peak = 700 << 20
	shutdownMarks(t, store, peak)

	got := ShutdownReport(store, testInfo(), day0.Add(3*time.Hour))

	for _, want := range []string{
		store.Dir(),      // where the archive is, said out loud
		"data",           // the class
		humanBytes(peak), // ITS PEAK. The figure that cannot be reconstructed.
		"mover-reported", // and which of the two sources it came from
		"on disk",        // the footprint, against the cap
		"0.6.6",          // the build the peak belongs to (§9: an upgrade invalidates the table)
		"highwater",      // the stream label, so a reader can find the table
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the shutdown block does not carry %q. It is the only off-volume copy of the\n"+
				"figures at the moment the volume may become unreachable.\n%s", want, got)
		}
	}
}

// TestShutdownReportIsOneGreppableBlock. The daily line is one line because seven have to fit on a
// screen; this is a block, emitted while several pods are being replaced into a log pipeline that
// interleaves them. A continuation line without the marker is a line `grep soak-shutdown` drops,
// and the reader gets the header and none of the numbers.
func TestShutdownReportIsOneGreppableBlock(t *testing.T) {
	store := newTestStore(t, 1<<20)
	shutdownMarks(t, store, 700<<20)

	got := ShutdownReport(store, testInfo(), day0)
	lines := strings.Split(got, "\n")
	if len(lines) < 10 {
		t.Fatalf("the block is %d line(s); it is meant to carry the whole status table\n%s",
			len(lines), got)
	}
	for i, l := range lines {
		if !strings.HasPrefix(l, shutdownPrefix) {
			t.Errorf("line %d does not carry the marker, so `grep %s` would drop it:\n  %q",
				i+1, shutdownPrefix, l)
		}
	}
}

// TestShutdownReportSaysWhyItExists. The block is read by somebody who has just replaced a pod, or
// is about to, and the actionable part is not the numbers — it is that deleting the PVC destroys the
// archive and that there is a procedure for doing it in the right order.
func TestShutdownReportSaysWhyItExists(t *testing.T) {
	store := newTestStore(t, 1<<20)
	got := ShutdownReport(store, testInfo(), day0)

	for _, want := range []string{
		"NOWHERE ELSE",      // the archive is on one volume
		"ReadWriteOnce",     // and this is the property that makes the handover risky
		"ContainerCreating", // what the failure actually looks like when it happens
		"README.md",         // and where the procedure is
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the shutdown block never mentions %q; a reader who does not know why the\n"+
				"figures are there will not copy them.\n%s", want, got)
		}
	}
}

// TestShutdownReportOnAFreshCollector. A collector eleven seconds old has nothing to report and
// must still report that — and must not invent a peak it never measured. Every restart of a young
// collector is another session and another volume handover, so the block a reader sees at eleven
// seconds has to be recognisable as "nothing yet" rather than as a fault.
func TestShutdownReportOnAFreshCollector(t *testing.T) {
	store := newTestStore(t, 1<<20)
	got := ShutdownReport(store, testInfo(), day0)

	if strings.TrimSpace(got) == "" {
		t.Fatal("an empty block: indistinguishable from a collector that was SIGKILLed")
	}
	if !strings.Contains(got, "NOTHING COLLECTED") {
		t.Errorf("a fresh collector's block does not say NOTHING COLLECTED:\n%s", got)
	}
	if !strings.Contains(got, "NOT MEASURED") {
		t.Errorf("a fresh collector's block does not report the high-water as NOT MEASURED — it "+
			"must never be reported as zero:\n%s", got)
	}
	if strings.Contains(got, "mover-reported") {
		t.Errorf("the block claims a mover-reported peak on a collector that has measured "+
			"nothing:\n%s", got)
	}
}

// TestShutdownReportSurvivesAStoreItCannotRead. The one guarantee that matters more than the
// content: this must never be the thing that turns an orderly SIGTERM into a crash, and it must
// never go silent because a directory was unreadable — that is precisely the shutdown where it is
// worth having.
func TestShutdownReportSurvivesAStoreItCannotRead(t *testing.T) {
	if got := ShutdownReport(nil, CollectorInfo{}, day0); !strings.HasPrefix(got, shutdownPrefix) {
		t.Errorf("no block at all with no store:\n%s", got)
	} else if !strings.Contains(got, "no store") {
		t.Errorf("the no-store block does not distinguish a startup failure from an empty soak:\n%s", got)
	}

	// A directory that does not exist: every reader underneath is expected to swallow its own
	// error, and the block still has to name the data dir a human would go and look at.
	store := &Store{dir: "/nonexistent/soak-data"}
	got := ShutdownReport(store, CollectorInfo{}, day0)
	if !strings.Contains(got, "/nonexistent/soak-data") {
		t.Errorf("the block does not name the unreadable data dir:\n%s", got)
	}

	// And a store with no directory at all still produces a parseable first line. `data-dir=`
	// followed by nothing reads as a truncated log line rather than as a collector that never
	// opened one, and the difference decides whether anyone goes and looks.
	if got := ShutdownReport(&Store{}, CollectorInfo{}, day0); !strings.Contains(got, "data-dir=(none)") {
		t.Errorf("an empty data dir renders as an empty value, which reads as a truncated line:\n%s", got)
	}
}

// TestTheLoopReportsOnItsWayOut is the wiring, and without it every test above passes against a
// report nothing ever calls. It is also the ordering: the block must come AFTER the flush, or the
// footprint and the last window it reports are a minute stale.
func TestTheLoopReportsOnItsWayOut(t *testing.T) {
	store := newTestStore(t, 1<<20)
	shutdownMarks(t, store, 700<<20)
	sessions, err := OpenSessionLog(store, day0, 15*time.Second, "test")
	if err != nil {
		t.Fatal(err)
	}

	var log bytes.Buffer
	c := &Collector{
		Store: store, Sessions: sessions, Info: testInfo(),
		Aggregator:      NewAggregator(5 * time.Minute),
		MetricsInterval: time.Minute, MoverInterval: time.Hour, StateInterval: time.Hour,
		Progress: &log,
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := c.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	out := log.String()
	if !strings.Contains(out, shutdownPrefix) {
		t.Fatalf("the loop shut down without writing the shutdown block — every test above is then "+
			"asserting about a function nothing calls:\n%s", out)
	}
	if !strings.Contains(out, humanBytes(700<<20)) {
		t.Errorf("the block the loop wrote does not carry the peak figure:\n%s", out)
	}
	// The ORDER — and it has to be asserted on the block's CONTENT, not on where it sits in the
	// log. That was the first attempt and it was worthless: `shutting down: flushing the open
	// metrics window` is printed BEFORE the flush either way, so a report moved above
	// `c.flushWindow` still lands after that line in the log and a line-position check passes. The
	// mutation that swaps the two survived exactly that assertion.
	//
	// What the flush actually does is write the open metrics window to disk. A block taken after it
	// counts the core segment it just produced; a block taken before counts nothing — which is a
	// report of a volume that the very next statement changes.
	if !strings.Contains(out, "(+1 core)") {
		t.Errorf("the block counts no core metrics segment, so it was written BEFORE the open "+
			"window was flushed and it describes a volume the flush is about to change.\n%s", out)
	}
}
