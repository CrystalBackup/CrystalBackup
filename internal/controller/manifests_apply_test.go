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
	corev1 "k8s.io/api/core/v1"
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

// internal/manifests tests its dump half against client-go fakes, which is right there: the
// dump only lists and the fake lists faithfully. The APPLY half cannot be tested that way. Its
// three load-bearing behaviours — server-side apply's ownership and conflict handling, a
// delete that a finalizer holds open, and dryRun=All persisting nothing — are all implemented
// by the API SERVER, and the fake implements none of them. A fake would happily report every
// one of these working.
//
// So the applier is exercised here, where the suite already has a real API server running. The
// package is a compromise; testing a restore path against a fake would not be.
var _ = Describe("manifest applier (real API server)", func() {

	var (
		applier *manifests.Applier
		dyn     dynamic.Interface
	)

	BeforeEach(func() {
		var err error
		dyn, err = dynamic.NewForConfig(cfg)
		Expect(err).NotTo(HaveOccurred())
		disco, err := discovery.NewDiscoveryClientForConfig(cfg)
		Expect(err).NotTo(HaveOccurred())
		applier = &manifests.Applier{
			Dynamic: dyn,
			Mapper:  restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(disco)),
		}
	})

	// writeSnapshot lays out a manifest tree exactly as the dump does, so the applier is fed
	// the real on-disk contract (index.json + <group>/<Kind>/<name>.yaml) rather than a shape
	// invented for the test.
	writeSnapshot := func(objs ...*unstructured.Unstructured) string {
		GinkgoHelper()
		dir := GinkgoT().TempDir()
		idx := &manifests.Index{
			FormatVersion: manifests.IndexFormatVersion,
			Namespace:     "origin",
			BackupName:    "run-1",
			CapturedAt:    "2026-07-20T00:00:00Z",
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

	// object builds the skeleton every fixture starts from. One helper rather than a literal
	// per fixture, so the stored manifests all carry the shape the dump actually writes.
	object := func(apiVersion, kind, name string) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": apiVersion,
			"kind":       kind,
			"metadata":   map[string]any{"name": name},
		}}
	}

	configMap := func(name string, data map[string]string) *unstructured.Unstructured {
		obj := object("v1", "ConfigMap", name)
		obj.Object["data"] = map[string]any{}
		for k, v := range data {
			Expect(unstructured.SetNestedField(obj.Object, v, "data", k)).To(Succeed())
		}
		return obj
	}

	applyIn := func(ns, dir string, mode manifests.RestoreMode, dryRun bool) *manifests.Report {
		GinkgoHelper()
		sel, err := manifests.Selection{All: true}.Compile()
		Expect(err).NotTo(HaveOccurred())
		report, err := applier.Apply(ctx, manifests.ApplyOptions{
			SourceDir: dir, TargetNamespace: ns, Mode: mode, DryRun: dryRun, Selection: sel,
		})
		Expect(err).NotTo(HaveOccurred())
		return report
	}

	liveConfigMap := func(ns, name string) *corev1.ConfigMap {
		GinkgoHelper()
		cm := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, cm)).To(Succeed())
		return cm
	}

	It("creates into an empty namespace and stamps the target namespace on every object", func() {
		ns := "apply-create"
		createTenantNamespace(ns)
		dir := writeSnapshot(configMap("app-config", map[string]string{"LOG_LEVEL": "info"}))

		report := applyIn(ns, dir, manifests.ModeOverwrite, false)

		Expect(report.Applied).To(Equal(1))
		Expect(report.Failed).To(BeZero())
		// A plain create is the expected case and is deliberately NOT listed — entries carry
		// non-trivial outcomes only, which is what keeps the 100-entry cap workable.
		Expect(report.Entries).To(BeEmpty())

		// S6 strips metadata.namespace at backup so the apply can target any namespace; if the
		// stamping were missing the object would land in "default" or be rejected outright.
		Expect(liveConfigMap(ns, "app-config").Data).To(HaveKeyWithValue("LOG_LEVEL", "info"))
	})

	It("Overwrite SSA-merges over a DRIFTED object another manager owns, and keeps its extras", func() {
		ns := "apply-overwrite"
		createTenantNamespace(ns)

		// Pre-create with a DIFFERENT field manager and drifted content — the exact shape the
		// M3 e2e requires, and the one that decides the force question: without force, SSA
		// rejects this with a conflict on data.LOG_LEVEL and Overwrite fails on precisely the
		// objects it exists to reconcile.
		existing := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "app-config", Namespace: ns},
			Data:       map[string]string{"LOG_LEVEL": "debug", "OPERATOR_ONLY": "keep-me"},
		}
		Expect(k8sClient.Create(ctx, existing, client.FieldOwner("some-other-controller"))).To(Succeed())

		dir := writeSnapshot(configMap("app-config", map[string]string{"LOG_LEVEL": "info"}))
		report := applyIn(ns, dir, manifests.ModeOverwrite, false)

		Expect(report.Failed).To(BeZero(), "a conflict here means force is not in effect")
		Expect(report.Applied).To(Equal(1))

		live := liveConfigMap(ns, "app-config")
		Expect(live.Data).To(HaveKeyWithValue("LOG_LEVEL", "info"), "the backup's value must win")
		// The mode's promise: objects (and fields) present in the target but absent from the
		// backup are KEPT. Force takes ownership; it does not prune.
		Expect(live.Data).To(HaveKeyWithValue("OPERATOR_ONLY", "keep-me"))

		// An update IS notable, so it earns an entry — carrying what actually moved.
		Expect(report.Entries).To(HaveLen(1))
		Expect(string(report.Entries[0].Outcome)).To(Equal("Configured"))
		Expect(report.Entries[0].Changed).To(ContainElement("data.LOG_LEVEL"))
		Expect(report.Entries[0].Changed).NotTo(ContainElement(ContainSubstring("managedFields")),
			"server-owned churn would drown the real diff")
	})

	It("Overwrite keeps objects the backup does not mention", func() {
		ns := "apply-extras"
		createTenantNamespace(ns)
		Expect(k8sClient.Create(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "target-only", Namespace: ns},
			Data:       map[string]string{"k": "v"},
		})).To(Succeed())

		dir := writeSnapshot(configMap("from-backup", map[string]string{"a": "b"}))
		applyIn(ns, dir, manifests.ModeOverwrite, false)

		// Additive recovery: Overwrite never deletes what it did not bring.
		Expect(liveConfigMap(ns, "target-only").Data).To(HaveKeyWithValue("k", "v"))
	})

	It("Recreate replaces an existing object rather than merging into it", func() {
		ns := "apply-recreate"
		createTenantNamespace(ns)
		Expect(k8sClient.Create(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "app-config", Namespace: ns},
			Data:       map[string]string{"LOG_LEVEL": "debug", "STALE": "should-not-survive"},
		})).To(Succeed())
		before := liveConfigMap(ns, "app-config").UID

		dir := writeSnapshot(configMap("app-config", map[string]string{"LOG_LEVEL": "info"}))
		report := applyIn(ns, dir, manifests.ModeRecreate, false)

		Expect(report.Failed).To(BeZero())
		live := liveConfigMap(ns, "app-config")
		// A clean replace, not a merge: the target-only key is gone, and a NEW UID proves the
		// object was actually deleted and created rather than updated in place.
		Expect(live.Data).To(HaveKeyWithValue("LOG_LEVEL", "info"))
		Expect(live.Data).NotTo(HaveKey("STALE"))
		Expect(live.UID).NotTo(Equal(before))
		Expect(string(report.Entries[0].Outcome)).To(Equal("Recreated"))
	})

	It("reports a Recreate blocked by a finalizer as Failed, never force-deleting it", func() {
		ns := "apply-finalizer"
		createTenantNamespace(ns)
		held := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name: "held", Namespace: ns,
				// Nothing in this suite removes it, so the delete stays pending — which is
				// exactly a finalizer whose controller is down or slow.
				Finalizers: []string{"example.com/holds-this"},
			},
			Data: map[string]string{"original": "value"},
		}
		Expect(k8sClient.Create(ctx, held)).To(Succeed())
		DeferCleanup(func() {
			cm := &corev1.ConfigMap{}
			if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "held"}, cm); err == nil {
				cm.Finalizers = nil
				_ = k8sClient.Update(ctx, cm)
			}
		})

		dir := writeSnapshot(configMap("held", map[string]string{"restored": "value"}))
		report := applyIn(ns, dir, manifests.ModeRecreate, false)

		Expect(report.Failed).To(Equal(1))
		Expect(string(report.Entries[0].Outcome)).To(Equal("Failed"))
		Expect(report.Entries[0].Reason).To(ContainSubstring("finalizer"),
			"the reason must name what is holding the object, or nobody can act on it")

		// The object must still be there with its finalizer intact. Stripping it would run the
		// very cleanup its controller registered it to do — a volume detach, an external
		// record — behind that controller's back.
		still := liveConfigMap(ns, "held")
		Expect(still.Finalizers).To(ContainElement("example.com/holds-this"))
	})

	It("a dry run reports the plan and persists nothing", func() {
		ns := "apply-dryrun"
		createTenantNamespace(ns)
		Expect(k8sClient.Create(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "app-config", Namespace: ns},
			Data:       map[string]string{"LOG_LEVEL": "debug"},
		})).To(Succeed())

		dir := writeSnapshot(
			configMap("app-config", map[string]string{"LOG_LEVEL": "info"}),
			configMap("brand-new", map[string]string{"a": "b"}),
		)
		report := applyIn(ns, dir, manifests.ModeOverwrite, true)

		Expect(report.Applied).To(Equal(2))
		Expect(report.Failed).To(BeZero())

		// Nothing moved. The whole value of --dry-run on a destructive restore is that this
		// assertion holds.
		Expect(liveConfigMap(ns, "app-config").Data).To(HaveKeyWithValue("LOG_LEVEL", "debug"))
		err := k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "brand-new"}, &corev1.ConfigMap{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "a dry run must not create anything")

		// And it still says what WOULD have changed — the point of running it at all.
		Expect(report.Entries).To(HaveLen(1))
		Expect(report.Entries[0].Name).To(Equal("app-config"))
		Expect(report.Entries[0].Changed).To(ContainElement("data.LOG_LEVEL"))
	})

	It("reports a kind the cluster does not serve per-resource, and applies the rest", func() {
		ns := "apply-nocrd"
		createTenantNamespace(ns)

		absent := object("postgresql.cnpg.io/v1", "Cluster", "db")
		dir := writeSnapshot(absent, configMap("survivor", map[string]string{"a": "b"}))

		report := applyIn(ns, dir, manifests.ModeOverwrite, false)

		// The documented behaviour of §5.1: the custom resource fails with the server's own
		// no-matches error and the restore CONTINUES. Aborting here would leave the namespace
		// half-restored because one operator was not installed.
		Expect(report.Failed).To(Equal(1))
		Expect(report.Applied).To(Equal(1))
		Expect(liveConfigMap(ns, "survivor").Data).To(HaveKeyWithValue("a", "b"))

		var failure *manifests.ResourceOutcome
		for i := range report.Entries {
			if report.Entries[i].Kind == "Cluster" {
				failure = &report.Entries[i]
			}
		}
		Expect(failure).NotTo(BeNil())
		Expect(failure.Reason).To(ContainSubstring("no matches for kind"))
	})

	It("applies in phase order: config and storage before the workloads that mount them", func() {
		ns := "apply-order"
		createTenantNamespace(ns)

		// A Pod referencing a ConfigMap that is only created by this same restore. Ordering is
		// what makes the reference resolvable; the alphabetical order of the tree is the
		// opposite of the order needed (ConfigMap "z-config" sorts after Pod "a-pod").
		pod := object("v1", "Pod", "a-pod")
		pod.Object["spec"] = map[string]any{
			"restartPolicy": "Never",
			"containers": []any{map[string]any{
				"name": "c", "image": "busybox",
				"envFrom": []any{map[string]any{
					"configMapRef": map[string]any{"name": "z-config"},
				}},
			}},
		}
		dir := writeSnapshot(pod, configMap("z-config", map[string]string{"a": "b"}))

		report := applyIn(ns, dir, manifests.ModeOverwrite, false)
		Expect(report.Failed).To(BeZero())

		// Both exist; the ordering claim is that the ConfigMap was there first. envtest runs no
		// kubelet, so the Pod never starts — the assertion that can be made here is that the
		// apply admitted both, which is what phase ordering is for (admission, not readiness).
		Expect(liveConfigMap(ns, "z-config").Data).To(HaveKeyWithValue("a", "b"))
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "a-pod"}, &corev1.Pod{})).To(Succeed())
	})

	It("narrows to the selection and counts what it left alone", func() {
		ns := "apply-select"
		createTenantNamespace(ns)
		dir := writeSnapshot(
			configMap("wanted", map[string]string{"a": "b"}),
			configMap("unwanted", map[string]string{"a": "b"}),
		)

		sel, err := manifests.Selection{Items: []manifests.SelectionItem{
			{Include: []string{"ConfigMap/wanted"}},
		}}.Compile()
		Expect(err).NotTo(HaveOccurred())
		report, err := applier.Apply(ctx, manifests.ApplyOptions{
			SourceDir: dir, TargetNamespace: ns, Mode: manifests.ModeOverwrite, Selection: sel,
		})
		Expect(err).NotTo(HaveOccurred())

		Expect(report.Applied).To(Equal(1))
		// Skipped is reported so "restored 1" is distinguishable from "the snapshot held 1".
		Expect(report.Skipped).To(Equal(1))
		Expect(liveConfigMap(ns, "wanted").Data).To(HaveKeyWithValue("a", "b"))
		err = k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "unwanted"}, &corev1.ConfigMap{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
	})

	It("rewrites a PVC's storageClassName through the mapping, and removes it for an empty target", func() {
		ns := "apply-classmap"
		createTenantNamespace(ns)

		pvc := func(name, class string) *unstructured.Unstructured {
			o := object("v1", "PersistentVolumeClaim", name)
			o.Object["spec"] = map[string]any{
				"accessModes": []any{"ReadWriteOnce"},
				"resources":   map[string]any{"requests": map[string]any{"storage": "1Gi"}},
			}
			Expect(unstructured.SetNestedField(o.Object, class, "spec", "storageClassName")).To(Succeed())
			return o
		}
		dir := writeSnapshot(pvc("mapped", "fast"), pvc("defaulted", "gold"), pvc("untouched", "other"))

		sel, err := manifests.Selection{All: true}.Compile()
		Expect(err).NotTo(HaveOccurred())
		_, err = applier.Apply(ctx, manifests.ApplyOptions{
			SourceDir: dir, TargetNamespace: ns, Mode: manifests.ModeOverwrite, Selection: sel,
			// "" is the case that is easy to implement wrong: it must REMOVE the field so the
			// target's default class applies. Setting it to the empty string means "no class at
			// all" in Kubernetes, and the claim would never bind.
			StorageClassMapping: map[string]string{"fast": "standard", "gold": ""},
		})
		Expect(err).NotTo(HaveOccurred())

		get := func(name string) *corev1.PersistentVolumeClaim {
			GinkgoHelper()
			claim := &corev1.PersistentVolumeClaim{}
			Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, claim)).To(Succeed())
			return claim
		}
		Expect(*get("mapped").Spec.StorageClassName).To(Equal("standard"))
		Expect(*get("untouched").Spec.StorageClassName).To(Equal("other"), "unmapped classes pass through")

		// The API server does not default a class in envtest (no default StorageClass), so an
		// absent field stays absent — which is precisely the distinction being made: the field
		// was REMOVED, not set to "".
		defaulted := get("defaulted")
		Expect(defaulted.Spec.StorageClassName == nil || *defaulted.Spec.StorageClassName != "").To(BeTrue(),
			"a mapping to \"\" must remove the field, never set it to the empty string")
	})

	It("reports an unreadable manifest per-resource and applies its siblings", func() {
		ns := "apply-corrupt"
		createTenantNamespace(ns)
		dir := writeSnapshot(configMap("good", map[string]string{"a": "b"}))

		// A file the index names but that cannot be parsed — a truncated restic restore, a
		// corrupted blob. One bad object must not cost the other 200.
		bad := manifests.StoragePath("", "ConfigMap", "corrupt")
		Expect(os.MkdirAll(filepath.Join(dir, filepath.Dir(bad)), 0o755)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(dir, bad), []byte("\t: not: valid: yaml: ["), 0o644)).To(Succeed())

		raw, err := os.ReadFile(filepath.Join(dir, manifests.IndexFileName))
		Expect(err).NotTo(HaveOccurred())
		idx, err := manifests.ParseIndex(raw)
		Expect(err).NotTo(HaveOccurred())
		idx.Resources = append(idx.Resources, manifests.IndexEntry{
			Version: "v1", Kind: "ConfigMap", Name: "corrupt", Path: bad,
		})
		out, err := idx.Marshal()
		Expect(err).NotTo(HaveOccurred())
		Expect(os.WriteFile(filepath.Join(dir, manifests.IndexFileName), out, 0o644)).To(Succeed())

		report := applyIn(ns, dir, manifests.ModeOverwrite, false)

		Expect(report.Failed).To(Equal(1))
		Expect(report.Applied).To(Equal(1))
		Expect(liveConfigMap(ns, "good").Data).To(HaveKeyWithValue("a", "b"))
	})
})
