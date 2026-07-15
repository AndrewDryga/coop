package ui

import (
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
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

func TestSpinFramesAndFreeze(t *testing.T) {
	boxWant := []string{".[  ]", ">[  ]", "[.  ]", "[ * ]", "[  .]", "[  ]>", "[  ]."}
	compactWant := []string{"◰", "◳", "◲", "◱"}
	for _, tc := range []struct {
		name   string
		frames []string
		want   []string
		width  int
	}{
		{"Box Run", SpinFrames, boxWant, SpinnerWidth},
		{"Corner Run", CompactSpinFrames, compactWant, 1},
	} {
		if len(tc.frames) != len(tc.want) {
			t.Fatalf("%s frames = %v, want %v", tc.name, tc.frames, tc.want)
		}
		for i, frame := range tc.want {
			if tc.frames[i] != frame || visible(frame) != tc.width {
				t.Errorf("%s frame[%d] = %q (width %d), want %d-column %q", tc.name, i, tc.frames[i], visible(frame), tc.width, frame)
			}
		}
	}

	for _, value := range []string{"0", "false"} {
		t.Run("COOP_SPINNER="+value, func(t *testing.T) {
			t.Setenv("COOP_SPINNER", value)
			if SpinnerEnabled() {
				t.Errorf("COOP_SPINNER=%s should freeze animation", value)
			}
			for i := 0; i < len(SpinFrames)*2; i++ {
				if got := SpinFrame(i); got != boxWant[0] {
					t.Errorf("frozen SpinFrame(%d) = %q, want %q", i, got, boxWant[0])
				}
				if got := CompactSpinFrame(i); got != compactWant[0] {
					t.Errorf("frozen CompactSpinFrame(%d) = %q, want %q", i, got, compactWant[0])
				}
			}
		})
	}

	t.Setenv("COOP_SPINNER", "1")
	if got := SpinFrame(8); got != boxWant[1] {
		t.Errorf("animated SpinFrame(8) = %q, want %q", got, boxWant[1])
	}
	if got := CompactSpinFrame(5); got != compactWant[1] {
		t.Errorf("animated CompactSpinFrame(5) = %q, want %q", got, compactWant[1])
	}
	if got := SpinFrame(-1); got != boxWant[0] {
		t.Errorf("negative SpinFrame = %q, want %q", got, boxWant[0])
	}
	if got := CompactSpinFrame(-1); got != compactWant[0] {
		t.Errorf("negative CompactSpinFrame = %q, want %q", got, compactWant[0])
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
		r, size := utf8.DecodeRuneInString(s[i:])
		if r != '\r' {
			n++
		}
		i += size
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
	// Colors are off under `go test`, so every state segment renders as plain █. Assert the total
	// filled-cell layout here; the protected-segment test below distinguishes colors with sentinels.
	for _, c := range []struct {
		done, doing, blocked, total, w int
		want                           string
	}{
		{0, 0, 0, 0, 4, "[░░░░]"},        // no tasks → empty
		{1, 1, 1, -1, 4, "[░░░░]"},       // non-positive total → empty
		{1, 1, 1, 3, 0, "[]"},            // zero width → no cells
		{1, 1, 1, 3, -2, "[]"},           // negative width clamps to zero
		{-1, -1, -1, 3, 4, "[░░░░]"},     // negative counts cannot create segments
		{2, 0, 0, 4, 4, "[██░░]"},        // half done
		{0, 2, 0, 4, 4, "[██░░]"},        // half in progress
		{2, 1, 1, 4, 4, "[████]"},        // all non-todo states fill
		{5, 1, 5, 10, 4, "[████]"},       // segments cannot exceed the width
		{2, 0, 0, 4, 10, "[█████░░░░░]"}, // 2/4 done scaled to width 10
	} {
		if got := ProgressBarStates(c.done, c.doing, c.blocked, c.total, c.w); got != c.want {
			t.Errorf("ProgressBarStates(d=%d,a=%d,b=%d,t=%d,w=%d) = %q, want %q", c.done, c.doing, c.blocked, c.total, c.w, got, c.want)
		}
	}
}

func TestProgressBarStatesLiveSegmentsNeverHidden(t *testing.T) {
	// Force sentinels so the state segments stay distinguishable without a TTY.
	saved := [4]string{cCyan, cYellow, cRed, cReset}
	cCyan, cYellow, cRed, cReset = "<c>", "<y>", "<r>", "</>"
	defer func() { cCyan, cYellow, cRed, cReset = saved[0], saved[1], saved[2], saved[3] }()

	seg := func(cyan, yellow, red, empty int) string {
		return "[" + Cyan(strings.Repeat("█", cyan)) + Yellow(strings.Repeat("█", yellow)) + Red(strings.Repeat("█", red)) + strings.Repeat("░", empty) + "]"
	}
	for _, c := range []struct {
		done, doing, blocked, total, w int
		want                           string
	}{
		// The reported 24 done / 1 active / 1 todo queue keeps one yellow cell.
		{24, 1, 0, 26, 22, seg(20, 1, 0, 1)},
		// A tiny active share that rounds to zero still surfaces one yellow cell.
		{40, 1, 0, 100, 14, seg(6, 1, 0, 7)},
		// A lone blocker still steals one cell from a near-complete done segment.
		{57, 0, 1, 58, 14, seg(13, 0, 1, 0)},
		// Both protected states remain visible; done yields the rounding overflow.
		{57, 1, 1, 59, 14, seg(12, 1, 1, 0)},
		// A dominant blocked share still reserves yellow before proportional allocation.
		{0, 1, 99, 100, 14, seg(0, 1, 13, 0)},
		// A dominant active share likewise cannot hide a real blocker.
		{0, 99, 1, 100, 14, seg(0, 13, 1, 0)},
		// One cell can show active work when no blocker competes for it.
		{0, 1, 0, 2, 1, seg(0, 1, 0, 0)},
		// A one-cell bar cannot show both; preserve the existing blocker priority.
		{0, 1, 1, 2, 1, seg(0, 0, 1, 0)},
	} {
		if got := ProgressBarStates(c.done, c.doing, c.blocked, c.total, c.w); got != c.want {
			t.Errorf("ProgressBarStates(d=%d,a=%d,b=%d,t=%d,w=%d) = %q, want %q", c.done, c.doing, c.blocked, c.total, c.w, got, c.want)
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
