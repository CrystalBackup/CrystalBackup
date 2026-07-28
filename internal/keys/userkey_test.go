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
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/CrystalBackup/CrystalBackup/internal/keys"
)

const (
	tenantNS   = "c-team-x"
	locName    = "my-offsite"
	userSecret = "offsite-key"
)

// putPasswordSecret creates a password Secret with the given data in the fake client.
func putPasswordSecret(t *testing.T, c client.Client, namespace, name string, data map[string][]byte) {
	t.Helper()
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Type:       corev1.SecretTypeOpaque,
		Data:       data,
	}
	if err := c.Create(context.Background(), s); err != nil {
		t.Fatalf("create secret %s/%s: %v", namespace, name, err)
	}
}

func TestEnsureUserPasswordReadsTheUserSuppliedSecret(t *testing.T) {
	c := newFakeClient(t)
	putPasswordSecret(t, c, tenantNS, userSecret, map[string][]byte{keys.UserPasswordSecretKey: []byte("hunter2")})

	got, err := keys.NewUserKeyManager(c).EnsureUserPassword(context.Background(), tenantNS, locName, userSecret)
	if err != nil {
		t.Fatalf("EnsureUserPassword: %v", err)
	}
	if got != "hunter2" {
		t.Fatalf("password = %q, want the user's own value", got)
	}

	// The user's Secret is read, never shadowed: no generated Secret appears beside it. A
	// generated one would mean the next backup could be written under a key the user's own
	// Secret cannot open.
	var generated corev1.Secret
	err = c.Get(context.Background(),
		client.ObjectKey{Namespace: tenantNS, Name: keys.UserPasswordSecretName(locName)}, &generated)
	if err == nil {
		t.Fatal("a generated password Secret was created even though the user supplied one")
	}
}

func TestEnsureUserPasswordFailsClosedOnAMissingReference(t *testing.T) {
	c := newFakeClient(t)

	// The reference names a Secret that does not exist. Generating one here would be the worst
	// possible recovery: the location would come up healthy under a password the user has never
	// seen, and their own Secret — once created — would not open the repository.
	_, err := keys.NewUserKeyManager(c).EnsureUserPassword(context.Background(), tenantNS, locName, userSecret)
	if err == nil {
		t.Fatal("EnsureUserPassword succeeded with a dangling repositoryPasswordSecretRef")
	}
	var generated corev1.Secret
	if getErr := c.Get(context.Background(),
		client.ObjectKey{Namespace: tenantNS, Name: keys.UserPasswordSecretName(locName)}, &generated); getErr == nil {
		t.Fatal("a password Secret was generated as a fallback for a dangling reference")
	}
}

func TestEnsureUserPasswordRejectsAnEmptyPassword(t *testing.T) {
	c := newFakeClient(t)
	// Present, well-named, and empty. restic would accept it (an empty password is
	// --insecure-no-password territory), which is precisely why this must not pass: the
	// repository's contents would be readable by anyone who can reach the bucket.
	putPasswordSecret(t, c, tenantNS, userSecret, map[string][]byte{keys.UserPasswordSecretKey: {}})

	if _, err := keys.NewUserKeyManager(c).EnsureUserPassword(context.Background(), tenantNS, locName, userSecret); err == nil {
		t.Fatal("EnsureUserPassword accepted an empty password")
	}
}

func TestEnsureUserPasswordNamesTheMissingDataKey(t *testing.T) {
	c := newFakeClient(t)
	putPasswordSecret(t, c, tenantNS, userSecret, map[string][]byte{"password": []byte("wrong-key")})

	_, err := keys.NewUserKeyManager(c).EnsureUserPassword(context.Background(), tenantNS, locName, userSecret)
	if err == nil {
		t.Fatal("EnsureUserPassword accepted a Secret without the expected data key")
	}
	// The single most likely user mistake is picking their own data key name, so the error has
	// to say which one is expected rather than merely that something is missing.
	if !strings.Contains(err.Error(), keys.UserPasswordSecretKey) {
		t.Fatalf("error %q does not name the expected data key %q", err, keys.UserPasswordSecretKey)
	}
}

func TestEnsureUserPasswordGeneratesOnceAndReusesForever(t *testing.T) {
	c := newFakeClient(t)
	m := keys.NewUserKeyManager(c)

	first, err := m.EnsureUserPassword(context.Background(), tenantNS, locName, "")
	if err != nil {
		t.Fatalf("EnsureUserPassword (mint): %v", err)
	}
	if first == "" {
		t.Fatal("generated password is empty")
	}

	// It landed in the USER's namespace, not the operator's — their key, their reversibility.
	var s corev1.Secret
	if err := c.Get(context.Background(),
		client.ObjectKey{Namespace: tenantNS, Name: keys.UserPasswordSecretName(locName)}, &s); err != nil {
		t.Fatalf("generated Secret not found in the tenant namespace: %v", err)
	}
	if string(s.Data[keys.UserPasswordSecretKey]) != first {
		t.Fatal("the persisted Secret does not hold the returned password")
	}
	// And it is PLAINTEXT, deliberately: the user must be able to feed it straight to upstream
	// restic. A wrapped blob here would make the operator a mandatory intermediary (R8).
	if len(s.OwnerReferences) != 0 {
		t.Fatal("the password Secret carries an ownerReference; deleting the BackupLocation " +
			"would garbage-collect the only key that opens the user's repository")
	}

	second, err := m.EnsureUserPassword(context.Background(), tenantNS, locName, "")
	if err != nil {
		t.Fatalf("EnsureUserPassword (reuse): %v", err)
	}
	if second != first {
		t.Fatal("a second call minted a NEW password; every prior snapshot would be orphaned")
	}
}

func TestEnsureUserPasswordConfinesGenerationToTheGivenNamespace(t *testing.T) {
	c := newFakeClient(t)
	m := keys.NewUserKeyManager(c)

	// Two tenants, same location name. Each must get its own key in its own namespace: a shared
	// one would let either tenant open the other's repository.
	a, err := m.EnsureUserPassword(context.Background(), "c-team-a", locName, "")
	if err != nil {
		t.Fatalf("EnsureUserPassword (team-a): %v", err)
	}
	b, err := m.EnsureUserPassword(context.Background(), "c-team-b", locName, "")
	if err != nil {
		t.Fatalf("EnsureUserPassword (team-b): %v", err)
	}
	if a == b {
		t.Fatal("two namespaces with the same location name share a repository password")
	}
}
