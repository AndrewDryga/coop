package tasks

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/AndrewDryga/coop/internal/forkspace"
)

// gitArgs builds `git -C dir <hardening> <args>`. The hardening list lives in internal/forkspace,
// next to the clone that creates a fork, so the whole repo has exactly one hardening set to audit;
// internal/cli keeps its own copy of this trio atop the same list (see its util.go) rather than
// exporting one across the package boundary — same shape internal/sessionsvc already uses.
func gitArgs(dir string, args []string) []string {
	return append(append([]string{"-C", dir}, forkspace.GitHardening...), args...)
}

// gitOut runs `git -C dir <args>` hardened and returns trimmed stdout, or "" on error.
func gitOut(dir string, args ...string) string {
	out, _ := gitOutErr(dir, args...)
	return out
}

// gitOutErr is gitOut for a read this package ACTS on: same hardened command, but a failure comes
// back as an error instead of an empty string (used by the ref-authority window's HEAD re-read,
// where "git broke" must not pass for "git said nothing").
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
