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

// ConditionDEKEscrowed tracks the wrapped-DEK bucket escrow (03-security §4, spec/02-api.md
// §Repository layout): True once the bucket object mirrors the in-cluster wrapped DEK (or a
// recovery just adopted the bucket's copy), False with a reason while it cannot.
//
// It USED to say "advisory by design — it never gates Ready", on the grounds that an escrow
// hiccup degrades bare-cluster DR completeness and never the backups themselves. That is true
// of a hiccup and false of everything else, and the difference cost a real incident.
//
// One escrow state is genuinely advisory and stays so: EscrowWriteFailed, where the in-cluster
// DEK is known-good and only the bucket copy is behind. Every other False state now blocks the
// repository and therefore Ready — see reconcileDEKEscrow, which owns the rule and states it as
// an invariant rather than as a list of cases.
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
// loc.Status.Conditions (the caller persists them).
//
// blockRepository=true tells the caller the repository must NOT be provisioned yet.
//
// THE INVARIANT, and it is stated as one because the list-of-cases version got it wrong:
// **provisioning is allowed only when this function has POSITIVELY ESTABLISHED that it is
// safe.** Not "when nothing has gone visibly wrong" — when it is known safe. There are exactly
// two such states, and they are the two ends of the directionality below: an in-cluster DEK
// already exists (so EnsureDEK has nothing to mint), or there is provably no DEK anywhere (so
// minting one is what should happen). Everything else — including every failure to even ASK the
// question — blocks.
//
// The inverted version of that rule is what produced the 2026-08-07 incident. An operator
// restarted into a namespace whose S3 credentials Secret had not been restored yet, while its
// ClusterBackupLocation still existed. Reading the credentials failed, so this function returned
// before it could learn whether an in-cluster DEK existed — and returned "do not block". The
// repository was provisioned, EnsureDEK minted a fresh DEK four seconds after the KEK landed,
// and the location spent the next hour reporting Ready while every mover failed
// `wrong password or no key found` against a repository holding 38 snapshots. Only the
// conflict guard below — which refuses to overwrite a bucket object it cannot prove redundant —
// kept that from being permanent data loss.
//
// So the failure modes that cannot answer the question (no credentials, no KEK, an unusable KEK,
// an S3 client that will not build, a DEK Secret that will not read) all block now. They read
// like transient plumbing errors, and that is exactly why they were let through: the question
// they leave unanswered is "is there a recoverable key in the bucket?", and the cost of guessing
// "no" is a forked repository.
//
// EscrowWriteFailed is the one False state that still does NOT block, and it is the only one
// that fits the original advisory argument: the in-cluster DEK is known-good and only the bucket
// copy is behind, so backups keep working correctly and bare-cluster DR is what degrades.
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
		return true
	}
	secretKey, err := r.Secrets.GetValue(ctx, r.OperatorNamespace, loc.Spec.S3.CredentialsSecretRef.Name, mover.SecretKeyAWSSecretAccessKey)
	if err != nil {
		setCond(metav1.ConditionFalse, "CredentialsUnavailable", "cannot read the location's S3 credentials for the escrow")
		return true
	}
	store, err := r.Escrow(loc.Spec.S3, string(accessKey), string(secretKey))
	if err != nil {
		setCond(metav1.ConditionFalse, "EscrowClientError", "cannot build the S3 client for the escrow: "+clampMessage(err.Error()))
		return true
	}

	// The KEK wrapper: needed to validate a recovered blob, and by the DEK manager either way.
	identity, err := r.Secrets.GetValue(ctx, r.OperatorNamespace, loc.Spec.Encryption.ClusterKEKSecretRef.Name, kekIdentityDataKey)
	if err != nil {
		setCond(metav1.ConditionFalse, "KEKUnavailable", "cannot read the cluster KEK for the escrow")
		return true
	}
	wrapper, err := keys.NewAgeWrapper(string(identity))
	if err != nil {
		setCond(metav1.ConditionFalse, "KEKInvalid", "cannot parse the cluster KEK for the escrow")
		return true
	}
	dm := keys.NewDEKManager(r.Client, wrapper, r.OperatorNamespace)

	prefix, clusterID := loc.Spec.S3.Prefix, loc.Spec.ClusterID
	wrapped, haveSecret, err := dm.WrappedDEK(ctx, loc.Name)
	if err != nil {
		setCond(metav1.ConditionFalse, "DEKUnreadable", clampMessage(err.Error()))
		return true
	}

	if haveSecret {
		escrowed, found, err := store.Fetch(ctx, prefix, clusterID)
		if err != nil {
			setCond(metav1.ConditionFalse, "EscrowUnreachable", clampMessage(err.Error()))
			return true
		}
		if found && !bytes.Equal(escrowed, wrapped) {
			// The bucket disagrees with the cluster. Overwriting is allowed ONLY when the
			// bucket bytes decrypt to the SAME plaintext DEK under the current KEK — i.e. a
			// KEK-rotation re-wrap, where refreshing the escrow is the whole point. Anything
			// else could be the sole surviving key of another repository generation: never
			// destroy it silently; demand an operator decision.
			bucketPlain, bucketErr := wrapper.Unwrap(escrowed)
			clusterPlain, clusterErr := wrapper.Unwrap(wrapped)

			// THREE DIFFERENT SITUATIONS, three reasons. They shared one reason and one
			// sentence — "the bucket copy does not decrypt to the same key" — and that sentence
			// is a lie in two of the three cases: it asserts a comparison that never happened.
			// During the 2026-08-07 incident it sent the reader (with this file open) looking for
			// a key mismatch when the truth was a fresh mint. "Did not decrypt" and "decrypted
			// to something else" are different emergencies and only one of them is about the KEK.
			switch {
			case bucketErr != nil:
				// The escrow object does not open under the KEK this cluster holds. Either the
				// restored KEK is not the one that wrapped it, or the object is not ours. This is
				// the ONE case where the operator's own key is in question, and it is the most
				// serious: nothing in the cluster can read that repository generation.
				setCond(metav1.ConditionFalse, "EscrowUnreadableUnderKEK",
					"the bucket escrow object does not decrypt under this cluster's KEK — the KEK may not be "+
						"the one that wrapped it; refusing to overwrite (03-security §4)")
				r.Recorder.Eventf(loc, nil, corev1.EventTypeWarning, "DEKEscrowUnreadableUnderKEK", "EscrowDEK",
					"the bucket escrow object exists but does not decrypt under the cluster KEK; it is NOT being "+
						"overwritten. Verify the KEK you restored is the one this repository was created with")
				return true
			case clusterErr != nil:
				// The IN-CLUSTER wrapped DEK does not open under the KEK. The bucket copy may be
				// perfectly good; it is the local Secret that is wrong or foreign. Distinct from
				// the case above because the remedy is the opposite: here the bucket is the
				// survivor and the local Secret is what has to go.
				setCond(metav1.ConditionFalse, "ClusterDEKUnreadableUnderKEK",
					"the in-cluster wrapped DEK does not decrypt under this cluster's KEK, so it cannot be "+
						"compared with the bucket escrow; the bucket copy is left untouched")
				r.Recorder.Eventf(loc, nil, corev1.EventTypeWarning, "DEKClusterUnreadableUnderKEK", "EscrowDEK",
					"the in-cluster wrapped DEK does not decrypt under the cluster KEK; the bucket escrow is "+
						"intact and is the copy to recover from")
				return true
			case !bytes.Equal(bucketPlain, clusterPlain):
				// Both decrypt, and they are different keys. THIS is the original conflict: two
				// repository generations, and the bucket may hold the only key to the older one.
				setCond(metav1.ConditionFalse, "EscrowConflict",
					"the bucket escrow holds a DIFFERENT wrapped DEK than the cluster; refusing to overwrite — "+
						"resolve manually (03-security §4)")
				r.Recorder.Eventf(loc, nil, corev1.EventTypeWarning, "DEKEscrowConflict", "EscrowDEK",
					"bucket escrow and in-cluster DEK both decrypt under the KEK but hold DIFFERENT keys; "+
						"refusing to overwrite the escrow object")
				return true
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
