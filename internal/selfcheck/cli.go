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

package selfcheck

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/discovery"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
)

// Names of the two subcommands, so cmd/main.go's dispatch and the usage text below cannot drift.
const (
	CommandSelfcheck = "selfcheck"
	CommandReport    = "report"
)

// Usage is the one-screen help both subcommands share. It is here rather than in cmd/main.go
// because the flags it documents are defined here.
//
// The collection flags are listed under `selfcheck` and referred to from `report` rather than
// repeated, because they ARE the same flags — one registration, one meaning (see collectFlags).
const Usage = `crystal-backup ` + CommandSelfcheck + ` [flags]
        Reads the cluster with the operator's own RBAC and writes a JSON installation report.

        --output <file>        write here instead of stdout
        --operator-namespace   where the operator and its mover Jobs live (default $POD_NAMESPACE)
        --mover-image          the configured mover image, recorded beside what is running
        --sync-image           the configured sync image, likewise
        --full                 DISABLE redaction. Identifiers appear verbatim; share privately only.
        --redaction-salt-file  salt the tokens from this file (>= 32 raw bytes) instead of from a
                               fresh random salt, so reports taken on different days share tokens
                               and can be read as one series. For a soak you keep. NOT for a public
                               issue: the tokens become a stable pseudonym, and anyone with the file
                               can reverse them. Cannot be combined with --full.

crystal-backup ` + CommandReport + ` [--from <file>] [--output <file>] [collection flags]
        Writes a self-contained HTML page. Two modes, and which one you get depends on --from.

        WITHOUT --from it collects the self-check itself, for right now, and formats that. This is
        the command to type when you do not know whether the installation is healthy. It needs a
        cluster — run it where the operator runs — and it takes every collection flag ` + CommandSelfcheck + `
        takes above, with the same meaning and the same default: identifiers redacted with a fresh
        random salt.

        WITH --from <file> it formats a self-check JSON that was collected somewhere else and needs
        NO cluster access at all: attach the JSON to an issue and a maintainer regenerates the page
        at their desk. In this mode the collection flags are REFUSED rather than ignored — the file
        was already collected, and nothing here can change what is in it.
`

// RunSelfcheck is `crystal-backup selfcheck`. It returns a process exit code.
//
// The exit code is 0 for any report it managed to produce, INCLUDING one full of breaches. That is
// deliberate: this is a reporting command, and a non-zero exit on a degraded installation would
// make it unusable in the one place it is most wanted — a support script that collects a report
// and attaches it, which must not abort because the thing it is diagnosing is broken. Failure to
// produce a report at all is what exits non-zero.
func RunSelfcheck(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return runSelfcheck(ctx, args, stdout, stderr, connectCluster)
}

// runSelfcheck is RunSelfcheck with the cluster connection handed in, which is the only way a test
// can exercise the collecting half of either subcommand without a cluster.
func runSelfcheck(ctx context.Context, args []string, stdout, stderr io.Writer, connect clusterConnector) int {
	fs := flag.NewFlagSet(CommandSelfcheck, flag.ContinueOnError)
	fs.SetOutput(stderr)
	output := fs.String("output", "", "write the JSON report here instead of stdout")
	cf := registerCollectFlags(fs)
	fs.Usage = func() { _, _ = fmt.Fprint(stderr, Usage) }
	if err := fs.Parse(args); err != nil {
		return 2
	}

	rep, code := cf.collect(ctx, CommandSelfcheck, connect, "", stderr)
	if rep == nil {
		return code
	}
	body, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "selfcheck: encode report: %v\n", err)
		return 1
	}
	body = append(body, '\n')
	if err := emit(*output, body, stdout); err != nil {
		_, _ = fmt.Fprintf(stderr, "selfcheck: %v\n", err)
		return 1
	}
	cf.announce(CommandSelfcheck, *output, stderr)
	return 0
}

// RunReport is `crystal-backup report [--from <file>]`.
//
// TWO MODES, and the difference between them is the whole design of this command.
//
// WITH --from it touches no cluster and holds no client. That is not an optimisation, it is the
// feature: the workflow this lot was built for — someone attaches a JSON to an issue, a maintainer
// renders it — only works if rendering is possible from the file alone, on a machine that has never
// seen the cluster.
//
// WITHOUT --from it collects the self-check itself, for this instant, through the very same Collect
// the `selfcheck` subcommand calls, and formats that. The reason is an incident on 2026-08-07: the
// only way to get a readable picture of an installation was to run `selfcheck`, capture 17 KB of
// JSON and parse it by hand — because the half of this binary that knows how to format that
// document insisted on a file nobody had. Both halves live in one image with one RBAC, so the file
// was never anything but a hurdle. A `report` that needs a file is a `report` nobody can use in the
// moment they need it, and this is meant to be THE command an operator types when they do not know
// whether things are healthy.
//
// The modes are kept apart by refusing the other one's flags rather than ignoring them (see
// runReport): a --full that silently did nothing because a file had already been collected redacted
// is the shape of defect this whole package exists to refuse.
//
// The exit code follows RunSelfcheck's rule for the same reason: 0 for any page it managed to write,
// however bad the news on it. Only failing to produce a page exits non-zero.
func RunReport(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return runReport(ctx, args, stdout, stderr, connectCluster)
}

func runReport(ctx context.Context, args []string, stdout, stderr io.Writer, connect clusterConnector) int {
	fs := flag.NewFlagSet(CommandReport, flag.ContinueOnError)
	fs.SetOutput(stderr)
	from := fs.String("from", "",
		"format this selfcheck JSON, needing NO cluster access at all; omit it to collect the "+
			"self-check from the cluster first and format that")
	output := fs.String("output", "", "write the HTML here instead of stdout")
	cf := registerCollectFlags(fs)
	fs.Usage = func() { _, _ = fmt.Fprint(stderr, Usage) }
	if err := fs.Parse(args); err != nil {
		return 2
	}

	var (
		rep  *Report
		code int
	)
	if *from == "" {
		rep, code = cf.collect(ctx, CommandReport, connect, reportNeedsCluster, stderr)
	} else {
		// Refused, not ignored. Every one of these flags describes a COLLECTION, and a file handed to
		// --from was collected by somebody else, at another instant, with the redaction they chose:
		// there is nothing left for --full or --operator-namespace to affect. Accepting them would
		// print a fully redacted page to someone who typed --full and believed they were looking at
		// real names — which is worse than either mode failing.
		if typed := typedCollectFlags(fs); len(typed) > 0 {
			_, _ = fmt.Fprintf(stderr,
				"report: --from formats a self-check that was ALREADY collected, so --%s cannot change "+
					"anything about it: the namespace, the images and the redaction were all decided by "+
					"whoever ran `%s`. Drop the flag to format the file, or drop --from to collect a "+
					"fresh self-check here and now.\n\n%s",
				strings.Join(typed, ", --"), CommandSelfcheck, Usage)
			return 2
		}
		rep, code = readReport(*from, stderr)
	}
	if rep == nil {
		return code
	}

	page, err := Render(rep)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "report: %v\n", err)
		return 1
	}
	if err := emit(*output, page, stdout); err != nil {
		_, _ = fmt.Fprintf(stderr, "report: %v\n", err)
		return 1
	}
	// Only on the self-generating path: in --from mode the redaction was settled by the producer and
	// this command has no salt to describe, which the page itself already states.
	if *from == "" {
		cf.announce(CommandReport, *output, stderr)
	}
	return 0
}

// reportNeedsCluster is appended when a self-generating `report` cannot reach a cluster.
//
// It names the MODE, and that is the point of it existing. This is the one subcommand with a mode
// that needs a cluster and a mode that pointedly does not, so a reader who typed the first by
// accident would otherwise be told only that there is no kubeconfig — true, and no help at all in
// working out that the answer is four characters they did not type.
const reportNeedsCluster = "\nreport: no --from was given, so this command has to COLLECT the " +
	"self-check before it can format it, and collecting needs a cluster — run it where the operator " +
	"runs (`kubectl exec deploy/crystal-backup -- /manager report`). Pass --from <file> to format a " +
	"self-check JSON that was collected elsewhere; that mode needs no cluster at all."

// readReport is the --from half: read the file, decode it, and say so when it comes from a newer
// producer than this binary.
func readReport(path string, stderr io.Writer) (*Report, int) {
	raw, err := os.ReadFile(path)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "report: %v\n", err)
		return nil, 1
	}
	rep, err := Parse(raw)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "report: %v\n", err)
		return nil, 1
	}
	// A report from a NEWER producer is rendered anyway, with a warning. Refusing would be the
	// wrong instinct for a format whose entire purpose is to be read years later by whatever
	// binary the reader happens to have: an imperfect page beats no page, and the warning says
	// which fields the reader should not trust.
	if rep.ReportVersion > ReportVersion {
		_, _ = fmt.Fprintf(stderr,
			"report: this file is schema v%d and this binary understands v%d — rendering anyway; "+
				"fields added after v%d will not appear.\n", rep.ReportVersion, ReportVersion, ReportVersion)
	}
	return rep, 0
}

// collectFlags are the flags that describe a COLLECTION: which namespace to read, which configured
// images to record beside the running ones, and what to do to the identifiers on the way out.
//
// They are registered on BOTH subcommands from this one place because a `report` with no --from does
// exactly what `selfcheck` does and then formats it. Declaring them twice would not be untidy so
// much as dangerous: a --full that redacted nothing in one command and something in the other, or
// two spellings of the operator namespace default, would be a discrepancy discovered at 3am by
// somebody holding a report they now cannot trust.
type collectFlags struct {
	namespace  *string
	moverImage *string
	syncImage  *string
	full       *bool
	saltFile   *string

	// salt is saltFile's loaded contents, kept so the closing message can state which promise this
	// report's tokens make. Nil is the default and means a fresh random salt.
	salt []byte
}

func registerCollectFlags(fs *flag.FlagSet) *collectFlags {
	return &collectFlags{
		namespace: fs.String("operator-namespace", defaultNamespace(),
			"namespace holding the operator and its mover Jobs"),
		moverImage: fs.String("mover-image", "",
			"the configured mover image, recorded beside what is actually running"),
		syncImage: fs.String("sync-image", "", "the configured sync image, likewise"),
		full: fs.Bool("full", false,
			"DISABLE redaction: namespace, tenant, PVC, bucket, endpoint and cluster identifiers appear verbatim"),
		saltFile: fs.String("redaction-salt-file", "",
			"salt the tokens from this file (>= 32 raw bytes) so reports taken on different days correlate"),
	}
}

// typedCollectFlags returns the collection flags the caller actually TYPED, in the order the
// FlagSet holds them.
//
// flag.FlagSet.Visit walks only the flags that were set, and that distinction is the whole reason
// this is not a comparison against the defaults: --operator-namespace carries a default that comes
// from $POD_NAMESPACE, so a `report --from file.json` run inside the operator pod would otherwise
// be refused for a flag nobody typed.
func typedCollectFlags(fs *flag.FlagSet) []string {
	collection := map[string]bool{
		"operator-namespace": true, "mover-image": true, "sync-image": true,
		"full": true, "redaction-salt-file": true,
	}
	var typed []string
	fs.Visit(func(f *flag.Flag) {
		if collection[f.Name] {
			typed = append(typed, f.Name)
		}
	})
	return typed
}

// collect is the ONE collection path: the flag rules, the salt, the client and Collect itself. It
// returns (nil, exit code) having already explained itself on stderr.
//
// Both `selfcheck` and a `report` with no --from come through here, and sharing it is the
// requirement rather than a convenience. A second collector for the second entry point would be
// free to drift — a different redaction default, a namespace resolved another way — and the two
// commands would then disagree about the same installation while both claiming to describe it.
//
// cmd prefixes the messages so a reader knows which subcommand is talking, and needsCluster is
// appended to a connection failure by the caller that has a second mode to point at.
func (f *collectFlags) collect(
	ctx context.Context, cmd string, connect clusterConnector, needsCluster string, stderr io.Writer,
) (*Report, int) {
	// Both flags together is an error rather than a precedence rule, and the choice is not a style
	// one. --full ignores the salt because there is nothing to salt — so a soak script that kept
	// --full from an earlier debugging session would emit a fortnight of VERBATIM namespace
	// inventories while its author believed they were reading pseudonyms. Refusing costs one line
	// in a script; accepting silently costs somebody their customer list.
	if *f.full && *f.saltFile != "" {
		_, _ = fmt.Fprint(stderr,
			cmd+": --full and --redaction-salt-file are mutually exclusive — --full redacts "+
				"nothing, so there is nothing for the salt to do. Drop one.\n\n"+Usage)
		return nil, 2
	}
	// Loaded BEFORE the cluster is touched, so a mistyped path fails in the first millisecond rather
	// than after a full collection, and never falls back to a random salt.
	if *f.saltFile != "" {
		salt, err := LoadRedactionSalt(*f.saltFile)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "%s: %v\n", cmd, err)
			return nil, 2
		}
		f.salt = salt
	}

	reader, info, err := connect()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%s: %v%s\n", cmd, err, needsCluster)
		return nil, 1
	}
	rep, err := Collect(ctx, Options{
		Reader:            reader,
		OperatorNamespace: *f.namespace,
		Now:               time.Now(),
		Full:              *f.full,
		RedactionSalt:     f.salt,
		Discovery:         info,
		DeclaredImages:    map[string]string{roleMover: *f.moverImage, roleSync: *f.syncImage},
	})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%s: %v\n", cmd, err)
		return nil, 1
	}
	return rep, 0
}

// announce says, on the way out, what was done to the identifiers in the file just written.
//
// On stderr and not only inside the document, because the person running this in a loop is reading
// the terminal and may never open what they collected. Silent when the output went to stdout, where
// it is already in front of them, and when --full was asked for explicitly.
func (f *collectFlags) announce(cmd, output string, stderr io.Writer) {
	switch {
	case output == "" || *f.full:
	case f.salt != nil:
		_, _ = fmt.Fprintf(stderr,
			"%s: wrote %s (identifiers redacted with YOUR salt: these tokens match every "+
				"other report made from the same salt file, so do not attach this to a public "+
				"issue — re-run without --redaction-salt-file for a copy that correlates with nothing)\n",
			cmd, output)
	default:
		_, _ = fmt.Fprintf(stderr,
			"%s: wrote %s (identifiers redacted; re-run with --full for an unredacted copy)\n", cmd, output)
	}
}

// clusterConnector opens what a collection reads: the operator's own client, and discovery when it
// can be had.
//
// It is a PARAMETER of the unexported entry points rather than a package-level hook a test could
// overwrite, because a global swapped by one test is a global still swapped in the next, and this
// package's entire subject is a report whose provenance can be trusted.
type clusterConnector func() (client.Reader, ServerInfo, error)

// connectCluster is the production connector, shared by both subcommands' collecting paths.
func connectCluster() (client.Reader, ServerInfo, error) {
	cfg, err := ctrl.GetConfig()
	if err != nil {
		return nil, nil, fmt.Errorf("no Kubernetes configuration: %w", err)
	}
	// A DIRECT client, not a manager cache. Starting informers for a one-shot command would mean
	// waiting for a full cluster-wide sync before printing anything, and would need list/watch where
	// this only needs list.
	c, err := client.New(cfg, client.Options{Scheme: buildScheme()})
	if err != nil {
		return nil, nil, fmt.Errorf("cannot build a client: %w", err)
	}
	// Discovery is optional, and its absence is reported as a diagnostic inside the report rather
	// than failing the run: without it the Kubernetes version is missing and the CRD inventory loses
	// its fallback, which is a smaller report, not a wrong one.
	var info ServerInfo
	if d, err := discovery.NewDiscoveryClientForConfig(cfg); err == nil {
		info = d
	}
	return c, info, nil
}

// Parse decodes a report, rejecting one that is not a report at all.
//
// The version check is the reason this is a function and not a bare json.Unmarshal: an empty or
// truncated file decodes into a zero Report without error, and rendering that would produce a
// plausible-looking page describing nothing — the worst possible outcome for a document whose job
// is to be trusted.
func Parse(raw []byte) (*Report, error) {
	var rep Report
	if err := json.Unmarshal(raw, &rep); err != nil {
		return nil, fmt.Errorf("not a valid selfcheck report: %w", err)
	}
	if rep.ReportVersion == 0 {
		return nil, fmt.Errorf("not a selfcheck report: no reportVersion field")
	}
	return &rep, nil
}

func emit(path string, body []byte, stdout io.Writer) error {
	if path == "" {
		_, err := stdout.Write(body)
		return err
	}
	// 0o600: a --full report is somebody's namespace inventory, and the default umask is not a
	// decision this command should be delegating.
	if err := os.WriteFile(path, body, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func defaultNamespace() string {
	if ns := os.Getenv("POD_NAMESPACE"); ns != "" {
		return ns
	}
	return apiconst.DefaultOperatorNamespace
}

// buildScheme carries only what the collector reads. Jobs and PVCs are here for the leak census;
// the VolumeSnapshot kinds are deliberately NOT, because the collector asks for them through
// unstructured by GVK exactly as internal/metrics does — a client whose scheme knew them would
// still need the CRDs to exist, and going through unstructured is what makes their absence a
// clean, catchable no-match instead of a panic at init.
func buildScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(cbv1.AddToScheme(s))
	utilruntime.Must(batchv1.AddToScheme(s))
	utilruntime.Must(corev1.AddToScheme(s))
	return s
}
