package safefile

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
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
	if err := os.WriteFile(filepath.Join(original, "value"), []byte("pinned"), 0o600); err != nil {
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
	if err := os.Mkdir(original, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(original, "value"), []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadRegular(root, "value", 64); err != nil || string(got) != "pinned" {
		t.Fatalf("held root followed its replaced path: %q, %v", got, err)
	}
	if _, err := OpenDir(root, "missing"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pinned root lookup error = %v", err)
	}
}

// Both link rules share one bounded read: a FIFO, a folder and a socket are refused without waiting,
// a file over its limit as too large, and a missing one reads as absent. They
// differ only on links: ReadRegular refuses any, ReadRegularIn follows one that stays inside the
// root and refuses one that leads out.
func TestBoundedReadsShareTheirRefusals(t *testing.T) {
	rootPath := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside")
	for path, body := range map[string]string{
		filepath.Join(rootPath, "value"): "secret", filepath.Join(rootPath, "large"): "0123456789", outside: "elsewhere",
	} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(rootPath, "folder"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(rootPath, "fifo"), 0o600); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(rootPath, "s")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := os.Symlink("value", filepath.Join(rootPath, "inside-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(rootPath, "outside-link")); err != nil {
		t.Fatal(err)
	}
	strict, err := OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer strict.Close()
	rooted, err := os.OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer rooted.Close()
	reads := map[string]func(string) ([]byte, error){
		"no links":     func(rel string) ([]byte, error) { return ReadRegular(strict, rel, 8) },
		"links inside": func(rel string) ([]byte, error) { return ReadRegularIn(rooted, rel, 8) },
	}
	for name, read := range reads {
		t.Run(name, func(t *testing.T) {
			if got, err := read("value"); err != nil || string(got) != "secret" {
				t.Fatalf("regular read = %q, %v", got, err)
			}
			// A socket fails at the open itself on macOS, before any check can call it irregular.
			for _, rel := range []string{"fifo", "folder", "s"} {
				done := make(chan error, 1)
				go func() { _, err := read(rel); done <- err }()
				select {
				case err := <-done:
					if err == nil || rel != "s" && !errors.Is(err, ErrNotRegular) {
						t.Errorf("%s = %v, want it refused as not regular", rel, err)
					}
				case <-time.After(10 * time.Second):
					t.Fatalf("reading %s waited", rel)
				}
			}
			if _, err := read("large"); !errors.Is(err, ErrTooLarge) {
				t.Errorf("an oversize file = %v, want a too-large refusal", err)
			}
			if _, err := read("missing"); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("a missing file = %v, want absent", err)
			}
			if data, err := read("outside-link"); err == nil || len(data) != 0 {
				t.Errorf("a link out of the root = %q, %v", data, err)
			}
			got, err := read("inside-link")
			if name == "no links" && (err == nil || len(got) != 0) {
				t.Errorf("a strict read followed a link: %q, %v", got, err)
			}
			if name == "links inside" && (err != nil || string(got) != "secret") {
				t.Errorf("a link inside the root = %q, %v; want it followed", got, err)
			}
		})
	}
}
