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

// Package escrow mirrors the WRAPPED platform DEK — the age ciphertext, useless without
// the KEK — into the location's own bucket, closing the bare-cluster DR gap
// (03-security-and-tenancy.md §4, spec/02-api.md §Repository layout): with the KEK safely
// escrowed out-of-band by the administrator, a total cluster loss previously stranded the
// wrapped DEK (it lived only in etcd), and the KEK alone cannot open the repository. With
// this escrow, DR bootstrap is: re-supply the KEK Secret + create a ClusterBackupLocation —
// the operator recovers the wrapped DEK from the bucket, discovery inventories the repo,
// and ClusterRestore reconstitutes namespaces.
//
// The object lives at a SIBLING of the repository prefix —
// "<prefix>/<clusterID>.crystal-meta/wrapped-dek.age" — deliberately OUTSIDE the restic
// repository root (restic never sees it; a repo copy/check never drags it along) and
// outside the movers' repo-scoped credential prefix "<prefix>/<clusterID>/*" (invariant I4:
// only the operator's root credentials reach it).
//
// Security note: the escrowed bytes are ciphertext under the admin-held KEK. Writing them
// to the bucket grants the bucket nothing it did not already have — an attacker with
// bucket access already holds the (encrypted) repository itself.
package escrow

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
)

// objectSuffix is the fixed tail of the escrow object key; metaSegment the sibling-marker
// inserted after the clusterID. Together: "<prefix>/<clusterID>.crystal-meta/wrapped-dek.age".
const (
	metaSegment  = ".crystal-meta"
	objectSuffix = "wrapped-dek.age"
)

// maxObjectSize bounds a fetched escrow object. A wrapped DEK is a few hundred bytes of age
// ciphertext; a multi-kilobyte object is not ours and is rejected rather than adopted.
const maxObjectSize = 64 * 1024

// ObjectKey returns the escrow object key for a repository coordinate — a sibling of the
// repository prefix, never inside it. With an empty prefix the key degenerates to
// "<clusterID>.crystal-meta/wrapped-dek.age". Deterministic and version-stable: this string
// is part of the DR contract (documented in 02-api.md) and must never drift.
func ObjectKey(prefix, clusterID string) string {
	segments := []string{}
	if p := strings.Trim(prefix, "/"); p != "" {
		segments = append(segments, p)
	}
	segments = append(segments, clusterID+metaSegment, objectSuffix)
	return strings.Join(segments, "/")
}

// Store reads and writes escrow objects on one location's S3 endpoint, with the location's
// own credentials. It is a thin, dependency-injected wrapper over the minio client so the
// controller integration stays testable (see the Client seam).
type Store struct {
	client objectAPI
	bucket string
}

// objectAPI is the minio surface Store consumes — a seam for tests.
type objectAPI interface {
	GetObject(ctx context.Context, bucketName, objectName string, opts minio.GetObjectOptions) (*minio.Object, error)
	PutObject(ctx context.Context, bucketName, objectName string, reader io.Reader, objectSize int64, opts minio.PutObjectOptions) (minio.UploadInfo, error)
	StatObject(ctx context.Context, bucketName, objectName string, opts minio.StatObjectOptions) (minio.ObjectInfo, error)
}

// New builds a Store for one location's S3 spec and static credentials. The endpoint's
// scheme selects TLS; an optional PEM caBundle pins the trust anchors (same semantics as
// the mover's restic client); forcePathStyle keeps path-style addressing for the non-AWS
// gateways this project targets.
func New(s3 cbv1.S3Spec, accessKey, secretKey string) (*Store, error) {
	endpoint := s3.Endpoint
	secure := true
	switch {
	case strings.HasPrefix(endpoint, "https://"):
		endpoint = strings.TrimPrefix(endpoint, "https://")
	case strings.HasPrefix(endpoint, "http://"):
		endpoint = strings.TrimPrefix(endpoint, "http://")
		secure = false
	}
	endpoint = strings.TrimRight(endpoint, "/")

	opts := &minio.Options{
		Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure: secure,
		Region: s3.Region,
	}
	if s3.ForcePathStyle {
		opts.BucketLookup = minio.BucketLookupPath
	}
	if secure && s3.CABundle != "" {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(s3.CABundle)) {
			return nil, errors.New("escrow: caBundle contains no usable PEM certificate")
		}
		transport, err := minio.DefaultTransport(true)
		if err != nil {
			return nil, fmt.Errorf("escrow: build TLS transport: %w", err)
		}
		transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
		opts.Transport = transport
	}

	c, err := minio.New(endpoint, opts)
	if err != nil {
		return nil, fmt.Errorf("escrow: build S3 client for %s: %w", s3.Endpoint, err)
	}
	return &Store{client: c, bucket: s3.Bucket}, nil
}

// Fetch returns the escrowed wrapped DEK, with found=false when the object does not exist.
// Any other failure (network, auth, an oversized object) is an error — the caller must be
// able to distinguish "no escrow yet" from "cannot know".
func (s *Store) Fetch(ctx context.Context, prefix, clusterID string) (wrapped []byte, found bool, err error) {
	key := ObjectKey(prefix, clusterID)
	if _, err := s.client.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{}); err != nil {
		if isNoSuchKey(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("escrow: stat %s/%s: %w", s.bucket, key, err)
	}
	obj, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, false, fmt.Errorf("escrow: get %s/%s: %w", s.bucket, key, err)
	}
	defer func() { _ = obj.Close() }()
	data, err := io.ReadAll(io.LimitReader(obj, maxObjectSize+1))
	if err != nil {
		return nil, false, fmt.Errorf("escrow: read %s/%s: %w", s.bucket, key, err)
	}
	if len(data) == 0 || len(data) > maxObjectSize {
		return nil, false, fmt.Errorf("escrow: object %s/%s has implausible size %d for a wrapped DEK", s.bucket, key, len(data))
	}
	return data, true, nil
}

// Put writes (or overwrites) the escrow object with the wrapped DEK ciphertext. Idempotent
// by content: callers compare with Fetch first and only Put on drift, so a healthy steady
// state issues one HEAD per reconcile, no writes.
func (s *Store) Put(ctx context.Context, prefix, clusterID string, wrapped []byte) error {
	key := ObjectKey(prefix, clusterID)
	_, err := s.client.PutObject(ctx, s.bucket, key, bytes.NewReader(wrapped), int64(len(wrapped)),
		minio.PutObjectOptions{ContentType: "application/octet-stream"})
	if err != nil {
		return fmt.Errorf("escrow: put %s/%s: %w", s.bucket, key, err)
	}
	return nil
}

// isNoSuchKey reports the one condition that means "the escrow OBJECT does not exist":
// minio's NoSuchKey (which its StatObject error mapping also synthesizes for a bodyless
// HEAD 404 when an object name was given). Anything else — NoSuchBucket from a mistyped
// bucket, auth failures, gateway oddities — stays an ERROR: Fetch's contract is that
// found=false asserts positive knowledge of absence, because the recovery path treats it
// as "a genuinely fresh location" and proceeds to mint.
func isNoSuchKey(err error) bool {
	resp := minio.ToErrorResponse(err)
	return resp.Code == "NoSuchKey" || resp.Code == "NotFound"
}
