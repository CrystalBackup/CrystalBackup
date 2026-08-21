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
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"filippo.io/age"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/keys"
)

// ---------------------------------------------------------------------------
// M1 shared helpers — the executable vocabulary the m1_*_test.go specs are
// written against. They drive the SAME live cluster as the rest of the
// crucible (the global k8s client + ctx from crucible_suite_test.go) and speak
// the pinned M1 API / restic / keys contract, so a one-line drift here surfaces
// as a failing acceptance scenario rather than silently mis-scoping tenancy.
//
// These live in a _test.go file (like every other crucible source file):
// k8s, ctx, operatorNS and envOr are defined in the suite's _test.go files, and
// a non-test file cannot reference them — it would break `go build`/`go test`.
// ---------------------------------------------------------------------------

const (
	// m1LocationName is the canonical cluster-DR location the m1 features call "dr".
	m1LocationName = "dr"
	// m1ClusterID is the restic --host and the repo path segment for the crucible cluster.
	m1ClusterID = "crucible"
	// m1KEKSecretName holds the platform KEK (an age X25519 identity) in the operator ns.
	m1KEKSecretName = "crucible-cluster-kek"
	// m1S3SecretName holds the DR S3 credentials in the operator ns.
	m1S3SecretName = "crucible-dr-s3"
	// m1S3Prefix is the bucket prefix under which the one shared repo lives (<prefix>/<clusterID>).
	m1S3Prefix = "crystal"
	// m1SeedLabel / m1SeedValue tag the crucible's seeded tenant namespaces (and their PVCs).
	m1SeedValue = "crucible"
)

// m1KEKIdentity is the age KEK identity string ("AGE-SECRET-KEY-1...") minted (or re-read)
// by m1EnsurePlatformSecrets and consumed by m1UnwrapDEK to turn the operator's wrapped DEK
// back into the restic repository password. Suite-level so a spec's Given/When/Then can
// straddle several helper calls.
var m1KEKIdentity string

// m1RequireS3 FAILS the current spec unless every S3 coordinate the M1 data path needs is present
// in the environment (exported by test/crucible/scripts/load-env.sh).
//
// It used to Skip(), and that was the wrong verb. Every spec from m1 to m5 that touches data goes
// through here, so one unset variable turned the entire data path — the reason this suite exists —
// into a block of skips, and a skip reads as a pass in every summary a human actually looks at.
// The suite already lost the m0 operator-readiness check that way for twelve releases.
//
// There is no legitimate run without S3: `mise run up` provisions the bucket and
// `mise run test` exports its coordinates. An empty variable here means the harness is broken,
// which is a failure, not a reason to measure less.
func m1RequireS3() {
	GinkgoHelper()
	for _, key := range []string{"S3_ENDPOINT", "S3_BUCKET", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY"} {
		Expect(os.Getenv(key)).NotTo(BeEmpty(),
			"S3 not configured: $%s is empty — run via `mise run test` so terraform facts + secrets are loaded", key)
	}
}

// m1EnsurePlatformSecrets idempotently provisions the two cluster-plane Secrets the M1
// controllers read from the operator namespace: the KEK (age identity, data key "identity")
// and the DR S3 credentials (data keys AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY). An
// existing KEK is REUSED verbatim — regenerating it would strand any DEK already wrapped
// under the previous identity — and its identity is stashed in m1KEKIdentity for m1UnwrapDEK.
func m1EnsurePlatformSecrets() {
	GinkgoHelper()

	var kek corev1.Secret
	err := k8s.Get(ctx, client.ObjectKey{Namespace: operatorNS, Name: m1KEKSecretName}, &kek)
	switch {
	case err == nil:
		m1KEKIdentity = strings.TrimSpace(string(kek.Data["identity"]))
		Expect(m1KEKIdentity).NotTo(BeEmpty(),
			"existing KEK Secret %s/%s has no data[\"identity\"]", operatorNS, m1KEKSecretName)
	case apierrors.IsNotFound(err):
		id, gerr := age.GenerateX25519Identity()
		Expect(gerr).NotTo(HaveOccurred(), "generate age KEK identity")
		m1KEKIdentity = id.String()
		Expect(k8s.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: m1KEKSecretName, Namespace: operatorNS},
			Type:       corev1.SecretTypeOpaque,
			Data:       map[string][]byte{"identity": []byte(m1KEKIdentity)},
		})).To(Succeed(), "create KEK Secret %s/%s", operatorNS, m1KEKSecretName)
	default:
		Expect(err).NotTo(HaveOccurred(), "get KEK Secret %s/%s", operatorNS, m1KEKSecretName)
	}

	s3 := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: m1S3SecretName, Namespace: operatorNS},
		Type:       corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"AWS_ACCESS_KEY_ID":     []byte(os.Getenv("AWS_ACCESS_KEY_ID")),
			"AWS_SECRET_ACCESS_KEY": []byte(os.Getenv("AWS_SECRET_ACCESS_KEY")),
		},
	}
	if createErr := k8s.Create(ctx, s3); !apierrors.IsAlreadyExists(createErr) {
		Expect(createErr).NotTo(HaveOccurred(), "create S3 Secret %s/%s", operatorNS, m1S3SecretName)
	}
}

// m1CreateLocation creates a Standard-mode ClusterBackupLocation for the DR bucket (from the
// environment), wired to the platform KEK and S3 Secrets and clusterID m1ClusterID.
func m1LocationObject(name string, isDefault bool) *cbv1.ClusterBackupLocation {
	return &cbv1.ClusterBackupLocation{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: cbv1.ClusterBackupLocationSpec{
			Default:   isDefault,
			Mode:      cbv1.LocationModeStandard,
			ClusterID: m1ClusterID,
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
			// Discover on a tight cadence (the controller's 1-minute floor) instead of the 1h default:
			// projection is nudged promptly by a ClusterBackup completion, but GC AFTER a retention
			// forget only happens on a periodic re-inventory, so the reliability/discovery windows need
			// the repository re-listed within minutes of a forget, not within the hour.
			Discovery: cbv1.DiscoverySpec{
				Enabled:  ptr.To(true),
				Interval: metav1.Duration{Duration: time.Minute},
			},
		},
	}
}

// m1CreateLocation is GONE, deliberately. It was the shape every lane's defensive bootstrap
// reached for — m1CreateLocation(m1LocationName, <flag of your choosing>) — and fifteen
// independent choices of that flag is what made the shared location's configuration a function of
// Ginkgo's shuffle. The shared location is created in exactly one place now
// (m1EnsureSharedLocation); a spec that needs a location of its OWN builds it explicitly, as the
// m4/m5/m6 isolated-repository helpers already do.

// m1TryCreateLocation builds and creates a location, RETURNING the create error (nil on
// success) instead of asserting — for tests that must observe an admission denial (the
// single-default webhook, adr/0010).
func m1TryCreateLocation(name string, isDefault bool) error {
	return k8s.Create(ctx, m1LocationObject(name, isDefault))
}

// m1DeleteLocation best-effort deletes a ClusterBackupLocation (cleanup; ignores NotFound).
//
// NEVER call this on m1LocationName. The shared "dr" location is suite infrastructure, owned by
// BeforeSuite (see m1EnsureSharedLocation) and depended on by roughly twenty containers across
// m1..m6; deleting it garbage-collects the owned BackupRepository through its ownerReference
// (clusterbackuplocation_controller.finalize), so a lane that tears it down leaves every later
// lane waiting on a repository that no longer exists. That is exactly the 0.6.7 failure. Use it
// for the locations a spec creates for ITSELF (dr2, the m4/m5/m6 isolated repositories).
func m1DeleteLocation(name string) {
	_ = k8s.Delete(ctx, &cbv1.ClusterBackupLocation{ObjectMeta: metav1.ObjectMeta{Name: name}})
}

// m1LocationIsDefault is the default flag the shared "dr" location MUST carry, in exactly one
// place, because it used to be decided by a coin flip.
//
// Until 0.6.7, fifteen containers each bootstrapped "dr" defensively — get; if IsNotFound, create
// — and fourteen of them passed true while m1_discovery_test.go passed false. Since only the
// FIRST of them to run actually created anything, and Ginkgo randomises top-level container order
// on every run, the flag the shared location ended up with was decided by the shuffle. Nobody
// noticed because the two requirements are not symmetric:
//
//   - true is REQUIRED. m1_repository_test.go's third scenario asserts spec.default is true on
//     "dr" before creating a conflicting second default, and the whole MultipleDefaults election
//     it exercises is meaningless without it. On the product side, location_binding.defaultClusterID
//     resolves the cluster ID a namespaced BackupLocation inherits from THE default
//     ClusterBackupLocation, and returns "" — "waiting" — when there is none.
//   - false is required by NOTHING. m1_discovery never reads spec.default, never creates a
//     namespaced BackupLocation, and its feature file (features/m1_discovery.feature) asks only
//     for "an initialized ClusterBackupLocation dr whose repository already holds snapshots".
//     The false dates to the original M1 commit and is an oversight, not a requirement.
//
// So there is no conflict to arbitrate: it is true. Rejected: giving m1_discovery its own
// non-default location instead. m1LocationObject hard-codes clusterID and prefix, so a second
// location under those coordinates addresses the SAME repository under another name — the alias
// hazard m5_externalsync_test.go exists to refuse — and discovery's subject is precisely the
// accumulated history of the shared repository, which a private one would not have.
const m1LocationIsDefault = true

// m1EnsureSharedLocation is the ONE way the shared "dr" ClusterBackupLocation comes into being.
//
// It is called from BeforeSuite (crucible_suite_test.go), which is what makes the location a
// suite-level PRECONDITION rather than a side effect of whichever container the shuffle happened
// to run first. In the 0.6.7 campaign m6/s3tuning drew a position ahead of every m1 lane, waited
// its full 600s on a location nobody had created yet, and reported a placement failure on a spec
// that had not yet done anything about placement. It had been winning that coin flip for
// releases; 0.6.6's green was luck, the same shape as the leak-check bug.
//
// It stays idempotent and is still called from every lane's BeforeAll, so `mise run test m4` on
// its own is a legitimate run. That is safe now only because every one of those call sites goes
// through THIS function: a defensive bootstrap that builds its own location object can disagree
// with the suite about the default flag, and for fifteen containers that is what one of them did.
//
// A pre-existing location with the wrong flag is CORRECTED rather than tolerated or failed on: it
// can only come from a previous campaign's binary on a reused cluster (locations are cluster-scoped
// and survive a namespace wipe), and leaving it would reinstate the ambiguity this function exists
// to remove.
//
// It deliberately does NOT wait for the repository to initialise. `restic init` is the slow part,
// and starting it at second zero lets it finish while the infra and m0 lanes run; the waiting stays
// at the call sites, where m1WaitRepositoryInitialized's budget and its failure diagnostics belong
// to the spec that needs the repository.
//
// Creating it HERE is also what makes "dr" the one location that pays the cold start, and 0.6.7
// found that out the expensive way: the first `restic init` cannot run until the mover image has
// been pulled onto a node with an empty cache, and BeforeSuite is the coldest moment there is.
// That is a cost, not a regression — the ordering fix is still right — and it is answered by
// m1ColdRepositoryInitBudget, which explains the 668s that two campaigns died on.
func m1EnsureSharedLocation() {
	GinkgoHelper()
	m1RequireS3()
	m1EnsurePlatformSecrets()

	var loc cbv1.ClusterBackupLocation
	err := k8s.Get(ctx, client.ObjectKey{Name: m1LocationName}, &loc)
	switch {
	case apierrors.IsNotFound(err):
		// AlreadyExists is not a failure: a lane's own BeforeAll may have raced ahead of a
		// re-entry here, and the object it created is the same object this would create.
		createErr := k8s.Create(ctx, m1LocationObject(m1LocationName, m1LocationIsDefault))
		if !apierrors.IsAlreadyExists(createErr) {
			Expect(createErr).NotTo(HaveOccurred(), "create the shared ClusterBackupLocation %s", m1LocationName)
		}
	case err != nil:
		Expect(err).NotTo(HaveOccurred(), "get ClusterBackupLocation %s", m1LocationName)
	case loc.Spec.Default != m1LocationIsDefault:
		Eventually(func() error {
			var live cbv1.ClusterBackupLocation
			if getErr := k8s.Get(ctx, client.ObjectKey{Name: m1LocationName}, &live); getErr != nil {
				return getErr
			}
			if live.Spec.Default == m1LocationIsDefault {
				return nil
			}
			live.Spec.Default = m1LocationIsDefault
			return k8s.Update(ctx, &live)
		}, time.Minute, 3*time.Second).Should(Succeed(),
			"a leftover ClusterBackupLocation %s carries default=%t and could not be corrected to %t",
			m1LocationName, loc.Spec.Default, m1LocationIsDefault)
	}
}

// m1EnsureSharedRepository is the shared Given of every spec that touches the "dr" repository:
// ensure the location (above), then wait for its BackupRepository to report Initialized.
// Idempotent, and cheap once BeforeSuite has done the creating.
func m1EnsureSharedRepository() *cbv1.BackupRepository {
	GinkgoHelper()
	m1EnsureSharedLocation()
	return m1WaitRepositoryInitialized(m1LocationName)
}

// m1FindRepository returns the cluster-scoped BackupRepository backing locationName, if one
// exists yet — first via the location's own status.repositoryRef, then by scanning the
// repositories' status.location back-reference (the controller may set either first).
func m1FindRepository(locationName string) (cbv1.BackupRepository, bool) {
	var loc cbv1.ClusterBackupLocation
	if err := k8s.Get(ctx, client.ObjectKey{Name: locationName}, &loc); err == nil && loc.Status.RepositoryRef != "" {
		var repo cbv1.BackupRepository
		if err := k8s.Get(ctx, client.ObjectKey{Name: loc.Status.RepositoryRef}, &repo); err == nil {
			return repo, true
		}
	}
	var repos cbv1.BackupRepositoryList
	if err := k8s.List(ctx, &repos); err != nil {
		return cbv1.BackupRepository{}, false
	}
	for i := range repos.Items {
		r := repos.Items[i]
		if r.Status.Location.Name == locationName && (r.Status.Scope == "" || r.Status.Scope == "Cluster") {
			return r, true
		}
	}
	return cbv1.BackupRepository{}, false
}

// m1WarmRepositoryInitBudget bounds an init that runs on a cluster where SOME repository has
// already initialised — which means the mover image (restic + rclone) has already been pulled at
// least once, and `restic init` starts as soon as its Job is scheduled.
//
// 10 min, was 5, and the measurement is worth writing down because the previous number lost a whole
// campaign by 462 milliseconds. In the 0.6.6 run this helper's Eventually gave up at 16:05:59.462
// and the BackupRepository's creationTimestamp is 16:05:59 — the same second. 92 of 93 checks had
// passed; the one red check was this precondition, in a [BeforeAll], reported as a placement failure
// on a spec that had not yet done anything about placement.
//
// Why provisioning can genuinely take that long HERE, when it takes seconds at the start of a run:
// the caller (m6_placement_test.go) runs m1EnsurePlatformSecrets() immediately before, so on a
// campaign where an earlier lane removed a platform Secret the operator has to provision from
// nothing — a `restic init` against Hetzner object storage plus the wrapped-DEK escrow write —
// two and a half hours into a full suite, against a cluster that is still finishing other work.
// Five minutes was not a wrong estimate of provisioning; it was an estimate with no headroom left
// for the state the suite is in by then.
//
// This number is UNCHANGED by the 0.6.7 cold-start raise below, deliberately. Every warm caller is
// an isolated repository created by a spec that is already deep into its lane, and the 0.6.7
// campaign measured three of them — m4-killed at 10:08, m4-corrupt at 10:13, m5-erase at 10:28 —
// initialising without trouble in the same run that lost "dr" at second zero. Raising all eighteen
// call sites to the cold budget would buy nothing and would cost the suite the thing 0.6.5 already
// taught it: budgets that everybody spends are how a run reaches the 240m suite timeout with 21 of
// 93 specs unrun (see m1AssertNoResidualSnapshotObjects).
const m1WarmRepositoryInitBudget = 10 * time.Minute

// m1ColdRepositoryInitBudget bounds the FIRST init on a cluster, which pays for the mover image
// pull before `restic init` can run at all.
//
// 20 min, was 10, was 5 — and a THIRD raise would mean this bound is the wrong instrument. Read
// that sentence before changing this number.
//
// The 0.6.7 campaign failed twice, on two independently provisioned clusters, at the same bound.
// The measurement, from the cluster that produced it:
//
//	ClusterBackupLocation dr created   07:29:56Z
//	BackupRepository dr Initialized    07:41:04Z   → 11m08s (668s)
//
// It missed 600s by 68 seconds. In the SAME campaign every LATER repository initialised without
// trouble, and a manual probe on a warm cluster measured a fresh repository at 31 seconds. So the
// 637-second difference is not provisioning and is not the object store: it is a cold-start cost
// paid ONCE per cluster. `restic init` runs in a Job built from the mover image, which carries both
// restic and rclone; on a node with an empty image cache that Job cannot start until the image is
// pulled.
//
// 0.6.7's own change made it worse, and that is not an argument against the change. Moving the
// shared "dr" location into a BeforeSuite precondition (m1EnsureSharedLocation) is correct for
// ordering — it removed a coin flip that had been decided by Ginkgo's shuffle for releases — but it
// moved the first initialisation to the coldest possible moment, the first seconds of the suite,
// before anything has pulled the image. Before that change the first init happened later, after
// some lane had already paid the pull, which is why the cost had never been visible.
//
// Why 20 and not a rounder or a tighter number. 668s is the ONLY measured cold value, so the budget
// is an argument about the variable term, not a scaled guess at the total. Decomposed: 31s of
// actual init, ~637s of image pull. 1200s leaves ~1169s for the pull — 1.8× what was measured —
// which is enough to absorb one kubelet pull retry (ImagePullBackOff is 10s/20s/40s… before a fresh
// attempt) or a registry serving at roughly half the bandwidth. 15 min was considered and rejected:
// 900s leaves 869s, a 1.36× margin over a single sample, and 1.36× is the same knife's edge that
// has now lost two campaigns — 0.6.6 by 462ms at the 5m bound, 0.6.7 by 68s at the 10m bound. The
// cost of the extra ten minutes is bounded and paid at most once per run: only the shared location
// draws this budget (m1RepositoryInitBudgetFor), and only when the repository never initialises.
//
// What a third raise would actually mean, and why the answer would not be a bigger number: it would
// mean either the pull is being paid more than once per cluster (the init Job's placement is
// unconstrained, so a later Job CAN land on a node that never pulled the image), or the pull is not
// the cause at all. Neither is fixed by widening the window. The fixes are to pre-pull the mover
// image at provisioning time, or to wait on the init Job's pod events instead of on a wall clock.
// The helper now MEASURES and reports the elapsed time on every run, so that decision is made from
// a number rather than reconstructed afterwards — which is exactly what 0.6.7 needed and could
// barely do: the events had expired and the operator pod had been recreated by a lane that kills it
// deliberately, so the only surviving evidence was a lastTransitionTime.
const m1ColdRepositoryInitBudget = 20 * time.Minute

// m1RepositoryInitBudgetFor is the rule that decides which of the two budgets a wait gets, in one
// place and by a fact rather than by taste.
//
// The shared "dr" location is the only one that can pay the cold start, and that is provable rather
// than observed: BeforeSuite creates it (m1EnsureSharedLocation) before any spec runs, so its init
// is necessarily the first init on the cluster, and every other location in the suite is created by
// a spec that runs afterwards.
//
// Rejected: deciding at RUN time from "has any repository initialised yet?". It is cheap — the List
// is already there — but as a budget selector it is dangerous in exactly one direction: an init Job
// whose placement lands it on a node that never pulled the image is cold even though another
// repository initialised long ago, and the run-time rule would hand that case the SHORT budget.
// Being generous when wrong costs a slower failure; being stingy when wrong is the bug that lost
// 0.6.7. The observation is still worth having, so it is REPORTED (m1InitializedRepositoryCount)
// and never used to shorten anything.
func m1RepositoryInitBudgetFor(locationName string) time.Duration {
	if locationName == m1LocationName {
		return m1ColdRepositoryInitBudget
	}
	return m1WarmRepositoryInitBudget
}

// m1InitializedRepositoryCount reports how many BackupRepositories already report
// status.initialized==true. Best-effort: it returns -1 rather than turning an unreadable list into
// a spec failure, the same stance m1OperatorRestartSummary takes.
//
// Zero is the one value that carries a real inference, and it is a sound one: a mover pod can only
// run against an initialized repository, so if nothing has initialized here then nothing has ever
// finished pulling the mover image, and this wait includes that pull. A non-zero count is only
// evidence that SOME node has the image, not this Job's node — which is why it is reported and not
// acted on.
func m1InitializedRepositoryCount() int {
	var repos cbv1.BackupRepositoryList
	if err := k8s.List(ctx, &repos); err != nil {
		return -1
	}
	n := 0
	for i := range repos.Items {
		if repos.Items[i].Status.Initialized {
			n++
		}
	}
	return n
}

// m1DescribeInitializedRepositoryCount renders that count for a human, including the "unknown" case.
func m1DescribeInitializedRepositoryCount(n int) string {
	switch {
	case n < 0:
		return "the repository list was unreadable, so it is unknown whether any had initialised"
	case n == 0:
		return "NO repository had yet initialised on this cluster, so this wait includes the one-time mover image pull"
	default:
		return fmt.Sprintf("%d repository(ies) had already initialised on this cluster, so the mover image was "+
			"already pulled onto at least one node (not necessarily this init Job's node)", n)
	}
}

// m1WaitRepositoryInitialized waits for the BackupRepository backing locationName to report
// status.initialized==true, and returns it. The budget is m1RepositoryInitBudgetFor's — 20 min for
// the shared location, which pays the cold start, 10 for every other.
//
// It MEASURES, and that is the point of the helper as much as the waiting is. Every campaign now
// yields the elapsed time to initialisation as a report entry, on success as well as on timeout,
// because 0.6.7 needed that number and had to reconstruct it from a lastTransitionTime after the
// events had expired and the operator pod had been replaced. AddReportEntry is how the rest of this
// suite surfaces a measured fact it does not assert on (m6_fidelity's corpus line,
// m6_s3tuning's per-wave throughput); this is the same channel, not a new one.
//
// This bound exists so a slow answer is not read as a missing one. It is not a claim about how fast
// provisioning should be: if a WARM init ever takes ten minutes there IS a defect, and the failure
// message names the location, the URL and the elapsed time so the next reader starts from the init
// Job and the object store rather than from here.
func m1WaitRepositoryInitialized(locationName string) *cbv1.BackupRepository {
	GinkgoHelper()

	budget := m1RepositoryInitBudgetFor(locationName)
	startedAt := time.Now()
	priorInits := m1InitializedRepositoryCount()

	// Reported from a defer so the measurement survives BOTH exits: on a timeout Ginkgo unwinds the
	// goroutine from inside Should(), and anything after the Eventually would never run. A wait that
	// only reports its number when it succeeds is the instrument 0.6.7 did not have.
	initialized := false
	defer func() {
		outcome := "TIMED OUT"
		if initialized {
			outcome = "initialized"
		}
		AddReportEntry(fmt.Sprintf("repository.init %s", locationName),
			fmt.Sprintf("%s after %s (budget %s; at entry: %s)",
				outcome, time.Since(startedAt).Round(time.Second), budget,
				m1DescribeInitializedRepositoryCount(priorInits)))
	}()

	var repo cbv1.BackupRepository
	Eventually(func(g Gomega) {
		found, ok := m1FindRepository(locationName)
		g.Expect(ok).To(BeTrue(), "no cluster-scoped BackupRepository backs location %q yet", locationName)
		g.Expect(found.Status.Initialized).To(BeTrue(),
			"BackupRepository %s (location %q) not Initialized yet (url=%q)",
			found.Name, locationName, found.Status.RepositoryURL)
		repo = found
	}, budget, 5*time.Second).Should(Succeed(),
		// A func() string description, not a format string: Gomega evaluates it only on failure,
		// which is the only way the elapsed time in it can be the REAL elapsed time rather than
		// zero measured at the call.
		func() string {
			return fmt.Sprintf(
				"BackupRepository for location %q never reported Initialized: gave up after %s (budget %s).\n"+
					"At the moment this wait began, %s.\n"+
					"The EXPECTED cause is a first-init on a cold cluster. `restic init` runs in a Job built from "+
					"the mover image (restic + rclone), and on a node with an empty image cache that Job cannot "+
					"start until the image is pulled. 0.6.7 measured that once: 668s cold, against 31s for a later "+
					"init on a warm cluster — a cost paid ONCE per cluster.\n"+
					"If the line above says a repository had ALREADY initialised, or the elapsed time is far past "+
					"668s, the pull is NOT the explanation and raising this bound a third time will not help: go to "+
					"the init Job and its pod events, and to the object store, not to this number.",
				locationName, time.Since(startedAt).Round(time.Second), budget,
				m1DescribeInitializedRepositoryCount(priorInits))
		})
	initialized = true
	return &repo
}

// m1UnwrapDEK reads the operator-namespace DEK Secret for locationName and unwraps its
// age-wrapped DEK with the suite KEK identity, returning the plaintext restic password.
func m1UnwrapDEK(locationName string) string {
	GinkgoHelper()
	Expect(m1KEKIdentity).NotTo(BeEmpty(),
		"m1EnsurePlatformSecrets must run before m1UnwrapDEK (KEK identity is unset)")

	name := keys.DEKSecretName(locationName)
	var sec corev1.Secret
	Expect(k8s.Get(ctx, client.ObjectKey{Namespace: operatorNS, Name: name}, &sec)).
		To(Succeed(), "get DEK Secret %s/%s", operatorNS, name)

	wrapped, ok := sec.Data[keys.DEKSecretKey]
	Expect(ok).To(BeTrue(), "DEK Secret %s is missing data[%q]", name, keys.DEKSecretKey)

	wrapper, err := keys.NewAgeWrapper(m1KEKIdentity)
	Expect(err).NotTo(HaveOccurred(), "parse KEK identity into an age wrapper")
	dek, err := wrapper.Unwrap(wrapped)
	Expect(err).NotTo(HaveOccurred(), "unwrap DEK from Secret %s", name)
	return string(dek)
}

// m1RunClusterBackup creates a MANUAL (no scheduleRef, deterministic name) ClusterBackup
// whose inline run spec targets locationName and the given namespace selector.
func m1RunClusterBackup(name, locationName string, nsSelector cbv1.NamespaceSelector) *cbv1.ClusterBackup {
	GinkgoHelper()
	cb := &cbv1.ClusterBackup{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: cbv1.ClusterBackupSpec{
			ClusterBackupRunSpec: cbv1.ClusterBackupRunSpec{
				LocationRef: cbv1.LocalObjectReference{Name: locationName},
				Namespaces:  nsSelector,
			},
		},
	}
	Expect(k8s.Create(ctx, cb)).To(Succeed(), "create ClusterBackup %s", name)
	return cb
}

// m1WaitClusterBackupTerminal waits (up to timeout) for a ClusterBackup to reach a terminal
// phase (Completed / PartiallyFailed / Failed) and returns it.
func m1WaitClusterBackupTerminal(name string, timeout time.Duration) *cbv1.ClusterBackup {
	GinkgoHelper()
	var cb cbv1.ClusterBackup
	Eventually(func(g Gomega) {
		g.Expect(k8s.Get(ctx, client.ObjectKey{Name: name}, &cb)).To(Succeed())
		g.Expect(cb.Status.Phase).To(BeElementOf("Completed", "PartiallyFailed", "Failed"),
			"ClusterBackup %s is still non-terminal (phase=%q)", name, cb.Status.Phase)
	}, timeout, 10*time.Second).Should(Succeed())
	return &cb
}

// m1ListBackups lists the Backup objects in a namespace.
func m1ListBackups(namespace string) []cbv1.Backup {
	GinkgoHelper()
	var list cbv1.BackupList
	Expect(k8s.List(ctx, &list, client.InNamespace(namespace))).To(Succeed(), "list Backups in %s", namespace)
	return list.Items
}

// m1LeakCheckBudget bounds the leak check on the ORDINARY path, where the operator survived the run
// and its own teardown is what removes the residue.
//
// 10 min (was 5, was 2): finalizer teardown of VolumeSnapshots/VSContents/clone PVCs on Ceph is slow
// under full-suite load, and M3's default manifest capture adds a manifest-mover Job to every
// backup's teardown. The objects DO drain — a round-1 validation lane measured the external
// snapshot-controller taking just over 5 minutes to collect a Delete-policy content whose teardown
// was already complete (the SAME helper passed at 5m12.8s in the very next spec, and the lane ended
// at zero residuals) — so 5 was the knife's edge of a latency we do not own. This stays a real
// leak-check, not a sleep: the property asserted is unchanged, only the external controller's
// patience.
//
// This number is UNCHANGED by the crash-path budget below, and for the reason
// m1WarmRepositoryInitBudget states in the same file: budgets that everybody spends are how a run
// reaches the suite timeout with a third of its specs unrun. Sixteen of the eighteen call sites run
// on a live operator whose teardown is prompt, and they keep the tight bound.
const m1LeakCheckBudget = 10 * time.Minute

// m1CrashPathLeakCheckBudget bounds the leak check on a spec that DELIBERATELY KILLED THE OPERATOR.
// It is the only budget wide enough to contain the orphan reaper, and only a crash-path spec needs
// it, because only a crash can produce residue that nothing but the reaper will ever collect.
//
// Why the ordinary budget cannot work here, measured on the 0.6.7 campaign-7 failure of
// "The operator killed mid-run converges via Job re-adoption" (m1_reliability_test.go):
//
//	VolumeSnapshotContent snapcontent-… created   21:26:10   deletionTimestamp <nil>, ownerRefs []
//	leak check entered                            21:31:16
//	leak check gave up (600.002s)                 21:41:16
//
// deletionTimestamp <nil> and ownerRefs [] together say that nothing had asked for this object's
// deletion and nothing would garbage-collect it: it is not draining, it is WAITING FOR THE REAPER —
// the designed backstop for exactly what a crashed operator never got to record. And the reaper's
// floor puts that collection out of reach of any ten-minute window:
//
//	internal/controller/reaper.go  defaultReaperMinAge   = 30 * time.Minute
//	internal/controller/reaper.go  defaultReaperInterval = 10 * time.Minute
//
//	eligible at            created + MinAge          = 21:56:10
//	swept no later than    eligible + one Interval   = 22:06:10   (the sweep before eligibility misses it)
//
// The check could not have observed the collection; it gave up 25 minutes before the object even
// became eligible. The spec has been green until now only when the ordinary terminal teardown won
// the race instead — the kill landing in this particular window is what removed that luck.
//
// The derivation, carried here rather than rounded away, so a change to either reaper figure is
// traceable to this bound:
//
//	MinAge                      30m   eligibility floor, from creation
//	+ one Interval              10m   worst case: the sweep just before eligibility misses it
//	                          = 40m   worst case from CREATION to the sweep that collects it
//	+ sweep + drain margin      10m   the sweep's delete and read-back, plus the external
//	                                  snapshot-controller's cascade on a Delete-policy content
//	                                  (measured at just over 5 min under load — the same latency
//	                                  m1LeakCheckBudget above was raised for), plus one 5s poll
//	                          = 50m
//
// The clock here starts when the CHECK ENTERS, which is strictly later than creation (5m06s later
// in the measurement above), so the real margin is this 10 minutes plus however long the spec spent
// between creating the run and looking — never less.
//
// WHAT A SPEC ON THIS BUDGET IS ACTUALLY ASSERTING — say it plainly, because it is weaker than what
// the same line used to claim and the change must not hide inside a bigger number. It is no longer
// "teardown is prompt". It is: A CRASHED OPERATOR LOSES NOTHING PERMANENTLY — the backstop collects
// what the crash left unrecorded. Promptness is still asserted on the ordinary path by the sixteen
// call sites that keep m1LeakCheckBudget; here the property under test is durability, not speed,
// and a spec that fails on THIS budget is reporting a genuine permanent leak: an object that
// outlived both the operator's own teardown and the reaper that exists to catch what teardown
// missed.
//
// THE FIX THIS IS NOT, and where it belongs. The right change is to make the reaper's floor
// reachable by a campaign: MinAge and Interval are already struct fields
// (internal/controller/reaper.go:167-168) with the defaults applied in Start, so the product is one
// step from configurable — but NOTHING EXPOSES THEM. There is no CLI flag (cmd/main.go constructs
// OrphanReaper with Client/APIReader/Recorder/OperatorNamespace and leaves both zero) and no Helm
// value, so a campaign cannot shorten the floor and has no choice but to tolerate forty minutes.
// That is worth naming for what it is: A BACKSTOP NO CAMPAIGN CAN REACH IS A BACKSTOP NOBODY HAS
// VERIFIED — until this failure, every green run of this spec was a run in which the ordinary
// teardown won and the reaper was never exercised at all. Exposing the two fields would let a
// crash-path spec set MinAge to a minute and assert the collection in minutes instead of enduring
// it, turning forty minutes of waiting into a real, repeatable test of the backstop. That is an M7
// product change (flag + Helm value + defaulting), deliberately NOT made here: this fix is
// test-side only.
const m1CrashPathLeakCheckBudget = 50 * time.Minute

// m1AssertNoResidualSnapshotObjects asserts (with a grace period for async cleanup) that a run left
// nothing behind in the given namespaces: no VolumeSnapshots, no VolumeSnapshotContents pointing
// back at them, and no temporary clone PVCs (the exposure label, but not the seed label the tenant
// PVCs carry).
//
// ownRuns names the runs the CALLING SPEC is answerable for — the ClusterBackups, Backups and
// Restores it created itself, whose teardown it is legitimately waiting on. It is a required
// parameter, not an option, because the single question this helper cannot answer for itself is
// "is this residue mine?", and a call site that silently forgot to say would inherit the very bug
// this parameter fixes. Passing nil is a valid, meaningful answer — "this spec owns no run, so any
// residue here is somebody else's" — and every call site says which of the two it means.
//
// This is the ORDINARY-PATH entry point and it spends m1LeakCheckBudget. A spec that killed the
// operator must call m1AssertNoResidualSnapshotObjectsWithin(m1CrashPathLeakCheckBudget, …)
// instead, and say so at the call site.
func m1AssertNoResidualSnapshotObjects(ownRuns []string, namespaces ...string) {
	GinkgoHelper()
	m1AssertNoResidualSnapshotObjectsWithin(m1LeakCheckBudget, ownRuns, namespaces...)
}

// m1AssertNoResidualSnapshotObjectsWithin is m1AssertNoResidualSnapshotObjects with the grace period
// named by the caller. It exists so that the ONE class of call site that depends on the orphan
// reaper — a spec that killed the operator mid-teardown — can wait for the reaper without every
// other call site paying for it. The detection, the ownership discriminator and the fail-fast are
// identical; only the patience differs. See m1LeakCheckBudget / m1CrashPathLeakCheckBudget for
// which is which and why, in the same spirit as the cold/warm repository-init budgets above.
func m1AssertNoResidualSnapshotObjectsWithin(budget time.Duration, ownRuns []string, namespaces ...string) {
	GinkgoHelper()

	// Exposure objects live in the tenant namespaces (the dynamic origin VolumeSnapshot) AND —
	// after ADR 0003's static VS/VSC re-bind — in the operator namespace (the static VolumeSnapshot
	// and the temp clone PVC the mover mounts). Check BOTH. Every exposure object carries a
	// crystalbackup.io/* label (internal/controller exposureLabels), which is the leak selector the
	// orphan reaper also uses.
	checkNS := append(append([]string{}, namespaces...), operatorNS)
	checkSet := make(map[string]bool, len(checkNS))
	for _, ns := range checkNS {
		checkSet[ns] = true
	}

	// The fail-fast below bounds the BLAST RADIUS of somebody else's leak, and it exists because the
	// 0.6.5 campaign lost a whole suite to one object.
	//
	// The DETECTION is deliberately broad: an object carrying the operator's exposure label is residue
	// no matter which namespace it sits in, because narrowing it per-spec is how a leak hides. That
	// breadth is right and stays. What was wrong is the WAITING. One leaked VolumeSnapshotContent
	// from the m5 lane failed five leak checks in four unrelated milestones, and each of them sat out
	// its full 600s Eventually before saying so — about 95 minutes, which is what pushed the run into
	// the 240m suite timeout and left 21 of 93 specs unrun. A truncated suite is worse than a failed
	// one: those 21 are not passes, they are unknowns.
	//
	// 0.6.5 discriminated on AGE, and stated its premise here as the rationale: "no teardown this spec
	// is waiting on can remove an object that already existed before the spec looked". THE PREMISE IS
	// FALSE, and it was false the day it was written. Every spec creates its run BEFORE it checks that
	// run for residue, so a spec's own still-draining objects necessarily predate their own leak check
	// and `created.Before(enteredAt)` is always true for them. The check could only ever pass by
	// winning a race it explicitly refused to wait for.
	//
	// It has therefore been a coin flip in every campaign since. 0.6.6 passed m6/stall's check at
	// 30m50.6s — that was LUCK, not a verdict. 0.6.7 lost the flip: the campaign reported a product
	// leak that did not exist, on m6/stall's own temporary clone PVC, created 30 minutes before the
	// check while the spec deliberately waited out the operator's 30-minute moverStartDeadline, and
	// whose teardown could not even BEGIN until the RBD node plugin was restored two lines above the
	// check. m6/stall is only where the window is widest; the flaw was general.
	//
	// So the discriminator is OWNERSHIP, not age. Residue attributable to one of ownRuns — via the
	// crystalbackup.io/{backup,cluster-backup,restore,cluster-restore} link labels, see
	// m1ResidueOwnedBy — is polled normally inside the budget below: that IS the race this helper
	// exists for. Residue attributable to any other run, or to no run at all (the 0.6.5 object's own
	// shape), still fails fast, unchanged.
	//
	// Rejected: raising the age threshold ("older than N minutes is foreign"). That is the same coin
	// flip with a longer edge — m6/stall's residue is 30 minutes old by construction, while a leak
	// from the immediately preceding lane can be seconds old, so no N separates the two classes.
	// Also rejected: dropping the fail-fast and letting every check spend its budget. That is exactly
	// the 95 minutes that truncated the 0.6.5 suite.
	//
	// enteredAt is no longer the discriminator, but it stays in both messages: "created 30 minutes
	// before the check entered" is the fact that explains the verdict either way.
	enteredAt := time.Now()
	failFastIfForeign := func(g Gomega, created time.Time, labels map[string]string, names []string,
		format string, args ...any) {
		if own := m1ResidueOwnedBy(labels, names, ownRuns); own != "" {
			// Ours, and possibly still draining. Poll it out, and SAY which discriminator decided
			// that — a reader six months from now must not have to re-derive why this one waited and
			// the next one did not.
			//
			// Gomega's optional description is variadic ...any, so a format plus its arguments has to
			// be assembled into one slice rather than passed as two.
			g.Expect(false).To(BeTrue(), append([]any{
				"residue belongs to run %q, which THIS spec owns (created %s, check entered %s) — own-run " +
					"residue is POLLED, never failed fast: it predates this check by construction, because " +
					"the spec created the run before checking it. Still present at the end of the budget " +
					"below means a real leak, not a race:\n  " + format,
				own, created.Format(time.RFC3339), enteredAt.Format(time.RFC3339)}, args...)...)
			return
		}
		StopTrying(fmt.Sprintf("residue is NOT this spec's: it attributes to %s, and this spec owns %v "+
			"(created %s, check entered %s) — it leaked in another lane, so waiting here cannot clear it "+
			"and only burns the budget of every spec after this one; fix the leak its own lane reported:\n  "+
			format,
			append([]any{m1DescribeResidueOwner(labels), ownRuns,
				created.Format(time.RFC3339), enteredAt.Format(time.RFC3339)}, args...)...)).Now()
	}

	Eventually(func(g Gomega) {
		// (1) No CrystalBackup-labelled VolumeSnapshots left in any checked namespace: neither the
		//     dynamic origin VS (tenant ns) nor the static re-bound VS (operator ns).
		for _, ns := range checkNS {
			vs := &unstructured.UnstructuredList{}
			vs.SetGroupVersionKind(schema.GroupVersionKind{
				Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshotList",
			})
			g.Expect(k8s.List(ctx, vs, client.InNamespace(ns))).To(Succeed())
			for i := range vs.Items {
				if m1IsExposureResidue(vs.Items[i].GetLabels()) {
					failFastIfForeign(g, vs.Items[i].GetCreationTimestamp().Time,
						vs.Items[i].GetLabels(), []string{vs.Items[i].GetName()},
						"residual VolumeSnapshot %s/%s (labels %v)", ns, vs.Items[i].GetName(), vs.Items[i].GetLabels())
				}
			}
		}

		// (2) No CrystalBackup VolumeSnapshotContents (cluster-scoped) left: neither our static VSC
		//     (which carries our labels and references the OPERATOR namespace, so a namespace filter
		//     alone would miss it) nor a dynamic origin VSC still pointing back at a checked namespace.
		vsc := &unstructured.UnstructuredList{}
		vsc.SetGroupVersionKind(schema.GroupVersionKind{
			Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshotContentList",
		})
		g.Expect(k8s.List(ctx, vsc)).To(Succeed())
		for i := range vsc.Items {
			item := &vsc.Items[i]
			labelled := m1IsExposureResidue(item.GetLabels())
			refNS, _, _ := unstructured.NestedString(item.Object, "spec", "volumeSnapshotRef", "namespace")
			// The forensics below exist because one real leak cost three paid crucible rounds to
			// attribute: the failure message must carry everything needed to identify the residue's
			// nature (deletionPolicy Retain = the operator's handover park; a set deletionTimestamp
			// = something DID ask for deletion and is stuck; a dynamic snapcontent-<uuid> name vs
			// our static <prefix>-vsc = which cleanup step was missed) without another run.
			policy, _, _ := unstructured.NestedString(item.Object, "spec", "deletionPolicy")
			refName, _, _ := unstructured.NestedString(item.Object, "spec", "volumeSnapshotRef", "name")
			if labelled || checkSet[refNS] {
				// refName joins the object's own name in the attribution names: a dynamic origin
				// content that leaked before the handover patch stamped our labels carries none of
				// them, and the VolumeSnapshot it references is named from moverNamePrefix — which is
				// the only thread left tying it to the run that made it.
				failFastIfForeign(g, item.GetCreationTimestamp().Time,
					item.GetLabels(), []string{item.GetName(), refName},
					"residual VolumeSnapshotContent %s (labels %v, refNS %s/%s, deletionPolicy %s, created %s, deletionTimestamp %v, ownerRefs %v, operator restarts: %s)",
					item.GetName(), item.GetLabels(), refNS, refName, policy,
					item.GetCreationTimestamp().Format(time.RFC3339), item.GetDeletionTimestamp(),
					item.GetOwnerReferences(), m1OperatorRestartSummary())
			}
		}

		// (3) No temporary clone PVCs (a crystalbackup.io/* label, but not a seed PVC) in any checked
		//     namespace — including the operator namespace, where the mover-mounted clone lives.
		for _, ns := range checkNS {
			var pvcs corev1.PersistentVolumeClaimList
			g.Expect(k8s.List(ctx, &pvcs, client.InNamespace(ns))).To(Succeed())
			for i := range pvcs.Items {
				p := &pvcs.Items[i]
				if m1IsExposureResidue(p.Labels) && p.Labels[m1SeedLabel] == "" {
					failFastIfForeign(g, p.CreationTimestamp.Time, p.Labels, []string{p.Name},
						"residual temporary clone PVC %s/%s (labels %v)", ns, p.Name, p.Labels)
				}
			}
		}
		// The budget is the caller's: m1LeakCheckBudget (10 min) on the ordinary path, where the
		// operator's own teardown removes the residue, and m1CrashPathLeakCheckBudget (50 min) for
		// a spec that killed the operator and can therefore only be cleared by the orphan reaper.
		// Both constants carry their own derivation; neither is a sleep — the property asserted
		// here is identical, only the patience differs.
	}, budget, 5*time.Second).Should(Succeed())
}

// m1OperatorRestartSummary reports the operator pods' identity, start time and container restart
// counts as one string, for the leak-check's failure forensics: a residual whose message shows a
// fresh pod (or a non-zero restartCount) points at the restart window; a stable pod excludes it.
// Best-effort — the summary must never turn an unreadable pod list into a spec failure.
func m1OperatorRestartSummary() string {
	var pods corev1.PodList
	if err := k8s.List(ctx, &pods, client.InNamespace(operatorNS),
		client.MatchingLabels{"app.kubernetes.io/name": "crystal-backup"}); err != nil {
		return "unreadable: " + err.Error()
	}
	parts := make([]string, 0, len(pods.Items))
	for i := range pods.Items {
		p := &pods.Items[i]
		restarts := int32(0)
		for _, cs := range p.Status.ContainerStatuses {
			restarts += cs.RestartCount
		}
		started := "?"
		if p.Status.StartTime != nil {
			started = p.Status.StartTime.Format(time.RFC3339)
		}
		parts = append(parts, fmt.Sprintf("%s(started %s, restarts %d)", p.Name, started, restarts))
	}
	if len(parts) == 0 {
		return "no operator pod found"
	}
	return strings.Join(parts, ", ")
}

// m1DeleteOperatorPod deletes the operator pod(s) (app.kubernetes.io/name=crystal-backup) to
// force a restart, then waits (up to 3 min) for a FRESH pod to become Ready.
func m1DeleteOperatorPod() {
	GinkgoHelper()
	sel := client.MatchingLabels{"app.kubernetes.io/name": "crystal-backup"}

	var before corev1.PodList
	Expect(k8s.List(ctx, &before, client.InNamespace(operatorNS), sel)).To(Succeed())
	old := make(map[types.UID]bool, len(before.Items))
	for _, p := range before.Items {
		old[p.UID] = true
	}

	Expect(k8s.DeleteAllOf(ctx, &corev1.Pod{}, client.InNamespace(operatorNS), sel)).
		To(Succeed(), "delete operator pod(s) in %s", operatorNS)

	Eventually(func(g Gomega) {
		var pods corev1.PodList
		g.Expect(k8s.List(ctx, &pods, client.InNamespace(operatorNS), sel)).To(Succeed())
		fresh := 0
		for _, p := range pods.Items {
			if old[p.UID] || p.DeletionTimestamp != nil {
				continue
			}
			for _, c := range p.Status.Conditions {
				if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
					fresh++
				}
			}
		}
		g.Expect(fresh).To(BeNumerically(">=", 1), "no fresh operator pod is Ready yet in %s", operatorNS)
	}, 3*time.Minute, 5*time.Second).Should(Succeed())
}
