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

// Command gendocs writes the website's Metrics and Alerts reference pages from internal/metrics
// and internal/alerts. Run it with `make observability-docs`; `make observability-docs-verify`
// fails the build when the committed pages no longer match, the same guard `make api-docs-verify`
// gives the generated API reference and `make alert-rules-verify` gives the chart's rule file.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/CrystalBackup/CrystalBackup/internal/refdocs"
)

func main() {
	root := flag.String("root", ".",
		"directory the page paths are written under (the repository root, or a scratch dir for the freshness check)")
	flag.Parse()

	families, err := refdocs.Families()
	if err != nil {
		fail(err)
	}
	metricsPage, err := refdocs.RenderMetrics(families)
	if err != nil {
		fail(err)
	}
	alertsPage, err := refdocs.RenderAlerts()
	if err != nil {
		fail(err)
	}

	for _, page := range []struct {
		path    string
		content []byte
	}{
		{refdocs.MetricsPage, metricsPage},
		{refdocs.AlertsPage, alertsPage},
	} {
		out := filepath.Join(*root, page.path)
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			fail(err)
		}
		if err := os.WriteFile(out, page.content, 0o644); err != nil {
			fail(err)
		}
		fmt.Printf("Wrote %s\n", out)
	}
	fmt.Printf("%d metric families, %d alert rules.\n", len(families), refdocs.RuleCount())
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "gendocs: %v\n", err)
	os.Exit(1)
}
