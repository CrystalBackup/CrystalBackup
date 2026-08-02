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
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/CrystalBackup/CrystalBackup/internal/selfcheck"
)

// CollectorInfo is what soak-collect writes down about itself so soak-export — which may be a
// different invocation, days later, of a binary that was given none of the same flags — can
// describe the collection instead of guessing at it.
//
// It is also how coverage.observedDays stays computed from the DATA: the resolution a window
// represents is a fact about the process that wrote it, and without it the export would have to
// assume one, which is exactly the flag-derived number §8 forbids.
type CollectorInfo struct {
	OperatorVersion     string    `json:"operatorVersion"`
	StartedAt           time.Time `json:"startedAt"`
	OperatorNamespace   string    `json:"operatorNamespace"`
	MetricsURL          string    `json:"metricsURL"`
	MetricsInterval     string    `json:"metricsInterval"`
	MetricsResolution   string    `json:"metricsResolution"`
	MoverSampleInterval string    `json:"moverSampleInterval"`
	SelfcheckInterval   string    `json:"selfcheckInterval"`
	StateInterval       string    `json:"stateInterval"`
	KubeletStats        bool      `json:"kubeletStats"`
	SaltFile            string    `json:"redactionSaltFile"`
	MaxBytes            int64     `json:"maxBytes"`
	SelfcheckEnabled    bool      `json:"selfcheckEnabled"`
}

func (c CollectorInfo) resolution() time.Duration {
	d, err := time.ParseDuration(c.MetricsResolution)
	if err != nil || d <= 0 {
		return 5 * time.Minute
	}
	return d
}

func (c CollectorInfo) stateInterval() time.Duration {
	d, err := time.ParseDuration(c.StateInterval)
	if err != nil || d <= 0 {
		return time.Hour
	}
	return d
}

func (c CollectorInfo) selfcheckInterval() time.Duration {
	d, err := time.ParseDuration(c.SelfcheckInterval)
	if err != nil || d <= 0 {
		return 24 * time.Hour
	}
	return d
}

// ReadCollectorInfo loads what the collector recorded about itself.
func ReadCollectorInfo(dir string) (CollectorInfo, error) {
	var info CollectorInfo
	raw, err := readFile(dir, fileCollectorID)
	if err != nil {
		return info, err
	}
	return info, json.Unmarshal(raw, &info)
}

// ExportOptions configure one export.
type ExportOptions struct {
	DataDir string
	Salt    []byte
	Full    bool
	// Since bounds the archive. Zero means everything.
	Since time.Duration
	Now   time.Time
}

// Export writes ONE tar.gz to w.
//
// To stdout, never to a file, and nothing else may ever be written to it. That is what makes
// `kubectl exec … > archive.tar.gz` work against a distroless image with no shell, no tar and no
// kubectl to cp with — and it is why every progress line in this package goes to stderr. A single
// stray Println here turns a valid gzip stream into a file that will not unpack, which
// collect.sh detects and reports as NOT COLLECTED with the first bytes quoted, because that is
// the only way anyone would ever work out what happened.
//
// It ALWAYS writes an archive, even when every stream is empty. The evidence of nothing is
// evidence, and it has to arrive as a file with a manifest that says so — not as a non-zero exit
// and no artefact, which is indistinguishable from the command not existing.
func Export(opts ExportOptions, w io.Writer, progress io.Writer) error {
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()

	red, err := buildRedactor(opts)
	if err != nil {
		return err
	}
	info, infoErr := ReadCollectorInfo(opts.DataDir)
	uptime, uptimeErr := ReadUptime(opts.DataDir, now)

	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	e := &exporter{
		opts: opts, now: now, red: red, info: info, uptime: uptime, tw: tw,
		progress: progress,
	}
	if infoErr != nil {
		e.problems = append(e.problems, "the collector never recorded its own configuration: "+
			infoErr.Error())
	}
	if uptimeErr != nil {
		e.problems = append(e.problems, "the collector's session log could not be read: "+
			uptimeErr.Error())
	}

	e.learnIdentifiers()
	e.writeMetrics()
	e.writeState()
	e.writeHighwater()
	e.writeSelfchecks()
	e.writeEvents()
	e.writeLogs()
	e.writeCollectorFiles()
	if err := e.writeManifest(); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

// buildRedactor is the one place the salt is turned into a redactor, and the one place --full is
// honoured.
//
// A hashed export with NO salt is refused. It would produce an archive whose tokens correlate
// with nothing the admin holds — collect.sh's token check would report FAILED, the streams would
// not cross-reference, and the whole reason the salt is stable would be gone. Refusing is loud;
// producing it would be a quiet waste of a fortnight.
func buildRedactor(opts ExportOptions) (*selfcheck.Redactor, error) {
	if opts.Full {
		return selfcheck.NewRedactor(true, nil)
	}
	if len(opts.Salt) == 0 {
		return nil, fmt.Errorf(
			"no redaction salt: pass --redaction-salt-file (the collector's manifest mounts it at " +
				"/etc/crystal-backup/soak/salt), or pass --full to export identifiers verbatim on purpose")
	}
	return selfcheck.NewRedactor(false, opts.Salt)
}

// learnIdentifiers is the pre-pass, and it is the difference between an archive that is redacted
// and one that is nearly redacted.
//
// Every identifier the collector saw over the fortnight has to be a substitution rule BEFORE any
// free text reaches Detail(). Learning as each stream is emitted is not enough and was not: the
// CR-state stream is written before the events, and a PVC whose name appears only in an event's
// involvedObject and in a status message would have gone out verbatim in the status message —
// which is a customer's volume name in a file about to be emailed to a maintainer.
//
// Only the small streams are pre-scanned. Metrics carry no free text, and their labels are
// registered by Labels() as they are written; writeMetrics runs first, so everything a metric
// label knows is a rule by the time anything is Detail()ed. The three streams here are the ones
// whose identifiers exist ONLY in a structured field somewhere later in the file.
func (e *exporter) learnIdentifiers() {
	cutoff := e.cutoff()

	for _, day := range dayIndex(e.opts.DataDir, StreamState) {
		if !cutoff.IsZero() && day.Add(24*time.Hour).Before(cutoff) {
			continue
		}
		lines, err := readNDJSONGz(e.segPath(StreamState, day))
		if err != nil {
			continue
		}
		for _, raw := range lines {
			var snap StateSnapshot
			if json.Unmarshal(raw, &snap) != nil {
				continue
			}
			e.red.Learn(selfcheck.KindNamespace, snap.Namespace)
			e.red.Learn(selfcheck.KindObject, snap.Name)
		}
	}

	for _, day := range dayIndex(e.opts.DataDir, StreamEvents) {
		if !cutoff.IsZero() && day.Add(24*time.Hour).Before(cutoff) {
			continue
		}
		lines, err := readNDJSON(e.segPath(StreamEvents, day))
		if err != nil {
			continue
		}
		for _, raw := range lines {
			var ev Event
			if json.Unmarshal(raw, &ev) != nil {
				continue
			}
			e.red.Learn(selfcheck.KindNamespace, ev.Namespace)
			// An event's involvedObject is very often a PVC, and a PVC name is the one identifier
			// class nothing else in this archive enumerates.
			e.red.Learn(selfcheck.KindObject, ev.Name)
		}
	}

	if raw, err := readFile(e.opts.DataDir, fileMarks); err == nil {
		var marks Marks
		if json.Unmarshal(raw, &marks) == nil {
			for _, p := range marks.Pods {
				e.red.Learn(selfcheck.KindPod, p.Pod)
				e.red.Learn(selfcheck.KindNamespace, p.Namespace)
				// The node name is here and nowhere else in the archive, and it is what a kubelet
				// eviction message is written about.
				e.red.Learn(selfcheck.KindHost, p.Node)
				e.red.Learn(selfcheck.KindObject, p.Job)
			}
			for _, j := range marks.Jobs {
				e.red.Learn(selfcheck.KindObject, j.Job)
			}
		}
	}
}

type exporter struct {
	opts     ExportOptions
	now      time.Time
	red      *selfcheck.Redactor
	info     CollectorInfo
	uptime   Uptime
	tw       *tar.Writer
	progress io.Writer

	streams  []StreamReport
	problems []string
}

func (e *exporter) note(format string, args ...any) {
	if e.progress == nil {
		return
	}
	_, _ = fmt.Fprintf(e.progress, format+"\n", args...)
}

// cutoff is the earliest day --since admits.
func (e *exporter) cutoff() time.Time {
	if e.opts.Since <= 0 {
		return time.Time{}
	}
	return e.now.Add(-e.opts.Since)
}

func (e *exporter) requestedDays() float64 {
	if e.opts.Since > 0 {
		return e.opts.Since.Hours() / 24
	}
	start := e.uptime.FirstStartedAt
	if start.IsZero() {
		start = e.info.StartedAt
	}
	if start.IsZero() {
		return 0
	}
	return e.now.Sub(start).Hours() / 24
}

func (e *exporter) addFile(name string, body []byte, modTime time.Time) {
	hdr := &tar.Header{
		Name: name, Mode: 0o600, Size: int64(len(body)),
		ModTime: modTime.UTC(), Typeflag: tar.TypeReg,
	}
	if err := e.tw.WriteHeader(hdr); err != nil {
		e.problems = append(e.problems, "tar header for "+name+": "+err.Error())
		return
	}
	if _, err := e.tw.Write(body); err != nil {
		e.problems = append(e.problems, "tar body for "+name+": "+err.Error())
	}
}

// ---------------------------------------------------------------------------------------------
// metrics
// ---------------------------------------------------------------------------------------------

// writeMetrics merges the two on-disk families back into one file per day and redacts every
// label on the way.
//
// The two families exist only for the cap: `metrics-core` is §3b's list, which survives to the
// end, and `metrics` is everything else, which is what gets sacrificed first. A reader of the
// archive should never have to know that, so they are merged here and the split shows up only as
// drops in the manifest.
//
// This runs FIRST, and that ordering is load-bearing. Redacting a label registers it, so by the
// time the free text in events/ and logs/ reaches Detail(), every namespace, PVC, location,
// schedule, sync, node and pod that appeared in a metric for the whole fortnight is already a
// substitution rule. internal/selfcheck's Collect makes the same ordering promise for the same
// reason, and says that reordering it would leak a name into a sentence.
func (e *exporter) writeMetrics() {
	days := mergeDays(dayIndex(e.opts.DataDir, StreamMetrics), dayIndex(e.opts.DataDir, streamMetricsCore))
	cutoff := e.cutoff()
	res := e.info.resolution()

	var (
		items       int
		okWindows   int
		first, last time.Time
		kept        int
	)
	for _, day := range days {
		if !cutoff.IsZero() && day.Add(24*time.Hour).Before(cutoff) {
			continue
		}
		var out bytes.Buffer
		gzw := gzip.NewWriter(&out)
		n, ok, f, l := e.copyMetricsDay(gzw, day)
		if err := gzw.Close(); err != nil {
			e.problems = append(e.problems, "compress metrics for "+day.Format(dayLayout)+": "+err.Error())
			continue
		}
		if n == 0 {
			continue
		}
		items += n
		okWindows += ok
		if first.IsZero() || (!f.IsZero() && f.Before(first)) {
			first = f
		}
		if l.After(last) {
			last = l
		}
		kept++
		e.addFile(path.Join("metrics", day.Format(dayLayout)+".ndjson.gz"), out.Bytes(), day)
	}
	e.note("metrics: %d points across %d day segment(s)", items, kept)

	note := "downsampled to one point per " + res.String() + " window, with the min and max seen " +
		"inside it — a queue depth that touched the concurrency limit for four minutes is invisible " +
		"in a five-minute last, and that spike is a finding. Raw scrapes were never kept. A series " +
		"marked \"absent\" STOPPED being exposed at that window and was not carried forward at its " +
		"last value; the difference between 0 and not-there is load-bearing here."
	report := newStreamReport(StreamMetrics, items, note).
		withSamples(first, last).
		withCoverage(e.requestedDays(), float64(okWindows)*res.Hours()/24).
		withDrops(append(dropsOf(e.opts.DataDir, StreamMetrics), dropsOf(e.opts.DataDir, streamMetricsCore)...))
	if items == 0 {
		report.Note = "NOTHING was scraped. Read collector-errors.json in this archive: the scrape " +
			"either never authenticated (a 401 means the projected token, a 403 means the " +
			"crystal-backup-metrics-reader ClusterRoleBinding) or never connected. " + report.Note
	}
	e.streams = append(e.streams, report)
}

// streamMetricsCore is the second on-disk metrics family. It is not a manifest stream name — its
// drops are reported against `metrics` — but it is a segment layout of its own so that §3b's list
// outlives everything else the cap is allowed to take.
const streamMetricsCore = "metrics-core"

func (e *exporter) copyMetricsDay(w io.Writer, day time.Time) (items, okWindows int, first, last time.Time) {
	for _, stream := range []string{streamMetricsCore, StreamMetrics} {
		layout := segmentLayouts[stream]
		p := filepath.Join(e.opts.DataDir, layout.dir, day.Format(dayLayout)+layout.suffix)
		lines, err := readNDJSONGz(p)
		if err != nil {
			if !os.IsNotExist(err) {
				e.problems = append(e.problems, "read "+p+": "+err.Error())
			}
			continue
		}
		for _, raw := range lines {
			out, at, isHealth, ok := e.redactMetricLine(raw)
			if !ok {
				continue
			}
			if isHealth {
				okWindows++
			}
			if _, err := w.Write(append(out, '\n')); err != nil {
				e.problems = append(e.problems, "write metrics for "+day.Format(dayLayout)+": "+err.Error())
				return items, okWindows, first, last
			}
			items++
			if first.IsZero() || at.Before(first) {
				first = at
			}
			if at.After(last) {
				last = at
			}
		}
	}
	return items, okWindows, first, last
}

// redactMetricLine rewrites one stored point's labels. It returns whether the line was a scrape
// health record with at least one successful scrape, which is what observedDays counts.
func (e *exporter) redactMetricLine(raw []byte) (out []byte, at time.Time, health bool, ok bool) {
	var probe struct {
		Name string `json:"name"`
		T    int64  `json:"t"`
		OK   *int   `json:"ok"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, time.Time{}, false, false
	}
	at = time.Unix(probe.T, 0).UTC()
	if probe.Name == scrapeHealthName {
		return raw, at, probe.OK != nil && *probe.OK > 0, true
	}
	var p point
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, time.Time{}, false, false
	}
	p.Labels = e.red.Labels(p.Labels)
	body, err := json.Marshal(p)
	if err != nil {
		return nil, time.Time{}, false, false
	}
	return body, at, false, true
}

// ---------------------------------------------------------------------------------------------
// CR state
// ---------------------------------------------------------------------------------------------

func (e *exporter) writeState() {
	cutoff := e.cutoff()
	var (
		items       int
		first, last time.Time
		hours       = map[int64]bool{}
	)
	for _, day := range dayIndex(e.opts.DataDir, StreamState) {
		if !cutoff.IsZero() && day.Add(24*time.Hour).Before(cutoff) {
			continue
		}
		lines, err := readNDJSONGz(e.segPath(StreamState, day))
		if err != nil {
			e.problems = append(e.problems, "read CR state for "+day.Format(dayLayout)+": "+err.Error())
			continue
		}
		var out bytes.Buffer
		gzw := gzip.NewWriter(&out)
		n := 0
		for _, raw := range lines {
			var snap StateSnapshot
			if err := json.Unmarshal(raw, &snap); err != nil {
				continue
			}
			hours[snap.At.Truncate(time.Hour).Unix()] = true
			if first.IsZero() || snap.At.Before(first) {
				first = snap.At
			}
			if snap.At.After(last) {
				last = snap.At
			}
			snap.Namespace = e.red.Namespace(snap.Namespace)
			snap.Name = e.red.Object(snap.Name)
			// The status blob is walked as free-form JSON and every string in it goes through
			// Detail. A CR's status carries object names in messages (a failing backup names its
			// PVC), and the alternative — a typed walker per kind — would leak the first field
			// anyone forgot to add to it.
			snap.Status = e.redactRawJSON(snap.Status)
			body, err := json.Marshal(snap)
			if err != nil {
				continue
			}
			if _, err := gzw.Write(append(body, '\n')); err != nil {
				break
			}
			n++
		}
		if err := gzw.Close(); err != nil {
			e.problems = append(e.problems, "compress CR state: "+err.Error())
			continue
		}
		if n == 0 {
			continue
		}
		items += n
		e.addFile(path.Join("state", day.Format(dayLayout)+".ndjson.gz"), out.Bytes(), day)
	}
	e.note("state: %d CR observations", items)

	observed := float64(len(hours)) * e.info.stateInterval().Hours() / 24
	e.streams = append(e.streams, newStreamReport(StreamState, items,
		"hourly snapshots of every crystalbackup.io object's STATUS. The spec is not captured — it is "+
			"configuration, the daily self-checks carry it, and it is where the free text lives.").
		withSamples(first, last).
		withCoverage(e.requestedDays(), observed).
		withDrops(dropsOf(e.opts.DataDir, StreamState)))
}

// redactRawJSON substitutes identifiers out of every string inside an arbitrary JSON document,
// leaving numbers, booleans and structure alone.
func (e *exporter) redactRawJSON(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return raw
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return raw
	}
	body, err := json.Marshal(redactAny(e.red, v))
	if err != nil {
		return raw
	}
	return body
}

func redactAny(red *selfcheck.Redactor, v any) any {
	switch t := v.(type) {
	case string:
		return red.Detail(t)
	case []any:
		for i := range t {
			t[i] = redactAny(red, t[i])
		}
		return t
	case map[string]any:
		for k := range t {
			t[k] = redactAny(red, t[k])
		}
		return t
	default:
		return v
	}
}

// ---------------------------------------------------------------------------------------------
// high-water marks
// ---------------------------------------------------------------------------------------------

func (e *exporter) writeHighwater() {
	raw, err := readFile(e.opts.DataDir, fileMarks)
	if err != nil {
		e.streams = append(e.streams, notMeasuredStream(StreamHighwater,
			"the collector never wrote a high-water file. Peak mover memory, OOM kills, evictions "+
				"and the restic cache high-water were NOT measured — this is not a measurement of zero, "+
				"and it is the one figure a test suite cannot produce afterwards because a mover pod "+
				"lives for minutes."))
		return
	}
	var marks Marks
	if err := json.Unmarshal(raw, &marks); err != nil {
		e.streams = append(e.streams, notMeasuredStream(StreamHighwater,
			"the high-water file will not parse: "+err.Error()))
		return
	}
	for i := range marks.Pods {
		marks.Pods[i].Pod = e.red.Pod(marks.Pods[i].Pod)
		marks.Pods[i].Namespace = e.red.Namespace(marks.Pods[i].Namespace)
		marks.Pods[i].Node = e.red.Host(marks.Pods[i].Node)
		marks.Pods[i].Job = e.red.Object(marks.Pods[i].Job)
		marks.Pods[i].EvictionMessage = e.red.Detail(marks.Pods[i].EvictionMessage)
	}
	for i := range marks.Jobs {
		marks.Jobs[i].Job = e.red.Object(marks.Jobs[i].Job)
	}
	for class, cm := range marks.Classes {
		cm.PeakMemoryPod = e.red.Pod(cm.PeakMemoryPod)
		cm.LongestJob = e.red.Object(cm.LongestJob)
		marks.Classes[class] = cm
	}
	body, err := json.MarshalIndent(marks, "", "  ")
	if err != nil {
		e.streams = append(e.streams, notMeasuredStream(StreamHighwater,
			"the high-water marks could not be encoded: "+err.Error()))
		return
	}
	e.addFile("highwater/marks.json", body, marks.UpdatedAt)
	e.note("highwater: %d mover pod(s), %d Job(s)", len(marks.Pods), len(marks.Jobs))

	report := newStreamReport(StreamHighwater, len(marks.Pods),
		"peak memory per mover operation class, with the SAMPLE COUNT and the pod's lifetime beside "+
			"each peak: a pod that lives eleven minutes is sampled ~44 times at 15s and the true peak "+
			"is above the highest sample, so a peak from three samples is a different claim from a "+
			"peak from four hundred. OOM kills and evictions are definitive and need no "+
			"metrics-server.").
		withSamples(marks.UpdatedAt, marks.UpdatedAt).
		withCoverage(e.requestedDays(), e.uptime.ObservedSeconds/86400)
	switch {
	case marks.MetricsServer.Status != statusOK:
		report.Status = statusNotMeasured
		report.Note = "peak memory NOT MEASURED — " + marks.MetricsServer.Reason + " " + report.Note
	case marks.KubeletStats.Status != statusOK:
		report = report.degrade("The restic cache high-water is " +
			strings.ToLower(marks.KubeletStats.Status) + ": " + marks.KubeletStats.Reason + ".")
	}
	e.streams = append(e.streams, report)
}

// ---------------------------------------------------------------------------------------------
// self-checks
// ---------------------------------------------------------------------------------------------

// writeSelfchecks copies the daily reports through unchanged.
//
// They are the ONE stream that is already tokenised on the volume, and that is not an oversight.
// internal/selfcheck redacts as it BUILDS the report — the redactor is internal to Collect, and
// which field is an identifier is knowledge only that code has — so a report cannot be redacted
// afterwards without a second, parallel walker whose first forgotten field would be a leak. The
// collector therefore runs the daily self-check under the SAME stable salt this export uses,
// which is what makes a namespace the same token in a metric here and in day 9's report.
//
// The consequence, stated in the manifest rather than left to be discovered: --full cannot
// un-redact these. A full export carries verbatim identifiers everywhere else and tokens here.
func (e *exporter) writeSelfchecks() {
	if !e.info.SelfcheckEnabled {
		e.streams = append(e.streams, disabledStream(StreamSelfcheck,
			"the collector ran without --redaction-salt-file, so no daily self-check was taken. "+
				"Without a stable salt a report would either carry verbatim identifiers onto the volume "+
				"or carry tokens that correlate with nothing in the rest of this archive; neither is "+
				"worth writing, so the stream was not enabled."))
		return
	}
	names, err := countSelfchecks(e.opts.DataDir)
	if err != nil {
		e.streams = append(e.streams, notMeasuredStream(StreamSelfcheck,
			"the self-check directory could not be read: "+err.Error()))
		return
	}
	cutoff := e.cutoff()
	var first, last time.Time
	kept := 0
	for _, name := range names {
		at, err := time.Parse(time.RFC3339, strings.TrimSuffix(name, ".json"))
		if err == nil && !cutoff.IsZero() && at.Before(cutoff) {
			continue
		}
		body, err := readFile(e.opts.DataDir, filepath.Join(dirSelfcheck, name))
		if err != nil {
			e.problems = append(e.problems, "read self-check "+name+": "+err.Error())
			continue
		}
		if err == nil {
			if first.IsZero() || at.Before(first) {
				first = at
			}
			if at.After(last) {
				last = at
			}
		}
		e.addFile(path.Join("selfcheck", name), body, at)
		kept++
	}
	e.note("selfcheck: %d report(s)", kept)

	note := "the daily installation reports, already tokenised under the same salt as the rest of " +
		"this archive — so a namespace here is the namespace in the metrics."
	if e.opts.Full {
		note += " NOTE: --full could not un-redact these. They are redacted at COLLECTION, by the " +
			"code that builds them, because which field is an identifier is knowledge only that code " +
			"has. Everything else in this archive is verbatim; these are tokens."
	}
	e.streams = append(e.streams, newStreamReport(StreamSelfcheck, kept, note).
		withSamples(first, last).
		withCoverage(e.requestedDays(), float64(kept)*e.info.selfcheckInterval().Hours()/24))
}

// ---------------------------------------------------------------------------------------------
// events
// ---------------------------------------------------------------------------------------------

// writeEvents is the only stream read twice, and it has to be.
//
// An event's message routinely names the object the event is about, so every involvedObject in
// the whole stream must be a substitution rule BEFORE any message reaches Detail(). Learning as
// it emitted would leave the first occurrence of every name in clear — which is the occurrence
// that matters, being the one that says what went wrong first.
func (e *exporter) writeEvents() {
	cutoff := e.cutoff()
	days := dayIndex(e.opts.DataDir, StreamEvents)

	perDay := map[string][]Event{}
	for _, day := range days {
		if !cutoff.IsZero() && day.Add(24*time.Hour).Before(cutoff) {
			continue
		}
		lines, err := readNDJSON(e.segPath(StreamEvents, day))
		if err != nil {
			e.problems = append(e.problems, "read events for "+day.Format(dayLayout)+": "+err.Error())
			continue
		}
		var evs []Event
		for _, raw := range lines {
			var ev Event
			if err := json.Unmarshal(raw, &ev); err != nil {
				continue
			}
			e.red.Learn(selfcheck.KindNamespace, ev.Namespace)
			e.red.Learn(selfcheck.KindObject, ev.Name)
			evs = append(evs, ev)
		}
		perDay[day.Format(dayLayout)] = evs
	}

	var buf bytes.Buffer
	var first, last time.Time
	items := 0
	dayKeys := make([]string, 0, len(perDay))
	for k := range perDay {
		dayKeys = append(dayKeys, k)
	}
	slices.Sort(dayKeys)
	// Deduplicated by (UID, recurrence count) on the way out as well as on the way in. The
	// collector's in-memory dedup is per PROCESS, so a restart re-captures the last hour of
	// events — which are still live in the API — and the same failure would otherwise appear
	// twice in the archive, once on each side of a gap that is itself a finding.
	seen := map[string]bool{}
	for _, k := range dayKeys {
		for _, ev := range perDay[k] {
			dedup := fmt.Sprintf("%s/%d", ev.UID, ev.Count)
			if seen[dedup] {
				continue
			}
			seen[dedup] = true
			ev.Namespace = e.red.Namespace(ev.Namespace)
			ev.Name = e.red.Object(ev.Name)
			ev.Message = e.red.Detail(ev.Message)
			body, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			buf.Write(body)
			buf.WriteByte('\n')
			items++
			if first.IsZero() || ev.At.Before(first) {
				first = ev.At
			}
			if ev.At.After(last) {
				last = ev.At
			}
		}
	}
	e.addFile("events/events.ndjson", buf.Bytes(), e.now)
	e.note("events: %d Warning(s)", items)

	e.streams = append(e.streams, newStreamReport(StreamEvents, items,
		"every Warning event in the cluster, deduplicated by UID and recurrence count, captured "+
			"CONTINUOUSLY. Kubernetes forgets events after about an hour, so this is the stream a "+
			"daily job structurally cannot have. Empty is a possible healthy answer here.").
		withSamples(first, last).
		withCoverage(e.requestedDays(), e.uptime.ObservedSeconds/86400))
}

// ---------------------------------------------------------------------------------------------
// operator error lines
// ---------------------------------------------------------------------------------------------

func (e *exporter) writeLogs() {
	cutoff := e.cutoff()
	var buf bytes.Buffer
	var first, last time.Time
	items := 0
	for _, day := range dayIndex(e.opts.DataDir, StreamLogs) {
		if !cutoff.IsZero() && day.Add(24*time.Hour).Before(cutoff) {
			continue
		}
		lines, err := readNDJSON(e.segPath(StreamLogs, day))
		if err != nil {
			e.problems = append(e.problems, "read error lines for "+day.Format(dayLayout)+": "+err.Error())
			continue
		}
		for _, raw := range lines {
			var l LogLine
			if err := json.Unmarshal(raw, &l); err != nil {
				continue
			}
			l.Pod = e.red.Pod(l.Pod)
			l.Line = e.red.Detail(l.Line)
			body, err := json.Marshal(l)
			if err != nil {
				continue
			}
			buf.Write(body)
			buf.WriteByte('\n')
			items++
			if first.IsZero() || l.At.Before(first) {
				first = l.At
			}
			if l.At.After(last) {
				last = l.At
			}
		}
	}
	e.addFile("logs/operator-errors.ndjson", buf.Bytes(), e.now)
	e.note("logs: %d operator error line(s)", items)

	e.streams = append(e.streams, newStreamReport(StreamLogs, items,
		"error-level lines read from the API as they happened, not scraped off a node at the end — "+
			"which is what makes day three's error survive to day fourteen past the kubelet's log "+
			"rotation. This is FREE TEXT: read it before you send this archive. Empty is a possible "+
			"healthy answer here.").
		withSamples(first, last).
		withCoverage(e.requestedDays(), e.uptime.ObservedSeconds/86400).
		withDrops(dropsOf(e.opts.DataDir, StreamLogs)))
}

// ---------------------------------------------------------------------------------------------
// the collector's own files
// ---------------------------------------------------------------------------------------------

func (e *exporter) writeCollectorFiles() {
	if body, err := json.MarshalIndent(e.uptime, "", "  "); err == nil {
		e.addFile("uptime.json", body, e.now)
	}
	// The collector's own failure record. Not in §7's file list, and included anyway: it is the
	// difference between a hole in the metrics that means "the operator emitted nothing" and one
	// that means "the collector was 401ing for eleven days", and no other file in the archive can
	// tell those apart.
	if raw, err := readFile(e.opts.DataDir, fileErrors); err == nil {
		e.addFile("collector-errors.json", e.redactRawJSON(raw), e.now)
	}
	info := e.info
	info.MetricsURL = e.red.Endpoint(info.MetricsURL)
	info.OperatorNamespace = e.red.Namespace(info.OperatorNamespace)
	if body, err := json.MarshalIndent(info, "", "  "); err == nil {
		e.addFile("collector-config.json", body, e.now)
	}
}

// ---------------------------------------------------------------------------------------------
// the manifest and the prose
// ---------------------------------------------------------------------------------------------

func (e *exporter) writeManifest() error {
	// The alerts stream, always NOT_MEASURED in this build. §3b wants ALERTS{alertname=~
	// "Crystalbackup.*"} opportunistically — which of the eleven rules fired, when and for how
	// long is the single most informative series in a soak — but it is not a crystalbackup_
	// series and is not available from the operator's own /metrics, and §1's flag table has no
	// Prometheus URL to point at. Reported as what it is rather than omitted, so the absence is
	// a stated one.
	e.streams = append(e.streams, notMeasuredStream(StreamAlerts,
		"ALERTS{alertname=~\"Crystalbackup.*\"} is a PROMETHEUS series and this collector "+
			"deliberately scrapes only the operator, which is what makes the kit work on a cluster "+
			"with no Prometheus at all. If you do run one, export that series yourself for the soak "+
			"window and send it alongside: which of the eleven rules fired, when and for how long is "+
			"the one thing this archive cannot tell you."))

	m := Manifest{
		Schema:                  ManifestSchema,
		OperatorVersion:         e.info.OperatorVersion,
		ExportedAt:              e.now.Format(time.RFC3339),
		Mode:                    "collector",
		Redaction:               e.red.Describe(),
		CollectorUptimeFraction: round2(e.uptime.Fraction),
		Streams:                 e.streams,
	}
	if !e.uptime.FirstStartedAt.IsZero() {
		m.CollectorStartedAt = e.uptime.FirstStartedAt.Format(time.RFC3339)
	} else if !e.info.StartedAt.IsZero() {
		m.CollectorStartedAt = e.info.StartedAt.Format(time.RFC3339)
	}
	if e.opts.Full {
		m.UnredactedNote = unredactedResidualFull
	} else {
		m.UnredactedNote = unredactedResidual
	}
	if len(e.problems) > 0 {
		m.UnredactedNote += " The export itself hit " + fmt.Sprint(len(e.problems)) +
			" problem(s); they are listed in COLLECTION-REPORT.txt."
	}
	body, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	e.addFile("MANIFEST.json", append(body, '\n'), e.now)
	e.addFile("COLLECTION-REPORT.txt", []byte(renderReport(m, e.uptime, e.problems)), e.now)
	return nil
}

// renderReport is MANIFEST.json in prose, for the human who opens the tarball rather than
// running collect.sh. It says the same things in the same order; if the two ever disagree the
// JSON is the contract and this is the mistake.
func renderReport(m Manifest, up Uptime, problems []string) string {
	var b strings.Builder
	_, _ = fmt.Fprintf(&b, "CrystalBackup soak archive\n==========================\n\n")
	_, _ = fmt.Fprintf(&b, "operator version   %s\n", orDash(m.OperatorVersion))
	_, _ = fmt.Fprintf(&b, "collector started  %s\n", orDash(m.CollectorStartedAt))
	_, _ = fmt.Fprintf(&b, "exported           %s\n", m.ExportedAt)
	_, _ = fmt.Fprintf(&b, "redaction          %s\n", m.Redaction.Mode)
	_, _ = fmt.Fprintf(&b, "collector uptime   %.1f%% of the period this archive covers\n\n",
		m.CollectorUptimeFraction*100)

	_, _ = fmt.Fprintf(&b, "STREAMS\n-------\n")
	for _, s := range m.Streams {
		_, _ = fmt.Fprintf(&b, "\n  %-10s %s  (%d item(s))\n", s.Status, s.Name, s.Items)
		if s.Coverage != nil && s.Coverage.RequestedDays > 0 {
			_, _ = fmt.Fprintf(&b, "    covers %.2f of %.2f day(s)\n",
				s.Coverage.ObservedDays, s.Coverage.RequestedDays)
		}
		for _, d := range s.Drops {
			_, _ = fmt.Fprintf(&b, "    DROPPED %s..%s (%s)\n", d.From, d.To, d.Reason)
		}
		if s.Note != "" {
			b.WriteString(wrap(s.Note, "    "))
		}
	}

	_, _ = fmt.Fprintf(&b, "\n\nCOLLECTOR UPTIME\n----------------\n")
	b.WriteString(wrap(up.Note, "  "))
	for _, g := range up.Gaps {
		_, _ = fmt.Fprintf(&b, "  GAP  %s .. %s  (%.0f minutes)\n",
			g.From.Format(time.RFC3339), g.To.Format(time.RFC3339), g.Seconds/60)
	}

	if len(problems) > 0 {
		_, _ = fmt.Fprintf(&b, "\nPROBLEMS DURING EXPORT\n----------------------\n")
		for _, p := range problems {
			_, _ = fmt.Fprintf(&b, "  - %s\n", p)
		}
	}

	_, _ = fmt.Fprintf(&b, "\nBEFORE YOU SEND THIS\n--------------------\n")
	b.WriteString(wrap(m.UnredactedNote, "  "))
	_, _ = fmt.Fprintf(&b, "\n  Do NOT send the salt file with this archive. With it every token is "+
		"reversible by dictionary in seconds; without it, by nobody.\n")
	return b.String()
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// wrap is a plain 88-column fold. The report is read in a terminal by someone who has just
// untarred it, and a 700-character line in that context is a line nobody reads.
func wrap(s, indent string) string {
	const width = 88
	var b strings.Builder
	line := indent
	for word := range strings.FieldsSeq(s) {
		if len(line)+len(word)+1 > width && len(line) > len(indent) {
			b.WriteString(line + "\n")
			line = indent
		}
		if len(line) > len(indent) {
			line += " "
		}
		line += word
	}
	if len(line) > len(indent) {
		b.WriteString(line + "\n")
	}
	return b.String()
}

// ---------------------------------------------------------------------------------------------
// --status
// ---------------------------------------------------------------------------------------------

// Status is `soak-export --status`: one screen on stderr, no archive.
//
// This is what the admin runs on day 1 and day 2, and it is the cheapest possible defence against
// discovering on day 14 that nothing was collected. It writes nothing to stdout — collect.sh
// captures it with 2>&1 into the review directory — and it exits 0 whenever it could look, so the
// script can tell "the collector answered and here is what it said" from "this build has no
// soak-export".
func Status(opts ExportOptions, out io.Writer) error {
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()

	info, infoErr := ReadCollectorInfo(opts.DataDir)
	uptime, _ := ReadUptime(opts.DataDir, now)

	_, _ = fmt.Fprintf(out, "CrystalBackup soak collector — status at %s\n\n", now.Format(time.RFC3339))
	if infoErr != nil {
		_, _ = fmt.Fprintf(out, "  !! the collector has never recorded its configuration in %s.\n",
			opts.DataDir)
		_, _ = fmt.Fprintf(out, "     Nothing is being collected. Look at the pod before you wait another day.\n\n")
	} else {
		_, _ = fmt.Fprintf(out, "  operator version     %s\n", orDash(info.OperatorVersion))
		_, _ = fmt.Fprintf(out, "  metrics URL          %s every %s, downsampled to %s\n",
			info.MetricsURL, info.MetricsInterval, info.MetricsResolution)
		_, _ = fmt.Fprintf(out, "  mover sampling       every %s\n", info.MoverSampleInterval)
		_, _ = fmt.Fprintf(out, "  cache high-water     %s\n", enabledWord(info.KubeletStats,
			"enabled (--kubelet-stats)", "NOT MEASURED (--kubelet-stats not set)"))
		_, _ = fmt.Fprintf(out, "  daily self-check     %s\n", enabledWord(info.SelfcheckEnabled,
			"enabled", "DISABLED (no --redaction-salt-file)"))
		_, _ = fmt.Fprintf(out, "  footprint cap        %s\n\n", humanBytes(info.MaxBytes))
	}

	_, _ = fmt.Fprintf(out, "  up %.1f%% of the %.1f day(s) since it first started, across %d session(s)\n",
		uptime.Fraction*100, uptime.SpanSeconds/86400, len(uptime.Sessions))
	for _, g := range uptime.Gaps {
		_, _ = fmt.Fprintf(out, "    GAP  %s .. %s  (%.0f minutes)\n",
			g.From.Format(time.RFC3339), g.To.Format(time.RFC3339), g.Seconds/60)
	}
	_, _ = fmt.Fprintln(out)

	for _, line := range statusLines(opts.DataDir, info) {
		_, _ = fmt.Fprintf(out, "  %s\n", line)
	}

	store := &Store{dir: opts.DataDir}
	if total, err := store.Footprint(); err == nil {
		_, _ = fmt.Fprintf(out, "\n  on disk              %s", humanBytes(total))
		if info.MaxBytes > 0 {
			_, _ = fmt.Fprintf(out, " of %s", humanBytes(info.MaxBytes))
		}
		_, _ = fmt.Fprintln(out)
	}
	drops := loadDropsFrom(opts.DataDir)
	if len(drops) > 0 {
		_, _ = fmt.Fprintf(out, "  DROPPED BY THE CAP   %d segment(s):\n", len(drops))
		for _, d := range drops {
			_, _ = fmt.Fprintf(out, "    %s  %s .. %s\n", d.Stream, d.From, d.To)
		}
	}
	if raw, err := readFile(opts.DataDir, fileDegraded); err == nil {
		var st degradedState
		if json.Unmarshal(raw, &st) == nil && st.Degraded {
			_, _ = fmt.Fprintf(out, "  DEGRADED since %s: %s\n", st.Since.Format(time.RFC3339), st.Reason)
		}
	}
	if raw, err := readFile(opts.DataDir, fileErrors); err == nil {
		var recs []ErrorRecord
		if json.Unmarshal(raw, &recs) == nil && len(recs) > 0 {
			_, _ = fmt.Fprintf(out, "\n  WHAT HAS BEEN GOING WRONG\n")
			for _, r := range recs {
				_, _ = fmt.Fprintf(out, "    [%s ×%d] %s\n", r.Stream, r.Count, truncate(r.Message, 160))
			}
		}
	}
	_, _ = fmt.Fprintf(out, "\n  This is a status screen, not an archive. Run hack/soak/collect.sh at the\n")
	_, _ = fmt.Fprintf(out, "  end of the soak to export one.\n")
	return nil
}

// statusLines counts what is on the volume, per stream, without unpacking anything.
func statusLines(dir string, info CollectorInfo) []string {
	out := []string{}
	for _, stream := range []string{StreamMetrics, StreamState, StreamEvents, StreamLogs} {
		days := dayIndex(dir, stream)
		extra := ""
		if stream == StreamMetrics {
			extra = fmt.Sprintf(" (+%d core)", len(dayIndex(dir, streamMetricsCore)))
		}
		word := fmt.Sprintf("%-20s %d day segment(s)%s", stream, len(days), extra)
		if len(days) == 0 {
			word += "   <-- NOTHING COLLECTED"
		}
		out = append(out, word)
	}
	reports, _ := countSelfchecks(dir)
	line := fmt.Sprintf("%-20s %d report(s)", StreamSelfcheck, len(reports))
	if !info.SelfcheckEnabled {
		line += "   <-- DISABLED (no --redaction-salt-file)"
	} else if len(reports) == 0 {
		line += "   <-- NOTHING COLLECTED"
	}
	out = append(out, line)

	raw, err := readFile(dir, fileMarks)
	switch {
	case err != nil:
		out = append(out, fmt.Sprintf("%-20s NOT MEASURED — no marks file yet", StreamHighwater))
	default:
		var marks Marks
		if json.Unmarshal(raw, &marks) != nil {
			out = append(out, fmt.Sprintf("%-20s unreadable marks file", StreamHighwater))
			break
		}
		out = append(out, fmt.Sprintf("%-20s %d mover pod(s), memory %s, cache %s",
			StreamHighwater, len(marks.Pods), marks.MetricsServer.Status, marks.KubeletStats.Status))
		for _, class := range sortedClasses(marks) {
			cm := marks.Classes[class]
			if cm.Pods == 0 {
				continue
			}
			out = append(out, fmt.Sprintf("  %-18s %d pod(s), peak %s over %d sample(s), %d OOM, %d evicted",
				class, cm.Pods, humanBytes(cm.PeakMemoryBytes), cm.PeakSamples, cm.OOMKills, cm.Evictions))
		}
	}
	return out
}

func sortedClasses(m Marks) []string {
	out := make([]string, 0, len(m.Classes))
	for k := range m.Classes {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func enabledWord(on bool, yes, no string) string {
	if on {
		return yes
	}
	return no
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ---------------------------------------------------------------------------------------------
// reading what the collector wrote
// ---------------------------------------------------------------------------------------------

func (e *exporter) segPath(stream string, day time.Time) string {
	layout := segmentLayouts[stream]
	return filepath.Join(e.opts.DataDir, layout.dir, day.Format(dayLayout)+layout.suffix)
}

// dayIndex lists a stream's day segments without needing an open Store.
func dayIndex(dir, stream string) []time.Time {
	return (&Store{dir: dir}).Days(stream)
}

func dropsOf(dir, stream string) []Drop {
	out := []Drop{}
	for _, d := range loadDropsFrom(dir) {
		if d.Stream == stream {
			out = append(out, d)
		}
	}
	return out
}

func loadDropsFrom(dir string) []Drop {
	raw, err := readFile(dir, fileDrops)
	if err != nil {
		return nil
	}
	var out []Drop
	for line := range strings.SplitSeq(string(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var d Drop
		if json.Unmarshal([]byte(line), &d) == nil {
			out = append(out, d)
		}
	}
	return out
}

// readNDJSONGz reads a segment written as concatenated gzip members. Go's reader handles the
// concatenation transparently, which is the property that lets Append be crash-safe.
func readNDJSONGz(p string) ([][]byte, error) {
	f, err := os.Open(p) // #nosec G304 -- a path built from the collector's own data directory
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	defer func() { _ = zr.Close() }()
	return scanLines(zr)
}

func readNDJSON(p string) ([][]byte, error) {
	f, err := os.Open(p) // #nosec G304 -- a path built from the collector's own data directory
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return scanLines(f)
}

// maxSegmentLine bounds one NDJSON record. A line longer than this is a corrupted segment, not a
// metric, and the scanner has to refuse it rather than allocate for it — the collector's own
// memory limit is 384Mi.
const maxSegmentLine = 4 << 20

func scanLines(r io.Reader) ([][]byte, error) {
	var out [][]byte
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), maxSegmentLine)
	for sc.Scan() {
		line := sc.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		cp := make([]byte, len(line))
		copy(cp, line)
		out = append(out, cp)
	}
	return out, sc.Err()
}

func mergeDays(a, b []time.Time) []time.Time {
	seen := map[int64]bool{}
	out := []time.Time{}
	for _, d := range append(append([]time.Time{}, a...), b...) {
		if seen[d.Unix()] {
			continue
		}
		seen[d.Unix()] = true
		out = append(out, d)
	}
	slices.SortFunc(out, func(a, b time.Time) int { return a.Compare(b) })
	return out
}
