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
const Usage = `crystal-backup ` + CommandSelfcheck + ` [flags]
        Reads the cluster with the operator's own RBAC and writes a JSON installation report.

        --output <file>        write here instead of stdout
        --operator-namespace   where the operator and its mover Jobs live (default $POD_NAMESPACE)
        --mover-image          the configured mover image, recorded beside what is running
        --sync-image           the configured sync image, likewise
        --redaction-salt-file  redact under a salt YOU hold (>= 32 raw bytes) instead of a random
                               one, so tokens correlate across reports. Never send it with them.
        --full                 DISABLE redaction. Identifiers appear verbatim; share privately only.

crystal-backup ` + CommandReport + ` --from <file> [--output <file>]
        Renders a self-contained HTML page from a report JSON. Needs NO cluster access: attach the
        JSON to an issue and a maintainer regenerates the page at their desk.
`

// RunSelfcheck is `crystal-backup selfcheck`. It returns a process exit code.
//
// The exit code is 0 for any report it managed to produce, INCLUDING one full of breaches. That is
// deliberate: this is a reporting command, and a non-zero exit on a degraded installation would
// make it unusable in the one place it is most wanted — a support script that collects a report
// and attaches it, which must not abort because the thing it is diagnosing is broken. Failure to
// produce a report at all is what exits non-zero.
func RunSelfcheck(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet(CommandSelfcheck, flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		output     = fs.String("output", "", "write the JSON report here instead of stdout")
		namespace  = fs.String("operator-namespace", defaultNamespace(), "namespace holding the operator and its mover Jobs")
		moverImage = fs.String("mover-image", "", "the configured mover image, recorded beside what is actually running")
		syncImage  = fs.String("sync-image", "", "the configured sync image, likewise")
		saltFile   = fs.String("redaction-salt-file", "",
			"redact under the salt in this file (>= 32 raw bytes) rather than a random one, so tokens "+
				"correlate across reports and across a soak's streams")
		full = fs.Bool("full", false,
			"DISABLE redaction: namespace, tenant, PVC, bucket, endpoint and cluster identifiers appear verbatim")
	)
	fs.Usage = func() { _, _ = fmt.Fprint(stderr, Usage) }
	if err := fs.Parse(args); err != nil {
		return 2
	}
	// Read before touching the cluster. A salt file that is missing or short must fail here, on
	// the operator's terminal, and not after a full collection has already been done under a
	// random salt the caller did not ask for.
	salt, err := ReadSaltFile(*saltFile)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "selfcheck: %v\n", err)
		return 2
	}

	cfg, err := ctrl.GetConfig()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "selfcheck: no Kubernetes configuration: %v\n", err)
		return 1
	}

	// A DIRECT client, not a manager cache. Starting informers for a one-shot command would mean
	// waiting for a full cluster-wide sync before printing anything, and would need list/watch where
	// this only needs list.
	c, err := client.New(cfg, client.Options{Scheme: buildScheme()})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "selfcheck: cannot build a client: %v\n", err)
		return 1
	}
	var info ServerInfo
	if d, err := discovery.NewDiscoveryClientForConfig(cfg); err == nil {
		info = d
	}

	rep, err := Collect(ctx, Options{
		Reader:            c,
		OperatorNamespace: *namespace,
		Now:               time.Now(),
		Full:              *full,
		RedactionSalt:     salt,
		Discovery:         info,
		DeclaredImages:    map[string]string{roleMover: *moverImage, roleSync: *syncImage},
	})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "selfcheck: %v\n", err)
		return 1
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
	if *output != "" && !*full {
		_, _ = fmt.Fprintf(stderr,
			"selfcheck: wrote %s (identifiers redacted; re-run with --full for an unredacted copy)\n", *output)
	}
	return 0
}

// RunReport is `crystal-backup report --from <file>`.
//
// It touches no cluster and holds no client. That is not an optimisation, it is the feature: the
// whole workflow this lot is for — someone attaches a JSON to an issue, a maintainer renders it —
// only works if rendering is possible from the file alone, on a machine that has never seen the
// cluster.
func RunReport(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet(CommandReport, flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		from   = fs.String("from", "", "the selfcheck JSON to render (required)")
		output = fs.String("output", "", "write the HTML here instead of stdout")
	)
	fs.Usage = func() { _, _ = fmt.Fprint(stderr, Usage) }
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *from == "" {
		_, _ = fmt.Fprint(stderr, "report: --from <file> is required\n\n"+Usage)
		return 2
	}
	raw, err := os.ReadFile(*from)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "report: %v\n", err)
		return 1
	}
	rep, err := Parse(raw)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "report: %v\n", err)
		return 1
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
	page, err := Render(rep)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "report: %v\n", err)
		return 1
	}
	if err := emit(*output, page, stdout); err != nil {
		_, _ = fmt.Fprintf(stderr, "report: %v\n", err)
		return 1
	}
	return 0
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

// ReadSaltFile reads a redaction salt. An empty path is not an error — it means "use a random
// salt" — and returns nil, which is what Options.RedactionSalt reads as absence.
//
// The length check is here rather than in the caller so that every entry point enforces the same
// floor with the same words: `selfcheck --redaction-salt-file`, `soak-collect` and `soak-export`
// all go through this, and hack/soak/collect.sh checks the same 32 bytes before it will even
// attempt the token check. A salt short enough to disagree about is a salt short enough to guess.
func ReadSaltFile(path string) ([]byte, error) {
	if path == "" {
		return nil, nil
	}
	salt, err := os.ReadFile(path) // #nosec G304 -- an operator-supplied path is the whole point
	if err != nil {
		return nil, fmt.Errorf("read the redaction salt: %w", err)
	}
	if len(salt) < MinSaltBytes {
		return nil, fmt.Errorf(
			"the redaction salt file %s is %d bytes; %d is the minimum (openssl rand -out soak-salt.bin %d)",
			path, len(salt), MinSaltBytes, MinSaltBytes)
	}
	return salt, nil
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
