import copy
import importlib.util
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[1]
SPEC = importlib.util.spec_from_file_location("ci_scope", ROOT / "tools/ci_scope.py")
scope = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(scope)


class CIScopeTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.repo = Path(self.tmp.name)
        self.git = shutil.which("git")
        self.env = {
            "PATH": os.path.dirname(self.git) + os.pathsep + "/usr/bin:/bin",
            "HOME": str(self.repo), "GIT_CONFIG_GLOBAL": os.devnull,
            "GIT_CONFIG_SYSTEM": os.devnull, "GIT_CONFIG_COUNT": "0",
            "GIT_AUTHOR_NAME": "Fixture", "GIT_AUTHOR_EMAIL": "fixture@example.test",
            "GIT_COMMITTER_NAME": "Fixture", "GIT_COMMITTER_EMAIL": "fixture@example.test",
        }
        self.run_git("init", "-q")
        (self.repo / "CHANGELOG.md").write_text("## Unreleased\nFirst.\n")
        self.run_git("add", ".")
        self.run_git("commit", "-qm", "base")
        self.before = self.run_git("rev-parse", "HEAD").strip()
        (self.repo / "CHANGELOG.md").write_text("## 1.2.3\nFinal.\n")
        self.commit()

    def run_git(self, *args):
        return subprocess.run([self.git, *args], cwd=self.repo, env=self.env,
                              check=True, capture_output=True, text=True, timeout=10).stdout

    def commit(self):
        self.run_git("add", "-A")
        self.run_git("commit", "-qm", "change")
        self.after = self.run_git("rev-parse", "HEAD").strip()
        self.context = {
            "CI_EVENT_NAME": "push", "CI_REF": "refs/heads/main",
            "CI_REPOSITORY": "AndrewDryga/coop", "CI_SHA": self.after,
            "CI_WORKFLOW_REF": "AndrewDryga/coop/.github/workflows/ci.yml@refs/heads/main",
        }
        self.event = {
            "before": self.before, "after": self.after, "ref": "refs/heads/main",
            "repository": {"full_name": "AndrewDryga/coop"},
            "created": False, "deleted": False, "forced": False,
        }

    def classify(self, event=None, context=None, read_git=None):
        original = Path.cwd()
        try:
            os.chdir(self.repo)
            return scope.classify(self.event if event is None else event,
                                  self.context if context is None else context,
                                  read_git or scope.git)
        finally:
            os.chdir(original)

    def test_finalize_and_reopen_one_regular_changelog_are_metadata(self):
        self.assertEqual(self.classify(), "metadata")
        self.before = self.after
        (self.repo / "CHANGELOG.md").write_text("## Unreleased\nNext.\n")
        self.commit()
        self.assertEqual(self.classify(), "metadata")

    def test_unknown_context_payload_and_non_main_calls_are_broad(self):
        for key, values in {
            "CI_EVENT_NAME": ("pull_request", "workflow_call", "", "workflow_dispatch"),
            "CI_REF": ("refs/tags/v1.2.3", "refs/heads/feature", ""),
            "CI_WORKFLOW_REF": ("AndrewDryga/coop/.github/workflows/release.yml@refs/heads/main", ""),
            "CI_REPOSITORY": ("other/repo", ""),
            "CI_SHA": ("0" * 40, "f" * 40, ""),
        }.items():
            for value in values:
                with self.subTest(key=key, value=value):
                    self.assertEqual(self.classify(context=dict(self.context, **{key: value})), "broad")
        for key in ("created", "deleted", "forced"):
            for value in (True, None, 0, "false"):
                self.assertEqual(self.classify(event=dict(self.event, **{key: value})), "broad")
            event = dict(self.event)
            del event[key]
            self.assertEqual(self.classify(event=event), "broad")
        for key in ("before", "after"):
            for value in ("", "0" * 40, "bad", None, "f" * 64, "\ud800"):
                self.assertEqual(self.classify(event=dict(self.event, **{key: value})), "broad")
        for event in ({}, None, [], {"repository": "bad"}):
            # None is deliberately tested directly, since the convenience helper defaults it.
            self.assertEqual(scope.classify(event, self.context), "broad")

    def test_other_paths_types_and_modes_are_never_metadata(self):
        for path, kind in (
            ("README.md", "file"), ("internal/fixture.go", "file"),
            ("go.mod", "file"), (".tool-versions", "file"),
            (".github/workflows/ci.yml", "file"), (".agent/skills/work/SKILL.md", "file"),
            ("archive.tar.gz", "file"), ("spaced name.md", "file"),
            ("tab\tname.md", "file"), ("line\nname.md", "file"),
            ("CHANGELOG.md", "executable"), ("CHANGELOG.md", "symlink"),
            ("CHANGELOG.md", "deleted"), ("renamed.md", "rename"),
        ):
            with self.subTest(path=path, kind=kind):
                self.run_git("reset", "--hard", self.after)
                selected = self.repo / path
                selected.parent.mkdir(parents=True, exist_ok=True)
                if kind == "deleted":
                    selected.unlink()
                elif kind == "symlink":
                    selected.unlink()
                    selected.symlink_to("README.md")
                elif kind == "executable":
                    selected.chmod(0o755)
                elif kind == "rename":
                    (self.repo / "CHANGELOG.md").rename(selected)
                else:
                    selected.write_text("not metadata\n")
                self.run_git("add", "-A")
                self.run_git("commit", "-qm", "unsafe kind")
                changed = self.run_git("rev-parse", "HEAD").strip()
                self.assertEqual(self.classify(
                    event=dict(self.event, before=self.after, after=changed),
                    context=dict(self.context, CI_SHA=changed)), "broad")

    def test_multi_commit_push_and_reverted_executable_change_stay_broad(self):
        (self.repo / "hidden.go").write_text("package hidden\n")
        self.commit()
        (self.repo / "hidden.go").unlink()
        (self.repo / "CHANGELOG.md").write_text("## 1.2.3\nChanged again.\n")
        self.commit()
        self.assertEqual(self.classify(), "broad")
        # Even a forged traversal view cannot turn the actual multi-step ancestry into one step.
        (self.repo / ".git/info/grafts").write_text(self.after + " " + self.before + "\n")
        self.assertEqual(self.classify(), "broad")

    def test_missing_objects_shallow_and_git_failures_go_broad(self):
        self.run_git("checkout", "--detach", self.before)
        self.assertEqual(self.classify(), "broad")
        self.run_git("checkout", "--detach", self.after)
        self.assertEqual(self.classify(event=dict(self.event, before="f" * 40)), "broad")
        (self.repo / ".git/shallow").write_text(self.after + "\n")
        self.assertEqual(self.classify(), "broad")
        def failing(*args):
            raise subprocess.CalledProcessError(1, ["git", *args])
        for error in (OSError("missing git"), subprocess.TimeoutExpired("git", 10), ValueError("oversize")):
            def unavailable(*args, error=error):
                raise error
            self.assertEqual(self.classify(read_git=unavailable), "broad")
        self.assertEqual(self.classify(read_git=failing), "broad")

    def test_merge_replacement_and_gitlink_cannot_hide_changed_inputs(self):
        base = self.after
        branch = self.run_git("symbolic-ref", "--short", "HEAD").strip()
        self.run_git("checkout", "-qb", "side", self.before)
        self.run_git("commit", "--allow-empty", "-qm", "side")
        self.run_git("checkout", "-q", branch)
        self.run_git("merge", "--no-ff", "side", "-m", "merge")
        merged = self.run_git("rev-parse", "HEAD").strip()
        self.assertEqual(self.classify(event=dict(self.event, before=base, after=merged),
                                       context=dict(self.context, CI_SHA=merged)), "broad")
        self.run_git("replace", "--graft", merged, base)
        self.assertEqual(self.classify(event=dict(self.event, before=base, after=merged),
                                       context=dict(self.context, CI_SHA=merged)), "broad")
        self.run_git("update-index", "--add", "--cacheinfo", "160000," + base + ",submodule")
        self.run_git("commit", "-qm", "gitlink")
        changed = self.run_git("rev-parse", "HEAD").strip()
        self.assertEqual(self.classify(event=dict(self.event, before=merged, after=changed),
                                       context=dict(self.context, CI_SHA=changed)), "broad")

    def test_raw_parser_refuses_partial_renamed_and_special_records(self):
        header = b":100644 100644 " + b"a" * 40 + b" " + b"b" * 40 + b" M"
        valid = header + b"\0CHANGELOG.md\0"
        self.assertTrue(scope.changelog_only(valid, 40))
        wide = b":100644 100644 " + b"a" * 64 + b" " + b"b" * 64 + b" M\0CHANGELOG.md\0"
        self.assertTrue(scope.changelog_only(wide, 64))
        for data in (
            b"", valid[:-1], valid + b"junk", valid + valid,
            header + b"\0CHANGELOG.md\nother\0", header + b"\0README.md\0",
            valid.replace(b":100644", b":100755"), valid.replace(b"100644 ", b"120000 "),
            valid.replace(b" M\0", b" R100\0"), valid.replace(b" M\0", b" T\0"),
            valid.replace(b"a" * 40, b"0" * 40), valid.replace(b"b" * 40, b"b" * 39),
            valid.replace(b"b" * 40, b"a" * 40), valid.replace(b":100644", b":160000"),
            valid.replace(b"CHANGELOG.md", b"CHANGELOG.md\t"),
        ):
            self.assertFalse(scope.changelog_only(data, 40), data)

    def test_main_outputs_only_fixed_values_and_truthful_scope(self):
        event_path, output = self.repo / "event.json", self.repo / "output"
        event_path.write_text(json.dumps(self.event))
        env = dict(self.env, **self.context, GITHUB_EVENT_PATH=str(event_path), GITHUB_OUTPUT=str(output))
        run = subprocess.run([sys.executable, str(ROOT / "tools/ci_scope.py")], cwd=self.repo,
                             env=env, capture_output=True, text=True, timeout=10)
        self.assertEqual(run.returncode, 0, run.stderr)
        self.assertEqual(output.read_text(), "mode=metadata\nreason=single-changelog-edit\n")
        self.assertIn("broad free checks are not run", run.stdout)
        output.unlink()
        event_path.write_text("{")
        run = subprocess.run([sys.executable, str(ROOT / "tools/ci_scope.py")], cwd=self.repo,
                             env=env, capture_output=True, text=True, timeout=10)
        self.assertEqual(run.returncode, 0, run.stderr)
        self.assertEqual(output.read_text(), "mode=broad\nreason=unproven-change\n")


if __name__ == "__main__":
    unittest.main()
