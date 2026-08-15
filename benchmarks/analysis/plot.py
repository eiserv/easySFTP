#!/usr/bin/env python3
"""Plots of stored benchmark results.

Reads the canonical CSVs the Go harness writes and nothing else: a sweep's
matrix.csv and the top-level trend.csv. It never runs a benchmark, never writes
into benchmarks/ outside the output directory, and is not needed by anything
that does (issue #190).

Columns are read by name, because the stored files span several schema
versions: the sweeps from before issue #184 have no link_profile and no
request_concurrency, and asking for a column that is not there is how a plot
silently starts showing the wrong axis.

    python benchmarks/analysis/plot.py heatmap [matrix.csv ...] [--out DIR]
    python benchmarks/analysis/plot.py trend   [trend.csv]      [--out DIR]
"""

import argparse
import csv
import sys
from collections import defaultdict
from datetime import datetime
from pathlib import Path

import matplotlib

matplotlib.use("Agg")  # no display on a runner, and none needed
import matplotlib.pyplot as plt  # noqa: E402

REPO = Path(__file__).resolve().parents[2]
BENCHMARKS = REPO / "benchmarks"
DEFAULT_OUT = BENCHMARKS / "analysis" / "out"


def rows(path):
    """The CSV as dicts, with empty strings left alone: '' is 'not recorded'."""
    with open(path, newline="", encoding="utf-8") as handle:
        return list(csv.DictReader(handle))


def number(row, column):
    """A numeric column, or None when it is absent or empty."""
    text = (row.get(column) or "").strip()
    if not text:
        return None
    try:
        return float(text)
    except ValueError:
        return None


def slug(text):
    keep = [c if c.isalnum() or c in "-_." else "-" for c in str(text)]
    return "".join(keep).strip("-") or "none"


def newest_matrix():
    sweeps = sorted((BENCHMARKS / "matrix").glob("matrix-*.csv"))
    if not sweeps:
        sys.exit("no sweep found under benchmarks/matrix; pass one explicitly")
    return [sweeps[-1]]


def heatmap(paths, out):
    """One heatmap per scenario, build, link profile and request_concurrency.

    Those four are what a cell is not: mixing any of them into one grid would
    put two different configurations in the same square.
    """
    written = []
    for path in paths:
        grids = defaultdict(dict)
        for row in rows(path):
            connections = number(row, "connections")
            concurrency = number(row, "concurrency")
            median = number(row, "median_ms")
            if connections is None or concurrency is None or median is None:
                continue
            key = (
                row.get("scenario", "?"),
                row.get("build", "?"),
                row.get("link_profile") or "baseline",
                row.get("request_concurrency") or "default",
            )
            grids[key][(int(connections), int(concurrency))] = (
                median,
                number(row, "mib_per_s"),
            )

        for key, cells in sorted(grids.items()):
            scenario, build, profile, requests = key
            if len(cells) < 2:
                # A single cell is a number, not a grid. matrix.json has it.
                continue
            written.append(_draw_heatmap(path, key, cells, out))

    if not written:
        sys.exit("nothing to plot: no sweep in those files had a grid")
    for item in written:
        print(item)


def _draw_heatmap(source, key, cells, out):
    scenario, build, profile, requests = key
    connections = sorted({c for c, _ in cells})
    concurrency = sorted({c for _, c in cells})

    speeds = [
        [
            (cells.get((row, column)) or (None, None))[1]
            for column in concurrency
        ]
        for row in connections
    ]

    figure, axes = plt.subplots(
        figsize=(1.5 + 1.2 * len(concurrency), 1.5 + 0.9 * len(connections))
    )
    image = axes.imshow(
        [[value if value is not None else float("nan") for value in line] for line in speeds],
        cmap="viridis",
        aspect="auto",
    )
    figure.colorbar(image, ax=axes, label="MiB/s")

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
                x, y, f"{cell[0]:.0f} ms",
                ha="center", va="center", fontsize=8, color="white",
            )

    figure.tight_layout()
    out.mkdir(parents=True, exist_ok=True)
    name = f"heatmap-{slug(Path(source).stem)}-{slug(scenario)}-{slug(build)}-{slug(profile)}-{slug(requests)}.png"
    target = out / name
    figure.savefig(target, dpi=140)
    plt.close(figure)
    return target


def trend(paths, out):
    """Throughput per scenario across the stored release measurements.

    Releases only: a manual result is a measurement of whatever someone was
    trying at the time, and a sweep is not a comparison basis at all. Both are
    in trend.csv and both are dropped here.
    """
    path = paths[0]
    series = defaultdict(list)
    runners = set()
    for row in rows(path):
        if row.get("kind") != "release":
            continue
        speed = number(row, "mib_per_s")
        recorded = row.get("recorded_at") or ""
        if speed is None or not recorded:
            continue
        try:
            when = datetime.strptime(recorded, "%Y-%m-%dT%H:%M:%SZ")
        except ValueError:
            continue
        label = row.get("version") or row.get("label") or "?"
        series[row.get("scenario", "?")].append((when, speed, label))
        runners.add(row.get("runner") or "unknown")

    if not series:
        sys.exit(f"no release measurement in {path}")

    figure, axes = plt.subplots(figsize=(9, 5))
    for scenario, points in sorted(series.items()):
        points.sort()
        axes.plot(
            [p[0] for p in points], [p[1] for p in points],
            marker="o", label=scenario,
        )
        for when, speed, label in points:
            axes.annotate(label, (when, speed), fontsize=7,
                          textcoords="offset points", xytext=(0, 6))

    axes.set_ylabel("MiB/s")
    axes.set_xlabel("release")
    axes.legend(title="scenario")
    axes.grid(alpha=0.3)
    axes.set_title("Release benchmarks over time")
    # Not decoration: two points measured on different runners or over
    # different links are not comparable, and the chart cannot tell them apart.
    if len(runners) > 1:
        figure.text(
            0.5, 0.01,
            "measured on more than one runner: these points are not comparable",
            ha="center", fontsize=8, color="crimson",
        )

    figure.tight_layout()
    out.mkdir(parents=True, exist_ok=True)
    target = out / "trend-releases.png"
    figure.savefig(target, dpi=140)
    plt.close(figure)
    print(target)


def main():
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("command", choices=["heatmap", "trend"])
    parser.add_argument("paths", nargs="*", type=Path)
    parser.add_argument("--out", type=Path, default=DEFAULT_OUT)
    args = parser.parse_args()

    if args.command == "heatmap":
        heatmap(args.paths or newest_matrix(), args.out)
    else:
        trend(args.paths or [BENCHMARKS / "trend.csv"], args.out)


if __name__ == "__main__":
    main()
