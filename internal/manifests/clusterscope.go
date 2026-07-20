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

import (
	"strings"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Cluster-scoped capture (R22, spec/adr/0011). A namespace does not restore into a vacuum: its
// PVCs name a StorageClass, its workloads a PriorityClass or IngressClass, and its custom
// resources cannot exist at all without their CustomResourceDefinitions. Those objects are
// cluster-scoped, so the namespaced backup path — which only ever enumerates
// `namespaced == true` — cannot see them by construction.
//
// This is the SELECTION half: which cluster-scoped kinds a run captures. adr/0011 §1 is the
// canonical list, and this file is its executable form.

// ClusterAllowKinds is the default allow-list, used when the run names no explicit include.
// Curated to be useful without being noise (adr/0011 §1): everything here is something a
// restored namespace can NEED, and nothing here is something a cluster grows on its own.
//
// Keyed on group AND kind, like the apply phases: a CRD is free to define its own Kind called
// StorageClass, and it does not belong in a DR allow-list just for sharing a name.
const (
	kindClusterRole        = "ClusterRole"
	kindClusterRoleBinding = "ClusterRoleBinding"
)

var ClusterAllowKinds = map[schema.GroupKind]bool{
	{Group: "apiextensions.k8s.io", Kind: "CustomResourceDefinition"}: true,
	{Group: "storage.k8s.io", Kind: "StorageClass"}:                   true,
	{Group: "snapshot.storage.k8s.io", Kind: "VolumeSnapshotClass"}:   true,
	{Group: groupNetworking, Kind: "IngressClass"}:                    true,
	{Group: "scheduling.k8s.io", Kind: "PriorityClass"}:               true,
	{Group: "node.k8s.io", Kind: "RuntimeClass"}:                      true,
	{Group: groupRBAC, Kind: kindClusterRole}:                         true,
	{Group: groupRBAC, Kind: kindClusterRoleBinding}:                  true,
	// PV specs, for the PV↔PVC rebinding story.
	{Group: coreGroup, Kind: "PersistentVolume"}: true,
}

// systemNamePrefix marks the control plane's own RBAC objects. They are excluded by default
// because restoring them means fighting the API server: kube-apiserver reconciles the default
// ClusterRoles on every start, so a restored copy is either overwritten immediately or — worse,
// if it drifted from the target cluster's version — briefly grants something the target's own
// policy did not.
const systemNamePrefix = "system:"

// wellKnownSystemNames are the control-plane RBAC objects that carry no system: prefix. They
// are as much the API server's own as the prefixed ones, and restoring cluster-admin over a
// target cluster's copy is the kind of thing that is only ever noticed afterwards.
var wellKnownSystemNames = map[string]bool{
	"cluster-admin":                      true,
	"admin":                              true,
	"edit":                               true,
	"view":                               true,
	"cluster-status":                     true,
	"kubeadm:get-nodes":                  true,
	"kubeadm:cluster-admins":             true,
	"kubeadm:kubelet-bootstrap":          true,
	"kubeadm:node-autoapprove-bootstrap": true,
	"kubeadm:node-autoapprove-certificate-rotation": true,
	"kubeadm:node-proxier":                          true,
}

// ClusterCaptureOptions is the resolved cluster-scoped selection for one run.
type ClusterCaptureOptions struct {
	// Include is an allow-list of <group>/<Kind>[/<name>] globs. EMPTY means the curated
	// default (ClusterAllowKinds) rather than "everything": a cluster-scoped capture that
	// defaulted to the whole cluster would sweep in every add-on's objects and make the
	// snapshot both enormous and dangerous to restore.
	Include []string
	// Exclude is applied after Include.
	Exclude []string
}

// ClusterSelector decides which cluster-scoped objects a run captures.
type ClusterSelector struct {
	include []resourcePattern
	exclude []resourcePattern
	// useDefaultAllowList is true when the run named no include, in which case membership is
	// decided by ClusterAllowKinds instead of by patterns.
	useDefaultAllowList bool
}

// CompileClusterSelector prepares the selection for one run.
func CompileClusterSelector(opts ClusterCaptureOptions) (*ClusterSelector, error) {
	inc, err := compilePatterns(opts.Include)
	if err != nil {
		return nil, err
	}
	exc, err := compilePatterns(opts.Exclude)
	if err != nil {
		return nil, err
	}
	return &ClusterSelector{
		include:             inc,
		exclude:             exc,
		useDefaultAllowList: len(inc) == 0,
	}, nil
}

// SelectsKind reports whether a KIND is in scope at all, before any object of it is listed.
// Separate from SelectsObject so the dump can skip a whole resource — and its List call —
// rather than enumerating a kind only to discard every object.
func (s *ClusterSelector) SelectsKind(group, kind string) bool {
	if s.useDefaultAllowList {
		return ClusterAllowKinds[schema.GroupKind{Group: group, Kind: kind}]
	}
	// A name-qualified include still selects its kind: the narrowing happens per object.
	return anyPatternKind(s.include, group, kind)
}

// SelectsObject decides one object, applying the default name exclusions and then the run's own.
func (s *ClusterSelector) SelectsObject(group, kind, name string) bool {
	if !s.SelectsKind(group, kind) {
		return false
	}
	if !s.useDefaultAllowList && !anyPattern(s.include, group, kind, name) {
		return false
	}
	// The control plane's own objects are excluded by DEFAULT, before the run's exclude list —
	// and an explicit include does not bring them back. A run that has to restore
	// system:controller:… is not a DR scenario, it is a broken cluster that needs rebuilding.
	if isSystemOwned(group, kind, name) {
		return false
	}
	return !anyPattern(s.exclude, group, kind, name)
}

// isSystemOwned reports whether an object belongs to the control plane rather than to the
// workloads being protected. Only RBAC carries these conventions; a StorageClass named
// "system:foo" would be a user's own object and is left alone.
func isSystemOwned(group, kind, name string) bool {
	if group != groupRBAC {
		return false
	}
	if kind != kindClusterRole && kind != kindClusterRoleBinding {
		return false
	}
	return strings.HasPrefix(name, systemNamePrefix) || wellKnownSystemNames[name]
}

// anyPatternKind reports whether any pattern could match this kind, ignoring the name segment.
// It is what lets the dump decide to skip a List entirely.
func anyPatternKind(pats []resourcePattern, group, kind string) bool {
	for i := range pats {
		if segMatch(pats[i].group, group) && segMatch(pats[i].kind, kind) {
			return true
		}
	}
	return false
}
