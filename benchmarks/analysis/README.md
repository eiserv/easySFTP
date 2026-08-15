# Benchmark analysis (optional)

Downstream reading of benchmark results: heatmaps of a sweep, throughput
trends across releases. This directory is **not** part of running, storing or
validating a benchmark. `cmd/easysftp-bench` does all of that and needs nothing
from here (issue #190).

The relationship is one way. Everything in here consumes the canonical outputs
the Go harness writes, and nothing in here may become an input to them:

```
cmd/easysftp-bench  ->  results.json / matrix.json / *.csv / index.json / trend.csv
                             |
                             v
                    benchmarks/analysis/  ->  plots, models, notebooks
```

A chart is a reading of the data, never a replacement for it. If a plot and the
JSON disagree, the JSON is right. Do not commit generated plots by default;
`out/` is ignored. Attach them to the run or the issue that needed them, and
commit one only when a document refers to it.

## Use

```bash
python -m venv .venv && . .venv/bin/activate   # Windows: .venv\Scripts\activate
pip install -r benchmarks/analysis/requirements.txt
```

A heatmap per scenario, build and link profile, from the newest stored sweep:

```bash
python benchmarks/analysis/plot.py heatmap
```

A specific sweep, and the release trend:

```bash
python benchmarks/analysis/plot.py heatmap benchmarks/matrix/matrix-20260730T224549Z-main.csv
```

```bash
python benchmarks/analysis/plot.py trend
```

Both write PNGs into `benchmarks/analysis/out/` unless `--out` says otherwise,
and print the path of every file they wrote.

## Reading the plots

A **heatmap** cell is one measured configuration: `connections` down,
`concurrency` across, colored by throughput (brighter is faster) and labelled
with the median duration. Two things to look for, and they are the reason the
grid exists at all:

- Where the bright region stops. Scaling that flattens along `concurrency` but
  keeps improving along `connections` means the run is bounded per connection,
  not by worker count.
- Whether the best cell touches the edge of the grid. If it does, the optimum
  was not measured, only bounded from below, and nothing should be fitted to
  the numbers until the axis is extended. `matrix.json` says the same thing in
  `scaling[].best_at_axis_max`, and that field is the authority.

A **trend** point is one stored release measurement of one scenario. Compare
points only within the same `environment` and, since issue #184, the same
`link`: a faster line and a faster easySFTP look identical here otherwise. The
trend chart cannot tell them apart and does not try.

## Adding to this

Keep it reproducible and keep it optional: read only the canonical files, pin
what you import in `requirements.txt`, and do not reach into `internal/` or
shell out to the harness. If an analysis turns out to be something the project
needs on every run, that is an argument for putting it in the Go harness, not
for making this directory a dependency of it.
