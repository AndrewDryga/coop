package forkctl

import (
	"fmt"
	"os"
	"strings"

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

// indent prefixes every line of s with two spaces.
func indent(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = "  " + l
	}
	return strings.Join(lines, "\n")
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
