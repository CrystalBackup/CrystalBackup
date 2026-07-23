//go:build e2e

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
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/CrystalBackup/CrystalBackup/test/utils"
)

// M2 admission on a REAL cluster (spec/08-testing §4 case 4's admission half + adr/0010):
// the chart-rendered ValidatingAdmissionPolicy set is applied to Kind and exercised through
// kubectl exactly as a user would hit it — the client-side denial the e2e catalogue demands
// — and the operator's dynamic single-default webhook (deployed by `make deploy` with its
// cert-manager certificate) rejects a second default location while the running controller
// parks an unconfirmed Restore in AwaitingConfirmation (R23's full round trip: VAP admits
// the empty confirmation, the CONTROLLER refuses to run without it).
//
// The data-path restore scenarios (modes × selection byte-verification, the R14 storage-
// level negative, ClusterRestore reconstitution) are crucible specs (test/crucible, label
// m2) per this project's M1 precedent: real CSI + real S3, not kind+hostpath.
//
// Rule 7 (denied namespaces) is exercised in envtest (test/admission), not here: its
// paramRef ConfigMap lives in the CHART's namespace, and this suite deploys via kustomize
// whose namespace may differ — asserting a fail-open paramRef would test nothing.
var _ = Describe("Crystal Backup admission (M2)", Ordered, func() {
	const tenantNS = "e2e-m2-tenant"

	// applyStdin pipes a manifest into kubectl (create/apply/delete per verb).
	applyStdin := func(manifest string, verb ...string) (string, error) {
		args := append(verb, "-f", "-")
		cmd := exec.Command("kubectl", args...)
		cmd.Stdin = strings.NewReader(manifest)
		return utils.Run(cmd)
	}

	BeforeAll(func() {
		deployOperatorFresh()

		// The chart renders rule 7's paramRef ConfigMap into the chart's canonical namespace
		// (crystal-backup-system) — this suite deploys via kustomize into a prefixed one, so
		// the canonical namespace must exist for the apply to land. Idempotent.
		By("ensuring the chart's canonical namespace exists for the paramRef ConfigMap")
		_, _ = kubectl("create", "namespace", "crystal-backup-system")

		By("installing the chart-rendered ValidatingAdmissionPolicy set")
		render := exec.Command("helm", "template", "e2e", filepath.Join("charts", "crystal-backup"),
			"--show-only", "templates/admission.yaml")
		out, err := utils.Run(render)
		Expect(err).NotTo(HaveOccurred(), "helm template must render the admission objects")
		_, err = applyStdin(out, "apply")
		Expect(err).NotTo(HaveOccurred(), "applying the VAP set")

		By("creating the tenant namespace")
		_, err = kubectl("create", "namespace", tenantNS)
		Expect(err).NotTo(HaveOccurred())
	})

	AfterAll(func() {
		// --wait=false on the resource deletes: a parked Restore holds a teardown finalizer and
		// a ClusterBackupLocation holds FinalizerLocation, so a default delete (and a namespace
		// delete, which waits for every contained object) would block on the controller. Best-
		// effort teardown must not hang the suite.
		_, _ = kubectl("delete", "namespace", tenantNS, "--ignore-not-found", "--wait=false")
		_, _ = kubectl("delete", "clusterbackuplocation", "--all", "--ignore-not-found", "--wait=false")
		_, _ = kubectl("delete", "validatingadmissionpolicybinding", "-l", "app.kubernetes.io/name=crystal-backup", "--ignore-not-found")
		_, _ = kubectl("delete", "validatingadmissionpolicy", "-l", "app.kubernetes.io/name=crystal-backup", "--ignore-not-found")
	})

	restoreManifest := func(name, mode, confirmation string) string {
		return fmt.Sprintf(`apiVersion: crystalbackup.io/v1alpha1
kind: Restore
metadata:
  name: %s
  namespace: %s
spec:
  source:
    backup: run-e2e
  mode: %s
  confirmation: %q
`, name, tenantNS, mode, confirmation)
	}

	It("denies a Recreate restore whose confirmation does not name the namespace (rule 1, R23)", func() {
		Eventually(func(g Gomega) {
			out, err := applyStdin(restoreManifest("m2-wrong-confirmation", "Recreate", "not-the-namespace"), "create")
			if err == nil {
				// The policy may still be compiling right after apply: remove and retry.
				// --wait=false is REQUIRED here: the Restore controller adds a teardown
				// finalizer on its first reconcile (internal apiconst.FinalizerRestore), so a
				// default `kubectl delete` blocks until that finalizer clears — an unbounded
				// wait that previously wedged the whole suite. Fire-and-forget: admission
				// (VAP) runs before storage, so the retried create is still denied even while
				// a prior object lingers in Terminating.
				_, _ = kubectl("delete", "restore", "-n", tenantNS, "m2-wrong-confirmation", "--ignore-not-found", "--wait=false")
			}
			g.Expect(err).To(HaveOccurred(), "a wrong confirmation must be denied, got: %s", out)
			g.Expect(err.Error()).To(ContainSubstring("confirmation"))
		}).Should(Succeed())
	})

	It("admits an empty confirmation and the controller parks the Restore in AwaitingConfirmation", func() {
		_, err := applyStdin(restoreManifest("m2-parked", "Overwrite", ""), "create")
		Expect(err).NotTo(HaveOccurred(), "an empty confirmation is admitted (it parks, it does not deny)")

		Eventually(func(g Gomega) {
			out, err := kubectl("get", "restore", "-n", tenantNS, "m2-parked", "-o", "jsonpath={.status.phase}")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(out)).To(Equal("AwaitingConfirmation"))
		}).Should(Succeed(), "the running controller must hold the unconfirmed restore (R23)")
	})

	It("denies a ClusterBackup whose namespace selector sets no positive form (rule 8)", func() {
		manifest := `apiVersion: crystalbackup.io/v1alpha1
kind: ClusterBackup
metadata:
  name: m2-shapeless
spec:
  locationRef:
    name: dr-e2e
`
		Eventually(func(g Gomega) {
			out, err := applyStdin(manifest, "create")
			if err == nil {
				// --wait=false: don't block on the controller's finalizer teardown (see rule 1).
				_, _ = kubectl("delete", "clusterbackup", "m2-shapeless", "--ignore-not-found", "--wait=false")
			}
			g.Expect(err).To(HaveOccurred(), "a selector-less ClusterBackup must be denied, got: %s", out)
			g.Expect(err.Error()).To(ContainSubstring("positive"))
		}).Should(Succeed())
	})

	It("denies an Immutable location that schedules a prune (rule 6)", func() {
		manifest := `apiVersion: crystalbackup.io/v1alpha1
kind: ClusterBackupLocation
metadata:
  name: m2-immutable-prune
spec:
  mode: Immutable
  clusterID: e2e
  s3:
    endpoint: https://s3.invalid
    bucket: b
    credentialsSecretRef: {name: s3}
  encryption:
    clusterKEKSecretRef: {name: kek}
  maintenance:
    pruneSchedule: "0 3 * * *"
`
		Eventually(func(g Gomega) {
			out, err := applyStdin(manifest, "create")
			if err == nil {
				// --wait=false: don't block on the controller's finalizer teardown (see rule 1).
				_, _ = kubectl("delete", "clusterbackuplocation", "m2-immutable-prune", "--ignore-not-found", "--wait=false")
			}
			g.Expect(err).To(HaveOccurred(), "Immutable+prune must be denied, got: %s", out)
			g.Expect(err.Error()).To(ContainSubstring("prune"))
		}).Should(Succeed())
	})

	It("rejects a second default ClusterBackupLocation through the dynamic webhook (rule 4)", func() {
		locationManifest := func(name string, isDefault bool) string {
			return fmt.Sprintf(`apiVersion: crystalbackup.io/v1alpha1
kind: ClusterBackupLocation
metadata:
  name: %s
spec:
  default: %t
  clusterID: e2e
  s3:
    endpoint: https://s3.invalid
    bucket: b
    credentialsSecretRef: {name: s3}
  encryption:
    clusterKEKSecretRef: {name: kek}
`, name, isDefault)
		}

		By("creating the first default location")
		_, err := applyStdin(locationManifest("m2-default-a", true), "create")
		Expect(err).NotTo(HaveOccurred())

		By("watching the webhook deny a competing default (fail-open until it serves)")
		// failurePolicy is Ignore by design (adr/0010): until the webhook endpoint serves,
		// a second default may slip through — delete it and retry until the DENIAL is
		// observed, which also proves the certificate chain and Service wiring work.
		Eventually(func(g Gomega) {
			out, err := applyStdin(locationManifest("m2-default-b", true), "create")
			if err == nil {
				// --wait=false: don't block on the controller's finalizer teardown (see rule 1).
				_, _ = kubectl("delete", "clusterbackuplocation", "m2-default-b", "--ignore-not-found", "--wait=false")
			}
			g.Expect(err).To(HaveOccurred(), "the second default must be denied once the webhook serves, got: %s", out)
			g.Expect(err.Error()).To(ContainSubstring("m2-default-a"))
		}, "3m", "2s").Should(Succeed())

		By("still admitting a non-default sibling")
		_, err = applyStdin(locationManifest("m2-secondary", false), "create")
		Expect(err).NotTo(HaveOccurred())
	})
})
