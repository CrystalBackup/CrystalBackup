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
	"context"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clocktesting "k8s.io/utils/clock/testing"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/client/secrets"
	"github.com/CrystalBackup/CrystalBackup/internal/mover"
	"github.com/CrystalBackup/CrystalBackup/internal/repo/s3stat"
)

// countingProber reports a fixed measurement and counts how often it was asked.
type countingProber struct {
	calls atomic.Int32
	stats s3stat.Stats
}

func (p *countingProber) Stat(context.Context, string, time.Time) (s3stat.Stats, error) {
	p.calls.Add(1)
	return p.stats, nil
}

// TestProbeRunsWhileAnOpHoldsTheLane pins the ordering that the crucible's killed-prune spec
// found broken: the footprint probe must run, and its result must reach status, while a
// maintenance op is in flight. A repository wedged by an orphaned exclusive lock is ALWAYS in
// flight — each prune waits out restic's lock retry, fails, and the next one is enqueued within
// the second — so a probe that only runs between ops never measures staleLocks, and the reap
// that would clear the lock never fires.
func TestProbeRunsWhileAnOpHoldsTheLane(t *testing.T) {
	const (
		name     = "wedged"
		opNS     = "crystal-backup-system"
		credsRef = "s3-creds"
	)
	scheme := escrowScheme(t)
	repo := &cbv1.BackupRepository{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status:     cbv1.BackupRepositoryStatus{Initialized: true},
	}
	loc := &cbv1.ClusterBackupLocation{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: cbv1.ClusterBackupLocationSpec{
			ClusterID: "prod-eu-1",
			S3: cbv1.S3Spec{
				Endpoint:             "https://s3.example.com",
				Bucket:               "backups",
				CredentialsSecretRef: cbv1.LocalObjectReference{Name: credsRef},
			},
			Maintenance: &cbv1.MaintenanceSpec{PruneSchedule: "* * * * *"},
		},
	}
	creds := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: credsRef, Namespace: opNS},
		Data: map[string][]byte{
			mover.SecretKeyAWSAccessKeyID:     []byte("ak"),
			mover.SecretKeyAWSSecretAccessKey: []byte("sk"),
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(repo, loc, creds).
		WithStatusSubresource(&cbv1.BackupRepository{}).
		Build()

	prober := &countingProber{stats: s3stat.Stats{SizeBytes: 1024, Objects: 3, StaleLocks: 1}}
	r := NewMaintenanceReconciler(c, scheme, secrets.NewByNameReader(c), nil, opNS, "", mover.Profiles{},
		mover.Placement{}, nil, clocktesting.NewFakePassiveClock(time.Now()))
	r.NewProber = func(cbv1.S3Spec, string, string) (s3stat.Prober, error) { return prober, nil }

	// A prune is on the lane, and stays there for the whole test.
	r.tracker.inFlight[name] = mover.OpPrune

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: name}}
	res, err := r.Reconcile(t.Context(), req)
	if err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	if res.RequeueAfter != maintenanceWatchdogInterval {
		t.Fatalf("with an op in flight the reconcile must keep watching, got RequeueAfter=%v", res.RequeueAfter)
	}

	// The probe runs on its own goroutine; wait for its result to land.
	deadline := time.Now().Add(5 * time.Second)
	for {
		r.tracker.mu.Lock()
		landed := r.tracker.probeResults[name] != nil && !r.tracker.probeInFlight[name]
		r.tracker.mu.Unlock()
		if landed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no probe ran while the prune held the lane (calls=%d)", prober.calls.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}

	if _, err := r.Reconcile(t.Context(), req); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	var live cbv1.BackupRepository
	if err := c.Get(t.Context(), client.ObjectKey{Name: name}, &live); err != nil {
		t.Fatalf("get repository: %v", err)
	}
	if live.Status.StaleLocks != 1 {
		t.Fatalf("staleLocks = %d with a prune in flight; the measurement never reached status, "+
			"so the stale-lock reap can never fire on a wedged repository", live.Status.StaleLocks)
	}
	if _, stillRunning := r.tracker.inFlight[name]; !stillRunning {
		t.Fatalf("the in-flight prune was dropped; the test no longer models a held lane")
	}
}
