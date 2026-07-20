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

	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
)

// The transient CLUSTER-scoped binding for a cluster-manifests capture (adr/0011 §1). It is the
// sibling of the namespaced transient RoleBinding (manifest_rbac.go), and it differs in exactly
// one structural way that changes how it is confined.
//
// A namespaced RoleBinding to a ClusterRole whose rules say apiGroups "*" is confined by the
// NAMESPACE it lives in — that is what makes the namespaced manifest reader safe. A
// ClusterRoleBinding has no namespace to confine it, so the same "*" would be a standing read of
// every object in the cluster. The confinement here is instead the ENUMERATED allow-list of the
// crystal-cluster-manifest-reader ClusterRole (charts/.../manifest-mover-rbac.yaml): the reader
// can only get/list the handful of kinds adr/0011 §1 names, and widening that list is an ADR
// change. The binding is still transient — created immediately before the capture Job, deleted
// when it reaches a terminal state — because even a bounded standing grant is a grant.
//
// As with the namespaced binding, no ownerReference can carry this: a ClusterRoleBinding is
// cluster-scoped and cannot be owned by the namespaced mover Job (Kubernetes forbids a
// cluster-scoped dependent of a namespaced owner). So the OrphanReaper is again the only
// automatic cleanup, and manifestBindingMinAge governs it just as it does the namespaced one.

// clusterManifestBindingName derives the ClusterRoleBinding name from its Job. ClusterRoleBindings
// share one global namespace, so the name is prefixed distinctly from the namespaced binding to
// keep the two from ever colliding, and it still names its own Job so a leaked binding points at
// its cause.
func clusterManifestBindingName(jobName string) string {
	return "crystal-cluster-manifest-" + jobName
}

// clusterManifestRBACRequest is everything one transient cluster-scoped binding needs. It has no
// TargetNamespace: the binding is cluster-scoped, and the reader ClusterRole is its own boundary.
type clusterManifestRBACRequest struct {
	// JobName is the capture Job this binding accompanies; it derives the binding name and is
	// what the reaper checks for liveness.
	JobName string
	// ClusterRoleName is the enumerated cluster-manifest-reader role.
	ClusterRoleName string
	// ServiceAccountName is the manifest mover SA, in the operator namespace.
	ServiceAccountName string
	// OperatorNamespace is where ServiceAccountName lives, and the label value the reaper keys
	// on to avoid reaping another operator's in-flight grant.
	OperatorNamespace string
}

// ensureClusterManifestBinding creates the transient ClusterRoleBinding, or leaves an identical
// existing one alone. Called immediately BEFORE the capture Job: a Job that starts without its
// binding fails on its first API call, having already consumed an attempt against the backoff
// limit.
func ensureClusterManifestBinding(ctx context.Context, c client.Client, req clusterManifestRBACRequest) error {
	crb := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: clusterManifestBindingName(req.JobName),
			// The same run-identity labels the reaper finds a namespaced binding by. The
			// mover-job and operator-namespace labels are what let it check liveness and refuse
			// to reap another operator's grant.
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

	if err := c.Create(ctx, crb); err != nil {
		if apierrors.IsAlreadyExists(err) {
			// A retried reconcile. The name is deterministic in the Job and roleRef is immutable,
			// so an existing binding for this Job is the one we would have created.
			return nil
		}
		// A Forbidden here is almost always the escalation check: creating a ClusterRoleBinding
		// that grants more than the creator holds requires `bind` on the referenced ClusterRole
		// (the operator's manifest-binder role carries it by resourceName).
		return fmt.Errorf("create transient ClusterRoleBinding %s to %s: %w",
			clusterManifestBindingName(req.JobName), req.ClusterRoleName, err)
	}
	return nil
}

// deleteClusterManifestBinding removes the transient ClusterRoleBinding. NotFound is success: the
// reaper may have got there first, or a previous attempt succeeded before its status update did.
func deleteClusterManifestBinding(ctx context.Context, c client.Client, jobName string) error {
	crb := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: clusterManifestBindingName(jobName)}}
	if err := c.Delete(ctx, crb); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete transient ClusterRoleBinding %s: %w", crb.Name, err)
	}
	return nil
}
