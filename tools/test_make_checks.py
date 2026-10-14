import json
import os
import shutil
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]


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
        self.env = {
            "PATH": str(self.bin) + os.pathsep + "/usr/bin:/bin",
            "HOME": str(self.root),
            "GIT_CONFIG_GLOBAL": "/dev/null",
            "GIT_CONFIG_SYSTEM": "/dev/null",
            "COMMAND_LOG": str(self.log),
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
            "fail = os.environ.get('STUB_FAIL_COMMAND')\n"
            "sys.exit(int(os.environ.get('STUB_EXIT', '0')) if not fail or fail == Path(sys.argv[0]).name else 0)\n"
        )
        for name in ("go", "gofmt", "python3", "git", "git-lfs", "docker"):
            path = self.bin / name
            path.write_text(stub)
            path.chmod(0o700)

    def make(self, *args, dry=False, exit_code=0, fail_command="", unformatted=False):
        env = dict(self.env, STUB_EXIT=str(exit_code), STUB_FAIL_COMMAND=fail_command,
                   STUB_FMT_OUTPUT="1" if unformatted else "")
        return subprocess.run(
            [shutil.which("make"), *(["-n"] if dry else []), *args],
            cwd=self.root, env=env, text=True, capture_output=True, timeout=10,
        )

    def commands(self):
        return [json.loads(line) for line in self.log.read_text().splitlines()]

    def test_docs_execute_only_the_actual_docs_and_rules_owners(self):
        run = self.make("check-local", "SCOPE=docs")
        self.assertEqual(run.returncode, 0, run.stderr)
        commands = [cmd for cmd in self.commands() if cmd[0] != "git"]
        self.assertEqual(commands, [
            ["go", "run", "./tools/gendocs", "-check"],
            ["python3", "tools/align-comments.py", "--check"],
            ["python3", "tools/check_rules.py"],
            ["go", "test", "./internal/agent", "-run", "TestProviderDecisionsStayInAdapters|TestProviderDecisionGuard"],
            *[["python3", "-m", "unittest", "discover", "-s", "tools", "-p", name]
              for name in ("test_site_content.py", "test_align_comments.py", "test_check_rules.py", "test_make_checks.py")],
        ])
        self.assertIn("Local checks: docs", run.stdout)

    def test_rules_do_not_load_docs_or_process_suites(self):
        run = self.make("check-local", "SCOPE=rules")
        self.assertEqual(run.returncode, 0, run.stderr)
        self.assertEqual([cmd for cmd in self.commands() if cmd[0] != "git"], [
            ["python3", "tools/check_rules.py"],
            ["go", "test", "./internal/agent", "-run", "TestProviderDecisionsStayInAdapters|TestProviderDecisionGuard"],
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
                self.assertEqual(tests[0][2:6], ["-p", "4", "-timeout", "20m"])
                self.assertIn(owner, tests[0][6:])
                self.assertNotIn("-race", tests[0])
                self.assertNotIn("-tags", tests[0])
                self.assertFalse(any(cmd[0] in ("docker", "python3") for cmd in commands))
                vets = [cmd for cmd in commands if cmd[:2] == ["go", "vet"]]
                self.assertEqual(len(vets), 4)  # both platforms, ordinary and all-tag compilation

    def test_missing_unknown_wildcard_or_multiword_scope_takes_the_full_gate(self):
        broad = self.make("check", dry=True)
        self.assertEqual(broad.returncode, 0, broad.stderr)
        for scope in ("", "unknown", "%", "ag%", "agent runtime", "docs cli"):
            with self.subTest(scope=scope):
                run = self.make("check-local", "SCOPE=" + scope, dry=True)
                self.assertEqual(run.returncode, 0, run.stderr)
                self.assertTrue(run.stdout.startswith(broad.stdout), run.stdout)
                self.assertIn("full free gate", run.stdout[len(broad.stdout):])
        missing = self.make("check-local", dry=True)
        self.assertTrue(missing.stdout.startswith(broad.stdout), missing.stdout)

    def test_failed_owner_check_is_not_a_successful_local_check(self):
        run = self.make("check-local", "SCOPE=docs", exit_code=17)
        self.assertNotEqual(run.returncode, 0)
        self.assertNotIn("Local checks: docs", run.stdout)
        self.assertEqual([cmd for cmd in self.commands() if cmd[0] != "git"],
                         [["go", "run", "./tools/gendocs", "-check"]])

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


if __name__ == "__main__":
    unittest.main()
