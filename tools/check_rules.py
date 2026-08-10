#!/usr/bin/env python3
"""Check that every .agent/kb card — descriptive and normative — is well-formed, indexed, honest.

Both registers are read on demand through a README index, so the metadata IS the
routing table: an unindexed card is never opened, and a `sources:` path that no
longer exists means the card may describe code that moved. A rule carries more,
because it is a verdict a review can fail against: a `check:` naming a test that
doesn't exist claims a gate that isn't there — worse than `check: none`, because
everyone stops looking for one.

What this enforces in BOTH registers:
  - frontmatter present, name matches the filename, updated is a date
  - every required key is present and non-empty (a blank `subsystem:` routes nowhere)
  - every path in sources: exists
  - a `## Changelog` section exists
  - README.md indexes every card exactly once, and every indexed link resolves

Rules (.agent/kb/rules) carry two more fields, so they get two more checks:
  - scope is in the known vocabulary
  - check: is `none`, a real `make <target>`, or a `go test <pkg> -run <Test>`
    whose package dir exists and which matches at least one real test function
    — also as a quoted alternation, `-run 'TestA|TestB'`, for a rule two test
    families gate; every alternative has to resolve

  tools/check_rules.py            # exit 1 and list problems
  tools/check_rules.py --quiet    # exit status only

Deliberately NOT checked: whether a card is still *correct*, or whether a rule's
check command passes. Only reading the card against its sources tells you the
first; the gate itself tells you the second.
"""
import pathlib
import re
import sys

RULE_REQUIRED = ["name", "description", "scope", "sources", "check", "updated"]
# Descriptive cards have no scope and no check — they describe, they don't rule.
# `subsystem:` is deliberately open vocabulary (the README's list ends in "…"), so
# it is checked for presence, never against a fixed set.
KB_REQUIRED = ["name", "description", "subsystem", "sources", "updated"]
RULES_DOC = ".agent/kb/rules/README.md"
KB_DOC = ".agent/kb/README.md"
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


def check_card(path, root, required, doc):
    """The metadata every card carries, whichever register it lives in.

    Returns (problems, frontmatter); frontmatter is None when there is none to read.
    """
    slug, text = path.stem, path.read_text()
    fm = parse_frontmatter(text)
    if fm is None:
        return ([f"{slug}: no frontmatter (see {doc} for the card format)"], None)

    problems = []
    for key in required:
        if key not in fm:
            problems.append(f"{slug}: frontmatter missing '{key}:'")
        elif not fm[key]:
            problems.append(f"{slug}: frontmatter '{key}:' is empty")
    if fm.get("name") != slug:
        problems.append(f"{slug}: name is {fm.get('name')!r}, must match the filename")
    if (updated := fm.get("updated")) and not DATE.match(str(updated)):
        problems.append(f"{slug}: updated {updated!r} is not YYYY-MM-DD")

    sources = fm.get("sources") or []
    if isinstance(sources, str):
        problems.append(f"{slug}: sources must be a [list]")
        sources = []
    for src in sources:
        if not (root / src).exists():
            problems.append(f"{slug}: sources names {src!r}, which does not exist — "
                            f"the card may describe code that moved")

    if "## Changelog" not in text:
        problems.append(f"{slug}: no '## Changelog' section")
    return (problems, fm)


def check_index(directory, cards, noun):
    """README pairing both ways: every card indexed once, every indexed link resolving."""
    readme = directory / "README.md"
    if not readme.exists():
        return [f"README.md missing — the index is how {noun}s get found on demand"]

    problems = []
    linked = [t for _, t in INDEX_LINK.findall(readme.read_text())]
    indexed = [t[:-3] for t in linked]
    for slug in (p.stem for p in cards):
        n = indexed.count(slug)
        if n == 0:
            problems.append(f"{slug}: not in the README index — an unindexed {noun} is never read")
        elif n > 1:
            problems.append(f"{slug}: appears {n} times in the README index")
    problems += [f"README index links {t}, which does not exist"
                 for t in linked if not (directory / t).exists()]
    return problems


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
        card_problems, fm = check_card(path, root, RULE_REQUIRED, RULES_DOC)
        problems += card_problems
        if fm is None:
            continue

        if (scope := fm.get("scope")) and scope not in SCOPES:
            problems.append(f"{slug}: scope {scope!r} is not one of {sorted(SCOPES)}")
        if "check" in fm:
            problems += check_command(str(fm["check"]), slug, root)
            if str(fm["check"]) != "none":
                gated += 1

    return (problems + check_index(rules, cards, "rule"), cards, gated)


def audit_kb(root):
    """Return (problems, cards) for the descriptive KB under root."""
    kb = root / ".agent" / "kb"
    if not kb.is_dir():
        return ([f"no {kb} directory"], [])

    # Flat on purpose: the other register (rules/) is audited by audit() against its own
    # format. The README allows per-subsystem subfolders once the list gets long — the day
    # one appears, this glob has to learn to recurse or those cards go unvalidated.
    cards = sorted(p for p in kb.glob("*.md") if p.stem != "README")
    problems = []
    for path in cards:
        problems += check_card(path, root, KB_REQUIRED, KB_DOC)[0]

    return (problems + check_index(kb, cards, "card"), cards)


def main(argv):
    quiet = "--quiet" in argv
    root = pathlib.Path(".")
    kb_problems, kb_cards = audit_kb(root)
    problems, cards, gated = audit(root)
    if kb_problems or problems:
        if not quiet:
            for label, doc, found in ((".agent/kb", KB_DOC, kb_problems),
                                      (".agent/kb/rules", RULES_DOC, problems)):
                if not found:
                    continue
                print(f"✗ {len(found)} problem(s) in {label}:\n", file=sys.stderr)
                for p in found:
                    print(f"  - {p}", file=sys.stderr)
                print(f"\n  the card format is documented in {doc}\n", file=sys.stderr)
        return 1
    if not quiet:
        print(f"✓ {len(kb_cards)} kb cards valid and indexed")
        print(f"✓ {len(cards)} rule cards valid and indexed — {gated} gated by a command, "
              f"{len(cards) - gated} enforced in review")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
