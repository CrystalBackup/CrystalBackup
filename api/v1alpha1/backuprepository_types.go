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

// RepositoryLocationRef identifies the location a BackupRepository backs.
type RepositoryLocationRef struct {
	// kind of the referenced location.
	// +optional
	// +kubebuilder:validation:Enum=ClusterBackupLocation;BackupLocation
	Kind string `json:"kind,omitempty"`
	// name of the referenced location.
	// +optional
	Name string `json:"name,omitempty"`
}

// BackupRepositorySpec is intentionally empty: a BackupRepository is operator-managed
// (one per ClusterBackupLocation or namespaced BackupLocation). Its meaningful content
// lives in status. It is not user-facing.
type BackupRepositorySpec struct {
}

// BackupRepositoryStatus holds repository state and the discovery inventory.
type BackupRepositoryStatus struct {
	// location the repository backs.
	// +optional
	Location RepositoryLocationRef `json:"location,omitempty"`
	// scope of the backing location.
	// +optional
	// +kubebuilder:validation:Enum=Cluster;Namespaced
	Scope string `json:"scope,omitempty"`
	// ownerNamespace is set for a namespaced (tenant) repository.
	// +optional
	OwnerNamespace string `json:"ownerNamespace,omitempty"`
	// repositoryURL is the restic repository URL (published for the standalone CLI, R8).
	// +optional
	RepositoryURL string `json:"repositoryURL,omitempty"`
	// initialized is true once restic init has succeeded.
	// +optional
	Initialized bool `json:"initialized,omitempty"`
	// mode of the repository.
	// +optional
	Mode LocationMode `json:"mode,omitempty"`
	// keySlots present in the repository (cluster: [platform]; tenant: [tenant] (+platform)).
	// +optional
	KeySlots []string `json:"keySlots,omitempty"`
	// snapshotCount in the repository.
	// +optional
	SnapshotCount int32 `json:"snapshotCount,omitempty"`
	// namespacesPresent is the count of distinct namespace tags found.
	// +optional
	NamespacesPresent int32 `json:"namespacesPresent,omitempty"`
	// lastDiscoveryTime is when the repository was last inventoried.
	// +optional
	LastDiscoveryTime *metav1.Time `json:"lastDiscoveryTime,omitempty"`
	// lastMaintenanceTime is when prune last ran.
	// +optional
	LastMaintenanceTime *metav1.Time `json:"lastMaintenanceTime,omitempty"`
	// lastCheckTime is when restic check last ran.
	// +optional
	LastCheckTime *metav1.Time `json:"lastCheckTime,omitempty"`
	// lastCheckResult of the most recent check.
	// +optional
	LastCheckResult string `json:"lastCheckResult,omitempty"`
	// approximateSizeBytes of the deduplicated repository.
	// +optional
	ApproximateSizeBytes int64 `json:"approximateSizeBytes,omitempty"`
	// conditions represent the current state.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=br
// +kubebuilder:printcolumn:name="Scope",type=string,JSONPath=`.status.scope`
// +kubebuilder:printcolumn:name="Initialized",type=boolean,JSONPath=`.status.initialized`
// +kubebuilder:printcolumn:name="URL",type=string,JSONPath=`.status.repositoryURL`
// +kubebuilder:printcolumn:name="Snapshots",type=integer,JSONPath=`.status.snapshotCount`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// BackupRepository is the operator-internal state and inventory of one restic repository.
type BackupRepository struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of BackupRepository
	// +required
	Spec BackupRepositorySpec `json:"spec"`

	// status defines the observed state of BackupRepository
	// +optional
	Status BackupRepositoryStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// BackupRepositoryList contains a list of BackupRepository
type BackupRepositoryList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []BackupRepository `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &BackupRepository{}, &BackupRepositoryList{})
		return nil
	})
}
