package sessionsvc

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// A restore writes bytes a control plane chose into a fork workspace. Nothing may land inside
// .git/ — a planted hook survives the post-restore verification failure and `git clean -x` — and a
// symlink the applied patch created must not steer a member there either.
func TestWriteRestoredCheckpointFileRefusesGitDirAndSymlinkParents(t *testing.T) {
	workspace := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspace, ".git", "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(".git", "hooks"), filepath.Join(workspace, "l")); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	for _, bad := range []string{".git/hooks/pre-commit", "vendor/.GIT/config", "l/pre-commit", "l/deeper/pre-commit"} {
		if err := writeRestoredCheckpointFile(root, []byte(bad), 0o755, []byte("#!/bin/sh\n")); err == nil {
			t.Errorf("%s: restored a member toward the Git directory", bad)
		}
	}
	entries, err := os.ReadDir(filepath.Join(workspace, ".git", "hooks"))
	if err != nil || len(entries) != 0 {
		t.Fatalf(".git/hooks after refused restores = %v, %v; want empty", entries, err)
	}
	if err := writeRestoredCheckpointFile(root, []byte("nested/dir/file.txt"), 0o644, []byte("ok\n")); err != nil {
		t.Fatalf("ordinary nested member: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "nested", "dir", "file.txt")); err != nil {
		t.Fatalf("ordinary member missing: %v", err)
	}
	if err := writeRestoredCheckpointFile(root, []byte("nested/dir/file.txt"), 0o644, []byte("dup\n")); !errors.Is(err, os.ErrExist) {
		t.Fatalf("second write of one member = %v, want exclusive creation to refuse", err)
	}
}
