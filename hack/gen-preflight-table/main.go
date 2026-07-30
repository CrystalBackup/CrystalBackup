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

// Command gen-preflight-table writes the exposer-selection block of the preflight script
// (website/public/preflight.sh) from internal/exposer, and writes the script's SHA-256
// sidecar.
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
//     marker, the exposer Kind values, the snapshot API group/version — are read out of the
//     declarations in internal/exposer/*.go by go/parser. Nothing is typed out here.
//
//  2. EXECUTION. The emitted shell rule is then re-evaluated in Go and compared, provisioner by
//     provisioner, against what the REAL Registry.For returns when driven with a fake client.
//     Extraction alone would catch a changed constant but not a changed structure — a new
//     routing branch would leave the constants intact and the emitted rule wrong. Executing the
//     real resolver over probe provisioners catches that.
//
// On top of both, the generator enumerates every Kind* constant the package declares and fails
// if it finds one this generator has no rule for. Adding a third exposer to internal/exposer
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
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/CrystalBackup/CrystalBackup/internal/exposer"
)

const (
	// beginMarker / endMarker delimit the region of the script this command owns. Everything
	// between them is overwritten; everything outside is preserved verbatim.
	beginMarker = "# >>> BEGIN GENERATED — exposer selection (make preflight-table) >>>"
	endMarker   = "# <<< END GENERATED <<<"

	exposerPkgDir = "internal/exposer"
)

// knownKinds maps every exposer Kind constant this generator knows how to describe to the
// verdict token the script prints for it. A Kind* constant in internal/exposer that is absent
// from this map is a hard error: see checkKindCoverage.
var knownKinds = map[string]string{
	"KindCSIGeneric":    "csi-generic",
	"KindCephFSShallow": "cephfs-shallow",
}

func main() {
	root := flag.String("root", ".", "repository root")
	out := flag.String("out", "", "write the script here instead of in place (verify mode)")
	flag.Parse()

	if err := run(*root, *out); err != nil {
		fmt.Fprintf(os.Stderr, "gen-preflight-table: %v\n", err)
		os.Exit(1)
	}
}

func run(root, out string) error {
	facts, err := extract(filepath.Join(root, exposerPkgDir))
	if err != nil {
		return err
	}
	if err := verifyAgainstRegistry(facts); err != nil {
		return err
	}

	scriptPath := filepath.Join(root, "website", "public", "preflight.sh")
	src, err := os.ReadFile(scriptPath) //nolint:gosec // path is repo-relative and fixed
	if err != nil {
		return fmt.Errorf("read %s: %w", scriptPath, err)
	}

	spliced, err := splice(string(src), facts.render())
	if err != nil {
		return fmt.Errorf("%s: %w", scriptPath, err)
	}

	dest := scriptPath
	if out != "" {
		dest = out
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
	}
	if err := os.WriteFile(dest, []byte(spliced), 0o755); err != nil { //nolint:gosec // an executable script is the point
		return fmt.Errorf("write %s: %w", dest, err)
	}

	// The sidecar checksum is generated in the same breath as the script, and held to the tree
	// by the same guard. A checksum regenerated separately — or by hand — is a checksum that
	// goes stale one commit after the script does, and a stale checksum does not fail quietly:
	// it fails in the hands of the one administrator who bothered to verify.
	sum := sha256.Sum256([]byte(spliced))
	sidecar := dest + ".sha256"
	line := fmt.Sprintf("%s  preflight.sh\n", hex.EncodeToString(sum[:]))
	if err := os.WriteFile(sidecar, []byte(line), 0o644); err != nil { //nolint:gosec // published sidecar
		return fmt.Errorf("write %s: %w", sidecar, err)
	}

	fmt.Printf("wrote %s and %s\n", dest, sidecar)
	return nil
}

// facts is everything read out of internal/exposer that the script needs.
type facts struct {
	cephfsMarker    string
	kindCSIGeneric  string
	kindCephFS      string
	snapshotGroup   string
	snapshotVersion string
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

// probe is one provisioner fed through the real Registry.For.
type probe struct {
	provisioner string
	hasVSClass  bool
}

// verifyAgainstRegistry drives the REAL exposer.Registry.For over a fake cluster and asserts
// that the rule this command is about to emit predicts the same verdict for every probe. This
// is the half of the guard that extraction cannot provide: constants can be right while the
// routing structure around them has changed.
func verifyAgainstRegistry(f *facts) error {
	probes := []probe{
		{"rook-ceph.rbd.csi.ceph.com", true},
		{"rook-ceph.cephfs.csi.ceph.com", true},
		{"cephfs.csi.ceph.com", true},
		{"driver.longhorn.io", true},
		{"ebs.csi.aws.com", true},
		{"pd.csi.storage.gke.io", true},
		{"rancher.io/local-path", false},
		{"kubernetes.io/no-provisioner", false},
		{"rook-ceph.cephfs.csi.ceph.com", false}, // CephFS with no snapshot class is still a skip.
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
		scName := fmt.Sprintf("sc-%d", i)
		sc := &storagev1.StorageClass{
			ObjectMeta:  metav1.ObjectMeta{Name: scName},
			Provisioner: p.provisioner,
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
			if err := unstructured.SetNestedField(vsc.Object, p.provisioner, "driver"); err != nil {
				return err
			}
			if err := c.Create(ctx, vsc); err != nil {
				return fmt.Errorf("seed VolumeSnapshotClass: %w", err)
			}
		}

		pvc := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "probe", Namespace: "tenant"},
			Spec:       corev1.PersistentVolumeClaimSpec{StorageClassName: &scName},
		}

		got := ""
		ex, err := exposer.NewRegistry(c, "crystal-backup-system").For(ctx, pvc)
		switch {
		case err == nil:
			got = ex.Kind()
		case errors.Is(err, exposer.ErrUnsupported):
			got = "skip"
		default:
			return fmt.Errorf("probe %q (vsclass=%v): unexpected error from Registry.For: %w",
				p.provisioner, p.hasVSClass, err)
		}

		if want := f.predict(p.provisioner, p.hasVSClass); got != want {
			return fmt.Errorf("the generated shell rule disagrees with internal/exposer.Registry.For:\n"+
				"  provisioner %q (VolumeSnapshotClass present: %v)\n"+
				"  Registry.For says %q, the generated rule would say %q\n"+
				"  The routing in internal/exposer/registry.go has changed shape. Update the emitted rule in\n"+
				"  hack/gen-preflight-table so the preflight script keeps telling the truth",
				p.provisioner, p.hasVSClass, got, want)
		}
	}
	return nil
}

// predict is the Go twin of the shell function render emits — the same decision, so the
// comparison above is meaningful. Keep the two in lockstep; that is the whole contract.
func (f *facts) predict(provisioner string, hasVSClass bool) string {
	if !hasVSClass {
		return "skip"
	}
	if strings.Contains(provisioner, f.cephfsMarker) {
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
	w("")
	w("# cb_exposer_for PROVISIONER HAS_SNAPSHOT_CLASS")
	w("#   HAS_SNAPSHOT_CLASS is 'yes' when some VolumeSnapshotClass in the cluster has")
	w("#   .driver == PROVISIONER. Prints the exposer kind, or 'skip'.")
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
