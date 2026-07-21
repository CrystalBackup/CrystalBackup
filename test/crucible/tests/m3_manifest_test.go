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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/restic"
	"github.com/CrystalBackup/CrystalBackup/internal/status"
)

// ---------------------------------------------------------------------------
// M3 acceptance — manifest backup & restore round-trip (R15; spec/04 §6, the M3
// gate). A dedicated tenant namespace is seeded with the full manifest matrix a
// real app needs — Deployment + NodePort Service + ConfigMap + a bound PVC — and
// backed up with the manifest capture ON by default. Then a self-service,
// namespaced Restore replays the manifests and every load-bearing property is
// asserted on the LIVE cluster:
//
//   - the run Completes; the child Backup records a manifests snapshot and the run
//     records clusterResourcesCaptured; `restic snapshots` shows a kind=manifests
//     snapshot (the repository, not the CR, is the oracle);
//   - mode is verified against a drifted ConfigMap — Overwrite SSA-merges and keeps
//     target-only extras, Recreate replaces to an exact match;
//   - a restored workload reaches Ready, its Service's nodePort is preserved while a
//     fresh clusterIP is allocated (sanitization stripped the captured one);
//   - the R23 confirmation gate is enforced: a destructive restore parks without a
//     confirmation, and a WRONG confirmation is rejected at admission.
// ---------------------------------------------------------------------------

var _ = Describe("M3 — manifest backup & restore round-trip", Label("m3"), Ordered, func() {
	const (
		nsName     = "m3-manifest"
		pvcName    = "data"
		cmName     = "app-config"
		svcName    = "web"
		deployName = "web"
	)
	// Unique per run so a re-run on the shared "dr" repo never collides with a prior snapshot (see m3RunID).
	runName := "m3-manifest-src-" + m3RunID

	// Captured at seed time, asserted after a Service-recreating restore.
	var origNodePort int32
	var origClusterIP string

	restore := func(name string, spec cbv1.RestoreSpec) *cbv1.Restore {
		r := &cbv1.Restore{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: nsName},
			Spec:       spec,
		}
		Expect(k8s.Create(ctx, r)).To(Succeed())
		return r
	}

	// getConfigMap reads the live ConfigMap the mode scenarios drift and heal.
	getConfigMap := func() *corev1.ConfigMap {
		var cm corev1.ConfigMap
		Expect(k8s.Get(ctx, client.ObjectKey{Namespace: nsName, Name: cmName}, &cm)).To(Succeed())
		return &cm
	}

	BeforeAll(func() {
		m3EnsureDRLocation()

		By("seeding a tenant namespace with a bound PVC and its checksummed corpus")
		// m2SeedVolume ensures the namespace, provisions pvcName on ceph-block and writes a
		// MANIFEST.sha256; the PVC has no live consumer afterwards, so it is free to snapshot.
		m2SeedVolume(nsName, pvcName, "ceph-block", "1Gi")

		By("adding the manifest matrix: ConfigMap + NodePort Service + Deployment")
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: cmName, Namespace: nsName},
			Data:       map[string]string{"app.conf": "original"},
		}
		Expect(client.IgnoreAlreadyExists(k8s.Create(ctx, cm))).To(Succeed())

		svc := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: svcName, Namespace: nsName},
			Spec: corev1.ServiceSpec{
				Type:     corev1.ServiceTypeNodePort,
				Selector: map[string]string{"app": "web"},
				Ports: []corev1.ServicePort{{
					Port:       80,
					TargetPort: intstr.FromInt32(80),
					Protocol:   corev1.ProtocolTCP,
				}},
			},
		}
		Expect(client.IgnoreAlreadyExists(k8s.Create(ctx, svc))).To(Succeed())

		replicas := int32(1)
		deploy := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: deployName, Namespace: nsName},
			Spec: appsv1.DeploymentSpec{
				Replicas: &replicas,
				Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "web"}},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{
							Name:  "nginx",
							Image: "nginx:1.27-alpine",
							Ports: []corev1.ContainerPort{{ContainerPort: 80}},
							VolumeMounts: []corev1.VolumeMount{{
								Name:      "content",
								MountPath: "/usr/share/nginx/html",
								ReadOnly:  true,
							}},
						}},
						Volumes: []corev1.Volume{{
							Name: "content",
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{Name: cmName},
								},
							},
						}},
					},
				},
			},
		}
		Expect(client.IgnoreAlreadyExists(k8s.Create(ctx, deploy))).To(Succeed())
		m3WaitDeploymentReady(nsName, deployName, 5*time.Minute)

		By("recording the Service's apiserver-allocated nodePort and clusterIP")
		Eventually(func(g Gomega) {
			var s corev1.Service
			g.Expect(k8s.Get(ctx, client.ObjectKey{Namespace: nsName, Name: svcName}, &s)).To(Succeed())
			g.Expect(s.Spec.Ports).NotTo(BeEmpty())
			g.Expect(s.Spec.Ports[0].NodePort).To(BeNumerically(">", 0), "NodePort not allocated yet")
			g.Expect(s.Spec.ClusterIP).NotTo(BeElementOf("", corev1.ClusterIPNone))
			origNodePort = s.Spec.Ports[0].NodePort
			origClusterIP = s.Spec.ClusterIP
		}, time.Minute, 2*time.Second).Should(Succeed())

		By("backing the namespace up: manifests + cluster-scoped, but NOT PVC data (pvcSelector excludes all)")
		// Manifest-ONLY: this spec proves the manifest round-trip (§6); volume data is exercised by
		// the DR-bootstrap and cluster-scoped specs. Excluding data keeps the child Backup's terminal
		// state — and thus its manifests status — off the CSI-snapshot data path, mirroring the kind
		// e2e's manifest-only milestone 1 (which is deterministic where a data+manifest capture is not).
		var existing cbv1.ClusterBackup
		if err := k8s.Get(ctx, client.ObjectKey{Name: runName}, &existing); err != nil {
			enabled := true
			Expect(k8s.Create(ctx, &cbv1.ClusterBackup{
				ObjectMeta: metav1.ObjectMeta{Name: runName},
				Spec: cbv1.ClusterBackupSpec{
					ClusterBackupRunSpec: cbv1.ClusterBackupRunSpec{
						LocationRef:      cbv1.LocalObjectReference{Name: m1LocationName},
						Namespaces:       cbv1.NamespaceSelector{MatchNames: []string{nsName}},
						PVCSelector:      cbv1.PVCSelector{Exclude: []string{"*"}},
						ClusterResources: cbv1.ClusterResourceCaptureSpec{Enabled: &enabled},
					},
				},
			})).To(Succeed(), "create manifest-only ClusterBackup %s", runName)
		}
		cb := m1WaitClusterBackupTerminal(runName, 20*time.Minute)
		Expect(cb.Status.Phase).To(Equal("Completed"),
			"the source backup must complete (phase=%q)", cb.Status.Phase)
	})

	AfterAll(func() {
		_ = k8s.Delete(ctx, &cbv1.ClusterBackup{ObjectMeta: metav1.ObjectMeta{Name: runName}})
		deleteNamespace(nsName)
		m1AssertNoResidualSnapshotObjects(nsName)
		m2AssertNoResidualRestoreObjects()
	})

	It("captures the namespace manifests: run Completes, child Backup + clusterResourcesCaptured set, kind=manifests snapshot present", func() {
		cb := m1WaitClusterBackupTerminal(runName, time.Minute)
		Expect(cb.Status.Phase).To(Equal("Completed"))
		Expect(cb.Status.ClusterResourcesCaptured).To(BeNumerically(">", 0),
			"default cluster-scoped capture must record a count (adr/0011)")

		By("the child Backup projected into the namespace records its manifests snapshot")
		// Poll: the ClusterBackup can report Completed a beat before the child Backup's manifests
		// status write is visible to a fresh Get. The kind e2e polls this exact field the same way.
		Eventually(func(g Gomega) {
			var child cbv1.Backup
			g.Expect(k8s.Get(ctx, client.ObjectKey{Namespace: nsName, Name: runName}, &child)).To(Succeed())
			g.Expect(child.Status.Manifests).NotTo(BeNil(), "child Backup must record a manifests snapshot")
			g.Expect(child.Status.Manifests.SnapshotID).NotTo(BeEmpty())
			g.Expect(child.Status.Manifests.ResourceCount).To(BeNumerically(">", 0))
		}, 3*time.Minute, 5*time.Second).Should(Succeed())

		By("the independent restic oracle shows a kind=manifests snapshot for this run/namespace")
		snap, ok := m3FindSnapshot(m1LocationName, restic.KindManifests, nsName, runName)
		Expect(ok).To(BeTrue(), "no kind=manifests snapshot for namespace %q in run %q", nsName, runName)
		Expect(snap.Paths).To(ContainElement("/manifests/" + nsName))
	})

	It("Overwrite-restores the drifted ConfigMap via SSA, keeping target-only extras (R23 parks without a confirmation)", func() {
		By("drifting the live ConfigMap: change a captured key, add a target-only key")
		cm := getConfigMap()
		cm.Data["app.conf"] = "drifted"
		cm.Data["target-only"] = "keep-me"
		Expect(k8s.Update(ctx, cm)).To(Succeed())

		By("creating an Overwrite Restore of ONLY the ConfigMap manifest, WITHOUT a confirmation (R23 parks it)")
		restore("m3-cm-overwrite", cbv1.RestoreSpec{
			Source:    cbv1.RestoreSource{Backup: runName},
			Mode:      cbv1.RestoreModeOverwrite,
			Resources: []cbv1.ResourceSelectorItem{{Include: []string{"ConfigMap/" + cmName}}},
			Volumes:   []cbv1.VolumeSelectorItem{}, // present-but-empty ⇒ restore no volumes
		})
		Eventually(func(g Gomega) {
			var r cbv1.Restore
			g.Expect(k8s.Get(ctx, client.ObjectKey{Namespace: nsName, Name: "m3-cm-overwrite"}, &r)).To(Succeed())
			g.Expect(r.Status.Phase).To(Equal(string(status.RestorePhaseAwaitingConfirmation)))
		}, 3*time.Minute, 3*time.Second).Should(Succeed())

		By("confirming with the namespace name and waiting for completion")
		Eventually(func(g Gomega) {
			var r cbv1.Restore
			g.Expect(k8s.Get(ctx, client.ObjectKey{Namespace: nsName, Name: "m3-cm-overwrite"}, &r)).To(Succeed())
			r.Spec.Confirmation = nsName
			g.Expect(k8s.Update(ctx, &r)).To(Succeed())
		}, time.Minute, 2*time.Second).Should(Succeed())
		done := m2WaitRestoreTerminal(nsName, "m3-cm-overwrite", 15*time.Minute)
		Expect(done.Status.Phase).To(Equal(string(status.RestorePhaseCompleted)))

		By("Overwrite healed the captured key via SSA and KEPT the target-only extra")
		cm = getConfigMap()
		Expect(cm.Data["app.conf"]).To(Equal("original"), "captured key must be healed")
		Expect(cm.Data).To(HaveKeyWithValue("target-only", "keep-me"), "Overwrite keeps target-only extras")
	})

	It("Recreate-restores the ConfigMap to an EXACT match (target-only extra removed)", func() {
		By("drifting the ConfigMap again")
		cm := getConfigMap()
		cm.Data["app.conf"] = "drifted-again"
		cm.Data["target-only"] = "keep-me"
		Expect(k8s.Update(ctx, cm)).To(Succeed())

		restore("m3-cm-recreate", cbv1.RestoreSpec{
			Source:       cbv1.RestoreSource{Backup: runName},
			Mode:         cbv1.RestoreModeRecreate,
			Resources:    []cbv1.ResourceSelectorItem{{Include: []string{"ConfigMap/" + cmName}}},
			Volumes:      []cbv1.VolumeSelectorItem{},
			Confirmation: nsName,
		})
		done := m2WaitRestoreTerminal(nsName, "m3-cm-recreate", 15*time.Minute)
		Expect(done.Status.Phase).To(Equal(string(status.RestorePhaseCompleted)))

		By("Recreate replaced the ConfigMap outright: captured key healed, extra GONE")
		cm = getConfigMap()
		Expect(cm.Data["app.conf"]).To(Equal("original"))
		Expect(cm.Data).NotTo(HaveKey("target-only"), "Recreate reconciles to an exact match")
	})

	It("restores the workload manifests: Deployment reaches Ready, nodePort preserved, clusterIP freshly allocated", func() {
		By("destroying the live Service and Deployment (only the backup remembers them now)")
		Expect(k8s.Delete(ctx, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: svcName, Namespace: nsName}})).To(Succeed())
		Expect(k8s.Delete(ctx, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: deployName, Namespace: nsName}})).To(Succeed())

		By("restoring ONLY those two manifests (Overwrite ⇒ R23-gated, so confirmation is required)")
		// R23 is unconditional: every Recreate/Overwrite requires spec.confirmation ==
		// the namespace, whether or not the target object currently exists
		// (restore_controller.go — the gate is checked before any target lookup). These
		// two objects were just deleted, but a create-only apply is still confirmation-gated.
		restore("m3-workload", cbv1.RestoreSpec{
			Source: cbv1.RestoreSource{Backup: runName},
			Mode:   cbv1.RestoreModeOverwrite,
			Resources: []cbv1.ResourceSelectorItem{{
				Include: []string{"Service/" + svcName, "apps/Deployment/" + deployName},
			}},
			Volumes:      []cbv1.VolumeSelectorItem{},
			Confirmation: nsName,
		})
		done := m2WaitRestoreTerminal(nsName, "m3-workload", 15*time.Minute)
		Expect(done.Status.Phase).To(Equal(string(status.RestorePhaseCompleted)))

		By("the restored Deployment rolls out and becomes Ready")
		m3WaitDeploymentReady(nsName, deployName, 5*time.Minute)

		By("the restored Service keeps its nodePort but is given a FRESH clusterIP")
		var svc corev1.Service
		Expect(k8s.Get(ctx, client.ObjectKey{Namespace: nsName, Name: svcName}, &svc)).To(Succeed())
		Expect(svc.Spec.Ports).NotTo(BeEmpty())
		Expect(svc.Spec.Ports[0].NodePort).To(Equal(origNodePort),
			"nodePort must be preserved from the manifest")
		Expect(svc.Spec.ClusterIP).NotTo(BeElementOf("", corev1.ClusterIPNone))
		Expect(svc.Spec.ClusterIP).NotTo(Equal(origClusterIP),
			"clusterIP was stripped at capture and must be freshly allocated on restore")
	})

	It("rejects a WRONG confirmation at admission (R23 gate)", func() {
		r := &cbv1.Restore{
			ObjectMeta: metav1.ObjectMeta{Name: "m3-cm-wrongconf", Namespace: nsName},
			Spec: cbv1.RestoreSpec{
				Source:       cbv1.RestoreSource{Backup: runName},
				Mode:         cbv1.RestoreModeOverwrite,
				Resources:    []cbv1.ResourceSelectorItem{{Include: []string{"ConfigMap/" + cmName}}},
				Volumes:      []cbv1.VolumeSelectorItem{},
				Confirmation: "not-the-namespace",
			},
		}
		err := k8s.Create(ctx, r)
		Expect(err).To(HaveOccurred(), "a confirmation that is not the namespace must be denied at admission")
		Expect(err.Error()).To(ContainSubstring("confirmation"),
			"the denial must cite the R23 confirmation rule (got: %v)", err)
	})
})

// m3WaitDeploymentReady waits until a Deployment reports at least one ready replica.
func m3WaitDeploymentReady(ns, name string, timeout time.Duration) {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		var d appsv1.Deployment
		g.Expect(k8s.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &d)).To(Succeed())
		g.Expect(d.Status.ReadyReplicas).To(BeNumerically(">=", 1),
			"deployment %s/%s has no ready replicas yet", ns, name)
	}, timeout, 5*time.Second).Should(Succeed())
}
