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

	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
	"github.com/CrystalBackup/CrystalBackup/internal/sanitize"
)

// DefaultPageSize bounds one List response. A namespace with tens of thousands of objects
// must not be pulled into memory in one answer, on either side of the connection.
const DefaultPageSize int64 = 500

const (
	kindService     = "Service"
	resourceService = "services"
	kindSecret      = "Secret"
)

// servicesGVR is the core-group Services resource, listed once per dump to classify
// Endpoints for E4.
var servicesGVR = schema.GroupVersionResource{Version: "v1", Resource: resourceService}

// Dumper captures a namespace's resources. Its two clients are deliberately the narrow
// upstream interfaces rather than a controller-runtime client: this runs in the mover, which
// has no scheme, no cache and no business knowing tenant types.
type Dumper struct {
	Disco     discovery.DiscoveryInterface
	Dynamic   dynamic.Interface
	Sanitizer *sanitize.Sanitizer
	// Clock is injectable so capturedAt is deterministic in tests (a real timestamp in a
	// golden file would make it fail a second later).
	Clock clock.PassiveClock
	// PageSize defaults to DefaultPageSize when zero.
	PageSize int64
}

// Options configures one dump.
type Options struct {
	Namespace  string
	ClusterID  string
	BackupName string
	// ExcludeSecretData strips Secret data/stringData and annotates what it did
	// (03-security-and-tenancy.md §10).
	ExcludeSecretData bool
}

// Dump enumerates the namespace, sanitizes what it keeps, writes the tree under destDir and
// returns the index. destDir is created if missing.
//
// The dump is a single pass and is NOT transactionally consistent across kinds — the same
// guarantee class as Velero. Making it consistent would need a cluster-wide freeze that
// Kubernetes does not offer, so the honest thing is to state the limit rather than imply
// otherwise by silence.
func (d *Dumper) Dump(ctx context.Context, opts Options, destDir string) (*Index, error) {
	if d.Sanitizer == nil {
		return nil, fmt.Errorf("dump: no sanitizer")
	}
	if opts.Namespace == "" {
		return nil, fmt.Errorf("dump: no namespace")
	}
	if err := os.MkdirAll(destDir, 0o750); err != nil {
		return nil, fmt.Errorf("dump: create %s: %w", destDir, err)
	}

	idx := &Index{
		FormatVersion:      IndexFormatVersion,
		ClusterID:          opts.ClusterID,
		Namespace:          opts.Namespace,
		BackupName:         opts.BackupName,
		CapturedAt:         d.now().UTC().Format(time.RFC3339),
		RulesetVersion:     d.Sanitizer.RulesetVersion(),
		SecretDataExcluded: opts.ExcludeSecretData,
	}

	if v, err := d.Disco.ServerVersion(); err != nil {
		idx.Warnings = append(idx.Warnings, Warning{Message: "could not read the server version: " + err.Error()})
	} else {
		idx.KubernetesVersion = v.GitVersion
	}

	resources, warnings := d.enumerate()
	idx.Warnings = append(idx.Warnings, warnings...)

	// One pre-pass so E4 can tell a control-plane-managed Endpoints object from a
	// hand-managed one: that decision needs a namespace-wide fact (does the same-named
	// Service have a selector) that no single object carries.
	excludeCtx := sanitize.ExcludeContext{ServicesWithSelector: d.servicesWithSelector(ctx, opts.Namespace, idx)}

	for _, gvr := range resources {
		entries, warns := d.dumpResource(ctx, gvr, opts, excludeCtx, destDir)
		idx.Resources = append(idx.Resources, entries...)
		idx.Warnings = append(idx.Warnings, warns...)
	}

	raw, err := idx.Marshal()
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(destDir, IndexFileName), raw, 0o600); err != nil {
		return nil, fmt.Errorf("dump: write index: %w", err)
	}
	return idx, nil
}

func (d *Dumper) now() time.Time {
	if d.Clock == nil {
		return time.Now()
	}
	return d.Clock.Now()
}

func (d *Dumper) pageSize() int64 {
	if d.PageSize > 0 {
		return d.PageSize
	}
	return DefaultPageSize
}

// enumerate returns the namespaced, listable resources to capture, using each group's
// PREFERRED version only (spec/04-manifest-backup.md §2.1). The captured apiVersion is stored
// verbatim and never converted: the target API server converts if it serves that version,
// and a conversion here would be this operator guessing at semantics it does not own.
func (d *Dumper) enumerate() ([]schema.GroupVersionResource, []Warning) {
	var warnings []Warning

	lists, err := d.Disco.ServerPreferredResources()
	if err != nil {
		// Partial discovery is normal and survivable: one broken aggregated API (a
		// metrics-server that is down, a webhook that times out) must not cost the tenant
		// their whole manifest backup. Whatever discovery did return is still used.
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
			if !keepResource(r) {
				continue
			}
			out = append(out, gv.WithResource(r.Name))
		}
	}

	// Deterministic order so two dumps of an unchanged namespace agree byte for byte, and so
	// a diff between two runs is readable.
	slices.SortFunc(out, func(a, b schema.GroupVersionResource) int {
		if c := strings.Compare(a.Group, b.Group); c != 0 {
			return c
		}
		return strings.Compare(a.Resource, b.Resource)
	})
	return out, warnings
}

// keepResource applies the enumeration filter of §2.1: namespaced, listable and gettable, and
// not a subresource.
func keepResource(r metav1.APIResource) bool {
	if !r.Namespaced {
		return false
	}
	// A name containing "/" is a subresource (pods/log, deployments/scale): not an object,
	// and listing it is meaningless.
	if strings.Contains(r.Name, "/") {
		return false
	}
	return slices.Contains(r.Verbs, "list") && slices.Contains(r.Verbs, "get")
}

// servicesWithSelector collects the Services that have a spec.selector, for E4.
func (d *Dumper) servicesWithSelector(ctx context.Context, namespace string, idx *Index) map[string]bool {
	out := map[string]bool{}
	err := d.eachObject(ctx, servicesGVR, namespace, func(obj unstructured.Unstructured) error {
		sel, found, err := unstructured.NestedMap(obj.Object, "spec", "selector")
		if err == nil && found && len(sel) > 0 {
			out[obj.GetName()] = true
		}
		return nil
	})
	if err != nil {
		// Not fatal, but not silent either: without this list E4 cannot distinguish a
		// control-plane-rebuilt Endpoints from a hand-managed one, so it keeps both. That
		// errs towards capturing too much, which is the safe direction, but the restore
		// should know the classification was degraded.
		idx.Warnings = append(idx.Warnings, Warning{
			Group: "", Kind: kindService,
			Message: "could not list Services to classify Endpoints (E4); hand-managed and " +
				"control-plane-managed Endpoints are both captured: " + err.Error(),
		})
	}
	return out
}

// dumpResource captures every object of one resource type.
func (d *Dumper) dumpResource(
	ctx context.Context,
	gvr schema.GroupVersionResource,
	opts Options,
	excludeCtx sanitize.ExcludeContext,
	destDir string,
) ([]IndexEntry, []Warning) {
	var entries []IndexEntry
	var warnings []Warning

	err := d.eachObject(ctx, gvr, opts.Namespace, func(obj unstructured.Unstructured) error {
		if excluded, _ := sanitize.ShouldExclude(&obj, excludeCtx); excluded {
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
		if opts.ExcludeSecretData {
			stripSecretData(clean)
		}

		body, err := sanitize.Marshal(clean)
		if err != nil {
			warnings = append(warnings, Warning{
				Group: gvr.Group, Version: gvr.Version, Kind: obj.GetKind(),
				Message: fmt.Sprintf("marshal %s: %v", obj.GetName(), err),
			})
			return nil
		}

		gvk := obj.GroupVersionKind()
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
		// A kind that cannot be listed — RBAC 403, an aggregated API that is down — is a
		// warning, not a failure. Losing one kind is bad; losing the whole namespace's
		// manifests because of one kind is worse (§2.1.5).
		warnings = append(warnings, Warning{
			Group: gvr.Group, Version: gvr.Version, Kind: gvr.Resource,
			Message: "list failed: " + err.Error(),
		})
	}
	return entries, warnings
}

// eachObject pages through a resource, calling fn for every object. Pagination is not
// optional: an unbounded List of a large namespace can exceed the API server's response
// limits and fail the whole kind.
func (d *Dumper) eachObject(
	ctx context.Context,
	gvr schema.GroupVersionResource,
	namespace string,
	fn func(unstructured.Unstructured) error,
) error {
	cont := ""
	for {
		list, err := d.Dynamic.Resource(gvr).Namespace(namespace).List(ctx, metav1.ListOptions{
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

// stripSecretData empties a Secret and says so, rather than dropping the object.
//
// Restoring an annotated empty Secret makes a workload that needs the values fail visibly on
// a missing key. Restoring nothing at all would make it fail on a missing Secret, which reads
// like the backup lost it — and dropping the object would also lose the Secret's type,
// labels and the fact that it existed.
func stripSecretData(obj *unstructured.Unstructured) {
	if obj.GetKind() != kindSecret || obj.GroupVersionKind().Group != "" {
		return
	}
	unstructured.RemoveNestedField(obj.Object, "data")
	unstructured.RemoveNestedField(obj.Object, "stringData")
	ann := obj.GetAnnotations()
	if ann == nil {
		ann = map[string]string{}
	}
	ann[apiconst.AnnotationSecretDataExcluded] = "true"
	obj.SetAnnotations(ann)
}
