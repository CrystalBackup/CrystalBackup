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

package manifests

import "testing"

func compileCluster(t *testing.T, opts ClusterCaptureOptions) *ClusterSelector {
	t.Helper()
	s, err := CompileClusterSelector(opts)
	if err != nil {
		t.Fatalf("CompileClusterSelector() = %v", err)
	}
	return s
}

func TestClusterDefaultAllowList(t *testing.T) {
	s := compileCluster(t, ClusterCaptureOptions{})

	// Every kind adr/0011 §1 names. A restored namespace can need each of these and cannot
	// create any of them itself.
	for _, c := range []struct{ group, kind string }{
		{"apiextensions.k8s.io", "CustomResourceDefinition"},
		{"storage.k8s.io", "StorageClass"},
		{"snapshot.storage.k8s.io", "VolumeSnapshotClass"},
		{"networking.k8s.io", "IngressClass"},
		{"scheduling.k8s.io", "PriorityClass"},
		{"node.k8s.io", "RuntimeClass"},
		{"rbac.authorization.k8s.io", "ClusterRole"},
		{"rbac.authorization.k8s.io", "ClusterRoleBinding"},
		{"", "PersistentVolume"},
	} {
		if !s.SelectsObject(c.group, c.kind, "app-owned") {
			t.Errorf("%s/%s: not selected by the default allow-list", c.group, c.kind)
		}
	}

	// The list is an ALLOW-list, not a denylist with holes: a kind nobody curated stays out,
	// or the snapshot fills with every add-on's cluster-scoped objects.
	for _, c := range []struct{ group, kind string }{
		{"", "Namespace"},
		{"", "Node"},
		{"admissionregistration.k8s.io", "ValidatingWebhookConfiguration"},
		{"apiregistration.k8s.io", "APIService"},
		{"cert-manager.io", "ClusterIssuer"},
	} {
		if s.SelectsObject(c.group, c.kind, "anything") {
			t.Errorf("%s/%s: selected, want excluded from the curated default", c.group, c.kind)
		}
	}
}

func TestClusterExcludesControlPlaneRBAC(t *testing.T) {
	s := compileCluster(t, ClusterCaptureOptions{})

	// Restoring these means fighting the API server, which reconciles its own ClusterRoles on
	// every start — and a drifted copy could briefly grant what the target's policy did not.
	for _, name := range []string{
		"system:controller:generic-garbage-collector",
		"system:node",
		"cluster-admin",
		"edit",
		"kubeadm:get-nodes",
	} {
		if s.SelectsObject("rbac.authorization.k8s.io", "ClusterRole", name) {
			t.Errorf("ClusterRole %q: selected, want excluded as control-plane owned", name)
		}
	}

	// A tenant's own ClusterRole is exactly what a DR needs to bring back.
	if !s.SelectsObject("rbac.authorization.k8s.io", "ClusterRole", "app-operator") {
		t.Error("ClusterRole app-operator: not selected; a workload's own RBAC is the point of a DR")
	}
}

func TestClusterSystemPrefixIsRBACOnly(t *testing.T) {
	// The system: convention belongs to RBAC. A StorageClass a user chose to call "system:ssd"
	// is their object, and silently dropping it from their DR would be a surprise nobody could
	// diagnose from the snapshot.
	s := compileCluster(t, ClusterCaptureOptions{})
	if !s.SelectsObject("storage.k8s.io", "StorageClass", "system:ssd") {
		t.Error("StorageClass system:ssd: excluded; the system: convention is RBAC's, not a global rule")
	}
}

func TestClusterExplicitIncludeReplacesTheDefault(t *testing.T) {
	// An admin who names an include is being specific on purpose; widening that back out with
	// the curated default would capture more than they asked for.
	s := compileCluster(t, ClusterCaptureOptions{
		Include: []string{"storage.k8s.io/StorageClass"},
	})

	if !s.SelectsObject("storage.k8s.io", "StorageClass", "fast") {
		t.Error("StorageClass fast: not selected by its own include")
	}
	if s.SelectsObject("scheduling.k8s.io", "PriorityClass", "high") {
		t.Error("PriorityClass high: selected; an explicit include must replace the default, not extend it")
	}
}

func TestClusterIncludeCannotResurrectControlPlaneRBAC(t *testing.T) {
	// The default name exclusions run AFTER include and are not overridable. A run that needs
	// system:controller:… back is not a DR scenario.
	s := compileCluster(t, ClusterCaptureOptions{
		Include: []string{"rbac.authorization.k8s.io/ClusterRole/*"},
	})
	if s.SelectsObject("rbac.authorization.k8s.io", "ClusterRole", "system:node") {
		t.Error("system:node: selected via an explicit include; the control-plane exclusion is not overridable")
	}
	if !s.SelectsObject("rbac.authorization.k8s.io", "ClusterRole", "app-operator") {
		t.Error("app-operator: not selected; the exclusion must be narrow, not a blanket kind ban")
	}
}

func TestClusterExcludeAppliesAfterInclude(t *testing.T) {
	s := compileCluster(t, ClusterCaptureOptions{
		Exclude: []string{"storage.k8s.io/StorageClass/legacy-*"},
	})
	if s.SelectsObject("storage.k8s.io", "StorageClass", "legacy-nfs") {
		t.Error("legacy-nfs: selected, want removed by exclude")
	}
	if !s.SelectsObject("storage.k8s.io", "StorageClass", "fast") {
		t.Error("fast: excluded, want kept")
	}
}

func TestClusterSelectsKindSkipsWholeResources(t *testing.T) {
	// SelectsKind exists so the dump can skip a List entirely rather than enumerate a kind and
	// discard every object — on a large cluster that is the difference between one API call and
	// paging through thousands of objects nobody wants.
	s := compileCluster(t, ClusterCaptureOptions{})
	if s.SelectsKind("", "Node") {
		t.Error("SelectsKind(Node) = true; an unlisted kind must be skipped before it is listed")
	}
	if !s.SelectsKind("storage.k8s.io", "StorageClass") {
		t.Error("SelectsKind(StorageClass) = false")
	}

	// A name-qualified include still has to admit its kind, or the object it names is never
	// reached.
	named := compileCluster(t, ClusterCaptureOptions{
		Include: []string{"storage.k8s.io/StorageClass/fast"},
	})
	if !named.SelectsKind("storage.k8s.io", "StorageClass") {
		t.Error("a name-qualified include must still select its kind at the List level")
	}
	if named.SelectsObject("storage.k8s.io", "StorageClass", "slow") {
		t.Error("slow: selected; the name narrowing must still apply per object")
	}
}
