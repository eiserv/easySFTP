#!/usr/bin/env python3
"""Self-checks for the analysis layer.

    python -m unittest discover -s benchmarks/analysis

Two things are worth asserting here and nothing else is. First, that the
readers keep reading every result already committed: the stored files span
several schema versions and the newest one is not the only one on disk, so
`benchdata` is exercised against all of them rather than against a fixture
written today. Second, that every command draws something from the newest
stored files, into a temporary directory, because a plot that raises on a
field that moved is the failure mode this layer actually has.

Nothing in here writes into `benchmarks/`, and nothing in here needs a
benchmark to have been run.
"""

import json
import sys
import tempfile
import unittest
from argparse import Namespace
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

import benchdata as bench  # noqa: E402
import plot  # noqa: E402

STORED = sorted(
    path
    for path in bench.BENCHMARKS.rglob("*.json")
    if path.name not in {"index.json"}
)


def options(out, **overrides):
    settings = {
        "paths": [],
        "out": Path(out),
        "format": "png",
        "dpi": 70,  # small: these files are thrown away at the end of the test
        "scenario": [],
        "profile": [],
        "metric": "mib_per_s",
        "include_deletes": False,
    }
    settings.update(overrides)
    return Namespace(**settings)


class Helpers(unittest.TestCase):
    def test_number_treats_blank_as_absent(self):
        row = {"a": "", "b": "3.5", "c": 7, "d": "x", "e": None}
        self.assertIsNone(bench.number(row, "a"))
        self.assertIsNone(bench.number(row, "missing"))
        self.assertIsNone(bench.number(row, "d"))
        self.assertIsNone(bench.number(row, "e"))
        self.assertEqual(bench.number(row, "b"), 3.5)
        self.assertEqual(bench.number(row, "c"), 7.0)

    def test_axis_layout_follows_what_varies(self):
        # A payload that only sweeps request_concurrency (`single`, one file)
        # must not get concurrency on the x axis: that is a one point line.
        points = [
            {"connections": 1, "concurrency": 1, "request_concurrency": value}
            for value in (1, 16, 64)
        ]
        x, series, facets = bench.axis_layout(points)
        self.assertEqual(x, "request_concurrency")
        self.assertEqual(facets, [])
        self.assertIn(series, ("concurrency", "connections"))

        points = [
            {"connections": c, "concurrency": w, "request_concurrency": None}
            for c in (1, 2, 4)
            for w in (1, 2, 4, 8)
        ]
        x, series, facets = bench.axis_layout(points)
        self.assertEqual((x, series), ("concurrency", "connections"))
        self.assertEqual(facets, [])

    def test_axis_label_keeps_null_apart_from_a_number(self):
        self.assertEqual(bench.axis_label("request_concurrency", None), "default")
        self.assertEqual(bench.axis_label("request_concurrency", 16), "16")
        self.assertEqual(bench.axis_label("connections", 4.0), "4")

    def test_sort_key_puts_the_unset_value_first(self):
        self.assertEqual(
            sorted([16, None, 1], key=plot.sort_key), [None, 1, 16]
        )


class Documents(unittest.TestCase):
    def test_every_stored_result_loads(self):
        self.assertTrue(STORED, "no stored result found to read")
        for path in STORED:
            with self.subTest(path=path.name):
                doc = bench.load(path)
                self.assertIn(doc.kind, {"release", "manual", "matrix", "standard"})
                self.assertTrue(doc.results, "a stored result with no rows")
                self.assertTrue(doc.provenance())
                self.assertIsInstance(doc.caveats(), list)

    def test_throughput_is_recomputed_when_a_row_lacks_it(self):
        doc = bench.Document("x.json", {})
        mib, files = doc.throughput(
            {"bytes": 1024 * 1024, "files": 10, "median_ms": 1000}
        )
        self.assertAlmostEqual(mib, 1.0)
        self.assertAlmostEqual(files, 10.0)
        self.assertEqual(doc.throughput({"median_ms": 0}), (None, None))

    def test_a_baseline_only_run_gets_no_shaping_caveat(self):
        # "baseline" is the real line and is never shaped, so asking for it and
        # getting it is not a caveat.
        doc = bench.Document(
            "x.json",
            {
                "benchmark": {
                    "link": {
                        "shaping": {
                            "available": False,
                            "reason": "no profile asked for shaping",
                            "requested": ["baseline"],
                            "applied": ["baseline"],
                        }
                    }
                }
            },
        )
        self.assertEqual(doc.caveats(), [])

    def test_an_unshaped_profile_is_a_caveat(self):
        doc = bench.Document(
            "x.json",
            {
                "benchmark": {
                    "link": {
                        "shaping": {
                            "available": False,
                            "reason": "tc is not installed",
                            "requested": ["baseline", "+50ms"],
                            "applied": ["baseline"],
                        }
                    }
                }
            },
        )
        self.assertEqual(len(doc.caveats()), 1)
        self.assertIn("say what was asked for", doc.caveats()[0])

    def test_a_bare_measurement_without_an_envelope_still_loads(self):
        # results.json as the harness writes it, before store wraps it.
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "results.json"
            path.write_text(
                json.dumps(
                    {
                        "benchmark_kind": "standard",
                        "candidate_ref": "x (1234567)",
                        "runner": "test",
                        "results": [{"scenario": "small", "median_ms": 1}],
                    }
                ),
                encoding="utf-8",
            )
            doc = bench.load(path)
            self.assertEqual(doc.kind, "standard")
            self.assertEqual(len(doc.results), 1)


class Commands(unittest.TestCase):
    """Every command against the newest stored files of its kind."""

    def run_command(self, name, **overrides):
        with tempfile.TemporaryDirectory() as directory:
            written = plot.COMMANDS[name](options(directory, **overrides))
            self.assertTrue(written, f"{name} drew nothing")
            for path in written:
                self.assertTrue(path.exists(), f"{name} named a file it did not write")
                self.assertGreater(path.stat().st_size, 0)
            return [path.name for path in written]

    def test_heatmap(self):
        self.run_command("heatmap", scenario=["small"], profile=["baseline"])

    def test_scaling(self):
        self.run_command("scaling", scenario=["small"], profile=["baseline"])

    def test_auto(self):
        self.run_command("auto", scenario=["small"])

    def test_canary(self):
        self.run_command("canary")

    def test_phases(self):
        self.run_command("phases", include_deletes=True)

    def test_operations(self):
        names = self.run_command("operations", scenario=["small"])
        for name in names:
            # The file is named after the row it shows, not after whatever a
            # loop inside the drawing code left behind.
            self.assertIn("small", name)

    def test_deletes(self):
        self.run_command("deletes")

    def test_link(self):
        self.run_command("link")

    def test_trend(self):
        self.run_command("trend")

    def test_report_writes_an_index_of_what_it_drew(self):
        with tempfile.TemporaryDirectory() as directory:
            written = plot.report(
                options(directory, scenario=["small"], profile=["baseline"])
            )
            index = Path(directory) / "report.md"
            self.assertIn(index, written)
            text = index.read_text(encoding="utf-8")
            for path in written:
                if path.suffix == ".png":
                    self.assertIn(path.name, text, "an image the index does not list")

    def test_report_names_its_sources_however_they_were_given(self):
        # The paths a caller passes are normally relative to the working
        # directory (the documented commands and the release workflow both do
        # that), while the fallbacks are absolute. Both have to end up as the
        # same repository path in the index.
        absolute = bench.newest_release_json()
        relative = absolute.relative_to(bench.REPO)
        expected = relative.as_posix()
        self.assertEqual(plot.source_label(absolute), expected)
        self.assertEqual(plot.source_label(relative), expected)

        with tempfile.TemporaryDirectory() as directory:
            written = plot.report(
                options(
                    directory,
                    paths=[relative],
                    scenario=["small"],
                    profile=["baseline"],
                )
            )
            index = Path(directory) / "report.md"
            self.assertIn(index, written)
            self.assertIn(expected, index.read_text(encoding="utf-8"))

    def test_a_filter_that_matches_nothing_draws_nothing(self):
        with tempfile.TemporaryDirectory() as directory:
            self.assertEqual(
                plot.scaling(options(directory, scenario=["no-such-scenario"])), []
            )


if __name__ == "__main__":
    unittest.main()
