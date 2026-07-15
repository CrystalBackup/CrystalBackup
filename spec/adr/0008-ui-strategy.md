# ADR 0008 — UI strategy: local thick client first, hosted multi-tenant console later

Status: **Accepted** (2026-07-11; realigned to the two-plane cascade model 2026-07-12)

## Context

R9 requires a UI that lists the backups of a location, browses a backup's file tree,
and downloads individual files or directories without retrieving everything. The
product owner has ranked the UI **below everything else**
([00-requirements.md §7](../00-requirements.md)): core path, CLI, namespace-plane
locations/keys all come first; the UI lands at **M7** ([90-roadmap.md](../90-roadmap.md)).

Constraints the UI inherits from the rest of the design:

- **Engine**: repositories are plain restic format ([adr/0001](0001-repository-engine-restic-format.md)).
  restic has no Go library API; a browse backend either shells out to the pinned restic
  CLI (`ls --json`, `dump`, `dump -a tar|zip`) or embeds `rustic_core` (pre-1.0,
  "API subject to change"). Browsing restic over S3 is known to be slow because pack
  files are fetched on demand (documented by Backrest itself).
- **Tenancy**: R2 (storage-level isolation), R14 (a user may only see backups
  originating from their own namespace) and the key model
  ([adr/0004](0004-encryption-key-management.md)) mean repository access is
  **operator/backend-mediated**: the shared cluster DR repo's platform key never leaves
  `crystal-backup-system`, and a namespace repo is unlocked with the user's own key. A
  multi-tenant web UI must therefore open repositories **server-side** — a cluster-origin
  backup under a non-forgeable `namespace=` tag filter, a namespace-origin backup with the
  user's own key — so credentials and keys never reach the browser. The 2026-07-11 survey
  concluded this server-side-open architecture is the **only** one satisfying R2, R9 and
  R14 together — and that no existing restic/kopia UI implements it.
- **Console context**: the platform gives its users a Kubernetes console (e.g. Rancher or
  Headlamp) fronted by an OIDC IdP such as Keycloak. Such consoles increasingly ship
  extension/plugin systems — a plausible home for a future integrated UI.
- **Reversibility (R8)**: whatever we build, a namespace user with S3 credentials + key
  must be able to do everything with upstream `restic`; the UI is convenience, never a
  lock-in layer.

## Decision

1. **The UI stays the lowest-priority deliverable** (M7). No milestone before M7 takes
   on UI work; the browse package API is built in M7 alongside the `crystalctl` CLI
   (they share `internal/browse`), not before.

2. **v1 UI (M7) = `crystalctl ui`, a local thick client.** A subcommand of the existing
   standalone CLI: one Go binary embedding a prebuilt SPA (`go:embed`), serving on
   **localhost only** (default `127.0.0.1`, random high port, per-session URL token,
   `Host`-header check). Credential model identical to the R8 CLI path — S3 credentials
   + repository key supplied by the user:

   ```
   crystalctl ui --repo s3:https://s3.example.net/team-x-backups/crystal/prod-eu-1 \
                 --password-file ./repo.key
   # serving on http://127.0.0.1:53412/?token=…
   ```

   Scope: **read-only browse and download** — list snapshots of a repository, browse a
   snapshot's tree, stream a single file, download a directory as a zip/tar built on
   the fly. **No cluster mutations**: no triggering backups, no restores, no CR
   edits. Restores remain the job of the CRDs and `crystalctl restore`.

3. **The browse backend is a reusable Go package** (`internal/browse`), not code welded
   into the `ui` subcommand. Its normative shape is defined in
   [07-ui.md §4](../07-ui.md): interface `Browser`
   (`ListSnapshots`, `ReadDir`, `OpenFile`, `Archive`) plus a thin HTTP
   handler layer (`GET /snapshots`, `GET /snapshots/{id}/ls?path=`,
   `GET /snapshots/{id}/file?path=`, `GET /snapshots/{id}/archive?path=`). v1
   implementation shells out to the pinned restic binary per
   [adr/0001](0001-repository-engine-restic-format.md), with a **persistent restic
   metadata cache** directory reused across invocations. The future hosted backend
   consumes the exact same package.

4. **Long-term (post-v1, separate go/no-go) = a console-integrated multi-tenant UI**:
   a **Rancher extension or a Headlamp plugin** — the choice is **deferred** to the M7+
   revisit (a platform console such as Rancher is a natural target, but the extension
   ecosystems are moving) — backed by a **hosted multi-tenant API** deployed in
   `crystal-backup-system`:

   - Front authenticates against **Keycloak (OIDC + PKCE)** (the platform IdP).
   - The backend validates the token, then resolves the user's namespace set **server-side
     from their cluster RBAC** (never from browser input or token claims): for each candidate
     namespace it issues `SubjectAccessReview{verb: create, resource: restores.crystalbackup.io,
     namespace: N}` with the user's identity — the user sees the backups of exactly the
     namespaces where they may **create a `Restore`**. Mirrors the R2/R14 server-side
     namespace-derivation invariant of [02-api.md](../02-api.md).
   - Isolation is **structural**: the browser never names an arbitrary repository or
     namespace. For a **namespace-origin** `Backup` the backend opens the user's **own**
     repository with the user's key; for a **cluster-origin** `Backup` it opens the shared
     DR repo under the non-forgeable server-side `namespace=<the user's namespace>` tag
     filter (the same mediation the operator enforces on `Restore`). It unwraps the
     platform DEK or reads the user key **in-process** via `internal/browse` and streams
     results. **Credentials and keys never reach the browser.**

5. **No existing UI becomes the product UI** (see Alternatives). **Backrest is retained
   as an optional internal admin tool** for platform administrators. A **time-boxed POC of
   Zerobyte is allowed** before committing to build the hosted backend (see Revisit
   triggers).

## Consequences

### Positive

- R9's browse/download value ships at a fraction of the hosted-service cost: no new
  internet-facing component, no OIDC integration, no server operations — the thick
  client holds credentials exactly like upstream `restic` would on the same laptop.
- `internal/browse` is written once and reused by the hosted backend; the thick client is a
  permanent integration test of the future console's data path.
- The thick client doubles as a **reversibility demonstration** (R8): everything it
  does is documented alongside its upstream `restic` equivalent.
- The Rancher-vs-Headlamp bet is deferred until there is real information (console
  strategy, extension API stability), instead of being locked in while the UI is the
  lowest priority.
- No repository credential ever transits a browser in v1 — the R2/R14 threat surface is
  unchanged by the UI milestone.

### Negative

- **Browse latency**: listing a restic tree over S3 fetches pack files; first browse of
  a large repo is slow. Mitigated (not eliminated) by the persistent metadata cache; the
  shared cluster DR repo carries a **large** index (all namespaces) and is the slow case,
  while namespace-plane user repos stay small. Accepted for a lowest-priority deliverable.
- **No hosted web UI for users until post-v1**: they use the CLI or the thick
  client. Accepted explicitly by the product owner's priority ranking.
- One restic subprocess per browse operation (cost accepted in
  [adr/0001](0001-repository-engine-restic-format.md)).
- The thick client adds a release artifact matrix (linux/darwin/windows × amd64/arm64)
  and an SPA toolchain to an otherwise pure-Go repo.

### Risks & mitigations

| Risk | Mitigation |
|---|---|
| Local server exposed beyond localhost (misconfiguration, DNS rebinding). | Bind `127.0.0.1` only (no flag to bind `0.0.0.0` in v1), random per-session URL token required on every request, `Host`/`Origin` checks, no CORS. |
| SPA dependency licenses conflict with future open-sourcing. | Permissive-license-only policy applies to frontend deps too; lockfile audited in CI (same rule as Go deps, 00-requirements §5). |
| Thick client and hosted backend drift apart. | Single `internal/browse` + shared HTTP handler layer; contract tests run against both binaries. |
| restic `--json` output changes break the browse package. | Pinned vendored restic + contract tests (identical mitigation to [adr/0001](0001-repository-engine-restic-format.md)). |
| Zerobyte POC quietly becomes production (AGPL-3.0 network copyleft would force publishing our modifications). | POC is time-boxed, unmodified-upstream only, on a throwaway repo copy; adoption requires reopening this ADR. |
| Cache directory leaks another tenant's metadata on a shared admin machine. | Cache is per-repository, keyed by repo ID, under the user's XDG cache dir with `0700` perms — same properties as restic's own cache. |

## Alternatives considered

- **Backrest** (garethgeorge/backrest, GPL-3.0, Go + React, wraps restic CLI) — the
  most popular restic UI (~6.9k stars, v1.13.0 May 2026); snapshot browser with direct
  file/zip download, imports repos created by other tools. **Rejected as the product
  UI**: no OIDC — [issue #1123](https://github.com/garethgeorge/backrest/issues/1123)
  open with no maintainer response — and its multi-user model has no roles and no
  per-repo isolation: **every authenticated user sees every repo**, which violates
  R2/R14 outright. GPL-3.0 has no network clause, so internal use is fine: **retained
  as an optional internal admin tool** for platform administrators. The "one instance per
  tenant behind oauth2-proxy" workaround was rejected as a product: an instance fleet to
  operate, and still no isolation guarantee inside each instance.
- **Zerobyte** (nicotsx/zerobyte, AGPL-3.0, TypeScript, wraps restic CLI) — the closest
  existing fit: organizations as tenants, per-org generic OIDC (Keycloak-compatible),
  attaches existing restic repos, file-level restore selection; very active (v0.40.0,
  June 2026). **Rejected as the default product UI**: AGPL-3.0 network copyleft (any
  modification served to users must be published), self-declared v0.x with
  breaking changes, no IdP group mapping, invitation-based provisioning, org members
  are coarse "trusted operators" with no fine-grained RBAC, and direct-browser
  download is unconfirmed in its docs. Because it is the only credible adoption
  candidate, a **time-boxed POC is allowed** before building the hosted backend (see
  Revisit triggers for the POC's exit questions).
- **kopia server + htmlui / KopiaUI** — good native browse-and-download story, but the
  **wrong engine**: the kopia format was rejected in
  [adr/0001](0001-repository-engine-restic-format.md) (R10 metadata gaps, single-key
  repos). Additionally its web UI connects with server-control credentials — a global
  admin view never designed for multi-tenant end users.
- **restic-browser** (emuell/restic-browser, MIT) — a desktop Tauri application:
  mono-user, no server, no web UI. Useful as an admin troubleshooting tool; does not
  address R9.
- **Build the hosted multi-tenant backend first, skip the thick client** — rejected on
  priority and risk: it stands up an internet-facing, credential-holding service
  before the core path, verification and key management have soaked in production
  (M6), for the lowest-ranked requirement. The thick client delivers the browse value
  now and `internal/browse` makes the hosted backend cheaper later, not redundant.
- **Embed the browse UI in the operator** — rejected: the operator never touches
  backup data bytes ([01-architecture.md §1](../01-architecture.md)); serving tenant
  file content from the control plane would concentrate every repository credential in
  one long-lived, cluster-privileged process and widen its attack surface.

## Revisit triggers

- **M7+ console decision**: reassess Rancher extension vs Headlamp plugin — Rancher
  extension API stability, Headlamp ecosystem maturity, and the platform's console
  roadmap at that date decide which front the hosted backend gets.
- **Zerobyte POC outcome** (if run): direct browser download works; operator-created
  repos attach cleanly (locks, cache) while being written; "1 org = 1 tenant" holds
  operationally; AGPL exposure acceptable. Pass → reopen this ADR to consider adoption
  for the hosted phase; fail → build on `internal/browse` as decided.
- **`rustic_core` maturity** (1.0, or its WebDAV/`ls`/`dump` APIs stabilize and it
  passes our metadata-fidelity suite): replace the restic shell-out inside
  `internal/browse` to eliminate per-request subprocesses — aligned with the engine
  revisit triggers in [adr/0001](0001-repository-engine-restic-format.md).
- **restic ships a public library API**
  ([restic#4406](https://github.com/restic/restic/issues/4406)): same effect, simpler.
- **Demand signal**: if pilot tenants request web browse before M7 completes, the
  product owner may re-rank the hosted backend — the architecture in Decision 4 is
  already agreed, only its schedule moves.
