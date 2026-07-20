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

package sanitize

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Default exclusions (spec/04-manifest-backup.md §2.2). Every excluded object is transient,
// controller-owned, or rebuilt by the target cluster — capturing it would at best waste space
// and at worst fight the controller that owns it on restore.
//
// The list is compiled in and versioned with the mover image rather than being configurable:
// what a backup contains is a contract a restore depends on, and a per-tenant knob here would
// make "what did I actually capture" unanswerable after the fact.
//
// The recurring judgement in this list is *who owns the object*. A ReplicaSet a Deployment
// created will be recreated by that Deployment; a ReplicaSet someone wrote by hand will not,
// so it stays. The same reasoning distinguishes control-plane-managed EndpointSlices from the
// hand-managed ones behind a selectorless Service.
const (
	ExcludeControllerOwnedPod        = "E1"
	ExcludeDeploymentOwnedReplicaSet = "E2"
	ExcludeCronJobOwnedJob           = "E3"
	ExcludeManagedEndpoints          = "E4"
	ExcludeEvent                     = "E5"
	ExcludeLease                     = "E6"
	ExcludeServiceAccountToken       = "E7"
	ExcludeCertManagerTransient      = "E8"
	ExcludeVolumeSnapshot            = "E9"
	ExcludeOwnRunRecord              = "E10"
	ExcludeRootCAConfigMap           = "E11"
)

// ExcludeContext carries the namespace-wide facts a per-object decision cannot see on its
// own. Today that is only E4: whether an Endpoints object is backed by a Service with a
// selector (control-plane managed, drop it) or a selectorless one (hand-managed, keep it).
type ExcludeContext struct {
	// ServicesWithSelector holds the names of Services in the namespace that declare a
	// spec.selector. An Endpoints/EndpointSlice for one of those is rebuilt by the control
	// plane; one for a selectorless Service is user intent and must survive.
	ServicesWithSelector map[string]bool
}

// API groups referenced by the exclusion rules.
const (
	groupCore          = ""
	groupApps          = "apps"
	groupBatch         = "batch"
	groupCoordination  = "coordination.k8s.io"
	groupDiscovery     = "discovery.k8s.io"
	groupEvents        = "events.k8s.io"
	groupSnapshot      = "snapshot.storage.k8s.io"
	groupCertManager   = "cert-manager.io"
	groupACME          = "acme.cert-manager.io"
	groupCrystalBackup = "crystalbackup.io"
)

// ShouldExclude reports whether an object is dropped from the dump, and which rule decided.
// The rule id is returned so the decision is traceable in index.json rather than showing up
// as an unexplained absence months later.
//
// Split into three passes because they answer different questions: is this a transient of a
// known operator, is this kind inherently ephemeral, and is something else going to recreate
// this object.
func ShouldExclude(obj *unstructured.Unstructured, ctx ExcludeContext) (bool, string) {
	if obj == nil {
		return false, ""
	}
	gvk := obj.GroupVersionKind()
	group, kind := gvk.Group, gvk.Kind

	for _, check := range []func(*unstructured.Unstructured, string, string, ExcludeContext) (bool, string){
		excludeCertManagerTransient,
		excludeEphemeralKind,
		excludeRecreatedByAController,
	} {
		if excluded, rule := check(obj, group, kind, ctx); excluded {
			return true, rule
		}
	}
	return false, ""
}

// E8. The http01-solver label is checked first because it marks solver Pods, Services AND
// Ingresses — a per-kind branch would claim them before the label ever got a look.
func excludeCertManagerTransient(obj *unstructured.Unstructured, group, kind string, _ ExcludeContext) (bool, string) {
	if obj.GetLabels()["acme.cert-manager.io/http01-solver"] == "true" {
		return true, ExcludeCertManagerTransient
	}
	switch {
	case group == groupACME && (kind == "Order" || kind == "Challenge"),
		// The kept Certificate (and its kept TLS Secret) reissues this.
		group == groupCertManager && kind == "CertificateRequest":
		return true, ExcludeCertManagerTransient
	}
	return false, ""
}

// E5, E6, E7, E9, E10, E11 — kinds (or specific objects) that are operational noise, bound to
// this cluster, or history rather than desired state.
func excludeEphemeralKind(obj *unstructured.Unstructured, group, kind string, _ ExcludeContext) (bool, string) {
	switch {
	case kind == "Event" && (group == groupCore || group == groupEvents):
		return true, ExcludeEvent

	case kind == "Lease" && group == groupCoordination:
		return true, ExcludeLease

	case kind == "VolumeSnapshot" && group == groupSnapshot:
		// Bound to cluster-local VolumeSnapshotContents, meaningless elsewhere. Also covers
		// the transient snapshots this operator creates during a run.
		return true, ExcludeVolumeSnapshot

	case group == groupCrystalBackup && (kind == "Backup" || kind == "Restore"):
		// Run records and operation records — history, not desired state. BackupSchedule and
		// BackupLocation are user configuration and are deliberately kept.
		return true, ExcludeOwnRunRecord

	case group == groupCore && kind == "ConfigMap" && obj.GetName() == "kube-root-ca.crt":
		return true, ExcludeRootCAConfigMap

	case group == groupCore && kind == "Secret":
		if t, _, _ := unstructured.NestedString(obj.Object, "type"); t == "kubernetes.io/service-account-token" {
			// Bound to a ServiceAccount UID from the source cluster; re-minted on demand.
			return true, ExcludeServiceAccountToken
		}
	}
	return false, ""
}

// E1–E4 — objects something else will rebuild. The judgement throughout is ownership: an
// object its controller will recreate is noise, the same kind written by hand is the only
// record of someone's intent.
func excludeRecreatedByAController(obj *unstructured.Unstructured, group, kind string, ctx ExcludeContext) (bool, string) {
	switch {
	case group == groupCore && kind == "Pod":
		if hasControllerOwner(obj, "") {
			return true, ExcludeControllerOwnedPod
		}

	case group == groupApps && kind == "ReplicaSet":
		if hasControllerOwner(obj, "Deployment") {
			return true, ExcludeDeploymentOwnedReplicaSet
		}

	case group == groupBatch && kind == "Job":
		if hasControllerOwner(obj, "CronJob") {
			return true, ExcludeCronJobOwnedJob
		}

	case kind == "EndpointSlice" && group == groupDiscovery:
		if obj.GetLabels()["endpointslice.kubernetes.io/managed-by"] == "endpointslice-controller.k8s.io" {
			return true, ExcludeManagedEndpoints
		}

	case group == groupCore && kind == "Endpoints":
		// An Endpoints object shares its Service's name. If that Service has a selector the
		// control plane rebuilds these; if it does not, someone is populating them by hand
		// and this object is the only record of it.
		if ctx.ServicesWithSelector[obj.GetName()] {
			return true, ExcludeManagedEndpoints
		}
	}
	return false, ""
}

// hasControllerOwner reports whether obj has an ownerReference with controller: true. When
// ofKind is non-empty the owner must also be of that kind — the difference between "any
// controller owns this Pod" (E1) and "a Deployment owns this ReplicaSet" (E2).
func hasControllerOwner(obj *unstructured.Unstructured, ofKind string) bool {
	for _, ref := range obj.GetOwnerReferences() {
		if ref.Controller == nil || !*ref.Controller {
			continue
		}
		if ofKind == "" || ref.Kind == ofKind {
			return true
		}
	}
	return false
}
