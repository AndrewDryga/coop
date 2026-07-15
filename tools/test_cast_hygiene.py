import json
import subprocess
import tempfile
import unittest
from pathlib import Path

import capture_cast
from cast_hygiene import CastValidationError, validate_cast


def write_cast(path: Path, text: str, version: int = 3) -> None:
    header = {"version": version, "term": {"cols": 80, "rows": 24}, "env": {"SHELL": "/bin/zsh"}}
    path.write_text(json.dumps(header) + "\n" + json.dumps([0.1, "o", text]) + "\n", encoding="utf-8")


class CastHygieneTest(unittest.TestCase):
    def test_safe_generic_capture(self):
        with tempfile.TemporaryDirectory() as raw:
            path = Path(raw) / "safe.cast"
            write_cast(path, "· using codex model sol credential personal\r\n/home/node/work is boxed\r\n")
            validate_cast(path, environ={}, root=Path(raw) / "repo")

    def test_rejects_private_paths_credentials_and_secrets(self):
        cases = {
            "home": "/Users/alice/code/private\r\n",
            "account": "target codex:gpt-5@alice-backup\r\n",
            "credential": "· using codex model sol credential customer-prod\r\n",
            "secret": "OPENAI_API_KEY=sk-example0123456789abcdef\r\n",
            "environment": "token-from-test-environment\r\n",
        }
        with tempfile.TemporaryDirectory() as raw:
            for name, text in cases.items():
                with self.subTest(name=name):
                    path = Path(raw) / f"{name}.cast"
                    write_cast(path, text)
                    env = {"SERVICE_TOKEN": "token-from-test-environment"}
                    with self.assertRaises(CastValidationError):
                        validate_cast(path, environ=env, root=Path(raw) / "repo")

    def test_rejects_split_private_value(self):
        with tempfile.TemporaryDirectory() as raw:
            path = Path(raw) / "split.cast"
            header = {"version": 3, "term": {"cols": 80, "rows": 24}}
            path.write_text(
                json.dumps(header) + "\n" + json.dumps([0.1, "o", "/Users/"]) + "\n" + json.dumps([0.1, "o", "alice/repo"]) + "\n",
                encoding="utf-8",
            )
            with self.assertRaises(CastValidationError):
                validate_cast(path, environ={}, root=Path(raw) / "repo")

    def test_capture_resolves_current_task_only_when_promoting(self):
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw)
            old = root / ".agent/tasks/10_in_progress/task-a"
            new = root / ".agent/tasks/99_done/task-a"
            old.mkdir(parents=True)
            (old / "task.md").write_text("task\n", encoding="utf-8")
            resolved = 0

            def resolver(_root, _task_id):
                nonlocal resolved
                resolved += 1
                return old if resolved == 1 else new

            def run(argv, **_kwargs):
                captured = Path(argv[-1])
                write_cast(captured, "safe output\r\n")
                new.parent.mkdir(parents=True, exist_ok=True)
                old.rename(new)
                return subprocess.CompletedProcess(argv, 0)

            destination = capture_cast.capture_and_promote(root, "task-a", "demo", ["coop", "loop"], resolver=resolver, run=run)
            self.assertEqual(destination, new / "artifacts/cast-candidates/demo.cast")
            self.assertTrue(destination.is_file())
            self.assertFalse(old.exists())

    def test_promotion_follows_task_directory_rename(self):
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw)
            old = root / "10_in_progress/task-a"
            new = root / "99_done/task-a"
            old.mkdir(parents=True)
            new.parent.mkdir(parents=True)
            (old / "task.md").write_text("task\n", encoding="utf-8")
            captured = root / "captured.cast"
            write_cast(captured, "safe output\r\n")

            original = capture_cast._ensure_directory
            moved = False

            def rename_then_ensure(parent_fd, name):
                nonlocal moved
                if not moved:
                    old.rename(new)
                    moved = True
                return original(parent_fd, name)

            capture_cast._ensure_directory = rename_then_ensure
            try:
                capture_cast._promote(captured, old, "demo")
            finally:
                capture_cast._ensure_directory = original

            self.assertTrue((new / "artifacts/cast-candidates/demo.cast").is_file())
            self.assertFalse(old.exists())

    def test_failed_capture_leaves_queue_untouched(self):
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw)
            task = root / ".agent/tasks/10_in_progress/task-a"
            task.mkdir(parents=True)
            (task / "task.md").write_text("task\n", encoding="utf-8")

            def resolver(_root, _task_id):
                return task

            def run(argv, **_kwargs):
                return subprocess.CompletedProcess(argv, 9)

            with self.assertRaises(RuntimeError):
                capture_cast.capture_and_promote(root, "task-a", "demo", ["false"], resolver=resolver, run=run)
            self.assertEqual(sorted(path.name for path in task.iterdir()), ["task.md"])


if __name__ == "__main__":
    unittest.main()
