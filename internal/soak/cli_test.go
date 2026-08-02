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

package soak

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestSoakCollectRefusesToStartBlind is §1, at the level an operator experiences it.
//
// "A collector that starts happily and collects nothing is the failure mode this whole kit exists
// to avoid, and it must not be possible to reach it by accident." Each of these is a REFUSAL and
// not a warning, because a warning at startup is a line in a log nobody reads on day one and
// nobody can find on day fourteen — by which point the fortnight cannot be re-run.
func TestSoakCollectRefusesToStartBlind(t *testing.T) {
	for _, tc := range []struct {
		name     string
		args     []string
		wantCode int
		wantSaid string
	}{
		{
			"no --metrics-url", []string{"--data-dir=" + t.TempDir()}, 2,
			"--metrics-url is required",
		},
		{
			"a --max-bytes that is not a size",
			[]string{"--data-dir=" + t.TempDir(), "--metrics-url=https://x/metrics", "--max-bytes=lots"},
			2, "not a size",
		},
		{
			"a zero scrape interval",
			[]string{"--data-dir=" + t.TempDir(), "--metrics-url=https://x/metrics", "--metrics-interval=0"},
			2, "must be positive",
		},
		{
			"a salt file that is not there",
			[]string{"--data-dir=" + t.TempDir(), "--metrics-url=https://x/metrics",
				"--salt-method=from-secret", "--redaction-salt-file=/nonexistent/salt"},
			2, "read redaction salt file",
		},
		{
			// A silent fallback to the derived salt here would produce an archive claiming one
			// guarantee and holding another — the defect the no-fallback rule exists to prevent.
			"fromSecret with no Secret named",
			[]string{"--data-dir=" + t.TempDir(), "--metrics-url=https://x/metrics",
				"--salt-method=from-secret"},
			2, "needs --redaction-salt-file",
		},
		{
			"an unknown salt method",
			[]string{"--data-dir=" + t.TempDir(), "--metrics-url=https://x/metrics",
				"--salt-method=random"},
			2, `valid values are "auto" and "from-secret"`,
		},
		{
			// The Secret is mounted, the flag is set, the manifest looks like a fromSecret
			// collector — and the salt would come from the namespace UID instead, invisibly.
			"a salt file under the derived method",
			[]string{"--data-dir=" + t.TempDir(), "--metrics-url=https://x/metrics",
				"--redaction-salt-file=/etc/crystal-backup/soak/salt"},
			2, "would IGNORE that file",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stderr bytes.Buffer
			got := RunCollect(t.Context(), tc.args, &stderr)
			if got != tc.wantCode {
				t.Errorf("exit = %d, want %d\n%s", got, tc.wantCode, stderr.String())
			}
			if !strings.Contains(stderr.String(), tc.wantSaid) {
				t.Errorf("stderr does not say %q:\n%s", tc.wantSaid, stderr.String())
			}
		})
	}
}

// TestProveScrapeRefusesAndSaysWhichFailureItWas is §1's third refusal.
//
// Three attempts, because the one benign failure is a collector pod that started before the
// operator finished coming up. The message has to name the four things that are actually wrong
// when this happens, because the failures are one ClusterRoleBinding, one flag and one
// NetworkPolicy apart and they look identical from the outside.
func TestProveScrapeRefusesAndSaysWhichFailureItWas(t *testing.T) {
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("Unauthorized"))
	}))
	defer srv.Close()

	defer func(d time.Duration) { startupScrapeBackoff = d }(startupScrapeBackoff)
	startupScrapeBackoff = time.Millisecond

	s := NewScraper(srv.URL, true)
	s.TokenPath = ""
	var stderr bytes.Buffer
	err := proveScrape(t.Context(), s, &stderr)
	if err == nil {
		t.Fatal("proveScrape accepted an endpoint that answered 401 every time. This collector " +
			"would run for a fortnight and produce an archive with no metrics in it")
	}
	if attempts != startupScrapeAttempts {
		t.Errorf("%d attempt(s), want %d", attempts, startupScrapeAttempts)
	}
	for _, want := range []string{"401", "crystal-backup-metrics-reader",
		"--metrics-insecure-skip-verify", "NetworkPolicy", "Refusing to start"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q:\n%v", want, err)
		}
	}
}

func TestProveScrapeAcceptsAWorkingEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("# TYPE up gauge\nup 1\n"))
	}))
	defer srv.Close()
	s := NewScraper(srv.URL, true)
	s.TokenPath = ""
	if err := proveScrape(t.Context(), s, &bytes.Buffer{}); err != nil {
		t.Fatalf("proveScrape refused a working endpoint: %v", err)
	}
}

// TestStatusNeedsNoSalt. hack/soak/collect.sh runs `soak-export --status` as its very first
// interaction with the collector and treats ANY non-zero exit as "this build has no
// soak-export" — a NOT COLLECTED verdict that sends the admin down the fallback-CronJob path.
// Reaching that conclusion because a Secret was mounted at the wrong path would be wrong twice.
func TestStatusNeedsNoSalt(t *testing.T) {
	s := seedCollector(t, 2, true)
	var stdout, stderr bytes.Buffer
	code := RunExport([]string{
		"--data-dir=" + s.Dir(), "--status", "--redaction-salt-file=/nonexistent/salt",
	}, &stdout, &stderr)
	if code != 0 {
		t.Errorf("exit = %d, want 0\n%s", code, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("--status wrote %d byte(s) to STDOUT; only the tarball may ever go there, and "+
			"collect.sh redirects stdout to the archive file", stdout.Len())
	}
	if !strings.Contains(stderr.String(), "status at") {
		t.Errorf("the status screen was not written to stderr:\n%s", stderr.String())
	}
}

// TestUsageNamesEveryFlagTheManifestPasses is the cheap guard on the one coupling nobody would
// notice breaking: charts/crystal-backup/templates/soak.yaml passes these by name, and a renamed
// flag makes the Deployment CrashLoop with "flag provided but not defined" — on the customer's
// cluster, on day one, after they have already installed everything. test/chart asserts the other
// half, that each flag carries the value its chart value was set to.
func TestUsageNamesEveryFlagTheManifestPasses(t *testing.T) {
	for _, flag := range []string{
		"--data-dir", "--max-bytes", "--metrics-url", "--metrics-insecure-skip-verify",
		"--metrics-interval", "--metrics-resolution", "--mover-sample-interval",
		"--selfcheck-interval", "--state-interval", "--salt-method", "--redaction-salt-file",
		"--kubelet-stats",
		"--heartbeat-check", "--full", "--since", "--status", "--operator-namespace",
	} {
		if !strings.Contains(Usage, flag) {
			t.Errorf("%s is not in the usage text", flag)
		}
	}
	// And the subcommand names cmd/main.go dispatches on.
	for _, cmd := range []string{CommandCollect, CommandExport} {
		if !strings.Contains(Usage, cmd) {
			t.Errorf("%s is not in the usage text", cmd)
		}
	}
}

// TestHeartbeatCheckNeedsNoClusterAndPrintsNothing. The liveness probe runs every five minutes
// for a fortnight in a distroless container: it must be a read of one small document, it must not
// build a Kubernetes client, and it must not write to stdout — a probe that printed would just
// fill the kubelet's event log.
func TestHeartbeatCheckNeedsNoClusterAndPrintsNothing(t *testing.T) {
	var stderr bytes.Buffer
	code := RunCollect(t.Context(), []string{
		"--heartbeat-check", "--data-dir=" + t.TempDir(),
	}, &stderr)
	if code == 0 {
		t.Error("a data directory with no heartbeat was reported healthy")
	}
	if stderr.Len() != 0 {
		t.Errorf("--heartbeat-check wrote to stderr: %q", stderr.String())
	}
}
