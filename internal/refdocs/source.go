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

// Package refdocs generates the website's Metrics and Alerts reference pages from the operator's
// own metric and alert declarations.
//
// # Why this is generated
//
// The same reason the API reference is (`make api-docs`), at the same scale of damage. Fifty-three
// series and eleven rules written by hand are fifty-three series and eleven rules that disagree
// with the operator within one release, and a metrics reference that disagrees with a scrape is
// worse than no reference at all: it sends someone to write an alert on a label that does not
// exist, which is valid PromQL that matches nothing, forever, in silence. That is precisely the
// failure M6 spent itself removing from the rules; reintroducing it in the documentation would be
// the same defect one step further from the code.
//
// # Where the facts come from
//
// Nothing here restates a name, a label, a help string, a bucket boundary, a threshold or an
// expression. Every one is read back out of the thing that publishes it:
//
//   - the metric families, their help text and their label sets come from the prometheus.Desc
//     values the collectors themselves register — the same Descs a scrape is served from;
//   - metrics.Catalogue() is checked against those Descs on the way through, so this generator
//     fails rather than documents a family whose exported catalogue entry has gone stale;
//   - histogram bucket boundaries are read from a real Gather of the registry, not typed out;
//   - the alert rules come from alerts.Rules(), including each rule's Rationale.
//
// The PROSE is written here, in Go, and that is deliberate too. The generated page is the whole
// file, so `make observability-docs-verify` can hold all of it — tables and explanation alike — to
// the tree. A hand-edited paragraph in a generated file is a paragraph nothing checks.
package refdocs

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/CrystalBackup/CrystalBackup/internal/metrics"
)

// Prefix every series this operator publishes carries. Everything else the controller-runtime
// registry holds (client-go's rest_client_*, the leader-election gauge) belongs to a dependency
// and is not ours to document.
const seriesPrefix = "crystalbackup_"

// Kind is the Prometheus metric type of a family.
type Kind string

const (
	KindGauge     Kind = "gauge"
	KindCounter   Kind = "counter"
	KindHistogram Kind = "histogram"
)

// Family is one crystalbackup_ metric family exactly as the operator publishes it.
type Family struct {
	Name   string
	Help   string
	Labels []string
	Kind   Kind
	// Buckets are the histogram's upper bounds, read from a real exposition. Nil for gauges and
	// counters.
	Buckets []float64
	// ScrapeDerived is true for a family the Collector recomputes from live object state on every
	// scrape, false for one incremented at the site of an event.
	//
	// It is HARVESTED (which collector describes the family), not inferred from the name, so that
	// buildFamilies can assert the correspondence the Metrics page states as a rule rather than
	// restating it as a hope. See the check at the bottom of buildFamilies.
	ScrapeDerived bool
}

// descPattern pulls the fqName, help and variable labels out of a Desc's String().
//
// prometheus.Desc keeps all three unexported and offers no accessor; String() is the only view
// client_golang gives. internal/metrics' own catalogue_export_test.go takes the identical
// dependency for the identical reason, and it is still the narrow choice: the alternative is
// typing fifty-three label sets and fifty-three help strings out a second time, which is the drift
// this package exists to prevent.
var descPattern = regexp.MustCompile(`^Desc\{fqName: "([^"]+)", help: (".*"), constLabels: \{[^}]*\}, variableLabels: \{([^}]*)\}\}$`)

// Families returns every crystalbackup_ family the operator publishes, ordered by name.
func Families() ([]Family, error) {
	scrape, err := describe(metrics.NewCollector(nil, ""))
	if err != nil {
		return nil, err
	}
	// The event-driven counters and histograms register themselves on the controller-runtime
	// registry in internal/metrics' init, so importing the package is what puts them here.
	registry, ok := ctrlmetrics.Registry.(prometheus.Collector)
	if !ok {
		return nil, fmt.Errorf("controller-runtime's metrics registry is no longer a prometheus.Collector; " +
			"the event-driven counters and histograms cannot be enumerated")
	}
	event, err := describe(registry)
	if err != nil {
		return nil, err
	}

	buckets, err := histogramBuckets()
	if err != nil {
		return nil, err
	}
	return buildFamilies(scrape, event, buckets)
}

// rawDesc is one parsed Desc, before the two sources are merged.
type rawDesc struct {
	name   string
	help   string
	labels []string
}

func describe(c prometheus.Collector) (map[string]rawDesc, error) {
	ch := make(chan *prometheus.Desc, 512)
	go func() {
		c.Describe(ch)
		close(ch)
	}()

	out := map[string]rawDesc{}
	for desc := range ch {
		m := descPattern.FindStringSubmatch(desc.String())
		if m == nil {
			return nil, fmt.Errorf("cannot parse a Desc — client_golang changed its String() format: %s", desc)
		}
		if !strings.HasPrefix(m[1], seriesPrefix) {
			continue
		}
		// Desc.String() renders the help through %q, so undo that rather than shipping a page
		// full of backslashes the first time somebody quotes something in a Help string.
		help, err := strconv.Unquote(m[2])
		if err != nil {
			return nil, fmt.Errorf("cannot unquote the help of %s: %w", m[1], err)
		}
		var labels []string
		for l := range strings.SplitSeq(m[3], ",") {
			if l = strings.TrimSpace(l); l != "" {
				labels = append(labels, l)
			}
		}
		out[m[1]] = rawDesc{name: m[1], help: help, labels: labels}
	}
	return out, nil
}

// histogramBuckets reads each histogram family's real upper bounds.
//
// The bucket slices are unexported and a Desc does not carry them, so the only honest way to learn
// them is to make the registry expose one child per histogram and read the exposition back. That
// is what the Record* calls below are for: they are the operator's own recording entry points,
// called once with zero-valued series, purely so a Gather has something to render. Typing the
// boundaries out instead would put a second copy of durationBuckets in this file — the exact
// species of drift the whole package is built to prevent, and the one a reader of the page would
// have no way to detect.
func histogramBuckets() (map[string][]float64, error) {
	ctx := context.Background()
	metrics.RecordBackupTerminal(ctx, metrics.BackupSeries{}, "Completed", 0, 0)
	metrics.RecordClusterBackupTerminal(ctx, metrics.ClusterBackupSeries{}, "Completed", 0)
	metrics.RecordRestoreTerminal(ctx, metrics.RestoreSeries{}, "", "Completed", 0)
	metrics.RecordExternalSyncDuration(ctx, metrics.ExternalSyncSeries{}, 0)
	metrics.RecordExposureReadyWait(ctx, "", "", "", "", 0)

	gathered, err := ctrlmetrics.Registry.Gather()
	if err != nil {
		return nil, fmt.Errorf("gather the metrics registry: %w", err)
	}
	out := map[string][]float64{}
	for _, f := range gathered {
		if f.GetType() != dto.MetricType_HISTOGRAM || len(f.GetMetric()) == 0 {
			continue
		}
		var bounds []float64
		for _, b := range f.GetMetric()[0].GetHistogram().GetBucket() {
			bounds = append(bounds, b.GetUpperBound())
		}
		out[f.GetName()] = bounds
	}
	return out, nil
}

// buildFamilies merges the two harvests, checks them against metrics.Catalogue(), and settles each
// family's type.
func buildFamilies(scrape, event map[string]rawDesc, buckets map[string][]float64) ([]Family, error) {
	catalogue := metrics.Catalogue()
	histograms := metrics.Histograms()

	all := map[string]Family{}
	add := func(d rawDesc, scrapeDerived bool) error {
		declared, ok := catalogue[d.name]
		if !ok {
			return fmt.Errorf("%s is published but absent from metrics.Catalogue(); this page would "+
				"document a label set nothing verifies", d.name)
		}
		if !slices.Equal(declared, d.labels) {
			return fmt.Errorf("%s: metrics.Catalogue() says labels %v, the collector publishes %v",
				d.name, declared, d.labels)
		}

		kind := KindGauge
		switch {
		case slices.Contains(histograms, d.name):
			kind = KindHistogram
		case strings.HasSuffix(d.name, "_total"):
			kind = KindCounter
		}
		f := Family{
			Name: d.name, Help: d.help, Labels: d.labels,
			Kind: kind, ScrapeDerived: scrapeDerived,
		}
		if kind == KindHistogram {
			if f.Buckets = buckets[d.name]; len(f.Buckets) == 0 {
				return fmt.Errorf("%s is a histogram but exposed no buckets; histogramBuckets() no "+
					"longer creates a child for it", d.name)
			}
		}
		all[d.name] = f
		return nil
	}

	for _, d := range scrape {
		if err := add(d, true); err != nil {
			return nil, err
		}
	}
	for _, d := range event {
		if _, dup := all[d.name]; dup {
			return nil, fmt.Errorf("%s is published by both the scrape collector and an event-driven "+
				"vec; the page cannot say which resets on restart", d.name)
		}
		if err := add(d, false); err != nil {
			return nil, err
		}
	}

	for name := range catalogue {
		if _, ok := all[name]; !ok {
			return nil, fmt.Errorf("metrics.Catalogue() declares %s, which nothing publishes", name)
		}
	}

	// The one claim the Metrics page makes about the catalogue AS A WHOLE: `_total` families and
	// histograms are real counters that reset when the operator restarts, and everything else is
	// recomputed from object state at scrape time and does not. Asserting it here is what lets the
	// page state it as a rule instead of hedging — the day someone registers a gauge as an
	// event-driven vec, this generator fails and the sentence never ships wrong.
	for _, f := range all {
		if f.ScrapeDerived != (f.Kind == KindGauge) {
			return nil, fmt.Errorf("%s is a %s and %s; the Metrics page's rule that "+
				"counters/histograms are event-driven and every other family is derived at scrape "+
				"no longer holds, so the prose in metrics.go must be rewritten before regenerating",
				f.Name, f.Kind, scrapeWord(f.ScrapeDerived))
		}
	}

	out := make([]Family, 0, len(all))
	for _, f := range all {
		out = append(out, f)
	}
	slices.SortFunc(out, func(a, b Family) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

func scrapeWord(scrapeDerived bool) string {
	if scrapeDerived {
		return "derived at scrape"
	}
	return "event-driven"
}

// promDuration formats a duration the way Prometheus and the rule file write them. Mirrors
// alerts.promDuration, which is unexported; a rule file's `for: 15m` must read as `15m` on the
// page too, not as Go's `15m0s`.
func promDuration(d time.Duration) string {
	switch {
	case d == 0:
		return "0s"
	case d%(24*time.Hour) == 0:
		return fmt.Sprintf("%dd", int64(d/(24*time.Hour)))
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", int64(d/time.Hour))
	case d%time.Minute == 0:
		return fmt.Sprintf("%dm", int64(d/time.Minute))
	default:
		return fmt.Sprintf("%ds", int64(d/time.Second))
	}
}
