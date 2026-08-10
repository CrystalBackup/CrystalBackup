//go:build unix

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

package soak

import "golang.org/x/sys/unix"

// freeBytes is the space available to an UNPRIVILEGED writer, which is Bavail and not Bfree.
//
// The difference is the filesystem's reserved blocks — 5% by default on ext4 — and it is exactly
// the amount by which a collector using Bfree would believe it had room, keep writing, and take
// the ENOSPC anyway. The collector runs as UID 65532; the reserved blocks are not for it.
func freeBytes(dir string) (int64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, err
	}
	// DO NOT delete the int64(st.Bsize) conversion because a linter calls it unnecessary. It is
	// unnecessary on ONE of the two platforms this file is built for, and required on the other.
	//
	// This file is `//go:build unix`, so it serves linux (the operator's actual runtime) and darwin
	// (where it is developed) from one body. unix.Statfs_t.Bsize is declared per-platform:
	//
	//   linux:  Bsize int64   -> int64(st.Bsize) is a no-op, and `unconvert` reports it
	//   darwin: Bsize uint32  -> int64(st.Bsize) is REQUIRED; without it this does not compile
	//           (invalid operation: mismatched types int64 and uint32)
	//
	// No single expression satisfies both, so the redundancy is suppressed instead of removed. The
	// alternative — splitting this into freespace_linux.go and freespace_darwin.go — would duplicate
	// the Bavail-not-Bfree reasoning above, which is the part worth keeping in one place, and would
	// drop the other unixes `//go:build unix` currently covers for free.
	//
	// This is also why a macOS `make lint` cannot see the finding at all: the linter only ever
	// analyses the file under the local GOOS, so the redundancy exists exclusively in CI's view. That
	// blind spot is the reason `make lint` now ends with a GOOS=linux pass (`make lint-linux` alone);
	// without it, this suppression is unverifiable from the machine it was written on.
	//
	// gosec: Bavail is a block count and Bsize a block size; no filesystem this runs on brings
	// either near 2^63, so the signed conversions cannot overflow.
	//nolint:gosec,unconvert // int64(Bsize): required on darwin (uint32), redundant on linux (int64)
	return int64(st.Bavail) * int64(st.Bsize), nil
}
