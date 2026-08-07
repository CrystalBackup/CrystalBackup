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

package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// ---------------------------------------------------------------------------------------------
// The operator namespace's Pod Security posture, checked from inside the cluster because that is
// the only place every install path passes through.
//
// Data movers run `runAsUser: 0` with DAC_OVERRIDE — they have to, to preserve file ownership on
// restore — so `crystal-backup-system` must enforce `baseline`, not `restricted`. The chart says
// so, and as of 0.6.3 it also REFUSES to install against a namespace whose posture disagrees.
//
// But that refusal is a Helm template guard, and it has two blind spots that are not edge cases:
//
//   - `helm template` (so: Argo CD, Flux, anything GitOps) evaluates `lookup` against nothing and
//     gets nothing. The guard passes vacuously.
//   - `helm install --create-namespace` makes the namespace AFTER rendering. At render time it
//     does not exist, so again the guard sees nothing — and Helm's own namespace carries no
//     PodSecurity labels at all.
//
// Both paths therefore produce an unlabelled namespace and a silent guard. The failure surfaces
// later, somewhere else, as a mover pod refused by admission — during the first backup, or worse,
// during a restore. operations/dr-runbook.md still documents a reinstall with
// `--create-namespace`, which is the second-worst moment to discover it.
//
// So the operator checks its own namespace once, on startup, and says so loudly. It does NOT
// refuse to run: an operator that exits on an upgrade of a cluster that has been running happily
// without the labels would turn a latent problem into an outage, and the labels are only needed
// when a mover is created. A running operator that says exactly what is wrong, in the log and as
// an Event on its own namespace, is the right trade.
// ---------------------------------------------------------------------------------------------

// psaEnforceLabel is the Pod Security Admission key that decides whether a mover can be created.
// audit/warn are advisory and deliberately not checked: they change what is reported, never what
// is admitted, and failing an install over them would be noise.
const psaEnforceLabel = "pod-security.kubernetes.io/enforce"

// psaRequiredEnforce is the level the movers need. Not a minimum and not a maximum — `restricted`
// denies them and `privileged` is wider than anything here asks for.
const psaRequiredEnforce = "baseline"

// NamespacePostureCheck is a one-shot startup Runnable. Registered with mgr.Add so it runs after
// the manager has started, which is what makes the Event recorder live and the read cheap.
type NamespacePostureCheck struct {
	// Reader must be UNCACHED (mgr.GetAPIReader). The manager's cache is scoped to the operator
	// namespace for the kinds the controllers watch, and Namespace is not one of them; going
	// through the cached client would either miss or start an informer over every namespace in
	// the cluster to answer one question, once.
	Reader client.Reader
	// Namespace is the operator's own namespace.
	Namespace string
	// Recorder emits the finding where `kubectl get events` will show it. Optional: a nil
	// recorder logs and nothing more, which is what the unit tests use.
	Recorder record.EventRecorder
}

// NeedLeaderElection is false: this is a read and a log line, it costs nothing, and a standby
// replica that has NOT noticed a broken posture is a standby that will fail the same way the
// moment it is elected. Every replica should say it.
func (c *NamespacePostureCheck) NeedLeaderElection() bool { return false }

// Start performs the check once and returns. Returning nil on every path is deliberate — see the
// header: this must not be able to take the operator down.
func (c *NamespacePostureCheck) Start(ctx context.Context) error {
	log := logf.FromContext(ctx).WithName("namespace-posture")

	var ns corev1.Namespace
	if err := c.Reader.Get(ctx, client.ObjectKey{Name: c.Namespace}, &ns); err != nil {
		// NOT a finding about the posture. An unreadable namespace says nothing about its labels,
		// and reporting "posture wrong" here would be the absence-reads-as-failure mistake — the
		// mirror of the one this project keeps hunting.
		log.Info("could not read the operator namespace to check its Pod Security posture; "+
			"nothing is concluded about it", "namespace", c.Namespace, "error", err.Error())
		return nil
	}

	got := ns.Labels[psaEnforceLabel]
	if got == psaRequiredEnforce {
		return nil
	}

	detail := PostureProblem(c.Namespace, got)
	log.Error(nil, detail)
	if c.Recorder != nil {
		c.Recorder.Event(&ns, corev1.EventTypeWarning, "PodSecurityPostureWrong", detail)
	}
	return nil
}

// PostureProblem is the sentence, built once so the log line, the Event and the tests cannot word
// it differently. It always ends with the command that fixes it: a diagnosis a reader has to
// translate into an action is half a diagnosis, and this one is reached by people who did not
// choose to be here.
func PostureProblem(namespace, got string) string {
	had := fmt.Sprintf("%q", got)
	if got == "" {
		had = "no " + psaEnforceLabel + " label at all"
	}
	return fmt.Sprintf(
		"the operator namespace %s has %s, but data movers need %s=%s: they run as uid 0 with "+
			"DAC_OVERRIDE to preserve file ownership on restore, which a stricter level denies. "+
			"Backups will fail at admission when the first mover is created, not now. Fix it with:"+
			"\n  kubectl label namespace %s %s=%s --overwrite",
		namespace, had, psaEnforceLabel, psaRequiredEnforce,
		namespace, psaEnforceLabel, psaRequiredEnforce)
}
