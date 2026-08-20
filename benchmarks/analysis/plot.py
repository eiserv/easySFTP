#!/usr/bin/env python3
"""Plots of stored benchmark results.

Reads the canonical files the Go harness writes and nothing else: a sweep's
`matrix.json` / `matrix.csv`, a run's `results.json` (`latest.json` is the
newest official one) and the top-level `trend.csv`. It never runs a benchmark,
never writes into `benchmarks/` outside the output directory, and is not needed
by anything that does (issue #190).

Fields are read by name and every one of them is optional, because the stored
files span several schema versions: the sweeps from before issue #184 have no
link profile, no `request_concurrency` and no `auto` rows, and version 1
results have no phases and no operations at all. Asking for something that is
not there is how a plot silently starts showing the wrong axis.

    python benchmarks/analysis/plot.py <command> [file ...] [options]

    heatmap     throughput over the connections x concurrency grid   matrix.csv
    scaling     the same grid as curves, with the linear reference   matrix.json
    auto        what easySFTP picks for itself against the best cell matrix.json
    canary      whether the line held still for the whole sweep      matrix.json
    phases      where the wall clock of a deployment goes            either
    operations  per round-trip cost and share of the work            either
    deletes     the delete sweeps, the only measurement of deletion  either
    link        measured throughput against the link's own control   either
    trend       throughput per scenario across releases              trend.csv
    report      every plot the given files support, plus report.md

Run `plot.py <command> --help` for the options of one command.
"""

import argparse
import sys
import textwrap
from collections import defaultdict
from datetime import datetime
from pathlib import Path

import matplotlib

matplotlib.use("Agg")  # no display on a runner, and none needed
import matplotlib.pyplot as plt  # noqa: E402

sys.path.insert(0, str(Path(__file__).resolve().parent))

import benchdata as bench  # noqa: E402
from benchdata import (  # noqa: E402
    AXIS_KEYS,
    BENCHMARKS,
    DEFAULT_OUT,
    OPERATION_ORDER,
    PHASE_ORDER,
    UMBRELLA_OPERATIONS,
    axis_label,
    axis_layout,
    human_ms,
    number,
    slug,
)

COLORS = matplotlib.colormaps["tab20"]
UNKNOWN = "#9e9e9e"

# Higher is better for every metric but the duration.
METRICS = {
    "mib_per_s": ("MiB/s", True),
    "files_per_s": ("files/s", True),
    "median_ms": ("median ms", False),
}


# --------------------------------------------------------------------------
# Drawing helpers shared by every command
# --------------------------------------------------------------------------


def color_for(name, order):
    if name in order:
        return COLORS(order.index(name) % 20)
    return UNKNOWN


def sort_key(value):
    """Sorts axis values with a null (an unset request_concurrency) first: it
    is not a number and it is not zero, it is 'whatever easySFTP picked'."""
    if value is None or value == "":
        return (0, 0.0, "")
    if isinstance(value, (int, float)):
        return (1, float(value), "")
    return (2, 0.0, str(value))


def footer(figure, doc, extra=(), warnings=()):
    """Provenance and caveats under every figure.

    Not decoration. Two results measured on different runners or over
    different links are not comparable and a chart cannot tell them apart, so
    it has to say what it is a chart of.
    """
    lines = [(str(text), "#404040") for text in extra if text]
    lines += [(str(text), "crimson") for text in warnings if text]
    if doc is not None:
        lines += [("caveat: " + note, "crimson") for note in doc.caveats()]
        lines.append((doc.provenance(), "#707070"))

    width = int(max(60, figure.get_figwidth() * 15))
    wrapped = []
    for text, color in lines:
        for piece in textwrap.wrap(text, width) or [""]:
            wrapped.append((piece, color))
    if not wrapped:
        figure.tight_layout()
        return

    # In figure fractions, so it has to come from the figure's height: a fixed
    # fraction writes the lines on top of each other on a short figure.
    step = 10.0 / (72.0 * figure.get_figheight())
    figure.tight_layout(rect=(0, min(step * len(wrapped) + step, 0.45), 1, 1))
    for index, (piece, color) in enumerate(reversed(wrapped)):
        figure.text(
            0.01, step * (0.4 + index), piece, fontsize=7, color=color, va="bottom"
        )


def save(figure, args, name):
    args.out.mkdir(parents=True, exist_ok=True)
    target = args.out / f"{name}.{args.format}"
    figure.savefig(target, dpi=args.dpi)
    plt.close(figure)
    return target


def selected(args, scenario=None, profile=None):
    """The scenario and profile filters, both optional and both exact."""
    if args.scenario and scenario is not None and scenario not in args.scenario:
        return False
    if args.profile and profile is not None and profile not in args.profile:
        return False
    return True


def document_stem(doc):
    """What a file made from this document is called.

    The stored name, except for `latest.json`: it is a copy of a release
    result, and a plot called "latest" stops being true the day after.
    """
    if doc.path.stem == "latest" and doc.envelope.get("version"):
        return f"release-{doc.envelope['version']}"
    return doc.path.stem


def sibling_document(path):
    """The JSON next to a CSV, for the provenance and the caveats a flat
    export cannot carry. Absent is normal, not an error."""
    candidate = Path(path).with_suffix(".json")
    if candidate.exists():
        try:
            return bench.load(candidate)
        except (ValueError, OSError):
            return None
    return None


# --------------------------------------------------------------------------
# heatmap: the grid as a grid
# --------------------------------------------------------------------------


def heatmap(args):
    """One heatmap per scenario, build, link profile and request_concurrency.

    Those four are what a cell is not: mixing any of them into one grid would
    put two different configurations in the same square.
    """
    written = []
    for path in args.paths or [bench.newest_matrix_csv()]:
        doc = sibling_document(path)
        grids = defaultdict(dict)
        for row in bench.rows(path):
            connections = number(row, "connections")
            concurrency = number(row, "concurrency")
            median = number(row, "median_ms")
            scenario = row.get("scenario", "?")
            profile = row.get("link_profile") or "baseline"
            if connections is None or concurrency is None or median is None:
                continue
            if not selected(args, scenario, profile):
                continue
            key = (
                scenario,
                row.get("build", "?"),
                profile,
                row.get("request_concurrency") or "default",
            )
            grids[key][(int(connections), int(concurrency))] = {
                "median_ms": median,
                "mib_per_s": number(row, "mib_per_s"),
                "files_per_s": number(row, "files_per_s"),
            }

        for key, cells in sorted(grids.items()):
            if len(cells) < 2:
                # A single cell is a number, not a grid. matrix.json has it.
                continue
            written.append(_draw_heatmap(path, doc, key, cells, args))
    return written


def _draw_heatmap(source, doc, key, cells, args):
    scenario, build, profile, requests = key
    metric, (unit, higher_is_better) = args.metric, METRICS[args.metric]
    connections = sorted({c for c, _ in cells})
    concurrency = sorted({c for _, c in cells})

    values = [
        [(cells.get((row, column)) or {}).get(metric) for column in concurrency]
        for row in connections
    ]
    measured = [
        (coordinate, cell[metric])
        for coordinate, cell in cells.items()
        if cell.get(metric) is not None
    ]
    best = (
        (max if higher_is_better else min)(measured, key=lambda item: item[1])[0]
        if measured
        else None
    )

    figure, axes = plt.subplots(
        figsize=(2.0 + 1.2 * len(concurrency), 2.2 + 0.9 * len(connections))
    )
    image = axes.imshow(
        [[v if v is not None else float("nan") for v in line] for line in values],
        cmap="viridis" if higher_is_better else "viridis_r",
        aspect="auto",
    )
    figure.colorbar(image, ax=axes, label=unit)

    axes.set_xticks(range(len(concurrency)), [str(c) for c in concurrency])
    axes.set_yticks(range(len(connections)), [str(c) for c in connections])
    axes.set_xlabel("concurrency")
    axes.set_ylabel("connections")
    title = f"{scenario} / {build} / {profile}"
    if requests != "default":
        title += f" / request_concurrency {requests}"
    axes.set_title(f"{title}\n{Path(source).name}", fontsize=9)

    # The duration in the cell, because the color answers "how fast" and the
    # number answers "how long", and a sweep is read for both.
    for y, row in enumerate(connections):
        for x, column in enumerate(concurrency):
            cell = cells.get((row, column))
            if cell is None:
                continue
            axes.text(
                x,
                y,
                f"{cell['median_ms']:.0f} ms",
                ha="center",
                va="center",
                fontsize=8,
                color="white",
            )

    notes = []
    if best is not None:
        y, x = connections.index(best[0]), concurrency.index(best[1])
        axes.add_patch(
            plt.Rectangle(
                (x - 0.5, y - 0.5), 1, 1, fill=False, edgecolor="crimson", linewidth=2.5
            )
        )
        edges = [
            name
            for name, value, axis in (
                ("connections", best[0], connections),
                ("concurrency", best[1], concurrency),
            )
            if value == max(axis) and len(axis) > 1
        ]
        note = (
            f"best cell (red): connections {best[0]}, concurrency {best[1]}, "
            f"{cells[best][metric]:.2f} {unit}"
        )
        if edges:
            # The reason the grid exists: a best cell on the largest swept
            # value is a cut-off, not an optimum. matrix.json says the same in
            # scaling[].best_at_axis_max, and that field is the authority.
            note += "; it sits on the largest swept " + " and ".join(edges)
            note += ", so the optimum was bounded from below, not measured"
        notes.append(note)

    footer(figure, doc, notes)
    return save(
        figure,
        args,
        "heatmap-"
        + "-".join(
            slug(part)
            for part in (Path(source).stem, scenario, build, profile, requests)
        ),
    )


# --------------------------------------------------------------------------
# scaling: the same grid, read as curves
# --------------------------------------------------------------------------


def scaling(args):
    """Throughput against the axis that was actually swept, per scenario.

    The heatmap answers "which cell won". This answers "where did it stop
    paying", which is the question `advanced.connections` and
    `advanced.concurrency` are set from. The dashed line is what perfect
    scaling from the leftmost point would have looked like; the gap to it is
    the answer.
    """
    written = []
    for doc in bench.documents(args.paths, bench.newest_matrix_json):
        if not doc.scaling:
            continue
        for entry in doc.scaling:
            scenario = entry.get("scenario", "?")
            profile = entry.get("link_profile") or "baseline"
            if not selected(args, scenario, profile):
                continue
            points = entry.get("points") or []
            if len(points) < 2:
                continue
            x_key, series_key, facet_keys = axis_layout(points)
            facet_key = facet_keys[0] if facet_keys else None
            facets = (
                sorted({p.get(facet_key) for p in points}, key=sort_key)
                if facet_key
                else [None]
            )
            for facet in facets:
                chosen = [
                    p for p in points if facet_key is None or p.get(facet_key) == facet
                ]
                if len(chosen) < 2:
                    continue
                written.append(
                    _draw_scaling(
                        doc, entry, chosen, x_key, series_key, facet_key, facet, args
                    )
                )
    return written


def _draw_scaling(doc, entry, points, x_key, series_key, facet_key, facet, args):
    scenario = entry.get("scenario", "?")
    label = entry.get("label", "?")
    profile = entry.get("link_profile") or "baseline"
    best = entry.get("best") or {}

    x_values = sorted({p.get(x_key) for p in points}, key=sort_key)
    positions = {value: index for index, value in enumerate(x_values)}
    series = sorted({p.get(series_key) for p in points}, key=sort_key)

    figure, (top, bottom) = plt.subplots(
        2, 1, figsize=(2.5 + 1.0 * len(x_values), 7.2), sharex=True
    )
    ceilings = {top: 0.0, bottom: 0.0}
    for index, value in enumerate(series):
        line = sorted(
            (p for p in points if p.get(series_key) == value),
            key=lambda p: sort_key(p.get(x_key)),
        )
        color = COLORS(index * 2 % 20)
        name = f"{series_key} {axis_label(series_key, value)}"
        for axes, field in ((top, "mib_per_s"), (bottom, "files_per_s")):
            xs = [positions[p.get(x_key)] for p in line if p.get(field) is not None]
            ys = [p[field] for p in line if p.get(field) is not None]
            if not ys:
                continue
            axes.plot(xs, ys, marker="o", color=color, label=name)
            ceilings[axes] = max(ceilings[axes], max(ys))

        # Perfect scaling from the leftmost measured point of this series.
        # Only drawn where the axis is numeric: "default" has no multiple.
        numeric = [p for p in line if isinstance(p.get(x_key), (int, float))]
        if len(numeric) >= 3:
            first = numeric[0]
            for axes, field in ((top, "mib_per_s"), (bottom, "files_per_s")):
                if first.get(field) in (None, 0):
                    continue
                axes.plot(
                    [positions[p.get(x_key)] for p in numeric],
                    [
                        first[field] * (p[x_key] / first[x_key])
                        for p in numeric
                    ],
                    linestyle=":",
                    linewidth=1,
                    color=color,
                    alpha=0.45,
                )

    if best.get(x_key) in positions:
        for axes, field in ((top, "mib_per_s"), (bottom, "files_per_s")):
            if best.get(field) is not None:
                axes.scatter(
                    [positions[best[x_key]]],
                    [best[field]],
                    marker="*",
                    s=190,
                    color="crimson",
                    zorder=5,
                    label="best cell" if axes is top else None,
                )

    # Scaled to what was measured, so the reference may leave the frame: an
    # axis stretched to fit perfect scaling flattens the curves the plot is for.
    for axes, ceiling in ceilings.items():
        if ceiling > 0:
            axes.set_ylim(0, ceiling * 1.15)

    top.set_ylabel("MiB/s")
    bottom.set_ylabel("files/s")
    bottom.set_xlabel(x_key)
    bottom.set_xticks(
        range(len(x_values)), [axis_label(x_key, value) for value in x_values]
    )
    for axes in (top, bottom):
        axes.grid(alpha=0.3)
    top.legend(fontsize=8)

    title = f"{scenario} / {label} / {profile}"
    if facet_key is not None:
        title += f" / {facet_key} {axis_label(facet_key, facet)}"
    top.set_title(
        f"{title}\n{doc.scenarios.get(scenario, '')}".rstrip(), fontsize=10
    )

    notes = [
        "dotted: perfect scaling from the leftmost point of the same series; the "
        "y axis is scaled to what was measured, so it normally leaves the frame"
    ]
    # `scaling[].best_at_axis_max` is the authority for this, but the sweeps
    # from before issue #184 do not carry it, so it is derived there instead of
    # the plot going quiet about the one thing the grid exists to show.
    edges = entry.get("best_at_axis_max")
    derived = edges is None
    if derived:
        edges = [
            key
            for key in AXIS_KEYS
            if isinstance(best.get(key), (int, float))
            and len({p.get(key) for p in entry.get("points") or []}) > 1
            and best[key] == max(
                p.get(key)
                for p in entry.get("points") or []
                if isinstance(p.get(key), (int, float))
            )
        ]
    if edges:
        notes.append(
            "the best cell sits on the largest swept "
            + " and ".join(edges)
            + ": the optimum was bounded from below, not measured, and nothing "
            "should be fitted to these numbers until the axis is extended"
            + (" (derived here: this sweep predates the stored field)" if derived else "")
        )
    footer(figure, doc, notes)
    return save(
        figure,
        args,
        "scaling-"
        + "-".join(
            slug(part)
            for part in (
                document_stem(doc),
                scenario,
                label,
                profile,
                f"{facet_key}{axis_label(facet_key, facet)}" if facet_key else "all",
            )
        ),
    )


# --------------------------------------------------------------------------
# phases: where the wall clock goes
# --------------------------------------------------------------------------


def _upload_rows(doc, args):
    """One (title, name, row) per (scenario, profile): the result of a standard
    run, the best cell of a sweep. A sweep has hundreds of cells and stacking
    all of them says nothing; the cell that won is the one worth taking apart.
    """
    picked = []
    if doc.is_matrix:
        for entry in doc.scaling:
            scenario = entry.get("scenario", "?")
            profile = entry.get("link_profile") or "baseline"
            if not selected(args, scenario, profile):
                continue
            cell, _ = doc.best_cell(scenario, entry.get("label"), profile)
            if cell is None:
                continue
            short = {
                "connections": "conn",
                "concurrency": "conc",
                "request_concurrency": "req",
            }
            coordinates = [
                f"{short[key]} {axis_label(key, cell.get(key))}" for key in AXIS_KEYS
            ]
            picked.append(
                (
                    f"{scenario} / {profile}\nbest cell: " + ", ".join(coordinates),
                    slug(f"{scenario}-{profile}-" + "-".join(coordinates)),
                    cell,
                )
            )
    else:
        for row in doc.results:
            scenario = row.get("scenario", "?")
            profile = doc.profile(row)
            if not selected(args, scenario, profile):
                continue
            picked.append((f"{scenario} / {profile}", slug(f"{scenario}-{profile}"), row))
    return picked


def phases(args):
    written = []
    for doc in bench.documents(args.paths, bench.newest_release_json):
        rows = [
            (title, row)
            for title, _, row in _upload_rows(doc, args)
            if row.get("phases")
        ]
        if args.include_deletes:
            rows += [
                (f"{row.get('scenario', '?')} / {doc.profile(row)}\ndelete sweep", row)
                for row in doc.deletes
                if row.get("phases")
                and selected(args, row.get("scenario"), doc.profile(row))
            ]
        if not rows:
            continue
        written.append(_draw_phases(doc, rows, args))
    return written


def _draw_phases(doc, rows, args):
    names = [
        name
        for name in PHASE_ORDER
        if any(
            any(phase.get("name") == name for phase in row.get("phases", []))
            for _, row in rows
        )
    ]
    extra = sorted(
        {
            phase.get("name")
            for _, row in rows
            for phase in row.get("phases", [])
            if phase.get("name") not in names
        }
    )
    names += extra

    figure, axes = plt.subplots(figsize=(13, 2.4 + 0.85 * len(rows)))
    offsets = [0.0] * len(rows)
    for name in names:
        widths = [
            next(
                (
                    number(phase, "median_ms") or 0.0
                    for phase in row.get("phases", [])
                    if phase.get("name") == name
                ),
                0.0,
            )
            for _, row in rows
        ]
        if not any(widths):
            continue
        axes.barh(
            range(len(rows)),
            widths,
            left=offsets,
            color=color_for(name, PHASE_ORDER),
            label=name,
            height=0.62,
        )
        offsets = [a + b for a, b in zip(offsets, widths)]

    for index, total in enumerate(offsets):
        axes.text(total, index, f"  {human_ms(total)}", va="center", fontsize=8)

    axes.set_yticks(range(len(rows)), [label for label, _ in rows], fontsize=8)
    axes.invert_yaxis()
    axes.set_xlabel("wall clock, median of the repeats (ms)")
    axes.set_xlim(0, max(offsets) * 1.12 if offsets else 1)
    axes.set_title("Where the wall clock of a deployment goes", fontsize=11)
    axes.legend(fontsize=8, loc="upper left", bbox_to_anchor=(1.01, 1))
    axes.grid(axis="x", alpha=0.3)

    footer(
        figure,
        doc,
        [
            "phases are wall clock and add up to roughly the run's duration; "
            "the per round-trip costs inside them are in the operations plot "
            "and are cumulative across the parallel workers, so they are not "
            "the same kind of number"
        ],
    )
    return save(figure, args, f"phases-{slug(document_stem(doc))}")


# --------------------------------------------------------------------------
# operations: what the round-trips cost
# --------------------------------------------------------------------------


def operations(args):
    written = []
    for doc in bench.documents(args.paths, bench.newest_release_json):
        for title, key, row in _upload_rows(doc, args):
            if row.get("operations"):
                written.append(_draw_operations(doc, title, key, row, args))
    return written


def _draw_operations(doc, title, key, row, args):
    ordered = sorted(
        row.get("operations", []),
        key=lambda op: number(op, "median_total_ms") or number(op, "total_ms") or 0.0,
    )
    names = [op.get("name", "?") for op in ordered]
    totals = [
        number(op, "median_total_ms") or number(op, "total_ms") or 0.0 for op in ordered
    ]
    counts = [int(op.get("count") or 0) for op in ordered]
    ticks = [
        f"{name} (n={count}){' *' if name in UMBRELLA_OPERATIONS else ''}"
        for name, count in zip(names, counts)
    ]

    figure, (left, right) = plt.subplots(
        1, 2, figsize=(13, 1.8 + 0.5 * len(ordered)), sharey=True
    )

    colors = [color_for(name, OPERATION_ORDER) for name in names]
    left.barh(range(len(ordered)), totals, color=colors, height=0.65)
    for index, (total, name) in enumerate(zip(totals, names)):
        share = ""
        billed = sum(
            value
            for value, other in zip(totals, names)
            if other not in UMBRELLA_OPERATIONS
        )
        if billed and name not in UMBRELLA_OPERATIONS:
            share = f" ({total / billed * 100:.0f}%)"
        left.text(total, index, f"  {human_ms(total)}{share}", va="center", fontsize=8)
    left.set_xlabel("cumulative across the parallel workers (ms)")
    left.set_xlim(0, max(totals) * 1.25 if totals else 1)
    left.set_yticks(range(len(ordered)), ticks, fontsize=8)
    left.grid(axis="x", alpha=0.3)
    left.set_title("share of the work", fontsize=10)

    for index, op in enumerate(ordered):
        percentiles = [
            (number(op, "p50_ms"), "o", "p50"),
            (number(op, "p90_ms"), "s", "p90"),
            (number(op, "p99_ms"), "^", "p99"),
            (number(op, "max_ms"), "|", "max"),
        ]
        spread = [value for value, _, _ in percentiles if value is not None]
        if spread:
            right.plot(
                [min(spread), max(spread)],
                [index, index],
                color=colors[index],
                alpha=0.5,
                linewidth=1.5,
            )
        for value, marker, name in percentiles:
            if value is None:
                continue
            right.scatter(
                [value],
                [index],
                marker=marker,
                color=colors[index],
                s=46,
                label=name if index == 0 else None,
            )
    right.set_xscale("log")
    right.set_xlabel("per call (ms, log scale)")
    right.grid(axis="x", alpha=0.3)
    right.set_title("per call cost", fontsize=10)
    right.legend(fontsize=8, loc="lower right")

    figure.suptitle(f"Operations: {title.replace(chr(10), ' | ')}", fontsize=11)
    footer(
        figure,
        doc,
        [
            "* file_upload is the umbrella around the sftp_* calls of one file: "
            "its total contains theirs, so the two are never added together and "
            "the share is taken over the others only",
            "operation totals are cumulative across the parallel upload workers "
            "and are normally larger than the phase they happened in; read them "
            "for the share of the work and the per call cost, never as elapsed time",
        ],
    )
    return save(figure, args, f"operations-{slug(document_stem(doc))}-{key}")


# --------------------------------------------------------------------------
# deletes: the only measurement of deletion there is
# --------------------------------------------------------------------------


def deletes(args):
    """The clean deployment that runs before every measured run.

    It is a pure delete sweep and it is instrumented on purpose (issue #184,
    phase 4): nothing else in the harness measures deletion, and its numbers
    are deliberately kept out of the upload aggregates.
    """
    written = []
    for doc in bench.documents(args.paths, bench.newest_release_json):
        rows = [
            row
            for row in doc.deletes
            if selected(args, row.get("scenario"), doc.profile(row))
        ]
        if not rows:
            continue
        varies = len({tuple(row.get(key) for key in AXIS_KEYS) for row in rows}) > 1
        if varies:
            for scenario in sorted({row.get("scenario", "?") for row in rows}):
                chosen = [row for row in rows if row.get("scenario") == scenario]
                if len(chosen) > 1:
                    written.append(_draw_delete_curve(doc, scenario, chosen, args))
        else:
            written.append(_draw_delete_bars(doc, rows, args))
    return written


def _delete_rate(doc, row):
    rate = number(row, "deletes_per_s")
    if rate is not None:
        return rate
    _, files_per_s = doc.throughput(
        {"files": row.get("files_deleted"), "median_ms": row.get("median_ms")}
    )
    return files_per_s


def _draw_delete_bars(doc, rows, args):
    labels = [f"{row.get('scenario', '?')}\n{doc.profile(row)}" for row in rows]
    rates = [_delete_rate(doc, row) or 0.0 for row in rows]

    figure, axes = plt.subplots(figsize=(2.5 + 1.1 * len(rows), 5.4))
    bars = axes.bar(range(len(rows)), rates, color=COLORS(4))
    for bar, row, rate in zip(bars, rows, rates):
        axes.text(
            bar.get_x() + bar.get_width() / 2,
            rate,
            f"{rate:.1f}/s\n{human_ms(number(row, 'median_ms'))}\n"
            f"{int(row.get('files_deleted') or 0)} files",
            ha="center",
            va="bottom",
            fontsize=8,
        )
    axes.set_xticks(range(len(rows)), labels, fontsize=8)
    axes.set_ylabel("deletes/s")
    axes.set_ylim(0, max(rates) * 1.35 if rates else 1)
    axes.grid(axis="y", alpha=0.3)
    axes.set_title("Delete sweeps (the pre-run clean deployment)", fontsize=11)

    footer(figure, doc, [_DELETE_NOTE])
    return save(figure, args, f"deletes-{slug(document_stem(doc))}")


def _draw_delete_curve(doc, scenario, rows, args):
    """The delete rate against the widest swept axis.

    One bold line per profile through the median of every cell measured at
    that coordinate, with the cells themselves behind it. A line per
    (profile, connections) pair would be a dozen crossing lines saying the
    same thing, and the thing they say is whether the rate moves at all.
    """
    x_key, _, _ = axis_layout(rows)
    x_values = sorted({row.get(x_key) for row in rows}, key=sort_key)
    positions = {value: index for index, value in enumerate(x_values)}

    figure, axes = plt.subplots(figsize=(3.5 + 0.95 * len(x_values), 5.8))
    for index, profile in enumerate(sorted({doc.profile(row) for row in rows})):
        color = COLORS(index * 2 % 20)
        measured = [
            (positions[row.get(x_key)], _delete_rate(doc, row))
            for row in rows
            if doc.profile(row) == profile and _delete_rate(doc, row) is not None
        ]
        if not measured:
            continue
        axes.scatter(
            [x for x, _ in measured],
            [y for _, y in measured],
            color=color,
            s=14,
            alpha=0.35,
        )
        middles = []
        for position in range(len(x_values)):
            at = sorted(y for x, y in measured if x == position)
            if at:
                middles.append((position, at[(len(at) - 1) // 2]))
        axes.plot(
            [x for x, _ in middles],
            [y for _, y in middles],
            marker="o",
            color=color,
            linewidth=2,
            label=profile,
        )

    axes.set_xticks(
        range(len(x_values)), [axis_label(x_key, value) for value in x_values]
    )
    axes.set_xlabel(x_key)
    axes.set_ylabel("deletes/s")
    axes.grid(alpha=0.3)
    axes.legend(fontsize=8, title="link profile")
    axes.set_title(
        f"Delete sweeps: {scenario}\n"
        "line: median of the cells at that coordinate, dots: the cells",
        fontsize=10,
    )

    footer(
        figure,
        doc,
        [
            _DELETE_NOTE,
            "everything outside the per-file upload path runs over one "
            "connection (`session.do`), so a rate that does not move with these "
            "axes is the expected shape, not a measurement error",
        ],
    )
    return save(figure, args, f"deletes-{slug(document_stem(doc))}-{slug(scenario)}")


_DELETE_NOTE = (
    "the clean deployment measured before every run: a pure delete sweep, and "
    "the only measurement of deletion in the harness; its numbers are kept out "
    "of the upload aggregates on purpose"
)


# --------------------------------------------------------------------------
# link: the measurement against the line it was measured over
# --------------------------------------------------------------------------


def link(args):
    """How much of the line a run actually used.

    `cmd/linkprobe` measures the path with x/crypto/ssh and pkg/sftp, importing
    nothing from `internal/uploader`: a control taken through the code under
    test would not be a control. Read against it, a slower easySFTP and a
    slower line stop looking the same.
    """
    written = []
    for doc in bench.documents(args.paths, bench.newest_release_json):
        profiles = [
            profile
            for profile in sorted({doc.profile(row) for row in doc.results})
            if selected(args, profile=profile) and doc.controls(profile)
        ]
        if not profiles:
            continue
        written.append(_draw_link(doc, profiles, args))
    return written


def _draw_link(doc, profiles, args):
    scenarios = sorted(
        {
            row.get("scenario", "?")
            for row in doc.results
            if selected(args, row.get("scenario"))
        }
    )
    measured = {}
    for profile in profiles:
        for scenario in scenarios:
            if doc.is_matrix:
                entry = next(
                    (
                        item
                        for item in doc.scaling
                        if item.get("scenario") == scenario
                        and (item.get("link_profile") or "baseline") == profile
                    ),
                    None,
                )
                value = (entry.get("best") or {}).get("mib_per_s") if entry else None
            else:
                row = next(
                    (
                        item
                        for item in doc.results
                        if item.get("scenario") == scenario
                        and doc.profile(item) == profile
                    ),
                    None,
                )
                value = doc.throughput(row)[0] if row else None
            measured[(profile, scenario)] = value

    series = [("control, 1 stream", "single_mib_per_s"), ("control, N streams", "n_mib_per_s")]
    width = 0.8 / (len(series) + len(scenarios))
    figure, (top, bottom) = plt.subplots(
        2,
        1,
        figsize=(max(9.0, 3.0 + 2.4 * len(profiles)), 8.0),
        height_ratios=(3, 1),
        sharex=True,
    )

    for index, (name, field) in enumerate(series):
        values = [(doc.controls(profile) or {}).get(field) or 0.0 for profile in profiles]
        top.bar(
            [p + index * width for p in range(len(profiles))],
            values,
            width=width,
            color="#555555" if index == 0 else "#999999",
            label=name,
        )
    for index, scenario in enumerate(scenarios):
        offset = (len(series) + index) * width
        values = [measured[(profile, scenario)] or 0.0 for profile in profiles]
        bars = top.bar(
            [p + offset for p in range(len(profiles))],
            values,
            width=width,
            color=COLORS(index * 2 % 20),
            label=scenario + (" (best cell)" if doc.is_matrix else ""),
        )
        for bar, profile, value in zip(bars, profiles, values):
            control = (doc.controls(profile) or {}).get("n_mib_per_s")
            if control and value:
                top.text(
                    bar.get_x() + bar.get_width() / 2,
                    value,
                    f"{value / control * 100:.0f}%",
                    ha="center",
                    va="bottom",
                    fontsize=7,
                )

    top.set_ylabel("MiB/s")
    top.set_title("Measured throughput against the link's own control", fontsize=11)
    top.legend(fontsize=8, ncol=2)
    top.grid(axis="y", alpha=0.3)

    rtts = [(doc.controls(profile) or {}).get("rtt_p50_ms") or 0.0 for profile in profiles]
    bottom.bar(range(len(profiles)), rtts, width=0.45, color="#b05a7a")
    for index, value in enumerate(rtts):
        bottom.text(index, value, f" {value:.1f} ms", va="center", fontsize=8)
    bottom.set_ylabel("RTT p50 (ms)")
    bottom.set_xticks(
        [p + 0.4 - width / 2 for p in range(len(profiles))], profiles, fontsize=9
    )
    bottom.grid(axis="y", alpha=0.3)

    footer(
        figure,
        doc,
        [
            "the percentage over a bar is its share of the N stream control",
            "the control is measured with x/crypto/ssh and pkg/sftp, not with "
            "easySFTP's uploader: it separates the line from easySFTP, not "
            "pkg/sftp from the line",
        ],
    )
    return save(figure, args, f"link-{slug(document_stem(doc))}")


# --------------------------------------------------------------------------
# auto: the policy against the settings a sweep would have chosen
# --------------------------------------------------------------------------


def auto(args):
    """What easySFTP picks for itself, scored against the best cell.

    `auto` chooses a coordinate rather than sitting at one, which is why it is
    not a build label in the grid. The regret is the whole point of measuring
    it: a policy within roughly 15% on every profile is defensible, one that
    only wins on the house line is not. Changing the policy is issue #156;
    this only measures it.
    """
    written = []
    for doc in bench.documents(args.paths, bench.newest_matrix_json):
        rows = [
            row
            for row in doc.auto
            if selected(args, row.get("scenario"), row.get("link_profile") or "baseline")
        ]
        if not rows:
            continue
        written.append(_draw_auto(doc, rows, args))
    return written


def _draw_auto(doc, rows, args):
    scenarios = sorted({row.get("scenario", "?") for row in rows})
    profiles = sorted({row.get("link_profile") or "baseline" for row in rows})
    width = 0.8 / max(len(profiles), 1)

    figure, (top, bottom) = plt.subplots(
        2, 1, figsize=(3.0 + 2.4 * len(scenarios), 8.4), sharex=True
    )

    for index, profile in enumerate(profiles):
        offset = index * width
        picked = [
            next(
                (
                    row
                    for row in rows
                    if row.get("scenario") == scenario
                    and (row.get("link_profile") or "baseline") == profile
                ),
                None,
            )
            for scenario in scenarios
        ]
        regrets = [number(row or {}, "regret_percent") or 0.0 for row in picked]
        bars = top.bar(
            [p + offset for p in range(len(scenarios))],
            regrets,
            width=width,
            color=COLORS(index * 2 % 20),
            label=profile,
        )
        for bar, row in zip(bars, picked):
            if row is None:
                continue
            chosen = row.get("chosen") or {}
            top.text(
                bar.get_x() + bar.get_width() / 2,
                bar.get_height(),
                "/".join(
                    axis_label(key, chosen.get(key))
                    for key in ("connections", "concurrency", "request_concurrency")
                ),
                ha="center",
                va="bottom",
                fontsize=7,
                rotation=90,
            )

        # Pale bar first, solid and narrower on top of it: the same x, so the
        # visible pale part above the solid one is exactly what was left on the
        # table.
        bottom.bar(
            [p + offset for p in range(len(scenarios))],
            [number((row or {}).get("best") or {}, "mib_per_s") or 0.0 for row in picked],
            width=width,
            color=COLORS(index * 2 % 20),
            alpha=0.3,
            edgecolor=COLORS(index * 2 % 20),
            linewidth=0.6,
        )
        bottom.bar(
            [p + offset for p in range(len(scenarios))],
            [number(row or {}, "mib_per_s") or 0.0 for row in picked],
            width=width * 0.55,
            color=COLORS(index * 2 % 20),
        )

    # In the legend rather than on the axes: the bars fill the width, and a
    # label written over them is unreadable exactly where it matters.
    top.axhline(
        15,
        linestyle="--",
        color="crimson",
        linewidth=1,
        label="15%, the line a defensible policy stays under",
    )
    # Headroom for the rotated coordinate labels, which sit on top of the bars.
    top.set_ylim(
        0, max([number(row, "regret_percent") or 0.0 for row in rows] + [16.0]) * 1.35
    )
    top.set_ylabel("regret (%) against the best cell")
    top.set_title(
        "What easySFTP picks for itself (label: connections/concurrency/request_concurrency)",
        fontsize=11,
    )
    top.legend(fontsize=8, ncol=2)
    top.grid(axis="y", alpha=0.3)

    bottom.set_ylabel("MiB/s")
    bottom.set_xticks(
        [p + 0.4 - width / 2 for p in range(len(scenarios))], scenarios, fontsize=9
    )
    bottom.legend(
        handles=[
            matplotlib.patches.Patch(facecolor="#555555", label="what auto reached"),
            matplotlib.patches.Patch(
                facecolor="#555555", alpha=0.3, label="the fastest cell measured"
            ),
        ],
        fontsize=8,
    )
    bottom.grid(axis="y", alpha=0.3)
    bottom.set_title("color: the link profile, as above", fontsize=9)

    outside = [
        f"{row.get('scenario')} / {row.get('link_profile') or 'baseline'}"
        for row in rows
        if row.get("chosen_in_grid") is False
    ]
    notes = [
        "the picked settings are read back from the run's own counters, so they "
        "are what easySFTP did and not what this script assumes"
    ]
    if outside:
        notes.append(
            "picked a coordinate that is not in the grid for " + ", ".join(outside)
            + ": the regret there is against the nearest measured cells, not an "
            "exact pair"
        )
    footer(figure, doc, notes)
    return save(figure, args, f"auto-{slug(document_stem(doc))}")


# --------------------------------------------------------------------------
# canary: did the line hold still for the whole sweep
# --------------------------------------------------------------------------


def canary(args):
    """One fixed cell, measured at the start, the middle and the end.

    A spread larger than the deltas the sweep is read for means the server or
    the line moved during the run, and the whole thing is a poor comparison
    basis. That is a property of the run, not of any cell in it.
    """
    written = []
    for doc in bench.documents(args.paths, bench.newest_matrix_json):
        rows = [
            row
            for row in doc.canary
            if selected(args, row.get("scenario"), row.get("link_profile") or "baseline")
        ]
        if not rows:
            continue
        written.append(_draw_canary(doc, rows, args))
    return written


def _draw_canary(doc, rows, args):
    # "mid" is what the harness writes; the longer spelling is accepted so a
    # rename does not silently sort the end of the sweep into the middle.
    order = ["start", "mid", "middle", "end"]
    stages = [stage for stage in order if any(row.get("at") == stage for row in rows)]
    stages += sorted({row.get("at") for row in rows if row.get("at") not in order})
    profiles = sorted({row.get("link_profile") or "baseline" for row in rows})

    figure, axes = plt.subplots(figsize=(8.5, 5.4))
    spreads = []
    for index, profile in enumerate(profiles):
        line = []
        for position, stage in enumerate(stages):
            row = next(
                (
                    row
                    for row in rows
                    if (row.get("link_profile") or "baseline") == profile
                    and row.get("at") == stage
                ),
                None,
            )
            duration = number(row or {}, "duration_ms")
            if duration is not None:
                line.append((position, duration))
        if not line:
            continue
        axes.plot(
            [p for p, _ in line],
            [d for _, d in line],
            marker="o",
            color=COLORS(index * 2 % 20),
            label=profile,
        )
        durations = [d for _, d in line]
        spread = (max(durations) - min(durations)) / min(durations) * 100
        spreads.append((profile, spread))
        axes.annotate(
            f"spread {spread:.1f}%",
            (line[-1][0], line[-1][1]),
            fontsize=8,
            textcoords="offset points",
            xytext=(6, 0),
        )

    axes.set_xticks(range(len(stages)), stages)
    axes.set_xlim(-0.25, len(stages) - 1 + 0.75)  # room for the spread labels
    axes.set_ylabel("duration of the fixed cell (ms)")
    axes.set_xlabel("when in the profile's grid")
    axes.grid(alpha=0.3)
    axes.legend(fontsize=8)
    sample = rows[0]
    axes.set_title(
        "Drift check: one fixed cell repeated through the sweep\n"
        f"{sample.get('scenario', '?')}, connections {sample.get('connections')}, "
        f"concurrency {sample.get('concurrency')}",
        fontsize=10,
    )

    notes = [
        "a spread larger than the deltas the sweep is read for means the server "
        "or the line moved during the run, and the whole run is a poor "
        "comparison basis"
    ]
    failed = [row for row in rows if row.get("exit_code") not in (0, None)]
    if failed:
        notes.append(f"{len(failed)} canary run(s) exited non zero")
    footer(figure, doc, notes)
    return save(figure, args, f"canary-{slug(document_stem(doc))}")


# --------------------------------------------------------------------------
# trend: across the stored releases
# --------------------------------------------------------------------------


def trend(args):
    """Throughput per scenario across the stored release measurements.

    Releases only: a manual result is a measurement of whatever someone was
    trying at the time, and a sweep is not a comparison basis at all. Both are
    in trend.csv and both are dropped here.
    """
    path = (args.paths or [BENCHMARKS / "trend.csv"])[0]
    series = defaultdict(list)
    runners = set()
    for row in bench.rows(path):
        if row.get("kind") != "release":
            continue
        scenario = row.get("scenario", "?")
        if not selected(args, scenario):
            continue
        recorded = row.get("recorded_at") or ""
        speed = number(row, "mib_per_s")
        if speed is None or not recorded:
            continue
        try:
            when = datetime.strptime(recorded, "%Y-%m-%dT%H:%M:%SZ")
        except ValueError:
            continue
        runner = row.get("runner") or "unknown"
        series[scenario].append(
            {
                "when": when,
                "mib_per_s": speed,
                "files_per_s": number(row, "files_per_s"),
                "label": row.get("version") or row.get("label") or "?",
                "runner": runner,
            }
        )
        runners.add(runner)

    if not series:
        return []

    markers = {runner: mark for runner, mark in zip(sorted(runners), "os^Dv*Xp")}
    figure, (top, bottom) = plt.subplots(2, 1, figsize=(10, 8.4), sharex=True)
    for index, (scenario, points) in enumerate(sorted(series.items())):
        points.sort(key=lambda point: point["when"])
        color = COLORS(index * 2 % 20)
        for axes, field in ((top, "mib_per_s"), (bottom, "files_per_s")):
            usable = [point for point in points if point.get(field) is not None]
            if not usable:
                continue
            axes.plot(
                [point["when"] for point in usable],
                [point[field] for point in usable],
                color=color,
                label=scenario if axes is top else None,
                zorder=2,
            )
            for point in usable:
                axes.scatter(
                    [point["when"]],
                    [point[field]],
                    color=color,
                    marker=markers.get(point["runner"], "o"),
                    zorder=3,
                )
    # One label per release, over the highest scenario measured for it: one
    # per point puts three labels on top of each other on release day.
    by_release = defaultdict(list)
    for points in series.values():
        for point in points:
            by_release[point["when"]].append(point)
    for when, points in by_release.items():
        top.annotate(
            points[0]["label"],
            (when, max(point["mib_per_s"] for point in points)),
            fontsize=8,
            ha="center",
            textcoords="offset points",
            xytext=(0, 8),
        )

    top.set_ylabel("MiB/s")
    bottom.set_ylabel("files/s")
    bottom.set_xlabel("release")
    top.legend(title="scenario", fontsize=8)
    for axes in (top, bottom):
        axes.grid(alpha=0.3)
    top.set_title("Release benchmarks over time", fontsize=11)

    notes = [
        "marker per runner: "
        + ", ".join(f"{mark} {runner}" for runner, mark in markers.items())
    ]
    warnings = []
    if len(runners) > 1:
        warnings.append(
            "measured on more than one runner: points with different markers are "
            "not comparable, and since issue #184 the link matters as much as "
            "the runner does"
        )
    footer(figure, None, notes, warnings)
    return [save(figure, args, "trend-releases")]


# --------------------------------------------------------------------------
# report: everything the given files support, and an index of it
# --------------------------------------------------------------------------

REPORT_SECTIONS = [
    ("heatmap", "The grid", "One measured configuration per square: `connections` down, `concurrency` across, colored by throughput and labelled with the median duration. The red square is the fastest cell."),
    ("scaling", "Where scaling stops paying", "The same cells as curves against the axis that was actually swept. The dotted line is perfect scaling from the leftmost point; the gap to it is what the setting is worth."),
    ("auto", "The policy against the grid", "What easySFTP picks for itself, scored against the fastest cell of the same scenario and profile (issue #184, phase 5; the policy itself is issue #156)."),
    ("canary", "Did the line hold still", "One fixed cell repeated through the sweep. A large spread makes the whole run a poor comparison basis."),
    ("phases", "Where the wall clock goes", "Phases are wall clock and add up to roughly the run's duration."),
    ("operations", "What the round-trips cost", "Cumulative work per operation on the left, per call cost on the right. The two panels are different kinds of number."),
    ("deletes", "Deletion", "The clean deployment measured before every run, which is the only measurement of deletion in the harness."),
    ("link", "Against the line itself", "Measured throughput next to the control the link probe took over the same path."),
    ("trend", "Across releases", "One point per stored release measurement, comparable only within one runner and one link."),
]


def source_label(path):
    """How a source is named in the index: its path inside the repository.

    Resolved first, because a path given on the command line is normally
    relative to the working directory ("benchmarks/latest.json") while the
    fallbacks are absolute, and comparing the two forms directly is what used
    to make `report` raise on exactly the invocation the documentation shows.
    A file from outside the repository keeps the name it was given.
    """
    given = Path(path)
    try:
        return given.resolve().relative_to(bench.REPO).as_posix()
    except ValueError:
        return given.as_posix()


def report(args):
    """Every plot the given files support, plus a Markdown index of them."""
    paths = args.paths or [bench.newest_release_json(), bench.newest_matrix_json()]
    matrices, standards = [], []
    for path in paths:
        path = Path(path)
        if path.suffix == ".csv":
            continue
        document = bench.load(path)
        (matrices if document.is_matrix else standards).append(path)

    produced = {}
    for name, function, targets in (
        ("heatmap", heatmap, [Path(p).with_suffix(".csv") for p in matrices]),
        ("scaling", scaling, matrices),
        ("auto", auto, matrices),
        ("canary", canary, matrices),
        ("phases", phases, standards + matrices),
        ("operations", operations, standards + matrices),
        ("deletes", deletes, standards + matrices),
        ("link", link, standards + matrices),
        ("trend", trend, []),
    ):
        targets = [path for path in targets if Path(path).exists()]
        if not targets and name != "trend":
            continue
        scoped = argparse.Namespace(**vars(args))
        scoped.paths = targets
        try:
            produced[name] = function(scoped)
        except SystemExit as failure:  # a missing input is not a failed report
            print(f"{name}: {failure}", file=sys.stderr)
            produced[name] = []

    written = [path for paths in produced.values() for path in paths]
    if not written:
        return []

    lines = [
        "# Benchmark analysis",
        "",
        "Generated by `benchmarks/analysis/plot.py report`. Every image here is a",
        "reading of a stored result; if a plot and the JSON disagree, the JSON is",
        "right (`benchmarks/analysis/README.md`).",
        "",
        "## Sources",
        "",
    ]
    for path in paths:
        lines.append(f"- `{source_label(path)}`")
    lines.append("")

    for name, title, description in REPORT_SECTIONS:
        images = produced.get(name) or []
        if not images:
            continue
        lines += [f"## {title}", "", description, ""]
        for image in images:
            lines.append(f"![{image.stem}]({image.name})")
            lines.append("")

    args.out.mkdir(parents=True, exist_ok=True)
    target = args.out / "report.md"
    target.write_text("\n".join(lines), encoding="utf-8")
    return written + [target]


# --------------------------------------------------------------------------
# CLI
# --------------------------------------------------------------------------

COMMANDS = {
    "heatmap": heatmap,
    "scaling": scaling,
    "phases": phases,
    "operations": operations,
    "deletes": deletes,
    "link": link,
    "auto": auto,
    "canary": canary,
    "trend": trend,
    "report": report,
}


def csv_list(text):
    return [piece.strip() for piece in text.split(",") if piece.strip()]


def main():
    parser = argparse.ArgumentParser(
        description=__doc__,
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    parser.add_argument("command", choices=sorted(COMMANDS))
    parser.add_argument(
        "paths",
        nargs="*",
        type=Path,
        help="stored files to read; the newest of the right kind when omitted",
    )
    parser.add_argument("--out", type=Path, default=DEFAULT_OUT)
    parser.add_argument("--format", default="png", choices=["png", "svg", "pdf"])
    parser.add_argument("--dpi", type=int, default=140)
    parser.add_argument(
        "--scenario",
        type=csv_list,
        default=[],
        help="only these scenarios, comma separated",
    )
    parser.add_argument(
        "--profile",
        type=csv_list,
        default=[],
        help="only these link profiles, comma separated",
    )
    parser.add_argument(
        "--metric",
        default="mib_per_s",
        choices=sorted(METRICS),
        help="what the heatmap colors and ranks by",
    )
    parser.add_argument(
        "--include-deletes",
        action="store_true",
        help="phases: add the delete sweeps as their own bars",
    )
    args = parser.parse_args()

    written = COMMANDS[args.command](args)
    if not written:
        sys.exit(
            f"nothing to plot: no input {args.command} can read had the fields it "
            "needs, or the filters excluded everything"
        )
    for path in written:
        print(path)


if __name__ == "__main__":
    main()
