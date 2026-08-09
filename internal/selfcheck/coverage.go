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

import (
	"github.com/CrystalBackup/CrystalBackup/internal/exposer"
	"github.com/CrystalBackup/CrystalBackup/internal/status"
)

// This file declares the PVC coverage census: the answer to "what will and will not be backed up",
// per PVC, with the treatment class.
//
// # Why the census exists
//
// Every other section of this report describes the operator's own objects. None of them answers the
// question an administrator asks thirty seconds after `helm install`: is my data going to be backed
// up? The object inventory cannot answer it, because a location that is Ready and a schedule that is
// firing on time tell you nothing about the PVC on rancher.io/local-path that will be silently
// skipped every night, and nothing at all about the PVC that no schedule selects. Those two are the
// installation defects this section exists to make visible, and both are invisible today.
//
// # Where the verdict comes from
//
// From internal/exposer, and from nowhere else. The Class of a PVC that CAN be backed up is
// literally the string internal/exposer.SnapshotExposer.Kind() returns for it — not a mapping table
// keyed on the driver, not a copy of the routing rule, the Kind of the exposer Registry.For actually
// resolved. The classes for a PVC that cannot are the sentinel errors the same resolver returns
// (ErrUnsupported, ErrPrecheckFailed) and the fallback the Backup controller records for everything
// else.
//
// That is a hard rule with its own history in this repository: see the package doc of
// hack/gen-preflight-table, § "Why this is generated". A second copy of the routing logic, shipped
// to users as an authoritative preview of what the operator will do, drifts on the first change to
// the routing and drifts SILENTLY — it keeps printing confident verdicts either way. A self-check
// that guessed the exposer from the StorageClass provisioner would be that same defect, one step
// closer to the user, because it would be printed by the operator's own binary.
//
// # What is deliberately absent
//
// A "best-effort" or filesystem class. The product owner's vocabulary for this feature included one,
// and this version has no such mode: snapshot-based exposure is the only thing 0.6.x implements, so a
// PVC whose storage cannot snapshot is not backed up at all, by any weaker means. Showing a class
// this build cannot deliver would be the one failure mode this whole package exists to refuse — a
// reader deciding they are covered by a mechanism that does not exist. bestEffortNote says so in
// words instead, once, in the section where somebody would go looking for it.

// The coverage classes. Six values, and each one is anchored to something the code already declares
// rather than invented here — that anchoring is what keeps this section honest as the resolver
// changes underneath it.
const (
	// CoverageClone is exposer.KindCSIGeneric: a VolumeSnapshot, then a temporary PVC PROVISIONED
	// FROM that snapshot which the mover reads. This is the "with copy" half of the product owner's
	// pair — with the caveat clonedCopyNote records, because on a driver whose
	// create-from-snapshot is copy-on-write (Ceph RBD is the reference case) the provisioning is a
	// lazy COW clone and moves nothing. What is always true is that a SECOND volume is created; how
	// much data crosses to fill it is the driver's business, not the operator's.
	CoverageClone = exposer.KindCSIGeneric
	// CoverageDirect is exposer.KindCephFSShallow: a VolumeSnapshot, then a ReadOnlyMany temporary
	// PVC that REFERENCES the snapshot rather than cloning it (ceph-csi's backingSnapshot). This is
	// the "without copy" half, and here the claim is unconditional — the exposer exists precisely
	// because a normal create-from-snapshot on CephFS is an O(data) subvolume copy.
	CoverageDirect = exposer.KindCephFSShallow

	// CoverageUnsupported is the class for exposer.ErrUnsupported: no VolumeSnapshotClass names this
	// volume's CSI driver, or the volume is not a CSI volume at all. The value is the VolumeStatus
	// reason the Backup controller records, so a row here and a row in `kubectl get backup -o yaml`
	// read the same. Its phase is Skipped, which is NEUTRAL in the Backup roll-up: a namespace
	// holding one permanently unsnapshottable PVC must not alarm on every run forever, which is
	// exactly why this section has to say it out loud — nothing else will.
	CoverageUnsupported = "CSISnapshotUnsupported"
	// CoveragePrecheckFailed is the class for exposer.ErrPrecheckFailed: the VolumeSnapshotClass
	// exists, names a live driver, and points the snapshotter at credentials that are not there.
	//
	// It is kept apart from CoverageUnsupported with some determination, because merging them is the
	// mistake that makes this section useless. "This storage can never be snapshotted" is a fact
	// about the storage that nobody is going to fix; "this cluster cannot snapshot it today" is a
	// cluster somebody broke and somebody can fix, and it fails the volume rather than skipping it.
	// One is a note, the other is work.
	CoveragePrecheckFailed = "SnapshotPrecheckFailed"
	// CoverageUnresolved is the class for every other resolution failure — a StorageClass deleted
	// out from under an unbound PVC that still names it, a PersistentVolume that cannot be read, an
	// API server having a bad minute. The Backup controller neither skips nor fails these: it parks
	// the volume and retries it until a deadline, because the cause may be permanent or may be a
	// cache that has not caught up and guessing is wrong in both directions. This section reports
	// the same restraint: the class names the cause, the Detail quotes the resolver, and neither
	// claims to know which it is.
	CoverageUnresolved = "ExposerUnresolvable"

	// CoverageSnapshotAPIAbsent is the one class that is NOT a per-PVC verdict. It is what every PVC
	// in the census gets when the VolumeSnapshot API itself could not be read — no CRDs, or no RBAC
	// on VolumeSnapshotClasses. Resolving 3000 PVCs against an API that is not there would produce
	// 3000 identical errors and one useless report, so the scan stops at the first read, says so
	// once, and classifies the whole cluster. It is a statement about the CLUSTER, and the honest
	// reading is "nothing here can be backed up until the snapshot API is installed".
	CoverageSnapshotAPIAbsent = "VolumeSnapshotAPIAbsent"
)

// maxCoverageItems caps the per-PVC rows the document carries.
//
// A cap is unavoidable — a 3000-PVC cluster would otherwise put 600 KB of rows into a file whose
// whole purpose is to be small enough to attach to an issue — but WHICH rows get dropped is the
// decision that matters, and it is not "the last ones the List returned". The scan sorts by
// attention before it truncates (see coverageOrder), so the classes an administrator has to act on
// are recorded first and the healthy majority is what falls off the end. A cap that could hide a
// skipped PVC behind nine hundred fine ones would defeat the section.
const maxCoverageItems = 500

// Coverage is the per-PVC census: how many volumes fall in each treatment class, which ones need
// attention, and how much this cost to find out.
type Coverage struct {
	// PVCs is every PVC the scan considered, and Namespaces how many namespaces they came from.
	// Both EXCLUDE the operator's own temporary exposure PVCs — those are the operator's residue,
	// counted in leakIndicators, and listing them here as unprotected user data would be a lie the
	// reader has no way to spot.
	PVCs       int `json:"pvcs"`
	Namespaces int `json:"namespaces"`
	// CapacityBytes is the summed requested capacity of every PVC counted, and BackedUpBytes the
	// part of it in a class that will actually be backed up. The pair is the one number an
	// administrator can sanity-check a bucket bill against.
	CapacityBytes int64 `json:"capacityBytes"`
	BackedUpBytes int64 `json:"backedUpBytes"`
	// Classes is the tally, in attention order (worst first), one entry per class that has at least
	// one PVC in it. Classes with no members are omitted rather than reported as zero: a healthy
	// installation should read as a short list, not as a form with blanks.
	Classes []CoverageTally `json:"classes,omitempty"`

	// Unselected counts PVCs that NO schedule selects. This is the single most valuable line this
	// command can print for a fresh installation and it is invisible everywhere else: the object is
	// healthy, its storage snapshots perfectly, and nothing in this cluster will ever back it up.
	// It is deliberately its own number rather than a class, because it is ORTHOGONAL to the
	// treatment — a PVC can be both unselected and unsnapshottable, and collapsing the two would
	// let one hide the other.
	Unselected int `json:"unselected"`
	// InertOnly counts PVCs selected only by schedules that cannot fire: suspended, or carrying a
	// cron expression that does not parse. Not merged into Unselected, because the remedy is
	// different and much smaller — there IS a schedule, it is just switched off.
	InertOnly int `json:"selectedOnlyByInertSchedules"`

	// Items are the per-PVC rows, attention-first (see maxCoverageItems). Redacted like every other
	// identifier in this document.
	Items []CoveredPVC `json:"items,omitempty"`
	// ItemsOmitted is how many PVCs are counted in Classes but carry no row here. Stated rather
	// than implied, so a reader who counts the rows and gets a smaller number than PVCs knows why.
	ItemsOmitted int `json:"itemsOmitted,omitempty"`

	// APIReads is how many API requests the scan made. It is REPORTED, not tuned: this section is
	// the one part of the report whose cost grows with the size of the cluster, and a number in the
	// document is what stops that growing unnoticed. See coverageReader for why it is a small
	// constant rather than a multiple of PVCs.
	APIReads int `json:"apiReads"`

	// Note is the caveat block: what "with copy" really means on a COW driver, and the fact that
	// this version has no weaker fallback for a PVC it cannot snapshot.
	Note string `json:"note,omitempty"`
}

// CoverageTally is one class's count, with the words that explain it. The sentence travels WITH the
// count, in the JSON as well as on the page, so that a reader of the raw document does not have to
// know what "cephfs-shallow" means to understand what will happen to their data.
type CoverageTally struct {
	Class string `json:"class"`
	Count int    `json:"count"`
	// Verdict is the one-word answer: CoverageVerdictBackedUp, CoverageVerdictSkipped,
	// CoverageVerdictFailed or CoverageVerdictUnknown. It is what a reader who reads nothing else
	// sees, and it is what the text renderer sorts and highlights on.
	Verdict string `json:"verdict"`
	// Summary is the plain sentence: what the operator will do to a PVC in this class, or why it
	// will not.
	Summary string `json:"summary"`
	// Phase and Reason are the status.volumes[] pair the Backup controller will record for a PVC in
	// this class, so this section and `kubectl get backup -o yaml` can be reconciled against each
	// other. Empty for the classes that complete normally.
	Phase  string `json:"phase,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// The four verdicts a class rolls up to. Four rather than three because "unknown" is a verdict: a
// PVC whose exposer could not be resolved has not been declared safe and has not been declared
// lost, and this package's whole discipline is that an absence of measurement never renders as an OK.
const (
	CoverageVerdictBackedUp = "backedUp"
	CoverageVerdictSkipped  = "skipped"
	CoverageVerdictFailed   = "failed"
	CoverageVerdictUnknown  = "unknown"
)

// CoveredPVC is one PVC's row.
type CoveredPVC struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Class     string `json:"class"`
	Verdict   string `json:"verdict"`
	// StorageClass is the name off the PVC's own spec, NOT the driver.
	//
	// The driver is absent on purpose. Learning it means resolving a PersistentVolume and applying
	// the CSI-migration rules, i.e. re-implementing Registry.driverFor — and this section's first
	// rule is that it re-implements nothing about resolution. Where the driver is diagnostically
	// necessary it is already in Detail, quoted from the resolver's own error, which also names
	// WHERE the driver came from (the PersistentVolume or the class). That is strictly more
	// information than a field could carry, and it cannot drift.
	StorageClass string `json:"storageClass,omitempty"`
	// Bound reports whether the PVC has a PersistentVolume. An unbound PVC has no data to back up
	// yet, and it is also the only case in which the resolver consults the StorageClass at all, so
	// it changes how the rest of this row should be read.
	Bound bool `json:"bound"`
	// CapacityBytes is the requested capacity (spec.resources.requests.storage), which is what an
	// administrator sizing a bucket has; the actual usage is not knowable from here.
	CapacityBytes int64 `json:"capacityBytes,omitempty"`

	// Selected is true when at least one schedule that CAN fire selects this PVC. Schedules is the
	// identity of those schedules (redacted, capped by maxCoveredSchedules), which is the answer to
	// "who is responsible for this volume".
	Selected  bool     `json:"selected"`
	Schedules []string `json:"schedules,omitempty"`
	// InertSchedules names schedules that select this PVC but cannot fire — suspended, or with an
	// unparseable cron. A PVC whose only entry is here is covered on paper and unprotected in fact.
	InertSchedules []string `json:"inertSchedules,omitempty"`

	// Detail is the resolver's own sentence for a class that is not a plain success — it names the
	// driver, the object and where the answer came from. Redacted through Redactor.Detail, because
	// it is free text containing object names, which is the one shape per-field redaction cannot
	// reach.
	Detail string `json:"detail,omitempty"`
}

// maxCoveredSchedules caps the schedule list on one row. Two is enough to answer "is anything
// looking after this" and to show an overlap; a PVC selected by nine schedules is a finding in
// itself, and the count is what says so.
const maxCoveredSchedules = 3

// coverageVerdict maps a class to its one-word verdict. Written as a switch over the class
// constants rather than as a field on a table, so adding an exposer Kind to internal/exposer
// produces the honest default (unknown) instead of silently inheriting "backed up".
func coverageVerdict(class string) string {
	switch class {
	case CoverageClone, CoverageDirect:
		return CoverageVerdictBackedUp
	case CoverageUnsupported:
		return CoverageVerdictSkipped
	case CoveragePrecheckFailed:
		return CoverageVerdictFailed
	case CoverageUnresolved, CoverageSnapshotAPIAbsent:
		return CoverageVerdictUnknown
	default:
		// A Kind this build has never heard of. It resolved, so the operator WILL try to back the
		// volume up — but this binary cannot describe how, and saying "backed up" on its behalf
		// would be a claim it has no basis for.
		return CoverageVerdictUnknown
	}
}

// coverageSummary is the plain sentence for a class. Every one of them is a statement about what the
// operator does, phrased for somebody who has never read this repository — which is the audience the
// product owner asked for.
func coverageSummary(class string) string {
	switch class {
	case CoverageClone:
		return "backed up: a CSI snapshot, then a temporary volume provisioned from it for the " +
			"mover to read"
	case CoverageDirect:
		return "backed up: a CSI snapshot mounted directly, read-only — no second copy of the data"
	case CoverageUnsupported:
		return "NOT backed up: this storage has no CSI snapshot support, so the volume is skipped " +
			"on every run"
	case CoveragePrecheckFailed:
		return "NOT backed up: the snapshot class exists but this cluster cannot serve a snapshot " +
			"on it today — fixable"
	case CoverageUnresolved:
		return "UNKNOWN: the exposer could not be resolved; the operator will retry and give up at " +
			"a deadline"
	case CoverageSnapshotAPIAbsent:
		return "UNKNOWN: the VolumeSnapshot API could not be read, so no volume's treatment could " +
			"be determined"
	default:
		return "backed up by exposer " + class + ", which this build of selfcheck cannot describe"
	}
}

// coveragePhaseReason is the (phase, reason) pair the Backup controller will record in
// status.volumes[] for a PVC in this class, so a reader can join this section to a real Backup
// object.
//
// The reason strings are the controller's own constants, which are unexported
// (internal/controller.backupReasonSkippedUnsupported and friends) and live in a file this lot does
// not own. They are repeated here rather than imported, and TestCoverageReasonsMatchTheController
// pins them against the declarations in internal/controller/backup_controller.go so the repetition
// cannot become a divergence. Two of the three are already documented there as verbatim cross-repo
// contracts asserted by the crucible, which is what makes pinning them cheap and safe.
func coveragePhaseReason(class string) (phase, reason string) {
	switch class {
	case CoverageUnsupported:
		return string(status.VolumePhaseSkipped), CoverageUnsupported
	case CoveragePrecheckFailed:
		return string(status.VolumePhaseFailed), CoveragePrecheckFailed
	case CoverageUnresolved:
		// Pending, not Failed, and that is the whole point of this class: the controller parks the
		// volume and retries it. It becomes Failed only when pendingResolveDeadline runs out.
		return string(status.VolumePhasePending), CoverageUnresolved
	default:
		return "", ""
	}
}

// coverageOrder is the attention order: the classes an administrator must act on first, the ones
// that are working last. It is the sort key for both the class tally and the per-PVC rows, and it is
// what makes maxCoverageItems safe to apply — truncation drops the tail, so the tail must be the
// part nobody needs.
func coverageOrder(class string) int {
	switch class {
	case CoveragePrecheckFailed:
		return 0
	case CoverageSnapshotAPIAbsent:
		return 1
	case CoverageUnresolved:
		return 2
	case CoverageUnsupported:
		return 3
	case CoverageDirect:
		return 5
	case CoverageClone:
		return 6
	default:
		// An unrecognised Kind sorts between the problems and the known-good classes: it is not a
		// failure, and it is not something this build can vouch for either.
		return 4
	}
}

// coverageNote is the caveat block. Both halves of it exist because a count without them would be
// read as a promise the operator does not make.
const coverageNote = "\"Temporary volume provisioned from the snapshot\" (csi-generic) does not " +
	"always mean the data is copied: on a driver whose create-from-snapshot is copy-on-write — Ceph " +
	"RBD is the reference case — provisioning is a lazy clone and moves nothing. What is always true " +
	"is that a second volume is created. " + bestEffortNote

// bestEffortNote is the honest answer to "what about best-effort / filesystem copy?".
//
// It is a sentence and not a class, and the difference matters more than it looks. This version
// implements snapshot-based exposure and nothing else, so there is no weaker mode a skipped PVC
// falls back to: it is simply not backed up. A reader who saw a "best-effort" column — even an empty
// one — would conclude that something catches those volumes. Nothing does.
const bestEffortNote = "This version has no filesystem or \"best-effort\" copy mode: a PVC whose " +
	"storage cannot be snapshotted is not backed up at all, by any weaker means."

// snapshotAPIAbsentNote is the Coverage.Note when the scan could not read the VolumeSnapshot API and
// gave up before resolving anything.
const snapshotAPIAbsentNote = "The VolumeSnapshotClass list could not be read, so no PVC's " +
	"treatment could be determined and the scan stopped rather than reporting one identical error " +
	"per volume. Either the snapshot.storage.k8s.io CRDs are not installed — in which case NOTHING " +
	"in this cluster can be backed up until they are — or the operator's ClusterRole has lost its " +
	"read on them. The diagnostics section carries the error."
