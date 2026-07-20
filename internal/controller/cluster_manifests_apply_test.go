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

package controller

import (
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/restmapper"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/CrystalBackup/CrystalBackup/internal/manifests"
	"github.com/CrystalBackup/CrystalBackup/internal/sanitize"
)

// The Applier's CLUSTER-scoped path (ApplyOptions.ClusterScoped), against a real API server.
// This is the L9 change that most needed a real apiserver: a cluster-scoped object has no
// namespace, so the two things flipped off — metadata.namespace stamping and the namespaced
// dynamic interface — cannot be validated by a fake (which does not enforce scope). A fake
// would accept a namespace-stamped StorageClass; the real server rejects it.
var _ = Describe("manifest applier — cluster-scoped path (real API server)", func() {

	var applier *manifests.Applier

	BeforeEach(func() {
		dyn, err := dynamic.NewForConfig(cfg)
		Expect(err).NotTo(HaveOccurred())
		disco, err := discovery.NewDiscoveryClientForConfig(cfg)
		Expect(err).NotTo(HaveOccurred())
		applier = &manifests.Applier{
			Dynamic: dyn,
			Mapper:  restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(disco)),
		}
	})

	storageClass := func(name, provisioner string) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion":  "storage.k8s.io/v1",
			"kind":        "StorageClass",
			"metadata":    map[string]any{"name": name},
			"provisioner": provisioner,
		}}
	}

	// writeClusterSnapshot lays out a cluster-manifests tree exactly as ClusterDumper does — the
	// index carries an EMPTY namespace, which is the on-disk marker of a cluster-scoped snapshot.
	writeClusterSnapshot := func(objs ...*unstructured.Unstructured) string {
		GinkgoHelper()
		dir := GinkgoT().TempDir()
		idx := &manifests.Index{
			FormatVersion: manifests.IndexFormatVersion,
			BackupName:    "run-1",
			CapturedAt:    "2026-07-21T00:00:00Z",
		}
		for _, o := range objs {
			gvk := o.GroupVersionKind()
			path := manifests.StoragePath(gvk.Group, gvk.Kind, o.GetName())
			Expect(os.MkdirAll(filepath.Join(dir, filepath.Dir(path)), 0o755)).To(Succeed())
			raw, err := sanitize.Marshal(o)
			Expect(err).NotTo(HaveOccurred())
			Expect(os.WriteFile(filepath.Join(dir, path), raw, 0o644)).To(Succeed())
			idx.Resources = append(idx.Resources, manifests.IndexEntry{
				Group: gvk.Group, Version: gvk.Version, Kind: gvk.Kind, Name: o.GetName(), Path: path,
			})
		}
		raw, err := idx.Marshal()
		Expect(err).NotTo(HaveOccurred())
		Expect(os.WriteFile(filepath.Join(dir, manifests.IndexFileName), raw, 0o644)).To(Succeed())
		return dir
	}

	applyCluster := func(dir string, mode manifests.RestoreMode, dryRun bool) *manifests.Report {
		GinkgoHelper()
		sel, err := manifests.Selection{All: true}.Compile()
		Expect(err).NotTo(HaveOccurred())
		report, err := applier.Apply(ctx, manifests.ApplyOptions{
			SourceDir: dir, Mode: mode, DryRun: dryRun, Selection: sel,
			// No TargetNamespace — the whole point. Apply must not require one here.
			ClusterScoped: true,
		})
		Expect(err).NotTo(HaveOccurred())
		return report
	}

	liveStorageClass := func(name string) *storagev1.StorageClass {
		GinkgoHelper()
		sc := &storagev1.StorageClass{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: name}, sc)).To(Succeed())
		return sc
	}

	It("creates a cluster-scoped object with NO namespace and no TargetNamespace", func() {
		dir := writeClusterSnapshot(storageClass("cbapply-fast", "example.com/fast"))

		report := applyCluster(dir, manifests.ModeOverwrite, false)

		Expect(report.Applied).To(Equal(1))
		Expect(report.Failed).To(BeZero())
		sc := liveStorageClass("cbapply-fast")
		Expect(sc.Provisioner).To(Equal("example.com/fast"))
		// The object is cluster-scoped: it landed with no namespace. A stamped namespace would
		// have been rejected by the server (StorageClass is cluster-scoped), which is the whole
		// reason the fake could not test this.
		Expect(sc.Namespace).To(BeEmpty())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, sc) })
	})

	It("Recreate replaces an existing cluster-scoped object (new UID)", func() {
		Expect(k8sClient.Create(ctx, &storagev1.StorageClass{
			ObjectMeta:  metav1.ObjectMeta{Name: "cbapply-recreate"},
			Provisioner: "example.com/old",
		})).To(Succeed())
		before := liveStorageClass("cbapply-recreate").UID

		dir := writeClusterSnapshot(storageClass("cbapply-recreate", "example.com/new"))
		report := applyCluster(dir, manifests.ModeRecreate, false)

		Expect(report.Failed).To(BeZero())
		sc := liveStorageClass("cbapply-recreate")
		// StorageClass.provisioner is immutable, so a Recreate (delete+create) is the ONLY way to
		// change it — a fresh UID proves the object was actually replaced, not updated in place.
		Expect(sc.Provisioner).To(Equal("example.com/new"))
		Expect(sc.UID).NotTo(Equal(before))
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, sc) })
	})

	It("a dry run of a cluster-scoped restore persists nothing", func() {
		dir := writeClusterSnapshot(storageClass("cbapply-dryrun", "example.com/x"))

		report := applyCluster(dir, manifests.ModeOverwrite, true)

		Expect(report.Applied).To(Equal(1))
		err := k8sClient.Get(ctx, client.ObjectKey{Name: "cbapply-dryrun"}, &storagev1.StorageClass{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "a dry run must create nothing")
	})
})
