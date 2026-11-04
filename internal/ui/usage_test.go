package ui

import (
	"strings"
	"testing"
)

// A cause can quote a file name or a runtime's error, and a box could have chosen either, so no value
// in the block reaches a terminal with its control characters intact.
func TestUsageErrorRenderEscapesControlCharacters(t *testing.T) {
	const hostile = "a\x1b[31mb"
	out := (&UsageError{
		Headline: hostile,
		Cause:    "first " + hostile + "\nsecond " + hostile,
		Choices:  []string{hostile},
		Rows:     [][2]string{{"Label:", hostile}, {Continuation, hostile}, {"", hostile}},
	}).Render(Palette{})
	if strings.Contains(out, "\x1b") {
		t.Errorf("the block printed a control character:\n%q", out)
	}
	if want := `a\u001B[31mb`; strings.Count(out, want) != 7 {
		t.Errorf("want every value escaped as %q:\n%s", want, out)
	}
	if !strings.Contains(out, "      second ") {
		t.Errorf("the cause lost its own line:\n%s", out)
	}
}

func TestOptionNeedsAnotherNamesBothOptions(t *testing.T) {
	got := OptionNeedsAnother("--yes", "--fresh", "coop fork").Render(Palette{})
	want := "\n✗ Option \"--yes\" needs \"--fresh\" in \"coop fork\"\n\n  Help: coop help fork\n"
	if got != want {
		t.Errorf("block = %q, want %q", got, want)
	}
}

// The other blocks print the same kinds of value, so they escape them the same way. A line break and
// a tab stay: they are how a block lays out what it prints.
func TestBlocksEscapeControlCharacters(t *testing.T) {
	const hostile = "a\x1b[31mb"
	for name, out := range map[string]string{
		"alert":   AlertBlock(Palette{}, hostile, hostile, [2]string{"Label:", hostile}),
		"failure": FailureBlock(Palette{}, hostile, hostile, [2]string{"Label:", hostile}),
		"warning": captureStderr(t, func() { Warning(hostile, hostile, hostile) }),
		"fail":    captureStderr(t, func() { Fail(hostile, hostile, hostile) }),
	} {
		if strings.Contains(out, hostile) {
			t.Errorf("%s printed a control character: %q", name, out)
		}
		if got := strings.Count(out, `a\u001B[31mb`); got != 3 {
			t.Errorf("%s escaped %d values, want 3: %q", name, got, out)
		}
	}
	if got, want := safeText("a\tb\nc\x1b"), "a\tb\nc\\u001B"; got != want {
		t.Errorf("safeText = %q, want %q", got, want)
	}
}
