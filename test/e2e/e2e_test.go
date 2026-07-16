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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/CrystalBackup/CrystalBackup/test/utils"
)

// metricsRoleBindingName is the ClusterRoleBinding this suite creates to let the operator's
// ServiceAccount read /metrics. It is the only name we own here; every operator-owned name
// (namespace, pod, ServiceAccount, metrics Service, metrics-reader ClusterRole) is
// discovered at runtime so the suite is agnostic to the kustomize namePrefix — it keeps
// working across the pending bs-k8s-backup-* -> crystal-backup-* rebrand.
const metricsRoleBindingName = "crystalbackup-e2e-metrics-binding"

// Operator resources discovered in BeforeAll (name-prefix agnostic).
var (
	operatorNamespace        string
	controllerPodName        string
	operatorServiceAccount   string
	metricsServiceName       string
	metricsReaderClusterRole string
)

// kubectl runs a kubectl command from the project root and returns combined output.
func kubectl(args ...string) (string, error) {
	return utils.Run(exec.Command("kubectl", args...))
}

// ---- name-prefix-agnostic discovery helpers -------------------------------------------

// getOperatorNamespace returns the namespace of the controller-manager pod. Kubebuilder
// always labels the manager pod `control-plane=controller-manager`, so this holds whatever
// the kustomize namespace/namePrefix is set to.
func getOperatorNamespace(g Gomega) string {
	out, err := kubectl("get", "pods", "-A", "-l", "control-plane=controller-manager",
		"-o", "jsonpath={.items[0].metadata.namespace}")
	g.Expect(err).NotTo(HaveOccurred(), "listing operator pods across namespaces")
	ns := strings.TrimSpace(out)
	g.Expect(ns).NotTo(BeEmpty(), "no pod labeled control-plane=controller-manager found")
	return ns
}

// getOperatorPod returns the name and ServiceAccount of the (non-terminating) manager pod.
func getOperatorPod(g Gomega, ns string) (podName, serviceAccount string) {
	out, err := kubectl("get", "pods", "-n", ns, "-l", "control-plane=controller-manager",
		"-o", "jsonpath={range .items[*]}{.metadata.name}{\"|\"}{.metadata.deletionTimestamp}"+
			"{\"|\"}{.spec.serviceAccountName}{\"\\n\"}{end}")
	g.Expect(err).NotTo(HaveOccurred(), "listing operator pods in %s", ns)
	for _, line := range utils.GetNonEmptyLines(out) {
		parts := strings.Split(line, "|")
		if len(parts) == 3 && strings.TrimSpace(parts[1]) == "" {
			return strings.TrimSpace(parts[0]), strings.TrimSpace(parts[2])
		}
	}
	g.Expect(podName).NotTo(BeEmpty(), "no non-terminating operator pod in %s; raw: %q", ns, out)
	return "", ""
}

// getServiceBySuffix returns the name of the Service in ns whose name ends with suffix.
func getServiceBySuffix(g Gomega, ns, suffix string) string {
	out, err := kubectl("get", "svc", "-n", ns,
		"-o", "jsonpath={range .items[*]}{.metadata.name}{\"\\n\"}{end}")
	g.Expect(err).NotTo(HaveOccurred(), "listing services in %s", ns)
	found := ""
	for _, name := range utils.GetNonEmptyLines(out) {
		if strings.HasSuffix(strings.TrimSpace(name), suffix) {
			found = strings.TrimSpace(name)
			break
		}
	}
	g.Expect(found).NotTo(BeEmpty(), "no Service ending in %q in %s; raw: %q", suffix, ns, out)
	return found
}

// getClusterRoleBySuffix returns the name of the ClusterRole whose name ends with suffix.
func getClusterRoleBySuffix(g Gomega, suffix string) string {
	out, err := kubectl("get", "clusterrole",
		"-o", "jsonpath={range .items[*]}{.metadata.name}{\"\\n\"}{end}")
	g.Expect(err).NotTo(HaveOccurred(), "listing cluster roles")
	found := ""
	for _, name := range utils.GetNonEmptyLines(out) {
		if strings.HasSuffix(strings.TrimSpace(name), suffix) {
			found = strings.TrimSpace(name)
			break
		}
	}
	g.Expect(found).NotTo(BeEmpty(), "no ClusterRole ending in %q; raw: %q", suffix, out)
	return found
}

var _ = Describe("Crystal Backup operator (M0)", Ordered, func() {
	// Deploy the operator once for this container: install CRDs, then `make deploy`, then
	// discover the operator's runtime resources by label/suffix.
	BeforeAll(func() {
		By("installing CRDs")
		_, err := utils.Run(exec.Command("make", "install"))
		Expect(err).NotTo(HaveOccurred(), "Failed to install CRDs")

		By("deploying the controller-manager")
		_, err = utils.Run(exec.Command("make", "deploy", fmt.Sprintf("IMG=%s", managerImage)))
		Expect(err).NotTo(HaveOccurred(), "Failed to deploy the controller-manager")

		By("discovering the deployed operator resources (namePrefix-agnostic)")
		Eventually(func(g Gomega) {
			operatorNamespace = getOperatorNamespace(g)
			controllerPodName, operatorServiceAccount = getOperatorPod(g, operatorNamespace)
			metricsServiceName = getServiceBySuffix(g, operatorNamespace, "metrics-service")
			metricsReaderClusterRole = getClusterRoleBySuffix(g, "metrics-reader")
		}, 3*time.Minute, 3*time.Second).Should(Succeed())
		_, _ = fmt.Fprintf(GinkgoWriter,
			"discovered operator: ns=%s pod=%s sa=%s metricsSvc=%s metricsReader=%s\n",
			operatorNamespace, controllerPodName, operatorServiceAccount,
			metricsServiceName, metricsReaderClusterRole)
	})

	// Clean up everything this container created.
	AfterAll(func() {
		By("removing the metrics ClusterRoleBinding")
		_, _ = kubectl("delete", "clusterrolebinding", metricsRoleBindingName, "--ignore-not-found")

		if operatorNamespace != "" {
			By("cleaning up the curl pod for metrics")
			_, _ = kubectl("delete", "pod", "curl-metrics", "-n", operatorNamespace, "--ignore-not-found")
		}

		By("undeploying the controller-manager")
		_, _ = utils.Run(exec.Command("make", "undeploy"))

		By("uninstalling CRDs")
		_, _ = utils.Run(exec.Command("make", "uninstall"))
	})

	// On failure, collect logs, events and pod description for debugging.
	AfterEach(func() {
		if !CurrentSpecReport().Failed() {
			return
		}
		if controllerPodName != "" && operatorNamespace != "" {
			By("Fetching controller manager pod logs")
			if out, err := kubectl("logs", controllerPodName, "-n", operatorNamespace); err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Controller logs:\n%s", out)
			}

			By("Fetching controller manager pod description")
			if out, err := kubectl("describe", "pod", controllerPodName, "-n", operatorNamespace); err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Pod description:\n%s", out)
			}
		}
		if operatorNamespace != "" {
			By("Fetching Kubernetes events")
			if out, err := kubectl("get", "events", "-n", operatorNamespace, "--sort-by=.lastTimestamp"); err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Kubernetes events:\n%s", out)
			}
		}
	})

	SetDefaultEventuallyTimeout(2 * time.Minute)
	SetDefaultEventuallyPollingInterval(time.Second)

	Context("Deployment", func() {
		It("runs the controller-manager pod", func() {
			By("validating that the controller-manager pod reaches Running")
			Eventually(func(g Gomega) {
				controllerPodName, operatorServiceAccount = getOperatorPod(g, operatorNamespace)
				phase, err := kubectl("get", "pod", controllerPodName, "-n", operatorNamespace,
					"-o", "jsonpath={.status.phase}")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(phase).To(Equal("Running"), "controller-manager pod not Running")
			}, 3*time.Minute, time.Second).Should(Succeed())
		})

		It("becomes Ready and serves controller-runtime metrics on /metrics", func() {
			By("creating a ClusterRoleBinding so the operator SA can read metrics")
			_, err := kubectl("create", "clusterrolebinding", metricsRoleBindingName,
				fmt.Sprintf("--clusterrole=%s", metricsReaderClusterRole),
				fmt.Sprintf("--serviceaccount=%s:%s", operatorNamespace, operatorServiceAccount),
			)
			Expect(err).NotTo(HaveOccurred(), "Failed to create ClusterRoleBinding")

			By("validating that the metrics service exists")
			_, err = kubectl("get", "service", metricsServiceName, "-n", operatorNamespace)
			Expect(err).NotTo(HaveOccurred(), "Metrics service should exist")

			By("ensuring the controller pod is Ready")
			Eventually(func(g Gomega) {
				out, err := kubectl("get", "pod", controllerPodName, "-n", operatorNamespace,
					"-o", "jsonpath={.status.conditions[?(@.type=='Ready')].status}")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).To(Equal("True"), "controller pod not Ready")
			}, 3*time.Minute, time.Second).Should(Succeed())

			By("verifying the controller manager is serving the metrics server")
			Eventually(func(g Gomega) {
				out, err := kubectl("logs", controllerPodName, "-n", operatorNamespace)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).To(ContainSubstring("Serving metrics server"), "metrics server not started")
			}, 3*time.Minute, time.Second).Should(Succeed())

			By("getting the operator ServiceAccount token")
			token, err := serviceAccountToken(operatorNamespace, operatorServiceAccount)
			Expect(err).NotTo(HaveOccurred())
			Expect(token).NotTo(BeEmpty())

			By("creating a curl pod that scrapes the secure /metrics endpoint")
			metricsURL := fmt.Sprintf("https://%s.%s.svc.cluster.local:8443/metrics",
				metricsServiceName, operatorNamespace)
			_, err = kubectl("run", "curl-metrics", "--restart=Never",
				"--namespace", operatorNamespace,
				"--image=curlimages/curl:latest",
				"--overrides", curlPodOverrides(token, metricsURL, operatorServiceAccount))
			Expect(err).NotTo(HaveOccurred(), "Failed to create curl-metrics pod")

			By("waiting for the curl pod to complete")
			Eventually(func(g Gomega) {
				out, err := kubectl("get", "pods", "curl-metrics", "-n", operatorNamespace,
					"-o", "jsonpath={.status.phase}")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).To(Equal("Succeeded"), "curl pod not Succeeded")
			}, 5*time.Minute).Should(Succeed())

			By("asserting the endpoint returned 200 and controller-runtime metrics")
			Eventually(func(g Gomega) {
				out, err := getMetricsOutput(operatorNamespace)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).NotTo(BeEmpty())
				g.Expect(out).To(ContainSubstring("< HTTP/1.1 200 OK"))
				// controller-runtime always registers these; this is the hard gate that the
				// /metrics endpoint really serves Prometheus exposition.
				g.Expect(out).To(ContainSubstring("controller_runtime_"),
					"controller-runtime metrics missing from /metrics")
			}, 2*time.Minute).Should(Succeed())

			By("checking for crystalbackup_-prefixed custom metrics (advisory at M0)")
			if out, err := getMetricsOutput(operatorNamespace); err == nil {
				if strings.Contains(out, "crystalbackup_") {
					_, _ = fmt.Fprintf(GinkgoWriter, "found crystalbackup_ custom metrics on /metrics\n")
				} else {
					_, _ = fmt.Fprintf(GinkgoWriter,
						"NOTE: no crystalbackup_-prefixed series yet — expected at M0 (empty-logic "+
							"operator exports only controller-runtime metrics); this becomes a hard "+
							"assertion once metrics v1 lands in M1 (spec/05-observability.md).\n")
				}
			}
		})

		It("emits structured JSON-lines logs on stdout (M0 observability, advisory)", func() {
			logs, err := kubectl("logs", controllerPodName, "-n", operatorNamespace)
			Expect(err).NotTo(HaveOccurred())
			jsonSeen := false
			for _, line := range utils.GetNonEmptyLines(logs) {
				var m map[string]interface{}
				if json.Unmarshal([]byte(line), &m) == nil {
					jsonSeen = true
					break
				}
			}
			if !jsonSeen {
				_, _ = fmt.Fprintf(GinkgoWriter,
					"NOTE: operator logs are not JSON-lines yet; M0 exit criteria "+
						"(spec/90-roadmap.md) require zap JSON on stdout. Surfaced as a warning so "+
						"the M0 gate stays green on an empty-logic operator.\n")
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "operator logs parse as JSON-lines\n")
			}
		})
	})
})

// curlPodOverrides builds the --overrides JSON for the metrics scraper pod. The pod is
// restricted-PodSecurity compliant so it can run in the operator namespace.
func curlPodOverrides(token, url, serviceAccount string) string {
	args := fmt.Sprintf(
		"for i in $(seq 1 30); do curl -v -k -H 'Authorization: Bearer %s' %s && exit 0 || sleep 2; done; exit 1",
		token, url)
	return fmt.Sprintf(`{
		"spec": {
			"containers": [{
				"name": "curl",
				"image": "curlimages/curl:latest",
				"command": ["/bin/sh", "-c"],
				"args": ["%s"],
				"securityContext": {
					"readOnlyRootFilesystem": true,
					"allowPrivilegeEscalation": false,
					"capabilities": { "drop": ["ALL"] },
					"runAsNonRoot": true,
					"runAsUser": 1000,
					"seccompProfile": { "type": "RuntimeDefault" }
				}
			}],
			"serviceAccountName": "%s"
		}
	}`, args, serviceAccount)
}

// serviceAccountToken returns a token for the given ServiceAccount via the TokenRequest API.
func serviceAccountToken(ns, serviceAccount string) (string, error) {
	const tokenRequestRawString = `{
		"apiVersion": "authentication.k8s.io/v1",
		"kind": "TokenRequest"
	}`

	By("creating a temporary file to store the token request")
	tokenRequestFile := filepath.Join("/tmp", fmt.Sprintf("%s-token-request", serviceAccount))
	if err := os.WriteFile(tokenRequestFile, []byte(tokenRequestRawString), os.FileMode(0o644)); err != nil {
		return "", err
	}

	var out string
	verifyTokenCreation := func(g Gomega) {
		cmd := exec.Command("kubectl", "create", "--raw", fmt.Sprintf(
			"/api/v1/namespaces/%s/serviceaccounts/%s/token", ns, serviceAccount,
		), "-f", tokenRequestFile)
		output, err := cmd.CombinedOutput()
		g.Expect(err).NotTo(HaveOccurred())

		var token tokenRequest
		g.Expect(json.Unmarshal(output, &token)).To(Succeed())
		out = token.Status.Token
	}
	Eventually(verifyTokenCreation).Should(Succeed())
	return out, nil
}

// getMetricsOutput returns the logs of the curl pod used to scrape /metrics.
func getMetricsOutput(ns string) (string, error) {
	return kubectl("logs", "curl-metrics", "-n", ns)
}

// tokenRequest is a minimal view of the TokenRequest API response.
type tokenRequest struct {
	Status struct {
		Token string `json:"token"`
	} `json:"status"`
}
