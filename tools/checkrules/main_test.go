package main

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const goodCard = `---
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
`

const index = `# .agent/kb/rules

## Index
- [sample-rule](sample-rule.md) — a sample rule
`

// A descriptive card: subsystem instead of scope, and no check: at all — it describes, it doesn't
// rule, so there is no command that can fail on a violation of it.
const goodKBCard = `---
name: sample-card
description: "how X actually works, and the trap"
subsystem: box
sources: [internal/cli/help.go]
updated: 2026-08-10
---

The fact, citing internal/cli/help.go.

## Changelog
- 2026-08-10 — created
`

const kbIndex = `# .agent/kb

## Index
- [sample-card](sample-card.md) — how X actually works
`

func writeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, text := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func scaffold(t *testing.T, root, card, index string) {
	writeFiles(t, root, map[string]string{
		".agent/kb/rules/sample-rule.md": card,
		".agent/kb/rules/README.md":      index,
		"internal/cli/help.go":           "package cli\n",
	})
}

func scaffoldKB(t *testing.T, root, card, index string) {
	writeFiles(t, root, map[string]string{
		".agent/kb/sample-card.md": card,
		".agent/kb/README.md":      index,
		"internal/cli/help.go":     "package cli\n",
	})
}

// has reports whether any problem contains want.
func has(problems []string, want string) bool {
	return slices.ContainsFunc(problems, func(p string) bool { return strings.Contains(p, want) })
}

func TestParseFrontmatter(t *testing.T) {
	fm := parseFrontmatter(goodCard)
	for key, want := range map[string]string{
		"name":        "sample-rule",
		"description": "a sample rule, quoted because it has: a colon",
		"check":       "none",
		"sources":     `["internal/cli/help.go"]`,
	} {
		if got := fm[key].String(); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	if !fm["sources"].list {
		t.Error("sources: [a] must read as a list")
	}
	for _, text := range []string{"# Just a heading\n", "# Heading\n---\nname: x\n---\n", "---\nname: x\n"} {
		if fm := parseFrontmatter(text); fm != nil {
			t.Errorf("%q has no frontmatter (it must open and close the file's first block), got %v", text, fm)
		}
	}
}

func TestCheckCommand(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"Makefile":                  "align: ## align\n\t@true\n",
		"internal/cli/x_test.go":    "package cli\n\nfunc TestRealThing(t *testing.T) {}\n",
		"internal/agent/x_test.go":  "package agent\n\nfunc TestOneRenewal(t *testing.T) {}\n\nfunc TestTwoRenewal(t *testing.T) {}\n",
		"internal/agent/helpers.go": "package agent\n\nfunc TestGhost() {}\n", // not a test file
	})
	unrunnable := "is not `none`, `make <target>`"
	for _, tc := range []struct {
		cmd, want string // want is part of the one problem expected; "" means none
	}{
		{"none", ""},
		{"make align", ""},
		{"make nope", "has no such target"},
		{"go test ./internal/cli -run TestRealThing", ""},
		// prefix match is how `go test -run` actually behaves
		{"go test ./internal/cli -run TestReal", ""},
		{"go test ./internal/cli -run TestGhost", "no test function matches it"},
		{"go test ./internal/nope -run TestRealThing", "package dir does not exist"},
		{"go test ./internal/agent -run 'TestOneRenewal|TestTwoRenewal'", ""},
		// a single name may be quoted too — the command runs either way
		{"go test ./internal/agent -run 'TestOneRenewal'", ""},
		// every alternative is validated, not just the first
		{"go test ./internal/agent -run 'TestOneRenewal|TestGhost'", "-run TestGhost but no test function"},
		{"go test ./internal/agent -run 'TestGhost|TestOneRenewal'", "-run TestGhost but no test function"},
		// unquoted, the shell eats the bar and runs something else than the card claims
		{"go test ./internal/agent -run TestOneRenewal|TestTwoRenewal", unrunnable},
		{"go test ./internal/agent -run 'TestOne.*'", unrunnable},
		{"go test ./internal/agent -run 'TestOneRenewal|'", unrunnable},
		{"go test ./internal/agent -run '|TestOneRenewal'", unrunnable},
		{"go test ./internal/agent -run ''", unrunnable},
		{"just look at it", unrunnable},
	} {
		problems := checkCommand(tc.cmd, "r", root)
		if tc.want == "" && len(problems) != 0 || tc.want != "" && (len(problems) != 1 || !has(problems, tc.want)) {
			t.Errorf("check %q: got %q, want one problem with %q (none when empty)", tc.cmd, problems, tc.want)
		}
	}
}

func TestAuditRules(t *testing.T) {
	for _, tc := range []struct {
		name, card, index, makefile string
		want                        []string // parts of expected problems; none means a clean tree
		gated                       int
	}{
		{name: "good tree is clean", card: goodCard, index: index},
		{name: "missing source", card: strings.Replace(goodCard, "internal/cli/help.go", "internal/cli/moved.go", 1), index: index,
			want: []string{`"internal/cli/moved.go", which does not exist`}},
		{name: "sources not a list", card: strings.Replace(goodCard, "[internal/cli/help.go]", "internal/cli/help.go", 1), index: index,
			want: []string{"sources must be a [list]"}},
		{name: "unindexed rule", card: goodCard, index: "# .agent/kb/rules\n\n## Index\n",
			want: []string{"not in the README index"}},
		{name: "rule indexed twice", card: goodCard, index: index + "- [sample-rule](sample-rule.md) — again\n",
			want: []string{"appears 2 times in the README index"}},
		{name: "index link to missing file", card: goodCard, index: index + "- [ghost](ghost.md) — gone\n",
			want: []string{"README index links ghost.md, which does not exist"}},
		{name: "no index", card: goodCard,
			want: []string{"README.md missing — the index is how rules get found on demand"}},
		{name: "name must match filename", card: strings.Replace(goodCard, "name: sample-rule", "name: other-rule", 1), index: index,
			want: []string{`name is "other-rule", must match the filename`}},
		{name: "unknown scope and bad date", card: strings.NewReplacer("scope: cli-output", "scope: vibes",
			"updated: 2026-08-06", "updated: last tuesday").Replace(goodCard), index: index,
			want: []string{`scope "vibes" is not one of ["agent-workflow", "architecture", `, `updated "last tuesday" is not YYYY-MM-DD`}},
		{name: "a list is not a scope", card: strings.Replace(goodCard, "scope: cli-output", "scope: [cli-output]", 1), index: index,
			want: []string{`scope ["cli-output"] is not one of`}},
		{name: "CRLF line endings read the same", card: strings.ReplaceAll(goodCard, "\n", "\r\n"), index: index},
		{name: "missing frontmatter", card: "# Bare rule\n\nno metadata at all\n", index: index,
			want: []string{"no frontmatter"}},
		{name: "unrunnable check", card: strings.Replace(goodCard, `check: "none"`, `check: "go test ./internal/cli -run 'TestOne.*'"`, 1), index: index,
			want: []string{`check "go test ./internal/cli -run 'TestOne.*'" is not`}, gated: 1},
		{name: "gated count tracks real checks", card: strings.Replace(goodCard, `check: "none"`, `check: "make align"`, 1), index: index,
			makefile: "align:\n\t@true\n", gated: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			scaffold(t, root, tc.card, tc.index)
			if tc.index == "" {
				if err := os.Remove(filepath.Join(root, ".agent/kb/rules/README.md")); err != nil {
					t.Fatal(err)
				}
			}
			if tc.makefile != "" {
				writeFiles(t, root, map[string]string{"Makefile": tc.makefile})
			}
			problems, cards, gated := auditRules(root)
			if len(tc.want) == 0 && len(problems) != 0 {
				t.Errorf("want a clean tree, got %q", problems)
			}
			for _, want := range tc.want {
				if !has(problems, want) {
					t.Errorf("want a problem with %q, got %q", want, problems)
				}
			}
			if len(cards) != 1 || gated != tc.gated {
				t.Errorf("got %d cards and %d gated, want 1 and %d", len(cards), gated, tc.gated)
			}
		})
	}
}

func TestAuditKB(t *testing.T) {
	for _, tc := range []struct {
		name, card, index string
		want              []string // parts of expected problems; none means a clean tree
	}{
		// no check: field, and none demanded
		{name: "good tree is clean", card: goodKBCard, index: kbIndex},
		{name: "missing subsystem", card: strings.Replace(goodKBCard, "subsystem: box\n", "", 1), index: kbIndex,
			want: []string{"frontmatter missing 'subsystem:'"}},
		{name: "empty required value", card: strings.Replace(goodKBCard, "subsystem: box", "subsystem:", 1), index: kbIndex,
			want: []string{"frontmatter 'subsystem:' is empty"}},
		{name: "missing source", card: strings.Replace(goodKBCard, "internal/cli/help.go]", "internal/cli/moved.go]", 1), index: kbIndex,
			want: []string{"does not exist"}},
		// it would resolve under the root, but an agent opens it as written
		{name: "absolute source", card: strings.Replace(goodKBCard, "[internal/cli/help.go]", "[/internal/cli/help.go]", 1), index: kbIndex,
			want: []string{`sources names "/internal/cli/help.go" — sources must be repo-relative`}},
		{name: "unindexed card", card: goodKBCard, index: "# .agent/kb\n\n## Index\n",
			want: []string{"not in the README index — an unindexed card is never read"}},
		{name: "index link to missing file", card: goodKBCard, index: kbIndex + "- [ghost](ghost.md) — gone\n",
			want: []string{"ghost.md"}},
		{name: "name, date and changelog", card: strings.NewReplacer("name: sample-card", "name: other-card",
			"updated: 2026-08-10", "updated: yesterday", "## Changelog", "## History").Replace(goodKBCard), index: kbIndex,
			want: []string{"must match the filename", "not YYYY-MM-DD", "no '## Changelog' section"}},
		{name: "missing frontmatter", card: "# Bare card\n\nno metadata at all\n", index: kbIndex,
			want: []string{"no frontmatter (see .agent/kb/README.md for the card format)"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			scaffoldKB(t, root, tc.card, tc.index)
			problems, cards := auditKB(root)
			if len(tc.want) == 0 && len(problems) != 0 {
				t.Errorf("want a clean tree, got %q", problems)
			}
			for _, want := range tc.want {
				if !has(problems, want) {
					t.Errorf("want a problem with %q, got %q", want, problems)
				}
			}
			if len(cards) != 1 {
				t.Errorf("got %d cards, want 1", len(cards))
			}
		})
	}
}

// rules/ is the other register: its cards belong to its own index, not this one.
func TestAuditKBSkipsTheRulesRegister(t *testing.T) {
	root := t.TempDir()
	scaffoldKB(t, root, goodKBCard, kbIndex)
	scaffold(t, root, goodCard, index)
	problems, cards := auditKB(root)
	if len(problems) != 0 || len(cards) != 1 || filepath.Base(cards[0]) != "sample-card.md" {
		t.Errorf("got problems %q and cards %q, want only sample-card", problems, cards)
	}
}

// This repo's own instructions must not teach an agent to truncate a mutation's result.
func TestAuditGuidance(t *testing.T) {
	for _, tc := range []struct {
		name, file, text string
		want             string // part of the one problem expected; "" means none
	}{
		{"a truncated mutation", ".agent/skills/sweep.md", "Finish it:\n\n    coop tasks done $id | tail -2\n", "sweep.md:3:"},
		{"a bounded tail of a log", ".agent/skills/sweep.md",
			"Run the gate:\n\n    make check > gate.log 2>&1; tail -n 40 gate.log\n\n`coop fork logs` | tail a loop log\n", ""},
		{"the rule that quotes the mistake", ".agent/kb/rules/read-a-mutation-result-whole.md", "    coop tasks block <id> 2>&1 | tail -2\n", ""},
		{"the same name elsewhere", "docs/read-a-mutation-result-whole.md", "    git commit -m x | head -1\n", "read-a-mutation-result-whole.md:1:"},
		{"the checker itself", "tools/checkrules/main.go", "// coop tasks done x | tail -2\n", ""},
		{"another tool", "tools/other/main.go", "// coop tasks done x | tail -2\n", "main.go:1:"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeFiles(t, root, map[string]string{"AGENTS.md": "# rules\n", tc.file: tc.text})
			problems := auditGuidance(root)
			if tc.want == "" && len(problems) != 0 || tc.want != "" && (len(problems) != 1 || !has(problems, tc.want)) {
				t.Errorf("got %q, want one problem with %q (none when empty)", problems, tc.want)
			}
		})
	}
}

// A guidance root that is a link (.agent/skills pointing at a split-out skills/) is read too.
func TestAuditGuidanceReadsALinkedRoot(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{"AGENTS.md": "# rules\n", "skills/sweep.md": "    coop tasks done $id | tail -2\n"})
	if err := os.MkdirAll(filepath.Join(root, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../skills", filepath.Join(root, ".agent", "skills")); err != nil {
		t.Fatal(err)
	}
	if problems := auditGuidance(root); len(problems) != 1 || !has(problems, ".agent/skills/sweep.md:1:") {
		t.Errorf("got %q, want the truncated mutation under the linked root", problems)
	}
}

// run is what `make rules-check` prints: a summary when every check passes, else each register's
// problems on stderr — and nothing at all with -quiet.
func TestRun(t *testing.T) {
	root := t.TempDir()
	scaffoldKB(t, root, goodKBCard, kbIndex)
	scaffold(t, root, goodCard, index)
	var stdout, stderr bytes.Buffer
	if code := run(root, false, &stdout, &stderr); code != 0 || stderr.Len() != 0 ||
		stdout.String() != "✓ 1 kb cards valid and indexed\n"+
			"✓ no mutating command's output is truncated in this repo's instructions\n"+
			"✓ 1 rule cards valid and indexed — 0 gated by a command, 1 enforced in review\n" {
		t.Errorf("clean tree: exit %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}

	scaffold(t, root, strings.Replace(goodCard, "## Changelog", "## History", 1), index)
	stdout.Reset()
	if code := run(root, false, &stdout, &stderr); code != 1 || stdout.Len() != 0 ||
		stderr.String() != "✗ 1 problem(s) in .agent/kb/rules:\n\n"+
			"  - sample-rule: no '## Changelog' section\n\n"+
			"  the card format is documented in .agent/kb/rules/README.md\n\n" {
		t.Errorf("bad rule: exit %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
	stderr.Reset()
	if code := run(root, true, &stdout, &stderr); code != 1 || stdout.Len()+stderr.Len() != 0 {
		t.Errorf("-quiet: exit %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
}
