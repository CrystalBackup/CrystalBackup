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

// Package v1alpha1 holds the operator's ONE dynamic admission webhook (adr/0010): the
// single-default-ClusterBackupLocation uniqueness check — the only blocking rule that needs
// cross-object state and therefore cannot be a ValidatingAdmissionPolicy. Everything else
// (confirmation, isolation, immutable-forbids-prune, denied namespaces, selector shape,
// sync distinctness) ships as VAP CEL in the Helm chart, and the retention-vs-Immutable
// advisory is controller-side.
//
// The webhook is deliberately NOT a safety boundary: its ValidatingWebhookConfiguration
// runs with failurePolicy: Ignore, scoped to this one CRD, so an unavailable operator never
// wedges API writes — a transient second default slipping through an outage is caught by
// the ClusterBackupLocation controller's MultipleDefaults reconcile condition, which flags
// the conflicted locations un-Ready until an admin picks one.
package v1alpha1

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	crystalbackupiov1alpha1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
)

// nolint:unused
// log is for logging in this package.
var clusterbackuplocationlog = logf.Log.WithName("clusterbackuplocation-resource")

// SetupClusterBackupLocationWebhookWithManager registers the webhook for ClusterBackupLocation in the manager.
func SetupClusterBackupLocationWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &crystalbackupiov1alpha1.ClusterBackupLocation{}).
		WithValidator(&ClusterBackupLocationCustomValidator{Client: mgr.GetClient()}).
		Complete()
}

// failurePolicy is ignore per adr/0010: an operator outage must never wedge writes; the
// MultipleDefaults reconcile condition is the backstop for anything that slips through.
// +kubebuilder:webhook:path=/validate-crystalbackup-io-v1alpha1-clusterbackuplocation,mutating=false,failurePolicy=ignore,sideEffects=None,groups=crystalbackup.io,resources=clusterbackuplocations,verbs=create;update,versions=v1alpha1,name=vclusterbackuplocation-v1alpha1.kb.io,admissionReviewVersions=v1

// ClusterBackupLocationCustomValidator enforces admission rule 4 (spec/02-api.md): at most
// ONE ClusterBackupLocation may set spec.default: true. Uniqueness is inherently a
// cross-object read, so this validator lists the existing locations through the manager's
// cached client — the one thing per-object VAP CEL cannot do.
//
// NOTE: The +kubebuilder:object:generate=false marker prevents controller-gen from generating DeepCopy methods,
// as this struct is used only for temporary operations and does not need to be deeply copied.
type ClusterBackupLocationCustomValidator struct {
	client.Client
}

// ValidateCreate rejects a second default location at create time.
func (v *ClusterBackupLocationCustomValidator) ValidateCreate(ctx context.Context, obj *crystalbackupiov1alpha1.ClusterBackupLocation) (admission.Warnings, error) {
	return v.validateSingleDefault(ctx, obj)
}

// ValidateUpdate rejects flipping spec.default to true while another default exists.
func (v *ClusterBackupLocationCustomValidator) ValidateUpdate(ctx context.Context, _, newObj *crystalbackupiov1alpha1.ClusterBackupLocation) (admission.Warnings, error) {
	return v.validateSingleDefault(ctx, newObj)
}

// ValidateDelete allows every delete — removing a location never violates uniqueness.
func (v *ClusterBackupLocationCustomValidator) ValidateDelete(_ context.Context, _ *crystalbackupiov1alpha1.ClusterBackupLocation) (admission.Warnings, error) {
	return nil, nil
}

// validateSingleDefault admits obj unless it claims default while a DIFFERENT location
// already does. A racing pair that both pass (cache lag — this check is a fast gate, not a
// transaction) is caught by the controller's MultipleDefaults condition.
func (v *ClusterBackupLocationCustomValidator) validateSingleDefault(ctx context.Context, obj *crystalbackupiov1alpha1.ClusterBackupLocation) (admission.Warnings, error) {
	if !obj.Spec.Default {
		return nil, nil
	}
	var locations crystalbackupiov1alpha1.ClusterBackupLocationList
	if err := v.List(ctx, &locations); err != nil {
		// Fail OPEN on a listing error, consistent with the Ignore failure policy: this rule
		// is uniqueness hygiene, not a safety boundary, and the reconcile condition backstops.
		clusterbackuplocationlog.Error(err, "Could not list ClusterBackupLocations; admitting without the single-default check",
			"name", obj.GetName())
		return nil, nil
	}
	for i := range locations.Items {
		other := &locations.Items[i]
		if other.Name != obj.Name && other.Spec.Default {
			return nil, errors.NewInvalid(
				schema.GroupKind{Group: crystalbackupiov1alpha1.GroupVersion.Group, Kind: "ClusterBackupLocation"},
				obj.Name,
				field.ErrorList{field.Invalid(
					field.NewPath("spec", "default"), true,
					fmt.Sprintf("exactly one ClusterBackupLocation may be the default; %q already is (rule 4)", other.Name),
				)},
			)
		}
	}
	return nil, nil
}
