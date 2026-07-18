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

// Package concurrency is the cluster-wide admission control for data-mover Jobs:
// a best-effort weighted semaphore that caps how many movers run at once across
// the whole cascade (maxConcurrentMovers). It is deliberately a COUNT of the live
// mover Jobs the API already tracks — not a reserved in-memory counter — so it is
// correct across an operator restart (the live Jobs are the ground truth) and
// needs no leader-held state. The trade-off is that it is advisory: two Backups
// reconciling in the same instant can both observe a free slot and momentarily
// overshoot the cap by a little; the cap is a resource-pacing guardrail, not a
// safety invariant, so a transient overshoot that drains as movers finish is
// acceptable. Every mover is weight one in M1 (the "weighted" hook is where a
// future size- or cost-aware weight would multiply in).
package concurrency

import (
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
)

// RunningMoverJobs counts the mover Jobs currently occupying a slot: every Job in
// the set that has been created but is not yet terminal. A finished Job (a
// Complete or Failed condition) has released its slot even if it lingers briefly
// before its TTL or the controller's eager teardown removes it, so it is not
// counted. Callers pass the Jobs already selected by the mover managed-by label,
// so this never has to re-filter what is or is not a mover.
func RunningMoverJobs(jobs []batchv1.Job) int {
	running := 0
	for i := range jobs {
		if !jobTerminal(&jobs[i]) {
			running++
		}
	}
	return running
}

// jobTerminal reports whether a Job has reached a terminal condition (Complete or
// Failed). A Job with no such condition — active, or freshly created with empty
// status — is still occupying its slot.
func jobTerminal(j *batchv1.Job) bool {
	for _, c := range j.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		if c.Type == batchv1.JobComplete || c.Type == batchv1.JobFailed {
			return true
		}
	}
	return false
}

// CanStartMover reports whether one more mover may start given the count of
// movers already running and the configured limit. A non-positive limit means
// "unlimited" (the maxConcurrentMovers field is optional; unset ⇒ no cap), so the
// gate is skipped entirely — the common single-tenant case pays nothing.
func CanStartMover(running int, limit int32) bool {
	if limit <= 0 {
		return true
	}
	return int32(running) < limit
}
