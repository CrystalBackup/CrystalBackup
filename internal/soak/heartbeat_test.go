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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/CrystalBackup/CrystalBackup/internal/mover"
)

// beatValue pulls one key= value off the line, the way a reader with `grep` and `cut` would.
func beatValue(t *testing.T, line, key string) string {
	t.Helper()
	for _, field := range strings.Fields(line) {
		if after, ok := strings.CutPrefix(field, key+"="); ok {
			return after
		}
	}
	t.Fatalf("the line has no %s= field:\n%s", key, line)
	return ""
}

// collectorFor builds a collector over a seeded store, wired to nothing that needs a cluster.
// Only the heartbeat is exercised.
func collectorFor(t *testing.T, store *Store, out *bytes.Buffer) *Collector {
	t.Helper()
	info, err := ReadCollectorInfo(store.Dir())
	if err != nil {
		t.Fatalf("ReadCollectorInfo: %v", err)
	}
	sessions, err := OpenSessionLog(store, day0, 15*time.Second, "test")
	if err != nil {
		t.Fatalf("OpenSessionLog: %v", err)
	}
	// A scraper pointed at a closed port and a metrics interval longer than the test: round()
	// does not nil-check the Scraper (it is proven at startup by proveScrape and cannot be nil in
	// production), so it gets a real one that simply fails. Every OTHER stream is left nil, which
	// round() tolerates by design — a stream that could not be built must not take the rest down.
	scraper := NewScraper("http://127.0.0.1:1/metrics", true)
	scraper.TokenPath = ""
	agg := NewAggregator(365 * 24 * time.Hour)
	agg.Start(day0)
	return &Collector{
		Store: store, Sessions: sessions, Info: info,
		Scraper:         scraper,
		Aggregator:      agg,
		MetricsInterval: 365 * 24 * time.Hour,
		MoverInterval:   15 * time.Second,
		StateInterval:   365 * 24 * time.Hour,
		Progress:        out,
	}
}

// TestTheHeartbeatSaysEnoughToJudgeASoakWithoutExportingIt.
//
// The whole point: an administrator on day 3 must be able to answer "is my soak healthy" from
// `kubectl logs`, without running an export and without knowing anything about this tool. So the
// line has to carry the day, the uptime, what each stream holds, the footprint against the cap —
// and the streams that are silent when silence is not a healthy answer.
func TestTheHeartbeatSaysEnoughToJudgeASoakWithoutExportingIt(t *testing.T) {
	store := seedCollector(t, 3, true)
	info, err := ReadCollectorInfo(store.Dir())
	if err != nil {
		t.Fatal(err)
	}
	line := collectHeartbeat(store, info, day0.AddDate(0, 0, 3)).String()

	if !strings.HasPrefix(line, heartbeatPrefix) {
		t.Fatalf("the line does not start with the grep prefix %q:\n%s", heartbeatPrefix, line)
	}
	if strings.Count(line, "\n") != 0 {
		t.Errorf("the heartbeat is more than one line; seven of them have to fit on a screen:\n%s", line)
	}
	for _, key := range []string{
		"at", "day", "up", "span", "sessions", "version",
		StreamMetrics, StreamState, StreamEvents, StreamLogs,
		"selfchecks", "movers", "footprint", "degraded", "drops", "silent",
	} {
		beatValue(t, line, key) // fails the test if the key is absent
	}
	if got := beatValue(t, line, "day"); got != "4" {
		t.Errorf("day = %s, want 4 (three full days after the start, counting the first as day 1)", got)
	}
	// The build, on the line, because an upgrade mid-soak invalidates every figure derived across
	// the whole fortnight and this is where it becomes a DATE rather than a surprise found at
	// export: `grep soak-heartbeat` over a week of logs shows the day the value changed.
	if got := beatValue(t, line, "version"); got != "0.6.1" {
		t.Errorf("version = %s, want the build the collector recorded for itself (0.6.1)", got)
	}
	if got := beatValue(t, line, StreamMetrics); got == "0" {
		t.Errorf("metrics = 0 over a seeded three-day store:\n%s", line)
	}
	if got := beatValue(t, line, "footprint"); !strings.Contains(got, "/") {
		t.Errorf("footprint = %s, want used/cap — the cap is what makes the number mean anything", got)
	}
}

// TestTheHeartbeatNamesEverySizingClassEvenAtZero is the guard for the failure the aggregate
// `movers=` count could not show.
//
// A four-hour run printed movers=87 and silent=none — healthy by every field on the line — while
// classes `data` and `manifests` were at zero throughout a campaign that executed dozens of
// backups. The collector was structurally blind to both, and repo-light alone kept the aggregate
// comfortably non-zero. So every class from the sizing table is printed, present at zero, which
// turns that failure into something a reader spots on day one: `data:0` next to a backup schedule
// that has been firing.
//
// Zero must be PRINTED, not omitted. A key that appears only once it is non-zero cannot be
// grepped for on the day it matters — the same reason silent= always prints "none".
func TestTheHeartbeatNamesEverySizingClassEvenAtZero(t *testing.T) {
	store := newTestStore(t, 1<<30)
	// One repo-light mover and nothing else: the exact shape of the broken run, where a non-zero
	// total masked two empty classes.
	at := day0.Add(time.Hour)
	writeMarksFixture(t, store, marksFixture(t,
		[]batchv1.Job{moverJob("job-snap", mover.OpSnapshots, at, time.Time{})},
		[]corev1.Pod{moverPod("job-snap-1", "job-snap", "worker-1", at)},
		at.Add(15*time.Second)))

	line := collectHeartbeat(store, CollectorInfo{}, day0.AddDate(0, 0, 2)).String()
	byClass := beatValue(t, line, "movers_by_class")

	for _, class := range mover.Classes() {
		if !strings.Contains(byClass, class+":") {
			t.Errorf("sizing class %q is missing from movers_by_class=%s.\n"+
				"A class that only appears once it is non-zero is a class nobody can notice is "+
				"empty, which is exactly how `data` stayed at zero for four hours unremarked.",
				class, byClass)
		}
	}
	if !strings.Contains(byClass, "data:0") {
		t.Errorf("movers_by_class=%s does not report data:0; an empty class must be stated, "+
			"not omitted", byClass)
	}
	if !strings.Contains(byClass, "repo-light:1") {
		t.Errorf("movers_by_class=%s lost the pod that WAS observed — this test would otherwise "+
			"pass against a breakdown that reports zero for everything", byClass)
	}
	if strings.Contains(byClass, " ") {
		t.Errorf("movers_by_class=%s contains a space; the line is parsed one key per awk field",
			byClass)
	}
}

// TestADeadStreamIsNamedAndAHealthySilenceIsNot is the part that earns the line its place.
//
// A metrics stream at zero means the scrape never worked and the fortnight is being wasted. An
// events stream at zero means nobody had a Warning, which is a GOOD fortnight — and naming it
// would train the reader to ignore the field, which is how the alarm stops working. The two
// cases are decided by the same emptyIsHealthy table MANIFEST.json grades by, so the daily line
// and the archive cannot disagree.
func TestADeadStreamIsNamedAndAHealthySilenceIsNot(t *testing.T) {
	store := newTestStore(t, 1<<30)
	info := CollectorInfo{
		StartedAt: day0, MetricsResolution: "5m0s", StateInterval: "1h0m0s",
		SelfcheckInterval: "24h0m0s", MaxBytes: 1 << 29, SelfcheckEnabled: true,
	}
	writeJSON(t, store, fileCollectorID, info)
	// Three days up, and nothing collected at all.
	writeJSON(t, store, fileStarts, []Session{{
		StartedAt: day0, LastBeat: day0.AddDate(0, 0, 3), BeatPeriod: "15s",
	}})

	h := collectHeartbeat(store, info, day0.AddDate(0, 0, 3))
	silent := beatValue(t, h.String(), "silent")

	for _, dead := range []string{StreamMetrics, StreamState, StreamSelfcheck, StreamHighwater} {
		if !strings.Contains(silent, dead) {
			t.Errorf("%q is empty after three days and is not named in silent=%s. That is the one "+
				"thing this line exists to say", dead, silent)
		}
	}
	for _, healthy := range []string{StreamEvents, StreamLogs} {
		if strings.Contains(silent, healthy) {
			t.Errorf("%q is named in silent=%s; a fortnight with no Warning event and no operator "+
				"error line is a GOOD fortnight, and reporting it as a fault trains the reader to "+
				"ignore this field", healthy, silent)
		}
	}

	// And the counts are on the line either way, so "0 because it is early" is still readable.
	if got := beatValue(t, h.String(), StreamEvents); got != "0" {
		t.Errorf("events = %s, want 0", got)
	}
}

// TestNothingIsCalledSilentBeforeItCouldHaveSpoken. The line written at startup — the one an
// administrator reads seconds after applying the manifest — must not name every stream as dead
// before any of them could possibly have written anything. A first-day false alarm is how a
// reader learns to ignore day nine's real one.
func TestNothingIsCalledSilentBeforeItCouldHaveSpoken(t *testing.T) {
	store := newTestStore(t, 1<<30)
	info := CollectorInfo{
		StartedAt: day0, MetricsResolution: "5m0s", StateInterval: "1h0m0s",
		SelfcheckInterval: "24h0m0s", MaxBytes: 1 << 29,
	}
	writeJSON(t, store, fileCollectorID, info)
	writeJSON(t, store, fileStarts, []Session{{StartedAt: day0, LastBeat: day0, BeatPeriod: "15s"}})

	h := collectHeartbeat(store, info, day0)
	if got := beatValue(t, h.String(), "silent"); got != "none" {
		t.Errorf("silent=%s on the very first line, before anything could have been collected. "+
			"Every one of those is a false alarm", got)
	}
	// But the moment each stream's own grace has passed, it IS named — the grace is a wait, not
	// an exemption.
	late := collectHeartbeat(store, info, day0.Add(25*time.Hour))
	silent := beatValue(t, late.String(), "silent")
	for _, dead := range []string{StreamMetrics, StreamState, StreamSelfcheck, StreamHighwater} {
		if !strings.Contains(silent, dead) {
			t.Errorf("%q is still not named after 25 hours of nothing: silent=%s", dead, silent)
		}
	}
}

// TestTheHeartbeatFiresOnceAtStartupAndThenOncePerDay.
//
// The cadence IS the feature: no line for two days means the collector is not running, and that
// inference only holds if the period is predictable and if nothing else is emitted between. A
// beat per round — the loop ticks every 15s — would be 5,760 lines a day and would bury the very
// thing it is for.
func TestTheHeartbeatFiresOnceAtStartupAndThenOncePerDay(t *testing.T) {
	store := seedCollector(t, 1, true)
	var out bytes.Buffer
	c := collectorFor(t, store, &out)

	// Startup.
	c.heartbeat(day0)
	if got := strings.Count(out.String(), heartbeatPrefix); got != 1 {
		t.Fatalf("%d line(s) at startup, want exactly 1:\n%s", got, out.String())
	}

	// A day of rounds at the collector's real tick. Not one more line.
	for at := day0; at.Before(day0.Add(heartbeatInterval)); at = at.Add(15 * time.Second) {
		c.round(t.Context(), at)
	}
	if got := strings.Count(out.String(), heartbeatPrefix); got != 1 {
		t.Errorf("%d line(s) after a day of 15s rounds, want 1. A heartbeat per round is %d lines "+
			"a day and buries the signal it exists to be", got, int(heartbeatInterval/(15*time.Second)))
	}

	// And exactly one more when the day is up.
	c.round(t.Context(), day0.Add(heartbeatInterval))
	if got := strings.Count(out.String(), heartbeatPrefix); got != 2 {
		t.Errorf("%d line(s) after 24h, want 2", got)
	}
	// A second day, one more line, and no drift: the next is armed from the one just written.
	c.round(t.Context(), day0.Add(heartbeatInterval+time.Second))
	if got := strings.Count(out.String(), heartbeatPrefix); got != 2 {
		t.Errorf("%d line(s) one second later, want 2", got)
	}
	c.round(t.Context(), day0.Add(2*heartbeatInterval+time.Second))
	if got := strings.Count(out.String(), heartbeatPrefix); got != 3 {
		t.Errorf("%d line(s) on day 3, want 3", got)
	}
}

// TestRunEmitsTheStartupLineBeforeTheFirstTick. Without it, an administrator who has just
// applied the manifest has nothing to look at for 24 hours — and the day-one check
// hack/soak/README.md asks them to make is the single step most worth not skipping. The loop's
// first tick is a whole interval away, so the line cannot wait for it.
func TestRunEmitsTheStartupLineBeforeTheFirstTick(t *testing.T) {
	store := seedCollector(t, 1, true)
	var out bytes.Buffer
	c := collectorFor(t, store, &out)

	ctx, cancel := context.WithCancel(t.Context())
	cancel() // Run sets everything up, then returns on the first select.
	if err := c.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := strings.Count(out.String(), heartbeatPrefix); got != 1 {
		t.Errorf("%d heartbeat line(s) from a collector that started and stopped, want 1. An "+
			"administrator applying the manifest has to see one within seconds, not in a "+
			"day:\n%s", got, out.String())
	}
}

// TestTheLineIsStableBetweenDays: two beats over an unchanged volume differ only in their
// timestamp and their day. That is what makes a week of them readable at a glance — a field that
// reordered itself would make every day look like a change.
func TestTheLineIsStableBetweenDays(t *testing.T) {
	store := seedCollector(t, 2, true)
	info, err := ReadCollectorInfo(store.Dir())
	if err != nil {
		t.Fatal(err)
	}
	a := collectHeartbeat(store, info, day0.AddDate(0, 0, 2)).String()
	b := collectHeartbeat(store, info, day0.AddDate(0, 0, 2)).String()
	if a != b {
		t.Errorf("two beats over the same volume differ:\n%s\n%s", a, b)
	}
}

// marksFixture builds a Marks through the REAL aggregation — a HighWater fed a fake lister — and
// not by hand-filling the struct.
//
// A hand-built fixture is how the first version of this helper produced a Marks with pods and an
// empty Classes map, a shape the collector cannot emit. Tests that assert on the report would
// then be asserting against a file no cluster ever writes. Everything downstream (per-class
// counts, the report's §5 section, the heartbeat breakdown) reads Classes, so the fixture has to
// come from the code that fills it.
func marksFixture(t *testing.T, jobs []batchv1.Job, pods []corev1.Pod, now time.Time) Marks {
	t.Helper()
	h := NewHighWater(newTestStore(t, 1<<20),
		&fakeLister{jobs: jobs, pods: pods, metrics: []podMetric{}},
		testNS, 15*time.Second, false)
	h.Sample(t.Context(), now)
	return h.Marks(now)
}

// writeMarksFixture lays the table down as marks.json the way the high-water stream does, so the
// reader under test goes through disk — a field that never survives the round trip is a field
// that does not exist as far as an archive is concerned.
func writeMarksFixture(t *testing.T, store *Store, m Marks) {
	t.Helper()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.Dir(), fileMarks), raw, 0o600); err != nil {
		t.Fatalf("write marks fixture: %v", err)
	}
}
