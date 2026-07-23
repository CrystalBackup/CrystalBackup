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

// BackupScheduleSpec is a CronJob-style schedule that stamps out Backup objects in
// the user's namespace against a namespaced BackupLocation.
type BackupScheduleSpec struct {
	// locationRef is a BackupLocation in this namespace (required; never a ClusterBackupLocation).
	// +required
	LocationRef LocalObjectReference `json:"locationRef"`

	// schedule is a cron expression.
	// +required
	// +kubebuilder:validation:MinLength=1
	Schedule string `json:"schedule"`

	// timezone for the cron expression (IANA name).
	// +optional
	Timezone string `json:"timezone,omitempty"`

	// jitter spreads execution deterministically.
	// +optional
	Jitter bool `json:"jitter,omitempty"`

	// concurrencyPolicy governs overlapping runs.
	// +optional
	// +kubebuilder:default=Forbid
	ConcurrencyPolicy ConcurrencyPolicy `json:"concurrencyPolicy,omitempty"`

	// startingDeadlineSeconds bounds catch-up after downtime.
	// +optional
	StartingDeadlineSeconds *int64 `json:"startingDeadlineSeconds,omitempty"`

	// pvcSelector selects PVCs (default all).
	// +optional
	PVCSelector PVCSelector `json:"pvcSelector,omitempty"`

	// includeManifests also captures namespace manifests (default true).
	// +optional
	// +kubebuilder:default=true
	IncludeManifests *bool `json:"includeManifests,omitempty"`

	// manifestOptions tunes what the manifest dump captures (03-security-and-tenancy.md §10).
	// +optional
	ManifestOptions ManifestOptions `json:"manifestOptions,omitempty"`

	// hooks are exec hooks around snapshotting (R16).
	// +optional
	Hooks HooksSpec `json:"hooks,omitempty"`

	// backoffLimit for mover Jobs.
	// +optional
	BackoffLimit int32 `json:"backoffLimit,omitempty"`
}

// BackupScheduleStatus is the observed state of a BackupSchedule.
type BackupScheduleStatus struct {
	// phase is a short human-readable summary.
	// +optional
	Phase string `json:"phase,omitempty"`
	// lastRunName is the most recent Backup.
	// +optional
	LastRunName string `json:"lastRunName,omitempty"`
	// lastSuccessTime is when the last run completed successfully.
	// +optional
	LastSuccessTime *metav1.Time `json:"lastSuccessTime,omitempty"`
	// nextScheduleTime is the next planned run.
	// +optional
	NextScheduleTime *metav1.Time `json:"nextScheduleTime,omitempty"`
	// conditions represent the current state.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=bs
// +kubebuilder:printcolumn:name="Schedule",type=string,JSONPath=`.spec.schedule`
// +kubebuilder:printcolumn:name="Location",type=string,JSONPath=`.spec.locationRef.name`
// +kubebuilder:printcolumn:name="Last-Success",type=date,JSONPath=`.status.lastSuccessTime`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// BackupSchedule stamps out user Backups on a cron schedule.
type BackupSchedule struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of BackupSchedule
	// +required
	Spec BackupScheduleSpec `json:"spec"`

	// status defines the observed state of BackupSchedule
	// +optional
	Status BackupScheduleStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// BackupScheduleList contains a list of BackupSchedule
type BackupScheduleList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []BackupSchedule `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &BackupSchedule{}, &BackupScheduleList{})
		return nil
	})
}
