#!/usr/bin/env python3
"""Fail closed before publication; emit notes only after every local check succeeds.

This is the release changelog format, not a general Markdown renderer. Release
sections use ATX H2 headings, with closed comments and fenced code blocks. Raw
HTML blocks are unsupported; put HTML examples in fenced code.
"""

import argparse
from pathlib import Path
import re
import subprocess
import sys


SEMVER = re.compile(
    r"v(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)"
    r"(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?"
    r"(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?"
)
HEADING = re.compile(r" {0,3}(#{1,6})(?:[ \t]+(.*)|[ \t]*)$")
FENCE = re.compile(r" {0,3}(`{3,}|~{3,})(.*)$")
OID = re.compile(r"(?:[0-9a-f]{40}|[0-9a-f]{64})$")
CONTAINER = re.compile(r"^(?:>[ \t]*|[-+*](?:[ \t]+|$)|[0-9]+[.)](?:[ \t]+|$))")
SEPARATOR = re.compile(r" {0,3}(?:(?:\*[ \t]*){3,}|(?:-[ \t]*){3,}|(?:_[ \t]*){3,})$")
HTML_BLOCK = re.compile(r" {0,3}<(?:/?[A-Za-z][A-Za-z0-9-]*(?=[ \t/>]|$)|\?|!(?!--))")


def substantive_line(line):
    content = line.strip()
    while marker := CONTAINER.match(content):
        content = content[marker.end():].lstrip(" \t")
    content = re.sub(r"^\[[ xX]\](?:[ \t]+|$)", "", content)
    return bool(content) and not (
        HEADING.fullmatch(content) or SEPARATOR.fullmatch(content)
        or re.match(r"^\[[^\]]+\]:", content)
    )


def block_boundary(line):
    return not line.strip(" \t") or (
        HEADING.fullmatch(line) or FENCE.fullmatch(line) or SEPARATOR.fullmatch(line)
        or CONTAINER.match(line.lstrip(" \t"))
        or re.fullmatch(r" {0,3}(?:-+|=+)[ \t]*", line)
        or HTML_BLOCK.match(line) or re.match(r" {0,3}<!--", line)
    )


def git(*args):
    result = subprocess.run(
        ["git", *args], capture_output=True, text=True, check=True, timeout=60
    )
    return result.stdout.strip()


def local_tag(tag):
    match = SEMVER.fullmatch(tag)
    if not match or any(
        part.isdecimal() and len(part) > 1 and part.startswith("0")
        for part in (match.group(1) or "").split(".")
    ):
        raise ValueError("tag must be strict vMAJOR.MINOR.PATCH SemVer")
    ref = "refs/tags/" + tag
    head = git("rev-parse", "--verify", "HEAD^{commit}")
    commit = git("rev-parse", "--verify", ref + "^{commit}")
    obj = git("rev-parse", "--verify", ref)
    if not OID.fullmatch(head) or not OID.fullmatch(obj) or head != commit:
        raise ValueError("event tag does not resolve to the checked-out commit")
    return ref, obj, commit


def verify_remote(ref, obj, commit):
    # Compare the tag object too: replacing an annotation at the same commit is
    # still tag mutation. Never fetch here and silently accept the replacement.
    expected = {ref: obj}
    if obj != commit:
        expected[ref + "^{}"] = commit
    actual = {}
    for line in git("ls-remote", "--exit-code", "origin", ref, ref + "^{}").splitlines():
        fields = line.split("\t")
        if len(fields) != 2 or not OID.fullmatch(fields[0]) or fields[1] in actual:
            raise ValueError("remote tag lookup returned malformed or duplicate refs")
        actual[fields[1]] = fields[0]
    if actual != expected:
        raise ValueError("remote event tag is missing or changed since checkout")


def release_notes(text, version):
    lines = text.replace("\r\n", "\n").replace("\r", "\n").split("\n")
    notes = []
    selected = False
    substantive = False
    comment = False
    fence = None
    paragraph = False
    code_end = None

    def matching_code_end(index, start, run):
        pattern = re.compile(r"(?<!`)" + re.escape(run) + r"(?!`)")
        for number in range(index, len(lines)):
            line = lines[number]
            if number != index and block_boundary(line):
                break
            match = pattern.search(line, start if number == index else 0)
            if match:
                return number, match.end()
        return None

    def without_comments(line, index):
        nonlocal comment, code_end
        output = []
        pos = 0
        while pos < len(line):
            if comment:
                end = line.find("-->", pos)
                if end < 0:
                    break
                comment = False
                pos = end + 3
            elif code_end:
                if index < code_end[0]:
                    output.append(line[pos:])
                    break
                stop = code_end[1]
                output.append(line[pos:stop])
                pos = stop
                code_end = None
            elif line.startswith("<!--", pos):
                comment = True
                pos += 4
            elif line[pos] == "\\" and pos + 1 < len(line):
                output.append(line[pos:pos + 2])
                pos += 2
            elif line[pos] == "`":
                run = re.match(r"`+", line[pos:]).group()
                # Code spans may cross paragraph lines, but never swallow the
                # next release heading, code block or blank-line boundary.
                code_end = matching_code_end(index, pos + len(run), run)
                output.append(run)
                pos += len(run)
            else:
                output.append(line[pos])
                pos += 1
        return "".join(output)

    for index, raw in enumerate(lines):
        if fence:
            closer = re.fullmatch(r" {0,3}" + re.escape(fence[0]) + "{" + str(fence[1]) + r",}[ \t]*", raw)
            if selected:
                notes.append(raw)
                substantive |= not closer and bool(raw.strip())
            if closer:
                fence = None
            paragraph = False
            continue

        # Code fences own their literal contents, including comment-looking bytes.
        if not comment and HTML_BLOCK.match(raw):
            raise ValueError("raw HTML blocks are unsupported; use fenced HTML examples")
        opener = FENCE.fullmatch(raw) if not comment else None
        raw_heading = HEADING.fullmatch(raw) if not comment else None
        line = raw if opener else without_comments(raw, index)
        if opener:
            run, info = opener.groups()
            if run[0] == "`" and "`" in info:
                raise ValueError("backtick fence info may not contain backticks")
            fence = (run[0], len(run))
            if selected:
                notes.append(line)
            paragraph = False
            continue

        if paragraph and re.fullmatch(r" {0,3}-+[ \t]*", line):
            raise ValueError("Setext H2 is unsupported; use ## version release headings")
        # Removing a comment must never manufacture a structural heading.
        heading = HEADING.fullmatch(line) if raw_heading else None
        if heading and len(heading.group(1)) == 2:
            if selected:
                break
            title = re.sub(r"[ \t]+#+[ \t]*$", "", heading.group(2) or "").strip(" \t")
            if title != version:
                raise ValueError("CHANGELOG.md's first H2 must be finalized as ## " + version)
            selected = True
            paragraph = False
            continue
        if selected:
            notes.append(line)
            substantive |= substantive_line(line)
        paragraph = bool(line.strip(" \t")) and not block_boundary(raw)

    if comment or fence:
        raise ValueError("CHANGELOG.md has an unclosed comment or code fence")
    if not selected or not substantive:
        raise ValueError("CHANGELOG.md needs a finalized first release section with substantive notes")
    while notes and not notes[0].strip():
        notes.pop(0)
    while notes and not notes[-1].strip():
        notes.pop()
    return "\n".join(notes) + "\n"


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("notes", "verify-remote"))
    parser.add_argument("tag")
    args = parser.parse_args()
    try:
        identity = local_tag(args.tag)
        if args.command == "verify-remote":
            verify_remote(*identity)
        else:
            notes = release_notes(Path("CHANGELOG.md").read_text(encoding="utf-8"), args.tag[1:])
            sys.stdout.write(notes)
    except (OSError, ValueError, subprocess.SubprocessError) as error:
        print(f"release preflight: {error}; refusing to publish", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
