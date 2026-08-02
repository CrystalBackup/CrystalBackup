//go:build !unix

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

import "errors"

// freeBytes has no answer off unix. The caller treats an error as "not measurable" and carries
// on with the cap alone rather than refusing to start — the collector's own image is
// distroless/linux, so this file exists to keep `go build ./...` honest on a maintainer's
// machine, not to support a platform the collector ships to.
func freeBytes(string) (int64, error) {
	return 0, errors.New("free space is not measurable on this platform")
}
