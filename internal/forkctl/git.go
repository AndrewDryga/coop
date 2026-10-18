package forkctl

import (
	"context"
	"os"
	"strings"

	"github.com/AndrewDryga/coop/internal/forkspace"
)

// gitInteractive runs a hardened git command wired to the real stdio (a diff to the terminal, a
// signing pinentry prompt, etc).
func gitInteractive(dir string, args ...string) error {
	cmd, err := forkspace.GitCommand(context.Background(), dir, args...)
	if err != nil {
		return err
	}
	cmd.Stdout, cmd.Stderr, cmd.Stdin = os.Stdout, os.Stderr, os.Stdin
	return cmd.Run()
}

// gitSign runs a hardened git command (like a rebase with signing), wiring Stdin so a TTY pinentry
// still works, but capturing CombinedOutput to silence benign chatter. The captured output is
// replayed to Stderr only on failure, or if GIT_TRACE is set.
func gitSign(dir string, args ...string) error {
	cmd, err := forkspace.GitCommand(context.Background(), dir, args...)
	if err != nil {
		return err
	}
	cmd.Stdin = os.Stdin
	out, err := cmd.CombinedOutput()
	trace := strings.TrimSpace(os.Getenv("GIT_TRACE"))
	if err != nil || (trace != "" && trace != "0" && !strings.EqualFold(trace, "false")) {
		_, _ = os.Stderr.Write(out)
	}
	return err
}

// gitGlobalOut reads from the host user's GLOBAL git config (`git config --global …`) — the
// trusted scope an agent can't write — for any value coop reads then EXECUTES or reads a host file
// from: your core.editor, your global diff.tool. The repo's own .git/config is agent-writable, so
// reading these from it would let a poisoned repo redirect coop to run or exfiltrate whatever it
// names. A value only in repo config is treated as unset (fail closed).
func gitGlobalOut(args ...string) string { return forkspace.GlobalGitConfig(args...) }

func gitBranch(dir string) string { return forkspace.GitOut(dir, "rev-parse", "--abbrev-ref", "HEAD") }

func gitDirty(dir string) bool { return forkspace.GitOut(dir, "status", "--porcelain") != "" }
