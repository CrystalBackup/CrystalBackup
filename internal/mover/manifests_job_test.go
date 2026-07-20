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
	"testing"
)

func manifestJobRequest() JobRequest {
	return JobRequest{
		Name:               "manifests-c-team-x",
		Namespace:          "crystal-backup-system",
		Image:              "ghcr.io/crystalbackup/mover@sha256:abc",
		Operation:          OpManifestsBackup,
		ResticArgs:         []string{"backup", "/manifests/c-team-x"},
		RepoURL:            "s3:https://s3.example.net/bucket/prod/prod-eu-1",
		SecretName:         "creds",
		ServiceAccountName: "crystal-manifest-mover",
		ManifestsVolume:    true,
	}
}

// I6 has exactly one exception and it must be spelled out, not inferred: the manifest mover
// names a ServiceAccount and gets its token, because reading the API server IS its job.
func TestManifestMoverGetsItsServiceAccountAndToken(t *testing.T) {
	spec := BuildJob(manifestJobRequest()).Spec.Template.Spec

	if spec.ServiceAccountName != "crystal-manifest-mover" {
		t.Errorf("ServiceAccountName = %q, want crystal-manifest-mover", spec.ServiceAccountName)
	}
	if spec.AutomountServiceAccountToken == nil || !*spec.AutomountServiceAccountToken {
		t.Error("the manifest mover must automount its token; without it the dump cannot read the API server")
	}

	var mounted bool
	for _, m := range spec.Containers[0].VolumeMounts {
		if m.MountPath == ManifestsRoot {
			mounted = true
		}
	}
	if !mounted {
		t.Errorf("no writable volume at %s; the dump has nowhere to write under a read-only root filesystem", ManifestsRoot)
	}
}

// The far more important half: every OTHER job shape keeps zero API access. A regression here
// would hand a data mover — which holds the repository credentials — an API token.
func TestDataAndMaintenanceJobsKeepZeroAPIAccess(t *testing.T) {
	for name, req := range map[string]JobRequest{
		"data backup": {
			Name: "b", Namespace: "crystal-backup-system", Image: "img",
			Operation: OpBackup, ResticArgs: []string{"backup", "/data/ns/pvc"},
			SecretName: "creds", PVC: &PVCMount{ClaimName: "pvc", MountPath: "/data/ns/pvc"},
		},
		"maintenance prune": {
			Name: "p", Namespace: "crystal-backup-system", Image: "img",
			Operation: OpPrune, ResticArgs: []string{"prune"}, SecretName: "creds",
		},
	} {
		t.Run(name, func(t *testing.T) {
			spec := BuildJob(req).Spec.Template.Spec
			if spec.ServiceAccountName != "" {
				t.Errorf("ServiceAccountName = %q, want empty — only the manifest mover names one", spec.ServiceAccountName)
			}
			if spec.AutomountServiceAccountToken == nil || *spec.AutomountServiceAccountToken {
				t.Error("token must not be automounted: a mover holding repository credentials " +
					"must have no way to reach the API server (I6)")
			}
			for _, m := range spec.Containers[0].VolumeMounts {
				if m.MountPath == ManifestsRoot {
					t.Error("a non-manifest job should not carry the manifest scratch volume")
				}
			}
		})
	}
}

// The dump destination and the snapshot's stored path are the same string by construction.
// If these ever diverge, restic records the snapshot under a path no retention group or
// restore looks at — and nothing would fail loudly.
func TestManifestsRootAgreesWithTheResticPath(t *testing.T) {
	const namespace = "c-team-x"
	want := "/manifests/" + namespace
	if got := ManifestsRoot + "/" + namespace; got != want {
		t.Errorf("dump destination %q != restic identity path %q", got, want)
	}
}

func TestSummaryToResultEchoesTheOperationItRanFor(t *testing.T) {
	for _, op := range []Operation{OpBackup, OpManifestsBackup} {
		got := SummaryToResult(op, ResticBackupSummary{SnapshotID: "abc", DataAdded: 10})
		if got.Operation != string(op) {
			t.Errorf("SummaryToResult(%s) echoed %q; the controller checks this field to confirm "+
				"it got the result it asked for", op, got.Operation)
		}
	}
}

func TestMoverResultCarriesManifestAccounting(t *testing.T) {
	r := MoverResult{OK: true, Operation: string(OpManifestsBackup), SnapshotID: "abc",
		ResourceCount: 141, IncompleteManifests: true}
	encoded, err := r.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	back, err := ParseMoverResult(encoded)
	if err != nil {
		t.Fatalf("ParseMoverResult: %v", err)
	}
	if back.ResourceCount != 141 {
		t.Errorf("resourceCount = %d, want 141", back.ResourceCount)
	}
	if !back.IncompleteManifests {
		t.Error("incompleteManifests must survive the round trip; it is what turns into " +
			"ManifestsComplete=False, and a partial capture that looks complete is the failure to avoid")
	}
}
