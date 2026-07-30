/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// BackupSpec is the single unit of execution and the projection of a restorable
// backup. Created by a BackupSchedule/ClusterBackup run or by discovery. A
// cluster-origin Backup (label crystalbackup.io/origin=cluster) is read-only to users.
type BackupSpec struct {
	// scheduleRef names the originating schedule (empty for manual/ad-hoc).
	// +optional
	ScheduleRef string `json:"scheduleRef,omitempty"`

	// locationRef is the backup location. On the namespace plane it is a BackupLocation;
	// a cluster-origin Backup references a ClusterBackupLocation.
	// +required
	LocationRef LocationReference `json:"locationRef"`

	// run is the run configuration MATERIALIZED by whatever created this Backup — a
	// ClusterBackup fan-out or a BackupSchedule stamp (adr/0017 §5). Identity still lives in
	// the fields above; this is the intent, copied down once at creation rather than pulled
	// from a parent at every reconcile.
	//
	// It is a POINTER because absent and empty must stay distinguishable. Absent means "this
	// object predates materialization, or was projected" — the controller falls back to
	// pulling the parent ClusterBackup, which is the only way an object created before this
	// field existed still executes. An empty struct means "materialized, and every knob was
	// left at its default", which must NOT trigger the fallback.
	//
	// DISCOVERY MUST NEVER SET OR OWN THIS FIELD. A projection is reconstructed from restic
	// snapshots alone, and no selector, manifest option or hook command was ever written to
	// the repository; an SSA field manager that owns a field it cannot reproduce would fight
	// the execution controller over the object forever (adr/0017 §2). Projections leave it
	// absent, and the crystalbackup.io/projected annotation stops them executing anyway.
	// +optional
	Run *BackupRunSpec `json:"run,omitempty"`
}

// BackupStatus is the observed state and the projected restore point.
type BackupStatus struct {
	// phase of the backup.
	// +optional
	// +kubebuilder:validation:Enum=Pending;SnapshottingHooks;Snapshotting;Uploading;Completed;PartiallyCompleted;PartiallyFailed;Failed
	Phase string `json:"phase,omitempty"`
	// backupTime is the point-in-time of the snapshot set.
	// +optional
	BackupTime *metav1.Time `json:"backupTime,omitempty"`
	// completionTime is when the run reached a terminal phase — succeeded or failed alike. Absent
	// while it is still running.
	//
	// It exists because there was no honest answer to "when did this Backup FAIL", and an alert
	// that has to ask that question was reading a timestamp that means something else. Neither of
	// the two candidates works:
	//
	//   - status.backupTime is the point-in-time of the SNAPSHOT SET, and every consumer reads it
	//     that way — the last_success gauge, the schedule's lastSuccessTime roll-up, the restore
	//     controller's point-in-time selection. It is only meaningful for a run that produced
	//     something, and reusing it as a failure clock would make "when was this namespace last
	//     captured" and "when did it last break" the same field.
	//   - the Ready condition's lastTransitionTime is the run's START, not its end. A failing run
	//     goes False(reason=InProgress) → False(reason=Failed), and meta.SetStatusCondition only
	//     refreshes lastTransitionTime when the STATUS changes, not when the reason does. On a
	//     forty-minute backup that is forty minutes of skew — enough to put a failure outside a
	//     one-hour alert window it belongs inside, or inside one it has already left.
	//
	// Written ONCE, on first arrival at a terminal phase, and never moved afterwards: a
	// re-reconcile of an already-terminal object must not restate when it finished.
	// ClusterBackupStatus.completionTime is the run-level sibling and means the same thing.
	// +optional
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`
	// manifests records the namespace-manifests snapshot.
	// +optional
	Manifests *ManifestsStatus `json:"manifests,omitempty"`
	// volumes is the per-PVC result set.
	// +optional
	Volumes []VolumeStatus `json:"volumes,omitempty"`
	// hooks records what each consistency hook did (R16). It is the durable account of the freeze
	// window: which pods were quiesced, whether the release ran, and — when it did not — what an
	// operator has to go and undo by hand.
	// +optional
	// +kubebuilder:validation:MaxItems=64
	Hooks []HookStatus `json:"hooks,omitempty"`
	// postHookAttempts counts how many times the post-hook (unfreeze) phase has been tried. Post
	// hooks are retried where pre hooks are not, and the asymmetry is deliberate: a failed pre hook
	// means the snapshot should not be taken, while a failed post hook means an application may
	// still be QUIESCED. Retrying is the difference between a transient blip and an outage.
	// +optional
	PostHookAttempts int32 `json:"postHookAttempts,omitempty"`
	// conditions represent the current state.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=bk
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Location",type=string,JSONPath=`.spec.locationRef.name`
// +kubebuilder:printcolumn:name="Backup-Time",type=date,JSONPath=`.status.backupTime`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Backup is the execution unit and restore-point projection (source of truth = the repository).
type Backup struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of Backup
	// +required
	Spec BackupSpec `json:"spec"`

	// status defines the observed state of Backup
	// +optional
	Status BackupStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// BackupList contains a list of Backup
type BackupList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Backup `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &Backup{}, &BackupList{})
		return nil
	})
}
