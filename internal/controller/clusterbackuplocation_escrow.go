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
// loc.Status.Conditions (the caller's single status write persists it). Never blocking and
// never an error: every failure lands on the advisory condition + a Warning event.
//
// Directionality is the whole point (03-security §4):
//   - Secret exists  → assert the bucket mirrors it (one Stat per pass; Put only on drift —
//     which is also what re-escrows after a KEK rotation re-wrap).
//   - Secret missing → RECOVER from the bucket if the object exists (bare-cluster DR
//     bootstrap: KEK re-supplied by the admin + this object = the repository opens), after
//     validating it unwraps under the current KEK. Runs BEFORE ensureRepository in the
//     reconcile, so recovery always precedes the first EnsureDEK a repository init would
//     run — a DR bootstrap can never mint a fresh DEK over a recoverable one.
//   - Neither exists → a genuinely fresh location; the first repository use mints the DEK
//     and the next pass escrows it.
func (r *ClusterBackupLocationReconciler) reconcileDEKEscrow(ctx context.Context, loc *cbv1.ClusterBackupLocation) {
	if r.Escrow == nil {
		return // escrow not wired (envtest); leave no condition either way.
	}
	log := logf.FromContext(ctx)

	setCond := func(condStatus metav1.ConditionStatus, reason, message string) {
		status.SetCondition(&loc.Status.Conditions, ConditionDEKEscrowed, condStatus, reason, message, loc.Generation)
	}

	accessKey, err := r.Secrets.GetValue(ctx, r.OperatorNamespace, loc.Spec.S3.CredentialsSecretRef.Name, mover.SecretKeyAWSAccessKeyID)
	if err != nil {
		setCond(metav1.ConditionFalse, "CredentialsUnavailable", "cannot read the location's S3 credentials for the escrow")
		return
	}
	secretKey, err := r.Secrets.GetValue(ctx, r.OperatorNamespace, loc.Spec.S3.CredentialsSecretRef.Name, mover.SecretKeyAWSSecretAccessKey)
	if err != nil {
		setCond(metav1.ConditionFalse, "CredentialsUnavailable", "cannot read the location's S3 credentials for the escrow")
		return
	}
	store, err := r.Escrow(loc.Spec.S3, string(accessKey), string(secretKey))
	if err != nil {
		setCond(metav1.ConditionFalse, "EscrowClientError", "cannot build the S3 client for the escrow: "+clampMessage(err.Error()))
		return
	}

	// The KEK wrapper: needed to validate a recovered blob, and by the DEK manager either way.
	identity, err := r.Secrets.GetValue(ctx, r.OperatorNamespace, loc.Spec.Encryption.ClusterKEKSecretRef.Name, kekIdentityDataKey)
	if err != nil {
		setCond(metav1.ConditionFalse, "KEKUnavailable", "cannot read the cluster KEK for the escrow")
		return
	}
	wrapper, err := keys.NewAgeWrapper(string(identity))
	if err != nil {
		setCond(metav1.ConditionFalse, "KEKInvalid", "cannot parse the cluster KEK for the escrow")
		return
	}
	dm := keys.NewDEKManager(r.Client, wrapper, r.OperatorNamespace)

	prefix, clusterID := loc.Spec.S3.Prefix, loc.Spec.ClusterID
	wrapped, haveSecret, err := dm.WrappedDEK(ctx, loc.Name)
	if err != nil {
		setCond(metav1.ConditionFalse, "DEKUnreadable", clampMessage(err.Error()))
		return
	}

	if haveSecret {
		escrowed, found, err := store.Fetch(ctx, prefix, clusterID)
		if err != nil {
			setCond(metav1.ConditionFalse, "EscrowUnreachable", clampMessage(err.Error()))
			return
		}
		if !found || !bytes.Equal(escrowed, wrapped) {
			if err := store.Put(ctx, prefix, clusterID, wrapped); err != nil {
				setCond(metav1.ConditionFalse, "EscrowWriteFailed", clampMessage(err.Error()))
				r.Recorder.Eventf(loc, nil, corev1.EventTypeWarning, "DEKEscrowWriteFailed", "EscrowDEK",
					"writing the wrapped DEK to the bucket escrow failed; bare-cluster DR is incomplete until it succeeds")
				return
			}
			log.Info("Escrowed the wrapped DEK to the bucket", "location", loc.Name)
			r.Recorder.Eventf(loc, nil, corev1.EventTypeNormal, "DEKEscrowed", "EscrowDEK",
				"wrapped DEK escrowed to the bucket (ciphertext only; useless without the KEK)")
		}
		setCond(metav1.ConditionTrue, "Escrowed", "the bucket escrow mirrors the wrapped DEK")
		return
	}

	// No in-cluster DEK: try the bucket (the bare-cluster DR bootstrap path).
	escrowed, found, err := store.Fetch(ctx, prefix, clusterID)
	if err != nil {
		setCond(metav1.ConditionFalse, "EscrowUnreachable", clampMessage(err.Error()))
		return
	}
	if !found {
		setCond(metav1.ConditionFalse, "AwaitingFirstDEK",
			"no DEK exists yet; the first repository use mints it and the next pass escrows it")
		return
	}
	if err := dm.AdoptWrappedDEK(ctx, loc.Name, escrowed); err != nil {
		setCond(metav1.ConditionFalse, "RecoveryFailed", clampMessage(err.Error()))
		r.Recorder.Eventf(loc, nil, corev1.EventTypeWarning, "DEKRecoveryFailed", "RecoverDEK",
			"a bucket-escrowed wrapped DEK exists but could not be adopted: %s", clampMessage(err.Error()))
		return
	}
	log.Info("Recovered the wrapped DEK from the bucket escrow", "location", loc.Name)
	r.Recorder.Eventf(loc, nil, corev1.EventTypeNormal, "DEKRecovered", "RecoverDEK",
		"wrapped DEK recovered from the bucket escrow (bare-cluster DR bootstrap)")
	setCond(metav1.ConditionTrue, "Recovered", "the wrapped DEK was recovered from the bucket escrow")
}
