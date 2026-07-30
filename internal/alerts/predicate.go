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

package alerts

import (
	"context"
	"fmt"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/metrics"
)

// Breach is one thing a rule would fire about, described the way the alert would describe it: the
// labels of the offending series and the value that crossed the bound. Same shape as an
// Alertmanager alert instance, deliberately — the point of the state predicates is that an
// operator with no Prometheus gets the SAME answer, not a differently-shaped one.
type Breach struct {
	Labels map[string]string
	Value  float64
	// Detail is the one thing a metric cannot carry: which object, by name.
	Detail string
}

// Predicate answers, from live cluster state, the same question a Rule's Expr asks of Prometheus.
//
// This is the half lot J (the exportable self-check) consumes: an operator who has not wired
// Crystal Backup into a monitoring stack — which is most of them on day one — still needs a
// verdict on whether their backups are healthy. Running the PromQL is not an option for them, and
// re-deriving the thresholds in a second place is how the two answers start disagreeing. So a
// predicate reads its bound from the SAME Rule.Threshold the expression was built from.
//
// `now` is a parameter rather than a call to time.Now so the age-based predicates are testable
// without a fake clock, exactly like internal/schedule.
type Predicate func(ctx context.Context, r client.Reader, now time.Time) ([]Breach, error)

// repositoryCheckFailed mirrors `crystalbackup_repository_last_check_success == 0`.
//
// Including the absence semantics, which are the subtle half: a repository that has never been
// checked emits no series (spec §2.4) and therefore does not fire. Reproducing that here means
// skipping repositories with a nil lastCheckTime rather than treating "no result" as a failure —
// get that wrong and the self-check reports every fresh installation as damaged.
func repositoryCheckFailed(ctx context.Context, r client.Reader, _ time.Time) ([]Breach, error) {
	repos := &cbv1.BackupRepositoryList{}
	if err := r.List(ctx, repos); err != nil {
		return nil, fmt.Errorf("list BackupRepositories: %w", err)
	}
	var out []Breach
	for i := range repos.Items {
		repo := &repos.Items[i]
		if repo.Status.LastCheckTime == nil || repo.Status.LastCheckResult == checkResultPassed {
			continue
		}
		out = append(out, Breach{
			Labels: repositoryLabels(repo),
			Value:  0,
			Detail: fmt.Sprintf("BackupRepository %s: last check %s at %s",
				repo.Name, repo.Status.LastCheckResult, repo.Status.LastCheckTime.Format(time.RFC3339)),
		})
	}
	return out, nil
}

// maintenanceStalled mirrors `time() - crystalbackup_repository_last_maintenance_timestamp_seconds
// > <Age>`, and is the worked example of why Threshold is a field: the bound below is read out of
// the rule table, so moving 26h moves both answers or neither.
//
// The nil-lastMaintenanceTime skip is again the absence semantics: an Immutable location never
// prunes by design (adr/0005) and emits no series, so it must not be reported here either.
func maintenanceStalled(ctx context.Context, r client.Reader, now time.Time) ([]Breach, error) {
	age := thresholdOf("CrystalbackupMaintenanceStalled").Age
	repos := &cbv1.BackupRepositoryList{}
	if err := r.List(ctx, repos); err != nil {
		return nil, fmt.Errorf("list BackupRepositories: %w", err)
	}
	var out []Breach
	for i := range repos.Items {
		repo := &repos.Items[i]
		last := repo.Status.LastMaintenanceTime
		if last == nil {
			continue
		}
		elapsed := now.Sub(last.Time)
		if elapsed <= age {
			continue
		}
		out = append(out, Breach{
			Labels: repositoryLabels(repo),
			Value:  elapsed.Seconds(),
			Detail: fmt.Sprintf("BackupRepository %s: last successful prune %s ago (bound %s)",
				repo.Name, elapsed.Truncate(time.Minute), age),
		})
	}
	return out, nil
}

// checkResultPassed is the one BackupRepository status value that means the repository verified
// clean; anything else with a lastCheckTime set is a failure.
const checkResultPassed = "Passed"

// repositoryLabels rebuilds the §2.4 label identity of a repository series. `cluster` is left
// empty: resolving it means looking the location up among the ClusterBackupLocations, which is the
// known M6 resolution gap spec §1 documents — filling it in here with a guess would put a WRONG
// value on a self-check report, which is worse than an empty one.
func repositoryLabels(repo *cbv1.BackupRepository) map[string]string {
	location := repo.Status.Location.Name
	if location == "" {
		location = repo.Name
	}
	return map[string]string{
		labelLocation:  location,
		labelScope:     metrics.ScopeLabelValue(repo.Status.Scope),
		labelNamespace: repo.Status.OwnerNamespace,
		labelCluster:   "",
	}
}

// thresholdOf finds a rule's declared bound by name. It panics on an unknown name because the only
// caller is a predicate in this package naming its own rule: a typo is a programming error, and
// rules_test.go exercises every predicate, so it cannot reach a release.
func thresholdOf(name string) Threshold {
	for _, r := range Rules() {
		if r.Name == name {
			return r.Threshold
		}
	}
	panic("alerts: no rule named " + name)
}
