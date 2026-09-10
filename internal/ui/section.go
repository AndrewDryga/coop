package ui

import (
	"errors"
	"fmt"
	"strings"
)

// An interactive launch narrates its host-side work as SECTIONS a person reads top to bottom —
// `Protecting secrets`, `Internet access`, `Starting Codex` — before the agent's own output
// begins. The invocation already says coop is speaking, so a section heading is bold and
// unprefixed; the cyan `coop:` anchor (Info) is kept for lifecycle lines interleaved AFTER
// arbitrary agent output, where the speaker is no longer obvious. See command-output-tiers.

// Section prints a bold section heading, preceded by a blank line so consecutive sections read
// as separate paragraphs (the same shape Steps uses for its header).
func Section(title string) {
	emit(fmt.Sprintf("\n%s%s%s\n", cBold, title, cReset))
}

// Pass prints a section result that succeeded: an indented green ✓ and the plain result text.
// Only the mark is colored — the sentence stays in the normal foreground.
func Pass(format string, a ...any) {
	emit(fmt.Sprintf("  %s✓%s %s\n", cGreen, cReset, fmt.Sprintf(format, a...)))
}

// Caution prints a section result that a person should notice without it being a failure: an
// indented yellow ⚠ and the plain text.
func Caution(format string, a ...any) {
	emit(fmt.Sprintf("  %s⚠%s %s\n", cYellow, cReset, fmt.Sprintf(format, a...)))
}

// Fail ends a section with its failure: the red ✗ headline indented like every other result,
// then, after a blank line, the concrete reason six spaces further in, and — when there is one —
// the remedy one level under the section after another blank line. Reason and remedy stay in the
// normal foreground: a dim reason is too easy to miss, and it is the one thing the person must
// read. A multi-line reason keeps every line at the same depth.
func Fail(headline, reason, remedy string) {
	var b strings.Builder
	fmt.Fprintf(&b, "  %s✗ %s%s\n", cRed, headline, cReset)
	if reason != "" {
		b.WriteString("\n")
		for _, line := range strings.Split(strings.TrimRight(reason, "\n"), "\n") {
			b.WriteString("        " + line + "\n")
		}
	}
	if remedy != "" {
		b.WriteString("\n    " + remedy + "\n")
	}
	emit(b.String())
}

// ErrReported marks an error that has already been rendered in full — a failed section, say —
// so the dispatcher's fallback "✗ …" line does not repeat it. Wrap with Reported; the original
// error stays reachable through errors.Is/As, so a cancellation is still a cancellation.
var ErrReported = errors.New("already reported")

// Reported returns err marked as already rendered. A nil error stays nil.
func Reported(err error) error {
	if err == nil {
		return nil
	}
	return reportedError{err}
}

type reportedError struct{ err error }

func (e reportedError) Error() string        { return e.err.Error() }
func (e reportedError) Unwrap() error        { return e.err }
func (e reportedError) Is(target error) bool { return target == ErrReported }
