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

// Package selfcheck produces the exportable installation report behind
// `crystal-backup selfcheck` and `crystal-backup report`.
//
// # What it is for
//
// Two questions, and neither of them has an answer today. "Is my installation healthy?" — asked by
// an operator with no Prometheus, which on day one is most of them. And "what does your
// installation look like?" — asked by a maintainer of someone who has just filed an issue, which
// currently costs a week of round-trips.
//
// # The three properties that make it worth having
//
// RUNNING, NOT DECLARED. Every image digest here is read from status.containerStatuses[].imageID —
// what the kubelet actually pulled — never from the chart's values. 0.5.1 shipped a supply chain
// that verified nothing for four releases behind a green pipeline, and the lesson generalises: a
// report of what was CONFIGURED is a report of somebody's intent, and intent is exactly what is
// wrong when something is wrong.
//
// ONE DECLARATION OF EVERY THRESHOLD. The rule verdicts are produced by running each
// alerts.Rule.Predicate over live objects. The bound it compares against is read out of the same
// alerts.Rule.Threshold the PromQL was assembled from, so the self-check and the alerting cannot
// disagree about when something is late — there is nothing to keep in sync. The predicate hangs off
// the rule for the same reason: a rule and the code that answers it are declared in one place.
//
// NOT-EVALUATED IS NOT OK. A rule with no predicate, or whose predicate errored, is reported as
// exactly that. An unmeasured OK is the failure mode this whole lot exists to prevent, and it is
// worth saying plainly that a report can be almost entirely "not evaluated" and still be honest,
// while one line of unearned green is not.
//
// # Redaction is on by default
//
// This file is designed to be attached to a public issue, and it would otherwise carry a list of
// somebody's customers: namespaces, tenants, PVCs, buckets, the S3 endpoint, the cluster ID. The
// default mode replaces each with a salted hash — stable within one report, so `ns-a3f2c1d9` is the
// same namespace everywhere in it, and useless outside it. See redact.go for why the salt is random
// per report and is never written into it.
package selfcheck

import "time"

// ReportVersion is the schema version of the JSON below.
//
// It is here because these files will be read for years inside issues, by a `crystal-backup report`
// newer than the operator that produced them, and the renderer has to be able to tell. Bump it when
// a consumer that understood the previous version could MISREAD this one — a field whose meaning
// changed, a removal, a unit change. Adding a field does not require a bump; every consumer in this
// repository ignores unknown fields, and the renderer treats an absent section as absent rather
// than as empty.
const ReportVersion = 1

// Report is the whole document. Field order is the reading order of the rendered page: what this
// is, what is running, what exists, and only then the verdicts — because a verdict is not
// actionable without the installation it was reached on.
type Report struct {
	ReportVersion int       `json:"reportVersion"`
	GeneratedAt   time.Time `json:"generatedAt"`
	// Redaction describes what was done to the identifiers below, so a reader knows whether they
	// are looking at names or at tokens.
	Redaction Redaction `json:"redaction"`

	Operator  Operator     `json:"operator"`
	Cluster   Cluster      `json:"cluster"`
	Images    Images       `json:"images"`
	CRDs      CRDInventory `json:"crds"`
	Inventory Inventory    `json:"inventory"`

	// Plan is what the custom resources are going to DO, in sentences: what each schedule targets,
	// when it next fires, how long the data is kept, when the next exclusive prune window is, and
	// the validity problems that only show up when two objects are read against each other. See
	// plan.go — it is not a second rendering of Inventory, it is the part a field dump cannot say.
	//
	// A POINTER, so that a report from a producer too old to have collected it renders as "not
	// collected" rather than as an installation with no locations and no schedules. That distinction
	// is the same one Diagnostics exists to preserve everywhere else in this document.
	Plan *Plan `json:"plan,omitempty"`
	// Coverage is the per-PVC census: what will and will not be backed up, with the treatment class,
	// resolved by the operator's own exposer registry rather than by any copy of its routing rules.
	// See coverage.go. A pointer for the same reason Plan is one.
	Coverage *Coverage `json:"coverage,omitempty"`

	Leaks   Leaks        `json:"leakIndicators"`
	Rules   []RuleResult `json:"rules"`
	Verdict Verdict      `json:"verdict"`

	// Diagnostics record what could NOT be read, and why. An empty section elsewhere in this
	// document means "nothing there"; a diagnostic is how the report distinguishes that from "the
	// operator was not allowed to look".
	Diagnostics []Diagnostic `json:"diagnostics,omitempty"`
}

// Redaction states the mode and the algorithm — and, by its silence, the salt.
type Redaction struct {
	// Mode is ModeHashed (the default) or ModeFull.
	Mode string `json:"mode"`
	// SaltSource distinguishes the THREE hashed modes, which make different promises and are
	// otherwise indistinguishable from the outside: tokens from a random salt are meaningless
	// outside their own report; tokens from a caller-supplied one correlate across every report
	// built on the same salt and can be reversed only by whoever holds the file; tokens from a
	// namespace-UID-derived one correlate the same way and can be reversed by anyone who can read
	// that namespace. A reader deciding whether a file is safe to attach to a public issue is
	// deciding on this field. Empty in full mode, where nothing is salted at all.
	SaltSource string `json:"saltSource,omitempty"`
	// Algorithm names the construction so a reader can reason about what a token does and does not
	// reveal, without having to trust a one-word mode name.
	Algorithm string `json:"algorithm,omitempty"`
	// SaltDisclosed is always false and is stated rather than implied. A reader who wonders whether
	// they can de-anonymise a report someone sent them deserves the answer in the document.
	SaltDisclosed bool `json:"saltDisclosed"`
	// Note is the human sentence: what survives redaction and what does not.
	Note string `json:"note,omitempty"`
}

// The two redaction modes. Named because three call sites now emit the hashed one and a typo in
// any of them would render a report whose mode nothing recognises.
const (
	// ModeHashed replaces identifiers with tokens. The default in every path.
	ModeHashed = "hashed"
	// ModeFull passes identifiers through verbatim.
	ModeFull = "full"
)

// The values of Redaction.SaltSource. They are compared by the renderer and by anything reading a
// stack of these files, so they are constants rather than repeated string literals.
const (
	// SaltRandomPerReport is the default: 32 bytes from crypto/rand, discarded with the process.
	SaltRandomPerReport = "random-per-report"
	// SaltCallerSupplied means --redaction-salt-file was used and the tokens correlate across every
	// report produced from that same file.
	SaltCallerSupplied = "caller-supplied"
	// SaltNamespaceUID means the salt was DERIVED from the operator namespace's UID rather than
	// generated or supplied. It correlates like a caller-supplied salt does — that is its purpose,
	// it is the soak collector's default — but it is reversible to anyone who can read the
	// namespace, which a caller-supplied salt is not. Stated as its own value rather than folded
	// into caller-supplied precisely because those two promises are different.
	SaltNamespaceUID = "namespace-uid"
)

// SaltNamespaceUIDDomain is the domain separator the namespace-UID derivation hashes under:
// SHA256(SaltNamespaceUIDDomain || uid). It lives here, beside the SaltSource value it explains,
// because the report's Algorithm line quotes it and the derivation (internal/soak) computes with
// it — one definition, so the sentence a reader is given and the bytes they were given can never
// describe different constructions. The "-v1" is load-bearing: revising the derivation means a
// new domain, not the same key computed differently.
const SaltNamespaceUIDDomain = "crystalbackup-soak-salt-v1"

// Operator is what the binary that produced this report knows about itself, from three
// independent sources — because they disagree, and the disagreement is diagnostic.
type Operator struct {
	// BuildVersion is internal/metrics.Version, i.e. crystalbackup_build_info's `version` label.
	BuildVersion string `json:"buildVersion"`
	// ChartVersion and AppVersion come off the operator Pod's own labels (helm.sh/chart,
	// app.kubernetes.io/version), so they describe the RELEASE that is running rather than the
	// build the binary thinks it is.
	ChartVersion string `json:"chartVersion,omitempty"`
	AppVersion   string `json:"appVersion,omitempty"`
	// Namespace is the operator namespace: where the platform Secrets and every mover Job live.
	Namespace string `json:"namespace"`
	// VersionNote is set when the three above disagree in a way worth explaining.
	VersionNote string `json:"versionNote,omitempty"`
}

// Cluster is the little that is worth recording about the cluster itself.
type Cluster struct {
	// KubernetesVersion is the API server's reported gitVersion. Empty when discovery was not
	// available (see Diagnostics).
	KubernetesVersion string `json:"kubernetesVersion,omitempty"`
	// ClusterID is the operator's own cluster identity — the default ClusterBackupLocation's
	// clusterID, or the value every location agrees on. Redacted: it is a name someone chose, and
	// it is frequently the customer's.
	ClusterID string `json:"clusterID,omitempty"`
	// SnapshotAPI reports whether the VolumeSnapshot CRDs are installed at all. Without them the
	// operator cannot take a snapshot-based backup, which is worth knowing before reading anything
	// else in this document.
	SnapshotAPI string `json:"snapshotAPI,omitempty"`
}

// Images is the section this report exists for as much as any other.
type Images struct {
	// Running is what the kubelet actually pulled, one entry per (pod, container).
	Running []RunningImage `json:"running"`
	// Declared is what the operator was CONFIGURED to use for the Jobs it creates (--mover-image,
	// --sync-image). Kept beside Running rather than instead of it: the pair is the finding.
	Declared map[string]string `json:"declared,omitempty"`
	// Note carries the caveat that matters for reading this section — chiefly that a mover image is
	// only observable while a mover Job's pod exists.
	Note string `json:"note,omitempty"`
}

// RunningImage is one container's real image identity.
type RunningImage struct {
	// Role is inferred from labels: operator, mover, sync or other.
	Role string `json:"role"`
	// Pod and Container name the source. Pod is redacted; container names are chart-fixed.
	Pod       string `json:"pod"`
	Container string `json:"container"`
	// Repository is the registry+path. Redacted by default — a private registry host names the
	// organisation as surely as a namespace does.
	Repository string `json:"repository,omitempty"`
	// Tag survives redaction: it is the PROJECT's version string, not the user's information.
	Tag string `json:"tag,omitempty"`
	// Digest is the answer to "which build is this", and it survives redaction in every mode
	// because it is a content hash and reveals nothing else. An empty digest means the container
	// has not started (no imageID yet), which is itself worth seeing.
	Digest string `json:"digest,omitempty"`
	// Ready and State come from the container status, so an image that is present but crash-looping
	// does not read as healthy.
	Ready    bool   `json:"ready"`
	State    string `json:"state,omitempty"`
	Restarts int32  `json:"restarts"`
}

// CRDInventory records which of the operator's own CRDs the cluster has, and — importantly — where
// that answer came from, because the two sources say different things.
type CRDInventory struct {
	// Source is "apiextensions" (the CRD objects: versions, storage version, served flags),
	// "discovery" (the API group only: served versions, no storage version), or "unavailable".
	Source string `json:"source"`
	// Reason explains a degraded or absent source. The common one is that the operator's ClusterRole
	// does not grant read on customresourcedefinitions.
	Reason string `json:"reason,omitempty"`
	Items  []CRD  `json:"items,omitempty"`
}

// CRD is one installed custom resource definition.
type CRD struct {
	Name           string   `json:"name"`
	Versions       []string `json:"versions,omitempty"`
	StorageVersion string   `json:"storageVersion,omitempty"`
	// GeneratedBy is the controller-gen version annotation, when present: it says which build of
	// the project produced the manifest, which is how a CRD left behind by an older chart is spotted.
	GeneratedBy string `json:"generatedBy,omitempty"`
}

// Inventory is the object census: what exists, what state it is in, and how old that state is.
type Inventory struct {
	Locations    []Location   `json:"locations,omitempty"`
	Repositories []Repository `json:"repositories,omitempty"`
	Schedules    []Schedule   `json:"schedules,omitempty"`
	Syncs        []Sync       `json:"syncs,omitempty"`
	Backups      BackupCensus `json:"backups"`
}

// Location is one BackupLocation or ClusterBackupLocation. Never its credentials: the S3 Secret is
// referenced by name (redacted) and never read, in either mode.
type Location struct {
	Scope     string `json:"scope"` // cluster | namespace
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
	Mode      string `json:"mode,omitempty"`
	Default   bool   `json:"default,omitempty"`
	Phase     string `json:"phase,omitempty"`
	Ready     string `json:"ready,omitempty"`
	ClusterID string `json:"clusterID,omitempty"`
	// Endpoint keeps its SCHEME and redacts its host: https-vs-http is a diagnosis, the hostname is
	// the customer.
	Endpoint       string `json:"endpoint,omitempty"`
	Bucket         string `json:"bucket,omitempty"`
	Prefix         string `json:"prefix,omitempty"`
	Region         string `json:"region,omitempty"`
	ForcePathStyle bool   `json:"forcePathStyle,omitempty"`
	// CABundle is reported as a boolean and never as content. A custom CA is a real cause of "my
	// location will not connect"; the bytes of it are not needed to say so.
	CustomCA bool `json:"customCA,omitempty"`
	// CredentialsSecret is a NAME, redacted, and the operator never reads the object. It is here
	// because "which Secret is this location pointing at" is a genuine question during triage.
	CredentialsSecret string `json:"credentialsSecret,omitempty"`
	AgeDays           int    `json:"ageDays"`
}

// Repository is one BackupRepository's status, which is where almost every restore-capability
// signal lives.
type Repository struct {
	Name           string `json:"name"`
	Location       string `json:"location,omitempty"`
	Scope          string `json:"scope,omitempty"`
	OwnerNamespace string `json:"ownerNamespace,omitempty"`
	Initialized    bool   `json:"initialized"`
	Mode           string `json:"mode,omitempty"`
	// KeySlots are the slot NAMES restic reports (platform, tenant). Not key material, and not a
	// password: the names are what says whether the platform can still open a tenant repository,
	// which is the guarantee adr/0004 is about.
	KeySlots      []string   `json:"keySlots,omitempty"`
	SnapshotCount int32      `json:"snapshotCount"`
	SizeBytes     int64      `json:"approximateSizeBytes"`
	StaleLocks    int32      `json:"staleLocks"`
	LastCheck     *time.Time `json:"lastCheckTime,omitempty"`
	CheckResult   string     `json:"lastCheckResult,omitempty"`
	LastPrune     *time.Time `json:"lastMaintenanceTime,omitempty"`
	LastDiscovery *time.Time `json:"lastDiscoveryTime,omitempty"`
	// DiscoverySuccess is a POINTER for the same reason the API field is: nil means never scanned,
	// which is a different thing from scanned-and-failed and must not render as either.
	DiscoverySuccess *bool `json:"lastDiscoverySuccess,omitempty"`
	ProjectedBackups int32 `json:"projectedBackups"`
	OrphanSnapshots  int32 `json:"orphanSnapshots"`
	// CheckAgeDays / PruneAgeDays are -1 when the corresponding timestamp is absent, so "never" is
	// never rendered as "0 days ago".
	CheckAgeDays int `json:"checkAgeDays"`
	PruneAgeDays int `json:"pruneAgeDays"`
}

// Schedule is one BackupSchedule or ClusterBackupSchedule.
type Schedule struct {
	Origin    string `json:"origin"` // cluster | namespace
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
	// Cron and Timezone survive redaction: an expression is a shape, not an identity, and it is the
	// single most useful field for explaining a deadline that surprised someone.
	Cron     string `json:"cron,omitempty"`
	Timezone string `json:"timezone,omitempty"`
	// CronValid is false when the expression cannot be parsed — a schedule that will never run, and
	// the one case BackupMissed has a separate branch for.
	CronValid   bool       `json:"cronValid"`
	PeriodHours float64    `json:"periodHours,omitempty"`
	Paused      bool       `json:"paused,omitempty"`
	Location    string     `json:"location,omitempty"`
	Phase       string     `json:"phase,omitempty"`
	LastSuccess *time.Time `json:"lastSuccessTime,omitempty"`
	NextRun     *time.Time `json:"nextScheduleTime,omitempty"`
	// LastSuccessAgeHours is -1 when nothing has ever succeeded.
	LastSuccessAgeHours float64 `json:"lastSuccessAgeHours"`
	AgeDays             int     `json:"ageDays"`
	// MatchedNamespaces is the resolved fan-out of a cluster schedule's selector — the thing that
	// decides how many namespaces a single object is responsible for.
	MatchedNamespaces int `json:"matchedNamespaces,omitempty"`
}

// Sync is one external-sync CR of either plane.
type Sync struct {
	Scope               string     `json:"scope"` // cluster | namespace
	Name                string     `json:"name"`
	Namespace           string     `json:"namespace,omitempty"`
	Source              string     `json:"source,omitempty"`
	Destination         string     `json:"destination,omitempty"`
	Mode                string     `json:"mode,omitempty"`
	Cron                string     `json:"cron,omitempty"`
	Paused              bool       `json:"paused,omitempty"`
	Phase               string     `json:"phase,omitempty"`
	LastSuccess         *time.Time `json:"lastSuccessTime,omitempty"`
	LastSuccessAgeHours float64    `json:"lastSuccessAgeHours"`
	SnapshotsCopied     int32      `json:"snapshotsCopied"`
	// Lag is the series that matters: a sync completing on schedule while falling further behind is
	// the quiet failure this whole family exists to catch.
	Lag     int32 `json:"lagSnapshots"`
	AgeDays int   `json:"ageDays"`
}

// BackupCensus is the aggregate, not the list. One entry per Backup object would make the report
// unreadable on any real cluster and would multiply the redaction surface for no diagnostic gain.
type BackupCensus struct {
	Total int `json:"total"`
	// ByPhase is keyed by phase name. Phases are API enum values, never user data.
	ByPhase map[string]int `json:"byPhase,omitempty"`
	// ByNamespace counts per (redacted) namespace, so an installation where one tenant produces
	// every failure is visible.
	ByNamespace map[string]int `json:"byNamespace,omitempty"`
	// Newest and Oldest bound the retained history.
	NewestSuccess *time.Time `json:"newestSuccess,omitempty"`
	OldestSuccess *time.Time `json:"oldestSuccess,omitempty"`
}

// Leaks is the residue census: the objects a crashed teardown leaves behind, which M4 spent a whole
// milestone learning to see.
//
// Every count is split by AGE against the orphan reaper's own grace period, because the raw count
// is meaningless — a backup that is running right now legitimately owns a temp PVC, a mover Job and
// a VolumeSnapshot. What is a leak is the same object still there long after the reaper's cycle.
type Leaks struct {
	// GraceMinutes is the reaper's minimum age, stated so a reader knows what "residual" means here.
	GraceMinutes int         `json:"graceMinutes"`
	Kinds        []LeakKind  `json:"kinds,omitempty"`
	Note         string      `json:"note,omitempty"`
	Samples      []LeakItem  `json:"samples,omitempty"`
	Unreadable   []string    `json:"unreadable,omitempty"`
	Totals       LeakSummary `json:"totals"`
}

// LeakKind is one object class's census.
type LeakKind struct {
	Kind string `json:"kind"`
	// Total is every managed object of this kind; Residual is those older than the grace period,
	// which is the number that means something.
	Total          int `json:"total"`
	Residual       int `json:"residual"`
	OldestAgeHours int `json:"oldestAgeHours"`
}

// LeakItem names one residual object, redacted, so an operator can go and look at it.
type LeakItem struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
	AgeHours  int    `json:"ageHours"`
}

// LeakSummary is the one-line answer.
type LeakSummary struct {
	Residual int `json:"residual"`
}

// RuleResult is one alert rule's state verdict — the half of this report that gives an opinion.
type RuleResult struct {
	Name     string `json:"name"`
	Severity string `json:"severity"`
	// Status is one of StatusOK, StatusBreached, StatusNotEvaluated, StatusError.
	Status string `json:"status"`
	// Threshold is the rule's DECLARED bound, carried into the report so a reader can see the
	// number the verdict was reached against without reading the source.
	Threshold ThresholdView `json:"threshold"`
	Summary   string        `json:"summary,omitempty"`
	// Remedy is the rule's description: what to do about it. A verdict with no stated remedy gets
	// ignored rather than acted on.
	Remedy string `json:"remedy,omitempty"`
	// Fidelity is non-empty when the predicate does not reproduce the PromQL exactly. It travels
	// with the verdict, in the JSON and on the page, because a bare OK over a measurement with a
	// known blind spot is the thing this report is built not to print.
	Fidelity string `json:"fidelity,omitempty"`
	// Reason explains a not-evaluated or errored status.
	Reason   string       `json:"reason,omitempty"`
	Breaches []BreachView `json:"breaches,omitempty"`
}

// Rule verdict statuses. Four, and the two that are not a verdict are deliberately distinct from
// each other: "no predicate exists" and "the predicate could not run" are different problems.
const (
	StatusOK           = "ok"
	StatusBreached     = "breached"
	StatusNotEvaluated = "notEvaluated"
	StatusError        = "error"
)

// ThresholdView is alerts.Threshold rendered for a reader, with the units spelled out.
type ThresholdView struct {
	Kind        string  `json:"kind"`
	AgeHours    float64 `json:"ageHours,omitempty"`
	Count       float64 `json:"count,omitempty"`
	Factor      float64 `json:"factor,omitempty"`
	GraceHours  float64 `json:"graceHours,omitempty"`
	Description string  `json:"description,omitempty"`
}

// BreachView is one alerts.Breach with its labels redacted.
type BreachView struct {
	Labels map[string]string `json:"labels,omitempty"`
	Value  float64           `json:"value"`
	Detail string            `json:"detail,omitempty"`
}

// Verdict is the headline.
type Verdict struct {
	// Status is "healthy", "healthyWithFindings", "degraded" or "unhealthy". Degraded and unhealthy
	// both mean a rule was breached, and unhealthy requires a CRITICAL one — the only rule severity
	// that says the restore path itself is compromised.
	//
	// "healthyWithFindings" is not a fourth severity: it is the same clean rule tally as "healthy",
	// over an installation that is also carrying something the rules do not measure — today, a
	// non-zero leakIndicators residual. It exists because "healthy" was being read as a verdict on
	// the whole installation rather than on the rules, which is what a reader who does not scroll
	// will always read it as. Anything comparing this field for equality with "healthy" and meaning
	// "is everything fine" wants to compare against that constant and then read the summary.
	Status string `json:"status"`
	// Summary is the sentence a human reads first, and the one place every fact in the headline is
	// stated together — including the ones no rule covers.
	Summary string `json:"summary"`
	// The counts behind it, including NotEvaluated — which is part of the headline on purpose. An
	// installation where seven of ten rules could not be evaluated has not been given a clean bill
	// of health, and the number saying so belongs next to the word "healthy".
	//
	// These are the RULE tally and nothing else. A leak residual moves none of them: no alert rule
	// fires on the residue census, and a report that counted one as a breach would claim an alert
	// that the alerting side would never send.
	Breached     int `json:"breached"`
	OK           int `json:"ok"`
	NotEvaluated int `json:"notEvaluated"`
	Errored      int `json:"errored"`
	Critical     int `json:"critical"`
}

// Diagnostic is one thing the collector could not do, and what it cost.
type Diagnostic struct {
	// Area names the section affected, so a reader can tie a gap to the empty part of the report.
	Area string `json:"area"`
	// Message is the error, already free of anything sensitive (it names kinds and verbs, not
	// objects).
	Message string `json:"message"`
	// Impact says what is missing from the report as a result — the difference between "empty" and
	// "not looked at" that this whole section exists to preserve.
	Impact string `json:"impact,omitempty"`
}
