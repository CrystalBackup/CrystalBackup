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

// Package soak is the resident soak collector and its exporter: `crystal-backup soak-collect`
// keeps five streams on a PVC for a fortnight, and `crystal-backup soak-export` writes one
// redacted tar.gz to stdout.
//
// Both are subcommands of the OPERATOR binary rather than a second artefact, for the reason
// cmd/main.go's dispatch comment gives: a second image is a second supply chain to sign, scan and
// get wrong. They run as the operator's neighbour, inside its image, with their own identity.
//
// The failure this package exists to prevent is not a crash. It is a collector that starts
// happily, collects nothing for fourteen days, and hands back a small tidy archive that looks
// complete. Every refusal at startup, every NOT_MEASURED, every recorded drop and the whole
// MANIFEST.json contract are there to make that outcome impossible to reach by accident.
package soak

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// The stream names. They are the `name` field of MANIFEST.json's stream entries, which
// hack/soak/collect.sh reads and grades one by one, so they are constants rather than literals:
// a stream renamed in one of the two places is a stream collect.sh reports under a name nobody
// recognises.
const (
	StreamMetrics   = "metrics"
	StreamState     = "state"
	StreamLogs      = "logs"
	StreamEvents    = "events"
	StreamSelfcheck = "selfcheck"
	StreamHighwater = "highwater"
	StreamAlerts    = "alerts"
)

// segmentLayout describes where one day-segmented stream lives and whether it may be dropped
// when the footprint cap bites.
//
// dropRank orders the sacrifice, and the order is not arbitrary (§4 of hack/soak/SPEC.md): raw
// metrics first because they are the bulkiest and the most reconstructable from their own
// aggregates, then CR state, then the operator's error lines. Rank 0 means NEVER — the events,
// the self-checks and the high-water marks are the parts that cannot be reconstructed at all and
// they are two orders of magnitude smaller than what is being dropped for them.
type segmentLayout struct {
	dir      string
	suffix   string
	gzip     bool
	dropRank int
}

// The two segment extensions, and the one reason a drop is ever recorded.
const (
	suffixNDJSON   = ".ndjson"
	suffixNDJSONGz = ".ndjson.gz"
	// reasonCap is the `reason` field of every Drop. collect.sh reports it verbatim to the admin.
	reasonCap = "cap"
)

var segmentLayouts = map[string]segmentLayout{
	StreamMetrics:     {dir: StreamMetrics, suffix: suffixNDJSONGz, gzip: true, dropRank: 1},
	StreamState:       {dir: StreamState, suffix: suffixNDJSONGz, gzip: true, dropRank: 2},
	StreamLogs:        {dir: StreamLogs, suffix: suffixNDJSON, dropRank: 3},
	streamMetricsCore: {dir: streamMetricsCore, suffix: suffixNDJSONGz, gzip: true, dropRank: 4},
	StreamEvents:      {dir: StreamEvents, suffix: suffixNDJSON, dropRank: 0},
}

// dropOrder is segmentLayouts' droppable half, in the order §4 fixes. Materialised as a slice
// because map iteration is randomised in Go, and "the OLDEST metrics segment goes before any
// state segment" is a promise the manifest makes to whoever reads the archive.
//
// metrics-core is LAST, after everything else the cap is allowed to take, and that is §3b: the
// series whose trend over a fortnight is the whole point — repository growth against protected
// bytes, a failure counter's slope, a p95 that moved — are kept at full resolution to the end,
// and the bulk of the exposition is what degrades. Both families are merged back into one
// metrics/<day>.ndjson.gz at export, so a reader of the archive never has to know this existed.
var dropOrder = []string{StreamMetrics, StreamState, StreamLogs, streamMetricsCore}

// Fixed paths under the data directory that are never segmented and never dropped.
const (
	dirSelfcheck    = "selfcheck"
	dirHighwater    = "highwater"
	dirCollector    = "collector"
	fileMarks       = "highwater/marks.json"
	fileHeartbeat   = "collector/heartbeat.json"
	fileStarts      = "collector/starts.ndjson"
	fileDrops       = "collector/drops.ndjson"
	fileErrors      = "collector/errors.json"
	fileDegraded    = "collector/degraded.json"
	fileCollectorID = "collector/collector.json"
)

// freeSpaceFloor is the point below which the collector stops writing raw samples entirely.
//
// The cap (--max-bytes) bounds what THIS process wrote. It cannot bound what else is on the
// volume: a filesystem smaller than the PVC claims, another writer, a reserved-blocks setting.
// Below this floor the collector keeps the aggregates, the events and the high-water marks and
// stops adding to the bulk streams — because on a node-backed StorageClass, filling this volume
// IS the DiskPressure eviction the soak was deployed to observe, and a collector that causes the
// incident it is measuring has destroyed the fortnight rather than recorded it.
const freeSpaceFloor = 64 << 20

// capCheckInterval is the longest the collector may go without measuring its own footprint. The
// cap is also checked after every segment close, which is the moment the footprint actually
// jumps; this is the backstop for a day on which nothing closes.
const capCheckInterval = 10 * time.Minute

// Drop is one segment the footprint cap deleted. It goes into MANIFEST.json verbatim, where
// collect.sh counts it and turns it into a THIN verdict — an archive that lost its first three
// days says so on its first page.
type Drop struct {
	Stream string `json:"stream"`
	From   string `json:"from"`
	To     string `json:"to"`
	Reason string `json:"reason"`
}

// Store owns everything under --data-dir. Nothing in this package writes a byte anywhere else:
// no node ephemeral storage, no /tmp beyond what Go's own tempdir needs, no ConfigMap, no Lease.
type Store struct {
	dir      string
	maxBytes int64

	mu             sync.Mutex
	drops          []Drop
	degraded       bool
	degradedAt     time.Time
	degradedReason string
	lastCapCheck   time.Time
	// errs is the collector's own failure record, keyed by message so a scrape that has been
	// 401ing every minute for eleven days is one entry with a count and two timestamps rather
	// than 15,840 lines. It is flushed whole; it is never dropped.
	errs map[string]*ErrorRecord
}

// ErrorRecord is one thing that kept going wrong, coalesced.
type ErrorRecord struct {
	Stream  string    `json:"stream"`
	Message string    `json:"message"`
	Count   int       `json:"count"`
	FirstAt time.Time `json:"firstAt"`
	LastAt  time.Time `json:"lastAt"`
}

// OpenStore prepares the data directory and REFUSES rather than degrades on the two conditions
// that would make the whole fortnight worthless.
//
// Both refusals are startup-only and both are loud. A collector that cannot write is not a
// collector, and one that starts on a volume with less free space than its own cap is one that
// will hit the free-space floor on day two and spend twelve days in degraded mode — which is a
// thing to discover now, with an operator watching, and not on day fourteen from a manifest.
func OpenStore(dir string, maxBytes int64) (*Store, error) {
	if dir == "" {
		return nil, fmt.Errorf("--data-dir is required")
	}
	for _, sub := range []string{dirSelfcheck, dirHighwater, dirCollector,
		StreamMetrics, streamMetricsCore, StreamState, StreamLogs, StreamEvents} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o750); err != nil {
			return nil, fmt.Errorf("--data-dir %s is not usable: %w", dir, err)
		}
	}
	// A WRITE, not a stat. os.MkdirAll returns nil for a directory that already exists, so a
	// volume whose layout was created on day one and has since been remounted read-only would get
	// all the way past the mkdir loop above — and then collect nothing for thirteen days.
	probe := filepath.Join(dir, dirCollector, ".writable")
	if err := os.WriteFile(probe, []byte("ok\n"), 0o600); err != nil {
		return nil, fmt.Errorf("--data-dir %s is not writable: %w", dir, err)
	}
	if err := os.Remove(probe); err != nil {
		return nil, fmt.Errorf("--data-dir %s is not writable: %w", dir, err)
	}
	free, err := freeBytes(dir)
	switch {
	case err != nil:
		// Not a refusal. An unreadable statfs is a property of the filesystem, not evidence that
		// the volume is small, and refusing on it would make the collector unrunnable on a
		// platform nobody anticipated. It is recorded instead.
		break
	case free < maxBytes:
		return nil, fmt.Errorf(
			"--data-dir %s has %s free and --max-bytes is %s: the collector would spend the soak in "+
				"degraded mode rather than collecting. Give it a larger volume or lower --max-bytes",
			dir, humanBytes(free), humanBytes(maxBytes))
	}
	s := &Store{dir: dir, maxBytes: maxBytes, errs: map[string]*ErrorRecord{}}
	if err := s.loadDrops(); err != nil {
		return nil, err
	}
	s.loadErrors()
	s.loadDegraded()
	if err == nil && free < maxBytes+freeSpaceFloor {
		s.RecordError("collector", fmt.Sprintf(
			"free space on the data volume (%s) leaves little headroom above --max-bytes (%s)",
			humanBytes(free), humanBytes(maxBytes)), time.Now().UTC())
	}
	return s, nil
}

// Dir is the data directory. Exported for the export path, which reads what the collector wrote.
func (s *Store) Dir() string { return s.dir }

// Append writes NDJSON lines into one day's segment of a stream.
//
// Gzipped segments get a COMPLETE gzip member per call, appended to the file. A concatenation of
// gzip members is itself a valid gzip stream and Go's reader decodes it transparently, so the
// segment stays append-only and crash-safe — a process killed mid-append loses at most the one
// member it was writing, not the day. The obvious alternative (one long-lived gzip.Writer per
// day) buys better compression and loses the whole day to any kill, which for a process that
// must survive a fortnight of node events is the wrong trade.
//
// In degraded mode the bulk streams are refused and the refusal is silent to the caller and
// LOUD in the manifest. That asymmetry is deliberate: the caller has nothing useful to do about
// it, and the reader of the archive has.
func (s *Store) Append(stream string, day time.Time, lines [][]byte) error {
	if len(lines) == 0 {
		return nil
	}
	layout, ok := segmentLayouts[stream]
	if !ok {
		return fmt.Errorf("no segment layout for stream %q", stream)
	}
	if s.Degraded() && layout.dropRank != 0 {
		return nil
	}
	var body bytes.Buffer
	for _, l := range lines {
		body.Write(l)
		if len(l) == 0 || l[len(l)-1] != '\n' {
			body.WriteByte('\n')
		}
	}
	payload := body.Bytes()
	if layout.gzip {
		var gz bytes.Buffer
		w := gzip.NewWriter(&gz)
		if _, err := w.Write(payload); err != nil {
			return fmt.Errorf("compress %s segment: %w", stream, err)
		}
		if err := w.Close(); err != nil {
			return fmt.Errorf("compress %s segment: %w", stream, err)
		}
		payload = gz.Bytes()
	}
	path := s.segmentPath(stream, day)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open %s segment: %w", stream, err)
	}
	if _, err := f.Write(payload); err != nil {
		_ = f.Close()
		return fmt.Errorf("write %s segment: %w", stream, err)
	}
	return f.Close()
}

func (s *Store) segmentPath(stream string, day time.Time) string {
	layout := segmentLayouts[stream]
	return filepath.Join(s.dir, layout.dir, day.UTC().Format(dayLayout)+layout.suffix)
}

// dayLayout is the segment file's name. UTC, always, on every cluster — a soak whose day
// boundary moved with a DST transition would have two days that are not 24 hours long, in the one
// stream whose entire value is the time axis.
const dayLayout = "2006-01-02"

// WriteFileAtomic replaces one of the small, undroppable documents (the marks, the heartbeat,
// the error record) via a temp file and a rename, so a kill mid-write leaves the previous
// version rather than a truncated one.
func (s *Store) WriteFileAtomic(rel string, body []byte) error {
	path := filepath.Join(s.dir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// AppendLine appends one line to an undroppable, unsegmented file.
func (s *Store) AppendLine(rel string, line []byte) error {
	path := filepath.Join(s.dir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// Footprint is the total on-disk size of everything under the data directory — not the size of
// what this process believes it wrote. The two differ exactly when something has gone wrong, and
// the difference is what the cap has to act on.
func (s *Store) Footprint() (int64, error) {
	var total int64
	err := filepath.WalkDir(s.dir, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			// A file that vanished between the walk and the stat is this process's own rename;
			// it is not a reason to abandon the measurement.
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		total += info.Size()
		return nil
	})
	return total, err
}

// EnforceCap brings the footprint back under --max-bytes by deleting whole CLOSED segments,
// oldest first, in the order dropOrder fixes — and records every deletion.
//
// Three properties are load-bearing:
//
//   - the segment for TODAY is never a candidate, in any stream. It is the one being appended to;
//     deleting it would drop data the collector is about to write more of, and would take the
//     current window's aggregates with it;
//   - each deletion is recorded as a Drop that reaches MANIFEST.json, so the archive says what it
//     lost and when. This is the difference between an archive that is short and an archive that
//     is short and lying;
//   - if nothing droppable is left and the footprint is still over, the collector goes DEGRADED
//     rather than deleting something it promised not to. Dropping the oldest data is defensible;
//     running a node out of disk is not, and neither is quietly eating the high-water marks.
func (s *Store) EnforceCap(now time.Time) error {
	s.mu.Lock()
	s.lastCapCheck = now
	s.mu.Unlock()

	total, err := s.Footprint()
	if err != nil {
		return err
	}
	for total > s.maxBytes {
		victim, ok := s.oldestDroppable(now)
		if !ok {
			s.setDegraded(now, fmt.Sprintf(
				"the footprint is %s against a --max-bytes of %s and there is no closed segment left to "+
					"drop: raw sampling is stopped. The high-water marks, the events and the self-checks "+
					"are kept",
				humanBytes(total), humanBytes(s.maxBytes)))
			return nil
		}
		info, err := os.Stat(victim.path)
		if err != nil {
			return err
		}
		if err := os.Remove(victim.path); err != nil {
			return err
		}
		total -= info.Size()
		if err := s.recordDrop(Drop{
			Stream: victim.stream,
			From:   victim.day.Format(time.RFC3339),
			To:     victim.day.Add(24 * time.Hour).Format(time.RFC3339),
			Reason: reasonCap,
		}); err != nil {
			return err
		}
	}
	// The free-space floor is checked on the same beat as the cap, and it can BOTH set and clear
	// degraded mode: a volume that recovered (the cap dropped a segment, someone else's data
	// went away) should resume collecting rather than stay crippled for the rest of the soak on
	// the strength of one bad afternoon.
	free, err := freeBytes(s.dir)
	if err != nil {
		return nil //nolint:nilerr // an unreadable statfs is not a reason to stop collecting
	}
	if free < freeSpaceFloor {
		s.setDegraded(now, fmt.Sprintf(
			"only %s free on the data volume, below the %s floor: raw sampling is stopped so the "+
				"collector cannot cause the DiskPressure eviction it was deployed to observe",
			humanBytes(free), humanBytes(freeSpaceFloor)))
		return nil
	}
	s.clearDegraded(now)
	return nil
}

// DueForCapCheck reports whether capCheckInterval has passed. The loop also calls EnforceCap
// after every segment close; this is what covers a quiet day.
func (s *Store) DueForCapCheck(now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return now.Sub(s.lastCapCheck) >= capCheckInterval
}

type candidate struct {
	stream string
	path   string
	day    time.Time
}

// oldestDroppable finds the next segment to sacrifice: the oldest CLOSED segment of the
// highest-ranked droppable stream that has one.
func (s *Store) oldestDroppable(now time.Time) (candidate, bool) {
	today := now.UTC().Format(dayLayout)
	for _, stream := range dropOrder {
		layout := segmentLayouts[stream]
		entries, err := os.ReadDir(filepath.Join(s.dir, layout.dir))
		if err != nil {
			continue
		}
		var days []string
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), layout.suffix) {
				continue
			}
			day := strings.TrimSuffix(e.Name(), layout.suffix)
			if day >= today {
				continue // the open segment, and anything a clock skew put in the future
			}
			if _, err := time.Parse(dayLayout, day); err != nil {
				continue
			}
			days = append(days, day)
		}
		if len(days) == 0 {
			continue
		}
		slices.Sort(days)
		day, _ := time.Parse(dayLayout, days[0])
		return candidate{
			stream: stream,
			path:   filepath.Join(s.dir, layout.dir, days[0]+layout.suffix),
			day:    day.UTC(),
		}, true
	}
	return candidate{}, false
}

func (s *Store) recordDrop(d Drop) error {
	s.mu.Lock()
	s.drops = append(s.drops, d)
	s.mu.Unlock()
	line, err := json.Marshal(d)
	if err != nil {
		return err
	}
	return s.AppendLine(fileDrops, line)
}

// Drops returns everything the cap has taken, across every process this collector has been.
func (s *Store) Drops() []Drop {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Drop, len(s.drops))
	copy(out, s.drops)
	return out
}

// DropsFor returns the drops for one stream, which is what MANIFEST.json carries per stream.
func (s *Store) DropsFor(stream string) []Drop {
	out := []Drop{}
	for _, d := range s.Drops() {
		if d.Stream == stream {
			out = append(out, d)
		}
	}
	return out
}

func (s *Store) loadDrops() error {
	raw, err := os.ReadFile(filepath.Join(s.dir, fileDrops))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for line := range strings.SplitSeq(string(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var d Drop
		if err := json.Unmarshal([]byte(line), &d); err == nil {
			s.drops = append(s.drops, d)
		}
	}
	return nil
}

// degradedState is the on-disk half of degraded mode. It survives a restart because the fact
// that raw sampling was stopped for six hours on day nine is a property of the ARCHIVE, not of
// the process that happened to be running at the time.
type degradedState struct {
	Degraded bool      `json:"degraded"`
	Since    time.Time `json:"since,omitempty"`
	Reason   string    `json:"reason,omitempty"`
}

func (s *Store) setDegraded(now time.Time, reason string) {
	s.mu.Lock()
	already := s.degraded
	if !already {
		s.degraded = true
		s.degradedAt = now.UTC()
	}
	s.degradedReason = reason
	state := degradedState{Degraded: true, Since: s.degradedAt, Reason: reason}
	s.mu.Unlock()
	if body, err := json.Marshal(state); err == nil {
		_ = s.WriteFileAtomic(fileDegraded, body)
	}
	if !already {
		s.RecordError("collector", reason, now)
	}
}

func (s *Store) clearDegraded(now time.Time) {
	s.mu.Lock()
	if !s.degraded {
		s.mu.Unlock()
		return
	}
	s.degraded = false
	s.degradedReason = ""
	s.mu.Unlock()
	if body, err := json.Marshal(degradedState{Degraded: false}); err == nil {
		_ = s.WriteFileAtomic(fileDegraded, body)
	}
	s.RecordError("collector", "raw sampling resumed: the data volume is back above the free-space floor", now)
}

func (s *Store) loadDegraded() {
	raw, err := os.ReadFile(filepath.Join(s.dir, fileDegraded))
	if err != nil {
		return
	}
	var st degradedState
	if err := json.Unmarshal(raw, &st); err != nil {
		return
	}
	s.degraded, s.degradedAt, s.degradedReason = st.Degraded, st.Since, st.Reason
}

// Degraded reports whether raw sampling is stopped.
func (s *Store) Degraded() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.degraded
}

// DegradedSince returns the mode, when it started and why.
func (s *Store) DegradedSince() (bool, time.Time, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.degraded, s.degradedAt, s.degradedReason
}

// RecordError is how everything that failed gets into the archive.
//
// It is the counterweight to every `if err != nil { continue }` in this package. A collector that
// swallowed a scrape failure would produce a metrics stream with a hole in it and no explanation,
// and the hole would read as "the operator emitted nothing" — which is the exact confusion
// internal/metrics/names.go documents an alert dying of. Coalesced by message so a persistent
// failure costs one entry, and never dropped by the cap, because it is the smallest and most
// load-bearing thing on the volume.
func (s *Store) RecordError(stream, message string, now time.Time) {
	s.mu.Lock()
	key := stream + "\x00" + message
	rec, ok := s.errs[key]
	if !ok {
		rec = &ErrorRecord{Stream: stream, Message: message, FirstAt: now.UTC()}
		s.errs[key] = rec
	}
	rec.Count++
	rec.LastAt = now.UTC()
	snapshot := s.errorSnapshotLocked()
	s.mu.Unlock()
	if body, err := json.MarshalIndent(snapshot, "", "  "); err == nil {
		_ = s.WriteFileAtomic(fileErrors, body)
	}
}

// Errors returns the coalesced failure record, newest-last.
func (s *Store) Errors() []ErrorRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.errorSnapshotLocked()
}

// ErrorsFor returns the failures recorded against one stream.
func (s *Store) ErrorsFor(stream string) []ErrorRecord {
	out := []ErrorRecord{}
	for _, e := range s.Errors() {
		if e.Stream == stream {
			out = append(out, e)
		}
	}
	return out
}

func (s *Store) errorSnapshotLocked() []ErrorRecord {
	out := make([]ErrorRecord, 0, len(s.errs))
	for _, r := range s.errs {
		out = append(out, *r)
	}
	slices.SortFunc(out, func(a, b ErrorRecord) int {
		if c := a.FirstAt.Compare(b.FirstAt); c != 0 {
			return c
		}
		return strings.Compare(a.Message, b.Message)
	})
	return out
}

func (s *Store) loadErrors() {
	raw, err := os.ReadFile(filepath.Join(s.dir, fileErrors))
	if err != nil {
		return
	}
	var recs []ErrorRecord
	if err := json.Unmarshal(raw, &recs); err != nil {
		return
	}
	for i := range recs {
		r := recs[i]
		s.errs[r.Stream+"\x00"+r.Message] = &r
	}
}

// Days lists the day segments a stream still has on disk, oldest first. It is how the export
// computes coverage.observedDays from the DATA rather than from the flags — §8's single most
// important number is the gap between what was asked for and what is there, and it would be
// worthless if it were derived from what was asked for.
func (s *Store) Days(stream string) []time.Time {
	layout, ok := segmentLayouts[stream]
	if !ok {
		return nil
	}
	entries, err := os.ReadDir(filepath.Join(s.dir, layout.dir))
	if err != nil {
		return nil
	}
	var days []time.Time
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), layout.suffix) {
			continue
		}
		d, err := time.Parse(dayLayout, strings.TrimSuffix(e.Name(), layout.suffix))
		if err != nil {
			continue
		}
		days = append(days, d.UTC())
	}
	slices.SortFunc(days, func(a, b time.Time) int { return a.Compare(b) })
	return days
}

// readDirNames lists one subdirectory of a data directory. An absent directory is an empty list,
// not an error: a stream that has never written anything has no directory, and that is exactly
// the case the manifest has to report as EMPTY rather than as a failure to look.
func readDirNames(dir, sub string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(dir, sub))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	slices.Sort(names)
	return names, nil
}

// humanBytes renders a byte count the way the operator wrote it on the command line. An error
// message that says "536870912" when the flag said "512Mi" makes the reader do arithmetic during
// an incident.
func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1fGi", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.0fMi", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0fKi", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%dB", n)
	}
}
