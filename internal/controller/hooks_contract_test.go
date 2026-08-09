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
	"time"

	. "github.com/onsi/ginkgo/v2" //nolint:revive,staticcheck
	. "github.com/onsi/gomega"    //nolint:revive,staticcheck

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
	"github.com/CrystalBackup/CrystalBackup/internal/hooks"
	"github.com/CrystalBackup/CrystalBackup/internal/mover"
)

// THE HOOK CONTRACTS THE CONTROLLER DECLARED AND DID NOT HONOUR (0.6.5, lot 11).
//
// All three defects have the same shape — the API promises something the controller silently drops —
// and none had a single test anywhere, which is how all three shipped.
//
//	A. `onError: Continue` on a PRE hook. The API states twice that it "records the failure and
//	   proceeds"; the controller recorded the failure, threw away the policy, and failed the run.
//	   There was ZERO coverage of Continue in this package. Its POST-hook mirror was affected too,
//	   through the shared recorder: a tolerated release failure spent the whole unfreeze budget.
//	B. A run declaring only POST hooks released nothing: the release was gated on a pre phase having
//	   run, and nothing is recorded when zero pre hooks resolve.
//	C. (found while fixing B) The PRE phase of a post-only run listed pods it had no hook to resolve
//	   against, and a failed listing there is TERMINAL — so a post-only run in a namespace the
//	   operator may not list pods in died over a quiesce it never declared. See
//	   TestResolveHooksIsPerPhase.
//
// The specs below are written so that each names ONE claim and dies for one reason. Every mutation
// named on a spec was actually applied, in a throwaway copy of the tree, and the spec confirmed RED
// before the mutation was reverted — including the two that turned out NOT to kill the spec they
// were guessed for, whose comments now say so rather than claiming coverage that is not there.

// hookConditionG reads a condition off a Backup THROUGH THE UNCACHED CLIENT.
//
// getBackupG already uses k8sClient, which talks straight to the apiserver rather than through the
// manager's cache — and for this file that is the assertion, not an implementation detail. The claim
// being made about a crash-consistency record is that it reached ETCD: a condition that existed only
// in the reconciler's in-memory object, or only in a cache, is exactly the invisible downgrade the
// record exists to prevent.
func hookConditionG(g Gomega, namespace, name, condType string) *metav1.Condition {
	b := getBackupG(g, namespace, name)
	for i := range b.Status.Conditions {
		if b.Status.Conditions[i].Type == condType {
			return &b.Status.Conditions[i]
		}
	}
	return nil
}

// hookEntry returns the first recorded entry for a phase, or nil.
func hookEntry(b cbv1.Backup, phase hooks.Phase) *cbv1.HookStatus {
	for i := range b.Status.Hooks {
		if b.Status.Hooks[i].Phase == string(phase) {
			return &b.Status.Hooks[i]
		}
	}
	return nil
}

var _ = Describe("Consistency hooks: onError=Continue is honoured (defect A)", func() {
	BeforeEach(func() { hookExecutor.reset() })

	// THE DEFECT, in one spec. A user who wrote `onError: Continue` was told by the API that the
	// backup would proceed and got no backup at all: recordHookResults wrote `Failed` whatever the
	// policy said, hookState aborted on any failed pre entry, and openFreezeWindow routed that into a
	// terminal Failed with no snapshot taken. Over-blocking did not err on the safe side — it moved
	// the outage from a degraded restore point to no restore point.
	//
	// The non-negotiable other half is asserted here too, and it is what makes honouring the policy
	// defensible rather than merely permissive: the run carries a DURABLE condition, on the Backup,
	// naming the hook that failed and saying the words "crash-consistent". Without it, `Continue`
	// would be a way for users to silently downgrade their own restore points — invisible at exactly
	// the moment it matters, which is a restore, months later.
	//
	// Mutations that must turn this spec red (all four were run):
	//   - recordHookResults dropping OnError from the entry → the next pass cannot tell the policy
	//     apart and aborts: phase Failed, no mover Job, spec times out on the Job.
	//   - hookState aborting on any failed pre entry (the 0.6.4 behaviour) → identical.
	//   - recordConsistencyVerdict not setting the condition → the condition assertion fails.
	//   - recordConsistencyVerdict setting it True regardless → the reason/message assertions fail.
	//   - recordConsistencyVerdict stamping the verdict on a DeepCopy, so it is computed and never
	//     persisted → also red, which is the point of reading it back through the uncached client: a
	//     record that exists only in the reconciler's object is not a record.
	It("takes the snapshot, Completes, and records a DURABLE crash-consistency condition", func() {
		const (
			location = "hk-cont"
			ns       = "hk-cont-ns"
			run      = "hk-cont-run"
			pvcName  = "data-vol"
		)
		seedInitializedRepo(location, "kek-hk-cont", "s3-hk-cont")
		createTenantNamespace(ns)
		createSourcePVC(ns, pvcName, "ceph-block")
		createMountingPod(ns, "db-0", pvcName, nil)

		hookExecutor.failPod = map[string]error{"db-0": errors.New("command terminated with exit code 1")}

		createHookedParent(run, location, cbv1.PVCSelector{}, cbv1.HooksSpec{
			Pre: []cbv1.Hook{{
				Command: []string{"flush-cache"},
				OnError: cbv1.HookErrorPolicyContinue,
			}},
		})
		createChildBackup(ns, run, location)

		By("the quiesce fails and the failure is recorded WITH the policy that was in effect")
		Eventually(func(g Gomega) {
			b := getBackupG(g, ns, run)
			e := hookEntry(b, hooks.PhasePre)
			g.Expect(e).NotTo(BeNil())
			g.Expect(e.Result).To(Equal(cbv1.HookFailed))
			g.Expect(e.OnError).To(Equal(cbv1.HookErrorPolicyContinue),
				"the policy must be part of the durable record: the abort decision is taken on a LATER "+
					"pass from status alone, so a record that drops it leaves the controller no choice but "+
					"to assume the strictest reading — which is the whole defect")
		}, initTimeout, initPoll).Should(Succeed())

		By("the snapshot IS taken and the volume proceeds: the user asked for the backup to continue")
		jobName := waitForMoverJob(ns, run, pvcName)
		simulateMoverSucceeded(jobName, "node-a", mover.MoverResult{
			OK: true, Operation: string(mover.OpBackup), SnapshotID: "snap-cont", SizeBytes: 4, AddedBytes: 2,
		})

		By("the run reaches Completed — not Failed, and not PartiallyCompleted")
		Eventually(func(g Gomega) {
			b := getBackupG(g, ns, run)
			g.Expect(b.Status.Phase).To(Equal("Completed"),
				"phase=%q — a tolerated quiesce failure must not fail the run, and nothing about it happened "+
					"PARTIALLY: every volume was captured, which is exactly what onError=Continue asked for",
				b.Status.Phase)
			g.Expect(volumeByPVC(b, pvcName).SnapshotID).To(Equal("snap-cont"))
			g.Expect(b.Status.CompletionTime).NotTo(BeNil())
		}, initTimeout, initPoll).Should(Succeed())

		By("and the restore point carries a durable, queryable record that it is crash-consistent only")
		Eventually(func(g Gomega) {
			c := hookConditionG(g, ns, run, ConditionApplicationConsistent)
			g.Expect(c).NotTo(BeNil(),
				"a backup that proceeded past a failed quiesce with nothing on it saying so is a silent "+
					"downgrade of somebody's restore point — worse than the bug being fixed")
			g.Expect(c.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(c.Reason).To(Equal(reasonCrashConsistent))
			g.Expect(c.Message).To(ContainSubstring("CRASH-CONSISTENT"))
			g.Expect(c.Message).To(ContainSubstring("db-0"),
				"the condition must NAME the hook that failed, or the reader cannot tell which part of "+
					"their application is unquiesced in this snapshot")
		}, initTimeout, initPoll).Should(Succeed())

		By("and an Event says the thing status.hooks does not: that the backup CONTINUED")
		Eventually(func(g Gomega) {
			g.Expect(backupWarningNotes(g, ns, run)).To(ContainElement(
				ContainSubstring("proceeding with a CRASH-CONSISTENT backup")),
				"the per-hook HookFailed Warning says a command failed, which on every other path means "+
					"the run is over: on its own it leaves the reader with the wrong expectation")
		}, initTimeout, initPoll).Should(Succeed())
	})

	// THE SAME PROMISE ON THE LESS-TESTED PATH. internal/hooks accepts `on-error: Continue` from a pod
	// annotation, so honorAnnotations offers this contract to anyone who can annotate a pod — and an
	// annotation-sourced hook is resolved by a different code path from a spec-declared one. Honouring
	// the policy for one and not the other would be the same defect with a smaller audience.
	//
	// Mutation that must turn this spec red: fromAnnotations defaulting OnError to Fail unconditionally
	// (i.e. dropping the `on-error` parse) → the run fails and no mover Job is ever created.
	It("honours on-error: Continue when it comes from a pod ANNOTATION", func() {
		const (
			location = "hk-anncont"
			ns       = "hk-anncont-ns"
			run      = "hk-anncont-run"
			pvcName  = "data-vol"
		)
		seedInitializedRepo(location, "kek-hk-anncont", "s3-hk-anncont")
		createTenantNamespace(ns)
		createSourcePVC(ns, pvcName, "ceph-block")
		createMountingPod(ns, "db-0", pvcName, map[string]string{
			apiconst.AnnotationPreBackupPrefix + "command":  "flush-cache",
			apiconst.AnnotationPreBackupPrefix + "on-error": "Continue",
		})

		hookExecutor.failPod = map[string]error{"db-0": errors.New("command terminated with exit code 1")}

		// No spec hooks at all: honorAnnotations is the only thing that resolves anything here, so
		// nothing in this spec can pass through the spec-declared path by accident.
		createHookedParent(run, location, cbv1.PVCSelector{}, cbv1.HooksSpec{HonorAnnotations: true})
		createChildBackup(ns, run, location)

		By("the annotation-sourced failure is recorded as tolerated, and names its source")
		Eventually(func(g Gomega) {
			e := hookEntry(getBackupG(g, ns, run), hooks.PhasePre)
			g.Expect(e).NotTo(BeNil())
			g.Expect(e.Result).To(Equal(cbv1.HookFailed))
			g.Expect(e.Source).To(Equal(string(hooks.SourceAnnotation)))
			g.Expect(e.OnError).To(Equal(cbv1.HookErrorPolicyContinue))
		}, initTimeout, initPoll).Should(Succeed())

		By("the snapshot is taken and the run Completes, exactly as for a spec-declared policy")
		jobName := waitForMoverJob(ns, run, pvcName)
		simulateMoverSucceeded(jobName, "node-a", mover.MoverResult{
			OK: true, Operation: string(mover.OpBackup), SnapshotID: "snap-anncont", SizeBytes: 5, AddedBytes: 1,
		})
		Eventually(func(g Gomega) {
			b := getBackupG(g, ns, run)
			g.Expect(b.Status.Phase).To(Equal("Completed"), "phase=%q", b.Status.Phase)
			c := hookConditionG(g, ns, run, ConditionApplicationConsistent)
			g.Expect(c).NotTo(BeNil())
			g.Expect(c.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(c.Message).To(ContainSubstring("annotation"),
				"the record must say WHERE the tolerating policy came from: on this path it was written "+
					"by whoever can annotate the pod, not by the admin who wrote the schedule")
		}, initTimeout, initPoll).Should(Succeed())
	})

	// THE ANTI-REGRESSION THAT MATTERS MORE THAN THE FIX. What must not happen is "consistency no
	// longer matters": the DEFAULT is still to abort, and R16's argument is untouched — a snapshot
	// taken behind a failed quiesce would look application-consistent without being so, and that is
	// only discovered at restore time.
	//
	// The policy is left UNSET here rather than written as `Fail`, which is the case
	// hooks_backup_test.go does not cover: it proves the CRD's `+kubebuilder:default=Fail` still
	// closes an omitted onError, end to end through a real apiserver.
	//
	// WHAT IT CANNOT PROVE, and the mutation campaign is what said so: making hookFailureTolerated
	// read the EMPTY policy as tolerated does NOT turn this spec red. It cannot — the CRD default
	// means an omitted onError never arrives empty at the controller, so the empty-policy path is
	// unreachable through the API and only reachable through a status entry written by an older
	// binary. That reading is pinned where it lives instead, in
	// TestHookPolicyIsReadBackFromTheRecord's "no policy recorded" case, which the same mutation kills
	// immediately. Two levels, one claim each, rather than one spec asserting something it does not
	// exercise.
	//
	// It also asserts the condition is ABSENT. ApplicationConsistent describes a restore point, and
	// this run has none; labelling the consistency of a backup that was never taken would be noise on
	// the one path where the Ready condition already says everything.
	//
	// Mutation that must turn this spec red: recordConsistencyVerdict recording a verdict for an
	// aborting failure instead of returning (the absence assertion fails).
	It("still fails the run with no snapshot when onError is left unset", func() {
		const (
			location = "hk-unset"
			ns       = "hk-unset-ns"
			run      = "hk-unset-run"
			pvcName  = "data-vol"
		)
		seedInitializedRepo(location, "kek-hk-unset", "s3-hk-unset")
		createTenantNamespace(ns)
		createSourcePVC(ns, pvcName, "ceph-block")
		createMountingPod(ns, "db-0", pvcName, nil)

		hookExecutor.failPod = map[string]error{"db-0": errors.New("command terminated with exit code 1")}

		createHookedParent(run, location, cbv1.PVCSelector{}, cbv1.HooksSpec{
			// No OnError: the CRD defaults it to Fail, and an entry carrying no policy at all is read
			// as Fail too. Both readings have to agree or an omitted policy fails OPEN.
			Pre: []cbv1.Hook{{Command: []string{"pg_backup_start"}}},
		})
		createChildBackup(ns, run, location)

		By("the run fails on the quiesce and no volume leaves Pending")
		Eventually(func(g Gomega) {
			b := getBackupG(g, ns, run)
			g.Expect(b.Status.Phase).To(Equal("Failed"),
				"phase=%q — honouring Continue must not have made the DEFAULT tolerant: a snapshot behind "+
					"a failed quiesce is a backup that LOOKS application-consistent and is not",
				b.Status.Phase)
			for _, v := range b.Status.Volumes {
				g.Expect(string(v.Phase)).To(Or(Equal("Pending"), Equal("")))
			}
			g.Expect(hookConditionG(g, ns, run, ConditionApplicationConsistent)).To(BeNil(),
				"there is no restore point here, so there is nothing whose consistency to describe")
		}, initTimeout, initPoll).Should(Succeed())

		By("and no mover Job was ever created for it")
		Consistently(func(g Gomega) {
			err := k8sClient.Get(ctx, client.ObjectKey{
				Namespace: suiteOperatorNamespace, Name: moverJobNameFor(ns, run, pvcName),
			}, &metav1.PartialObjectMetadata{TypeMeta: metav1.TypeMeta{APIVersion: "batch/v1", Kind: "Job"}})
			g.Expect(err).To(HaveOccurred(), "a snapshot was taken after a quiesce that did not happen")
		}, consistentlyWindow, 500*time.Millisecond).Should(Succeed())
	})

	// THE POST-HOOK MIRROR, and it was affected exactly as suspected: recordHookResults is shared, so
	// a post hook with onError=Continue was recorded like an intolerable one, postHooksRan reported the
	// release as still owed, and the run spent its whole unfreeze budget re-running a command whose
	// failure its author had said to tolerate — while lot 10's hold kept the run non-terminal for the
	// duration and an operator was warned, once loudly, about an application that was not stuck.
	//
	// The QUIESCE succeeds here and only the RELEASE fails, in the same pod, which is the shape every
	// real unfreeze problem has (see stubHookExecutor.failCommand for why that needed a new seam).
	//
	// Mutations that must turn this spec red: postHooksRan ignoring OnError (attempts climb to 3 and
	// the UnfreezeFailed Event fires); untoleratedResults counting tolerated failures (the "retrying"
	// Event fires over a release nobody is retrying).
	It("does not burn the unfreeze budget on a post hook whose failure the user tolerated", func() {
		const (
			location = "hk-postcont"
			ns       = "hk-postcont-ns"
			run      = "hk-postcont-run"
			pvcName  = "data-vol"
		)
		seedInitializedRepo(location, "kek-hk-postcont", "s3-hk-postcont")
		createTenantNamespace(ns)
		createSourcePVC(ns, pvcName, "ceph-block")
		createMountingPod(ns, "db-0", pvcName, nil)

		hookExecutor.failCommand = map[string]error{"thaw": errors.New("command terminated with exit code 1")}

		createHookedParent(run, location, cbv1.PVCSelector{}, cbv1.HooksSpec{
			Pre:  []cbv1.Hook{{Command: []string{"freeze"}}},
			Post: []cbv1.Hook{{Command: []string{"thaw"}, OnError: cbv1.HookErrorPolicyContinue}},
		})
		createChildBackup(ns, run, location)

		By("the quiesce succeeds")
		Eventually(func(g Gomega) {
			e := hookEntry(getBackupG(g, ns, run), hooks.PhasePre)
			g.Expect(e).NotTo(BeNil())
			g.Expect(e.Result).To(Equal(cbv1.HookSucceeded))
		}, initTimeout, initPoll).Should(Succeed())

		jobName := waitForMoverJob(ns, run, pvcName)
		simulateMoverSucceeded(jobName, "node-a", mover.MoverResult{
			OK: true, Operation: string(mover.OpBackup), SnapshotID: "snap-postcont", SizeBytes: 9, AddedBytes: 3,
		})

		By("the release fails, is recorded, and the run reaches a terminal phase anyway")
		Eventually(func(g Gomega) {
			b := getBackupG(g, ns, run)
			e := hookEntry(b, hooks.PhasePost)
			g.Expect(e).NotTo(BeNil())
			g.Expect(e.Result).To(Equal(cbv1.HookFailed))
			g.Expect(e.OnError).To(Equal(cbv1.HookErrorPolicyContinue))
			g.Expect(b.Status.Phase).To(Equal("Completed"),
				"phase=%q — a tolerated release failure owes no retry, so nothing may HOLD the terminal "+
					"phase over it either", b.Status.Phase)
		}, initTimeout, initPoll).Should(Succeed())

		By("and exactly ONE attempt was ever spent, with no page raised about a frozen application")
		Consistently(func(g Gomega) {
			b := getBackupG(g, ns, run)
			g.Expect(b.Status.PostHookAttempts).To(Equal(int32(1)),
				"postHookAttempts=%d — the budget exists for a release that may have left an application "+
					"quiesced; spending it on a failure the spec declared survivable is retrying nothing "+
					"three times", b.Status.PostHookAttempts)
			for _, note := range backupWarningNotes(g, ns, run) {
				g.Expect(note).NotTo(ContainSubstring("may remain quiesced and needs manual attention"),
					"a human was paged over a post-hook failure its own author marked tolerable")
				g.Expect(note).NotTo(ContainSubstring("retrying"),
					"the run promised a retry it will not perform")
			}
		}, consistentlyWindow, 500*time.Millisecond).Should(Succeed())
	})
})

var _ = Describe("Consistency hooks: a run declaring only post hooks (defect B)", func() {
	BeforeEach(func() { hookExecutor.reset() })

	// THE DEFECT. `hooks.pre` and `hooks.post` are independent fields and admission accepts either
	// alone, but the release was gated on a pre phase having RUN — and advancePreHooks records nothing
	// when zero pre hooks resolve, so preRan was false forever. A post-only spec therefore never
	// executed a single command and never said a word about it, which is the same defect class as a
	// policy the recorder throws away.
	//
	// SEMANTICS CHOSEN: post hooks fire once the snapshots are cut, whether or not this operator froze
	// anything. The field is called "post-backup", not "unfreeze", and the two obvious real uses — a
	// "the backup finished" command, and a thaw for something quiesced out of band — are unreachable
	// under the old gate.
	//
	// Mutation that must turn this spec red: restoring the `!st.preRan` gate in closeFreezeWindow → no
	// post entry is ever recorded and this spec times out on the hook count.
	It("runs them once the snapshots are cut, with no pre phase at all", func() {
		const (
			location = "hk-postonly"
			ns       = "hk-postonly-ns"
			run      = "hk-postonly-run"
			pvcName  = "data-vol"
		)
		seedInitializedRepo(location, "kek-hk-postonly", "s3-hk-postonly")
		createTenantNamespace(ns)
		createSourcePVC(ns, pvcName, "ceph-block")
		createMountingPod(ns, "db-0", pvcName, nil)

		createHookedParent(run, location, cbv1.PVCSelector{}, cbv1.HooksSpec{
			// No Pre: nothing is frozen by us, and the release must still happen.
			Post: []cbv1.Hook{{Command: []string{"notify-backup-done"}}},
		})
		createChildBackup(ns, run, location)

		jobName := waitForMoverJob(ns, run, pvcName)
		simulateMoverSucceeded(jobName, "node-a", mover.MoverResult{
			OK: true, Operation: string(mover.OpBackup), SnapshotID: "snap-postonly", SizeBytes: 6, AddedBytes: 4,
		})

		By("the declared post hook RUNS, and is recorded")
		Eventually(func(g Gomega) {
			b := getBackupG(g, ns, run)
			g.Expect(hookPhaseCount(b, hooks.PhasePre)).To(Equal(0),
				"nothing declared a quiesce, so nothing may claim one happened")
			e := hookEntry(b, hooks.PhasePost)
			g.Expect(e).NotTo(BeNil(),
				"a declared hook that never executes and says nothing is the defect: post hooks are not "+
					"conditional on this operator having frozen something first")
			g.Expect(e.Result).To(Equal(cbv1.HookSucceeded))
			g.Expect(e.Pod).To(Equal("db-0"))
		}, initTimeout, initPoll).Should(Succeed())

		By("the command really reached the pod, and only the pod holding the backed-up data")
		Eventually(func(g Gomega) {
			var seen int
			for _, c := range hookExecutor.recorded() {
				g.Expect(c.Pod).To(Equal(types.NamespacedName{Namespace: ns, Name: "db-0"}))
				g.Expect(c.Command).To(Equal([]string{"notify-backup-done"}))
				seen++
			}
			g.Expect(seen).To(BeNumerically(">=", 1))
		}, initTimeout, initPoll).Should(Succeed())

		By("and the run terminates on a bounded, single attempt")
		Eventually(func(g Gomega) {
			b := getBackupG(g, ns, run)
			g.Expect(b.Status.Phase).To(Equal("Completed"), "phase=%q", b.Status.Phase)
			g.Expect(b.Status.PostHookAttempts).To(Equal(int32(1)))
		}, initTimeout, initPoll).Should(Succeed())
	})

	// LOT 10'S TRAP, RE-ASSERTED because this lot edits the very guard that closes it.
	//
	// The hold on the terminal phase cannot be keyed on "a pre hook ran and no post hook is recorded",
	// because that is also true forever of every run that declares pre hooks and no post hooks.
	// Widening the release to post-only runs is exactly the kind of change that reopens it — the
	// `!st.preRan` gate is gone, so the SPEC-derived guard and the `!ran` guard are now the only two
	// things standing between this codebase and a category of healthy runs pinned non-terminal for
	// good. Lot 10's report records that a mutation survived until BOTH existed, and re-running it here
	// reproduced that exactly: reporting owed=true from the `!ran` branch alone leaves this spec GREEN
	// (a pre-only run returns at the spec-derived guard and never reaches `!ran`), and removing the
	// spec-derived guard alone leaves it green too (`!ran` then catches it). Only removing BOTH turns
	// it red — the run never leaves Uploading — which is the honest statement of what it guards: the
	// PAIR, not either half. The half-mutations are each killed by
	// TestCloseFreezeWindowOwedIsThreeState, which is the level they belong at.
	//
	// It carries a second claim that is entirely its own: the POSITIVE half of the consistency
	// tri-state. A quiesce that was asked for and happened says so, which is what lets an operator
	// tell "application-consistent" apart from "nobody asked".
	It("still reaches a terminal phase with pre hooks and NO post hooks", func() {
		const (
			location = "hk-preonly"
			ns       = "hk-preonly-ns"
			run      = "hk-preonly-run"
			pvcName  = "data-vol"
		)
		seedInitializedRepo(location, "kek-hk-preonly", "s3-hk-preonly")
		createTenantNamespace(ns)
		createSourcePVC(ns, pvcName, "ceph-block")
		createMountingPod(ns, "db-0", pvcName, nil)

		createHookedParent(run, location, cbv1.PVCSelector{}, cbv1.HooksSpec{
			Pre: []cbv1.Hook{{Command: []string{"freeze"}}},
		})
		createChildBackup(ns, run, location)

		Eventually(func(g Gomega) {
			g.Expect(hookPhaseCount(getBackupG(g, ns, run), hooks.PhasePre)).To(Equal(1))
		}, initTimeout, initPoll).Should(Succeed())

		jobName := waitForMoverJob(ns, run, pvcName)
		simulateMoverSucceeded(jobName, "node-a", mover.MoverResult{
			OK: true, Operation: string(mover.OpBackup), SnapshotID: "snap-preonly", SizeBytes: 2, AddedBytes: 1,
		})

		Eventually(func(g Gomega) {
			b := getBackupG(g, ns, run)
			g.Expect(b.Status.Phase).To(Equal("Completed"),
				"phase=%q — a run with no post hooks owes no release, so nothing may hold it: no "+
					"backupTime, no completionTime, and a Forbid schedule that skips every night after",
				b.Status.Phase)
			g.Expect(b.Status.CompletionTime).NotTo(BeNil())
			g.Expect(b.Status.PostHookAttempts).To(Equal(int32(0)))
			// The positive half of the tri-state: the quiesce was asked for AND happened.
			c := hookConditionG(g, ns, run, ConditionApplicationConsistent)
			g.Expect(c).NotTo(BeNil())
			g.Expect(c.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(c.Reason).To(Equal(reasonQuiesced))
		}, initTimeout, initPoll).Should(Succeed())
	})
})

// TestHookPolicyIsReadBackFromTheRecord is the unit-level statement of defect A's root cause: the
// abort is decided on a LATER pass than the execution, from status alone, so the policy has to be
// IN status. It also pins the compatibility reading that makes the new field safe to roll out.
//
// Mutation that must turn this red: hookFailureTolerated ignoring OnError (either direction).
func TestHookPolicyIsReadBackFromTheRecord(t *testing.T) {
	cases := []struct {
		name        string
		entry       cbv1.HookStatus
		wantAborted bool
	}{
		{
			name:  "tolerated: the user said proceed, so the run proceeds",
			entry: cbv1.HookStatus{Result: cbv1.HookFailed, OnError: cbv1.HookErrorPolicyContinue},
		},
		{
			name:        "explicit Fail: aborts",
			entry:       cbv1.HookStatus{Result: cbv1.HookFailed, OnError: cbv1.HookErrorPolicyFail},
			wantAborted: true,
		},
		{
			// THE UPGRADE CASE. A Backup already mid-freeze-window when the operator was upgraded has
			// entries written by a binary that recorded no policy. Reading empty as Fail is what keeps
			// those aborting exactly as they did, instead of a newer binary silently reinterpreting
			// them as tolerated — which would be this fix causing the very thing it is guarding against.
			name:        "no policy recorded (an entry written before the field existed): aborts",
			entry:       cbv1.HookStatus{Result: cbv1.HookFailed},
			wantAborted: true,
		},
		{
			name:  "succeeded, with a tolerant policy that never came into play",
			entry: cbv1.HookStatus{Result: cbv1.HookSucceeded, OnError: cbv1.HookErrorPolicyContinue},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entry := tc.entry
			entry.Phase = string(hooks.PhasePre)
			entry.Pod = "db-0"
			st := hookState(&cbv1.Backup{Status: cbv1.BackupStatus{Hooks: []cbv1.HookStatus{entry}}})
			if !st.preRan {
				t.Fatalf("preRan = false: a recorded pre entry means the phase ran, whatever it returned")
			}
			if st.aborted != tc.wantAborted {
				t.Errorf("aborted = %v, want %v", st.aborted, tc.wantAborted)
			}
			if tc.wantAborted && st.abortMessage == "" {
				t.Error("the abort carries no message; the terminal condition would say nothing")
			}
		})
	}
}

// TestRecordHookResultsCarriesThePolicy closes the gap the mutation campaign found in the test suite
// itself: every other unit test here builds HookStatus entries by hand, so deleting the one line in
// recordHookResults that copies the policy onto the entry was invisible below envtest. It is a
// one-line mutation in a shared function, which is exactly the kind that should not need a
// five-minute integration run to be caught.
func TestRecordHookResultsCarriesThePolicy(t *testing.T) {
	r := &BackupReconciler{Recorder: noopRecorder{}}
	backup := &cbv1.Backup{ObjectMeta: metav1.ObjectMeta{Namespace: "team-x", Name: "run"}}

	tolerant := hooks.Resolved{
		Phase: hooks.PhasePre, Pod: types.NamespacedName{Name: "db-0"}, Container: "db",
		Source: hooks.SourceSpec, OnError: cbv1.HookErrorPolicyContinue,
	}
	strict := tolerant
	strict.Pod = types.NamespacedName{Name: "db-1"}
	strict.OnError = cbv1.HookErrorPolicyFail

	r.recordHookResults(backup, []hooks.Result{
		{Hook: tolerant, Err: errors.New("exit 1")},
		{Hook: strict},
	})

	if len(backup.Status.Hooks) != 2 {
		t.Fatalf("recorded %d entries for 2 results", len(backup.Status.Hooks))
	}
	if got := backup.Status.Hooks[0]; got.Result != cbv1.HookFailed || got.OnError != cbv1.HookErrorPolicyContinue {
		t.Errorf("failed entry = {result:%q onError:%q}, want {Failed Continue} — dropping the policy here "+
			"erases the user's answer at the moment it is written down", got.Result, got.OnError)
	}
	// Recorded on successes too, so an empty policy never has to be read as "this operator did not
	// record policies" rather than "Fail".
	if got := backup.Status.Hooks[1].OnError; got != cbv1.HookErrorPolicyFail {
		t.Errorf("succeeded entry onError = %q, want Fail", got)
	}
}

// TestResolveHooksIsPerPhase is the third instance of the same defect class, found while fixing the
// first two: the pre phase of a POST-ONLY run listed pods it had no hook to resolve against, and a
// failed listing in the pre phase is a terminal failure. So a post-only run in a namespace the
// operator may not list pods in died with "the pre-backup quiesce could not be performed" — about a
// quiesce it never declared. Defect B made post-only runs work; this is what makes them work when
// RBAC is narrowed, which is the scenario this codebase keeps discovering the hard way.
//
// It uses listOnlyClient, whose embedded client.Client is nil: a phase that must not perform I/O is
// asserted by a client that can only List, returning the error below if it is reached at all.
//
// Mutation that must turn this red: making hookPhaseDeclared per-RUN again (returning true when
// either list is non-empty) → the pre phase lists, the refusal surfaces, and this run fails.
func TestResolveHooksIsPerPhase(t *testing.T) {
	refused := errors.New("pods is forbidden")
	r := &BackupReconciler{Client: &listOnlyClient{err: refused}}
	postOnly := cbv1.HooksSpec{Post: []cbv1.Hook{{Command: []string{"thaw"}}}}
	vols := []cbv1.VolumeStatus{{Pvc: "data"}}
	backup := &cbv1.Backup{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "run"}}

	resolved, err := r.resolveHooks(context.Background(), backup, postOnly, vols, hooks.PhasePre)
	if err != nil {
		t.Errorf("the PRE phase of a post-only run reached the apiserver and failed: %v — that failure is "+
			"terminal, so the run dies over a quiesce it never declared", err)
	}
	if len(resolved) != 0 {
		t.Errorf("resolved %d pre hooks for a run that declares none", len(resolved))
	}

	// The POST phase of the same run MUST still list: that is where its hooks live, and a refusal there
	// is a genuine release failure the run has to be loud about.
	if _, err := r.resolveHooks(context.Background(), backup, postOnly, vols, hooks.PhasePost); err == nil {
		t.Error("the POST phase did not list pods: a declared release must be resolved, and a refused " +
			"listing must surface as the release failure it is")
	}

	// honorAnnotations makes both phases possible whatever the spec lists — the commands are on the pods.
	annotated := cbv1.HooksSpec{HonorAnnotations: true}
	if _, err := r.resolveHooks(context.Background(), backup, annotated, vols, hooks.PhasePre); err == nil {
		t.Error("honorAnnotations did not list pods for the pre phase: an annotated pod's command can only " +
			"be discovered by listing, so skipping it would silently ignore the annotations")
	}
}

// TestPostHooksRanToleratesWhatItWasToldTo is the same rule on the shared function's other half.
// recordHookResults serves both phases, so fixing only the pre side would have left this one looking
// done while still spending an unfreeze budget on a failure the spec declared survivable.
//
// Mutation that must turn this red: postHooksRan ignoring OnError.
func TestPostHooksRanToleratesWhatItWasToldTo(t *testing.T) {
	post := func(onErr cbv1.HookErrorPolicy) *cbv1.Backup {
		return &cbv1.Backup{Status: cbv1.BackupStatus{
			PostHookAttempts: 1,
			Hooks: []cbv1.HookStatus{{
				Phase: string(hooks.PhasePost), Pod: "db-0", Result: cbv1.HookFailed, OnError: onErr,
			}},
		}}
	}
	if !postHooksRan(post(cbv1.HookErrorPolicyContinue)) {
		t.Error("a tolerated release failure still owed a retry: the run would spend its whole unfreeze " +
			"budget re-running a command whose failure its author said to tolerate")
	}
	if postHooksRan(post(cbv1.HookErrorPolicyFail)) {
		t.Error("an INTOLERABLE release failure must still owe a retry — the outstanding problem is an " +
			"application that may still be frozen")
	}
	if postHooksRan(post("")) {
		t.Error("an entry with no recorded policy must be read as Fail, or an upgrade silently stops " +
			"retrying releases it used to retry")
	}
}

// TestUntoleratedResultsCountsDecisionsNotEvents pins the split between the two counts, which is
// subtle enough to be re-merged by accident: failedResults feeds the RECORD (the span, which must
// show a command that exited non-zero however its policy read), untoleratedResults feeds the
// DECISIONS (retry the release? page a human?).
func TestUntoleratedResultsCountsDecisionsNotEvents(t *testing.T) {
	tolerant := hooks.Resolved{Phase: hooks.PhasePost, OnError: cbv1.HookErrorPolicyContinue}
	strict := hooks.Resolved{Phase: hooks.PhasePost, OnError: cbv1.HookErrorPolicyFail}
	results := []hooks.Result{
		{Hook: tolerant, Err: errors.New("exit 1")},
		{Hook: strict, Err: errors.New("exit 1")},
		// Skipped is neither: it never ran, and counting it would report one failure as two.
		{Hook: strict, Err: hooks.ErrSkipped},
		{Hook: strict},
	}
	if got := failedResults(results); got != 2 {
		t.Errorf("failedResults = %d, want 2 — the trace must show every command that failed", got)
	}
	if got := untoleratedResults(results); got != 1 {
		t.Errorf("untoleratedResults = %d, want 1 — only the intolerable failure owes anybody anything", got)
	}
}
