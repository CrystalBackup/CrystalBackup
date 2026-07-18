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

package schedule

import (
	"testing"
	"time"
)

func mustParse(t *testing.T, expr, tz string) *Schedule {
	t.Helper()
	s, err := Parse(expr, tz)
	if err != nil {
		t.Fatalf("Parse(%q, %q) errored: %v", expr, tz, err)
	}
	return s
}

func at(h, m, s int) time.Time { return time.Date(2026, 7, 17, h, m, s, 0, time.UTC) }

func dur(d time.Duration) *time.Duration { return &d }

func TestParse(t *testing.T) {
	for _, tc := range []struct {
		name, expr, tz string
		wantErr        bool
	}{
		{"standard 5-field", "0 2 * * *", "", false},
		{"macro", "@hourly", "", false},
		{"step", "*/15 * * * *", "", false},
		{"valid timezone", "0 2 * * *", "Europe/Paris", false},
		{"invalid cron", "not a cron", "", true},
		{"too many fields", "0 0 0 0 0 0 0", "", true},
		{"invalid timezone", "0 2 * * *", "Nowhere/Nope", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(tc.expr, tc.tz)
			if tc.wantErr && err == nil {
				t.Fatal("expected an error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestNext(t *testing.T) {
	everyMinute := mustParse(t, "* * * * *", "")
	got := everyMinute.Next(at(10, 0, 30))
	if want := at(10, 1, 0); !got.Equal(want) {
		t.Fatalf("Next = %v, want %v", got, want)
	}
	// Strictly after: from an exact tick, the NEXT one.
	got = everyMinute.Next(at(10, 1, 0))
	if want := at(10, 2, 0); !got.Equal(want) {
		t.Fatalf("Next from exact tick = %v, want %v", got, want)
	}
}

func TestDueTick(t *testing.T) {
	everyMinute := mustParse(t, "* * * * *", "")
	daily := mustParse(t, "0 0 * * *", "") // fires at 00:00

	tests := []struct {
		name     string
		sched    *Schedule
		after    time.Time
		now      time.Time
		deadline *time.Duration
		wantOK   bool
		wantTick time.Time
	}{
		{
			name:  "no new tick since the last fired one",
			sched: everyMinute, after: at(10, 0, 0), now: at(10, 0, 30),
			wantOK: false,
		},
		{
			name:  "exactly one tick due",
			sched: everyMinute, after: at(10, 0, 0), now: at(10, 1, 30),
			wantOK: true, wantTick: at(10, 1, 0),
		},
		{
			name:  "several missed ticks collapse to the latest (catch-up to one run)",
			sched: everyMinute, after: at(10, 0, 0), now: at(10, 5, 30),
			wantOK: true, wantTick: at(10, 5, 0),
		},
		{
			name:  "fresh tick within the starting deadline is due",
			sched: everyMinute, after: at(10, 0, 0), now: at(10, 5, 30), deadline: dur(90 * time.Second),
			wantOK: true, wantTick: at(10, 5, 0),
		},
		{
			name:  "stale latest tick past the starting deadline is skipped",
			sched: daily, after: at(0, 0, 0).Add(-24 * time.Hour), now: at(10, 30, 0), deadline: dur(time.Hour),
			// latest daily tick is today 00:00, now is 10:30 → 10.5h old > 1h deadline.
			wantOK: false,
		},
		{
			name:  "sparse tick with no deadline fires however old",
			sched: daily, after: at(0, 0, 0).Add(-48 * time.Hour), now: at(10, 30, 0),
			wantOK: true, wantTick: at(0, 0, 0),
		},
		{
			name:  "baseline in the future yields nothing due",
			sched: everyMinute, after: at(11, 0, 0), now: at(10, 0, 0),
			wantOK: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := tc.sched.DueTick(tc.after, tc.now, tc.deadline)
			if ok != tc.wantOK {
				t.Fatalf("DueTick ok = %v, want %v (tick %v)", ok, tc.wantOK, got)
			}
			if ok && !got.Equal(tc.wantTick) {
				t.Fatalf("DueTick = %v, want %v", got, tc.wantTick)
			}
		})
	}
}

func TestJitterOffset(t *testing.T) {
	const window = 60 * time.Second

	// Deterministic: same seed ⇒ same offset.
	a := JitterOffset("cbs-uid-1234", window)
	b := JitterOffset("cbs-uid-1234", window)
	if a != b {
		t.Fatalf("JitterOffset not deterministic: %v != %v", a, b)
	}

	// Bounded to [0, window).
	if a < 0 || a >= window {
		t.Fatalf("JitterOffset %v out of [0, %v)", a, window)
	}

	// Distinct seeds generally differ (guards against a constant/zero hash).
	if JitterOffset("seed-a", window) == JitterOffset("seed-b", window) {
		t.Fatal("JitterOffset produced the same offset for two different seeds")
	}

	// Non-positive window ⇒ no offset.
	if got := JitterOffset("seed", 0); got != 0 {
		t.Fatalf("JitterOffset with zero window = %v, want 0", got)
	}
}
