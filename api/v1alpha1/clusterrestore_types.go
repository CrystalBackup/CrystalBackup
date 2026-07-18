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

// ClusterRestoreSpec restores any namespace from a repository coordinate. It works
// even when the source namespace no longer exists and can create the target.
// Uses the shared restore selection model (mode + resources/volumes lists).
type ClusterRestoreSpec struct {
	// source is the repository coordinate to restore from.
	// +required
	Source ClusterRestoreSource `json:"source"`

	// target is where the restore lands.
	// +required
	Target ClusterRestoreTarget `json:"target"`

	// mode selects Recreate or Overwrite (default Overwrite).
	// +optional
	// +kubebuilder:default=Overwrite
	Mode RestoreMode `json:"mode,omitempty"`

	// resources selects manifests to restore (omitted with volumes ⇒ whole namespace).
	// +optional
	Resources []ResourceSelectorItem `json:"resources,omitempty"`

	// volumes selects PVCs (and optionally files) to restore. Bounded so the per-item CEL
	// cost stays within the apiserver's per-CRD budget.
	// +optional
	// +kubebuilder:validation:MaxItems=128
	Volumes []VolumeSelectorItem `json:"volumes,omitempty"`

	// clusterResources selects cluster-scoped resources to restore (omitted ⇒ none; adr/0011).
	// +optional
	ClusterResources *ClusterResourceRestoreSpec `json:"clusterResources,omitempty"`

	// confirmation must equal target.namespace when the operation modifies existing objects (R23).
	// +optional
	Confirmation string `json:"confirmation,omitempty"`
}

// ClusterRestoreStatus is the observed state of a ClusterRestore.
type ClusterRestoreStatus struct {
	// phase of the restore.
	// +optional
	// +kubebuilder:validation:Enum=Pending;AwaitingConfirmation;Running;Completed;PartiallyFailed;Failed
	Phase string `json:"phase,omitempty"`
	// restoredResources count.
	// +optional
	RestoredResources int32 `json:"restoredResources,omitempty"`
	// restoredVolumes count.
	// +optional
	RestoredVolumes int32 `json:"restoredVolumes,omitempty"`
	// restoredBytes total.
	// +optional
	RestoredBytes int64 `json:"restoredBytes,omitempty"`
	// conditions represent the current state.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=crst
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.spec.target.namespace`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ClusterRestore restores a namespace from a repository coordinate (admin, R14).
type ClusterRestore struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of ClusterRestore
	// +required
	Spec ClusterRestoreSpec `json:"spec"`

	// status defines the observed state of ClusterRestore
	// +optional
	Status ClusterRestoreStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// ClusterRestoreList contains a list of ClusterRestore
type ClusterRestoreList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []ClusterRestore `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &ClusterRestore{}, &ClusterRestoreList{})
		return nil
	})
}
