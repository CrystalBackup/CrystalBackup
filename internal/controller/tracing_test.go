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

package controller

import (
	"context"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
	"github.com/CrystalBackup/CrystalBackup/internal/tracing"
)

// TestTracedOmitsTheTraceIDKeyWhenTracingIsOff covers spec/05-observability.md §4's exact wording:
// `traceID` is "present when tracing is active".
//
// Absent, not empty, and the difference is not cosmetic. A `traceID: ""` on every line of an
// untraced install is a dead column in every log index that ships these lines, and a Loki query
// for the lines belonging to some trace would match every line belonging to none. The controllers
// get this for free because traced() returns the context untouched when the anchor is inert —
// this test pins that it stays that way.
func TestTracedOmitsTheTraceIDKeyWhenTracingIsOff(t *testing.T) {
	if tracing.Active() {
		t.Skip("tracing is active in this process; this test covers the unconfigured default")
	}
	base := context.Background()
	ctx, anchor := traced(base, "6f1c2b8e-0d3a-4a11-9f2e-7c5d4b3a2190")

	if anchor.Valid() {
		t.Error("traced returned a valid anchor with tracing inactive")
	}
	if ctx != base {
		t.Error("traced replaced the context with tracing inactive; the logger must be left alone " +
			"so no traceID key is added")
	}
}

// TestBackupSpanAttrsUseTheSpecsNamesAndOmitEmpties pins the attribute contract of §5: the
// crystalbackup.* keys, spelled as the spec spells them, with absent values genuinely absent.
func TestBackupSpanAttrsUseTheSpecsNamesAndOmitEmpties(t *testing.T) {
	backup := &cbv1.Backup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "dr-daily-20260712-020000",
			Namespace: "c-team-x",
			Labels: map[string]string{
				apiconst.LabelTenant: "team-x",
				apiconst.LabelOrigin: apiconst.OriginCluster,
				// No schedule label, and no cluster-backup label: both must be omitted, not blank.
			},
		},
		Spec: cbv1.BackupSpec{LocationRef: cbv1.LocationReference{Name: "dr"}},
	}
	got := map[string]string{}
	for _, kv := range backupSpanAttrs(backup, &backupRunContext{clusterID: "prod-eu-1"}) {
		got[string(kv.Key)] = kv.Value.AsString()
	}

	for key, want := range map[string]string{
		tracing.AttrNamespace: "c-team-x",
		tracing.AttrTenant:    "team-x",
		tracing.AttrBackup:    "dr-daily-20260712-020000",
		tracing.AttrOrigin:    apiconst.OriginCluster,
		tracing.AttrLocation:  "dr",
		tracing.AttrCluster:   "prod-eu-1",
	} {
		if got[key] != want {
			t.Errorf("%s = %q, want %q", key, got[key], want)
		}
	}
	for _, key := range []string{tracing.AttrSchedule, tracing.AttrClusterBackup} {
		if _, present := got[key]; present {
			t.Errorf("%s is present with no value to report; it must be omitted", key)
		}
	}
}

// TestBackupSpanAttrsFallBackToTheNamespaceForTenant mirrors backupMetricSeries, so a span and the
// metric series describing the same backup never disagree about whose data it is.
func TestBackupSpanAttrsFallBackToTheNamespaceForTenant(t *testing.T) {
	backup := &cbv1.Backup{ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: "c-team-y"}}
	for _, kv := range backupSpanAttrs(backup, nil) {
		if string(kv.Key) == tracing.AttrTenant && kv.Value.AsString() != "c-team-y" {
			t.Errorf("tenant = %q, want the namespace as fallback", kv.Value.AsString())
		}
	}
}

// TestJobWindowFallsBackWhenAJobFailed. completionTime is set only on SUCCESS, so a mover that
// exhausted its backoffLimit has none — and a `mover` span that inherited a zero end time would be
// clamped to zero length, hiding exactly the run whose duration is worth knowing.
func TestJobWindowFallsBackWhenAJobFailed(t *testing.T) {
	started := metav1.NewTime(time.Now().Add(-time.Hour))
	failed := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.NewTime(started.Add(-time.Minute))},
		Status:     batchv1.JobStatus{StartTime: &started}, // no CompletionTime: it failed
	}
	start, end := jobWindow(failed)
	if !start.Equal(started.Time) {
		t.Errorf("start = %s, want the Job's startTime %s", start, started.Time)
	}
	if end.Before(start) || time.Since(end) > time.Minute {
		t.Errorf("end = %s; a failed Job's span must end at roughly now, not at the zero time", end)
	}

	// A Job that never scheduled a pod has no startTime either: its creation is the honest start.
	created := metav1.NewTime(time.Now().Add(-2 * time.Hour))
	unscheduled := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{CreationTimestamp: created}}
	start, _ = jobWindow(unscheduled)
	if !start.Equal(created.Time) {
		t.Errorf("start = %s, want the Job's creationTimestamp %s", start, created.Time)
	}
}
