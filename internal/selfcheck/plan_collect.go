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
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
	"github.com/CrystalBackup/CrystalBackup/internal/nsselector"
	"github.com/CrystalBackup/CrystalBackup/internal/restic"
)

// plan narrates the custom resources: what each location and schedule is going to do, and what is
// wrong with it. See plan.go for why this is not simply a second rendering of the inventory.
//
// It reads five collections and returns nil only when it could see nothing at all, so that "no plan
// section" and "an installation with nothing configured" stay distinguishable — the latter gets a
// Plan carrying a Note that says which nothing it is.
func (c *collector) plan(ctx context.Context) *Plan {
	p := &Plan{}
	clusterLocs, nsLocs := c.planLocations(ctx, p)
	c.planSchedules(ctx, p, clusterLocs, nsLocs)

	switch {
	case len(p.Locations) == 0:
		p.Note = planNoLocations
	case len(p.Schedules) == 0:
		p.Note = planNoSchedules
	}
	return p
}

// planLocations describes every location and returns the two name sets the schedule pass needs to
// answer "does the location this schedule names actually exist?".
//
// That cross-reference is the reason the two passes share a function rather than being independent:
// a schedule pointing at a location nobody created is one of the commonest ways an installation is
// silently dead, and neither object on its own carries the finding. The schedule looks fine. The
// location list looks fine. Only the join is wrong.
func (c *collector) planLocations(ctx context.Context, p *Plan) (cluster map[string]bool, namespaced map[string]bool) {
	cluster = map[string]bool{}
	namespaced = map[string]bool{}

	var clusterLocs cbv1.ClusterBackupLocationList
	if err := c.Reader.List(ctx, &clusterLocs); err != nil {
		c.diag("plan", "cluster-plane locations are not described, and schedules naming one cannot "+
			"be checked against it", err)
	} else {
		for i := range clusterLocs.Items {
			l := &clusterLocs.Items[i]
			cluster[l.Name] = true
			p.Locations = append(p.Locations, c.planClusterLocation(l))
		}
	}

	var locs cbv1.BackupLocationList
	if err := c.Reader.List(ctx, &locs); err != nil {
		c.diag("plan", "namespace-plane locations are not described", err)
	} else {
		for i := range locs.Items {
			l := &locs.Items[i]
			namespaced[l.Namespace+"/"+l.Name] = true
			p.Locations = append(p.Locations, c.planNamespaceLocation(l))
		}
	}

	sortByName(p.Locations, func(l PlannedLocation) string {
		return l.Scope + "/" + l.Namespace + "/" + l.Name
	})
	return cluster, namespaced
}

func (c *collector) planClusterLocation(l *cbv1.ClusterBackupLocation) PlannedLocation {
	out := PlannedLocation{
		Scope:     apiconst.OriginCluster,
		Name:      c.red.Location(l.Name),
		Mode:      string(l.Spec.Mode),
		Default:   l.Spec.Default,
		Ready:     conditionOf(l.Status.Conditions),
		Sentences: []string{c.destinationSentence(l.Spec.S3, l.Spec.ClusterID)},
	}
	out.Sentences = append(out.Sentences, retentionSentence(l.Spec.Retention, l.Spec.Mode))
	out.Sentences = append(out.Sentences, discoverySentence(l.Spec.Discovery))
	out.Maintenance = c.planMaintenance(l.Spec.Maintenance)
	out.Problems = c.locationProblems(l.Spec.Mode, l.Spec.Maintenance, l.Spec.Retention,
		l.Status.Conditions, l.Status.Phase)
	return out
}

func (c *collector) planNamespaceLocation(l *cbv1.BackupLocation) PlannedLocation {
	out := PlannedLocation{
		Scope:     apiconst.OriginNamespace,
		Name:      c.red.Location(l.Name),
		Namespace: c.red.Namespace(l.Namespace),
		Mode:      string(l.Spec.Mode),
		Ready:     conditionOf(l.Status.Conditions),
		Sentences: []string{c.destinationSentence(l.Spec.S3, l.Status.ClusterID)},
	}
	out.Sentences = append(out.Sentences, retentionSentence(l.Spec.Retention, l.Spec.Mode))
	out.Sentences = append(out.Sentences, discoverySentence(l.Spec.Discovery))
	// No maintenance list, and no problem raised for its absence: BackupLocationSpec has no
	// maintenance field at all, so a namespace repository's check and prune are not this object's to
	// declare. Reporting "no maintenance configured" here would read as a misconfiguration and be a
	// statement about the API rather than about the installation.
	out.Problems = c.locationProblems(l.Spec.Mode, nil, l.Spec.Retention,
		l.Status.Conditions, l.Status.Phase)
	return out
}

// destinationSentence says where the data goes, in one line, with the identifiers redacted exactly as
// the inventory redacts them — same tokens, so the two sections can be read together.
func (c *collector) destinationSentence(s3 cbv1.S3Spec, clusterID string) string {
	// The repository path is composed the way the location controller composes it
	// (<endpoint>/<bucket>/<prefix>/<clusterID>), because the whole value of this line is that an
	// administrator can compare it against what they see in their bucket browser.
	parts := []string{c.red.Endpoint(s3.Endpoint), c.red.Bucket(s3.Bucket)}
	if s3.Prefix != "" {
		parts = append(parts, c.red.Prefix(s3.Prefix))
	}
	if clusterID != "" {
		parts = append(parts, c.red.ClusterID(clusterID))
	}
	sentence := "Writes to " + strings.Join(parts, "/") + "."
	if s3.Region != "" {
		sentence += " Region " + s3.Region + "."
	}
	if s3.CABundle != "" {
		sentence += " Verifies the endpoint against a custom CA bundle."
	}
	return sentence
}

// retentionSentence turns the keep policy into a span of history, and says the dangerous thing out
// loud when there is no policy at all.
//
// The no-policy case is not a stylistic nicety. internal/restic.ForgetCommand returns ok=false for a
// keep-less policy and internal/controller.maybeEnqueueRetentionForget then runs no forget at all —
// deliberately, because a keep-less forget would delete every snapshot. The consequence is that a
// location with an empty spec.retention never forgets anything and its bucket grows without bound,
// forever, with no condition, no event and no alert to say so. An administrator reading a report of
// their new installation should be told that in words the first time they look.
func retentionSentence(r cbv1.RetentionSpec, mode cbv1.LocationMode) string {
	// Asked of internal/restic rather than by comparing the fields to zero here: ForgetCommand's
	// ok is the operator's OWN definition of "is there a policy", and it is the thing that actually
	// decides whether a forget runs.
	if _, ok := restic.ForgetCommand(r); !ok {
		return "NO retention policy: no `restic forget` ever runs, so every snapshot is kept for " +
			"ever and this repository grows without bound."
	}
	if mode == cbv1.LocationModeImmutable {
		return "Retention is declared but IGNORED: this location is Immutable (object-lock), and " +
			"object-lock forbids forget and prune until the lock expires. " + retentionKeepWords(r)
	}
	return "Keeps " + retentionKeepWords(r) + " per PVC, thinned by a `restic forget` after each " +
		"successful backup. " + retentionSpanWords(r)
}

// retentionKeepWords lists the keep buckets in restic's own order of granularity.
func retentionKeepWords(r cbv1.RetentionSpec) string {
	var parts []string
	for _, b := range retentionBuckets(r) {
		if b.count > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", b.count, b.word))
		}
	}
	if len(parts) == 0 {
		return "nothing"
	}
	return joinAnd(parts)
}

// retentionSpanWords states how far back the history reaches, which is the number an administrator
// actually wants and the one no field carries.
//
// It is the WIDEST bucket, and it is qualified as approximate on purpose: restic's month and year
// buckets are calendar ones, so "6 monthly" reaches back about six months rather than exactly 180
// days, and a report that printed an exact figure would be precise about something it cannot be.
func retentionSpanWords(r cbv1.RetentionSpec) string {
	var widest time.Duration
	for _, b := range retentionBuckets(r) {
		if b.count > 0 {
			if span := time.Duration(b.count) * b.span; span > widest {
				widest = span
			}
		}
	}
	if widest == 0 {
		return ""
	}
	return "The oldest restore point a PVC keeps is roughly " + approxSpan(widest) + " old."
}

// retentionBucket is one restic keep bucket: its count, its English name and the period one entry of
// it covers.
type retentionBucket struct {
	count int32
	word  string
	span  time.Duration
}

func retentionBuckets(r cbv1.RetentionSpec) []retentionBucket {
	const day = 24 * time.Hour
	return []retentionBucket{
		// keepLast is a plain count of the most recent snapshots, so its span depends entirely on how
		// often backups run. A zero span keeps it out of retentionSpanWords rather than letting it
		// claim a history it cannot promise.
		{r.KeepLast, "most recent", 0},
		{r.KeepHourly, "hourly", time.Hour},
		{r.KeepDaily, "daily", day},
		{r.KeepWeekly, "weekly", 7 * day},
		{r.KeepMonthly, "monthly", 30 * day},
		{r.KeepYearly, "yearly", 365 * day},
	}
}

// approxSpan renders a duration in the largest unit that still reads as a plain answer.
func approxSpan(d time.Duration) string {
	const day = 24 * time.Hour
	switch {
	case d >= 365*day:
		return plural(int(d/(365*day)), "year")
	case d >= 30*day:
		return plural(int(d/(30*day)), "month")
	case d >= 7*day:
		return plural(int(d/(7*day)), "week")
	case d >= day:
		return plural(int(d/day), "day")
	default:
		return plural(int(d/time.Hour), "hour")
	}
}

// discoverySentence describes whether the repository is inventoried, which is what decides whether
// restore points taken by another cluster (or before this operator was installed) are visible as
// Backup objects at all.
func discoverySentence(d cbv1.DiscoverySpec) string {
	// A nil Enabled means unset, and the CRD default is true — the field is a pointer precisely so
	// "I did not say" and "I said no" stay distinguishable, and a report must honour that rather than
	// reading nil as false.
	if d.Enabled != nil && !*d.Enabled {
		return "Discovery is OFF: restore points already in this repository will NOT appear as " +
			"Backup objects, so `kubectl get backups` will not list what can be restored."
	}
	interval := d.Interval.Duration
	if interval == 0 {
		return "Discovery is on, at the default interval: the repository is inventoried and its " +
			"restore points are projected as Backup objects."
	}
	return "Discovery is on every " + interval.String() + ": the repository is inventoried and its " +
		"restore points are projected as Backup objects."
}

// planMaintenance describes the check and prune windows, and computes the next occurrence of each.
//
// The prune entry is flagged Exclusive because it is: one shared repository means one cluster-wide
// prune window during which no namespace can start a backup (adr/0009). "When is the next heavy
// maintenance?" is a question that has been answered by hand off a cron expression in this project
// before, and this is the field that stops it being a manual calculation.
func (c *collector) planMaintenance(m *cbv1.MaintenanceSpec) []PlannedMaintenance {
	if m == nil {
		return nil
	}
	var out []PlannedMaintenance
	if m.CheckSchedule != "" {
		e := PlannedMaintenance{
			Operation: "check",
			Cron:      m.CheckSchedule,
			InWords:   cronInWords(m.CheckSchedule, m.Timezone),
			Next:      cronNext(m.CheckSchedule, m.Timezone, c.Now),
		}
		if m.CheckReadDataSubset != "" {
			e.Detail = "reads " + m.CheckReadDataSubset + " of the pack data"
		} else {
			// Said explicitly, because the difference is the difference between detecting bit rot and
			// not: a structural check verifies that every object is present and the right length, and
			// a rotted object is present and the right length.
			e.Detail = "structural only: verifies that objects exist and are the right length, and " +
				"will NOT detect a silently corrupted one (set checkReadDataSubset to read data)"
		}
		out = append(out, e)
	}
	if m.PruneSchedule != "" {
		e := PlannedMaintenance{
			Operation: "prune",
			Cron:      m.PruneSchedule,
			InWords:   cronInWords(m.PruneSchedule, m.Timezone),
			Next:      cronNext(m.PruneSchedule, m.Timezone, c.Now),
			Exclusive: true,
		}
		if m.PruneMaxRepackSize != "" {
			e.Detail = "repacks at most " + m.PruneMaxRepackSize + " per run, which bounds how long " +
				"the exclusive window lasts"
		} else {
			e.Detail = "no repack cap: the window lasts as long as the run needs, and no backup can " +
				"start while it does"
		}
		out = append(out, e)
	}
	return out
}

// locationProblems is the findings list for one location.
func (c *collector) locationProblems(
	mode cbv1.LocationMode, m *cbv1.MaintenanceSpec, r cbv1.RetentionSpec,
	conds []metav1.Condition, phase string,
) []string {
	var problems []string
	if ready := readyCondition(conds); ready != nil && ready.Status != metav1.ConditionTrue {
		problems = append(problems, fmt.Sprintf(
			"NOT Ready (%s): %s — nothing will be written here until this clears",
			ready.Reason, c.red.Detail(plainText(ready.Message))))
	} else if ready == nil {
		problems = append(problems, "no Ready condition yet: the operator has not reconciled this "+
			"location, so it has never been proven reachable"+phaseHint(phase))
	}
	if mode == cbv1.LocationModeStandard && m != nil && m.PruneSchedule == "" {
		problems = append(problems, "no pruneSchedule: deleted snapshots are forgotten but their "+
			"data is never reclaimed, so the bucket keeps growing even with a retention policy")
	}
	if mode == cbv1.LocationModeStandard && m != nil && m.CheckSchedule == "" {
		problems = append(problems, "no checkSchedule: the repository is never verified, so "+
			"corruption is discovered during a restore rather than before one")
	}
	if mode == cbv1.LocationModeImmutable && retentionAsked(r) {
		problems = append(problems, "spec.retention is set on an Immutable location and is ignored: "+
			"object-lock forbids forget and prune until the lock expires")
	}
	return problems
}

// retentionAsked reports whether a keep policy was requested at all, through the same
// internal/restic gate the controller uses to decide whether to run a forget.
func retentionAsked(r cbv1.RetentionSpec) bool {
	_, ok := restic.ForgetCommand(r)
	return ok
}

// phaseHint appends the status phase when there is one, so "never reconciled" and "reconciling"
// do not read the same.
func phaseHint(phase string) string {
	if phase == "" {
		return ""
	}
	return " (phase: " + phase + ")"
}

// planSchedules describes every schedule of both planes, resolving what each one targets and
// checking it against the locations that exist.
func (c *collector) planSchedules(ctx context.Context, p *Plan, cluster, namespaced map[string]bool) {
	var namespaces corev1.NamespaceList
	nsListed := c.Reader.List(ctx, &namespaces) == nil

	var nsScheds cbv1.BackupScheduleList
	if err := c.Reader.List(ctx, &nsScheds); err != nil {
		c.diag("plan", "namespace-plane schedules are not described", err)
	} else {
		for i := range nsScheds.Items {
			p.Schedules = append(p.Schedules, c.planNamespaceSchedule(&nsScheds.Items[i], namespaced))
		}
	}

	var clScheds cbv1.ClusterBackupScheduleList
	if err := c.Reader.List(ctx, &clScheds); err != nil {
		c.diag("plan", "cluster-plane schedules are not described", err)
	} else {
		for i := range clScheds.Items {
			p.Schedules = append(p.Schedules,
				c.planClusterSchedule(&clScheds.Items[i], cluster, namespaces.Items, nsListed))
		}
	}

	sortByName(p.Schedules, func(s PlannedSchedule) string {
		return s.Origin + "/" + s.Namespace + "/" + s.Name
	})
}

func (c *collector) planNamespaceSchedule(s *cbv1.BackupSchedule, locations map[string]bool) PlannedSchedule {
	out := PlannedSchedule{
		Origin:    apiconst.OriginNamespace,
		Name:      c.red.Schedule(s.Name),
		Namespace: c.red.Namespace(s.Namespace),
		Cron:      s.Spec.Schedule,
		Timezone:  s.Spec.Timezone,
		InWords:   cronInWords(s.Spec.Schedule, s.Spec.Timezone),
		Next:      cronNext(s.Spec.Schedule, s.Spec.Timezone, c.Now),
		Suspended: s.Spec.Paused,
		Ready:     conditionOf(s.Status.Conditions),
	}
	out.Sentences = append(out.Sentences, fmt.Sprintf("Backs up %s in namespace %s to location %s, %s.",
		pvcSelectorWords(s.Spec.PVCSelector), c.red.Namespace(s.Namespace),
		c.red.Location(s.Spec.LocationRef.Name), c.cronPhrase(s.Spec.Schedule, s.Spec.Timezone)))
	out.Sentences = append(out.Sentences, manifestSentence(s.Spec.IncludeManifests, s.Spec.ManifestOptions))
	out.Sentences = append(out.Sentences, hooksSentence(s.Spec.Hooks))
	out.Problems = c.scheduleProblems(s.Spec.Schedule, s.Spec.Timezone, s.Spec.Paused,
		s.Status.Conditions, s.Status.LastSuccessTime, s.CreationTimestamp.Time)
	if !locations[s.Namespace+"/"+s.Spec.LocationRef.Name] {
		// The join nobody sees. A BackupSchedule may only name a BackupLocation in its OWN namespace
		// (never a ClusterBackupLocation), so a name that resolves nowhere is a schedule that will
		// produce Backups which can never bind a repository.
		out.Problems = append(out.Problems, fmt.Sprintf(
			"spec.locationRef names BackupLocation %q, which does not exist in namespace %s — every "+
				"run will fail to resolve a destination",
			c.red.Location(s.Spec.LocationRef.Name), c.red.Namespace(s.Namespace)))
	}
	return out
}

func (c *collector) planClusterSchedule(
	s *cbv1.ClusterBackupSchedule, locations map[string]bool,
	namespaces []corev1.Namespace, nsListed bool,
) PlannedSchedule {
	spec := s.Spec.Template.Spec
	out := PlannedSchedule{
		Origin:    apiconst.OriginCluster,
		Name:      c.red.Schedule(s.Name),
		Cron:      s.Spec.Schedule,
		Timezone:  s.Spec.Timezone,
		InWords:   cronInWords(s.Spec.Schedule, s.Spec.Timezone),
		Next:      cronNext(s.Spec.Schedule, s.Spec.Timezone, c.Now),
		Suspended: s.Spec.Paused,
		Ready:     conditionOf(s.Status.Conditions),
	}

	target := "namespaces its selector matches"
	if nsListed {
		// nsselector.Match, the operator's own resolver, so this sentence and the fan-out cannot
		// disagree. Its error is the rule-8 shape violation, and a schedule in that state fans out
		// into nothing at all — which is a problem, not a count of zero.
		names, err := nsselector.Match(namespaces, spec.Namespaces)
		switch {
		case err != nil:
			out.Problems = append(out.Problems, "spec.template.spec.namespaces is not a valid "+
				"selector and this schedule will select NOTHING: "+plainText(err.Error()))
		case len(names) == 0:
			out.Problems = append(out.Problems, "the namespace selector matches NO namespace in this "+
				"cluster, so every run will back up nothing at all")
			target = "0 namespaces"
		default:
			target = plural(len(names), "namespace") + " (" + c.namespaceSample(names) + ")"
		}
	}

	out.Sentences = append(out.Sentences, fmt.Sprintf("Backs up %s in %s to location %s, %s.",
		pvcSelectorWords(spec.PVCSelector), target,
		c.red.Location(spec.LocationRef.Name), c.cronPhrase(s.Spec.Schedule, s.Spec.Timezone)))
	out.Sentences = append(out.Sentences, clusterResourceSentence(spec.ClusterResources))
	out.Sentences = append(out.Sentences, manifestSentence(spec.IncludeManifests, spec.ManifestOptions))
	out.Sentences = append(out.Sentences, hooksSentence(spec.Hooks))
	out.Problems = append(out.Problems, c.scheduleProblems(s.Spec.Schedule, s.Spec.Timezone,
		s.Spec.Paused, s.Status.Conditions, s.Status.LastSuccessTime, s.CreationTimestamp.Time)...)
	if !locations[spec.LocationRef.Name] {
		out.Problems = append(out.Problems, fmt.Sprintf(
			"spec.template.spec.locationRef names ClusterBackupLocation %q, which does not exist — "+
				"every run will fail to resolve a destination",
			c.red.Location(spec.LocationRef.Name)))
	}
	if s.Spec.ConcurrencyPolicy == cbv1.ConcurrencyPolicy("Forbid") || s.Spec.ConcurrencyPolicy == "" {
		out.Sentences = append(out.Sentences, "A run that is still going when the next activation "+
			"arrives makes that activation be SKIPPED (concurrencyPolicy: Forbid).")
	}
	return out
}

// maxNamespaceSample bounds the namespace list in a sentence. Three names plus a count answers "is it
// selecting what I think it is" without turning one line into a page.
const maxNamespaceSample = 3

func (c *collector) namespaceSample(names []string) string {
	shown := names
	suffix := ""
	if len(names) > maxNamespaceSample {
		shown = names[:maxNamespaceSample]
		suffix = fmt.Sprintf(", +%d more", len(names)-maxNamespaceSample)
	}
	out := make([]string, 0, len(shown))
	for _, n := range shown {
		out = append(out, c.red.Namespace(n))
	}
	return strings.Join(out, ", ") + suffix
}

// cronPhrase is the schedule clause of a sentence: the words when they could be derived, and the raw
// expression when they could not. Never nothing — a sentence that trailed off would be worse than one
// quoting a cron expression.
func (c *collector) cronPhrase(expr, tz string) string {
	if w := cronInWords(expr, tz); w != "" {
		return w
	}
	return "on cron `" + expr + "`" + tzSuffix(tz)
}

// pvcSelectorWords describes which PVCs a run takes. The empty selector — by far the commonest — is
// stated as "every PVC" rather than omitted, because "backs up namespace X" leaves a reader
// wondering whether something was filtered.
func pvcSelectorWords(sel cbv1.PVCSelector) string {
	var clauses []string
	if len(sel.MatchLabels) > 0 {
		pairs := make([]string, 0, len(sel.MatchLabels))
		for k, v := range sel.MatchLabels {
			pairs = append(pairs, k+"="+v)
		}
		// Sorted so the same selector renders identically in two reports.
		slices.Sort(pairs)
		clauses = append(clauses, "labelled "+joinAnd(pairs))
	}
	if len(sel.Include) > 0 {
		clauses = append(clauses, "named "+joinAnd(quoteAll(sel.Include)))
	}
	if len(sel.Exclude) > 0 {
		clauses = append(clauses, "except "+joinAnd(quoteAll(sel.Exclude)))
	}
	if len(clauses) == 0 {
		return "every PVC"
	}
	return "PVCs " + strings.Join(clauses, ", ")
}

func quoteAll(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		out = append(out, "`"+s+"`")
	}
	return out
}

// manifestSentence says whether the namespace's Kubernetes objects are captured alongside the volume
// data, which is the difference between a restore that brings the workload back and one that hands
// somebody a volume.
func manifestSentence(include *bool, opts cbv1.ManifestOptions) string {
	if include != nil && !*include {
		return "Captures volume DATA ONLY: no namespace manifests, so a restore brings back the " +
			"volumes and not the workloads that used them."
	}
	s := "Also captures the namespace's manifests, so a restore can recreate the workloads."
	if opts.ExcludeSecretData {
		s += " Secret VALUES are excluded from that capture."
	}
	return s
}

// clusterResourceSentence says whether cluster-scoped objects are captured, which is what separates a
// namespace-by-namespace backup from something a whole cluster can be rebuilt from.
func clusterResourceSentence(c cbv1.ClusterResourceCaptureSpec) string {
	if c.Enabled != nil && !*c.Enabled {
		return "Cluster-scoped resources are NOT captured, so this is not a full-cluster DR backup."
	}
	return "Also captures cluster-scoped resources for full-cluster DR."
}

// hooksSentence mentions exec hooks only when there are some. Their failure policy is the part worth
// stating: onError Fail turns a hook that cannot run into a failed backup.
func hooksSentence(h cbv1.HooksSpec) string {
	if len(h.Pre) == 0 && len(h.Post) == 0 {
		return ""
	}
	s := fmt.Sprintf("Runs %d pre-snapshot and %d post-snapshot exec hook(s) in the workload pods.",
		len(h.Pre), len(h.Post))
	for _, hook := range append(append([]cbv1.Hook{}, h.Pre...), h.Post...) {
		if hook.OnError == cbv1.HookErrorPolicyFail {
			return s + " At least one is onError: Fail, so a hook that cannot run FAILS the backup."
		}
	}
	return s
}

// scheduleProblems is the findings list shared by both planes.
func (c *collector) scheduleProblems(
	expr, tz string, paused bool, conds []metav1.Condition,
	lastSuccess *metav1.Time, created time.Time,
) []string {
	var problems []string
	if paused {
		problems = append(problems, "SUSPENDED (spec.paused): no run will start until this is cleared")
	}
	if !cronValid(expr, tz) {
		problems = append(problems, fmt.Sprintf(
			"spec.schedule %q is not a valid cron expression in timezone %q, so this schedule will "+
				"NEVER fire", expr, tzOrUTC(tz)))
	}
	if ready := readyCondition(conds); ready != nil && ready.Status != metav1.ConditionTrue && !paused {
		problems = append(problems, fmt.Sprintf("NOT Ready (%s): %s",
			ready.Reason, c.red.Detail(plainText(ready.Message))))
	}
	// "Never succeeded" is only a finding once the schedule has been alive long enough to have had a
	// turn. Reporting it on a schedule created four minutes ago would train a reader to ignore the
	// line, which is how a report stops being read.
	if lastSuccess == nil && !paused && c.Now.Sub(created) > neverRanGrace {
		problems = append(problems, fmt.Sprintf(
			"has NEVER completed a successful run in the %s since it was created",
			approxSpan(c.Now.Sub(created))))
	}
	return problems
}

// neverRanGrace is how long a schedule may exist without a success before that is worth reporting.
// Twenty-six hours covers a daily schedule that was created just after its own activation time —
// the commonest way a brand-new schedule legitimately has no success yet.
const neverRanGrace = 26 * time.Hour

func tzOrUTC(tz string) string {
	if tz == "" {
		return "UTC"
	}
	return tz
}

// readyCondition returns the Ready condition, or nil when the object carries none.
//
// It returns a POINTER, and the nil case is the reason this is not conditionOf: an object with no
// Ready condition has never been reconciled, which is a different — and on a fresh installation more
// likely — finding than one whose Ready is False. conditionOf renders both as the empty string,
// which is right for a table cell and wrong for a problem list.
func readyCondition(conds []metav1.Condition) *metav1.Condition {
	for i := range conds {
		if conds[i].Type == conditionReady {
			return &conds[i]
		}
	}
	return nil
}
