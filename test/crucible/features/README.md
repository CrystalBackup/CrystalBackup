# Crucible acceptance features (Gherkin)

These `.feature` files are the **human-readable acceptance contract** for each
milestone, written in plain [Gherkin](https://cucumber.io/docs/gherkin/). They are
written **first** (test-driven): a scenario describes the expected behaviour before
the code that satisfies it exists.

## How they relate to the Go tests

There is **one** e2e framework in the crucible — Ginkgo (`../tests/`). We deliberately
do **not** run a second BDD engine (godog/Cucumber) against the same live cluster: it
would fork the suite into two runners and two reports, and throw away the crucible's
existing labelled specs and readable report. Instead:

- each `Scenario` here maps **1:1** to a Ginkgo `It` in `../tests/m1_*_test.go`,
  labelled `Label("m1")`;
- the `Given` / `When` / `Then` steps appear verbatim as `By("Given …")` calls, so they
  show up in the crucible's readable report (`mise run test`);
- the scenario title is reused as the `It` text, so a reader can trace a report line
  straight back to the scenario here.

So these files are the **specification**; the Ginkgo specs are the **executable**
implementation of exactly these scenarios. Keep them in sync — a new scenario means a
new `It`.

## When they run

Only **live**, against a real crucible cluster (`mise run test m1`), because they
exercise the real data path: CSI snapshots, mover Jobs, restic, S3. They are the
**final acceptance gate** for the milestone. During controller development the fast
red/green loop is `envtest` (in `internal/controller/...`); these features are the
end-to-end truth that says the milestone is actually done.

Until the M1 controllers exist the `m1` specs compile (they only use the shipped CRD
API) but fail live — that red **is** the definition of done we are building toward.

## M1 features

| feature | asserts |
| --- | --- |
| [`m1_repository.feature`](m1_repository.feature) | a `ClusterBackupLocation` provisions one initialized, encrypted shared repository; init happens exactly once; single-default election |
| [`m1_cascade.feature`](m1_cascade.feature) | a `ClusterBackupSchedule` fans out a `Backup` into every matched namespace; snapshots land in the shared repo with the correct restic identity; unsupported CSI is Skipped not Failed |
| [`m1_discovery.feature`](m1_discovery.feature) | discovery projects a `Backup` per (namespace, run) into existing namespaces; a projected `Backup` is a view, not the source of truth |
| [`m1_reliability.feature`](m1_reliability.feature) | leak-check (zero residual VS/VSC/temp-PVC); operator killed mid-run converges via Job re-adoption; an OOMKilled mover is a failure, not a silent success |
