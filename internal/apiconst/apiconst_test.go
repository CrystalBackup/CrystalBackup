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

package apiconst

import (
	"testing"
	"time"
)

// TestConstants pins every exported key to its exact spec/02-api.md string. These
// values are a wire/API contract (restic tags, cross-namespace links, RBAC selectors);
// a silent change here would mis-scope tenancy or break discovery, so the literals
// are asserted verbatim rather than derived from the constants they guard.
func TestConstants(t *testing.T) {
	cases := []struct{ name, got, want string }{
		{"Domain", Domain, "crystalbackup.io"},
		{"DefaultOperatorNamespace", DefaultOperatorNamespace, "crystal-backup-system"},

		{"LabelClusterBackup", LabelClusterBackup, "crystalbackup.io/cluster-backup"},
		{"LabelOrigin", LabelOrigin, "crystalbackup.io/origin"},
		{"LabelSchedule", LabelSchedule, "crystalbackup.io/schedule"},
		{"LabelNamespace", LabelNamespace, "crystalbackup.io/namespace"},
		{"LabelTenant", LabelTenant, "crystalbackup.io/tenant"},
		{"LabelProtect", LabelProtect, "crystalbackup.io/protect"},
		{"LabelPVC", LabelPVC, "crystalbackup.io/pvc"},

		{"LabelManagedBy", LabelManagedBy, "app.kubernetes.io/managed-by"},
		{"ManagedByValue", ManagedByValue, "crystal-backup"},

		{"OriginCluster", OriginCluster, "cluster"},
		{"OriginNamespace", OriginNamespace, "namespace"},

		{"AnnotationPreBackupPrefix", AnnotationPreBackupPrefix, "crystalbackup.io/pre-backup-"},
		{"AnnotationProjected", AnnotationProjected, "crystalbackup.io/projected"},
		{"AnnotationProjectedValue", AnnotationProjectedValue, "true"},

		{"FinalizerLocation", FinalizerLocation, "crystalbackup.io/location"},
		{"FinalizerRepository", FinalizerRepository, "crystalbackup.io/repository"},
		{"FinalizerBackup", FinalizerBackup, "crystalbackup.io/backup"},

		{"RunTimestampLayout", RunTimestampLayout, "20060102-150405"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
}

// TestRunName checks the run-name contract: "<schedule>-<YYYYMMDD-HHMMSS>" in UTC,
// independent of the input location.
func TestRunName(t *testing.T) {
	// A non-UTC instant to prove the name normalises to UTC.
	loc := time.FixedZone("UTC+2", 2*3600)
	ts := time.Date(2026, 7, 16, 14, 30, 5, 0, loc) // 12:30:05 UTC
	if got, want := RunName("daily", ts), "daily-20260716-123005"; got != want {
		t.Errorf("RunName = %q, want %q", got, want)
	}
	// Empty schedule (manual run) still produces a parseable timestamped name.
	if got, want := RunName("", time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)), "-20260102-030405"; got != want {
		t.Errorf("RunName(empty) = %q, want %q", got, want)
	}
}
