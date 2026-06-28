package cli

import (
	"os"
	"path/filepath"
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

// TestLegacyTasksMigrationHint: a coop-v2 repo (a `.agent/TASKS.md`, no `.agent/tasks/`) must be
// pointed at MIGRATING.md — not told to `coop init`, which would scaffold an empty queue beside the
// populated legacy file and read as "v3 ate my tasks". Guards the #1 v3 upgrade-breakage risk.
func TestLegacyTasksMigrationHint(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".agent", "TASKS.md"), []byte("## Active\n- [ ] old task\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, sub := range [][]string{{"list"}, {"add", "x"}} {
		if _, err := appFor(repo).cmdTasks(sub); err == nil || !strings.Contains(err.Error(), "MIGRATING.md") {
			t.Errorf("`coop tasks %v` on a v2 repo should point at MIGRATING.md; got %v", sub, err)
		}
	}
	if isTaskDir(filepath.Join(repo, ".agent", "tasks")) {
		t.Error("a command on a v2 repo must NOT bootstrap an empty .agent/tasks queue")
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
