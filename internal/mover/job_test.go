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
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestBuildJobTopologySpread(t *testing.T) {
	withSpread := BuildJob(JobRequest{Operation: OpBackup, SpreadOverLabels: map[string]string{"app": "mover"}})
	tsc := withSpread.Spec.Template.Spec.TopologySpreadConstraints
	if len(tsc) != 1 {
		t.Fatalf("want 1 topology-spread constraint, got %d", len(tsc))
	}
	if tsc[0].MaxSkew != 1 || tsc[0].TopologyKey != corev1.LabelHostname || tsc[0].WhenUnsatisfiable != corev1.ScheduleAnyway {
		t.Fatalf("unexpected constraint shape: %+v", tsc[0])
	}
	if tsc[0].LabelSelector == nil || tsc[0].LabelSelector.MatchLabels["app"] != "mover" {
		t.Fatalf("constraint selector not the given labels: %+v", tsc[0].LabelSelector)
	}

	// A soft (ScheduleAnyway) constraint must never keep a mover from scheduling: without
	// SpreadOverLabels there is no constraint at all, so the scheduler's defaults apply.
	without := BuildJob(JobRequest{Operation: OpBackup})
	if got := without.Spec.Template.Spec.TopologySpreadConstraints; len(got) != 0 {
		t.Fatalf("want no constraint without SpreadOverLabels, got %d", len(got))
	}
}

// --- small lookup helpers so the assertions read by NAME, independent of slice order -----

func findEnv(t *testing.T, env []corev1.EnvVar, name string) corev1.EnvVar {
	t.Helper()
	for _, e := range env {
		if e.Name == name {
			return e
		}
	}
	t.Fatalf("env %q not found in %v", name, env)
	return corev1.EnvVar{}
}

func hasEnv(env []corev1.EnvVar, name string) bool {
	for _, e := range env {
		if e.Name == name {
			return true
		}
	}
	return false
}

func findVolume(t *testing.T, vols []corev1.Volume, name string) corev1.Volume {
	t.Helper()
	for _, v := range vols {
		if v.Name == name {
			return v
		}
	}
	t.Fatalf("volume %q not found in %v", name, vols)
	return corev1.Volume{}
}

func hasVolume(vols []corev1.Volume, name string) bool {
	for _, v := range vols {
		if v.Name == name {
			return true
		}
	}
	return false
}

func findMount(t *testing.T, mounts []corev1.VolumeMount, name string) corev1.VolumeMount {
	t.Helper()
	for _, m := range mounts {
		if m.Name == name {
			return m
		}
	}
	t.Fatalf("mount %q not found in %v", name, mounts)
	return corev1.VolumeMount{}
}

func hasMount(mounts []corev1.VolumeMount, name string) bool {
	for _, m := range mounts {
		if m.Name == name {
			return true
		}
	}
	return false
}

// dataRequest is a representative DATA (backup) request reused across assertions.
func dataRequest() JobRequest {
	return JobRequest{
		Name:         "backup-team-x-pvc-1-abc",
		Namespace:    "crystal-backup-system",
		Image:        "ghcr.io/crystalbackup/crystalbackup:v0.1.0",
		Operation:    OpBackup,
		ResticArgs:   []string{"backup", "/data/team-x/pvc-1", "--host", "prod-eu-1", "--tag", "crystalbackup"},
		RepoURL:      "s3:https://s3.example.net/bucket/crystal/prod-eu-1",
		SecretName:   "mover-secret-abc",
		PVC:          &PVCMount{ClaimName: "pvc-1", MountPath: "/data/team-x/pvc-1"},
		Labels:       map[string]string{"crystalbackup.io/run": "run-1", "app": "mover"},
		BackoffLimit: 3,
		TTLSeconds:   600,
		GoMemLimit:   "900MiB",
		ExtraEnv:     []corev1.EnvVar{{Name: "AWS_DEFAULT_REGION", Value: "eu-west-1"}},
	}
}

// TestBuildJobDataRequest asserts the entire runtime contract for a backup job: entrypoint,
// argv, data volume, env (plain + secretKeyRef), securityContext, pod hardening, Job knobs
// and labels. This is the shape every backup flows through, so each field is pinned.
func TestBuildJobDataRequest(t *testing.T) {
	req := dataRequest()
	job := BuildJob(req)

	if job.Name != req.Name || job.Namespace != req.Namespace {
		t.Errorf("Job meta = (%q,%q), want (%q,%q)", job.Name, job.Namespace, req.Name, req.Namespace)
	}

	containers := job.Spec.Template.Spec.Containers
	if len(containers) != 1 {
		t.Fatalf("containers = %d, want 1", len(containers))
	}
	c := containers[0]

	if c.Image != req.Image {
		t.Errorf("image = %q, want %q", c.Image, req.Image)
	}

	// command == exactly the shim binary; restic invocation is entirely in args.
	if !reflect.DeepEqual(c.Command, []string{MoverBinaryPath}) {
		t.Errorf("command = %v, want [%q]", c.Command, MoverBinaryPath)
	}

	// args == --operation <op> -- <restic argv verbatim>.
	wantArgs := []string{"--operation", "backup", "--",
		"backup", "/data/team-x/pvc-1", "--host", "prod-eu-1", "--tag", "crystalbackup"}
	if !reflect.DeepEqual(c.Args, wantArgs) {
		t.Errorf("args = %v, want %v", c.Args, wantArgs)
	}

	// --- data volume + mount ------------------------------------------------------------
	dv := findVolume(t, job.Spec.Template.Spec.Volumes, volumeData)
	if dv.PersistentVolumeClaim == nil {
		t.Fatalf("data volume %q has no PersistentVolumeClaim source: %+v", volumeData, dv)
	}
	if dv.PersistentVolumeClaim.ClaimName != req.PVC.ClaimName {
		t.Errorf("data volume claim = %q, want %q", dv.PersistentVolumeClaim.ClaimName, req.PVC.ClaimName)
	}
	if !dv.PersistentVolumeClaim.ReadOnly {
		t.Error("data volume PVC source ReadOnly = false, want true")
	}
	dm := findMount(t, c.VolumeMounts, volumeData)
	if dm.MountPath != req.PVC.MountPath {
		t.Errorf("data mount path = %q, want %q (must equal the restic backup path)", dm.MountPath, req.PVC.MountPath)
	}
	if !dm.ReadOnly {
		t.Error("data mount ReadOnly = false, want true")
	}

	// --- secret + scratch volumes/mounts ------------------------------------------------
	sv := findVolume(t, job.Spec.Template.Spec.Volumes, volumeSecret)
	if sv.Secret == nil || sv.Secret.SecretName != req.SecretName {
		t.Errorf("secret volume = %+v, want Secret{SecretName:%q}", sv.VolumeSource, req.SecretName)
	}
	sm := findMount(t, c.VolumeMounts, volumeSecret)
	if sm.MountPath != SecretMountPath || !sm.ReadOnly {
		t.Errorf("secret mount = (path %q, ro %v), want (%q, true)", sm.MountPath, sm.ReadOnly, SecretMountPath)
	}
	if m := findMount(t, c.VolumeMounts, volumeCache); m.MountPath != CacheDir {
		t.Errorf("cache mount path = %q, want %q", m.MountPath, CacheDir)
	}
	if m := findMount(t, c.VolumeMounts, volumeTmp); m.MountPath != "/tmp" {
		t.Errorf("tmp mount path = %q, want /tmp", m.MountPath)
	}
	for _, name := range []string{volumeCache, volumeTmp} {
		v := findVolume(t, job.Spec.Template.Spec.Volumes, name)
		if v.EmptyDir == nil {
			t.Errorf("volume %q is not an emptyDir: %+v", name, v.VolumeSource)
		}
	}
	// The restic cache is CAPPED (an unbounded one fills the node's disk and takes other people's
	// pods down with it), and /tmp is deliberately NOT — a second eviction trigger for a directory
	// that holds one pack file at a time would be all risk and no gain.
	cacheLimit := findVolume(t, job.Spec.Template.Spec.Volumes, volumeCache).EmptyDir.SizeLimit
	wantCacheLimit := resource.MustParse(defaultCacheSizeLimit)
	if cacheLimit == nil || cacheLimit.Cmp(wantCacheLimit) != 0 {
		t.Errorf("cache emptyDir sizeLimit = %v, want %s", cacheLimit, wantCacheLimit.String())
	}
	if got := findVolume(t, job.Spec.Template.Spec.Volumes, volumeTmp).EmptyDir.SizeLimit; got != nil {
		t.Errorf("tmp emptyDir sizeLimit = %v, want unset", got)
	}

	// --- fixed env ----------------------------------------------------------------------
	env := c.Env
	assertEnvValue(t, env, "RESTIC_REPOSITORY", req.RepoURL)
	assertEnvValue(t, env, "RESTIC_PASSWORD_FILE", ResticPasswordFilePath)
	assertEnvValue(t, env, "RESTIC_CACHE_DIR", CacheDir)
	assertEnvValue(t, env, "TMPDIR", "/tmp")
	assertEnvValue(t, env, "GOMEMLIMIT", "900MiB")
	assertEnvValue(t, env, "AWS_DEFAULT_REGION", "eu-west-1") // caller ExtraEnv passed through

	// AWS creds arrive by secretKeyRef on the per-Job Secret, never as literal values.
	for _, key := range []string{SecretKeyAWSAccessKeyID, SecretKeyAWSSecretAccessKey} {
		e := findEnv(t, env, key)
		if e.Value != "" {
			t.Errorf("%s must have no literal Value, got %q", key, e.Value)
		}
		if e.ValueFrom == nil || e.ValueFrom.SecretKeyRef == nil {
			t.Fatalf("%s has no secretKeyRef: %+v", key, e)
		}
		ref := e.ValueFrom.SecretKeyRef
		if ref.Name != req.SecretName || ref.Key != key {
			t.Errorf("%s secretKeyRef = (name %q, key %q), want (%q, %q)", key, ref.Name, ref.Key, req.SecretName, key)
		}
	}

	// --- container securityContext ------------------------------------------------------
	sc := c.SecurityContext
	if sc == nil {
		t.Fatal("container securityContext is nil")
	}
	if got := derefInt64(sc.RunAsUser); got != 0 {
		t.Errorf("runAsUser = %d, want 0", got)
	}
	if got := derefInt64(sc.RunAsGroup); got != 0 {
		t.Errorf("runAsGroup = %d, want 0", got)
	}
	if !derefBool(sc.ReadOnlyRootFilesystem) {
		t.Error("readOnlyRootFilesystem = false, want true")
	}
	if derefBool(sc.AllowPrivilegeEscalation) {
		t.Error("allowPrivilegeEscalation = true, want false")
	}
	if sc.Capabilities == nil ||
		!reflect.DeepEqual(sc.Capabilities.Drop, []corev1.Capability{"ALL"}) ||
		!reflect.DeepEqual(sc.Capabilities.Add, []corev1.Capability{"DAC_OVERRIDE"}) {
		t.Errorf("capabilities = %+v, want drop [ALL] add [DAC_OVERRIDE]", sc.Capabilities)
	}
	if sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("container seccompProfile = %+v, want RuntimeDefault", sc.SeccompProfile)
	}

	// termination-message protocol: read the file, do NOT fall back to logs (empty == crash).
	if c.TerminationMessagePath != TerminationMessagePath {
		t.Errorf("terminationMessagePath = %q, want %q", c.TerminationMessagePath, TerminationMessagePath)
	}
	if c.TerminationMessagePolicy != corev1.TerminationMessageReadFile {
		t.Errorf("terminationMessagePolicy = %q, want ReadFile", c.TerminationMessagePolicy)
	}

	// Sizing comes from the profile table for THIS operation — never from nothing. A backup with
	// no explicit table is the built-in data class, and a pod with no requests at all is BestEffort:
	// first in line for eviction under node pressure, which is the state a backup must not be in.
	wantBackup := Profiles(nil).For(OpBackup)
	if !reflect.DeepEqual(c.Resources.Requests, wantBackup.Requests) {
		t.Errorf("resources.requests = %+v, want %+v (the built-in backup profile)", c.Resources.Requests, wantBackup.Requests)
	}
	if !reflect.DeepEqual(c.Resources.Limits, wantBackup.Limits) {
		t.Errorf("resources.limits = %+v, want %+v (the built-in backup profile)", c.Resources.Limits, wantBackup.Limits)
	}

	// --- pod hardening ------------------------------------------------------------------
	pod := job.Spec.Template.Spec
	if !isFalse(pod.AutomountServiceAccountToken) {
		t.Errorf("automountServiceAccountToken = %v, want false", pod.AutomountServiceAccountToken)
	}
	if pod.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("restartPolicy = %q, want Never", pod.RestartPolicy)
	}
	if pod.ServiceAccountName != "" {
		t.Errorf("serviceAccountName = %q, want empty (default SA)", pod.ServiceAccountName)
	}
	if pod.SecurityContext == nil || pod.SecurityContext.SeccompProfile == nil ||
		pod.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("pod seccompProfile = %+v, want RuntimeDefault", pod.SecurityContext)
	}

	// --- Job knobs ----------------------------------------------------------------------
	if got := derefInt32(job.Spec.BackoffLimit); got != req.BackoffLimit {
		t.Errorf("backoffLimit = %d, want %d", got, req.BackoffLimit)
	}
	if got := derefInt32(job.Spec.TTLSecondsAfterFinished); got != req.TTLSeconds {
		t.Errorf("ttlSecondsAfterFinished = %d, want %d", got, req.TTLSeconds)
	}

	// --- labels on both Job and pod template --------------------------------------------
	if !reflect.DeepEqual(job.Labels, req.Labels) {
		t.Errorf("Job labels = %v, want %v", job.Labels, req.Labels)
	}
	if !reflect.DeepEqual(job.Spec.Template.Labels, req.Labels) {
		t.Errorf("pod template labels = %v, want %v", job.Spec.Template.Labels, req.Labels)
	}
}

// TestBuildJobMaintenanceRequest asserts a maintenance job (nil PVC, OpInit): no data
// volume or mount at all, args carry just the operation + restic argv, GOMEMLIMIT is absent
// when unset, and the shared hardening still applies.
func TestBuildJobMaintenanceRequest(t *testing.T) {
	req := JobRequest{
		Name:         "init-prod-eu-1",
		Namespace:    "crystal-backup-system",
		Image:        "ghcr.io/crystalbackup/crystalbackup:v0.1.0",
		Operation:    OpInit,
		ResticArgs:   []string{"init"},
		RepoURL:      "s3:https://s3.example.net/bucket/crystal/prod-eu-1",
		SecretName:   "mover-secret-init",
		PVC:          nil, // maintenance
		Labels:       map[string]string{"crystalbackup.io/op": "init"},
		BackoffLimit: 5,
		TTLSeconds:   300,
		GoMemLimit:   "", // must be omitted
	}
	job := BuildJob(req)
	c := job.Spec.Template.Spec.Containers[0]

	// No data volume or mount.
	if hasVolume(job.Spec.Template.Spec.Volumes, volumeData) {
		t.Error("maintenance job has a data-source volume, want none")
	}
	if hasMount(c.VolumeMounts, volumeData) {
		t.Error("maintenance job has a data-source mount, want none")
	}

	// args == --operation init -- init.
	wantArgs := []string{"--operation", "init", "--", "init"}
	if !reflect.DeepEqual(c.Args, wantArgs) {
		t.Errorf("args = %v, want %v", c.Args, wantArgs)
	}

	// GOMEMLIMIT omitted when the request leaves it empty.
	if hasEnv(c.Env, "GOMEMLIMIT") {
		t.Error("GOMEMLIMIT present, want omitted when GoMemLimit is empty")
	}

	// The repo/password/cache/tmp env and the secret + scratch volumes are still there.
	assertEnvValue(t, c.Env, "RESTIC_REPOSITORY", req.RepoURL)
	assertEnvValue(t, c.Env, "RESTIC_PASSWORD_FILE", ResticPasswordFilePath)
	if !hasVolume(job.Spec.Template.Spec.Volumes, volumeSecret) ||
		!hasVolume(job.Spec.Template.Spec.Volumes, volumeCache) ||
		!hasVolume(job.Spec.Template.Spec.Volumes, volumeTmp) {
		t.Error("maintenance job is missing a secret/cache/tmp volume")
	}

	// Shared hardening: SA token not mounted, restartPolicy Never, RO root fs.
	if !isFalse(job.Spec.Template.Spec.AutomountServiceAccountToken) {
		t.Error("automountServiceAccountToken != false")
	}
	if job.Spec.Template.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Error("restartPolicy != Never")
	}
	if !derefBool(c.SecurityContext.ReadOnlyRootFilesystem) {
		t.Error("readOnlyRootFilesystem != true")
	}
}

// TestBuildJobExtraEnvAppendedLast pins the env ORDER: the fixed protocol vars come first,
// GOMEMLIMIT (when set) after them, and caller ExtraEnv last, so a caller can never shadow
// RESTIC_* / AWS_* by supplying an earlier duplicate.
func TestBuildJobExtraEnvAppendedLast(t *testing.T) {
	req := dataRequest()
	req.ExtraEnv = []corev1.EnvVar{
		{Name: "RESTIC_COMPRESSION", Value: "max"},
		{Name: "AWS_DEFAULT_REGION", Value: "eu-west-1"},
	}
	env := BuildJob(req).Spec.Template.Spec.Containers[0].Env

	idx := func(name string) int {
		for i, e := range env {
			if e.Name == name {
				return i
			}
		}
		return -1
	}
	// Every fixed var must precede every extra var.
	lastFixed := idx("GOMEMLIMIT")
	firstExtra := idx("RESTIC_COMPRESSION")
	if lastFixed < 0 || firstExtra < 0 || firstExtra <= lastFixed {
		t.Errorf("env order wrong: GOMEMLIMIT at %d, RESTIC_COMPRESSION at %d (extras must come after fixed)", lastFixed, firstExtra)
	}
	if idx("RESTIC_REPOSITORY") != 0 {
		t.Errorf("RESTIC_REPOSITORY at %d, want 0 (first)", idx("RESTIC_REPOSITORY"))
	}
}

// TestBuildJobLabelsIndependent proves the Job labels, the pod-template labels and the
// caller's map are three independent maps — mutating one must not bleed into the others.
// Sharing a single map instance across ObjectMetas is a classic aliasing footgun.
func TestBuildJobLabelsIndependent(t *testing.T) {
	req := dataRequest()
	job := BuildJob(req)

	job.Labels["app"] = "MUTATED"
	if job.Spec.Template.Labels["app"] == "MUTATED" {
		t.Error("mutating Job labels also changed pod template labels (maps are aliased)")
	}
	if req.Labels["app"] == "MUTATED" {
		t.Error("mutating Job labels also changed the caller's map (maps are aliased)")
	}
}

// TestBuildJobNilLabels confirms a request with no labels yields nil label maps (not empty
// maps), keeping an empty `labels: {}` out of the serialized object.
func TestBuildJobNilLabels(t *testing.T) {
	req := dataRequest()
	req.Labels = nil
	job := BuildJob(req)
	if job.Labels != nil {
		t.Errorf("Job labels = %v, want nil", job.Labels)
	}
	if job.Spec.Template.Labels != nil {
		t.Errorf("pod template labels = %v, want nil", job.Spec.Template.Labels)
	}
}

// --- pointer/deref helpers --------------------------------------------------------------

func assertEnvValue(t *testing.T, env []corev1.EnvVar, name, want string) {
	t.Helper()
	e := findEnv(t, env, name)
	if e.Value != want {
		t.Errorf("env %s = %q, want %q", name, e.Value, want)
	}
}

func derefInt32(p *int32) int32 {
	if p == nil {
		return -1
	}
	return *p
}

func derefInt64(p *int64) int64 {
	if p == nil {
		return -1
	}
	return *p
}

func derefBool(p *bool) bool {
	return p != nil && *p
}

// isFalse reports whether a *bool is explicitly false (non-nil and false), which is what
// automountServiceAccountToken must be — nil (defaulting to true) would be wrong.
func isFalse(p *bool) bool {
	return p != nil && !*p
}

// TestBuildJobWithoutTraceEnvIsUnchanged is the byte-reproducibility half of the tracing lot's
// no-op promise (spec/05-observability.md §5): an install with no OTLP collector must get EXACTLY
// the Job spec it got before tracing existed. Two blank variables in every mover pod would be a
// small thing that is nonetheless a change to a spec this package promises is a pure function of
// its request.
func TestBuildJobWithoutTraceEnvIsUnchanged(t *testing.T) {
	req := dataRequest()
	req.TraceEnv = nil
	withoutField := BuildJob(req).Spec.Template.Spec.Containers[0].Env

	req.TraceEnv = map[string]string{} // an empty map must behave exactly like nil
	withEmptyMap := BuildJob(req).Spec.Template.Spec.Containers[0].Env

	if len(withoutField) != len(withEmptyMap) {
		t.Fatalf("an empty TraceEnv changed the env length: %d vs %d", len(withoutField), len(withEmptyMap))
	}
	for _, e := range withoutField {
		if e.Name == "TRACEPARENT" || e.Name == "TRACESTATE" {
			t.Errorf("%s is present on a Job built with no trace context", e.Name)
		}
	}
}

// TestBuildJobTraceEnvIsSortedAndPrecedesExtraEnv pins the two properties the handover depends on.
//
// SORTED, because a mover Job spec must be byte-reproducible across releases and across builds,
// and ranging a Go map is deliberately not. BEFORE ExtraEnv, because a duplicate env var is
// resolved by the kubelet in favour of the LAST occurrence — so an operator who sets one of these
// through their own chart values overrides what the controller injected, rather than being
// silently overridden by it.
func TestBuildJobTraceEnvIsSortedAndPrecedesExtraEnv(t *testing.T) {
	req := dataRequest()
	req.TraceEnv = map[string]string{
		"TRACEPARENT":                 "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
		"OTEL_SERVICE_NAME":           "crystal-backup-mover",
		"OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector.observability:4317",
	}
	req.ExtraEnv = []corev1.EnvVar{{Name: "OTEL_SERVICE_NAME", Value: "operator-override"}}
	env := BuildJob(req).Spec.Template.Spec.Containers[0].Env

	idx := func(name string) []int {
		var out []int
		for i, e := range env {
			if e.Name == name {
				out = append(out, i)
			}
		}
		return out
	}

	// Sorted among themselves.
	endpoint, service, traceparent := idx("OTEL_EXPORTER_OTLP_ENDPOINT"), idx("OTEL_SERVICE_NAME"), idx("TRACEPARENT")
	if len(endpoint) != 1 || len(traceparent) != 1 || len(service) != 2 {
		t.Fatalf("unexpected occurrences: endpoint=%v service=%v traceparent=%v", endpoint, service, traceparent)
	}
	if endpoint[0] >= service[0] || service[0] >= traceparent[0] {
		t.Errorf("TraceEnv is not emitted in sorted key order: endpoint=%d service=%d traceparent=%d",
			endpoint[0], service[0], traceparent[0])
	}
	// And the caller's ExtraEnv wins the duplicate, by coming last.
	if service[1] <= traceparent[0] {
		t.Errorf("ExtraEnv's OTEL_SERVICE_NAME at %d does not follow the trace block ending at %d",
			service[1], traceparent[0])
	}
	if env[service[1]].Value != "operator-override" {
		t.Errorf("the last OTEL_SERVICE_NAME is %q, want the caller's override", env[service[1]].Value)
	}
	// The fixed protocol variables still come first.
	if env[0].Name != envRepository {
		t.Errorf("env[0] = %s, want %s", env[0].Name, envRepository)
	}
}
