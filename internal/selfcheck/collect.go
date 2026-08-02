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

package selfcheck

import (
	"cmp"
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/version"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/alerts"
	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
	"github.com/CrystalBackup/CrystalBackup/internal/metrics"
	"github.com/CrystalBackup/CrystalBackup/internal/nsselector"
	"github.com/CrystalBackup/CrystalBackup/internal/schedule"
)

// ServerInfo is the sliver of client-go's discovery interface this package needs. Narrow on
// purpose: it makes the collector testable without a cluster, and it makes explicit that the only
// things asked of discovery are a version string and a group list — neither of which can leak
// anything, and both of which are readable by any authenticated identity.
type ServerInfo interface {
	ServerVersion() (*version.Info, error)
	ServerGroups() (*metav1.APIGroupList, error)
}

// Options configure one collection.
type Options struct {
	// Reader is the operator's own client: the self-check reads what the collectors read, with the
	// RBAC the operator already has.
	Reader client.Reader
	// OperatorNamespace holds the operator Deployment, every mover Job and the platform Secrets.
	OperatorNamespace string
	// Now is injected so the age arithmetic is testable, exactly as internal/schedule and the
	// alert predicates do it.
	Now time.Time
	// Full disables redaction.
	Full bool
	// RedactionSalt, when non-nil, is used instead of a fresh random salt, which makes this report's
	// tokens correlate with every other report built on the same bytes. Nil is the default and the
	// behaviour everything but a soak wants. See redact.go for what it costs.
	RedactionSalt []byte
	// RedactionSaltSource names where RedactionSalt came from, and is REPORTED rather than acted
	// on: two 32-byte salts are the same bytes to this package and make different promises to a
	// reader (see selfcheck.Salt* and Redactor.Describe). Ignored when RedactionSalt is nil;
	// defaults to SaltCallerSupplied when a salt is given without one, which is what every caller
	// before the soak's derived mode meant.
	RedactionSaltSource string
	// Discovery is optional. Without it the Kubernetes version is absent and the CRD inventory
	// loses its fallback — both reported as diagnostics rather than silently omitted.
	Discovery ServerInfo
	// DeclaredImages is what the operator was CONFIGURED to run for the Jobs it creates, keyed by
	// role. Reported beside what is actually running, never instead of it.
	DeclaredImages map[string]string
}

// reaperGrace mirrors internal/controller's defaultReaperMinAge: the age below which a residual
// object is simply an in-flight one. It is duplicated rather than exported from there because
// importing the controller package into a CLI path would drag the whole manager in; the number's
// only job here is to LABEL a count, and leakGraceNote says so in the report.
const reaperGrace = 30 * time.Minute

// Collect builds the report. It never fails on a partial read: anything unreadable becomes a
// Diagnostic and leaves its section empty, because "the operator was not allowed to look" and
// "there is nothing there" must not render the same way.
func Collect(ctx context.Context, opts Options) (*Report, error) {
	source := opts.RedactionSaltSource
	if source == "" {
		source = SaltCallerSupplied
	}
	red, err := NewRedactorWithSource(opts.Full, opts.RedactionSalt, source)
	if err != nil {
		return nil, err
	}
	c := &collector{Options: opts, red: red}
	if c.Now.IsZero() {
		c.Now = time.Now()
	}

	rep := &Report{
		ReportVersion: ReportVersion,
		GeneratedAt:   c.Now.UTC(),
	}

	// Inventory first: it is what teaches the redactor the names that later sections' free text
	// must have substituted out. Reordering these calls would leak a name into a breach Detail.
	rep.Inventory = c.inventory(ctx)
	rep.Operator, rep.Cluster = c.identity(ctx)
	rep.Images = c.images(ctx)
	rep.CRDs = c.crds(ctx)
	rep.Leaks = c.leaks(ctx)
	rep.Rules = c.rules(ctx)
	rep.Verdict = verdictOf(rep.Rules)
	rep.Redaction = red.Describe()
	rep.Diagnostics = c.diags
	return rep, nil
}

type collector struct {
	Options
	red   *Redactor
	diags []Diagnostic

	// clusterID is resolved once from the locations and reused by the sections that carry it.
	clusterID string
}

func (c *collector) diag(area, impact string, err error) {
	c.diags = append(c.diags, Diagnostic{Area: area, Message: err.Error(), Impact: impact})
}

// --- identity -------------------------------------------------------------------------------

// identity fills the operator and cluster blocks from three independent sources, and says so when
// they disagree.
func (c *collector) identity(ctx context.Context) (Operator, Cluster) {
	op := Operator{
		BuildVersion: metrics.Version,
		// Redacted like any other namespace, and the temptation not to is worth naming: it is the
		// one namespace a reader might want in clear, to paste into a kubectl command. But it is
		// still a name an admin chose, it is frequently the organisation's, and it appears again in
		// every leak sample — so leaving it in clear here would have been both a leak and an
		// inconsistency, the same string rendered two ways in one document. The person who ran the
		// self-check already knows their own namespace; --full is for the case where the reader
		// needs it too.
		Namespace: c.red.Namespace(c.OperatorNamespace),
	}
	cl := Cluster{ClusterID: c.red.ClusterID(c.clusterID)}

	var pods corev1.PodList
	if err := c.Reader.List(ctx, &pods, client.InNamespace(c.OperatorNamespace)); err != nil {
		c.diag("operator", "chart and app version unknown; images section empty", err)
	} else {
		for i := range pods.Items {
			p := &pods.Items[i]
			if roleOf(p) != roleOperator {
				continue
			}
			if v := p.Labels[labelChart]; v != "" {
				op.ChartVersion = v
			}
			if v := p.Labels[labelVersion]; v != "" {
				op.AppVersion = v
			}
		}
	}
	// The disagreement worth naming, still — but it means something different since 0.6.0.
	//
	// crystalbackup_build_info's version comes from an -X link flag. NOTHING passed it until M6,
	// so every image up to and including 0.5.1 reports "dev" no matter which release it is. From
	// 0.6.0 the release workflow, the Makefile and the Dockerfile all stamp it, and "dev" now
	// means one of two honest things: an operator from a pre-0.6.0 image, or a local build that
	// was never told what it is (a bare `docker build .` takes the Dockerfile's ARG default).
	//
	// Either way the reader needs the same steer — believe appVersion and the digests — so the
	// note stays. Dropping it would leave a bare "dev" next to a real chart version, which is the
	// reading most likely to be mistaken for an unreleased build.
	if op.BuildVersion == "dev" && op.AppVersion != "" {
		op.VersionNote = "buildVersion is the compiled-in default (\"dev\"): this binary was linked " +
			"without -X internal/metrics.Version — either a pre-0.6.0 image, which never stamped it, " +
			"or a local build. Trust appVersion and the image digests below."
	}

	if c.Discovery != nil {
		if v, err := c.Discovery.ServerVersion(); err == nil && v != nil {
			cl.KubernetesVersion = v.GitVersion
		} else if err != nil {
			c.diag("cluster", "Kubernetes version unknown", err)
		}
	}

	// Whether the snapshot API exists at all, which decides whether a volume backup is possible.
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(schema.GroupVersionKind{
		Group: snapshotGroup, Version: "v1", Kind: "VolumeSnapshotClassList",
	})
	switch err := c.Reader.List(ctx, list); {
	case err != nil:
		cl.SnapshotAPI = "absent or unreadable"
	case len(list.Items) == 0:
		cl.SnapshotAPI = "installed, but no VolumeSnapshotClass exists"
	default:
		cl.SnapshotAPI = fmt.Sprintf("installed, %d VolumeSnapshotClass(es)", len(list.Items))
	}
	return op, cl
}

// --- images ---------------------------------------------------------------------------------

// The chart's own labels, and the group the snapshot API lives in. Named once each because a
// label key compared against a misspelled literal matches nothing and reports "no operator pod".
const (
	labelPartOf    = "app.kubernetes.io/part-of"
	labelComponent = "app.kubernetes.io/component"
	labelChart     = "helm.sh/chart"
	labelVersion   = "app.kubernetes.io/version"
	partOfValue    = "crystal-backup"
	snapshotGroup  = "snapshot.storage.k8s.io"
	kindSnapshot   = "VolumeSnapshot"
	conditionReady = "Ready"
)

// The three words the headline verdict can take. Named because they appear in the JSON, in the
// template's class selectors and in the summary sentence, and a fourth spelling would silently
// style an unhealthy report as a healthy one.
const (
	verdictHealthy   = "healthy"
	verdictDegraded  = "degraded"
	verdictUnhealthy = "unhealthy"
)

const (
	roleOperator = "operator"
	roleMover    = "mover"
	roleSync     = "sync"
	roleOther    = "other"
)

// roleOf classifies a pod in the operator namespace.
//
// Sync is separated from mover because it is a THIRD image, not a bigger one: rclone is a hard
// requirement of sync and of nothing else, and adr/0013's whole point is that its dependency
// surface stays off the backup/restore path. A report that collapsed the two would be unable to
// show the one thing that split is for — that the digest running a copy is not the digest running a
// backup.
func roleOf(p *corev1.Pod) string {
	if p.Labels[apiconst.LabelManagedBy] == apiconst.ManagedByValue {
		if p.Labels[labelComponent] == roleSync {
			return roleSync
		}
		return roleMover
	}
	if p.Labels[labelPartOf] == partOfValue ||
		p.Labels[labelComponent] == roleOperator ||
		p.Labels["control-plane"] == "controller-manager" {
		return roleOperator
	}
	return roleOther
}

// images reads status.containerStatuses[].imageID — what the kubelet RESOLVED and pulled — rather
// than spec.containers[].image, which is only what somebody asked for.
//
// The two differ in exactly the case that matters: a mutable tag. `:0.6.0` re-pushed, or `:latest`
// anywhere, means the running digest and the declared reference have nothing to do with each other,
// and every report of the declared one is a report of a fiction. The spec image is still recorded,
// because the PAIR is the finding.
func (c *collector) images(ctx context.Context) Images {
	out := Images{
		Declared: map[string]string{},
		Note: "Digests are read from status.containerStatuses[].imageID — what the kubelet actually " +
			"pulled — never from chart values. A mover or sync image only appears here while one of " +
			"its Job pods exists, so an idle installation shows the operator alone; the configured " +
			"references are listed under `declared` for comparison.",
	}
	for role, ref := range c.DeclaredImages {
		if ref == "" {
			continue
		}
		repo, tag, digest := splitImageRef(ref)
		rendered := c.red.ImageRepository(repo)
		if tag != "" {
			rendered += ":" + tag
		}
		if digest != "" {
			rendered += "@" + digest
		}
		out.Declared[role] = rendered
	}

	var pods corev1.PodList
	if err := c.Reader.List(ctx, &pods, client.InNamespace(c.OperatorNamespace)); err != nil {
		c.diag("images", "no running image digests: the report cannot say what is actually deployed", err)
		return out
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		role := roleOf(p)
		byName := map[string]*corev1.ContainerStatus{}
		for j := range p.Status.ContainerStatuses {
			cs := &p.Status.ContainerStatuses[j]
			byName[cs.Name] = cs
		}
		for _, spec := range p.Spec.Containers {
			img := RunningImage{
				Role:      role,
				Pod:       c.red.Pod(p.Name),
				Container: spec.Name,
			}
			repo, tag, _ := splitImageRef(spec.Image)
			img.Repository = c.red.ImageRepository(repo)
			img.Tag = tag
			if cs := byName[spec.Name]; cs != nil {
				img.Ready = cs.Ready
				img.Restarts = cs.RestartCount
				img.State = containerState(cs)
				// The resolved digest. imageID takes several shapes across runtimes
				// (`repo@sha256:…`, `docker-pullable://repo@sha256:…`, a bare digest); digestOf
				// normalises them, and an empty result means the container has not started, which
				// is itself the answer to "why is nothing happening".
				img.Digest = digestOf(cs.ImageID)
			}
			out.Running = append(out.Running, img)
		}
	}
	slices.SortFunc(out.Running, func(a, b RunningImage) int {
		return cmp.Or(
			cmp.Compare(a.Role, b.Role),
			cmp.Compare(a.Pod, b.Pod),
			cmp.Compare(a.Container, b.Container))
	})
	return out
}

// containerState renders a container's current state in one word plus, when it is not running, the
// reason — which is the whole diagnostic value of the field.
func containerState(cs *corev1.ContainerStatus) string {
	switch {
	case cs.State.Running != nil:
		return "running"
	case cs.State.Waiting != nil:
		return "waiting: " + cs.State.Waiting.Reason
	case cs.State.Terminated != nil:
		return "terminated: " + cs.State.Terminated.Reason
	default:
		return ""
	}
}

// splitImageRef breaks a reference into (repository, tag, digest). The tag/digest split has one
// trap: a registry host may carry a port (`registry:5000/x`), so the tag colon is only the one
// after the last slash.
func splitImageRef(ref string) (repo, tag, digest string) {
	if ref == "" {
		return "", "", ""
	}
	if at := strings.LastIndex(ref, "@"); at >= 0 {
		digest = ref[at+1:]
		ref = ref[:at]
	}
	slash := strings.LastIndex(ref, "/")
	if colon := strings.LastIndex(ref, ":"); colon > slash {
		tag = ref[colon+1:]
		ref = ref[:colon]
	}
	return ref, tag, digest
}

// digestOf extracts the sha256 digest from a container status imageID.
func digestOf(imageID string) string {
	if at := strings.LastIndex(imageID, "@"); at >= 0 {
		return imageID[at+1:]
	}
	if strings.HasPrefix(imageID, "sha256:") {
		return imageID
	}
	return ""
}

// --- CRDs -----------------------------------------------------------------------------------

// crdGVK is asked for through unstructured rather than by importing apiextensions-apiserver: the
// operator's scheme does not carry the type, and adding a dependency to read four fields off an
// object it may not even be allowed to list is not a trade worth making.
var crdGVK = schema.GroupVersionKind{
	Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinitionList",
}

// READ-ONLY, and it must stay that way: an operator able to WRITE a CustomResourceDefinition could
// rewrite the schema of the very objects it is trusted to back up. get/list/watch is the whole
// requirement, and it exists solely so `crystal-backup selfcheck` can report which CRDs are
// installed with their storage version and provenance.
//
// This block is deliberately FREE-STANDING, separated by a blank line from the doc comment below.
// +kubebuilder:rbac is a PACKAGE-scoped marker: attached to a declaration's doc comment it is a
// declaration comment, controller-gen does not collect it, and `make manifests` regenerates a role
// without the rule — silently, with no error and no diff. Written the wrong way first, and found
// only by checking the generated file rather than the exit code.
//
// +kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,verbs=get;list;watch

// crds inventories the operator's own CRDs, from the richest source available.
//
// The discovery fallback stays even with the grant above. A self-check runs on clusters whose RBAC
// nobody has audited, including ones installed by a chart that predates this rule, and the honest
// behaviour there is a weaker answer rather than an error. Discovery gives the SERVED versions of
// the API group and nothing else: no storage version, no per-CRD provenance annotation. Reporting
// which source produced the answer is the difference between a degraded report and a misleading
// one.
func (c *collector) crds(ctx context.Context) CRDInventory {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(crdGVK)
	if err := c.Reader.List(ctx, list); err == nil {
		inv := CRDInventory{Source: "apiextensions"}
		for i := range list.Items {
			item := &list.Items[i]
			group, _, _ := unstructured.NestedString(item.Object, "spec", "group")
			if group != apiconst.Domain {
				continue
			}
			crd := CRD{Name: item.GetName()}
			if v := item.GetAnnotations()["controller-gen.kubebuilder.io/version"]; v != "" {
				crd.GeneratedBy = v
			}
			versions, _, _ := unstructured.NestedSlice(item.Object, "spec", "versions")
			for _, raw := range versions {
				m, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				name, _ := m["name"].(string)
				if served, _ := m["served"].(bool); served && name != "" {
					crd.Versions = append(crd.Versions, name)
				}
				if storage, _ := m["storage"].(bool); storage {
					crd.StorageVersion, _ = m["name"].(string)
				}
			}
			inv.Items = append(inv.Items, crd)
		}
		slices.SortFunc(inv.Items, func(a, b CRD) int { return cmp.Compare(a.Name, b.Name) })
		if len(inv.Items) == 0 {
			inv.Reason = "no " + apiconst.Domain + " CustomResourceDefinition is installed"
		}
		return inv
	} else if c.Discovery == nil {
		c.diag("crds", "CRD inventory unavailable", err)
		return CRDInventory{Source: "unavailable", Reason: err.Error()}
	}

	groups, err := c.Discovery.ServerGroups()
	if err != nil {
		c.diag("crds", "CRD inventory unavailable", err)
		return CRDInventory{Source: "unavailable", Reason: err.Error()}
	}
	inv := CRDInventory{
		Source: "discovery",
		Reason: "the CustomResourceDefinition objects could not be read (the operator's ClusterRole " +
			"does not grant it); this is the API group's SERVED versions only — no storage version " +
			"and no per-CRD provenance",
	}
	for _, g := range groups.Groups {
		if g.Name != apiconst.Domain {
			continue
		}
		crd := CRD{Name: g.Name, StorageVersion: ""}
		for _, v := range g.Versions {
			crd.Versions = append(crd.Versions, v.Version)
		}
		inv.Items = append(inv.Items, crd)
	}
	if len(inv.Items) == 0 {
		inv.Reason += "; the API group is not served at all"
	}
	return inv
}

// --- inventory ------------------------------------------------------------------------------

func (c *collector) inventory(ctx context.Context) Inventory {
	var inv Inventory

	// Namespaces and tenants are learned BEFORE anything else, and the ordering is load-bearing.
	//
	// A token is per-VALUE, not per-(kind, value), so that a BackupRepository and the location it
	// is named after share one token instead of appearing as two things. The cost is that the
	// token's KIND prefix is whichever kind saw the string first — and organisations reuse their
	// own name: `acme-corp` is routinely the tenant AND the S3 prefix. Read in location order, the
	// tenant label came out as `prefix-542dde86`, which is correct and reads like a bug.
	//
	// So the identity kinds go first. A namespace or tenant name that is also used as a bucket
	// prefix now renders as `ns-…`/`tenant-…` everywhere, which is what it is.
	c.red.Learn(kindNamespace, c.OperatorNamespace)
	var namespaces corev1.NamespaceList
	if err := c.Reader.List(ctx, &namespaces); err != nil {
		c.diag("namespaces", "tenant identity unresolved; cluster schedule fan-out not counted", err)
	} else {
		for i := range namespaces.Items {
			ns := &namespaces.Items[i]
			c.red.Learn(kindNamespace, ns.Name)
			c.red.Learn(kindTenant, ns.Labels[apiconst.LabelTenant])
		}
	}

	// Locations next: they carry the clusterID every other section resolves through, and their
	// names are the ones the schedules and syncs refer to.
	var clusterLocs cbv1.ClusterBackupLocationList
	if err := c.Reader.List(ctx, &clusterLocs); err != nil {
		c.diag("locations", "cluster-plane locations missing from the report", err)
	} else {
		var defaultID, consensus string
		agreed := true
		for i := range clusterLocs.Items {
			l := &clusterLocs.Items[i]
			c.red.Learn(kindLocation, l.Name)
			if l.Spec.Default && l.Spec.ClusterID != "" {
				defaultID = l.Spec.ClusterID
			}
			switch {
			case l.Spec.ClusterID == "":
			case consensus == "":
				consensus = l.Spec.ClusterID
			case consensus != l.Spec.ClusterID:
				agreed = false
			}
			inv.Locations = append(inv.Locations, Location{
				Scope:             "cluster",
				Name:              c.red.Location(l.Name),
				Mode:              string(l.Spec.Mode),
				Default:           l.Spec.Default,
				Phase:             l.Status.Phase,
				Ready:             conditionOf(l.Status.Conditions),
				ClusterID:         c.red.ClusterID(l.Spec.ClusterID),
				Endpoint:          c.red.Endpoint(l.Spec.S3.Endpoint),
				Bucket:            c.red.Bucket(l.Spec.S3.Bucket),
				Prefix:            c.red.Prefix(l.Spec.S3.Prefix),
				Region:            l.Spec.S3.Region,
				ForcePathStyle:    l.Spec.S3.ForcePathStyle,
				CustomCA:          l.Spec.S3.CABundle != "",
				CredentialsSecret: c.red.Secret(l.Spec.S3.CredentialsSecretRef.Name),
				AgeDays:           days(c.Now.Sub(l.CreationTimestamp.Time)),
			})
		}
		switch {
		case defaultID != "":
			c.clusterID = defaultID
		case agreed:
			c.clusterID = consensus
		}
		c.red.Learn(kindCluster, c.clusterID)
	}

	var nsLocs cbv1.BackupLocationList
	if err := c.Reader.List(ctx, &nsLocs); err != nil {
		c.diag("locations", "namespace-plane locations missing from the report", err)
	} else {
		for i := range nsLocs.Items {
			l := &nsLocs.Items[i]
			c.red.Learn(kindLocation, l.Name)
			c.red.Learn(kindNamespace, l.Namespace)
			inv.Locations = append(inv.Locations, Location{
				Scope:             "namespace",
				Name:              c.red.Location(l.Name),
				Namespace:         c.red.Namespace(l.Namespace),
				Mode:              string(l.Spec.Mode),
				Phase:             l.Status.Phase,
				Ready:             conditionOf(l.Status.Conditions),
				ClusterID:         c.red.ClusterID(l.Status.ClusterID),
				Endpoint:          c.red.Endpoint(l.Spec.S3.Endpoint),
				Bucket:            c.red.Bucket(l.Spec.S3.Bucket),
				Prefix:            c.red.Prefix(l.Spec.S3.Prefix),
				Region:            l.Spec.S3.Region,
				ForcePathStyle:    l.Spec.S3.ForcePathStyle,
				CustomCA:          l.Spec.S3.CABundle != "",
				CredentialsSecret: c.red.Secret(l.Spec.S3.CredentialsSecretRef.Name),
				AgeDays:           days(c.Now.Sub(l.CreationTimestamp.Time)),
			})
		}
	}
	sortByName(inv.Locations, func(l Location) string { return l.Scope + "/" + l.Namespace + "/" + l.Name })

	var repos cbv1.BackupRepositoryList
	if err := c.Reader.List(ctx, &repos); err != nil {
		c.diag("repositories", "restore-capability signals (check, prune, discovery) missing", err)
	} else {
		for i := range repos.Items {
			r := &repos.Items[i]
			c.red.Learn(kindLocation, r.Name)
			c.red.Learn(kindNamespace, r.Status.OwnerNamespace)
			inv.Repositories = append(inv.Repositories, Repository{
				Name:             c.red.Location(r.Name),
				Location:         c.red.Location(r.Status.Location.Name),
				Scope:            metrics.ScopeLabelValue(r.Status.Scope),
				OwnerNamespace:   c.red.Namespace(r.Status.OwnerNamespace),
				Initialized:      r.Status.Initialized,
				Mode:             string(r.Status.Mode),
				KeySlots:         r.Status.KeySlots,
				SnapshotCount:    r.Status.SnapshotCount,
				SizeBytes:        r.Status.ApproximateSizeBytes,
				StaleLocks:       r.Status.StaleLocks,
				LastCheck:        timeOf(r.Status.LastCheckTime),
				CheckResult:      r.Status.LastCheckResult,
				LastPrune:        timeOf(r.Status.LastMaintenanceTime),
				LastDiscovery:    timeOf(r.Status.LastDiscoveryTime),
				DiscoverySuccess: r.Status.LastDiscoverySuccess,
				ProjectedBackups: r.Status.ProjectedBackups,
				OrphanSnapshots:  r.Status.OrphanSnapshots,
				CheckAgeDays:     ageDays(c.Now, r.Status.LastCheckTime),
				PruneAgeDays:     ageDays(c.Now, r.Status.LastMaintenanceTime),
			})
		}
		sortByName(inv.Repositories, func(r Repository) string { return r.Name })
	}

	inv.Schedules = c.schedules(ctx)
	inv.Syncs = c.syncs(ctx)
	inv.Backups = c.backups(ctx)
	return inv
}

func (c *collector) schedules(ctx context.Context) []Schedule {
	var out []Schedule

	var namespaces corev1.NamespaceList
	nsListed := c.Reader.List(ctx, &namespaces) == nil

	var nsScheds cbv1.BackupScheduleList
	if err := c.Reader.List(ctx, &nsScheds); err != nil {
		c.diag("schedules", "namespace-plane schedules missing from the report", err)
	} else {
		for i := range nsScheds.Items {
			s := &nsScheds.Items[i]
			c.red.Learn(kindSchedule, s.Name)
			c.red.Learn(kindNamespace, s.Namespace)
			out = append(out, Schedule{
				Origin:              apiconst.OriginNamespace,
				Name:                c.red.Schedule(s.Name),
				Namespace:           c.red.Namespace(s.Namespace),
				Cron:                s.Spec.Schedule,
				Timezone:            s.Spec.Timezone,
				Paused:              s.Spec.Paused,
				Location:            c.red.Location(s.Spec.LocationRef.Name),
				Phase:               s.Status.Phase,
				LastSuccess:         timeOf(s.Status.LastSuccessTime),
				NextRun:             timeOf(s.Status.NextScheduleTime),
				LastSuccessAgeHours: ageHours(c.Now, s.Status.LastSuccessTime),
				AgeDays:             days(c.Now.Sub(s.CreationTimestamp.Time)),
				CronValid:           cronValid(s.Spec.Schedule, s.Spec.Timezone),
				PeriodHours:         periodHours(s.Spec.Schedule, s.Spec.Timezone, c.Now),
			})
		}
	}

	var clScheds cbv1.ClusterBackupScheduleList
	if err := c.Reader.List(ctx, &clScheds); err != nil {
		c.diag("schedules", "cluster-plane schedules missing from the report", err)
	} else {
		for i := range clScheds.Items {
			s := &clScheds.Items[i]
			c.red.Learn(kindSchedule, s.Name)
			matched := 0
			if nsListed {
				if names, err := nsselector.Match(namespaces.Items, s.Spec.Template.Spec.Namespaces); err == nil {
					matched = len(names)
				}
			}
			out = append(out, Schedule{
				Origin:              apiconst.OriginCluster,
				Name:                c.red.Schedule(s.Name),
				Cron:                s.Spec.Schedule,
				Timezone:            s.Spec.Timezone,
				Paused:              s.Spec.Paused,
				Location:            c.red.Location(s.Spec.Template.Spec.LocationRef.Name),
				Phase:               s.Status.Phase,
				LastSuccess:         timeOf(s.Status.LastSuccessTime),
				NextRun:             timeOf(s.Status.NextScheduleTime),
				LastSuccessAgeHours: ageHours(c.Now, s.Status.LastSuccessTime),
				AgeDays:             days(c.Now.Sub(s.CreationTimestamp.Time)),
				CronValid:           cronValid(s.Spec.Schedule, s.Spec.Timezone),
				PeriodHours:         periodHours(s.Spec.Schedule, s.Spec.Timezone, c.Now),
				MatchedNamespaces:   matched,
			})
		}
	}
	sortByName(out, func(s Schedule) string { return s.Origin + "/" + s.Namespace + "/" + s.Name })
	return out
}

func (c *collector) syncs(ctx context.Context) []Sync {
	var out []Sync

	var clSyncs cbv1.ClusterBackupExternalSyncList
	if err := c.Reader.List(ctx, &clSyncs); err != nil {
		c.diag("syncs", "cluster-plane external syncs missing from the report", err)
	} else {
		for i := range clSyncs.Items {
			s := &clSyncs.Items[i]
			c.red.Learn(kindSync, s.Name)
			out = append(out, Sync{
				Scope:               apiconst.OriginCluster,
				Name:                c.red.Sync(s.Name),
				Source:              c.red.Location(s.Spec.SourceLocationRef.Name),
				Destination:         c.red.Location(s.Spec.DestinationLocationRef.Name),
				Mode:                string(s.Spec.Mode),
				Cron:                s.Spec.Schedule,
				Paused:              s.Spec.Paused,
				Phase:               s.Status.Phase,
				LastSuccess:         timeOf(s.Status.LastSuccessTime),
				LastSuccessAgeHours: ageHours(c.Now, s.Status.LastSuccessTime),
				SnapshotsCopied:     s.Status.SnapshotsCopied,
				Lag:                 s.Status.LagSnapshots,
				AgeDays:             days(c.Now.Sub(s.CreationTimestamp.Time)),
			})
		}
	}

	var nsSyncs cbv1.BackupExternalSyncList
	if err := c.Reader.List(ctx, &nsSyncs); err != nil {
		c.diag("syncs", "namespace-plane external syncs missing from the report", err)
	} else {
		for i := range nsSyncs.Items {
			s := &nsSyncs.Items[i]
			c.red.Learn(kindSync, s.Name)
			c.red.Learn(kindNamespace, s.Namespace)
			out = append(out, Sync{
				Scope:               apiconst.OriginNamespace,
				Name:                c.red.Sync(s.Name),
				Namespace:           c.red.Namespace(s.Namespace),
				Source:              c.red.Location(s.Spec.SourceLocationRef.Name),
				Destination:         c.red.Location(s.Spec.DestinationLocationRef.Name),
				Mode:                string(s.Spec.Mode),
				Cron:                s.Spec.Schedule,
				Paused:              s.Spec.Paused,
				Phase:               s.Status.Phase,
				LastSuccess:         timeOf(s.Status.LastSuccessTime),
				LastSuccessAgeHours: ageHours(c.Now, s.Status.LastSuccessTime),
				SnapshotsCopied:     s.Status.SnapshotsCopied,
				Lag:                 s.Status.LagSnapshots,
				AgeDays:             days(c.Now.Sub(s.CreationTimestamp.Time)),
			})
		}
	}
	sortByName(out, func(s Sync) string { return s.Scope + "/" + s.Namespace + "/" + s.Name })
	return out
}

func (c *collector) backups(ctx context.Context) BackupCensus {
	census := BackupCensus{ByPhase: map[string]int{}, ByNamespace: map[string]int{}}
	var backups cbv1.BackupList
	if err := c.Reader.List(ctx, &backups); err != nil {
		c.diag("backups", "restore-point census missing from the report", err)
		return census
	}
	for i := range backups.Items {
		b := &backups.Items[i]
		c.red.Learn(kindNamespace, b.Namespace)
		census.Total++
		phase := b.Status.Phase
		if phase == "" {
			phase = "(none)"
		}
		census.ByPhase[phase]++
		census.ByNamespace[c.red.Namespace(b.Namespace)]++
		if t := b.Status.BackupTime; t != nil {
			if census.NewestSuccess == nil || t.After(*census.NewestSuccess) {
				at := t.UTC()
				census.NewestSuccess = &at
			}
			if census.OldestSuccess == nil || t.Time.Before(*census.OldestSuccess) {
				at := t.UTC()
				census.OldestSuccess = &at
			}
		}
	}
	return census
}

// --- leaks ----------------------------------------------------------------------------------

// leaks counts the residue a crashed teardown leaves behind: the object classes M4's leak audit
// found stranded, each split by age against the reaper's own grace period.
//
// The split is the whole measurement. A backup running right now legitimately owns a temp clone
// PVC, a mover Job and a VolumeSnapshot; a raw count of those says nothing. What says something is
// the same object still there long after the reaper has had a full cycle to remove it.
func (c *collector) leaks(ctx context.Context) Leaks {
	out := Leaks{
		GraceMinutes: int(reaperGrace.Minutes()),
		Note: "`residual` counts managed objects older than the orphan reaper's grace period — the " +
			"age past which an object is no longer plausibly in flight. A non-zero residual is the " +
			"leak signal; the raw total is not, because a running backup legitimately owns a temp " +
			"PVC, a mover Job and a VolumeSnapshot.",
	}
	cutoff := c.Now.Add(-reaperGrace)
	managed := client.MatchingLabels{apiconst.LabelManagedBy: apiconst.ManagedByValue}
	inOperatorNS := client.InNamespace(c.OperatorNamespace)

	type candidate struct {
		kind      string
		namespace string
		name      string
		created   time.Time
	}
	var found []candidate
	tally := map[string]*LeakKind{}
	record := func(kind, namespace, name string, created time.Time) {
		k := tally[kind]
		if k == nil {
			k = &LeakKind{Kind: kind}
			tally[kind] = k
		}
		k.Total++
		age := c.Now.Sub(created)
		if !created.Before(cutoff) {
			return
		}
		k.Residual++
		if h := int(age.Hours()); h > k.OldestAgeHours {
			k.OldestAgeHours = h
		}
		found = append(found, candidate{kind: kind, namespace: namespace, name: name, created: created})
	}

	// The per-PVC exposure objects in the operator namespace. HasLabels on the per-PVC label is
	// what separates them from the standing managed objects that must never be counted as residue —
	// the repository-init Jobs and, above all, the wrapped-DEK Secret.
	hasPVC := client.HasLabels{apiconst.LabelPVC}
	var jobs batchv1.JobList
	if err := c.Reader.List(ctx, &jobs, inOperatorNS, managed, hasPVC); err != nil {
		out.Unreadable = append(out.Unreadable, "Job: "+err.Error())
	} else {
		for i := range jobs.Items {
			j := &jobs.Items[i]
			record("Job", j.Namespace, j.Name, j.CreationTimestamp.Time)
		}
	}
	var pvcs corev1.PersistentVolumeClaimList
	if err := c.Reader.List(ctx, &pvcs, inOperatorNS, managed, hasPVC); err != nil {
		out.Unreadable = append(out.Unreadable, "PersistentVolumeClaim: "+err.Error())
	} else {
		for i := range pvcs.Items {
			p := &pvcs.Items[i]
			record("PersistentVolumeClaim", p.Namespace, p.Name, p.CreationTimestamp.Time)
		}
	}

	// The snapshot residue is CLUSTER-WIDE and is the half whose absence from the reaper's charter
	// stranded a Retain-parked, owner-less VolumeSnapshotContent forever. It is listed by GVK for
	// the usual reason (not in the scheme), and a missing snapshot API is recorded rather than
	// treated as zero.
	for _, kind := range []string{kindSnapshot, kindSnapshot + "Content"} {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(schema.GroupVersionKind{
			Group: snapshotGroup, Version: "v1", Kind: kind + "List",
		})
		if err := c.Reader.List(ctx, list, managed); err != nil {
			out.Unreadable = append(out.Unreadable, kind+": "+err.Error())
			continue
		}
		for i := range list.Items {
			it := &list.Items[i]
			record(kind, it.GetNamespace(), it.GetName(), it.GetCreationTimestamp().Time)
		}
	}

	for _, k := range tally {
		out.Kinds = append(out.Kinds, *k)
		out.Totals.Residual += k.Residual
	}
	slices.SortFunc(out.Kinds, func(a, b LeakKind) int { return cmp.Compare(a.Kind, b.Kind) })

	// A bounded sample, oldest first — enough to go and look, not enough to turn the report into a
	// listing of somebody's cluster.
	slices.SortFunc(found, func(a, b candidate) int { return a.created.Compare(b.created) })
	const maxSamples = 10
	for i, f := range found {
		if i >= maxSamples {
			break
		}
		out.Samples = append(out.Samples, LeakItem{
			Kind:      f.kind,
			Namespace: c.red.Namespace(f.namespace),
			Name:      c.red.Object(f.name),
			AgeHours:  int(c.Now.Sub(f.created).Hours()),
		})
	}
	return out
}

// --- rules ----------------------------------------------------------------------------------

// rules evaluates every alert rule's state predicate.
//
// This is the half that gives an opinion, and the reason it can be trusted next to a Prometheus
// that is not there: each predicate reads its bound out of the very alerts.Rule.Threshold the
// PromQL was assembled from, so a threshold moved in the rule table moves both answers at once.
// There is no number here to keep in sync, because there is no number here.
//
// A rule with no predicate is StatusNotEvaluated with the reason stated. That is the point of the
// status existing: an unmeasured OK is precisely the thing this report is built to refuse. Every
// rule carries one today; the branch stays because "not evaluated" must remain expressible for a
// question object state genuinely cannot answer.
//
// The predicate is read off the rule itself. It briefly came from a separate exported map, and the
// lookup here tried both — which meant a rule could be answered by one predicate through one path
// and a different one through the other. One field, one path.
func (c *collector) rules(ctx context.Context) []RuleResult {
	rules := alerts.Rules()
	out := make([]RuleResult, 0, len(rules))
	for _, r := range rules {
		res := RuleResult{
			Name:      r.Name,
			Severity:  string(r.Severity),
			Summary:   plainText(r.Summary),
			Remedy:    plainText(r.Description),
			Threshold: thresholdView(r.Threshold),
			Fidelity:  alerts.Fidelity(r.Name),
		}
		p := r.Predicate
		if p == nil {
			res.Status = StatusNotEvaluated
			res.Reason = "no state predicate: this rule's question cannot be answered from object " +
				"state, or has not been implemented. It is NOT a pass — only Prometheus can answer it."
		} else {
			breaches, err := p(ctx, c.Reader, c.Now)
			switch {
			case err != nil:
				res.Status = StatusError
				res.Reason = err.Error()
				// Also a top-level diagnostic, because a rule that errored is a HOLE in the verdict
				// and the diagnostics section is where this report says what it could not measure.
				// Without it the gap is visible only to a reader who walks all eleven rule entries
				// looking for the one that is not green — which is the reading nobody does under
				// pressure, and this is a document read under pressure.
				//
				// Both causes land here and both belong. alerts.ErrUnknownRule is OUR bug and used
				// to be a panic inside this binary; anything else is the operator's environment,
				// usually RBAC. The Area names the rule so the two can be told apart at a glance.
				c.diag("rules/"+r.Name, "this rule was not evaluated: the report is silent about "+
					"what it covers, which is not the same as it being healthy", err)
			case len(breaches) == 0:
				res.Status = StatusOK
			default:
				res.Status = StatusBreached
				for _, b := range breaches {
					// Labels FIRST, and not only because the struct field order says so: LearnLabels
					// registers every identifier the breach carries so that Detail — a sentence, not
					// a field — has a substitution rule for each of them. A PVC name is the case
					// this exists for: it appears in a Detail and nowhere else in this report.
					c.red.LearnLabels(b.Labels)
					res.Breaches = append(res.Breaches, BreachView{
						Labels: c.red.Labels(b.Labels),
						Value:  b.Value,
						Detail: c.red.Detail(b.Detail),
					})
				}
			}
		}
		out = append(out, res)
	}
	return out
}

// promTemplate matches the Go-template placeholders Prometheus expands at fire time —
// `{{ $labels.namespace }}`, `{{ $value }}` — inside a rule's summary and description.
var promTemplate = regexp.MustCompile(`\{\{\s*\$(?:labels\.([A-Za-z_][A-Za-z0-9_]*)|value)\s*\}\}`)

// plainText turns an alert annotation into a sentence a human can read here.
//
// The annotations belong to Prometheus, which expands them against a firing alert instance. This
// report has no Prometheus, so copying them verbatim printed literal `{{ $labels.namespace }}` down
// the page — noise in the one place a reader is trying to understand a verdict, and it made the
// rendered report look broken.
//
// They are NEUTRALISED rather than expanded. Expanding would mean picking one breach's labels to
// stand for a rule that may have twenty, and a summary that silently describes the first namespace
// alphabetically is worse than one that describes the shape. The per-object answer is already in
// each breach's Detail, which is written for a human and not for a template engine.
func plainText(s string) string {
	return promTemplate.ReplaceAllStringFunc(s, func(m string) string {
		sub := promTemplate.FindStringSubmatch(m)
		if sub[1] == "" {
			return "<value>"
		}
		return "<" + sub[1] + ">"
	})
}

// thresholdView renders a rule's declared bound with its units named, and a sentence a reader can
// act on without opening the source.
func thresholdView(t alerts.Threshold) ThresholdView {
	v := ThresholdView{Kind: string(t.Kind)}
	switch t.Kind {
	case alerts.ThresholdState:
		v.Description = "boolean state: the rule fires on the state itself, there is no number"
	case alerts.ThresholdAge:
		v.AgeHours = t.Age.Hours()
		v.Description = fmt.Sprintf("staleness bound: %s", t.Age)
	case alerts.ThresholdCount:
		v.Count = t.Count
		v.Description = fmt.Sprintf("fires above %g", t.Count)
	case alerts.ThresholdPeriod:
		v.Factor = t.Factor
		v.GraceHours = t.Grace.Hours()
		v.AgeHours = t.Age.Hours()
		v.Description = fmt.Sprintf(
			"derived per schedule: %g x its own cron period + %s, falling back to %s when the cron cannot be parsed",
			t.Factor, t.Grace, t.Age)
	}
	return v
}

// verdictOf turns the per-rule statuses into the headline.
//
// NotEvaluated is carried INTO the summary sentence, not left in a footnote. An installation where
// most rules could not be evaluated has not been given a clean bill of health, and the word
// "healthy" on its own would be exactly the unearned green this report exists to avoid.
func verdictOf(rules []RuleResult) Verdict {
	v := Verdict{Status: verdictHealthy}
	for _, r := range rules {
		switch r.Status {
		case StatusOK:
			v.OK++
		case StatusBreached:
			v.Breached++
			if r.Severity == string(alerts.SeverityCritical) {
				v.Critical++
			}
		case StatusNotEvaluated:
			v.NotEvaluated++
		case StatusError:
			v.Errored++
		}
	}
	switch {
	case v.Critical > 0:
		v.Status = verdictUnhealthy
	case v.Breached > 0:
		v.Status = verdictDegraded
	}
	switch v.Status {
	case verdictUnhealthy:
		v.Summary = fmt.Sprintf("%d rule(s) breached, %d of them CRITICAL — restore capability is "+
			"compromised right now.", v.Breached, v.Critical)
	case verdictDegraded:
		v.Summary = fmt.Sprintf("%d rule(s) breached, none critical.", v.Breached)
	default:
		v.Summary = fmt.Sprintf("No rule breached among the %d evaluated.", v.OK)
	}
	if v.NotEvaluated > 0 || v.Errored > 0 {
		v.Summary += fmt.Sprintf(" %d rule(s) were NOT evaluated and %d errored — those are not passes.",
			v.NotEvaluated, v.Errored)
	}
	return v
}

// --- small helpers --------------------------------------------------------------------------

// conditionOf renders the Ready condition as "True (Reason)", which is more use in a report than
// either half alone.
func conditionOf(conds []metav1.Condition) string {
	for i := range conds {
		if conds[i].Type == conditionReady {
			return fmt.Sprintf("%s (%s)", conds[i].Status, conds[i].Reason)
		}
	}
	return ""
}

func timeOf(t *metav1.Time) *time.Time {
	if t == nil {
		return nil
	}
	at := t.UTC()
	return &at
}

// ageDays returns -1 for an absent timestamp so "never" is never rendered as "today".
func ageDays(now time.Time, t *metav1.Time) int {
	if t == nil {
		return -1
	}
	return days(now.Sub(t.Time))
}

func ageHours(now time.Time, t *metav1.Time) float64 {
	if t == nil {
		return -1
	}
	return now.Sub(t.Time).Round(time.Minute).Hours()
}

func days(d time.Duration) int { return int(d.Hours() / 24) }

func cronValid(expr, tz string) bool {
	_, err := schedule.Parse(expr, tz)
	return err == nil
}

func periodHours(expr, tz string, now time.Time) float64 {
	s, err := schedule.Parse(expr, tz)
	if err != nil {
		return 0
	}
	return s.Period(now).Round(time.Minute).Hours()
}

// sortByName orders a slice by a derived key so a report generated twice from unchanged state is
// byte-identical — the same reason the predicates sort their breaches.
func sortByName[T any](s []T, key func(T) string) {
	slices.SortStableFunc(s, func(a, b T) int { return cmp.Compare(key(a), key(b)) })
}
