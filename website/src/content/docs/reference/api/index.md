---
title: API reference
description: Every field of every CrystalBackup custom resource, generated from the Go types in api/v1alpha1.
tableOfContents:
  minHeadingLevel: 2
  maxHeadingLevel: 2
---

<!-- GENERATED FILE — do not edit. Run `make api-docs` after changing api/v1alpha1/. -->

This page is generated from the Go types in `api/v1alpha1/`, so it is exactly what
the CRDs installed in your cluster accept. `kubectl explain` on a live cluster is the
same information from the same source.






## Resource types
- [Backup](#backup)
- [BackupExternalSync](#backupexternalsync)
- [BackupLocation](#backuplocation)
- [BackupRepository](#backuprepository)
- [BackupSchedule](#backupschedule)
- [ClusterBackup](#clusterbackup)
- [ClusterBackupExternalSync](#clusterbackupexternalsync)
- [ClusterBackupLocation](#clusterbackuplocation)
- [ClusterBackupSchedule](#clusterbackupschedule)
- [ClusterErasure](#clustererasure)
- [ClusterRestore](#clusterrestore)
- [Restore](#restore)



## Backup



Backup is the execution unit and restore-point projection (source of truth = the repository).





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `crystalbackup.io/v1alpha1` | | |
| `kind` _string_ | `Backup` | | |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.30/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[BackupSpec](#backupspec)_ | spec defines the desired state of Backup |  |  |
| `status` _[BackupStatus](#backupstatus)_ | status defines the observed state of Backup |  |  |


## BackupExternalSync



BackupExternalSync replicates a namespace's backups to a secondary location (R28).





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `crystalbackup.io/v1alpha1` | | |
| `kind` _string_ | `BackupExternalSync` | | |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.30/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[BackupExternalSyncSpec](#backupexternalsyncspec)_ | spec defines the desired state of BackupExternalSync |  |  |
| `status` _[BackupExternalSyncStatus](#backupexternalsyncstatus)_ | status defines the observed state of BackupExternalSync |  |  |


## BackupExternalSyncSpec



BackupExternalSyncSpec replicates the namespace's backups from one BackupLocation
to another BackupLocation in the same namespace via restic copy, re-encrypted to
the destination's own user key. Both refs are same-namespace (structural confinement);
the platform key is never involved, so client siloing holds. R28, adr/0013.



_Appears in:_
- [BackupExternalSync](#backupexternalsync)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `sourceLocationRef` _[LocalObjectReference](#localobjectreference)_ | sourceLocationRef is a BackupLocation in this namespace (default: the default one). |  |  |
| `destinationLocationRef` _[LocalObjectReference](#localobjectreference)_ | destinationLocationRef is another BackupLocation in this namespace with its own key<br />(must differ from source — admission rule 9; never a ClusterBackupLocation — rule 2). |  |  |
| `schedule` _string_ | schedule is a cron expression; empty ⇒ on-demand only. |  |  |
| `timezone` _string_ | timezone for the cron expression (IANA name). |  |  |
| `paused` _boolean_ | paused suspends new syncs. |  |  |
| `mode` _[ExternalSyncMode](#externalsyncmode)_ | mode tracks the source (Mirror) or only adds (AppendOnly, forced on Immutable destinations). | Mirror | Enum: [Mirror AppendOnly] <br /> |


## BackupExternalSyncStatus



BackupExternalSyncStatus is the observed state of a BackupExternalSync.



_Appears in:_
- [BackupExternalSync](#backupexternalsync)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `phase` _string_ | phase of the sync. |  | Enum: [Pending Running Completed PartiallyFailed Failed] <br /> |
| `lastSuccessTime` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.30/#time-v1-meta)_ | lastSuccessTime of a completed sync. |  |  |
| `snapshotsCopied` _integer_ | snapshotsCopied in the last run. |  |  |
| `bytesCopied` _integer_ | bytesCopied (data streamed this run). |  |  |
| `lagSnapshots` _integer_ | lagSnapshots is the count of source snapshots not yet at the destination. |  |  |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.30/#condition-v1-meta) array_ | conditions represent the current state. |  |  |


## BackupLocation



BackupLocation is a namespace user's own off-platform object storage + key.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `crystalbackup.io/v1alpha1` | | |
| `kind` _string_ | `BackupLocation` | | |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.30/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[BackupLocationSpec](#backuplocationspec)_ | spec defines the desired state of BackupLocation |  |  |
| `status` _[BackupLocationStatus](#backuplocationstatus)_ | status defines the observed state of BackupLocation |  |  |


## BackupLocationSpec



BackupLocationSpec is the namespace user's own external object storage and their
own key, isolated by construction (their bucket, credentials and key).

Repository identity (clusterID + s3.endpoint/bucket/prefix) and storage mode are IMMUTABLE
after creation (CEL, update-only), mirroring ClusterBackupLocation: they compose the repository
path, so an edit would silently re-point the location at a different repository, and mode is an
object-lock property fixed at repository creation. clusterID is optional here (it defaults from
the default ClusterBackupLocation), so its rule tolerates absent↔absent but rejects any change.



_Appears in:_
- [BackupLocation](#backuplocation)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `mode` _[LocationMode](#locationmode)_ | mode selects Standard (prunable) or Immutable (object-lock; no prune). | Standard | Enum: [Standard Immutable] <br /> |
| `clusterID` _string_ | clusterID defaults from the default ClusterBackupLocation if unset. |  |  |
| `s3` _[S3Spec](#s3spec)_ | s3 is the user's object storage. |  |  |
| `encryption` _[NamespaceEncryptionSpec](#namespaceencryptionspec)_ | encryption configures the user-owned key (generated in the namespace if unset). |  |  |
| `discovery` _[DiscoverySpec](#discoveryspec)_ | discovery projects Backup objects from this repository into this namespace. |  |  |
| `retention` _[RetentionSpec](#retentionspec)_ | retention is the snapshot retention policy for this location's repository,<br />applied per PVC by a `restic forget` after each successful backup. It lives on<br />the location (not on BackupSchedules) so a single authoritative policy governs<br />the repository, mirroring ClusterBackupLocation. Standard mode only (reported<br />ignored on an Immutable location). |  |  |


## BackupLocationStatus



BackupLocationStatus is the observed state of a BackupLocation.



_Appears in:_
- [BackupLocation](#backuplocation)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `phase` _string_ | phase is a short human-readable summary. |  |  |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.30/#condition-v1-meta) array_ | conditions represent the current state (e.g. Ready, Reachable). |  |  |
| `repositoryRef` _string_ | repositoryRef names the BackupRepository backing this location. |  |  |
| `clusterID` _string_ | clusterID is the EFFECTIVE cluster identifier for this location: spec.clusterID when the<br />user set one, otherwise the value defaulted from the default ClusterBackupLocation.<br />It is recorded here, and never re-resolved once set, because it composes the repository<br />path (restic host + URL). spec.clusterID is immutable precisely so an edit cannot silently<br />re-point a location at a different repository — but leaving it UNSET would reopen that same<br />hole through the back door: the default ClusterBackupLocation can be changed by an admin at<br />any time, and a location that re-derived its cluster ID on every reconcile would follow it,<br />abandoning every snapshot written under the old path. Sticky-once makes the defaulted case<br />as immutable as the explicit one. |  |  |


## BackupRepository



BackupRepository is the operator-internal state and inventory of one restic repository.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `crystalbackup.io/v1alpha1` | | |
| `kind` _string_ | `BackupRepository` | | |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.30/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[BackupRepositorySpec](#backuprepositoryspec)_ | spec defines the desired state of BackupRepository |  |  |
| `status` _[BackupRepositoryStatus](#backuprepositorystatus)_ | status defines the observed state of BackupRepository |  |  |


## BackupRepositorySpec



BackupRepositorySpec is intentionally empty: a BackupRepository is operator-managed
(one per ClusterBackupLocation or namespaced BackupLocation). Its meaningful content
lives in status. It is not user-facing.



_Appears in:_
- [BackupRepository](#backuprepository)



## BackupRepositoryStatus



BackupRepositoryStatus holds repository state and the discovery inventory.



_Appears in:_
- [BackupRepository](#backuprepository)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `location` _[RepositoryLocationRef](#repositorylocationref)_ | location the repository backs. |  |  |
| `scope` _string_ | scope of the backing location. |  | Enum: [Cluster Namespaced] <br /> |
| `ownerNamespace` _string_ | ownerNamespace is set for a namespaced (tenant) repository. |  |  |
| `repositoryURL` _string_ | repositoryURL is the restic repository URL (published for the standalone CLI, R8). |  |  |
| `initialized` _boolean_ | initialized is true once restic init has succeeded. |  |  |
| `mode` _[LocationMode](#locationmode)_ | mode of the repository. |  | Enum: [Standard Immutable] <br /> |
| `keySlots` _string array_ | keySlots present in the repository (cluster: [platform]; tenant: [tenant] (+platform)). |  |  |
| `snapshotCount` _integer_ | snapshotCount in the repository. |  |  |
| `namespacesPresent` _integer_ | namespacesPresent is the count of distinct namespace tags found. |  |  |
| `lastDiscoveryTime` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.30/#time-v1-meta)_ | lastDiscoveryTime is when the repository was last inventoried. |  |  |
| `lastDiscoverySuccess` _boolean_ | lastDiscoverySuccess records whether the last discovery attempt finished cleanly: it<br />listed the repository AND reconciled every snapshot group into a Backup projection. A<br />partial pass — inventory recorded, some namespaces refusing their projection — is FALSE,<br />because what a reader of this field wants to know is whether `kubectl get backups` can<br />still be trusted against the repository, and after a partial pass it cannot.<br />A POINTER, so that "never attempted" is distinguishable from "attempted and failed". The<br />distinction is load-bearing: the DiscoveryFailed alert fires on the metric derived from<br />this field being 0, and a bool defaulting to false would page for every location between<br />its creation and its first scan. |  |  |
| `projectedBackups` _integer_ | projectedBackups is how many snapshot (namespace, run) groups the last scan projected into<br />namespaces that exist — i.e. exactly what `kubectl get backups` lists for this repository<br />(CR lifetime = data lifetime, R26). |  |  |
| `orphanSnapshots` _integer_ | orphanSnapshots is how many snapshot (namespace, run) groups the last scan found whose<br />namespace does NOT exist. They are not projected and are restorable only through a<br />ClusterRestore. A non-zero value is DR data for gone namespaces — the repository outliving<br />the cluster is the design (adr/0009), not a fault — so nothing alerts on it; it is here so<br />an admin can see how much of the repository has no in-cluster representation. |  |  |
| `lastMaintenanceTime` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.30/#time-v1-meta)_ | lastMaintenanceTime is when prune last SUCCEEDED. A failed prune deliberately leaves it<br />alone: the field answers "how long since this repository was actually reclaimed", and the<br />staleness alert depends on a failure not refreshing it. |  |  |
| `lastCheckTime` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.30/#time-v1-meta)_ | lastCheckTime is when restic check last ran — success or failure, unlike<br />lastMaintenanceTime. Paired with lastCheckResult it distinguishes "verified recently and<br />it was bad" from "not verified in weeks", which are different incidents. |  |  |
| `lastCheckResult` _string_ | lastCheckResult of the most recent check. Failed means restic found repository damage<br />(R17) — it is the RepositoryCheckFailed alert, not a transient error. |  | Enum: [Passed Failed] <br /> |
| `approximateSizeBytes` _integer_ | approximateSizeBytes is the repository's PHYSICAL size: the sum of the objects actually<br />stored under its prefix, post-dedup and post-compression. For the shared cluster<br />repository that is the whole cluster's footprint in that bucket, all namespaces together. |  |  |
| `staleLocks` _integer_ | staleLocks is the number of repository lock objects older than restic's 30-minute<br />staleness threshold. Normally zero: a hard-killed mover's lock is cleared by an unlock op.<br />A persistent non-zero value means locks are accumulating faster than they are reaped, and<br />every exclusive operation will eventually stall behind them. |  |  |
| `recentMaintenance` _[MaintenanceRecord](#maintenancerecord) array_ | recentMaintenance is a capped, NEWEST-FIRST history of maintenance attempts. The<br />maintenance Job and its pod are deleted as soon as an op finishes, so this is the only<br />durable record of what ran and why it failed. |  | MaxItems: 10 <br /> |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.30/#condition-v1-meta) array_ | conditions represent the current state. |  |  |


## BackupRunSpec



BackupRunSpec is the run configuration of ONE namespace's backup: what to select, how to
move it, what to exec. It deliberately holds nothing that says WHICH namespaces or WHICH
repository — that is the caller's business and differs per plane.

The split exists because two planes stamp the same execution unit (adr/0017 §5). The cluster
plane inlines it into ClusterBackupRunSpec alongside the fan-out fields (namespaces,
clusterResources, locationRef); the namespace plane's BackupSchedule declares the same fields
on its own tenant-facing surface. Both MATERIALIZE this struct into Backup.spec.run at
creation, so a Backup no longer has to pull its configuration from a parent that may already
be gone.

Two invariants ride on this type, both from adr/0017:
  - Discovery must NEVER own spec.run. A projection's only input is the repository, and none
    of these fields was ever written to restic — an owner that cannot reproduce a field must
    not claim it under server-side apply.
  - It is TENANT-SUBMITTABLE on the namespace plane. Every field added here becomes something
    a namespace user can make the operator do on their behalf; hooks in particular are why
    admission has to check the creator's own exec rights (03-security-and-tenancy.md §5).



_Appears in:_
- [BackupSpec](#backupspec)
- [ClusterBackupRunSpec](#clusterbackuprunspec)
- [ClusterBackupSpec](#clusterbackupspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `pvcSelector` _[PVCSelector](#pvcselector)_ | pvcSelector selects PVCs per namespace (default all). |  |  |
| `includeManifests` _boolean_ | includeManifests also captures namespace manifests (default true). | true |  |
| `manifestOptions` _[ManifestOptions](#manifestoptions)_ | manifestOptions tunes what the manifest dump captures (03-security-and-tenancy.md §10). |  |  |
| `hooks` _[HooksSpec](#hooksspec)_ | hooks are exec hooks around snapshotting (R16). |  |  |
| `maxConcurrentMovers` _integer_ | maxConcurrentMovers caps parallel mover Jobs. The cap is CLUSTER-WIDE (it is checked<br />against every mover Job in the operator namespace, not just this run's), which is why it<br />stays off the tenant-facing BackupSchedule surface: a namespace user setting it would be<br />setting a platform-wide limit. |  |  |
| `backoffLimit` _integer_ | backoffLimit for mover Jobs. |  |  |


## BackupSchedule



BackupSchedule stamps out user Backups on a cron schedule.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `crystalbackup.io/v1alpha1` | | |
| `kind` _string_ | `BackupSchedule` | | |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.30/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[BackupScheduleSpec](#backupschedulespec)_ | spec defines the desired state of BackupSchedule |  |  |
| `status` _[BackupScheduleStatus](#backupschedulestatus)_ | status defines the observed state of BackupSchedule |  |  |


## BackupScheduleSpec



BackupScheduleSpec is a CronJob-style schedule that stamps out Backup objects in
the user's namespace against a namespaced BackupLocation.



_Appears in:_
- [BackupSchedule](#backupschedule)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `locationRef` _[LocalObjectReference](#localobjectreference)_ | locationRef is a BackupLocation in this namespace (required; never a ClusterBackupLocation). |  |  |
| `schedule` _string_ | schedule is a cron expression. |  | MinLength: 1 <br /> |
| `timezone` _string_ | timezone for the cron expression (IANA name). |  |  |
| `paused` _boolean_ | paused suspends new runs. |  |  |
| `jitter` _boolean_ | jitter spreads execution deterministically. |  |  |
| `concurrencyPolicy` _[ConcurrencyPolicy](#concurrencypolicy)_ | concurrencyPolicy governs overlapping runs. | Forbid | Enum: [Forbid Skip] <br /> |
| `startingDeadlineSeconds` _integer_ | startingDeadlineSeconds bounds catch-up after downtime. |  |  |
| `pvcSelector` _[PVCSelector](#pvcselector)_ | pvcSelector selects PVCs (default all). |  |  |
| `includeManifests` _boolean_ | includeManifests also captures namespace manifests (default true). | true |  |
| `manifestOptions` _[ManifestOptions](#manifestoptions)_ | manifestOptions tunes what the manifest dump captures (03-security-and-tenancy.md §10). |  |  |
| `hooks` _[HooksSpec](#hooksspec)_ | hooks are exec hooks around snapshotting (R16). |  |  |
| `backoffLimit` _integer_ | backoffLimit for mover Jobs. |  |  |


## BackupScheduleStatus



BackupScheduleStatus is the observed state of a BackupSchedule.



_Appears in:_
- [BackupSchedule](#backupschedule)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `phase` _string_ | phase is a short human-readable summary. |  |  |
| `lastRunName` _string_ | lastRunName is the most recent Backup. |  |  |
| `lastSuccessTime` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.30/#time-v1-meta)_ | lastSuccessTime is when the last run completed successfully. |  |  |
| `nextScheduleTime` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.30/#time-v1-meta)_ | nextScheduleTime is the next planned run. |  |  |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.30/#condition-v1-meta) array_ | conditions represent the current state. |  |  |


## BackupSpec



BackupSpec is the single unit of execution and the projection of a restorable
backup. Created by a BackupSchedule/ClusterBackup run or by discovery. A
cluster-origin Backup (label crystalbackup.io/origin=cluster) is read-only to users.



_Appears in:_
- [Backup](#backup)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `scheduleRef` _string_ | scheduleRef names the originating schedule (empty for manual/ad-hoc). |  |  |
| `locationRef` _[LocationReference](#locationreference)_ | locationRef is the backup location. On the namespace plane it is a BackupLocation;<br />a cluster-origin Backup references a ClusterBackupLocation. |  |  |
| `run` _[BackupRunSpec](#backuprunspec)_ | run is the run configuration MATERIALIZED by whatever created this Backup — a<br />ClusterBackup fan-out or a BackupSchedule stamp (adr/0017 §5). Identity still lives in<br />the fields above; this is the intent, copied down once at creation rather than pulled<br />from a parent at every reconcile.<br />It is a POINTER because absent and empty must stay distinguishable. Absent means "this<br />object predates materialization, or was projected" — the controller falls back to<br />pulling the parent ClusterBackup, which is the only way an object created before this<br />field existed still executes. An empty struct means "materialized, and every knob was<br />left at its default", which must NOT trigger the fallback.<br />DISCOVERY MUST NEVER SET OR OWN THIS FIELD. A projection is reconstructed from restic<br />snapshots alone, and no selector, manifest option or hook command was ever written to<br />the repository; an SSA field manager that owns a field it cannot reproduce would fight<br />the execution controller over the object forever (adr/0017 §2). Projections leave it<br />absent, and the crystalbackup.io/projected annotation stops them executing anyway. |  |  |


## BackupStatus



BackupStatus is the observed state and the projected restore point.



_Appears in:_
- [Backup](#backup)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `phase` _string_ | phase of the backup. |  | Enum: [Pending SnapshottingHooks Snapshotting Uploading Completed PartiallyCompleted PartiallyFailed Failed] <br /> |
| `backupTime` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.30/#time-v1-meta)_ | backupTime is the point-in-time of the snapshot set. |  |  |
| `completionTime` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.30/#time-v1-meta)_ | completionTime is when the run reached a terminal phase — succeeded or failed alike. Absent<br />while it is still running.<br />It exists because there was no honest answer to "when did this Backup FAIL", and an alert<br />that has to ask that question was reading a timestamp that means something else. Neither of<br />the two candidates works:<br />  - status.backupTime is the point-in-time of the SNAPSHOT SET, and every consumer reads it<br />    that way — the last_success gauge, the schedule's lastSuccessTime roll-up, the restore<br />    controller's point-in-time selection. It is only meaningful for a run that produced<br />    something, and reusing it as a failure clock would make "when was this namespace last<br />    captured" and "when did it last break" the same field.<br />  - the Ready condition's lastTransitionTime is the run's START, not its end. A failing run<br />    goes False(reason=InProgress) → False(reason=Failed), and meta.SetStatusCondition only<br />    refreshes lastTransitionTime when the STATUS changes, not when the reason does. On a<br />    forty-minute backup that is forty minutes of skew — enough to put a failure outside a<br />    one-hour alert window it belongs inside, or inside one it has already left.<br />Written ONCE, on first arrival at a terminal phase, and never moved afterwards: a<br />re-reconcile of an already-terminal object must not restate when it finished.<br />ClusterBackupStatus.completionTime is the run-level sibling and means the same thing. |  |  |
| `manifests` _[ManifestsStatus](#manifestsstatus)_ | manifests records the namespace-manifests snapshot. |  |  |
| `volumes` _[VolumeStatus](#volumestatus) array_ | volumes is the per-PVC result set. |  |  |
| `hooks` _[HookStatus](#hookstatus) array_ | hooks records what each consistency hook did (R16). It is the durable account of the freeze<br />window: which pods were quiesced, whether the release ran, and — when it did not — what an<br />operator has to go and undo by hand. |  | MaxItems: 64 <br /> |
| `postHookAttempts` _integer_ | postHookAttempts counts how many times the post-hook (unfreeze) phase has been tried. Post<br />hooks are retried where pre hooks are not, and the asymmetry is deliberate: a failed pre hook<br />means the snapshot should not be taken, while a failed post hook means an application may<br />still be QUIESCED. Retrying is the difference between a transient blip and an outage. |  |  |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.30/#condition-v1-meta) array_ | conditions represent the current state. |  |  |


## ClusterBackup



ClusterBackup is one DR run that fans out Backup objects into selected namespaces.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `crystalbackup.io/v1alpha1` | | |
| `kind` _string_ | `ClusterBackup` | | |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.30/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[ClusterBackupSpec](#clusterbackupspec)_ | spec defines the desired state of ClusterBackup |  |  |
| `status` _[ClusterBackupStatus](#clusterbackupstatus)_ | status defines the observed state of ClusterBackup |  |  |


## ClusterBackupExternalSync



ClusterBackupExternalSync replicates the shared DR repo to a secondary location (R28).





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `crystalbackup.io/v1alpha1` | | |
| `kind` _string_ | `ClusterBackupExternalSync` | | |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.30/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[ClusterBackupExternalSyncSpec](#clusterbackupexternalsyncspec)_ | spec defines the desired state of ClusterBackupExternalSync |  |  |
| `status` _[ClusterBackupExternalSyncStatus](#clusterbackupexternalsyncstatus)_ | status defines the observed state of ClusterBackupExternalSync |  |  |


## ClusterBackupExternalSyncSpec



ClusterBackupExternalSyncSpec replicates the shared repository to a secondary
ClusterBackupLocation using restic copy, re-encrypted to the destination's own
platform DEK (an independent repo, not a byte clone). R28, adr/0013.



_Appears in:_
- [ClusterBackupExternalSync](#clusterbackupexternalsync)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `sourceLocationRef` _[LocalObjectReference](#localobjectreference)_ | sourceLocationRef is a ClusterBackupLocation (default: the default one). |  |  |
| `destinationLocationRef` _[LocalObjectReference](#localobjectreference)_ | destinationLocationRef is another ClusterBackupLocation with its own key<br />(must differ from source — admission rule 9). |  |  |
| `schedule` _string_ | schedule is a cron expression; empty ⇒ on-demand only. |  |  |
| `timezone` _string_ | timezone for the cron expression (IANA name). |  |  |
| `paused` _boolean_ | paused suspends new syncs. |  |  |
| `mode` _[ExternalSyncMode](#externalsyncmode)_ | mode tracks the source (Mirror) or only adds (AppendOnly, forced on Immutable destinations). | Mirror | Enum: [Mirror AppendOnly] <br /> |
| `selection` _[ExternalSyncSelection](#externalsyncselection)_ | selection narrows the copy by namespace tag; omitted ⇒ whole repository. |  |  |


## ClusterBackupExternalSyncStatus



ClusterBackupExternalSyncStatus is the observed state of a ClusterBackupExternalSync.



_Appears in:_
- [ClusterBackupExternalSync](#clusterbackupexternalsync)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `phase` _string_ | phase of the sync. |  | Enum: [Pending Running Completed PartiallyFailed Failed] <br /> |
| `lastSuccessTime` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.30/#time-v1-meta)_ | lastSuccessTime of a completed sync. |  |  |
| `snapshotsCopied` _integer_ | snapshotsCopied in the last run. |  |  |
| `bytesCopied` _integer_ | bytesCopied (data streamed this run, blob-incremental). |  |  |
| `lagSnapshots` _integer_ | lagSnapshots is the count of source snapshots not yet at the destination. |  |  |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.30/#condition-v1-meta) array_ | conditions represent the current state. |  |  |


## ClusterBackupLocation



ClusterBackupLocation is the platform disaster-recovery object storage: one
shared restic repository for all (or selected) namespaces, tenancy by tag.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `crystalbackup.io/v1alpha1` | | |
| `kind` _string_ | `ClusterBackupLocation` | | |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.30/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[ClusterBackupLocationSpec](#clusterbackuplocationspec)_ | spec defines the desired state of ClusterBackupLocation |  |  |
| `status` _[ClusterBackupLocationStatus](#clusterbackuplocationstatus)_ | status defines the observed state of ClusterBackupLocation |  |  |


## ClusterBackupLocationSpec



ClusterBackupLocationSpec defines the platform object storage, platform key
and maintenance/verification for the one shared cluster DR repository.

The repository IDENTITY and the storage MODE are IMMUTABLE after creation (CEL, update-only):
clusterID + s3.endpoint/bucket/prefix compose the repository path the operator re-derives every
reconcile (status.repositoryURL = <endpoint>/<bucket>/<prefix>/<clusterID>), so editing any of
them post-init would silently re-point the location at a DIFFERENT repository — orphaning every
backup taken so far — with no data movement. mode is fixed too: it is an object-lock property
chosen at repository creation (adr/0005), and allowing Immutable→Standard would defeat the WORM
guarantee (R18) by re-enabling prune/forget on a location provisioned append-only. To change any
of these, create a new location.



_Appears in:_
- [ClusterBackupLocation](#clusterbackuplocation)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `default` _boolean_ | default marks this as the default location; exactly one may be default<br />(enforced by the operator webhook — admission rule 4). |  |  |
| `mode` _[LocationMode](#locationmode)_ | mode selects Standard (prunable) or Immutable (object-lock; no prune). | Standard | Enum: [Standard Immutable] <br /> |
| `clusterID` _string_ | clusterID is the snapshot host and repository path segment (R20, multi-cluster):<br />the shared repo lives at <s3.prefix>/<clusterID>/. |  | MinLength: 1 <br /> |
| `s3` _[S3Spec](#s3spec)_ | s3 is the object storage backing the shared repository. |  |  |
| `encryption` _[ClusterEncryptionSpec](#clusterencryptionspec)_ | encryption configures the platform key (age KEK wrapping the platform DEK). |  |  |
| `discovery` _[DiscoverySpec](#discoveryspec)_ | discovery inventories the repository and projects Backup objects on add and periodically. |  |  |
| `maintenance` _[MaintenanceSpec](#maintenancespec)_ | maintenance configures prune/check windows (Standard mode only). |  |  |
| `immutable` _[ImmutableSpec](#immutablespec)_ | immutable configures object-lock and window rotation (Immutable mode only). |  |  |
| `retention` _[RetentionSpec](#retentionspec)_ | retention is the snapshot retention policy for this location's shared<br />repository, applied per PVC (per restic (host,paths) group) by a `restic<br />forget` after each successful backup. It lives on the location — not on<br />individual ClusterBackup runs or schedules — because a location backs ONE<br />shared restic repository (adr/0009) and `restic forget` operates on the whole<br />repository: a single authoritative policy per location is the only way to keep<br />several runs from fighting over the same snapshots. Standard mode only; on an<br />Immutable location it is reported ignored (RetentionIgnored condition) because<br />object-lock forbids prune/forget until lock expiry. |  |  |


## ClusterBackupLocationStatus



ClusterBackupLocationStatus is the observed state of a ClusterBackupLocation.



_Appears in:_
- [ClusterBackupLocation](#clusterbackuplocation)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `phase` _string_ | phase is a short human-readable summary. |  |  |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.30/#condition-v1-meta) array_ | conditions represent the current state (e.g. Ready, Reachable). |  |  |
| `repositoryRef` _string_ | repositoryRef names the BackupRepository backing this location. |  |  |
| `namespacesProtected` _integer_ | namespacesProtected counts distinct namespaces present in the repository. |  |  |
| `lastDiscoveryTime` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.30/#time-v1-meta)_ | lastDiscoveryTime is when the repository was last inventoried. |  |  |


## ClusterBackupRunSpec



ClusterBackupRunSpec is the run configuration shared by a ClusterBackupSchedule
template and a (manual or fanned-out) ClusterBackup: the cluster-plane fan-out fields plus
the per-namespace BackupRunSpec every child inherits.



_Appears in:_
- [ClusterBackupSpec](#clusterbackupspec)
- [ClusterBackupTemplate](#clusterbackuptemplate)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `locationRef` _[LocalObjectReference](#localobjectreference)_ | locationRef is the ClusterBackupLocation to write to. |  |  |
| `namespaces` _[NamespaceSelector](#namespaceselector)_ | namespaces selects the namespaces to back up (rule 8: one positive form + optional exclude). |  |  |
| `clusterResources` _[ClusterResourceCaptureSpec](#clusterresourcecapturespec)_ | clusterResources captures cluster-scoped objects for full DR (adr/0011). |  |  |
| `pvcSelector` _[PVCSelector](#pvcselector)_ | pvcSelector selects PVCs per namespace (default all). |  |  |
| `includeManifests` _boolean_ | includeManifests also captures namespace manifests (default true). | true |  |
| `manifestOptions` _[ManifestOptions](#manifestoptions)_ | manifestOptions tunes what the manifest dump captures (03-security-and-tenancy.md §10). |  |  |
| `hooks` _[HooksSpec](#hooksspec)_ | hooks are exec hooks around snapshotting (R16). |  |  |
| `maxConcurrentMovers` _integer_ | maxConcurrentMovers caps parallel mover Jobs. The cap is CLUSTER-WIDE (it is checked<br />against every mover Job in the operator namespace, not just this run's), which is why it<br />stays off the tenant-facing BackupSchedule surface: a namespace user setting it would be<br />setting a platform-wide limit. |  |  |
| `backoffLimit` _integer_ | backoffLimit for mover Jobs. |  |  |


## ClusterBackupSchedule



ClusterBackupSchedule stamps out ClusterBackup DR runs on a cron schedule.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `crystalbackup.io/v1alpha1` | | |
| `kind` _string_ | `ClusterBackupSchedule` | | |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.30/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[ClusterBackupScheduleSpec](#clusterbackupschedulespec)_ | spec defines the desired state of ClusterBackupSchedule |  |  |
| `status` _[ClusterBackupScheduleStatus](#clusterbackupschedulestatus)_ | status defines the observed state of ClusterBackupSchedule |  |  |


## ClusterBackupScheduleSpec



ClusterBackupScheduleSpec is a CronJob-style schedule that stamps out
ClusterBackup runs from a template.



_Appears in:_
- [ClusterBackupSchedule](#clusterbackupschedule)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `schedule` _string_ | schedule is a cron expression. |  | MinLength: 1 <br /> |
| `timezone` _string_ | timezone for the cron expression (IANA name). |  |  |
| `paused` _boolean_ | paused suspends new runs. |  |  |
| `jitter` _boolean_ | jitter spreads per-namespace fan-out deterministically (anti thundering herd). |  |  |
| `concurrencyPolicy` _[ConcurrencyPolicy](#concurrencypolicy)_ | concurrencyPolicy governs overlapping runs. | Forbid | Enum: [Forbid Skip] <br /> |
| `startingDeadlineSeconds` _integer_ | startingDeadlineSeconds bounds catch-up after downtime to one run. |  |  |
| `successfulRunsHistoryLimit` _integer_ | successfulRunsHistoryLimit is the number of ClusterBackup run records kept<br />(distinct from snapshot retention). | 10 |  |
| `failedRunsHistoryLimit` _integer_ | failedRunsHistoryLimit is the number of failed run records kept. | 10 |  |
| `template` _[ClusterBackupTemplate](#clusterbackuptemplate)_ | template is the ClusterBackup run configuration (jobTemplate analogue). |  |  |


## ClusterBackupScheduleStatus



ClusterBackupScheduleStatus is the observed state of a ClusterBackupSchedule.



_Appears in:_
- [ClusterBackupSchedule](#clusterbackupschedule)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `phase` _string_ | phase is a short human-readable summary. |  |  |
| `lastRunName` _string_ | lastRunName is the most recent ClusterBackup run. |  |  |
| `lastSuccessTime` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.30/#time-v1-meta)_ | lastSuccessTime is when the last run completed successfully. |  |  |
| `nextScheduleTime` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.30/#time-v1-meta)_ | nextScheduleTime is the next planned run. |  |  |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.30/#condition-v1-meta) array_ | conditions represent the current state. |  |  |


## ClusterBackupSpec



ClusterBackupSpec defines one DR run. It resolves spec.namespaces and creates a
Backup in each matching namespace (linked by label crystalbackup.io/cluster-backup),
and captures cluster-scoped resources (adr/0011). Per-namespace detail lives in the
child Backup objects; this object keeps only aggregate status.



_Appears in:_
- [ClusterBackup](#clusterbackup)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `scheduleRef` _string_ | scheduleRef names the ClusterBackupSchedule that created this run (empty for manual). |  |  |
| `locationRef` _[LocalObjectReference](#localobjectreference)_ | locationRef is the ClusterBackupLocation to write to. |  |  |
| `namespaces` _[NamespaceSelector](#namespaceselector)_ | namespaces selects the namespaces to back up (rule 8: one positive form + optional exclude). |  |  |
| `clusterResources` _[ClusterResourceCaptureSpec](#clusterresourcecapturespec)_ | clusterResources captures cluster-scoped objects for full DR (adr/0011). |  |  |
| `pvcSelector` _[PVCSelector](#pvcselector)_ | pvcSelector selects PVCs per namespace (default all). |  |  |
| `includeManifests` _boolean_ | includeManifests also captures namespace manifests (default true). | true |  |
| `manifestOptions` _[ManifestOptions](#manifestoptions)_ | manifestOptions tunes what the manifest dump captures (03-security-and-tenancy.md §10). |  |  |
| `hooks` _[HooksSpec](#hooksspec)_ | hooks are exec hooks around snapshotting (R16). |  |  |
| `maxConcurrentMovers` _integer_ | maxConcurrentMovers caps parallel mover Jobs. The cap is CLUSTER-WIDE (it is checked<br />against every mover Job in the operator namespace, not just this run's), which is why it<br />stays off the tenant-facing BackupSchedule surface: a namespace user setting it would be<br />setting a platform-wide limit. |  |  |
| `backoffLimit` _integer_ | backoffLimit for mover Jobs. |  |  |


## ClusterBackupStatus



ClusterBackupStatus is the aggregate observed state of a DR run (no unbounded
perNamespace map; failures is capped).



_Appears in:_
- [ClusterBackup](#clusterbackup)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `phase` _string_ | phase of the run. |  | Enum: [Pending Running Completed PartiallyFailed Failed] <br /> |
| `startTime` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.30/#time-v1-meta)_ | startTime is when the run began. |  |  |
| `completionTime` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.30/#time-v1-meta)_ | completionTime is when the run finished. |  |  |
| `namespacesMatched` _integer_ | namespacesMatched by the selector. |  |  |
| `namespacesSucceeded` _integer_ | namespacesSucceeded fully. |  |  |
| `namespacesFailed` _integer_ | namespacesFailed is the number of namespaces whose child Backup RAN and did not fully<br />succeed (phase Failed or PartiallyFailed). It is derived from nothing but those children's<br />own phases, so it can always be reconciled against them.<br />It deliberately does NOT include namespaces this run never backed up — those are<br />namespacesBlocked. Merging the two is the defect this split closes: a run once reported 32<br />failed namespaces over children that read Completed, because a namespace whose coordinate was<br />occupied incremented the same field a genuinely failed backup did. |  |  |
| `namespacesBlocked` _integer_ | namespacesBlocked is the number of matched namespaces this run did NOT back up at all: the<br />Backup coordinate was already occupied by an object the run did not create (a previous run's<br />terminal record, a discovery projection, another plane's Backup), or the child could not be<br />created. status.failures carries the per-namespace reason.<br />It is not a milder failure — nothing protected those namespaces this run, and they degrade<br />the phase exactly as namespacesFailed does. It is a DIFFERENT one: there is no child of this<br />run there whose status could be read, so the run reports that it did not act rather than<br />passing a verdict on an object that is not its own. Alerting on "namespaces this run did not<br />protect" must watch namespacesFailed + namespacesBlocked.<br />namespacesSucceeded + namespacesFailed + namespacesBlocked, plus the namespaces still in<br />flight, account for every namespace the run is answerable for. |  |  |
| `pvcsSucceeded` _integer_ | pvcsSucceeded across all namespaces. |  |  |
| `pvcsFailed` _integer_ | pvcsFailed across all namespaces. |  |  |
| `clusterResourcesCaptured` _integer_ | clusterResourcesCaptured in the kind=cluster-manifests snapshot (adr/0011). A flat mirror<br />of clusterManifests.resourceCount, kept because it is the field the run's headline count<br />has always exposed. |  |  |
| `clusterManifests` _[ManifestsStatus](#manifestsstatus)_ | clusterManifests records the run's one kind=cluster-manifests snapshot (adr/0011 §1). Its<br />presence is also the "capture is terminal" marker the reconcile keys on — set once, it<br />stops a second capture of the same run, exactly as backup.status.manifests does for a<br />namespace. Absent means either the capture is still in flight or the run opted out. |  |  |
| `addedBytes` _integer_ | addedBytes is the deduplicated bytes added by this run. |  |  |
| `failures` _[FailureRecord](#failurerecord) array_ | failures is a capped list of per-namespace failures. |  |  |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.30/#condition-v1-meta) array_ | conditions represent the current state. |  |  |


## ClusterBackupTemplate



ClusterBackupTemplate wraps a ClusterBackupRunSpec as a schedule's jobTemplate analogue.



_Appears in:_
- [ClusterBackupScheduleSpec](#clusterbackupschedulespec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `spec` _[ClusterBackupRunSpec](#clusterbackuprunspec)_ | spec is the ClusterBackup run configuration. |  |  |


## ClusterEncryptionSpec



ClusterEncryptionSpec configures the platform key for a ClusterBackupLocation.



_Appears in:_
- [ClusterBackupLocationSpec](#clusterbackuplocationspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `clusterKEKSecretRef` _[LocalObjectReference](#localobjectreference)_ | clusterKEKSecretRef references the age identity wrapping the platform DEK. |  |  |


## ClusterErasure



ClusterErasure erases a tenant/namespace/PVC from a location (right-to-erasure, R21).





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `crystalbackup.io/v1alpha1` | | |
| `kind` _string_ | `ClusterErasure` | | |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.30/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[ClusterErasureSpec](#clustererasurespec)_ | spec defines the desired state of ClusterErasure |  |  |
| `status` _[ClusterErasureStatus](#clustererasurestatus)_ | status defines the observed state of ClusterErasure |  |  |


## ClusterErasureSpec



ClusterErasureSpec is the right-to-erasure operation: forget+prune a tenant,
namespace or PVC from a ClusterBackupLocation (physical deletion in the shared repo).



_Appears in:_
- [ClusterErasure](#clustererasure)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `locationRef` _[LocalObjectReference](#localobjectreference)_ | locationRef is the ClusterBackupLocation to erase from. |  |  |
| `target` _[ErasureTarget](#erasuretarget)_ | target selects exactly one erasure scope (tenant, namespace, or namespace+pvc). |  |  |
| `confirmation` _string_ | confirmation must equal the target identity (tenant, namespace, or <namespace>/<pvc>; R23).<br />Optional (not required) on purpose, mirroring Restore/ClusterRestore: an ABSENT/empty value is<br />admitted so the controller can park the erasure in phase AwaitingConfirmation until the operator<br />edits it in — the deliberate two-step for the destructive path. A required+MinLength=1 field<br />would be rejected by the API server's structural schema BEFORE the confirmation VAP runs, making<br />the AwaitingConfirmation phase unreachable and contradicting the admission policy (which admits<br />empty and denies only a non-matching non-empty value). |  |  |


## ClusterErasureStatus



ClusterErasureStatus is the observed state of a ClusterErasure. On Immutable
locations the erasure is Blocked until object-lock expiry.



_Appears in:_
- [ClusterErasure](#clustererasure)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `phase` _string_ | phase of the erasure. |  | Enum: [Pending AwaitingConfirmation Running Completed Blocked Failed] <br /> |
| `snapshotsTargeted` _integer_ | snapshotsTargeted is how many snapshots matched this erasure's filter when its scope was<br />measured, BEFORE anything was removed. It is the denominator of the record: the erasure has to<br />count what it is about to destroy while the evidence still exists, and this field is where that<br />count lives. It never changes once written. |  |  |
| `snapshotsForgotten` _integer_ | snapshotsForgotten is how many snapshots this erasure is ESTABLISHED to have removed — never how<br />many it intended to remove. It stays 0 while the erasure is running, and on a terminal object it<br />is either the whole scope (the forget+prune reported success) or the scope minus what a<br />post-failure listing still found.<br />This field is a compliance attestation, not a progress counter: it is what somebody points at to<br />assert that a GDPR erasure, a contractual deletion or a tenant offboarding was carried out. It<br />previously held the PRE-erasure count, so a failed erasure published a failed phase beside<br />"snapshotsForgotten: 10" — a record claiming a destruction that had not happened. Read it<br />together with snapshotsRemaining, which says what is left. |  |  |
| `snapshotsRemaining` _integer_ | snapshotsRemaining is how many snapshots matching this erasure's filter are still in the<br />repository. Zero on a completed erasure; on a failed one it is the work left to do, and it is<br />what makes a partial erasure legible (4 of 10 removed reads forgotten 4, remaining 6).<br />When the erasure failed AND the verification listing could not be read either, this field holds<br />the whole targeted scope: an outcome nobody could establish is reported as an erasure that<br />destroyed nothing, never as an empty repository.<br />snapshotsForgotten + snapshotsRemaining == snapshotsTargeted, except when snapshots matching the<br />target were written AFTER the scope was measured, in which case remaining can exceed the scope<br />and the operator says so in a Warning event rather than adjusting a number to make it balance. |  |  |
| `reclaimedBytes` _integer_ | reclaimedBytes after prune. |  |  |
| `blockedUntil` _string_ | blockedUntil is set on Immutable locations (object-lock expiry). |  |  |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.30/#condition-v1-meta) array_ | conditions represent the current state. |  |  |


## ClusterResourceCaptureSpec



ClusterResourceCaptureSpec configures cluster-scoped resource capture on a run (adr/0011).



_Appears in:_
- [ClusterBackupRunSpec](#clusterbackuprunspec)
- [ClusterBackupSpec](#clusterbackupspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `enabled` _boolean_ | enabled turns on cluster-scoped capture (default true on the cluster plane). | true |  |
| `include` _string array_ | include is an allowlist; empty selects a curated default (CRDs, StorageClasses,<br />IngressClasses, PriorityClasses, RuntimeClasses, non-system ClusterRoles/Bindings, PVs). |  |  |
| `exclude` _string array_ | exclude is a denylist applied after include (default: system:* names). |  |  |
| `labelSelector` _[LabelSelector](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.30/#labelselector-v1-meta)_ | labelSelector is an optional extra filter. |  |  |


## ClusterResourceRestoreSpec



ClusterResourceRestoreSpec selects cluster-scoped resources to restore (adr/0011).
Omitted ⇒ nothing cluster-scoped is restored; admin-only.



_Appears in:_
- [ClusterRestoreSpec](#clusterrestorespec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `include` _string array_ | include is a list of <group>/<Kind>[/<name>] globs. |  |  |
| `exclude` _string array_ | exclude is applied after include. |  |  |


## ClusterRestore



ClusterRestore restores a namespace from a repository coordinate (admin, R14).





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `crystalbackup.io/v1alpha1` | | |
| `kind` _string_ | `ClusterRestore` | | |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.30/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[ClusterRestoreSpec](#clusterrestorespec)_ | spec defines the desired state of ClusterRestore |  |  |
| `status` _[ClusterRestoreStatus](#clusterrestorestatus)_ | status defines the observed state of ClusterRestore |  |  |


## ClusterRestoreSource



ClusterRestoreSource identifies a repository coordinate for an admin restore.
Exactly one of backup and time must be set (CEL).



_Appears in:_
- [ClusterRestoreSpec](#clusterrestorespec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `locationRef` _[LocalObjectReference](#localobjectreference)_ | locationRef is the source ClusterBackupLocation. |  |  |
| `namespace` _string_ | namespace is the origin namespace (repository tag filter). |  | MaxLength: 63 <br />MinLength: 1 <br /> |
| `backup` _string_ | backup names a run; alternatively use time. |  | MaxLength: 253 <br /> |
| `time` _string_ | time selects "latest" or an RFC3339 instant. |  | MaxLength: 64 <br /> |


## ClusterRestoreSpec



ClusterRestoreSpec restores any namespace from a repository coordinate. It works
even when the source namespace no longer exists and can create the target.
Uses the shared restore selection model (mode + resources/volumes lists).

The execution identity — source, mode and the target namespace — is IMMUTABLE after
creation (CEL): the controller re-derives them every pass, and a mid-run edit would mix
two repository coordinates (or drift the owner labels off the objects already created
under the old target namespace). confirmation and the selection lists stay mutable, as
do createNamespace/storageClassMapping (they only shape volumes not yet started).
(`namespace` is a CEL reserved word: schema-typed CRD rules must select it as
`__namespace__`, unlike the chart's dynamically-typed VAP expressions.)



_Appears in:_
- [ClusterRestore](#clusterrestore)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `source` _[ClusterRestoreSource](#clusterrestoresource)_ | source is the repository coordinate to restore from. |  |  |
| `target` _[ClusterRestoreTarget](#clusterrestoretarget)_ | target is where the restore lands. |  |  |
| `mode` _[RestoreMode](#restoremode)_ | mode selects Recreate or Overwrite (default Overwrite). | Overwrite | Enum: [Recreate Overwrite] <br /> |
| `resources` _[ResourceSelectorItem](#resourceselectoritem) array_ | resources selects manifests to restore (omitted with volumes ⇒ whole namespace). Bounded to<br />match the volumes cap — an unbounded selector array is an etcd/object-size smell.<br />NOTE: no `omitempty`. A PRESENT-but-empty list means "restore nothing of this kind",<br />while an omitted one means "everything" (spec/02-api.md § Restore selection model), and<br />`omitempty` erases exactly that difference on the way OUT: a Go client sending an empty<br />slice would emit no field at all, and the operator would read it back as omitted and<br />restore the whole namespace. That is the failure mode this model must never have —<br />crystalctl's `--data-only` writes `resources: []`, and it would widen to everything in<br />Overwrite or Recreate mode against a live namespace. |  | MaxItems: 128 <br /> |
| `volumes` _[VolumeSelectorItem](#volumeselectoritem) array_ | volumes selects PVCs (and optionally files) to restore. Bounded so the per-item CEL<br />cost stays within the apiserver's per-CRD budget.<br />No `omitempty`, for the same reason as resources above. |  | MaxItems: 128 <br /> |
| `clusterResources` _[ClusterResourceRestoreSpec](#clusterresourcerestorespec)_ | clusterResources selects cluster-scoped resources to restore (omitted ⇒ none; adr/0011). |  |  |
| `confirmation` _string_ | confirmation must equal target.namespace when the operation modifies existing objects (R23). |  |  |
| `dryRun` _boolean_ | dryRun runs the whole pipeline — ordering, selection, mapping, mode resolution — with<br />server-side dry-run applies, persists nothing, and writes the plan to<br />status.resources. On the cluster plane this matters most: a ClusterRestore can<br />recreate CRDs and cluster RBAC, and seeing the plan first is the difference between a<br />reviewed DR and a hopeful one (04-manifest-backup.md §5.4). |  |  |


## ClusterRestoreStatus



ClusterRestoreStatus is the observed state of a ClusterRestore.



_Appears in:_
- [ClusterRestore](#clusterrestore)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `phase` _string_ | phase of the restore. |  | Enum: [Pending AwaitingConfirmation Running Completed PartiallyFailed Failed] <br /> |
| `restoredResources` _integer_ | restoredResources count. |  |  |
| `resources` _[RestoreResourcesStatus](#restoreresourcesstatus)_ | resources is the per-resource detail of the manifest half (04-manifest-backup.md §5.4).<br />Under dryRun it holds the PLAN rather than an observed outcome. |  |  |
| `plannedVolumes` _integer_ | plannedVolumes is how many volumes this restore's plan covers: the denominator of the two<br />counters below. Written on every pass, so restoredVolumes/plannedVolumes answers "how far<br />along" while the restore is still running.<br />It is the intersection of spec.volumes with what the repository coordinate actually holds, so it<br />can be smaller than either — and it is 0 for a volumes-free restore (resources[] or<br />clusterResources only), which is a valid restore, not an empty one. |  |  |
| `restoredVolumes` _integer_ | restoredVolumes is how many planned volumes have their data back. It is written on EVERY pass,<br />not only the terminal one: a ClusterRestore is what runs during a disaster recovery, and a<br />counter that reads 0 until the whole run finishes cannot answer "is it moving".<br />VOLUMES ONLY. The manifest halves of a restore — resources[] and clusterResources — are counted<br />in restoredResources and resources.failedCount, and the terminal PHASE rolls up ALL of them, so<br />a restore whose every volume landed can still read Failed or PartiallyFailed because<br />cluster-scoped resources failed to apply, with failedVolumes at 0. That asymmetry is deliberate<br />(the halves fail for unrelated reasons and are driven independently) and is spelled out here<br />because this is where a reader meets it. |  |  |
| `failedVolumes` _integer_ | failedVolumes is how many planned volumes settled WITHOUT their data — a failed mover, an<br />exposure that never became mountable, an unsupported target, or a volume whose error budget ran<br />out. It is the answer to "what did not come back", and it is written on every pass.<br />plannedVolumes - restoredVolumes - failedVolumes is what is still in flight. On a terminal<br />restore that difference is 0 by construction: the restore does not go terminal until every<br />planned volume has settled. |  |  |
| `restoredBytes` _integer_ | restoredBytes total. |  |  |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.30/#condition-v1-meta) array_ | conditions represent the current state. |  |  |


## ClusterRestoreTarget



ClusterRestoreTarget is where an admin restore lands.



_Appears in:_
- [ClusterRestoreSpec](#clusterrestorespec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `namespace` _string_ | namespace to restore into. |  | MaxLength: 63 <br />MinLength: 1 <br /> |
| `createNamespace` _boolean_ | createNamespace creates the target namespace if absent (non-destructive). |  |  |
| `storageClassMapping` _object (keys:string, values:string)_ | storageClassMapping rewrites storageClassName on restore. |  |  |


## ConcurrencyPolicy

_Underlying type:_ _string_

ConcurrencyPolicy governs overlapping scheduled runs.

_Validation:_
- Enum: [Forbid Skip]

_Appears in:_
- [BackupScheduleSpec](#backupschedulespec)
- [ClusterBackupScheduleSpec](#clusterbackupschedulespec)



## DiscoverySpec



DiscoverySpec configures repository→Backup projection.



_Appears in:_
- [BackupLocationSpec](#backuplocationspec)
- [ClusterBackupLocationSpec](#clusterbackuplocationspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `enabled` _boolean_ | enabled turns on inventory and projection of Backup objects from the repository.<br />A POINTER, like ClusterResourceCaptureSpec.Enabled, and for the same reason: this defaults to<br />TRUE, and a plain bool cannot express all three states a default-true field needs. With<br />`omitempty` encoding/json drops `false`, so no Go client could ever disable discovery (the<br />API server would substitute the default) even though `kubectl apply` with `enabled: false`<br />worked. Without `omitempty` the zero value serialises as an explicit `false`, so every caller<br />that left the struct alone would silently switch discovery OFF. Only nil-means-unset<br />distinguishes "I did not say" from "I said no". | true |  |
| `interval` _[Duration](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.30/#duration-v1-meta)_ | interval between periodic discovery passes. | 1h |  |


## ErasureTarget



ErasureTarget selects exactly one erasure scope (tenant, namespace, or namespace+pvc).



_Appears in:_
- [ClusterErasureSpec](#clustererasurespec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `tenant` _string_ | tenant erases all snapshots tagged tenant=<t>. |  |  |
| `namespace` _string_ | namespace erases all snapshots tagged namespace=<ns>. |  |  |
| `pvc` _string_ | pvc, together with namespace, narrows erasure to a single PVC. |  |  |


## ExternalSyncMode

_Underlying type:_ _string_

ExternalSyncMode governs how a sync tracks its source.

_Validation:_
- Enum: [Mirror AppendOnly]

_Appears in:_
- [BackupExternalSyncSpec](#backupexternalsyncspec)
- [ClusterBackupExternalSyncSpec](#clusterbackupexternalsyncspec)

| Field | Description |
| --- | --- |
| `Mirror` | ExternalSyncModeMirror tracks the source and forgets extras at the destination.<br /> |
| `AppendOnly` | ExternalSyncModeAppendOnly only adds snapshots (forced on Immutable destinations).<br /> |


## ExternalSyncSelection



ExternalSyncSelection narrows a ClusterBackupExternalSync (omitted ⇒ whole repo).



_Appears in:_
- [ClusterBackupExternalSyncSpec](#clusterbackupexternalsyncspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `namespaces` _[NamespaceSelector](#namespaceselector)_ | namespaces narrows the copy by namespace tag. |  |  |


## FailureRecord



FailureRecord is one capped failure entry on a ClusterBackup (no unbounded perNamespace map).



_Appears in:_
- [ClusterBackupStatus](#clusterbackupstatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `namespace` _string_ | namespace where the failure occurred. |  |  |
| `backup` _string_ | backup is the child Backup name. |  |  |
| `message` _string_ | message is a short human-readable cause. |  |  |


## Hook



Hook is an exec hook run in a selected pod around snapshotting (R16). Candidate pods are those
MOUNTING the volumes being snapshotted, always in the CR's own namespace; podSelector narrows
that set further, and an empty selector means "every pod holding this data".



_Appears in:_
- [HooksSpec](#hooksspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `podSelector` _[LabelSelector](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.30/#labelselector-v1-meta)_ | podSelector selects the pod(s) to exec into, among those mounting the backed-up volumes.<br />Empty matches them all. |  |  |
| `container` _string_ | container name to exec into. Empty uses the pod's FIRST container. |  |  |
| `command` _string array_ | command to run, as an argv. It is exec'd directly, NOT through a shell, so pipes and<br />redirections need an explicit interpreter (e.g. ["sh","-c","..."]). |  | MinItems: 1 <br /> |
| `timeout` _[Duration](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.30/#duration-v1-meta)_ | timeout for the hook, bounding how long the application stays quiesced. It defaults to 30s<br />rather than to the zero value on purpose: this is a non-pointer duration, so an omitted<br />timeout would arrive as 0s, and a literal zero deadline expires immediately — every hook<br />that did not set one would fail before it ran. | 30s |  |
| `onError` _[HookErrorPolicy](#hookerrorpolicy)_ | onError governs whether a failure fails the backup or is tolerated. | Fail | Enum: [Fail Continue] <br /> |


## HookErrorPolicy

_Underlying type:_ _string_

HookErrorPolicy governs behaviour when a hook fails.

_Validation:_
- Enum: [Fail Continue]

_Appears in:_
- [Hook](#hook)
- [HookStatus](#hookstatus)

| Field | Description |
| --- | --- |
| `Fail` | HookErrorPolicyFail aborts the backup when the hook fails. The default, because a<br />pre-snapshot hook exists precisely to make the snapshot trustworthy: if the quiesce did not<br />happen, a snapshot taken anyway is a backup that LOOKS application-consistent and is not.<br /> |
| `Continue` | HookErrorPolicyContinue records the failure and proceeds — for best-effort hooks whose<br />absence degrades consistency without invalidating the backup.<br /> |


## HookResult

_Underlying type:_ _string_

HookResult is the outcome of one hook execution.

_Validation:_
- Enum: [Succeeded Failed Skipped]

_Appears in:_
- [HookStatus](#hookstatus)

| Field | Description |
| --- | --- |
| `Succeeded` | HookSucceeded means the command exited 0 within its timeout.<br /> |
| `Failed` | HookFailed means it exited non-zero, timed out, or could not be started.<br /> |
| `Skipped` | HookSkipped means an earlier hook in the same phase failed with onError=Fail, so this one<br />never ran. Recorded rather than omitted: a list showing three of five hooks invites the<br />reader to assume the missing two passed.<br /> |


## HookStatus



HookStatus is one hook execution's durable record.



_Appears in:_
- [BackupStatus](#backupstatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `phase` _string_ | phase the hook ran in: "pre" (before snapshotting) or "post" (after every snapshot was cut). |  | Enum: [pre post] <br /> |
| `pod` _string_ | pod the command ran in, and container within it. |  |  |
| `container` _string_ |  |  |  |
| `source` _string_ | source records where the hook was declared: an annotation on the pod itself, or the CR spec. |  | Enum: [annotation spec] <br /> |
| `result` _[HookResult](#hookresult)_ | result of the execution. |  | Enum: [Succeeded Failed Skipped] <br /> |
| `onError` _[HookErrorPolicy](#hookerrorpolicy)_ | onError is the failure policy that was IN EFFECT for this execution, resolved from the hook<br />that produced it — the spec's onError, or the pod's `on-error` annotation when annotations<br />won for that pod.<br />It is recorded because it is what makes a `Failed` entry legible, to a reader and to the<br />controller alike. The same result means two opposite things: under `Fail` the run was aborted<br />and there is no snapshot, under `Continue` the run PROCEEDED and its restore point is<br />crash-consistent only. Without the policy beside the result, a `Failed` pre entry on a<br />`Completed` Backup is unexplainable in the one place an operator trusts to be literal.<br />The controller needs it for a harder reason: nothing about the freeze window is kept in<br />memory (see status.hooks' own doc), so the decision "abort, or proceed degraded" is taken on<br />a LATER reconcile from this record alone. A status that dropped the policy — as 0.6.4's did —<br />erases the user's answer at the moment it is written down, which is how `onError: Continue`<br />came to fail the run it was asked to let through.<br />EMPTY IS READ AS FAIL, matching HookErrorPolicy's own zero-value rule, and that is what makes<br />an upgrade over a Backup already mid-freeze-window safe: entries written by an operator that<br />did not record the policy abort exactly as they did before, rather than silently becoming<br />tolerated by a newer binary. |  | Enum: [Fail Continue] <br /> |
| `message` _string_ | message carries the failure, including the command's own stderr — usually the whole<br />diagnosis. Empty on success. |  | MaxLength: 1024 <br /> |
| `finishedAt` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.30/#time-v1-meta)_ | finishedAt is when the execution ended. |  |  |


## HooksSpec



HooksSpec groups pre/post hooks and annotation honouring (R16).



_Appears in:_
- [BackupRunSpec](#backuprunspec)
- [BackupScheduleSpec](#backupschedulespec)
- [ClusterBackupRunSpec](#clusterbackuprunspec)
- [ClusterBackupSpec](#clusterbackupspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `honorAnnotations` _boolean_ | honorAnnotations enables the crystalbackup.io/pre-backup-* and post-backup-* pod<br />annotations. It is opt-in because it delegates WHAT the operator execs to whoever can<br />annotate a pod in the backed-up namespace. When a pod carries them they take precedence and<br />the hooks below are skipped for that pod — never merged (the same rule Velero applies). |  |  |
| `pre` _[Hook](#hook) array_ | pre hooks run before snapshotting. |  |  |
| `post` _[Hook](#hook) array_ | post hooks run after snapshotting. |  |  |
| `serviceAccountName` _string_ | serviceAccountName is a ServiceAccount IN THE BACKED-UP NAMESPACE that the operator<br />IMPERSONATES to run these hooks. It is how the confinement invariant of<br />03-security-and-tenancy.md §5 — "users can only make the platform run commands they can<br />already run themselves" — stops being prose and becomes something the API server enforces.<br />The namespace is NOT a field and never will be: it is always the namespace being backed up,<br />derived from the pod the hook targets. A settable namespace would be a cross-tenant hole by<br />construction, so the only degree of freedom is WHICH ServiceAccount inside the tenant's own<br />namespace — one they (or an admin) created and granted `create pods/exec` on.<br />Empty means "run as the operator itself", which is the pre-M5 behaviour and stays available<br />on the CLUSTER plane, where hooks are admin-authored. On the NAMESPACE plane it is required:<br />a tenant-authored hook with no identity to run as is rejected rather than silently escalated<br />to the operator's own privileges.<br />It governs annotation-sourced hooks too (honorAnnotations). A pod annotation supplies the<br />command, never the identity — so even there, the command runs with exactly the rights the<br />namespace granted this ServiceAccount. |  |  |


## ImmutableSpec



ImmutableSpec configures Immutable-mode repositories (object-lock; no prune).



_Appears in:_
- [ClusterBackupLocationSpec](#clusterbackuplocationspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `objectLockMode` _[ObjectLockMode](#objectlockmode)_ | objectLockMode selects the WORM enforcement. | Governance | Enum: [Governance Compliance AppendOnlyProxy] <br /> |
| `rotationPeriod` _[Duration](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.30/#duration-v1-meta)_ | rotationPeriod is the window-repo rotation period (object-lock repos cannot prune). | 720h |  |


## LocalObjectReference



LocalObjectReference references another object by name, resolved within the
same namespace for namespaced kinds or the operator namespace for cluster kinds.



_Appears in:_
- [BackupExternalSyncSpec](#backupexternalsyncspec)
- [BackupScheduleSpec](#backupschedulespec)
- [ClusterBackupExternalSyncSpec](#clusterbackupexternalsyncspec)
- [ClusterBackupRunSpec](#clusterbackuprunspec)
- [ClusterBackupSpec](#clusterbackupspec)
- [ClusterEncryptionSpec](#clusterencryptionspec)
- [ClusterErasureSpec](#clustererasurespec)
- [ClusterRestoreSource](#clusterrestoresource)
- [NamespaceEncryptionSpec](#namespaceencryptionspec)
- [S3Spec](#s3spec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | name of the referent. |  | MaxLength: 253 <br />MinLength: 1 <br /> |


## LocationMode

_Underlying type:_ _string_

LocationMode selects the durability/erasure semantics of a repository.

_Validation:_
- Enum: [Standard Immutable]

_Appears in:_
- [BackupLocationSpec](#backuplocationspec)
- [BackupRepositoryStatus](#backuprepositorystatus)
- [ClusterBackupLocationSpec](#clusterbackuplocationspec)

| Field | Description |
| --- | --- |
| `Standard` | LocationModeStandard allows prune; erasure is immediate.<br /> |
| `Immutable` | LocationModeImmutable uses object-lock; no prune, erasure deferred to lock expiry.<br /> |


## LocationReference



LocationReference references a backup location. On the namespace plane the
kind defaults to BackupLocation; a cluster-origin Backup may reference a
ClusterBackupLocation.



_Appears in:_
- [BackupSpec](#backupspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `kind` _string_ | kind of the referenced location. | BackupLocation | Enum: [BackupLocation ClusterBackupLocation] <br /> |
| `name` _string_ | name of the referenced location. |  | MinLength: 1 <br /> |


## MaintenanceRecord



MaintenanceRecord is one completed repository-maintenance attempt, kept so an operator can see
WHY a repository is unhealthy without going to the operator log — the maintenance Job and its
pod are deleted as soon as the op finishes (no ownerReference is possible: the Job lives in the
operator namespace, the triggering object does not), so this record is the only durable trace.



_Appears in:_
- [BackupRepositoryStatus](#backuprepositorystatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `operation` _string_ | operation that ran, e.g. "prune" or "check". |  |  |
| `startTime` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.30/#time-v1-meta)_ | startTime is when the operation was enqueued on the repository's exclusive lane. It<br />deliberately INCLUDES the wait for its turn: "the prune took three hours" is the number an<br />operator needs, and the lane is exactly where contention shows up. |  |  |
| `completionTime` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.30/#time-v1-meta)_ | completionTime is when it finished. Absent while it is still running. |  |  |
| `result` _[MaintenanceResult](#maintenanceresult)_ | result of the attempt. |  | Enum: [Succeeded Failed] <br /> |
| `message` _string_ | message carries the failure reason, truncated. Empty on success. |  | MaxLength: 512 <br /> |


## MaintenanceResult

_Underlying type:_ _string_

MaintenanceResult is the outcome of one repository-maintenance attempt.

_Validation:_
- Enum: [Succeeded Failed]

_Appears in:_
- [MaintenanceRecord](#maintenancerecord)

| Field | Description |
| --- | --- |
| `Succeeded` | MaintenanceSucceeded means the maintenance Job ran to completion with exit status 0.<br /> |
| `Failed` | MaintenanceFailed means the Job failed, timed out, or never got its turn on the lane.<br /> |


## MaintenanceSpec



MaintenanceSpec configures Standard-mode repository maintenance. Immutable locations never
prune (object-lock forbids it, adr/0005) — admission rule 6 denies pruneSchedule there.

The two value fields carry restic's OWN grammars, pinned here as CRD patterns so a typo is
rejected at apply time instead of becoming a maintenance Job that starts, pulls an image, opens
the repository and only then dies on a flag parse error. internal/restic re-validates them at
argv-build time: these patterns bind only what this API accepts, and the repository contract
belongs to the package that owns the argv.



_Appears in:_
- [ClusterBackupLocationSpec](#clusterbackuplocationspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `pruneSchedule` _string_ | pruneSchedule (cron) for the repository-wide exclusive prune window. One shared cluster<br />repository means ONE cluster-wide prune window (adr/0009) during which no namespace can<br />start a backup — schedule it off-peak. |  | MinLength: 1 <br /> |
| `pruneMaxRepackSize` _string_ | pruneMaxRepackSize caps repacking per prune run (e.g. "50G") — the practical bound on how<br />long that exclusive window lasts. Empty means restic's default: repack whatever the run<br />needs. A byte count with an optional k/K, m/M, g/G or t/T suffix. |  | Pattern: `^[0-9]+(\.[0-9]+)?[kKmMgGtT]?$` <br /> |
| `checkSchedule` _string_ | checkSchedule (cron) for restic check. |  | MinLength: 1 <br /> |
| `timezone` _string_ | timezone (IANA name) both cron expressions are interpreted in. Empty means UTC. It matters<br />more here than it looks: the whole point of pruneSchedule is to put a cluster-wide exclusive<br />window somewhere off-peak, and "off-peak" is a local-time notion — "0 3 * * *" read as UTC<br />lands in the middle of the working day for half the world. |  |  |
| `checkReadDataSubset` _string_ | checkReadDataSubset is how much pack data each check actually READS (R17). Empty means a<br />structural check only, which catches a missing or truncated object but never a silently<br />corrupted one — its bytes rotted while its name and length stayed right. Accepts "n/t" for<br />a specific part, a percentage like "5%" or "2.5%", or a size with a k/K, m/M, g/G or t/T<br />suffix. |  | Pattern: `^([0-9]+/[0-9]+\|[0-9]+(\.[0-9]+)?%\|[0-9]+(\.[0-9]+)?[kKmMgGtT]?)$` <br /> |


## ManifestOptions



ManifestOptions tunes what the namespace manifest dump captures
(03-security-and-tenancy.md §10).



_Appears in:_
- [BackupRunSpec](#backuprunspec)
- [BackupScheduleSpec](#backupschedulespec)
- [ClusterBackupRunSpec](#clusterbackuprunspec)
- [ClusterBackupSpec](#clusterbackupspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `excludeSecretData` _boolean_ | excludeSecretData stores Secret manifests with data/stringData stripped and the<br />annotation crystalbackup.io/secret-data-excluded: "true". Restore recreates them<br />empty, carrying the same annotation, so a workload that needs the values fails<br />visibly instead of silently coming back with wrong ones.<br />This is an opt-out from a deliberate default: a full namespace recovery (R15) needs<br />the Secrets, and the control on them is the repository key — admin-only on the shared<br />DR repo, the user's own on a user location. Excluding the data trades recoverability<br />for a smaller blast radius if that key is ever compromised. |  |  |


## ManifestsStatus



ManifestsStatus records the namespace-manifests snapshot within a Backup.



_Appears in:_
- [BackupStatus](#backupstatus)
- [ClusterBackupStatus](#clusterbackupstatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `snapshotID` _string_ | snapshotID of the manifests snapshot. |  |  |
| `resourceCount` _integer_ | resourceCount captured. |  |  |


## NamespaceEncryptionSpec



NamespaceEncryptionSpec configures the user key for a BackupLocation.

One field, and that is the design. A namespace-plane repository has exactly ONE key slot —
the user's — and there is deliberately no way to ask for a second (adr/0004, 2026-07-28
amendment). An operator slot would be a password held in crystal-backup-system that keeps
working after the user rotates their key or deletes their Secret, and because removing a
restic key slot does not rotate the master key, one they could never take back. The guarantee
that platform access ends when the user's key does is bought by the mechanism not existing,
rather than by a webhook that a flag or a future maintainer could switch off.



_Appears in:_
- [BackupLocationSpec](#backuplocationspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `repositoryPasswordSecretRef` _[LocalObjectReference](#localobjectreference)_ | repositoryPasswordSecretRef references the user-owned restic password Secret<br />(same namespace). If omitted the operator generates one and stores it in the<br />user's namespace (their key, their reversibility). |  |  |


## NamespaceSelector



NamespaceSelector selects namespaces for cluster-plane backup. At least one
positive form (matchNames/matchLabels/matchExpressions/regexp) must be set;
exclude is applied last (admission rule 8).



_Appears in:_
- [ClusterBackupRunSpec](#clusterbackuprunspec)
- [ClusterBackupSpec](#clusterbackupspec)
- [ExternalSyncSelection](#externalsyncselection)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `matchNames` _string array_ | matchNames is a list of glob patterns on namespace names. |  |  |
| `matchLabels` _object (keys:string, values:string)_ | matchLabels selects namespaces by label. |  |  |
| `matchExpressions` _[LabelSelectorRequirement](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.30/#labelselectorrequirement-v1-meta) array_ | matchExpressions selects namespaces by label expressions. |  |  |
| `regexp` _string_ | regexp matches namespace names (power tool; see adr/0009). |  |  |
| `exclude` _string array_ | exclude is a list of glob patterns removed after the positive match. |  |  |


## ObjectLockMode

_Underlying type:_ _string_

ObjectLockMode selects the immutability enforcement mechanism.

_Validation:_
- Enum: [Governance Compliance AppendOnlyProxy]

_Appears in:_
- [ImmutableSpec](#immutablespec)



## PVCSelector



PVCSelector selects PersistentVolumeClaims within a namespace (empty ⇒ all).



_Appears in:_
- [BackupRunSpec](#backuprunspec)
- [BackupScheduleSpec](#backupschedulespec)
- [ClusterBackupRunSpec](#clusterbackuprunspec)
- [ClusterBackupSpec](#clusterbackupspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `matchLabels` _object (keys:string, values:string)_ | matchLabels selects PVCs by label. |  |  |
| `include` _string array_ | include is a list of PVC-name globs to add. |  |  |
| `exclude` _string array_ | exclude is a list of PVC-name globs to remove. |  |  |


## RepositoryLocationRef



RepositoryLocationRef identifies the location a BackupRepository backs.



_Appears in:_
- [BackupRepositoryStatus](#backuprepositorystatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `kind` _string_ | kind of the referenced location. |  | Enum: [ClusterBackupLocation BackupLocation] <br /> |
| `name` _string_ | name of the referenced location. |  |  |


## ResourceSelectorItem



ResourceSelectorItem selects manifests to restore (AND within an item, OR between items).



_Appears in:_
- [ClusterRestoreSpec](#clusterrestorespec)
- [RestoreSpec](#restorespec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `selector` _[LabelSelector](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.30/#labelselector-v1-meta)_ | selector matches resources by label. |  |  |
| `include` _string array_ | include is a list of <group>/<Kind>[/<name>] globs. |  |  |
| `exclude` _string array_ | exclude removes from what selector and include selected, so an item reads<br />"these kinds, minus these". Applied after both (04-manifest-backup.md §5.4).<br />The backup-time default exclusions already applied at capture and cannot be<br />re-included here. |  |  |


## Restore



Restore is a self-service restore of the user's own namespace.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `crystalbackup.io/v1alpha1` | | |
| `kind` _string_ | `Restore` | | |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.30/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[RestoreSpec](#restorespec)_ | spec defines the desired state of Restore |  |  |
| `status` _[RestoreStatus](#restorestatus)_ | status defines the observed state of Restore |  |  |


## RestoreMode

_Underlying type:_ _string_

RestoreMode selects how a restore reconciles existing objects and data.

_Validation:_
- Enum: [Recreate Overwrite]

_Appears in:_
- [ClusterRestoreSpec](#clusterrestorespec)
- [RestoreSpec](#restorespec)

| Field | Description |
| --- | --- |
| `Recreate` | RestoreModeRecreate deletes selected existing resources then recreates from backup.<br /> |
| `Overwrite` | RestoreModeOverwrite applies create-or-update and keeps objects absent from the backup.<br /> |


## RestoreResourceEntry



RestoreResourceEntry is one resource's outcome in a manifest restore.



_Appears in:_
- [RestoreResourcesStatus](#restoreresourcesstatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `group` _string_ | group is the API group ("" for the core group). |  |  |
| `kind` _string_ | kind is the PascalCase kind. |  |  |
| `name` _string_ | name of the object. |  |  |
| `outcome` _[RestoreResourceOutcome](#restoreresourceoutcome)_ | outcome of the apply. In a dry run this is the PLANNED action, not an observed one. |  | Enum: [Created Configured Recreated Failed] <br /> |
| `reason` _string_ | reason carries the server's error when outcome is Failed (a nodePort collision, a<br />finalizer holding a Recreate delete, a CRD absent in the target cluster). |  |  |
| `changed` _string array_ | changed lists the field paths a server-side apply modified (Overwrite). Capped at<br />MaxRestoreChangedPaths per entry. |  | MaxItems: 20 <br /> |


## RestoreResourceOutcome

_Underlying type:_ _string_

RestoreResourceOutcome is what happened to one manifest during a restore.

_Validation:_
- Enum: [Created Configured Recreated Failed]

_Appears in:_
- [RestoreResourceEntry](#restoreresourceentry)

| Field | Description |
| --- | --- |
| `Created` | RestoreResourceCreated means the object did not exist and was created.<br /> |
| `Configured` | RestoreResourceConfigured means an existing object was server-side applied (Overwrite).<br /> |
| `Recreated` | RestoreResourceRecreated means an existing object was deleted then created (Recreate).<br /> |
| `Failed` | RestoreResourceFailed means the object could not be applied; the restore continued.<br /> |


## RestoreResourcesStatus



RestoreResourcesStatus is the per-resource detail of a manifest restore
(04-manifest-backup.md §5.4). Additive to the restoredResources counter of 02-api.md.



_Appears in:_
- [ClusterRestoreStatus](#clusterrestorestatus)
- [RestoreStatus](#restorestatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `failedCount` _integer_ | failedCount is how many resources failed to apply. A restore reports per-resource<br />failures and continues; it does not abort on the first one. |  |  |
| `truncated` _boolean_ | truncated is true when entries were dropped to stay within the caps, so a reader can<br />tell an empty tail from a complete report. |  |  |
| `entries` _[RestoreResourceEntry](#restoreresourceentry) array_ | entries records non-trivial outcomes only — a plain Created is the expected case and<br />would drown the interesting ones. Capped at MaxRestoreResourceEntries. |  | MaxItems: 100 <br /> |


## RestoreSource



RestoreSource identifies a Backup in the same namespace (self-service Restore).
Exactly one of backup and time must be set (CEL); origin only refines time.



_Appears in:_
- [RestoreSpec](#restorespec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `backup` _string_ | backup names a Backup in this namespace. |  | MaxLength: 253 <br /> |
| `time` _string_ | time selects "latest" or an RFC3339 instant instead of a named backup. Bounded so the<br />CEL rule's cost stays within the apiserver's per-CRD budget. |  | MaxLength: 64 <br /> |
| `origin` _string_ | origin disambiguates when using time. |  | Enum: [cluster namespace] <br /> |


## RestoreSpec



RestoreSpec restores only this namespace, referencing a Backup in this namespace
(no locationRef, no target-namespace field — structural confinement, R14). If the
Backup is origin=cluster, the operator mediates against the shared DR repo with the
non-forgeable namespace= tag filter. Uses the shared restore selection model.

The execution identity — source and mode — is IMMUTABLE after creation (CEL): the
controller re-derives both every pass, so an edit mid-run would mix two points in time
(or two destructive modes) inside one restore. confirmation stays mutable (R23 is
confirmed by editing it) and so do the selection lists (an edit applies to volumes not
yet started; residue of a deselected volume is reaped once the restore is terminal).



_Appears in:_
- [Restore](#restore)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `source` _[RestoreSource](#restoresource)_ | source is a Backup in this namespace (or latest). |  |  |
| `mode` _[RestoreMode](#restoremode)_ | mode selects Recreate or Overwrite (default Overwrite). | Overwrite | Enum: [Recreate Overwrite] <br /> |
| `resources` _[ResourceSelectorItem](#resourceselectoritem) array_ | resources selects manifests to restore (omitted with volumes ⇒ whole namespace). Bounded to<br />match the volumes cap — an unbounded selector array is an etcd/object-size smell.<br />NOTE: no `omitempty`. A PRESENT-but-empty list means "restore nothing of this kind",<br />while an omitted one means "everything" (spec/02-api.md § Restore selection model), and<br />`omitempty` erases exactly that difference on the way OUT: a Go client sending an empty<br />slice would emit no field at all, and the operator would read it back as omitted and<br />restore the whole namespace. That is the failure mode this model must never have —<br />crystalctl's `--data-only` writes `resources: []`, and it would widen to everything in<br />Overwrite or Recreate mode against a live namespace. |  | MaxItems: 128 <br /> |
| `volumes` _[VolumeSelectorItem](#volumeselectoritem) array_ | volumes selects PVCs (and optionally files) to restore. Bounded so the per-item CEL<br />cost stays within the apiserver's per-CRD budget.<br />No `omitempty`, for the same reason as resources above. |  | MaxItems: 128 <br /> |
| `confirmation` _string_ | confirmation must equal this namespace when the operation modifies existing objects (R23). |  |  |
| `dryRun` _boolean_ | dryRun runs the whole pipeline — ordering, selection, mode resolution — with<br />server-side dry-run applies, persists nothing, and writes the plan to<br />status.resources. The point is to let an operator see what a destructive restore<br />WOULD do before committing to it (04-manifest-backup.md §5.4). |  |  |


## RestoreStatus



RestoreStatus is the observed state of a Restore.



_Appears in:_
- [Restore](#restore)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `phase` _string_ | phase of the restore. |  | Enum: [Pending AwaitingConfirmation Running Completed PartiallyFailed Failed] <br /> |
| `restoredResources` _integer_ | restoredResources count. |  |  |
| `resources` _[RestoreResourcesStatus](#restoreresourcesstatus)_ | resources is the per-resource detail of the manifest half (04-manifest-backup.md §5.4).<br />Under dryRun it holds the PLAN rather than an observed outcome. |  |  |
| `plannedVolumes` _integer_ | plannedVolumes is how many volumes this restore's plan covers: the denominator of the two<br />counters below. Written on every pass, so restoredVolumes/plannedVolumes answers "how far<br />along" while the restore is still running.<br />It is the intersection of spec.volumes with the source Backup's restorable volumes, so it can<br />be smaller than either — and it is 0 for a volumes-free restore (resources[] only), which is a<br />valid restore, not an empty one. |  |  |
| `restoredVolumes` _integer_ | restoredVolumes is how many planned volumes have their data back. It is written on EVERY pass,<br />not only the terminal one: a restore is the operation people run under time pressure, and a<br />counter that reads 0 for forty minutes and then jumps to 9 cannot answer "is it moving".<br />VOLUMES ONLY. The manifest half of a restore is counted in restoredResources and<br />resources.failedCount, and the terminal PHASE rolls up BOTH halves — so a restore whose every<br />volume landed can still read Failed or PartiallyFailed because manifests failed to apply, with<br />failedVolumes at 0. That asymmetry is deliberate (the two halves fail for unrelated reasons and<br />are driven independently) and is spelled out here because this is where a reader meets it. |  |  |
| `failedVolumes` _integer_ | failedVolumes is how many planned volumes settled WITHOUT their data — a failed mover, an<br />exposure that never became mountable, an unsupported target, or a volume whose error budget ran<br />out. It is the answer to "what did not come back", and it is written on every pass.<br />plannedVolumes - restoredVolumes - failedVolumes is what is still in flight. On a terminal<br />restore that difference is 0 by construction: the restore does not go terminal until every<br />planned volume has settled. |  |  |
| `restoredBytes` _integer_ | restoredBytes total. |  |  |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.30/#condition-v1-meta) array_ | conditions represent the current state. |  |  |


## RetentionSpec



RetentionSpec expresses restic-granularity retention, applied per PVC.



_Appears in:_
- [BackupLocationSpec](#backuplocationspec)
- [ClusterBackupLocationSpec](#clusterbackuplocationspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `keepLast` _integer_ |  |  |  |
| `keepHourly` _integer_ |  |  |  |
| `keepDaily` _integer_ |  |  |  |
| `keepWeekly` _integer_ |  |  |  |
| `keepMonthly` _integer_ |  |  |  |
| `keepYearly` _integer_ |  |  |  |


## S3Spec



S3Spec describes the S3-compatible object storage backing a repository.



_Appears in:_
- [BackupLocationSpec](#backuplocationspec)
- [ClusterBackupLocationSpec](#clusterbackuplocationspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `endpoint` _string_ | endpoint URL of the S3-compatible service. |  | MinLength: 1 <br /> |
| `bucket` _string_ | bucket name. |  | MinLength: 1 <br /> |
| `prefix` _string_ | prefix under which the single shared repository lives (<prefix>/<clusterID>/). |  |  |
| `region` _string_ | region of the bucket. |  |  |
| `credentialsSecretRef` _[LocalObjectReference](#localobjectreference)_ | credentialsSecretRef references a Secret holding the S3 credentials (same<br />namespace as a BackupLocation, or crystal-backup-system for a ClusterBackupLocation). |  |  |
| `caBundle` _string_ | caBundle is an optional PEM CA bundle for the endpoint. |  |  |
| `forcePathStyle` _boolean_ | forcePathStyle selects path-style addressing (required by most non-AWS gateways).<br />It is honoured by every consumer that speaks S3 through an AWS SDK or through rclone —<br />internal/escrow, internal/repo/s3stat, internal/selfcheck, and the rclone remotes the<br />external-sync image builds. It is deliberately NOT forwarded to restic, and that is a<br />decision rather than an omission: restic's S3 backend is minio-go, whose default<br />bucket-lookup is `auto`, and minio-go's `auto` resolves to VIRTUAL-HOST style only for<br />Amazon, Google and Aliyun endpoints (s3utils.IsVirtualHostSupported) — every other<br />endpoint, which is precisely "most non-AWS gateways", already gets PATH style with no<br />flag at all. Forwarding it as `-o s3.bucket-lookup=path` would therefore set restic to<br />the value it had already computed, while making the one case it DOES change a wrong one:<br />a location whose endpoint really is AWS would be forced off virtual-host addressing,<br />which AWS has deprecated path style for. Verified against the pinned engine<br />(build/melange/restic.yaml, restic 0.19.1 / minio-go v7). |  |  |
| `connections` _integer_ | connections caps how many concurrent HTTP connections restic opens to this endpoint<br />(restic's own `-o s3.connections`, forwarded by internal/mover.BuildJob).<br />A POINTER, for the reason DiscoverySpec.Enabled is one: nil must stay distinguishable from<br />a value. restic's own default is 5 (internal/backend/s3.NewConfig), and the operator does<br />not want to restate it — a nil here emits no `-o` at all, so restic's default remains<br />restic's to change across an engine bump rather than something this CRD has silently<br />frozen at whatever 5 meant in 0.19.1.<br />The MAXIMUM is the load-bearing half. BackupLocation is TENANT-writable: a namespace can<br />edit its own location, and every namespace of a cluster points at ONE shared gateway<br />(adr/0009). Without a ceiling, `connections: 100000` is a tenant-authored denial of<br />service against every other tenant's backups — not a footgun aimed at themselves. 100 is<br />well past any real throughput knee and still a bound. |  | Maximum: 100 <br />Minimum: 1 <br /> |


## VolumePhase

_Underlying type:_ _string_

VolumePhase is the per-PVC phase within a Backup.

_Validation:_
- Enum: [Pending Snapshotting Uploading Completed Skipped Failed]

_Appears in:_
- [VolumeStatus](#volumestatus)



## VolumeSelectorItem



VolumeSelectorItem selects PVCs (and optionally files within them) to restore.
When several items match the same PVC, the FIRST matching item wins (02-api.md).



_Appears in:_
- [ClusterRestoreSpec](#clusterrestorespec)
- [RestoreSpec](#restorespec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `names` _string array_ | names of PVCs (whole-PVC restore). |  |  |
| `include` _string array_ | include is a list of file globs within the selected PVC(s) (partial restore, R7). |  |  |
| `exclude` _string array_ | exclude is a list of file globs to skip. |  |  |
| `targetPath` _string_ | targetPath overrides the restore root within the PVC (empty or "/" ⇒ the PVC root).<br />It is resolved inside the PVC and must not contain ".." segments. Bounded so the CEL<br />rule's cost stays within the apiserver's per-CRD budget. |  | MaxLength: 256 <br /> |


## VolumeStatus



VolumeStatus is the per-PVC result within a Backup projection.



_Appears in:_
- [BackupStatus](#backupstatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `pvc` _string_ | pvc name. |  |  |
| `snapshotID` _string_ | snapshotID of the PVC data snapshot. |  |  |
| `sizeBytes` _integer_ | sizeBytes is the logical size of the snapshot. |  |  |
| `addedBytes` _integer_ | addedBytes is the deduplicated bytes added by this backup (best-effort). |  |  |
| `phase` _[VolumePhase](#volumephase)_ | phase of this volume. |  | Enum: [Pending Snapshotting Uploading Completed Skipped Failed] <br /> |
| `node` _string_ | node the mover ran on. |  |  |
| `reason` _string_ | reason explains a non-Completed phase (e.g. CSISnapshotUnsupported). |  |  |
| `firstAttemptAt` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.30/#time-v1-meta)_ | firstAttemptAt is when this volume was first ATTEMPTED — not when it was enumerated. Volumes<br />are advanced one per reconcile, so a volume waits its turn before anything is tried on it, and<br />a clock started at enumeration would run while the volume was merely queued.<br />It exists to bound Pending. The three deadlines that bound the later phases each hang off an<br />object that already carries its own creation time (the origin VolumeSnapshot, the mover Job); a<br />volume stuck BEFORE exposure has created nothing, so there is no other clock to read. Without<br />this field the only shared clock is the Backup's own start time, and using that would fail the<br />next volume the instant it reached the head of the queue, having never been tried. |  |  |
| `phaseEnteredAt` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.30/#time-v1-meta)_ | phaseEnteredAt is when this volume last CHANGED PHASE. It is stamped on every transition, in<br />one place (the per-PVC state machine's single call site), so a phase added later cannot forget<br />to set it.<br />It is a BACKSTOP CLOCK, and the distinction from every other deadline in this controller is the<br />reason it had to exist at all. The per-phase bounds hang off the object being waited on — the<br />origin VolumeSnapshot's creationTimestamp, the mover Job's — precisely because that object<br />carries evidence a wall clock cannot: whether anything ever picked the request up, whether a<br />pod ever reached Running. Those bounds are better than this field and must always win. What was<br />missing is a bound for the case where that object CANNOT BE REACHED at all: an exposure that<br />cannot be reconstructed, a mover Job that cannot be read or is absent, an origin snapshot whose<br />progress is unreadable. A volume in Snapshotting or Uploading is not deferred by the queue<br />discipline (correctly — it legitimately holds work in flight), so with no clock it kept the head<br />of its namespace's queue with nothing that could ever end it. That is the thirty-hour incident<br />one and two phases later, and this field is the only clock left once the per-object ones are<br />unreachable.<br />It is a SIBLING of firstAttemptAt rather than a generalisation of it, and merging the two would<br />be a silent regression. firstAttemptAt deliberately means "first ATTEMPT", not "entered<br />Pending": volumes are advanced one per reconcile, so a Pending volume waits its turn before<br />anything is tried on it, and its own doc records why a clock started at enumeration is wrong<br />(it would fail the next volume the instant it reached the head of the queue, having never been<br />tried). A field re-stamped on every transition cannot also be a field stamped once on first<br />contact, and one field meaning both would have quietly converted the Pending bound into the<br />enumeration bound it was written to avoid.<br />Absent means NO CLOCK, and therefore no verdict — never "long ago". A volume already in flight<br />when the operator was upgraded to a version that writes this field has no transition to read,<br />so it is stamped on the first pass that touches it and bounded from there: the backstop can be<br />late, never early. |  |  |


