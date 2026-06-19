// Package ui handles human-facing terminal output: the dimmed "coop:" progress
// lines, red errors, and the colored check/cross marks doctor prints. Colors
// auto-disable when stderr is not a terminal, so logs and pipes stay clean.
package ui

import (
	"fmt"
	"os"
)

// ANSI codes, blanked when stderr is not a terminal.
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
	if IsTerminal(os.Stderr) {
		cGreen, cRed, cYellow, cCyan = "\033[32m", "\033[31m", "\033[33m", "\033[36m"
		cMagenta, cDim, cBold, cReset = "\033[35m", "\033[2m", "\033[1m", "\033[0m"
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

// TermWidth returns f's terminal column count, or 80 when it can't be determined (not a
// terminal, or the ioctl is unavailable) so callers always have a usable width.
func TermWidth(f *os.File) int {
	if f != nil {
		if w := termWidthFd(f.Fd()); w > 0 {
			return w
		}
	}
	return 80
}

// Info prints a "coop:" progress line to stderr, the prefix bold cyan so coop's own voice
// stands out from the agent's output in a busy live view.
func Info(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "%s%scoop:%s %s\n", cBold, cCyan, cReset, fmt.Sprintf(format, a...))
}

// Error prints a red "coop:" error line to stderr. It does not exit.
func Error(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "%scoop: %s%s\n", cRed, fmt.Sprintf(format, a...), cReset)
}

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
