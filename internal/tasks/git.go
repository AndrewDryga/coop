package tasks

import (
	"context"
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
	out, err := gitRawOutErr(dir, args...)
	return strings.TrimSpace(out), err
}

// A detached HEAD is symbolic-ref's normal, silent exit1. View construction and other Git
// failures are not evidence that the checked-out branch is missing or changed.
func gitBranchErr(dir string) (string, error) {
	cmd, err := forkspace.GitCommand(context.Background(), dir, "symbolic-ref", "--quiet", "HEAD")
	if err != nil {
		return "", err
	}
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			detail := strings.TrimSpace(string(exitErr.Stderr))
			if exitErr.ExitCode() == 1 && detail == "" {
				return "", nil
			}
			if detail != "" {
				return "", fmt.Errorf("git symbolic-ref: %w: %s", err, detail)
			}
		}
		return "", fmt.Errorf("git symbolic-ref: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// NUL-delimited paths and other exact-byte output must not pass through TrimSpace.
func gitRawOutErr(dir string, args ...string) (string, error) {
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
	return string(out), nil
}
