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

package controller

import (
	"regexp"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
)

// TestSetCompletionTimeNeverMovesAnExistingStamp is the guard on the one property
// status.completionTime has to have to be usable as a failure clock.
//
// Everything else in this controller re-runs: a conflict retry, a re-list, the already-terminal
// sweep at the top of Reconcile. A stamp rewritten on any of those would creep forward every time
// the object was touched — and the metric derived from it, crystalbackup_backup_last_failure_timestamp_seconds,
// would report a week-old failure as having happened moments ago. The alert reading it over a
// one-hour window would then never clear, which is the failure mode that gets a rule silenced
// permanently rather than fixed.
func TestSetCompletionTimeNeverMovesAnExistingStamp(t *testing.T) {
	first := metav1.NewTime(time.Date(2026, 7, 17, 2, 0, 0, 0, time.UTC))
	b := &cbv1.Backup{Status: cbv1.BackupStatus{CompletionTime: &first}}

	setCompletionTime(b)
	if !b.Status.CompletionTime.Equal(&first) {
		t.Errorf("completionTime moved to %s; a re-reconcile of a terminal Backup must not restate "+
			"when it finished", b.Status.CompletionTime)
	}

	// The other half: an unstamped Backup does get one, or the field would never be written at all.
	fresh := &cbv1.Backup{}
	setCompletionTime(fresh)
	if fresh.Status.CompletionTime == nil {
		t.Fatal("completionTime was not set on a Backup reaching a terminal phase for the first time")
	}
}

// dns1123Label is the Kubernetes label/name shape the derived mover Job name must satisfy.
var dns1123Label = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// TestMoverNamePrefixNamespaceQualified is the regression guard for the cluster-DR name-collision
// defect: a ClusterBackup run fans out one child Backup of the SAME name into every matched
// namespace, and all per-PVC mover/exposure objects share the operator namespace (plus a
// cluster-scoped static VSC). If the derived name omitted the origin namespace, two namespaces
// holding a same-named PVC in one run would collide and — because every Create tolerates
// AlreadyExists — the second would silently adopt the first's Job/exposure (its own PVC never
// backed up, the first's snapshot recorded as its own). moverNamePrefix must therefore be
// injective in the namespace for a fixed (run, pvc).
func TestMoverNamePrefixNamespaceQualified(t *testing.T) {
	const (
		run = "dr-daily-20260719-020000"
		pvc = "data" // the archetypal shared name across tenant namespaces
	)

	// Two distinct namespaces, identical run + pvc: the exact collision the fix closes.
	ns1 := moverNamePrefix("c-team-a", run, pvc)
	ns2 := moverNamePrefix("c-team-b", run, pvc)
	if ns1 == ns2 {
		t.Fatalf("moverNamePrefix collides across namespaces for the same (run, pvc): both %q", ns1)
	}

	// Determinism: same (namespace, run, pvc) must always derive the same name, so a restarted
	// controller re-adopts its own objects rather than orphaning them.
	if got := moverNamePrefix("c-team-a", run, pvc); got != ns1 {
		t.Errorf("moverNamePrefix not deterministic: %q then %q", ns1, got)
	}

	// Length + DNS-1123: the derived Job name (<prefix>-mover) must stay a valid <=63-char label.
	for _, prefix := range []string{ns1, ns2} {
		if len(prefix) > moverNamePrefixMax {
			t.Errorf("prefix %q exceeds moverNamePrefixMax %d", prefix, moverNamePrefixMax)
		}
		if jobName := prefix + "-mover"; !dns1123Label.MatchString(jobName) || len(jobName) > 63 {
			t.Errorf("derived Job name %q is not a valid <=63-char DNS-1123 label", jobName)
		}
	}
}

// TestMoverNamePrefixCollisionFreeUnderTruncation checks that namespace-qualification still
// disambiguates when the raw "<namespace>-<run>-<pvc>" overflows the cap and is truncated: the
// fnv-32a hash sanitizeDNSName appends is taken over the FULL original input (namespace included),
// so two long namespaces that share a truncation prefix still derive distinct names. Without this,
// the fix would leak back for long tenant names.
func TestMoverNamePrefixCollisionFreeUnderTruncation(t *testing.T) {
	const (
		run = "cluster-backup-run-with-a-deliberately-long-name-000001"
		pvc = "postgres-data-primary-volume-claim"
	)
	longA := "c-" + strings.Repeat("tenant-alpha-", 6)
	longB := "c-" + strings.Repeat("tenant-alpha-", 6) + "x" // shares a long common prefix with longA

	a := moverNamePrefix(longA, run, pvc)
	b := moverNamePrefix(longB, run, pvc)
	if len(a) > moverNamePrefixMax || len(b) > moverNamePrefixMax {
		t.Fatalf("truncation did not bound length: %d / %d (max %d)", len(a), len(b), moverNamePrefixMax)
	}
	if a == b {
		t.Fatalf("truncated names collide for distinct long namespaces: both %q", a)
	}
	if !dns1123Label.MatchString(a) || !dns1123Label.MatchString(b) {
		t.Errorf("truncated names are not valid DNS-1123 labels: %q / %q", a, b)
	}
}
