"""Tests for the lifecycle benchmark's pure parts.

The cases themselves launch real boxes and are not unit-testable; what IS testable is everything a
wrong answer would travel through — the statistics, the redaction that decides what a retained
artifact says about the machine it ran on, the parse that turns the box's own clock into a number,
and the report that a later comparison reads back.
"""

import io
import json
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

import lifecycle_bench as bench


class PercentileTest(unittest.TestCase):
    def test_interpolates_between_samples(self):
        # Eight samples is a realistic run, and p95 has to land between the top two rather than
        # snapping to the max — otherwise every p95 is just "the slowest sample" under another name.
        values = [1.0, 2.0, 3.0, 4.0, 5.0, 6.0, 7.0, 8.0]
        self.assertAlmostEqual(bench.percentile(values, 0.5), 4.5)
        self.assertAlmostEqual(bench.percentile(values, 0.95), 7.65)

    def test_single_and_empty(self):
        self.assertEqual(bench.percentile([2.5], 0.95), 2.5)
        self.assertNotEqual(bench.percentile([], 0.5), bench.percentile([], 0.5))  # NaN

    def test_order_does_not_matter(self):
        self.assertEqual(bench.percentile([3.0, 1.0, 2.0], 0.5), 2.0)


class RedactTest(unittest.TestCase):
    def test_hides_the_machine_but_keeps_the_evidence(self):
        text = f"{bench.HOME}/Projects/os/coop failed at /tmp/coop-run-123/state"
        redacted = bench.redact(text)
        self.assertNotIn(bench.HOME, redacted)
        self.assertIn("~", redacted)
        self.assertNotIn("coop-run-123", redacted)
        # A commit or a version is the whole point of retaining the artifact; it must survive.
        self.assertIn("8299aa2cdca2f6c111593a0090575b52b458bf93",
                      bench.redact("commit 8299aa2cdca2f6c111593a0090575b52b458bf93"))

    def test_private_tmp_form(self):
        self.assertNotIn("smoke-repo", bench.redact("/private/tmp/smoke-repo/x"))


class InitializeResultTest(unittest.TestCase):
    def test_accepts_the_answer_to_request_one(self):
        line = '{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1,"agentInfo":{"name":"coop"}}}'
        self.assertEqual(bench.initialize_result(line)["agentInfo"]["name"], "coop")

    def test_rejects_anything_that_is_not_that_answer(self):
        # Substring matching used to accept all of these, and a log line that happens to contain the
        # right characters would have been timed as the agent's answer.
        for line in [
            'log: sending {"id":1,"result":...}',
            '{"jsonrpc":"2.0","method":"session/update","params":{"id":1,"result":true}}',
            '{"jsonrpc":"2.0","id":2,"result":{}}',
            '{"jsonrpc":"2.0","id":1,"error":{"code":-32601}}',
            '{"jsonrpc":"2.0","id":1,"result":"not-an-object"}',
            "", "not json at all",
        ]:
            self.assertIsNone(bench.initialize_result(line), line)


class ReportTest(unittest.TestCase):
    def setUp(self):
        self.args = type("Args", (), {"samples": 2, "cases": "repeat_start", "coop": "/bin/sh"})()
        self.environment = {"coop_version": "v1", "runtime": "docker"}
        self.workspace = {"path": "/tmp/workspace", "tracked_files": "3"}

    def test_failed_samples_are_visible_not_averaged_away(self):
        case = bench.CaseResult(name="repeat_start", description="d", boundary="start")
        case.samples = [bench.Sample("repeat_start", True, seconds=1.0),
                        bench.Sample("repeat_start", False, detail="the box never appeared")]
        report = bench.render_report([case], self.environment, self.args, self.workspace)
        self.assertIn("1/2", report)  # the count says one of two, not "p50 = 1.0s"
        self.assertIn("the box never appeared", report)

    def test_a_case_with_no_successes_reports_no_numbers(self):
        case = bench.CaseResult(name="cancelled_stop", description="d", boundary="stop")
        case.samples = [bench.Sample("cancelled_stop", False, detail="did not exit")]
        report = bench.render_report([case], self.environment, self.args, self.workspace)
        row = next(line for line in report.splitlines() if line.startswith("| cancelled_stop"))
        self.assertIn("0/1", row)
        # A case with nothing to average must show dashes, not a NaN dressed up as a percentile.
        self.assertNotIn("nan", row.lower())
        self.assertEqual(row.count("—"), 4)

    def test_unverified_is_carried_into_the_report(self):
        case = bench.CaseResult(name="cold_start", description="d", boundary="start",
                                unverified="only the first sample is genuinely cold")
        case.samples = [bench.Sample("cold_start", True, seconds=0.5)]
        self.assertIn("unverified: only the first sample is genuinely cold",
                      bench.render_report([case], self.environment, self.args, self.workspace))

    def test_repeat_command_is_present(self):
        case = bench.CaseResult(name="repeat_start", description="d", boundary="start")
        report = bench.render_report([case], self.environment, self.args, self.workspace)
        self.assertIn("tools/lifecycle_bench.py --samples 2 --cases repeat_start", report)
        # The workspace belongs in the repeat line: the same command in a different tree measures
        # a different product.
        self.assertIn("--repo /tmp/workspace", report)


class EnvironmentTest(unittest.TestCase):
    def collect(self, info, version="runtime v1"):
        def fake_run(cmd, timeout):
            if cmd == ["coop", "version"]:
                return subprocess.CompletedProcess(cmd, 0, stdout="coop v1", stderr="")
            if cmd[:3] == ["runtime", "info", "--format"]:
                self.assertEqual(timeout, 60)
                if isinstance(info, Exception):
                    raise info
                return info
            if cmd == ["runtime", "--version"]:
                if isinstance(version, Exception):
                    raise version
                return subprocess.CompletedProcess(cmd, 0, stdout=version, stderr="")
            if cmd == ["sysctl", "-n", "hw.memsize"]:
                return subprocess.CompletedProcess(cmd, 0, stdout=str(8 << 30), stderr="")
            self.fail(f"unexpected metadata command: {cmd}")

        with patch.object(bench, "run", side_effect=fake_run):
            return bench.collect_environment("coop", "runtime")

    def test_missing_runtime_keeps_metadata_available(self):
        environment = self.collect(FileNotFoundError("runtime absent"),
                                   version=FileNotFoundError("runtime absent"))
        self.assertEqual(environment["runtime_daemon"], "unknown")
        self.assertEqual(environment["runtime_version"], "unavailable")
        self.assertEqual(environment["coop_version"], "coop v1")
        self.assertEqual(environment["runtime"], "runtime")

    def test_daemon_timeout_keeps_runtime_version(self):
        environment = self.collect(subprocess.TimeoutExpired("runtime info", 60))
        self.assertEqual(environment["runtime_daemon"], "unknown")
        self.assertEqual(environment["runtime_version"], "runtime v1")

    def test_failed_or_empty_daemon_info_is_unknown(self):
        for code, output in [(1, "daemon failed"), (0, "")]:
            with self.subTest(code=code, output=output):
                environment = self.collect(subprocess.CompletedProcess(
                    [], code, stdout=output, stderr=""))
                self.assertEqual(environment["runtime_daemon"], "unknown")

    def test_successful_daemon_info_retains_redacted_first_line(self):
        environment = self.collect(subprocess.CompletedProcess(
            [], 0, stdout=f"daemon at {bench.HOME}/runtime\nextra line\n", stderr=""))
        self.assertEqual(environment["runtime_daemon"], "daemon at ~/runtime")


class SamplingLoopTest(unittest.TestCase):
    """The loop around the cases: it must record a failure as a failure and still write its output."""

    def setUp(self):
        cases = patch.dict(bench.CASES)
        cases.start()
        self.addCleanup(cases.stop)
        environment = patch.object(bench, "collect_environment", return_value={
            "coop_version": "fixture v1", "runtime": "fixture", "runtime_daemon": "fixture daemon",
        })
        self.environment = environment.start()
        self.addCleanup(environment.stop)
        # main() refuses a runtime with other runs' boxes; these tests need no runtime at all
        quiet = patch.object(bench, "owned", return_value=set())
        quiet.start()
        self.addCleanup(quiet.stop)

    def _run(self, case_function, samples=2):
        import tempfile
        bench.CASES["fake"] = ("a fake case", "start", case_function, [])
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        out = directory.name
        argv = ["lifecycle_bench.py", "--samples", str(samples), "--cases", "fake",
                "--coop", "/bin/sh", "--repo", ".", "--out", out]
        import contextlib
        import io
        import sys
        original_argv, sys.argv = sys.argv, argv
        try:
            # main() reports where it wrote; swallow it so `make tools-test` output stays the
            # test result and not a stray path per case.
            with contextlib.redirect_stdout(io.StringIO()):
                code = bench.main()
        finally:
            sys.argv = original_argv
        return code, Path(out)

    def test_a_timeout_becomes_a_failed_sample_not_a_crash(self):
        def timing_out(*_):
            raise bench.subprocess.TimeoutExpired(cmd="coop", timeout=1)
        code, out = self._run(timing_out)
        self.assertEqual(code, 0)
        report = (out / "report.md").read_text()
        self.assertIn("0/2", report)
        self.assertIn("case exceeded", report)

    def test_output_is_written_and_names_the_workspace(self):
        code, out = self._run(lambda *_: bench.Sample("", True, seconds=0.5))
        self.assertEqual(code, 0)
        samples = json.loads((out / "samples.json").read_text())
        self.assertIn("workspace", samples)
        self.assertIn("tracked_files", samples["workspace"])
        self.assertEqual(samples["environment"], self.environment.return_value)
        self.assertIn("--repo", (out / "report.md").read_text())

    def test_an_interrupted_run_still_writes_what_it_collected(self):
        state = {"calls": 0}

        def interrupt_on_second(*_):
            state["calls"] += 1
            if state["calls"] > 1:
                raise KeyboardInterrupt
            return bench.Sample("", True, seconds=0.25)

        code, out = self._run(interrupt_on_second, samples=5)
        self.assertEqual(code, 1)
        report = (out / "report.md").read_text()
        self.assertIn("1/1", report)  # the one sample it managed
        self.assertIn("interrupted before every case finished", report)


class ProviderSwitchTest(unittest.TestCase):
    OPTIONS = {"configOptions": [
        {"id": "coop_preset", "currentValue": "none", "options": [{"value": "none"}]},
        {"id": "coop_provider", "currentValue": "claude",
         "options": [{"value": "claude"}, {"value": "codex"}, {"value": "gemini"}]},
    ]}

    def test_reads_the_provider_selector(self):
        self.assertEqual(bench.provider_choices(self.OPTIONS), ("claude", ["claude", "codex", "gemini"]))
        self.assertEqual(bench.provider_choices({}), ("", []))

    def test_the_switch_is_done_when_the_session_shows_the_new_provider(self):
        update = {"jsonrpc": "2.0", "method": "session/update", "params": {"sessionId": "S", "update": {
            "sessionUpdate": "config_option_update", "configOptions": [
                {"id": "coop_provider", "currentValue": "codex", "options": [{"value": "codex"}]}]}}}
        self.assertTrue(bench.provider_update(update, "S", "codex"))
        # The same update for another session, or still naming the old provider, is not the switch.
        self.assertFalse(bench.provider_update(update, "other", "codex"))
        self.assertFalse(bench.provider_update(update, "S", "gemini"))
        self.assertFalse(bench.provider_update({"method": "session/update", "params": "S"}, "S", "codex"))

    def test_evidence_comes_from_the_trace_not_the_timing(self):
        trace = "\n".join([
            "12:00:00.001 | spawn box on target=codex preset=",
            "12:00:01.000 | spawn: cold box for gemini@personal",
            "12:00:02.000 | spawn: warm box for codex@work",
            "12:00:02.100 | replay: negotiating codex and restoring 1 session(s) on the restarted box",
            "12:00:02.400 | replay: codex@work is live on codex-acp 0.13.1",
        ])
        self.assertEqual(bench.switch_evidence(trace, "codex"),
                         {"box": "warm", "account": "work", "adapter": "codex-acp 0.13.1", "live_account": "work"})
        self.assertEqual(bench.switch_evidence(trace, "gemini")["box"], "cold")
        self.assertEqual(bench.switch_evidence(trace, "grok"), {})

    def test_pool_readiness_is_per_provider(self):
        trace = "12:00:00.500 | warm pool: gemini@personal parked"
        self.assertTrue(bench.pool_ready(trace, "gemini"))
        self.assertFalse(bench.pool_ready(trace, "codex"))


class FakeRuntime:
    """A runtime that answers `ps`/`volume ls` from a fixed world and records what it was asked.

    Every stop number this tool publishes means "nothing the run owned is left". That claim is only
    as good as what the question covers, so the question itself is what these tests pin.
    """

    def __init__(self, containers=(), volumes=(), labels=None):
        self.containers, self.volumes = list(containers), list(volumes)
        self.labels = labels or {}  # name -> (coop.host, coop.network.run)
        self.commands: list[list[str]] = []

    def __call__(self, cmd, env=None, timeout=None, cwd=None):
        self.commands.append(cmd)
        label = cmd[cmd.index("--filter") + 1] if "--filter" in cmd else ""
        if cmd[1] == "inspect" or cmd[1:3] == ["volume", "inspect"]:
            host, network = self.labels.get(cmd[-1], ("", ""))
            return subprocess.CompletedProcess(cmd, 0, stdout=f"{host}\t{network}\n", stderr="")
        if cmd[1:3] == ["volume", "ls"]:
            names = [n for n, owner in self.volumes if owner == label]
        elif cmd[1] == "ps":
            names = [n for n, owner in self.containers if owner == label]
        else:
            names = []
        return subprocess.CompletedProcess(cmd, 0, stdout="\n".join(names), stderr="")


class OtherRunsTest(unittest.TestCase):
    """The bench removes what its own launch made and nothing else: another run's boxes and volumes
    (a loop in another repository, an editor session) are theirs, data included."""

    def setUp(self):
        self.real_run = bench.run
        self.addCleanup(lambda: setattr(bench, "run", self.real_run))

    def test_cleanup_takes_only_its_own_launch(self):
        bench.run = FakeRuntime(labels={
            "mine": ("v1:ws:4242:t", ""), "theirs": ("v1:ws:999:t", ""),
            "ipc": ("", "r1"), "their-db": ("", "r2"), "unknown": ("", ""),
        })
        leftovers = {"c:mine", "c:theirs", "v:ipc", "v:their-db", "v:unknown"}
        self.assertEqual(bench.launched_by("docker", leftovers, 4242, {"r1"}), {"c:mine", "v:ipc"})

    def test_its_box_claims_its_gateway_without_the_printed_run(self):
        bench.run = FakeRuntime(labels={"mine": ("v1:ws:4242:t", "r1"), "guard": ("", "r1"), "other": ("", "r2")})
        self.assertEqual(bench.launched_by("docker", {"c:mine", "c:guard", "c:other"}, 4242, set()), {"c:mine", "c:guard"})

    def test_a_pid_inside_another_number_is_not_ours(self):
        bench.run = FakeRuntime(labels={"theirs": ("v1:ws:14242:t", "")})
        self.assertEqual(bench.launched_by("docker", {"c:theirs"}, 4242, set()), set())

    def test_it_reads_the_network_run_a_launch_prints(self):
        said = "Networking stats:\n  Allowed   no external connections\nFull details: coop net inspect 61727efd6e10631b31d8ae600cfa3a4d --json\n"
        self.assertEqual(bench.network_runs(said), {"61727efd6e10631b31d8ae600cfa3a4d"})

    def test_a_busy_runtime_is_refused_before_any_launch(self):
        launched = []
        real_owned, real_launch = bench.owned, bench.launch
        self.addCleanup(lambda: (setattr(bench, "owned", real_owned), setattr(bench, "launch", real_launch)))
        bench.owned = lambda runtime: {"c:a-loop-in-another-repo"}
        bench.launch = lambda *args: launched.append(args)
        with tempfile.TemporaryDirectory() as out, patch.object(sys, "argv", ["lifecycle_bench.py", "--out", out, "--coop", "/bin/sh", "--cases", "repeat_start"]), \
                patch("sys.stderr", new_callable=io.StringIO) as err:
            self.assertEqual(bench.main(), 2)
        self.assertEqual(launched, [])
        self.assertIn("from other runs are present", err.getvalue())


class OwnershipTest(unittest.TestCase):
    """A filtered launch owns more than the agent box, and a stop that only counts boxes lies."""

    GATEWAY = "label=coop.network.run"
    BOX = "label=coop=box"

    def setUp(self):
        self.real_run = bench.run
        self.addCleanup(lambda: setattr(bench, "run", self.real_run))

    def use(self, fake):
        bench.run = fake
        return fake

    def test_a_gateway_left_behind_is_not_reported_as_nothing_left(self):
        # The exact failure this tool exists to catch: the agent box is gone, its gateway is not.
        fake = self.use(FakeRuntime(containers=[("guard1", self.GATEWAY)],
                                    volumes=[("ipc1", self.GATEWAY)]))
        self.assertEqual(bench.owned("docker"), {"c:guard1", "v:ipc1"})
        asked = {c[1] if c[1] != "volume" else "volume ls" for c in fake.commands}
        self.assertEqual(asked, {"ps", "volume ls"}, "both containers AND volumes must be asked about")

    def test_ownership_spans_both_label_families(self):
        self.use(FakeRuntime(containers=[("box1", self.BOX), ("ctrl1", self.GATEWAY)],
                             volumes=[("obs1", self.GATEWAY)]))
        self.assertEqual(bench.owned("docker"), {"c:box1", "c:ctrl1", "v:obs1"})

    def test_a_surviving_volume_means_not_gone(self):
        # Containers clear immediately; the volume never does. A stop is not over until both are.
        self.use(FakeRuntime(volumes=[("ipc1", self.GATEWAY)]))
        self.assertFalse(bench.wait_until_gone("docker", set(), 0.2))

    def test_gone_when_nothing_new_remains(self):
        self.use(FakeRuntime())
        self.assertTrue(bench.wait_until_gone("docker", set(), 1.0))

    def test_what_was_already_there_is_not_this_run_s_leftover(self):
        # Another project's stopped containers are not evidence against this run.
        self.use(FakeRuntime(containers=[("someone-elses", self.BOX)]))
        self.assertTrue(bench.wait_until_gone("docker", {"c:someone-elses"}, 1.0))

    def test_cleanup_removes_a_volume_as_a_volume(self):
        fake = self.use(FakeRuntime())
        bench.remove_owned("docker", {"c:box1", "v:ipc1"})
        self.assertIn(["docker", "rm", "-f", "box1"], fake.commands)
        self.assertIn(["docker", "volume", "rm", "-f", "ipc1"], fake.commands)


if __name__ == "__main__":
    unittest.main()
