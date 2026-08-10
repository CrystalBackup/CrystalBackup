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
	"errors"
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
	"github.com/CrystalBackup/CrystalBackup/internal/exposer"
	"github.com/CrystalBackup/CrystalBackup/internal/nsselector"
)

// coverage walks every PVC in the cluster and answers, per volume, what the operator would do with
// it — by asking the operator's own resolver, once per PVC, over a reader that makes that affordable
// (coverage_reader.go).
//
// It returns nil only when the PVC list itself could not be read, which is recorded as a Diagnostic:
// an absent section and an empty one must not look the same, and this is the section where that
// distinction costs the most. Everything short of that produces a census, however partial, because a
// census that names four skipped volumes and admits it could not read the fifth is worth far more
// than nothing.
func (c *collector) coverage(ctx context.Context) *Coverage {
	// EVERY read this census makes goes through the counted wrapper, including the ones the resolver
	// never sees. Coverage.APIReads is a claim about what this section cost, and a claim assembled
	// from two counters — one inside the wrapper, one kept by hand out here — is a claim that goes
	// wrong the first time somebody adds a List.
	reader := newCoverageReader(c.Reader)

	var pvcs corev1.PersistentVolumeClaimList
	if err := reader.List(ctx, &pvcs); err != nil {
		c.diag("coverage",
			"the per-PVC census is absent: it is not known which volumes will and will not be backed up",
			err)
		return nil
	}

	cov := &Coverage{Note: coverageNote}
	sel := c.coverageSelectors(ctx, reader)

	// Priming before the loop is what turns "no snapshot CRDs installed" from three thousand
	// identical per-volume errors into one sentence about the cluster. See primeSnapshotClasses.
	apiAbsent := false
	if err := reader.primeSnapshotClasses(ctx); err != nil {
		apiAbsent = true
		c.diag("coverage",
			"no PVC's treatment could be determined; every volume is reported as unknown rather "+
				"than as backed up",
			err)
		cov.Note = snapshotAPIAbsentNote + " " + bestEffortNote
	}

	registry := exposer.NewRegistry(readOnlyClient{Reader: reader, scheme: buildScheme()}, c.OperatorNamespace)

	// The observation that qualifies this section's predictions, aggregated over the very PVCs this
	// section is about to classify. Costs no API request: stuckSnapshots already listed the snapshots,
	// and the PVC list is the one above.
	scEvidence := c.snapshotEvidenceByStorageClass(pvcs.Items)

	counts := map[string]int{}
	namespaces := map[string]bool{}
	for i := range pvcs.Items {
		pvc := &pvcs.Items[i]
		// The operator's own temporary exposure PVCs are NOT user data and must not appear in a census
		// of what will be backed up. They live in the operator namespace, they are labelled
		// app.kubernetes.io/managed-by=crystal-backup, they exist for the duration of one mover Job,
		// and they are already counted — as residue — in leakIndicators. Listing them here would
		// invent unprotected volumes out of the operator's own working set, and an administrator has
		// no way to tell that is what happened.
		if pvc.Labels[apiconst.LabelManagedBy] == apiconst.ManagedByValue {
			continue
		}
		c.red.Learn(kindNamespace, pvc.Namespace)
		c.red.Learn(kindPVC, pvc.Name)
		namespaces[pvc.Namespace] = true

		row := c.coverPVC(ctx, registry, pvc, sel, apiAbsent)
		// AFTER the resolution and never inside it. coverPVC's whole discipline is that it re-implements
		// nothing about routing; this is a separate statement about the same volume, applied on top, and
		// keeping it outside is what stops an observation from ever being mistaken for a resolution rule.
		qualifyWithSnapshotEvidence(&row, scEvidence[storageClassKey(pvc)])
		counts[row.Class]++
		cov.PVCs++
		cov.CapacityBytes += row.CapacityBytes
		if row.Verdict == CoverageVerdictBackedUp {
			cov.BackedUpBytes += row.CapacityBytes
		}
		if row.SnapshotEvidence != "" {
			cov.StalledStorage++
		}
		switch {
		case len(row.Schedules) > 0:
		case len(row.InertSchedules) > 0:
			cov.InertOnly++
		default:
			cov.Unselected++
		}
		cov.Items = append(cov.Items, row)
	}
	cov.Namespaces = len(namespaces)
	cov.APIReads = reader.Reads()

	for class, n := range counts {
		phase, reason := coveragePhaseReason(class)
		cov.Classes = append(cov.Classes, CoverageTally{
			Class:   class,
			Count:   n,
			Verdict: coverageVerdict(class),
			Summary: coverageSummary(class),
			Phase:   phase,
			Reason:  reason,
		})
	}
	slices.SortFunc(cov.Classes, func(a, b CoverageTally) int {
		if d := coverageOrder(a.Class) - coverageOrder(b.Class); d != 0 {
			return d
		}
		return strings.Compare(a.Class, b.Class)
	})

	sortCoverageItems(cov.Items)
	if len(cov.Items) > maxCoverageItems {
		cov.ItemsOmitted = len(cov.Items) - maxCoverageItems
		cov.Items = cov.Items[:maxCoverageItems]
	}
	return cov
}

// sortCoverageItems puts the rows in attention order, and within a rank in a stable identity order so
// two reports of the same cluster are diffable.
//
// The FIRST key is "does anybody have to do something about this row", and it is first rather than the
// treatment class for a reason that took a second pass to see. Sorting by class first would put a
// perfectly healthy PVC that NO SCHEDULE SELECTS behind every other csi-generic row — several hundred
// of them on a real cluster — and straight off the end of maxCoverageItems. That is the one finding
// this whole section was added to surface, and the truncation would have silently eaten it while every
// count above still said it was there. Ordering by attention first is what makes the cap safe.
//
// After that: the treatment class (worst first), then the selection rank within it, then identity.
func sortCoverageItems(items []CoveredPVC) {
	slices.SortFunc(items, func(a, b CoveredPVC) int {
		if d := coverageNeedsAttention(a) - coverageNeedsAttention(b); d != 0 {
			return d
		}
		if d := coverageOrder(a.Class) - coverageOrder(b.Class); d != 0 {
			return d
		}
		if d := coverageSelectionRank(a) - coverageSelectionRank(b); d != 0 {
			return d
		}
		if d := strings.Compare(a.Namespace, b.Namespace); d != 0 {
			return d
		}
		return strings.Compare(a.Name, b.Name)
	})
}

// coverageNeedsAttention is 0 for a row somebody must act on and 1 for one that is simply working. The
// three axes are OR-ed because they are independent: a volume is a problem if its treatment is not a
// clean success, equally if nothing that can fire selects it, and equally if the storage under it is
// observed not to be finishing the snapshots its treatment depends on.
//
// The third axis is what makes maxCoverageItems safe for the new finding. Without it, a perfectly
// resolved, perfectly selected row on a StorageClass that has never produced a ready snapshot sorts
// with the healthy majority and falls straight off the end of the cap on any cluster with more than
// five hundred volumes — silently, with every count above still saying it was there. That is the same
// truncation trap the attention-first ordering was introduced to close.
func coverageNeedsAttention(p CoveredPVC) int {
	if p.Verdict != CoverageVerdictBackedUp || !p.Selected || p.SnapshotEvidence != "" {
		return 0
	}
	return 1
}

// coverageSelectionRank ranks rows within one class: unselected first, then selected-only-by-an-inert
// schedule, then the properly covered ones.
func coverageSelectionRank(p CoveredPVC) int {
	switch {
	case len(p.Schedules) == 0 && len(p.InertSchedules) == 0:
		return 0
	case len(p.Schedules) == 0:
		return 1
	default:
		return 2
	}
}

// coverPVC is one PVC's verdict: the exposer the operator would resolve for it, or the reason it
// cannot, plus which schedules are responsible for it.
//
// The resolution is exposer.Registry.For followed by SnapshotExposer.Precheck, in that order, which
// is the same order and the same pair of calls the Backup controller makes in advancePending. Nothing
// here interprets a driver, a provisioner or a StorageClass: the class is the resolver's own
// SnapshotExposer.Kind() or the resolver's own sentinel error, and the Detail is the resolver's own
// sentence.
func (c *collector) coverPVC(
	ctx context.Context,
	registry *exposer.Registry,
	pvc *corev1.PersistentVolumeClaim,
	sel []coverageSelector,
	apiAbsent bool,
) CoveredPVC {
	row := CoveredPVC{
		Namespace:     c.red.Namespace(pvc.Namespace),
		Name:          c.red.PVC(pvc.Name),
		Bound:         pvc.Spec.VolumeName != "",
		CapacityBytes: requestedCapacity(pvc),
	}
	if sc := pvc.Spec.StorageClassName; sc != nil {
		// A StorageClass name is a platform-chosen identifier ("ceph-block", "longhorn"), not a
		// customer one, and it is the single most useful field for reading this table — so it survives
		// redaction, exactly as an image tag and a cron expression do.
		row.StorageClass = *sc
	}
	for _, s := range sel {
		if !s.selects(pvc) {
			continue
		}
		if s.inert {
			row.InertSchedules = appendCapped(row.InertSchedules, s.identity+" ("+s.inertWhy+")")
			continue
		}
		row.Schedules = appendCapped(row.Schedules, s.identity)
	}
	row.Selected = len(row.Schedules) > 0

	if apiAbsent {
		row.Class = CoverageSnapshotAPIAbsent
		row.Verdict = coverageVerdict(row.Class)
		return row
	}

	ex, err := registry.For(ctx, pvc)
	switch {
	case err == nil:
		// The class IS the exposer's Kind. Not derived from it, not looked up by it — the string the
		// implementation the operator would actually use reports about itself.
		row.Class = ex.Kind()
	case errors.Is(err, exposer.ErrUnsupported):
		row.Class = CoverageUnsupported
		row.Detail = c.red.Detail(err.Error())
	default:
		row.Class = CoverageUnresolved
		row.Detail = c.red.Detail(err.Error())
	}
	if row.Class == CoverageUnsupported || row.Class == CoverageUnresolved {
		row.Verdict = coverageVerdict(row.Class)
		return row
	}

	// The ACTIVE half. It reads one Secret at most and creates nothing (see exposer.Precheck), and it
	// is what separates "this cluster cannot snapshot the volume today" from "this storage never
	// can" — two verdicts an administrator acts on completely differently, and the whole reason this
	// section resolves rather than tabulating StorageClasses.
	if err := ex.Precheck(ctx); err != nil {
		if errors.Is(err, exposer.ErrPrecheckFailed) {
			row.Class = CoveragePrecheckFailed
		} else {
			row.Class = CoverageUnresolved
		}
		row.Detail = c.red.Detail(err.Error())
	}
	row.Verdict = coverageVerdict(row.Class)
	return row
}

// storageClassKey is the grouping key the snapshot evidence is aggregated under: the PVC's own
// spec.storageClassName, or "" for a PVC that names none.
//
// It is the NAME and not the object, which is deliberate: a statically bound PVC may name a
// StorageClass that does not exist, and grouping two volumes under a string they both name is a fact
// about those two volumes whether or not the object behind the name is there.
func storageClassKey(pvc *corev1.PersistentVolumeClaim) string {
	if pvc.Spec.StorageClassName == nil {
		return ""
	}
	return *pvc.Spec.StorageClassName
}

// snapshotEvidenceByStorageClass aggregates what the cluster's VolumeSnapshots say, per StorageClass,
// over the PVCs this census is scanning.
//
// # Why the StorageClass and not the driver
//
// The driver is the real unit — a stalled csi-snapshotter is stalled for every class its driver serves
// — and this section deliberately does not know it (see CoveredPVC.StorageClass: learning the driver
// means re-implementing the resolver's migration rules, which this section re-implements nothing of).
// Grouping by StorageClass therefore UNDER-claims: two classes on the same broken driver do not inform
// each other, so a volume can be reported unqualified when a driver-level view would have qualified it.
// That is the correct direction to be wrong in. The alternative — grouping by a driver this section
// derived for itself — buys a wider net at the price of the one rule that keeps the whole census
// trustworthy.
//
// # Why the PVCs and not the snapshots
//
// A VolumeSnapshot names its source PVC; it does not name a StorageClass. The PVC list is where the two
// meet, and it is already in hand. A snapshot whose source PVC no longer exists, or which was restored
// from a content rather than a claim, contributes to no class — it is counted in the top-level census
// and simply cannot be attributed here, which StuckSnapshots.WithoutSourcePVC says out loud.
//
// A nil c.snapEvidence — the snapshot LIST was refused — yields an empty map, so every row goes
// unqualified. That is degradation and not a clean bill of health: the refusal is a Diagnostic, which is
// this report's only mechanism for "the operator was not allowed to look".
func (c *collector) snapshotEvidenceByStorageClass(
	pvcs []corev1.PersistentVolumeClaim,
) map[string]snapshotEvidence {
	out := map[string]snapshotEvidence{}
	if len(c.snapEvidence) == 0 {
		return out
	}
	for i := range pvcs {
		pvc := &pvcs[i]
		// The operator's own temporary exposure PVCs are excluded here for the same reason they are
		// excluded from the census itself, and there is a second reason: an exposure clone is provisioned
		// FROM a snapshot, so counting the snapshots that name it would fold the operator's own working
		// set back into evidence about the user's storage.
		if pvc.Labels[apiconst.LabelManagedBy] == apiconst.ManagedByValue {
			continue
		}
		sc := storageClassKey(pvc)
		if sc == "" {
			// A PVC naming no StorageClass at all. It is NOT grouped under "": that bucket would lump
			// together every hand-bound volume in the cluster, which have nothing in common but the
			// absence of a name, and one stalled snapshot would then qualify all of them.
			continue
		}
		ev, ok := c.snapEvidence[pvc.Namespace+"/"+pvc.Name]
		if !ok {
			continue
		}
		out[sc] = out[sc].add(ev)
	}
	return out
}

// qualifyWithSnapshotEvidence attaches the observation to a row whose prediction it qualifies, and to
// no other row.
//
// THREE gates, and each one is a way this could have gone wrong:
//
//   - Only a row the census calls BACKED UP. A skipped or failed row already says it is not backed up;
//     adding "and the snapshotter is stuck" to it is noise on a line that is already actionable.
//   - Only where a snapshot is actually STUCK. A class with no snapshots at all, or with none older
//     than the grace, is left alone — absence of evidence is not evidence of failure, and a fresh
//     installation has no snapshots anywhere.
//   - The Verdict and Class are left EXACTLY as the resolver set them. The row still says what the
//     operator will do, because that has not changed; what is added is what the cluster is observed to
//     be doing about it. Turning this into Skipped or Failed would be diagnosing somebody else's
//     controller from a symptom, and it would move a phase that real automation reacts to.
//
// The two sentences are different because the remedies are. "None of the snapshots that exist on this
// class is ready" is the CephFS incident — a snapshotter that has never worked, and the reader needs to
// go and look at that driver's sidecar. "Some are ready and some are stuck" is a working class with
// something wrong in it, which is a much smaller claim and is worded like one.
func qualifyWithSnapshotEvidence(row *CoveredPVC, ev snapshotEvidence) {
	if row.Verdict != CoverageVerdictBackedUp || ev.stuck == 0 {
		return
	}
	row.StuckOnStorageClass = ev.stuck
	row.ReadyOnStorageClass = ev.ready
	grace := int(snapshotStallGrace.Minutes())
	if ev.ready == 0 {
		row.SnapshotEvidence = fmt.Sprintf(
			"OBSERVED, not predicted: of the %d VolumeSnapshot(s) that exist on StorageClass %q, NONE is "+
				"readyToUse and %d have been bound to a VolumeSnapshotContent for more than %d minutes "+
				"(oldest %d h). The treatment above is what the operator WILL attempt; on the evidence in "+
				"this cluster it has not yet completed once on this class. Check the csi-snapshotter "+
				"sidecar of the driver behind it. This changes no verdict here.",
			ev.total, row.StorageClass, ev.stuck, grace, ev.oldestStuckHours)
		return
	}
	row.SnapshotEvidence = fmt.Sprintf(
		"OBSERVED, not predicted: StorageClass %q has %d readyToUse VolumeSnapshot(s) and %d that have "+
			"been bound to a VolumeSnapshotContent for more than %d minutes without becoming ready "+
			"(oldest %d h). The class works; something on it is not advancing. This changes no verdict here.",
		row.StorageClass, ev.ready, ev.stuck, grace, ev.oldestStuckHours)
}

// appendCapped adds an identity to a row's schedule list, keeping it to maxCoveredSchedules and
// recording the overflow as a count rather than dropping it silently.
func appendCapped(list []string, id string) []string {
	switch {
	case len(list) < maxCoveredSchedules:
		return append(list, id)
	case strings.HasPrefix(list[len(list)-1], "+"):
		n := 0
		_, _ = fmt.Sscanf(list[len(list)-1], "+%d more", &n)
		list[len(list)-1] = fmt.Sprintf("+%d more", n+1)
		return list
	default:
		return append(list, "+1 more")
	}
}

// requestedCapacity is the PVC's requested storage in bytes, or 0 when it asks for none. The REQUEST
// and not the actual usage, because actual usage is not knowable from the API and a number that
// looked like it was would be worse than no number.
func requestedCapacity(pvc *corev1.PersistentVolumeClaim) int64 {
	q, ok := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	if !ok {
		return 0
	}
	v, ok := q.AsInt64()
	if !ok {
		return 0
	}
	return v
}

// coverageSelector is one standing schedule reduced to the question this census asks of it: which
// namespaces does it reach, which PVCs inside them does it take, and can it fire at all.
type coverageSelector struct {
	// identity is the redacted display name, "cluster/<name>" or "<namespace>/<name>".
	identity string
	// namespaces is the resolved target set. For a namespace-plane BackupSchedule that is its own
	// namespace and nothing else; for a ClusterBackupSchedule it is nsselector.Match's answer over
	// the live namespaces — the operator's own resolver, so a report of the fan-out cannot disagree
	// with the fan-out.
	namespaces map[string]bool
	pvcSel     cbv1.PVCSelector

	// inert means this schedule cannot produce a run: it is suspended, or its cron does not parse.
	// Such a schedule still SELECTS its PVCs — that is why it is recorded rather than dropped — but a
	// volume whose only cover is inert is unprotected in fact while looking covered on paper, and the
	// census counts it separately (Coverage.InertOnly) for exactly that reason.
	inert    bool
	inertWhy string
}

// selects reports whether this schedule takes pvc.
func (s coverageSelector) selects(pvc *corev1.PersistentVolumeClaim) bool {
	return s.namespaces[pvc.Namespace] && s.pvcSel.Matches(pvc.Name, pvc.Labels)
}

// coverageSelectors builds the selection index from every standing schedule of both planes.
//
// Only SCHEDULES are read. A one-off Backup or ClusterBackup protected a namespace once, on a day
// somebody typed a command; this section answers "what will happen from now on", and a manual run is
// not an answer to that. A PVC whose only protection was a manual backup last March is correctly
// reported as selected by nothing.
//
// It reads through the census's counted reader rather than the collector's own, so its three Lists
// appear in Coverage.APIReads. The plan section lists the same objects for its own purposes; that is
// three duplicated requests on the whole command, and the alternative — one shared cache whose cost
// is attributed to whichever section happened to touch it first — would make the reported figure
// depend on the order of the calls in Collect.
func (c *collector) coverageSelectors(ctx context.Context, reader *coverageReader) []coverageSelector {
	var out []coverageSelector

	var nsScheds cbv1.BackupScheduleList
	if err := reader.List(ctx, &nsScheds); err != nil {
		c.diag("coverage",
			"namespace-plane schedules were not counted, so some PVCs may be reported as selected "+
				"by nothing when they are not",
			err)
	} else {
		for i := range nsScheds.Items {
			s := &nsScheds.Items[i]
			inert, why := scheduleIsInert(s.Spec.Paused, s.Spec.Schedule, s.Spec.Timezone)
			out = append(out, coverageSelector{
				identity:   c.red.Namespace(s.Namespace) + "/" + c.red.Schedule(s.Name),
				namespaces: map[string]bool{s.Namespace: true},
				pvcSel:     s.Spec.PVCSelector,
				inert:      inert,
				inertWhy:   why,
			})
		}
	}

	var clScheds cbv1.ClusterBackupScheduleList
	if err := reader.List(ctx, &clScheds); err != nil {
		c.diag("coverage",
			"cluster-plane schedules were not counted, so some PVCs may be reported as selected by "+
				"nothing when they are not",
			err)
		return out
	}
	if len(clScheds.Items) == 0 {
		return out
	}

	var namespaces corev1.NamespaceList
	if err := reader.List(ctx, &namespaces); err != nil {
		c.diag("coverage",
			"the cluster schedules' namespace fan-out could not be resolved, so their PVCs may be "+
				"reported as selected by nothing",
			err)
		return out
	}
	for i := range clScheds.Items {
		s := &clScheds.Items[i]
		inert, why := scheduleIsInert(s.Spec.Paused, s.Spec.Schedule, s.Spec.Timezone)
		// nsselector.Match, not a re-reading of the selector. It returns an error for a selector that
		// breaks admission rule 8 (no positive form, or more than one), and such a schedule fans out
		// into NOTHING — the cluster-backup controller refuses to guess and so does this. The
		// resulting empty namespace set is the honest answer: it protects no PVC, and the plan section
		// says why in words.
		names, err := nsselector.Match(namespaces.Items, s.Spec.Template.Spec.Namespaces)
		if err != nil {
			continue
		}
		set := make(map[string]bool, len(names))
		for _, n := range names {
			set[n] = true
		}
		out = append(out, coverageSelector{
			identity:   apiconst.OriginCluster + "/" + c.red.Schedule(s.Name),
			namespaces: set,
			pvcSel:     s.Spec.Template.Spec.PVCSelector,
			inert:      inert,
			inertWhy:   why,
		})
	}
	return out
}

// scheduleIsInert reports whether a schedule can produce a run at all, and why not.
//
// Two causes, and they are folded together on purpose: from the point of view of the data, a
// suspended schedule and one whose cron expression does not parse are the same event — nothing will
// run — and a census that counted only the first would leave the second reading as protected. The
// WHY is kept so the remedy stays distinct, because clearing spec.paused and fixing a typo are very
// different jobs.
func scheduleIsInert(paused bool, cron, tz string) (bool, string) {
	if paused {
		return true, "suspended"
	}
	if !cronValid(cron, tz) {
		return true, "cron does not parse"
	}
	return false, ""
}

// compile-time assurance that the coverage reader really is usable where a client.Reader is wanted,
// and the read-only client where a client.Client is. Both are asserted here rather than discovered at
// the call site so a signature change in controller-runtime fails the build with a clear location.
var (
	_ client.Reader = (*coverageReader)(nil)
	_ client.Client = readOnlyClient{}
)
