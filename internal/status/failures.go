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

package status

import "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"

// DefaultFailureCap is the fallback maximum number of FailureRecord entries kept
// on ClusterBackupStatus.Failures. The list is deliberately bounded: a DR run can
// fan out across hundreds of namespaces, and an unbounded failure list would
// bloat the object (and etcd) without adding signal — the true totals live in the
// numeric counters (NamespacesFailed, PVCsFailed). A handful of sample failures
// is enough to diagnose a systemic problem; the counters carry the accounting.
const DefaultFailureCap = 10

// AppendCappedFailure appends rec to failures unless the list has already reached
// cap, in which case failures is returned unchanged. A non-positive cap means
// "use DefaultFailureCap".
//
// It keeps the FIRST cap records rather than the most recent, on purpose: the
// earliest failures of a run are usually the root cause (later ones are often
// cascades from the same underlying problem), and because the numeric counters —
// not this list — carry the true totals, there is nothing to gain by rotating.
//
// It never trims: if failures somehow already exceeds cap it is returned as-is.
// Following normal append semantics the returned slice may share backing storage
// with the input, so callers must use the return value.
func AppendCappedFailure(failures []v1alpha1.FailureRecord, rec v1alpha1.FailureRecord, cap int) []v1alpha1.FailureRecord {
	if cap <= 0 {
		cap = DefaultFailureCap
	}
	if len(failures) >= cap {
		return failures
	}
	return append(failures, rec)
}
