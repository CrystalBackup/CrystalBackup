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
	"fmt"
	"slices"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
	"github.com/CrystalBackup/CrystalBackup/internal/mover"
)

// ---------------------------------------------------------------------------------------------
// M6 — mover placement reaches the pod, on a real cluster, through the real path.
//
// WHAT IS ALREADY PROVED ELSEWHERE, so that this spec can be about the part that is not.
// internal/mover/placement_test.go proves BuildJob puts a placement on the pod spec and drops the
// selector on a node-pinned Job; test/chart/placement_test.go proves the chart renders a document
// the operator's own loader accepts; internal/controller/mover_wiring_test.go proves every one of
// the ten JobRequest sites threads the field. Every one of those can be green while the operator
// on a cluster reads no placement at all — a ConfigMap key that never got mounted, a flag pointing
// at the wrong path, a value the chart wrote and the operator never re-read. That gap is exactly
// how JobRequest.GoMemLimit stayed unassigned from M1 to 0.6.1 with the suite green throughout.
//
// So this spec goes the whole way round: it writes a placement into the ConfigMap the operator
// mounts, restarts the operator so the flag is re-read, runs a real backup, and reads the answer
// off the Jobs and the PODS the cluster created.
//
// WHY IT PATCHES THE ConfigMap RATHER THAN RUNNING `helm upgrade`. Two reasons. The suite talks to
// the API and nothing else, so a helm binary would be a new dependency for one spec. And the
// ConfigMap IS the contract: the chart's only job is to write that document, and test/chart
// already proves it writes it correctly. Patching it exercises precisely the half the chart test
// cannot reach — mount, flag, parse, apply.
//
// WHAT IT DELIBERATELY DOES NOT COVER. The node-pinned restore exception (a same-node restore
// keeps the tolerations and drops the selector) would need a placement that excludes the node an
// RWO volume is already attached to — an inject-and-hope setup whose failure mode is a stuck
// restore rather than a clear red. It is unit-tested against BuildJob, which is where the rule
// lives, and stated in the values.yaml comment a reader would meet first.
// ---------------------------------------------------------------------------------------------

const (
	// m6PlacementLabel is put on exactly ONE node. One, not all, because a selector every node
	// satisfies proves the field was written and nothing about whether it was honoured — and this
	// spec's whole subject is a cluster where only some nodes can do the work.
	m6PlacementLabel = "crystalbackup.io/crucible-mover"
	m6PlacementValue = "yes"

	// m6PlacementNS is a seeded namespace with real volumes, so the run produces data movers and
	// not only the manifest capture.
	m6PlacementNS = "c-db"

	// m6PlacementCMSuffix identifies the placement ConfigMap without hardcoding the release
	// prefix, the same discipline m6FindRBDNodePlugin applies to the node plugin's name.
	m6PlacementCMSuffix = "-mover-placement"
	m6PlacementCMKey    = "placement.yaml"
)

var _ = Describe("Milestone M6 — mover placement", Ordered, Label("m6", "placement"), func() {
	// Everything this spec changes about the cluster, captured before it changes it. `restore`
	// guards the AfterAll: a BeforeAll that failed halfway must not write its zero values back as
	// if they were facts — the trap m6_precheck_test.go names, where an unguarded restore would
	// have scaled the storage operator to zero as a cleanup step.
	var (
		configMapName string
		priorDocument string
		targetNode    string
		restore       bool
	)

	BeforeAll(func() {
		// Ensure-then-wait, like every other lane: this one used to only WAIT, which made it
		// depend on some other container having drawn an earlier position in Ginkgo's shuffle —
		// the same latent flake that took m6/s3tuning down in 0.6.7.
		m1EnsureSharedRepository()

		By("Given the placement ConfigMap the operator mounts")
		configMapName = m6FindPlacementConfigMap()
		priorDocument = m6ReadPlacementDocument(configMapName)

		By("And one schedulable worker node, chosen deterministically")
		targetNode = m6PickPlacementNode()
		restore = true
	})

	AfterAll(func() {
		if !restore {
			return
		}
		// Order is load-bearing and is the reverse of the setup. The operator must stop asking for
		// the label BEFORE the label goes away: unlabel first and every mover for the rest of the
		// campaign is unschedulable, with a Pending pod and no clue as to why.
		m6WritePlacementDocument(configMapName, priorDocument)
		m1DeleteOperatorPod()
		m6SetNodeLabel(targetNode, m6PlacementLabel, "")
	})

	It("puts every mover pod on the node the platform chose, and the backup still completes", func() {
		By("Given one node carries the label and the platform's placement requires it")
		m6SetNodeLabel(targetNode, m6PlacementLabel, m6PlacementValue)
		m6WritePlacementDocument(configMapName, fmt.Sprintf("nodeSelector:\n  %q: %q\n",
			m6PlacementLabel, m6PlacementValue))

		By("And the operator has been restarted, because the placement is read once at startup")
		m1DeleteOperatorPod()

		run := crucibleRunName("m6-placement")
		By("When a backup runs over a namespace with real volumes")
		m1RunClusterBackup(run, m1LocationName, cbv1.NamespaceSelector{MatchNames: []string{m6PlacementNS}})

		// The Jobs first. This is the assertion that cannot be a coincidence: with three workers a
		// single mover lands on the chosen node one time in three by luck, but a nodeSelector does
		// not appear in a pod template by luck.
		By("Then every mover Job carries the platform's nodeSelector in its pod template")
		var jobNames []string
		Eventually(func(g Gomega) {
			jobs := m6MoverJobsForRun(g, run)
			g.Expect(jobs).NotTo(BeEmpty(), "no mover Job for run %s yet", run)
			jobNames = make([]string, 0, len(jobs))
			for i := range jobs {
				sel := jobs[i].Spec.Template.Spec.NodeSelector
				g.Expect(sel).To(HaveKeyWithValue(m6PlacementLabel, m6PlacementValue),
					"mover Job %s has nodeSelector %v. The placement reached the ConfigMap and "+
						"stopped somewhere between there and the Job — a mount, a flag, or a "+
						"startup that never re-read the file.", jobs[i].Name, sel)
				jobNames = append(jobNames, jobs[i].Name)
			}
		}, 10*time.Minute, 10*time.Second).Should(Succeed())

		// And then the pods, which prove the selector was SATISFIABLE. A placement that no node
		// matches produces exactly the same Job as a correct one and a backup that never moves;
		// asserting only the template would pass on a cluster where backups had stopped.
		By("And every mover pod actually ran on that node")
		Eventually(func(g Gomega) {
			placed := 0
			for _, job := range jobNames {
				for _, pod := range m6PodsOfJob(g, job) {
					if pod.Spec.NodeName == "" {
						g.Expect(pod.Spec.NodeName).NotTo(BeEmpty(),
							"mover pod %s is still unscheduled; if it stays that way the selector "+
								"matches no node and every backup in this cluster has stopped", pod.Name)
					}
					g.Expect(pod.Spec.NodeName).To(Equal(targetNode),
						"mover pod %s ran on %q, not on the node the platform chose (%q)",
						pod.Name, pod.Spec.NodeName, targetNode)
					placed++
				}
			}
			g.Expect(placed).To(BeNumerically(">=", 1),
				"none of the %d mover Jobs has a pod yet", len(jobNames))
		}, 10*time.Minute, 10*time.Second).Should(Succeed())

		// The property the two assertions above cannot state between them: that constraining the
		// movers did not simply break the product. This is the one an administrator cares about.
		By("And the run completes — a placement that is honoured is not a placement that blocks")
		cb := m1WaitClusterBackupTerminal(run, 20*time.Minute)
		Expect(cb.Status.Phase).To(Equal("Completed"),
			"the run ended %q. Constraining where movers may run must not change whether they "+
				"succeed; if it does, this field is a foot-gun rather than a knob.", cb.Status.Phase)
	})
})

// --- helpers ---------------------------------------------------------------------------------

// m6FindPlacementConfigMap resolves the placement ConfigMap by suffix rather than by full name,
// because the chart release-prefixes its objects and a spec that hardcodes one release's prefix is
// a spec that fails on the next cluster for a reason having nothing to do with the product.
func m6FindPlacementConfigMap() string {
	GinkgoHelper()
	var list corev1.ConfigMapList
	Expect(k8s.List(ctx, &list, client.InNamespace(operatorNS))).To(Succeed())

	var names []string
	for i := range list.Items {
		if strings.HasSuffix(list.Items[i].Name, m6PlacementCMSuffix) {
			names = append(names, list.Items[i].Name)
		}
	}
	Expect(names).NotTo(BeEmpty(),
		"no ConfigMap in %s ends in %q. The operator's --mover-placement-file points into a mount "+
			"of that ConfigMap, so its absence means the chart did not render it and the operator "+
			"is running with no placement at all — which this spec would otherwise report as a "+
			"placement that was ignored.", operatorNS, m6PlacementCMSuffix)
	slices.Sort(names)
	return names[0]
}

func m6ReadPlacementDocument(name string) string {
	GinkgoHelper()
	var cm corev1.ConfigMap
	Expect(k8s.Get(ctx, client.ObjectKey{Namespace: operatorNS, Name: name}, &cm)).To(Succeed())
	return cm.Data[m6PlacementCMKey]
}

// m6WritePlacementDocument replaces the placement document. An empty string writes an empty
// document rather than deleting the key: the operator tolerates a missing file, but the deployment
// mounts this key by name and a subPath-style mount of an absent key is a pod that will not start.
func m6WritePlacementDocument(name, document string) {
	GinkgoHelper()
	var cm corev1.ConfigMap
	Expect(k8s.Get(ctx, client.ObjectKey{Namespace: operatorNS, Name: name}, &cm)).To(Succeed())
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	cm.Data[m6PlacementCMKey] = document
	Expect(k8s.Update(ctx, &cm)).To(Succeed(), "write %s of ConfigMap %s/%s", m6PlacementCMKey, operatorNS, name)
}

// m6PickPlacementNode returns one schedulable worker, chosen by sorted name so two runs of this
// spec against the same cluster choose the same node and a failure is reproducible.
//
// Control-plane nodes are excluded because a mover cannot be assumed to tolerate their taints, and
// this spec must not be the thing that discovers that: its subject is placement, and a pod Pending
// on a taint would look exactly like a placement that was not honoured.
func m6PickPlacementNode() string {
	GinkgoHelper()
	var nodes corev1.NodeList
	Expect(k8s.List(ctx, &nodes)).To(Succeed())

	var candidates []string
	for i := range nodes.Items {
		n := &nodes.Items[i]
		if n.Spec.Unschedulable || len(n.Spec.Taints) > 0 {
			continue
		}
		if _, isControlPlane := n.Labels["node-role.kubernetes.io/control-plane"]; isControlPlane {
			continue
		}
		ready := false
		for _, c := range n.Status.Conditions {
			if c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue {
				ready = true
			}
		}
		if ready {
			candidates = append(candidates, n.Name)
		}
	}
	Expect(len(candidates)).To(BeNumerically(">=", 2),
		"this spec needs at least two schedulable workers to mean anything: with one, a mover "+
			"lands on the chosen node whether the placement was honoured or not. Found %v", candidates)
	slices.Sort(candidates)
	return candidates[0]
}

// m6SetNodeLabel sets a node label, or removes it when value is empty.
func m6SetNodeLabel(node, key, value string) {
	GinkgoHelper()
	var n corev1.Node
	Expect(k8s.Get(ctx, client.ObjectKey{Name: node}, &n)).To(Succeed())
	if n.Labels == nil {
		n.Labels = map[string]string{}
	}
	if value == "" {
		delete(n.Labels, key)
	} else {
		n.Labels[key] = value
	}
	Expect(k8s.Update(ctx, &n)).To(Succeed(), "set label %s=%q on node %s", key, value, node)
}

// m6MoverJobsForRun returns this run's mover Jobs — every operation, not only the data movers,
// because "every mover" is precisely the claim values.yaml makes about this field.
func m6MoverJobsForRun(g Gomega, run string) []batchv1.Job {
	var jobs batchv1.JobList
	g.Expect(k8s.List(ctx, &jobs,
		client.InNamespace(operatorNS),
		client.MatchingLabels{mover.LabelAppName: mover.AppName},
	)).To(Succeed())

	var out []batchv1.Job
	for i := range jobs.Items {
		if jobs.Items[i].Labels[apiconst.LabelClusterBackup] == run {
			out = append(out, jobs.Items[i])
		}
	}
	return out
}

func m6PodsOfJob(g Gomega, jobName string) []corev1.Pod {
	var pods corev1.PodList
	g.Expect(k8s.List(ctx, &pods, client.InNamespace(operatorNS),
		client.MatchingLabels{batchv1.JobNameLabel: jobName})).To(Succeed())
	return pods.Items
}
