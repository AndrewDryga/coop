package forkctl

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/AndrewDryga/coop/internal/forkspace"
)

// gitArgs builds `git -C dir <hardening> <args>`. The hardening goes first so a caller's own
// trailing -c flags (e.g. forkspace.TrustedSignArgs) still win — git's last -c for a key takes
// effect. The list itself lives in internal/forkspace, next to the clone that creates a fork, so
// the whole repo has exactly one hardening set to audit; internal/cli, internal/tasks and
// internal/sessionsvc each keep their own copy of these thin runners atop the same list (see their
// util.go/git.go) rather than exporting one across a package boundary.
func gitArgs(dir string, args []string) []string {
	return append(append([]string{"-C", dir}, forkspace.GitHardening...), args...)
}

// gitOut runs `git -C dir <args>` hardened and returns trimmed stdout, or "" on error. Every repo
// coop runs git against is agent-writable, so hardening is the default; to read a value coop will
// execute or read a host file from, use gitGlobalOut (the trusted global scope), never the repo.
// It CONFLATES a failed read with an empty one — fine for display, wrong for a decision: read those
// with gitOutErr.
func gitOut(dir string, args ...string) string {
	out, _ := gitOutErr(dir, args...)
	return out
}

// gitOutErr is gitOut for a read coop ACTS on: same hardened command, but a failure comes back as
// an error instead of an empty string, so "git broke" can't pass for "git said nothing". The
// message carries git's own stderr — os/exec caps that capture at 32KB — because a caller
// surfacing this to a human has nothing else to explain the failure with.
func gitOutErr(dir string, args ...string) (string, error) {
	out, err := exec.Command("git", gitArgs(dir, args)...).Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			if detail := strings.TrimSpace(string(exitErr.Stderr)); detail != "" {
				return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, detail)
			}
		}
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out)), nil
}

// gitRun runs `git -C dir <args>` hardened, for effect, returning its error.
func gitRun(dir string, args ...string) error {
	return exec.Command("git", gitArgs(dir, args)...).Run()
}

// gitInteractive runs a hardened git command wired to the real stdio (a diff to the terminal, a
// signing pinentry prompt, etc).
func gitInteractive(dir string, args ...string) error {
	cmd := exec.Command("git", gitArgs(dir, args)...)
	cmd.Stdout, cmd.Stderr, cmd.Stdin = os.Stdout, os.Stderr, os.Stdin
	return cmd.Run()
}

// gitSign runs a hardened git command (like a rebase with signing), wiring Stdin so a TTY pinentry
// still works, but capturing CombinedOutput to silence benign chatter. The captured output is
// replayed to Stderr only on failure, or if GIT_TRACE is set.
func gitSign(dir string, args ...string) error {
	cmd := exec.Command("git", gitArgs(dir, args)...)
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
func gitGlobalOut(args ...string) string {
	out, err := exec.Command("git", append([]string{"config", "--global"}, args...)...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func gitBranch(dir string) string { return gitOut(dir, "rev-parse", "--abbrev-ref", "HEAD") }

func gitDirty(dir string) bool { return gitOut(dir, "status", "--porcelain") != "" }
