package safefile

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestStrictReadsRejectLinksAndSpecialFiles(t *testing.T) {
	rootPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rootPath, "real"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "real", "value"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", filepath.Join(rootPath, "linked-dir")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real/value", filepath.Join(rootPath, "linked-file")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(rootPath, "fifo"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if got, err := ReadRegular(root, "real/value", 6); err != nil || string(got) != "secret" {
		t.Fatalf("regular read = %q, %v", got, err)
	}
	for _, rel := range []string{"linked-dir/value", "linked-file", "fifo"} {
		if data, err := ReadRegular(root, rel, 64); err == nil || len(data) != 0 {
			t.Errorf("unsafe read %q = %q, %v", rel, data, err)
		}
	}
	if data, err := ReadRegular(root, "real/value", 5); err == nil || len(data) != 0 {
		t.Fatalf("oversized read = %q, %v", data, err)
	}
}

func TestOpenRootRejectsLinkAndPinsDirectory(t *testing.T) {
	parent := t.TempDir()
	original := filepath.Join(parent, "original")
	if err := os.Mkdir(original, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(parent, "alias")
	if err := os.Symlink(original, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenRoot(alias); err == nil {
		t.Fatal("symlink root was accepted")
	}
	root, err := OpenRoot(original)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := os.Rename(original, filepath.Join(parent, "moved")); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenDir(root, "missing"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pinned root lookup error = %v", err)
	}
}
