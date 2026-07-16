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

// ClusterBackupLocationSpec defines the platform object storage, platform key
// and maintenance/verification for the one shared cluster DR repository.
type ClusterBackupLocationSpec struct {
	// default marks this as the default location; exactly one may be default
	// (enforced by the operator webhook — admission rule 4).
	// +optional
	Default bool `json:"default,omitempty"`

	// mode selects Standard (prunable) or Immutable (object-lock; no prune).
	// +optional
	// +kubebuilder:default=Standard
	Mode LocationMode `json:"mode,omitempty"`

	// clusterID is the snapshot host and repository path segment (R20, multi-cluster):
	// the shared repo lives at <s3.prefix>/<clusterID>/.
	// +required
	// +kubebuilder:validation:MinLength=1
	ClusterID string `json:"clusterID"`

	// s3 is the object storage backing the shared repository.
	// +required
	S3 S3Spec `json:"s3"`

	// encryption configures the platform key (age KEK wrapping the platform DEK).
	// +required
	Encryption ClusterEncryptionSpec `json:"encryption"`

	// discovery inventories the repository and projects Backup objects on add and periodically.
	// +optional
	Discovery DiscoverySpec `json:"discovery,omitempty"`

	// maintenance configures prune/check windows (Standard mode only).
	// +optional
	Maintenance *MaintenanceSpec `json:"maintenance,omitempty"`

	// immutable configures object-lock and window rotation (Immutable mode only).
	// +optional
	Immutable *ImmutableSpec `json:"immutable,omitempty"`
}

// ClusterBackupLocationStatus is the observed state of a ClusterBackupLocation.
type ClusterBackupLocationStatus struct {
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

	// namespacesProtected counts distinct namespaces present in the repository.
	// +optional
	NamespacesProtected int32 `json:"namespacesProtected,omitempty"`

	// lastDiscoveryTime is when the repository was last inventoried.
	// +optional
	LastDiscoveryTime *metav1.Time `json:"lastDiscoveryTime,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=cbl
// +kubebuilder:printcolumn:name="Mode",type=string,JSONPath=`.spec.mode`
// +kubebuilder:printcolumn:name="Default",type=boolean,JSONPath=`.spec.default`
// +kubebuilder:printcolumn:name="Protected",type=integer,JSONPath=`.status.namespacesProtected`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ClusterBackupLocation is the platform disaster-recovery object storage: one
// shared restic repository for all (or selected) namespaces, tenancy by tag.
type ClusterBackupLocation struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of ClusterBackupLocation
	// +required
	Spec ClusterBackupLocationSpec `json:"spec"`

	// status defines the observed state of ClusterBackupLocation
	// +optional
	Status ClusterBackupLocationStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// ClusterBackupLocationList contains a list of ClusterBackupLocation
type ClusterBackupLocationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []ClusterBackupLocation `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &ClusterBackupLocation{}, &ClusterBackupLocationList{})
		return nil
	})
}
