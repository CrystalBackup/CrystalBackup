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
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/CrystalBackup/CrystalBackup/test/utils"
)

// crdTestNamespace holds the namespaced CRs applied by the round-trip. Cluster-scoped CRs
// have no namespace; deleting this namespace only reclaims the namespaced ones, so the
// AfterAll also best-effort deletes the cluster-scoped CRs by name.
const crdTestNamespace = "crystalbackup-e2e-crd"

// crdRoundTripCase is a minimal, schema-valid CR for one of the 12 kinds. The manifests
// carry ONLY the fields the generated CRD marks required (parsed from config/crd/bases):
//   - the API group crystalbackup.io/v1alpha1 is the stable contract (spec/02-api.md), so
//     these do not depend on the kustomize namePrefix;
//   - referenced Secrets/locations need not exist: at M0 there is no controller, no admission
//     webhook and no ValidatingAdmissionPolicy (those arrive in M2), so a create only has to
//     satisfy the OpenAPI/structural schema. This keeps the round-trip decoupled from the
//     (empty) operator logic and from S3.
type crdRoundTripCase struct {
	kind       string // Kind, for readable spec names
	resource   string // plural, used as <resource>.crystalbackup.io for kubectl + the CRD name
	namespaced bool
	name       string // metadata.name
	manifest   string // full CR YAML
}

// kubectlApplyStdin pipes a manifest to `kubectl apply -f -`.
func kubectlApplyStdin(manifest string) (string, error) {
	cmd := exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(manifest)
	return utils.Run(cmd)
}

// The 12 CRDs of the cascade set (spec/02-api.md): 6 cluster-plane + 5 namespace-plane +
// the internal BackupRepository.
var crdRoundTripCases = []crdRoundTripCase{
	{
		kind: "ClusterBackupLocation", resource: "clusterbackuplocations", namespaced: false,
		name: "e2e-clusterbackuplocation",
		manifest: `apiVersion: crystalbackup.io/v1alpha1
kind: ClusterBackupLocation
metadata:
  name: e2e-clusterbackuplocation
spec:
  clusterID: e2e-cluster
  s3:
    endpoint: http://seaweedfs.crystalbackup-e2e.svc.cluster.local:8333
    bucket: e2e-dr
    credentialsSecretRef:
      name: e2e-s3
  encryption:
    clusterKEKSecretRef:
      name: e2e-cluster-kek
`,
	},
	{
		kind: "ClusterBackupSchedule", resource: "clusterbackupschedules", namespaced: false,
		name: "e2e-clusterbackupschedule",
		manifest: `apiVersion: crystalbackup.io/v1alpha1
kind: ClusterBackupSchedule
metadata:
  name: e2e-clusterbackupschedule
spec:
  schedule: "0 2 * * *"
  template:
    spec:
      locationRef:
        name: e2e-clusterbackuplocation
`,
	},
	{
		kind: "ClusterBackup", resource: "clusterbackups", namespaced: false,
		name: "e2e-clusterbackup",
		manifest: `apiVersion: crystalbackup.io/v1alpha1
kind: ClusterBackup
metadata:
  name: e2e-clusterbackup
spec:
  locationRef:
    name: e2e-clusterbackuplocation
`,
	},
	{
		kind: "ClusterRestore", resource: "clusterrestores", namespaced: false,
		name: "e2e-clusterrestore",
		manifest: `apiVersion: crystalbackup.io/v1alpha1
kind: ClusterRestore
metadata:
  name: e2e-clusterrestore
spec:
  source:
    locationRef:
      name: e2e-clusterbackuplocation
    namespace: tenant-a
  target:
    namespace: tenant-a-restored
`,
	},
	{
		kind: "ClusterErasure", resource: "clustererasures", namespaced: false,
		name: "e2e-clustererasure",
		manifest: `apiVersion: crystalbackup.io/v1alpha1
kind: ClusterErasure
metadata:
  name: e2e-clustererasure
spec:
  locationRef:
    name: e2e-clusterbackuplocation
  target:
    namespace: tenant-a
  confirmation: tenant-a
`,
	},
	{
		kind: "ClusterBackupExternalSync", resource: "clusterbackupexternalsyncs", namespaced: false,
		name: "e2e-clusterbackupexternalsync",
		manifest: `apiVersion: crystalbackup.io/v1alpha1
kind: ClusterBackupExternalSync
metadata:
  name: e2e-clusterbackupexternalsync
spec:
  sourceLocationRef:
    name: e2e-clusterbackuplocation
  destinationLocationRef:
    name: e2e-clusterbackuplocation-secondary
`,
	},
	{
		kind: "BackupLocation", resource: "backuplocations", namespaced: true,
		name: "e2e-backuplocation",
		manifest: `apiVersion: crystalbackup.io/v1alpha1
kind: BackupLocation
metadata:
  name: e2e-backuplocation
  namespace: crystalbackup-e2e-crd
spec:
  s3:
    endpoint: http://seaweedfs.crystalbackup-e2e.svc.cluster.local:8333
    bucket: tenant-a-backups
    credentialsSecretRef:
      name: e2e-offsite-s3
  encryption: {}
`,
	},
	{
		kind: "BackupSchedule", resource: "backupschedules", namespaced: true,
		name: "e2e-backupschedule",
		manifest: `apiVersion: crystalbackup.io/v1alpha1
kind: BackupSchedule
metadata:
  name: e2e-backupschedule
  namespace: crystalbackup-e2e-crd
spec:
  locationRef:
    name: e2e-backuplocation
  schedule: "0 1 * * *"
`,
	},
	{
		kind: "Backup", resource: "backups", namespaced: true,
		name: "e2e-backup",
		manifest: `apiVersion: crystalbackup.io/v1alpha1
kind: Backup
metadata:
  name: e2e-backup
  namespace: crystalbackup-e2e-crd
spec:
  locationRef:
    name: e2e-backuplocation
`,
	},
	{
		kind: "Restore", resource: "restores", namespaced: true,
		name: "e2e-restore",
		manifest: `apiVersion: crystalbackup.io/v1alpha1
kind: Restore
metadata:
  name: e2e-restore
  namespace: crystalbackup-e2e-crd
spec:
  source:
    backup: e2e-backup
`,
	},
	{
		kind: "BackupExternalSync", resource: "backupexternalsyncs", namespaced: true,
		name: "e2e-backupexternalsync",
		manifest: `apiVersion: crystalbackup.io/v1alpha1
kind: BackupExternalSync
metadata:
  name: e2e-backupexternalsync
  namespace: crystalbackup-e2e-crd
spec:
  sourceLocationRef:
    name: e2e-backuplocation
  destinationLocationRef:
    name: e2e-backuplocation-2
`,
	},
	{
		kind: "BackupRepository", resource: "backuprepositories", namespaced: false,
		name: "e2e-backuprepository",
		manifest: `apiVersion: crystalbackup.io/v1alpha1
kind: BackupRepository
metadata:
  name: e2e-backuprepository
spec: {}
`,
	},
}

var _ = Describe("CRD installation & round-trip (M0)", Ordered, func() {
	// Install CRDs (idempotent — the operator container may also install them) and create
	// the namespace for the namespaced CRs. This container does NOT deploy the operator: at
	// M0 every CRD must install and round-trip on its own, independent of controller logic.
	BeforeAll(func() {
		By("installing CRDs via `make install`")
		_, err := utils.Run(exec.Command("make", "install"))
		Expect(err).NotTo(HaveOccurred(), "Failed to install CRDs")

		By("creating the CRD round-trip namespace")
		_, _ = kubectl("create", "namespace", crdTestNamespace)
	})

	AfterAll(func() {
		By("deleting the namespaced round-trip CRs and their namespace")
		_, _ = kubectl("delete", "namespace", crdTestNamespace, "--ignore-not-found", "--wait=false")

		By("best-effort deleting any leftover cluster-scoped round-trip CRs")
		for _, c := range crdRoundTripCases {
			if c.namespaced {
				continue
			}
			_, _ = kubectl("delete", c.resource+".crystalbackup.io", c.name, "--ignore-not-found")
		}
	})

	roundTrip := func(c crdRoundTripCase) {
		By("waiting for the " + c.kind + " CRD to be Established")
		Eventually(func(g Gomega) {
			out, err := kubectl("get", "crd", c.resource+".crystalbackup.io",
				"-o", "jsonpath={.status.conditions[?(@.type=='Established')].status}")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(out)).To(Equal("True"))
		}, 2*time.Minute, 2*time.Second).Should(Succeed())

		By("applying a minimal valid " + c.kind)
		out, err := kubectlApplyStdin(c.manifest)
		Expect(err).NotTo(HaveOccurred(), "apply %s failed: %s", c.kind, out)

		By("reading the " + c.kind + " back (round-trip)")
		args := []string{"get", c.resource + ".crystalbackup.io", c.name, "-o", "jsonpath={.metadata.uid}"}
		if c.namespaced {
			args = append(args, "-n", crdTestNamespace)
		}
		uid, err := kubectl(args...)
		Expect(err).NotTo(HaveOccurred(), "get %s failed", c.kind)
		Expect(strings.TrimSpace(uid)).NotTo(BeEmpty(), "%s has no UID after apply", c.kind)

		By("deleting the " + c.kind)
		delArgs := []string{"delete", c.resource + ".crystalbackup.io", c.name, "--ignore-not-found"}
		if c.namespaced {
			delArgs = append(delArgs, "-n", crdTestNamespace)
		}
		_, err = kubectl(delArgs...)
		Expect(err).NotTo(HaveOccurred(), "delete %s failed", c.kind)
	}

	DescribeTable("each of the 12 CRDs installs and round-trips a minimal valid CR",
		roundTrip,
		Entry("ClusterBackupLocation", crdRoundTripCases[0]),
		Entry("ClusterBackupSchedule", crdRoundTripCases[1]),
		Entry("ClusterBackup", crdRoundTripCases[2]),
		Entry("ClusterRestore", crdRoundTripCases[3]),
		Entry("ClusterErasure", crdRoundTripCases[4]),
		Entry("ClusterBackupExternalSync", crdRoundTripCases[5]),
		Entry("BackupLocation", crdRoundTripCases[6]),
		Entry("BackupSchedule", crdRoundTripCases[7]),
		Entry("Backup", crdRoundTripCases[8]),
		Entry("Restore", crdRoundTripCases[9]),
		Entry("BackupExternalSync", crdRoundTripCases[10]),
		Entry("BackupRepository", crdRoundTripCases[11]),
	)
})
