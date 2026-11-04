// Package threadtext checks a message Coop writes into an editor thread against
// .agent/kb/rules/acp-thread-messages-are-structured.md.
package threadtext

import (
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
)

var (
	title     = regexp.MustCompile(`^(⚠ )?\*\*[^*]+\*\*`)
	command   = regexp.MustCompile(`(^|[^\w/.-])coop [a-z]`)
	plainWord = regexp.MustCompile(`^[A-Za-z]+[,;:]?$`)
)

func longProse(line string) bool {
	words := 0
	for _, word := range strings.Fields(line) {
		if plainWord.MatchString(word) {
			words++
		}
	}
	return utf8.RuneCountInString(line) > 60 && words > 6
}

// Check fails t unless text has its titles alone on their lines, every command to run in a code block
// of its own, each block set apart by blank lines so that it renders as one, and no sentence in a block:
// an editor does not wrap one, so the sentence runs off the panel's edge. A sentence need not end in a
// stop, so a long line of many plain words counts as one too; a long command has few among its flags.
func Check(t testing.TB, text string) {
	t.Helper()
	fenced := false
	lines := strings.Split(strings.Trim(text, "\n"), "\n")
	blank := func(i int) bool { return i < 0 || i >= len(lines) || lines[i] == "" }
	for i, line := range lines {
		if strings.HasPrefix(line, "```") {
			if !fenced && !blank(i-1) || fenced && !blank(i+1) {
				t.Errorf("a code block needs a blank line on each side:\n%s", text)
			}
			fenced = !fenced
			continue
		}
		if fenced {
			if strings.HasSuffix(line, ".") || strings.HasSuffix(line, ":") || longProse(line) {
				t.Errorf("a code block holds only what to copy, not a sentence: %q in\n%s", line, text)
			}
			continue
		}
		if heading := title.FindString(line); heading != "" && (heading != line || !blank(i+1)) {
			t.Errorf("a title goes alone on its line, then a blank line: %q in\n%s", line, text)
		}
		if command.MatchString(line) {
			t.Errorf("a command to run goes in a code block: %q in\n%s", line, text)
		}
	}
	if fenced {
		t.Errorf("a code block never closes:\n%s", text)
	}
}
