Feature: Cluster-DR backup cascade
  As a platform administrator
  I want one ClusterBackupSchedule to back up many tenant namespaces into the shared repository
  So that the whole platform is protected by a single run, tenancy carried by restic tags

  Background:
    Given an initialized ClusterBackupLocation "dr" for the shared repository
    And the seeded tenant namespaces labelled crystalbackup.io/seed=crucible
      | namespace | storage           | note                          |
      | c-db      | ceph-block (RWO)  | StatefulSet, checksummed data |
      | c-media   | cephfs (RWX)      | shared by two pods            |
      | c-edge    | longhorn          | exotic data (xattrs, links)   |
      | c-legacy  | local-path        | NO CSI snapshot support       |
      | c-web     | (none)            | manifests only, no PVC        |
      | c-empty   | (none)            | policy objects only           |

  Scenario: A ClusterBackupSchedule fans out a Backup into every matched namespace
    When I create a ClusterBackupSchedule selecting crystalbackup.io/seed=crucible
    And a run is triggered
    Then a ClusterBackup run is created and reaches a terminal phase within 20 minutes
    And a Backup named after the run exists in each of the 6 matched namespaces
    And every child Backup carries labels crystalbackup.io/cluster-backup=<run> and crystalbackup.io/origin=cluster
    And no child Backup has an ownerReference to the ClusterBackup (the cross-namespace link is by label only)
    And the ClusterBackup status has namespacesMatched=6 and pvcsSucceeded greater than zero

  Scenario: Every snapshottable PVC lands in the shared repository with the correct restic identity
    Given a completed ClusterBackup run "R"
    When I list the repository snapshots with the platform DEK
    Then there is a data snapshot for c-db's PVC with host "crucible", path "/data/c-db/<pvc>", and tags including "namespace=c-db", "pvc=<pvc>", "kind=data", "run=R"
    And there is a data snapshot for c-media's RWX cephfs volume
    And there is a data snapshot for c-edge's longhorn volume
    And c-edge's exotic data is preserved (the mover stored xattrs and kept hardlinks/symlinks)

  Scenario: A volume on storage without snapshot support is Skipped, not Failed
    Given the run backs up c-legacy whose PVC is on local-path with no VolumeSnapshotClass
    Then c-legacy's Backup lists that volume with phase Skipped and reason CSISnapshotUnsupported
    And c-legacy's Backup is PartiallyCompleted, never Failed
    And one unsupported volume does not fail the whole platform run

  Scenario: A namespace with no PVC completes cleanly
    Given c-web and c-empty have no PersistentVolumeClaims
    Then their Backups reach a terminal, non-failed phase with zero volume failures
