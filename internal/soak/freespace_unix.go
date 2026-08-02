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
	return int64(st.Bavail) * int64(st.Bsize), nil //nolint:gosec // block counts do not overflow int64
}
