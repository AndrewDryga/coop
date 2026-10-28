package tasks

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestReadForkRegistryFile(t *testing.T) {
	for _, kind := range []string{"small", "exact limit", "oversized", "missing", "directory", "symlink", "in-root symlink", "hardlink", "fifo"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			path := filepath.Join(dir, "record")
			outside := filepath.Join(t.TempDir(), "original")
			if err := os.WriteFile(outside, []byte("safe"), 0o600); err != nil {
				t.Fatal(err)
			}
			var want string
			switch kind {
			case "small":
				want = "ok"
				err = os.WriteFile(path, []byte(want), 0o600)
			case "exact limit":
				want = "1234"
				err = os.WriteFile(path, []byte(want), 0o600)
			case "oversized":
				err = os.WriteFile(path, []byte("12345"), 0o600)
			case "directory":
				err = os.Mkdir(path, 0o700)
			case "in-root symlink":
				inside := filepath.Join(dir, "safe")
				if err := os.WriteFile(inside, []byte("safe"), 0o600); err != nil {
					t.Fatal(err)
				}
				err = os.Symlink("safe", path)
			case "symlink":
				err = os.Symlink(outside, path)
			case "hardlink":
				err = os.Link(outside, path)
			case "fifo":
				err = syscall.Mkfifo(path, 0o600)
			}
			if err != nil {
				t.Fatal(err)
			}
			data, err := readForkRegistryFile(root, "record", "test registry", 4)
			if want != "" {
				if err != nil || string(data) != want {
					t.Fatalf("read = %q, %v; want %q", data, err, want)
				}
			} else if err == nil || data != nil {
				t.Fatalf("unsafe read = %q, %v; want refusal", data, err)
			}
			if kind == "missing" && !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("missing file error = %v", err)
			}
			if data, err := os.ReadFile(outside); err != nil || string(data) != "safe" {
				t.Fatalf("refused target changed: %q, %v", data, err)
			}
		})
	}
}
