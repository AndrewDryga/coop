package ui

import (
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"
)

// SpinFrames is the braille spinner cycle used by the live displays.
var SpinFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// Region owns a block of lines pinned to the bottom of a terminal and repaints them in place,
// so a status display updates without scrolling away. Update scrolls optional history lines
// into the scrollback above the region, then redraws the region; Clear erases it and ends it.
// It is the primitive behind the loop's live progress bar. Methods are safe for concurrent
// use. Callers build a Region only when the target is a real terminal.
type Region struct {
	w         io.Writer
	width     func() int
	mu        sync.Mutex
	shown     int      // region lines currently on screen
	closed    bool     // Clear ran — the terminal is line-oriented again, never repaint
	lastLines []string // the last region content painted — skip an identical repaint (no flicker)
	lastW     int      // terminal width at the last paint — a resize forces a repaint
}

// NewRegion writes to w, sizing region lines with width (called on each repaint, so a resized
// terminal is picked up) — region lines are clipped to width so they never wrap and desync the
// in-place repaint.
func NewRegion(w io.Writer, width func() int) *Region {
	return &Region{w: w, width: width}
}

// Update writes history (possibly empty or multi-line) into the scrollback above the region,
// then redraws region as the bottom-pinned lines. On a closed region (Clear ran — teardown can
// race one last status line) history degrades to plain appended lines and nothing repaints.
func (r *Region) Update(history string, region []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		if history != "" {
			for _, line := range strings.Split(strings.TrimRight(history, "\n"), "\n") {
				fmt.Fprintln(r.w, line)
			}
		}
		return
	}
	w := r.width()
	// Nothing new — no history to scroll, same region content + width → skip the repaint (matches
	// AltScreen.Frame), so a static bar or a no-op progress poll sits still instead of flickering.
	// Any real change — new history, new content, or a resize — falls through and repaints.
	if history == "" && w == r.lastW && slices.Equal(region, r.lastLines) {
		return
	}
	r.eraseLocked()
	if history != "" {
		for _, line := range strings.Split(strings.TrimRight(history, "\n"), "\n") {
			fmt.Fprint(r.w, "\033[K"+line+"\n")
		}
	}
	for i, line := range region {
		if i > 0 {
			fmt.Fprint(r.w, "\n")
		}
		fmt.Fprint(r.w, "\033[K"+clip(line, w-1))
	}
	if len(region) > 0 {
		// Park the cursor at column 0, not after the region's last cell. An interactive terminal
		// echoes a Ctrl-C as literal ^C at the cursor; at end-of-line that echo fills the final
		// column and wraps, silently dropping the cursor one line down — after which every erase
		// operates a line too low and stale frames leak into scrollback. Parked at the line start,
		// the echo merely overtypes the region's first cells until the next repaint wipes it.
		fmt.Fprint(r.w, "\r")
	}
	r.shown = len(region)
	r.lastLines = append(r.lastLines[:0], region...)
	r.lastW = w
}

// Clear erases the region and ends it: normal line-oriented output owns the terminal again.
func (r *Region) Clear() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.eraseLocked()
	r.closed = true
}

// eraseLocked moves the cursor to the region's top-left and clears it and everything below.
// With nothing drawn yet it does nothing, so the first paint doesn't disturb prior output.
func (r *Region) eraseLocked() {
	if r.shown == 0 {
		return
	}
	if r.shown > 1 {
		fmt.Fprintf(r.w, "\033[%dA", r.shown-1)
	}
	fmt.Fprint(r.w, "\r\033[J")
	r.shown = 0
}

// AltScreen drives a full-screen live view on the terminal's alternate buffer — the model
// behind `coop fleet watch`. A bottom-pinned Region scrolls its top lines into scrollback on
// every repaint once the content is taller than the window (the "coop fleet — N running" spam);
// the alternate buffer has no scrollback to pollute, and Frame repaints from the top-left rather
// than doing cursor-up math from the bottom, so an over-tall dashboard degrades to "shows what
// fits" instead of orphaning its header. Enter switches to the alt buffer and hides the cursor;
// Leave restores the prior screen. Build one only when the target is a real terminal.
type AltScreen struct {
	w     io.Writer
	width func() int
	last  []string // the last painted frame — skip the repaint when nothing changed (no flicker)
	lastW int      // terminal width at the last paint — a resize forces a repaint
}

// NewAltScreen writes to w, sizing each frame's lines with width (called per repaint, so a
// resized terminal is picked up).
func NewAltScreen(w io.Writer, width func() int) *AltScreen {
	return &AltScreen{w: w, width: width}
}

// Enter switches to the alternate screen buffer and hides the cursor.
func (s *AltScreen) Enter() { fmt.Fprint(s.w, "\033[?1049h\033[?25l") }

// Frame repaints the whole view from the top-left: the cursor homes, each line is cleared to the
// end of line and clipped to the width so it never wraps, and a final erase-to-end wipes any
// lines left over from a taller previous frame.
func (s *AltScreen) Frame(lines []string) {
	w := s.width()
	// Nothing changed since the last paint (same content, same width) → don't repaint. A static
	// dashboard (no fork running, so no spinner animating) then sits still instead of flickering
	// every poll; a resize or any content change still repaints.
	if w == s.lastW && slices.Equal(lines, s.last) {
		return
	}
	var b strings.Builder
	b.WriteString("\033[H") // cursor home
	for i, line := range lines {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString("\033[K" + clip(line, w-1))
	}
	b.WriteString("\033[J") // clear anything below — a frame shorter than the last
	fmt.Fprint(s.w, b.String())
	s.last = append(s.last[:0], lines...)
	s.lastW = w
}

// Leave shows the cursor and restores the screen that was active before Enter.
func (s *AltScreen) Leave() { fmt.Fprint(s.w, "\033[?25h\033[?1049l") }

// ProgressBar renders a width-cell bar filled to frac (0..1), the filled cells cyan.
func ProgressBar(frac float64, width int) string {
	if width < 0 {
		width = 0 // a negative width would make strings.Repeat panic on the empty portion
	}
	if frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	filled := int(frac*float64(width) + 0.5)
	return "[" + Cyan(strings.Repeat("█", filled)) + strings.Repeat("░", width-filled) + "]"
}

// ProgressBarStates is ProgressBar with a blocked segment: `done` cells filled (cyan), then `blocked`
// cells in red, then the rest (todo + in-progress) empty — all out of total. A glance shows both how
// far along AND how much is parked on a decision. A non-zero blocked count always claims at least one
// cell, stolen from done if need be: a lone blocker must never round away and let a near-complete bar
// read as all-done — that would hide the very issue a human still has to clear.
func ProgressBarStates(done, blocked, total, width int) string {
	if width < 0 {
		width = 0
	}
	if total <= 0 {
		return "[" + strings.Repeat("░", width) + "]"
	}
	cells := func(n int) int {
		if n < 0 {
			return 0
		}
		return int(float64(n)/float64(total)*float64(width) + 0.5)
	}
	b := cells(blocked)
	if blocked > 0 && b == 0 {
		b = 1 // floor a real blocker at one visible cell, never round it away
	}
	if b > width {
		b = width
	}
	d := cells(done)
	if d+b > width {
		d = width - b // done yields the overflow to the blocker, not the reverse
	}
	return "[" + Cyan(strings.Repeat("█", d)) + Red(strings.Repeat("█", b)) + strings.Repeat("░", width-d-b) + "]"
}

// clip truncates s to at most n visible columns, counting runes but not ANSI SGR escapes, and
// appends a reset if it cut inside styled text — so a colored status line can be width-bounded
// without leaving a dangling color or miscounting escape bytes as width.
func clip(s string, n int) string {
	if n <= 0 {
		return ""
	}
	var b strings.Builder
	vis, styled := 0, false
	for i := 0; i < len(s); {
		if s[i] == '\033' { // copy a whole escape without counting it as width
			j := i + 1
			if j < len(s) && s[j] == '[' { // CSI: ESC [ <params 0x20-0x3f> <final 0x40-0x7e>
				j++
				for j < len(s) && s[j] >= 0x20 && s[j] <= 0x3f {
					j++
				}
				if j < len(s) {
					j++ // the final byte (e.g. 'm')
				}
			} else if j < len(s) {
				j++ // a non-CSI escape; skip the next byte
			}
			b.WriteString(s[i:j])
			styled = true
			i = j
			continue
		}
		if vis >= n {
			if styled {
				b.WriteString("\033[0m")
			}
			return b.String()
		}
		_, size := utf8.DecodeRuneInString(s[i:])
		b.WriteString(s[i : i+size])
		vis++
		i += size
	}
	return s
}
