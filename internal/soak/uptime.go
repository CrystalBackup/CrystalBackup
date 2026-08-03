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
	"os"
	"path/filepath"
	"slices"
	"time"
)

// ---------------------------------------------------------------------------------------------
// §9 — the collector observes itself.
//
// A collector that died on day 4 and was restarted on day 11 must not produce an archive that
// reads like a fortnight. Every stream in that archive would be internally consistent and
// externally a lie: a metrics series with no points between day 4 and day 11 is indistinguishable
// from a cluster that emitted nothing, and a reader drawing a line between the last point before
// the gap and the first one after it would be reading an interpolation as an observation.
//
// So the process records its own liveness, the gaps are computed from it, and MANIFEST.json
// carries the fraction. hack/soak/collect.sh turns anything below 0.9 into a THIN verdict.
// ---------------------------------------------------------------------------------------------

// Session is one run of the collector process.
type Session struct {
	StartedAt time.Time `json:"startedAt"`
	LastBeat  time.Time `json:"lastBeat"`
	// BeatPeriod is how often this session promised to write a heartbeat. Recorded per session
	// because it is a function of the flags that session ran with, and --heartbeat-check has to
	// judge freshness against the period the RUNNING collector uses, not against a constant that
	// would be wrong for anyone who changed --mover-sample-interval.
	BeatPeriod string `json:"beatPeriod,omitempty"`
	// OperatorVersion is the build that ran THIS session, and it is here for the same reason
	// BeatPeriod is: it is a property of one process, not of the soak.
	//
	// Without it an upgraded soak lies by omission. CollectorInfo.OperatorVersion is rewritten in
	// full on every collector start, so the archive reports whichever build happened to start
	// last — a fortnight that ran six days of one version and eight of another comes out as a
	// single flat version, while highwater/marks.json (which persists across restarts) silently
	// mixes the two systems into one peak. The gap between the sessions is recorded; the fact
	// that the measured system CHANGED at that gap was not, anywhere.
	//
	// `omitempty` on purpose: an archive written before this field existed has sessions without
	// it, and those must read as "unknown", never as a version they cannot attest to.
	OperatorVersion string `json:"operatorVersion,omitempty"`
}

// VersionSpan is a stretch of the soak during which one build was collecting, derived by
// collapsing consecutive sessions that carry the same version.
//
// Consecutive, not grouped: a soak that went 0.6.2 → 0.6.3 → 0.6.2 (a rollback) produces THREE
// spans, because merging the two 0.6.2 stretches into one would erase the rollback from the
// record while leaving its gap in place.
type VersionSpan struct {
	// Version is the build, or "" when the sessions predate this field. Rendered as `unknown`
	// downstream and never dropped — a span nobody can name is still time that was collected.
	Version         string    `json:"version"`
	From            time.Time `json:"from"`
	To              time.Time `json:"to"`
	ObservedSeconds float64   `json:"observedSeconds"`
	Sessions        int       `json:"sessions"`
}

// Gap is a stretch during which nothing was collecting.
type Gap struct {
	From    time.Time `json:"from"`
	To      time.Time `json:"to"`
	Seconds float64   `json:"seconds"`
}

// Uptime is uptime.json, and the source of MANIFEST.json's collectorUptimeFraction.
type Uptime struct {
	FirstStartedAt  time.Time `json:"firstStartedAt"`
	LastBeatAt      time.Time `json:"lastBeatAt"`
	Sessions        []Session `json:"sessions"`
	Gaps            []Gap     `json:"gaps"`
	SpanSeconds     float64   `json:"spanSeconds"`
	ObservedSeconds float64   `json:"observedSeconds"`
	Fraction        float64   `json:"fraction"`
	Note            string    `json:"note"`
	// Versions is which builds produced this archive, in order. One entry is the ordinary case
	// and says nothing new; more than one is a finding, because every figure derived across the
	// whole span — the mover high-water table above all — then describes more than one system.
	Versions []VersionSpan `json:"versions"`
}

// MixedVersions reports whether more than one build contributed to this archive.
//
// Named rather than left as `len(...) > 1` at each call site: four places have to agree about
// what "mixed" means (the manifest, the report header, the §5 section and the status screen), and
// a fifth will be added by somebody who does not read the other four.
func (u Uptime) MixedVersions() bool { return len(u.Versions) > 1 }

// heartbeat is the tiny file --heartbeat-check reads, and it is deliberately NOT the sessions
// file: the liveness probe runs `/manager soak-collect --heartbeat-check` every five minutes in
// a distroless container, and it should read one small document rather than parse a growing one.
type heartbeat struct {
	At         time.Time `json:"at"`
	StartedAt  time.Time `json:"startedAt"`
	BeatPeriod string    `json:"beatPeriod"`
}

// SessionLog owns both files.
type SessionLog struct {
	store    *Store
	period   time.Duration
	sessions []Session
}

// OpenSessionLog appends a new session and returns the log.
//
// The previous session's last heartbeat is already on disk, so the gap between it and this start
// is computed without the dead process having had to do anything on its way out — which is the
// only way it can work, since the interesting deaths are the ones with no way out: an OOM kill, a
// node that went away, a SIGKILL after the grace period.
// The operatorVersion argument is the build starting this session. It is passed in rather than
// read here so this file stays free of the metrics package, and so a test can drive two different
// versions through it without linker tricks.
func OpenSessionLog(store *Store, now time.Time, beatPeriod time.Duration, operatorVersion string) (*SessionLog, error) {
	l := &SessionLog{store: store, period: beatPeriod}
	if err := l.load(); err != nil {
		return nil, err
	}
	l.sessions = append(l.sessions, Session{
		StartedAt: now.UTC(), LastBeat: now.UTC(), BeatPeriod: beatPeriod.String(),
		OperatorVersion: operatorVersion,
	})
	return l, l.flush(now)
}

// Beat records that the collector is still collecting. Called every loop; it is what the liveness
// probe reads and what §9's arithmetic is built from.
//
// A process that is alive but has stopped collecting is the failure the probe exists for, and it
// is not one an HTTP handler on the same process would ever catch — the handler would answer
// perfectly while the loop it shares a process with was blocked forever on a socket.
func (l *SessionLog) Beat(now time.Time) error {
	if len(l.sessions) == 0 {
		return nil
	}
	l.sessions[len(l.sessions)-1].LastBeat = now.UTC()
	return l.flush(now)
}

func (l *SessionLog) flush(now time.Time) error {
	body, err := json.MarshalIndent(l.sessions, "", "  ")
	if err != nil {
		return err
	}
	if err := l.store.WriteFileAtomic(fileStarts, body); err != nil {
		return err
	}
	beat, err := json.Marshal(heartbeat{
		At:         now.UTC(),
		StartedAt:  l.sessions[len(l.sessions)-1].StartedAt,
		BeatPeriod: l.period.String(),
	})
	if err != nil {
		return err
	}
	return l.store.WriteFileAtomic(fileHeartbeat, beat)
}

func (l *SessionLog) load() error {
	raw, err := readFile(l.store.Dir(), fileStarts)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return json.Unmarshal(raw, &l.sessions)
}

// Uptime computes §9 from whatever sessions are on disk.
func ReadUptime(dir string, now time.Time) (Uptime, error) {
	var sessions []Session
	raw, err := readFile(dir, fileStarts)
	if err == nil {
		if err := json.Unmarshal(raw, &sessions); err != nil {
			return Uptime{}, fmt.Errorf("the collector's session log will not parse: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return Uptime{}, err
	}
	return computeUptime(sessions, now), nil
}

func computeUptime(sessions []Session, now time.Time) Uptime {
	u := Uptime{Sessions: sessions, Gaps: []Gap{}}
	if len(sessions) == 0 {
		u.Note = "the collector never recorded a session: nothing was collecting, and no part of " +
			"this archive can be read as covering any period of time."
		return u
	}
	slices.SortFunc(u.Sessions, func(a, b Session) int {
		return a.StartedAt.Compare(b.StartedAt)
	})
	u.FirstStartedAt = u.Sessions[0].StartedAt
	for i, s := range u.Sessions {
		observed := s.LastBeat.Sub(s.StartedAt).Seconds()
		if observed > 0 {
			u.ObservedSeconds += observed
		}
		if s.LastBeat.After(u.LastBeatAt) {
			u.LastBeatAt = s.LastBeat
		}
		if i+1 < len(u.Sessions) {
			from, to := s.LastBeat, u.Sessions[i+1].StartedAt
			if to.After(from) {
				u.Gaps = append(u.Gaps, Gap{From: from, To: to, Seconds: to.Sub(from).Seconds()})
			}
		}
		// The version spans, collapsed in the SAME pass over the same already-sorted sessions, so
		// they cannot disagree with the gaps about ordering. Consecutive-only: see VersionSpan.
		if n := len(u.Versions); n > 0 && u.Versions[n-1].Version == s.OperatorVersion {
			sp := &u.Versions[n-1]
			sp.To = s.LastBeat
			sp.Sessions++
			if observed > 0 {
				sp.ObservedSeconds += observed
			}
			continue
		}
		span := VersionSpan{
			Version: s.OperatorVersion, From: s.StartedAt, To: s.LastBeat, Sessions: 1,
		}
		if observed > 0 {
			span.ObservedSeconds = observed
		}
		u.Versions = append(u.Versions, span)
	}
	// The span ends at NOW and not at the last beat. A collector that died an hour before the
	// export must have that hour counted against it — measuring the span from the last beat would
	// make a dead collector look like a perfect one, which is the exact reading §9 exists to
	// prevent.
	end := now.UTC()
	if u.LastBeatAt.After(end) {
		end = u.LastBeatAt
	}
	u.SpanSeconds = end.Sub(u.FirstStartedAt).Seconds()
	if trailing := end.Sub(u.LastBeatAt).Seconds(); trailing > 0 {
		u.Gaps = append(u.Gaps, Gap{From: u.LastBeatAt, To: end, Seconds: trailing})
	}
	switch {
	case u.SpanSeconds <= 0:
		u.Fraction = 1
	default:
		u.Fraction = u.ObservedSeconds / u.SpanSeconds
	}
	if u.Fraction > 1 {
		u.Fraction = 1
	}
	switch {
	case len(u.Gaps) == 0:
		u.Note = "the collector was up for the whole period this archive covers."
	default:
		u.Note = fmt.Sprintf(
			"the collector was up for %.1f%% of the period this archive covers, across %d session(s) "+
				"with %d gap(s). Nothing that happened inside a gap is here, and a series that looks "+
				"unbroken across one is an illusion of the plot rather than a fact about the cluster.",
			u.Fraction*100, len(u.Sessions), len(u.Gaps))
	}
	return u
}

// heartbeatGrace multiplies the collector's own beat period to get the staleness threshold, with
// a floor.
//
// Three periods, not one: a single missed beat is an API call that took longer than usual, and a
// liveness probe that killed the collector for it would restart the process — losing the in-memory
// aggregation window and creating exactly the gap it is supposed to detect. The manifest's
// failureThreshold of 3 then makes this nine periods before a restart, which is the right
// conservatism for a process whose restart costs data.
const (
	heartbeatGrace    = 3
	heartbeatMinAge   = 2 * time.Minute
	heartbeatCheckMax = 30 * time.Minute
)

// CheckHeartbeat is `soak-collect --heartbeat-check`: the liveness probe.
//
// It prints NOTHING and returns an exit code, because that is what an exec probe consumes, and
// because a probe that wrote to stdout in a distroless container would just fill the kubelet's
// event log. A missing heartbeat file is stale — a collector that has not written one yet has not
// started collecting.
func CheckHeartbeat(dir string, now time.Time) int {
	raw, err := os.ReadFile(filepath.Join(dir, filepath.Clean(fileHeartbeat))) // #nosec G304
	if err != nil {
		return 1
	}
	var hb heartbeat
	if err := json.Unmarshal(raw, &hb); err != nil {
		return 1
	}
	period, err := time.ParseDuration(hb.BeatPeriod)
	if err != nil || period <= 0 {
		period = heartbeatMinAge
	}
	maxAge := min(max(time.Duration(heartbeatGrace)*period, heartbeatMinAge), heartbeatCheckMax)
	if now.UTC().Sub(hb.At) > maxAge {
		return 1
	}
	return 0
}
