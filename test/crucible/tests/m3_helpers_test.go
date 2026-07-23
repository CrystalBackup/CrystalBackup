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
	"strconv"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/restic"
)

// ---------------------------------------------------------------------------
// M3 shared helpers — the manifest-restore / cluster-scoped-DR vocabulary the
// m3_*_test.go specs are written against. They drive the SAME live cluster and
// shared DR repository the M1/M2 specs use (the "dr" ClusterBackupLocation), and
// deliberately REUSE the M1/M2 helpers (m1EnsurePlatformSecrets, m1RunClusterBackup,
// m1ResticSnapshots, m2SeedVolume, m2WaitClusterRestoreTerminal, …) rather than
// re-implementing them — a one-line drift there must surface as a failing M2/M3
// acceptance scenario, not be papered over here.
// ---------------------------------------------------------------------------

// m3RunID makes every M3 backup name unique per run. The "dr" repository is SHARED and never
// reset between runs, so re-running the suite on the same cluster would otherwise reuse a prior
// run's ClusterBackup name — and the discovery controller, projecting that prior run's snapshot
// back into the namespace as an already-Completed Backup, short-circuits the new run's manifest
// capture (child.status.manifests stays nil). Each `mise run test` is a fresh test binary, so this
// is stable within a run and unique across runs — the crucible's equivalent of the kind e2e's
// fresh-clusterID-per-run hermeticity.
var m3RunID = strconv.FormatInt(time.Now().Unix(), 36)

// m3EnsureDRLocation is the shared Given of every M3 spec: skip if S3 is unconfigured,
// provision the platform KEK/S3 Secrets, ensure the canonical "dr" ClusterBackupLocation
// exists, and wait for its BackupRepository to be Initialized. Idempotent — several M3
// specs call it in their BeforeAll and share the one repository.
func m3EnsureDRLocation() {
	GinkgoHelper()
	m1SkipIfNoS3()
	m1EnsurePlatformSecrets()
	var loc cbv1.ClusterBackupLocation
	if apierrors.IsNotFound(k8s.Get(ctx, client.ObjectKey{Name: m1LocationName}, &loc)) {
		m1CreateLocation(m1LocationName, true)
	}
	m1WaitRepositoryInitialized(m1LocationName)
}

// m3RunClusterBackup creates a manual ClusterBackup like m1RunClusterBackup, but pins
// clusterResources.enabled explicitly so an M3 spec's capture assertions
// (status.clusterResourcesCaptured, the kind=cluster-manifests snapshot) do not depend on
// the schema default surviving a client round-trip. IncludeManifests is left at its true
// default (adr/0011; spec/04 §6).
func m3RunClusterBackup(name, locationName string, nsSelector cbv1.NamespaceSelector, captureClusterResources bool) *cbv1.ClusterBackup {
	GinkgoHelper()
	enabled := captureClusterResources
	cb := &cbv1.ClusterBackup{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: cbv1.ClusterBackupSpec{
			ClusterBackupRunSpec: cbv1.ClusterBackupRunSpec{
				LocationRef:      cbv1.LocalObjectReference{Name: locationName},
				Namespaces:       nsSelector,
				ClusterResources: cbv1.ClusterResourceCaptureSpec{Enabled: &enabled},
			},
		},
	}
	Expect(k8s.Create(ctx, cb)).To(Succeed(), "create ClusterBackup %s", name)
	return cb
}

// m3FindSnapshot returns the first repository snapshot carrying kind=<kind> and run=<run>
// (and namespace=<namespace> when namespace is non-empty), read through the independent
// restic oracle (m1ResticSnapshots) so a controller bug that both writes and reports the
// same wrong thing cannot pass. namespace is left empty for kind=cluster-manifests, which
// carries NO namespace tag (adr/0011 §1).
func m3FindSnapshot(locationName, kind, namespace, run string) (restic.Snapshot, bool) {
	GinkgoHelper()
	for _, s := range m1ResticSnapshots(locationName) {
		if !m1SnapshotHasTag(s, restic.Tag(restic.TagKeyKind, kind)) {
			continue
		}
		if !m1SnapshotHasTag(s, restic.Tag(restic.TagKeyRun, run)) {
			continue
		}
		if namespace != "" && !m1SnapshotHasTag(s, restic.Tag(restic.TagKeyNamespace, namespace)) {
			continue
		}
		return s, true
	}
	return restic.Snapshot{}, false
}

// m3NonAdminClient builds a client that impersonates a NON-admin identity (a tenant user,
// no binding to the crystal-backup-admin ClusterRole). It is the negative-authorization
// probe for the cluster-scoped restore path: creating a ClusterRestore as this identity
// must be RBAC-denied (DoD case 16). It mirrors the suite's k8sAsOperator construction —
// same kubeconfig, an ImpersonationConfig instead of the operator SA username.
func m3NonAdminClient() client.Client {
	GinkgoHelper()
	kubeconfig := os.Getenv("KUBECONFIG")
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	Expect(err).NotTo(HaveOccurred(), "build client config from KUBECONFIG for the non-admin probe")

	cfg = rest.CopyConfig(cfg)
	cfg.Impersonate = rest.ImpersonationConfig{
		UserName: "crucible:nonadmin",
		Groups:   []string{"crucible:tenants"},
	}

	sc := runtime.NewScheme()
	Expect(clientscheme.AddToScheme(sc)).To(Succeed())
	Expect(cbv1.AddToScheme(sc)).To(Succeed())

	c, err := client.New(cfg, client.Options{Scheme: sc})
	Expect(err).NotTo(HaveOccurred(), "build the impersonating non-admin client")
	return c
}

// m3RunVerifyingConsumer mounts an already-restored PVC in a long-lived pod whose entrypoint
// FIRST verifies the volume against its seed manifest (`sha256sum -c MANIFEST.sha256`) and
// only then sleeps. A pod that reaches Running therefore proves two things at once with a
// single RWO mount (no multi-attach contention with a separate verify Job): the DR-restored
// volume is mountable by a real workload on its target StorageClass AND its bytes match the
// source. A checksum mismatch exits the container non-zero (set -e) so the pod never reaches
// Running and the assertion fails with a readable message.
func m3RunVerifyingConsumer(ns, name, pvcName string) *corev1.Pod {
	GinkgoHelper()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{{
				Name:    "consumer",
				Image:   "busybox:1.36",
				Command: []string{"/bin/sh", "-c", "set -e; cd /data; sha256sum -c MANIFEST.sha256; sleep infinity"},
				VolumeMounts: []corev1.VolumeMount{
					{Name: "data", MountPath: "/data"},
				},
			}},
			Volumes: []corev1.Volume{{
				Name: "data",
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: pvcName},
				},
			}},
		},
	}
	Expect(k8s.Create(ctx, pod)).To(Succeed(), "create verifying consumer %s/%s", ns, name)

	Eventually(func(g Gomega) {
		var p corev1.Pod
		g.Expect(k8s.Get(ctx, client.ObjectKeyFromObject(pod), &p)).To(Succeed())
		g.Expect(p.Status.Phase).To(Equal(corev1.PodRunning),
			"consumer %s/%s must reach Running on the DR-restored volume (its data verified against MANIFEST.sha256)", ns, name)
	}, 5*time.Minute, 5*time.Second).Should(Succeed())
	return pod
}
