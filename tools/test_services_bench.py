import contextlib
import io
import json
import os
import stat
import sys
import tempfile
import time
import unittest
from pathlib import Path
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parent))
import services_bench  # noqa: E402


class SummaryTest(unittest.TestCase):
    def test_a_failed_launch_is_counted_not_timed(self):
        rows = [{"seconds": 4.0}, {"seconds": None}, {"seconds": 2.0}, {"seconds": 3.0}]
        self.assertEqual(services_bench.summarize(rows), {"n": 3, "failed": 1, "p50": 3.0, "min": 2.0, "max": 4.0})

    def test_no_launch_at_all_has_no_numbers(self):
        self.assertEqual(services_bench.summarize([{"seconds": None}]), {"n": 0, "failed": 1, "p50": None, "min": None, "max": None})


# A stand-in coop: it records each call (where it ran, its arguments, and the project and auto-up it
# was given), and `run` does what the box would, or hangs after its marker, or talks a lot first.
STAND_IN = """#!/bin/sh
printf '%s|%s|%s|%s\\n' "$(pwd -P)" "$*" "$COOP_REPO" "$COOP_AUTO_UP" >> "@LOG@"
[ "$1" = up ] && [ -n "$STAND_IN_UP_FAILS" ] && { echo "services did not start" >&2; exit 1; }
[ "$1" = run ] || exit 0
echo $$ > "@LOG@.pid"
[ -n "$STAND_IN_CHATTY" ] && head -c 1000000 /dev/zero | tr '\\0' x
shift 2
"$@"
[ -n "$STAND_IN_HANG" ] && exec sleep 30
exit 0
"""


class OwnProjectOnlyTest(unittest.TestCase):
    """The bench runs coop up, coop down and coop run in the project it was given, and nothing else:
    it never removes a box or volume itself, and it always leaves the project's services stopped."""

    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.repo, self.log, self.out = Path(tmp.name, "repo"), Path(tmp.name, "calls.log"), Path(tmp.name, "out")
        self.repo.mkdir()
        self.coop = Path(tmp.name, "coop")
        self.coop.write_text(STAND_IN.replace("@LOG@", str(self.log)))
        self.coop.chmod(self.coop.stat().st_mode | stat.S_IXUSR)

    def bench(self, samples=2):
        with contextlib.redirect_stdout(io.StringIO()):
            return services_bench.main(["--repo", str(self.repo), "--samples", str(samples), "--out", str(self.out),
                                        "--coop", str(self.coop)])

    def calls(self):
        return [line.split("|") for line in self.log.read_text().splitlines()]

    def test_it_only_runs_up_down_and_run_in_its_project(self):
        # a shell pointed at another project, with services off, changes neither
        with patch.dict(os.environ, {"COOP_REPO": "/somewhere/else", "COOP_AUTO_UP": "0"}):
            summary = self.bench()
        self.assertEqual({case: row["n"] for case, row in summary.items()}, {"stopped": 2, "running": 2})
        calls = self.calls()
        self.assertTrue(calls)
        for cwd, args, project, auto_up in calls:
            with self.subTest(call=args):
                self.assertEqual(cwd, str(self.repo.resolve()))
                self.assertEqual(project, str(self.repo))
                self.assertEqual(auto_up, "1", "the stopped case must time the launch that starts the services")
                self.assertIn(args.split()[0], {"up", "down", "run"})
        self.assertEqual(calls[-1][1], "down", "the bench leaves the project's services stopped")
        self.assertFalse(Path(self.repo, services_bench.MARK).exists(), "the marker is cleaned up")
        data = json.loads(Path(self.out, "samples.json").read_text())
        self.assertEqual(len(data["samples"]["stopped"]), 2)

    def test_a_launch_that_hangs_after_its_marker_is_stopped(self):
        with patch.dict(os.environ, {"STAND_IN_HANG": "1"}), patch.object(services_bench, "TIMEOUT", 1):
            summary = self.bench(samples=1)
        self.assertEqual(summary["stopped"]["n"], 1, "the start itself was timed")
        with self.assertRaises(ProcessLookupError, msg="the hung launch was left running"):
            os.kill(int(Path(str(self.log) + ".pid").read_text()), 0)
        self.assertEqual(self.calls()[-1][1], "down")

    def test_a_chatty_launch_does_not_stall_before_its_marker(self):
        # a megabyte of output is more than a pipe holds: unread, it would block the launch
        with patch.dict(os.environ, {"STAND_IN_CHATTY": "1"}), patch.object(services_bench, "TIMEOUT", 20):
            summary = self.bench(samples=1)
        self.assertEqual((summary["stopped"]["failed"], summary["running"]["failed"]), (0, 0))

    def test_an_interrupted_bench_still_stops_the_services(self):
        real_sleep = time.sleep
        def interrupt(seconds):  # Ctrl-C while waiting for the first launch's marker
            real_sleep(seconds)
            raise KeyboardInterrupt
        with patch.dict(os.environ, {"STAND_IN_HANG": "1"}), patch.object(services_bench.time, "sleep", interrupt), \
                self.assertRaises(KeyboardInterrupt):
            self.bench(samples=1)
        self.assertEqual(self.calls()[-1][1], "down")
        pid = Path(str(self.log) + ".pid")
        if pid.exists():
            with self.assertRaises(ProcessLookupError, msg="the interrupted launch was left running"):
                os.kill(int(pid.read_text()), 0)

    def test_a_failed_up_stops_the_bench_instead_of_mislabeling_samples(self):
        with patch.dict(os.environ, {"STAND_IN_UP_FAILS": "1"}), self.assertRaises(SystemExit) as stopped:
            self.bench(samples=1)
        self.assertIn("coop up exited 1: services did not start", str(stopped.exception))
        self.assertFalse(self.out.exists(), "no samples are written for a run that could not set its cases up")
        self.assertEqual(self.calls()[-1][1], "down")


if __name__ == "__main__":
    unittest.main()
