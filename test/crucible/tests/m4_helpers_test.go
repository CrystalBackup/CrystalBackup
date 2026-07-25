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
	"bytes"
	"crypto/rand"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/restic"
)

// ---------------------------------------------------------------------------
// M4 shared helpers — maintenance (prune / check / stale-lock reaping, R17).
//
// Two of the three M4 scenarios are DESTRUCTIVE to the repository they run
// against: one kills a prune mid-flight and leaves a real exclusive lock, the
// other deliberately corrupts a pack object. Neither may touch the shared "dr"
// repository, because a lock nobody clears or a pack that fails `restic check`
// would fail every LATER spec — including M1's — and turn one real finding into
// a cascade nobody can read. They get their own repository (a distinct
// clusterID under the same bucket), which changes nothing about the behaviour
// under test: the exclusive lane, the lock and the pack format are identical.
//
// The one NON-destructive scenario — a prune racing a live backup — runs on the
// shared "dr" repository on purpose. "One shared repository means one
// cluster-wide prune window" (adr/0009) is precisely the property, and testing
// it anywhere else would test something weaker than what production does.
// ---------------------------------------------------------------------------

// m4RunID makes this run's repositories and object names unique. Same rule and same reason as
// m3RunID: the bucket is never reset between runs, so a reused clusterID would meet a previous
// run's snapshots — and, for these specs, a previous run's deliberately corrupted pack.
var m4RunID = strconv.FormatInt(time.Now().Unix(), 36)

// m4DedicatedClusterID returns the isolated repository path segment for one destructive spec.
// Distinct per spec AND per run, so two specs never share a blast radius.
func m4DedicatedClusterID(spec string) string {
	return "m4-" + spec + "-" + m4RunID
}

// m4LocationObject builds a location on its own clusterID with an explicit maintenance block.
// Discovery is OFF: these repositories exist to be pruned and checked, and a discovery pass
// projecting Backups out of them would only add unrelated reconciles to read through.
func m4LocationObject(name, clusterID string, maintenance *cbv1.MaintenanceSpec) *cbv1.ClusterBackupLocation {
	return &cbv1.ClusterBackupLocation{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: cbv1.ClusterBackupLocationSpec{
			Mode:      cbv1.LocationModeStandard,
			ClusterID: clusterID,
			S3: cbv1.S3Spec{
				Endpoint:             os.Getenv("S3_ENDPOINT"),
				Bucket:               os.Getenv("S3_BUCKET"),
				Prefix:               m1S3Prefix,
				Region:               envOr("S3_REGION", "fsn1"),
				CredentialsSecretRef: cbv1.LocalObjectReference{Name: m1S3SecretName},
				ForcePathStyle:       true,
			},
			Encryption: cbv1.ClusterEncryptionSpec{
				ClusterKEKSecretRef: cbv1.LocalObjectReference{Name: m1KEKSecretName},
			},
			Maintenance: maintenance,
		},
	}
}

// m4CreateIsolatedLocation creates a location on its own repository, waits for the repository to
// initialise, and registers its teardown. Returns the live BackupRepository.
func m4CreateIsolatedLocation(name, clusterID string, maintenance *cbv1.MaintenanceSpec) *cbv1.BackupRepository {
	GinkgoHelper()
	loc := m4LocationObject(name, clusterID, maintenance)
	Expect(k8s.Create(ctx, loc)).To(Succeed(), "create isolated ClusterBackupLocation %s", name)
	DeferCleanup(func() { m1DeleteLocation(name) })
	return m1WaitRepositoryInitialized(name)
}

// m4EverySecondCron is a cron that is due on the next reconcile. The maintenance controller's
// baseline is the ATTEMPT history and a fresh repository falls back to its creation time, so a
// once-a-minute schedule fires almost immediately — the crucible cannot wait for 03:00.
const m4EverySecondCron = "* * * * *"

// m4StorageClass is the snapshot-capable class the crucible provisions from. Passing "" here would
// NOT mean "the default class" -- an explicitly empty storageClassName disables dynamic
// provisioning entirely and the claim would never bind.
var m4StorageClass = envOr("CRUCIBLE_STORAGE_CLASS", "ceph-block")

// ---------------------------------------------------------------------------
// Object-store access. The suite had none: m1ResticExec reads the repository
// through restic, which is the right oracle for "what does the repo contain"
// but cannot answer "what if a byte rots underneath it". Case 12 needs to WRITE
// garbage into a pack, so this talks to the bucket directly.
//
// It runs from the TEST PROCESS, not from a Job in the cluster: the host
// already reaches the endpoint (infra_test.go proves it), and going through a
// pod would make a two-line mutation cost an image, a NetworkPolicy label and a
// log scrape.
// ---------------------------------------------------------------------------

// m4S3Client builds a minio client against the crucible's bucket from the same environment
// coordinates the operator is configured with.
func m4S3Client() *minio.Client {
	GinkgoHelper()
	endpoint := os.Getenv("S3_ENDPOINT")
	secure := !strings.HasPrefix(endpoint, "http://")
	endpoint = strings.TrimPrefix(strings.TrimPrefix(endpoint, "https://"), "http://")
	endpoint = strings.TrimRight(endpoint, "/")

	c, err := minio.New(endpoint, &minio.Options{
		Creds: credentials.NewStaticV4(
			os.Getenv("AWS_ACCESS_KEY_ID"), os.Getenv("AWS_SECRET_ACCESS_KEY"), ""),
		Secure:       secure,
		Region:       envOr("S3_REGION", "fsn1"),
		BucketLookup: minio.BucketLookupPath,
	})
	Expect(err).NotTo(HaveOccurred(), "build minio client for %s", os.Getenv("S3_ENDPOINT"))
	return c
}

// m4ObjectKeys lists every object under a repository subtree, e.g. "data/" or "locks/".
func m4ObjectKeys(clusterID, subtree string) []string {
	GinkgoHelper()
	prefix := restic.RepoObjectPrefix(m1S3Prefix, clusterID) + subtree
	var keys []string
	for obj := range m4S3Client().ListObjects(ctx, os.Getenv("S3_BUCKET"),
		minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
		Expect(obj.Err).NotTo(HaveOccurred(), "listing %s", prefix)
		keys = append(keys, obj.Key)
	}
	return keys
}

// m4CorruptOnePack rewrites the middle of one pack object with random bytes, preserving its LENGTH
// and its name.
//
// That is the whole point of the scenario. A truncated or missing object is caught by a structural
// `restic check`, which only verifies that the index and the objects agree about what exists. Silent
// bit-rot — a bucket that hands back the right number of bytes and the wrong bytes — is invisible
// until something actually READS and re-hashes the pack, which is what `--read-data-subset` is for.
// Corrupting the length here would make the test pass against a check that never reads any data.
//
// Returns the key that was damaged, so the spec can name it in its failure message.
func m4CorruptOnePack(clusterID string) string {
	GinkgoHelper()
	c := m4S3Client()
	bucket := os.Getenv("S3_BUCKET")

	keys := m4ObjectKeys(clusterID, "data/")
	Expect(keys).NotTo(BeEmpty(), "repository %s has no pack objects to corrupt", clusterID)
	key := keys[0]

	obj, err := c.GetObject(ctx, bucket, key, minio.GetObjectOptions{})
	Expect(err).NotTo(HaveOccurred(), "get pack %s", key)
	defer func() { _ = obj.Close() }()
	original, err := io.ReadAll(obj)
	Expect(err).NotTo(HaveOccurred(), "read pack %s", key)
	Expect(len(original)).To(BeNumerically(">", 64), "pack %s is too small to corrupt meaningfully", key)

	// Flip a window in the middle: the header and trailer carry structure restic may sanity-check
	// cheaply, and the point is to damage DATA in a way only re-hashing can see.
	damaged := append([]byte(nil), original...)
	window := make([]byte, 32)
	_, err = rand.Read(window)
	Expect(err).NotTo(HaveOccurred(), "generate corruption bytes")
	copy(damaged[len(damaged)/2:], window)
	Expect(damaged).NotTo(Equal(original), "the random window happened to match the original bytes")

	_, err = c.PutObject(ctx, bucket, key, bytes.NewReader(damaged), int64(len(damaged)),
		minio.PutObjectOptions{})
	Expect(err).NotTo(HaveOccurred(), "overwrite pack %s with corrupted bytes", key)
	return key
}

// ---------------------------------------------------------------------------
// Maintenance observation
// ---------------------------------------------------------------------------

// m4WaitMaintenanceRecord waits for a recentMaintenance entry for op with a terminal result and
// returns it. The history is the operator's own account of what it attempted, and unlike the
// lastMaintenanceTime/lastCheckTime fields it records FAILURES too — which is what the two
// destructive specs assert on.
func m4WaitMaintenanceRecord(repoName, op string, timeout time.Duration) cbv1.MaintenanceRecord {
	GinkgoHelper()
	var found cbv1.MaintenanceRecord
	Eventually(func(g Gomega) {
		var repo cbv1.BackupRepository
		g.Expect(k8s.Get(ctx, client.ObjectKey{Name: repoName}, &repo)).To(Succeed())
		for _, rec := range repo.Status.RecentMaintenance {
			if rec.Operation == op && rec.Result != "" {
				found = rec
				return
			}
		}
		g.Expect(false).To(BeTrue(), "no terminal %s record on repository %s yet (history=%d)",
			op, repoName, len(repo.Status.RecentMaintenance))
	}, timeout, 5*time.Second).Should(Succeed())
	return found
}

// m4MaintenanceJobs returns the live maintenance Jobs in the operator namespace. They carry the
// mover family's labels with component=maintenance, which is what distinguishes them from data
// movers — the concurrency semaphore and the orphan reaper both rely on that same distinction.
func m4MaintenanceJobs() []batchv1.Job {
	GinkgoHelper()
	var jobs batchv1.JobList
	Expect(k8s.List(ctx, &jobs, client.InNamespace(operatorNS),
		client.MatchingLabels{"app.kubernetes.io/component": "maintenance"})).To(Succeed())
	var live []batchv1.Job
	for i := range jobs.Items {
		if jobs.Items[i].DeletionTimestamp.IsZero() {
			live = append(live, jobs.Items[i])
		}
	}
	return live
}
