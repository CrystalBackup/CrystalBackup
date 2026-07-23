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
	"reflect"
	"slices"
)

// MaxChangedPaths caps the field paths reported per resource (04-manifest-backup.md §5.4).
// status is a fixed-size list on a status object that a large restore would otherwise grow
// without bound, and the 1 MiB etcd limit is a hard ceiling — a status that cannot be written
// loses the whole report, not just its tail.
const MaxChangedPaths = 20

// changedPaths lists the field paths that differ between the object as it was and as the
// server returned it after an apply. It exists to answer the one question a user has about an
// Overwrite — "what did this actually touch?" — which a bare Configured cannot.
//
// Sorted and capped, so the report is deterministic and bounded.
func changedPaths(before, after map[string]any, limit int) []string {
	var paths []string
	diffMaps(before, after, "", &paths)
	slices.Sort(paths)
	if len(paths) > limit {
		paths = paths[:limit]
	}
	return paths
}

// serverOwnedPaths are fields the API server rewrites on every write regardless of what was
// applied. Reporting them would drown the real diff in noise that means nothing to a user:
// resourceVersion changes on every update by definition, and managedFields changes precisely
// BECAUSE we just applied.
var serverOwnedPaths = map[string]bool{
	"status":                     true,
	"metadata.managedFields":     true,
	"metadata.resourceVersion":   true,
	"metadata.generation":        true,
	"metadata.uid":               true,
	"metadata.creationTimestamp": true,
	"metadata.selfLink":          true,
	"metadata.deletionTimestamp": true,
	"metadata.annotations.kubectl.kubernetes.io/last-applied-configuration": true,
}

func diffMaps(before, after map[string]any, prefix string, out *[]string) {
	// Walk the union of keys: a field the apply ADDED is as much a change as one it altered,
	// and a field it removed likewise.
	seen := make(map[string]bool, len(before)+len(after))
	for k := range before {
		seen[k] = true
	}
	for k := range after {
		seen[k] = true
	}

	for k := range seen {
		p := k
		if prefix != "" {
			p = prefix + "." + k
		}
		if serverOwnedPaths[p] {
			continue
		}
		b, inBefore := before[k]
		a, inAfter := after[k]
		switch {
		case !inBefore || !inAfter:
			*out = append(*out, p)
		default:
			diffValues(b, a, p, out)
		}
	}
}

// diffValues descends into nested maps so a change reports as "data.LOG_LEVEL" rather than
// the whole "data" block. Lists are compared whole: an element-wise path would need the
// server's merge keys to be meaningful, and "spec.ports" is a more honest answer than
// "spec.ports[2].nodePort" derived from a positional guess.
func diffValues(before, after any, path string, out *[]string) {
	bm, bok := before.(map[string]any)
	am, aok := after.(map[string]any)
	if bok && aok {
		diffMaps(bm, am, path, out)
		return
	}
	if !reflect.DeepEqual(before, after) {
		*out = append(*out, path)
	}
}
