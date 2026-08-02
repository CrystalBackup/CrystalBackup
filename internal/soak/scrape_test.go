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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"

	"github.com/CrystalBackup/CrystalBackup/internal/metrics"
)

func parseExposition(t *testing.T, body string) map[string]*dto.MetricFamily {
	t.Helper()
	p := expfmt.NewTextParser(model.UTF8Validation)
	fams, err := p.TextToMetricFamilies(strings.NewReader(body))
	if err != nil {
		t.Fatalf("the fixture exposition does not parse: %v", err)
	}
	return fams
}

func decodePoints(t *testing.T, lines [][]byte) []point {
	t.Helper()
	out := make([]point, 0, len(lines))
	for _, l := range lines {
		var probe struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(l, &probe) == nil && probe.Name == scrapeHealthName {
			continue
		}
		var p point
		if err := json.Unmarshal(l, &p); err != nil {
			t.Fatalf("a stored line does not decode: %v (%s)", err, l)
		}
		out = append(out, p)
	}
	return out
}

func healthOf(t *testing.T, lines [][]byte) scrapeHealth {
	t.Helper()
	for _, l := range lines {
		var h scrapeHealth
		if json.Unmarshal(l, &h) == nil && h.Name == scrapeHealthName {
			return h
		}
	}
	t.Fatal("no scrape-health record in the window: a hole with no explanation reads as \"the " +
		"operator emitted nothing\"")
	return scrapeHealth{}
}

// TestDownsamplingKeepsTheSpike is §3. A queue depth that touched the concurrency limit for four
// minutes is invisible in a five-minute `last`, and that spike is a finding.
func TestDownsamplingKeepsTheSpike(t *testing.T) {
	a := NewAggregator(5 * time.Minute)
	base := day0.Add(time.Hour)
	a.Start(base)

	for i, v := range []float64{1, 8, 8, 8, 2} {
		a.Observe(parseExposition(t, "# TYPE q gauge\nq{location=\"primary\"} "+
			itoa(v)+"\n"), base.Add(time.Duration(i)*time.Minute))
	}
	core, bulk, day := a.Flush()
	if !day.Equal(base.Truncate(5 * time.Minute)) {
		t.Errorf("window start = %s, want %s", day, base.Truncate(5*time.Minute))
	}
	pts := decodePoints(t, append(core, bulk...))
	if len(pts) != 1 {
		t.Fatalf("%d point(s), want 1", len(pts))
	}
	p := pts[0]
	if p.Last != 2 {
		t.Errorf("last = %v, want 2", p.Last)
	}
	if p.Min != 1 || p.Max != 8 {
		t.Errorf("min/max = %v/%v, want 1/8: the four-minute spike is the finding and `last` "+
			"cannot see it", p.Min, p.Max)
	}
	if p.N != 5 {
		t.Errorf("n = %d, want 5", p.N)
	}
	if p.Labels["location"] != "primary" {
		t.Errorf("the label set was not carried in the line: %+v", p.Labels)
	}
	h := healthOf(t, core)
	if h.OK != 5 || h.Fail != 0 {
		t.Errorf("scrape health = %+v, want ok=5 fail=0", h)
	}
}

// TestAScrapeFailureIsAWindowThatSaysSo. A window in which every scrape failed says nothing about
// which series exist, and marking them all absent would manufacture a cluster-wide event out of a
// network blip.
func TestAScrapeFailureIsAWindowThatSaysSo(t *testing.T) {
	a := NewAggregator(5 * time.Minute)
	base := day0.Add(time.Hour)
	a.Start(base)

	a.Observe(parseExposition(t, "# TYPE up gauge\nup 1\n"), base)
	a.ObserveFailure()
	a.ObserveFailure()
	core, _, _ := a.Flush()
	if h := healthOf(t, core); h.OK != 1 || h.Fail != 2 {
		t.Errorf("scrape health = %+v, want ok=1 fail=2", h)
	}
	a.Advance(base.Add(5 * time.Minute))

	// A window with NOTHING but failures must not report the series as having disappeared.
	a.ObserveFailure()
	a.ObserveFailure()
	core2, bulk2, _ := a.Flush()
	for _, p := range decodePoints(t, append(core2, bulk2...)) {
		if p.Absent {
			t.Errorf("series %q was marked ABSENT in a window where every scrape failed: that is a "+
				"statement about the cluster derived from a fact about the network", p.Name)
		}
	}
	if h := healthOf(t, core2); h.OK != 0 || h.Fail != 2 {
		t.Errorf("scrape health = %+v, want ok=0 fail=2", h)
	}
}

// TestAbsentIsNotZero is the distinction internal/metrics/names.go documents an alert dying of: a
// CounterVec child only materialises on its first Inc(), so after a restart the series is ABSENT
// rather than reset, and increase() cannot see across a disappearance the way it sees across a
// reset. An export that smoothed that over would recreate the bug in the data.
func TestAbsentIsNotZero(t *testing.T) {
	a := NewAggregator(time.Minute)
	base := day0.Add(time.Hour)
	a.Start(base)

	a.Observe(parseExposition(t,
		"# TYPE f counter\nf{namespace=\"prod\"} 3\nf{namespace=\"staging\"} 1\n"), base)
	a.Flush()
	a.Advance(base.Add(time.Minute))

	// staging is gone — the namespace was deleted.
	a.Observe(parseExposition(t, "# TYPE f counter\nf{namespace=\"prod\"} 4\n"), base.Add(time.Minute))
	core, bulk, _ := a.Flush()

	var sawAbsent, sawZero bool
	for _, p := range decodePoints(t, append(core, bulk...)) {
		if p.Labels["namespace"] != "staging" {
			continue
		}
		if p.Absent {
			sawAbsent = true
		}
		if !p.Absent && p.Last == 0 {
			sawZero = true
		}
	}
	if !sawAbsent {
		t.Error("a series that stopped being exposed was not recorded as absent")
	}
	if sawZero {
		t.Error("the disappeared series was recorded as zero. \"0\" and \"not there\" are different " +
			"facts and this project has an alert that could not fire because of exactly that confusion")
	}

	// One marker per absence RUN, not one per window: the marker plus the next present sample
	// bound the gap exactly, and repeating it every window for a namespace deleted on day three
	// would cost more than the series it describes.
	a.Advance(base.Add(2 * time.Minute))
	a.Observe(parseExposition(t, "# TYPE f counter\nf{namespace=\"prod\"} 5\n"), base.Add(2*time.Minute))
	core2, bulk2, _ := a.Flush()
	for _, p := range decodePoints(t, append(core2, bulk2...)) {
		if p.Labels["namespace"] == "staging" {
			t.Errorf("the absence marker repeated in the next window too: %+v", p)
		}
	}
}

// TestCoreSeriesAreSplitOffForTheCap is §3b. The series worth a fortnight go into the family the
// cap sacrifices LAST.
func TestCoreSeriesAreSplitOffForTheCap(t *testing.T) {
	a := NewAggregator(time.Minute)
	base := day0.Add(time.Hour)
	a.Start(base)
	a.Observe(parseExposition(t, strings.Join([]string{
		"# TYPE " + metrics.NameRepositorySize + " gauge",
		metrics.NameRepositorySize + `{location="primary"} 1024`,
		"# TYPE " + metrics.NameSchedulePeriod + " gauge",
		metrics.NameSchedulePeriod + `{schedule="nightly"} 86400`,
		"# TYPE " + metrics.NameBackupDuration + " histogram",
		metrics.NameBackupDuration + `_bucket{le="1"} 0`,
		metrics.NameBackupDuration + `_bucket{le="+Inf"} 3`,
		metrics.NameBackupDuration + `_sum 42`,
		metrics.NameBackupDuration + `_count 3`,
		"",
	}, "\n")), base)
	core, bulk, _ := a.Flush()

	coreNames := map[string]bool{}
	for _, p := range decodePoints(t, core) {
		coreNames[p.Name] = true
	}
	if !coreNames[metrics.NameRepositorySize] {
		t.Error("repository_size_bytes is not in the core family; it is the slowest signal there is " +
			"and the one no other test can produce")
	}
	// The histogram's buckets follow the family name it was listed under, or a p95 that moved
	// over ten days would be dropped while its _sum survived.
	if !coreNames[metrics.NameBackupDuration+"_bucket"] {
		t.Error("the duration histogram's buckets are not in the core family")
	}
	for _, p := range decodePoints(t, bulk) {
		if p.Name == metrics.NameSchedulePeriod {
			return
		}
	}
	t.Error("schedule_period_seconds is not in the bulk family; §3b deliberately excludes the " +
		"static configuration surface, which every daily self-check carries already")
}

// TestHistogramsAndSummariesExplode: the exposure histogram is where real CSI latency under real
// contention shows up, and it does not exist as a single value anywhere in the protobuf model.
func TestHistogramsAndSummariesExplode(t *testing.T) {
	fams := parseExposition(t, strings.Join([]string{
		"# TYPE h histogram",
		`h_bucket{le="1"} 1`,
		`h_bucket{le="+Inf"} 4`,
		"h_sum 10",
		"h_count 4",
		"# TYPE s summary",
		`s{quantile="0.5"} 2`,
		"s_sum 6",
		"s_count 3",
		"",
	}, "\n"))
	got := map[string]bool{}
	for _, fam := range fams {
		for _, m := range fam.GetMetric() {
			for _, s := range explode(fam, m) {
				key := s.name
				if q, ok := s.labels["quantile"]; ok {
					key += "{quantile=" + q + "}"
				}
				if le, ok := s.labels["le"]; ok {
					key += "{le=" + le + "}"
				}
				got[key] = true
			}
		}
	}
	for _, want := range []string{
		"h_bucket{le=1}", "h_bucket{le=+Inf}", "h_sum", "h_count",
		"s{quantile=0.5}", "s_sum", "s_count",
	} {
		if !got[want] {
			t.Errorf("%s was not produced; got %v", want, keysOf(got))
		}
	}
}

// TestScrapeReadsTheTokenEveryTime is the day-3 401.
//
// The projected ServiceAccount token is bound to the pod and rotated by the kubelet. A collector
// that read it once at startup would authenticate perfectly for a few hours and then fail every
// minute for the remaining thirteen days, producing an archive with one day of metrics and
// thirteen of silence.
func TestScrapeReadsTheTokenEveryTime(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Authorization"))
		_, _ = w.Write([]byte("# TYPE up gauge\nup 1\n"))
	}))
	defer srv.Close()

	tokenPath := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenPath, []byte("first-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewScraper(srv.URL, true)
	s.TokenPath = tokenPath

	if _, err := s.Scrape(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenPath, []byte("rotated-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Scrape(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 {
		t.Fatalf("%d request(s), want 2", len(seen))
	}
	if seen[0] != "Bearer first-token" {
		t.Errorf("first Authorization = %q", seen[0])
	}
	if seen[1] != "Bearer rotated-token" {
		t.Errorf("second Authorization = %q: the token was cached and this collector would 401 on "+
			"day 3 of 14", seen[1])
	}
}

// TestScrapeSurfacesTheStatusAndTheBody: a 401 from an expired token and a 403 from an unbound
// crystal-backup-metrics-reader are one ClusterRoleBinding apart and look identical without the
// body.
func TestScrapeSurfacesTheStatusAndTheBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("forbidden: User cannot get path /metrics"))
	}))
	defer srv.Close()

	s := NewScraper(srv.URL, true)
	s.TokenPath = ""
	_, err := s.Scrape(t.Context())
	if err == nil {
		t.Fatal("a 403 was not an error")
	}
	for _, want := range []string{"403", "/metrics"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %q: %v", want, err)
		}
	}
}

func TestSeriesKeyDistinguishesLabelSets(t *testing.T) {
	a := sample{name: "m", labels: map[string]string{"a": "1", "b": "2"}}
	b := sample{name: "m", labels: map[string]string{"b": "2", "a": "1"}}
	if a.key() != b.key() {
		t.Error("label order changed the series identity; map iteration would then split one series " +
			"into many")
	}
	// A label value can contain anything, so the encoding must be injective rather than rely on a
	// separator byte. These two collided under the first implementation, which would have merged
	// two series into one line whose min and max spanned both of them — silently, for a fortnight.
	for _, tc := range []struct{ c, d sample }{
		{
			sample{name: "m", labels: map[string]string{"a": "1\x00b\x002"}},
			sample{name: "m", labels: map[string]string{"a": "1", "b": "2"}},
		},
		{
			sample{name: "m", labels: map[string]string{"a": "1:b1:2"}},
			sample{name: "m", labels: map[string]string{"a": "1", "b": "2"}},
		},
		{
			sample{name: "ma", labels: map[string]string{"b": "1"}},
			sample{name: "m", labels: map[string]string{"ab": "1"}},
		},
	} {
		if tc.c.key() == tc.d.key() {
			t.Errorf("two different series collided into one key: %+v and %+v", tc.c.labels, tc.d.labels)
		}
	}
}

func itoa(f float64) string { return strconv.FormatFloat(f, 'g', -1, 64) }

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
