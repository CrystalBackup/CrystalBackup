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

package manifests

import "k8s.io/apimachinery/pkg/runtime/schema"

// The apply order of spec/04-manifest-backup.md §5.1. Phases run sequentially; within a
// phase, resources sort by (group, Kind, name) so two restores of the same snapshot issue
// the same calls in the same order.
//
// The ordering is about what an object NEEDS to exist before it is admitted or before it can
// do its job, not about readiness — nothing here waits. A Deployment referencing a missing
// ConfigMap is admitted and then sits unready, so ordering only has to beat admission
// failures and the reconcile storms that follow.
const (
	phaseServiceAccounts = iota + 1
	phaseRBAC
	phaseConfigAndSecrets
	phasePVCs
	phaseEverythingElse
	phaseWorkloads
	phaseNetworking
	// phaseCount is the number of phases, so a loop over them cannot fall out of step with
	// the constants above.
	phaseCount = phaseNetworking
)

// coreGroup is the unnamed API group, spelled out where a map key needs it.
const coreGroup = ""

const (
	groupApps          = "apps"
	groupBatch         = "batch"
	groupRBAC          = "rbac.authorization.k8s.io"
	groupNetworking    = "networking.k8s.io"
	kindPVC            = "PersistentVolumeClaim"
	kindServiceAccount = "ServiceAccount"
)

// phaseByKind pins the kinds §5.1 names to their phase. Keyed on group AND kind rather than
// kind alone: a custom resource may legitimately be called Job or Role, and it belongs in the
// generic phase with the rest of its CRD's kinds, not among the built-in workloads.
var phaseByKind = map[schema.GroupKind]int{
	{Group: coreGroup, Kind: kindServiceAccount}: phaseServiceAccounts,

	{Group: groupRBAC, Kind: "Role"}:        phaseRBAC,
	{Group: groupRBAC, Kind: "RoleBinding"}: phaseRBAC,

	{Group: coreGroup, Kind: "ConfigMap"}: phaseConfigAndSecrets,
	{Group: coreGroup, Kind: "Secret"}:    phaseConfigAndSecrets,

	{Group: coreGroup, Kind: kindPVC}: phasePVCs,

	{Group: groupApps, Kind: "Deployment"}:  phaseWorkloads,
	{Group: groupApps, Kind: "StatefulSet"}: phaseWorkloads,
	{Group: groupApps, Kind: "DaemonSet"}:   phaseWorkloads,
	{Group: groupApps, Kind: "ReplicaSet"}:  phaseWorkloads,
	{Group: groupBatch, Kind: "Job"}:        phaseWorkloads,
	{Group: groupBatch, Kind: "CronJob"}:    phaseWorkloads,
	{Group: coreGroup, Kind: "Pod"}:         phaseWorkloads,

	{Group: coreGroup, Kind: "Service"}:             phaseNetworking,
	{Group: groupNetworking, Kind: "Ingress"}:       phaseNetworking,
	{Group: groupNetworking, Kind: "NetworkPolicy"}: phaseNetworking,
}

// applyPhase returns the phase a resource belongs to. Anything §5.1 does not name — custom
// resources, PDBs, HPAs — lands in the generic phase, which sits AFTER storage and config and
// BEFORE the workloads that tend to depend on it.
func applyPhase(group, kind string) int {
	if p, ok := phaseByKind[schema.GroupKind{Group: group, Kind: kind}]; ok {
		return p
	}
	return phaseEverythingElse
}
