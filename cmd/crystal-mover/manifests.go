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

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"

	"github.com/CrystalBackup/CrystalBackup/internal/manifests"
	"github.com/CrystalBackup/CrystalBackup/internal/mover"
	"github.com/CrystalBackup/CrystalBackup/internal/sanitize"
)

// dumpManifests reads the namespace from the API server and writes the sanitized tree that
// restic is about to back up.
//
// This is the one place the mover touches Kubernetes. It exists here, inside the mover,
// rather than in the operator because the operator never handles backup data bytes
// (01-architecture.md §1) and the dump must not travel through exec/stdout (delta 8) — so the
// only remaining shape is: the process that writes to the repository is the process that
// reads the objects.
//
// It runs BEFORE restic, and a failure here is fatal for the run: `restic backup` over an
// empty or half-written directory would produce a snapshot that looks successful and restores
// to nothing, which is the single worst outcome a backup tool has.
func dumpManifests(ctx context.Context) (*manifests.Index, error) {
	namespace := os.Getenv(mover.EnvManifestsNamespace)
	if namespace == "" {
		return nil, fmt.Errorf("%s is not set; the manifest mover cannot guess its namespace",
			mover.EnvManifestsNamespace)
	}

	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("in-cluster config (the manifest mover needs an automounted "+
			"ServiceAccount token — see spec/03-security-and-tenancy.md I6): %w", err)
	}
	disco, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("discovery client: %w", err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("dynamic client: %w", err)
	}
	s, err := sanitize.New()
	if err != nil {
		return nil, fmt.Errorf("sanitizer: %w", err)
	}

	d := &manifests.Dumper{Disco: disco, Dynamic: dyn, Sanitizer: s}
	// The destination is ManifestsRoot/<namespace>, which is byte-for-byte the path restic
	// will record (internal/restic.ManifestsIdentity). Deriving it here from the same two
	// pieces the identity is built from means the dump target and the snapshot path cannot
	// drift apart.
	dest := mover.ManifestsRoot + "/" + namespace

	idx, err := d.Dump(ctx, manifests.Options{
		Namespace:         namespace,
		ClusterID:         os.Getenv(mover.EnvManifestsClusterID),
		BackupName:        os.Getenv(mover.EnvManifestsBackupName),
		ExcludeSecretData: os.Getenv(mover.EnvManifestsExcludeSecretData) == "true",
	}, dest)
	if err != nil {
		return nil, fmt.Errorf("dump %s: %w", namespace, err)
	}

	// The pod log is where an operator looks first; the full detail is in index.json inside
	// the snapshot, which survives the pod.
	fmt.Fprintf(os.Stderr, "crystal-mover: captured %d resource(s) from %s into %s\n",
		idx.ResourceCount, namespace, dest)
	for _, w := range idx.Warnings {
		fmt.Fprintf(os.Stderr, "crystal-mover: WARNING %s/%s: %s\n", w.Group, w.Kind, w.Message)
	}
	return idx, nil
}

// dumpClusterManifests reads the cluster's CLUSTER-SCOPED objects from the API server and writes
// the sanitized tree that restic is about to back up (adr/0011 §1). It is the cluster-plane
// sibling of dumpManifests and shares its shape exactly: it is the ONE place this mover touches
// Kubernetes, it runs BEFORE restic, and a failure here is fatal — `restic backup` over an empty
// or half-written directory would produce a snapshot that looks successful and restores to
// nothing, the single worst outcome a backup tool has.
//
// It differs from dumpManifests only in what the feature is about: there is no namespace (the
// capture spans the whole cluster and its snapshot belongs to none), and the selection arrives
// JSON-encoded in the environment, compiled here into the ClusterSelector the dump drives. A
// malformed selection is fatal rather than degrading into "capture everything cluster-scoped".
func dumpClusterManifests(ctx context.Context) (*manifests.Index, error) {
	opts, err := manifests.DecodeClusterCaptureOptions(os.Getenv(mover.EnvClusterManifestsSelection))
	if err != nil {
		return nil, err
	}
	selector, err := manifests.CompileClusterSelector(opts)
	if err != nil {
		// A malformed selection must never degrade into "capture everything cluster-scoped": that
		// would sweep in every add-on's cluster-scoped objects and make the snapshot both enormous
		// and dangerous to restore.
		return nil, fmt.Errorf("cluster selection: %w", err)
	}

	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("in-cluster config (the cluster-manifest mover needs an automounted "+
			"ServiceAccount token — see spec/03-security-and-tenancy.md I6): %w", err)
	}
	disco, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("discovery client: %w", err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("dynamic client: %w", err)
	}
	s, err := sanitize.New()
	if err != nil {
		return nil, fmt.Errorf("sanitizer: %w", err)
	}

	d := &manifests.ClusterDumper{Disco: disco, Dynamic: dyn, Sanitizer: s, Selector: selector}
	// The destination is ClusterManifestsRoot, byte-for-byte the path restic will record
	// (restic.ClusterManifestsIdentity). There is no per-namespace suffix — one run writes one
	// cluster-manifests snapshot at the fixed path — so, unlike dumpManifests, nothing is appended.
	dest := mover.ClusterManifestsRoot

	idx, err := d.Dump(ctx, manifests.ClusterOptions{
		ClusterID:  os.Getenv(mover.EnvClusterManifestsClusterID),
		BackupName: os.Getenv(mover.EnvClusterManifestsBackupName),
	}, dest)
	if err != nil {
		return nil, fmt.Errorf("cluster dump: %w", err)
	}

	// The pod log is where an operator looks first; the full detail is in index.json inside the
	// snapshot, which survives the pod.
	fmt.Fprintf(os.Stderr, "crystal-mover: captured %d cluster-scoped resource(s) into %s\n",
		idx.ResourceCount, dest)
	for _, w := range idx.Warnings {
		fmt.Fprintf(os.Stderr, "crystal-mover: WARNING %s/%s: %s\n", w.Group, w.Kind, w.Message)
	}
	return idx, nil
}

// applyManifests applies a restored manifest tree to the target namespace
// (spec/04-manifest-backup.md §5). The mirror of dumpManifests, and it runs on the other side
// of restic: the tree must exist before there is anything to apply.
//
// A failure here means the restore did not happen, so it is reported as a failed run — but a
// per-RESOURCE failure is not: those are counted, listed and stepped over, because a restore
// that stops at the first bad object leaves a namespace in a state nobody asked for (adr/0007).
func applyManifests(ctx context.Context) (*manifests.Report, error) {
	targetNamespace := os.Getenv(mover.EnvManifestsNamespace)
	if targetNamespace == "" {
		return nil, fmt.Errorf("%s is not set; the manifest mover cannot guess its target namespace",
			mover.EnvManifestsNamespace)
	}
	sourceDir := os.Getenv(mover.EnvManifestsRestoreDir)
	if sourceDir == "" {
		return nil, fmt.Errorf("%s is not set; the apply cannot guess where restic put the tree",
			mover.EnvManifestsRestoreDir)
	}

	selection, err := manifests.DecodeSelection(os.Getenv(mover.EnvManifestsSelection))
	if err != nil {
		return nil, err
	}
	compiled, err := selection.Compile()
	if err != nil {
		// A malformed selector must never degrade into "restore everything": that would turn a
		// narrow, deliberate restore into a namespace-wide one, in Overwrite or Recreate mode.
		return nil, fmt.Errorf("selection: %w", err)
	}
	classMapping, err := decodeStorageClassMapping(os.Getenv(mover.EnvManifestsStorageClassMapping))
	if err != nil {
		return nil, err
	}

	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("in-cluster config (the manifest mover needs an automounted "+
			"ServiceAccount token — see spec/03-security-and-tenancy.md I6): %w", err)
	}
	disco, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("discovery client: %w", err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("dynamic client: %w", err)
	}

	// A deferred mapper resolves kinds against the TARGET cluster's discovery, which is what
	// makes "no matches for kind" the honest per-resource answer for a custom resource whose
	// CRD was never installed here (§5.1).
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(disco))

	a := &manifests.Applier{Dynamic: dyn, Mapper: mapper}
	opts := manifests.ApplyOptions{
		SourceDir:           sourceDir,
		TargetNamespace:     targetNamespace,
		Mode:                manifests.RestoreMode(os.Getenv(mover.EnvManifestsMode)),
		DryRun:              os.Getenv(mover.EnvManifestsDryRun) == "true",
		Selection:           compiled,
		StorageClassMapping: classMapping,
	}

	report, err := a.Apply(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("apply into %s: %w", targetNamespace, err)
	}

	// The pod log carries the FULL report; the termination message carries only what fits in
	// 4096 bytes. An operator chasing a specific object needs this, and it is the only place
	// the untruncated list exists.
	dry := ""
	if opts.DryRun {
		dry = " (dry run, nothing persisted)"
	}
	fmt.Fprintf(os.Stderr, "crystal-mover: applied %d, failed %d, skipped %d in %s%s\n",
		report.Applied, report.Failed, report.Skipped, targetNamespace, dry)
	for _, e := range report.Entries {
		fmt.Fprintf(os.Stderr, "crystal-mover: %s %s/%s/%s %s %s\n",
			e.Outcome, e.Group, e.Kind, e.Name, e.Reason, strings.Join(e.Changed, ","))
	}
	return report, nil
}

// applyClusterManifests applies a restored CLUSTER-scoped manifest tree (adr/0011 §2). The
// cluster-plane sibling of applyManifests, and it runs on the same side of restic (after). It
// differs in exactly the way the objects differ: no target namespace is read or stamped
// (ClusterScoped), and the source tree is the cluster-manifests restore dir. Per-resource
// failures are counted and stepped over, same as the namespaced apply — a CRD that fails to
// apply must not abandon the StorageClasses that would have followed it.
func applyClusterManifests(ctx context.Context) (*manifests.Report, error) {
	sourceDir := os.Getenv(mover.EnvManifestsRestoreDir)
	if sourceDir == "" {
		return nil, fmt.Errorf("%s is not set; the cluster apply cannot guess where restic put the tree",
			mover.EnvManifestsRestoreDir)
	}

	selection, err := manifests.DecodeSelection(os.Getenv(mover.EnvManifestsSelection))
	if err != nil {
		return nil, err
	}
	compiled, err := selection.Compile()
	if err != nil {
		// A malformed selector must never degrade into "restore everything cluster-scoped":
		// recreating CRDs and cluster RBAC is the most privileged thing this tool does.
		return nil, fmt.Errorf("selection: %w", err)
	}

	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("in-cluster config (the cluster-manifest mover needs an automounted "+
			"ServiceAccount token — see spec/03-security-and-tenancy.md I6): %w", err)
	}
	disco, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("discovery client: %w", err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("dynamic client: %w", err)
	}
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(disco))

	a := &manifests.Applier{Dynamic: dyn, Mapper: mapper}
	opts := manifests.ApplyOptions{
		SourceDir:     sourceDir,
		Mode:          manifests.RestoreMode(os.Getenv(mover.EnvManifestsMode)),
		DryRun:        os.Getenv(mover.EnvManifestsDryRun) == "true",
		Selection:     compiled,
		ClusterScoped: true,
	}

	report, err := a.Apply(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("apply cluster-scoped objects: %w", err)
	}

	dry := ""
	if opts.DryRun {
		dry = " (dry run, nothing persisted)"
	}
	fmt.Fprintf(os.Stderr, "crystal-mover: applied %d, failed %d, skipped %d cluster-scoped resource(s)%s\n",
		report.Applied, report.Failed, report.Skipped, dry)
	for _, e := range report.Entries {
		fmt.Fprintf(os.Stderr, "crystal-mover: %s %s/%s/%s %s %s\n",
			e.Outcome, e.Group, e.Kind, e.Name, e.Reason, strings.Join(e.Changed, ","))
	}
	return report, nil
}

// decodeStorageClassMapping reads the JSON map the operator passed. An empty value is the
// namespaced Restore's normal case — it exposes no storageClassMapping at all (§5.3).
func decodeStorageClassMapping(encoded string) (map[string]string, error) {
	if strings.TrimSpace(encoded) == "" {
		return nil, nil
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(encoded), &m); err != nil {
		return nil, fmt.Errorf("decode %s: %w", mover.EnvManifestsStorageClassMapping, err)
	}
	return m, nil
}

// reportToResult folds the apply's accounting into the MoverResult the controller reads.
func reportToResult(result mover.MoverResult, report *manifests.Report) mover.MoverResult {
	// Counts are bounded by a namespace's object count, orders of magnitude below int32.
	result.RestoredResources = int32(report.Applied) //nolint:gosec // bounded above
	result.FailedResources = int32(report.Failed)    //nolint:gosec // bounded above
	result.SkippedResources = int32(report.Skipped)  //nolint:gosec // bounded above
	for _, e := range report.Entries {
		result.ResourceEntries = append(result.ResourceEntries, mover.ResourceEntry{
			Group: e.Group, Kind: e.Kind, Name: e.Name,
			Outcome: string(e.Outcome), Reason: e.Reason, Changed: e.Changed,
		})
	}
	// Trim to what the kubelet will actually carry. The counts survive any amount of trimming.
	return result.Fit()
}
