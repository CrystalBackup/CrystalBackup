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

package controller

import (
	"context"
	"fmt"
	"time"

	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
)

// The transient RoleBinding lifecycle of spec/03-security-and-tenancy.md §5.
//
// A manifest mover needs read (backup) or write (restore) on a tenant namespace. That grant is
// the largest in the system, so it exists only for the life of one Job: the operator creates
// the RoleBinding in the TARGET namespace immediately before the Job and deletes it when the
// Job reaches a terminal state.
//
// AN ownerReference CANNOT CARRY THIS, and that is structural rather than an oversight. The
// Job lives in crystal-backup-system (invariant I5) while the binding must live in the tenant
// namespace, and Kubernetes does not permit cross-namespace ownerReferences — the garbage
// collector treats such a dependent as orphaned and deletes it. The creds-Secret precedent in
// I4 works only because that Secret is co-located with its owner.
//
// So the OrphanReaper backstop is not belt and braces; it is the only automatic cleanup, and
// its sweep parameters are a security control rather than a housekeeping tunable. See
// manifestBindingMinAge.

// manifestBindingMinAge is how long a manifest-mover RoleBinding must exist before the reaper
// will consider it, even when it already looks orphaned.
//
// Deliberately far below the reaper's general 30 minutes. That default is calibrated for a
// temp clone PVC, where reaping early would corrupt an in-flight backup — the cost of waiting
// is only storage. Here the object left behind is a standing read-on-all-Secrets (or
// create/update-on-arbitrary-kinds) grant in a tenant namespace, so the cost of waiting is a
// live privilege. Two minutes is long enough to clear the window between a Job going terminal
// and the reconcile that tears it down, and short enough that a leak is measured in minutes.
const manifestBindingMinAge = 2 * time.Minute

// manifestBindingName derives the RoleBinding name from the Job it accompanies, so the two are
// findable from each other without a lookup table and a leaked binding names its own cause.
func manifestBindingName(jobName string) string {
	return "crystal-manifest-" + jobName
}

// manifestRBACRequest is everything one transient binding needs.
type manifestRBACRequest struct {
	// TargetNamespace is the tenant namespace the grant applies in. The RoleBinding lands
	// here, which is what confines a ClusterRole with apiGroups "*" to one namespace.
	TargetNamespace string
	// JobName is the mover Job this binding accompanies; it derives the binding name and is
	// what the reaper checks for liveness.
	JobName string
	// ClusterRoleName is the reader (backup) or writer (restore) role.
	ClusterRoleName string
	// ServiceAccountName is the manifest mover SA, which lives in the operator namespace even
	// though the binding does not — a RoleBinding may name a subject from another namespace,
	// and that asymmetry is the whole reason this works.
	ServiceAccountName string
	// OperatorNamespace is where ServiceAccountName lives.
	OperatorNamespace string
}

// ensureManifestRoleBinding creates the transient binding, or leaves an identical existing one
// alone. It is called immediately BEFORE the Job is created: a Job that starts without its
// binding fails on the first API call, having already been counted as started.
func ensureManifestRoleBinding(ctx context.Context, c client.Client, req manifestRBACRequest) error {
	rb := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      manifestBindingName(req.JobName),
			Namespace: req.TargetNamespace,
			// The run-identity labels are how the reaper finds these without sweeping every
			// RoleBinding in the cluster, and how an operator answers "what is this doing in my
			// namespace" without reading the controller's source.
			Labels: map[string]string{
				apiconst.LabelManagedBy:  apiconst.ManagedByValue,
				apiconst.LabelMoverRole:  apiconst.MoverRoleManifest,
				apiconst.LabelMoverJob:   req.JobName,
				apiconst.LabelOperatorNS: req.OperatorNamespace,
			},
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "ClusterRole",
			Name:     req.ClusterRoleName,
		},
		Subjects: []rbacv1.Subject{{
			Kind:      rbacv1.ServiceAccountKind,
			Name:      req.ServiceAccountName,
			Namespace: req.OperatorNamespace,
		}},
	}

	if err := c.Create(ctx, rb); err != nil {
		if apierrors.IsAlreadyExists(err) {
			// A retried reconcile. The name is deterministic in the Job, and roleRef is
			// immutable in Kubernetes anyway, so an existing binding for this Job is the one we
			// would have created.
			return nil
		}
		// Worth naming explicitly: a Forbidden here is almost always the escalation check, not
		// a missing verb. Creating a RoleBinding that grants more than the creator holds
		// requires `bind` on the referenced ClusterRole (charts/.../manifest-mover-rbac.yaml).
		return fmt.Errorf("create transient RoleBinding %s/%s to %s: %w",
			req.TargetNamespace, manifestBindingName(req.JobName), req.ClusterRoleName, err)
	}
	return nil
}

// deleteManifestRoleBinding removes the transient binding. This is the nominal path and it is
// the one that must be fast — the binding's lifetime is meant to be the Job's, not the run's.
// A NotFound is success: the reaper may have got there first, or a previous attempt succeeded
// before its status update did.
func deleteManifestRoleBinding(ctx context.Context, c client.Client, targetNamespace, jobName string) error {
	rb := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: manifestBindingName(jobName), Namespace: targetNamespace},
	}
	if err := c.Delete(ctx, rb); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete transient RoleBinding %s/%s: %w", targetNamespace, rb.Name, err)
	}
	return nil
}
