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
	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
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

// TestExposureLabelsNameTheOwnerOnBothPlanesAndNeverStampAnEmptyValue is the root-cause guard for
// the 0.6.5 leak, one level below the three collectors that went blind on it.
//
// exposureLabels is the single builder for the identity every exposure object carries, and it used
// to key that identity on the ClusterBackup run — a coordinate the namespace plane does not have. It
// therefore stamped `crystalbackup.io/cluster-backup: ""` there, and two things followed:
//
//   - a selector built from the map became `cluster-backup=`, which matches the objects whose value
//     is literally "" rather than any object at all, so the terminal sweep's verification read, the
//     orphan reaper and the exposer's crash-window reclaim were each keyed on a value that pins
//     nothing;
//   - and the objects did not even agree on carrying it, because the origin content's handover patch
//     goes through a label MERGE that skips an empty desired value (pinned in
//     internal/exposer/cleanup_test.go). The cluster-scoped content — the expensive one — was the one
//     that lacked it.
//
// So the two properties asserted here are the fix, and every reader depends on both: an owner NAME
// on both planes, and no empty values at all.
func TestExposureLabelsNameTheOwnerOnBothPlanesAndNeverStampAnEmptyValue(t *testing.T) {
	planes := []struct {
		what    string
		backup  *cbv1.Backup
		wantRun string
	}{
		{
			what: "namespace plane: no ClusterBackup parent, so no run to name",
			backup: &cbv1.Backup{ObjectMeta: metav1.ObjectMeta{
				Namespace: "m5-tenant", Name: "m5-np-run-tjip7a",
				Labels: map[string]string{apiconst.LabelOrigin: apiconst.OriginNamespace},
			}},
			wantRun: "",
		},
		{
			what: "cluster plane: a fan-out child, whose name IS the run",
			backup: &cbv1.Backup{ObjectMeta: metav1.ObjectMeta{
				Namespace: "c-db", Name: "nightly-20260803",
				Labels: map[string]string{
					apiconst.LabelOrigin:        apiconst.OriginCluster,
					apiconst.LabelClusterBackup: "nightly-20260803",
				},
			}},
			wantRun: "nightly-20260803",
		},
	}

	for _, p := range planes {
		t.Run(p.what, func(t *testing.T) {
			got := exposureLabels(p.backup, "tenant-data")

			for key, value := range got {
				if value == "" {
					t.Errorf("label %q was stamped with an empty value.\n"+
						"An empty value is not a wildcard in a selector, and a label merge will not even "+
						"write it — which is how one VolumeSnapshotContent survived three collectors and "+
						"failed five leak checks.", key)
				}
			}
			if got[apiconst.LabelBackup] != p.backup.Name {
				t.Errorf("%s = %q, want the owning Backup's name %q — it is the only owner coordinate "+
					"that exists on both planes",
					apiconst.LabelBackup, got[apiconst.LabelBackup], p.backup.Name)
			}
			if got[apiconst.LabelNamespace] != p.backup.Namespace {
				t.Errorf("%s = %q, want %q", apiconst.LabelNamespace, got[apiconst.LabelNamespace], p.backup.Namespace)
			}
			if got[apiconst.LabelPVC] != "tenant-data" {
				t.Errorf("%s = %q, want the source PVC", apiconst.LabelPVC, got[apiconst.LabelPVC])
			}
			// The run key stays for the cluster plane (it is the run-wide coordinate humans and the
			// crucible query by) and is ABSENT — not empty — where there is no run.
			run, present := got[apiconst.LabelClusterBackup]
			if p.wantRun == "" && present {
				t.Errorf("%s is present as %q on the namespace plane; it must be omitted entirely",
					apiconst.LabelClusterBackup, run)
			}
			if p.wantRun != "" && run != p.wantRun {
				t.Errorf("%s = %q, want %q", apiconst.LabelClusterBackup, run, p.wantRun)
			}
		})
	}
}

// TestOwnerBackupNameFromLabelsResolvesEveryShape pins the fallback chain the reaper and the Job
// watch resolve owners through — including the two PRE-UPGRADE shapes, because an upgrade that
// cannot attribute an older object turns residue that was merely leaked into residue that is
// permanent.
func TestOwnerBackupNameFromLabelsResolvesEveryShape(t *testing.T) {
	cases := []struct {
		what   string
		labels map[string]string
		want   string
	}{
		{
			what:   "this version, either plane: the owner name is stamped",
			labels: map[string]string{apiconst.LabelBackup: "m5-np-run-tjip7a"},
			want:   "m5-np-run-tjip7a",
		},
		{
			what:   "pre-upgrade cluster plane: only the run, whose value IS the child Backup's name",
			labels: map[string]string{apiconst.LabelClusterBackup: "nightly-20260803"},
			want:   "nightly-20260803",
		},
		{
			what: "both present: the explicit owner name wins over the run-wide coordinate",
			labels: map[string]string{
				apiconst.LabelBackup:        "nightly-20260803",
				apiconst.LabelClusterBackup: "nightly-20260803",
			},
			want: "nightly-20260803",
		},
		{
			what:   "pre-upgrade namespace plane: no owner name anywhere",
			labels: map[string]string{apiconst.LabelNamespace: "m5-tenant", apiconst.LabelPVC: "tenant-data"},
			want:   "",
		},
		{
			what:   "the empty run value an older version stamped is not an owner name",
			labels: map[string]string{apiconst.LabelClusterBackup: ""},
			want:   "",
		},
	}
	for _, c := range cases {
		if got := ownerBackupNameFromLabels(c.labels); got != c.want {
			t.Errorf("%s: ownerBackupNameFromLabels = %q, want %q", c.what, got, c.want)
		}
	}
}
