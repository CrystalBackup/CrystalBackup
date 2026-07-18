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

package keys_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"testing"

	"filippo.io/age"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/CrystalBackup/CrystalBackup/internal/keys"
)

// newAgeWrapper builds an AgeWrapper backed by a freshly generated X25519 identity. Each
// call yields an independent KEK, which the cross-identity tests rely on.
func newAgeWrapper(t *testing.T) (*keys.AgeWrapper, *age.X25519Identity) {
	t.Helper()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("generate age identity: %v", err)
	}
	w, err := keys.NewAgeWrapper(id.String())
	if err != nil {
		t.Fatalf("NewAgeWrapper: %v", err)
	}
	return w, id
}

// newFakeClient builds a controller-runtime fake client with only corev1 registered — the
// scheme is assembled by hand rather than via client-go's aggregate scheme so the test
// depends on nothing beyond the DEK Secret's own type.
func newFakeClient(t *testing.T) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("register corev1 with scheme: %v", err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).Build()
}

// TestAgeWrapperRoundTrip is the core confidentiality contract: Wrap then Unwrap round-trips
// the exact bytes, the ciphertext does not leak the plaintext, and a DIFFERENT KEK cannot
// unwrap it (fail-closed on a wrong key).
func TestAgeWrapperRoundTrip(t *testing.T) {
	w, _ := newAgeWrapper(t)

	dek := make([]byte, 32)
	if _, err := rand.Read(dek); err != nil {
		t.Fatalf("read random DEK: %v", err)
	}

	ct, err := w.Wrap(dek)
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	if bytes.Contains(ct, dek) {
		t.Fatal("ciphertext contains the plaintext DEK — encryption did not happen")
	}

	pt, err := w.Unwrap(ct)
	if err != nil {
		t.Fatalf("Unwrap: %v", err)
	}
	if !bytes.Equal(pt, dek) {
		t.Fatalf("round-trip mismatch: got %x, want %x", pt, dek)
	}

	// A second, independent KEK must not be able to unwrap the first KEK's ciphertext.
	other, _ := newAgeWrapper(t)
	if _, err := other.Unwrap(ct); err == nil {
		t.Fatal("Unwrap with a different identity succeeded; want an error")
	}
}

// TestNewAgeWrapperTrimsWhitespace proves the constructor tolerates the trailing newline a
// KEK read from a Secret or file routinely carries.
func TestNewAgeWrapperTrimsWhitespace(t *testing.T) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("generate age identity: %v", err)
	}
	padded := "\n  " + id.String() + "  \n"
	if _, err := keys.NewAgeWrapper(padded); err != nil {
		t.Fatalf("NewAgeWrapper should trim surrounding whitespace, got: %v", err)
	}
}

// TestNewAgeWrapperRejectsBadInput asserts empty, whitespace-only and malformed identities
// are refused rather than deferred to a later, more confusing failure.
func TestNewAgeWrapperRejectsBadInput(t *testing.T) {
	for _, in := range []string{
		"",
		"   \n\t  ",
		"not-an-age-secret-key",
		"AGE-SECRET-KEY-1-garbage",
	} {
		if _, err := keys.NewAgeWrapper(in); err == nil {
			t.Errorf("NewAgeWrapper(%q) = nil error; want an error", in)
		}
	}
}

// TestGenerateDEK checks the shape of a DEK: non-empty, decodes to exactly 32 bytes of
// entropy, and is unique across calls.
func TestGenerateDEK(t *testing.T) {
	a, err := keys.GenerateDEK()
	if err != nil {
		t.Fatalf("GenerateDEK: %v", err)
	}
	if a == "" {
		t.Fatal("GenerateDEK returned an empty string")
	}
	raw, err := base64.StdEncoding.DecodeString(a)
	if err != nil {
		t.Fatalf("DEK is not valid base64: %v", err)
	}
	if len(raw) != 32 {
		t.Fatalf("DEK decodes to %d bytes; want 32", len(raw))
	}

	b, err := keys.GenerateDEK()
	if err != nil {
		t.Fatalf("GenerateDEK (second): %v", err)
	}
	if a == b {
		t.Fatal("two GenerateDEK calls returned the same value")
	}
}

// TestEnsureDEKCreatesThenReuses exercises the whole wrap→store→get→unwrap path with a real
// AgeWrapper: the first call creates the Secret in the operator namespace and returns a DEK,
// the persisted data["dek"] is the WRAPPED form (never the plaintext), and a second call
// returns the SAME DEK from the stored ciphertext.
func TestEnsureDEKCreatesThenReuses(t *testing.T) {
	const (
		ns  = "crystal-backup-system"
		loc = "dr-eu-1"
	)
	c := newFakeClient(t)
	w, _ := newAgeWrapper(t)
	m := keys.NewDEKManager(c, w, ns)
	ctx := context.Background()

	dek1, err := m.EnsureDEK(ctx, loc)
	if err != nil {
		t.Fatalf("first EnsureDEK: %v", err)
	}
	if dek1 == "" {
		t.Fatal("first EnsureDEK returned an empty DEK")
	}

	// The Secret must exist in the operator namespace under the deterministic name.
	var secret corev1.Secret
	name := keys.DEKSecretName(loc)
	if name != "crystal-dek-"+loc {
		t.Fatalf("DEKSecretName(%q) = %q; want %q", loc, name, "crystal-dek-"+loc)
	}
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &secret); err != nil {
		t.Fatalf("get persisted DEK secret: %v", err)
	}

	// At rest, the only representation is the WRAPPED ciphertext: not equal to and not
	// containing the plaintext DEK.
	stored := secret.Data[keys.DEKSecretKey]
	if len(stored) == 0 {
		t.Fatalf("persisted secret has no %q data", keys.DEKSecretKey)
	}
	if bytes.Equal(stored, []byte(dek1)) {
		t.Fatal("DEK is stored in cleartext — data[\"dek\"] equals the returned DEK")
	}
	if bytes.Contains(stored, []byte(dek1)) {
		t.Fatal("persisted blob contains the plaintext DEK")
	}
	if secret.Type != corev1.SecretTypeOpaque {
		t.Errorf("secret type = %q; want %q", secret.Type, corev1.SecretTypeOpaque)
	}
	if got := secret.Labels["app.kubernetes.io/managed-by"]; got != "crystal-backup" {
		t.Errorf("managed-by label = %q; want %q", got, "crystal-backup")
	}
	if got := secret.Labels["app.kubernetes.io/name"]; got != "crystal-backup" {
		t.Errorf("name label = %q; want %q", got, "crystal-backup")
	}

	// Idempotency: a second EnsureDEK reads the stored ciphertext back and unwraps it to
	// the very same DEK — the repository keeps its one password for life.
	dek2, err := m.EnsureDEK(ctx, loc)
	if err != nil {
		t.Fatalf("second EnsureDEK: %v", err)
	}
	if dek2 != dek1 {
		t.Fatalf("EnsureDEK is not idempotent: %q then %q", dek1, dek2)
	}
}

// TestEnsureDEKFailsClosedOnUndecryptableSecret proves the safety property that a present
// but unreadable DEK Secret is a hard error: EnsureDEK must NOT silently mint a new DEK over
// it, which would orphan the repository whose real password is the unreadable blob.
func TestEnsureDEKFailsClosedOnUndecryptableSecret(t *testing.T) {
	const (
		ns  = "crystal-backup-system"
		loc = "corrupt"
	)
	c := newFakeClient(t)
	w, _ := newAgeWrapper(t)
	ctx := context.Background()

	// Plant a Secret whose dek is not valid age ciphertext (e.g. wrapped under a lost KEK).
	planted := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: keys.DEKSecretName(loc), Namespace: ns},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{keys.DEKSecretKey: []byte("this is not an age ciphertext")},
	}
	if err := c.Create(ctx, planted); err != nil {
		t.Fatalf("plant corrupt secret: %v", err)
	}

	m := keys.NewDEKManager(c, w, ns)
	if _, err := m.EnsureDEK(ctx, loc); err == nil {
		t.Fatal("EnsureDEK succeeded over an undecryptable DEK secret; want a fail-closed error")
	}
}
