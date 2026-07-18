# ADR 0015 — Per-repository exclusive-queue serialization: movers are readers, maintenance ops are writers, in-process (no Lease)

Status: **Accepted** (2026-07-18)

## Context

Every namespace and the cluster-DR plane point at **one shared restic repository per
`[Cluster]BackupLocation`** ([adr/0009](0009-shared-cluster-repo-tag-tenancy.md)). restic's
on-disk format tolerates concurrent *reads* and concurrent *backups*, but repository-lifecycle
operations — `init`, `forget`, `prune`, `check`, scoped erasure, and forced lock removal — are
**not** safe to run concurrently against the same repository:

- Two `restic init` racing on an empty shared repo is the exact upstream shared-repo init race
  (K8up [#1055](https://github.com/k8up-io/k8up/issues/1055)).
- A `prune` racing a `forget` (or another `prune`) can delete packs another writer still
  references — a corruption-class outcome.
- A forced `unlock --remove-all` (used to clear the lock a hard-killed mover left behind — see
  [adr/0001](0001-repository-engine-restic-format.md) and §Decision-3 below) removes **every**
  lock, including the live lock a concurrent backup mover is holding. If a mover is mid-write when
  its lock is yanked, the repository can be left mid-transaction.

M1 introduced the mechanism that makes all of this safe (`internal/repo/queue`,
`internal/controller/backup_maintenance.go`, `internal/controller/backup_controller.go`) but
recorded only the *stale-lock* facet in [adr/0001](0001-repository-engine-restic-format.md). The
serialization model itself — and, critically, **the contract every future mutating operation must
honour** — lived only in code comments. `prune` (M4) and scoped `erase` (M5) are exactly the
operations that will get this wrong if the contract is not written down: an implementer who adds
`OpPrune` to the queue but forgets to make it *block movers* reintroduces the corruption race the
queue was built to prevent. This ADR promotes the model to a first-class decision.

## Decision

### 1. The per-repository exclusive queue is the single serialization point

`internal/repo/queue` is an **in-process, per-repository-key single-flight**: for a given
repository key it admits **at most one mutating operation at a time, FIFO**; operations on
different repositories run fully concurrently (one worker goroutine per key). Every mutating restic
operation is modelled as an `OpKind` and **must** be enqueued here — never run directly:

| `OpKind`   | Operation                    | Introduced |
|------------|------------------------------|------------|
| `OpInit`   | `restic init`                | M1         |
| `OpUnlock` | `restic unlock --remove-all` | M1         |
| `OpForget` | `restic forget` (retention)  | M1         |
| `OpCheck`  | `restic check`               | M4         |
| `OpPrune`  | `restic prune`               | M4         |
| `OpErase`  | scoped erasure (`ClusterErasure`) | M5    |

Concurrency-safe **reads** (`restic snapshots`, `stats`, `ls`) are intentionally **not** gated by
this queue and must not be enqueued.

### 2. Readers and writers

The concurrency model is a per-repository **readers/writers** discipline, but note the asymmetry —
the two sides are enforced by *different* mechanisms and both are required:

- **Readers = data-movers.** A backup mover holds restic's own on-repo lock for the duration of its
  snapshot. Many movers may run against one repository at once (restic permits concurrent backups);
  they are *not* serialized by the queue.
- **Writers = mutating maintenance ops** (the `OpKind`s above). They are serialized against each
  other by the queue (§1). A writer that also **invalidates or removes locks a live reader could be
  holding** must additionally be serialized *against the readers* — see §3.

### 3. Mover quiescence for lock-destroying writers (`blocksMovers`)

Some writers cannot merely take their turn in the queue; they must run only when **no reader holds
a lock**. `OpUnlock` is the M1 example: it runs `unlock --remove-all`, which would rip out a live
backup's lock. Such ops are marked by `queue.blocksMovers(kind) == true`, which drives a **per-repo
backup⇄unlock mutex** with two halves:

- **Reader admission (holds movers back).** The queue counts how many `blocksMovers` ops are pending
  or in-flight per repo and exposes it as `Manager.QuiescenceRequired(repoKey)`. The Backup
  controller's mover-admission path refuses to start a **new** mover while that is true (an already
  in-flight mover Job is re-adopted, not blocked).
- **Writer drain (waits for readers to clear).** Before the lock-destroying op runs, the maintenance
  path (`waitForMoverQuiescence` / `maintenanceOpBlocksMovers`) drains in-flight mover Jobs — using
  the live Job set as restart-safe ground truth — bounded by the maintenance deadline.

Writers that are exclusive *by construction of what they rewrite* but do not remove a live reader's
lock (e.g. `forget`, `check`) do **not** set `blocksMovers`: the queue turn is enough.

### 4. In-process, not a Kubernetes Lease or mutex CRD

The operator runs single-active under controller-runtime **leader election**, so at any instant
exactly one process may mutate repositories. An in-process keyed mutex is therefore a sufficient —
and far cheaper — serialization point than a Lease or a bespoke mutex CRD: **zero** API-server
round-trips on a hot reconcile path, and it cannot itself wedge on API latency. restic's on-repo
lock object remains the **cross-process backstop** for the (leader-election-forbidden, but
defensively assumed) split-brain window; the queue is the **intra-process** guarantee that a single
leader never races itself.

This is deliberately *not* a distributed lock. If CrystalBackup ever runs repository maintenance
from more than one process (e.g. an out-of-operator maintenance Job, or active-active operators),
this decision must be revisited — the in-process queue would no longer be the single serialization
point, and a real distributed lock (a Lease, or reliance on restic's on-repo lock with retry) would
be required. That is out of scope for the single-leader operator.

## Forward contract — checklist for any new mutating operation (M4 `prune`/`check`, M5 `erase`)

When you add a repository-mutating operation, **all** of the following are mandatory. Skipping the
starred items reintroduces a corruption-class race:

1. Add its `OpKind` constant in `internal/repo/queue/queue.go` and **enqueue it** — never call
   restic directly. (The `OpKind`s already exist as stubs; wiring the reconciler is the work.)
2. ★ **If the op removes or rewrites locks/packs a live backup mover could hold or reference**
   (`prune` and `erase` both do), make `blocksMovers(kind)` return `true`, **and** perform the
   writer-side drain (`waitForMoverQuiescence`) before running it. Marking `blocksMovers` without
   the drain, or vice-versa, is only half the mutex.
3. Reads stay off the queue.
4. Add the op to `spec/01-architecture.md` §7 (maintenance) and, if it changes the readers/writers
   picture, update this ADR's table and §2/§3.
5. Cover it in the crucible: a live test that the op does not corrupt or unlock a **concurrent**
   backup on the same repository (mirror the M1 `An OOMKilled mover … reported as a failure` +
   leak-check pattern, which exercises the `OpUnlock` mutex under a sibling mover).

## Alternatives considered

- **Kubernetes Lease / mutex CRD** — rejected (§4): redundant under single-leader election, adds
  API round-trips to a hot path, can wedge on API latency.
- **Rely solely on restic's on-repo lock** — insufficient: it does not prevent a single leader from
  racing itself, does not order operations (FIFO fairness), and the whole point of `OpUnlock` is
  that a *bare* lock is sometimes exactly what must be removed.
- **One repository per namespace (sharding)** — would sidestep cross-tenant contention but was
  already rejected in [adr/0009](0009-shared-cluster-repo-tag-tenancy.md) for dedup and DR-discovery
  reasons.

## Consequences

- The corruption races (double-init, prune⇄forget, unlock-vs-live-backup) are structurally
  prevented for a single-leader operator.
- Every future maintenance milestone inherits a documented, testable contract instead of a code
  comment; the `blocksMovers` gate is the one line an M4/M5 author must not forget.
- The guarantee is scoped to single-leader; multi-writer deployments are explicitly out of scope and
  flagged for revisit (§4).

## References

- Mechanism: `internal/repo/queue/queue.go` (queue + `blocksMovers` + `QuiescenceRequired`),
  `internal/controller/backup_maintenance.go` (`waitForMoverQuiescence`,
  `maintenanceOpBlocksMovers`), `internal/controller/backup_controller.go` (mover admission),
  `internal/restic` (`UnlockArgs` → `unlock --remove-all`).
- [adr/0001](0001-repository-engine-restic-format.md) — restic format; stale-lock mitigation row.
- [adr/0009](0009-shared-cluster-repo-tag-tenancy.md) — one shared repository per location.
- [spec/01-architecture.md](../01-architecture.md) §7 (maintenance) / §8 (concurrency).
