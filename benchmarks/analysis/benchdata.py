#!/usr/bin/env python3
"""Reading the canonical benchmark documents.

`plot.py` draws; this module reads. It knows the shapes `cmd/easysftp-bench`
writes (`results.json`, `matrix.json`, their CSV exports and `trend.csv`) and
nothing about matplotlib, so a new plot does not have to relearn the schema.

Two rules hold everywhere in here, both learned from the stored files:

- Every field is optional. The results span several schema versions: version 1
  (up to v3.3.2) has no `link`, no `phases`, no `operations`; the sweeps from
  before issue #184 have no `link_profile`, no `request_concurrency` and no
  `auto` rows. Asking for a key that is not there is how a plot silently drops
  older results, or worse, shows the wrong axis.
- Nothing here writes into `benchmarks/` and nothing here runs a benchmark.
  The relationship is one way (`benchmarks/analysis/README.md`).
"""

from __future__ import annotations

import csv
import json
from pathlib import Path

REPO = Path(__file__).resolve().parents[2]
BENCHMARKS = REPO / "benchmarks"
DEFAULT_OUT = BENCHMARKS / "analysis" / "out"

# The phases cmd/easysftp-bench records, in the order benchmarks/README.md
# lists them. Fixed so one phase keeps one color across every figure of a
# report, which is the only reason two stacked bars can be compared by eye.
PHASE_ORDER = [
    "connect",
    "local_scan",
    "remote_scan",
    "hash",
    "create_dirs",
    "sweep_stale_temps",
    "upload",
    "delete_sweep",
    "manifest_read",
    "manifest_write",
    "prune_dirs",
    "cleanup",
]

# Same idea for the per round-trip operations. `file_upload` is deliberately
# first: it is the umbrella around the sftp_* calls of one file, so its total
# contains theirs and the two must never be added together.
OPERATION_ORDER = [
    "file_upload",
    "sftp_open",
    "sftp_write",
    "sftp_chmod",
    "sftp_setstat",
    "sftp_rename",
    "sftp_remove",
    "sftp_rmdir",
    "sftp_mkdirall",
    "sftp_stat",
    "sftp_readdir",
    "sftp_realpath",
    "ssh_connect",
]

# An operation that wraps other operations. Its total is not additional time.
UMBRELLA_OPERATIONS = {"file_upload"}

# The three settings a matrix cell is a point in.
AXIS_KEYS = ("concurrency", "connections", "request_concurrency")


# --------------------------------------------------------------------------
# Small shared helpers
# --------------------------------------------------------------------------


def rows(path):
    """A CSV as dicts, with empty strings left alone: '' is 'not recorded'."""
    with open(path, newline="", encoding="utf-8") as handle:
        return list(csv.DictReader(handle))


def number(row, column):
    """A numeric column or field, or None when it is absent or empty."""
    value = row.get(column)
    if value is None:
        return None
    if isinstance(value, (int, float)):
        return float(value)
    text = str(value).strip()
    if not text:
        return None
    try:
        return float(text)
    except ValueError:
        return None


def slug(text):
    keep = [c if c.isalnum() or c in "-_." else "-" for c in str(text)]
    return "".join(keep).strip("-") or "none"


def human_ms(value):
    if value is None:
        return "?"
    if value < 1000:
        return f"{value:.0f} ms"
    if value < 60_000:
        return f"{value / 1000:.1f} s"
    return f"{value / 60_000:.1f} min"


def axis_label(key, value):
    """A cell coordinate as it should be read: null request_concurrency is
    'default', which is not the same as any measured number."""
    if value is None or value == "":
        return "default" if key == "request_concurrency" else "?"
    if isinstance(value, float) and value.is_integer():
        return str(int(value))
    return str(value)


def axis_layout(points, keys=AXIS_KEYS):
    """Which of the three settings to put where, from what actually varies.

    A sweep does not use the same axes for every scenario: `scenario.AxisFor`
    caps both axes at the file count and `scenario.SweepsRequests` only lets
    request_concurrency through for large files, so `single` varies only
    request_concurrency while `small` varies only the other two. Hard-coding
    concurrency onto x draws a one-point line for half the scenarios.

    Returns (x_key, series_key, facet_keys): the widest axis across the bottom,
    the next widest as one line each, and any remaining axis that still varies
    as a separate figure. Ties keep the order in `keys`.
    """
    ranked = sorted(
        (
            (len({point.get(key) for point in points}), -index, key)
            for index, key in enumerate(keys)
        ),
        reverse=True,
    )
    order = [key for _, _, key in ranked]
    facets = [
        key for key in order[2:] if len({point.get(key) for point in points}) > 1
    ]
    return order[0], order[1], facets


# --------------------------------------------------------------------------
# The stored documents
# --------------------------------------------------------------------------


class Document:
    """One stored result: the envelope plus the measurement under `.benchmark`.

    The accessors below are the whole schema surface the plots are allowed to
    know about. Add to them rather than reaching into `.benchmark` from a plot,
    so the version tolerance stays in one place.
    """

    def __init__(self, path, envelope):
        self.path = Path(path)
        self.envelope = envelope if isinstance(envelope, dict) else {}
        benchmark = self.envelope.get("benchmark")
        # A bare results.json/matrix.json (a run artifact that was never
        # stored) has no envelope around it. Both are worth plotting.
        if not isinstance(benchmark, dict):
            benchmark = self.envelope
            self.envelope = {}
        self.benchmark = benchmark

    # -- provenance ---------------------------------------------------------

    @property
    def kind(self):
        kind = self.envelope.get("kind") or self.benchmark.get("benchmark_kind")
        if kind:
            return kind
        return "matrix" if self.benchmark.get("cells") else "standard"

    @property
    def is_matrix(self):
        return bool(self.benchmark.get("cells")) or self.kind == "matrix"

    @property
    def ref(self):
        return self.benchmark.get("candidate_ref") or "?"

    @property
    def runner(self):
        return self.benchmark.get("runner") or "?"

    @property
    def recorded_at(self):
        return self.envelope.get("recorded_at") or ""

    @property
    def scenarios(self):
        return self.benchmark.get("scenarios") or {}

    # -- measurements -------------------------------------------------------

    @property
    def results(self):
        """The upload aggregates: results[] for a standard run, cells[] for a
        sweep. One row per configuration in both cases."""
        return self.benchmark.get("results") or self.benchmark.get("cells") or []

    @property
    def deletes(self):
        return self.benchmark.get("deletes") or []

    @property
    def scaling(self):
        return self.benchmark.get("scaling") or []

    @property
    def auto(self):
        return self.benchmark.get("auto") or []

    @property
    def canary(self):
        return self.benchmark.get("canary") or []

    @property
    def link(self):
        return self.benchmark.get("link") or {}

    @property
    def probes(self):
        return self.link.get("probes") or []

    @property
    def shaping(self):
        return self.link.get("shaping") or {}

    # -- derived ------------------------------------------------------------

    def profile(self, row):
        return row.get("link_profile") or "baseline"

    def throughput(self, row):
        """MiB/s and files/s, computed when the row does not carry them.

        Sweep cells written before the CSV export did carry both; the delete
        rows never do. Recomputing from bytes and the median keeps one formula
        in one place instead of three that drift.
        """
        mib = number(row, "mib_per_s")
        files_per_s = number(row, "files_per_s")
        median = number(row, "median_ms")
        if median and median > 0:
            if mib is None and number(row, "bytes") is not None:
                mib = number(row, "bytes") / (1024 * 1024) / (median / 1000)
            if files_per_s is None and number(row, "files") is not None:
                files_per_s = number(row, "files") / (median / 1000)
        return mib, files_per_s

    def controls(self, profile):
        """What the link probe measured for one profile, averaged over the
        probes taken for it (normally one at the start and one at the end).

        This is the control: measured with x/crypto/ssh and pkg/sftp, not with
        easySFTP's uploader. Comparing a run against it separates the line from
        easySFTP, which is the only reason it exists.
        """
        taken = [p for p in self.probes if (p.get("profile") or "baseline") == profile]
        if not taken:
            return None

        def mean(values):
            values = [v for v in values if v is not None]
            return sum(values) / len(values) if values else None

        return {
            "probes": len(taken),
            "rtt_p50_ms": mean([(p.get("rtt_ms") or {}).get("p50") for p in taken]),
            "handshake_ms": mean([p.get("handshake_ms") for p in taken]),
            "single_mib_per_s": mean(
                [(p.get("control") or {}).get("single_stream_mib_per_s") for p in taken]
            ),
            "n_mib_per_s": mean(
                [(p.get("control") or {}).get("n_stream_mib_per_s") for p in taken]
            ),
            "streams": next(
                (
                    (p.get("control") or {}).get("streams")
                    for p in taken
                    if (p.get("control") or {}).get("streams")
                ),
                None,
            ),
        }

    def best_cell(self, scenario, label, profile):
        """The fastest measured cell of one scenario, build and profile.

        Read out of `scaling[].best` where the harness recorded it, so a plot
        and `matrix.md` never disagree about which cell won.
        """
        for entry in self.scaling:
            if (
                entry.get("scenario") == scenario
                and entry.get("label") == label
                and (entry.get("link_profile") or "baseline") == profile
            ):
                best = entry.get("best") or {}
                coordinates = {key: best.get(key) for key in AXIS_KEYS}
                for cell in self.results:
                    if (
                        cell.get("scenario") == scenario
                        and cell.get("label") == label
                        and self.profile(cell) == profile
                        and all(cell.get(k) == v for k, v in coordinates.items())
                    ):
                        return cell, entry
                return None, entry
        return None, None

    def caveats(self):
        """What a reader has to know before comparing anything in this file.

        Every figure carries these under it. They are not decoration: a sweep
        measured with shaping unavailable has profile names that say what was
        asked for, not what happened, and a chart cannot tell that by itself.
        """
        notes = []

        shaping = self.shaping
        # "baseline" is the real line and is never shaped, so a run that asked
        # for nothing else is not missing anything and gets no caveat.
        requested = [
            profile for profile in (shaping.get("requested") or []) if profile != "baseline"
        ]
        applied = [
            profile for profile in (shaping.get("applied") or []) if profile != "baseline"
        ]
        if requested and not shaping.get("available", True):
            reason = shaping.get("reason") or "reason not recorded"
            notes.append(
                "link shaping was not available ("
                + reason
                + "): every profile was measured on the real line, so the "
                "profile names say what was asked for, not what happened"
            )
        elif requested and applied and set(requested) != set(applied):
            notes.append(
                "shaping applied for "
                + " ".join(applied)
                + " only, out of the requested "
                + " ".join(requested)
            )

        failed = sum(
            int(row.get("failed_runs") or 0) for row in self.results
        ) + sum(int(row.get("failed_sweeps") or 0) for row in self.deletes)
        if failed:
            notes.append(f"{failed} repeat(s) failed and are not in these aggregates")

        refused = sum(
            int(row.get("connections_refused") or row.get("refused_connections") or 0)
            for row in self.results
        )
        if refused:
            notes.append(
                f"{refused} connection(s) were refused and fell back to the first one"
            )

        errors = sum(int(row.get("errors") or 0) for row in self.results)
        if errors:
            notes.append(f"{errors} operation error(s) were recorded")

        return notes

    def provenance(self):
        parts = [self.kind, self.ref, self.runner]
        if self.recorded_at:
            parts.append(self.recorded_at)
        return ", ".join(str(p) for p in parts if p and p != "?")


def load(path):
    with open(path, encoding="utf-8") as handle:
        return Document(path, json.load(handle))


# --------------------------------------------------------------------------
# Finding the stored files
# --------------------------------------------------------------------------


def newest_matrix_json():
    found = sorted((BENCHMARKS / "matrix").glob("matrix-*.json"))
    if not found:
        raise SystemExit(
            "no sweep found under benchmarks/matrix; pass a matrix.json explicitly"
        )
    return found[-1]


def newest_matrix_csv():
    found = sorted((BENCHMARKS / "matrix").glob("matrix-*.csv"))
    if not found:
        raise SystemExit(
            "no sweep found under benchmarks/matrix; pass a matrix.csv explicitly"
        )
    return found[-1]


def newest_release_json():
    """`latest.json` when it is there: it is the copy of the newest official
    release result and the one link that must never move."""
    latest = BENCHMARKS / "latest.json"
    if latest.exists():
        return latest
    found = sorted((BENCHMARKS / "releases").glob("release-*.json"))
    if not found:
        raise SystemExit(
            "no release result found under benchmarks/releases; pass one explicitly"
        )
    return found[-1]


def documents(paths, default):
    """Load the given paths, or the newest stored file of the right kind."""
    chosen = [Path(p) for p in paths] or [default()]
    return [load(path) for path in chosen]
