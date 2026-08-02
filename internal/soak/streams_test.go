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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TestErrorLineSelectsWhatCollectShSelects. The resident collector and the fallback CronJob path
// must pick the SAME lines out of the operator's log, or an archive from one is not comparable
// with an archive from the other — and the fallback is what an admin gets on a build without
// these subcommands.
func TestErrorLineSelectsWhatCollectShSelects(t *testing.T) {
	for _, tc := range []struct {
		name string
		line string
		want bool
	}{
		{"zap JSON error", `{"level":"error","msg":"backup failed"}`, true},
		{"zap JSON dpanic", `{"level":"dpanic","msg":"x"}`, true},
		{"zap JSON fatal", `{"level":"fatal","msg":"x"}`, true},
		{"zap console ERROR", "2026-06-01T00:00:00Z\tERROR\tsetup\tboom", true},
		{"zap console FATAL", "2026-06-01T00:00:00Z FATAL setup boom", true},
		{"info is not an error", `{"level":"info","msg":"Starting manager"}`, false},
		{"debug is not an error", `{"level":"debug","msg":"x"}`, false},
		// The one that matters for volume: a fortnight of INFO lines from a busy operator would
		// bury the errors and eat the cap.
		{"the word error in a message is not a level", `{"level":"info","msg":"no error occurred"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := errorLine.MatchString(tc.line); got != tc.want {
				t.Errorf("errorLine.MatchString(%q) = %v, want %v", tc.line, got, tc.want)
			}
		})
	}
}

// TestFlattenEventFallsBackThroughTheTimestamps. An Event has three places a time can live and
// which one is populated depends on whether the emitter used the old core/v1 recorder or the new
// events/v1 one. A collector that read only LastTimestamp would stamp half the stream with the
// zero time, and a zero timestamp in the archive reads as 0001-01-01 rather than as "unknown".
func TestFlattenEventFallsBackThroughTheTimestamps(t *testing.T) {
	at := day0.Add(3 * time.Hour)
	for _, tc := range []struct {
		name  string
		event corev1.Event
	}{
		{"lastTimestamp", corev1.Event{LastTimestamp: metav1.NewTime(at)}},
		{"eventTime", corev1.Event{EventTime: metav1.NewMicroTime(at)}},
		{"creationTimestamp", corev1.Event{
			ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.NewTime(at)}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := flattenEvent(&tc.event)
			if !got.At.Equal(at.UTC()) {
				t.Errorf("at = %s, want %s", got.At, at.UTC())
			}
		})
	}

	full := corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{UID: "abc"},
		InvolvedObject: corev1.ObjectReference{Kind: "PersistentVolumeClaim", Namespace: "prod", Name: "pgdata"},
		Reason:         "ProvisioningFailed",
		Count:          7,
		Source:         corev1.EventSource{Component: "csi-provisioner"},
		Message:        "boom",
		LastTimestamp:  metav1.NewTime(at),
	}
	got := flattenEvent(&full)
	// The involvedObject is what the export LEARNS before it redacts any message, so losing it
	// here would silently un-redact every sentence that names the object.
	if got.Namespace != "prod" || got.Name != "pgdata" || got.Kind != "PersistentVolumeClaim" {
		t.Errorf("the involvedObject was not carried: %+v", got)
	}
	if got.Count != 7 {
		t.Errorf("count = %d, want 7: it is half the dedup key, and a failure that repeated four "+
			"hundred times must not look like one that happened once", got.Count)
	}
}

// fakeReader is a client.Reader that answers List for the crystalbackup.io kinds and nothing
// else, so StateStream can be exercised without a cluster.
type fakeReader struct {
	byKind map[string][]unstructured.Unstructured
	fail   map[string]error
}

func (f *fakeReader) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return errors.New("not implemented")
}

func (f *fakeReader) List(_ context.Context, list client.ObjectList, _ ...client.ListOption) error {
	ul, ok := list.(*unstructured.UnstructuredList)
	if !ok {
		return errors.New("not an unstructured list")
	}
	kind := ul.GetKind()
	if err, bad := f.fail[kind]; bad {
		return err
	}
	ul.Items = f.byKind[kind]
	return nil
}

// TestStateSnapshotKeepsStatusAndNotSpec. The spec is configuration: every daily self-check
// carries it, it does not change hour to hour, and it is where the free text lives. What a
// fortnight is for is the status.
func TestStateSnapshotKeepsStatusAndNotSpec(t *testing.T) {
	store := newTestStore(t, 1<<20)
	item := unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "crystalbackup.io/v1alpha1",
		"kind":       kindBackup,
		"metadata": map[string]any{
			"name": "nightly-1", "namespace": "acme-prod",
			"creationTimestamp": day0.Format(time.RFC3339),
		},
		"spec":   map[string]any{"location": "primary-eu", "secretName": "do-not-capture"},
		"status": map[string]any{"phase": "Succeeded", "snapshotID": "abc123"},
	}}
	r := &fakeReader{
		byKind: map[string][]unstructured.Unstructured{"BackupList": {item}},
		fail:   map[string]error{"RestoreList": errors.New("customresourcedefinitions.apiextensions.k8s.io \"restores\" not found")},
	}

	s := NewStateStream(r, store)
	s.Snapshot(t.Context(), day0.Add(time.Hour))

	lines, err := readNDJSONGz(store.segmentPath(StreamState, day0))
	if err != nil {
		t.Fatalf("the state stream wrote nothing: %v", err)
	}
	if len(lines) != 1 {
		t.Fatalf("%d snapshot(s), want 1", len(lines))
	}
	var snap StateSnapshot
	if err := json.Unmarshal(lines[0], &snap); err != nil {
		t.Fatal(err)
	}
	if snap.Kind != kindBackup || snap.Namespace != "acme-prod" || snap.Name != "nightly-1" {
		t.Errorf("identity was not captured: %+v", snap)
	}
	if len(snap.Status) == 0 {
		t.Fatal("the status was not captured, which is the only part that changes hour to hour")
	}
	var status map[string]any
	if err := json.Unmarshal(snap.Status, &status); err != nil {
		t.Fatal(err)
	}
	if status["phase"] != "Succeeded" {
		t.Errorf("status = %v", status)
	}
	// The spec is deliberately NOT here, and a Secret NAME appearing in the archive is the cheap
	// way to notice if it ever becomes so.
	for _, line := range lines {
		if bytes.Contains(line, []byte("do-not-capture")) {
			t.Errorf("the spec reached the volume: %s", line)
		}
	}

	// A kind whose CRD is absent is recorded once, not silently swallowed and not once per hour
	// for a fortnight.
	errs := store.ErrorsFor(StreamState)
	if len(errs) != 1 {
		t.Fatalf("%d error(s) for a missing CRD, want 1: %+v", len(errs), errs)
	}
	s.Snapshot(t.Context(), day0.Add(2*time.Hour))
	if got := store.ErrorsFor(StreamState); len(got) != 1 || got[0].Count != 2 {
		t.Errorf("the repeated failure did not coalesce: %+v", got)
	}
}

// TestStateSnapshotWritesNothingWhenThereIsNothing. An empty cluster must not produce a segment
// full of empty lines that the manifest would then count as coverage.
func TestStateSnapshotWritesNothingWhenThereIsNothing(t *testing.T) {
	store := newTestStore(t, 1<<20)
	s := NewStateStream(&fakeReader{}, store)
	s.Snapshot(t.Context(), day0)
	if got := len(store.Days(StreamState)); got != 0 {
		t.Errorf("%d state segment(s) for a cluster with no CRs, want 0", got)
	}
}
