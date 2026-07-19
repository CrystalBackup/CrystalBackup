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
	"sigs.k8s.io/controller-runtime/pkg/client"

	"filippo.io/age"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
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
	m1SeedLabel = "crystalbackup.io/seed"
	m1SeedValue = "crucible"
)

// m1KEKIdentity is the age KEK identity string ("AGE-SECRET-KEY-1...") minted (or re-read)
// by m1EnsurePlatformSecrets and consumed by m1UnwrapDEK to turn the operator's wrapped DEK
// back into the restic repository password. Suite-level so a spec's Given/When/Then can
// straddle several helper calls.
var m1KEKIdentity string

// m1SkipIfNoS3 Skip()s the current spec unless every S3 coordinate the M1 data path needs is
// present in the environment (exported by test/crucible/scripts/load-env.sh).
func m1SkipIfNoS3() {
	GinkgoHelper()
	for _, key := range []string{"S3_ENDPOINT", "S3_BUCKET", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY"} {
		if os.Getenv(key) == "" {
			Skip("S3 not configured: $" + key + " is empty — run via `mise run test` so terraform facts + secrets are loaded")
		}
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
				Enabled:  true,
				Interval: metav1.Duration{Duration: time.Minute},
			},
		},
	}
}

// m1CreateLocation builds and creates a location, asserting the create succeeds.
func m1CreateLocation(name string, isDefault bool) *cbv1.ClusterBackupLocation {
	GinkgoHelper()
	loc := m1LocationObject(name, isDefault)
	Expect(k8s.Create(ctx, loc)).To(Succeed(), "create ClusterBackupLocation %s", name)
	return loc
}

// m1TryCreateLocation builds and creates a location, RETURNING the create error (nil on
// success) instead of asserting — for tests that must observe an admission denial (the
// single-default webhook, adr/0010).
func m1TryCreateLocation(name string, isDefault bool) error {
	return k8s.Create(ctx, m1LocationObject(name, isDefault))
}

// m1DeleteLocation best-effort deletes a ClusterBackupLocation (cleanup; ignores NotFound).
func m1DeleteLocation(name string) {
	_ = k8s.Delete(ctx, &cbv1.ClusterBackupLocation{ObjectMeta: metav1.ObjectMeta{Name: name}})
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

// m1WaitRepositoryInitialized waits (up to 5 min) for the BackupRepository backing
// locationName to report status.initialized==true, and returns it.
func m1WaitRepositoryInitialized(locationName string) *cbv1.BackupRepository {
	GinkgoHelper()
	var repo cbv1.BackupRepository
	Eventually(func(g Gomega) {
		found, ok := m1FindRepository(locationName)
		g.Expect(ok).To(BeTrue(), "no cluster-scoped BackupRepository backs location %q yet", locationName)
		g.Expect(found.Status.Initialized).To(BeTrue(),
			"BackupRepository %s (location %q) not Initialized yet (url=%q)",
			found.Name, locationName, found.Status.RepositoryURL)
		repo = found
	}, 5*time.Minute, 5*time.Second).Should(Succeed())
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

// m1SeedSelector is the label selector matching the crucible's seeded tenant namespaces.
func m1SeedSelector() metav1.LabelSelector {
	return metav1.LabelSelector{MatchLabels: map[string]string{m1SeedLabel: m1SeedValue}}
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

// m1HasCrystalLabel reports whether labels carry any crystalbackup.io/* key.
func m1HasCrystalLabel(labels map[string]string) bool {
	for k := range labels {
		if strings.HasPrefix(k, apiconst.Domain+"/") {
			return true
		}
	}
	return false
}

// m1AssertNoResidualSnapshotObjects asserts (with a short grace period for async cleanup)
// that a run left nothing behind in the given namespaces: no VolumeSnapshots, no
// VolumeSnapshotContents pointing back at them, and no temporary clone PVCs (a
// crystalbackup.io/* label but not the seed label the tenant PVCs carry).
func m1AssertNoResidualSnapshotObjects(namespaces ...string) {
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
				g.Expect(m1HasCrystalLabel(vs.Items[i].GetLabels())).To(BeFalse(),
					"residual VolumeSnapshot %s/%s (labels %v)", ns, vs.Items[i].GetName(), vs.Items[i].GetLabels())
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
			labelled := m1HasCrystalLabel(vsc.Items[i].GetLabels())
			refNS, _, _ := unstructured.NestedString(vsc.Items[i].Object, "spec", "volumeSnapshotRef", "namespace")
			g.Expect(labelled || checkSet[refNS]).To(BeFalse(),
				"residual VolumeSnapshotContent %s (labels %v, refNS %s)",
				vsc.Items[i].GetName(), vsc.Items[i].GetLabels(), refNS)
		}

		// (3) No temporary clone PVCs (a crystalbackup.io/* label, but not a seed PVC) in any checked
		//     namespace — including the operator namespace, where the mover-mounted clone lives.
		for _, ns := range checkNS {
			var pvcs corev1.PersistentVolumeClaimList
			g.Expect(k8s.List(ctx, &pvcs, client.InNamespace(ns))).To(Succeed())
			for i := range pvcs.Items {
				p := &pvcs.Items[i]
				residual := m1HasCrystalLabel(p.Labels) && p.Labels[m1SeedLabel] == ""
				g.Expect(residual).To(BeFalse(),
					"residual temporary clone PVC %s/%s (labels %v)", ns, p.Name, p.Labels)
			}
		}
	}, 2*time.Minute, 5*time.Second).Should(Succeed())
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
