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
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
	"github.com/CrystalBackup/CrystalBackup/internal/client/secrets"
	"github.com/CrystalBackup/CrystalBackup/internal/mover"
	"github.com/CrystalBackup/CrystalBackup/internal/restic"
	"github.com/CrystalBackup/CrystalBackup/internal/rexposer"
	"github.com/CrystalBackup/CrystalBackup/internal/status"
)

// ---------------------------------------------------------------------------
// THE ERRORED-PASS CLASS, SWEPT ON THE NAMESPACED RESTORE.
//
// A restore is the operation people run on their worst day, so a restore that loses a pass's progress
// is worse than a backup that does — the person reading the object is mid-disaster and deciding
// whether to intervene. Three instances of the class lived in this Reconcile, and the two that matter
// are here (the third — a hard apiserver error inside prepare discarding only the Confirmed condition
// — is documented at its site as deliberately left).
//
//  1. STEP (3b), advanceResources. Its error was returned one line below the call, which is UPSTREAM
//     of the volume drive as well as of the status write. So a manifest Job that could not be created
//     held every volume of the restore back, in flat contradiction of (3b)'s own doc ("driven
//     independently of the volumes — coupling them would lose one to the other's bad day").
//  2. STEP (4), the volume drive's error. driveVolumes deliberately drives EVERY volume and only
//     REMEMBERS the first transient per-volume error, so that "one flaky volume never stalls its
//     siblings" — and then the caller returned that error, upstream of the single status write, and
//     discarded everything the pass had just computed about all of them. The counters 0.6.5 added so
//     an operator could tell a working restore from a wedged one were the first casualty. The second
//     is worse and is what the second test below is about: on the pass where the MANIFEST half's
//     result was read out of the mover pod's termination message, that report was discarded too — and
//     readMoverResult reads the POD (this half has no annotation fallback), so once the pod is gone
//     the re-derived verdict is "did not report a result … some resources may have been applied",
//     failedCount 1. A clean apply of a whole namespace, re-read as a possible failure, because of
//     something that had nothing to do with it.
//
// The shapes chosen differ, and the difference is the point: (3b) is a WHOLE-OBJECT failure (one Job
// for the restore) so it persists first and then propagates — the backoff belongs to the object at
// fault. (4) is PER-VOLUME, so it is recorded and the pass returns nil, exactly as the Backup
// controller's step (10) argues: propagating would charge the backoff to the restore and stretch the
// poll driving every OTHER volume of it.
//
// The third test covers the case those two shapes do not: a drive that could not read the mover
// census at all, so no volume has a verdict. There the tally is EMPTY, and publishing it would
// overwrite a real 4-of-6 with plannedVolumes 0 — not a missing answer but a wrong one, in front of
// somebody mid-restore. That pass persists everything else, leaves the counters alone, says so in the
// condition, and propagates.
// ---------------------------------------------------------------------------

const (
	restoreErpNS   = "rst-erp-ns"
	restoreErpName = "recover-erp"
	restoreErpRun  = "dr-erp-20260810-010000"
	restoreErpLoc  = "rst-erp-loc"
	restoreErpKEK  = "rst-erp-kek"
	restoreErpS3   = "rst-erp-s3"
	// The two volumes. Their roles are fixed and asymmetric on purpose — see restoreErpWorld.
	restoreErpVolA = "vol-a"
	restoreErpVolB = "vol-b"
)

// restoreErpOwnerID is the identity every operator-namespace object name of this restore derives
// from, read through the production helper so the test and the controller cannot disagree.
func restoreErpOwnerID() string { return restoreOwnerID(restoreErpNS, restoreErpName) }

// restoreErpVolumeJob is one volume's mover Job name.
func restoreErpVolumeJob(pvc string) string {
	return restoreJobName(restoreNamePrefix(restoreErpOwnerID(), pvc))
}

// restoreErpResourcesJob is the manifest half's Job name.
func restoreErpResourcesJob() string {
	return manifestsJobName(resourcesJobPrefix(restoreErpOwnerID()))
}

// manifestsSnapshotFixture is the kind=manifests snapshot of the run, which is what makes
// resolveResourcesPlan return a plan at all (no manifest snapshot ⇒ no manifest half ⇒ the whole of
// instance 1 is unreachable and the test would pass vacuously).
func manifestsSnapshotFixture(id string) restic.Snapshot {
	return restic.Snapshot{
		ID:   id,
		Time: time.Now(),
		Host: "erp-cluster",
		Tags: []string{
			restic.TagBase,
			restic.Tag(restic.TagKeyKind, restic.KindManifests),
			restic.Tag(restic.TagKeyNamespace, restoreErpNS),
			restic.Tag(restic.TagKeyRun, restoreErpRun),
		},
		Paths: []string{"/manifests/" + restoreErpNS},
	}
}

// settledMoverJob builds a mover Job in a terminal state plus the pod whose termination message
// carries its result — the only durable record of what a mover did, exactly as production reads it
// (readMoverResult reads the POD, and these halves have no annotation fallback).
//
// failed=true stamps the JobFailed condition, which settles a restore volume as failed WITHOUT
// touching the target exposer: the drive loop needs a real, publishable verdict for at least one
// volume, and a failure is the one outcome reachable without playing the binder, the PV controller
// and the handover. What is under test is which verdicts get PERSISTED, not how they are reached.
func settledMoverJob(name string, result mover.MoverResult, failed bool) []crclient.Object {
	msg, err := result.Encode()
	if err != nil {
		panic(err) // a fixture that cannot encode is a broken test, not a test failure.
	}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: suiteOperatorNamespace,
			Labels: map[string]string{
				apiconst.LabelManagedBy:    apiconst.ManagedByValue,
				apiconst.LabelExposureKind: rexposer.KindTwin,
			},
		},
	}
	exitCode := int32(0)
	if failed {
		job.Status.Failed = 1
		job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}
		exitCode = 1
	} else {
		job.Status.Succeeded = 1
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name + "-pod",
			Namespace: suiteOperatorNamespace,
			Labels:    map[string]string{batchv1.JobNameLabel: name},
		},
		Spec: corev1.PodSpec{NodeName: "node-erp", RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{{Name: "mover", Image: suiteMoverImage}}},
		Status: corev1.PodStatus{
			Phase: corev1.PodSucceeded,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:  "mover",
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: exitCode, Message: msg}},
			}},
		},
	}
	return []crclient.Object{job, pod}
}

// restoreErpWorld builds everything one Restore pass needs, with the two volumes in fixed roles:
//
//   - vol-b always has a SETTLED (failed) mover Job, so every pass has one real verdict to publish.
//     Without it a test could not tell "the counters were persisted" from "there was nothing to
//     persist".
//   - vol-a is left to the caller: absent (so the drive starts it), or refused (so the drive produces
//     the transient per-volume error the class is about).
//
// resourcesSettled adds the manifest half's finished Job + its result pod, so the pass under test is
// the one that READS the apply's report — which is the pass whose loss is unrecoverable.
func restoreErpWorld(t *testing.T, resourcesSettled bool, extra ...crclient.Object) []crclient.Object {
	t.Helper()
	objs := []crclient.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: restoreErpNS}},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: restoreErpKEK, Namespace: suiteOperatorNamespace},
			Data:       map[string][]byte{kekIdentityDataKey: []byte(testAgeIdentity(t))},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: restoreErpS3, Namespace: suiteOperatorNamespace},
			Data: map[string][]byte{
				mover.SecretKeyAWSAccessKeyID:     []byte("AKIARST"),
				mover.SecretKeyAWSSecretAccessKey: []byte("secret"),
			},
		},
		&cbv1.ClusterBackupLocation{
			ObjectMeta: metav1.ObjectMeta{Name: restoreErpLoc},
			Spec: cbv1.ClusterBackupLocationSpec{
				ClusterID: "erp-cluster",
				S3: cbv1.S3Spec{
					Endpoint:             "https://s3.invalid.test",
					Bucket:               "erp",
					CredentialsSecretRef: cbv1.LocalObjectReference{Name: restoreErpS3},
				},
				Encryption: cbv1.ClusterEncryptionSpec{
					ClusterKEKSecretRef: cbv1.LocalObjectReference{Name: restoreErpKEK},
				},
			},
		},
		&cbv1.BackupRepository{
			ObjectMeta: metav1.ObjectMeta{Name: restoreErpLoc},
			Status: cbv1.BackupRepositoryStatus{
				Initialized:   true,
				RepositoryURL: "s3:https://s3.invalid.test/erp",
			},
		},
		// The source: a discovery projection of the run, the realistic restorable shape.
		&cbv1.Backup{
			ObjectMeta: metav1.ObjectMeta{
				Namespace:   restoreErpNS,
				Name:        restoreErpRun,
				Annotations: map[string]string{apiconst.AnnotationProjected: apiconst.AnnotationProjectedValue},
				Labels:      map[string]string{apiconst.LabelOrigin: apiconst.OriginCluster},
			},
			Spec: cbv1.BackupSpec{
				LocationRef: cbv1.LocationReference{Kind: kindClusterBackupLocation, Name: restoreErpLoc},
			},
			Status: cbv1.BackupStatus{
				Phase: string(status.BackupPhaseCompleted),
				Volumes: []cbv1.VolumeStatus{
					{Pvc: restoreErpVolA, Phase: status.VolumePhaseCompleted, SnapshotID: strings.Repeat("a", 64)},
					{Pvc: restoreErpVolB, Phase: status.VolumePhaseCompleted, SnapshotID: strings.Repeat("b", 64)},
				},
			},
		},
	}
	// vol-b's settled (failed) mover: the pass's one publishable verdict.
	objs = append(objs, settledMoverJob(restoreErpVolumeJob(restoreErpVolB),
		mover.MoverResult{Operation: string(mover.OpRestore), Error: "restic restore failed"}, true)...)
	if resourcesSettled {
		objs = append(objs, settledMoverJob(restoreErpResourcesJob(), mover.MoverResult{
			OK: true, Operation: string(mover.OpManifestsRestore), RestoredResources: 138,
		}, false)...)
	}
	return append(objs, extra...)
}

// newRestoreErpReconciler wires a Restore reconciler over a fake client carrying the refusal under
// test, plus the mediated lister seeded with the run's data AND manifests snapshots.
//
// The finalizer is pre-set on the fixture Restore (see restoreErpObject) because the real first pass
// only adds it and requeues; the passes this file is about are the ones after that.
func newRestoreErpReconciler(t *testing.T, funcs interceptor.Funcs, objs []crclient.Object) (*RestoreReconciler, crclient.Client) {
	t.Helper()
	s := aggregateScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(objs...).
		WithStatusSubresource(&cbv1.Restore{}, &cbv1.Backup{}, &cbv1.BackupRepository{}).
		WithInterceptorFuncs(funcs).
		Build()

	lister := &stubFilteredLister{}
	lister.seed([]restic.Snapshot{
		dataSnapshot(strings.Repeat("a", 64), restoreErpNS, restoreErpVolA, restoreErpRun),
		dataSnapshot(strings.Repeat("b", 64), restoreErpNS, restoreErpVolB, restoreErpRun),
		manifestsSnapshotFixture(strings.Repeat("c", 64)),
	})

	r := NewRestoreReconciler(c, s, secrets.NewByNameReader(c),
		rexposer.NewTargetExposer(c, suiteOperatorNamespace), lister,
		suiteOperatorNamespace, suiteMoverImage, suiteMoverProfiles, suiteMoverPlacement,
		suiteManifestMoverSA, suiteManifestWriterRole, events.NewFakeRecorder(128), nil)
	return r, c
}

// restoreErpObject is the Restore under test: confirmed, finalizer already present, and carrying the
// PREVIOUS pass's published counters. Those seeded counters are load-bearing in the third test: they
// are what a pass with no verdict must leave alone rather than reset to zero.
func restoreErpObject(planned, restored, failed int32) *cbv1.Restore {
	return &cbv1.Restore{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:  restoreErpNS,
			Name:       restoreErpName,
			UID:        types.UID("cccccccc-cccc-cccc-cccc-cccccccccccc"),
			Finalizers: []string{apiconst.FinalizerRestore},
		},
		Spec: cbv1.RestoreSpec{
			Source:       cbv1.RestoreSource{Backup: restoreErpRun},
			Mode:         cbv1.RestoreModeOverwrite,
			Confirmation: restoreErpNS,
		},
		Status: cbv1.RestoreStatus{
			Phase:           string(status.RestorePhaseRunning),
			PlannedVolumes:  planned,
			RestoredVolumes: restored,
			FailedVolumes:   failed,
		},
	}
}

// getRestoreErp re-reads the Restore from the store. Every assertion in this file goes through it and
// never through the object the reconciler mutated — a status "write" that was discarded is
// indistinguishable from one that landed unless the read is a fresh one.
func getRestoreErp(t *testing.T, c crclient.Client) cbv1.Restore {
	t.Helper()
	var r cbv1.Restore
	if err := c.Get(context.Background(), crclient.ObjectKey{Namespace: restoreErpNS, Name: restoreErpName}, &r); err != nil {
		t.Fatalf("get Restore: %v", err)
	}
	return r
}

// readyMessage returns the Ready condition's message, which is where a non-terminal restore says what
// it is waiting on and what went wrong.
func readyMessage(conds []metav1.Condition) string {
	if c := status.FindCondition(conds, ConditionReady); c != nil {
		return c.Message
	}
	return ""
}

// TestRestoreManifestHalfFailureStillDrivesAndPersistsTheVolumes is instance 1.
//
// The manifest Job's CREATE is refused — a validating webhook, a narrowed batch RBAC, a quota; all
// durable. The old code returned that error before the volumes were driven at all AND before the
// status write, so the restore's volumes were held back for as long as the cause lasted and the object
// said nothing about any of it.
//
// Mutations that must turn this red: restoring `if err != nil { return ctrl.Result{}, err }` after the
// advanceResources call (plannedVolumes never reaches the store — the whole pass is discarded); and
// dropping the deferred `errors.Join(resourcesErr, drive.passErr)` at the bottom of the non-terminal
// branch (the pass persists but the failure is never counted or backed off, which is how a manifest
// half that cannot start ships invisible).
func TestRestoreManifestHalfFailureStillDrivesAndPersistsTheVolumes(t *testing.T) {
	ctx := context.Background()
	objs := restoreErpWorld(t, false, restoreErpObject(0, 0, 0))
	// vol-a also gets a settled failed Job, so this test's drive is entirely deterministic and the
	// only failure in the pass is the manifest half's.
	objs = append(objs, settledMoverJob(restoreErpVolumeJob(restoreErpVolA),
		mover.MoverResult{Operation: string(mover.OpRestore), Error: "restic restore failed"}, true)...)

	r, c := newRestoreErpReconciler(t, interceptor.Funcs{
		Create: func(ctx context.Context, cl crclient.WithWatch, obj crclient.Object, opts ...crclient.CreateOption) error {
			if job, isJob := obj.(*batchv1.Job); isJob && job.Name == restoreErpResourcesJob() {
				return apierrors.NewForbidden(schema.GroupResource{Group: "batch", Resource: "jobs"},
					job.Name, errRefusedByTest)
			}
			return cl.Create(ctx, obj, opts...)
		},
	}, objs)

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: restoreErpNS, Name: restoreErpName}})
	if err == nil {
		t.Fatal("Reconcile returned no error: the manifest half's failure is a whole-object fault and must " +
			"still be propagated, or the restore stops being backed off and nothing counts the failure")
	}
	if !strings.Contains(err.Error(), "create manifest restore Job") {
		t.Errorf("Reconcile error = %v, want it to name the refused Job create", err)
	}

	got := getRestoreErp(t, c)
	if got.Status.PlannedVolumes != 2 {
		t.Errorf("plannedVolumes = %d, want 2: the volumes were driven and classified on this pass, and "+
			"the manifest half's bad day must not discard their verdicts — nor stop them being driven at all",
			got.Status.PlannedVolumes)
	}
	if got.Status.FailedVolumes != 2 {
		t.Errorf("failedVolumes = %d, want 2 — both volumes settled failed this pass", got.Status.FailedVolumes)
	}
	if got.Status.Phase != string(status.RestorePhaseRunning) {
		t.Errorf("phase = %q, want Running: the manifest half never settled, so the restore is not finished "+
			"and must not trip the already-terminal short-circuit", got.Status.Phase)
	}
	if msg := readyMessage(got.Status.Conditions); !strings.Contains(msg, "manifests applying") {
		t.Errorf("Ready message = %q, want it to say which half is still outstanding", msg)
	}
}

// TestRestoreOneVolumeErrorKeepsTheManifestReportAndTheCounts is instance 2, and the sharpest
// consequence of the class anywhere in this sweep.
//
// The manifest half SETTLES on this pass: advanceResources reads the mover's report (138 objects
// applied) out of the pod's termination message and stamps it on status in memory. One volume's mover
// Job then cannot be read — the shape of narrowed batch RBAC, or an apiserver having a bad minute —
// which driveVolumes correctly treats as "no verdict yet, keep going". The old code returned that
// error, discarding the report; and because readMoverResult reads the POD, a pod that is gone by the
// next pass turns a clean apply into "did not report a result … some resources may have been applied",
// failedCount 1, which then folds into the restore's terminal phase as a failure.
//
// Mutations that must turn this red: restoring `if drive.err != nil { return ctrl.Result{}, drive.err }`
// (status.resources never reaches the store, and neither do the counts); returning drive.volumeErr from
// the non-terminal branch (the pass would then be backed off for a per-volume fault, and this test's
// nil-error assertion fails); and dropping the volumeErr clause from restoreProgressMessage (the one
// cause that moves no counter becomes invisible).
func TestRestoreOneVolumeErrorKeepsTheManifestReportAndTheCounts(t *testing.T) {
	ctx := context.Background()
	refusedJob := restoreErpVolumeJob(restoreErpVolA)
	r, c := newRestoreErpReconciler(t, interceptor.Funcs{
		Get: func(ctx context.Context, cl crclient.WithWatch, key crclient.ObjectKey, obj crclient.Object, opts ...crclient.GetOption) error {
			if _, isJob := obj.(*batchv1.Job); isJob && key.Name == refusedJob {
				return apierrors.NewForbidden(schema.GroupResource{Group: "batch", Resource: "jobs"},
					key.Name, errRefusedByTest)
			}
			return cl.Get(ctx, key, obj, opts...)
		},
	}, restoreErpWorld(t, true, restoreErpObject(0, 0, 0)))

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: restoreErpNS, Name: restoreErpName}})
	if err != nil {
		t.Fatalf("Reconcile returned %v: a TRANSIENT per-volume error must be recorded, not propagated — "+
			"controller-runtime charges the backoff to the RESTORE, so one flaky volume would stretch the "+
			"poll driving every other volume of it", err)
	}

	got := getRestoreErp(t, c)

	// THE REPORT THAT WAS BEING LOST. It exists exactly once, in the pod's termination message, and
	// this pass is the only one that could read it.
	if got.Status.Resources == nil {
		t.Fatal("status.resources is nil: the manifest half's report was read on this pass and then " +
			"discarded, and it cannot be re-derived once the mover pod is gone — the next pass would " +
			"publish \"some resources may have been applied\" over an apply that fully succeeded")
	}
	if got.Status.RestoredResources != 138 {
		t.Errorf("restoredResources = %d, want 138", got.Status.RestoredResources)
	}
	if got.Status.Resources.FailedCount != 0 {
		t.Errorf("resources.failedCount = %d, want 0: this apply succeeded, and reporting otherwise "+
			"degrades the restore's terminal phase over a fault that was somewhere else entirely",
			got.Status.Resources.FailedCount)
	}

	// And the volume accounting the same pass computed: two planned, one settled failed, one still in
	// flight (the refused one — no verdict, which is neither restored nor failed).
	if got.Status.PlannedVolumes != 2 || got.Status.FailedVolumes != 1 || got.Status.RestoredVolumes != 0 {
		t.Errorf("counters = planned %d, restored %d, failed %d; want 2/0/1 — the siblings of a volume "+
			"that could not be read still advanced, and their verdicts still have to be published",
			got.Status.PlannedVolumes, got.Status.RestoredVolumes, got.Status.FailedVolumes)
	}

	// The recorded cause. A retrying volume moves no counter, so without this the object gives an
	// operator no sign at all that something is failing every pass.
	msg := readyMessage(got.Status.Conditions)
	if !strings.Contains(msg, "one volume is retrying after an error") {
		t.Errorf("Ready message = %q, want it to record the transient volume error: the error is no longer "+
			"returned for controller-runtime to log, so status is where it has to appear", msg)
	}
	// The cause itself, not merely that there was one. Asserted on the API server's own verb rather
	// than on the whole sentence: the message is clamped to the status cap (clampMessage), and a real
	// Forbidden carries the operator's ServiceAccount, the resource and the namespace before it ever
	// reaches the reason — so what has to survive the cap is the kind of refusal, which is what sends
	// an operator to RBAC rather than to the storage.
	if !strings.Contains(msg, "is forbidden") {
		t.Errorf("Ready message = %q, want it to carry the API server's own verdict", msg)
	}
	if n := len([]rune(msg)); n > clusterBackupMessageCap {
		t.Errorf("Ready message is %d runes, past the %d cap: a status field must never carry an "+
			"unbounded blob into etcd", n, clusterBackupMessageCap)
	}
}

// TestRestorePassWithNoVerdictLeavesTheCountersAlone is the case neither shape above covers: the drive
// could not read the mover census, so NO volume was looked at and the tally is empty.
//
// Publishing that tally is not a missing answer, it is a wrong one: it would overwrite a real "1 of 2
// restored" with plannedVolumes 0, in front of the person deciding whether to abandon a restore. So
// the pass persists everything else it computed (the manifest report above all), leaves the counters
// at their last published values, says in the condition that it could not assess them, and propagates
// — the census read is a whole-pass fault, not a per-volume one.
//
// Mutations that must turn this red: dropping the `if drive.tallied()` guard around stampVolumeCounts
// (restoredVolumes goes back to 0 and reports a regression that did not happen); dropping
// `!drive.tallied()` from the non-terminal condition (an empty plan list would take the TERMINAL
// roll-up on a pass that never looked at anything); and returning drive.passErr before the write (the
// manifest report is discarded again).
func TestRestorePassWithNoVerdictLeavesTheCountersAlone(t *testing.T) {
	ctx := context.Background()
	r, c := newRestoreErpReconciler(t, interceptor.Funcs{
		List: func(ctx context.Context, cl crclient.WithWatch, list crclient.ObjectList, opts ...crclient.ListOption) error {
			// ownerRunningMovers is the only Job LIST in this Reconcile's path — the concurrency gate's
			// supply side, read before the first volume is touched.
			if _, isJobs := list.(*batchv1.JobList); isJobs {
				return apierrors.NewForbidden(schema.GroupResource{Group: "batch", Resource: "jobs"},
					"", errRefusedByTest)
			}
			return cl.List(ctx, list, opts...)
		},
	}, restoreErpWorld(t, true, restoreErpObject(2, 1, 0)))

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: restoreErpNS, Name: restoreErpName}})
	if err == nil {
		t.Fatal("Reconcile returned no error: a pass that could not assess any volume is a whole-pass " +
			"fault and belongs in the backoff")
	}
	if !strings.Contains(err.Error(), "concurrency gate") {
		t.Errorf("Reconcile error = %v, want the census read named", err)
	}

	got := getRestoreErp(t, c)
	if got.Status.PlannedVolumes != 2 || got.Status.RestoredVolumes != 1 {
		t.Errorf("counters = planned %d, restored %d; want the previous pass's 2/1 left untouched: an "+
			"untallied pass has no verdict for any volume, and zeroing them reports a regression that "+
			"did not happen to somebody mid-disaster",
			got.Status.PlannedVolumes, got.Status.RestoredVolumes)
	}
	if got.Status.Resources == nil || got.Status.RestoredResources != 138 {
		t.Errorf("status.resources = %+v, restoredResources = %d: the manifest half's report was read on "+
			"this pass and must be persisted even though the volumes could not be assessed",
			got.Status.Resources, got.Status.RestoredResources)
	}
	if got.Status.Phase != string(status.RestorePhaseRunning) {
		t.Errorf("phase = %q, want Running", got.Status.Phase)
	}
	msg := readyMessage(got.Status.Conditions)
	if !strings.Contains(msg, "could not assess the volumes") {
		t.Errorf("Ready message = %q, want it to say the counts are the previous pass's and why", msg)
	}
	if strings.Contains(msg, "0/0 volumes") {
		t.Errorf("Ready message = %q: it renders an empty tally as real counts, which is the one thing "+
			"this branch exists to avoid", msg)
	}
}

// TestRestoreEmptySelectionDoesNotGoTerminalOnAnUnassessedPass is the one scenario the counters cannot
// speak for, and the only one that pins the `!drive.tallied()` term in the non-terminal condition.
//
// An empty volume selection is a VALID, immediately-terminal restore (02-api: a present-but-empty list
// restores nothing) — the resources-only restore. len(plans) is therefore 0, so `settled() < len(plans)`
// is false no matter what happened, and the terminal roll-up would be taken on a pass that never looked
// at anything: Completed, published over a drive that failed, with the error never returned and the
// already-terminal short-circuit sealing it. tallied() is what stands between those two facts.
//
// Mutation that must turn this red: dropping `!drive.tallied() ||` from the non-terminal condition.
func TestRestoreEmptySelectionDoesNotGoTerminalOnAnUnassessedPass(t *testing.T) {
	ctx := context.Background()
	obj := restoreErpObject(0, 0, 0)
	// Present-but-empty: restore the manifests, none of the volumes.
	obj.Spec.Volumes = []cbv1.VolumeSelectorItem{}

	r, c := newRestoreErpReconciler(t, interceptor.Funcs{
		List: func(ctx context.Context, cl crclient.WithWatch, list crclient.ObjectList, opts ...crclient.ListOption) error {
			if _, isJobs := list.(*batchv1.JobList); isJobs {
				return apierrors.NewForbidden(schema.GroupResource{Group: "batch", Resource: "jobs"},
					"", errRefusedByTest)
			}
			return cl.List(ctx, list, opts...)
		},
	}, restoreErpWorld(t, true, obj))

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: restoreErpNS, Name: restoreErpName}})
	if err == nil {
		t.Fatal("Reconcile returned no error over a drive that failed: the pass concluded something it had " +
			"no evidence for")
	}

	got := getRestoreErp(t, c)
	if status.IsTerminalRestorePhase(got.Status.Phase) {
		t.Fatalf("phase = %q: a restore went TERMINAL on a pass that could not assess a single volume — and "+
			"a terminal Restore is never re-executed, so that verdict is final", got.Status.Phase)
	}
}

// TestRestoreProgressMessageHasOneShapePerKindOfPass pins the two shapes as a pure statement, because
// the difference between them is a judgement about honesty rather than about formatting, and it is
// cheap to get wrong in a later edit that "unifies" them.
func TestRestoreProgressMessageHasOneShapePerKindOfPass(t *testing.T) {
	tallied := volumeDrive{tally: status.RestoreTally{Planned: 6, Restored: 4, Failed: 1, InFlight: 1}}

	if got := restoreProgressMessage(tallied, ", manifests settled"); !strings.Contains(got, "4/6 volumes restored") ||
		!strings.Contains(got, "1 failed") || !strings.Contains(got, "manifests settled") {
		t.Errorf("tallied message = %q, want the real counts and the caller's own half", got)
	}

	withVolumeErr := tallied
	withVolumeErr.volumeErr = errRefusedByTest
	if got := restoreProgressMessage(withVolumeErr, ""); !strings.Contains(got, "one volume is retrying") ||
		!strings.Contains(got, errRefusedByTest.Error()) {
		t.Errorf("message with a transient volume error = %q, want the cause recorded: it moves no counter, "+
			"so status is the only place it can appear", got)
	}

	untallied := volumeDrive{passErr: errRefusedByTest}
	got := restoreProgressMessage(untallied, "")
	if strings.Contains(got, "0/0") || strings.Contains(got, "volumes restored") {
		t.Errorf("untallied message = %q: an empty tally must NEVER be rendered as counts — that is a "+
			"false report, not a missing one", got)
	}
	if !strings.Contains(got, "could not assess") || !strings.Contains(got, errRefusedByTest.Error()) {
		t.Errorf("untallied message = %q, want it to say the assessment failed and why", got)
	}

	// A status field is not a log. A mover error can be arbitrarily long and this lands in etcd.
	long := volumeDrive{tally: status.RestoreTally{Planned: 1}}
	long.volumeErr = &longTestError{}
	if n := len([]rune(restoreProgressMessage(long, ""))); n > clusterBackupMessageCap {
		t.Errorf("message is %d runes, past the %d cap", n, clusterBackupMessageCap)
	}
}

// longTestError is an error whose message is far past the status cap.
type longTestError struct{}

func (*longTestError) Error() string { return strings.Repeat("x", 4096) }
