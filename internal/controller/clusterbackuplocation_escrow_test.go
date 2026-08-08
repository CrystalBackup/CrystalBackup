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
	"testing"

	"filippo.io/age"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/client/secrets"
	"github.com/CrystalBackup/CrystalBackup/internal/keys"
	"github.com/CrystalBackup/CrystalBackup/internal/mover"
)

// ---------------------------------------------------------------------------------------------
// The wrapped-DEK escrow, which had NO test at all until the incident that produced these.
//
// It is the piece of this operator with the worst failure mode: everything else can lose a
// backup run, and this can lose the ability to read a repository. It is also the piece whose
// correct behaviour is least visible — a mistake here produces a healthy-looking location and
// a `wrong password or no key found` from restic hours later.
//
// The table below covers every outcome the reconcile can reach. The test AFTER it is the one
// that matters most: it asserts the INVARIANT rather than the cases, so a branch added later
// with the wrong return value fails even though nobody thought to add a row for it.
// ---------------------------------------------------------------------------------------------

const escrowTestNS = "crystal-backup-system"

// escrowStub is an EscrowStore whose two operations are whatever the test wants them to be.
type escrowStub struct {
	fetchBytes []byte
	fetchFound bool
	fetchErr   error
	putErr     error
	puts       int
}

func (s *escrowStub) Fetch(context.Context, string, string) ([]byte, bool, error) {
	return s.fetchBytes, s.fetchFound, s.fetchErr
}

func (s *escrowStub) Put(context.Context, string, string, []byte) error {
	s.puts++
	return s.putErr
}

// newKEK returns a usable age identity string and its wrapper.
func newKEK(t *testing.T) (string, keys.Wrapper) {
	t.Helper()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("generate age identity: %v", err)
	}
	w, err := keys.NewAgeWrapper(id.String())
	if err != nil {
		t.Fatalf("new age wrapper: %v", err)
	}
	return id.String(), w
}

// escrowFixture assembles a reconciler over a fake client, with whichever Secrets the case wants.
type escrowFixture struct {
	r     *ClusterBackupLocationReconciler
	loc   *cbv1.ClusterBackupLocation
	store *escrowStub
}

type escrowOpts struct {
	noCredentials bool
	noKEK         bool
	brokenKEK     bool
	factoryErr    bool
	// localDEK, when non-nil, seeds the in-cluster wrapped DEK Secret with these bytes.
	localDEK []byte
	store    *escrowStub
}

// escrowScheme carries core/v1 (the Secrets) and the project's own API group (the location).
func escrowScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	if err := cbv1.AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme cbv1: %v", err)
	}
	return s
}

func newEscrowFixture(t *testing.T, kekIdentity string, o escrowOpts) *escrowFixture {
	t.Helper()

	loc := &cbv1.ClusterBackupLocation{
		ObjectMeta: metav1.ObjectMeta{Name: "loc-1", Generation: 1},
		Spec: cbv1.ClusterBackupLocationSpec{
			ClusterID: "prod-eu-1",
			S3: cbv1.S3Spec{
				Endpoint:             "https://s3.example.com",
				Bucket:               "backups",
				Prefix:               "dr",
				CredentialsSecretRef: cbv1.LocalObjectReference{Name: "s3-creds"},
			},
			Encryption: cbv1.ClusterEncryptionSpec{
				ClusterKEKSecretRef: cbv1.LocalObjectReference{Name: "cluster-kek"},
			},
		},
	}

	objs := []crclient.Object{}
	if !o.noCredentials {
		objs = append(objs, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: escrowTestNS, Name: "s3-creds"},
			Data: map[string][]byte{
				mover.SecretKeyAWSAccessKeyID:     []byte("AK"),
				mover.SecretKeyAWSSecretAccessKey: []byte("SK"),
			},
		})
	}
	if !o.noKEK {
		identity := kekIdentity
		if o.brokenKEK {
			identity = "this-is-not-an-age-identity"
		}
		objs = append(objs, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: escrowTestNS, Name: "cluster-kek"},
			Data:       map[string][]byte{kekIdentityDataKey: []byte(identity)},
		})
	}
	if o.localDEK != nil {
		objs = append(objs, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: escrowTestNS, Name: keys.DEKSecretName(loc.Name)},
			Data:       map[string][]byte{keys.DEKSecretKey: o.localDEK},
		})
	}

	c := fake.NewClientBuilder().WithScheme(escrowScheme(t)).WithObjects(objs...).Build()
	store := o.store
	if store == nil {
		store = &escrowStub{}
	}

	r := &ClusterBackupLocationReconciler{
		Client:            c,
		Secrets:           secrets.NewByNameReader(c),
		OperatorNamespace: escrowTestNS,
		Recorder:          noopRecorder{},
		Escrow: func(cbv1.S3Spec, string, string) (EscrowStore, error) {
			if o.factoryErr {
				return nil, errors.New("cannot build client")
			}
			return store, nil
		},
	}
	return &escrowFixture{r: r, loc: loc, store: store}
}

func escrowReason(t *testing.T, loc *cbv1.ClusterBackupLocation) (metav1.ConditionStatus, string) {
	t.Helper()
	for i := range loc.Status.Conditions {
		if loc.Status.Conditions[i].Type == ConditionDEKEscrowed {
			return loc.Status.Conditions[i].Status, loc.Status.Conditions[i].Reason
		}
	}
	t.Fatalf("no %s condition was set", ConditionDEKEscrowed)
	return "", ""
}

// TestEscrowOutcomes walks every reachable outcome and pins BOTH halves of each: the reason an
// administrator reads, and whether the repository is allowed to be provisioned.
//
// The two are inseparable, which is why they are one table. The 2026-08-07 incident was exactly
// a correct reason (`CredentialsUnavailable`) beside a wrong block decision.
func TestEscrowOutcomes(t *testing.T) {
	kekID, wrapper := newKEK(t)

	goodDEK, err := wrapper.Wrap([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("wrap: %v", err)
	}
	otherDEK, err := wrapper.Wrap([]byte("ffffffffffffffffffffffffffffffff"))
	if err != nil {
		t.Fatalf("wrap other: %v", err)
	}
	// Ciphertext under a DIFFERENT KEK: unwraps under nobody's key here.
	_, foreignWrapper := newKEK(t)
	foreignDEK, err := foreignWrapper.Wrap([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("wrap foreign: %v", err)
	}

	cases := map[string]struct {
		opts       escrowOpts
		wantReason string
		wantBlock  bool
		why        string
	}{
		"no S3 credentials": {
			opts:       escrowOpts{noCredentials: true},
			wantReason: "CredentialsUnavailable",
			wantBlock:  true,
			why: "THE REGRESSION TEST. This returned false, the repository was provisioned, and " +
				"EnsureDEK minted a fresh DEK over an escrow holding the only key to 38 snapshots.",
		},
		"no KEK": {
			opts: escrowOpts{noKEK: true, localDEK: goodDEK}, wantReason: "KEKUnavailable", wantBlock: true,
			why: "without the KEK nothing can be compared or adopted; the question is unanswered",
		},
		"unparseable KEK": {
			opts: escrowOpts{brokenKEK: true, localDEK: goodDEK}, wantReason: "KEKInvalid", wantBlock: true,
		},
		"escrow client will not build": {
			opts: escrowOpts{factoryErr: true, localDEK: goodDEK}, wantReason: "EscrowClientError", wantBlock: true,
		},
		"bucket unreachable, no local DEK": {
			opts:       escrowOpts{store: &escrowStub{fetchErr: errors.New("timeout")}},
			wantReason: "EscrowUnreachable", wantBlock: true,
		},
		"bucket unreachable, local DEK present": {
			opts:       escrowOpts{localDEK: goodDEK, store: &escrowStub{fetchErr: errors.New("timeout")}},
			wantReason: "EscrowUnverifiable", wantBlock: false,
			why: "an in-cluster DEK exists, so EnsureDEK has nothing to mint and nothing can fork. " +
				"This row said `true` in the first cut of 0.6.4, with a justification about the " +
				"bucket possibly holding another generation's key — which is about conflict " +
				"DETECTION and does not license a block. The crucible caught it (m6/alerts went " +
				"red) after this very test had blessed it, which is why the invariant test below " +
				"matters more than this table.",
		},
		"genuinely fresh location": {
			opts:       escrowOpts{store: &escrowStub{fetchFound: false}},
			wantReason: "AwaitingFirstDEK", wantBlock: false,
			why: "no DEK anywhere, and Fetch's contract makes found=false positive knowledge of " +
				"absence — minting is what SHOULD happen",
		},
		"bare-cluster DR bootstrap": {
			opts:       escrowOpts{store: &escrowStub{fetchFound: true, fetchBytes: goodDEK}},
			wantReason: "Recovered", wantBlock: false,
		},
		"steady state, escrow mirrors the cluster": {
			opts:       escrowOpts{localDEK: goodDEK, store: &escrowStub{fetchFound: true, fetchBytes: goodDEK}},
			wantReason: "Escrowed", wantBlock: false,
		},
		"escrow object missing, cluster DEK good": {
			opts:       escrowOpts{localDEK: goodDEK, store: &escrowStub{fetchFound: false}},
			wantReason: "Escrowed", wantBlock: false,
			why: "the escrow is written on this pass; nothing is at risk",
		},
		"escrow write fails": {
			opts:       escrowOpts{localDEK: goodDEK, store: &escrowStub{fetchFound: false, putErr: errors.New("403")}},
			wantReason: "EscrowWriteFailed", wantBlock: false,
			why: "the ONE advisory case: the in-cluster DEK is known-good, backups work, only " +
				"bare-cluster DR is incomplete",
		},
		"bucket object does not open under this KEK": {
			opts:       escrowOpts{localDEK: goodDEK, store: &escrowStub{fetchFound: true, fetchBytes: foreignDEK}},
			wantReason: "EscrowUnreadableUnderKEK", wantBlock: true,
			why: "the most serious case, and it used to share one message with the two below — " +
				"which asserted a key comparison that never happened",
		},
		"cluster DEK does not open under this KEK": {
			opts:       escrowOpts{localDEK: foreignDEK, store: &escrowStub{fetchFound: true, fetchBytes: goodDEK}},
			wantReason: "ClusterDEKUnreadableUnderKEK", wantBlock: true,
			why: "opposite remedy to the case above: here the bucket is the survivor",
		},
		"both open, different keys": {
			opts:       escrowOpts{localDEK: goodDEK, store: &escrowStub{fetchFound: true, fetchBytes: otherDEK}},
			wantReason: "EscrowConflict", wantBlock: true,
			why: "two repository generations; the bucket may hold the only key to the older one",
		},
	}

	for name, tc := range cases {
		f := newEscrowFixture(t, kekID, tc.opts)
		block := f.r.reconcileDEKEscrow(context.Background(), f.loc)
		_, reason := escrowReason(t, f.loc)

		if reason != tc.wantReason {
			t.Errorf("%s: reason = %q, want %q", name, reason, tc.wantReason)
		}
		if block != tc.wantBlock {
			t.Errorf("%s: blockRepository = %v, want %v (%s)", name, block, tc.wantBlock, tc.why)
		}
	}
}

// TestNoEscrowStateLetsAMintThroughUnlessItIsProvablySafe is the guard that survives a refactor.
//
// The table above enumerates the outcomes that exist TODAY. This asserts the rule they follow, so
// that a branch added next year with a copied-and-pasted `return false` fails here even though
// nobody added a row for it. That is the difference that mattered: the original code was a
// correct list of cases whose rule had been inverted in five of them.
//
// The allow-list is deliberately tiny and each entry names why minting is provably safe.
func TestNoEscrowStateLetsAMintThroughUnlessItIsProvablySafe(t *testing.T) {
	mayProceed := map[string]string{
		"Escrowed":         "an in-cluster DEK exists, so EnsureDEK has nothing to mint",
		"Recovered":        "the bucket's DEK was just adopted into the cluster; nothing left to mint",
		"AwaitingFirstDEK": "provably no DEK in the cluster AND none in the bucket; minting is correct",
		"EscrowWriteFailed": "the in-cluster DEK is known-good and unchanged; only the bucket copy is " +
			"behind, which degrades bare-cluster DR and not the backups",
		"EscrowUnverifiable": "the bucket could not be read, but an in-cluster DEK is present: " +
			"EnsureDEK has nothing to mint, and Put is never reached, so a bucket this pass could " +
			"not read cannot be overwritten by it. Distinct from EscrowUnreachable, which is the " +
			"same failure with NO local DEK and therefore blocks",
	}

	kekID, wrapper := newKEK(t)
	goodDEK, err := wrapper.Wrap([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("wrap: %v", err)
	}
	_, foreignWrapper := newKEK(t)
	foreignDEK, _ := foreignWrapper.Wrap([]byte("0123456789abcdef0123456789abcdef"))
	otherDEK, _ := wrapper.Wrap([]byte("ffffffffffffffffffffffffffffffff"))

	// Every shape of input this function can be handed. The point is coverage of REACHED
	// branches, not of inputs: the assertion is on whatever reason each one lands on.
	inputs := []escrowOpts{
		{noCredentials: true},
		{noKEK: true, localDEK: goodDEK},
		{brokenKEK: true, localDEK: goodDEK},
		{factoryErr: true, localDEK: goodDEK},
		{store: &escrowStub{fetchErr: errors.New("x")}},
		{localDEK: goodDEK, store: &escrowStub{fetchErr: errors.New("x")}},
		{store: &escrowStub{}},
		{store: &escrowStub{fetchFound: true, fetchBytes: goodDEK}},
		{localDEK: goodDEK, store: &escrowStub{fetchFound: true, fetchBytes: goodDEK}},
		{localDEK: goodDEK, store: &escrowStub{}},
		{localDEK: goodDEK, store: &escrowStub{putErr: errors.New("403")}},
		{localDEK: goodDEK, store: &escrowStub{fetchFound: true, fetchBytes: foreignDEK}},
		{localDEK: foreignDEK, store: &escrowStub{fetchFound: true, fetchBytes: goodDEK}},
		{localDEK: goodDEK, store: &escrowStub{fetchFound: true, fetchBytes: otherDEK}},
	}

	seen := map[string]bool{}
	for i, o := range inputs {
		f := newEscrowFixture(t, kekID, o)
		block := f.r.reconcileDEKEscrow(context.Background(), f.loc)
		_, reason := escrowReason(t, f.loc)
		seen[reason] = true

		why, safe := mayProceed[reason]
		switch {
		case safe && block:
			t.Errorf("input %d: reason %q blocks the repository, but it is a provably-safe state (%s) — "+
				"blocking it would wedge a legitimate install", i, reason, why)
		case !safe && !block:
			t.Errorf("input %d: reason %q does NOT block the repository, and it is not in the "+
				"provably-safe list. Either it belongs there with an argument for why minting a DEK "+
				"cannot fork the repository, or it must block. This is the exact shape of the "+
				"2026-08-07 incident.", i, reason)
		}
	}

	// A guard that reached three branches proves little. Eight distinct reasons is the current
	// count; if this collapses, the fixture stopped exercising the function.
	if len(seen) < 8 {
		t.Fatalf("only %d distinct reasons reached (%v) — the inputs stopped covering the branches", len(seen), seen)
	}
}

// TestEscrowNotWiredNeverBlocks pins the envtest/no-S3 path. A nil factory means "there is no
// escrow in this deployment", which must not be confused with "the escrow question is
// unresolved" — otherwise every location on a cluster without S3 wiring would sit Degraded.
func TestEscrowNotWiredNeverBlocks(t *testing.T) {
	kekID, _ := newKEK(t)
	f := newEscrowFixture(t, kekID, escrowOpts{})
	f.r.Escrow = nil

	if block := f.r.reconcileDEKEscrow(context.Background(), f.loc); block {
		t.Error("a location on a deployment with no escrow wired must not block its repository")
	}
	for i := range f.loc.Status.Conditions {
		if f.loc.Status.Conditions[i].Type == ConditionDEKEscrowed {
			t.Errorf("no escrow wired must leave NO %s condition, got reason %q",
				ConditionDEKEscrowed, f.loc.Status.Conditions[i].Reason)
		}
	}
}
