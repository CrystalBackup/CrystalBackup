//go:build crucible

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

package crucible

// M4 — crash-only teardown. The leak audit proved the exposure teardown used to be one-shot: the
// reconcile pass that persisted a Backup's terminal status was also the only pass that ever
// deleted its VolumeSnapshot/VolumeSnapshotContent pair, and a process death inside that window
// stranded a Retain-parked, owner-less VSC forever (the fanout's ~1-in-3 residual). The fix is
// re-entry — the terminal short-circuit re-runs an idempotent, derive-only sweep until it has
// verified the teardown and stamped crystalbackup.io/exposures-cleaned.
//
// This spec aims a grace-0 SIGKILL at that exact window ON REAL INFRASTRUCTURE: watch a child
// Backup at high frequency, kill the operator the instant the terminal phase appears, and
// require the fresh operator to converge to zero residuals. Before the sweep existed, a kill
// landing inside the window made this fail; with it, the kill must not matter.

import (
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
)

var _ = Describe("M4 — crash-only teardown (terminal re-entry sweep)", Ordered, Label("m4"), func() {
	// One namespace keeps the timing tight: a single child Backup, one unambiguous terminal
	// transition to aim the kill at. c-db is the seeded ceph-block RWO workload the OOM spec
	// already relies on.
	const targetNS = "c-db"

	// Same budget as the m1 reliability container's runs — a single-namespace run finishes far
	// sooner, but a loaded full-suite cluster deserves the same headroom.
	const clusterBackupTerminalTimeout = 20 * time.Minute

	var (
		run             *cbv1.ClusterBackup
		createdLocation bool
	)

	BeforeAll(func() {
		m1RequireS3()
		m1EnsurePlatformSecrets()

		// Reuse the shared cluster-DR location a sibling feature already created; otherwise
		// create (and own) it — the same convention as the m1 reliability container.
		var loc cbv1.ClusterBackupLocation
		err := k8s.Get(ctx, client.ObjectKey{Name: m1LocationName}, &loc)
		switch {
		case apierrors.IsNotFound(err):
			m1CreateLocation(m1LocationName, true)
			createdLocation = true
		default:
			Expect(err).NotTo(HaveOccurred(), "get ClusterBackupLocation %q", m1LocationName)
		}
		m1WaitRepositoryInitialized(m1LocationName)
	})

	AfterAll(func() {
		if run != nil {
			_ = k8s.Delete(ctx, run)
		}
		if createdLocation {
			m1DeleteLocation(m1LocationName)
		}
	})

	It("The operator SIGKILLed at the terminal transition still converges to zero residual snapshot objects", func() {
		By("Given a single-namespace run whose child Backup is watched at high frequency")
		run = m1RunClusterBackup(fmt.Sprintf("m4td-killterm-%d", time.Now().Unix()), m1LocationName,
			cbv1.NamespaceSelector{MatchNames: []string{targetNS}})

		By("When the operator is SIGKILLed the instant the child Backup goes terminal")
		// The 200ms poll aims the grace-0 kill within a few hundred ms of the terminal status
		// write — inside (or hard against) the [status write → teardown deletes → marker] window.
		// Runs where the inline teardown wins the race are still valid: the invariant below must
		// hold REGARDLESS of where the kill lands; that indifference is the feature under test.
		Eventually(func(g Gomega) {
			var bk cbv1.Backup
			g.Expect(k8s.Get(ctx, client.ObjectKey{Namespace: targetNS, Name: run.Name}, &bk)).To(Succeed())
			g.Expect(bk.Status.Phase).To(BeElementOf("Completed", "PartiallyCompleted", "PartiallyFailed", "Failed"),
				"child Backup %s/%s is not terminal yet", targetNS, run.Name)
		}, clusterBackupTerminalTimeout, 200*time.Millisecond).Should(Succeed())
		killTime := time.Now()
		Expect(k8s.DeleteAllOf(ctx, &corev1.Pod{}, client.InNamespace(operatorNS),
			client.MatchingLabels{"app.kubernetes.io/name": "crystal-backup"},
			client.GracePeriodSeconds(0))).To(Succeed(), "SIGKILL the operator pod(s)")

		By("Then a fresh operator comes up")
		Eventually(func(g Gomega) {
			var pods corev1.PodList
			g.Expect(k8s.List(ctx, &pods, client.InNamespace(operatorNS),
				client.MatchingLabels{"app.kubernetes.io/name": "crystal-backup"})).To(Succeed())
			fresh := false
			for i := range pods.Items {
				p := &pods.Items[i]
				if p.Status.StartTime == nil || p.Status.StartTime.Time.Before(killTime.Add(-time.Second)) {
					continue // the pod we just killed, still terminating
				}
				for _, c := range p.Status.Conditions {
					if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
						fresh = true
					}
				}
			}
			g.Expect(fresh).To(BeTrue(), "no fresh Ready operator pod after the kill")
		}, 3*time.Minute, 5*time.Second).Should(Succeed())

		By("And the terminal re-entry sweep verifies the teardown and stamps exposures-cleaned")
		// The marker is the fix's own receipt: it is only stamped once NOTHING remains — the
		// sweep re-verifies (and re-drives the direct reclaim) while the external
		// snapshot-controller's cascade drains, so under full-suite load the stamp can lag the
		// terminal phase by minutes. 10 min mirrors the leak-check helper's budget, and for the
		// same measured reason.
		Eventually(func(g Gomega) {
			var bk cbv1.Backup
			g.Expect(k8s.Get(ctx, client.ObjectKey{Namespace: targetNS, Name: run.Name}, &bk)).To(Succeed())
			g.Expect(bk.Annotations).To(HaveKeyWithValue(
				apiconst.AnnotationExposuresCleaned, apiconst.AnnotationExposuresCleanedValue),
				"the re-entry sweep never verified the teardown of Backup %s/%s", targetNS, run.Name)
			g.Expect(bk.Status.Phase).To(BeElementOf("Completed", "PartiallyCompleted", "PartiallyFailed", "Failed"),
				"the sweep must never disturb the terminal record")
		}, 10*time.Minute, 5*time.Second).Should(Succeed())

		By("And the leak-check invariant holds: zero residual snapshot objects")
		m1AssertNoResidualSnapshotObjects(targetNS)
	})
})
