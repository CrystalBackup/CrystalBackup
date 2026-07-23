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
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/utils/clock"

	"github.com/CrystalBackup/CrystalBackup/internal/sanitize"
)

// ClusterDumper captures a cluster's CLUSTER-SCOPED resources (adr/0011). It is the cluster-plane
// sibling of Dumper and shares its shape exactly: the same two narrow upstream clients (never a
// controller-runtime client — this runs in the mover, which has no scheme, no cache and no
// business knowing tenant types), the same reused Sanitizer, the same injectable clock.
//
// The two differ in only what the whole feature is about: Dumper enumerates the
// `namespaced == true` resources of ONE namespace and drives the namespaced exclusion context,
// while ClusterDumper enumerates the `namespaced == false` resources of the WHOLE cluster and
// drives a ClusterSelector — which cluster-scoped kinds a run captures, and which objects within
// them (adr/0011 §1). A namespace does not restore into a vacuum: its PVCs name a StorageClass,
// its workloads a PriorityClass, its custom resources their CRDs — objects the namespaced path
// cannot see by construction.
type ClusterDumper struct {
	Disco     discovery.DiscoveryInterface
	Dynamic   dynamic.Interface
	Sanitizer *sanitize.Sanitizer
	// Selector decides which cluster-scoped kinds and objects this run captures. It is the
	// executable form of adr/0011 §1's allow-list + the run's include/exclude, compiled once by
	// the caller (CompileClusterSelector) and driven here.
	Selector *ClusterSelector
	// Clock is injectable so capturedAt is deterministic in tests (a real timestamp in a golden
	// file would make it fail a second later).
	Clock clock.PassiveClock
	// PageSize defaults to DefaultPageSize when zero.
	PageSize int64
}

// ClusterOptions configures one cluster-scoped dump. Unlike Options there is no Namespace (the
// capture spans the whole cluster and its snapshot belongs to none) and no ExcludeSecretData
// (cluster-scoped kinds carry no Secret data).
type ClusterOptions struct {
	ClusterID  string
	BackupName string
}

// Dump enumerates the cluster's cluster-scoped resources, keeps what the Selector selects,
// sanitizes each, writes the tree under destDir and returns the index. destDir is created if
// missing. It is the mirror of Dumper.Dump and carries the same guarantee class: a single pass,
// NOT transactionally consistent across kinds (Kubernetes offers no cluster-wide freeze), so the
// honest thing is to state the limit rather than imply otherwise by silence.
func (d *ClusterDumper) Dump(ctx context.Context, opts ClusterOptions, destDir string) (*Index, error) {
	if d.Sanitizer == nil {
		return nil, fmt.Errorf("cluster dump: no sanitizer")
	}
	if d.Selector == nil {
		return nil, fmt.Errorf("cluster dump: no selector")
	}
	if err := os.MkdirAll(destDir, 0o750); err != nil {
		return nil, fmt.Errorf("cluster dump: create %s: %w", destDir, err)
	}

	idx := &Index{
		FormatVersion:  IndexFormatVersion,
		ClusterID:      opts.ClusterID,
		BackupName:     opts.BackupName,
		CapturedAt:     d.now().UTC().Format(time.RFC3339),
		RulesetVersion: d.Sanitizer.RulesetVersion(),
		// Namespace is deliberately left empty: a cluster-manifests snapshot belongs to no
		// namespace (adr/0011), which is exactly what its restic identity records by carrying no
		// namespace tag (restic.ClusterManifestsIdentity).
	}

	if v, err := d.Disco.ServerVersion(); err != nil {
		idx.Warnings = append(idx.Warnings, Warning{Message: "could not read the server version: " + err.Error()})
	} else {
		idx.KubernetesVersion = v.GitVersion
	}

	resources, warnings := d.enumerate()
	idx.Warnings = append(idx.Warnings, warnings...)

	for _, gvr := range resources {
		entries, warns := d.dumpResource(ctx, gvr, destDir)
		idx.Resources = append(idx.Resources, entries...)
		idx.Warnings = append(idx.Warnings, warns...)
	}

	raw, err := idx.Marshal()
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(destDir, IndexFileName), raw, 0o600); err != nil {
		return nil, fmt.Errorf("cluster dump: write index: %w", err)
	}
	return idx, nil
}

func (d *ClusterDumper) now() time.Time {
	if d.Clock == nil {
		return time.Now()
	}
	return d.Clock.Now()
}

func (d *ClusterDumper) pageSize() int64 {
	if d.PageSize > 0 {
		return d.PageSize
	}
	return DefaultPageSize
}

// enumerate returns the cluster-scoped, listable resources this run captures, using each group's
// PREFERRED version only (the mirror of Dumper.enumerate). Two filters run here, cheapest first:
// keepClusterResource drops namespaced kinds, subresources and non-listable ones, then
// Selector.SelectsKind drops any kind the run does not want BEFORE it is ever listed — so an
// unwanted kind costs nothing, not one List call and certainly not a full enumeration whose
// objects are all discarded. The captured apiVersion is stored verbatim and never converted, for
// the same reason as the namespaced dump: the target API server owns conversion, not this mover.
func (d *ClusterDumper) enumerate() ([]schema.GroupVersionResource, []Warning) {
	var warnings []Warning

	lists, err := d.Disco.ServerPreferredResources()
	if err != nil {
		// Partial discovery is normal and survivable: one broken aggregated API (a metrics-server
		// that is down, a webhook that times out) must not cost the whole cluster-manifests
		// capture. Whatever discovery did return is still used. A TOTAL failure (nil lists) is a
		// hard error — there is nothing to capture and nothing to be honest about but the failure.
		if lists == nil {
			return nil, []Warning{{Message: "resource discovery failed entirely: " + err.Error()}}
		}
		warnings = append(warnings, Warning{Message: "partial resource discovery: " + err.Error()})
	}

	var out []schema.GroupVersionResource
	for _, list := range lists {
		if list == nil {
			continue
		}
		gv, parseErr := schema.ParseGroupVersion(list.GroupVersion)
		if parseErr != nil {
			warnings = append(warnings, Warning{
				Message: fmt.Sprintf("unparsable groupVersion %q: %v", list.GroupVersion, parseErr),
			})
			continue
		}
		for _, r := range list.APIResources {
			if !keepClusterResource(r) {
				continue
			}
			// The KIND-level selection runs here so an unlisted kind is skipped before any API
			// call. The per-OBJECT narrowing (system: exclusions, the run's include/exclude) is
			// applied in dumpResource, where each object's name is known.
			if !d.Selector.SelectsKind(gv.Group, r.Kind) {
				continue
			}
			out = append(out, gv.WithResource(r.Name))
		}
	}

	// Deterministic order so two dumps of an unchanged cluster agree byte for byte, and so a diff
	// between two runs is readable.
	slices.SortFunc(out, func(a, b schema.GroupVersionResource) int {
		if c := strings.Compare(a.Group, b.Group); c != 0 {
			return c
		}
		return strings.Compare(a.Resource, b.Resource)
	})
	return out, warnings
}

// keepClusterResource is the cluster-scoped mirror of keepResource: it keeps the CLUSTER-scoped
// (namespaced == false) resources that are listable, gettable and not subresources. The
// namespaced/cluster split is the whole reason two dumpers exist — keepResource takes
// `namespaced == true`, this takes its complement (adr/0011).
func keepClusterResource(r metav1.APIResource) bool {
	if r.Namespaced {
		return false
	}
	// A name containing "/" is a subresource (nodes/status, ...): not an object, and listing it
	// is meaningless.
	if strings.Contains(r.Name, "/") {
		return false
	}
	return slices.Contains(r.Verbs, "list") && slices.Contains(r.Verbs, "get")
}

// dumpResource captures every SELECTED object of one cluster-scoped resource type. It mirrors
// Dumper.dumpResource, with the namespaced exclusion context replaced by the ClusterSelector's
// per-object decision and no Secret-data stripping (no cluster-scoped kind carries Secret data).
func (d *ClusterDumper) dumpResource(
	ctx context.Context,
	gvr schema.GroupVersionResource,
	destDir string,
) ([]IndexEntry, []Warning) {
	var entries []IndexEntry
	var warnings []Warning

	err := d.eachObject(ctx, gvr, func(obj unstructured.Unstructured) error {
		gvk := obj.GroupVersionKind()
		// SelectsKind already admitted this resource at enumeration; SelectsObject applies the
		// per-object narrowing — the default control-plane (system:) RBAC exclusions, which an
		// explicit include cannot override, and the run's own include/exclude.
		if !d.Selector.SelectsObject(gvk.Group, gvk.Kind, obj.GetName()) {
			return nil
		}
		clean, _, err := d.Sanitizer.Sanitize(&obj)
		if err != nil {
			warnings = append(warnings, Warning{
				Group: gvr.Group, Version: gvr.Version, Kind: obj.GetKind(),
				Message: fmt.Sprintf("sanitize %s: %v", obj.GetName(), err),
			})
			return nil
		}

		body, err := sanitize.Marshal(clean)
		if err != nil {
			warnings = append(warnings, Warning{
				Group: gvr.Group, Version: gvr.Version, Kind: obj.GetKind(),
				Message: fmt.Sprintf("marshal %s: %v", obj.GetName(), err),
			})
			return nil
		}

		rel := StoragePath(gvk.Group, gvk.Kind, obj.GetName())
		full := filepath.Join(destDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			return fmt.Errorf("create %s: %w", filepath.Dir(full), err)
		}
		if err := os.WriteFile(full, body, 0o600); err != nil {
			return fmt.Errorf("write %s: %w", full, err)
		}

		entries = append(entries, IndexEntry{
			Group: gvk.Group, Version: gvk.Version, Kind: gvk.Kind,
			Name: obj.GetName(), Path: rel,
		})
		return nil
	})
	if err != nil {
		// A kind that cannot be listed — RBAC 403, an aggregated API that is down — is a warning,
		// not a failure. Losing one kind is bad; losing the whole cluster-manifests capture
		// because of one kind is worse (mirror of Dumper.dumpResource, §2.1.5).
		warnings = append(warnings, Warning{
			Group: gvr.Group, Version: gvr.Version, Kind: gvr.Resource,
			Message: "list failed: " + err.Error(),
		})
	}
	return entries, warnings
}

// eachObject pages through a CLUSTER-scoped resource, calling fn for every object. Unlike
// Dumper.eachObject it lists WITHOUT a namespace (dynamic .Resource(gvr).List, not
// .Namespace(ns).List): a namespace filter on a cluster-scoped kind matches nothing. Pagination
// is not optional for the same reason as the namespaced dump — an unbounded List of a cluster
// with thousands of PVs or ClusterRoles can exceed the API server's response limit and fail the
// whole kind.
func (d *ClusterDumper) eachObject(
	ctx context.Context,
	gvr schema.GroupVersionResource,
	fn func(unstructured.Unstructured) error,
) error {
	cont := ""
	for {
		list, err := d.Dynamic.Resource(gvr).List(ctx, metav1.ListOptions{
			Limit:    d.pageSize(),
			Continue: cont,
		})
		if err != nil {
			return err
		}
		for i := range list.Items {
			if err := fn(list.Items[i]); err != nil {
				return err
			}
		}
		cont = list.GetContinue()
		if cont == "" {
			return nil
		}
	}
}
