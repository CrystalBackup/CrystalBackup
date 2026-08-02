//go:build linux || darwin

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

package main

import "syscall"

// maxRSSUnit converts ru_maxrss to bytes. Linux reports KIBIBYTES and darwin reports BYTES for
// the same field — a difference that would silently misreport the number by 1024x on whichever
// platform the constant was not written for, which is why it is a named per-platform value and
// not a 1024 buried in an expression. The mover only ever runs on linux; darwin is here so the
// maintainer's `make test` compiles this file rather than skipping it.
const maxRSSUnit = rusageMaxRSSUnit

// readMaxRSS returns the peak resident set size of THIS PROCESS and of its waited-for CHILDREN,
// in bytes.
//
// RUSAGE_CHILDREN is the whole trick. It accumulates only over children that have been waited
// for — cmd.Run does exactly that — and reports the MAXIMUM ru_maxrss among them, not a sum. The
// shim runs exactly one child, restic, so this is restic's own high-water mark, taken by the
// kernel as it happened rather than sampled afterwards by anybody.
//
// It is also the reason this measurement survives everything else being unavailable: it needs no
// cgroup mount, no metrics-server, no API access and no capability, and it is identical on
// cgroup v1 and v2.
func readMaxRSS() (self, children int64, err error) {
	var s, c syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &s); err != nil {
		return 0, 0, err
	}
	if err := syscall.Getrusage(syscall.RUSAGE_CHILDREN, &c); err != nil {
		return 0, 0, err
	}
	return s.Maxrss * maxRSSUnit, c.Maxrss * maxRSSUnit, nil
}
