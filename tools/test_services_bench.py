import contextlib
import io
import json
import stat
import sys
import tempfile
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import services_bench  # noqa: E402


class SummaryTest(unittest.TestCase):
    def test_a_failed_launch_is_counted_not_timed(self):
        rows = [{"seconds": 4.0}, {"seconds": None}, {"seconds": 2.0}, {"seconds": 3.0}]
        self.assertEqual(services_bench.summarize(rows), {"n": 3, "failed": 1, "p50": 3.0, "min": 2.0, "max": 4.0})

    def test_no_launch_at_all_has_no_numbers(self):
        self.assertEqual(services_bench.summarize([{"seconds": None}]), {"n": 0, "failed": 1, "p50": None, "min": None, "max": None})


class OwnProjectOnlyTest(unittest.TestCase):
    """The bench runs coop up, coop down and coop run in the project it was given, and nothing else:
    it must never remove another run's boxes or volumes, as lifecycle_bench.py's cleanup can."""

    def test_it_only_runs_up_down_and_run_in_its_project(self):
        with tempfile.TemporaryDirectory() as tmp:
            repo, log = Path(tmp, "repo"), Path(tmp, "calls.log")
            repo.mkdir()
            coop = Path(tmp, "coop")
            # a stand-in coop: records each call and where it ran; `run` does what the box would
            coop.write_text("#!/bin/sh\n"
                            f'printf "%s|%s\\n" "$(pwd -P)" "$*" >> "{log}"\n'
                            'if [ "$1" = run ]; then shift 2; "$@"; fi\n')
            coop.chmod(coop.stat().st_mode | stat.S_IXUSR)
            out = Path(tmp, "out")
            with contextlib.redirect_stdout(io.StringIO()):
                summary = services_bench.main(["--repo", str(repo), "--samples", "2", "--out", str(out), "--coop", str(coop)])
            self.assertEqual({case: row["n"] for case, row in summary.items()}, {"stopped": 2, "running": 2})
            calls = [line.split("|", 1) for line in log.read_text().splitlines()]
            self.assertTrue(calls)
            for cwd, args in calls:
                with self.subTest(call=args):
                    self.assertEqual(cwd, str(repo.resolve()))
                    self.assertIn(args.split()[0], {"up", "down", "run"})
            self.assertEqual(calls[-1][1], "down", "the bench leaves the project's services stopped")
            self.assertFalse(Path(repo, services_bench.MARK).exists(), "the marker is cleaned up")
            data = json.loads(Path(out, "samples.json").read_text())
            self.assertEqual(len(data["samples"]["stopped"]), 2)

    def test_a_failed_up_stops_the_bench_instead_of_mislabeling_samples(self):
        with tempfile.TemporaryDirectory() as tmp:
            repo = Path(tmp, "repo")
            repo.mkdir()
            coop = Path(tmp, "coop")
            coop.write_text('#!/bin/sh\nif [ "$1" = up ]; then echo "services did not start" >&2; exit 1; fi\n'
                            'if [ "$1" = run ]; then shift 2; "$@"; fi\n')
            coop.chmod(coop.stat().st_mode | stat.S_IXUSR)
            out = Path(tmp, "out")
            with self.assertRaises(SystemExit) as stopped, contextlib.redirect_stdout(io.StringIO()):
                services_bench.main(["--repo", str(repo), "--samples", "1", "--out", str(out), "--coop", str(coop)])
            self.assertIn("coop up exited 1: services did not start", str(stopped.exception))
            self.assertFalse(out.exists(), "no samples are written for a run that could not set its cases up")


if __name__ == "__main__":
    unittest.main()
