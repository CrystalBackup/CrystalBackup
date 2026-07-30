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

package metrics

import (
	"context"

	"github.com/prometheus/client_golang/prometheus"
	batchv1 "k8s.io/api/batch/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
	"github.com/CrystalBackup/CrystalBackup/internal/concurrency"
	"github.com/CrystalBackup/CrystalBackup/internal/status"
)

// Concurrency and queueing (spec/05-observability.md §2.7). Platform-scope: the `cluster` label
// and nothing else, because maxConcurrentMovers is a CLUSTER-WIDE cap and attributing its
// pressure to one namespace would misdescribe who is waiting for whom.
//
// These three are meant to be read together — usage against limit — which is why the limit is
// exported at all rather than left in the CR: a dashboard that shows 8 active movers cannot tell
// you whether that is idle or saturated without it.
var (
	moverLabels = []string{clusterLabel}

	moverActiveDesc = prometheus.NewDesc(
		NameMoverActive,
		"Mover Jobs currently occupying a concurrency slot (backup and restore alike).",
		moverLabels, nil)
	moverQueueDepthDesc = prometheus.NewDesc(
		NameMoverQueueDepth,
		"Backup volumes admitted by a controller that have no mover Job running yet — work waiting on the maxConcurrentMovers semaphore.",
		moverLabels, nil)
	moverConcurrencyLimitDesc = prometheus.NewDesc(
		NameMoverConcurrencyLimit,
		"Configured maxConcurrentMovers across the live cluster-plane runs and schedules. 0 means unlimited.",
		moverLabels, nil)
)

// collectMovers emits the mover census. It lists the Jobs in the operator namespace directly —
// the same listing the concurrency gate itself performs, so the gauge and the gate cannot
// disagree about what counts as a mover.
//
// A listing error emits nothing at all rather than a partial census. Half a concurrency picture
// is worse than none: a mover_active that silently drops the Jobs it could not read looks exactly
// like a quiet cluster, which is the state an operator would act on.
func (c *Collector) collectMovers(ctx context.Context, ch chan<- prometheus.Metric,
	backups []cbv1.Backup, clusterSchedules []cbv1.ClusterBackupSchedule, runs []cbv1.ClusterBackup,
	clusterID string,
) {
	var jobs batchv1.JobList
	if err := c.reader.List(ctx, &jobs, client.InNamespace(c.operatorNamespace),
		client.MatchingLabels{apiconst.LabelManagedBy: apiconst.ManagedByValue}); err != nil {
		return
	}

	// A mover is a per-PVC Job; a repository-init or maintenance Job carries managed-by but no PVC
	// label. Restore movers are movers too — they hold a repository lock and a slot exactly as a
	// backup mover does (adr/0015) — so they count towards active.
	var active, backupMovers []batchv1.Job
	for i := range jobs.Items {
		j := jobs.Items[i]
		if j.Labels[apiconst.LabelPVC] == "" {
			continue
		}
		active = append(active, j)
		if j.Labels[apiconst.LabelRestore] == "" && j.Labels[apiconst.LabelClusterRestore] == "" {
			backupMovers = append(backupMovers, j)
		}
	}
	runningAll := concurrency.RunningMoverJobs(active)
	runningBackup := concurrency.RunningMoverJobs(backupMovers)

	ch <- prometheus.MustNewConstMetric(moverActiveDesc, prometheus.GaugeValue, float64(runningAll), clusterID)
	ch <- prometheus.MustNewConstMetric(moverConcurrencyLimitDesc, prometheus.GaugeValue,
		float64(configuredMoverLimit(clusterSchedules, runs)), clusterID)

	// Queue depth is DERIVED, not counted, and the derivation is worth stating because it is an
	// approximation with a known direction of error.
	//
	// A mover held back by the semaphore leaves no object behind: the controller simply declines
	// to create the Job and holds its volume in Snapshotting. There is nothing to count. What
	// there IS, is the gap between the volumes a controller has admitted into the mover phases and
	// the mover Jobs that actually exist — and that gap is exactly the backlog, plus any volume
	// whose CSI snapshot is still binding. So this OVER-reports during a snapshot storm and never
	// under-reports; a zero is trustworthy, and a persistent non-zero next to
	// mover_active == mover_concurrency_limit is the saturation the metric exists to show.
	//
	// Backup-side only, matched against backup-side movers. Restore volumes contend for the same
	// semaphore but expose no per-volume phase to count from, so folding restore Jobs into the
	// subtraction without their pending work would make the depth read low — worse than leaving
	// the restore backlog out of a metric that says "backup volumes" on the tin.
	//
	// The Backups arrive from the caller's single listing rather than being re-listed here: one
	// scrape, one read per kind.
	wanted := 0
	for i := range backups {
		b := &backups[i]
		if b.Annotations[apiconst.AnnotationProjected] == apiconst.AnnotationProjectedValue {
			continue // a discovery projection never runs a mover.
		}
		for _, v := range b.Status.Volumes {
			if v.Phase == status.VolumePhaseSnapshotting || v.Phase == status.VolumePhaseUploading {
				wanted++
			}
		}
	}
	depth := max(wanted-runningBackup, 0)
	ch <- prometheus.MustNewConstMetric(moverQueueDepthDesc, prometheus.GaugeValue, float64(depth), clusterID)
}

// configuredMoverLimit reports the cap in force, as the LARGEST maxConcurrentMovers any live
// cluster-plane run or schedule declares.
//
// The cap is enforced cluster-wide but declared per run (ClusterBackupRunSpec), which is a shape
// the metric's single `cluster` label cannot represent faithfully when two schedules disagree.
// The maximum is the honest summary of the two readings an operator cares about: it is the number
// of concurrent movers the platform may actually reach, and it is what a "usage vs limit" panel
// must not under-state. A zero anywhere means unlimited and is reported as 0, matching
// concurrency.CanStartMover's own reading of the field.
func configuredMoverLimit(clusterSchedules []cbv1.ClusterBackupSchedule, runs []cbv1.ClusterBackup) int32 {
	var limit int32
	for i := range clusterSchedules {
		if v := clusterSchedules[i].Spec.Template.Spec.MaxConcurrentMovers; v > limit {
			limit = v
		}
	}
	for i := range runs {
		if isTerminalRunPhase(runs[i].Status.Phase) {
			continue
		}
		if v := runs[i].Spec.MaxConcurrentMovers; v > limit {
			limit = v
		}
	}
	return limit
}

// isTerminalRunPhase reports whether a ClusterBackup run has finished, so a long-retained history
// of old runs cannot keep advertising a limit nothing is enforcing any more.
func isTerminalRunPhase(phase string) bool {
	switch status.ClusterBackupPhase(phase) {
	case status.ClusterBackupPhaseCompleted, status.ClusterBackupPhasePartiallyFailed, status.ClusterBackupPhaseFailed:
		return true
	default:
		return false
	}
}
