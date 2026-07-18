Feature: Shared cluster-DR repository lifecycle
  As a platform administrator
  I want a ClusterBackupLocation to provision exactly one encrypted, shared restic repository
  So that every tenant namespace is backed up into one place, initialized once, with no init race

  Background:
    Given an S3 bucket reachable at the crucible's S3 endpoint
    And a platform KEK (an age X25519 identity) stored as a Secret in "crystal-backup-system"

  Scenario: A ClusterBackupLocation provisions one initialized shared repository
    When I create a ClusterBackupLocation "dr" for the bucket with clusterID "crucible"
    Then a cluster-scoped BackupRepository is created for "dr"
    And the BackupRepository reaches Initialized=true within 5 minutes
    And its status.repositoryURL is "s3:<endpoint>/<bucket>/<prefix>/crucible"
    And exactly one Secret "crystal-dek-dr" exists in "crystal-backup-system"
    And the Secret "crystal-dek-dr" holds only the age-wrapped DEK, never a plaintext password
    And the ClusterBackupLocation reports condition Reachable=true and Ready=true

  Scenario: The shared repository is initialized exactly once, even under concurrent reconciles
    Given a ClusterBackupLocation "dr" that has provisioned its repository
    When the operator reconciles the location repeatedly and concurrently
    Then "restic cat config" succeeds against the repository with the platform DEK
    And the repository was initialized exactly once, with no duplicate or corrupt config from an init race

  Scenario: Only one ClusterBackupLocation may be the default
    Given a default ClusterBackupLocation "dr"
    When I create a second ClusterBackupLocation "dr2" also marked default
    Then one of the two locations reports condition MultipleDefaults=true
    And the operator never silently treats both as the default
