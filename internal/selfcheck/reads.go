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

package selfcheck

import "github.com/CrystalBackup/CrystalBackup/internal/apiconst"

// This file is the ONE place that says what a self-check reads, and it exists because the
// alternative shipped a defect that ran for nine days on a production cluster.
//
// # What happened
//
// 0.6.5 changed internal/exposer.Registry.driverFor to resolve a bound PVC's CSI driver from its
// PersistentVolume instead of from its StorageClass — the right change, and the resolver is what the
// census calls. The resident soak collector runs `selfcheck` daily under its OWN ServiceAccount, and
// that ServiceAccount's ClusterRole (charts/crystal-backup/templates/soak.yaml) listed the core
// resources it needed by hand: namespaces, persistentvolumeclaims, pods, events. Nobody extended it,
// because nothing connected the two files. For nine consecutive nights the census reported 30 of 30
// PVCs as ExposerUnresolvable and the verdict read "volumes that will NOT be backed up" — on a
// cluster that was backing 28 of them up successfully every night.
//
// The mechanism of the defect is not the missing verb. It is that "what selfcheck reads" was written
// down TWICE — once as code that reads, once as a YAML list of resources — with nothing holding the
// two together. A duplicated fact drifts on the first change to either copy, and it drifts silently:
// both files kept rendering, compiling and passing their tests. So the fact is declared once, here,
// and both sides consume it:
//
//   - TestSelfcheckReadsAreDeclared (reads_test.go) runs a whole Collect over a recording reader and
//     fails when a read reaches the API server that is not in this list. Adding a read to this
//     package without declaring it here is therefore a test failure, not a fortnight of somebody's
//     soak.
//   - TestSoakCollectorRoleCoversEverySelfcheckRead (test/chart/soak_test.go) renders the chart and
//     fails when a declared read is not granted by the collector's ClusterRole. Declaring a read
//     without granting it is therefore a test failure too.
//
// The chain is what matters: a new read fails the first gate, declaring it fails the second, and
// only granting it makes both green. Neither gate alone would have caught 0.6.5.
//
// # What this list is NOT
//
// It is not the operator's RBAC. The operator creates, patches and deletes; those grants live in
// charts/crystal-backup/templates/rbac.yaml and are far wider than this. This is the read-only set a
// process needs to PRODUCE A REPORT about a cluster, which is exactly what a soak collector, a
// support-bundle job or a namespace-scoped auditor needs and no more.

// The two verbs and the two core resource names this file repeats, named once. They are shared with
// coverage_reader.go, which fabricates NotFound answers naming the same resources — one spelling for
// one thing, which is the whole argument of this file applied to itself.
const (
	verbGet              = "get"
	verbList             = "list"
	resPersistentVolumes = "persistentvolumes"
	resStorageClasses    = "storageclasses"
)

// APIRead is one kind the self-check reads, why it reads it, and the verbs that make that read
// possible.
//
// Kind rather than the list Kind: a read of PodList and a read of a Pod are the same permission, and
// the recorder strips the "List" suffix before matching. Resource is the RBAC spelling of the same
// thing, carried beside the Kind rather than derived from it — deriving it means pluralisation rules
// (and their exceptions), and a wrong guess here would silently weaken the gate that this file is
// entirely about.
type APIRead struct {
	// Group is the API group, "" for the core group.
	Group string
	// Kind is the singular object Kind as the client asks for it.
	Kind string
	// Resource is the RBAC resource name.
	Resource string
	// Verbs are the verbs a role must carry for this read to succeed.
	Verbs []string
	// Why is the section of the report that stops working without it. It is a sentence rather than a
	// symbol because it is what an administrator reads when they are deciding whether to grant it.
	Why string
	// Ungranted, when non-empty, marks a read that a read-only role deliberately does NOT carry, and
	// says what the report does instead. It is not an oversight to be fixed later: the chart test
	// asserts the collector's role does not grant these, so a future widening has to delete the
	// sentence here first and explain itself.
	Ungranted string
}

// APIReads is every read a self-check makes. Ordered by API group so it reads like the ClusterRole it
// has to be kept in step with.
var APIReads = []APIRead{
	{
		Group: "", Kind: "Namespace", Resource: "namespaces", Verbs: []string{verbList},
		Why: "the cluster schedules' namespace fan-out, which decides which PVCs count as selected",
	},
	{
		Group: "", Kind: "PersistentVolumeClaim", Resource: "persistentvolumeclaims", Verbs: []string{verbList},
		Why: "the per-PVC coverage census itself, and the operator's residual exposure clones",
	},
	{
		// THE 0.6.5 DEFECT. Since 0.6.5 the exposer resolves a bound PVC's CSI driver from its
		// PersistentVolume (internal/exposer.driverFor: the PV names the driver holding the data, a
		// StorageClass is a mutable indirection that can name a different one). Without this read
		// every bound PVC in the cluster resolves to nothing.
		Group: "", Kind: "PersistentVolume", Resource: resPersistentVolumes, Verbs: []string{verbGet, verbList},
		Why: "the CSI driver serving each bound PVC; without it no bound volume's treatment can be " +
			"determined at all",
	},
	{
		Group: "", Kind: "Pod", Resource: "pods", Verbs: []string{verbList},
		Why: "the operator's own chart/app version and the image digests actually running",
	},
	{
		Group: "", Kind: "Secret", Resource: "secrets", Verbs: []string{verbGet},
		Why: "exposer.Precheck asks whether the Secret a VolumeSnapshotClass points its snapshotter " +
			"at exists — by NAME, and it never reads the contents",
		Ungranted: "A report-only identity is not given Secrets. The check degrades to NOT_CHECKABLE, " +
			"which is a first-class verdict here (internal/exposer/precheck.go) and never reads as a " +
			"pass — so the cost of not granting it is one honest 'not verified' line, and the cost of " +
			"granting it is a token that can read every credential in the cluster.",
	},
	{
		Group: "batch", Kind: "Job", Resource: "jobs", Verbs: []string{verbList},
		Why: "the operator's residual mover Jobs, counted as leak indicators",
	},
	{
		// Read only for a PVC that is NOT bound: there the class is the only evidence of a driver
		// there is. It is a small path and it was missing from the collector's role beside the
		// PersistentVolume one, with the same effect on the volumes it touches.
		Group: "storage.k8s.io", Kind: "StorageClass", Resource: resStorageClasses, Verbs: []string{verbGet, verbList},
		Why: "the driver an UNBOUND PVC would get, which is the only case where the resolver consults " +
			"a StorageClass at all",
	},
	{
		Group: snapshotGroup, Kind: kindSnapshot, Resource: "volumesnapshots",
		Verbs: []string{verbList},
		Why:   "the stuck-snapshot observation and the snapshot half of the residue census",
	},
	{
		Group: snapshotGroup, Kind: kindSnapshot + "Content", Resource: "volumesnapshotcontents",
		Verbs: []string{verbList},
		Why:   "the owner-less, Retain-parked contents nothing else in this product looks for",
	},
	{
		Group: snapshotGroup, Kind: kindSnapshotClass, Resource: "volumesnapshotclasses",
		Verbs: []string{verbList},
		Why: "whether this cluster can snapshot a given driver at all — the census stops before it " +
			"starts without it",
	},
	{
		Group: "apiextensions.k8s.io", Kind: "CustomResourceDefinition", Resource: "customresourcedefinitions",
		Verbs: []string{verbList},
		Why:   "whether this product's CRDs are installed, and at which version",
	},
	// The product's own objects. The collector's ClusterRole grants the whole group with a wildcard,
	// so these entries buy nothing there — they are here for the OTHER gate: a section that starts
	// reading a CR kind nobody declared has to come through this list, and a reader of this file gets
	// the complete inventory rather than a wildcard that means "trust us".
	{
		Group: apiconst.Domain, Kind: "Backup", Resource: "backups", Verbs: []string{verbList},
		Why: "the recent-runs history behind the missed-backup and failure rules",
	},
	{
		Group: apiconst.Domain, Kind: "BackupLocation", Resource: "backuplocations", Verbs: []string{verbList},
		Why: "the namespace-plane locations, their readiness and their retention",
	},
	{
		Group: apiconst.Domain, Kind: "ClusterBackupLocation", Resource: "clusterbackuplocations",
		Verbs: []string{verbList},
		Why:   "the cluster-plane locations, and the cluster identity the whole report is keyed on",
	},
	{
		Group: apiconst.Domain, Kind: "BackupRepository", Resource: "backuprepositories", Verbs: []string{verbList},
		Why: "repository health, maintenance freshness and the restic lock story",
	},
	{
		Group: apiconst.Domain, Kind: "BackupSchedule", Resource: "backupschedules", Verbs: []string{verbList},
		Why: "which namespace-plane schedules select which PVCs, and whether they can fire",
	},
	{
		Group: apiconst.Domain, Kind: "ClusterBackupSchedule", Resource: "clusterbackupschedules",
		Verbs: []string{verbList},
		Why:   "the same, for the cluster plane, including the namespace fan-out",
	},
	{
		Group: apiconst.Domain, Kind: "BackupExternalSync", Resource: "backupexternalsyncs", Verbs: []string{verbList},
		Why: "the external-sync mirrors of the namespace plane and their last outcome",
	},
	{
		Group: apiconst.Domain, Kind: "ClusterBackupExternalSync", Resource: "clusterbackupexternalsyncs",
		Verbs: []string{verbList},
		Why:   "the same, for the cluster plane",
	},
	{
		// Not read by this package at all: it is read by internal/alerts' predicates, which the rules
		// section evaluates over the same client (collect.go, alerts.Rules). It is in this list
		// because the recorder test found it and a human audit of THIS package never would have —
		// which is the entire argument for driving the inventory off a recording of real reads
		// rather than off a reading of the code.
		Group: apiconst.Domain, Kind: "ClusterErasure", Resource: "clustererasures", Verbs: []string{verbList},
		Why: "the erasure rule, evaluated through internal/alerts over this command's own reader",
	},
}

// GrantedAPIReads is APIReads minus the reads a report-only role deliberately does not carry. It is
// what a ClusterRole has to cover, and it is a function rather than a second slice so the two can
// never disagree.
func GrantedAPIReads() []APIRead {
	out := make([]APIRead, 0, len(APIReads))
	for _, r := range APIReads {
		if r.Ungranted == "" {
			out = append(out, r)
		}
	}
	return out
}
