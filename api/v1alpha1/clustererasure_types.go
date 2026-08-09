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
	// Optional (not required) on purpose, mirroring Restore/ClusterRestore: an ABSENT/empty value is
	// admitted so the controller can park the erasure in phase AwaitingConfirmation until the operator
	// edits it in — the deliberate two-step for the destructive path. A required+MinLength=1 field
	// would be rejected by the API server's structural schema BEFORE the confirmation VAP runs, making
	// the AwaitingConfirmation phase unreachable and contradicting the admission policy (which admits
	// empty and denies only a non-matching non-empty value).
	// +optional
	Confirmation string `json:"confirmation,omitempty"`
}

// ClusterErasureStatus is the observed state of a ClusterErasure. On Immutable
// locations the erasure is Blocked until object-lock expiry.
type ClusterErasureStatus struct {
	// phase of the erasure.
	// +optional
	// +kubebuilder:validation:Enum=Pending;AwaitingConfirmation;Running;Completed;Blocked;Failed
	Phase string `json:"phase,omitempty"`
	// snapshotsTargeted is how many snapshots matched this erasure's filter when its scope was
	// measured, BEFORE anything was removed. It is the denominator of the record: the erasure has to
	// count what it is about to destroy while the evidence still exists, and this field is where that
	// count lives. It never changes once written.
	// +optional
	SnapshotsTargeted int32 `json:"snapshotsTargeted,omitempty"`
	// snapshotsForgotten is how many snapshots this erasure is ESTABLISHED to have removed — never how
	// many it intended to remove. It stays 0 while the erasure is running, and on a terminal object it
	// is either the whole scope (the forget+prune reported success) or the scope minus what a
	// post-failure listing still found.
	//
	// This field is a compliance attestation, not a progress counter: it is what somebody points at to
	// assert that a GDPR erasure, a contractual deletion or a tenant offboarding was carried out. It
	// previously held the PRE-erasure count, so a failed erasure published a failed phase beside
	// "snapshotsForgotten: 10" — a record claiming a destruction that had not happened. Read it
	// together with snapshotsRemaining, which says what is left.
	// +optional
	SnapshotsForgotten int32 `json:"snapshotsForgotten,omitempty"`
	// snapshotsRemaining is how many snapshots matching this erasure's filter are still in the
	// repository. Zero on a completed erasure; on a failed one it is the work left to do, and it is
	// what makes a partial erasure legible (4 of 10 removed reads forgotten 4, remaining 6).
	//
	// When the erasure failed AND the verification listing could not be read either, this field holds
	// the whole targeted scope: an outcome nobody could establish is reported as an erasure that
	// destroyed nothing, never as an empty repository.
	//
	// snapshotsForgotten + snapshotsRemaining == snapshotsTargeted, except when snapshots matching the
	// target were written AFTER the scope was measured, in which case remaining can exceed the scope
	// and the operator says so in a Warning event rather than adjusting a number to make it balance.
	// +optional
	SnapshotsRemaining int32 `json:"snapshotsRemaining,omitempty"`
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
// +kubebuilder:printcolumn:name="Targeted",type=integer,JSONPath=`.status.snapshotsTargeted`
// +kubebuilder:printcolumn:name="Forgotten",type=integer,JSONPath=`.status.snapshotsForgotten`
// +kubebuilder:printcolumn:name="Remaining",type=integer,JSONPath=`.status.snapshotsRemaining`
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
