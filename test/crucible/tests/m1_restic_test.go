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
	"bytes"
	"os"
	"os/exec"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/CrystalBackup/CrystalBackup/internal/restic"
)

// ---------------------------------------------------------------------------
// The crucible's independent restic oracle. The M1 acceptance specs must be
// able to read the shared repository WITHOUT trusting any CrystalBackup CR —
// otherwise a controller bug that both writes and reports the same wrong thing
// would pass. m1ResticExec runs the real `restic` binary in a Job against the
// same repo URL, wrapped-DEK password and S3 credentials the operator uses, so
// what it sees is ground truth about the object store.
// ---------------------------------------------------------------------------

// m1ResticImage is the restic image the in-cluster restic Jobs run — overridable so an
// air-gapped crucible can point at a mirror; defaults to a public restic release.
func m1ResticImage() string {
	return envOr("CRUCIBLE_RESTIC_IMAGE", "restic/restic:0.17.3")
}

// m1ResticExec runs `restic <args...>` against locationName's shared repository from a
// short-lived Job in the operator namespace, waits for it to finish, and returns the pod's
// captured stdout/stderr log. The Job is deleted afterwards (best-effort) so repeated calls
// do not accumulate completed Jobs.
func m1ResticExec(locationName string, args ...string) string {
	GinkgoHelper()

	repoURL := restic.RepoURL(os.Getenv("S3_ENDPOINT"), os.Getenv("S3_BUCKET"), m1S3Prefix, m1ClusterID)
	password := m1UnwrapDEK(locationName)

	backoff, deadline := int32(0), int64(300)
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "crucible-restic-", Namespace: operatorNS},
		Spec: batchv1.JobSpec{
			BackoffLimit:          &backoff,
			ActiveDeadlineSeconds: &deadline,
			Template: corev1.PodTemplateSpec{
				// The oracle talks to object storage exactly like a data mover, so it needs the
				// mover's egress. Under M3's default-deny NetworkPolicy (03-security §7), a pod in
				// the operator namespace reaches S3 ONLY if it matches the mover-egress policy,
				// which selects app.kubernetes.io/managed-by=crystal-backup (chart
				// networkPolicy.moverManagedByValue). Without this label the Job is default-denied
				// and restic times out dialing the S3 endpoint. This is NOT a crystalbackup.io/*
				// label, so m1HasCrystalLabel still excludes the oracle from the mover-Job predicates.
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app.kubernetes.io/managed-by": "crystal-backup"},
				},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{{
						Name:    "restic",
						Image:   m1ResticImage(),
						Command: []string{"restic"},
						Args:    args,
						Env: []corev1.EnvVar{
							{Name: "RESTIC_REPOSITORY", Value: repoURL},
							{Name: "RESTIC_PASSWORD", Value: password},
							{Name: "AWS_ACCESS_KEY_ID", Value: os.Getenv("AWS_ACCESS_KEY_ID")},
							{Name: "AWS_SECRET_ACCESS_KEY", Value: os.Getenv("AWS_SECRET_ACCESS_KEY")},
							{Name: "AWS_DEFAULT_REGION", Value: envOr("S3_REGION", "fsn1")},
						},
					}},
				},
			},
		},
	}
	Expect(k8s.Create(ctx, job)).To(Succeed(), "create restic Job in %s", operatorNS)
	defer func() {
		_ = k8s.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground))
	}()

	Eventually(func(g Gomega) {
		var got batchv1.Job
		g.Expect(k8s.Get(ctx, client.ObjectKeyFromObject(job), &got)).To(Succeed())
		g.Expect(got.Status.Succeeded+got.Status.Failed).To(BeNumerically(">", 0),
			"restic Job %s/%s has not finished (active=%d)", operatorNS, job.Name, got.Status.Active)
	}, 5*time.Minute, 3*time.Second).Should(Succeed())

	return m1PodLogs(job.Name)
}

// m1PodLogs returns the merged container log of a finished Job's pod via `kubectl logs`
// (controller-runtime's client cannot stream logs; kubectl inherits KUBECONFIG the same way
// the crucible's other shell-outs do).
func m1PodLogs(jobName string) string {
	GinkgoHelper()
	out, err := exec.Command("kubectl", "-n", operatorNS, "logs", "job/"+jobName, "--tail=-1").Output()
	if err != nil {
		stderr := ""
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = string(ee.Stderr)
		}
		Expect(err).NotTo(HaveOccurred(), "kubectl logs job/%s in %s failed: %s", jobName, operatorNS, stderr)
	}
	return string(out)
}

// m1ResticSnapshots lists the repository's CrystalBackup snapshots
// (`restic snapshots --json --tag crystalbackup`) and parses them. The JSON array is sliced
// out of the merged log first, so an incidental restic stderr line never breaks the parse.
func m1ResticSnapshots(locationName string) []restic.Snapshot {
	GinkgoHelper()
	out := m1ResticExec(locationName, "snapshots", "--json", "--tag", restic.TagBase)
	snaps, err := restic.ParseSnapshots(m1JSONArray([]byte(out)))
	Expect(err).NotTo(HaveOccurred(), "parse `restic snapshots --json` output: %s", out)
	return snaps
}

// m1JSONArray returns the outermost [ ... ] slice of b, or b unchanged when there is no
// bracketed array (so ParseSnapshots surfaces the real restic output in its error message).
func m1JSONArray(b []byte) []byte {
	start := bytes.IndexByte(b, '[')
	end := bytes.LastIndexByte(b, ']')
	if start >= 0 && end >= start {
		return b[start : end+1]
	}
	return b
}
