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

// Package admission proves the CHART-SHIPPED ValidatingAdmissionPolicy set (adr/0010,
// spec/02-api.md §Validation) against a real API server: the policies are rendered with
// `helm template` — the exact objects an install applies, never a test-local copy that
// could drift — installed into envtest, and exercised with the 08-testing §3 fixture
// matrix. The suite needs the `helm` binary; it fails with a clear message when absent
// (CI runners ship helm; a laptop without it should install it rather than skip the gate).
package admission

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	clientscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"

	corev1 "k8s.io/api/core/v1"
)

const (
	operatorNamespace = "crystal-backup-system"
	// operatorSAUser is the chart's rendered operator ServiceAccount username
	// ("crystal-backup.serviceAccountName" = "<fullname>-operator") — the identity the
	// policies' matchConditions exempt.
	operatorSAUser = "system:serviceaccount:crystal-backup-system:crystal-backup-operator"
	pollInterval   = 200 * time.Millisecond
	pollTimeout    = 30 * time.Second
)

// renderChartAdmissionObjects helm-templates the chart and returns every admission-relevant
// object (the VAPs, their bindings, and the denied-namespaces param ConfigMap).
func renderChartAdmissionObjects(t *testing.T) []*unstructured.Unstructured {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Fatalf("the helm binary is required to render the chart's admission policies: %v", err)
	}
	chart := filepath.Join("..", "..", "charts", "crystal-backup")
	cmd := exec.Command("helm", "template", "vaptest", chart,
		"--namespace", operatorNamespace,
		"--show-only", "templates/admission.yaml")
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("helm template: %v\n%s", err, stderr.String())
	}

	var objs []*unstructured.Unstructured
	decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(out.Bytes()), 4096)
	for {
		var raw map[string]interface{}
		err := decoder.Decode(&raw)
		if err != nil {
			break
		}
		if len(raw) == 0 {
			continue
		}
		objs = append(objs, &unstructured.Unstructured{Object: raw})
	}
	if len(objs) == 0 {
		t.Fatal("helm template produced no admission objects")
	}
	return objs
}

// startEnv boots envtest with the CRDs and returns an admin client plus a client
// impersonating the operator ServiceAccount (granted cluster-admin so RBAC never masks an
// admission verdict).
func startEnv(t *testing.T) (client.Client, client.Client) {
	t.Helper()
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest (run via `make test`): %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })

	sc := runtime.NewScheme()
	if err := clientscheme.AddToScheme(sc); err != nil {
		t.Fatal(err)
	}
	if err := cbv1.AddToScheme(sc); err != nil {
		t.Fatal(err)
	}
	admin, err := client.New(cfg, client.Options{Scheme: sc})
	if err != nil {
		t.Fatal(err)
	}

	saCfg := rest.CopyConfig(cfg)
	saCfg.Impersonate = rest.ImpersonationConfig{UserName: operatorSAUser}
	saClient, err := client.New(saCfg, client.Options{Scheme: sc})
	if err != nil {
		t.Fatal(err)
	}
	// Grant the impersonated SA cluster-admin: this suite tests ADMISSION, and an RBAC 403
	// would mask the verdict under test.
	crb := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "vaptest-operator-sa-admin"},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.UserKind, Name: operatorSAUser, APIGroup: rbacv1.GroupName}},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "cluster-admin"},
	}
	if err := admin.Create(context.Background(), crb); err != nil {
		t.Fatal(err)
	}
	return admin, saClient
}

// isPolicyDenial reports whether err is a VAP denial (never an RBAC or schema error).
func isPolicyDenial(err error) bool {
	if err == nil {
		return false
	}
	return apierrors.IsInvalid(err) || apierrors.IsForbidden(err)
}

// eventuallyDenied asserts the API server ends up denying create(obj) via admission —
// Eventually, because freshly-installed policies take a moment to compile. The object is
// deleted if a create unexpectedly lands while the policy warms up.
func eventuallyDenied(t *testing.T, c client.Client, build func() client.Object, wantSubstring string) {
	t.Helper()
	deadline := time.Now().Add(pollTimeout)
	for {
		obj := build()
		err := c.Create(context.Background(), obj)
		if isPolicyDenial(err) {
			if wantSubstring != "" && !strings.Contains(err.Error(), wantSubstring) {
				t.Fatalf("denied for the wrong reason: %v (want substring %q)", err, wantSubstring)
			}
			return
		}
		if err == nil {
			_ = c.Delete(context.Background(), obj) // policy not compiled yet; clean and retry.
		}
		if time.Now().After(deadline) {
			t.Fatalf("create was never denied (last err: %v)", err)
		}
		time.Sleep(pollInterval)
	}
}

// mustAdmit asserts create(obj) is admitted, and cleans it up.
func mustAdmit(t *testing.T, c client.Client, obj client.Object, note string) {
	t.Helper()
	if err := c.Create(context.Background(), obj); err != nil {
		t.Fatalf("%s: unexpectedly denied/failed: %v", note, err)
	}
	t.Cleanup(func() { _ = c.Delete(context.Background(), obj) })
}

func TestChartValidatingAdmissionPolicies(t *testing.T) {
	admin, saClient := startEnv(t)
	ctx := context.Background()

	for _, ns := range []string{operatorNamespace, "tenant-a", "kube-vaptest"} {
		if err := admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
			t.Fatal(err)
		}
	}
	for _, obj := range renderChartAdmissionObjects(t) {
		if err := admin.Create(ctx, obj); err != nil {
			t.Fatalf("install %s %s: %v", obj.GetKind(), obj.GetName(), err)
		}
	}

	restore := func(mode cbv1.RestoreMode, confirmation string) client.Object {
		return &cbv1.Restore{
			ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-a", Name: fmt.Sprintf("r-%d", time.Now().UnixNano())},
			Spec: cbv1.RestoreSpec{
				Source:       cbv1.RestoreSource{Backup: "run-1"},
				Mode:         mode,
				Confirmation: confirmation,
			},
		}
	}

	t.Run("rule 1: R23 confirmation on Restore", func(t *testing.T) {
		eventuallyDenied(t, admin, func() client.Object { return restore(cbv1.RestoreModeRecreate, "wrong-ns") },
			"confirmation")
		mustAdmit(t, admin, restore(cbv1.RestoreModeOverwrite, ""), "empty confirmation parks, never denies")
		mustAdmit(t, admin, restore(cbv1.RestoreModeRecreate, "tenant-a"), "matching confirmation")
	})

	t.Run("rule 1: R23 confirmation on ClusterRestore", func(t *testing.T) {
		build := func(confirmation string) client.Object {
			return &cbv1.ClusterRestore{
				ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("cr-%d", time.Now().UnixNano())},
				Spec: cbv1.ClusterRestoreSpec{
					Source: cbv1.ClusterRestoreSource{
						LocationRef: cbv1.LocalObjectReference{Name: "dr"}, Namespace: "gone", Backup: "run-1",
					},
					Target:       cbv1.ClusterRestoreTarget{Namespace: "restored", CreateNamespace: true},
					Mode:         cbv1.RestoreModeRecreate,
					Confirmation: confirmation,
				},
			}
		}
		eventuallyDenied(t, admin, func() client.Object { return build("gone") }, "target.namespace")
		mustAdmit(t, admin, build("restored"), "confirmation == target namespace")
	})

	t.Run("rule 1: R23 confirmation on ClusterErasure (identity forms)", func(t *testing.T) {
		build := func(target cbv1.ErasureTarget, confirmation string) client.Object {
			return &cbv1.ClusterErasure{
				ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("er-%d", time.Now().UnixNano())},
				Spec: cbv1.ClusterErasureSpec{
					LocationRef:  cbv1.LocalObjectReference{Name: "dr"},
					Target:       target,
					Confirmation: confirmation,
				},
			}
		}
		eventuallyDenied(t, admin,
			func() client.Object { return build(cbv1.ErasureTarget{Tenant: "team-x"}, "not-team-x") }, "identity")
		mustAdmit(t, admin, build(cbv1.ErasureTarget{Tenant: "team-x"}, "team-x"), "tenant identity")
		mustAdmit(t, admin, build(cbv1.ErasureTarget{Namespace: "c-a"}, "c-a"), "namespace identity")
		mustAdmit(t, admin, build(cbv1.ErasureTarget{Namespace: "c-a", PVC: "data"}, "c-a/data"), "pvc identity")
	})

	t.Run("rule 2: user isolation with operator-SA exemption", func(t *testing.T) {
		build := func() client.Object {
			return &cbv1.Backup{
				ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-a", Name: fmt.Sprintf("b-%d", time.Now().UnixNano())},
				Spec: cbv1.BackupSpec{
					LocationRef: cbv1.LocationReference{Kind: "ClusterBackupLocation", Name: "dr"},
				},
			}
		}
		eventuallyDenied(t, admin, build, "ClusterBackupLocation")
		mustAdmit(t, saClient, build(), "the operator SA's fan-out Backups are exempt")
		mustAdmit(t, admin, &cbv1.Backup{
			ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-a", Name: "b-user-ok"},
			Spec:       cbv1.BackupSpec{LocationRef: cbv1.LocationReference{Kind: "BackupLocation", Name: "mine"}},
		}, "a namespaced BackupLocation reference")
	})

	t.Run("rule 6: Immutable forbids pruneSchedule", func(t *testing.T) {
		build := func(mode cbv1.LocationMode, prune string) client.Object {
			loc := &cbv1.ClusterBackupLocation{
				ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("loc-%d", time.Now().UnixNano())},
				Spec: cbv1.ClusterBackupLocationSpec{
					Mode:      mode,
					ClusterID: "c",
					S3: cbv1.S3Spec{Endpoint: "https://s3", Bucket: "b",
						CredentialsSecretRef: cbv1.LocalObjectReference{Name: "s3"}},
					Encryption: cbv1.ClusterEncryptionSpec{ClusterKEKSecretRef: cbv1.LocalObjectReference{Name: "kek"}},
				},
			}
			if prune != "" {
				loc.Spec.Maintenance = &cbv1.MaintenanceSpec{PruneSchedule: prune}
			}
			return loc
		}
		eventuallyDenied(t, admin,
			func() client.Object { return build(cbv1.LocationModeImmutable, "0 3 * * *") }, "Immutable")
		mustAdmit(t, admin, build(cbv1.LocationModeStandard, "0 3 * * *"), "Standard mode prunes freely")
		mustAdmit(t, admin, build(cbv1.LocationModeImmutable, ""), "Immutable without a prune schedule")
	})

	t.Run("rule 7: denied namespaces via the param ConfigMap", func(t *testing.T) {
		build := func(ns string) client.Object {
			return &cbv1.BackupSchedule{
				ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: fmt.Sprintf("s-%d", time.Now().UnixNano())},
				Spec: cbv1.BackupScheduleSpec{
					LocationRef: cbv1.LocalObjectReference{Name: "mine"},
					Schedule:    "0 1 * * *",
				},
			}
		}
		eventuallyDenied(t, admin, func() client.Object { return build("kube-vaptest") }, "denied")
		mustAdmit(t, admin, build("tenant-a"), "an ordinary tenant namespace")
	})

	t.Run("rule 8: namespaces selector must set exactly one positive form", func(t *testing.T) {
		build := func(sel cbv1.NamespaceSelector) client.Object {
			return &cbv1.ClusterBackup{
				ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("cb-%d", time.Now().UnixNano())},
				Spec: cbv1.ClusterBackupSpec{
					ClusterBackupRunSpec: cbv1.ClusterBackupRunSpec{
						LocationRef: cbv1.LocalObjectReference{Name: "dr"},
						Namespaces:  sel,
					},
				},
			}
		}
		// The typed client omits empty list fields, so the present-but-EMPTY shapes (the
		// non-emptiness half of rule 8, matching internal/nsselector's len>0 counting) are
		// built raw: `matchNames: []` must count as "not set".
		raw := func(namespaces map[string]any) *unstructured.Unstructured {
			u := &unstructured.Unstructured{}
			u.SetAPIVersion("crystalbackup.io/v1alpha1")
			u.SetKind("ClusterBackup")
			u.SetName(fmt.Sprintf("cbr-%d", time.Now().UnixNano()))
			spec := map[string]any{"locationRef": map[string]any{"name": "dr"}}
			if namespaces != nil {
				spec["namespaces"] = namespaces
			}
			_ = unstructured.SetNestedMap(u.Object, spec, "spec")
			return u
		}
		eventuallyDenied(t, admin, func() client.Object { return build(cbv1.NamespaceSelector{}) }, "positive")
		eventuallyDenied(t, admin, func() client.Object {
			return build(cbv1.NamespaceSelector{Regexp: "^c-.*$", MatchNames: []string{"c-a"}})
		}, "positive")
		mustAdmit(t, admin, build(cbv1.NamespaceSelector{Regexp: "^c-.*$", Exclude: []string{"kube-*"}}),
			"one positive form plus exclude")
		// Non-emptiness counting: an empty list is NOT a set form — alone it is denied,
		// alongside a real form it must not trip the exactly-one rule.
		eventuallyDenied(t, admin, func() client.Object {
			return raw(map[string]any{"matchNames": []any{}})
		}, "positive")
		mustAdmit(t, admin, raw(map[string]any{
			"matchNames": []any{}, "matchLabels": map[string]any{"team": "a"},
		}), "an empty matchNames beside one real form")
		// An entirely absent spec.namespaces is a clean rule-8 denial (message, not a CEL
		// evaluation error).
		eventuallyDenied(t, admin, func() client.Object { return raw(nil) }, "positive")
		// The operator's own ServiceAccount is exempt (matchConditions): the engine
		// re-validates at execution, and a pre-policy schedule's stamped-out ClusterBackups
		// must not wedge after an upgrade tightens the rule.
		mustAdmit(t, saClient, build(cbv1.NamespaceSelector{}), "operator SA bypasses rule 8")
	})

	t.Run("rule 9: external sync must copy between distinct locations", func(t *testing.T) {
		build := func(src, dst string) client.Object {
			return &cbv1.BackupExternalSync{
				ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-a", Name: fmt.Sprintf("x-%d", time.Now().UnixNano())},
				Spec: cbv1.BackupExternalSyncSpec{
					SourceLocationRef:      cbv1.LocalObjectReference{Name: src},
					DestinationLocationRef: cbv1.LocalObjectReference{Name: dst},
				},
			}
		}
		eventuallyDenied(t, admin, func() client.Object { return build("same", "same") }, "differ")
		mustAdmit(t, admin, build("primary", "secondary"), "distinct locations")
	})
}
