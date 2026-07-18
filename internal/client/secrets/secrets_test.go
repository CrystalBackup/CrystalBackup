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

package secrets

import (
	"bytes"
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const (
	testNamespace = "crystal-backup-system"
	testName      = "dr-s3-credentials"
	testKey       = "token"
)

var testValue = []byte("s3cr3t-token-bytes")

// newReader builds a ByNameReader over a fake client seeded with a single Secret
// {testNamespace/testName, data{testKey: testValue}}. The scheme is built
// explicitly with corev1 registered rather than relying on the fake builder's
// default global scheme, so the test states exactly which types it depends on. The
// fake client is non-caching, mirroring the uncached API reader ByNameReader must be
// wired with in production.
func newReader(t *testing.T) *ByNameReader {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: testName},
		Data:       map[string][]byte{testKey: testValue},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()
	return NewByNameReader(c)
}

// TestGetReturnsSecret pins the happy path of Get: the seeded Secret is returned
// with its identity and data intact. This is the primitive GetValue and every
// caller build on.
func TestGetReturnsSecret(t *testing.T) {
	b := newReader(t)

	got, err := b.Get(context.Background(), testNamespace, testName)
	if err != nil {
		t.Fatalf("Get: unexpected error: %v", err)
	}
	if got.Namespace != testNamespace || got.Name != testName {
		t.Errorf("Get returned %s/%s, want %s/%s", got.Namespace, got.Name, testNamespace, testName)
	}
	if !bytes.Equal(got.Data[testKey], testValue) {
		t.Errorf("Get data[%q] = %q, want %q", testKey, got.Data[testKey], testValue)
	}
}

// TestGetValueReturnsBytes proves the convenience path returns exactly the bytes
// stored under the key — no truncation, decoding or copying artefact. Secret bytes
// are crypto material (a wrapped DEK, an S3 secret key), so an exact-bytes assertion
// is the point.
func TestGetValueReturnsBytes(t *testing.T) {
	b := newReader(t)

	got, err := b.GetValue(context.Background(), testNamespace, testName, testKey)
	if err != nil {
		t.Fatalf("GetValue: unexpected error: %v", err)
	}
	if !bytes.Equal(got, testValue) {
		t.Errorf("GetValue = %q, want %q", got, testValue)
	}
}

// TestGetMissingSecretIsNotFound checks the failure identity a caller relies on to
// treat an absent Secret as a transient, requeue-able condition: the error from Get
// must satisfy apierrors.IsNotFound. It also asserts the returned pointer is nil so
// a caller cannot accidentally dereference it on the error path.
func TestGetMissingSecretIsNotFound(t *testing.T) {
	b := newReader(t)

	got, err := b.Get(context.Background(), testNamespace, "does-not-exist")
	if got != nil {
		t.Errorf("Get returned non-nil Secret %v on error", got)
	}
	if !apierrors.IsNotFound(err) {
		t.Fatalf("Get error = %v, want apierrors.IsNotFound", err)
	}
}

// TestGetValueMissingSecretIsNotFound checks GetValue passes Get's not-found error
// through untouched: a missing Secret must still surface as apierrors.IsNotFound and
// must NOT be mistaken for the missing-key case (errors.Is ErrKeyNotFound must be
// false). This is the disambiguation the two sentinels exist to provide.
func TestGetValueMissingSecretIsNotFound(t *testing.T) {
	b := newReader(t)

	got, err := b.GetValue(context.Background(), testNamespace, "does-not-exist", testKey)
	if got != nil {
		t.Errorf("GetValue returned non-nil bytes %q on error", got)
	}
	if !apierrors.IsNotFound(err) {
		t.Fatalf("GetValue error = %v, want apierrors.IsNotFound", err)
	}
	if errors.Is(err, ErrKeyNotFound) {
		t.Errorf("GetValue error also matched ErrKeyNotFound; the two cases must stay distinct")
	}
}

// TestGetValueMissingKey checks the other failure identity: the Secret exists but
// lacks the key. The error must satisfy errors.Is(err, ErrKeyNotFound) and must NOT
// be a not-found (the object is present), so callers can branch on "misconfigured
// Secret" versus "Secret not created yet".
func TestGetValueMissingKey(t *testing.T) {
	b := newReader(t)

	got, err := b.GetValue(context.Background(), testNamespace, testName, "absent-key")
	if got != nil {
		t.Errorf("GetValue returned non-nil bytes %q on error", got)
	}
	if !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("GetValue error = %v, want errors.Is ErrKeyNotFound", err)
	}
	if apierrors.IsNotFound(err) {
		t.Errorf("GetValue key-missing error matched apierrors.IsNotFound; the two cases must stay distinct")
	}
}
