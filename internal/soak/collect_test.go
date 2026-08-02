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
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/CrystalBackup/CrystalBackup/internal/mover"
)

// TestTheLoopActuallyCollects is the whole-package version of the failure everything else here is
// about: code that compiles, starts, beats its heartbeat and writes nothing.
//
// Every other test in this package exercises one component in isolation, and every one of them
// would still pass against a `round` whose body had been deleted. This one drives the real loop
// against a real HTTP endpoint and a faked cluster, and then looks at the volume.
func TestTheLoopActuallyCollects(t *testing.T) {
	exposition := "# TYPE " + "crystalbackup_repository_size_bytes" + " gauge\n" +
		`crystalbackup_repository_size_bytes{location="primary"} 1024` + "\n" +
		"# TYPE crystalbackup_schedule_period_seconds gauge\n" +
		`crystalbackup_schedule_period_seconds{schedule="nightly"} 86400` + "\n"
	scrapes := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		scrapes++
		_, _ = w.Write([]byte(exposition))
	}))
	defer srv.Close()

	store := newTestStore(t, 1<<20)
	sessions, err := OpenSessionLog(store, day0, 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	scraper := NewScraper(srv.URL, true)
	scraper.TokenPath = ""

	lister := &fakeLister{
		jobs:    []batchv1.Job{moverJob("job-backup", mover.OpBackup, day0, time.Time{})},
		pods:    []corev1.Pod{moverPod("job-backup-1", "job-backup", "worker-1", day0)},
		metrics: []podMetric{{Name: "job-backup-1", MemoryUsed: 512 << 20}},
	}

	c := &Collector{
		Store: store, Sessions: sessions,
		Scraper:         scraper,
		Aggregator:      NewAggregator(5 * time.Minute),
		HighWater:       NewHighWater(store, lister, testNS, 15*time.Second, false),
		MetricsInterval: time.Minute, MoverInterval: 15 * time.Second,
		StateInterval: time.Hour,
		Progress:      &bytes.Buffer{},
	}
	c.Aggregator.Start(day0)
	c.nextMetrics, c.nextMover = day0, day0
	c.nextState, c.nextEvents, c.nextLogs = day0, day0, day0

	// Six minutes at one round a minute: five scrapes inside the first five-minute window, then
	// one more that closes it.
	for i := range 7 {
		c.round(t.Context(), day0.Add(time.Duration(i)*time.Minute))
	}

	if scrapes < 5 {
		t.Errorf("%d scrape(s) in six simulated minutes at a one-minute interval, want at least 5", scrapes)
	}

	// The window closed and BOTH families reached the disk, in their own directories, so the cap
	// can sacrifice one and keep the other.
	if got := len(store.Days(streamMetricsCore)); got != 1 {
		t.Errorf("%d core metrics segment(s), want 1: §3b's series never reached the volume", got)
	}
	if got := len(store.Days(StreamMetrics)); got != 1 {
		t.Errorf("%d bulk metrics segment(s), want 1", got)
	}
	lines, err := readNDJSONGz(store.segmentPath(streamMetricsCore, day0))
	if err != nil {
		t.Fatal(err)
	}
	var sawSize, sawHealth bool
	for _, p := range decodePoints(t, lines) {
		if p.Name == "crystalbackup_repository_size_bytes" && p.N >= 5 {
			sawSize = true
		}
	}
	if h := healthOf(t, lines); h.OK >= 5 && h.Fail == 0 {
		sawHealth = true
	}
	if !sawSize {
		t.Error("the repository-size series was not written with its five samples folded into one point")
	}
	if !sawHealth {
		t.Error("the window carries no scrape-health record")
	}

	// The high-water marks were taken and persisted, so a restart would not lose them.
	marks, err := readFile(store.Dir(), fileMarks)
	if err != nil {
		t.Fatalf("the loop never wrote the high-water marks: %v", err)
	}
	if len(marks) == 0 {
		t.Error("the high-water file is empty")
	}

	// And the heartbeat is fresh at the last round's clock, which is what keeps the liveness
	// probe from restarting a working collector.
	if CheckHeartbeat(store.Dir(), day0.Add(6*time.Minute)) != 0 {
		t.Error("the loop did not beat: the liveness probe would restart a collector that is working")
	}
}

// TestTheLoopRecordsAScrapeItCouldNotMake, end to end. A hole in the metrics with no explanation
// reads as "the operator emitted nothing".
func TestTheLoopRecordsAScrapeItCouldNotMake(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("Unauthorized"))
	}))
	defer srv.Close()

	store := newTestStore(t, 1<<20)
	sessions, err := OpenSessionLog(store, day0, 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	scraper := NewScraper(srv.URL, true)
	scraper.TokenPath = ""

	c := &Collector{
		Store: store, Sessions: sessions, Scraper: scraper,
		Aggregator:      NewAggregator(5 * time.Minute),
		MetricsInterval: time.Minute, MoverInterval: time.Hour, StateInterval: time.Hour,
		Progress: &bytes.Buffer{},
	}
	c.Aggregator.Start(day0)
	for i := range 7 {
		c.round(t.Context(), day0.Add(time.Duration(i)*time.Minute))
	}

	errs := store.ErrorsFor(StreamMetrics)
	if len(errs) != 1 {
		t.Fatalf("%d error record(s), want 1", len(errs))
	}
	if errs[0].Count < 5 {
		t.Errorf("count = %d, want at least 5: how long the 401 went on is the finding", errs[0].Count)
	}

	// The window still closed, and it closed SAYING the scrapes failed rather than looking like a
	// period in which the operator exposed nothing.
	lines, err := readNDJSONGz(store.segmentPath(streamMetricsCore, day0))
	if err != nil {
		t.Fatalf("no window was written for a period of total scrape failure: %v", err)
	}
	h := healthOf(t, lines)
	if h.OK != 0 || h.Fail < 5 {
		t.Errorf("scrape health = %+v, want ok=0 and fail>=5", h)
	}
}
