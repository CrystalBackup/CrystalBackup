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
