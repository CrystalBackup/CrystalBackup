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

package metrics

import (
	"regexp"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// The Prometheus base-unit convention says a metric holding a Unix time ends in
// `_timestamp_seconds`. This catalogue followed it unanimously — ten gauges across the backup,
// clusterbackup, restore, repository, discovery, erasure, schedule and externalsync families — and
// then the eleventh was added as `crystalbackup_backup_last_failure` and nothing noticed. It was
// caught by a human reading the diff, which is not a control.
//
// A metric name is permanent in a way almost nothing else in this repository is: it is compiled
// into somebody's dashboard, somebody's recording rule and somebody's alert, none of which are in
// this tree, and renaming it later breaks all three silently. So the convention gets a check.
//
// The invariant is between two things the AUTHOR writes, one of which already declares the unit:
// if a Desc's help text says "Unix time", its name must say `_timestamp_seconds`. That is what
// makes this checkable at all — there is no way to look at a float64 and know it is an epoch, but
// there is a way to notice that the person who wrote the help knew.
var (
	descFQName = regexp.MustCompile(`fqName: "([^"]+)"`)
	descHelp   = regexp.MustCompile(`help: "((?:[^"\\]|\\.)*)"`)
)

func TestUnixTimeGaugesAreNamedTimestampSeconds(t *testing.T) {
	descs := describeCollector(t)
	if len(descs) == 0 {
		t.Fatal("the collector described no metrics — this test would pass by checking nothing")
	}

	// The one series that declares a Unix time and must NOT carry the suffix, if such a thing
	// ever exists, belongs here with a reason. Empty is the honest state today.
	exempt := map[string]string{}

	var checked int
	for name, help := range descs {
		if !strings.Contains(strings.ToLower(help), "unix time") {
			continue
		}
		checked++
		if why, ok := exempt[name]; ok {
			t.Logf("%s is exempt from the timestamp suffix: %s", name, why)
			continue
		}
		if !strings.HasSuffix(name, "_timestamp_seconds") {
			t.Errorf("%s holds a Unix time (help: %q) but does not end in _timestamp_seconds.\n"+
				"Every other epoch gauge in this catalogue does, and the suffix is the Prometheus "+
				"base-unit convention. Rename the constant in names.go — a metric name cannot be "+
				"corrected after it ships into somebody's dashboard.", name, help)
		}
	}

	// Without this the test would go quiet the day the help texts are reworded, and a quiet test
	// that checks nothing is exactly the failure mode this file exists to prevent.
	if checked < 10 {
		t.Errorf("only %d Desc(s) declare a Unix time; there were 11 when this check was written. "+
			"Either the help texts stopped saying \"Unix time\" — in which case this check has "+
			"gone blind and needs a new hook — or epoch gauges were removed and this floor "+
			"should be lowered deliberately", checked)
	}
}

// TestEveryDescribedMetricIsInTheCatalogue is the other half: a series the collector emits but
// Catalogue() does not list is invisible to every consumer that reads the catalogue — the
// reference docs, and the label-set agreement the alert rules depend on.
func TestEveryDescribedMetricIsInTheCatalogue(t *testing.T) {
	catalogue := Catalogue()
	for name := range describeCollector(t) {
		if _, ok := catalogue[name]; !ok {
			t.Errorf("the collector describes %s but Catalogue() does not list it — it will be "+
				"absent from the generated reference docs, and nothing checks its label set "+
				"against the rules that read it", name)
		}
	}
}

// describeCollector returns fqName -> help for every Desc the state-derived collector publishes.
//
// It goes through Describe rather than Gather deliberately: Gather only returns series that some
// object happened to produce, so a metric with no matching state would silently not be checked.
// Describe is the collector's declaration of everything it can ever emit.
func describeCollector(t *testing.T) map[string]string {
	t.Helper()
	ch := make(chan *prometheus.Desc, 256)
	NewCollector(newFakeClient(t), testOperatorNamespace).Describe(ch)
	close(ch)

	out := map[string]string{}
	for d := range ch {
		s := d.String()
		name := descFQName.FindStringSubmatch(s)
		help := descHelp.FindStringSubmatch(s)
		if name == nil || help == nil {
			t.Fatalf("could not parse a Desc: %q\n"+
				"prometheus.Desc.String() changed shape; this test parses it and must be updated", s)
		}
		out[name[1]] = help[1]
	}
	return out
}
