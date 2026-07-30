#!/usr/bin/env python3
"""Verify that every series and label the shipped Grafana dashboards reference actually exists.

Why this script exists
----------------------
A Grafana panel whose query names a series nobody emits renders "No data". On a BACKUP
dashboard "No data" is visually indistinguishable from "nothing to report, all is well" —
which is the exact failure the 0.5.x cycle shipped: five of nine alert rules were valid
PromQL against series the operator never emitted. Valid, evaluable, and unable to fire.

`internal/metrics/names.go` is the catalogue and the contract. This script makes the
dashboards meet it, so a renamed series breaks the build instead of quietly emptying a
panel in production.

What it checks
--------------
1. Every `crystalbackup_*` token anywhere in the dashboard JSON resolves to a catalogue
   constant (histogram families may carry the `_bucket` / `_sum` / `_count` suffixes
   Prometheus derives).
2. Every label used in a selector `{...}`, in a `by (...)` / `without (...)` grouping, or
   in a `{{legend}}` placeholder belongs to the label set of the families in that query.
   A query grouping on a label the family does not carry is as broken as one naming a
   series nobody emits, and fails the same way: silently.
3. The JSON is valid and structurally a dashboard (`panels`, `templating`, `schemaVersion`,
   `uid`, `title`; unique panel ids; every panel has a `gridPos`).
4. No dashboard is close to the ~1 MiB ConfigMap ceiling.

It also REPORTS (without failing) catalogue series no dashboard shows. That list is a
coverage gap, not an error — some series exist only for alert rules.

The label sets are parsed out of the collector sources rather than restated here: a second
copy of the contract is how the first drift started.

Usage: python3 hack/check-dashboard-metrics.py [--verbose]
"""

from __future__ import annotations

import glob
import json
import os
import re
import sys

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
METRICS_DIR = os.path.join(REPO, "internal", "metrics")
DASHBOARD_DIR = os.path.join(REPO, "charts", "crystal-backup", "dashboards")

# A ConfigMap may not exceed 1 MiB including keys and metadata. Warn well before.
CONFIGMAP_LIMIT = 1024 * 1024
CONFIGMAP_WARN = int(CONFIGMAP_LIMIT * 0.75)

# Label VALUES a dashboard hardcodes are the blind spot the name and label checks leave wide
# open, and it is not hypothetical: this very dashboard set shipped `scope="namespace"` for an
# afternoon. Valid metric, valid label, and a value the operator never emits — the enum is
# `Cluster;Namespaced` — so six panels of a tenant's repository row would have read "No data"
# forever while looking exactly like a tenant who simply has no repository.
#
# Each entry binds a (family pattern, label) pair to WHERE its value set is declared. The values
# are never restated here, only their address, so a renamed constant fails loudly instead of
# rubber-stamping stale expectations.
#
# ("kubebuilder", file, anchor): the +kubebuilder:validation:Enum marker above `anchor`.
# ("goconst", file, pattern, transform): Go constants whose declaration matches `pattern`.
VALUE_SOURCES = [
    # repository/discovery do NOT emit BackupRepositoryStatus.Scope verbatim. metricScope()
    # translates the API enum (Cluster|Namespaced) into the label vocabulary below before
    # emission, so the API enum is the wrong address to validate against — it would pass
    # exactly the values the collector no longer produces. The constants metricScope() returns
    # are the authority.
    (r"^crystalbackup_(repository|discovery)_", "scope",
     ("goconst", "internal/metrics/repository.go",
      r"^\s*scope(Cluster|Namespace)\s+=", None)),
    # external sync reaches the same vocabulary by a different road (apiconst.Origin*). Bound
    # separately rather than assumed identical: if the two ever diverge again, this catches it.
    (r"^crystalbackup_externalsync_", "scope",
     ("goconst", "internal/apiconst/apiconst.go", r"^\s*Origin(Cluster|Namespace)\s*=", None)),
    (r"^crystalbackup_", "origin",
     ("goconst", "internal/apiconst/apiconst.go", r"^\s*Origin(Cluster|Namespace)\s*=", None)),
    (r"^crystalbackup_restore_", "mode",
     ("goconst", "api/v1alpha1/common_types.go", r"^\s*RestoreMode\w+\s+RestoreMode\s*=", None)),
    # resultOf() lowercases the terminal phase, so the metric carries the phase name lowercased.
    (r"^crystalbackup_backup_total$", "result",
     ("goconst", "internal/status/phases.go", r"^\s*BackupPhase\w+\s+BackupPhase\s*=", "lower")),
    (r"^crystalbackup_clusterbackup_runs_total$", "result",
     ("goconst", "internal/status/phases.go",
      r"^\s*ClusterBackupPhase\w+\s+ClusterBackupPhase\s*=", "lower")),
]

# Labels Prometheus or the scrape config attaches, which no collector declares.
SYNTHETIC_LABELS = {
    "le", "job", "instance", "pod", "container", "endpoint", "service", "node", "__name__",
}

# Suffixes Prometheus derives from a histogram family.
HISTOGRAM_SUFFIXES = ("_bucket", "_sum", "_count")

# Go tokens that may appear in a label-set expression and carry no label of their own.
GO_NOISE = {"string", "append", "nil", "var", "const"}

# Grafana core panel plugins. A typo here ("timeserie") imports cleanly and renders an empty
# box saying the panel type is not found — another way to look like "nothing to report".
# Deliberately core-only: these dashboards must render on a stock Grafana with no plugins.
KNOWN_PANEL_TYPES = {
    "row", "text", "stat", "gauge", "timeseries", "table", "barchart", "bargauge",
    "piechart", "heatmap", "histogram", "status-history", "state-timeline", "logs",
    "news", "nodeGraph", "trend", "xychart", "dashlist", "alertlist", "annolist",
    "candlestick", "canvas", "datagrid", "flamegraph", "geomap", "traces",
}


class Fail(Exception):
    pass


# --------------------------------------------------------------------------------------
# Parsing the contract out of internal/metrics
# --------------------------------------------------------------------------------------

def _go_sources() -> dict[str, str]:
    out = {}
    for path in sorted(glob.glob(os.path.join(METRICS_DIR, "*.go"))):
        if path.endswith("_test.go"):
            continue
        with open(path, encoding="utf-8") as fh:
            out[path] = fh.read()
    if not out:
        raise Fail("no Go sources under %s — is the checkout complete?" % METRICS_DIR)
    return out


def _balanced(text: str, open_at: int) -> tuple[str, int]:
    """Return the content of the parenthesised group starting at `open_at`, and its end."""
    depth = 0
    i = open_at
    in_str = False
    while i < len(text):
        ch = text[i]
        if in_str:
            if ch == "\\":
                i += 2
                continue
            if ch == '"':
                in_str = False
        elif ch == '"':
            in_str = True
        elif ch == "(":
            depth += 1
        elif ch == ")":
            depth -= 1
            if depth == 0:
                return text[open_at + 1:i], i
        i += 1
    raise Fail("unbalanced parentheses starting at offset %d" % open_at)


def parse_catalogue() -> tuple[dict[str, str], dict[str, set[str]], set[str]]:
    """Return (series -> const ident, series -> label set, histogram series)."""
    sources = _go_sources()
    blob = "\n".join(sources.values())

    # const ident -> "crystalbackup_..." (names.go is the only place these live)
    name_consts: dict[str, str] = {}
    for ident, value in re.findall(r'^\s*(\w+)\s*=\s*"(crystalbackup_[a-z0-9_]+)"\s*$',
                                   blob, re.MULTILINE):
        name_consts[ident] = value
    if not name_consts:
        raise Fail("parsed zero series constants out of internal/metrics — the parser is stale")

    # const ident -> label name. `var`/`const` prefixes are optional: these are declared both
    # inside grouped blocks and as standalone one-liners across the package.
    label_consts: dict[str, str] = {}
    for ident, value in re.findall(
            r'^\s*(?:var\s+|const\s+)?(\w*[Ll]abel)\s*=\s*"([a-zA-Z_][a-zA-Z0-9_]*)"\s*$',
            blob, re.MULTILINE):
        label_consts[ident] = value

    # var ident -> []string{...} of label names
    label_slices: dict[str, list[str]] = {}
    for ident, body in re.findall(r'^\s*(?:var\s+)?(\w+)\s*=\s*\[\]string\{([^}]*)\}',
                                  blob, re.MULTILINE):
        resolved = _resolve_label_expr("[]string{%s}" % body, label_consts, {})
        if resolved is None:
            raise Fail("cannot resolve the label slice %s = []string{%s} — update this script"
                       % (ident, body.strip()))
        label_slices[ident] = sorted(resolved)

    series_labels: dict[str, set[str]] = {}
    histograms: set[str] = set()
    unresolved: list[str] = []

    def record(const_ident: str, expr: str, is_histogram: bool, where: str):
        series = name_consts.get(const_ident)
        if series is None:
            # A Desc built from something other than a names.go constant is exactly the
            # drift this file was written to prevent.
            unresolved.append("%s: metric name %r is not a constant from names.go" % (where, const_ident))
            return
        labels = _resolve_label_expr(expr, label_consts, label_slices)
        if labels is None:
            unresolved.append("%s: cannot resolve the label set of %s from %r" % (where, series, expr))
            return
        series_labels[series] = labels
        if is_histogram:
            histograms.add(series)

    for path, text in sources.items():
        base = os.path.basename(path)

        # prometheus.NewDesc(NameX, "help", <labels>, nil)
        for m in re.finditer(r"prometheus\.NewDesc\(", text):
            args, _ = _balanced(text, m.end() - 1)
            parts = _split_top_level(args)
            if len(parts) < 3:
                unresolved.append("%s: NewDesc with %d args" % (base, len(parts)))
                continue
            record(parts[0].strip(), parts[2], False, base)

        # prometheus.New{Counter,Gauge,Histogram,Summary}Vec(Opts{...}, <labels>)
        for m in re.finditer(r"prometheus\.New(Counter|Gauge|Histogram|Summary)Vec\(", text):
            args, _ = _balanced(text, m.end() - 1)
            parts = _split_top_level(args)
            if len(parts) < 2:
                unresolved.append("%s: New%sVec with %d args" % (base, m.group(1), len(parts)))
                continue
            name_match = re.search(r"\bName:\s*(\w+)", parts[0])
            if not name_match:
                unresolved.append("%s: New%sVec without a Name: field" % (base, m.group(1)))
                continue
            record(name_match.group(1), parts[1], m.group(1) == "Histogram", base)

    if unresolved:
        # Loud, never silent: an unresolvable family means this parser no longer understands
        # the collector sources, and a check that quietly stops checking is worse than none.
        raise Fail("the metric catalogue could not be fully parsed — update this script:\n  - "
                   + "\n  - ".join(unresolved))

    # A constant declared in names.go with no collector behind it yet is legitimate while a
    # milestone is in flight: the catalogue is the contract, the implementation follows it.
    # Dashboards may reference such a series. Say so, do not fail — but record the label set
    # as unknown so no label claim is silently rubber-stamped against it.
    missing = sorted(set(name_consts.values()) - set(series_labels))
    if missing:
        print("note: %d catalogue constant(s) have no collector yet (declared, not emitted): %s"
              % (len(missing), ", ".join(missing)), file=sys.stderr)
        for name in missing:
            series_labels[name] = None

    return name_consts, series_labels, histograms


def _split_top_level(args: str) -> list[str]:
    parts, depth, cur, in_str = [], 0, [], False
    i = 0
    while i < len(args):
        ch = args[i]
        if in_str:
            cur.append(ch)
            if ch == "\\":
                cur.append(args[i + 1])
                i += 2
                continue
            if ch == '"':
                in_str = False
        elif ch == '"':
            in_str = True
            cur.append(ch)
        elif ch in "([{":
            depth += 1
            cur.append(ch)
        elif ch in ")]}":
            depth -= 1
            cur.append(ch)
        elif ch == "," and depth == 0:
            parts.append("".join(cur))
            cur = []
        else:
            cur.append(ch)
        i += 1
    parts.append("".join(cur))
    return [p for p in parts if p.strip()]


def _resolve_label_expr(expr, label_consts, label_slices):
    """Resolve a Go label-set expression to a set of label names.

    Handles the three forms the collectors use: a slice variable, a `[]string{...}` literal,
    and `append(append([]string{}, xLabels...), yLabel)`. Order is irrelevant here.
    """
    expr = expr.strip()
    if expr in ("nil", ""):
        return set()
    found: set[str] = set()
    # Quoted literals inside []string{...}
    for lit in re.findall(r'"([a-zA-Z_][a-zA-Z0-9_]*)"', expr):
        found.add(lit)
    # Strict: every remaining bare identifier must resolve. A partially resolved label set
    # would under-report, which is the same silent hole this script exists to close.
    for ident in re.findall(r"\b([A-Za-z_]\w*)\b", re.sub(r'"[^"]*"', "", expr)):
        if ident in GO_NOISE:
            continue
        if ident in label_slices:
            found.update(label_slices[ident])
        elif ident in label_consts:
            found.add(label_consts[ident])
        else:
            return None
    return found


def _read(rel: str) -> str:
    path = os.path.join(REPO, rel)
    if not os.path.exists(path):
        raise Fail("%s no longer exists — a label-value source moved, update VALUE_SOURCES" % rel)
    with open(path, encoding="utf-8") as fh:
        return fh.read()


def resolve_values(source) -> set[str]:
    """Resolve one VALUE_SOURCES entry to the set of values that label may carry."""
    kind = source[0]
    if kind == "kubebuilder":
        _, rel, anchor = source
        lines = _read(rel).splitlines()
        for i, line in enumerate(lines):
            if not re.search(anchor, line):
                continue
            for j in range(i - 1, max(i - 9, -1), -1):
                m = re.search(r"\+kubebuilder:validation:Enum=(\S+)", lines[j])
                if m:
                    return {v for v in m.group(1).split(";") if v}
            raise Fail("no +kubebuilder:validation:Enum marker above %r in %s" % (anchor, rel))
        raise Fail("anchor %r not found in %s — update VALUE_SOURCES" % (anchor, rel))

    if kind == "goconst":
        _, rel, pattern, transform = source
        values = set()
        for line in _read(rel).splitlines():
            if re.search(pattern, line):
                m = re.search(r'=\s*"([^"]+)"', line)
                if m:
                    values.add(m.group(1).lower() if transform == "lower" else m.group(1))
        if not values:
            raise Fail("pattern %r matched no constants in %s — update VALUE_SOURCES" % (pattern, rel))
        return values

    raise Fail("unknown value source kind %r" % kind)


def allowed_values(family: str, label: str):
    """The value set for (family, label), or None when the label carries no enum."""
    for pattern, lbl, source in VALUE_SOURCES:
        if lbl == label and re.search(pattern, family):
            return resolve_values(source)
    return None


# --------------------------------------------------------------------------------------
# Reading the dashboards
# --------------------------------------------------------------------------------------

METRIC_TOKEN = re.compile(r"\bcrystalbackup_[a-z0-9_]+\b")
SELECTOR_LABEL = re.compile(r"([a-zA-Z_][a-zA-Z0-9_]*)\s*(?:=~|!~|=|!=)")
MATCHER = re.compile(r'([a-zA-Z_][a-zA-Z0-9_]*)\s*(=~|!~|=|!=)\s*"([^"]*)"')
# A plain alternation of literals — anything with real regex syntax is left alone rather than
# guessed at, because a false accusation here would train people to ignore this script.
PLAIN_ALTERNATION = re.compile(r"^[A-Za-z0-9_]+(\|[A-Za-z0-9_]+)*$")


def _matcher_values(op: str, value: str) -> list[str]:
    """The literal values a matcher pins down, or [] when it cannot be decided statically."""
    if "$" in value:  # a Grafana variable; its value is not knowable here
        return []
    if op in ("=", "!="):
        return [value]
    if PLAIN_ALTERNATION.match(value):
        return value.split("|")
    return []
GROUPING = re.compile(r"\b(?:by|without)\s*\(([^)]*)\)")
LEGEND_REF = re.compile(r"\{\{\s*([a-zA-Z_][a-zA-Z0-9_]*)\s*\}\}")


def walk(node, path="$"):
    """Yield (json path, string value) for every string in the document."""
    if isinstance(node, dict):
        for k, v in node.items():
            yield from walk(v, "%s.%s" % (path, k))
    elif isinstance(node, list):
        for i, v in enumerate(node):
            yield from walk(v, "%s[%d]" % (path, i))
    elif isinstance(node, str):
        yield path, node


def resolve_series(token, series_labels, histograms):
    """Map a referenced token to its catalogue family, or None."""
    if token in series_labels:
        return token
    for suffix in HISTOGRAM_SUFFIXES:
        if token.endswith(suffix):
            base = token[: -len(suffix)]
            if base in histograms:
                return base
            if base in series_labels:
                # A non-histogram family has no derived _bucket/_sum/_count series.
                return None
    return None


def collect_queries(doc):
    """Yield (description, promql, [legend formats]) for everything that queries Prometheus."""
    for panel in doc.get("panels", []):
        for target in panel.get("targets", []) or []:
            expr = target.get("expr")
            if not expr:
                continue
            legends = [target["legendFormat"]] if target.get("legendFormat") else []
            yield ("panel %r target %s" % (panel.get("title", "?"), target.get("refId", "?")),
                   expr, legends)
    for var in doc.get("templating", {}).get("list", []) or []:
        q = var.get("query")
        expr = q.get("query") if isinstance(q, dict) else q
        if isinstance(expr, str) and "crystalbackup_" in expr:
            yield ("variable $%s" % var.get("name", "?"), expr, [])


def check_dashboard(path, series_labels, histograms, referenced, errors, verbose):
    name = os.path.basename(path)
    raw = open(path, encoding="utf-8").read()

    try:
        doc = json.loads(raw)
    except json.JSONDecodeError as exc:
        errors.append("%s: not valid JSON: %s" % (name, exc))
        return

    # --- structure -------------------------------------------------------------------
    for key in ("panels", "templating", "schemaVersion", "uid", "title"):
        if key not in doc:
            errors.append("%s: missing top-level %r — Grafana will not import this" % (name, key))
    if not isinstance(doc.get("panels"), list) or not doc.get("panels"):
        errors.append("%s: `panels` is empty or not a list" % name)
    if not isinstance(doc.get("templating", {}).get("list"), list):
        errors.append("%s: `templating.list` is missing or not a list" % name)
    if not isinstance(doc.get("schemaVersion"), int):
        errors.append("%s: `schemaVersion` must be an integer" % name)

    seen_ids = {}
    for i, panel in enumerate(doc.get("panels", []) or []):
        if not isinstance(panel, dict):
            errors.append("%s: panels[%d] is not an object" % (name, i))
            continue
        if "type" not in panel:
            errors.append("%s: panels[%d] has no `type`" % (name, i))
        elif panel["type"] not in KNOWN_PANEL_TYPES:
            errors.append("%s: panel %r has type %r, which is not a Grafana core panel — "
                          "it will render as 'panel plugin not found'"
                          % (name, panel.get("title", i), panel["type"]))
        if "gridPos" not in panel:
            errors.append("%s: panel %r has no `gridPos`" % (name, panel.get("title", i)))
        pid = panel.get("id")
        if pid is None:
            errors.append("%s: panel %r has no `id`" % (name, panel.get("title", i)))
        elif pid in seen_ids:
            errors.append("%s: duplicate panel id %s (%r and %r)"
                          % (name, pid, seen_ids[pid], panel.get("title")))
        else:
            seen_ids[pid] = panel.get("title")

    # --- layout ----------------------------------------------------------------------
    # A dashboard with overflowing or overlapping panels imports without complaint and then
    # looks wrong, which is its own kind of untrustworthy.
    occupied: dict[tuple[int, int], str] = {}
    for panel in doc.get("panels", []) or []:
        gp = panel.get("gridPos") or {}
        x, w, y0, h = gp.get("x", 0), gp.get("w", 0), gp.get("y", 0), gp.get("h", 0)
        title = panel.get("title", "?")
        if w <= 0 or h <= 0:
            errors.append("%s: panel %r has a zero-sized gridPos" % (name, title))
            continue
        if x + w > 24:
            errors.append("%s: panel %r overflows the 24-column grid (x=%d w=%d)"
                          % (name, title, x, w))
        clashes = set()
        for cy in range(y0, y0 + h):
            for cx in range(x, min(x + w, 24)):
                other = occupied.get((cx, cy))
                if other is not None:
                    clashes.add(other)
                occupied[(cx, cy)] = title
        for other in sorted(clashes):
            errors.append("%s: panel %r overlaps %r on the grid" % (name, title, other))

    # --- ConfigMap budget ------------------------------------------------------------
    size = len(raw.encode("utf-8"))
    if size >= CONFIGMAP_LIMIT:
        errors.append("%s: %d bytes exceeds the 1 MiB ConfigMap limit" % (name, size))
    elif size >= CONFIGMAP_WARN:
        print("warning: %s is %d bytes, %.0f%% of the 1 MiB ConfigMap limit"
              % (name, size, 100.0 * size / CONFIGMAP_LIMIT), file=sys.stderr)

    # --- every crystalbackup_ token anywhere in the file -----------------------------
    for jpath, value in walk(doc):
        for token in METRIC_TOKEN.findall(value):
            family = resolve_series(token, series_labels, histograms)
            if family is None:
                errors.append(
                    "%s: %s references %r, which is not in internal/metrics/names.go.\n"
                    "        A panel on an unknown series renders 'No data', which on a backup "
                    "dashboard reads as 'all clear'.\n"
                    "        Fix the query, or add the constant to names.go if the series is real."
                    % (name, jpath, token))
            else:
                referenced.add(family)

    # --- labels ----------------------------------------------------------------------
    for where, expr, legends in collect_queries(doc):
        families = set()
        unknown_family = False
        for token in METRIC_TOKEN.findall(expr):
            fam = resolve_series(token, series_labels, histograms)
            if fam is None:
                unknown_family = True
            elif series_labels[fam] is None:
                unknown_family = True  # declared but not yet emitted; label set unknown
            else:
                families.add(fam)
        if unknown_family or not families:
            # Either already reported above, or an inherited controller-runtime / workqueue
            # series (spec §2.10) whose labels this catalogue does not own.
            continue

        allowed = set(SYNTHETIC_LABELS)
        for fam in families:
            allowed |= series_labels[fam]

        used = set()
        for m in re.finditer(r"(crystalbackup_[a-z0-9_]+)\s*\{([^}]*)\}", expr):
            fam = resolve_series(m.group(1), series_labels, histograms)
            for label, op, value in MATCHER.findall(m.group(2)):
                used.add(label)
                if fam is None:
                    continue
                permitted = allowed_values(fam, label)
                if permitted is None:
                    continue
                for candidate in _matcher_values(op, value):
                    if candidate not in permitted:
                        errors.append(
                            "%s: %s selects %s=%r, but %s only ever carries %s.\n"
                            "        The query is valid PromQL and matches nothing — the panel "
                            "renders 'No data', which on a backup dashboard reads as 'all clear'."
                            % (name, where, label, candidate, fam,
                               " | ".join(sorted(permitted))))
        for m in GROUPING.finditer(expr):
            used |= {p.strip() for p in m.group(1).split(",") if p.strip()}
        for legend in legends:
            used |= set(LEGEND_REF.findall(legend))

        for label in sorted(used - allowed):
            errors.append(
                "%s: %s uses label %r, which %s does not carry (labels: %s).\n"
                "        A grouping or selector on a non-existent label silently yields no series."
                % (name, where, label, "/".join(sorted(families)),
                   ", ".join(sorted(allowed - SYNTHETIC_LABELS))))

    if verbose:
        print("%s: %d panels, %d bytes, ok" % (name, len(doc.get("panels", [])), size))


def main() -> int:
    verbose = "--verbose" in sys.argv or "-v" in sys.argv

    try:
        name_consts, series_labels, histograms = parse_catalogue()
    except Fail as exc:
        print("FAIL: %s" % exc, file=sys.stderr)
        return 1

    dashboards = sorted(glob.glob(os.path.join(DASHBOARD_DIR, "*.json")))
    if not dashboards:
        print("FAIL: no dashboards found under %s" % DASHBOARD_DIR, file=sys.stderr)
        return 1

    referenced: set[str] = set()
    errors: list[str] = []
    for path in dashboards:
        check_dashboard(path, series_labels, histograms, referenced, errors, verbose)

    catalogue = set(name_consts.values())
    unshown = sorted(catalogue - referenced)

    print("checked %d dashboard(s) against %d catalogue series in internal/metrics/names.go"
          % (len(dashboards), len(catalogue)))
    if unshown:
        print("\n%d catalogue series appear on no dashboard (coverage note, not an error):"
              % len(unshown))
        for series in unshown:
            print("  - %s" % series)

    if errors:
        print("\nFAIL: %d problem(s):\n" % len(errors), file=sys.stderr)
        for err in errors:
            print("  - %s" % err, file=sys.stderr)
        return 1

    print("\nOK: every referenced series and label exists.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
