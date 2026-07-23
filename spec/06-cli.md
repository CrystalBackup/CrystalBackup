# CLI — `crystalctl`

Status: draft v2 (two-plane cascade + de-brand); the whole `crystalctl` binary ships in **M7**
(reprioritized with the UI — the repo being standard restic already guarantees R8 reversibility,
so the CLI is a convenience: [90-roadmap.md](90-roadmap.md)). Naming contract: [02-api.md](02-api.md).
Engine choice: [adr/0001](adr/0001-repository-engine-restic-format.md). Model rationale:
[adr/0009](adr/0009-shared-cluster-repo-tag-tenancy.md).

## 1. Purpose and scope

`crystalctl` is a single static Go binary (`cmd/crystalctl`) with three modes:

1. **Standalone mode** (R8) — the `repo` subtree. Requires **no Kubernetes**: only S3
   credentials and a repository key. It is a thin wrapper over the restic-format engine,
   usable from a laptop against any repository the caller holds a key for — a **namespace
   user** against their own `BackupLocation` repo, or a **platform administrator** against
   the shared cluster repo. Ships in **M7**.
2. **Cluster mode** — the `backup`, `restore` and `admin` subtrees. Requires a
   kubeconfig; these commands are **conveniences that create or read the CRs defined in
   [02-api.md](02-api.md)** — nothing they do is impossible with `kubectl`. Every
   CR-creating command supports `--dry-run` to print the CR YAML instead of applying it.
3. **Hybrid mode** — the `ui` subtree only (§4.4). Accepts either a standalone repo
   profile **or** a kubeconfig to resolve repositories, and touches S3 directly like
   `repo` mode.

Mode is determined by the subcommand, never by autodetection: `repo` never loads a
kubeconfig; `backup`/`restore`/`admin` never touch S3 directly; `ui` is the only
subcommand allowed to combine both credential sources.

## 2. Standalone mode (`crystalctl repo …`)

### 2.1 Commands

```
crystalctl repo snapshots [--pvc <name>] [--namespace <ns>] [--run <name>] [--schedule <name>] [--kind data|manifests]
crystalctl repo ls <snapshotID|latest> [path]
crystalctl repo dump <snapshotID|latest> <path>                # raw bytes on stdout
crystalctl repo export (--tar|--zip) <snapshotID|latest> [path] -o <file>
crystalctl repo restore <snapshotID|latest> --target <dir> [--include <glob>]... [--sparse]
crystalctl repo stats
```

- `snapshots` lists snapshots with the tags written by the mover (`crystalbackup`,
  `tenant=`, `namespace=`, `pvc=`, `kind=data|manifests`, `schedule=`, `run=`; the snapshot
  **host** is the `clusterID` — see
  [02-api.md § Repository layout](02-api.md#repository-layout--snapshot-identity)); the
  filter flags are sugar over restic tag filters (`--namespace` here is the `namespace=`
  **tag**, not a kubeconfig namespace — `repo` never loads a kubeconfig).
- `dump` streams a single file to stdout, reading only the blobs required (suitable for
  piping); diagnostics go to stderr.
- `export` streams a whole snapshot or sub-path as a tar or zip archive without
  materializing it locally; `-o -` writes the archive to stdout.
- `restore` performs a full or partial (R7) restore to a local directory, preserving
  uid/gid/mode, xattrs, ACLs, hardlinks and (with `--sparse`) sparse files (R10).
- Namespace **manifests** (R15) are snapshots tagged `kind=manifests` with path
  `/manifests/<namespace>`; `crystalctl repo export --tar latest --namespace <ns> --kind
  manifests -o m.tar` on such a snapshot yields the sanitized YAML files.

### 2.2 Reversibility contract: upstream-restic equivalence (R8)

The repository is a standard **restic repo v2**. Every `repo` command has an exact
upstream `restic` equivalent, documented in `--help` and in the user guide. If Crystal
Backup tooling is unavailable, users fall back to plain restic
(`RESTIC_REPOSITORY`/`RESTIC_PASSWORD` + `AWS_*` env):

| `crystalctl` command | Upstream restic equivalent |
|---|---|
| `repo snapshots` | `restic snapshots [--json]` |
| `repo snapshots --pvc data-postgres-0` | `restic snapshots --tag pvc=data-postgres-0` |
| `repo snapshots --namespace c-team-x` | `restic snapshots --tag namespace=c-team-x` |
| `repo ls <snap> [path]` | `restic ls [--json] <snap> [path]` |
| `repo dump <snap> <path>` | `restic dump <snap> <path>` |
| `repo export --tar <snap> [path] -o f.tar` | `restic dump --archive tar <snap> <path> > f.tar` |
| `repo export --zip <snap> [path] -o f.zip` | `restic dump --archive zip <snap> <path> > f.zip` |
| `repo restore <snap> --target d --include g` | `restic restore <snap> --target d --include g [--sparse]` |
| `repo stats` | `restic stats` |

The repository URL is the restic S3 URL `s3:<endpoint>/<bucket>/<prefix>/<clusterID>` (R20
layout — **one shared repo per location**, tenancy carried by tags, **no per-namespace
suffix**); the exact string is published in `BackupRepository.status.repositoryURL`. The
repository key is any registered restic key slot
([adr/0004](adr/0004-encryption-key-management.md)): for the **cluster** repo, the
admin-only platform key (the DEK unwrapped from `crystal-backup-system`); for a
**namespace** `BackupLocation` repo, the user's own password from their
`repositoryPasswordSecretRef` (or the operator-generated password stored in the user's
namespace).

### 2.3 Snapshot addressing

`<snapshotID>` is a restic snapshot ID (short form accepted) or `latest`. `latest`
combined with `--pvc`/`--run`/`--namespace` filters resolves exactly like
`restic snapshots latest --tag …`. Ambiguous prefixes are an error (exit 4).

## 3. Configuration

Precedence, highest first: **flags > environment > config file profile > defaults**
(`CRYSTALCTL_*`/`AWS_*` env override the file).

### 3.1 Flags (standalone globals)

`--repo <url>`, `--profile <name>`, `--password-file <path>` (`-` = stdin),
`--password-command <cmd>`, `--ca-bundle <pem-file>`, `--cache-dir <dir>`, `--no-cache`,
`--output (table|json)`. There is deliberately **no `--password <literal>` flag**:
secrets never appear in argv (visible in `ps`/shell history).

### 3.2 Environment variables

| Variable | Meaning |
|---|---|
| `CRYSTALCTL_PROFILE` | named profile from the config file |
| `CRYSTALCTL_REPOSITORY` | repository URL |
| `CRYSTALCTL_PASSWORD_FILE` / `CRYSTALCTL_PASSWORD_COMMAND` | key source (preferred) |
| `CRYSTALCTL_PASSWORD` | key literal (accepted, discouraged) |
| `CRYSTALCTL_CA_BUNDLE`, `CRYSTALCTL_CACHE_DIR`, `CRYSTALCTL_OUTPUT` | as the flags |
| `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `AWS_SESSION_TOKEN`, `AWS_DEFAULT_REGION`, `AWS_PROFILE`, `AWS_SHARED_CREDENTIALS_FILE` | passed through **verbatim** to the engine |
| `KUBECONFIG` | cluster mode only (standard client-go rules) |

`RESTIC_*` variables in the caller's environment are **not** forwarded (they would bypass
profile resolution silently); a warning is printed if any are set.

### 3.3 Config file

`~/.config/crystalctl/config.yaml` (override: `--config`, `CRYSTALCTL_CONFIG`). Named repo
profiles; no inline passwords or S3 secrets — file/command references only:

```yaml
currentProfile: offsite
profiles:
  offsite:
    repository: s3:https://s3.example.net/team-x-backups/crystal/prod-eu-1
    passwordFile: ~/.config/crystalctl/keys/offsite.key
    caBundle: ~/.config/crystalctl/ca/offsite.pem
    awsProfile: offsite-backups           # resolved via standard AWS config files
```

`crystalctl` refuses to start (exit 2) if the config file or a referenced password file is
group- or world-readable.

## 4. Cluster mode

Standard kubeconfig loading (`--kubeconfig`, `--context`, `-n/--namespace` defaulting to
the kubeconfig context namespace). Namespace-plane commands need only the **namespace-user**
role (CRUD on `backupschedules`/`backuplocations`/`restores`, read-only `backups`); admin
commands (`--cluster-schedule`, cross-namespace restore, `admin erase`, `admin
decommission`) need the **platform-admin** role
([02-api.md § RBAC packaging](02-api.md)).

### 4.1 `crystalctl backup`

```
crystalctl backup trigger --schedule <name> [--wait] [--dry-run]
crystalctl backup trigger --cluster-schedule <name> [--wait] [--dry-run]   # admin: manual DR run
crystalctl backup status [<backupName>] [--origin cluster|namespace]
```

- `trigger --schedule` reads the `BackupSchedule`, creates an ad-hoc namespaced `Backup`
  named `<schedule>-manual-<timestamp>` (spec mirrors the schedule's
  `locationRef`/`pvcSelector`/`includeManifests`/`hooks`/`retention`, `scheduleRef` empty,
  per [02-api.md](02-api.md)). `trigger --cluster-schedule` (admin) creates a manual
  `ClusterBackup` from a `ClusterBackupSchedule` template, which fans out `Backup` objects
  into the selected namespaces. `--wait` polls `status.phase` until terminal, exits 0/6/1
  for `Completed`/`PartiallyFailed`/`Failed`.
- `status` without argument lists the namespace's `Backup` CRs (origin, phase, volumes,
  sizes, `addedBytes`); with an argument, prints per-volume detail. `Backup` objects are
  **discovery-projected views of the repository** (the repository is the source of truth);
  cluster-origin `Backup` objects (`crystalbackup.io/origin: cluster`) are read-only.

### 4.2 `crystalctl restore`

```
crystalctl restore create
    # source — self-service (→ namespaced Restore, target = the caller's namespace):
    (--backup <name> | --latest [--origin cluster|namespace])
    # source — admin (→ ClusterRestore, a repo coordinate):
    (--location <name> --source-namespace <ns> (--run <name> | --time latest|<RFC3339>)
       --target-namespace <ns> [--create-namespace] [--storage-class-map <src>=<dst>]...)
    # mode + selection (both selection groups omitted ⇒ whole namespace):
    [--mode Recreate|Overwrite]
    [--resources <glob>]... | --data-only
    [--volume <name>]... [--volume-include <name>=<glob>]... [--volume-exclude <name>=<glob>]... [--target-path <name>=<path>]... | --manifests-only
    [--confirm <namespace>] [--wait] [--dry-run] [--plan]
crystalctl restore status [<restoreName>]
```

The **self-service form** creates a namespaced `Restore` naming a `Backup` **in the
caller's namespace** (no `locationRef`, no target-namespace field), so it is structurally
confined to that namespace (R14); if that `Backup` is `origin=cluster`, the operator
mediates against the shared DR repo with the non-forgeable `namespace=` tag filter. The
**admin form** (any of `--location`/`--source-namespace`/`--target-namespace`) creates a
`ClusterRestore` addressing a **repo coordinate**; `--create-namespace` restores into a
fresh namespace (non-destructive).

- **mode** maps to `spec.mode`: `Recreate` (delete+replace) or `Overwrite` (default;
  server-side apply that keeps objects absent from the backup).
- **selection** maps to the NetworkPolicy-style lists `spec.resources[]` / `spec.volumes[]`
  (OR between items, AND within one): each `--resources` adds a resources item (type/name
  globs); `--volume`/`--volume-include`/`--volume-exclude`/`--target-path` add a volumes
  item (`--volume-include` gives partial-PVC restore, R7). `--data-only` sets `resources:
  []` (restore no manifests); `--manifests-only` sets `volumes: []` (restore no data); both
  groups omitted restores the **whole namespace**. Arbitrary label selectors or multiple
  include/exclude sets per item need `--dry-run` then a hand-edited apply.
- For **destructive** operations (R23 — any `Recreate` or `Overwrite`) the CLI validates
  `--confirm` **locally** against the exact target
  namespace (own namespace for `Restore`, `--target-namespace` for `ClusterRestore`) and
  refuses to create the CR otherwise (exit 5), printing the expected value; the admission
  policy remains the authoritative gate (VAP, [adr/0010](adr/0010-admission-vap-first.md); the
  `AwaitingConfirmation` phase exists for CRs created via kubectl/GitOps).

  **Two different dry runs, deliberately kept apart.** `--dry-run` is the global CLI
  convention of §1: print the CR YAML instead of applying it — a *client-side* affordance
  that never reaches the cluster. The *server-side* plan is the CR's own field, set with
  `--plan`, which creates a real `Restore` with `spec.dryRun: true`; it runs the full
  pipeline with server-side dry-run applies, persists nothing, and reports the plan in
  `status.resources` ([04-manifest-backup.md §5.4](04-manifest-backup.md), landed M3).
  Overloading one flag with both would be a trap: `--dry-run` promises "nothing happens",
  and the server-side plan does create an object
  ([02-api.md](02-api.md), [04-manifest-backup.md](04-manifest-backup.md)).

### 4.3 `crystalctl admin erase` and `admin decommission` (CLI in M7; wraps the `ClusterErasure` CR delivered in M5, R21)

```
crystalctl admin erase --location <name>
    (--tenant <t> | --namespace <ns> | --namespace <ns> --pvc <name>)
    --confirm <identity> [--wait]
```

**Right-to-erasure** — **physical** deletion in the shared repo. Creates a `ClusterErasure`
that runs `restic forget --tag` (`tenant=` / `namespace=` / `namespace=+pvc=`) then `prune`.
Exactly one target; `--confirm` must equal the target **identity** (the tenant name, the
namespace name, or `<namespace>/<pvc>` — R23), validated locally (exit 5) and re-checked by
admission. On **Immutable** locations the `ClusterErasure` is `Blocked` until object-lock
expiry; the CLI prints `blockedUntil` and exits 6 ([adr/0005](adr/0005-immutability-mode.md)).
Per-tenant crypto-shredding does **not** exist in a single-key shared repo — this replaces
the former `admin shred` ([adr/0004](adr/0004-encryption-key-management.md),
[adr/0009](adr/0009-shared-cluster-repo-tag-tenancy.md)).

```
crystalctl admin decommission --location <name> --confirm <repositoryURL>
```

Retires an **entire repository** by destroying its wrapped platform DEK (and, per policy,
its KEK). This is **repo-granularity — a lifecycle/retirement tool, not a GDPR erasure
mechanism** (use `admin erase` for tenant/namespace/PVC erasure). Strong **double
confirmation**: requires the platform-admin role, `--confirm` must equal the repository URL,
**and** an interactive re-type of the repository URL on a TTY after the CLI prints the
affected `BackupRepository` (repository URL, `keySlots`, `approximateSizeBytes`). There is
deliberately no non-interactive bypass in v1. Emits a Kubernetes Event on the
`BackupRepository` and an audit log line. (Leaked-DEK re-encryption — `crystalctl admin
reencrypt` — is backlog, not v1; see [adr/0004](adr/0004-encryption-key-management.md).)

### 4.4 `crystalctl ui` (M7, R9)

Local thick client — the **hybrid mode** of §1, listed here for discoverability: serves
the browse SPA on `127.0.0.1:<random-port>` (loopback only), using either a standalone
profile or the kubeconfig to resolve repositories, and accesses S3 directly. Lower
priority than everything else; behavior specified in [07-ui.md](07-ui.md).

## 5. Output and exit codes

- `--output table` (default) for humans; `--output json` emits a **stable, documented
  `crystalctl` schema** on stdout for scripting (engine JSON is passed through only where it
  is already 1:1, e.g. `repo ls --output json`). Byte-stream commands (`dump`, `export`)
  ignore `--output`; all diagnostics go to stderr.
- Exit codes (stable API for scripts):

| Code | Meaning |
|---|---|
| 0 | success |
| 1 | runtime error (engine failure, I/O, apiserver error) |
| 2 | usage or configuration error (bad flags, unreadable/insecure config) |
| 3 | repository access error (S3 unreachable, bad credentials, wrong key) |
| 4 | not found / ambiguous (snapshot, path, CR, profile) |
| 5 | confirmation or validation refused (R23, admission denial) |
| 6 | partial success (`PartiallyFailed`, partial restore, `Blocked` erasure) |
| 7 | timeout (`--wait` deadline exceeded) |

Engine exit codes are normalized: restic `10` (no repository) and `12` (wrong password)
map to 3, `11` (lock) to 3 with a retry hint, anything else non-zero to 1.

## 6. Engine embedding and secret hygiene

- The binary **embeds the pinned upstream restic release** for its OS/arch (`go:embed`),
  the same engine version as the mover image (ADR 0001). At startup it is extracted to a
  `0700` per-user runtime directory, its SHA-256 verified against a compiled-in checksum
  before exec, and removed on exit. `crystalctl version` reports both versions.
- The repository key is passed to the engine through an **inherited pipe file
  descriptor** (`RESTIC_PASSWORD_FILE=/dev/fd/3` on POSIX; anonymous handle on Windows)
  — never argv, never a temp file on disk, never the parent's exported environment.
- S3 credentials reach the engine only via its child-process environment (`AWS_*`),
  never argv.
- Key material is held in `[]byte` buffers and best-effort zeroed after use (Go gives no
  hard guarantee; documented limitation). Interactive password prompts require a TTY and
  disable echo.
- No telemetry, no network calls other than S3/apiserver endpoints explicitly configured.

## 7. Distribution and versioning

- Targets: `linux/amd64`, `linux/arm64`, `darwin/amd64`, `darwin/arm64`,
  `windows/amd64`, `windows/arm64`; `CGO_ENABLED=0` static builds.
- Published as release artifacts alongside the operator image and the `crystal-backup` Helm
  chart, with a `SHA256SUMS` file (signature of the sums file: see open questions).
- The CLI version **equals the operator chart `appVersion`** (single release train,
  [adr/0014](adr/0014-versioning-and-release.md)).
  `crystalctl version --output json` → `{version, gitCommit, engineVersion, goVersion}`.
- Skew policy: `repo` commands work against any repository (format is the contract);
  cluster-mode commands support an operator within ±1 minor version and warn beyond.

## 8. Open questions

1. Artifact signing: checksums only, or additionally cosign/minisign signatures on the
   release binaries (matters for the user-facing reversibility story)?
2. Should `repo` gain a `mount` command (FUSE browse, restic parity)? Excluded from v1:
   platform-specific dependencies conflict with the single-static-binary rule.
3. Windows (a **confirmed v1 target**, amd64 + arm64): `/dev/fd` password passing needs an
   equivalent (named pipe with per-user ACL) — an M7 implementation detail, not a scope
   question.
