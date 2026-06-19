package ui

import (
	"strings"
	"testing"
)

func TestClip(t *testing.T) {
	if got := clip("hello world", 5); got != "hello" {
		t.Errorf("clip plain = %q, want %q", got, "hello")
	}
	if got := clip("hi", 10); got != "hi" {
		t.Errorf("clip short = %q, want %q", got, "hi")
	}
	if got := clip("anything", 0); got != "" {
		t.Errorf("clip to 0 = %q, want empty", got)
	}
	// ANSI escapes don't count toward width and a cut inside styled text gets a reset.
	got := clip("\033[32mhello\033[0m world", 3)
	if !strings.HasPrefix(got, "\033[32mhel") {
		t.Errorf("clip styled lost color/text: %q", got)
	}
	if !strings.HasSuffix(got, "\033[0m") {
		t.Errorf("clip styled should reset when cutting mid-style: %q", got)
	}
	if visible(got) != 3 {
		t.Errorf("clip styled visible width = %d, want 3 (%q)", visible(got), got)
	}
}

// visible counts runes outside ANSI escapes, for asserting clip's width.
func visible(s string) int {
	n := 0
	for i := 0; i < len(s); {
		if s[i] == '\033' {
			for i < len(s) && !(s[i] >= '@' && s[i] <= '~' && s[i] != '[') {
				i++
			}
			if i < len(s) {
				i++
			}
			continue
		}
		n++
		i++
	}
	return n
}

func TestProgressBar(t *testing.T) {
	// Colors are off under `go test`, so the bar is plain blocks.
	for _, c := range []struct {
		frac float64
		w    int
		want string
	}{
		{0, 4, "[░░░░]"},
		{1, 4, "[████]"},
		{0.5, 10, "[█████░░░░░]"},
		{2, 4, "[████]"},  // clamped high
		{-1, 4, "[░░░░]"}, // clamped low
	} {
		if got := ProgressBar(c.frac, c.w); got != c.want {
			t.Errorf("ProgressBar(%v,%d) = %q, want %q", c.frac, c.w, got, c.want)
		}
	}
}

func TestRegion(t *testing.T) {
	var buf strings.Builder
	r := NewRegion(&buf, func() int { return 40 })

	// First paint: no cursor-up (nothing drawn yet); history then the bar appear.
	r.Update("hello", []string{"BAR"})
	if s := buf.String(); !strings.Contains(s, "hello") || !strings.Contains(s, "BAR") {
		t.Fatalf("first update missing content: %q", s)
	}
	if strings.Contains(buf.String(), "\033[1A") {
		t.Errorf("first paint should not move the cursor up: %q", buf.String())
	}

	// A refresh of a 1-line region erases (\r\033[J) and repaints, no cursor-up.
	buf.Reset()
	r.Update("", []string{"BAR2"})
	if s := buf.String(); !strings.Contains(s, "\033[J") || !strings.Contains(s, "BAR2") {
		t.Errorf("refresh should erase and repaint: %q", s)
	}

	// Region lines are clipped to the width so they never wrap.
	buf.Reset()
	r.Update("", []string{strings.Repeat("x", 100)})
	if got := visible(buf.String()); got > 40 {
		t.Errorf("region line not clipped to width: visible=%d (%q)", got, buf.String())
	}

	// Clear erases.
	buf.Reset()
	r.Clear()
	if !strings.Contains(buf.String(), "\033[J") {
		t.Errorf("clear should erase the region: %q", buf.String())
	}
}
