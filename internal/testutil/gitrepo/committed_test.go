package gitrepo

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCommittedSeedCopiesHaveIndependentObjectsAndAuthority(t *testing.T) {
	a, runA := NewCommitted(t)
	b, runB := NewCommitted(t)
	head := func(repo string) string {
		t.Helper()
		cmd := exec.Command("git", "rev-parse", "HEAD")
		cmd.Dir = repo
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(out))
	}
	original := head(a)
	if head(b) != original {
		t.Fatal("seed copies have different bases")
	}
	if err := filepath.WalkDir(filepath.Join(a, ".git"), func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(a, path)
		if err != nil {
			return err
		}
		left, err := os.Lstat(path)
		if err != nil {
			return err
		}
		right, err := os.Lstat(filepath.Join(b, rel))
		if err != nil {
			return err
		}
		if !left.Mode().IsRegular() || !right.Mode().IsRegular() || os.SameFile(left, right) {
			t.Fatalf("shared/link seed file %s", rel)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"objects/info/alternates", "commondir"} {
		if _, err := os.Lstat(filepath.Join(b, ".git", name)); !os.IsNotExist(err) {
			t.Fatalf("shared authority %s: %v", name, err)
		}
	}
	runA("config", "user.name", "Changed")
	runA("commit", "-q", "--allow-empty", "-m", "private change")
	worktree := filepath.Join(t.TempDir(), "linked")
	runA("worktree", "add", "-q", "--detach", worktree)
	if head(a) == original || head(b) != original {
		t.Fatal("commit crossed private copies")
	}
	if err := os.RemoveAll(filepath.Join(a, ".git")); err != nil {
		t.Fatal(err)
	}
	runB("fsck", "--full")
	c, _ := NewCommitted(t)
	if head(c) != original || head(b) != original {
		t.Fatal("mutating one copy changed the seed")
	}
	body, err := os.ReadFile(filepath.Join(c, ".git", "config"))
	if err != nil || strings.Contains(string(body), "Changed") {
		t.Fatalf("config crossed seed copies: %s, %v", body, err)
	}
}

// A fixture commit starts no git maintenance: since git 2.47 it runs in the background after the
// commit returns and takes .git/objects/maintenance.lock while the seed or a test reads the folder.
func TestFixtureCommitsStartNoMaintenance(t *testing.T) {
	trace := filepath.Join(t.TempDir(), "trace")
	t.Setenv("GIT_TRACE", trace)
	_, fresh := New(t)
	fresh("commit", "-q", "--allow-empty", "-m", "base")
	_, committed := NewCommitted(t)
	committed("commit", "-q", "--allow-empty", "-m", "next")
	data, err := os.ReadFile(trace)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "commit") {
		t.Fatalf("GIT_TRACE recorded no commit, so this check proves nothing:\n%s", data)
	}
	if strings.Contains(string(data), "maintenance") {
		t.Fatalf("a fixture commit started git maintenance:\n%s", data)
	}
}
