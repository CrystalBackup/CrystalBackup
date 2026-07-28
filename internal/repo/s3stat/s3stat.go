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

// Package s3stat measures a restic repository's physical footprint and its lock backlog by
// listing object storage directly, with no restic involved.
//
// # Why not ask restic
//
// The two figures 05-observability §2.4 asks for — the repository's stored size and how many
// stale lock objects are lying around — could both be had from restic (`stats --mode raw-data`,
// and reading the lock objects). Both would cost a mover Job: a pod, an image pull, the
// repository password, and a turn of wall-clock. That is an absurd price for two numbers a LIST
// already answers, and it would put a recurring Job on a cluster purely to feed a gauge.
//
// The operator already holds the location's S3 credentials (it probes reachability with them), so
// listing the repository's own key prefix gives the exact physical size — post-dedup,
// post-compression, the actual bill for that bucket — and an exact count of lock objects past
// restic's staleness window. No key material is involved: this reads object METADATA only, never
// object contents, so it cannot see anything encrypted and does not need the DEK.
//
// # What it deliberately does not do
//
// It does not delete anything. A stale lock count is a signal, not a reaper: locks are removed by
// `restic unlock --remove-all` under mover quiescence (adr/0015 §3), because deleting a lock
// object whose owner is actually alive corrupts a live write. Counting is safe; acting is not.
package s3stat

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
)

// LockStaleAfter is restic's own staleness window for repository locks. A lock younger than this
// belongs to something that may well still be running; one older is either abandoned or held by a
// process that has stopped refreshing it, which is what the gauge is for.
const LockStaleAfter = 30 * time.Minute

// lockPrefix is the sub-path restic keeps lock objects under, relative to the repository root.
const lockPrefix = "locks/"

// Stats is one measurement of a repository's footprint.
type Stats struct {
	// SizeBytes is the sum of every object stored under the repository's prefix.
	SizeBytes int64
	// Objects is how many objects that sum covers — useful when reading a surprising size.
	Objects int
	// StaleLocks counts lock objects older than LockStaleAfter.
	StaleLocks int
}

// Prober is the seam controllers depend on, so the maintenance controller can be tested without
// object storage.
type Prober interface {
	Stat(ctx context.Context, repoPrefix string, now time.Time) (Stats, error)
}

// listAPI is the minio surface Client consumes — a seam for tests.
type listAPI interface {
	ListObjects(ctx context.Context, bucket string, opts minio.ListObjectsOptions) <-chan minio.ObjectInfo
}

// Client measures repositories on one location's S3 endpoint with that location's credentials.
type Client struct {
	api    listAPI
	bucket string
}

// New builds a Client for one location's S3 spec and static credentials. The endpoint's scheme
// selects TLS, an optional PEM caBundle pins the trust anchors, and forcePathStyle keeps
// path-style addressing — the same construction as internal/escrow, because it is the same
// endpoint with the same credentials and the two must not disagree about how to reach it.
func New(s3 cbv1.S3Spec, accessKey, secretKey string) (*Client, error) {
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
			return nil, errors.New("s3stat: caBundle contains no usable PEM certificate")
		}
		transport, err := minio.DefaultTransport(true)
		if err != nil {
			return nil, fmt.Errorf("s3stat: build TLS transport: %w", err)
		}
		transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
		opts.Transport = transport
	}

	c, err := minio.New(endpoint, opts)
	if err != nil {
		return nil, fmt.Errorf("s3stat: build S3 client for %s: %w", s3.Endpoint, err)
	}
	return &Client{api: c, bucket: s3.Bucket}, nil
}

// Stat lists everything under repoPrefix once and returns the footprint.
//
// One recursive LIST answers both questions, so the lock count rides along for free rather than
// costing a second round trip. The pass is O(objects) in requests-of-1000, which is why callers
// must run it off the reconcile worker and on a slow cadence: a multi-terabyte repository is
// hundreds of thousands of objects, and this is a gauge, not a control signal.
//
// now is injected so a test can age locks deterministically.
func (c *Client) Stat(ctx context.Context, repoPrefix string, now time.Time) (Stats, error) {
	var out Stats
	locks := repoPrefix + lockPrefix

	for obj := range c.api.ListObjects(ctx, c.bucket, minio.ListObjectsOptions{
		Prefix:    repoPrefix,
		Recursive: true,
	}) {
		if obj.Err != nil {
			// Partial results are worse than none: a truncated listing would report a repository
			// as having shrunk, which reads as data loss on a dashboard.
			return Stats{}, fmt.Errorf("s3stat: list %s/%s: %w", c.bucket, repoPrefix, obj.Err)
		}
		out.SizeBytes += obj.Size
		out.Objects++
		if strings.HasPrefix(obj.Key, locks) && now.Sub(obj.LastModified) > LockStaleAfter {
			out.StaleLocks++
		}
	}
	return out, nil
}
