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
	"maps"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Internal names for the mover container and its volumes. They are stable identifiers, not
// a wire contract with any other system, so they are unexported; the controller addresses
// the result via ContainerStatuses[0] (there is exactly one container), never by name.
const (
	containerName = "mover"

	volumeSecret = "mover-secret" // the per-Job Secret: restic password + AWS creds
	volumeCache  = "restic-cache" // restic cache emptyDir (writable)
	volumeTmp    = "tmp"          // /tmp emptyDir (writable)
	volumeData   = "data-source"  // the source PVC (data jobs only), mounted read-only
)

// tmpDir is both the TMPDIR env value and the mount path of the tmp emptyDir. restic and
// its helpers write temp files here; with a read-only root filesystem it must be a real,
// writable mount. Unexported because it is not part of the operator↔mover path contract
// the way SecretMountPath / CacheDir are.
const tmpDir = "/tmp"

// operationFlag is the CLI flag the crystal-mover shim reads to select its operation
// (`--operation <op>`); BuildJob passes it as the first arg, before restic's "--" separator.
const operationFlag = "--operation"

// Fixed environment variable names restic reads. The AWS credential names are intentionally
// absent here: they equal SecretKeyAWSAccessKeyID / SecretKeyAWSSecretAccessKey, so moverEnv
// reuses those constants for both the env var Name and the secretKeyRef Key.
const (
	envRepository   = "RESTIC_REPOSITORY"
	envPasswordFile = "RESTIC_PASSWORD_FILE"
	envCacheDir     = "RESTIC_CACHE_DIR"
	envTMPDIR       = "TMPDIR"
	envGoMemLimit   = "GOMEMLIMIT"
)

// PVCMount identifies the source volume of a DATA (backup) job. Its presence on a
// JobRequest is what makes a Job a data job rather than a maintenance job.
type PVCMount struct {
	// ClaimName is the PersistentVolumeClaim to snapshot.
	ClaimName string
	// MountPath is where the claim is mounted — READ-ONLY — inside the mover. The caller
	// sets it to the restic backup path (e.g. /data/<ns>/<pvc>) so the snapshot's stored
	// path equals its restic identity (see internal/restic.Identity.Path). BuildJob does
	// NOT invent its own mount path; it mounts exactly here.
	MountPath string
}

// JobRequest is the fully-resolved description of one mover Job. Every field is a value the
// calling controller has already decided — BuildJob adds no policy, does no lookups and
// reads no environment; it is a pure translation of this struct into a batchv1.Job. That
// keeps the Job spec a deterministic, testable function of the request.
type JobRequest struct {
	// Name and Namespace place the Job. Namespace is the operator namespace (where the
	// per-Job Secret and the mover pods live); the caller supplies it.
	Name, Namespace string
	// Image is the CrystalBackup image carrying the crystal-mover binary and restic.
	Image string
	// Operation is passed as `--operation <op>` and echoed in MoverResult.Operation.
	Operation Operation
	// ResticArgs is the restic argv AFTER the shim's "--" separator (the subcommand plus
	// its non-secret flags), built by the caller. It is forwarded verbatim.
	ResticArgs []string
	// RepoURL becomes RESTIC_REPOSITORY (see internal/restic.RepoURL).
	RepoURL string
	// SecretName is the per-Job Secret holding SecretKeyResticPassword and the two AWS keys.
	// It is both mounted (for the password file) and referenced (for the AWS env vars).
	SecretName string
	// PVC, when non-nil, makes this a data job and is mounted read-only per PVCMount. Nil
	// makes this a maintenance job with no data volume at all.
	PVC *PVCMount
	// Labels are stamped on BOTH the Job and its pod template (for discovery/selection).
	Labels map[string]string
	// BackoffLimit and TTLSeconds map to the Job's backoffLimit and ttlSecondsAfterFinished.
	BackoffLimit int32
	TTLSeconds   int32
	// GoMemLimit, when non-empty, sets GOMEMLIMIT to cap the mover's Go heap; skipped when
	// empty (an empty GOMEMLIMIT is invalid and would abort the runtime).
	GoMemLimit string
	// ExtraEnv is appended AFTER the fixed env (e.g. AWS_DEFAULT_REGION, RESTIC_COMPRESSION),
	// so a caller can add knobs without displacing the protocol variables.
	ExtraEnv []corev1.EnvVar
	// Resources are the container's requests/limits, passed through as-is.
	Resources corev1.ResourceRequirements
	// SpreadOverLabels, when non-empty, adds a SOFT topology-spread constraint (maxSkew 1 over
	// kubernetes.io/hostname, whenUnsatisfiable=ScheduleAnyway) selecting pods by these labels, so a
	// wide fan-out's movers prefer distinct nodes instead of piling onto one. Empty ⇒ no constraint.
	SpreadOverLabels map[string]string
}

// BuildJob assembles the batchv1.Job for one mover run, exactly per the package's runtime
// contract. It is pure: same request in, byte-identical Job out. A nil req.PVC yields a
// maintenance job (no data volume); a set req.PVC yields a data job that additionally
// mounts the source claim read-only at req.PVC.MountPath. No ServiceAccountName is set, so
// the Job uses the namespace default SA — and its token is not even mounted (see below).
func BuildJob(req JobRequest) *batchv1.Job {
	volumes, mounts := moverVolumes(req)

	container := corev1.Container{
		Name:    containerName,
		Image:   req.Image,
		Command: []string{MoverBinaryPath},
		// Everything after "--" is restic's own argv, forwarded verbatim by the shim. The
		// prefix literal has len == cap, so append allocates fresh and never aliases ResticArgs.
		Args:         append([]string{operationFlag, string(req.Operation), "--"}, req.ResticArgs...),
		Env:          moverEnv(req),
		VolumeMounts: mounts,
		Resources:    req.Resources,
		// root + DAC_OVERRIDE lets the mover read every source file regardless of owner or
		// mode (a backup must be able to read data it does not own), while everything else is
		// stripped: no privilege escalation, a read-only root filesystem (only the cache and
		// tmp emptyDirs are writable), all other capabilities dropped, default seccomp.
		SecurityContext: &corev1.SecurityContext{
			RunAsUser:                ptrTo[int64](0),
			RunAsGroup:               ptrTo[int64](0),
			AllowPrivilegeEscalation: ptrTo(false),
			ReadOnlyRootFilesystem:   ptrTo(true),
			Capabilities: &corev1.Capabilities{
				Drop: []corev1.Capability{"ALL"},
				Add:  []corev1.Capability{"DAC_OVERRIDE"},
			},
			SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
		// The shim writes its MoverResult JSON to TerminationMessagePath and the controller
		// reads it back off the terminated container status. ReadFile (the default) is pinned
		// explicitly: we must NOT fall back to container logs, because an EMPTY message is the
		// load-bearing signal that the container was killed before it could report, which
		// ParseMoverResult turns into a failure. FallbackToLogsOnError would replace that empty
		// message with unparseable log tail and mask the crash.
		TerminationMessagePath:   TerminationMessagePath,
		TerminationMessagePolicy: corev1.TerminationMessageReadFile,
	}

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      req.Name,
			Namespace: req.Namespace,
			// Independent copies so the Job's labels and the template's labels never alias one
			// another or the caller's map (a later edit of one must not mutate the others).
			Labels: copyLabels(req.Labels),
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            ptrTo(req.BackoffLimit),
			TTLSecondsAfterFinished: ptrTo(req.TTLSeconds),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: copyLabels(req.Labels)},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					// A mover talks to S3 and the source PVC only; it never calls the Kubernetes
					// API, so its SA token must not be mounted (least privilege, smaller blast radius).
					AutomountServiceAccountToken: ptrTo(false),
					SecurityContext: &corev1.PodSecurityContext{
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					TopologySpreadConstraints: spreadConstraints(req.SpreadOverLabels),
					Containers:                []corev1.Container{container},
					Volumes:                   volumes,
				},
			},
		},
	}
}

// spreadConstraints returns the soft topology-spread constraint selecting pods by labels, or nil
// when no labels are given. It is soft (ScheduleAnyway) on purpose: a mover must still schedule
// even onto a busy node when the cluster cannot honour the skew, because completing the backup
// matters more than spreading it.
func spreadConstraints(labels map[string]string) []corev1.TopologySpreadConstraint {
	if len(labels) == 0 {
		return nil
	}
	return []corev1.TopologySpreadConstraint{{
		MaxSkew:           1,
		TopologyKey:       corev1.LabelHostname,
		WhenUnsatisfiable: corev1.ScheduleAnyway,
		LabelSelector:     &metav1.LabelSelector{MatchLabels: copyLabels(labels)},
	}}
}

// moverEnv builds the container environment in a fixed order: the restic repo/password/
// cache/tmp variables, then the two AWS credentials by secretKeyRef, then the optional
// GOMEMLIMIT, then the caller's ExtraEnv. Order is stable so the produced Job is
// byte-reproducible across releases and in tests.
func moverEnv(req JobRequest) []corev1.EnvVar {
	env := []corev1.EnvVar{
		{Name: envRepository, Value: req.RepoURL},
		// restic reads the password from a FILE, never an env var or argv; the file is this
		// key projected into the read-only Secret mount at ResticPasswordFilePath.
		{Name: envPasswordFile, Value: ResticPasswordFilePath},
		{Name: envCacheDir, Value: CacheDir},
		{Name: envTMPDIR, Value: tmpDir},
		// AWS creds are injected by reference: the env var name the AWS SDK inside restic reads
		// is identical to the Secret data key it comes from, so one constant names both sides.
		awsCredEnv(SecretKeyAWSAccessKeyID, req.SecretName),
		awsCredEnv(SecretKeyAWSSecretAccessKey, req.SecretName),
	}
	if req.GoMemLimit != "" {
		env = append(env, corev1.EnvVar{Name: envGoMemLimit, Value: req.GoMemLimit})
	}
	return append(env, req.ExtraEnv...)
}

// awsCredEnv builds an env var whose value is pulled from a required key of the per-Job
// Secret. key is used for both the env var Name and the secretKeyRef Key because the AWS
// SDK's env var name and the Secret data key are the same string by design.
func awsCredEnv(key, secretName string) corev1.EnvVar {
	return corev1.EnvVar{
		Name: key,
		ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
				Key:                  key,
			},
		},
	}
}

// moverVolumes builds the volumes and their matching mounts. Every job gets the read-only
// Secret mount plus the two writable scratch emptyDirs (cache, tmp) that a read-only root
// filesystem requires. A data job (req.PVC != nil) additionally gets the source PVC mounted
// read-only at req.PVC.MountPath; a maintenance job gets no data volume at all.
func moverVolumes(req JobRequest) ([]corev1.Volume, []corev1.VolumeMount) {
	volumes := []corev1.Volume{
		{
			Name: volumeSecret,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{SecretName: req.SecretName},
			},
		},
		{Name: volumeCache, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: volumeTmp, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
	}
	mounts := []corev1.VolumeMount{
		// The Secret is read-only: the mover reads the password file and the AWS keys, never
		// writes them.
		{Name: volumeSecret, MountPath: SecretMountPath, ReadOnly: true},
		{Name: volumeCache, MountPath: CacheDir},
		{Name: volumeTmp, MountPath: tmpDir},
	}

	if req.PVC != nil {
		volumes = append(volumes, corev1.Volume{
			Name: volumeData,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: req.PVC.ClaimName,
					// Read-only at the volume source as well as the mount: a backup must never be
					// able to mutate the very data it is copying (defence in depth beyond the mount).
					ReadOnly: true,
				},
			},
		})
		mounts = append(mounts, corev1.VolumeMount{
			Name:      volumeData,
			MountPath: req.PVC.MountPath,
			ReadOnly:  true,
		})
	}
	return volumes, mounts
}

// copyLabels returns an independent copy of in. The Job's metadata labels and the pod
// template's labels are set from the same request field; handing each its own map prevents
// an in-place edit of one (or of the caller's original) from silently mutating the others.
// A nil or empty map yields nil so the serialized object carries no empty `labels: {}`.
func copyLabels(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	maps.Copy(out, in)
	return out
}
