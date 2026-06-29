package ui

import (
	"strings"
	"testing"
)

// With a live sink set, ui status lines route through it (so the loop bar can scroll them above
// itself) instead of straight to stderr — and the trailing newline is trimmed (the region positions
// lines itself). nil restores the default.
func TestLiveSinkRoutesOutput(t *testing.T) {
	var got []string
	SetLiveSink(func(s string) { got = append(got, s) })
	defer SetLiveSink(nil)

	Info("hello %d", 1)
	OK("done")

	if len(got) != 2 {
		t.Fatalf("expected 2 lines through the sink, got %d: %v", len(got), got)
	}
	if !strings.Contains(got[0], "coop:") || !strings.Contains(got[0], "hello 1") {
		t.Errorf("Info not routed through the sink: %q", got[0])
	}
	if strings.HasSuffix(got[0], "\n") {
		t.Errorf("sink line should have its trailing newline trimmed: %q", got[0])
	}
}
