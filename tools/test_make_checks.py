import json
import os
import re
import shutil
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
# The owners CI runs one per runner. `make check` runs them all but check-unit.
CI_OWNERS = ("check-static", "check-unit", "check-process-loop", "check-process-rest",
             "check-race-rest", "check-race-sessionsvc-1", "check-race-sessionsvc-2")
SESSIONSVC = "github.com/AndrewDryga/coop/internal/sessionsvc"
PACKAGES = f"github.com/AndrewDryga/coop\n{SESSIONSVC}\ngithub.com/AndrewDryga/coop/internal/tasks"


def make_variable(name):
    match = re.search(rf"^{name} :?= (.+)$", (ROOT / "Makefile").read_text(), re.M)
    if not match:
        raise AssertionError(f"the Makefile does not set {name}")
    return match.group(1)


def test_names(package, prefix="Test"):
    """Every top-level test function in a package's test files, whatever their build tags."""
    return sorted(name for path in (ROOT / package).glob("*_test.go")
                  for name in re.findall(r"^func (Test\w*)\(\w+ \*testing\.T\)", path.read_text(), re.M)
                  if name.startswith(prefix))


class MakeChecksTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        (self.root / "Makefile").write_bytes((ROOT / "Makefile").read_bytes())
        graph = self.root / "internal/box"
        graph.mkdir(parents=True)
        (graph / "staticcheck.mod").write_bytes((ROOT / "internal/box/staticcheck.mod").read_bytes())
        self.bin = self.root / "bin"
        self.bin.mkdir()
        self.log = self.root / "commands.jsonl"
        (self.root / "tmp").mkdir()
        versions = dict(line.strip().split()[:2] for line in (graph / "staticcheck.mod").read_text().splitlines()
                        if line.strip().startswith(("honnef.co/go/tools ", "golang.org/x/tools ")))
        self.env = {
            "PATH": str(self.bin) + os.pathsep + "/usr/bin:/bin",
            "HOME": str(self.root),
            "GIT_CONFIG_GLOBAL": "/dev/null",
            "GIT_CONFIG_SYSTEM": "/dev/null",
            "COMMAND_LOG": str(self.log),
            "TMPDIR": str(self.root / "tmp"),
            "STUB_TMP_PATH": str(self.root / "tmp/tagged-test"),
            "STATICCHECK_PIN": versions["honnef.co/go/tools"].removeprefix("v"),
            "XTOOLS_PIN": versions["golang.org/x/tools"],
        }
        # Every executable recipe dependency is inert and recorded. Real Make selects and
        # executes the repository recipes; no compiler, model, Docker or host config is used.
        stub = "#!" + sys.executable + "\n" + (
            "import json, os, sys\n"
            "from pathlib import Path\n"
            "with open(os.environ['COMMAND_LOG'], 'a') as log:\n"
            "    log.write(json.dumps([Path(sys.argv[0]).name, *sys.argv[1:]]) + '\\n')\n"
            "if Path(sys.argv[0]).name == 'git': print('fixture-version')\n"
            "if Path(sys.argv[0]).name == 'gofmt' and os.environ.get('STUB_FMT_OUTPUT'): print('unformatted.go')\n"
            "if Path(sys.argv[0]).name == 'staticcheck' and sys.argv[1:] == ['-version']: print('staticcheck (' + os.environ['STATICCHECK_PIN'] + ')')\n"
            "if Path(sys.argv[0]).name == 'staticcheck' and sys.argv[1:] == ['-debug.version']: print('golang.org/x/tools@' + os.environ['XTOOLS_PIN'])\n"
            "if Path(sys.argv[0]).name == 'govulncheck' and sys.argv[1:] == ['-version']: print('Scanner: govulncheck@v1.8.0')\n"
            "if Path(sys.argv[0]).name == 'go':\n"
            "    with open(os.environ['COMMAND_LOG'] + '.gocache', 'a') as log:\n"
            "        log.write(os.environ.get('GOCACHE', '') + '\\n')\n"
            "    if sys.argv[1:] == ['list', './...']: print(os.environ['STUB_GO_LIST'])\n"
            "if Path(sys.argv[0]).name == 'mktemp':\n"
            "    if sys.argv[1:]: sys.exit('unexpected mktemp arguments')\n"
            "    Path(os.environ['STUB_TMP_PATH']).touch()\n"
            "    print(os.environ['STUB_TMP_PATH'])\n"
            "fail, arg = os.environ.get('STUB_FAIL_COMMAND'), os.environ.get('STUB_FAIL_ARG')\n"
            "if arg: sys.exit(int(os.environ.get('STUB_EXIT', '0')) if arg in sys.argv[1:] else 0)\n"
            "sys.exit(int(os.environ.get('STUB_EXIT', '0')) if not fail or fail == Path(sys.argv[0]).name else 0)\n"
        )
        for name in ("go", "gofmt", "python3", "node", "git", "git-lfs", "docker", "staticcheck", "govulncheck", "shellcheck", "mktemp"):
            path = self.bin / name
            path.write_text(stub)
            path.chmod(0o700)

    def make(self, *args, dry=False, exit_code=0, fail_command="", fail_arg="", unformatted=False,
             packages=PACKAGES):
        env = dict(self.env, STUB_EXIT=str(exit_code), STUB_FAIL_COMMAND=fail_command,
                   STUB_FAIL_ARG=fail_arg, STUB_FMT_OUTPUT="1" if unformatted else "",
                   STUB_GO_LIST=packages)
        return subprocess.run(
            [shutil.which("make"), *(["-n"] if dry else []), *args],
            cwd=self.root, env=env, text=True, capture_output=True, timeout=10,
        )

    def commands(self):
        return [json.loads(line) for line in self.log.read_text().splitlines()]

    def test_site_owner_requires_node_instead_of_skipping_script_behavior(self):
        (self.bin / "node").unlink()
        env = dict(self.env, PATH=str(self.bin))
        run = subprocess.run(
            [shutil.which("make"), "docs-tools-test", "VERSION=fixture", "STATICCHECK_VERSION=fixture", "STATICCHECK_XTOOLS_VERSION=fixture"],
            cwd=self.root, env=env, text=True, capture_output=True, timeout=10,
        )
        self.assertNotEqual(run.returncode, 0)
        self.assertIn("Node.js is not installed", run.stdout)
        commands = self.commands() if self.log.exists() else []
        self.assertFalse(any(cmd[:2] == ["python3", "-m"] for cmd in commands))

    def test_docs_execute_only_the_actual_docs_and_rules_owners(self):
        run = self.make("check-local", "SCOPE=docs")
        self.assertEqual(run.returncode, 0, run.stderr)
        commands = [cmd for cmd in self.commands() if cmd[0] != "git"]
        self.assertEqual(commands, [
            ["go", "run", "./tools/gendocs", "-check"],
            ["python3", "tools/align-comments.py", "--check"],
            ["python3", "tools/check_rules.py"],
            ["go", "test", "-count=1", "./internal/agent", "-run", "TestProviderDecisionsStayInAdapters|TestProviderDecisionGuard"],
            *[["python3", "-m", "unittest", "discover", "-s", "tools", "-p", name]
              for name in ("test_site_content.py", "test_align_comments.py", "test_check_rules.py", "test_make_checks.py")],
        ])
        self.assertIn("Local checks: docs", run.stdout)

    def test_rules_do_not_load_docs_or_process_suites(self):
        run = self.make("check-local", "SCOPE=rules")
        self.assertEqual(run.returncode, 0, run.stderr)
        self.assertEqual([cmd for cmd in self.commands() if cmd[0] != "git"], [
            ["python3", "tools/check_rules.py"],
            ["go", "test", "-count=1", "./internal/agent", "-run", "TestProviderDecisionsStayInAdapters|TestProviderDecisionGuard"],
        ])

    def test_each_go_scope_runs_nonempty_owner_tests_without_a_live_suite(self):
        for scope, owner in (
            ("agent", "./internal/agent"), ("runtime", "./internal/runtime"),
            ("sessions", "./internal/session"), ("tasks", "./internal/tasks"),
            ("connector", "./internal/workerconnector"), ("cli", "./internal/cli"),
        ):
            with self.subTest(scope=scope):
                self.log.unlink(missing_ok=True)
                run = self.make("check-local", "SCOPE=" + scope)
                self.assertEqual(run.returncode, 0, run.stderr)
                commands = [cmd for cmd in self.commands() if cmd[0] != "git"]
                tests = [cmd for cmd in commands if cmd[:2] == ["go", "test"]]
                self.assertEqual(len(tests), 1)
                self.assertEqual(tests[0][2:7], ["-count=1", "-p", "4", "-timeout", "20m"])
                self.assertIn(owner, tests[0][7:])
                self.assertNotIn("-race", tests[0])
                self.assertNotIn("-tags", tests[0])
                self.assertFalse(any(cmd[0] in ("docker", "python3") for cmd in commands))
                vets = [cmd for cmd in commands if cmd[:2] == ["go", "vet"]]
                self.assertEqual(len(vets), 4)  # both platforms, ordinary and all-tag compilation

    def test_missing_unknown_wildcard_or_multiword_scope_takes_the_full_gate(self):
        # The gate's test owners run side by side, so the dry run lists them in any order.
        broad = self.make("check", dry=True)
        self.assertEqual(broad.returncode, 0, broad.stderr)
        for scope in ("", "unknown", "%", "ag%", "agent runtime", "docs cli"):
            with self.subTest(scope=scope):
                run = self.make("check-local", "SCOPE=" + scope, dry=True)
                self.assertEqual(run.returncode, 0, run.stderr)
                lines = run.stdout.splitlines()
                self.assertEqual(sorted(line for line in lines if "full free gate" not in line),
                                 sorted(broad.stdout.splitlines()), run.stdout)
                self.assertIn("full free gate", lines[-1])
        missing = self.make("check-local", dry=True)
        self.assertEqual(sorted(missing.stdout.splitlines()[:-1]), sorted(broad.stdout.splitlines()),
                         missing.stdout)

    def test_failed_owner_check_is_not_a_successful_local_check(self):
        run = self.make("check-local", "SCOPE=docs", exit_code=17)
        self.assertNotEqual(run.returncode, 0)
        self.assertNotIn("Local checks: docs", run.stdout)
        self.assertEqual([cmd for cmd in self.commands() if cmd[0] != "git"],
                         [["go", "run", "./tools/gendocs", "-check"]])

    def test_free_partition_union_executes_all_owners_and_every_failure_propagates(self):
        sessions, scripted = make_variable("SESSIONSVC_SHARD"), make_variable("PROVIDER_SHARD")
        owned = {}
        for target in CI_OWNERS:
            self.log.unlink(missing_ok=True)
            (self.root / target).touch()  # A same-named file must not skip the owner.
            run = self.make(target)
            self.assertEqual(run.returncode, 0, run.stdout + run.stderr)
            commands = [cmd for cmd in self.commands() if cmd[0] != "git"]
            owned[target] = commands
            tests = [cmd for cmd in commands if cmd[:2] == ["go", "test"]]
            if target == "check-unit":
                self.assertEqual(tests, [["go", "test", "-count=1", "-p", "4", "-timeout", "20m", "./..."]])
            elif target == "check-race-rest":
                self.assertEqual(commands, [
                    ["go", "list", "./..."],
                    ["go", "test", "-count=1", "-race", "-p", "4", "-timeout", "30m",
                     "github.com/AndrewDryga/coop", "github.com/AndrewDryga/coop/internal/tasks"],
                ])
            elif target == "check-race-sessionsvc-1":
                self.assertEqual(commands, [["go", "test", "-count=1", "-race", "-timeout", "30m",
                                             "-run", sessions, "./internal/sessionsvc"]])
            elif target == "check-race-sessionsvc-2":
                self.assertEqual(commands, [["go", "test", "-count=1", "-race", "-timeout", "30m",
                                             "-skip", sessions, "./internal/sessionsvc"]])
            elif target == "check-static":
                self.assertEqual(commands[:2], [
                    ["go", "mod", "tidy", "-diff"],
                    ["go", "test", "./internal/gatewayimage", "-run",
                     "^TestEmbeddedGatewaySourcesExactlyMatchCheckout$", "-count=1"],
                ])
                for expected in (
                    ["go", "mod", "tidy", "-diff"], ["gofmt", "-l", "."],
                    ["go", "build", "./..."], ["govulncheck", "./..."],
                    ["python3", "tools/align-comments.py", "--check"],
                    ["go", "run", "./tools/gendocs", "-check"],
                    ["python3", "-m", "unittest", "discover", "-s", "tools", "-p", "test_*.py"],
                    ["python3", "tools/check_rules.py"],
                ):
                    self.assertIn(expected, commands)
                self.assertEqual(sum(cmd[0] == "shellcheck" for cmd in commands), 1)
                self.assertEqual(commands.count(["go", "vet", "./..."]), 2)
                self.assertEqual(commands.count(["go", "vet", "-tags",
                    "acpe2e,boxruntimee2e,cooplivetest,networkruntimee2e,providere2e,providerlivee2e,reviewwritee2e", "./..."]), 2)
                self.assertEqual(commands.count(["staticcheck", "./..."]), 2)
                self.assertEqual(len(tests), 2)  # gateway freshness plus provider/rules guard
            elif target == "check-process-loop":
                self.assertEqual(commands, [["go", "test", "-tags", "providere2e", "-run", scripted,
                                             "-count=1", "-timeout", "20m", "-v", "./internal/cli/"]])
            else:
                self.assertEqual(len(tests), 4)  # fixtures, other scripted consumers, ownership race, tagged compile
                self.assertIn(["go", "test", "-count=1", "./internal/testutil/procharness",
                               "./internal/cli/testdata/providerfixture"], tests)
                self.assertIn(["go", "test", "-tags", "providere2e", "-run", "^TestProviderScripted", "-skip", scripted,
                               "-count=1", "-timeout", "20m", "-v", "./internal/cli/"], tests)
                self.assertTrue(any("-race" in cmd and "providerlivee2e,cooplivetest" in cmd and "-count=1" in cmd for cmd in tests))
                self.assertTrue(any("-c" in cmd and "acpe2e" in cmd for cmd in tests))
            self.assertFalse(any(cmd[0] == "docker" for cmd in commands))
            self.log.unlink(missing_ok=True)
            failed = self.make(target, exit_code=17, fail_command="go")
            self.assertNotEqual(failed.returncode, 0, target)
            if target == "check-static":
                self.assertEqual([cmd for cmd in self.commands() if cmd[0] != "git"],
                                 [["go", "mod", "tidy", "-diff"]])
        # Locally the static owner goes first, then every other owner but check-unit, whose tests
        # the race owners run; they run side by side, so in any order.
        self.log.unlink(missing_ok=True)
        run = self.make("check")
        self.assertEqual(run.returncode, 0, run.stdout + run.stderr)
        local = [cmd for cmd in self.commands() if cmd[0] != "git"]
        self.assertEqual(local[:len(owned["check-static"])], owned["check-static"])
        self.assertEqual(sorted(map(json.dumps, local)),
                         sorted(json.dumps(cmd) for target in CI_OWNERS if target != "check-unit"
                                for cmd in owned[target]))
        for target in CI_OWNERS[2:]:
            self.assertIn("ok   " + target, run.stdout.splitlines())
            self.assertIn("elapsed", (self.root / ".agent/gate" / (target + ".log")).read_text())
        # A failing owner fails the gate and shows its log.
        self.log.unlink(missing_ok=True)
        failed = self.make("check", exit_code=17, fail_arg="-race")
        self.assertNotEqual(failed.returncode, 0)
        self.assertIn("FAIL check-race-sessionsvc-1 (full log: .agent/gate/check-race-sessionsvc-1.log)", failed.stdout)

    def test_local_gate_builds_with_its_own_cache(self):
        # One gate died when another session cleared the shared Go build cache under it.
        run = self.make("check")
        self.assertEqual(run.returncode, 0, run.stdout + run.stderr)
        caches = set((self.root / "commands.jsonl.gocache").read_text().splitlines())
        self.assertEqual(caches, {str(self.root / ".cache/coop-gate/go-build")})
        (self.root / "commands.jsonl.gocache").unlink()
        run = self.make("check-race-sessionsvc-1")
        self.assertEqual(run.returncode, 0, run.stdout + run.stderr)
        self.assertEqual((self.root / "commands.jsonl.gocache").read_text().splitlines(), [""])

    def test_race_rest_refuses_a_package_list_without_sessionsvc(self):
        for packages in ("github.com/AndrewDryga/coop\ngithub.com/AndrewDryga/coop/internal/tasks", SESSIONSVC):
            with self.subTest(packages=packages):
                self.log.unlink(missing_ok=True)
                run = self.make("check-race-rest", packages=packages)
                self.assertNotEqual(run.returncode, 0)
                self.assertIn("go list must name", run.stdout)
                self.assertEqual([cmd for cmd in self.commands() if cmd[0] != "git"], [["go", "list", "./..."]])

    def test_ci_owners_are_the_local_gate_and_check_unit(self):
        self.assertEqual(tuple(make_variable("CI_OWNERS").split()), CI_OWNERS)
        self.assertEqual(sorted(["check-static", "check-unit", *make_variable("GATE_TEST_OWNERS").split()]),
                         sorted(CI_OWNERS))

    def test_race_owners_run_every_test_the_unit_owner_runs(self):
        # `make check` leaves check-unit to CI because the race owners run the same packages and
        # tests. A file built only with or only without -race would break that.
        files = subprocess.run(["git", "ls-files", "*.go"], cwd=ROOT, text=True, capture_output=True,
                               check=True).stdout.split()
        self.assertTrue(files)
        constrained = [name for name in files
                       if re.search(r"^(//go:build|// \+build) .*\brace\b", (ROOT / name).read_text(), re.M)]
        self.assertEqual(constrained, [])

    def test_each_shard_pattern_splits_its_tests_between_both_shards(self):
        # -run and -skip take one pattern, so no test can fall outside both shards; a pattern that
        # matched every test or none would leave one shard empty and the other the whole suite.
        for variable, package, prefix in (("SESSIONSVC_SHARD", "internal/sessionsvc", "Test"),
                                          ("PROVIDER_SHARD", "internal/cli", "TestProviderScripted")):
            with self.subTest(variable=variable):
                pattern, names = make_variable(variable), test_names(package, prefix)
                matched = [name for name in names if re.search(pattern, name)]
                self.assertTrue(names)
                self.assertTrue(0 < len(matched) < len(names), (pattern, len(matched), len(names)))

    def test_metadata_owner_executes_docs_and_artifact_checks_without_process_or_race(self):
        run = self.make("check-metadata")
        self.assertEqual(run.returncode, 0, run.stderr)
        commands = [cmd for cmd in self.commands() if cmd[0] != "git"]
        self.assertIn(["go", "test", "-count=1", ".", "-run",
                       "^Test(DocsArtifactDetector|NoDocArtifactsInTrackedMarkdown)$"], commands)
        self.assertFalse(any("-race" in cmd or "-tags" in cmd or cmd[0] == "docker" for cmd in commands))
        self.assertFalse(any(cmd[:2] == ["go", "test"] and "./..." in cmd for cmd in commands))

    def test_formatting_checks_preserve_failure_and_unformatted_output(self):
        for target in (("check-local", "SCOPE=cli"), ("lint",)):
            for broken in (True, False):
                with self.subTest(target=target, broken=broken):
                    self.log.unlink(missing_ok=True)
                    run = self.make(*target, exit_code=17 if broken else 0,
                                    fail_command="gofmt", unformatted=not broken)
                    self.assertNotEqual(run.returncode, 0)
                    self.assertEqual([cmd for cmd in self.commands() if cmd[0] != "git"],
                                     [["gofmt", "-l", "."]])
                    if not broken:
                        self.assertIn("unformatted.go", run.stdout)

    def test_explicit_provider_recipe_owns_one_log_directory_and_stops_on_failure(self):
        suites = [
            "native-roles-e2e", "skills-e2e", "mcp-e2e",
            "provider-live-e2e-all", "provider-resume-live-e2e-all",
            "provider-loop-live-e2e-all", "provider-consult-live-e2e-all",
            "provider-delegate-live-e2e-all", "provider-network-live-e2e-all",
            "provider-accounts-live-e2e-all", "acp-e2e",
        ]
        logs = self.root / "qualification-logs"
        env = dict(self.env, QUALIFY_LOGS=str(logs))
        # Execute the real recipe, but refuse every command outside its explicit protocol.
        stub = "#!" + sys.executable + "\n" + """
import json, os, sys
from pathlib import Path
name, args = Path(sys.argv[0]).name, sys.argv[1:]
with open(os.environ['COMMAND_LOG'], 'a') as log:
    log.write(json.dumps([name, *args]) + '\\n')
logs = Path(os.environ['QUALIFY_LOGS'])
if name == 'mktemp' and args == ['-d']:
    logs.mkdir()
    print(logs)
elif name == 'go' and args == ['run', './tools/qualify', '-preflight']:
    pass
elif name == 'go' and args == ['build', '-o', str(logs / 'coop'), '.']:
    (logs / 'coop').write_bytes(Path(sys.argv[0]).read_bytes())
    (logs / 'coop').chmod(0o700)
elif name == 'coop' and args in [['build'], ['net', 'setup']]:
    print('fixture setup')
elif name == 'submake' and len(args) == 2 and args[0] == '--no-print-directory':
    expected = 'claude:test/high,codex:test/xhigh' if (logs / 'targets-requested').exists() else 'all'
    if os.environ.get('COOP_LIVE_TARGETS') != expected:
        sys.exit('wrong target environment')
    print('fixture suite: ' + args[1])
    if args[1] == os.environ.get('FAIL_SUITE'):
        sys.exit(17)
elif name == 'go' and args == ['run', './tools/qualify', '-targets']:
    (logs / 'targets-requested').touch()
    print('claude:test/high,codex:test/xhigh')
elif name == 'go' and args == ['run', './tools/qualify', '-logs', str(logs)]:
    print('fixture recorded')
elif name == 'git' and args == ['init', '-q']:
    pass
elif name == 'git' and args == ['describe', '--tags', '--always', '--dirty']:
    print('fixture-version')
else:
    sys.exit('unexpected qualification command: ' + repr([name, *args]))
"""
        for name in ("go", "submake", "mktemp", "git"):
            path = self.bin / name
            path.write_text(stub)
            path.chmod(0o700)
        for failed in ("", "skills-e2e"):
            with self.subTest(failed=failed):
                if logs.exists():
                    shutil.rmtree(logs)
                self.log.unlink(missing_ok=True)
                run = subprocess.run(
                    [shutil.which("make"), "provider-qualify", "MAKE=" + str(self.bin / "submake"),
                     "VERSION=fixture", "STATICCHECK_VERSION=fixture", "STATICCHECK_XTOOLS_VERSION=fixture"],
                    cwd=self.root, env=dict(env, FAIL_SUITE=failed), text=True,
                    capture_output=True, timeout=10,
                )
                commands = self.commands()
                prefix = [
                    ["git", "describe", "--tags", "--always", "--dirty"],
                    ["go", "run", "./tools/qualify", "-preflight"],
                    ["mktemp", "-d"], ["go", "build", "-o", str(logs / "coop"), "."],
                    ["git", "init", "-q"], ["coop", "build"], ["coop", "net", "setup"],
                ]
                attempted = suites[:2] if failed else suites
                expected = prefix + [["submake", "--no-print-directory", suite] for suite in attempted]
                if not failed:
                    expected += [
                        ["go", "run", "./tools/qualify", "-targets"],
                        ["submake", "--no-print-directory", "provider-live-e2e-all"],
                        ["go", "run", "./tools/qualify", "-logs", str(logs)],
                    ]
                    self.assertEqual(run.returncode, 0, run.stderr + run.stdout)
                    self.assertIn("fixture recorded", run.stdout)
                    self.assertEqual((logs / "provider-live-e2e-effort.log").read_text(),
                                     "fixture suite: provider-live-e2e-all\n")
                else:
                    self.assertNotEqual(run.returncode, 0)
                    self.assertNotIn("fixture recorded", run.stdout)
                self.assertEqual(commands, expected)
                self.assertIn("logs: " + str(logs), run.stdout)
                self.assertEqual((logs / "setup.log").read_text(),
                                 "fixture setup\nfixture setup\n")
                for suite in attempted:
                    self.assertEqual((logs / (suite + ".log")).read_text(),
                                     "fixture suite: " + suite + "\n")


if __name__ == "__main__":
    unittest.main()
