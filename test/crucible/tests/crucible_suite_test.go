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
	ctx           = context.Background()
)

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

	opCfg := rest.CopyConfig(cfg)
	opCfg.Impersonate = rest.ImpersonationConfig{UserName: operatorSAUser}
	k8sAsOperator, err = client.New(opCfg, client.Options{Scheme: sc})
	Expect(err).NotTo(HaveOccurred())
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
	}, 2*time.Minute, 3*time.Second).Should(Succeed())
}

func deleteNamespace(name string) {
	_ = k8s.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}})
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
