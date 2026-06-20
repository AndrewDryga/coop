package ui

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"unicode/utf8"
)

// SpinFrames is the braille spinner cycle used by the live displays.
var SpinFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// Region owns a block of lines pinned to the bottom of a terminal and repaints them in place,
// so a status display updates without scrolling away. Update scrolls optional history lines
// into the scrollback above the region, then redraws the region; Clear erases it. It is the
// shared primitive behind the loop's live progress bar and `coop fleet watch`. Methods are
// safe for concurrent use. Callers build a Region only when the target is a real terminal.
type Region struct {
	w     io.Writer
	width func() int
	mu    sync.Mutex
	shown int // region lines currently on screen
}

// NewRegion writes to w, sizing region lines with width (called on each repaint, so a resized
// terminal is picked up) — region lines are clipped to width so they never wrap and desync the
// in-place repaint.
func NewRegion(w io.Writer, width func() int) *Region {
	return &Region{w: w, width: width}
}

// Update writes history (possibly empty or multi-line) into the scrollback above the region,
// then redraws region as the bottom-pinned lines.
func (r *Region) Update(history string, region []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.eraseLocked()
	if history != "" {
		for _, line := range strings.Split(strings.TrimRight(history, "\n"), "\n") {
			fmt.Fprint(r.w, "\033[K"+line+"\n")
		}
	}
	w := r.width()
	for i, line := range region {
		if i > 0 {
			fmt.Fprint(r.w, "\n")
		}
		fmt.Fprint(r.w, "\033[K"+clip(line, w-1))
	}
	r.shown = len(region)
}

// Clear erases the region so normal output resumes on a clean line.
func (r *Region) Clear() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.eraseLocked()
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
}

// Leave shows the cursor and restores the screen that was active before Enter.
func (s *AltScreen) Leave() { fmt.Fprint(s.w, "\033[?25h\033[?1049l") }

// ProgressBar renders a width-cell bar filled to frac (0..1), the filled cells cyan.
func ProgressBar(frac float64, width int) string {
	if frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	filled := int(frac*float64(width) + 0.5)
	return "[" + Cyan(strings.Repeat("█", filled)) + strings.Repeat("░", width-filled) + "]"
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
