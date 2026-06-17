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

// queueHasTodo reports whether a TASKS.md file still has an unclaimed task: a list
// item that begins a line with "- [ ]". It matches at line start so the "[ ]" in the
// legend/comment header, in prose, or in an "# Example" block (which uses "[E]") is
// never mistaken for real work.
func queueHasTodo(queue string) bool {
	data, err := os.ReadFile(queue)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "- [ ]") {
			return true
		}
	}
	return false
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

// forkGitHardening are -c overrides applied to every git command we run *inside* an
// agent-controlled fork. A fork's .git/ is agent-writable, so its hooks and local
// config could otherwise execute arbitrary host commands the moment we fetch, rebase,
// or even `status` it on review/merge — defeating the whole point of the box. We turn
// hooks off and blank every config knob that shells out. Verified host-exec vectors:
// .git/hooks/* (and core.hooksPath), core.fsmonitor, and a forced commit.gpgsign with
// a planted gpg.program; the rest are defense in depth. Signing on land is re-enabled
// with trusted *parent* values (see trustedSignArgs), never the fork's.
//
// Residual (can't be closed with -c, since the driver names are arbitrary): an in-tree
// .gitattributes assigning a filter to a path plus a fork-local filter.<name>.smudge
// can run on checkout during the land rebase. policyScan surfaces a fork's changed
// files for review before that point, which is the backstop for this one.
var forkGitHardening = []string{
	"-c", "core.hooksPath=/dev/null",
	"-c", "core.fsmonitor=",
	"-c", "core.sshCommand=",
	"-c", "core.pager=cat",
	"-c", "core.editor=true",
	"-c", "sequence.editor=true",
	"-c", "diff.external=",
	"-c", "uploadpack.packObjectsHook=",
	"-c", "protocol.ext.allow=never",
	"-c", "commit.gpgsign=false",
	"-c", "gpg.program=false",
	"-c", "gpg.ssh.program=false",
	"-c", "gpg.x509.program=false",
}

// hardenedFork prepends forkGitHardening to a fork-side git argument list. Any extra
// -c flags a caller appends after these (e.g. trustedSignArgs) win, since git's last
// -c for a key takes effect.
func hardenedFork(args []string) []string {
	return append(append([]string{}, forkGitHardening...), args...)
}

// gitRunFork / gitOutFork / gitInteractiveFork mirror gitRun / gitOut / gitInteractive
// but harden the invocation for an agent-controlled fork. Use these for every git
// command run with -C <fork>; the plain forms are for the trusted parent repo.
func gitRunFork(dir string, args ...string) error  { return gitRun(dir, hardenedFork(args)...) }
func gitOutFork(dir string, args ...string) string { return gitOut(dir, hardenedFork(args)...) }
func gitInteractiveFork(dir string, args ...string) error {
	return gitInteractive(dir, hardenedFork(args)...)
}

// forkDirty reports whether a fork's working tree has uncommitted changes, hardened
// (plain gitDirty runs `status`, which would fire a fork's core.fsmonitor on the host).
func forkDirty(ws string) bool { return gitOutFork(ws, "status", "--porcelain") != "" }

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

// indent prefixes every line of s with two spaces.
func indent(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = "  " + l
	}
	return strings.Join(lines, "\n")
}

// lastLines returns the last n lines of s (trailing blank lines trimmed first).
func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// approve reports whether a destructive step is approved. --yes approves without
// asking; otherwise it prompts interactively. In a non-interactive run (no TTY)
// without --yes it refuses rather than silently taking the default — so a pipe or CI
// job can't land work and delete a fork on its own. Callers gate the whole command on
// this up front (with a clear "pass --yes" error); this is also the safe fallback.
func approve(prompt string, yes bool) bool {
	if yes {
		return true
	}
	if !ui.IsTerminal(os.Stdin) {
		return false
	}
	return confirm(prompt, true)
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
