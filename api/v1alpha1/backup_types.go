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
	// manifests records the namespace-manifests snapshot.
	// +optional
	Manifests *ManifestsStatus `json:"manifests,omitempty"`
	// volumes is the per-PVC result set.
	// +optional
	Volumes []VolumeStatus `json:"volumes,omitempty"`
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
