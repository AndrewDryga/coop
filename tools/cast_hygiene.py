#!/usr/bin/env python3
"""Validate asciicast files before they become public website assets."""

from __future__ import annotations

import argparse
import json
import os
import re
import sys
from pathlib import Path


ANSI_RE = re.compile(r"\x1b(?:\[[0-?]*[ -/]*[@-~]|\][^\x07]*(?:\x07|\x1b\\))")
POSIX_HOME_RE = re.compile(r"(?<![A-Za-z0-9_])/(Users|home)/([^/\s]+)(?:/|\b)")
WINDOWS_HOME_RE = re.compile(r"(?i)\b[A-Z]:\\Users\\([^\\\s]+)(?:\\|\b)")
TARGET_ACCOUNT_RE = re.compile(r"\b(?:claude|codex|gemini|grok)(?::[^\s@,]+)?@([A-Za-z0-9_.+-]+)")
USING_CREDENTIAL_RE = re.compile(r"(?i)\busing\b[^\r\n]*?\b(?:credential|account)\s+([A-Za-z0-9_.@+-]+)")
ASSIGNED_CREDENTIAL_RE = re.compile(r"(?i)\b(?:credential|account)\s*[:=]\s*['\"]?([A-Za-z0-9_.@+-]+)")
ASSIGNED_SECRET_RE = re.compile(
    r"(?i)\b(?:api[_-]?key|access[_-]?token|auth[_-]?token|password|passwd|secret)"
    r"\s*[:=]\s*['\"]?([A-Za-z0-9_./+=:@-]{12,})"
)

SAFE_ACCOUNT_LABELS = {"auto", "default", "personal", "work"}
SAFE_POSIX_HOMES = {("home", "node")}
SECRET_ENV_NAME_RE = re.compile(r"(?i)(?:TOKEN|SECRET|PASSWORD|PASSWD|API_?KEY|PRIVATE_?KEY)")
SECRET_LITERAL_PATTERNS = [
    ("private key", re.compile(r"-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----")),
    ("OpenAI/Anthropic key", re.compile(r"\bsk-(?:ant-)?[A-Za-z0-9_-]{16,}")),
    ("GitHub token", re.compile(r"\b(?:gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,})")),
    ("AWS access key", re.compile(r"\b(?:AKIA|ASIA)[A-Z0-9]{16}\b")),
    ("Google API key", re.compile(r"\bAIza[A-Za-z0-9_-]{30,}")),
    ("bearer token", re.compile(r"(?i)\bBearer\s+[A-Za-z0-9._~+/=-]{16,}")),
]


class CastValidationError(ValueError):
    pass


def _cast_paths(raw_paths: list[str]) -> list[Path]:
    paths: list[Path] = []
    for raw in raw_paths:
        path = Path(raw)
        if path.is_dir():
            paths.extend(sorted(path.glob("*.cast")))
        else:
            paths.append(path)
    return paths


def _read_cast(path: Path) -> tuple[dict, str]:
    try:
        lines = path.read_text(encoding="utf-8").splitlines()
    except OSError as exc:
        raise CastValidationError(f"cannot read: {exc}") from exc
    if not lines:
        raise CastValidationError("empty cast")
    try:
        header = json.loads(lines[0])
    except json.JSONDecodeError as exc:
        raise CastValidationError(f"invalid header JSON: {exc}") from exc
    if not isinstance(header, dict) or header.get("version") not in (2, 3):
        raise CastValidationError("header must declare asciicast version 2 or 3")

    metadata: list[str] = []
    chunks: list[str] = []
    for field in ("command", "title"):
        if isinstance(header.get(field), str):
            metadata.append(header[field])
    env = header.get("env")
    if isinstance(env, dict):
        metadata.extend(str(value) for value in env.values())
    for line_no, line in enumerate(lines[1:], 2):
        try:
            event = json.loads(line)
        except json.JSONDecodeError as exc:
            raise CastValidationError(f"line {line_no}: invalid event JSON: {exc}") from exc
        if not isinstance(event, list) or len(event) < 3 or not isinstance(event[2], str):
            raise CastValidationError(f"line {line_no}: malformed event")
        chunks.append(event[2])
    if len(lines) == 1:
        raise CastValidationError("cast has no events")
    text = "\n".join(metadata + ["".join(chunks)])
    return header, ANSI_RE.sub("", text)


def _unsafe_environment_values(environ: dict[str, str] | None) -> list[str]:
    env = os.environ if environ is None else environ
    return [value for key, value in env.items() if SECRET_ENV_NAME_RE.search(key) and len(value) >= 12]


def cast_findings(path: Path, *, environ: dict[str, str] | None = None, root: Path | None = None) -> list[str]:
    _, text = _read_cast(path)
    findings: list[str] = []

    for match in POSIX_HOME_RE.finditer(text):
        home = (match.group(1), match.group(2))
        if home not in SAFE_POSIX_HOMES:
            findings.append(f"host home path {match.group(0).rstrip('/')!r}")
    if match := WINDOWS_HOME_RE.search(text):
        findings.append(f"Windows home path {match.group(0).rstrip(chr(92))!r}")

    resolved_root = (root or Path(__file__).resolve().parent.parent).resolve()
    for private_path in {str(Path.home()), str(resolved_root)}:
        if private_path and private_path in text:
            findings.append(f"private local path {private_path!r}")

    labels = []
    labels.extend(match.group(1) for match in TARGET_ACCOUNT_RE.finditer(text))
    labels.extend(match.group(1) for match in USING_CREDENTIAL_RE.finditer(text))
    labels.extend(match.group(1) for match in ASSIGNED_CREDENTIAL_RE.finditer(text))
    for label in sorted(set(labels)):
        if label.lower() not in SAFE_ACCOUNT_LABELS:
            findings.append(f"identifying credential/account label {label!r}")

    for name, pattern in SECRET_LITERAL_PATTERNS:
        if pattern.search(text):
            findings.append(f"{name} literal")
    if match := ASSIGNED_SECRET_RE.search(text):
        field = match.group(0).split(":", 1)[0].split("=", 1)[0].strip()
        findings.append(f"secret-like assigned value for {field!r}")
    for value in _unsafe_environment_values(environ):
        if value in text:
            findings.append("value from a secret-bearing environment variable")
            break

    return list(dict.fromkeys(findings))


def validate_cast(path: Path, *, environ: dict[str, str] | None = None, root: Path | None = None) -> None:
    findings = cast_findings(path, environ=environ, root=root)
    if findings:
        raise CastValidationError("; ".join(findings))


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("paths", nargs="+", help="cast file or directory containing casts")
    args = parser.parse_args(argv)
    paths = _cast_paths(args.paths)
    if not paths:
        parser.error("no .cast files found")

    failed = False
    for path in paths:
        try:
            validate_cast(path)
        except CastValidationError as exc:
            failed = True
            print(f"unsafe cast {path}: {exc}", file=sys.stderr)
    if failed:
        return 1
    print(f"safe casts: {len(paths)} checked")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
