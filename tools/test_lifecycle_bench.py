"""Tests for the lifecycle benchmark's pure parts.

The cases themselves launch real boxes and are not unit-testable; what IS testable is everything a
wrong answer would travel through — the statistics, the redaction that decides what a retained
artifact says about the machine it ran on, the parse that turns the box's own clock into a number,
and the report that a later comparison reads back.
"""

import contextlib
import io
import json
import os
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
    as good as what the question covers, so the question itself is what these tests pin. `appear`
    joins the world once the file `launched` exists (a stand-in coop creates it), and a removal takes
    a resource out of it. Anything that is not a runtime command (a coop launch) really runs.
    """

    def __init__(self, containers=(), volumes=(), labels=None, appear=((), ()), launched=None):
        self.containers, self.volumes = list(containers), list(volumes)
        self.labels = labels or {}  # name -> (coop.host, coop.network.run[, coop-bench.sample])
        self.appear, self.launched = appear, launched
        self.on_appear = None  # called with the launched file's text when the world appears
        self.commands: list[list[str]] = []
        self.removed: set[str] = set()
        self.real_run = bench.run

    def __call__(self, cmd, env=None, timeout=None, cwd=None):
        if cmd[0] != "docker":
            return self.real_run(cmd, env=env, timeout=timeout, cwd=cwd)
        self.commands.append(cmd)
        if self.launched and os.path.exists(self.launched):
            if self.on_appear:
                self.on_appear(Path(self.launched).read_text())
            self.containers += self.appear[0]
            self.volumes += self.appear[1]
            self.launched = None
        label = cmd[cmd.index("--filter") + 1] if "--filter" in cmd else ""
        if cmd[1] == "inspect" or cmd[1:3] == ["volume", "inspect"]:
            host, network, sample = (tuple(self.labels.get(cmd[-1], ())) + ("", "", ""))[:3]
            return subprocess.CompletedProcess(cmd, 0, stdout=f"{host}\t{network}\t{sample}\n", stderr="")
        if cmd[1] == "rm" or cmd[1:3] == ["volume", "rm"]:
            self.removed.add(cmd[-1])
            self.containers = [c for c in self.containers if c[0] != cmd[-1]]
            self.volumes = [v for v in self.volumes if v[0] != cmd[-1]]
            names = []
        elif cmd[1:3] == ["volume", "ls"]:
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

    def ours(self, pid=4242, heard=""):
        ours = bench.Ownership("docker", "tok", pid)
        ours.heard(heard)
        return ours

    def test_cleanup_takes_only_its_own_launch(self):
        bench.run = FakeRuntime(labels={
            "mine": ("v1:ws:4242:t", ""), "theirs": ("v1:ws:999:t", ""),
            "ipc": ("", "1a"), "their-db": ("", "2b"), "unknown": ("", ""),
        })
        leftovers = {"c:mine", "c:theirs", "v:ipc", "v:their-db", "v:unknown"}
        self.assertEqual(self.ours(heard="coop net inspect 1a").mine(leftovers), {"c:mine", "v:ipc"})

    def test_its_box_claims_its_gateway_without_the_printed_run(self):
        bench.run = FakeRuntime(labels={"mine": ("v1:ws:4242:t", "1a"), "guard": ("", "1a"), "other": ("", "2b")})
        self.assertEqual(self.ours().mine({"c:mine", "c:guard", "c:other"}), {"c:mine", "c:guard"})

    def test_a_pid_inside_another_number_is_not_ours(self):
        bench.run = FakeRuntime(labels={"theirs": ("v1:ws:14242:t", "")})
        self.assertEqual(self.ours().mine({"c:theirs"}), set())

    def test_a_box_with_the_sample_label_is_ours_whichever_process_started_it(self):
        # An editor session's boxes are started by its per-provider coop processes, not the one the
        # bench launched; they carry the sample's label all the same, and claim their gateways.
        bench.run = FakeRuntime(labels={"child-box": ("v1:ws:5151:t", "3c", "tok"), "its-ipc": ("", "3c"),
                                        "other-box": ("v1:ws:6161:t", "4d", "another-sample"), "other-ipc": ("", "4d")})
        self.assertEqual(self.ours().mine({"c:child-box", "v:its-ipc", "c:other-box", "v:other-ipc"}),
                         {"c:child-box", "v:its-ipc"})

    def test_a_box_seen_alive_still_claims_its_gateway_once_it_is_gone(self):
        bench.run = FakeRuntime(labels={"mine": ("v1:ws:4242:t", "5e"), "ipc": ("", "5e")})
        ours = self.ours()
        self.assertEqual(ours.mine({"c:mine"}), {"c:mine"})
        self.assertEqual(ours.mine({"v:ipc"}), {"v:ipc"})

    def test_each_resource_is_inspected_once(self):
        fake = bench.run = FakeRuntime(labels={"mine": ("v1:ws:4242:t", "")})
        ours = self.ours()
        for _ in range(3):
            ours.mine({"c:mine"})
        self.assertEqual(sum(1 for c in fake.commands if c[1] == "inspect"), 1)

    def test_the_sample_label_rides_on_the_person_s_own_run_args(self):
        with patch.dict(os.environ, {"COOP_RUN_ARGS": "-v /cache:/cache"}):
            self.assertEqual(bench.tagged_run_args("tok"), "-v /cache:/cache --label coop-bench.sample=tok")
        with patch.dict(os.environ, {}, clear=True):
            self.assertEqual(bench.tagged_env("tok")["COOP_RUN_ARGS"], "--label coop-bench.sample=tok")

    def test_a_stopped_process_hands_back_what_it_printed_after_a_timeout(self):
        # A sample whose coop outlives its timeout still names its network run on the way out.
        process = subprocess.Popen(["sh", "-c", "echo 'coop net inspect 6f'; sleep 30"], stdout=subprocess.PIPE,
                                   stderr=subprocess.PIPE, text=True, start_new_session=True)
        with self.assertRaises(subprocess.TimeoutExpired):
            process.communicate(timeout=0.3)
        self.assertEqual(bench.network_runs(bench.stop_process(process)), {"6f"})

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


# A stand-in coop: it records the COOP_RUN_ARGS it was given, names its network run the way a
# filtered launch does, and behaves just enough like `coop run` and `coop acp` for each case.
STAND_IN_COOP = """#!/bin/sh
printf '%s' "$COOP_RUN_ARGS" > "@LAUNCHED@"
echo "Full details: coop net inspect 8b --json" >&2
case "$1" in
run)
    while [ $# -gt 0 ] && [ "$1" != -- ]; do shift; done
    shift
    [ -n "$COOP_IMAGE" ] && exit 1
    exec "$@" ;;
acp)
    IFS= read -r line
    [ -n "$COOP_ACP_TRACE" ] && exit 0
    printf '%s\\n' '{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1}}'
    exit 0 ;;
esac
"""


class EveryCaseCleansOnlyItsOwnTest(unittest.TestCase):
    """Whatever the case, cleanup removes what its own launch left (its labelled box, and the gateway
    resources of the network run that box carries or the launch printed) and never another run's,
    even one that started while the sample ran."""

    BOX, GATEWAY = "label=coop=box", "label=coop.network.run"

    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        root = Path(tmp.name)
        self.repo = root / "repo"
        self.repo.mkdir()
        launched = root / "launched"
        self.coop = root / "coop"
        self.coop.write_text(STAND_IN_COOP.replace("@LAUNCHED@", str(launched)))
        self.coop.chmod(0o755)
        self.fake = FakeRuntime(
            appear=([("our-box", self.BOX), ("their-box", self.BOX)],
                    [("our-ipc", self.GATEWAY), ("printed-ipc", self.GATEWAY), ("their-db", self.GATEWAY)]),
            launched=str(launched),
            labels={"our-ipc": ("", "7a"), "printed-ipc": ("", "8b"),
                    "their-box": ("v1:ws:999:t", "9c", "another-sample"), "their-db": ("", "9c")})

        def label_our_box(run_args):
            # the box carries the sample's label only if the launch really passed it on
            sample = run_args.rsplit("coop-bench.sample=", 1)[1] if "coop-bench.sample=" in run_args else ""
            self.fake.labels["our-box"] = ("", "7a", sample)
        self.fake.on_appear = label_our_box
        for name, value in (("run", self.fake), ("sample_token", lambda: "tok"), ("GONE_TIMEOUT_SECONDS", 0.3)):
            patcher = patch.object(bench, name, value)
            patcher.start()
            self.addCleanup(patcher.stop)

    def check(self, case):
        with contextlib.redirect_stderr(io.StringIO()):
            case(str(self.coop), str(self.repo), "docker", [])
        self.assertEqual(self.fake.removed, {"our-box", "our-ipc", "printed-ipc"})

    def test_start(self):
        self.check(bench.case_start)

    def test_stop(self):
        self.check(bench.case_stop)

    def test_failed_start_preflight(self):
        self.check(bench.case_failed_start_preflight)

    def test_failed_start_after_create(self):
        self.check(bench.case_failed_start_after_create)

    def test_cancelled_stop(self):
        self.check(bench.case_cancelled_stop)

    def test_acp_initialize(self):
        self.check(bench.case_acp_initialize)

    def test_acp_switch(self):
        self.check(bench.case_acp_switch_cold)


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

    def ours(self):
        ours = bench.Ownership("docker", "tok")
        ours.heard("coop net inspect 1a")
        return ours

    def test_a_surviving_volume_means_not_gone(self):
        # Containers clear immediately; the volume never does. A stop is not over until both are.
        self.use(FakeRuntime(volumes=[("ipc1", self.GATEWAY)], labels={"ipc1": ("", "1a")}))
        self.assertFalse(bench.wait_until_gone("docker", set(), 0.2, self.ours()))

    def test_gone_when_nothing_new_remains(self):
        self.use(FakeRuntime())
        self.assertTrue(bench.wait_until_gone("docker", set(), 1.0, self.ours()))

    def test_what_was_already_there_is_not_this_run_s_leftover(self):
        # Another project's stopped containers are not evidence against this run.
        self.use(FakeRuntime(containers=[("someone-elses", self.BOX)]))
        self.assertTrue(bench.wait_until_gone("docker", {"c:someone-elses"}, 1.0, self.ours()))

    def test_another_run_starting_meanwhile_is_not_waited_for(self):
        self.use(FakeRuntime(containers=[("their-box", self.BOX)], labels={"their-box": ("v1:ws:999:t", "", "theirs")}))
        self.assertTrue(bench.wait_until_gone("docker", set(), 1.0, self.ours()))

    def test_cleanup_removes_a_volume_as_a_volume(self):
        fake = self.use(FakeRuntime())
        bench.remove_owned("docker", {"c:box1", "v:ipc1"})
        self.assertIn(["docker", "rm", "-f", "box1"], fake.commands)
        self.assertIn(["docker", "volume", "rm", "-f", "ipc1"], fake.commands)


if __name__ == "__main__":
    unittest.main()
