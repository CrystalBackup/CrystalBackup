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
	"regexp"
	"strconv"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
	"github.com/CrystalBackup/CrystalBackup/internal/keys"
	"github.com/CrystalBackup/CrystalBackup/internal/restic"
)

// ---------------------------------------------------------------------------
// M5 shared helpers — the namespace plane (R3/R5), external sync (R28) and
// right-to-erasure (R21).
//
// Two conventions run through all of it, and both are deliberate.
//
// ONE BUCKET, SEVERAL PREFIXES. The crucible provisions exactly one Hetzner
// Object Storage bucket, and M5 needs THREE distinct repositories that are not
// the shared "dr" one: a sync source, a sync destination, and a user's own
// off-platform store. They are separated by PREFIX (and by clusterID), not by
// bucket. That costs nothing in fidelity — restic's repository identity is the
// full path, and the operator addresses each one exactly as it would address a
// separate bucket — while keeping the terraform footprint (and the bill) as it
// was.
//
// SEPARATION IS BY KEY, NOT BY CREDENTIAL. Hetzner Object Storage credentials
// are PROJECT-wide, so every repository here opens with the same access key no
// matter how many prefixes or buckets it is spread across. That is precisely
// why cross-credential external sync is proven in kind (two SeaweedFS
// identities, the second scoped to the destination bucket alone) and NOT here.
// What the crucible is uniquely able to prove is the other half, the one that
// needs a real object store and real keys: that the destination repository is
// re-encrypted — it opens with its OWN key and NOT with the source's.
// ---------------------------------------------------------------------------

const (
	// m5SyncPrefix is the SECOND bucket prefix, holding external-sync destinations. Distinct from
	// m1S3Prefix so a destination repository can never share a path with the source it copies from,
	// which is the one arrangement `restic copy` cannot survive (it would open one repository twice
	// and contend with its own lock).
	m5SyncPrefix = "crystal-offsite"
	// m5TenantPrefix is the THIRD prefix: a namespace user's own off-platform store. Separate from
	// both so the namespace plane's "their bucket, their key" claim is not quietly resting on the
	// platform's repository path.
	m5TenantPrefix = "crystal-tenant"

	// m5TenantNS is the namespace the M5 namespace-plane specs own outright. NOT one of the seeded
	// tenants: those carry the storage case matrix every other milestone reads, and this one gets a
	// PVC created and destroyed inside a single spec.
	m5TenantNS = "m5-tenant"
	// m5TenantS3Secret holds the S3 credentials IN THE TENANT'S OWN NAMESPACE. A namespaced
	// location reads its credentials from its own namespace and nowhere else — putting them here is
	// not a convenience, it is the tenancy contract.
	m5TenantS3Secret = "m5-tenant-s3"
)

// m5RunID makes this run's repositories unique. Same rule and same reason as m3RunID/m4RunID: the
// bucket is never reset between runs, and M5's specs FORGET and PRUNE — a reused path would meet a
// previous run's snapshots and a previous run's deletions.
var m5RunID = strconv.FormatInt(time.Now().Unix(), 36)

// m5ClusterIDFor returns an isolated repository path segment for one M5 scenario, distinct per
// scenario AND per run so no two of them ever share a blast radius.
func m5ClusterIDFor(scenario string) string { return "m5-" + scenario + "-" + m5RunID }

// ---------------------------------------------------------------------------
// Cluster-plane locations on an arbitrary prefix
// ---------------------------------------------------------------------------

// m5ClusterLocationObject builds a non-default cluster location on an explicit prefix + clusterID.
//
// Discovery is turned OFF explicitly rather than by omission — it defaults to TRUE, so leaving the
// field alone would enable it. These repositories exist to be copied from, copied into and erased;
// a discovery pass projecting Backups out of them would only add unrelated reconciles to read
// through when a spec fails.
func m5ClusterLocationObject(name, clusterID, prefix string, mode cbv1.LocationMode) *cbv1.ClusterBackupLocation {
	return &cbv1.ClusterBackupLocation{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: cbv1.ClusterBackupLocationSpec{
			Mode:      mode,
			ClusterID: clusterID,
			S3: cbv1.S3Spec{
				Endpoint:             os.Getenv("S3_ENDPOINT"),
				Bucket:               os.Getenv("S3_BUCKET"),
				Prefix:               prefix,
				Region:               envOr("S3_REGION", "fsn1"),
				CredentialsSecretRef: cbv1.LocalObjectReference{Name: m1S3SecretName},
				ForcePathStyle:       true,
			},
			Encryption: cbv1.ClusterEncryptionSpec{
				ClusterKEKSecretRef: cbv1.LocalObjectReference{Name: m1KEKSecretName},
			},
			Discovery: cbv1.DiscoverySpec{Enabled: ptr.To(false)},
		},
	}
}

// m5CreateClusterLocation creates a Standard cluster location, waits for its repository to
// initialise and registers its teardown. Returns the live BackupRepository.
//
// Each location gets its OWN wrapped DEK (keys.DEKSecretName is per LOCATION), which is what makes
// two locations under one KEK two genuinely distinct repository keys — the material the
// re-encryption assertions rest on.
func m5CreateClusterLocation(name, clusterID, prefix string) *cbv1.BackupRepository {
	GinkgoHelper()
	Expect(k8s.Create(ctx, m5ClusterLocationObject(name, clusterID, prefix, cbv1.LocationModeStandard))).
		To(Succeed(), "create ClusterBackupLocation %s", name)
	DeferCleanup(func() { m1DeleteLocation(name) })
	return m1WaitRepositoryInitialized(name)
}

// m5RepoURL is the `s3:` spelling of a repository under an arbitrary prefix — the address the
// ORACLE uses.
//
// The operator addresses a sync endpoint as an rclone remote instead (adr/0013 amendment), and the
// difference is exactly the point: same bytes, two addressings. An oracle that could only reach a
// repository the way the operator does would be checking the operator's arithmetic with the
// operator's calculator.
func m5RepoURL(prefix, clusterID string) string {
	return restic.RepoURL(os.Getenv("S3_ENDPOINT"), os.Getenv("S3_BUCKET"), prefix, clusterID)
}

// ---------------------------------------------------------------------------
// Repository oracle, keyed by an explicit password
// ---------------------------------------------------------------------------

// m5ResticSnapshotsOn lists a repository's CrystalBackup snapshots with an explicit password, and
// FAILS the spec if restic could not open it. Use it when the repository is expected to open.
func m5ResticSnapshotsOn(repoURL, password string) []restic.Snapshot {
	GinkgoHelper()
	out, ok := m1ResticRun(repoURL, password, "snapshots", "--json", "--tag", restic.TagBase)
	Expect(ok).To(BeTrue(), "restic could not open %s with the password given: %s", repoURL, out)
	snaps, err := restic.ParseSnapshots(m1JSONArray([]byte(out)))
	Expect(err).NotTo(HaveOccurred(), "parse `restic snapshots --json` output: %s", out)
	return snaps
}

// m5ResticOpens reports whether a password opens a repository at all, returning the log for the
// failure message. `cat config` is the cheapest operation that still requires the master key: it
// decrypts the repository config, so a wrong password fails there and nowhere later.
func m5ResticOpens(repoURL, password string) (string, bool) {
	GinkgoHelper()
	return m1ResticRun(repoURL, password, "cat", "config")
}

// m5ResticKeySlotRow matches one row of `restic key list`: an optional "*" (the key currently in
// use) then the 8-hex short ID. The header and the dashed rule match neither.
var m5ResticKeySlotRow = regexp.MustCompile(`(?m)^\s*\*?[0-9a-f]{8}\s`)

// m5ResticKeySlots counts the KEY SLOTS in a repository.
//
// This is the physical form of adr/0004's amendment. "No operator key slot on a user repository" is
// a claim about the repository itself, not about the operator's code, and restic answers it
// directly: a user repository must hold exactly ONE key. Asserting on the Go source would only
// prove that today's code does not add one; counting the slots proves that none is there.
func m5ResticKeySlots(repoURL, password string) int {
	GinkgoHelper()
	out, ok := m1ResticRun(repoURL, password, "key", "list")
	Expect(ok).To(BeTrue(), "restic key list failed on %s: %s", repoURL, out)
	return len(m5ResticKeySlotRow.FindAllString(out, -1))
}

// m5SnapshotByNamespace returns the snapshots carrying namespace=<ns>.
func m5SnapshotByNamespace(snaps []restic.Snapshot, namespace string) []restic.Snapshot {
	var out []restic.Snapshot
	for _, s := range snaps {
		if ns, ok := restic.TagValue(s.Tags, restic.TagKeyNamespace); ok && ns == namespace {
			out = append(out, s)
		}
	}
	return out
}

// m5SnapshotIDs renders a snapshot set as its IDs, for failure messages that name what was actually
// found rather than only how many.
func m5SnapshotIDs(snaps []restic.Snapshot) []string {
	ids := make([]string, 0, len(snaps))
	for _, s := range snaps {
		id := s.ID
		if s.Original != "" {
			id += "(from " + s.Original + ")"
		}
		ids = append(ids, id)
	}
	return ids
}

// ---------------------------------------------------------------------------
// Runs
// ---------------------------------------------------------------------------

// m5RunManifestBackup runs a MANIFEST-ONLY ClusterBackup over the given namespaces and waits for it
// to reach a terminal phase.
//
// Manifest-only (every PVC excluded, cluster capture off) because the M5 specs are about what
// happens to snapshots AFTER they exist — copying them, re-keying them, erasing them — and a data
// snapshot would add a CSI round trip per volume without changing one assertion. It still produces
// a real, per-namespace, namespace-TAGGED snapshot, which is the only property the selection and
// erasure filters key on. Cluster capture is off on purpose too: a cluster-manifests snapshot
// carries NO namespace tag, and a narrowed sync deliberately leaves those behind, so including one
// would add an untagged snapshot to every count for no gain.
func m5RunManifestBackup(name, locationName string, namespaces ...string) *cbv1.ClusterBackup {
	GinkgoHelper()
	cb := &cbv1.ClusterBackup{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: cbv1.ClusterBackupSpec{
			ClusterBackupRunSpec: cbv1.ClusterBackupRunSpec{
				LocationRef:      cbv1.LocalObjectReference{Name: locationName},
				Namespaces:       cbv1.NamespaceSelector{MatchNames: namespaces},
				ClusterResources: cbv1.ClusterResourceCaptureSpec{Enabled: ptr.To(false)},
				BackupRunSpec: cbv1.BackupRunSpec{
					PVCSelector: cbv1.PVCSelector{Exclude: []string{"*"}},
				},
			},
		},
	}
	Expect(k8s.Create(ctx, cb)).To(Succeed(), "create ClusterBackup %s", name)
	DeferCleanup(func() { _ = k8s.Delete(ctx, cb) })

	run := m1WaitClusterBackupTerminal(name, 15*time.Minute)
	Expect(run.Status.Phase).To(Equal("Completed"),
		"ClusterBackup %s did not complete (phase=%q) — the M5 specs need its snapshots to exist",
		name, run.Status.Phase)
	return run
}

// ---------------------------------------------------------------------------
// Namespace plane
// ---------------------------------------------------------------------------

// m5EnsureTenantS3Secret puts the S3 credentials in the TENANT's namespace.
//
// They are the same project-wide Hetzner credentials the platform uses, because Hetzner has no
// second identity to offer (see the file header) — but they are read from the tenant's namespace,
// which is the part that is under test. A controller that defaulted the credentials namespace to
// the operator's would pass here and ship a tenant's data to whatever bucket the PLATFORM
// credentials reach.
func m5EnsureTenantS3Secret(namespace string) {
	GinkgoHelper()
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: m5TenantS3Secret, Namespace: namespace},
		Type:       corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"AWS_ACCESS_KEY_ID":     []byte(os.Getenv("AWS_ACCESS_KEY_ID")),
			"AWS_SECRET_ACCESS_KEY": []byte(os.Getenv("AWS_SECRET_ACCESS_KEY")),
		},
	}
	if err := k8s.Create(ctx, sec); !apierrors.IsAlreadyExists(err) {
		Expect(err).NotTo(HaveOccurred(), "create tenant S3 Secret %s/%s", namespace, m5TenantS3Secret)
	}
}

// m5CreateUserKeySecret creates the user's OWN restic password Secret in their namespace and
// returns the password, so a spec can later open the repository with it directly.
//
// The password is minted by the TEST, not by the operator: a generated one would also live in the
// user's namespace (adr/0004), but then "only the user's key opens this" would be a claim about a
// value neither side chose. Here the test holds the only other copy.
func m5CreateUserKeySecret(namespace, name string) string {
	GinkgoHelper()
	password := "m5-user-key-" + m5RunID + "-" + name

	// Replaced, not reconciled. A leftover Secret from an aborted run carries a DIFFERENT password
	// (the run ID is in it), and reusing it would have the spec hold one key while the operator used
	// another — which fails as "restic cannot open the repository" and reads as a product bug.
	_ = k8s.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}})
	Eventually(func() error {
		return k8s.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Type:       corev1.SecretTypeOpaque,
			Data:       map[string][]byte{keys.UserPasswordSecretKey: []byte(password)},
		})
	}, time.Minute, 2*time.Second).Should(Succeed(), "create user key Secret %s/%s", namespace, name)
	return password
}

// m5TenantLocationObject builds a namespaced BackupLocation on the tenant prefix, wired to the
// user's own credentials Secret and their own password Secret.
func m5TenantLocationObject(namespace, name, clusterID, passwordSecret string) *cbv1.BackupLocation {
	return &cbv1.BackupLocation{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: cbv1.BackupLocationSpec{
			Mode:      cbv1.LocationModeStandard,
			ClusterID: clusterID,
			S3: cbv1.S3Spec{
				Endpoint:             os.Getenv("S3_ENDPOINT"),
				Bucket:               os.Getenv("S3_BUCKET"),
				Prefix:               m5TenantPrefix,
				Region:               envOr("S3_REGION", "fsn1"),
				CredentialsSecretRef: cbv1.LocalObjectReference{Name: m5TenantS3Secret},
				ForcePathStyle:       true,
			},
			Encryption: cbv1.NamespaceEncryptionSpec{
				RepositoryPasswordSecretRef: &cbv1.LocalObjectReference{Name: passwordSecret},
			},
			Discovery: cbv1.DiscoverySpec{Enabled: ptr.To(false)},
		},
	}
}

// m5CreateTenantLocation creates a namespaced location and waits for its repository to initialise.
func m5CreateTenantLocation(namespace, name, clusterID, passwordSecret string) *cbv1.BackupRepository {
	GinkgoHelper()
	Expect(k8s.Create(ctx, m5TenantLocationObject(namespace, name, clusterID, passwordSecret))).
		To(Succeed(), "create BackupLocation %s/%s", namespace, name)
	DeferCleanup(func() {
		_ = k8s.Delete(ctx, &cbv1.BackupLocation{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		})
	})
	return m5WaitRepositoryReady(m5NamespacedRepositoryName(namespace, name))
}

// m5NamespacedRepositoryName mirrors the controller's naming for a namespaced location's
// cluster-scoped BackupRepository ("<namespace>--<location>").
func m5NamespacedRepositoryName(namespace, location string) string {
	return namespace + "--" + location
}

// m5WaitRepositoryReady waits for a BackupRepository BY NAME to report Initialized.
//
// By name rather than through m1FindRepository: that one scans for a CLUSTER-scoped repository
// behind a location, which a namespaced location's repository is not, and would either find nothing
// or — worse, on a suite that also runs cluster locations — find somebody else's.
func m5WaitRepositoryReady(repoName string) *cbv1.BackupRepository {
	GinkgoHelper()
	var repo cbv1.BackupRepository
	Eventually(func(g Gomega) {
		g.Expect(k8s.Get(ctx, client.ObjectKey{Name: repoName}, &repo)).To(Succeed(),
			"BackupRepository %q does not exist yet", repoName)
		g.Expect(repo.Status.Initialized).To(BeTrue(),
			"BackupRepository %s is not Initialized yet (url=%q)", repoName, repo.Status.RepositoryURL)
	}, 10*time.Minute, 5*time.Second).Should(Succeed())
	return &repo
}

// m5RunNamespaceBackup creates a namespace-plane Backup against the user's own location and waits
// for it to reach a terminal phase, returning it.
//
// No fan-out and no parent: a namespace user's backup is a Backup they created themselves, in their
// own namespace, pointing at their own BackupLocation. spec.run is materialized here for the same
// reason a BackupSchedule stamp materializes it — the intent is copied down at creation, and there
// is no parent object to fall back to.
func m5RunNamespaceBackup(namespace, name, locationName string, timeout time.Duration) *cbv1.Backup {
	GinkgoHelper()
	b := &cbv1.Backup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    map[string]string{apiconst.LabelOrigin: apiconst.OriginNamespace},
		},
		Spec: cbv1.BackupSpec{
			LocationRef: cbv1.LocationReference{Kind: "BackupLocation", Name: locationName},
			Run:         &cbv1.BackupRunSpec{IncludeManifests: ptr.To(true)},
		},
	}
	Expect(k8s.Create(ctx, b)).To(Succeed(), "create Backup %s/%s", namespace, name)
	DeferCleanup(func() { _ = k8s.Delete(ctx, b) })

	var got cbv1.Backup
	Eventually(func(g Gomega) {
		g.Expect(k8s.Get(ctx, client.ObjectKeyFromObject(b), &got)).To(Succeed())
		g.Expect(got.Status.Phase).To(BeElementOf(
			"Completed", "PartiallyCompleted", "PartiallyFailed", "Failed"),
			"Backup %s/%s is still non-terminal (phase=%q)", namespace, name, got.Status.Phase)
	}, timeout, 10*time.Second).Should(Succeed())
	return &got
}

// m5DescribeConditions renders a CR's conditions on one line, for failure messages that say WHY
// rather than only that a phase never arrived.
func m5DescribeConditions(conds []metav1.Condition) string {
	out := ""
	for _, c := range conds {
		out += fmt.Sprintf("[%s=%s %s: %s] ", c.Type, c.Status, c.Reason, c.Message)
	}
	return out
}
