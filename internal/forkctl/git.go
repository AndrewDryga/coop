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

// gitSignQuiet runs a hardened git command (like a rebase with signing), wiring Stdin so a TTY
// pinentry still works, but capturing CombinedOutput to silence benign chatter. The output is
// replayed to Stderr on success only if GIT_TRACE is set; a failure's output goes to the caller,
// which may have better words for it: a rebase's conflict hints name git commands for a rebase Coop
// already undid.
func gitSignQuiet(dir string, args ...string) ([]byte, error) {
	cmd, err := forkspace.GitCommand(context.Background(), dir, args...)
	if err != nil {
		return nil, err
	}
	cmd.Stdin = os.Stdin
	out, err := cmd.CombinedOutput()
	if trace := strings.TrimSpace(os.Getenv("GIT_TRACE")); err == nil && trace != "" && trace != "0" && !strings.EqualFold(trace, "false") {
		_, _ = os.Stderr.Write(out)
	}
	return out, err
}

// gitGlobalOut reads from the host user's GLOBAL git config (`git config --global …`) — the
// trusted scope an agent can't write — for any value coop reads then EXECUTES or reads a host file
// from: your core.editor, your global diff.tool. The repo's own .git/config is agent-writable, so
// reading these from it would let a poisoned repo redirect coop to run or exfiltrate whatever it
// names. A value only in repo config is treated as unset (fail closed).
func gitGlobalOut(args ...string) string { return forkspace.GlobalGitConfig(args...) }

func gitBranch(dir string) string { return forkspace.GitOut(dir, "rev-parse", "--abbrev-ref", "HEAD") }

func gitDirty(dir string) bool { return forkspace.GitOut(dir, "status", "--porcelain") != "" }

// gitTrackedDirty is gitDirty without untracked files. The parent's check before an ordinary merge
// uses it, as the isolated merge does: the land is a fast-forward, and FastForwardParent refuses an
// incoming file that would land on an untracked one, so an unrelated scratch file blocks nothing.
func gitTrackedDirty(dir string) bool {
	return forkspace.GitOut(dir, "status", "--porcelain", "--untracked-files=no") != ""
}

// untrackedCollisions lists the untracked, not ignored files in dir that the commit ref changes from
// HEAD, so a fast-forward to ref would land on them. A path untracked here is not in HEAD, so ref
// adds it. A list Git cannot give is empty, which leaves the refusal to the fast-forward.
func untrackedCollisions(dir, ref string) []string {
	untracked, err := forkspace.GitOutput(dir, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil || untracked == "" {
		return nil
	}
	incoming, err := forkspace.GitOutput(dir, "diff", "--no-renames", "--name-only", "-z", "HEAD", ref, "--")
	if err != nil {
		return nil
	}
	changed := map[string]bool{}
	for _, path := range strings.Split(incoming, "\x00") {
		changed[path] = path != ""
	}
	var hits []string
	for _, path := range strings.Split(untracked, "\x00") {
		if changed[path] {
			hits = append(hits, path)
		}
	}
	return hits
}
