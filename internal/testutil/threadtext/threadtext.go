// Package threadtext checks a message Coop writes into an editor thread against
// .agent/kb/rules/acp-thread-messages-are-structured.md.
package threadtext

import (
	"regexp"
	"strings"
	"testing"
)

var (
	title   = regexp.MustCompile(`^(⚠ )?\*\*[^*]+\*\*`)
	command = regexp.MustCompile(`(^|[^\w/.-])coop [a-z]`)
)

// Check fails t unless text has its titles alone on their lines, every command to run in a code block
// of its own, each block set apart by blank lines so that it renders as one, and no sentence in a block:
// an editor does not wrap one, so the sentence runs off the panel's edge.
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
			if strings.HasSuffix(line, ".") || strings.HasSuffix(line, ":") {
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
