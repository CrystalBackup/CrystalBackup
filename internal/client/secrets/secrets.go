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

// Package secrets is CrystalBackup's ONLY sanctioned way to read a Kubernetes
// Secret: a deliberately minimal, UNcached, GET-by-name reader. It exists to
// enforce tenancy invariant I3 — the operator must NEVER build a cluster-wide
// Secret cache or informer.
//
// Why a whole package for one Get call: controller-runtime's default client
// (manager.GetClient()) is CACHED. The first time any controller reads a Secret
// through it, the manager transparently spins up a shared informer that LISTs and
// WATCHes Secrets — and, unless a namespace/field selector is configured, it does
// so cluster-wide. That has two consequences CrystalBackup must not accept:
//
//   - RBAC: a list/watch informer needs the `list` and `watch` verbs on Secrets.
//     The operator's Secret RBAC is intentionally GET-only (it reads specific,
//     named platform Secrets — the DEKs, the cluster KEK, the DR S3 credentials —
//     under apiconst.DefaultOperatorNamespace, plus caller-named BackupLocation
//     credential Secrets). Granting list/watch to satisfy the cache would hand the
//     operator the ability to enumerate every Secret in the cluster.
//   - Blast radius / memory: a cluster-wide Secret informer mirrors the decrypted
//     bytes of EVERY tenant's Secrets into the operator's process memory and keeps
//     them hot for the process lifetime. A backup operator reading a handful of its
//     own credential Secrets has no business holding the whole cluster's secret
//     material resident.
//
// This package sidesteps both by wrapping a client.Reader that performs a direct,
// one-shot API GET for exactly the (namespace, name) asked for — no cache, no
// informer, no list, no watch. See NewByNameReader for the one constructor rule
// that makes this guarantee real.
//
// It is pure plumbing: it imports only the stdlib, the core/v1 Secret type and the
// controller-runtime client interface, so any controller may depend on it without a
// cycle. It performs no decryption — callers hand the returned bytes to
// internal/age (the DEK/KEK material) or to restic (the repository password / S3
// credentials); keeping crypto out of the reader keeps the reader trivially
// auditable.
package secrets

import (
	"context"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ErrKeyNotFound reports that the Secret was found but does not contain the
// requested data key. It is a distinct sentinel (test it with errors.Is) precisely
// so a caller can tell this apart from "the Secret object itself is missing":
//
//   - Secret object missing  → the error from Get satisfies apierrors.IsNotFound.
//   - Secret present, key absent → the error from GetValue satisfies
//     errors.Is(err, ErrKeyNotFound).
//
// The two failure modes usually warrant different handling (a missing Secret is
// often "wait and requeue, it may be created"; a Secret that exists but lacks the
// expected key is a misconfiguration to surface on status), which is why they carry
// different, machine-checkable identities instead of one opaque error.
var ErrKeyNotFound = errors.New("key not found in secret")

// ByNameReader reads Secrets one at a time, by name, through an uncached
// client.Reader. It holds no state beyond that reader and caches nothing itself;
// every call is an independent API round-trip. It is safe for concurrent use to the
// same extent the underlying reader is (controller-runtime's readers are).
type ByNameReader struct {
	// reader is the uncached, GET-capable API reader every lookup delegates to.
	// The tenancy guarantee of this whole package rests on this being an UNcached
	// reader (see NewByNameReader) — the type is the read-only client.Reader
	// interface (not client.Client) as a small reminder that this path only ever
	// reads, never writes, Secrets.
	reader client.Reader
}

// NewByNameReader wraps r in a ByNameReader.
//
// IN PRODUCTION r MUST be manager.GetAPIReader() — the manager's direct, UNCACHED
// API reader — and MUST NOT be manager.GetClient(). This is the entire reason the
// package exists: GetClient() is cache-backed, and the first Secret read through it
// makes controller-runtime start a cluster-wide Secret informer (list+watch),
// violating tenancy invariant I3 and demanding list/watch RBAC the operator does
// not and must not have. GetAPIReader() issues a plain GET straight to the API
// server, so wiring this reader with it keeps the operator's Secret access GET-only
// and cache-free. Passing GetClient() here would compile and pass tests yet quietly
// re-introduce exactly the cluster-wide cache this package was built to prevent — so
// the wiring is asserted at the call site, not here.
//
// In tests r is a fake client (sigs.k8s.io/controller-runtime/pkg/client/fake),
// which is likewise non-caching.
func NewByNameReader(r client.Reader) *ByNameReader {
	return &ByNameReader{reader: r}
}

// Get fetches the single Secret named (namespace, name) with one direct API GET.
//
// It returns the freshly deserialized *corev1.Secret, or the underlying reader's
// error UNWRAPPED so the standard apimachinery predicates keep working — most
// importantly apierrors.IsNotFound(err), which callers use to treat an
// absent Secret as a transient, requeue-able condition rather than a hard failure.
// Because the read is uncached, the returned object is a fresh copy owned entirely
// by the caller; it shares no memory with any informer cache (there is none) and is
// safe to retain or mutate.
func (b *ByNameReader) Get(ctx context.Context, namespace, name string) (*corev1.Secret, error) {
	var s corev1.Secret
	if err := b.reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// GetValue fetches the Secret and returns the raw bytes stored under data key `key`.
//
// It is the common case — the caller wants one field (a repository password, an S3
// access key, a wrapped DEK) rather than the whole object — expressed so the two
// ways it can fail stay distinguishable:
//
//   - the Secret does not exist → the returned error satisfies apierrors.IsNotFound
//     (it is Get's error, passed through untouched);
//   - the Secret exists but has no such key → the returned error satisfies
//     errors.Is(err, ErrKeyNotFound), and its message names the namespace, secret
//     and key for a human reading logs/events.
//
// Only the Secret's Data map is consulted. StringData is a write-only convenience
// field: the API server folds it into Data (base64-encoded) on write, so a Secret
// read back from the API server always carries every value in Data — checking
// StringData here would be dead code. A nil Data map (a Secret with no entries) is
// handled as simply "key absent". The returned slice is the caller's to keep: the
// enclosing Secret was freshly read (uncached), so nothing else references it.
func (b *ByNameReader) GetValue(ctx context.Context, namespace, name, key string) ([]byte, error) {
	s, err := b.Get(ctx, namespace, name)
	if err != nil {
		return nil, err
	}
	v, ok := s.Data[key]
	if !ok {
		return nil, fmt.Errorf("secret %s/%s: %w: %q", namespace, name, ErrKeyNotFound, key)
	}
	return v, nil
}
