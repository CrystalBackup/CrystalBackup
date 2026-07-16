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

// Package keys implements CrystalBackup's platform DEK/KEK key envelope: the one
// mechanism that lets the operator persist a restic repository password without ever
// writing it to etcd in the clear.
//
// The model (adr/0004, the M1 plan). Every ClusterBackupLocation is backed by ONE shared
// restic repository, and that repository is encrypted under a single high-entropy
// passphrase — the platform Data Encryption Key (DEK). The DEK *is* the restic repository
// password: whoever holds it can read every tenant's data in that repository, so it must
// never be stored in plaintext. Instead the DEK is wrapped (age-encrypted) under the
// cluster Key Encryption Key (KEK), and the wrapped blob is persisted as the Secret
// crystal-dek-<location> in the operator namespace (apiconst.DefaultOperatorNamespace,
// crystal-backup-system). Only something holding the KEK can turn that blob back into the
// DEK.
//
// Why the KEK is an age IDENTITY, not merely a recipient. The operator does not just
// *produce* wrapped DEKs; it must UNWRAP them at runtime to hand restic its password on
// every backup, restore and prune. Encrypting needs only the public recipient, but
// decrypting needs the private key, so the cluster KEK is a full age X25519 identity
// ("AGE-SECRET-KEY-1...") that the operator holds in memory (loaded from the Secret named
// by ClusterEncryptionSpec.ClusterKEKSecretRef). Escrow / HSM-backed KEKs are a later
// hardening step — a different Wrapper — so in M1 the operator holds the age identity
// itself.
//
// The Wrapper seam. All DEK confidentiality flows through the small Wrapper interface, so
// a future KMS- or HSM-backed KEK can replace age wholesale without touching the DEK
// lifecycle (DEKManager) or any controller: only a new Wrapper implementation is added and
// wired in. AgeWrapper is the M1 implementation.
//
// Secrets discipline. The plaintext DEK and the KEK identity live only in local variables;
// they are never written to logs, error messages, events or object fields. Every error in
// this package names the Secret, never its contents. At rest the sole representation of a
// DEK is the wrapped ciphertext under data["dek"] — the plaintext exists only transiently
// in the operator's memory while it drives restic.
package keys

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"

	"filippo.io/age"
)

// Wrapper is the seam that isolates every controller and the DEK lifecycle from HOW a DEK
// is kept confidential at rest. Wrap turns a plaintext DEK into an opaque ciphertext that
// is safe to persist in a Secret; Unwrap reverses it. The interface is intentionally tiny
// and byte-oriented so that swapping the M1 age KEK for a future KMS/HSM KEK is a matter of
// providing a new implementation — DEKManager and the controllers are written against this
// interface and never against age.
//
// Implementations must be safe for concurrent use: a single Wrapper is shared by every
// reconcile.
type Wrapper interface {
	// Wrap encrypts plaintext (a DEK) under the KEK and returns the opaque ciphertext to
	// persist. It must never return the plaintext, in whole or in part.
	Wrap(plaintext []byte) (ciphertext []byte, err error)
	// Unwrap decrypts a ciphertext previously produced by Wrap under the SAME KEK and
	// returns the original plaintext. A ciphertext produced under a different KEK must
	// yield an error rather than garbage — callers rely on this to fail closed instead of
	// handing restic a wrong password.
	Unwrap(ciphertext []byte) (plaintext []byte, err error)
}

// AgeWrapper is the M1 Wrapper: it wraps DEKs with an age X25519 identity that is the
// cluster KEK. It holds the private identity (not just the recipient) because the operator
// must Unwrap, not only Wrap — see the package doc. The zero value is not usable; construct
// it with NewAgeWrapper.
type AgeWrapper struct {
	// identity is the cluster KEK. Its public half (identity.Recipient()) encrypts DEKs;
	// its private half decrypts them. Held only in memory for the operator's lifetime.
	identity *age.X25519Identity
}

// Compile-time proof that AgeWrapper honours the Wrapper contract, so a signature drift is
// caught at build time rather than where a controller wires the two together.
var _ Wrapper = (*AgeWrapper)(nil)

// NewAgeWrapper parses a cluster KEK age identity ("AGE-SECRET-KEY-1...") into an
// AgeWrapper. Surrounding whitespace and newlines are trimmed first because the KEK is
// almost always read from a Kubernetes Secret or a file, both of which routinely carry a
// trailing newline that age.ParseX25519Identity would otherwise reject. An empty (or
// whitespace-only) input is rejected with a clear error rather than deferred to age, so a
// missing/unmounted KEK Secret surfaces as an obvious configuration fault.
func NewAgeWrapper(kekIdentity string) (*AgeWrapper, error) {
	trimmed := strings.TrimSpace(kekIdentity)
	if trimmed == "" {
		return nil, errors.New("keys: empty cluster KEK identity")
	}
	identity, err := age.ParseX25519Identity(trimmed)
	if err != nil {
		// age's message never echoes the secret material, so it is safe to wrap.
		return nil, fmt.Errorf("keys: parse cluster KEK identity: %w", err)
	}
	return &AgeWrapper{identity: identity}, nil
}

// Wrap age-encrypts pt to the KEK's recipient and returns the binary age file. The age
// stream is buffered in memory (a DEK is tens of bytes, so the ciphertext is well under a
// kilobyte) and — critically — the writer is Closed before the bytes are read: age flushes
// and authenticates the final chunk only on Close, so returning buf.Bytes() before Close
// would yield a truncated, undecryptable ciphertext.
func (w *AgeWrapper) Wrap(pt []byte) ([]byte, error) {
	var buf bytes.Buffer
	wc, err := age.Encrypt(&buf, w.identity.Recipient())
	if err != nil {
		return nil, fmt.Errorf("keys: init age encryption: %w", err)
	}
	if _, err := wc.Write(pt); err != nil {
		return nil, fmt.Errorf("keys: write plaintext to age stream: %w", err)
	}
	if err := wc.Close(); err != nil {
		return nil, fmt.Errorf("keys: finalize age stream: %w", err)
	}
	return buf.Bytes(), nil
}

// Unwrap age-decrypts ct with the KEK identity and returns the plaintext DEK. age tries the
// identity against the file's recipient stanzas and returns an error when none matches, so
// a ciphertext wrapped under a DIFFERENT KEK fails here instead of silently producing the
// wrong bytes — the operator then refuses to run rather than open the repo with a bad
// password. That error is surfaced verbatim (wrapped), not swallowed.
func (w *AgeWrapper) Unwrap(ct []byte) ([]byte, error) {
	r, err := age.Decrypt(bytes.NewReader(ct), w.identity)
	if err != nil {
		return nil, fmt.Errorf("keys: init age decryption: %w", err)
	}
	pt, err := io.ReadAll(r)
	if err != nil {
		// A read failure here means the stream failed authentication (tampering or
		// truncation), which age reports mid-stream — treat it as a hard error.
		return nil, fmt.Errorf("keys: read decrypted plaintext: %w", err)
	}
	return pt, nil
}

// dekEntropyBytes is the size of the raw random seed behind a DEK: 256 bits, far more than
// restic's key derivation needs, chosen so the base64 passphrase is unguessable with a wide
// margin.
const dekEntropyBytes = 32

// GenerateDEK mints a fresh DEK: dekEntropyBytes of cryptographically secure randomness,
// base64-encoded. The returned string IS the restic repository password.
//
// It is base64 rather than raw bytes deliberately: this string is written verbatim to a
// RESTIC_PASSWORD_FILE, so it must be pure printable ASCII with no newline and no byte that
// a shell, a file round-trip, or a locale would mangle. base64.StdEncoding yields exactly
// that ([A-Za-z0-9+/=], no newline). The read is fail-closed via io.ReadFull: a short read
// from the entropy source returns an error instead of a weak, partially-random password.
func GenerateDEK() (string, error) {
	raw := make([]byte, dekEntropyBytes)
	if _, err := io.ReadFull(rand.Reader, raw); err != nil {
		return "", fmt.Errorf("keys: read %d bytes of DEK entropy: %w", dekEntropyBytes, err)
	}
	return base64.StdEncoding.EncodeToString(raw), nil
}
