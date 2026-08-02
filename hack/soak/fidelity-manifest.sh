#!/bin/bash
# CrystalBackup restore drill — capture one volume's fidelity manifest.
# SPDX-License-Identifier: Apache-2.0
#
# ---------------------------------------------------------------------------------------------
# WHAT THIS IS
#
#   One record per path under ROOT, on stdout, in a form you can `diff` against the same thing
#   taken from the restored copy. Content is the least interesting half of restore fidelity —
#   what breaks in practice is the metadata: a mode that lost its setuid bit, an ACL that did not
#   survive, an mtime rounded to the second, a hardlink pair that came back as two separate files.
#
#   It is READ-ONLY. It opens regular files to hash them, never opens a FIFO (which would block
#   forever), and writes nothing outside its own temporary files.
#
# USAGE
#
#   bash fidelity-manifest.sh /path/to/volume [--allow-missing-tools]
#
#   Typically, piped into a pod that mounts the volume:
#
#     kubectl -n <ns> exec -i <pod> -- bash -s -- /data < fidelity-manifest.sh > before.txt
#
#   Run it on the SOURCE volume, and again on the RESTORED volume, then:
#
#     diff -u before.txt after.txt
#
#   An empty diff is the strongest statement this drill can make.
#
# WHY BASH AND NOT sh
#
#   Only `read -d ''` can walk a NUL-separated file list, and only a NUL-separated list survives
#   a filename containing a newline. Silently mis-parsing such a name would drop it from BOTH
#   manifests and the diff would come back clean — the exact failure this whole drill exists to
#   catch. The image the runbook prescribes has bash.
#
# WHY IT REFUSES TO RUN WITHOUT getfattr AND getfacl
#
#   Because the alternative is worse than useless. Without them, every entry's xattr and ACL
#   field would be empty ON BOTH SIDES, the diff would be clean, and the drill would report
#   success on precisely the two facets most likely to have regressed. --allow-missing-tools
#   proceeds anyway and stamps NOT COMPARED into the header, so a reader of the diff cannot
#   mistake absence for agreement. The same reasoning covers nanosecond mtimes: a `stat` that
#   cannot print them makes a one-second rounding invisible, so that is probed too.
#
#   An image that has everything:
#     debian:stable-slim, then  apt-get update && apt-get install -y attr acl
# ---------------------------------------------------------------------------------------------

set -u

ROOT=${1:-}
ALLOW_MISSING=no
[ "${2:-}" = --allow-missing-tools ] && ALLOW_MISSING=yes

if [ -z "$ROOT" ] || [ "$ROOT" = -h ] || [ "$ROOT" = --help ]; then
	printf 'usage: bash fidelity-manifest.sh <root> [--allow-missing-tools]\n' >&2
	exit 2
fi
if [ ! -d "$ROOT" ]; then
	printf 'FATAL: %s is not a directory — nothing to compare.\n' "$ROOT" >&2
	exit 1
fi

# --- what this run can and cannot see ------------------------------------------------------------

COVER_XATTR=yes
COVER_ACL=yes
COVER_NSEC=yes
MISSING=''

for t in stat find sort base64 sha256sum readlink mktemp; do
	command -v "$t" >/dev/null 2>&1 || MISSING="${MISSING}${t} "
done
if [ -n "$MISSING" ]; then
	printf 'FATAL: missing essential tools: %s\n' "$MISSING" >&2
	printf 'Use an image with GNU coreutils — debian:stable-slim will do.\n' >&2
	exit 1
fi

command -v getfattr >/dev/null 2>&1 || COVER_XATTR=no
command -v getfacl >/dev/null 2>&1 || COVER_ACL=no

# Nanosecond probe. A `stat` that does not understand %.9Y prints the format literally or a whole
# number; either way, the presence of a fractional part is what decides.
probe=$(stat -c '%.9Y' -- "$ROOT" 2>/dev/null)
case $probe in
*.*) : ;;
*) COVER_NSEC=no ;;
esac

if [ "$COVER_XATTR" = no ] || [ "$COVER_ACL" = no ] || [ "$COVER_NSEC" = no ]; then
	if [ "$ALLOW_MISSING" != yes ]; then
		printf 'FATAL: this image cannot measure everything the drill compares:\n' >&2
		[ "$COVER_XATTR" = no ] && printf '  - getfattr is absent: extended attributes would compare EQUAL on both sides\n' >&2
		[ "$COVER_ACL" = no ] && printf '  - getfacl is absent: POSIX ACLs would compare EQUAL on both sides\n' >&2
		[ "$COVER_NSEC" = no ] && printf '  - stat cannot print nanoseconds: a one-second rounding would be invisible\n' >&2
		# %s, not a bare format: a format string starting with "--" is an option to printf.
		printf '\nUse debian:stable-slim plus apt-get install -y attr acl, or re-run it with\n%s\n' \
			'--allow-missing-tools to proceed with those facets marked NOT COMPARED.' >&2
		exit 1
	fi
fi

# base64 -w0 is GNU. busybox has no -w, so fall back to stripping the newlines by hand.
if base64 -w0 </dev/null >/dev/null 2>&1; then
	b64() { base64 -w0; }
else
	b64() { base64 | tr -d '\n'; }
fi

xattr_dump() {
	if [ "$COVER_XATTR" != yes ]; then
		printf 'NOT-COMPARED'
		return
	fi
	# Every namespace (-m -), values base64-encoded, no dereference (-h). security.selinux is
	# excluded on purpose: it is a label the host's policy applies, not tenant data, and it
	# differs between two nodes for reasons that have nothing to do with the restore.
	# sed, not grep, does the filtering: grep exits 1 when it prints nothing, which under a
	# pipeline that anyone later adds `set -o pipefail` to would turn "this file has no xattrs"
	# into a fatal error.
	{ getfattr -h -d -m - -e base64 -- "$1" 2>/dev/null || true; } |
		sed -e '/^#/d' -e '/^$/d' -e '/^security\.selinux=/d' | LC_ALL=C sort | b64
}

acl_dump() {
	if [ "$COVER_ACL" != yes ]; then
		printf 'NOT-COMPARED'
		return
	fi
	# -c drops the comment header (it names the file, and the two roots have different names);
	# -n keeps ids numeric, because a name would be measuring the image's /etc/passwd.
	{ getfacl -c -n -- "$1" 2>/dev/null || true; } | sed -e '/^$/d' | b64
}

# --- pass one: the raw records ---------------------------------------------------------------------

PATHS=$(mktemp) || exit 1
RECS=$(mktemp) || exit 1
BODY=$(mktemp) || exit 1
GRPFILE=''
trap 'rm -f "$PATHS" "$RECS" "$GRPFILE" "$BODY"' EXIT HUP INT TERM

cd "$ROOT" || exit 1
find . -mindepth 1 -print0 | LC_ALL=C sort -z >"$PATHS"

n=0
while IFS= read -r -d '' p; do
	rel=${p#./}

	# -L first: -f follows symlinks and would classify a link to a file as a file.
	if [ -L "$p" ]; then t=l
	elif [ -d "$p" ]; then t=d
	elif [ -p "$p" ]; then t=p
	elif [ -f "$p" ]; then t=f
	else t=o; fi

	meta=$(stat -c '%a %u %g %h %s %b %i' -- "$p" 2>/dev/null) || continue
	# shellcheck disable=SC2086
	set -- $meta
	mode=$1 uid=$2 gid=$3 nlink=$4 size=$5 blocks=$6 inode=$7
	if [ "$COVER_NSEC" = yes ]; then
		mtime=$(stat -c '%.9Y' -- "$p")
	else
		mtime="$(stat -c '%Y' -- "$p").NOT-COMPARED"
	fi

	content='-'
	density='-'
	case $t in
	f)
		# Redirected, not passed by name: sha256sum escapes names containing newlines or
		# backslashes, and a volume worth testing has both.
		content=$(sha256sum <"$p" | cut -d' ' -f1)
		# Sparseness as a comparable fact rather than a raw block count: two volumes on two
		# storage classes legitimately differ in blocks allocated, but "this file has holes"
		# should survive a restore, and when it does not that is worth a sentence in the report.
		if [ "$size" -gt 0 ]; then
			if [ $((blocks * 512)) -lt "$size" ]; then density=sparse; else density=dense; fi
		fi
		;;
	d)
		# A directory's size is its own bookkeeping and differs between filesystems.
		size='-'
		;;
	l)
		content=$(readlink -- "$p" | tr -d '\n' | b64)
		;;
	*) : ;;
	esac

	printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s|%s\n' \
		"$(printf '%s' "$rel" | b64)" "$t" "$mode" "$uid" "$gid" "$nlink" "$size" \
		"$mtime" "$density" "$inode" "$content" "$(xattr_dump "$p")" "$(acl_dump "$p")" \
		>>"$RECS"
	n=$((n + 1))
done <"$PATHS"

# Inode numbers are not comparable across two volumes, but the GROUPING they induce is: two paths
# that share an inode on the source must share one on the restore, or a hardlink pair came back as
# two independent copies — which restores the bytes and silently doubles the space, forever.
#
# Two details that a counter would get wrong, and both were found by running this against a
# deliberately damaged copy:
#
#   * DIRECTORIES ARE EXCLUDED. A directory's link count is 2 plus its subdirectory count, which
#     is structure, not hardlinking. Including them put every leaf directory in a "group" of one.
#   * THE GROUP IS NAMED AFTER ITS FIRST PATH, not numbered. With a counter, breaking one
#     hardlink renumbers every group after it, and a single real defect prints as a diff against
#     half the manifest. Named by path, a change stays local to the thing that changed.
GRPFILE=$(mktemp) || exit 1
awk -F'\t' '$2 != "d" && $6 > 1 { if (!($10 in g)) g[$10] = $1; print $10 "\t" g[$10] }' "$RECS" |
	LC_ALL=C sort -u >"$GRPFILE"

# --- output ------------------------------------------------------------------------------------

# FILENAME, not the usual NR == FNR, and this is not pedantry — it is a bug that was in this
# script and that a damaged-copy test found. `NR == FNR` means "still reading the first file"
# ONLY IF the first file has records in it. A volume with no hardlinks at all produces an EMPTY
# group file, awk then never reads a record from it, and every record of the SECOND file
# satisfies NR == FNR and is skipped. The manifest came out with a correct header and NO ENTRIES
# — which, compared against another empty manifest, is a clean diff and a false pass on
# everything. Comparing FILENAME is immune to it.
awk -F'\t' -v OFS='\t' -v GF="$GRPFILE" '
  FILENAME == GF { if (NF == 2) grp[$1] = $2; next }
  { $10 = ($10 in grp) ? "link:" grp[$10] : "-"; print }
' "$GRPFILE" "$RECS" >"$BODY"

# And the belt to that brace: the body must have exactly as many records as the walk counted. Any
# future rewrite of the two awk passes that silently drops records fails here instead of shipping
# an emptier manifest than the volume.
emitted=$(awk 'END { print NR + 0 }' "$BODY")
if [ "$emitted" != "$n" ]; then
	printf 'FATAL: walked %s entries but emitted %s records. Refusing to print a manifest that\n' \
		"$n" "$emitted" >&2
	printf 'does not describe the whole volume — an incomplete manifest compares CLEAN against\n' >&2
	printf 'another incomplete one, which is the one outcome this drill must never produce.\n' >&2
	exit 1
fi

printf '# crystalbackup fidelity manifest v1\n'
printf '# root                %s\n' "$ROOT"
printf '# entries             %s\n' "$n"
if [ "$COVER_XATTR" = yes ]; then
	printf '# extended attributes compared\n'
else
	printf '# extended attributes NOT COMPARED (getfattr absent)\n'
fi
if [ "$COVER_ACL" = yes ]; then
	printf '# POSIX ACLs          compared\n'
else
	printf '# POSIX ACLs          NOT COMPARED (getfacl absent)\n'
fi
if [ "$COVER_NSEC" = yes ]; then
	printf '# mtime resolution    nanosecond\n'
else
	printf '# mtime resolution    SECOND ONLY (stat cannot print nanoseconds)\n'
fi
printf '# columns             path(b64) type mode uid gid nlink size mtime density linkgroup content xattr|acl\n'
printf '#\n'

cat "$BODY"

printf '# end %s entries\n' "$n"
