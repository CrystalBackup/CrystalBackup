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

// BackupLocationSpec is the namespace user's own external object storage and their
// own key, isolated by construction (their bucket, credentials and key).
type BackupLocationSpec struct {
	// mode selects Standard (prunable) or Immutable (object-lock; no prune).
	// +optional
	// +kubebuilder:default=Standard
	Mode LocationMode `json:"mode,omitempty"`

	// clusterID defaults from the default ClusterBackupLocation if unset.
	// +optional
	ClusterID string `json:"clusterID,omitempty"`

	// s3 is the user's object storage.
	// +required
	S3 S3Spec `json:"s3"`

	// encryption configures the user-owned key (generated in the namespace if unset).
	// +required
	Encryption NamespaceEncryptionSpec `json:"encryption"`

	// discovery projects Backup objects from this repository into this namespace.
	// +optional
	Discovery DiscoverySpec `json:"discovery,omitempty"`
}

// BackupLocationStatus is the observed state of a BackupLocation.
type BackupLocationStatus struct {
	// phase is a short human-readable summary.
	// +optional
	Phase string `json:"phase,omitempty"`
	// conditions represent the current state (e.g. Ready, Reachable).
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// repositoryRef names the BackupRepository backing this location.
	// +optional
	RepositoryRef string `json:"repositoryRef,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=bl
// +kubebuilder:printcolumn:name="Mode",type=string,JSONPath=`.spec.mode`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// BackupLocation is a namespace user's own off-platform object storage + key.
type BackupLocation struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of BackupLocation
	// +required
	Spec BackupLocationSpec `json:"spec"`

	// status defines the observed state of BackupLocation
	// +optional
	Status BackupLocationStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// BackupLocationList contains a list of BackupLocation
type BackupLocationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []BackupLocation `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &BackupLocation{}, &BackupLocationList{})
		return nil
	})
}
