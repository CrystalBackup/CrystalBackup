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
// THE ERRORED-PASS CLASS, SWEPT ON THE ADMIN CLUSTERRESTORE.
//
// Same class, same two shapes as the namespaced twin, and one instructive difference in the FIX.
//
//  1. STEP (3b), advanceClusterRestore. Its error was returned upstream of every status write in the
//     function. It now persists first and propagates after — but the volumes are still NOT driven on
//     such a pass, and that is not the coupling the namespaced twin removes: here the ordering is a
//     real dependency (a namespace's PVCs reference StorageClasses this half brings back), so holding
//     the volumes is correct and only the DISCARDING was wrong. The same audited class, two different
//     right answers, decided by what the failing unit is.
//  2. STEP (4), the volume drive. Identical to the namespaced twin, and it costs the same thing: on
//     the pass where the cluster-scoped apply's report is read out of the mover pod's termination
//     message, one unreadable volume Job discarded it — and once the pod is gone the re-derived
//     verdict is "did not report a result … some resources may have been applied", failedCount 1,
//     over a clean recreation of forty CRDs and cluster RoleBindings.
// ---------------------------------------------------------------------------

const (
	clusterRestoreErpName   = "dr-erp"
	clusterRestoreErpSrcNS  = "crst-erp-src"
	clusterRestoreErpTgtNS  = "crst-erp-tgt"
	clusterRestoreErpRun    = "dr-erp-20260810-020000"
	clusterRestoreErpVolA   = "vol-a"
	clusterRestoreErpVolB   = "vol-b"
	clusterRestoreErpLoc    = "crst-erp-loc"
	clusterRestoreErpKEK    = "crst-erp-kek"
	clusterRestoreErpS3Cred = "crst-erp-s3"
)

func clusterRestoreErpOwnerID() string { return clusterRestoreOwnerID(clusterRestoreErpName) }

func clusterRestoreErpVolumeJob(pvc string) string {
	return restoreJobName(restoreNamePrefix(clusterRestoreErpOwnerID(), pvc))
}

func clusterRestoreErpClusterJob() string {
	return clusterRestoreJobName(clusterRestoreErpOwnerID())
}

// clusterManifestsSnapshotFixture is the run's kind=cluster-manifests snapshot. It deliberately
// carries NO namespace tag, exactly as restic.ClusterManifestsIdentity produces it — which is why the
// controller resolves it through a second, separately filtered listing.
func clusterManifestsSnapshotFixture(id string) restic.Snapshot {
	return restic.Snapshot{
		ID:   id,
		Host: "erp-cluster",
		Tags: []string{
			restic.TagBase,
			restic.Tag(restic.TagKeyKind, restic.KindClusterManifests),
			restic.Tag(restic.TagKeyRun, clusterRestoreErpRun),
		},
		Paths: []string{mover.ClusterManifestsRoot},
	}
}

// clusterRestoreErpWorld builds the world one ClusterRestore pass needs. vol-b always has a settled
// (failed) mover Job so every pass has one publishable verdict; clusterSettled adds the cluster-scoped
// half's finished Job and its result pod, making the pass under test the one that READS the apply's
// report — the pass whose loss cannot be recovered.
func clusterRestoreErpWorld(t *testing.T, clusterSettled bool, extra ...crclient.Object) []crclient.Object {
	t.Helper()
	objs := []crclient.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: clusterRestoreErpTgtNS}},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: clusterRestoreErpKEK, Namespace: suiteOperatorNamespace},
			Data:       map[string][]byte{kekIdentityDataKey: []byte(testAgeIdentity(t))},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: clusterRestoreErpS3Cred, Namespace: suiteOperatorNamespace},
			Data: map[string][]byte{
				mover.SecretKeyAWSAccessKeyID:     []byte("AKIACRST"),
				mover.SecretKeyAWSSecretAccessKey: []byte("secret"),
			},
		},
		&cbv1.ClusterBackupLocation{
			ObjectMeta: metav1.ObjectMeta{Name: clusterRestoreErpLoc},
			Spec: cbv1.ClusterBackupLocationSpec{
				ClusterID: "erp-cluster",
				S3: cbv1.S3Spec{
					Endpoint:             "https://s3.invalid.test",
					Bucket:               "erp",
					CredentialsSecretRef: cbv1.LocalObjectReference{Name: clusterRestoreErpS3Cred},
				},
				Encryption: cbv1.ClusterEncryptionSpec{
					ClusterKEKSecretRef: cbv1.LocalObjectReference{Name: clusterRestoreErpKEK},
				},
			},
		},
		&cbv1.BackupRepository{
			ObjectMeta: metav1.ObjectMeta{Name: clusterRestoreErpLoc},
			Status: cbv1.BackupRepositoryStatus{
				Initialized:   true,
				RepositoryURL: "s3:https://s3.invalid.test/erp",
			},
		},
	}
	objs = append(objs, settledMoverJob(clusterRestoreErpVolumeJob(clusterRestoreErpVolB),
		mover.MoverResult{Operation: string(mover.OpRestore), Error: "restic restore failed"}, true)...)
	if clusterSettled {
		objs = append(objs, settledMoverJob(clusterRestoreErpClusterJob(), mover.MoverResult{
			OK: true, Operation: string(mover.OpClusterManifestsRestore), RestoredResources: 40,
		}, false)...)
	}
	return append(objs, extra...)
}

// clusterRestoreErpObject is the ClusterRestore under test: confirmed, finalizer already present (the
// real first pass only adds it), the cluster-scoped half opted IN, and carrying the previous pass's
// published counters.
func clusterRestoreErpObject(planned, restored, failed int32) *cbv1.ClusterRestore {
	return &cbv1.ClusterRestore{
		ObjectMeta: metav1.ObjectMeta{
			Name:       clusterRestoreErpName,
			UID:        types.UID("dddddddd-dddd-dddd-dddd-dddddddddddd"),
			Finalizers: []string{apiconst.FinalizerClusterRestore},
		},
		Spec: cbv1.ClusterRestoreSpec{
			Source: cbv1.ClusterRestoreSource{
				LocationRef: cbv1.LocalObjectReference{Name: clusterRestoreErpLoc},
				Namespace:   clusterRestoreErpSrcNS,
				Backup:      clusterRestoreErpRun,
			},
			Target:           cbv1.ClusterRestoreTarget{Namespace: clusterRestoreErpTgtNS},
			Mode:             cbv1.RestoreModeOverwrite,
			Confirmation:     clusterRestoreErpTgtNS,
			ClusterResources: &cbv1.ClusterResourceRestoreSpec{},
		},
		Status: cbv1.ClusterRestoreStatus{
			Phase:           string(status.RestorePhaseRunning),
			PlannedVolumes:  planned,
			RestoredVolumes: restored,
			FailedVolumes:   failed,
		},
	}
}

func newClusterRestoreErpReconciler(t *testing.T, funcs interceptor.Funcs, objs []crclient.Object) (*ClusterRestoreReconciler, crclient.Client) {
	t.Helper()
	s := aggregateScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(objs...).
		WithStatusSubresource(&cbv1.ClusterRestore{}, &cbv1.BackupRepository{}).
		WithInterceptorFuncs(funcs).
		Build()

	lister := &stubFilteredLister{}
	lister.seed([]restic.Snapshot{
		dataSnapshot(strings.Repeat("a", 64), clusterRestoreErpSrcNS, clusterRestoreErpVolA, clusterRestoreErpRun),
		dataSnapshot(strings.Repeat("b", 64), clusterRestoreErpSrcNS, clusterRestoreErpVolB, clusterRestoreErpRun),
		clusterManifestsSnapshotFixture(strings.Repeat("e", 64)),
	})

	r := NewClusterRestoreReconciler(c, s, secrets.NewByNameReader(c),
		rexposer.NewTargetExposer(c, suiteOperatorNamespace), lister,
		suiteOperatorNamespace, suiteMoverImage, suiteMoverProfiles, suiteMoverPlacement,
		suiteManifestMoverSA, suiteClusterManifestWriterRole, events.NewFakeRecorder(128), nil)
	return r, c
}

func getClusterRestoreErp(t *testing.T, c crclient.Client) cbv1.ClusterRestore {
	t.Helper()
	var cr cbv1.ClusterRestore
	if err := c.Get(context.Background(), crclient.ObjectKey{Name: clusterRestoreErpName}, &cr); err != nil {
		t.Fatalf("get ClusterRestore: %v", err)
	}
	return cr
}

// TestClusterRestoreClusterHalfFailureStillPersistsThePass is instance 1.
//
// The cluster-scoped Job's CREATE is refused. The volumes stay held back — correctly, they depend on
// the StorageClasses this half restores — but the pass must still say so durably, with the cause, and
// still be backed off.
//
// Mutations that must turn this red: restoring `if err != nil { return ctrl.Result{}, err }` after the
// advanceClusterRestore call (the phase and the condition never reach the store, so an operator
// watching a DR sees an object that says nothing at all while every pass fails); and dropping the
// clusterErr clause from the condition message (the object then claims to be "restoring cluster-scoped
// resources before volumes" indefinitely, with nothing anywhere saying the attempt fails every pass).
func TestClusterRestoreClusterHalfFailureStillPersistsThePass(t *testing.T) {
	ctx := context.Background()
	r, c := newClusterRestoreErpReconciler(t, interceptor.Funcs{
		Create: func(ctx context.Context, cl crclient.WithWatch, obj crclient.Object, opts ...crclient.CreateOption) error {
			if job, isJob := obj.(*batchv1.Job); isJob && job.Name == clusterRestoreErpClusterJob() {
				return apierrors.NewForbidden(schema.GroupResource{Group: "batch", Resource: "jobs"},
					job.Name, errRefusedByTest)
			}
			return cl.Create(ctx, obj, opts...)
		},
	}, clusterRestoreErpWorld(t, false, clusterRestoreErpObject(0, 0, 0)))

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: clusterRestoreErpName}})
	if err == nil {
		t.Fatal("Reconcile returned no error: the cluster-scoped half is a whole-object fault and must " +
			"still be propagated")
	}
	if !strings.Contains(err.Error(), "create cluster-manifest restore Job") {
		t.Errorf("Reconcile error = %v, want the refused Job create named", err)
	}

	got := getClusterRestoreErp(t, c)
	if got.Status.Phase != string(status.RestorePhaseRunning) {
		t.Errorf("phase = %q, want Running to have reached the store", got.Status.Phase)
	}
	msg := readyMessage(got.Status.Conditions)
	if !strings.Contains(msg, "restoring cluster-scoped resources before volumes") {
		t.Errorf("Ready message = %q, want it to say which half the restore is waiting on", msg)
	}
	if !strings.Contains(msg, "is forbidden") {
		t.Errorf("Ready message = %q, want the cause recorded: an object that reports only \"restoring\" "+
			"while every pass fails tells an operator mid-DR nothing they can act on", msg)
	}
	if n := len([]rune(msg)); n > clusterBackupMessageCap {
		t.Errorf("Ready message is %d runes, past the %d cap", n, clusterBackupMessageCap)
	}

	// The volumes are deliberately NOT driven while the cluster half is unsettled: no mover Job for
	// vol-a may exist. This is the half of the fix that is DIFFERENT from the namespaced twin, and
	// getting it wrong would provision PVCs against StorageClasses that are not there yet.
	err = c.Get(ctx, crclient.ObjectKey{Namespace: suiteOperatorNamespace,
		Name: clusterRestoreErpVolumeJob(clusterRestoreErpVolA)}, &batchv1.Job{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("a volume mover Job exists (%v): the volumes must stay held back until the cluster-scoped "+
			"half settles — that ordering is a real dependency, not the coupling this sweep removes", err)
	}
}

// TestClusterRestoreOneVolumeErrorKeepsTheClusterReport is instance 2: the cluster-scoped apply's
// report survives a sibling volume whose mover Job cannot be read.
//
// Mutations that must turn this red: restoring `if drive.err != nil { return ctrl.Result{}, drive.err }`
// (status.resources never reaches the store, and the next pass publishes "some resources may have been
// applied" over an apply of forty cluster objects that fully succeeded); and returning drive.volumeErr
// from the non-terminal branch (the nil-error assertion fails — a per-volume fault would then back off
// the whole DR).
func TestClusterRestoreOneVolumeErrorKeepsTheClusterReport(t *testing.T) {
	ctx := context.Background()
	refused := clusterRestoreErpVolumeJob(clusterRestoreErpVolA)
	r, c := newClusterRestoreErpReconciler(t, interceptor.Funcs{
		Get: func(ctx context.Context, cl crclient.WithWatch, key crclient.ObjectKey, obj crclient.Object, opts ...crclient.GetOption) error {
			if _, isJob := obj.(*batchv1.Job); isJob && key.Name == refused {
				return apierrors.NewForbidden(schema.GroupResource{Group: "batch", Resource: "jobs"},
					key.Name, errRefusedByTest)
			}
			return cl.Get(ctx, key, obj, opts...)
		},
	}, clusterRestoreErpWorld(t, true, clusterRestoreErpObject(0, 0, 0)))

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: clusterRestoreErpName}})
	if err != nil {
		t.Fatalf("Reconcile returned %v: a transient per-volume error must be recorded, not propagated", err)
	}

	got := getClusterRestoreErp(t, c)
	if got.Status.Resources == nil {
		t.Fatal("status.resources is nil: the cluster-scoped apply's report was read on this pass and then " +
			"discarded, and it lives only in the mover pod's termination message")
	}
	if got.Status.RestoredResources != 40 {
		t.Errorf("restoredResources = %d, want 40", got.Status.RestoredResources)
	}
	if got.Status.Resources.FailedCount != 0 {
		t.Errorf("resources.failedCount = %d, want 0: this apply succeeded, and a re-derived failure would "+
			"fold into the restore's terminal phase as a degradation that never happened",
			got.Status.Resources.FailedCount)
	}
	if got.Status.PlannedVolumes != 2 || got.Status.FailedVolumes != 1 {
		t.Errorf("counters = planned %d, failed %d; want 2/1", got.Status.PlannedVolumes, got.Status.FailedVolumes)
	}
	if msg := readyMessage(got.Status.Conditions); !strings.Contains(msg, "one volume is retrying after an error") {
		t.Errorf("Ready message = %q, want the transient volume error recorded", msg)
	}
}

// TestClusterRestoreEmptySelectionDoesNotGoTerminalOnAnUnassessedPass is the ClusterRestore twin of the
// namespaced empty-selection case: with no planned volumes, `settled() < len(plans)` is false whatever
// happened, so without tallied() the terminal roll-up would declare a fleet DR Completed on a pass that
// never looked at a single volume — and a terminal ClusterRestore is never re-executed.
//
// Mutation that must turn this red: dropping `!drive.tallied() ||` from the non-terminal condition.
func TestClusterRestoreEmptySelectionDoesNotGoTerminalOnAnUnassessedPass(t *testing.T) {
	ctx := context.Background()
	obj := clusterRestoreErpObject(0, 0, 0)
	// A selection that matches nothing in the repository's listing: valid, and it plans no volume.
	obj.Spec.Volumes = []cbv1.VolumeSelectorItem{{Names: []string{"no-such-pvc"}}}

	r, c := newClusterRestoreErpReconciler(t, interceptor.Funcs{
		List: func(ctx context.Context, cl crclient.WithWatch, list crclient.ObjectList, opts ...crclient.ListOption) error {
			if _, isJobs := list.(*batchv1.JobList); isJobs {
				return apierrors.NewForbidden(schema.GroupResource{Group: "batch", Resource: "jobs"},
					"", errRefusedByTest)
			}
			return cl.List(ctx, list, opts...)
		},
	}, clusterRestoreErpWorld(t, true, obj))

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: clusterRestoreErpName}})
	if err == nil {
		t.Fatal("Reconcile returned no error over a drive that failed")
	}
	if got := getClusterRestoreErp(t, c); status.IsTerminalRestorePhase(got.Status.Phase) {
		t.Fatalf("phase = %q: a fleet DR went terminal on a pass that assessed nothing", got.Status.Phase)
	}
}

// TestClusterRestorePassWithNoVerdictLeavesTheCountersAlone is the untallied pass, on the plane where
// it matters most: a fleet DR's counters are what somebody watches to decide whether the recovery is
// progressing.
//
// Mutation that must turn this red: dropping the `if drive.tallied()` guard around stampVolumeCounts —
// a DR reporting 3 of 5 volumes back drops to 0 of 0 on one unreadable census read.
func TestClusterRestorePassWithNoVerdictLeavesTheCountersAlone(t *testing.T) {
	ctx := context.Background()
	r, c := newClusterRestoreErpReconciler(t, interceptor.Funcs{
		List: func(ctx context.Context, cl crclient.WithWatch, list crclient.ObjectList, opts ...crclient.ListOption) error {
			if _, isJobs := list.(*batchv1.JobList); isJobs {
				return apierrors.NewForbidden(schema.GroupResource{Group: "batch", Resource: "jobs"},
					"", errRefusedByTest)
			}
			return cl.List(ctx, list, opts...)
		},
	}, clusterRestoreErpWorld(t, true, clusterRestoreErpObject(2, 1, 0)))

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: clusterRestoreErpName}})
	if err == nil {
		t.Fatal("Reconcile returned no error: a pass that could not assess any volume belongs in the backoff")
	}

	got := getClusterRestoreErp(t, c)
	if got.Status.PlannedVolumes != 2 || got.Status.RestoredVolumes != 1 {
		t.Errorf("counters = planned %d, restored %d; want the previous pass's 2/1 left untouched",
			got.Status.PlannedVolumes, got.Status.RestoredVolumes)
	}
	if got.Status.Resources == nil || got.Status.RestoredResources != 40 {
		t.Errorf("the cluster-scoped report was not persisted: resources = %+v, restoredResources = %d",
			got.Status.Resources, got.Status.RestoredResources)
	}
	if msg := readyMessage(got.Status.Conditions); !strings.Contains(msg, "could not assess the volumes") {
		t.Errorf("Ready message = %q, want it to say the counts are the previous pass's and why", msg)
	}
}
