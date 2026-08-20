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
	"slices"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
)

// ---------------------------------------------------------------------------
// M6 acceptance — the SNAPSHOT PRE-CHECK and the PROGRESS DEADLINE (delta 9).
//
// Both exist to kill the same failure: a backup that HANGS. A VolumeSnapshot
// nobody can serve does not fail — it sits in Snapshotting, which looks exactly
// like ordinary progress, so nothing alerts, the run never ends, and every
// attempt leaves another live snapshot object behind. That is strictly worse
// than a red backup, and it is the only failure mode in this product that is
// invisible by construction.
//
// Two independent guards, because they catch different halves:
//
//   - The PRE-CHECK, before anything is created: the VolumeSnapshotClass names
//     a snapshotter Secret that does not exist, so no snapshot on it could ever
//     be authenticated. Verdict: the volume Fails immediately, naming the
//     Secret — and NOTHING is created, which is the ordering property this file
//     asserts hardest.
//   - The PROGRESS DEADLINE, after: a snapshot request that no controller ever
//     picks up. Verdict: the volume Fails after snapshotProgressDeadline rather
//     than waiting for the suite's own timeout to notice.
//
// This spec is DESTRUCTIVE to cluster-wide storage machinery for the length of
// its own run — it deletes a rook CSI Secret and stops the external
// snapshot-controller — and it therefore restores each thing before the next It
// begins. Ordered, and the crucible suite runs serially, so nothing else is
// in flight while it does.
//
// COST: the deadline case waits out a real fifteen-minute deadline. There is no
// knob to shorten it, deliberately: the deadline is a compile-time constant of
// the operator, and a test that could shorten it would be testing a product
// nobody ships (this suite's standing rule — no tunable tolerances). Budget
// ~20 minutes of wall clock for the last It.
// ---------------------------------------------------------------------------

const (
	// m6PrecheckNS is this spec's own tenant namespace — created here, not part of the seed, so
	// the destructive steps below cannot affect any other spec's data.
	m6PrecheckNS = "m6-precheck"
	// m6PrecheckPVC is the one volume backed up. Small: this spec is about admission decisions,
	// not throughput.
	m6PrecheckPVC = "probe"
	// m6PrecheckSnapClass is the VolumeSnapshotClass whose snapshotter Secret is the subject.
	// ceph-block is the crucible's default RBD class (deploy/manifests/ceph-storage.yaml).
	m6PrecheckSnapClass = "ceph-block"
	// m6PrecheckStorageClass is the matching StorageClass.
	m6PrecheckStorageClass = "ceph-block"

	// m6RookOperatorNS / m6RookOperatorDeploy is the rook-ceph operator. It is stopped for the
	// duration of the missing-Secret case, because it OWNS the CSI Secrets: left running, it can
	// re-create the Secret between the delete and the operator's reconcile, and the spec would
	// then observe a perfectly healthy backup and fail with a message about the wrong thing.
	m6RookOperatorNS     = "rook-ceph"
	m6RookOperatorDeploy = "rook-ceph-operator"

	// m6SnapshotControllerNS is where the snapshot-controller lives. Its NAME is discovered, not
	// declared — see m6FindSnapshotController.
	//
	// READ THIS BEFORE POINTING THE SPEC AT THE RBD PROVISIONER INSTEAD. The progress deadline
	// fires only when the origin VolumeSnapshot has NEITHER a bound VolumeSnapshotContent NOR a
	// recorded error — and the component that writes both of those is the snapshot-controller, not
	// the per-driver csi-snapshotter sidecar. Scaling `csi-rbdplugin-provisioner` to zero kills the
	// sidecar, but the snapshot-controller still binds a VolumeSnapshotContent within seconds, so
	// the snapshot IS acknowledged, the deadline correctly does NOT fire, and a spec pointed there
	// would be red against a correct implementation. The sidecar-only failure is a real gap in the
	// deadline's coverage and is recorded as such — it is not something this spec can assert.
	m6SnapshotControllerNS = "kube-system"
	// m6SnapshotControllerMatch is the substring that identifies it. The name is distro-dependent:
	// deploy/deploy.sh installs kubernetes-csi's `snapshot-controller` only when the cluster ships
	// no VolumeSnapshot CRDs, and RKE2 — which the crucible runs — bundles its own
	// `rke2-snapshot-controller` instead. The deploy script matches it exactly this way, and a
	// hard-coded name here would fail on whichever of the two the cluster happens to have.
	m6SnapshotControllerMatch = "snapshot-controller"
)

var _ = Describe("M6 — snapshot pre-check and progress deadline", Label("m6", "precheck"), Ordered, func() {
	// One run name per case, campaign-scoped (see crucibleRunName): a fixed name would collide
	// with a previous campaign's snapshots in the shared repository.
	var (
		refusedRun  = crucibleRunName("m6-precheck-refused")
		restoredRun = crucibleRunName("m6-precheck-restored")
		stalledRun  = crucibleRunName("m6-precheck-stalled")
	)

	// The snapshotter Secret coordinates, READ OFF THE CLASS rather than hard-coded, exactly as
	// the operator's pre-check reads them. If rook ever renames its CSI Secret, this spec follows
	// it instead of failing for an unrelated reason.
	var secretNS, secretName string
	// secretBackup is the captured Secret, replayed to put the cluster back.
	var secretBackup *corev1.Secret
	// snapControllerDeploy is the snapshot-controller Deployment's NAME, discovered in BeforeAll
	// (see m6SnapshotControllerMatch).
	var snapControllerDeploy string
	// The replica counts this spec stops and must restore. Captured, never assumed: the
	// snapshot-controller ships with more than one replica in some releases, and an AfterAll that
	// "restored" it to a hard-coded 1 would leave the cluster quietly degraded for every campaign
	// after this one.
	var rookReplicas, snapControllerReplicas int32

	BeforeAll(func() {
		By("Given the shared cluster-DR repository")
		m1EnsureSharedRepository()

		By("And a small Ceph RBD volume in this spec's own namespace")
		ensureNamespace(m6PrecheckNS)
		startPVCConsumer(m6PrecheckNS, m6PrecheckPVC, m6PrecheckStorageClass)

		By("And the snapshotter Secret the ceph-block VolumeSnapshotClass names")
		secretNS, secretName = m6SnapshotterSecretRef(m6PrecheckSnapClass)
		Expect(secretName).NotTo(BeEmpty(),
			"VolumeSnapshotClass %s declares no snapshotter Secret — this spec has nothing to remove, and "+
				"the pre-check it exercises would (correctly) have nothing to check", m6PrecheckSnapClass)
		secretBackup = m6CaptureSecret(secretNS, secretName)

		By("And the identity and size of the two Deployments this spec stops")
		snapControllerDeploy = m6FindSnapshotController()
		rookReplicas = m6DeploymentReplicas(m6RookOperatorNS, m6RookOperatorDeploy)
		snapControllerReplicas = m6DeploymentReplicas(m6SnapshotControllerNS, snapControllerDeploy)
	})

	AfterAll(func() {
		// Belt and braces: every It restores what it broke, but a spec that fails mid-way must not
		// leave the cluster's storage machinery down for the rest of the campaign.
		m6RestoreSecret(secretBackup)
		// Restore only what BeforeAll actually captured. AfterAll runs even when BeforeAll failed,
		// and the zero value of these is 0 — so an unguarded call here would SCALE THE CLUSTER'S
		// STORAGE OPERATOR AND SNAPSHOT-CONTROLLER TO ZERO as a cleanup step, on the very run that
		// already went wrong. A restore that can destroy is not a restore.
		if rookReplicas > 0 {
			m6ScaleDeployment(m6RookOperatorNS, m6RookOperatorDeploy, rookReplicas)
		}
		if snapControllerReplicas > 0 && snapControllerDeploy != "" {
			m6ScaleDeployment(m6SnapshotControllerNS, snapControllerDeploy, snapControllerReplicas)
		}
		deleteNamespace(m6PrecheckNS)
	})

	It("fails a volume whose snapshotter Secret is missing, names it, and creates NO VolumeSnapshot", func() {
		By("Given the rook operator is stopped so it cannot put the Secret back mid-test")
		m6ScaleDeployment(m6RookOperatorNS, m6RookOperatorDeploy, 0)
		DeferCleanup(func() { m6ScaleDeployment(m6RookOperatorNS, m6RookOperatorDeploy, rookReplicas) })

		By(fmt.Sprintf("And the snapshotter Secret %s/%s is deleted", secretNS, secretName))
		Expect(k8s.Delete(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: secretNS, Name: secretName},
		})).To(Succeed(), "delete snapshotter Secret %s/%s", secretNS, secretName)
		DeferCleanup(func() { m6RestoreSecret(secretBackup) })

		By("When a backup runs over the namespace")
		m1RunClusterBackup(refusedRun, m1LocationName, cbv1.NamespaceSelector{MatchNames: []string{m6PrecheckNS}})

		// THE ordering assertion, and the reason this case is worth a paid campaign. A pre-check
		// that ran AFTER Expose would reach the same red status and would still be a regression:
		// every refusal would leave a live VolumeSnapshot in the tenant namespace, which is
		// precisely what the leak-check forbids. So the window is watched from the instant the run
		// is created — the moment Expose would fire is inside it, not after it. Watching only
		// after the volume is already Failed would be too late: the same pass that fails a volume
		// also tears its exposure down, so a leaked snapshot could be gone before the first look.
		By("Then no VolumeSnapshot is EVER created for it — the pre-check runs strictly before Expose")
		Consistently(func(g Gomega) {
			g.Expect(m6ExposureSnapshots(g, m6PrecheckNS)).To(BeEmpty(),
				"a refused volume must never reach Expose")
		}, 90*time.Second, 2*time.Second).Should(Succeed())

		// One reconcile, not a deadline: the pre-check is an API read the controller performs on
		// the SAME pass that would otherwise have exposed the volume. The generous Eventually
		// budget covers the cascade reaching this namespace at all, not the decision itself.
		By("And the volume Fails with SnapshotPrecheckFailed — not Skipped, not a permanent wait")
		Eventually(func(g Gomega) {
			vol := m6PrecheckVolume(g, m6PrecheckNS, refusedRun, m6PrecheckPVC)
			g.Expect(string(vol.Phase)).To(Equal("Failed"),
				"a structurally unserveable snapshot must FAIL the volume (visible, alertable), never gate it")
			g.Expect(vol.Reason).To(Equal("SnapshotPrecheckFailed"))
		}, 5*time.Minute, 10*time.Second).Should(Succeed())

		By("And a Warning Event names the missing Secret, so the operator needs no second lookup")
		Eventually(func(g Gomega) {
			notes := m6BackupEventNotes(g, m6PrecheckNS, refusedRun, corev1.EventTypeWarning)
			g.Expect(notes).NotTo(BeEmpty(), "no Warning Event was recorded on the Backup at all")
			g.Expect(strings.Join(notes, "\n")).To(And(
				ContainSubstring(secretName),
				ContainSubstring(secretNS),
				ContainSubstring(m6PrecheckPVC),
			), "the Event must name the Secret (namespace and name) and the PVC")
		}, 2*time.Minute, 5*time.Second).Should(Succeed())

		By("And the run reports the failure rather than hanging")
		cb := m1WaitClusterBackupTerminal(refusedRun, 10*time.Minute)
		Expect(cb.Status.Phase).To(BeElementOf("Failed", "PartiallyFailed"))

		By("And nothing leaked")
		// refusedRun is this It's own run, and its residue predates this check by the several
		// minutes the refusal took — poll it out. Anything else here is another lane's.
		m1AssertNoResidualSnapshotObjects([]string{refusedRun}, m6PrecheckNS)
	})

	It("completes the very next run once the Secret is back", func() {
		By("Given the snapshotter Secret exists again")
		m6RestoreSecret(secretBackup)
		Eventually(func(g Gomega) {
			g.Expect(k8s.Get(ctx, client.ObjectKey{Namespace: secretNS, Name: secretName}, &corev1.Secret{})).To(Succeed())
		}, 2*time.Minute, 5*time.Second).Should(Succeed())

		By("When a backup runs over the same namespace")
		m1RunClusterBackup(restoredRun, m1LocationName, cbv1.NamespaceSelector{MatchNames: []string{m6PrecheckNS}})

		// The half of the contract that keeps the pre-check honest. A check that refuses is easy;
		// what makes it usable is that it stops refusing the moment the cluster is fixed, with no
		// operator intervention on the CrystalBackup side — no cached verdict, no cooldown, no
		// object to delete first.
		By("Then the volume completes — the refusal was a property of the cluster, not a sticky state")
		Eventually(func(g Gomega) {
			vol := m6PrecheckVolume(g, m6PrecheckNS, restoredRun, m6PrecheckPVC)
			g.Expect(string(vol.Phase)).To(Equal("Completed"),
				"reason=%q — the pre-check must stop refusing as soon as the Secret is back", vol.Reason)
			g.Expect(vol.SnapshotID).NotTo(BeEmpty(), "a completed volume must carry its restic snapshot id")
		}, 15*time.Minute, 10*time.Second).Should(Succeed())

		Expect(m1WaitClusterBackupTerminal(restoredRun, 15*time.Minute).Status.Phase).To(Equal("Completed"))

		By("And nothing leaked")
		// restoredRun only: refusedRun was certified residue-free by the It above, and this
		// container is Ordered, so that certification is a precondition of reaching this line.
		m1AssertNoResidualSnapshotObjects([]string{restoredRun}, m6PrecheckNS)
	})

	It("fails a volume whose snapshot request nobody picks up, instead of hanging to the suite timeout", func() {
		By("Given the external snapshot-controller is stopped — nothing will bind a VolumeSnapshotContent")
		m6ScaleDeployment(m6SnapshotControllerNS, snapControllerDeploy, 0)
		DeferCleanup(func() {
			m6ScaleDeployment(m6SnapshotControllerNS, snapControllerDeploy, snapControllerReplicas)
		})

		By("When a backup runs over the namespace (the pre-check passes — the Secret is present)")
		m1RunClusterBackup(stalledRun, m1LocationName, cbv1.NamespaceSelector{MatchNames: []string{m6PrecheckNS}})

		By("Then the volume is exposed and the origin VolumeSnapshot sits untouched")
		Eventually(func(g Gomega) {
			g.Expect(m6ExposureSnapshots(g, m6PrecheckNS)).NotTo(BeEmpty(),
				"the exposure must actually start — this case is about what happens AFTER admission")
		}, 5*time.Minute, 10*time.Second).Should(Succeed())

		// The wait is the point. 20 minutes covers the operator's 15-minute
		// snapshotProgressDeadline plus its 5-second poll and the cascade's own scheduling; the
		// spec's verdict is that the operator gives up ON ITS OWN, with a reason, rather than
		// leaving the run for the suite timeout to cut.
		By("Then, past the progress deadline, the volume Fails with SnapshotProgressDeadlineExceeded")
		Eventually(func(g Gomega) {
			vol := m6PrecheckVolume(g, m6PrecheckNS, stalledRun, m6PrecheckPVC)
			g.Expect(string(vol.Phase)).To(Equal("Failed"))
			g.Expect(vol.Reason).To(Equal("SnapshotProgressDeadlineExceeded"))
		}, 20*time.Minute, 15*time.Second).Should(Succeed())

		By("And the Event points at the snapshot machinery rather than restating the phase")
		Eventually(func(g Gomega) {
			notes := m6BackupEventNotes(g, m6PrecheckNS, stalledRun, corev1.EventTypeWarning)
			g.Expect(notes).NotTo(BeEmpty(), "no Warning Event was recorded on the Backup at all")
			g.Expect(strings.Join(notes, "\n")).To(And(
				ContainSubstring("snapshot-controller"),
				ContainSubstring("csi-snapshotter"),
				ContainSubstring(m6PrecheckPVC),
			), "the Event must name both components that could have picked the request up, and the PVC")
		}, 2*time.Minute, 5*time.Second).Should(Succeed())

		By("And the run reaches a terminal phase")
		cb := m1WaitClusterBackupTerminal(stalledRun, 10*time.Minute)
		Expect(cb.Status.Phase).To(BeElementOf("Failed", "PartiallyFailed"))

		By("Restoring the snapshot-controller so the teardown of this run's objects can drain")
		m6ScaleDeployment(m6SnapshotControllerNS, snapControllerDeploy, snapControllerReplicas)

		By("And nothing leaked — a failed-by-deadline volume is torn down like any other")
		// stalledRun's objects are older than this check by the full 15-minute progress deadline,
		// and their teardown only becomes possible when the snapshot-controller comes back one
		// line above — the same shape as m6/stall, and the same reason ownership decides here.
		m1AssertNoResidualSnapshotObjects([]string{stalledRun}, m6PrecheckNS)
	})
})

// ---------------------------------------------------------------------------
// Spec-local helpers (thin, file-scoped; the shared vocabulary lives in
// m1_helpers_test.go and crucible_suite_test.go and is reused, never redefined).
// ---------------------------------------------------------------------------

// m6SnapshotterSecretRef reads the snapshotter Secret reference off a VolumeSnapshotClass, the
// same two CSI-standard parameters internal/exposer's pre-check reads. Reading them rather than
// hard-coding rook's names is what keeps this spec pointed at the real subject when the storage
// layer is upgraded under it.
func m6SnapshotterSecretRef(className string) (namespace, name string) {
	GinkgoHelper()
	vsc := &unstructured.Unstructured{}
	vsc.SetGroupVersionKind(volumeSnapshotClassGVK())
	Expect(k8s.Get(ctx, client.ObjectKey{Name: className}, vsc)).To(Succeed(),
		"get VolumeSnapshotClass %s", className)

	params, _, err := unstructured.NestedStringMap(vsc.Object, "parameters")
	Expect(err).NotTo(HaveOccurred(), "read parameters of VolumeSnapshotClass %s", className)
	return params["csi.storage.k8s.io/snapshotter-secret-namespace"],
		params["csi.storage.k8s.io/snapshotter-secret-name"]
}

// m6CaptureSecret snapshots a Secret in a form that can be replayed with Create: server-assigned
// identity (resourceVersion, UID, managedFields, ownerReferences) stripped, payload kept.
func m6CaptureSecret(namespace, name string) *corev1.Secret {
	GinkgoHelper()
	var live corev1.Secret
	Expect(k8s.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &live)).To(Succeed(),
		"capture Secret %s/%s before deleting it", namespace, name)

	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:   live.Namespace,
			Name:        live.Name,
			Labels:      live.Labels,
			Annotations: live.Annotations,
		},
		Type:       live.Type,
		Data:       live.Data,
		StringData: nil,
	}
}

// m6RestoreSecret puts a captured Secret back, idempotently (an AlreadyExists means the cluster's
// owner beat us to it, which is the same end state). Never fails the spec: it runs from
// DeferCleanup and AfterAll, where masking the real failure with a cleanup error helps nobody.
func m6RestoreSecret(captured *corev1.Secret) {
	if captured == nil {
		return
	}
	restored := captured.DeepCopy()
	if err := k8s.Create(ctx, restored); err != nil && !apierrors.IsAlreadyExists(err) {
		AddReportEntry("restore snapshotter Secret failed",
			fmt.Sprintf("%s/%s: %v", captured.Namespace, captured.Name, err))
	}
}

// m6FindSnapshotController returns the name of the snapshot-controller Deployment in kube-system,
// matching the same way deploy/deploy.sh does. A cluster with none is a hard failure: the deadline
// case's whole premise is that this component can be stopped, and skipping it quietly would leave
// the deadline untested while the report still counted the spec.
func m6FindSnapshotController() string {
	GinkgoHelper()
	var deploys appsv1.DeploymentList
	Expect(k8s.List(ctx, &deploys, client.InNamespace(m6SnapshotControllerNS))).To(Succeed())

	var names []string
	for i := range deploys.Items {
		if strings.Contains(deploys.Items[i].Name, m6SnapshotControllerMatch) {
			names = append(names, deploys.Items[i].Name)
		}
	}
	Expect(names).NotTo(BeEmpty(),
		"no Deployment in %s matches %q — this cluster has no snapshot-controller to stop, and the "+
			"progress-deadline case cannot be made honest without one",
		m6SnapshotControllerNS, m6SnapshotControllerMatch)
	slices.Sort(names) // deterministic when a distro ships more than one match
	return names[0]
}

// m6DeploymentReplicas reads a Deployment's desired replica count. A Deployment this spec plans to
// stop and cannot even read is a hard failure, not something to default around: silently doing
// nothing would let the case that depends on the stop pass for the wrong reason.
func m6DeploymentReplicas(namespace, name string) int32 {
	GinkgoHelper()
	var deploy appsv1.Deployment
	Expect(k8s.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &deploy)).To(Succeed(),
		"get Deployment %s/%s — this spec cannot make its point without it", namespace, name)
	if deploy.Spec.Replicas == nil {
		return 1 // the API default when the field is unset
	}
	return *deploy.Spec.Replicas
}

// m6ScaleDeployment sets a Deployment's replicas, waiting out a scale-DOWN so the component is
// really gone before the spec depends on its absence.
func m6ScaleDeployment(namespace, name string, replicas int32) {
	GinkgoHelper()
	var deploy appsv1.Deployment
	Expect(k8s.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &deploy)).To(Succeed(),
		"get Deployment %s/%s — this spec cannot make its point without it", namespace, name)

	if deploy.Spec.Replicas != nil && *deploy.Spec.Replicas == replicas {
		return
	}
	deploy.Spec.Replicas = &replicas
	Expect(k8s.Update(ctx, &deploy)).To(Succeed(), "scale Deployment %s/%s to %d", namespace, name, replicas)

	// Scaling DOWN must be observed, not merely requested: the whole point is that the component
	// is gone before the backup runs, and a pod that is still terminating would still do its job.
	if replicas == 0 {
		Eventually(func(g Gomega) {
			var current appsv1.Deployment
			g.Expect(k8s.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &current)).To(Succeed())
			g.Expect(current.Status.Replicas).To(BeZero(),
				"Deployment %s/%s still has running pods", namespace, name)
		}, 3*time.Minute, 5*time.Second).Should(Succeed())
	}
}

// m6PrecheckVolume returns one PVC's VolumeStatus from the namespace's Backup, failing the
// enclosing Eventually (not the spec) while either is still absent.
func m6PrecheckVolume(g Gomega, namespace, run, pvc string) cbv1.VolumeStatus {
	var b cbv1.Backup
	g.Expect(k8s.Get(ctx, client.ObjectKey{Namespace: namespace, Name: run}, &b)).To(Succeed())
	for i := range b.Status.Volumes {
		if b.Status.Volumes[i].Pvc == pvc {
			return b.Status.Volumes[i]
		}
	}
	g.Expect(b.Status.Volumes).To(ContainElement(HaveField("Pvc", pvc)),
		"Backup %s/%s does not list volume %s yet", namespace, run, pvc)
	return cbv1.VolumeStatus{}
}

// m6BackupEventNotes returns the notes of every Event of the given type recorded against a Backup.
// It reads events.k8s.io/v1 — the API the operator's recorder actually writes — rather than the
// core/v1 compatibility view, so no field translation sits between the assertion and the string
// the controller produced.
func m6BackupEventNotes(g Gomega, namespace, run, eventType string) []string {
	var list eventsv1.EventList
	g.Expect(k8s.List(ctx, &list, client.InNamespace(namespace))).To(Succeed())

	var notes []string
	for i := range list.Items {
		e := &list.Items[i]
		if e.Type != eventType || e.Regarding.Kind != "Backup" || e.Regarding.Name != run {
			continue
		}
		notes = append(notes, e.Note)
	}
	return notes
}

// m6ExposureSnapshots lists the VolumeSnapshots in a namespace that carry the exposure label — the
// objects a backup creates, as opposed to anything a tenant or another spec made. It reuses the
// leak-check's own predicate so "no exposure snapshot exists" means exactly what the leak-check
// means by it, and the two can never drift into disagreeing about what residue is.
func m6ExposureSnapshots(g Gomega, namespace string) []string {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshotList",
	})
	g.Expect(k8s.List(ctx, list, client.InNamespace(namespace))).To(Succeed())

	var names []string
	for i := range list.Items {
		labels := list.Items[i].GetLabels()
		if m1IsExposureResidue(labels) && labels[apiconst.LabelPVC] == m6PrecheckPVC {
			names = append(names, list.Items[i].GetName())
		}
	}
	return names
}
