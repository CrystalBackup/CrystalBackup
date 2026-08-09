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
	"context"
	"fmt"
	"sync"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
)

// This file is the reason the coverage scan is affordable, and it exists because the alternative was
// not.
//
// internal/exposer.Registry.For is written for ONE PVC at a time, in a reconcile, and it is exactly
// right for that: per call it GETs the PVC's PersistentVolume (or its StorageClass) and LISTs every
// VolumeSnapshotClass in the cluster. Called once per PVC over a cluster with three thousand of them,
// that is three thousand cluster-wide Lists and three thousand Gets — a self-check that takes minutes
// and hits the API server harder than the thing it is checking.
//
// The wrong fix is to look at the driver ourselves and skip the resolver. That is the one thing this
// section may not do (see coverage.go, and hack/gen-preflight-table's § "Why this is generated").
//
// The right fix is that the resolver never learns it is being called in a loop. coverageReader is a
// read-through client.Reader that answers the resolver's reads out of three collections it fetches
// ONCE, so a scan of N PVCs costs a constant number of requests:
//
//	1  List PersistentVolumeClaim   (cluster-wide, the census itself)
//	1  List Namespace               (the schedule fan-out)
//	1  List PersistentVolume        (serves every per-PVC Get)
//	1  List StorageClass            (serves the unbound-PVC Gets)
//	1  List VolumeSnapshotClass     (unstructured; served to every For call)
//	k  Get Secret                   (one per DISTINCT snapshotter Secret the pre-check looks up)
//
// = 5 + k requests for any N, where k is bounded by the number of VolumeSnapshotClasses and is 0 or 1
// on every cluster this project has met. Coverage.APIReads reports the measured figure rather than
// this comment's claim, because a claim in a comment is exactly the kind of thing that stops being
// true.
//
// # Secrets are the one thing that is never pre-listed
//
// Precheck reads a Secret by name, usually rook-ceph's, and those reads are MEMOISED per key —
// including the not-found answers, which is what keeps a broken class from costing one request per
// PVC. What this reader must never do is List Secrets to build that cache. Mirroring every Secret in
// the cluster into the operator's memory is precisely what invariant I3 exists to forbid, and it is
// why cmd/main.go builds the manager client with Cache.DisableFor(&corev1.Secret{}). A cache keyed on
// the names actually asked for holds only the handful of Secrets the snapshot classes name; a List
// would hold everybody's.

// coverageReader wraps the collector's own client.Reader and serves the exposer resolver's reads out
// of pre-fetched collections. It is single-use, built per scan, and its counters are the numbers
// Coverage.APIReads reports.
//
// The mutex is not for concurrency — the scan is sequential — but for the same reason the rest of
// this package is written the way it is: the value is handed to code outside this package
// (Registry.For, Precheck) which is free to change how it reads, and a lazily-populated cache that
// would race if that code ever grew a goroutine is a defect waiting for someone else's refactor.
type coverageReader struct {
	inner client.Reader

	mu sync.Mutex
	// reads is every request that actually reached the API server.
	reads int

	// pvs and storageClasses are name-indexed, fetched on first use. A nil map plus a true `loaded`
	// flag is a successful empty List; the error is retained so a second Get returns the same
	// failure instead of re-listing.
	pvs           map[string]*corev1.PersistentVolume
	pvsLoaded     bool
	pvsErr        error
	classes       map[string]*storagev1.StorageClass
	classesLoaded bool
	classesErr    error

	// snapClasses is the VolumeSnapshotClass list, fetched once and copied to every caller.
	snapClasses *unstructured.UnstructuredList
	snapLoaded  bool
	snapErr     error

	// secrets memoises Secret lookups by key, including the misses (a nil value with the key
	// present means "asked, and it is not there").
	secrets map[client.ObjectKey]*corev1.Secret
}

func newCoverageReader(inner client.Reader) *coverageReader {
	return &coverageReader{inner: inner, secrets: map[client.ObjectKey]*corev1.Secret{}}
}

// Reads is the measured request count.
func (r *coverageReader) Reads() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reads
}

// countedList delegates to the inner reader and counts the request. Every path that reaches the API
// server goes through it, so Coverage.APIReads cannot drift from what actually happened.
func (r *coverageReader) countedList(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	r.reads++
	return r.inner.List(ctx, list, opts...)
}

func (r *coverageReader) countedGet(ctx context.Context, key client.ObjectKey, obj client.Object) error {
	r.reads++
	return r.inner.Get(ctx, key, obj)
}

// primeSnapshotClasses fetches the VolumeSnapshotClass list up front and reports whether it can be
// read at all.
//
// The scan calls it BEFORE resolving anything, and gives up on the whole census when it fails. That
// is a deliberate choice against the more obvious one: letting each PVC fail individually would
// produce a report with one identical "cannot list VolumeSnapshotClasses" per volume, three thousand
// of them on a cluster with no snapshot CRDs installed — the commonest possible first-run state.
// One statement about the cluster is both smaller and truer than N statements about volumes.
func (r *coverageReader) primeSnapshotClasses(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, err := r.snapshotClasses(ctx)
	return err
}

func (r *coverageReader) snapshotClasses(ctx context.Context) (*unstructured.UnstructuredList, error) {
	if !r.snapLoaded {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(volumeSnapshotClassListGVK())
		r.snapErr = r.countedList(ctx, list)
		if r.snapErr == nil {
			r.snapClasses = list
		}
		r.snapLoaded = true
	}
	return r.snapClasses, r.snapErr
}

// volumeSnapshotClassListGVK mirrors internal/exposer's own unexported helper. It is three constant
// strings, and it is here rather than imported because exposing it from that package to satisfy a
// reporting path would widen an interface that has exactly one honest consumer. The GVK is pinned by
// TestCoverageReaderServesTheResolverList: if the resolver ever asks for a different one, the cache
// misses, the request count jumps and that test fails.
func volumeSnapshotClassListGVK() schema.GroupVersionKind {
	return schema.GroupVersionKind{
		Group:   snapshotGroup,
		Version: snapshotVersion,
		Kind:    kindSnapshotClass + "List",
	}
}

// Get serves the three object kinds the resolver reads, and delegates anything else.
//
// The delegation default is the important half: this wrapper's job is to make a known access pattern
// cheap, not to decide what the resolver is allowed to read. A future pre-check that reads a fourth
// kind gets a correct (if uncached) answer rather than a NotFound.
func (r *coverageReader) Get(
	ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption,
) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	switch out := obj.(type) {
	case *corev1.PersistentVolume:
		return r.getPV(ctx, key, out)
	case *storagev1.StorageClass:
		return r.getStorageClass(ctx, key, out)
	case *corev1.Secret:
		return r.getSecret(ctx, key, out)
	default:
		return r.countedGet(ctx, key, obj)
	}
}

func (r *coverageReader) getPV(ctx context.Context, key client.ObjectKey, out *corev1.PersistentVolume) error {
	if !r.pvsLoaded {
		var list corev1.PersistentVolumeList
		r.pvsErr = r.countedList(ctx, &list)
		if r.pvsErr == nil {
			r.pvs = make(map[string]*corev1.PersistentVolume, len(list.Items))
			for i := range list.Items {
				r.pvs[list.Items[i].Name] = &list.Items[i]
			}
		}
		r.pvsLoaded = true
	}
	if r.pvsErr != nil {
		return r.pvsErr
	}
	pv, ok := r.pvs[key.Name]
	if !ok {
		return apierrors.NewNotFound(
			schema.GroupResource{Resource: "persistentvolumes"}, key.Name)
	}
	// DEEP-copied into the caller's object rather than assigned. The resolver does not mutate what it
	// reads, but a cache that hands out its own storage is one refactor away from a scan that
	// corrupts its own inputs halfway through.
	pv.DeepCopyInto(out)
	return nil
}

func (r *coverageReader) getStorageClass(ctx context.Context, key client.ObjectKey, out *storagev1.StorageClass) error {
	if !r.classesLoaded {
		var list storagev1.StorageClassList
		r.classesErr = r.countedList(ctx, &list)
		if r.classesErr == nil {
			r.classes = make(map[string]*storagev1.StorageClass, len(list.Items))
			for i := range list.Items {
				r.classes[list.Items[i].Name] = &list.Items[i]
			}
		}
		r.classesLoaded = true
	}
	if r.classesErr != nil {
		return r.classesErr
	}
	sc, ok := r.classes[key.Name]
	if !ok {
		return apierrors.NewNotFound(
			schema.GroupResource{Group: storagev1.GroupName, Resource: "storageclasses"}, key.Name)
	}
	sc.DeepCopyInto(out)
	return nil
}

// getSecret memoises per key, MISSES INCLUDED. The miss is the case worth naming: a
// VolumeSnapshotClass pointing at a Secret that does not exist is the exact condition
// exposer.Precheck exists to catch, so on an affected cluster every single PVC's pre-check asks for
// the same absent object. Caching only the hits would leave the broken cluster paying one request per
// PVC — the one cluster where the scan most needs to be cheap.
func (r *coverageReader) getSecret(ctx context.Context, key client.ObjectKey, out *corev1.Secret) error {
	cached, asked := r.secrets[key]
	if !asked {
		var s corev1.Secret
		err := r.countedGet(ctx, key, &s)
		switch {
		case err == nil:
			cached = &s
		case apierrors.IsNotFound(err):
			cached = nil
		default:
			// Not memoised: a Forbidden or a timeout says something about our read rather than about
			// the cluster, and Precheck turns it into NOT_CHECKABLE. Caching it would freeze a
			// transient into the whole report.
			return err
		}
		r.secrets[key] = cached
	}
	if cached == nil {
		return apierrors.NewNotFound(schema.GroupResource{Resource: "secrets"}, key.Name)
	}
	cached.DeepCopyInto(out)
	return nil
}

// List serves the VolumeSnapshotClass list from the single pre-fetched copy and delegates everything
// else.
func (r *coverageReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	u, ok := list.(*unstructured.UnstructuredList)
	if !ok || u.GroupVersionKind() != volumeSnapshotClassListGVK() {
		return r.countedList(ctx, list, opts...)
	}
	cached, err := r.snapshotClasses(ctx)
	if err != nil {
		return err
	}
	u.Items = make([]unstructured.Unstructured, len(cached.Items))
	for i := range cached.Items {
		u.Items[i] = *cached.Items[i].DeepCopy()
	}
	return nil
}

// readOnlyClient adapts a client.Reader to the client.Client the exposer Registry is constructed
// with.
//
// Registry takes a Client because the exposers it hands back CREATE things; the two methods this
// package calls — Registry.For and SnapshotExposer.Precheck — only read, and both say so in their
// own documentation ("For remains PURE RESOLUTION", "Precheck ... is TOTAL"). This adapter is how
// that documented property is turned into a mechanical one: every write method returns
// errSelfcheckReadOnly, so a self-check can never create, patch or delete anything in somebody's
// cluster even if the code beneath it changes to want to.
//
// The alternative — panicking on a write — was rejected. A self-check that crashes the operator
// binary is worse than one that reports an error, and this way the failure arrives as a Diagnostic in
// the document instead of as a stack trace in a support ticket.
type readOnlyClient struct {
	client.Reader
	scheme *runtime.Scheme
}

// errSelfcheckReadOnly is what every mutating method returns. It names the command, because the
// message would otherwise reach an operator as an unexplained refusal from deep inside the exposer
// package.
var errSelfcheckReadOnly = fmt.Errorf(
	"selfcheck: this command is read-only and creates, patches or deletes nothing; " +
		"a code path asked it to write, which is a bug")

func (readOnlyClient) Create(context.Context, client.Object, ...client.CreateOption) error {
	return errSelfcheckReadOnly
}

func (readOnlyClient) Delete(context.Context, client.Object, ...client.DeleteOption) error {
	return errSelfcheckReadOnly
}

func (readOnlyClient) Update(context.Context, client.Object, ...client.UpdateOption) error {
	return errSelfcheckReadOnly
}

func (readOnlyClient) Patch(context.Context, client.Object, client.Patch, ...client.PatchOption) error {
	return errSelfcheckReadOnly
}

func (readOnlyClient) DeleteAllOf(context.Context, client.Object, ...client.DeleteAllOfOption) error {
	return errSelfcheckReadOnly
}

// Apply is server-side apply, the newest way controller-runtime offers to write. It is refused like
// every other write, and it is worth noting that the compiler is what forced this method to exist:
// widening client.Client broke this type's build rather than silently giving a self-check a way to
// mutate somebody's cluster. That is the property this adapter was chosen for.
func (readOnlyClient) Apply(context.Context, runtime.ApplyConfiguration, ...client.ApplyOption) error {
	return errSelfcheckReadOnly
}

func (c readOnlyClient) Status() client.SubResourceWriter { return readOnlySubResource{} }

func (c readOnlyClient) SubResource(string) client.SubResourceClient { return readOnlySubResource{} }

func (c readOnlyClient) Scheme() *runtime.Scheme { return c.scheme }

// RESTMapper returns nil, and that is not laziness: neither Registry.For nor Precheck consults one,
// and fabricating a mapper would mean either a network round-trip this command does not need or a
// hand-built lie about the cluster's resources. A caller that grows a dependency on it gets a nil
// dereference at its own call site, which is a far better bug report than a wrong answer.
func (readOnlyClient) RESTMapper() meta.RESTMapper { return nil }

func (c readOnlyClient) GroupVersionKindFor(obj runtime.Object) (schema.GroupVersionKind, error) {
	return apiutil.GVKForObject(obj, c.scheme)
}

// IsObjectNamespaced needs a RESTMapper, which this client does not have (see RESTMapper). It
// returns an error rather than a guess: the two plausible defaults are both wrong half the time, and
// nothing on the self-check's path asks.
func (readOnlyClient) IsObjectNamespaced(runtime.Object) (bool, error) {
	return false, errSelfcheckReadOnly
}

// readOnlySubResource refuses the status and scale writers for the same reason readOnlyClient refuses
// the main ones.
type readOnlySubResource struct{}

func (readOnlySubResource) Get(context.Context, client.Object, client.Object, ...client.SubResourceGetOption) error {
	return errSelfcheckReadOnly
}

func (readOnlySubResource) Create(
	context.Context, client.Object, client.Object, ...client.SubResourceCreateOption,
) error {
	return errSelfcheckReadOnly
}

func (readOnlySubResource) Update(context.Context, client.Object, ...client.SubResourceUpdateOption) error {
	return errSelfcheckReadOnly
}

func (readOnlySubResource) Patch(
	context.Context, client.Object, client.Patch, ...client.SubResourcePatchOption,
) error {
	return errSelfcheckReadOnly
}

func (readOnlySubResource) Apply(
	context.Context, runtime.ApplyConfiguration, ...client.SubResourceApplyOption,
) error {
	return errSelfcheckReadOnly
}
