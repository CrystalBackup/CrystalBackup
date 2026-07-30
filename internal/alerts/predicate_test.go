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
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/metrics"
)

func newFakeClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	if err := cbv1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

func repo(name string, mutate func(*cbv1.BackupRepository)) *cbv1.BackupRepository {
	r := &cbv1.BackupRepository{ObjectMeta: metav1.ObjectMeta{Name: name}}
	r.Status.Location.Name = name
	mutate(r)
	return r
}

// TestRepositoryCheckFailedMirrorsAbsenceSemantics is the test that matters for the predicate
// idea at all. The PromQL cannot fire on a repository that has never been checked, because the
// series does not exist (spec §2.4); a predicate that reported "no result" as a failure would give
// a DIFFERENT answer from the alert for every fresh installation — and a self-check that
// contradicts the alerting is worse than no self-check.
func TestRepositoryCheckFailedMirrorsAbsenceSemantics(t *testing.T) {
	now := metav1.NewTime(time.Now())
	c := newFakeClient(t,
		repo("never-checked", func(*cbv1.BackupRepository) {}),
		repo("passed", func(r *cbv1.BackupRepository) {
			r.Status.LastCheckTime = &now
			r.Status.LastCheckResult = "Passed"
		}),
		repo("damaged", func(r *cbv1.BackupRepository) {
			r.Status.LastCheckTime = &now
			r.Status.LastCheckResult = "Failed"
			r.Status.Scope = "Namespaced"
			r.Status.OwnerNamespace = "team-a"
		}),
	)

	breaches, err := repositoryCheckFailed(context.Background(), c, time.Now())
	if err != nil {
		t.Fatalf("predicate: %v", err)
	}
	if len(breaches) != 1 {
		t.Fatalf("want exactly one breach (only `damaged`), got %d: %+v", len(breaches), breaches)
	}
	got := breaches[0].Labels
	if got["location"] != "damaged" {
		t.Errorf("location = %q, want damaged", got["location"])
	}
	// The trap §2.4 documents: the label carries the MAPPED value, never the API enum.
	if got["scope"] != metrics.ScopeLabelValue("Namespaced") || got["scope"] == "Namespaced" {
		t.Errorf("scope = %q, want the collector's mapped value, not the kubebuilder enum", got["scope"])
	}
	if got["namespace"] != "team-a" {
		t.Errorf("namespace = %q, want team-a", got["namespace"])
	}
}

// TestMaintenanceStalledReadsTheRuleThreshold is the point of the Threshold field: the predicate
// does not know that the bound is 26h, it asks the table. Move the number in one place and both
// answers move.
func TestMaintenanceStalledReadsTheRuleThreshold(t *testing.T) {
	age := mustThreshold(t, ruleMaintenanceStalled).Age
	if age == 0 {
		t.Fatal("CrystalbackupMaintenanceStalled declares no Age threshold")
	}
	now := time.Now()
	inside := metav1.NewTime(now.Add(-age + time.Minute))
	outside := metav1.NewTime(now.Add(-age - time.Minute))

	c := newFakeClient(t,
		repo("immutable-never-prunes", func(*cbv1.BackupRepository) {}),
		repo("fresh", func(r *cbv1.BackupRepository) { r.Status.LastMaintenanceTime = &inside }),
		repo("stalled", func(r *cbv1.BackupRepository) { r.Status.LastMaintenanceTime = &outside }),
	)

	breaches, err := maintenanceStalled(context.Background(), c, now)
	if err != nil {
		t.Fatalf("predicate: %v", err)
	}
	if len(breaches) != 1 || breaches[0].Labels["location"] != "stalled" {
		t.Fatalf("want exactly the `stalled` repository; an Immutable location that never prunes "+
			"emits no series and must not be reported. got %+v", breaches)
	}
}

// TestPredicateLabelsMatchTheMetricLabelSet keeps the two halves reporting the same identity: a
// self-check that labels its findings differently from the alerts cannot be correlated with them.
func TestPredicateLabelsMatchTheMetricLabelSet(t *testing.T) {
	now := metav1.NewTime(time.Now())
	c := newFakeClient(t, repo("r", func(r *cbv1.BackupRepository) {
		r.Status.LastCheckTime = &now
		r.Status.LastCheckResult = "Failed"
	}))
	breaches, err := repositoryCheckFailed(context.Background(), c, time.Now())
	if err != nil || len(breaches) != 1 {
		t.Fatalf("setup: %v / %+v", err, breaches)
	}
	want := metrics.Catalogue()[metrics.NameRepositoryCheckSuccess]
	if len(breaches[0].Labels) != len(want) {
		t.Fatalf("predicate reports labels %v, the series carries %v", breaches[0].Labels, want)
	}
	for _, l := range want {
		if _, ok := breaches[0].Labels[l]; !ok {
			t.Errorf("predicate does not report label %q, which %s carries",
				l, metrics.NameRepositoryCheckSuccess)
		}
	}
}

// TestEveryPredicateIsReachable — a Predicate field that nothing exercises would rot before lot J
// arrives to consume it.
func TestEveryPredicateIsReachable(t *testing.T) {
	c := newFakeClient(t)
	implemented := 0
	for _, r := range Rules() {
		if r.Predicate == nil {
			continue
		}
		implemented++
		if _, err := r.Predicate(context.Background(), c, time.Now()); err != nil {
			t.Errorf("%s: predicate errors on an empty cluster: %v", r.Name, err)
		}
	}
	if implemented == 0 {
		t.Error("no rule has a state predicate; the field exists for lot J and needs at least one " +
			"worked example to keep its shape honest")
	}
}
