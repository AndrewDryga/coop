// Command checkrules checks that every .agent/kb card — descriptive and normative — is well-formed,
// indexed and honest.
//
// Both registers are read on demand through a README index, so the metadata IS the routing table:
// an unindexed card is never opened, and a `sources:` path that no longer exists means the card may
// describe code that moved. A rule carries more, because it is a verdict a review can fail against:
// a `check:` naming a test that doesn't exist claims a gate that isn't there — worse than
// `check: none`, because everyone stops looking for one.
//
// What this enforces in BOTH registers:
//   - frontmatter present, name matches the filename, updated is a date
//   - every required key is present and non-empty (a blank `subsystem:` routes nowhere)
//   - every path in sources: exists
//   - a `## Changelog` section exists
//   - README.md indexes every card exactly once, and every indexed link resolves
//
// Rules (.agent/kb/rules) carry two more fields, so they get two more checks:
//   - scope is in the known vocabulary
//   - check: is `none`, a real `make <target>`, or a `go test <pkg> -run <Test>` whose package dir
//     exists and which matches at least one real test function — also as a quoted alternation,
//     `-run 'TestA|TestB'`, for a rule two test families gate; every alternative has to resolve
//
// Run it from the repository root (make rules-check does):
//
//	go run ./tools/checkrules           # exit 1 and list problems
//	go run ./tools/checkrules -quiet    # exit status only
//
// One rule is enforced here beyond the cards themselves, because this repo's own instructions are
// where an agent learns a habit: a state-changing command whose output is truncated
// (`coop tasks done … | tail`) teaches every agent that reads it to re-run mutations blind. See
// .agent/kb/rules/read-a-mutation-result-whole.md.
//
// Deliberately NOT checked: whether a card is still *correct*, or whether a rule's check command
// passes. Only reading the card against its sources tells you the first; the gate itself tells you
// the second.
package main

import (
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

var (
	ruleRequired = []string{"name", "description", "scope", "sources", "check", "updated"}
	// Descriptive cards have no scope and no check — they describe, they don't rule. `subsystem:` is
	// deliberately open vocabulary (the README's list ends in "…"), so it is checked for presence,
	// never against a fixed set.
	kbRequired = []string{"name", "description", "subsystem", "sources", "updated"}
	// Sorted, because the problem message lists them.
	scopes = []string{"agent-workflow", "architecture", "box", "cli-grammar", "cli-output", "docs", "loop", "scaffold", "security"}
	date   = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	// A bare test name, or the shell-quoted alternation a rule gated by two test families needs. The
	// quotes are load-bearing, so an unquoted bar is rejected: the shell would eat it and run
	// something other than what the card claims. Nothing else — a free-form regex here would be a
	// check nobody can confirm resolves to real tests.
	goTest     = regexp.MustCompile(`^go test (\./\S+) -run (\w+|'\w+(?:\|\w+)*')$`)
	makeTarget = regexp.MustCompile(`^make ([\w-]+)$`)
	indexLink  = regexp.MustCompile(`(?m)^- \[([^\]]+)\]\(([^)]+\.md)\)`)
	// A mutating command whose output is piped into tail/head. The backtick in the excluded run of
	// characters keeps a markdown table cell ("`coop fork logs` | tail a loop log") from reading as
	// a pipeline. The rule card itself quotes the mistake it exists for, so it is not scanned.
	truncatedMutation = regexp.MustCompile(`(?:coop (?:tasks|fork|backlog) [a-z-]+|git (?:commit|push)|docker rm)[^|` +
		"`" + `\n]*\| *(?:tail|head)\b`)
	// The three files that spell the mistake out on purpose: the rule itself, this checker, and its
	// test. Named one by one so the exemption stays auditable — a pattern would hide a real one.
	quotesTheMistake = []string{".agent/kb/rules/" + mutationRule + ".md", "tools/checkrules/main.go", "tools/checkrules/main_test.go"}
	// Where this repo instructs its agents. A truncated mutation in any of these is the habit itself.
	guidance = []string{"AGENTS.md", "README.md", "Makefile", "docs", "tools", ".agent/skills", ".agent/kb"}
)

const (
	rulesDoc     = ".agent/kb/rules/README.md"
	kbDoc        = ".agent/kb/README.md"
	mutationRule = "read-a-mutation-result-whole"
)

func main() {
	quiet := flag.Bool("quiet", false, "print nothing; report through the exit status only")
	flag.Parse()
	os.Exit(run(".", *quiet, os.Stdout, os.Stderr))
}

func run(root string, quiet bool, stdout, stderr io.Writer) int {
	kbProblems, kbCards := auditKB(root)
	problems, cards, gated := auditRules(root)
	instructions := auditGuidance(root)
	if len(kbProblems) > 0 || len(problems) > 0 || len(instructions) > 0 {
		if quiet {
			return 1
		}
		for _, section := range []struct {
			label, doc string
			found      []string
		}{
			{".agent/kb", kbDoc, kbProblems},
			{".agent/kb/rules", rulesDoc, problems},
			{"this repo's instructions", ".agent/kb/rules/" + mutationRule + ".md", instructions},
		} {
			if len(section.found) == 0 {
				continue
			}
			fmt.Fprintf(stderr, "✗ %d problem(s) in %s:\n\n", len(section.found), section.label)
			for _, p := range section.found {
				fmt.Fprintf(stderr, "  - %s\n", p)
			}
			fmt.Fprintf(stderr, "\n  the card format is documented in %s\n\n", section.doc)
		}
		return 1
	}
	if !quiet {
		fmt.Fprintf(stdout, "✓ %d kb cards valid and indexed\n", len(kbCards))
		fmt.Fprintln(stdout, "✓ no mutating command's output is truncated in this repo's instructions")
		fmt.Fprintf(stdout, "✓ %d rule cards valid and indexed — %d gated by a command, %d enforced in review\n",
			len(cards), gated, len(cards)-gated)
	}
	return 0
}

// value is one frontmatter field: a scalar, or a list written as [a, b].
type value struct {
	text  string
	items []string
	list  bool
}

func (v value) empty() bool { return v.text == "" && len(v.items) == 0 }

// String is the field as the checks compare it: a list reads as its quoted form, so a list where a
// scalar belongs never passes for one.
func (v value) String() string {
	if v.list {
		return v.quoted()
	}
	return v.text
}

// quoted is the field as a problem quotes it, so a stray space or an empty value stays visible.
func (v value) quoted() string {
	if v.list {
		return quoteList(v.items)
	}
	return quote(v.text)
}

func quoteList(items []string) string {
	quoted := make([]string, len(items))
	for i, item := range items {
		quoted[i] = quote(item)
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

// quote is s as a problem quotes it, so a stray space or an empty value stays visible.
func quote(s string) string { return strconv.Quote(s) }

// parseFrontmatter is a minimal reader for the fixed shape cards use; nil means there is none.
func parseFrontmatter(text string) map[string]value {
	if !strings.HasPrefix(text, "---\n") {
		return nil
	}
	end := strings.Index(text[4:], "\n---\n")
	if end == -1 {
		return nil
	}
	fm := map[string]value{}
	for _, line := range strings.Split(text[4:4+end], "\n") {
		key, val, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(line) == "" {
			continue
		}
		val = strings.TrimSpace(val)
		var v value
		switch {
		case len(val) > 1 && strings.HasPrefix(val, `"`) && strings.HasSuffix(val, `"`):
			v.text = strings.ReplaceAll(strings.ReplaceAll(val[1:len(val)-1], `\"`, `"`), `\\`, `\`)
		case strings.HasPrefix(val, "[") && strings.HasSuffix(val, "]"):
			v.list = true
			for _, item := range strings.Split(val[1:len(val)-1], ",") {
				if item = strings.TrimSpace(item); item != "" {
					v.items = append(v.items, item)
				}
			}
		default:
			v.text = val
		}
		fm[strings.TrimSpace(key)] = v
	}
	return fm
}

// readText reads a file as text with its line endings normalized, so a card checked out with CRLF
// endings reads the same as one with LF.
func readText(path string) (string, error) {
	data, err := os.ReadFile(path)
	return strings.ReplaceAll(strings.ReplaceAll(string(data), "\r\n", "\n"), "\r", "\n"), err
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// checkCommand returns the problems with a rule's check: field, which must name something that
// exists and can actually fail.
func checkCommand(cmd, slug, root string) []string {
	if cmd == "none" {
		return nil
	}
	if m := makeTarget.FindStringSubmatch(cmd); m != nil {
		text, _ := readText(filepath.Join(root, "Makefile")) // no Makefile has no targets
		if !regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(m[1]) + `:`).MatchString(text) {
			return []string{fmt.Sprintf("%s: check names 'make %s' but the Makefile has no such target", slug, m[1])}
		}
		return nil
	}
	if m := goTest.FindStringSubmatch(cmd); m != nil {
		pkg, tests := m[1], strings.Split(strings.Trim(m[2], "'"), "|")
		pkgdir := filepath.Join(root, pkg[2:])
		entries, err := os.ReadDir(pkgdir)
		if err != nil {
			return []string{fmt.Sprintf("%s: check runs '%s' but that package dir does not exist", slug, pkg)}
		}
		var bodies []string
		for _, entry := range entries {
			if strings.HasSuffix(entry.Name(), "_test.go") {
				if body, err := readText(filepath.Join(pkgdir, entry.Name())); err == nil {
					bodies = append(bodies, body)
				}
			}
		}
		var problems []string
		for _, test := range tests {
			fn := regexp.MustCompile(`(?m)^func ` + test + `\w*\(`)
			if !slices.ContainsFunc(bodies, fn.MatchString) {
				problems = append(problems, fmt.Sprintf("%s: check runs -run %s but no test function matches it in %s", slug, test, pkg))
			}
		}
		return problems
	}
	return []string{fmt.Sprintf("%s: check %s is not `none`, `make <target>`, `go test <pkg> -run <Test>`, "+
		"or `go test <pkg> -run '<TestA>|<TestB>'` "+
		"— an unrunnable check claims a gate that isn't there", slug, quote(cmd))}
}

// checkCard checks the metadata every card carries, whichever register it lives in. The
// frontmatter it returns is nil when the card has none to read.
func checkCard(root, path string, required []string, doc string) ([]string, map[string]value) {
	slug := strings.TrimSuffix(filepath.Base(path), ".md")
	text, err := readText(path)
	if err != nil {
		return []string{fmt.Sprintf("%s: %v", slug, err)}, nil
	}
	fm := parseFrontmatter(text)
	if fm == nil {
		return []string{fmt.Sprintf("%s: no frontmatter (see %s for the card format)", slug, doc)}, nil
	}

	var problems []string
	for _, key := range required {
		if v, ok := fm[key]; !ok {
			problems = append(problems, fmt.Sprintf("%s: frontmatter missing '%s:'", slug, key))
		} else if v.empty() {
			problems = append(problems, fmt.Sprintf("%s: frontmatter '%s:' is empty", slug, key))
		}
	}
	if name, ok := fm["name"]; ok && (name.list || name.text != slug) {
		problems = append(problems, fmt.Sprintf("%s: name is %s, must match the filename", slug, name.quoted()))
	}
	if updated := fm["updated"]; !updated.empty() && !date.MatchString(updated.String()) {
		problems = append(problems, fmt.Sprintf("%s: updated %s is not YYYY-MM-DD", slug, updated.quoted()))
	}

	if sources := fm["sources"]; !sources.list && !sources.empty() {
		problems = append(problems, fmt.Sprintf("%s: sources must be a [list]", slug))
	} else {
		for _, src := range sources.items {
			switch {
			case filepath.IsAbs(src):
				// Joined under the root it would resolve, yet an agent reading the card opens it as
				// written: a host path, or nothing.
				problems = append(problems, fmt.Sprintf("%s: sources names %s — sources must be repo-relative", slug, quote(src)))
			case !exists(filepath.Join(root, src)):
				problems = append(problems, fmt.Sprintf("%s: sources names %s, which does not exist — "+
					"the card may describe code that moved", slug, quote(src)))
			}
		}
	}

	if !strings.Contains(text, "## Changelog") {
		problems = append(problems, fmt.Sprintf("%s: no '## Changelog' section", slug))
	}
	return problems, fm
}

// checkIndex pairs the README index with the cards both ways: every card indexed once, every
// indexed link resolving.
func checkIndex(dir string, cards []string, noun string) []string {
	readme, err := readText(filepath.Join(dir, "README.md"))
	if err != nil {
		return []string{fmt.Sprintf("README.md missing — the index is how %ss get found on demand", noun)}
	}

	var problems, linked []string
	indexed := map[string]int{}
	for _, m := range indexLink.FindAllStringSubmatch(readme, -1) {
		linked = append(linked, m[2])
		indexed[strings.TrimSuffix(m[2], ".md")]++
	}
	for _, card := range cards {
		slug := strings.TrimSuffix(filepath.Base(card), ".md")
		switch n := indexed[slug]; {
		case n == 0:
			problems = append(problems, fmt.Sprintf("%s: not in the README index — an unindexed %s is never read", slug, noun))
		case n > 1:
			problems = append(problems, fmt.Sprintf("%s: appears %d times in the README index", slug, n))
		}
	}
	for _, target := range linked {
		if !exists(filepath.Join(dir, target)) {
			problems = append(problems, fmt.Sprintf("README index links %s, which does not exist", target))
		}
	}
	return problems
}

// cardsIn lists a register's cards, sorted: every .md file in dir but the README index. Flat on
// purpose: the rules register under the descriptive one is audited against its own format. The kb
// README allows per-subsystem subfolders once the list gets long — the day one appears, this has to
// learn to recurse or those cards go unvalidated.
func cardsIn(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var cards []string
	for _, entry := range entries {
		if name := entry.Name(); strings.HasSuffix(name, ".md") && name != "README.md" {
			cards = append(cards, filepath.Join(dir, name))
		}
	}
	return cards, nil
}

// auditRules returns the problems in the rules KB under root, its cards, and how many of them a
// command gates.
func auditRules(root string) ([]string, []string, int) {
	rules := filepath.Join(root, ".agent", "kb", "rules")
	cards, err := cardsIn(rules)
	if err != nil {
		return []string{fmt.Sprintf("no %s directory", rules)}, nil, 0
	}

	var problems []string
	gated := 0
	for _, path := range cards {
		slug := strings.TrimSuffix(filepath.Base(path), ".md")
		cardProblems, fm := checkCard(root, path, ruleRequired, rulesDoc)
		problems = append(problems, cardProblems...)
		if fm == nil {
			continue
		}

		if scope := fm["scope"]; !scope.empty() && (scope.list || !slices.Contains(scopes, scope.text)) {
			problems = append(problems, fmt.Sprintf("%s: scope %s is not one of %s", slug, scope.quoted(), quoteList(scopes)))
		}
		if check, ok := fm["check"]; ok {
			problems = append(problems, checkCommand(check.String(), slug, root)...)
			if check.String() != "none" {
				gated++
			}
		}
	}
	return append(problems, checkIndex(rules, cards, "rule")...), cards, gated
}

// auditKB returns the problems in the descriptive KB under root, and its cards.
func auditKB(root string) ([]string, []string) {
	kb := filepath.Join(root, ".agent", "kb")
	cards, err := cardsIn(kb)
	if err != nil {
		return []string{fmt.Sprintf("no %s directory", kb)}, nil
	}
	var problems []string
	for _, path := range cards {
		cardProblems, _ := checkCard(root, path, kbRequired, kbDoc)
		problems = append(problems, cardProblems...)
	}
	return append(problems, checkIndex(kb, cards, "card")...), cards
}

// auditGuidance returns the problems in this repo's own instructions: a mutation whose result is
// truncated.
func auditGuidance(root string) []string {
	var problems []string
	for _, rel := range guidance {
		for _, file := range filesUnder(filepath.Join(root, rel)) {
			name, err := filepath.Rel(root, file)
			if err != nil || slices.Contains(quotesTheMistake, filepath.ToSlash(name)) ||
				slices.Contains(strings.Split(filepath.ToSlash(name), "/"), "__pycache__") {
				continue
			}
			text, err := readText(file)
			if err != nil || !utf8.ValidString(text) {
				continue // a binary or unreadable file teaches nobody a habit
			}
			for number, line := range strings.Split(text, "\n") {
				if truncatedMutation.MatchString(line) {
					problems = append(problems, fmt.Sprintf("%s:%d: a mutating command's output is truncated "+
						"(`| tail`/`| head`) — read it whole, or check the state it changed", file, number+1))
				}
			}
		}
	}
	return problems
}

// filesUnder lists every file under path, sorted, or path itself when it is not a directory.
func filesUnder(path string) []string {
	if info, err := os.Stat(path); err != nil || !info.IsDir() {
		return []string{path}
	}
	var files []string
	// The trailing separator makes the walk enter a root that is itself a link (.agent/skills may
	// link to a split-out skills/); walked bare, such a root reads as one file and nothing under it.
	_ = filepath.WalkDir(path+string(filepath.Separator), func(file string, entry fs.DirEntry, err error) error {
		if err == nil && !entry.IsDir() {
			if info, err := os.Stat(file); err == nil && info.Mode().IsRegular() {
				files = append(files, file)
			}
		}
		return nil // an unreadable directory holds nothing to read
	})
	return files
}
