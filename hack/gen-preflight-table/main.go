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

// Command gen-preflight-table writes the exposer-selection block of the published shell scripts
// (website/public/preflight.sh and website/public/snapshot-probe.sh) from internal/exposer, and
// writes each script's SHA-256 sidecar.
//
// # Why two scripts share one generated block
//
// They ask the two halves of the same question and must agree on the answer to the first half.
// preflight.sh reports which exposer would be chosen; snapshot-probe.sh then goes and exercises
// that exposer's shape — it picks the same VolumeSnapshotClass by the same tie-break, and it
// builds its restored PVC ReadWriteOnce or ReadOnlyMany according to the same rule, because a
// probe that mounts an object the operator would never create proves nothing about the operator.
// One generated region, spliced into both, is what keeps that true.
//
// # Why this is generated
//
// The preflight script's entire reason for existing is to tell an administrator, per
// StorageClass, which exposer CrystalBackup would choose and which volumes it would skip. That
// answer is a property of internal/exposer.Registry.For, and nothing else. A shell
// re-implementation of that rule is a second copy of the routing logic, in a language no
// compiler checks, shipped to strangers as the authoritative preview of what the operator will
// do. It drifts on the first change to the routing — and it drifts SILENTLY, because the script
// keeps printing confident verdicts either way. This project spent M6 removing exactly that
// failure mode from the alert rules and the metrics reference; a hand-written copy here would
// reintroduce it, one step further from the code and one step closer to the user.
//
// # Where the facts come from, and how they are checked
//
// Two independent passes, both required to agree:
//
//  1. EXTRACTION. The string constants that parameterise the routing — the CephFS provisioner
//     marker, the exposer Kind values, the snapshot API group/version, the CSI-migration
//     annotation — are read out of the declarations in internal/exposer/*.go by go/parser.
//     Nothing is typed out here.
//
//  2. EXECUTION. The emitted shell rule is then re-evaluated in Go and compared, provisioner by
//     provisioner, against what the REAL Registry.For returns when driven with a fake client.
//     Extraction alone would catch a changed constant but not a changed structure — a new
//     routing branch would leave the constants intact and the emitted rule wrong. Executing the
//     real resolver over probe provisioners catches that. Each provisioner is probed twice, once
//     through a bound PVC and once through an unbound one, because Registry.For sources the driver
//     from the PersistentVolume in the first case and the StorageClass in the second; probing one
//     path would leave the other free to drift, and the bound path is the one real PVCs take.
//     A third probe pins the branch no provisioner can reach: a PVC bound to a non-CSI volume.
//
// # What the model has to cover, and what it once did not
//
// The comparison in pass 2 is only as good as its model of the SCRIPTS. The scripts do not call
// Registry.For; they read a StorageClass, DERIVE a driver from it, and look for a
// VolumeSnapshotClass matching that driver. For a long while every probe here had an effective
// driver identical to its .provisioner, so the derivation was invisible to the guard — and when
// driverFor learned to prefer the pv.kubernetes.io/migrated-to annotation on the StorageClass path,
// the scripts kept reading .provisioner and this guard stayed green. A CSI-migrated in-tree class
// would have been reported DATA SKIPPED for volumes the operator snapshots perfectly well.
//
// So the derivation is now modelled too (storageClassDriver), emitted into the scripts rather than
// hand-written in each of them (cb_sc_driver), and probed by a class whose serving driver differs
// from its provisioner. driverServingVolumes — the truth used to seed the fake cluster — is
// deliberately a SEPARATE function from storageClassDriver, the model: collapsing them would make
// the two agree by construction rather than by being right.
//
// On top of both passes, the generator enumerates every Kind* constant the package declares and
// fails if it finds one this generator has no rule for. Adding a third exposer to internal/exposer
// therefore breaks `make preflight-table-verify` in CI with a message naming the new kind,
// rather than shipping a script that quietly never mentions it.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/CrystalBackup/CrystalBackup/internal/exposer"
)

const (
	// beginMarker / endMarker delimit the region of the script this command owns. Everything
	// between them is overwritten; everything outside is preserved verbatim.
	beginMarker = "# >>> BEGIN GENERATED — exposer selection (make preflight-table) >>>"
	endMarker   = "# <<< END GENERATED <<<"

	exposerPkgDir = "internal/exposer"

	// probeNamespace is where the probe PVCs live. Nothing depends on the name — it is a constant
	// so the string is written once.
	probeNamespace = "tenant"

	// verdictSkip is the token the emitted shell rule prints for a volume that cannot be
	// snapshotted at all: internal/exposer's ErrUnsupported, reduced to the script's vocabulary.
	verdictSkip = "skip"
)

// scripts are the published shell scripts that carry the generated region, relative to the
// repository root. Both are downloaded and run by administrators against their own clusters, and
// both are checksummed here rather than separately: a sidecar regenerated in its own pass goes
// stale one commit after the script does, and it fails in the hands of the one person who
// followed the documentation and verified before running.
var scripts = []string{
	filepath.Join("website", "public", "preflight.sh"),
	filepath.Join("website", "public", "snapshot-probe.sh"),
}

// knownKinds maps every exposer Kind constant this generator knows how to describe to the
// verdict token the script prints for it. A Kind* constant in internal/exposer that is absent
// from this map is a hard error: see checkKindCoverage.
var knownKinds = map[string]string{
	"KindCSIGeneric":    "csi-generic",
	"KindCephFSShallow": "cephfs-shallow",
}

func main() {
	root := flag.String("root", ".", "repository root")
	outDir := flag.String("out-dir", "", "write the scripts and sidecars here instead of in place (verify mode)")
	flag.Parse()

	if err := run(*root, *outDir); err != nil {
		fmt.Fprintf(os.Stderr, "gen-preflight-table: %v\n", err)
		os.Exit(1)
	}
}

func run(root, outDir string) error {
	facts, err := extract(filepath.Join(root, exposerPkgDir))
	if err != nil {
		return err
	}
	if err := verifyAgainstRegistry(facts); err != nil {
		return err
	}

	if outDir != "" {
		if err := os.MkdirAll(outDir, 0o755); err != nil {
			return err
		}
	}
	for _, rel := range scripts {
		if err := emit(root, outDir, rel, facts); err != nil {
			return err
		}
	}
	return nil
}

// emit splices the generated region into one script and writes it plus its sidecar.
func emit(root, outDir, rel string, facts *facts) error {
	scriptPath := filepath.Join(root, rel)
	src, err := os.ReadFile(scriptPath) //nolint:gosec // path is repo-relative and fixed
	if err != nil {
		return fmt.Errorf("read %s: %w", scriptPath, err)
	}

	if err := checkMigratedAnnotationRead(string(src), scriptPath, facts); err != nil {
		return err
	}

	spliced, err := splice(string(src), facts.render())
	if err != nil {
		return fmt.Errorf("%s: %w", scriptPath, err)
	}

	base := filepath.Base(rel)
	dest := scriptPath
	if outDir != "" {
		dest = filepath.Join(outDir, base)
	}
	if err := os.WriteFile(dest, []byte(spliced), 0o755); err != nil { //nolint:gosec // an executable script is the point
		return fmt.Errorf("write %s: %w", dest, err)
	}

	// The sidecar checksum is generated in the same breath as the script, and held to the tree
	// by the same guard. A checksum regenerated separately — or by hand — is a checksum that
	// goes stale one commit after the script does, and a stale checksum does not fail quietly:
	// it fails in the hands of the one administrator who bothered to verify.
	//
	// The name inside the sidecar is the BASENAME, not the path it was written to, so that
	// `sha256sum -c` works in the directory the file is published from — which is the only place
	// anybody will ever run it.
	sum := sha256.Sum256([]byte(spliced))
	sidecar := dest + ".sha256"
	line := fmt.Sprintf("%s  %s\n", hex.EncodeToString(sum[:]), base)
	if err := os.WriteFile(sidecar, []byte(line), 0o644); err != nil { //nolint:gosec // published sidecar
		return fmt.Errorf("write %s: %w", sidecar, err)
	}

	fmt.Printf("wrote %s and %s\n", dest, sidecar)
	return nil
}

// checkMigratedAnnotationRead is the one thing about the migrated-to annotation the generated region
// cannot own. The DERIVATION is generated (cb_sc_driver), but the annotation's NAME also has to
// appear inside the scripts' kubectl jsonpath expressions, where every dot must be backslash-escaped
// — and a jsonpath assembled from a shell variable would mean turning every one of those
// single-quoted expressions inside out, trading a readable query for an unreadable one.
//
// So the name is checked instead of substituted: if migratedToAnnotation ever changes in
// internal/exposer, `make preflight-table` fails here naming the new value, rather than emitting a
// script that reads an annotation nothing sets and calls every migrated volume unsnapshottable.
func checkMigratedAnnotationRead(src, scriptPath string, facts *facts) error {
	escaped := strings.ReplaceAll(facts.migratedToAnnotation, ".", `\.`)
	if strings.Contains(src, escaped) {
		return nil
	}
	return fmt.Errorf("%s does not read the %q annotation anywhere:\n"+
		"  expected its jsonpath-escaped form %q to appear in a kubectl query.\n"+
		"  internal/exposer resolves a CSI-migrated volume's driver from that annotation, on both the\n"+
		"  StorageClass and the PersistentVolume. A script that never reads it reports every migrated\n"+
		"  volume as unsnapshottable — confidently, and wrongly. Update the jsonpath in %s",
		scriptPath, facts.migratedToAnnotation, escaped, filepath.Base(scriptPath))
}

// facts is everything read out of internal/exposer that the script needs.
type facts struct {
	cephfsMarker    string
	kindCSIGeneric  string
	kindCephFS      string
	snapshotGroup   string
	snapshotVersion string

	// migratedToAnnotation is the annotation Kubernetes leaves on an in-tree StorageClass and on its
	// PersistentVolumes when a CSI driver has superseded the plugin. driverFor prefers it over both
	// .provisioner and .spec.csi.driver, so it is as much a routing parameter as the CephFS marker
	// is, and it is read out of the source for the same reason.
	migratedToAnnotation string
}

// extract reads the string constants out of the exposer package's declarations. It never falls
// back to a default: a constant that cannot be found is an error, because a silently-defaulted
// routing parameter is precisely the drift this command exists to prevent.
func extract(dir string) (*facts, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}

	fset := token.NewFileSet()
	consts := map[string]string{}
	parsed := 0
	for _, entry := range entries {
		fname := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(fname, ".go") || strings.HasSuffix(fname, "_test.go") {
			continue
		}
		path := filepath.Join(dir, fname)
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		if file.Name.Name != "exposer" {
			continue
		}
		parsed++
		for _, decl := range file.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok || len(vs.Names) != len(vs.Values) {
					continue
				}
				for i, name := range vs.Names {
					lit, ok := vs.Values[i].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					v, err := strconv.Unquote(lit.Value)
					if err != nil {
						continue
					}
					consts[name.Name] = v
				}
			}
		}
	}

	if parsed == 0 {
		return nil, fmt.Errorf("no package exposer source files found in %s", dir)
	}

	if err := checkKindCoverage(consts); err != nil {
		return nil, err
	}

	f := &facts{}
	for _, want := range []struct {
		name string
		dst  *string
	}{
		{"cephfsProvisionerMarker", &f.cephfsMarker},
		{"KindCSIGeneric", &f.kindCSIGeneric},
		{"KindCephFSShallow", &f.kindCephFS},
		{"volumeSnapshotGroup", &f.snapshotGroup},
		{"volumeSnapshotVersion", &f.snapshotVersion},
		{"migratedToAnnotation", &f.migratedToAnnotation},
	} {
		v, ok := consts[want.name]
		if !ok || v == "" {
			return nil, fmt.Errorf("constant %s not found (or empty) in %s: the preflight script's "+
				"exposer table cannot be generated without it", want.name, dir)
		}
		*want.dst = v
	}
	return f, nil
}

// checkKindCoverage fails when internal/exposer declares a Kind* constant this generator has no
// verdict for. That is the guard that makes a third exposer a CI failure with a name in it,
// instead of a preflight script that silently never reports it.
func checkKindCoverage(consts map[string]string) error {
	var unknown []string
	for name := range consts {
		if strings.HasPrefix(name, "Kind") {
			if _, ok := knownKinds[name]; !ok {
				unknown = append(unknown, fmt.Sprintf("%s=%q", name, consts[name]))
			}
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	slices.Sort(unknown)
	return fmt.Errorf("internal/exposer declares exposer kind(s) this generator does not describe: %s\n"+
		"  The preflight script reports, per StorageClass, which exposer would be chosen. A new exposer that\n"+
		"  the script never names is a volume class an administrator is told nothing about. Teach\n"+
		"  hack/gen-preflight-table (knownKinds + the emitted rule + verifyAgainstRegistry probes) about it,\n"+
		"  then re-run `make preflight-table`", strings.Join(unknown, ", "))
}

// probe is one StorageClass, declared the way an administrator would declare it, fed through the
// real Registry.For AND through this generator's model of what the scripts will say about it.
type probe struct {
	// provisioner is the StorageClass's declared .provisioner. For an in-tree plugin superseded by
	// a CSI driver this is NOT the driver serving the volumes — migratedTo is — and that difference
	// is the whole reason this struct has three fields instead of two.
	provisioner string

	// migratedTo is the pv.kubernetes.io/migrated-to annotation, carried both by the StorageClass
	// and by the PersistentVolumes provisioned under it. Empty for a class that is not CSI-migrated.
	migratedTo string

	// hasVSClass seeds a VolumeSnapshotClass for the class's EFFECTIVE driver, and for that driver
	// only. On a migrated class that means the cluster holds a snapshot class for the CSI driver and
	// none whatsoever for the in-tree provisioner name — which is the true shape, and the shape that
	// tells a script reading the wrong field from a script reading the right one.
	hasVSClass bool
}

// driverServingVolumes is what KUBERNETES does with this class: the volumes of a CSI-migrated class
// are served by the driver in the annotation, whatever .provisioner still says. It seeds the fake
// cluster — the VolumeSnapshotClass is created for THIS driver and for no other, which is why a
// migrated probe's cluster holds a class for ebs.csi.aws.com and nothing at all for
// kubernetes.io/aws-ebs.
//
// It is deliberately NOT the same function as storageClassDriver, which models what the published
// scripts derive. Collapsing the two would make this guard incapable of ever failing on the axis it
// was just blind to: the model would then agree with reality by construction instead of by being
// right, and that is the difference between a guard and a decoration.
func (p probe) driverServingVolumes() string {
	if p.migratedTo != "" {
		return p.migratedTo
	}
	return p.provisioner
}

// verifyAgainstRegistry drives the REAL exposer.Registry.For over a fake cluster and asserts
// that the rule this command is about to emit predicts the same verdict for every probe. This
// is the half of the guard that extraction cannot provide: constants can be right while the
// routing structure around them has changed.
func verifyAgainstRegistry(f *facts) error {
	probes := []probe{
		{provisioner: "rook-ceph.rbd.csi.ceph.com", hasVSClass: true},
		{provisioner: "rook-ceph.cephfs.csi.ceph.com", hasVSClass: true},
		{provisioner: "cephfs.csi.ceph.com", hasVSClass: true},
		{provisioner: "driver.longhorn.io", hasVSClass: true},
		{provisioner: "ebs.csi.aws.com", hasVSClass: true},
		{provisioner: "pd.csi.storage.gke.io", hasVSClass: true},
		{provisioner: "rancher.io/local-path", hasVSClass: false},
		{provisioner: "kubernetes.io/no-provisioner", hasVSClass: false},
		// CephFS with no snapshot class is still a skip.
		{provisioner: "rook-ceph.cephfs.csi.ceph.com", hasVSClass: false},

		// A CSI-migrated in-tree StorageClass: the provisioner names a plugin that no longer serves
		// anything, and the driver that does is in the annotation. The cluster holds a snapshot class
		// for ebs.csi.aws.com and NONE for kubernetes.io/aws-ebs, so a resolver reading .provisioner
		// concludes "no snapshot class, skip this class" while the operator snapshots it happily.
		//
		// This probe exists because that is not a hypothetical: the driver resolution in
		// internal/exposer gained the migrated-to read on the StorageClass path in the same release
		// as this comment, the published scripts kept reading .provisioner, and this guard — whose
		// entire job is to make that drift impossible — did not fire, because no probe here had an
		// effective driver different from its provisioner. Every probe above pins the routing; this
		// one pins the INPUT to the routing, which is where the guard was blind.
		{provisioner: "kubernetes.io/aws-ebs", migratedTo: "ebs.csi.aws.com", hasVSClass: true},
	}

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		return err
	}
	if err := storagev1.AddToScheme(scheme); err != nil {
		return err
	}

	ctx := context.Background()
	for i, p := range probes {
		// served is the truth about the cluster; modelled is what the published scripts would derive
		// from the same StorageClass. For every probe but the migrated one they are the same string,
		// and the point of the migrated one is that they are not.
		served := p.driverServingVolumes()
		modelled := storageClassDriver(p.provisioner, p.migratedTo)

		scName := fmt.Sprintf("sc-%d", i)
		sc := &storagev1.StorageClass{
			ObjectMeta:  metav1.ObjectMeta{Name: scName},
			Provisioner: p.provisioner,
		}
		if p.migratedTo != "" {
			sc.Annotations = map[string]string{f.migratedToAnnotation: p.migratedTo}
		}
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sc).Build()
		if p.hasVSClass {
			vsc := &unstructured.Unstructured{}
			vsc.SetGroupVersionKind(schema.GroupVersionKind{
				Group:   f.snapshotGroup,
				Version: f.snapshotVersion,
				Kind:    "VolumeSnapshotClass",
			})
			vsc.SetName(fmt.Sprintf("vsc-%d", i))
			if err := unstructured.SetNestedField(vsc.Object, served, "driver"); err != nil {
				return err
			}
			if err := c.Create(ctx, vsc); err != nil {
				return fmt.Errorf("seed VolumeSnapshotClass: %w", err)
			}
		}

		// Both resolution paths, against the same expected verdict. Registry.For takes a bound PVC's
		// driver from its PersistentVolume and an unbound one's from its StorageClass, so probing
		// only one of them would leave the other free to drift — and the bound path is the one
		// nearly every real PVC takes. The emitted shell rule maps a DRIVER to an exposer and is
		// therefore common to both; that is exactly the invariant worth pinning here.
		pvName := fmt.Sprintf("pv-%d", i)
		pvcs := map[string]*corev1.PersistentVolumeClaim{
			"unbound (driver from the StorageClass)": {
				ObjectMeta: metav1.ObjectMeta{Name: "probe", Namespace: probeNamespace},
				Spec:       corev1.PersistentVolumeClaimSpec{StorageClassName: &scName},
			},
			"bound (driver from the PersistentVolume)": {
				ObjectMeta: metav1.ObjectMeta{Name: "probe-bound", Namespace: probeNamespace},
				Spec: corev1.PersistentVolumeClaimSpec{
					StorageClassName: &scName,
					VolumeName:       pvName,
				},
			},
		}
		// The PV the bound probe resolves through, built the way the class it came from would build
		// it: a CSI volume naming the driver, or — for a migrated class — an in-tree volume whose
		// source is the superseded plugin and whose driver is only in the annotation. Both sources
		// resolve to the same effective driver, so any difference in verdict between the two paths is
		// a difference in ROUTING and not in inputs.
		pv := &corev1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{Name: pvName},
			Spec:       corev1.PersistentVolumeSpec{StorageClassName: scName},
		}
		if p.migratedTo != "" {
			pv.Annotations = map[string]string{f.migratedToAnnotation: p.migratedTo}
			pv.Spec.PersistentVolumeSource = corev1.PersistentVolumeSource{
				AWSElasticBlockStore: &corev1.AWSElasticBlockStoreVolumeSource{VolumeID: "vol-0probe"},
			}
		} else {
			pv.Spec.PersistentVolumeSource = corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{Driver: p.provisioner},
			}
		}
		if err := c.Create(ctx, pv); err != nil {
			return fmt.Errorf("seed PersistentVolume: %w", err)
		}

		// The scripts never call Registry.For. They read a StorageClass, derive a driver from it, and
		// look for a VolumeSnapshotClass whose .driver matches THAT — so both halves of the chain are
		// modelled here. A guard that modelled only the second half is precisely the guard that missed
		// the migrated class: the emitted rule was right about the driver it was handed, and the
		// script was handing it the wrong one. Deriving the wrong driver does not merely mislabel the
		// row, it makes the snapshot-class lookup miss, and a working class comes out DATA SKIPPED.
		modelSeesVSClass := p.hasVSClass && served == modelled
		want := f.predict(modelled, modelSeesVSClass)
		for path, pvc := range pvcs {
			got, err := probeVerdict(ctx, c, pvc)
			if err != nil {
				return fmt.Errorf("probe %q (vsclass=%v, %s): unexpected error from Registry.For: %w",
					p.provisioner, p.hasVSClass, path, err)
			}
			if got != want {
				return fmt.Errorf("the generated shell rule disagrees with internal/exposer.Registry.For:\n"+
					"  StorageClass .provisioner %q, %s annotation %q\n"+
					"  the volumes are served by driver %q; the scripts would derive %q\n"+
					"  a VolumeSnapshotClass exists for the serving driver: %v; the scripts would find one: %v\n"+
					"  resolved via the %s path: Registry.For says %q, the scripts would say %q\n"+
					"  The driver resolution in internal/exposer/registry.go has changed shape. Update the emitted\n"+
					"  rule — and storageClassDriver, the derivation the scripts feed it — in hack/gen-preflight-table,\n"+
					"  so the published scripts keep telling the truth",
					p.provisioner, f.migratedToAnnotation, p.migratedTo,
					served, modelled, p.hasVSClass, modelSeesVSClass, path, got, want)
			}
		}
	}

	// The structural branch the per-provisioner probes above cannot reach: a PVC bound to a
	// PersistentVolume that is not a CSI volume at all. No provisioner is involved — there is no
	// driver to route — so the verdict must be "skip" whatever classes the cluster holds. It is
	// pinned here because it is the branch that decides whether a statically provisioned NFS volume
	// is skipped (correct, and terminal) or errors (which once held a namespace's queue for thirty
	// hours), and nothing else in this generator would notice it disappearing.
	if err := verifyNonCSIVolumeIsSkipped(ctx, scheme, f); err != nil {
		return err
	}
	return nil
}

// probeVerdict runs the real Registry.For over pvc and reduces the outcome to the vocabulary the
// emitted shell rule speaks: an exposer Kind, or "skip".
func probeVerdict(ctx context.Context, c client.Client, pvc *corev1.PersistentVolumeClaim) (string, error) {
	ex, err := exposer.NewRegistry(c, "crystal-backup-system").For(ctx, pvc)
	switch {
	case err == nil:
		return ex.Kind(), nil
	case errors.Is(err, exposer.ErrUnsupported):
		return verdictSkip, nil
	default:
		return "", err
	}
}

// verifyNonCSIVolumeIsSkipped drives Registry.For with a PVC bound to a statically provisioned NFS
// PersistentVolume, in a cluster that DOES hold a usable snapshot class for a real driver — so a
// verdict of "skip" can only come from the volume itself being non-CSI, not from an empty cluster.
func verifyNonCSIVolumeIsSkipped(ctx context.Context, scheme *runtime.Scheme, f *facts) error {
	const staticClass = "slow" // named by PVC and PV, existing as neither: legal for a static binding
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		&corev1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{Name: "pv-static-nfs"},
			Spec: corev1.PersistentVolumeSpec{
				StorageClassName: staticClass,
				PersistentVolumeSource: corev1.PersistentVolumeSource{
					NFS: &corev1.NFSVolumeSource{Server: "nfs.example.invalid", Path: "/export"},
				},
			},
		},
	).Build()
	vsc := &unstructured.Unstructured{}
	vsc.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   f.snapshotGroup,
		Version: f.snapshotVersion,
		Kind:    "VolumeSnapshotClass",
	})
	vsc.SetName("rbd-snapclass")
	if err := unstructured.SetNestedField(vsc.Object, "rook-ceph.rbd.csi.ceph.com", "driver"); err != nil {
		return err
	}
	if err := c.Create(ctx, vsc); err != nil {
		return fmt.Errorf("seed VolumeSnapshotClass: %w", err)
	}

	scName := staticClass
	got, err := probeVerdict(ctx, c, &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "probe-static", Namespace: probeNamespace},
		Spec:       corev1.PersistentVolumeClaimSpec{StorageClassName: &scName, VolumeName: "pv-static-nfs"},
	})
	if err != nil {
		return fmt.Errorf("a PVC bound to a non-CSI PersistentVolume must resolve to a skip, but Registry.For "+
			"returned an error: %w\n"+
			"  An error here is not a verdict the preflight script can print, and in the Backup controller it is\n"+
			"  a volume that never leaves Pending — which blocks every volume queued behind it in its namespace", err)
	}
	if got != verdictSkip {
		return fmt.Errorf("a PVC bound to a non-CSI (NFS) PersistentVolume resolved to %q, want %q", got, verdictSkip)
	}
	return nil
}

// storageClassDriver is the Go twin of the emitted cb_sc_driver — the driver the published scripts
// derive for a StorageClass, and it must be the same choice driverFor makes on its unbound path:
// the migrated-to annotation when the class carries one, else .provisioner.
//
// The annotation wins because on a CSI-migrated class .provisioner names an in-tree plugin that no
// longer serves anything. No VolumeSnapshotClass will ever carry that name, so a script resolving
// through it finds none and reports the class DATA SKIPPED — a confident wrong answer about a class
// the operator snapshots perfectly well.
func storageClassDriver(provisioner, migratedTo string) string {
	if migratedTo != "" {
		return migratedTo
	}
	return provisioner
}

// predict is the Go twin of the shell function render emits — the same decision, so the
// comparison above is meaningful. Keep the two in lockstep; that is the whole contract.
func (f *facts) predict(driver string, hasVSClass bool) string {
	if !hasVSClass {
		return verdictSkip
	}
	if strings.Contains(driver, f.cephfsMarker) {
		return f.kindCephFS
	}
	return f.kindCSIGeneric
}

// shQuote renders s as a POSIX single-quoted shell word.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// render produces the generated region: plain POSIX sh, no bashisms, no side effects.
func (f *facts) render() string {
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }

	w("# Generated from internal/exposer by `make preflight-table` — do not edit by hand.")
	w("# Sources: internal/exposer/registry.go (Registry.For), exposer.go (Kind values),")
	w("# snapshot.go (snapshot API group/version). The generator additionally executes the real")
	w("# Registry.For against a fake cluster and refuses to emit a rule that disagrees with it.")
	w("")
	w("CB_SNAPSHOT_GROUP=%s", shQuote(f.snapshotGroup))
	w("CB_SNAPSHOT_VERSION=%s", shQuote(f.snapshotVersion))
	w("CB_KIND_CSI_GENERIC=%s", shQuote(f.kindCSIGeneric))
	w("CB_KIND_CEPHFS_SHALLOW=%s", shQuote(f.kindCephFS))
	w("CB_CEPHFS_MARKER=%s", shQuote(f.cephfsMarker))
	w("CB_MIGRATED_TO_ANNOTATION=%s", shQuote(f.migratedToAnnotation))
	w("")
	w("# cb_sc_driver DECLARED_PROVISIONER MIGRATED_TO")
	w("#   The driver serving a StorageClass's volumes: its %s", f.migratedToAnnotation)
	w("#   annotation when it carries one, else its .provisioner. Same choice driverFor makes for a")
	w("#   PVC that is bound to nothing yet.")
	w("#")
	w("#   The annotation wins because on a CSI-migrated class .provisioner names an in-tree plugin")
	w("#   that no longer serves anything: no VolumeSnapshotClass will ever carry that name, so")
	w("#   resolving through it finds none and calls a class DATA SKIPPED that is snapshotted fine.")
	w("#   This derivation is GENERATED for the same reason the rule below is — it is part of the")
	w("#   routing, and a hand-written copy of it in each script drifted within one release.")
	w("cb_sc_driver() {")
	w("\tif [ -n \"$2\" ]; then")
	w("\t\tprintf '%%s\\n' \"$2\"")
	w("\t\treturn 0")
	w("\tfi")
	w("\tprintf '%%s\\n' \"$1\"")
	w("}")
	w("")
	w("# cb_exposer_for DRIVER HAS_SNAPSHOT_CLASS")
	w("#   DRIVER is the driver serving the volume — cb_sc_driver's answer for an unbound PVC, or the")
	w("#   PersistentVolume's own driver for a bound one. HAS_SNAPSHOT_CLASS is 'yes' when some")
	w("#   VolumeSnapshotClass in the cluster has .driver == DRIVER. Prints the exposer kind, or 'skip'.")
	// The case arm and the printfs read the variables above rather than re-embedding the
	// literals, so each fact appears exactly once even inside the generated region. The quoted
	// "$CB_CEPHFS_MARKER" is matched literally by POSIX case; only the surrounding * are globs.
	w("cb_exposer_for() {")
	w("\tif [ \"$2\" != yes ]; then")
	w("\t\tprintf '%%s\\n' skip")
	w("\t\treturn 0")
	w("\tfi")
	w("\tcase $1 in")
	w("\t*\"$CB_CEPHFS_MARKER\"*)")
	w("\t\tprintf '%%s\\n' \"$CB_KIND_CEPHFS_SHALLOW\"")
	w("\t\t;;")
	w("\t*)")
	w("\t\tprintf '%%s\\n' \"$CB_KIND_CSI_GENERIC\"")
	w("\t\t;;")
	w("\tesac")
	w("}")
	w("")
	w("# cb_pick_vsclass: reads candidate VolumeSnapshotClass names on stdin, one per line, and")
	w("#   prints the one the operator would actually resolve when several classes share a driver.")
	w("#   internal/exposer's findVolumeSnapshotClass sorts the candidates and takes the first; it")
	w("#   uses Go's slices.Sort, which on strings is a byte-wise comparison, so the sort must run")
	w("#   under LC_ALL=C to reproduce it. Under a locale collation this would silently pick a")
	w("#   different class from the one the operator will.")
	w("cb_pick_vsclass() {")
	w("\tLC_ALL=C sort | head -n 1")
	w("}")

	return b.String()
}

// splice replaces the region between the markers, preserving the rest of the file byte for
// byte. A missing or malformed region is an error, never a silent append.
func splice(src, region string) (string, error) {
	lines := strings.Split(src, "\n")
	begin, end := -1, -1
	for i, l := range lines {
		switch strings.TrimSpace(l) {
		case beginMarker:
			if begin >= 0 {
				return "", errors.New("duplicate BEGIN GENERATED marker")
			}
			begin = i
		case endMarker:
			if end >= 0 {
				return "", errors.New("duplicate END GENERATED marker")
			}
			end = i
		}
	}
	if begin < 0 || end < 0 {
		return "", fmt.Errorf("generated-region markers not found (expected %q ... %q)", beginMarker, endMarker)
	}
	if end < begin {
		return "", errors.New("END GENERATED marker precedes BEGIN GENERATED")
	}

	out := make([]string, 0, len(lines))
	out = append(out, lines[:begin+1]...)
	out = append(out, strings.Split(strings.TrimRight(region, "\n"), "\n")...)
	out = append(out, lines[end:]...)
	return strings.Join(out, "\n"), nil
}
