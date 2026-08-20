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
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	batchv1 "k8s.io/api/batch/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
	"github.com/CrystalBackup/CrystalBackup/internal/mover"
)

// ---------------------------------------------------------------------------------------------
// M6 — s3.connections reaches the mover, and raising it does not break the gateway.
//
// WHY THIS SPEC EXISTS AT ALL, given the unit tests already assert the argv.
//
// `internal/mover/job_test.go` proves BuildJob emits `-o s3.connections=N`, and
// `internal/controller/mover_wiring_test.go` proves every call site sets the field. Neither can
// prove the value survives the trip through a real ClusterBackupLocation, a real controller and a
// real Job — which is exactly the gap that let JobRequest.GoMemLimit sit unassigned from M1 until
// 0.6.1 with a passing test suite the whole time. So the first It reads the argv off a Job the
// cluster actually created.
//
// AND ONE PROPERTY THIS SPEC GETS FOR FREE, which is worth knowing before reading the assertions.
// Measured against the pinned engine (restic 0.19.1, build/melange/restic.yaml): restic IGNORES an
// option whose namespace it does not apply — `-o s3.connections=8` on a local repository exits 0,
// and so does `-o s3.connections=abc`, unparsed — but it is FATAL on an unknown key inside a
// namespace it DOES apply (`-o local.bogus=1` → "Fatal: option local.bogus is not known"). The
// repository here is s3, so restic applies the s3 namespace: a misspelled key would kill the mover
// rather than be ignored. A backup that COMPLETES with the flag present therefore proves restic
// recognised the key, which no amount of argv assertion can.
//
// What it still does not prove is that restic HONOURED the number — a connection cap is not
// observable from the outside. That is what the wave below is for, and why the wave reports data
// rather than asserting a threshold.
// ---------------------------------------------------------------------------------------------

const (
	// m6TuneConnections is the value under test. Well inside the CRD's 1..100 bound, and different
	// from restic's own default of 5 so that finding it in the argv cannot be a coincidence.
	m6TuneConnections int32 = 8

	// m6TuneWaveConnections are the rising waves. The point is not to find the knee — that is a
	// property of somebody's gateway, not of this operator — but to establish that raising the cap
	// does not start producing errors on the one gateway we do control.
	m6TuneWaveLow  int32 = 2
	m6TuneWaveHigh int32 = 32
)

var _ = Describe("Milestone M6 — S3 connection tuning", Ordered, Label("m6", "s3tuning"), func() {
	// priorConnections is what the location carried before this spec touched it, so AfterAll can
	// put it back exactly — including "it was unset", which is a different state from "it was 5"
	// and the one a naive restore would destroy.
	var priorConnections *int32
	var captured bool

	BeforeAll(func() {
		// This is the lane the 0.6.7 campaign died in, twice-removed from its own subject: it only
		// WAITED for the shared "dr" repository, never established it, so it passed or failed on
		// Ginkgo's container shuffle. It timed out after 600s at 03:01:52 and the location was
		// created at ~03:07 by a lane that ran later; the verdict printed was a connection-cap
		// failure on a spec that had not yet set a connection cap. BeforeSuite creates the
		// location now, and this ensures-then-waits like every other lane.
		m1EnsureSharedRepository()

		var loc cbv1.ClusterBackupLocation
		Expect(k8s.Get(ctx, client.ObjectKey{Name: m1LocationName}, &loc)).To(Succeed())
		priorConnections = loc.Spec.S3.Connections
		captured = true
	})

	AfterAll(func() {
		// Only restore what was actually captured. A BeforeAll that failed before reading the
		// location must not have its zero value written into the cluster as if it were a fact —
		// the same rule the m6 pre-check spec applies to a replica count it never captured.
		if !captured {
			return
		}
		m6SetConnections(priorConnections)
	})

	It("carries the location's connection cap into the mover Job the cluster actually created", func() {
		m6SetConnections(&[]int32{m6TuneConnections}[0])

		run := fmt.Sprintf("m6tune-%d", time.Now().Unix())
		By("running a ClusterBackup with the cap set on the location")
		m1RunClusterBackup(run, m1LocationName, cbv1.NamespaceSelector{MatchNames: []string{"c-db"}})

		By("and finding the flag in a mover Job's argv, read off the API rather than off BuildJob")
		want := fmt.Sprintf("%s=%d", "s3.connections", m6TuneConnections)
		var seen []string
		Eventually(func(g Gomega) {
			args, jobs := m6MoverArgs(run)
			g.Expect(jobs).NotTo(BeZero(),
				"no mover Job for run %s yet; the argv cannot be read from a Job that does not exist", run)
			seen = args
			g.Expect(strings.Join(args, " ")).To(ContainSubstring(want),
				"a mover Job for run %s carries no %q. The value reached the CRD and stopped somewhere "+
					"between there and the Job — which is the shape of the defect that left GoMemLimit "+
					"unassigned for six milestones with every unit test green.\nargv: %v",
				run, want, args)
		}, 5*time.Minute, 5*time.Second).Should(Succeed())

		By("and the option preceding restic's subcommand, where restic documents its global flags")
		idx := indexOf(seen, "-o")
		Expect(idx).To(BeNumerically(">=", 0), "no -o flag at all in %v", seen)
		sep := indexOf(seen, "--")
		Expect(sep).To(BeNumerically(">=", 0), "no -- separator in %v", seen)
		Expect(idx).To(BeNumerically(">", sep),
			"the -o flag must sit AFTER the shim's -- separator (it is restic's argv, not the shim's) "+
				"and before restic's own subcommand: %v", seen)

		By("and the run completing, which is what proves restic ACCEPTED the key")
		// Not decoration. On an s3 repository restic applies the s3 namespace, so an unknown key
		// there is fatal — see this file's header. A completed run is the only evidence available
		// that `s3.connections` is a key this engine knows, and it is evidence a unit test asserting
		// the same constant against itself structurally cannot produce.
		cb := m1WaitClusterBackupTerminal(run, 15*time.Minute)
		Expect(cb.Status.Phase).To(Equal("Completed"),
			"the backup did not complete with -o %s set. If restic rejected the option the mover "+
				"would have died with \"option ... is not known\"; check the mover logs before "+
				"assuming this is a storage problem.", want)
	})

	It("does not start failing movers as the cap rises", func() {
		type wave struct {
			connections int32
			seconds     float64
			failed      int
		}
		caps := []int32{m6TuneWaveLow, m6TuneConnections, m6TuneWaveHigh}
		waves := make([]wave, 0, len(caps))

		for _, n := range caps {
			m6SetConnections(&[]int32{n}[0])

			run := fmt.Sprintf("m6wave-%d-%d", n, time.Now().Unix())
			started := time.Now()
			m1RunClusterBackup(run, m1LocationName, cbv1.NamespaceSelector{MatchNames: []string{"c-db", "c-media"}})
			cb := m1WaitClusterBackupTerminal(run, 20*time.Minute)
			elapsed := time.Since(started).Seconds()

			failed := 0
			for _, b := range append(m1ListBackups("c-db"), m1ListBackups("c-media")...) {
				if b.Labels[apiconst.LabelClusterBackup] != run {
					continue
				}
				for _, v := range b.Status.Volumes {
					if string(v.Phase) == "Failed" {
						failed++
					}
				}
			}
			waves = append(waves, wave{connections: n, seconds: elapsed, failed: failed})

			// REPORTED, not asserted. Where the throughput knee sits is a property of this
			// gateway's rgw_max_concurrent_requests, not of the operator, and a threshold here
			// would fail on somebody else's cluster for a reason that is not a defect.
			AddReportEntry(fmt.Sprintf("s3.connections=%d", n),
				fmt.Sprintf("run %s: %.0fs wall clock, %d failed volume(s), phase %s",
					run, elapsed, failed, cb.Status.Phase))
		}

		By("and the highest wave failing no mover on the gateway's account")
		// The ONE hard assertion, and the regression the knob exists to prevent: raising the cap
		// must not push the gateway into refusing requests. 503 / SlowDown is how an RGW at
		// rgw_max_concurrent_requests answers, and restic surfaces it as a failed mover.
		highest := waves[len(waves)-1]
		Expect(highest.failed).To(BeZero(),
			"raising s3.connections to %d failed %d volume(s). If the mover logs carry 503 or "+
				"SlowDown, the cap is now above what this gateway's rgw_max_concurrent_requests "+
				"will serve — which is the failure this knob exists to let an operator avoid, not "+
				"one it should cause.", highest.connections, highest.failed)
	})
})

// m6SetConnections patches the DR location's s3.connections, including back to unset.
//
// A nil write is a real state and not a no-op: "the field is absent" means restic's own default
// applies and is restic's to change, which is a different guarantee from "the field is 5".
func m6SetConnections(n *int32) {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		var loc cbv1.ClusterBackupLocation
		g.Expect(k8s.Get(ctx, client.ObjectKey{Name: m1LocationName}, &loc)).To(Succeed())
		loc.Spec.S3.Connections = n
		g.Expect(k8s.Update(ctx, &loc)).To(Succeed())
	}, time.Minute, 2*time.Second).Should(Succeed(),
		"could not set s3.connections on %s", m1LocationName)
}

// m6MoverArgs returns the argv of one mover Job belonging to a run, and how many it found.
//
// Selected by the identity label mover.BuildJob stamps on every Job it builds — the label that
// meant "the four call sites that remembered" until 0.6.2, and that means every mover since.
func m6MoverArgs(run string) ([]string, int) {
	var jobs batchv1.JobList
	if err := k8s.List(ctx, &jobs,
		client.InNamespace(operatorNS),
		client.MatchingLabels{mover.LabelAppName: mover.AppName},
	); err != nil {
		return nil, 0
	}
	found := 0
	var args []string
	for i := range jobs.Items {
		j := &jobs.Items[i]
		if j.Labels[apiconst.LabelClusterBackup] != run {
			continue
		}
		found++
		if len(j.Spec.Template.Spec.Containers) > 0 && args == nil {
			args = j.Spec.Template.Spec.Containers[0].Args
		}
	}
	return args, found
}

func indexOf(in []string, want string) int {
	for i, s := range in {
		if s == want {
			return i
		}
	}
	return -1
}
