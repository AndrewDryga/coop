#!/usr/bin/env python3
"""Only a proven, single-commit CHANGELOG edit may use metadata checks."""

import json
import os
from pathlib import Path
import re
import subprocess
import tempfile


LIMIT = 1 << 20
OID = re.compile(rb"(?:[0-9a-f]{40}|[0-9a-f]{64})")
MAIN = "refs/heads/main"


def valid_oid(value):
    return (
        isinstance(value, str) and re.fullmatch(r"(?:[0-9a-f]{40}|[0-9a-f]{64})", value)
        and set(value) != {"0"}
    )


def git(*args):
    # Builtins only: no ambient Git directory, replacements, hooks or external diff helpers.
    env = {
        "PATH": os.environ.get("PATH", "/usr/bin:/bin"),
        "LC_ALL": "C", "GIT_CONFIG_GLOBAL": os.devnull, "GIT_CONFIG_SYSTEM": os.devnull,
        "GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_COUNT": "0",
        "GIT_NO_REPLACE_OBJECTS": "1", "GIT_TERMINAL_PROMPT": "0",
    }
    with tempfile.TemporaryFile() as output:
        subprocess.run(
            ["git", "--no-pager", "-c", "core.fsmonitor=false", *args],
            env=env, stdout=output, stderr=subprocess.DEVNULL, check=True, timeout=10,
        )
        output.seek(0)
        data = output.read(LIMIT + 1)
        if len(data) > LIMIT:
            raise ValueError("Git output exceeds the classification limit")
        return data


def changelog_only(data, oid_length):
    # Raw -z uses NUL header/path pairs. Do not decode filenames or accept partial records.
    if not data or not data.endswith(b"\0"):
        return False
    records = data[:-1].split(b"\0")
    if len(records) != 2 or records[1] != b"CHANGELOG.md":
        return False
    fields = records[0].split(b" ")
    return (
        len(fields) == 5 and fields[:2] == [b":100644", b"100644"] and fields[4] == b"M"
        and fields[2] != fields[3]
        and all(OID.fullmatch(oid) and len(oid) == oid_length and set(oid) != {ord("0")}
                for oid in fields[2:4])
    )


def classify(event, context, read_git=git):
    """Unknown, malformed, non-linear and release inputs always take the broad path."""
    if not isinstance(event, dict):
        return "broad"
    repository = context.get("CI_REPOSITORY", "")
    if (
        context.get("CI_EVENT_NAME") != "push"
        or context.get("CI_REF") != MAIN or event.get("ref") != MAIN
        or not re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", repository)
        or context.get("CI_WORKFLOW_REF") != repository + "/.github/workflows/ci.yml@" + MAIN
        or not isinstance(event.get("repository"), dict)
        or event["repository"].get("full_name") != repository
        or any(event.get(flag) is not False for flag in ("created", "deleted", "forced"))
    ):
        return "broad"
    before, after = event.get("before"), event.get("after")
    if (
        not valid_oid(before) or not valid_oid(after) or len(before) != len(after)
        or context.get("CI_SHA") != after
    ):
        return "broad"
    try:
        if read_git("rev-parse", "--is-shallow-repository").strip() != b"false":
            return "broad"
        if read_git("rev-parse", "--verify", "HEAD^{commit}").strip() != after.encode():
            return "broad"
        # A net diff can hide executable changes reverted inside a multi-commit push.
        # Read actual object headers: rev-list ancestry can be rewritten by info/grafts.
        headers = read_git("cat-file", "commit", after).split(b"\n\n", 1)[0].split(b"\n")
        parents = [line[7:] for line in headers if line.startswith(b"parent ")]
        if parents != [before.encode()]:
            return "broad"
        diff = read_git(
            "diff-tree", "--no-commit-id", "-r", "--raw", "-z", "--no-abbrev",
            "--no-renames", "--no-ext-diff", "--no-textconv", "--ignore-submodules=none",
            before, after, "--", ".",
        )
        return "metadata" if changelog_only(diff, len(after)) else "broad"
    except (OSError, ValueError, subprocess.SubprocessError):
        return "broad"


def main():
    mode = "broad"
    try:
        with Path(os.environ["GITHUB_EVENT_PATH"]).open("rb") as source:
            data = source.read(LIMIT + 1)
        if len(data) <= LIMIT:
            mode = classify(json.loads(data), os.environ)
    except (KeyError, OSError, ValueError):
        pass
    reason = "single-changelog-edit" if mode == "metadata" else "unproven-change"
    # Only fixed values reach Actions outputs; filenames and Git errors never do.
    if output := os.environ.get("GITHUB_OUTPUT"):
        with Path(output).open("a") as target:
            target.write(f"mode={mode}\nreason={reason}\n")
    print(f"CI scope: {mode} ({reason})")
    if mode == "metadata":
        print("Only metadata checks selected; broad free checks are not run.")


if __name__ == "__main__":
    main()
