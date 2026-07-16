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

import (
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
)

// M1 — the cascade must converge and leave no orphans. This spec implements
// test/crucible/features/m1_reliability.feature 1:1: every run cleans up its
// snapshot objects, the run survives an operator restart via Job re-adoption,
// and a mover killed before it reports (empty termination message) is surfaced
// as a failure rather than a silent success. It drives the SAME live cluster and
// speaks the SAME pinned API/label/phase contract as the rest of the crucible via
// the m1_helpers_test.go vocabulary, so a controller bug shows up here as a failing
// acceptance scenario — these WILL fail (TDD red) until the M1 controllers exist.
var _ = Describe("M1 — convergence and no orphans", Label("m1"), Ordered, func() {

	// clusterBackupTerminalTimeout bounds how long a real run (snapshot + upload of the
	// seeded PVCs to Hetzner object storage) may take before it must reach a terminal phase;
	// the cascade feature fixes this budget at 20 minutes.
	const clusterBackupTerminalTimeout = 20 * time.Minute

	var (
		// createdLocation records whether THIS spec created the shared "dr" location (so it is
		// the one that must delete it). A sibling feature may have created it first, in which
		// case it is reused and left in place.
		createdLocation bool
		// seedNamespaces are the crucible's seeded tenant namespaces (label
		// crystalbackup.io/seed=crucible), discovered at setup time.
		seedNamespaces []string
		// leakRun is the completed run backing the leak-check scenario; restartRun and oomRun
		// are the in-flight runs scenarios 2 and 3 drive themselves. All three are best-effort
		// deleted in AfterAll.
		leakRun    *cbv1.ClusterBackup
		restartRun *cbv1.ClusterBackup
		oomRun     *cbv1.ClusterBackup
	)

	// seedSelector selects the seeded tenant namespaces for a whole-platform run.
	seedSelector := cbv1.NamespaceSelector{MatchLabels: map[string]string{m1SeedLabel: m1SeedValue}}

	// listActiveMoverJobs returns the mover Jobs — a crystalbackup.io/* label, in the operator
	// namespace — that still have an active pod. The operator pod itself carries only
	// app.kubernetes.io/* labels and the restic-oracle Jobs carry none, so this predicate
	// selects exactly the run's data/manifests movers.
	listActiveMoverJobs := func() []batchv1.Job {
		var jobs batchv1.JobList
		Expect(k8s.List(ctx, &jobs, client.InNamespace(operatorNS))).To(Succeed())
		var active []batchv1.Job
		for i := range jobs.Items {
			if m1HasCrystalLabel(jobs.Items[i].Labels) && jobs.Items[i].Status.Active > 0 {
				active = append(active, jobs.Items[i])
			}
		}
		return active
	}

	// listRunningMoverPods returns the currently-Running mover pods (same crystalbackup.io/*
	// label discriminator, in the operator namespace).
	listRunningMoverPods := func() []corev1.Pod {
		var pods corev1.PodList
		Expect(k8s.List(ctx, &pods, client.InNamespace(operatorNS))).To(Succeed())
		var movers []corev1.Pod
		for i := range pods.Items {
			if m1HasCrystalLabel(pods.Items[i].Labels) && pods.Items[i].Status.Phase == corev1.PodRunning {
				movers = append(movers, pods.Items[i])
			}
		}
		return movers
	}

	// childBackupsForRun collects the fanned-out child Backups of run across the seeded
	// namespaces — found by the crystalbackup.io/cluster-backup link label, never by an
	// ownerReference (a namespaced Backup cannot be owned by a cluster-scoped ClusterBackup).
	childBackupsForRun := func(run string) []cbv1.Backup {
		var children []cbv1.Backup
		for _, ns := range seedNamespaces {
			for _, bk := range m1ListBackups(ns) {
				if bk.Labels[apiconst.LabelClusterBackup] == run {
					children = append(children, bk)
				}
			}
		}
		return children
	}

	// looksLikeLockID reports whether s is a restic lock ID (64 lowercase hex chars), so the
	// lock check can ignore any incidental informational line restic prints and flag only a
	// real, stale lock.
	looksLikeLockID := func(s string) bool {
		return len(s) == 64 && strings.Trim(s, "0123456789abcdef") == ""
	}

	BeforeAll(func() {
		m1SkipIfNoS3()
		m1EnsurePlatformSecrets()

		// Ensure the one shared cluster-DR location exists and its repository is initialized.
		// Reuse a location a sibling feature already created; otherwise create (and own) it.
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

		// Discover the seeded tenant namespaces (label crystalbackup.io/seed=crucible).
		var nsList corev1.NamespaceList
		Expect(k8s.List(ctx, &nsList, client.MatchingLabels{m1SeedLabel: m1SeedValue})).To(Succeed())
		for i := range nsList.Items {
			seedNamespaces = append(seedNamespaces, nsList.Items[i].Name)
		}
		Expect(seedNamespaces).NotTo(BeEmpty(),
			"no seeded namespaces (label %s=%s) — is the crucible seeded?", m1SeedLabel, m1SeedValue)

		// Drive one ClusterBackup run to completion across every seeded namespace: the shared,
		// expensive precondition for the leak-check scenario.
		leakRun = m1RunClusterBackup(fmt.Sprintf("m1rel-leak-%d", time.Now().Unix()), m1LocationName, seedSelector)
		m1WaitClusterBackupTerminal(leakRun.Name, clusterBackupTerminalTimeout)
	})

	AfterAll(func() {
		for _, cb := range []*cbv1.ClusterBackup{leakRun, restartRun, oomRun} {
			if cb != nil {
				_ = k8s.Delete(ctx, cb)
			}
		}
		if createdLocation {
			m1DeleteLocation(m1LocationName)
		}
	})

	It("Leak-check — a run leaves zero residual snapshot objects", func() {
		By("Given a completed ClusterBackup run across all seeded namespaces")
		var cb cbv1.ClusterBackup
		Expect(k8s.Get(ctx, client.ObjectKey{Name: leakRun.Name}, &cb)).To(Succeed())
		Expect(cb.Status.Phase).To(BeElementOf("Completed", "PartiallyFailed"),
			"the leak-check precondition run did not complete (phase=%q)", cb.Status.Phase)
		Expect(int(cb.Status.NamespacesMatched)).To(Equal(len(seedNamespaces)),
			"the run should have matched every seeded namespace")

		By("Then there are zero VolumeSnapshots left behind in any tenant namespace")
		By("And zero VolumeSnapshotContents attributable to the run")
		By("And zero temporary clone PVCs created by the exposer")
		// The exposer's ReadyToUse wait + ordered cleanup + the orphan reaper must have removed
		// every VolumeSnapshot, its VolumeSnapshotContent, and every temporary clone PVC.
		m1AssertNoResidualSnapshotObjects(seedNamespaces...)
	})

	It("The operator killed mid-run converges via Job re-adoption", func() {
		By("Given a ClusterBackup run in progress with mover Jobs still running")
		restartRun = m1RunClusterBackup(fmt.Sprintf("m1rel-restart-%d", time.Now().Unix()), m1LocationName, seedSelector)

		moverJobsBefore := map[string]types.UID{}
		Eventually(func(g Gomega) {
			var cb cbv1.ClusterBackup
			g.Expect(k8s.Get(ctx, client.ObjectKey{Name: restartRun.Name}, &cb)).To(Succeed())
			g.Expect(cb.Status.Phase).To(Equal("Running"),
				"ClusterBackup %s is not Running yet (phase=%q)", restartRun.Name, cb.Status.Phase)
			active := listActiveMoverJobs()
			g.Expect(active).NotTo(BeEmpty(), "no mover Job is running yet for run %s", restartRun.Name)
			for _, j := range active {
				moverJobsBefore[j.Name] = j.UID
			}
		}, 10*time.Minute, 2*time.Second).Should(Succeed())

		By("When the operator pod is deleted and then restarts")
		m1DeleteOperatorPod()

		By("Then the operator re-adopts the in-flight mover Jobs instead of orphaning or duplicating them")
		// Re-adoption preserves a Job's identity: a duplicated or orphaned-then-recreated Job
		// would surface as the same name with a NEW UID (delete+recreate). A Job that has since
		// completed and been TTL-collected is simply gone (NotFound) — convergence, not a leak.
		Consistently(func(g Gomega) {
			for name, uid := range moverJobsBefore {
				var j batchv1.Job
				err := k8s.Get(ctx, client.ObjectKey{Namespace: operatorNS, Name: name}, &j)
				if apierrors.IsNotFound(err) {
					continue
				}
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(j.UID).To(Equal(uid),
					"mover Job %s was recreated (new UID) — duplicated/orphaned, not re-adopted", name)
			}
		}, 30*time.Second, 5*time.Second).Should(Succeed())

		By("And the run reaches a terminal phase with no Backup left stuck in a non-terminal phase")
		m1WaitClusterBackupTerminal(restartRun.Name, clusterBackupTerminalTimeout)
		children := childBackupsForRun(restartRun.Name)
		Expect(children).NotTo(BeEmpty(), "run %s fanned out no child Backups", restartRun.Name)
		for _, bk := range children {
			// The cross-namespace parent→child link is a label, never an ownerReference.
			Expect(bk.Labels[apiconst.LabelOrigin]).To(Equal(apiconst.OriginCluster),
				"child Backup %s/%s must carry origin=cluster", bk.Namespace, bk.Name)
			for _, or := range bk.OwnerReferences {
				Expect(or.Kind).NotTo(Equal("ClusterBackup"),
					"child Backup %s/%s must not have an ownerReference to the ClusterBackup", bk.Namespace, bk.Name)
			}
			Expect(bk.Status.Phase).To(BeElementOf("Completed", "PartiallyCompleted", "PartiallyFailed", "Failed"),
				"child Backup %s/%s is stuck in a non-terminal phase (phase=%q)", bk.Namespace, bk.Name, bk.Status.Phase)
		}

		By("And the leak-check invariant still holds afterwards")
		m1AssertNoResidualSnapshotObjects(seedNamespaces...)
	})

	It("An OOMKilled mover is reported as a failure, not a silent success", func() {
		By("Given a mover container that is killed before it writes its termination message")
		// Scope the run to a single snapshottable namespace (c-db, ceph-block RWO) so its lone
		// data mover is unambiguous, then SIGKILL that mover pod (grace 0) before it can write
		// its MoverResult. The kubelet then leaves an EMPTY termination message — exactly the
		// OOMKilled/eviction signal the mover protocol must treat as a crash, never a success.
		const oomNS = "c-db"
		Expect(seedNamespaces).To(ContainElement(oomNS), "seed namespace %q is required for this scenario", oomNS)
		oomRun = m1RunClusterBackup(fmt.Sprintf("m1rel-oom-%d", time.Now().Unix()), m1LocationName,
			cbv1.NamespaceSelector{MatchNames: []string{oomNS}})

		Eventually(func(g Gomega) {
			movers := listRunningMoverPods()
			g.Expect(movers).NotTo(BeEmpty(), "no mover pod is Running yet for run %s", oomRun.Name)
			for i := range movers {
				g.Expect(k8s.Delete(ctx, &movers[i], client.GracePeriodSeconds(0))).To(Succeed(),
					"SIGKILL mover pod %s/%s", movers[i].Namespace, movers[i].Name)
			}
		}, 10*time.Minute, 2*time.Second).Should(Succeed())

		By("Then that volume's status is Failed (an empty termination message is treated as a crash, never as success)")
		Eventually(func(g Gomega) {
			var bk cbv1.Backup
			g.Expect(k8s.Get(ctx, client.ObjectKey{Namespace: oomNS, Name: oomRun.Name}, &bk)).To(Succeed())
			g.Expect(bk.Status.Volumes).NotTo(BeEmpty(), "Backup %s/%s has no volume status yet", oomNS, oomRun.Name)
			failed := false
			for _, v := range bk.Status.Volumes {
				// The crash must never be laundered into a Completed (silent success) volume.
				g.Expect(string(v.Phase)).NotTo(Equal("Completed"),
					"crashed volume %s of Backup %s/%s was reported as a silent success", v.Pvc, oomNS, oomRun.Name)
				if string(v.Phase) == "Failed" {
					failed = true
				}
			}
			g.Expect(failed).To(BeTrue(), "no volume of Backup %s/%s is Failed after the mover crash", oomNS, oomRun.Name)
		}, clusterBackupTerminalTimeout, 10*time.Second).Should(Succeed())

		By("And the repository lock is checked and cleared so the next run is not blocked by a stale lock")
		// Let the run settle, then read the repository's locks with the crucible's independent
		// restic oracle: a crashed mover must not leave a stale exclusive lock behind.
		m1WaitClusterBackupTerminal(oomRun.Name, clusterBackupTerminalTimeout)
		Eventually(func(g Gomega) {
			locks := m1ResticExec(m1LocationName, "list", "locks")
			for _, line := range strings.Split(locks, "\n") {
				g.Expect(looksLikeLockID(strings.TrimSpace(line))).To(BeFalse(),
					"stale restic lock %q remains after the crash", strings.TrimSpace(line))
			}
		}, 5*time.Minute, 15*time.Second).Should(Succeed())
	})
})
