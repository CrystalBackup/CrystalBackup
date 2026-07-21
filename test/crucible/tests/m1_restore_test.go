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
	"os"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/restic"
)

// ---------------------------------------------------------------------------
// M1 acceptance — off-cluster restore (R8 reversibility, the load-bearing promise).
//
// Disaster recovery must not need the operator, the CRDs, or even a surviving
// cluster: given the S3 credentials and the repository password, a plain upstream
// `restic` must restore the data. This scenario proves exactly that. It backs up a
// seeded tenant (c-db, RWO, whose init-container recorded a MANIFEST.sha256 of its
// data), then in a throwaway Job running ONLY the stock restic image — no operator,
// no CrystalBackup CR consulted — runs `restic restore` straight from the bucket and
// `sha256sum -c` the seed manifest against the restored tree. A green Job means every
// restored byte matches the source: the repository is genuinely readable off-platform.
//
// In M1 the repository password is the platform DEK unwrapped from the KEK (proving
// the restic FORMAT is standard and reversible); reconstructing that DEK from the
// bucket alone (true "no surviving cluster") is the DR-bootstrap escrow work of M2
// (spec/03-security-and-tenancy.md).
// ---------------------------------------------------------------------------

var _ = Describe("M1 — off-cluster restore with upstream restic", Label("m1"), Ordered, func() {
	// restoreNS is c-db: an RWO StatefulSet whose seed init-container writes deterministic data
	// ONCE and records a MANIFEST.sha256 at each volume root, then only sleeps — so the data is
	// immutable and the manifest still matches at backup/restore time.
	const restoreNS = "c-db"
	const restoreRunName = "crucible-restore"

	BeforeAll(func() {
		m1SkipIfNoS3()

		By("Given an initialized ClusterBackupLocation \"dr\" for the shared repository")
		m1EnsurePlatformSecrets()
		var loc cbv1.ClusterBackupLocation
		err := k8s.Get(ctx, client.ObjectKey{Name: m1LocationName}, &loc)
		if apierrors.IsNotFound(err) {
			m1CreateLocation(m1LocationName, true)
		} else {
			Expect(err).NotTo(HaveOccurred(), "get ClusterBackupLocation %s", m1LocationName)
		}
		m1WaitRepositoryInitialized(m1LocationName)

		By("And a completed cluster-DR backup of " + restoreNS)
		var existing cbv1.ClusterBackup
		if apierrors.IsNotFound(k8s.Get(ctx, client.ObjectKey{Name: restoreRunName}, &existing)) {
			m1RunClusterBackup(restoreRunName, m1LocationName,
				cbv1.NamespaceSelector{MatchNames: []string{restoreNS}})
		}
		cb := m1WaitClusterBackupTerminal(restoreRunName, 20*time.Minute)
		Expect(cb.Status.Phase).To(Equal("Completed"),
			"the %s backup must complete before we can restore it (phase=%q)", restoreNS, cb.Status.Phase)
	})

	AfterAll(func() {
		_ = k8s.Delete(ctx, &cbv1.ClusterBackup{ObjectMeta: metav1.ObjectMeta{Name: restoreRunName}})
	})

	It("restores a data snapshot straight from S3 with the restic CLI and byte-verifies it against the seed", func() {
		By("locating the kind=data snapshot for " + restoreNS + " in the shared repository")
		snap := m1DataSnapshot(m1LocationName, restoreNS, restoreRunName)

		By("running `restic restore` in a stock-restic Job (no operator) and checking every file against MANIFEST.sha256")
		ok, log := m1ResticRestoreVerify(m1LocationName, snap)
		Expect(ok).To(BeTrue(),
			"off-cluster `restic restore` + sha256 verification of %s must succeed:\n%s", snap.Paths[0], log)
	})
})

// m1DataSnapshot returns the first kind=data snapshot for (namespace, run) in locationName's shared
// repository, read through the independent restic oracle. Filtering by run tag keeps it from
// matching another scenario's snapshots of the same namespace in the shared repo; a namespace with
// several PVCs yields several — any one carries its own MANIFEST.sha256, so the first is enough.
func m1DataSnapshot(locationName, namespace, run string) restic.Snapshot {
	GinkgoHelper()
	for _, s := range m1ResticSnapshots(locationName) {
		if m1SnapshotHasTag(s, "namespace="+namespace) &&
			m1SnapshotHasTag(s, "kind=data") &&
			m1SnapshotHasTag(s, "run="+run) {
			Expect(s.Paths).To(HaveLen(1), "a CrystalBackup snapshot carries exactly one path")
			return s
		}
	}
	Fail(fmt.Sprintf("no kind=data snapshot for namespace %q in run %q", namespace, run))
	return restic.Snapshot{}
}

// m1SnapshotHasTag reports whether snapshot s carries the exact restic tag.
func m1SnapshotHasTag(s restic.Snapshot, tag string) bool {
	for _, t := range s.Tags {
		if t == tag {
			return true
		}
	}
	return false
}

// m1ResticRestoreVerify runs, in a short-lived Job on the stock restic image (the operator plays no
// part), `restic restore <snap> --target /restore` against the shared repo with the DEK-derived
// password and the S3 credentials, then `sha256sum -c MANIFEST.sha256` on the restored tree. restic
// restores the snapshot's absolute path under --target, so the volume root lands at /restore<path>
// (path == /data/<ns>/<pvc>); the seed wrote MANIFEST.sha256 there with entries relative to it
// (find . -type f), so `sha256sum -c` from that directory verifies every file's content byte-for-
// byte. `set -e` makes a failed restore OR a single mismatching file fail the Job. Returns whether
// the Job succeeded, plus its log.
func m1ResticRestoreVerify(locationName string, snap restic.Snapshot) (bool, string) {
	GinkgoHelper()
	repoURL := restic.RepoURL(os.Getenv("S3_ENDPOINT"), os.Getenv("S3_BUCKET"), m1S3Prefix, m1ClusterID)
	password := m1UnwrapDEK(locationName)
	script := fmt.Sprintf(
		"set -e; restic restore %s --target /restore; cd /restore%s; sha256sum -c MANIFEST.sha256",
		snap.ID, snap.Paths[0])

	backoff, deadline := int32(0), int64(600)
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "crucible-restore-", Namespace: operatorNS},
		Spec: batchv1.JobSpec{
			BackoffLimit:          &backoff,
			ActiveDeadlineSeconds: &deadline,
			Template: corev1.PodTemplateSpec{
				// Reaches object storage like a data mover, so it needs the mover's egress. Under
				// M3's default-deny NetworkPolicy a pod in the operator namespace reaches S3 ONLY if
				// it matches the mover-egress policy (app.kubernetes.io/managed-by=crystal-backup);
				// without this label the Job is default-denied and restic times out dialing S3.
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app.kubernetes.io/managed-by": "crystal-backup"},
				},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{{
						Name:    "restic",
						Image:   m1ResticImage(),
						Command: []string{"sh", "-c"},
						Args:    []string{script},
						Env: []corev1.EnvVar{
							{Name: "RESTIC_REPOSITORY", Value: repoURL},
							{Name: "RESTIC_PASSWORD", Value: password},
							{Name: "AWS_ACCESS_KEY_ID", Value: os.Getenv("AWS_ACCESS_KEY_ID")},
							{Name: "AWS_SECRET_ACCESS_KEY", Value: os.Getenv("AWS_SECRET_ACCESS_KEY")},
							{Name: "AWS_DEFAULT_REGION", Value: envOr("S3_REGION", "fsn1")},
						},
					}},
				},
			},
		},
	}
	Expect(k8s.Create(ctx, job)).To(Succeed(), "create restore Job in %s", operatorNS)
	defer func() {
		_ = k8s.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground))
	}()

	var final batchv1.Job
	Eventually(func(g Gomega) {
		g.Expect(k8s.Get(ctx, client.ObjectKeyFromObject(job), &final)).To(Succeed())
		g.Expect(final.Status.Succeeded+final.Status.Failed).To(BeNumerically(">", 0),
			"restore Job %s/%s has not finished (active=%d)", operatorNS, job.Name, final.Status.Active)
	}, 10*time.Minute, 5*time.Second).Should(Succeed())

	return final.Status.Succeeded > 0, m1PodLogs(job.Name)
}
