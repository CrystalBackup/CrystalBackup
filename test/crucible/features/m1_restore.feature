Feature: Off-cluster restore with upstream restic (R8 reversibility)
  As a platform administrator facing disaster recovery
  I want to restore a backup straight from object storage with the stock restic CLI
  So that my data is recoverable without the operator, the CRDs, or even a surviving cluster

  Background:
    Given an initialized ClusterBackupLocation "dr" for the shared repository
    And a completed cluster-DR backup of the seeded tenant "c-db" (RWO, with a recorded MANIFEST.sha256 at its volume root)

  Scenario: A data snapshot restores byte-for-byte with the restic CLI alone
    Given the kind=data snapshot for "c-db" in the shared repository
    When I run "restic restore" from S3 in a Job running only the stock restic image, with the platform DEK as the password and no CrystalBackup CR consulted
    And I check the restored tree against the seed's MANIFEST.sha256 with "sha256sum -c"
    Then the restore completes and every restored file matches the source byte-for-byte
    And the repository is proven readable off-platform, with no operator and no custom resources
