package tasks

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/AndrewDryga/coop/internal/forkspace"
)

// A detached HEAD is symbolic-ref's normal, silent exit1. View construction and other Git
// failures are not evidence that the checked-out branch is missing or changed.
func gitBranchErr(dir string) (string, error) {
	cmd, err := forkspace.GitCommand(context.Background(), dir, "symbolic-ref", "--quiet", "HEAD")
	if err != nil {
		return "", err
	}
	out, err := cmd.Output()
	if err != nil {
		err = forkspace.GitStalled(cmd, err)
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
