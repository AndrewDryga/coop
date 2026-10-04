package shadowpath

import (
	"errors"
	"os"
	"path/filepath"
	"runtime/debug"
	"slices"
	"syscall"
	"testing"
)

func TestReadRegularClosesNestedDescriptors(t *testing.T) {
	repo := t.TempDir()
	dir := filepath.Join(repo, "a", "b")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"ok.txt": "ok", CoopIgnoreFile: "deny.txt\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	root, err := os.Open(repo)
	if err != nil {
		t.Fatal(err)
	}
	tree, snapshot, err := OpenTree(root, "")
	_ = root.Close()
	if err != nil {
		t.Fatal(err)
	}
	defer tree.Close()
	// Darwin can reject os.ReadDir("/dev/fd") inside Go even though the directory exists.
	// Fstat inventories this process directly on both supported hosts; new descriptors use
	// the lowest free numbers, so a range beyond the pinned tree covers traversal leaks.
	scanLimit := int(tree.Fd()) + 1024
	countFDs := func() int {
		t.Helper()
		var count int
		for fd := range scanLimit {
			var stat syscall.Stat_t
			err := syscall.Fstat(fd, &stat)
			if err == nil {
				count++
			} else if !errors.Is(err, syscall.EBADF) {
				t.Fatalf("inspect descriptor %d: %v", fd, err)
			}
		}
		return count
	}

	// A finalizer must not be needed to release each nested directory descriptor.
	priorGC := debug.SetGCPercent(-1)
	defer debug.SetGCPercent(priorGC)
	baseline := countFDs()
	for range 16 {
		body, err := snapshot.ReadRegular(tree, "", filepath.Join("a", "b", "ok.txt"), 32)
		if err != nil || string(body) != "ok" {
			t.Fatalf("nested read = %q, %v", body, err)
		}
		if _, err := snapshot.ReadRegular(tree, "", filepath.Join("a", "b", "deny.txt"), 32); !errors.Is(err, ErrProtected) {
			t.Fatalf("nested protected read = %v, want ErrProtected", err)
		}
		if _, err := snapshot.ReadRegular(tree, "", filepath.Join("a", "b", "missing.txt"), 32); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("nested missing read = %v, want os.ErrNotExist", err)
		}
		if _, err := snapshot.ReadRegular(tree, "", filepath.Join("a", "absent", "ok.txt"), 32); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("nested missing directory = %v, want os.ErrNotExist", err)
		}
	}
	if got := countFDs(); got > baseline+4 {
		t.Fatalf("nested reads retained %d descriptors (baseline %d, now %d)", got-baseline, baseline, got)
	}
}

// Only a line that starts with # is a comment, so an entry with a comment after its pattern is one
// pattern that hides nothing its author meant; CommentedEntries finds those, and only those.
func TestCommentedEntries(t *testing.T) {
	data := []byte("# a whole-line comment\nprod.yml   # basename\nvault/\tthe folder # note\nissue#12.txt\n  # indented comment\nkeys/*.pem\n")
	got := CommentedEntries(data)
	want := []string{"prod.yml   # basename", "vault/\tthe folder # note"}
	if !slices.Equal(got, want) {
		t.Fatalf("CommentedEntries = %q, want %q", got, want)
	}
	// and such an entry really does hide nothing: the pattern keeps its comment
	g := ParseUserGlobs([]byte("prod.yml   # basename\n"))
	if slices.Contains(g.Base, "prod.yml") {
		t.Fatalf("a commented entry parsed as %q; the warning would be wrong", g.Base)
	}
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, CoopIgnoreFile), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := RepoCommentedEntries(repo); !slices.Equal(got, want) {
		t.Fatalf("RepoCommentedEntries = %q, want %q", got, want)
	}
	if got := RepoCommentedEntries(t.TempDir()); got != nil {
		t.Fatalf("a repository without .coopignore reported %q", got)
	}
}
