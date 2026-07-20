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

// RestoreSpec restores only this namespace, referencing a Backup in this namespace
// (no locationRef, no target-namespace field — structural confinement, R14). If the
// Backup is origin=cluster, the operator mediates against the shared DR repo with the
// non-forgeable namespace= tag filter. Uses the shared restore selection model.
//
// The execution identity — source and mode — is IMMUTABLE after creation (CEL): the
// controller re-derives both every pass, so an edit mid-run would mix two points in time
// (or two destructive modes) inside one restore. confirmation stays mutable (R23 is
// confirmed by editing it) and so do the selection lists (an edit applies to volumes not
// yet started; residue of a deselected volume is reaped once the restore is terminal).
// +kubebuilder:validation:XValidation:rule="self.source == oldSelf.source",message="spec.source is immutable"
// +kubebuilder:validation:XValidation:rule="self.mode == oldSelf.mode",message="spec.mode is immutable"
type RestoreSpec struct {
	// source is a Backup in this namespace (or latest).
	// +required
	Source RestoreSource `json:"source"`

	// mode selects Recreate or Overwrite (default Overwrite).
	// +optional
	// +kubebuilder:default=Overwrite
	Mode RestoreMode `json:"mode,omitempty"`

	// resources selects manifests to restore (omitted with volumes ⇒ whole namespace). Bounded to
	// match the volumes cap — an unbounded selector array is an etcd/object-size smell.
	// +optional
	// +kubebuilder:validation:MaxItems=128
	Resources []ResourceSelectorItem `json:"resources,omitempty"`

	// volumes selects PVCs (and optionally files) to restore. Bounded so the per-item CEL
	// cost stays within the apiserver's per-CRD budget.
	// +optional
	// +kubebuilder:validation:MaxItems=128
	Volumes []VolumeSelectorItem `json:"volumes,omitempty"`

	// confirmation must equal this namespace when the operation modifies existing objects (R23).
	// +optional
	Confirmation string `json:"confirmation,omitempty"`

	// dryRun runs the whole pipeline — ordering, selection, mode resolution — with
	// server-side dry-run applies, persists nothing, and writes the plan to
	// status.resources. The point is to let an operator see what a destructive restore
	// WOULD do before committing to it (04-manifest-backup.md §5.4).
	// +optional
	DryRun bool `json:"dryRun,omitempty"`
}

// RestoreStatus is the observed state of a Restore.
type RestoreStatus struct {
	// phase of the restore.
	// +optional
	// +kubebuilder:validation:Enum=Pending;AwaitingConfirmation;Running;Completed;PartiallyFailed;Failed
	Phase string `json:"phase,omitempty"`
	// restoredResources count.
	// +optional
	RestoredResources int32 `json:"restoredResources,omitempty"`
	// resources is the per-resource detail of the manifest half (04-manifest-backup.md §5.4).
	// Under dryRun it holds the PLAN rather than an observed outcome.
	// +optional
	Resources *RestoreResourcesStatus `json:"resources,omitempty"`
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
// +kubebuilder:resource:scope=Namespaced,shortName=rst
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Restore is a self-service restore of the user's own namespace.
type Restore struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of Restore
	// +required
	Spec RestoreSpec `json:"spec"`

	// status defines the observed state of Restore
	// +optional
	Status RestoreStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// RestoreList contains a list of Restore
type RestoreList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Restore `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &Restore{}, &RestoreList{})
		return nil
	})
}
