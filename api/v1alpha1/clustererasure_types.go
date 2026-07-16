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

// ClusterErasureSpec is the right-to-erasure operation: forget+prune a tenant,
// namespace or PVC from a ClusterBackupLocation (physical deletion in the shared repo).
type ClusterErasureSpec struct {
	// locationRef is the ClusterBackupLocation to erase from.
	// +required
	LocationRef LocalObjectReference `json:"locationRef"`

	// target selects exactly one erasure scope (tenant, namespace, or namespace+pvc).
	// +required
	Target ErasureTarget `json:"target"`

	// confirmation must equal the target identity (tenant, namespace, or <namespace>/<pvc>; R23).
	// +required
	// +kubebuilder:validation:MinLength=1
	Confirmation string `json:"confirmation"`
}

// ClusterErasureStatus is the observed state of a ClusterErasure. On Immutable
// locations the erasure is Blocked until object-lock expiry.
type ClusterErasureStatus struct {
	// phase of the erasure.
	// +optional
	// +kubebuilder:validation:Enum=Pending;AwaitingConfirmation;Running;Completed;Blocked;Failed
	Phase string `json:"phase,omitempty"`
	// snapshotsForgotten count.
	// +optional
	SnapshotsForgotten int32 `json:"snapshotsForgotten,omitempty"`
	// reclaimedBytes after prune.
	// +optional
	ReclaimedBytes int64 `json:"reclaimedBytes,omitempty"`
	// blockedUntil is set on Immutable locations (object-lock expiry).
	// +optional
	BlockedUntil string `json:"blockedUntil,omitempty"`
	// conditions represent the current state.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=cer
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Forgotten",type=integer,JSONPath=`.status.snapshotsForgotten`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ClusterErasure erases a tenant/namespace/PVC from a location (right-to-erasure, R21).
type ClusterErasure struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of ClusterErasure
	// +required
	Spec ClusterErasureSpec `json:"spec"`

	// status defines the observed state of ClusterErasure
	// +optional
	Status ClusterErasureStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// ClusterErasureList contains a list of ClusterErasure
type ClusterErasureList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []ClusterErasure `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &ClusterErasure{}, &ClusterErasureList{})
		return nil
	})
}
