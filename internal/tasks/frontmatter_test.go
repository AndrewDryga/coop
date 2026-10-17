package tasks

import (
	"path/filepath"
	"strings"
	"testing"
	"unicode"
)

// TestSplitFrontmatterSkipsLeadingComment: `coop tasks add` seeds a `<!-- … -->` header before the
// `---` fence, so the parser must skip it — otherwise a seeded task's title/status/labels go unparsed
// and the `status:` lint guard silently dies.
func TestSplitFrontmatterSkipsLeadingComment(t *testing.T) {
	content := "<!-- Task spec — work from this file alone.\n     ref: README -->\n---\nid: x\n" +
		"title: \"[DEAD] thing\"\nstatus: todo\n---\n\n# [DEAD] thing\nbody\n"
	fields, body := SplitFrontmatter(content)
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
	if f2, _ := SplitFrontmatter("---\ntitle: plain\n---\nbody\n"); f2["title"] != "plain" {
		t.Errorf("no-comment frontmatter regressed: %q", f2["title"])
	}
}

// TestSanitizeCell: a task title/decision can be agent-authored, so whatever a terminal would act on
// or a reader could not see is dropped before printing — no escape injection into a pipe, no
// spoofing the colored UI on a TTY, no row turned around or split, nothing hidden in a title — and
// ordinary text stays as it is.
func TestSanitizeCell(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"plain title", "a normal — title 99", "a normal — title 99"},
		{"accents, CJK, Hangul and emoji", "café e\u0301 日本語 한국어 ❤\uFE0F 👍🏽 🇺🇦 1\uFE0F\u20E3 葛\U000E0100", "café e\u0301 日本語 한국어 ❤\uFE0F 👍🏽 🇺🇦 1\uFE0F\u20E3 葛\U000E0100"},
		{"escape sequence and tab", "evil\x1b[31mRED\x1b[0m\ttitle", "evil[31mRED[0mtitle"},
		{"new line and carriage return", "a\nb\rc", "abc"},
		{"C1 controls and DEL", "a\u009Bb\u0085c\x7f", "abc"},
		{"direction marks, overrides and isolates", "a\u061Cb\u200Ec\u200F\u202Ad\u202Ee\u2066f\u2069", "abcdef"},
		{"zero-width space", "w\u200Beb", "web"},
		{"zero-width non-joiner and joiner", "a\u200Cb 👨\u200D👩\u200D👧", "ab 👨👩👧"},
		{"byte-order mark and soft hyphen", "\uFEFFwe\u00ADb", "web"},
		{"tag characters", "web\U000E0041\U000E0000", "web"},
		{"line and paragraph separators", "a\u2028b\u2029c", "abc"},
		{"Hangul fillers and grapheme joiner", "w\u1160e\u3164b\u034F", "web"},
		{"variation selector at the start and after an ASCII letter", "\uFE0Ew\uFE0Feb", "web"},
		{"variation selector after a letter once a zero-width space is dropped", "w\u200B\uFE0Feb", "web"},
		{"invalid UTF-8", "a\xffb", "a\uFFFDb"},
	} {
		if got := sanitizeCell(tc.in); got != tc.want {
			t.Errorf("%s: sanitizeCell(%+q) = %+q, want %+q", tc.name, tc.in, got, tc.want)
		}
	}
	// Every character of the dropped classes, not only the ones above, is dropped.
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if !unicode.In(r, unicode.Cc, unicode.Cf, unicode.Zl, unicode.Zp, unicode.Other_Default_Ignorable_Code_Point) {
			continue
		}
		if got := sanitizeCell("a" + string(r) + "b"); got != "ab" {
			t.Errorf("sanitizeCell(%U between two letters) = %+q, want %q", r, got, "ab")
		}
	}
}

// A box writes task.md, so titles holding control characters, direction overrides, line
// separators and invisible characters list exactly like the plain titles: the approved listing,
// byte for byte.
func TestTasksListDropsHiddenCharactersFromTitles(t *testing.T) {
	root := t.TempDir()
	writeTaskFile(t, filepath.Join(root, StateInProgress, "2026-09-11-fix-login-retries", "task.md"),
		"# Fix lo\u202Egin\u2066 retries\u2069\n\n## Subtasks\n- [x] one\n- [x] two\n- [ ] three\n- [ ] four\n")
	writeTaskFile(t, filepath.Join(root, StateTodo, "2026-09-11-add-password-reset", "task.md"), "# Add a pass\u200Bword\u00AD reset li\u2028nk\n")
	writeTaskFile(t, filepath.Join(root, StateBlocked, "2026-09-11-choose-session-duration", "task.md"), "# \uFEFFChoose how\u009B long sessions\u2029 last\x7f\n")
	writeTaskFile(t, filepath.Join(root, StateBlocked, "2026-09-11-choose-session-duration", "decision.md"), "# Decision: How long should a session last?\n")
	writeTaskFile(t, filepath.Join(root, StateDone, "2026-09-10-show-sign-in-errors", "task.md"), "# Show\u1160 sign-in\uFE0F errors\U000E0041\x1b\n")
	assertApprovedOutput(t, "36-normal-listing", captureStdout(t, func() { _, _ = tasksFolderList(root, false) }))
}
