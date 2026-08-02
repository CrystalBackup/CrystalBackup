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
	log, err := OpenSessionLog(s, day0, 15*time.Second)
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
func TestSessionLogRecordsEveryRestart(t *testing.T) {
	s := newTestStore(t, 1<<20)
	first, err := OpenSessionLog(s, day0, 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Beat(day0.Add(4 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	// The process is SIGKILLed here. Nothing is written on the way out.
	second, err := OpenSessionLog(s, day0.Add(9*time.Hour), 15*time.Second)
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
}
