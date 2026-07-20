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

// Command crystal-mover is CrystalBackup's mover shim: the thin process a short-lived mover
// Job runs as its container entrypoint (mover.MoverBinaryPath). It runs exactly ONE restic
// operation and reports the outcome back to the controller through the pod's termination
// message. It is the CONSUMER half of the mover runtime contract that internal/mover pins;
// the JobBuilder there is the PRODUCER, so this binary deliberately re-uses that package's
// constants and result types rather than restating the wire format.
//
// What it does NOT do. The shim never constructs the repository URL, the password, or the S3
// credentials: the Job injects those entirely through the environment (RESTIC_REPOSITORY,
// RESTIC_PASSWORD_FILE pointing into a read-only Secret mount, AWS_* via secretKeyRef), and
// the shim simply inherits that environment when it execs restic. Keeping secrets off argv is
// a security property of the whole design — a credential on the command line would leak into
// `kubectl get pod -o yaml`, the Job spec and in-cluster process listings — so the shim is
// careful to forward only the restic subcommand and its non-secret flags, and to keep secret
// material out of the error string it reports.
//
// Invocation. The container command is crystal-mover; its args are
//
//	--operation <op> -- <restic argv...>
//
// where <op> is one of the mover.Op* values and everything after the "--" separator is
// restic's own argv (subcommand + flags). The Go flag package stops parsing at "--", so
// fs.Args() yields the restic argv verbatim, untouched by the shim's own flag set.
//
// Result protocol. On exit the shim marshals a tiny mover.MoverResult JSON to the termination
// message path (mover.TerminationMessagePath, one of the few writable paths under the mover's
// read-only root filesystem). The kubelet surfaces those bytes on
// pod.Status.ContainerStatuses[0].State.Terminated.Message, which the controller reads and
// ParseMoverResult()s. A BLANK message is treated by that parser as a hard failure ("the
// container died before it could write"), so the shim always writes a result — success or
// failure — before it exits, and exits 0 only when the result is OK.
//
// main() is deliberately thin: it parses flags, wires restic's stdio, execs it, and hands the
// captured stdout + run error to the pure decision funcs in run.go (buildResult,
// ensureSummaryJSON, writeResult), which is where all the testable logic lives.
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"

	"github.com/CrystalBackup/CrystalBackup/internal/manifests"
	"github.com/CrystalBackup/CrystalBackup/internal/mover"
)

func main() {
	// A dedicated FlagSet with ContinueOnError (rather than the process-global flag.CommandLine,
	// which exits the process on a parse error) lets the shim turn a bad invocation into a
	// proper failure MoverResult on the termination log instead of a blank message the
	// controller would have to guess about.
	fs := flag.NewFlagSet("crystal-mover", flag.ContinueOnError)
	operation := fs.String("operation", "",
		"the mover operation to run: one of backup, restore, init, forget, prune, check, snapshots, "+
			"unlock (mover.Op* values)")
	terminationLog := fs.String("termination-log", mover.TerminationMessagePath,
		"path the MoverResult JSON is written to; defaults to the kubelet's termination message file, "+
			"overridable so tests can point it at a temp file")

	// Flags precede "--"; the restic argv follows it. On a parse error we still have a usable
	// termination-log path (its default) and whatever --operation parsed, so we can report a
	// failure rather than dying silently.
	if err := fs.Parse(os.Args[1:]); err != nil {
		fail(*terminationLog, *operation, fmt.Errorf("parse mover flags: %w", err))
	}
	resticArgv := fs.Args()

	// Reject an unknown operation before running anything. The Job builder only ever emits a
	// valid mover.Op* value, so this is defence in depth — but it is load-bearing defence: were
	// a mistyped "backup" to slip through, buildResult would take the maintenance path and
	// report OK for a run that produced no snapshot, silently masking a lost backup.
	if !knownOperation(*operation) {
		fail(*terminationLog, *operation, fmt.Errorf("unknown --operation %q", *operation))
	}
	// The restic subcommand and its args are mandatory; without them there is nothing to run.
	if len(resticArgv) == 0 {
		fail(*terminationLog, *operation, fmt.Errorf("no restic argv after the %q separator", "--"))
	}

	// A manifest backup has a step BEFORE restic: read the namespace from the API server and
	// write the sanitized tree that restic is about to snapshot. A failure here is fatal on
	// purpose — backing up an empty or half-written directory would produce a snapshot that
	// reports success and restores to nothing, which is the worst outcome a backup tool has.
	var manifestIndex *manifests.Index
	if mover.Operation(*operation) == mover.OpManifestsBackup {
		idx, err := dumpManifests(context.Background())
		if err != nil {
			fail(*terminationLog, *operation, err)
		}
		manifestIndex = idx
	}

	// For a backup or restore, guarantee restic emits a machine-readable summary we can parse;
	// a no-op for every other operation and when the caller already passed --json.
	resticArgv = ensureSummaryJSON(*operation, resticArgv)

	// Inherit the environment the Job set (RESTIC_REPOSITORY, RESTIC_PASSWORD_FILE, RESTIC_CACHE_DIR,
	// AWS_*, ...): a nil cmd.Env means "use the current process environment", so the shim passes
	// restic its repo, password file and credentials without ever naming them here.
	cmd := exec.Command("restic", resticArgv...)

	// stdout is captured into a buffer for parsing/logging; the exact wiring differs by operation
	// (see below). stderr is streamed live to the pod log AND tailed into a bounded buffer so the
	// shim can quote restic's final (fatal) line when it reports a failure.
	var stdout bytes.Buffer
	stderrTail := &tailWriter{max: maxStderrTailBytes}
	cmd.Stderr = io.MultiWriter(os.Stderr, stderrTail)
	if parsesJSONSummary(*operation) {
		// A backup's or restore's stdout is restic's --json firehose (status objects then the
		// summary): capture it for parsing but keep it OUT of the pod log. The human-useful
		// signal — warnings about unreadable/unrestorable files — is on stderr, which we
		// stream; teeing the JSON stream on top would only bury it.
		cmd.Stdout = &stdout
	} else {
		// Maintenance stdout is human-readable result text (init/forget/prune/check output); tee it
		// to the pod log AND capture it so the logs stay complete.
		cmd.Stdout = io.MultiWriter(os.Stdout, &stdout)
	}

	runErr := cmd.Run()
	if runErr != nil {
		// Fold restic's final stderr line into the error so the reported reason is specific
		// ("exit status 1: Fatal: repository is already locked") rather than a bare exit code.
		// restic does not echo the password (read from a file) or the AWS secret on stderr, so the
		// tail is credential-free; clampError still single-lines and length-bounds it.
		if line := lastLine(stderrTail.Bytes()); line != "" {
			runErr = fmt.Errorf("%w: %s", runErr, line)
		}
	}

	result := buildResult(*operation, stdout.Bytes(), runErr)
	// Fold the dump's accounting into the result the controller reads. Only on success: a
	// resource count attached to a failed snapshot would claim a capture the repository does
	// not hold.
	if manifestIndex != nil && result.OK {
		// A namespace's object count fits an int32 by orders of magnitude.
		result.ResourceCount = int32(manifestIndex.ResourceCount) //nolint:gosec // bounded above
		result.IncompleteManifests = len(manifestIndex.Warnings) > 0
	}

	// A manifest restore has its step AFTER restic: the tree has to exist before anything can
	// be applied. Gated on result.OK because applying a half-restored tree would create a
	// partial namespace and report it as a restore.
	if mover.Operation(*operation) == mover.OpManifestsRestore && result.OK {
		applied, err := applyManifests(context.Background())
		if err != nil {
			fail(*terminationLog, *operation, err)
		}
		result = reportToResult(result, applied)
	}
	report(*terminationLog, result)
}

// report is the shim's single exit point once a MoverResult exists. It persists the result to
// the termination log (the controller's only channel), echoes it to stderr for the pod log, and
// exits 0 on success / 1 on failure so the Job's restartPolicy:Never + backoffLimit see the
// right pod phase. It never returns.
func report(path string, result mover.MoverResult) {
	if err := writeResult(path, result); err != nil {
		// If we cannot write the termination log the controller will read a blank message and
		// (correctly, per ParseMoverResult) treat the run as failed; log loudly so the pod log
		// still explains what happened, then fall through to exit non-zero.
		fmt.Fprintf(os.Stderr, "crystal-mover: writing termination message to %q: %v\n", path, err)
	}
	// Echo the result as one line to the pod log so an operator reading `kubectl logs` sees the
	// same verdict the controller will parse.
	if encoded, err := result.Encode(); err == nil {
		fmt.Fprintln(os.Stderr, encoded)
	}
	if result.OK {
		os.Exit(0)
	}
	os.Exit(1)
}

// fail reports a failure that happened BEFORE (or instead of) a clean restic run — bad flags,
// an unknown operation, a missing restic argv. It funnels through buildResult (with a non-nil
// error, which always yields OK:false) so the pre-run and post-run failure shapes are identical,
// then reports and exits. It never returns.
func fail(path, operation string, err error) {
	report(path, buildResult(operation, nil, err))
}
