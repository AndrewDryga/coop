package cli

import (
	"strings"
	"testing"
)

// TestSplitFrontmatterSkipsLeadingComment: `coop tasks add` seeds a `<!-- … -->` header before the
// `---` fence, so the parser must skip it — otherwise a seeded task's title/status/labels go unparsed
// and the `status:` lint guard silently dies.
func TestSplitFrontmatterSkipsLeadingComment(t *testing.T) {
	content := "<!-- Task spec — work from this file alone.\n     ref: README -->\n---\nid: x\n" +
		"title: \"[DEAD] thing\"\nstatus: todo\n---\n\n# [DEAD] thing\nbody\n"
	fields, body := splitFrontmatter(content)
	if fields["title"] != "[DEAD] thing" {
		t.Errorf("title = %q, want [DEAD] thing (unquoted, parsed past the comment)", fields["title"])
	}
	if fields["status"] != "todo" {
		t.Errorf("status = %q, want todo", fields["status"])
	}
	if strings.Contains(body, "<!--") || !strings.Contains(body, "# [DEAD] thing") {
		t.Errorf("body should be the content after the fence, no comment: %q", body)
	}
	// regression: a plain `---`-at-line-0 file (no comment) still parses.
	if f2, _ := splitFrontmatter("---\ntitle: plain\n---\nbody\n"); f2["title"] != "plain" {
		t.Errorf("no-comment frontmatter regressed: %q", f2["title"])
	}
}

// TestSanitizeCell: a task title/decision can be agent-authored, so control chars / ANSI must be
// stripped before printing — no escape injection into a pipe, no spoofing the colored UI on a TTY.
func TestSanitizeCell(t *testing.T) {
	if got := sanitizeCell("evil\x1b[31mRED\x1b[0m\ttitle"); strings.ContainsAny(got, "\x1b\t") {
		t.Errorf("sanitizeCell kept a control char: %q", got)
	}
	if got := sanitizeCell("a normal — title 99"); got != "a normal — title 99" {
		t.Errorf("sanitizeCell altered a clean string: %q", got)
	}
}
