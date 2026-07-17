package ui

import (
	"os"
	"strings"
	"testing"
)

func TestColorWrappersContainInput(t *testing.T) {
	// Whether or not colors are enabled, the wrapped text must survive.
	for _, fn := range []func(string) string{Bold, Dim, Green, Red} {
		if !strings.Contains(fn("hello"), "hello") {
			t.Errorf("color wrapper dropped its input: %q", fn("hello"))
		}
	}
	if !strings.Contains(Check(), "✓") || !strings.Contains(Cross(), "✗") {
		t.Errorf("Check/Cross marks missing glyphs: %q %q", Check(), Cross())
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
