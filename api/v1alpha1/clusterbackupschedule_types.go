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

// ClusterBackupScheduleSpec is a CronJob-style schedule that stamps out
// ClusterBackup runs from a template.
type ClusterBackupScheduleSpec struct {
	// schedule is a cron expression.
	// +required
	// +kubebuilder:validation:MinLength=1
	Schedule string `json:"schedule"`

	// timezone for the cron expression (IANA name).
	// +optional
	Timezone string `json:"timezone,omitempty"`

	// paused suspends new runs.
	// +optional
	Paused bool `json:"paused,omitempty"`

	// jitter spreads per-namespace fan-out deterministically (anti thundering herd).
	// +optional
	Jitter bool `json:"jitter,omitempty"`

	// concurrencyPolicy governs overlapping runs.
	// +optional
	// +kubebuilder:default=Forbid
	ConcurrencyPolicy ConcurrencyPolicy `json:"concurrencyPolicy,omitempty"`

	// startingDeadlineSeconds bounds catch-up after downtime to one run.
	// +optional
	StartingDeadlineSeconds *int64 `json:"startingDeadlineSeconds,omitempty"`

	// successfulRunsHistoryLimit is the number of ClusterBackup run records kept
	// (distinct from snapshot retention).
	// +optional
	// +kubebuilder:default=10
	SuccessfulRunsHistoryLimit int32 `json:"successfulRunsHistoryLimit,omitempty"`

	// failedRunsHistoryLimit is the number of failed run records kept.
	// +optional
	// +kubebuilder:default=10
	FailedRunsHistoryLimit int32 `json:"failedRunsHistoryLimit,omitempty"`

	// template is the ClusterBackup run configuration (jobTemplate analogue).
	// +required
	Template ClusterBackupTemplate `json:"template"`
}

// ClusterBackupScheduleStatus is the observed state of a ClusterBackupSchedule.
type ClusterBackupScheduleStatus struct {
	// phase is a short human-readable summary.
	// +optional
	Phase string `json:"phase,omitempty"`
	// lastRunName is the most recent ClusterBackup run.
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
// +kubebuilder:resource:scope=Cluster,shortName=cbs
// +kubebuilder:printcolumn:name="Schedule",type=string,JSONPath=`.spec.schedule`
// +kubebuilder:printcolumn:name="Paused",type=boolean,JSONPath=`.spec.paused`
// +kubebuilder:printcolumn:name="Last-Success",type=date,JSONPath=`.status.lastSuccessTime`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ClusterBackupSchedule stamps out ClusterBackup DR runs on a cron schedule.
type ClusterBackupSchedule struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of ClusterBackupSchedule
	// +required
	Spec ClusterBackupScheduleSpec `json:"spec"`

	// status defines the observed state of ClusterBackupSchedule
	// +optional
	Status ClusterBackupScheduleStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// ClusterBackupScheduleList contains a list of ClusterBackupSchedule
type ClusterBackupScheduleList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []ClusterBackupSchedule `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &ClusterBackupSchedule{}, &ClusterBackupScheduleList{})
		return nil
	})
}
