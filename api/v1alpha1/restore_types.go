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
	// NOTE: no `omitempty`. A PRESENT-but-empty list means "restore nothing of this kind",
	// while an omitted one means "everything" (spec/02-api.md § Restore selection model), and
	// `omitempty` erases exactly that difference on the way OUT: a Go client sending an empty
	// slice would emit no field at all, and the operator would read it back as omitted and
	// restore the whole namespace. That is the failure mode this model must never have —
	// crystalctl's `--data-only` writes `resources: []`, and it would widen to everything in
	// Overwrite or Recreate mode against a live namespace.
	Resources []ResourceSelectorItem `json:"resources"`

	// volumes selects PVCs (and optionally files) to restore. Bounded so the per-item CEL
	// cost stays within the apiserver's per-CRD budget.
	// +optional
	// +kubebuilder:validation:MaxItems=128
	// No `omitempty`, for the same reason as resources above.
	Volumes []VolumeSelectorItem `json:"volumes"`

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
	// plannedVolumes is how many volumes this restore's plan covers: the denominator of the two
	// counters below. Written on every pass, so restoredVolumes/plannedVolumes answers "how far
	// along" while the restore is still running.
	//
	// It is the intersection of spec.volumes with the source Backup's restorable volumes, so it can
	// be smaller than either — and it is 0 for a volumes-free restore (resources[] only), which is a
	// valid restore, not an empty one.
	// +optional
	PlannedVolumes int32 `json:"plannedVolumes,omitempty"`
	// restoredVolumes is how many planned volumes have their data back. It is written on EVERY pass,
	// not only the terminal one: a restore is the operation people run under time pressure, and a
	// counter that reads 0 for forty minutes and then jumps to 9 cannot answer "is it moving".
	//
	// VOLUMES ONLY. The manifest half of a restore is counted in restoredResources and
	// resources.failedCount, and the terminal PHASE rolls up BOTH halves — so a restore whose every
	// volume landed can still read Failed or PartiallyFailed because manifests failed to apply, with
	// failedVolumes at 0. That asymmetry is deliberate (the two halves fail for unrelated reasons and
	// are driven independently) and is spelled out here because this is where a reader meets it.
	// +optional
	RestoredVolumes int32 `json:"restoredVolumes,omitempty"`
	// failedVolumes is how many planned volumes settled WITHOUT their data — a failed mover, an
	// exposure that never became mountable, an unsupported target, or a volume whose error budget ran
	// out. It is the answer to "what did not come back", and it is written on every pass.
	//
	// plannedVolumes - restoredVolumes - failedVolumes is what is still in flight. On a terminal
	// restore that difference is 0 by construction: the restore does not go terminal until every
	// planned volume has settled.
	// +optional
	FailedVolumes int32 `json:"failedVolumes,omitempty"`
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
// +kubebuilder:printcolumn:name="Restored",type=integer,JSONPath=`.status.restoredVolumes`
// +kubebuilder:printcolumn:name="Failed",type=integer,JSONPath=`.status.failedVolumes`
// +kubebuilder:printcolumn:name="Volumes",type=integer,JSONPath=`.status.plannedVolumes`
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
