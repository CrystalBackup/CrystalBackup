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

// Package schedule is the pure, clock-free core of the ClusterBackupSchedule
// controller's timing: parsing a CronJob-style expression in a timezone,
// computing the next activation, deciding which (single) past activation is due
// to fire now, and deriving a deterministic per-schedule jitter offset. Keeping
// it free of any client or wall clock (every function takes the relevant instant
// as a parameter) makes the catch-up and deadline logic exhaustively
// unit-testable without an API server or a fake clock.
package schedule

import (
	"fmt"
	"hash/fnv"
	"time"

	"github.com/robfig/cron/v3"
)

// maxCatchupTicks bounds the forward scan in DueTick so a lost status or a very
// old baseline cannot make it walk an unbounded history. In steady state the
// baseline is at most one period behind now (the controller derives it from the
// newest surviving run), so the scan is a handful of iterations; this cap only
// ever bites in a pathological clock-skew / long-downtime case, where firing a
// slightly-old activation once (which then advances the baseline) is a safe,
// self-healing outcome.
const maxCatchupTicks = 1000

// Schedule is a parsed cron expression bound to a timezone.
type Schedule struct {
	cron cron.Schedule
	loc  *time.Location
}

// Parse parses a standard 5-field cron expression (the CronJob dialect, with
// macros like @hourly / @daily) to be evaluated in the given IANA timezone (empty
// ⇒ UTC).
func Parse(expr, timezone string) (*Schedule, error) {
	loc := time.UTC
	if timezone != "" {
		l, err := time.LoadLocation(timezone)
		if err != nil {
			return nil, fmt.Errorf("load timezone %q: %w", timezone, err)
		}
		loc = l
	}
	c, err := cron.ParseStandard(expr)
	if err != nil {
		return nil, fmt.Errorf("parse cron %q: %w", expr, err)
	}
	return &Schedule{cron: c, loc: loc}, nil
}

// Next returns the first activation strictly after t, evaluated against the
// schedule's timezone.
func (s *Schedule) Next(t time.Time) time.Time {
	return s.cron.Next(t.In(s.loc))
}

// DueTick reports the most recent activation that should fire at wall-clock now
// but has not fired yet, where after is the last already-fired activation (or, on
// a fresh schedule, its creation time — so a schedule never fires "on apply").
//
// It bounds catch-up to a SINGLE run: it returns only the latest due activation,
// never a backlog. It honours startingDeadline: an activation older than
// now-deadline is stale and NOT returned (a nil deadline means unbounded, still
// internally capped by maxCatchupTicks). ok=false means nothing is due.
func (s *Schedule) DueTick(after, now time.Time, deadline *time.Duration) (time.Time, bool) {
	var last time.Time
	found := false
	// Next(after) is strictly after `after`, so every tick considered is one we have
	// not fired; keep the latest that is at or before now.
	t := s.Next(after)
	for i := 0; i < maxCatchupTicks && !t.After(now); i++ {
		last = t
		found = true
		t = s.Next(t)
	}
	if !found {
		return time.Time{}, false
	}
	if deadline != nil && now.Sub(last) > *deadline {
		return time.Time{}, false
	}
	return last, true
}

// JitterOffset derives a deterministic offset in [0, window) from seed (the
// schedule's stable identity — its UID or name), so many schedules sharing a cron
// expression fire at spread-out instants instead of stampeding the same tick,
// while any single schedule always jitters by the same amount (reproducible,
// testable). A non-positive window yields no offset.
func JitterOffset(seed string, window time.Duration) time.Duration {
	if window <= 0 {
		return 0
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(seed))
	return time.Duration(h.Sum64() % uint64(window))
}
