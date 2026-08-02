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
	"encoding/json"
	"fmt"
	"io"
	"time"

	"k8s.io/client-go/discovery"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/CrystalBackup/CrystalBackup/internal/metrics"
	"github.com/CrystalBackup/CrystalBackup/internal/selfcheck"
)

// Collector is the resident loop. One process, one goroutine, one tick.
//
// Single-threaded on purpose. Every stream here writes to the same Store, the cap has to be
// enforced against the total, and a fan of goroutines would buy concurrency this workload does
// not need at the price of a lock discipline that has to be right for fourteen days inside a
// 384Mi limit. The tick is the shortest configured interval and each stream runs when its own is
// due.
type Collector struct {
	Store    *Store
	Sessions *SessionLog
	Info     CollectorInfo

	Scraper    *Scraper
	Aggregator *Aggregator
	HighWater  *HighWater
	Events     *EventStream
	Logs       *LogStream
	State      *StateStream

	// Selfcheck is nil when --redaction-salt-file was not given. See writeSelfchecks in export.go
	// for why the stream is DISABLED rather than degraded in that case.
	Selfcheck *SelfcheckRunner

	MetricsInterval time.Duration
	MoverInterval   time.Duration
	StateInterval   time.Duration

	Progress io.Writer

	nextMetrics, nextMover, nextState, nextEvents, nextLogs time.Time
	// nextHeartbeat is the daily line, kept on its own clock rather than folded into one of the
	// others so its cadence stays a constant a reader can assume. See heartbeat.go.
	nextHeartbeat time.Time
}

// eventPollInterval and logPollInterval are not flags.
//
// Events expire after about an hour, so anything well under that catches every one of them and
// there is no operator decision to make; the same is true of the operator's error lines, which
// are read by timestamp and cannot be missed by polling slower. Two more knobs would be two more
// ways to configure the collector into not collecting.
const (
	eventPollInterval = 2 * time.Minute
	logPollInterval   = 2 * time.Minute
)

// Run is the loop. It returns only when ctx is cancelled.
func (c *Collector) Run(ctx context.Context) error {
	tick := c.tickInterval()
	now := time.Now().UTC()
	c.nextMetrics, c.nextMover = now, now
	c.nextState, c.nextEvents, c.nextLogs = now, now, now

	c.Aggregator.Start(now)
	c.progress("collecting: metrics every %s into %s windows, movers every %s, CR state every %s",
		c.MetricsInterval, c.Info.MetricsResolution, c.MoverInterval, c.StateInterval)
	// One line NOW, before the first tick. An administrator who has just applied the manifest
	// gets their confirmation in seconds rather than a day from now, and the day-one check that
	// hack/soak/README.md asks for has something to look at.
	c.heartbeat(now)

	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			c.progress("shutting down: flushing the open metrics window")
			c.flushWindow(time.Now().UTC())
			return nil
		case <-t.C:
			c.round(ctx, time.Now().UTC())
		}
	}
}

// tickInterval is the shortest thing the loop has to be able to honour.
func (c *Collector) tickInterval() time.Duration {
	tick := c.MetricsInterval
	for _, d := range []time.Duration{c.MoverInterval, c.StateInterval, eventPollInterval, logPollInterval} {
		if d > 0 && d < tick {
			tick = d
		}
	}
	if tick <= 0 || tick > time.Minute {
		tick = time.Minute
	}
	return tick
}

func (c *Collector) round(ctx context.Context, now time.Time) {
	if !now.Before(c.nextMetrics) {
		c.nextMetrics = now.Add(c.MetricsInterval)
		c.scrapeOnce(ctx, now)
	}
	// The window is closed on the CLOCK, not on the scrape: a scrape that failed still ends the
	// window it was in, so a five-minute outage produces five minutes of "ok:0, fail:5" rather
	// than one enormous window whose `n` hides the outage inside it.
	if c.Aggregator.Due(now) {
		c.flushWindow(now)
	}
	// Each stream is skipped when it was not constructed rather than dereferenced. The production
	// path wires all of them, but a stream that could not be built must not take the other four
	// down with it — a collector holding four streams is worth a fortnight, and a CrashLooping
	// one is worth nothing.
	if c.HighWater != nil && !now.Before(c.nextMover) {
		c.nextMover = now.Add(c.MoverInterval)
		c.HighWater.Sample(ctx, now)
	}
	if c.Events != nil && !now.Before(c.nextEvents) {
		c.nextEvents = now.Add(eventPollInterval)
		c.Events.Poll(ctx, now)
	}
	if c.Logs != nil && !now.Before(c.nextLogs) {
		c.nextLogs = now.Add(logPollInterval)
		c.Logs.Poll(ctx, now)
	}
	if c.State != nil && !now.Before(c.nextState) {
		c.nextState = now.Add(c.StateInterval)
		c.State.Snapshot(ctx, now)
	}
	if c.Selfcheck != nil {
		c.Selfcheck.MaybeRun(ctx, now)
	}
	if c.Store.DueForCapCheck(now) {
		if err := c.Store.EnforceCap(now); err != nil {
			c.Store.RecordError("collector", "enforce the footprint cap: "+err.Error(), now)
		}
	}
	// The heartbeat goes LAST, after everything this round was supposed to do. A beat written at
	// the top of the loop would keep saying "alive" while every stream below it failed, which is
	// precisely the "alive but not collecting" state the liveness probe exists to catch.
	if err := c.Sessions.Beat(now); err != nil {
		c.Store.RecordError("collector", "write the heartbeat: "+err.Error(), now)
	}
	// And once a day, the same fact said OUT LOUD, in the log. The file above is read by the
	// liveness probe and by the export; this is the one an administrator can see without either.
	if !now.Before(c.nextHeartbeat) {
		c.heartbeat(now)
	}
}

// heartbeat writes the daily line and arms the next one.
//
// The next beat is scheduled from the time this one was WRITTEN, not from a wall-clock boundary,
// so a collector that was down over midnight does not skip a day silently — its next line lands
// 24h after its last, and the gap in between is itself the signal.
func (c *Collector) heartbeat(now time.Time) {
	c.nextHeartbeat = now.Add(heartbeatInterval)
	c.progress("%s", collectHeartbeat(c.Store, c.Info, now))
}

func (c *Collector) scrapeOnce(ctx context.Context, now time.Time) {
	families, err := c.Scraper.Scrape(ctx)
	if err != nil {
		// Recorded, never swallowed. A hole in the metrics with no explanation reads as "the
		// operator emitted nothing", and this is the record that says otherwise.
		c.Store.RecordError(StreamMetrics, err.Error(), now)
		c.Aggregator.ObserveFailure()
		return
	}
	c.Aggregator.Observe(families, now)
}

// flushWindow closes a resolution window and writes it, then checks the cap — that being the
// moment the footprint actually jumps.
func (c *Collector) flushWindow(now time.Time) {
	core, bulk, day := c.Aggregator.Flush()
	c.Aggregator.Advance(now)
	if err := c.Store.Append(streamMetricsCore, day, core); err != nil {
		c.Store.RecordError(StreamMetrics, "append core metrics: "+err.Error(), now)
	}
	if err := c.Store.Append(StreamMetrics, day, bulk); err != nil {
		c.Store.RecordError(StreamMetrics, "append metrics: "+err.Error(), now)
	}
	if err := c.Store.EnforceCap(now); err != nil {
		c.Store.RecordError("collector", "enforce the footprint cap: "+err.Error(), now)
	}
}

func (c *Collector) progress(format string, args ...any) {
	if c.Progress == nil {
		return
	}
	_, _ = fmt.Fprintf(c.Progress, format+"\n", args...)
}

// ---------------------------------------------------------------------------------------------
// the daily self-check
// ---------------------------------------------------------------------------------------------

// SelfcheckRunner runs `selfcheck` on an interval, through the same code path as the subcommand.
//
// Through the SAME path, not a copy of it: the whole value of the daily report inside a soak is
// that it is comparable with the day-zero baseline the admin took with `crystal-backup selfcheck`
// before installing the collector, and a second implementation would eventually differ from it in
// some field nobody thought to check.
type SelfcheckRunner struct {
	Reader    client.Reader
	Discovery discovery.DiscoveryInterface
	Namespace string
	Salt      []byte
	// SaltSource travels with the salt so every daily report states which method produced it. It
	// is not derivable from the bytes, and a report that named the wrong one would describe a
	// guarantee it does not hold.
	SaltSource string
	Interval   time.Duration
	Store      *Store

	next time.Time
}

// MaybeRun takes a report if one is due.
func (r *SelfcheckRunner) MaybeRun(ctx context.Context, now time.Time) {
	if now.Before(r.next) {
		return
	}
	r.next = now.Add(r.Interval)
	var info selfcheck.ServerInfo
	if r.Discovery != nil {
		info = r.Discovery
	}
	rep, err := selfcheck.Collect(ctx, selfcheck.Options{
		Reader:            r.Reader,
		OperatorNamespace: r.Namespace,
		Now:               now,
		// Hashed, under the SAME salt as the export. That is what makes a namespace the same
		// token in a metric here and in day 9's report — §6's cross-stream identity, which is the
		// whole reason the salt is stable.
		RedactionSalt:       r.Salt,
		RedactionSaltSource: r.SaltSource,
		Discovery:           info,
	})
	if err != nil {
		r.Store.RecordError(StreamSelfcheck, err.Error(), now)
		return
	}
	body, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		r.Store.RecordError(StreamSelfcheck, "encode the self-check: "+err.Error(), now)
		return
	}
	name := dirSelfcheck + "/" + now.UTC().Format(time.RFC3339) + ".json"
	if err := r.Store.WriteFileAtomic(name, body); err != nil {
		r.Store.RecordError(StreamSelfcheck, "write the self-check: "+err.Error(), now)
	}
}

// operatorVersion is what the collector records as the build under test. It is the SAME variable
// the operator stamps into crystalbackup_build_info, so an archive cannot disagree with the
// metric about which build produced it.
func operatorVersion() string { return metrics.Version }
