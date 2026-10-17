package ui

import (
	"os"
	"strings"
	"testing"
	"unicode"
)

// A box chooses the names SafeInline prints, so a character that can act on the terminal, break the
// line or hide inside a name comes out as its code, and ordinary text stays as it is.
func TestSafeInline(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"plain text", "web/app.go", "web/app.go"},
		{"accents, composed and combining", "café naïve e\u0301", "café naïve e\u0301"},
		{"CJK and Hangul", "日本語 한국어", "日本語 한국어"},
		{"emoji, variation selector, skin tone, flag and keycap", "🙂 ❤\uFE0F 👍🏽 🇺🇦 1\uFE0F\u20E3", "🙂 ❤\uFE0F 👍🏽 🇺🇦 1\uFE0F\u20E3"},
		{"invalid UTF-8", "a\xffb", "a\uFFFDb"},
		{"escape sequence", "x\x1b[2Jy", `x\u001B[2Jy`},
		{"new line and carriage return", "a\nb\rc", `a\u000Ab\u000Dc`},
		{"C1 control and DEL", "a\u009B2Jb\x7f", `a\u009B2Jb\u007F`},
		{"direction marks, overrides and isolates", "\u061C\u200E\u200F\u202A\u202E\u2066\u2069", `\u061C\u200E\u200F\u202A\u202E\u2066\u2069`},
		{"zero-width space", "w\u200Beb", `w\u200Beb`},
		{"zero-width non-joiner, which Persian words also use", "a\u200Cb", `a\u200Cb`},
		{"zero-width joiner inside a family emoji", "👨\u200D👩\u200D👧", `👨\u200D👩\u200D👧`},
		{"byte-order mark", "\uFEFFweb", `\uFEFFweb`},
		{"soft hyphen", "we\u00ADb", `we\u00ADb`},
		{"tag character", "web\U000E0041", `web\U000E0041`},
		{"England's flag, made of tag characters", "🏴\U000E0067\U000E0062\U000E0065\U000E006E\U000E0067\U000E007F", `🏴\U000E0067\U000E0062\U000E0065\U000E006E\U000E0067\U000E007F`},
		{"unassigned tag-block code point", "web\U000E0000", `web\U000E0000`},
		{"line separator", "a\u2028b", `a\u2028b`},
		{"paragraph separator", "a\u2029b", `a\u2029b`},
		{"Hangul filler", "w\u1160eb", `w\u1160eb`},
		{"combining grapheme joiner", "w\u034Feb", `w\u034Feb`},
		{"variation selector after an ASCII letter", "w\uFE0Feb", `w\uFE0Feb`},
		{"variation selector at the start", "\uFE0Eweb", `\uFE0Eweb`},
		{"ideographic variation selector after a kanji", "葛\U000E0100", "葛\U000E0100"},
	} {
		if got := SafeInline(tc.in); got != tc.want {
			t.Errorf("%s: SafeInline(%+q) = %+q, want %+q", tc.name, tc.in, got, tc.want)
		}
	}
	// Every character of the escaped classes, not only the ones above, comes out as printable ASCII.
	for _, table := range []*unicode.RangeTable{unicode.Cc, unicode.Cf, unicode.Zl, unicode.Zp, unicode.Other_Default_Ignorable_Code_Point} {
		for _, r16 := range table.R16 {
			for r := rune(r16.Lo); r <= rune(r16.Hi); r += rune(r16.Stride) {
				assertPrintableASCII(t, r)
			}
		}
		for _, r32 := range table.R32 {
			for r := rune(r32.Lo); r <= rune(r32.Hi); r += rune(r32.Stride) {
				assertPrintableASCII(t, r)
			}
		}
	}
}

func assertPrintableASCII(t *testing.T, r rune) {
	t.Helper()
	if got := SafeInline(string(r)); strings.IndexFunc(got, func(c rune) bool { return c < ' ' || c > '~' }) >= 0 {
		t.Errorf("SafeInline(%U) = %+q, want printable ASCII", r, got)
	}
}

func TestColorWrappersContainInput(t *testing.T) {
	// Whether or not colors are enabled, the wrapped text must survive.
	for _, fn := range []func(string) string{Bold, Dim, Green, Red} {
		if !strings.Contains(fn("hello"), "hello") {
			t.Errorf("color wrapper dropped its input: %q", fn("hello"))
		}
	}
}

func TestIsTerminalOnRegularFile(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "f")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if IsTerminal(f) {
		t.Error("a regular file must not be reported as a terminal")
	}
}

// /dev/null is a character device but not a tty — the old ModeCharDevice check
// wrongly called it a terminal, which made docker -it fail.
func TestIsTerminalOnDevNull(t *testing.T) {
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Skip("no /dev/null")
	}
	defer f.Close()
	if IsTerminal(f) {
		t.Error("/dev/null must not be reported as a terminal")
	}
}

func TestIsTerminalNil(t *testing.T) {
	if IsTerminal(nil) {
		t.Error("nil file must not be a terminal")
	}
}

func TestPaletteLink(t *testing.T) {
	on := Palette{on: true}
	if got, want := on.Link("file:///a/b", "id"), "\x1b]8;;file:///a/b\x1b\\id\x1b]8;;\x1b\\"; got != want {
		t.Errorf("Link(on) = %q, want %q", got, want)
	}
	// A pipe (color off) and an empty uri both fall back to plain text — no escapes.
	if got := (Palette{on: false}).Link("file:///a/b", "id"); got != "id" {
		t.Errorf("Link(off) = %q, want plain %q", got, "id")
	}
	if got := on.Link("", "id"); got != "id" {
		t.Errorf("Link(empty uri) = %q, want plain %q", got, "id")
	}
}
