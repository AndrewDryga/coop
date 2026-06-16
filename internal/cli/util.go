package cli

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/AndrewDryga/coop/internal/ui"
)

func fileExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir()
}

func pathExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// queueHasTodo reports whether a TASKS.md file still has an unclaimed "[ ]" item.
func queueHasTodo(queue string) bool {
	data, err := os.ReadFile(queue)
	if err != nil {
		return false
	}
	return strings.Contains(string(data), "[ ]")
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

func gitClone(src, dst string) error {
	return exec.Command("git", "clone", "--quiet", src, dst).Run()
}

func gitCheckoutNewBranch(repo, branch string) error {
	return exec.Command("git", "-C", repo, "checkout", "--quiet", "-b", branch).Run()
}

// gitOut runs `git -C dir <args>` and returns trimmed stdout, or "" on error.
func gitOut(dir string, args ...string) string {
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// gitRun runs `git -C dir <args>` for effect, returning its error.
func gitRun(dir string, args ...string) error {
	return exec.Command("git", append([]string{"-C", dir}, args...)...).Run()
}

// gitInteractive runs a git command wired to the real stdio (diff in a pager, etc).
func gitInteractive(dir string, args ...string) error {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Stdout, cmd.Stderr, cmd.Stdin = os.Stdout, os.Stderr, os.Stdin
	return cmd.Run()
}

func gitBranch(dir string) string { return gitOut(dir, "rev-parse", "--abbrev-ref", "HEAD") }

func gitDirty(dir string) bool { return gitOut(dir, "status", "--porcelain") != "" }

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

// confirm asks a yes/no question, returning def with no tty (batch runs) or on a
// bare Enter.
func confirm(prompt string, def bool) bool {
	if !ui.IsTerminal(os.Stdin) {
		return def
	}
	hint := "Y/n"
	if !def {
		hint = "y/N"
	}
	fmt.Fprintf(os.Stderr, "%s [%s] ", prompt, hint)
	var resp string
	fmt.Scanln(&resp)
	switch strings.ToLower(strings.TrimSpace(resp)) {
	case "":
		return def
	case "y", "yes":
		return true
	default:
		return false
	}
}
