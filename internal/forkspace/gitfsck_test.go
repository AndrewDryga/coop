package forkspace

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func fsckRun(t *testing.T, repo string) (string, error) {
	t.Helper()
	var output bytes.Buffer
	err := RunGitFsck(context.Background(), repo, os.Environ(), &output, &output,
		"--full", "--strict", "--no-dangling", "--no-progress")
	return output.String(), err
}

func TestGitFsckHealthyMetadata(t *testing.T) {
	for _, format := range []string{"sha1", "sha256"} {
		t.Run(format, func(t *testing.T) {
			t.Setenv("GIT_DEFAULT_HASH", format)
			repo, run := viewTestRepo(t)
			t.Setenv("TMPDIR", t.TempDir())
			check := func(path string) {
				t.Helper()
				if out, err := fsckRun(t, path); err != nil {
					t.Fatalf("fsck: %v\n%s", err, out)
				}
				if entries, err := os.ReadDir(os.Getenv("TMPDIR")); err != nil || len(entries) != 0 {
					t.Fatalf("fsck left temporary metadata: %v, %v", entries, err)
				}
			}
			check(repo)
			run("pack-refs", "--all")
			check(repo)
			linked := filepath.Join(t.TempDir(), "linked")
			run("worktree", "add", "-q", linked, "-b", "linked")
			check(linked)
			if err := os.WriteFile(filepath.Join(repo, ".git", "shallow"), readOut(t, repo, "rev-parse", "HEAD"), 0o600); err != nil {
				t.Fatal(err)
			}
			check(repo)
			view, err := fsckGitView(context.Background(), repo)
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(view.dir)
			if info, err := os.Lstat(filepath.Join(view.dir, "refs")); err != nil || !info.IsDir() {
				t.Fatalf("fsck refs root is not a real directory: %v, %v", info, err)
			}
		})
	}
}

func TestGitFsckRejectsInvalidHistory(t *testing.T) {
	for _, damage := range []string{"missing-object", "malformed-ref", "ignored-fsck-error"} {
		t.Run(damage, func(t *testing.T) {
			repo, run := viewTestRepo(t)
			t.Setenv("TMPDIR", t.TempDir())
			switch damage {
			case "missing-object":
				blob := strings.TrimSpace(string(readOut(t, repo, "rev-parse", "HEAD:tracked.txt")))
				if err := os.Remove(filepath.Join(repo, ".git", "objects", blob[:2], blob[2:])); err != nil {
					t.Fatal(err)
				}
			case "malformed-ref":
				if err := os.WriteFile(filepath.Join(repo, ".git", "refs", "heads", "invalid"), []byte("not-an-object\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "ignored-fsck-error":
				tree := strings.TrimSpace(string(readOut(t, repo, "rev-parse", "HEAD^{tree}")))
				cmd := exec.Command("git", "-C", repo, "hash-object", "-t", "commit", "--literally", "-w", "--stdin")
				cmd.Stdin = strings.NewReader("tree " + tree + "\ncommitter T <t@t> 0 +0000\n\nmissing author\n")
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("fixture commit: %v: %s", err, out)
				}
				commit := strings.TrimSpace(string(out))
				run("update-ref", "refs/heads/invalid", commit)
				run("config", "fsck.missingAuthor", "ignore")
				run("fsck", "--full", "--strict", "--no-reflogs", "--no-dangling")
			}
			if out, err := fsckRun(t, repo); err == nil {
				t.Fatalf("fsck accepted %s: %s", damage, out)
			}
			if entries, err := os.ReadDir(os.Getenv("TMPDIR")); err != nil || len(entries) != 0 {
				t.Fatalf("rejected history left metadata: %v, %v", entries, err)
			}
		})
	}
}

func TestGitFsckRejectsUnsafeMetadataAndCleansUp(t *testing.T) {
	for _, name := range []string{"HEAD", "config", "refs", "refs/heads/main"} {
		for _, kind := range []string{"symlink", "fifo"} {
			t.Run(name+"/"+kind, func(t *testing.T) {
				repo, _ := viewTestRepo(t)
				t.Setenv("TMPDIR", t.TempDir())
				path := filepath.Join(repo, ".git", name)
				if err := os.RemoveAll(path); err != nil {
					t.Fatal(err)
				}
				if kind == "symlink" {
					if err := os.Symlink("/nonexistent-outside-repository", path); err != nil {
						t.Fatal(err)
					}
				} else if err := unix.Mkfifo(path, 0o600); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if err := RunGitFsck(ctx, repo, os.Environ(), nil, nil); err == nil || ctx.Err() != nil {
					t.Fatalf("unsafe metadata did not refuse promptly: %v (context %v)", err, ctx.Err())
				}
				if entries, err := os.ReadDir(os.Getenv("TMPDIR")); err != nil || len(entries) != 0 {
					t.Fatalf("failed fsck left metadata: %v, %v", entries, err)
				}
			})
		}
	}
}
