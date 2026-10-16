// Command aligncomments checks (or fixes) trailing-`#`-comment alignment in the CLI docs and site.
//
// Inside a code block, a run of trailing comments must line up: every `#` sits in one vertical
// column, one space past the block's widest commented line. A single long line poking its comment
// out past the rest is the defect this guards against.
//
// A "code block" is a ``` fence (.md) or a <pre> (.html); the whole `coop help` manual in
// docs/cli.md is one big fence. Within a block, a blank line starts a fresh alignment group. Only
// groups with >=2 trailing comments are checked (one comment can't be misaligned). Width is visual:
// HTML tags and entities resolved.
//
//	go run ./tools/aligncomments -check FILE...   # exit 1 and list offenders
//	go run ./tools/aligncomments -write FILE...   # rewrite to align
//
// The default file set is the docs + site pages that carry these examples; the source of truth
// (internal/cli/help.go) is guarded transitively — its manual is regenerated into docs/cli.md by
// `make docs`, which `make docs-check` enforces.
package main

import (
	"flag"
	"fmt"
	"html"
	"os"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

var defaultFiles = []string{"README.md", "CONTRIBUTING.md", "MIGRATING.md", "site/index.html", "site/docs.html",
	"docs/agents.md", "docs/box.md", "docs/cli.md", "docs/configuration.md", "docs/evals.md",
	"docs/forks.md", "docs/integrations.md", "docs/loop.md", "docs/networking.md",
	"docs/session-api.md"}

var (
	tag = regexp.MustCompile(`<[^>]+>`)
	// <pre>'s '>' artifact: a <pre> tag closed at the start of the next line leaves its '>' there.
	preArtifact = regexp.MustCompile(`^\s*>`)
	// code # comment: the comment is the last run of " #…" on the line.
	trailingComment = regexp.MustCompile(`^(.*\S) +(#.*)$`)
)

// A lone line this many runes longer than the next-widest is a standalone example, not part of the
// column — don't drag every comment out to it (mirrors help-output-style's "completeness beats
// alignment" verb-row exception). Enforce alignment on the rest.
const outlierGap = 28

// comment is one line's trailing comment: line indexes the file, pre is the code before it.
type comment struct {
	line         int
	pre, comment string
}

type offender struct {
	line int
	text string
}

func main() {
	check := flag.Bool("check", false, "exit 1 when a block is misaligned")
	write := flag.Bool("write", false, "rewrite misaligned blocks in place")
	flag.Parse()
	files := flag.Args()
	if len(files) == 0 {
		files = defaultFiles
	}
	bad := 0
	for _, file := range files {
		offenders, err := process(file, *write)
		if err != nil {
			fmt.Fprintf(os.Stderr, "aligncomments: %v\n", err)
			os.Exit(1)
		}
		for _, o := range offenders {
			bad++
			fmt.Printf("%s:%d: trailing comment not aligned with its block: %s\n", file, o.line, o.text)
		}
	}
	if *check && bad > 0 {
		fmt.Printf("\n%d misaligned comment line(s). Run: go run ./tools/aligncomments -write\n", bad)
		os.Exit(1)
	}
}

// vis is the visual width. In HTML, drop tags, resolve entities and drop <pre>'s '>' artifact. A
// markdown fence shows its text as written, so a placeholder like <id> counts there.
func vis(s string, markup bool) int {
	if !markup {
		return utf8.RuneCountInString(s)
	}
	return utf8.RuneCountInString(html.UnescapeString(preArtifact.ReplaceAllString(tag.ReplaceAllString(s, ""), "")))
}

// splitComment splits raw into the code and its TRAILING comment; ok is false when there is no
// comment or no code before it.
func splitComment(raw string) (pre, comment string, ok bool) {
	if i := strings.Index(raw, `<span class="t">`); i >= 0 {
		pre = strings.TrimRight(raw[:i], " ")
		code := strings.TrimRightFunc(strings.TrimLeft(html.UnescapeString(tag.ReplaceAllString(pre, "")), "> "), unicode.IsSpace)
		return pre, raw[i:], code != ""
	}
	m := trailingComment.FindStringSubmatch(raw)
	if m == nil || quoted(m[1]) {
		return "", "", false // a '#' inside a quoted string (tmux's '#{...}') is not a comment
	}
	return m[1], m[2], true
}

// quoted reports whether pre leaves a quote open, so the '#' after it sits inside a string.
func quoted(pre string) bool {
	return strings.Count(pre, "'")%2 == 1 || strings.Count(pre, `"`)%2 == 1
}

// inCode reports, per line, whether it sits inside a ``` fence or <pre> code block.
func inCode(lines []string, markup bool) []bool {
	flags := make([]bool, len(lines))
	fenced, pre := false, false
	for i, ln := range lines {
		if markup {
			here := pre
			if strings.Contains(ln, "<pre") {
				pre = true
			}
			if strings.Contains(ln, "</pre>") {
				pre = false
			}
			flags[i] = here || strings.Contains(ln, "<pre")
			continue
		}
		if strings.HasPrefix(strings.TrimLeftFunc(ln, unicode.IsSpace), "```") {
			fenced = !fenced // the fence line itself is not code
			continue
		}
		flags[i] = fenced
	}
	return flags
}

func isBlank(raw string, markup bool) bool {
	if !markup {
		return strings.TrimSpace(raw) == ""
	}
	text := strings.TrimSpace(html.UnescapeString(tag.ReplaceAllString(raw, "")))
	return text == "" || text == ">"
}

// blocks returns the trailing comments of each code block; leaving a fence or <pre> is the boundary
// between examples.
func blocks(lines []string, markup bool) [][]comment {
	var out [][]comment
	var cur []comment
	for i, code := range inCode(lines, markup) {
		if !code {
			if cur != nil {
				out, cur = append(out, cur), nil
			}
			continue
		}
		if pre, text, ok := splitComment(lines[i]); ok {
			cur = append(cur, comment{i, pre, text})
		}
	}
	if cur != nil {
		out = append(out, cur)
	}
	return out
}

// stanzas splits a block's comments where a blank line falls between two of them.
func stanzas(block []comment, lines []string, markup bool) [][]comment {
	var out [][]comment
	var cur []comment
	for n, c := range block {
		if n > 0 && slices.ContainsFunc(lines[block[n-1].line+1:c.line], func(ln string) bool { return isBlank(ln, markup) }) {
			out, cur = append(out, cur), nil
		}
		cur = append(cur, c)
	}
	return append(out, cur)
}

func process(path string, write bool) ([]offender, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	// Line endings are normalized, so a CRLF file measures (and is rewritten) like an LF one.
	lines := strings.Split(strings.ReplaceAll(strings.ReplaceAll(string(data), "\r\n", "\n"), "\r", "\n"), "\n")
	markup := strings.HasSuffix(path, ".html")
	var offenders []offender
	dirty := false
	for _, block := range blocks(lines, markup) {
		if len(block) < 2 {
			continue
		}
		span := make([]int, len(block))
		for n, c := range block {
			span[n] = vis(c.pre, markup)
		}
		// One column for the whole example only when its lines are comparable in width; else align
		// each blank-separated stanza on its own, so two disparate command lists aren't dragged into
		// a single far-right column.
		groups := [][]comment{block}
		if slices.Max(span)-slices.Min(span) > outlierGap {
			groups = stanzas(block, lines, markup)
		}
		for _, grp := range groups {
			if len(grp) < 2 {
				continue
			}
			widths := make([]int, len(grp))
			columns := map[int]bool{}
			for n, c := range grp {
				widths[n] = vis(c.pre, markup)
				columns[vis(lines[c.line][:strings.Index(lines[c.line], c.comment)], markup)] = true
			}
			slices.Sort(widths)
			widest := widths[len(widths)-1]
			if widest-widths[len(widths)-2] > outlierGap {
				continue // a lone over-long line — leave it standalone
			}
			if len(columns) == 1 {
				continue // already one column — keep the author's gap, don't churn it
			}
			for _, c := range grp {
				offenders = append(offenders, offender{c.line + 1, strings.TrimRightFunc(lines[c.line], unicode.IsSpace)})
			}
			if write {
				for _, c := range grp {
					if fixed := c.pre + strings.Repeat(" ", widest+1-vis(c.pre, markup)) + c.comment; fixed != lines[c.line] {
						lines[c.line], dirty = fixed, true
					}
				}
			}
		}
	}
	if write && dirty {
		if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
			return nil, err
		}
	}
	return offenders, nil
}
