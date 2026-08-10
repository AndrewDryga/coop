package forkctl

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/tasks"
	"github.com/AndrewDryga/coop/internal/ui"
)

// The stateless leaves this package shares with internal/cli, redeclared here rather than exported
// across the boundary — the same call internal/tasks/util.go and internal/sessionsvc/git.go already
// make, and for the same reason: a `pathExists` or a `padRight` is not an API, and an export would
// make internal/cli a dependency of everything that formats a table.

func fileExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir()
}

func pathExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// isTargetHead reports whether s begins with a registered provider (so it names an agent run, not a
// preset). internal/cli owns the full who-runs grammar; the fleet file only needs this one test, to
// classify a fork's `agent:` as a target or a preset name.
func isTargetHead(s string) bool {
	head := strings.TrimSpace(s)
	provider := head
	if i := strings.IndexAny(head, ":/@"); i >= 0 {
		provider = head[:i]
	}
	return agents.Valid(provider)
}

// paintCount renders a count, applying paint only when it's nonzero so a zero stays plain — a
// "0 blocked" shouldn't read as an alarm.
func paintCount(v int, paint func(string) string) string {
	if v > 0 {
		return paint(strconv.Itoa(v))
	}
	return strconv.Itoa(v)
}

// truncate shortens s to n runes, marking elision with an ellipsis.
func truncate(s string, n int) string {
	if n <= 0 {
		return "" // guards the r[:n-1] / r[:n] negative-index panic on a non-positive width
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 1 {
		return string(r[:n])
	}
	return string(r[:n-1]) + "…"
}

// padRight right-pads s to w columns counted in RUNES — unlike fmt's %-Ns, which counts bytes and
// so mis-pads a value carrying a multibyte glyph (e.g. a truncated name's "…").
func padRight(s string, w int) string {
	if n := utf8.RuneCountInString(s); n < w {
		return s + strings.Repeat(" ", w-n)
	}
	return s
}

// colWidth is the width to size a table column to: the widest value (counted in runes), clamped
// to [min, max]. Values longer than max are meant to be ellipsis-truncated to max by the caller.
func colWidth(values []string, min, max int) int {
	w := min
	for _, v := range values {
		if n := utf8.RuneCountInString(v); n > w {
			w = n
		}
	}
	if w > max {
		w = max
	}
	return w
}

// indent prefixes every line of s with two spaces.
func indent(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = "  " + l
	}
	return strings.Join(lines, "\n")
}

// parseShortstat pulls insertion/deletion counts out of a `git diff --shortstat`
// line ("N files changed, I insertions(+), D deletions(-)").
func parseShortstat(s string) (ins, del int) {
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		n := 0
		fmt.Sscanf(part, "%d", &n)
		switch {
		case strings.Contains(part, "insertion"):
			ins = n
		case strings.Contains(part, "deletion"):
			del = n
		}
	}
	return ins, del
}

// rejectArgs returns a usage error when a command that takes no arguments is given some, so a stray
// token fails clearly instead of being silently ignored.
func rejectArgs(cmd string, args []string) error {
	if len(args) == 0 {
		return nil
	}
	return fmt.Errorf("%s takes no arguments (got %q) — see 'coop %s --help'", cmd, strings.Join(args, " "), cmd)
}

// approve reports whether a destructive step is approved. --yes approves without asking; otherwise
// it prompts interactively. In a non-interactive run (no TTY) without --yes it refuses rather than
// silently taking the default — so a pipe or CI job can't land work and delete a fork on its own.
func approve(prompt string, yes bool) bool {
	if yes {
		return true
	}
	if !ui.IsTerminal(os.Stdin) {
		return false
	}
	return ui.Confirm(prompt, true)
}

// hasYes reports whether args carry the -y/--yes confirmation-skip flag that destructive commands
// accept to run unattended (distinct from --force, which overrides a safety guard, not the prompt).
func hasYes(args []string) bool {
	for _, a := range args {
		if a == "-y" || a == "--yes" {
			return true
		}
	}
	return false
}

// progressLine is a queue's at-a-glance state: done/total (done greened when nonzero), a blocked
// tally only when there is one, and the task being worked.
func progressLine(c tasks.TaskCounts, activity string) string {
	s := fmt.Sprintf("%s/%d done", paintCount(c.Done, ui.Green), c.Total())
	if c.Blocked > 0 {
		s += fmt.Sprintf(" · %s blocked", paintCount(c.Blocked, ui.Red))
	}
	if activity != "" {
		s += " · now: " + truncate(activity, progressActivityWidth)
	}
	return s
}

const progressActivityWidth = 48
