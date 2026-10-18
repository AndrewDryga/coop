package forkspace

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHostGitRejectsSubmoduleEscapeArguments(t *testing.T) {
	for _, args := range [][]string{
		{"status", "--ignore-submodules=none"}, {"status", "--ignore-submodules=untracked"},
		{"status", "--ignore-sub=none"}, {"diff", "--submodule=diff"}, {"show", "--submod=diff"},
		{"clone", "--recursive", "source", "destination"}, {"clone", "--recurs", "source", "destination"},
		{"fetch", "--recurse-submodules=on-demand"}, {"fetch", "--recurse-submodules-default=yes"},
		{"checkout", "--recurse-submodules"}, {"reset", "--recurse-submodules"},
		{"add", "-A"}, {"add", "-u"}, {"submodule", "foreach", "true"}, {"submodule", "update"},
		{"commit", "-am", "implicit add"}, {"new-unreviewed-command"}, {"lfs", "checkout"}, {"lfs", "status"},
		{"rebase", "--exec=true", "HEAD"}, {"rebase", "-xtrue", "HEAD"}, {"rebase", "--strategy=evil", "HEAD"},
		{"merge", "-sevil", "HEAD"}, {"-C", "somewhere", "status"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			command := TrustedGitCommand(t.Context(), t.TempDir(), IndependentGitEnv(), args...)
			if err := command.Run(); err == nil || command.Process != nil {
				t.Fatalf("unsafe command was launched: %v, %v", args, err)
			}
		})
	}
	for _, args := range [][]string{
		{"status"}, {"diff"}, {"checkout", "-b", "branch"}, {"worktree", "add", "path"},
		{"cat-file", "--filters", "HEAD:file"}, {"cat-file", "--textconv", "HEAD:file"},
		{"hash-object", "--pa=file", "--stdin"},
		// A verb alone is not a shape: these read box-written credential helpers or write the
		// host's own configuration.
		{"remote", "update"}, {"remote", "add", "evil", "url"}, {"config", "--global", "user.name", "x"},
		{"config", "core.fsmonitor", "evil"}, {"config", "--add", "user.name", "x"}, {"init"},
		{"branch", "-D", "x"}, {"update-ref", "-d", "refs/heads/x"}, {"symbolic-ref", "HEAD", "refs/remotes/x"},
		{"for-each-ref", "--format=%(refname)", "refs/"}, {"rev-parse", "HEAD"}, {"worktree", "add", "--no-checkout", "path"},
	} {
		if command := GitRefCommand(t.Context(), t.TempDir(), args...); command.Err == nil {
			t.Errorf("raw metadata accepted %v", args)
		}
	}
}

// Every shape host code passes to GitRefCommand is accepted; each is a ref, config value, path or
// object operation that runs nothing from the repository's configuration.
func TestGitRefCommandAcceptsTheShapesItsCallersUse(t *testing.T) {
	commit := strings.Repeat("a", 40)
	for _, args := range [][]string{
		{"config", "user.name", "Coop"}, {"config", "user.email", "coop@localhost"},
		{"config", "user.signingkey", "key"}, {"config", "gpg.format", "ssh"}, {"config", "remote.origin.url", "/src"},
		{"config", "--get", "user.name"},
		{"rev-parse", "--path-format=absolute", "--git-path", "objects"}, {"rev-parse", "--path-format=absolute", "--git-common-dir"},
		{"update-ref", "refs/heads/main", commit}, {"update-ref", "refs/heads/main", commit, commit},
		{"update-ref", "-m", "coop: re-sign commits", "refs/heads/main", commit, commit},
		{"update-ref", "--no-deref", "HEAD", commit}, {"update-ref", "--stdin"},
		{"symbolic-ref", "HEAD", "refs/heads/main"}, {"for-each-ref", "--format=delete %(refname)"},
		{"branch", "-q", "-D", "review/fork"}, {"branch", "topic", "HEAD"}, {"remote", "remove", "origin"},
		{"init", "--bare", "--quiet"}, {"init", "--bare", "--quiet", "--object-format=sha256"},
		{"init", "--quiet", "--template=", "--object-format=sha1", "--initial-branch=main"},
		{"cat-file", "-e", commit + "^{commit}"}, {"hash-object", "-w", "-t", "blob", "--stdin"},
		{"worktree", "add", "--no-checkout", "--detach", "--quiet", "/tmp/worktree", commit},
		{"worktree", "remove", "--force", "/tmp/worktree"}, {"worktree", "remove", "--force", "--", "/tmp/worktree"},
	} {
		if err := refOnlyGitArgs(args); err != nil {
			t.Errorf("a caller's shape was refused: %v", err)
		}
	}
}

// A caller's -c may set only reviewed values. A driver definition from a caller would run a command
// that a box-written .gitattributes selects, so filter, diff and merge drivers never launch.
func TestHostGitRefusesCallerDriverSettings(t *testing.T) {
	repo, run := viewTestRepo(t)
	if err := os.WriteFile(filepath.Join(repo, ".gitattributes"), []byte("* filter=probe diff=probe merge=probe\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", ".gitattributes")
	run("commit", "-qm", "attributes")
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "driver-ran")
	for _, setting := range []string{
		"filter.probe.clean=touch " + marker, "filter.probe.process=touch " + marker,
		"diff.probe.textconv=touch " + marker, "diff.probe.command=touch " + marker, "diff.external=touch " + marker,
		"merge.probe.driver=touch " + marker, "core.attributesFile=" + filepath.Join(repo, ".gitattributes"),
		"gpg.format=../planted", "commit.gpgsign",
	} {
		command, err := GitCommand(t.Context(), repo, "-c", setting, "diff", "HEAD")
		if err == nil {
			err = command.Run()
		}
		if err == nil || command != nil && command.Process != nil {
			t.Errorf("-c %s launched Git: %v", setting, err)
		}
		if command := TrustedGitCommand(t.Context(), repo, IndependentGitEnv(), "-c", setting, "diff", "HEAD"); command.Run() == nil || command.Process != nil {
			t.Errorf("-c %s launched trusted Git", setting)
		}
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a caller's driver ran")
	}
	for _, setting := range []string{
		"core.attributesFile=" + os.DevNull, "http.followRedirects=false", "difftool.vimdiff.cmd=vimdiff",
		"diff.tool=vimdiff", "commit.gpgsign=true", "gpg.format=ssh", "user.useConfigOnly=true",
	} {
		if _, err := nonRecursiveGitArgs([]string{"-c", setting, "status"}); err != nil {
			t.Errorf("a reviewed setting was refused: %v", err)
		}
	}
}

func TestHostGitViewsNeverEnterSubmoduleConfig(t *testing.T) {
	repo, run := viewTestRepo(t)
	child, childGit := viewTestRepo(t)
	if err := os.WriteFile(filepath.Join(child, ".gitattributes"), []byte("*.txt filter=submodule_probe\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	childGit("add", ".gitattributes")
	childGit("commit", "-qm", "attributes")
	run("-c", "protocol.file.allow=always", "submodule", "add", "--quiet", child, "child")
	run("config", "-f", ".gitmodules", "submodule.child.ignore", "none")
	run("commit", "-qam", "module")
	// Advance the checked-out child HEAD before installing the filter. Parent status
	// and worktree diff must still report the changed gitlink while avoiding drivers.
	childDir := filepath.Join(repo, "child")
	fixture := func(args ...string) {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", childDir}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("fixture git: %s, %v", out, err)
		}
	}
	if err := os.WriteFile(filepath.Join(childDir, "new.txt"), []byte("new\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fixture("add", "new.txt")
	fixture("-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-qm", "advance child")
	marker := filepath.Join(t.TempDir(), "filter-ran")
	fixture("config", "filter.submodule_probe.clean", "printf ran > '"+marker+"'; cat")
	stamp := time.Now().Add(-24 * time.Hour)
	if err := os.Chtimes(filepath.Join(childDir, "tracked.txt"), stamp, stamp); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"status", "--porcelain"}, {"diff", "--name-only"}, {"diff", "--name-only", "--", "child"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			command, err := GitCommand(t.Context(), repo, args...)
			if err != nil {
				t.Fatal(err)
			}
			out, err := command.CombinedOutput()
			if err != nil || !strings.Contains(string(out), "child") {
				t.Fatalf("gitlink disappeared: %s, %v", out, err)
			}
			out, err = ObserveGit(t.Context(), repo, args...)
			if err != nil || !strings.Contains(string(out), "child") {
				t.Fatalf("observed gitlink disappeared: %s, %v", out, err)
			}
			if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("child driver ran: %v", err)
			}
		})
	}
	command, err := GitCommand(t.Context(), repo, "status", "--porcelain", "--ignore-submodules=all")
	if err != nil {
		t.Fatal(err)
	}
	if out, err := command.CombinedOutput(); err != nil || len(out) != 0 {
		t.Fatalf("explicit all changed: %s, %v", out, err)
	}
}
