// Package ui handles human-facing terminal output: the dimmed "coop:" progress
// lines, red errors, and the colored check/cross marks doctor prints. Colors
// auto-disable when stderr is not a terminal, so logs and pipes stay clean.
package ui

import (
	"fmt"
	"os"
	"strings"
)

// Raw SGR codes — always defined. Both the stderr-gated package vars below and the
// stream-scoped Palette draw from these, so the escape sequences live in exactly one place.
const (
	codeReset   = "\033[0m"
	codeBold    = "\033[1m"
	codeDim     = "\033[2m"
	codeRed     = "\033[31m"
	codeGreen   = "\033[32m"
	codeYellow  = "\033[33m"
	codeMagenta = "\033[35m"
	codeCyan    = "\033[36m"
	codeGray    = "\033[90m" // bright black — a true gray, vs codeDim's terminal-dependent "faint"
)

// ANSI codes for the package-level helpers, blanked when stderr is not a terminal — coop's
// progress and diagnostic lines go to stderr. A stdout view (e.g. `coop tasks list`) colors
// through a Palette gated on stdout instead.
var (
	cGreen   string
	cRed     string
	cYellow  string
	cCyan    string
	cMagenta string
	cDim     string
	cGray    string
	cBold    string
	cReset   string
)

func init() {
	if colorEnabled(os.Stderr) {
		cGreen, cRed, cYellow, cCyan = codeGreen, codeRed, codeYellow, codeCyan
		cMagenta, cDim, cBold, cReset = codeMagenta, codeDim, codeBold, codeReset
		cGray = codeGray
	}
}

// IsTerminal reports whether f is a real terminal (a tty), via the platform
// isatty ioctl. Unlike a ModeCharDevice check it correctly rejects character
// devices that are not terminals (e.g. /dev/null), so `coop run … < /dev/null`
// does not wrongly request a docker tty. It is the basis for both color and the
// docker -it decision.
func IsTerminal(f *os.File) bool {
	if f == nil {
		return false
	}
	return isTerminalFd(f.Fd())
}

// colorEnabled reports whether ANSI color should be emitted for stream f: f must be a terminal
// AND NO_COLOR must be unset. NO_COLOR follows the no-color.org convention — its mere presence
// (any value, including empty) disables color — so `NO_COLOR=1 coop …` is plain everywhere.
func colorEnabled(f *os.File) bool {
	if _, off := os.LookupEnv("NO_COLOR"); off {
		return false
	}
	return IsTerminal(f)
}

// TermWidthRaw returns f's terminal column count, or 0 when it can't be determined (not a
// terminal, or the ioctl is unavailable) — letting a caller distinguish "unknown" from a real
// narrow width and choose its own fallback. TermWidth defaults the unknown case to 80.
func TermWidthRaw(f *os.File) int {
	if f == nil {
		return 0
	}
	return termWidthFd(f.Fd())
}

// TermWidth returns f's terminal column count, or 80 when it can't be determined (not a
// terminal, or the ioctl is unavailable) so callers always have a usable width.
func TermWidth(f *os.File) int {
	if w := TermWidthRaw(f); w > 0 {
		return w
	}
	return 80
}

// Info prints a "coop:" progress line to stderr, the prefix bold cyan so coop's own voice
// stands out from the agent's output in a busy live view.
func Info(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "%s%scoop:%s %s\n", cBold, cCyan, cReset, fmt.Sprintf(format, a...))
}

// Note prints a plain status line to stderr — like Info but WITHOUT the "coop:" prefix. Use it for
// a direct command's own result (e.g. the `coop tasks` family), where the user already knows coop
// is speaking; the prefix earns its place only when coop's voice must stand out from OTHER output —
// an agent's in a run/loop, or the block of dim Detail progress it anchors (see command-output-tiers).
func Note(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "%s\n", fmt.Sprintf(format, a...))
}

// OK prints a success result to stderr, led by a green ✓ — for a command's positive outcome. No
// "coop:" prefix (the user invoked it); the message states the result, not the command name.
func OK(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "%s✓%s %s\n", cGreen, cReset, fmt.Sprintf(format, a...))
}

// Warn prints a caution to stderr, led by a yellow ⚠ — a non-fatal heads-up (a blind spot, a
// not-yet-done precondition) the user should know about but that didn't fail the command.
func Warn(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "%s⚠%s %s\n", cYellow, cReset, fmt.Sprintf(format, a...))
}

// Error prints a failure to stderr, led by a red ✗. It does not exit. The dispatcher routes every
// returned error here, so a good message says what failed and how to fix it — not just what.
func Error(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "%s✗ %s%s\n", cRed, fmt.Sprintf(format, a...), cReset)
}

// Detail prints an indented, faint sub-step (no "coop:" prefix) — the routine per-file progress
// under a command like `coop init`, so a long run reads as one quiet block instead of repeating
// the prefix on every line. The faint log recedes behind the Info anchors and Steps that matter.
func Detail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "  %s%s%s\n", cDim, fmt.Sprintf(format, a...), cReset)
}

// Steps prints a blank line, a bold "next steps:" header, then each action on its own cyan-arrow
// line — so what you need to do next stands clear of the log of what just happened. No-op when
// there are no steps.
func Steps(steps ...string) {
	if len(steps) == 0 {
		return
	}
	fmt.Fprintf(os.Stderr, "\n%snext steps:%s\n", cBold, cReset)
	for _, s := range steps {
		fmt.Fprintf(os.Stderr, "  %s→%s %s\n", cCyan, cReset, s)
	}
}

// Color wrappers, used to compose richer output (e.g. the doctor report).
func Bold(s string) string    { return cBold + s + cReset }
func Dim(s string) string     { return cDim + s + cReset }
func Gray(s string) string    { return cGray + s + cReset }
func Green(s string) string   { return cGreen + s + cReset }
func Red(s string) string     { return cRed + s + cReset }
func Yellow(s string) string  { return cYellow + s + cReset }
func Cyan(s string) string    { return cCyan + s + cReset }
func Magenta(s string) string { return cMagenta + s + cReset }

// DimLine renders the whole of s faint, re-applying the dim after any internal reset so colored
// spans inside it (e.g. a progress bar) are dimmed too instead of snapping back to full color.
func DimLine(s string) string {
	if cDim == "" {
		return s
	}
	return cDim + strings.ReplaceAll(s, cReset, cReset+cDim) + cReset
}

// Check and Cross are the doctor pass/fail marks.
func Check() string { return cGreen + "✓" + cReset }
func Cross() string { return cRed + "✗" + cReset }

// Palette applies ANSI color gated on a chosen stream. Use For(os.Stdout) for a stdout view —
// `coop tasks list` — so a redirect or pipe (`coop tasks ls > file`) stays plain text, where the
// package-level Bold/Gray/… helpers gate on stderr (coop's progress stream). Each method is the
// identity function when color is off, so the call site reads the same with or without it.
type Palette struct{ on bool }

// For returns a Palette that emits color iff f is a real terminal.
func For(f *os.File) Palette { return Palette{on: colorEnabled(f)} }

// Enabled reports whether this palette emits color (its stream is a terminal) — for callers
// that add adornments meant only for a human at a terminal (rules, banners), not a pipe.
func (p Palette) Enabled() bool { return p.on }

func (p Palette) paint(code, s string) string {
	if !p.on {
		return s
	}
	return code + s + codeReset
}

func (p Palette) Bold(s string) string   { return p.paint(codeBold, s) }
func (p Palette) Dim(s string) string    { return p.paint(codeDim, s) }
func (p Palette) Gray(s string) string   { return p.paint(codeGray, s) }
func (p Palette) Faint(s string) string  { return p.paint(codeDim+codeGray, s) } // dim + gray — the most recessive text (e.g. a task id)
func (p Palette) Green(s string) string  { return p.paint(codeGreen, s) }
func (p Palette) Red(s string) string    { return p.paint(codeRed, s) }
func (p Palette) Yellow(s string) string { return p.paint(codeYellow, s) }
func (p Palette) Cyan(s string) string   { return p.paint(codeCyan, s) }
