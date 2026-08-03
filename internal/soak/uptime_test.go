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
	"crypto/hmac"
	"crypto/sha256"
	"math"
	"strings"
	"testing"
	"time"
)

func hmacSHA256(key, msg []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(msg)
	return m.Sum(nil)
}

// TestACollectorDownFromDay4ToDay11IsNotAFortnight is §9, stated as the failure it prevents.
//
// Every stream in such an archive is internally consistent and externally a lie: a metrics series
// with no points between day 4 and day 11 is indistinguishable from a cluster that emitted
// nothing, and a reader drawing a line between the last point before the gap and the first one
// after it is reading an interpolation as an observation.
func TestACollectorDownFromDay4ToDay11IsNotAFortnight(t *testing.T) {
	sessions := []Session{
		{StartedAt: day0, LastBeat: day0.AddDate(0, 0, 4), BeatPeriod: "15s"},
		{StartedAt: day0.AddDate(0, 0, 11), LastBeat: day0.AddDate(0, 0, 14), BeatPeriod: "15s"},
	}
	u := computeUptime(sessions, day0.AddDate(0, 0, 14))

	if got := u.SpanSeconds / 86400; math.Abs(got-14) > 0.01 {
		t.Errorf("span = %.2f days, want 14", got)
	}
	if got := u.ObservedSeconds / 86400; math.Abs(got-7) > 0.01 {
		t.Errorf("observed = %.2f days, want 7 (four plus three)", got)
	}
	if math.Abs(u.Fraction-0.5) > 0.01 {
		t.Errorf("fraction = %.3f, want 0.5", u.Fraction)
	}
	// collect.sh turns anything below 0.9 into a THIN verdict; this must be well under it.
	if u.Fraction >= 0.9 {
		t.Error("a collector that was down for half the soak would be graded COLLECTED")
	}
	if len(u.Gaps) != 1 {
		t.Fatalf("%d gap(s), want 1: %+v", len(u.Gaps), u.Gaps)
	}
	g := u.Gaps[0]
	if !g.From.Equal(day0.AddDate(0, 0, 4)) || !g.To.Equal(day0.AddDate(0, 0, 11)) {
		t.Errorf("the gap is %s..%s, want day 4..day 11", g.From, g.To)
	}
	if !strings.Contains(u.Note, "illusion") {
		t.Errorf("the note does not warn that an unbroken-looking series across the gap is not one: %q",
			u.Note)
	}
}

// TestUptimeCountsTheTrailingSilence: a collector that died an hour before the export must have
// that hour counted against it. Measuring the span from the last beat instead of from now would
// make a dead collector look like a perfect one.
func TestUptimeCountsTheTrailingSilence(t *testing.T) {
	sessions := []Session{{StartedAt: day0, LastBeat: day0.Add(2 * time.Hour), BeatPeriod: "15s"}}
	u := computeUptime(sessions, day0.Add(10*time.Hour))
	if math.Abs(u.Fraction-0.2) > 0.01 {
		t.Errorf("fraction = %.3f, want 0.2 (two hours up out of ten elapsed)", u.Fraction)
	}
	if len(u.Gaps) != 1 || u.Gaps[0].Seconds != (8*time.Hour).Seconds() {
		t.Errorf("the eight hours since the last heartbeat are not a gap: %+v", u.Gaps)
	}
}

func TestUptimeWithNoSessionsSaysNothingWasCollecting(t *testing.T) {
	u := computeUptime(nil, day0)
	if u.Fraction != 0 {
		t.Errorf("fraction = %v with no sessions", u.Fraction)
	}
	if !strings.Contains(u.Note, "nothing was collecting") {
		t.Errorf("the note does not say what happened: %q", u.Note)
	}
}

// TestUptimeReachesTheManifest, which is where collect.sh reads it.
func TestUptimeReachesTheManifest(t *testing.T) {
	s := seedCollector(t, 14, true)
	writeJSON(t, s, fileStarts, []Session{
		{StartedAt: day0, LastBeat: day0.AddDate(0, 0, 4), BeatPeriod: "15s"},
		{StartedAt: day0.AddDate(0, 0, 11), LastBeat: day0.AddDate(0, 0, 14), BeatPeriod: "15s"},
	})
	a := exportToArchive(t, ExportOptions{
		DataDir: s.Dir(), Salt: exportSalt, Now: day0.AddDate(0, 0, 14),
	})
	if a.manifest.CollectorUptimeFraction >= 0.9 {
		t.Errorf("collectorUptimeFraction = %v; collect.sh would grade this fortnight-with-a-"+
			"seven-day-hole as COLLECTED", a.manifest.CollectorUptimeFraction)
	}
	if _, ok := a.files["uptime.json"]; !ok {
		t.Error("uptime.json is not in the archive")
	}
	if !strings.Contains(string(a.files["COLLECTION-REPORT.txt"]), "GAP") {
		t.Error("COLLECTION-REPORT.txt does not name the gap")
	}
}

// TestHeartbeatCheckIsTheLivenessProbe. A process that is alive but has stopped collecting is the
// failure this probe exists for, and it is not one an HTTP handler on the same process would ever
// catch — the handler would answer perfectly while the loop it shares a process with was blocked
// forever on a socket.
func TestHeartbeatCheckIsTheLivenessProbe(t *testing.T) {
	t.Run("no heartbeat file is stale", func(t *testing.T) {
		if got := CheckHeartbeat(t.TempDir(), day0); got == 0 {
			t.Error("a collector that has never written a heartbeat was reported healthy")
		}
	})

	s := newTestStore(t, 1<<20)
	log, err := OpenSessionLog(s, day0, 15*time.Second, "0.6.2")
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Beat(day0); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name  string
		at    time.Time
		alive bool
	}{
		{"immediately", day0, true},
		{"one period later", day0.Add(15 * time.Second), true},
		// Three periods of 15s is 45s, floored to two minutes: one missed beat is an API call
		// that took longer than usual, and killing the collector for it would restart the process
		// and create exactly the gap the probe is meant to detect.
		{"a minute later, inside the floor", day0.Add(time.Minute), true},
		{"five minutes later", day0.Add(5 * time.Minute), false},
		{"an hour later", day0.Add(time.Hour), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := CheckHeartbeat(s.Dir(), tc.at) == 0
			if got != tc.alive {
				t.Errorf("alive = %v at %s, want %v", got, tc.at.Sub(day0), tc.alive)
			}
		})
	}
}

// TestSessionLogRecordsEveryRestart: the gap between one process's last beat and the next one's
// start is computed without the dead process having had to do anything on its way out — which is
// the only way it can work, since the interesting deaths are the ones with no way out.
//
// And the restart here is an UPGRADE, because that is the half of it nothing recorded. The gap was
// always in the file; that the system on the far side of it was a DIFFERENT BUILD was nowhere, and
// every figure computed across the whole span — the mover high-water table above all, since
// marks.json survives the restart the sessions record — quietly covered two systems.
func TestSessionLogRecordsEveryRestart(t *testing.T) {
	s := newTestStore(t, 1<<20)
	first, err := OpenSessionLog(s, day0, 15*time.Second, "0.6.2")
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Beat(day0.Add(4 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	// The process is SIGKILLed here. Nothing is written on the way out, and what comes back up is
	// a different image.
	second, err := OpenSessionLog(s, day0.Add(9*time.Hour), 15*time.Second, "0.6.3")
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Beat(day0.Add(12 * time.Hour)); err != nil {
		t.Fatal(err)
	}

	u, err := ReadUptime(s.Dir(), day0.Add(12*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(u.Sessions) != 2 {
		t.Fatalf("%d session(s) recorded, want 2", len(u.Sessions))
	}
	if len(u.Gaps) != 1 || u.Gaps[0].Seconds != (5*time.Hour).Seconds() {
		t.Errorf("the five hours the collector was dead are not a gap: %+v", u.Gaps)
	}
	if math.Abs(u.Fraction-(7.0/12.0)) > 0.01 {
		t.Errorf("fraction = %.3f, want %.3f", u.Fraction, 7.0/12.0)
	}

	// The session log has to carry the build, or nothing downstream can reconstruct it: the
	// collector's own configuration file is rewritten in full on every start and remembers only
	// the last one.
	for i, want := range []string{"0.6.2", "0.6.3"} {
		if got := u.Sessions[i].OperatorVersion; got != want {
			t.Errorf("session %d recorded version %q, want %q. Without it an upgraded soak reports "+
				"whichever build happened to start last, for the whole fortnight", i, got, want)
		}
	}
	if !u.MixedVersions() {
		t.Error("MixedVersions() is false over two builds; four places ask this question and all " +
			"four would report a single-system archive")
	}
	if len(u.Versions) != 2 {
		t.Fatalf("%d version span(s), want 2: %+v", len(u.Versions), u.Versions)
	}
	for i, want := range []VersionSpan{
		{Version: "0.6.2", From: day0, To: day0.Add(4 * time.Hour),
			ObservedSeconds: (4 * time.Hour).Seconds(), Sessions: 1},
		{Version: "0.6.3", From: day0.Add(9 * time.Hour), To: day0.Add(12 * time.Hour),
			ObservedSeconds: (3 * time.Hour).Seconds(), Sessions: 1},
	} {
		got := u.Versions[i]
		if got.Version != want.Version || !got.From.Equal(want.From) || !got.To.Equal(want.To) ||
			got.ObservedSeconds != want.ObservedSeconds || got.Sessions != want.Sessions {
			t.Errorf("span %d = %+v, want %+v. The BOUNDARIES are the point: they are what says "+
				"which half of the soak each figure belongs to", i, got, want)
		}
	}
}

// TestVersionSpansCollapseOnlyWhatIsConsecutive pins the one decision in this that is not obvious.
//
// Collapsing every session of a version into one entry would be tidier and would erase a rollback
// from the record: a soak that went 0.6.2 → 0.6.3 → 0.6.2 would report two spans, with the middle
// one's dates swallowed, while its gaps sat there in the same file describing three restarts. The
// spans are consecutive-run boundaries, not a grouping.
func TestVersionSpansCollapseOnlyWhatIsConsecutive(t *testing.T) {
	hour := func(n int) time.Time { return day0.Add(time.Duration(n) * time.Hour) }

	t.Run("two restarts on the same build are one span", func(t *testing.T) {
		u := computeUptime([]Session{
			{StartedAt: hour(0), LastBeat: hour(2), OperatorVersion: "0.6.2"},
			{StartedAt: hour(3), LastBeat: hour(5), OperatorVersion: "0.6.2"},
			{StartedAt: hour(6), LastBeat: hour(8), OperatorVersion: "0.6.3"},
		}, hour(8))
		if len(u.Versions) != 2 {
			t.Fatalf("%d span(s), want 2 — a collector that was OOM-killed and came back on the "+
				"same image did not change the system being measured: %+v", len(u.Versions), u.Versions)
		}
		got := u.Versions[0]
		if got.Sessions != 2 || !got.From.Equal(hour(0)) || !got.To.Equal(hour(5)) {
			t.Errorf("the collapsed span is %+v; want it to run hour 0 .. hour 5 over 2 session(s), "+
				"so the restart inside it is still countable", got)
		}
		if got.ObservedSeconds != (4 * time.Hour).Seconds() {
			t.Errorf("observedSeconds = %v, want %v: the hour the collector was DEAD between the two "+
				"sessions must not be inside the span's observed time",
				got.ObservedSeconds, (4 * time.Hour).Seconds())
		}
	})

	t.Run("a rollback is three spans, not two", func(t *testing.T) {
		u := computeUptime([]Session{
			{StartedAt: hour(0), LastBeat: hour(2), OperatorVersion: "0.6.2"},
			{StartedAt: hour(3), LastBeat: hour(5), OperatorVersion: "0.6.3"},
			{StartedAt: hour(6), LastBeat: hour(8), OperatorVersion: "0.6.2"},
		}, hour(8))
		if len(u.Versions) != 3 {
			t.Fatalf("%d span(s), want 3. Merging the two 0.6.2 stretches erases the rollback from "+
				"the record while leaving its gaps in place: %+v", len(u.Versions), u.Versions)
		}
		if !u.Versions[2].From.Equal(hour(6)) {
			t.Errorf("the third span starts at %s, want hour 6 — the moment the cluster went BACK",
				u.Versions[2].From)
		}
	})
}

// TestASessionWithNoVersionReadsAsUnknownAndIsNotDropped.
//
// Every archive written before the field existed has sessions without it, and so does any session
// whose binary could not name itself. That time was still collected: dropping the span would make
// the fortnight's arithmetic disagree with its own uptime, and inferring a version for it would be
// a guess printed next to measurements — the failure §5 spends three paragraphs forbidding for
// mover classes.
func TestASessionWithNoVersionReadsAsUnknownAndIsNotDropped(t *testing.T) {
	u := computeUptime([]Session{
		{StartedAt: day0, LastBeat: day0.Add(4 * time.Hour)},
		{StartedAt: day0.Add(4 * time.Hour), LastBeat: day0.Add(6 * time.Hour),
			OperatorVersion: "0.6.3"},
	}, day0.Add(6*time.Hour))

	if len(u.Versions) != 2 {
		t.Fatalf("%d span(s), want 2: %+v", len(u.Versions), u.Versions)
	}
	if u.Versions[0].Version != "" {
		t.Errorf("the unnamed span reports version %q; a version nobody recorded must not be "+
			"invented", u.Versions[0].Version)
	}
	if u.Versions[0].ObservedSeconds != (4 * time.Hour).Seconds() {
		t.Errorf("the unnamed span observed %v seconds, want %v — four hours of collection were "+
			"dropped because nothing could name the build that did it",
			u.Versions[0].ObservedSeconds, (4 * time.Hour).Seconds())
	}
	if got := versionWord(u.Versions[0].Version); got != "unknown" {
		t.Errorf("an unnamed span renders as %q; a blank in a list of versions reads as a "+
			"formatting fault rather than as an absence", got)
	}
	if !strings.Contains(versionListing(u.Versions), "unknown") {
		t.Errorf("the listing hides the unnamed span: %q", versionListing(u.Versions))
	}
}
