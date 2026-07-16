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

// BackupExternalSyncSpec replicates the namespace's backups from one BackupLocation
// to another BackupLocation in the same namespace via restic copy, re-encrypted to
// the destination's own user key. Both refs are same-namespace (structural confinement);
// the platform key is never involved, so client siloing holds. R28, adr/0013.
type BackupExternalSyncSpec struct {
	// sourceLocationRef is a BackupLocation in this namespace (default: the default one).
	// +required
	SourceLocationRef LocalObjectReference `json:"sourceLocationRef"`

	// destinationLocationRef is another BackupLocation in this namespace with its own key
	// (must differ from source — admission rule 9; never a ClusterBackupLocation — rule 2).
	// +required
	DestinationLocationRef LocalObjectReference `json:"destinationLocationRef"`

	// schedule is a cron expression; empty ⇒ on-demand only.
	// +optional
	Schedule string `json:"schedule,omitempty"`

	// timezone for the cron expression (IANA name).
	// +optional
	Timezone string `json:"timezone,omitempty"`

	// mode tracks the source (Mirror) or only adds (AppendOnly, forced on Immutable destinations).
	// +optional
	// +kubebuilder:default=Mirror
	Mode ExternalSyncMode `json:"mode,omitempty"`
}

// BackupExternalSyncStatus is the observed state of a BackupExternalSync.
type BackupExternalSyncStatus struct {
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
	// bytesCopied (data streamed this run).
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
// +kubebuilder:resource:scope=Namespaced,shortName=bes
// +kubebuilder:printcolumn:name="Mode",type=string,JSONPath=`.spec.mode`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// BackupExternalSync replicates a namespace's backups to a secondary location (R28).
type BackupExternalSync struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of BackupExternalSync
	// +required
	Spec BackupExternalSyncSpec `json:"spec"`

	// status defines the observed state of BackupExternalSync
	// +optional
	Status BackupExternalSyncStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// BackupExternalSyncList contains a list of BackupExternalSync
type BackupExternalSyncList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []BackupExternalSync `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &BackupExternalSync{}, &BackupExternalSyncList{})
		return nil
	})
}
