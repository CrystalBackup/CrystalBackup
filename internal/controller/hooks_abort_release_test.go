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
	"errors"
	"testing"

	. "github.com/onsi/ginkgo/v2" //nolint:revive,staticcheck
	. "github.com/onsi/gomega"    //nolint:revive,staticcheck

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/hooks"
	"github.com/CrystalBackup/CrystalBackup/internal/mover"
	"github.com/CrystalBackup/CrystalBackup/internal/status"
)

// THE TWO DEFECTS 0.6.5 NAMED AND DID NOT FIX (0.6.6 lot 2). Both live in the hook chain, both leave
// a production application frozen, and both are loud about the wrong thing or silent altogether.
//
//	D. AN ABORTED PRE PHASE NEVER RELEASED WHAT IT HAD ALREADY FROZEN. hooks.Run stops a pre chain at
//	   the first onError=Fail failure — correctly — but the hooks before it succeeded and their
//	   applications are quiesced. failHooks wrote a terminal Failed, and the already-terminal
//	   short-circuit at the top of Reconcile meant closeFreezeWindow never ran again. Nothing thawed
//	   them and nothing said so, on the one path where a human is least likely to look: a Backup
//	   reporting Failed over a quiesce reads as a run that never started.
//	E. A Fail-POLICY POST-HOOK FAILURE BLOCKED THE RELEASE OF EVERY LATER POD. The rest of the chain
//	   was marked Skipped and each retry restarted it from the beginning, so a permanently broken first
//	   hook meant the later pods were never thawed across all three attempts. UnfreezeFailed did fire —
//	   about the one pod whose command failed, and not about the others, which were the ones a human
//	   could have unfrozen by hand.
//
// Every mutation named below was actually applied, in a throwaway `git worktree`, and the spec
// confirmed RED before the mutation was reverted. Where a mutation did NOT kill the spec it was
// guessed for, the comment says so and names the level that does kill it — or says that nothing does
// — rather than claiming coverage that is not there. Exactly one mutation survived everywhere, and it
// survived because it is behaviour-preserving; that is recorded on the guard itself in hooks_phase.go
// rather than papered over with a spec that would not have been testing it.

var _ = Describe("Consistency hooks: an aborted pre phase releases what it froze (defect D)", func() {
	BeforeEach(func() { hookExecutor.reset() })

	// THE DEFECT, in one spec, and the assertion that matters is the CONJUNCTION: the run still ends
	// Failed for the quiesce it could not perform, AND the application it did quiesce is thawed, AND
	// the thaw is in the durable record rather than only in a log line.
	//
	// Two pods, one pre hook each, selected by label so the commands differ per workload. Pod order is
	// made deterministic by the suite's client wrapper (see faultInjectingClient.List) — without that,
	// the aborting pod lands first half the time, nothing is ever successfully quiesced, and this spec
	// passes for the wrong reason every other run.
	//
	// Mutations that turned this spec red (each was applied and observed):
	//   - openFreezeWindow's abort branch calling failHooks directly again (the 0.6.5 behaviour) → the
	//     thaw assertion fails with "nothing thawed db-0 and nothing said so", which is the defect
	//     verbatim. It kills the next spec at the same time.
	//   - quiescedPodsFromStatus counting every pre entry, Failed and Skipped included → db-1 is thawed
	//     too and the "ONLY that pod" assertion fails on a post entry for db-1.
	//   - restrictToPods ignoring owedTo → identical.
	//   - failHooks not called when nothing is owed → the run never reaches Failed.
	//
	// AND ONE THAT DID NOT, which is worth more than the ones that did: removing the spec-derived guard
	// from releaseAbortedQuiesce (`len(spec.Post) == 0 && !spec.HonorAnnotations`) kills nothing, here
	// or at the unit level. It is redundant with hookPhaseDeclared, which already resolves a
	// post-undeclared phase to nil without listing pods. The guard is kept anyway and the reason is
	// argued where it lives; what is NOT done is claiming a spec covers it.
	It("thaws the pod it quiesced, and still ends Failed for the pre-hook abort", func() {
		const (
			location = "hk-abrel"
			ns       = "hk-abrel-ns"
			run      = "hk-abrel-run"
			pvcName  = "data-vol"
		)
		seedInitializedRepo(location, "kek-hk-abrel", "s3-hk-abrel")
		createTenantNamespace(ns)
		createSourcePVC(ns, pvcName, "ceph-block")
		createLabelledMountingPod(ns, "db-0", pvcName, map[string]string{"role": "first"}, nil)
		createLabelledMountingPod(ns, "db-1", pvcName, map[string]string{"role": "second"}, nil)

		// The SECOND pod's quiesce is the one that fails. Keyed on the command rather than the pod so
		// the release ("thaw") stays healthy in the same pod the broken quiesce lives in — failPod
		// cannot express that, which is exactly why failCommand exists.
		hookExecutor.failCommand = map[string]error{
			"freeze-second": errors.New("command terminated with exit code 1"),
		}

		createHookedParent(run, location, cbv1.PVCSelector{}, cbv1.HooksSpec{
			Pre: []cbv1.Hook{
				{
					PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"role": "first"}},
					Command:     []string{"freeze-first"},
				},
				{
					PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"role": "second"}},
					Command:     []string{"freeze-second"},
					// onError left unset: the CRD defaults it to Fail, which is what aborts the chain.
				},
			},
			// One release, matching every pod. Which of them it is OWED to is the question under test.
			Post: []cbv1.Hook{{Command: []string{"thaw"}}},
		})
		createChildBackup(ns, run, location)

		By("the chain quiesces db-0, aborts on db-1, and records both")
		Eventually(func(g Gomega) {
			b := getBackupG(g, ns, run)
			g.Expect(hookEntryForPod(b, hooks.PhasePre, "db-0")).NotTo(BeNil())
			g.Expect(hookEntryForPod(b, hooks.PhasePre, "db-0").Result).To(Equal(cbv1.HookSucceeded))
			g.Expect(hookEntryForPod(b, hooks.PhasePre, "db-1")).NotTo(BeNil())
			g.Expect(hookEntryForPod(b, hooks.PhasePre, "db-1").Result).To(Equal(cbv1.HookFailed))
		}, initTimeout, initPoll).Should(Succeed())

		By("the run ends Failed, for the PRE-HOOK abort and nothing else")
		Eventually(func(g Gomega) {
			b := getBackupG(g, ns, run)
			g.Expect(b.Status.Phase).To(Equal("Failed"),
				"phase=%q — a thaw that worked does not make this a success, a partial success, or anything "+
					"other than a run whose quiesce failed: there is no restore point here to qualify",
				b.Status.Phase)
			cond := hookConditionG(g, ns, run, ConditionReady)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Reason).To(Equal("PreHookFailed"),
				"the release the abort triggered must not displace the reason the run failed")
			g.Expect(cond.Message).To(ContainSubstring("db-1"))
			g.Expect(b.Status.CompletionTime).NotTo(BeNil())
			// R16's other half is untouched: no volume was snapshotted behind a failed quiesce.
			for _, v := range b.Status.Volumes {
				g.Expect(string(v.Phase)).To(Or(Equal("Pending"), Equal("")))
			}
		}, initTimeout, initPoll).Should(Succeed())

		By("and the application it DID freeze was released — durably, in status, on the terminal object")
		Eventually(func(g Gomega) {
			b := getBackupG(g, ns, run)
			thawed := hookEntryForPod(b, hooks.PhasePost, "db-0")
			g.Expect(thawed).NotTo(BeNil(),
				"nothing thawed db-0 and nothing said so: its quiesce succeeded and is still in effect, "+
					"which is R16's priority — the release matters more than the backup — inverted")
			g.Expect(thawed.Result).To(Equal(cbv1.HookSucceeded))
			g.Expect(b.Status.PostHookAttempts).To(Equal(int32(1)))
		}, initTimeout, initPoll).Should(Succeed())

		By("and ONLY that pod: db-1's quiesce failed, so nothing froze it and nothing may thaw it")
		Consistently(func(g Gomega) {
			b := getBackupG(g, ns, run)
			g.Expect(hookEntryForPod(b, hooks.PhasePost, "db-1")).To(BeNil(),
				"a thaw was issued against a pod whose quiesce failed — a pg_backup_stop() over a database "+
					"never told to start a backup, chosen by the operator on the application owner's behalf")
			g.Expect(hookConditionG(g, ns, run, ConditionApplicationConsistent)).To(BeNil(),
				"no snapshot was taken, so there is no restore point whose consistency to describe")
		}, consistentlyWindow, initPoll).Should(Succeed())

		By("and the thaw really reached the pod, rather than only being recorded")
		//
		// The COUNT is asserted as >= 1 rather than == 1 deliberately. "Exactly one attempt" is a claim
		// about the durable record and is made above, on PostHookAttempts; the exec log cannot carry it,
		// because a status-write conflict on the terminating pass legitimately re-drives the whole abort
		// path and re-runs an idempotent thaw. Pinning the exec count would make this spec fail on a
		// retry the design explicitly allows. What the exec log CAN prove, and what matters, is WHICH pod
		// the operator sent a command to.
		var thawCalls int
		for _, c := range hookExecutor.recorded() {
			if len(c.Command) > 0 && c.Command[0] == "thaw" {
				thawCalls++
				Expect(c.Pod.Name).To(Equal("db-0"),
					"a thaw was exec'd into %s, whose quiesce never succeeded", c.Pod.Name)
				Expect(c.Pod.Namespace).To(Equal(ns))
			}
		}
		Expect(thawCalls).To(BeNumerically(">=", 1), "the release was recorded but never exec'd")
	})

	// THE THAW ITSELF FAILING, which is the case that decides the ORDERING of the whole fix.
	//
	// Running the thaw and letting failHooks write terminal in the same pass unconditionally is the
	// simpler shape and it breaks here: the already-terminal short-circuit would bar every retry, so the
	// product's three attempts at unfreezing an application become one — the exact bug lot 10 closed for
	// the normal release, reintroduced on the path where the application is MORE likely to be stuck. So
	// the terminal write is held while a release is owed, and this spec proves both halves: the retries
	// happen, and the hold cannot outlive the attempt budget.
	//
	// HOW THE HOLD IS PROVED, since "the phase was SnapshottingHooks for 40ms" is not observable:
	// arithmetic. Reaching postHookMaxAttempts needs at least that many passes after the abort was
	// decided, and without the hold there is exactly one — the pass that writes Failed.
	//
	// Mutations that turned this spec red, all three on the attempt-count assertion below:
	//   - failHooks called unconditionally in abortFreezeWindow (the 0.6.5 behaviour).
	//   - releaseAbortedQuiesce returning owed=false after a failed attempt → observed
	//     `postHookAttempts=1, phase="Failed"`, which is the one-of-three-attempts bug exactly.
	//   - the hold path's status write dropped → PostHookAttempts never becomes durable, so the budget
	//     never advances, the thaw is re-run forever and the run never terminates. It fails on the
	//     attempt count rather than on the terminal assertion, because a counter read back from etcd
	//     that never moves is indistinguishable from a release that was never retried.
	It("bounds a thaw that keeps failing, ends at UnfreezeFailed, and then reports Failed", func() {
		const (
			location = "hk-abstuck"
			ns       = "hk-abstuck-ns"
			run      = "hk-abstuck-run"
			pvcName  = "data-vol"
		)
		seedInitializedRepo(location, "kek-hk-abstuck", "s3-hk-abstuck")
		createTenantNamespace(ns)
		createSourcePVC(ns, pvcName, "ceph-block")
		createLabelledMountingPod(ns, "db-0", pvcName, map[string]string{"role": "first"}, nil)
		createLabelledMountingPod(ns, "db-1", pvcName, map[string]string{"role": "second"}, nil)

		hookExecutor.failCommand = map[string]error{
			"freeze-second": errors.New("command terminated with exit code 1"),
			// And the thaw owed to db-0 is broken too: the worst realistic case, and the only one where
			// a human genuinely has to go and unfreeze something by hand.
			"thaw": errors.New("command terminated with exit code 1"),
		}

		createHookedParent(run, location, cbv1.PVCSelector{}, cbv1.HooksSpec{
			Pre: []cbv1.Hook{
				{
					PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"role": "first"}},
					Command:     []string{"freeze-first"},
				},
				{
					PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"role": "second"}},
					Command:     []string{"freeze-second"},
				},
			},
			Post: []cbv1.Hook{{Command: []string{"thaw"}}},
		})
		createChildBackup(ns, run, location)

		By("the release is attempted its FULL budget of times, which needs the terminal phase held")
		Eventually(func(g Gomega) {
			b := getBackupG(g, ns, run)
			g.Expect(b.Status.PostHookAttempts).To(BeNumerically(">=", int32(postHookMaxAttempts)),
				"postHookAttempts=%d, phase=%q — reaching %d needs at least that many passes after the abort "+
					"was decided, and without the hold there is exactly one: the run reports Failed over an "+
					"application it froze and never released",
				b.Status.PostHookAttempts, b.Status.Phase, postHookMaxAttempts)
		}, initTimeout, initPoll).Should(Succeed())

		By("and it ends at the EXISTING loud Event, not at a new one")
		Eventually(func(g Gomega) {
			g.Expect(backupWarningNotes(g, ns, run)).To(ContainElement(
				ContainSubstring("may remain quiesced and needs manual attention")),
				"the backup is beyond saving and an application may not be: that is the one thing only a "+
					"human can fix, and it is the same sentence whether the run failed on its quiesce or not")
		}, initTimeout, initPoll).Should(Succeed())

		By("and the hold is BOUNDED: the run then reaches Failed for the pre-hook reason")
		Eventually(func(g Gomega) {
			b := getBackupG(g, ns, run)
			g.Expect(b.Status.Phase).To(Equal("Failed"),
				"phase=%q — a hold that outlives the attempt budget is a run that never terminates: no "+
					"backupTime, no completionTime, and a Forbid schedule that skips every night after. That "+
					"is a worse bug than the one being fixed.", b.Status.Phase)
			g.Expect(b.Status.CompletionTime).NotTo(BeNil())
			cond := hookConditionG(g, ns, run, ConditionReady)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Reason).To(Equal("PreHookFailed"))
			// The failed thaw keeps its own record beside the verdict, so a reader can see BOTH facts.
			thawed := hookEntryForPod(b, hooks.PhasePost, "db-0")
			g.Expect(thawed).NotTo(BeNil())
			g.Expect(thawed.Result).To(Equal(cbv1.HookFailed))
		}, initTimeout, initPoll).Should(Succeed())
	})
})

var _ = Describe("Consistency hooks: one broken thaw no longer strands the other pods (defect E)", func() {
	BeforeEach(func() { hookExecutor.reset() })

	// THE DEFECT. Three pods, three thaws owed to three different applications, and the FIRST one is
	// permanently broken. hooks.Run marked the other two Skipped, every retry restarted the chain and
	// died in the same place, and the two healthy applications stayed frozen across all three attempts
	// with nothing naming them. UnfreezeFailed fired, so it was loud — about the wrong pod.
	//
	// Stopping the chain is right for the PRE phase, where the hooks are one collective act producing
	// one trustworthy point in time, and wrong for the POST phase, where every entry is a thaw owed to
	// somebody else. That is a per-phase rule, and it lives in hooks.chainStopsOnAbort.
	//
	// Mutation that turned this spec red: chainStopsOnAbort returning true unconditionally (the 0.6.5
	// behaviour) → observed `db-1 release result = "Skipped"`, the frozen application with a status
	// entry saying its thaw never ran.
	//
	// chainStopsOnAbort returning FALSE unconditionally leaves this spec GREEN, and that was confirmed
	// by running it rather than assumed: no pre phase is exercised here, so the loosened rule changes
	// nothing. The other direction is pinned where it belongs, in internal/hooks'
	// TestRunStopsThePreChainOnly, which that mutation kills immediately.
	It("runs the whole release chain, so a broken first hook costs its own pod only", func() {
		const (
			location = "hk-postfan"
			ns       = "hk-postfan-ns"
			run      = "hk-postfan-run"
			pvcName  = "data-vol"
		)
		seedInitializedRepo(location, "kek-hk-postfan", "s3-hk-postfan")
		createTenantNamespace(ns)
		createSourcePVC(ns, pvcName, "ceph-block")
		// Three applications sharing the backed-up volume, so all three are hook candidates and all
		// three are owed a release. Names sort in the order the chain runs them.
		createMountingPod(ns, "db-0", pvcName, nil)
		createMountingPod(ns, "db-1", pvcName, nil)
		createMountingPod(ns, "db-2", pvcName, nil)

		// The FIRST pod's hooks are broken, which under the old chain rule was enough to strand the
		// other two. failPod (not failCommand) because the pod is what is broken here.
		hookExecutor.failPod = map[string]error{"db-0": errors.New("command terminated with exit code 1")}

		createHookedParent(run, location, cbv1.PVCSelector{}, cbv1.HooksSpec{
			// No pre phase: this spec is about the release chain alone, and a failing quiesce here would
			// abort the run and test defect D instead.
			Post: []cbv1.Hook{{Command: []string{"thaw"}}},
		})
		createChildBackup(ns, run, location)

		jobName := waitForMoverJob(ns, run, pvcName)
		simulateMoverSucceeded(jobName, "node-a", mover.MoverResult{
			OK: true, Operation: string(mover.OpBackup), SnapshotID: "snap-postfan", SizeBytes: 8, AddedBytes: 2,
		})

		By("the two healthy applications ARE thawed, despite the broken hook ahead of them")
		Eventually(func(g Gomega) {
			b := getBackupG(g, ns, run)
			for _, pod := range []string{"db-1", "db-2"} {
				e := hookEntryForPod(b, hooks.PhasePost, pod)
				g.Expect(e).NotTo(BeNil(), "no release entry for %s at all", pod)
				g.Expect(e.Result).To(Equal(cbv1.HookSucceeded),
					"%s release result = %q — a thaw owed to this application was abandoned because a "+
						"DIFFERENT application's hook was broken, and the loud Event named the other one",
					pod, e.Result)
			}
			broken := hookEntryForPod(b, hooks.PhasePost, "db-0")
			g.Expect(broken).NotTo(BeNil())
			g.Expect(broken.Result).To(Equal(cbv1.HookFailed))
		}, initTimeout, initPoll).Should(Succeed())

		By("and the commands really reached them, rather than only being recorded")
		Eventually(func(g Gomega) {
			reached := map[string]bool{}
			for _, c := range hookExecutor.recorded() {
				if c.Pod.Namespace == ns && len(c.Command) > 0 && c.Command[0] == "thaw" {
					reached[c.Pod.Name] = true
				}
			}
			g.Expect(reached).To(HaveKey("db-1"))
			g.Expect(reached).To(HaveKey("db-2"))
		}, initTimeout, initPoll).Should(Succeed())

		By("the broken one is still loud once its budget is gone, and the run still terminates")
		Eventually(func(g Gomega) {
			b := getBackupG(g, ns, run)
			g.Expect(b.Status.PostHookAttempts).To(BeNumerically(">=", int32(postHookMaxAttempts)))
			g.Expect(isTerminalBackupPhase(b.Status.Phase)).To(BeTrue(), "phase=%q", b.Status.Phase)
			g.Expect(b.Status.CompletionTime).NotTo(BeNil())
		}, initTimeout, initPoll).Should(Succeed())
		Eventually(func(g Gomega) {
			g.Expect(backupWarningNotes(g, ns, run)).To(ContainElement(
				ContainSubstring("may remain quiesced and needs manual attention")))
		}, initTimeout, initPoll).Should(Succeed())
	})
})

// hookEntryForPod returns the recorded entry for one phase AND one pod, or nil. The specs above turn
// on WHICH pods appear in the release record, which hookEntry (first entry of a phase) cannot express.
func hookEntryForPod(b cbv1.Backup, phase hooks.Phase, pod string) *cbv1.HookStatus {
	for i := range b.Status.Hooks {
		if b.Status.Hooks[i].Phase == string(phase) && b.Status.Hooks[i].Pod == pod {
			return &b.Status.Hooks[i]
		}
	}
	return nil
}

// TestQuiescedPodsFromStatus is the unit-level statement of "what exactly is owed after an abort",
// and every one of its exclusions is a command this operator would otherwise issue against an
// application it never touched.
//
// It reads STATUS and nothing else, for the same reason hookState does: the abort is decided on a
// later pass than the execution, so a controller that died in between has no other way to come back
// knowing which applications it froze.
//
// Five mutations were applied and every one turned it red: admitting Failed entries; admitting
// Skipped entries; ignoring the phase, so a recorded release counts as a quiesce; keying the set on
// the CONTAINER instead of the pod; and returning nil instead of an empty set for a Backup with no
// pre record — which downstream means "no restriction", i.e. thaw every pod in the namespace.
func TestQuiescedPodsFromStatus(t *testing.T) {
	backup := &cbv1.Backup{Status: cbv1.BackupStatus{Hooks: []cbv1.HookStatus{
		// Quiesced, and still in effect: owed a thaw.
		{Phase: string(hooks.PhasePre), Pod: "db-0", Result: cbv1.HookSucceeded},
		// The aborting hook. Its quiesce did not happen, so nothing froze this pod.
		{Phase: string(hooks.PhasePre), Pod: "db-1", Result: cbv1.HookFailed,
			OnError: cbv1.HookErrorPolicyFail, Message: "exit status 1"},
		// Behind the abort: never ran at all. This is the exclusion the roadmap entry names.
		{Phase: string(hooks.PhasePre), Pod: "db-2", Result: cbv1.HookSkipped,
			Message: hooks.ErrSkipped.Error()},
		// A pod with TWO pre hooks, the first of which succeeded: something IS in effect on it.
		{Phase: string(hooks.PhasePre), Pod: "db-3", Result: cbv1.HookSucceeded},
		{Phase: string(hooks.PhasePre), Pod: "db-3", Result: cbv1.HookFailed, OnError: cbv1.HookErrorPolicyFail},
		// A release already recorded for somebody: not a quiesce, and must not be read as one.
		{Phase: string(hooks.PhasePost), Pod: "db-9", Result: cbv1.HookSucceeded},
	}}}

	got := quiescedPodsFromStatus(backup)
	want := map[string]bool{"db-0": true, "db-3": true}
	for pod := range got {
		if !want[pod] {
			switch pod {
			case "db-1":
				t.Error("a pod whose quiesce FAILED is owed a thaw: the operator would issue a release " +
					"command over an application it never successfully froze")
			case "db-2":
				t.Error("a pod whose quiesce was SKIPPED is owed a thaw: nothing ever ran in it, so the " +
					"thaw is a command the application owner never asked for")
			case "db-9":
				t.Error("a POST entry was read as a quiesce")
			default:
				t.Errorf("unexpected pod %q in the owed set", pod)
			}
		}
	}
	for pod := range want {
		if _, ok := got[pod]; !ok {
			t.Errorf("%s is not owed a thaw, and it was successfully quiesced — this is the whole defect", pod)
		}
	}
	if len(got) != len(want) {
		t.Errorf("owed set = %v, want %v", got, want)
	}

	// A run with no pre record owes nothing, and the EMPTY (not nil) result is the contract: nil means
	// "no restriction" to advancePostHooks, so returning nil here would silently widen an aborted run's
	// release to every pod in the namespace.
	empty := quiescedPodsFromStatus(&cbv1.Backup{})
	if empty == nil {
		t.Error("quiescedPodsFromStatus returned nil for a run with no pre record; nil means NO RESTRICTION " +
			"downstream, which would thaw every pod including the ones nothing ever froze")
	}
	if len(empty) != 0 {
		t.Errorf("owed set = %v for a Backup with no hooks", empty)
	}
}

// TestRestrictToPodsDistinguishesNilFromEmpty pins the one footgun in the type: len() cannot tell the
// two apart, and they mean opposite things.
func TestRestrictToPodsDistinguishesNilFromEmpty(t *testing.T) {
	resolved := []hooks.Resolved{
		{Pod: types.NamespacedName{Namespace: "ns", Name: "db-0"}},
		{Pod: types.NamespacedName{Namespace: "ns", Name: "db-1"}},
	}
	if got := restrictToPods(resolved, nil); len(got) != 2 {
		t.Errorf("a nil restriction kept %d of 2 hooks; nil means the whole-run release", len(got))
	}
	if got := restrictToPods(resolved, quiescedPods{}); len(got) != 0 {
		t.Errorf("an EMPTY restriction kept %d hooks; it means nothing was frozen", len(got))
	}
	got := restrictToPods(resolved, quiescedPods{"db-1": {}})
	if len(got) != 1 || got[0].Pod.Name != "db-1" {
		t.Errorf("restrictToPods = %+v, want only db-1", got)
	}
}

// TestReleaseAbortedQuiesceOwedIsThreeState is the abort path's own copy of the question
// TestCloseFreezeWindowOwedIsThreeState asks for the normal path, and it has to be asked separately:
// the two functions share every BOUND (postHooksRan, the spec-derived guard, the `!ran` guard,
// advancePostHooks' attempt accounting) and differ in the two things a flag could not express — the
// trigger, which is the abort rather than snapshotsCut, and the scope, which is the pods actually
// frozen rather than everything the post phase resolves.
//
// It uses listOnlyClient (backup_errored_pass_test.go), whose embedded client.Client is nil: the four
// cases that must reach their verdict WITHOUT touching the apiserver are asserted by a client that
// would panic on anything but List and returns the error below when List is reached at all.
//
// The volumes are all PENDING in every case, which is the abort's real state — no volume ever leaves
// Pending once the quiesce failed. That is the assertion behind the missing snapshotsCut gate: a
// release keyed on the snapshots being cut would never fire here at all.
func TestReleaseAbortedQuiesceOwedIsThreeState(t *testing.T) {
	postDeclared := cbv1.HooksSpec{
		Pre:  []cbv1.Hook{{Command: []string{"freeze"}}},
		Post: []cbv1.Hook{{Command: []string{"thaw"}}},
	}
	quiesced := []cbv1.HookStatus{
		{Phase: string(hooks.PhasePre), Pod: "db-0", Result: cbv1.HookSucceeded},
		{Phase: string(hooks.PhasePre), Pod: "db-1", Result: cbv1.HookFailed, OnError: cbv1.HookErrorPolicyFail},
	}
	frozeNothing := []cbv1.HookStatus{
		{Phase: string(hooks.PhasePre), Pod: "db-0", Result: cbv1.HookFailed, OnError: cbv1.HookErrorPolicyFail},
		{Phase: string(hooks.PhasePre), Pod: "db-1", Result: cbv1.HookSkipped, Message: hooks.ErrSkipped.Error()},
	}
	mustNotList := errors.New("the apiserver must not be reached on this path")

	cases := []struct {
		name         string
		spec         cbv1.HooksSpec
		entries      []cbv1.HookStatus
		attempts     int32
		listErr      error
		wantOwed     bool
		wantErr      bool
		wantAttempts int32
	}{
		{
			// THE TRAP, in its abort-path form. A pod WAS frozen, so something is genuinely owed to it —
			// and this run declares no way to release it, so no release can ever be owed and the run must
			// fail NOW. Decided from the spec, before any I/O, or a namespace whose pods cannot be listed
			// holds a doomed run for three more passes and warns three times about a release that could
			// not have existed.
			name:    "a pod was quiesced but no post hooks are declared: nothing can ever be owed",
			spec:    cbv1.HooksSpec{Pre: []cbv1.Hook{{Command: []string{"freeze"}}}},
			entries: quiesced, listErr: mustNotList,
		},
		{
			// The common single-broken-hook case: the quiesce failed on the only pod it reached, so
			// nothing is frozen and the run must fail as promptly as it did before this change.
			name: "post hooks declared but nothing was ever frozen: nothing owed, and no pods-list",
			spec: postDeclared, entries: frozeNothing, listErr: mustNotList,
		},
		{
			// The abort's real state: every volume Pending. snapshotsCut is FALSE here, and gating on it
			// would mean the thaw never fires — which is why this path does not consult it.
			name: "resolution failed over a pod that WAS frozen: hold, and spend an attempt doing it",
			spec: postDeclared, entries: quiesced,
			listErr:      apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "", errors.New("nope")),
			wantOwed:     true,
			wantErr:      true,
			wantAttempts: 1,
		},
		{
			name: "the attempt budget is spent: the hold is over, however the thaw went",
			spec: postDeclared, entries: quiesced,
			attempts: postHookMaxAttempts, listErr: mustNotList,
			wantAttempts: postHookMaxAttempts,
		},
		{
			// The pods list succeeds and resolves nothing for the frozen pod (it is gone, or no exec path
			// is wired). Nothing ran and nothing was SPENT, so reporting owed would be an UNBOUNDED hold —
			// a run that never reaches Failed, which is strictly worse than the gap being closed.
			name: "nothing resolves: nothing owed, and nothing spent",
			spec: postDeclared, entries: quiesced,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &BackupReconciler{
				Client:   &listOnlyClient{err: tc.listErr},
				Recorder: events.NewFakeRecorder(8),
			}
			backup := &cbv1.Backup{
				ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "run"},
				Status: cbv1.BackupStatus{
					Volumes:          []cbv1.VolumeStatus{{Pvc: "v", Phase: status.VolumePhasePending}},
					Hooks:            tc.entries,
					PostHookAttempts: tc.attempts,
				},
			}
			owed, err := r.releaseAbortedQuiesce(context.Background(), backup, tc.spec)
			if owed != tc.wantOwed {
				t.Errorf("owed = %v, want %v", owed, tc.wantOwed)
			}
			if (err != nil) != tc.wantErr {
				t.Errorf("err = %v, want an error: %v", err, tc.wantErr)
			}
			if backup.Status.PostHookAttempts != tc.wantAttempts {
				t.Errorf("postHookAttempts = %d, want %d — an owed hold that spends nothing is an UNBOUNDED "+
					"hold, and the run never reaches a terminal phase at all",
					backup.Status.PostHookAttempts, tc.wantAttempts)
			}
		})
	}
}
