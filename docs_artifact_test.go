package main

import (
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// docArtifactRe matches an AI file-write wrapper tag left in tracked docs — the `</content>` that
// shipped at the end of README.md from 2026-06-16. It is deliberately NARROW: a whole-line,
// anchored match on the bare `<content>`/`</content>` tag only, so ordinary prose, inline code that
// merely mentions the word, and gendocs-generated tables full of `<name>`-style angle-bracket
// placeholders and fenced code never trip it. Another wrapper tag leaks? Add it here with a paired
// fixture case below — don't broaden this into an unbalanced-fence heuristic (too false-positive-prone
// for the gate).
var docArtifactRe = regexp.MustCompile(`^\s*</?content>\s*$`)

// TestDocsArtifactDetector pins the guard's shape: it fires on the wrapper tag on its own line and
// not on real markdown, inline code, or angle-bracket placeholders.
func TestDocsArtifactDetector(t *testing.T) {
	for _, s := range []string{"</content>", "<content>", "  </content>  ", "\t<content>"} {
		if !docArtifactRe.MatchString(s) {
			t.Errorf("docArtifactRe should flag the wrapper tag %q", s)
		}
	}
	for _, s := range []string{
		"the run wraps output in `</content>` tags", // inline code mentioning it
		"<content>real text</content>",              // has content between — not a bare stub
		"usage: coop fork <name> | ls | rm",         // angle-bracket placeholders
		"| `--content` | a flag |",                  // table cell
		"```",                                       // a code fence
	} {
		if docArtifactRe.MatchString(s) {
			t.Errorf("docArtifactRe should NOT flag %q", s)
		}
	}
}

// TestNoDocArtifactsInTrackedMarkdown graduates the check into the gate: every tracked *.md is
// scanned so an AI-wrapper artifact can't ship in docs again (README, CHANGELOG, site, rules, …).
func TestNoDocArtifactsInTrackedMarkdown(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	out, err := exec.Command("git", "ls-files", "*.md").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	for _, f := range strings.Fields(string(out)) {
		data, err := os.ReadFile(f)
		if err != nil {
			continue // listed but absent (e.g. mid-rename) — not our concern
		}
		for i, line := range strings.Split(string(data), "\n") {
			if docArtifactRe.MatchString(line) {
				t.Errorf("%s:%d ships an AI-wrapper artifact: %q — delete it", f, i+1, strings.TrimSpace(line))
			}
		}
	}
}
