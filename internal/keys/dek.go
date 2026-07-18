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

// dekSecretNamePrefix is prepended to a location name to form the DEK Secret's name. It is
// a fixed, human-recognisable prefix so an operator can list every wrapped DEK in the
// namespace with `kubectl get secret -o name | grep crystal-dek-`.
const dekSecretNamePrefix = "crystal-dek-"

// DEKSecretKey is the data key inside a DEK Secret that holds the WRAPPED DEK ciphertext
// (the output of Wrapper.Wrap). There is deliberately no key holding the plaintext DEK: the
// wrapped blob is the only representation that ever touches etcd.
const DEKSecretKey = "dek"

// Standard Kubernetes "app" labels stamped on every DEK Secret so the controller's owned
// platform Secrets are discoverable (`kubectl get secret -l
// app.kubernetes.io/managed-by=crystal-backup`). These are the recommended common labels
// and are intentionally distinct from the crystalbackup.io/* domain labels in
// internal/apiconst, which tag backup RUNS rather than owned platform Secrets.
const (
	labelManagedByKey = "app.kubernetes.io/managed-by"
	labelNameKey      = "app.kubernetes.io/name"
	labelAppValue     = "crystal-backup"
)

// DEKSecretName returns the name of the Secret that holds the wrapped DEK for a given
// ClusterBackupLocation. The mapping is one Secret per location, named deterministically
// from the location name, so any reconcile can find a location's DEK without a lookup.
func DEKSecretName(locationName string) string {
	return dekSecretNamePrefix + locationName
}

// DEKManager owns the lifecycle of the wrapped-DEK Secrets. It is the only writer of those
// Secrets: it mints a DEK the first time a location needs one, wraps it under the KEK, and
// stores the ciphertext; on every later call it reads the ciphertext back and unwraps it.
// It never touches any namespace but the operator namespace, and it never returns a DEK it
// has not confirmed is persisted.
type DEKManager struct {
	// client reads and creates the DEK Secrets. A cache-backed client is fine — a DEK
	// Secret is immutable once created, so a stale read can only ever miss a Secret that a
	// racing create then reports via AlreadyExists (handled in EnsureDEK).
	client client.Client
	// wrapper wraps/unwraps DEKs under the cluster KEK (AgeWrapper in M1).
	wrapper Wrapper
	// operatorNamespace is the single namespace the DEK Secrets live in
	// (apiconst.DefaultOperatorNamespace by default). All Get/Create calls are scoped here.
	operatorNamespace string
}

// NewDEKManager builds a DEKManager. operatorNamespace is where crystal-dek-* Secrets live
// and MUST be the operator's own namespace — these Secrets are cluster-plane material and
// must never be placed in a tenant namespace.
func NewDEKManager(c client.Client, w Wrapper, operatorNamespace string) *DEKManager {
	return &DEKManager{client: c, wrapper: w, operatorNamespace: operatorNamespace}
}

// EnsureDEK returns the plaintext DEK (the restic repository password) for a location,
// creating and persisting a fresh wrapped DEK the first time and reusing the stored one
// forever after. It is idempotent and safe under concurrent reconciles.
//
// The invariant this method protects is: a restic repository has exactly ONE password for
// its whole life. Rotating the DEK would orphan every existing snapshot (they were
// encrypted under the old password), so once a DEK Secret exists it is NEVER overwritten —
// EnsureDEK only ever reads it back. That is why the create path handles an AlreadyExists
// race by re-reading and unwrapping the winner's DEK instead of returning the DEK it just
// generated but failed to persist.
//
// Neither the plaintext DEK nor the KEK is logged or embedded in any error; failures name
// the Secret only.
func (m *DEKManager) EnsureDEK(ctx context.Context, locationName string) (string, error) {
	name := DEKSecretName(locationName)
	key := client.ObjectKey{Namespace: m.operatorNamespace, Name: name}

	// Fast path: the DEK already exists — read and unwrap it. This is the case on every
	// reconcile after the first, so it dominates.
	var existing corev1.Secret
	err := m.client.Get(ctx, key, &existing)
	switch {
	case err == nil:
		return m.unwrapDEK(&existing, name)
	case apierrors.IsNotFound(err):
		// First time for this location: fall through and mint one.
	default:
		return "", fmt.Errorf("keys: get DEK secret %s/%s: %w", m.operatorNamespace, name, err)
	}

	// Mint a fresh DEK, wrap it under the KEK, and persist ONLY the ciphertext.
	dek, err := GenerateDEK()
	if err != nil {
		return "", err
	}
	wrapped, err := m.wrapper.Wrap([]byte(dek))
	if err != nil {
		return "", fmt.Errorf("keys: wrap DEK for secret %s: %w", name, err)
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: m.operatorNamespace,
			Labels: map[string]string{
				labelManagedByKey: labelAppValue,
				labelNameKey:      labelAppValue,
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{DEKSecretKey: wrapped},
	}

	err = m.client.Create(ctx, secret)
	switch {
	case err == nil:
		return dek, nil
	case apierrors.IsAlreadyExists(err):
		// A concurrent reconcile (or replica) won the create race. The DEK we just
		// generated was never persisted; returning it would hand restic a password that no
		// stored Secret can reproduce, orphaning the repo on the next restart. Re-read the
		// winner and return ITS DEK instead.
		var winner corev1.Secret
		if getErr := m.client.Get(ctx, key, &winner); getErr != nil {
			return "", fmt.Errorf("keys: re-get DEK secret %s/%s after create race: %w", m.operatorNamespace, name, getErr)
		}
		return m.unwrapDEK(&winner, name)
	default:
		return "", fmt.Errorf("keys: create DEK secret %s/%s: %w", m.operatorNamespace, name, err)
	}
}

// unwrapDEK reads the wrapped ciphertext out of a DEK Secret and unwraps it to the plaintext
// DEK. A Secret that exists but lacks the data key, or whose ciphertext cannot be unwrapped
// under the current KEK, is a hard error: EnsureDEK must fail closed rather than silently
// mint a NEW DEK over an unreadable-but-present one, which would orphan the repository whose
// real password is that unreadable blob.
func (m *DEKManager) unwrapDEK(secret *corev1.Secret, name string) (string, error) {
	wrapped, ok := secret.Data[DEKSecretKey]
	if !ok || len(wrapped) == 0 {
		return "", fmt.Errorf("keys: DEK secret %s is missing data key %q", name, DEKSecretKey)
	}
	pt, err := m.wrapper.Unwrap(wrapped)
	if err != nil {
		return "", fmt.Errorf("keys: unwrap DEK from secret %s: %w", name, err)
	}
	return string(pt), nil
}
