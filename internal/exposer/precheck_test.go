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
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// The crucible's own snapshotter Secret coordinates (test/crucible/deploy/manifests/
// ceph-storage.yaml), spelled here so the unit tests exercise the exact shape the real cluster
// runs rather than an invented one.
const (
	testSnapshotterSecretNS   = "rook-ceph"
	testSnapshotterSecretName = "rook-csi-rbd-provisioner"
)

// vsClassWithParameters builds an unstructured VolumeSnapshotClass carrying `parameters`. Values
// are `any` so a test can plant a NON-STRING parameter — a shape the API server would reject but
// a hand-written or third-party-generated object can still present, and one whose handling
// (NOT_CHECKABLE, never "no Secret declared") is the point of one of the cases below.
func vsClassWithParameters(t *testing.T, name string, params map[string]any) *unstructured.Unstructured {
	t.Helper()
	vsc := newVolumeSnapshotClass(name, rbdProvisioner)
	if params == nil {
		return vsc
	}
	if err := unstructured.SetNestedMap(vsc.Object, params, "parameters"); err != nil {
		t.Fatalf("set parameters on VolumeSnapshotClass %s: %v", name, err)
	}
	return vsc
}

// newPrecheckTestClient builds a fake client holding zero or more Secrets. Precheck reads nothing
// else, so nothing else is seeded.
func newPrecheckTestClient(t *testing.T, secrets ...*corev1.Secret) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme(corev1): %v", err)
	}
	if err := storagev1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme(storagev1): %v", err)
	}
	objs := make([]client.Object, 0, len(secrets))
	for _, s := range secrets {
		objs = append(objs, s)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

func newSecret(namespace, name string) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}}
}

// TestSnapshotterSecretRef is the pure table: every shape a VolumeSnapshotClass can name (or fail
// to name) its snapshotter credentials in, and what the extractor makes of it. The two templating
// shapes and the absent-parameter case are the ones that decide whether the pre-check may rule at
// all, so they are the reason this function is separated from Precheck and tested without a
// client at all.
func TestSnapshotterSecretRef(t *testing.T) {
	cases := []struct {
		name          string
		params        map[string]any
		wantNamespace string
		wantName      string
		wantTemplated bool
	}{
		{
			name: "literal reference — the crucible's ceph-block class",
			params: map[string]any{
				"clusterID":                     "rook-ceph",
				snapshotterSecretNamespaceParam: testSnapshotterSecretNS,
				snapshotterSecretNameParam:      testSnapshotterSecretName,
			},
			wantNamespace: testSnapshotterSecretNS,
			wantName:      testSnapshotterSecretName,
		},
		{
			// The per-VolumeSnapshotContent template: the sidecar substitutes the content's name,
			// so the Secret this class points at does not exist until the snapshot does.
			name: "templated name — ${volumesnapshotcontent.name}",
			params: map[string]any{
				snapshotterSecretNamespaceParam: testSnapshotterSecretNS,
				snapshotterSecretNameParam:      "${volumesnapshotcontent.name}",
			},
			wantNamespace: testSnapshotterSecretNS,
			wantName:      "${volumesnapshotcontent.name}",
			wantTemplated: true,
		},
		{
			// The per-tenant template: credentials live in whichever namespace the VolumeSnapshot
			// was created in, which is not knowable from the class.
			name: "templated namespace — ${volumesnapshot.namespace}",
			params: map[string]any{
				snapshotterSecretNamespaceParam: "${volumesnapshot.namespace}",
				snapshotterSecretNameParam:      "tenant-csi-creds",
			},
			wantNamespace: "${volumesnapshot.namespace}",
			wantName:      "tenant-csi-creds",
			wantTemplated: true,
		},
		{
			name:   "no parameters at all — the class needs no credentials",
			params: nil,
		},
		{
			name:   "parameters present but naming no snapshotter Secret",
			params: map[string]any{"clusterID": "rook-ceph", "csi.storage.k8s.io/fstype": "ext4"},
		},
		{
			// Blank is not a reference to the object named "". Trimming keeps a whitespace-only
			// parameter from being looked up as a real Secret key and reported missing.
			name: "blank values read as absent, not as a reference to \"\"",
			params: map[string]any{
				snapshotterSecretNamespaceParam: "   ",
				snapshotterSecretNameParam:      "",
			},
		},
		{
			name: "only the name half is set",
			params: map[string]any{
				snapshotterSecretNameParam: testSnapshotterSecretName,
			},
			wantName: testSnapshotterSecretName,
		},
		{
			name: "only the namespace half is set",
			params: map[string]any{
				snapshotterSecretNamespaceParam: testSnapshotterSecretNS,
			},
			wantNamespace: testSnapshotterSecretNS,
		},
		{
			// A parameter that is not a string must NOT degrade to "no Secret declared" — that is
			// an absence reading as health, which is the failure mode this whole file guards.
			name: "non-string parameter value is not statically knowable",
			params: map[string]any{
				snapshotterSecretNamespaceParam: testSnapshotterSecretNS,
				snapshotterSecretNameParam:      int64(42),
			},
			wantNamespace: testSnapshotterSecretNS,
			wantName:      "42",
			wantTemplated: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			namespace, name, templated := snapshotterSecretRef(vsClassWithParameters(t, "probe", tc.params))
			if namespace != tc.wantNamespace {
				t.Errorf("namespace = %q, want %q", namespace, tc.wantNamespace)
			}
			if name != tc.wantName {
				t.Errorf("name = %q, want %q", name, tc.wantName)
			}
			if templated != tc.wantTemplated {
				t.Errorf("templated = %v, want %v", templated, tc.wantTemplated)
			}
		})
	}
}

// TestSnapshotterSecretRefNilClass pins the nil case: no panic, and no reference.
func TestSnapshotterSecretRefNilClass(t *testing.T) {
	namespace, name, templated := snapshotterSecretRef(nil)
	if namespace != "" || name != "" || templated {
		t.Errorf("snapshotterSecretRef(nil) = (%q, %q, %v), want (\"\", \"\", false)", namespace, name, templated)
	}
}

// TestPrecheckSecretPresentPasses: the healthy cluster. The named Secret exists, the single check
// is OK, and Err() is nil.
func TestPrecheckSecretPresentPasses(t *testing.T) {
	c := newPrecheckTestClient(t, newSecret(testSnapshotterSecretNS, testSnapshotterSecretName))
	vsc := vsClassWithParameters(t, "ceph-block", map[string]any{
		snapshotterSecretNamespaceParam: testSnapshotterSecretNS,
		snapshotterSecretNameParam:      testSnapshotterSecretName,
	})

	res := Precheck(context.Background(), c, vsc)
	if !res.OK {
		t.Fatalf("Precheck.OK = false, want true (message: %s)", res.Message)
	}
	if err := res.Err(); err != nil {
		t.Errorf("Precheck.Err() = %v, want nil", err)
	}
	if len(res.Checks) != 1 || res.Checks[0].Status != CheckOK {
		t.Fatalf("Checks = %+v, want exactly one OK check", res.Checks)
	}
	if res.Checks[0].Name != checkSnapshotterSecret {
		t.Errorf("check name = %q, want %q", res.Checks[0].Name, checkSnapshotterSecret)
	}
}

// TestPrecheckSecretMissingFails is the check's whole reason for existing: the class names a
// Secret nobody created, so no VolumeSnapshot on it could ever be served. The verdict must be
// FAILED, must carry the sentinel for the controller's errors.Is, and must NAME THE SECRET — the
// message goes verbatim into the Warning Event an operator reads.
func TestPrecheckSecretMissingFails(t *testing.T) {
	c := newPrecheckTestClient(t) // no Secrets at all
	vsc := vsClassWithParameters(t, "ceph-block", map[string]any{
		snapshotterSecretNamespaceParam: testSnapshotterSecretNS,
		snapshotterSecretNameParam:      testSnapshotterSecretName,
	})

	res := Precheck(context.Background(), c, vsc)
	if res.OK {
		t.Fatalf("Precheck.OK = true, want false (checks: %+v)", res.Checks)
	}
	if res.Reason != reasonSnapshotterSecretMissing {
		t.Errorf("Reason = %q, want %q", res.Reason, reasonSnapshotterSecretMissing)
	}
	if len(res.Checks) != 1 || res.Checks[0].Status != CheckFailed {
		t.Fatalf("Checks = %+v, want exactly one FAILED check", res.Checks)
	}

	err := res.Err()
	if !errors.Is(err, ErrPrecheckFailed) {
		t.Fatalf("Err() = %v, want errors.Is(err, ErrPrecheckFailed)", err)
	}
	var pe *PrecheckError
	if !errors.As(err, &pe) {
		t.Fatalf("Err() = %v, want errors.As(&PrecheckError{}) so the caller can reach the Checks", err)
	}
	if pe.Result.Reason != reasonSnapshotterSecretMissing {
		t.Errorf("PrecheckError.Result.Reason = %q, want %q", pe.Result.Reason, reasonSnapshotterSecretMissing)
	}
	for _, want := range []string{testSnapshotterSecretNS, testSnapshotterSecretName, "ceph-block"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q — an operator cannot act on it", err.Error(), want)
		}
	}
}

// TestPrecheckTemplatedSecretIsNotCheckable pins the discipline this file is built around: a
// templated reference is neither a pass nor a failure. It must not fail (the cluster may be
// perfectly healthy), and it must not silently report a clean bill of health either — the check is
// recorded NOT_CHECKABLE with the template quoted back.
func TestPrecheckTemplatedSecretIsNotCheckable(t *testing.T) {
	for _, tc := range []struct {
		name   string
		params map[string]any
	}{
		{"templated name", map[string]any{
			snapshotterSecretNamespaceParam: testSnapshotterSecretNS,
			snapshotterSecretNameParam:      "${volumesnapshotcontent.name}",
		}},
		{"templated namespace", map[string]any{
			snapshotterSecretNamespaceParam: "${volumesnapshot.namespace}",
			snapshotterSecretNameParam:      "tenant-csi-creds",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// No Secrets seeded: if the implementation ever resolved a template to a literal name
			// it would come back FAILED here, not OK, so this client makes the wrong answer loud.
			c := newPrecheckTestClient(t)
			res := Precheck(context.Background(), c, vsClassWithParameters(t, "ceph-block", tc.params))

			if !res.OK {
				t.Fatalf("Precheck.OK = false, want true — a templated Secret must never FAIL a backup (%s)", res.Message)
			}
			if len(res.Checks) != 1 || res.Checks[0].Status != CheckNotCheckable {
				t.Fatalf("Checks = %+v, want exactly one NOT_CHECKABLE check (never OK: nothing was verified)", res.Checks)
			}
			if !strings.Contains(res.Checks[0].Detail, "${") {
				t.Errorf("detail %q does not quote the template back", res.Checks[0].Detail)
			}
		})
	}
}

// TestPrecheckNoSecretDeclaredPasses: a class that names no snapshotter Secret is a positive
// statement, not an absence — there is nothing to verify and nothing missing, so it is OK (not
// NOT_CHECKABLE).
func TestPrecheckNoSecretDeclaredPasses(t *testing.T) {
	c := newPrecheckTestClient(t)
	res := Precheck(context.Background(), c, vsClassWithParameters(t, "longhorn", nil))

	if !res.OK {
		t.Fatalf("Precheck.OK = false, want true (%s)", res.Message)
	}
	if len(res.Checks) != 1 || res.Checks[0].Status != CheckOK {
		t.Fatalf("Checks = %+v, want exactly one OK check", res.Checks)
	}
}

// TestPrecheckHalfSpecifiedSecretIsNotCheckable: only one half of the reference is set. Tempting
// to call broken; refused anyway, because this check FAILS backups and has no false-positive
// budget for a shape we have never observed in the field.
func TestPrecheckHalfSpecifiedSecretIsNotCheckable(t *testing.T) {
	c := newPrecheckTestClient(t)
	res := Precheck(context.Background(), c, vsClassWithParameters(t, "half", map[string]any{
		snapshotterSecretNameParam: testSnapshotterSecretName,
	}))

	if !res.OK {
		t.Fatalf("Precheck.OK = false, want true (%s)", res.Message)
	}
	if len(res.Checks) != 1 || res.Checks[0].Status != CheckNotCheckable {
		t.Fatalf("Checks = %+v, want exactly one NOT_CHECKABLE check", res.Checks)
	}
}

// TestPrecheckNilClassIsNotCheckable: unreachable through Registry.For, but the answer must still
// be the one that cannot be mistaken for health.
func TestPrecheckNilClassIsNotCheckable(t *testing.T) {
	res := Precheck(context.Background(), newPrecheckTestClient(t), nil)
	if !res.OK {
		t.Fatalf("Precheck.OK = false, want true (%s)", res.Message)
	}
	if len(res.Checks) != 1 || res.Checks[0].Status != CheckNotCheckable {
		t.Fatalf("Checks = %+v, want exactly one NOT_CHECKABLE check", res.Checks)
	}
}

// TestResolvedExposerPrechecksItsOwnClass wires the two halves together: an exposer resolved by
// the REAL Registry.For carries the class it resolved, and its Precheck reads that class's
// parameters. Without this, precheck.go could be perfect and still never consulted for the class
// the volume will actually be snapshotted on.
func TestResolvedExposerPrechecksItsOwnClass(t *testing.T) {
	ctx := context.Background()
	vsc := vsClassWithParameters(t, "ceph-block-snapclass", map[string]any{
		snapshotterSecretNamespaceParam: testSnapshotterSecretNS,
		snapshotterSecretNameParam:      testSnapshotterSecretName,
	})
	c := newRegistryTestClient(t,
		[]*storagev1.StorageClass{newStorageClass("ceph-block", rbdProvisioner)},
		[]*unstructured.Unstructured{vsc},
	)
	pvc := pvcOnStorageClass("c-db", "data", "ceph-block")

	ex, err := NewRegistry(c, testOperatorNamespace).For(ctx, pvc)
	if err != nil {
		t.Fatalf("For: unexpected error: %v", err)
	}

	err = ex.Precheck(ctx)
	if !errors.Is(err, ErrPrecheckFailed) {
		t.Fatalf("Precheck with no snapshotter Secret in the cluster = %v, want ErrPrecheckFailed", err)
	}

	// And it passes once the Secret the class names exists.
	if err := c.Create(ctx, newSecret(testSnapshotterSecretNS, testSnapshotterSecretName)); err != nil {
		t.Fatalf("create snapshotter Secret: %v", err)
	}
	if err := ex.Precheck(ctx); err != nil {
		t.Fatalf("Precheck with the Secret present = %v, want nil", err)
	}
}
