package ui

import (
	"fmt"
	"io"
	"slices"
	"strings"
	"unicode/utf8"
)

// SpinFrames is the braille spinner cycle used by the live displays.
var SpinFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// AltScreen drives a full-screen live view on the terminal's alternate buffer — the model
// behind `coop fleet watch`. The alternate buffer has no scrollback to pollute, and Frame
// repaints from the top-left, so an over-tall dashboard degrades to "shows what fits" instead
// of orphaning its header. Enter switches to the alt buffer and hides the cursor; Leave restores
// the prior screen. Build one only when the target is a real terminal.
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
