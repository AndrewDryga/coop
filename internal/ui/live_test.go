package ui

import (
	"regexp"
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

// visible counts runes outside ANSI escapes, for asserting clip's width. A carriage return is
// cursor motion (Region parks the cursor with one), not a visible cell.
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
		if s[i] != '\r' {
			n++
		}
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

func TestProgressBarStates(t *testing.T) {
	// Colors off under `go test` — done (cyan) and blocked (red) both render as plain █, so assert the
	// cell layout: done + blocked fill, the rest is empty.
	for _, c := range []struct {
		done, blocked, total, w int
		want                    string
	}{
		{0, 0, 0, 4, "[░░░░]"},        // no tasks → empty
		{2, 0, 4, 4, "[██░░]"},        // half done, none blocked
		{2, 2, 4, 4, "[████]"},        // 2 done + 2 blocked → full
		{0, 4, 4, 4, "[████]"},        // all blocked → full
		{5, 5, 10, 4, "[████]"},       // done+blocked can't exceed the width
		{2, 0, 4, 10, "[█████░░░░░]"}, // 2/4 done scaled to width 10
	} {
		if got := ProgressBarStates(c.done, c.blocked, c.total, c.w); got != c.want {
			t.Errorf("ProgressBarStates(d=%d,b=%d,t=%d,w=%d) = %q, want %q", c.done, c.blocked, c.total, c.w, got, c.want)
		}
	}
}

func TestProgressBarStatesBlockedNeverHidden(t *testing.T) {
	// Colors are off under `go test`, so done (cyan) and blocked (red) both render as plain █ — a
	// rounded-away blocker would be invisible in the string. Force sentinels so the two segments are
	// distinct, then prove a non-zero blocked count always keeps at least one red cell.
	saved := [3]string{cCyan, cRed, cReset}
	cCyan, cRed, cReset = "<c>", "<r>", "</>"
	defer func() { cCyan, cRed, cReset = saved[0], saved[1], saved[2] }()

	seg := func(cyan, red, empty int) string {
		return "[" + Cyan(strings.Repeat("█", cyan)) + Red(strings.Repeat("█", red)) + strings.Repeat("░", empty) + "]"
	}
	for _, c := range []struct {
		done, blocked, total, w int
		want                    string
	}{
		// 57/58 done, 1 blocked, w14 — the real board line: done alone rounds up to a full bar, but
		// the blocker steals one cell from done (13 cyan + 1 red, no empty) instead of vanishing.
		{57, 1, 58, 14, seg(13, 1, 0)},
		// A blocker that rounds to zero far from full still surfaces one red cell, out of the empties.
		{40, 1, 100, 14, seg(6, 1, 7)},
	} {
		if got := ProgressBarStates(c.done, c.blocked, c.total, c.w); got != c.want {
			t.Errorf("ProgressBarStates(d=%d,b=%d,t=%d,w=%d) = %q, want %q", c.done, c.blocked, c.total, c.w, got, c.want)
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
	// Every paint parks the cursor at column 0: a Ctrl-C echoed as ^C then overtypes the bar's
	// first cells instead of extending its last line past the final column, where the wrap would
	// desync the erase math and leak stale bar frames into scrollback.
	if !strings.HasSuffix(buf.String(), "\r") {
		t.Errorf("paint must park the cursor at column 0: %q", buf.String())
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

// An identical frame must not repaint — a static dashboard (nothing running, so no spinner) then
// sits still instead of flickering every poll; any change (or a width change) still repaints.
func TestAltScreenSkipsUnchangedFrame(t *testing.T) {
	var buf strings.Builder
	s := NewAltScreen(&buf, func() int { return 40 })
	s.Frame([]string{"a", "b"})
	if buf.Len() == 0 {
		t.Fatal("first frame should paint")
	}
	buf.Reset()
	s.Frame([]string{"a", "b"}) // identical → skip
	if buf.Len() != 0 {
		t.Errorf("an unchanged frame must not repaint, wrote %q", buf.String())
	}
	buf.Reset()
	s.Frame([]string{"a", "c"}) // changed → repaint
	if buf.Len() == 0 {
		t.Error("a changed frame must repaint")
	}
}

func TestAltScreen(t *testing.T) {
	var buf strings.Builder
	s := NewAltScreen(&buf, func() int { return 40 })

	s.Enter()
	if !strings.Contains(buf.String(), "\033[?1049h") {
		t.Errorf("Enter should switch to the alternate buffer: %q", buf.String())
	}

	// A frame homes the cursor, draws each line top-down, and clears below — no cursor-up math, so
	// it can't orphan the header into scrollback the way a bottom-pinned region does when too tall.
	buf.Reset()
	s.Frame([]string{"HEADER", "row one", "BAR"})
	out := buf.String()
	if !strings.HasPrefix(out, "\033[H") {
		t.Errorf("frame should home the cursor first: %q", out)
	}
	if !strings.HasSuffix(out, "\033[J") {
		t.Errorf("frame should clear leftover lines below: %q", out)
	}
	if regexp.MustCompile("\033\\[[0-9]*A").MatchString(out) { // never a cursor-up (\033[<n>A) — that's the orphaning the alt screen avoids
		t.Errorf("frame should not use cursor-up: %q", out)
	}
	for _, want := range []string{"HEADER", "row one", "BAR"} {
		if !strings.Contains(out, want) {
			t.Errorf("frame missing %q: %q", want, out)
		}
	}

	// Lines are clipped to the width so they never wrap and desync the repaint.
	buf.Reset()
	s.Frame([]string{strings.Repeat("x", 100)})
	if got := visible(buf.String()); got > 40 {
		t.Errorf("frame line not clipped to width: visible=%d (%q)", got, buf.String())
	}

	// Leave restores the main buffer (and the cursor).
	buf.Reset()
	s.Leave()
	if !strings.Contains(buf.String(), "\033[?1049l") {
		t.Errorf("Leave should restore the main buffer: %q", buf.String())
	}
}
