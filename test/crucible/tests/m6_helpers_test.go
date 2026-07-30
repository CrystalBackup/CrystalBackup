//go:build crucible

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

package crucible

import (
	"encoding/base64"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ---------------------------------------------------------------------------
// M6 restore-fidelity gate — the corpus, the manifest format, and the diff.
//
// The gate answers ONE question: does a restore give back exactly what was backed
// up? "Exactly" is the whole point — a matching file count is not evidence of
// anything. So the comparison is a per-path MANIFEST: content digest plus every
// piece of metadata a backup tool is supposed to carry (mode, setuid/setgid/
// sticky, numeric ownership, link counts and the hardlink partition, nanosecond
// mtimes, symlink targets, extended attributes, POSIX ACLs, and allocated blocks
// for sparse files). It is generated on the source volume BEFORE the backup and
// re-generated on the RESTORED volume, then diffed field by field in Go.
//
// Three rules this file is written to obey:
//
//  1. NOTHING HERE CAN SELF-DISABLE. There is no enable flag, no conditional
//     Skip(), no configurable tolerance. If a tool is missing, if the manifest
//     job cannot run, if the log came back truncated — the gate FAILS. A skip
//     reads as a pass in every summary a human actually looks at, which is how
//     the operator-readiness check stayed silently disabled from M1 to M4.
//
//  2. THE RESTORE TARGET IS AN EMPTY VOLUME. The gate restores through a
//     ClusterRestore into a namespace that does not exist yet, so the PVC is
//     created from the snapshot's own pvcsize/pvcclass/pvcmodes tags and the
//     mover writes into a freshly-formatted filesystem. Restoring in place over
//     the source would make most of this file lie: an xattr, an ACL or a mode
//     that restic failed to restore would still be sitting on the pre-existing
//     file, and the diff would come back green.
//
//  3. FACETS ARE MEASURED, NOT EXCLUDED. Every deviation is classified by facet
//     and asserted by its own spec, so a single broken facet shows up as one red
//     line in the report next to the facets that hold — instead of being dropped
//     from the corpus to keep the run green. restic#5543 (content corruption when
//     restoring at scale to Rook-Ceph) is open upstream; the roadmap says the gate
//     exists WHILE it is open, so content is measured with per-file digests and
//     per-16MiB-window digests that name the offset of a corrupted region.
// ---------------------------------------------------------------------------

const (
	// m6CorpusRoot is the subdirectory the corpus lives in, inside the PVC. It is deliberately
	// not the volume root: ext4 puts a lost+found there, which is mkfs bookkeeping rather than
	// tenant data, and comparing it would add noise without adding evidence.
	m6CorpusRoot = "/data/corpus"

	// m6Image carries bash + GNU coreutils/findutils out of the box; attr and acl are added at
	// runtime. busybox is not usable here: its stat has no nanosecond precision and it ships
	// neither getfattr nor getfacl, so the two facets most likely to regress would go unmeasured.
	m6Image = "debian:12-slim"

	// m6FieldSep separates manifest fields. ASCII US (0x1F) rather than a space or a tab because
	// the corpus deliberately contains names with spaces, tabs and quotes.
	m6FieldSep = "\x1f"
	// m6FieldCount is the exact number of fields in a manifest record; a record of any other
	// width fails the parse rather than being tolerated.
	m6FieldCount = 14

	m6ManifestBegin = "===CRUCIBLE-M6-MANIFEST-BEGIN==="
	m6ManifestEnd   = "===CRUCIBLE-M6-MANIFEST-END==="
	// m6CountPrefix introduces the entry count the generator itself counted. The parser compares
	// it against what it actually parsed, so a container log that was rotated or truncated fails
	// loudly instead of silently comparing two shorter manifests to each other.
	m6CountPrefix = "===CRUCIBLE-M6-COUNT="

	// m6ChunkBytes is the window size of the per-region digests recorded for large files. It
	// exists to make a restic#5543-style corruption legible: "the bytes at 0x0C000000-0x0D000000
	// differ" is an actionable bug report, "the sha256 of a 384 MiB file differs" is not.
	m6ChunkBytes = 16 << 20
)

// m6Facet names one property of restore fidelity. Each facet is asserted by its own spec so the
// crucible report shows which properties hold and which do not, one line each.
type m6Facet string

const (
	m6FacetPresence    m6Facet = "presence"
	m6FacetContent     m6Facet = "content"
	m6FacetPermissions m6Facet = "permissions"
	m6FacetXattrs      m6Facet = "xattrs"
	m6FacetACL         m6Facet = "posix-acl"
	m6FacetTimestamps  m6Facet = "timestamps"
	m6FacetSymlinks    m6Facet = "symlinks"
	m6FacetHardlinks   m6Facet = "hardlinks"
	m6FacetSparse      m6Facet = "sparse"
)

// m6Entry is one manifest record: one path in the corpus, with everything about it that a
// restore is supposed to reproduce.
//
// Two things are deliberately NOT in here, and their absence is a decision rather than an
// oversight:
//
//   - atime — measuring it invalidates it. Generating the manifest reads every file, so the
//     "before" and "after" access times can never be compared meaningfully. No backup tool
//     promises it either.
//   - ctime — no interface exists to set it. Every restored inode necessarily has a fresh one.
//
// Inode is recorded but never compared directly: only the PARTITION it induces (which paths
// share an inode) is a fidelity property. Blocks is recorded but only read by the sparse rule,
// because exact allocation is a filesystem's business, not the backup's.
type m6Entry struct {
	Path   string // relative to m6CorpusRoot
	Type   string // f regular, d directory, l symlink, p FIFO, o other
	Mode   string // octal, including setuid/setgid/sticky
	UID    string
	GID    string
	Nlink  string
	Size   string // bytes for f and l; "-" for d (directory size is filesystem bookkeeping)
	MTime  string // seconds.nanoseconds
	Blocks string // 512-byte blocks allocated, "-" unless f
	Inode  string // never compared as a value — only the hardlink partition it induces
	Extra  string // f: sha256 of the content; l: base64 of the target; otherwise "-"
	Xattrs string // base64 of the sorted getfattr dump
	ACL    string // base64 of the getfacl dump ("-" for symlinks)
	Chunks string // space-separated per-m6ChunkBytes digests for large files, else "-"
}

// m6Delta is one measured deviation between the source and the restored volume.
type m6Delta struct {
	Facet m6Facet
	Path  string
	Field string
	Pre   string
	Post  string
}

func (d m6Delta) String() string {
	return fmt.Sprintf("[%s] %s — %s: source %q → restored %q", d.Facet, d.Path, d.Field, d.Pre, d.Post)
}

// m6RequireS3 asserts — never skips — that the S3 coordinates the data path needs are present.
//
// The rest of the suite Skip()s here, which is defensible for a scenario that merely *uses* the
// data path. It is not defensible for the gate that certifies restores: a fidelity gate that
// quietly does nothing when the environment is half-configured is worse than no gate, because
// the run still reports green. If the bucket is not configured, this run cannot answer the
// question it exists to answer, and says so.
func m6RequireS3() {
	GinkgoHelper()
	for _, key := range []string{"S3_ENDPOINT", "S3_BUCKET", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY"} {
		Expect(os.Getenv(key)).NotTo(BeEmpty(),
			"$%s is empty: the restore-fidelity gate cannot back anything up, and it FAILS rather "+
				"than skips — run via `mise run test` so terraform facts + secrets are loaded", key)
	}
}

// m6ToolJob runs a bash script in m6Image with pvcName mounted read-write at /data, waits for it
// to finish, and returns success + the pod log. Same shape as m2VolumeJob, but with the toolchain
// the fidelity manifest needs and a budget sized for half a gigabyte of hashing.
func m6ToolJob(ns, pvcName, name, script string) (bool, string) {
	GinkgoHelper()
	backoff, deadline := int32(0), int64(2400)
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{GenerateName: name + "-", Namespace: ns},
		Spec: batchv1.JobSpec{
			BackoffLimit:          &backoff,
			ActiveDeadlineSeconds: &deadline,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{{
						Name:    "fidelity",
						Image:   m6Image,
						Command: []string{"/bin/bash", "-c"},
						Args:    []string{m6ToolPrelude + "\n" + script},
						VolumeMounts: []corev1.VolumeMount{
							{Name: "data", MountPath: "/data"},
						},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("100m"),
								corev1.ResourceMemory: resource.MustParse("128Mi"),
							},
						},
					}},
					Volumes: []corev1.Volume{{
						Name: "data",
						VolumeSource: corev1.VolumeSource{
							PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: pvcName},
						},
					}},
				},
			},
		},
	}
	Expect(k8s.Create(ctx, job)).To(Succeed(), "create %s Job in %s", name, ns)
	defer func() {
		_ = k8s.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground))
	}()

	var final batchv1.Job
	Eventually(func(g Gomega) {
		g.Expect(k8s.Get(ctx, client.ObjectKeyFromObject(job), &final)).To(Succeed())
		g.Expect(final.Status.Succeeded+final.Status.Failed).To(BeNumerically(">", 0),
			"%s Job %s/%s has not finished (active=%d)", name, ns, job.Name, final.Status.Active)
	}, 45*time.Minute, 10*time.Second).Should(Succeed())
	return final.Status.Succeeded > 0, m2JobLog(ns, job.Name)
}

// m6ToolPrelude installs and then PROVES the toolchain the manifest depends on. Every branch
// ends in `exit 1`: a corpus seeded without setfattr would be a corpus that silently stops
// testing xattrs, and a manifest generated without nanosecond stat would compare two identical
// placeholder strings forever.
const m6ToolPrelude = `set -euo pipefail
export LC_ALL=C DEBIAN_FRONTEND=noninteractive

if ! command -v getfattr >/dev/null 2>&1 || ! command -v getfacl >/dev/null 2>&1; then
  installed=0
  for attempt in 1 2 3; do
    if apt-get -o Acquire::Retries=3 -qq update >/dev/null 2>&1 \
       && apt-get -o Acquire::Retries=3 -qq install -y --no-install-recommends attr acl >/dev/null 2>&1; then
      installed=1; break
    fi
    echo "WARN: apt attempt ${attempt} failed, retrying" >&2
    sleep 10
  done
  if [ "${installed}" != "1" ]; then
    echo "FATAL: could not install attr+acl — the xattr and POSIX-ACL facets cannot be measured."
    echo "FATAL: this run cannot certify restore fidelity; it FAILS rather than measuring less."
    exit 1
  fi
fi

for t in bash stat find sort base64 sha256sum dd mkfifo seq touch chown chmod \
         getfattr setfattr getfacl setfacl; do
  command -v "$t" >/dev/null 2>&1 || { echo "FATAL: required tool '$t' is missing"; exit 1; }
done

# Nanosecond mtimes are a fidelity property, so prove stat can actually report them. Without
# this, an unsupported '%.9Y' would print the literal format string on BOTH sides of the diff
# and the timestamp facet would pass while measuring nothing at all.
probe=$(mktemp)
touch -d '2020-01-02 03:04:05.123456789' "$probe"
probe_mtime=$(stat -c '%.9Y' -- "$probe")
rm -f "$probe"
if ! [[ "${probe_mtime}" =~ ^[0-9]+\.[0-9]{9}$ ]]; then
  echo "FATAL: stat cannot report nanosecond mtimes (got '${probe_mtime}')"; exit 1
fi
if [ "${probe_mtime}" != "1577934245.123456789" ]; then
  echo "FATAL: nanosecond mtime probe round-tripped wrong (got '${probe_mtime}')"; exit 1
fi
`

// m6SeedScript writes the corpus. Every entry is here because it breaks something in practice,
// not because it is easy to assert; the comment on each block says what it catches.
//
// Two deliberate omissions, stated rather than left to be discovered:
//   - device nodes (mknod c/b): the mover claims CAP_MKNOD for restores, but whether a device
//     node can be created in a tenant PVC at all depends on the kubelet's mount options, not on
//     the backup — a red here would be an environment property masquerading as a fidelity bug.
//     The FIFO below covers the "non-regular, non-symlink inode" shape.
//   - a filename containing a newline IS included; one containing a raw NUL cannot exist.
const m6SeedScript = `
ROOT=` + m6CorpusRoot + `
rm -rf "${ROOT}"
mkdir -p "${ROOT}"
cd "${ROOT}"

# ── content: the restic#5543 surface ────────────────────────────────────────
# large.bin spans ~6 packs at the mover's --pack-size 64 (MiB), which is the regime where the
# open upstream bug corrupts restored bytes at an offset inside a file. The medium/small fan-out
# exists so a corruption that only hits some files is still caught, and so the run has enough
# distinct blobs to be more than a single-object smoke test.
mkdir -p content
head -c 402653184 /dev/urandom > content/large.bin          # 384 MiB
for i in $(seq -w 1 32); do head -c 4194304 /dev/urandom > "content/medium-${i}.bin"; done
for i in $(seq -w 1 128); do head -c 4096 /dev/urandom > "content/small-${i}.bin"; done
: > content/empty.bin                                       # a 0-byte file is still a file
head -c 1 /dev/urandom > content/one-byte.bin

# ── sparse: the invisible regression ────────────────────────────────────────
# A sparse file restored dense passes every checksum ever written and quietly fills the volume.
# big.sparse is 2 GiB apparent for 128 KiB of data; holed.sparse has three data islands so a
# restore that only preserves a trailing hole is still caught.
mkdir -p sparse
head -c 65536 /dev/urandom > sparse/big.sparse
dd if=/dev/urandom of=sparse/big.sparse bs=64K count=1 seek=32767 conv=notrunc status=none
: > sparse/holed.sparse
dd if=/dev/urandom of=sparse/holed.sparse bs=64K count=1 seek=0    conv=notrunc status=none
dd if=/dev/urandom of=sparse/holed.sparse bs=64K count=1 seek=4096 conv=notrunc status=none
dd if=/dev/urandom of=sparse/holed.sparse bs=64K count=1 seek=8191 conv=notrunc status=none

# ── permissions and ownership ───────────────────────────────────────────────
# setuid/setgid/sticky are dropped by any restore that chowns AFTER chmod (chown clears them),
# which is the classic ordering bug. The numeric owners are intentionally uids with no passwd
# entry: restic records the user NAME at backup time and prefers it at restore time, so a named
# owner would measure the two images' /etc/passwd rather than the backup.
mkdir -p perm
echo private > perm/mode-0400.txt   ; chmod 0400 perm/mode-0400.txt
echo none    > perm/mode-0000.txt   ; chmod 0000 perm/mode-0000.txt
echo suid    > perm/setuid.bin      ; chmod 4755 perm/setuid.bin
echo sgid    > perm/setgid.bin      ; chmod 2755 perm/setgid.bin
echo both    > perm/setuid-setgid.bin ; chmod 6755 perm/setuid-setgid.bin
mkdir -p perm/sticky-dir            ; chmod 1777 perm/sticky-dir
mkdir -p perm/setgid-dir            ; chmod 2775 perm/setgid-dir
echo owned   > perm/owned-file.txt  ; chown 1234:5678 perm/owned-file.txt
mkdir -p perm/owned-dir             ; chown 4242:4242 perm/owned-dir ; chmod 0750 perm/owned-dir

# ── extended attributes ─────────────────────────────────────────────────────
# The mover stores xattrs unconditionally and passes no xattr filter at restore, so this is a
# promise the product makes (R10). Binary values and a 512-byte value are here because a naive
# implementation that round-trips through a string breaks exactly on those.
mkdir -p xattr
echo carrier > xattr/attrs.bin
setfattr -n user.crystalbackup.simple -v "must-survive-restore"  xattr/attrs.bin
setfattr -n user.crystalbackup.binary -v "0sAAECA/8AAQ=="        xattr/attrs.bin
setfattr -n user.crystalbackup.long   -v "$(head -c 512 /dev/zero | tr '\0' 'x')" xattr/attrs.bin
mkdir -p xattr/dir-with-xattr
setfattr -n user.crystalbackup.dir    -v "directories-carry-xattrs-too" xattr/dir-with-xattr
echo many > xattr/many.bin
for i in $(seq 1 16); do setfattr -n "user.cb.attr${i}" -v "value-${i}" xattr/many.bin; done

# ── POSIX ACLs ──────────────────────────────────────────────────────────────
# ACLs travel as system.posix_acl_access / system.posix_acl_default xattrs; the DEFAULT ACL on a
# directory is the half that gets forgotten, because nothing in the directory's own mode hints
# at it. Both are captured through getfacl, which is the authoritative rendering.
mkdir -p acl
echo acl-file > acl/file.txt
setfacl -m u:1234:rwx,g:5678:r-x,m::rwx acl/file.txt
mkdir -p acl/dir
setfacl    -m u:4242:r-x acl/dir
setfacl -d -m u:1234:rwx,g:5678:r-x,o::--- acl/dir

# ── symbolic links ──────────────────────────────────────────────────────────
# The dangling link is the interesting one: a restore that resolves targets instead of copying
# them fails there first. sym-rel is lchown'd, sym-abs has its own (link, not target) mtime.
mkdir -p links noms
ln -s ../content/small-001.bin       links/sym-rel
ln -s /data/corpus/content/empty.bin links/sym-abs
ln -s ./nowhere-at-all               links/sym-broken
ln -s ../content                     links/sym-to-dir
ln -s "../noms/éclair ☃ übergroß.txt" links/sym-unicode
chown -h 1234:5678 links/sym-rel

# ── hard links ──────────────────────────────────────────────────────────────
# Three names, one inode, one of them in a different directory. A restore that writes three
# independent copies matches every checksum and triples the volume's footprint.
echo hardlink-corpus > links/hard-a
ln links/hard-a links/hard-b
mkdir -p deep/l1
ln links/hard-a deep/l1/hard-c

# ── special inodes ──────────────────────────────────────────────────────────
mkdir -p special
mkfifo special/fifo
chmod 0640 special/fifo
mkdir -p special/empty-dir

# ── names ───────────────────────────────────────────────────────────────────
# Non-ASCII, whitespace, glob metacharacters, quotes, a leading dash, a trailing space and a
# newline. Glob metacharacters matter beyond aesthetics: the restore path forwards user globs to
# restic as --include patterns, so a name containing '*' is a real hazard.
echo e > "noms/éclair ☃ übergroß.txt"
echo j > "noms/日本語ファイル.txt"
echo g > "noms/Ωμέγα-ç-√.txt"
printf 'x\n' > "$(printf 'noms/space and\ttab.txt')"
printf 'x\n' > "$(printf 'noms/with\nnewline.txt')"
echo q > "noms/quote'and\"backslash\\.txt"
echo d > "noms/-leading-dash.txt"
echo t > "noms/trailing-space .txt"
echo a > "noms/asterisk*and?question.txt"
echo c > "noms/colon:and|pipe.txt"
echo n > "noms/$(printf 'n%.0s' $(seq 1 240)).txt"

# ── deep tree ───────────────────────────────────────────────────────────────
d=deep
for i in $(seq 1 16); do d="${d}/level-${i}"; mkdir -p "${d}"; done
echo leaf > "${d}/leaf.txt"
mkdir -p "deep/$(printf 'w%.0s' $(seq 1 240))"
echo wide > "deep/$(printf 'w%.0s' $(seq 1 240))/file.txt"

# ── timestamps, applied LAST ────────────────────────────────────────────────
# Directory mtimes are rewritten by every child creation, so they are set after the tree is
# complete. Restoring a directory's mtime requires a second pass after its children are written;
# a restorer that forgets it fails here and nowhere else.
mkdir -p times
echo t1 > times/epoch-plus-one.txt ; touch -d '@1'                              times/epoch-plus-one.txt
echo t2 > times/y2038.txt          ; touch -d '2038-01-19 03:14:08'             times/y2038.txt
echo t3 > times/nanoseconds.txt    ; touch -d '2020-01-02 03:04:05.123456789'   times/nanoseconds.txt
echo t4 > times/far-future.txt     ; touch -d '2099-12-31 23:59:59.999999999'   times/far-future.txt
mkdir -p times/dated-dir           ; touch -d '2011-11-11 11:11:11.111111111'   times/dated-dir
touch -h -d '2001-09-09 01:46:40.000000000' links/sym-abs

sync
echo "corpus seeded: $(find "${ROOT}" -mindepth 1 | wc -l) entries"
`

// m6ManifestScript walks the corpus and prints one record per path between the begin/end
// markers, followed by the count it counted. Everything it emits is either a fidelity property
// or (inode, blocks) a value used to derive one.
//
// A var rather than a const only because the chunk size is injected through strconv.
var m6ManifestScript = `
ROOT=` + m6CorpusRoot + `
[ -d "${ROOT}" ] || { echo "FATAL: ${ROOT} is absent — nothing to compare"; exit 1; }
SEP=$'\x1f'
CHUNK=` + strconv.Itoa(m6ChunkBytes) + `

xattr_dump() {
  # Every namespace (-m -), values base64-encoded, no dereference. The '# file:' header and
  # blank lines are dropped, and the remainder sorted, so the encoding is stable. security.selinux
  # is excluded on purpose: it is a kernel label applied by the host's policy, not tenant data.
  # sed (not grep) does the filtering: grep exits 1 when it prints nothing, which under
  # 'set -o pipefail' would turn "this file has no xattrs" into a fatal error.
  { getfattr -h -d -m - -e base64 -- "$1" 2>/dev/null || true; } \
    | sed -e '/^#/d' -e '/^$/d' -e '/^security\.selinux=/d' | LC_ALL=C sort | base64 -w0
}

acl_dump() {
  # -c drops the comment header, -n keeps ids numeric (a name would measure /etc/passwd).
  { getfacl -c -n -- "$1" 2>/dev/null || true; } | sed -e '/^$/d' | base64 -w0
}

chunk_digests() {
  local f="$1" size="$2" off=0 out=""
  while [ "${off}" -lt "${size}" ]; do
    out="${out} $(dd if="${f}" bs="${CHUNK}" skip=$((off / CHUNK)) count=1 status=none | sha256sum | cut -d' ' -f1)"
    off=$((off + CHUNK))
  done
  printf '%s' "${out# }"
}

cd "${ROOT}"
n=0
echo "` + m6ManifestBegin + `"
while IFS= read -r -d '' p; do
  rel="${p#./}"
  # -L first: -f follows symlinks and would classify a link to a file as a file.
  if   [ -L "${p}" ]; then t=l
  elif [ -d "${p}" ]; then t=d
  elif [ -p "${p}" ]; then t=p
  elif [ -f "${p}" ]; then t=f
  else t=o; fi

  meta=$(stat -c '%a %u %g %h %s %b %i' -- "${p}")
  # shellcheck disable=SC2086
  set -- ${meta}
  mode=$1 uid=$2 gid=$3 nlink=$4 size=$5 blocks=$6 inode=$7
  mtime=$(stat -c '%.9Y' -- "${p}")

  extra='-'; chunks='-'; acl='-'
  case "${t}" in
    f)
      # Redirect rather than pass the name: sha256sum escapes names containing newlines or
      # backslashes, and the corpus has both.
      extra=$(sha256sum < "${p}" | cut -d' ' -f1)
      if [ "${size}" -gt "${CHUNK}" ]; then chunks=$(chunk_digests "${p}" "${size}"); fi
      acl=$(acl_dump "${p}")
      ;;
    d)
      size='-'; blocks='-'
      acl=$(acl_dump "${p}")
      ;;
    l)
      blocks='-'
      extra=$(readlink -- "${p}" | tr -d '\n' | base64 -w0)
      ;;
    *)
      # A FIFO must never be opened for reading here: it would block forever.
      size='-'; blocks='-'
      acl=$(acl_dump "${p}")
      ;;
  esac
  xa=$(xattr_dump "${p}")
  pb=$(printf '%s' "${rel}" | base64 -w0)

  printf '%s\n' "${pb}${SEP}${t}${SEP}${mode}${SEP}${uid}${SEP}${gid}${SEP}${nlink}${SEP}${size}${SEP}${mtime}${SEP}${blocks}${SEP}${inode}${SEP}${extra}${SEP}${xa}${SEP}${acl}${SEP}${chunks}"
  n=$((n + 1))
done < <(find . -mindepth 1 -print0 | LC_ALL=C sort -z)
echo "` + m6CountPrefix + `${n}==="
echo "` + m6ManifestEnd + `"
`

// m6SeedCorpus provisions ns/pvcName on ceph-block (Rook-Ceph RBD — the storage the gate is
// specified against) and writes the corpus onto it.
func m6SeedCorpus(ns, pvcName, capacity string) {
	GinkgoHelper()
	ensureNamespace(ns)
	sc := "ceph-block"
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: pvcName, Namespace: ns},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: &sc,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(capacity)},
			},
		},
	}
	Expect(client.IgnoreAlreadyExists(k8s.Create(ctx, pvc))).To(Succeed(), "create PVC %s/%s", ns, pvcName)

	ok, log := m6ToolJob(ns, pvcName, "m6-seed", m6SeedScript)
	Expect(ok).To(BeTrue(), "seeding the fidelity corpus on %s/%s must succeed:\n%s", ns, pvcName, log)
}

// m6CaptureManifest runs the manifest generator against ns/pvcName and parses its output.
func m6CaptureManifest(ns, pvcName, phase string) map[string]*m6Entry {
	GinkgoHelper()
	ok, log := m6ToolJob(ns, pvcName, "m6-manifest-"+phase, m6ManifestScript)
	Expect(ok).To(BeTrue(), "the %s manifest job on %s/%s must succeed:\n%s", phase, ns, pvcName, log)
	return m6ParseManifest(phase, log)
}

// m6ParseManifest turns a manifest job's log into entries, failing on anything it cannot fully
// account for: no markers, a short record, a bad base64 path, or a count that disagrees with what
// was parsed (a rotated or truncated container log).
func m6ParseManifest(phase, log string) map[string]*m6Entry {
	GinkgoHelper()

	begin := strings.Index(log, m6ManifestBegin)
	Expect(begin).To(BeNumerically(">=", 0),
		"the %s manifest has no begin marker — the generator did not run to the walk:\n%s", phase, log)
	end := strings.Index(log, m6ManifestEnd)
	Expect(end).To(BeNumerically(">", begin),
		"the %s manifest has no end marker — its output was cut off:\n%s", phase, log)

	entries := map[string]*m6Entry{}
	declared := -1
	for _, line := range strings.Split(log[begin+len(m6ManifestBegin):end], "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		if strings.HasPrefix(line, m6CountPrefix) {
			n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(line, m6CountPrefix), "==="))
			Expect(err).NotTo(HaveOccurred(), "unreadable %s manifest count line %q", phase, line)
			declared = n
			continue
		}
		f := strings.Split(line, m6FieldSep)
		Expect(f).To(HaveLen(m6FieldCount),
			"malformed %s manifest record (%d fields, want %d): %q", phase, len(f), m6FieldCount, line)
		raw, err := base64.StdEncoding.DecodeString(f[0])
		Expect(err).NotTo(HaveOccurred(), "undecodable path in the %s manifest: %q", phase, f[0])
		e := &m6Entry{
			Path: string(raw), Type: f[1], Mode: f[2], UID: f[3], GID: f[4], Nlink: f[5],
			Size: f[6], MTime: f[7], Blocks: f[8], Inode: f[9], Extra: f[10],
			Xattrs: f[11], ACL: f[12], Chunks: f[13],
		}
		_, dup := entries[e.Path]
		Expect(dup).To(BeFalse(), "duplicate path %q in the %s manifest", e.Path, phase)
		entries[e.Path] = e
	}

	Expect(declared).To(BeNumerically(">=", 0),
		"the %s manifest carries no entry count — cannot prove the log was complete", phase)
	Expect(entries).To(HaveLen(declared),
		"the %s manifest is TRUNCATED: the generator counted %d entries, %d were readable in the log. "+
			"Comparing what survived would understate the diff, so this fails instead.",
		phase, declared, len(entries))
	Expect(entries).NotTo(BeEmpty(), "the %s manifest is empty — there is nothing to compare", phase)
	return entries
}

// m6LinkGroups maps every regular file to the canonical rendering of its hardlink group (the
// sorted set of paths sharing its inode). Inode NUMBERS are meaningless across two filesystems;
// the partition they induce is exactly the fidelity property.
func m6LinkGroups(m map[string]*m6Entry) map[string]string {
	byInode := map[string][]string{}
	for p, e := range m {
		if e.Type != "f" {
			continue
		}
		byInode[e.Inode] = append(byInode[e.Inode], p)
	}
	out := make(map[string]string, len(m))
	for _, paths := range byInode {
		slices.Sort(paths)
		key := strings.Join(paths, " + ")
		for _, p := range paths {
			out[p] = key
		}
	}
	return out
}

// m6SparseFiles are the corpus paths whose sparseness is itself the assertion.
var m6SparseFiles = []string{"sparse/big.sparse", "sparse/holed.sparse"}

// m6SparseDenseRatio is the hard-coded structural rule: a file that was sparse at the source must
// still occupy less than a quarter of its apparent size after a restore.
//
// This is NOT a tolerance knob, and it is deliberately not exposed anywhere. Block-for-block
// equality is the wrong assertion — allocation granularity is the filesystem's business and would
// produce a red that means nothing. "Still sparse" is the property that matters, because the
// failure mode being guarded against (--sparse silently stopping to work) turns a 128 KiB file
// into a 2 GiB one, which is three orders of magnitude away from any granularity effect.
const m6SparseDenseRatio = 4

// m6Diff compares two manifests field by field and returns every deviation, classified by facet.
func m6Diff(pre, post map[string]*m6Entry) []m6Delta {
	var deltas []m6Delta
	preGroups, postGroups := m6LinkGroups(pre), m6LinkGroups(post)

	paths := make([]string, 0, len(pre)+len(post))
	for p := range pre {
		paths = append(paths, p)
	}
	for p := range post {
		if _, ok := pre[p]; !ok {
			paths = append(paths, p)
		}
	}
	slices.Sort(paths)

	for _, p := range paths {
		a, inPre := pre[p]
		b, inPost := post[p]
		switch {
		case !inPost:
			deltas = append(deltas, m6Delta{m6FacetPresence, p, "exists", "present in the source", "MISSING after restore"})
			continue
		case !inPre:
			deltas = append(deltas, m6Delta{m6FacetPresence, p, "exists", "absent from the source", "INVENTED by the restore"})
			continue
		}
		if a.Type != b.Type {
			deltas = append(deltas, m6Delta{m6FacetPresence, p, "inode type", a.Type, b.Type})
			continue // every other field is meaningless once the kind changed
		}

		add := func(f m6Facet, field, x, y string) {
			if x != y {
				deltas = append(deltas, m6Delta{f, p, field, x, y})
			}
		}
		add(m6FacetPermissions, "mode", a.Mode, b.Mode)
		add(m6FacetPermissions, "uid", a.UID, b.UID)
		add(m6FacetPermissions, "gid", a.GID, b.GID)
		add(m6FacetTimestamps, "mtime", a.MTime, b.MTime)
		add(m6FacetXattrs, "xattrs", m6Readable(a.Xattrs), m6Readable(b.Xattrs))
		add(m6FacetACL, "acl", m6Readable(a.ACL), m6Readable(b.ACL))
		add(m6FacetHardlinks, "link count", a.Nlink, b.Nlink)
		add(m6FacetHardlinks, "hardlink group", preGroups[p], postGroups[p])

		switch a.Type {
		case "f":
			add(m6FacetContent, "size", a.Size, b.Size)
			add(m6FacetContent, "sha256", a.Extra, b.Extra)
			deltas = append(deltas, m6ChunkDeltas(p, a, b)...)
		case "l":
			add(m6FacetSymlinks, "target", m6Readable(a.Extra), m6Readable(b.Extra))
			add(m6FacetSymlinks, "target length", a.Size, b.Size)
		}
	}

	return append(deltas, m6SparseDeltas(pre, post)...)
}

// m6ChunkDeltas turns a whole-file digest mismatch into the byte range that actually differs.
// This is the diagnostic restic#5543 is reported in ("Unexpected content in X, starting at offset
// N") — without it a corrupted 384 MiB file is one useless line saying the hashes differ.
func m6ChunkDeltas(path string, a, b *m6Entry) []m6Delta {
	if a.Chunks == "-" || b.Chunks == "-" || a.Chunks == b.Chunks {
		return nil
	}
	x, y := strings.Fields(a.Chunks), strings.Fields(b.Chunks)
	if len(x) != len(y) {
		return []m6Delta{{m6FacetContent, path, "region count",
			fmt.Sprintf("%d regions of %d bytes", len(x), m6ChunkBytes),
			fmt.Sprintf("%d regions", len(y))}}
	}
	var out []m6Delta
	for i := range x {
		if x[i] != y[i] {
			out = append(out, m6Delta{m6FacetContent, path,
				fmt.Sprintf("bytes 0x%X–0x%X", i*m6ChunkBytes, (i+1)*m6ChunkBytes),
				x[i], y[i]})
		}
	}
	return out
}

// m6SparseDeltas applies the sparseness rule, and also checks the SOURCE side: a corpus whose
// "sparse" file is not actually sparse would make the whole facet vacuous, so that is reported as
// a deviation too rather than passing quietly.
func m6SparseDeltas(pre, post map[string]*m6Entry) []m6Delta {
	var out []m6Delta
	for _, p := range m6SparseFiles {
		a, okA := pre[p]
		if !okA {
			out = append(out, m6Delta{m6FacetSparse, p, "source file",
				"a sparse file the corpus claims to seed", "ABSENT from the source manifest"})
			continue
		}
		b, okB := post[p]
		if !okB {
			continue // already reported by the presence facet
		}
		apparent, err1 := strconv.ParseInt(a.Size, 10, 64)
		preAlloc, err2 := strconv.ParseInt(a.Blocks, 10, 64)
		postAlloc, err3 := strconv.ParseInt(b.Blocks, 10, 64)
		if err1 != nil || err2 != nil || err3 != nil || apparent <= 0 {
			out = append(out, m6Delta{m6FacetSparse, p, "measurement",
				fmt.Sprintf("size=%q blocks=%q", a.Size, a.Blocks),
				fmt.Sprintf("size=%q blocks=%q", b.Size, b.Blocks)})
			continue
		}
		preBytes, postBytes := preAlloc*512, postAlloc*512
		limit := apparent / m6SparseDenseRatio
		if preBytes >= limit {
			out = append(out, m6Delta{m6FacetSparse, p, "source sparseness",
				fmt.Sprintf("expected < %d allocated bytes for %d apparent", limit, apparent),
				fmt.Sprintf("the SOURCE file allocates %d bytes — the corpus is not testing sparseness", preBytes)})
			continue
		}
		if postBytes >= limit {
			out = append(out, m6Delta{m6FacetSparse, p, "restored sparseness",
				fmt.Sprintf("%d bytes allocated for %d apparent", preBytes, apparent),
				fmt.Sprintf("%d bytes allocated — restored DENSE (%.1f× the source footprint)",
					postBytes, float64(postBytes)/float64(max(preBytes, 1)))})
		}
	}
	return out
}

// m6Readable renders a base64 manifest field as text when it decodes to something printable, so
// a failure message shows `user.crystalbackup.simple=0s...` rather than an opaque blob. Falls
// back to the raw encoding, and renders the empty field as an explicit "(none)" — "" and "no
// xattrs at all" must not look the same in a diff.
func m6Readable(v string) string {
	if v == "" {
		return "(none)"
	}
	if v == "-" {
		return v
	}
	raw, err := base64.StdEncoding.DecodeString(v)
	if err != nil {
		return v
	}
	s := strings.TrimSpace(strings.ReplaceAll(string(raw), "\n", " ; "))
	if s == "" {
		return "(empty)"
	}
	return s
}

// m6DeltasFor filters the measured deviations down to one facet.
func m6DeltasFor(deltas []m6Delta, facet m6Facet) []m6Delta {
	var out []m6Delta
	for _, d := range deltas {
		if d.Facet == facet {
			out = append(out, d)
		}
	}
	return out
}

// m6Report renders deviations for a failure message: every one of them up to a cap, then a count
// of what was elided. The cap exists so a total restore failure does not produce a megabyte of
// output; it never hides the fact that more exist.
func m6Report(deltas []m6Delta) string {
	const shown = 40
	if len(deltas) == 0 {
		return "(no deviation)"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d deviation(s):\n", len(deltas))
	for i, d := range deltas {
		if i == shown {
			fmt.Fprintf(&b, "  … and %d more\n", len(deltas)-shown)
			break
		}
		fmt.Fprintf(&b, "  %s\n", d)
	}
	return b.String()
}

// m6RequirePaths asserts that named corpus paths are present on BOTH sides with identical
// content. Used by the specs that call out a specific hazard (a multi-pack file, hostile names, a
// deep tree) so the report carries a line per hazard instead of one undifferentiated verdict.
func m6RequirePaths(pre, post map[string]*m6Entry, paths []string, what string) {
	GinkgoHelper()
	for _, p := range paths {
		a, okA := pre[p]
		Expect(okA).To(BeTrue(), "%s: the corpus never seeded %q — the check would be vacuous", what, p)
		b, okB := post[p]
		Expect(okB).To(BeTrue(), "%s: %q did not come back from the restore", what, p)
		Expect(b.Type).To(Equal(a.Type), "%s: %q came back as a different kind of inode", what, p)
		Expect(b.Extra).To(Equal(a.Extra),
			"%s: %q differs (source %s / restored %s)", what, p, m6Readable(a.Extra), m6Readable(b.Extra))
	}
}

// m6SortedPaths lists the corpus paths of a manifest, sorted — used only in failure messages.
func m6SortedPaths(m map[string]*m6Entry) []string {
	out := make([]string, 0, len(m))
	for p := range m {
		out = append(out, p)
	}
	slices.Sort(out)
	return out
}
