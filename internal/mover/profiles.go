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

package mover

import (
	"fmt"
	"os"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/yaml"
)

// ---------------------------------------------------------------------------
// The mover sizing table. THIS FILE IS THE ONLY PLACE THE NUMBERS EXIST.
//
// The chart carries no defaults (its `mover.profiles` map is empty and purely an override),
// docs/MOVER-RESOURCES.md is GENERATED from this table, and the operator logs the effective
// table at startup. A quantity written twice is a quantity that will disagree with itself.
// ---------------------------------------------------------------------------

// profileSpec is one cell of the built-in table: quantities as strings so the table above reads
// like the documentation it generates. Empty means "do not set this field on the pod at all",
// which is NOT the same as zero — see resolve.
type profileSpec struct {
	// class names the sizing class this cell came from. It carries no behaviour — it is what the
	// generated documentation groups operations by, so the table an operator reads and the table
	// the operator RUNS are the same object.
	class          string
	cpuRequest     string
	memoryRequest  string
	cpuLimit       string
	memoryLimit    string
	cacheSizeLimit string
}

// The four sizing classes. Every operation maps to exactly one of them, so an argument about
// sizing happens once per class rather than once per operation, and adding an operation is a
// choice between four documented shapes rather than five fresh numbers.
//
// WHAT THESE ARE NOT: benchmark output. The millions-of-files load test is roadmap M6's third
// deferred item and has NOT been run (it needs paid infrastructure). Requests are therefore
// deliberately modest — they are a scheduling floor multiplied by maxConcurrentMovers, not a
// reservation — and limits are deliberately generous: their job is to keep ONE runaway mover
// from taking a node down, not to right-size restic. Raise them; do not trust them.
//
// CPU LIMITS ARE UNSET ON PURPOSE. CPU is compressible: a mover that wants more than its share
// gets throttled by the scheduler's shares, and a hard CPU limit on a backup buys nothing but a
// slower backup and a support ticket that reads "it used to finish before the window closed".
// An operator who needs one sets `limits.cpu` per operation; nothing in the code assumes absence.
var (
	// classData — backup and restore. They stream a PVC through restic: CPU for chunking,
	// compression and encryption, memory for the repository index they hold while doing it.
	classData = profileSpec{
		class:      "data",
		cpuRequest: "200m", memoryRequest: "256Mi",
		memoryLimit: "4Gi", cacheSizeLimit: defaultCacheSizeLimit,
	}
	// classRepoHeavy — prune, check, sync. These are the roadmap's "prune > backup": prune
	// rewrites the repository's pack files and holds the whole index while deciding what is
	// unreferenced; check re-reads the structure; sync (`restic copy`) opens TWO repositories at
	// once and holds both indexes plus rclone's own buffers. All three are repository-wide work
	// whose cost tracks the repository, not the volume in front of them.
	classRepoHeavy = profileSpec{
		class:      "repo-heavy",
		cpuRequest: "500m", memoryRequest: "1Gi",
		memoryLimit: "8Gi", cacheSizeLimit: defaultCacheSizeLimit,
	}
	// classRepoLight — init, forget, snapshots, unlock. Index/metadata operations that read a
	// little and write less. forget without prune only rewrites snapshot files; snapshots lists
	// them; unlock deletes lock files; init writes a config. None of them touches pack data.
	classRepoLight = profileSpec{
		class:      "repo-light",
		cpuRequest: "50m", memoryRequest: "128Mi",
		memoryLimit: "1Gi", cacheSizeLimit: defaultCacheSizeLimit,
	}
	// classManifests — the four manifest shapes. They are the API-server operations (I6's sole
	// exception): the dump/apply half is a few MB of YAML, the restic half is a small tree, and
	// the memory floor is the client-go object cache for one namespace (or, cluster-scoped, one
	// cluster's CRDs and RBAC), which is why this class sits above classRepoLight rather than in it.
	classManifests = profileSpec{
		class:      "manifests",
		cpuRequest: "100m", memoryRequest: "256Mi",
		memoryLimit: "2Gi", cacheSizeLimit: defaultCacheSizeLimit,
	}
)

// defaultCacheSizeLimit caps the restic cache emptyDir (volumeCache) for EVERY operation, and it
// is the same number for all four classes on purpose.
//
// WHAT IT PREVENTS: the cache is a node-local emptyDir on the kubelet's root disk. Unbounded, a
// mover against a large repository can fill that disk, which does not fail the backup — it puts
// the NODE under DiskPressure and evicts other people's pods. Bounding it converts a node-wide
// incident into one failed, retryable backup.
//
// WHAT IT COSTS, AND THE FAILURE MODE THIS INTRODUCES: exceeding an emptyDir sizeLimit does not
// return ENOSPC to restic. The kubelet's eviction manager notices out of band (it polls, so the
// kill lands seconds after the breach) and EVICTS THE POD. The mover therefore dies without
// writing a termination message, exactly like an OOM kill — which, before this constant existed,
// the operator reported as the generic "MoverCrashed". That is the "mysterious pod disappearance"
// this knob would otherwise buy, so it is not shipped alone: readMoverResult now reads the pod's
// own eviction reason and the failure surfaces as MoverEvicted with the kubelet's message ("Usage
// of EmptyDir volume "restic-cache" exceeds the limit "20Gi""), naming the volume and the number.
//
// WHY 20Gi AND NOT SOMETHING TIGHTER: the cache is COLD ON EVERY RUN (a fresh emptyDir per pod,
// never reused), so its high-water mark is whatever ONE operation downloads — index, snapshots
// and the tree blobs it walks. That tracks repository size, and the load test that would give us
// the real curve has not been run. A tight limit would turn working backups into evictions mid-run,
// which is strictly worse than the unbounded cache it replaced; so this is deliberately a CEILING
// AGAINST A RUNAWAY, not a sizing estimate. It is chosen to be far above any cache a repository of
// the size this release is offered for will produce, and small enough to leave a 100Gi node disk
// standing. Set `cacheSizeLimit: "0"` to go back to unbounded.
//
// The /tmp and manifest-scratch emptyDirs stay UNBOUNDED. restic assembles pack files under
// /tmp, the ceiling there is a pack (tens of MB), and a second eviction trigger for no gain is
// a bad trade.
const defaultCacheSizeLimit = "20Gi"

// builtinSpecs assigns every Operation its class. It is exhaustive by test
// (TestEveryOperationHasABuiltinProfile parses the Operation constants out of mover.go), so a
// new operation cannot ship sized by accident.
var builtinSpecs = map[Operation]profileSpec{
	OpBackup:  classData,
	OpRestore: classData,

	OpPrune: classRepoHeavy,
	OpCheck: classRepoHeavy,
	OpSync:  classRepoHeavy,

	OpInit:      classRepoLight,
	OpForget:    classRepoLight,
	OpSnapshots: classRepoLight,
	OpUnlock:    classRepoLight,

	OpManifestsBackup:         classManifests,
	OpManifestsRestore:        classManifests,
	OpClusterManifestsBackup:  classManifests,
	OpClusterManifestsRestore: classManifests,
}

// DefaultProfileKey is the reserved key of the override map that applies to EVERY operation
// before its own entry does. It is not an operation name and never will be — the operations are
// restic verbs.
const DefaultProfileKey = "default"

// unboundedQuantity is the override value that REMOVES a field the built-in table sets: a limit
// an operator does not want, or the cache cap. "0" rather than "" because "" already means
// "inherit", and an operator who wants no memory limit must be able to say so without the chart
// having to know what the built-in was.
const unboundedQuantity = "0"

// Profile is one operation's resolved sizing: what BuildJob stamps on the container and on the
// cache volume. Nil/empty fields are omitted from the pod spec entirely.
type Profile struct {
	// Requests and Limits become the container's resources. A nil map is omitted.
	Requests corev1.ResourceList
	Limits   corev1.ResourceList
	// CacheSizeLimit becomes the restic cache emptyDir's sizeLimit. Nil means unbounded — the
	// pre-0.6.1 behaviour, reachable with `cacheSizeLimit: "0"`.
	CacheSizeLimit *resource.Quantity
}

// goMemLimitFraction is how much of the container's memory limit the Go runtime is told it may
// use. The remaining fifth is not slack: it covers what GOMEMLIMIT does not account for —
// goroutine stacks past the runtime's own bookkeeping, the mover shim's own small heap living
// alongside restic in the same cgroup, and anything the allocator has not returned to the OS yet.
const goMemLimitFraction = 0.8

// goMemLimitFloor is the derived value below which this is not set at all.
//
// The failure GOMEMLIMIT prevents is an OOM kill. The failure it CAUSES, when set below what the
// program actually needs live, is a GC death spiral: the runtime collects continuously, makes no
// progress, and the backup neither finishes nor fails. Go caps GC at 50% of CPU so it is not a
// hang, but a prune that takes four times as long and still gets killed is worse than one that is
// killed immediately with a legible reason.
//
// restic holds the repository index in memory, and that index grows with the repository rather
// than with the volume being backed up — so the floor is about the REPOSITORY, which the operator
// cannot see from here. 256Mi is chosen as the point below which restic on a non-trivial
// repository is more likely to thrash than to fit. No built-in profile comes near it (the
// smallest memory limit is repo-light's 1Gi, deriving 819MiB), so this only ever fires on an
// aggressive override — which is exactly the case where the operator has taken the decision away
// from us and a silent cap would be the wrong answer.
var goMemLimitFloor = resource.MustParse("256Mi")

// GoMemLimit is the value of the GOMEMLIMIT environment variable for this profile, or "" when it
// must not be set.
//
// It is DERIVED, never configured. GOMEMLIMIT and limits.memory are two statements about the same
// budget, and a knob that let an operator set them independently would let them disagree — the
// only interesting way for them to disagree being the one that kills a backup. It was a
// configurable field on JobRequest from M1 to 0.6.1 and no caller ever set it, so the operator
// shipped a spec-promised heap cap that was never once applied.
//
// Two cases yield "":
//
//   - No memory limit. `limits.memory: "0"` is how an operator says "do not cap this pod", and
//     capping its heap anyway would honour the letter of their override and invert its point.
//     Before 0.6.1 every mover was in this state, which is why nothing needed GOMEMLIMIT then.
//   - A derived value under goMemLimitFloor — see there.
//
// The value is floored to whole MiB and emitted with the unit rather than as raw bytes, because
// the first person to read it will be reading a pod spec during an incident.
//
// It reaches restic as well as the shim, since the child inherits the environment. That is the
// point: restic is what allocates. The shim execs it and waits on a small JSON result, so the two
// nominally claiming the same budget is arithmetic that never happens in practice.
func (p Profile) GoMemLimit() string {
	limit, ok := p.Limits[corev1.ResourceMemory]
	if !ok {
		return ""
	}
	mib := int64(float64(limit.Value()) * goMemLimitFraction / (1 << 20))
	if mib <= 0 {
		return ""
	}
	derived := resource.NewQuantity(mib*(1<<20), resource.BinarySI)
	if derived.Cmp(goMemLimitFloor) < 0 {
		return ""
	}
	return fmt.Sprintf("%dMiB", mib)
}

// Profiles is the resolved table the operator hands to every Job builder: built-in defaults with
// the platform's overrides already folded in, computed ONCE at startup so a malformed override
// fails the process rather than one backup at 3am.
//
// A nil Profiles is valid and yields the built-in table. That is what envtest and every unit test
// get, and it is why a controller that has not been wired to the operator's table still produces
// sized pods rather than BestEffort ones.
type Profiles map[Operation]Profile

// For returns the resolved profile for one operation, deep-copied so two Job specs can never
// share a ResourceList (the copyLabels argument, applied to sizing).
func (p Profiles) For(op Operation) Profile {
	if resolved, ok := p[op]; ok {
		return resolved.deepCopy()
	}
	// Either a nil table (tests, envtest, an unwired controller) or an operation the table does
	// not name. Both fall back to the built-in, which is exhaustive over the operations that
	// exist; an operation that is somehow neither gets classRepoLight rather than nothing,
	// because an unsized pod is the state this file exists to end.
	spec, ok := builtinSpecs[op]
	if !ok {
		spec = classRepoLight
	}
	profile, err := spec.resolve()
	if err != nil {
		// Unreachable: the table is constants and TestBuiltinProfilesParse proves they parse.
		panic(fmt.Sprintf("built-in mover profile for %q is malformed: %v", op, err))
	}
	return profile
}

// DefaultProfiles is the built-in table with no overrides applied — the operator's sizing when
// the platform sets nothing, and the input the docs generator renders.
func DefaultProfiles() Profiles {
	out := make(Profiles, len(builtinSpecs))
	for op, spec := range builtinSpecs {
		profile, err := spec.resolve()
		if err != nil {
			panic(fmt.Sprintf("built-in mover profile for %q is malformed: %v", op, err))
		}
		out[op] = profile
	}
	return out
}

// ResourceOverride is the requests/limits half of one YAML override entry. Empty string inherits
// the built-in; "0" removes the field.
type ResourceOverride struct {
	CPU    string `json:"cpu,omitempty"`
	Memory string `json:"memory,omitempty"`
}

// ProfileOverride is one entry of the platform's override file — the shape the chart's
// `mover.profiles.<operation>` values render into.
type ProfileOverride struct {
	Requests *ResourceOverride `json:"requests,omitempty"`
	Limits   *ResourceOverride `json:"limits,omitempty"`
	// CacheSizeLimit overrides the restic cache emptyDir cap; "0" makes it unbounded.
	CacheSizeLimit string `json:"cacheSizeLimit,omitempty"`
}

// LoadProfiles parses the platform's override file and folds it over the built-in table.
//
// It is STRICT on every axis a silent misconfiguration could hide in, because the one thing worse
// than a mover with the wrong limits is a knob an operator set, read back in `helm get values`,
// and that never reached a pod:
//
//   - an unknown YAML field is an error (yaml.UnmarshalStrict), so `cacheSizelimit:` does not
//     parse as nothing;
//   - an unknown top-level key is an error naming the valid ones, so `pruning:` cannot pass for
//     `prune`;
//   - an unparseable quantity is an error naming the operation and the field;
//   - a request above its own limit is an error HERE rather than a Job the API server rejects at
//     3am with the reason on an object nobody is watching.
//
// Empty input (no file, or a file the chart rendered from an empty values map) is not an error:
// it is the normal case and yields the built-in table.
func LoadProfiles(data []byte) (Profiles, error) {
	var overrides map[string]ProfileOverride
	if err := yaml.UnmarshalStrict(data, &overrides); err != nil {
		return nil, fmt.Errorf("parse mover profiles: %w", err)
	}

	for key := range overrides {
		if key == DefaultProfileKey {
			continue
		}
		if _, ok := builtinSpecs[Operation(key)]; !ok {
			return nil, fmt.Errorf("mover profile %q names no operation; valid keys are %s and %s",
				key, DefaultProfileKey, strings.Join(operationNames(), ", "))
		}
	}

	out := make(Profiles, len(builtinSpecs))
	for op, spec := range builtinSpecs {
		merged := spec
		if def, ok := overrides[DefaultProfileKey]; ok {
			merged = merged.apply(def)
		}
		if own, ok := overrides[string(op)]; ok {
			merged = merged.apply(own)
		}
		profile, err := merged.resolve()
		if err != nil {
			return nil, fmt.Errorf("mover profile %q: %w", op, err)
		}
		out[op] = profile
	}
	return out, nil
}

// LoadProfilesFile is LoadProfiles over a path, with the two non-error cases spelled out: an empty
// path (nobody configured overrides) and a MISSING file both yield the built-in table.
//
// Missing-is-fine is deliberate and narrow. The chart always renders the ConfigMap, so on a Helm
// install the file is always there; the case this tolerates is the kustomize/`make deploy` install
// that passes no ConfigMap at all. A file that EXISTS and is broken is still fatal — the operator
// must never start on half an override.
func LoadProfilesFile(path string) (Profiles, error) {
	if path == "" {
		return DefaultProfiles(), nil
	}
	data, err := os.ReadFile(path) // #nosec G304 -- an operator flag, not user input
	if err != nil {
		if os.IsNotExist(err) {
			return DefaultProfiles(), nil
		}
		return nil, fmt.Errorf("read mover profiles %s: %w", path, err)
	}
	return LoadProfiles(data)
}

// apply folds one override entry over a spec, field by field. FIELD-BY-FIELD IS THE POINT: an
// operator raising prune's memory limit must not silently drop prune's CPU request, which is what
// a whole-object replacement would do and what makes "I only changed one line" a debugging session.
func (s profileSpec) apply(o ProfileOverride) profileSpec {
	if o.Requests != nil {
		if o.Requests.CPU != "" {
			s.cpuRequest = o.Requests.CPU
		}
		if o.Requests.Memory != "" {
			s.memoryRequest = o.Requests.Memory
		}
	}
	if o.Limits != nil {
		if o.Limits.CPU != "" {
			s.cpuLimit = o.Limits.CPU
		}
		if o.Limits.Memory != "" {
			s.memoryLimit = o.Limits.Memory
		}
	}
	if o.CacheSizeLimit != "" {
		s.cacheSizeLimit = o.CacheSizeLimit
	}
	return s
}

// resolve turns the string cell into the typed Profile, dropping every field whose quantity is
// empty (never set) or zero (explicitly removed by an override).
func (s profileSpec) resolve() (Profile, error) {
	requests, err := resourceList(s.cpuRequest, s.memoryRequest, "requests")
	if err != nil {
		return Profile{}, err
	}
	limits, err := resourceList(s.cpuLimit, s.memoryLimit, "limits")
	if err != nil {
		return Profile{}, err
	}
	for _, name := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
		request, hasRequest := requests[name]
		limit, hasLimit := limits[name]
		if hasRequest && hasLimit && request.Cmp(limit) > 0 {
			return Profile{}, fmt.Errorf("requests.%s (%s) exceeds limits.%s (%s)",
				name, request.String(), name, limit.String())
		}
	}

	profile := Profile{Requests: requests, Limits: limits}
	cache, err := optionalQuantity(s.cacheSizeLimit, "cacheSizeLimit")
	if err != nil {
		return Profile{}, err
	}
	profile.CacheSizeLimit = cache
	return profile, nil
}

// resourceList builds one half of the ResourceRequirements, or nil when neither quantity is set —
// nil so the serialized pod spec carries no empty `requests: {}`.
func resourceList(cpu, memory, half string) (corev1.ResourceList, error) {
	out := corev1.ResourceList{}
	if q, err := optionalQuantity(cpu, half+".cpu"); err != nil {
		return nil, err
	} else if q != nil {
		out[corev1.ResourceCPU] = *q
	}
	if q, err := optionalQuantity(memory, half+".memory"); err != nil {
		return nil, err
	} else if q != nil {
		out[corev1.ResourceMemory] = *q
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// optionalQuantity parses one cell: "" and "0" both mean "omit the field", every other value must
// be a valid Kubernetes quantity or the caller fails loudly.
func optionalQuantity(value, field string) (*resource.Quantity, error) {
	if value == "" || value == unboundedQuantity {
		return nil, nil
	}
	q, err := resource.ParseQuantity(value)
	if err != nil {
		return nil, fmt.Errorf("%s: %q is not a valid quantity: %w", field, value, err)
	}
	if q.Sign() < 0 {
		return nil, fmt.Errorf("%s: %q is negative", field, value)
	}
	return &q, nil
}

// deepCopy so a Job spec can never share a map with the table every other Job is built from.
func (p Profile) deepCopy() Profile {
	out := Profile{}
	if p.Requests != nil {
		out.Requests = p.Requests.DeepCopy()
	}
	if p.Limits != nil {
		out.Limits = p.Limits.DeepCopy()
	}
	if p.CacheSizeLimit != nil {
		cache := p.CacheSizeLimit.DeepCopy()
		out.CacheSizeLimit = &cache
	}
	return out
}

// operationNames lists the operations the override file may name, sorted — for the error message
// an operator reads after a typo, and for the docs generator's row order.
func operationNames() []string {
	names := make([]string, 0, len(builtinSpecs))
	for op := range builtinSpecs {
		names = append(names, string(op))
	}
	slices.Sort(names)
	return names
}

// Operations returns every operation the sizing table covers, sorted. Exported for the docs
// generator and the operator's startup log.
func Operations() []Operation {
	names := operationNames()
	out := make([]Operation, 0, len(names))
	for _, name := range names {
		out = append(out, Operation(name))
	}
	return out
}

// ClassOf names the sizing class an operation belongs to, and reports whether the operation is
// one this table knows at all.
//
// It exists for internal/soak, which measures peak memory per CLASS rather than per operation:
// the four classes are where the sizing argument actually happens, and a curve for `prune`
// separate from one for `check` would be two thin samples of the same 8Gi decision instead of one
// good one. The `ok` return is what keeps a class from being GUESSED — a pod whose operation
// cannot be recovered is filed as unknown, because a `data` peak reported under `repo-heavy`
// would be read as evidence that prune's limit is right when it is evidence about backup's.
func ClassOf(op Operation) (string, bool) {
	spec, ok := builtinSpecs[op]
	if !ok {
		return "", false
	}
	return spec.class, true
}

// Classes returns the four sizing classes, sorted. The soak's high-water table is keyed on them
// and has to be able to say "this class had no movers at all" rather than omitting the row.
func Classes() []string {
	seen := map[string]bool{}
	out := make([]string, 0, 4)
	for _, spec := range builtinSpecs {
		if !seen[spec.class] {
			seen[spec.class] = true
			out = append(out, spec.class)
		}
	}
	slices.Sort(out)
	return out
}

// String renders one profile as the operator logs it and as the generated docs table shows it —
// one definition so the log line and the documentation cannot describe different pods.
func (p Profile) String() string {
	parts := []string{
		"requests=" + resourceListString(p.Requests),
		"limits=" + resourceListString(p.Limits),
		"cacheSizeLimit=" + quantityString(p.CacheSizeLimit),
	}
	return strings.Join(parts, " ")
}

func resourceListString(list corev1.ResourceList) string {
	if len(list) == 0 {
		return "-"
	}
	cpu, hasCPU := list[corev1.ResourceCPU]
	memory, hasMemory := list[corev1.ResourceMemory]
	parts := make([]string, 0, 2)
	if hasCPU {
		parts = append(parts, "cpu="+cpu.String())
	}
	if hasMemory {
		parts = append(parts, "memory="+memory.String())
	}
	return strings.Join(parts, ",")
}

func quantityString(q *resource.Quantity) string {
	if q == nil {
		return "unbounded"
	}
	return q.String()
}
