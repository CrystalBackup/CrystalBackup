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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cbv1 "github.com/CrystalBackup/CrystalBackup/api/v1alpha1"
	"github.com/CrystalBackup/CrystalBackup/internal/apiconst"
	"github.com/CrystalBackup/CrystalBackup/internal/client/secrets"
	"github.com/CrystalBackup/CrystalBackup/internal/metrics"
	"github.com/CrystalBackup/CrystalBackup/internal/mover"
	"github.com/CrystalBackup/CrystalBackup/internal/nsselector"
	"github.com/CrystalBackup/CrystalBackup/internal/status"
	"github.com/CrystalBackup/CrystalBackup/internal/tracing"
	"go.opentelemetry.io/otel/attribute"
)

// clusterBackupPollInterval paces re-reconciles while a run is still Running. The child-Backup
// watch (see SetupWithManager) drives most re-aggregation the instant a child's status changes;
// this requeue is the watch-independent backstop so a run never stalls on a missed event.
const clusterBackupPollInterval = 15 * time.Second

// clusterBackupMessageCap bounds a FailureRecord/condition message so a pathological child error
// cannot bloat the ClusterBackup's status (which already caps the failures LIST via
// status.AppendCappedFailure); this caps each ENTRY's length.
const clusterBackupMessageCap = 256

// ClusterBackupReconciler reconciles a ClusterBackup: one cluster-DR RUN that fans a Backup out
// into every namespace its selector matches, then aggregates those children into a single bounded
// run status. It creates children but never OWNS them — a namespaced Backup cannot carry an
// ownerReference to a cluster-scoped ClusterBackup, and history GC must never cascade-delete a
// still-restorable child (apiconst) — so the parent→child link is the crystalbackup.io/cluster-backup
// label alone, and a label-based Backup watch (not Owns) re-aggregates the run when a child moves.
//
// It is the single writer of ClusterBackup.status: every status mutation happens in Reconcile.
// The child Backups own their OWN status and execution (internal/controller/backup_controller.go);
// this controller only reads them.
type ClusterBackupReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// OperatorNamespace is where the cluster-plane platform Secrets and mover Jobs live.
	OperatorNamespace string
	// Secrets, MoverImage, ManifestMoverServiceAccount and ClusterManifestReaderClusterRole drive
	// the run-level cluster-manifests capture (adr/0011 §1): the mover image and identity, the
	// DEK reader, and the enumerated ClusterRole the operator binds TRANSIENTLY for the capture.
	// The names are configured rather than derived — the chart release-prefixes them. An empty
	// reader role disables the capture, the same way the namespaced half handles an unconfigured
	// operator.
	Secrets       *secrets.ByNameReader
	MoverImage    string
	MoverProfiles mover.Profiles
	// MoverPlacement is the operator-wide scheduling policy carried by the cluster-manifests
	// capture Job, so that the one mover with API-server credentials is not also the one mover
	// running somewhere the administrator did not put the others.
	MoverPlacement                   mover.Placement
	ManifestMoverServiceAccount      string
	ClusterManifestReaderClusterRole string
	Recorder                         events.EventRecorder
}

// NewClusterBackupReconciler builds a ClusterBackupReconciler. Callers (main.go, the envtest
// suite) go through this constructor to keep the wiring in one place, mirroring the sibling
// reconcilers.
func NewClusterBackupReconciler(
	c client.Client,
	scheme *runtime.Scheme,
	operatorNamespace string,
	secretsReader *secrets.ByNameReader,
	moverImage string,
	moverProfiles mover.Profiles,
	moverPlacement mover.Placement,
	manifestMoverSA, clusterManifestReaderRole string,
	recorder events.EventRecorder,
) *ClusterBackupReconciler {
	return &ClusterBackupReconciler{
		Client:                           c,
		Scheme:                           scheme,
		OperatorNamespace:                operatorNamespace,
		Secrets:                          secretsReader,
		MoverImage:                       moverImage,
		MoverProfiles:                    moverProfiles,
		MoverPlacement:                   moverPlacement,
		ManifestMoverServiceAccount:      manifestMoverSA,
		ClusterManifestReaderClusterRole: clusterManifestReaderRole,
		Recorder:                         recorder,
	}
}

// +kubebuilder:rbac:groups=crystalbackup.io,resources=clusterbackups,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=crystalbackup.io,resources=clusterbackups/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=crystalbackup.io,resources=clusterbackuplocations,verbs=get;list;watch
// +kubebuilder:rbac:groups=crystalbackup.io,resources=backups,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=crystalbackup.io,resources=backuprepositories,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch

// Reconcile drives one ClusterBackup run: fail-fast on a missing location, resolve the namespace
// selector, ensure one child Backup per matched namespace (idempotent, label-linked), then
// aggregate the children into the run's status exactly once.
func (r *ClusterBackupReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var cb cbv1.ClusterBackup
	if err := r.Get(ctx, req.NamespacedName, &cb); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// The RUN's own trace anchor (spec/05-observability.md §5), and the `traceID` log key that
	// goes with it (§4). Every child Backup derives a LINK to this same UID, which is why the run
	// gets a root span of its own rather than being the parent of two hundred namespaces' spans.
	ctx, _ = traced(ctx, cb.UID)
	log := logf.FromContext(ctx)

	// A terminal run is frozen: a stray child-watch event on a finished run must not re-run it or
	// re-open its aggregate record. (First reconcile has an empty phase and falls through.)
	if isTerminalClusterBackupPhase(cb.Status.Phase) {
		return ctrl.Result{}, nil
	}

	// (1) Fail fast if the referenced ClusterBackupLocation is absent: do not fan children out into
	// N namespaces only to have every one of them gate on a location that does not exist.
	locName := cb.Spec.LocationRef.Name
	var loc cbv1.ClusterBackupLocation
	if err := r.Get(ctx, client.ObjectKey{Name: locName}, &loc); err != nil {
		if apierrors.IsNotFound(err) {
			return r.blocked(ctx, &cb, "LocationNotFound",
				fmt.Sprintf("ClusterBackupLocation %q not found", locName))
		}
		return ctrl.Result{}, fmt.Errorf("get ClusterBackupLocation %s: %w", locName, err)
	}

	// (2) Resolve the namespace selector against the live namespace set. A rule-8 / regexp error is
	// a spec fault (nsselector fails loudly rather than guess): surface it and refuse to fan out.
	var nsList corev1.NamespaceList
	if err := r.List(ctx, &nsList); err != nil {
		return ctrl.Result{}, fmt.Errorf("list namespaces: %w", err)
	}
	matched, err := nsselector.Match(nsList.Items, cb.Spec.Namespaces)
	if err != nil {
		return r.blocked(ctx, &cb, "SelectorInvalid", clampMessage(err.Error()))
	}

	// (3) Fan out: ensure one child Backup per matched namespace (idempotent, label-linked, no
	// ownerRef). A per-namespace create failure is recorded and does not abort the other namespaces.
	//
	// A run-name COLLISION is separated out from ordinary create failures because it changes what
	// the namespace's roll-up may say, not merely what the failure list contains: the coordinate is
	// occupied by a Backup this run did not write, so that Backup's status — which the aggregate
	// List will happily pick up, since a projection and a previous run's child both carry the
	// cluster-backup label — must never be counted as this run's result.
	var fanoutFailures []cbv1.FailureRecord
	collided := make(map[string]string)
	for _, ns := range matched {
		err := r.ensureChildBackup(ctx, &cb, ns)
		if err == nil {
			continue
		}
		if c := asRunNameCollision(err); c != nil {
			log.Error(err, "fan-out: run-name collision; namespace NOT backed up", "namespace", ns, "run", cb.Name)
			collided[ns] = clampMessage(c.Error())
			r.Recorder.Eventf(&cb, nil, corev1.EventTypeWarning, reasonRunNameCollision, "FanOut",
				"namespace %q was NOT backed up: %s", ns, c.Error())
			continue
		}
		log.Error(err, "fan-out: ensure child Backup failed", "namespace", ns, "run", cb.Name)
		fanoutFailures = append(fanoutFailures, cbv1.FailureRecord{
			Namespace: ns, Backup: cb.Name, Message: clampMessage(err.Error()),
		})
	}

	// (3.5) The run-level cluster-manifests capture (adr/0011 §1), driven INDEPENDENTLY of the
	// children — the two halves fail for unrelated reasons. Gated on matched being non-empty:
	// a misaimed selector terminates the run fast (below), and starting a capture Job we would
	// then strand mid-flight is exactly the orphaned-snapshot loss the namespaced half taught us
	// to avoid. Disabled, or its repository not yet ready, leaves captureDone at a safe value.
	captureDone := true
	teardownCluster := ""
	if len(matched) > 0 && captureClusterManifests(&cb) && r.ClusterManifestReaderClusterRole != "" {
		cc, ready, err := r.resolveClusterCaptureContext(ctx, &cb, &loc)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !ready {
			// The shared repository is not initialized yet, or its DEK is not resolvable. Hold
			// the run non-terminal and retry; the children gate on the same repository anyway.
			captureDone = false
		} else {
			done, teardownJob, err := r.advanceClusterManifests(ctx, &cb, cc)
			if err != nil {
				return ctrl.Result{}, err
			}
			captureDone, teardownCluster = done, teardownJob
		}
	}

	// (4) Aggregate the children into the run status and write it once. captureDone gates the
	// terminal phase: a run whose namespaces are all done but whose cluster capture is still
	// running must NOT go terminal, or the already-terminal guard at the top of Reconcile would
	// stop the pass that records the capture's snapshot.
	res, err := r.aggregateAndWrite(ctx, &cb, &loc, matched, fanoutFailures, collided, captureDone)
	if err != nil {
		return res, err
	}
	// The terminal result is durable: only now reclaim the capture's residue (its cluster-scoped
	// grant is a live read until this runs), and keep polling while the capture is in flight.
	if teardownCluster != "" {
		r.teardownClusterManifests(ctx, teardownCluster)
	}
	if !captureDone && res.IsZero() {
		res = ctrl.Result{RequeueAfter: clusterBackupPollInterval}
	}
	return res, nil
}

// resolveClusterCaptureContext resolves the repository coordinates the capture needs. ready is
// false (with a nil error) when the shared repository is not initialized yet or its DEK cannot be
// resolved — a transient not-ready, not a fault: the caller holds the run non-terminal and
// retries, exactly as the children do.
func (r *ClusterBackupReconciler) resolveClusterCaptureContext(
	ctx context.Context, cb *cbv1.ClusterBackup, loc *cbv1.ClusterBackupLocation,
) (*clusterCaptureContext, bool, error) {
	var repo cbv1.BackupRepository
	if err := r.Get(ctx, client.ObjectKey{Name: loc.Name}, &repo); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("get BackupRepository %s: %w", loc.Name, err)
	}
	if !repo.Status.Initialized {
		return nil, false, nil
	}
	dek, _, _, ok := resolvePlatformDEKCommon(ctx, r.Client, r.Secrets, r.OperatorNamespace, loc)
	if !ok {
		return nil, false, nil
	}
	return &clusterCaptureContext{
		clusterID:     loc.Spec.ClusterID,
		scheduleRef:   cb.Spec.ScheduleRef,
		run:           cb.Name,
		repoURL:       repo.Status.RepositoryURL,
		dek:           dek,
		s3CredsSecret: loc.Spec.S3.CredentialsSecretRef.Name,
		s3Connections: loc.Spec.S3.Connections,
		include:       cb.Spec.ClusterResources.Include,
		exclude:       cb.Spec.ClusterResources.Exclude,
	}, true, nil
}

// ensureChildBackup creates the run's child Backup in namespace ns if it does not already exist.
// The child is named after the run (the name equals the run tag in every namespace; the namespace
// disambiguates), linked to the run by the crystalbackup.io/cluster-backup label, marked
// cluster-origin (read-only to users, per RBAC), pointed at the run's ClusterBackupLocation, and
// STAMPED with this run's UID (apiconst.AnnotationParentUID). It carries NO ownerReference to the
// ClusterBackup. A child that is genuinely this run's is left untouched — it owns its own lifecycle.
//
// The UID stamp is what makes "already exists" answerable. The bare Get this used to do tested
// only the coordinate, and a discovery projection, a previous same-named run's terminal Backup, or
// a namespace-plane schedule's Backup all satisfy it — after which the run skipped the namespace
// and counted the stranger's Completed volumes as its own. Anything at the coordinate that is not
// this run's is a *runNameCollisionError, which the caller records as a per-namespace FAILURE.
func (r *ClusterBackupReconciler) ensureChildBackup(ctx context.Context, cb *cbv1.ClusterBackup, ns string) error {
	key := client.ObjectKey{Namespace: ns, Name: cb.Name}
	var existing cbv1.Backup
	if err := r.Get(ctx, key, &existing); err == nil {
		return r.resolveExistingChild(ctx, cb, &existing)
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get child Backup %s/%s: %w", ns, cb.Name, err)
	}

	child := &cbv1.Backup{
		ObjectMeta: metav1.ObjectMeta{
			Name:        cb.Name,
			Namespace:   ns,
			Labels:      childBackupLabels(cb, ns),
			Annotations: map[string]string{apiconst.AnnotationParentUID: string(cb.UID)},
		},
		Spec: cbv1.BackupSpec{
			ScheduleRef: cb.Spec.ScheduleRef,
			LocationRef: cbv1.LocationReference{
				Kind: kindClusterBackupLocation,
				Name: cb.Spec.LocationRef.Name,
			},
			// Materialize the run configuration into the child (adr/0017 §5) instead of leaving
			// it to be pulled back through the label at every reconcile. The child then executes
			// what this run declared AT FAN-OUT TIME, and keeps executing it after the parent has
			// been edited or garbage-collected — the link is a label on purpose, so nothing keeps
			// the parent alive for its children. DeepCopy, not a shared pointer: cb is a cache
			// object and the child is about to be serialized.
			Run: cb.Spec.BackupRunSpec.DeepCopy(),
		},
	}
	if err := r.Create(ctx, child); err != nil {
		if apierrors.IsAlreadyExists(err) {
			// The cached Get above missed it. That is USUALLY a lost race with a prior reconcile of
			// this same run — but it is equally how a stale cache reports a coordinate somebody else
			// already occupies, so re-read (the AlreadyExists proves the object is there now) and
			// classify instead of assuming the create was ours.
			var raced cbv1.Backup
			if gerr := r.Get(ctx, key, &raced); gerr != nil {
				return fmt.Errorf("re-read child Backup %s/%s after AlreadyExists: %w", ns, cb.Name, gerr)
			}
			return r.resolveExistingChild(ctx, cb, &raced)
		}
		return fmt.Errorf("create child Backup %s/%s: %w", ns, cb.Name, err)
	}
	r.Recorder.Eventf(cb, nil, corev1.EventTypeNormal, "FannedOut", "FanOut",
		"created child Backup in namespace %q", ns)
	return nil
}

// resolveExistingChild answers whether the Backup already sitting at this run's coordinate is the
// run's own child. It returns nil for "mine" (the ordinary idempotent second pass), nil after
// ADOPTING an unstamped in-flight child (the operator-upgrade path — see coordinateAdoptable), and
// a *runNameCollisionError for anything else.
func (r *ClusterBackupReconciler) resolveExistingChild(
	ctx context.Context, cb *cbv1.ClusterBackup, existing *cbv1.Backup,
) error {
	switch owner, detail := classifyCoordinate(existing, cb.UID); owner {
	case coordinateMine:
		return nil // idempotent: never mutate an existing child here.
	case coordinateAdoptable:
		// A pre-stamp build fanned this child out and the run is still in flight. Claim it with
		// the UID stamp so every later pass — and every later PROCESS — recognises it. This is the
		// single exception to "never mutate an existing child": it touches one annotation, never
		// spec or status, and only while the child holds no result of any kind.
		patch := client.MergeFrom(existing.DeepCopy())
		if existing.Annotations == nil {
			existing.Annotations = map[string]string{}
		}
		existing.Annotations[apiconst.AnnotationParentUID] = string(cb.UID)
		if err := r.Patch(ctx, existing, patch); err != nil {
			return fmt.Errorf("stamp parent UID on child Backup %s/%s: %w", existing.Namespace, existing.Name, err)
		}
		return nil
	default:
		return &runNameCollisionError{Namespace: existing.Namespace, Name: existing.Name, Detail: detail}
	}
}

// childBackupLabels are the labels every fanned-out child carries. crystalbackup.io/cluster-backup
// is the load-bearing parent link (the aggregate List selector, the child's own run resolution,
// and the crucible's cleanup all key off it); origin=cluster marks the child cluster-owned; the
// namespace and (optional) schedule labels mirror the restic tags for queryability.
func childBackupLabels(cb *cbv1.ClusterBackup, ns string) map[string]string {
	l := map[string]string{
		apiconst.LabelClusterBackup: cb.Name,
		apiconst.LabelOrigin:        apiconst.OriginCluster,
		apiconst.LabelNamespace:     ns,
	}
	if cb.Spec.ScheduleRef != "" {
		l[apiconst.LabelSchedule] = cb.Spec.ScheduleRef
	}
	return l
}

// aggregateAndWrite lists every child of the run in one label-scoped, cluster-wide call, folds the
// children into the run's counters/failures/phase, and writes the run status exactly once. The
// roll-up is driven off the MATCHED-namespace set (not merely the children found): a matched
// namespace whose child has not yet appeared in cache counts as in-flight, which keeps the run
// Running until every matched namespace has a reporting child.
func (r *ClusterBackupReconciler) aggregateAndWrite(
	ctx context.Context, cb *cbv1.ClusterBackup, loc *cbv1.ClusterBackupLocation, matched []string,
	fanoutFailures []cbv1.FailureRecord, collided map[string]string, captureDone bool,
) (ctrl.Result, error) {
	var children cbv1.BackupList
	if err := r.List(ctx, &children, client.MatchingLabels{apiconst.LabelClusterBackup: cb.Name}); err != nil {
		return ctrl.Result{}, fmt.Errorf("list child Backups for run %s: %w", cb.Name, err)
	}
	byNS := make(map[string]*cbv1.Backup, len(children.Items))
	for i := range children.Items {
		c := &children.Items[i]
		byNS[c.Namespace] = c
	}

	st := &cb.Status
	// Recompute every tally from scratch each pass so the aggregate is a pure function of the
	// current children (idempotent; no drift from a partial prior write).
	st.NamespacesMatched = int32(len(matched))
	st.NamespacesSucceeded = 0
	st.NamespacesFailed = 0
	st.PVCsSucceeded = 0
	st.PVCsFailed = 0
	st.AddedBytes = 0
	// The cluster-scoped capture's headline count. Recomputed from the durable snapshot record
	// each pass like every other tally, so it survives the from-scratch reset above; zero until
	// the capture completes (or when the run opted out).
	if cb.Status.ClusterManifests != nil {
		st.ClusterResourcesCaptured = cb.Status.ClusterManifests.ResourceCount
	} else {
		st.ClusterResourcesCaptured = 0
	}
	st.Failures = nil
	for _, f := range fanoutFailures {
		st.Failures = status.AppendCappedFailure(st.Failures, f, status.DefaultFailureCap)
	}

	childPhases := make([]string, 0, len(matched))
	for _, ns := range matched {
		// A namespace whose coordinate is occupied by a Backup this run did not create is a hard
		// per-namespace FAILURE, and its occupant's status is not this run's to read: the List above
		// picks such an occupant up (a projection and a previous run's child both carry the
		// cluster-backup label), so counting it would be exactly the false success this guard exists
		// to stop. Recorded before the child lookup so no volume of it is ever tallied.
		if msg, bad := collided[ns]; bad {
			childPhases = append(childPhases, string(status.BackupPhaseFailed))
			st.NamespacesFailed++
			st.Failures = status.AppendCappedFailure(st.Failures, cbv1.FailureRecord{
				Namespace: ns, Backup: cb.Name, Message: msg,
			}, status.DefaultFailureCap)
			continue
		}

		child := byNS[ns]
		if child == nil {
			childPhases = append(childPhases, "") // fanned out but not observed yet → in-flight.
			continue
		}
		// Defence in depth, independent of the collision detection above: a discovery PROJECTION is
		// a view of snapshots that already exist in the repository, derived by
		// discovery.VolumesFromSnapshots — every one of its volumes reads Completed by construction,
		// and none of them was written by this run. Even if the coordinate check upstream ever
		// regressed, a projection must not be able to increment pvcsSucceeded.
		if child.Annotations[apiconst.AnnotationProjected] == apiconst.AnnotationProjectedValue {
			childPhases = append(childPhases, string(status.BackupPhaseFailed))
			st.NamespacesFailed++
			st.Failures = status.AppendCappedFailure(st.Failures, cbv1.FailureRecord{
				Namespace: ns, Backup: child.Name,
				Message: clampMessage((&runNameCollisionError{
					Namespace: ns, Name: child.Name,
					Detail: "it is a discovery projection of snapshots already in the repository",
				}).Error()),
			}, status.DefaultFailureCap)
			continue
		}
		childPhases = append(childPhases, child.Status.Phase)

		for _, v := range child.Status.Volumes {
			switch v.Phase {
			case status.VolumePhaseCompleted:
				st.PVCsSucceeded++
			case status.VolumePhaseFailed:
				st.PVCsFailed++
			}
			st.AddedBytes += v.AddedBytes
		}

		switch child.Status.Phase {
		case string(status.BackupPhaseCompleted), string(status.BackupPhasePartiallyCompleted):
			st.NamespacesSucceeded++
		case string(status.BackupPhaseFailed), string(status.BackupPhasePartiallyFailed):
			st.NamespacesFailed++
			st.Failures = status.AppendCappedFailure(st.Failures, cbv1.FailureRecord{
				Namespace: ns, Backup: child.Name, Message: childFailureMessage(child),
			}, status.DefaultFailureCap)
		}
	}

	phase := status.RollUpBackupPhases(childPhases)
	switch {
	case len(matched) == 0:
		// A valid selector that matches no namespace has nothing to protect: terminate the run
		// (vacuously Completed) rather than hot-loop in Pending, but surface it so a misaimed
		// selector is diagnosable. The terminal guard then freezes it after this single event.
		// No cluster capture runs for such a run (gated on matched in Reconcile), so nothing is
		// stranded by terminating here.
		phase = status.ClusterBackupPhaseCompleted
		r.Recorder.Eventf(cb, nil, corev1.EventTypeWarning, "NoNamespacesMatched", "SelectNamespaces",
			"namespace selector matched no namespaces; nothing to back up")
	case !captureDone && isTerminalClusterBackupPhase(string(phase)):
		// Both halves gate the run. Every namespace is done but the cluster-scoped capture is
		// still in flight, so the run is not finished: hold it at Running. Letting it go terminal
		// here would trip the already-terminal guard at the top of Reconcile and the capture in
		// flight would never have its snapshot recorded — an orphaned kind=cluster-manifests
		// snapshot that no ClusterBackup points at.
		phase = status.ClusterBackupPhaseRunning
	}
	st.Phase = string(phase)

	if st.StartTime == nil {
		now := metav1.Now()
		st.StartTime = &now
	}
	terminal := isTerminalClusterBackupPhase(string(phase))
	// completionTime being unset is the durable "this run's terminal transition has not been
	// counted yet" marker, exactly as backupTime is for a Backup. aggregateAndWrite recomputes
	// every tally from scratch on each pass, so nothing else here distinguishes the first arrival.
	justTerminal := terminal && st.CompletionTime == nil
	if terminal && st.CompletionTime == nil {
		now := metav1.Now()
		st.CompletionTime = &now
	}
	setClusterBackupCondition(cb, phase)

	if err := r.Status().Update(ctx, cb); err != nil {
		return ctrl.Result{}, fmt.Errorf("update status for ClusterBackup %s: %w", cb.Name, err)
	}
	if justTerminal {
		// Run duration is fan-out start to every child settling — startTime, not the object's
		// creation, because a run that waited on a concurrencyPolicy or a gated location did not
		// spend that time moving data and folding it in would make the histogram describe queueing
		// rather than throughput.
		var duration time.Duration
		if st.StartTime != nil && st.CompletionTime != nil {
			duration = st.CompletionTime.Sub(st.StartTime.Time)
		}
		metrics.RecordClusterBackupTerminal(ctx, metrics.ClusterBackupSeries{
			Schedule: cb.Spec.ScheduleRef,
			Location: cb.Spec.LocationRef.Name,
			Cluster:  loc.Spec.ClusterID,
		}, string(phase), duration)
		emitClusterBackupRootSpan(ctx, cb, loc, phase, st)
	}
	if terminal {
		return ctrl.Result{}, nil
	}
	return ctrl.Result{RequeueAfter: clusterBackupPollInterval}, nil
}

// blocked records a non-terminal blocker (missing location, invalid selector) on the Ready
// condition, keeps the run Pending, and requeues on the fixable-fault cadence. It never fans out —
// the blocker must clear first (a spec edit re-triggers immediately via the generation change).
func (r *ClusterBackupReconciler) blocked(ctx context.Context, cb *cbv1.ClusterBackup, reason, message string) (ctrl.Result, error) {
	cb.Status.Phase = string(status.ClusterBackupPhasePending)
	status.SetCondition(&cb.Status.Conditions, ConditionReady, metav1.ConditionFalse, reason, message, cb.Generation)
	if err := r.Status().Update(ctx, cb); err != nil {
		return ctrl.Result{}, fmt.Errorf("update status for ClusterBackup %s: %w", cb.Name, err)
	}
	return ctrl.Result{RequeueAfter: shortRequeueInterval}, nil
}

// childFailureMessage extracts a short cause for a failed child from its Ready condition, falling
// back to the raw phase. Always clamped so one child cannot bloat the parent's status.
func childFailureMessage(child *cbv1.Backup) string {
	if c := status.FindCondition(child.Status.Conditions, ConditionReady); c != nil && c.Message != "" {
		return clampMessage(c.Message)
	}
	return clampMessage("backup phase " + child.Status.Phase)
}

// setClusterBackupCondition records the headline Ready condition from the aggregate phase: True
// once every namespace succeeded, False (with a distinguishing reason) while running or on any
// failure.
func setClusterBackupCondition(cb *cbv1.ClusterBackup, phase status.ClusterBackupPhase) {
	switch phase {
	case status.ClusterBackupPhaseCompleted:
		status.SetCondition(&cb.Status.Conditions, ConditionReady, metav1.ConditionTrue, "Completed",
			"all matched namespaces backed up", cb.Generation)
	case status.ClusterBackupPhaseFailed:
		status.SetCondition(&cb.Status.Conditions, ConditionReady, metav1.ConditionFalse, "Failed",
			"every matched namespace failed", cb.Generation)
	case status.ClusterBackupPhasePartiallyFailed:
		status.SetCondition(&cb.Status.Conditions, ConditionReady, metav1.ConditionFalse, "PartiallyFailed",
			"one or more matched namespaces failed", cb.Generation)
	default: // Pending, Running
		status.SetCondition(&cb.Status.Conditions, ConditionReady, metav1.ConditionFalse, "InProgress",
			"run in progress ("+string(phase)+")", cb.Generation)
	}
}

// isTerminalClusterBackupPhase reports whether a run has reached a final phase (no more work, no
// requeue). Mirrors isTerminalBackupPhase one level up.
func isTerminalClusterBackupPhase(phase string) bool {
	switch status.ClusterBackupPhase(phase) {
	case status.ClusterBackupPhaseCompleted,
		status.ClusterBackupPhaseFailed,
		status.ClusterBackupPhasePartiallyFailed:
		return true
	default:
		return false
	}
}

// clampMessage bounds a status message to clusterBackupMessageCap runes, appending an ellipsis
// when it truncates, so a pathological child error string cannot bloat the run status.
func clampMessage(s string) string {
	r := []rune(s)
	if len(r) <= clusterBackupMessageCap {
		return s
	}
	return string(r[:clusterBackupMessageCap-1]) + "…"
}

// SetupWithManager registers this reconciler. It reconciles ClusterBackups directly and, via a
// label-based mapping (NOT Owns — a namespaced child Backup cannot be owned by a cluster-scoped
// ClusterBackup), re-reconciles the run whenever one of its children changes. The
// clusterBackupPollInterval requeue is the watch-independent progress backstop.
func (r *ClusterBackupReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&cbv1.ClusterBackup{}).
		Watches(&cbv1.Backup{}, handler.EnqueueRequestsFromMapFunc(r.mapChildToRun)).
		Named("clusterbackup").
		Complete(r)
}

// mapChildToRun maps a child Backup back to its ClusterBackup run using only the child's labels:
// crystalbackup.io/origin=cluster gates it to cluster-owned children, and
// crystalbackup.io/cluster-backup names the run (a cluster-scoped object, so the request carries an
// empty namespace). A user-plane or unlabelled Backup maps to nothing.
func (r *ClusterBackupReconciler) mapChildToRun(_ context.Context, obj client.Object) []reconcile.Request {
	labels := obj.GetLabels()
	if labels[apiconst.LabelOrigin] != apiconst.OriginCluster {
		return nil
	}
	run := labels[apiconst.LabelClusterBackup]
	if run == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: run}}}
}

// emitClusterBackupRootSpan emits the RUN's root span (spec/05-observability.md §5): one span
// covering a whole fleet DR run, from fan-out start to the last child settling.
//
// It is a root of its OWN trace, and every per-namespace Backup carries a span LINK to it rather
// than being parented by it. That is the shape §5 specifies and the arithmetic is the reason: a
// nightly run over two hundred namespaces, each with a handful of PVCs, parents into a single
// trace of tens of thousands of spans — past what Tempo renders, well past what anyone reads, and
// it would bury the one namespace that failed among the hundred and ninety-nine that did not.
// Linked, each namespace gets a human-sized trace and the run stays one click away from all of
// them.
//
// The run's own span therefore has few children of its own: it is a fan-out marker carrying the
// aggregate verdict, which is exactly what a fleet operator opens it for.
func emitClusterBackupRootSpan(
	ctx context.Context, cb *cbv1.ClusterBackup, loc *cbv1.ClusterBackupLocation,
	phase status.ClusterBackupPhase, st *cbv1.ClusterBackupStatus,
) {
	anchor := tracing.AnchorFor(string(cb.UID))
	if !anchor.Valid() {
		return
	}
	attrs := make([]attribute.KeyValue, 0, 8)
	attrs = append(attrs,
		attribute.Int("crystalbackup.namespaces_matched", int(st.NamespacesMatched)),
		attribute.Int("crystalbackup.namespaces_failed", int(st.NamespacesFailed)))
	attrs = append(attrs, tracing.StringAttr(tracing.AttrClusterBackup, cb.Name)...)
	attrs = append(attrs, tracing.StringAttr(tracing.AttrSchedule, cb.Spec.ScheduleRef)...)
	attrs = append(attrs, tracing.StringAttr(tracing.AttrLocation, cb.Spec.LocationRef.Name)...)
	attrs = append(attrs, tracing.StringAttr(tracing.AttrCluster, loc.Spec.ClusterID)...)
	attrs = append(attrs, tracing.StringAttr(tracing.AttrOrigin, apiconst.OriginCluster)...)
	attrs = append(attrs, tracing.StringAttr(tracing.AttrPhase, string(phase))...)

	var spanErr error
	if st.NamespacesFailed > 0 {
		spanErr = fmt.Errorf("run %s ended %s: %d of %d namespace(s) failed",
			cb.Name, phase, st.NamespacesFailed, st.NamespacesMatched)
	}
	// startTime, not the object's creation, exactly as the run-duration histogram measures it: a
	// run that waited on a concurrencyPolicy or a gated location did not spend that time moving
	// data, and a span that included the wait would describe queueing rather than throughput.
	anchor.EmitRoot(ctx, "clusterbackup",
		timeOrZero(st.StartTime), timeOrZero(st.CompletionTime), spanErr, nil, attrs...)
}
