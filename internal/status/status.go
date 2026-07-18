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

// Package status is the shared, cluster-free status machinery for the
// CrystalBackup controllers. Several controllers write a status subresource
// (Backup, ClusterBackup, and the schedules/aggregations that feed them), and
// two things are easy to get subtly — and divergently — wrong if each controller
// reimplements them:
//
//  1. How a metav1.Condition is set. We funnel every controller through the
//     upstream meta.SetStatusCondition semantics so LastTransitionTime is bumped
//     only on a real Status change and a no-op reconcile never churns the object.
//
//  2. How a finer-grained phase set rolls up into a coarser one. There are two
//     such mappers and they are DISTINCT: RollUpVolumePhases folds the per-PVC
//     VolumePhase set into a Backup phase, and RollUpBackupPhases folds the child
//     Backup phases into a ClusterBackup phase. The enums have different members
//     (Skipped exists only per-volume; PartiallyCompleted/PartiallyFailed only on
//     the aggregates; Running only on ClusterBackup), so the two mappers must
//     never be substituted for one another.
//
// Everything here is a pure function of its inputs — no client, no context, no
// clock we own beyond what meta.SetStatusCondition reads — so the package is
// fully unit-testable without an API server and the controllers stay thin.
//
// Phase-constant note: the three phase vocabularies are pinned here as typed
// string constants because api/v1alpha1 currently spells them only inside
// +kubebuilder:validation:Enum markers and plain string status fields, with no
// Go consts to reference. Treat these consts as deliberate candidates to PROMOTE
// into api/v1alpha1 once a second package needs them; until then this package is
// the single Go source of truth and a pinning test guards them against drifting
// from the CRD markers.
package status

import (
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SetCondition upserts a condition of type condType into *conditions using the
// upstream meta.SetStatusCondition semantics. Controllers call it instead of
// hand-building a metav1.Condition so that every controller behaves identically
// on two points that matter for object stability:
//
//   - LastTransitionTime is left zero on the value handed to the library, which
//     tells meta.SetStatusCondition to stamp "now" ONLY when the Status actually
//     transitions. Re-asserting the same Status on a subsequent reconcile — even
//     with a fresher Reason or Message — therefore does not move the timestamp,
//     which avoids needless status writes and watch churn.
//   - ObservedGeneration is always recorded from the caller's observed
//     generation, so consumers can tell whether a condition reflects the current
//     spec generation or a stale one.
//
// The condition-type vocabulary is deliberately NOT defined here (it is
// per-controller). Reason should be a non-empty PascalCase token — the API
// server rejects an empty Reason on write — but this pure helper does not
// validate it; that is the caller's contract.
func SetCondition(conditions *[]metav1.Condition, condType string, status metav1.ConditionStatus, reason, message string, observedGeneration int64) {
	meta.SetStatusCondition(conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: observedGeneration,
	})
}

// IsConditionTrue reports whether a condition of type condType is present and has
// Status == metav1.ConditionTrue. A missing condition is not True. It wraps
// meta.IsStatusConditionTrue so callers reach conditions through one package.
func IsConditionTrue(conditions []metav1.Condition, condType string) bool {
	return meta.IsStatusConditionTrue(conditions, condType)
}

// FindCondition returns a pointer to the condition of type condType, or nil if
// absent. The pointer aliases the backing array of conditions: callers may read
// through it, but should mutate conditions via SetCondition rather than through
// the returned pointer so LastTransitionTime stays correctly managed. It wraps
// meta.FindStatusCondition for symmetry with the other helpers.
func FindCondition(conditions []metav1.Condition, condType string) *metav1.Condition {
	return meta.FindStatusCondition(conditions, condType)
}
