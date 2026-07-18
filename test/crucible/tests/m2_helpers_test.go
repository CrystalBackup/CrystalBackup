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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
	"github.com/CrystalBackup/CrystalBackup/internal/status"
)

// ---------------------------------------------------------------------------
// M2 shared helpers: self-contained volume fixtures (seed / mutate / verify all
// run as short Jobs mounting the PVC — no exec, RWO-safe because the specs
// serialize them), restore-CR drivers, and the M2 leak-check extension.
// ---------------------------------------------------------------------------

// m2VolumeJob runs a short busybox Job in ns with pvcName mounted RW at /data and waits for
// it to finish; returns success + log. The building block of seed/mutate/verify.
func m2VolumeJob(ns, pvcName, name, script string) (bool, string) {
	GinkgoHelper()
	backoff, deadline := int32(0), int64(600)
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{GenerateName: name + "-", Namespace: ns},
		Spec: batchv1.JobSpec{
			BackoffLimit:          &backoff,
			ActiveDeadlineSeconds: &deadline,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{{
						Name:    "vol",
						Image:   "busybox:1.36",
						Command: []string{"/bin/sh", "-c"},
						Args:    []string{script},
						VolumeMounts: []corev1.VolumeMount{
							{Name: "data", MountPath: "/data"},
						},
					}},
					Volumes: []corev1.Volume{{
						Name: "data",
						VolumeSource: corev1.VolumeSource{
							PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: pvcName},
						},
					}},
				},
			},
		},
	}
	Expect(k8s.Create(ctx, job)).To(Succeed(), "create %s Job in %s", name, ns)
	defer func() {
		_ = k8s.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground))
	}()

	var final batchv1.Job
	Eventually(func(g Gomega) {
		g.Expect(k8s.Get(ctx, client.ObjectKeyFromObject(job), &final)).To(Succeed())
		g.Expect(final.Status.Succeeded+final.Status.Failed).To(BeNumerically(">", 0),
			"%s Job %s/%s has not finished (active=%d)", name, ns, job.Name, final.Status.Active)
	}, 10*time.Minute, 5*time.Second).Should(Succeed())
	return final.Status.Succeeded > 0, m1PodLogs(job.Name)
}

// m2SeedScript writes a deterministic corpus (a few files across two directories) plus its
// MANIFEST.sha256 (relative entries, like the platform seed) and an untouched MANIFEST.copy
// for later drift comparisons.
const m2SeedScript = `set -e
cd /data
mkdir -p alpha beta
for i in 1 2 3; do head -c $((i * 1024)) /dev/urandom > alpha/file$i.bin; done
head -c 4096 /dev/urandom > beta/blob.bin
find . -type f | sort | xargs sha256sum > MANIFEST.sha256
cp MANIFEST.sha256 MANIFEST.copy
sync`

// m2SeedVolume provisions ns/pvcName on storageClass (capacity per the caller) and seeds it.
func m2SeedVolume(ns, pvcName, storageClass, capacity string) {
	GinkgoHelper()
	ensureNamespace(ns)
	sc := storageClass
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: pvcName, Namespace: ns},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: &sc,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(capacity)},
			},
		},
	}
	Expect(client.IgnoreAlreadyExists(k8s.Create(ctx, pvc))).To(Succeed())
	ok, log := m2VolumeJob(ns, pvcName, "m2-seed", m2SeedScript)
	Expect(ok).To(BeTrue(), "seeding %s/%s must succeed:\n%s", ns, pvcName, log)
}

// m2WaitRestoreTerminal waits for a namespaced Restore to reach a terminal phase.
func m2WaitRestoreTerminal(ns, name string, timeout time.Duration) *cbv1.Restore {
	GinkgoHelper()
	var r cbv1.Restore
	Eventually(func(g Gomega) {
		g.Expect(k8s.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &r)).To(Succeed())
		g.Expect(isTerminalM2Phase(r.Status.Phase)).To(BeTrue(),
			"Restore %s/%s not terminal yet (phase=%q)", ns, name, r.Status.Phase)
	}, timeout, 5*time.Second).Should(Succeed())
	return &r
}

// m2WaitClusterRestoreTerminal waits for a ClusterRestore to reach a terminal phase.
func m2WaitClusterRestoreTerminal(name string, timeout time.Duration) *cbv1.ClusterRestore {
	GinkgoHelper()
	var cr cbv1.ClusterRestore
	Eventually(func(g Gomega) {
		g.Expect(k8s.Get(ctx, client.ObjectKey{Name: name}, &cr)).To(Succeed())
		g.Expect(isTerminalM2Phase(cr.Status.Phase)).To(BeTrue(),
			"ClusterRestore %s not terminal yet (phase=%q)", name, cr.Status.Phase)
	}, timeout, 5*time.Second).Should(Succeed())
	return &cr
}

func isTerminalM2Phase(phase string) bool { return status.IsTerminalRestorePhase(phase) }

// m2AssertNoResidualRestoreObjects extends the leak-check invariant to the M2 restore
// machinery: after a scenario, the operator namespace holds no restore mover Jobs, creds
// Secrets or staging claims for the given owners, and no crystalbackup.io/pv-role-labeled
// PersistentVolume survives anywhere (a delivered transplant is unlabeled by handover; a
// twin is deleted by teardown).
func m2AssertNoResidualRestoreObjects() {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		sel := client.MatchingLabels{apiconst.LabelManagedBy: apiconst.ManagedByValue}

		var jobs batchv1.JobList
		g.Expect(k8s.List(ctx, &jobs, client.InNamespace(operatorNS), sel)).To(Succeed())
		for i := range jobs.Items {
			j := &jobs.Items[i]
			owned := j.Labels[apiconst.LabelRestore] != "" || j.Labels[apiconst.LabelClusterRestore] != ""
			g.Expect(owned).To(BeFalse(), "residual restore mover Job %s", j.Name)
		}

		var pvcs corev1.PersistentVolumeClaimList
		g.Expect(k8s.List(ctx, &pvcs, client.InNamespace(operatorNS), sel)).To(Succeed())
		for i := range pvcs.Items {
			p := &pvcs.Items[i]
			owned := p.Labels[apiconst.LabelRestore] != "" || p.Labels[apiconst.LabelClusterRestore] != ""
			g.Expect(owned && p.DeletionTimestamp == nil).To(BeFalse(), "residual restore staging PVC %s", p.Name)
		}

		var pvs corev1.PersistentVolumeList
		g.Expect(k8s.List(ctx, &pvs, client.HasLabels{apiconst.LabelPVRole})).To(Succeed())
		g.Expect(pvs.Items).To(BeEmpty(), func() string {
			names := ""
			for i := range pvs.Items {
				names += " " + pvs.Items[i].Name
			}
			return fmt.Sprintf("residual pv-role-labeled PersistentVolumes:%s", names)
		}())
	}, 5*time.Minute, 5*time.Second).Should(Succeed())
}
