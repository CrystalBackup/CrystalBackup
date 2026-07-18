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
	"bytes"
	"context"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/keys"
	"github.com/CrystalBackup/CrystalBackup/internal/mover"
	"github.com/CrystalBackup/CrystalBackup/internal/status"
)

// ConditionDEKEscrowed is the ADVISORY condition tracking the wrapped-DEK bucket escrow
// (03-security §4, spec/02-api.md §Repository layout): True once the bucket object mirrors
// the in-cluster wrapped DEK (or a recovery just adopted the bucket's copy), False with a
// reason while it cannot. Advisory by design — an escrow hiccup degrades bare-cluster DR
// completeness, never the backups themselves, so it never gates Ready.
const ConditionDEKEscrowed = "DEKEscrowed"

// EscrowStore is the bucket side of the wrapped-DEK escrow — internal/escrow.Store in
// production, a stub in envtest.
type EscrowStore interface {
	Fetch(ctx context.Context, prefix, clusterID string) (wrapped []byte, found bool, err error)
	Put(ctx context.Context, prefix, clusterID string, wrapped []byte) error
}

// EscrowFactory builds an EscrowStore for one location's S3 spec and credentials. A nil
// factory on the reconciler disables the escrow entirely (the envtest default — no S3).
type EscrowFactory func(s3 cbv1.S3Spec, accessKey, secretKey string) (EscrowStore, error)

// reconcileDEKEscrow drives the wrapped-DEK bucket escrow for one location, in-memory on
// loc.Status.Conditions (the caller persists them). blockRepository=true tells the caller
// the repository must NOT be provisioned yet: the in-cluster DEK Secret is missing AND the
// escrow question is unresolved (unreachable, or an object exists but could not be
// adopted) — provisioning would let EnsureDEK mint a FRESH DEK while a recoverable one may
// exist, forking the repository password.
//
// Directionality is the whole point (03-security §4):
//   - Secret exists → assert the bucket mirrors it. A bucket object with DIFFERENT bytes is
//     only overwritten when it decrypts to the SAME plaintext DEK under the current KEK (a
//     KEK-rotation re-wrap); anything else is an EscrowConflict surfaced to the operator and
//     NEVER overwritten — the bucket copy may be the only key to an older repository
//     generation, and destroying it is exactly what the escrow exists to prevent.
//   - Secret missing → RECOVER from the bucket if the object exists (bare-cluster DR
//     bootstrap: KEK re-supplied by the admin + this object = the repository opens), after
//     validating it unwraps under the current KEK.
//   - Neither exists → a genuinely fresh location; the first repository use mints the DEK
//     and the next pass escrows it.
func (r *ClusterBackupLocationReconciler) reconcileDEKEscrow(ctx context.Context, loc *cbv1.ClusterBackupLocation) (blockRepository bool) {
	if r.Escrow == nil {
		return false // escrow not wired (envtest); leave no condition either way.
	}
	log := logf.FromContext(ctx)

	setCond := func(condStatus metav1.ConditionStatus, reason, message string) {
		status.SetCondition(&loc.Status.Conditions, ConditionDEKEscrowed, condStatus, reason, message, loc.Generation)
	}

	accessKey, err := r.Secrets.GetValue(ctx, r.OperatorNamespace, loc.Spec.S3.CredentialsSecretRef.Name, mover.SecretKeyAWSAccessKeyID)
	if err != nil {
		setCond(metav1.ConditionFalse, "CredentialsUnavailable", "cannot read the location's S3 credentials for the escrow")
		return false
	}
	secretKey, err := r.Secrets.GetValue(ctx, r.OperatorNamespace, loc.Spec.S3.CredentialsSecretRef.Name, mover.SecretKeyAWSSecretAccessKey)
	if err != nil {
		setCond(metav1.ConditionFalse, "CredentialsUnavailable", "cannot read the location's S3 credentials for the escrow")
		return false
	}
	store, err := r.Escrow(loc.Spec.S3, string(accessKey), string(secretKey))
	if err != nil {
		setCond(metav1.ConditionFalse, "EscrowClientError", "cannot build the S3 client for the escrow: "+clampMessage(err.Error()))
		return false
	}

	// The KEK wrapper: needed to validate a recovered blob, and by the DEK manager either way.
	identity, err := r.Secrets.GetValue(ctx, r.OperatorNamespace, loc.Spec.Encryption.ClusterKEKSecretRef.Name, kekIdentityDataKey)
	if err != nil {
		setCond(metav1.ConditionFalse, "KEKUnavailable", "cannot read the cluster KEK for the escrow")
		return false
	}
	wrapper, err := keys.NewAgeWrapper(string(identity))
	if err != nil {
		setCond(metav1.ConditionFalse, "KEKInvalid", "cannot parse the cluster KEK for the escrow")
		return false
	}
	dm := keys.NewDEKManager(r.Client, wrapper, r.OperatorNamespace)

	prefix, clusterID := loc.Spec.S3.Prefix, loc.Spec.ClusterID
	wrapped, haveSecret, err := dm.WrappedDEK(ctx, loc.Name)
	if err != nil {
		setCond(metav1.ConditionFalse, "DEKUnreadable", clampMessage(err.Error()))
		return false
	}

	if haveSecret {
		escrowed, found, err := store.Fetch(ctx, prefix, clusterID)
		if err != nil {
			setCond(metav1.ConditionFalse, "EscrowUnreachable", clampMessage(err.Error()))
			return false
		}
		if found && !bytes.Equal(escrowed, wrapped) {
			// The bucket disagrees with the cluster. Overwriting is allowed ONLY when the
			// bucket bytes decrypt to the SAME plaintext DEK under the current KEK — i.e. a
			// KEK-rotation re-wrap, where refreshing the escrow is the whole point. Anything
			// else could be the sole surviving key of another repository generation: never
			// destroy it silently; demand an operator decision.
			bucketPlain, bucketErr := wrapper.Unwrap(escrowed)
			clusterPlain, clusterErr := wrapper.Unwrap(wrapped)
			if bucketErr != nil || clusterErr != nil || !bytes.Equal(bucketPlain, clusterPlain) {
				setCond(metav1.ConditionFalse, "EscrowConflict",
					"the bucket escrow holds a DIFFERENT wrapped DEK than the cluster; refusing to overwrite — "+
						"resolve manually (03-security §4)")
				r.Recorder.Eventf(loc, nil, corev1.EventTypeWarning, "DEKEscrowConflict", "EscrowDEK",
					"bucket escrow and in-cluster DEK disagree and the bucket copy does not decrypt to the same key; "+
						"refusing to overwrite the escrow object")
				return false
			}
		}
		if !found || !bytes.Equal(escrowed, wrapped) {
			if err := store.Put(ctx, prefix, clusterID, wrapped); err != nil {
				setCond(metav1.ConditionFalse, "EscrowWriteFailed", clampMessage(err.Error()))
				r.Recorder.Eventf(loc, nil, corev1.EventTypeWarning, "DEKEscrowWriteFailed", "EscrowDEK",
					"writing the wrapped DEK to the bucket escrow failed; bare-cluster DR is incomplete until it succeeds")
				return false
			}
			log.Info("Escrowed the wrapped DEK to the bucket", "location", loc.Name)
			r.Recorder.Eventf(loc, nil, corev1.EventTypeNormal, "DEKEscrowed", "EscrowDEK",
				"wrapped DEK escrowed to the bucket (ciphertext only; useless without the KEK)")
		}
		setCond(metav1.ConditionTrue, "Escrowed", "the bucket escrow mirrors the wrapped DEK")
		return false
	}

	// No in-cluster DEK: try the bucket (the bare-cluster DR bootstrap path). While the
	// escrow question is UNRESOLVED (unreachable, or unadoptable object), the repository
	// must not be provisioned — EnsureDEK would mint a fresh DEK over a recoverable one.
	escrowed, found, err := store.Fetch(ctx, prefix, clusterID)
	if err != nil {
		setCond(metav1.ConditionFalse, "EscrowUnreachable", clampMessage(err.Error()))
		return true
	}
	if !found {
		setCond(metav1.ConditionFalse, "AwaitingFirstDEK",
			"no DEK exists yet; the first repository use mints it and the next pass escrows it")
		return false
	}
	if err := dm.AdoptWrappedDEK(ctx, loc.Name, escrowed); err != nil {
		setCond(metav1.ConditionFalse, "RecoveryFailed", clampMessage(err.Error()))
		r.Recorder.Eventf(loc, nil, corev1.EventTypeWarning, "DEKRecoveryFailed", "RecoverDEK",
			"a bucket-escrowed wrapped DEK exists but could not be adopted: %s", clampMessage(err.Error()))
		return true
	}
	log.Info("Recovered the wrapped DEK from the bucket escrow", "location", loc.Name)
	r.Recorder.Eventf(loc, nil, corev1.EventTypeNormal, "DEKRecovered", "RecoverDEK",
		"wrapped DEK recovered from the bucket escrow (bare-cluster DR bootstrap)")
	setCond(metav1.ConditionTrue, "Recovered", "the wrapped DEK was recovered from the bucket escrow")
	return false
}
