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

// Command genrules writes the chart's Prometheus rule group from the table in
// internal/alerts/rules.go. Run it with `make alert-rules`; `make alert-rules-verify` fails the
// build when the committed file no longer matches, the same guard `make api-docs-verify` gives the
// generated API reference.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/CrystalBackup/CrystalBackup/internal/alerts"
)

func main() {
	chartDir := flag.String("chart-dir", "charts/crystal-backup",
		"chart directory the rule file is written under")
	flag.Parse()

	out := filepath.Join(*chartDir, alerts.RulesFile)
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "genrules: %v\n", err)
		os.Exit(1)
	}
	if err := os.WriteFile(out, alerts.Render(), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "genrules: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Wrote %s (%d rules)\n", out, len(alerts.Rules()))
}
