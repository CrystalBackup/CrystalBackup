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
	"fmt"

	. "github.com/onsi/ginkgo/v2" //nolint:revive,staticcheck
	. "github.com/onsi/gomega"    //nolint:revive,staticcheck

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
	"github.com/CrystalBackup/CrystalBackup/internal/exposer"
	"github.com/CrystalBackup/CrystalBackup/internal/mover"
)

// ---------------------------------------------------------------------------
// The exposer seam stub. envtest has no external snapshot CRDs and no CSI
// driver, so the Backup reconciler is wired with this stub instead of a real
// *exposer.Registry. It keeps the flow realistic where it matters — Expose
// creates an ACTUAL temp clone PVC in the operator namespace (the object the
// mover Job mounts), Cleanup deletes it — and short-circuits everything that
// would need a real snapshotter (Ready is instantly true). A PVC whose storage
// class is the magic stubUnsupportedStorageClass resolves to exposer.ErrUnsupported,
// letting a spec exercise the Skipped path deterministically (mirroring how the
// ClusterBackupLocation suite keys stubS3Prober off a magic endpoint value rather
// than a shared mutable flag).
// ---------------------------------------------------------------------------

// stubUnsupportedStorageClass is the magic storageClassName the stub registry maps to
// exposer.ErrUnsupported, standing in for a class (e.g. rancher.io/local-path) with no
// VolumeSnapshotClass.
const stubUnsupportedStorageClass = "no-snapshots.test"

// stubKind is the Exposure.Kind the stub reports; it is never re-resolved by the controller.
const stubKind = "stub"

type stubExposerRegistry struct {
	client            client.Client
	operatorNamespace string
}

var _ ExposerRegistry = (*stubExposerRegistry)(nil)

func (s *stubExposerRegistry) For(_ context.Context, pvc *corev1.PersistentVolumeClaim) (exposer.SnapshotExposer, error) {
	if pvc.Spec.StorageClassName != nil && *pvc.Spec.StorageClassName == stubUnsupportedStorageClass {
		return nil, fmt.Errorf("stub: storage class %q has no snapshot support: %w",
			stubUnsupportedStorageClass, exposer.ErrUnsupported)
	}
	return &stubExposer{client: s.client, operatorNamespace: s.operatorNamespace}, nil
}

type stubExposer struct {
	client            client.Client
	operatorNamespace string
}

var _ exposer.SnapshotExposer = (*stubExposer)(nil)

func (e *stubExposer) Kind() string { return stubKind }

// Expose creates the temp clone PVC (deterministic name <prefix>-clone, mirroring the real
// exposer) in the operator namespace so the mover Job has a real PVC to mount, then returns the
// deterministic Exposure. Idempotent: the Create tolerates AlreadyExists.
func (e *stubExposer) Expose(ctx context.Context, req exposer.ExposeRequest) (*exposer.Exposure, error) {
	tempName := req.NamePrefix + "-clone"
	capacity := req.Capacity
	if capacity.IsZero() {
		capacity = resource.MustParse("1Gi")
	}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: tempName, Namespace: e.operatorNamespace, Labels: req.Labels},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: capacity},
			},
		},
	}
	if err := e.client.Create(ctx, pvc); err != nil && !apierrors.IsAlreadyExists(err) {
		return nil, err
	}
	return &exposer.Exposure{
		Kind:              stubKind,
		OriginNamespace:   req.Namespace,
		OperatorNamespace: e.operatorNamespace,
		TempPVCName:       tempName,
		ExposedPVCName:    tempName,
		StorageClass:      req.StorageClass,
		Capacity:          req.Capacity,
		Labels:            req.Labels,
	}, nil
}

func (e *stubExposer) Ready(_ context.Context, _ *exposer.Exposure) (bool, error) { return true, nil }

// Cleanup deletes the temp clone PVC. envtest enables the StorageObjectInUseProtection admission
// plugin (which stamps the kubernetes.io/pvc-protection finalizer on every PVC at creation) but
// runs NO pvc-protection controller to clear it, so a plain Delete would leave the clone stuck
// Terminating. We therefore Delete it and then strip its finalizers with a name-scoped merge
// PATCH — standing in for the controller a real cluster runs once the mover pod that mounted the
// clone is gone. Both are DIRECT writes with no optimistic lock and no cache read, so this never
// conflicts with the (cached) client's lag; the finalizer strip lands on the terminating object
// and the clone actually disappears, letting the "no residual clone PVC" leak-check pass.
func (e *stubExposer) Cleanup(ctx context.Context, ex *exposer.Exposure) error {
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: ex.TempPVCName, Namespace: ex.OperatorNamespace},
	}
	if err := e.client.Delete(ctx, pvc); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	patch := client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"finalizers":null}}`))
	if err := e.client.Patch(ctx, pvc, patch); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// ---------------------------------------------------------------------------
// Backup-suite helpers.
// ---------------------------------------------------------------------------

// seedInitializedRepo drives the REAL ClusterBackupLocation + BackupRepository controllers to an
// initialized repository (simulating the init Job's success exactly as the BackupRepository specs
// do), so the Backup reconciler resolves a genuine location, repository, DEK and repo URL. It
// reuses the BackupRepository suite's helpers (createKEKSecret, createTestLocation, …).
func seedInitializedRepo(location, kekSecret, s3Secret string) {
	GinkgoHelper()
	createKEKSecret(kekSecret, generateAgeIdentity())
	createS3CredsSecret(s3Secret)
	registerRepoCleanup(location)
	createTestLocation(newTestLocation(location, kekSecret, s3Secret, false))

	waitForInitJobCreated(location)
	patchInitJobStatus(location, func(j *batchv1.Job) { j.Status.Succeeded = 1 })
	Eventually(func(g Gomega) {
		g.Expect(getRepositoryG(g, location).Status.Initialized).To(BeTrue())
	}, initTimeout, initPoll).Should(Succeed())
}

// createParentClusterBackup creates the (cluster-scoped) ClusterBackup the child Backups link to.
// No ClusterBackup controller is registered in the suite — the Backup controller only READS this
// object for its run config (locationRef, pvcSelector) — so it simply sits there.
func createParentClusterBackup(name, location string, sel cbv1.PVCSelector) {
	GinkgoHelper()
	cb := &cbv1.ClusterBackup{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: cbv1.ClusterBackupSpec{
			ClusterBackupRunSpec: cbv1.ClusterBackupRunSpec{
				LocationRef: cbv1.LocalObjectReference{Name: location},
				PVCSelector: sel,
			},
		},
	}
	Expect(k8sClient.Create(ctx, cb)).To(Succeed())
	DeferCleanup(func() { _ = k8sClient.Delete(context.Background(), cb) })
}

// createVolumeOnlyParent is createParentClusterBackup with the manifest half switched OFF.
//
// includeManifests defaults to true, so every run now has two halves and a Backup only reaches
// a terminal phase when BOTH settle. A test that is about volume roll-up and never simulates a
// manifest mover would otherwise sit at Uploading forever — failing on a timeout that says
// nothing about what it meant to check. Opting out keeps those assertions about the thing they
// name; the interaction between the two halves has its own test in manifests_phase_test.go.
func createVolumeOnlyParent(name, location string, sel cbv1.PVCSelector) {
	GinkgoHelper()
	off := false
	cb := &cbv1.ClusterBackup{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: cbv1.ClusterBackupSpec{
			ClusterBackupRunSpec: cbv1.ClusterBackupRunSpec{
				LocationRef:      cbv1.LocalObjectReference{Name: location},
				PVCSelector:      sel,
				IncludeManifests: &off,
			},
		},
	}
	Expect(k8sClient.Create(ctx, cb)).To(Succeed())
	DeferCleanup(func() { _ = k8sClient.Delete(context.Background(), cb) })
}

// createChildBackup creates a cluster-origin Backup in namespace, named after the run (== the
// parent ClusterBackup name, per apiconst.LabelClusterBackup's contract) and linked by label.
func createChildBackup(namespace, name, location string) *cbv1.Backup {
	GinkgoHelper()
	b := &cbv1.Backup{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      name,
			Labels: map[string]string{
				apiconst.LabelClusterBackup: name,
				apiconst.LabelOrigin:        apiconst.OriginCluster,
			},
		},
		Spec: cbv1.BackupSpec{
			LocationRef: cbv1.LocationReference{Kind: "ClusterBackupLocation", Name: location},
		},
	}
	Expect(k8sClient.Create(ctx, b)).To(Succeed())
	DeferCleanup(func() { _ = k8sClient.Delete(context.Background(), b) })
	return b
}

// createTenantNamespace creates a tenant namespace (best-effort deleted after the spec; envtest
// has no namespace controller, so it lingers Terminating — harmless, names are unique per spec).
func createTenantNamespace(name string) {
	GinkgoHelper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	Expect(k8sClient.Create(ctx, ns)).To(Succeed())
	DeferCleanup(func() { _ = k8sClient.Delete(context.Background(), ns) })
}

// createSourcePVC creates a 1Gi source PVC in a tenant namespace, optionally on storageClass.
func createSourcePVC(namespace, name, storageClass string) {
	GinkgoHelper()
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
			},
		},
	}
	if storageClass != "" {
		sc := storageClass
		pvc.Spec.StorageClassName = &sc
	}
	Expect(k8sClient.Create(ctx, pvc)).To(Succeed())
	DeferCleanup(func() { _ = k8sClient.Delete(context.Background(), pvc) })
}

// moverJobNameFor is the deterministic mover Job/creds-Secret name for a (namespace, backup, pvc).
// It reaches into the package-private moverNamePrefix (the test is in package controller) so the
// spec and the controller can never disagree on the name — including the namespace qualifier that
// keeps same-named PVCs in different namespaces of one run from colliding.
func moverJobNameFor(namespace, backupName, pvcName string) string {
	return moverNamePrefix(namespace, backupName, pvcName) + "-mover"
}

// tempCloneNameFor is the deterministic temp clone PVC name (operator ns) the stub Expose creates.
func tempCloneNameFor(namespace, backupName, pvcName string) string {
	return moverNamePrefix(namespace, backupName, pvcName) + "-clone"
}

// waitForMoverJob waits for the reconciler to have created the mover Job for (namespace, backup, pvc).
func waitForMoverJob(namespace, backupName, pvcName string) string {
	GinkgoHelper()
	name := moverJobNameFor(namespace, backupName, pvcName)
	Eventually(func(g Gomega) {
		g.Expect(k8sClient.Get(ctx,
			client.ObjectKey{Namespace: suiteOperatorNamespace, Name: name}, &batchv1.Job{})).To(Succeed())
	}, initTimeout, initPoll).Should(Succeed())
	return name
}

// simulateMoverSucceeded fakes a finished mover Job: a synthetic pod carrying the batch job-name
// label whose terminated container reports result via its termination message, then the Job's
// status.succeeded=1. envtest has no kubelet, so this is the only way a mover Job reaches a
// terminal state (mirroring the BackupRepository suite's init-Job simulation).
func simulateMoverSucceeded(jobName, node string, result mover.MoverResult) {
	GinkgoHelper()
	msg, err := result.Encode()
	Expect(err).NotTo(HaveOccurred())

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName + "-pod",
			Namespace: suiteOperatorNamespace,
			Labels:    map[string]string{batchv1.JobNameLabel: jobName},
		},
		Spec: corev1.PodSpec{
			NodeName:      node,
			RestartPolicy: corev1.RestartPolicyNever,
			Containers:    []corev1.Container{{Name: "mover", Image: suiteMoverImage}},
		},
	}
	Expect(k8sClient.Create(ctx, pod)).To(Succeed())
	DeferCleanup(func() { _ = k8sClient.Delete(context.Background(), pod) })

	pod.Status.Phase = corev1.PodSucceeded
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:  "mover",
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, Message: msg}},
	}}
	Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())

	Eventually(func(g Gomega) {
		var job batchv1.Job
		g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: suiteOperatorNamespace, Name: jobName}, &job)).To(Succeed())
		job.Status.Succeeded = 1
		g.Expect(k8sClient.Status().Update(ctx, &job)).To(Succeed())
	}, initTimeout, initPoll).Should(Succeed())
}

// getBackupG fetches a Backup inside an Eventually block (retry-on-missing, not hard-fail).
func getBackupG(g Gomega, namespace, name string) cbv1.Backup {
	var b cbv1.Backup
	g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &b)).To(Succeed())
	return b
}

// volumeByPVC returns the VolumeStatus for pvc, or nil.
func volumeByPVC(b cbv1.Backup, pvc string) *cbv1.VolumeStatus {
	for i := range b.Status.Volumes {
		if b.Status.Volumes[i].Pvc == pvc {
			return &b.Status.Volumes[i]
		}
	}
	return nil
}

var _ = Describe("BackupReconciler", func() {

	It("backs up a PVC end to end: exposes it, runs a mover Job, records the snapshot, and Completes", func() {
		const (
			location = "bk-happy"
			ns       = "bk-happy-ns"
			run      = "bk-happy-run"
			pvcName  = "data-vol"
		)
		seedInitializedRepo(location, "kek-bk-happy", "s3-bk-happy")
		createTenantNamespace(ns)
		createSourcePVC(ns, pvcName, "ceph-block")
		createVolumeOnlyParent(run, location, cbv1.PVCSelector{})
		createChildBackup(ns, run, location)

		By("the reconciler exposes the PVC and creates a mover Job that mounts the temp clone")
		jobName := waitForMoverJob(ns, run, pvcName)
		var job batchv1.Job
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: suiteOperatorNamespace, Name: jobName}, &job)).To(Succeed())
		// The mover Job mounts the temp clone PVC the (stub) exposer created, and carries the
		// exposure labels the reaper/leak-check select on — never an ownerReference to the Backup.
		Expect(job.Labels).To(HaveKeyWithValue(apiconst.LabelManagedBy, apiconst.ManagedByValue))
		Expect(job.Labels).To(HaveKeyWithValue(apiconst.LabelClusterBackup, run))
		Expect(job.Labels).To(HaveKeyWithValue(apiconst.LabelNamespace, ns))
		Expect(job.Labels).To(HaveKeyWithValue(apiconst.LabelPVC, pvcName))
		Expect(job.OwnerReferences).To(BeEmpty(), "a mover Job must not be owned across namespaces")
		var clone corev1.PersistentVolumeClaim
		Expect(k8sClient.Get(ctx,
			client.ObjectKey{Namespace: suiteOperatorNamespace, Name: tempCloneNameFor(ns, run, pvcName)}, &clone)).To(Succeed())

		By("simulating the mover Job succeeding with a snapshot result")
		simulateMoverSucceeded(jobName, "node-a", mover.MoverResult{
			OK: true, Operation: string(mover.OpBackup), SnapshotID: "snap-abc123", SizeBytes: 4096, AddedBytes: 1024,
		})

		By("the volume records the snapshot identity and the Backup rolls up to Completed")
		Eventually(func(g Gomega) {
			b := getBackupG(g, ns, run)
			g.Expect(b.Status.Phase).To(Equal("Completed"))
			g.Expect(b.Status.BackupTime).NotTo(BeNil())
			vol := volumeByPVC(b, pvcName)
			g.Expect(vol).NotTo(BeNil())
			g.Expect(string(vol.Phase)).To(Equal("Completed"))
			g.Expect(vol.SnapshotID).To(Equal("snap-abc123"))
			g.Expect(vol.SizeBytes).To(Equal(int64(4096)))
			g.Expect(vol.AddedBytes).To(Equal(int64(1024)))
			g.Expect(vol.Node).To(Equal("node-a"))
			g.Expect(apimeta.IsStatusConditionTrue(b.Status.Conditions, ConditionReady)).To(BeTrue())
		}, initTimeout, initPoll).Should(Succeed())

		By("the exposure and mover Job are torn down on completion (no leak)")
		Eventually(func(g Gomega) {
			err := k8sClient.Get(ctx, client.ObjectKey{Namespace: suiteOperatorNamespace, Name: jobName}, &batchv1.Job{})
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
			err = k8sClient.Get(ctx,
				client.ObjectKey{Namespace: suiteOperatorNamespace, Name: tempCloneNameFor(ns, run, pvcName)}, &corev1.PersistentVolumeClaim{})
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
		}, initTimeout, initPoll).Should(Succeed())
	})

	It("marks a volume on unsupported storage Skipped without degrading the Backup below Completed", func() {
		const (
			location = "bk-skip"
			ns       = "bk-skip-ns"
			run      = "bk-skip-run"
			skipPVC  = "vol-a-skip" // sorted first: Skipped immediately, no mover
			okPVC    = "vol-b-ok"   // sorted second: backed up normally
		)
		seedInitializedRepo(location, "kek-bk-skip", "s3-bk-skip")
		createTenantNamespace(ns)
		createSourcePVC(ns, skipPVC, stubUnsupportedStorageClass)
		createSourcePVC(ns, okPVC, "ceph-block")
		createVolumeOnlyParent(run, location, cbv1.PVCSelector{})
		createChildBackup(ns, run, location)

		By("the unsupported volume becomes Skipped/CSISnapshotUnsupported without a mover Job")
		Eventually(func(g Gomega) {
			vol := volumeByPVC(getBackupG(g, ns, run), skipPVC)
			g.Expect(vol).NotTo(BeNil())
			g.Expect(string(vol.Phase)).To(Equal("Skipped"))
			g.Expect(vol.Reason).To(Equal("CSISnapshotUnsupported"))
		}, initTimeout, initPoll).Should(Succeed())

		By("the snapshottable volume runs a mover Job that we simulate to success")
		jobName := waitForMoverJob(ns, run, okPVC)
		simulateMoverSucceeded(jobName, "node-b", mover.MoverResult{
			OK: true, Operation: string(mover.OpBackup), SnapshotID: "snap-ok", SizeBytes: 10, AddedBytes: 5,
		})

		By("one Skipped + one Completed rolls up to Completed (Skipped is neutral), never PartiallyCompleted or Failed")
		Eventually(func(g Gomega) {
			b := getBackupG(g, ns, run)
			g.Expect(b.Status.Phase).To(Equal("Completed"))
			g.Expect(string(volumeByPVC(b, okPVC).Phase)).To(Equal("Completed"))
			g.Expect(string(volumeByPVC(b, skipPVC).Phase)).To(Equal("Skipped"))
			g.Expect(apimeta.IsStatusConditionTrue(b.Status.Conditions, ConditionReady)).To(BeTrue())
		}, initTimeout, initPoll).Should(Succeed())
	})

	It("completes a namespace with no matching PVCs cleanly, with zero volumes", func() {
		const (
			location = "bk-empty"
			ns       = "bk-empty-ns"
			run      = "bk-empty-run"
		)
		seedInitializedRepo(location, "kek-bk-empty", "s3-bk-empty")
		createTenantNamespace(ns)
		createVolumeOnlyParent(run, location, cbv1.PVCSelector{})
		createChildBackup(ns, run, location)

		By("the Backup reaches Completed with an empty volume set")
		Eventually(func(g Gomega) {
			b := getBackupG(g, ns, run)
			g.Expect(b.Status.Phase).To(Equal("Completed"))
			g.Expect(b.Status.Volumes).To(BeEmpty())
			g.Expect(b.Status.BackupTime).NotTo(BeNil())
		}, initTimeout, initPoll).Should(Succeed())
	})

	It("gates on the repository not being initialized, staying non-terminal", func() {
		const (
			location = "bk-gate"
			ns       = "bk-gate-ns"
			run      = "bk-gate-run"
			pvcName  = "gate-vol"
		)
		// Seed the location + secrets but DO NOT simulate the init Job — the repository never
		// reaches Initialized=true, so the Backup must gate on it.
		createKEKSecret("kek-bk-gate", generateAgeIdentity())
		createS3CredsSecret("s3-bk-gate")
		registerRepoCleanup(location)
		createTestLocation(newTestLocation(location, "kek-bk-gate", "s3-bk-gate", false))
		createTenantNamespace(ns)
		createSourcePVC(ns, pvcName, "ceph-block")
		createParentClusterBackup(run, location, cbv1.PVCSelector{})
		createChildBackup(ns, run, location)

		By("the Backup reports Ready=False reason RepositoryNotReady and never terminates")
		Eventually(func(g Gomega) {
			b := getBackupG(g, ns, run)
			cond := apimeta.FindStatusCondition(b.Status.Conditions, ConditionReady)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(cond.Reason).To(Equal("RepositoryNotReady"))
		}, initTimeout, initPoll).Should(Succeed())

		Consistently(func(g Gomega) {
			g.Expect(isTerminalBackupPhase(getBackupG(g, ns, run).Status.Phase)).To(BeFalse())
			// It must not have created any mover Job while gated.
			err := k8sClient.Get(ctx,
				client.ObjectKey{Namespace: suiteOperatorNamespace, Name: moverJobNameFor(ns, run, pvcName)}, &batchv1.Job{})
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
		}, "3s", "500ms").Should(Succeed())
	})

	It("finalizes on delete: tears down the live exposure and mover Job, then removes the object", func() {
		const (
			location = "bk-del"
			ns       = "bk-del-ns"
			run      = "bk-del-run"
			pvcName  = "del-vol"
		)
		seedInitializedRepo(location, "kek-bk-del", "s3-bk-del")
		createTenantNamespace(ns)
		createSourcePVC(ns, pvcName, "ceph-block")
		createParentClusterBackup(run, location, cbv1.PVCSelector{})
		backup := createChildBackup(ns, run, location)

		By("driving the volume to a live exposure + mover Job (Uploading), NOT simulating success")
		jobName := waitForMoverJob(ns, run, pvcName)
		cloneName := tempCloneNameFor(ns, run, pvcName)
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx,
				client.ObjectKey{Namespace: suiteOperatorNamespace, Name: cloneName}, &corev1.PersistentVolumeClaim{})).To(Succeed())
		}, initTimeout, initPoll).Should(Succeed())

		By("deleting the Backup")
		Expect(k8sClient.Delete(ctx, backup)).To(Succeed())

		By("the finalizer tears the exposure + mover Job down and the object is fully removed")
		Eventually(func(g Gomega) {
			err := k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: run}, &cbv1.Backup{})
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
			err = k8sClient.Get(ctx, client.ObjectKey{Namespace: suiteOperatorNamespace, Name: jobName}, &batchv1.Job{})
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
			err = k8sClient.Get(ctx, client.ObjectKey{Namespace: suiteOperatorNamespace, Name: cloneName}, &corev1.PersistentVolumeClaim{})
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
		}, initTimeout, initPoll).Should(Succeed())
	})
})
