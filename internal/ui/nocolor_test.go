package ui

import (
	"os"
	"testing"
)

// TestColorEnabledHonorsNoColor: NO_COLOR (the no-color.org convention) disables color regardless
// of TTY, and a Palette built under it is off — so `NO_COLOR=1 coop tasks ls` stays plain text.
func TestColorEnabledHonorsNoColor(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	if colorEnabled(os.Stdout) {
		t.Error("colorEnabled must be false when NO_COLOR is set")
	}
	if For(os.Stdout).Enabled() {
		t.Error("ui.For(...).Enabled() must be false when NO_COLOR is set")
	}
}
