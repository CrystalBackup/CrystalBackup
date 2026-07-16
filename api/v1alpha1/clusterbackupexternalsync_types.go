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

// ClusterBackupExternalSyncSpec replicates the shared repository to a secondary
// ClusterBackupLocation using restic copy, re-encrypted to the destination's own
// platform DEK (an independent repo, not a byte clone). R28, adr/0013.
type ClusterBackupExternalSyncSpec struct {
	// sourceLocationRef is a ClusterBackupLocation (default: the default one).
	// +required
	SourceLocationRef LocalObjectReference `json:"sourceLocationRef"`

	// destinationLocationRef is another ClusterBackupLocation with its own key
	// (must differ from source — admission rule 9).
	// +required
	DestinationLocationRef LocalObjectReference `json:"destinationLocationRef"`

	// schedule is a cron expression; empty ⇒ on-demand only.
	// +optional
	Schedule string `json:"schedule,omitempty"`

	// timezone for the cron expression (IANA name).
	// +optional
	Timezone string `json:"timezone,omitempty"`

	// paused suspends new syncs.
	// +optional
	Paused bool `json:"paused,omitempty"`

	// mode tracks the source (Mirror) or only adds (AppendOnly, forced on Immutable destinations).
	// +optional
	// +kubebuilder:default=Mirror
	Mode ExternalSyncMode `json:"mode,omitempty"`

	// selection narrows the copy by namespace tag; omitted ⇒ whole repository.
	// +optional
	Selection *ExternalSyncSelection `json:"selection,omitempty"`
}

// ClusterBackupExternalSyncStatus is the observed state of a ClusterBackupExternalSync.
type ClusterBackupExternalSyncStatus struct {
	// phase of the sync.
	// +optional
	// +kubebuilder:validation:Enum=Pending;Running;Completed;PartiallyFailed;Failed
	Phase string `json:"phase,omitempty"`
	// lastSuccessTime of a completed sync.
	// +optional
	LastSuccessTime *metav1.Time `json:"lastSuccessTime,omitempty"`
	// snapshotsCopied in the last run.
	// +optional
	SnapshotsCopied int32 `json:"snapshotsCopied,omitempty"`
	// bytesCopied (data streamed this run, blob-incremental).
	// +optional
	BytesCopied int64 `json:"bytesCopied,omitempty"`
	// lagSnapshots is the count of source snapshots not yet at the destination.
	// +optional
	LagSnapshots int32 `json:"lagSnapshots,omitempty"`
	// conditions represent the current state.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=cbes
// +kubebuilder:printcolumn:name="Mode",type=string,JSONPath=`.spec.mode`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Lag",type=integer,JSONPath=`.status.lagSnapshots`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ClusterBackupExternalSync replicates the shared DR repo to a secondary location (R28).
type ClusterBackupExternalSync struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of ClusterBackupExternalSync
	// +required
	Spec ClusterBackupExternalSyncSpec `json:"spec"`

	// status defines the observed state of ClusterBackupExternalSync
	// +optional
	Status ClusterBackupExternalSyncStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// ClusterBackupExternalSyncList contains a list of ClusterBackupExternalSync
type ClusterBackupExternalSyncList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []ClusterBackupExternalSync `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &ClusterBackupExternalSync{}, &ClusterBackupExternalSyncList{})
		return nil
	})
}
