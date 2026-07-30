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
	"slices"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// descPattern pulls the fqName and the variable label list out of a Desc's String(). Desc keeps
// both unexported and offers no accessor, and String() is the only view of them client_golang
// gives — a narrow dependency, taken deliberately: the alternative is restating every label set a
// second time, which is the drift this whole file exists to prevent.
var descPattern = regexp.MustCompile(`Desc\{fqName: "([^"]+)", .*variableLabels: \{([^}]*)\}`)

// registeredDescs walks everything this package publishes — the scrape-time Collector and the
// event-driven Vecs registered in events.go's init — and returns each family's real label set.
func registeredDescs(t *testing.T) map[string][]string {
	t.Helper()

	collectors := []prometheus.Collector{
		NewCollector(newFakeClient(t), testOperatorNamespace),
		locksReaped,
		backupDuration, backupAddedTotal, backupFailuresTotal, backupTotal,
		clusterBackupDuration, clusterBackupRunsTotal,
		restoreDuration, restoreFailuresTotal,
		externalSyncDuration,
		erasureForgottenTotal, erasureReclaimedTotal,
		moverJobRetries, webhookDenials, exposureReadyWait,
	}

	out := map[string][]string{}
	for _, c := range collectors {
		ch := make(chan *prometheus.Desc, 128)
		go func() {
			c.Describe(ch)
			close(ch)
		}()
		for desc := range ch {
			m := descPattern.FindStringSubmatch(desc.String())
			if m == nil {
				t.Fatalf("cannot parse a Desc — client_golang changed its String() format: %s", desc)
			}
			var labels []string
			for _, l := range strings.Split(m[2], ",") {
				if l = strings.TrimSpace(l); l != "" {
					labels = append(labels, l)
				}
			}
			slices.Sort(labels)
			out[m[1]] = labels
		}
	}
	return out
}

// TestCatalogueMatchesRegisteredDescs is the guard that makes Catalogue() safe to trust from
// outside this package. internal/alerts validates every rule expression against it, so a family
// whose entry here is wrong — or missing — would hand the alert checker a label set the operator
// does not emit, and rubber-stamp exactly the kind of expression that matches nothing forever.
func TestCatalogueMatchesRegisteredDescs(t *testing.T) {
	real := registeredDescs(t)
	declared := Catalogue()

	for name, want := range real {
		got, ok := declared[name]
		if !ok {
			t.Errorf("%s is emitted but absent from Catalogue() — add it, or internal/alerts "+
				"cannot check any rule that reads it", name)
			continue
		}
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Errorf("%s: Catalogue() says labels %v, the collector emits %v", name, got, want)
		}
	}

	for name := range declared {
		if _, ok := real[name]; !ok {
			t.Errorf("Catalogue() declares %s, which nothing registers — a rule written against "+
				"it would be valid PromQL that can never fire", name)
		}
	}
}

// TestHistogramsAreHistograms keeps the derived-suffix list honest: a family listed in
// Histograms() that is not one would let a checker bless `..._bucket` on a gauge.
func TestHistogramsAreHistograms(t *testing.T) {
	declared := Catalogue()
	for _, name := range Histograms() {
		if _, ok := declared[name]; !ok {
			t.Errorf("Histograms() lists %s, which is not in Catalogue()", name)
		}
	}
	reg := prometheus.NewRegistry()
	for _, c := range []prometheus.Collector{
		backupDuration, clusterBackupDuration, restoreDuration, externalSyncDuration, exposureReadyWait,
	} {
		reg.MustRegister(c)
	}
}
