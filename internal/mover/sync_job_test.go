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
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// envByName indexes a container's environment for assertions.
func envByName(env []corev1.EnvVar) map[string]corev1.EnvVar {
	out := make(map[string]corev1.EnvVar, len(env))
	for _, e := range env {
		out[e.Name] = e
	}
	return out
}

// syncRequest is the shape an external-sync Job takes: no PVC, both repositories addressed
// through rclone remotes, two independent restic passwords.
func syncRequest() JobRequest {
	return JobRequest{
		Name:             "sync-job",
		Namespace:        "crystal-backup-system",
		Image:            "ghcr.io/crystalbackup/sync@sha256:abc",
		Operation:        OpSync,
		RepoURL:          RcloneRepoURL(SyncRemoteDest, "secondary", "", "cid-1"),
		SecretName:       "sync-creds",
		FromPasswordFile: true,
		CredentialKeys:   SyncCredentialKeys(),
		ExtraEnv: []corev1.EnvVar{
			{Name: EnvFromRepository, Value: RcloneRepoURL(SyncRemoteSource, "primary", "", "cid-1")},
			{Name: EnvRcloneConfig, Value: RcloneConfigNone},
		},
	}
}

// TestSyncJobCarriesTwoIndependentPasswords is the property adr/0013 is built on: the copy
// re-encrypts, so the destination is readable under ITS OWN key and a client secondary stays
// opaque to the platform key. That only holds if both password files reach the pod.
//
// They share one Secret and one mount by construction — a mounted Secret projects every key to a
// same-named file — so the failure this guards is a missing env var, not a missing volume.
func TestSyncJobCarriesTwoIndependentPasswords(t *testing.T) {
	env := envByName(BuildJob(syncRequest()).Spec.Template.Spec.Containers[0].Env)

	dst, ok := env[envPasswordFile]
	if !ok || dst.Value != ResticPasswordFilePath {
		t.Fatalf("%s = %q, want %q (the DESTINATION password)", envPasswordFile, dst.Value, ResticPasswordFilePath)
	}
	src, ok := env[EnvFromPasswordFile]
	if !ok || src.Value != ResticFromPasswordFilePath {
		t.Fatalf("%s = %q, want %q (the SOURCE password)", EnvFromPasswordFile, src.Value, ResticFromPasswordFilePath)
	}
	if dst.Value == src.Value {
		t.Fatal("both passwords resolve to the same file; the copy would not be re-encrypting anything")
	}
}

// TestSyncJobDirection pins which repository is which, in the one place the two are assembled.
//
// restic's spelling is asymmetric: the UNQUALIFIED RESTIC_REPOSITORY is the DESTINATION and
// RESTIC_FROM_REPOSITORY is the SOURCE. Getting it backwards does not fail — it succeeds at
// copying the secondary over the primary, which for a DR copy means the backup destroying what
// it protects. restic.SyncArgs cannot name a repository at all, so this env pair is the entire
// definition of the direction and deserves an explicit assertion.
func TestSyncJobDirection(t *testing.T) {
	env := envByName(BuildJob(syncRequest()).Spec.Template.Spec.Containers[0].Env)

	if got := env[envRepository].Value; got != "rclone:dst:secondary/cid-1" {
		t.Errorf("%s = %q; the UNQUALIFIED repository must be the DESTINATION", envRepository, got)
	}
	if got := env[EnvFromRepository].Value; got != "rclone:src:primary/cid-1" {
		t.Errorf("%s = %q; the FROM repository must be the SOURCE", EnvFromRepository, got)
	}
}

// TestSyncJobUsesPerRemoteCredentials guards the substitution that makes two credential sets
// possible at all.
//
// restic over its own s3 backend reads ONE AWS_ACCESS_KEY_ID for the whole process, which is
// exactly why a cross-account sync needs rclone. Two things must therefore be true: the rclone
// per-remote keys are present, and the AWS pair is ABSENT — present, it would be a required
// secretKeyRef into keys this Secret has no reason to hold, so the pod would never start; and if
// it did, it would advertise a credential path nothing beneath restic reads once the repository
// URL says rclone:.
func TestSyncJobUsesPerRemoteCredentials(t *testing.T) {
	env := envByName(BuildJob(syncRequest()).Spec.Template.Spec.Containers[0].Env)

	for _, key := range SyncCredentialKeys() {
		e, ok := env[key]
		if !ok {
			t.Fatalf("%s missing; the remote it configures cannot authenticate", key)
		}
		if e.ValueFrom == nil || e.ValueFrom.SecretKeyRef == nil {
			t.Fatalf("%s is not a secretKeyRef; a credential must never be an inline value", key)
		}
		if e.ValueFrom.SecretKeyRef.Key != key {
			t.Errorf("%s reads Secret key %q; the env name and the data key are one constant",
				key, e.ValueFrom.SecretKeyRef.Key)
		}
		if e.ValueFrom.SecretKeyRef.Optional != nil {
			t.Errorf("%s is optional; a missing credential must stop the pod, not produce an "+
				"unreachable-repository report", key)
		}
	}
	for _, key := range []string{SecretKeyAWSAccessKeyID, SecretKeyAWSSecretAccessKey} {
		if _, present := env[key]; present {
			t.Errorf("sync Job carries %s: it references a Secret key the sync Secret does not hold, "+
				"so the pod would never start", key)
		}
	}
}

// TestSyncJobNeverUsesTheGlobalRcloneForm is the trap this design exists to avoid.
//
// RCLONE_S3_ACCESS_KEY_ID configures the s3 BACKEND, globally: set it for the source and the
// destination inherits it. That is the one-credential-set limitation restic already has, merely
// relocated — a sync that silently authenticates both repositories with the source's account,
// failing or, worse, succeeding against the wrong bucket.
func TestSyncJobNeverUsesTheGlobalRcloneForm(t *testing.T) {
	for _, e := range BuildJob(syncRequest()).Spec.Template.Spec.Containers[0].Env {
		if strings.HasPrefix(e.Name, "RCLONE_S3_") {
			t.Fatalf("env carries the backend-global %s; only the per-remote "+
				"RCLONE_CONFIG_<REMOTE>_* form keeps the two credential sets independent", e.Name)
		}
	}
}

// TestSyncJobPinsTheRcloneConfigFile: the remotes are defined entirely from environment, and
// /dev/null is what makes that exhaustive rather than merely usual. Without it, a config file
// shipped in the image or mounted by a later change could redefine "src" or "dst" — silently
// pointing a copy at somebody else's storage.
func TestSyncJobPinsTheRcloneConfigFile(t *testing.T) {
	env := envByName(BuildJob(syncRequest()).Spec.Template.Spec.Containers[0].Env)
	if got := env[EnvRcloneConfig].Value; got != RcloneConfigNone {
		t.Fatalf("%s = %q, want %q", EnvRcloneConfig, got, RcloneConfigNone)
	}
}

// TestSyncJobIsAMaintenanceShape: a copy reads and writes repositories, never a PVC. It must
// therefore mount no data volume and hold no capability — DAC_OVERRIDE exists to read files
// owned by arbitrary UIDs, which a sync never does.
func TestSyncJobIsAMaintenanceShape(t *testing.T) {
	pod := BuildJob(syncRequest()).Spec.Template.Spec
	for _, v := range pod.Volumes {
		if v.PersistentVolumeClaim != nil {
			t.Fatalf("sync Job mounts PVC %q; a copy touches no volume", v.PersistentVolumeClaim.ClaimName)
		}
	}
	if caps := pod.Containers[0].SecurityContext.Capabilities.Add; len(caps) != 0 {
		t.Fatalf("sync Job adds capabilities %v; a repository-only operation needs none", caps)
	}
	if pod.ServiceAccountName != "" {
		t.Fatalf("sync Job runs as %q; it reaches no API server and must keep the zero-API posture (I6)",
			pod.ServiceAccountName)
	}
}

// TestDefaultCredentialKeysUnchanged: CredentialKeys was introduced for sync, and every other
// operation must keep the S3 pair it has always had. An empty value meaning "no credentials"
// would break every backup in the fleet at once.
func TestDefaultCredentialKeysUnchanged(t *testing.T) {
	env := envByName(BuildJob(JobRequest{Operation: OpBackup, SecretName: "creds"}).
		Spec.Template.Spec.Containers[0].Env)
	for _, key := range []string{SecretKeyAWSAccessKeyID, SecretKeyAWSSecretAccessKey} {
		if _, ok := env[key]; !ok {
			t.Fatalf("a request with no CredentialKeys lost %s", key)
		}
	}
	if _, ok := env[EnvFromPasswordFile]; ok {
		t.Errorf("a request with FromPasswordFile unset carries %s; RESTIC_FROM_REPOSITORY in the "+
			"environment also makes `restic init` fail outright, so this must not leak", EnvFromPasswordFile)
	}
}

// TestRcloneRemoteEnvShape pins the name the per-remote form produces, since agreeing with rclone
// here is what the whole scheme rests on: rclone reads RCLONE_CONFIG_<REMOTE>_<KEY> with the
// remote upper-cased, and a name it does not recognise is silently ignored — leaving a remote
// with no credentials and a copy that fails to authenticate for no visible reason.
func TestRcloneRemoteEnvShape(t *testing.T) {
	if got := RcloneRemoteEnv(SyncRemoteSource, RcloneKeyAccessKeyID); got != "RCLONE_CONFIG_SRC_ACCESS_KEY_ID" {
		t.Fatalf("RcloneRemoteEnv = %q, want RCLONE_CONFIG_SRC_ACCESS_KEY_ID", got)
	}
	if got := RcloneRemoteEnv(SyncRemoteDest, RcloneKeyEndpoint); got != "RCLONE_CONFIG_DST_ENDPOINT" {
		t.Fatalf("RcloneRemoteEnv = %q, want RCLONE_CONFIG_DST_ENDPOINT", got)
	}
}

// TestRcloneRepoURLOmitsTheEndpoint: an rclone remote carries its own endpoint in its config
// block, so repeating it in the path would be read as part of the bucket name — addressing a
// bucket that does not exist. This is the one structural difference from RepoURL, and the rest
// of the path must stay identical so the same repository is reachable by either spelling.
func TestRcloneRepoURLOmitsTheEndpoint(t *testing.T) {
	cases := map[string]struct{ remote, bucket, prefix, clusterID, want string }{
		"no prefix":       {"src", "backups", "", "cid-1", "rclone:src:backups/cid-1"},
		"with prefix":     {"dst", "backups", "team", "cid-1", "rclone:dst:backups/team/cid-1"},
		"slashy prefix":   {"dst", "backups", "/team/", "cid-1", "rclone:dst:backups/team/cid-1"},
		"slashy bucket":   {"src", "/backups/", "", "cid-1", "rclone:src:backups/cid-1"},
		"empty clusterID": {"src", "backups", "team", "", "rclone:src:backups/team"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := RcloneRepoURL(tc.remote, tc.bucket, tc.prefix, tc.clusterID)
			if got != tc.want {
				t.Fatalf("RcloneRepoURL = %q, want %q", got, tc.want)
			}
			if strings.Contains(got, "://") {
				t.Fatalf("%q carries a scheme; the endpoint belongs to the rclone remote, not the path", got)
			}
		})
	}
}

// TestSyncCredentialKeysCoverBothRemotes: four keys, two per remote. Three would leave one
// repository unauthenticated; duplicates across remotes would mean one account reaching both.
func TestSyncCredentialKeysCoverBothRemotes(t *testing.T) {
	keys := SyncCredentialKeys()
	want := []string{
		"RCLONE_CONFIG_SRC_ACCESS_KEY_ID",
		"RCLONE_CONFIG_SRC_SECRET_ACCESS_KEY",
		"RCLONE_CONFIG_DST_ACCESS_KEY_ID",
		"RCLONE_CONFIG_DST_SECRET_ACCESS_KEY",
	}
	if !slices.Equal(keys, want) {
		t.Fatalf("SyncCredentialKeys = %v, want %v", keys, want)
	}
}
