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
	"math"
	"slices"
	"strings"
	"sync"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/CrystalBackup/CrystalBackup/internal/mover"
)

// ---------------------------------------------------------------------------------------------
// §5 — the part 0.6.1 could not prove.
//
// internal/mover/profiles.go says it in its own words: the shipped limits are "deliberately
// generous", their job is "to keep ONE runaway mover from taking a node down, not to right-size
// restic", and the 20Gi cache ceiling is "a CEILING AGAINST A RUNAWAY, not a sizing estimate"
// because "the load test that would give us the real curve has not been run". This stream is that
// curve, measured on real data instead of paid infrastructure.
//
// TWO SOURCES, AND THE SECOND ONE IS THE ONE THAT WORKS.
//
// This stream was built to poll metrics.k8s.io every --mover-sample-interval, on the assumption
// that a mover pod lives for minutes. It does not. Measured over four hours on a real crucible
// cluster: 63 mover pods, lifetimes from 0.9s to 17s, and ZERO samples on every single one, with
// metrics-server installed, healthy and answering for long-lived pods throughout. metrics-server
// exposes a pod only after its own ~15s scrape has covered it, so a container that lives one
// second never exists for it at all — and sampling faster cannot fix that, because the data is
// not there to be sampled.
//
// So the mover measures itself. The shim already writes a structured mover.MoverResult to its
// termination message; it now carries its own peak RSS (exact, from the kernel's per-process
// accounting), its cgroup peak, and the two memory.events counters. The collector reads them off
// the terminated pod — an object it was listing anyway, which survives the pod for the Job's
// ttlSecondsAfterFinished (600s to 3600s), two to three orders of magnitude more forgiving than
// one scrape interval.
//
// The sampled path stays, for pods long-lived enough to be caught and as a cross-check, but the
// mover-reported figure is PREFERRED and every figure names its source. The two are not the same
// quantity: one is an exact process peak, the other is the highest sample anyone happened to
// catch.
// ---------------------------------------------------------------------------------------------

// classUnknown is where a mover whose operation could not be recovered is filed. NEVER a guess:
// §5 is explicit, and the reason is that a `data` peak recorded under `repo-heavy` would be read
// as evidence that prune's 8Gi limit is right when it is evidence about backup's 4Gi.
const classUnknown = "unknown"

// moverAppLabel selects mover pods and their Jobs. The controllers stamp it on both the Job and
// its pod template (internal/controller's moverAppName), so one label selector finds the whole
// population without the collector having to know which controller created what.
const (
	moverAppLabel = "app.kubernetes.io/name"
	moverAppValue = "crystal-mover"
	labelJobName  = "job-name"
)

// The two container-termination verdicts this stream reads off a pod. Spelled once: a misspelling
// here does not fail, it silently reports zero OOM kills on a cluster that had them, which is
// exactly the absence-reads-as-health shape §5 warns about.
const (
	reasonOOMKilled = "OOMKilled"
	reasonEvicted   = "Evicted"
)

// jobNameLabels are the two spellings of "which Job owns this pod". Kubernetes added the
// batch.kubernetes.io/ prefix in 1.27 and kept the bare one; a collector that knew only the new
// spelling would attribute nothing on an older cluster, and a collector that knew only the old
// one would break on a cluster where it has finally been dropped.
var jobNameLabels = []string{"batch.kubernetes.io/" + labelJobName, labelJobName}

// PodMark is everything measured about ONE mover pod.
//
// Samples and Lifetime are stored beside the peak because a peak from three samples is a
// different claim from a peak from four hundred. A pod that lives eleven minutes is sampled ~44
// times at 15s and the TRUE peak is above the highest sample; a reader who cannot see the sample
// count has no way to know how much confidence the number carries.
type PodMark struct {
	Pod       string `json:"pod"`
	Namespace string `json:"namespace"`
	Node      string `json:"node,omitempty"`
	Job       string `json:"job,omitempty"`
	Operation string `json:"operation"`
	Class     string `json:"class"`

	PeakMemoryBytes int64     `json:"peakMemoryBytes"`
	Samples         int       `json:"samples"`
	FirstSeen       time.Time `json:"firstSeen"`
	LastSeen        time.Time `json:"lastSeen"`
	LifetimeSeconds float64   `json:"lifetimeSeconds"`

	// OOMKilled is definitive, always available and the one number that says a limit is too low.
	// Unlike the memory peak it needs no metrics-server and cannot be missed by sampling: the
	// kubelet writes it into the container status and it stays there.
	OOMKilled bool `json:"oomKilled,omitempty"`
	// Evicted covers the emptyDir-over-limit kill too. Exceeding a cache sizeLimit does not
	// return ENOSPC to restic — the kubelet's eviction manager notices out of band and kills the
	// pod — so this is where a 20Gi cache breach lands, with the kubelet's own message naming the
	// volume and the number.
	Evicted         bool   `json:"evicted,omitempty"`
	EvictionMessage string `json:"evictionMessage,omitempty"`

	CachePeakBytes int64 `json:"cachePeakBytes,omitempty"`
	CacheSamples   int   `json:"cacheSamples,omitempty"`

	// ------------------------------------------------------------------------------------
	// What the MOVER said about itself, read off its termination message
	// (mover.MoverResult). Everything above this line is sampled from outside; everything
	// below it was measured by the process that ran, and the two must never be confused,
	// which is why they are separate fields with separate names rather than one number whose
	// provenance a reader has to guess.
	//
	// This is the half that works. A mover lives seconds and metrics-server exposes a pod only
	// after its own ~15s scrape has covered it, so the sampled fields above are empty for most
	// real movers — 63 of 63 on a four-hour crucible run. The termination message survives the
	// pod for as long as the Job's ttlSecondsAfterFinished (600s to 3600s across the
	// controllers), which is two to three orders of magnitude more forgiving than one scrape
	// interval.
	// ------------------------------------------------------------------------------------

	// ReportedPeakRSSBytes is the restic process's peak resident set size, exact. This is the
	// figure a memory limit has to cover.
	ReportedPeakRSSBytes int64 `json:"reportedPeakRSSBytes,omitempty"`
	// ReportedShimPeakRSSBytes is the crystal-mover shim's own peak, resident AT THE SAME TIME
	// (the shim waits while restic runs), so a limit has to cover the sum of the two.
	ReportedShimPeakRSSBytes int64 `json:"reportedShimPeakRSSBytes,omitempty"`
	// ReportedCgroupPeakBytes is the peak of the container cgroup's memory.current: anonymous
	// memory PLUS reclaimable page cache. An upper bound, NOT a sizing target — a backup
	// streams a volume through the page cache and every one of those pages is charged here.
	ReportedCgroupPeakBytes int64 `json:"reportedCgroupPeakBytes,omitempty"`
	// ReportedLimitHits is memory.events `max`: times the cgroup reached its limit and the
	// kernel reclaimed. Zero means the limit was never pressed, so a large cgroup peak is cache
	// the container was merely allowed to keep.
	ReportedLimitHits int64 `json:"reportedLimitHits,omitempty"`
	// ReportedOOMKills is memory.events `oom_kill`. Not redundant with OOMKilled above: when
	// restic is killed and the shim survives to report, the container exits 1 and Kubernetes
	// records no OOM anywhere.
	ReportedOOMKills int64 `json:"reportedOOMKills,omitempty"`
	// ReportedSource is the mover's own MemorySource, naming which of its readers succeeded.
	// Empty means this pod reported no memory figures at all.
	ReportedSource string `json:"reportedSource,omitempty"`
}

// JobMark is one mover Job's wall clock, which is what actually blocks a backup window.
//
// The duration histogram measures the OPERATOR's view of the same thing and both belong in the
// archive: they disagree exactly when the interesting part is queueing, scheduling or image pull,
// and the gap between them is itself a finding.
type JobMark struct {
	Job         string     `json:"job"`
	Operation   string     `json:"operation"`
	Class       string     `json:"class"`
	StartedAt   *time.Time `json:"startedAt,omitempty"`
	CompletedAt *time.Time `json:"completedAt,omitempty"`
	Seconds     float64    `json:"seconds,omitempty"`
	Succeeded   bool       `json:"succeeded"`
}

// MeasuredValue is the type every number in this file that could be absent uses.
//
// NOT MEASURED is a first-class state and not a zero. This project has been bitten five times by
// an absence reading as health, and a peak memory of 0 for the `data` class would read as "movers
// use no memory" rather than "metrics-server is not installed" — which is the difference between
// shipping a smaller limit and shipping a broken one.
type MeasuredValue struct {
	Status string `json:"status"` // OK | NOT_MEASURED | DISABLED
	Reason string `json:"reason,omitempty"`
	// Source names WHERE an OK figure came from, because two sources with different meanings
	// now feed the same field and a reader must never have to wonder which one they are
	// looking at: sourceMover is the mover's own exact measurement of the process that ran,
	// sourceSampled is metrics.k8s.io polled from outside. Empty when nothing was measured.
	Source string `json:"source,omitempty"`
}

// The provenances a MeasuredValue can carry.
const (
	// sourceMover — the mover measured itself and reported through its termination message.
	// Exact, and the only source that works for a pod that lives seconds.
	sourceMover = "mover-reported"
	// sourceSampled — polled from metrics.k8s.io at --mover-sample-interval. Structurally
	// blind to short-lived pods; a peak from it is the highest SAMPLE, not the peak.
	sourceSampled = "sampled"
)

// ClassMarks is the answer, per operation class.
type ClassMarks struct {
	Class string `json:"class"`
	Pods  int    `json:"pods"`

	Memory          MeasuredValue `json:"memory"`
	PeakMemoryBytes int64         `json:"peakMemoryBytes,omitempty"`
	PeakMemoryPod   string        `json:"peakMemoryPod,omitempty"`
	PeakSamples     int           `json:"peakSamples,omitempty"`
	P95MemoryBytes  int64         `json:"p95MemoryBytes,omitempty"`
	MemorySamples   int           `json:"memorySamples,omitempty"`
	MemoryPods      int           `json:"memoryPods,omitempty"`

	// The mover's own figures, aggregated. PREFERRED over the sampled ones above: they are
	// exact, they exist for pods no sampler could ever catch, and Memory.Source says which of
	// the two the class's verdict rests on. Peaks are the MAX across the class's pods; the two
	// counters are SUMS, because "how many times did any mover of this shape hit its limit" is
	// the question they answer.
	ReportedPods             int    `json:"reportedPods,omitempty"`
	ReportedPeakRSSBytes     int64  `json:"reportedPeakRSSBytes,omitempty"`
	ReportedPeakPod          string `json:"reportedPeakPod,omitempty"`
	ReportedP95RSSBytes      int64  `json:"reportedP95RSSBytes,omitempty"`
	ReportedShimPeakRSSBytes int64  `json:"reportedShimPeakRSSBytes,omitempty"`
	ReportedCgroupPeakBytes  int64  `json:"reportedCgroupPeakBytes,omitempty"`
	ReportedLimitHits        int64  `json:"reportedLimitHits,omitempty"`
	ReportedOOMKills         int64  `json:"reportedOOMKills,omitempty"`

	OOMKills  int `json:"oomKills"`
	Evictions int `json:"evictions"`

	Cache               MeasuredValue `json:"cache"`
	CacheHighWaterBytes int64         `json:"cacheHighWaterBytes,omitempty"`
	CacheSamples        int           `json:"cacheSamples,omitempty"`

	LongestSeconds   float64 `json:"longestSeconds,omitempty"`
	LongestOperation string  `json:"longestOperation,omitempty"`
	LongestJob       string  `json:"longestJob,omitempty"`
	JobsObserved     int     `json:"jobsObserved"`
}

// Marks is highwater/marks.json.
type Marks struct {
	UpdatedAt      time.Time             `json:"updatedAt"`
	SampleInterval string                `json:"sampleInterval"`
	MetricsServer  MeasuredValue         `json:"metricsServer"`
	KubeletStats   MeasuredValue         `json:"kubeletStats"`
	Classes        map[string]ClassMarks `json:"classes"`
	// LongestByOperation is §5's last row, kept per operation as well as per class because
	// "longest prune" and "longest check" are different questions even though both are
	// repo-heavy, and the answer to the first one is what decides whether a maintenance window is
	// big enough.
	LongestByOperation map[string]float64 `json:"longestByOperation"`
	Pods               []PodMark          `json:"pods"`
	Jobs               []JobMark          `json:"jobs"`
}

// podLister is the sliver of client-go the sampler needs. Narrow so the whole thing is testable
// without a cluster — which matters here more than anywhere else in this package, because this
// is the stream whose correctness nobody can check after the fact.
type podLister interface {
	ListPods(ctx context.Context, namespace, selector string) (*corev1.PodList, error)
	ListJobs(ctx context.Context, namespace, selector string) (*batchv1.JobList, error)
	// PodMetrics fetches metrics.k8s.io/v1beta1 for a namespace. An absent metrics-server must
	// come back as a NotFound rather than as an empty list, so the caller can tell "no
	// metrics-server" from "no mover pods running".
	PodMetrics(ctx context.Context, namespace string) ([]podMetric, error)
	// NodeStats fetches the kubelet's stats/summary through nodes/proxy.
	NodeStats(ctx context.Context, node string) ([]podVolumeStats, error)
}

type podMetric struct {
	Name       string
	MemoryUsed int64
}

type podVolumeStats struct {
	Namespace string
	Pod       string
	Volumes   map[string]int64
}

// HighWater is the resident sampler.
type HighWater struct {
	Namespace    string
	Interval     time.Duration
	KubeletStats bool

	lister podLister
	store  *Store

	mu   sync.Mutex
	pods map[string]*PodMark
	jobs map[string]*JobMark
	// jobClass caches the class attribution per Job name, so a mover pod whose Job has already
	// been garbage-collected by ttlSecondsAfterFinished still gets attributed rather than sliding
	// to unknown at the end of its own life.
	jobClass map[string]jobIdentity

	metricsServer MeasuredValue
	kubelet       MeasuredValue
}

type jobIdentity struct {
	Operation string
	Class     string
}

// NewHighWater builds the sampler with its two NOT MEASURED states already correct: nothing has
// been measured yet, and that is what it says until something is.
func NewHighWater(store *Store, lister podLister, namespace string, interval time.Duration, kubeletStats bool) *HighWater {
	h := &HighWater{
		Namespace: namespace, Interval: interval, KubeletStats: kubeletStats,
		lister: lister, store: store,
		pods: map[string]*PodMark{}, jobs: map[string]*JobMark{}, jobClass: map[string]jobIdentity{},
		metricsServer: MeasuredValue{Status: statusNotMeasured, Reason: "no sample taken yet"},
	}
	if kubeletStats {
		h.kubelet = MeasuredValue{Status: statusNotMeasured, Reason: "no sample taken yet"}
	} else {
		h.kubelet = MeasuredValue{Status: statusDisabled, Reason: kubeletDisabledReason}
	}
	h.load()
	return h
}

const kubeletDisabledReason = "--kubelet-stats was not set, so the restic cache high-water was " +
	"never read. It cannot be obtained from the API: the cache is an emptyDir inside a mover pod " +
	"and the only in-cluster source for its size is the kubelet's own stats/summary endpoint, " +
	"which needs nodes/proxy — a grant the chart renders but deliberately leaves unbound until " +
	"soak.kubeletStats is set"

// Sample takes one round: every mover pod, its class, its memory, its kills and — when enabled —
// its cache.
//
// Errors are RECORDED and the round continues. A metrics-server that is down for an hour must
// not stop the OOM-kill and eviction counting, which needs nothing but the pod list and is the
// half of this stream that is always available.
func (h *HighWater) Sample(ctx context.Context, now time.Time) {
	selector := moverAppLabel + "=" + moverAppValue

	jobs, err := h.lister.ListJobs(ctx, h.Namespace, selector)
	if err != nil {
		h.store.RecordError(StreamHighwater, "list mover Jobs: "+err.Error(), now)
	} else {
		h.observeJobs(jobs)
	}

	pods, err := h.lister.ListPods(ctx, h.Namespace, selector)
	if err != nil {
		h.store.RecordError(StreamHighwater, "list mover pods: "+err.Error(), now)
		return
	}
	h.observePods(pods, now)

	usage, err := h.lister.PodMetrics(ctx, h.Namespace)
	switch {
	case err != nil && apierrors.IsNotFound(err):
		h.setMetricsServer(MeasuredValue{Status: statusNotMeasured, Reason: "metrics.k8s.io is not " +
			"served by this cluster (no metrics-server): peak mover memory was never sampled. It is " +
			"NOT zero — nothing looked"})
	case err != nil:
		h.setMetricsServer(MeasuredValue{Status: statusNotMeasured,
			Reason: "metrics.k8s.io could not be read: " + err.Error()})
		h.store.RecordError(StreamHighwater, "read metrics.k8s.io: "+err.Error(), now)
	default:
		h.setMetricsServer(MeasuredValue{Status: statusOK})
		h.observeMemory(usage, now)
	}

	if h.KubeletStats {
		h.sampleCache(ctx, pods, now)
	}
	h.persist(now)
}

func (h *HighWater) sampleCache(ctx context.Context, pods *corev1.PodList, now time.Time) {
	nodes := map[string]bool{}
	for i := range pods.Items {
		if n := pods.Items[i].Spec.NodeName; n != "" {
			nodes[n] = true
		}
	}
	if len(nodes) == 0 {
		return
	}
	names := make([]string, 0, len(nodes))
	for n := range nodes {
		names = append(names, n)
	}
	slices.Sort(names)

	var lastErr error
	measured := false
	for _, node := range names {
		stats, err := h.lister.NodeStats(ctx, node)
		if err != nil {
			lastErr = err
			continue
		}
		measured = true
		h.observeCache(stats)
	}
	switch {
	case measured:
		h.setKubelet(MeasuredValue{Status: statusOK})
	case lastErr != nil:
		h.setKubelet(MeasuredValue{Status: statusNotMeasured,
			Reason: "the kubelet stats endpoint could not be read (nodes/proxy is the grant it " +
				"needs): " + lastErr.Error()})
		h.store.RecordError(StreamHighwater, "read kubelet stats/summary: "+lastErr.Error(), now)
	}
}

func (h *HighWater) observeJobs(jobs *batchv1.JobList) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i := range jobs.Items {
		job := &jobs.Items[i]
		op, class := operationOfJob(job)
		h.jobClass[job.Name] = jobIdentity{Operation: op, Class: class}
		mark, ok := h.jobs[job.Name]
		if !ok {
			mark = &JobMark{Job: job.Name, Operation: op, Class: class}
			h.jobs[job.Name] = mark
		}
		mark.Operation, mark.Class = op, class
		if job.Status.StartTime != nil {
			t := job.Status.StartTime.UTC()
			mark.StartedAt = &t
		}
		if job.Status.CompletionTime != nil {
			t := job.Status.CompletionTime.UTC()
			mark.CompletedAt = &t
			mark.Succeeded = job.Status.Succeeded > 0
		}
		if mark.StartedAt != nil && mark.CompletedAt != nil {
			mark.Seconds = mark.CompletedAt.Sub(*mark.StartedAt).Seconds()
		}
	}
}

func (h *HighWater) observePods(pods *corev1.PodList, now time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i := range pods.Items {
		pod := &pods.Items[i]
		mark, ok := h.pods[pod.Name]
		if !ok {
			job := jobNameOf(pod)
			ident := h.jobClass[job]
			if ident.Class == "" {
				ident = jobIdentity{Operation: classUnknown, Class: classUnknown}
			}
			mark = &PodMark{
				Pod: pod.Name, Namespace: pod.Namespace, Job: job,
				Operation: ident.Operation, Class: ident.Class,
				FirstSeen: now.UTC(),
			}
			h.pods[pod.Name] = mark
		}
		// A pod first seen before its Job was listed starts as unknown; the moment the Job shows
		// up it is corrected. Correcting is safe (it can only move OFF unknown) and it is what
		// keeps the first mover of the soak from being permanently unattributed.
		if mark.Class == classUnknown {
			if ident, ok := h.jobClass[mark.Job]; ok && ident.Class != "" {
				mark.Operation, mark.Class = ident.Operation, ident.Class
			}
		}
		mark.Node = pod.Spec.NodeName
		mark.LastSeen = now.UTC()
		if pod.Status.StartTime != nil {
			mark.LifetimeSeconds = now.UTC().Sub(pod.Status.StartTime.Time).Seconds()
		} else {
			mark.LifetimeSeconds = mark.LastSeen.Sub(mark.FirstSeen).Seconds()
		}
		markTerminations(mark, pod)
		markReportedMemory(mark, pod)
	}
}

// markReportedMemory reads what the mover said about its own memory off the pod's termination
// message.
//
// This is the stream's answer to the thing sampling structurally cannot do. The shim writes a
// mover.MoverResult to /dev/termination-log on the way out and the kubelet hangs those bytes on
// the terminated container status, where they stay for as long as the pod object does — the
// mover Jobs' ttlSecondsAfterFinished runs from 600s to 3600s, against a 15s sample interval, so
// a pod that lived 0.9s is still readable minutes later. Nothing here perturbs the mover: the
// figures are already in an object the collector was listing anyway.
//
// A message that does not parse is IGNORED, never counted as zero. That is the normal shape of a
// hard kill (the kubelet leaves the message empty when the container never got to write) and of
// a mover image older than these fields; both mean "not measured", and both must stay
// distinguishable from a mover that measured itself and found a small number.
func markReportedMemory(mark *PodMark, pod *corev1.Pod) {
	statuses := make([]corev1.ContainerStatus, 0,
		len(pod.Status.ContainerStatuses)+len(pod.Status.InitContainerStatuses))
	statuses = append(statuses, pod.Status.ContainerStatuses...)
	statuses = append(statuses, pod.Status.InitContainerStatuses...)
	for _, cs := range statuses {
		for _, state := range []*corev1.ContainerStateTerminated{
			cs.State.Terminated, cs.LastTerminationState.Terminated,
		} {
			if state == nil {
				continue
			}
			result, err := mover.ParseMoverResult(state.Message)
			if err != nil || result.MemorySource == "" {
				continue
			}
			// Sticky maxima. A container that terminated twice (a retry in place) reports twice,
			// and the interesting attempt is the expensive one.
			mark.ReportedPeakRSSBytes = max(mark.ReportedPeakRSSBytes, result.PeakRSSBytes)
			mark.ReportedShimPeakRSSBytes = max(mark.ReportedShimPeakRSSBytes, result.ShimPeakRSSBytes)
			mark.ReportedCgroupPeakBytes = max(mark.ReportedCgroupPeakBytes, result.CgroupPeakBytes)
			mark.ReportedLimitHits = max(mark.ReportedLimitHits, result.MemoryLimitHits)
			mark.ReportedOOMKills = max(mark.ReportedOOMKills, result.MemoryOOMKills)
			mark.ReportedSource = result.MemorySource
		}
	}
}

// markTerminations reads the two definitive verdicts off the pod. Both are sticky: once true
// they stay true, because a pod that OOMed on its first attempt and succeeded on its retry is a
// pod whose limit was too low.
func markTerminations(mark *PodMark, pod *corev1.Pod) {
	statuses := make([]corev1.ContainerStatus, 0,
		len(pod.Status.ContainerStatuses)+len(pod.Status.InitContainerStatuses))
	statuses = append(statuses, pod.Status.ContainerStatuses...)
	statuses = append(statuses, pod.Status.InitContainerStatuses...)
	for _, cs := range statuses {
		if cs.State.Terminated != nil && cs.State.Terminated.Reason == reasonOOMKilled {
			mark.OOMKilled = true
		}
		if cs.LastTerminationState.Terminated != nil &&
			cs.LastTerminationState.Terminated.Reason == reasonOOMKilled {
			mark.OOMKilled = true
		}
	}
	if pod.Status.Phase == corev1.PodFailed && pod.Status.Reason == reasonEvicted {
		mark.Evicted = true
		if mark.EvictionMessage == "" {
			mark.EvictionMessage = pod.Status.Message
		}
	}
}

func (h *HighWater) observeMemory(usage []podMetric, now time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, u := range usage {
		mark, ok := h.pods[u.Name]
		if !ok {
			// PodMetrics for something that is not a mover pod, or a mover pod that has not been
			// listed yet. Not attributed: §5 forbids guessing a class, and a peak with no class
			// is worse than no peak.
			continue
		}
		mark.Samples++
		mark.LastSeen = now.UTC()
		if u.MemoryUsed > mark.PeakMemoryBytes {
			mark.PeakMemoryBytes = u.MemoryUsed
		}
	}
}

func (h *HighWater) observeCache(stats []podVolumeStats) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, s := range stats {
		mark, ok := h.pods[s.Pod]
		if !ok || s.Namespace != h.Namespace {
			continue
		}
		used, ok := s.Volumes[moverCacheVolume]
		if !ok {
			continue
		}
		mark.CacheSamples++
		if used > mark.CachePeakBytes {
			mark.CachePeakBytes = used
		}
	}
}

// moverCacheVolume is internal/mover's restic cache emptyDir. Named here as a literal because it
// is unexported there and exporting a volume name for one reader would make it look like a
// contract it is not; if it ever changes, this stream goes NOT MEASURED with an empty volume map
// rather than silently reporting zero.
const moverCacheVolume = "restic-cache"

func jobNameOf(pod *corev1.Pod) string {
	for _, key := range jobNameLabels {
		if v := pod.Labels[key]; v != "" {
			return v
		}
	}
	for _, ref := range pod.OwnerReferences {
		if ref.Kind == "Job" {
			return ref.Name
		}
	}
	return ""
}

// operationOfJob recovers what a mover was DOING.
//
// §5 says the operation comes from the owning Job, and the labels the controllers stamp
// (app.kubernetes.io/component: maintenance | repo-init | discovery | sync) do NOT carry it —
// "maintenance" covers prune, check, forget and unlock, which straddle two sizing classes. What
// does carry it, unambiguously, is the argv internal/mover.BuildJob writes: `--operation <op>`
// as the first two arguments, before restic's "--" separator. That is read from the Job spec,
// which is a fact about the object and not an inference from it.
//
// Anything that does not parse becomes unknown. Never a guess.
func operationOfJob(job *batchv1.Job) (string, string) {
	for _, c := range job.Spec.Template.Spec.Containers {
		for i, arg := range c.Args {
			if arg != "--operation" || i+1 >= len(c.Args) {
				continue
			}
			op := c.Args[i+1]
			if class, ok := mover.ClassOf(mover.Operation(op)); ok {
				return op, class
			}
			// A known flag with an operation this build's table does not name: report the
			// operation (it is real) and refuse to class it.
			return op, classUnknown
		}
	}
	return classUnknown, classUnknown
}

func (h *HighWater) setMetricsServer(v MeasuredValue) {
	h.mu.Lock()
	defer h.mu.Unlock()
	// Once something HAS been measured, a later outage does not erase it. The marks would
	// otherwise be reported NOT MEASURED because metrics-server happened to be restarting at
	// export time, which is the absence-reads-as-health failure inverted.
	if h.metricsServer.Status == statusOK && v.Status != statusOK {
		return
	}
	h.metricsServer = v
}

func (h *HighWater) setKubelet(v MeasuredValue) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.kubelet.Status == statusOK && v.Status != statusOK {
		return
	}
	h.kubelet = v
}

// Marks derives the per-class answer from the per-pod table.
func (h *HighWater) Marks(now time.Time) Marks {
	h.mu.Lock()
	defer h.mu.Unlock()

	m := Marks{
		UpdatedAt:          now.UTC(),
		SampleInterval:     h.Interval.String(),
		MetricsServer:      h.metricsServer,
		KubeletStats:       h.kubelet,
		Classes:            map[string]ClassMarks{},
		LongestByOperation: map[string]float64{},
	}
	for _, class := range append(mover.Classes(), classUnknown) {
		m.Classes[class] = ClassMarks{
			Class:  class,
			Memory: h.metricsServer,
			Cache:  h.kubelet,
		}
	}

	peaks := map[string][]int64{}
	reported := map[string][]int64{}
	for _, p := range h.pods {
		cm := m.Classes[p.Class]
		if cm.Class == "" {
			cm = ClassMarks{Class: p.Class, Memory: h.metricsServer, Cache: h.kubelet}
		}
		cm.Pods++
		if p.OOMKilled {
			cm.OOMKills++
		}
		if p.Evicted {
			cm.Evictions++
		}
		if p.Samples > 0 {
			cm.MemoryPods++
			cm.MemorySamples += p.Samples
			peaks[p.Class] = append(peaks[p.Class], p.PeakMemoryBytes)
			if p.PeakMemoryBytes > cm.PeakMemoryBytes {
				cm.PeakMemoryBytes = p.PeakMemoryBytes
				cm.PeakMemoryPod = p.Pod
				cm.PeakSamples = p.Samples
			}
		}
		if p.ReportedSource != "" {
			cm.ReportedPods++
			reported[p.Class] = append(reported[p.Class], p.ReportedPeakRSSBytes)
			if p.ReportedPeakRSSBytes > cm.ReportedPeakRSSBytes {
				cm.ReportedPeakRSSBytes = p.ReportedPeakRSSBytes
				cm.ReportedPeakPod = p.Pod
			}
			cm.ReportedShimPeakRSSBytes = max(cm.ReportedShimPeakRSSBytes, p.ReportedShimPeakRSSBytes)
			cm.ReportedCgroupPeakBytes = max(cm.ReportedCgroupPeakBytes, p.ReportedCgroupPeakBytes)
			cm.ReportedLimitHits += p.ReportedLimitHits
			cm.ReportedOOMKills += p.ReportedOOMKills
		}
		if p.CacheSamples > 0 {
			cm.CacheSamples += p.CacheSamples
			if p.CachePeakBytes > cm.CacheHighWaterBytes {
				cm.CacheHighWaterBytes = p.CachePeakBytes
			}
		}
		m.Classes[p.Class] = cm
		m.Pods = append(m.Pods, *p)
	}

	for _, j := range h.jobs {
		cm := m.Classes[j.Class]
		if cm.Class == "" {
			cm = ClassMarks{Class: j.Class, Memory: h.metricsServer, Cache: h.kubelet}
		}
		cm.JobsObserved++
		if j.Seconds > cm.LongestSeconds {
			cm.LongestSeconds = j.Seconds
			cm.LongestOperation = j.Operation
			cm.LongestJob = j.Job
		}
		m.Classes[j.Class] = cm
		if j.Seconds > m.LongestByOperation[j.Operation] {
			m.LongestByOperation[j.Operation] = j.Seconds
		}
		m.Jobs = append(m.Jobs, *j)
	}

	for class, values := range peaks {
		cm := m.Classes[class]
		cm.P95MemoryBytes = percentile(values)
		m.Classes[class] = cm
	}
	for class, values := range reported {
		cm := m.Classes[class]
		cm.ReportedP95RSSBytes = percentile(values)
		m.Classes[class] = cm
	}
	// The verdict, in preference order, and it is stated POSITIVELY in every branch — a reader
	// must never have to infer which of two sources with different meanings they are looking at.
	for class, cm := range m.Classes {
		switch {
		case cm.ReportedPods > 0:
			// The mover measured itself. This wins over the sampled figure even when
			// metrics-server answered, because the two are not the same quantity and only one of
			// them is exact.
			cm.Memory = MeasuredValue{
				Status: statusOK, Source: sourceMover,
				Reason: moverReportedReason(cm),
			}
		case cm.MemoryPods > 0:
			cm.Memory = MeasuredValue{
				Status: statusOK, Source: sourceSampled,
				Reason: "polled from metrics.k8s.io every " + h.Interval.String() + ". This is the " +
					"highest SAMPLE, not the peak: the true peak sits above it by however much the " +
					"pod grew between two scrapes. No mover of this class reported a peak of its own",
			}
		case cm.Memory.Status == statusOK:
			// metrics-server answered and nothing of this class ever landed in it.
			cm.Memory = MeasuredValue{Status: statusNotMeasured, Reason: noPeakReason(class, cm.Pods)}
		case cm.Pods > 0:
			// metrics-server was unavailable AND no mover reported. Both halves, so the reader
			// knows the fallback was tried rather than absent.
			cm.Memory = MeasuredValue{Status: statusNotMeasured, Reason: cm.Memory.Reason +
				fmt.Sprintf(". Nor did any of the %d %s mover pod(s) report a peak of its own: a "+
					"mover image older than the reporting itself, or one killed before it could "+
					"write its result", cm.Pods, class)}
		}
		m.Classes[class] = cm
	}

	slices.SortFunc(m.Pods, func(a, b PodMark) int { return a.FirstSeen.Compare(b.FirstSeen) })
	slices.SortFunc(m.Jobs, func(a, b JobMark) int { return strings.Compare(a.Job, b.Job) })
	return m
}

// moverReportedReason states what the mover-reported figures MEAN, beside the figures, because a
// number whose meaning lives in another document is a number that will be read wrong.
//
// The one sentence that has to survive being skimmed: the cgroup peak is not a sizing target. It
// counts reclaimable page cache, a backup streams a volume through the page cache, and the kernel
// reclaims that cache before it OOM-kills anything — so raising a memory limit to cover it raises
// a ceiling that was never the constraint. The RSS pair is what a limit has to cover, and the two
// counters say whether the limit was ever pressed at all.
func moverReportedReason(cm ClassMarks) string {
	b := fmt.Sprintf("measured by the %d mover pod(s) themselves and read back off their "+
		"termination messages — exact, not sampled, and available for pods far too short-lived for "+
		"metrics.k8s.io to have ever seen. reportedPeakRSSBytes is restic's peak RESIDENT SET "+
		"(anonymous + mapped, no page cache): this is what a memory limit has to cover, together "+
		"with reportedShimPeakRSSBytes, which is resident at the same time",
		cm.ReportedPods)
	if cm.ReportedCgroupPeakBytes > 0 {
		b += ". reportedCgroupPeakBytes is the cgroup's peak memory.current and INCLUDES " +
			"reclaimable page cache, so for a streaming backup it can sit an order of magnitude " +
			"above the RSS peak: it is an upper bound, NOT a sizing target"
		if cm.ReportedLimitHits == 0 {
			b += ". reportedLimitHits is 0, so the limit was never reached: whatever the cgroup " +
				"peak is, it is memory the container was merely ALLOWED to keep"
		} else {
			b += fmt.Sprintf(". reportedLimitHits is %d, so the limit WAS reached and the kernel "+
				"reclaimed to stay under it — this class is running at its ceiling", cm.ReportedLimitHits)
		}
	}
	if cm.ReportedOOMKills > 0 {
		b += fmt.Sprintf(". reportedOOMKills is %d: the cgroup OOM killer killed a process, which "+
			"the kubelet does NOT record when the shim survives to report", cm.ReportedOOMKills)
	}
	return b
}

// noPeakReason spells the TWO ways a class ends up with no peak, because they are two different
// findings and only one of them is about the workload.
//
// The first is an idle class: nothing of that shape ran. The second is a MEASUREMENT failure: pods
// ran and not one of them was ever sampled. Reporting the first sentence over the second is a false
// statement in the very object that carries the pod count — a soak reporting `"pods": 57` beside
// "no repo-light mover pod ran" invites the reader to conclude their workload was idle when what
// actually happened is that the instrument missed every one of them.
//
// The second case names its cause, which is structural rather than a bug in the sampling loop:
// metrics-server exposes a pod only after its own scrape has covered it, on its own cadence, so a
// container that lives a couple of seconds never exists for metrics.k8s.io at all. Measured on a
// real cluster: 63 mover pods, lifetimes 0.9s to 17s, metrics-server healthy throughout and
// answering for long-lived pods, ZERO samples on every single mover. A shorter
// --mover-sample-interval cannot fix it — the data is not there to be sampled — which is why the
// mover reports its own peak (mover.MoverResult) and why this stream prefers that number.
func noPeakReason(class string, pods int) string {
	if pods == 0 {
		return fmt.Sprintf("no %s mover pod ran while the collector was up, so there is no peak to "+
			"report. This is not a measurement of zero", class)
	}
	return fmt.Sprintf("%d %s mover pod(s) ran and NOT ONE of them was ever sampled: metrics-server "+
		"only exposes a pod after it has scraped it, on its own cadence (~15s), so a mover that "+
		"lives seconds never exists for metrics.k8s.io. Sampling faster does not help — the data is "+
		"not there to be sampled. This is not a measurement of zero, and it is not a measurement of "+
		"an idle class either", pods, class)
}

// p95 is the only percentile this stream reports, named rather than passed: every call site wants
// the same one, and a parameter would invite a second answer to a question that has one.
const p95 = 0.95

// percentile is the nearest-rank p95 over the per-pod peaks. Nearest-rank rather than an
// interpolating estimator because these are a handful of real observations, not a distribution:
// interpolating between two measured pods would produce a number no pod ever reached, which is
// precisely what a sizing decision must not be based on.
func percentile(values []int64) int64 {
	if len(values) == 0 {
		return 0
	}
	sorted := make([]int64, len(values))
	copy(sorted, values)
	slices.Sort(sorted)
	rank := min(max(int(math.Ceil(p95*float64(len(sorted)))), 1), len(sorted))
	return sorted[rank-1]
}

// persist writes the marks, atomically, on every round. They are never dropped by the cap and
// they are the smallest thing on the volume — which is the whole reason §4 sacrifices days of
// raw metrics for them.
func (h *HighWater) persist(now time.Time) {
	body, err := json.MarshalIndent(h.Marks(now), "", "  ")
	if err != nil {
		h.store.RecordError(StreamHighwater, "encode the high-water marks: "+err.Error(), now)
		return
	}
	if err := h.store.WriteFileAtomic(fileMarks, body); err != nil {
		h.store.RecordError(StreamHighwater, "write the high-water marks: "+err.Error(), now)
	}
}

// load restores the table after a restart. Without it a collector that was OOM-killed on day
// three would report the peak of day four onwards as the peak of the soak.
func (h *HighWater) load() {
	raw, err := readFile(h.store.Dir(), fileMarks)
	if err != nil {
		return
	}
	var m Marks
	if err := json.Unmarshal(raw, &m); err != nil {
		return
	}
	for i := range m.Pods {
		p := m.Pods[i]
		h.pods[p.Pod] = &p
	}
	for i := range m.Jobs {
		j := m.Jobs[i]
		h.jobs[j.Job] = &j
		h.jobClass[j.Job] = jobIdentity{Operation: j.Operation, Class: j.Class}
	}
	// The two MeasuredValues are NOT restored as OK from disk. Whether metrics-server answers is
	// a fact about the cluster right now, and a previous process's success is not evidence for
	// this one — but a previous process's SAMPLES are still real, which is why the pod table is
	// restored and the verdict is not.
	if len(m.Pods) > 0 {
		h.metricsServer = MeasuredValue{Status: statusNotMeasured,
			Reason: "no sample taken since this collector restarted; earlier samples are kept"}
	}
}

// parseQuantity converts a metrics.k8s.io memory string ("123456Ki") to bytes.
func parseQuantity(s string) (int64, bool) {
	q, err := resource.ParseQuantity(strings.TrimSpace(s))
	if err != nil {
		return 0, false
	}
	v, ok := q.AsInt64()
	return v, ok
}

// listOptionsFor is the selector every list in this file uses.
func listOptionsFor(selector string) metav1.ListOptions {
	return metav1.ListOptions{LabelSelector: selector}
}
