// Package ui owns the terminal: the plain progress lines, red errors, the colored
// check/cross marks doctor prints, the live region and alt screen, and — the one thing that reads
// back — the y/N confirmation a destructive verb has to ask (see confirm.go). Colors auto-disable
// when stderr is not a terminal, so logs and pipes stay clean.
package ui

import (
	"fmt"
	"os"
	"strings"
	"sync"
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
// progress and diagnostic lines go to stderr. A stdout view (e.g. `coop tasks ls`) colors
// through a Palette gated on stdout instead.
var (
	cGreen   string
	cRed     string
	cYellow  string
	cCyan    string
	cMagenta string
	cDim     string
	cBold    string
	cReset   string
)

func init() {
	if colorEnabled(os.Stderr) {
		cGreen, cRed, cYellow, cCyan = codeGreen, codeRed, codeYellow, codeCyan
		cMagenta, cDim, cBold, cReset = codeMagenta, codeDim, codeBold, codeReset
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

// liveSink, when set, receives each ui status line (trailing newline trimmed) instead of os.Stderr,
// so a live region — the loop's bottom bar — can scroll it cleanly into the history above itself
// rather than have it overprint the bar. The owner sets it while the region is up, clears it after.
// It is guarded by liveSinkMu: the loop sets/clears it on the main goroutine while the interrupt
// watcher (and any other caller) can emit concurrently, so an unguarded var would race — and a
// two-read check-then-call could even read non-nil then call nil after a clear, panicking.
var (
	liveSinkMu sync.Mutex
	liveSink   func(string)
)

// SetLiveSink routes ui status lines through fn (a live region's funnel); nil restores os.Stderr.
func SetLiveSink(fn func(string)) {
	liveSinkMu.Lock()
	liveSink = fn
	liveSinkMu.Unlock()
}

// LiveActive reports whether a live sink currently owns ui output — a live region is on screen,
// so the terminal is not line-oriented right now and callers must not write raw newlines around
// their ui calls to position them.
func LiveActive() bool {
	liveSinkMu.Lock()
	defer liveSinkMu.Unlock()
	return liveSink != nil
}

// emit writes one fully-formatted ui line: through the live sink if active (the region positions
// lines itself, so the trailing newline is trimmed), else straight to stderr. It snapshots the
// sink under the lock into a local, so a concurrent SetLiveSink can't turn a non-nil check into a
// nil call.
func emit(s string) {
	liveSinkMu.Lock()
	sink := liveSink
	liveSinkMu.Unlock()
	if sink != nil {
		sink(strings.TrimRight(s, "\n"))
		return
	}
	fmt.Fprint(os.Stderr, s)
}

// Note prints one plain status line to stderr: coop's own voice, with no "coop:" anchor in front
// of it. Human output carries no tool prefix — including where it follows a provider's output —
// so a line reads as the sentence it is. Machine streams and protocol payloads are untouched.
func Note(format string, a ...any) {
	emit(fmt.Sprintf("%s\n", fmt.Sprintf(format, a...)))
}

// OK prints a success result to stderr, led by a green ✓ — for a command's positive outcome. The
// message states the result, not the command name (the user invoked it).
func OK(format string, a ...any) {
	emit(fmt.Sprintf("%s✓%s %s\n", cGreen, cReset, fmt.Sprintf(format, a...)))
}

// Warn prints a caution to stderr, led by a yellow ⚠ — a non-fatal heads-up (a blind spot, a
// not-yet-done precondition) the user should know about but that didn't fail the command.
func Warn(format string, a ...any) {
	emit(fmt.Sprintf("%s⚠%s %s\n", cYellow, cReset, fmt.Sprintf(format, a...)))
}

// Warning is a top-level caution BLOCK: the amber ⚠ headline at column zero, the bounded reason
// six spaces in after a blank line, and the action two spaces in after another. It is Fail's shape
// for something that did not stop the command — services that would not start, a trace that could
// not be opened, ports that were not published — so a person reads a warning and a failure the
// same way. Only the mark is colored; reason and action are each optional. Warn stays the ONE-LINE
// form, for a heads-up with nothing further to say.
func Warning(headline, reason, action string) {
	var b strings.Builder
	fmt.Fprintf(&b, "%s⚠%s %s\n", cYellow, cReset, headline)
	if reason != "" {
		b.WriteString("\n")
		for _, line := range strings.Split(strings.TrimRight(reason, "\n"), "\n") {
			b.WriteString("      " + line + "\n")
		}
	}
	if action != "" {
		b.WriteString("\n  " + action + "\n")
	}
	emit(b.String())
}

// Count renders a number with its noun, pluralized — Count(1, "task") = "1 task", Count(2, "task")
// = "2 tasks". Pass an explicit plural for an irregular noun: Count(2, "box", "boxes") = "2 boxes".
// Use it for human counts in results so output reads "2 forks", not "2 fork(s)".
func Count(n int, singular string, plural ...string) string {
	noun := singular + "s"
	if n == 1 {
		noun = singular
	} else if len(plural) > 0 {
		noun = plural[0]
	}
	return fmt.Sprintf("%d %s", n, noun)
}

// List joins items the way a sentence does — "claude", "claude or codex", "claude, codex, or all"
// — with conj the closing conjunction ("and" / "or"). Use it wherever output names a set a person
// reads aloud (the accepted values of an option, a project's services), so every such list is
// punctuated the same way.
func List(items []string, conj string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	case 2:
		return items[0] + " " + conj + " " + items[1]
	}
	return strings.Join(items[:len(items)-1], ", ") + ", " + conj + " " + items[len(items)-1]
}

// Bytes renders a byte count the way a human reads a transfer: exact below a kilobyte, then one
// decimal place — 773 B, 5.3 KB, 1.2 MB. Decimal units (1 KB = 1000 B), matching how network
// traffic is normally quoted. Use it for a result a person skims; an exact audit number belongs
// in the --json view, not in a line someone reads once.
func Bytes(n uint64) string {
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	value, exp := float64(n)/unit, 0
	for value >= unit && exp < 3 {
		value, exp = value/unit, exp+1
	}
	return fmt.Sprintf("%.1f %s", value, [...]string{"KB", "MB", "GB", "TB"}[exp])
}

// Error prints a failure to stderr, led by a red ✗. It does not exit. The dispatcher routes every
// returned error here, so a good message says what failed and how to fix it — not just what.
func Error(format string, a ...any) {
	emit(fmt.Sprintf("%s✗ %s%s\n", cRed, fmt.Sprintf(format, a...), cReset))
}

// Detail prints an indented, faint sub-step — the routine per-file progress under a command like
// `coop init`, so a long run reads as one quiet block. The faint log recedes behind the Note
// anchors and Steps that matter.
func Detail(format string, a ...any) {
	emit(fmt.Sprintf("  %s%s%s\n", cDim, fmt.Sprintf(format, a...), cReset))
}

// Actions prints a blank line, a bold header naming the job, then each action on its own
// cyan-arrow line — so what you need to do next stands clear of the log of what just happened.
// A command with several distinct jobs left (finish setup, then verify, then start working)
// prints one block per job. No-op when there are no actions.
func Actions(header string, actions ...string) {
	if len(actions) == 0 {
		return
	}
	fmt.Fprintf(os.Stderr, "\n%s%s%s\n", cBold, header, cReset)
	for _, s := range actions {
		fmt.Fprintf(os.Stderr, "  %s→%s %s\n", cCyan, cReset, s)
	}
}

// Steps is the one-block form of Actions, under the standard "next steps:" header.
func Steps(steps ...string) { Actions("next steps:", steps...) }

// Color wrappers, used to compose richer output (e.g. the doctor report).
func Bold(s string) string    { return cBold + s + cReset }
func Dim(s string) string     { return cDim + s + cReset }
func Green(s string) string   { return cGreen + s + cReset }
func Red(s string) string     { return cRed + s + cReset }
func Yellow(s string) string  { return cYellow + s + cReset }
func Cyan(s string) string    { return cCyan + s + cReset }
func Magenta(s string) string { return cMagenta + s + cReset }

// Check and Cross are the doctor pass/fail marks.
func Check() string { return cGreen + "✓" + cReset }
func Cross() string { return cRed + "✗" + cReset }

// Palette applies ANSI color gated on a chosen stream. Use For(os.Stdout) for a stdout view —
// `coop tasks ls` — so a redirect or pipe (`coop tasks ls > file`) stays plain text, where the
// package-level color helpers gate on stderr (coop's progress stream). Each method is the
// identity function when color is off, so the call site reads the same with or without it.
type Palette struct{ on bool }

// For returns a Palette that emits color iff f is a real terminal.
func For(f *os.File) Palette { return Palette{on: colorEnabled(f)} }

// Colored is a Palette that always emits color, regardless of any stream. It is
// for a renderer proving its styling where no terminal exists — a test — and
// nothing else: production output gates on its own stream through For.
func Colored() Palette { return Palette{on: true} }

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

// Link wraps text in an OSC 8 terminal hyperlink to uri, so a supporting terminal makes it
// clickable — but only when this palette is enabled (a real terminal) and uri is non-empty, so a
// pipe stays clean text. A terminal that doesn't understand OSC 8 ignores the escape and shows
// text unchanged, so it degrades gracefully.
func (p Palette) Link(uri, text string) string {
	if !p.on || uri == "" {
		return text
	}
	return "\x1b]8;;" + uri + "\x1b\\" + text + "\x1b]8;;\x1b\\"
}
