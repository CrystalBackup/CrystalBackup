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

package concurrency

import (
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
)

func job(conds ...batchv1.JobCondition) batchv1.Job {
	return batchv1.Job{Status: batchv1.JobStatus{Conditions: conds}}
}

func cond(t batchv1.JobConditionType, s corev1.ConditionStatus) batchv1.JobCondition {
	return batchv1.JobCondition{Type: t, Status: s}
}

func TestRunningMoverJobs(t *testing.T) {
	jobs := []batchv1.Job{
		job(), // freshly created, no status → running
		job(cond(batchv1.JobComplete, corev1.ConditionTrue)),      // done → free
		job(cond(batchv1.JobFailed, corev1.ConditionTrue)),        // failed → free
		job(cond(batchv1.JobComplete, corev1.ConditionFalse)),     // condition present but not True → running
		job(cond(batchv1.JobSuspended, corev1.ConditionTrue)),     // suspended is not terminal → running
		job(cond(batchv1.JobFailureTarget, corev1.ConditionTrue)), // not a terminal condition → running
	}
	if got := RunningMoverJobs(jobs); got != 4 {
		t.Fatalf("RunningMoverJobs = %d, want 4 (2 of 6 terminal)", got)
	}
	if got := RunningMoverJobs(nil); got != 0 {
		t.Fatalf("RunningMoverJobs(nil) = %d, want 0", got)
	}
}

func TestCanStartMover(t *testing.T) {
	for _, tc := range []struct {
		name    string
		running int
		limit   int32
		want    bool
	}{
		{"unlimited when limit is zero", 100, 0, true},
		{"unlimited when limit is negative", 100, -1, true},
		{"below the limit", 2, 3, true},
		{"at the limit", 3, 3, false},
		{"above the limit", 4, 3, false},
		{"limit one, none running", 0, 1, true},
		{"limit one, one running", 1, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := CanStartMover(tc.running, tc.limit); got != tc.want {
				t.Fatalf("CanStartMover(%d, %d) = %v, want %v", tc.running, tc.limit, got, tc.want)
			}
		})
	}
}
