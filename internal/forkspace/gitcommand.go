package forkspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"
)

// gitStallLimit bounds a git command whose caller set no deadline. Git opens the index, refs and
// objects of a repository a box can write with blocking opens, and a FIFO a box puts at one holds
// git until something writes to it, which is never. The longest host git run, a clone or fsck of a
// large repository, finishes well inside it.
var gitStallLimit = 15 * time.Minute

// gitStalls holds the commands stopped at gitStallLimit, by command, until GitStalled reads them:
// only stopped ones, so it stays as small as the stalls are rare.
var gitStalls sync.Map

// GitStalled is err, or, for a cmd this package stopped at its time limit, a plain refusal naming
// the repository it was run in. Callers that report git's failure pass its error through it.
func GitStalled(cmd *exec.Cmd, err error) error {
	dir, stalled := gitStalls.LoadAndDelete(cmd)
	if !stalled || err == nil {
		return err
	}
	return fmt.Errorf("git did not finish in %s within %s, so Coop stopped it: a file in its .git may be a FIFO or on a disk that does not answer: %w", dir, gitStallLimit, err)
}

// TrustedGitCommand runs Git with metadata the caller already owns (a view or a
// private snapshot). It preserves that metadata's environment and index. Ordinary
// workspaces must use GitCommand instead: argv hardening alone cannot disable
// arbitrarily named drivers in box-written configuration.
//
// This is the shared submodule boundary. Git must never launch a child command
// under a submodule's own configuration. Inspect children through separate views.
func TrustedGitCommand(ctx context.Context, dir string, env []string, args ...string) *exec.Cmd {
	args, err := nonRecursiveGitArgs(args)
	full := append([]string{}, GitHardening...)
	if dir != "" {
		full = append(full, "-C", dir)
	}
	full = append(full, args...)
	cmd := boundedGitCommand(ctx, dir, full...)
	cmd.Env = env
	if err != nil {
		cmd.Err = err
	}
	return cmd
}

// boundedGitCommand is exec.CommandContext for git, stopped at gitStallLimit when ctx sets no
// deadline of its own; GitStalled then names dir in the error.
func boundedGitCommand(ctx context.Context, dir string, args ...string) *exec.Cmd {
	var stop context.CancelFunc
	if _, bounded := ctx.Deadline(); !bounded {
		ctx, stop = context.WithTimeout(ctx, gitStallLimit)
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	if stop != nil {
		cmd.Cancel = func() error {
			stop()
			// A person at the terminal decides how long a command they see takes, a pager or a
			// signing prompt: it runs on, and its own exit status stands.
			if cmd.Stdin == os.Stdin || cmd.Stdout == os.Stdout {
				return os.ErrProcessDone
			}
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				gitStalls.Store(cmd, dir)
			}
			return cmd.Process.Kill()
		}
	}
	return cmd
}

// GlobalGitConfig reads the host user's GLOBAL git config (`git config --global <args>`), or ""
// when it is unset or cannot be read: the one scope an agent cannot write, for every value coop
// reads and then executes or opens. Git starts with a git dir that cannot exist, so it never looks
// for a repository from coop's working folder, which may be a box's, where a FIFO at .git/HEAD
// would hold it; and it is stopped at gitStallLimit.
func GlobalGitConfig(args ...string) string {
	cmd := boundedGitCommand(context.Background(), "", append([]string{"config", "--global"}, args...)...)
	cmd.Dir = "/"
	cmd.Env = append(withoutGitEnv(os.Environ()), "GIT_DIR="+noGitDir, "GIT_CEILING_DIRECTORIES=/")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func nonRecursiveGitArgs(args []string) ([]string, error) {
	// Only these global options are used by host callers. In particular, callers
	// cannot relocate the repository or inject a different command through -C.
	i := 0
	for i < len(args) && args[i] == "-c" {
		if i+1 >= len(args) {
			return nil, fmt.Errorf("missing Git configuration value")
		}
		i += 2
	}
	if i == len(args) {
		return nil, fmt.Errorf("missing Git command")
	}
	verb := args[i]
	var flags []string
	switch verb {
	case "status", "diff", "diff-files", "diff-index", "diff-tree", "show", "log", "difftool":
		ignore := "dirty" // compare gitlink HEADs without asking a child's status
		for _, arg := range args[i+1:] {
			if arg == "--" {
				break
			}
			if arg == "--ignore-submodules=all" {
				ignore = "all" // callers deliberately omitting gitlinks retain that contract
			}
		}
		flags = append(flags, "--ignore-submodules="+ignore)
		if verb != "status" {
			flags = append(flags, "--submodule=short")
			if verb != "difftool" {
				flags = append(flags, "--no-ext-diff", "--no-textconv")
			}
		}
	case "push":
		// Publication transfers reviewed objects/refs only. Never visit children,
		// even when caller or repository configuration requests recursive pushes.
		flags = append(flags, "--recurse-submodules=no")
	case "var":
		if len(args[i:]) != 2 || args[i+1] != "GIT_COMMITTER_IDENT" {
			return nil, fmt.Errorf("host Git var requires a reviewed identity query")
		}
	case "lfs":
		// Fetch reads committed pointers. Publication supplies explicit object IDs
		// on stdin; it must not turn into a ref/worktree scan. Env is diagnostic only.
		lfs := args[i+1:]
		if len(lfs) == 0 {
			return nil, fmt.Errorf("missing host Git LFS command")
		}
		switch lfs[0] {
		case "fetch":
		case "env":
			if len(lfs) != 1 {
				return nil, fmt.Errorf("host Git LFS env takes no arguments")
			}
		case "push":
			if len(lfs) != 4 || lfs[1] != "--object-id" || lfs[2] == "" || strings.HasPrefix(lfs[2], "-") || lfs[3] != "--stdin" {
				return nil, fmt.Errorf("host Git LFS push requires explicit object IDs on stdin")
			}
		default:
			return nil, fmt.Errorf("host Git LFS command %q has no safety policy", lfs[0])
		}
	case "add", "submodule":
		return nil, fmt.Errorf("host git %s can enter box-written submodule configuration; use explicit index plumbing or a child view", verb)
	case "rebase", "reset", "checkout", "read-tree", "update-index", "merge", "clean",
		"fetch", "clone", "init", "config", "ls-remote", "rev-parse", "rev-list", "merge-base", "ls-files",
		"ls-tree", "cat-file", "hash-object", "write-tree", "commit-tree", "update-ref",
		"symbolic-ref", "for-each-ref", "show-ref", "branch", "remote", "worktree",
		"pack-objects", "index-pack", "fsck", "check-attr", "check-ignore", "count-objects":
		// These commands do not recurse with submodule.recurse/fetch recursion off.
		// Rebase's internal clean-tree checks explicitly override submodule ignore config.
	default:
		return nil, fmt.Errorf("host Git command %q has no submodule safety policy", verb)
	}
	out := append([]string{}, args[:i]...)
	// Last global values win, including over caller -c flags. They also reach
	// Git's own subprocesses (rebase, difftool and transport helpers).
	out = append(out, gitViewHardening...)
	out = append(out, "-c", "diff.submodule=short", "-c", "fetch.recurseSubmodules=false", "-c", "push.recurseSubmodules=no")
	out = append(out, verb)
	options := true
	for _, arg := range args[i+1:] {
		if arg == "--" {
			out = append(out, flags...)
			flags = nil
			options = false
		}
		if options && (verb == "rebase" || verb == "merge") && (arg == "-s" || strings.HasPrefix(arg, "-s") && !strings.HasPrefix(arg, "--") || verb == "rebase" && strings.HasPrefix(arg, "-x")) {
			return nil, fmt.Errorf("host Git strategy or execution option %q is not allowed", arg)
		}
		if options && strings.HasPrefix(arg, "--") {
			key, _, _ := strings.Cut(arg, "=")
			// Reject abbreviations as well as full recursive/inline options. Git
			// accepts unique prefixes; checking only the full spelling is unsafe.
			if len(key) > 2 && (strings.HasPrefix("--recurse-submodules", key) || strings.HasPrefix("--recursive", key) || strings.HasPrefix("--recurse-submodules-default", key)) && arg != "--recurse-submodules=no" {
				return nil, fmt.Errorf("recursive host Git option %q is not allowed", arg)
			}
			if (verb == "rebase" || verb == "merge") && len(key) > 2 && (strings.HasPrefix("--strategy", key) || verb == "rebase" && strings.HasPrefix("--exec", key)) {
				return nil, fmt.Errorf("host Git strategy or execution option %q is not allowed", arg)
			}
			if len(key) > 2 && strings.HasPrefix("--ignore-submodules", key) {
				if arg != "--ignore-submodules=all" && arg != "--ignore-submodules=dirty" && !(verb == "update-index" && arg == "--ignore-submodules") {
					return nil, fmt.Errorf("unsafe host Git submodule option %q", arg)
				}
			}
			if len(key) > 2 && strings.HasPrefix("--submodule", key) && arg != "--submodule=short" {
				return nil, fmt.Errorf("unsafe host Git submodule format %q", arg)
			}
		}
		out = append(out, arg)
	}
	return append(out, flags...), nil
}

// Real metadata is only for operations that cannot use a view. Keep this list
// separate from the worktree policy: a safe argv does not make local drivers safe.
func refOnlyGitArgs(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("missing ref-only Git command")
	}
	switch args[0] {
	case "config", "rev-parse", "update-ref", "symbolic-ref", "for-each-ref", "branch", "remote", "init":
		return nil
	case "cat-file":
		if len(args) == 3 && args[1] == "-e" {
			return nil
		}
	case "hash-object":
		if len(args) == 5 && args[1] == "-w" && args[2] == "-t" && (args[3] == "tree" || args[3] == "commit" || args[3] == "blob") && args[4] == "--stdin" {
			return nil
		}
	case "worktree":
		if len(args) > 1 && (args[1] == "add" && slices.Contains(args, "--no-checkout") || args[1] == "remove" && slices.Contains(args, "--force")) {
			return nil
		}
	}
	return fmt.Errorf("git %v needs a trusted worktree view", args)
}
