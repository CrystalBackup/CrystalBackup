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

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/CrystalBackup/CrystalBackup/internal/mover"
)

// ------------------------------------------------------------------------------------------
// What this mover cost, measured by the mover.
//
// THE PROBLEM. Peak mover memory cannot be obtained by polling. metrics-server exposes a pod
// only after its own scrape has covered it, on its own ~15s cadence; a mover lives seconds. A
// four-hour run on a real crucible cluster observed 63 mover pods with lifetimes from 0.9s to
// 17s and recorded ZERO samples on every single one, with metrics-server installed, healthy,
// and returning data for long-lived pods throughout. That is structural: sampling faster does
// not help, because a container that never existed for metrics-server for a whole scrape
// interval has no row to read.
//
// THE ANSWER. The process that ran is the one that knows, and it already writes a structured
// result to its termination message. Reading its own kernel accounting there costs no RBAC, no
// metrics-server and no extra API object, and the number is EXACT rather than sampled.
//
// WHAT IS ACTUALLY PREDICTIVE, which is the part that decides what this reports. The obvious
// candidate — cgroup v2's memory.peak — tracks the peak of memory.current, and memory.current
// counts PAGE CACHE. A backup streams a whole volume through the page cache and every one of
// those pages is charged to the mover's cgroup, so memory.peak drifts up towards the limit
// simply because the cgroup is allowed to keep reclaimable cache. The kernel reclaims that
// cache before it OOM-kills anything, so a limit sized to memory.peak would be raised to cover
// memory that was never the constraint. Reporting it alone would be worse than reporting
// nothing.
//
// So this reports a PAIR, each with its meaning attached:
//
//   - the peak RSS of the restic process (and of this shim, separately), from ru_maxrss. RSS is
//     anonymous memory plus mapped file pages and excludes the streamed page cache, which makes
//     it the figure a memory limit actually has to cover;
//   - the cgroup peak, labelled as the upper bound it is;
//
// plus the two memory.events counters that say whether the limit was ever pressed at all —
// which is what lets a reader tell "the cgroup peak is high because of cache" (max == 0) from
// "the workload is living at its ceiling" (max > 0).
// ------------------------------------------------------------------------------------------

// cgroupRoot is where every container runtime mounts the container's own cgroup. Under a private
// cgroup namespace — the default for cgroup v2 — this directory IS the container's cgroup, so
// memory.peak here is the container's peak and nothing else's.
const cgroupRoot = "/sys/fs/cgroup"

// procSelfCgroup is the fallback for a HOST cgroup namespace, where /sys/fs/cgroup may be the
// whole hierarchy and the container sits at a path inside it. The path comes from the kernel's
// own view of this process, so a file found under it is unambiguously this container's; that is
// the only reason the fallback is safe to have.
const procSelfCgroup = "/proc/self/cgroup"

// The cgroup v2 files this reads. memory.peak needs kernel 5.19+; memory.events has been there
// since v2 itself.
const (
	fileMemoryPeak   = "memory.peak"
	fileMemoryEvents = "memory.events"
)

// The two memory.events keys that matter. `max` counts times the cgroup hit its limit and the
// kernel reclaimed to stay under it; `oom_kill` counts processes the cgroup OOM killer actually
// killed.
const (
	eventKeyMax     = "max"
	eventKeyOOMKill = "oom_kill"
)

// memoryFigures is one mover's memory accounting, before it is stamped onto a MoverResult.
type memoryFigures struct {
	// resticPeakRSS and shimPeakRSS are ru_maxrss for the waited-for children and for this
	// process, in bytes. Zero means "restic never ran" on the child side, which is exactly what
	// a pre-run failure should report.
	resticPeakRSS int64
	shimPeakRSS   int64
	// cgroupPeak, limitHits and oomKills come from the container's cgroup v2 directory, or are
	// zero when it could not be read — which is why sources exists.
	cgroupPeak int64
	limitHits  int64
	oomKills   int64
	// sources is the MemorySource string: which readers succeeded, never inferred from whether
	// a number happens to be zero.
	sources string
	// note explains, in one sentence, why a reader is missing. It goes to the POD LOG, never
	// into the termination message: the message has 4096 bytes and a sentence nobody budgeted
	// for is how a result stops parsing.
	note string
}

// collectMemoryFigures reads everything this process can say about its own memory. root and proc
// are parameters rather than the constants so the whole thing is testable against a directory
// tree; production passes cgroupRoot and procSelfCgroup.
//
// It never fails. Every reader that does not answer simply leaves its fields absent and says so
// through sources — a mover must not fail a backup because it could not measure itself.
func collectMemoryFigures(root, proc string) memoryFigures {
	var f memoryFigures
	var sources []string

	if self, children, err := readMaxRSS(); err == nil {
		f.shimPeakRSS, f.resticPeakRSS = self, children
		sources = append(sources, mover.MemorySourceRusage)
	} else {
		f.note = "peak RSS unavailable: " + err.Error()
	}

	dir, err := resolveCgroupV2(root, proc)
	if err != nil {
		f.note = strings.TrimSpace(f.note + " " + err.Error())
		f.sources = strings.Join(sources, "+")
		return f
	}
	peak, err := readCgroupUint(filepath.Join(dir, fileMemoryPeak))
	if err != nil {
		f.note = strings.TrimSpace(f.note + " " + err.Error())
		f.sources = strings.Join(sources, "+")
		return f
	}
	f.cgroupPeak = peak
	// The counters are read from the SAME directory in the same pass, so "MemorySource names
	// cgroup2" is a true statement about all three numbers at once. A memory.events that will not
	// parse leaves the counters at zero and is noted, but does not retract the peak: the peak was
	// read and is real.
	if events, err := readCgroupEvents(filepath.Join(dir, fileMemoryEvents)); err != nil {
		f.note = strings.TrimSpace(f.note + " " + err.Error())
	} else {
		f.limitHits, f.oomKills = events[eventKeyMax], events[eventKeyOOMKill]
	}
	sources = append(sources, mover.MemorySourceCgroup2)
	f.sources = strings.Join(sources, "+")
	return f
}

// resolveCgroupV2 finds THIS CONTAINER's cgroup v2 directory, or refuses.
//
// Two candidates, in order, and the order is the safety property. A cgroup peak that belongs to
// the node rather than to this container would be wrong by orders of magnitude while looking
// entirely plausible, so every path here either resolves to a directory that provably belongs to
// this process or resolves to nothing at all.
//
//  1. root itself. Under a private cgroup namespace the mount IS the container's own cgroup.
//     This cannot accidentally be the node's: the cgroup v2 ROOT cgroup has no memory.peak (nor
//     memory.current or memory.max) at all, so a container that can see the whole host hierarchy
//     finds nothing here and falls through.
//  2. root + the path /proc/self/cgroup reports, for a host cgroup namespace. That path is the
//     kernel's own answer to "which cgroup is this process in", so a memory.peak found under it
//     is this container's by construction.
//
// CGROUP V1 IS DELIBERATELY REFUSED. It is not that the counter is missing — v1 has
// memory.max_usage_in_bytes, which is the same peak-of-usage including page cache. It is that on
// v1 the container conventionally runs in the HOST cgroup namespace, where /sys/fs/cgroup/memory
// may be the node's root memory cgroup, may be a bind mount of the container's own directory,
// and offers nothing to tell those two apart. The node's peak reported as a mover's peak is
// precisely the order-of-magnitude lie this whole measurement exists to avoid, so absent is the
// honest answer. The RSS peaks are unaffected: they are per-process kernel accounting and owe
// nothing to cgroups, so a v1 node still gets the number that actually sizes a limit.
func resolveCgroupV2(root, proc string) (string, error) {
	if fileReadable(filepath.Join(root, fileMemoryPeak)) {
		return root, nil
	}
	if rel, err := cgroupV2Path(proc); err == nil && rel != "" && rel != "/" {
		if dir := filepath.Join(root, filepath.Clean(rel)); fileReadable(filepath.Join(dir, fileMemoryPeak)) {
			return dir, nil
		}
	}
	if isDir(filepath.Join(root, "memory")) {
		return "", errors.New("cgroup peak not reported: this node runs cgroup v1, whose " +
			"memory.max_usage_in_bytes cannot be attributed to this container from inside it")
	}
	return "", fmt.Errorf("cgroup peak not reported: no readable %s under %s (kernel < 5.19, or "+
		"a cgroup layout this shim will not guess at)", fileMemoryPeak, root)
}

// cgroupV2Path extracts the unified-hierarchy path from /proc/self/cgroup, whose v2 line is
// `0::<path>`. A v1-only host has no such line and yields "".
func cgroupV2Path(proc string) (string, error) {
	raw, err := os.ReadFile(filepath.Clean(proc))
	if err != nil {
		return "", err
	}
	for line := range strings.SplitSeq(string(raw), "\n") {
		if after, ok := strings.CutPrefix(strings.TrimSpace(line), "0::"); ok {
			return after, nil
		}
	}
	return "", nil
}

// readCgroupUint reads a cgroup file holding a single unsigned integer.
//
// "max" is rejected rather than mapped to a large number: memory.peak never holds it, and a
// silent translation of a sentinel into a byte count is how an unlimited cgroup comes to report
// eight exabytes of peak memory.
func readCgroupUint(path string) (int64, error) {
	raw, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return 0, fmt.Errorf("cgroup peak not reported: %s is unreadable (%v)", filepath.Base(path), err)
	}
	v, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("cgroup peak not reported: %s does not hold a number (%q)",
			filepath.Base(path), strings.TrimSpace(string(raw)))
	}
	if v <= 0 {
		// A live cgroup's peak is never zero — this process is running in it. Zero means the file
		// is not what this code thinks it is, and reporting it would read as "used no memory".
		return 0, fmt.Errorf("cgroup peak not reported: %s reads %d, which no running cgroup does",
			filepath.Base(path), v)
	}
	return v, nil
}

// readCgroupEvents parses memory.events, whose format is `<key> <count>` per line.
func readCgroupEvents(path string) (map[string]int64, error) {
	raw, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil, fmt.Errorf("cgroup memory events not reported: %s is unreadable (%v)",
			filepath.Base(path), err)
	}
	out := map[string]int64{}
	for line := range strings.SplitSeq(string(raw), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		if v, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64); err == nil {
			out[key] = v
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("cgroup memory events not reported: %s parsed to nothing",
			filepath.Base(path))
	}
	return out, nil
}

func fileReadable(path string) bool {
	info, err := os.Stat(filepath.Clean(path))
	return err == nil && info.Mode().IsRegular()
}

func isDir(path string) bool {
	info, err := os.Stat(filepath.Clean(path))
	return err == nil && info.IsDir()
}

// withMemoryFigures stamps the figures onto the result the controller reads.
//
// It is separate from the collection so the size budget is testable without a cgroup, and it is
// applied at the LAST moment before the result is fitted and written, so the cgroup peak covers
// everything this process did — including a manifest apply that runs after restic has exited.
func withMemoryFigures(r mover.MoverResult, f memoryFigures) mover.MoverResult {
	r.PeakRSSBytes = f.resticPeakRSS
	r.ShimPeakRSSBytes = f.shimPeakRSS
	r.CgroupPeakBytes = f.cgroupPeak
	r.MemoryLimitHits = f.limitHits
	r.MemoryOOMKills = f.oomKills
	r.MemorySource = f.sources
	return r
}

// finalize is everything that happens to a result between "the work is done" and "the bytes are
// written": the memory figures go on, and then the whole thing is FITTED.
//
// Fitting here, rather than only where the manifest report is built, is the size guard. The
// manifest paths fit their per-resource report BEFORE these fields exist, so re-fitting at the
// end is what makes "inside the kubelet's 4096-byte cap" a statement about the message as it is
// actually written. Past the cap the kubelet TRUNCATES rather than rejects, the controller reads
// JSON that will not parse, and ParseMoverResult (correctly) calls that a mover that died before
// reporting — turning a successful restore into a recorded failure.
//
// HOW MUCH THIS ORDER BUYS, honestly: today, not much. Fit trims to a budget that already leaves
// resultSizeMargin (256 bytes) of slack, and the memory block is 219 bytes at its absolute
// largest, so stamping after a fit would still land inside the cap — the reverse order was tried
// and no test could tell. What this order buys is that the slack stays available for what it is
// FOR: strings this code does not control. The assumption is not left implicit either;
// mover.TestMemoryFiguresFitTheSizeMargin fails if the block ever outgrows the margin.
//
// Separate from report() because report() ends in os.Exit and cannot be called from a test.
func finalize(result mover.MoverResult, figures memoryFigures) mover.MoverResult {
	return withMemoryFigures(result, figures).Fit()
}

// describeMemory is the pod-log sentence, where the MEANING lives. The termination message
// carries the numbers under a 4096-byte cap; a human reading `kubectl logs` gets the sentence
// that keeps them from sizing a limit against the page cache.
func describeMemory(f memoryFigures) string {
	if f.sources == "" {
		return "crystal-mover: memory not measured: " + f.note
	}
	var b strings.Builder
	fmt.Fprintf(&b, "crystal-mover: peak RSS restic=%s shim=%s (anonymous + mapped, the figure a "+
		"memory limit must cover)", humanBytes(f.resticPeakRSS), humanBytes(f.shimPeakRSS))
	if strings.Contains(f.sources, mover.MemorySourceCgroup2) {
		fmt.Fprintf(&b, "; cgroup peak=%s (INCLUDES reclaimable page cache — an upper bound, not a "+
			"sizing target); limit hits=%d, cgroup OOM kills=%d",
			humanBytes(f.cgroupPeak), f.limitHits, f.oomKills)
	}
	if f.note != "" {
		b.WriteString("; " + f.note)
	}
	return b.String()
}

// humanBytes renders a byte count the way the sizing table does (MiB/GiB), because the number a
// reader compares it against is written `4Gi`.
func humanBytes(n int64) string {
	switch {
	case n <= 0:
		return "n/a"
	case n >= 1<<30:
		return fmt.Sprintf("%.2fGi", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1fMi", float64(n)/(1<<20))
	default:
		return fmt.Sprintf("%dB", n)
	}
}
