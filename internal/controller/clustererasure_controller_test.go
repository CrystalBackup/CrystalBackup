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
	"context"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/restic"
)

// createErasure creates a ClusterErasure and registers its cleanup.
func createErasure(name, location string, target cbv1.ErasureTarget, confirmation string) *cbv1.ClusterErasure {
	GinkgoHelper()
	er := &cbv1.ClusterErasure{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: cbv1.ClusterErasureSpec{
			LocationRef:  cbv1.LocalObjectReference{Name: location},
			Target:       target,
			Confirmation: confirmation,
		},
	}
	Expect(k8sClient.Create(ctx, er)).To(Succeed())
	DeferCleanup(func() { _ = k8sClient.Delete(context.Background(), er) })
	return er
}

// getErasureG fetches a ClusterErasure inside an Eventually block.
func getErasureG(g Gomega, name string) cbv1.ClusterErasure {
	var er cbv1.ClusterErasure
	g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: name}, &er)).To(Succeed())
	return er
}

var _ = Describe("ClusterErasureReconciler", func() {

	It("parks in AwaitingConfirmation and names the exact string to type back", func() {
		const (
			location = "cer-await-loc"
			name     = "cer-await"
		)
		seedInitializedRepo(location, "kek-cer-await", "s3-cer-await")
		createErasure(name, location, cbv1.ErasureTarget{Namespace: "team-x", PVC: "data"}, "")

		// The two-step is the point (R23): an absent confirmation must PARK, not be rejected, so
		// the object can be read — "this is what I am about to destroy" — before it is armed.
		Eventually(func(g Gomega) {
			er := getErasureG(g, name)
			g.Expect(er.Status.Phase).To(Equal(erasurePhaseAwaiting))
			cond := apimeta.FindStatusCondition(er.Status.Conditions, ConditionReady)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Message).To(ContainSubstring(`"team-x/data"`),
				"the message must state the exact confirmation value")
			g.Expect(cond.Message).To(ContainSubstring("PERMANENTLY"))
		}, initTimeout, initPoll).Should(Succeed())
	})

	It("fails a confirmation that does not match the target", func() {
		const (
			location = "cer-wrong-loc"
			name     = "cer-wrong"
		)
		seedInitializedRepo(location, "kek-cer-wrong", "s3-cer-wrong")
		createErasure(name, location, cbv1.ErasureTarget{Namespace: "team-x"}, "team-y")

		Eventually(func(g Gomega) {
			er := getErasureG(g, name)
			g.Expect(er.Status.Phase).To(Equal(erasurePhaseFailed))
			cond := apimeta.FindStatusCondition(er.Status.Conditions, ConditionReady)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Reason).To(Equal("ConfirmationMismatch"))
		}, initTimeout, initPoll).Should(Succeed())
	})

	It("refuses a target that selects nothing rather than erasing everything", func() {
		const (
			location = "cer-empty-loc"
			name     = "cer-empty"
		)
		seedInitializedRepo(location, "kek-cer-empty", "s3-cer-empty")
		// A bare PVC name is not an identity — the same name exists in many namespaces. Treating
		// it as "match anything with pvc=data" would cross tenants; treating it as "no filter"
		// would erase the entire repository.
		createErasure(name, location, cbv1.ErasureTarget{PVC: "data"}, "")

		Eventually(func(g Gomega) {
			er := getErasureG(g, name)
			g.Expect(er.Status.Phase).To(Equal(erasurePhaseFailed))
			cond := apimeta.FindStatusCondition(er.Status.Conditions, ConditionReady)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Reason).To(Equal("InvalidTarget"))
		}, initTimeout, initPoll).Should(Succeed())
	})

	It("blocks on an Immutable location instead of reporting a deletion that cannot happen", func() {
		const (
			location = "cer-immutable-loc"
			name     = "cer-immutable"
		)
		createKEKSecret("kek-cer-imm", generateAgeIdentity())
		createS3CredsSecret("s3-cer-imm")
		registerRepoCleanup(location)
		loc := newTestLocation(location, "kek-cer-imm", "s3-cer-imm", false)
		loc.Spec.Mode = cbv1.LocationModeImmutable
		createTestLocation(loc)

		createErasure(name, location, cbv1.ErasureTarget{Namespace: "team-x"}, "team-x")

		// Object Lock forbids the deletion. "Completed" here would be the worst possible answer to
		// a GDPR request: a compliance record asserting removal while the objects are still there.
		Eventually(func(g Gomega) {
			er := getErasureG(g, name)
			g.Expect(er.Status.Phase).To(Equal(erasurePhaseBlocked))
			cond := apimeta.FindStatusCondition(er.Status.Conditions, ConditionReady)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Reason).To(Equal("ImmutableLocation"))
			g.Expect(cond.Message).To(ContainSubstring("nothing has been erased"))
		}, initTimeout, initPoll).Should(Succeed())
	})

	It("completes without touching the repository when the scope matches nothing", func() {
		const (
			location = "cer-nomatch-loc"
			name     = "cer-nomatch"
		)
		seedInitializedRepo(location, "kek-cer-nomatch", "s3-cer-nomatch")
		restoreLister.seed(nil)

		createErasure(name, location, cbv1.ErasureTarget{Namespace: "ghost-ns"}, "ghost-ns")

		// An empty scope is a legitimate success — the data is already not there — and it must not
		// enqueue a forget, which would run an erasure command for nothing.
		Eventually(func(g Gomega) {
			er := getErasureG(g, name)
			g.Expect(er.Status.Phase).To(Equal(erasurePhaseCompleted))
			g.Expect(er.Status.SnapshotsForgotten).To(BeZero())
			g.Expect(apimeta.IsStatusConditionTrue(er.Status.Conditions, conditionErasureComplete)).To(BeTrue())
		}, initTimeout, initPoll).Should(Succeed())
	})

	It("records how many snapshots it is about to erase BEFORE erasing them", func() {
		const (
			location = "cer-count-loc"
			name     = "cer-count"
		)
		seedInitializedRepo(location, "kek-cer-count", "s3-cer-count")
		restoreLister.seed([]restic.Snapshot{
			{ID: "s1", Tags: []string{restic.TagBase, restic.Tag(restic.TagKeyNamespace, "team-x")}},
			{ID: "s2", Tags: []string{restic.TagBase, restic.Tag(restic.TagKeyNamespace, "team-x")}},
			{ID: "s3", Tags: []string{restic.TagBase, restic.Tag(restic.TagKeyNamespace, "other")}},
		})

		createErasure(name, location, cbv1.ErasureTarget{Namespace: "team-x"}, "team-x")

		// The count is the compliance record, and afterwards the evidence is gone — so it has to be
		// persisted before the forget, not derived from it. Two snapshots match; the third belongs
		// to another namespace and must not be counted (nor, later, erased).
		Eventually(func(g Gomega) {
			er := getErasureG(g, name)
			g.Expect(er.Status.SnapshotsForgotten).To(Equal(int32(2)),
				"the scope must exclude the snapshot belonging to another namespace")
			g.Expect(er.Status.Phase).To(Equal(erasurePhaseRunning))
		}, initTimeout, initPoll).Should(Succeed())
	})
})

// TestErasurePruneMaxRepackSizeToleratesNoMaintenanceBlock: spec.maintenance is optional and a
// POINTER, and erasure is the only caller that reaches prune without one.
//
// The controller dereferenced it unconditionally, so a ClusterErasure against a location that had
// simply not configured scheduled maintenance — the default — PANICKED the reconciler. A panic
// requeues, so it panicked again on every retry, on the most destructive path in the system, with
// the object frozen at Running and 2 snapshots already counted.
//
// The maintenance controller dereferences the same field and is safe only by construction: it
// arrives from a prune/check SCHEDULE, which cannot exist without the block. Erasure arrives from a
// ClusterErasure and knows nothing about maintenance, which is why this needs its own accessor.
func TestErasurePruneMaxRepackSizeToleratesNoMaintenanceBlock(t *testing.T) {
	none := &cbv1.ClusterBackupLocation{}
	if got := erasurePruneMaxRepackSize(none); got != "" {
		t.Fatalf("erasurePruneMaxRepackSize on a location with no maintenance block = %q, want empty", got)
	}
	// And the value is still honoured when the block IS there.
	withCap := &cbv1.ClusterBackupLocation{}
	withCap.Spec.Maintenance = &cbv1.MaintenanceSpec{PruneMaxRepackSize: "2G"}
	if got := erasurePruneMaxRepackSize(withCap); got != "2G" {
		t.Fatalf("erasurePruneMaxRepackSize = %q, want 2G", got)
	}
}
