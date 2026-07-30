//go:build e2e
// +build e2e

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

package e2e

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/CrystalBackup/CrystalBackup/test/utils"
)

// M5 — external sync on a real cluster (R28, spec/adr/0013-external-backup-sync.md,
// spec/08-testing-and-dod.md case 18). NAMESPACE plane: a user gives their namespace a second
// BackupLocation with its own bucket and its own key, and a BackupExternalSync copies their
// backups into it with `restic copy`.
//
// What THIS harness proves, and why it and not the crucible:
//
//   - CROSS-CREDENTIAL. adr/0013's amendment exists because restic carries two repository KEYS but
//     only one set of backend CREDENTIALS, which is why both repositories are addressed as rclone
//     remotes. A sync whose two ends authenticate with the SAME access key would exercise the
//     plumbing and prove nothing about the limitation it was built to escape. SeaweedFS gives us
//     two independent S3 identities (test/e2e/manifests/seaweedfs.yaml), the second one scoped to
//     the destination bucket ALONE — so if the copy ever addressed the source bucket with the
//     destination's key it would fail AccessDenied rather than silently pass. The crucible cannot
//     make this assertion at all: Hetzner Object Storage credentials are project-wide, so two
//     buckets there share one account.
//   - THE COPY RUNS THE SYNC IMAGE, AND IN THE RIGHT DIRECTION. `-r` is the DESTINATION and
//     `--from-repo` is the SOURCE; reading `-r` as "the repository I am working on" gets it
//     backwards, and backwards means copying the secondary over the primary. The Job's env is
//     inspected while it runs, so the direction is pinned by observation rather than by argument.
//   - ADMISSION. Rule 9 (source != destination) is a chart-rendered ValidatingAdmissionPolicy, so
//     this container installs the chart WITH admission enabled — unlike the M3 container, which
//     disables it. Rule 2's namespace-plane half is STRUCTURAL (both refs are name-only, resolved
//     in the CR's own namespace), and a structural guarantee is proven by showing the resolution,
//     not by looking for a policy that does not exist.
//
// What it deliberately does NOT prove: that the destination opens with its OWN key and not the
// source's. That is case 18's re-encryption half and it belongs to the crucible, where the restic
// oracle (m1ResticExec) can open a repository with an arbitrary password against real object
// storage and observe the failure.

const (
	// m5OperatorNS is the chart's fixed operator namespace — the same singleton the M3 container
	// installs into (the chart's fullname is NOT release-prefixed, so only one release can own it
	// at a time; see m5UninstallOtherReleases).
	m5OperatorNS = "crystal-backup-system"
	// m5Release is this container's Helm release name.
	m5Release        = "crystal-backup-m5"
	m5OperatorDeploy = "crystal-backup"

	// m5TenantNS is the user namespace that owns both locations, both keys and both credential
	// Secrets. Everything the sync touches lives here or in the operator namespace — there is no
	// third party.
	m5TenantNS = "m5-tenant"

	// The two namespaced locations of case 18, named as the case names them.
	m5SourceLocation = "my-offsite"
	m5DestLocation   = "my-offsite-2"

	// The two buckets pre-created by the SeaweedFS bootstrap Job. tenant-a-offsite-2 is the ONLY
	// bucket the second identity may touch.
	m5SourceBucket = "tenant-a-backups"
	m5DestBucket   = "tenant-a-offsite-2"

	// One credential Secret per location, in the TENANT's namespace (a namespaced location's
	// credentials are read from its own namespace, never the operator's). Different access keys:
	// that is the point.
	m5SourceCredsSecret = "m5-source-s3"
	m5DestCredsSecret   = "m5-dest-s3"

	// One restic password per location, both user-owned. Distinct values, so the destination is an
	// independent repository rather than a byte clone.
	m5SourceKeySecret = "m5-source-key"
	m5DestKeySecret   = "m5-dest-key"

	m5BackupRun = "m5-run"
	m5SyncName  = "m5-offsite"
	// m5SyncCopyJob is the copy Job's DETERMINISTIC name: syncResourceName(syncJobPrefixNamespaced,
	// <sync name>, "copy"). Deterministic is what makes the in-flight inspection below possible.
	m5SyncCopyJob = "bes-" + m5SyncName + "-copy"

	m5ConfigMap = "m5-payload"

	m5S3Endpoint = "http://seaweedfs.crystalbackup-e2e.svc.cluster.local:8333"
	m5S3Prefix   = "crystal"
	m5S3Region   = "us-east-1"
)

// m5ClusterID is the repository path segment (<bucket>/<prefix>/<clusterID>), unique per run so a
// reused Kind cluster never re-opens a previous run's repository with this run's freshly minted
// keys. Both locations share it — they differ by BUCKET, which is what makes them two repositories.
var m5ClusterID string

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

// m5UninstallOtherReleases removes any OTHER Helm release from the operator namespace.
//
// The chart is a singleton: crystal-backup.fullname is not release-prefixed, so two releases would
// both claim the objects named "crystal-backup" and the second install fails on Helm's ownership
// metadata. Every e2e container that Helm-installs the chart uninstalls itself afterwards, but a
// container that died mid-run leaves its release behind — and then THIS container's install fails
// for a reason that has nothing to do with external sync. Clearing the namespace first turns that
// into a no-op instead of a confusing failure.
func m5UninstallOtherReleases() {
	GinkgoHelper()
	out, err := utils.Run(exec.Command("helm", "list", "-n", m5OperatorNS, "-q"))
	if err != nil {
		return // no namespace yet, or no helm state — nothing to clear
	}
	for _, name := range utils.GetNonEmptyLines(out) {
		if name = strings.TrimSpace(name); name == "" || name == m5Release {
			continue
		}
		_, _ = utils.RunWithTimeout(exec.Command("helm", "uninstall", name,
			"--namespace", m5OperatorNS, "--wait", "--timeout", "2m"), 3*time.Minute)
	}
}

// m5ClearUnownedChartObjects removes chart-rendered objects that no Helm release owns.
//
// The M2 admission container installs the SAME admission objects with `helm template | kubectl
// apply`, so they carry no meta.helm.sh ownership annotations, and its AfterAll reaps the policies
// and bindings BY LABEL — which misses rule 7's paramRef ConfigMap, the one chart object in that
// template that is not a policy. A later `helm install` then refuses to adopt it and the whole
// container dies at setup with "invalid ownership metadata".
//
// The M3 container never met this because it installs with admission DISABLED, so the chart renders
// none of these; this container is the first to install WITH it. Called after
// m5UninstallOtherReleases, so by construction any chart object still standing in the namespace is
// owned by nothing and safe to remove — the install below recreates all of it.
func m5ClearUnownedChartObjects() {
	GinkgoHelper()
	_, _ = kubectl("delete", "configmap", "crystal-backup-denied-namespaces",
		"-n", m5OperatorNS, "--ignore-not-found")
	// Belt and braces for a container that died before its own AfterAll: same label the M2
	// container reaps by, so this is a no-op on a clean cluster.
	_, _ = kubectl("delete", "validatingadmissionpolicybinding",
		"-l", "app.kubernetes.io/name=crystal-backup", "--ignore-not-found")
	_, _ = kubectl("delete", "validatingadmissionpolicy",
		"-l", "app.kubernetes.io/name=crystal-backup", "--ignore-not-found")
}

// m5DeployOperatorViaHelm installs the operator from the packaged chart with the data path wired
// AND admission enabled.
//
// Admission is the difference from the M3 container's otherwise identical install: rule 9 ships as
// a chart-rendered ValidatingAdmissionPolicy, so a container that disabled admission could not
// observe the denial at all. The dynamic webhook stays off (it guards the single-default
// ClusterBackupLocation, which this container never creates) so nothing here depends on a serving
// certificate.
func m5DeployOperatorViaHelm() {
	GinkgoHelper()

	By("installing CRDs (idempotent; the chart's crds/ are create-if-absent)")
	_, err := utils.Run(exec.Command("make", "install"))
	Expect(err).NotTo(HaveOccurred(), "Failed to install CRDs")

	By("ensuring the operator namespace exists with baseline PodSecurity (movers run runAsUser:0)")
	_, _ = kubectl("create", "namespace", m5OperatorNS)
	_, err = kubectl("label", "namespace", m5OperatorNS,
		"pod-security.kubernetes.io/enforce=baseline", "--overwrite")
	Expect(err).NotTo(HaveOccurred(), "labelling %s with baseline PSA", m5OperatorNS)

	opRepo, opTag := splitImageRef(managerImage)
	mvRepo, mvTag := splitImageRef(moverImage)
	syRepo, syTag := splitImageRef(syncImage)

	By("helm upgrade --install the operator (data path + admission wired)")
	helmArgs := []string{
		"upgrade", "--install", m5Release, filepath.Join("charts", "crystal-backup"),
		"--namespace", m5OperatorNS,
		// CRDs belong to kubectl (`make install` above); see the M3 container for why Helm must
		// stay off them.
		"--skip-crds",
		"--set", "namespace.create=false",
		"--set", "image.digest=",
		"--set", "image.repository=" + opRepo,
		"--set", "image.tag=" + opTag,
		"--set", "image.pullPolicy=IfNotPresent",
		"--set", "mover.image.digest=",
		"--set", "mover.image.repository=" + mvRepo,
		"--set", "mover.image.tag=" + mvTag,
		// The image under test in this container. A wrong value here does not fail loudly — it
		// fails as an ImagePullBackOff inside a Job nobody is watching — which is exactly why the
		// spec below reads the running Job's image back rather than trusting this flag.
		"--set", "sync.image.digest=",
		"--set", "sync.image.repository=" + syRepo,
		"--set", "sync.image.tag=" + syTag,
		"--set", "admission.vap.enabled=true",
		"--set", "admission.webhook.enabled=false",
		// kindnet does not enforce NetworkPolicy, so creating them would only add objects that
		// mean nothing here.
		"--set", "networkPolicy.create=false",
		"--wait", "--timeout", "6m",
	}
	out, err := utils.RunWithTimeout(exec.Command("helm", helmArgs...), 8*time.Minute)
	Expect(err).NotTo(HaveOccurred(), "helm install failed: %s", out)

	By("waiting for the operator Deployment to be Available")
	_, err = kubectl("rollout", "status", "-n", m5OperatorNS, "deploy/"+m5OperatorDeploy, "--timeout=3m")
	Expect(err).NotTo(HaveOccurred(), "operator Deployment did not roll out")
}

// m5SeedTenant creates the tenant namespace, the two S3 credential Secrets (DIFFERENT identities),
// the two restic password Secrets (DIFFERENT passwords) and a ConfigMap for the manifest capture to
// have something to write.
func m5SeedTenant() {
	GinkgoHelper()

	By("ensuring any prior tenant namespace is fully gone before seeding")
	Eventually(func(g Gomega) {
		out, _ := kubectl("get", "namespace", m5TenantNS, "--ignore-not-found", "-o", "name")
		g.Expect(strings.TrimSpace(out)).To(BeEmpty(), "tenant namespace still terminating")
	}, 3*time.Minute, 3*time.Second).Should(Succeed())

	_, err := kubectl("create", "namespace", m5TenantNS)
	Expect(err).NotTo(HaveOccurred(), "create tenant namespace")

	// The two identities are the ones baked into test/e2e/manifests/seaweedfs.yaml. `offsite-2` is
	// authorized on tenant-a-offsite-2 ONLY — a copy that mixed the two remotes up would be denied
	// by SeaweedFS, not silently succeed.
	m5CreateSecret(m5SourceCredsSecret,
		"AWS_ACCESS_KEY_ID=crystalbackup", "AWS_SECRET_ACCESS_KEY=crystalbackup-secret")
	m5CreateSecret(m5DestCredsSecret,
		"AWS_ACCESS_KEY_ID=offsite-2", "AWS_SECRET_ACCESS_KEY=offsite-2-secret")

	// Two USER-owned restic passwords. Distinct on purpose: the destination is an independent
	// repository with its own key (adr/0013), never a byte clone of the source.
	m5CreateSecret(m5SourceKeySecret, "restic-password=m5-source-"+m5ClusterID)
	m5CreateSecret(m5DestKeySecret, "restic-password=m5-dest-"+m5ClusterID)

	_, err = m3Apply(fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: %s
  namespace: %s
data:
  payload: external-sync
`, m5ConfigMap, m5TenantNS))
	Expect(err).NotTo(HaveOccurred(), "seed the tenant ConfigMap")
}

// m5CreateSecret applies an Opaque Secret in the tenant namespace from literal key=value pairs.
func m5CreateSecret(name string, literals ...string) {
	GinkgoHelper()
	args := []string{"create", "secret", "generic", name, "-n", m5TenantNS}
	for _, l := range literals {
		args = append(args, "--from-literal="+l)
	}
	args = append(args, "--dry-run=client", "-o", "yaml")
	rendered, err := kubectl(args...)
	Expect(err).NotTo(HaveOccurred(), "render Secret %s", name)
	_, err = m3Apply(rendered)
	Expect(err).NotTo(HaveOccurred(), "apply Secret %s", name)
}

// m5LocationManifest renders a namespaced BackupLocation on its own bucket, with its own
// credentials and its own user key.
//
// clusterID is set EXPLICITLY rather than defaulted: the default comes from the default
// ClusterBackupLocation, and this container deliberately creates none — the namespace plane must
// stand on its own here, with no cluster-plane object anywhere in the picture.
func m5LocationManifest(name, bucket, credsSecret, keySecret string) string {
	return fmt.Sprintf(`apiVersion: crystalbackup.io/v1alpha1
kind: BackupLocation
metadata:
  name: %[1]s
  namespace: %[2]s
spec:
  mode: Standard
  clusterID: %[3]s
  s3:
    endpoint: %[4]s
    bucket: %[5]s
    prefix: %[6]s
    region: %[7]s
    credentialsSecretRef:
      name: %[8]s
    forcePathStyle: true
  encryption:
    repositoryPasswordSecretRef:
      name: %[9]s
  discovery:
    enabled: false
`, name, m5TenantNS, m5ClusterID, m5S3Endpoint, bucket, m5S3Prefix, m5S3Region, credsSecret, keySecret)
}

// m5CreateLocations creates both locations and waits for both repositories to initialise — two
// restic-init mover Jobs, one per bucket, each authenticating with its OWN access key. A green here
// already proves the two credential sets are independently usable.
func m5CreateLocations() {
	GinkgoHelper()

	_, err := m3Apply(m5LocationManifest(m5SourceLocation, m5SourceBucket, m5SourceCredsSecret, m5SourceKeySecret))
	Expect(err).NotTo(HaveOccurred(), "apply the source BackupLocation")
	_, err = m3Apply(m5LocationManifest(m5DestLocation, m5DestBucket, m5DestCredsSecret, m5DestKeySecret))
	Expect(err).NotTo(HaveOccurred(), "apply the destination BackupLocation")

	for _, name := range []string{m5SourceLocation, m5DestLocation} {
		By("waiting for BackupLocation " + name + " to become Ready (repository initialised)")
		m5WaitLocationReady(name)
	}
}

// m5WaitLocationReady waits for a namespaced location's Ready condition, reporting the phase and
// message when it does not arrive (a repository init failure is otherwise invisible from here).
func m5WaitLocationReady(name string) {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		out, err := kubectl("get", "backuplocation", name, "-n", m5TenantNS,
			"-o", "jsonpath={.status.conditions[?(@.type=='Ready')].status}")
		g.Expect(err).NotTo(HaveOccurred())
		if strings.TrimSpace(out) == "True" {
			return
		}
		phase, _ := kubectl("get", "backuplocation", name, "-n", m5TenantNS, "-o", "jsonpath={.status.phase}")
		msg, _ := kubectl("get", "backuplocation", name, "-n", m5TenantNS,
			"-o", "jsonpath={.status.conditions[?(@.type=='Ready')].message}")
		g.Expect(strings.TrimSpace(out)).To(Equal("True"),
			"BackupLocation %s not Ready yet — phase=%q message=%q",
			name, strings.TrimSpace(phase), strings.TrimSpace(msg))
	}, 10*time.Minute, 5*time.Second).Should(Succeed())
}

// m5RunBackup runs a MANIFEST-ONLY namespace-plane Backup into the source location, so the source
// repository holds a snapshot for the sync to copy.
//
// Manifest-only (pvcSelector excludes everything) because what this container is about is the COPY:
// a data snapshot would add a CSI round trip and a mover per PVC without changing a single
// assertion below. The snapshot it does produce carries the full identity — host, paths, tags —
// that discovery and the copy both key on.
func m5RunBackup() {
	GinkgoHelper()
	_, err := m3Apply(fmt.Sprintf(`apiVersion: crystalbackup.io/v1alpha1
kind: Backup
metadata:
  name: %[1]s
  namespace: %[2]s
  labels:
    crystalbackup.io/origin: namespace
spec:
  locationRef:
    kind: BackupLocation
    name: %[3]s
  run:
    includeManifests: true
    pvcSelector:
      exclude: ["*"]
`, m5BackupRun, m5TenantNS, m5SourceLocation))
	Expect(err).NotTo(HaveOccurred(), "apply the namespace-plane Backup")

	By("waiting for the Backup to complete with a manifests snapshot in the user's own repository")
	Eventually(func(g Gomega) {
		phase, err := kubectl("get", "backup", m5BackupRun, "-n", m5TenantNS, "-o", "jsonpath={.status.phase}")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(strings.TrimSpace(phase)).To(Equal("Completed"), "Backup not Completed yet")
		snap, err := kubectl("get", "backup", m5BackupRun, "-n", m5TenantNS,
			"-o", "jsonpath={.status.manifests.snapshotID}")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(strings.TrimSpace(snap)).NotTo(BeEmpty(), "the Backup captured no manifests snapshot")
	}, 10*time.Minute, 5*time.Second).Should(Succeed())
}

// m5SyncManifest renders a BackupExternalSync between two same-namespace locations.
func m5SyncManifest(name, source, destination, mode string) string {
	return fmt.Sprintf(`apiVersion: crystalbackup.io/v1alpha1
kind: BackupExternalSync
metadata:
  name: %[1]s
  namespace: %[2]s
spec:
  sourceLocationRef:
    name: %[3]s
  destinationLocationRef:
    name: %[4]s
  mode: %[5]s
`, name, m5TenantNS, source, destination, mode)
}

// m5JobContainer is the copy Job's container, reduced to what the direction assertions need.
type m5JobContainer struct {
	Image string
	Env   map[string]string
}

// m5CaptureCopyJob polls for the deterministic copy Job and returns its container the first time it
// exists.
//
// The Job is inspected WHILE IT RUNS because the driver deletes it as soon as the copy finishes
// (runSyncCopy's deferred cleanup) — there is no post-mortem to read. The poll is tight rather than
// racy: the Job exists from creation until the copy completes, which is a pod schedule plus a real
// restic run, and a 200 ms poll started before the sync object even exists cannot miss that window.
func m5CaptureCopyJob(jobName string, timeout time.Duration) m5JobContainer {
	GinkgoHelper()

	var raw string
	Eventually(func(g Gomega) {
		out, err := kubectl("get", "job", jobName, "-n", m5OperatorNS, "-o", "json")
		g.Expect(err).NotTo(HaveOccurred(),
			"the copy Job %s/%s has not appeared — the sync never reached the queue", m5OperatorNS, jobName)
		raw = out
	}, timeout, 200*time.Millisecond).Should(Succeed())

	var job struct {
		Spec struct {
			Template struct {
				Spec struct {
					Containers []struct {
						Image string `json:"image"`
						Env   []struct {
							Name  string `json:"name"`
							Value string `json:"value"`
						} `json:"env"`
					} `json:"containers"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
	}
	Expect(json.Unmarshal([]byte(raw), &job)).To(Succeed(), "decode copy Job %s", jobName)
	Expect(job.Spec.Template.Spec.Containers).NotTo(BeEmpty(), "copy Job %s has no container", jobName)

	c := job.Spec.Template.Spec.Containers[0]
	env := make(map[string]string, len(c.Env))
	for _, e := range c.Env {
		env[e.Name] = e.Value
	}
	return m5JobContainer{Image: c.Image, Env: env}
}

// m5WaitSyncPhase waits for a BackupExternalSync to report the given phase, surfacing the sync's own
// condition message when it does not.
func m5WaitSyncPhase(name, phase string, timeout time.Duration) {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		out, err := kubectl("get", "backupexternalsync", name, "-n", m5TenantNS,
			"-o", "jsonpath={.status.phase}")
		g.Expect(err).NotTo(HaveOccurred())
		if strings.TrimSpace(out) == phase {
			return
		}
		msg, _ := kubectl("get", "backupexternalsync", name, "-n", m5TenantNS,
			"-o", "jsonpath={.status.conditions[?(@.type=='SyncComplete')].message}")
		g.Expect(strings.TrimSpace(out)).To(Equal(phase),
			"sync %s is %q, not %q — %s", name, strings.TrimSpace(out), phase, strings.TrimSpace(msg))
	}, timeout, 3*time.Second).Should(Succeed())
}

// m5ForceCleanupCRs best-effort removes this container's CRs, clearing finalizers so nothing wedges
// a reused cluster (same reasoning as the M3 container's equivalent).
func m5ForceCleanupCRs() {
	for _, spec := range []struct{ kind, ns string }{
		{"backupexternalsync", m5TenantNS},
		{"backup", m5TenantNS},
		{"backuplocation", m5TenantNS},
		{"backuprepository", ""},
	} {
		nsArgs := []string{}
		if spec.ns != "" {
			nsArgs = []string{"-n", spec.ns}
		}
		names, err := kubectl(append([]string{"get", spec.kind}, append(nsArgs, "-o", "name")...)...)
		if err != nil {
			continue // CRD absent on a fresh cluster, or the list failed — nothing to reap
		}
		for _, n := range utils.GetNonEmptyLines(names) {
			n = strings.TrimSpace(n)
			// Delete FIRST, THEN clear finalizers, so a live operator cannot re-add one between
			// the two calls and leave the object stuck.
			_, _ = kubectl(append([]string{"delete", n}, append(nsArgs, "--ignore-not-found", "--wait=false")...)...)
			_, _ = kubectl(append([]string{"patch", n}, append(nsArgs,
				"--type=merge", "-p", `{"metadata":{"finalizers":null}}`)...)...)
		}
	}
}

// ---------------------------------------------------------------------------
// Specs
// ---------------------------------------------------------------------------

var _ = Describe("Crystal Backup external sync (M5)", Ordered, func() {
	BeforeAll(func() {
		m5ClusterID = fmt.Sprintf("e2e-m5-%d", time.Now().Unix())

		By("clearing any leftovers from a prior run (idempotent reused-cluster hygiene)")
		m5ForceCleanupCRs()
		_, _ = kubectl("delete", "namespace", m5TenantNS, "--ignore-not-found", "--wait=false")

		m3RemoveForeignOperators()
		m5UninstallOtherReleases()
		m5ClearUnownedChartObjects()
		m5DeployOperatorViaHelm()
		m5SeedTenant()
		m5CreateLocations()
		m5RunBackup()
	})

	AfterAll(func() {
		// Drain every CR with the operator still up — it is the only process that clears their
		// finalizers — and wait for the whole set, not just the tenant locations. Asserted at the
		// end so the release still gets uninstalled when a defect is found.
		defect := teardownCustomResources(3 * time.Minute)
		_, _ = kubectl("delete", "namespace", m5TenantNS, "--ignore-not-found", "--wait=false")

		By("uninstalling the Helm release (it owns the admission policies this container enabled)")
		_, _ = utils.RunWithTimeout(exec.Command("helm", "uninstall", m5Release,
			"--namespace", m5OperatorNS, "--wait", "--timeout", "2m"), 3*time.Minute)

		Expect(defect).To(BeEmpty(), defect)
	})

	AfterEach(func() {
		if !CurrentSpecReport().Failed() {
			return
		}
		By("dumping operator logs, sync Jobs and CR states for the failed spec")
		if out, err := kubectl("logs", "-n", m5OperatorNS, "deploy/"+m5OperatorDeploy, "--tail=300"); err == nil {
			_, _ = fmt.Fprintf(GinkgoWriter, "operator logs:\n%s\n", out)
		}
		if out, err := kubectl("get", "jobs", "-n", m5OperatorNS, "-o", "wide"); err == nil {
			_, _ = fmt.Fprintf(GinkgoWriter, "mover/sync Jobs:\n%s\n", out)
		}
		if out, err := kubectl("get", "backuplocation,backup,backupexternalsync", "-n", m5TenantNS,
			"-o", "wide"); err == nil {
			_, _ = fmt.Fprintf(GinkgoWriter, "tenant CR states:\n%s\n", out)
		}
		if out, err := kubectl("get", "backuprepository", "-o", "wide"); err == nil {
			_, _ = fmt.Fprintf(GinkgoWriter, "repositories:\n%s\n", out)
		}
	})

	It("copies the namespace's snapshots into the second location, on the sync image, from source to destination", func() {
		By("Given the operator was told about a sync image distinct from the mover image")
		args, err := kubectl("get", "deploy", m5OperatorDeploy, "-n", m5OperatorNS,
			"-o", "jsonpath={.spec.template.spec.containers[0].args}")
		Expect(err).NotTo(HaveOccurred())
		Expect(args).To(ContainSubstring("--sync-image="+syncImage),
			"the chart must pass --sync-image; got args %s", args)
		Expect(syncImage).NotTo(Equal(moverImage),
			"the sync and mover images must be different tags, or the assertion below is vacuous")

		By("When a BackupExternalSync copies my-offsite into my-offsite-2")
		_, err = m3Apply(m5SyncManifest(m5SyncName, m5SourceLocation, m5DestLocation, "Mirror"))
		Expect(err).NotTo(HaveOccurred(), "apply the BackupExternalSync")
		DeferCleanup(func() {
			_, _ = kubectl("delete", "backupexternalsync", m5SyncName, "-n", m5TenantNS,
				"--ignore-not-found", "--wait=false")
		})

		By("Then the copy runs in a Job built on the SYNC image, not the mover image")
		// Captured in flight: the driver deletes the Job the moment the copy returns.
		job := m5CaptureCopyJob(m5SyncCopyJob, 5*time.Minute)
		Expect(job.Image).To(Equal(syncImage),
			"the copy Job must run the sync image (mover=%s) — rclone lives only in the sync image", moverImage)

		By("And the direction is destination-in--r, source-in---from-repo (adr/0013: reading -r as " +
			"'the repository I am working on' copies the secondary over the primary)")
		Expect(job.Env).To(HaveKeyWithValue("RESTIC_REPOSITORY",
			fmt.Sprintf("rclone:dst:%s/%s/%s", m5DestBucket, m5S3Prefix, m5ClusterID)),
			"RESTIC_REPOSITORY must be the DESTINATION, addressed through the dst rclone remote")
		Expect(job.Env).To(HaveKeyWithValue("RESTIC_FROM_REPOSITORY",
			fmt.Sprintf("rclone:src:%s/%s/%s", m5SourceBucket, m5S3Prefix, m5ClusterID)),
			"RESTIC_FROM_REPOSITORY must be the SOURCE, addressed through the src rclone remote")

		By("And each remote carries its OWN credentials — the one thing restic's single backend " +
			"configuration cannot express, and the reason rclone is in the picture at all")
		// The values themselves travel by secretKeyRef (they are never inline), so what is
		// assertable here is that the two remotes are configured independently and that neither
		// inherits the other's endpoint or account.
		Expect(job.Env).To(HaveKeyWithValue("RCLONE_CONFIG_SRC_ENDPOINT", m5S3Endpoint))
		Expect(job.Env).To(HaveKeyWithValue("RCLONE_CONFIG_DST_ENDPOINT", m5S3Endpoint))
		Expect(job.Env).To(HaveKeyWithValue("RCLONE_CONFIG", "/dev/null"),
			"the remotes must come from environment alone; no config file may redefine src or dst")
		// Without this, rclone probes and would CREATE the bucket before its first upload — which
		// the destination's bucket-scoped identity has no right to do, so the copy would fail on a
		// bucket that exists. It is also what keeps the rclone spelling behaving like the s3: one,
		// which never creates a bucket either.
		Expect(job.Env).To(HaveKeyWithValue("RCLONE_CONFIG_SRC_NO_CHECK_BUCKET", "true"))
		Expect(job.Env).To(HaveKeyWithValue("RCLONE_CONFIG_DST_NO_CHECK_BUCKET", "true"))

		By("And the sync completes with the source's snapshots at the destination and zero lag")
		m5WaitSyncPhase(m5SyncName, "Completed", 15*time.Minute)

		copied, err := kubectl("get", "backupexternalsync", m5SyncName, "-n", m5TenantNS,
			"-o", "jsonpath={.status.snapshotsCopied}")
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.TrimSpace(copied)).NotTo(BeElementOf("", "0"),
			"the sync reports no copied snapshot, so nothing reached the second location")

		lag, err := kubectl("get", "backupexternalsync", m5SyncName, "-n", m5TenantNS,
			"-o", "jsonpath={.status.lagSnapshots}")
		Expect(err).NotTo(HaveOccurred())
		// An absent field is the zero value; kubectl renders it as an empty string.
		Expect(strings.TrimSpace(lag)).To(BeElementOf("", "0"),
			"the destination is behind the source (lag=%s) after a completed sync", strings.TrimSpace(lag))

		By("And that copy was a genuine CROSS-ACCOUNT one: the destination bucket is the only " +
			"bucket the second identity may touch, so a copy that collapsed both remotes onto one " +
			"credential set would have been denied rather than silently succeeding")
		for _, pair := range []struct{ location, secret string }{
			{m5SourceLocation, m5SourceCredsSecret},
			{m5DestLocation, m5DestCredsSecret},
		} {
			ref, err := kubectl("get", "backuplocation", pair.location, "-n", m5TenantNS,
				"-o", "jsonpath={.spec.s3.credentialsSecretRef.name}")
			Expect(err).NotTo(HaveOccurred())
			Expect(strings.TrimSpace(ref)).To(Equal(pair.secret))
		}
	})

	It("denies a sync whose source and destination are the same location (rule 9)", func() {
		By("Given the chart-rendered admission policy set is installed with the release")
		// failurePolicy is Fail, but a freshly applied policy takes a moment to compile — retry
		// until the DENIAL is observed rather than asserting on the first attempt.
		Eventually(func(g Gomega) {
			out, err := m3Apply(m5SyncManifest("m5-self-sync", m5SourceLocation, m5SourceLocation, "Mirror"))
			if err == nil {
				_, _ = kubectl("delete", "backupexternalsync", "m5-self-sync", "-n", m5TenantNS,
					"--ignore-not-found", "--wait=false")
			}
			g.Expect(err).To(HaveOccurred(),
				"a sync onto its own location must be denied, got: %s", out)
			g.Expect(err.Error()).To(ContainSubstring("must differ"))
		}, 3*time.Minute, 3*time.Second).Should(Succeed())
	})

	It("cannot reach out of its own namespace or onto a ClusterBackupLocation (rule 2, structural)", func() {
		By("Given a cluster-scoped location exists with a name a tenant might try to borrow")
		// It never becomes usable (no KEK, no credentials in the operator namespace) and it does
		// not need to: what is under test is whether a namespaced sync can RESOLVE it at all.
		const clusterLocation = "m5-cluster-only"
		_, err := m3Apply(fmt.Sprintf(`apiVersion: crystalbackup.io/v1alpha1
kind: ClusterBackupLocation
metadata:
  name: %s
spec:
  mode: Standard
  clusterID: %s
  s3:
    endpoint: %s
    bucket: %s
    prefix: %s
    credentialsSecretRef:
      name: nonexistent-platform-creds
  encryption:
    clusterKEKSecretRef:
      name: nonexistent-platform-kek
`, clusterLocation, m5ClusterID, m5S3Endpoint, m5DestBucket, m5S3Prefix))
		Expect(err).NotTo(HaveOccurred(), "apply the cluster-scoped decoy location")
		DeferCleanup(func() {
			_, _ = kubectl("delete", "clusterbackuplocation", clusterLocation,
				"--ignore-not-found", "--wait=false")
			_, _ = kubectl("patch", "clusterbackuplocation", clusterLocation,
				"--type=merge", "-p", `{"metadata":{"finalizers":null}}`)
		})

		By("When a BackupExternalSync names it as its destination")
		_, err = m3Apply(m5SyncManifest("m5-crossplane", m5SourceLocation, clusterLocation, "Mirror"))
		Expect(err).NotTo(HaveOccurred(),
			"admission has nothing to say here — the confinement is structural, not a policy")
		DeferCleanup(func() {
			_, _ = kubectl("delete", "backupexternalsync", "m5-crossplane", "-n", m5TenantNS,
				"--ignore-not-found", "--wait=false")
		})

		By("Then the name resolves as a BackupLocation IN THE SYNC'S OWN NAMESPACE, and there is none")
		// This is what "structural" means in practice: both refs are LocalObjectReference, so the
		// only namespace the controller can look in is the CR's own. The cluster-scoped object of
		// that name is not merely rejected — it is unreachable, because no field can name it.
		Eventually(func(g Gomega) {
			phase, err := kubectl("get", "backupexternalsync", "m5-crossplane", "-n", m5TenantNS,
				"-o", "jsonpath={.status.phase}")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(phase)).To(Equal("Pending"))
			msg, err := kubectl("get", "backupexternalsync", "m5-crossplane", "-n", m5TenantNS,
				"-o", "jsonpath={.status.conditions[?(@.type=='Ready')].message}")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(msg).To(ContainSubstring("does not exist in namespace "+`"`+m5TenantNS+`"`),
				"the destination must have been looked for in the tenant's OWN namespace; got %q", msg)
		}, 3*time.Minute, 3*time.Second).Should(Succeed())

		By("And no copy Job was ever created for it")
		out, _ := kubectl("get", "job", "bes-m5-crossplane-copy", "-n", m5OperatorNS,
			"--ignore-not-found", "-o", "name")
		Expect(strings.TrimSpace(out)).To(BeEmpty(),
			"an unresolved sync must not reach the queue at all")
	})
})
