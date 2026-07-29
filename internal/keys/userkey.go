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

package keys

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The namespace plane's key model (adr/0004 §2), and why it is NOT the DEK model in dek.go.
//
// A cluster repository is protected by a platform DEK: minted by the operator, wrapped under
// the cluster KEK, stored as ciphertext in crystal-backup-system, and never shown to anyone.
// A namespaced BackupLocation is the opposite on every axis. Its repository is protected by
// the USER'S OWN restic password; it lives as PLAINTEXT in a Secret in THEIR namespace; and
// that is the point, not a weakening — R8 reversibility says a namespace user must be able to
// read their own backups with upstream restic given nothing but S3 credentials and this
// Secret. Wrapping it under a platform KEK would make the operator a mandatory intermediary
// for the user's own data, which is exactly the dependency the namespace plane exists to avoid.
//
// There is therefore no Wrapper here, and no crystal-backup-system involvement at all: the
// operator reads the password by name to run the user's own movers, the same way it reads
// their S3 credentials by name (03-security-and-tenancy.md §4).

// userPasswordSecretNamePrefix is prepended to a location name to form the name of the
// password Secret the operator GENERATES when a BackupLocation omits
// spec.encryption.repositoryPasswordSecretRef. Fixed and human-recognisable so a user can find
// their own key with `kubectl get secret -n <ns> | grep crystal-repo-password-` — a key they
// cannot find is a key they cannot use for the upstream-restic exit path.
const userPasswordSecretNamePrefix = "crystal-repo-password-"

// UserPasswordSecretKey is the data key holding the restic repository password inside a
// namespace-plane password Secret — both the ones the operator generates and the ones a user
// supplies via repositoryPasswordSecretRef.
//
// It is the SAME string the mover projects the password to (mover.ResticPasswordFileName), and
// that is deliberate: the system should have exactly one name for "a restic repository
// password", so a user who copies the convention from anywhere in the docs or from a mover
// Secret is right. The same principle already governs user-supplied S3 credential Secrets,
// which use the AWS_* names the mover itself consumes. A test in the controller package pins
// the two constants together.
const UserPasswordSecretKey = "restic-password"

// UserPasswordSecretName returns the name of the Secret the operator generates to hold the
// repository password for a namespaced BackupLocation that did not supply one. The mapping is
// one Secret per location, named deterministically from the location name, so any reconcile
// can find it without a lookup.
func UserPasswordSecretName(locationName string) string {
	return userPasswordSecretNamePrefix + locationName
}

// UserKeyManager resolves the restic repository password for a namespaced BackupLocation. It
// is the namespace-plane counterpart of DEKManager, minus the envelope: it reads a
// user-supplied Secret, or mints-once-and-reuses-forever a generated one in the user's own
// namespace.
type UserKeyManager struct {
	// client reads and creates password Secrets in TENANT namespaces. Unlike DEKManager it is
	// not pinned to one namespace — the namespace is an argument, and it is always the
	// location's own (structural confinement: a BackupLocation may only reference Secrets
	// beside it).
	client client.Client
}

// NewUserKeyManager builds a UserKeyManager over c.
func NewUserKeyManager(c client.Client) *UserKeyManager {
	return &UserKeyManager{client: c}
}

// EnsureUserPassword returns the plaintext restic password protecting the repository behind the
// BackupLocation named locationName in namespace.
//
// refName is spec.encryption.repositoryPasswordSecretRef.name:
//
//   - NON-EMPTY — the user supplied their own Secret. It is read, never created and never
//     mutated. A missing Secret or a missing data key is a hard error, never a silent fallback
//     to generation: generating a password over a reference the user believes is in force would
//     write their next backup under a key their own Secret cannot open.
//   - EMPTY — the operator generates one and stores it in the user's namespace, minting once
//     and reusing forever (adr/0004 §2). The create race is resolved the same way DEKManager
//     resolves it: the loser re-reads the winner rather than returning a password that was
//     never persisted.
//
// Neither branch ever logs or embeds the password: failures name the Secret only.
func (m *UserKeyManager) EnsureUserPassword(ctx context.Context, namespace, locationName, refName string) (string, error) {
	if refName != "" {
		return m.readPassword(ctx, namespace, refName)
	}

	name := UserPasswordSecretName(locationName)
	key := client.ObjectKey{Namespace: namespace, Name: name}

	// Fast path: already generated on a previous pass. Dominates after the first reconcile.
	var existing corev1.Secret
	err := m.client.Get(ctx, key, &existing)
	switch {
	case err == nil:
		return passwordFrom(&existing, namespace, name)
	case apierrors.IsNotFound(err):
		// First time for this location: fall through and mint one.
	default:
		return "", fmt.Errorf("keys: get repository password secret %s/%s: %w", namespace, name, err)
	}

	// GenerateDEK is the shared generator, not a DEK-specific one: both planes need the same
	// thing — 256 bits of entropy, base64 so the string survives a RESTIC_PASSWORD_FILE
	// round-trip as printable ASCII with no newline.
	password, err := GenerateDEK()
	if err != nil {
		return "", err
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				labelManagedByKey: labelAppValue,
				labelNameKey:      labelAppValue,
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{UserPasswordSecretKey: []byte(password)},
	}

	// Deliberately NO ownerReference to the BackupLocation. This Secret is the only key that
	// can open the user's repository, so garbage-collecting it when the CR is deleted would
	// destroy every snapshot in that bucket as a side effect of a `kubectl delete
	// backuplocation` — the same stickiness rule adr/0009 states for the platform DEK, and it
	// matters more here because the data is the user's own and the platform holds no second
	// slot unless they asked for one.
	err = m.client.Create(ctx, secret)
	switch {
	case err == nil:
		return password, nil
	case apierrors.IsAlreadyExists(err):
		var winner corev1.Secret
		if getErr := m.client.Get(ctx, key, &winner); getErr != nil {
			return "", fmt.Errorf("keys: re-get repository password secret %s/%s after create race: %w", namespace, name, getErr)
		}
		return passwordFrom(&winner, namespace, name)
	default:
		return "", fmt.Errorf("keys: create repository password secret %s/%s: %w", namespace, name, err)
	}
}

// readPassword reads a user-supplied password Secret. It exists as its own method so the
// error text can say "the Secret you referenced", which is a different operator action from
// "the Secret we generated is unreadable".
func (m *UserKeyManager) readPassword(ctx context.Context, namespace, name string) (string, error) {
	var secret corev1.Secret
	if err := m.client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &secret); err != nil {
		return "", fmt.Errorf("keys: get referenced repository password secret %s/%s: %w", namespace, name, err)
	}
	return passwordFrom(&secret, namespace, name)
}

// passwordFrom extracts the password from a Secret, failing closed on an absent or empty data
// key. Empty is rejected as firmly as absent: restic accepts an empty password
// (--insecure-no-password territory), so letting one through would silently initialize a
// repository whose contents are unauthenticated to anyone who can reach the bucket.
func passwordFrom(secret *corev1.Secret, namespace, name string) (string, error) {
	password, ok := secret.Data[UserPasswordSecretKey]
	if !ok || len(password) == 0 {
		return "", fmt.Errorf("keys: repository password secret %s/%s is missing data key %q",
			namespace, name, UserPasswordSecretKey)
	}
	return string(password), nil
}
