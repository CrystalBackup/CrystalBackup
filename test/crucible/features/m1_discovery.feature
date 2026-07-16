Feature: Discovery makes backups restorable with no prior CRs
  As a platform administrator recovering a cluster
  I want the operator to inventory the shared repository and list what is restorable
  So that adding a ClusterBackupLocation is enough to see every restore point, with no pre-existing objects

  Background:
    Given an initialized ClusterBackupLocation "dr" whose repository already holds snapshots for c-db and c-media

  Scenario: Discovery projects a Backup per (namespace, run) into existing namespaces
    When the discovery controller inventories the repository
    Then a Backup named after each run appears in c-db and in c-media
    And each projected Backup has origin=cluster and status.volumes derived from the repository snapshots
    And a run whose namespace does not exist on the cluster is NOT projected (it stays available only to ClusterRestore)

  Scenario: A projected Backup is a view, not the source of truth
    Given a projected Backup "R" in c-db
    When I delete the Backup CR "R"
    Then the repository snapshots for c-db/R are unchanged (deleting a Backup never runs restic forget)
    And discovery re-creates the projected Backup "R" on its next pass

  Scenario: kubectl get backups lists exactly the restorable set
    Given the repository holds exactly the run set {R} for c-db
    Then "kubectl get backups -n c-db" lists exactly {R} — no fabricated and no missing entries
    And after the snapshots for a run are removed from the repository, discovery removes that projected Backup
