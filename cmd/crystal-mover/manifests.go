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

package main

import (
	"context"
	"fmt"
	"os"

	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"

	"github.com/CrystalBackup/CrystalBackup/internal/manifests"
	"github.com/CrystalBackup/CrystalBackup/internal/mover"
	"github.com/CrystalBackup/CrystalBackup/internal/sanitize"
)

// dumpManifests reads the namespace from the API server and writes the sanitized tree that
// restic is about to back up.
//
// This is the one place the mover touches Kubernetes. It exists here, inside the mover,
// rather than in the operator because the operator never handles backup data bytes
// (01-architecture.md §1) and the dump must not travel through exec/stdout (delta 8) — so the
// only remaining shape is: the process that writes to the repository is the process that
// reads the objects.
//
// It runs BEFORE restic, and a failure here is fatal for the run: `restic backup` over an
// empty or half-written directory would produce a snapshot that looks successful and restores
// to nothing, which is the single worst outcome a backup tool has.
func dumpManifests(ctx context.Context) (*manifests.Index, error) {
	namespace := os.Getenv(mover.EnvManifestsNamespace)
	if namespace == "" {
		return nil, fmt.Errorf("%s is not set; the manifest mover cannot guess its namespace",
			mover.EnvManifestsNamespace)
	}

	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("in-cluster config (the manifest mover needs an automounted "+
			"ServiceAccount token — see spec/03-security-and-tenancy.md I6): %w", err)
	}
	disco, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("discovery client: %w", err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("dynamic client: %w", err)
	}
	s, err := sanitize.New()
	if err != nil {
		return nil, fmt.Errorf("sanitizer: %w", err)
	}

	d := &manifests.Dumper{Disco: disco, Dynamic: dyn, Sanitizer: s}
	// The destination is ManifestsRoot/<namespace>, which is byte-for-byte the path restic
	// will record (internal/restic.ManifestsIdentity). Deriving it here from the same two
	// pieces the identity is built from means the dump target and the snapshot path cannot
	// drift apart.
	dest := mover.ManifestsRoot + "/" + namespace

	idx, err := d.Dump(ctx, manifests.Options{
		Namespace:         namespace,
		ClusterID:         os.Getenv(mover.EnvManifestsClusterID),
		BackupName:        os.Getenv(mover.EnvManifestsBackupName),
		ExcludeSecretData: os.Getenv(mover.EnvManifestsExcludeSecretData) == "true",
	}, dest)
	if err != nil {
		return nil, fmt.Errorf("dump %s: %w", namespace, err)
	}

	// The pod log is where an operator looks first; the full detail is in index.json inside
	// the snapshot, which survives the pod.
	fmt.Fprintf(os.Stderr, "crystal-mover: captured %d resource(s) from %s into %s\n",
		idx.ResourceCount, namespace, dest)
	for _, w := range idx.Warnings {
		fmt.Fprintf(os.Stderr, "crystal-mover: WARNING %s/%s: %s\n", w.Group, w.Kind, w.Message)
	}
	return idx, nil
}
