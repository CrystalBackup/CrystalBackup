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

package exposer

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// deriveExposure rebuilds the Exposure for (originNamespace, namePrefix) WITHOUT any API call —
// every name is deterministic from the prefix (see Exposure's doc), so teardown never needs the
// source PVC, the Registry, or an Expose round-trip to know what to delete.
//
// Kind, StorageClass and Capacity are left zero on purpose: they parameterise CREATION (which
// temp-PVC access mode, how big), and this value exists only to be torn down — cleanup() is
// shared by both exposers ("Cleanup identical", ADR 0003) and reads none of the three.
func deriveExposure(originNamespace, operatorNamespace, namePrefix string, labels map[string]string) *Exposure {
	return &Exposure{
		OriginNamespace:   originNamespace,
		OperatorNamespace: operatorNamespace,
		OriginVSName:      volumeSnapshotName(namePrefix),
		StaticVSCName:     staticVSCName(namePrefix),
		StaticVSName:      staticVSName(namePrefix),
		TempPVCName:       tempPVCName(namePrefix),
		ExposedPVCName:    tempPVCName(namePrefix),
		Labels:            labels,
	}
}

// TeardownExposure removes every object an exposure with this identity may have left — temp PVC,
// static VS/VSC pair, origin VS, and the Retain-parked origin VSC — in cleanup()'s exact ADR
// step-5 order, idempotently and NotFound-tolerantly.
//
// This is the ONLY entry point a cleanup path should use, and its two properties are the point:
//
//   - It never READS the source PVC, so a PVC (or whole tenant namespace) deleted after the
//     exposure was created cannot make teardown conclude "nothing to clean" while the
//     cluster-scoped VSC lingers.
//   - It never CREATES: the previous shape (reconstruct via Expose, then Cleanup) could re-create
//     the origin VolumeSnapshot mid-teardown — a fresh, unbound origin VS makes reclaimOrigin
//     skip the Retain→Delete restore AND hides reclaimOrphanOriginVSC behind its NotFound guard,
//     turning the cleanup pass itself into a source of the very leak it exists to prevent.
//
// Callers that must retry until success (the terminal re-entry sweep, the orphan reaper) depend
// on both properties: a teardown that can create is not safe to re-run, and one that needs the
// PVC is not safe to run late.
func TeardownExposure(ctx context.Context, c client.Client, originNamespace, operatorNamespace, namePrefix string, labels map[string]string) error {
	return cleanup(ctx, c, deriveExposure(originNamespace, operatorNamespace, namePrefix, labels))
}

// TeardownExposure is the Registry-bound form of the package function: the Registry already
// knows the operator namespace, so callers holding a *Registry (the Backup controller's
// ExposerRegistry seam) supply only the exposure's identity.
func (r *Registry) TeardownExposure(ctx context.Context, originNamespace, namePrefix string, labels map[string]string) error {
	return TeardownExposure(ctx, r.client, originNamespace, r.operatorNamespace, namePrefix, labels)
}
