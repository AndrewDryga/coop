package forkctl

import (
	"fmt"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/AndrewDryga/coop/internal/ui"
)

// The stateless leaves this package shares with internal/cli, redeclared here rather than exported
// across the boundary — the same call internal/tasks/util.go and internal/sessionsvc/git.go already
// make, and for the same reason: a `pathExists` or a `padRight` is not an API, and an export would
// make internal/cli a dependency of everything that formats a table.

func pathExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// composeTarget rebuilds the positional target passed to a detached fork worker from the
// pieces parsed by the parent. A model may carry the account itself; contradictory sources fail.
func composeTarget(agent, model, effort, credential string) (string, error) {
	modelPart, acctInModel, hasAt := strings.Cut(model, "@")
	acct := credential
	if hasAt && acctInModel != "" {
		if credential != "" && credential != acctInModel {
			return "", fmt.Errorf("account set twice: model %q pins @%s but credential is %q", model, acctInModel, credential)
		}
		acct = acctInModel
	}
	target := agent
	if modelPart != "" {
		target += ":" + modelPart
	}
	if effort != "" {
		target += "/" + effort
	}
	if acct != "" {
		target += "@" + acct
	}
	return target, nil
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
