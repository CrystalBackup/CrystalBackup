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

package metrics

import (
	"context"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// pvcLabel names a source PersistentVolumeClaim. It appears in exactly ONE metric, and that is
// the whole point of the comment below.
const pvcLabel = "pvc"

// crystalbackup_pvc_volumesnapshot_count (spec/05-observability.md §2.9) — the catalogue's single
// documented exception to the §1 no-per-PVC-label rule.
//
// The exception is earned by what it prevents. When Crystal Backup runs alongside an incumbent
// tool (Velero, adr/0006), both create VolumeSnapshots of the same PVCs, and on ceph-csi a volume
// that accumulates snapshots past the driver's flatten threshold gets FLATTENED — a full,
// synchronous copy of the image, in the middle of a production workload's I/O path. Nothing in
// either tool reports it; the first symptom is a database going slow for no visible reason. A
// per-PVC series is the only shape that can point at the volume in question, and per-namespace
// totals would average the one PVC at 40 snapshots into a fleet that looks fine.
//
// Cardinality is bounded by the live PVC count and shrinks with it (the series stops being
// emitted the scrape after the PVC is deleted, because this is derived state, not a counter).
//
// It counts EVERYONE'S snapshots, not the operator's — deliberately. A count restricted to
// Crystal Backup's own exposures would read reassuringly low on exactly the cluster where the
// pile-up is happening, since the incumbent tool's snapshots are the ones stacking up. Coexistence
// is the scenario; measuring only our half of it measures nothing.
var pvcVolumeSnapshotCountDesc = prometheus.NewDesc(
	NamePVCVolumeSnapshotting,
	"VolumeSnapshot objects per source PVC, from ALL tools (an incumbent's included) — the ceph-csi flatten-threshold signal during coexistence.",
	[]string{namespaceLabel, pvcLabel, clusterLabel}, nil)

// volumeSnapshotListGVK is the snapshot API's list kind. The type is not in the operator's scheme
// (internal/exposer works through unstructured for the same reason), so the collector asks for it
// by GVK.
var volumeSnapshotListGVK = schema.GroupVersionKind{
	Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshotList",
}

// collectPVCSnapshots counts the VolumeSnapshots pointing at each source PVC, cluster-wide.
//
// A listing error emits nothing, which is also the correct behaviour on a cluster with no
// snapshot CRDs installed at all: there, the List fails with a no-kind-match and this family is
// simply absent, rather than reporting a fleet-wide zero that would look like a measurement.
func (c *Collector) collectPVCSnapshots(ctx context.Context, ch chan<- prometheus.Metric, clusterID string) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(volumeSnapshotListGVK)
	if err := c.reader.List(ctx, list); err != nil {
		return
	}

	type pvcKey struct{ namespace, pvc string }
	counts := map[pvcKey]float64{}
	for i := range list.Items {
		vs := &list.Items[i]
		// spec.source.persistentVolumeClaimName is set on a DYNAMIC snapshot only. A static
		// snapshot (spec.source.volumeSnapshotContentName) has no source claim — including the
		// re-bound one this operator creates in its own namespace to hand a mover a mountable
		// copy. Skipping them is not a gap: a static snapshot adds no snapshot to the source
		// volume's chain, so counting it would inflate the very number the flatten threshold
		// is measured against.
		pvc, found, err := unstructured.NestedString(vs.Object, "spec", "source", "persistentVolumeClaimName")
		if err != nil || !found || pvc == "" {
			continue
		}
		counts[pvcKey{namespace: vs.GetNamespace(), pvc: pvc}]++
	}
	for key, n := range counts {
		ch <- prometheus.MustNewConstMetric(pvcVolumeSnapshotCountDesc, prometheus.GaugeValue, n,
			key.namespace, key.pvc, clusterID)
	}
}
