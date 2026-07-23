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
)

// coreGroup is the unnamed API group, spelled out where a map key needs it.
const coreGroup = ""

const (
	groupApps          = "apps"
	groupBatch         = "batch"
	groupRBAC          = "rbac.authorization.k8s.io"
	groupNetworking    = "networking.k8s.io"
	groupStorage       = "storage.k8s.io"
	groupAPIExtensions = "apiextensions.k8s.io"

	kindPVC            = "PersistentVolumeClaim"
	kindServiceAccount = "ServiceAccount"
	kindRole           = "Role"
	kindRoleBinding    = "RoleBinding"
	kindConfigMap      = "ConfigMap"
	kindDeployment     = "Deployment"
	kindStatefulSet    = "StatefulSet"
	kindDaemonSet      = "DaemonSet"
	kindReplicaSet     = "ReplicaSet"
	kindJob            = "Job"
	kindCronJob        = "CronJob"
	kindPod            = "Pod"
	kindIngress        = "Ingress"
	kindNetworkPolicy  = "NetworkPolicy"
)

// phaseByKind pins the kinds §5.1 names to their phase. Keyed on group AND kind rather than
// kind alone: a custom resource may legitimately be called Job or Role, and it belongs in the
// generic phase with the rest of its CRD's kinds, not among the built-in workloads.
var phaseByKind = map[schema.GroupKind]int{
	{Group: coreGroup, Kind: kindServiceAccount}: phaseServiceAccounts,

	{Group: groupRBAC, Kind: kindRole}:        phaseRBAC,
	{Group: groupRBAC, Kind: kindRoleBinding}: phaseRBAC,

	{Group: coreGroup, Kind: kindConfigMap}: phaseConfigAndSecrets,
	{Group: coreGroup, Kind: kindSecret}:    phaseConfigAndSecrets,

	{Group: coreGroup, Kind: kindPVC}: phasePVCs,

	{Group: groupApps, Kind: kindDeployment}:  phaseWorkloads,
	{Group: groupApps, Kind: kindStatefulSet}: phaseWorkloads,
	{Group: groupApps, Kind: kindDaemonSet}:   phaseWorkloads,
	{Group: groupApps, Kind: kindReplicaSet}:  phaseWorkloads,
	{Group: groupBatch, Kind: kindJob}:        phaseWorkloads,
	{Group: groupBatch, Kind: kindCronJob}:    phaseWorkloads,
	{Group: coreGroup, Kind: kindPod}:         phaseWorkloads,

	{Group: coreGroup, Kind: kindService}:             phaseNetworking,
	{Group: groupNetworking, Kind: kindIngress}:       phaseNetworking,
	{Group: groupNetworking, Kind: kindNetworkPolicy}: phaseNetworking,
}

// The cluster-scoped apply order (adr/0011 §2): CRDs first, so a later kind that a CRD defines
// is registered; then the other cluster-scoped kinds; then Namespaces last, so a namespaced
// object (applied by the SEPARATE, later namespaced Job) has its namespace to bind into. A
// kind=cluster-manifests snapshot holds ONLY cluster-scoped objects, so this is its whole order
// — it does not interleave with the namespaced §5.1 phases, which run in a different Job.
const (
	phaseClusterCRDs = iota + 1
	phaseClusterOther
	phaseClusterNamespaces
)

// kindCRD, kindStorageClass, kindPersistentVolume, kindClusterRole and kindClusterRoleBinding
// are declared in clusterscope.go (this package) and reused here.
const (
	kindNamespace           = "Namespace"
	kindPriorityClass       = "PriorityClass"
	kindIngressClass        = "IngressClass"
	kindRuntimeClass        = "RuntimeClass"
	kindVolumeSnapshotClass = "VolumeSnapshotClass"
	groupScheduling         = "scheduling.k8s.io"
	groupNode               = "node.k8s.io"
	groupSnapshot           = "snapshot.storage.k8s.io"
)

// clusterPhaseByKind pins the cluster-scoped kinds to their phase. CRDs alone sit in the first
// phase; Namespaces alone in the last. Everything else — StorageClass, the *Class kinds, cluster
// RBAC, PVs — is in the middle, where relative order does not matter (none depends on another).
// A cluster-scoped kind not listed here (a custom cluster-scoped CR) lands in the middle too.
var clusterPhaseByKind = map[schema.GroupKind]int{
	{Group: groupAPIExtensions, Kind: kindCRD}: phaseClusterCRDs,

	{Group: groupStorage, Kind: kindStorageClass}:         phaseClusterOther,
	{Group: groupScheduling, Kind: kindPriorityClass}:     phaseClusterOther,
	{Group: groupNetworking, Kind: kindIngressClass}:      phaseClusterOther,
	{Group: groupNode, Kind: kindRuntimeClass}:            phaseClusterOther,
	{Group: groupSnapshot, Kind: kindVolumeSnapshotClass}: phaseClusterOther,
	{Group: groupRBAC, Kind: kindClusterRole}:             phaseClusterOther,
	{Group: groupRBAC, Kind: kindClusterRoleBinding}:      phaseClusterOther,
	{Group: coreGroup, Kind: kindPersistentVolume}:        phaseClusterOther,

	{Group: coreGroup, Kind: kindNamespace}: phaseClusterNamespaces,
}

// applyPhase returns the phase a resource belongs to. For a namespaced restore, anything §5.1
// does not name lands in the generic phase (after storage/config, before workloads). For a
// cluster-scoped restore it uses the adr/0011 §2 order, where an unlisted kind is a middle
// cluster-scoped object.
func applyPhase(group, kind string, clusterScoped bool) int {
	if clusterScoped {
		if p, ok := clusterPhaseByKind[schema.GroupKind{Group: group, Kind: kind}]; ok {
			return p
		}
		return phaseClusterOther
	}
	if p, ok := phaseByKind[schema.GroupKind{Group: group, Kind: kind}]; ok {
		return p
	}
	return phaseEverythingElse
}
