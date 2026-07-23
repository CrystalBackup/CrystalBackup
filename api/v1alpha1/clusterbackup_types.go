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

// ClusterBackupSpec defines one DR run. It resolves spec.namespaces and creates a
// Backup in each matching namespace (linked by label crystalbackup.io/cluster-backup),
// and captures cluster-scoped resources (adr/0011). Per-namespace detail lives in the
// child Backup objects; this object keeps only aggregate status.
type ClusterBackupSpec struct {
	// scheduleRef names the ClusterBackupSchedule that created this run (empty for manual).
	// +optional
	ScheduleRef string `json:"scheduleRef,omitempty"`

	// ClusterBackupRunSpec is the run configuration — inherited from the schedule
	// template, or specified directly for a manual run.
	ClusterBackupRunSpec `json:",inline"`
}

// ClusterBackupStatus is the aggregate observed state of a DR run (no unbounded
// perNamespace map; failures is capped).
type ClusterBackupStatus struct {
	// phase of the run.
	// +optional
	// +kubebuilder:validation:Enum=Pending;Running;Completed;PartiallyFailed;Failed
	Phase string `json:"phase,omitempty"`
	// startTime is when the run began.
	// +optional
	StartTime *metav1.Time `json:"startTime,omitempty"`
	// completionTime is when the run finished.
	// +optional
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`
	// namespacesMatched by the selector.
	// +optional
	NamespacesMatched int32 `json:"namespacesMatched,omitempty"`
	// namespacesSucceeded fully.
	// +optional
	NamespacesSucceeded int32 `json:"namespacesSucceeded,omitempty"`
	// namespacesFailed at least one PVC.
	// +optional
	NamespacesFailed int32 `json:"namespacesFailed,omitempty"`
	// pvcsSucceeded across all namespaces.
	// +optional
	PVCsSucceeded int32 `json:"pvcsSucceeded,omitempty"`
	// pvcsFailed across all namespaces.
	// +optional
	PVCsFailed int32 `json:"pvcsFailed,omitempty"`
	// clusterResourcesCaptured in the kind=cluster-manifests snapshot (adr/0011). A flat mirror
	// of clusterManifests.resourceCount, kept because it is the field the run's headline count
	// has always exposed.
	// +optional
	ClusterResourcesCaptured int32 `json:"clusterResourcesCaptured,omitempty"`
	// clusterManifests records the run's one kind=cluster-manifests snapshot (adr/0011 §1). Its
	// presence is also the "capture is terminal" marker the reconcile keys on — set once, it
	// stops a second capture of the same run, exactly as backup.status.manifests does for a
	// namespace. Absent means either the capture is still in flight or the run opted out.
	// +optional
	ClusterManifests *ManifestsStatus `json:"clusterManifests,omitempty"`
	// addedBytes is the deduplicated bytes added by this run.
	// +optional
	AddedBytes int64 `json:"addedBytes,omitempty"`
	// failures is a capped list of per-namespace failures.
	// +optional
	Failures []FailureRecord `json:"failures,omitempty"`
	// conditions represent the current state.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=cb
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Matched",type=integer,JSONPath=`.status.namespacesMatched`
// +kubebuilder:printcolumn:name="Succeeded",type=integer,JSONPath=`.status.namespacesSucceeded`
// +kubebuilder:printcolumn:name="Failed",type=integer,JSONPath=`.status.namespacesFailed`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ClusterBackup is one DR run that fans out Backup objects into selected namespaces.
type ClusterBackup struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of ClusterBackup
	// +required
	Spec ClusterBackupSpec `json:"spec"`

	// status defines the observed state of ClusterBackup
	// +optional
	Status ClusterBackupStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// ClusterBackupList contains a list of ClusterBackup
type ClusterBackupList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []ClusterBackup `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &ClusterBackup{}, &ClusterBackupList{})
		return nil
	})
}
