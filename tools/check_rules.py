#!/usr/bin/env python3
"""Check that every .agent/kb/rules card is well-formed, indexed, and honest.

A rule is a verdict a review can fail against, so its metadata has to be true:
a `sources:` path that no longer exists means the rule may describe code that
moved, and a `check:` naming a test that doesn't exist claims a gate that isn't
there — worse than `check: none`, because everyone stops looking for one.

What this enforces:
  - frontmatter present, with name/description/scope/sources/check/updated
  - name matches the filename; scope is in the known vocabulary; updated is a date
  - every path in sources: exists
  - check: is `none`, a real `make <target>`, or a `go test <pkg> -run <Test>`
    whose package dir exists and which matches at least one real test function
    — also as a quoted alternation, `-run 'TestA|TestB'`, for a rule two test
    families gate; every alternative has to resolve
  - a `## Changelog` section exists
  - README.md indexes every rule exactly once, and every indexed link resolves

  tools/check_rules.py            # exit 1 and list problems
  tools/check_rules.py --quiet    # exit status only

Deliberately NOT checked: whether a rule is still *correct*, or whether its
check command passes. Only reading the rule against its sources tells you the
first; the gate itself tells you the second.
"""
import pathlib
import re
import sys

REQUIRED = ["name", "description", "scope", "sources", "check", "updated"]
SCOPES = {"cli-grammar", "cli-output", "docs", "box", "loop", "scaffold",
          "security", "architecture", "agent-workflow"}
DATE = re.compile(r"^\d{4}-\d{2}-\d{2}$")
# A bare test name, or the shell-quoted alternation a rule gated by two test families needs.
# The quotes are load-bearing, so an unquoted bar is rejected: the shell would eat it and run
# something other than what the card claims. Nothing else — a free-form regex here would be a
# check nobody can confirm resolves to real tests.
GO_TEST = re.compile(r"^go test (\./\S+) -run (\w+|'\w+(?:\|\w+)*')$")
MAKE = re.compile(r"^make ([\w-]+)$")
INDEX_LINK = re.compile(r"^- \[([^\]]+)\]\(([^)]+\.md)\)", re.M)


def parse_frontmatter(text):
    """Minimal front-matter reader for the fixed shape rule cards use."""
    if not text.startswith("---\n"):
        return None
    end = text.find("\n---\n", 4)
    if end == -1:
        return None
    fm = {}
    for line in text[4:end].split("\n"):
        if not line.strip() or ":" not in line:
            continue
        key, _, val = line.partition(":")
        val = val.strip()
        if val.startswith('"') and val.endswith('"') and len(val) > 1:
            val = val[1:-1].replace('\\"', '"').replace("\\\\", "\\")
        elif val.startswith("[") and val.endswith("]"):
            val = [v.strip() for v in val[1:-1].split(",") if v.strip()]
        fm[key.strip()] = val
    return fm


def check_command(cmd, slug, root):
    """A check: must name something that exists and can actually fail."""
    if cmd == "none":
        return []
    if m := MAKE.match(cmd):
        makefile = root / "Makefile"
        text = makefile.read_text() if makefile.exists() else ""
        if not re.search(rf"^{re.escape(m.group(1))}:", text, re.M):
            return [f"{slug}: check names 'make {m.group(1)}' but the Makefile has no such target"]
        return []
    if m := GO_TEST.match(cmd):
        pkg, tests = m.group(1), m.group(2).strip("'").split("|")
        pkgdir = root / pkg[2:]
        if not pkgdir.is_dir():
            return [f"{slug}: check runs '{pkg}' but that package dir does not exist"]
        bodies = [f.read_text() for f in pkgdir.glob("*_test.go")]
        return [f"{slug}: check runs -run {test} but no test function matches it in {pkg}"
                for test in tests
                if not any(re.search(rf"^func {test}\w*\(", body, re.M) for body in bodies)]
    return [f"{slug}: check {cmd!r} is not `none`, `make <target>`, `go test <pkg> -run <Test>`, "
            f"or `go test <pkg> -run '<TestA>|<TestB>'` "
            f"— an unrunnable check claims a gate that isn't there"]


def audit(root):
    """Return (problems, cards, gated) for the rules KB under root."""
    rules = root / ".agent" / "kb" / "rules"
    problems = []
    if not rules.is_dir():
        return ([f"no {rules} directory"], [], 0)

    cards = sorted(p for p in rules.glob("*.md") if p.stem != "README")
    gated = 0
    for path in cards:
        slug = path.stem
        text = path.read_text()

        fm = parse_frontmatter(text)
        if fm is None:
            problems.append(f"{slug}: no frontmatter (see .agent/kb/rules/README.md for the card format)")
            continue

        for key in REQUIRED:
            if key not in fm:
                problems.append(f"{slug}: frontmatter missing '{key}:'")
        if fm.get("name") != slug:
            problems.append(f"{slug}: name is {fm.get('name')!r}, must match the filename")
        if (scope := fm.get("scope")) and scope not in SCOPES:
            problems.append(f"{slug}: scope {scope!r} is not one of {sorted(SCOPES)}")
        if (updated := fm.get("updated")) and not DATE.match(str(updated)):
            problems.append(f"{slug}: updated {updated!r} is not YYYY-MM-DD")

        sources = fm.get("sources") or []
        if isinstance(sources, str):
            problems.append(f"{slug}: sources must be a [list]")
            sources = []
        for src in sources:
            if not (root / src).exists():
                problems.append(f"{slug}: sources names {src!r}, which does not exist — "
                                f"the rule may describe code that moved")

        if "check" in fm:
            problems += check_command(str(fm["check"]), slug, root)
            if str(fm["check"]) != "none":
                gated += 1

        if "## Changelog" not in text:
            problems.append(f"{slug}: no '## Changelog' section")

    readme = rules / "README.md"
    if not readme.exists():
        problems.append("README.md missing — the index is how rules get found on demand")
    else:
        linked = [t for _, t in INDEX_LINK.findall(readme.read_text())]
        indexed = [t[:-3] for t in linked]
        for slug in (p.stem for p in cards):
            n = indexed.count(slug)
            if n == 0:
                problems.append(f"{slug}: not in the README index — an unindexed rule is never read")
            elif n > 1:
                problems.append(f"{slug}: appears {n} times in the README index")
        for target in linked:
            if not (rules / target).exists():
                problems.append(f"README index links {target}, which does not exist")

    return (problems, cards, gated)


def main(argv):
    quiet = "--quiet" in argv
    problems, cards, gated = audit(pathlib.Path("."))
    if problems:
        if not quiet:
            print(f"✗ {len(problems)} problem(s) in .agent/kb/rules:\n", file=sys.stderr)
            for p in problems:
                print(f"  - {p}", file=sys.stderr)
            print("\n  the card format is documented in .agent/kb/rules/README.md", file=sys.stderr)
        return 1
    if not quiet:
        print(f"✓ {len(cards)} rule cards valid and indexed — {gated} gated by a command, "
              f"{len(cards) - gated} enforced in review")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
