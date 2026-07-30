//go:build e2e
// +build e2e

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

package e2e

import (
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"

	"github.com/CrystalBackup/CrystalBackup/test/utils"
)

// ---------------------------------------------------------------------------------------------
// The uninstall order, encoded.
//
// Six of the twelve kinds carry a controller finalizer (crystalbackup.io/location, /repository,
// /backup, /restore-teardown, /cluster-restore-teardown). The ONLY process that removes them is
// the operator. Delete the operator first and every object still carrying one becomes
// unfinalizable: its namespace hangs in Terminating forever, and a `kubectl delete crd` — which
// waits for every instance — never returns.
//
// That is not a bug, it is how every finalizer-based operator behaves. It is also how this suite
// burned 35 minutes: `make undeploy` removes the Deployment, its namespace and the CRDs in ONE
// `kubectl delete`, so the operator died while four Backups in a demo namespace still held
// crystalbackup.io/backup, and the CRD deletion in the same command waited on objects nobody
// could release any more. The CI machine wins that race often enough to look green; a loaded
// laptop does not.
//
// The rule this file encodes, and which docs/DECOMMISSION.md §3 states for administrators:
// delete the custom resources FIRST, with the operator still running, WAIT for them to actually
// be gone, and only then remove the operator and the CRDs.
// ---------------------------------------------------------------------------------------------

// crystalBackupResources is every CRD kind the operator owns, ordered so that objects whose
// teardown depends on another are deleted first: restores and syncs before the backups they
// read, backups before the locations that address their repository, repositories last.
//
// Fully qualified on purpose: `backups` alone is ambiguous against other operators installed in
// the same cluster, and this list drives deletes.
var crystalBackupResources = []string{
	"restores.crystalbackup.io",
	"clusterrestores.crystalbackup.io",
	"backupexternalsyncs.crystalbackup.io",
	"clusterbackupexternalsyncs.crystalbackup.io",
	"backupschedules.crystalbackup.io",
	"clusterbackupschedules.crystalbackup.io",
	"clustererasures.crystalbackup.io",
	"clusterbackups.crystalbackup.io",
	"backups.crystalbackup.io",
	"backuplocations.crystalbackup.io",
	"clusterbackuplocations.crystalbackup.io",
	"backuprepositories.crystalbackup.io",
}

// liveCR is one CrystalBackup object still present in the cluster.
type liveCR struct {
	resource   string // fully qualified, e.g. backups.crystalbackup.io
	namespace  string // empty for a cluster-scoped kind
	name       string
	finalizers string // raw jsonpath rendering of .metadata.finalizers, for the failure report
}

func (c liveCR) String() string {
	where := c.name
	if c.namespace != "" {
		where = c.namespace + "/" + c.name
	}
	return fmt.Sprintf("%s %s finalizers=%s", c.resource, where, c.finalizers)
}

// nsArgs returns the `-n <ns>` pair for a namespaced object, or nothing.
func (c liveCR) nsArgs() []string {
	if c.namespace == "" {
		return nil
	}
	return []string{"-n", c.namespace}
}

// listCrystalBackupCRs returns every live CrystalBackup object in the cluster.
//
// A kind whose CRD is not installed simply yields a list error, which is skipped: the e2e
// containers install and uninstall the CRDs in randomized order, so "no such resource type" is a
// normal state here, not a failure.
func listCrystalBackupCRs() []liveCR {
	var live []liveCR
	for _, res := range crystalBackupResources {
		out, err := kubectl("get", res, "--all-namespaces",
			"-o", `jsonpath={range .items[*]}{.metadata.namespace}{"|"}{.metadata.name}{"|"}`+
				`{.metadata.finalizers}{"\n"}{end}`)
		if err != nil {
			continue
		}
		for _, line := range utils.GetNonEmptyLines(out) {
			parts := strings.SplitN(strings.TrimSpace(line), "|", 3)
			if len(parts) != 3 || parts[1] == "" {
				continue
			}
			live = append(live, liveCR{
				resource: res, namespace: parts[0], name: parts[1], finalizers: parts[2],
			})
		}
	}
	return live
}

// operatorIsServing reports whether at least one controller-manager pod is Ready and NOT
// terminating — i.e. whether anything is currently able to clear a finalizer.
//
// This is the distinction the teardown turns on. Objects that keep their finalizers while an
// operator is Ready are a product defect and must fail the suite. Objects that keep them with no
// operator running are ordinary teardown residue: forcing those is legitimate cleanup.
func operatorIsServing() bool {
	out, err := kubectl("get", "pods", "-A", "-l", "control-plane=controller-manager",
		"-o", `jsonpath={range .items[*]}{.metadata.deletionTimestamp}{"|"}`+
			`{.status.conditions[?(@.type=='Ready')].status}{"\n"}{end}`)
	if err != nil {
		return false
	}
	for _, line := range utils.GetNonEmptyLines(out) {
		parts := strings.SplitN(strings.TrimSpace(line), "|", 2)
		if len(parts) == 2 && parts[0] == "" && strings.TrimSpace(parts[1]) == "True" {
			return true
		}
	}
	return false
}

// drainCustomResources deletes every CrystalBackup object in the cluster and waits, BOUNDED, for
// them to actually disappear. Every delete is `--wait=false`: the waiting is done here, against
// the whole set, on one budget, instead of in a kubectl that would block forever on the first
// object whose finalizer never clears.
//
// Returns what is still standing when the budget expires, and whether an operator was Ready for
// the WHOLE window (a single poll that finds none flips it to false and it never flips back — an
// operator that died mid-drain cannot be held responsible for what it did not release).
func drainCustomResources(budget time.Duration) (remaining []liveCR, operatorServedThroughout bool) {
	served := operatorIsServing()

	deadline := time.Now().Add(budget)
	for {
		live := listCrystalBackupCRs()
		if len(live) == 0 {
			return nil, served
		}
		// Re-issued every round, not just once: a controller that is still fanning out (a
		// ClusterBackup stamping its per-namespace Backups) can mint an object after the first
		// pass, and an undeleted newcomer would hold the drain open until the budget expired.
		for _, c := range live {
			args := append([]string{"delete", c.resource, c.name}, c.nsArgs()...)
			_, _ = kubectl(append(args, "--ignore-not-found", "--wait=false")...)
		}
		if !operatorIsServing() {
			served = false
		}
		if time.Now().After(deadline) {
			return listCrystalBackupCRs(), served
		}
		time.Sleep(3 * time.Second)
	}
}

// forceReleaseFinalizers strips the finalizers from objects the operator will never release,
// because it is gone or about to be. Called only after drainCustomResources has given the
// operator its full budget, and always alongside a loud report of what had to be forced.
func forceReleaseFinalizers(objs []liveCR) {
	for _, c := range objs {
		args := append([]string{"patch", c.resource, c.name}, c.nsArgs()...)
		_, _ = kubectl(append(args, "--type=merge", "-p", `{"metadata":{"finalizers":null}}`)...)
	}
}

// teardownCustomResources is the step every container must run BEFORE it removes the operator,
// whether by `make undeploy` or `helm uninstall`.
//
// It drains the CRs on a bounded budget, and if anything survives it says so loudly on
// GinkgoWriter and forces the finalizers off so the teardown that follows cannot wedge — a
// cleanup step that does not return loses the run AND the diagnosis.
//
// It returns a non-empty string only for the case that is a PRODUCT DEFECT: an operator was
// Ready throughout and still did not release. The caller asserts on it at the very END of its
// teardown, so the suite goes red without the assertion skipping the rest of the cleanup. When
// no operator was serving, the leftovers are orphans from an earlier container and forcing them
// is the legitimate cleanup, not a finding — reported, not failed.
func teardownCustomResources(budget time.Duration) string {
	By("deleting every CrystalBackup custom resource while the operator can still clear finalizers")
	remaining, served := drainCustomResources(budget)
	if len(remaining) == 0 {
		return ""
	}

	report := new(strings.Builder)
	for _, c := range remaining {
		fmt.Fprintf(report, "  - %s\n", c)
	}
	_, _ = fmt.Fprintf(GinkgoWriter,
		"TEARDOWN: %d CrystalBackup object(s) still held a finalizer after %s "+
			"(operator Ready throughout: %t):\n%s",
		len(remaining), budget, served, report)

	By("force-clearing the finalizers left behind so the teardown cannot block")
	forceReleaseFinalizers(remaining)

	if !served {
		return ""
	}
	return fmt.Sprintf(
		"%d CrystalBackup object(s) still carried a finalizer after %s with a Ready "+
			"controller-manager. The operator was there and did not release them, which is what "+
			"strands a namespace in Terminating for good:\n%s",
		len(remaining), budget, report)
}
