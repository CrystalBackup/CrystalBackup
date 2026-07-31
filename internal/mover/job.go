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

	"github.com/CrystalBackup/CrystalBackup/internal/tracing"
)

// Internal names for the mover container and its volumes. They are stable identifiers, not
// a wire contract with any other system, so they are unexported; the controller addresses
// the result via ContainerStatuses[0] (there is exactly one container), never by name.
const (
	containerName = "mover"

	volumeSecret    = "mover-secret" // the per-Job Secret: restic password + AWS creds
	volumeCache     = "restic-cache" // restic cache emptyDir (writable)
	volumeTmp       = "tmp"          // /tmp emptyDir (writable)
	volumeManifests = "manifests"    // the manifest dump scratch emptyDir (manifest jobs only)
	volumeData      = "data-source"  // the source PVC (data jobs only), mounted read-only
)

// tmpDir is both the TMPDIR env value and the mount path of the tmp emptyDir. restic and
// its helpers write temp files here; with a read-only root filesystem it must be a real,
// writable mount. Unexported because it is not part of the operator↔mover path contract
// the way SecretMountPath / CacheDir are.
const tmpDir = "/tmp"

// Capability names the security contexts below add/drop; named once (they recur across the
// per-operation sets).
const (
	capAll         corev1.Capability = "ALL"
	capChown       corev1.Capability = "CHOWN"
	capDACOverride corev1.Capability = "DAC_OVERRIDE"
	capFowner      corev1.Capability = "FOWNER"
	capMknod       corev1.Capability = "MKNOD"
	capSetfcap     corev1.Capability = "SETFCAP"
)

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

// PVCMount identifies the data volume of a DATA job. Its presence on a JobRequest is what
// makes a Job a data job rather than a maintenance job.
type PVCMount struct {
	// ClaimName is the PersistentVolumeClaim to snapshot (backup) or fill (restore).
	ClaimName string
	// MountPath is where the claim is mounted inside the mover. A backup caller sets it to
	// the restic backup path (e.g. /data/<ns>/<pvc>) so the snapshot's stored path equals
	// its restic identity (see internal/restic.Identity.Path); a restore caller sets it to
	// the neutral restore root its --target points into. BuildJob does NOT invent its own
	// mount path; it mounts exactly here.
	MountPath string
	// ReadWrite mounts the claim writable. Only OpRestore sets it — it fills the target
	// PVC — and it defaults to false so every other data job keeps the backup guarantee
	// that a mover can never mutate the data it is copying (read-only at both the volume
	// source and the mount).
	ReadWrite bool
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
	// TTLSeconds == NoTTL leaves ttlSecondsAfterFinished UNSET: the Job persists until its
	// owner deletes it (used when the Job itself is the caller's durable state).
	BackoffLimit int32
	TTLSeconds   int32
	// GoMemLimit, when non-empty, sets GOMEMLIMIT to cap the mover's Go heap; skipped when
	// empty (an empty GOMEMLIMIT is invalid and would abort the runtime).
	GoMemLimit string
	// ExtraEnv is appended AFTER the fixed env (e.g. AWS_DEFAULT_REGION, RESTIC_COMPRESSION),
	// so a caller can add knobs without displacing the protocol variables.
	ExtraEnv []corev1.EnvVar
	// TraceEnv is the OpenTelemetry handover: the W3C TRACEPARENT/TRACESTATE of the span this
	// Job's work belongs to, plus the OTLP settings the shim needs to export to the same
	// collector (internal/tracing.JobEnv). Emitted in sorted key order, so the Job spec stays
	// byte-reproducible.
	//
	// EMPTY IS THE NORMAL CASE and it must produce a Job spec identical to the one this builder
	// produced before tracing existed: an install with no collector has no business carrying two
	// blank variables into every mover pod. It sits BEFORE ExtraEnv so an operator who sets one
	// of these through their own chart values overrides it — a later duplicate is the one the
	// kubelet keeps.
	TraceEnv map[string]string
	// CredentialKeys are the per-Job Secret keys projected as environment variables of the SAME
	// name (one constant naming both sides, as SecretKeyAWS* already does). Empty means the
	// default S3 pair — AWS_ACCESS_KEY_ID + AWS_SECRET_ACCESS_KEY — which is what every
	// restic-over-s3 operation needs, so every existing caller keeps its behaviour unchanged.
	//
	// Only the sync Job overrides it (mover.SyncCredentialKeys): its two repositories are
	// addressed through rclone remotes, whose credentials arrive as RCLONE_CONFIG_<REMOTE>_*.
	// Overriding rather than adding is deliberate — these are REQUIRED secretKeyRefs, so leaving
	// the AWS pair in place would keep the sync pod from ever starting on a Secret that has no
	// reason to carry those keys, and carrying them anyway would advertise a credential path
	// nothing beneath restic reads once the repository URL says rclone:.
	CredentialKeys []string
	// FromPasswordFile mounts the SOURCE repository's password for a two-repository operation:
	// it sets RESTIC_FROM_PASSWORD_FILE, and the file itself is the SecretKeyResticFromPassword
	// key of the same per-Job Secret, projected by the mount that is already there.
	//
	// A bool rather than a path: the path is fixed by the Secret projection, so a caller that
	// could choose it could only choose it wrong.
	FromPasswordFile bool
	// Profiles is the operator's resolved sizing table (built-in defaults with the platform's
	// overrides folded in). BuildJob picks THIS request's Operation out of it for the container's
	// requests/limits and for the restic cache emptyDir's sizeLimit.
	//
	// The table rather than one operation's numbers, and nil rather than a required field, both
	// on purpose: a caller cannot hand a prune Job a backup's limits by picking the wrong row,
	// and a nil table (every unit test, envtest, a controller not yet wired to the operator's
	// table) still produces a SIZED pod from the built-in defaults instead of a BestEffort one.
	// Purity is unaffected — the table is an input like any other field.
	Profiles Profiles
	// SpreadOverLabels, when non-empty, adds a SOFT topology-spread constraint (maxSkew 1 over
	// kubernetes.io/hostname, whenUnsatisfiable=ScheduleAnyway) selecting pods by these labels, so a
	// wide fan-out's movers prefer distinct nodes instead of piling onto one. Empty ⇒ no constraint.
	SpreadOverLabels map[string]string
	// NodeName, when non-empty, pins the pod to that node (PodSpec.nodeName, bypassing the
	// scheduler). Only the restore twin-PV path sets it — an RWO volume attached on exactly
	// one node must be mounted from that same node (adr/0016 §2); empty everywhere else.
	NodeName string
	// ServiceAccountName, when non-empty, runs the pod under that ServiceAccount AND
	// automounts its token. Empty — the default for every data and maintenance job — keeps
	// the zero-API posture of I6: the namespace default SA with its token not even mounted.
	//
	// Only the manifest mover sets this (crystal-manifest-mover), because it is the one
	// operation that must read the API server. Making it opt-in rather than a parameter with
	// a default means a new job shape cannot acquire API access by forgetting a field.
	ServiceAccountName string
	// ManifestsVolume adds the writable emptyDir the manifest dump writes into. Only the manifest
	// operations set it; the tree it holds is small, so an unsized emptyDir is proportionate.
	ManifestsVolume bool
	// ManifestsMountPath is where that emptyDir is mounted. Empty defaults to ManifestsRoot, so
	// every existing caller keeps its behaviour; the cluster-manifests capture sets it to
	// ClusterManifestsRoot, because its dump writes there and the root filesystem is read-only.
	// The mount path MUST equal the directory the dump writes to, or the dump fails on a
	// read-only filesystem.
	ManifestsMountPath string
}

// BuildJob assembles the batchv1.Job for one mover run, exactly per the package's runtime
// contract. It is pure: same request in, byte-identical Job out. A nil req.PVC yields a
// maintenance job (no data volume); a set req.PVC yields a data job that additionally
// mounts the source claim read-only at req.PVC.MountPath. No ServiceAccountName is set, so
// the Job uses the namespace default SA — and its token is not even mounted (see below).
func BuildJob(req JobRequest) *batchv1.Job {
	// One lookup, used twice: the container's requests/limits and the cache volume's sizeLimit
	// come from the same row, so a Job can never be sized as a prune and capped as a backup.
	profile := req.Profiles.For(req.Operation)
	volumes, mounts := moverVolumes(req, profile)

	container := corev1.Container{
		Name:    containerName,
		Image:   req.Image,
		Command: []string{MoverBinaryPath},
		// Everything after "--" is restic's own argv, forwarded verbatim by the shim. The
		// prefix literal has len == cap, so append allocates fresh and never aliases ResticArgs.
		Args:         append([]string{operationFlag, string(req.Operation), "--"}, req.ResticArgs...),
		Env:          moverEnv(req),
		VolumeMounts: mounts,
		Resources:    corev1.ResourceRequirements{Requests: profile.Requests, Limits: profile.Limits},
		// root + a per-operation capability set (see moverCapabilities), while everything
		// else is stripped: no privilege escalation, a read-only root filesystem (only the
		// cache and tmp emptyDirs are writable), all other capabilities dropped, default
		// seccomp.
		SecurityContext: &corev1.SecurityContext{
			RunAsUser:                ptrTo[int64](0),
			RunAsGroup:               ptrTo[int64](0),
			AllowPrivilegeEscalation: ptrTo(false),
			ReadOnlyRootFilesystem:   ptrTo(true),
			Capabilities: &corev1.Capabilities{
				Drop: []corev1.Capability{capAll},
				Add:  moverCapabilities(req),
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
			TTLSecondsAfterFinished: ttlSeconds(req.TTLSeconds),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: copyLabels(req.Labels)},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					// A data or maintenance mover talks to S3 and the source PVC only; it never
					// calls the Kubernetes API, so its SA token must not be mounted (least
					// privilege, smaller blast radius). The single exception is the manifest
					// mover, whose whole job IS reading the API server (I6): it names a
					// ServiceAccount, and the token follows the account rather than being a
					// separate knob that could be set without one.
					ServiceAccountName:           req.ServiceAccountName,
					AutomountServiceAccountToken: ptrTo(req.ServiceAccountName != ""),
					SecurityContext: &corev1.PodSecurityContext{
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					TopologySpreadConstraints: spreadConstraints(req.SpreadOverLabels),
					// Empty for every job except a same-node-pinned restore (JobRequest.NodeName).
					NodeName:   req.NodeName,
					Containers: []corev1.Container{container},
					Volumes:    volumes,
				},
			},
		},
	}
}

// moverCapabilities returns the capability set added on top of Drop:ALL for one Job, keyed on what
// that Job can actually touch rather than on its operation name.
//
// DAC_OVERRIDE exists for ONE reason: a data job walks a tenant's files, owned by arbitrary uids
// the mover is not. That is true exactly when a data volume is mounted (req.PVC != nil). A
// MAINTENANCE job — init, forget, prune, check, snapshots, unlock, and the manifest shapes — mounts
// no PVC at all: it touches its own root-owned emptyDirs (restic cache, /tmp, the manifest scratch)
// and a read-only Secret, all of which uid 0 already reads and writes without overriding a single
// permission check. Granting it there was the catch-all's doing, not a requirement (M3.2; the case
// that made it visible was OpSnapshots, discovery's listing Job, which mounts `PVC: nil`).
//
// A RESTORE additionally re-creates metadata (R10): CHOWN (arbitrary uid/gid ownership), FOWNER
// (mode/timestamp changes on files it now owns as root-with-DAC), SETFCAP (security.capability
// xattrs) and MKNOD (device nodes). All five are in the container runtime's default set, so PSA
// `baseline` on the operator namespace still admits the pod (03-security-and-tenancy.md §6);
// trusted.* xattrs would need CAP_SYS_ADMIN and are documented as NOT restored. The list order is
// pinned for byte-reproducible Job specs.
func moverCapabilities(req JobRequest) []corev1.Capability {
	switch {
	case req.Operation == OpRestore:
		return []corev1.Capability{capChown, capDACOverride, capFowner, capMknod, capSetfcap}
	case req.PVC != nil:
		return []corev1.Capability{capDACOverride}
	default:
		return nil // maintenance shape: no data volume, nothing to override
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
// cache/tmp variables, the optional source-repository password, then the backend credentials
// by secretKeyRef, then the optional GOMEMLIMIT, then the caller's ExtraEnv. Order is stable
// so the produced Job is byte-reproducible across releases and in tests.
func moverEnv(req JobRequest) []corev1.EnvVar {
	env := []corev1.EnvVar{
		{Name: envRepository, Value: req.RepoURL},
		// restic reads the password from a FILE, never an env var or argv; the file is this
		// key projected into the read-only Secret mount at ResticPasswordFilePath.
		{Name: envPasswordFile, Value: ResticPasswordFilePath},
		{Name: envCacheDir, Value: CacheDir},
		{Name: envTMPDIR, Value: tmpDir},
	}
	// The SOURCE repository's password, for the one operation that opens two repositories. Placed
	// before the credentials so the two password variables read together.
	if req.FromPasswordFile {
		env = append(env, corev1.EnvVar{Name: EnvFromPasswordFile, Value: ResticFromPasswordFilePath})
	}
	// Backend credentials are injected by reference: the env var name the backend reads is
	// identical to the Secret data key it comes from, so one constant names both sides.
	for _, key := range credentialKeys(req) {
		env = append(env, secretEnv(key, req.SecretName))
	}
	if req.GoMemLimit != "" {
		env = append(env, corev1.EnvVar{Name: envGoMemLimit, Value: req.GoMemLimit})
	}
	// The trace handover, in sorted key order (a Go map's range order is not one). Nil when
	// tracing is inactive, which is when this whole block adds nothing at all.
	for _, name := range tracing.SortedEnvKeys(req.TraceEnv) {
		env = append(env, corev1.EnvVar{Name: name, Value: req.TraceEnv[name]})
	}
	return append(env, req.ExtraEnv...)
}

// credentialKeys resolves JobRequest.CredentialKeys, defaulting to the S3 pair. The default
// lives here rather than at each call site so an operation that says nothing about credentials
// gets the ones restic-over-s3 actually needs.
func credentialKeys(req JobRequest) []string {
	if len(req.CredentialKeys) > 0 {
		return req.CredentialKeys
	}
	return []string{SecretKeyAWSAccessKeyID, SecretKeyAWSSecretAccessKey}
}

// secretEnv builds an env var whose value is pulled from a required key of the per-Job
// Secret. key is used for both the env var Name and the secretKeyRef Key because the backend's
// env var name and the Secret data key are the same string by design — true of the AWS SDK
// inside restic (AWS_ACCESS_KEY_ID) and of rclone's per-remote form alike.
//
// The reference is REQUIRED (no Optional): a missing key must keep the pod from starting, not
// let restic run against a repository it will fail to authenticate to and report as unreachable.
func secretEnv(key, secretName string) corev1.EnvVar {
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
// filesystem requires. A data job (req.PVC != nil) additionally gets the PVC mounted at
// req.PVC.MountPath — read-only unless the caller set PVCMount.ReadWrite (restore); a
// maintenance job gets no data volume at all.
//
// The restic CACHE emptyDir carries the profile's sizeLimit (nil ⇒ unbounded, the pre-0.6.1
// behaviour). /tmp and the manifest scratch stay unbounded — see defaultCacheSizeLimit for why
// bounding the cache is worth an eviction risk and bounding the other two is not.
func moverVolumes(req JobRequest, profile Profile) ([]corev1.Volume, []corev1.VolumeMount) {
	volumes := []corev1.Volume{
		{
			Name: volumeSecret,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{SecretName: req.SecretName},
			},
		},
		{Name: volumeCache, VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: profile.CacheSizeLimit},
		}},
		{Name: volumeTmp, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
	}
	mounts := []corev1.VolumeMount{
		// The Secret is read-only: the mover reads the password file and the AWS keys, never
		// writes them.
		{Name: volumeSecret, MountPath: SecretMountPath, ReadOnly: true},
		{Name: volumeCache, MountPath: CacheDir},
		{Name: volumeTmp, MountPath: tmpDir},
	}

	if req.ManifestsVolume {
		// Writable scratch the manifest dump writes its tree into, at exactly the path restic
		// will be asked to back up. Separate from the restic cache so the two cannot crowd each
		// other out of one emptyDir's budget. The mount path defaults to ManifestsRoot and is
		// ClusterManifestsRoot for a cluster-scoped capture.
		mountPath := req.ManifestsMountPath
		if mountPath == "" {
			mountPath = ManifestsRoot
		}
		volumes = append(volumes, corev1.Volume{
			Name:         volumeManifests,
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		})
		mounts = append(mounts, corev1.VolumeMount{Name: volumeManifests, MountPath: mountPath})
	}

	if req.PVC != nil {
		// Read-only at the volume source as well as the mount unless this is a restore
		// (PVCMount.ReadWrite): a backup must never be able to mutate the very data it is
		// copying (defence in depth beyond the mount), while a restore's entire job is to
		// write the target.
		readOnly := !req.PVC.ReadWrite
		volumes = append(volumes, corev1.Volume{
			Name: volumeData,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: req.PVC.ClaimName,
					ReadOnly:  readOnly,
				},
			},
		})
		mounts = append(mounts, corev1.VolumeMount{
			Name:      volumeData,
			MountPath: req.PVC.MountPath,
			ReadOnly:  readOnly,
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
