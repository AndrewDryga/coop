import tempfile
import unittest
from pathlib import Path

from check_rules import audit, check_command, parse_frontmatter


GOOD_CARD = """---
name: sample-rule
description: "a sample rule, quoted because it has: a colon"
scope: cli-output
sources: [internal/cli/help.go]
check: "none"
updated: 2026-08-06
---

# Sample rule

**Why:** because.

## Changelog
- 2026-08-06 — created
"""

INDEX = """# .agent/kb/rules

## Index
- [sample-rule](sample-rule.md) — a sample rule
"""


def go_pkg(raw):
    """A root holding ./internal/agent with two real test functions."""
    root = Path(raw)
    pkg = root / "internal" / "agent"
    pkg.mkdir(parents=True)
    (pkg / "x_test.go").write_text("package agent\n\nfunc TestOneRenewal(t *testing.T) {}\n"
                                   "\nfunc TestTwoRenewal(t *testing.T) {}\n")
    return root


def scaffold(root, card=GOOD_CARD, index=INDEX):
    rules = root / ".agent" / "kb" / "rules"
    rules.mkdir(parents=True)
    (rules / "sample-rule.md").write_text(card)
    (rules / "README.md").write_text(index)
    (root / "internal" / "cli").mkdir(parents=True)
    (root / "internal" / "cli" / "help.go").write_text("package cli\n")
    return rules


class ParseFrontmatterTest(unittest.TestCase):
    def test_reads_quoted_scalars_and_lists(self):
        fm = parse_frontmatter(GOOD_CARD)
        self.assertEqual(fm["name"], "sample-rule")
        self.assertEqual(fm["description"], "a sample rule, quoted because it has: a colon")
        self.assertEqual(fm["sources"], ["internal/cli/help.go"])
        self.assertEqual(fm["check"], "none")

    def test_no_frontmatter_is_none(self):
        self.assertIsNone(parse_frontmatter("# Just a heading\n"))


class CheckCommandTest(unittest.TestCase):
    def test_none_is_fine(self):
        with tempfile.TemporaryDirectory() as raw:
            self.assertEqual(check_command("none", "r", Path(raw)), [])

    def test_make_target_must_exist(self):
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw)
            (root / "Makefile").write_text("align: ## align\n\t@true\n")
            self.assertEqual(check_command("make align", "r", root), [])
            self.assertTrue(check_command("make nope", "r", root))

    def test_go_test_must_match_a_real_function(self):
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw)
            pkg = root / "internal" / "cli"
            pkg.mkdir(parents=True)
            (pkg / "x_test.go").write_text("package cli\n\nfunc TestRealThing(t *testing.T) {}\n")
            self.assertEqual(check_command("go test ./internal/cli -run TestRealThing", "r", root), [])
            # prefix match is how `go test -run` actually behaves
            self.assertEqual(check_command("go test ./internal/cli -run TestReal", "r", root), [])
            self.assertTrue(check_command("go test ./internal/cli -run TestGhost", "r", root))
            self.assertTrue(check_command("go test ./internal/nope -run TestRealThing", "r", root))

    def test_go_test_accepts_a_quoted_alternation(self):
        with tempfile.TemporaryDirectory() as raw:
            root = go_pkg(raw)
            self.assertEqual(
                check_command("go test ./internal/agent -run 'TestOneRenewal|TestTwoRenewal'", "r", root), [])
            # a single name may be quoted too — the command runs either way
            self.assertEqual(check_command("go test ./internal/agent -run 'TestOneRenewal'", "r", root), [])
            # every alternative is validated, not just the first
            self.assertTrue(check_command("go test ./internal/agent -run 'TestOneRenewal|TestGhost'", "r", root))
            self.assertTrue(check_command("go test ./internal/agent -run 'TestGhost|TestOneRenewal'", "r", root))

    def test_go_test_rejects_looser_run_patterns(self):
        with tempfile.TemporaryDirectory() as raw:
            root = go_pkg(raw)
            # unquoted, the shell eats the bar and runs something else than the card claims
            self.assertTrue(check_command("go test ./internal/agent -run TestOneRenewal|TestTwoRenewal", "r", root))
            self.assertTrue(check_command("go test ./internal/agent -run 'TestOne.*'", "r", root))
            self.assertTrue(check_command("go test ./internal/agent -run 'TestOneRenewal|'", "r", root))
            self.assertTrue(check_command("go test ./internal/agent -run '|TestOneRenewal'", "r", root))
            self.assertTrue(check_command("go test ./internal/agent -run ''", "r", root))

    def test_unrunnable_shape_is_rejected(self):
        with tempfile.TemporaryDirectory() as raw:
            self.assertTrue(check_command("just look at it", "r", Path(raw)))


class AuditTest(unittest.TestCase):
    def test_good_tree_is_clean(self):
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw)
            scaffold(root)
            problems, cards, gated = audit(root)
            self.assertEqual(problems, [])
            self.assertEqual(len(cards), 1)
            self.assertEqual(gated, 0)

    def test_missing_source_is_caught(self):
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw)
            scaffold(root, card=GOOD_CARD.replace("internal/cli/help.go", "internal/cli/moved.go"))
            problems, _, _ = audit(root)
            self.assertTrue(any("does not exist" in p for p in problems))

    def test_unindexed_rule_is_caught(self):
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw)
            scaffold(root, index="# .agent/kb/rules\n\n## Index\n")
            problems, _, _ = audit(root)
            self.assertTrue(any("not in the README index" in p for p in problems))

    def test_index_link_to_missing_file_is_caught(self):
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw)
            scaffold(root, index=INDEX + "- [ghost](ghost.md) — gone\n")
            problems, _, _ = audit(root)
            self.assertTrue(any("ghost.md" in p for p in problems))

    def test_name_must_match_filename(self):
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw)
            scaffold(root, card=GOOD_CARD.replace("name: sample-rule", "name: other-rule"))
            problems, _, _ = audit(root)
            self.assertTrue(any("must match the filename" in p for p in problems))

    def test_unknown_scope_and_bad_date_are_caught(self):
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw)
            scaffold(root, card=GOOD_CARD.replace("scope: cli-output", "scope: vibes")
                                         .replace("updated: 2026-08-06", "updated: last tuesday"))
            problems, _, _ = audit(root)
            self.assertTrue(any("is not one of" in p for p in problems))
            self.assertTrue(any("not YYYY-MM-DD" in p for p in problems))

    def test_missing_frontmatter_and_changelog_are_caught(self):
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw)
            scaffold(root, card="# Bare rule\n\nno metadata at all\n")
            problems, _, _ = audit(root)
            self.assertTrue(any("no frontmatter" in p for p in problems))

    def test_gated_count_tracks_real_checks(self):
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw)
            scaffold(root, card=GOOD_CARD.replace('check: "none"', 'check: "make align"'))
            (root / "Makefile").write_text("align:\n\t@true\n")
            problems, _, gated = audit(root)
            self.assertEqual(problems, [])
            self.assertEqual(gated, 1)


if __name__ == "__main__":
    unittest.main()
