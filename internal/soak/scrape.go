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
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"

	"github.com/CrystalBackup/CrystalBackup/internal/metrics"
)

// serviceAccountTokenPath is the projected token the collector authenticates its scrape with.
//
// It is read PER SCRAPE and never cached. The projected token is bound to the pod and rotated by
// the kubelet — typically hourly, always well inside a fortnight — and a collector that read it
// once at startup would authenticate perfectly for a few hours and then 401 every minute for the
// remaining thirteen days, producing an archive with one day of metrics and thirteen of silence.
// Re-reading a small file once a minute costs nothing measurable.
const serviceAccountTokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token" // #nosec G101

// scrapeTimeout bounds one scrape. Generous against a 60s default interval, because a metrics
// endpoint that takes twenty seconds under load is a FINDING and cutting it off at two would
// record it as an outage instead.
const scrapeTimeout = 30 * time.Second

// Scraper reads the operator's /metrics.
//
// Scraping the operator DIRECTLY is what removes Prometheus from this whole kit, and that is the
// single largest adoption difference in the design: the soak now works on a cluster whose
// monitoring stack keeps 24 hours of history, or none at all.
type Scraper struct {
	URL       string
	TokenPath string
	Client    *http.Client
}

// NewScraper builds the scrape client.
//
// insecureSkipVerify is a FLAG and not an implicit fallback. The metrics server presents a
// self-signed certificate (the chart requires no cert-manager and its own ServiceMonitor sets
// insecureSkipVerify for the same reason), so it will normally be on — but a cluster that has
// wired a real certificate must be able to turn it off, and a collector that silently downgraded
// from a verification failure to no verification would take that choice away from them without
// telling them.
func NewScraper(url string, insecureSkipVerify bool) *Scraper {
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{
			MinVersion:         tls.VersionTLS12,
			InsecureSkipVerify: insecureSkipVerify, // #nosec G402 -- operator-selected, see above
		},
	}
	return &Scraper{
		URL:       url,
		TokenPath: serviceAccountTokenPath,
		Client:    &http.Client{Transport: tr, Timeout: scrapeTimeout},
	}
}

// Scrape fetches and parses one exposition.
//
// Parsing goes through expfmt, which client_golang already pulls in. Writing a text-format parser
// here would be a second implementation of a format with escaping rules, exemplars and a
// histogram encoding — and its bugs would look exactly like the operator emitting bad data.
func (s *Scraper) Scrape(ctx context.Context) (map[string]*dto.MetricFamily, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.URL, nil)
	if err != nil {
		return nil, err
	}
	if s.TokenPath != "" {
		token, err := os.ReadFile(s.TokenPath) // #nosec G304 -- the pod's own projected token
		if err == nil {
			req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
		} else if !os.IsNotExist(err) {
			return nil, fmt.Errorf("read the ServiceAccount token: %w", err)
		}
	}
	req.Header.Set("Accept", string(expfmt.NewFormat(expfmt.TypeTextPlain)))
	resp, err := s.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		// The body is included because the two failures that actually happen here say which one
		// they are in it: a 401 from an expired token and a 403 from an unbound
		// crystal-backup-metrics-reader are one ClusterRoleBinding apart and look identical
		// otherwise.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("scrape %s: HTTP %d: %s", s.URL, resp.StatusCode,
			strings.TrimSpace(string(body)))
	}
	// The validation scheme is passed EXPLICITLY. A zero-valued TextParser carries
	// model.UnsetValidation and PANICS on the first metric name it sees — client_golang sets the
	// package global in its own init, and this subcommand parses an exposition without ever
	// registering a collector, so it does not inherit that. UTF8 rather than legacy because the
	// collector's job is to record what the operator emitted, not to have an opinion about it.
	parser := expfmt.NewTextParser(model.UTF8Validation)
	families, err := parser.TextToMetricFamilies(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("parse the exposition: %w", err)
	}
	return families, nil
}

// ---------------------------------------------------------------------------------------------
// Downsampling
// ---------------------------------------------------------------------------------------------

// point is one series in one resolution window, and it is the ONLY thing that reaches the disk.
// Raw scrapes are never kept: one exposition a minute for fourteen days is millions of lines and
// would blow the cap on day two.
//
// min and max are not decoration. A queue depth that touched the concurrency limit for four
// minutes is invisible in a five-minute `last`, and that spike is a finding. For counters `last`
// is the only meaningful field and the other two cost nothing.
type point struct {
	T      int64             `json:"t"`
	Name   string            `json:"name"`
	Labels map[string]string `json:"labels,omitempty"`
	Last   float64           `json:"last"`
	Min    float64           `json:"min"`
	Max    float64           `json:"max"`
	N      int               `json:"n"`
	// Absent marks the window in which a series STOPPED being exposed. It carries no value, on
	// purpose: the difference between "0" and "not there" is load-bearing in this project —
	// internal/metrics/names.go documents the alert that could not fire because a CounterVec
	// child was absent rather than zero after a restart — and an archive that smoothed it over
	// would recreate that bug in the data.
	//
	// One marker per absence RUN, not one per window. The marker plus the next present sample
	// bound the gap exactly, and repeating it every five minutes for a namespace deleted on day
	// three would cost more than the series it is describing.
	Absent bool `json:"absent,omitempty"`
}

// scrapeHealth is the per-window record of whether the scrape itself worked.
//
// Without it a window with no data is ambiguous between "the operator exposed nothing" and "the
// collector could not reach it", and those two readings of the same hole lead to opposite
// conclusions about the cluster. It also stops a failed scrape from being read as every series
// in the exposition disappearing at once.
type scrapeHealth struct {
	T    int64  `json:"t"`
	Name string `json:"name"`
	OK   int    `json:"ok"`
	Fail int    `json:"fail"`
}

// scrapeHealthName is not a crystalbackup_ series and is prefixed so it can never collide with
// one.
const scrapeHealthName = "__collector_scrape__"

// Aggregator reduces a stream of scrapes to one point per series per resolution window.
type Aggregator struct {
	resolution time.Duration

	windowStart time.Time
	cur         map[string]*point
	// present is the set of series seen in the last window that FLUSHED with at least one
	// successful scrape. It is what absence is measured against, and it carries each series'
	// identity rather than only its key so an absence marker is written from what was OBSERVED
	// rather than parsed back out of a string.
	present map[string]seriesID
	// absent is the set already marked absent, so the marker is emitted once per run.
	absent map[string]bool

	ok, fail int
}

// NewAggregator builds the downsampler. A non-positive resolution is refused by the flag parser,
// so this does not have to defend against it.
func NewAggregator(resolution time.Duration) *Aggregator {
	return &Aggregator{
		resolution: resolution,
		cur:        map[string]*point{},
		present:    map[string]seriesID{},
		absent:     map[string]bool{},
	}
}

// WindowStart is the start of the window a timestamp falls in, truncated in UTC so a window
// boundary is the same instant on every cluster.
func (a *Aggregator) WindowStart(t time.Time) time.Time {
	return t.UTC().Truncate(a.resolution)
}

// Observe folds one successful scrape in.
func (a *Aggregator) Observe(families map[string]*dto.MetricFamily, now time.Time) {
	a.ok++
	for _, fam := range families {
		for _, m := range fam.GetMetric() {
			for _, sample := range explode(fam, m) {
				a.fold(sample, now)
			}
		}
	}
}

// ObserveFailure records that a scrape did not happen. The window still flushes; it flushes
// SAYING so.
func (a *Aggregator) ObserveFailure() { a.fail++ }

func (a *Aggregator) fold(s sample, now time.Time) {
	key := s.key()
	p, ok := a.cur[key]
	if !ok {
		p = &point{
			T: a.WindowStart(now).Unix(), Name: s.name, Labels: s.labels,
			Min: s.value, Max: s.value,
		}
		a.cur[key] = p
	}
	p.Last = s.value
	p.N++
	if s.value < p.Min {
		p.Min = s.value
	}
	if s.value > p.Max {
		p.Max = s.value
	}
}

// Due reports whether the window a timestamp falls in is later than the one being accumulated.
func (a *Aggregator) Due(now time.Time) bool {
	if a.windowStart.IsZero() {
		return false
	}
	return a.WindowStart(now).After(a.windowStart)
}

// Start opens the first window. Called once, on the first scrape attempt.
func (a *Aggregator) Start(now time.Time) {
	if a.windowStart.IsZero() {
		a.windowStart = a.WindowStart(now)
	}
}

// Flush closes the current window and returns its lines, split into the two families the
// footprint cap treats differently: `core` is §3b's list, which survives to the end, and `bulk`
// is everything else, which is what gets sacrificed first.
//
// The split is by NAME and not by importance-at-the-time, because the point of §3b is that the
// series worth a fortnight are the ones whose TREND says something — repository growth against
// protected bytes, a failure counter's slope, a p95 that moved — and none of those is
// recognisable from a single window.
func (a *Aggregator) Flush() (core, bulk [][]byte, day time.Time) {
	day = a.windowStart
	stamp := a.windowStart.Unix()

	keys := make([]string, 0, len(a.cur))
	for k := range a.cur {
		keys = append(keys, k)
	}
	slices.Sort(keys)

	nowPresent := make(map[string]seriesID, len(a.cur))
	for _, k := range keys {
		p := a.cur[k]
		p.T = stamp
		nowPresent[k] = seriesID{name: p.Name, labels: p.Labels}
		delete(a.absent, k)
		line, err := json.Marshal(p)
		if err != nil {
			continue
		}
		if isCoreSeries(p.Name) {
			core = append(core, line)
		} else {
			bulk = append(bulk, line)
		}
	}

	// Absence is only meaningful if the exposition was actually read. A window in which every
	// scrape failed says nothing about which series exist, and marking them all absent would
	// manufacture a cluster-wide event out of a network blip.
	if a.ok > 0 {
		absentKeys := make([]string, 0)
		for k := range a.present {
			if _, still := nowPresent[k]; !still && !a.absent[k] {
				absentKeys = append(absentKeys, k)
			}
		}
		slices.Sort(absentKeys)
		for _, k := range absentKeys {
			a.absent[k] = true
			id := a.present[k]
			name := id.name
			line, err := json.Marshal(point{T: stamp, Name: name, Labels: id.labels, Absent: true})
			if err != nil {
				continue
			}
			if isCoreSeries(name) {
				core = append(core, line)
			} else {
				bulk = append(bulk, line)
			}
		}
		a.present = nowPresent
	}

	if line, err := json.Marshal(scrapeHealth{
		T: stamp, Name: scrapeHealthName, OK: a.ok, Fail: a.fail,
	}); err == nil {
		// The health line goes in the CORE family. It is four numbers a day's worth of, and it is
		// what makes every hole in the bulk stream readable after the bulk stream is gone.
		core = append(core, line)
	}

	a.cur = map[string]*point{}
	a.ok, a.fail = 0, 0
	return core, bulk, day
}

// Advance moves the accumulator to the window containing now. Called after Flush.
func (a *Aggregator) Advance(now time.Time) { a.windowStart = a.WindowStart(now) }

// ---------------------------------------------------------------------------------------------
// Series identity
// ---------------------------------------------------------------------------------------------

type sample struct {
	name   string
	labels map[string]string
	value  float64
}

// seriesID is a series' identity, carried beside its key so an absence marker is written from
// what was observed rather than parsed back out of a string.
type seriesID struct {
	name   string
	labels map[string]string
}

// key is the canonical (name, labels) identity: labels sorted by name and LENGTH-PREFIXED.
//
// Length-prefixed rather than separated by a byte assumed not to occur, which was the first
// attempt and was wrong. A label VALUE is arbitrary text in the exposition format, so any
// separator can appear inside one, and with a bare NUL separator the label set
// {a: "1\x00b\x002"} produced exactly the same key as {a: "1", b: "2"}.
//
// Two series folding into one key is not a cosmetic bug. It merges two namespaces' points into a
// single line whose min and max span both of them — silently, for a fortnight, in the stream
// whose whole value is that it can be trusted after the fact.
func (s sample) key() string {
	var b strings.Builder
	writeLenPrefixed(&b, s.name)
	names := make([]string, 0, len(s.labels))
	for k := range s.labels {
		names = append(names, k)
	}
	slices.Sort(names)
	for _, k := range names {
		writeLenPrefixed(&b, k)
		writeLenPrefixed(&b, s.labels[k])
	}
	return b.String()
}

func writeLenPrefixed(b *strings.Builder, s string) {
	b.WriteString(strconv.Itoa(len(s)))
	b.WriteByte(':')
	b.WriteString(s)
}

// explode turns one dto.Metric into the sample(s) the text format would have shown.
//
// Histograms and summaries are the reason this is not a one-liner, and the exposure histogram is
// one of the series §3b keeps: `crystalbackup_exposure_ready_wait_seconds_bucket` is where real
// CSI latency under real contention shows up, and it does not exist as a single value anywhere in
// the protobuf model.
func explode(fam *dto.MetricFamily, m *dto.Metric) []sample {
	base := fam.GetName()
	labels := map[string]string{}
	for _, lp := range m.GetLabel() {
		labels[lp.GetName()] = lp.GetValue()
	}
	clone := func(extra map[string]string) map[string]string {
		out := make(map[string]string, len(labels)+len(extra))
		maps.Copy(out, labels)
		maps.Copy(out, extra)
		return out
	}
	switch fam.GetType() {
	case dto.MetricType_COUNTER:
		return []sample{{name: base, labels: labels, value: m.GetCounter().GetValue()}}
	case dto.MetricType_GAUGE:
		return []sample{{name: base, labels: labels, value: m.GetGauge().GetValue()}}
	case dto.MetricType_UNTYPED:
		return []sample{{name: base, labels: labels, value: m.GetUntyped().GetValue()}}
	case dto.MetricType_HISTOGRAM:
		h := m.GetHistogram()
		out := make([]sample, 0, len(h.GetBucket())+2)
		for _, b := range h.GetBucket() {
			out = append(out, sample{
				name:   base + "_bucket",
				labels: clone(map[string]string{"le": strconv.FormatFloat(b.GetUpperBound(), 'g', -1, 64)}),
				value:  float64(b.GetCumulativeCount()),
			})
		}
		out = append(out,
			sample{name: base + "_sum", labels: labels, value: h.GetSampleSum()},
			sample{name: base + "_count", labels: labels, value: float64(h.GetSampleCount())})
		return out
	case dto.MetricType_SUMMARY:
		sm := m.GetSummary()
		out := make([]sample, 0, len(sm.GetQuantile())+2)
		for _, q := range sm.GetQuantile() {
			out = append(out, sample{
				name:   base,
				labels: clone(map[string]string{"quantile": strconv.FormatFloat(q.GetQuantile(), 'g', -1, 64)}),
				value:  q.GetValue(),
			})
		}
		out = append(out,
			sample{name: base + "_sum", labels: labels, value: sm.GetSampleSum()},
			sample{name: base + "_count", labels: labels, value: float64(sm.GetSampleCount())})
		return out
	case dto.MetricType_GAUGE_HISTOGRAM:
		// Not emitted by this operator. Recorded as untyped rather than dropped, because a series
		// this collector did not understand is still a series that existed.
		return []sample{{name: base, labels: labels, value: m.GetGauge().GetValue()}}
	default:
		return nil
	}
}

// ---------------------------------------------------------------------------------------------
// §3b — the series that must survive the cap
// ---------------------------------------------------------------------------------------------

// coreSeries is hack/soak/SPEC.md §3b, built from internal/metrics' constants rather than from
// string literals — the whole reason names.go exists is that a series name written twice
// eventually disagrees with itself, and a priority list that named a series the operator stopped
// emitting would silently protect nothing.
//
// They are chosen for what their TREND says over a fortnight. A soak buys exactly one thing a
// test suite cannot — the time axis — and a series whose value is only meaningful instantaneously
// wastes it. Deliberately absent: schedule_period_seconds, the created-timestamp families and the
// rest of the static configuration surface, which every daily self-check carries already, where a
// change between two days reads as a change.
var coreSeries = map[string]bool{
	// Repository growth and retention effectiveness. The slowest signal there is: size alone is
	// unreadable, and means something only against protected bytes (the denominator) and added
	// bytes (the churn that fed it). A repository growing faster than its input is the headline
	// finding of a soak and it takes a week to become visible.
	metrics.NameRepositorySize:       true,
	metrics.NameRepositorySnapshots:  true,
	metrics.NameBackupProtectedBytes: true,
	metrics.NameBackupAddedTotal:     true,

	// Maintenance, the operation that blocks windows. There is NO prune-duration series in
	// internal/metrics/names.go, so a forty-minute prune is only reconstructable as a plateau in
	// the last-maintenance timestamp followed by a step, with mover_active and mover_queue_depth
	// raised in between — which is why those keep company here. A stale lock that appears at 3am
	// and clears by 6am exists only if something was sampling.
	metrics.NameRepositoryLastPrune:    true,
	metrics.NameRepositoryLastCheck:    true,
	metrics.NameRepositoryCheckSuccess: true,
	metrics.NameRepositoryStaleLocks:   true,
	metrics.NameRepositoryLocksReaped:  true,

	// Failure counters: a counter's value says nothing, its slope over fourteen days is the
	// soak's core evidence. Paired with the consecutive-failure gauge, which is what shows the
	// transient that recovered, and with the last-success/last-failure pair, which reconstructs
	// every namespace's timeline without a single log line.
	metrics.NameBackupFailuresTotal:    true,
	metrics.NameRestoreFailuresTotal:   true,
	metrics.NameMoverJobRetriesTotal:   true,
	metrics.NameClusterBackupRunsTotal: true,
	metrics.NameExternalSyncFailures:   true,
	metrics.NameWebhookDenialsTotal:    true,
	metrics.NameBackupFailures:         true,
	metrics.NameBackupLastSuccess:      true,
	metrics.NameBackupLastFailure:      true,

	// Durations, because drift is the finding: a p95 that moves from four minutes to twenty-five
	// over ten days is precisely what vanishes without continuous capture.
	metrics.NameBackupDuration:        true,
	metrics.NameBackupLastDuration:    true,
	metrics.NameExposureReadyWait:     true,
	metrics.NameMoverActive:           true,
	metrics.NameMoverQueueDepth:       true,
	metrics.NameMoverConcurrencyLimit: true,
	metrics.NamePVCVolumeSnapshotting: true,
	metrics.NameBackupTotal:           true,
	metrics.NameScheduleActive:        true,
	metrics.NameDiscoveryProjected:    true,
	metrics.NameDiscoveryOrphans:      true,
	metrics.NameDiscoveryLastSuccess:  true,
	metrics.NameExternalSyncLag:       true,

	// Identity: the only way to know the operator restarted or was upgraded mid-soak, and the
	// thing that pins which build produced the whole window.
	metrics.NameBuildInfo: true,
}

// isCoreSeries matches a stored series name against §3b, allowing for the suffixes a histogram
// explodes into — the priority list names `crystalbackup_backup_duration_seconds` and means all
// of its buckets.
func isCoreSeries(name string) bool {
	if name == scrapeHealthName || coreSeries[name] {
		return true
	}
	for _, suffix := range []string{"_bucket", "_sum", "_count"} {
		if base, ok := strings.CutSuffix(name, suffix); ok && coreSeries[base] {
			return true
		}
	}
	return false
}
