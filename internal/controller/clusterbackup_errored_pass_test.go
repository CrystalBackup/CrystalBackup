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
	"errors"
	"strings"
	"testing"

	"filippo.io/age"
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
	"github.com/CrystalBackup/CrystalBackup/internal/status"
)

// ---------------------------------------------------------------------------
// THE ERRORED-PASS CLASS, SWEPT ON THE CLUSTER PLANE.
//
// The class, in one sentence: a reconcile computes state in memory, mutates its object's status, and
// writes it ONCE near the end — so any error returned BEFORE that single write discards everything
// the pass computed. 0.6.5 closed it in the Backup controller and verified by two independent methods
// that none remained there. The three sibling controllers that share the shape were never swept.
// This file is the ClusterBackup half of that sweep.
//
// TWO INSTANCES existed here, both in step (3.5), the run-level cluster-manifests capture:
//
//  1. resolveClusterCaptureContext returning a real error (the shared BackupRepository could not be
//     READ — RBAC narrowed under a running operator, an apiserver having a bad minute).
//  2. advanceClusterManifests returning one (its Job could not be created — a validating webhook, a
//     narrowed batch RBAC, a quota).
//
// Both sat between the fan-out at step (3) and the single status write at step (4), so either one's
// bad day discarded the whole run's accounting: namespacesMatched, namespacesSucceeded over children
// that had genuinely finished, and the status.failures list the fan-out had just built. Nothing about
// the run reached the store at all, while its children's snapshots sat in the repository with nothing
// pointing at them — and a run held non-terminal is a run a Forbid schedule counts as still working,
// which is the mechanism of the thirty-one-hour incident verbatim.
//
// The fix is PERSIST FIRST, THEN PROPAGATE: the failing unit is the run's own capture, not a
// namespace, so the object controller-runtime charges the backoff to is the object at fault.
//
// WHY THESE ARE FAKE-CLIENT UNIT TESTS rather than envtest specs. The assertion that matters is
// "the status reached the store" — which is made here exactly as the envtest specs make it, by
// RE-READING the object through the client after Reconcile returns, never by inspecting the copy the
// reconciler mutated. What envtest adds over the fake client is a real apiserver's validation and the
// shared informer cache; what it cannot give is a deterministic refusal of one named API call, which
// is the whole scenario. interceptor.Funcs gives that exactly, with no manager racing the spec for
// the same object.
// ---------------------------------------------------------------------------

// erroredPassRun is the run under test in this file, with a UID so its children can be stamped as
// its own (buildRunLedger only counts a stray child that carries this).
const (
	erroredPassRun    = "nightly-20260810-040000"
	erroredPassRunUID = types.UID("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	erroredPassLoc    = "erp-loc"
	erroredPassKEK    = "erp-kek"
	erroredPassS3     = "erp-s3"
)

// errRefusedByTest is the sentence every injected refusal in this sweep's tests carries. It reads
// like the API server's own so a spec's failure output cannot be mistaken for a genuine cluster
// fault, and it is shared by the three files of the sweep — one string, so a grep for it finds every
// injected refusal in the package.
var errRefusedByTest = errors.New("refused by the errored-pass sweep's fault injection")

// testAgeIdentity generates an age identity for a unit test (the Ginkgo helper of the same purpose
// calls Expect, which is unusable from a plain testing.T).
func testAgeIdentity(t *testing.T) string {
	t.Helper()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("generate age identity: %v", err)
	}
	return id.String()
}

// erroredPassObjects builds the whole world one ClusterBackup pass needs: the location, its
// initialized repository, the KEK and S3 Secrets the capture's DEK and creds resolve from, two
// matched namespaces, and one child per namespace — one Completed, one Failed.
//
// The child mix is the point of the fixture rather than decoration: the aggregate the old code threw
// away has to be one that says something an administrator would act on. A single-namespace run whose
// only child was still Pending would leave every counter at zero and the spec could not tell a
// persisted aggregate from a discarded one.
func erroredPassObjects(t *testing.T) []crclient.Object {
	t.Helper()
	return []crclient.Object{
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: erroredPassKEK, Namespace: suiteOperatorNamespace},
			Data:       map[string][]byte{kekIdentityDataKey: []byte(testAgeIdentity(t))},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: erroredPassS3, Namespace: suiteOperatorNamespace},
			Data: map[string][]byte{
				mover.SecretKeyAWSAccessKeyID:     []byte("AKIAERP"),
				mover.SecretKeyAWSSecretAccessKey: []byte("secret"),
			},
		},
		&cbv1.ClusterBackupLocation{
			ObjectMeta: metav1.ObjectMeta{Name: erroredPassLoc},
			Spec: cbv1.ClusterBackupLocationSpec{
				ClusterID: "erp-cluster",
				S3: cbv1.S3Spec{
					Endpoint:             "https://s3.invalid.test",
					Bucket:               "erp",
					CredentialsSecretRef: cbv1.LocalObjectReference{Name: erroredPassS3},
				},
				Encryption: cbv1.ClusterEncryptionSpec{
					ClusterKEKSecretRef: cbv1.LocalObjectReference{Name: erroredPassKEK},
				},
			},
		},
		&cbv1.BackupRepository{
			ObjectMeta: metav1.ObjectMeta{Name: erroredPassLoc},
			Status: cbv1.BackupRepositoryStatus{
				Initialized:   true,
				RepositoryURL: "s3:https://s3.invalid.test/erp",
			},
		},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "erp-ns-a"}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "erp-ns-b"}},
		erroredPassChild("erp-ns-a", string(status.BackupPhaseCompleted), status.VolumePhaseCompleted),
		erroredPassChild("erp-ns-b", string(status.BackupPhaseFailed), status.VolumePhaseFailed),
		&cbv1.ClusterBackup{
			ObjectMeta: metav1.ObjectMeta{Name: erroredPassRun, UID: erroredPassRunUID},
			Spec: cbv1.ClusterBackupSpec{
				ClusterBackupRunSpec: cbv1.ClusterBackupRunSpec{
					LocationRef: cbv1.LocalObjectReference{Name: erroredPassLoc},
					Namespaces:  cbv1.NamespaceSelector{MatchNames: []string{"erp-ns-*"}},
				},
			},
		},
	}
}

// erroredPassChild is one settled child of the run, stamped with the parent UID so the ledger
// recognises it.
func erroredPassChild(ns, phase string, volPhase status.VolumePhase) *cbv1.Backup {
	return &cbv1.Backup{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:   ns,
			Name:        erroredPassRun,
			Annotations: map[string]string{apiconst.AnnotationParentUID: string(erroredPassRunUID)},
			Labels: map[string]string{
				apiconst.LabelClusterBackup: erroredPassRun,
				apiconst.LabelOrigin:        apiconst.OriginCluster,
				apiconst.LabelNamespace:     ns,
			},
		},
		Status: cbv1.BackupStatus{
			Phase:   phase,
			Volumes: []cbv1.VolumeStatus{{Pvc: "data", Phase: volPhase, AddedBytes: 4096}},
		},
	}
}

// newErroredPassReconciler wires a ClusterBackup reconciler over a fake client carrying the
// interceptor under test. The cluster-manifest reader role is set because an empty one disables the
// capture entirely — the very code path being tested.
func newErroredPassReconciler(t *testing.T, funcs interceptor.Funcs) (*ClusterBackupReconciler, crclient.Client) {
	t.Helper()
	s := aggregateScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(erroredPassObjects(t)...).
		WithStatusSubresource(&cbv1.ClusterBackup{}, &cbv1.Backup{}, &cbv1.BackupRepository{}).
		WithInterceptorFuncs(funcs).
		Build()
	return &ClusterBackupReconciler{
		Client:                           c,
		Scheme:                           s,
		OperatorNamespace:                suiteOperatorNamespace,
		Secrets:                          secrets.NewByNameReader(c),
		MoverImage:                       suiteMoverImage,
		ManifestMoverServiceAccount:      suiteManifestMoverSA,
		ClusterManifestReaderClusterRole: suiteClusterManifestReaderRole,
		Recorder:                         events.NewFakeRecorder(128),
	}, c
}

// TestClusterBackupCaptureFailureStillPersistsTheRunAggregate is the sweep's ClusterBackup half.
//
// Each case arms ONE refusal, drives one pass, and then asserts the two halves of the fix together:
// the run's accounting REACHED THE STORE (re-read, never inspected in the reconciler's copy), and the
// error was still propagated so controller-runtime backs the run off and reconcile_errors_total counts
// a failure that deserves counting.
//
// Mutations that must turn this test red: restoring `return ctrl.Result{}, err` at either site in
// step (3.5) (the whole aggregate is discarded — every counter reads zero and the phase stays empty);
// and setting captureDone TRUE on the resolve-error path (the run goes terminal over a capture that
// never happened — 1 succeeded + 1 failed rolls up to PartiallyFailed — after which the
// already-terminal guard at the top of Reconcile freezes it forever, which the non-terminal assertion
// below catches). The equivalent forcing on the advance path is belt-and-braces rather than
// load-bearing today: advanceClusterManifests answers done=false on every error path it has, so no
// mutation of that one line alone changes behaviour. It is there for the error path somebody adds
// later, and its own comment says so.
func TestClusterBackupCaptureFailureStillPersistsTheRunAggregate(t *testing.T) {
	refusedRepoRead := apierrors.NewForbidden(
		schema.GroupResource{Group: "crystalbackup.io", Resource: "backuprepositories"}, erroredPassLoc,
		errRefusedByTest)
	refusedJobCreate := apierrors.NewForbidden(
		schema.GroupResource{Group: "batch", Resource: "jobs"}, clusterManifestsJobName(erroredPassRun),
		errRefusedByTest)

	cases := []struct {
		name string
		// funcs is the refusal, and each stands in for a named real cause.
		funcs interceptor.Funcs
		// wantErr is a substring of the error the pass must STILL return: persisting is not the same
		// as swallowing, and a capture that silently stopped being retried would be a worse defect
		// than the one being fixed.
		wantErr string
	}{
		{
			// Instance 1: the shared repository behind the capture cannot be read. Every child gates on
			// the same repository, so this is a whole-run fault — and it is upstream of any Job, so the
			// old code returned before the run had recorded anything at all.
			name:    "the repository behind the capture cannot be read",
			wantErr: "get BackupRepository",
			funcs: interceptor.Funcs{
				Get: func(ctx context.Context, c crclient.WithWatch, key crclient.ObjectKey, obj crclient.Object, opts ...crclient.GetOption) error {
					if _, isRepo := obj.(*cbv1.BackupRepository); isRepo {
						return refusedRepoRead
					}
					return c.Get(ctx, key, obj, opts...)
				},
			},
		},
		{
			// Instance 2: the capture Job cannot be created. The shape of a validating webhook, a
			// narrowed batch RBAC, or a ResourceQuota — durable causes, which is what makes the
			// discarded aggregate permanent rather than a one-pass blip.
			name:    "the cluster-manifests capture Job cannot be created",
			wantErr: "create cluster-manifest Job",
			funcs: interceptor.Funcs{
				Create: func(ctx context.Context, c crclient.WithWatch, obj crclient.Object, opts ...crclient.CreateOption) error {
					if job, isJob := obj.(*batchv1.Job); isJob &&
						job.Name == clusterManifestsJobName(erroredPassRun) {
						return refusedJobCreate
					}
					return c.Create(ctx, obj, opts...)
				},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			r, c := newErroredPassReconciler(t, tc.funcs)

			_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: erroredPassRun}})
			if err == nil {
				t.Fatalf("Reconcile returned no error: the capture failure must still be propagated, or " +
					"the run stops being backed off and the failure stops being counted")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("Reconcile error = %v, want it to name the refused call (%q)", err, tc.wantErr)
			}

			// THE ASSERTION AN ERRORED PASS COULD NEVER SATISFY: the run's accounting, re-read from the
			// store rather than from the object the reconciler mutated.
			var run cbv1.ClusterBackup
			if err := c.Get(ctx, crclient.ObjectKey{Name: erroredPassRun}, &run); err != nil {
				t.Fatalf("get run: %v", err)
			}
			st := run.Status
			if st.Phase == "" {
				t.Fatalf("status.phase is empty: the pass returned before its single status write, so " +
					"NOTHING it computed was persisted — not the namespaces it matched, not the children " +
					"that had already finished, not the failures the fan-out recorded")
			}
			if st.NamespacesMatched != 2 {
				t.Errorf("namespacesMatched = %d, want 2 — the selector's own answer must survive an "+
					"unrelated half's bad day", st.NamespacesMatched)
			}
			if st.NamespacesSucceeded != 1 {
				t.Errorf("namespacesSucceeded = %d, want 1: a namespace whose backup genuinely completed "+
					"must be counted, or the run under-reports the protection it actually achieved",
					st.NamespacesSucceeded)
			}
			if st.NamespacesFailed != 1 {
				t.Errorf("namespacesFailed = %d, want 1", st.NamespacesFailed)
			}
			if len(st.Failures) != 1 || st.Failures[0].Namespace != "erp-ns-b" {
				t.Errorf("status.failures = %+v, want the failed namespace named: the failure list is the "+
					"only place an administrator learns WHICH namespace is unprotected", st.Failures)
			}

			// And the run is NOT terminal, because the capture's state could not be established. A
			// terminal phase here would trip the already-terminal guard at the top of Reconcile and no
			// later pass would ever record the capture — an orphaned kind=cluster-manifests snapshot, or
			// none at all, with the run claiming to be finished either way.
			if isTerminalClusterBackupPhase(st.Phase) {
				t.Errorf("phase = %q, want a non-terminal phase: the cluster-scoped capture never settled, "+
					"and a run that goes terminal over it can never come back to record it", st.Phase)
			}
			if st.Phase != string(status.ClusterBackupPhaseRunning) {
				t.Errorf("phase = %q, want Running", st.Phase)
			}
		})
	}
}

// TestClusterBackupCaptureIsStillRetriedAfterPersisting is the other half of "persist first, then
// propagate", and a status assertion cannot see it: that the capture is genuinely RETRIED rather than
// quietly abandoned once its error stopped ending the pass early.
//
// Mutation that must turn this red: making step (3.5) swallow captureErr (return nil) — the pass then
// persists and the capture is still retried on the poll, but the run is no longer backed off and
// nothing counts the failure, which is how a broken capture ships invisible. The count assertion
// below stays green under that mutation; the error assertion in the sibling test is what catches it.
// Both are needed, which is why they are separate tests.
func TestClusterBackupCaptureIsStillRetriedAfterPersisting(t *testing.T) {
	ctx := context.Background()
	attempts := 0
	r, c := newErroredPassReconciler(t, interceptor.Funcs{
		Create: func(ctx context.Context, cl crclient.WithWatch, obj crclient.Object, opts ...crclient.CreateOption) error {
			if job, isJob := obj.(*batchv1.Job); isJob && job.Name == clusterManifestsJobName(erroredPassRun) {
				attempts++
				return apierrors.NewForbidden(schema.GroupResource{Group: "batch", Resource: "jobs"},
					job.Name, errRefusedByTest)
			}
			return cl.Create(ctx, obj, opts...)
		},
	})

	for pass := 1; pass <= 3; pass++ {
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: erroredPassRun}}); err == nil {
			t.Fatalf("pass %d returned no error", pass)
		}
		if attempts != pass {
			t.Fatalf("after %d passes the capture Job create was attempted %d time(s): a persisted pass "+
				"must not stop the capture from being re-attempted", pass, attempts)
		}
	}

	// Three failing passes and the run's aggregate is still the same durable, honest record — the
	// recompute-from-scratch guarantee holds across them rather than drifting.
	var run cbv1.ClusterBackup
	if err := c.Get(ctx, crclient.ObjectKey{Name: erroredPassRun}, &run); err != nil {
		t.Fatalf("get run: %v", err)
	}
	if run.Status.NamespacesSucceeded != 1 || run.Status.NamespacesFailed != 1 || run.Status.NamespacesMatched != 2 {
		t.Errorf("counters after three failing passes = matched %d, succeeded %d, failed %d; want 2/1/1",
			run.Status.NamespacesMatched, run.Status.NamespacesSucceeded, run.Status.NamespacesFailed)
	}
	if run.Status.AddedBytes != 8192 {
		t.Errorf("addedBytes = %d, want 8192 (both children's volumes counted once)", run.Status.AddedBytes)
	}
}
