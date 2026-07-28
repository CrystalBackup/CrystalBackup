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
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
)

// The two planes, reduced to the handful of facts a repository actually needs.
//
// A restic repository does not care which plane it belongs to: it needs a URL, a password, S3
// credentials and a mode. What differs is only WHERE each of those comes from — the operator
// namespace and a wrapped platform DEK on the cluster plane, the user's own namespace and their
// own password on the namespace plane. locationBinding is that difference, resolved once per
// reconcile so every consumer below it can be plane-agnostic.
//
// It exists rather than a `switch loc.(type)` at each call site because the number of consumers
// is large (repository init, backup, restore, discovery, maintenance, erasure, sync) and every
// one of them that forgets a plane is a bug that only shows up on the plane nobody tested.

const (
	// kindBackupLocation is the status.location.kind a namespace-plane repository records.
	kindBackupLocation = "BackupLocation"

	// scopeNamespaced is BackupRepository.status.scope for a repository backing a namespaced
	// BackupLocation.
	scopeNamespaced = "Namespaced"

	// keySlotTenant is the key slot a namespace-plane repository always advertises: the user's
	// own restic password. A repository with platformAccess also advertises keySlotPlatform,
	// and the ORDER is part of the contract (02-api.md: `[tenant]` or `[tenant, platform]`) —
	// the tenant slot is the primary one.
	keySlotTenant = "tenant"

	// namespacedRepoSeparator joins a namespace and a location name into the cluster-scoped
	// BackupRepository name (02-api.md: `c-team-x--my-offsite`). Two dashes, because a single
	// one is legal inside both halves and would make the boundary unfindable.
	namespacedRepoSeparator = "--"
)

// namespacedRepositoryName returns the cluster-scoped BackupRepository name backing the
// namespaced BackupLocation name in namespace.
//
// The mapping is deterministic so any reconcile can find a location's repository without a
// lookup — but it is NOT injective, and that is worth knowing rather than hoping: namespace and
// object names may both contain "--", so ("a--b", "c") and ("a", "b--c") produce the same name.
// Nothing here can fix that (the name has one flat namespace), so the collision is caught where
// it can be caught: the BackupLocation controller refuses to adopt a repository whose back-link
// labels name a DIFFERENT location, rather than silently writing one tenant's backups into
// another tenant's repository.
func namespacedRepositoryName(namespace, name string) string {
	return namespace + namespacedRepoSeparator + name
}

// locationBinding is the plane-independent view of the location behind a BackupRepository.
type locationBinding struct {
	// Kind is kindClusterBackupLocation or kindBackupLocation.
	Kind string
	// Namespace is the location's namespace; empty for a cluster location.
	Namespace string
	// Name is the location's name.
	Name string
	// S3 is the object storage the repository lives in.
	S3 cbv1.S3Spec
	// Mode is Standard or Immutable.
	Mode cbv1.LocationMode
	// ClusterID composes the repository path. For a namespaced location it is the EFFECTIVE
	// value recorded in its status (sticky once resolved), never spec.clusterID directly.
	ClusterID string
	// CredsNamespace is where S3.CredentialsSecretRef lives: the operator namespace for a
	// cluster location, the location's OWN namespace for a namespaced one. Getting this wrong
	// is not a "not found" — on the namespace plane it would read an admin Secret of the same
	// name and back a tenant's data up with platform credentials.
	CredsNamespace string
	// PasswordSecretRef is the namespace plane's spec.encryption.repositoryPasswordSecretRef
	// name; empty means "generate one". Unused on the cluster plane.
	PasswordSecretRef string
	// PlatformAccess mirrors the namespace plane's spec.encryption.platformAccess.
	PlatformAccess bool
}

// Namespaced reports whether this binding is a namespace-plane one.
func (b *locationBinding) Namespaced() bool { return b.Kind == kindBackupLocation }

// Scope returns the BackupRepository.status.scope this binding implies.
func (b *locationBinding) Scope() string {
	if b.Namespaced() {
		return scopeNamespaced
	}
	return scopeCluster
}

// KeySlots returns the BackupRepository.status.keySlots this binding implies.
func (b *locationBinding) KeySlots() []string {
	if !b.Namespaced() {
		return []string{keySlotPlatform}
	}
	if b.PlatformAccess {
		return []string{keySlotTenant, keySlotPlatform}
	}
	return []string{keySlotTenant}
}

// Describe returns a short "kind ns/name" (or "kind name") for messages and events.
func (b *locationBinding) Describe() string {
	if b.Namespace == "" {
		return b.Kind + " " + b.Name
	}
	return b.Kind + " " + b.Namespace + "/" + b.Name
}

// bindingFromClusterLocation builds the cluster-plane binding.
func bindingFromClusterLocation(cbl *cbv1.ClusterBackupLocation, operatorNamespace string) *locationBinding {
	return &locationBinding{
		Kind:           kindClusterBackupLocation,
		Name:           cbl.Name,
		S3:             cbl.Spec.S3,
		Mode:           cbl.Spec.Mode,
		ClusterID:      cbl.Spec.ClusterID,
		CredsNamespace: operatorNamespace,
	}
}

// bindingFromNamespacedLocation builds the namespace-plane binding. It uses status.ClusterID —
// the sticky effective value — rather than spec.ClusterID, so a location that defaulted its
// cluster ID keeps pointing at the repository it already wrote to even if the default
// ClusterBackupLocation changes underneath it.
func bindingFromNamespacedLocation(loc *cbv1.BackupLocation) *locationBinding {
	return &locationBinding{
		Kind:              kindBackupLocation,
		Namespace:         loc.Namespace,
		Name:              loc.Name,
		S3:                loc.Spec.S3,
		Mode:              loc.Spec.Mode,
		ClusterID:         loc.Status.ClusterID,
		CredsNamespace:    loc.Namespace,
		PasswordSecretRef: passwordSecretRefName(loc),
		PlatformAccess:    loc.Spec.Encryption.PlatformAccess,
	}
}

// passwordSecretRefName flattens the optional repositoryPasswordSecretRef to a name, with the
// empty string meaning "the operator generates one".
func passwordSecretRefName(loc *cbv1.BackupLocation) string {
	if loc.Spec.Encryption.RepositoryPasswordSecretRef == nil {
		return ""
	}
	return loc.Spec.Encryption.RepositoryPasswordSecretRef.Name
}

// repositoryBackLinkLabels are the labels a namespace-plane BackupRepository carries in place of
// the ownerReference it cannot have. See apiconst.LabelLocation.
func repositoryBackLinkLabels(namespace, name string) map[string]string {
	return map[string]string{
		apiconst.LabelManagedBy: apiconst.ManagedByValue,
		apiconst.LabelNamespace: namespace,
		apiconst.LabelLocation:  name,
	}
}

// resolveLocationBinding resolves the location behind br, from either plane.
//
// Cluster plane: the controller ownerReference the ClusterBackupLocation controller set — a real
// one, since both objects are cluster-scoped. Namespace plane: the back-link labels, because a
// cluster-scoped BackupRepository CANNOT carry an ownerReference to a namespaced BackupLocation
// (Kubernetes reads a namespaced owner on a cluster-scoped dependent as dangling and deletes the
// dependent — the repository would vanish under the location that created it).
//
// A missing or unresolvable owner is returned as an apierrors NotFound so the caller can treat
// "no owner yet" and "owner transiently missing" identically: degrade and requeue, never
// hard-fail.
func resolveLocationBinding(ctx context.Context, c client.Client, br *cbv1.BackupRepository, operatorNamespace string) (*locationBinding, error) {
	if ns, name := br.Labels[apiconst.LabelNamespace], br.Labels[apiconst.LabelLocation]; ns != "" && name != "" {
		var loc cbv1.BackupLocation
		if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &loc); err != nil {
			return nil, err
		}
		return bindingFromNamespacedLocation(&loc), nil
	}

	owner := metav1.GetControllerOf(br)
	if owner == nil || owner.Kind != kindClusterBackupLocation {
		return nil, apierrors.NewNotFound(
			cbv1.GroupVersion.WithResource("clusterbackuplocations").GroupResource(), "<none>")
	}
	var cbl cbv1.ClusterBackupLocation
	if err := c.Get(ctx, client.ObjectKey{Name: owner.Name}, &cbl); err != nil {
		return nil, err
	}
	return bindingFromClusterLocation(&cbl, operatorNamespace), nil
}

// defaultClusterID resolves the cluster identifier a namespaced BackupLocation inherits when it
// does not declare one: the default ClusterBackupLocation's. It returns an empty string with a
// nil error when there is no default yet — a legitimate transient state on a fresh cluster,
// which the caller reports as "waiting" rather than as a fault.
//
// Ambiguity is resolved by NAME order, deterministically, so two admins racing to mark their
// location default cannot make a tenant's repository path flip between reconciles. The
// ClusterBackupLocation controller separately surfaces the conflict on the locations themselves
// (MultipleDefaults); this function's job is only to stop being a source of nondeterminism.
func defaultClusterID(ctx context.Context, c client.Client) (string, error) {
	var locations cbv1.ClusterBackupLocationList
	if err := c.List(ctx, &locations); err != nil {
		return "", fmt.Errorf("list ClusterBackupLocations to default the cluster ID: %w", err)
	}
	best := ""
	for i := range locations.Items {
		l := &locations.Items[i]
		if !l.Spec.Default || l.Spec.ClusterID == "" {
			continue
		}
		if best == "" || strings.Compare(l.Name, best) < 0 {
			best = l.Name
		}
	}
	if best == "" {
		return "", nil
	}
	for i := range locations.Items {
		if locations.Items[i].Name == best {
			return locations.Items[i].Spec.ClusterID, nil
		}
	}
	return "", nil
}
