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
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
)

// clusterLister is the production podLister: a plain clientset, no informers.
//
// No cache and no manager. A resident process with a cluster-wide pod informer would hold every
// pod in the cluster in memory for a fortnight inside a 384Mi limit, which is how a collector
// becomes the incident. Listing the operator namespace's mover pods every fifteen seconds costs
// one API call against a handful of objects.
type clusterLister struct {
	cs *kubernetes.Clientset
}

func (l *clusterLister) ListPods(ctx context.Context, ns, selector string) (*corev1.PodList, error) {
	return l.cs.CoreV1().Pods(ns).List(ctx, listOptionsFor(selector))
}

func (l *clusterLister) ListJobs(ctx context.Context, ns, selector string) (*batchv1.JobList, error) {
	return l.cs.BatchV1().Jobs(ns).List(ctx, listOptionsFor(selector))
}

// WatchPods and WatchJobs are the event half (see moverEventWatcher). Same namespace, same
// selector, same handful of objects as the List above — a watch on a label-selected slice of one
// namespace is not the cluster-wide informer this package rules out, it is one long-lived HTTP
// response carrying only mover events.
func (l *clusterLister) WatchPods(ctx context.Context, ns, selector string) (watch.Interface, error) {
	return l.cs.CoreV1().Pods(ns).Watch(ctx, listOptionsFor(selector))
}

func (l *clusterLister) WatchJobs(ctx context.Context, ns, selector string) (watch.Interface, error) {
	return l.cs.BatchV1().Jobs(ns).Watch(ctx, listOptionsFor(selector))
}

// podMetricsList is the shape of metrics.k8s.io/v1beta1 PodMetricsList, transcribed rather than
// imported.
//
// k8s.io/metrics is not in this module's dependency graph and adding a module for four fields
// would be a new supply-chain edge on the operator image for the benefit of one subcommand — the
// same argument cmd/main.go makes about a second binary. The API is beta and has been stable for
// a decade; what this needs from it is a pod name and a memory quantity.
type podMetricsList struct {
	Items []struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Containers []struct {
			Name  string `json:"name"`
			Usage struct {
				Memory string `json:"memory"`
			} `json:"usage"`
		} `json:"containers"`
	} `json:"items"`
}

// PodMetrics reads live memory usage for one namespace.
//
// The container usages are SUMMED rather than taking the mover container alone. A mover pod has
// exactly one container today, but the cgroup the memory limit applies to is the pod's, and a
// peak attributed to one container of two would understate exactly the case where the limit is
// about to be hit.
func (l *clusterLister) PodMetrics(ctx context.Context, ns string) ([]podMetric, error) {
	raw, err := l.cs.RESTClient().Get().
		AbsPath("/apis/metrics.k8s.io/v1beta1/namespaces/" + ns + "/pods").
		DoRaw(ctx)
	if err != nil {
		return nil, err
	}
	var list podMetricsList
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("decode PodMetrics: %w", err)
	}
	out := make([]podMetric, 0, len(list.Items))
	for _, item := range list.Items {
		var total int64
		for _, c := range item.Containers {
			if v, ok := parseQuantity(c.Usage.Memory); ok {
				total += v
			}
		}
		out = append(out, podMetric{Name: item.Metadata.Name, MemoryUsed: total})
	}
	return out, nil
}

// kubeletSummary is the sliver of the kubelet's stats/summary this needs.
type kubeletSummary struct {
	Pods []struct {
		PodRef struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		} `json:"podRef"`
		Volume []struct {
			Name      string `json:"name"`
			UsedBytes int64  `json:"usedBytes"`
		} `json:"volume"`
	} `json:"pods"`
}

// NodeStats reads one node's kubelet stats through nodes/proxy.
//
// This is the only source for the restic cache high-water: the cache is an emptyDir inside a
// mover pod and nothing in the API reports its size. §10 rules out the alternative — exec'ing
// into a mover pod during a backup — as perturbation of the thing being measured, and the
// operator's ClusterRole would even allow it. The kubelet answers the same question from outside.
func (l *clusterLister) NodeStats(ctx context.Context, node string) ([]podVolumeStats, error) {
	raw, err := l.cs.RESTClient().Get().
		AbsPath("/api/v1/nodes/" + node + "/proxy/stats/summary").
		DoRaw(ctx)
	if err != nil {
		return nil, err
	}
	var summary kubeletSummary
	if err := json.Unmarshal(raw, &summary); err != nil {
		return nil, fmt.Errorf("decode the kubelet stats summary: %w", err)
	}
	out := make([]podVolumeStats, 0, len(summary.Pods))
	for _, p := range summary.Pods {
		vols := map[string]int64{}
		for _, v := range p.Volume {
			vols[v.Name] = v.UsedBytes
		}
		out = append(out, podVolumeStats{
			Namespace: p.PodRef.Namespace, Pod: p.PodRef.Name, Volumes: vols,
		})
	}
	return out, nil
}

// readFile is the one path-joining read this package does against the data directory. Centralised
// so nothing in here ever builds a path out of untrusted input.
func readFile(dir, rel string) ([]byte, error) {
	return os.ReadFile(filepath.Join(dir, filepath.Clean(rel))) // #nosec G304 -- rel is a constant
}
