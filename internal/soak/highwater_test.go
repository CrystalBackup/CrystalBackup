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
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/CrystalBackup/CrystalBackup/internal/mover"
)

// fakeLister drives the sampler with no cluster. Everything about the high-water stream that can
// be wrong — attribution, absence, sample counting — is decided in this package, and the crucible
// is far too coarse an instrument to catch a peak filed under the wrong class.
type fakeLister struct {
	pods    []corev1.Pod
	jobs    []batchv1.Job
	metrics []podMetric
	// metricsErr is what the metrics.k8s.io read returns. NotFound is how the absence of
	// metrics-server actually presents.
	metricsErr error
	nodeStats  map[string][]podVolumeStats
	nodeErr    error
}

func (f *fakeLister) ListPods(context.Context, string, string) (*corev1.PodList, error) {
	return &corev1.PodList{Items: f.pods}, nil
}

func (f *fakeLister) ListJobs(context.Context, string, string) (*batchv1.JobList, error) {
	return &batchv1.JobList{Items: f.jobs}, nil
}

func (f *fakeLister) PodMetrics(context.Context, string) ([]podMetric, error) {
	return f.metrics, f.metricsErr
}

func (f *fakeLister) NodeStats(_ context.Context, node string) ([]podVolumeStats, error) {
	if f.nodeErr != nil {
		return nil, f.nodeErr
	}
	return f.nodeStats[node], nil
}

const testNS = "crystal-backup-system"

func moverJob(name string, op mover.Operation, start, end time.Time) batchv1.Job {
	j := batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: testNS,
			Labels: map[string]string{moverAppLabel: moverAppValue},
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name: "mover",
					Args: []string{"--operation", string(op), "--", "backup", "/data"},
				}}},
			},
		},
	}
	if !start.IsZero() {
		t := metav1.NewTime(start)
		j.Status.StartTime = &t
	}
	if !end.IsZero() {
		t := metav1.NewTime(end)
		j.Status.CompletionTime = &t
		j.Status.Succeeded = 1
	}
	return j
}

func moverPod(name, job, node string, started time.Time) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: testNS,
			Labels: map[string]string{moverAppLabel: moverAppValue, "job-name": job},
		},
		Spec:   corev1.PodSpec{NodeName: node},
		Status: corev1.PodStatus{StartTime: ptrTime(started)},
	}
}

func ptrTime(t time.Time) *metav1.Time {
	m := metav1.NewTime(t)
	return &m
}

// TestHighWaterAttributesByOperationNeverByGuess is §5's hardest rule.
//
// A `data` peak filed under `repo-heavy` would be read as evidence that prune's 8Gi limit is
// right when it is evidence about backup's 4Gi — and the reader would have no way of telling.
// The operation is recovered from the owning Job's argv (`--operation <op>`, which
// internal/mover.BuildJob writes), because the labels the controllers stamp do NOT carry it:
// `app.kubernetes.io/component: maintenance` covers prune, check, forget and unlock, which
// straddle two sizing classes.
func TestHighWaterAttributesByOperationNeverByGuess(t *testing.T) {
	for _, tc := range []struct {
		name      string
		job       batchv1.Job
		wantOp    string
		wantClass string
	}{
		{"backup is data", moverJob("j1", mover.OpBackup, time.Time{}, time.Time{}), "backup", "data"},
		{"prune is repo-heavy", moverJob("j2", mover.OpPrune, time.Time{}, time.Time{}), "prune", "repo-heavy"},
		{"check is repo-heavy, not the same as prune", moverJob("j3", mover.OpCheck, time.Time{}, time.Time{}), "check", "repo-heavy"},
		{"forget is repo-light", moverJob("j4", mover.OpForget, time.Time{}, time.Time{}), "forget", "repo-light"},
		{"manifests backup is manifests", moverJob("j5", mover.OpManifestsBackup, time.Time{}, time.Time{}), "manifests-backup", "manifests"},
		{
			"an operation this build does not know is NOT classed",
			moverJob("j6", mover.Operation("teleport"), time.Time{}, time.Time{}),
			"teleport", classUnknown,
		},
		{
			"a Job with no --operation is unknown, not the first class in the map",
			batchv1.Job{Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Args: []string{"--", "snapshots"}}}},
			}}},
			classUnknown, classUnknown,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			op, class := operationOfJob(&tc.job)
			if op != tc.wantOp || class != tc.wantClass {
				t.Errorf("operationOfJob = (%q, %q), want (%q, %q)", op, class, tc.wantOp, tc.wantClass)
			}
		})
	}
	// And the operation names in the table above must be the real ones, or this test is asserting
	// against fiction.
	if _, ok := mover.ClassOf(mover.OpManifestsBackup); !ok {
		t.Fatal("mover.OpManifestsBackup is not in the sizing table")
	}
}

// TestHighWaterKeepsThePeakAndTheSampleCount is the number the maintainer cannot obtain any other
// way.
//
// The sample count travels with the peak because a pod that lives eleven minutes is sampled ~44
// times at 15s and the TRUE peak is above the highest sample. A peak from three samples is a
// different claim from a peak from four hundred, and a reader who cannot see which one they have
// is being invited to size a limit on noise.
func TestHighWaterKeepsThePeakAndTheSampleCount(t *testing.T) {
	store := newTestStore(t, 1<<20)
	start := day0.Add(time.Hour)
	f := &fakeLister{
		jobs: []batchv1.Job{moverJob("job-backup", mover.OpBackup, start, time.Time{})},
		pods: []corev1.Pod{moverPod("job-backup-xyz", "job-backup", "worker-1", start)},
	}
	h := NewHighWater(store, f, testNS, 15*time.Second, false)

	usages := []int64{100 << 20, 900 << 20, 400 << 20}
	for i, u := range usages {
		f.metrics = []podMetric{{Name: "job-backup-xyz", MemoryUsed: u}}
		h.Sample(t.Context(), start.Add(time.Duration(i+1)*15*time.Second))
	}

	marks := h.Marks(start.Add(time.Minute))
	data := marks.Classes["data"]
	if data.Memory.Status != statusOK {
		t.Fatalf("memory status = %q (%s), want OK", data.Memory.Status, data.Memory.Reason)
	}
	if data.PeakMemoryBytes != 900<<20 {
		t.Errorf("peak = %d, want %d (the max across samples, not the last one)",
			data.PeakMemoryBytes, 900<<20)
	}
	if data.PeakSamples != len(usages) {
		t.Errorf("peakSamples = %d, want %d: without it the reader cannot tell a measured peak "+
			"from a lucky one", data.PeakSamples, len(usages))
	}
	if data.Pods != 1 {
		t.Errorf("pods = %d, want 1", data.Pods)
	}
	if len(marks.Pods) != 1 || marks.Pods[0].LifetimeSeconds <= 0 {
		t.Errorf("the pod's lifetime was not recorded: %+v", marks.Pods)
	}
	// Every other class must say NOT MEASURED rather than reporting a peak of zero.
	for _, class := range mover.Classes() {
		if class == "data" {
			continue
		}
		cm := marks.Classes[class]
		if cm.Memory.Status != statusNotMeasured {
			t.Errorf("class %q with no movers reports %q; it must be NOT_MEASURED, because a peak "+
				"of zero reads as \"movers use no memory\"", class, cm.Memory.Status)
		}
		if cm.PeakMemoryBytes != 0 {
			t.Errorf("class %q reports a peak of %d with no pods", class, cm.PeakMemoryBytes)
		}
	}
}

// TestPodsRanButWereNeverSampledSaysSo is the sentence a four-hour soak on a real crucible
// cluster actually printed, beside `"pods": 57`:
//
//	"no repo-light mover pod ran while the collector was up"
//
// The status was right and the reason was false — and self-contradicting, since the same object
// carried the count of the pods it said had not run. A reader diagnosing that soak would conclude
// their workload was idle when what failed was the instrument.
//
// The two cases are distinguished here from the one fact that separates them: whether any pod of
// that class was ever SEEN. Nothing about the sampling changes; only what the report claims.
func TestPodsRanButWereNeverSampledSaysSo(t *testing.T) {
	store := newTestStore(t, 1<<20)
	start := day0.Add(time.Hour)
	// metrics-server is healthy — it answers, with an empty list, exactly as it does for a pod
	// that lived less than one of its scrape intervals. This is the 63-mover case: no error to
	// record, no NotFound, simply nothing about any mover ever.
	f := &fakeLister{
		jobs: []batchv1.Job{moverJob("job-forget", mover.OpForget, start, time.Time{})},
		pods: []corev1.Pod{
			moverPod("job-forget-a", "job-forget", "worker-1", start),
			moverPod("job-forget-b", "job-forget", "worker-2", start),
		},
		metrics: []podMetric{},
	}
	h := NewHighWater(store, f, testNS, 15*time.Second, false)
	h.Sample(t.Context(), start.Add(15*time.Second))

	marks := h.Marks(start.Add(time.Minute))
	light := marks.Classes["repo-light"]
	if light.Pods != 2 {
		t.Fatalf("pods = %d, want 2 (the pods were listed; only the sampling failed)", light.Pods)
	}
	if light.Memory.Status != statusNotMeasured {
		t.Fatalf("memory status = %q, want NOT_MEASURED", light.Memory.Status)
	}
	if strings.Contains(light.Memory.Reason, "no repo-light mover pod ran") {
		t.Errorf("the reason contradicts the object it lives in: pods=%d yet the reason says none "+
			"ran.\nreason: %q", light.Pods, light.Memory.Reason)
	}
	// It must say the pods ran, that none was sampled, WHY (metrics-server's cadence), that the
	// obvious fix is not one — and it must stay honest that this is not a measurement of zero.
	for _, want := range []string{
		"2 repo-light mover pod(s) ran",
		"was ever sampled",
		"metrics-server",
		"Sampling faster does not help",
		"not a measurement of zero",
	} {
		if !strings.Contains(light.Memory.Reason, want) {
			t.Errorf("the reason is missing %q:\n%q", want, light.Memory.Reason)
		}
	}

	// The other side of the same coin: a class that genuinely saw nothing still says so.
	heavy := marks.Classes["repo-heavy"]
	if heavy.Pods != 0 {
		t.Fatalf("repo-heavy pods = %d, want 0", heavy.Pods)
	}
	if !strings.Contains(heavy.Memory.Reason, "no repo-heavy mover pod ran") {
		t.Errorf("a class with no pods at all must say so plainly: %q", heavy.Memory.Reason)
	}
}

// TestNoMetricsServerIsNotMeasuredNotZero. This project has been bitten five times by an absence
// reading as health.
func TestNoMetricsServerIsNotMeasuredNotZero(t *testing.T) {
	store := newTestStore(t, 1<<20)
	start := day0.Add(time.Hour)
	f := &fakeLister{
		jobs: []batchv1.Job{moverJob("job-prune", mover.OpPrune, start, time.Time{})},
		pods: []corev1.Pod{moverPod("job-prune-abc", "job-prune", "worker-2", start)},
		metricsErr: apierrors.NewNotFound(
			schema.GroupResource{Group: "metrics.k8s.io", Resource: "pods"}, "pods"),
	}
	h := NewHighWater(store, f, testNS, 15*time.Second, false)
	h.Sample(t.Context(), start.Add(15*time.Second))

	marks := h.Marks(start.Add(time.Minute))
	if marks.MetricsServer.Status != statusNotMeasured {
		t.Fatalf("metricsServer.status = %q, want %q", marks.MetricsServer.Status, statusNotMeasured)
	}
	if !strings.Contains(marks.MetricsServer.Reason, "not") {
		t.Errorf("the reason does not say what was missing: %q", marks.MetricsServer.Reason)
	}
	heavy := marks.Classes["repo-heavy"]
	if heavy.Memory.Status != statusNotMeasured {
		t.Errorf("class memory status = %q, want NOT_MEASURED", heavy.Memory.Status)
	}
	// The half of the stream that needs no metrics-server still works: the pod was seen, and its
	// class was attributed.
	if heavy.Pods != 1 {
		t.Errorf("pods = %d, want 1: OOM and eviction counting needs nothing but the pod list",
			heavy.Pods)
	}
}

// TestKubeletStatsDisabledIsDisabledNotZero: without --kubelet-stats the restic cache high-water
// is not obtainable at all, and the manifest must say DISABLED rather than reporting a cache of
// nothing against a 20Gi ceiling.
func TestKubeletStatsDisabledIsDisabledNotZero(t *testing.T) {
	store := newTestStore(t, 1<<20)
	h := NewHighWater(store, &fakeLister{}, testNS, 15*time.Second, false)
	marks := h.Marks(day0)
	if marks.KubeletStats.Status != statusDisabled {
		t.Errorf("kubeletStats.status = %q, want %q", marks.KubeletStats.Status, statusDisabled)
	}
	if !strings.Contains(marks.KubeletStats.Reason, "nodes/proxy") {
		t.Errorf("the reason does not name the grant that would enable it: %q", marks.KubeletStats.Reason)
	}
}

// TestCacheHighWaterFromKubeletStats is the 20Gi ceiling internal/mover/profiles.go calls "a
// CEILING AGAINST A RUNAWAY, not a sizing estimate". This is the only in-cluster source for the
// real number.
func TestCacheHighWaterFromKubeletStats(t *testing.T) {
	store := newTestStore(t, 1<<20)
	start := day0.Add(time.Hour)
	f := &fakeLister{
		jobs:    []batchv1.Job{moverJob("job-sync", mover.OpSync, start, time.Time{})},
		pods:    []corev1.Pod{moverPod("job-sync-1", "job-sync", "worker-9", start)},
		metrics: []podMetric{{Name: "job-sync-1", MemoryUsed: 1 << 30}},
		nodeStats: map[string][]podVolumeStats{
			"worker-9": {{
				Namespace: testNS, Pod: "job-sync-1",
				Volumes: map[string]int64{moverCacheVolume: 3 << 30, "tmp": 1 << 20},
			}},
		},
	}
	h := NewHighWater(store, f, testNS, 15*time.Second, true)
	h.Sample(t.Context(), start.Add(15*time.Second))
	f.nodeStats["worker-9"][0].Volumes[moverCacheVolume] = 2 << 30 // it went down; the peak stands
	h.Sample(t.Context(), start.Add(30*time.Second))

	marks := h.Marks(start.Add(time.Minute))
	if marks.KubeletStats.Status != statusOK {
		t.Fatalf("kubeletStats.status = %q (%s)", marks.KubeletStats.Status, marks.KubeletStats.Reason)
	}
	if got := marks.Classes["repo-heavy"].CacheHighWaterBytes; got != 3<<30 {
		t.Errorf("cache high-water = %d, want %d (the peak, not the last reading)", got, 3<<30)
	}
}

// TestOOMKillsAndEvictionsAreCountedDefinitively. Both are read off the pod and need no
// metrics-server; the eviction is where a cache-sizeLimit breach lands, because exceeding an
// emptyDir sizeLimit does not return ENOSPC to restic — the kubelet kills the pod out of band.
func TestOOMKillsAndEvictionsAreCounted(t *testing.T) {
	store := newTestStore(t, 1<<20)
	start := day0.Add(time.Hour)

	oomed := moverPod("job-backup-oom", "job-backup", "worker-1", start)
	oomed.Status.ContainerStatuses = []corev1.ContainerStatus{{
		LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			Reason: "OOMKilled",
		}},
	}}
	evicted := moverPod("job-backup-evict", "job-backup", "worker-2", start)
	evicted.Status.Phase = corev1.PodFailed
	evicted.Status.Reason = "Evicted"
	evicted.Status.Message = `Usage of EmptyDir volume "restic-cache" exceeds the limit "20Gi"`

	f := &fakeLister{
		jobs: []batchv1.Job{moverJob("job-backup", mover.OpBackup, start, time.Time{})},
		pods: []corev1.Pod{oomed, evicted},
	}
	f.metricsErr = apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, "pods")
	h := NewHighWater(store, f, testNS, 15*time.Second, false)
	h.Sample(t.Context(), start.Add(15*time.Second))

	data := h.Marks(start.Add(time.Minute)).Classes["data"]
	if data.OOMKills != 1 {
		t.Errorf("oomKills = %d, want 1: it is the one number that says a limit is too low", data.OOMKills)
	}
	if data.Evictions != 1 {
		t.Errorf("evictions = %d, want 1", data.Evictions)
	}
	var msg string
	for _, p := range h.Marks(start.Add(time.Minute)).Pods {
		if p.Evicted {
			msg = p.EvictionMessage
		}
	}
	if !strings.Contains(msg, "restic-cache") {
		t.Errorf("the kubelet's message was not kept: %q. It names the volume and the number, which "+
			"is the whole difference between MoverEvicted and a mysterious pod disappearance", msg)
	}
}

// TestLongestJobIsWallClock: the duration histogram measures the OPERATOR's view of the same
// thing, and both belong in the archive. What blocks a backup window is wall clock.
func TestLongestJobIsWallClock(t *testing.T) {
	store := newTestStore(t, 1<<20)
	start := day0.Add(time.Hour)
	f := &fakeLister{jobs: []batchv1.Job{
		moverJob("prune-1", mover.OpPrune, start, start.Add(40*time.Minute)),
		moverJob("prune-2", mover.OpPrune, start, start.Add(9*time.Minute)),
		moverJob("check-1", mover.OpCheck, start, start.Add(20*time.Minute)),
	}}
	h := NewHighWater(store, f, testNS, 15*time.Second, false)
	h.Sample(t.Context(), start.Add(time.Hour))

	marks := h.Marks(start.Add(time.Hour))
	if got := marks.LongestByOperation["prune"]; got != (40 * time.Minute).Seconds() {
		t.Errorf("longest prune = %.0fs, want %.0fs", got, (40 * time.Minute).Seconds())
	}
	if got := marks.LongestByOperation["check"]; got != (20 * time.Minute).Seconds() {
		t.Errorf("longest check = %.0fs, want %.0fs", got, (20 * time.Minute).Seconds())
	}
	// Per class as well, because "longest repo-heavy" is what sizes a maintenance window.
	if marks.Classes["repo-heavy"].LongestOperation != "prune" {
		t.Errorf("longest repo-heavy operation = %q, want prune",
			marks.Classes["repo-heavy"].LongestOperation)
	}
}

// TestMarksSurviveARestart: a collector OOM-killed on day three must not report the peak of day
// four onwards as the peak of the soak.
func TestMarksSurviveARestart(t *testing.T) {
	store := newTestStore(t, 1<<20)
	start := day0.Add(time.Hour)
	f := &fakeLister{
		jobs:    []batchv1.Job{moverJob("job-backup", mover.OpBackup, start, time.Time{})},
		pods:    []corev1.Pod{moverPod("job-backup-1", "job-backup", "worker-1", start)},
		metrics: []podMetric{{Name: "job-backup-1", MemoryUsed: 3 << 30}},
	}
	h := NewHighWater(store, f, testNS, 15*time.Second, false)
	h.Sample(t.Context(), start.Add(15*time.Second))

	reborn := NewHighWater(store, &fakeLister{}, testNS, 15*time.Second, false)
	marks := reborn.Marks(start.Add(time.Hour))
	if got := marks.Classes["data"].PeakMemoryBytes; got != 3<<30 {
		t.Errorf("after a restart the data peak is %d, want %d", got, 3<<30)
	}
	// But the VERDICT is not restored as OK: whether metrics-server answers is a fact about the
	// cluster right now, not about the previous process's luck.
	if marks.MetricsServer.Status == statusOK {
		t.Error("a restarted collector claims metrics-server is OK without having asked it")
	}
}

func TestPercentileIsNearestRank(t *testing.T) {
	for _, tc := range []struct {
		name   string
		values []int64
		want   int64
	}{
		{"empty", nil, 0},
		{"one", []int64{7}, 7},
		{"two", []int64{1, 9}, 9},
		{"ten, p95 is the largest", []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, 10},
		{"twenty, p95 is the nineteenth", []int64{
			1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20}, 19},
		{"unsorted input", []int64{9, 1, 5}, 9},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := percentile(tc.values, 0.95); got != tc.want {
				t.Errorf("percentile = %d, want %d", got, tc.want)
			}
		})
	}
	// Nearest-rank, so the answer is always a value some pod actually reached. Interpolating
	// would produce a number no mover ever used, which is precisely what a sizing decision must
	// not be based on.
	got := percentile([]int64{100, 200}, 0.95)
	if got != 100 && got != 200 {
		t.Errorf("percentile returned %d, which is neither observation", got)
	}
}

func TestJobNameOfKnowsBothLabelSpellings(t *testing.T) {
	for _, tc := range []struct {
		name   string
		pod    corev1.Pod
		want   string
		reason string
	}{
		{
			"the modern label", corev1.Pod{ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{"batch.kubernetes.io/job-name": "j"}}}, "j",
			"Kubernetes 1.27+ stamps the prefixed spelling",
		},
		{
			"the legacy label", corev1.Pod{ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{"job-name": "j"}}}, "j",
			"older clusters stamp only the bare one",
		},
		{
			"the ownerReference when neither label is there", corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{OwnerReferences: []metav1.OwnerReference{
					{Kind: "Job", Name: "j"}}}}, "j",
			"a pod with no job-name label is still owned by its Job",
		},
		{"nothing at all", corev1.Pod{}, "", "and then the class must be unknown, not guessed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := jobNameOf(&tc.pod); got != tc.want {
				t.Errorf("jobNameOf = %q, want %q (%s)", got, tc.want, tc.reason)
			}
		})
	}
}
