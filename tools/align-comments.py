#!/usr/bin/env python3
"""Check (or fix) trailing-`#`-comment alignment in the CLI docs and site.

Inside a code block, a run of trailing comments must line up: every `#` sits in
one vertical column, one space past the block's widest commented line. A single
long line poking its comment out past the rest is the defect this guards against.

A "code block" is a ``` fence (.md) or a <pre> (.html); the whole `coop help`
manual in docs/cli.md is one big fence. Within a block, a blank line starts a
fresh alignment group. Only groups with >=2 trailing comments are checked (one
comment can't be misaligned). Width is visual: HTML tags and entities resolved.

  tools/align-comments.py --check FILE...   # exit 1 and list offenders
  tools/align-comments.py --write FILE...   # rewrite to align

The default file set is the docs + site pages that carry these examples; the
source of truth (internal/cli/help.go) is guarded transitively — its manual is
regenerated into docs/cli.md by `make docs`, which `make docs-check` enforces.
"""
import html
import re
import sys

DEFAULT_FILES = ["README.md", "site/index.html", "site/docs.html", "docs/cli.md"]
TAG = re.compile(r"<[^>]+>")
# A lone line this many runes longer than the next-widest is a standalone example,
# not part of the column — don't drag every comment out to it (mirrors help-output-style's
# "completeness beats alignment" verb-row exception). Enforce alignment on the rest.
OUTLIER_GAP = 28


def vis(s):  # visual width: drop HTML tags, resolve entities, drop <pre>'s '>' artifact
    return len(html.unescape(re.sub(r"^\s*>", "", TAG.sub("", s))))


def split_comment(raw):
    """(pre, comment) if raw has a TRAILING comment (code before it), else None."""
    i = raw.find('<span class="t">')
    if i >= 0:
        pre = raw[:i].rstrip(" ")
        code = html.unescape(TAG.sub("", pre)).lstrip("> ").rstrip()
        return (pre, raw[i:]) if code else None
    m = re.match(r"^(.*\S) +(#.*)$", raw)  # plain: code # comment
    return (m.group(1), m.group(2)) if m else None


def in_code_flags(lines, path):
    """Bool per line: is this line inside a ``` fence / <pre> code block?"""
    flags, fenced, pre = [], False, False
    for ln in lines:
        if path.endswith(".html"):
            here = pre
            if "<pre" in ln:
                pre = True
            if "</pre>" in ln:
                pre = False
            flags.append(here or "<pre" in ln)
        else:  # markdown-ish: ``` toggles
            if ln.lstrip().startswith("```"):
                fenced = not fenced
                flags.append(False)
            else:
                flags.append(fenced)
    return flags


def is_blank(raw):
    return html.unescape(TAG.sub("", raw)).strip() in ("", ">")


def blocks(lines, path):
    """Yield [(index, pre, comment), ...] — the trailing comments in one code block."""
    code = in_code_flags(lines, path)
    cur = []
    for i, ln in enumerate(lines):
        if not code[i]:  # left the ``` fence / <pre> — boundary between examples
            if cur:
                yield cur
                cur = []
            continue
        if sc := split_comment(ln):
            cur.append((i, *sc))
    if cur:
        yield cur


def stanzas(block, lines):
    """Split a block's comments where a blank line falls between two of them."""
    out, cur, prev = [], [], None
    for item in block:
        if prev is not None and any(is_blank(lines[j]) for j in range(prev + 1, item[0])):
            out.append(cur)
            cur = []
        cur.append(item)
        prev = item[0]
    return out + [cur] if cur else out


def process(path, write):
    lines = open(path).read().split("\n")
    offenders, dirty = [], False
    for block in blocks(lines, path):
        if len(block) < 2:
            continue
        span = [vis(pre) for _, pre, _ in block]
        # One column for the whole example only when its lines are comparable in width;
        # else align each blank-separated stanza on its own, so two disparate command
        # lists aren't dragged into a single far-right column.
        for grp in [block] if max(span) - min(span) <= OUTLIER_GAP else stanzas(block, lines):
            if len(grp) < 2:
                continue
            w = sorted((vis(pre) for _, pre, _ in grp), reverse=True)
            if w[0] - w[1] > OUTLIER_GAP:  # a lone over-long line — leave it standalone
                continue
            if len({vis(lines[i][: lines[i].find(cm)]) for i, _, cm in grp}) == 1:
                continue  # already one column — keep the author's gap, don't churn it
            offenders += [(i + 1, lines[i].rstrip()) for i, _, _ in grp]
            if write:
                for i, pre, comment in grp:
                    new = pre + " " * (w[0] + 1 - vis(pre)) + comment
                    if new != lines[i]:
                        lines[i], dirty = new, True
    if write and dirty:
        open(path, "w").write("\n".join(lines))
    return offenders


def main():
    args = sys.argv[1:]
    write = "--write" in args
    check = "--check" in args
    files = [a for a in args if not a.startswith("--")] or DEFAULT_FILES
    bad = 0
    for f in files:
        offenders = process(f, write)
        for ln, text in offenders:
            bad += 1
            print(f"{f}:{ln}: trailing comment not aligned with its block: {text}")
    if check and bad:
        print(f"\n{bad} misaligned comment line(s). Run: tools/align-comments.py --write")
        sys.exit(1)


if __name__ == "__main__":
    main()
