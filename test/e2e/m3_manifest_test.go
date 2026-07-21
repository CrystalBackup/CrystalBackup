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

	"filippo.io/age"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/CrystalBackup/CrystalBackup/test/utils"
)

// M3 — namespace MANIFEST backup & restore on real infra (spec/04-manifest-backup.md §6,
// spec/08-testing-and-dod.md §4 case 9). This is the FIRST kind e2e that runs real mover Jobs:
// the operator is installed from the packaged Helm chart (the chart is under test — spec/08 §4),
// which is the ONLY deploy that wires the data path (--mover-image, the manifest-mover identity
// and its transient reader/writer ClusterRoles, and POD_NAMESPACE ⇒ operator namespace). The
// kustomize `make deploy` used by the M0/M2 containers wires none of that, so this container
// deploys independently into crystal-backup-system and tears itself down; Ginkgo runs top-level
// containers serially, so at most one operator is ever live.
//
// The round-trip: seed a demo namespace (Deployment + NodePort Service + ConfigMap + a
// CSI-hostpath-bound PVC), capture its manifests with a ClusterBackup, then restore in-namespace
// with a namespaced Restore (the L7 path — a namespaced Restore only ever restores into its OWN
// namespace; ClusterRestore does NOT rehydrate a namespace's own manifests, adr/0011). We prove
// the four §6 gates: workloads reach Ready, the Service nodePort is preserved, a pre-drifted
// ConfigMap is SSA-merged under Overwrite (target-only keys kept) and REPLACED under Recreate,
// and the R23 confirmation gate parks a destructive restore until spec.confirmation names the
// namespace.
//
// The backup here is MANIFEST-ONLY (pvcSelector excludes all PVCs, cluster capture disabled): the
// PVC is seeded + bound so its manifest is captured, but its DATA is not snapshotted — §6 is a
// manifest gate, and isolating it keeps the first milestone's Completed signal about manifests.

const (
	// m3OperatorNS is the Helm chart's fixed operator namespace: where the manager runs and
	// every cluster-plane platform Secret (KEK, DR S3 creds, wrapped DEK), mover Job and staging
	// PVC lives (POD_NAMESPACE ⇒ --operator-namespace; tenancy invariants I3/I5).
	m3OperatorNS = "crystal-backup-system"
	// m3Release is the Helm release name (object names are fixed to "crystal-backup", not
	// release-prefixed — the chart is a singleton, _helpers.tpl fullname).
	m3Release        = "crystal-backup-m3"
	m3OperatorDeploy = "crystal-backup" // fullname ⇒ Deployment/ServiceAccount base name

	m3DemoNS   = "m3-demo"
	m3Location = "dr-e2e-m3"
	m3Run      = "m3-run"
	m3CMName   = "web-config"
	m3SvcName  = "web"
	m3DeployN  = "web"
	m3PVCName  = "web-data"
	m3NodePort = 30080

	// Milestone 2 (case 16): cluster-scoped capture & selective restore.
	m3ClusterRun         = "m3-cluster-run"
	m3FixtureCRD         = "widgets.m3demo.example.com"
	m3FixtureSC          = "m3-fixture-sc"
	m3FixtureRole        = "m3-fixture-role"
	m3ClusterRebornNS    = "m3-cluster-reborn"
	m3ClusterRebornNS2   = "m3-cluster-reborn2"
	m3ClusterRestore     = "m3-cluster-restore"
	m3ClusterRestoreNone = "m3-cluster-none"

	m3KEKSecret = "m3-cluster-kek"
	m3S3Secret  = "m3-dr-s3"
	// m3DEKSecret is the operator-generated wrapped-DEK Secret for the location (keys.DEKSecretName
	// ⇒ "crystal-dek-<location>"). The suite deletes it on setup so a rerun's fresh KEK is never
	// asked to unwrap a DEK wrapped under a previous, now-deleted KEK.
	m3DEKSecret = "crystal-dek-" + m3Location
)

// m3ClusterID is the restic repository path segment (<prefix>/<clusterID>). It is UNIQUE per run
// so every run writes a fresh repository, making the suite hermetic on a reused cluster: no run
// re-inits or re-opens another run's repo, and a fresh KEK never meets a stale wrapped DEK. Set
// in BeforeAll.
var m3ClusterID string

// splitImageRef splits a "repo:tag" reference at its LAST colon (a repository may itself contain
// a colon in a registry:port host). Used to feed the Helm chart's repository/tag values, which
// it recombines — pointing --mover-image / the operator image at the tags loaded into Kind.
func splitImageRef(ref string) (repo, tag string) {
	if i := strings.LastIndex(ref, ":"); i >= 0 && !strings.Contains(ref[i+1:], "/") {
		return ref[:i], ref[i+1:]
	}
	return ref, "latest"
}

// m3Kubectl applies a manifest via `kubectl <verb> -f -` (reuses the suite's deadline-bounded Run).
func m3Apply(manifest string) (string, error) {
	cmd := exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(manifest)
	return utils.Run(cmd)
}

// m3RemoveForeignOperators deletes any controller-manager Deployment that is NOT this container's
// helm operator, and waits for its pods to disappear. The M0/M2 kustomize operator is left running
// by the M2 admission container's AfterAll (it undeploys nothing), and it DEFAULTS its
// operatorNamespace to crystal-backup-system — the very namespace this container's operator owns —
// so when Ginkgo's randomized order puts M2 before M3 that stale operator would CO-RECONCILE M3's
// ClusterBackupLocation / ClusterBackup / mover Jobs from a second manager that carries none of the
// mover/manifest wiring. Exactly one operator must drive M3, so evict the other one first. A later
// M0/M2 container simply redeploys its own via deployOperatorFresh.
func m3RemoveForeignOperators() {
	GinkgoHelper()
	out, _ := kubectl("get", "deploy", "-A", "-l", "control-plane=controller-manager",
		"-o", "jsonpath={range .items[*]}{.metadata.namespace}/{.metadata.name}{\"\\n\"}{end}")
	for _, line := range utils.GetNonEmptyLines(out) {
		ns, name, ok := strings.Cut(strings.TrimSpace(line), "/")
		if !ok || ns == m3OperatorNS {
			continue
		}
		_, _ = kubectl("delete", "deploy", name, "-n", ns, "--ignore-not-found", "--wait=false")
	}
	Eventually(func(g Gomega) {
		pods, _ := kubectl("get", "pods", "-A", "-l", "control-plane=controller-manager",
			"-o", "jsonpath={range .items[*]}{.metadata.namespace}{\"\\n\"}{end}")
		for _, ns := range utils.GetNonEmptyLines(pods) {
			g.Expect(strings.TrimSpace(ns)).To(Equal(m3OperatorNS), "a foreign operator is still running")
		}
	}, 2*time.Minute, 3*time.Second).Should(Succeed())
}

// m3DeployOperatorViaHelm installs the operator from the packaged chart with the data path fully
// wired and admission/network-policy disabled (M3 exercises the CONTROLLER's R23 gate and the
// data path, not admission — that is the M2 container's job — and kindnet does not enforce
// NetworkPolicy anyway). Idempotent: `helm upgrade --install` + a pre-created, baseline-labelled
// namespace so a rerun on a reused cluster just rolls the release.
func m3DeployOperatorViaHelm() {
	GinkgoHelper()

	By("installing CRDs (idempotent; the chart's crds/ are create-if-absent)")
	_, err := utils.Run(exec.Command("make", "install"))
	Expect(err).NotTo(HaveOccurred(), "Failed to install CRDs")

	By("ensuring the operator namespace exists with baseline PodSecurity (movers run runAsUser:0)")
	_, _ = kubectl("create", "namespace", m3OperatorNS)
	_, err = kubectl("label", "namespace", m3OperatorNS,
		"pod-security.kubernetes.io/enforce=baseline", "--overwrite")
	Expect(err).NotTo(HaveOccurred(), "labelling %s with baseline PSA", m3OperatorNS)

	opRepo, opTag := splitImageRef(managerImage)
	mvRepo, mvTag := splitImageRef(moverImage)

	By("helm upgrade --install the operator from charts/crystal-backup (data path wired)")
	helmArgs := []string{
		"upgrade", "--install", m3Release, filepath.Join("charts", "crystal-backup"),
		"--namespace", m3OperatorNS,
		// CRDs are managed by `make install` (kustomize/kubectl) above and by the other e2e
		// containers, all under the kubectl field manager. Helm v4 installs CRDs via server-side
		// apply under its OWN manager, which conflicts with that owner on an already-installed CRD
		// — so hand CRD ownership to kubectl exclusively and keep Helm off them.
		"--skip-crds",
		"--set", "namespace.create=false",
		// Point both images at the tags loaded into Kind; clear the digest pin so the tag wins.
		"--set", "image.digest=",
		"--set", "image.repository=" + opRepo,
		"--set", "image.tag=" + opTag,
		"--set", "image.pullPolicy=IfNotPresent",
		"--set", "mover.image.digest=",
		"--set", "mover.image.repository=" + mvRepo,
		"--set", "mover.image.tag=" + mvTag,
		// M3 does not test admission (M2 does) and kindnet ignores NetworkPolicy — disabling both
		// keeps this container's cluster-scoped footprint from colliding with the M2 container's
		// VAP set and avoids a webhook serving-cert dependency.
		"--set", "admission.vap.enabled=false",
		"--set", "admission.webhook.enabled=false",
		"--set", "networkPolicy.create=false",
		"--wait", "--timeout", "6m",
	}
	out, err := utils.RunWithTimeout(exec.Command("helm", helmArgs...), 8*time.Minute)
	Expect(err).NotTo(HaveOccurred(), "helm install failed: %s", out)

	By("waiting for the operator Deployment to be Available")
	_, err = kubectl("rollout", "status", "-n", m3OperatorNS, "deploy/"+m3OperatorDeploy, "--timeout=3m")
	Expect(err).NotTo(HaveOccurred(), "operator Deployment did not roll out")
}

// m3ProvisionPlatformSecrets creates (idempotently, via apply) the two cluster-plane Secrets the
// operator reads from its own namespace for a ClusterBackupLocation: the cluster KEK (an age
// X25519 identity under data key "identity") and the DR S3 credentials (AWS_* keys). The chart
// never creates Secrets — the admin's root of trust is provisioned out of band — so the suite
// mints a throwaway KEK here.
func m3ProvisionPlatformSecrets() {
	GinkgoHelper()

	id, err := age.GenerateX25519Identity()
	Expect(err).NotTo(HaveOccurred(), "generate age KEK identity")

	kekYAML, err := kubectl("create", "secret", "generic", m3KEKSecret, "-n", m3OperatorNS,
		"--from-literal=identity="+id.String(), "--dry-run=client", "-o", "yaml")
	Expect(err).NotTo(HaveOccurred(), "render KEK Secret")
	_, err = m3Apply(kekYAML)
	Expect(err).NotTo(HaveOccurred(), "apply KEK Secret")

	// Mirror the SeaweedFS admin identity baked into test/e2e/manifests/seaweedfs.yaml.
	s3YAML, err := kubectl("create", "secret", "generic", m3S3Secret, "-n", m3OperatorNS,
		"--from-literal=AWS_ACCESS_KEY_ID=crystalbackup",
		"--from-literal=AWS_SECRET_ACCESS_KEY=crystalbackup-secret",
		"--dry-run=client", "-o", "yaml")
	Expect(err).NotTo(HaveOccurred(), "render S3 Secret")
	_, err = m3Apply(s3YAML)
	Expect(err).NotTo(HaveOccurred(), "apply S3 Secret")
}

// m3CreateLocation creates the shared cluster-DR location against the in-cluster SeaweedFS bucket
// (e2e-dr, pre-created by the infra bootstrap) and waits for its repository to initialise — the
// first mover Job of the run (restic init), so a green here already proves the mover image works.
func m3CreateLocation() {
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
    enabled: true
    interval: 1m
`, m3Location, m3S3Secret, m3KEKSecret, m3ClusterID)
	_, err := m3Apply(manifest)
	Expect(err).NotTo(HaveOccurred(), "apply ClusterBackupLocation")

	By("waiting for the location to become Ready (repository initialised by a mover Job)")
	Eventually(func(g Gomega) {
		out, err := kubectl("get", "clusterbackuplocation", m3Location,
			"-o", "jsonpath={.status.conditions[?(@.type=='Ready')].status}")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(strings.TrimSpace(out)).To(Equal("True"),
			"location not Ready yet (repo init mover still running or S3 unreachable)")
	}, 6*time.Minute, 5*time.Second).Should(Succeed())
}

// m3SeedDemoNamespace seeds the demo namespace with the four kinds §6 exercises: a Deployment
// that reaches Ready, a fixed-nodePort Service, a ConfigMap, and a CSI-hostpath PVC bound by the
// Deployment mounting it. Waits until the Deployment is Available and the PVC Bound.
func m3SeedDemoNamespace() {
	GinkgoHelper()

	By("ensuring any prior demo namespace is fully gone before seeding")
	// A namespace still Terminating from the BeforeAll cleanup would swallow the fixtures: objects
	// applied into a Terminating namespace are immediately garbage-collected ("object has been
	// deleted"). Wait for it to disappear before recreating it.
	Eventually(func(g Gomega) {
		out, _ := kubectl("get", "namespace", m3DemoNS, "--ignore-not-found", "-o", "name")
		g.Expect(strings.TrimSpace(out)).To(BeEmpty(), "demo namespace still terminating")
	}, 2*time.Minute, 3*time.Second).Should(Succeed())

	manifest := fmt.Sprintf(`apiVersion: v1
kind: Namespace
metadata:
  name: %[1]s
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: %[2]s
  namespace: %[1]s
data:
  greeting: hello
  role: original
---
apiVersion: v1
kind: Service
metadata:
  name: %[3]s
  namespace: %[1]s
spec:
  type: NodePort
  selector:
    app: web
  ports:
    - name: http
      port: 80
      targetPort: 8080
      nodePort: %[4]d
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: %[5]s
  namespace: %[1]s
spec:
  accessModes: ["ReadWriteOnce"]
  storageClassName: csi-hostpath-sc
  resources:
    requests:
      storage: 1Gi
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: %[6]s
  namespace: %[1]s
spec:
  replicas: 1
  selector:
    matchLabels:
      app: web
  template:
    metadata:
      labels:
        app: web
    spec:
      securityContext:
        runAsNonRoot: true
        runAsUser: 1000
        fsGroup: 1000
        seccompProfile:
          type: RuntimeDefault
      containers:
        - name: app
          image: busybox:1.36
          command: ["sleep", "infinity"]
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities:
              drop: ["ALL"]
          volumeMounts:
            - name: data
              mountPath: /data
      volumes:
        - name: data
          persistentVolumeClaim:
            claimName: %[5]s
`, m3DemoNS, m3CMName, m3SvcName, m3NodePort, m3PVCName, m3DeployN)
	_, err := m3Apply(manifest)
	Expect(err).NotTo(HaveOccurred(), "apply demo namespace fixtures")

	By("waiting for the Deployment to be Available and the PVC Bound")
	_, err = kubectl("rollout", "status", "-n", m3DemoNS, "deploy/"+m3DeployN, "--timeout=3m")
	Expect(err).NotTo(HaveOccurred(), "demo Deployment did not become Available")
	Eventually(func(g Gomega) {
		out, err := kubectl("get", "pvc", m3PVCName, "-n", m3DemoNS, "-o", "jsonpath={.status.phase}")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(strings.TrimSpace(out)).To(Equal("Bound"), "PVC not Bound")
	}, 2*time.Minute, 3*time.Second).Should(Succeed())
}

// m3RunBackup creates a MANIFEST-ONLY ClusterBackup of the demo namespace and waits for the run
// to Complete AND the child Backup to report a captured manifests snapshot.
func m3RunBackup() {
	GinkgoHelper()
	manifest := fmt.Sprintf(`apiVersion: crystalbackup.io/v1alpha1
kind: ClusterBackup
metadata:
  name: %s
spec:
  locationRef:
    name: %s
  namespaces:
    matchNames: ["%s"]
  pvcSelector:
    exclude: ["*"]
  clusterResources:
    enabled: false
`, m3Run, m3Location, m3DemoNS)
	_, err := m3Apply(manifest)
	Expect(err).NotTo(HaveOccurred(), "apply ClusterBackup")

	By("waiting for the ClusterBackup run to reach Completed")
	Eventually(func(g Gomega) {
		out, err := kubectl("get", "clusterbackup", m3Run, "-o", "jsonpath={.status.phase}")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(strings.TrimSpace(out)).To(Equal("Completed"),
			"run not Completed yet (phase above); manifest mover still running or failed")
	}, 8*time.Minute, 5*time.Second).Should(Succeed())

	By("asserting the child Backup captured a manifests snapshot")
	Eventually(func(g Gomega) {
		snap, err := kubectl("get", "backup", m3Run, "-n", m3DemoNS,
			"-o", "jsonpath={.status.manifests.snapshotID}")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(strings.TrimSpace(snap)).NotTo(BeEmpty(), "child Backup has no manifests snapshot")
		count, err := kubectl("get", "backup", m3Run, "-n", m3DemoNS,
			"-o", "jsonpath={.status.manifests.resourceCount}")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(strings.TrimSpace(count)).NotTo(BeElementOf("", "0"),
			"child Backup manifests captured zero resources")
	}, 2*time.Minute, 3*time.Second).Should(Succeed())
}

// m3ConfigMapData returns the demo ConfigMap's data map.
func m3ConfigMapData(g Gomega) map[string]string {
	out, err := kubectl("get", "configmap", m3CMName, "-n", m3DemoNS, "-o", "json")
	g.Expect(err).NotTo(HaveOccurred())
	var cm struct {
		Data map[string]string `json:"data"`
	}
	g.Expect(json.Unmarshal([]byte(out), &cm)).To(Succeed())
	return cm.Data
}

// m3ServiceNodePort returns the demo Service's first port's nodePort.
func m3ServiceNodePort(g Gomega) string {
	out, err := kubectl("get", "service", m3SvcName, "-n", m3DemoNS,
		"-o", "jsonpath={.spec.ports[0].nodePort}")
	g.Expect(err).NotTo(HaveOccurred())
	return strings.TrimSpace(out)
}

// m3RestoreManifest builds a namespaced Restore that restores ONLY the three workload manifests
// (volumes: [] ⇒ no data) from the run's child Backup.
func m3RestoreManifest(name, mode, confirmation string) string {
	return fmt.Sprintf(`apiVersion: crystalbackup.io/v1alpha1
kind: Restore
metadata:
  name: %s
  namespace: %s
spec:
  source:
    backup: %s
  mode: %s
  confirmation: %q
  resources:
    - include: ["ConfigMap/%s", "Service/%s", "apps/Deployment/%s"]
  volumes: []
`, name, m3DemoNS, m3Run, mode, confirmation, m3CMName, m3SvcName, m3DeployN)
}

// m3WaitRestorePhase waits for a Restore to report the given phase.
func m3WaitRestorePhase(name, phase string, timeout time.Duration) {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		out, err := kubectl("get", "restore", name, "-n", m3DemoNS, "-o", "jsonpath={.status.phase}")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(strings.TrimSpace(out)).To(Equal(phase), "restore %s not %s yet", name, phase)
	}, timeout, 5*time.Second).Should(Succeed())
}

// m3SeedClusterFixtures creates the three cluster-scoped fixtures case 16 captures: a CRD, its
// StorageClass, and a NON-system: ClusterRole. All three are in the capture's curated default
// allow-list (ClusterAllowKinds); the system: prefix exclusion is why the ClusterRole name is
// plain.
func m3SeedClusterFixtures() {
	GinkgoHelper()
	manifest := fmt.Sprintf(`apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: %[1]s
spec:
  group: m3demo.example.com
  scope: Namespaced
  names:
    plural: widgets
    singular: widget
    kind: Widget
    listKind: WidgetList
  versions:
    - name: v1
      served: true
      storage: true
      schema:
        openAPIV3Schema:
          type: object
          properties:
            spec:
              type: object
              properties:
                size:
                  type: integer
---
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: %[2]s
provisioner: m3demo.example.com/fixture
reclaimPolicy: Delete
volumeBindingMode: Immediate
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: %[3]s
rules:
  - apiGroups: ["m3demo.example.com"]
    resources: ["widgets"]
    verbs: ["get", "list", "watch"]
`, m3FixtureCRD, m3FixtureSC, m3FixtureRole)
	_, err := m3Apply(manifest)
	Expect(err).NotTo(HaveOccurred(), "apply cluster-scoped fixtures")

	By("waiting for the fixture CRD to be Established")
	_, err = kubectl("wait", "--for=condition=Established", "crd/"+m3FixtureCRD, "--timeout=60s")
	Expect(err).NotTo(HaveOccurred(), "fixture CRD not Established")
}

// m3RunClusterBackup creates a ClusterBackup with cluster-scoped capture ENABLED (the plane
// default) and waits for it to Complete. Unlike the milestone-1 run it INCLUDES the demo PVC's
// data (via the CSI-hostpath snapshot path): a ClusterRestore resolves its source by the run's
// per-namespace DATA snapshots (RunNotFound otherwise), so the cluster-scoped restore this
// milestone exercises needs at least one data snapshot to exist even though it restores none of it.
func m3RunClusterBackup() {
	GinkgoHelper()
	manifest := fmt.Sprintf(`apiVersion: crystalbackup.io/v1alpha1
kind: ClusterBackup
metadata:
  name: %[1]s
spec:
  locationRef:
    name: %[2]s
  namespaces:
    matchNames: ["%[3]s"]
  clusterResources:
    enabled: true
`, m3ClusterRun, m3Location, m3DemoNS)
	_, err := m3Apply(manifest)
	Expect(err).NotTo(HaveOccurred(), "apply cluster-capture ClusterBackup")

	By("waiting for the cluster-capture run to reach Completed")
	Eventually(func(g Gomega) {
		out, err := kubectl("get", "clusterbackup", m3ClusterRun, "-o", "jsonpath={.status.phase}")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(strings.TrimSpace(out)).To(Equal("Completed"),
			"cluster-capture run not Completed yet")
	}, 8*time.Minute, 5*time.Second).Should(Succeed())
}

// m3ClusterRestoreManifest builds a ClusterRestore. include==nil ⇒ NO clusterResources selector
// (opt-out: nothing cluster-scoped restored); a non-nil include restores exactly those kinds.
// resources/volumes are always empty (this milestone restores no namespaced objects or data).
func m3ClusterRestoreManifest(name, targetNS, confirmation string, include []string) string {
	clusterBlock := ""
	if include != nil {
		var b strings.Builder
		b.WriteString("  clusterResources:\n    include:\n")
		for _, inc := range include {
			fmt.Fprintf(&b, "      - %q\n", inc)
		}
		clusterBlock = b.String()
	}
	return fmt.Sprintf(`apiVersion: crystalbackup.io/v1alpha1
kind: ClusterRestore
metadata:
  name: %[1]s
spec:
  source:
    locationRef:
      name: %[2]s
    namespace: %[3]s
    backup: %[4]s
  target:
    namespace: %[5]s
    createNamespace: true
  mode: Overwrite
  confirmation: %[6]q
  resources: []
  volumes: []
%[7]s`, name, m3Location, m3DemoNS, m3ClusterRun, targetNS, confirmation, clusterBlock)
}

// m3WaitClusterRestorePhase waits for a ClusterRestore to report the given phase.
func m3WaitClusterRestorePhase(name, phase string, timeout time.Duration) {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		out, err := kubectl("get", "clusterrestore", name, "-o", "jsonpath={.status.phase}")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(strings.TrimSpace(out)).To(Equal(phase), "clusterrestore %s not %s yet", name, phase)
	}, timeout, 5*time.Second).Should(Succeed())
}

// m3ForceCleanupCRs best-effort removes this container's CRs, clearing finalizers so nothing wedges
// a reused cluster (a location/backup whose operator was uninstalled before its finalizer cleared).
func m3ForceCleanupCRs() {
	for _, spec := range []struct{ kind, ns string }{
		{"restore", m3DemoNS},
		{"clusterrestore", ""},
		{"clusterbackup", ""},
		{"backup", m3DemoNS},
		{"clusterbackuplocation", ""},
		{"backuprepository", ""},
	} {
		nsArgs := []string{}
		if spec.ns != "" {
			nsArgs = []string{"-n", spec.ns}
		}
		names, err := kubectl(append([]string{"get", spec.kind}, append(nsArgs, "-o", "name")...)...)
		if err != nil {
			// CRD not installed yet (fresh cluster) or the list failed — nothing to reap. Skip
			// rather than parse the error text as if it were a list of resource names.
			continue
		}
		for _, n := range utils.GetNonEmptyLines(names) {
			n = strings.TrimSpace(n)
			// Delete FIRST (sets deletionTimestamp), THEN clear finalizers, so a still-running
			// operator cannot re-add a finalizer between the two and leave the object stuck.
			_, _ = kubectl(append([]string{"delete", n}, append(nsArgs, "--ignore-not-found", "--wait=false")...)...)
			_, _ = kubectl(append([]string{"patch", n}, append(nsArgs,
				"--type=merge", "-p", `{"metadata":{"finalizers":null}}`)...)...)
		}
	}
}

var _ = Describe("Crystal Backup manifest round-trip (M3)", Ordered, func() {
	BeforeAll(func() {
		// A fresh repository path per run keeps the suite hermetic on a reused cluster.
		m3ClusterID = fmt.Sprintf("e2e-%d", time.Now().Unix())

		By("clearing any leftovers from a prior run (idempotent reused-cluster hygiene)")
		m3ForceCleanupCRs()
		_, _ = kubectl("delete", "namespace", m3DemoNS, "--ignore-not-found", "--wait=false")
		// Drop the operator-generated wrapped DEK so a fresh KEK is never handed a DEK wrapped
		// under a previous KEK (would fail "identity did not match any of the recipients").
		_, _ = kubectl("delete", "secret", m3DEKSecret, "-n", m3OperatorNS, "--ignore-not-found")
		// Cluster-scoped milestone-2 fixtures + reborn namespaces from a prior run (hermetic reruns).
		_, _ = kubectl("delete", "crd", m3FixtureCRD, "--ignore-not-found", "--wait=false")
		_, _ = kubectl("delete", "storageclass", m3FixtureSC, "--ignore-not-found")
		_, _ = kubectl("delete", "clusterrole", m3FixtureRole, "--ignore-not-found")
		_, _ = kubectl("delete", "namespace", m3ClusterRebornNS, m3ClusterRebornNS2,
			"--ignore-not-found", "--wait=false")

		m3RemoveForeignOperators()
		m3DeployOperatorViaHelm()
		m3ProvisionPlatformSecrets()
		m3CreateLocation()
		m3SeedDemoNamespace()
		m3RunBackup()
	})

	AfterAll(func() {
		By("deleting the demo namespace and this run's CRs (finalizers cleared, operator still up)")
		m3ForceCleanupCRs()
		_, _ = kubectl("delete", "namespace", m3DemoNS, "--ignore-not-found", "--wait=false")
		// Give the operator a beat to drain finalizers before it is uninstalled.
		Eventually(func(g Gomega) {
			out, _ := kubectl("get", "clusterbackuplocation", m3Location, "--ignore-not-found", "-o", "name")
			g.Expect(strings.TrimSpace(out)).To(BeEmpty(), "location still terminating")
		}, 2*time.Minute, 5*time.Second).Should(Succeed())

		By("uninstalling the Helm release")
		_, _ = utils.RunWithTimeout(exec.Command("helm", "uninstall", m3Release,
			"--namespace", m3OperatorNS, "--wait", "--timeout", "2m"), 3*time.Minute)
		_, _ = kubectl("delete", "secret", m3KEKSecret, m3S3Secret, "-n", m3OperatorNS, "--ignore-not-found")
	})

	AfterEach(func() {
		if !CurrentSpecReport().Failed() {
			return
		}
		By("dumping operator logs, mover Jobs and demo events for the failed spec")
		if out, err := kubectl("logs", "-n", m3OperatorNS, "deploy/"+m3OperatorDeploy, "--tail=200"); err == nil {
			_, _ = fmt.Fprintf(GinkgoWriter, "operator logs:\n%s\n", out)
		}
		if out, err := kubectl("get", "jobs", "-n", m3OperatorNS); err == nil {
			_, _ = fmt.Fprintf(GinkgoWriter, "mover Jobs:\n%s\n", out)
		}
		if out, err := kubectl("get", "clusterbackup,backup,restore,clusterbackuplocation",
			"-A", "-o", "wide"); err == nil {
			_, _ = fmt.Fprintf(GinkgoWriter, "CR states:\n%s\n", out)
		}
		if out, err := kubectl("get", "events", "-n", m3DemoNS, "--sort-by=.lastTimestamp"); err == nil {
			_, _ = fmt.Fprintf(GinkgoWriter, "demo events:\n%s\n", out)
		}
	})

	It("parks a destructive Overwrite restore until R23 confirmation, then SSA-merges the drifted ConfigMap keeping target-only keys", func() {
		By("drifting the live ConfigMap: change a backed-up key and add a target-only one")
		_, err := kubectl("patch", "configmap", m3CMName, "-n", m3DemoNS, "--type=merge",
			"-p", `{"data":{"greeting":"DRIFTED","extra":"localonly"}}`)
		Expect(err).NotTo(HaveOccurred())

		By("creating the Overwrite Restore WITHOUT a confirmation — R23 must park it")
		_, err = m3Apply(m3RestoreManifest("m3-overwrite", "Overwrite", ""))
		Expect(err).NotTo(HaveOccurred())
		m3WaitRestorePhase("m3-overwrite", "AwaitingConfirmation", 3*time.Minute)

		By("confirming with the namespace name and waiting for completion")
		_, err = kubectl("patch", "restore", "m3-overwrite", "-n", m3DemoNS, "--type=merge",
			"-p", `{"spec":{"confirmation":"`+m3DemoNS+`"}}`)
		Expect(err).NotTo(HaveOccurred())
		m3WaitRestorePhase("m3-overwrite", "Completed", 8*time.Minute)

		By("asserting the ConfigMap was SSA-merged: backed-up keys healed, target-only key kept")
		Eventually(func(g Gomega) {
			data := m3ConfigMapData(g)
			g.Expect(data).To(HaveKeyWithValue("greeting", "hello"), "backed-up key must be healed")
			g.Expect(data).To(HaveKeyWithValue("role", "original"), "backed-up key must be present")
			g.Expect(data).To(HaveKeyWithValue("extra", "localonly"),
				"Overwrite must KEEP a target-only key the backup never knew")
		}, time.Minute, 3*time.Second).Should(Succeed())

		By("asserting the workload is Ready and the Service nodePort is preserved")
		_, err = kubectl("rollout", "status", "-n", m3DemoNS, "deploy/"+m3DeployN, "--timeout=2m")
		Expect(err).NotTo(HaveOccurred())
		Eventually(func(g Gomega) {
			g.Expect(m3ServiceNodePort(g)).To(Equal(fmt.Sprintf("%d", m3NodePort)))
		}, time.Minute, 3*time.Second).Should(Succeed())
	})

	It("Recreate replaces the drifted ConfigMap (target-only key removed) and preserves the nodePort across delete+recreate", func() {
		By("re-drifting the ConfigMap with a fresh target-only key")
		_, err := kubectl("patch", "configmap", m3CMName, "-n", m3DemoNS, "--type=merge",
			"-p", `{"data":{"greeting":"DRIFTED2","stray":"gone-after-recreate"}}`)
		Expect(err).NotTo(HaveOccurred())

		By("creating a confirmed Recreate Restore (delete-then-create)")
		_, err = m3Apply(m3RestoreManifest("m3-recreate", "Recreate", m3DemoNS))
		Expect(err).NotTo(HaveOccurred())
		m3WaitRestorePhase("m3-recreate", "Completed", 8*time.Minute)

		By("asserting the ConfigMap was REPLACED: exactly the backed-up keys, target-only key gone")
		Eventually(func(g Gomega) {
			data := m3ConfigMapData(g)
			g.Expect(data).To(HaveKeyWithValue("greeting", "hello"))
			g.Expect(data).To(HaveKeyWithValue("role", "original"))
			g.Expect(data).NotTo(HaveKey("stray"), "Recreate must drop target-only keys")
			g.Expect(data).NotTo(HaveKey("extra"), "Recreate must drop target-only keys")
		}, time.Minute, 3*time.Second).Should(Succeed())

		By("asserting the Service nodePort survived the delete+recreate and the workload is Ready")
		Eventually(func(g Gomega) {
			g.Expect(m3ServiceNodePort(g)).To(Equal(fmt.Sprintf("%d", m3NodePort)))
		}, 2*time.Minute, 3*time.Second).Should(Succeed())
		_, err = kubectl("rollout", "status", "-n", m3DemoNS, "deploy/"+m3DeployN, "--timeout=3m")
		Expect(err).NotTo(HaveOccurred())
	})

	// -----------------------------------------------------------------------------------------
	// Cluster-scoped capture & selective restore (spec/08-testing-and-dod.md §4 case 16, adr/0011).
	//
	// Reuses the operator + location from the outer BeforeAll. A second, cluster-capturing
	// ClusterBackup snapshots a kind=cluster-manifests tree holding a fixture CRD + its
	// StorageClass + a non-system: ClusterRole; a ClusterRestore with clusterResources.include
	// then SELECTIVELY recreates the CRD + StorageClass (not the ClusterRole), a restore that
	// OMITS clusterResources restores nothing cluster-scoped (opt-in), and R23 gates both.
	// -----------------------------------------------------------------------------------------
	Context("cluster-scoped capture & selective restore (case 16)", Ordered, func() {
		BeforeAll(func() {
			m3SeedClusterFixtures()
			m3RunClusterBackup()
		})

		AfterAll(func() {
			// Delete the cluster restores + the reborn namespaces + the fixtures BEFORE the outer
			// AfterAll uninstalls the operator, so finalizers can drain.
			_, _ = kubectl("delete", "clusterrestore", m3ClusterRestore, m3ClusterRestoreNone,
				"--ignore-not-found", "--wait=false")
			_, _ = kubectl("delete", "clusterbackup", m3ClusterRun, "--ignore-not-found", "--wait=false")
			_, _ = kubectl("delete", "namespace", m3ClusterRebornNS, m3ClusterRebornNS2,
				"--ignore-not-found", "--wait=false")
			_, _ = kubectl("delete", "crd", m3FixtureCRD, "--ignore-not-found", "--wait=false")
			_, _ = kubectl("delete", "storageclass", m3FixtureSC, "--ignore-not-found")
			_, _ = kubectl("delete", "clusterrole", m3FixtureRole, "--ignore-not-found")
		})

		It("captures cluster-scoped resources into a kind=cluster-manifests snapshot (R22)", func() {
			By("asserting the run reports a captured cluster-manifests snapshot")
			Eventually(func(g Gomega) {
				count, err := kubectl("get", "clusterbackup", m3ClusterRun,
					"-o", "jsonpath={.status.clusterResourcesCaptured}")
				g.Expect(err).NotTo(HaveOccurred())
				n := strings.TrimSpace(count)
				g.Expect(n).NotTo(BeElementOf("", "0"), "clusterResourcesCaptured not set")
				snap, err := kubectl("get", "clusterbackup", m3ClusterRun,
					"-o", "jsonpath={.status.clusterManifests.snapshotID}")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(strings.TrimSpace(snap)).NotTo(BeEmpty(), "clusterManifests snapshot missing")
			}, 2*time.Minute, 3*time.Second).Should(Succeed())
		})

		It("selectively recreates the CRD + StorageClass (not the ClusterRole), R23-gated", func() {
			By("deleting the fixtures so the restore must recreate them from the snapshot")
			_, _ = kubectl("delete", "crd", m3FixtureCRD, "--ignore-not-found", "--wait=false")
			_, _ = kubectl("delete", "storageclass", m3FixtureSC, "--ignore-not-found")
			_, _ = kubectl("delete", "clusterrole", m3FixtureRole, "--ignore-not-found")
			Eventually(func(g Gomega) {
				out, _ := kubectl("get", "crd", m3FixtureCRD, "--ignore-not-found", "-o", "name")
				g.Expect(strings.TrimSpace(out)).To(BeEmpty(), "fixture CRD still present")
			}, time.Minute, 3*time.Second).Should(Succeed())

			By("creating the selective ClusterRestore WITHOUT a confirmation — R23 must park it")
			_, err := m3Apply(m3ClusterRestoreManifest(m3ClusterRestore, m3ClusterRebornNS, "",
				[]string{
					"apiextensions.k8s.io/CustomResourceDefinition/" + m3FixtureCRD,
					"storage.k8s.io/StorageClass/" + m3FixtureSC,
				}))
			Expect(err).NotTo(HaveOccurred())
			m3WaitClusterRestorePhase(m3ClusterRestore, "AwaitingConfirmation", 3*time.Minute)

			By("confirming with the target namespace and waiting for completion")
			_, err = kubectl("patch", "clusterrestore", m3ClusterRestore, "--type=merge",
				"-p", `{"spec":{"confirmation":"`+m3ClusterRebornNS+`"}}`)
			Expect(err).NotTo(HaveOccurred())
			m3WaitClusterRestorePhase(m3ClusterRestore, "Completed", 8*time.Minute)

			By("asserting the CRD + StorageClass were recreated and the ClusterRole was NOT")
			Eventually(func(g Gomega) {
				out, err := kubectl("get", "crd", m3FixtureCRD, "-o", "name")
				g.Expect(err).NotTo(HaveOccurred(), "fixture CRD not recreated")
				g.Expect(strings.TrimSpace(out)).NotTo(BeEmpty())
			}, time.Minute, 3*time.Second).Should(Succeed())
			_, err = kubectl("get", "storageclass", m3FixtureSC)
			Expect(err).NotTo(HaveOccurred(), "fixture StorageClass not recreated")
			out, _ := kubectl("get", "clusterrole", m3FixtureRole, "--ignore-not-found", "-o", "name")
			Expect(strings.TrimSpace(out)).To(BeEmpty(),
				"ClusterRole must NOT be restored — it was not in clusterResources.include")
		})

		It("restores NOTHING cluster-scoped when clusterResources is omitted (opt-in)", func() {
			By("deleting the CRD again so a non-selective restore would have to recreate it to fail the gate")
			_, _ = kubectl("delete", "crd", m3FixtureCRD, "--ignore-not-found", "--wait=false")
			Eventually(func(g Gomega) {
				out, _ := kubectl("get", "crd", m3FixtureCRD, "--ignore-not-found", "-o", "name")
				g.Expect(strings.TrimSpace(out)).To(BeEmpty())
			}, time.Minute, 3*time.Second).Should(Succeed())

			By("creating a confirmed ClusterRestore with NO clusterResources selector")
			_, err := m3Apply(m3ClusterRestoreManifest(m3ClusterRestoreNone, m3ClusterRebornNS2,
				m3ClusterRebornNS2, nil))
			Expect(err).NotTo(HaveOccurred())
			m3WaitClusterRestorePhase(m3ClusterRestoreNone, "Completed", 8*time.Minute)

			By("asserting nothing cluster-scoped came back")
			Consistently(func(g Gomega) {
				out, _ := kubectl("get", "crd", m3FixtureCRD, "--ignore-not-found", "-o", "name")
				g.Expect(strings.TrimSpace(out)).To(BeEmpty(),
					"omitting clusterResources must restore nothing cluster-scoped")
			}, 15*time.Second, 5*time.Second).Should(Succeed())
		})
	})
})
