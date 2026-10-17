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
	} {
		if command := GitRefCommand(t.Context(), t.TempDir(), args...); command.Err == nil {
			t.Errorf("raw metadata accepted %v", args)
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
