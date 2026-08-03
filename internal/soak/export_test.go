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
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/hex"
	"encoding/json"
	"io"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/CrystalBackup/CrystalBackup/internal/metrics"
	"github.com/CrystalBackup/CrystalBackup/internal/mover"
)

// exportSalt is 32 bytes and is not the known-answer vector, so a test that used the wrong salt
// could not pass by coincidence.
var exportSalt = []byte("soak-export-test-salt-0123456789")

// archive is one unpacked export, so the assertions below read as questions about the file the
// admin would actually send.
type archive struct {
	files    map[string][]byte
	manifest Manifest
	raw      []byte
}

func exportToArchive(t *testing.T, opts ExportOptions) archive {
	t.Helper()
	var out bytes.Buffer
	var progress bytes.Buffer
	if err := Export(opts, &out, &progress); err != nil {
		t.Fatalf("Export: %v", err)
	}
	// The contract with hack/soak/collect.sh: the bytes on stdout are a valid gzip stream, and
	// nothing else ever writes there. The script checks this with `gzip -t` and reports a
	// NOT COLLECTED with the first bytes quoted when it fails, because that is the only way
	// anyone would work out that something Printf'd inside the container.
	zr, err := gzip.NewReader(bytes.NewReader(out.Bytes()))
	if err != nil {
		t.Fatalf("stdout is not a valid gzip stream (something else wrote to it): %v\nfirst bytes: %q",
			err, out.Bytes()[:min(80, out.Len())])
	}
	a := archive{files: map[string][]byte{}, raw: out.Bytes()}
	tr := tar.NewReader(zr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("the archive will not unpack: %v", err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		a.files[hdr.Name] = body
	}
	body, ok := a.files["MANIFEST.json"]
	if !ok {
		t.Fatalf("no MANIFEST.json in the archive (%v). collect.sh derives its ENTIRE verdict from "+
			"it and reports its absence as \"nothing in it can be judged complete or incomplete\"",
			keysOfBytes(a.files))
	}
	if err := json.Unmarshal(body, &a.manifest); err != nil {
		t.Fatalf("MANIFEST.json does not parse: %v\n%s", err, body)
	}
	return a
}

func (a archive) stream(t *testing.T, name string) StreamReport {
	t.Helper()
	for _, s := range a.manifest.Streams {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("no %q stream in the manifest; collect.sh grades one row per stream and cannot report "+
		"one that is not there", name)
	return StreamReport{}
}

// seedCollector lays down what a collector would have written, so the export can be exercised
// without a cluster.
func seedCollector(t *testing.T, days int, selfcheckEnabled bool) *Store {
	t.Helper()
	s := newTestStore(t, 1<<30)
	start := day0

	info := CollectorInfo{
		OperatorVersion: "0.6.1", StartedAt: start, OperatorNamespace: "crystal-backup-system",
		MetricsURL:      "https://crystal-backup-metrics.crystal-backup-system.svc:8443/metrics",
		MetricsInterval: "1m0s", MetricsResolution: "5m0s", MoverSampleInterval: "15s",
		SelfcheckInterval: "24h0m0s", StateInterval: "1h0m0s",
		MaxBytes: 1 << 29, SelfcheckEnabled: selfcheckEnabled,
	}
	writeJSON(t, s, fileCollectorID, info)

	// One session, on the build collector-id.json names. A real collector records the version per
	// session, and a fixture that left it out would exercise the pre-field shape in every test
	// that uses it rather than the one a collector writes today.
	sessions := []Session{{
		StartedAt: start, LastBeat: start.AddDate(0, 0, days), BeatPeriod: "15s",
		OperatorVersion: info.OperatorVersion,
	}}
	writeJSON(t, s, fileStarts, sessions)

	for d := range days {
		day := start.AddDate(0, 0, d)
		core := make([][]byte, 0, 576)
		bulk := make([][]byte, 0, 288)
		for w := range 288 { // one full day of five-minute windows
			at := day.Add(time.Duration(w) * 5 * time.Minute)
			core = append(core, mustJSON(t, scrapeHealth{
				T: at.Unix(), Name: scrapeHealthName, OK: 5,
			}))
			core = append(core, mustJSON(t, point{
				T: at.Unix(), Name: "crystalbackup_repository_size_bytes",
				Labels: map[string]string{"location": "primary-eu"},
				Last:   float64(1000 + w), Min: 1000, Max: 2000, N: 5,
			}))
			bulk = append(bulk, mustJSON(t, point{
				T: at.Unix(), Name: "crystalbackup_schedule_period_seconds",
				Labels: map[string]string{"namespace": "acme-prod", "schedule": "nightly"},
				Last:   86400, Min: 86400, Max: 86400, N: 5,
			}))
		}
		if err := s.Append(streamMetricsCore, day, core); err != nil {
			t.Fatal(err)
		}
		if err := s.Append(StreamMetrics, day, bulk); err != nil {
			t.Fatal(err)
		}

		state := make([][]byte, 0, 24)
		for h := range 24 {
			state = append(state, mustJSON(t, StateSnapshot{
				At: day.Add(time.Duration(h) * time.Hour), Kind: kindBackup,
				Namespace: "acme-prod", Name: "nightly-20260601",
				Status: json.RawMessage(`{"phase":"Succeeded","message":"backed up acme-prod/pgdata"}`),
			}))
		}
		if err := s.Append(StreamState, day, state); err != nil {
			t.Fatal(err)
		}

		// Written TWICE, as a collector that restarted inside the events' one-hour API lifetime
		// would have written it: the archive must carry it once.
		warning := mustJSON(t, Event{
			At: day.Add(3 * time.Hour), UID: "uid-" + day.Format(dayLayout), Namespace: "acme-prod",
			Kind: "PersistentVolumeClaim", Name: "pgdata", Reason: "ProvisioningFailed", Count: 1,
			Message: "failed to provision volume pgdata in namespace acme-prod on node worker-3.internal",
		})
		if err := s.Append(StreamEvents, day, [][]byte{warning, warning}); err != nil {
			t.Fatal(err)
		}
		if err := s.Append(StreamLogs, day, [][]byte{mustJSON(t, LogLine{
			At: day.Add(4 * time.Hour), Pod: "crystal-backup-7d9f-abcde",
			Line: `{"level":"error","msg":"backup failed for acme-prod/pgdata against primary-eu"}`,
		})}); err != nil {
			t.Fatal(err)
		}

		if selfcheckEnabled {
			writeRaw(t, s, dirSelfcheck+"/"+day.Format(time.RFC3339)+".json",
				[]byte(`{"reportVersion":1,"inventory":{"namespaces":["ns-deadbeef"]}}`))
		}
	}

	writeJSON(t, s, fileMarks, Marks{
		UpdatedAt: start.AddDate(0, 0, days), SampleInterval: "15s",
		MetricsServer: MeasuredValue{Status: statusOK},
		KubeletStats:  MeasuredValue{Status: statusOK},
		Classes: map[string]ClassMarks{"data": {
			Class: "data", Pods: 3, Memory: MeasuredValue{Status: statusOK},
			PeakMemoryBytes: 2 << 30, PeakMemoryPod: "job-backup-xyz", PeakSamples: 44,
			Cache: MeasuredValue{Status: statusOK}, CacheHighWaterBytes: 3 << 30,
		}},
		LongestByOperation: map[string]float64{"backup": 660},
		Pods: []PodMark{{
			Pod: "job-backup-xyz", Namespace: "crystal-backup-system", Node: "worker-3.internal",
			Job: "crystal-backup-backup-acme-prod", Operation: "backup", Class: "data",
			PeakMemoryBytes: 2 << 30, Samples: 44,
			EvictionMessage: `Usage of EmptyDir volume "restic-cache" exceeds the limit "20Gi"`,
		}},
	})
	return s
}

func writeJSON(t *testing.T, s *Store, rel string, v any) {
	t.Helper()
	writeRaw(t, s, rel, mustJSON(t, v))
}

func writeRaw(t *testing.T, s *Store, rel string, body []byte) {
	t.Helper()
	if err := s.WriteFileAtomic(rel, body); err != nil {
		t.Fatal(err)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func keysOfBytes(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestExportProducesTheManifestCollectShReads is the contract test.
//
// hack/soak/collect.sh derives its ENTIRE verdict from MANIFEST.json, by name, field by field. A
// deviation here does not fail loudly — it makes that script misgrade a real soak, quietly, in
// the direction of flattery, on a customer's cluster after fourteen days that cannot be re-run.
func TestExportProducesTheManifestCollectShReads(t *testing.T) {
	s := seedCollector(t, 14, true)
	a := exportToArchive(t, ExportOptions{
		DataDir: s.Dir(), Salt: exportSalt, Now: day0.AddDate(0, 0, 14),
	})

	if a.manifest.Schema != ManifestSchema {
		t.Errorf("schema = %q, want %q", a.manifest.Schema, ManifestSchema)
	}
	if a.manifest.Redaction.Mode != "hashed" {
		t.Errorf("redaction.mode = %q, want hashed (collect.sh reports \"full\" as a FINDING)",
			a.manifest.Redaction.Mode)
	}
	if a.manifest.Redaction.SaltDisclosed {
		t.Error("redaction.saltDisclosed is true")
	}
	if a.manifest.UnredactedNote == "" {
		t.Error("unredactedNote is empty; collect.sh prints it as step 1 of \"before you send it\"")
	}
	if a.manifest.CollectorUptimeFraction < 0.99 {
		t.Errorf("collectorUptimeFraction = %v for an unbroken collector", a.manifest.CollectorUptimeFraction)
	}
	if _, err := time.Parse(time.RFC3339, a.manifest.ExportedAt); err != nil {
		t.Errorf("exportedAt = %q is not RFC3339: %v", a.manifest.ExportedAt, err)
	}
	if _, err := time.Parse(time.RFC3339, a.manifest.CollectorStartedAt); err != nil {
		t.Errorf("collectorStartedAt = %q is not RFC3339: %v", a.manifest.CollectorStartedAt, err)
	}

	// Every status collect.sh switches on, and nothing else — an unrecognised one makes it record
	// the stream as FAILED.
	known := map[string]bool{
		statusOK: true, statusEmpty: true, statusDegraded: true,
		statusNotMeasured: true, statusDisabled: true,
	}
	for _, st := range a.manifest.Streams {
		if !known[st.Status] {
			t.Errorf("stream %q has status %q, which collect.sh will report as unrecognised",
				st.Name, st.Status)
		}
		if st.Drops == nil {
			t.Errorf("stream %q has a null drops field; collect.sh does `(.drops // []) | length`, "+
				"but a null here is a shape nobody should have to defend against", st.Name)
		}
	}

	for _, name := range []string{StreamMetrics, StreamState, StreamEvents, StreamLogs,
		StreamSelfcheck, StreamHighwater} {
		st := a.stream(t, name)
		if st.Status != statusOK {
			t.Errorf("stream %q = %s (%s), want OK on a complete fixture", name, st.Status, st.Note)
		}
		if st.Items == 0 {
			t.Errorf("stream %q reports 0 items", name)
		}
	}

	// Coverage is computed from the DATA. The metrics fixture is 14 full days of five-minute
	// windows, all with successful scrapes.
	cov := a.stream(t, StreamMetrics).Coverage
	if cov == nil {
		t.Fatal("the metrics stream has no coverage block; the gap between what was asked for and " +
			"what is there is the single most important number in the file")
	}
	if cov.ObservedDays < 13.9 || cov.ObservedDays > 14.1 {
		t.Errorf("metrics observedDays = %v, want ~14", cov.ObservedDays)
	}
	if cov.RequestedDays < 13.9 || cov.RequestedDays > 14.1 {
		t.Errorf("metrics requestedDays = %v, want ~14", cov.RequestedDays)
	}

	// §7: "every Warning captured, deduplicated by UID, in arrival order". The fixture wrote each
	// one twice, as a collector restarting inside the events' one-hour API lifetime would have.
	if got := a.stream(t, StreamEvents).Items; got != 14 {
		t.Errorf("events items = %d, want 14 (one per day, not two): the collector's dedup is "+
			"per-process, so the export has to be the one that holds", got)
	}

	// The layout §7 fixes.
	for _, want := range []string{
		"MANIFEST.json", "COLLECTION-REPORT.txt", "uptime.json",
		"metrics/2026-06-01.ndjson.gz", "state/2026-06-01.ndjson.gz",
		"highwater/marks.json", "events/events.ndjson", "logs/operator-errors.ndjson",
		"selfcheck/" + day0.Format(time.RFC3339) + ".json",
	} {
		if _, ok := a.files[want]; !ok {
			t.Errorf("%s is not in the archive; §7 fixes the layout and collect.sh tells the admin "+
				"to read logs/ and events/ by name", want)
		}
	}
}

// TestHighwaterWithoutKubeletStatsIsThin. The default collector does NOT bind nodes/proxy, so the
// restic cache high-water is unobtainable — and the manifest must degrade the stream for it
// rather than report a complete measurement. collect.sh turns DEGRADED into a THIN verdict, which
// is the honest grade for a soak that could not measure the one figure the 20Gi ceiling was
// guessed at.
func TestHighwaterWithoutKubeletStatsIsThin(t *testing.T) {
	s := seedCollector(t, 2, true)
	writeJSON(t, s, fileMarks, Marks{
		UpdatedAt:     day0.AddDate(0, 0, 2),
		MetricsServer: MeasuredValue{Status: statusOK},
		KubeletStats:  MeasuredValue{Status: statusDisabled, Reason: kubeletDisabledReason},
		Classes: map[string]ClassMarks{"data": {
			Class: "data", Pods: 1, Memory: MeasuredValue{Status: statusOK},
			PeakMemoryBytes: 1 << 30, PeakSamples: 12, Cache: MeasuredValue{Status: statusDisabled},
		}},
		Pods: []PodMark{{Pod: "p", Class: "data", PeakMemoryBytes: 1 << 30, Samples: 12}},
	})
	a := exportToArchive(t, ExportOptions{
		DataDir: s.Dir(), Salt: exportSalt, Now: day0.AddDate(0, 0, 2),
	})
	st := a.stream(t, StreamHighwater)
	if st.Status != statusDegraded {
		t.Errorf("status = %q, want %q", st.Status, statusDegraded)
	}
	if !strings.Contains(st.Note, "nodes/proxy") {
		t.Errorf("the note does not name the grant that would have measured it: %q", st.Note)
	}
}

// TestHighwaterWithNoMetricsServerIsNotMeasured, which is a stronger statement than degraded: the
// peak memory is the point of the stream, and without metrics-server there is none.
func TestHighwaterWithNoMetricsServerIsNotMeasured(t *testing.T) {
	s := seedCollector(t, 2, true)
	writeJSON(t, s, fileMarks, Marks{
		UpdatedAt: day0.AddDate(0, 0, 2),
		MetricsServer: MeasuredValue{Status: statusNotMeasured,
			Reason: "metrics.k8s.io is not served by this cluster (no metrics-server)"},
		KubeletStats: MeasuredValue{Status: statusDisabled},
		Pods:         []PodMark{{Pod: "p", Class: "data"}},
	})
	a := exportToArchive(t, ExportOptions{
		DataDir: s.Dir(), Salt: exportSalt, Now: day0.AddDate(0, 0, 2),
	})
	st := a.stream(t, StreamHighwater)
	if st.Status != statusNotMeasured {
		t.Errorf("status = %q, want %q — never 0, and never OK", st.Status, statusNotMeasured)
	}
	if !strings.Contains(st.Note, "metrics-server") {
		t.Errorf("the note does not say what was missing: %q", st.Note)
	}
}

// TestNoIdentifierSurvivesTheExport is the check hack/soak/collect.sh does from outside, done
// from inside where it can be a gate rather than a report.
//
// The script takes every namespace, PVC, location and bucket name on the cluster and greps the
// unpacked archive for it verbatim. A hit is not a nuance — it is a customer name about to be
// emailed to a maintainer.
func TestNoIdentifierSurvivesTheExport(t *testing.T) {
	s := seedCollector(t, 3, true)
	a := exportToArchive(t, ExportOptions{
		DataDir: s.Dir(), Salt: exportSalt, Now: day0.AddDate(0, 0, 3),
	})

	// Decompress everything, so a name hidden inside a gzipped segment is searched too.
	var all bytes.Buffer
	for name, body := range a.files {
		all.Write(body)
		if !strings.HasSuffix(name, ".gz") {
			continue
		}
		zr, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			t.Fatalf("%s is not gzip: %v", name, err)
		}
		inner, err := io.ReadAll(zr)
		if err != nil {
			t.Fatalf("%s will not decompress: %v", name, err)
		}
		all.Write(inner)
	}

	for _, ident := range []string{
		"acme-prod",           // a namespace, in metric labels, CR state, an event and a log line
		"primary-eu",          // a location, in a metric label and in free text
		"pgdata",              // a PVC, only ever in free text
		"worker-3.internal",   // a node, only ever in free text
		"job-backup-xyz",      // a mover pod, in the high-water marks
		"crystal-backup-7d9f", // the operator's pod, in the log stream
	} {
		if bytes.Contains(all.Bytes(), []byte(ident)) {
			t.Errorf("%q appears VERBATIM in the archive. hack/soak/collect.sh greps for exactly "+
				"this and reports it as INCOMPLETE with \"do not send this archive\"", ident)
		}
	}

	// And the salt itself, in every encoding it could leak in.
	for _, form := range [][]byte{exportSalt, []byte(hex.EncodeToString(exportSalt))} {
		if bytes.Contains(all.Bytes(), form) {
			t.Error("the redaction salt is IN the archive. With it every token is reversible by " +
				"dictionary in seconds; the whole construction is worthless")
		}
	}

	// The other direction, which is what makes "it looks redacted" into something checked: the
	// token each identifier should have become IS present, computed the way collect.sh computes
	// it.
	if !bytes.Contains(all.Bytes(), []byte(tokenFor(t, "ns", "acme-prod"))) {
		t.Error("the namespace's token is not in the archive: the tokens do not correlate with the " +
			"salt the admin holds, and collect.sh would report token-check FAILED")
	}
}

// TestManifestReportsADeadStreamAsDead is the anti-vacuity contract.
//
// A collector that started happily and collected nothing must not produce an archive that reads
// as a clean fortnight. §8's rules: DISABLED for a stream never enabled, NOT_MEASURED with a
// reason for one that could not be measured, and EMPTY only for "read successfully and there was
// nothing" — with each stream declaring whether that is a healthy answer.
func TestManifestReportsADeadStreamAsDead(t *testing.T) {
	s := newTestStore(t, 1<<20)
	writeJSON(t, s, fileCollectorID, CollectorInfo{
		OperatorVersion: "0.6.1", StartedAt: day0, MetricsResolution: "5m0s",
		StateInterval: "1h0m0s", SelfcheckInterval: "24h0m0s",
		SelfcheckEnabled: false, // never given a salt: DISABLED, not EMPTY
	})
	writeJSON(t, s, fileStarts, []Session{{
		StartedAt: day0, LastBeat: day0.AddDate(0, 0, 14), BeatPeriod: "15s",
	}})

	a := exportToArchive(t, ExportOptions{
		DataDir: s.Dir(), Salt: exportSalt, Now: day0.AddDate(0, 0, 14),
	})

	for _, tc := range []struct {
		stream         string
		wantStatus     string
		emptyIsHealthy bool
		reason         string
	}{
		{StreamMetrics, statusEmpty, false,
			"nothing was scraped for a fortnight; empty is not a healthy answer here"},
		{StreamState, statusEmpty, false, "no CR was ever snapshotted"},
		{StreamSelfcheck, statusDisabled, false,
			"a stream that was never enabled is DISABLED, never EMPTY"},
		{StreamHighwater, statusNotMeasured, false,
			"a stream that could not be measured is NOT_MEASURED with a reason, never 0"},
		{StreamEvents, statusEmpty, true,
			"a fortnight with no Warning is a GOOD fortnight"},
		{StreamLogs, statusEmpty, true,
			"a fortnight with no operator error is a GOOD fortnight"},
		{StreamAlerts, statusNotMeasured, false,
			"ALERTS is a Prometheus series and this collector deliberately scrapes only the operator"},
	} {
		t.Run(tc.stream, func(t *testing.T) {
			st := a.stream(t, tc.stream)
			if st.Status != tc.wantStatus {
				t.Errorf("status = %q, want %q — %s", st.Status, tc.wantStatus, tc.reason)
			}
			if st.EmptyIsHealthy != tc.emptyIsHealthy {
				t.Errorf("emptyIsHealthy = %v, want %v; collect.sh reads it to decide whether EMPTY "+
					"is an OK or an INCOMPLETE", st.EmptyIsHealthy, tc.emptyIsHealthy)
			}
			if st.Status != statusOK && st.Note == "" {
				t.Error("a non-OK stream carries no note; collect.sh prints the note as the whole " +
					"explanation of its verdict")
			}
		})
	}

	// And an archive is written ANYWAY. The evidence of nothing is evidence, and it has to arrive
	// as a file with a manifest that says so — not as a non-zero exit and no artefact, which is
	// indistinguishable from the subcommand not existing.
	if len(a.raw) == 0 {
		t.Fatal("no archive was written for an empty collection")
	}
	if _, ok := a.files["COLLECTION-REPORT.txt"]; !ok {
		t.Error("no COLLECTION-REPORT.txt")
	}
}

// TestCapDropsReachTheManifest closes the loop between §4 and §8: an archive that lost its first
// three days says so on its first page, in the field collect.sh counts.
func TestCapDropsReachTheManifest(t *testing.T) {
	s := seedCollector(t, 6, true)
	// Re-open under a cap small enough to bite, then run it.
	small, err := OpenStore(s.Dir(), 24<<10)
	if err != nil {
		t.Fatal(err)
	}
	if err := small.EnforceCap(day0.AddDate(0, 0, 6)); err != nil {
		t.Fatal(err)
	}
	if len(small.Drops()) == 0 {
		t.Fatal("the fixture did not exceed the cap; this test proves nothing")
	}

	a := exportToArchive(t, ExportOptions{
		DataDir: s.Dir(), Salt: exportSalt, Now: day0.AddDate(0, 0, 6),
	})
	st := a.stream(t, StreamMetrics)
	if len(st.Drops) == 0 {
		t.Fatal("the cap deleted metrics segments and the manifest reports no drop. collect.sh " +
			"turns drops into a THIN verdict and there is nothing else that could tell it")
	}
	for _, d := range st.Drops {
		if d.Reason != "cap" {
			t.Errorf("drop reason = %q, want cap", d.Reason)
		}
	}
	if !strings.Contains(st.Note, "footprint cap") {
		t.Errorf("the note does not mention the cap: %q", st.Note)
	}
	report := string(a.files["COLLECTION-REPORT.txt"])
	if !strings.Contains(report, "DROPPED") {
		t.Error("COLLECTION-REPORT.txt does not mention the dropped segments; it is what a human " +
			"who untars the file reads instead of the JSON")
	}
}

// TestFullExportSaysSoEverywhere. --full is a decision an admin may legitimately make; it must
// never be one they make by accident, and collect.sh reports it as a FINDING.
func TestFullExportSaysSoEverywhere(t *testing.T) {
	s := seedCollector(t, 2, true)
	a := exportToArchive(t, ExportOptions{
		DataDir: s.Dir(), Full: true, Now: day0.AddDate(0, 0, 2),
	})
	if a.manifest.Redaction.Mode != "full" {
		t.Fatalf("redaction.mode = %q, want full", a.manifest.Redaction.Mode)
	}
	if !strings.Contains(a.manifest.UnredactedNote, "NOTHING in this archive is redacted") {
		t.Errorf("unredactedNote still describes a partial residual: %q", a.manifest.UnredactedNote)
	}
	// The identifiers really are verbatim, which is the point of the flag.
	if !bytes.Contains(a.files["events/events.ndjson"], []byte("acme-prod")) {
		t.Error("--full still redacted the events")
	}
	// And the self-check stream says what --full could NOT do, because those reports are redacted
	// at collection by the code that builds them and cannot be un-redacted afterwards.
	if !strings.Contains(a.stream(t, StreamSelfcheck).Note, "could not un-redact") {
		t.Errorf("the self-check note does not warn that --full did not reach it: %q",
			a.stream(t, StreamSelfcheck).Note)
	}
}

// TestExportRefusesHashedWithNoSalt: an archive whose tokens correlate with nothing the admin
// holds is a wasted fortnight, and collect.sh would report token-check FAILED without being able
// to say why.
func TestExportRefusesHashedWithNoSalt(t *testing.T) {
	s := seedCollector(t, 1, true)
	err := Export(ExportOptions{DataDir: s.Dir(), Now: day0.AddDate(0, 0, 1)}, io.Discard, io.Discard)
	if err == nil {
		t.Fatal("Export produced a hashed archive with no salt")
	}
	if !strings.Contains(err.Error(), "--redaction-salt-file") {
		t.Errorf("the refusal does not name the flag: %v", err)
	}
}

// TestSinceBoundsTheArchive.
func TestSinceBoundsTheArchive(t *testing.T) {
	s := seedCollector(t, 10, true)
	a := exportToArchive(t, ExportOptions{
		DataDir: s.Dir(), Salt: exportSalt, Since: 72 * time.Hour,
		Now: day0.AddDate(0, 0, 10),
	})
	for name := range a.files {
		if strings.HasPrefix(name, "metrics/") && name < "metrics/2026-06-06" {
			t.Errorf("%s is inside the archive despite --since=72h", name)
		}
	}
	if _, ok := a.files["metrics/2026-06-09.ndjson.gz"]; !ok {
		t.Error("the most recent day is missing")
	}
}

// TestStatusIsOneScreenAndNoArchive is what the admin runs on day 1 and day 2. It is the cheapest
// possible defence against discovering on day 14 that nothing was collected.
func TestStatusIsOneScreenAndNoArchive(t *testing.T) {
	t.Run("a collector that is working", func(t *testing.T) {
		s := seedCollector(t, 3, true)
		var out bytes.Buffer
		if err := Status(ExportOptions{DataDir: s.Dir(), Now: day0.AddDate(0, 0, 3)}, &out); err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"0.6.1", "day segment", "mover pod", "up 100.0%"} {
			if !strings.Contains(out.String(), want) {
				t.Errorf("the status screen does not mention %q:\n%s", want, out.String())
			}
		}
	})

	t.Run("a collector that has collected nothing says so on its first line", func(t *testing.T) {
		dir := t.TempDir()
		var out bytes.Buffer
		if err := Status(ExportOptions{DataDir: dir, Now: day0}, &out); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "Nothing is being collected") {
			t.Errorf("the status screen is quiet about a collector that has never run:\n%s", out.String())
		}
	})

	t.Run("the errors that have been happening are on the screen", func(t *testing.T) {
		s := seedCollector(t, 1, true)
		s.RecordError(StreamMetrics, "scrape https://op:8443/metrics: HTTP 401: Unauthorized", day0)
		var out bytes.Buffer
		if err := Status(ExportOptions{DataDir: s.Dir(), Now: day0.AddDate(0, 0, 1)}, &out); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "HTTP 401") {
			t.Errorf("a scrape that has been 401ing does not appear on the status screen:\n%s", out.String())
		}
	})
}

// tokenFor reproduces hack/soak/collect.sh's token construction independently of the redactor, so
// this test cannot pass merely because both sides changed together.
func tokenFor(t *testing.T, kind, value string) string {
	t.Helper()
	mac := hmacSHA256(exportSalt, append(append([]byte(kind), 0), []byte(value)...))
	return kind + "-" + hex.EncodeToString(mac[:4])
}

// TestTheArchiveStatesItsOwnAnswer is about who reads this file.
//
// The archive comes back from a cluster its recipient does not have access to. Everything else in
// COLLECTION-REPORT.txt grades the COLLECTION — how many days, which streams, what was dropped —
// and for six streams that is the right thing to say. For the high-water table it is not: the
// numbers the fortnight was spent to obtain lived only in highwater/marks.json, so reading the
// result meant parsing JSON. An archive whose headline finding is machine-readable only is an
// archive that gets filed.
//
// It also pins the harder half: EVERY sizing class is printed, including the empty ones. Omitting
// a class with no pods is exactly how `data` and `manifests` stayed invisible through a campaign
// that ran dozens of backups.
func TestTheArchiveStatesItsOwnAnswer(t *testing.T) {
	s := seedCollector(t, 2, true)
	// One repo-light mover with a peak, and nothing at all for the other three classes — the
	// asymmetric case, where a section that only printed what it had would look complete.
	at := day0.Add(time.Hour)
	pod := terminatedWith(moverPod("job-snap-1", "job-snap", "worker-1", at), mover.MoverResult{
		OK: true, Operation: string(mover.OpSnapshots),
		PeakRSSBytes: 101 << 20, MemorySource: mover.MemorySourceBoth,
	})
	writeMarksFixture(t, s, marksFixture(t,
		[]batchv1.Job{moverJob("job-snap", mover.OpSnapshots, at, time.Time{})},
		[]corev1.Pod{pod}, at.Add(15*time.Second)))

	a := exportToArchive(t, ExportOptions{
		DataDir: s.Dir(), Salt: exportSalt, Now: day0.AddDate(0, 0, 2),
	})
	report := string(a.files["COLLECTION-REPORT.txt"])

	if !strings.Contains(report, "MOVER RESOURCE PROFILES") {
		t.Fatalf("the report does not state the §5 answer at all; the numbers exist only in "+
			"highwater/marks.json and the recipient has to parse JSON to read the result:\n%s", report)
	}
	for _, class := range mover.Classes() {
		if !strings.Contains(report, class) {
			t.Errorf("sizing class %q is absent from the report. A class omitted because it had no "+
				"pods is a class nobody notices is missing — which is how two of them stayed "+
				"invisible for four hours.", class)
		}
	}
	// And the class that DOES have a figure carries it, with its provenance — otherwise this test
	// passes against a section that prints four headings and no data.
	if !strings.Contains(report, "101Mi") {
		t.Errorf("the measured peak is not in the report:\n%s", report)
	}
	if !strings.Contains(report, "mover-reported") {
		t.Error("the peak appears without naming its source; an exact process peak and the highest " +
			"sample anyone caught are different quantities and must never read alike")
	}
}

// ---------------------------------------------------------------------------------------------
// which build produced this
// ---------------------------------------------------------------------------------------------

// upgradedSessions is the fortnight this whole thing exists for: six days on one build, a restart,
// and seven on another. Written the way the collector writes them — one record per process, each
// naming the build that ran it — because the export has no other source for this.
func upgradedSessions() []Session {
	return []Session{
		{StartedAt: day0, LastBeat: day0.AddDate(0, 0, 6), BeatPeriod: "15s",
			OperatorVersion: "0.6.2"},
		{StartedAt: day0.AddDate(0, 0, 7), LastBeat: day0.AddDate(0, 0, 14), BeatPeriod: "15s",
			OperatorVersion: "0.6.3"},
	}
}

// marksAcrossVersions builds a high-water table whose pods were FIRST SEEN at three different
// moments — one under each build and one inside the gap between them — through the same real
// aggregation marksFixture uses.
//
// It has to go through HighWater rather than filling PodMark.FirstSeen by hand for the reason
// marksFixture states: a hand-built table once produced a shape the collector cannot emit, and the
// attribution under test reads exactly that field. The lister is mutated between samples because
// that is how a cluster presents — a pod that does not exist yet is not in the list.
func marksAcrossVersions(t *testing.T, underFirst, inGap, underSecond time.Time) Marks {
	t.Helper()
	f := &fakeLister{metrics: []podMetric{}}
	h := NewHighWater(newTestStore(t, 1<<20), f, testNS, 15*time.Second, false)

	f.jobs = []batchv1.Job{moverJob("job-backup-a", mover.OpBackup, underFirst, time.Time{})}
	f.pods = []corev1.Pod{moverPod("job-backup-a-1", "job-backup-a", "worker-1", underFirst)}
	h.Sample(t.Context(), underFirst)

	f.jobs = append(f.jobs, moverJob("job-snap", mover.OpSnapshots, inGap, time.Time{}))
	f.pods = append(f.pods, moverPod("job-snap-1", "job-snap", "worker-2", inGap))
	h.Sample(t.Context(), inGap)

	f.jobs = append(f.jobs, moverJob("job-backup-b", mover.OpBackup, underSecond, time.Time{}))
	f.pods = append(f.pods, moverPod("job-backup-b-1", "job-backup-b", "worker-1", underSecond))
	h.Sample(t.Context(), underSecond)

	return h.Marks(underSecond)
}

// moverProfilesSection is §5 and nothing else, so an assertion about the table cannot be satisfied
// by a word that appears somewhere else in the report.
func moverProfilesSection(t *testing.T, report string) string {
	t.Helper()
	start := strings.Index(report, "MOVER RESOURCE PROFILES")
	if start < 0 {
		t.Fatalf("the report has no §5 section at all:\n%s", report)
	}
	rest := report[start:]
	if end := strings.Index(rest, "COLLECTOR UPTIME"); end > 0 {
		rest = rest[:end]
	}
	return rest
}

// TestAnUpgradedSoakSaysWhichBuildsProducedIt.
//
// `operatorVersion` comes from collector-id.json, which is rewritten in full on every collector
// start — so it names whichever build started LAST, and a fortnight that ran six days of 0.6.2 and
// eight of 0.6.3 came back claiming one flat version. The restart was recorded, its gap was
// recorded, and the one thing that mattered about it — that the measured system CHANGED there —
// was recorded nowhere. Everything downstream then averaged two systems into one answer.
func TestAnUpgradedSoakSaysWhichBuildsProducedIt(t *testing.T) {
	s := seedCollector(t, 14, true)
	writeJSON(t, s, fileStarts, upgradedSessions())

	a := exportToArchive(t, ExportOptions{
		DataDir: s.Dir(), Salt: exportSalt, Now: day0.AddDate(0, 0, 14),
	})

	if len(a.manifest.OperatorVersions) != 2 {
		t.Fatalf("MANIFEST.json reports %d version span(s), want 2: %+v. collect.sh and every "+
			"reader after it take this file as the archive's own account of itself",
			len(a.manifest.OperatorVersions), a.manifest.OperatorVersions)
	}
	for i, want := range []string{"0.6.2", "0.6.3"} {
		if got := a.manifest.OperatorVersions[i].Version; got != want {
			t.Errorf("operatorVersions[%d].version = %q, want %q", i, got, want)
		}
	}
	if days := a.manifest.OperatorVersions[0].ObservedSeconds / 86400; days < 5.9 || days > 6.1 {
		t.Errorf("the first build was observed for %.2f day(s), want ~6 — the SHARE each build "+
			"holds is what says whether a mixed figure is mostly about one of them", days)
	}
	if a.manifest.OperatorVersionNote == "" {
		t.Error("operatorVersionNote is empty over two builds. The JSON half and the prose half of " +
			"this archive have to say the same thing about it, and this is the field that makes " +
			"that structural rather than a matter of both being edited together")
	}

	report := string(a.files["COLLECTION-REPORT.txt"])
	for _, want := range []string{"0.6.2", "0.6.3", "2 BUILDS", "MORE THAN ONE BUILD"} {
		if !strings.Contains(report, want) {
			t.Errorf("COLLECTION-REPORT.txt does not contain %q. It is the file a human opens "+
				"instead of the JSON:\n%s", want, report)
		}
	}
	// The dates, not just the names: what a reader needs from a mixed soak is which half of the
	// fortnight each build was measured over, so they can go and look at what else changed then.
	if !strings.Contains(report, day0.AddDate(0, 0, 7).Format(time.RFC3339)) {
		t.Errorf("the report names two builds without saying WHEN each ran:\n%s", report)
	}
}

// TestTheMoverTableDisclosesTheVersionsBehindIt is the requirement that matters.
//
// highwater/marks.json PERSISTS across collector restarts — NewHighWater loads it back — so a soak
// that was upgraded mid-run produces ONE peak table over TWO systems. §5's table is the finding the
// fortnight was spent to obtain and it is read as a property of a single build; a table that mixes
// two and does not say so is worse than no table, because a limit gets sized off it.
//
// The disclosure goes above the first number. A reader who has already read the figures has
// already decided what they mean.
func TestTheMoverTableDisclosesTheVersionsBehindIt(t *testing.T) {
	s := seedCollector(t, 14, true)
	writeJSON(t, s, fileStarts, upgradedSessions())
	writeMarksFixture(t, s, marksAcrossVersions(t,
		day0.AddDate(0, 0, 1),                   // under 0.6.2
		day0.AddDate(0, 0, 6).Add(12*time.Hour), // inside the gap: no build can claim it
		day0.AddDate(0, 0, 8),                   // under 0.6.3
	))

	a := exportToArchive(t, ExportOptions{
		DataDir: s.Dir(), Salt: exportSalt, Now: day0.AddDate(0, 0, 14),
	})
	section := moverProfilesSection(t, string(a.files["COLLECTION-REPORT.txt"]))

	disclosure := strings.Index(section, "MORE THAN ONE BUILD")
	if disclosure < 0 {
		t.Fatalf("§5 states a peak table built over two builds and says nothing about it. Every "+
			"number under this heading is being read as a property of one system:\n%s", section)
	}
	firstClass := strings.Index(section, "pod(s) over")
	if firstClass < 0 {
		t.Fatalf("§5 has no class lines at all; this test would pass against an empty table:\n%s",
			section)
	}
	if disclosure > firstClass {
		t.Errorf("the disclosure appears AFTER the first class line. A reader who has read the "+
			"figures has already decided what they mean:\n%s", section)
	}
	for _, want := range []string{"0.6.2 (6.0 day(s))", "0.6.3 (7.0 day(s))"} {
		if !strings.Contains(section, want) {
			t.Errorf("§5 does not name %q. \"more than one build\" without the versions and the "+
				"time each holds is a warning nobody can act on:\n%s", want, section)
		}
	}

	// Per class, from PodMark.FirstSeen and nothing else. The two backups landed one on each side
	// of the upgrade, so the class that carries the sizing answer carries both builds.
	if !strings.Contains(section, "(pods ran under 0.6.2 ×1, 0.6.3 ×1)") {
		t.Errorf("the `data` line does not attribute its pods to the builds they ran under:\n%s",
			section)
	}
	// And the pod that started inside the GAP is attributed to NOTHING. Filing it under the
	// neighbouring build would be a guess printed beside measurements — the same rule that makes a
	// mover of unrecoverable operation `unknown` rather than the class that looks likeliest.
	if !strings.Contains(section, "repo-light   1 pod(s) over 1 Job(s)  (1 unattributed)") {
		t.Errorf("a pod first seen while no collector was running was attributed to a build "+
			"anyway:\n%s", section)
	}
}

// TestABuildTheOperatorClaimsAndNoSessionDoesIsAProblem.
//
// Two independent records of which build ran: the session log, which is the COLLECTOR binary's own
// version, and crystalbackup_build_info, which is the MEASURED operator's. They come apart exactly
// when somebody rolls the operator image forward without touching the collector's — the one case
// where the session log is unanimous and wrong, and therefore the one case a session-only check
// could never catch.
//
// Nothing picks a winner. The disagreement is the finding, and it goes where the export's other
// problems go.
func TestABuildTheOperatorClaimsAndNoSessionDoesIsAProblem(t *testing.T) {
	// The label name the export reads is a bare string matching a constant internal/metrics keeps
	// unexported, and a misspelling there fails the way internal/selfcheck's labelKinds describes:
	// silently, by finding nothing and reporting nothing, for as long as nobody checks. So it is
	// checked, against the label set that package publishes.
	if labels := metrics.Catalogue()[metrics.NameBuildInfo]; !slices.Contains(labels, buildInfoVersionLabel) {
		t.Fatalf("crystalbackup_build_info carries labels %v and this package reads %q. Every "+
			"corroboration below would find nothing and say nothing", labels, buildInfoVersionLabel)
	}

	buildInfo := func(t *testing.T, s *Store, day time.Time, version string) {
		t.Helper()
		if err := s.Append(streamMetricsCore, day, [][]byte{mustJSON(t, point{
			T: day.Add(time.Hour).Unix(), Name: metrics.NameBuildInfo,
			Labels: map[string]string{"version": version}, Last: 1, Min: 1, Max: 1, N: 1,
		})}); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("a version no session claims", func(t *testing.T) {
		s := seedCollector(t, 3, true)
		buildInfo(t, s, day0.AddDate(0, 0, 1), "0.6.9")

		a := exportToArchive(t, ExportOptions{
			DataDir: s.Dir(), Salt: exportSalt, Now: day0.AddDate(0, 0, 3),
		})
		report := string(a.files["COLLECTION-REPORT.txt"])
		problems := report[max(strings.Index(report, "PROBLEMS DURING EXPORT"), 0):]
		if !strings.Contains(report, "PROBLEMS DURING EXPORT") || !strings.Contains(problems, "0.6.9") {
			t.Errorf("the operator reported a build the collector never ran under and the archive "+
				"is silent about it. The sessions all say 0.6.1 and every one of them is right; "+
				"what changed was the thing being measured:\n%s", report)
		}
		if a.manifest.OperatorVersionNote == "" {
			t.Error("MANIFEST.json does not carry the disagreement, so a reader who parses the JSON " +
				"instead of the prose sees a single-version archive")
		}
	})

	t.Run("the version the sessions do claim is not a problem", func(t *testing.T) {
		s := seedCollector(t, 3, true)
		buildInfo(t, s, day0.AddDate(0, 0, 1), "0.6.1")

		a := exportToArchive(t, ExportOptions{
			DataDir: s.Dir(), Salt: exportSalt, Now: day0.AddDate(0, 0, 3),
		})
		report := string(a.files["COLLECTION-REPORT.txt"])
		if strings.Contains(report, "PROBLEMS DURING EXPORT") {
			t.Errorf("the operator corroborated the session log and the export reported a problem. "+
				"An alarm that fires on agreement is an alarm nobody reads on the day of a real "+
				"disagreement:\n%s", report)
		}
		if a.manifest.OperatorVersionNote != "" {
			t.Errorf("operatorVersionNote = %q over one agreed build", a.manifest.OperatorVersionNote)
		}
	})
}

// TestStatusShowsAMidRunUpgradeOnTheDayItHappens.
//
// This screen is what the admin runs on day 1 and day 2, and hack/soak/collect.sh captures it as
// its first cluster interaction. An upgrade found here is a decision somebody can still make —
// restart the soak, or accept a mixed fortnight knowingly. The same fact found at export is a
// fortnight already spent.
func TestStatusShowsAMidRunUpgradeOnTheDayItHappens(t *testing.T) {
	s := seedCollector(t, 8, true)
	writeJSON(t, s, fileStarts, upgradedSessions())

	var out bytes.Buffer
	if err := Status(ExportOptions{DataDir: s.Dir(), Now: day0.AddDate(0, 0, 8)}, &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"2 BUILDS", "0.6.2", "0.6.3", "MORE THAN ONE BUILD"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the status screen does not mention %q; the operator version line reports "+
				"whichever build collector-id.json was last written with:\n%s", want, out.String())
		}
	}
}

// TestASingleVersionSoakReadsExactlyAsBefore is the other half of the requirement, and the half
// that is easy to lose.
//
// Almost every soak runs one build. If saying "more than one build produced this" costs a block on
// every archive that says everything was normal, the block is noise — and a reader who has skimmed
// past it thirteen times will skim past it on the fourteenth, which is the archive it was written
// for. Nothing new appears anywhere until there is something to say.
func TestASingleVersionSoakReadsExactlyAsBefore(t *testing.T) {
	s := seedCollector(t, 3, true)
	a := exportToArchive(t, ExportOptions{
		DataDir: s.Dir(), Salt: exportSalt, Now: day0.AddDate(0, 0, 3),
	})
	report := string(a.files["COLLECTION-REPORT.txt"])

	// The header line, byte for byte, with nothing inserted after it.
	if !strings.Contains(report, "operator version   0.6.1\ncollector started  ") {
		t.Errorf("the version header is no longer one line followed by `collector started`:\n%s",
			report)
	}
	// The §5 class line ends where it always ended: no attribution suffix when there is nothing to
	// attribute between.
	if !regexp.MustCompile(`\n {2}data +\d+ pod\(s\) over \d+ Job\(s\)\n`).MatchString(report) {
		t.Errorf("the §5 class line grew something after `Job(s)` on a single-build soak:\n%s",
			moverProfilesSection(t, report))
	}
	// The tokens this change can add, and only those. Not the bare word "unattributed": §5 already
	// uses it for the `unknown` sizing class, and a guard that fails on somebody else's sentence is
	// a guard that gets deleted rather than read.
	for _, unwanted := range []string{"BUILDS", "MORE THAN ONE BUILD", "pods ran under"} {
		if strings.Contains(report, unwanted) {
			t.Errorf("%q appears in the report of a soak that ran ONE build:\n%s", unwanted, report)
		}
	}
	if a.manifest.OperatorVersionNote != "" {
		t.Errorf("operatorVersionNote = %q on a single-build archive", a.manifest.OperatorVersionNote)
	}
	// The machine-readable half still carries the span, because a reader who wants to check has to
	// be able to: silence in the prose is not the same as an absent record.
	if len(a.manifest.OperatorVersions) != 1 {
		t.Errorf("operatorVersions = %+v, want exactly one span", a.manifest.OperatorVersions)
	}

	var status bytes.Buffer
	if err := Status(ExportOptions{DataDir: s.Dir(), Now: day0.AddDate(0, 0, 3)}, &status); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(status.String(), "  operator version     0.6.1\n  metrics URL") {
		t.Errorf("the --status version line gained something on a single-build soak:\n%s",
			status.String())
	}
}
