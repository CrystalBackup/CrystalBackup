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
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/CrystalBackup/CrystalBackup/internal/mover"
)

// cgroupV2Dir writes a cgroup v2 directory the way the kernel presents one.
func cgroupV2Dir(t *testing.T, dir, peak, events string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if peak != "" {
		if err := os.WriteFile(filepath.Join(dir, fileMemoryPeak), []byte(peak), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if events != "" {
		if err := os.WriteFile(filepath.Join(dir, fileMemoryEvents), []byte(events), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func procFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cgroup")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// realEvents is memory.events as the kernel writes it: one `key value` per line.
const realEvents = "low 0\nhigh 0\nmax 12\noom 1\noom_kill 1\n"

// TestCgroupPeakAndCountersComeFromTheContainersOwnCgroup — the private-cgroup-namespace case,
// which is what a cgroup v2 node running containerd actually gives a pod: /sys/fs/cgroup IS the
// container's own cgroup, so the peak here is the container's and nobody else's.
func TestCgroupPeakAndCountersComeFromTheContainersOwnCgroup(t *testing.T) {
	root := t.TempDir()
	cgroupV2Dir(t, root, "3221225472\n", realEvents)

	f := collectMemoryFigures(root, procFile(t, "0::/\n"))

	if f.cgroupPeak != 3<<30 {
		t.Errorf("cgroupPeak = %d, want %d", f.cgroupPeak, int64(3)<<30)
	}
	if f.limitHits != 12 {
		t.Errorf("limitHits = %d, want 12 (memory.events `max`)", f.limitHits)
	}
	if f.oomKills != 1 {
		t.Errorf("oomKills = %d, want 1 (memory.events `oom_kill`)", f.oomKills)
	}
	if !strings.Contains(f.sources, mover.MemorySourceCgroup2) {
		t.Errorf("sources = %q, want it to name %q: the counters are only meaningful when it does",
			f.sources, mover.MemorySourceCgroup2)
	}
	// The RSS peaks come from the kernel's per-process accounting, so they are there regardless.
	if !strings.Contains(f.sources, mover.MemorySourceRusage) {
		t.Errorf("sources = %q, want it to name %q", f.sources, mover.MemorySourceRusage)
	}
	if f.shimPeakRSS <= 0 {
		t.Errorf("shimPeakRSS = %d; this process is running and has a resident set", f.shimPeakRSS)
	}
	// And the pod-log sentence must carry the MEANING, or the number invites the wrong fix.
	desc := describeMemory(f)
	for _, want := range []string{"page cache", "not a sizing target", "memory limit must cover"} {
		if !strings.Contains(desc, want) {
			t.Errorf("the pod-log line is missing %q:\n%s", want, desc)
		}
	}
}

// TestHostCgroupNamespaceResolvesThroughProcSelfCgroup. When the container can see the whole
// hierarchy, /sys/fs/cgroup/memory.peak does not exist (the cgroup v2 ROOT has no memory files),
// and the container's own directory is the one the kernel names in /proc/self/cgroup. Resolving
// through that path is safe by construction: the path is the kernel's answer to "which cgroup is
// this process in".
func TestHostCgroupNamespaceResolvesThroughProcSelfCgroup(t *testing.T) {
	root := t.TempDir()
	rel := "/kubepods.slice/kubepods-burstable.slice/crio-abc123.scope"
	cgroupV2Dir(t, filepath.Join(root, rel), "1073741824\n", realEvents)

	f := collectMemoryFigures(root, procFile(t, "0::"+rel+"\n"))

	if f.cgroupPeak != 1<<30 {
		t.Errorf("cgroupPeak = %d, want %d: the container's own directory was not found through "+
			"/proc/self/cgroup", f.cgroupPeak, int64(1)<<30)
	}
}

// TestCgroupV1IsAbsentNotGuessed. v1 HAS an equivalent counter — memory.max_usage_in_bytes — and
// this deliberately does not read it: in the host cgroup namespace v1 deployments use,
// /sys/fs/cgroup/memory can be the NODE's root memory cgroup, and nothing visible from inside the
// container distinguishes that from a bind mount of the container's own directory. A node-wide
// peak reported as a mover's peak is the order-of-magnitude lie this measurement exists to avoid.
//
// The RSS peaks survive, which is the point: the number that actually sizes a limit owes nothing
// to cgroups and a v1 node still gets it.
func TestCgroupV1IsAbsentNotGuessed(t *testing.T) {
	root := t.TempDir()
	v1 := filepath.Join(root, "memory")
	if err := os.MkdirAll(v1, 0o755); err != nil {
		t.Fatal(err)
	}
	// The v1 counter is right there, and holds the node's number.
	if err := os.WriteFile(filepath.Join(v1, "memory.max_usage_in_bytes"),
		[]byte("64424509440\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	f := collectMemoryFigures(root, procFile(t, "4:memory:/kubepods/pod-abc/container-def\n"))

	if f.cgroupPeak != 0 {
		t.Errorf("cgroupPeak = %d on cgroup v1; it must be absent, not the node's 60Gi", f.cgroupPeak)
	}
	if f.limitHits != 0 || f.oomKills != 0 {
		t.Errorf("counters = %d/%d on cgroup v1; nothing read them", f.limitHits, f.oomKills)
	}
	if strings.Contains(f.sources, mover.MemorySourceCgroup2) {
		t.Errorf("sources = %q claims a cgroup2 reading that did not happen", f.sources)
	}
	if f.sources != mover.MemorySourceRusage {
		t.Errorf("sources = %q, want %q: the per-process peaks are unaffected by the cgroup version",
			f.sources, mover.MemorySourceRusage)
	}
	if !strings.Contains(f.note, "cgroup v1") {
		t.Errorf("the note does not say why the cgroup peak is missing: %q", f.note)
	}
}

// TestAnUnreadableCgroupIsAbsentAndSaysSo — the file not being there at all (a kernel below 5.19,
// a layout this shim will not guess at). Absent, noted, and never zero.
func TestAnUnreadableCgroupIsAbsentAndSaysSo(t *testing.T) {
	f := collectMemoryFigures(t.TempDir(), procFile(t, "0::/\n"))

	if f.cgroupPeak != 0 || strings.Contains(f.sources, mover.MemorySourceCgroup2) {
		t.Errorf("figures = %+v; nothing was readable", f)
	}
	if f.note == "" {
		t.Error("no note explains why the cgroup peak is missing")
	}
	if !strings.Contains(describeMemory(f), f.note) {
		t.Errorf("the note never reaches the pod log:\n%s", describeMemory(f))
	}
	// A result stamped from these figures must carry no cgroup fields at all, so the far end
	// reads "not measured" rather than "measured zero".
	r := withMemoryFigures(mover.MoverResult{OK: true, Operation: "backup"}, f)
	if r.CgroupPeakBytes != 0 || r.MemoryLimitHits != 0 || r.MemoryOOMKills != 0 {
		t.Errorf("result carries cgroup figures nothing measured: %+v", r)
	}
	encoded, err := r.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(encoded, "cgroupPeakBytes") {
		t.Errorf("an unmeasured cgroup peak is on the wire as a number: %s", encoded)
	}
}

// TestACgroupPeakOfZeroIsRefused. A live cgroup's peak is never zero — a process is running in
// it. Zero means the file is not what this code thinks it is, and a zero on the wire reads as
// "this mover used no memory", which is worse than no number at all.
func TestACgroupPeakOfZeroIsRefused(t *testing.T) {
	root := t.TempDir()
	cgroupV2Dir(t, root, "0\n", realEvents)

	f := collectMemoryFigures(root, procFile(t, "0::/\n"))

	if f.cgroupPeak != 0 || strings.Contains(f.sources, mover.MemorySourceCgroup2) {
		t.Errorf("a zero peak was accepted: %+v", f)
	}
	if !strings.Contains(f.note, "no running cgroup does") {
		t.Errorf("the note does not explain the refusal: %q", f.note)
	}
}

// TestAMangledPeakIsRefused — "max" is a real cgroup sentinel in neighbouring files, and mapping
// a sentinel to a byte count is how an unlimited cgroup comes to report eight exabytes.
func TestAMangledPeakIsRefused(t *testing.T) {
	for _, content := range []string{"max\n", "\n", "not a number\n", "12 34\n"} {
		root := t.TempDir()
		cgroupV2Dir(t, root, content, realEvents)
		f := collectMemoryFigures(root, procFile(t, "0::/\n"))
		if f.cgroupPeak != 0 || strings.Contains(f.sources, mover.MemorySourceCgroup2) {
			t.Errorf("memory.peak %q was accepted as %d", content, f.cgroupPeak)
		}
	}
}

// TestEventsThatWillNotParseDoNotRetractThePeak: the peak was read and is real; only the counters
// are missing, and the note says so.
func TestEventsThatWillNotParseDoNotRetractThePeak(t *testing.T) {
	root := t.TempDir()
	cgroupV2Dir(t, root, "2147483648\n", "")

	f := collectMemoryFigures(root, procFile(t, "0::/\n"))

	if f.cgroupPeak != 2<<30 {
		t.Errorf("cgroupPeak = %d, want %d: an unreadable memory.events must not retract a peak "+
			"that WAS read", f.cgroupPeak, int64(2)<<30)
	}
	if !strings.Contains(f.note, "memory events") {
		t.Errorf("the note does not mention the missing counters: %q", f.note)
	}
}

// helperEnv makes the test binary re-enter itself as the CHILD process below.
const helperEnv = "CRYSTAL_MOVER_RSS_HELPER_BYTES"

// TestPeakRSSTracksTheChildNotTheShim is the claim the whole measurement rests on: ru_maxrss for
// RUSAGE_CHILDREN reports the peak of the process the shim EXECS, not of the shim.
//
// It matters because the shim is a few tens of MiB and restic is the one that streams a
// repository index; a reader who was quietly being shown the shim's footprint would size a limit
// an order of magnitude too small and only find out during a restore. So this spawns a child that
// really does touch a quarter of a gigabyte, and asserts the number comes back.
func TestPeakRSSTracksTheChildNotTheShim(t *testing.T) {
	if want := os.Getenv(helperEnv); want != "" {
		// The child. Touch every page — an untouched allocation is not resident and would
		// (correctly) not show up in an RSS peak at all.
		n, err := strconv.Atoi(want)
		if err != nil {
			os.Exit(2)
		}
		buf := make([]byte, n)
		for i := 0; i < len(buf); i += 4096 {
			buf[i] = 1
		}
		runtime.KeepAlive(buf)
		os.Exit(0)
	}

	self, _, err := readMaxRSS()
	if err != nil {
		t.Skipf("no per-process peak on this platform: %v", err)
	}

	const childBytes = 256 << 20
	cmd := exec.Command(os.Args[0], "-test.run=^TestPeakRSSTracksTheChildNotTheShim$")
	cmd.Env = append(os.Environ(), helperEnv+"="+strconv.Itoa(childBytes))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("helper process: %v\n%s", err, out)
	}

	_, children, err := readMaxRSS()
	if err != nil {
		t.Fatalf("readMaxRSS after the child: %v", err)
	}
	// Generous floor: the child touched 256MiB, so anything near it proves the reading is the
	// child's. A shim-only number would be an order of magnitude below this.
	if children < childBytes/2 {
		t.Errorf("children peak RSS = %d bytes after a child that touched %d; RUSAGE_CHILDREN is "+
			"not reporting the exec'd process (shim self = %d)", children, childBytes, self)
	}
	if children <= self {
		t.Errorf("children peak (%d) is not above this process's own (%d); the two are being "+
			"confused", children, self)
	}
	// The other side of the same assertion, and the one that pins the per-platform UNIT: linux
	// reports ru_maxrss in kibibytes and darwin in bytes, so a wrong maxRSSUnit is a silent
	// 1024x. A child that touched 256MiB cannot have peaked at a gigabyte.
	if children > 4*childBytes {
		t.Errorf("children peak RSS = %d bytes after a child that touched %d; maxRSSUnit (%d) is "+
			"wrong for this platform", children, childBytes, maxRSSUnit)
	}
}

// TestFinalizeStampsAndFitsInThatOrder is the shim-side size guard, on the composition report()
// actually performs. Fitting BEFORE the figures are stamped would leave a manifest report sized
// against a result that was about to grow by a couple of hundred bytes — and past the kubelet's
// cap the message is truncated rather than rejected, so a successful restore would reach the
// controller as unparseable JSON and be recorded as a mover that never reported.
func TestFinalizeStampsAndFitsInThatOrder(t *testing.T) {
	figures := memoryFigures{
		resticPeakRSS: 9223372036854775807, shimPeakRSS: 9223372036854775807,
		cgroupPeak: 9223372036854775807, limitHits: 9223372036854775807,
		oomKills: 9223372036854775807, sources: mover.MemorySourceBoth,
	}
	entries := make([]mover.ResourceEntry, 0, 400)
	for i := range 400 {
		entries = append(entries, mover.ResourceEntry{
			Group: "apps", Kind: "Deployment", Name: "workload-number-" + strconv.Itoa(i),
			Outcome: "Configured", Changed: []string{"spec.template.spec.containers"},
		})
	}

	got := finalize(mover.MoverResult{
		OK: true, Operation: string(mover.OpManifestsRestore),
		RestoredResources: 400, ResourceEntries: entries,
	}, figures)

	encoded, err := got.Encode()
	if err != nil {
		t.Fatalf("Encode() = %v", err)
	}
	if len(encoded) > mover.TerminationMessageLimit {
		t.Errorf("the finalised message is %d bytes, over the %d-byte kubelet cap", len(encoded),
			mover.TerminationMessageLimit)
	}
	if _, err := mover.ParseMoverResult(encoded); err != nil {
		t.Errorf("the finalised message does not parse: %v", err)
	}
	if got.PeakRSSBytes == 0 || got.MemorySource == "" {
		t.Errorf("the figures were not stamped: %+v", got)
	}
	if !got.ResourcesTruncated {
		t.Error("the report was not trimmed, so nothing proves the fit ran after the stamp")
	}
}

func TestCgroupV2PathReadsTheUnifiedLine(t *testing.T) {
	for _, tc := range []struct {
		name, content, want string
	}{
		{"unified only", "0::/kubepods/pod/container\n", "/kubepods/pod/container"},
		{"private namespace", "0::/\n", "/"},
		{
			"hybrid: the v2 line among v1 controllers",
			"9:memory:/kubepods/x\n4:cpu:/kubepods/x\n0::/kubepods/x\n", "/kubepods/x",
		},
		{"v1 only, no unified line", "9:memory:/kubepods/x\n4:cpu:/kubepods/x\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := cgroupV2Path(procFile(t, tc.content))
			if err != nil {
				t.Fatalf("cgroupV2Path() = %v", err)
			}
			if got != tc.want {
				t.Errorf("cgroupV2Path() = %q, want %q", got, tc.want)
			}
		})
	}
}
