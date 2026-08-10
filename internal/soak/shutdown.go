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
	"fmt"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------------------------
// THE SHUTDOWN REPORT, and the incident that bought it.
//
// Observed on a customer cluster, in this order:
//
//  1. an autosync reconciler replaced the collector's pod at the moment the fortnight's archive
//     was about to be exported;
//  2. `Multi-Attach error for volume … already exclusively attached to one node`, then the attach
//     succeeded on the NEW node;
//  3. `rbd: map failed: (22) Invalid argument` — the replacement pod sat in ContainerCreating
//     indefinitely, for a reason that had nothing to do with this product (most of that cluster's
//     nodes ran a kernel ceph-csi calls "does not support required features", so exactly one node
//     could ever map that image);
//  4. hack/soak/collect.sh: `NOT COLLECTED: deploy/crystal-backup-soak exists but has no Running
//     pod`, exit 3.
//
// The archive was intact and unreachable, on a volume the operator was about to delete. What
// saved the campaign was that somebody had run `soak-export --status` minutes earlier and
// transcribed its sizing figures BY HAND.
//
// So: the collector does that transcription itself, every time it is asked to stop, into the one
// place that is not the volume it is about to release — its own log.
//
// WHAT WAS REJECTED, and why.
//
//   - `strategy: Recreate` as the answer. It was ALREADY in templates/soak.yaml, with a comment
//     explaining that it prevents a rolling update from deadlocking on the multi-attach. It does
//     prevent that, and it did not help here: Recreate makes the handover brief and orderly, and
//     an orderly handover to a node that cannot map the image still loses the volume. It is kept,
//     and it is not a defence. This is the correction to that comment.
//
//   - a preStop exec hook running `soak-export --status`. It reads like the obvious answer and it
//     writes to nowhere: the kubelet does not append lifecycle-hook output to the container log,
//     and the only place it ever surfaces is a FailedPreStopHook Event when the hook exits
//     non-zero. A report that is only visible when it fails is not a report. The process's own
//     SIGTERM path is the one place in a terminating pod whose stdout is still the pod's log.
//
//   - writing the report to the volume. That is where the archive already is, and the failure
//     being defended against is precisely that the volume is unreachable.
//
//   - recording it as a cluster object (a ConfigMap, an Event). The collector's RBAC is read-only
//     cluster-wide and hack/soak/SPEC.md §10 makes that a promise rather than an accident. Buying
//     one report with a write verb held for a fortnight is the wrong trade.
//
//   - folding it into the daily heartbeat line. §9 promises one line a day plus one at startup,
//     byte-identical between unchanged days. This is neither daily nor one line, and the reason it
//     is not one line is the payload: the per-class high-water figures are the part of the archive
//     that cannot be reconstructed from anything else, and they are what a human has to be able to
//     copy off a screen. The heartbeat carries mover COUNTS; it has never carried the peaks.
//
// WHAT IT IS HONEST ABOUT. A pod's log dies with the pod, so this is durable exactly as far as the
// cluster's log pipeline is — the same reach §9 already claims for the daily line ("it lands in
// kubectl logs and therefore in whatever log pipeline the cluster already has"). It is insurance,
// not an archive. The archive is still exported by hack/soak/README.md's procedure, and the report
// says so in its last line.
// ---------------------------------------------------------------------------------------------

// shutdownPrefix is what a reader greps for, and it is on EVERY line of the block rather than only
// the first.
//
// The heartbeat is one line because seven of them have to fit on a screen. This is a block, and a
// block whose continuation lines carry no marker is unfindable in a shared log stream: it is
// emitted at the moment several pods are being replaced at once, into a pipeline that interleaves
// them, and `grep soak-shutdown` has to return the whole thing or it returns nothing useful. The
// twenty characters per line are the price of that.
const shutdownPrefix = "WARN soak-shutdown |"

// ShutdownReport is what the collector says on its way out: the figures that exist only on the
// volume it is about to release, plus what to do about it.
//
// It NEVER fails and never returns an empty string. A collector eleven seconds old has nothing to
// report and must still report that — a report withheld because a directory was unreadable would
// vanish on exactly the shutdown where it matters, and an empty string would be indistinguishable
// from a collector that was SIGKILLed before it could speak.
func ShutdownReport(store *Store, info CollectorInfo, now time.Time) string {
	now = now.UTC()
	dir := ""
	if store != nil {
		dir = store.Dir()
	}

	lines := []string{
		fmt.Sprintf("at=%s data-dir=%s", now.Format(time.RFC3339), dirWord(dir)),
		"",
		"The collector has been asked to stop, and everything it has collected is in that",
		"directory and NOWHERE ELSE. Its PVC is ReadWriteOnce by default: handing that volume to",
		"a replacement pod means moving an exclusive attachment between nodes, and that can fail",
		"for reasons that have nothing to do with this product — a node whose kernel cannot map",
		"the image, a stale attachment, a CSI controller mid-restart. When it does fail, the",
		"replacement pod stays in ContainerCreating and the archive is intact and unreachable.",
		"",
		"THE LINES BELOW ARE THE FIGURES THAT ARE NOT RECOVERABLE FROM ANYWHERE ELSE. If you are",
		"replacing this pod, or deleting its PVC, copy them somewhere before you do.",
		"",
	}

	if store == nil {
		// Defensive, and it says which of the two silences this is. A report that just stopped here
		// would read as "nothing collected".
		lines = append(lines, "!! no store: this collector never opened its data directory, so there are no")
		lines = append(lines, "   figures to carry. That is a startup failure, not an empty soak.")
		return joinShutdown(lines)
	}

	uptime, _ := ReadUptime(dir, now)
	lines = append(lines, fmt.Sprintf("up %.1f%% of the %.1f day(s) since it first started, across %d session(s)",
		uptime.Fraction*100, uptime.SpanSeconds/86400, len(uptime.Sessions)))
	// Unconditional, and `unknown` when the collector could not name its own build. A version this
	// block omitted would be read as agreeing with whatever the previous block said, and an upgrade
	// mid-soak is the one change that invalidates the peak table below it (§9).
	lines = append(lines, fmt.Sprintf("operator version     %s", versionWord(info.OperatorVersion)))
	lines = append(lines, "")

	// The SAME lines `soak-export --status` prints, from the same function. Not a second rendering
	// of the same numbers: the status screen is what the incident's operator transcribed by hand,
	// and a report that reformatted it would eventually disagree with it about which classes were
	// measured. statusLines already prints a class at zero rather than omitting it, which is the
	// property that matters most here.
	lines = append(lines, statusLines(dir, info)...)

	if total, err := store.Footprint(); err == nil {
		line := fmt.Sprintf("on disk              %s", humanBytes(total))
		if info.MaxBytes > 0 {
			line += fmt.Sprintf(" of %s", humanBytes(info.MaxBytes))
		}
		lines = append(lines, line)
	}
	if drops := store.Drops(); len(drops) > 0 {
		lines = append(lines, fmt.Sprintf("DROPPED BY THE CAP   %d segment(s)", len(drops)))
	}
	if degraded, since, reason := store.DegradedSince(); degraded {
		lines = append(lines, fmt.Sprintf("DEGRADED since %s: %s", since.UTC().Format(time.RFC3339), reason))
	}

	lines = append(lines,
		"",
		"Before you replace this pod again: export the archive first, then let your reconciler",
		"remove the Deployment and the PVC. hack/soak/README.md, \"Ending a series and starting",
		"another\", is the procedure — including why `kubectl scale`/`kubectl delete` is undone",
		"within minutes on a cluster with an autosync reconciler.",
	)
	return joinShutdown(lines)
}

// joinShutdown puts the marker on every line, including the blank ones — a blank line without it
// is a line the grep above drops, and the block would come back with its paragraphs run together.
func joinShutdown(lines []string) string {
	var b strings.Builder
	for i, l := range lines {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(shutdownPrefix)
		if l != "" {
			b.WriteByte(' ')
			b.WriteString(l)
		}
	}
	return b.String()
}

// dirWord keeps the first line parseable when there is no directory to name. An empty value after
// `data-dir=` reads as a truncated line rather than as a collector that never opened one.
func dirWord(dir string) string {
	if dir == "" {
		return "(none)"
	}
	return dir
}
