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

// Package crucible holds the real-conditions e2e suite that runs against a
// live Hetzner cluster provisioned by test/crucible (RKE2 + rook-ceph +
// longhorn + local-path + a Hetzner Object Storage bucket).
//
// Every test file carries the `crucible` build tag so `go test ./...` (and
// `make test`) never tries to reach a live cluster. Run through the crucible
// Makefile instead:
//
//	cd test/crucible && make test            # everything
//	cd test/crucible && make test LABELS=m0  # one milestone
//
// Specs are labeled by milestone (infra, m0, m1, ...); each new milestone adds
// its own file with its own label — see test/crucible/README.md.
package crucible
