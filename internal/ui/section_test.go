package ui

import (
	"context"
	"errors"
	"testing"
)

// A launch section is a bold, unprefixed heading with indented results; a failed one ends with
// the red headline, a blank line, the reason six spaces further in, a blank line, and the remedy
// one level under the section. Off a terminal the bytes carry no ANSI at all.
func TestSectionShapes(t *testing.T) {
	got := captureStderr(t, func() {
		Section("Checking the Coop box")
		Note("  Current image was built by Coop v1")
		Pass("Box updated")
		Caution("Unrestricted — nothing is blocked")
		Fail("Could not update the box", "Docker is unavailable.", "Start Docker, then run 'coop codex' again.")
		Fail("Could not start Codex", "first line\nsecond line", "")
	})
	want := "\nChecking the Coop box\n" +
		"  Current image was built by Coop v1\n" +
		"  ✓ Box updated\n" +
		"  ⚠ Unrestricted — nothing is blocked\n" +
		"  ✗ Could not update the box\n" +
		"\n" +
		"        Docker is unavailable.\n" +
		"\n" +
		"    Start Docker, then run 'coop codex' again.\n" +
		"  ✗ Could not start Codex\n" +
		"\n" +
		"        first line\n" +
		"        second line\n"
	if got != want {
		t.Fatalf("sections rendered:\n%q\nwant:\n%q", got, want)
	}
}

// Reported marks an error as already shown without hiding what it was: the dispatcher can skip
// its fallback line while a cancellation is still a cancellation to errors.Is.
func TestReportedErrorKeepsItsCause(t *testing.T) {
	if Reported(nil) != nil {
		t.Fatal("Reported(nil) must stay nil")
	}
	err := Reported(context.Canceled)
	if !errors.Is(err, ErrReported) || !errors.Is(err, context.Canceled) {
		t.Fatalf("reported error lost a side: reported=%v canceled=%v", errors.Is(err, ErrReported), errors.Is(err, context.Canceled))
	}
	if err.Error() != context.Canceled.Error() {
		t.Fatalf("Error() = %q, want the cause's own message", err.Error())
	}
	if unmarked := errors.New("plain"); errors.Is(unmarked, ErrReported) {
		t.Fatal("an unmarked error must not read as reported")
	}
}
