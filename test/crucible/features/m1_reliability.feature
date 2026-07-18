Feature: The cascade converges and leaves no orphans
  As a platform operator
  I want backup runs to clean up after themselves and to survive an operator restart
  So that repeated runs never leak storage objects and a crash never wedges a run

  Scenario: Leak-check — a run leaves zero residual snapshot objects
    Given a completed ClusterBackup run across all seeded namespaces
    Then there are zero VolumeSnapshots left behind in any tenant namespace
    And zero VolumeSnapshotContents attributable to the run
    And zero temporary clone PVCs created by the exposer
    # held by the exposer's ReadyToUse wait + ordered cleanup + the orphan reaper

  Scenario: The operator killed mid-run converges via Job re-adoption
    Given a ClusterBackup run in progress with mover Jobs still running
    When the operator pod is deleted and then restarts
    Then the operator re-adopts the in-flight mover Jobs instead of orphaning or duplicating them
    And the run reaches a terminal phase with no Backup left stuck in a non-terminal phase
    And the leak-check invariant still holds afterwards

  Scenario: An OOMKilled mover is reported as a failure, not a silent success
    Given a mover container that is killed before it writes its termination message
    Then that volume's status is Failed (an empty termination message is treated as a crash, never as success)
    And the repository lock is checked and cleared so the next run is not blocked by a stale lock
