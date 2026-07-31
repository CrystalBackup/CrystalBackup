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

// Command genmoverdocs writes the mover sizing reference from the table in
// internal/mover/profiles.go. Run it with `make mover-profiles`; `make mover-profiles-verify`
// fails the build when the committed file no longer matches, exactly as `make alert-rules-verify`
// guards the generated alert rules.
//
// The point is not convenience. An operator sizing a cluster reads the documented numbers and an
// operator debugging an eviction reads the running ones; if those were two files, one of them
// would eventually be wrong, and it would be the one nobody re-read.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/CrystalBackup/CrystalBackup/internal/mover"
)

func main() {
	root := flag.String("root", ".", "repository root the reference is written under")
	flag.Parse()

	out := filepath.Join(*root, mover.ProfilesDocFile)
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "genmoverdocs: %v\n", err)
		os.Exit(1)
	}
	if err := os.WriteFile(out, mover.RenderProfilesDoc(), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "genmoverdocs: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Wrote %s (%d operations)\n", out, len(mover.Operations()))
}
