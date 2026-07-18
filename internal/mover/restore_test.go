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

package mover

import (
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// TestBuildJobRestoreMountsReadWrite pins the one mount-mode exception of the whole mover
// contract: a restore's PVCMount.ReadWrite makes BOTH the volume source and the mount
// writable, while every other data job stays read-only at both layers (defence in depth for
// backup). The read-only default is asserted explicitly so a future zero-value change to
// PVCMount cannot silently flip backups writable.
func TestBuildJobRestoreMountsReadWrite(t *testing.T) {
	base := JobRequest{
		Name: "r", Namespace: "crystal-backup-system", Image: "img",
		RepoURL: "s3:ep/b/p/c", SecretName: "r",
	}

	restore := base
	restore.Operation = OpRestore
	restore.PVC = &PVCMount{ClaimName: "target", MountPath: "/crystal/target", ReadWrite: true}
	job := BuildJob(restore)
	spec := job.Spec.Template.Spec
	pvcSource := spec.Volumes[len(spec.Volumes)-1].PersistentVolumeClaim
	if pvcSource == nil || pvcSource.ReadOnly {
		t.Fatalf("restore volume source = %+v, want a read-WRITE PVC source", pvcSource)
	}
	mounts := spec.Containers[0].VolumeMounts
	dataMount := mounts[len(mounts)-1]
	if dataMount.MountPath != "/crystal/target" || dataMount.ReadOnly {
		t.Errorf("restore data mount = %+v, want read-write at /crystal/target", dataMount)
	}

	backup := base
	backup.Operation = OpBackup
	backup.PVC = &PVCMount{ClaimName: "clone", MountPath: "/data/ns/pvc"}
	job = BuildJob(backup)
	spec = job.Spec.Template.Spec
	pvcSource = spec.Volumes[len(spec.Volumes)-1].PersistentVolumeClaim
	if pvcSource == nil || !pvcSource.ReadOnly {
		t.Fatalf("backup volume source = %+v, want a read-ONLY PVC source", pvcSource)
	}
	mounts = spec.Containers[0].VolumeMounts
	if dataMount = mounts[len(mounts)-1]; !dataMount.ReadOnly {
		t.Errorf("backup data mount = %+v, want read-only", dataMount)
	}
}

// TestMoverCapabilitiesByOperation pins the per-operation capability sets
// (03-security-and-tenancy.md §6): restore gets the metadata-fidelity set (CHOWN,
// DAC_OVERRIDE, FOWNER, MKNOD, SETFCAP — all within the runtime default set, so PSA
// baseline still admits the pod), everything else keeps the single DAC_OVERRIDE. Drop:ALL
// is asserted too — the adds only ever sit on top of a full drop.
func TestMoverCapabilitiesByOperation(t *testing.T) {
	restoreCaps := []corev1.Capability{"CHOWN", "DAC_OVERRIDE", "FOWNER", "MKNOD", "SETFCAP"}
	for _, tc := range []struct {
		op   Operation
		want []corev1.Capability
	}{
		{OpRestore, restoreCaps},
		{OpBackup, []corev1.Capability{"DAC_OVERRIDE"}},
		{OpInit, []corev1.Capability{"DAC_OVERRIDE"}},
		{OpPrune, []corev1.Capability{"DAC_OVERRIDE"}},
	} {
		job := BuildJob(JobRequest{Name: "j", Namespace: "ns", Operation: tc.op, SecretName: "j"})
		caps := job.Spec.Template.Spec.Containers[0].SecurityContext.Capabilities
		if len(caps.Drop) != 1 || caps.Drop[0] != "ALL" {
			t.Errorf("op %s: Drop = %v, want [ALL]", tc.op, caps.Drop)
		}
		if !reflect.DeepEqual(caps.Add, tc.want) {
			t.Errorf("op %s: Add = %v, want %v", tc.op, caps.Add, tc.want)
		}
	}
}

// TestBuildJobNodeNamePin pins the same-node placement seam of the twin-PV path (adr/0016
// §2): a set JobRequest.NodeName lands verbatim on the pod spec, and the default stays
// empty so every non-pinned mover keeps normal scheduling.
func TestBuildJobNodeNamePin(t *testing.T) {
	pinned := BuildJob(JobRequest{Name: "j", Namespace: "ns", Operation: OpRestore, SecretName: "j", NodeName: "worker-3"})
	if got := pinned.Spec.Template.Spec.NodeName; got != "worker-3" {
		t.Errorf("pinned NodeName = %q, want worker-3", got)
	}
	free := BuildJob(JobRequest{Name: "j", Namespace: "ns", Operation: OpBackup, SecretName: "j"})
	if got := free.Spec.Template.Spec.NodeName; got != "" {
		t.Errorf("unpinned NodeName = %q, want empty", got)
	}
}

// TestParseRestoreSummary covers the restore summary parser over the same stream shapes the
// backup parser is tested on: a clean stream, interleaved status noise with last-wins, and
// the hard error on a summary-less stream (a clean exit whose summary is missing means the
// stream was truncated — never an implicit success).
func TestParseRestoreSummary(t *testing.T) {
	stream := strings.Join([]string{
		`{"message_type":"status","percent_done":0.5}`,
		`not json noise`,
		`{"message_type":"summary","total_files":12,"files_restored":10,"total_bytes":2048,"bytes_restored":1536}`,
	}, "\n")
	got, err := ParseRestoreSummary([]byte(stream))
	if err != nil {
		t.Fatalf("ParseRestoreSummary error: %v", err)
	}
	want := ResticRestoreSummary{MessageType: "summary", TotalBytes: 2048, BytesRestored: 1536, FilesRestored: 10}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ParseRestoreSummary = %+v, want %+v", got, want)
	}

	if _, err := ParseRestoreSummary([]byte(`{"message_type":"status"}`)); err == nil {
		t.Error("ParseRestoreSummary(no summary) = nil error, want error")
	}
	if _, err := ParseRestoreSummary(nil); err == nil {
		t.Error("ParseRestoreSummary(nil) = nil error, want error")
	}
}

// TestRestoreSummaryToResult pins the summary→result field translation: bytes_restored →
// RestoredBytes, total_bytes → SizeBytes, operation echoed as "restore", and NO snapshot id
// (a restore creates none — a non-empty id here would corrupt the volume bookkeeping).
func TestRestoreSummaryToResult(t *testing.T) {
	got := RestoreSummaryToResult(ResticRestoreSummary{TotalBytes: 100, BytesRestored: 90, FilesRestored: 3})
	want := MoverResult{OK: true, Operation: string(OpRestore), SizeBytes: 100, RestoredBytes: 90}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("RestoreSummaryToResult = %+v, want %+v", got, want)
	}
}

// TestMoverResultRestoredBytesRoundTrip asserts the new RestoredBytes field survives the
// termination-message round trip and is omitted when zero (the wire stays minimal for every
// non-restore operation).
func TestMoverResultRestoredBytesRoundTrip(t *testing.T) {
	in := MoverResult{OK: true, Operation: string(OpRestore), RestoredBytes: 42}
	encoded, err := in.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	out, err := ParseMoverResult(encoded)
	if err != nil {
		t.Fatalf("ParseMoverResult: %v", err)
	}
	if !reflect.DeepEqual(out, in) {
		t.Errorf("round trip = %+v, want %+v", out, in)
	}

	plain, err := MoverResult{OK: true, Operation: "forget"}.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if strings.Contains(plain, "restoredBytes") {
		t.Errorf("zero RestoredBytes serialized in %q, want omitted", plain)
	}
}
