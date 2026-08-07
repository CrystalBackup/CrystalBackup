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
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
)

// ---------------------------------------------------------------------------
// The MOVER START DEADLINE, and the anti-regression that makes it safe to ship.
//
// The production incident: a nightly cascade created six data movers whose pods went
// ContainerCreating and STAYED THERE FOR THIRTY-SIX HOURS, because the temp clone PVC could not be
// mapped by the node's krbd. Nothing gave up — advanceUploading polled the Job and answered "still
// running; requeue" forever — nothing alerted, and concurrencyPolicy: Forbid meant no further
// nightly ran at all. Fifteen days, one backup.
//
// The two specs below are the two halves of the fix, and the SECOND is the one that has to be
// believed. A timeout that fails a stuck mover is easy; a timeout that fails a stuck mover and
// leaves a running one alone is the whole design. Without the anti-regression this change is a time
// bomb pointed at every large volume in the fleet.
//
// HOW THE CLOCK IS REACHED. The bound is measured from the mover Job's own creationTimestamp, which
// is durable and survives an operator restart — and which envtest cannot backdate: the apiserver
// resets metadata.creationTimestamp from the stored object on every update
// (rest.objectMetaSystemFieldsUpdate), so a patch is a silent no-op. The specs therefore shorten the
// BOUND instead, on a shallow copy of the suite's reconciler
// (BackupReconciler.MoverStartDeadline — see its doc for why the field exists and why it is not a
// user-facing knob). Everything else is the real thing: the real Backup controller drives the real
// exposure and creates the real mover Job, and the copy shares the same client, recorder and
// exposer registry as the registered reconciler.
// ---------------------------------------------------------------------------

// kubeletFailedMountMessage is the kubelet's own account of the incident, verbatim from the user's
// cluster. It is the string the reason has to end up carrying: an operator who reads
// `kubectl get backup` must learn what `kubectl describe pod` would have told them, because in the
// real incident nobody ever ran the second command.
const kubeletFailedMountMessage = `MountVolume.MountDevice failed for volume "pvc-9d1f" : ` +
	`rbd: map failed with error (exit status 22), rbd output: rbd: sysfs write failed / ` +
	`rbd: map failed: (22) Invalid argument`

// deadlineReconciler is a shallow copy of the registered Backup reconciler with a shortened
// mover-start bound. Shallow on purpose: same client, same event recorder (so the Warning Event
// lands in the apiserver where a spec can read it), same stub exposer registry, same queue.
func deadlineReconciler(bound time.Duration) *BackupReconciler {
	GinkgoHelper()
	Expect(backupReconciler).NotTo(BeNil(), "the suite's Backup reconciler was never wired")
	copied := *backupReconciler
	copied.MoverStartDeadline = bound
	return &copied
}

// createMoverPod creates the pod a real kubelet would have created for a mover Job, in whatever
// state the spec needs. envtest runs no kubelet and no Job controller, so a mover Job never grows a
// pod on its own — which is exactly why every pod shape in this file is written out by hand.
func createMoverPod(jobName, podName string, mutate func(*corev1.Pod)) *corev1.Pod {
	GinkgoHelper()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: suiteOperatorNamespace,
			Labels:    map[string]string{batchv1.JobNameLabel: jobName},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers:    []corev1.Container{{Name: "mover", Image: suiteMoverImage}},
		},
	}
	Expect(k8sClient.Create(ctx, pod)).To(Succeed())
	DeferCleanup(func() { _ = k8sClient.Delete(context.Background(), pod) })

	mutate(pod)
	Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
	return pod
}

// recordPodWarning writes the Warning Event a kubelet would have written about a pod it cannot
// start. It uses the LEGACY core/v1 shape (source + first/last timestamps + count) rather than the
// events.k8s.io one, because that is what a kubelet emits for FailedMount and because the aggregated
// count/lastTimestamp pair is the thing eventObservedAt has to read correctly: in the incident this
// single Event carried "x1069 over 36h".
func recordPodWarning(podName, reason, message string, count int32) {
	GinkgoHelper()
	first := metav1.NewTime(time.Now().Add(-36 * time.Hour))
	e := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName + ".warn",
			Namespace: suiteOperatorNamespace,
		},
		InvolvedObject: corev1.ObjectReference{
			Kind: "Pod", Namespace: suiteOperatorNamespace, Name: podName,
		},
		Type:           corev1.EventTypeWarning,
		Reason:         reason,
		Message:        message,
		Source:         corev1.EventSource{Component: "kubelet"},
		FirstTimestamp: first,
		LastTimestamp:  metav1.NewTime(time.Now()),
		Count:          count,
	}
	Expect(k8sClient.Create(ctx, e)).To(Succeed())
	DeferCleanup(func() { _ = k8sClient.Delete(context.Background(), e) })
}

// driveToUploading runs the real cascade until the volume's mover Job exists, and returns its name.
// From here the Backup sits in Uploading indefinitely — envtest has no kubelet, so nothing will ever
// finish that Job — which is precisely the state the deadline is about.
func driveToUploading(location, ns, run, pvcName, kek, s3 string) string {
	GinkgoHelper()
	seedInitializedRepo(location, kek, s3)
	createTenantNamespace(ns)
	createSourcePVC(ns, pvcName, "ceph-block")
	createVolumeOnlyParent(run, location, cbv1.PVCSelector{})
	createChildBackup(ns, run, location)

	jobName := waitForMoverJob(ns, run, pvcName)
	Eventually(func(g Gomega) {
		vol := volumeByPVC(getBackupG(g, ns, run), pvcName)
		g.Expect(vol).NotTo(BeNil())
		g.Expect(string(vol.Phase)).To(Equal("Uploading"))
	}, initTimeout, initPoll).Should(Succeed())
	return jobName
}

var _ = Describe("BackupReconciler per-phase progress deadline", func() {

	It("fails a volume whose mover pod never reached Running, carrying the kubelet's own words", func() {
		const (
			location = "bk-stuckpod"
			ns       = "bk-stuckpod-ns"
			run      = "bk-stuckpod-run"
			pvcName  = "data-vol"
		)
		jobName := driveToUploading(location, ns, run, pvcName, "kek-bk-stuckpod", "s3-bk-stuckpod")

		By("Given the mover pod is wedged in ContainerCreating, exactly as the incident's six were")
		createMoverPod(jobName, jobName+"-pod", func(pod *corev1.Pod) {
			startedLongAgo := metav1.NewTime(time.Now().Add(-36 * time.Hour))
			pod.Status.Phase = corev1.PodPending
			// startTime IS set — the kubelet accepted the pod thirty-six hours ago. It is the trap
			// moverPodEverStarted refuses to read, and it is set here so this spec would catch a
			// future edit that starts reading it.
			pod.Status.StartTime = &startedLongAgo
			pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
				Name: "mover",
				State: corev1.ContainerState{
					Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"},
				},
			}}
		})

		By("And the kubelet has been publishing the cause every minute for 36 hours")
		recordPodWarning(jobName+"-pod", "FailedMount", kubeletFailedMountMessage, 1069)

		By("When a reconcile runs past the mover-start deadline")
		r := deadlineReconciler(time.Millisecond)
		req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: run}}
		Eventually(func(g Gomega) {
			_, err := r.Reconcile(ctx, req)
			// A conflict is expected and uninteresting: the suite's registered reconciler is driving
			// the same object on its own 5-second poll. The retry is the assertion loop.
			if err != nil && !apierrors.IsConflict(err) {
				g.Expect(err).NotTo(HaveOccurred())
			}
			vol := volumeByPVC(getBackupG(g, ns, run), pvcName)
			g.Expect(vol).NotTo(BeNil())
			g.Expect(string(vol.Phase)).To(Equal("Failed"),
				"a mover pod that never started must not be waited on forever")
		}, initTimeout, initPoll).Should(Succeed())

		// THE assertion of the lot. A reason of "MoverStartDeadlineExceeded" and nothing else would
		// have left this user exactly as blind as thirty-six hours of silence did: it names OUR
		// decision, not THEIR fault. The kubelet published the diagnosis 1069 times; the reason has
		// to be where an operator finally meets it.
		By("Then the reason names the deadline AND quotes the kubelet's diagnosis")
		vol := volumeByPVC(getBackupG(Default, ns, run), pvcName)
		Expect(vol.Reason).To(HavePrefix(backupReasonMoverStartDeadline+":"),
			"the reason must name which deadline fired")
		Expect(vol.Reason).To(ContainSubstring("FailedMount"),
			"the kubelet's Event reason is half of what `kubectl describe pod` shows")
		Expect(vol.Reason).To(ContainSubstring("rbd: map failed"),
			"the kubelet's own message is the only place the CAUSE exists — a reason without it "+
				"converts a silent hang into a silent failure")
		// Bounded, because a status field is not a log: the Event carries the full text.
		Expect(len(vol.Reason)).To(BeNumerically("<=", 200))

		By("And a Warning Event on the Backup carries the long form, pointing at the pod to describe")
		Eventually(func(g Gomega) {
			g.Expect(backupWarningNotes(g, ns, run)).To(ContainElement(And(
				ContainSubstring(pvcName),
				ContainSubstring("no pod of it ever reached Running"),
				ContainSubstring("rbd: map failed"),
			)))
		}, initTimeout, initPoll).Should(Succeed())

		// A volume failed by a deadline is a terminal volume like any other, and terminal volumes
		// have their exposure, clone PVC, mover Job and creds Secret removed. The crucible's
		// leak-check enforces this on real storage; here the observable proxies are the stub
		// registry's recorded teardown and the objects actually disappearing.
		By("And nothing leaks: the exposure, the clone PVC, the Job and its Secret all go")
		Eventually(func(g Gomega) {
			g.Expect(backupExposers.teardownCalls()).To(ContainElement(ns + "/" + moverNamePrefix(ns, run, pvcName)))

			err := k8sClient.Get(ctx, client.ObjectKey{Namespace: suiteOperatorNamespace, Name: jobName}, &batchv1.Job{})
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "mover Job survived a failed-by-deadline volume: %v", err)

			err = k8sClient.Get(ctx, client.ObjectKey{
				Namespace: suiteOperatorNamespace, Name: tempCloneNameFor(ns, run, pvcName),
			}, &corev1.PersistentVolumeClaim{})
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "temp clone PVC survived: %v", err)

			err = k8sClient.Get(ctx, client.ObjectKey{Namespace: suiteOperatorNamespace, Name: jobName}, &corev1.Secret{})
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "mover creds Secret survived: %v", err)
		}, initTimeout, initPoll).Should(Succeed())
	})

	// THE ANTI-REGRESSION. Read the previous spec and this one together or neither means anything:
	// the first proves the operator gives up on a mover that never started, and this one proves it
	// does NOT give up on one that did — with the deadline set to a millisecond and the pod running
	// for well past it, so the ONLY thing standing between a legitimate upload and destruction is
	// moverPodEverStarted.
	//
	// This is the case that costs data if it regresses. A 500 GB volume legitimately uploads for
	// hours; a wall-clock cap dressed up as a stall detector would fail exactly the backups that
	// matter most, on exactly the nights they matter most, and it would look like a working feature
	// while doing it.
	It("leaves a mover whose pod IS Running alone, however far past the deadline it is", func() {
		const (
			location = "bk-slowpod"
			ns       = "bk-slowpod-ns"
			run      = "bk-slowpod-run"
			pvcName  = "data-vol"
		)
		jobName := driveToUploading(location, ns, run, pvcName, "kek-bk-slowpod", "s3-bk-slowpod")

		By("Given the mover pod is Running — restic is reading a large volume and has been for hours")
		createMoverPod(jobName, jobName+"-pod", func(pod *corev1.Pod) {
			pod.Status.Phase = corev1.PodRunning
			pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
				Name: "mover",
				State: corev1.ContainerState{
					Running: &corev1.ContainerStateRunning{
						StartedAt: metav1.NewTime(time.Now().Add(-6 * time.Hour)),
					},
				},
			}}
		})

		// A Warning on the pod too — a transient FailedScheduling from before it landed, or a
		// probe warning. Its mere presence must not be read as evidence of a stall: the deadline
		// keys off the pod having RUN, never off there being something to complain about.
		By("And there is a Warning Event about it, which must not be mistaken for a stall")
		recordPodWarning(jobName+"-pod", "Unhealthy", "readiness probe failed", 3)

		By("When reconciles run repeatedly with the deadline set to a millisecond")
		r := deadlineReconciler(time.Millisecond)
		req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: run}}
		Consistently(func(g Gomega) {
			_, err := r.Reconcile(ctx, req)
			if err != nil && !apierrors.IsConflict(err) {
				g.Expect(err).NotTo(HaveOccurred())
			}
			vol := volumeByPVC(getBackupG(g, ns, run), pvcName)
			g.Expect(vol).NotTo(BeNil())
			g.Expect(string(vol.Phase)).To(Equal("Uploading"),
				"reason=%q — a RUNNING mover was failed by the start deadline. This is the "+
					"regression that turns a stall detector into a wall-clock cap on backups, and it "+
					"destroys the largest volumes in the fleet first.", vol.Reason)
			g.Expect(vol.Reason).To(BeEmpty())
		}, 3*time.Second, 200*time.Millisecond).Should(Succeed())

		By("And its mover Job is still there, still running")
		Expect(k8sClient.Get(ctx,
			client.ObjectKey{Namespace: suiteOperatorNamespace, Name: jobName}, &batchv1.Job{})).To(Succeed())
	})

	// The second half of the Snapshotting bound: the origin VolumeSnapshot WAS acknowledged — a
	// VolumeSnapshotContent is bound — and readyToUse never arrived. internal/exposer's
	// SnapshotProgress documents this gap and declines to close it, because closing it means
	// choosing a maximum legitimate snapshot duration on storage we do not own; the choice is made
	// in the controller (snapshotReadyDeadline) and this is where it is exercised.
	It("fails a volume whose snapshot was picked up and never became ready", func() {
		const (
			location = "bk-notready"
			ns       = "bk-notready-ns"
			run      = "bk-notready-run"
			pvcName  = "data-vol"
		)
		// Acknowledged, and started well past the two-hour bound. The distinction from the
		// unacknowledged case is the whole point: that one fails at fifteen minutes with a different
		// reason, and a spec that armed the wrong flag would silently be testing it instead.
		backupExposers.stallSnapshotAcknowledged(ns+"/"+pvcName, time.Now().Add(-2*snapshotReadyDeadline))

		seedInitializedRepo(location, "kek-bk-notready", "s3-bk-notready")
		createTenantNamespace(ns)
		createSourcePVC(ns, pvcName, "ceph-block")
		createVolumeOnlyParent(run, location, cbv1.PVCSelector{})
		createChildBackup(ns, run, location)

		By("Then the volume Fails with SnapshotReadyDeadlineExceeded, not the unattended reason")
		Eventually(func(g Gomega) {
			vol := volumeByPVC(getBackupG(g, ns, run), pvcName)
			g.Expect(vol).NotTo(BeNil())
			g.Expect(string(vol.Phase)).To(Equal("Failed"))
			g.Expect(vol.Reason).To(HavePrefix(backupReasonSnapshotReadyDeadline),
				"an ACKNOWLEDGED snapshot that never became ready is a different diagnosis from one "+
					"nobody picked up, and it sends the operator to a different component")
		}, initTimeout, initPoll).Should(Succeed())

		By("And the Event names the sidecar rather than restating the phase")
		Eventually(func(g Gomega) {
			g.Expect(backupWarningNotes(g, ns, run)).To(ContainElement(And(
				ContainSubstring(pvcName),
				ContainSubstring("never became readyToUse"),
				ContainSubstring("csi-snapshotter"),
			)))
		}, initTimeout, initPoll).Should(Succeed())

		By("And its exposure is torn down, exactly as for any other terminal volume")
		Eventually(func(g Gomega) {
			g.Expect(backupExposers.teardownCalls()).To(ContainElement(ns + "/" + moverNamePrefix(ns, run, pvcName)))
		}, initTimeout, initPoll).Should(Succeed())
	})
})

// backupWarningNotes returns the notes of every Warning Event recorded ABOUT a Backup. It reads
// events.k8s.io/v1 because that is the API controller-runtime's recorder writes through, and it
// filters by regarding rather than by namespace alone so a spec cannot accidentally assert on a
// neighbour's event.
func backupWarningNotes(g Gomega, namespace, name string) []string {
	var list eventsv1.EventList
	g.Expect(k8sClient.List(ctx, &list, client.InNamespace(namespace))).To(Succeed())
	var notes []string
	for i := range list.Items {
		e := &list.Items[i]
		if e.Type != corev1.EventTypeWarning || e.Regarding.Kind != kindBackup || e.Regarding.Name != name {
			continue
		}
		notes = append(notes, strings.TrimSpace(e.Note))
	}
	return notes
}
