package loop

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/AndrewDryga/coop/internal/forkspace"
)

// gitOut runs `git -C dir <args>` hardened and returns trimmed stdout, or "" on error. Every repo
// coop runs git against is agent-writable, so hardening is the default; to read a value coop will
// execute or read a host file from, read the trusted GLOBAL scope (`git config --global`), never
// the repo.
// It CONFLATES a failed read with an empty one — fine for display, wrong for a decision: read those
// with gitOutErr.
func gitOut(dir string, args ...string) string {
	out, _ := gitOutErr(dir, args...)
	return out
}

// gitOutErr is gitOut for a read coop ACTS on: same hardened command, but a failure comes back as an
// error instead of an empty string, so "git broke" can't pass for "git said nothing" (an unreadable
// HEAD read as "" perturbs the loop's stall bookkeeping; an unreadable range reconciles no tasks and
// looks clean). The message carries git's own stderr — os/exec caps that capture at 32KB — because a
// caller surfacing this to a human has nothing else to explain the failure with.
func gitOutErr(dir string, args ...string) (string, error) {
	cmd, err := forkspace.GitCommand(context.Background(), dir, args...)
	if err != nil {
		return "", err
	}
	out, err := cmd.Output()
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
