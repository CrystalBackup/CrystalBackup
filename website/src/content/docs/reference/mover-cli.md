---
title: Command-line tools
description: The state of crystalctl, the crystal-mover container entrypoint, and how to drive a repository with upstream restic today.
---

## There is no user-facing CLI in this release

`crystalctl` — the standalone binary that will list, browse, dump and export from a
repository with no Kubernetes dependency, plus kubectl-style helpers — is specified and
**not implemented**. There is no `cmd/crystalctl` in the tree, and nothing to download.

It will not ship from this repository either: the CLI becomes a `kubectl` plugin in its own
repository, distributed through krew, and the browse UI becomes its own project. That is a
packaging decision, not a downgrade — the specification is unchanged, and the constraint that
comes with it is that **no capability will ever be reachable only through the CLI**. Everything
stays expressible as a custom resource, and everything stays readable with upstream `restic`.

This page is here so that is unambiguous, and so you know what to use instead.

## What to use instead: upstream restic

This is not a workaround. Reading a Crystal Backup repository with upstream `restic` is the
reversibility guarantee the project is built around, and it costs two environment variables.

### Getting the password

**A namespace-plane repository** — the password is yours:

```bash
export RESTIC_PASSWORD=$(kubectl -n team-x get secret offsite-key \
  -o jsonpath='{.data.password}' | base64 -d)
```

If the operator generated it, the Secret is `crystal-repo-password-<location>` in your
namespace.

**A cluster-plane repository** — the key is wrapped under the age KEK, so unwrap it:

```bash
kubectl -n crystal-backup-system get secret cluster-kek \
  -o jsonpath='{.data.identity}' | base64 -d > /tmp/kek.txt

export RESTIC_PASSWORD=$(kubectl -n crystal-backup-system get secret crystal-dek-<location> \
  -o jsonpath='{.data.dek}' | base64 -d | age -d -i /tmp/kek.txt)
```

Delete `/tmp/kek.txt` afterwards. On a rebuilt cluster you unwrap from your out-of-band
escrow instead, and the wrapped key comes from the bucket at
`<prefix>/<clusterID>.crystal-meta/wrapped-dek.age`.

### The repository URL

```
s3:<endpoint>/<bucket>/<prefix>/<clusterID>
```

An empty prefix drops that segment. The operator publishes the exact string:

```bash
kubectl get backuprepository <name> -o jsonpath='{.status.repositoryURL}{"\n"}'
```

### Things worth knowing how to do

```bash
export AWS_ACCESS_KEY_ID=... AWS_SECRET_ACCESS_KEY=...
R=s3:https://s3.example.com/crystal-backups/dr/prod-eu-1

# What is in there.
restic -r $R snapshots --tag crystalbackup

# One namespace only. Comma-joined tags in ONE --tag flag are ANDed;
# repeating --tag would OR them.
restic -r $R snapshots --tag crystalbackup,namespace=team-x

# One run.
restic -r $R snapshots --tag crystalbackup,namespace=team-x,run=dr-daily-20260730-020000

# Browse a snapshot's tree.
restic -r $R ls <snapshot-id>

# Pull one file out, without restoring anything into the cluster.
restic -r $R dump <snapshot-id> /data/team-x/uploads/images/2026/photo.jpg > photo.jpg

# Restore to local disk.
restic -r $R restore <snapshot-id> --target ./recovered

# Verify the repository.
restic -r $R check --read-data-subset 1%
```

Nothing in that list involves a Crystal Backup component. That is the point of it.

:::caution[Do not write to a live repository by hand]
`forget`, `prune` and `unlock` take exclusive locks that the operator's own queue is
designed to serialise against. Running them yourself, out of band, is outside the
single-writer assumption the queue is built on and can collide with an in-flight mover.
Reads are safe. For writes, use `ClusterErasure` or the location's `maintenance` schedule.
:::

## `crystal-mover`

`crystal-mover` is the **container entrypoint of mover Jobs**, not a tool you run. It is
documented here because you will see it in Job specs and pod logs while diagnosing, and
because knowing its shape makes those logs readable.

It is a thin shim around restic. It takes two flags and forwards everything after `--`
verbatim:

```
crystal-mover --operation <op> -- <restic argv...>
```

| Flag | Default | Meaning |
|---|---|---|
| `--operation` | *(required)* | Which operation this Job is. |
| `--termination-log` | `/dev/termination-log` | Where the result JSON is written. |

Accepted operations:

| Value | What the Job is doing |
|---|---|
| `backup` | back up one PVC |
| `restore` | restore one PVC |
| `init` | initialise the repository (idempotent — an "already initialized" failure is treated as success) |
| `forget` | apply retention |
| `prune` | reclaim space |
| `check` | verify the repository |
| `snapshots` | inventory the repository for discovery |
| `unlock` | clear stale locks |
| `sync` | `restic copy` for external sync |
| `manifests-backup` | dump and upload a namespace's manifests |
| `manifests-restore` | restore a namespace's manifests |
| `cluster-manifests-backup` | capture cluster-scoped resources |
| `cluster-manifests-restore` | restore cluster-scoped resources |

Two properties worth knowing while diagnosing:

- **Secrets never appear in argv.** The repository URL, the password file path and the S3
  credentials arrive as environment variables from a read-only Secret mount. A Job spec you
  read with `kubectl get job -o yaml` therefore contains no key material.
- **The outcome is a JSON result on the termination message**, not the exit path alone. If
  a mover pod is gone before you could read its logs, the durable trace is in the owning
  object's status — `status.volumes[]` on a `Backup`, `status.recentMaintenance[]` on a
  `BackupRepository`.

```bash
kubectl -n crystal-backup-system get jobs -l app.kubernetes.io/managed-by=crystal-backup
kubectl -n crystal-backup-system logs job/<name>
```

Mover Jobs have **deterministic names** — a pure function of what they do, never random —
so a restarted operator re-adopts a running Job rather than starting a second one.

## What `crystalctl` will be

For completeness, from the specification. None of it exists yet.

- A standalone static binary for linux, windows and darwin on amd64 and arm64, with **no
  Kubernetes dependency** — it opens a repository from S3 credentials and a key.
- `list`, `browse`, `dump`, `export tar`, and local restore.
- kubectl-style helpers when a kubeconfig is present: trigger a backup, watch status.
- Administrative wrappers over erasure and repository decommissioning.
- A local browse UI as a subcommand.

Track it on the [roadmap](https://github.com/CrystalBackup/CrystalBackup/blob/main/spec/90-roadmap.md).
