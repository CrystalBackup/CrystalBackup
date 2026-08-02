//go:build !linux && !darwin

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

import "errors"

// readMaxRSS on a platform with no getrusage the stdlib exposes. It REFUSES rather than returning
// zeros: a zero peak would travel to the soak as "this mover used no memory", and an absent one
// travels as "not measured", which is the only true statement available here.
func readMaxRSS() (self, children int64, err error) {
	return 0, 0, errors.New("peak RSS is not available on this platform")
}
