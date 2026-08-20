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

// The coverage classes. Seven values, six of them anchored to something the code already declares
// rather than invented here — that anchoring is what keeps this section honest as the resolver
// changes underneath it.
//
// The seventh, CoverageUndetermined, is anchored to nothing in the resolver on purpose: it is the
// class for a volume this command could not form an opinion about, and there is no resolver answer
// that means "I was not allowed to look".
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

	// CoverageUndetermined is the class for "I could not read the object I needed, so I do not know",
	// and it is the one class in this list that says nothing whatsoever about the storage.
	//
	// Every other class is a finding about a volume. This one is a finding about the OBSERVER: a read
	// the resolution depends on was refused (an RBAC policy, a namespace-scoped install, a policy
	// engine) or simply did not come back (a timeout, an apiserver having a bad minute). The volume
	// may be perfectly protected; this command is in no position to say either way.
	//
	// It exists because collapsing it into CoverageUnresolved cost nine days of false alarms. A
	// resident soak collector whose ClusterRole had no read on PersistentVolumes — the object the
	// exposer has resolved a bound PVC's driver from since 0.6.5 — reported 30 of 30 volumes as
	// ExposerUnresolvable every night, under a headline saying they would NOT be backed up, while 28
	// of them were being backed up successfully every night by an operator that DID hold the grant.
	// Every number in that report was arithmetically correct and its meaning was false.
	//
	// The line is drawn at whether the apiserver ANSWERED. A NotFound is an answer — the object is
	// genuinely gone, which is CoverageUnresolved's business and the controller retries it. A
	// Forbidden, an Unauthorized, a timeout or a transport failure is not an answer at all.
	CoverageUndetermined = "ExposerUndetermined"

	// CoverageSnapshotAPIAbsent is the one class that is NOT a per-PVC verdict. It is what every PVC
	// in the census gets when the VolumeSnapshot API is not THERE: the CRDs are not installed, so the
	// apiserver answers that it has never heard of the kind. Resolving 3000 PVCs against an API that
	// is not there would produce 3000 identical errors and one useless report, so the scan stops at
	// the first read, says so once, and classifies the whole cluster.
	//
	// A REFUSED read is not this class and used to be: an identity that may not list
	// VolumeSnapshotClasses learns nothing about the cluster, and saying "nothing here can be backed
	// up" on that evidence is the same false alarm CoverageUndetermined exists to end. That case is
	// CoverageUndetermined, decided once for the whole cluster in the same place. It is a statement about the CLUSTER, and the honest
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
	// SelectionUndetermined says that Unselected and InertOnly are NOT measurements. It is set when a
	// schedule list — or the namespace list the cluster-plane fan-out resolves through — could not be
	// read, which makes "nothing selects this volume" unsupportable for every row at once: the
	// schedule that selects it may be sitting in the list this command was refused.
	//
	// It is a flag beside the counts rather than a zeroing of them because the counts are still the
	// best available reading and a reader may want them; what they may not be is quoted as a finding.
	// Same discipline as the ExposerUndetermined class, applied to the one number in this section that
	// is not a treatment class — and it is here because that number went wrong in exactly the same way
	// and would have gone on doing so after the class was fixed.
	SelectionUndetermined bool `json:"selectionUndetermined,omitempty"`
	// StalledStorage counts PVCs whose treatment class says BACKED UP and whose StorageClass is
	// carrying a VolumeSnapshot that has been bound and not ready past the grace period — a prediction
	// this section can no longer make on its own. See qualifyWithSnapshotEvidence.
	//
	// Its own number, like Unselected, and for the same reason: it is ORTHOGONAL to the treatment. The
	// class is still the class the resolver chose and the row's Verdict is untouched; what has changed
	// is that the cluster is being observed not to complete that treatment, and collapsing the two
	// would let one hide the other.
	StalledStorage int `json:"backedUpOnStorageNotAdvancing,omitempty"`

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

	// SnapshotEvidence is where a PREDICTION becomes qualified by an OBSERVATION, and it is the only
	// field in this row that is not a statement about resolution.
	//
	// Empty on all but one shape of row: a row whose Verdict is backedUp, sitting on a StorageClass this
	// cluster is observed to be failing to snapshot. Empty on a class with no snapshots at all — absence
	// of evidence is not evidence of failure, and a section that maligned every StorageClass nobody has
	// backed up yet would be worthless on the fresh installation it is written for.
	//
	// The row's Verdict and Class are NOT touched by it. What the operator will do to this volume is
	// still what the resolver says; what this sentence adds is that the cluster is not currently
	// finishing that job, which is a fact about the storage's behaviour and not a verdict this report is
	// entitled to reach.
	SnapshotEvidence string `json:"snapshotEvidence,omitempty"`
	// StuckOnStorageClass and ReadyOnStorageClass are the counts behind that sentence, over every
	// VolumeSnapshot in the cluster whose source PVC names this row's StorageClass. Carried as numbers
	// as well as prose because a machine reading this document should not have to parse English to find
	// out that the evidence was thin (one stuck snapshot) or overwhelming (eight stuck, none ready).
	StuckOnStorageClass int `json:"stuckSnapshotsOnStorageClass,omitempty"`
	ReadyOnStorageClass int `json:"readySnapshotsOnStorageClass,omitempty"`
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
	case CoverageUnresolved, CoverageSnapshotAPIAbsent, CoverageUndetermined:
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
	case CoverageUndetermined:
		return "NOT DETERMINED — this is about THIS REPORT, not about your data: a read it needed " +
			"was refused or failed, so the treatment could not be worked out. It is not a finding " +
			"that these volumes are unprotected"
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
	case CoverageUndetermined:
		// No pair, and the empty return is the statement. The controller reconciles with the
		// OPERATOR's RBAC; a read THIS command was refused predicts nothing about what will land in
		// status.volumes[]. Naming a phase here would print a confident prediction on the one row
		// whose entire content is "I do not know".
		return "", ""
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
	// Above the storage findings rather than below them, though it is not one. A reader whose census
	// is partly blind must learn that BEFORE they read any of the counts underneath, because it
	// changes what those counts mean — and the remedy (a grant) is usually a two-line edit, which
	// makes it the cheapest thing on the list to act on.
	case CoverageUndetermined:
		return 2
	case CoverageUnresolved:
		return 3
	case CoverageUnsupported:
		return 4
	case CoverageDirect:
		return 6
	case CoverageClone:
		return 7
	default:
		// An unrecognised Kind sorts between the problems and the known-good classes: it is not a
		// failure, and it is not something this build can vouch for either.
		return 5
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
// snapshotAPIUnreadableNote is the Coverage.Note when the VolumeSnapshotClass list was REFUSED rather
// than absent. It is a separate sentence from snapshotAPIAbsentNote because the two send a reader to
// completely different places: one to install a CRD, the other to look at the identity this command
// ran as. Printing the first when the second is true is what makes an administrator go and check a
// storage stack that was never broken.
const snapshotAPIUnreadableNote = "The VolumeSnapshotClass list could not be READ by the identity " +
	"this command ran as — it was refused, or the request failed — so no PVC's treatment could be " +
	"determined. This is a statement about this report's permissions and NOT about your data: the " +
	"volumes below may well be backed up perfectly. The operator reconciles with its own, wider " +
	"ClusterRole, so it is entirely possible for every backup in this cluster to be succeeding while " +
	"this section can see nothing. Grant this identity `list` on volumesnapshotclasses and run it " +
	"again. The diagnostics section carries the error."

const snapshotAPIAbsentNote = "The VolumeSnapshotClass list could not be read, so no PVC's " +
	"treatment could be determined and the scan stopped rather than reporting one identical error " +
	"per volume. Either the snapshot.storage.k8s.io CRDs are not installed — in which case NOTHING " +
	"in this cluster can be backed up until they are — or the operator's ClusterRole has lost its " +
	"read on them. The diagnostics section carries the error."
