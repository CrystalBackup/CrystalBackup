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

package restic

import (
	"slices"
	"strconv"
	"strings"
)

// PVC-meta tag keys (spec/02-api.md §Repository layout, since 0.2 — adr/0016). Stamped on
// kind=data snapshots ONLY, they are informational and additive: they let ClusterRestore
// recreate a PVC (size, class, access modes) from the repository alone when the source
// namespace — and its Backup CRs — no longer exist. They are deliberately NOT part of the
// snapshot's identity: discovery grouping, retention grouping and the tenancy filters never
// read them, so their absence (every pre-0.2 snapshot) degrades to a documented fallback,
// never to a mis-scoped operation.
const (
	// TagKeyPVCSize carries the PVC's REQUESTED capacity in bytes (decimal integer) — the
	// spec.resources.requests[storage] of the source claim, not the snapshot's data size.
	TagKeyPVCSize = "pvcsize"
	// TagKeyPVCClass carries the source PVC's storageClassName; the tag is omitted entirely
	// when the claim had none (never emitted with an empty value).
	TagKeyPVCClass = "pvcclass"
	// TagKeyPVCModes carries the claim's access modes as sorted, "+"-joined abbreviations
	// (e.g. "RWO" or "RWO+RWX"). "+" is the joiner because restic's --tag flag treats a
	// COMMA inside one flag value as a tag separator — a comma-joined value would silently
	// split into bogus tags.
	TagKeyPVCModes = "pvcmodes"
)

// accessModeAbbrevs maps the Kubernetes PersistentVolumeAccessMode string values to the
// compact abbreviations stored in the pvcmodes= tag. Taking the mode as a plain string
// (corev1.PersistentVolumeAccessMode is a string type) keeps this package free of any k8s
// dependency beyond the API types it already uses.
// The Kubernetes PersistentVolumeAccessMode string values (spelled locally — this package
// deliberately imports no k8s API package beyond the CRD types it already uses).
const (
	accessModeRWO  = "ReadWriteOnce"
	accessModeROX  = "ReadOnlyMany"
	accessModeRWX  = "ReadWriteMany"
	accessModeRWOP = "ReadWriteOncePod"
)

var accessModeAbbrevs = map[string]string{
	accessModeRWO:  "RWO",
	accessModeROX:  "ROX",
	accessModeRWX:  "RWX",
	accessModeRWOP: "RWOP",
}

// accessModeNames is the exact inverse of accessModeAbbrevs, derived once so encode and
// decode can never drift.
var accessModeNames = func() map[string]string {
	m := make(map[string]string, len(accessModeAbbrevs))
	for name, abbrev := range accessModeAbbrevs {
		m[abbrev] = name
	}
	return m
}()

// pvcModesJoiner joins access-mode abbreviations inside the pvcmodes= tag value. It must
// never be a comma (restic would split the tag); see TagKeyPVCModes.
const pvcModesJoiner = "+"

// PVCMetaTags renders the informational PVC-meta tags for one kind=data snapshot:
// pvcsize=<bytes>, pvcclass=<class> (omitted when class is empty), and pvcmodes=<abbrevs>
// (omitted when no known mode is given). accessModes are the Kubernetes mode names
// ("ReadWriteOnce", ...); unknown names are skipped rather than invented. The abbreviations
// are sorted so the tag value is deterministic regardless of the claim's mode order.
func PVCMetaTags(capacityBytes int64, storageClass string, accessModes []string) []string {
	tags := []string{Tag(TagKeyPVCSize, strconv.FormatInt(capacityBytes, 10))}
	if storageClass != "" {
		tags = append(tags, Tag(TagKeyPVCClass, storageClass))
	}
	var abbrevs []string
	for _, m := range accessModes {
		if a, ok := accessModeAbbrevs[m]; ok {
			abbrevs = append(abbrevs, a)
		}
	}
	if len(abbrevs) > 0 {
		slices.Sort(abbrevs)
		tags = append(tags, Tag(TagKeyPVCModes, strings.Join(abbrevs, pvcModesJoiner)))
	}
	return tags
}

// PVCMeta is the decoded PVC-meta of one kind=data snapshot (ParsePVCMeta). Zero values
// mean "not recorded": CapacityBytes 0 and empty StorageClass/AccessModes on a pre-0.2
// snapshot, for which the caller applies the adr/0016 fallback (logical size rounded up,
// target default class, RWO).
type PVCMeta struct {
	// CapacityBytes is the source claim's requested capacity in bytes (0 when absent).
	CapacityBytes int64
	// StorageClass is the source claim's storageClassName ("" when absent or classless).
	StorageClass string
	// AccessModes are the decoded Kubernetes mode names ("ReadWriteOnce", ...), sorted by
	// their abbreviation; empty when absent.
	AccessModes []string
}

// ParsePVCMeta reads the PVC-meta tags back off a snapshot's tag list. It is best-effort by
// design: a missing tag leaves its field zero, a malformed pvcsize yields 0, and unknown
// mode abbreviations are skipped — the caller treats zeros as "apply the fallback", so a
// corrupt tag can degrade sizing but never fail or mis-scope a restore.
func ParsePVCMeta(tags []string) PVCMeta {
	var meta PVCMeta
	if v, ok := TagValue(tags, TagKeyPVCSize); ok {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			meta.CapacityBytes = n
		}
	}
	if v, ok := TagValue(tags, TagKeyPVCClass); ok {
		meta.StorageClass = v
	}
	if v, ok := TagValue(tags, TagKeyPVCModes); ok {
		for a := range strings.SplitSeq(v, pvcModesJoiner) {
			if name, known := accessModeNames[a]; known {
				meta.AccessModes = append(meta.AccessModes, name)
			}
		}
	}
	return meta
}

// restoreCmd is restic's restore subcommand; flagInclude/flagExclude/flagTarget/flagDelete/
// flagOverwrite are its selection and reconciliation flags (see RestoreArgs).
const (
	restoreCmd    = "restore"
	flagTarget    = "--target"
	flagOverwrite = "--overwrite"
	flagDelete    = "--delete"
	flagInclude   = "--include"
	flagExclude   = "--exclude"
	flagSparse    = "--sparse"
	// flagRetryLock rides out a transient repository lock instead of failing the run outright;
	// retryLockFor is how long. Every command that touches a locked repository uses the same
	// budget, so it is named once rather than repeated at each call site.
	flagRetryLock = "--retry-lock"
	retryLockFor  = "5m"
)

// overwriteAlways is the --overwrite policy both restore modes share: files present in both
// the target and the snapshot are always replaced with the snapshot's content (mtime-based
// skipping would silently keep locally-modified files — wrong for a restore). The modes
// differ ONLY in --delete (spec/02-api.md §Restore selection model).
const overwriteAlways = "always"

// RestoreArgs builds the restic argv (after the mover shim's "--" separator) for one PVC
// restore:
//
//	restore <snapshotID>:<snapshotPath> --target <targetPath> --overwrite always
//	        [--delete] [--include p]... [--exclude p]... --sparse --retry-lock 5m
//
// snapshotID is the server-resolved snapshot (never a user-supplied free string — for a
// cluster-origin restore it comes from a listing filtered by the derived namespace= tag,
// adr/0016 §3), and snapshotPath is the /data/<ns>/<pvc> subtree recorded at backup time, so
// "<id>:<path>" restores exactly that PVC's tree rooted at --target. deleteExtras selects
// the Recreate reconciliation (--delete: files present in the target but absent from the
// snapshot are removed — exact match); without it Overwrite semantics apply (extras kept).
// includes/excludes are the user's file globs (R7 partial restore), forwarded one flag per
// pattern; restic resolves them against the restored subtree. --sparse preserves sparse
// files (R10); no xattr filter is passed so xattrs/ACLs travel (R10); --retry-lock rides out
// a transient repository lock exactly like the backup path.
func RestoreArgs(snapshotID, snapshotPath, targetPath string, deleteExtras bool, includes, excludes []string) []string {
	args := []string{
		restoreCmd, snapshotID + ":" + snapshotPath,
		flagTarget, targetPath,
		flagOverwrite, overwriteAlways,
	}
	if deleteExtras {
		args = append(args, flagDelete)
	}
	for _, p := range includes {
		args = append(args, flagInclude, p)
	}
	for _, p := range excludes {
		args = append(args, flagExclude, p)
	}
	return append(args, flagSparse, flagRetryLock, retryLockFor)
}

// ManifestsRestoreArgs builds the restic argv for the restic half of a manifest restore:
//
//	restore <snapshotID>:/manifests/<sourceNamespace> --target <targetDir> --overwrite always --retry-lock 5m
//
// It takes NO mode and NO include/exclude, and both omissions are deliberate.
//
// spec.mode governs how objects already in the TARGET NAMESPACE are reconciled — server-side
// apply versus delete-then-create — which happens later, against the API server. It says
// nothing about files, so --delete has nothing to reconcile here: the destination is a fresh
// emptyDir in a pod that was created moments ago. Passing --delete would be harmless and
// misleading, implying a file-level semantic the mode does not have.
//
// Selection is likewise not restic's business. resources[] selects by group/Kind/name and by
// LABEL, and a label lives inside the file — so the narrowing must happen after parsing, in
// the applier. Restoring the whole tree and filtering in the apply is not a shortcut; it is
// the only order in which a label selector can be evaluated at all.
func ManifestsRestoreArgs(snapshotID, snapshotPath, targetDir string) []string {
	return []string{
		restoreCmd, snapshotID + ":" + snapshotPath,
		flagTarget, targetDir,
		flagOverwrite, overwriteAlways,
		flagRetryLock, retryLockFor,
	}
}

// SnapshotsFilterArgs is the restic argv for a FILTERED snapshot listing:
//
//	snapshots --json --tag crystalbackup[,<k=v>...]
//
// Extra filter tags are AND-combined with the base marker by joining them into ONE --tag
// value with commas — restic's --tag semantics: comma-joined tags within one flag must ALL
// be present, while repeated --tag flags would OR. This is the server-side mediation
// primitive of adr/0016 §3: a cluster-origin restore lists with
// SnapshotsFilterArgs(Tag(TagKeyNamespace, ns), Tag(TagKeyRun, run)) so the repository
// itself only ever returns snapshots of the CR's own namespace. Tag values are DNS-1123
// derived (namespace, run names) and can never contain a comma, so the joining is safe;
// with no extra tags this degenerates to exactly SnapshotsArgs.
func SnapshotsFilterArgs(filterTags ...string) []string {
	return []string{snapshotsCmd, flagJSON, flagTag, strings.Join(append([]string{TagBase}, filterTags...), ",")}
}
