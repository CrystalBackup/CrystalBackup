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
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// This file is the ACTIVE half of exposure admission.
//
// Registry.For already answers a PASSIVE question — "does a VolumeSnapshotClass exist for this
// PVC's driver at all?" — and a "no" there means Skipped/CSISnapshotUnsupported (ADR 0003). That
// answer is cheap and structural, and it is not enough. A VolumeSnapshotClass can exist, name a
// live driver, and STILL be unable to produce a snapshot, because the credentials it points the
// snapshotter sidecar at are not there. What the operator then does, without this file, is
// create a VolumeSnapshot that nobody can serve and wait — the backup does not fail, it HANGS,
// and it hangs in a phase (Snapshotting) that looks like ordinary progress. A run that never
// finishes is worse than a run that fails: nothing alerts, and the residue is a live
// VolumeSnapshot per volume per attempt.
//
// So before a single object is created, Precheck asks the one question that is answerable in a
// single API read and whose "no" is definitive: does the Secret the class names actually exist?
//
// # What is deliberately NOT here
//
// Trash monitoring (`rbd trash ls`) and VolumeSnapshotContent ⇄ RBD-image reconciliation
// (`rbd snap ls`) were considered for this same pass and REJECTED. Both need Ceph credentials in
// the backup system, and ADR 0003 accepts the opposite as a consequence of its whole design:
// "No Ceph credentials anywhere in the backup system; movers need network access to S3 only".
// The ADR's own risk table already assigns that side of the problem to a platform alert on
// ceph-mgr (`ceph rbd task list` flatten backlog) — i.e. to the party that already holds the
// keys. Adding a Ceph key slot here to gain a monitoring signal would trade the ADR's central
// security property for an observation somebody else is better placed to make.
//
// # NOT_CHECKABLE is a first-class verdict
//
// The CSI spec lets a VolumeSnapshotClass name its Secret through TEMPLATES resolved per-snapshot
// by the sidecar (`${volumesnapshot.namespace}`, `${volumesnapshotcontent.name}`, …). When it
// does, no Secret name exists to look up ahead of time, and the honest answer is neither "pass"
// nor "fail": it is "this cannot be determined". That is the same discipline
// internal/soak/highwater.go's MeasuredValue enforces on every number that could be absent, for
// the same reason it gives — this project has been bitten five times by an absence reading as
// health, and a pre-check that silently returns "OK" for every templated class would be a green
// light that means nothing at all on exactly the clusters that use one.
//
// A NOT_CHECKABLE check does not block the backup (there is nothing to block it ON), but it is
// recorded in Checks so the verdict a reader sees is "not verified", never "verified good".

// ErrPrecheckFailed is the sentinel a failed pre-check surfaces as, beside ErrUnsupported, so the
// Backup controller keeps ONE errors.Is discrimination over the exposure-admission answers rather
// than two parallel verdict shapes:
//
//	ErrUnsupported     -> Skipped / CSISnapshotUnsupported   (this volume can never be snapshotted)
//	ErrPrecheckFailed  -> Failed  / SnapshotPrecheckFailed   (this cluster cannot snapshot it TODAY)
//	anything else      -> a hard reconcile error, retried with backoff
//
// The distinction between the first two is not cosmetic: Skipped is neutral in the phase roll-up
// (a permanently unsnapshottable PVC must not alarm forever), while a broken snapshotter IS a
// degradation somebody has to fix.
var ErrPrecheckFailed = errors.New("exposer: snapshot pre-check failed")

const (
	// snapshotterSecretNameParam / snapshotterSecretNamespaceParam are the two VolumeSnapshotClass
	// parameters the external-snapshotter sidecar resolves its CSI credentials from before calling
	// CreateSnapshot. They are the CSI-standard keys (not a Ceph-ism): ceph-csi sets them, and so
	// does every other driver that authenticates its snapshot calls — see the crucible's own
	// VolumeSnapshotClasses in test/crucible/deploy/manifests/ceph-storage.yaml.
	snapshotterSecretNameParam      = "csi.storage.k8s.io/snapshotter-secret-name"
	snapshotterSecretNamespaceParam = "csi.storage.k8s.io/snapshotter-secret-namespace"

	// csiTemplateMarker opens a CSI parameter template. The sidecar substitutes
	// ${volumesnapshot.name}, ${volumesnapshot.namespace}, ${volumesnapshotcontent.name} and the
	// annotation forms at snapshot time, so a value containing it names a DIFFERENT Secret per
	// snapshot and cannot be resolved from the class alone. Matching on the opening marker rather
	// than on a list of known variables is deliberate: an unknown template variable must also come
	// out NOT_CHECKABLE, and a list would quietly classify it as a literal name and report FAILED
	// on a cluster that is perfectly healthy.
	csiTemplateMarker = "${"
)

// checkSnapshotterSecret is the Check.Name of the one check this file performs. It is a stable
// string: it reaches an operator through the Backup's Event and through the crucible's assertions.
const checkSnapshotterSecret = "SnapshotterSecret"

// reasonSnapshotterSecretMissing is the PrecheckResult.Reason for the one failure this file can
// return. It is finer-grained than the controller's per-volume reason (SnapshotPrecheckFailed) on
// purpose — the volume phase says WHICH GATE refused, this says WHY.
const reasonSnapshotterSecretMissing = "SnapshotterSecretMissing"

// CheckStatus is one check's verdict. Three values, not two, because "could not be determined" is
// a real outcome that must never be collapsed into either of the others (see this file's header).
type CheckStatus string

const (
	// CheckOK — the precondition was verified present.
	CheckOK CheckStatus = "OK"
	// CheckFailed — the precondition was verified ABSENT. This is the only status that blocks an
	// exposure, and every check that can return it must be STRUCTURAL: a condition that a retry
	// cannot fix, because the caller turns it into a terminal per-volume phase, not a requeue.
	CheckFailed CheckStatus = "FAILED"
	// CheckNotCheckable — the precondition could not be evaluated (CSI templating, an unreadable
	// parameter, an API error on the read itself). Never blocks; always recorded.
	CheckNotCheckable CheckStatus = "NOT_CHECKABLE"
)

// Check is one precondition and what became of it. Detail is a full human sentence rather than a
// code, because it is what lands in the Warning Event an operator reads at 3am — it must name the
// object that is missing and the class that asked for it, without a second lookup.
type Check struct {
	Name   string
	Status CheckStatus
	Detail string
}

// PrecheckResult is the verdict over all checks. OK is false ONLY when some check came back
// FAILED: a NOT_CHECKABLE check leaves OK true, because refusing to back a volume up over a
// question we could not ask would turn every templated-Secret cluster into a permanent outage.
// Reason and Message are populated only when OK is false (Reason a stable code, Message the
// failing check's Detail).
type PrecheckResult struct {
	OK      bool
	Reason  string
	Message string
	Checks  []Check
}

// Err renders a failed result as an error wrapping ErrPrecheckFailed, and nil when OK. This is the
// bridge between the STRUCTURED verdict (which Precheck must produce — a total function with no
// error return, so no failure mode can hide in an err path) and the controller's single
// errors.Is-discriminated error funnel.
func (r PrecheckResult) Err() error {
	if r.OK {
		return nil
	}
	return &PrecheckError{Result: r}
}

// PrecheckError carries a failed PrecheckResult as an error. It unwraps to ErrPrecheckFailed for
// errors.Is, and remains reachable by errors.As for a caller that wants the individual Checks
// rather than the rendered sentence.
type PrecheckError struct {
	Result PrecheckResult
}

func (e *PrecheckError) Error() string {
	return fmt.Sprintf("%s: %s", ErrPrecheckFailed.Error(), e.Result.Message)
}

// Unwrap makes errors.Is(err, ErrPrecheckFailed) true.
func (e *PrecheckError) Unwrap() error { return ErrPrecheckFailed }

// Precheck evaluates the cluster-side preconditions for taking a snapshot on vsc and returns the
// verdict. It is TOTAL — it returns no error, ever. Every way of not knowing is a check status, so
// there is no err path a caller could accidentally read as "fine".
//
// c must be a client whose Secret reads are UNCACHED. In the operator that holds by construction:
// cmd/main.go builds the manager client with Cache.DisableFor containing &corev1.Secret{}, so the
// Get below is a direct API read that opens no informer — which is the whole reason this check can
// afford to look at an arbitrary namespace's Secret (rook-ceph's, typically) with the cluster-wide
// `get` the chart and config/rbac/role.yaml already grant. A cached client here would instead
// start a cluster-wide Secret informer, i.e. mirror every Secret in the cluster into the
// operator's memory — the exact thing invariant I3 exists to prevent.
func Precheck(ctx context.Context, c client.Client, vsc *unstructured.Unstructured) PrecheckResult {
	if vsc == nil {
		// Unreachable through Registry.For (which only ever hands over a class it just read), so
		// this is not a real cluster state — it is a programming error. It still gets a verdict
		// rather than a panic, and specifically the verdict that CANNOT be mistaken for health.
		return notCheckableResult(checkSnapshotterSecret,
			"no VolumeSnapshotClass was resolved, so its snapshotter Secret cannot be identified")
	}
	className := vsc.GetName()

	namespace, name, templated := snapshotterSecretRef(vsc)
	switch {
	case templated:
		return notCheckableResult(checkSnapshotterSecret, fmt.Sprintf(
			"VolumeSnapshotClass %q names its snapshotter Secret in a form that cannot be resolved "+
				"ahead of time — CSI templating (a different Secret per snapshot), or a parameter "+
				"that is not a plain string: %s=%q, %s=%q",
			className, snapshotterSecretNamespaceParam, namespace, snapshotterSecretNameParam, name))

	case namespace == "" && name == "":
		// A class that names no snapshotter Secret is not an absence we are guessing about: it is
		// a positive statement that this driver authenticates its snapshot calls some other way
		// (or not at all). There is nothing to verify, and nothing missing.
		return okResult(checkSnapshotterSecret, fmt.Sprintf(
			"VolumeSnapshotClass %q declares no snapshotter Secret", className))

	case namespace == "" || name == "":
		// Half a reference. The sidecar would almost certainly reject this, so FAILED is tempting
		// — and wrong. This check's false-positive budget is zero (it FAILS a backup), and a
		// half-specified pair is a shape we have never observed in the field, so we would be
		// betting a customer's backup on a guess about a driver we have not tested. Refusing to
		// rule is the only answer that cannot be wrong.
		return notCheckableResult(checkSnapshotterSecret, fmt.Sprintf(
			"VolumeSnapshotClass %q specifies only half of its snapshotter Secret reference "+
				"(%s=%q, %s=%q); no Secret key can be formed from it",
			className, snapshotterSecretNamespaceParam, namespace, snapshotterSecretNameParam, name))
	}

	var secret corev1.Secret
	err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &secret)
	switch {
	case err == nil:
		return okResult(checkSnapshotterSecret, fmt.Sprintf(
			"snapshotter Secret %s/%s for VolumeSnapshotClass %q exists", namespace, name, className))

	case apierrors.IsNotFound(err):
		return failedResult(checkSnapshotterSecret, reasonSnapshotterSecretMissing, fmt.Sprintf(
			"VolumeSnapshotClass %q names snapshotter Secret %s/%s, which does not exist; "+
				"the CSI snapshotter cannot authenticate and any VolumeSnapshot on this class "+
				"would never become ready",
			className, namespace, name))

	default:
		// Forbidden, a timeout, an apiserver having a bad minute. This says something about OUR
		// read, not about the cluster's ability to snapshot, and reading it as FAILED would fail
		// backups over a transient. Not checkable, and say why.
		return notCheckableResult(checkSnapshotterSecret, fmt.Sprintf(
			"snapshotter Secret %s/%s for VolumeSnapshotClass %q could not be read: %v",
			namespace, name, className, err))
	}
}

// snapshotterSecretRef extracts the snapshotter Secret reference a VolumeSnapshotClass declares.
// Pure and table-testable: it performs no I/O and reads nothing but vsc.parameters.
//
// templated reports that the class DOES name a Secret but not in a form this code can resolve to a
// concrete object — either CSI templating (a `${…}` substitution the sidecar performs per
// snapshot) or a parameter value that is not a plain string at all. The returned namespace/name are
// still the raw values in that case, so a caller can quote them back to the operator; what
// templated forbids is treating them as an object key. Both halves are trimmed, so a class whose
// parameter is present but blank reads as absent rather than as a reference to "".
func snapshotterSecretRef(vsc *unstructured.Unstructured) (namespace, name string, templated bool) {
	if vsc == nil {
		return "", "", false
	}
	params, found, err := unstructured.NestedMap(vsc.Object, "parameters")
	if err != nil || !found {
		// `parameters` absent, or not a map at all. Absent is the common, healthy case (a class
		// with no credentials); a non-map is malformed. Neither yields a reference, and the
		// caller's "no Secret declared" branch covers both — there is no Secret NAME here to
		// wrongly declare missing.
		return "", "", false
	}

	namespace, nsOK := paramString(params, snapshotterSecretNamespaceParam)
	name, nameOK := paramString(params, snapshotterSecretNameParam)

	// A present-but-unreadable value (not a string) is not statically knowable either, and it must
	// not silently degrade to "no Secret declared" — that is the absence-reads-as-health failure
	// this whole file is built against.
	unreadable := !nsOK || !nameOK
	templated = strings.Contains(namespace, csiTemplateMarker) ||
		strings.Contains(name, csiTemplateMarker) ||
		unreadable

	return namespace, name, templated
}

// paramString reads one parameter as a trimmed string. ok is false when the key is present with a
// non-string value — distinct from the key being absent, which returns ("", true): "not set" and
// "set to something we cannot read" are different facts and this file never conflates them.
func paramString(params map[string]any, key string) (string, bool) {
	raw, present := params[key]
	if !present || raw == nil {
		return "", true
	}
	s, isString := raw.(string)
	if !isString {
		return fmt.Sprintf("%v", raw), false
	}
	return strings.TrimSpace(s), true
}

// okResult / notCheckableResult / failedResult build the single-check verdicts Precheck returns.
// One check exists today; the slice is there so a second one can be added without changing the
// value's shape — at which point these three grow into an aggregator whose rule is already fixed
// by PrecheckResult's doc: any FAILED makes OK false, NOT_CHECKABLE never does.
func okResult(name, detail string) PrecheckResult {
	return PrecheckResult{OK: true, Checks: []Check{{Name: name, Status: CheckOK, Detail: detail}}}
}

// The name parameter looks inert today and is kept deliberately: it is what makes these three
// helpers one shape rather than three, and the second check will pass a different one. Same
// judgement, same annotation as internal/controller/restore_resources.go's classMapping.
func notCheckableResult(name, detail string) PrecheckResult { //nolint:unparam // one check today; the trio stays symmetric for the second
	return PrecheckResult{OK: true, Checks: []Check{{Name: name, Status: CheckNotCheckable, Detail: detail}}}
}

func failedResult(name, reason, detail string) PrecheckResult {
	return PrecheckResult{
		OK:      false,
		Reason:  reason,
		Message: detail,
		Checks:  []Check{{Name: name, Status: CheckFailed, Detail: detail}},
	}
}
