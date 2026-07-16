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
	"fmt"
	"os"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/CrystalBackup/CrystalBackup/test/utils"
)

const (
	operatorNS = "crystal-backup-system"
	m0NS       = "crucible-m0"
)

// The 12 CRDs of the crystalbackup.io/v1alpha1 surface (spec/02-api.md).
var crdNames = []string{
	"clusterbackuplocations.crystalbackup.io",
	"clusterbackupschedules.crystalbackup.io",
	"clusterbackups.crystalbackup.io",
	"clusterrestores.crystalbackup.io",
	"clustererasures.crystalbackup.io",
	"clusterbackupexternalsyncs.crystalbackup.io",
	"backuplocations.crystalbackup.io",
	"backupschedules.crystalbackup.io",
	"backups.crystalbackup.io",
	"restores.crystalbackup.io",
	"backupexternalsyncs.crystalbackup.io",
	"backuprepositories.crystalbackup.io",
}

// M0 — scaffolding on a REAL cluster: chart installed, all 12 CRDs served,
// every kind round-trips through the live API server (the kind-agnostic
// equivalent of the envtest exit criterion, plus the helm artifacts).
var _ = Describe("Milestone M0 — CRDs and chart on a real cluster", Label("m0"), func() {

	It("has all 12 CRDs Established", func() {
		for _, name := range crdNames {
			crd := &unstructured.Unstructured{}
			crd.SetGroupVersionKind(crdGVK())
			Expect(k8s.Get(ctx, client.ObjectKey{Name: name}, crd)).To(Succeed(),
				"CRD %s must exist", name)

			conditions, _, err := unstructured.NestedSlice(crd.Object, "status", "conditions")
			Expect(err).NotTo(HaveOccurred())
			established := false
			for _, c := range conditions {
				cond, ok := c.(map[string]interface{})
				if !ok {
					continue
				}
				if cond["type"] == "Established" && cond["status"] == "True" {
					established = true
				}
			}
			Expect(established).To(BeTrue(), "CRD %s is not Established", name)
		}
	})

	It("has the chart artifacts (namespace, deployment, RBAC, service account)", func() {
		var ns corev1.Namespace
		Expect(k8s.Get(ctx, client.ObjectKey{Name: operatorNS}, &ns)).To(Succeed())
		Expect(ns.Labels["pod-security.kubernetes.io/enforce"]).To(Equal("baseline"))

		var deploy appsv1.Deployment
		Expect(k8s.Get(ctx, client.ObjectKey{Namespace: operatorNS, Name: "crystal-backup"}, &deploy)).To(Succeed())

		var sa corev1.ServiceAccount
		Expect(k8s.Get(ctx, client.ObjectKey{Namespace: operatorNS, Name: "crystal-backup-operator"}, &sa)).To(Succeed())

		for _, role := range []string{"crystal-backup-operator", "crystal-backup-tenant", "crystal-backup-admin"} {
			var cr rbacv1.ClusterRole
			Expect(k8s.Get(ctx, client.ObjectKey{Name: role}, &cr)).To(Succeed(),
				"ClusterRole %s must exist", role)
		}
	})

	It("runs the operator (once a released image exists)", func() {
		var deploy appsv1.Deployment
		Expect(k8s.Get(ctx, client.ObjectKey{Namespace: operatorNS, Name: "crystal-backup"}, &deploy)).To(Succeed())

		if os.Getenv("CRUCIBLE_EXPECT_OPERATOR_READY") != "true" {
			Skip("no released operator image yet (pre-v0.0.1) — " +
				"set CRUCIBLE_EXPECT_OPERATOR_READY=true once GHCR has one")
		}

		Eventually(func(g Gomega) {
			var d appsv1.Deployment
			g.Expect(k8s.Get(ctx, client.ObjectKey{Namespace: operatorNS, Name: "crystal-backup"}, &d)).To(Succeed())
			available := false
			for _, c := range d.Status.Conditions {
				if c.Type == appsv1.DeploymentAvailable && c.Status == corev1.ConditionTrue {
					available = true
				}
			}
			g.Expect(available).To(BeTrue(), "operator deployment not Available")
		}, 5*time.Minute, 5*time.Second).Should(Succeed())
	})

	Describe("live round-trip of every kind", Ordered, func() {
		BeforeAll(func() {
			ensureNamespace(m0NS)
			// Clear leftovers from a previous aborted run (cluster-scoped kinds
			// survive namespace deletion).
			for _, obj := range utils.SampleObjects(m0NS) {
				_ = k8s.Delete(ctx, obj)
			}
		})
		AfterAll(func() {
			for _, obj := range utils.SampleObjects(m0NS) {
				_ = k8s.Delete(ctx, obj)
			}
			deleteNamespace(m0NS)
		})

		It("creates, reads back and deletes a minimal CR of each of the 12 kinds", func() {
			all := utils.SampleObjects(m0NS)
			Expect(all).To(HaveLen(12))

			for _, obj := range all {
				kind := fmt.Sprintf("%T", obj)

				// A leftover may still be terminating — wait for the slot.
				Eventually(func() error {
					err := k8s.Create(ctx, obj)
					if apierrors.IsAlreadyExists(err) {
						return fmt.Errorf("%s %s still exists from a previous run", kind, obj.GetName())
					}
					return err
				}, time.Minute, 2*time.Second).Should(Succeed(), "create %s", kind)

				fresh := obj.DeepCopyObject().(client.Object)
				Expect(k8s.Get(ctx, client.ObjectKeyFromObject(obj), fresh)).To(Succeed(),
					"get %s/%s after create", kind, obj.GetName())
				Expect(fresh.GetUID()).NotTo(BeEmpty(), "%s not persisted", kind)

				Expect(k8s.Delete(ctx, fresh)).To(Succeed(), "delete %s/%s", kind, obj.GetName())
				GinkgoWriter.Printf("ok  %-60s live round-trip (uid=%s)\n", kind, fresh.GetUID())
			}
		})
	})
})
