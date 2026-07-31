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
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// TestEveryOperationHasABuiltinProfile reads the Operation constants OUT OF THE SOURCE and checks
// the sizing table names every one of them.
//
// Parsing the file rather than listing the operations in a slice is the point: a slice would be a
// second list to forget. A new operation added to mover.go with no row in builtinSpecs would fall
// through Profiles.For to the light class and ship silently mis-sized — this makes it fail here,
// in the change that adds it.
func TestEveryOperationHasABuiltinProfile(t *testing.T) {
	declared := operationConstants(t)
	if len(declared) < 10 {
		t.Fatalf("found only %d Operation constants in mover.go — the parser is not reading what it thinks", len(declared))
	}
	for _, op := range declared {
		if _, ok := builtinSpecs[op]; !ok {
			t.Errorf("operation %q has no row in builtinSpecs: it would be sized as generic maintenance", op)
		}
	}
	for op := range builtinSpecs {
		found := false
		for _, d := range declared {
			if d == op {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("builtinSpecs sizes %q, which is not an Operation constant", op)
		}
	}
}

// operationConstants extracts the `Op… Operation = "…"` const values from mover.go.
func operationConstants(t *testing.T) []Operation {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "mover.go", nil, 0)
	if err != nil {
		t.Fatalf("parse mover.go: %v", err)
	}
	var out []Operation
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		var lastType string
		for _, spec := range gen.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			if ident, ok := value.Type.(*ast.Ident); ok {
				lastType = ident.Name
			}
			if lastType != "Operation" || len(value.Values) == 0 {
				continue
			}
			lit, ok := value.Values[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				continue
			}
			unquoted, err := strconv.Unquote(lit.Value)
			if err != nil {
				t.Fatalf("unquote %s: %v", lit.Value, err)
			}
			out = append(out, Operation(unquoted))
		}
	}
	return out
}

// TestBuiltinProfilesResolve proves the constants in the table are valid quantities and internally
// consistent. Profiles.For panics on a malformed cell (it cannot return an error), so this is what
// keeps that panic unreachable.
func TestBuiltinProfilesResolve(t *testing.T) {
	for op, spec := range builtinSpecs {
		if _, err := spec.resolve(); err != nil {
			t.Errorf("built-in profile %q does not resolve: %v", op, err)
		}
	}
	for op := range builtinSpecs {
		profile := Profiles(nil).For(op)
		if len(profile.Requests) == 0 {
			t.Errorf("operation %q has no requests: its pod would be BestEffort, first to be evicted", op)
		}
		if profile.CacheSizeLimit == nil {
			t.Errorf("operation %q has no restic-cache sizeLimit: an unbounded cache can fill the node disk", op)
		}
	}
}

// TestPruneIsSizedAboveBackup is the roadmap item itself, as an assertion: "mover resources by
// operation type (prune > backup)". A change that flattens the table back to one size for
// everything compiles, deploys, and is caught here.
func TestPruneIsSizedAboveBackup(t *testing.T) {
	prune, backup := Profiles(nil).For(OpPrune), Profiles(nil).For(OpBackup)

	pruneReq, backupReq := prune.Requests[corev1.ResourceMemory], backup.Requests[corev1.ResourceMemory]
	if pruneReq.Cmp(backupReq) <= 0 {
		t.Errorf("prune memory request %s is not above backup's %s", pruneReq.String(), backupReq.String())
	}
	pruneLimit, backupLimit := prune.Limits[corev1.ResourceMemory], backup.Limits[corev1.ResourceMemory]
	if pruneLimit.Cmp(backupLimit) <= 0 {
		t.Errorf("prune memory limit %s is not above backup's %s", pruneLimit.String(), backupLimit.String())
	}

	// …and the light class must be BELOW backup, or "by operation type" means nothing in the
	// other direction: an unlock reserving a backup's memory is capacity nobody gets to use.
	unlock := Profiles(nil).For(OpUnlock)
	unlockReq := unlock.Requests[corev1.ResourceMemory]
	if unlockReq.Cmp(backupReq) >= 0 {
		t.Errorf("unlock memory request %s is not below backup's %s", unlockReq.String(), backupReq.String())
	}
}

// TestLoadProfilesMergesFieldByField is the promise made to a platform admin: raising ONE number
// leaves every other number of that operation, and every other operation, alone.
func TestLoadProfilesMergesFieldByField(t *testing.T) {
	// The override touches ONE field of the requests group and ONE field of the limits group. The
	// untouched siblings — prune's cpu request above all — are what a whole-object replacement
	// would silently blank, and a blanked CPU request is a prune that schedules anywhere.
	profiles, err := LoadProfiles([]byte(`
prune:
  requests:
    memory: 2Gi
  limits:
    memory: 32Gi
`))
	if err != nil {
		t.Fatalf("LoadProfiles() = %v", err)
	}

	prune, builtinPrune := profiles.For(OpPrune), Profiles(nil).For(OpPrune)
	if got := prune.Limits[corev1.ResourceMemory]; got.String() != "32Gi" {
		t.Errorf("prune memory limit = %s, want 32Gi (the override)", got.String())
	}
	if got := prune.Requests[corev1.ResourceMemory]; got.String() != "2Gi" {
		t.Errorf("prune memory request = %s, want 2Gi (the override)", got.String())
	}
	if got, want := prune.Requests[corev1.ResourceCPU], builtinPrune.Requests[corev1.ResourceCPU]; got.Cmp(want) != 0 {
		t.Errorf("prune cpu request = %s, want the untouched built-in %s — the sibling field of an "+
			"overridden one must survive", got.String(), want.String())
	}
	if _, ok := prune.Requests[corev1.ResourceCPU]; !ok {
		t.Error("prune has no cpu request left at all after overriding its memory request")
	}

	backup, builtinBackup := profiles.For(OpBackup), Profiles(nil).For(OpBackup)
	if got, want := backup.Limits[corev1.ResourceMemory], builtinBackup.Limits[corev1.ResourceMemory]; got.Cmp(want) != 0 {
		t.Errorf("backup memory limit = %s, want the untouched built-in %s — raising prune must not move backup",
			got.String(), want.String())
	}
}

// TestLoadProfilesDefaultKeyAppliesEverywhere, and is beaten by an operation's own entry.
func TestLoadProfilesDefaultKeyAppliesEverywhere(t *testing.T) {
	profiles, err := LoadProfiles([]byte(`
default:
  requests:
    cpu: 250m
  cacheSizeLimit: 5Gi
check:
  requests:
    cpu: 2
`))
	if err != nil {
		t.Fatalf("LoadProfiles() = %v", err)
	}

	for _, op := range []Operation{OpBackup, OpRestore, OpInit, OpSync} {
		profile := profiles.For(op)
		if got := profile.Requests[corev1.ResourceCPU]; got.String() != "250m" {
			t.Errorf("%s cpu request = %s, want the default override 250m", op, got.String())
		}
		if got := profile.CacheSizeLimit; got == nil || got.String() != "5Gi" {
			t.Errorf("%s cacheSizeLimit = %v, want the default override 5Gi", op, got)
		}
	}
	check := profiles.For(OpCheck)
	if got := check.Requests[corev1.ResourceCPU]; got.String() != "2" {
		t.Errorf("check cpu request = %s, want its own override 2 (it must beat `default`)", got.String())
	}
	if got := check.CacheSizeLimit; got == nil || got.String() != "5Gi" {
		t.Errorf("check cacheSizeLimit = %v, want the default override 5Gi it did not restate", got)
	}
}

// TestLoadProfilesZeroRemovesTheField: "0" is how an operator says "no limit here", and it must
// produce an ABSENT field rather than a limit of zero (which the API server would honour by
// killing the pod instantly).
func TestLoadProfilesZeroRemovesTheField(t *testing.T) {
	profiles, err := LoadProfiles([]byte(`
backup:
  limits:
    memory: "0"
  cacheSizeLimit: "0"
`))
	if err != nil {
		t.Fatalf("LoadProfiles() = %v", err)
	}
	backup := profiles.For(OpBackup)
	if _, ok := backup.Limits[corev1.ResourceMemory]; ok {
		t.Errorf("backup memory limit present, want removed by the \"0\" override")
	}
	if backup.CacheSizeLimit != nil {
		t.Errorf("backup cacheSizeLimit = %v, want nil (unbounded) for the \"0\" override", backup.CacheSizeLimit)
	}
	// The Job spec must then carry an emptyDir with no sizeLimit at all, not a zero one.
	job := BuildJob(JobRequest{Operation: OpBackup, Profiles: profiles})
	if got := cacheVolume(t, job).EmptyDir.SizeLimit; got != nil {
		t.Errorf("cache emptyDir sizeLimit = %v, want unset", got)
	}
}

// TestLoadProfilesRejects covers every way a misconfiguration could otherwise be swallowed. Each
// case is a real thing an admin types; none of them may produce a running operator.
func TestLoadProfilesRejects(t *testing.T) {
	cases := map[string]struct{ yaml, want string }{
		"unknown operation": {
			yaml: "pruning:\n  requests:\n    cpu: 1\n",
			want: "names no operation",
		},
		// The field an admin reaches for by analogy with the container's own schema. It is not a
		// field here, and being ignored would leave the cache capped at the default while
		// `helm get values` showed the number they typed.
		"unknown field": {
			yaml: "prune:\n  cacheLimit: 5Gi\n",
			want: "parse mover profiles",
		},
		"unknown nested field": {
			yaml: "prune:\n  requests:\n    ephemeral-storage: 5Gi\n",
			want: "parse mover profiles",
		},
		"unparseable quantity": {
			yaml: "prune:\n  requests:\n    memory: 4 gigs\n",
			want: "not a valid quantity",
		},
		"request above its own limit": {
			yaml: "backup:\n  requests:\n    memory: 16Gi\n",
			want: "exceeds limits.memory",
		},
		"negative quantity": {
			yaml: "backup:\n  requests:\n    cpu: -1\n",
			want: "is negative",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := LoadProfiles([]byte(tc.yaml))
			if err == nil {
				t.Fatalf("LoadProfiles(%q) = nil error, want a refusal", tc.yaml)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), tc.want)
			}
		})
	}
}

// TestLoadProfilesEmptyIsTheBuiltInTable — the chart renders an empty map when nobody overrides
// anything, and that must not be read as "no sizing".
func TestLoadProfilesEmptyIsTheBuiltInTable(t *testing.T) {
	for _, input := range []string{"", "{}\n", "# nothing\n"} {
		profiles, err := LoadProfiles([]byte(input))
		if err != nil {
			t.Fatalf("LoadProfiles(%q) = %v", input, err)
		}
		got, want := profiles.For(OpBackup), Profiles(nil).For(OpBackup)
		if got.String() != want.String() {
			t.Errorf("LoadProfiles(%q) backup profile = %s, want the built-in %s", input, got, want)
		}
	}
}

// TestBuildJobCarriesTheOperationsOwnProfile is the anti-inertness assertion: the table reaches the
// pod, and a prune Job is sized as a prune rather than as whatever the previous caller asked for.
func TestBuildJobCarriesTheOperationsOwnProfile(t *testing.T) {
	profiles, err := LoadProfiles([]byte(`
prune:
  requests:
    cpu: 1500m
    memory: 3Gi
  limits:
    memory: 12Gi
  cacheSizeLimit: 60Gi
`))
	if err != nil {
		t.Fatalf("LoadProfiles() = %v", err)
	}

	prune := BuildJob(JobRequest{Operation: OpPrune, Profiles: profiles})
	c := prune.Spec.Template.Spec.Containers[0]
	if got := c.Resources.Requests[corev1.ResourceCPU]; got.String() != "1500m" {
		t.Errorf("prune cpu request in the Job = %s, want 1500m", got.String())
	}
	if got := c.Resources.Requests[corev1.ResourceMemory]; got.String() != "3Gi" {
		t.Errorf("prune memory request in the Job = %s, want 3Gi", got.String())
	}
	if got := c.Resources.Limits[corev1.ResourceMemory]; got.String() != "12Gi" {
		t.Errorf("prune memory limit in the Job = %s, want 12Gi", got.String())
	}
	if got := cacheVolume(t, prune).EmptyDir.SizeLimit; got == nil || got.String() != "60Gi" {
		t.Errorf("prune cache sizeLimit in the Job = %v, want 60Gi", got)
	}

	// The same table, a different operation: backup keeps the built-in row.
	backup := BuildJob(JobRequest{Operation: OpBackup, Profiles: profiles})
	wantMemory := Profiles(nil).For(OpBackup).Requests[corev1.ResourceMemory]
	gotMemory := backup.Spec.Template.Spec.Containers[0].Resources.Requests[corev1.ResourceMemory]
	if gotMemory.Cmp(wantMemory) != 0 {
		t.Errorf("backup memory request = %s, want the built-in %s — one operation's override must not leak",
			gotMemory.String(), wantMemory.String())
	}
}

// TestProfilesForNeverAliasesTheTable: two Jobs built from one table must not share a map, or an
// edit to one Job's resources would silently retune every subsequent Job.
func TestProfilesForNeverAliasesTheTable(t *testing.T) {
	profiles := DefaultProfiles()
	first := profiles.For(OpBackup)
	first.Requests[corev1.ResourceMemory] = resource.MustParse("64Gi")

	second := profiles.For(OpBackup)
	if got := second.Requests[corev1.ResourceMemory]; got.String() == "64Gi" {
		t.Error("Profiles.For returned an aliased ResourceList: mutating one Job's requests changed the table")
	}
}

// cacheVolume returns the restic cache volume of a built Job, failing the test if it is missing.
func cacheVolume(t *testing.T, job *batchv1.Job) corev1.Volume {
	t.Helper()
	for _, v := range job.Spec.Template.Spec.Volumes {
		if v.Name == volumeCache {
			if v.EmptyDir == nil {
				t.Fatalf("cache volume %q is not an emptyDir", volumeCache)
			}
			return v
		}
	}
	t.Fatalf("no %q volume in the Job", volumeCache)
	return corev1.Volume{}
}

// TestGoMemLimitIsBelowEveryOperationsOwnLimit is the invariant the derivation exists for: the Go
// runtime must start collecting BEFORE the cgroup kills the process, so for every operation the
// derived GOMEMLIMIT has to be strictly under that operation's own limits.memory.
//
// It walks the built-in table rather than a handful of chosen rows, so an operation added later
// with a memory limit nobody thought about is covered on the day it appears.
func TestGoMemLimitIsBelowEveryOperationsOwnLimit(t *testing.T) {
	var checked int
	for op := range builtinSpecs {
		profile := Profiles(nil).For(op)
		limit, hasLimit := profile.Limits[corev1.ResourceMemory]
		if !hasLimit {
			t.Errorf("%s has no memory limit; every built-in row is expected to carry one", op)
			continue
		}
		value := profile.GoMemLimit()
		if value == "" {
			t.Errorf("%s has a memory limit of %s but derives no GOMEMLIMIT", op, limit.String())
			continue
		}
		checked++

		derived, err := resource.ParseQuantity(strings.TrimSuffix(value, "iB") + "i")
		if err != nil {
			t.Fatalf("%s: GOMEMLIMIT %q does not parse back: %v", op, value, err)
		}
		if derived.Cmp(limit) >= 0 {
			t.Errorf("%s: GOMEMLIMIT %s is not below limits.memory %s — the runtime would only "+
				"start collecting once the kubelet had already killed it", op, value, limit.String())
		}
		if derived.Cmp(goMemLimitFloor) < 0 {
			t.Errorf("%s: GOMEMLIMIT %s is under the %s floor; a built-in row should never be that tight",
				op, value, goMemLimitFloor.String())
		}
	}
	// Without this, deleting builtinSpecs would make every assertion above vacuous.
	if checked < 13 {
		t.Errorf("only %d operation(s) checked; there were 13 when this was written", checked)
	}
}

// TestGoMemLimitOmittedWhenThereIsNothingSafeToDerive pins the two cases that must produce no
// GOMEMLIMIT at all. Both are ways of making things WORSE than not setting it.
func TestGoMemLimitOmittedWhenThereIsNothingSafeToDerive(t *testing.T) {
	// No memory limit: `limits.memory: "0"` is how an operator says "do not cap this pod", and
	// capping its heap anyway would obey the override's letter and invert its purpose.
	unbounded := Profile{Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")}}
	if got := unbounded.GoMemLimit(); got != "" {
		t.Errorf("GoMemLimit() = %q with no memory limit, want empty", got)
	}
	if got := (Profile{}).GoMemLimit(); got != "" {
		t.Errorf("GoMemLimit() = %q on an empty profile, want empty", got)
	}

	// Under the floor: 80% of 128Mi is 102MiB, tight enough that a GC death spiral is likelier
	// than the OOM kill this is meant to prevent. Silence beats a cap that stalls a backup.
	tight := Profile{Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("128Mi")}}
	if got := tight.GoMemLimit(); got != "" {
		t.Errorf("GoMemLimit() = %q under the floor, want empty", got)
	}

	// And the other side of that boundary, so the floor is a threshold rather than a blanket
	// refusal: 80% of 1Gi is 819MiB, comfortably over, and must be set.
	ample := Profile{Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")}}
	if got := ample.GoMemLimit(); got != "819MiB" {
		t.Errorf("GoMemLimit() = %q for a 1Gi limit, want 819MiB", got)
	}
}

// TestGoMemLimitFollowsAnOverride is what stops this becoming a second inert knob: an operator who
// raises prune's memory must get a GOMEMLIMIT that moved with it. The failure this catches is a
// derivation wired to the BUILT-IN table instead of the resolved one, which no built-in-only test
// can see.
func TestGoMemLimitFollowsAnOverride(t *testing.T) {
	before := Profiles(nil).For(OpPrune).GoMemLimit()

	profiles, err := LoadProfiles([]byte("prune:\n  limits:\n    memory: 16Gi\n"))
	if err != nil {
		t.Fatalf("load profiles: %v", err)
	}
	after := profiles.For(OpPrune).GoMemLimit()

	if after == before {
		t.Errorf("GOMEMLIMIT stayed %q after raising prune's memory limit to 16Gi — it is reading "+
			"the built-in table, not the resolved one", before)
	}
	if after != "13107MiB" { // 80% of 16Gi, floored to whole MiB
		t.Errorf("GOMEMLIMIT = %q after the override, want 13107MiB", after)
	}

	// The operation next to it must not have moved: overrides are per-operation.
	if got := profiles.For(OpBackup).GoMemLimit(); got != "3276MiB" {
		t.Errorf("overriding prune changed backup's GOMEMLIMIT to %q, want 3276MiB", got)
	}
}
