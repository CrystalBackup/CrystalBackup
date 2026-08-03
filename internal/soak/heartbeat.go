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
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/CrystalBackup/CrystalBackup/internal/mover"
)

// ---------------------------------------------------------------------------------------------
// The daily line.
//
// THE HOLE THIS CLOSES. Everything the collector knows is already available through
// `soak-export --status` — and somebody has to remember to run it. Nobody runs it daily. So a
// collector that broke on day 3 (the projected token stopped being accepted, the PVC came back
// read-only, the cap was reached far sooner than the sizing predicted) was discovered on day 14,
// with the fortnight already spent. A soak nobody can check is a soak you can waste.
//
// So the collector says one thing a day, in its own log, and that is the whole feature: no
// Prometheus, no alert rule, no configuration, no new dependency. It lands in `kubectl logs` and
// therefore in whatever log pipeline the cluster already has, and `kubectl logs --tail=7` is a
// week of history.
//
// AND IT IS AS USEFUL ABSENT AS PRESENT. No line for two days means the collector is not running
// — an inference anyone can draw without knowing anything about this tool, which is why the
// cadence is a fixed 24h from the last line rather than something derived from the flags. The
// startup line exists for the same reason from the other end: an administrator who has just
// applied the manifest gets confirmation in seconds instead of waiting a day for the first beat.
//
// ONE LINE, not a block, and not chatty. Seven of them fit on a screen and the difference
// between two days is visible at a glance; a block would make that a diff exercise. Nothing else
// is emitted at this level between them.
// ---------------------------------------------------------------------------------------------

// heartbeatInterval is the cadence, and it is a constant rather than a flag on purpose: the
// absence inference ("no line since Tuesday") only works if a reader can assume the period
// without having to find out how this collector was configured.
const heartbeatInterval = 24 * time.Hour

// heartbeatPrefix is what a reader greps for. Fixed, and fixed first on the line, so
// `kubectl logs deploy/crystal-backup-soak | grep soak-heartbeat` is the whole interface.
const heartbeatPrefix = "INFO soak-heartbeat"

// heartbeatStreams are the streams the line counts, in the order it prints them. Day-segmented
// ones are counted in SEGMENTS (one per UTC day); the other two carry their own units and are
// printed separately.
var heartbeatStreams = []string{StreamMetrics, StreamState, StreamEvents, StreamLogs}

// silenceGrace is how long a stream is given before an empty one is called out by name.
//
// Without it the FIRST line — the one written at startup, before anything could possibly have
// been collected — would name every stream as silent, which is alarming and false, and a reader
// who learns to ignore the first day's alarm will ignore day nine's. Each grace is derived from
// the cadence the stream actually writes at, so it is "long enough that empty is a finding"
// rather than a number chosen to look calm.
//
// The counts themselves are on the line either way. `silent=` is the alarm, not the data: a
// reader who wants to know whether day 1 has any state snapshots yet can see state=0 directly.
func silenceGrace(stream string, info CollectorInfo) time.Duration {
	switch stream {
	case StreamMetrics:
		// A window has to close and be written before there is a segment at all.
		return maxDuration(2*info.resolution(), 10*time.Minute)
	case StreamState:
		return maxDuration(2*info.stateInterval(), 2*time.Hour)
	case StreamSelfcheck:
		// The self-check runs on the collector's FIRST round, so anything past an hour with none
		// on disk is a failure rather than a wait.
		return time.Hour
	case StreamHighwater:
		// marks.json appears within one sample interval, but the number this line reports is mover
		// PODS, and that needs a schedule to fire. A day is the shortest honest wait.
		return 24 * time.Hour
	default:
		// Events and logs are empty-is-healthy; they are never named here at all.
		return 0
	}
}

func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}

// heartbeatLine is everything one line says, separated from its formatting so both can be tested
// without a running collector and without a clock.
type heartbeatLine struct {
	At         time.Time
	Day        int
	UpFraction float64
	SpanDays   float64
	Sessions   int
	Segments   map[string]int
	Selfchecks int
	MoverPods  int
	// MoverClasses is the per-sizing-class pod count, and it exists because the aggregate above
	// hid the worst measurement failure this kit has had.
	//
	// A four-hour run reported movers=87 — healthy by every signal on this line, `silent=none` —
	// while classes `data` and `manifests` sat at zero through a campaign that executed dozens of
	// backups. The collector could not see them at all, and an aggregate that only has to be
	// non-zero cannot say so: repo-light alone kept the number up. A per-class breakdown makes
	// `data:0` visible on day one, to a reader doing nothing more than the ten-second check.
	MoverClasses map[string]int
	Footprint    int64
	MaxBytes     int64
	Degraded     bool
	Drops        int
	Silent       []string
}

// collectHeartbeat reads the volume and says what is there. It never fails: a heartbeat that
// could not be produced because one directory was unreadable would remove the signal on exactly
// the day it matters.
func collectHeartbeat(store *Store, info CollectorInfo, now time.Time) heartbeatLine {
	dir := store.Dir()
	up, _ := ReadUptime(dir, now)

	h := heartbeatLine{
		At:         now.UTC(),
		UpFraction: up.Fraction,
		SpanDays:   up.SpanSeconds / 86400,
		Sessions:   len(up.Sessions),
		Segments:   map[string]int{},
		MaxBytes:   info.MaxBytes,
		Degraded:   store.Degraded(),
		Drops:      len(store.Drops()),
	}
	// Day 1 is the day it started. A reader counting days of a fortnight counts from one.
	h.Day = int(h.SpanDays) + 1

	for _, stream := range heartbeatStreams {
		h.Segments[stream] = len(store.Days(stream))
	}
	if reports, err := countSelfchecks(dir); err == nil {
		h.Selfchecks = len(reports)
	}
	if raw, err := readFile(dir, fileMarks); err == nil {
		var marks Marks
		if json.Unmarshal(raw, &marks) == nil {
			h.MoverPods = len(marks.Pods)
			// Every class from the sizing table, present even at zero. A key that only appears
			// once it is non-zero is a key nobody can grep for on the day it matters — the same
			// reason silent= always prints "none".
			h.MoverClasses = map[string]int{}
			for _, class := range mover.Classes() {
				h.MoverClasses[class] = 0
			}
			for _, p := range marks.Pods {
				if _, known := h.MoverClasses[p.Class]; known {
					h.MoverClasses[p.Class]++
				}
			}
		}
	}
	if total, err := store.Footprint(); err == nil {
		h.Footprint = total
	}

	// The part that is worth having. A stream that is empty when empty is not a healthy answer is
	// NAMED, and the table that decides which those are is the same one MANIFEST.json grades by
	// (emptyIsHealthy) — so the line and the archive can never disagree about whether zero
	// Warning events is good news.
	counts := map[string]int{
		StreamMetrics:   h.Segments[StreamMetrics],
		StreamState:     h.Segments[StreamState],
		StreamEvents:    h.Segments[StreamEvents],
		StreamLogs:      h.Segments[StreamLogs],
		StreamSelfcheck: h.Selfchecks,
		StreamHighwater: h.MoverPods,
	}
	span := time.Duration(up.SpanSeconds * float64(time.Second))
	// A FIXED order, not a map range: two consecutive days must produce the same line when
	// nothing changed, or the diff-at-a-glance this whole format exists for is noise.
	for _, stream := range []string{
		StreamMetrics, StreamState, StreamEvents, StreamLogs, StreamSelfcheck, StreamHighwater,
	} {
		if emptyIsHealthy[stream] || counts[stream] > 0 {
			continue
		}
		if span < silenceGrace(stream, info) {
			continue
		}
		h.Silent = append(h.Silent, stream)
	}
	return h
}

// String is the line.
//
// logfmt-ish key=value, because the two things anyone does with it are read it and grep it, and
// both want the keys visible. No spaces inside a value, so `awk` and `cut` work on it too.
func (h heartbeatLine) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s at=%s day=%d up=%.1f%% span=%.1fd sessions=%d",
		heartbeatPrefix, h.At.Format(time.RFC3339), h.Day, h.UpFraction*100, h.SpanDays, h.Sessions)
	for _, stream := range heartbeatStreams {
		fmt.Fprintf(&b, " %s=%d", stream, h.Segments[stream])
	}
	fmt.Fprintf(&b, " selfchecks=%d movers=%d", h.Selfchecks, h.MoverPods)
	// Sorted, so two consecutive days diff cleanly, and comma-separated with no spaces so the
	// whole line stays one awk field per key.
	if len(h.MoverClasses) > 0 {
		parts := make([]string, 0, len(h.MoverClasses))
		for _, class := range slices.Sorted(maps.Keys(h.MoverClasses)) {
			parts = append(parts, fmt.Sprintf("%s:%d", class, h.MoverClasses[class]))
		}
		fmt.Fprintf(&b, " movers_by_class=%s", strings.Join(parts, ","))
	}
	fmt.Fprintf(&b, " footprint=%s/%s", humanBytes(h.Footprint), humanBytes(h.MaxBytes))
	fmt.Fprintf(&b, " degraded=%t drops=%d", h.Degraded, h.Drops)
	// silent= is always present, with an explicit "none", because a key that disappears when
	// there is nothing to report cannot be grepped for the day it appears.
	if len(h.Silent) == 0 {
		b.WriteString(" silent=none")
	} else {
		b.WriteString(" silent=" + strings.Join(h.Silent, ","))
	}
	return b.String()
}
