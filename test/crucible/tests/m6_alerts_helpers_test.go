//go:build crucible

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

package crucible

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// ---------------------------------------------------------------------------
// Talking to the crucible's Prometheus.
//
// Through the API server's service proxy, not a port-forward and not a NodePort. A port-forward is
// a goroutine that has to be kept alive and torn down and that fails in ways indistinguishable from
// "Prometheus is down"; a NodePort is a Prometheus HTTP API open on a public Hetzner IP for the
// life of the run. The proxy subresource is one authenticated GET over the kubeconfig the suite
// already holds, and it fails loudly.
//
// There is NO fallback and no toggle. If Prometheus cannot be reached, these specs fail — the whole
// point of lot I is that nothing in this suite is allowed to quietly measure less. Three
// self-disabling guards were removed from this suite in the two milestones before this one.
// ---------------------------------------------------------------------------

const (
	m6MonitoringNS  = "monitoring"
	m6PrometheusSvc = "crucible-prometheus"
	m6PrometheusPrt = "web"
)

// m6PromAPI GETs a Prometheus HTTP API path and returns the decoded `data` field.
//
// It asserts on the envelope (`status: success`) rather than returning it, because every caller
// would otherwise repeat the same three lines and one of them would eventually forget.
func m6PromAPI(path string, params map[string]string, out any) error {
	raw, err := k8sTyped.CoreV1().Services(m6MonitoringNS).
		ProxyGet("http", m6PrometheusSvc, m6PrometheusPrt, path, params).
		DoRaw(ctx)
	if err != nil {
		return fmt.Errorf("GET %s via the API-server proxy to %s/%s: %w",
			path, m6MonitoringNS, m6PrometheusSvc, err)
	}
	var env struct {
		Status string          `json:"status"`
		Error  string          `json:"error"`
		Data   json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("decode %s: %w (body: %.400s)", path, err, string(raw))
	}
	if env.Status != "success" {
		return fmt.Errorf("%s returned status=%q: %s", path, env.Status, env.Error)
	}
	return json.Unmarshal(env.Data, out)
}

// m6Alert is one entry of /api/v1/alerts.
type m6Alert struct {
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	State       string            `json:"state"` // pending | firing
	ActiveAt    time.Time         `json:"activeAt"`
	Value       string            `json:"value"`
}

func (a m6Alert) String() string {
	keys := make([]string, 0, len(a.Labels))
	for k := range a.Labels {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+a.Labels[k])
	}
	return fmt.Sprintf("%s{%s} state=%s value=%s", a.Labels["alertname"], strings.Join(parts, ","), a.State, a.Value)
}

// m6Alerts returns every alert Prometheus currently has in flight, pending as well as firing.
// Pending ones are included on purpose: "pending but not firing" and "not there at all" are
// completely different diagnoses when a spec times out, and a helper that filtered them away would
// throw the useful half of the answer on the floor.
func m6Alerts() ([]m6Alert, error) {
	var data struct {
		Alerts []m6Alert `json:"alerts"`
	}
	if err := m6PromAPI("/api/v1/alerts", nil, &data); err != nil {
		return nil, err
	}
	return data.Alerts, nil
}

// m6AlertsNamed filters to one alertname, further narrowed by any label pairs given. The label
// match is what keeps a spec honest on a shared cluster: `CrystalbackupRepositoryCheckFailed is
// firing` is a much weaker statement than `it is firing for the location this spec corrupted`, and
// the weaker one passes even when the alert is about somebody else's repository.
func m6AlertsNamed(alerts []m6Alert, name string, match map[string]string) []m6Alert {
	var out []m6Alert
next:
	for _, a := range alerts {
		if a.Labels["alertname"] != name {
			continue
		}
		for k, v := range match {
			if a.Labels[k] != v {
				continue next
			}
		}
		out = append(out, a)
	}
	return out
}

// m6DescribeAlerts renders the live alert set for a failure message. A spec that times out waiting
// for an alert should say what Prometheus DID have, so the reader can tell "the condition never
// happened" from "it happened and the rule did not match it".
func m6DescribeAlerts(alerts []m6Alert) string {
	if len(alerts) == 0 {
		return "<no alerts of any kind in Prometheus>"
	}
	lines := make([]string, 0, len(alerts))
	for _, a := range alerts {
		lines = append(lines, "  "+a.String())
	}
	slices.Sort(lines)
	return "\n" + strings.Join(lines, "\n")
}

// m6WaitAlertFiring blocks until an alert with the given name and labels is FIRING.
//
// `firing`, never `pending`: pending means the expression matched once, which is exactly the state
// a rule with a broken `for:` gets stuck in forever. Accepting it would make the wait cheaper and
// the assertion meaningless.
func m6WaitAlertFiring(name string, match map[string]string, timeout time.Duration) m6Alert {
	GinkgoHelper()
	var found m6Alert
	Eventually(func(g Gomega) {
		all, err := m6Alerts()
		g.Expect(err).NotTo(HaveOccurred(),
			"Prometheus must be reachable for this spec to mean anything")
		hits := m6AlertsNamed(all, name, match)
		for _, a := range hits {
			if a.State == "firing" {
				found = a
				return
			}
		}
		g.Expect(false).To(BeTrue(),
			"%s%v is not firing. Live alerts: %s", name, match, m6DescribeAlerts(all))
	}, timeout, 15*time.Second).Should(Succeed())
	return found
}

// m6Rule is one entry of /api/v1/rules, flattened to the fields that matter here.
type m6Rule struct {
	Name      string            `json:"name"`
	Query     string            `json:"query"`
	Type      string            `json:"type"`
	Health    string            `json:"health"`
	LastError string            `json:"lastError"`
	Labels    map[string]string `json:"labels"`
}

// m6LoadedRules returns the alerting rules Prometheus has actually LOADED, keyed by alert name.
//
// This is the assertion that the chart's PrometheusRule was picked up at all. A PrometheusRule
// installed into a cluster whose Prometheus does not select it is valid, present, and completely
// inert — the failure mode the chart's own values.yaml warns about for `metrics.rules.labels`, and
// one that no amount of `helm template` can detect.
func m6LoadedRules() (map[string]m6Rule, error) {
	var data struct {
		Groups []struct {
			Name  string   `json:"name"`
			File  string   `json:"file"`
			Rules []m6Rule `json:"rules"`
		} `json:"groups"`
	}
	if err := m6PromAPI("/api/v1/rules", map[string]string{"type": "alert"}, &data); err != nil {
		return nil, err
	}
	out := map[string]m6Rule{}
	for _, g := range data.Groups {
		for _, r := range g.Rules {
			out[r.Name] = r
		}
	}
	return out, nil
}

// m6Target is one entry of /api/v1/targets, narrowed to what a scrape verdict needs.
type m6Target struct {
	Labels     map[string]string `json:"labels"`
	ScrapePool string            `json:"scrapePool"`
	Health     string            `json:"health"`
	LastError  string            `json:"lastError"`
}

// m6ActiveTargets returns Prometheus's active scrape targets.
func m6ActiveTargets() ([]m6Target, error) {
	var data struct {
		ActiveTargets []m6Target `json:"activeTargets"`
	}
	if err := m6PromAPI("/api/v1/targets", map[string]string{"state": "active"}, &data); err != nil {
		return nil, err
	}
	return data.ActiveTargets, nil
}

// m6Sample is one (labels, value) pair from an instant query.
type m6Sample struct {
	Metric map[string]string `json:"metric"`
	Value  []any             `json:"value"`
}

// m6AllCrystalbackupSeries returns every crystalbackup_ series Prometheus holds AT THIS INSTANT,
// with its full label set.
//
// An instant query, and NOT /api/v1/labels — which is the instrument the obvious version of this
// check would reach for and which would be wrong. The labels endpoint answers "which label names
// exist anywhere in this TSDB", block-granular: on the crucible it still listed
// `exported_namespace` after the ServiceMonitor was fixed and the operator redeployed, because
// series scraped before the fix were still inside the 6h retention window — and it kept listing it
// when the query was narrowed to the last two minutes. A gate built on that endpoint goes red
// forever the first time the bug occurs, including on the run that proves it fixed, which is the
// most reliable way to get a gate deleted.
//
// The instant vector is the honest question: what is the CURRENT scrape putting in the database.
func m6AllCrystalbackupSeries() ([]m6Sample, error) {
	return m6Query(`{__name__=~"crystalbackup_.+"}`)
}

// m6Query runs an instant PromQL query and returns the vector result.
func m6Query(expr string) ([]m6Sample, error) {
	var data struct {
		ResultType string     `json:"resultType"`
		Result     []m6Sample `json:"result"`
	}
	if err := m6PromAPI("/api/v1/query", map[string]string{"query": expr}, &data); err != nil {
		return nil, err
	}
	if data.ResultType != "vector" {
		return nil, fmt.Errorf("query %q returned %s, expected a vector", expr, data.ResultType)
	}
	return data.Result, nil
}

// m6PositiveSamples keeps only the samples whose value is strictly greater than zero.
//
// It exists because "the query returned something" and "the query returned a number that means
// yes" are different questions, and PromQL makes it very easy to ask the first while believing you
// asked the second. An `increase()` over a series that EXISTS in the window but never incremented
// yields a sample — value 0. A `Expect(samples).NotTo(BeEmpty())` written over that is satisfied by
// zero, so it certifies the counter moved at the exact moment it provably did not.
//
// That is not hypothetical: it is how the 0.6.0 campaign's alert lane reported "And the counter
// series moves" as passing, immediately before CrystalbackupBackupFailed timed out ten minutes
// later. Every value assertion in this file goes through here or compares the value explicitly.
func m6PositiveSamples(samples []m6Sample) []m6Sample {
	var out []m6Sample
	for _, s := range samples {
		v, err := m6SampleValue(s)
		if err != nil || v <= 0 {
			continue
		}
		out = append(out, s)
	}
	return out
}

// m6SampleValue parses the numeric half of a sample. Prometheus encodes it as a STRING (so that
// NaN and ±Inf survive JSON), which is why this cannot be a type assertion on a float64.
func m6SampleValue(s m6Sample) (float64, error) {
	if len(s.Value) != 2 {
		return 0, fmt.Errorf("sample has %d value fields, expected [timestamp, value]", len(s.Value))
	}
	raw, ok := s.Value[1].(string)
	if !ok {
		return 0, fmt.Errorf("sample value is %T, expected the string Prometheus encodes", s.Value[1])
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, fmt.Errorf("sample value %q is not a number: %w", raw, err)
	}
	return v, nil
}

// m6DescribeSamples renders a result vector for a failure message: the values, with the labels that
// distinguish them. A bare "no positive sample" leaves the reader unable to tell "the series is
// absent" from "the series is there and reads zero" — which are opposite diagnoses.
func m6DescribeSamples(samples []m6Sample) string {
	if len(samples) == 0 {
		return "an EMPTY vector (no such series at all)"
	}
	parts := make([]string, 0, len(samples))
	for _, s := range samples {
		v, err := m6SampleValue(s)
		if err != nil {
			parts = append(parts, fmt.Sprintf("%v=<unparseable: %v>", s.Metric, err))
			continue
		}
		parts = append(parts, fmt.Sprintf("%v=%g", s.Metric, v))
	}
	return strings.Join(parts, ", ")
}

// m6RequirePrometheus fails the spec unless Prometheus answers and is scraping the operator.
//
// Run before anything is provoked, and that ordering is the point: an alert spec that discovers a
// broken scrape only after spending twenty minutes corrupting a repository has spent twenty minutes
// to produce a confusing failure. Here the message says which of the two things is wrong.
func m6RequirePrometheus() {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		targets, err := m6ActiveTargets()
		g.Expect(err).NotTo(HaveOccurred(),
			"the crucible's Prometheus (%s/%s) must be reachable through the API-server proxy — "+
				"deploy/deploy.sh installs it; a run without it cannot certify any alert",
			m6MonitoringNS, m6PrometheusSvc)

		var ours []m6Target
		for _, t := range targets {
			if strings.Contains(t.ScrapePool, "crystal-backup") ||
				t.Labels["namespace"] == operatorNS {
				ours = append(ours, t)
			}
		}
		g.Expect(ours).NotTo(BeEmpty(),
			"Prometheus has no scrape target for crystal-backup — the chart's ServiceMonitor was not "+
				"selected (metrics.serviceMonitor.enabled, or the Prometheus serviceMonitorSelector). "+
				"Active pools: %v", m6ScrapePools(targets))
		for _, t := range ours {
			g.Expect(t.Health).To(Equal("up"),
				"the crystal-backup metrics target is %s: %s — this is the scrape path itself "+
					"(metrics-reader RBAC, the endpoint's TLS, or the operator NetworkPolicy's "+
					"monitoringNamespace), not an alert-rule problem", t.Health, t.LastError)
		}
	}, 5*time.Minute, 10*time.Second).Should(Succeed())
}

func m6ScrapePools(targets []m6Target) []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range targets {
		if !seen[t.ScrapePool] {
			seen[t.ScrapePool] = true
			out = append(out, t.ScrapePool)
		}
	}
	slices.Sort(out)
	return out
}

// m6ShippedAlertNames is the list of alert names the chart's generated rule file contains, as of
// the milestone that added them. It is written out rather than parsed from the YAML on purpose: the
// spec that uses it is checking that PROMETHEUS loaded what the chart ships, and reading the same
// file the chart reads would only prove the file is self-consistent.
//
// Adding a rule to internal/alerts/rules.go and not to this list makes the crucible red. That is
// the intent — `make alert-rules-covered` says the same thing about the promtool tests.
var m6ShippedAlertNames = []string{
	"CrystalbackupBackupMissed",
	"CrystalbackupBackupFailed",
	"CrystalbackupRepositoryCheckFailed",
	"CrystalbackupStaleLocks",
	"CrystalbackupMaintenanceStalled",
	"CrystalbackupDiscoveryFailed",
	"CrystalbackupErasureBlocked",
	"CrystalbackupPVCSnapshotPileup",
	"CrystalbackupExternalSyncStale",
	"CrystalbackupSchedulePausedTooLong",
	"CrystalbackupExternalSyncPausedTooLong",
}
