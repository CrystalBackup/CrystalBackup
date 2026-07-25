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

package s3stat

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
)

// fakeList replays a fixed object listing, and records the prefix it was asked for so a test can
// prove the LIST was scoped to one repository.
type fakeList struct {
	objects   []minio.ObjectInfo
	gotPrefix string
	recursive bool
}

func (f *fakeList) ListObjects(_ context.Context, _ string, opts minio.ListObjectsOptions) <-chan minio.ObjectInfo {
	f.gotPrefix = opts.Prefix
	f.recursive = opts.Recursive
	ch := make(chan minio.ObjectInfo, len(f.objects))
	for _, o := range f.objects {
		ch <- o
	}
	close(ch)
	return ch
}

func obj(key string, size int64, age time.Duration, now time.Time) minio.ObjectInfo {
	return minio.ObjectInfo{Key: key, Size: size, LastModified: now.Add(-age)}
}

func TestStatSumsSizeAndAgesLocks(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	const prefix = "prod/eu-1/"

	f := &fakeList{objects: []minio.ObjectInfo{
		obj(prefix+"config", 155, time.Hour, now),
		obj(prefix+"data/ab/abcdef", 64<<20, 2*time.Hour, now),
		obj(prefix+"data/cd/cdef01", 32<<20, 2*time.Hour, now),
		obj(prefix+"index/0011", 4096, time.Hour, now),
		obj(prefix+"snapshots/aa11", 512, time.Hour, now),
		// One fresh lock (a live operation) and two past restic's staleness window.
		obj(prefix+"locks/fresh", 155, 5*time.Minute, now),
		obj(prefix+"locks/abandoned", 155, 90*time.Minute, now),
		obj(prefix+"locks/older", 155, 26*time.Hour, now),
	}}
	c := &Client{api: f, bucket: "dr"}

	got, err := c.Stat(context.Background(), prefix, now)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}

	wantSize := int64(155 + 64<<20 + 32<<20 + 4096 + 512 + 155*3)
	if got.SizeBytes != wantSize {
		t.Errorf("SizeBytes = %d, want %d", got.SizeBytes, wantSize)
	}
	if got.Objects != 8 {
		t.Errorf("Objects = %d, want 8", got.Objects)
	}
	// The fresh lock must NOT count: it plausibly belongs to something still running, and a gauge
	// that flags every live operation as a problem is a gauge nobody looks at.
	if got.StaleLocks != 2 {
		t.Errorf("StaleLocks = %d, want 2 (the fresh lock must not count)", got.StaleLocks)
	}

	// Scoped to this repository, recursively: an unscoped LIST would bill one cluster for another's
	// data on a bucket shared by several clusterIDs (R20).
	if f.gotPrefix != prefix {
		t.Errorf("listed prefix %q, want %q", f.gotPrefix, prefix)
	}
	if !f.recursive {
		t.Error("the listing must be recursive; a delimited one would miss every pack under data/")
	}
}

// TestStatFailsClosed: a truncated listing must be an error, never a partial sum. Reporting a
// repository as having shrunk because a page failed reads as data loss on a dashboard, and the
// gauge would then recover on its own — the worst kind of alert.
func TestStatFailsClosed(t *testing.T) {
	now := time.Now()
	f := &fakeList{objects: []minio.ObjectInfo{
		obj("prod/eu-1/config", 155, time.Hour, now),
		{Err: errors.New("connection reset")},
	}}
	c := &Client{api: f, bucket: "dr"}

	got, err := c.Stat(context.Background(), "prod/eu-1/", now)
	if err == nil {
		t.Fatalf("Stat returned %+v and no error on a failed listing", got)
	}
	if got.SizeBytes != 0 {
		t.Errorf("a failed Stat returned a partial size %d; it must return the zero value", got.SizeBytes)
	}
	if !strings.Contains(err.Error(), "connection reset") {
		t.Errorf("error %q does not carry the underlying cause", err)
	}
}

// TestStatIgnoresANeighbouringRepository is the reason RepoObjectPrefix ends in a slash. Two
// clusters sharing a bucket can be "prod-eu-1" and "prod-eu-10"; a prefix without the trailing
// separator would fold the second's objects into the first's size.
func TestStatIgnoresANeighbouringRepository(t *testing.T) {
	now := time.Now()
	// The fake replays whatever it is given, so this asserts the LOCK matching specifically: a lock
	// key from a neighbour must not be counted even if the storage returned it.
	f := &fakeList{objects: []minio.ObjectInfo{
		obj("prod/eu-1/locks/mine", 155, 2*time.Hour, now),
		obj("prod/eu-10/locks/theirs", 155, 2*time.Hour, now),
	}}
	c := &Client{api: f, bucket: "dr"}

	got, err := c.Stat(context.Background(), "prod/eu-1/", now)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got.StaleLocks != 1 {
		t.Errorf("StaleLocks = %d, want 1 — a neighbouring repository's lock was counted", got.StaleLocks)
	}
}
