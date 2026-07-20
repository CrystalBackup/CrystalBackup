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

// Package manifests enumerates, sanitizes and lays out a namespace's Kubernetes resources as
// the file tree of a kind=manifests restic snapshot (R15, spec/04-manifest-backup.md).
//
// It never hardcodes a kind list: what a namespace contains is discovered from the API server
// every run, so a tenant's custom resources come along without this repo knowing about them.
package manifests

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// IndexFormatVersion is the schema version of index.json. A restore reads it before anything
// else, so an incompatible layout change must bump it rather than reinterpret old files.
const IndexFormatVersion = 1

// IndexFileName is the manifest inventory, at the root of the snapshot's path.
const IndexFileName = "index.json"

// IndexEntry identifies one captured object.
type IndexEntry struct {
	Group   string `json:"group"`
	Version string `json:"version"`
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	// Path is where the manifest sits relative to the snapshot root, so a restore does not
	// have to re-derive the layout convention from the group and kind.
	Path string `json:"path"`
}

// Warning records a kind that could not be enumerated. The dump continues past these: a
// namespace missing one aggregated API is still worth capturing, but a restore must be able
// to tell "this namespace had no CronJobs" from "we could not read the CronJobs", which is
// the whole reason these are recorded rather than logged and forgotten.
type Warning struct {
	Group   string `json:"group,omitempty"`
	Version string `json:"version,omitempty"`
	Kind    string `json:"kind,omitempty"`
	Message string `json:"message"`
}

// Index is the deterministic inventory written to index.json.
type Index struct {
	FormatVersion int    `json:"formatVersion"`
	ClusterID     string `json:"clusterID"`
	Namespace     string `json:"namespace"`
	BackupName    string `json:"backupName"`
	// KubernetesVersion of the source cluster, for diagnosing a restore into a distant version.
	KubernetesVersion string `json:"kubernetesVersion,omitempty"`
	CapturedAt        string `json:"capturedAt"`
	// RulesetVersion pins which sanitization rules produced these manifests (adr/0007).
	RulesetVersion string `json:"rulesetVersion"`
	// SecretDataExcluded records that manifestOptions.excludeSecretData was in force, so a
	// restore can explain empty Secrets instead of looking like it lost them.
	SecretDataExcluded bool         `json:"secretDataExcluded,omitempty"`
	ResourceCount      int          `json:"resourceCount"`
	Resources          []IndexEntry `json:"resources"`
	Warnings           []Warning    `json:"warnings,omitempty"`
}

// sortStable orders the index so an unchanged namespace serialises to identical bytes on
// every run. Without this the index alone would defeat dedup on a snapshot whose manifests
// are otherwise unchanged (R13).
func (idx *Index) sortStable() {
	slices.SortFunc(idx.Resources, func(a, b IndexEntry) int {
		return strings.Compare(a.Path, b.Path)
	})
	slices.SortFunc(idx.Warnings, func(a, b Warning) int {
		if c := strings.Compare(a.Group, b.Group); c != 0 {
			return c
		}
		if c := strings.Compare(a.Kind, b.Kind); c != 0 {
			return c
		}
		return strings.Compare(a.Message, b.Message)
	})
}

// Marshal renders index.json: sorted, indented, newline-terminated.
func (idx *Index) Marshal() ([]byte, error) {
	idx.sortStable()
	idx.ResourceCount = len(idx.Resources)
	b, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal index: %w", err)
	}
	return append(b, '\n'), nil
}

// ParseIndex reads an index.json written by Marshal.
func ParseIndex(b []byte) (*Index, error) {
	var idx Index
	if err := json.Unmarshal(b, &idx); err != nil {
		return nil, fmt.Errorf("parse index: %w", err)
	}
	if idx.FormatVersion != IndexFormatVersion {
		return nil, fmt.Errorf("unsupported manifest index formatVersion %d (this build reads %d)",
			idx.FormatVersion, IndexFormatVersion)
	}
	return &idx, nil
}

// StoragePath is where a manifest lives relative to the snapshot root:
// <group>/<Kind>/<name>.yaml, with the legacy core group written as "core"
// (spec/04-manifest-backup.md §3). Object names are DNS-safe, so they are valid file names.
func StoragePath(group, kind, name string) string {
	if group == "" {
		group = CoreGroupDir
	}
	return strings.Join([]string{group, kind, name + ".yaml"}, "/")
}

// CoreGroupDir is the directory standing in for the unnamed core API group, which has no
// name to use as a path segment.
const CoreGroupDir = "core"
