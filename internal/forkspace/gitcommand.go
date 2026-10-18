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

// GitOut runs `git -C dir <args>` under the trusted view of dir's repository and returns its
// trimmed stdout, or "" on error. Every repository Coop runs git against is box-writable, so the
// view is the default; a value Coop will execute or read a host file from comes from the trusted
// global scope (GlobalGitConfig), never the repository. It CONFLATES a failed read with an empty
// one — fine for display, wrong for a decision: read those with GitOutErr.
func GitOut(dir string, args ...string) string {
	out, _ := GitOutErr(dir, args...)
	return out
}

// GitOutErr is GitOut for a read Coop acts on: a failure comes back as an error, so "git broke"
// cannot pass for "git said nothing".
func GitOutErr(dir string, args ...string) (string, error) {
	out, err := GitOutput(dir, args...)
	return strings.TrimSpace(out), err
}

// GitOutput is GitOutErr without the trim, for NUL-delimited paths and other exact-byte output.
// The error names the command and carries git's own stderr (os/exec caps that capture at 32 KB),
// because a caller surfacing it to a person has nothing else to explain the failure with.
func GitOutput(dir string, args ...string) (string, error) {
	cmd, err := GitCommand(context.Background(), dir, args...)
	if err != nil {
		return "", err
	}
	out, err := cmd.Output()
	if err != nil {
		err = GitStalled(cmd, err)
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

// GitRun runs `git -C dir <args>` under the trusted view, for effect.
func GitRun(dir string, args ...string) error {
	cmd, err := GitCommand(context.Background(), dir, args...)
	if err != nil {
		return err
	}
	return GitStalled(cmd, cmd.Run())
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
		if !callerGitSetting(args[i+1]) {
			return nil, fmt.Errorf("host Git setting %q is not one a caller may pass", args[i+1])
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

// callerGitSettings are the -c settings a host caller may pass, each with the values it may take.
// None defines a driver or program that a box-written file can select: the signing values and the
// difftool come from the host's global config, and the rest turn a feature off or name a fixed
// value. A filter.<name>.clean, diff.<name>.textconv or merge.<name>.driver from a caller would
// define a command that a box-written .gitattributes runs.
var callerGitSettings = map[string]func(string) bool{
	// Signing (TrustedSignArgs): the key, format and program from the host's global config.
	"commit.gpgsign": isGitValue("true"), "user.signingkey": anyGitValue,
	"gpg.format":  isGitValue("openpgp", "ssh", "x509"),
	"gpg.program": anyGitValue, "gpg.ssh.program": anyGitValue, "gpg.x509.program": anyGitValue,
	"gpg.ssh.defaultkeycommand": anyGitValue,
	// The difftool the host's global diff.tool names; difftool.<tool>.cmd is matched below.
	"diff.tool": anyGitValue,
	// Attributes, and identity, which is data that runs nothing.
	"core.attributesfile": isGitValue(os.DevNull), "user.useconfigonly": isGitValue("true"),
	"user.name": anyGitValue, "user.email": anyGitValue,
	// Transport: the worker's own endpoints, and LFS features turned off.
	"http.followredirects": isGitValue("false"),
	"lfs.url":              anyGitValue, "lfs.pushurl": anyGitValue, "remote.origin.lfsurl": anyGitValue,
	"lfs.basictransfersonly": isGitValue("true"), "lfs.standalonetransferagent": isGitValue(""),
	"lfs.remote.autodetect": isGitValue("false"), "lfs.remote.searchall": isGitValue("false"),
	"lfs.transfer.enablehrefrewrite": isGitValue("false"), "lfs.skipdownloaderrors": isGitValue("false"),
	"lfs.fetchrecentalways": isGitValue("false"),
}

func anyGitValue(string) bool { return true }

func isGitValue(values ...string) func(string) bool {
	return func(value string) bool { return slices.Contains(values, value) }
}

// callerGitSetting reports whether a caller may pass setting, a "key=value" for -c.
func callerGitSetting(setting string) bool {
	key, value, ok := strings.Cut(setting, "=")
	if !ok {
		return false // a bare key means true; no reviewed setting is spelled that way
	}
	lower := strings.ToLower(key)
	if strings.HasPrefix(lower, "difftool.") && strings.HasSuffix(lower, ".cmd") && len(lower) > len("difftool..cmd") {
		return true // the host's global difftool.<tool>.cmd
	}
	valid := callerGitSettings[lower]
	return valid != nil && valid(value)
}

// Real metadata is only for operations that cannot use a view. Keep this list
// separate from the worktree policy: a safe argv does not make local drivers safe.
func refOnlyGitArgs(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("missing ref-only Git command")
	}
	if refOnlyShape(args) {
		return nil
	}
	return fmt.Errorf("git %v needs a trusted worktree view", args)
}

// refOnlyShape reports whether args is one of the exact ref-only calls host code makes. A verb is
// not enough: `remote update` fetches with the box-written config's credential helpers, and
// `config --global` writes the host's own configuration.
func refOnlyShape(args []string) bool {
	plain := func(values ...string) bool {
		for _, value := range values {
			if value == "" || strings.HasPrefix(value, "-") {
				return false
			}
		}
		return true
	}
	switch args[0] {
	case "config": // the fork's identity and signing values, the clone's origin, or a read
		writable := []string{"user.name", "user.email", "user.signingkey", "gpg.format", "remote.origin.url"}
		return len(args) == 3 && slices.Contains(writable, args[1]) ||
			len(args) == 3 && args[1] == "--get" && plain(args[2])
	case "rev-parse":
		return len(args) == 4 && args[1] == "--path-format=absolute" && args[2] == "--git-path" && plain(args[3]) ||
			slices.Equal(args, []string{"rev-parse", "--path-format=absolute", "--git-common-dir"})
	case "update-ref":
		rest := args[1:]
		if slices.Equal(rest, []string{"--stdin"}) {
			return true
		}
		if len(rest) > 2 && rest[0] == "-m" {
			rest = rest[2:]
		}
		if len(rest) > 0 && rest[0] == "--no-deref" {
			rest = rest[1:]
		}
		return (len(rest) == 2 || len(rest) == 3) && plain(rest...)
	case "symbolic-ref":
		return len(args) == 3 && args[1] == "HEAD" && strings.HasPrefix(args[2], "refs/heads/") && plain(args[2])
	case "for-each-ref":
		return len(args) == 2 && strings.HasPrefix(args[1], "--format=")
	case "branch":
		return len(args) == 4 && args[1] == "-q" && args[2] == "-D" && plain(args[3]) ||
			len(args) == 3 && plain(args[1]) && args[2] == "HEAD"
	case "remote":
		return len(args) == 3 && args[1] == "remove" && plain(args[2])
	case "init":
		return slices.Equal(args, []string{"init", "--bare", "--quiet"}) ||
			slices.Equal(args, []string{"init", "--bare", "--quiet", "--object-format=sha256"}) ||
			slices.Equal(args, []string{"init", "--quiet", "--template=", "--object-format=sha1", "--initial-branch=main"})
	case "cat-file":
		return len(args) == 3 && args[1] == "-e"
	case "hash-object":
		return len(args) == 5 && args[1] == "-w" && args[2] == "-t" && (args[3] == "tree" || args[3] == "commit" || args[3] == "blob") && args[4] == "--stdin"
	case "worktree":
		return len(args) == 7 && slices.Equal(args[1:5], []string{"add", "--no-checkout", "--detach", "--quiet"}) && plain(args[5], args[6]) ||
			len(args) == 4 && args[1] == "remove" && args[2] == "--force" && plain(args[3]) ||
			len(args) == 5 && slices.Equal(args[1:4], []string{"remove", "--force", "--"}) && args[4] != ""
	}
	return false
}
