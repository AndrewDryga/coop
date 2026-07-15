#!/usr/bin/env python3
"""Record a real Coop cast outside the task queue, validate it, then promote it."""

from __future__ import annotations

import argparse
import os
import re
import secrets
import shlex
import subprocess
import sys
import tempfile
from pathlib import Path
from typing import Callable

from cast_hygiene import CastValidationError, validate_cast


ROOT = Path(__file__).resolve().parent.parent
NAME_RE = re.compile(r"[a-z0-9][a-z0-9-]*")


def resolve_task(root: Path, task_id: str) -> Path:
    proc = subprocess.run(
        ["go", "run", ".", "tasks", "path", task_id],
        cwd=root,
        check=False,
        capture_output=True,
        text=True,
    )
    if proc.returncode != 0:
        detail = (proc.stderr or proc.stdout).strip()
        raise RuntimeError(f"cannot resolve task {task_id!r}: {detail or 'coop tasks path failed'}")
    path = Path(proc.stdout.strip()).resolve()
    if not (path / "task.md").is_file():
        raise RuntimeError(f"resolved task folder has no task.md: {path}")
    return path


def _open_directory(name: str | Path, *, dir_fd: int | None = None) -> int:
    flags = os.O_RDONLY | getattr(os, "O_DIRECTORY", 0) | getattr(os, "O_NOFOLLOW", 0)
    return os.open(name, flags, dir_fd=dir_fd)


def _ensure_directory(parent_fd: int, name: str) -> int:
    try:
        os.mkdir(name, 0o755, dir_fd=parent_fd)
    except FileExistsError:
        pass
    return _open_directory(name, dir_fd=parent_fd)


def _promote(source: Path, task_dir: Path, name: str) -> None:
    task_fd = _open_directory(task_dir)
    artifacts_fd = casts_fd = None
    pending = f".{name}.{secrets.token_hex(6)}.tmp"
    try:
        os.stat("task.md", dir_fd=task_fd, follow_symlinks=False)
        artifacts_fd = _ensure_directory(task_fd, "artifacts")
        casts_fd = _ensure_directory(artifacts_fd, "cast-candidates")
        output_fd = os.open(pending, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o644, dir_fd=casts_fd)
        with source.open("rb") as src, os.fdopen(output_fd, "wb") as dst:
            while chunk := src.read(128 * 1024):
                dst.write(chunk)
            dst.flush()
            os.fsync(dst.fileno())
        os.replace(pending, f"{name}.cast", src_dir_fd=casts_fd, dst_dir_fd=casts_fd)
    finally:
        if casts_fd is not None:
            try:
                os.unlink(pending, dir_fd=casts_fd)
            except FileNotFoundError:
                pass
            os.close(casts_fd)
        if artifacts_fd is not None:
            os.close(artifacts_fd)
        os.close(task_fd)


def capture_and_promote(
    root: Path,
    task_id: str,
    name: str,
    command: list[str],
    *,
    idle_limit: float = 2.0,
    window_size: str = "120x36",
    resolver: Callable[[Path, str], Path] = resolve_task,
    run: Callable[..., subprocess.CompletedProcess] = subprocess.run,
) -> Path:
    if not NAME_RE.fullmatch(name):
        raise ValueError("cast name must use lowercase letters, digits, and hyphens")
    if not command:
        raise ValueError("a command is required after --")

    # Fail early, but never retain or write through this path: the task may move while recording.
    resolver(root, task_id)
    with tempfile.TemporaryDirectory(prefix="coop-cast-") as raw_tmp:
        captured = Path(raw_tmp) / f"{name}.cast"
        env = os.environ.copy()
        env.setdefault("COOP_SPINNER", "0")
        proc = run(
            [
                "asciinema",
                "record",
                "--quiet",
                "--overwrite",
                "--return",
                "--idle-time-limit",
                str(idle_limit),
                "--window-size",
                window_size,
                "--command",
                shlex.join(command),
                str(captured),
            ],
            cwd=root,
            env=env,
            check=False,
        )
        if proc.returncode != 0:
            raise RuntimeError(f"recorded command failed with exit {proc.returncode}; no task artifact was written")
        try:
            validate_cast(captured, root=root)
        except CastValidationError as exc:
            raise RuntimeError(f"recording is unsafe and was not promoted: {exc}") from exc

        current = resolver(root, task_id)
        _promote(captured, current, name)
        final = resolver(root, task_id)
        return final / "artifacts" / "cast-candidates" / f"{name}.cast"


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--task", required=True, help="task id; its current folder is resolved after recording")
    parser.add_argument("--name", required=True, help="artifact basename without .cast")
    parser.add_argument("--idle-time-limit", type=float, default=2.0)
    parser.add_argument("--window-size", default="120x36")
    parser.add_argument("command", nargs=argparse.REMAINDER)
    args = parser.parse_args(argv)
    command = args.command[1:] if args.command[:1] == ["--"] else args.command
    try:
        destination = capture_and_promote(
            ROOT,
            args.task,
            args.name,
            command,
            idle_limit=args.idle_time_limit,
            window_size=args.window_size,
        )
    except (OSError, RuntimeError, ValueError) as exc:
        print(f"capture failed: {exc}", file=sys.stderr)
        return 1
    print(f"captured {destination.relative_to(ROOT)}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
