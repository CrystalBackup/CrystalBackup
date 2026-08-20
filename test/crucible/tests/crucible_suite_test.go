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
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	clientscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
)

// operatorSAUser is the operator ServiceAccount username the admission VAPs exempt via their
// matchConditions. Tests that must set up operator-only state (e.g. a cluster-origin projected
// Backup, which the user-isolation VAP forbids ordinary identities from writing) impersonate it.
const operatorSAUser = "system:serviceaccount:crystal-backup-system:crystal-backup-operator"

var (
	k8s client.Client
	// k8sAsOperator impersonates the operator SA — it bypasses the tenant-facing admission
	// VAPs (which exempt that identity), letting a spec reach a controller/storage backstop
	// that the admission layer would otherwise stop at creation.
	k8sAsOperator client.Client
	// k8sTyped is the typed clientset. controller-runtime's client speaks only to the object
	// API, and the M6 alert specs need the SUBRESOURCE proxy — reaching Prometheus's HTTP API
	// through the API server instead of a port-forward or a NodePort left open on a public IP.
	k8sTyped *kubernetes.Clientset
	ctx      = context.Background()
)

// ---------------------------------------------------------------------------
// Hermeticity against PREVIOUS CAMPAIGNS — read this before naming anything.
//
// The object store outlives the cluster. `scripts/nuke.sh` destroys servers, volumes and the
// kubeconfig, and says so explicitly about the bucket: it is never deleted. The shared "dr"
// repository therefore accumulates every snapshot every campaign ever wrote, and a fresh cluster
// pointed at it is NOT a fresh slate.
//
// What that costs, concretely. A restic snapshot is addressed by (namespace, run) — the run tag is
// the ClusterBackup's name, verbatim. The discovery controller inventories the repository and
// projects each (namespace, run) group back into the cluster as a read-only Backup, phase
// Completed, annotated crystalbackup.io/projected: "true". So a spec that hard-codes its run name
// finds LAST MONTH's snapshots already sitting on its coordinate, projected into the namespace,
// before its own ClusterBackup exists. The fan-out then refuses the namespace with
// RunNameCollision — correctly: it will not report success over data it did not write.
//
// The operator is right and the spec is wrong. The fix is never to weaken the collision check; it
// is for the suite to stop reusing a name the repository already knows.
//
// crucibleRunID is that identity: one value per test BINARY, so every spec in a campaign shares it
// (a failure names one campaign) and no two campaigns ever share it. Base-36 seconds keeps it short
// enough to leave room under the 253-char object-name limit.
//
// USE crucibleRunName FOR EVERY ClusterBackup / Backup NAME. A const run name is a latent red
// campaign six weeks from now, with the diagnosis to redo from scratch — which is exactly what
// happened between 0.5.1 and 0.6.0, twice, despite a note in the project's memory. It is now
// enforced: runname_hermeticity_test.go fails `make test` on a fixed run name, without touching
// paid infrastructure. The two legitimate exceptions and how to declare them are documented there.
var crucibleRunID = strconv.FormatInt(time.Now().Unix(), 36)

// crucibleRunName suffixes a stable, readable base with this campaign's identity. The base still
// says which spec owns the run — "m6-fidelity-src-mfa2k7q" reads as well in `restic snapshots` as
// the fixed name did, and does not collide with the campaign before it.
func crucibleRunName(base string) string { return base + "-" + crucibleRunID }

func TestCrucible(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Crucible — real-conditions e2e")
}

var _ = BeforeSuite(func() {
	kubeconfig := os.Getenv("KUBECONFIG")
	Expect(kubeconfig).NotTo(BeEmpty(),
		"KUBECONFIG must point at the crucible cluster (run via `mise run test` in test/crucible)")
	Expect(kubeconfig).To(BeAnExistingFile())

	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	Expect(err).NotTo(HaveOccurred())

	sc := runtime.NewScheme()
	Expect(clientscheme.AddToScheme(sc)).To(Succeed())
	Expect(cbv1.AddToScheme(sc)).To(Succeed())

	k8s, err = client.New(cfg, client.Options{Scheme: sc})
	Expect(err).NotTo(HaveOccurred())

	k8sTyped, err = kubernetes.NewForConfig(cfg)
	Expect(err).NotTo(HaveOccurred())

	opCfg := rest.CopyConfig(cfg)
	opCfg.Impersonate = rest.ImpersonationConfig{UserName: operatorSAUser}
	k8sAsOperator, err = client.New(opCfg, client.Options{Scheme: sc})
	Expect(err).NotTo(HaveOccurred())

	// ---------------------------------------------------------------------------
	// The shared "dr" ClusterBackupLocation is SUITE INFRASTRUCTURE, established here, once.
	//
	// It used to be a side effect: fifteen containers each carried a defensive get-or-create in
	// their BeforeAll, so the location existed only from the moment the first of them ran — while
	// three others (m6/s3tuning, m6/placement, m4/maintenance's shared-repository case) merely
	// WAITED for it. Ginkgo randomises top-level container order on every run, so which of those
	// two groups went first was a coin flip. 0.6.7 lost it: m6/s3tuning's BeforeAll timed out
	// after 600s at 03:01:52 and the location was created at ~03:07 by a lane that ran later.
	//
	// Establishing it here removes the ordering dependency at its root rather than adding a
	// sixteenth defensive bootstrap. m1EnsureSharedLocation also settles the quieter half of the
	// bug — the default flag, which the same shuffle used to decide; its comment carries that
	// reasoning.
	//
	// It creates and does not wait: `restic init` proceeds in the background while the infra and
	// m0 lanes run, and each lane still waits for its own precondition through
	// m1EnsureSharedRepository, where the timeout's diagnostics belong.
	m1EnsureSharedLocation()
})

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

func volumeSnapshotGVK() schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshot"}
}

func volumeSnapshotClassGVK() schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshotClass"}
}

func crdGVK() schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"}
}

// ensureNamespace creates ns (idempotently), waiting out a Terminating leftover
// from a previous aborted run.
func ensureNamespace(name string) {
	GinkgoHelper()
	Eventually(func() error {
		var existing corev1.Namespace
		err := k8s.Get(ctx, client.ObjectKey{Name: name}, &existing)
		switch {
		case apierrors.IsNotFound(err):
			return k8s.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}})
		case err != nil:
			return err
		case existing.Status.Phase == corev1.NamespaceTerminating:
			return fmt.Errorf("namespace %s still terminating", name)
		default:
			return nil
		}
		// 5 min (was 2): on a reused cluster a same-named namespace from a prior run can still be
		// draining finalizers (PVCs/VolumeSnapshots on Ceph) when the next run re-seeds it; give the
		// termination headroom rather than fail the re-seed.
	}, 5*time.Minute, 3*time.Second).Should(Succeed())
}

func deleteNamespace(name string) {
	_ = k8s.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}})
}

// deleteNamespaceAndWaitGone deletes a namespace and FAILS if it does not actually disappear.
//
// "Deleted" and "gone" are not the same thing, and the gap between them is where M3.2's worst bug
// lived: a restore transplanted a CSI finalizer onto a PVC, so the claim could never be deleted and
// the namespace sat in Terminating for the rest of the cluster's life — invisible to every spec,
// because deleteNamespace is fire-and-forget. Any spec that RESTORES into a namespace should tear
// it down through here, so the next such leak fails the run that caused it instead of the run after.
//
// The diagnostic is the point of the failure message: the namespace status names both the resource
// kinds still present and the finalizers holding them, which is exactly what a reader needs.
func deleteNamespaceAndWaitGone(name string, timeout time.Duration) {
	GinkgoHelper()
	_ = k8s.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}})
	Eventually(func(g Gomega) {
		var ns corev1.Namespace
		err := k8s.Get(ctx, client.ObjectKey{Name: name}, &ns)
		if apierrors.IsNotFound(err) {
			return
		}
		g.Expect(err).NotTo(HaveOccurred())
		var stuck []string
		for _, c := range ns.Status.Conditions {
			if c.Status == corev1.ConditionTrue {
				stuck = append(stuck, fmt.Sprintf("%s: %s", c.Type, c.Message))
			}
		}
		g.Expect(ns.Status.Phase).NotTo(Equal(corev1.NamespaceTerminating),
			"namespace %s never finished deleting — something holds a finalizer nothing will release: %s",
			name, strings.Join(stuck, " | "))
	}, timeout, 5*time.Second).Should(Succeed())
}

// startPVCConsumer creates a PVC on the given StorageClass plus a pod that
// mounts it, and waits until the pod is Running (which implies Bound — also
// for WaitForFirstConsumer classes like local-path).
func startPVCConsumer(ns, name, storageClass string) (*corev1.PersistentVolumeClaim, *corev1.Pod) {
	GinkgoHelper()

	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: &storageClass,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("100Mi")},
			},
		},
	}
	Expect(k8s.Create(ctx, pvc)).To(Succeed())

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{{
				Name:    "consumer",
				Image:   "busybox:1.36",
				Command: []string{"/bin/sh", "-c", "echo crucible > /data/probe.txt && sleep infinity"},
				VolumeMounts: []corev1.VolumeMount{{
					Name:      "data",
					MountPath: "/data",
				}},
			}},
			Volumes: []corev1.Volume{{
				Name: "data",
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: name},
				},
			}},
		},
	}
	Expect(k8s.Create(ctx, pod)).To(Succeed())

	Eventually(func(g Gomega) {
		var p corev1.Pod
		g.Expect(k8s.Get(ctx, client.ObjectKeyFromObject(pod), &p)).To(Succeed())
		g.Expect(p.Status.Phase).To(Equal(corev1.PodRunning),
			"pod %s/%s should be Running (PVC bound + mounted)", ns, name)
	}, 5*time.Minute, 5*time.Second).Should(Succeed())

	return pvc, pod
}

// snapshotAndWaitReady snapshots a PVC with the given VolumeSnapshotClass and
// waits for readyToUse.
func snapshotAndWaitReady(ns, name, pvcName, snapClass string) *unstructured.Unstructured {
	GinkgoHelper()

	snap := &unstructured.Unstructured{}
	snap.SetGroupVersionKind(volumeSnapshotGVK())
	snap.SetName(name)
	snap.SetNamespace(ns)
	Expect(unstructured.SetNestedField(snap.Object, snapClass, "spec", "volumeSnapshotClassName")).To(Succeed())
	Expect(unstructured.SetNestedField(snap.Object, pvcName, "spec", "source", "persistentVolumeClaimName")).To(Succeed())
	Expect(k8s.Create(ctx, snap)).To(Succeed())

	Eventually(func(g Gomega) {
		got := &unstructured.Unstructured{}
		got.SetGroupVersionKind(volumeSnapshotGVK())
		g.Expect(k8s.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, got)).To(Succeed())
		ready, found, err := unstructured.NestedBool(got.Object, "status", "readyToUse")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(found).To(BeTrue(), "VolumeSnapshot %s/%s has no status.readyToUse yet", ns, name)
		g.Expect(ready).To(BeTrue(), "VolumeSnapshot %s/%s not readyToUse", ns, name)
	}, 5*time.Minute, 5*time.Second).Should(Succeed())

	return snap
}
