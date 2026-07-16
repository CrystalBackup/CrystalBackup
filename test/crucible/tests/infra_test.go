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
	"os/exec"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const smokeNS = "crucible-smoke"

// Infrastructure: the cluster the product tests rely on must itself be sane.
// If these fail, fix the platform before reading any product test result.
var _ = Describe("Infrastructure", Label("infra"), func() {

	It("has all nodes Ready with the expected roles", func() {
		var nodes corev1.NodeList
		Expect(k8s.List(ctx, &nodes)).To(Succeed())

		masters, workers := 0, 0
		for _, n := range nodes.Items {
			ready := false
			for _, c := range n.Status.Conditions {
				if c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue {
					ready = true
				}
			}
			Expect(ready).To(BeTrue(), "node %s is not Ready", n.Name)

			switch n.Labels["crystalbackup.io/node-role"] {
			case "master":
				masters++
			case "worker":
				workers++
			}
		}
		Expect(masters).To(BeNumerically(">=", 3), "want >=3 master nodes")
		Expect(workers).To(BeNumerically(">=", 3), "want >=3 worker nodes")
	})

	It("exposes the four storage classes (ceph-block default)", func() {
		for _, name := range []string{"ceph-block", "ceph-filesystem", "longhorn", "local-path"} {
			var sc storagev1.StorageClass
			Expect(k8s.Get(ctx, client.ObjectKey{Name: name}, &sc)).To(Succeed(),
				"StorageClass %s must exist", name)
			if name == "ceph-block" {
				Expect(sc.Annotations["storageclass.kubernetes.io/is-default-class"]).To(Equal("true"),
					"ceph-block should be the default StorageClass")
			}
		}
	})

	It("exposes the snapshot classes for the snapshot-capable drivers", func() {
		for _, name := range []string{"ceph-block", "ceph-filesystem", "longhorn"} {
			vsc := &unstructured.Unstructured{}
			vsc.SetGroupVersionKind(volumeSnapshotClassGVK())
			Expect(k8s.Get(ctx, client.ObjectKey{Name: name}, vsc)).To(Succeed(),
				"VolumeSnapshotClass %s must exist", name)
		}
	})

	It("reports Ceph HEALTH_OK (via the rook toolbox)", func() {
		out, err := exec.Command("kubectl", "-n", "rook-ceph",
			"exec", "deploy/rook-ceph-tools", "--", "ceph", "health").CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), "ceph health failed: %s", string(out))
		Expect(strings.TrimSpace(string(out))).To(HavePrefix("HEALTH_OK"),
			"ceph must be HEALTH_OK, got: %s", string(out))
	})

	It("reaches the Hetzner Object Storage backup bucket", func() {
		bucket, endpoint := os.Getenv("S3_BUCKET"), os.Getenv("S3_ENDPOINT")
		if bucket == "" || endpoint == "" {
			Skip("S3_BUCKET / S3_ENDPOINT not set (run via `mise run test` so terraform facts are loaded)")
		}
		if _, err := exec.LookPath("aws"); err != nil {
			Skip("aws CLI not on PATH (provided by test/crucible/mise.toml)")
		}
		cmd := exec.Command("aws", "s3api", "head-bucket",
			"--bucket", bucket, "--endpoint-url", endpoint)
		// Hetzner (like most S3-compatibles) rejects the newer AWS SDK
		// mandatory checksums — only compute them when required.
		cmd.Env = append(os.Environ(),
			"AWS_REQUEST_CHECKSUM_CALCULATION=when_required",
			"AWS_RESPONSE_CHECKSUM_VALIDATION=when_required",
			"AWS_DEFAULT_REGION="+envOr("S3_REGION", "fsn1"),
		)
		out, err := cmd.CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), "head-bucket %s failed: %s", bucket, string(out))
	})

	Describe("dynamic provisioning + CSI snapshots", Ordered, func() {
		BeforeAll(func() { ensureNamespace(smokeNS) })
		AfterAll(func() { deleteNamespace(smokeNS) })

		It("provisions and snapshots a ceph-block volume", func() {
			pvc, pod := startPVCConsumer(smokeNS, "smoke-ceph-block", "ceph-block")
			snap := snapshotAndWaitReady(smokeNS, "smoke-ceph-block", pvc.Name, "ceph-block")
			Expect(k8s.Delete(ctx, snap)).To(Succeed())
			Expect(k8s.Delete(ctx, pod)).To(Succeed())
		})

		It("provisions and snapshots a longhorn volume", func() {
			pvc, pod := startPVCConsumer(smokeNS, "smoke-longhorn", "longhorn")
			snap := snapshotAndWaitReady(smokeNS, "smoke-longhorn", pvc.Name, "longhorn")
			Expect(k8s.Delete(ctx, snap)).To(Succeed())
			Expect(k8s.Delete(ctx, pod)).To(Succeed())
		})

		It("provisions a local-path volume (the class WITHOUT snapshot support)", func() {
			_, pod := startPVCConsumer(smokeNS, "smoke-local-path", "local-path")
			Expect(k8s.Delete(ctx, pod)).To(Succeed())
		})
	})
})

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
