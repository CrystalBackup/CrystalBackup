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
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/yaml"
)

// FieldManager is the server-side-apply manager name every manifest restore writes under
// (04-manifest-backup.md §5.2). It is a fixed identity on purpose: Kubernetes tracks field
// ownership by manager name, so a stable one lets a second restore of the same backup update
// exactly the fields the first one owns instead of accumulating a new owner each time.
const FieldManager = "crystalbackup-restore"

// RestoreMode mirrors the CRD's spec.mode for the resource half. Kept as a local string type
// so this package — which runs inside the mover — does not import the API types.
type RestoreMode string

const (
	// ModeOverwrite server-side applies: create-or-update, keeping objects present in the
	// target but absent from the backup.
	ModeOverwrite RestoreMode = "Overwrite"
	// ModeRecreate deletes an existing object and creates it again from the backup.
	ModeRecreate RestoreMode = "Recreate"
)

// Outcome is what happened to one manifest, mirroring RestoreResourceOutcome in the API.
type Outcome string

const (
	OutcomeCreated    Outcome = "Created"
	OutcomeConfigured Outcome = "Configured"
	OutcomeRecreated  Outcome = "Recreated"
	OutcomeFailed     Outcome = "Failed"
)

// deleteSettleTimeout bounds the wait for a Recreate delete to actually take effect. A
// finalizer that never runs must not hang the restore of every remaining object, so the wait
// gives up and reports the object Failed — never force-deletes. Stripping a finalizer here
// would run the very finalizer-guarded cleanup (a volume detach, an external record) that the
// object's controller registered it to do (04-manifest-backup.md §5.2).
const deleteSettleTimeout = 30 * time.Second

// deleteSettlePoll is how often the delete wait re-checks.
const deleteSettlePoll = 500 * time.Millisecond

// Applier applies a restored manifest tree to a target namespace. Like Dumper it takes narrow
// upstream interfaces: this runs in the mover, which has no scheme and no controller-runtime
// client.
type Applier struct {
	Dynamic dynamic.Interface
	// Mapper resolves an index entry's group/version/kind to the resource the dynamic client
	// needs. A kind the target cluster does not serve fails HERE, per resource, which is what
	// turns a missing CRD into one reported failure instead of an aborted restore.
	Mapper meta.RESTMapper
}

// ApplyOptions configures one restore pass.
type ApplyOptions struct {
	// SourceDir is the restored snapshot root: index.json plus <group>/<Kind>/<name>.yaml.
	SourceDir string
	// TargetNamespace is stamped onto every object. S6 strips metadata.namespace at backup
	// precisely so this can differ from the captured namespace (cross-namespace ClusterRestore).
	TargetNamespace string
	// Mode resolves conflicts on objects the target already holds.
	Mode RestoreMode
	// DryRun runs the whole pipeline against the API server with dryRun=All: the server
	// validates and merges, and persists nothing (04-manifest-backup.md §5.4).
	DryRun bool
	// Selection narrows the set. Nil restores everything in the snapshot.
	Selection *CompiledSelection
	// StorageClassMapping rewrites PVC spec.storageClassName (§5.3). A value of "" REMOVES the
	// field so the target's default class applies; an unmapped class passes through untouched.
	StorageClassMapping map[string]string
}

// Report is the accounting one restore pass produces.
type Report struct {
	// Applied counts objects that reached the cluster (or would have, in a dry run).
	Applied int
	// Failed counts objects that could not be applied. The restore continues past each one.
	Failed int
	// Entries records NON-TRIVIAL outcomes only. A plain Created is the expected case and
	// listing every one of them would bury the handful a human needs to see
	// (04-manifest-backup.md §5.4).
	Entries []ResourceOutcome
	// Skipped counts manifests the selection excluded. Reported so "restored 3 of 200" is
	// distinguishable from "the snapshot only had 3".
	Skipped int
}

// ResourceOutcome is one manifest's result.
type ResourceOutcome struct {
	Group   string   `json:"group,omitempty"`
	Kind    string   `json:"kind,omitempty"`
	Name    string   `json:"name,omitempty"`
	Outcome Outcome  `json:"outcome,omitempty"`
	Reason  string   `json:"reason,omitempty"`
	Changed []string `json:"changed,omitempty"`
}

// plannedResource is one selected manifest, resolved and ordered.
type plannedResource struct {
	entry IndexEntry
	obj   *unstructured.Unstructured
	phase int
}

// Apply restores the tree under opts.SourceDir into opts.TargetNamespace.
//
// It returns an error ONLY for a failure that invalidates the whole pass (an unreadable
// index). Everything per-object — a missing CRD, a webhook rejection, a finalizer holding a
// delete — is recorded in the Report and the pass continues, because a restore that stops at
// the first bad object leaves the namespace in a state nobody asked for and no one can reason
// about (adr/0007).
func (a *Applier) Apply(ctx context.Context, opts ApplyOptions) (*Report, error) {
	if opts.TargetNamespace == "" {
		return nil, errors.New("apply: no target namespace")
	}

	idx, err := a.readIndex(opts.SourceDir)
	if err != nil {
		return nil, err
	}

	planned, report := a.plan(idx, opts)

	for i := range planned {
		a.applyOne(ctx, &planned[i], opts, report)
	}
	return report, nil
}

func (a *Applier) readIndex(dir string) (*Index, error) {
	raw, err := os.ReadFile(filepath.Join(dir, IndexFileName))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", IndexFileName, err)
	}
	return ParseIndex(raw)
}

// plan reads, filters, transforms and orders the manifests before a single API call is made.
// Doing it in one pass up front means the ordering is over the objects that will actually be
// applied, not over the snapshot's contents.
func (a *Applier) plan(idx *Index, opts ApplyOptions) ([]plannedResource, *Report) {
	report := &Report{}
	planned := make([]plannedResource, 0, len(idx.Resources))

	for _, entry := range idx.Resources {
		obj, err := readManifest(filepath.Join(opts.SourceDir, entry.Path))
		if err != nil {
			// A file the index names but that is unreadable is a per-resource failure, not a
			// dead restore: the other 200 objects are still worth applying.
			report.Failed++
			report.Entries = append(report.Entries, ResourceOutcome{
				Group: entry.Group, Kind: entry.Kind, Name: entry.Name,
				Outcome: OutcomeFailed, Reason: err.Error(),
			})
			continue
		}
		if opts.Selection != nil && !opts.Selection.Matches(entry.Group, entry.Kind, entry.Name, obj.GetLabels()) {
			report.Skipped++
			continue
		}

		// The two — and only two — restore-time transformations (04-manifest-backup.md §5.3
		// and S6). Anything else the stored manifest needed was already done at backup.
		obj.SetNamespace(opts.TargetNamespace)
		if isPVC(entry.Group, entry.Kind) {
			applyStorageClassMapping(obj, opts.StorageClassMapping)
		}

		planned = append(planned, plannedResource{
			entry: entry, obj: obj, phase: applyPhase(entry.Group, entry.Kind),
		})
	}

	// Phase first, then (group, Kind, name) within it: the spec's ordering, and the reason two
	// restores of one snapshot issue identical call sequences (§5.1).
	slices.SortFunc(planned, func(x, y plannedResource) int {
		if c := x.phase - y.phase; c != 0 {
			return c
		}
		if c := strings.Compare(x.entry.Group, y.entry.Group); c != 0 {
			return c
		}
		if c := strings.Compare(x.entry.Kind, y.entry.Kind); c != 0 {
			return c
		}
		return strings.Compare(x.entry.Name, y.entry.Name)
	})
	return planned, report
}

func readManifest(path string) (*unstructured.Unstructured, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	var m map[string]any
	if err := yaml.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("parse manifest %s: %w", filepath.Base(path), err)
	}
	return &unstructured.Unstructured{Object: m}, nil
}

func isPVC(group, kind string) bool { return group == coreGroup && kind == kindPVC }

// applyStorageClassMapping rewrites a PVC's class. The three cases are distinct and the
// middle one is easy to get wrong: mapping to "" must REMOVE the field (so the target's
// default class is chosen) rather than set it to the empty string, which Kubernetes reads as
// "no class at all" and leaves the claim unbound forever (04-manifest-backup.md §5.3).
func applyStorageClassMapping(obj *unstructured.Unstructured, mapping map[string]string) {
	if len(mapping) == 0 {
		return
	}
	current, found, err := unstructured.NestedString(obj.Object, "spec", "storageClassName")
	if err != nil || !found || current == "" {
		return
	}
	target, mapped := mapping[current]
	if !mapped {
		return // unmapped classes pass through unchanged
	}
	if target == "" {
		unstructured.RemoveNestedField(obj.Object, "spec", "storageClassName")
		return
	}
	// The value came from a map[string]string, so this cannot fail.
	_ = unstructured.SetNestedField(obj.Object, target, "spec", "storageClassName")
}

// applyOne resolves the resource, reads what is already there, and applies it per mode.
func (a *Applier) applyOne(ctx context.Context, p *plannedResource, opts ApplyOptions, report *Report) {
	gvk := schema.GroupVersionKind{Group: p.entry.Group, Version: p.entry.Version, Kind: p.entry.Kind}
	mapping, err := a.Mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		// The documented shape of "a custom resource whose CRD is absent in the target
		// cluster" (§5.1): the server's own no-matches error, per resource, restore continues.
		report.fail(p.entry, err.Error())
		return
	}
	ri := a.Dynamic.Resource(mapping.Resource).Namespace(opts.TargetNamespace)

	live, err := ri.Get(ctx, p.entry.Name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		live = nil
	case err != nil:
		report.fail(p.entry, fmt.Sprintf("read the existing object: %v", err))
		return
	}

	if opts.Mode == ModeRecreate {
		a.recreate(ctx, ri, p, live, opts, report)
		return
	}
	a.overwrite(ctx, ri, p, live, opts, report)
}

// overwrite server-side applies (04-manifest-backup.md §5.2).
//
// Force is TRUE, and that is a deliberate reading rather than an oversight. Kubernetes rejects
// an apply that changes a field another manager owns unless forced — and in a real namespace
// essentially every pre-existing object is owned by kubectl, Helm or its controller. Without
// force, Overwrite would fail on precisely the objects it exists to reconcile, and the M3 e2e
// (a pre-created DRIFTED ConfigMap that Overwrite must SSA-merge) could not pass. The spec's
// "server-side apply resolves field ownership per Kubernetes conflict rules" describes the
// MERGE semantics — how lists key, which fields are atomic — not a refusal to take ownership.
//
// What force does not do is delete anything: fields present in the target but absent from the
// backup keep their values, which is exactly the "extras preserved" the mode promises.
func (a *Applier) overwrite(
	ctx context.Context,
	ri dynamic.ResourceInterface,
	p *plannedResource,
	live *unstructured.Unstructured,
	opts ApplyOptions,
	report *Report,
) {
	applied, err := ri.Apply(ctx, p.entry.Name, p.obj, metav1.ApplyOptions{
		FieldManager: FieldManager,
		Force:        true,
		DryRun:       dryRunOption(opts.DryRun),
	})
	if err != nil {
		report.fail(p.entry, err.Error())
		return
	}

	report.Applied++
	if live == nil {
		// A plain create is the expected case: counted, not listed.
		return
	}
	// An update IS notable — something in the target differed from the backup — so it earns an
	// entry, carrying the paths that moved.
	report.Entries = append(report.Entries, ResourceOutcome{
		Group: p.entry.Group, Kind: p.entry.Kind, Name: p.entry.Name,
		Outcome: OutcomeConfigured,
		Changed: changedPaths(live.Object, applied.Object, MaxChangedPaths),
	})
}

// recreate deletes an existing object and creates it again (04-manifest-backup.md §5.2).
func (a *Applier) recreate(
	ctx context.Context,
	ri dynamic.ResourceInterface,
	p *plannedResource,
	live *unstructured.Unstructured,
	opts ApplyOptions,
	report *Report,
) {
	if live != nil {
		// A dry run must not delete. The planned outcome is still Recreated, and validating the
		// object with a forced dry-run apply is the closest the server can get to "would this
		// work" without destroying the object it would replace.
		if opts.DryRun {
			if _, err := ri.Apply(ctx, p.entry.Name, p.obj, metav1.ApplyOptions{
				FieldManager: FieldManager, Force: true, DryRun: []string{metav1.DryRunAll},
			}); err != nil {
				report.fail(p.entry, err.Error())
				return
			}
			report.Applied++
			report.Entries = append(report.Entries, ResourceOutcome{
				Group: p.entry.Group, Kind: p.entry.Kind, Name: p.entry.Name, Outcome: OutcomeRecreated,
			})
			return
		}
		if err := a.deleteAndWait(ctx, ri, p.entry.Name, live.GetUID()); err != nil {
			report.fail(p.entry, err.Error())
			return
		}
	}

	if _, err := ri.Create(ctx, p.obj, metav1.CreateOptions{
		FieldManager: FieldManager,
		DryRun:       dryRunOption(opts.DryRun),
	}); err != nil {
		report.fail(p.entry, err.Error())
		return
	}

	report.Applied++
	if live != nil {
		report.Entries = append(report.Entries, ResourceOutcome{
			Group: p.entry.Group, Kind: p.entry.Kind, Name: p.entry.Name, Outcome: OutcomeRecreated,
		})
	}
}

// deleteAndWait deletes the object and waits for it to actually be gone before the recreate.
//
// Waiting is not optional: Delete returns as soon as deletionTimestamp is set, so creating
// immediately after would race an object that still exists and fail with AlreadyExists — or,
// worse, succeed against a name the API server frees a moment later. The UID check
// distinguishes "gone" from "gone and something else recreated it under the same name", which
// is a live possibility when a controller is watching.
func (a *Applier) deleteAndWait(ctx context.Context, ri dynamic.ResourceInterface, name string, uid types.UID) error {
	// Preconditions pin the delete to the object we read: without them a retry could delete a
	// replacement that appeared in between.
	policy := metav1.DeletePropagationForeground
	err := ri.Delete(ctx, name, metav1.DeleteOptions{
		Preconditions:     &metav1.Preconditions{UID: &uid},
		PropagationPolicy: &policy,
	})
	if err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
		return fmt.Errorf("delete for recreate: %w", err)
	}

	deadline := time.Now().Add(deleteSettleTimeout)
	for {
		current, getErr := ri.Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(getErr) || (getErr == nil && current.GetUID() != uid) {
			return nil
		}
		if getErr != nil && !apierrors.IsNotFound(getErr) {
			return fmt.Errorf("waiting for the delete to settle: %w", getErr)
		}
		if time.Now().After(deadline) {
			// Report what is actually holding it, which is nearly always a finalizer. Never
			// force: see deleteSettleTimeout.
			if f := current.GetFinalizers(); len(f) > 0 {
				return fmt.Errorf("still present %s after delete, held by finalizer(s) %s",
					deleteSettleTimeout, strings.Join(f, ", "))
			}
			return fmt.Errorf("still present %s after delete", deleteSettleTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(deleteSettlePoll):
		}
	}
}

// dryRunOption turns the flag into the API's dryRun list.
func dryRunOption(dry bool) []string {
	if dry {
		return []string{metav1.DryRunAll}
	}
	return nil
}

func (r *Report) fail(entry IndexEntry, reason string) {
	r.Failed++
	r.Entries = append(r.Entries, ResourceOutcome{
		Group: entry.Group, Kind: entry.Kind, Name: entry.Name,
		Outcome: OutcomeFailed, Reason: reason,
	})
}
