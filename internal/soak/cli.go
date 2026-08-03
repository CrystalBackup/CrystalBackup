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
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
)

// The subcommand names, so cmd/main.go's dispatch and the usage text below cannot drift.
const (
	CommandCollect = "soak-collect"
	CommandExport  = "soak-export"
)

// Usage is the one screen both subcommands share. It is here rather than in cmd/main.go because
// the flags it documents are defined here, and because `go build cmd/main.go` compiles ONE file.
const Usage = `crystal-backup ` + CommandCollect + ` [flags]
        The resident soak collector. Runs until killed, writing five streams to a PVC.

        --data-dir <dir>                 the PVC mount (default /var/lib/crystal-backup-soak)
        --max-bytes <size>               hard cap on the on-disk footprint (default 512Mi)
        --metrics-url <url>              the operator's /metrics. REQUIRED.
        --metrics-insecure-skip-verify   the metrics server's certificate is self-signed
        --metrics-interval <dur>         scrape cadence (default 60s)
        --metrics-resolution <dur>       downsample window (default 5m)
        --mover-sample-interval <dur>    mover high-water sampling (default 15s)
        --selfcheck-interval <dur>       daily self-check (default 24h)
        --state-interval <dur>           CR snapshots (default 1h)
        --operator-namespace <ns>        default $POD_NAMESPACE
        --salt-method <auto|from-secret>  where the redaction salt comes from (default auto:
                                         derived from the operator namespace's UID)
        --redaction-salt-file <path>     REQUIRED by --salt-method=from-secret; recorded so
                                         soak-export finds it
        --kubelet-stats                  read the restic cache high-water (needs nodes/proxy)
        --heartbeat-check                the liveness probe: exit 0 if the heartbeat is fresh

crystal-backup ` + CommandExport + ` [flags]
        Writes ONE tar.gz to STDOUT. Progress goes to stderr; nothing else touches stdout.

        --data-dir <dir>                 the PVC mount
        --salt-method <auto|from-secret>  must match the collector's; defaults to what the
                                         collector recorded
        --redaction-salt-file <path>     REQUIRED by --salt-method=from-secret, unless --full
        --full                           DISABLE redaction. Identifiers verbatim.
        --since <dur>                    only this far back (default: everything)
        --status                         no archive: one screen on stderr saying what is there
`

// Defaults, named so the usage text, the flag registration and the chart values that render into
// these flags (charts/crystal-backup/values.yaml, `soak:`) can be checked against each other.
const (
	defaultDataDir             = "/var/lib/crystal-backup-soak"
	defaultMaxBytes            = "512Mi"
	defaultMetricsInterval     = time.Minute
	defaultMetricsResolution   = 5 * time.Minute
	defaultMoverSampleInterval = 15 * time.Second
	defaultSelfcheckInterval   = 24 * time.Hour
	defaultStateInterval       = time.Hour
)

// startupScrapeAttempts is how many times a scrape may fail before the collector refuses to run.
//
// Three, with a short pause between, because the one legitimate reason a first scrape fails is
// that the collector's pod started before the operator finished coming up. Everything else — a
// wrong URL, an unbound crystal-backup-metrics-reader, a NetworkPolicy that does not allow 8443 —
// is permanent, and finding out about it now beats finding out on day fourteen from an empty
// manifest.
const startupScrapeAttempts = 3

// startupScrapeBackoff is a var only so the tests can collapse it. Nothing at runtime changes it.
var startupScrapeBackoff = 5 * time.Second

// RunCollect is `crystal-backup soak-collect`. It returns a process exit code.
func RunCollect(ctx context.Context, args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet(CommandCollect, flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		dataDir     = fs.String("data-dir", defaultDataDir, "the PVC mount; everything is written here and nowhere else")
		maxBytes    = fs.String("max-bytes", defaultMaxBytes, "hard cap on the on-disk footprint")
		metricsURL  = fs.String("metrics-url", "", "the operator's /metrics endpoint (required)")
		skipVerify  = fs.Bool("metrics-insecure-skip-verify", false, "accept the metrics server's self-signed certificate")
		metricsIvl  = fs.Duration("metrics-interval", defaultMetricsInterval, "scrape cadence")
		resolution  = fs.Duration("metrics-resolution", defaultMetricsResolution, "downsample window")
		moverIvl    = fs.Duration("mover-sample-interval", defaultMoverSampleInterval, "mover high-water sampling cadence")
		selfIvl     = fs.Duration("selfcheck-interval", defaultSelfcheckInterval, "daily self-check cadence")
		stateIvl    = fs.Duration("state-interval", defaultStateInterval, "CR snapshot cadence")
		namespace   = fs.String("operator-namespace", defaultNamespace(), "where the operator and its mover Jobs live")
		saltMethod  = fs.String("salt-method", SaltMethodAuto, "where the redaction salt comes from: auto | from-secret")
		saltFile    = fs.String("redaction-salt-file", "", "the stable redaction salt (>= 32 raw bytes); required by --salt-method=from-secret")
		kubeletStat = fs.Bool("kubelet-stats", false, "read the restic cache high-water from the kubelet (needs nodes/proxy)")
		heartbeat   = fs.Bool("heartbeat-check", false, "exit 0 if the heartbeat is fresh, non-zero if it is not; print nothing")
	)
	fs.Usage = func() { _, _ = fmt.Fprint(stderr, Usage) }
	if err := fs.Parse(args); err != nil {
		return 2
	}

	// The liveness probe. Handled before anything else opens a file or builds a client: it runs
	// every five minutes for a fortnight in a container with a 200m CPU limit, and it must be a
	// read of one small document and nothing more.
	if *heartbeat {
		return CheckHeartbeat(*dataDir, time.Now())
	}

	cap, err := parseSize(*maxBytes)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "soak-collect: --max-bytes: %v\n", err)
		return 2
	}
	if *metricsURL == "" {
		// §1's first refusal. A collector with no metrics URL would run for a fortnight and
		// produce an archive with the one stream that cannot be reconstructed afterwards missing,
		// and it would look healthy the whole time.
		_, _ = fmt.Fprint(stderr,
			"soak-collect: --metrics-url is required. Without it this would collect no metrics at all "+
				"for the length of the soak and say nothing about it.\n\n"+
				"  --metrics-url=https://crystal-backup-metrics.<namespace>.svc:8443/metrics\n\n"+
				"Check the Service name against your release: kubectl -n <ns> get svc | grep metrics\n")
		return 2
	}
	for name, d := range map[string]time.Duration{
		"--metrics-interval": *metricsIvl, "--metrics-resolution": *resolution,
		"--mover-sample-interval": *moverIvl, "--selfcheck-interval": *selfIvl,
		"--state-interval": *stateIvl,
	} {
		if d <= 0 {
			_, _ = fmt.Fprintf(stderr, "soak-collect: %s must be positive, got %s\n", name, d)
			return 2
		}
	}
	// The salt method is validated BEFORE anything opens a file, builds a client or spends three
	// scrape attempts: an unknown value is a typo in a manifest, and finding out about it now
	// beats finding out on day fourteen from an archive whose tokens correlate with nothing.
	if err := checkSaltFlags(*saltMethod, *saltFile); err != nil {
		_, _ = fmt.Fprintf(stderr, "soak-collect: %v\n", err)
		return 2
	}

	store, err := OpenStore(*dataDir, cap)
	if err != nil {
		// §1's other two refusals, both of them startup-only and both loud. A collector that
		// starts happily and collects nothing is the failure this whole kit exists to avoid, and
		// it must not be possible to reach it by accident.
		_, _ = fmt.Fprintf(stderr, "soak-collect: %v\n", err)
		return 1
	}

	cfg, err := ctrl.GetConfig()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "soak-collect: no Kubernetes configuration: %v\n", err)
		return 1
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "soak-collect: cannot build a Kubernetes client: %v\n", err)
		return 1
	}
	reader, err := client.New(cfg, client.Options{Scheme: soakScheme()})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "soak-collect: cannot build an API reader: %v\n", err)
		return 1
	}

	// The salt, from the named method. A REFUSAL either way — see ResolveSalt: there is no
	// fallback from fromSecret to auto and none from auto to random, because both would produce
	// an archive that claims one guarantee and holds another. It comes after the client because
	// `auto` reads the namespace, and before anything is written because the whole fortnight is
	// keyed on it.
	salt, saltSource, err := ResolveSalt(*saltMethod, *saltFile, namespaceUID(ctx, cs, *namespace))
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "soak-collect: %v\n", err)
		return 2
	}

	scraper := NewScraper(*metricsURL, *skipVerify)
	if err := proveScrape(ctx, scraper, stderr); err != nil {
		_, _ = fmt.Fprintf(stderr, "soak-collect: %v\n", err)
		return 1
	}

	now := time.Now().UTC()
	info := CollectorInfo{
		OperatorVersion:     operatorVersion(),
		StartedAt:           now,
		OperatorNamespace:   *namespace,
		MetricsURL:          *metricsURL,
		MetricsInterval:     metricsIvl.String(),
		MetricsResolution:   resolution.String(),
		MoverSampleInterval: moverIvl.String(),
		SelfcheckInterval:   selfIvl.String(),
		StateInterval:       stateIvl.String(),
		KubeletStats:        *kubeletStat,
		SaltMethod:          *saltMethod,
		SaltFile:            *saltFile,
		MaxBytes:            cap,
		SelfcheckEnabled:    len(salt) > 0,
	}
	if body, err := json.MarshalIndent(info, "", "  "); err == nil {
		if err := store.WriteFileAtomic(fileCollectorID, body); err != nil {
			_, _ = fmt.Fprintf(stderr, "soak-collect: cannot write to the data directory: %v\n", err)
			return 1
		}
	}

	sessions, err := OpenSessionLog(store, now, *moverIvl)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "soak-collect: cannot record this session: %v\n", err)
		return 1
	}

	lister := &clusterLister{cs: cs}
	highWater := NewHighWater(store, lister, *namespace, *moverIvl, *kubeletStat)

	c := &Collector{
		Store: store, Sessions: sessions, Info: info,
		Scraper:    scraper,
		Aggregator: NewAggregator(*resolution),
		HighWater:  highWater,
		// The event half. Without it the mover figures would be whatever a 15s poll happened to
		// catch of Jobs that live ten to twenty seconds — see moverwatch.go.
		MoverWatch:      NewMoverWatch(highWater, store, lister),
		Events:          NewEventStream(cs, store),
		Logs:            NewLogStream(cs, store, *namespace),
		State:           NewStateStream(reader, store),
		MetricsInterval: *metricsIvl, MoverInterval: *moverIvl, StateInterval: *stateIvl,
		Progress: stderr,
	}
	// Unconditional now, and it was effectively unconditional before: the previous code loaded the
	// salt with LoadRedactionSalt(""), which cannot succeed, so the "self-check DISABLED" branch it
	// carried was unreachable — soak-collect simply refused to start without a Secret. Both methods
	// either yield a salt or refuse, so the stream is always on.
	var disco discovery.DiscoveryInterface
	if d, err := discovery.NewDiscoveryClientForConfig(cfg); err == nil {
		disco = d
	}
	c.Selfcheck = &SelfcheckRunner{
		Reader: reader, Discovery: disco, Namespace: *namespace,
		Salt: salt, SaltSource: saltSource, Interval: *selfIvl, Store: store,
	}
	_, _ = fmt.Fprintf(stderr, "soak-collect: redaction salt from --salt-method=%s (saltSource: %s)\n",
		*saltMethod, saltSource)

	runCtx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return exitCode(c.Run(runCtx), stderr)
}

func exitCode(err error, stderr io.Writer) int {
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "soak-collect: %v\n", err)
		return 1
	}
	return 0
}

// proveScrape is §1's third refusal: the metrics URL must be scrapeable before the collector will
// commit to a fortnight.
//
// It is a REFUSAL and not a warning. A warning at startup is a line in a log nobody reads on day
// one and nobody can find on day fourteen, by which point the archive is missing the stream this
// whole design was rebuilt around. Three attempts, because the one benign failure is a pod that
// started before the operator did.
func proveScrape(ctx context.Context, s *Scraper, stderr io.Writer) error {
	var last error
	for i := 1; i <= startupScrapeAttempts; i++ {
		if _, err := s.Scrape(ctx); err == nil {
			return nil
		} else {
			last = err
		}
		_, _ = fmt.Fprintf(stderr, "soak-collect: scrape attempt %d/%d failed: %v\n",
			i, startupScrapeAttempts, last)
		if i < startupScrapeAttempts {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(startupScrapeBackoff):
			}
		}
	}
	return fmt.Errorf(
		"could not scrape %s after %d attempts: %w\n\n"+
			"  A 401 means the projected ServiceAccount token was rejected.\n"+
			"  A 403 means this ServiceAccount is not bound to crystal-backup-metrics-reader —\n"+
			"    that is the <release>-soak-metrics-reader ClusterRoleBinding the chart renders\n"+
			"    under soak.enabled.\n"+
			"  A TLS error means you want --metrics-insecure-skip-verify (the metrics server's\n"+
			"    certificate is self-signed unless you wired cert-manager).\n"+
			"  A timeout means a NetworkPolicy: the chart's default-deny covers pods that did not\n"+
			"    exist when it was applied, which includes this one.\n\n"+
			"  Refusing to start. A collector that runs for a fortnight without ever scraping\n"+
			"  produces an archive that looks complete and is not",
		s.URL, startupScrapeAttempts, last)
}

// RunExport is `crystal-backup soak-export`. It returns a process exit code.
func RunExport(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet(CommandExport, flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		dataDir    = fs.String("data-dir", defaultDataDir, "the PVC mount")
		saltMethod = fs.String("salt-method", "", "where the redaction salt comes from: auto | from-secret (default: what the collector recorded)")
		saltFile   = fs.String("redaction-salt-file", "", "the stable redaction salt (>= 32 raw bytes)")
		full       = fs.Bool("full", false, "DISABLE redaction: identifiers appear verbatim")
		since      = fs.Duration("since", 0, "only export this far back (default: everything)")
		status     = fs.Bool("status", false, "write a status screen to stderr instead of an archive")
	)
	fs.Usage = func() { _, _ = fmt.Fprint(stderr, Usage) }
	if err := fs.Parse(args); err != nil {
		return 2
	}
	// --status FIRST, before the salt is looked at. It writes no archive and redacts nothing, so
	// it must not be able to fail for a reason that has nothing to do with what it reports —
	// hack/soak/collect.sh runs it as its very first cluster interaction and treats any non-zero
	// exit as "this build has no soak-export", which is the wrong conclusion to reach because a
	// Secret was mounted at the wrong path.
	if *status {
		opts := ExportOptions{DataDir: *dataDir, Now: time.Now()}
		if err := Status(opts, stderr); err != nil {
			_, _ = fmt.Fprintf(stderr, "soak-export: %v\n", err)
			return 1
		}
		return 0
	}

	// The method and the salt file the COLLECTOR was given, if this invocation was not told them.
	// That is the entire reason soak-collect records both: `kubectl exec … soak-export` must
	// reproduce the same salt days later, and asking the operator to remember how the running
	// collector was configured is asking them to break the correlation by hand.
	namespace := defaultNamespace()
	if info, err := ReadCollectorInfo(*dataDir); err == nil {
		if *saltFile == "" {
			*saltFile = info.SaltFile
		}
		if *saltMethod == "" {
			*saltMethod = info.SaltMethod
		}
		if info.OperatorNamespace != "" {
			namespace = info.OperatorNamespace
		}
	}
	if *saltMethod == "" {
		// An archive from a collector older than the methods. It had a Secret or it had nothing,
		// and its manifest names the file — so fromSecret is what it actually used, and saying so
		// is a statement about that archive rather than a default applied to it.
		*saltMethod = SaltMethodFromSecret
	}

	var salt []byte
	if !*full {
		var err error
		salt, _, err = ResolveSalt(*saltMethod, *saltFile, exportNamespaceUID(namespace))
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "soak-export: %v\n", err)
			return 2
		}
	}

	opts := ExportOptions{DataDir: *dataDir, Salt: salt, Full: *full, Since: *since, Now: time.Now()}
	if err := Export(opts, stdout, stderr); err != nil {
		_, _ = fmt.Fprintf(stderr, "soak-export: %v\n", err)
		return 1
	}
	return 0
}

// parseSize accepts what the Kubernetes world writes: 512Mi, 1Gi, 536870912.
func parseSize(s string) (int64, error) {
	q, err := resource.ParseQuantity(strings.TrimSpace(s))
	if err != nil {
		return 0, fmt.Errorf("%q is not a size (try 512Mi): %w", s, err)
	}
	v, ok := q.AsInt64()
	if !ok || v <= 0 {
		return 0, fmt.Errorf("%q is not a usable size", s)
	}
	return v, nil
}

func defaultNamespace() string {
	if ns := os.Getenv("POD_NAMESPACE"); ns != "" {
		return ns
	}
	return apiconst.DefaultOperatorNamespace
}

// soakScheme carries only what the CR-state stream and the self-check read. The crystalbackup.io
// kinds go through unstructured by GVK, exactly as internal/selfcheck and internal/metrics do, so
// a CRD that is not installed is a clean no-match rather than a decode failure.
func soakScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(cbv1.AddToScheme(s))
	utilruntime.Must(batchv1.AddToScheme(s))
	utilruntime.Must(corev1.AddToScheme(s))
	return s
}
