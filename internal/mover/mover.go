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

// Package mover is the single source of truth for CrystalBackup's mover contract: how
// the operator packages exactly ONE restic operation as a short-lived Job, and how that
// Job reports its outcome back through the pod's termination message. Every controller's
// data path — repository init, per-PVC backup, retention forget, prune and check — flows
// through BuildJob on the way out and ParseMoverResult on the way back, so the strings
// and shapes pinned here ARE the operator↔mover wire protocol, not an implementation
// detail either side may drift.
//
// One image, two shapes. A mover is the crystal-mover binary (MoverBinaryPath) run from
// the CrystalBackup image as `crystal-mover --operation <op> -- <restic argv>`. A
// MAINTENANCE job (OpInit/OpForget/OpPrune/OpCheck/OpSnapshots/OpUnlock) mounts only the
// repository credentials and scratch; a DATA job additionally mounts a PVC — OpBackup
// mounts the source read-only at the very path restic will store the snapshot under (so
// the snapshot's on-disk path equals its restic identity, see internal/restic), OpRestore
// mounts the target read-write at a neutral path restic restores into
// (PVCMount.ReadWrite). BuildJob assembles all shapes from one JobRequest, differing only
// by whether JobRequest.PVC is set and how.
//
// Secrets never touch argv. restic learns its repository, password and S3 credentials
// entirely from the environment and a mounted Secret: RESTIC_REPOSITORY, a
// RESTIC_PASSWORD_FILE pointing into the read-only Secret mount, and AWS_* pulled via
// secretKeyRef. The operator passes NO secret on the command line, where it would leak
// into `kubectl get pod -o yaml`, the Job spec, and in-cluster process listings. Only the
// restic subcommand and its non-secret flags ever appear on argv.
//
// The result protocol. Kubernetes surfaces a terminated container's termination message
// (the last bytes it wrote to TerminationMessagePath, capped by the kubelet at 4096
// bytes — a MoverResult is far smaller) on
// pod.Status.ContainerStatuses[0].State.Terminated.Message. The shim marshals a tiny
// MoverResult JSON there on exit; the controller ParseMoverResult()s it. A BLANK message
// is therefore NOT "success with no detail" but "the container died before it could
// write" — an OOMKill, a SIGKILL, a node eviction — which ParseMoverResult turns into a
// clear error so a controller can never mistake a crash for a clean run.
//
// Purity. Nothing here performs I/O, imports controller-runtime, or holds a client:
// BuildJob is a deterministic function of its request and the parsers are pure. It
// depends only on the stdlib and the k8s API types, so the shim (which produces the
// termination message and parses restic's stdout) and every controller (which builds the
// Job and reads the result) share this one definition without a cycle.
package mover

// Operation is the mover's single verb, passed as `--operation <op>`. It tells the shim
// which restic operation this Job runs and is echoed back in MoverResult.Operation. The
// value doubles as the leading restic subcommand for every operation except forget/prune
// nuances, but the shim (not this package) owns argv beyond the "--" separator, so the
// canonical coupling here is only Operation -> the --operation flag.
type Operation string

// The operations a mover can run. OpBackup is the sole DATA operation (it reads a PVC); the
// others are MAINTENANCE operations against the repository alone. Splitting them as named
// constants (rather than free strings at call sites) keeps the --operation value and the
// MoverResult.Operation echo from ever disagreeing.
//
// OpSnapshots is the one operation whose PAYLOAD is its stdout, not its MoverResult: discovery
// reads the `restic snapshots --json` array back off the pod log (the maintenance stdout wiring
// tees stdout to the pod log), because a snapshot inventory is far larger than the 4096-byte
// termination message. Its MoverResult still reports OK/failure like any maintenance op.
const (
	// OpBackup snapshots one PVC into the repository; mounts the source PVC read-only.
	OpBackup Operation = "backup"
	// OpRestore writes one snapshot subtree back into a PVC (`restic restore`); the only
	// shape that mounts data READ-WRITE (PVCMount.ReadWrite). Against the repository it is
	// a reader like OpBackup (non-exclusive lock), so it is never enqueued on the per-repo
	// exclusive queue but counts in the mover-quiescence census (adr/0015, adr/0016).
	OpRestore Operation = "restore"
	// OpInit creates the restic repository if it does not yet exist (idempotent).
	OpInit Operation = "init"
	// OpForget applies a retention policy, dropping snapshots outside the keep window.
	OpForget Operation = "forget"
	// OpPrune reclaims repository space no snapshot references any more.
	OpPrune Operation = "prune"
	// OpCheck verifies repository structural integrity.
	OpCheck Operation = "check"
	// OpSnapshots lists the repository's snapshots (`restic snapshots --json`); discovery reads
	// the JSON off the pod log. Maintenance shape (no PVC).
	OpSnapshots Operation = "snapshots"
	// OpUnlock removes stale repository locks (`restic unlock`) so an OOM-killed mover's lock
	// cannot wedge the next run. Maintenance shape (no PVC).
	OpUnlock Operation = "unlock"
	// OpManifestsBackup dumps a namespace's Kubernetes resources and backs the tree up (R15,
	// spec/04-manifest-backup.md §2.3). It is the ONLY operation that talks to the API server:
	// the dump must not go through exec/stdout (delta 8), so the mover reads the objects and
	// writes them to the repository itself. That makes it the sole exception to the zero-API
	// mover invariant (I6) — it runs under crystal-manifest-mover with an automounted token
	// and a transient RoleBinding, never under the zero-RBAC crystal-mover.
	OpManifestsBackup Operation = "manifests-backup"
	// OpManifestsRestore restores a manifest snapshot and APPLIES the tree to a target
	// namespace (spec/04-manifest-backup.md §5). The mirror of OpManifestsBackup, and the
	// second half of I6's sole exception: it runs under crystal-manifest-mover with a
	// transient RoleBinding to crystal-manifest-writer — create/update/delete on arbitrary
	// kinds in ONE namespace for ONE Job's lifetime, the largest grant in the system.
	//
	// It is the mirror in a second way that matters to the shim: the API work runs AFTER
	// restic here, not before. restic writes the tree, then the shim applies it.
	OpManifestsRestore Operation = "manifests-restore"
	// OpClusterManifestsBackup captures the cluster's CLUSTER-SCOPED objects (CRDs,
	// StorageClasses, PersistentVolumes, ClusterRoles/Bindings, IngressClasses, …) and backs the
	// tree up as ONE kind=cluster-manifests snapshot (adr/0011 §1). It is the cluster-plane
	// sibling of OpManifestsBackup: the same "dump then restic backup" shape, and the same sole
	// exception to the zero-API mover invariant (I6) — it runs under a privileged-read
	// ServiceAccount transiently bound to ClusterRole crystal-cluster-manifest-reader, never
	// under the zero-RBAC crystal-mover. Unlike the namespaced dump it belongs to no namespace,
	// so its snapshot carries NEITHER tenant nor namespace tag (restic.ClusterManifestsIdentity),
	// and its dump destination is the fixed ClusterManifestsRoot with no per-namespace suffix.
	OpClusterManifestsBackup Operation = "cluster-manifests-backup"
	// OpClusterManifestsRestore restores a kind=cluster-manifests snapshot and APPLIES its
	// cluster-scoped objects (adr/0011 §2). The mirror of OpManifestsRestore on the cluster
	// plane: same "restic restore then apply" shape (the API work runs AFTER restic), but the
	// objects are cluster-scoped — no target namespace is stamped, and the transient grant is a
	// cluster-scoped ClusterRoleBinding to crystal-cluster-manifest-writer, the second half of
	// I6's exception on the restore side. Admin-only, opt-in, and confirmation-gated at the CR
	// level; the mover only executes what the operator resolved.
	OpClusterManifestsRestore Operation = "cluster-manifests-restore"
)

// Filesystem layout inside the mover container. These paths are a two-sided contract: the
// Job spec (BuildJob) mounts volumes and sets env to them, and the shim + restic read
// from them. They are pinned here so the producer and the consumer can never disagree on
// where the password lives or where the cache goes.
const (
	// MoverBinaryPath is the shim entrypoint, baked into the CrystalBackup image. It is the
	// container command BuildJob pins (args carry --operation and the restic argv), so it MUST
	// equal where the image actually installs the binary: build/melange/mover.yaml copies
	// crystal-mover to /usr/bin/crystal-mover and build/apko/mover.yaml's entrypoint is the same
	// path (alongside restic at /usr/bin/restic). A mismatch here fails every mover Job with
	// "no such file", so this constant and those two build files are one contract.
	MoverBinaryPath = "/usr/bin/crystal-mover"

	// SecretMountPath is where the per-Job Secret (restic password + AWS creds) is mounted
	// READ-ONLY. Each Secret data key becomes a file of the same name under this directory.
	SecretMountPath = "/crystal/secret"

	// ResticPasswordFileName is the Secret data key holding the repository password and,
	// because a mounted Secret projects each key to a same-named file, also the file's
	// basename under SecretMountPath. RESTIC_PASSWORD_FILE points at the resulting file.
	ResticPasswordFileName = "restic-password"

	// ResticPasswordFilePath is the absolute path RESTIC_PASSWORD_FILE is set to — the
	// projection of ResticPasswordFileName under SecretMountPath. restic reads the password
	// from this file so it never appears in the environment listing or on argv.
	ResticPasswordFilePath = SecretMountPath + "/" + ResticPasswordFileName

	// CacheDir is restic's cache directory (RESTIC_CACHE_DIR), backed by an emptyDir. It is
	// one of the only two writable paths in the container (the root filesystem is
	// read-only); the other is /tmp.
	CacheDir = "/crystal/cache"

	// TerminationMessagePath is where the shim writes its MoverResult JSON on exit and where
	// the kubelet reads the container's termination message from. Exported so the shim and
	// the Job spec name the exact same path (BuildJob pins it on the container).
	TerminationMessagePath = "/dev/termination-log"

	// ManifestsRoot is where the manifest emptyDir is mounted, and it is chosen to EQUAL the
	// restic path prefix of restic.ManifestsIdentity ("/manifests/<namespace>"). restic
	// records the absolute path it was given, so the dump destination and the snapshot's
	// stored path are the same string by construction — exactly as a data job mounts its PVC
	// at /data/<ns>/<pvc> rather than somewhere neutral. Writing the dump anywhere else would
	// silently store the snapshot under the wrong path and break every retention group and
	// restore that keys on it.
	ManifestsRoot = "/manifests"

	// ManifestsRestoreDir is where the restic half of a manifest restore lands the tree, and
	// therefore where the apply reads index.json from. A SUBDIRECTORY of ManifestsRoot rather
	// than the root itself: on backup that root is the dump's parent, and reusing it here would
	// make one path mean "tree being written out" in one operation and "tree being read in" in
	// the other. They never run in the same pod, so nothing would break — but a future reader
	// deciding whether it is safe to clear the directory deserves an unambiguous answer.
	ManifestsRestoreDir = ManifestsRoot + "/restore"

	// ClusterManifestsRoot is where the cluster-manifests emptyDir is mounted, and it is chosen
	// to EQUAL the restic path of restic.ClusterManifestsIdentity ("/cluster-manifests"). As with
	// ManifestsRoot, restic records the absolute path it is given, so the dump destination and the
	// snapshot's stored path are the same string by construction. Unlike ManifestsRoot there is NO
	// per-namespace suffix: one run writes one cluster-manifests snapshot at this fixed path
	// (adr/0011). Writing the dump anywhere else would silently store the snapshot under the wrong
	// path and break every retention group and ClusterRestore that keys on it.
	ClusterManifestsRoot = "/cluster-manifests"

	// ClusterManifestsRestoreDir is where the restic half of a cluster-manifests RESTORE lands the
	// tree, and where the apply reads index.json from. A SUBDIRECTORY of ClusterManifestsRoot for
	// the same reason ManifestsRestoreDir is one of ManifestsRoot: keep "tree written out" and
	// "tree read in" as distinct paths so a reader can reason about each unambiguously.
	ClusterManifestsRestoreDir = ClusterManifestsRoot + "/restore"
)

// Environment the manifest mover reads. The mover has no config file and no flags beyond
// --operation; everything else arrives as env, so these are the contract between the Job
// builder and the shim.
const (
	// EnvManifestsNamespace is the namespace to dump. The destination directory is
	// ManifestsRoot + "/" + this, which is what makes it agree with the restic identity.
	EnvManifestsNamespace = "CRYSTAL_MANIFESTS_NAMESPACE"
	// EnvManifestsClusterID is recorded in index.json.
	EnvManifestsClusterID = "CRYSTAL_MANIFESTS_CLUSTER_ID"
	// EnvManifestsBackupName is the run name (the `run` tag), recorded in index.json.
	EnvManifestsBackupName = "CRYSTAL_MANIFESTS_BACKUP_NAME"
	// EnvManifestsExcludeSecretData is "true" when manifestOptions.excludeSecretData is set.
	EnvManifestsExcludeSecretData = "CRYSTAL_MANIFESTS_EXCLUDE_SECRET_DATA"

	// EnvManifestsRestoreDir is where restic restored the manifest tree, and therefore where
	// the apply reads index.json from. Passed explicitly rather than re-derived: the RESTORE
	// target has no reason to equal the captured path — a ClusterRestore may send another
	// cluster's namespace into a differently-named one — so the one thing the shim must not do
	// is guess it.
	EnvManifestsRestoreDir = "CRYSTAL_MANIFESTS_RESTORE_DIR"
	// EnvManifestsMode is the Restore's spec.mode ("Overwrite" or "Recreate").
	EnvManifestsMode = "CRYSTAL_MANIFESTS_MODE"
	// EnvManifestsDryRun is "true" for a spec.dryRun restore: the full pipeline runs against
	// the API server with dryRun=All and nothing is persisted.
	EnvManifestsDryRun = "CRYSTAL_MANIFESTS_DRY_RUN"
	// EnvManifestsSelection is the JSON-encoded manifests.Selection resolved from the
	// Restore's resources[]. It travels as env rather than argv because everything after the
	// shim's "--" separator belongs to restic verbatim. An UNSET variable means "restore
	// everything", which is what an operator with no concept of narrowing meant.
	EnvManifestsSelection = "CRYSTAL_MANIFESTS_SELECTION"
	// EnvManifestsStorageClassMapping is the JSON-encoded map[string]string of
	// ClusterRestore.spec.target.storageClassMapping, applied to PVC manifests (§5.3).
	EnvManifestsStorageClassMapping = "CRYSTAL_MANIFESTS_STORAGE_CLASS_MAPPING"
)

// Environment the CLUSTER-scoped manifest mover reads (OpClusterManifestsBackup). The
// cluster-plane sibling of the EnvManifests* block: it carries NO namespace (the capture belongs
// to no namespace) and NO excludeSecretData (cluster-scoped kinds hold no Secret data), but the
// same clusterID + run name, plus the JSON-encoded cluster selection.
const (
	// EnvClusterManifestsClusterID is the resolved clusterID, recorded in index.json — the same
	// value the snapshot's restic --host carries.
	EnvClusterManifestsClusterID = "CRYSTAL_CLUSTER_MANIFESTS_CLUSTER_ID"
	// EnvClusterManifestsBackupName is the run name (the `run` tag), recorded in index.json.
	EnvClusterManifestsBackupName = "CRYSTAL_CLUSTER_MANIFESTS_BACKUP_NAME"
	// EnvClusterManifestsSelection is the JSON-encoded manifests.ClusterCaptureOptions
	// (include/exclude) resolved from ClusterBackup.spec.clusterResources
	// (manifests.EncodeClusterCaptureOptions). It travels as env rather than argv because
	// everything after the shim's "--" separator belongs to restic verbatim. An UNSET or empty
	// value means the curated default allow-list (adr/0011 §1) — an empty include is NOT "capture
	// everything cluster-scoped", it is "capture the DR-relevant defaults", which is what capture
	// being ON by default means.
	EnvClusterManifestsSelection = "CRYSTAL_CLUSTER_MANIFESTS_SELECTION"
)

// Secret data keys the per-Job Secret must carry. The restic password is consumed as a
// FILE (mounted, RESTIC_PASSWORD_FILE); the two AWS keys are consumed as ENV via
// secretKeyRef. All three live in one Secret named per Job (JobRequest.SecretName).
const (
	// SecretKeyResticPassword is the repository-password key. It equals ResticPasswordFileName
	// on purpose: the mounted Secret projects this key to a file of the same name, and
	// RESTIC_PASSWORD_FILE points at that file — coupling them here makes that projection
	// impossible to break by editing one and not the other.
	SecretKeyResticPassword = ResticPasswordFileName

	// SecretKeyAWSAccessKeyID and SecretKeyAWSSecretAccessKey are the S3 credential keys.
	// Their names are exactly the environment variable names the AWS SDK embedded in restic
	// reads, so moverEnv reuses these same constants as the env var Name and the secretKeyRef
	// Key — the two sides of each AWS credential are named by one constant.
	SecretKeyAWSAccessKeyID     = "AWS_ACCESS_KEY_ID"
	SecretKeyAWSSecretAccessKey = "AWS_SECRET_ACCESS_KEY"
)

// ptrTo returns a pointer to v. The k8s Job/Pod specs express optional scalars
// (backoffLimit, ttlSecondsAfterFinished, runAsUser, the *bool toggles) as pointers to
// distinguish "unset" from a zero value; this is the local, dependency-free equivalent of
// k8s.io/utils/ptr.To, which this package deliberately does not import.
func ptrTo[T any](v T) *T { return &v }

// NoTTL, as JobRequest.TTLSeconds, leaves the Job's ttlSecondsAfterFinished unset — the Job
// persists until its owner deletes it (used when the Job itself is the caller's durable state).
const NoTTL int32 = -1

// ttlSeconds maps JobRequest.TTLSeconds to the Job field: nil for NoTTL, a pointer otherwise.
func ttlSeconds(v int32) *int32 {
	if v == NoTTL {
		return nil
	}
	return ptrTo(v)
}
