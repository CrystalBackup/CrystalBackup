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
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/CrystalBackup/CrystalBackup/test/utils"
)

// M4 — consistency hooks against a REAL kubelet (spec/08-testing-and-dod.md §4 case 7, R16).
//
// This is the only place the exec path is exercised end to end. envtest has no kubelet, so the
// unit suite substitutes a stub executor: the SPDY stream, the container resolution, the argv
// handling and — crucially — the operator's `pods/exec` `create` grant are all unproven until
// here. That grant was missing from config/rbac entirely until M4 (no kubebuilder marker existed,
// so `make manifests` could not notice), which is exactly the class of bug only a real cluster
// finds.
//
// The three properties under test are the ones the freeze window rests on:
//
//  1. Confinement — a Running pod in the same namespace mounting NONE of the run's PVCs is never
//     exec'd into.
//  2. onError: Fail aborts before any snapshot exists. A snapshot taken after a failed quiesce
//     would LOOK application-consistent without being so, and that is discovered at restore time.
//  3. The unfreeze survives the operator dying mid-window. Nothing about the window is held in
//     memory; a workload left frozen by a crashed operator is an outage the backup itself caused.
//
// The operator is installed from the packaged Helm chart (the only deploy that wires the data
// path) into the same fixed namespace as the M3 container, and torn down after — Ginkgo runs
// top-level containers serially, so at most one operator is ever live.

const (
	m4DemoNS   = "m4-demo"
	m4Location = "dr-e2e-m4"

	// m4DEKSecret is the operator-generated wrapped-DEK Secret for this location
	// (keys.DEKSecretName => "crystal-dek-<location>"). It MUST be dropped on setup, and the
	// reason is not hypothetical: this suite mints a FRESH KEK on every run, while `make e2e` only
	// runs cleanup-test-e2e AFTER the tests — so a failing run leaves its kind cluster standing and
	// the next one reuses it. The new KEK is then handed a DEK wrapped under the old one and the
	// repository never initialises ("identity did not match any of the recipients"), which surfaces
	// far downstream as a Backup stuck in Pending. M3 documents the same trap; this container
	// reused its provisioning without its hygiene.
	m4DEKSecret = "crystal-dek-" + m4Location

	// m4HookedPVC is the claim the hooked pod mounts, and the only one the runs select.
	m4HookedPVC = "hooked-data"
	// m4BystanderPVC is mounted by a second Running pod in the SAME namespace. It is never in a
	// run's selection, so its pod must never be exec'd into — the confinement assertion.
	m4BystanderPVC = "bystander-data"

	m4HookedPod     = "hooked-app"
	m4BystanderPod  = "bystander-app"
	m4HookContainer = "app"

	// m4SnapPVCs / m4SnapPod are the ONLY fixtures on snapshot-capable storage. They exist for the
	// crash spec alone, which needs a real pre->post window to step into.
	m4SnapPVCA = "snap-data-a"
	m4SnapPVCB = "snap-data-b"
	m4SnapPod  = "snap-app"

	// m4SnapClass is kind's CSI-hostpath class, the one with a VolumeSnapshotClass.
	m4SnapClass = "csi-hostpath-sc"
	// m4NoSnapClass is kind's built-in local-path class, which has NO VolumeSnapshotClass. A PVC on
	// it is reported Skipped/CSISnapshotUnsupported within a reconcile or two.
	//
	// Three of the four specs use it deliberately, and not merely for speed. They are about the
	// freeze WINDOW -- does the quiesce reach the right pod, does the release always follow -- and
	// a Skipped volume exercises the release's hardest case: the backup produced nothing for that
	// claim and the application was quiesced anyway. Standing the window's tests on a working data
	// path would also make every one of them fail whenever the data path did, which is how a
	// milestone's real finding arrives as four red specs pointing at the wrong thing.
	m4NoSnapClass = "standard"

	// m4MarkerDir is a writable emptyDir the hooks touch. A hook's only observable effect through
	// kubectl is what it leaves in the pod's filesystem, so the markers ARE the evidence that a
	// command ran inside that specific container.
	m4MarkerDir = "/hookmarks"
)

// m4ClusterID keeps the repository fresh per run, so a rerun on a reused kind cluster never meets
// a stale wrapped DEK. Same rule as m3ClusterID.
var m4ClusterID string

// m4PodManifest renders a long-lived pod mounting pvc. It runs `sleep`, so it is Running (a hook
// candidate) without needing an image with a server in it, and it declares an emptyDir the hooks
// write their markers into. Hook annotations are applied later with `kubectl annotate`, so one
// spec can exercise the annotation path without leaving it set for the others.
func m4PodManifest(name, pvc string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  name: %[1]s
  namespace: %[2]s
spec:
  restartPolicy: Never
  containers:
    - name: %[4]s
      image: busybox:1.36
      command: ["sh", "-c", "sleep 3600"]
      volumeMounts:
        - { name: data, mountPath: /data }
        - { name: marks, mountPath: %[5]s }
  volumes:
    - name: data
      persistentVolumeClaim: { claimName: %[3]s }
    - name: marks
      emptyDir: {}
`, name, m4DemoNS, pvc, m4HookContainer, m4MarkerDir)
}

// m4TwoClaimPodManifest renders a pod mounting two claims, for the crash spec: the controller
// advances ONE volume per reconcile, so two claims widen the pre->post window.
func m4TwoClaimPodManifest(name, pvcA, pvcB string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  name: %[1]s
  namespace: %[2]s
  labels: { app: snap }
spec:
  restartPolicy: Never
  containers:
    - name: %[5]s
      image: busybox:1.36
      command: ["sh", "-c", "sleep 3600"]
      volumeMounts:
        - { name: a, mountPath: /data-a }
        - { name: b, mountPath: /data-b }
        - { name: marks, mountPath: %[6]s }
  volumes:
    - name: a
      persistentVolumeClaim: { claimName: %[3]s }
    - name: b
      persistentVolumeClaim: { claimName: %[4]s }
    - name: marks
      emptyDir: {}
`, name, m4DemoNS, pvcA, pvcB, m4HookContainer, m4MarkerDir)
}

// m4PVCManifest renders a small claim on the given class. Small on purpose: these runs are about
// the hook boundary, and the upload that follows is incidental.
func m4PVCManifest(name, storageClass string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: PersistentVolumeClaim
metadata: { name: %s, namespace: %s }
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: %s
  resources: { requests: { storage: 64Mi } }
`, name, m4DemoNS, storageClass)
}

// m4MarkerExists reports whether a hook left its marker inside the pod. This is the ground truth
// for "the command really ran in that container" — status is the operator's account of what it
// did, the marker is the pod's.
func m4MarkerExists(pod, marker string) bool {
	out, err := kubectl("exec", "-n", m4DemoNS, pod, "-c", m4HookContainer,
		"--", "sh", "-c", "test -f "+m4MarkerDir+"/"+marker+" && echo yes || echo no")
	if err != nil {
		return false
	}
	return strings.TrimSpace(out) == "yes"
}

// m4HookEntries reads status.hooks off the run's child Backup. Returns nil while the Backup does
// not exist yet, so callers can poll on it.
func m4HookEntries(run string) []map[string]any {
	out, err := kubectl("get", "backup", run, "-n", m4DemoNS, "-o", "jsonpath={.status.hooks}")
	if err != nil || strings.TrimSpace(out) == "" {
		return nil
	}
	var entries []map[string]any
	if json.Unmarshal([]byte(out), &entries) != nil {
		return nil
	}
	return entries
}

// m4HooksInPhase filters status.hooks to one phase ("pre" or "post").
func m4HooksInPhase(run, phase string) []map[string]any {
	var out []map[string]any
	for _, e := range m4HookEntries(run) {
		if e["phase"] == phase {
			out = append(out, e)
		}
	}
	return out
}

// m4BackupDiag renders the Backup's phase and per-volume phases for a failure message.
//
// It exists because the first run of this suite failed with "hooks are empty" and that sentence
// contains no information: it cannot distinguish "the Backup was never created" from "the hooks
// never ran" from "the volumes never left the snapshot phase, so the release was never owed". The
// release is gated on the volumes, so the volumes belong in the message.
func m4BackupDiag(run string) string {
	phase, err := kubectl("get", "backup", run, "-n", m4DemoNS, "-o", "jsonpath={.status.phase}")
	if err != nil {
		return "no Backup " + m4DemoNS + "/" + run + " exists (the run never fanned out): " + err.Error()
	}
	vols, _ := kubectl("get", "backup", run, "-n", m4DemoNS,
		"-o", "jsonpath={range .status.volumes[*]}{.pvc}={.phase}({.reason}) {end}")
	hooks, _ := kubectl("get", "backup", run, "-n", m4DemoNS,
		"-o", "jsonpath={range .status.hooks[*]}{.phase}/{.pod}={.result} {end}")
	cond, _ := kubectl("get", "backup", run, "-n", m4DemoNS,
		"-o", "jsonpath={.status.conditions[?(@.type=='Ready')].message}")
	return fmt.Sprintf("Backup phase=%q volumes=[%s] hooks=[%s] ready=%q",
		strings.TrimSpace(phase), strings.TrimSpace(vols), strings.TrimSpace(hooks), strings.TrimSpace(cond))
}

// m4RunBackup creates a ClusterBackup over the named PVCs with the given hooks block. Manifests
// are off: this container is about the freeze window, and a manifest Job running alongside would
// only add ways for the run to be slow.
func m4RunBackup(run string, pvcs []string, hooksBlock string) {
	GinkgoHelper()
	manifest := fmt.Sprintf(`apiVersion: crystalbackup.io/v1alpha1
kind: ClusterBackup
metadata: { name: %[1]s }
spec:
  locationRef: { name: %[2]s }
  namespaces:
    matchNames: ["%[3]s"]
  pvcSelector:
    include: ["%[4]s"]
  includeManifests: false
  clusterResources:
    enabled: false
%[5]s
`, run, m4Location, m4DemoNS, strings.Join(pvcs, `","`), hooksBlock)
	_, err := m3Apply(manifest)
	Expect(err).NotTo(HaveOccurred(), "apply ClusterBackup %s", run)
	DeferCleanup(func() {
		_, _ = kubectl("delete", "clusterbackup", run, "--ignore-not-found", "--wait=false")
	})
}

var _ = Describe("M4 — consistency hooks (R16)", Ordered, func() {
	BeforeAll(func() {
		m4ClusterID = fmt.Sprintf("e2e-m4-%d", time.Now().Unix())

		By("clearing leftovers from a prior run (the cluster survives a failed run)")
		_, _ = kubectl("delete", "clusterbackuplocation", m4Location, "--ignore-not-found", "--wait=false")
		_, _ = kubectl("delete", "secret", m4DEKSecret, "-n", m3OperatorNS, "--ignore-not-found")
		for _, run := range []string{"m4-confine", "m4-annotation", "m4-onerror-fail", "m4-crash-window"} {
			_, _ = kubectl("delete", "clusterbackup", run, "--ignore-not-found", "--wait=false")
		}

		m3RemoveForeignOperators()
		m3DeployOperatorViaHelm()
		m3ProvisionPlatformSecrets()
		m4CreateLocation()
		m4SeedDemoNamespace()
	})

	AfterAll(func() {
		// Order matters and is not cosmetic: the CRs carry finalizers only the operator clears, so
		// uninstalling it first would strand the location and leave the namespace Terminating
		// forever (the M3.2 failure mode). Delete while the operator is still up, wait for the
		// location to actually go, and only then remove the release.
		By("deleting this container's CRs while the operator is still running")
		_, _ = kubectl("delete", "clusterbackuplocation", m4Location, "--ignore-not-found", "--wait=false")
		_, _ = kubectl("delete", "namespace", m4DemoNS, "--ignore-not-found", "--wait=false")
		Eventually(func(g Gomega) {
			out, _ := kubectl("get", "clusterbackuplocation", m4Location, "--ignore-not-found", "-o", "name")
			g.Expect(strings.TrimSpace(out)).To(BeEmpty(), "location still terminating")
		}, 2*time.Minute, 5*time.Second).Should(Succeed())

		By("uninstalling the Helm release")
		m4UninstallOperator()
	})

	It("execs the quiesce into the pod holding the backed-up data — and only that pod", func() {
		const run = "m4-confine"

		By("running a backup whose pre hook writes a marker inside the hooked pod")
		m4RunBackup(run, []string{m4HookedPVC}, `  hooks:
    pre:
      - podSelector: {}
        container: `+m4HookContainer+`
        command: ["touch", "`+m4MarkerDir+`/pre-ran"]
        timeout: 30s
        onError: Fail
    post:
      - podSelector: {}
        container: `+m4HookContainer+`
        command: ["touch", "`+m4MarkerDir+`/post-ran"]
        timeout: 30s`)

		By("the pre hook is recorded against the pod mounting the run's PVC")
		Eventually(func(g Gomega) {
			pre := m4HooksInPhase(run, "pre")
			g.Expect(pre).To(HaveLen(1), "expected exactly one pre-hook record — %s", m4BackupDiag(run))
			g.Expect(pre[0]["pod"]).To(Equal(m4HookedPod))
			g.Expect(pre[0]["container"]).To(Equal(m4HookContainer))
			g.Expect(pre[0]["result"]).To(Equal("Succeeded"))
			g.Expect(pre[0]["source"]).To(Equal("spec"))
		}, 5*time.Minute, 2*time.Second).Should(Succeed())

		By("the command really ran inside that container (the pod's own account, not the operator's)")
		Expect(m4MarkerExists(m4HookedPod, "pre-ran")).To(BeTrue(),
			"status says the hook succeeded but the container has no marker")

		By("the bystander pod was never touched")
		// It mounts a PVC this run does not back up, so it holds none of the captured data and is
		// not consistency-relevant. Exec'ing it would mean the operator's cluster-wide pods/exec
		// grant reaches workloads the backup has nothing to do with.
		Expect(m4MarkerExists(m4BystanderPod, "pre-ran")).To(BeFalse(),
			"a pod holding none of the backed-up data was exec'd into")

		By("the release still runs even though the claim could not be snapshotted at all")
		// The claim is on a class with no VolumeSnapshotClass, so the volume goes Skipped and the
		// backup produces NOTHING for it. The application was quiesced regardless, which is exactly
		// the case a release conditioned on success would strand.
		Eventually(func(g Gomega) {
			g.Expect(m4HooksInPhase(run, "post")).NotTo(BeEmpty(), "%s", m4BackupDiag(run))
		}, 6*time.Minute, 3*time.Second).Should(Succeed())
		Expect(m4MarkerExists(m4HookedPod, "post-ran")).To(BeTrue(),
			"status records a release the container never saw")
	})

	It("lets a pod's own annotation REPLACE the run's hook for that pod", func() {
		const run = "m4-annotation"

		By("annotating the hooked pod with its own quiesce command")
		// Velero's precedence, matched deliberately: the workload owner knows how to quiesce their
		// database better than the platform's blanket rule does, so the annotation wins — and it
		// REPLACES rather than merges, or a pod would be quiesced twice by two different commands.
		_, err := kubectl("annotate", "pod", m4HookedPod, "-n", m4DemoNS, "--overwrite",
			"crystalbackup.io/pre-backup-command=[\"touch\",\""+m4MarkerDir+"/from-annotation\"]",
			"crystalbackup.io/pre-backup-container="+m4HookContainer)
		Expect(err).NotTo(HaveOccurred(), "annotating the hooked pod")
		DeferCleanup(func() {
			_, _ = kubectl("annotate", "pod", m4HookedPod, "-n", m4DemoNS,
				"crystalbackup.io/pre-backup-command-",
				"crystalbackup.io/pre-backup-container-")
		})

		By("running a backup that ALSO declares a spec hook for the same pod")
		m4RunBackup(run, []string{m4HookedPVC}, `  hooks:
    honorAnnotations: true
    pre:
      - podSelector: {}
        container: `+m4HookContainer+`
        command: ["touch", "`+m4MarkerDir+`/from-spec"]
        timeout: 30s`)

		By("the annotation's command ran and is recorded as the source")
		Eventually(func(g Gomega) {
			pre := m4HooksInPhase(run, "pre")
			g.Expect(pre).To(HaveLen(1), "%s", m4BackupDiag(run))
			g.Expect(pre[0]["source"]).To(Equal("annotation"))
			g.Expect(pre[0]["result"]).To(Equal("Succeeded"))
		}, 5*time.Minute, 2*time.Second).Should(Succeed())
		Expect(m4MarkerExists(m4HookedPod, "from-annotation")).To(BeTrue())

		By("the spec's command did NOT also run — the annotation replaced it, it did not add to it")
		Expect(m4MarkerExists(m4HookedPod, "from-spec")).To(BeFalse(),
			"both the annotation and the spec hook ran; precedence must replace, not merge")
	})

	It("aborts before snapshotting when the quiesce fails with onError=Fail", func() {
		const run = "m4-onerror-fail"

		By("running a backup whose pre hook exits non-zero")
		m4RunBackup(run, []string{m4HookedPVC}, `  hooks:
    pre:
      - podSelector: {}
        container: `+m4HookContainer+`
        command: ["sh", "-c", "exit 7"]
        timeout: 30s
        onError: Fail`)

		By("the Backup fails on the quiesce and no volume ever leaves Pending")
		Eventually(func(g Gomega) {
			phase, err := kubectl("get", "backup", run, "-n", m4DemoNS, "-o", "jsonpath={.status.phase}")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(phase)).To(Equal("Failed"))

			pre := m4HooksInPhase(run, "pre")
			g.Expect(pre).NotTo(BeEmpty(), "%s", m4BackupDiag(run))
			g.Expect(pre[0]["result"]).To(Equal("Failed"))
			// The command's own stderr/exit is the diagnosis an operator reads first.
			g.Expect(pre[0]["message"]).NotTo(BeEmpty(), "a failed hook recorded no message")
		}, 5*time.Minute, 2*time.Second).Should(Succeed())

		By("no VolumeSnapshot was created for the run")
		// The whole point of the abort: a snapshot taken after a failed quiesce is a backup that
		// looks application-consistent and is not.
		out, err := kubectl("get", "volumesnapshot", "-n", m4DemoNS,
			"-o", "jsonpath={range .items[*]}{.metadata.name}{\"\\n\"}{end}")
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.TrimSpace(out)).To(BeEmpty(), "a snapshot was cut despite the quiesce failing")
	})

	It("still unfreezes when the operator is killed between the quiesce and the release", func() {
		const run = "m4-crash-window"

		By("running a backup whose release writes a marker")
		// This is the ONE spec that needs a working data path: it steps into the gap between the
		// quiesce and the release, and a Skipped volume closes that gap in a single reconcile. Both
		// snapshot-capable claims are in the run because the controller advances ONE volume per
		// reconcile, spending at least a backupPollInterval on each, so two volumes widen the window
		// to tens of seconds — deterministic, rather than a race against one fast snapshot.
		m4RunBackup(run, []string{m4SnapPVCA, m4SnapPVCB}, `  hooks:
    pre:
      - podSelector: { matchLabels: { app: snap } }
        container: `+m4HookContainer+`
        command: ["touch", "`+m4MarkerDir+`/frozen"]
        timeout: 30s
    post:
      - podSelector: { matchLabels: { app: snap } }
        container: `+m4HookContainer+`
        command: ["touch", "`+m4MarkerDir+`/thawed"]
        timeout: 30s`)

		By("waiting for the window to be OPEN: the quiesce is recorded and the release is not")
		// The controller persists the pre record and returns before creating any VolumeSnapshot,
		// precisely so this state is observable and durable. The next reconcile then spends at
		// least one backupPollInterval per volume waiting for ReadyToUse, which is the window we
		// step into.
		Eventually(func(g Gomega) {
			g.Expect(m4HooksInPhase(run, "pre")).NotTo(BeEmpty(), "quiesce not recorded yet — %s", m4BackupDiag(run))
			g.Expect(m4HooksInPhase(run, "post")).To(BeEmpty(), "the release already ran")
		}, 5*time.Minute, 250*time.Millisecond).Should(Succeed())

		By("killing the operator mid-window")
		// Scale to zero rather than delete a pod: a deleted pod is replaced immediately, so the
		// window would close under a controller that never actually stopped. At zero replicas the
		// absence assertion below is meaningful.
		_, err := kubectl("scale", "-n", m3OperatorNS, "deploy/"+m3OperatorDeploy, "--replicas=0")
		Expect(err).NotTo(HaveOccurred(), "scaling the operator down")
		Eventually(func(g Gomega) {
			out, err := kubectl("get", "deploy", m3OperatorDeploy, "-n", m3OperatorNS,
				"-o", "jsonpath={.status.replicas}")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(out)).To(Or(Equal("0"), BeEmpty()))
		}, 2*time.Minute, 2*time.Second).Should(Succeed())

		By("the application is left quiesced with nothing running to release it")
		Expect(m4MarkerExists(m4SnapPod, "frozen")).To(BeTrue(), "the quiesce never reached the pod")
		Expect(m4MarkerExists(m4SnapPod, "thawed")).To(BeFalse(),
			"the release ran before the operator was stopped — the window was not actually open")
		Expect(m4HooksInPhase(run, "post")).To(BeEmpty())

		By("bringing a FRESH operator process up")
		_, err = kubectl("scale", "-n", m3OperatorNS, "deploy/"+m3OperatorDeploy, "--replicas=1")
		Expect(err).NotTo(HaveOccurred(), "scaling the operator back up")
		_, err = kubectl("rollout", "status", "-n", m3OperatorNS, "deploy/"+m3OperatorDeploy, "--timeout=3m")
		Expect(err).NotTo(HaveOccurred(), "operator did not come back")

		By("it reads status.hooks, sees a quiesce with no release, and thaws the application")
		// This is the feature's most important assertion. The new process shares nothing with the
		// old one but the Backup's status, so a release that happens here can only have been
		// derived from it.
		Eventually(func(g Gomega) {
			g.Expect(m4HooksInPhase(run, "post")).NotTo(BeEmpty(),
				"the release never ran after the restart — %s", m4BackupDiag(run))
		}, 6*time.Minute, 3*time.Second).Should(Succeed())
		Expect(m4MarkerExists(m4SnapPod, "thawed")).To(BeTrue(),
			"status records a release the container never saw")
	})
})

// m4CreateLocation mirrors m3CreateLocation against this container's own clusterID, so the two
// never share a repository.
func m4CreateLocation() {
	GinkgoHelper()
	manifest := fmt.Sprintf(`apiVersion: crystalbackup.io/v1alpha1
kind: ClusterBackupLocation
metadata:
  name: %[1]s
spec:
  mode: Standard
  clusterID: %[4]s
  s3:
    endpoint: http://seaweedfs.crystalbackup-e2e.svc.cluster.local:8333
    bucket: e2e-dr
    prefix: crystal
    region: us-east-1
    credentialsSecretRef:
      name: %[2]s
    forcePathStyle: true
  encryption:
    clusterKEKSecretRef:
      name: %[3]s
  discovery:
    enabled: false
`, m4Location, m3S3Secret, m3KEKSecret, m4ClusterID)
	_, err := m3Apply(manifest)
	Expect(err).NotTo(HaveOccurred(), "apply ClusterBackupLocation")

	By("waiting for the location to become Ready")
	Eventually(func(g Gomega) {
		out, err := kubectl("get", "clusterbackuplocation", m4Location,
			"-o", "jsonpath={.status.conditions[?(@.type=='Ready')].status}")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(strings.TrimSpace(out)).To(Equal("True"),
			"location not Ready yet (credentials or KEK unresolved, or S3 unreachable)")
	}, 6*time.Minute, 5*time.Second).Should(Succeed())

	By("waiting for its BackupRepository to report initialized")
	// A Ready LOCATION does not mean an initialised REPOSITORY: they are two objects, and the
	// location goes Ready on its credentials and key resolving, well before the restic-init mover
	// Job has run. A Backup gates on the repository, so waiting on the location alone pushed the
	// wait into the first spec, where it surfaced as "no hooks after 5 minutes" — a sentence that
	// points at the feature under test rather than at the setup that was not finished.
	Eventually(func(g Gomega) {
		repo, err := kubectl("get", "clusterbackuplocation", m4Location,
			"-o", "jsonpath={.status.repositoryRef}")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(strings.TrimSpace(repo)).NotTo(BeEmpty(), "location has no repositoryRef yet")

		init, err := kubectl("get", "backuprepository", strings.TrimSpace(repo),
			"-o", "jsonpath={.status.initialized}")
		g.Expect(err).NotTo(HaveOccurred())
		if strings.TrimSpace(init) != "true" {
			cond, _ := kubectl("get", "backuprepository", strings.TrimSpace(repo),
				"-o", "jsonpath={.status.conditions[*].message}")
			jobs, _ := kubectl("get", "jobs", "-n", m3OperatorNS, "-o",
				"jsonpath={range .items[*]}{.metadata.name}={.status.succeeded}/{.status.failed} {end}")
			g.Expect(strings.TrimSpace(init)).To(Equal("true"),
				"repository %s not initialized — conditions=%q jobs=[%s]",
				strings.TrimSpace(repo), strings.TrimSpace(cond), strings.TrimSpace(jobs))
		}
	}, 8*time.Minute, 5*time.Second).Should(Succeed())
}

// m4SeedDemoNamespace creates two claims and two Running pods: one mounting the claim the runs
// back up, one mounting a claim they never select. The second exists solely so confinement is
// falsifiable — without it, "only the right pod was exec'd into" is unobservable.
func m4SeedDemoNamespace() {
	GinkgoHelper()

	By("ensuring any prior demo namespace is fully gone before seeding")
	_, _ = kubectl("delete", "namespace", m4DemoNS, "--ignore-not-found", "--wait=true", "--timeout=3m")
	_, err := kubectl("create", "namespace", m4DemoNS)
	Expect(err).NotTo(HaveOccurred(), "create %s", m4DemoNS)

	for _, pvc := range []string{m4HookedPVC, m4BystanderPVC} {
		_, err = m3Apply(m4PVCManifest(pvc, m4NoSnapClass))
		Expect(err).NotTo(HaveOccurred(), "apply PVC %s", pvc)
	}
	for _, pvc := range []string{m4SnapPVCA, m4SnapPVCB} {
		_, err = m3Apply(m4PVCManifest(pvc, m4SnapClass))
		Expect(err).NotTo(HaveOccurred(), "apply PVC %s", pvc)
	}

	_, err = m3Apply(m4PodManifest(m4HookedPod, m4HookedPVC))
	Expect(err).NotTo(HaveOccurred(), "apply hooked pod")
	_, err = m3Apply(m4PodManifest(m4BystanderPod, m4BystanderPVC))
	Expect(err).NotTo(HaveOccurred(), "apply bystander pod")
	_, err = m3Apply(m4TwoClaimPodManifest(m4SnapPod, m4SnapPVCA, m4SnapPVCB))
	Expect(err).NotTo(HaveOccurred(), "apply snapshot-capable pod")

	By("waiting for every pod to be Running (a hook candidate must be Running)")
	for _, pod := range []string{m4HookedPod, m4BystanderPod, m4SnapPod} {
		_, err = kubectl("wait", "--for=condition=Ready", "pod/"+pod, "-n", m4DemoNS, "--timeout=4m")
		Expect(err).NotTo(HaveOccurred(), "pod %s never became Ready", pod)
	}
}

// m4UninstallOperator removes the Helm release this container installed (the same release name the
// M3 container uses — both go through m3DeployOperatorViaHelm), so a later container's operator is
// the only one reconciling. See m3RemoveForeignOperators for why two managers must never overlap.
func m4UninstallOperator() {
	_, _ = utils.RunWithTimeout(exec.Command("helm", "uninstall", m3Release,
		"--namespace", m3OperatorNS, "--wait", "--timeout", "2m"), 3*time.Minute)
	_, _ = kubectl("delete", "secret", m3KEKSecret, m3S3Secret, "-n", m3OperatorNS, "--ignore-not-found")
}
