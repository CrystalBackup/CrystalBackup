#!/bin/sh
# CrystalBackup soak collector — pull the archive out, and refuse to be quiet about what is
# missing from it.
# SPDX-License-Identifier: Apache-2.0
#
# ---------------------------------------------------------------------------------------------
# WHAT THIS DOES
#
#   The collecting happens in the cluster: a resident Deployment (the chart's `soak.enabled`) has
#   been keeping metrics, events, high-water marks, daily self-checks and the operator's error
#   lines for the length of the soak. This script does the four things that have to happen from
#   OUTSIDE it:
#
#     1. exports the archive       kubectl exec … /manager soak-export  >  one .tar.gz
#     2. reads its manifest        and turns "this stream is empty" into a loud verdict
#     3. checks it for leaks       every namespace, PVC, location and bucket name on your
#                                  cluster is searched for VERBATIM in the archive. A hit means
#                                  the redaction did not do what it says, and this script says so
#                                  instead of handing you a tidy file.
#     4. tells you what to read    before you send it to anyone.
#
#   It is READ-ONLY on your cluster: `kubectl get`, `kubectl logs`, and one `kubectl exec` of a
#   subcommand that reads a volume and writes to its own stdout.
#
# PRIVACY
#
#   The archive is redacted inside the cluster, by the operator's own redactor, under the
#   collector's salt — so a namespace is the same token in a metric, in an event, in a log line
#   and in day 9's self-check. Each report states, in its own redaction block, WHICH salt that
#   was and therefore who can reverse it:
#
#     saltSource: namespace-uid     (the collector's default) derived from the operator
#                                   namespace's UID. Opaque to a stranger; REVERSIBLE BY
#                                   DICTIONARY to anyone who can `get` that namespace, since they
#                                   can recompute the salt. See ../README.md.
#     saltSource: caller-supplied   a Secret you created. Reversible only by whoever holds it.
#     saltSource: random-per-report reversible by nobody, correlates with nothing.
#
#   Read that field before you send the archive anywhere; it is the whole answer to "is this safe
#   to attach".
#
#   What cannot be tokenised, and what the manifest will say in its own words: free text nobody
#   enumerated — a path inside a volume, a restic snapshot ID, a URL inside a library's error
#   string. That is why step 4 exists.
#
#   DO NOT send the salt file with the archive. With it, every token is reversible by dictionary
#   in seconds.
#
# USAGE
#
#   ./collect.sh [--namespace crystal-backup-system] [--out FILE] [--salt-file soak-salt.bin]
#
#     --namespace NS     operator namespace (default crystal-backup-system)
#     --out FILE         archive path (default crystalbackup-soak-<date>.tar.gz)
#     --salt-file PATH   the salt, used LOCALLY and only to check that the tokens in the archive
#                        are the ones your identifiers should have produced. Never uploaded,
#                        never written into the archive. Without it that check is reported as
#                        NOT MADE — which is not the same as passing. Under the derived default
#                        you do not hold a salt file, but you can reproduce one in a command:
#                        see step 1 of ../README.md ("Before you start: the baseline").
#     --review-dir DIR   where the archive is unpacked for inspection (default <out>.review)
#     --since DURATION   passed to soak-export (default: everything)
#     --full             export WITHOUT redaction. Identifiers verbatim. Says so everywhere.
#     --no-color | --help | --version
#
#   Needs kubectl, tar, gzip and jq. openssl (or python3) for the token check.
#
# EXIT CODES
#
#   0  COLLECTED        every stream the collector was asked for is present and plausible.
#   1  COLLECTED, THIN  the archive is worth sending, but something covers materially less than
#                       the soak — dropped days, a collector that was down, a mark NOT MEASURED.
#   2  INCOMPLETE       a stream is empty where empty is not a healthy answer, or the leak check
#                       found a raw identifier. The archive is still written, and the finding is
#                       in it.
#   3  NOT COLLECTED    could not run at all. No archive, and nothing concluded.
# ---------------------------------------------------------------------------------------------

# `set -e` is deliberately NOT used: this script runs commands that are allowed to fail — an
# absent Deployment, a pod whose node is gone, an export that comes back short are findings to be
# recorded, not reasons to abort holding half an archive. `set -u` stays on: an unset variable
# here would be a bug in this script, not a fact about the cluster.
set -u

SCRIPT_VERSION='2.0.0'

NS='crystal-backup-system'
OUT_ARCHIVE=''
REVIEW_DIR=''
SALT_FILE=''
SINCE=''
FULL=no
USE_COLOR=auto
TMP=''
MODE=''
MANIFEST=''

KUBECTL=${KUBECTL:-kubectl}

# The chart's default name for the collector Deployment: "<fullname>-soak", and fullname is the
# chart name unless the release set fullnameOverride. Override it here if yours did:
#   SOAK_DEPLOY=<release>-soak ./collect.sh …
DEPLOY=${SOAK_DEPLOY:-crystal-backup-soak}
CONTAINER='collector'

# The minimum length of an identifier the leak check will look for. Below it a name is more
# likely to be an English fragment inside a message than a leaked identity, and the false
# positives would drown the real finding. Anything skipped for this reason is NAMED in the
# output — an identifier that is not checked for is never silently not checked for.
MIN_IDENT_LEN=3

# Known-answer test for the token construction, which this script has to reproduce byte for byte
# to be able to check the archive at all:
#   HMAC-SHA256(salt, kind || 0x00 || value), first 4 bytes, hex, prefixed "<kind>-"
KAT_SALT='crystalbackup-soak-self-test-vec'
KAT_EXPECT='ns-6756b3ef'

C_RESET='' C_OK='' C_WARN='' C_BAD='' C_DIM='' C_BOLD=''
US=$(printf '\037')
TAB=$(printf '\t')
FINDINGS=''
N_FAILED=0
N_EMPTY=0
N_THIN=0

# --- output plumbing --------------------------------------------------------------------------

setup_color() {
	[ "$USE_COLOR" = no ] && return
	if [ "$USE_COLOR" = auto ]; then
		[ -n "${NO_COLOR:-}" ] && return
		[ -t 1 ] || return
	fi
	C_RESET=$(printf '\033[0m')
	C_OK=$(printf '\033[32m')
	C_WARN=$(printf '\033[33m')
	C_BAD=$(printf '\033[31m')
	C_DIM=$(printf '\033[2m')
	C_BOLD=$(printf '\033[1m')
}

say() { printf '%s\n' "$*" >&2; }

die_uncollected() {
	printf '%sNOT COLLECTED%s: %s\n' "$C_BAD" "$C_RESET" "$1" >&2
	printf 'No archive was produced. Nothing was concluded about this soak.\n' >&2
	exit 3
}

usage() {
	if [ -r "$0" ] && head -n 1 "$0" 2>/dev/null | grep -q '^#!'; then
		sed -n '2,64p' "$0" | sed -e 's/^# \{0,1\}//' -e 's/^#$//'
		return
	fi
	printf 'collect.sh [--namespace NS] [--out FILE] [--salt-file PATH]\n'
}

# record WHAT STATUS COUNT DETAIL
#   STATUS is OK | THIN | EMPTY | FAILED | NOT_MADE | DISABLED.
#
#   EMPTY and FAILED are never merged: one says the cluster had nothing, the other says this
#   script could not look. NOT_MADE is the third, and it exists because a check that could not be
#   performed must degrade the verdict rather than flatter it.
record() {
	FINDINGS="${FINDINGS}${1}${US}${2}${US}${3}${US}${4}
"
	case $2 in
	FAILED | EMPTY) if [ "$2" = FAILED ]; then N_FAILED=$((N_FAILED + 1)); else N_EMPTY=$((N_EMPTY + 1)); fi ;;
	THIN | NOT_MADE) N_THIN=$((N_THIN + 1)) ;;
	esac
}

# --- token construction (local only) --------------------------------------------------------------

SALT_HEX=''
TOKEN_BACKEND=''

hmac_hex() {
	if [ "$TOKEN_BACKEND" = openssl ]; then
		{
			printf '%s' "$1"
			printf '\000'
			printf '%s' "$2"
		} |
			openssl dgst -sha256 -mac HMAC -macopt "hexkey:$SALT_HEX" -binary 2>/dev/null |
			od -An -tx1 -N4 | tr -d ' \n'
	else
		SOAK_KIND="$1" SOAK_VALUE="$2" SOAK_SALT_HEX="$SALT_HEX" python3 -c '
import binascii, hashlib, hmac, os, sys
salt = binascii.unhexlify(os.environ["SOAK_SALT_HEX"])
msg  = os.environ["SOAK_KIND"].encode() + b"\x00" + os.environ["SOAK_VALUE"].encode()
sys.stdout.write(hmac.new(salt, msg, hashlib.sha256).hexdigest()[:8])
'
	fi
}

cb_token() {
	_t=$(hmac_hex "$1" "$2")
	printf '%s-%s' "$1" "$_t"
	unset _t
}

# select_token_backend picks openssl or python3 and then PROVES it against the vector above. A
# backend that silently produced a wrong digest — an openssl that ignored -macopt, a printf that
# swallowed the NUL — would make the token check report a perfect archive as unrecognisable, or
# worse, report a broken one as fine.
select_token_backend() {
	_keep="$SALT_HEX"
	SALT_HEX=$(printf '%s' "$KAT_SALT" | od -An -tx1 -v | tr -d ' \n')
	for _b in openssl python3; do
		command -v "$_b" >/dev/null 2>&1 || continue
		TOKEN_BACKEND=$_b
		if [ "$(cb_token ns production)" = "$KAT_EXPECT" ]; then
			SALT_HEX="$_keep"
			unset _keep _b
			return 0
		fi
	done
	SALT_HEX="$_keep"
	TOKEN_BACKEND=''
	unset _keep _b
	return 1
}

# --- the identifiers this cluster uses ------------------------------------------------------------
#
# One value per line. Used twice: to search the archive for a VERBATIM occurrence (a leak), and
# — when the salt is available — to check that the token it should have become is present
# instead (a correlation). The two directions together are what makes "it looks redacted" into
# something checked rather than assumed.

IDENTS=''

note_ident() {
	[ -n "${2:-}" ] || return 0
	case $2 in
	*"$TAB"* | *' '*) return 0 ;;
	esac
	case "
$IDENTS" in
	*"
$1$TAB$2"*) return 0 ;;
	esac
	IDENTS="${IDENTS}${1}${TAB}${2}
"
}

enumerate_identifiers() {
	say "${C_DIM}enumerating this cluster's identifiers…${C_RESET}"
	_out=$("$KUBECTL" get namespaces -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null)
	for _n in $_out; do note_ident ns "$_n"; done

	for _pair in \
		'backupschedules.crystalbackup.io sched' \
		'clusterbackupschedules.crystalbackup.io sched' \
		'backuplocations.crystalbackup.io loc' \
		'clusterbackuplocations.crystalbackup.io loc' \
		'backuprepositories.crystalbackup.io loc' \
		'backupexternalsyncs.crystalbackup.io sync' \
		'clusterbackupexternalsyncs.crystalbackup.io sync'; do
		# shellcheck disable=SC2086
		set -- $_pair
		_out=$("$KUBECTL" get "$1" -A -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null)
		for _n in $_out; do note_ident "$2" "$_n"; done
	done

	_out=$("$KUBECTL" get backuplocations.crystalbackup.io,clusterbackuplocations.crystalbackup.io \
		-A -o json 2>/dev/null |
		jq -r '.items[]? | [.spec.bucket?, .spec.prefix?, .spec.clusterID?]
                        | .[] | select(. != null and . != "")' 2>/dev/null)
	for _n in $_out; do note_ident bucket "$_n"; done

	_out=$("$KUBECTL" get pvc -A -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null | sort -u)
	for _n in $_out; do note_ident pvc "$_n"; done

	printf '%s' "$IDENTS" >"$TMP/idents.tsv"
	unset _out _n _pair
}

# --- export ---------------------------------------------------------------------------------------

detect_mode() {
	if "$KUBECTL" -n "$NS" get deploy "$DEPLOY" >/dev/null 2>&1; then
		MODE=collector
		return
	fi
	if [ -n "$("$KUBECTL" -n "$NS" get pods -l crystalbackup.io/soak=selfcheck \
		-o jsonpath='{.items[*].metadata.name}' 2>/dev/null)" ]; then
		MODE=fallback
		return
	fi
	die_uncollected "found neither the soak collector (deploy/$DEPLOY) nor any fallback CronJob pod in $NS. Nothing has been collecting. See hack/soak/README.md before concluding anything about this cluster."
}

export_from_collector() {
	_pod=$("$KUBECTL" -n "$NS" get pods -l crystalbackup.io/soak=collector \
		--field-selector=status.phase=Running \
		-o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
	[ -n "$_pod" ] || die_uncollected \
		"deploy/$DEPLOY exists but has no Running pod. Look at it before exporting: kubectl -n $NS describe deploy/$DEPLOY"

	# The collector's own view first. It is one screen, it goes into the review directory beside
	# the archive, and it is the cheapest way to learn on day 1 that nothing is being collected.
	"$KUBECTL" -n "$NS" exec "$_pod" -c "$CONTAINER" -- \
		/manager soak-export --status >"$TMP/status.txt" 2>&1
	_st=$?
	if [ $_st -ne 0 ]; then
		die_uncollected "the collector rejected 'soak-export --status' (exit $_st): $(head -c 400 "$TMP/status.txt" | tr '\n' ' '). If this build has no soak-export, you are on the fallback CronJob path — see manifests/fallback-selfcheck-cronjob.yaml."
	fi

	set -- soak-export
	[ -n "$SINCE" ] && set -- "$@" "--since=$SINCE"
	[ "$FULL" = yes ] && set -- "$@" --full

	say "${C_DIM}exporting from $_pod…${C_RESET}"
	# No -t: a TTY would translate the byte stream and corrupt the gzip. Nothing but the tarball
	# goes to stdout, by contract; the subcommand's own progress is on stderr.
	if ! "$KUBECTL" -n "$NS" exec "$_pod" -c "$CONTAINER" -- /manager "$@" \
		>"$OUT_ARCHIVE" 2>"$TMP/export.err"; then
		die_uncollected "soak-export failed: $(tr '\n' ' ' <"$TMP/export.err" | head -c 400)"
	fi
	[ -s "$OUT_ARCHIVE" ] || die_uncollected \
		'soak-export produced an empty file. Nothing was exported; do not send this.'
	gzip -t "$OUT_ARCHIVE" 2>/dev/null || die_uncollected \
		"the exported file is not a valid gzip stream — something else wrote to stdout inside the container. First bytes: $(head -c 120 "$OUT_ARCHIVE" | tr -d '\000-\010\013\014\016-\037')"
	unset _pod _st
}

# The degraded path: no resident collector, only the fallback CronJob's daily self-checks. It is
# structurally missing metrics, high-water marks and the event history, so it can never be better
# than INCOMPLETE — which is exactly what it should read as.
export_from_fallback() {
	say "${C_WARN}no resident collector: falling back to the CronJob's daily self-checks.${C_RESET}"
	_dir="$TMP/fallback"
	mkdir -p "$_dir/selfcheck" "$_dir/logs" "$_dir/events"

	"$KUBECTL" -n "$NS" get pods -l crystalbackup.io/soak=selfcheck \
		--sort-by=.metadata.creationTimestamp \
		-o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.metadata.creationTimestamp}{"\n"}{end}' \
		>"$TMP/soakpods.tsv" 2>/dev/null

	_n=0
	_bad=0
	while IFS="$TAB" read -r _pod _created; do
		[ -n "${_pod:-}" ] || continue
		_raw=$("$KUBECTL" -n "$NS" logs "$_pod" --tail=-1 2>&1)
		# A stray line on stderr — controller-runtime's "log.SetLogger was never called" — is
		# merged into `kubectl logs` output and would make the document unparsable. The report is
		# MarshalIndent'd, so it starts with a bare `{` line and ends with a bare `}` line: take
		# that slice rather than the whole stream.
		printf '%s\n' "$_raw" | sed -n '/^{$/,/^}$/p' >"$TMP/one.json"
		if [ -s "$TMP/one.json" ] && jq -e '.reportVersion' "$TMP/one.json" >/dev/null 2>&1; then
			cp "$TMP/one.json" "$_dir/selfcheck/$(printf '%s' "$_created" | tr ':' '-').json"
			_n=$((_n + 1))
		else
			_bad=$((_bad + 1))
		fi
	done <"$TMP/soakpods.tsv"

	# Best effort on the other two streams, with their horizons stated rather than implied.
	for _p in $("$KUBECTL" -n "$NS" get pods -l control-plane=controller-manager \
		-o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null); do
		"$KUBECTL" -n "$NS" logs "$_p" --timestamps --tail=-1 2>/dev/null |
			awk '/"level":"(error|dpanic|panic|fatal)"/ || /[[:space:]](ERROR|DPANIC|PANIC|FATAL)[[:space:]]/' \
				>>"$_dir/logs/operator-errors.log"
	done
	"$KUBECTL" get events -A -o json 2>/dev/null |
		jq '{items: [.items[] | select(.type == "Warning")]}' >"$_dir/events/warnings.json" 2>/dev/null

	# The manifest this mode can honestly produce. Every stream the resident collector would have
	# held is named and marked NOT_MEASURED, so the archive cannot be mistaken for a full one.
	jq -n --arg n "$_n" --arg bad "$_bad" --arg ns "$NS" '{
      schema: "crystalbackup.soak/v1",
      mode: "fallback-cronjob",
      exportedAt: (now | todate),
      redaction: { mode: "hashed", saltDisclosed: false,
        note: "self-check reports were redacted by the operator. The operator error lines and the events in this archive were NOT redacted by anything — this fallback path has no redactor. Read them before sending." },
      unredactedNote: "logs/ and events/ are VERBATIM in fallback mode.",
      streams: [
        { name: "selfcheck", status: (if ($n|tonumber) > 0 then "OK" else "EMPTY" end),
          items: ($n|tonumber), emptyIsHealthy: false,
          note: "\($bad) pod(s) produced output that was not a report." },
        { name: "metrics",   status: "NOT_MEASURED", items: 0,
          note: "no resident collector: nothing scraped /metrics." },
        { name: "highwater", status: "NOT_MEASURED", items: 0,
          note: "peak mover memory and cache high-water need a resident sampler." },
        { name: "events",    status: "DEGRADED", items: 0, emptyIsHealthy: true,
          note: "collected once, at export. Kubernetes drops events after about an hour, so this is the last hour and nothing before it." },
        { name: "logs",      status: "DEGRADED", items: 0, emptyIsHealthy: true,
          note: "read from the kubelet at export time, so bounded by log rotation — not the soak." }
      ]
    }' >"$_dir/MANIFEST.json"

	tar -czf "$OUT_ARCHIVE" -C "$_dir" . 2>/dev/null ||
		die_uncollected 'could not write the fallback archive.'
	unset _dir _n _bad _pod _created _raw _p
}

# --- reading the manifest ---------------------------------------------------------------------------

unpack_and_read_manifest() {
	mkdir -p "$REVIEW_DIR" || die_uncollected "cannot create the review directory: $REVIEW_DIR"
	tar -xzf "$OUT_ARCHIVE" -C "$REVIEW_DIR" 2>"$TMP/untar.err" ||
		die_uncollected "the archive will not unpack: $(tr '\n' ' ' <"$TMP/untar.err" | head -c 300)"

	_m=$(find "$REVIEW_DIR" -name MANIFEST.json -maxdepth 3 | head -n 1)
	if [ -z "$_m" ] || ! jq -e '.streams' "$_m" >/dev/null 2>&1; then
		record manifest FAILED 0 \
			'the archive carries no readable MANIFEST.json, so nothing in it can be judged complete or incomplete. Send it anyway and say this happened.'
		unset _m
		return
	fi
	MANIFEST="$_m"

	if [ "$(jq -r '.redaction.mode // "unknown"' "$MANIFEST")" = full ]; then
		record redaction FAILED 0 \
			'this archive was exported with --full: every identifier in it is VERBATIM. That is a decision, not a defect — but it must be a deliberate one, so it is reported as a finding.'
	fi

	# One finding per stream, straight from the manifest, so this script agrees with the archive
	# by construction rather than by a second opinion.
	# US (0x1f) as the separator, not a tab, and this is not cosmetic: tab is IFS *whitespace*, so
	# `IFS=<tab> read` collapses runs of tabs into one delimiter and an empty field silently
	# shifts every field after it. A stream with no coverage block has two empty fields in a row —
	# so with a tab separator the note ends up read as the drop count, and a stream this script is
	# supposed to be loud about is described by the wrong sentence. US is not IFS whitespace.
	jq -r --arg us "$US" '.streams[] |
      [ .name,
        (.status // "unknown"),
        ((.items // 0) | tostring),
        ((.emptyIsHealthy // false) | tostring),
        ((.coverage.observedDays // "") | tostring),
        ((.coverage.requestedDays // "") | tostring),
        ((.drops // []) | length | tostring),
        (.note // "")
      ] | join($us)' "$MANIFEST" >"$TMP/streams.tsv" 2>/dev/null

	while IFS="$US" read -r _name _status _items _emptyok _obs _req _drops _note; do
		[ -n "${_name:-}" ] || continue
		case $_status in
		OK)
			if [ -n "$_obs" ] && [ -n "$_req" ] &&
				awk -v o="$_obs" -v r="$_req" 'BEGIN { exit !(r > 0 && o / r < 0.9) }' 2>/dev/null; then
				record "$_name" THIN "$_items" \
					"covers $_obs of $_req day(s). ${_drops} segment(s) dropped by the footprint cap. $_note"
			elif [ "${_drops:-0}" != 0 ]; then
				record "$_name" THIN "$_items" "$_drops segment(s) dropped by the footprint cap. $_note"
			else
				record "$_name" OK "$_items" "$_note"
			fi
			;;
		EMPTY)
			if [ "$_emptyok" = true ]; then
				record "$_name" OK 0 "empty, and empty is a possible healthy answer here. $_note"
			else
				record "$_name" EMPTY 0 \
					"nothing was collected, and for this stream that is not a possible healthy answer. $_note"
			fi
			;;
		NOT_MEASURED) record "$_name" THIN "$_items" "NOT MEASURED — $_note" ;;
		DEGRADED) record "$_name" THIN "$_items" "degraded — $_note" ;;
		DISABLED) record "$_name" DISABLED "$_items" "$_note" ;;
		*) record "$_name" FAILED "$_items" "unrecognised status '$_status'. $_note" ;;
		esac
	done <"$TMP/streams.tsv"

	_frac=$(jq -r '.collectorUptimeFraction // empty' "$MANIFEST" 2>/dev/null)
	if [ -n "$_frac" ] && awk -v f="$_frac" 'BEGIN { exit !(f < 0.9) }' 2>/dev/null; then
		record collector THIN 0 \
			"the collector was up for only $_frac of the soak. Whatever happened in the gaps is not in this archive, and an unbroken-looking series across one is an illusion."
	fi
	unset _m _name _status _items _emptyok _obs _req _drops _note _frac
}

# --- the leak check ---------------------------------------------------------------------------------
#
# The one check that is not a matter of trusting the manifest. Every identifier this cluster uses
# is searched for VERBATIM in the unpacked archive. In hashed mode there should be none, so a hit
# is not a nuance — it is a customer name about to be emailed to a maintainer.
#
# And the other direction, when the salt is available: the token each identifier SHOULD have
# become is searched for too. Present means the archive is redacted with the salt you think it
# is; absent for all of them means the two sides used the salt differently and the streams do not
# correlate, which is worth knowing before the analysis rather than during it.

leak_check() {
	_checked=0
	_skipped=''
	_hits=''
	while IFS="$TAB" read -r _kind _val; do
		[ -n "${_val:-}" ] || continue
		if [ "${#_val}" -lt "$MIN_IDENT_LEN" ]; then
			_skipped="${_skipped}${_val} "
			continue
		fi
		_checked=$((_checked + 1))
		# Whole-identifier match, and dots escaped: a name is RFC 1123, so `.` is the only
		# character in it that means something to an ERE.
		_re=$(printf '%s' "$_val" | sed -e 's/\./\\./g')
		if grep -rqE "(^|[^-A-Za-z0-9.])${_re}([^-A-Za-z0-9.]|$)" "$REVIEW_DIR" 2>/dev/null; then
			_hits="${_hits}${_kind}:${_val} "
		fi
	done <"$TMP/idents.tsv"

	if [ -n "$_skipped" ]; then
		record leak-check-short NOT_MADE 0 \
			"not searched for, being shorter than $MIN_IDENT_LEN characters (they would match ordinary words): $_skipped"
	fi

	if [ "$FULL" = yes ]; then
		record leak-check DISABLED "$_checked" \
			'--full was requested: identifiers are verbatim on purpose, so there is nothing to leak-check.'
		unset _checked _skipped _hits _kind _val _re
		return
	fi
	if [ -n "$_hits" ]; then
		record leak-check FAILED 0 \
			"these identifiers appear VERBATIM in the archive: $_hits — the redaction did not cover them. Do not send this archive until you have looked at where they appear (grep -r in $REVIEW_DIR)."
	else
		record leak-check OK "$_checked" \
			"$_checked identifier(s) searched for verbatim; none found. The archive carries no name this cluster uses."
	fi
	unset _checked _skipped _hits _kind _val _re
}

token_check() {
	if [ -z "$SALT_FILE" ]; then
		record token-check NOT_MADE 0 \
			'no --salt-file, so it was not verified that the tokens in the archive are the ones your identifiers produce. The check was NOT made, which is not the same as passing.'
		return
	fi
	_hit=0
	_miss=0
	_n=0
	while IFS="$TAB" read -r _kind _val; do
		[ -n "${_val:-}" ] || continue
		[ "$_n" -ge 30 ] && break
		_n=$((_n + 1))
		if grep -rq -- "$(cb_token "$_kind" "$_val")" "$REVIEW_DIR" 2>/dev/null; then
			_hit=$((_hit + 1))
		else
			_miss=$((_miss + 1))
		fi
	done <"$TMP/idents.tsv"

	if [ "$_hit" -eq 0 ]; then
		record token-check FAILED 0 \
			"none of the $_miss tokens computed from your salt appears in the archive. Either the export used a different salt, or it constructs tokens differently from the way this script does — in both cases the pseudonyms in this archive cannot be matched to anything you hold. Say so when you send it."
	else
		record token-check OK "$_hit" \
			"$_hit of $((_hit + _miss)) sampled identifiers appear as the token your salt produces; the archive is redacted under the salt you hold."
	fi
	unset _hit _miss _n _kind _val
}

# --- verdict ------------------------------------------------------------------------------------

verdict_of() {
	if [ "$N_FAILED" -gt 0 ] || [ "$N_EMPTY" -gt 0 ]; then
		printf 'INCOMPLETE'
	elif [ "$N_THIN" -gt 0 ]; then
		printf 'COLLECTED, THIN'
	else
		printf 'COLLECTED'
	fi
}

exit_code_of() {
	if [ "$N_FAILED" -gt 0 ] || [ "$N_EMPTY" -gt 0 ]; then
		printf 2
	elif [ "$N_THIN" -gt 0 ]; then
		printf 1
	else
		printf 0
	fi
}

print_report() {
	printf '\n%s%s%s   (%s mode)\n\n' "$C_BOLD" "$(verdict_of)" "$C_RESET" "$MODE" >&2
	while IFS="$US" read -r _w _st _c _d; do
		[ -n "${_w:-}" ] || continue
		case $_st in
		OK) _col=$C_OK ;;
		THIN | NOT_MADE) _col=$C_WARN ;;
		DISABLED) _col=$C_DIM ;;
		*) _col=$C_BAD ;;
		esac
		printf '  %s%-9s%s %-16s %s\n' "$_col" "$_st" "$C_RESET" "$_w" "$_c" >&2
		if [ "$_st" != OK ] && [ "$_st" != DISABLED ] && [ -n "$_d" ]; then
			printf '%s\n' "$_d" | fold -s -w 80 | sed -e 's/^/                     /' >&2
		fi
	done <<EOF
$FINDINGS
EOF
	unset _w _st _c _d _col
}

# --- main ----------------------------------------------------------------------------------------

while [ $# -gt 0 ]; do
	case $1 in
	--namespace | -n)
		[ $# -ge 2 ] || die_uncollected '--namespace needs a value'
		NS=$2
		shift 2
		;;
	--out)
		[ $# -ge 2 ] || die_uncollected '--out needs a path'
		OUT_ARCHIVE=$2
		shift 2
		;;
	--salt-file)
		[ $# -ge 2 ] || die_uncollected '--salt-file needs a path'
		SALT_FILE=$2
		shift 2
		;;
	--review-dir)
		[ $# -ge 2 ] || die_uncollected '--review-dir needs a path'
		REVIEW_DIR=$2
		shift 2
		;;
	--since)
		[ $# -ge 2 ] || die_uncollected '--since needs a duration'
		SINCE=$2
		shift 2
		;;
	--full)
		FULL=yes
		shift
		;;
	--no-color)
		USE_COLOR=no
		shift
		;;
	-h | --help)
		usage
		exit 3
		;;
	--version)
		printf 'crystalbackup-soak-collect %s\n' "$SCRIPT_VERSION"
		exit 3
		;;
	*) die_uncollected "unknown option: $1 (try --help)" ;;
	esac
done

setup_color
[ -n "$OUT_ARCHIVE" ] || OUT_ARCHIVE="crystalbackup-soak-$(date -u '+%Y%m%d').tar.gz"
[ -n "$REVIEW_DIR" ] || REVIEW_DIR="${OUT_ARCHIVE}.review"

for _t in jq tar gzip awk find od; do
	command -v "$_t" >/dev/null 2>&1 ||
		die_uncollected "$_t is not on PATH. This needs kubectl, jq, tar, gzip, awk and od."
done
unset _t
command -v "$KUBECTL" >/dev/null 2>&1 ||
	die_uncollected 'kubectl not found on PATH (set KUBECTL= to override).'
"$KUBECTL" get --raw /version >/dev/null 2>&1 ||
	die_uncollected "cannot reach the cluster API with $KUBECTL. Check KUBECONFIG and your context."

TMP=$(mktemp -d "${TMPDIR:-/tmp}/cb-soak.XXXXXX") || die_uncollected 'cannot create a temporary directory.'
trap 'rm -rf "$TMP"' EXIT HUP INT TERM

if [ -n "$SALT_FILE" ]; then
	[ -r "$SALT_FILE" ] || die_uncollected "cannot read the salt file: $SALT_FILE"
	_len=$(wc -c <"$SALT_FILE" | tr -d ' ')
	# The same floor the operator's flag enforces. A short salt is a guessable one, and a
	# guessable salt makes every token in the archive reversible with a dictionary.
	[ "$_len" -ge 32 ] || die_uncollected \
		"the salt file is $_len bytes; 32 is the minimum (openssl rand -out soak-salt.bin 32)."
	SALT_HEX=$(od -An -tx1 -v <"$SALT_FILE" | tr -d ' \n')
	select_token_backend || die_uncollected \
		'neither openssl nor python3 reproduced the known-answer vector for the token construction, so the token check cannot be trusted. Re-run without --salt-file to skip it (it will be reported NOT MADE), or fix the tooling.'
	unset _len
fi

if [ "$FULL" = yes ]; then
	printf '%s!! --full: the archive will carry namespace names, bucket names, endpoints and log\n' "$C_BAD" >&2
	printf '!! messages VERBATIM. Only do this if you have decided to share them.%s\n\n' "$C_RESET" >&2
fi

detect_mode
enumerate_identifiers

case $MODE in
collector) export_from_collector ;;
fallback)
	export_from_fallback
	record mode THIN 0 \
		'this is the fallback CronJob path: no metrics, no high-water marks and no event history were ever collected. It is a series of daily self-checks and nothing else.'
	;;
esac

unpack_and_read_manifest
leak_check
token_check

[ -f "$TMP/status.txt" ] && cp "$TMP/status.txt" "$REVIEW_DIR/collector-status.txt" 2>/dev/null

print_report

printf '\n  archive     %s%s%s (%s bytes)\n' "$C_BOLD" "$OUT_ARCHIVE" "$C_RESET" \
	"$(wc -c <"$OUT_ARCHIVE" | tr -d ' ')" >&2
printf '  unpacked    %s\n' "$REVIEW_DIR" >&2
printf '\n  %sBefore you send it%s\n' "$C_BOLD" "$C_RESET" >&2
printf '    1. read %s/MANIFEST.json — it says what could not be tokenised\n' "$REVIEW_DIR" >&2
printf '    2. read the free text: %s/logs/ and %s/events/\n' "$REVIEW_DIR" "$REVIEW_DIR" >&2
printf '    3. do NOT send the salt file with the archive\n\n' >&2

# The unpacked copy is deliberately left behind. Deleting it would mean the instruction above is
# to read something this script has just removed, and an admin who is asked to review an archive
# needs it open, not re-extracted.

exit "$(exit_code_of)"
